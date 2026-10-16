package main

import (
	"context"
	"encoding/json"
	"errors"
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
	// readMatrixWaitlist resolves matrixLockDefault itself. Its account home
	// here is this process's HOME, which TestMain makes private.
	matrixAccountHome = func() (string, error) { return os.Getenv("HOME"), nil }
}

// The readers' own default path and account home, kept before init replaces
// them.
var nativeMatrixWaitDefaultPath, nativeMatrixAccountHome = matrixWaitDefaultPath, matrixAccountHome

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

// matrixUnknownReads is every lock read the relay cannot confirm. Each leaves
// the reader with no parsed lock for this host; set runs after a usable lock
// was written to the fixture's path.
func matrixUnknownReads(t *testing.T) map[string]func(t *testing.T) {
	t.Helper()
	path := os.Getenv("TAILTERM_MATRIX_HOST_LOCK")
	replace := func(content string) func(*testing.T) {
		return func(t *testing.T) {
			t.Helper()
			if err := os.WriteFile(path, []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	return map[string]func(t *testing.T){
		"absent": func(t *testing.T) {
			t.Helper()
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
		},
		"unreadable": func(t *testing.T) {
			t.Helper()
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(path, 0700); err != nil { // reading a directory fails for any user
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Remove(path) })
		},
		"not JSON":        replace("{"),
		"unknown version": replace(strings.Replace(matrixLockJSON("test-mini", ""), `"version":1`, `"version":9`, 1)),
		"another host":    replace(matrixLockJSON("other-mini", "")),
		"no host":         replace(matrixLockJSON("", "")),
		"hostname lookup fails": func(t *testing.T) {
			t.Helper()
			old := matrixWaitHostname
			t.Cleanup(func() { matrixWaitHostname = old })
			matrixWaitHostname = func() (string, error) { return "", os.ErrPermission }
		},
		"relative path": func(t *testing.T) {
			t.Helper()
			t.Setenv("TAILTERM_MATRIX_HOST_LOCK", "host.json")
		},
	}
}

// wi_a598ffa55a326699: only a confirmed lock read ends a wait. An unknown
// read repeats the last confirmed wait the hub holds for the agent's run.
func TestMatrixWaitUnknownLockRead(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	since := now.Add(-20 * time.Minute)
	mine := matrixEntry("22", matrixTestItem, "lead-a", since.Format(time.RFC3339))
	held := func(w *api.MatrixWait) api.Agent {
		a := api.Agent{Name: "lead-a", WorkItem: &api.AgentWorkItemBinding{ItemID: matrixTestItem}}
		if w != nil {
			a.Activity = &api.AgentActivity{State: "idle", MatrixWait: w}
		}
		return a
	}
	running := &api.MatrixWait{Role: api.MatrixWaitRunning, Item: matrixTestItem, Since: since}
	waiting := &api.MatrixWait{Role: api.MatrixWaitWaiting, Item: matrixTestItem, Position: 2, Length: 3, Since: since}
	write := matrixLockFixture(t, 22)
	write(matrixLockJSON("test-mini", ""))
	for name := range matrixUnknownReads(t) {
		t.Run(name, func(t *testing.T) {
			write := matrixLockFixture(t, 22)
			write(matrixLockJSON("test-mini", ""))
			matrixUnknownReads(t)[name](t)
			if readMatrixLock() != nil {
				t.Fatal("the read is confirmed")
			}
			for _, w := range []*api.MatrixWait{running, waiting} {
				got := matrixWaitFor(held(w), now)
				if got == nil || *got != *w || got == w {
					t.Fatalf("held %+v: %+v", w, got)
				}
			}
			// With no wait in the hub's activity there is none, as before.
			if got := matrixWaitFor(held(nil), now); got != nil {
				t.Fatalf("nothing held: %+v", got)
			}
			if got := matrixWaitFor(api.Agent{Name: "lead-a", Activity: &api.AgentActivity{State: "idle"}}, now); got != nil {
				t.Fatalf("activity without a wait: %+v", got)
			}
			// A held wait that is not valid, or starts after now, is none.
			for _, bad := range []*api.MatrixWait{
				{Role: api.MatrixWaitRunning, Item: matrixTestItem},
				{Role: api.MatrixWaitWaiting, Item: matrixTestItem, Position: 4, Length: 3, Since: since},
				{Role: api.MatrixWaitRunning, Item: matrixTestItem, Since: now.Add(time.Second)},
			} {
				if got := matrixWaitFor(held(bad), now); got != nil {
					t.Fatalf("held %+v: %+v", bad, got)
				}
			}
			if got := matrixWaitFor(api.Agent{Activity: &api.AgentActivity{State: "idle", MatrixWait: running}}, now); got != nil {
				t.Fatalf("agent without a name: %+v", got)
			}
		})
	}
	// A confirmed read ignores what the hub holds: the entry gone, or its pid
	// dead, ends the wait, and a live entry is read from the lock.
	if got := matrixWaitFor(held(running), now); got != nil {
		t.Fatalf("confirmed read without the entry: %+v", got)
	}
	write(matrixLockJSON("test-mini", matrixEntry("33", matrixTestItem, "lead-a", since.Format(time.RFC3339))))
	if got := matrixWaitFor(held(running), now); got != nil {
		t.Fatalf("confirmed read with a dead pid: %+v", got)
	}
	write(matrixLockJSON("test-mini", "", mine))
	if got := matrixWaitFor(held(running), now); got == nil || got.Role != api.MatrixWaitWaiting || got.Position != 1 {
		t.Fatalf("confirmed read with a live waiter: %+v", got)
	}
}

// The same rule through the relay's tick against a hub that, like the real
// one, returns the activity it last accepted for the run: no report on an
// unknown read, one at the confirmed end, none after it.
func TestMatrixWaitUnknownLockReadTick(t *testing.T) {
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
	mine := matrixEntry("22", matrixTestItem, "lead-a", "2026-10-02T11:40:00.000Z")
	write := matrixLockFixture(t)
	alive := true
	matrixWaitPIDAlive = func(pid int) bool { return pid == 22 && alive }

	var accepted *api.AgentActivity
	var reports []api.AgentActivity
	var bodies []string
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/agents/agt_0123456789abcdef"):
			_ = json.NewEncoder(w).Encode(api.Agent{ID: "agt_0123456789abcdef", RunID: "run_0123456789abcdef", Name: "lead-a", Status: api.AgentRunning, Online: true, Session: "synthetic",
				WorkItem: &api.AgentWorkItemBinding{ItemID: matrixTestItem}, Activity: accepted})
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/obligations"):
			_, _ = io.WriteString(w, `{"obligations":[]}`)
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/activity"):
			body, _ := io.ReadAll(r.Body)
			var req api.ActivityReport
			if json.Unmarshal(body, &req) != nil {
				t.Error("invalid report")
			}
			accepted = &req.Activity
			reports, bodies = append(reports, req.Activity), append(bodies, string(body))
			_ = json.NewEncoder(w).Encode(req.Activity)
		default:
			http.NotFound(w, r)
		}
	}))
	defer hub.Close()
	b := runtimeBinding{Hub: hub.URL, Task: "tsk_0123456789abcdef", Agent: "agt_0123456789abcdef", Run: "run_0123456789abcdef", Thread: thread, Runtime: "codex", Codex: filepath.Join(home, "codex")}
	probe := func(runtimeBinding, api.Agent) (bool, bool, error) { return true, true, nil }
	after := time.Duration(0)
	// tick is one relay check 16 seconds after the last, by a relay that
	// keeps nothing in memory: a new client and only the saved cursor.
	tick := func(what string, posts int) {
		t.Helper()
		client, _ := api.NewClient(hub.URL, 5*time.Second)
		if err := relayActivityTick(context.Background(), b, client, now.Add(after), probe); err != nil {
			t.Fatal(err)
		}
		after += 16 * time.Second
		if len(reports) != posts {
			t.Fatalf("%s: %d reports, want %d: %+v", what, len(reports), posts, reports)
		}
	}
	live := matrixLockJSON("test-mini", mine)
	write(live)
	tick("the run holds the host", 1)
	if w := reports[0].MatrixWait; w == nil || w.Role != api.MatrixWaitRunning {
		t.Fatalf("first report: %+v", reports[0])
	}
	// Each unknown read in turn, then the same wait readable again: nothing.
	names := []string{"absent", "unreadable", "not JSON", "unknown version", "another host", "no host", "hostname lookup fails", "relative path"}
	if len(names) != len(matrixUnknownReads(t)) {
		t.Fatal("an unknown read is not exercised")
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			matrixUnknownReads(t)[name](t)
			tick(name, 1)
			tick(name+", again", 1)
		})
		write(live)
		tick("readable again after "+name, 1)
	}
	// Killed: the entry stays in the lock and its pid is gone. A confirmed
	// read, so one report without the wait, carrying nothing of the run.
	alive = false
	tick("killed", 2)
	if last := reports[1]; last.State != "idle" || last.MatrixWait != nil {
		t.Fatalf("report after the kill: %+v", last)
	}
	for _, private := range []string{"abc1234", "/private/output", `"pid"`, "outputDir", "matrixWait"} {
		if strings.Contains(bodies[1], private) {
			t.Fatalf("report carries %q: %s", private, bodies[1])
		}
	}
	tick("the dead entry is still there", 2)
	// After the accepted end the hub holds no wait, so an unknown read is none.
	t.Run("after the end", func(t *testing.T) {
		matrixUnknownReads(t)["not JSON"](t)
		tick("unknown read after the end", 2)
	})
	// A second run, then an unknown read at its end: the wait is kept until a
	// confirmed read shows the entry gone, and that is one report.
	alive = true
	write(matrixLockJSON("test-mini", matrixEntry("22", matrixTestItem, "lead-a", "2026-10-02T12:05:00.000Z")))
	tick("a second run", 3)
	t.Run("unknown at the end", func(t *testing.T) {
		matrixUnknownReads(t)["absent"](t)
		tick("unknown read", 3)
	})
	write(matrixLockJSON("test-mini", ""))
	tick("the entry is gone", 4)
	if reports[3].MatrixWait != nil {
		t.Fatalf("report after the second run: %+v", reports[3])
	}
	tick("steady", 4)
}

// wi_ee29358d0c7791ab: without TAILTERM_MATRIX_HOST_LOCK every reader takes
// the default lock from the account home, as the lock script does. Under
// another HOME none falls back to HOME's file: the activity has an unknown
// read, the sweep skips its run and the waitlist names the override.
func TestMatrixLockDefaultAccountHome(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	lead := api.Agent{Name: "lead-a", WorkItem: &api.AgentWorkItemBinding{ItemID: matrixTestItem}}
	content := matrixLockJSON("test-mini", "", matrixEntry("22", matrixTestItem, "lead-a", "2026-10-02T11:40:00.000Z"))
	matrixLockFixture(t, 22)
	t.Setenv("TAILTERM_MATRIX_HOST_LOCK", "")
	oldDefault, oldAccount, oldLocal := matrixWaitDefaultPath, matrixAccountHome, matrixLocalHost
	t.Cleanup(func() { matrixWaitDefaultPath, matrixAccountHome, matrixLocalHost = oldDefault, oldAccount, oldLocal })
	matrixWaitDefaultPath = nativeMatrixWaitDefaultPath
	matrixLocalHost = func() (string, error) { return "Test-Mini.local", nil }

	root := t.TempDir()
	account, private := filepath.Join(root, "account"), filepath.Join(root, "private")
	matrixAccountHome = func() (string, error) { return account, nil }
	lockIn := func(dir string) string { return filepath.Join(dir, ".local/state/tailterm-matrix/host.json") }
	write := func(path string) {
		t.Helper()
		if os.MkdirAll(filepath.Dir(path), 0o700) != nil || os.WriteFile(path, []byte(content), 0o600) != nil {
			t.Fatal("write lock file")
		}
	}
	// The same live waiter in both homes, so a reader of HOME's file shows it.
	write(lockIn(account))
	write(lockIn(private))
	// reads is what each reader made of the lock at path: a confirmed wait,
	// a waitlist with one waiter, and the sweep's file. The fixture lacks the
	// sweep's sequence fields, so the sweep refuses the file it read.
	reads := func(what, path string) {
		t.Helper()
		if w := matrixWaitFor(lead, now); w == nil || w.Role != api.MatrixWaitWaiting || w.Item != matrixTestItem {
			t.Fatalf("%s: matrix wait %+v", what, w)
		}
		if list, line := readMatrixWaitlist(); list == nil || line != "" || len(list.Waiters) != 1 {
			t.Fatalf("%s: waitlist %+v %q", what, list, line)
		}
		if m, why := loadMatrixExclusion(); m != nil || why != "invalid requestSeq" {
			t.Fatalf("%s: sweep %+v %q", what, m, why)
		}
		if os.Rename(path, path+".moved") != nil {
			t.Fatal("move lock file")
		}
		defer os.Rename(path+".moved", path)
		if m, why := loadMatrixExclusion(); m == nil || why != "" || m.file != path {
			t.Fatalf("%s: sweep with the file absent %+v %q, want a free host at %s", what, m, why, path)
		}
	}
	// refuses is every reader failing closed for the reason given.
	refuses := func(what, reason string) {
		t.Helper()
		path, refused := matrixLockDefault()
		if path != "" || !strings.Contains(refused, reason) || !strings.Contains(refused, "set TAILTERM_MATRIX_HOST_LOCK to an absolute lock path") {
			t.Fatalf("%s: default %q %q, want a refusal naming %q and the override", what, path, refused, reason)
		}
		if got := matrixWaitDefaultPath(); got != "" {
			t.Fatalf("%s: matrixWaitDefaultPath %q", what, got)
		}
		if w := matrixWaitFor(lead, now); w != nil {
			t.Fatalf("%s: matrix wait %+v", what, w)
		}
		if list, line := readMatrixWaitlist(); list != nil || line != "matrix host: "+refused+"; waitlist not shown" {
			t.Fatalf("%s: waitlist %+v %q", what, list, line)
		}
		// Never a free host, with HOME's file present or absent.
		if m, why := loadMatrixExclusion(); m != nil || why == "" {
			t.Fatalf("%s: sweep %+v %q", what, m, why)
		}
		if os.Rename(lockIn(private), lockIn(private)+".moved") != nil {
			t.Fatal("move lock file")
		}
		defer os.Rename(lockIn(private)+".moved", lockIn(private))
		if m, why := loadMatrixExclusion(); m != nil || why == "" {
			t.Fatalf("%s: sweep with HOME's file absent %+v %q", what, m, why)
		}
	}

	// Another HOME, with no override: no reader uses its file.
	t.Setenv("HOME", private)
	refuses("isolated HOME", "HOME ("+private+") is not this account's home ("+account+")")

	// HOME is the account home, the same directory through a link, or unset:
	// the account's shared file.
	t.Setenv("HOME", account)
	if path, refused := matrixLockDefault(); path != lockIn(account) || refused != "" {
		t.Fatalf("account HOME: %q %q", path, refused)
	}
	reads("account HOME", lockIn(account))
	link := filepath.Join(root, "link")
	if os.Symlink(account, link) != nil {
		t.Fatal("symlink")
	}
	t.Setenv("HOME", link)
	reads("linked HOME", lockIn(account))
	os.Unsetenv("HOME")
	reads("no HOME", lockIn(account))

	// An account home that cannot be read, or is not absolute, gives no path
	// either, even with HOME unset.
	for name, home := range map[string]func() (string, error){
		"unreadable account home": func() (string, error) { return "", errors.New("no such user") },
		"relative account home":   func() (string, error) { return "account", nil },
		"empty account home":      func() (string, error) { return "", nil },
	} {
		matrixAccountHome = home
		t.Setenv("HOME", account)
		refuses(name, "cannot read this account's home")
		os.Unsetenv("HOME")
		refuses(name+" without HOME", "cannot read this account's home")
	}
	matrixAccountHome = func() (string, error) { return account, nil }

	// An absolute override still wins under another HOME, and a relative one
	// is refused as before, not replaced by a default.
	t.Setenv("HOME", private)
	override := filepath.Join(root, "override", "host.json")
	write(override)
	t.Setenv("TAILTERM_MATRIX_HOST_LOCK", override)
	reads("override under isolated HOME", override)
	t.Setenv("TAILTERM_MATRIX_HOST_LOCK", "host.json")
	t.Chdir(filepath.Dir(override))
	if w := matrixWaitFor(lead, now); w != nil {
		t.Fatalf("relative override: matrix wait %+v", w)
	}
	if list, line := readMatrixWaitlist(); list != nil || line != "matrix host: TAILTERM_MATRIX_HOST_LOCK must be an absolute path; waitlist not shown" {
		t.Fatalf("relative override: waitlist %+v %q", list, line)
	}
	if m, why := loadMatrixExclusion(); m != nil || why != "the lock path is not absolute" {
		t.Fatalf("relative override: sweep %+v %q", m, why)
	}

	// The native account home comes from the user database, not HOME.
	if home, err := nativeMatrixAccountHome(); err != nil || !filepath.IsAbs(home) || sameMatrixHome(home, private) {
		t.Fatalf("native account home %q %v under HOME %s", home, err, private)
	}
}
