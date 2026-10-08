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
	"strconv"
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

// executions waits until the fake tt has run exactly n times, bounded by
// handlerLaunchWait because tmux starts it after the launch has returned. It
// fails at once on more than n, so an extra launch is never waited out.
func (f *stewardCLI) executions(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(handlerLaunchWait)
	for {
		data, _ := os.ReadFile(f.marker)
		if len(data) == n {
			return
		}
		if len(data) > n || !time.Now().Before(deadline) {
			t.Fatalf("expected %d launches, got %q", n, data)
		}
		time.Sleep(10 * time.Millisecond)
	}
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
	return stewardDeps{template: fixtureTemplate, spawn: cmdSpawn, cleanup: productionTeamRunner().cleanup, host: spawn.Host, online: 3 * time.Second, poll: 20 * time.Millisecond}
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

func (f *stewardCLI) saveSummary(t *testing.T, as api.Agent, revision int64, body, key string) {
	t.Helper()
	file := filepath.Join(t.TempDir(), "summary.md")
	if err := os.WriteFile(file, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	e := env{hub: f.c.Base, task: f.task.ID, agent: as.ID, agentName: as.Name, runID: as.RunID}
	if _, err := captureStdout(t, func() error {
		return cmdStewardSummary(e, []string{"set", "--revision", strconv.FormatInt(revision, 10), "--body-file", file, "--request-id", key})
	}); err != nil {
		t.Fatal(err)
	}
}

func (f *stewardCLI) brief(t *testing.T, a api.Agent) string {
	t.Helper()
	out, err := captureStdout(t, func() error {
		return cmdBrief(env{hub: f.c.Base, task: f.task.ID, agent: a.ID, agentName: a.Name})
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestStewardSummaryBriefingNamesLatestRevision(t *testing.T) {
	f := newStewardCLI(t)
	steward := f.steward(t)
	if own := f.brief(t, steward); !strings.Contains(own, "No backlog summary is saved yet") {
		t.Fatalf("briefing without a summary:\n%s", own)
	}
	f.saveSummary(t, steward, 0, "## Themes\nsynthetic", "summary-r1")
	f.saveSummary(t, steward, 1, "## Themes\nsynthetic, revised", "summary-r2")
	if own := f.brief(t, steward); !strings.Contains(own, "The latest backlog summary is revision 2. Read it first with tt steward summary get") {
		t.Fatalf("briefing with a summary:\n%s", own)
	}
	out, err := captureStdout(t, func() error {
		return cmdStewardSummary(env{hub: f.c.Base, task: f.task.ID}, []string{"get", "--revision", "1"})
	})
	if err != nil || !strings.Contains(out, "Backlog summary revision 1") || !strings.Contains(out, "## Themes\nsynthetic\n") {
		t.Fatalf("summary get: %q %v", out, err)
	}
	out, err = captureStdout(t, func() error { return cmdStewardSummary(env{hub: f.c.Base, task: f.task.ID}, []string{"history"}) })
	if err != nil || !strings.Contains(out, "r1 ") || !strings.Contains(out, "r2 ") || !strings.Contains(out, "by "+steward.ID) {
		t.Fatalf("summary history: %q %v", out, err)
	}
}

// pauseAndResume pauses the project with every open agent as a target,
// confirms cleanup, resumes it with a fresh planned lead and confirms that
// lead's launch, as project_pause_test.go does.
func (f *stewardCLI) pauseAndResume(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	by := api.Caller{Node: "fixture", User: "owner"}
	agents, err := f.c.ListAgents(ctx, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	var targets []api.ProjectPauseTargetRequest
	for _, a := range agents {
		if a.Status != api.AgentClosed {
			targets = append(targets, api.ProjectPauseTargetRequest{AgentID: a.ID, RunID: a.RunID, ServiceDisposition: api.PauseServiceNone})
		}
	}
	task, err := f.st.GetTask(ctx, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.st.PauseProject(ctx, f.task.ID, api.PauseProjectRequest{Version: 1, RequestID: "pause-" + api.NewID("req"), ExpectedLifecycleGeneration: task.LifecycleGeneration, Targets: targets}, by); err != nil {
		t.Fatal(err)
	}
	for _, target := range targets {
		if _, err = f.st.ReportCleanup(ctx, target.AgentID, api.CleanupRequest{RunID: target.RunID}, by); err != nil {
			t.Fatal(err)
		}
	}
	status, err := f.st.ProjectPauseStatus(ctx, f.task.ID)
	if err != nil || status.State != api.ProjectPausePaused {
		t.Fatalf("pause status: %+v %v", status, err)
	}
	lead := api.ProjectResumeOrchestrator{AgentID: api.NewID("agt"), RunID: api.NewID("run"), Name: "fresh-lead"}
	resumed, err := f.st.ResumeProject(ctx, f.task.ID, api.ResumeProjectRequest{Version: 1, RequestID: "resume-" + api.NewID("req"), ExpectedPauseGeneration: status.PauseGeneration,
		ExpectedLifecycleGeneration: status.LifecycleGeneration, RetainedHandoffDigest: status.RetainedHandoffDigest, SelectedTeamID: "synthetic-team", Orchestrator: lead}, by)
	if err != nil || resumed.Receipt == nil {
		t.Fatalf("resume: %+v %v", resumed, err)
	}
	fresh, err := f.st.AddAgent(ctx, f.task.ID, api.AddAgentRequest{AgentID: lead.AgentID, ExpectedRunID: lead.RunID, ResumeReceiptID: resumed.Receipt.ID, Name: lead.Name,
		Host: "fixture", Session: "fresh-lead", Runtime: "claude", ExpectedLifecycleGeneration: resumed.LifecycleGeneration}, by)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.st.ConfirmProjectResume(ctx, f.task.ID, api.ConfirmProjectResumeRequest{Version: 1, RequestID: "confirm-" + api.NewID("req"), ExpectedLifecycleGeneration: resumed.LifecycleGeneration,
		ResumeReceiptID: resumed.Receipt.ID, AgentID: fresh.ID, RunID: fresh.RunID}, by); err != nil {
		t.Fatal(err)
	}
}

func TestStewardSummaryPauseResumeSetupAdmitsFreshIdentity(t *testing.T) {
	f := newStewardCLI(t)
	ctx := context.Background()
	first := f.steward(t)
	f.saveSummary(t, first, 0, "## Themes\nkept across the pause", "summary-before-pause")
	f.pauseAndResume(t)
	if a, err := f.c.GetAgent(ctx, f.task.ID, first.ID); err != nil || a.Status != api.AgentClosed {
		t.Fatalf("steward after pause: %+v %v", a, err)
	}
	second, err := setupSteward(ctx, f.deps(), f.e, f.c, f.task.ID, "", "", "")
	if err != nil {
		t.Fatalf("setup after resume: %v", err)
	}
	if second.ID == first.ID || second.Name != first.Name || second.Role != api.AgentRoleBacklogSteward {
		t.Fatalf("setup after resume reused the closed identity: %+v", second)
	}
	f.live(t, second)
	if own := f.brief(t, second); !strings.Contains(own, "The latest backlog summary is revision 1.") {
		t.Fatalf("resumed steward briefing:\n%s", own)
	}
}

// Steward rotation CLI tests (wi_5b4b94dbc9a11e8b, order #14942), on the
// isolated hub, private tmux socket and fake tt of newStewardCLI.

// rotationDeps launch the successor through the real cmdSpawn and
// ensureHandler and, unless offline, mark it started and idle, as a live
// Claude session's hooks would. Cleanup calls are counted.
func (f *stewardCLI) rotationDeps(t *testing.T, offline bool, cleanups *int) stewardDeps {
	d := f.deps()
	d.spawn = func(e env, args []string) error {
		if err := cmdSpawn(e, args); err != nil {
			return err
		}
		if offline {
			return nil
		}
		for i := 0; i+1 < len(args); i++ {
			if args[i] == "--agent-id" {
				a, err := f.c.GetAgent(context.Background(), f.task.ID, args[i+1])
				if err != nil {
					return err
				}
				f.live(t, a)
			}
		}
		return nil
	}
	cleanup := d.cleanup
	d.cleanup = func(ctx context.Context, e env, task, agent string) error {
		*cleanups++
		return cleanup(ctx, e, task, agent)
	}
	return d
}

// rotationSetup provisions a steward with a saved summary and gives it one
// role request and one by-name question from a worker.
func (f *stewardCLI) rotationSetup(t *testing.T, summary bool) (api.Agent, api.Agent) {
	t.Helper()
	steward := f.steward(t)
	// Count the old steward's launch before a rotation can stop its session.
	f.executions(t, 1)
	worker := f.agent(t, "builder", "")
	if summary {
		f.saveSummary(t, steward, 0, "## Themes\nsynthetic backlog", "summary-r1")
	}
	e := env{hub: f.c.Base, task: f.task.ID, agent: worker.ID, agentName: worker.Name, runID: worker.RunID}
	if _, err := captureStdout(t, func() error {
		return cmdSend(e, []string{"--kind", "request", "--to", "role:backlog_steward", "--subject", "Draft this synthetic intake", "--ask", "Please research it."})
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := captureStdout(t, func() error {
		return cmdSend(e, []string{"--kind", "question", "--to", steward.Name, "--subject", "Which batch holds the scroll items", "--question", "Which batch?"})
	}); err != nil {
		t.Fatal(err)
	}
	return steward, worker
}

func (f *stewardCLI) rotate(t *testing.T, d stewardDeps) (api.StewardRotation, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return rotateSteward(ctx, d, f.e, f.c, f.task.ID, api.StewardRotationReasonManual, api.StewardRotationTriggerOwner)
}

func (f *stewardCLI) stewards(t *testing.T) (open, total int) {
	t.Helper()
	agents, err := f.c.ListAgents(context.Background(), f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range agents {
		if a.Role == api.AgentRoleBacklogSteward {
			total++
			if a.Status != api.AgentClosed {
				open++
			}
		}
	}
	return open, total
}

func TestStewardRotationOwnerRotationEndToEnd(t *testing.T) {
	f := newStewardCLI(t)
	ctx := context.Background()
	cleanups := 0
	d := f.rotationDeps(t, false, &cleanups)
	steward, worker := f.rotationSetup(t, false)
	// Prepare refusals change nothing.
	if _, err := f.rotate(t, d); err == nil || !strings.Contains(err.Error(), api.StewardRefusedSummaryMissing) {
		t.Fatalf("rotation without a summary: %v", err)
	}
	f.saveSummary(t, steward, 0, "## Themes\nsynthetic backlog", "summary-r1")
	now := time.Now().UTC()
	for _, state := range []string{"working", "idle"} {
		if state == "idle" {
			break
		}
		if _, err := f.c.ReportActivity(ctx, f.task.ID, steward.ID, api.ActivityReport{RequestID: api.NewID("act"), RunID: steward.RunID, Activity: api.AgentActivity{State: state, ObservedAt: now, LastEventAt: now}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.rotate(t, d); !stewardRotationBusy(err) {
		t.Fatalf("rotation while working: %v", err)
	}
	f.live(t, steward)
	if _, err := os.Stat(stewardRotationJournalPath(f.c.Base, f.task.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused prepare left a journal: %v", err)
	}
	// Watch the pending phase: the old steward still holds the slot and role.
	d.after = func(phase string) error {
		if phase != rotationPhaseSpawned {
			return nil
		}
		status, err := f.c.BacklogSteward(ctx, f.task.ID)
		if err != nil || status.Steward == nil || status.Steward.ID != steward.ID || status.PendingSuccessorID == "" {
			t.Errorf("status while the successor is pending: %+v %v", status, err)
		}
		return nil
	}
	r, err := f.rotate(t, d)
	if err != nil {
		t.Fatal(err)
	}
	if r.State != api.StewardRotationCommitted || r.SuccessorName != "backlog-steward-r2" || r.Handoff == nil || r.Handoff.SummaryRevision != 1 || r.Handoff.SummaryDigest == "" ||
		r.Receipt == nil || r.Receipt.Reissued != 2 || cleanups != 1 {
		t.Fatalf("rotation: %+v cleanups=%d", r, cleanups)
	}
	old, err := f.c.GetAgent(ctx, f.task.ID, steward.ID)
	if err != nil || old.Status != api.AgentClosed || !old.CleanupDone {
		t.Fatalf("old steward after rotation: %+v %v", old, err)
	}
	if open, _ := f.stewards(t); open != 1 {
		t.Fatalf("open stewards after rotation: %d", open)
	}
	successor, err := f.c.GetAgent(ctx, f.task.ID, r.SuccessorAgentID)
	if err != nil || successor.Status == api.AgentClosed {
		t.Fatal(err)
	}
	open, err := f.c.ListObligations(ctx, f.task.ID, successor.ID, "", true, false)
	notices := 0
	for _, o := range open {
		if o.SourceKind == api.EnvelopeKindNotice {
			notices++
		}
	}
	if err != nil || len(open) != 3 || notices != 1 {
		t.Fatalf("successor obligations: %+v %v", open, err)
	}
	msgs, err := f.c.ListMessages(ctx, f.task.ID, 0, successor.ID, 200)
	if err != nil {
		t.Fatal(err)
	}
	handoffNotice := false
	for _, m := range msgs {
		if m.Seq == r.Receipt.NoticeSeq && strings.Contains(m.Text, r.ID) && strings.Contains(m.Text, "summary revision 1") {
			handoffNotice = true
		}
	}
	if !handoffNotice {
		t.Fatal("no directed handoff notice names the rotation and summary revision")
	}
	saved, err := loadStewardIdentity(f.c.Base, f.task.ID)
	if err != nil || saved.AgentID != successor.ID || saved.Name != successor.Name || saved.RotationID != r.ID {
		t.Fatalf("identity file after rotation: %+v %v", saved, err)
	}
	if _, err = os.Stat(stewardRotationJournalPath(f.c.Base, f.task.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("journal left after rotation: %v", err)
	}
	// A later setup reuses the successor and admits nothing new.
	_, total := f.stewards(t)
	again, err := setupSteward(ctx, f.deps(), f.e, f.c, f.task.ID, "", "", "")
	if err != nil || again.ID != successor.ID {
		t.Fatalf("setup after rotation: %+v %v", again, err)
	}
	if _, after := f.stewards(t); after != total {
		t.Fatalf("setup after rotation admitted a steward: %d -> %d", total, after)
	}
	// Intake now reaches the successor.
	e := env{hub: f.c.Base, task: f.task.ID, agent: worker.ID, agentName: worker.Name, runID: worker.RunID}
	if _, err = captureStdout(t, func() error {
		return cmdSend(e, []string{"--kind", "request", "--to", "role:backlog_steward", "--subject", "Intake after the rotation", "--ask", "Please research it."})
	}); err != nil {
		t.Fatal(err)
	}
	if open, _ = f.c.ListObligations(ctx, f.task.ID, successor.ID, "", true, false); len(open) != 4 {
		t.Fatalf("successor obligations after new intake: %d", len(open))
	}
}

// A steward running on Claude rotates onto the real embedded template
// (wi_4e85f423a1078a52, a4): the hub refuses a successor on another runtime.
func TestStewardRotationOntoEmbeddedTemplateKeepsClaudeRuntime(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node unavailable")
	}
	f := newStewardCLI(t)
	ctx := context.Background()
	cleanups := 0
	d := f.rotationDeps(t, false, &cleanups)
	steward, _ := f.rotationSetup(t, true)
	if steward.Runtime != "claude" {
		t.Fatalf("fixture steward runtime: %q", steward.Runtime)
	}
	d.template = loadStewardTemplate
	tmpl, err := loadStewardTemplate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	r, err := f.rotate(t, d)
	if err != nil {
		t.Fatalf("rotation onto the embedded %s template: %v", tmpl.Runtime, err)
	}
	if r.State != api.StewardRotationCommitted || cleanups != 1 {
		t.Fatalf("rotation: %+v cleanups=%d", r, cleanups)
	}
	successor, err := f.c.GetAgent(ctx, f.task.ID, r.SuccessorAgentID)
	if err != nil || successor.Status == api.AgentClosed || successor.Runtime != "claude" || successor.Runtime != tmpl.Runtime {
		t.Fatalf("successor: %+v %v", successor, err)
	}
	if open, _ := f.stewards(t); open != 1 {
		t.Fatalf("open stewards after rotation: %d", open)
	}
}

func TestStewardRotationResumeAfterEachJournalPhase(t *testing.T) {
	for _, stop := range []string{rotationPhasePreparing, rotationPhasePrepared, rotationPhaseSpawned, rotationPhaseCommitted, stewardPhaseCleaned} {
		t.Run(stop, func(t *testing.T) {
			f := newStewardCLI(t)
			ctx := context.Background()
			cleanups := 0
			d := f.rotationDeps(t, false, &cleanups)
			f.rotationSetup(t, true)
			d.after = func(phase string) error {
				if phase == stop {
					return errors.New("interrupted at " + phase)
				}
				return nil
			}
			if _, err := f.rotate(t, d); err == nil || !strings.Contains(err.Error(), "interrupted at "+stop) {
				t.Fatalf("interrupted rotation: %v", err)
			}
			d.after = nil
			r, err := f.rotate(t, d)
			if err != nil || r.State != api.StewardRotationCommitted {
				t.Fatalf("resumed rotation: %+v %v", r, err)
			}
			open, total := f.stewards(t)
			if open != 1 || total != 2 {
				t.Fatalf("stewards after resume: open %d total %d", open, total)
			}
			f.executions(t, 2) // the old steward and exactly one successor
			moved, err := f.c.ListObligations(ctx, f.task.ID, r.SuccessorAgentID, "", true, false)
			if err != nil || len(moved) != 3 || r.Receipt.Reissued != 2 {
				t.Fatalf("obligations after resume: %d, receipt %+v, %v", len(moved), r.Receipt, err)
			}
			if cleanups != 1 {
				t.Fatalf("old session cleanups: %d", cleanups)
			}
		})
	}
}

func TestStewardRotationLaunchFailureKeepsOldStewardThenAbort(t *testing.T) {
	f := newStewardCLI(t)
	ctx := context.Background()
	cleanups := 0
	steward, _ := f.rotationSetup(t, true)
	before, err := f.c.ListObligations(ctx, f.task.ID, steward.ID, "", true, false)
	if err != nil {
		t.Fatal(err)
	}
	identityBefore, _ := os.ReadFile(stewardIdentityPath(f.c.Base, f.task.ID))
	// The successor spawn fails outright.
	d := f.rotationDeps(t, false, &cleanups)
	d.spawn = func(env, []string) error { return errors.New("synthetic launch failure") }
	if _, err = f.rotate(t, d); err == nil || !strings.Contains(err.Error(), "stays prepared") {
		t.Fatalf("failed launch: %v", err)
	}
	rotations, err := f.c.ListStewardRotations(ctx, f.task.ID)
	if err != nil || len(rotations) != 1 || rotations[0].State != api.StewardRotationPrepared {
		t.Fatalf("rotation after failed launch: %+v %v", rotations, err)
	}
	if _, err = f.c.StewardRotationAction(ctx, f.task.ID, api.StewardRotationRequest{Operation: api.StewardRotationCommit, RequestID: "commit-probe", RotationID: rotations[0].ID}); !stewardRotationCode(err, api.StewardRefusedSuccessor) {
		t.Fatalf("commit without a successor: %v", err)
	}
	status, err := f.c.BacklogSteward(ctx, f.task.ID)
	if err != nil || status.Steward == nil || status.Steward.ID != steward.ID {
		t.Fatalf("steward after failed launch: %+v %v", status, err)
	}
	after, _ := f.c.ListObligations(ctx, f.task.ID, steward.ID, "", true, false)
	if len(after) != len(before) {
		t.Fatalf("old steward's obligations changed: %d -> %d", len(before), len(after))
	}
	aborted, err := abortStewardRotation(ctx, d, f.e, f.c, f.task.ID)
	if err != nil || aborted.State != api.StewardRotationAborted {
		t.Fatalf("abort: %+v %v", aborted, err)
	}
	if identityAfter, _ := os.ReadFile(stewardIdentityPath(f.c.Base, f.task.ID)); string(identityAfter) != string(identityBefore) {
		t.Fatal("abort rewrote the setup identity file")
	}
	// The successor launches but never comes online.
	d = f.rotationDeps(t, true, &cleanups)
	d.online = 300 * time.Millisecond
	if _, err = f.rotate(t, d); err == nil || !strings.Contains(err.Error(), "did not come online") {
		t.Fatalf("offline successor: %v", err)
	}
	rotations, _ = f.c.ListStewardRotations(ctx, f.task.ID)
	pending := rotations[len(rotations)-1]
	if pending.State != api.StewardRotationPrepared {
		t.Fatalf("rotation with an offline successor: %+v", pending)
	}
	if _, err = f.c.StewardRotationAction(ctx, f.task.ID, api.StewardRotationRequest{Operation: api.StewardRotationCommit, RequestID: "commit-offline", RotationID: pending.ID}); !stewardRotationCode(err, api.StewardRefusedSuccessor) {
		t.Fatalf("commit with an offline successor: %v", err)
	}
	if status, _ = f.c.BacklogSteward(ctx, f.task.ID); status.Steward == nil || status.Steward.ID != steward.ID {
		t.Fatalf("the offline successor took the role: %+v", status)
	}
	if _, err = abortStewardRotation(ctx, d, f.e, f.c, f.task.ID); err != nil {
		t.Fatal(err)
	}
	gone, err := f.c.GetAgent(ctx, f.task.ID, pending.SuccessorAgentID)
	if err != nil || gone.Status != api.AgentClosed || !gone.CleanupDone || cleanups != 1 {
		t.Fatalf("pending successor after abort: %+v cleanups=%d %v", gone, cleanups, err)
	}
	// A new rotation succeeds; there was never more or fewer than one holder.
	d = f.rotationDeps(t, false, &cleanups)
	if r, err := f.rotate(t, d); err != nil || r.State != api.StewardRotationCommitted {
		t.Fatalf("rotation after abort: %+v %v", r, err)
	}
	if open, _ := f.stewards(t); open != 1 {
		t.Fatalf("open stewards: %d", open)
	}
}

func stewardRotationCode(err error, code string) bool {
	var httpErr *api.HTTPError
	return errors.As(err, &httpErr) && httpErr.Status == http.StatusConflict && httpErr.Code == code
}

func TestStewardRotationAgentCallerRefused(t *testing.T) {
	f := newStewardCLI(t)
	ctx := context.Background()
	steward, worker := f.rotationSetup(t, true)
	e := env{hub: f.c.Base, task: f.task.ID, agent: worker.ID, agentName: worker.Name, runID: worker.RunID}
	for _, args := range [][]string{{"rotate", "--task", f.task.ID}, {"rotate", "--task", f.task.ID, "--abort"}, {"setup", "--task", f.task.ID, "--cwd", f.dir},
		{"policy", "set", "--task", f.task.ID, "--revision", "0", "--enabled=false"}} {
		if err := cmdSteward(e, args); err == nil || !strings.Contains(err.Error(), "owner command") {
			t.Fatalf("%v from an agent session: %v", args, err)
		}
	}
	if _, err := f.c.StewardRotationAction(ctx, f.task.ID, api.StewardRotationRequest{Operation: api.StewardRotationPrepare, RequestID: "agent-prepare", ActorAgentID: steward.ID}); !stewardRotationCode(err, api.StewardRefusedAgentCaller) {
		t.Fatalf("agent caller over HTTP: %v", err)
	}
	if rotations, _ := f.c.ListStewardRotations(ctx, f.task.ID); len(rotations) != 0 {
		t.Fatalf("an agent caller created a rotation: %+v", rotations)
	}
}

func TestStewardRotationPolicyCLI(t *testing.T) {
	f := newStewardCLI(t)
	out, err := captureStdout(t, func() error { return cmdSteward(env{hub: f.c.Base}, []string{"policy", "get", "--task", f.task.ID}) })
	if err != nil || !strings.Contains(out, "(revision 0): enabled=true max-total-tokens=300000000 on-template-change=true") {
		t.Fatalf("policy get: %q %v", out, err)
	}
	if err = cmdSteward(env{hub: f.c.Base}, []string{"policy", "set", "--task", f.task.ID, "--revision", "3", "--max-total-tokens", "5000"}); !stewardRotationCode(err, api.StewardRefusedStalePolicy) {
		t.Fatalf("stale policy revision: %v", err)
	}
	out, err = captureStdout(t, func() error {
		return cmdSteward(env{hub: f.c.Base}, []string{"policy", "set", "--task", f.task.ID, "--revision", "0", "--max-total-tokens", "5000"})
	})
	if err != nil || !strings.Contains(out, "(revision 1): enabled=true max-total-tokens=5000 on-template-change=true") {
		t.Fatalf("policy set: %q %v", out, err)
	}
}

func TestStewardRotationTickNeedsLocalStewardSessionAndSpacesRequests(t *testing.T) {
	f := newStewardCLI(t)
	cleanups := 0
	clock := time.Now()
	runner := &stewardRotationRunner{deps: f.rotationDeps(t, false, &cleanups), now: func() time.Time { return clock }, interval: time.Minute}
	client := func() (*api.Client, error) { return f.c, nil }
	ctx := context.Background()
	host := spawn.Host()
	// No tt-steward-* session for this hub runs here: no request at all.
	if err := runner.hostTick(ctx, f.e, client, host); err != nil || f.calls.due.Load() != 0 {
		t.Fatalf("tick without a steward session: %v due=%d", err, f.calls.due.Load())
	}
	f.rotationSetup(t, true)
	if err := runner.hostTick(ctx, f.e, client, host); err != nil || f.calls.due.Load() != 1 {
		t.Fatalf("first tick: %v due=%d", err, f.calls.due.Load())
	}
	clock = clock.Add(30 * time.Second)
	if err := runner.hostTick(ctx, f.e, client, host); err != nil || f.calls.due.Load() != 1 {
		t.Fatalf("second tick within a minute: %v due=%d", err, f.calls.due.Load())
	}
	if f.calls.actions.Load() != 0 {
		t.Fatal("the runner rotated a current, under-limit steward")
	}
	// A template change makes the idle steward due; the runner rotates it.
	runner.digest = strings.Repeat("e", 64)
	clock = clock.Add(time.Minute)
	if err := runner.hostTick(ctx, f.e, client, host); err != nil || f.calls.due.Load() != 2 {
		t.Fatalf("tick after the interval: %v due=%d", err, f.calls.due.Load())
	}
	rotations, err := f.c.ListStewardRotations(ctx, f.task.ID)
	if err != nil || len(rotations) != 1 || rotations[0].State != api.StewardRotationCommitted || rotations[0].Trigger != api.StewardRotationTriggerRunner || rotations[0].Reason != api.StewardRotationReasonTemplate {
		t.Fatalf("runner rotation: %+v %v", rotations, err)
	}
}

// Review round 1, b2: prepare, spawn failure, pause, resume, rerun rotate.
// The rerun admits no successor and aborts the stale rotation; setup then
// admits exactly one steward.
func TestStewardRotationResumeAfterPauseAbortsStaleRotation(t *testing.T) {
	f := newStewardCLI(t)
	ctx := context.Background()
	cleanups := 0
	old, _ := f.rotationSetup(t, true)
	d := f.rotationDeps(t, false, &cleanups)
	d.spawn = func(env, []string) error { return errors.New("synthetic launch failure") }
	if _, err := f.rotate(t, d); err == nil || !strings.Contains(err.Error(), "stays prepared") {
		t.Fatalf("failed launch: %v", err)
	}
	journal, err := loadStewardRotationJournal(f.c.Base, f.task.ID)
	if err != nil || journal == nil || journal.Phase != rotationPhasePrepared {
		t.Fatalf("journal after failed launch: %+v %v", journal, err)
	}
	f.pauseAndResume(t)
	d = f.rotationDeps(t, false, &cleanups)
	r, err := f.rotate(t, d)
	if err == nil || !strings.Contains(err.Error(), "was stale") || r.State != api.StewardRotationAborted || r.ID != journal.RotationID {
		t.Fatalf("rerun after pause: %+v %v", r, err)
	}
	if _, err = f.c.GetAgent(ctx, f.task.ID, journal.SuccessorAgentID); err == nil {
		t.Fatal("the stale rotation's successor was admitted")
	}
	if _, err = os.Stat(stewardRotationJournalPath(f.c.Base, f.task.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("journal after the stale abort: %v", err)
	}
	steward, err := setupSteward(ctx, f.deps(), f.e, f.c, f.task.ID, "", "", "")
	if err != nil || steward.ID == old.ID || steward.ID == journal.SuccessorAgentID {
		t.Fatalf("setup after the stale rotation: %+v %v", steward, err)
	}
	if open, _ := f.stewards(t); open != 1 {
		t.Fatalf("open stewards after setup: %d", open)
	}
}
