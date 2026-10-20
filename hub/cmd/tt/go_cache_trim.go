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
	"os/user"
	"path/filepath"
	"runtime"
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
//
// The verification cache trim. The same pass trims the build directories of
// the harness-owned verification cache root (scripts/verify-matrix.mjs,
// docs/objective-verification.md) with the same entry rules. It is on unless
// the owner turns it off, since that root belongs to the harness, not the
// user, and Go's own five-day trim let it reach 51 GB (wi_dad4e23eb9f43ae1).
// It trims while matrix runs hold or wait for the host lock, since it removes
// only entries no build has used for at least two hours (see
// trimVerificationBuildDirs), but not while the lock cannot be read or
// trusted; and only a root and build directories the runner itself would use.
// Above a size cap it also removes the oldest entries older than two hours
// until the build directories fit, and the journal line's reason names the
// rules that removed entries.

const (
	defaultGoCacheTrimMaxAge = 24 * time.Hour
	// defaultVerificationTrimMaxAge keeps every entry a matrix run used in
	// the last day: go refreshes a used entry's mtime once it is an hour old,
	// and the host runs the matrix many times a day on few toolchains. The
	// owner's one-day cleanup on 10-17 freed 17 GiB of the 51 GB that Go's
	// five-day trim had kept.
	defaultVerificationTrimMaxAge = 24 * time.Hour
	// defaultVerificationTrimMaxBytes caps the verification build
	// directories. On 10-18, a quiet day, they held 15.2-15.4 GiB after each
	// age trim; on 10-19, a busy one, 22.2-23.5 GiB, and they reached 29 GiB in
	// three hours of skipped passes. 20 GiB leaves a quiet day alone and keeps
	// about the newest 20 hours of a busy one, everything under 12 hours with
	// room to spare.
	defaultVerificationTrimMaxBytes = 20 << 30
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
	// goCacheTrimSkipRefused is a verification root the runner would refuse:
	// it overlaps one of the user's Go caches.
	goCacheTrimSkipRefused = "refused"
	// goCacheTrimSkipNoRoot is a verification root that cannot be named.
	goCacheTrimSkipNoRoot = "no-root"
)

// The rules that remove verification cache entries, as a trimmed line's
// reason names them, joined by "+" in this order. The age walk is age-bound,
// or lock-time-age when a read of the host lock before or during it showed a
// holder or waiter; the cap is size-cap, or low-space when the volume's free
// space was under the sweep's threshold as the cap began.
const (
	verificationRuleAge      = "age-bound"
	verificationRuleLockAge  = "lock-time-age"
	verificationRuleSizeCap  = "size-cap"
	verificationRuleLowSpace = "low-space"
)

type goCacheTrimSettings struct {
	On            bool
	MaxAge        time.Duration
	Interval      time.Duration
	LowSpaceBytes int64
	// MaxBytes is the verification trim's size cap; 0 is none. The user
	// trim has none.
	MaxBytes int64
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
	relayDuration(config, "goBuildCacheTrimMaxAge", &settings.MaxAge, minGoCacheTrimMaxAge)
	relayDuration(config, "goBuildCacheTrimInterval", &settings.Interval, minSweepInterval)
	sweep, _ := readSweepSettings()
	settings.LowSpaceBytes = sweep.LowSpaceBytes
	return settings, ""
}

// relayDuration reads one duration key of relay.json into into: a bad value
// keeps into, one under least uses least, and each says so once on stderr.
func relayDuration(config map[string]json.RawMessage, key string, into *time.Duration, least time.Duration) {
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

// readVerificationTrimSettings reads the three verificationCacheTrim keys from
// relay.json. The trim is on unless verificationCacheTrim is "off". A file
// that exists but cannot be read or parsed, or a switch that is neither "on"
// nor "off", is a fault and the run is skipped, since an owner's "off" cannot
// be read from it. A bad age uses the default and one under 2h uses 2h, as
// for goBuildCacheTrimMaxAge. verificationCacheTrimMaxGiB is a number of GiB
// read as worktreeSweepLowSpaceGiB is: a bad one uses 20 GiB and says so once
// on stderr. The interval and the low-space threshold are the sweep's, so the
// trim runs once per scheduled sweep pass.
func readVerificationTrimSettings() (goCacheTrimSettings, string) {
	sweep, _ := readSweepSettings()
	settings := goCacheTrimSettings{On: true, MaxAge: defaultVerificationTrimMaxAge, Interval: sweep.Interval, LowSpaceBytes: sweep.LowSpaceBytes, MaxBytes: defaultVerificationTrimMaxBytes}
	config, fault := readRelayConfig()
	if fault != "" {
		return settings, fault
	}
	if raw, ok := config["verificationCacheTrim"]; ok {
		var value string
		if json.Unmarshal(raw, &value) != nil || (value != "on" && value != "off") {
			return settings, `verificationCacheTrim is neither "on" nor "off"`
		}
		settings.On = value == "on"
	}
	relayDuration(config, "verificationCacheTrimMaxAge", &settings.MaxAge, minGoCacheTrimMaxAge)
	if raw, ok := config["verificationCacheTrimMaxGiB"]; ok {
		var value float64
		if json.Unmarshal(raw, &value) != nil || !(value > 0) || value > 1<<20 {
			sweepSettingsWarn("verificationCacheTrimMaxGiB", string(raw), humanBytes(defaultVerificationTrimMaxBytes))
		} else {
			settings.MaxBytes = int64(value * (1 << 30))
		}
	}
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
	// Rule is the verification trim's journal reason, and MaxBytes its cap,
	// for the notice.
	Rule     string `json:"rule,omitempty"`
	MaxBytes int64  `json:"maxBytes,omitempty"`
	// CacheDir is the build cache the last trimmed run validated; the sweep
	// reads its volume for the low-space notice. On Verification it is the
	// verification cache root.
	CacheDir string `json:"cacheDir,omitempty"`
	// Verification is the verification cache trim's own schedule and last
	// trimmed run, kept on the user trim's record so the sweep's low-space
	// notice carries both.
	Verification *goCacheTrimState `json:"verification,omitempty"`
}

// goCacheTrimDue is sweepDue for the trim's own times.
func goCacheTrimDue(state *goCacheTrimState, interval time.Duration, now time.Time) bool {
	if state == nil {
		return true
	}
	return sweepDue(sweepState{LastRunAt: state.LastRunAt, LastAttemptAt: state.LastAttemptAt}, interval, now)
}

// goCacheTrimNoticeSentence is the two trims' sentences in the sweep's
// low-space notice: the user's Go build cache, then the verification cache.
func goCacheTrimNoticeSentence(state *goCacheTrimState) string {
	var user string
	if state == nil || state.LastRunAt.IsZero() {
		user = "The Go build cache trim is off or has not run on this host."
	} else {
		user = fmt.Sprintf("Go build cache trim: last run %s removed %d entries older than %s and freed %s; the cache now holds %s.",
			state.LastRunAt.UTC().Format(time.RFC3339), state.FilesRemoved, state.MaxAge, humanBytes(state.BytesFreed), humanBytes(state.CacheBytesAfter))
	}
	var v *goCacheTrimState
	if state != nil {
		v = state.Verification
	}
	if v == nil || v.LastRunAt.IsZero() {
		return user + " The verification cache trim is off or has not trimmed on this host."
	}
	removed := "older than " + v.MaxAge
	if strings.Contains(v.Rule, verificationRuleSizeCap) || strings.Contains(v.Rule, verificationRuleLowSpace) {
		removed = fmt.Sprintf("(%s; age bound %s, size cap %s)", v.Rule, v.MaxAge, humanBytes(v.MaxBytes))
	}
	return user + fmt.Sprintf(" Verification cache trim: last run %s removed %d entries %s and freed %s; its build directories now hold %s.",
		v.LastRunAt.UTC().Format(time.RFC3339), v.FilesRemoved, removed, humanBytes(v.BytesFreed), humanBytes(v.CacheBytesAfter))
}

// verificationTrimLabel names a verification cache trim line in `--journal`.
// The line's source is goCacheTrimSource, so the sweep's printer hands it to
// goCacheTrimJournalText; lowestVolume names the root, which no user trim
// line sets.
const verificationTrimLabel = "verification-cache-trim"

func isVerificationTrimLine(l sweepJournalLine) bool {
	return l.Source == goCacheTrimSource && l.LowestVolume != ""
}

// goCacheTrimJournalText is one trim line as `--journal` prints it.
func goCacheTrimJournalText(l sweepJournalLine) string {
	label := goCacheTrimSource
	if isVerificationTrimLine(l) {
		label = verificationTrimLabel + " root=" + l.LowestVolume
	}
	if l.Outcome != "trimmed" {
		text := fmt.Sprintf("%s %s %s %s", l.At, label, l.Outcome, l.Reason)
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
	text := fmt.Sprintf("%s %s trimmed files=%d freed=%s cache=%s free=%s max-age=%s", l.At, label, files, humanBytes(l.BytesFreed), humanBytes(cache), free, l.MaxAge)
	if l.Reason != "" {
		text += " rule=" + l.Reason
	}
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
	// StopReason is stop's answer when it ended the walk.
	StopReason string
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
	return trimGoBuildCacheUntil(ctx, cache, maxAge, now, nil, nil)
}

// goCacheKeptEntry is an entry the age walk kept that the size cap may
// remove: its two-hex directory, name, size and mtime at the walk's look.
type goCacheKeptEntry struct {
	dir, name string
	size      int64
	mtime     time.Time
}

// trimGoBuildCacheUntil is trimGoBuildCache that also asks stop, when set,
// before each two-hex directory: a reason ends the walk there. keep, when set,
// is given every entry the walk keeps that is older than
// minGoCacheTrimMaxAge.
func trimGoBuildCacheUntil(ctx context.Context, cache *goCacheHandle, maxAge time.Duration, now time.Time, stop func() string, keep func(goCacheKeptEntry)) goCacheTrimResult {
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
		if stop != nil {
			if why := stop(); why != "" {
				res.StoppedEarly, res.StopReason = true, why
				break
			}
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
		trimGoCacheDirectory(sub, name, maxAge, now, &res, keep)
		_ = sub.Close()
	}
	return res
}

func trimGoCacheDirectory(sub *os.Root, dir string, maxAge time.Duration, now time.Time, res *goCacheTrimResult, keep func(goCacheKeptEntry)) {
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
		kept := func() {
			if keep != nil && now.Sub(mtime) > minGoCacheTrimMaxAge {
				keep(goCacheKeptEntry{dir: dir, name: name, size: size, mtime: mtime})
			}
		}
		if !look() {
			continue
		}
		entry := strings.HasSuffix(name, "-a") || strings.HasSuffix(name, "-d")
		if !entry || !old() {
			res.CacheBytesAfter += size
			if entry {
				kept()
			}
			continue
		}
		// A second look just before the removal: a build may have used the
		// entry, or something else may have taken its name.
		if !look() {
			continue
		}
		if !old() {
			res.CacheBytesAfter += size
			kept()
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

	// The verification cache trim's inputs. With verificationRoot nil it
	// does not run, as in the user trim's own tests.
	verificationSettings func() (goCacheTrimSettings, string)
	verificationRoot     func() (string, error)
	// goEnv is go env's GOPATH, GOMODCACHE and GOCACHE, which the root must
	// not overlap.
	goEnv func(ctx context.Context) (verificationGoEnv, error)
	// matrixLock reads the host lock: the matrix runs that hold or wait for
	// it, or why it cannot be read or trusted, which forbids a trim.
	matrixLock func() (runs int, unreadable string)
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
		now:                  time.Now,
		settings:             readGoCacheTrimSettings,
		freeBytes:            volumeFreeBytes,
		cacheDir:             func(ctx context.Context) (string, string, error) { return nativeGoCacheDir(ctx) },
		verificationSettings: readVerificationTrimSettings,
		verificationRoot:     func() (string, error) { return nativeVerificationCacheRoot() },
		goEnv:                func(ctx context.Context) (verificationGoEnv, error) { return nativeVerificationGoEnv(ctx) },
		matrixLock:           verificationTrimMatrixLock,
	}
}

// goCacheTrimAttempt is the scheduled pass's two trims: the user's Go build
// cache when its switch is on, then the verification cache root unless its
// switch is off. It reports whether either made an attempt, which is exactly
// when a journal line was written.
func goCacheTrimAttempt(ctx context.Context, d goCacheTrimDeps) bool {
	user := goBuildCacheTrimAttempt(ctx, d)
	if d.verificationRoot == nil {
		return user
	}
	return verificationTrimAttempt(ctx, d) || user
}

// goBuildCacheTrimAttempt runs one trim when the host's switch is on and a
// run is due. It reports whether it made an attempt, which is exactly when it
// wrote a journal line. One trim runs at a time on a host: the lock is taken
// first, and the state is read and the due check made only under it.
func goBuildCacheTrimAttempt(ctx context.Context, d goCacheTrimDeps) bool {
	now := d.now()
	started := time.Now()
	settings, fault := d.settings()
	if fault == "" && !settings.On {
		return false
	}
	if goCacheTrimStateFaults.blocked(now) {
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
	// stateFault journals a state read or save that failed, once per episode
	// (see stateFaultGate). It reports whether the state is good, and whether
	// a line was written.
	stateFault := func(kind string, err error, detail string) (ok, wrote bool) {
		ok, journal := goCacheTrimStateFaults.check(kind, err, now)
		if journal {
			write("failed", "state", strings.TrimPrefix(detail+"; "+err.Error(), "; "))
		}
		return ok, journal
	}
	state, err := loadSweepState()
	if ok, wrote := stateFault(stateFaultRead, err, ""); !ok {
		return wrote
	}
	if !goCacheTrimDue(state.GoCacheTrim, settings.Interval, now) {
		return false
	}
	// The attempt time is saved before the cache is resolved or touched, so
	// every outcome below is spaced, and a state file that cannot be saved
	// stops the run here.
	err = updateSweepState(func(s *sweepState) error {
		if s.GoCacheTrim == nil {
			s.GoCacheTrim = &goCacheTrimState{}
		}
		s.GoCacheTrim.LastAttemptAt = now.UTC()
		return nil
	})
	if ok, wrote := stateFault(stateFaultSave, err, ""); !ok {
		return wrote
	}
	skip := func(reason, detail string) bool {
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
	err = updateSweepState(func(s *sweepState) error {
		var verification *goCacheTrimState
		if s.GoCacheTrim != nil {
			verification = s.GoCacheTrim.Verification
		}
		s.GoCacheTrim = &goCacheTrimState{LastRunAt: now.UTC(), LastAttemptAt: now.UTC(), FilesRemoved: removed, BytesFreed: res.BytesFreed, CacheBytesAfter: cacheBytes, MaxAge: settings.MaxAge.String(), CacheDir: dir, Verification: verification}
		return nil
	})
	// The entries are gone, so the line it writes keeps its counts.
	if ok, wrote := stateFault(stateFaultSave, err, detail); !ok {
		return wrote
	}
	write("trimmed", "", detail)
	return true
}

// The verification cache trim.

// verificationTrimStateFaults is goCacheTrimStateFaults for this trim.
var verificationTrimStateFaults stateFaultGate

// verificationBuildFiles are the names a build directory may hold beside its
// two-hex directories and temporary files, as BUILD_FILES in
// scripts/verify-matrix.mjs.
var verificationBuildFiles = map[string]bool{"README": true, "trim.txt": true, "testexpire.txt": true, verificationMarkerName: true}

const verificationMarkerName = "tailterm-cache.json"

// verificationCacheRoot is scripts/verify-matrix.mjs verificationCacheRoot: an
// absolute TAILTERM_VERIFICATION_CACHE, else under HOME (or the account's
// home without one) Library/Caches on macOS and an absolute XDG_CACHE_HOME or
// .cache elsewhere.
func verificationCacheRoot(getenv func(string) string, goos string) (string, error) {
	if named := getenv("TAILTERM_VERIFICATION_CACHE"); named != "" && filepath.IsAbs(named) {
		return named, nil
	}
	home := getenv("HOME")
	if home == "" {
		u, err := user.Current()
		if err != nil {
			return "", err
		}
		home = u.HomeDir
	}
	if home == "" {
		return "", errors.New("no home directory")
	}
	if goos == "darwin" {
		return filepath.Join(home, "Library", "Caches", "tailterm-verification"), nil
	}
	if xdg := getenv("XDG_CACHE_HOME"); xdg != "" && filepath.IsAbs(xdg) {
		return filepath.Join(xdg, "tailterm-verification"), nil
	}
	return filepath.Join(home, ".cache", "tailterm-verification"), nil
}

type verificationGoEnv struct {
	GOPATH     string
	GOMODCACHE string
	GOCACHE    string
}

// The live root and go's answer. The test binary replaces both, so no test
// can name the host's verification cache.
var (
	nativeVerificationCacheRoot = func() (string, error) { return verificationCacheRoot(os.Getenv, runtime.GOOS) }
	// nativeVerificationGoEnv asks go as the runner's userGoPaths does, with
	// GOTOOLCHAIN=local from the temporary directory.
	nativeVerificationGoEnv = func(ctx context.Context) (verificationGoEnv, error) {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "go", "env", "-json", "GOPATH", "GOMODCACHE", "GOCACHE")
		cmd.Dir = os.TempDir()
		cmd.Env = append(os.Environ(), "GOTOOLCHAIN=local")
		out, err := cmd.Output()
		if err != nil {
			return verificationGoEnv{}, err
		}
		var env verificationGoEnv
		if err := json.Unmarshal(out, &env); err != nil {
			return verificationGoEnv{}, errors.New("go env printed an unexpected answer")
		}
		return env, nil
	}
)

// verificationTrimMatrixLock reads this host's matrix lock as the sweep does
// (loadMatrixExclusion) and returns its holders and waiters. A lock path that
// is refused or a file that cannot be read or trusted forbids a trim, since
// the runs it would name are unknown. Only a valid file, or an absent file at
// a resolved path, gives a count; holders and waiters do not stop the trim.
func verificationTrimMatrixLock() (int, string) {
	m, why := loadMatrixExclusion()
	if m == nil {
		return 0, "the matrix host lock cannot be read: " + why
	}
	return len(m.entries), ""
}

func matrixEntriesBusy(n int) string {
	if n == 0 {
		return ""
	}
	return fmt.Sprintf("%d matrix run(s) hold or wait for the host lock", n)
}

// validVerificationMarker is the script's validCacheMarker: a version 1
// object of the kind, whose owns are relative paths without "..".
func validVerificationMarker(raw []byte, kind string) bool {
	marker, ok := jsonObject(raw)
	if !ok {
		return false
	}
	var version float64
	if json.Unmarshal(marker["version"], &version) != nil || version != 1 {
		return false
	}
	if g, ok := jsonText(marker["kind"]); !ok || g != kind {
		return false
	}
	owns, ok := jsonArray(marker["owns"])
	if !ok {
		return false
	}
	for _, raw := range owns {
		path, ok := jsonText(raw)
		if !ok || path == "" || strings.HasPrefix(path, "/") {
			return false
		}
		for _, part := range strings.Split(path, "/") {
			if part == ".." {
				return false
			}
		}
	}
	return true
}

// readRootFile reads a regular file directly inside an opened directory, no
// larger than 64 KiB, without following a symlink.
func readRootFile(root *os.Root, name string) ([]byte, string) {
	seen, err := root.Lstat(name)
	if err != nil {
		return nil, "no " + name
	}
	if !seen.Mode().IsRegular() {
		return nil, name + " is not a regular file"
	}
	file, err := root.Open(name)
	if err != nil {
		return nil, name + " cannot be read"
	}
	defer file.Close()
	if opened, err := file.Stat(); err != nil || !os.SameFile(seen, opened) {
		return nil, name + " is not a regular file"
	}
	data, err := io.ReadAll(io.LimitReader(file, 64<<10+1))
	if err != nil || len(data) > 64<<10 {
		return nil, name + " cannot be read"
	}
	return data, ""
}

// openVerificationRoot applies the runner's refusal rules to the root and
// opens it, or returns the skip reason and why. The root must not be inside,
// equal to or contain ~/go, ~/Library/Caches/go-build or go env's GOPATH
// entries, GOMODCACHE or GOCACHE (validateCacheRoot), compared by real path;
// and it must carry a valid root marker. The runner also refuses a root
// inside its checkout; the relay has no checkout, so that rule has nothing to
// compare. A root without a marker is left alone even where the runner would
// adopt it, since the relay only trims what a run marked.
func openVerificationRoot(root string, env verificationGoEnv) (*goCacheHandle, string, string) {
	if !filepath.IsAbs(root) {
		return nil, goCacheTrimSkipNotCache, "the root is not an absolute path"
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return nil, goCacheTrimSkipRefused, "no home directory"
	}
	others := []string{filepath.Join(home, "go"), filepath.Join(home, "Library", "Caches", "go-build")}
	others = append(others, filepath.SplitList(env.GOPATH)...)
	others = append(others, env.GOMODCACHE, env.GOCACHE)
	real := canonicalPath(root)
	for _, other := range others {
		if other == "" || !filepath.IsAbs(other) {
			continue
		}
		if o := canonicalPath(other); pathHolds(real, o) || pathHolds(o, real) {
			return nil, goCacheTrimSkipRefused, "the root overlaps the user's Go cache " + other
		}
	}
	handle, err := openGoBuildCache(real)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, goCacheTrimSkipNotCache, "the root does not exist"
		}
		return nil, goCacheTrimSkipNotCache, "the root is not a directory that can be opened without following a symlink"
	}
	data, why := readRootFile(handle.root, verificationMarkerName)
	if why == "" && !validVerificationMarker(data, "root") {
		why = verificationMarkerName + " is not a valid root marker"
	}
	if why != "" {
		handle.close()
		return nil, goCacheTrimSkipNotCache, why
	}
	return handle, "", ""
}

// openVerificationBuildDir opens one name in <root>/build as a build cache
// the runner would use (validBuildDir and selectBuildDir): a real directory
// directly in build, not a symlink, with a valid build marker, holding only
// what go and the runner write there, and passing validateGoBuildCache. It
// returns nil and why otherwise.
func openVerificationBuildDir(build *os.Root, path, name, modCache string) (*goCacheHandle, string) {
	sub, err := openGoCacheDirectory(build, name)
	if err != nil {
		return nil, err.Error()
	}
	if sub == nil {
		return nil, "not a real directory"
	}
	cache := &goCacheHandle{root: sub}
	data, why := readRootFile(sub, verificationMarkerName)
	if why == "" && !validVerificationMarker(data, "build") {
		why = "not a valid build marker"
	}
	if why == "" {
		names, err := rootNames(sub)
		if err != nil {
			why = "cannot be listed"
		}
		for _, n := range names {
			if !verificationBuildFiles[n] && !isTwoHex(n) && !strings.HasSuffix(n, ".tmp") {
				why = "holds " + n
				break
			}
		}
	}
	if why == "" {
		why = validateGoBuildCache(cache, path, modCache)
	}
	if why != "" {
		cache.close()
		return nil, why
	}
	return cache, ""
}

// verificationTrimPass is one attempt's trim of the build directories: its
// settings, clock and lock and free-space readings, and what it saw and did.
type verificationTrimPass struct {
	settings goCacheTrimSettings
	now      time.Time
	lock     func() (int, string)
	free     func() (int64, error)
	// lockHeld is set by any read of the lock that showed a holder or waiter;
	// ageLockHeld is its value when the age walk ended.
	lockHeld, ageLockHeld bool
	ageRemoved            int64
	capRemoved, capFreed  int64
	// lowSpace is set when the volume was under the sweep's threshold as the
	// cap began.
	lowSpace bool
}

// stop reads the lock again: only a lock that cannot be read or trusted ends
// the walk.
func (p *verificationTrimPass) stop() string {
	runs, unreadable := p.lock()
	if unreadable != "" {
		return unreadable
	}
	if runs > 0 {
		p.lockHeld = true
	}
	return ""
}

// rules is the trimmed line's reason: the rules that removed at least one
// entry, or "".
func (p *verificationTrimPass) rules() string {
	var rules []string
	if p.ageRemoved > 0 {
		if p.ageLockHeld {
			rules = append(rules, verificationRuleLockAge)
		} else {
			rules = append(rules, verificationRuleAge)
		}
	}
	if p.capRemoved > 0 {
		if p.lowSpace {
			rules = append(rules, verificationRuleLowSpace)
		} else {
			rules = append(rules, verificationRuleSizeCap)
		}
	}
	return strings.Join(rules, "+")
}

// verificationCapCandidate is an entry the age walk kept in one build
// directory that the size cap may remove.
type verificationCapCandidate struct {
	build string
	goCacheKeptEntry
}

// trimVerificationBuildDirs trims every build directory the runner would use
// under the opened root by age, then, while the build directories hold more
// than the cap, removes the oldest entries older than minGoCacheTrimMaxAge.
// Before each two-hex directory, and every 256 removals by the cap, it reads
// the host lock again and stops at a lock that cannot be read or trusted.
//
// It runs while matrix runs hold or wait for the lock. An entry goes only when
// two no-follow looks, the second just before the removal, see a regular -a or
// -d file older than the age bound or, for the cap, than minGoCacheTrimMaxAge:
// at least two hours either way. go writes a new entry with mtime now and
// refreshes a used entry's mtime once it is an hour old, so such an entry has
// not been used by any build for at least an hour. go takes an entry that has
// gone, or an action entry whose output has gone, as a miss and rebuilds it,
// so a concurrent run's checks are slower at worst and never wrong; and
// unlinking a file a build has open does not disturb that read. A build
// directory under construction (a name ending .tmp) is never opened.
func trimVerificationBuildDirs(ctx context.Context, root *goCacheHandle, realRoot, modCache string, p *verificationTrimPass) (goCacheTrimResult, []string) {
	var res goCacheTrimResult
	build, err := openGoCacheDirectory(root.root, "build")
	if err != nil {
		res.WalkError = "build: " + err.Error()
		return res, nil
	}
	if build == nil {
		return res, []string{"no build directory"}
	}
	defer build.Close()
	names, err := rootNames(build)
	if err != nil {
		res.WalkError = "build: " + err.Error()
		return res, nil
	}
	// The trimmed build directories stay open for the cap.
	opened := map[string]*goCacheHandle{}
	defer func() {
		for _, cache := range opened {
			cache.close()
		}
	}()
	var candidates []verificationCapCandidate
	var skipped []string
	for _, name := range names {
		// A peer's directory under construction.
		if strings.HasSuffix(name, ".tmp") {
			continue
		}
		if ctx.Err() != nil {
			res.StoppedEarly = true
			break
		}
		path := filepath.Join(realRoot, "build", name)
		cache, why := openVerificationBuildDir(build, path, name, modCache)
		if cache == nil {
			skipped = append(skipped, name+" ("+why+")")
			continue
		}
		opened[name] = cache
		r := trimGoBuildCacheUntil(ctx, cache, p.settings.MaxAge, p.now, p.stop, func(e goCacheKeptEntry) {
			candidates = append(candidates, verificationCapCandidate{build: name, goCacheKeptEntry: e})
		})
		res.FilesRemoved += r.FilesRemoved
		res.BytesFreed += r.BytesFreed
		res.CacheBytesAfter += r.CacheBytesAfter
		res.Errors += r.Errors
		if res.FirstError == "" && r.FirstError != "" {
			res.FirstError = name + "/" + r.FirstError
		}
		if r.WalkError != "" {
			res.fail(name, errors.New(r.WalkError))
		}
		if r.StoppedEarly {
			res.StoppedEarly, res.StopReason = true, r.StopReason
			break
		}
	}
	p.ageRemoved, p.ageLockHeld = res.FilesRemoved, p.lockHeld
	var notes []string
	if len(skipped) > 0 {
		notes = append(notes, "left alone: "+strings.Join(skipped, ", "))
	}
	if !res.StoppedEarly {
		notes = append(notes, p.capBuildDirs(ctx, &res, opened, candidates)...)
	}
	return res, notes
}

// capBuildDirs removes the oldest candidates, oldest mtime first, until the
// build directories hold no more than the cap, and returns its notes.
func (p *verificationTrimPass) capBuildDirs(ctx context.Context, res *goCacheTrimResult, opened map[string]*goCacheHandle, candidates []verificationCapCandidate) []string {
	limit := p.settings.MaxBytes
	if limit <= 0 || res.CacheBytesAfter <= limit {
		return nil
	}
	if free, err := p.free(); err == nil && free < p.settings.LowSpaceBytes {
		p.lowSpace = true
	}
	sort.Slice(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if !a.mtime.Equal(b.mtime) {
			return a.mtime.Before(b.mtime)
		}
		if a.build != b.build {
			return a.build < b.build
		}
		if a.dir != b.dir {
			return a.dir < b.dir
		}
		return a.name < b.name
	})
	for i, c := range candidates {
		if res.CacheBytesAfter <= limit {
			break
		}
		if ctx.Err() != nil {
			res.StoppedEarly = true
			break
		}
		if i%256 == 0 {
			if why := p.stop(); why != "" {
				res.StoppedEarly, res.StopReason = true, why
				break
			}
		}
		p.removeCandidate(opened[c.build], c, res)
	}
	var notes []string
	if p.capRemoved > 0 {
		notes = append(notes, fmt.Sprintf("size cap %s: removed %d entries, freed %s", humanBytes(limit), p.capRemoved, humanBytes(p.capFreed)))
	}
	if !res.StoppedEarly && res.CacheBytesAfter > limit {
		notes = append(notes, "size cap "+humanBytes(limit)+" not reached: the rest is under 2h")
	}
	return notes
}

// removeCandidate removes one entry for the cap after a second no-follow look,
// in its two-hex directory opened again under the build directory's root. An
// entry a build has used since the walk is kept; one that has gone, or is no
// longer a regular file, no longer counts.
func (p *verificationTrimPass) removeCandidate(cache *goCacheHandle, c verificationCapCandidate, res *goCacheTrimResult) {
	where := c.build + "/" + c.dir + "/" + c.name
	sub, err := openGoCacheDirectory(cache.root, c.dir)
	if err != nil {
		res.fail(where, err)
		return
	}
	if sub == nil {
		res.CacheBytesAfter -= c.size
		return
	}
	defer sub.Close()
	info, err := sub.Lstat(c.name)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		res.CacheBytesAfter -= c.size
		return
	case err != nil:
		res.fail(where, err)
		return
	case !info.Mode().IsRegular():
		res.CacheBytesAfter -= c.size
		return
	case p.now.Sub(info.ModTime()) <= minGoCacheTrimMaxAge:
		return
	}
	if hook := goCacheTrimBeforeUnlink; hook != nil {
		hook(c.dir, c.name)
	}
	switch err := sub.Remove(c.name); {
	case err == nil:
		res.FilesRemoved++
		res.BytesFreed += info.Size()
		res.CacheBytesAfter -= c.size
		p.capRemoved++
		p.capFreed += info.Size()
	case errors.Is(err, fs.ErrNotExist):
		res.CacheBytesAfter -= c.size
	default:
		res.fail(where, err)
	}
}

// verificationTrimAttempt runs one verification cache trim when its switch
// is not off and a run is due, as goBuildCacheTrimAttempt does for the user's
// cache, with its own lock file, state record and fault gate. Every line it
// writes has source goCacheTrimSource and names the root in lowestVolume.
func verificationTrimAttempt(ctx context.Context, d goCacheTrimDeps) bool {
	now := d.now()
	started := time.Now()
	settings, fault := d.verificationSettings()
	if fault == "" && !settings.On {
		return false
	}
	if verificationTrimStateFaults.blocked(now) {
		return false
	}
	if err := os.MkdirAll(relayDir(), 0700); err != nil {
		fmt.Fprintf(os.Stderr, "[tt relay] verification cache trim: %v\n", err)
		return false
	}
	lock, err := os.OpenFile(filepath.Join(relayDir(), "verification-cache-trim.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[tt relay] verification cache trim lock: %v\n", err)
		return false
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return false
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)

	var zero, removed, cacheBytes int64
	line := sweepJournalLine{At: now.UTC().Format(time.RFC3339Nano), Source: goCacheTrimSource, Kept: map[string]int{}, FreeBytesAfter: -1,
		FilesRemoved: &zero, CacheBytesAfter: &zero, MaxAge: settings.MaxAge.String(), LowestVolume: "(unresolved)"}
	write := func(outcome, reason, detail string) {
		line.Outcome, line.Reason, line.Detail = outcome, reason, detail
		line.DurationMs = time.Since(started).Milliseconds()
		if err := appendSweepJournal(line); err != nil {
			fmt.Fprintf(os.Stderr, "[tt relay] verification cache trim journal: %v\n", err)
		}
	}
	stateFault := func(kind string, err error, detail string) (ok, wrote bool) {
		ok, journal := verificationTrimStateFaults.check(kind, err, now)
		if journal {
			write("failed", "state", strings.TrimPrefix(detail+"; "+err.Error(), "; "))
		}
		return ok, journal
	}
	state, err := loadSweepState()
	if ok, wrote := stateFault(stateFaultRead, err, ""); !ok {
		return wrote
	}
	var last *goCacheTrimState
	if state.GoCacheTrim != nil {
		last = state.GoCacheTrim.Verification
	}
	if !goCacheTrimDue(last, settings.Interval, now) {
		return false
	}
	err = updateSweepState(func(s *sweepState) error {
		if s.GoCacheTrim == nil {
			s.GoCacheTrim = &goCacheTrimState{}
		}
		if s.GoCacheTrim.Verification == nil {
			s.GoCacheTrim.Verification = &goCacheTrimState{}
		}
		s.GoCacheTrim.Verification.LastAttemptAt = now.UTC()
		return nil
	})
	if ok, wrote := stateFault(stateFaultSave, err, ""); !ok {
		return wrote
	}
	skip := func(reason, detail string) bool {
		write("skipped", reason, detail)
		return true
	}
	if fault != "" {
		return skip(sweepSkipSettings, fault)
	}
	root, err := d.verificationRoot()
	if err != nil {
		return skip(goCacheTrimSkipNoRoot, err.Error())
	}
	line.LowestVolume = root
	runs, unreadable := d.matrixLock()
	if unreadable != "" {
		return skip(sweepSkipMatrix, unreadable)
	}
	env, err := d.goEnv(ctx)
	if err != nil {
		return skip(goCacheTrimSkipNoGo, err.Error())
	}
	handle, reason, why := openVerificationRoot(root, env)
	if handle == nil {
		return skip(reason, why)
	}
	defer handle.close()
	realRoot := canonicalPath(root)
	pass := &verificationTrimPass{settings: settings, now: now, lock: d.matrixLock, free: func() (int64, error) { return d.freeBytes(realRoot) }, lockHeld: runs > 0}
	res, notes := trimVerificationBuildDirs(ctx, handle, realRoot, env.GOMODCACHE, pass)
	if res.WalkError != "" {
		write("failed", goCacheTrimFailWalk, res.WalkError)
		return true
	}
	removed, cacheBytes = res.FilesRemoved, res.CacheBytesAfter
	line.FilesRemoved, line.CacheBytesAfter, line.BytesFreed = &removed, &cacheBytes, res.BytesFreed
	if free, err := d.freeBytes(realRoot); err == nil {
		line.FreeBytesAfter = free
	}
	line.LowSpace = line.FreeBytesAfter >= 0 && line.FreeBytesAfter < settings.LowSpaceBytes
	switch {
	case res.StopReason != "":
		notes = append(notes, "stopped early: "+res.StopReason)
	case res.StoppedEarly:
		notes = append(notes, "stopped early")
	}
	if res.Errors > 0 {
		notes = append(notes, fmt.Sprintf("%d removal error(s), first %s", res.Errors, res.FirstError))
	}
	if runs > 0 {
		notes = append(notes, "trimmed while "+matrixEntriesBusy(runs))
	}
	detail := strings.Join(notes, "; ")
	rules := pass.rules()
	err = updateSweepState(func(s *sweepState) error {
		if s.GoCacheTrim == nil {
			s.GoCacheTrim = &goCacheTrimState{}
		}
		s.GoCacheTrim.Verification = &goCacheTrimState{LastRunAt: now.UTC(), LastAttemptAt: now.UTC(), FilesRemoved: removed, BytesFreed: res.BytesFreed, CacheBytesAfter: cacheBytes, MaxAge: settings.MaxAge.String(),
			Rule: rules, MaxBytes: settings.MaxBytes, CacheDir: root}
		return nil
	})
	if ok, wrote := stateFault(stateFaultSave, err, detail); !ok {
		return wrote
	}
	write("trimmed", rules, detail)
	return true
}
