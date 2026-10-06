package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

// No test in this package reads the host's live matrix lock: without an
// explicit TAILTERM_MATRIX_HOST_LOCK (TestMain clears it) there is no file.
func init() {
	matrixWaitDefaultPath = func() string { return "" }
}

const matrixTestItem = "wi_0123456789abcdef"

// matrixLockFixture points the reader at a private lock file on host
// test-mini whose only live pids are those given.
func matrixLockFixture(t *testing.T, alive ...int) (write func(content string)) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "host.json")
	t.Setenv("TAILTERM_MATRIX_HOST_LOCK", path)
	oldHost, oldAlive := matrixWaitHostname, matrixWaitPIDAlive
	t.Cleanup(func() { matrixWaitHostname, matrixWaitPIDAlive = oldHost, oldAlive })
	matrixWaitHostname = func() (string, error) { return "Test-Mini.local", nil }
	matrixWaitPIDAlive = func(pid int) bool {
		for _, p := range alive {
			if p == pid {
				return true
			}
		}
		return false
	}
	return func(content string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func matrixLockJSON(host string, holder string, waiters ...string) string {
	if holder == "" {
		holder = "null"
	}
	return `{"version":1,"host":"` + host + `","holder":` + holder + `,"waiters":[` + strings.Join(waiters, ",") + `]}`
}

// matrixLockV2JSON is a version 2 lock as the script writes it: a list of
// holders under a holder limit and no single holder.
func matrixLockV2JSON(host string, holders []string, waiters ...string) string {
	return `{"version":2,"host":"` + host + `","requestSeq":9,"grantSeq":4,"holderLimit":2,"holders":[` + strings.Join(holders, ",") + `],"waiters":[` + strings.Join(waiters, ",") + `]}`
}

func matrixEntry(pid, item, agent, at string) string {
	return `{"pid":` + pid + `,"kind":"matrix","item":"` + item + `","agent":"` + agent + `","priority":"normal","requestedAt":"` + at + `","startedAt":"` + at + `","commit":"abc1234","outputDir":"/private/output"}`
}

func TestMatrixWaitFor(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	requested := "2026-10-02T11:40:00.000Z"
	since := time.Date(2026, 10, 2, 11, 40, 0, 0, time.UTC)
	lead := api.Agent{Name: "lead-a", WorkItem: &api.AgentWorkItemBinding{ItemID: matrixTestItem}}
	other := matrixEntry("11", "wi_fedcba9876543210", "lead-b", requested)
	mine := matrixEntry("22", matrixTestItem, "lead-a", requested)
	write := matrixLockFixture(t, 11, 22, 33)

	write(matrixLockJSON("test-mini", other, other, mine, other))
	if w := matrixWaitFor(lead, now); w == nil || *w != (api.MatrixWait{Role: api.MatrixWaitWaiting, Item: matrixTestItem, Position: 2, Length: 3, Since: since}) || !w.Valid() {
		t.Fatalf("named waiter: %+v", w)
	}
	write(matrixLockJSON("test-mini", mine, other))
	if w := matrixWaitFor(lead, now); w == nil || *w != (api.MatrixWait{Role: api.MatrixWaitRunning, Item: matrixTestItem, Since: since}) || !w.Valid() {
		t.Fatalf("named holder: %+v", w)
	}
	// An unbound agent and an entry without an item id match by name alone.
	write(matrixLockJSON("test-mini", "", matrixEntry("22", "unknown", "lead-a", requested)))
	if w := matrixWaitFor(lead, now); w == nil || w.Item != "" || w.Position != 1 || w.Length != 1 {
		t.Fatalf("entry without an item: %+v", w)
	}
	write(matrixLockJSON("test-mini", "", mine))
	if w := matrixWaitFor(api.Agent{Name: "lead-a"}, now); w == nil || w.Item != matrixTestItem {
		t.Fatalf("unbound agent: %+v", w)
	}

	// Version 2: any of several holders is running, not only the first, and a
	// waiter keeps its place in the ordered list.
	write(matrixLockV2JSON("test-mini", []string{other, mine}, other))
	if w := matrixWaitFor(lead, now); w == nil || *w != (api.MatrixWait{Role: api.MatrixWaitRunning, Item: matrixTestItem, Since: since}) || !w.Valid() {
		t.Fatalf("version 2 second holder: %+v", w)
	}
	write(matrixLockV2JSON("test-mini", []string{mine, other}))
	if w := matrixWaitFor(lead, now); w == nil || w.Role != api.MatrixWaitRunning || w.Position != 0 || w.Length != 0 {
		t.Fatalf("version 2 first holder: %+v", w)
	}
	write(matrixLockV2JSON("Test-Mini.local", []string{other, other}, other, other, mine))
	if w := matrixWaitFor(lead, now); w == nil || *w != (api.MatrixWait{Role: api.MatrixWaitWaiting, Item: matrixTestItem, Position: 3, Length: 3, Since: since}) || !w.Valid() {
		t.Fatalf("version 2 waiter: %+v", w)
	}
	// Holding wins over a later place in the waitlist.
	write(matrixLockV2JSON("test-mini", []string{other, mine}, mine))
	if w := matrixWaitFor(lead, now); w == nil || w.Role != api.MatrixWaitRunning {
		t.Fatalf("version 2 holder that also waits: %+v", w)
	}

	for name, content := range map[string]string{
		"dead pid":       matrixLockJSON("test-mini", "", matrixEntry("44", matrixTestItem, "lead-a", requested)),
		"another agent":  matrixLockJSON("test-mini", other, other),
		"another item":   matrixLockJSON("test-mini", "", matrixEntry("22", "wi_fedcba9876543210", "lead-a", requested)),
		"another host":   matrixLockJSON("other-mini", mine, mine),
		"no host":        matrixLockJSON("", mine, mine),
		"not JSON":       "{broken",
		"wrong version":  strings.Replace(matrixLockJSON("test-mini", mine, mine), `"version":1`, `"version":3`, 1),
		"version 3 list": strings.Replace(matrixLockV2JSON("test-mini", []string{mine}, mine), `"version":2`, `"version":3`, 1),
		"no version":     strings.Replace(matrixLockJSON("test-mini", mine, mine), `"version":1,`, ``, 1),
		// Each version reads only its own holder field.
		"version 2 single holder": strings.Replace(matrixLockJSON("test-mini", mine), `"version":1`, `"version":2`, 1),
		"version 1 holder list":   strings.Replace(matrixLockV2JSON("test-mini", []string{mine}), `"version":2`, `"version":1`, 1),
		"version 2 dead holder":   matrixLockV2JSON("test-mini", []string{other, matrixEntry("44", matrixTestItem, "lead-a", requested)}),
		"version 2 another agent": matrixLockV2JSON("test-mini", []string{other, other}, other),
		"version 2 another item":  matrixLockV2JSON("test-mini", []string{matrixEntry("22", "wi_fedcba9876543210", "lead-a", requested)}),
		"version 2 another host":  matrixLockV2JSON("other-mini", []string{mine}, mine),
		"version 2 no host":       matrixLockV2JSON("", []string{mine}, mine),
		"version 2 too many":      matrixLockV2JSON("test-mini", nil, strings.Split(strings.Repeat(other+"\x00", 1000)+mine, "\x00")...),
		"unparsable time":         matrixLockJSON("test-mini", "", matrixEntry("22", matrixTestItem, "lead-a", "soon")),
		"too many waiters":        matrixLockJSON("test-mini", "", strings.Split(strings.Repeat(other+"\x00", 1000)+mine, "\x00")...),
	} {
		write(content)
		if w := matrixWaitFor(lead, now); w != nil {
			t.Fatalf("%s: %+v", name, w)
		}
	}
	write(matrixLockJSON("test-mini", mine, mine))
	if w := matrixWaitFor(api.Agent{WorkItem: lead.WorkItem}, now); w != nil {
		t.Fatalf("agent without a name: %+v", w)
	}

	path := os.Getenv("TAILTERM_MATRIX_HOST_LOCK")
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := os.ReadFile(path); err == nil {
		t.Log("running as a user that reads mode 0 files; unreadable case not exercised")
	} else if w := matrixWaitFor(lead, now); w != nil {
		t.Fatalf("unreadable file: %+v", w)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if w := matrixWaitFor(lead, now); w != nil {
		t.Fatalf("absent override file: %+v", w)
	}

	// A relative override is refused, not resolved against the directory.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "host.json"), []byte(matrixLockJSON("test-mini", mine)), 0600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	t.Setenv("TAILTERM_MATRIX_HOST_LOCK", "host.json")
	if w := matrixWaitFor(lead, now); w != nil {
		t.Fatalf("relative override: %+v", w)
	}
	// No override: the default path, here an absent file under a private home,
	// then a usable one.
	t.Setenv("TAILTERM_MATRIX_HOST_LOCK", "")
	oldDefault := matrixWaitDefaultPath
	t.Cleanup(func() { matrixWaitDefaultPath = oldDefault })
	matrixWaitDefaultPath = func() string { return filepath.Join(dir, "state", "host.json") }
	if w := matrixWaitFor(lead, now); w != nil {
		t.Fatalf("absent default file: %+v", w)
	}
	matrixWaitDefaultPath = func() string { return filepath.Join(dir, "host.json") }
	if w := matrixWaitFor(lead, now); w == nil || w.Role != api.MatrixWaitRunning {
		t.Fatalf("default path: %+v", w)
	}
	// The real pid probe, on this process and on pids that name none.
	if !nativeMatrixWaitPIDAlive(os.Getpid()) || nativeMatrixWaitPIDAlive(0) || nativeMatrixWaitPIDAlive(-1) {
		t.Fatal("pid probe")
	}
}

// The one parse both readers share: holders and ordered waiters for either
// version, and the reason for a file it refuses.
func TestParseMatrixLock(t *testing.T) {
	at := "2026-10-02T11:40:00.000Z"
	a, b, c := matrixEntry("11", matrixTestItem, "lead-a", at), matrixEntry("22", matrixTestItem, "lead-b", at), matrixEntry("33", matrixTestItem, "lead-c", at)
	agents := func(entries []matrixLockEntry) string {
		var names []string
		for _, e := range entries {
			names = append(names, e.Agent)
		}
		return strings.Join(names, ",")
	}
	for name, tc := range map[string]struct {
		content          string
		version          int
		holders, waiters string
	}{
		"version 1 holder":      {matrixLockJSON("Test-Mini", a, b, c), 1, "lead-a", "lead-b,lead-c"},
		"version 1 free":        {matrixLockJSON("Test-Mini", "", c, b), 1, "", "lead-c,lead-b"},
		"version 2 two holders": {matrixLockV2JSON("Test-Mini", []string{b, a}, c), 2, "lead-b,lead-a", "lead-c"},
		"version 2 free":        {matrixLockV2JSON("Test-Mini", nil), 2, "", ""},
	} {
		lock, reason := parseMatrixLock([]byte(tc.content))
		if lock == nil || reason != "" || lock.Version != tc.version || lock.Host != "Test-Mini" || agents(lock.Holders) != tc.holders || agents(lock.Waiters) != tc.waiters {
			t.Fatalf("%s: %+v %q", name, lock, reason)
		}
	}
	lock, _ := parseMatrixLock([]byte(matrixLockV2JSON("test-mini", []string{a})))
	if h := lock.Holders[0]; h != (matrixLockEntry{PID: 11, Kind: "matrix", Item: matrixTestItem, Agent: "lead-a", Priority: "normal", RequestedAt: at, StartedAt: at}) {
		t.Fatalf("entry fields: %+v", h)
	}
	for content, want := range map[string]string{
		"{broken":                 "not JSON",
		`[]`:                      "not JSON",
		`{"version":"2"}`:         "not JSON",
		`{}`:                      "unknown version",
		`{"version":0}`:           "unknown version",
		`{"version":3,"host":""}`: "unknown version",
	} {
		if lock, reason := parseMatrixLock([]byte(content)); lock != nil || reason != want {
			t.Fatalf("%s: %+v %q, want %q", content, lock, reason, want)
		}
	}
}

// The probe the relay uses, kept before any test replaces it.
var nativeMatrixWaitPIDAlive = matrixWaitPIDAlive

func TestMatrixWaitReportKey(t *testing.T) {
	since := time.Date(2026, 10, 2, 11, 40, 0, 0, time.UTC)
	idle := api.AgentActivity{State: "idle", Reason: "turn ended by API error (rate_limit)"}
	// Without a wait the key is the one saved before this field existed, so
	// an upgraded relay sends no report for a steady agent.
	if got, want := activityReportKey(idle), activityKeyVersion+legacyActivityKey(idle)+"\x00reason="+idle.Reason; got != want {
		t.Fatalf("key without a wait changed: %q, want %q", got, want)
	}
	wait := func(role string, position int, at time.Time) api.AgentActivity {
		a := idle
		a.MatrixWait = &api.MatrixWait{Role: role, Item: matrixTestItem, Position: position, Length: 3, Since: at}
		return a
	}
	waiting := activityReportKey(wait(api.MatrixWaitWaiting, 3, since))
	if waiting == activityReportKey(idle) {
		t.Fatal("a wait must be a new report")
	}
	if waiting != activityReportKey(wait(api.MatrixWaitWaiting, 1, since)) {
		t.Fatal("a moved position must not be a new report")
	}
	if waiting == activityReportKey(wait(api.MatrixWaitRunning, 3, since)) || waiting == activityReportKey(wait(api.MatrixWaitWaiting, 3, since.Add(time.Second))) {
		t.Fatal("a role or since change must be a new report")
	}
	if !sameActivityKey(waiting, wait(api.MatrixWaitWaiting, 2, since)) || sameActivityKey(waiting, idle) {
		t.Fatal("saved key comparison")
	}
}

func TestMatrixWaitActivityTick(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TAILTERM_RELAY_STATE", filepath.Join(home, "relay"))
	thread := "12345678-1234-1234-1234-123456789abc"
	dir := filepath.Join(home, ".codex", "sessions", "2026", "10", "02")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	transcript := `{"type":"task_started","timestamp":"2026-10-02T11:00:00Z"}` + "\n" + `{"type":"task_complete","timestamp":"2026-10-02T11:01:00Z"}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "rollout-test-"+thread+".jsonl"), []byte(transcript), 0600); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	requested := "2026-10-02T11:40:00.000Z"
	mine := matrixEntry("22", matrixTestItem, "lead-a", requested)
	other := matrixEntry("11", "wi_fedcba9876543210", "lead-b", requested)
	write := matrixLockFixture(t, 11, 22)

	var reports []api.AgentActivity
	var bodies []string
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/agents/agt_0123456789abcdef"):
			_ = json.NewEncoder(w).Encode(api.Agent{ID: "agt_0123456789abcdef", RunID: "run_0123456789abcdef", Name: "lead-a", Status: api.AgentRunning, Online: true, Session: "synthetic", WorkItem: &api.AgentWorkItemBinding{ItemID: matrixTestItem}})
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/obligations"):
			_, _ = io.WriteString(w, `{"obligations":[]}`)
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/activity"):
			body, _ := io.ReadAll(r.Body)
			var req api.ActivityReport
			if json.Unmarshal(body, &req) != nil {
				t.Error("invalid report")
			}
			reports, bodies = append(reports, req.Activity), append(bodies, string(body))
			_ = json.NewEncoder(w).Encode(req.Activity)
		default:
			http.NotFound(w, r)
		}
	}))
	defer hub.Close()
	client, _ := api.NewClient(hub.URL, 5*time.Second)
	b := runtimeBinding{Hub: hub.URL, Task: "tsk_0123456789abcdef", Agent: "agt_0123456789abcdef", Run: "run_0123456789abcdef", Thread: thread, Runtime: "codex", Codex: filepath.Join(home, "codex")}
	probe := func(runtimeBinding, api.Agent) (bool, bool, error) { return true, true, nil }
	tick := func(after time.Duration, lock string, posts int) {
		t.Helper()
		write(lock)
		if err := relayActivityTick(context.Background(), b, client, now.Add(after), probe); err != nil {
			t.Fatal(err)
		}
		if len(reports) != posts {
			t.Fatalf("after %s: %d reports, want %d: %+v", after, len(reports), posts, reports)
		}
	}
	tick(0, matrixLockJSON("test-mini", other, other, mine), 1)
	first := reports[0]
	if w := first.MatrixWait; first.State != "idle" || w == nil || !w.Valid() || w.Role != api.MatrixWaitWaiting || w.Position != 2 || w.Length != 2 || w.Item != matrixTestItem {
		t.Fatalf("first report: %+v wait %+v", first, w)
	}
	// Nothing of the run but its place leaves the host.
	for _, private := range []string{"abc1234", "/private/output", `"pid"`, "outputDir"} {
		if strings.Contains(bodies[0], private) {
			t.Fatalf("report carries %q: %s", private, bodies[0])
		}
	}
	tick(16*time.Second, matrixLockJSON("test-mini", other, mine), 1)
	tick(32*time.Second, matrixLockJSON("test-mini", other), 2)
	if last := reports[1]; last.State != "idle" || last.MatrixWait != nil {
		t.Fatalf("report after the entry left: %+v", last)
	}
	tick(48*time.Second, matrixLockJSON("test-mini", other), 2)
	// The run gets the host: one report, role running.
	tick(64*time.Second, matrixLockJSON("test-mini", mine), 3)
	if w := reports[2].MatrixWait; w == nil || !w.Valid() || w.Role != api.MatrixWaitRunning {
		t.Fatalf("holder report: %+v", w)
	}
	// A version 2 lock reports the same way: the second of two holders is the
	// same running wait, a waiter is a new report, and leaving clears it.
	tick(80*time.Second, matrixLockV2JSON("test-mini", []string{other, mine}, other), 3)
	tick(96*time.Second, matrixLockV2JSON("test-mini", []string{other, other}, other, mine), 4)
	if w := reports[3].MatrixWait; w == nil || !w.Valid() || w.Role != api.MatrixWaitWaiting || w.Position != 2 || w.Length != 2 {
		t.Fatalf("version 2 waiter report: %+v", w)
	}
	tick(112*time.Second, matrixLockV2JSON("test-mini", []string{other, other}), 5)
	if last := reports[4]; last.MatrixWait != nil {
		t.Fatalf("report after the version 2 entry left: %+v", last)
	}
}
