package main

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
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
func TestMain(m *testing.M) {
	for _, kv := range os.Environ() {
		if name, _, _ := strings.Cut(kv, "="); strings.HasPrefix(name, "TAILTERM_") {
			os.Unsetenv(name)
		}
	}
	os.Exit(m.Run())
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
