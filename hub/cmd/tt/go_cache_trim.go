package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// The Go build cache trim. On a host whose owner turned it on in
// ~/.config/tailterm/relay.json, the relay unlinks build cache entries unused
// for longer than a set age, in the same pass as the scheduled sweep and
// before it. It never empties the cache: only old regular entry files directly
// inside the cache's two-hex-digit directories go, and every directory, the
// README, trim.txt and the module cache stay. Each run adds one line to the
// sweep's journal and posts nothing; the sweep's low-space notice carries the
// last run's numbers.

const (
	defaultGoCacheTrimMaxAge = 24 * time.Hour
	// minGoCacheTrimMaxAge is the least age: go refreshes an entry's mtime
	// only when it is more than an hour old.
	minGoCacheTrimMaxAge = 2 * time.Hour
	goCacheTrimSource    = "go-cache-trim"
	// goCacheReadmeLine is the first line of the README go writes in a build
	// cache; a directory without it is not trimmed.
	goCacheReadmeLine = "This directory holds cached build artifacts from the Go build system."
)

// Trim skip and failure reasons, as the journal names them.
const (
	goCacheTrimSkipNoGo     = "no-go"
	goCacheTrimSkipNotCache = "not-a-cache"
	goCacheTrimFailWalk     = "walk"
)

type goCacheTrimSettings struct {
	On            bool
	MaxAge        time.Duration
	Interval      time.Duration
	LowSpaceBytes int64
}

// readGoCacheTrimSettings reads the three goBuildCacheTrim keys from
// relay.json. The trim is off unless goBuildCacheTrim is "on": a missing,
// unreadable or malformed file is off with no fault, since the sweep journals
// that case. A goBuildCacheTrim value that is neither "on" nor "off" is a
// fault. A bad age or interval uses its default, one under its floor uses the
// floor, and each says so once on stderr. The low-space threshold is the
// sweep's.
func readGoCacheTrimSettings() (goCacheTrimSettings, string) {
	settings := goCacheTrimSettings{MaxAge: defaultGoCacheTrimMaxAge, Interval: defaultSweepInterval, LowSpaceBytes: defaultSweepLowSpaceGiB << 30}
	config, fault := readRelayConfig()
	if fault != "" {
		return settings, ""
	}
	raw, ok := config["goBuildCacheTrim"]
	if !ok {
		return settings, ""
	}
	var value string
	if json.Unmarshal(raw, &value) != nil || (value != "on" && value != "off") {
		return settings, `goBuildCacheTrim is neither "on" nor "off"`
	}
	if value == "off" {
		return settings, ""
	}
	settings.On = true
	duration := func(key string, into *time.Duration, least time.Duration) {
		raw, ok := config[key]
		if !ok {
			return
		}
		var value string
		d, err := time.Duration(0), error(nil)
		if json.Unmarshal(raw, &value) == nil {
			d, err = time.ParseDuration(value)
		}
		if value == "" || err != nil || d <= 0 {
			sweepSettingsWarn(key, string(raw), into.String())
			return
		}
		if d < least {
			sweepSettingsWarn(key, string(raw), least.String())
			d = least
		}
		*into = d
	}
	duration("goBuildCacheTrimMaxAge", &settings.MaxAge, minGoCacheTrimMaxAge)
	duration("goBuildCacheTrimInterval", &settings.Interval, minSweepInterval)
	sweep, _ := readSweepSettings()
	settings.LowSpaceBytes = sweep.LowSpaceBytes
	return settings, ""
}

// goCacheTrimState is the trim's part of worktree-sweep-state.json: its
// schedule and the numbers of its last trimmed run.
type goCacheTrimState struct {
	// LastRunAt is the last trimmed run; LastAttemptAt also counts skipped
	// and failed attempts.
	LastRunAt       time.Time `json:"lastRunAt"`
	LastAttemptAt   time.Time `json:"lastAttemptAt"`
	FilesRemoved    int64     `json:"filesRemoved"`
	BytesFreed      int64     `json:"bytesFreed"`
	CacheBytesAfter int64     `json:"cacheBytesAfter"`
	MaxAge          string    `json:"maxAge,omitempty"`
}

// goCacheTrimDue is sweepDue for the trim's own times.
func goCacheTrimDue(state *goCacheTrimState, interval time.Duration, now time.Time) bool {
	if state == nil {
		return true
	}
	return sweepDue(sweepState{LastRunAt: state.LastRunAt, LastAttemptAt: state.LastAttemptAt}, interval, now)
}

// goCacheTrimNoticeSentence is the trim's sentence in the sweep's low-space
// notice.
func goCacheTrimNoticeSentence(state *goCacheTrimState) string {
	if state == nil || state.LastRunAt.IsZero() {
		return "The Go build cache trim is off or has not run on this host."
	}
	return fmt.Sprintf("Go build cache trim: last run %s removed %d entries older than %s and freed %s; the cache now holds %s.",
		state.LastRunAt.UTC().Format(time.RFC3339), state.FilesRemoved, state.MaxAge, humanBytes(state.BytesFreed), humanBytes(state.CacheBytesAfter))
}

// goCacheTrimJournalText is one trim line as `--journal` prints it.
func goCacheTrimJournalText(l sweepJournalLine) string {
	if l.Outcome != "trimmed" {
		text := fmt.Sprintf("%s %s %s %s", l.At, goCacheTrimSource, l.Outcome, l.Reason)
		if l.Detail != "" {
			text += ": " + l.Detail
		}
		return text
	}
	var files, cache int64
	if l.FilesRemoved != nil {
		files = *l.FilesRemoved
	}
	if l.CacheBytesAfter != nil {
		cache = *l.CacheBytesAfter
	}
	free := "unknown"
	if l.FreeBytesAfter >= 0 {
		free = humanBytes(l.FreeBytesAfter)
	}
	text := fmt.Sprintf("%s %s trimmed files=%d freed=%s cache=%s free=%s max-age=%s", l.At, goCacheTrimSource, files, humanBytes(l.BytesFreed), humanBytes(cache), free, l.MaxAge)
	if l.Detail != "" {
		text += "; " + l.Detail
	}
	if l.LowSpace {
		text += "; low space"
	}
	return text
}

// The walk.

// Test hooks, nil in production. goCacheTrimAfterList runs after the
// top-level listing and before each two-hex directory is opened;
// goCacheTrimBeforeUnlink runs between an entry's second look and its unlink.
var (
	goCacheTrimAfterList    func(name string)
	goCacheTrimBeforeUnlink func(dir, name string)
)

const goCacheDirFlags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC

// goCacheHandle is the cache directory, opened once without following a
// symlink at its path. All file work is relative to it, so nothing that
// happens to the path afterwards redirects the trim.
type goCacheHandle struct {
	dir *os.File
}

func (h *goCacheHandle) fd() int { return int(h.dir.Fd()) }

func (h *goCacheHandle) close() { _ = h.dir.Close() }

func openGoBuildCache(dir string) (*goCacheHandle, error) {
	fd, err := openNoFollow(unix.AT_FDCWD, dir, goCacheDirFlags)
	if err != nil {
		return nil, err
	}
	return &goCacheHandle{dir: os.NewFile(uintptr(fd), dir)}, nil
}

func openNoFollow(dirFD int, name string, flags int) (int, error) {
	for {
		fd, err := unix.Openat(dirFD, name, flags, 0)
		if err != unix.EINTR {
			return fd, err
		}
	}
}

func isRegular(st *unix.Stat_t) bool { return uint32(st.Mode)&unix.S_IFMT == unix.S_IFREG }

func sameFile(a, b *unix.Stat_t) bool {
	return uint64(a.Dev) == uint64(b.Dev) && uint64(a.Ino) == uint64(b.Ino)
}

func isTwoHex(name string) bool {
	if len(name) != 2 {
		return false
	}
	for i := 0; i < 2; i++ {
		if c := name[i]; !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// pathHolds reports whether child is parent or lies inside it.
func pathHolds(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// validateGoBuildCache returns why the opened directory must not be trimmed,
// or "". It is a build cache only with go's README, and never the home
// directory, the module cache, a directory holding either, or a directory
// inside the module cache. A cache inside the home directory is the usual
// layout and is accepted.
func validateGoBuildCache(cache *goCacheHandle, dir, modCache string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "no home directory"
	}
	if modCache == "" || !filepath.IsAbs(modCache) {
		return "the module cache path is unknown"
	}
	readme, err := openNoFollow(cache.fd(), "README", unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC)
	if err != nil {
		return "no README"
	}
	file := os.NewFile(uintptr(readme), "README")
	defer file.Close()
	var st unix.Stat_t
	if err := unix.Fstat(readme, &st); err != nil || !isRegular(&st) {
		return "README is not a regular file"
	}
	head := make([]byte, len(goCacheReadmeLine)+1)
	n, _ := file.Read(head)
	head = head[:n]
	if string(head) != goCacheReadmeLine && string(head) != goCacheReadmeLine+"\n" {
		return "README is not the Go build cache's"
	}
	real, realHome, realMod := canonicalPath(dir), canonicalPath(home), canonicalPath(modCache)
	if pathHolds(real, realHome) {
		return "the path is or contains the home directory"
	}
	if pathHolds(real, realMod) {
		return "the path is or contains the module cache"
	}
	if pathHolds(realMod, real) {
		return "the path is inside the module cache"
	}
	var self, other unix.Stat_t
	if err := unix.Fstat(cache.fd(), &self); err != nil {
		return "the directory cannot be read"
	}
	if unix.Stat(home, &other) == nil && sameFile(&self, &other) {
		return "the directory is the home directory"
	}
	if unix.Stat(modCache, &other) == nil && sameFile(&self, &other) {
		return "the directory is the module cache"
	}
	return ""
}

type goCacheTrimResult struct {
	// FilesRemoved counts successful unlinks and BytesFreed their sizes.
	FilesRemoved int64
	BytesFreed   int64
	// CacheBytesAfter is the apparent size of the regular files left directly
	// inside the two-hex directories that were opened.
	CacheBytesAfter int64
	// Errors counts entries and directories that could not be handled;
	// FirstError names the first.
	Errors     int
	FirstError string
	// WalkError is set when the cache itself could not be listed.
	WalkError    string
	StoppedEarly bool
}

func (r *goCacheTrimResult) fail(what string, err error) {
	r.Errors++
	if r.FirstError == "" {
		r.FirstError = what + ": " + err.Error()
	}
}

// trimGoBuildCache unlinks the entries of an opened cache whose mtime is more
// than maxAge before now. It opens only two-hex directories, each without
// following a symlink, and in them looks only at regular files named as go
// names entries (ending -a or -d). The unlink is unlinkat with no flags,
// which cannot take a directory. It never descends further.
//
// One window remains: an entry swapped for a symlink between its second look
// and the unlink loses that symlink; the symlink's target is not touched.
func trimGoBuildCache(ctx context.Context, cache *goCacheHandle, maxAge time.Duration, now time.Time) goCacheTrimResult {
	var res goCacheTrimResult
	names, err := cache.dir.Readdirnames(-1)
	if err != nil {
		res.WalkError = err.Error()
		return res
	}
	sort.Strings(names)
	for _, name := range names {
		if !isTwoHex(name) {
			continue
		}
		if ctx.Err() != nil {
			res.StoppedEarly = true
			break
		}
		if hook := goCacheTrimAfterList; hook != nil {
			hook(name)
		}
		fd, err := openNoFollow(cache.fd(), name, goCacheDirFlags)
		if err != nil {
			// A symlink, a file, or a name that has gone is not a cache directory.
			if err != unix.ELOOP && err != unix.ENOTDIR && err != unix.ENOENT {
				res.fail(name, err)
			}
			continue
		}
		sub := os.NewFile(uintptr(fd), name)
		trimGoCacheDirectory(sub, name, maxAge, now, &res)
		_ = sub.Close()
	}
	return res
}

func trimGoCacheDirectory(sub *os.File, dir string, maxAge time.Duration, now time.Time, res *goCacheTrimResult) {
	names, err := sub.Readdirnames(-1)
	if err != nil {
		res.fail(dir, err)
		return
	}
	sort.Strings(names)
	fd := int(sub.Fd())
	old := func(st *unix.Stat_t) bool {
		return now.Sub(time.Unix(int64(st.Mtim.Sec), int64(st.Mtim.Nsec))) > maxAge
	}
	for _, name := range names {
		var st unix.Stat_t
		look := func() bool {
			if err := unix.Fstatat(fd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
				if err != unix.ENOENT {
					res.fail(dir+"/"+name, err)
				}
				return false
			}
			return isRegular(&st)
		}
		if !look() {
			continue
		}
		if !(strings.HasSuffix(name, "-a") || strings.HasSuffix(name, "-d")) || !old(&st) {
			res.CacheBytesAfter += st.Size
			continue
		}
		// A second look just before the unlink: a build may have used the
		// entry, or something else may have taken its name.
		if !look() {
			continue
		}
		if !old(&st) {
			res.CacheBytesAfter += st.Size
			continue
		}
		if hook := goCacheTrimBeforeUnlink; hook != nil {
			hook(dir, name)
		}
		size := st.Size
		switch err := unix.Unlinkat(fd, name, 0); err {
		case nil:
			res.FilesRemoved++
			res.BytesFreed += size
		case unix.ENOENT:
			// go's own trim got there first.
		default:
			res.fail(dir+"/"+name, err)
			if look() {
				res.CacheBytesAfter += st.Size
			}
		}
	}
}

// The attempt.

// goCacheTrimDeps are the trim's inputs; tests inject them.
type goCacheTrimDeps struct {
	now      func() time.Time
	settings func() (goCacheTrimSettings, string)
	// freeBytes is the free space of the volume holding path.
	freeBytes func(path string) (int64, error)
	// cacheDir names the build cache and the module cache.
	cacheDir func(ctx context.Context) (dir, modCache string, err error)
}

// nativeGoCacheDir asks go for the build cache and the module cache. There is
// no fallback: without a working go the run is skipped. The test binary
// replaces it, so no test can name the real cache.
var nativeGoCacheDir = func(ctx context.Context) (string, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "env", "GOCACHE", "GOMODCACHE")
	if home, err := os.UserHomeDir(); err == nil {
		cmd.Dir = home
	}
	out, err := cmd.Output()
	if err != nil {
		return "", "", err
	}
	lines := strings.Split(strings.TrimRight(string(out), "\r\n"), "\n")
	if len(lines) != 2 {
		return "", "", errors.New("go env printed an unexpected answer")
	}
	return strings.TrimSpace(lines[0]), strings.TrimSpace(lines[1]), nil
}

func nativeGoCacheTrimDeps() goCacheTrimDeps {
	return goCacheTrimDeps{
		now:       time.Now,
		settings:  readGoCacheTrimSettings,
		freeBytes: volumeFreeBytes,
		cacheDir:  func(ctx context.Context) (string, string, error) { return nativeGoCacheDir(ctx) },
	}
}

// goCacheTrimAttempt runs one trim when the host's switch is on and a run is
// due. It reports whether it made an attempt, which is exactly when it wrote
// a journal line. One trim runs at a time on a host: the lock is taken first,
// and the state is read and the due check made only under it.
func goCacheTrimAttempt(ctx context.Context, d goCacheTrimDeps) bool {
	now := d.now()
	started := time.Now()
	settings, fault := d.settings()
	if fault == "" && !settings.On {
		return false
	}
	if err := os.MkdirAll(relayDir(), 0700); err != nil {
		fmt.Fprintf(os.Stderr, "[tt relay] go build cache trim: %v\n", err)
		return false
	}
	lock, err := os.OpenFile(filepath.Join(relayDir(), "go-cache-trim.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[tt relay] go build cache trim lock: %v\n", err)
		return false
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		// Another trim is running; it writes the receipt.
		return false
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)

	var zero, removed, cacheBytes int64
	line := sweepJournalLine{At: now.UTC().Format(time.RFC3339Nano), Source: goCacheTrimSource, Kept: map[string]int{}, FreeBytesAfter: -1,
		FilesRemoved: &zero, CacheBytesAfter: &zero, MaxAge: settings.MaxAge.String()}
	write := func(outcome, reason, detail string) {
		line.Outcome, line.Reason, line.Detail = outcome, reason, detail
		line.DurationMs = time.Since(started).Milliseconds()
		if err := appendSweepJournal(line); err != nil {
			fmt.Fprintf(os.Stderr, "[tt relay] go build cache trim journal: %v\n", err)
		}
	}
	state, err := loadSweepState()
	if err != nil {
		write("failed", "state", err.Error())
		return true
	}
	if !goCacheTrimDue(state.GoCacheTrim, settings.Interval, now) {
		return false
	}
	attempted := func() {
		_ = updateSweepState(func(s *sweepState) error {
			if s.GoCacheTrim == nil {
				s.GoCacheTrim = &goCacheTrimState{}
			}
			s.GoCacheTrim.LastAttemptAt = now.UTC()
			return nil
		})
	}
	skip := func(reason, detail string) bool {
		attempted()
		write("skipped", reason, detail)
		return true
	}
	if fault != "" {
		return skip(sweepSkipSettings, fault)
	}
	dir, modCache, err := d.cacheDir(ctx)
	if err != nil {
		return skip(goCacheTrimSkipNoGo, err.Error())
	}
	if dir == "" || dir == "off" || !filepath.IsAbs(dir) {
		return skip(goCacheTrimSkipNotCache, "go names no build cache directory")
	}
	cache, err := openGoBuildCache(dir)
	if err != nil {
		return skip(goCacheTrimSkipNotCache, "the path is not a directory that can be opened without following a symlink")
	}
	defer cache.close()
	if why := validateGoBuildCache(cache, dir, modCache); why != "" {
		return skip(goCacheTrimSkipNotCache, why)
	}
	res := trimGoBuildCache(ctx, cache, settings.MaxAge, now)
	if res.WalkError != "" {
		attempted()
		write("failed", goCacheTrimFailWalk, res.WalkError)
		return true
	}
	removed, cacheBytes = res.FilesRemoved, res.CacheBytesAfter
	line.FilesRemoved, line.CacheBytesAfter, line.BytesFreed = &removed, &cacheBytes, res.BytesFreed
	if free, err := d.freeBytes(dir); err == nil {
		line.FreeBytesAfter = free
	}
	line.LowSpace = line.FreeBytesAfter >= 0 && line.FreeBytesAfter < settings.LowSpaceBytes
	var notes []string
	if res.StoppedEarly {
		notes = append(notes, "stopped early")
	}
	if res.Errors > 0 {
		notes = append(notes, fmt.Sprintf("%d removal error(s), first %s", res.Errors, res.FirstError))
	}
	detail := strings.Join(notes, "; ")
	if err := updateSweepState(func(s *sweepState) error {
		s.GoCacheTrim = &goCacheTrimState{LastRunAt: now.UTC(), LastAttemptAt: now.UTC(), FilesRemoved: removed, BytesFreed: res.BytesFreed, CacheBytesAfter: cacheBytes, MaxAge: settings.MaxAge.String()}
		return nil
	}); err != nil {
		// The entries are gone, so the line keeps its counts.
		write("failed", "state", strings.TrimPrefix(detail+"; "+err.Error(), "; "))
		return true
	}
	write("trimmed", "", detail)
	return true
}
