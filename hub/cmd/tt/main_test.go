package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/server"
	"github.com/scs32/tailterm/hub/internal/spawn"
	"github.com/scs32/tailterm/hub/internal/store"
)

// TestMain drops TAILTERM_* variables inherited from the shell, so the suite
// behaves the same when a Tailterm-launched agent runs it (a reviewer's
// TAILTERM_REASONING would otherwise leak into spawn defaults).
//
// It also raises minCommandTimeout to testCommandTimeout for the whole
// package, after saving the value the binary ships with. A test that needs
// the shipped deadlines sets minCommandTimeout back for its own duration.
//
// The tests then run with a temporary home of their own; see runWithOwnHome.
func TestMain(m *testing.M) {
	for _, kv := range os.Environ() {
		if name, _, _ := strings.Cut(kv, "="); strings.HasPrefix(name, "TAILTERM_") {
			os.Unsetenv(name)
		}
	}
	productionMinCommandTimeout = minCommandTimeout
	minCommandTimeout = testCommandTimeout
	os.Exit(runWithOwnHome(func() int { return runWithOwnTmuxDir(m) }))
}

// runWithOwnHome runs the package's tests with HOME pointing at an empty
// directory only this test process owns, and removes it afterwards. A test
// that forgets its own isolation, and any process it starts, then resolves
// relay.json, hub.json, handoff.json, the relay state and the tool ledger
// under that directory, never under the home of the user running the tests
// (wi_fc17a11cbecb5af7: a child test binary once read the host's redaction
// setting and lost ledger rows).
//
// The guard is checked before and after the run, and fails it with the path
// that would have been read. A child that is this test binary again inherits
// the real homes in testRealHomesEnv and checks the home it was given, so a
// test that starts one with the user's home fails with that child's message.
// A child may have another temporary home; it never gets one of its own here.
// Neither does a process whose caller already gave it a home that is not the
// user's: it runs under the guard with that home.
//
// Two runs keep the home they were given, unguarded. With TT_LIVE_CLAUDE=1
// or TT_LIVE_RUNTIME_PROMPT=1 the opt-in live tests start a real Claude Code
// session, which needs the caller's home for its own login. And the
// redaction probe helper is not a test: it is that session's hook and exits
// inside its test function.
//
// CODEX_HOME and CLAUDE_CONFIG_DIR are dropped like the TAILTERM_ variables:
// an agent's shell sets them to the user's own directories. The Go build and
// module caches keep their places, because several tests build a binary and
// an empty home would rebuild the module and download its dependencies.
func runWithOwnHome(run func() int) int {
	if os.Getenv(redactProbeHelperEnv) != "" {
		return run()
	}
	_, child := os.LookupEnv(testRealHomesEnv)
	for _, live := range []string{"TT_LIVE_CLAUDE", "TT_LIVE_RUNTIME_PROMPT"} {
		if os.Getenv(live) != "1" {
			continue
		}
		if !child {
			fmt.Fprintln(os.Stderr, live+"=1: these tests keep the caller's HOME, because Claude Code needs its own login; the real-home guard is off for this run")
		}
		os.Setenv(testRealHomesEnv, "")
		return run()
	}
	os.Unsetenv("CODEX_HOME")
	os.Unsetenv("CLAUDE_CONFIG_DIR")
	if child {
		return guardedRun(run)
	}
	home, _ := os.UserHomeDir()
	// user.Current does not read HOME where cgo is on, so a caller's
	// temporary HOME does not hide the user's own.
	real := home
	if u, err := user.Current(); err == nil && u.HomeDir != "" {
		real = u.HomeDir
	}
	os.Setenv(testRealHomesEnv, real)
	if home != "" && !pathWithinAny(home, []string{real}) {
		return guardedRun(run)
	}
	if out, err := exec.Command("go", "env", "GOCACHE", "GOMODCACHE", "GOPATH").Output(); err == nil {
		if values := strings.Split(strings.TrimSpace(string(out)), "\n"); len(values) == 3 {
			for i, name := range []string{"GOCACHE", "GOMODCACHE", "GOPATH"} {
				os.Setenv(name, values[i])
			}
		}
	}
	dir, err := os.MkdirTemp("", "tt-home-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "private home directory:", err)
		return 1
	}
	os.Setenv("HOME", dir)
	code := guardedRun(run)
	if err := os.RemoveAll(dir); err != nil {
		fmt.Fprintln(os.Stderr, "remove private home directory:", err)
		if code == 0 {
			code = 1
		}
	}
	return code
}

// guardedRun checks the guard on both sides of the run, so a test that left
// the process environment pointing at the user's home fails the run too.
func guardedRun(run func() int) int {
	if err := realHomeGuard(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	code := run()
	if err := realHomeGuard(); err != nil {
		fmt.Fprintln(os.Stderr, "after the tests:", err)
		if code == 0 {
			code = 1
		}
	}
	return code
}

// testRealHomesEnv lists the homes of the user running the tests, as the
// first test process found them. Set but empty, it turns the guard off.
const testRealHomesEnv = "TT_TEST_REAL_HOMES"

// realHomeGuard returns an error naming the first path this process would
// read under a real home: the home itself, or a setting, state or runtime
// directory anywhere below one. A path under the temporary directory, or
// under a home that is not the user's, is this run's own even when that
// directory lies below the user's home.
func realHomeGuard() error {
	real := filepath.SplitList(os.Getenv(testRealHomesEnv))
	home, _ := os.UserHomeDir()
	const rule = "a test and every process it starts must use a temporary home (see runWithOwnHome)"
	own := []string{os.TempDir()}
	for _, dir := range real {
		if home != "" && pathWithinAny(dir, []string{home}) && pathWithinAny(home, []string{dir}) {
			return fmt.Errorf("real-home guard: the home of this hub/cmd/tt test process is the real user home %s; %s", home, rule)
		}
	}
	if home != "" {
		own = append(own, home)
	}
	for _, p := range []struct{ what, path string }{
		{"the redaction setting", toolRedactSettingPath()},
		{"the relay state", relayDir()},
		{"the handoff config", handoffConfigPath()},
		{"the handoff directory", handoffRoot()},
		{"the tool ledger", toolLedgerRoot()},
		{"the Codex home", codexHome()},
	} {
		if p.path != "" && !pathWithinAny(p.path, own) && pathWithinAny(p.path, real) {
			return fmt.Errorf("real-home guard: this hub/cmd/tt test process resolves %s to %s, inside the real user home; %s", p.what, p.path, rule)
		}
	}
	return nil
}

// pathWithinAny reports whether path is one of dirs or below one, comparing
// the paths as written and with their symbolic links resolved.
func pathWithinAny(path string, dirs []string) bool {
	forms := func(p string) []string {
		out := []string{filepath.Clean(p)}
		if resolved, err := filepath.EvalSymlinks(p); err == nil && resolved != out[0] {
			out = append(out, resolved)
		}
		return out
	}
	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		for _, d := range forms(dir) {
			for _, p := range forms(path) {
				if rel, err := filepath.Rel(d, p); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
					return true
				}
			}
		}
	}
	return false
}

// runWithOwnTmuxDir runs the package's tests with TMUX_TMPDIR pointing at a
// directory only this test process owns, and removes it afterwards, on pass
// and on failure. tmux leaves a socket file behind when its server stops, so
// every private -L server a test started used to leave one in the shared
// per-user directory (wi_3493188f14edd3e3). Nothing is removed by name or
// age, and the shared directory and its default server are never touched.
//
// The directory sits directly under /tmp rather than os.TempDir(): a unix
// socket path holds about 104 bytes, and the macOS per-user temporary
// directory would use half of that before the socket name.
//
// A child that is this test binary again keeps the directory it inherited.
// Some such children exit without returning here: with a directory each, one
// package run left eleven empty ones behind.
func runWithOwnTmuxDir(m *testing.M) int {
	if dir := os.Getenv(testTmuxDirEnv); dir != "" && dir == os.Getenv("TMUX_TMPDIR") {
		return m.Run()
	}
	dir, err := os.MkdirTemp("/tmp", "tt-tmux-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "private tmux directory:", err)
		return 1
	}
	os.Setenv("TMUX_TMPDIR", dir)
	os.Setenv(testTmuxDirEnv, dir)
	code := m.Run()
	// Stop any server a test left running; it would be unreachable once its
	// socket is gone.
	sockets, _ := filepath.Glob(filepath.Join(dir, "tmux-*", "*"))
	for _, socket := range sockets {
		_ = exec.Command("tmux", "-S", socket, "kill-server").Run()
	}
	if err := os.RemoveAll(dir); err != nil {
		fmt.Fprintln(os.Stderr, "remove private tmux directory:", err)
		if code == 0 {
			code = 1
		}
	}
	return code
}

// testTmuxDirEnv names the TMUX_TMPDIR a parent test process owns, so a child
// can tell an inherited directory from one the caller's shell set.
const testTmuxDirEnv = "TT_TEST_TMUX_TMPDIR"

// testCommandTimeout is the floor the package's tests put under the CLI's
// fixed command deadlines. Under full-matrix load one loopback request to a
// trivial test hub has outlasted the 10 s a command asks for
// (wi_5e804f501074629f, wi_a19eb9be318f557e).
const testCommandTimeout = 60 * time.Second

// productionMinCommandTimeout is minCommandTimeout as the binary ships it,
// read by TestMain before the package floor replaces it.
var productionMinCommandTimeout time.Duration

func TestInboxWaitFindsDirectedMessageBehindSelfSentPage(t *testing.T) {
	h := newInboxHub(t)
	e, _, link, order := h.bind(t, "wait-bound")
	old := inboxPollInterval
	inboxPollInterval = 20 * time.Millisecond
	t.Cleanup(func() { inboxPollInterval = old })

	own := h.post(t, api.PostMessageRequest{Text: "self progress", RequestID: "wait-self", AgentID: e.agent, RunID: e.runID, WorkItems: link, WorkOrderMessage: order}, 51)
	if n, err := unreadCount(h.c, e.task, e.agent); err != nil || n != 0 {
		t.Fatalf("self-sent unread count = %d, %v; want 0", n, err)
	}
	directed := h.post(t, api.PostMessageRequest{Text: "directed wake", AgentID: h.lead.ID, To: e.agent}, 1)[0]
	if n, err := unreadCount(h.c, e.task, e.agent); err != nil || n != 1 {
		t.Errorf("unread count behind 51 self-sent messages = %d, %v; want 1", n, err)
	}
	start := time.Now()
	out, err := captureCLIOutput(t, func() error {
		return cmdInbox(e, []string{"--unread", "--wait", "2s", "--mark-read", "--json"})
	})
	elapsed := time.Since(start)
	var messages []api.Message
	if err != nil || json.Unmarshal([]byte(out), &messages) != nil || len(messages) != 1 || messages[0].Seq != directed.Seq {
		t.Fatalf("wait JSON = %q, %v; want only directed message #%d", out, err, directed.Seq)
	}
	if elapsed >= time.Second {
		t.Errorf("wait took %v despite an unread directed message; want under 1s", elapsed)
	}
	a, err := h.c.GetAgent(context.Background(), e.task, e.agent)
	if err != nil || a.ReadUpTo != own[49].Seq || a.Unread != 1 {
		t.Fatalf("first page cursor/count = %+v, %v; want cursor %d and unread 1", a, err, own[49].Seq)
	}
	out, err = captureCLIOutput(t, func() error {
		return cmdInbox(e, []string{"--unread", "--wait", "2s", "--mark-read", "--json"})
	})
	if err != nil || json.Unmarshal([]byte(out), &messages) != nil || len(messages) != 1 || messages[0].Seq != directed.Seq {
		t.Fatalf("tail JSON = %q, %v; want the directed message again", out, err)
	}
	a, err = h.c.GetAgent(context.Background(), e.task, e.agent)
	if err != nil || a.ReadUpTo != directed.Seq || a.Unread != 0 {
		t.Fatalf("drained cursor/count = %+v, %v; want cursor %d and unread 0", a, err, directed.Seq)
	}
}

func TestInboxWaitPropagatesUnreadCountError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/tasks/task/agents/agent" {
			t.Errorf("unexpected request after failed unread count: %s", r.URL.Path)
		}
		http.Error(w, "unread count unavailable", http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	c, err := api.NewClient(srv.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := unreadCount(c, "task", "agent"); n != 0 || err == nil || !strings.Contains(err.Error(), "503") || !strings.Contains(err.Error(), "unread count unavailable") {
		t.Fatalf("unread count error = %d, %v", n, err)
	}
	err = cmdInbox(env{hub: srv.URL, task: "task", agent: "agent"}, []string{"--unread", "--wait", "2s"})
	if err == nil || !strings.Contains(err.Error(), "503") || !strings.Contains(err.Error(), "unread count unavailable") {
		t.Fatalf("wait error = %v", err)
	}
}

// closeHub is an isolated SQLite hub behind httptest with a private tmux
// socket and relay state, for tt close tests against the real handler floor
// (wi_67fb6b7716a5c1b4, order #19263). writes counts every non-GET request.
type closeHub struct {
	c      *api.Client
	e      env
	task   api.Task
	db     *sql.DB
	writes atomic.Int64
}

func newCloseHub(t *testing.T) *closeHub {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux unavailable")
	}
	t.Setenv("TT_TMUX_SOCKET", "tt-close-handler-"+strings.TrimPrefix(api.NewID("agt"), "agt_"))
	t.Setenv("TAILTERM_RELAY_STATE", t.TempDir())
	t.Cleanup(func() { _, _ = startupTmux(context.Background(), "kill-server") })
	f := &closeHub{}
	dbPath := filepath.Join(t.TempDir(), "hub.sqlite")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if f.db, err = sql.Open("sqlite", dbPath); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.db.Close() })
	handler := server.New(st, func(r *http.Request) (api.Caller, error) { return api.Caller{Node: "fixture", User: "owner"}, nil })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			f.writes.Add(1)
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	if f.c, err = api.NewClient(srv.URL, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if f.task, err = f.c.CreateTask(context.Background(), api.CreateTaskRequest{Name: "isolated close test"}); err != nil {
		t.Fatal(err)
	}
	f.e = env{hub: f.c.Base, task: f.task.ID}
	return f
}

// agent registers an agent on this host and starts the tmux session its
// exact run owns.
func (f *closeHub) agent(t *testing.T, name, role string) api.Agent {
	t.Helper()
	ctx := context.Background()
	req := api.AddAgentRequest{Name: name, Host: spawn.Host(), Session: name, Runtime: "generic"}
	if role != "" {
		req.Role, req.AgentID = role, api.NewID("agt")
	}
	a, err := f.c.AddAgent(ctx, f.task.ID, req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = startupTmux(ctx, "new-session", "-d", "-s", a.Session, "-e", "TAILTERM_HUB="+f.e.hub, "-e", "TAILTERM_TASK="+f.task.ID, "-e", "TAILTERM_AGENT="+a.ID, "-e", "TAILTERM_RUN="+a.RunID, "sleep 300"); err != nil {
		t.Fatal(err)
	}
	return a
}

func (f *closeHub) exec(t *testing.T, query string, args ...any) {
	t.Helper()
	if _, err := f.db.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}

func (f *closeHub) status(t *testing.T, id string) string {
	t.Helper()
	a, err := f.c.GetAgent(context.Background(), f.task.ID, id)
	if err != nil {
		t.Fatal(err)
	}
	return a.Status
}

func (f *closeHub) sessionAlive(a api.Agent) bool {
	_, err := startupTmux(context.Background(), "has-session", "-t", "="+a.Session)
	return err == nil
}

// a1: a retired handler that is not the primary closes through the hub with
// its exact run, and its session cleanup is confirmed.
func TestCloseSendsRetiredNonPrimaryHandlerToHub(t *testing.T) {
	f := newCloseHub(t)
	ctx := context.Background()
	keep := f.agent(t, "db-handler", api.AgentRoleDatabaseHandler)
	old := f.agent(t, "db-handler-2", api.AgentRoleDatabaseHandler)
	retired := api.AgentRetired
	if _, err := f.c.UpdateAgent(ctx, f.task.ID, old.ID, api.UpdateAgentRequest{Status: &retired}); err != nil {
		t.Fatalf("retire the non-primary handler: %v", err)
	}
	// A stale run is refused by the hub, so a close that lands proves the CLI
	// sent this handler's exact run.
	if _, err := f.c.CloseAgent(ctx, f.task.ID, old.ID, api.NewID("run")); err == nil {
		t.Fatal("hub closed a retired handler for a stale run")
	}
	if err := cmdClose(f.e, []string{old.Name}); err != nil {
		t.Fatalf("tt close retired non-primary handler: %v", err)
	}
	got, err := f.c.GetAgent(ctx, f.task.ID, old.ID)
	if err != nil || got.Status != api.AgentClosed || got.RunID != old.RunID || !got.CleanupDone {
		t.Fatalf("retired handler after close: %+v %v", got, err)
	}
	if f.sessionAlive(old) {
		t.Fatal("closed handler's session survived")
	}
	if f.status(t, keep.ID) == api.AgentClosed || !f.sessionAlive(keep) {
		t.Fatal("close touched the remaining handler")
	}
}

// a2: the hub's refusal of a retired primary reaches the caller unchanged, and
// nothing is cleaned up.
func TestCloseReportsHubRefusalForRetiredPrimaryHandler(t *testing.T) {
	f := newCloseHub(t)
	primary := f.agent(t, "db-handler", api.AgentRoleDatabaseHandler)
	f.agent(t, "db-handler-2", api.AgentRoleDatabaseHandler)
	// The floor refuses to retire a primary, so the fixture records the state
	// an older hub or a pause could leave behind.
	f.exec(t, `UPDATE tasks SET primary_handler_id=? WHERE id=?`, primary.ID, f.task.ID)
	f.exec(t, `UPDATE agents SET status=? WHERE id=?`, api.AgentRetired, primary.ID)
	_, hubErr := f.c.CloseAgent(context.Background(), f.task.ID, primary.ID, primary.RunID)
	if hubErr == nil || !strings.Contains(hubErr.Error(), "409") || !strings.Contains(hubErr.Error(), "db-handler is the project's primary database handler; rotate it with tt handler rotate") {
		t.Fatalf("hub close of the retired primary: %v", hubErr)
	}
	err := cmdClose(f.e, []string{primary.ID})
	if err == nil || err.Error() != hubErr.Error() {
		t.Fatalf("tt close retired primary = %v, want the hub's refusal %v", err, hubErr)
	}
	if got := f.status(t, primary.ID); got != api.AgentRetired {
		t.Fatalf("retired primary status after refused close: %s", got)
	}
	if !f.sessionAlive(primary) {
		t.Fatal("refused close cleaned up the handler's session")
	}
}

// a3: a handler that is not retired, the deployment agent and the steward keep
// their client-side refusals in every status, and no write reaches the hub.
func TestCloseStillRefusesAvailableHandlerAndPersistentRoles(t *testing.T) {
	f := newCloseHub(t)
	f.agent(t, "db-handler", api.AgentRoleDatabaseHandler)
	const handler = "the active database handler remains available while the project is open"
	for _, tc := range []struct {
		name, role, want string
		statuses         []string
	}{
		{name: "db-handler-2", role: api.AgentRoleDatabaseHandler, want: handler, statuses: []string{api.AgentRunning, api.AgentDone}},
		{name: "deployer", role: api.AgentRoleDeployment, want: handler, statuses: []string{api.AgentRunning, api.AgentRetired}},
		{name: "backlog-steward", role: api.AgentRoleBacklogSteward, want: "the backlog steward remains available while the project is open; rotate it with tt steward rotate", statuses: []string{api.AgentRunning, api.AgentRetired}},
	} {
		a := f.agent(t, tc.name, tc.role)
		for _, status := range tc.statuses {
			t.Run(tc.name+"/"+status, func(t *testing.T) {
				f.exec(t, `UPDATE agents SET status=? WHERE id=?`, status, a.ID)
				writes := f.writes.Load()
				err := cmdClose(f.e, []string{a.ID})
				if err == nil || err.Error() != tc.want {
					t.Fatalf("tt close = %v, want %q", err, tc.want)
				}
				if n := f.writes.Load() - writes; n != 0 {
					t.Fatalf("refused close sent %d hub writes", n)
				}
				if got := f.status(t, a.ID); got != status || !f.sessionAlive(a) {
					t.Fatalf("refused close changed the agent: status %s -> %s, session alive %v", status, got, f.sessionAlive(a))
				}
			})
		}
	}
}
