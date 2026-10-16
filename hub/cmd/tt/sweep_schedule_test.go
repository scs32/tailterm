package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

// No test in this package runs the relay's real scheduled sweep: a test that
// starts the relay loop gets this no-op, and the schedule tests call
// sweepScheduleAttempt with their own fixture.
func init() { relaySweepSchedule = func(context.Context) {} }

// No test in this package probes a real process for the matrix mutex owner:
// the probe starts as strayMatrixOwnerProbe, newSweepFixture replaces it with
// a fake table, and only nativeMatrixOwnerProbeFor installs the real one.
func init() { matrixOwnerProbe = strayMatrixOwnerProbe }

// strayMatrixOwnerProbe fails the test binary at the first owner probe that
// reaches it, before any signal or ps runs, naming the pid and the fix.
func strayMatrixOwnerProbe(pid int) (bool, time.Time, bool) {
	panic(fmt.Sprintf("a test probed matrix mutex owner pid %d without a process table: use newSweepFixture or fakeMatrixOwners, or opt in with nativeMatrixOwnerProbeFor(t, reason)", pid))
}

// nativeMatrixOwnerProbeFor is the one way a test may signal and inspect a
// real process to learn whether a mutex owner is alive. The reason says why a
// fake table cannot serve and is logged with the test; it may not be empty.
// Only TestSweepMatrixOwnerProbeGuard uses it, and calls no probe.
func nativeMatrixOwnerProbeFor(t *testing.T, reason string) {
	t.Helper()
	if strings.TrimSpace(reason) == "" {
		t.Fatal("the native matrix owner probe needs a stated reason")
	}
	t.Logf("this test signals and inspects real processes: %s", reason)
	old := matrixOwnerProbe
	matrixOwnerProbe = nativeMatrixOwnerProbe
	t.Cleanup(func() { matrixOwnerProbe = old })
}

const (
	sweepHost = "fixture-host"
	sweepTask = "tsk_5eeb5eeb5eeb5e00"
	sweepItem = "wi_5eeb5eeb5eeb5e01"
)

// sweepHub is a fixture hub that also stores posted messages, with the
// failures the notice has to survive.
type sweepHub struct {
	cleanupHub
	posts sync.Mutex
	// postMode: "" stores and answers; "lose" stores and answers 500; "fail"
	// answers 500 and "conflict" 409 without storing.
	postMode string
	// lookup lets the receipt lookup find stored messages; without it the
	// lookup answers 404.
	lookup   bool
	attempts []api.PostMessageRequest
	bodies   []string
	postedTo []string
	stored   []api.Message
}

func (h *sweepHub) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if !strings.Contains(req.URL.Path, "/messages") {
		h.cleanupHub.ServeHTTP(w, req)
		return
	}
	h.posts.Lock()
	defer h.posts.Unlock()
	task := strings.SplitN(strings.TrimPrefix(req.URL.Path, "/v1/tasks/"), "/", 2)[0]
	find := func(requestID string) *api.Message {
		for i := range h.stored {
			if h.stored[i].TaskID == task && h.stored[i].PostReceipt.RequestID == requestID {
				return &h.stored[i]
			}
		}
		return nil
	}
	w.Header().Set("Content-Type", "application/json")
	if req.Method == http.MethodGet {
		found := find(filepath.Base(req.URL.Path))
		if !h.lookup || found == nil {
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(found)
		return
	}
	body, _ := io.ReadAll(req.Body)
	var post api.PostMessageRequest
	if json.Unmarshal(body, &post) != nil || req.Header.Get(relayAuthorHeader) == "" {
		http.Error(w, `{"error":"bad post"}`, http.StatusBadRequest)
		return
	}
	h.attempts, h.bodies, h.postedTo = append(h.attempts, post), append(h.bodies, string(body)), append(h.postedTo, task)
	switch h.postMode {
	case "fail":
		http.Error(w, `{"error":"down"}`, http.StatusInternalServerError)
		return
	case "conflict":
		http.Error(w, `{"error":"request ID was already used with different message data"}`, http.StatusConflict)
		return
	}
	found := find(post.RequestID)
	if found == nil {
		seq := int64(len(h.stored) + 100)
		h.stored = append(h.stored, api.Message{Seq: seq, TaskID: task, Text: post.Text, To: post.To,
			PostReceipt: &api.MessagePostReceipt{ID: fmt.Sprintf("mpr_%d", seq), RequestID: post.RequestID, TaskID: task, MessageSeq: seq}})
		found = &h.stored[len(h.stored)-1]
	}
	if h.postMode == "lose" {
		http.Error(w, `{"error":"response lost"}`, http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(found)
}

func (h *sweepHub) messages() ([]api.Message, []api.PostMessageRequest, []string, []string) {
	h.posts.Lock()
	defer h.posts.Unlock()
	return append([]api.Message(nil), h.stored...), append([]api.PostMessageRequest(nil), h.attempts...), append([]string(nil), h.bodies...), append([]string(nil), h.postedTo...)
}

func (h *sweepHub) setPosts(mode string, lookup bool) {
	h.posts.Lock()
	h.postMode, h.lookup = mode, lookup
	h.posts.Unlock()
}

// sweepFixture is a fake repository, hub, home, clock and free-space reading.
type sweepFixture struct {
	r      cleanupRepo
	hub    *sweepHub
	home   string
	host   string
	hubURL string
	now    time.Time
	free   int64
	sweeps int
	last   []worktreeDecision
	deps   sweepScheduleDeps
}

// sentinelCaches are stand-ins for the shared user caches under the fake
// home; isolatedHome points every never-touch name at them.
func isolatedHome(t *testing.T, root string) string {
	t.Helper()
	home := filepath.Join(root, "home")
	for _, file := range []string{"Library/Caches/go-build/aa/entry", "go/pkg/mod/cache/download/list", ".npm/_cacache/index"} {
		writeFixtureFile(t, filepath.Join(home, file), "shared cache: "+file+"\n")
	}
	t.Setenv("HOME", home)
	t.Setenv("GOCACHE", filepath.Join(home, "Library", "Caches", "go-build"))
	t.Setenv("GOMODCACHE", filepath.Join(home, "go", "pkg", "mod"))
	t.Setenv("GOPATH", filepath.Join(home, "go"))
	t.Setenv("npm_config_cache", filepath.Join(home, ".npm"))
	return home
}

// resetStateFaultGates starts a test with no state fault open in the relay's
// memory, and leaves none behind.
func resetStateFaultGates(t *testing.T) {
	t.Helper()
	reset := func() {
		for _, gate := range []*stateFaultGate{&sweepStateFaults, &goCacheTrimStateFaults} {
			gate.mu.Lock()
			gate.kind, gate.lastTry = "", time.Time{}
			gate.mu.Unlock()
		}
	}
	reset()
	t.Cleanup(reset)
}

func newSweepFixture(t *testing.T) *sweepFixture {
	t.Helper()
	resetStateFaultGates(t)
	r := newCleanupRepo(t)
	r.ignoreBuildOutput(t)
	f := &sweepFixture{r: r, home: isolatedHome(t, r.root), host: sweepHost, now: time.Now().Add(48 * time.Hour), free: 100 << 30}
	oldHost := matrixWaitHostname
	matrixWaitHostname = func() (string, error) { return sweepHost, nil }
	t.Cleanup(func() { matrixWaitHostname = oldHost })
	// An empty process table: every mutex owner, including a node child the
	// test started, is alive with an unknown start and is never reclaimed. A
	// test that needs a dead or restarted owner passes its own table.
	fakeMatrixOwners(t, nil)
	entry := releasedArtifactEntry(sweepItem)
	entry.ID, entry.TaskID, entry.Host, entry.Repository = "tqe_sweep_repo", sweepTask, sweepHost, r.main
	f.hub = &sweepHub{cleanupHub: cleanupHub{
		tasks:   []api.Task{{ID: sweepTask, Status: api.TaskOpen}},
		details: map[string]api.TaskDetail{sweepTask: {Task: api.Task{ID: sweepTask, Status: api.TaskOpen}}},
		queues:  map[string]api.TeamQueueList{sweepTask: {Entries: []api.TeamQueueEntry{entry}}},
	}}
	server := httptest.NewServer(f.hub)
	t.Cleanup(server.Close)
	f.hubURL = server.URL
	settings := defaultSweepSettings()
	f.deps = sweepScheduleDeps{
		now:       func() time.Time { return f.now },
		freeBytes: func(string) (int64, error) { return f.free, nil },
		settings:  func() (sweepSettings, string) { return settings, "" },
		hub: func() (*api.Client, string, bool) {
			c, err := api.NewClient(server.URL, 5*time.Second)
			if err != nil {
				t.Error(err)
				return nil, "", false
			}
			c.HTTP.Transport = relayAuthorTransport{base: c.HTTP.Transport}
			return c, f.host, true
		},
		sweep: func(ctx context.Context, state sweepHubState, host string, o sweepOptions) ([]worktreeDecision, error) {
			f.sweeps++
			decisions, err := runWorktreeSweep(ctx, state, host, o)
			f.last = decisions
			return decisions, err
		},
	}
	return f
}

func (f *sweepFixture) attempt() bool { return sweepScheduleAttempt(context.Background(), f.deps) }

func (f *sweepFixture) setEntries(entries ...api.TeamQueueEntry) {
	f.hub.mu.Lock()
	f.hub.queues[sweepTask] = api.TeamQueueList{Entries: entries}
	f.hub.mu.Unlock()
}

func (f *sweepFixture) entries() []api.TeamQueueEntry {
	f.hub.mu.Lock()
	defer f.hub.mu.Unlock()
	return append([]api.TeamQueueEntry(nil), f.hub.queues[sweepTask].Entries...)
}

// stale adds a clean detached worktree at tasks-hub: removable once idle.
func (f *sweepFixture) stale(t *testing.T, name string) string {
	t.Helper()
	path := f.r.queuePath(name)
	cleanupGit(t, f.r.main, "worktree", "add", "-q", "--detach", path, "tasks-hub")
	return path
}

func sweepJournalLines(t *testing.T) []sweepJournalLine {
	t.Helper()
	lines, err := readSweepJournal(1000)
	if err != nil {
		t.Fatal(err)
	}
	return lines
}

func lastSweepLine(t *testing.T) sweepJournalLine {
	t.Helper()
	lines := sweepJournalLines(t)
	if len(lines) == 0 {
		t.Fatal("the journal is empty")
	}
	return lines[len(lines)-1]
}

// writeMatrixLock writes a version 2 lock file as the script does.
func writeMatrixLock(t *testing.T, host string, holders, waiters []map[string]any) string {
	t.Helper()
	path := os.Getenv("TAILTERM_MATRIX_HOST_LOCK")
	if holders == nil {
		holders = []map[string]any{}
	}
	if waiters == nil {
		waiters = []map[string]any{}
	}
	state := map[string]any{"version": 2, "host": host, "updatedAt": "2026-10-07T10:00:00.000Z", "requestSeq": 5, "grantSeq": 3, "holderLimit": 2, "holders": holders, "waiters": waiters}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, path, string(data)+"\n")
	return path
}

func sweepMatrixEntry(id, worktree string, holder bool) map[string]any {
	e := map[string]any{"id": id, "pid": os.Getpid(), "seq": 1, "requestedAt": "2026-10-07T10:00:00.000Z", "kind": "run", "item": sweepItem, "agent": "verifier", "priority": "normal", "runTimeoutMs": 600000, "worktree": worktree}
	if holder {
		e["startedAt"] = "2026-10-07T10:00:01.000Z"
	} else {
		e["grantSeqAtRequest"], e["overtakenBy"] = 0, 0
	}
	return e
}

// t1: the schedule runs when due and not before, retries five minutes after a
// skip, and takes its interval from the settings.
func TestSweepScheduleSpacing(t *testing.T) {
	f := newSweepFixture(t)
	gone := f.stale(t, "queue-5eeb0001")
	if !f.attempt() || f.sweeps != 1 {
		t.Fatalf("no state: attempted with %d sweeps", f.sweeps)
	}
	requireExists(t, gone, false)
	first := lastSweepLine(t)
	if first.Outcome != "swept" || first.TreesRemoved != 1 || first.MinIdle != "6h0m0s" || first.Repositories != 1 {
		t.Fatalf("first run %+v", first)
	}
	start := f.now
	f.now = start.Add(59 * time.Minute)
	if f.attempt() || f.sweeps != 1 || len(sweepJournalLines(t)) != 1 {
		t.Fatalf("ran before the hour: %d sweeps, %d lines", f.sweeps, len(sweepJournalLines(t)))
	}
	f.now = start.Add(time.Hour)
	if !f.attempt() || f.sweeps != 2 {
		t.Fatalf("not run at the hour: %d sweeps", f.sweeps)
	}
	// A skipped attempt is tried again five minutes later, not sooner.
	lock, err := worktreeCleanupLock()
	if err != nil {
		t.Fatal(err)
	}
	f.now = start.Add(2 * time.Hour)
	if !f.attempt() || f.sweeps != 2 || lastSweepLine(t).Reason != sweepSkipCleanup {
		t.Fatalf("held cleanup lock: %d sweeps, %+v", f.sweeps, lastSweepLine(t))
	}
	unlockQueueLaunch(lock)
	f.now = start.Add(2*time.Hour + 4*time.Minute)
	if f.attempt() {
		t.Fatal("retried four minutes after a skip")
	}
	f.now = start.Add(2*time.Hour + 5*time.Minute)
	if !f.attempt() || f.sweeps != 3 {
		t.Fatalf("not retried five minutes after a skip: %d sweeps", f.sweeps)
	}
	// The interval comes from the settings at each check.
	longer := defaultSweepSettings()
	longer.Interval = 3 * time.Hour
	f.deps.settings = func() (sweepSettings, string) { return longer, "" }
	ran := f.now
	f.now = ran.Add(2 * time.Hour)
	if f.attempt() {
		t.Fatal("ran inside a three-hour interval")
	}
	f.now = ran.Add(3 * time.Hour)
	if !f.attempt() || f.sweeps != 4 {
		t.Fatalf("not run after three hours: %d sweeps", f.sweeps)
	}
	if !sweepDue(sweepState{}, time.Hour, f.now) || sweepDue(sweepState{LastRunAt: f.now}, time.Hour, f.now.Add(time.Minute)) {
		t.Fatal("due rule")
	}
}

func writeRelaySettings(t *testing.T, home, content string) {
	t.Helper()
	writeFixtureFile(t, filepath.Join(home, ".config", "tailterm", "relay.json"), content)
}

// t2: the owner's switch stops the sweep, is journalled once, and takes
// effect in both directions with no restart.
func TestSweepScheduleOffSwitch(t *testing.T) {
	f := newSweepFixture(t)
	f.deps.settings = readSweepSettings
	kept := f.stale(t, "queue-5eeb0002")
	writeRelaySettings(t, f.home, `{"worktreeSweep":"off"}`)
	if !f.attempt() || f.sweeps != 0 {
		t.Fatalf("off: attempted with %d sweeps", f.sweeps)
	}
	if line := lastSweepLine(t); line.Outcome != "skipped" || line.Reason != sweepSkipOff {
		t.Fatalf("off line %+v", line)
	}
	for i := 0; i < 3; i++ {
		f.now = f.now.Add(2 * time.Hour)
		if f.attempt() {
			t.Fatal("an off switch was journalled again")
		}
	}
	requireExists(t, kept, true)
	if len(sweepJournalLines(t)) != 1 || f.sweeps != 0 {
		t.Fatalf("off: %d lines, %d sweeps", len(sweepJournalLines(t)), f.sweeps)
	}
	writeRelaySettings(t, f.home, `{"worktreeSweep":"on"}`)
	if !f.attempt() || f.sweeps != 1 {
		t.Fatalf("on again: %d sweeps", f.sweeps)
	}
	requireExists(t, kept, false)
	// Off again is journalled once more.
	writeRelaySettings(t, f.home, `{"worktreeSweep":"off"}`)
	f.now = f.now.Add(2 * time.Hour)
	if !f.attempt() || f.attempt() || lastSweepLine(t).Reason != sweepSkipOff || f.sweeps != 1 {
		t.Fatalf("off again: %+v, %d sweeps", lastSweepLine(t), f.sweeps)
	}
	// k4: on while no run is due writes no line, and the next off is
	// journalled as its own transition.
	offLines := func() int {
		n := 0
		for _, line := range sweepJournalLines(t) {
			if line.Outcome == "skipped" && line.Reason == sweepSkipOff {
				n++
			}
		}
		return n
	}
	writeRelaySettings(t, f.home, `{"worktreeSweep":"on"}`)
	f.now = f.now.Add(time.Minute)
	if !f.attempt() || f.sweeps != 2 {
		t.Fatalf("on with a run due: %d sweeps", f.sweeps)
	}
	before, lines := offLines(), len(sweepJournalLines(t))
	writeRelaySettings(t, f.home, `{"worktreeSweep":"off"}`)
	f.now = f.now.Add(time.Minute)
	if !f.attempt() || lastSweepLine(t).Reason != sweepSkipOff {
		t.Fatalf("off: %+v", lastSweepLine(t))
	}
	writeRelaySettings(t, f.home, `{"worktreeSweep":"on"}`)
	f.now = f.now.Add(time.Minute)
	if f.attempt() || f.sweeps != 2 || len(sweepJournalLines(t)) != lines+1 {
		t.Fatalf("on while not due: %d sweeps, %d lines", f.sweeps, len(sweepJournalLines(t)))
	}
	if state, err := loadSweepState(); err != nil || state.OffSince != nil {
		t.Fatalf("on did not clear the off mark: %+v %v", state.OffSince, err)
	}
	writeRelaySettings(t, f.home, `{"worktreeSweep":"off"}`)
	f.now = f.now.Add(time.Minute)
	if !f.attempt() || lastSweepLine(t).Reason != sweepSkipOff {
		t.Fatalf("off after on while not due: %+v", lastSweepLine(t))
	}
	for i := 0; i < 10; i++ {
		f.now = f.now.Add(30 * time.Second)
		if f.attempt() {
			t.Fatal("an off switch was journalled again")
		}
	}
	// Off, on while not due, off: exactly two off lines, and none for on.
	if got := offLines(); got != before+2 || len(sweepJournalLines(t)) != lines+2 || f.sweeps != 2 {
		t.Fatalf("%d off lines after %d, %d lines after %d, %d sweeps", got, before, len(sweepJournalLines(t)), lines, f.sweeps)
	}
}

// t3: settings fail safe. A file that cannot be trusted skips the run; a bad
// duration or threshold falls back to its default.
func TestSweepScheduleSettingsFailSafe(t *testing.T) {
	f := newSweepFixture(t)
	f.deps.settings = readSweepSettings
	kept := f.stale(t, "queue-5eeb0003")
	if settings, fault := readSweepSettings(); fault != "" || settings != defaultSweepSettings() {
		t.Fatalf("missing file: %+v %q", settings, fault)
	}
	for _, content := range []string{`{"worktreeSweep":`, `[]`, `null`, `{"worktreeSweep":"OFF"}`, `{"worktreeSweep":true}`, `{"worktreeSweep":"off","worktreeSweep":"maybe"}`} {
		writeRelaySettings(t, f.home, content)
		f.now = f.now.Add(time.Hour)
		if !f.attempt() || f.sweeps != 0 {
			t.Fatalf("%s: attempted with %d sweeps", content, f.sweeps)
		}
		if line := lastSweepLine(t); line.Outcome != "skipped" || line.Reason != sweepSkipSettings || line.Detail == "" {
			t.Fatalf("%s: line %+v", content, line)
		}
	}
	requireExists(t, kept, true)
	// Each bad value is reported once per process; start from none reported.
	sweepSettingsWarned.Clear()
	t.Cleanup(sweepSettingsWarned.Clear)
	var stderr strings.Builder
	restore := captureStderr(t, &stderr)
	writeRelaySettings(t, f.home, `{"worktreeSweepInterval":"soon","worktreeSweepMinIdle":"-1h","worktreeSweepLowSpaceGiB":0,"worktreesweep":"off"}`)
	settings, fault := readSweepSettings()
	_, _ = readSweepSettings()
	writeRelaySettings(t, f.home, `{"worktreeSweepInterval":"1m","worktreeSweepMinIdle":"30m","worktreeSweepLowSpaceGiB":2.5}`)
	custom, customFault := readSweepSettings()
	restore()
	if fault != "" || settings != defaultSweepSettings() {
		t.Fatalf("bad values: %+v %q", settings, fault)
	}
	if strings.Count(stderr.String(), "worktreeSweepInterval=\"soon\"") != 1 || !strings.Contains(stderr.String(), "worktreeSweepMinIdle") || !strings.Contains(stderr.String(), "worktreeSweepLowSpaceGiB") {
		t.Fatalf("bad values said %q", stderr.String())
	}
	if want := (sweepSettings{Interval: minSweepInterval, MinIdle: 30 * time.Minute, LowSpaceBytes: 5 << 29}); customFault != "" || custom != want {
		t.Fatalf("custom settings %+v %q, want %+v", custom, customFault, want)
	}
}

// t4: another cleanup skips the whole run; an absent lock file is a free
// host; a matrix entry's worktree, output and record folder are kept in use
// while another tree goes.
func TestSweepScheduleSkipsAndMatrixProtection(t *testing.T) {
	f := newSweepFixture(t)
	held := f.stale(t, "queue-5eeb0041")
	withOutput := f.stale(t, "queue-5eeb0042")
	withRecord := f.stale(t, "queue-5eeb0043")
	waited := f.stale(t, "queue-5eeb0044")
	free := f.stale(t, "queue-5eeb0045")
	lock, err := worktreeCleanupLock()
	if err != nil {
		t.Fatal(err)
	}
	if !f.attempt() || f.sweeps != 0 || lastSweepLine(t).Reason != sweepSkipCleanup {
		t.Fatalf("cleanup lock held: %+v", lastSweepLine(t))
	}
	unlockQueueLaunch(lock)
	holder := sweepMatrixEntry("holder-1", held, true)
	holder["output"], holder["recordDirectory"] = filepath.Join(withOutput, ".build", "matrix-out"), filepath.Join(withRecord, ".build", "record")
	writeMatrixLock(t, sweepHost, []map[string]any{holder}, []map[string]any{sweepMatrixEntry("waiter-1", waited, false)})
	f.now = f.now.Add(time.Hour)
	if !f.attempt() || f.sweeps != 1 {
		t.Fatalf("with a matrix run: %d sweeps", f.sweeps)
	}
	line := lastSweepLine(t)
	if line.Outcome != "swept" || line.TreesRemoved != 1 || line.Kept[keepInUse] != 4 {
		t.Fatalf("matrix protection line %+v", line)
	}
	requireExists(t, free, false)
	for _, p := range []string{held, withOutput, withRecord, waited} {
		requireExists(t, p, true)
	}
	for _, d := range f.last {
		if d.Path != free && (d.Action != "kept" || d.Reason != keepInUse) {
			t.Fatalf("decision %+v", d)
		}
	}
	// With the lock file gone the host is free and the rest goes.
	if err := os.Remove(os.Getenv("TAILTERM_MATRIX_HOST_LOCK")); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(time.Hour)
	if !f.attempt() || lastSweepLine(t).TreesRemoved != 4 {
		t.Fatalf("absent lock: %+v", lastSweepLine(t))
	}
	if _, err := os.Stat(os.Getenv("TAILTERM_MATRIX_HOST_LOCK")); !os.IsNotExist(err) {
		t.Fatalf("the sweep wrote the matrix lock file: %v", err)
	}
}

// With no override and a refused default lock, the sweep skips its run and
// its journal line names the refusal as matrixLockDefault states it, not the
// empty path's shape (wi_b52092ee3adcf6be). A relative override still reads
// as not absolute.
func TestSweepScheduleMatrixLockDefaultRefused(t *testing.T) {
	f := newSweepFixture(t)
	kept := f.stale(t, "queue-5eeb00f1")
	account := filepath.Join(f.r.root, "account")
	oldDefault, oldAccount := matrixWaitDefaultPath, matrixAccountHome
	t.Cleanup(func() { matrixWaitDefaultPath, matrixAccountHome = oldDefault, oldAccount })
	matrixWaitDefaultPath = nativeMatrixWaitDefaultPath
	matrixAccountHome = func() (string, error) { return account, nil }
	t.Setenv("TAILTERM_MATRIX_HOST_LOCK", "")
	skips := func(what, want string) {
		t.Helper()
		if m, why := loadMatrixExclusion(); m != nil || why != want {
			t.Fatalf("%s: sweep %+v %q, want %q", what, m, why, want)
		}
		f.now = f.now.Add(time.Hour)
		if !f.attempt() || f.sweeps != 0 {
			t.Fatalf("%s: attempted with %d sweeps", what, f.sweeps)
		}
		if line := lastSweepLine(t); line.Outcome != "skipped" || line.Reason != sweepSkipMatrix || line.Detail != want {
			t.Fatalf("%s: line %+v, want detail %q", what, line, want)
		}
	}

	// The fixture's HOME is not the account home.
	skips("isolated HOME", "HOME ("+f.home+") is not this account's home ("+account+"), so the default lock would be private; set TAILTERM_MATRIX_HOST_LOCK to an absolute lock path (the shared one is "+filepath.Join(account, ".local/state/tailterm-matrix/host.json")+")")
	matrixAccountHome = func() (string, error) { return "", errors.New("no such user") }
	skips("unreadable account home", "cannot read this account's home (no such user); set TAILTERM_MATRIX_HOST_LOCK to an absolute lock path")
	matrixAccountHome = func() (string, error) { return account, nil }
	t.Setenv("TAILTERM_MATRIX_HOST_LOCK", "host.json")
	skips("relative override", "the lock path is not absolute")
	requireExists(t, kept, true)
}

// t5: a lock file that is unreadable, not JSON, or valid JSON that fails the
// structural check skips the run and deletes nothing.
func TestSweepScheduleMatrixLockValidation(t *testing.T) {
	f := newSweepFixture(t)
	kept := f.stale(t, "queue-5eeb0005")
	path := os.Getenv("TAILTERM_MATRIX_HOST_LOCK")
	good := sweepMatrixEntry("holder-1", kept, true)
	with := func(change func(state map[string]any)) string {
		state := map[string]any{"version": 2, "host": sweepHost, "requestSeq": 1, "grantSeq": 1, "holderLimit": 2, "holders": []any{good}, "waiters": []any{}}
		change(state)
		data, _ := json.Marshal(state)
		return string(data)
	}
	entry := func(change func(e map[string]any)) string {
		e := sweepMatrixEntry("holder-1", kept, true)
		change(e)
		return with(func(state map[string]any) { state["holders"] = []any{e} })
	}
	cases := map[string]string{
		"not JSON":           `{"version": 2,`,
		"not an object":      `[]`,
		"no holders":         with(func(s map[string]any) { delete(s, "holders") }),
		"no waiters":         with(func(s map[string]any) { delete(s, "waiters") }),
		"null waiters":       with(func(s map[string]any) { s["waiters"] = nil }),
		"holderLimit 0":      with(func(s map[string]any) { s["holderLimit"] = 0 }),
		"fractional counter": strings.Replace(with(func(s map[string]any) {}), `"requestSeq":1`, `"requestSeq":1.5`, 1),
		"holder key in v2":   with(func(s map[string]any) { s["holder"] = nil }),
		"too many holders": with(func(s map[string]any) {
			s["holderLimit"] = 1
			s["holders"] = []any{good, sweepMatrixEntry("holder-2", kept, true)}
		}),
		"duplicate ids":        with(func(s map[string]any) { s["holders"] = []any{good, good} }),
		"other host":           with(func(s map[string]any) { s["host"] = "another-host.example" }),
		"no host":              with(func(s map[string]any) { delete(s, "host") }),
		"unknown version":      with(func(s map[string]any) { s["version"] = 3 }),
		"relative output":      entry(func(e map[string]any) { e["output"] = "relative/out" }),
		"no worktree":          entry(func(e map[string]any) { delete(e, "worktree") }),
		"relative worktree":    entry(func(e map[string]any) { e["worktree"] = "checkout" }),
		"empty id":             entry(func(e map[string]any) { e["id"] = "" }),
		"pid 0":                entry(func(e map[string]any) { e["pid"] = 0 }),
		"bad kind":             entry(func(e map[string]any) { e["kind"] = "other" }),
		"bad priority":         entry(func(e map[string]any) { e["priority"] = "low" }),
		"no startedAt":         entry(func(e map[string]any) { delete(e, "startedAt") }),
		"waiter no counters":   with(func(s map[string]any) { s["waiters"] = []any{sweepMatrixEntry("waiter-1", kept, true)} }),
		"v1 holder not object": `{"version":1,"host":"` + sweepHost + `","requestSeq":0,"grantSeq":0,"waiters":[],"holder":7}`,
	}
	for name, raw := range cases {
		if entries, why := validMatrixState([]byte(raw), sweepHost); why == "" {
			t.Fatalf("%s: accepted with entries %+v", name, entries)
		}
		writeFixtureFile(t, path, raw)
		f.now = f.now.Add(time.Hour)
		if !f.attempt() || f.sweeps != 0 {
			t.Fatalf("%s: attempted with %d sweeps", name, f.sweeps)
		}
		if line := lastSweepLine(t); line.Outcome != "skipped" || line.Reason != sweepSkipMatrix || line.Detail == "" {
			t.Fatalf("%s: line %+v", name, line)
		}
	}
	// An unreadable file skips too.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(time.Hour)
	if !f.attempt() || f.sweeps != 0 || lastSweepLine(t).Reason != sweepSkipMatrix {
		t.Fatalf("unreadable: %+v", lastSweepLine(t))
	}
	requireExists(t, kept, true)
	// The valid shapes are accepted: version 2, and version 1 with a holder.
	for name, raw := range map[string]string{
		"v2":      with(func(s map[string]any) { s["waiters"] = []any{sweepMatrixEntry("waiter-1", kept, false)} }),
		"v1":      `{"version":1,"host":"` + strings.ToUpper(sweepHost) + `.tail.example","requestSeq":0,"grantSeq":0,"waiters":[],"holder":` + func() string { data, _ := json.Marshal(good); return string(data) }() + `}`,
		"v1 free": `{"version":1,"host":"` + sweepHost + `","requestSeq":0,"grantSeq":0,"waiters":[],"holder":null}`,
	} {
		if _, why := validMatrixState([]byte(raw), sweepHost); why != "" {
			t.Fatalf("%s rejected: %s", name, why)
		}
	}
}

// shortenMatrixTimes makes the mutex wait, the hold release and the gap
// between holds short for one test.
func shortenMatrixTimes(t *testing.T, wait, release time.Duration) {
	t.Helper()
	oldWait, oldRelease, oldGap := matrixMutexWait, matrixHoldRelease, matrixHoldGap
	matrixMutexWait, matrixHoldRelease, matrixHoldGap = wait, release, time.Millisecond
	t.Cleanup(func() { matrixMutexWait, matrixHoldRelease, matrixHoldGap = oldWait, oldRelease, oldGap })
}

// t6 (a7): while the sweep holds the mutex no registration can be made, and
// the path is gone when the mutex is free again; a holder registered before
// the hold keeps its path; a mutex left by a gone process is reclaimed and a
// live owner's is not; a removal that outruns the limit releases the mutex
// and is journalled; no hold reaches the limit.
func TestSweepScheduleMatrixExclusion(t *testing.T) {
	f := newSweepFixture(t)
	mutex := os.Getenv("TAILTERM_MATRIX_HOST_LOCK") + ".lock"
	t.Run("registration is shut out during the final check and removal", func(t *testing.T) {
		gone := f.stale(t, "queue-5eeb0061")
		var refused, present atomic.Int32
		exclusiveAfterCheck = func() {
			token, err := takeMatrixMutex(mutex)
			if err != nil || token != "" {
				t.Errorf("a registration took the mutex during the hold: %q %v", token, err)
				return
			}
			refused.Add(1)
			if _, err := os.Stat(gone); err == nil {
				present.Add(1)
			}
		}
		t.Cleanup(func() { exclusiveAfterCheck = nil })
		if !f.attempt() || lastSweepLine(t).TreesRemoved != 1 {
			t.Fatalf("line %+v", lastSweepLine(t))
		}
		exclusiveAfterCheck = nil
		if refused.Load() != 1 || present.Load() != 1 {
			t.Fatalf("refused %d, present %d", refused.Load(), present.Load())
		}
		requireExists(t, gone, false)
		// The path is gone before a registration can be made.
		token, err := takeMatrixMutex(mutex)
		if err != nil || token == "" {
			t.Fatalf("the mutex was not released: %v", err)
		}
		releaseMatrixMutex(mutex, token)
		requireExists(t, mutex, false)
	})
	t.Run("a holder registered before the hold keeps its path", func(t *testing.T) {
		kept := f.stale(t, "queue-5eeb0062")
		matrix, why := loadMatrixExclusion()
		if matrix == nil {
			t.Fatal(why)
		}
		// The run registers after classification, before the sweep's hold.
		realRun := matrix.run
		registered := false
		decisions, err := cleanupWorktrees(context.Background(), worktreeCleanupInputs{Repo: f.r.main, Now: f.now, Apply: true, Receipt: worktreeCleanupReceipt{Source: "sweep"},
			Exclusive: func(act func(live []string)) (string, string) {
				if !registered {
					registered = true
					writeMatrixLock(t, sweepHost, []map[string]any{sweepMatrixEntry("holder-late", filepath.Join(kept, "hub"), true)}, nil)
				}
				return realRun(act)
			}})
		if err != nil {
			t.Fatal(err)
		}
		if len(decisions) != 1 || decisions[0].Action != "kept" || decisions[0].Reason != keepInUse || !strings.Contains(decisions[0].Detail, filepath.Join(kept, "hub")) {
			t.Fatalf("decisions %+v", decisions)
		}
		requireExists(t, kept, true)
		requireExists(t, mutex, false)
		// The file turning unusable during the pass keeps the path and ends it.
		writeFixtureFile(t, os.Getenv("TAILTERM_MATRIX_HOST_LOCK"), `{"version":9}`)
		if reason, detail := matrix.run(func([]string) { t.Error("act ran on an unusable file") }); reason != keepInUse || !strings.Contains(detail, "unknown version") {
			t.Fatalf("unusable file: %q %q", reason, detail)
		}
		if err := os.Remove(os.Getenv("TAILTERM_MATRIX_HOST_LOCK")); err != nil {
			t.Fatal(err)
		}
		if reason, _ := matrix.run(func([]string) { t.Error("act ran after the pass ended") }); reason != keepInUse {
			t.Fatalf("after the pass ended: %q", reason)
		}
		requireExists(t, mutex, false)
		if broken, _, _ := matrix.status(); broken == "" {
			t.Fatal("the pass was not marked ended")
		}
		if err := os.RemoveAll(kept); err != nil {
			t.Fatal(err)
		}
		cleanupGit(t, f.r.main, "worktree", "prune")
	})
	t.Run("a mutex left by a gone process is reclaimed", func(t *testing.T) {
		shortenMatrixTimes(t, 60*time.Millisecond, matrixHoldRelease)
		fakeMatrixOwners(t, map[int]fakeMatrixOwner{2147483646: {}})
		gone := f.stale(t, "queue-5eeb0063")
		writeFixtureFile(t, mutex, `{"pid":2147483646,"token":"left-by-a-dead-sweep","at":"2026-10-07T10:00:00.000Z"}`+"\n")
		f.now = f.now.Add(time.Hour)
		if !f.attempt() {
			t.Fatal("no attempt")
		}
		line := lastSweepLine(t)
		if line.Outcome != "swept" || line.TreesRemoved != 1 || !strings.Contains(line.Detail, "reclaimed the matrix mutex left by pid 2147483646") {
			t.Fatalf("reclaim line %+v", line)
		}
		requireExists(t, gone, false)
		requireExists(t, mutex, false)
		requireExists(t, mutex+".reclaim", false)
	})
	t.Run("a busy mutex with a live owner keeps the path and is not reclaimed", func(t *testing.T) {
		shortenMatrixTimes(t, 60*time.Millisecond, matrixHoldRelease)
		fakeMatrixOwners(t, map[int]fakeMatrixOwner{2147483646: {alive: true}})
		kept := f.stale(t, "queue-5eeb0065")
		live := `{"pid":2147483646,"token":"held-by-a-live-run","at":"2026-10-07T10:00:00.000Z"}` + "\n"
		writeFixtureFile(t, mutex, live)
		f.now = f.now.Add(time.Hour)
		if !f.attempt() {
			t.Fatal("no attempt")
		}
		line := lastSweepLine(t)
		if line.Outcome != "skipped" || line.Reason != sweepSkipMatrix || line.TreesRemoved != 0 || line.Kept[keepInUse] != 1 || !strings.Contains(line.Detail, "1 path(s) kept: matrix mutex busy (owner pid 2147483646)") {
			t.Fatalf("busy line %+v", line)
		}
		if len(f.last) != 1 || !strings.Contains(f.last[0].Detail, "matrix lock busy") {
			t.Fatalf("busy decisions %+v", f.last)
		}
		requireExists(t, kept, true)
		if data, err := os.ReadFile(mutex); err != nil || string(data) != live {
			t.Fatalf("the sweep touched another owner's mutex: %q %v", data, err)
		}
		requireExists(t, mutex+".reclaim", false)
		if err := os.Remove(mutex); err != nil {
			t.Fatal(err)
		}
		if err := os.RemoveAll(kept); err != nil {
			t.Fatal(err)
		}
		cleanupGit(t, f.r.main, "worktree", "prune")
	})
	t.Run("a removal that outruns the limit releases the mutex and is journalled", func(t *testing.T) {
		// The hold is released at 300 ms here; the removal takes over a second.
		shortenMatrixTimes(t, matrixMutexWait, 300*time.Millisecond)
		gone := f.stale(t, "queue-5eeb0064")
		original := worktreeGit
		var freedDuringRemoval atomic.Bool
		worktreeGit = func(ctx context.Context, dir string, args ...string) (string, error) {
			if len(args) == 3 && args[1] == "remove" && args[2] == gone {
				time.Sleep(1200 * time.Millisecond)
				if _, err := os.Stat(mutex); os.IsNotExist(err) {
					freedDuringRemoval.Store(true)
				}
			}
			return original(ctx, dir, args...)
		}
		t.Cleanup(func() { worktreeGit = original })
		var matrix *matrixExclusion
		f.deps.sweep = func(ctx context.Context, state sweepHubState, host string, o sweepOptions) ([]worktreeDecision, error) {
			matrix = o.Matrix
			return runWorktreeSweep(ctx, state, host, o)
		}
		f.now = f.now.Add(time.Hour)
		if !f.attempt() {
			t.Fatal("no attempt")
		}
		line := lastSweepLine(t)
		if line.Outcome != "swept" || line.TreesRemoved != 1 || line.ExclusionExceeded != 1 {
			t.Fatalf("exceeded line %+v", line)
		}
		if !freedDuringRemoval.Load() {
			t.Fatal("the mutex was still held while the long removal ran")
		}
		requireExists(t, gone, false)
		requireExists(t, mutex, false)
		var slow worktreeCleanupReceipt
		for _, receipt := range readCleanupReceipts(t) {
			if receipt.Path == gone && receipt.Action == "removed" {
				slow = receipt
			}
		}
		if !strings.Contains(slow.Detail, "outran the matrix hold limit") {
			t.Fatalf("the long removal's receipt %+v", slow)
		}
		// The hold ended at the release, well before the removal did.
		_, exceeded, longest := matrix.status()
		if exceeded != 1 || longest < 300*time.Millisecond || longest >= 1200*time.Millisecond {
			t.Fatalf("exceeded %d, longest hold %s", exceeded, longest)
		}
	})
	// The shipped limits: the mutex is released before the limit, which is
	// below the 30 s after which a matrix holder aborts its own run.
	if matrixHoldRelease >= matrixHoldLimit || matrixHoldLimit != 20*time.Second || matrixMutexWait != 2*time.Second {
		t.Fatalf("limits: release %s, limit %s, wait %s", matrixHoldRelease, matrixHoldLimit, matrixMutexWait)
	}
}

const matrixLockScript = "../../../scripts/verify-matrix-host-lock.mjs"

// t7 (a7): the mutex is the script's own. While Go holds it the script's
// updateHostState is not done; while the script holds it Go reports busy; and
// a file the script wrote passes the sweep's validation.
func TestSweepScheduleMatrixScriptCompatibility(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not on PATH; the script side of the mutex is not exercised")
	}
	script, err := filepath.Abs(matrixLockScript)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(script); err != nil {
		t.Skipf("the matrix lock script is not at %s", script)
	}
	f := newSweepFixture(t)
	shortenMatrixTimes(t, 80*time.Millisecond, matrixHoldRelease)
	file := os.Getenv("TAILTERM_MATRIX_HOST_LOCK")
	if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
		t.Fatal(err)
	}
	hostname, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	matrixWaitHostname = os.Hostname
	held := f.stale(t, "queue-5eeb0071")
	runNode := func(source string) string {
		t.Helper()
		cmd := exec.Command(node, "--input-type=module", "-e", source)
		cmd.Env = append(os.Environ(), "LOCK_SCRIPT="+script, "LOCK_FILE="+file, "LOCK_WORKTREE="+held)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("node: %v: %s", err, out)
		}
		return strings.TrimSpace(string(out))
	}
	register := `const { updateHostState } = await import(process.env.LOCK_SCRIPT);
const result = updateHostState(process.env.LOCK_FILE, (state) => {
  state.requestSeq += 1;
  state.grantSeq += 1;
  state.holders.push({ id: "script-holder", pid: process.pid, seq: state.requestSeq, requestedAt: new Date().toISOString(), startedAt: new Date().toISOString(),
    kind: "run", item: "` + sweepItem + `", agent: "verifier", priority: "normal", runTimeoutMs: 600000, worktree: process.env.LOCK_WORKTREE });
});
console.log(JSON.stringify({ done: result.done }));`
	// Go holds the mutex: the script cannot register.
	mutex := file + ".lock"
	token, err := takeMatrixMutex(mutex)
	if err != nil || token == "" {
		t.Fatalf("take: %q %v", token, err)
	}
	if out := runNode(register); out != `{"done":false}` {
		t.Fatalf("the script registered while Go held the mutex: %s", out)
	}
	requireExists(t, file, false)
	releaseMatrixMutex(mutex, token)
	requireExists(t, mutex, false)
	// Released: the script registers, and the sweep trusts the file it wrote.
	if out := runNode(register); out != `{"done":true}` {
		t.Fatalf("the script could not register on a free mutex: %s", out)
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	entries, why := validMatrixState(raw, hostname)
	if why != "" || len(entries) != 1 || entries[0].Worktree != held {
		t.Fatalf("script-written file: %q %+v\n%s", why, entries, raw)
	}
	if !f.attempt() {
		t.Fatal("no attempt")
	}
	if line := lastSweepLine(t); line.Outcome != "swept" || line.Kept[keepInUse] != 1 {
		t.Fatalf("script holder line %+v", line)
	}
	requireExists(t, held, true)
	// The sweep itself holds the mutex across its final check and removal:
	// from inside that hold the script cannot register on the path, and when
	// the mutex is free again the path is already gone.
	gone := f.stale(t, "queue-5eeb0072")
	registerOnGone := strings.Replace(register, `id: "script-holder"`, `id: "script-late"`, 1)
	during := ""
	exclusiveAfterCheck = func() {
		cmd := exec.Command(node, "--input-type=module", "-e", registerOnGone)
		cmd.Env = append(os.Environ(), "LOCK_SCRIPT="+script, "LOCK_FILE="+file, "LOCK_WORKTREE="+gone)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Errorf("node during the hold: %v: %s", err, out)
		}
		during = strings.TrimSpace(string(out))
		if _, err := os.Stat(gone); err != nil {
			t.Errorf("the path was removed before the hold's check ended: %v", err)
		}
	}
	t.Cleanup(func() { exclusiveAfterCheck = nil })
	f.now = f.now.Add(time.Hour)
	if !f.attempt() {
		t.Fatal("no attempt")
	}
	exclusiveAfterCheck = nil
	if during != `{"done":false}` {
		t.Fatalf("the script registered while the sweep held the mutex: %q", during)
	}
	if line := lastSweepLine(t); line.Outcome != "swept" || line.TreesRemoved != 1 || line.ExclusionExceeded != 0 {
		t.Fatalf("sweep under a script registration attempt: %+v", line)
	}
	requireExists(t, gone, false)
	requireExists(t, held, true)
	if raw, err := os.ReadFile(file); err != nil || strings.Contains(string(raw), "script-late") || !strings.Contains(string(raw), "script-holder") {
		t.Fatalf("the lock file after the refused registration: %v\n%s", err, raw)
	}
	// The script holds the mutex: Go reports busy and removes nothing.
	hold := exec.Command(node, "--input-type=module", "-e", `const { takeMutex, releaseMutex, lockPaths } = await import(process.env.LOCK_SCRIPT);
const paths = lockPaths(process.env.LOCK_FILE);
const token = takeMutex(paths);
console.log(token ? "held" : "busy");
process.stdin.on("end", () => { if (token) releaseMutex(paths, token); process.exit(0); });
process.stdin.resume();`)
	hold.Env = append(os.Environ(), "LOCK_SCRIPT="+script, "LOCK_FILE="+file)
	stdin, err := hold.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := hold.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := hold.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stdin.Close(); _ = hold.Wait() })
	if line, _ := bufio.NewReader(stdout).ReadString('\n'); strings.TrimSpace(line) != "held" {
		t.Fatalf("the script did not take the mutex: %q", line)
	}
	matrix, why := loadMatrixExclusion()
	if matrix == nil {
		t.Fatal(why)
	}
	if reason, detail := matrix.run(func([]string) { t.Error("act ran while the script held the mutex") }); reason != keepInUse || !strings.Contains(detail, "matrix lock busy") {
		t.Fatalf("Go against a script-held mutex: %q %q", reason, detail)
	}
	stdin.Close()
	if err := hold.Wait(); err != nil {
		t.Fatal(err)
	}
	requireExists(t, mutex, false)
	// k3: Go reclaims a mutex whose owner is gone under the script's guard
	// protocol, and the script registers afterwards.
	fakeMatrixOwners(t, map[int]fakeMatrixOwner{2147483646: {}})
	writeFixtureFile(t, mutex, `{"pid":2147483646,"token":"left-by-a-dead-sweep","at":"2026-10-07T10:00:00.000Z"}`+"\n")
	acted := false
	if reason, detail := matrix.run(func([]string) { acted = true }); reason != "" || !acted {
		t.Fatalf("Go against a dead owner's mutex: %q %q", reason, detail)
	}
	if busy, _, reclaimed := matrix.mutexStatus(); busy != 1 || len(reclaimed) != 1 {
		t.Fatalf("after the reclaim: busy %d, reclaimed %v", busy, reclaimed)
	}
	requireExists(t, mutex, false)
	requireExists(t, mutex+".reclaim", false)
	if out := runNode(strings.Replace(register, `id: "script-holder"`, `id: "script-after-reclaim"`, 1)); out != `{"done":true}` {
		t.Fatalf("the script could not register after Go's reclaim: %s", out)
	}
	if raw, err := os.ReadFile(file); err != nil || !strings.Contains(string(raw), "script-after-reclaim") {
		t.Fatalf("the lock file after the reclaim: %v\n%s", err, raw)
	}
}

// t8 (a3): each attempt adds exactly one journal line with every planned
// field; --journal prints it as text and JSON; a run above the threshold
// posts nothing.
func TestSweepScheduleJournal(t *testing.T) {
	f := newSweepFixture(t)
	f.stale(t, "queue-5eeb0081")
	temp := filepath.Join(f.r.tempRoot(), claudeScratchKey(f.r.queuePath("queue-5eeb0082")))
	writeFixtureFile(t, filepath.Join(temp, "session", "scratchpad", "note.txt"), "scratch\n")
	entry := f.entries()[0]
	entry.Cwd = f.r.queuePath("queue-5eeb0082")
	f.setEntries(entry)
	if out, err := captureSweepStdout(t, func() error { return cmdTeamQueueSweepWorktrees(env{}, []string{"--journal"}) }); err != nil || !strings.Contains(out, "no scheduled sweep has run") {
		t.Fatalf("empty journal: %v %q", err, out)
	}
	if !f.attempt() {
		t.Fatal("no attempt")
	}
	lock, err := worktreeCleanupLock()
	if err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(time.Hour)
	if !f.attempt() {
		t.Fatal("no skipped attempt")
	}
	unlockQueueLaunch(lock)
	data, err := os.ReadFile(sweepJournalPath())
	if err != nil {
		t.Fatal(err)
	}
	raw := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(raw) != 2 {
		t.Fatalf("two attempts wrote %d lines:\n%s", len(raw), data)
	}
	for _, text := range raw {
		var fields map[string]any
		if err := json.Unmarshal([]byte(text), &fields); err != nil {
			t.Fatalf("line %q: %v", text, err)
		}
		for _, key := range []string{"at", "source", "outcome", "reason", "repositories", "treesRemoved", "foldersRemoved", "cachesTrimmed", "pruned", "bytesFreed", "freeBytesAfter", "kept", "minIdle", "durationMs", "exclusionExceeded", "lowSpace", "noticeSeq", "noticePending"} {
			if _, ok := fields[key]; !ok {
				t.Fatalf("line lacks %s: %s", key, text)
			}
		}
	}
	lines := sweepJournalLines(t)
	swept := lines[0]
	if swept.Source != "schedule" || swept.Outcome != "swept" || swept.TreesRemoved != 1 || swept.FoldersRemoved != 1 || swept.BytesFreed <= 0 || swept.FreeBytesAfter != 100<<30 || swept.LowSpace || swept.NoticeSeq != 0 || swept.NoticePending != "" {
		t.Fatalf("swept line %+v", swept)
	}
	// The journal's time is the time on that pass's cleanup receipts.
	receipts := readCleanupReceipts(t)
	if len(receipts) == 0 {
		t.Fatal("no cleanup receipts")
	}
	for _, r := range receipts {
		if r.At != swept.At || r.Source != "sweep" {
			t.Fatalf("receipt %+v against journal time %s", r, swept.At)
		}
	}
	if lines[1].Outcome != "skipped" || lines[1].Reason != sweepSkipCleanup {
		t.Fatalf("skipped line %+v", lines[1])
	}
	stored, attempts, _, _ := f.hub.messages()
	if len(stored) != 0 || len(attempts) != 0 {
		t.Fatalf("a run with space to spare posted: %+v", attempts)
	}
	if _, writes := f.hub.snapshot(); len(writes) != 0 {
		t.Fatalf("the sweep wrote to the hub: %v", writes)
	}
	// The tt read path: text, JSON, a limit, and no hub.
	out, err := captureSweepStdout(t, func() error { return cmdTeamQueueSweepWorktrees(env{}, []string{"--journal"}) })
	if err != nil || !strings.Contains(out, swept.At+" swept repositories=1 trees=1 folders=1 binaries=0 caches=0 pruned=0 freed=") || !strings.Contains(out, "free=100.0 GiB min-idle=6h0m0s") || !strings.Contains(out, lines[1].At+" skipped cleanup-active") {
		t.Fatalf("--journal: %v\n%s", err, out)
	}
	out, err = captureSweepStdout(t, func() error {
		return cmdTeamQueueSweepWorktrees(env{}, []string{"--journal", "--json", "--limit", "1"})
	})
	var printed []sweepJournalLine
	if err != nil || json.Unmarshal([]byte(out), &printed) != nil || len(printed) != 1 || !reflect.DeepEqual(printed[0], lines[1]) {
		t.Fatalf("--journal --json --limit 1: %v\n%s", err, out)
	}
	for _, args := range [][]string{{"--journal", "--apply"}, {"--journal", "--limit", "0"}, {"--journal", "extra"}} {
		if err := cmdTeamQueueSweepWorktrees(env{}, args); err == nil || !strings.Contains(err.Error(), "usage:") {
			t.Fatalf("%v: %v", args, err)
		}
	}
}

func (f *sweepFixture) helper(id, name, task string, created time.Time) api.Agent {
	return api.Agent{ID: id, Name: name, TaskID: task, Role: api.AgentRoleOwnerHelper, Status: api.AgentRunning, CreatedAt: created}
}

func (f *sweepFixture) setAgents(task string, agents ...api.Agent) {
	f.hub.mu.Lock()
	detail := f.hub.details[task]
	detail.Agents = agents
	f.hub.details[task] = detail
	f.hub.mu.Unlock()
}

// t9 (a4): one notice per low-space episode, naming free space, bytes freed
// and the largest kept items with their reasons.
func TestSweepScheduleLowSpaceEpisode(t *testing.T) {
	f := newSweepFixture(t)
	f.setAgents(sweepTask, f.helper("agt_5eeb5eeb5eeb5e0a", "owner-helper", sweepTask, time.Now().Add(-time.Hour)),
		api.Agent{ID: "agt_5eeb5eeb5eeb5e0b", Name: "owner-helper-old", TaskID: sweepTask, Role: api.AgentRoleOwnerHelper, Status: api.AgentClosed, CreatedAt: time.Now()})
	f.stale(t, "queue-5eeb0091")
	big := f.r.branchWorktree(t, f.r.queuePath("queue-5eeb0092"), "feat/kept-big")
	writeFixtureFile(t, filepath.Join(big, ".build", "blob.bin"), strings.Repeat("x", 9000))
	small := f.r.branchWorktree(t, f.r.queuePath("queue-5eeb0093"), "feat/kept-small")
	f.free = 5 << 30
	if !f.attempt() {
		t.Fatal("no attempt")
	}
	stored, attempts, _, tasks := f.hub.messages()
	if len(stored) != 1 || len(attempts) != 1 || tasks[0] != sweepTask {
		t.Fatalf("first low run stored %d of %d posts", len(stored), len(attempts))
	}
	post := attempts[0]
	if post.To != "agt_5eeb5eeb5eeb5e0a" || post.Envelope == nil || post.Envelope.Kind != api.EnvelopeKindNotice || post.Envelope.To != "owner-helper" || post.Envelope.Subject != sweepNoticeSubject ||
		post.RequestID != sweepNoticeRequestID(sweepHost, f.now) || post.AgentID != "" || post.ReplyTo != 0 {
		t.Fatalf("notice post %+v", post)
	}
	text := post.Envelope.Body.Text
	for _, want := range []string{"Host " + sweepHost + " has 5.0 GiB free", "below the 8.0 GiB threshold", "This run freed ", "1 worktrees, 0 session temp folders, 0 matrix test binaries, 0 caches trimmed",
		"Largest kept: " + big + " (", ", " + keepUnpushed + "); " + small + " (", "once per low-space episode", "tt team queue sweep-worktrees --journal"} {
		if !strings.Contains(text, want) {
			t.Fatalf("notice lacks %q:\n%s", want, text)
		}
	}
	if post.Text != api.RenderText(*post.Envelope) || len(text) > api.MaxEnvelopeBodyBytes {
		t.Fatalf("notice text does not render its envelope, or is too long (%d)", len(text))
	}
	line := lastSweepLine(t)
	if !line.LowSpace || line.NoticeSeq != stored[0].Seq || line.NoticePending != "" || line.FreeBytesAfter != 5<<30 {
		t.Fatalf("low line %+v", line)
	}
	// Still low: the same episode, no second notice.
	for i := 0; i < 2; i++ {
		f.now = f.now.Add(time.Hour)
		if !f.attempt() || !lastSweepLine(t).LowSpace || lastSweepLine(t).NoticeSeq != 0 {
			t.Fatalf("still low: %+v", lastSweepLine(t))
		}
	}
	// Skipped and failed runs neither begin nor end an episode.
	lock, err := worktreeCleanupLock()
	if err != nil {
		t.Fatal(err)
	}
	f.free = 50 << 30
	f.now = f.now.Add(time.Hour)
	f.attempt()
	unlockQueueLaunch(lock)
	if state, _ := loadSweepState(); state.Episode == nil {
		t.Fatal("a skipped run ended the episode")
	}
	// Recovered: nothing is posted and the episode ends.
	f.now = f.now.Add(time.Hour)
	if !f.attempt() || lastSweepLine(t).LowSpace {
		t.Fatalf("recovered: %+v", lastSweepLine(t))
	}
	if state, _ := loadSweepState(); state.Episode != nil {
		t.Fatal("the episode did not end")
	}
	if stored, attempts, _, _ := f.hub.messages(); len(stored) != 1 || len(attempts) != 1 {
		t.Fatalf("after recovery: %d stored, %d posts", len(stored), len(attempts))
	}
	// Low again: a second episode and a second notice under its own identity.
	f.free = 7 << 30
	f.now = f.now.Add(time.Hour)
	if !f.attempt() {
		t.Fatal("no attempt")
	}
	stored, attempts, _, _ = f.hub.messages()
	if len(stored) != 2 || len(attempts) != 2 || attempts[1].RequestID == attempts[0].RequestID || attempts[1].RequestID != sweepNoticeRequestID(sweepHost, f.now) {
		t.Fatalf("second episode: %d stored, posts %+v", len(stored), attempts)
	}
	// Exactly at the threshold is not low.
	f.free = 8 << 30
	f.now = f.now.Add(time.Hour)
	if !f.attempt() || lastSweepLine(t).LowSpace {
		t.Fatalf("at the threshold: %+v", lastSweepLine(t))
	}
}

func pendingSweepNotices(t *testing.T) []sweepNotice {
	t.Helper()
	state, err := loadSweepState()
	if err != nil {
		t.Fatal(err)
	}
	return state.PendingNotices
}

// t10 (a4, R2): the notice is frozen and saved before it is posted, replayed
// unchanged until a matching receipt, and each episode keeps its own.
func TestSweepScheduleNoticeDurability(t *testing.T) {
	helperAt := time.Now().Add(-time.Hour)
	low := func(t *testing.T) *sweepFixture {
		f := newSweepFixture(t)
		f.setAgents(sweepTask, f.helper("agt_5eeb5eeb5eeb5e0a", "owner-helper", sweepTask, helperAt))
		f.free = 1 << 30
		return f
	}
	t.Run("a lost response is settled by the receipt lookup", func(t *testing.T) {
		f := low(t)
		f.hub.setPosts("lose", true)
		if !f.attempt() {
			t.Fatal("no attempt")
		}
		stored, attempts, _, _ := f.hub.messages()
		line := lastSweepLine(t)
		if len(stored) != 1 || len(attempts) != 1 || line.NoticeSeq != stored[0].Seq || line.NoticePending != "" || len(pendingSweepNotices(t)) != 0 {
			t.Fatalf("stored %d, posts %d, line %+v, pending %+v", len(stored), len(attempts), line, pendingSweepNotices(t))
		}
		f.hub.setPosts("", true)
		f.now = f.now.Add(time.Hour)
		f.attempt()
		if stored, attempts, _, _ := f.hub.messages(); len(stored) != 1 || len(attempts) != 1 {
			t.Fatalf("a delivered notice was posted again: %d stored, %d posts", len(stored), len(attempts))
		}
	})
	t.Run("a lost response with no lookup is settled by the unchanged replay", func(t *testing.T) {
		// The live hub files a relay-authored post under the relay, so the
		// owner's lookup does not find it: the replay must settle it.
		f := low(t)
		f.hub.setPosts("lose", false)
		f.attempt()
		line := lastSweepLine(t)
		if stored, _, _, _ := f.hub.messages(); len(stored) != 1 || line.NoticeSeq != 0 || line.NoticePending != sweepNoticeRequestID(sweepHost, f.now) {
			t.Fatalf("stored %d, line %+v", len(stored), line)
		}
		f.hub.setPosts("", false)
		f.now = f.now.Add(time.Hour)
		f.attempt()
		stored, attempts, bodies, _ := f.hub.messages()
		line = lastSweepLine(t)
		if len(stored) != 1 || len(attempts) != 2 || bodies[0] != bodies[1] || line.NoticeSeq != stored[0].Seq || line.NoticePending != "" {
			t.Fatalf("stored %d, posts %d, same bytes %v, line %+v", len(stored), len(attempts), bodies[0] == bodies[1], line)
		}
	})
	t.Run("a post the hub never stored is replayed with the same identity and bytes after a restart and a helper change", func(t *testing.T) {
		f := low(t)
		f.hub.setPosts("fail", true)
		f.attempt()
		stored, attempts, _, _ := f.hub.messages()
		pending := pendingSweepNotices(t)
		line := lastSweepLine(t)
		if len(stored) != 0 || len(attempts) != 1 || len(pending) != 1 || line.NoticeSeq != 0 || line.NoticePending != pending[0].RequestID || !line.LowSpace {
			t.Fatalf("stored %d, posts %d, pending %+v, line %+v", len(stored), len(attempts), pending, line)
		}
		frozen := pending[0]
		// A relay restart keeps nothing in memory: only the state file. Before
		// the retry a newer owner helper appears in another project.
		const otherTask = "tsk_5eeb5eeb5eeb5e99"
		f.hub.mu.Lock()
		f.hub.tasks = append(f.hub.tasks, api.Task{ID: otherTask, Status: api.TaskOpen})
		f.hub.details[otherTask] = api.TaskDetail{Task: api.Task{ID: otherTask, Status: api.TaskOpen}, Agents: []api.Agent{f.helper("agt_5eeb5eeb5eeb5e0c", "newer-helper", otherTask, time.Now())}}
		f.hub.mu.Unlock()
		restarted := newSweepFixtureDeps(f)
		f.hub.setPosts("", true)
		f.now = f.now.Add(time.Hour)
		if !sweepScheduleAttempt(context.Background(), restarted) {
			t.Fatal("no attempt after the restart")
		}
		stored, attempts, bodies, tasks := f.hub.messages()
		if len(stored) != 1 || len(attempts) != 2 || bodies[0] != bodies[1] || tasks[1] != sweepTask || attempts[1].To != "agt_5eeb5eeb5eeb5e0a" || attempts[1].RequestID != frozen.RequestID || attempts[1].Text != frozen.Text {
			t.Fatalf("replay: stored %d, posts %+v, tasks %v", len(stored), attempts, tasks)
		}
		if line := lastSweepLine(t); line.NoticeSeq != stored[0].Seq || line.NoticePending != "" || len(pendingSweepNotices(t)) != 0 {
			t.Fatalf("after the replay: %+v, pending %+v", line, pendingSweepNotices(t))
		}
	})
	t.Run("a bare conflict with no matching receipt stays pending", func(t *testing.T) {
		f := low(t)
		f.hub.setPosts("conflict", true)
		f.attempt()
		f.now = f.now.Add(time.Hour)
		f.attempt()
		stored, attempts, _, _ := f.hub.messages()
		if line := lastSweepLine(t); len(stored) != 0 || len(attempts) != 2 || len(pendingSweepNotices(t)) != 1 || line.NoticeSeq != 0 || line.NoticePending == "" {
			t.Fatalf("stored %d, posts %d, line %+v", len(stored), len(attempts), line)
		}
	})
	t.Run("nothing is posted when the notice cannot be saved first", func(t *testing.T) {
		f := low(t)
		f.free = 50 << 30
		f.attempt()
		// From here the state file cannot be replaced: its directory is read-only.
		if err := os.Chmod(relayDir(), 0500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(relayDir(), 0700) })
		if os.Getuid() == 0 {
			t.Skip("root writes to a read-only directory")
		}
		f.free = 1 << 30
		f.now = f.now.Add(time.Hour)
		f.attempt()
		if _, attempts, _, _ := f.hub.messages(); len(attempts) != 0 {
			t.Fatalf("posted from an unsaved state: %+v", attempts)
		}
		if line := lastSweepLine(t); line.Outcome != "failed" || line.Reason != "state" || !line.LowSpace {
			t.Fatalf("line %+v", line)
		}
		if state, _ := loadSweepState(); state.Episode != nil || len(state.PendingNotices) != 0 {
			t.Fatalf("state %+v", state)
		}
	})
	t.Run("crossed episodes keep one notice each", func(t *testing.T) {
		f := low(t)
		f.hub.setPosts("fail", false)
		f.attempt()
		first := pendingSweepNotices(t)
		f.free = 50 << 30
		f.now = f.now.Add(time.Hour)
		f.attempt()
		if state, _ := loadSweepState(); state.Episode != nil || len(state.PendingNotices) != 1 {
			t.Fatalf("recovered with the post still failing: %+v", state)
		}
		f.free = 2 << 30
		f.now = f.now.Add(time.Hour)
		f.attempt()
		both := pendingSweepNotices(t)
		if len(first) != 1 || len(both) != 2 || !reflect.DeepEqual(both[0], first[0]) || both[1].RequestID == both[0].RequestID {
			t.Fatalf("pending after the second episode began: %+v", both)
		}
		if line := lastSweepLine(t); line.NoticePending != both[0].RequestID+","+both[1].RequestID {
			t.Fatalf("line %+v", line)
		}
		f.hub.setPosts("", false)
		f.now = f.now.Add(time.Hour)
		f.attempt()
		stored, attempts, _, _ := f.hub.messages()
		if len(stored) != 2 || stored[0].PostReceipt.RequestID != both[0].RequestID || stored[1].PostReceipt.RequestID != both[1].RequestID || len(pendingSweepNotices(t)) != 0 {
			t.Fatalf("stored %+v after %d posts, pending %+v", stored, len(attempts), pendingSweepNotices(t))
		}
		if !strings.Contains(stored[0].Text, "1.0 GiB free") || !strings.Contains(stored[1].Text, "2.0 GiB free") {
			t.Fatalf("each episode's own text: %q / %q", stored[0].Text, stored[1].Text)
		}
		f.now = f.now.Add(time.Hour)
		f.attempt()
		if stored, _, _, _ := f.hub.messages(); len(stored) != 2 {
			t.Fatalf("%d messages for two episodes", len(stored))
		}
	})
	t.Run("the pending list is bounded and the journal says what it dropped", func(t *testing.T) {
		f := low(t)
		f.hub.setPosts("fail", false)
		var ids []string
		for i := 0; i < maxPendingSweepNotices+1; i++ {
			f.free = 1 << 30
			f.now = f.now.Add(time.Hour)
			f.attempt()
			ids = append(ids, sweepNoticeRequestID(sweepHost, f.now))
			if i < maxPendingSweepNotices && lastSweepLine(t).NoticesDropped != 0 {
				t.Fatalf("dropped early: %+v", lastSweepLine(t))
			}
			if i < maxPendingSweepNotices {
				f.free = 50 << 30
				f.now = f.now.Add(time.Hour)
				f.attempt()
			}
		}
		pending := pendingSweepNotices(t)
		if line := lastSweepLine(t); len(pending) != maxPendingSweepNotices || pending[0].RequestID != ids[1] || line.NoticesDropped != 1 {
			t.Fatalf("pending %d, oldest %s, line %+v", len(pending), pending[0].RequestID, line)
		}
	})
	t.Run("with no owner helper the notice goes to the Board of the host's newest entry", func(t *testing.T) {
		f := low(t)
		f.setAgents(sweepTask)
		f.attempt()
		stored, attempts, _, tasks := f.hub.messages()
		if len(stored) != 1 || attempts[0].To != "" || attempts[0].Envelope.To != "" || tasks[0] != sweepTask {
			t.Fatalf("board notice %+v in %v", attempts, tasks)
		}
	})
}

// newSweepFixtureDeps is a second set of schedule inputs over the same
// fixture, as a restarted relay would build.
func newSweepFixtureDeps(f *sweepFixture) sweepScheduleDeps {
	deps := f.deps
	deps.now = func() time.Time { return f.now }
	return deps
}

// t11 (a10): the relay runs one sweep at a time off its loop, and none under
// --once or --status.
func TestSweepScheduleRelayWiring(t *testing.T) {
	old := relaySweepSchedule
	t.Cleanup(func() { relaySweepSchedule = old })
	var calls atomic.Int32
	release := make(chan struct{})
	relaySweepSchedule = func(ctx context.Context) {
		calls.Add(1)
		if _, ok := ctx.Deadline(); !ok {
			t.Error("the sweep has no deadline")
		}
		<-release
	}
	var pass relaySweepPass
	pass.start()
	pass.lastCheck = time.Time{}
	pass.start()
	pass.collect()
	if pass.done == nil {
		t.Fatal("the pass was collected while in flight")
	}
	close(release)
	<-pass.done
	pass.collect()
	if calls.Load() != 1 || pass.done != nil {
		t.Fatalf("%d sweeps in flight at once", calls.Load())
	}
	// Due checks are spaced.
	pass.start()
	if calls.Load() != 1 {
		t.Fatal("a second check ran inside the check interval")
	}
	pass.lastCheck = time.Now().Add(-sweepCheckInterval)
	pass.start()
	<-pass.done
	pass.collect()
	if calls.Load() != 2 {
		t.Fatalf("%d sweeps after the interval", calls.Load())
	}
	// A panic in one sweep does not take the relay down.
	relaySweepSchedule = func(context.Context) { panic("sweep fault") }
	var stderr strings.Builder
	restore := captureStderr(t, &stderr)
	pass.lastCheck = time.Time{}
	pass.start()
	<-pass.done
	restore()
	pass.collect()
	if !strings.Contains(stderr.String(), "worktree sweep: sweep fault") {
		t.Fatalf("panic output %q", stderr.String())
	}

	// The relay command: nothing under --once or --status, one sweep from
	// the loop.
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("TAILTERM_RELAY_STATE", filepath.Join(root, "relay"))
	t.Setenv("TAILTERM_HUB", "")
	oldHandler, oldSteward, oldPause := relayHandlerRotation, relayStewardRotation, relayLoopPause
	t.Cleanup(func() { relayHandlerRotation, relayStewardRotation, relayLoopPause = oldHandler, oldSteward, oldPause })
	relayHandlerRotation = func(context.Context) error { return nil }
	relayStewardRotation = func(context.Context) error { return nil }
	calls.Store(0)
	ran := make(chan struct{}, 8)
	relaySweepSchedule = func(context.Context) { calls.Add(1); ran <- struct{}{} }
	for _, flag := range []string{"--once", "--status", "--once"} {
		if _, err := captureRelayOutput(t, true, func() error { return cmdRelay([]string{flag}) }); err != nil {
			t.Fatalf("relay %s: %v", flag, err)
		}
	}
	time.Sleep(50 * time.Millisecond)
	if calls.Load() != 0 {
		t.Fatalf("%d sweeps started under --once or --status", calls.Load())
	}
	passes := 0
	relayLoopPause = func() bool {
		passes++
		if passes == 1 {
			<-ran
		}
		return passes < 3
	}
	if _, err := captureRelayOutput(t, true, func() error { return cmdRelay(nil) }); err != nil {
		t.Fatalf("relay loop: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("the loop started %d sweeps in three passes", calls.Load())
	}
}

// The state file: one lock for every writer, and a corrupt file fails closed.
func TestSweepScheduleState(t *testing.T) {
	f := newSweepFixture(t)
	kept := f.stale(t, "queue-5eeb00a1")
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := saveDetachIntent(detachIntent{Tombstone: fmt.Sprintf("%s%02d", tombstonePrefix, i)}); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	intents, err := loadDetachIntents()
	if err != nil || len(intents) != 20 {
		t.Fatalf("%d intents after 20 concurrent writers: %v", len(intents), err)
	}
	for _, intent := range intents {
		if err := clearDetachIntent(intent.Tombstone); err != nil {
			t.Fatal(err)
		}
	}
	if info, err := os.Stat(sweepStatePath()); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("state file mode: %v %v", info, err)
	}
	if err := os.WriteFile(sweepStatePath(), []byte("{not json"), 0600); err != nil {
		t.Fatal(err)
	}
	if !f.attempt() || f.sweeps != 0 {
		t.Fatalf("a corrupt state file ran %d sweeps", f.sweeps)
	}
	if line := lastSweepLine(t); line.Outcome != "failed" || line.Reason != "state" {
		t.Fatalf("line %+v", line)
	}
	requireExists(t, kept, true)
	if err := saveDetachIntent(detachIntent{Tombstone: tombstonePrefix + "x"}); err == nil {
		t.Fatal("an intent was saved over a corrupt state file")
	}
}

// A host with no hub configured makes no attempt, and a hub that cannot be
// read skips the run before anything is touched.
func TestSweepScheduleHubFailures(t *testing.T) {
	f := newSweepFixture(t)
	kept := f.stale(t, "queue-5eeb00b1")
	realHub := f.deps.hub
	f.deps.hub = func() (*api.Client, string, bool) { return nil, "", false }
	if f.attempt() || len(sweepJournalLines(t)) != 0 {
		t.Fatal("a host with no hub made an attempt")
	}
	f.deps.hub = func() (*api.Client, string, bool) {
		c, _ := api.NewClient("http://127.0.0.1:1", time.Second)
		return c, sweepHost, true
	}
	if !f.attempt() || f.sweeps != 0 {
		t.Fatalf("unreachable hub: %d sweeps", f.sweeps)
	}
	if line := lastSweepLine(t); line.Outcome != "skipped" || line.Reason != sweepSkipHub {
		t.Fatalf("line %+v", line)
	}
	requireExists(t, kept, true)
	// A host whose entries name no repository sweeps nothing and posts nothing.
	f.deps.hub = realHub
	f.setEntries()
	f.free = 1
	f.now = f.now.Add(time.Hour)
	if !f.attempt() || f.sweeps != 0 {
		t.Fatalf("no repository: %d sweeps", f.sweeps)
	}
	if line := lastSweepLine(t); line.Outcome != "swept" || line.Repositories != 0 || line.FreeBytesAfter != -1 || line.LowSpace {
		t.Fatalf("no repository line %+v", line)
	}
	requireExists(t, kept, true)
}

// fakeMatrixOwner is one pid in the fake process table: gone unless alive,
// with a start time when known.
type fakeMatrixOwner struct {
	alive   bool
	started time.Time
	known   bool
}

// fakeMatrixOwners replaces the mutex owner probe with a table, so no test
// signals a real process. A pid outside the table is alive with an unknown
// start, which is never reclaimed.
func fakeMatrixOwners(t *testing.T, table map[int]fakeMatrixOwner) {
	t.Helper()
	old := matrixOwnerProbe
	matrixOwnerProbe = func(pid int) (bool, time.Time, bool) {
		owner, ok := table[pid]
		if !ok {
			return true, time.Time{}, false
		}
		return owner.alive, owner.started, owner.known
	}
	t.Cleanup(func() { matrixOwnerProbe = old })
}

// The owner probe outside a fixture refuses before signalling anything,
// newSweepFixture answers from a fake table, the opt-in installs the native
// probe only for its own test, and no test file reaches the native probe or
// replaces the owner probe except through these helpers.
func TestSweepMatrixOwnerProbeGuard(t *testing.T) {
	probe := func() uintptr { return reflect.ValueOf(matrixOwnerProbe).Pointer() }
	stray, native := reflect.ValueOf(strayMatrixOwnerProbe).Pointer(), reflect.ValueOf(nativeMatrixOwnerProbe).Pointer()
	dead := matrixOwner{PID: 2147483646, Token: "fixture", At: time.Now(), HasAt: true}
	if probe() != stray {
		t.Fatal("outside a fixture the owner probe is not the stray guard: a test left its probe behind")
	}
	func() {
		defer func() {
			if r := recover(); !strings.Contains(fmt.Sprint(r), "matrix mutex owner pid 2147483646 without a process table") {
				t.Errorf("an owner probe outside a fixture did not refuse: %v", r)
			}
		}()
		matrixOwnerDead(dead)
	}()
	t.Run("the fixture installs an empty table", func(t *testing.T) {
		newSweepFixture(t)
		if probe() == stray || probe() == native {
			t.Fatal("newSweepFixture left the owner probe unreplaced")
		}
		if why := matrixOwnerDead(dead); why != "" {
			t.Fatalf("an owner outside the fixture's table: %q", why)
		}
	})
	if probe() != stray {
		t.Fatal("the fixture did not restore the stray guard")
	}
	t.Run("the opt-in installs the native probe for its own test", func(t *testing.T) {
		nativeMatrixOwnerProbeFor(t, "the guard checks which probe is installed and calls none")
		if probe() != native {
			t.Fatal("the opt-in did not install the native probe")
		}
	})
	if probe() != stray {
		t.Fatal("the opt-in did not restore the stray guard")
	}
	// Source check: only the opt-in reaches the native probe, and only the
	// guard, the fake table and the opt-in replace the owner probe.
	files, err := filepath.Glob("*_test.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("test files: %v %v", files, err)
	}
	fset := token.NewFileSet()
	for _, file := range files {
		parsed, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range parsed.Decls {
			name := ""
			if fn, ok := decl.(*ast.FuncDecl); ok {
				name = fn.Name.Name
			}
			ast.Inspect(decl, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.Ident:
					if n.Name == "nativeMatrixOwnerProbe" && name != "nativeMatrixOwnerProbeFor" && name != "TestSweepMatrixOwnerProbeGuard" {
						t.Errorf("%s: %s reaches the native owner probe without nativeMatrixOwnerProbeFor", fset.Position(n.Pos()), name)
					}
				case *ast.AssignStmt:
					for _, lhs := range n.Lhs {
						if id, ok := lhs.(*ast.Ident); ok && id.Name == "matrixOwnerProbe" && name != "init" && name != "fakeMatrixOwners" && name != "nativeMatrixOwnerProbeFor" {
							t.Errorf("%s: %s replaces the owner probe outside fakeMatrixOwners and nativeMatrixOwnerProbeFor", fset.Position(n.Pos()), name)
						}
					}
				}
				return true
			})
		}
	}
}

// notRepository adds a queue entry whose repository directory exists but is
// not a Git repository, and returns that directory.
func (f *sweepFixture) notRepository(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(f.r.root, name)
	writeFixtureFile(t, filepath.Join(dir, "README"), "no longer a repository\n")
	entry := releasedArtifactEntry(sweepItem)
	entry.ID, entry.TaskID, entry.Host, entry.Repository = "tqe_"+name, sweepTask, sweepHost, dir
	f.setEntries(append(f.entries(), entry)...)
	return dir
}

// a1, a2 (k1): one failing repository is journalled and skipped while the
// others still make a swept run; a run in which every repository fails is
// spaced like a swept run; both still raise the low-space notice.
func TestSweepScheduleFailingRepository(t *testing.T) {
	t.Run("one of two repositories fails", func(t *testing.T) {
		f := newSweepFixture(t)
		f.setAgents(sweepTask, f.helper("agt_5eeb5eeb5eeb5e0a", "owner-helper", sweepTask, time.Now().Add(-time.Hour)))
		gone := f.stale(t, "queue-5eeb00c1")
		bad := f.notRepository(t, "not-a-repo")
		var failures []string
		f.deps.sweep = func(ctx context.Context, state sweepHubState, host string, o sweepOptions) ([]worktreeDecision, error) {
			f.sweeps++
			decisions, err := runWorktreeSweep(ctx, state, host, o)
			if err != nil {
				failures = append(failures, o.Repo+": "+err.Error())
			}
			return decisions, err
		}
		f.free = 5 << 30
		if !f.attempt() || f.sweeps != 2 {
			t.Fatalf("attempted with %d sweeps", f.sweeps)
		}
		if len(failures) != 1 || !strings.HasPrefix(failures[0], bad+": ") {
			t.Fatalf("the real sweep of a directory that is not a repository: %v", failures)
		}
		lines := sweepJournalLines(t)
		line := lines[len(lines)-1]
		if len(lines) != 1 || line.Outcome != "swept" || line.Repositories != 2 || line.RepositoriesFailed != 1 || line.TreesRemoved != 1 ||
			!strings.Contains(line.Detail, "1 repository failed and was skipped: "+failures[0][:len(bad)+10]) || !line.LowSpace {
			t.Fatalf("lines %+v after %v", lines, failures)
		}
		requireExists(t, gone, false)
		state, err := loadSweepState()
		if err != nil || !state.LastRunAt.Equal(f.now) || state.Episode == nil {
			t.Fatalf("state %+v: %v", state, err)
		}
		stored, attempts, _, _ := f.hub.messages()
		if len(stored) != 1 || len(attempts) != 1 || line.NoticeSeq != stored[0].Seq || !strings.Contains(attempts[0].Envelope.Body.Text, "1 of 2 repositories could not be swept and were skipped.") {
			t.Fatalf("notice: %d stored, %+v, line %+v", len(stored), attempts, line)
		}
		f.now = f.now.Add(6 * time.Minute)
		if f.attempt() || f.sweeps != 2 || len(sweepJournalLines(t)) != 1 {
			t.Fatalf("retried six minutes after a swept run: %d sweeps", f.sweeps)
		}
		out, err := captureSweepStdout(t, func() error { return cmdTeamQueueSweepWorktrees(env{}, []string{"--journal"}) })
		if err != nil || !strings.Contains(out, "; 1 repository failed and was skipped: "+bad) {
			t.Fatalf("--journal: %v\n%s", err, out)
		}
	})
	t.Run("every repository fails", func(t *testing.T) {
		f := newSweepFixture(t)
		f.setAgents(sweepTask, f.helper("agt_5eeb5eeb5eeb5e0a", "owner-helper", sweepTask, time.Now().Add(-time.Hour)))
		kept := f.stale(t, "queue-5eeb00c2")
		f.notRepository(t, "not-a-repo-1")
		for i := 2; i <= 5; i++ {
			f.notRepository(t, fmt.Sprintf("not-a-repo-%d", i))
		}
		long := strings.Repeat("x", 300)
		f.deps.sweep = func(_ context.Context, _ sweepHubState, _ string, o sweepOptions) ([]worktreeDecision, error) {
			f.sweeps++
			return nil, fmt.Errorf("cannot sweep %s", long)
		}
		f.free = 5 << 30
		start := f.now
		if !f.attempt() || f.sweeps != 6 {
			t.Fatalf("attempted with %d sweeps", f.sweeps)
		}
		lines := sweepJournalLines(t)
		line := lines[len(lines)-1]
		if len(lines) != 1 || line.Outcome != "failed" || line.Reason != "sweep" || line.Repositories != 6 || line.RepositoriesFailed != 6 || !line.LowSpace ||
			!strings.HasPrefix(line.Detail, "6 repositories failed and were skipped: ") || !strings.HasSuffix(line.Detail, "; and 3 more") || strings.Contains(line.Detail, strings.Repeat("x", 201)) {
			t.Fatalf("lines %+v", lines)
		}
		requireExists(t, kept, true)
		state, err := loadSweepState()
		if err != nil || !state.LastRunAt.Equal(f.now) || state.Episode == nil {
			t.Fatalf("state %+v: %v", state, err)
		}
		stored, attempts, _, _ := f.hub.messages()
		if len(stored) != 1 || len(attempts) != 1 || !strings.Contains(attempts[0].Envelope.Body.Text, "6 of 6 repositories could not be swept and were skipped.") || len(attempts[0].Envelope.Body.Text) > api.MaxEnvelopeBodyBytes {
			t.Fatalf("notice: %d stored, %+v", len(stored), attempts)
		}
		f.now = start.Add(6 * time.Minute)
		if f.attempt() || f.sweeps != 6 {
			t.Fatalf("retried six minutes after every repository failed: %d sweeps", f.sweeps)
		}
		f.now = start.Add(time.Hour)
		if !f.attempt() || f.sweeps != 12 || len(sweepJournalLines(t)) != 2 {
			t.Fatalf("not run at the hour: %d sweeps, %d lines", f.sweeps, len(sweepJournalLines(t)))
		}
		if stored, attempts, _, _ := f.hub.messages(); len(stored) != 1 || len(attempts) != 1 {
			t.Fatalf("a second notice in one episode: %d stored, %d posts", len(stored), len(attempts))
		}
	})
}

// j1, j3: a run that removes one worktree and three matrix test binaries
// journals treesRemoved=1 and binariesRemoved=3, and frees the bytes of all
// four; a journal line written before binariesRemoved existed still decodes
// and prints binaries=0.
func TestSweepScheduleCountsBinariesApartFromTrees(t *testing.T) {
	t.Run("one tree and three binaries", func(t *testing.T) {
		f := newSweepFixture(t)
		f.setAgents(sweepTask, f.helper("agt_5eeb5eeb5eeb5e0b", "owner-helper", sweepTask, time.Now().Add(-time.Hour)))
		gone := f.stale(t, "queue-b1a2b3c4")
		var treeBytes int64
		f.deps.sweep = func(ctx context.Context, state sweepHubState, host string, o sweepOptions) ([]worktreeDecision, error) {
			decisions, err := runWorktreeSweep(ctx, state, host, o)
			for _, d := range decisions {
				if d.Action == "removed" {
					treeBytes += d.Bytes
				}
			}
			for i := 1; i <= 3; i++ {
				decisions = append(decisions, worktreeDecision{Path: filepath.Join(f.r.root, fmt.Sprintf("tt.test-%d", i)), Kind: kindMatrixBinary, Action: "removed", Reason: artifactItemReleased, Bytes: 1000})
			}
			return decisions, err
		}
		f.free = 5 << 30
		if !f.attempt() {
			t.Fatal("no attempt")
		}
		requireExists(t, gone, false)
		line := lastSweepLine(t)
		if line.Outcome != "swept" || line.TreesRemoved != 1 || line.BinariesRemoved != 3 || line.FoldersRemoved != 0 || treeBytes <= 0 || line.BytesFreed != treeBytes+3000 {
			t.Fatalf("line %+v; tree bytes %d", line, treeBytes)
		}
		data, err := os.ReadFile(sweepJournalPath())
		if err != nil || !strings.Contains(string(data), `"treesRemoved":1,`) || !strings.Contains(string(data), `"binariesRemoved":3,`) {
			t.Fatalf("raw journal: %v\n%s", err, data)
		}
		out, err := captureSweepStdout(t, func() error { return cmdTeamQueueSweepWorktrees(env{}, []string{"--journal"}) })
		if err != nil || !strings.Contains(out, line.At+" swept repositories=1 trees=1 folders=0 binaries=3 caches=0 pruned=0 freed=") {
			t.Fatalf("--journal: %v\n%s", err, out)
		}
		_, attempts, _, _ := f.hub.messages()
		if len(attempts) != 1 || !strings.Contains(attempts[0].Envelope.Body.Text, "1 worktrees, 0 session temp folders, 3 matrix test binaries, 0 caches trimmed") {
			t.Fatalf("notice: %+v", attempts)
		}
	})
	t.Run("lines without binariesRemoved", func(t *testing.T) {
		newSweepFixture(t)
		old := `{"at":"2026-10-01T00:00:00Z","source":"schedule","outcome":"swept","reason":"","repositories":1,"treesRemoved":4,"foldersRemoved":1,"cachesTrimmed":0,"pruned":0,"bytesFreed":4096,"freeBytesAfter":-1,"kept":{},"minIdle":"6h0m0s","durationMs":10,"exclusionExceeded":0,"lowSpace":false,"noticeSeq":0,"noticePending":""}`
		cut := `{"at":"2026-10-01T01:00:00Z","source":"schedule","outcome":"skipped","reason":"matrix-lock","repositories":1,"treesRemoved":1,"foldersRemoved":0,"binariesRemoved":2,"cachesTrimmed":0,"pruned":0,"bytesFreed":2048,"freeBytesAfter":-1,"kept":{},"minIdle":"6h0m0s","durationMs":10,"exclusionExceeded":0,"lowSpace":false,"noticeSeq":0,"noticePending":""}`
		if err := os.MkdirAll(filepath.Dir(sweepJournalPath()), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(sweepJournalPath(), []byte(old+"\n"+cut+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		lines := sweepJournalLines(t)
		if len(lines) != 2 || lines[0].TreesRemoved != 4 || lines[0].BinariesRemoved != 0 || lines[1].TreesRemoved != 1 || lines[1].BinariesRemoved != 2 {
			t.Fatalf("decoded %+v", lines)
		}
		out, err := captureSweepStdout(t, func() error { return cmdTeamQueueSweepWorktrees(env{}, []string{"--journal"}) })
		if err != nil || !strings.Contains(out, "2026-10-01T00:00:00Z swept repositories=1 trees=4 folders=1 binaries=0 caches=0 pruned=0 freed=") ||
			!strings.Contains(out, "2026-10-01T01:00:00Z skipped matrix-lock; trees=1 folders=0 binaries=2 caches=0 pruned=0 freed=") {
			t.Fatalf("--journal: %v\n%s", err, out)
		}
	})
}

// a3 (k2): a state file that cannot be read, or cannot be saved, is
// journalled once per episode and tried every five minutes, not every check.
func TestSweepScheduleStateFaultSpacing(t *testing.T) {
	failedState := func(t *testing.T) int {
		n := 0
		for _, line := range sweepJournalLines(t) {
			if line.Outcome == "failed" && line.Reason == "state" {
				n++
			}
		}
		return n
	}
	// checks runs the relay's 30 s checks over 20 minutes of injected time.
	checks := func(f *sweepFixture) (attempts int) {
		for i := 0; i < 40; i++ {
			if f.attempt() {
				attempts++
			}
			f.now = f.now.Add(sweepCheckInterval)
		}
		return attempts
	}
	t.Run("unreadable", func(t *testing.T) {
		f := newSweepFixture(t)
		gone := f.stale(t, "queue-5eeb00d1")
		writeFixtureFile(t, sweepStatePath(), "{not json")
		if got := checks(f); got != 1 || failedState(t) != 1 || len(sweepJournalLines(t)) != 1 || f.sweeps != 0 {
			t.Fatalf("corrupt state: %d attempts, %d lines, %d sweeps", got, len(sweepJournalLines(t)), f.sweeps)
		}
		if line := lastSweepLine(t); !strings.Contains(line.Detail, "not readable JSON") {
			t.Fatalf("line %+v", line)
		}
		requireExists(t, gone, true)
		// Repaired: the next try sweeps.
		if err := os.Remove(sweepStatePath()); err != nil {
			t.Fatal(err)
		}
		f.now = f.now.Add(sweepRetryAfter)
		if !f.attempt() || f.sweeps != 1 || lastSweepLine(t).Outcome != "swept" {
			t.Fatalf("after the repair: %d sweeps, %+v", f.sweeps, lastSweepLine(t))
		}
		requireExists(t, gone, false)
		// Corrupt again: a new episode and one new line.
		writeFixtureFile(t, sweepStatePath(), "{not json")
		if got := checks(f); got != 1 || failedState(t) != 2 || len(sweepJournalLines(t)) != 3 || f.sweeps != 1 {
			t.Fatalf("corrupt again: %d attempts, %d lines, %d sweeps", got, len(sweepJournalLines(t)), f.sweeps)
		}
	})
	t.Run("unsaveable", func(t *testing.T) {
		if os.Getuid() == 0 {
			t.Skip("root writes to a read-only directory")
		}
		f := newSweepFixture(t)
		if !f.attempt() {
			t.Fatal("no first attempt")
		}
		// From here the state file reads but cannot be replaced.
		if err := os.Chmod(relayDir(), 0500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(relayDir(), 0700) })
		f.now = f.now.Add(time.Hour)
		if got := checks(f); got != 1 || failedState(t) != 1 || len(sweepJournalLines(t)) != 2 {
			t.Fatalf("unsaveable state: %d attempts, lines %+v", got, sweepJournalLines(t))
		}
		// The off switch cannot be recorded either: still the same episode.
		off := defaultSweepSettings()
		off.Off = true
		on := f.deps.settings
		f.deps.settings = func() (sweepSettings, string) { return off, "" }
		if got := checks(f); got != 0 || len(sweepJournalLines(t)) != 2 {
			t.Fatalf("off with an unsaveable state: %d attempts, lines %+v", got, sweepJournalLines(t))
		}
		f.deps.settings = on
		if err := os.Chmod(relayDir(), 0700); err != nil {
			t.Fatal(err)
		}
		f.now = f.now.Add(sweepRetryAfter)
		if !f.attempt() || lastSweepLine(t).Outcome != "swept" {
			t.Fatalf("after the repair: %+v", lastSweepLine(t))
		}
		if state, err := loadSweepState(); err != nil || !state.LastRunAt.Equal(f.now) {
			t.Fatalf("state %+v: %v", state, err)
		}
	})
}

// countingReaderAt counts the bytes read through it.
type countingReaderAt struct {
	file  io.ReaderAt
	bytes int64
}

func (c *countingReaderAt) ReadAt(p []byte, off int64) (int, error) {
	n, err := c.file.ReadAt(p, off)
	c.bytes += int64(n)
	return n, err
}

// a4 (k2): the journal is bounded by size, keeps whole lines and the newest
// line, loses no concurrent writer's line, and is read from its tail.
func TestSweepScheduleJournalBound(t *testing.T) {
	root := t.TempDir()
	isolatedHome(t, root)
	t.Setenv("TAILTERM_RELAY_STATE", filepath.Join(root, "relay"))
	numbered := func(n int) sweepJournalLine {
		return sweepJournalLine{At: "2026-10-07T12:00:00Z", Source: "schedule", Outcome: "skipped", Reason: sweepSkipHub, Detail: fmt.Sprintf("line-%07d %s", n, strings.Repeat("p", 180)), Kept: map[string]int{}}
	}
	// fill writes numbered lines straight to the file until it holds at
	// least size bytes, and returns the next number.
	fill := func(size int64) int {
		t.Helper()
		var data []byte
		n := 0
		for ; int64(len(data)) < size; n++ {
			raw, err := json.Marshal(numbered(n))
			if err != nil {
				t.Fatal(err)
			}
			data = append(append(data, raw...), '\n')
		}
		if err := os.MkdirAll(relayDir(), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(sweepJournalPath(), data, 0600); err != nil {
			t.Fatal(err)
		}
		return n
	}
	rawLines := func() []string {
		t.Helper()
		data, err := os.ReadFile(sweepJournalPath())
		if err != nil {
			t.Fatal(err)
		}
		if len(data) == 0 || data[len(data)-1] != '\n' {
			t.Fatalf("the journal does not end on a whole line (%d bytes)", len(data))
		}
		lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
		for i, text := range lines {
			var line sweepJournalLine
			if err := json.Unmarshal([]byte(text), &line); err != nil || line.Source != "schedule" {
				t.Fatalf("line %d of %d is not a whole journal line: %v: %.80q", i, len(lines), err, text)
			}
		}
		return lines
	}
	const lineBytes = 512

	// Appends past the bound rewrite the file to its newest whole lines.
	next := fill(sweepJournalMaxBytes - 3*lineBytes)
	trimmed := false
	for i := 0; i < 12; i++ {
		before, err := os.Stat(sweepJournalPath())
		if err != nil {
			t.Fatal(err)
		}
		if err := appendSweepJournal(numbered(next)); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(sweepJournalPath())
		if err != nil {
			t.Fatal(err)
		}
		if info.Size() > sweepJournalMaxBytes+lineBytes || info.Mode().Perm() != 0600 {
			t.Fatalf("after append %d the journal is %d bytes, mode %v", i, info.Size(), info.Mode())
		}
		if info.Size() < before.Size() {
			trimmed = true
			if info.Size() > sweepJournalKeepBytes+lineBytes || info.Size() < sweepJournalKeepBytes/2 {
				t.Fatalf("the rewrite kept %d bytes", info.Size())
			}
		}
		lines := rawLines()
		if !strings.Contains(lines[len(lines)-1], fmt.Sprintf("line-%07d ", next)) {
			t.Fatalf("append %d: the newest line is not last: %.60q", i, lines[len(lines)-1])
		}
		// Numbered lines stay in order with none missing from the kept tail.
		first := 0
		if _, err := fmt.Sscanf(lines[0][strings.Index(lines[0], "line-"):], "line-%d", &first); err != nil || first+len(lines)-1 != next {
			t.Fatalf("append %d: %d lines from %d do not end at %d: %v", i, len(lines), first, next, err)
		}
		next++
	}
	if !trimmed {
		t.Fatal("the journal was never rewritten")
	}
	if leftovers, _ := filepath.Glob(filepath.Join(relayDir(), ".worktree-sweep-journal-*")); len(leftovers) != 0 {
		t.Fatalf("the rewrite left %v", leftovers)
	}

	// The reader takes the last lines from the tail only.
	last := fill(sweepJournalMaxBytes) - 1
	file, err := os.Open(sweepJournalPath())
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	counted := &countingReaderAt{file: file}
	tail, err := readSweepJournalTail(counted, info.Size(), 20)
	if err != nil || len(tail) != 20 || counted.bytes >= 1<<20 {
		t.Fatalf("tail read: %d lines, %d bytes read of %d: %v", len(tail), counted.bytes, info.Size(), err)
	}
	for i, line := range tail {
		if !strings.HasPrefix(line.Detail, fmt.Sprintf("line-%07d ", last-19+i)) {
			t.Fatalf("tail line %d is %.20q, want line %d", i, line.Detail, last-19+i)
		}
	}
	if viaPath, err := readSweepJournal(20); err != nil || !reflect.DeepEqual(viaPath, tail) {
		t.Fatalf("readSweepJournal differs from the tail read: %v", err)
	}
	// A limit beyond the file returns every line; unreadable lines are passed
	// over without shortening the answer.
	if err := os.WriteFile(sweepJournalPath(), nil, 0600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if err := appendSweepJournal(numbered(i)); err != nil {
			t.Fatal(err)
		}
	}
	junk, err := os.OpenFile(sweepJournalPath(), os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := junk.WriteString(strings.Repeat("not json\n", 3*sweepJournalChunk/9)); err != nil {
		t.Fatal(err)
	}
	junk.Close()
	if lines, err := readSweepJournal(1000); err != nil || len(lines) != 5 || !strings.HasPrefix(lines[4].Detail, "line-0000004 ") {
		t.Fatalf("a limit beyond the file: %d lines: %v", len(lines), err)
	}
	if lines, err := readSweepJournal(2); err != nil || len(lines) != 2 || !strings.HasPrefix(lines[0].Detail, "line-0000003 ") {
		t.Fatalf("two lines behind unreadable ones: %d lines: %v", len(lines), err)
	}

	// R4: writers appending across the bound lose no line to a rewrite.
	fill(sweepJournalMaxBytes - 40*lineBytes)
	const writers, each = 20, 30
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				line := numbered(0)
				line.Detail = fmt.Sprintf("writer-%02d-%03d %s", w, i, strings.Repeat("p", 180))
				if err := appendSweepJournal(line); err != nil {
					t.Error(err)
				}
			}
		}(w)
	}
	wg.Wait()
	all := strings.Join(rawLines(), "\n")
	for w := 0; w < writers; w++ {
		for i := 0; i < each; i++ {
			if !strings.Contains(all, fmt.Sprintf("writer-%02d-%03d ", w, i)) {
				t.Fatalf("writer %d's line %d was lost to a rewrite", w, i)
			}
		}
	}
	if info, err := os.Stat(sweepJournalPath()); err != nil || info.Size() > sweepJournalKeepBytes+writers*each*lineBytes {
		t.Fatalf("the journal was not rewritten under concurrent appends: %v %v", info, err)
	}
}

// a5 (k3): a mutex whose owner is gone, or whose pid now belongs to a later
// process, is reclaimed under the script's guard protocol; any other owner
// keeps it, and a run that kept paths only for the mutex is skipped.
func TestSweepScheduleMatrixReclaim(t *testing.T) {
	at := time.Date(2026, 10, 7, 10, 0, 0, 0, time.UTC)
	const ownerPID, guardPID = 4242, 4343
	record := `{"pid":4242,"token":"left-behind","at":"2026-10-07T10:00:00.000Z"}` + "\n"
	guardRecord := `{"pid":4343,"token":"guard-left-behind","at":"2026-10-07T10:00:00.000Z"}` + "\n"
	cases := []struct {
		name    string
		owner   fakeMatrixOwner
		mutex   string
		guard   string
		reclaim string
		busy    string
	}{
		{name: "the owner pid is gone", mutex: record, reclaim: "pid 4242 is gone"},
		{name: "the pid belongs to a process started an hour after the record", owner: fakeMatrixOwner{alive: true, started: at.Add(time.Hour), known: true}, mutex: record,
			reclaim: "pid 4242 now belongs to a process started after the record was written"},
		{name: "the owner is alive and started before the record", owner: fakeMatrixOwner{alive: true, started: at.Add(-time.Hour), known: true}, mutex: record, busy: "owner pid 4242"},
		{name: "the owner started exactly at the margin", owner: fakeMatrixOwner{alive: true, started: at.Add(matrixOwnerStartMargin), known: true}, mutex: record, busy: "owner pid 4242"},
		{name: "the owner is alive with an unknown start", owner: fakeMatrixOwner{alive: true}, mutex: record, busy: "owner pid 4242"},
		{name: "the record's time does not parse", owner: fakeMatrixOwner{alive: true, started: at.Add(time.Hour), known: true},
			mutex: `{"pid":4242,"token":"left-behind","at":"yesterday"}` + "\n", busy: "owner pid 4242"},
		{name: "the owner record is unparseable", mutex: "{not an owner\n", busy: "an owner record that names no owner"},
		{name: "the owner record has no token", mutex: `{"pid":4242}` + "\n", busy: "an owner record that names no owner"},
		{name: "a reclaim guard was left by a gone pid", mutex: record, guard: guardRecord, busy: "reclaim guard left by pid 4343"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newSweepFixture(t)
			f.setAgents(sweepTask, f.helper("agt_5eeb5eeb5eeb5e0a", "owner-helper", sweepTask, time.Now().Add(-time.Hour)))
			shortenMatrixTimes(t, 60*time.Millisecond, matrixHoldRelease)
			fakeMatrixOwners(t, map[int]fakeMatrixOwner{ownerPID: c.owner, guardPID: {}})
			path := f.stale(t, "queue-5eeb00e1")
			mutex := os.Getenv("TAILTERM_MATRIX_HOST_LOCK") + ".lock"
			guard := mutex + ".reclaim"
			writeFixtureFile(t, mutex, c.mutex)
			if c.guard != "" {
				writeFixtureFile(t, guard, c.guard)
			}
			f.free = 5 << 30
			start := f.now
			if !f.attempt() {
				t.Fatal("no attempt")
			}
			lines := sweepJournalLines(t)
			line := lines[len(lines)-1]
			stored, attempts, _, _ := f.hub.messages()
			if c.reclaim != "" {
				if len(lines) != 1 || line.Outcome != "swept" || line.TreesRemoved != 1 || line.Detail != "reclaimed the matrix mutex left by pid 4242 ("+c.reclaim+")" {
					t.Fatalf("reclaim lines %+v", lines)
				}
				requireExists(t, path, false)
				requireExists(t, mutex, false)
				requireExists(t, guard, false)
				if leftovers, _ := filepath.Glob(mutex + "*"); len(leftovers) != 0 {
					t.Fatalf("the reclaim left %v", leftovers)
				}
				if len(stored) != 1 || strings.Contains(attempts[0].Envelope.Body.Text, "cut short") {
					t.Fatalf("notice after a reclaim: %d stored, %+v", len(stored), attempts)
				}
				return
			}
			if len(lines) != 1 || line.Outcome != "skipped" || line.Reason != sweepSkipMatrix || line.TreesRemoved != 0 || line.Kept[keepInUse] != 1 ||
				line.Detail != "1 path(s) kept: matrix mutex busy ("+c.busy+")" || !line.LowSpace {
				t.Fatalf("busy lines %+v", lines)
			}
			requireExists(t, path, true)
			if data, err := os.ReadFile(mutex); err != nil || string(data) != c.mutex {
				t.Fatalf("the mutex changed: %q %v", data, err)
			}
			if c.guard == "" {
				requireExists(t, guard, false)
			} else if data, err := os.ReadFile(guard); err != nil || string(data) != c.guard {
				t.Fatalf("the guard changed: %q %v", data, err)
			}
			// The low-space notice is still raised, once.
			if len(stored) != 1 || len(attempts) != 1 || !strings.Contains(attempts[0].Envelope.Body.Text, "The run was cut short by the matrix lock.") {
				t.Fatalf("notice from a skipped run: %d stored, %+v", len(stored), attempts)
			}
			state, err := loadSweepState()
			if err != nil || !state.LastRunAt.IsZero() || !state.LastAttemptAt.Equal(start) || state.Episode == nil {
				t.Fatalf("state %+v: %v", state, err)
			}
			// Tried again after five minutes, not before.
			f.now = start.Add(4 * time.Minute)
			if f.attempt() {
				t.Fatal("retried four minutes after a busy mutex")
			}
			f.now = start.Add(6 * time.Minute)
			if !f.attempt() || f.sweeps != 2 || lastSweepLine(t).Reason != sweepSkipMatrix {
				t.Fatalf("not retried six minutes after a busy mutex: %d sweeps, %+v", f.sweeps, lastSweepLine(t))
			}
			if stored, _, _, _ := f.hub.messages(); len(stored) != 1 {
				t.Fatalf("%d notices in one episode", len(stored))
			}
		})
	}
	// R2: a start time ps prints in another form is unknown, so never a
	// reason to reclaim.
	for text, want := range map[string]time.Time{
		"Tue Oct 13 17:20:01 2026\n":  time.Date(2026, 10, 13, 17, 20, 1, 0, time.UTC),
		"Thu Oct  1 09:05:03 2026   ": time.Date(2026, 10, 1, 9, 5, 3, 0, time.UTC),
	} {
		if got, ok := parsePsLstart(text); !ok || !got.Equal(want) {
			t.Fatalf("parsePsLstart(%q) = %v %v", text, got, ok)
		}
	}
	for _, text := range []string{"", "mar. 13 oct. 17:20:01 2026", "2026-10-13T17:20:01Z", "17:20"} {
		if got, ok := parsePsLstart(text); ok {
			t.Fatalf("parsePsLstart(%q) = %v", text, got)
		}
	}
}

// a8 (k6): low space on the volume of the Go build cache the trim manages
// opens an episode, and the one notice names that volume.
func TestSweepScheduleLowSpaceCacheVolume(t *testing.T) {
	f := newSweepFixture(t)
	f.setAgents(sweepTask, f.helper("agt_5eeb5eeb5eeb5e0a", "owner-helper", sweepTask, time.Now().Add(-time.Hour)))
	cache := filepath.Join(f.r.root, "other-volume", "go-build")
	writeFixtureFile(t, filepath.Join(cache, "README"), "a cache on another volume\n")
	var read []string
	f.deps.freeBytes = func(path string) (int64, error) {
		read = append(read, path)
		if path == cache {
			return 2 << 30, nil
		}
		return 100 << 30, nil
	}
	// With no cache recorded the same readings open no episode.
	if !f.attempt() {
		t.Fatal("no attempt")
	}
	if line := lastSweepLine(t); line.LowSpace || line.FreeBytesAfter != 100<<30 || line.LowestVolume != f.r.main || strings.Contains(strings.Join(read, "\n"), cache) {
		t.Fatalf("no cache recorded: %+v, read %v", line, read)
	}
	if state, _ := loadSweepState(); state.Episode != nil {
		t.Fatal("an episode opened with no cache recorded")
	}
	if err := updateSweepState(func(s *sweepState) error {
		s.GoCacheTrim = &goCacheTrimState{LastRunAt: f.now.UTC(), LastAttemptAt: f.now.UTC(), MaxAge: "24h0m0s", CacheDir: cache}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(time.Hour)
	if !f.attempt() {
		t.Fatal("no attempt")
	}
	line := lastSweepLine(t)
	stored, attempts, _, _ := f.hub.messages()
	if !line.LowSpace || line.FreeBytesAfter != 2<<30 || line.LowestVolume != cache || len(stored) != 1 || line.NoticeSeq != stored[0].Seq {
		t.Fatalf("low cache volume: %+v, %d stored", line, len(stored))
	}
	if text := attempts[0].Envelope.Body.Text; !strings.Contains(text, "has 2.0 GiB free on the volume holding "+cache+" after the scheduled worktree sweep") {
		t.Fatalf("notice does not name the cache volume:\n%s", text)
	}
	if state, _ := loadSweepState(); state.Episode == nil {
		t.Fatal("no episode")
	}
	// Still low: the same episode, nothing more posted.
	f.now = f.now.Add(time.Hour)
	if !f.attempt() || !lastSweepLine(t).LowSpace {
		t.Fatalf("second low run: %+v", lastSweepLine(t))
	}
	if stored, attempts, _, _ := f.hub.messages(); len(stored) != 1 || len(attempts) != 1 {
		t.Fatalf("second low run: %d stored, %d posts", len(stored), len(attempts))
	}
	// A recorded cache that no longer exists is not read.
	if err := os.RemoveAll(cache); err != nil {
		t.Fatal(err)
	}
	read = nil
	f.now = f.now.Add(time.Hour)
	if !f.attempt() || lastSweepLine(t).LowSpace || strings.Contains(strings.Join(read, "\n"), cache) {
		t.Fatalf("a missing cache directory: %+v, read %v", lastSweepLine(t), read)
	}
	// A cache the sweep itself decided on is read where it lies.
	writeFixtureFile(t, filepath.Join(cache, "README"), "a worktree cache on another volume\n")
	f.deps.sweep = func(ctx context.Context, state sweepHubState, host string, o sweepOptions) ([]worktreeDecision, error) {
		decisions, err := runWorktreeSweep(ctx, state, host, o)
		return append(decisions, worktreeDecision{Path: cache, Kind: kindCache, Action: "kept", Reason: keepInUse}), err
	}
	if err := updateSweepState(func(s *sweepState) error {
		s.GoCacheTrim = nil
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(time.Hour)
	if !f.attempt() {
		t.Fatal("no attempt")
	}
	if line := lastSweepLine(t); !line.LowSpace || line.LowestVolume != cache || line.NoticeSeq == 0 {
		t.Fatalf("a swept cache on a low volume: %+v", line)
	}
	if stored, _, _, _ := f.hub.messages(); len(stored) != 2 {
		t.Fatalf("%d notices for two episodes", len(stored))
	}
}
