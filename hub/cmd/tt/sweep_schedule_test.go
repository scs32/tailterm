package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
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

func newSweepFixture(t *testing.T) *sweepFixture {
	t.Helper()
	r := newCleanupRepo(t)
	r.ignoreBuildOutput(t)
	f := &sweepFixture{r: r, home: isolatedHome(t, r.root), host: sweepHost, now: time.Now().Add(48 * time.Hour), free: 100 << 30}
	oldHost := matrixWaitHostname
	matrixWaitHostname = func() (string, error) { return sweepHost, nil }
	t.Cleanup(func() { matrixWaitHostname = oldHost })
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
// the hold keeps its path; a busy mutex keeps the path and is never
// reclaimed; a removal that outruns the limit releases the mutex and is
// journalled; no hold reaches the limit.
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
	t.Run("a busy mutex keeps the path and is not reclaimed", func(t *testing.T) {
		shortenMatrixTimes(t, 60*time.Millisecond, matrixHoldRelease)
		kept := f.stale(t, "queue-5eeb0063")
		// Left by a process that is gone: only the script may reclaim it.
		stale := `{"pid":2147483646,"token":"left-by-a-dead-sweep","at":"2026-10-07T10:00:00.000Z"}` + "\n"
		writeFixtureFile(t, mutex, stale)
		f.now = f.now.Add(time.Hour)
		if !f.attempt() {
			t.Fatal("no attempt")
		}
		line := lastSweepLine(t)
		if line.Outcome != "swept" || line.TreesRemoved != 0 || line.Kept[keepInUse] != 1 {
			t.Fatalf("busy line %+v", line)
		}
		if len(f.last) != 1 || !strings.Contains(f.last[0].Detail, "matrix lock busy") {
			t.Fatalf("busy decisions %+v", f.last)
		}
		requireExists(t, kept, true)
		if data, err := os.ReadFile(mutex); err != nil || string(data) != stale {
			t.Fatalf("the sweep touched another owner's mutex: %q %v", data, err)
		}
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
	if err != nil || !strings.Contains(out, swept.At+" swept repositories=1 trees=1 folders=1 caches=0 pruned=0 freed=") || !strings.Contains(out, "free=100.0 GiB min-idle=6h0m0s") || !strings.Contains(out, lines[1].At+" skipped cleanup-active") {
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
	for _, want := range []string{"Host " + sweepHost + " has 5.0 GiB free", "below the 8.0 GiB threshold", "This run freed ", "1 worktrees, 0 session temp folders, 0 caches trimmed",
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
