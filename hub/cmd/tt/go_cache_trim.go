package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// The Go build cache trim. On a host whose owner turned it on in
// ~/.config/tailterm/relay.json, the relay unlinks build cache entries unused
// for longer than a set age, in the same pass as the scheduled sweep and
// before it. It never empties the cache: only old regular entry files directly
// inside the cache's two-hex-digit directories go, and the cache directory,
// the README, trim.txt and the module cache stay. Each run adds one line to the
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
// goCacheTrimBeforeUnlink runs between an entry's second look and its removal.
var (
	goCacheTrimAfterList    func(name string)
	goCacheTrimBeforeUnlink func(dir, name string)
)

// goCacheHandle is the cache directory, opened once as a root. All file work
// is relative to it, so nothing that happens to the path afterwards redirects
// the trim.
type goCacheHandle struct {
	root *os.Root
}

func (h *goCacheHandle) close() { _ = h.root.Close() }

// openGoBuildCache opens dir as a root, and only if the name is a real
// directory: the opened directory must be the file a no-follow look at the
// path saw, so a symlink at the path, or one put there in between, fails.
func openGoBuildCache(dir string) (*goCacheHandle, error) {
	seen, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !seen.IsDir() {
		return nil, errors.New("not a directory")
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	if opened, err := root.Stat("."); err != nil || !os.SameFile(seen, opened) {
		_ = root.Close()
		return nil, errors.New("the path changed while it was opened")
	}
	return &goCacheHandle{root: root}, nil
}

// openGoCacheDirectory opens one name inside parent as its own root, under
// the same rule: a real directory, and the same file the no-follow look saw.
// It returns nil for anything else, including a name that has gone.
func openGoCacheDirectory(parent *os.Root, name string) (*os.Root, error) {
	seen, err := parent.Lstat(name)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	if !seen.IsDir() {
		return nil, nil
	}
	sub, err := parent.OpenRoot(name)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	if opened, err := sub.Stat("."); err != nil || !os.SameFile(seen, opened) {
		_ = sub.Close()
		return nil, nil
	}
	return sub, nil
}

func rootNames(root *os.Root) ([]string, error) {
	dir, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	names, err := dir.Readdirnames(-1)
	sort.Strings(names)
	return names, err
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
	seen, err := cache.root.Lstat("README")
	if err != nil {
		return "no README"
	}
	if !seen.Mode().IsRegular() {
		return "README is not a regular file"
	}
	readme, err := cache.root.Open("README")
	if err != nil {
		return "README cannot be read"
	}
	defer readme.Close()
	if opened, err := readme.Stat(); err != nil || !os.SameFile(seen, opened) {
		return "README is not a regular file"
	}
	head := make([]byte, len(goCacheReadmeLine)+1)
	n, _ := io.ReadFull(readme, head)
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
	self, err := cache.root.Stat(".")
	if err != nil {
		return "the directory cannot be read"
	}
	if other, err := os.Stat(home); err == nil && os.SameFile(self, other) {
		return "the directory is the home directory"
	}
	if other, err := os.Stat(modCache); err == nil && os.SameFile(self, other) {
		return "the directory is the module cache"
	}
	return ""
}

type goCacheTrimResult struct {
	// FilesRemoved counts successful removals and BytesFreed their sizes.
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

// trimGoBuildCache removes the entries of an opened cache whose mtime is more
// than maxAge before now. It opens only two-hex directories, each as its own
// root and only when the name is a real directory, and in them looks only at
// regular files named as go names entries (ending -a or -d). It never
// descends further, and a root cannot reach outside its directory.
//
// One window remains, between an entry's second look and its removal: an
// entry swapped there for a symlink loses that symlink, never its target, and
// one swapped for an empty directory loses that empty directory. No regular
// file other than an old entry and no directory with anything in it can go.
func trimGoBuildCache(ctx context.Context, cache *goCacheHandle, maxAge time.Duration, now time.Time) goCacheTrimResult {
	var res goCacheTrimResult
	names, err := rootNames(cache.root)
	if err != nil {
		res.WalkError = err.Error()
		return res
	}
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
		sub, err := openGoCacheDirectory(cache.root, name)
		if err != nil {
			res.fail(name, err)
			continue
		}
		if sub == nil {
			// A symlink, a file, or a name that has gone is not a cache directory.
			continue
		}
		trimGoCacheDirectory(sub, name, maxAge, now, &res)
		_ = sub.Close()
	}
	return res
}

func trimGoCacheDirectory(sub *os.Root, dir string, maxAge time.Duration, now time.Time, res *goCacheTrimResult) {
	names, err := rootNames(sub)
	if err != nil {
		res.fail(dir, err)
		return
	}
	for _, name := range names {
		// look is a no-follow stat: the size of a regular file, or false.
		var size int64
		var mtime time.Time
		look := func() bool {
			info, err := sub.Lstat(name)
			if err != nil {
				if !errors.Is(err, fs.ErrNotExist) {
					res.fail(dir+"/"+name, err)
				}
				return false
			}
			size, mtime = info.Size(), info.ModTime()
			return info.Mode().IsRegular()
		}
		old := func() bool { return now.Sub(mtime) > maxAge }
		if !look() {
			continue
		}
		if !(strings.HasSuffix(name, "-a") || strings.HasSuffix(name, "-d")) || !old() {
			res.CacheBytesAfter += size
			continue
		}
		// A second look just before the removal: a build may have used the
		// entry, or something else may have taken its name.
		if !look() {
			continue
		}
		if !old() {
			res.CacheBytesAfter += size
			continue
		}
		if hook := goCacheTrimBeforeUnlink; hook != nil {
			hook(dir, name)
		}
		freed := size
		switch err := sub.Remove(name); {
		case err == nil:
			res.FilesRemoved++
			res.BytesFreed += freed
		case errors.Is(err, fs.ErrNotExist):
			// go's own trim got there first.
		default:
			res.fail(dir+"/"+name, err)
			if look() {
				res.CacheBytesAfter += size
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
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		// Another trim is running; it writes the receipt.
		return false
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)

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
