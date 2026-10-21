package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

// No test in this package can name the real Go build cache: the resolver the
// relay uses is replaced for the whole test binary, and every trim test hands
// its own temporary directory to the attempt.
func init() {
	nativeGoCacheDir = func(context.Context) (string, string, error) {
		return "", "", errors.New("tests never resolve the real Go build cache")
	}
	// Nor the host's verification cache root, nor go's answer for it.
	nativeVerificationCacheRoot = func() (string, error) {
		return "", errors.New("tests never resolve the real verification cache")
	}
	nativeVerificationGoEnv = func(context.Context) (verificationGoEnv, error) {
		return verificationGoEnv{}, errors.New("tests never ask go for the verification cache")
	}
}

// nativeRelaySweepSchedule is the relay's real schedule function, captured
// before the sweep tests' init replaces it.
var nativeRelaySweepSchedule = relaySweepSchedule

// goTrimFixture is a fake home with a Go build cache in the usual place, a
// sentinel module cache, a relay state directory, a clock and a free-space
// reading. Nothing outside t.TempDir() is named.
type goTrimFixture struct {
	t          *testing.T
	root       string
	home       string
	cache      string
	mod        string
	now        time.Time
	free       int64
	settings   goCacheTrimSettings
	fault      string
	resolveErr error
	resolves   int
	deps       goCacheTrimDeps
}

func writeGoCacheReadme(t *testing.T, dir string) {
	t.Helper()
	writeFixtureFile(t, filepath.Join(dir, "README"), goCacheReadmeLine+"\nRun \"go clean -cache\" if the directory is getting too large.\n")
}

func newGoTrimFixture(t *testing.T) *goTrimFixture {
	t.Helper()
	resetStateFaultGates(t)
	root := t.TempDir()
	if real, err := filepath.EvalSymlinks(root); err == nil {
		root = real
	}
	f := &goTrimFixture{t: t, root: root, home: isolatedHome(t, root), now: time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC), free: 100 << 30,
		settings: goCacheTrimSettings{On: true, MaxAge: defaultGoCacheTrimMaxAge, Interval: defaultSweepInterval, LowSpaceBytes: defaultSweepLowSpaceGiB << 30}}
	t.Setenv("TAILTERM_RELAY_STATE", filepath.Join(root, "relay"))
	t.Setenv("TAILTERM_HUB", "")
	f.cache = filepath.Join(f.home, "Library", "Caches", "go-build")
	f.mod = filepath.Join(f.home, "go", "pkg", "mod")
	writeGoCacheReadme(t, f.cache)
	writeFixtureFile(t, filepath.Join(f.cache, "trim.txt"), "1759838400\n")
	f.deps = f.newDeps()
	return f
}

func (f *goTrimFixture) newDeps() goCacheTrimDeps {
	return goCacheTrimDeps{
		now:       func() time.Time { return f.now },
		settings:  func() (goCacheTrimSettings, string) { return f.settings, f.fault },
		freeBytes: func(string) (int64, error) { return f.free, nil },
		cacheDir: func(context.Context) (string, string, error) {
			f.resolves++
			return f.cache, f.mod, f.resolveErr
		},
	}
}

func (f *goTrimFixture) attempt() bool { return goCacheTrimAttempt(context.Background(), f.deps) }

// file writes size bytes at path with an mtime age before the fixture's now.
func (f *goTrimFixture) file(path string, size int, age time.Duration) string {
	f.t.Helper()
	writeFixtureFile(f.t, path, strings.Repeat("x", size))
	at := f.now.Add(-age)
	if err := os.Chtimes(path, at, at); err != nil {
		f.t.Fatal(err)
	}
	return path
}

// entry writes a file inside the fixture's cache.
func (f *goTrimFixture) entry(rel string, size int, age time.Duration) string {
	f.t.Helper()
	return f.file(filepath.Join(f.cache, rel), size, age)
}

// oldLink makes a symlink and requires that its own mtime, which is the real
// clock's, is older than the trim age by the fixture's clock.
func (f *goTrimFixture) oldLink(target, path string) {
	f.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		f.t.Fatal(err)
	}
	if info, err := os.Lstat(path); err != nil || f.now.Sub(info.ModTime()) <= f.settings.MaxAge {
		f.t.Fatalf("the symlink %s is not old by the fixture clock: %v", path, err)
	}
}

func (f *goTrimFixture) relayState() string { return filepath.Join(f.root, "relay") }

// trimTreeDigest describes every name under root, with each file's size,
// content and mtime and each symlink's target, leaving out skip.
func trimTreeDigest(t *testing.T, root string, skip ...string) string {
	t.Helper()
	var b strings.Builder
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		for _, s := range skip {
			if path == s {
				return filepath.SkipDir
			}
		}
		rel, _ := filepath.Rel(root, path)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			target, _ := os.Readlink(path)
			fmt.Fprintf(&b, "L %s -> %s\n", rel, target)
		case d.IsDir():
			fmt.Fprintf(&b, "D %s\n", rel)
		default:
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			fmt.Fprintf(&b, "F %s %d %x %d\n", rel, info.Size(), sha256.Sum256(data), info.ModTime().UnixNano())
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func trimJournalRaw(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(sweepJournalPath())
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.Contains(line, `"source":"`+goCacheTrimSource+`"`) && !strings.Contains(line, `"lowestVolume":`) {
			out = append(out, line)
		}
	}
	return out
}

// trimLines are the user trim's lines; verificationLines the verification
// cache trim's.
func trimLines(t *testing.T) []sweepJournalLine {
	t.Helper()
	var out []sweepJournalLine
	for _, l := range sweepJournalLines(t) {
		if l.Source == goCacheTrimSource && !isVerificationTrimLine(l) {
			out = append(out, l)
		}
	}
	return out
}

func verificationLines(t *testing.T) []sweepJournalLine {
	t.Helper()
	var out []sweepJournalLine
	for _, l := range sweepJournalLines(t) {
		if isVerificationTrimLine(l) {
			out = append(out, l)
		}
	}
	return out
}

func lastTrimLine(t *testing.T) sweepJournalLine {
	t.Helper()
	lines := trimLines(t)
	if len(lines) == 0 {
		t.Fatal("no trim line in the journal")
	}
	return lines[len(lines)-1]
}

func setTrimHooks(t *testing.T, afterList func(string), beforeUnlink func(string, string)) {
	t.Helper()
	oldList, oldUnlink := goCacheTrimAfterList, goCacheTrimBeforeUnlink
	goCacheTrimAfterList, goCacheTrimBeforeUnlink = afterList, beforeUnlink
	t.Cleanup(func() { goCacheTrimAfterList, goCacheTrimBeforeUnlink = oldList, oldUnlink })
}

// a1: off unless the owner's switch is on; the switch is read at every
// attempt; while off nothing is written and the cache is never resolved.
func TestGoCacheTrimOffByDefault(t *testing.T) {
	f := newGoTrimFixture(t)
	f.deps.settings = readGoCacheTrimSettings
	old := f.entry("aa/old-a", 10, 48*time.Hour)
	off := func(label string) {
		t.Helper()
		if f.attempt() {
			t.Fatalf("%s: attempted", label)
		}
		requireExists(t, old, true)
		requireExists(t, f.relayState(), false)
		if f.resolves != 0 {
			t.Fatalf("%s: the cache was resolved %d times", label, f.resolves)
		}
	}
	off("no relay.json")
	for _, content := range []string{`{}`, `{"worktreeSweep":"on","worktreeSweepInterval":"1h"}`, `{"goBuildCacheTrim":"off"}`,
		`{"goBuildCacheTrim":"off","goBuildCacheTrimMaxAge":"3h"}`, `not json`, `[]`, `{"goBuildCacheTrimMaxAge":"3h"}`} {
		writeRelaySettings(t, f.home, content)
		off(content)
	}
	if settings, fault := readGoCacheTrimSettings(); fault != "" || settings.On {
		t.Fatalf("settings without the switch: %+v %q", settings, fault)
	}
	// On: it runs, with no restart in between.
	writeRelaySettings(t, f.home, `{"goBuildCacheTrim":"on"}`)
	if !f.attempt() || f.resolves != 1 {
		t.Fatalf("on: no attempt, %d resolves", f.resolves)
	}
	requireExists(t, old, false)
	if line := lastTrimLine(t); line.Outcome != "trimmed" || *line.FilesRemoved != 1 || line.MaxAge != "24h0m0s" {
		t.Fatalf("on: line %+v", line)
	}
	// Off again, then on again, each read at the attempt.
	again := f.entry("aa/again-a", 10, 48*time.Hour)
	writeRelaySettings(t, f.home, `{"goBuildCacheTrim":"off"}`)
	f.now = f.now.Add(2 * time.Hour)
	if f.attempt() || f.resolves != 1 || len(trimLines(t)) != 1 {
		t.Fatalf("switched off: %d resolves, %d lines", f.resolves, len(trimLines(t)))
	}
	requireExists(t, again, true)
	writeRelaySettings(t, f.home, `{"goBuildCacheTrim":"on"}`)
	if !f.attempt() || f.resolves != 2 {
		t.Fatal("switched on again: no attempt")
	}
	requireExists(t, again, false)
	// A switch that is neither on nor off is a fault: journalled, nothing
	// resolved, nothing removed.
	third := f.entry("aa/third-a", 10, 48*time.Hour)
	writeRelaySettings(t, f.home, `{"goBuildCacheTrim":"yes"}`)
	f.now = f.now.Add(2 * time.Hour)
	if !f.attempt() || f.resolves != 2 {
		t.Fatalf("bad switch: %d resolves", f.resolves)
	}
	if line := lastTrimLine(t); line.Outcome != "skipped" || line.Reason != sweepSkipSettings || !strings.Contains(line.Detail, "goBuildCacheTrim") {
		t.Fatalf("bad switch: line %+v", line)
	}
	requireExists(t, third, true)
}

// a2: entries older than the age go and newer ones stay; the age and the
// interval come from the settings, with their floors; runs are spaced.
func TestGoCacheTrimAgeAndSpacing(t *testing.T) {
	f := newGoTrimFixture(t)
	f.deps.settings = readGoCacheTrimSettings
	writeRelaySettings(t, f.home, `{"goBuildCacheTrim":"on","worktreeSweepLowSpaceGiB":20}`)
	if settings, fault := readGoCacheTrimSettings(); fault != "" || !settings.On || settings.MaxAge != 24*time.Hour || settings.Interval != time.Hour || settings.LowSpaceBytes != 20<<30 {
		t.Fatalf("defaults: %+v %q", settings, fault)
	}
	gone := []string{f.entry("aa/old-a", 10, 25*time.Hour), f.entry("0f/olddata-d", 10, 24*time.Hour+time.Second)}
	kept := []string{f.entry("aa/new-a", 10, 23*time.Hour), f.entry("bb/exact-a", 10, 24*time.Hour), f.entry("bb/future-d", 10, -time.Hour)}
	if !f.attempt() {
		t.Fatal("no attempt")
	}
	for _, path := range gone {
		requireExists(t, path, false)
	}
	for _, path := range kept {
		requireExists(t, path, true)
	}
	if line := lastTrimLine(t); *line.FilesRemoved != 2 || line.BytesFreed != 20 {
		t.Fatalf("first run %+v", line)
	}
	// Spacing: nothing inside the hour, a run at the hour.
	start := f.now
	later := f.entry("aa/later-a", 10, 48*time.Hour)
	f.now = start.Add(59 * time.Minute)
	if f.attempt() || len(trimLines(t)) != 1 || f.resolves != 1 {
		t.Fatalf("ran inside the interval: %d lines", len(trimLines(t)))
	}
	requireExists(t, later, true)
	f.now = start.Add(time.Hour)
	if !f.attempt() || len(trimLines(t)) != 2 {
		t.Fatal("not run at the interval")
	}
	requireExists(t, later, false)
	// A custom age and interval.
	writeRelaySettings(t, f.home, `{"goBuildCacheTrim":"on","goBuildCacheTrimMaxAge":"3h","goBuildCacheTrimInterval":"10m"}`)
	four, two := f.entry("aa/four-a", 10, 4*time.Hour+10*time.Minute), f.entry("aa/two-a", 10, 2*time.Hour+30*time.Minute)
	f.now = f.now.Add(9 * time.Minute)
	if f.attempt() {
		t.Fatal("ran inside a ten-minute interval")
	}
	f.now = f.now.Add(time.Minute)
	if !f.attempt() || lastTrimLine(t).MaxAge != "3h0m0s" {
		t.Fatalf("custom: %+v", lastTrimLine(t))
	}
	requireExists(t, four, false)
	requireExists(t, two, true)
	// Under the floors: 2 h for the age, 5 m for the interval. A bad value
	// uses the default.
	writeRelaySettings(t, f.home, `{"goBuildCacheTrim":"on","goBuildCacheTrimMaxAge":"1h","goBuildCacheTrimInterval":"1m"}`)
	if settings, _ := readGoCacheTrimSettings(); settings.MaxAge != 2*time.Hour || settings.Interval != 5*time.Minute {
		t.Fatalf("floors: %+v", settings)
	}
	ninety, three := f.entry("aa/ninety-a", 10, 90*time.Minute), f.entry("aa/three-a", 10, 3*time.Hour)
	f.now = f.now.Add(4 * time.Minute)
	if f.attempt() {
		t.Fatal("ran inside the five-minute floor")
	}
	f.now = f.now.Add(time.Minute)
	if !f.attempt() || lastTrimLine(t).MaxAge != "2h0m0s" {
		t.Fatalf("floor: %+v", lastTrimLine(t))
	}
	requireExists(t, ninety, true)
	requireExists(t, three, false)
	writeRelaySettings(t, f.home, `{"goBuildCacheTrim":"on","goBuildCacheTrimMaxAge":"soon","goBuildCacheTrimInterval":7}`)
	if settings, fault := readGoCacheTrimSettings(); fault != "" || settings.MaxAge != 24*time.Hour || settings.Interval != time.Hour {
		t.Fatalf("bad values: %+v %q", settings, fault)
	}
	// A skipped attempt is tried again five minutes later, not sooner.
	f.deps.settings = func() (goCacheTrimSettings, string) { return f.settings, "" }
	f.resolveErr = errors.New("go is not installed")
	f.now = f.now.Add(2 * time.Hour)
	if !f.attempt() || lastTrimLine(t).Outcome != "skipped" || lastTrimLine(t).Reason != goCacheTrimSkipNoGo {
		t.Fatalf("no go: %+v", lastTrimLine(t))
	}
	f.resolveErr = nil
	lines := len(trimLines(t))
	f.now = f.now.Add(4 * time.Minute)
	if f.attempt() || len(trimLines(t)) != lines {
		t.Fatal("retried four minutes after a skip")
	}
	f.now = f.now.Add(time.Minute)
	if !f.attempt() || lastTrimLine(t).Outcome != "trimmed" {
		t.Fatalf("not retried five minutes after a skip: %+v", lastTrimLine(t))
	}
}

// a2: two attempts cannot both run, the second reloads the state under the
// lock, and the spacing survives a relay restart.
func TestGoCacheTrimInterleavingAndRestart(t *testing.T) {
	f := newGoTrimFixture(t)
	first, second := f.entry("aa/first-a", 10, 48*time.Hour), f.entry("bb/second-a", 10, 48*time.Hour)
	entered, release := make(chan struct{}), make(chan struct{})
	paused := false
	setTrimHooks(t, nil, func(dir, name string) {
		if !paused {
			paused = true
			close(entered)
			<-release
		}
	})
	done := make(chan bool)
	go func() { done <- f.attempt() }()
	<-entered
	// B, while A holds the lock inside its trim.
	b := *f
	b.deps = b.newDeps()
	if b.attempt() {
		t.Fatal("a second attempt ran while the first held the lock")
	}
	requireExists(t, first, true)
	requireExists(t, second, true)
	if len(trimJournalRaw(t)) != 0 || b.resolves != f.resolves {
		t.Fatalf("the waiting attempt wrote %d lines", len(trimJournalRaw(t)))
	}
	close(release)
	if !<-done {
		t.Fatal("the first attempt did not run")
	}
	requireExists(t, first, false)
	requireExists(t, second, false)
	// B again at the same clock: the state it reloads says a run just ended.
	late := f.entry("aa/late-a", 10, 48*time.Hour)
	b.resolves = 0
	if b.attempt() || b.resolves != 0 || len(trimJournalRaw(t)) != 1 {
		t.Fatalf("the second attempt ran after the first: %d lines", len(trimJournalRaw(t)))
	}
	requireExists(t, late, true)
	// Restart: fresh inputs, only the state file carried over.
	state, err := loadSweepState()
	if err != nil || state.GoCacheTrim == nil || !state.GoCacheTrim.LastRunAt.Equal(f.now) {
		t.Fatalf("saved state %+v: %v", state.GoCacheTrim, err)
	}
	restarted := &goTrimFixture{t: t, root: f.root, home: f.home, cache: f.cache, mod: f.mod, now: f.now.Add(30 * time.Minute), free: f.free, settings: f.settings}
	restarted.deps = restarted.newDeps()
	if restarted.attempt() || restarted.resolves != 0 {
		t.Fatal("ran 30 minutes after the last run, after a restart")
	}
	requireExists(t, late, true)
	restarted.now = f.now.Add(61 * time.Minute)
	if !restarted.attempt() {
		t.Fatal("did not run 61 minutes after the last run, after a restart")
	}
	requireExists(t, late, false)
	if len(trimJournalRaw(t)) != 2 {
		t.Fatalf("%d lines", len(trimJournalRaw(t)))
	}
}

// a3: never a clear. Everything but old entries stays, the usual home layout
// is trimmed, and the module cache is untouched.
func TestGoCacheTrimNeverClears(t *testing.T) {
	f := newGoTrimFixture(t)
	day := 30 * 24 * time.Hour
	entries := []string{f.entry("aa/one-a", 10, day), f.entry("aa/two-d", 10, day), f.entry("ff/three-a", 10, day), f.entry("00/four-d", 10, day)}
	// Everything else is just as old, and stays.
	stays := []string{filepath.Join(f.cache, "README"), f.entry("trim.txt", 11, day), f.entry("stray-a", 10, day), f.entry("log.txt", 10, day),
		f.entry("fuzz/corpus-a", 10, day), f.entry("aa/sub/deep-a", 10, day), f.entry("aa/notes.txt", 10, day), f.entry("zz/x-a", 10, day), f.entry("abc/x-a", 10, day),
		f.entry("a/x-a", 10, day), f.entry("aaa/x-a", 10, day), f.entry("gg/x-a", 10, day)}
	if err := os.Chtimes(stays[0], f.now.Add(-day), f.now.Add(-day)); err != nil {
		t.Fatal(err)
	}
	modBefore := trimTreeDigest(t, f.mod)
	if !strings.HasPrefix(f.cache, f.home+string(filepath.Separator)) {
		t.Fatalf("the fixture cache %s is not inside the home %s", f.cache, f.home)
	}
	if !f.attempt() {
		t.Fatal("a cache in the usual home layout was not trimmed")
	}
	if line := lastTrimLine(t); line.Outcome != "trimmed" || *line.FilesRemoved != int64(len(entries)) {
		t.Fatalf("line %+v", line)
	}
	for _, path := range entries {
		requireExists(t, path, false)
		requireExists(t, filepath.Dir(path), true)
	}
	for _, path := range stays {
		requireExists(t, path, true)
	}
	requireExists(t, f.cache, true)
	if after := trimTreeDigest(t, f.mod); after != modBefore {
		t.Fatalf("the module cache changed:\n%s\n%s", modBefore, after)
	}
	// The source holds no call that could clear a cache.
	source, err := os.ReadFile("go_cache_trim.go")
	if err != nil {
		t.Fatal(err)
	}
	if found := regexp.MustCompile(`RemoveAll|"clean"`).Find(source); found != nil {
		t.Fatalf("go_cache_trim.go contains %q", found)
	}
}

// a3: a directory that is not a Go build cache, or that is or holds the home
// directory or the module cache, loses nothing.
func TestGoCacheTrimRejectsNotACache(t *testing.T) {
	day := 30 * 24 * time.Hour
	cases := []struct {
		name  string
		setup func(f *goTrimFixture)
	}{
		{"no README", func(f *goTrimFixture) {
			if err := os.Remove(filepath.Join(f.cache, "README")); err != nil {
				f.t.Fatal(err)
			}
		}},
		{"another README", func(f *goTrimFixture) {
			writeFixtureFile(f.t, filepath.Join(f.cache, "README"), "This directory holds my holiday photographs.\n")
		}},
		{"a README with more on its first line", func(f *goTrimFixture) {
			writeFixtureFile(f.t, filepath.Join(f.cache, "README"), goCacheReadmeLine+" Not really.\n")
		}},
		{"a README that is a symlink", func(f *goTrimFixture) {
			real := filepath.Join(f.root, "real-readme")
			writeFixtureFile(f.t, real, goCacheReadmeLine+"\n")
			if err := os.Remove(filepath.Join(f.cache, "README")); err != nil {
				f.t.Fatal(err)
			}
			if err := os.Symlink(real, filepath.Join(f.cache, "README")); err != nil {
				f.t.Fatal(err)
			}
		}},
		{"the cache path is a symlink", func(f *goTrimFixture) {
			link := filepath.Join(f.root, "cache-link")
			if err := os.Symlink(f.cache, link); err != nil {
				f.t.Fatal(err)
			}
			f.cache = link
		}},
		{"the cache is the home directory", func(f *goTrimFixture) {
			writeGoCacheReadme(f.t, f.home)
			f.file(filepath.Join(f.home, "aa", "old-a"), 10, day)
			f.cache = f.home
		}},
		{"the cache contains the home directory", func(f *goTrimFixture) {
			writeGoCacheReadme(f.t, f.root)
			f.file(filepath.Join(f.root, "aa", "old-a"), 10, day)
			f.cache = f.root
		}},
		{"the cache is the module cache", func(f *goTrimFixture) {
			writeGoCacheReadme(f.t, f.mod)
			f.file(filepath.Join(f.mod, "aa", "old-a"), 10, day)
			f.cache = f.mod
		}},
		{"the cache contains the module cache", func(f *goTrimFixture) {
			f.mod = filepath.Join(f.cache, "ab")
			f.file(filepath.Join(f.mod, "module-a"), 10, day)
		}},
		{"the cache is inside the module cache", func(f *goTrimFixture) {
			inner := filepath.Join(f.mod, "cache", "go-build")
			writeGoCacheReadme(f.t, inner)
			f.file(filepath.Join(inner, "aa", "old-a"), 10, day)
			f.cache = inner
		}},
		{"go names no cache", func(f *goTrimFixture) { f.cache = "off" }},
		{"a relative path", func(f *goTrimFixture) { f.cache = "Library/Caches/go-build" }},
		{"no module cache path", func(f *goTrimFixture) { f.mod = "" }},
		{"a missing directory", func(f *goTrimFixture) { f.cache = filepath.Join(f.root, "absent") }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newGoTrimFixture(t)
			f.entry("aa/old-a", 10, day)
			f.entry("bb/old-d", 10, day)
			c.setup(f)
			before := trimTreeDigest(t, f.root, f.relayState())
			if !f.attempt() {
				t.Fatal("no journalled attempt")
			}
			line := lastTrimLine(t)
			if line.Outcome != "skipped" || line.Reason != goCacheTrimSkipNotCache || line.Detail == "" || *line.FilesRemoved != 0 || *line.CacheBytesAfter != 0 || line.BytesFreed != 0 {
				t.Fatalf("line %+v", line)
			}
			if after := trimTreeDigest(t, f.root, f.relayState()); after != before {
				t.Fatalf("something changed:\n%s\n%s", before, after)
			}
		})
	}
	// A resolver that fails is skipped the same way, under its own reason.
	f := newGoTrimFixture(t)
	f.entry("aa/old-a", 10, day)
	f.resolveErr = errors.New("exec: \"go\": executable file not found in $PATH")
	before := trimTreeDigest(t, f.root, f.relayState())
	if !f.attempt() || lastTrimLine(t).Reason != goCacheTrimSkipNoGo || trimTreeDigest(t, f.root, f.relayState()) != before {
		t.Fatalf("no go: %+v", lastTrimLine(t))
	}
	// The resolver every test in this binary gets names nothing.
	if dir, mod, err := nativeGoCacheDir(context.Background()); err == nil || dir != "" || mod != "" {
		t.Fatalf("the test binary can resolve a real cache: %q %q", dir, mod)
	}
}

// a3: swapping the cache path for a symlink after validation redirects
// nothing; the trim stays in the directory it opened.
func TestGoCacheTrimCachePathSwap(t *testing.T) {
	f := newGoTrimFixture(t)
	day := 30 * 24 * time.Hour
	f.entry("aa/old-a", 10, day)
	decoy := filepath.Join(f.root, "decoy")
	writeGoCacheReadme(t, decoy)
	f.file(filepath.Join(decoy, "aa", "old-a"), 10, day)
	f.file(filepath.Join(decoy, "bb", "old-d"), 10, day)
	decoyBefore := trimTreeDigest(t, decoy)
	moved := f.cache + ".moved"
	swapped := false
	setTrimHooks(t, func(string) {
		if swapped {
			return
		}
		swapped = true
		if err := os.Rename(f.cache, moved); err != nil {
			t.Error(err)
		}
		if err := os.Symlink(decoy, f.cache); err != nil {
			t.Error(err)
		}
	}, nil)
	if !f.attempt() || !swapped {
		t.Fatal("no attempt, or the swap did not happen")
	}
	if after := trimTreeDigest(t, decoy); after != decoyBefore {
		t.Fatalf("the decoy changed:\n%s\n%s", decoyBefore, after)
	}
	requireExists(t, filepath.Join(moved, "aa", "old-a"), false)
	requireExists(t, filepath.Join(moved, "README"), true)
	if line := lastTrimLine(t); line.Outcome != "trimmed" || *line.FilesRemoved != 1 {
		t.Fatalf("line %+v", line)
	}
}

// a4: one trim at a time, no symlink followed, and only regular entry files
// directly inside two-hex directories.
func TestGoCacheTrimOnlyEntriesNoSymlinks(t *testing.T) {
	f := newGoTrimFixture(t)
	// Symlinks get the real clock's mtime, so the fixture's clock runs ahead.
	f.now = time.Now().Add(72 * time.Hour)
	day := 30 * 24 * time.Hour
	real := f.entry("aa/real-a", 10, day)
	outside := filepath.Join(f.root, "outside")
	f.file(filepath.Join(outside, "target-a"), 10, day)
	f.file(filepath.Join(outside, "dir", "inner-a"), 10, day)
	f.file(filepath.Join(outside, "hex", "old-a"), 10, day)
	f.oldLink(filepath.Join(outside, "target-a"), filepath.Join(f.cache, "aa", "link-a"))
	f.oldLink(filepath.Join(outside, "dir"), filepath.Join(f.cache, "aa", "dirlink-d"))
	f.oldLink(filepath.Join(outside, "hex"), filepath.Join(f.cache, "bb"))
	f.entry("fuzz/x-a", 10, day)
	f.oldLink(filepath.Join(f.cache, "fuzz"), filepath.Join(f.cache, "cc"))
	stays := []string{f.entry("aa/sub/x-a", 10, day), f.entry("zz/x-a", 10, day), f.entry("abc/x-a", 10, day), f.entry("aa/notes.txt", 10, day),
		f.entry("aa/entry-b", 10, day), f.entry("dd", 10, day), filepath.Join(f.cache, "fuzz", "x-a")}
	outsideBefore := trimTreeDigest(t, outside)

	// While another holder has the trim lock, nothing happens.
	if err := os.MkdirAll(f.relayState(), 0700); err != nil {
		t.Fatal(err)
	}
	holder, err := os.OpenFile(filepath.Join(f.relayState(), "go-cache-trim.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if err := syscall.Flock(int(holder.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	if f.attempt() || f.resolves != 0 || len(trimJournalRaw(t)) != 0 {
		t.Fatal("an attempt ran while the lock was held")
	}
	requireExists(t, real, true)
	if err := syscall.Flock(int(holder.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}

	if !f.attempt() {
		t.Fatal("no attempt")
	}
	requireExists(t, real, false)
	if line := lastTrimLine(t); line.Outcome != "trimmed" || *line.FilesRemoved != 1 || line.Detail != "" {
		t.Fatalf("line %+v", line)
	}
	for _, path := range stays {
		requireExists(t, path, true)
	}
	for _, link := range []string{"aa/link-a", "aa/dirlink-d", "bb", "cc"} {
		if info, err := os.Lstat(filepath.Join(f.cache, link)); err != nil || info.Mode()&fs.ModeSymlink == 0 {
			t.Fatalf("%s is no longer a symlink: %v", link, err)
		}
	}
	if after := trimTreeDigest(t, outside); after != outsideBefore {
		t.Fatalf("a symlink target changed:\n%s\n%s", outsideBefore, after)
	}
}

// a4 (i): a two-hex directory replaced by a symlink after the listing is not
// opened, whether the symlink points inside the cache or outside it.
func TestGoCacheTrimDirectorySwap(t *testing.T) {
	day := 30 * 24 * time.Hour
	for _, where := range []string{"inside", "outside"} {
		t.Run(where, func(t *testing.T) {
			f := newGoTrimFixture(t)
			f.entry("aa/old-a", 10, day)
			other := f.entry("bb/old-a", 10, day)
			decoy := filepath.Join(f.root, "decoy")
			if where == "inside" {
				decoy = filepath.Join(f.cache, "fuzz")
			}
			f.file(filepath.Join(decoy, "victim-a"), 10, day)
			f.file(filepath.Join(decoy, "victim-d"), 10, day)
			before := trimTreeDigest(t, decoy)
			swapped := false
			setTrimHooks(t, func(name string) {
				if name != "aa" {
					return
				}
				swapped = true
				if err := os.Rename(filepath.Join(f.cache, "aa"), filepath.Join(f.cache, "aa.aside")); err != nil {
					t.Error(err)
				}
				if err := os.Symlink(decoy, filepath.Join(f.cache, "aa")); err != nil {
					t.Error(err)
				}
			}, nil)
			if !f.attempt() || !swapped {
				t.Fatal("no attempt, or the swap did not happen")
			}
			if after := trimTreeDigest(t, decoy); after != before {
				t.Fatalf("the decoy changed:\n%s\n%s", before, after)
			}
			requireExists(t, filepath.Join(f.cache, "aa.aside", "old-a"), true)
			requireExists(t, other, false)
			if line := lastTrimLine(t); *line.FilesRemoved != 1 || line.Detail != "" {
				t.Fatalf("line %+v", line)
			}
		})
	}
}

// a4 (ii), (iii): an entry replaced just before its unlink. A directory in
// its place is not removed; a symlink in its place loses only the symlink.
func TestGoCacheTrimEntrySwap(t *testing.T) {
	day := 30 * 24 * time.Hour
	// A directory with anything in it, swapped in, stays with its contents.
	t.Run("non-empty directory", func(t *testing.T) {
		f := newGoTrimFixture(t)
		victim := f.entry("aa/victim-a", 10, day)
		other := f.entry("aa/keep.txt", 10, day)
		setTrimHooks(t, nil, func(dir, name string) {
			if dir != "aa" || name != "victim-a" {
				t.Errorf("hook for %s/%s", dir, name)
			}
			if err := os.Remove(victim); err != nil {
				t.Error(err)
			}
			f.file(filepath.Join(victim, "inner-a"), 10, day)
			f.file(filepath.Join(victim, "deep", "inner-d"), 10, day)
		})
		if !f.attempt() {
			t.Fatal("no attempt")
		}
		for _, path := range []string{filepath.Join(victim, "inner-a"), filepath.Join(victim, "deep", "inner-d"), other} {
			requireExists(t, path, true)
		}
		line := lastTrimLine(t)
		if line.Outcome != "trimmed" || *line.FilesRemoved != 0 || line.BytesFreed != 0 || !strings.Contains(line.Detail, "1 removal error(s), first aa/victim-a: ") {
			t.Fatalf("line %+v", line)
		}
	})
	// The stated residual: an EMPTY directory swapped in may go. No regular
	// file and no directory with contents is lost with it.
	t.Run("empty directory", func(t *testing.T) {
		f := newGoTrimFixture(t)
		victim := f.entry("aa/victim-a", 10, day)
		for _, path := range []string{f.entry("aa/keep.txt", 10, day), f.entry("aa/sub/x-a", 10, day), f.entry("aa/fresh-a", 10, time.Hour), f.entry("fuzz/x-a", 10, day)} {
			defer requireExists(t, path, true)
		}
		outside := f.file(filepath.Join(f.root, "outside", "precious-a"), 10, day)
		// rest is everything but the swapped-in directory itself.
		rest := func() string {
			swapped, _ := filepath.Rel(f.root, victim)
			return strings.ReplaceAll(trimTreeDigest(t, f.root, f.relayState()), "D "+swapped+"\n", "")
		}
		var before string
		setTrimHooks(t, nil, func(dir, name string) {
			if err := os.Remove(victim); err != nil {
				t.Error(err)
			}
			before = rest()
			if err := os.Mkdir(victim, 0755); err != nil {
				t.Error(err)
			}
		})
		if !f.attempt() {
			t.Fatal("no attempt")
		}
		// Everything that existed without the swapped-in directory is intact.
		if after := rest(); after != before {
			t.Fatalf("something besides the empty directory changed:\n%s\n%s", before, after)
		}
		requireExists(t, outside, true)
	})
	t.Run("symlink", func(t *testing.T) {
		f := newGoTrimFixture(t)
		victim := f.entry("aa/victim-a", 10, day)
		outside := f.file(filepath.Join(f.root, "outside", "precious"), 10, day)
		before := trimTreeDigest(t, filepath.Dir(outside))
		setTrimHooks(t, nil, func(dir, name string) {
			if err := os.Remove(victim); err != nil {
				t.Error(err)
			}
			if err := os.Symlink(outside, victim); err != nil {
				t.Error(err)
			}
		})
		if !f.attempt() {
			t.Fatal("no attempt")
		}
		if after := trimTreeDigest(t, filepath.Dir(outside)); after != before {
			t.Fatalf("the symlink's target changed:\n%s\n%s", before, after)
		}
		if _, err := os.Lstat(victim); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("the symlink itself: %v", err)
		}
	})
	// An entry already gone at the unlink is not an error and not a removal.
	t.Run("gone", func(t *testing.T) {
		f := newGoTrimFixture(t)
		victim := f.entry("aa/victim-a", 10, day)
		setTrimHooks(t, nil, func(dir, name string) {
			if err := os.Remove(victim); err != nil {
				t.Error(err)
			}
		})
		if !f.attempt() {
			t.Fatal("no attempt")
		}
		if line := lastTrimLine(t); *line.FilesRemoved != 0 || line.Detail != "" {
			t.Fatalf("line %+v", line)
		}
	})
	// An entry a build used after the listing is read at its turn, and kept.
	t.Run("used meanwhile", func(t *testing.T) {
		f := newGoTrimFixture(t)
		first, used := f.entry("aa/a1-a", 10, day), f.entry("aa/a2-a", 10, day)
		setTrimHooks(t, nil, func(dir, name string) {
			if name == "a1-a" {
				if err := os.Chtimes(used, f.now, f.now); err != nil {
					t.Error(err)
				}
			}
		})
		if !f.attempt() {
			t.Fatal("no attempt")
		}
		requireExists(t, first, false)
		requireExists(t, used, true)
	})
	// A cancelled run stops between directories and says so.
	t.Run("cancelled", func(t *testing.T) {
		f := newGoTrimFixture(t)
		victim := f.entry("aa/victim-a", 10, day)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if !goCacheTrimAttempt(ctx, f.deps) {
			t.Fatal("no attempt")
		}
		requireExists(t, victim, true)
		if line := lastTrimLine(t); line.Outcome != "trimmed" || line.Detail != "stopped early" || *line.FilesRemoved != 0 {
			t.Fatalf("line %+v", line)
		}
	})
}

// a5: one receipt line per run with exact counts, zeros written, readable
// through tt as text and JSON.
func TestGoCacheTrimReceipt(t *testing.T) {
	f := newGoTrimFixture(t)
	day := 30 * 24 * time.Hour
	sentinel, err := os.Stat(filepath.Join(f.cache, "aa", "entry"))
	if err != nil {
		t.Fatal(err)
	}
	// Only fresh entries: a run that removes nothing still writes its zeros.
	f.entry("aa/keep-a", 40, time.Hour)
	if !f.attempt() {
		t.Fatal("no attempt")
	}
	raw := trimJournalRaw(t)
	if len(raw) != 1 || !strings.Contains(raw[0], `"filesRemoved":0`) || !strings.Contains(raw[0], `"bytesFreed":0`) {
		t.Fatalf("zero run wrote %d lines: %v", len(raw), raw)
	}
	var fields map[string]any
	if err := json.Unmarshal([]byte(raw[0]), &fields); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"at", "source", "outcome", "filesRemoved", "bytesFreed", "cacheBytesBefore", "cacheBytesAfter", "freeBytesAfter", "lowSpace", "durationMs", "maxAge"} {
		if _, ok := fields[key]; !ok {
			t.Fatalf("line lacks %s: %s", key, raw[0])
		}
	}
	if line := lastTrimLine(t); line.At != f.now.Format(time.RFC3339Nano) || *line.CacheBytesAfter != 40+sentinel.Size() || line.FreeBytesAfter != 100<<30 || line.LowSpace {
		t.Fatalf("zero line %+v", line)
	}
	// Known sizes.
	f.entry("aa/old1-a", 100, day)
	f.entry("bb/old2-d", 250, day)
	f.entry("aa/notes.txt", 7, day)
	f.entry("fuzz/x", 1000, day)
	f.entry("aa/sub/x", 500, day)
	f.entry("top-a", 300, day)
	f.now = f.now.Add(time.Hour)
	f.free = 42 << 30
	if !f.attempt() {
		t.Fatal("no second attempt")
	}
	if len(trimJournalRaw(t)) != 2 {
		t.Fatalf("two runs wrote %d lines", len(trimJournalRaw(t)))
	}
	line := lastTrimLine(t)
	wantAfter := 40 + 7 + sentinel.Size()
	// Before counts the same files at the walk's first look, the two
	// removed entries included.
	wantBefore := wantAfter + 350
	if line.Outcome != "trimmed" || *line.FilesRemoved != 2 || line.BytesFreed != 350 || *line.CacheBytesAfter != wantAfter || line.CacheBytesBefore == nil || *line.CacheBytesBefore != wantBefore ||
		line.FreeBytesAfter != 42<<30 || line.MaxAge != "24h0m0s" {
		t.Fatalf("sized line %+v, want cache %d after and %d before", line, wantAfter, wantBefore)
	}
	state, err := loadSweepState()
	if err != nil || state.GoCacheTrim == nil || state.GoCacheTrim.FilesRemoved != 2 || state.GoCacheTrim.BytesFreed != 350 || state.GoCacheTrim.CacheBytesAfter != wantAfter || !state.GoCacheTrim.LastRunAt.Equal(f.now) {
		t.Fatalf("state %+v: %v", state.GoCacheTrim, err)
	}
	// The sweep reads the low-space volume of the cache this run validated.
	if state.GoCacheTrim.CacheDir != f.cache {
		t.Fatalf("recorded cache directory %q, want %q", state.GoCacheTrim.CacheDir, f.cache)
	}
	// A skipped line also carries both counts, as zeros.
	f.resolveErr = errors.New("no go")
	f.now = f.now.Add(time.Hour)
	if !f.attempt() {
		t.Fatal("no skipped attempt")
	}
	raw = trimJournalRaw(t)
	if len(raw) != 3 || !strings.Contains(raw[2], `"filesRemoved":0`) || !strings.Contains(raw[2], `"cacheBytesAfter":0`) || strings.Contains(raw[2], `"cacheBytesBefore"`) {
		t.Fatalf("skipped line: %v", raw)
	}
	// A skipped attempt keeps the recorded cache directory.
	if state, err := loadSweepState(); err != nil || state.GoCacheTrim.CacheDir != f.cache || !state.GoCacheTrim.LastAttemptAt.Equal(f.now) {
		t.Fatalf("state after a skip %+v: %v", state.GoCacheTrim, err)
	}
	// The tt read path, as text and as JSON.
	out, err := captureSweepStdout(t, func() error { return cmdTeamQueueSweepWorktrees(env{}, []string{"--journal"}) })
	want := fmt.Sprintf("%s go-cache-trim trimmed files=2 freed=350 B cache-before=%d B cache=%d B free=42.0 GiB max-age=24h0m0s", line.At, wantBefore, wantAfter)
	if err != nil || !strings.Contains(out, want) || !strings.Contains(out, " go-cache-trim skipped no-go: no go") || !strings.Contains(out, " go-cache-trim trimmed files=0 freed=0 B ") {
		t.Fatalf("--journal: %v\nwant %q in\n%s", err, want, out)
	}
	out, err = captureSweepStdout(t, func() error { return cmdTeamQueueSweepWorktrees(env{}, []string{"--journal", "--json"}) })
	var printed []sweepJournalLine
	if err != nil || json.Unmarshal([]byte(out), &printed) != nil || len(printed) != 3 {
		t.Fatalf("--journal --json: %v\n%s", err, out)
	}
	if got := printed[1]; got.Source != goCacheTrimSource || *got.FilesRemoved != 2 || got.BytesFreed != 350 || *got.CacheBytesAfter != wantAfter || *got.CacheBytesBefore != wantBefore || got.FreeBytesAfter != 42<<30 || got.At != line.At {
		t.Fatalf("--journal --json line %+v", got)
	}
}

const noVerificationTrim = "The verification cache trim is off or has not trimmed on this host."

// a6: the trim never posts; its receipt says when the cache's volume is low;
// the sweep's one notice per episode carries the trim's last numbers.
func TestGoCacheTrimLowSpaceNotice(t *testing.T) {
	f := newSweepFixture(t)
	f.setAgents(sweepTask, f.helper("agt_5eeb5eeb5eeb5e0a", "owner-helper", sweepTask, time.Now().Add(-time.Hour)))
	cache := filepath.Join(f.home, "Library", "Caches", "go-build")
	writeGoCacheReadme(t, cache)
	trimFree := int64(100 << 30)
	settings := goCacheTrimSettings{On: true, MaxAge: defaultGoCacheTrimMaxAge, Interval: defaultSweepInterval, LowSpaceBytes: defaultSweepLowSpaceGiB << 30}
	deps := goCacheTrimDeps{
		now:       func() time.Time { return f.now },
		settings:  func() (goCacheTrimSettings, string) { return settings, "" },
		freeBytes: func(string) (int64, error) { return trimFree, nil },
		cacheDir: func(context.Context) (string, string, error) {
			return cache, filepath.Join(f.home, "go", "pkg", "mod"), nil
		},
	}
	posts := func() int {
		stored, attempts, _, _ := f.hub.messages()
		if len(stored) != len(attempts) {
			t.Fatalf("%d stored of %d posts", len(stored), len(attempts))
		}
		return len(attempts)
	}
	// An episode before any trim: the notice says the trim has not run.
	f.free = 5 << 30
	if !f.attempt() || posts() != 1 {
		t.Fatalf("first low sweep: %d posts", posts())
	}
	_, attempts, _, _ := f.hub.messages()
	if text := attempts[0].Envelope.Body.Text; !strings.HasSuffix(text, "tt team queue sweep-worktrees --journal. The Go build cache trim is off or has not run on this host. "+noVerificationTrim) || len(text) > api.MaxEnvelopeBodyBytes {
		t.Fatalf("first notice:\n%s", text)
	}
	// The episode ends.
	f.free = 50 << 30
	f.now = f.now.Add(time.Hour)
	if !f.attempt() || posts() != 1 {
		t.Fatal("recovery")
	}
	// Trims with space fine and with space low: neither posts.
	old := filepath.Join(cache, "ab", "old-a")
	writeFixtureFile(t, old, strings.Repeat("x", 100))
	at := f.now.Add(-48 * time.Hour)
	if err := os.Chtimes(old, at, at); err != nil {
		t.Fatal(err)
	}
	if !goCacheTrimAttempt(context.Background(), deps) || lastTrimLine(t).LowSpace || posts() != 1 {
		t.Fatalf("trim with space fine: %+v, %d posts", lastTrimLine(t), posts())
	}
	requireExists(t, old, false)
	trimAt := f.now
	trimFree = 3 << 30
	f.now = f.now.Add(time.Hour)
	if !goCacheTrimAttempt(context.Background(), deps) || posts() != 1 {
		t.Fatalf("trim with space low posted: %d posts", posts())
	}
	low := lastTrimLine(t)
	if !low.LowSpace || low.FreeBytesAfter != 3<<30 || low.NoticeSeq != 0 || low.NoticePending != "" || !strings.Contains(trimJournalRaw(t)[1], `"lowSpace":true`) {
		t.Fatalf("low trim line %+v", low)
	}
	// Exactly at the threshold is not low.
	trimFree = 8 << 30
	f.now = f.now.Add(time.Hour)
	if !goCacheTrimAttempt(context.Background(), deps) || lastTrimLine(t).LowSpace {
		t.Fatalf("at the threshold: %+v", lastTrimLine(t))
	}
	if state, _ := loadSweepState(); state.Episode != nil {
		t.Fatal("a trim began a low-space episode")
	}
	// The sweep's next episode notice carries the trim's last receipt. The
	// last trimmed run removed nothing; its numbers are the state's.
	state, err := loadSweepState()
	if err != nil || state.GoCacheTrim == nil {
		t.Fatalf("state: %v", err)
	}
	sentence := fmt.Sprintf("Go build cache trim: last run %s removed 0 entries older than 24h0m0s and freed 0 B; the cache now holds %s. %s",
		f.now.UTC().Format(time.RFC3339), humanBytes(state.GoCacheTrim.CacheBytesAfter), noVerificationTrim)
	if !trimAt.Before(f.now) || goCacheTrimNoticeSentence(state.GoCacheTrim) != sentence {
		t.Fatalf("sentence %q, want %q", goCacheTrimNoticeSentence(state.GoCacheTrim), sentence)
	}
	f.free = 5 << 30
	f.now = f.now.Add(time.Hour)
	if !f.attempt() || posts() != 2 {
		t.Fatalf("second low sweep: %d posts", posts())
	}
	_, attempts, _, _ = f.hub.messages()
	text := attempts[1].Envelope.Body.Text
	if !strings.HasSuffix(text, "tt team queue sweep-worktrees --journal. "+sentence) || !strings.Contains(text, "Host "+sweepHost+" has 5.0 GiB free") || attempts[1].Text != api.RenderText(*attempts[1].Envelope) {
		t.Fatalf("second notice:\n%s", text)
	}
	// Still low, sweep and trim: no second notice in the episode.
	trimFree = 3 << 30
	for i := 0; i < 2; i++ {
		f.now = f.now.Add(time.Hour)
		if !goCacheTrimAttempt(context.Background(), deps) || !lastTrimLine(t).LowSpace {
			t.Fatalf("low trim %d: %+v", i, lastTrimLine(t))
		}
		if !f.attempt() || !lastSweepLine(t).LowSpace {
			t.Fatalf("low sweep %d: %+v", i, lastSweepLine(t))
		}
	}
	if posts() != 2 {
		t.Fatalf("%d posts in one episode", posts())
	}
	if _, writes := f.hub.snapshot(); len(writes) != 0 {
		t.Fatalf("hub writes: %v", writes)
	}
	// A removal's numbers reach the sentence too.
	if got := goCacheTrimNoticeSentence(&goCacheTrimState{LastRunAt: trimAt, FilesRemoved: 1, BytesFreed: 100, CacheBytesAfter: 2048, MaxAge: "24h0m0s"}); got !=
		"Go build cache trim: last run "+trimAt.UTC().Format(time.RFC3339)+" removed 1 entries older than 24h0m0s and freed 100 B; the cache now holds 2.0 KiB. "+noVerificationTrim {
		t.Fatalf("sentence %q", got)
	}
}

// The relay's schedule function runs the trim, from the settings file and the
// resolver, before the sweep.
func TestGoCacheTrimRelayWiring(t *testing.T) {
	f := newGoTrimFixture(t)
	old := f.entry("aa/old-a", 10, 30*24*time.Hour)
	// The schedule's clock is the real one, so make the file old by it.
	at := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(old, at, at); err != nil {
		t.Fatal(err)
	}
	fresh := f.entry("aa/fresh-a", 10, 0)
	now := time.Now()
	if err := os.Chtimes(fresh, now, now); err != nil {
		t.Fatal(err)
	}
	oldResolver := nativeGoCacheDir
	t.Cleanup(func() { nativeGoCacheDir = oldResolver })
	nativeGoCacheDir = func(context.Context) (string, string, error) { return f.cache, f.mod, nil }
	nativeRelaySweepSchedule(context.Background())
	requireExists(t, old, true)
	if len(trimJournalRaw(t)) != 0 {
		t.Fatal("the schedule trimmed with the switch off")
	}
	// The verification trim ran on the same pass, on by default, and the
	// test binary's resolver named no root.
	if lines := verificationLines(t); len(lines) != 1 || lines[0].Outcome != "skipped" || lines[0].Reason != goCacheTrimSkipNoRoot {
		t.Fatalf("verification lines %+v", lines)
	}
	writeRelaySettings(t, f.home, `{"goBuildCacheTrim":"on"}`)
	nativeRelaySweepSchedule(context.Background())
	requireExists(t, old, false)
	requireExists(t, fresh, true)
	if line := lastTrimLine(t); line.Outcome != "trimmed" || *line.FilesRemoved != 1 || line.FreeBytesAfter <= 0 {
		t.Fatalf("line %+v", line)
	}
}

// a9 (k7): a state file the trim cannot read, or cannot save, is journalled
// once per episode and tried every five minutes, not at every relay check;
// the cache is not resolved or touched meanwhile.
func TestGoCacheTrimStateFaultSpacing(t *testing.T) {
	failedState := func(t *testing.T) int {
		n := 0
		for _, line := range trimLines(t) {
			if line.Outcome == "failed" && line.Reason == "state" {
				n++
			}
		}
		return n
	}
	// checks runs the relay's 30 s checks over 20 minutes of injected time.
	checks := func(f *goTrimFixture) (attempts int) {
		for i := 0; i < 40; i++ {
			if f.attempt() {
				attempts++
			}
			f.now = f.now.Add(sweepCheckInterval)
		}
		return attempts
	}
	day := 30 * 24 * time.Hour
	t.Run("unreadable", func(t *testing.T) {
		f := newGoTrimFixture(t)
		old := f.entry("aa/old-a", 100, day)
		writeFixtureFile(t, sweepStatePath(), "{not json")
		if got := checks(f); got != 1 || failedState(t) != 1 || len(trimLines(t)) != 1 || f.resolves != 0 {
			t.Fatalf("corrupt state: %d attempts, %d lines, %d resolves", got, len(trimLines(t)), f.resolves)
		}
		if line := lastTrimLine(t); !strings.Contains(line.Detail, "not readable JSON") || line.FilesRemoved == nil || *line.FilesRemoved != 0 {
			t.Fatalf("line %+v", line)
		}
		requireExists(t, old, true)
		// Repaired: the next try trims.
		if err := os.Remove(sweepStatePath()); err != nil {
			t.Fatal(err)
		}
		f.now = f.now.Add(sweepRetryAfter)
		if !f.attempt() || f.resolves != 1 || lastTrimLine(t).Outcome != "trimmed" {
			t.Fatalf("after the repair: %d resolves, %+v", f.resolves, lastTrimLine(t))
		}
		requireExists(t, old, false)
		// Corrupt again: a new episode and one new line.
		writeFixtureFile(t, sweepStatePath(), "{not json")
		if got := checks(f); got != 1 || failedState(t) != 2 || len(trimLines(t)) != 3 || f.resolves != 1 {
			t.Fatalf("corrupt again: %d attempts, %d lines, %d resolves", got, len(trimLines(t)), f.resolves)
		}
	})
	t.Run("unsaveable", func(t *testing.T) {
		if os.Getuid() == 0 {
			t.Skip("root writes to a read-only directory")
		}
		f := newGoTrimFixture(t)
		if !f.attempt() || f.resolves != 1 {
			t.Fatalf("first attempt: %d resolves", f.resolves)
		}
		old := f.entry("aa/old-a", 100, day)
		// From here the state file reads but cannot be replaced.
		if err := os.Chmod(f.relayState(), 0500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(f.relayState(), 0700) })
		f.now = f.now.Add(time.Hour)
		if got := checks(f); got != 1 || failedState(t) != 1 || len(trimLines(t)) != 2 || f.resolves != 1 {
			t.Fatalf("unsaveable state: %d attempts, lines %+v, %d resolves", got, trimLines(t), f.resolves)
		}
		// Nothing is trimmed from a state that cannot record the attempt.
		requireExists(t, old, true)
		if err := os.Chmod(f.relayState(), 0700); err != nil {
			t.Fatal(err)
		}
		f.now = f.now.Add(sweepRetryAfter)
		if !f.attempt() || f.resolves != 2 || lastTrimLine(t).Outcome != "trimmed" {
			t.Fatalf("after the repair: %d resolves, %+v", f.resolves, lastTrimLine(t))
		}
		requireExists(t, old, false)
		if state, err := loadSweepState(); err != nil || !state.GoCacheTrim.LastRunAt.Equal(f.now) {
			t.Fatalf("state %+v: %v", state.GoCacheTrim, err)
		}
	})
	// The two ticks keep separate episodes over the one state file: each
	// journals its own line once.
	t.Run("the sweep and the trim each journal once", func(t *testing.T) {
		f := newGoTrimFixture(t)
		writeFixtureFile(t, sweepStatePath(), "{not json")
		sweep := sweepScheduleDeps{
			now:      func() time.Time { return f.now },
			settings: func() (sweepSettings, string) { return defaultSweepSettings(), "" },
			hub:      func() (*api.Client, string, bool) { t.Error("the sweep reached the hub"); return nil, "", false },
		}
		for i := 0; i < 40; i++ {
			f.attempt()
			sweepScheduleAttempt(context.Background(), sweep)
			f.now = f.now.Add(sweepCheckInterval)
		}
		lines, err := readSweepJournal(1000)
		if err != nil || len(lines) != 2 || lines[0].Source != goCacheTrimSource || lines[1].Source != "schedule" || lines[0].Reason != "state" || lines[1].Reason != "state" {
			t.Fatalf("lines %+v: %v", lines, err)
		}
	})
}

// The verification cache trim (wi_dad4e23eb9f43ae1).

// verificationFixture is a fake home with the user's Go caches, a
// verification cache root outside them with a marked build directory and
// module, speech-model and download entries, a temporary host lock and a
// relay state directory. The user trim is off, so every line is the
// verification trim's.
type verificationFixture struct {
	*goTrimFixture
	vroot     string
	build     string
	vsettings goCacheTrimSettings
	vfault    string
	env       verificationGoEnv
	lock      string
}

const verificationMarkers = `{"version":1,"kind":"%s","producer":"scripts/verify-matrix.mjs@0123","item":"wi_5eeb5eeb5eeb5e01","run":"run_1","createdAt":"2026-10-14T00:00:00.000Z","owns":[%s]}`

func newVerificationFixture(t *testing.T) *verificationFixture {
	t.Helper()
	g := newGoTrimFixture(t)
	g.settings.On = false
	reset := func() {
		verificationTrimStateFaults.mu.Lock()
		verificationTrimStateFaults.kind, verificationTrimStateFaults.lastTry = "", time.Time{}
		verificationTrimStateFaults.mu.Unlock()
	}
	reset()
	t.Cleanup(reset)
	oldHost := matrixWaitHostname
	matrixWaitHostname = func() (string, error) { return sweepHost, nil }
	t.Cleanup(func() { matrixWaitHostname = oldHost })
	f := &verificationFixture{goTrimFixture: g, vroot: filepath.Join(g.root, "vcache"),
		vsettings: goCacheTrimSettings{On: true, MaxAge: defaultVerificationTrimMaxAge, Interval: defaultSweepInterval, LowSpaceBytes: defaultSweepLowSpaceGiB << 30, MaxBytes: defaultVerificationTrimMaxBytes},
		env:       verificationGoEnv{GOPATH: filepath.Join(g.home, "go"), GOMODCACHE: g.mod, GOCACHE: g.cache},
		lock:      filepath.Join(g.root, "matrix", "host.json")}
	t.Setenv("TAILTERM_MATRIX_HOST_LOCK", f.lock)
	writeFixtureFile(t, filepath.Join(f.vroot, verificationMarkerName), fmt.Sprintf(verificationMarkers, "root", `"mod","build","speech-model","downloads"`))
	f.build = f.buildDir("go1.26.6-darwin-arm64")
	// What the trim never touches, all old by the fixture's clock.
	for _, rel := range []string{"mod/current.json", "mod/gen-0123456789abcdef/cache/download/x/@v/v1.0.0.zip", "mod/gen-0123456789abcdef/aa/old-a",
		"mod/gen-0123456789abcdef.stamps/" + strings.Repeat("a", 64) + ".json", "speech-model/r1/m/model.bin", "downloads/" + strings.Repeat("b", 64) + ".tar.gz"} {
		f.file(filepath.Join(f.vroot, rel), 10, 30*24*time.Hour)
	}
	f.deps.verificationSettings = func() (goCacheTrimSettings, string) { return f.vsettings, f.vfault }
	f.deps.verificationRoot = func() (string, error) { return f.vroot, nil }
	f.deps.goEnv = func(context.Context) (verificationGoEnv, error) { return f.env, nil }
	f.deps.matrixLock = verificationTrimMatrixLock
	return f
}

// buildDir makes a marked build directory as the runner does, with go's
// README and trim files, all old.
func (f *verificationFixture) buildDir(name string) string {
	f.t.Helper()
	dir := filepath.Join(f.vroot, "build", name)
	writeFixtureFile(f.t, filepath.Join(dir, verificationMarkerName), fmt.Sprintf(verificationMarkers, "build", `"build/`+name+`"`))
	writeGoCacheReadme(f.t, dir)
	for _, name := range []string{"README", verificationMarkerName} {
		at := f.now.Add(-30 * 24 * time.Hour)
		if err := os.Chtimes(filepath.Join(dir, name), at, at); err != nil {
			f.t.Fatal(err)
		}
	}
	f.file(filepath.Join(dir, "trim.txt"), 10, 30*24*time.Hour)
	f.file(filepath.Join(dir, "testexpire.txt"), 10, 30*24*time.Hour)
	return dir
}

func (f *verificationFixture) ventry(rel string, size int, age time.Duration) string {
	f.t.Helper()
	return f.file(filepath.Join(f.build, rel), size, age)
}

// kept is the digest of everything but the build directory's two-hex
// directories: markers, README, trim files, mod/, speech-model/ and
// downloads/.
func (f *verificationFixture) kept() string {
	f.t.Helper()
	var skip []string
	names, _ := os.ReadDir(f.build)
	for _, n := range names {
		if isTwoHex(n.Name()) {
			skip = append(skip, filepath.Join(f.build, n.Name()))
		}
	}
	return trimTreeDigest(f.t, f.vroot, skip...)
}

func lastVerificationLine(t *testing.T) sweepJournalLine {
	t.Helper()
	lines := verificationLines(t)
	if len(lines) == 0 {
		t.Fatal("no verification trim line in the journal")
	}
	return lines[len(lines)-1]
}

// a1, a5: old entries go and young ones stay, with the user trim off; the
// README, trim.txt, testexpire.txt, the markers, mod/, speech-model/ and
// downloads/ stay; one line names the root; runs are spaced by the sweep's
// interval; the state record feeds the notice.
func TestGoCacheTrimVerificationAgeAndKept(t *testing.T) {
	f := newVerificationFixture(t)
	old1 := f.ventry("aa/"+strings.Repeat("1", 64)+"-a", 100, 25*time.Hour)
	old2 := f.ventry("ab/"+strings.Repeat("2", 64)+"-d", 250, 30*24*time.Hour)
	young := f.ventry("aa/"+strings.Repeat("3", 64)+"-d", 40, 23*time.Hour)
	other := f.ventry("ab/notanentry", 7, 30*24*time.Hour)
	kept := f.kept()
	if !f.attempt() {
		t.Fatal("no attempt")
	}
	requireExists(t, old1, false)
	requireExists(t, old2, false)
	requireExists(t, young, true)
	requireExists(t, other, true)
	if got := f.kept(); got != kept {
		t.Fatalf("the trim changed what it keeps:\n%s\nwant\n%s", got, kept)
	}
	if len(trimLines(t)) != 0 || f.resolves != 0 {
		t.Fatalf("the user trim ran: %d lines, %d resolves", len(trimLines(t)), f.resolves)
	}
	line := lastVerificationLine(t)
	if line.Outcome != "trimmed" || *line.FilesRemoved != 2 || line.BytesFreed != 350 || *line.CacheBytesAfter != 47 || *line.CacheBytesBefore != 397 || line.LowestVolume != f.vroot ||
		line.MaxAge != "24h0m0s" || line.FreeBytesAfter != 100<<30 || line.LowSpace || line.Detail != "" {
		t.Fatalf("line %+v", line)
	}
	if text := goCacheTrimJournalText(line); !strings.Contains(text, " verification-cache-trim root="+f.vroot+" trimmed files=2 freed=350 B cache-before=397 B cache=47 B") {
		t.Fatalf("--journal text %q", text)
	}
	state, err := loadSweepState()
	if err != nil || state.GoCacheTrim == nil || state.GoCacheTrim.Verification == nil || state.GoCacheTrim.Verification.CacheDir != f.vroot || !state.GoCacheTrim.LastRunAt.IsZero() {
		t.Fatalf("state %+v %v", state.GoCacheTrim, err)
	}
	if got := goCacheTrimNoticeSentence(state.GoCacheTrim); got != "The Go build cache trim is off or has not run on this host. Verification cache trim: last run "+
		f.now.UTC().Format(time.RFC3339)+" removed 2 entries older than 24h0m0s and freed 350 B; its build directories now hold 47 B." {
		t.Fatalf("sentence %q", got)
	}
	// Not due again within the sweep's interval.
	again := f.ventry("ac/"+strings.Repeat("4", 64)+"-a", 10, 48*time.Hour)
	f.now = f.now.Add(30 * time.Minute)
	if f.attempt() || len(verificationLines(t)) != 1 {
		t.Fatal("ran again inside the interval")
	}
	requireExists(t, again, true)
	f.now = f.now.Add(31 * time.Minute)
	if !f.attempt() || len(verificationLines(t)) != 2 {
		t.Fatal("not run after the interval")
	}
	requireExists(t, again, false)
	// The user trim's own run keeps the verification record.
	f.settings.On = true
	f.entry("aa/user-a", 10, 48*time.Hour)
	f.now = f.now.Add(2 * time.Hour)
	if !f.attempt() || len(trimLines(t)) != 1 {
		t.Fatal("the user trim did not run")
	}
	if state, _ := loadSweepState(); state.GoCacheTrim.Verification == nil || state.GoCacheTrim.Verification.LastRunAt != f.now.UTC() || state.GoCacheTrim.LastRunAt != f.now.UTC() {
		t.Fatalf("state after both %+v", state.GoCacheTrim)
	}
}

// a3, a5: a lock file that cannot be trusted or a refused lock path skips
// the pass with the reason and removes nothing; a free lock file or an absent
// one at a set path trims by the age bound. A holder or waiter no longer
// skips (TestGoCacheTrimVerificationUnderHeldLock).
func TestGoCacheTrimVerificationHostLock(t *testing.T) {
	f := newVerificationFixture(t)
	old := f.ventry("aa/"+strings.Repeat("1", 64)+"-a", 10, 48*time.Hour)
	cases := []struct {
		name  string
		setup func()
		want  string
	}{
		{"invalid", func() { writeFixtureFile(t, f.lock, "not json") }, "the matrix host lock cannot be read: not a JSON object"},
		{"another host", func() { writeMatrixLock(t, "other-host", nil, nil) }, "the matrix host lock cannot be read: written by another host"},
		{"refused default", func() { t.Setenv("TAILTERM_MATRIX_HOST_LOCK", "") }, "the matrix host lock cannot be read: "},
	}
	for _, c := range cases {
		c.setup()
		f.now = f.now.Add(2 * time.Hour)
		before := trimTreeDigest(t, f.vroot)
		if !f.attempt() {
			t.Fatalf("%s: no attempt", c.name)
		}
		line := lastVerificationLine(t)
		if line.Outcome != "skipped" || line.Reason != sweepSkipMatrix || !strings.HasPrefix(line.Detail, c.want) || line.LowestVolume != f.vroot {
			t.Fatalf("%s: line %+v", c.name, line)
		}
		requireExists(t, old, true)
		if after := trimTreeDigest(t, f.vroot); after != before {
			t.Fatalf("%s: the tree changed", c.name)
		}
		// A skip is retried after five minutes, not a full interval.
		f.now = f.now.Add(4 * time.Minute)
		if f.attempt() {
			t.Fatalf("%s: retried within five minutes", c.name)
		}
		t.Setenv("TAILTERM_MATRIX_HOST_LOCK", f.lock)
	}
	writeMatrixLock(t, sweepHost, nil, nil)
	f.now = f.now.Add(10 * time.Minute)
	if !f.attempt() {
		t.Fatal("free lock: no attempt")
	}
	if line := lastVerificationLine(t); line.Outcome != "trimmed" || line.Reason != verificationRuleAge || line.Detail != "" ||
		!strings.Contains(goCacheTrimJournalText(line), " max-age=24h0m0s rule=age-bound") {
		t.Fatalf("free lock: %+v", line)
	}
	requireExists(t, old, false)
	if err := os.Remove(f.lock); err != nil {
		t.Fatal(err)
	}
	again := f.ventry("aa/"+strings.Repeat("2", 64)+"-a", 10, 48*time.Hour)
	f.now = f.now.Add(2 * time.Hour)
	if !f.attempt() || lastVerificationLine(t).Outcome != "trimmed" || lastVerificationLine(t).Reason != verificationRuleAge {
		t.Fatalf("absent lock: %+v", lastVerificationLine(t))
	}
	requireExists(t, again, false)
}

// a1, a5: a run that registers mid-pass no longer stops the walk, and the
// line names the lock-time rule; a lock that turns unreadable mid-pass stops
// it before the next two-hex directory.
func TestGoCacheTrimVerificationStopsForANewRun(t *testing.T) {
	f := newVerificationFixture(t)
	first := f.ventry("aa/"+strings.Repeat("1", 64)+"-a", 10, 48*time.Hour)
	second := f.ventry("ab/"+strings.Repeat("2", 64)+"-a", 10, 48*time.Hour)
	setTrimHooks(t, func(name string) {
		if name == "aa" {
			writeMatrixLock(t, sweepHost, nil, []map[string]any{sweepMatrixEntry("w1", filepath.Join(f.root, "wt"), false)})
		}
	}, nil)
	if !f.attempt() {
		t.Fatal("no attempt")
	}
	requireExists(t, first, false)
	requireExists(t, second, false)
	if line := lastVerificationLine(t); line.Outcome != "trimmed" || *line.FilesRemoved != 2 || line.Reason != verificationRuleLockAge || line.Detail != "" {
		t.Fatalf("line %+v", line)
	}
	// A lock that cannot be read mid-pass stops the walk.
	third := f.ventry("aa/"+strings.Repeat("3", 64)+"-a", 10, 48*time.Hour)
	fourth := f.ventry("ab/"+strings.Repeat("4", 64)+"-a", 10, 48*time.Hour)
	writeMatrixLock(t, sweepHost, nil, nil)
	setTrimHooks(t, func(name string) {
		if name == "aa" {
			writeFixtureFile(t, f.lock, "not json")
		}
	}, nil)
	f.now = f.now.Add(2 * time.Hour)
	if !f.attempt() {
		t.Fatal("no attempt")
	}
	requireExists(t, third, false)
	requireExists(t, fourth, true)
	if line := lastVerificationLine(t); line.Outcome != "trimmed" || *line.FilesRemoved != 1 || line.Reason != verificationRuleAge ||
		line.Detail != "stopped early: the matrix host lock cannot be read: not a JSON object" {
		t.Fatalf("line %+v", line)
	}
}

// a1, a2, a5: an unmarked or wrongly marked root, a root that overlaps a user
// Go cache either way, and an absent root are left alone with a skip; an
// unmarked, foreign or symlinked build directory is left alone beside a good
// one.
func TestGoCacheTrimVerificationRefusals(t *testing.T) {
	f := newVerificationFixture(t)
	old := f.ventry("aa/"+strings.Repeat("1", 64)+"-a", 10, 48*time.Hour)
	marker := filepath.Join(f.vroot, verificationMarkerName)
	good, _ := os.ReadFile(marker)
	skipped := func(label, reason, detail string) {
		t.Helper()
		f.now = f.now.Add(2 * time.Hour)
		before := trimTreeDigest(t, f.root, f.relayState())
		if !f.attempt() {
			t.Fatalf("%s: no attempt", label)
		}
		if line := lastVerificationLine(t); line.Outcome != "skipped" || line.Reason != reason || !strings.Contains(line.Detail, detail) {
			t.Fatalf("%s: line %+v", label, line)
		}
		if after := trimTreeDigest(t, f.root, f.relayState()); after != before {
			t.Fatalf("%s: the tree changed", label)
		}
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	skipped("no marker", goCacheTrimSkipNotCache, "no tailterm-cache.json")
	writeFixtureFile(t, marker, fmt.Sprintf(verificationMarkers, "build", `"build"`))
	skipped("build marker on the root", goCacheTrimSkipNotCache, "not a valid root marker")
	writeFixtureFile(t, marker, fmt.Sprintf(verificationMarkers, "root", `"../build"`))
	skipped("owns outside", goCacheTrimSkipNotCache, "not a valid root marker")
	writeFixtureFile(t, marker, `{"version":2,"kind":"root","owns":[]}`)
	skipped("version 2", goCacheTrimSkipNotCache, "not a valid root marker")
	writeFixtureFile(t, marker, string(good))

	// Overlaps, both ways, compared by real path.
	inside := filepath.Join(f.home, "go", "verify")
	writeFixtureFile(t, filepath.Join(inside, verificationMarkerName), string(good))
	f.vroot = inside
	skipped("inside GOPATH", goCacheTrimSkipRefused, "overlaps the user's Go cache "+filepath.Join(f.home, "go"))
	f.vroot = filepath.Join(f.home, "Library")
	writeFixtureFile(t, filepath.Join(f.vroot, verificationMarkerName), string(good))
	skipped("holds GOCACHE", goCacheTrimSkipRefused, "overlaps the user's Go cache")
	elsewhere := filepath.Join(f.root, "elsewhere", "cache")
	writeFixtureFile(t, filepath.Join(elsewhere, "x"), "x")
	f.env.GOCACHE = elsewhere
	f.vroot = filepath.Join(f.root, "elsewhere")
	writeFixtureFile(t, filepath.Join(f.vroot, verificationMarkerName), string(good))
	skipped("holds go env GOCACHE", goCacheTrimSkipRefused, "overlaps the user's Go cache "+elsewhere)
	link := filepath.Join(f.root, "link-to-home-go")
	if err := os.Symlink(filepath.Join(f.home, "go"), link); err != nil {
		t.Fatal(err)
	}
	f.vroot = filepath.Join(link, "verify")
	skipped("through a symlink into GOPATH", goCacheTrimSkipRefused, "overlaps the user's Go cache")
	f.vroot = filepath.Join(f.root, "absent")
	skipped("absent", goCacheTrimSkipNotCache, "the root does not exist")
	f.vroot = "relative/root"
	skipped("relative", goCacheTrimSkipNotCache, "not an absolute path")
	f.vroot = filepath.Join(f.root, "vcache")
	f.env.GOCACHE = f.cache
	requireExists(t, old, true)

	// Build directories: unmarked, foreign content, no README, symlinked.
	unmarked := filepath.Join(f.vroot, "build", "go1.25.0-darwin-arm64")
	writeGoCacheReadme(t, unmarked)
	u := f.file(filepath.Join(unmarked, "aa", strings.Repeat("5", 64)+"-a"), 10, 48*time.Hour)
	foreign := f.buildDir("go1.24.0-darwin-arm64")
	fo := f.file(filepath.Join(foreign, "aa", strings.Repeat("6", 64)+"-a"), 10, 48*time.Hour)
	writeFixtureFile(t, filepath.Join(foreign, "notes.txt"), "mine")
	noReadme := f.buildDir("go1.23.0-darwin-arm64")
	nr := f.file(filepath.Join(noReadme, "aa", strings.Repeat("7", 64)+"-a"), 10, 48*time.Hour)
	if err := os.Remove(filepath.Join(noReadme, "README")); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(f.root, "outside-build")
	writeFixtureFile(t, filepath.Join(target, verificationMarkerName), fmt.Sprintf(verificationMarkers, "build", `"build/go1.22.0-darwin-arm64"`))
	writeGoCacheReadme(t, target)
	tg := f.file(filepath.Join(target, "aa", strings.Repeat("8", 64)+"-a"), 10, 48*time.Hour)
	if err := os.Symlink(target, filepath.Join(f.vroot, "build", "go1.22.0-darwin-arm64")); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(2 * time.Hour)
	if !f.attempt() {
		t.Fatal("no attempt")
	}
	requireExists(t, old, false)
	for _, path := range []string{u, fo, nr, tg} {
		requireExists(t, path, true)
	}
	line := lastVerificationLine(t)
	for _, want := range []string{"go1.25.0-darwin-arm64 (no tailterm-cache.json)", "go1.24.0-darwin-arm64 (holds notes.txt)", "go1.23.0-darwin-arm64 (no README)", "go1.22.0-darwin-arm64 (not a real directory)"} {
		if line.Outcome != "trimmed" || *line.FilesRemoved != 1 || !strings.Contains(line.Detail, want) {
			t.Fatalf("line %+v lacks %q", line, want)
		}
	}
}

// a1, a5: the switch, the age and the cap come from relay.json at every
// attempt: on by default, off stops it with no line, a bad switch is a fault,
// the age has a 2h floor and the cap defaults to 20 GiB.
func TestGoCacheTrimVerificationSettings(t *testing.T) {
	f := newVerificationFixture(t)
	f.deps.verificationSettings = readVerificationTrimSettings
	old := f.ventry("aa/"+strings.Repeat("1", 64)+"-a", 10, 3*time.Hour)
	writeRelaySettings(t, f.home, `{"verificationCacheTrim":"off","verificationCacheTrimMaxAge":"1h"}`)
	if f.attempt() || len(verificationLines(t)) != 0 {
		t.Fatal("ran with the switch off")
	}
	writeRelaySettings(t, f.home, `{"verificationCacheTrim":"yes"}`)
	if !f.attempt() {
		t.Fatal("bad switch: no attempt")
	}
	if line := lastVerificationLine(t); line.Outcome != "skipped" || line.Reason != sweepSkipSettings || !strings.Contains(line.Detail, "verificationCacheTrim") {
		t.Fatalf("bad switch: %+v", line)
	}
	writeRelaySettings(t, f.home, `not json`)
	f.now = f.now.Add(2 * time.Hour)
	if !f.attempt() || lastVerificationLine(t).Reason != sweepSkipSettings {
		t.Fatalf("unreadable relay.json: %+v", lastVerificationLine(t))
	}
	requireExists(t, old, true)
	// Default on, at 24h: a 3h entry stays.
	writeRelaySettings(t, f.home, `{}`)
	f.now = f.now.Add(2 * time.Hour)
	old = f.ventry("aa/"+strings.Repeat("1", 64)+"-a", 10, 3*time.Hour)
	if !f.attempt() || lastVerificationLine(t).MaxAge != "24h0m0s" {
		t.Fatalf("default: %+v", lastVerificationLine(t))
	}
	requireExists(t, old, true)
	// 1h is raised to the 2h floor, so the 3h entry goes.
	writeRelaySettings(t, f.home, `{"verificationCacheTrim":"on","verificationCacheTrimMaxAge":"1h"}`)
	f.now = f.now.Add(2 * time.Hour)
	old = f.ventry("aa/"+strings.Repeat("1", 64)+"-a", 10, 3*time.Hour)
	if !f.attempt() || lastVerificationLine(t).MaxAge != "2h0m0s" {
		t.Fatalf("floor: %+v", lastVerificationLine(t))
	}
	requireExists(t, old, false)
	if settings, fault := readVerificationTrimSettings(); fault != "" || !settings.On || settings.Interval != defaultSweepInterval {
		t.Fatalf("settings %+v %q", settings, fault)
	}
	// The cap is 20 GiB unless verificationCacheTrimMaxGiB says otherwise; a
	// bad value says so once and uses 20 GiB.
	sweepSettingsWarned.Clear()
	t.Cleanup(sweepSettingsWarned.Clear)
	read := func(content string) goCacheTrimSettings {
		t.Helper()
		writeRelaySettings(t, f.home, content)
		settings, fault := readVerificationTrimSettings()
		if fault != "" {
			t.Fatalf("%s: fault %q", content, fault)
		}
		return settings
	}
	if got := read(`{}`).MaxBytes; got != 20<<30 {
		t.Fatalf("default cap %d", got)
	}
	if got := read(`{"verificationCacheTrimMaxGiB":15}`).MaxBytes; got != 15<<30 {
		t.Fatalf("15 GiB cap %d", got)
	}
	if got := read(`{"verificationCacheTrimMaxGiB":2.5}`).MaxBytes; got != 5<<29 {
		t.Fatalf("2.5 GiB cap %d", got)
	}
	var stderr strings.Builder
	restore := captureStderr(t, &stderr)
	var bad []int64
	for _, content := range []string{`{"verificationCacheTrimMaxGiB":"20"}`, `{"verificationCacheTrimMaxGiB":0}`, `{"verificationCacheTrimMaxGiB":-1}`, `{"verificationCacheTrimMaxGiB":2000000}`, `{"verificationCacheTrimMaxGiB":0}`} {
		bad = append(bad, read(content).MaxBytes)
	}
	restore()
	for _, got := range bad {
		if got != 20<<30 {
			t.Fatalf("bad values gave caps %v", bad)
		}
	}
	if strings.Count(stderr.String(), "verificationCacheTrimMaxGiB=0 is not usable; using 20.0 GiB") != 1 || strings.Count(stderr.String(), "verificationCacheTrimMaxGiB=") != 4 {
		t.Fatalf("bad values said %q", stderr.String())
	}
}

// a2: the root is resolved as the runner resolves it; the test binary never
// resolves the live one.
func TestGoCacheTrimVerificationRoot(t *testing.T) {
	env := func(vars map[string]string) func(string) string { return func(k string) string { return vars[k] } }
	cases := []struct {
		vars map[string]string
		goos string
		want string
	}{
		{map[string]string{"TAILTERM_VERIFICATION_CACHE": "/v/root", "HOME": "/h"}, "darwin", "/v/root"},
		{map[string]string{"TAILTERM_VERIFICATION_CACHE": "relative", "HOME": "/h"}, "darwin", "/h/Library/Caches/tailterm-verification"},
		{map[string]string{"HOME": "/h", "XDG_CACHE_HOME": "/x"}, "darwin", "/h/Library/Caches/tailterm-verification"},
		{map[string]string{"HOME": "/h", "XDG_CACHE_HOME": "/x"}, "linux", "/x/tailterm-verification"},
		{map[string]string{"HOME": "/h", "XDG_CACHE_HOME": "x"}, "linux", "/h/.cache/tailterm-verification"},
		{map[string]string{"HOME": "/h"}, "linux", "/h/.cache/tailterm-verification"},
	}
	for _, c := range cases {
		if got, err := verificationCacheRoot(env(c.vars), c.goos); err != nil || got != c.want {
			t.Fatalf("%v on %s: %q %v, want %q", c.vars, c.goos, got, err, c.want)
		}
	}
	if root, err := nativeVerificationCacheRoot(); err == nil || root != "" {
		t.Fatalf("the test binary resolved a verification root: %q", root)
	}
	if _, err := nativeVerificationGoEnv(context.Background()); err == nil {
		t.Fatal("the test binary asked go for the verification cache")
	}
}

// a1, a4: the relay's schedule runs the verification trim on every pass,
// with goBuildCacheTrim off, and --journal prints its line.
func TestGoCacheTrimVerificationRelayWiring(t *testing.T) {
	f := newVerificationFixture(t)
	old := f.ventry("aa/"+strings.Repeat("1", 64)+"-a", 10, 0)
	at := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(old, at, at); err != nil {
		t.Fatal(err)
	}
	fresh := f.ventry("aa/"+strings.Repeat("2", 64)+"-a", 10, 0)
	now := time.Now()
	if err := os.Chtimes(fresh, now, now); err != nil {
		t.Fatal(err)
	}
	oldRoot, oldEnv := nativeVerificationCacheRoot, nativeVerificationGoEnv
	t.Cleanup(func() { nativeVerificationCacheRoot, nativeVerificationGoEnv = oldRoot, oldEnv })
	nativeVerificationCacheRoot = func() (string, error) { return f.vroot, nil }
	nativeVerificationGoEnv = func(context.Context) (verificationGoEnv, error) { return f.env, nil }
	writeRelaySettings(t, f.home, `{"goBuildCacheTrim":"off"}`)
	nativeRelaySweepSchedule(context.Background())
	requireExists(t, old, false)
	requireExists(t, fresh, true)
	if line := lastVerificationLine(t); line.Outcome != "trimmed" || *line.FilesRemoved != 1 || line.FreeBytesAfter <= 0 || len(trimLines(t)) != 0 {
		t.Fatalf("line %+v", line)
	}
	out, err := captureStdout(t, func() error { return printSweepJournal(10, false) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "verification-cache-trim root="+f.vroot+" trimmed files=1 freed=10 B") {
		t.Fatalf("--journal printed:\n%s", out)
	}
}

// a1, a5 (t1, t4): with a holder, and with a waiter, entries past the age
// bound go and the line names lock-time-age; a younger entry, a build
// directory under construction and everything outside the two-hex
// directories stay; at the 2h floor a 1h entry stays and a 3h one goes.
func TestGoCacheTrimVerificationUnderHeldLock(t *testing.T) {
	for _, holder := range []bool{true, false} {
		f := newVerificationFixture(t)
		entry := sweepMatrixEntry("r1", filepath.Join(f.root, "wt"), holder)
		if holder {
			writeMatrixLock(t, sweepHost, []map[string]any{entry}, nil)
		} else {
			writeMatrixLock(t, sweepHost, nil, []map[string]any{entry})
		}
		old1 := f.ventry("aa/"+strings.Repeat("1", 64)+"-a", 100, 25*time.Hour)
		old2 := f.ventry("ab/"+strings.Repeat("2", 64)+"-d", 250, 30*24*time.Hour)
		young := f.ventry("aa/"+strings.Repeat("3", 64)+"-d", 40, 23*time.Hour)
		// A peer's build directory under construction, old by the clock.
		building := filepath.Join(f.vroot, "build", "go1.27.0-darwin-arm64.tmp")
		writeGoCacheReadme(t, building)
		inBuilding := f.file(filepath.Join(building, "aa", strings.Repeat("5", 64)+"-a"), 10, 30*24*time.Hour)
		kept := f.kept()
		if !f.attempt() {
			t.Fatalf("holder=%v: no attempt", holder)
		}
		requireExists(t, old1, false)
		requireExists(t, old2, false)
		requireExists(t, young, true)
		requireExists(t, inBuilding, true)
		if got := f.kept(); got != kept {
			t.Fatalf("holder=%v: the trim changed what it keeps:\n%s\nwant\n%s", holder, got, kept)
		}
		line := lastVerificationLine(t)
		if line.Outcome != "trimmed" || line.Reason != verificationRuleLockAge || *line.FilesRemoved != 2 || line.BytesFreed != 350 || *line.CacheBytesAfter != 40 ||
			line.Detail != "trimmed while 1 matrix run(s) hold or wait for the host lock" {
			t.Fatalf("holder=%v: line %+v", holder, line)
		}
		if text := goCacheTrimJournalText(line); !strings.Contains(text, " trimmed files=2 freed=350 B cache-before=390 B cache=40 B free=100.0 GiB max-age=24h0m0s rule=lock-time-age; trimmed while 1 matrix run(s)") {
			t.Fatalf("holder=%v: --journal text %q", holder, text)
		}
		// An age-only run keeps the notice's wording.
		state, _ := loadSweepState()
		if got := goCacheTrimNoticeSentence(state.GoCacheTrim); !strings.HasSuffix(got, " Verification cache trim: last run "+
			f.now.UTC().Format(time.RFC3339)+" removed 2 entries older than 24h0m0s and freed 350 B; its build directories now hold 40 B.") {
			t.Fatalf("holder=%v: sentence %q", holder, got)
		}
		// The 2h floor holds under the lock.
		f.vsettings.MaxAge = minGoCacheTrimMaxAge
		f.now = f.now.Add(2 * time.Hour)
		hour := f.ventry("ac/"+strings.Repeat("6", 64)+"-a", 10, time.Hour)
		three := f.ventry("ac/"+strings.Repeat("7", 64)+"-d", 10, 3*time.Hour)
		if !f.attempt() {
			t.Fatalf("holder=%v: no attempt at the floor", holder)
		}
		requireExists(t, hour, true)
		requireExists(t, three, false)
		requireExists(t, inBuilding, true)
		if line := lastVerificationLine(t); line.Outcome != "trimmed" || line.Reason != verificationRuleLockAge || line.MaxAge != "2h0m0s" {
			t.Fatalf("holder=%v: floor line %+v", holder, line)
		}
	}
}

// a2, a5 (t2, t4): above the cap's margin the oldest entries go first until
// the build directories fit it; entries under 2h are never removed, so the
// margin may stay unreached; an entry a build refreshed since the walk is
// kept; with a holder the age and cap rules combine.
func TestGoCacheTrimVerificationSizeCap(t *testing.T) {
	f := newVerificationFixture(t)
	f.vsettings.MaxBytes = 1000
	e1 := f.ventry("aa/"+strings.Repeat("1", 64)+"-a", 300, 20*time.Hour)
	e2 := f.ventry("ab/"+strings.Repeat("2", 64)+"-a", 300, 18*time.Hour)
	e3 := f.ventry("ac/"+strings.Repeat("3", 64)+"-d", 300, 10*time.Hour)
	e4 := f.ventry("ad/"+strings.Repeat("4", 64)+"-a", 300, 3*time.Hour)
	e5 := f.ventry("ae/"+strings.Repeat("5", 64)+"-a", 300, time.Hour)
	other := f.ventry("aa/notanentry", 7, 30*24*time.Hour)
	kept := f.kept()
	if !f.attempt() {
		t.Fatal("no attempt")
	}
	// The cap trims to its margin, 750 B.
	requireExists(t, e1, false)
	requireExists(t, e2, false)
	requireExists(t, e3, false)
	for _, path := range []string{e4, e5, other} {
		requireExists(t, path, true)
	}
	if got := f.kept(); got != kept {
		t.Fatalf("the cap changed what it keeps:\n%s\nwant\n%s", got, kept)
	}
	line := lastVerificationLine(t)
	if line.Outcome != "trimmed" || line.Reason != verificationRuleSizeCap || *line.FilesRemoved != 3 || line.BytesFreed != 900 || *line.CacheBytesAfter != 607 ||
		line.Detail != "size cap 1000 B, margin 750 B: removed 3 entries, freed 900 B" || line.LowSpace {
		t.Fatalf("line %+v", line)
	}
	if text := goCacheTrimJournalText(line); !strings.Contains(text, " rule=size-cap; size cap 1000 B, margin 750 B: removed 3 entries") {
		t.Fatalf("--journal text %q", text)
	}
	state, _ := loadSweepState()
	if got := goCacheTrimNoticeSentence(state.GoCacheTrim); !strings.HasSuffix(got, " Verification cache trim: last run "+
		f.now.UTC().Format(time.RFC3339)+" removed 3 entries (size-cap; age bound 24h0m0s, size cap 1000 B) and freed 900 B; its build directories now hold 607 B.") {
		t.Fatalf("sentence %q", got)
	}

	// Only entries under 2h are left above the margin: it stops there.
	f.vsettings.MaxBytes = 400
	f.now = f.now.Add(2 * time.Hour)
	y1 := f.ventry("ba/"+strings.Repeat("6", 64)+"-a", 300, 90*time.Minute)
	y2 := f.ventry("bb/"+strings.Repeat("7", 64)+"-a", 300, time.Hour)
	y3 := f.ventry("bc/"+strings.Repeat("8", 64)+"-a", 300, 30*time.Minute)
	if !f.attempt() {
		t.Fatal("no attempt")
	}
	for _, path := range []string{e3, e4, e5} {
		requireExists(t, path, false)
	}
	for _, path := range []string{y1, y2, y3} {
		requireExists(t, path, true)
	}
	if line := lastVerificationLine(t); line.Reason != verificationRuleSizeCap || *line.FilesRemoved != 2 || *line.CacheBytesAfter != 907 ||
		line.Detail != "size cap 400 B, margin 300 B: removed 2 entries, freed 600 B; size cap 400 B, margin 300 B not reached: the rest is under 2h" {
		t.Fatalf("not reached: %+v", line)
	}

	// A build refreshes y2 while the cap removes y1: y2 is kept and y3 goes,
	// which reaches the 375 B margin.
	f.vsettings.MaxBytes = 500
	f.now = f.now.Add(2 * time.Hour)
	setTrimHooks(t, nil, func(dir, name string) {
		if name == filepath.Base(y1) {
			if err := os.Chtimes(y2, f.now, f.now); err != nil {
				t.Fatal(err)
			}
		}
	})
	if !f.attempt() {
		t.Fatal("no attempt")
	}
	setTrimHooks(t, nil, nil)
	requireExists(t, y1, false)
	requireExists(t, y2, true)
	requireExists(t, y3, false)
	if line := lastVerificationLine(t); line.Reason != verificationRuleSizeCap || *line.FilesRemoved != 2 || *line.CacheBytesAfter != 307 || strings.Contains(line.Detail, "not reached") {
		t.Fatalf("refreshed: %+v", line)
	}

	// With a holder in the lock: an entry past the age bound goes by age,
	// then the cap removes the oldest of the rest.
	writeMatrixLock(t, sweepHost, []map[string]any{sweepMatrixEntry("h1", filepath.Join(f.root, "wt"), true)}, nil)
	f.vsettings.MaxBytes = 500
	f.now = f.now.Add(2 * time.Hour)
	aged := f.ventry("ca/"+strings.Repeat("9", 64)+"-a", 100, 30*time.Hour)
	c1 := f.ventry("cb/"+strings.Repeat("a", 64)+"-a", 300, 5*time.Hour)
	c2 := f.ventry("cc/"+strings.Repeat("b", 64)+"-a", 300, 4*time.Hour)
	if !f.attempt() {
		t.Fatal("no attempt")
	}
	requireExists(t, aged, false)
	requireExists(t, c1, false)
	requireExists(t, c2, false)
	requireExists(t, y2, true)
	line = lastVerificationLine(t)
	if line.Reason != verificationRuleLockAge+"+"+verificationRuleSizeCap || *line.FilesRemoved != 3 || *line.CacheBytesAfter != 307 ||
		line.Detail != "size cap 500 B, margin 375 B: removed 2 entries, freed 600 B; trimmed while 1 matrix run(s) hold or wait for the host lock" {
		t.Fatalf("held: %+v", line)
	}
	if text := goCacheTrimJournalText(line); !strings.Contains(text, " rule=lock-time-age+size-cap; ") {
		t.Fatalf("--journal text %q", text)
	}
	state, _ = loadSweepState()
	if got := goCacheTrimNoticeSentence(state.GoCacheTrim); !strings.Contains(got, " removed 3 entries (lock-time-age+size-cap; age bound 24h0m0s, size cap 500 B) and freed 700 B;") {
		t.Fatalf("sentence %q", got)
	}
}

// a3, a5 (t2, t3, t4): with free space under the sweep's threshold the cap
// removes oldest first to its margin under the low-space rule, never an entry
// under 2h; with free space above it the same cap is size-cap.
func TestGoCacheTrimVerificationLowSpace(t *testing.T) {
	f := newVerificationFixture(t)
	f.free = 1 << 30
	f.vsettings.MaxBytes = 200
	e1 := f.ventry("aa/"+strings.Repeat("1", 64)+"-a", 300, 20*time.Hour)
	e2 := f.ventry("ab/"+strings.Repeat("2", 64)+"-a", 300, 10*time.Hour)
	e3 := f.ventry("ac/"+strings.Repeat("3", 64)+"-a", 300, 3*time.Hour)
	young := f.ventry("ad/"+strings.Repeat("4", 64)+"-a", 300, time.Hour)
	if !f.attempt() {
		t.Fatal("no attempt")
	}
	requireExists(t, e1, false)
	requireExists(t, e2, false)
	requireExists(t, e3, false)
	requireExists(t, young, true)
	line := lastVerificationLine(t)
	if line.Reason != verificationRuleLowSpace || *line.FilesRemoved != 3 || *line.CacheBytesAfter != 300 || !line.LowSpace ||
		line.Detail != "size cap 200 B, margin 150 B: removed 3 entries, freed 900 B; size cap 200 B, margin 150 B not reached: the rest is under 2h" {
		t.Fatalf("low space: %+v", line)
	}
	if text := goCacheTrimJournalText(line); !strings.Contains(text, " rule=low-space; ") || !strings.HasSuffix(text, "; low space") {
		t.Fatalf("--journal text %q", text)
	}
	state, _ := loadSweepState()
	if got := goCacheTrimNoticeSentence(state.GoCacheTrim); !strings.Contains(got, " removed 3 entries (low-space; age bound 24h0m0s, size cap 200 B) and freed 900 B;") {
		t.Fatalf("sentence %q", got)
	}
	// Free space above the threshold: the same cap is size-cap.
	f.free = 100 << 30
	f.now = f.now.Add(2 * time.Hour)
	z := f.ventry("ae/"+strings.Repeat("5", 64)+"-a", 300, 10*time.Hour)
	if !f.attempt() {
		t.Fatal("no attempt")
	}
	requireExists(t, z, false)
	requireExists(t, young, false)
	if line := lastVerificationLine(t); line.Reason != verificationRuleSizeCap || *line.FilesRemoved != 2 || *line.CacheBytesAfter != 0 || line.LowSpace {
		t.Fatalf("enough space: %+v", line)
	}
}

// The cap's margin (wi_66dcbf8fbd2fc501).

// c1, c4: build directories under the cap but over its margin are trimmed to
// the margin, oldest first, and an entry under 2h stays even while the margin
// is not reached; at the margin the cap removes nothing. The line records the
// counted size before and after.
func TestGoCacheTrimVerificationMargin(t *testing.T) {
	if got := int64(defaultVerificationTrimMaxBytes) * verificationTrimMarginPercent / 100; got != 15<<30 {
		t.Fatalf("the default margin is %s, want 15 GiB", humanBytes(got))
	}
	f := newVerificationFixture(t)
	f.vsettings.MaxBytes = 1000
	old := f.ventry("aa/"+strings.Repeat("1", 64)+"-a", 300, 10*time.Hour)
	older := f.ventry("ab/"+strings.Repeat("2", 64)+"-a", 300, 12*time.Hour)
	young := f.ventry("ac/"+strings.Repeat("3", 64)+"-a", 300, 90*time.Minute)
	if !f.attempt() {
		t.Fatal("no attempt")
	}
	// 900 B is under the 1000 B cap and over its 750 B margin: the oldest goes.
	requireExists(t, older, false)
	requireExists(t, old, true)
	requireExists(t, young, true)
	line := lastVerificationLine(t)
	if line.Reason != verificationRuleSizeCap || *line.FilesRemoved != 1 || *line.CacheBytesBefore != 900 || *line.CacheBytesAfter != 600 ||
		line.Detail != "size cap 1000 B, margin 750 B: removed 1 entries, freed 300 B" {
		t.Fatalf("margin: %+v", line)
	}
	if text := goCacheTrimJournalText(line); !strings.Contains(text, " trimmed files=1 freed=300 B cache-before=900 B cache=600 B ") {
		t.Fatalf("--journal text %q", text)
	}

	// Exactly at the margin: nothing goes.
	f.now = f.now.Add(time.Hour)
	at := f.ventry("ad/"+strings.Repeat("4", 64)+"-a", 150, 5*time.Hour)
	if !f.attempt() {
		t.Fatal("no attempt at the margin")
	}
	for _, path := range []string{old, young, at} {
		requireExists(t, path, true)
	}
	if line := lastVerificationLine(t); line.Reason != "" || *line.FilesRemoved != 0 || *line.CacheBytesBefore != 750 || *line.CacheBytesAfter != 750 || line.Detail != "" {
		t.Fatalf("at the margin: %+v", line)
	}

	// Over the margin with only young entries left to take: they stay.
	f.now = f.now.Add(time.Hour)
	f.vsettings.MaxBytes = 400
	fresh := f.ventry("ae/"+strings.Repeat("5", 64)+"-a", 300, time.Minute)
	if !f.attempt() {
		t.Fatal("no attempt over the margin")
	}
	requireExists(t, fresh, true)
	requireExists(t, young, false)
	if line := lastVerificationLine(t); *line.CacheBytesBefore != 1050 || *line.CacheBytesAfter != 300 ||
		line.Detail != "size cap 400 B, margin 300 B: removed 3 entries, freed 750 B" {
		t.Fatalf("young kept: %+v", line)
	}
	f.now = f.now.Add(time.Hour)
	fresh2 := f.ventry("af/"+strings.Repeat("6", 64)+"-a", 300, time.Minute)
	if !f.attempt() {
		t.Fatal("no attempt with young entries only")
	}
	requireExists(t, fresh, true)
	requireExists(t, fresh2, true)
	if line := lastVerificationLine(t); line.Reason != "" || *line.FilesRemoved != 0 || *line.CacheBytesAfter != 600 ||
		line.Detail != "size cap 400 B, margin 300 B not reached: the rest is under 2h" {
		t.Fatalf("young only: %+v", line)
	}
}

// c2, c4: between scheduled passes, a cache volume under the sweep's
// low-space threshold brings an extra pass, at most one per
// verificationLowSpaceTrimEvery; none while space is fine or the trim lock is
// held, and an extra pass keeps the matrix lock's rules.
func TestGoCacheTrimVerificationLowSpaceExtraPass(t *testing.T) {
	f := newVerificationFixture(t)
	var read []string
	f.deps.freeBytes = func(path string) (int64, error) {
		read = append(read, path)
		return f.free, nil
	}
	start := f.now
	first := f.ventry("aa/"+strings.Repeat("1", 64)+"-a", 100, 48*time.Hour)
	if !f.attempt() || len(verificationLines(t)) != 1 {
		t.Fatal("no scheduled pass")
	}
	requireExists(t, first, false)

	// Space is fine 20 minutes later: no extra pass.
	second := f.ventry("ab/"+strings.Repeat("2", 64)+"-a", 100, 48*time.Hour)
	f.now = start.Add(20 * time.Minute)
	read = nil
	if f.attempt() || len(verificationLines(t)) != 1 {
		t.Fatal("an extra pass ran with enough space")
	}
	if len(read) != 1 || read[0] != f.vroot {
		t.Fatalf("free space read at %v, want the root %s", read, f.vroot)
	}
	requireExists(t, second, true)

	// Under the threshold, but another trim holds the lock: nothing happens.
	f.free = 1 << 30
	holder, err := os.OpenFile(filepath.Join(f.relayState(), "verification-cache-trim.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if err := syscall.Flock(int(holder.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	if f.attempt() || len(verificationLines(t)) != 1 {
		t.Fatal("an extra pass ran while the trim lock was held")
	}
	requireExists(t, second, true)
	if err := syscall.Flock(int(holder.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}

	// Released: the extra pass runs and says why.
	if !f.attempt() || len(verificationLines(t)) != 2 {
		t.Fatal("no extra pass on low space")
	}
	requireExists(t, second, false)
	line := lastVerificationLine(t)
	if line.Outcome != "trimmed" || line.Reason != verificationRuleAge || !line.LowSpace || *line.FilesRemoved != 1 ||
		line.Detail != "extra pass: 1.0 GiB free, under the low-space threshold 8.0 GiB" {
		t.Fatalf("extra pass: %+v", line)
	}
	state, _ := loadSweepState()
	if v := state.GoCacheTrim.Verification; !v.LastRunAt.Equal(f.now) || !v.LastAttemptAt.Equal(f.now) {
		t.Fatalf("an extra pass is a run: %+v", v)
	}

	// Still low ten minutes later: within the interval, no pass.
	third := f.ventry("ac/"+strings.Repeat("3", 64)+"-a", 100, 48*time.Hour)
	f.now = start.Add(30 * time.Minute)
	if f.attempt() || len(verificationLines(t)) != 2 {
		t.Fatal("an extra pass ran within its interval")
	}
	requireExists(t, third, true)

	// An unreadable matrix lock skips an extra pass as it does a scheduled
	// one, and the skip spaces the next.
	f.now = start.Add(35 * time.Minute)
	writeFixtureFile(t, f.lock, "not json")
	if !f.attempt() || len(verificationLines(t)) != 3 {
		t.Fatal("no extra attempt with an unreadable lock")
	}
	requireExists(t, third, true)
	if line := lastVerificationLine(t); line.Outcome != "skipped" || line.Reason != sweepSkipMatrix ||
		!strings.HasPrefix(line.Detail, "extra pass: 1.0 GiB free, under the low-space threshold 8.0 GiB; the matrix host lock cannot be read") || line.CacheBytesBefore != nil {
		t.Fatalf("skipped extra pass: %+v", line)
	}
	writeMatrixLock(t, sweepHost, nil, nil)
	f.now = start.Add(45 * time.Minute)
	if f.attempt() || len(verificationLines(t)) != 3 {
		t.Fatal("an extra pass ran within its interval after a skip")
	}
	f.now = start.Add(50 * time.Minute)
	if !f.attempt() || len(verificationLines(t)) != 4 {
		t.Fatal("no extra pass after the interval")
	}
	requireExists(t, third, false)

	// Space recovers: the next pass is the scheduled one, an interval after
	// the last run.
	f.free = 100 << 30
	f.ventry("ad/"+strings.Repeat("4", 64)+"-a", 100, 48*time.Hour)
	f.now = start.Add(50*time.Minute + defaultSweepInterval - time.Minute)
	if f.attempt() {
		t.Fatal("a pass ran before the schedule with enough space")
	}
	f.now = f.now.Add(time.Minute)
	if !f.attempt() || lastVerificationLine(t).Detail != "" {
		t.Fatalf("scheduled pass: %+v", lastVerificationLine(t))
	}
}

// c3: a trim line written before the before field still parses and renders
// as it did, and a sweep line never carries it.
func TestGoCacheTrimVerificationBeforeFieldOldLines(t *testing.T) {
	var old sweepJournalLine
	raw := `{"at":"2026-10-21T18:47:00Z","source":"go-cache-trim","outcome":"trimmed","reason":"lock-time-age","bytesFreed":300,"freeBytesAfter":1024,` +
		`"lowestVolume":"/v","kept":{},"filesRemoved":1,"cacheBytesAfter":600,"maxAge":"24h0m0s"}`
	if err := json.Unmarshal([]byte(raw), &old); err != nil || old.CacheBytesBefore != nil {
		t.Fatalf("old line %+v: %v", old, err)
	}
	if text := goCacheTrimJournalText(old); text != "2026-10-21T18:47:00Z verification-cache-trim root=/v trimmed files=1 freed=300 B cache=600 B free=1.0 KiB max-age=24h0m0s rule=lock-time-age" {
		t.Fatalf("old line text %q", text)
	}
	encoded, err := json.Marshal(sweepJournalLine{Source: "schedule"})
	if err != nil || strings.Contains(string(encoded), "cacheBytesBefore") {
		t.Fatalf("sweep line %s: %v", encoded, err)
	}
}
