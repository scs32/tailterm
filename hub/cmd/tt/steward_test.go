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

func (f *stewardCLI) agent(t *testing.T, name, role string) api.Agent {
	t.Helper()
	req := api.AddAgentRequest{Name: name, Host: "fixture", Session: name, Runtime: "claude", Cwd: f.dir}
	if role != "" {
		req.Role, req.AgentID = role, api.NewID("agt")
	}
	a, err := f.c.AddAgent(context.Background(), f.task.ID, req)
	if err != nil {
		t.Fatal(err)
	}
	f.live(t, a)
	return a
}

func (f *stewardCLI) steward(t *testing.T) api.Agent {
	t.Helper()
	a, err := setupSteward(context.Background(), f.deps(), f.e, f.c, f.task.ID, f.dir, "", "")
	if err != nil {
		t.Fatal(err)
	}
	f.live(t, a)
	return a
}

func TestStewardRoleRecipientThroughTTSend(t *testing.T) {
	f := newStewardCLI(t)
	ctx := context.Background()
	// A parallel project (no fixed cap) needs a host policy first.
	if _, err := f.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: api.NewID("tqr"), Operation: "set_host_policy", Host: "fixture", HostPolicyVersion: 1,
		HostPolicyExpires: time.Now().Add(time.Hour).UTC().Format(time.RFC3339), HostMaxSessions: 100, HostMaxPolling: 10, LimiterDomain: "https://fixture.invalid",
		HostMaxRelayBindings: 100, HostMaxRequestsPerMinute: 100000, HostMaxBurst: 10000, HostHeadroomPercent: 20}); err != nil {
		t.Fatal(err)
	}
	free := int64(1 << 20)
	if _, err := f.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: api.NewID("tqr"), Operation: "observe_host", Host: "fixture", HostUsage: &api.TeamHostUsage{Host: "fixture",
		LimiterDomain: "https://fixture.invalid", PolicyVersion: 1, ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), RelayBindings: 1, Complete: true,
		SourceDigest: strings.Repeat("a", 64), FreeDiskMiB: &free}}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: api.NewID("tqr"), Operation: "set_limit", Host: "fixture", ConcurrencyLimit: 0}); err != nil {
		t.Fatal(err)
	}
	if list, err := f.c.ListTeamQueue(ctx, f.task.ID); err != nil || list.ConcurrencyLimit != 0 {
		t.Fatalf("fixture is not parallel: %+v %v", list.ConcurrencyLimit, err)
	}
	worker := f.agent(t, "builder", "")
	e := env{hub: f.c.Base, task: f.task.ID, agent: worker.ID, agentName: worker.Name, runID: worker.RunID}
	send := func(subject string) error {
		return cmdSend(e, []string{"--kind", "request", "--to", "role:backlog_steward", "--subject", subject, "--ask", "Please research and draft this synthetic intake."})
	}
	if err := send("Intake before the steward exists"); err == nil || !strings.Contains(err.Error(), "no running agent holds it") {
		t.Fatalf("send without a steward: %v", err)
	}
	steward := f.steward(t)
	if _, err := captureStdout(t, func() error { return send("Intake outside the bound item") }); err != nil {
		t.Fatal(err)
	}
	open, err := f.c.ListObligations(ctx, f.task.ID, steward.ID, "", true, false)
	if err != nil || len(open) != 1 || open[0].AgentID != steward.ID {
		t.Fatalf("steward obligations: %+v %v", open, err)
	}
}

func TestStewardBriefingNamesStewardForAgentsAndHandler(t *testing.T) {
	f := newStewardCLI(t)
	handler := f.agent(t, "db-handler", api.AgentRoleDatabaseHandler)
	worker := f.agent(t, "builder", "")
	brief := func(a api.Agent) string {
		t.Helper()
		out, err := captureStdout(t, func() error {
			return cmdBrief(env{hub: f.c.Base, task: f.task.ID, agent: a.ID, agentName: a.Name})
		})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	baseWorker, baseHandler := brief(worker), brief(handler)
	// Without a steward, both briefings are byte-identical to the base emitter.
	detail, err := f.c.GetTask(context.Background(), f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	agents := primaryHandlerFirst(detail.Task, detail.Agents)
	if baseWorker != agentTaskBriefingForHandler(detail.Task, worker.Name, "", selfPath(), agents, 0, false) ||
		baseHandler != agentTaskBriefingForHandler(detail.Task, handler.Name, api.AgentRoleDatabaseHandler, selfPath(), agents, 0, false) {
		t.Fatal("briefings without a steward differ from the base")
	}
	if strings.Contains(baseWorker, "Backlog steward") || strings.Contains(baseHandler, "Backlog steward") {
		t.Fatal("a briefing names a steward that does not exist")
	}
	digestBefore := handlerTemplateDigest("handler assignment")
	steward := f.steward(t)
	withWorker, withHandler := brief(worker), brief(handler)
	sb := stewardBriefing{Active: steward.Name}
	if withWorker != baseWorker+sb.agentLine() {
		t.Fatalf("worker briefing with a steward:\n%s", strings.TrimPrefix(withWorker, baseWorker))
	}
	if !strings.Contains(withWorker, "Send new bug/feature intake and follow-ups you find outside your bound item to it with tt send --to role:backlog_steward") ||
		!strings.Contains(withWorker, "Scope changes to your bound item stay with your handler") {
		t.Fatal("worker briefing does not route intake to the steward")
	}
	if withHandler != baseHandler+sb.handlerLine() || !strings.Contains(withHandler, "with the original owner message as --source-seq") {
		t.Fatalf("handler briefing with a steward:\n%s", strings.TrimPrefix(withHandler, baseHandler))
	}
	// The steward text sits outside the handler template: its digest and the
	// runner's template check do not change.
	if handlerTemplateDigest("handler assignment") != digestBefore || !strings.Contains(withHandler, primaryHandlerGuidance+sb.handlerLine()) {
		t.Fatal("the steward changed the handler template")
	}
	due := api.HandlerRotationDue{RecordedDigest: digestBefore, Policy: api.HandlerRotationPolicy{OnTemplateChange: true}}
	spec := handlerSpec{Args: []string{"--prompt", "handler assignment"}}
	if reasons := rotationDueReasons(due, &spec); len(reasons) != 0 {
		t.Fatalf("a steward's arrival made handler rotation due: %v", reasons)
	}
	// The steward's own briefing is its branch: direct reads, handler writes.
	own := brief(steward)
	if !strings.Contains(own, backlogStewardGuidance) || !strings.Contains(own, "This project's primary Database handler is db-handler") ||
		strings.Contains(own, "Do not use tt work-items") || strings.Contains(own, "ALL agent work-item database reads and writes") {
		t.Fatalf("steward briefing branch:\n%s", own)
	}
}

// The store test files a real review follow-up; here an item with the same
// hub-written description checks the CLI's steward access and printer.
func TestTriageHeldForTriageCLIAsSteward(t *testing.T) {
	f := newStewardCLI(t)
	ctx := context.Background()
	steward := f.steward(t)
	held, err := f.c.CreateWorkItem(ctx, f.task.ID, api.CreateWorkItemRequest{Kind: "bug", Title: "Refuse an empty recipient in the review fixture", Priority: "normal",
		Description: "Review follow-up from wi_0000000000000001, message #7. Held for triage.\n{\"id\":\"f1\"}", RequestID: api.NewID("req")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.c.CreateWorkItem(ctx, f.task.ID, api.CreateWorkItemRequest{Kind: "bug", Title: "An ordinary open item", Priority: "normal", RequestID: api.NewID("req")}); err != nil {
		t.Fatal(err)
	}
	e := env{hub: f.c.Base, task: f.task.ID, agent: steward.ID, agentName: steward.Name, runID: steward.RunID}
	out, err := captureStdout(t, func() error { return cmdWorkItems(e, []string{"triage", "--project", f.task.ID}) })
	if err != nil || !strings.Contains(out, "Review follow-ups held for triage (1):") || !strings.Contains(out, held.ID) {
		t.Fatalf("steward triage text: %q %v", out, err)
	}
	out, err = captureStdout(t, func() error { return cmdWorkItems(e, []string{"triage", "--project", f.task.ID, "--json"}) })
	var got api.WorkItemTriage
	if err != nil || json.Unmarshal([]byte(out), &got) != nil || len(got.HeldForTriage) != 1 || got.HeldForTriage[0].Item.ID != held.ID {
		t.Fatalf("steward triage JSON: %q %v", out, err)
	}
	// A stale steward run is refused.
	stale := e
	stale.runID = api.NewID("run")
	if _, err = captureStdout(t, func() error { return cmdWorkItems(stale, []string{"triage", "--project", f.task.ID}) }); err == nil {
		t.Fatal("a stale steward run read triage")
	}
}
