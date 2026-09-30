package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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

// Backlog steward CLI tests (wi_5b4b94dbc9a11e8b, order #14942): an isolated
// SQLite hub behind httptest, a private tmux socket, a temporary relay state
// directory and HOME, and a fake tt that only sleeps. Stewards launch through
// the real cmdSpawn and ensureHandler; only the tmux wrapper is the fake.
type stewardCLI struct {
	st     *store.Store
	dbPath string
	c      *api.Client
	e      env
	task   api.Task
	dir    string
	marker string
	// dropBefore loses the next agent registration before the hub sees it;
	// dropAfter loses the reply after the hub saved it.
	dropBefore atomic.Bool
	dropAfter  atomic.Bool
	calls      sync32
}

type sync32 struct{ due, actions atomic.Int32 }

func newStewardCLI(t *testing.T) *stewardCLI {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux unavailable")
	}
	t.Setenv("TT_TMUX_SOCKET", "tt-steward-test-"+strings.TrimPrefix(api.NewID("agt"), "agt_"))
	t.Setenv("TAILTERM_RELAY_STATE", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Cleanup(func() { _, _ = startupTmux(context.Background(), "kill-server") })
	f := &stewardCLI{dbPath: filepath.Join(t.TempDir(), "hub.sqlite")}
	var err error
	if f.st, err = store.Open(f.dbPath); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.st.Close() })
	handler := server.New(f.st, func(r *http.Request) (api.Caller, error) { return api.Caller{Node: "fixture", User: "owner"}, nil })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/steward-rotations/due") {
			f.calls.due.Add(1)
		}
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/steward-rotations") {
			f.calls.actions.Add(1)
		}
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/agents") {
			if f.dropBefore.CompareAndSwap(true, false) {
				if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
					conn.Close()
				}
				return
			}
			if f.dropAfter.CompareAndSwap(true, false) {
				handler.ServeHTTP(httptest.NewRecorder(), r)
				if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
					conn.Close()
				}
				return
			}
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	if f.c, err = api.NewClient(srv.URL, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	f.e = env{hub: f.c.Base}
	if f.task, err = f.c.CreateTask(context.Background(), api.CreateTaskRequest{Name: "isolated steward test"}); err != nil {
		t.Fatal(err)
	}
	f.dir = t.TempDir()
	f.marker = filepath.Join(f.dir, "executions")
	self := filepath.Join(f.dir, "fake-tt")
	if err = os.WriteFile(self, []byte("#!/bin/sh\nprintf x >> "+spawn.ShellQuote(f.marker)+"\nexec sleep 300\n"), 0700); err != nil {
		t.Fatal(err)
	}
	prior := ensurePersistent
	ensurePersistent = func(ctx context.Context, c *api.Client, task string, req api.AddAgentRequest, opts spawn.Options) (api.Agent, error) {
		opts.Self = self
		return ensureHandler(ctx, c, task, req, opts)
	}
	t.Cleanup(func() { ensurePersistent = prior })
	return f
}

func (f *stewardCLI) db(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func (f *stewardCLI) executions(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if data, _ := os.ReadFile(f.marker); len(data) == n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	data, _ := os.ReadFile(f.marker)
	t.Fatalf("expected %d launches, got %q", n, data)
}

// spawnArgs are tt spawn flags for a steward with a fixed synthetic template.
func (f *stewardCLI) spawnArgs(id, name string) []string {
	return []string{"--role", api.AgentRoleBacklogSteward, "--agent-id", id, "--name", name, "--run", "claude", "--task", f.task.ID, "--hub", f.c.Base,
		"--cwd", f.dir, "--prompt", "synthetic steward prompt", "--model", "claude-opus-5-5", "--reasoning", "high"}
}

// fixtureTemplate stands in for the embedded bundle in setup tests.
func fixtureTemplate(context.Context) (stewardTemplate, error) {
	return stewardTemplate{ID: "backlog_steward", Name: "backlog-steward", Title: "Backlog steward", Role: api.AgentRoleBacklogSteward, Runtime: "claude",
		Model: "claude-opus-5-5", Reasoning: "high", Prompt: "synthetic steward prompt"}, nil
}

func (f *stewardCLI) deps() stewardDeps {
	return stewardDeps{template: fixtureTemplate, spawn: cmdSpawn}
}

func (f *stewardCLI) live(t *testing.T, a api.Agent) {
	t.Helper()
	ctx := context.Background()
	if _, err := f.c.PostEvent(ctx, f.task.ID, api.PostEventRequest{AgentID: a.ID, RunID: a.RunID, Kind: api.EventStarted}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := f.c.ReportActivity(ctx, f.task.ID, a.ID, api.ActivityReport{RequestID: api.NewID("act"), RunID: a.RunID, Activity: api.AgentActivity{State: "idle", ObservedAt: now, LastEventAt: now}}); err != nil {
		t.Fatal(err)
	}
}

func captureStdout(t *testing.T, run func() error) (string, error) {
	t.Helper()
	out, err := os.Create(filepath.Join(t.TempDir(), "stdout.txt"))
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = out
	runErr := run()
	os.Stdout = old
	_ = out.Close()
	data, err := os.ReadFile(out.Name())
	if err != nil {
		t.Fatal(err)
	}
	return string(data), runErr
}

func TestStewardSpawnRegistersRoleThroughTTSpawn(t *testing.T) {
	f := newStewardCLI(t)
	ctx := context.Background()
	id := api.NewID("agt")
	if err := cmdSpawn(f.e, f.spawnArgs(id, "backlog-steward")); err != nil {
		t.Fatal(err)
	}
	f.executions(t, 1)
	a, err := f.c.GetAgent(ctx, f.task.ID, id)
	if err != nil {
		t.Fatal(err)
	}
	if a.Role != api.AgentRoleBacklogSteward || a.ParentAgentID != "" || a.WorkItem != nil || a.Session != "tt-steward-"+strings.TrimPrefix(id, "agt_") || a.Runtime != "claude" {
		t.Fatalf("steward registration: %+v", a)
	}
	var digest string
	if err = f.db(t).QueryRow(`SELECT template_digest FROM steward_runs WHERE run_id=? AND agent_id=?`, a.RunID, a.ID).Scan(&digest); err != nil || digest != stewardTemplateDigest("claude-opus-5-5", "high", "synthetic steward prompt") {
		t.Fatalf("steward template digest %q: %v", digest, err)
	}
	listing, err := captureStdout(t, func() error { return cmdAgents(env{hub: f.c.Base, task: f.task.ID}, nil) })
	if err != nil || !strings.Contains(listing, "backlog-steward") || !strings.Contains(listing, "role=backlog_steward") {
		t.Fatalf("tt agents: %q %v", listing, err)
	}
	// A second identity is refused with the named reason; nothing launches.
	err = cmdSpawn(f.e, f.spawnArgs(api.NewID("agt"), "backlog-steward-2"))
	var httpErr *api.HTTPError
	if !errors.As(err, &httpErr) || httpErr.Status != http.StatusConflict || httpErr.Code != api.StewardRefusedActive {
		t.Fatalf("second steward: %v", err)
	}
	f.executions(t, 1)
}

func TestStewardSpawnRefusedInsideAgentSession(t *testing.T) {
	f := newStewardCLI(t)
	worker, err := f.c.AddAgent(context.Background(), f.task.ID, api.AddAgentRequest{Name: "builder", Host: "fixture", Session: "builder", Runtime: "generic"})
	if err != nil {
		t.Fatal(err)
	}
	e := env{hub: f.c.Base, task: f.task.ID, agent: worker.ID, agentName: worker.Name}
	if err = cmdSpawn(e, f.spawnArgs(api.NewID("agt"), "backlog-steward")); err == nil || !strings.Contains(err.Error(), "owner command") {
		t.Fatalf("agent-session steward spawn: %v", err)
	}
	if agents, _ := f.c.ListAgents(context.Background(), f.task.ID); len(agents) != 1 {
		t.Fatalf("agents after refused spawn: %+v", agents)
	}
}

func TestStewardCloseRefusedFromAgentSession(t *testing.T) {
	f := newStewardCLI(t)
	ctx := context.Background()
	id := api.NewID("agt")
	if err := cmdSpawn(f.e, f.spawnArgs(id, "backlog-steward")); err != nil {
		t.Fatal(err)
	}
	worker, err := f.c.AddAgent(ctx, f.task.ID, api.AddAgentRequest{Name: "builder", Host: "fixture", Session: "builder", Runtime: "generic"})
	if err != nil {
		t.Fatal(err)
	}
	e := env{hub: f.c.Base, task: f.task.ID, agent: worker.ID, agentName: worker.Name}
	err = cmdClose(e, []string{"backlog-steward"})
	if err == nil || !strings.Contains(err.Error(), "backlog steward remains available") {
		t.Fatalf("tt close steward: %v", err)
	}
	a, err := f.c.GetAgent(ctx, f.task.ID, id)
	if err != nil || a.Status == api.AgentClosed {
		t.Fatalf("steward after refused close: %+v %v", a, err)
	}
}

func TestStewardTemplateFromEmbeddedBundle(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node unavailable")
	}
	out, err := captureStdout(t, func() error { return cmdStewardTemplate(env{}, []string{"--json"}) })
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		stewardTemplate
		Digest string `json:"digest"`
	}
	if err = json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("template JSON %q: %v", out, err)
	}
	if got.Role != api.AgentRoleBacklogSteward || got.Runtime != "claude" || got.Model != "claude-opus-5-5" || got.Reasoning != "high" || got.Name != "backlog-steward" {
		t.Fatalf("template fields: %+v", got.stewardTemplate)
	}
	if !strings.Contains(got.Prompt, "run tt ack SEQ before you start") || got.Digest != stewardTemplateDigest(got.Model, got.Reasoning, got.Prompt) {
		t.Fatalf("template prompt or digest: %s", got.Digest)
	}
	// The same bundle the TailOS client ships: compare with the JS module.
	js, err := exec.Command("node", "--input-type=module", "-e",
		`import { PROJECT_ROLE_TEMPLATES } from "`+filepath.Join(repoRoot(t), "client", "team-examples.js")+`"; process.stdout.write(JSON.stringify(PROJECT_ROLE_TEMPLATES.backlog_steward));`).Output()
	if err != nil {
		t.Fatal(err)
	}
	var source stewardTemplate
	if err = json.Unmarshal(js, &source); err != nil || source != got.stewardTemplate {
		t.Fatalf("embedded bundle drifted from client/team-examples.js: %v", err)
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Clean(filepath.Join(wd, "..", "..", ".."))
}

func TestStewardSetupReusesSavedIdentityAfterLostRegistration(t *testing.T) {
	f := newStewardCLI(t)
	ctx := context.Background()
	// Lost before the hub saved it: the retry reuses the identity (404 path).
	f.dropBefore.Store(true)
	if _, err := setupSteward(ctx, f.deps(), f.e, f.c, f.task.ID, f.dir, "", ""); err == nil {
		t.Fatal("setup reported success although registration was lost")
	}
	saved, err := loadStewardIdentity(f.c.Base, f.task.ID)
	if err != nil || saved == nil {
		t.Fatalf("identity file after lost registration: %+v %v", saved, err)
	}
	a, err := setupSteward(ctx, f.deps(), f.e, f.c, f.task.ID, f.dir, "", "")
	if err != nil || a.ID != saved.AgentID || a.Name != "backlog-steward" {
		t.Fatalf("retried setup: %+v %v", a, err)
	}
	f.executions(t, 1)
	// Setup is idempotent while the steward is open.
	again, err := setupSteward(ctx, f.deps(), f.e, f.c, f.task.ID, "", "", "")
	if err != nil || again.ID != a.ID || again.RunID != a.RunID {
		t.Fatalf("repeated setup: %+v %v", again, err)
	}
	f.executions(t, 1)
	if info, err := os.Stat(stewardIdentityPath(f.c.Base, f.task.ID)); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("identity file mode: %v %v", info, err)
	}
	if data, _ := os.ReadFile(stewardIdentityPath(f.c.Base, f.task.ID)); strings.Contains(strings.ToLower(string(data)), "token") {
		t.Fatalf("identity file holds a credential field: %s", data)
	}
}

func TestStewardSetupReusesIdentityWhenReplyWasLost(t *testing.T) {
	f := newStewardCLI(t)
	ctx := context.Background()
	f.dropAfter.Store(true)
	if _, err := setupSteward(ctx, f.deps(), f.e, f.c, f.task.ID, f.dir, "", ""); err == nil {
		t.Fatal("setup reported success although the reply was lost")
	}
	saved, err := loadStewardIdentity(f.c.Base, f.task.ID)
	if err != nil || saved == nil {
		t.Fatal(err)
	}
	a, err := setupSteward(ctx, f.deps(), f.e, f.c, f.task.ID, f.dir, "", "")
	if err != nil || a.ID != saved.AgentID {
		t.Fatalf("retried setup: %+v %v", a, err)
	}
	f.executions(t, 1)
	agents, err := f.c.ListAgents(ctx, f.task.ID)
	if err != nil || len(agents) != 1 {
		t.Fatalf("stewards after retried setup: %+v %v", agents, err)
	}
}

func TestStewardSetupAfterCloseAdmitsFreshIdentity(t *testing.T) {
	f := newStewardCLI(t)
	ctx := context.Background()
	first, err := setupSteward(ctx, f.deps(), f.e, f.c, f.task.ID, f.dir, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.c.CloseAgent(ctx, f.task.ID, first.ID, first.RunID); err != nil {
		t.Fatal(err)
	}
	second, err := setupSteward(ctx, f.deps(), f.e, f.c, f.task.ID, "", "", "")
	if err != nil || second.ID == first.ID || second.Name != first.Name || second.Cwd != f.dir {
		t.Fatalf("setup after close: %+v %v", second, err)
	}
	saved, err := loadStewardIdentity(f.c.Base, f.task.ID)
	if err != nil || saved.AgentID != second.ID {
		t.Fatalf("identity file after fresh setup: %+v %v", saved, err)
	}
}

func TestStewardSetupRefusedInsideAgentSession(t *testing.T) {
	err := cmdStewardSetup(env{hub: "http://127.0.0.1:1", task: "tsk_0000000000000001", agent: "agt_0000000000000001"}, []string{"--task", "tsk_0000000000000001", "--cwd", t.TempDir()}, stewardDeps{})
	if err == nil || !strings.Contains(err.Error(), "owner command") {
		t.Fatalf("agent-session setup: %v", err)
	}
}
