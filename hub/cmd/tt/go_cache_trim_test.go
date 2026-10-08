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
		if strings.Contains(line, `"source":"`+goCacheTrimSource+`"`) {
			out = append(out, line)
		}
	}
	return out
}

func trimLines(t *testing.T) []sweepJournalLine {
	t.Helper()
	var out []sweepJournalLine
	for _, l := range sweepJournalLines(t) {
		if l.Source == goCacheTrimSource {
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
	for _, key := range []string{"at", "source", "outcome", "filesRemoved", "bytesFreed", "cacheBytesAfter", "freeBytesAfter", "lowSpace", "durationMs", "maxAge"} {
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
	if line.Outcome != "trimmed" || *line.FilesRemoved != 2 || line.BytesFreed != 350 || *line.CacheBytesAfter != wantAfter || line.FreeBytesAfter != 42<<30 || line.MaxAge != "24h0m0s" {
		t.Fatalf("sized line %+v, want cache %d", line, wantAfter)
	}
	state, err := loadSweepState()
	if err != nil || state.GoCacheTrim == nil || state.GoCacheTrim.FilesRemoved != 2 || state.GoCacheTrim.BytesFreed != 350 || state.GoCacheTrim.CacheBytesAfter != wantAfter || !state.GoCacheTrim.LastRunAt.Equal(f.now) {
		t.Fatalf("state %+v: %v", state.GoCacheTrim, err)
	}
	// A skipped line also carries both counts, as zeros.
	f.resolveErr = errors.New("no go")
	f.now = f.now.Add(time.Hour)
	if !f.attempt() {
		t.Fatal("no skipped attempt")
	}
	raw = trimJournalRaw(t)
	if len(raw) != 3 || !strings.Contains(raw[2], `"filesRemoved":0`) || !strings.Contains(raw[2], `"cacheBytesAfter":0`) {
		t.Fatalf("skipped line: %v", raw)
	}
	// The tt read path, as text and as JSON.
	out, err := captureSweepStdout(t, func() error { return cmdTeamQueueSweepWorktrees(env{}, []string{"--journal"}) })
	want := fmt.Sprintf("%s go-cache-trim trimmed files=2 freed=350 B cache=%d B free=42.0 GiB max-age=24h0m0s", line.At, wantAfter)
	if err != nil || !strings.Contains(out, want) || !strings.Contains(out, " go-cache-trim skipped no-go: no go") || !strings.Contains(out, " go-cache-trim trimmed files=0 freed=0 B ") {
		t.Fatalf("--journal: %v\nwant %q in\n%s", err, want, out)
	}
	out, err = captureSweepStdout(t, func() error { return cmdTeamQueueSweepWorktrees(env{}, []string{"--journal", "--json"}) })
	var printed []sweepJournalLine
	if err != nil || json.Unmarshal([]byte(out), &printed) != nil || len(printed) != 3 {
		t.Fatalf("--journal --json: %v\n%s", err, out)
	}
	if got := printed[1]; got.Source != goCacheTrimSource || *got.FilesRemoved != 2 || got.BytesFreed != 350 || *got.CacheBytesAfter != wantAfter || got.FreeBytesAfter != 42<<30 || got.At != line.At {
		t.Fatalf("--journal --json line %+v", got)
	}
}

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
	if text := attempts[0].Envelope.Body.Text; !strings.HasSuffix(text, "tt team queue sweep-worktrees --journal. The Go build cache trim is off or has not run on this host.") || len(text) > api.MaxEnvelopeBodyBytes {
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
	sentence := fmt.Sprintf("Go build cache trim: last run %s removed 0 entries older than 24h0m0s and freed 0 B; the cache now holds %s.",
		f.now.UTC().Format(time.RFC3339), humanBytes(state.GoCacheTrim.CacheBytesAfter))
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
		"Go build cache trim: last run "+trimAt.UTC().Format(time.RFC3339)+" removed 1 entries older than 24h0m0s and freed 100 B; the cache now holds 2.0 KiB." {
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
	writeRelaySettings(t, f.home, `{"goBuildCacheTrim":"on"}`)
	nativeRelaySweepSchedule(context.Background())
	requireExists(t, old, false)
	requireExists(t, fresh, true)
	if line := lastTrimLine(t); line.Outcome != "trimmed" || *line.FilesRemoved != 1 || line.FreeBytesAfter <= 0 {
		t.Fatalf("line %+v", line)
	}
}
