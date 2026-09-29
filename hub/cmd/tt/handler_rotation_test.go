package main

import (
	"context"
	"database/sql"
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

// Handler rotation CLI tests (wi_611e4d3c6992a664, order #11573): an isolated
// SQLite hub behind httptest, a private tmux socket, a temporary relay state
// directory and HOME, and a fake tt that only sleeps. Successors launch through
// the real ensureHandler; the fake spawn stands in for cmdSpawn's process.
type rotationCLI struct {
	st        *store.Store
	dbPath    string
	c         *api.Client
	e         env
	task      api.Task
	dir       string
	opts      spawn.Options
	marker    string
	old       api.Agent
	worker    api.Agent
	spec      handlerSpec
	deps      rotationDeps
	online    bool
	spawns    int
	spawnArgs [][]string
	briefings []string
	dueCalls  atomic.Int32
	actions   atomic.Int32
}

func newRotationCLI(t *testing.T, oldDigest string) *rotationCLI {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux unavailable")
	}
	t.Setenv("TT_TMUX_SOCKET", "tt-rotation-test-"+api.NewID("agt"))
	t.Setenv("TAILTERM_RELAY_STATE", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Cleanup(func() { _, _ = startupTmux(context.Background(), "kill-server") })
	f := &rotationCLI{online: true, dbPath: filepath.Join(t.TempDir(), "hub.sqlite")}
	var err error
	if f.st, err = store.Open(f.dbPath); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.st.Close() })
	handler := server.New(f.st, func(r *http.Request) (api.Caller, error) { return api.Caller{Node: "fixture", User: "owner"}, nil })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/handler-rotations/due") {
			f.dueCalls.Add(1)
		}
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/handler-rotations") {
			f.actions.Add(1)
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	if f.c, err = api.NewClient(srv.URL, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	f.e = env{hub: f.c.Base}
	ctx := context.Background()
	if f.task, err = f.c.CreateTask(ctx, api.CreateTaskRequest{Name: "isolated rotation test"}); err != nil {
		t.Fatal(err)
	}
	f.dir = t.TempDir()
	f.marker = filepath.Join(f.dir, "executions")
	self := filepath.Join(f.dir, "fake-tt")
	if err = os.WriteFile(self, []byte("#!/bin/sh\nprintf x >> "+spawn.ShellQuote(f.marker)+"\nexec sleep 300\n"), 0700); err != nil {
		t.Fatal(err)
	}
	f.opts = spawn.Options{Cwd: f.dir, Self: self, Command: "fixture handler command", Env: map[string]string{"TAILTERM_BRIEFING": "variable briefing"}}
	old := api.AddAgentRequest{AgentID: api.NewID("agt"), Role: api.AgentRoleDatabaseHandler, Name: "db-handler", Runtime: "generic", Host: "fixture", Cwd: f.dir, TemplateDigest: oldDigest}
	if f.old, err = ensureHandler(ctx, f.c, f.task.ID, old, f.opts); err != nil {
		t.Fatal(err)
	}
	if n := f.executions(t, 1); n != 1 {
		t.Fatalf("old handler launches: %d", n)
	}
	f.live(t, f.old)
	if f.worker, err = f.c.AddAgent(ctx, f.task.ID, api.AddAgentRequest{Name: "builder", Host: "fixture", Session: "builder", Runtime: "generic"}); err != nil {
		t.Fatal(err)
	}
	f.live(t, f.worker)
	if f.spec, err = newHandlerSpec(f.c.Base, f.task.ID, []string{"--run", "sleep 300", "--runtime", "generic", "--cwd", f.dir, "--prompt", "handler assignment"}); err != nil {
		t.Fatal(err)
	}
	f.deps = rotationDeps{spawn: f.spawn, cleanup: productionTeamRunner().cleanup, host: func() string { return "fixture" }, online: 3 * time.Second, poll: 20 * time.Millisecond}
	return f
}

func (f *rotationCLI) live(t *testing.T, a api.Agent) {
	t.Helper()
	ctx := context.Background()
	if _, err := f.c.PostEvent(ctx, f.task.ID, api.PostEventRequest{AgentID: a.ID, RunID: a.RunID, Kind: api.EventStarted}); err != nil {
		t.Fatal(err)
	}
	f.activity(t, a, "idle", "", 0)
}

func (f *rotationCLI) activity(t *testing.T, a api.Agent, state, tool string, tokens int64) {
	t.Helper()
	now := time.Now().UTC()
	activity := api.AgentActivity{State: state, ObservedAt: now, LastEventAt: now, PendingTool: tool, Tokens: api.TokenTotals{Total: tokens}}
	if tool != "" {
		activity.PendingSince = now
	}
	if _, err := f.c.ReportActivity(context.Background(), f.task.ID, a.ID, api.ActivityReport{RequestID: api.NewID("act"), RunID: a.RunID, Activity: activity}); err != nil {
		t.Fatal(err)
	}
}

// spawn mirrors cmdSpawn's database handler branch: the same briefing
// function and template digest, launched through the real ensureHandler.
func (f *rotationCLI) spawn(_ env, args []string) error {
	f.spawns++
	f.spawnArgs = append(f.spawnArgs, append([]string(nil), args...))
	flags, successor := map[string]string{}, false
	for i := 0; i < len(args); i++ {
		if args[i] == "--handler-successor" {
			successor = true
			continue
		}
		flags[args[i]] = args[i+1]
		i++
	}
	ctx := context.Background()
	detail, err := f.c.GetTask(ctx, flags["--task"])
	if err != nil {
		return err
	}
	f.briefings = append(f.briefings, agentTaskBriefingForHandler(detail.Task, flags["--name"], flags["--role"], "", primaryHandlerFirst(detail.Task, detail.Agents), 0, successor))
	req := api.AddAgentRequest{AgentID: flags["--agent-id"], Role: flags["--role"], Name: flags["--name"], Host: "fixture", Runtime: flags["--runtime"], Cwd: flags["--cwd"],
		TemplateDigest: handlerTemplateDigest(flags["--prompt"])}
	a, err := ensureHandler(ctx, f.c, flags["--task"], req, f.opts)
	if err != nil {
		return err
	}
	if f.online {
		if _, err = f.c.PostEvent(ctx, f.task.ID, api.PostEventRequest{AgentID: a.ID, RunID: a.RunID, Kind: api.EventStarted}); err != nil {
			return err
		}
	}
	return nil
}

func (f *rotationCLI) rotate(t *testing.T) (api.HandlerRotation, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return rotateHandler(ctx, f.deps, f.e, f.c, f.task.ID, api.HandlerRotationReasonManual, api.HandlerRotationTriggerOwner, f.spec)
}

func (f *rotationCLI) request(t *testing.T, to, subject string) api.Message {
	t.Helper()
	env := api.Envelope{Kind: api.EnvelopeKindRequest, To: to, Subject: subject, Body: api.EnvelopeBody{Ask: "Please record this synthetic fixture item."}}
	req := api.PostMessageRequest{Envelope: &env, Text: api.RenderText(env), AgentID: f.worker.ID, RunID: f.worker.RunID}
	if !strings.HasPrefix(to, "role:") {
		req.To = f.old.ID
		env.To = f.old.Name
	}
	m, err := f.c.PostMessage(context.Background(), f.task.ID, req)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func (f *rotationCLI) db(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func (f *rotationCLI) handlers(t *testing.T) (open, total int) {
	t.Helper()
	agents, err := f.c.ListAgents(context.Background(), f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range agents {
		if a.Role == api.AgentRoleDatabaseHandler && strings.HasPrefix(a.Name, "db-handler") {
			total++
			if a.Status != api.AgentClosed {
				open++
			}
		}
	}
	return open, total
}

// executions waits briefly for want launches of the fake tt, then reports
// the count; each launch appends one byte once tmux starts it.
func (f *rotationCLI) executions(t *testing.T, want int) int {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		data, _ := os.ReadFile(f.marker)
		if len(data) >= want || time.Now().After(deadline) {
			time.Sleep(50 * time.Millisecond)
			data, _ = os.ReadFile(f.marker)
			return len(data)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (f *rotationCLI) assertRotated(t *testing.T, r api.HandlerRotation, moved int) api.Agent {
	t.Helper()
	ctx := context.Background()
	if r.State != api.HandlerRotationCommitted || r.Receipt == nil || r.Handoff == nil || r.Receipt.Reissued != moved {
		t.Fatalf("rotation: %+v", r)
	}
	successor, err := f.c.GetAgent(ctx, f.task.ID, r.SuccessorAgentID)
	if err != nil {
		t.Fatal(err)
	}
	if successor.Role != api.AgentRoleDatabaseHandler || successor.ID == f.old.ID || successor.RunID == f.old.RunID || !runIDPattern.MatchString(successor.RunID) ||
		successor.Host != f.old.Host || successor.Runtime != f.old.Runtime || successor.Cwd != f.old.Cwd || successor.Name != "db-handler-r2" {
		t.Fatalf("successor: %+v", successor)
	}
	detail, err := f.c.GetTask(ctx, f.task.ID)
	if err != nil || detail.Task.PrimaryHandlerID != successor.ID || detail.Task.HandlerRevision != 2 {
		t.Fatalf("primary: %+v %v", detail.Task, err)
	}
	old, err := f.c.GetAgent(ctx, f.task.ID, f.old.ID)
	if err != nil || old.Status != api.AgentClosed || !old.CleanupDone || old.SuccessorID != successor.ID {
		t.Fatalf("old handler must be closed with its cleanup receipt: %+v %v", old, err)
	}
	if owned, err := handlerOwned(ctx, f.c.Base, f.task.ID, f.old.ID, f.old.RunID, f.old.Session, nil); err != nil || owned != nil {
		t.Fatalf("old owned session still present: %+v %v", owned, err)
	}
	if owned, err := handlerOwned(ctx, f.c.Base, f.task.ID, successor.ID, successor.RunID, successor.Session, nil); err != nil || owned == nil {
		t.Fatalf("successor owned session missing: %+v %v", owned, err)
	}
	if open, total := f.handlers(t); open != 1 || total != 2 {
		t.Fatalf("handlers open=%d total=%d, want exactly one successor", open, total)
	}
	if _, err := os.Stat(handlerRotationJournalPath(f.c.Base, f.task.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("journal left after cleanup: %v", err)
	}
	var superseded, reissued int
	db := f.db(t)
	if err := db.QueryRow(`SELECT count(*) FROM obligations WHERE agent_id=? AND outcome=?`, f.old.ID, api.OutcomeSuperseded).Scan(&superseded); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM obligations WHERE agent_id=? AND state<>'closed' AND needs<>?`, successor.ID, api.ObligationNeedsDelivery).Scan(&reissued); err != nil {
		t.Fatal(err)
	}
	if superseded != moved || reissued != moved {
		t.Fatalf("one reissue set: superseded=%d reissued=%d want %d", superseded, reissued, moved)
	}
	return successor
}

func TestHandlerRotationOwnerRotationEndToEnd(t *testing.T) {
	f := newRotationCLI(t, "")
	ctx := context.Background()
	f.request(t, "role:"+api.RoleDatabaseHandler, "Record the first synthetic fixture item")
	f.request(t, f.old.Name, "Record the second synthetic fixture item")
	authored := func() api.Message {
		env := api.Envelope{Kind: api.EnvelopeKindRequest, To: f.worker.Name, Subject: "Confirm the synthetic fixture handoff", Body: api.EnvelopeBody{Ask: "Confirm the fixture."}}
		m, err := f.c.PostMessage(ctx, f.task.ID, api.PostMessageRequest{Envelope: &env, Text: api.RenderText(env), To: f.worker.ID, AgentID: f.old.ID, RunID: f.old.RunID})
		if err != nil {
			t.Fatal(err)
		}
		return m
	}()
	// An auxiliary handler created after the old one, before the successor.
	aux, err := f.c.AddAgent(ctx, f.task.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Role: api.AgentRoleDatabaseHandler, Name: "aux-handler", Host: "fixture", Session: "aux", Runtime: "generic", Cwd: f.dir})
	if err != nil {
		t.Fatal(err)
	}
	f.live(t, aux)
	r, err := f.rotate(t)
	if err != nil {
		t.Fatal(err)
	}
	successor := f.assertRotated(t, r, 2)
	if n := f.executions(t, 2); f.spawns != 1 || n != 2 {
		t.Fatalf("spawns=%d executions=%d", f.spawns, n)
	}
	args := strings.Join(f.spawnArgs[0], " ")
	for _, want := range []string{"--run sleep 300", "--cwd " + f.dir, "--prompt handler assignment", "--role database_handler", "--agent-id " + successor.ID, "--name db-handler-r2", "--handler-successor"} {
		if !strings.Contains(args, want) {
			t.Fatalf("successor launch args %q miss %q", args, want)
		}
	}
	if b := f.briefings[0]; !strings.Contains(b, primaryHandlerGuidance) || !strings.Contains(b, handlerSuccessorBriefing) || strings.Contains(b, "auxiliary Database handler") {
		t.Fatalf("successor briefing is not the primary handler prompt:\n%s", b)
	}
	due, err := f.c.HandlerRotationsDue(ctx, "fixture", handlerTemplateDigest("handler assignment"))
	if err != nil || len(due.Entries) != 1 || due.Entries[0].Agent.ID != successor.ID || !due.Entries[0].DigestMatches || due.Entries[0].RecordedDigest != handlerTemplateDigest("handler assignment") {
		t.Fatalf("successor template digest: %+v %v", due, err)
	}
	got, err := f.c.GetHandlerRotation(ctx, f.task.ID, r.ID)
	if err != nil || got.State != api.HandlerRotationCommitted || got.Handoff == nil || len(got.Handoff.Reissued) != 2 || len(got.Handoff.AuthoredOpen) != 1 || got.Handoff.AuthoredOpen[0].MessageSeq != authored.Seq {
		t.Fatalf("rotation get: %+v %v", got, err)
	}
	// Routing continuity: names forward for messages, never for lifecycle.
	worker := env{hub: f.c.Base, task: f.task.ID, agent: f.worker.ID, agentName: f.worker.Name, runID: f.worker.RunID}
	if id, name, err := resolveRecipient(ctx, f.c, f.task.ID, "db-handler"); err != nil || id != successor.ID || name != successor.Name {
		t.Fatalf("recipient forwarding: %s %s %v", id, name, err)
	}
	if _, err := resolveAgent(ctx, f.c, f.task.ID, "db-handler"); err == nil {
		t.Fatal("lifecycle resolution followed the rotation")
	}
	if err := cmdRetirement(worker, "retire", []string{"db-handler"}); err == nil {
		t.Fatal("tt retire of the old name was forwarded")
	}
	result := api.Envelope{Kind: api.EnvelopeKindResult, Subject: "Synthetic fixture handoff confirmed", Body: api.EnvelopeBody{Outcome: "done", Status: map[string]string{"a1": "pass"}},
		Evidence: map[string]api.Evidence{"e1": {Type: "command", Value: "fixture -> ok"}}}
	reply, err := f.c.PostMessage(ctx, f.task.ID, api.PostMessageRequest{Envelope: &result, Text: api.RenderText(result), ReplyTo: authored.Seq, AgentID: f.worker.ID, RunID: f.worker.RunID})
	if err != nil || reply.To != successor.ID {
		t.Fatalf("reply to the rotated author: %+v %v", reply, err)
	}
	if err := cmdPost(worker, []string{"--to", "db-handler", "--", "synthetic fixture note for the handler"}); err != nil {
		t.Fatal(err)
	}
	if err := cmdSend(worker, []string{"--kind", "notice", "--to", "db-handler", "--subject", "Synthetic fixture notice for the handler", "--text", "fixture"}); err != nil {
		t.Fatal(err)
	}
	messages, err := f.c.ListMessages(ctx, f.task.ID, reply.Seq-1, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	forwarded := 0
	for _, m := range messages {
		if m.From.AgentID == f.worker.ID && m.To == successor.ID {
			forwarded++
			if m.Envelope != nil && m.Envelope.To != "" && m.Envelope.To != successor.Name {
				t.Fatalf("forwarded envelope names %q", m.Envelope.To)
			}
		}
	}
	if forwarded != 3 {
		t.Fatalf("post, send and reply must reach the successor: %d %+v", forwarded, messages)
	}
	// Briefings: workers name the successor; the auxiliary stays auxiliary.
	detail, err := f.c.GetTask(ctx, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	agents := primaryHandlerFirst(detail.Task, detail.Agents)
	if b := agentTaskBriefing(detail.Task, "fresh-worker", "", "", agents); !strings.Contains(b, "This project's Database handler is db-handler-r2.") {
		t.Fatalf("worker briefing: %s", b)
	}
	if b := agentTaskBriefing(detail.Task, "aux-handler", api.AgentRoleDatabaseHandler, "", agents); !strings.Contains(b, "auxiliary Database handler") {
		t.Fatalf("auxiliary briefing lost: %s", b)
	}
	if b := agentTaskBriefing(detail.Task, "db-handler-r2", api.AgentRoleDatabaseHandler, "", agents); strings.Contains(b, "auxiliary Database handler") || !strings.Contains(b, primaryHandlerGuidance) {
		t.Fatalf("successor re-brief: %s", b)
	}
}

func TestHandlerRotationResumesAfterEachJournalPhase(t *testing.T) {
	for _, phase := range []string{rotationPhasePreparing, rotationPhasePrepared, "spawned-unrecorded", rotationPhaseSpawned, rotationPhaseCommitted} {
		t.Run(phase, func(t *testing.T) {
			f := newRotationCLI(t, "")
			f.request(t, f.old.Name, "Record the crash fixture item")
			stopped := false
			stop := errors.New("simulated crash")
			if phase == "spawned-unrecorded" {
				// The successor launched but the routine died before saving it.
				spawnOnce := f.deps.spawn
				f.deps.spawn = func(e env, args []string) error {
					if err := spawnOnce(e, args); err != nil || stopped {
						return err
					}
					stopped = true
					return stop
				}
			} else {
				f.deps.after = func(p string) error {
					if p == phase && !stopped {
						stopped = true
						return stop
					}
					return nil
				}
			}
			if _, err := f.rotate(t); !errors.Is(err, stop) {
				t.Fatalf("expected the simulated crash, got %v", err)
			}
			r, err := f.rotate(t)
			if err != nil {
				t.Fatal(err)
			}
			f.assertRotated(t, r, 1)
			if n := f.executions(t, 2); n != 2 {
				t.Fatalf("successor launched %d times", n-1)
			}
			rotations, err := f.c.ListHandlerRotations(context.Background(), f.task.ID)
			if err != nil || len(rotations) != 1 {
				t.Fatalf("rotations: %+v %v", rotations, err)
			}
		})
	}
}

func TestHandlerRotationOfflineSuccessorAbortsCleanly(t *testing.T) {
	f := newRotationCLI(t, "")
	ctx := context.Background()
	f.request(t, f.old.Name, "Record the abort fixture item")
	f.online = false
	f.deps.online = 200 * time.Millisecond
	if _, err := f.rotate(t); err == nil || !strings.Contains(err.Error(), "did not come online") {
		t.Fatalf("bounded wait: %v", err)
	}
	rotations, _ := f.c.ListHandlerRotations(ctx, f.task.ID)
	if len(rotations) != 1 || rotations[0].State != api.HandlerRotationPrepared {
		t.Fatalf("rotation must stay prepared: %+v", rotations)
	}
	succID := rotations[0].SuccessorAgentID
	r, err := abortHandlerRotation(ctx, f.deps, f.e, f.c, f.task.ID)
	if err != nil || r.State != api.HandlerRotationAborted {
		t.Fatalf("abort: %+v %v", r, err)
	}
	succ, _ := f.c.GetAgent(ctx, f.task.ID, succID)
	old, _ := f.c.GetAgent(ctx, f.task.ID, f.old.ID)
	detail, _ := f.c.GetTask(ctx, f.task.ID)
	if succ.Status != api.AgentClosed || !succ.CleanupDone || old.Status == api.AgentClosed || detail.Task.PrimaryHandlerID != "" {
		t.Fatalf("abort: successor %+v old %+v task %+v", succ, old, detail.Task)
	}
	if owned, err := handlerOwned(ctx, f.c.Base, f.task.ID, succ.ID, succ.RunID, succ.Session, nil); err != nil || owned != nil {
		t.Fatalf("aborted successor session: %+v %v", owned, err)
	}
	obligations, _ := f.c.ListObligations(ctx, f.task.ID, "", "", false, false)
	for _, o := range obligations {
		if o.AgentID != f.old.ID && o.Needs != api.ObligationNeedsDelivery {
			t.Fatalf("abort moved an obligation: %+v", o)
		}
	}
	// A new rotation is allowed and succeeds with one fresh successor.
	f.online = true
	r, err = f.rotate(t)
	if err != nil {
		t.Fatal(err)
	}
	if r.SuccessorAgentID == succID || r.Receipt.Reissued != 1 {
		t.Fatalf("second rotation: %+v", r)
	}
}

func TestHandlerRotationRefusedWhileBusyThenResumesWhenIdle(t *testing.T) {
	f := newRotationCLI(t, "")
	ctx := context.Background()
	f.request(t, f.old.Name, "Record the busy fixture item")
	f.activity(t, f.old, "working", "", 0)
	before := f.actions.Load()
	if _, err := f.rotate(t); !rotationBusy(err) {
		t.Fatalf("busy prepare: %v", err)
	}
	if rotations, _ := f.c.ListHandlerRotations(ctx, f.task.ID); len(rotations) != 0 || f.spawns != 0 || f.actions.Load() != before+1 {
		t.Fatalf("refused prepare changed state: rotations=%+v spawns=%d", rotations, f.spawns)
	}
	if _, err := os.Stat(handlerRotationJournalPath(f.c.Base, f.task.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a refused prepare left a journal")
	}
	// Idle for prepare, busy again before commit: the commit is refused and
	// the rotation stays prepared; the rerun after idle completes it.
	f.activity(t, f.old, "idle", "", 0)
	f.deps.after = func(p string) error {
		if p == rotationPhaseSpawned {
			f.activity(t, f.old, "hung_tool", "Bash", 0)
		}
		return nil
	}
	if _, err := f.rotate(t); !rotationBusy(err) {
		t.Fatalf("commit re-check: %v", err)
	}
	detail, _ := f.c.GetTask(ctx, f.task.ID)
	if detail.Task.PrimaryHandlerID != "" {
		t.Fatal("the successor became primary while the old handler was busy")
	}
	f.deps.after = nil
	f.activity(t, f.old, "idle", "", 0)
	r, err := f.rotate(t)
	if err != nil {
		t.Fatal(err)
	}
	f.assertRotated(t, r, 1)
	if f.spawns != 1 {
		t.Fatalf("second successor launch: %d", f.spawns)
	}
}

func TestHandlerRotationOwnerCommandOverridesThresholds(t *testing.T) {
	f := newRotationCLI(t, handlerTemplateDigest("handler assignment"))
	ctx := context.Background()
	policy, err := f.c.HandlerRotationPolicy(ctx, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.c.SetHandlerRotationPolicy(ctx, f.task.ID, api.HandlerRotationPolicyRequest{ExpectedRevision: policy.Revision, Enabled: true, MaxItems: 100, MaxTotalTokens: 1 << 40, OnTemplateChange: true}); err != nil {
		t.Fatal(err)
	}
	r, err := f.rotate(t)
	if err != nil {
		t.Fatal(err)
	}
	f.assertRotated(t, r, 0)
}

// unreachableHub is a closed test server's address: no request can reach a hub.
func unreachableHub(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()
	return srv.URL
}

func TestHandlerRotationCommandsRefuseAgentSessions(t *testing.T) {
	hub := unreachableHub(t)
	agent := env{hub: hub, task: api.NewID("tsk"), agent: api.NewID("agt")}
	for _, args := range [][]string{
		{"rotate", "--task", agent.task},
		{"policy", "set", "--task", agent.task, "--revision", "0"},
		{"spec", "--task", agent.task, "--", "--run", "codex"},
	} {
		if err := cmdHandler(agent, args); err == nil || !strings.Contains(err.Error(), "owner command") {
			t.Fatalf("%v: %v", args, err)
		}
	}
	if _, err := newHandlerSpec(hub, agent.task, []string{"--run", "codex", "--agent-id", "agt_x"}); err == nil {
		t.Fatal("launch spec accepted an identity flag")
	}
	if _, err := newHandlerSpec(hub, agent.task, []string{"--cwd", "/tmp"}); err == nil {
		t.Fatal("launch spec without --run accepted")
	}
}

func setRotationPolicy(t *testing.T, f *rotationCLI, req api.HandlerRotationPolicyRequest) {
	t.Helper()
	current, err := f.c.HandlerRotationPolicy(context.Background(), f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	req.ExpectedRevision = current.Revision
	if _, err = f.c.SetHandlerRotationPolicy(context.Background(), f.task.ID, req); err != nil {
		t.Fatal(err)
	}
}

func (f *rotationCLI) runner() *rotationRunner {
	now := time.Now()
	return &rotationRunner{deps: f.deps, now: func() time.Time { return now }, notified: map[string]bool{}}
}

func (f *rotationCLI) tick(t *testing.T, r *rotationRunner) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := r.tick(ctx, f.e, f.c, "fixture"); err != nil {
		t.Fatal(err)
	}
}

func (f *rotationCLI) finishedLeases(t *testing.T, n int) {
	t.Helper()
	ctx := context.Background()
	db := f.db(t)
	for i := 0; i < n; i++ {
		item, err := f.st.CreateWorkItem(ctx, f.task.ID, api.CreateWorkItemRequest{Kind: "feature", Title: "Finished fixture item", Priority: "normal", RequestID: api.NewID("req")}, api.Caller{Node: "fixture", User: "owner"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = db.Exec(`INSERT INTO team_queue_entries(id,task_id,item_id,item_revision,order_seq,template,position,state,host,cwd,created_at,updated_at,released_at,handler_id,handler_run_id,handler_lease_generation)
 VALUES(?,?,?,1,1,'fixture',?,'finished','fixture',?,?,?,?,?,?,1)`, api.NewID("tqe"), f.task.ID, item.ID, 1000+i, f.dir, time.Now().UTC().Format(time.RFC3339Nano), time.Now().UTC().Format(time.RFC3339Nano), time.Now().UTC().Format(time.RFC3339Nano), f.old.ID, f.old.RunID); err != nil {
			t.Fatal(err)
		}
	}
}

func TestHandlerRotationRunnerTriggers(t *testing.T) {
	current := handlerTemplateDigest("handler assignment")
	cases := []struct {
		name   string
		digest string
		policy api.HandlerRotationPolicyRequest
		below  func(*testing.T, *rotationCLI)
		reach  func(*testing.T, *rotationCLI)
	}{
		{"items", current, api.HandlerRotationPolicyRequest{Enabled: true, MaxItems: 3},
			func(t *testing.T, f *rotationCLI) { f.finishedLeases(t, 2) },
			func(t *testing.T, f *rotationCLI) { f.finishedLeases(t, 1) }},
		{"tokens", current, api.HandlerRotationPolicyRequest{Enabled: true, MaxTotalTokens: 1000},
			func(t *testing.T, f *rotationCLI) { f.activity(t, f.old, "finished_silent", "", 999) },
			func(t *testing.T, f *rotationCLI) { f.activity(t, f.old, "idle", "", 1000) }},
		{"template-changed", handlerTemplateDigest("an older assignment"), api.HandlerRotationPolicyRequest{Enabled: true, OnTemplateChange: true}, nil, nil},
		{"template-legacy-run", "", api.HandlerRotationPolicyRequest{Enabled: true, OnTemplateChange: true}, nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newRotationCLI(t, tc.digest)
			if err := saveHandlerSpec(f.spec); err != nil {
				t.Fatal(err)
			}
			setRotationPolicy(t, f, tc.policy)
			f.request(t, f.old.Name, "Record the runner fixture item")
			r := f.runner()
			if tc.below != nil {
				tc.below(t, f)
				before := f.actions.Load()
				f.tick(t, r)
				if f.actions.Load() != before || f.spawns != 0 {
					t.Fatalf("rotated just below the %s limit", tc.name)
				}
				tc.reach(t, f)
			}
			// Due but busy: nothing happens this tick.
			tokens := int64(0)
			if tc.name == "tokens" {
				tokens = 1000
			}
			f.activity(t, f.old, "working", "", tokens)
			before := f.actions.Load()
			f.tick(t, r)
			if f.actions.Load() != before || f.spawns != 0 {
				t.Fatal("a busy handler was rotated")
			}
			f.activity(t, f.old, "idle", "", tokens)
			f.tick(t, r)
			rotations, err := f.c.ListHandlerRotations(context.Background(), f.task.ID)
			if err != nil || len(rotations) != 1 {
				t.Fatalf("rotations: %+v %v", rotations, err)
			}
			want := map[string]string{"items": "items", "tokens": "tokens"}[tc.name]
			if want == "" {
				want = "template"
			}
			if rotations[0].Reason != want || rotations[0].Trigger != api.HandlerRotationTriggerRunner {
				t.Fatalf("reason %q trigger %q", rotations[0].Reason, rotations[0].Trigger)
			}
			f.assertRotated(t, rotations[0], 1)
		})
	}
}

func TestHandlerRotationRunnerWithoutSpecNotifiesOnceAndDisabledPolicyIsQuiet(t *testing.T) {
	f := newRotationCLI(t, "")
	ctx := context.Background()
	r := f.runner()
	for i := 0; i < 3; i++ {
		f.tick(t, r)
	}
	if f.spawns != 0 || f.actions.Load() != 0 {
		t.Fatalf("no saved spec must not rotate: spawns=%d actions=%d", f.spawns, f.actions.Load())
	}
	messages, err := f.c.ListMessages(ctx, f.task.ID, 0, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	notices := 0
	for _, m := range messages {
		if m.Envelope != nil && strings.Contains(m.Envelope.Subject, "no saved launch spec") {
			notices++
		}
	}
	if notices != 1 {
		t.Fatalf("owner notices: %d", notices)
	}
	// A fresh runner (a relay restart) keeps the notice keyed to the episode.
	f.tick(t, f.runner())
	messages, _ = f.c.ListMessages(ctx, f.task.ID, 0, "", 100)
	count := 0
	for _, m := range messages {
		if m.Envelope != nil && strings.Contains(m.Envelope.Subject, "no saved launch spec") {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("notice repeated after restart: %d", count)
	}
	// Disabled policy: no rotation request, and the empty due answer is cached.
	setRotationPolicy(t, f, api.HandlerRotationPolicyRequest{Enabled: false, MaxItems: 1})
	if err := saveHandlerSpec(f.spec); err != nil {
		t.Fatal(err)
	}
	quiet := f.runner()
	dueBefore := f.dueCalls.Load()
	for i := 0; i < 4; i++ {
		f.tick(t, quiet)
	}
	if f.actions.Load() != 0 || f.spawns != 0 || f.dueCalls.Load()-dueBefore != 1 {
		t.Fatalf("disabled policy: actions=%d spawns=%d due calls=%d", f.actions.Load(), f.spawns, f.dueCalls.Load()-dueBefore)
	}
	later := time.Now().Add(rotationQuietCache + time.Second)
	quiet.now = func() time.Time { return later }
	f.tick(t, quiet)
	if f.dueCalls.Load()-dueBefore != 2 {
		t.Fatalf("quiet cache did not expire: %d", f.dueCalls.Load()-dueBefore)
	}
}

func TestHandlerRotationRunnerQuietWhenTemplateCurrentAndResumesInterruptedRotation(t *testing.T) {
	f := newRotationCLI(t, handlerTemplateDigest("handler assignment"))
	if err := saveHandlerSpec(f.spec); err != nil {
		t.Fatal(err)
	}
	setRotationPolicy(t, f, api.HandlerRotationPolicyRequest{Enabled: true, OnTemplateChange: true})
	f.request(t, f.old.Name, "Record the interrupted fixture item")
	r := f.runner()
	f.tick(t, r)
	if f.actions.Load() != 0 || f.spawns != 0 {
		t.Fatal("rotated although the recorded template is current")
	}
	// An owner rotation stops after its successor launched; the next runner
	// tick resumes it from the host journal without a second successor.
	stop := errors.New("simulated crash")
	f.deps.after = func(p string) error {
		if p == rotationPhaseSpawned {
			return stop
		}
		return nil
	}
	if _, err := f.rotate(t); !errors.Is(err, stop) {
		t.Fatalf("expected the simulated crash, got %v", err)
	}
	f.deps.after = nil
	r.deps = f.deps
	f.tick(t, r)
	rotations, err := f.c.ListHandlerRotations(context.Background(), f.task.ID)
	if err != nil || len(rotations) != 1 || rotations[0].Trigger != api.HandlerRotationTriggerOwner {
		t.Fatalf("rotations: %+v %v", rotations, err)
	}
	f.assertRotated(t, rotations[0], 1)
	if f.spawns != 1 {
		t.Fatalf("successor launches: %d", f.spawns)
	}
}

func TestHandlerRotationRelayTickNeedsLocalHandlerSession(t *testing.T) {
	f := newRotationCLI(t, "")
	ctx := context.Background()
	if runs, err := hostRunsHandler(ctx, f.c.Base); err != nil || !runs {
		t.Fatalf("the fixture handler session: %v %v", runs, err)
	}
	if runs, err := hostRunsHandler(ctx, unreachableHub(t)); err != nil || runs {
		t.Fatalf("another hub's handler: %v %v", runs, err)
	}
	if _, err := startupTmux(ctx, "kill-server"); err != nil {
		t.Fatal(err)
	}
	t.Setenv(spawn.EnvHub, f.c.Base)
	before := f.dueCalls.Load()
	if err := relayHandlerRotationTick(ctx); err != nil {
		t.Fatal(err)
	}
	if f.dueCalls.Load() != before {
		t.Fatal("the relay asked the hub although no handler session runs here")
	}
}

func TestHandlerRotationRunnerSpacesDueRequestsWhileEnabled(t *testing.T) {
	f := newRotationCLI(t, handlerTemplateDigest("handler assignment"))
	now := time.Now()
	r := &rotationRunner{deps: f.deps, now: func() time.Time { return now }, interval: time.Minute, notified: map[string]bool{}}
	for i := 0; i < 5; i++ {
		f.tick(t, r)
	}
	if f.dueCalls.Load() != 1 {
		t.Fatalf("due requests within one interval: %d", f.dueCalls.Load())
	}
	now = now.Add(time.Minute)
	f.tick(t, r)
	if f.dueCalls.Load() != 2 {
		t.Fatalf("due requests after the interval: %d", f.dueCalls.Load())
	}
}

// Round-one blocker b1 (#14244): a current handler launched with a prompt and
// no saved spec on this host is not template-due and gets no owner notice.
func TestHandlerRotationRunnerNoSpecCurrentTemplateIsNotDue(t *testing.T) {
	f := newRotationCLI(t, handlerTemplateDigest("handler assignment"))
	for i := 0; i < 2; i++ {
		f.tick(t, f.runner())
	}
	messages, err := f.c.ListMessages(context.Background(), f.task.ID, 0, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range messages {
		if m.Envelope != nil && strings.Contains(m.Envelope.Subject, "no saved launch spec") {
			t.Fatalf("false due notice for a current-template handler: %s", m.Envelope.Body.Text)
		}
	}
	if f.actions.Load() != 0 || f.spawns != 0 {
		t.Fatalf("rotation requested: actions=%d spawns=%d", f.actions.Load(), f.spawns)
	}
	due := api.HandlerRotationDue{Policy: api.HandlerRotationPolicy{OnTemplateChange: true}, RecordedDigest: handlerTemplateDigest("some other prompt")}
	if reasons := rotationDueReasons(due, nil); len(reasons) != 0 {
		t.Fatalf("recorded digest without a spec asserted a change: %v", reasons)
	}
	due.RecordedDigest = ""
	if reasons := rotationDueReasons(due, nil); len(reasons) != 1 || reasons[0] != api.HandlerRotationReasonTemplate {
		t.Fatalf("legacy run without a spec: %v", reasons)
	}
}
