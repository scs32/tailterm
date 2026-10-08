package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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
	// auth is the owner's authorization passed to rotate; zero for an ordinary rotation.
	auth rotationAuthorization
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
		TemplateDigest: handlerTemplateDigest(flags["--prompt"]), HandlerModel: flags["--model"], HandlerReasoning: flags["--reasoning"]}
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
	return rotateHandler(ctx, f.deps, f.e, f.c, f.task.ID, api.HandlerRotationReasonManual, api.HandlerRotationTriggerOwner, f.spec, f.auth)
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

// executions waits for want launches of the fake tt, then reports the count
// for the caller to assert; each launch appends one byte once tmux starts it.
// The wait is bounded by handlerLaunchWait because that start can trail the
// launch on a loaded host. More than want is reported at once.
func (f *rotationCLI) executions(t *testing.T, want int) int {
	t.Helper()
	deadline := time.Now().Add(handlerLaunchWait)
	for {
		data, _ := os.ReadFile(f.marker)
		if len(data) > want {
			return len(data)
		}
		if len(data) == want || !time.Now().Before(deadline) {
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
		successor.Host != f.old.Host || successor.Runtime != f.spec.runtime() || successor.Cwd != f.old.Cwd || successor.Name != "db-handler-r2" {
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

// a10 (c5): the queue handler briefing records ownership at scope
// confirmation, saves a plain done for an owner-integrated entry, mentions
// triage, and no longer asks the handler to scope unscoped entries by hand.
// The briefing is part of the handler template digest, so this release makes
// handlers with prompt-change rotation due for a template rotation.
func TestQueueHandlerBriefingIntakeOwnershipAndPlainDone(t *testing.T) {
	briefing := queueHandlerAcceptanceBriefing()
	for _, want := range []string{
		"`tt work-items scope confirm ... --owns PATH`",
		"If the item's entry shows an owner integration (`tt team queue list`: `owner-integrated`), save done with a plain `tt work-items update … --status done`; do not pass `--worktree`, `--branch` or `--commit`, and no leased run is required.",
		"tt work-items triage --project TASK",
	} {
		if !strings.Contains(briefing, want) {
			t.Fatalf("queue handler briefing lacks %q", want)
		}
	}
	if strings.Contains(briefing, "an unscoped queued entry waits until nothing else runs") {
		t.Fatal("queue handler briefing still asks for hand scoping")
	}
	// The lane default and the amendment definition (wi_2b66e2a634afa39d)
	// change the template again.
	const before = "13fde4ef0dfd1e49c301770b452e76fb7aa5f9a62e6d5388487c59bd26664030" // f9c15ca
	const current = "099dff2eecaadc67c6b90a6a9709331b23922e50663ec20c4c5b28a2fbcd98b4"
	if got := handlerTemplateDigest("handler assignment"); got != current || got == before {
		t.Fatalf("handler template digest %s, want %s", got, current)
	}
}

// The primary handler is told once to record a proposed token estimate, with
// the flags that save it and the fact that the item revision stays.
func TestPrimaryHandlerGuidanceRecordsEstimates(t *testing.T) {
	if n := strings.Count(primaryHandlerGuidance, handlerEstimateRule); n != 1 || strings.Count(primaryHandlerGuidance, "--estimate-tokens") != 1 {
		t.Fatalf("the estimate rule appears %d times in the handler guidance", n)
	}
	for _, want := range []string{"When a filing or ranking REQUEST proposes a token estimate, record it", "--estimate-tokens N --estimate-basis TEXT", "leaves the item revision unchanged"} {
		if !strings.Contains(handlerEstimateRule, want) {
			t.Fatalf("handler estimate rule lacks %q", want)
		}
	}
	if strings.Contains(queueHandlerAcceptanceBriefing(), "--estimate-tokens") {
		t.Fatal("the estimate rule is repeated in the queue acceptance briefing")
	}
}

// Handler arms (wi_fc1396aef8a72a06): under a saved arm policy, rotation stays
// within the old run's arm. A saved spec with another model or reasoning is
// refused before any spawn or prepare; a matching spec rotates and records the
// same model. Without a policy, rotation is unchanged.
func armedRotationCLI(t *testing.T, armed bool) *rotationCLI {
	t.Helper()
	f := newRotationCLI(t, "")
	if _, err := f.db(t).Exec(`INSERT OR REPLACE INTO handler_runs(task_id,agent_id,run_id,template_digest,model,reasoning,created_at) VALUES(?,?,?,?,?,?,?)`,
		f.task.ID, f.old.ID, f.old.RunID, handlerTemplateDigest("handler assignment"), "claude-sonnet-5-5", "high", time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if armed {
		if _, err := f.c.SetHandlerArmPolicy(context.Background(), f.task.ID, api.HandlerArmPolicyRequest{RequestID: "arms", Seed: "K", Arms: []api.HandlerArm{
			{ID: "S", Runtime: "generic", Model: "claude-sonnet-5-5", Reasoning: "high", Weight: 1}, {ID: "O", Runtime: "codex", Model: "gpt-6.1-sol", Reasoning: "high", Weight: 1}}}); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func (f *rotationCLI) armSpec(t *testing.T, extra ...string) handlerSpec {
	t.Helper()
	s, err := newHandlerSpec(f.c.Base, f.task.ID, append([]string{"--run", "sleep 300", "--runtime", "generic", "--cwd", f.dir, "--prompt", "handler assignment"}, extra...))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestHandlerRotationSpecArmPrecheck(t *testing.T) {
	f := armedRotationCLI(t, true)
	for _, s := range []handlerSpec{f.armSpec(t, "--model", "claude-opus-5-5", "--reasoning", "high"), f.armSpec(t, "--model", "claude-sonnet-5-5", "--reasoning", "max"), f.armSpec(t)} {
		f.spec = s
		if _, err := f.rotate(t); err == nil || !strings.Contains(err.Error(), api.HandlerRotationRefusedArmChanged) {
			t.Fatalf("spec %v: %v", s.Args, err)
		}
	}
	if f.spawns != 0 || f.actions.Load() != 0 {
		t.Fatalf("refused rotation spawned %d and sent %d actions", f.spawns, f.actions.Load())
	}
	f.spec = f.armSpec(t, "--model", "claude-sonnet-5-5", "--reasoning", "high")
	r, err := f.rotate(t)
	if err != nil {
		t.Fatal(err)
	}
	successor := f.assertRotated(t, r, 0)
	view, err := f.c.HandlerArmPolicy(context.Background(), f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range view.Handlers {
		if h.AgentID == successor.ID && (h.Model != "claude-sonnet-5-5" || h.Reasoning != "high" || h.Arm != "S") {
			t.Fatalf("successor recorded %+v", h)
		}
	}
}

func TestHandlerRotationWithoutArmPolicyIgnoresSpecModel(t *testing.T) {
	f := armedRotationCLI(t, false)
	f.spec = f.armSpec(t, "--model", "claude-opus-5-5", "--reasoning", "max")
	r, err := f.rotate(t)
	if err != nil {
		t.Fatalf("rotation without an arm policy: %v", err)
	}
	f.assertRotated(t, r, 0)
}

// Owner-authorized change of the primary's runtime, model or arm
// (wi_f250a91c85367e6f, order #20511): the old primary is a Codex run in arm O
// under an enabled arm policy, and the saved spec launches Claude in arm S.
func crossRuntimeCLI(t *testing.T) *rotationCLI {
	t.Helper()
	f := newRotationCLI(t, "")
	digest := handlerTemplateDigest("handler assignment")
	db := f.db(t)
	if _, err := db.Exec(`UPDATE agents SET runtime='codex' WHERE id=?`, f.old.ID); err != nil {
		t.Fatal(err)
	}
	f.old.Runtime = "codex"
	if _, err := db.Exec(`INSERT OR REPLACE INTO handler_runs(task_id,agent_id,run_id,template_digest,model,reasoning,created_at) VALUES(?,?,?,?,?,?,?)`,
		f.task.ID, f.old.ID, f.old.RunID, digest, "gpt-6.1-sol", "high", time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.c.SetHandlerArmPolicy(context.Background(), f.task.ID, api.HandlerArmPolicyRequest{RequestID: "arms", Enabled: true, Fallback: true, Seed: "K", TemplateDigest: digest, Arms: []api.HandlerArm{
		{ID: "S", Runtime: "claude", Model: "claude-sonnet-5-5", Reasoning: "high", Weight: 1}, {ID: "O", Runtime: "codex", Model: "gpt-6.1-sol", Reasoning: "high", Weight: 1}}}); err != nil {
		t.Fatal(err)
	}
	var err error
	if f.spec, err = newHandlerSpec(f.c.Base, f.task.ID, []string{"--run", "sleep 300", "--runtime", "claude", "--model", "claude-sonnet-5-5", "--reasoning", "high", "--cwd", f.dir, "--prompt", "handler assignment"}); err != nil {
		t.Fatal(err)
	}
	return f
}

var crossRuntimeAuthorization = rotationAuthorization{by: "owner", reason: "Owner decision: move the primary handler from Codex to Sonnet"}

func (f *rotationCLI) assertAuthorized(t *testing.T, r api.HandlerRotation) {
	t.Helper()
	want := api.HandlerRotationAuthorization{AuthorizedBy: crossRuntimeAuthorization.by, Reason: crossRuntimeAuthorization.reason, OldRuntime: "codex", SuccessorRuntime: "claude"}
	if r.Authorization == nil || *r.Authorization != want {
		t.Fatalf("authorization on the rotation: %+v", r.Authorization)
	}
}

func TestHandlerRotationAuthorizedRuntimeChangeEndToEnd(t *testing.T) {
	f := crossRuntimeCLI(t)
	ctx := context.Background()
	f.request(t, "role:"+api.RoleDatabaseHandler, "Record the first cross-runtime fixture item")
	f.request(t, f.old.Name, "Record the second cross-runtime fixture item")
	before, err := f.c.HandlerArmPolicy(ctx, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	f.auth = crossRuntimeAuthorization
	r, err := f.rotate(t)
	if err != nil {
		t.Fatal(err)
	}
	// One successor, the unchanged handoff, the old session's cleanup receipt
	// and no journal left.
	successor := f.assertRotated(t, r, 2)
	if n := f.executions(t, 2); f.spawns != 1 || n != 2 {
		t.Fatalf("spawns=%d executions=%d", f.spawns, n)
	}
	if successor.Runtime != "claude" || f.old.Runtime != "codex" {
		t.Fatalf("runtimes: old %q successor %q", f.old.Runtime, successor.Runtime)
	}
	args := strings.Join(f.spawnArgs[0], " ")
	for _, want := range []string{"--runtime claude", "--model claude-sonnet-5-5", "--reasoning high", "--cwd " + f.dir, "--agent-id " + successor.ID, "--name db-handler-r2", "--handler-successor"} {
		if !strings.Contains(args, want) {
			t.Fatalf("successor launch args %q miss %q", args, want)
		}
	}
	f.assertAuthorized(t, r)
	got, err := f.c.GetHandlerRotation(ctx, f.task.ID, r.ID)
	if err != nil || got.State != api.HandlerRotationCommitted || got.Handoff == nil || len(got.Handoff.Reissued) != 2 {
		t.Fatalf("rotation get: %+v %v", got, err)
	}
	f.assertAuthorized(t, got)
	// The arm policy is untouched; the new primary's run is in arm S.
	after, err := f.c.HandlerArmPolicy(ctx, f.task.ID)
	if err != nil || after.Policy.Revision != before.Policy.Revision || !after.Policy.Enabled || len(after.Policy.Arms) != 2 ||
		after.Policy.UpdatedAt == nil || before.Policy.UpdatedAt == nil || !after.Policy.UpdatedAt.Equal(*before.Policy.UpdatedAt) {
		t.Fatalf("arm policy changed: before %+v after %+v %v", before.Policy, after.Policy, err)
	}
	arm := ""
	for _, h := range after.Handlers {
		if h.AgentID == successor.ID {
			arm = h.Arm
		}
	}
	if arm != "S" {
		t.Fatalf("successor arm %q, want S: %+v", arm, after.Handlers)
	}
	// The record is readable in the CLI text.
	line := "Authorized by owner: Owner decision: move the primary handler from Codex to Sonnet (runtime codex -> claude)"
	if out, err := captureStdout(t, func() error { return cmdHandlerRotation(f.e, []string{"get", r.ID, "--task", f.task.ID}) }); err != nil || !strings.Contains(out, line+"\n") {
		t.Fatalf("tt handler rotation get: %v\n%s", err, out)
	}
	if out, err := captureStdout(t, func() error { return cmdHandlerRotation(f.e, []string{"list", "--task", f.task.ID}) }); err != nil || !strings.Contains(out, `authorized-by="owner"`) {
		t.Fatalf("tt handler rotation list: %v\n%s", err, out)
	}
	var data string
	if err := f.db(t).QueryRow(`SELECT data FROM events WHERE task_id=? AND text='Handler rotation committed'`, f.task.ID).Scan(&data); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"authorizedBy":"owner"`, `"oldRuntime":"codex"`, `"successorRuntime":"claude"`} {
		if !strings.Contains(data, want) {
			t.Fatalf("committed event misses %s: %s", want, data)
		}
	}
}

func TestHandlerRotationRuntimeChangeWithoutAuthorizationRefusedBeforeSpawn(t *testing.T) {
	f := crossRuntimeCLI(t)
	f.request(t, f.old.Name, "Record the refused fixture item")
	untouched := func(what string) {
		t.Helper()
		if f.spawns != 0 || f.actions.Load() != 0 {
			t.Fatalf("%s spawned %d and sent %d rotation writes", what, f.spawns, f.actions.Load())
		}
		if _, err := os.Stat(handlerRotationJournalPath(f.c.Base, f.task.ID)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s left a journal: %v", what, err)
		}
		if open, total := f.handlers(t); open != 1 || total != 1 {
			t.Fatalf("%s: handlers open=%d total=%d", what, open, total)
		}
	}
	_, err := f.rotate(t)
	if err == nil || !strings.Contains(err.Error(), "differs from handler") || !strings.Contains(err.Error(), "--authorized-by") {
		t.Fatalf("unauthorized runtime change: %v", err)
	}
	untouched("an unauthorized runtime change")
	// An authorization never covers another directory.
	elsewhere, err := newHandlerSpec(f.c.Base, f.task.ID, []string{"--run", "sleep 300", "--runtime", "claude", "--model", "claude-sonnet-5-5", "--reasoning", "high", "--cwd", t.TempDir(), "--prompt", "handler assignment"})
	if err != nil {
		t.Fatal(err)
	}
	f.spec, f.auth = elsewhere, crossRuntimeAuthorization
	if _, err = f.rotate(t); err == nil || !strings.Contains(err.Error(), "differs from handler") || strings.Contains(err.Error(), "--authorized-by") {
		t.Fatalf("authorized change of directory: %v", err)
	}
	untouched("an authorized change of directory")
}

func TestHandlerRotationAuthorizationSurvivesResume(t *testing.T) {
	f := crossRuntimeCLI(t)
	f.request(t, f.old.Name, "Record the resumed fixture item")
	stop, stopped := errors.New("simulated crash"), false
	f.deps.after = func(phase string) error {
		if phase == rotationPhasePrepared && !stopped {
			stopped = true
			return stop
		}
		return nil
	}
	f.auth = crossRuntimeAuthorization
	if _, err := f.rotate(t); !errors.Is(err, stop) {
		t.Fatalf("expected the simulated crash, got %v", err)
	}
	j, err := loadRotationJournal(f.c.Base, f.task.ID)
	if err != nil || j == nil || j.Phase != rotationPhasePrepared || j.AuthorizedBy != crossRuntimeAuthorization.by || j.AuthorizationReason != crossRuntimeAuthorization.reason {
		t.Fatalf("journal: %+v %v", j, err)
	}
	// The rerun passes no authorization, as the runner's resume does.
	f.auth = rotationAuthorization{}
	r, err := f.rotate(t)
	if err != nil {
		t.Fatal(err)
	}
	f.assertRotated(t, r, 1)
	f.assertAuthorized(t, r)
	if n := f.executions(t, 2); f.spawns != 1 || n != 2 {
		t.Fatalf("spawns=%d executions=%d, want one successor", f.spawns, n)
	}
	rotations, err := f.c.ListHandlerRotations(context.Background(), f.task.ID)
	if err != nil || len(rotations) != 1 {
		t.Fatalf("rotations: %+v %v", rotations, err)
	}
}

func TestHandlerRotateAuthorizationFlags(t *testing.T) {
	t.Setenv("TAILTERM_RELAY_STATE", t.TempDir())
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "unexpected request", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	owner := env{hub: srv.URL}
	task := api.NewID("tsk")
	spawnFlags := []string{"--", "--run", "claude", "--cwd", t.TempDir(), "--prompt", "handler assignment"}
	for name, own := range map[string][]string{
		"name only":    {"--authorized-by", "owner"},
		"reason only":  {"--authorization-reason", "move to Sonnet"},
		"blank name":   {"--authorized-by", "  ", "--authorization-reason", "move to Sonnet"},
		"blank reason": {"--authorized-by", "owner", "--authorization-reason", " "},
		"with abort":   {"--abort", "--authorized-by", "owner", "--authorization-reason", "move to Sonnet"},
	} {
		args := append(append([]string{"--task", task}, own...), spawnFlags...)
		if err := cmdHandlerRotate(owner, args); err == nil || !strings.Contains(err.Error(), "usage: tt handler rotate") || !strings.Contains(err.Error(), "--authorized-by NAME --authorization-reason TEXT") {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if err := cmdHandlerRotate(owner, []string{"--task", task, "--authorized-by", "owner", "--authorization-reason", strings.Repeat("r", 1001)}); err == nil || !strings.Contains(err.Error(), "at most 1000") {
		t.Fatalf("long reason: %v", err)
	}
	if n := requests.Load(); n != 0 {
		t.Fatalf("a usage error sent %d request(s)", n)
	}
	if _, err := os.Stat(handlerSpecPath(srv.URL, task)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a usage error saved the launch spec: %v", err)
	}
}

// Dead primary rotation on the host runner (wi_b863e669073858f4, owner order
// #28057). The death probe is injected: these tests read no process and no
// tmux listing to decide a death, and rotate only the fixture's fake handler.

// deadProbe is a scripted death probe: one answer per call, the last repeated.
type deadProbe struct {
	f       *rotationCLI
	answers []string
	calls   int
	agents  []api.Agent
	// before runs ahead of answer n (from 0), for a change between probes.
	before map[int]func()
}

func (p *deadProbe) probe(_ context.Context, hub string, a api.Agent) (string, string, *api.HandlerDeathEvidence) {
	if fn := p.before[p.calls]; fn != nil {
		fn()
	}
	answer := p.answers[min(p.calls, len(p.answers)-1)]
	p.calls++
	p.agents = append(p.agents, a)
	if hub != p.f.c.Base || answer != handlerProbeGone {
		return answer, "scripted " + answer, nil
	}
	return answer, "scripted gone", &api.HandlerDeathEvidence{Host: "fixture", AgentID: a.ID, RunID: a.RunID, ObservedAt: time.Now().UTC(), SessionName: a.Session,
		SessionState: api.HandlerDeathStateGone, PID: 4242, ProcessStarted: "Wed Oct  7 06:00:00 2026", ProcessState: api.HandlerDeathStateGone}
}

// deadRotationCLI is a fixture whose old handler the hub last heard from
// `silent` ago, recorded as working: offline and busy. Its template is
// current and no limit is reached, so only the dead primary branch can act.
func deadRotationCLI(t *testing.T, silent time.Duration, answers ...string) (*rotationCLI, *deadProbe) {
	t.Helper()
	f := newRotationCLI(t, handlerTemplateDigest("handler assignment"))
	if err := saveHandlerSpec(f.spec); err != nil {
		t.Fatal(err)
	}
	f.lastHeard(t, silent)
	p := &deadProbe{f: f, answers: answers, before: map[int]func(){}}
	f.deps.probe = p.probe
	return f, p
}

// lastHeard sets everything the hub recorded from the old run to `ago`.
func (f *rotationCLI) lastHeard(t *testing.T, ago time.Duration) {
	t.Helper()
	at := time.Now().UTC().Add(-ago).Format(time.RFC3339Nano)
	db := f.db(t)
	if _, err := db.Exec(`UPDATE agents SET last_seen_at=? WHERE id=?`, at, f.old.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE agent_activity SET state='working',observed_at=?,payload='{"state":"working"}' WHERE agent_id=? AND run_id=?`, at, f.old.ID, f.old.RunID); err != nil {
		t.Fatal(err)
	}
}

func (f *rotationCLI) rotations(t *testing.T) []api.HandlerRotation {
	t.Helper()
	rotations, err := f.c.ListHandlerRotations(context.Background(), f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	return rotations
}

func (f *rotationCLI) journalExists() bool {
	_, err := os.Stat(handlerRotationJournalPath(f.c.Base, f.task.ID))
	return err == nil
}

func (f *rotationCLI) subjects(t *testing.T, subject string) int {
	t.Helper()
	messages, err := f.c.ListMessages(context.Background(), f.task.ID, 0, "", 200)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, m := range messages {
		if m.Envelope != nil && m.Envelope.Subject == subject {
			n++
		}
	}
	return n
}

// T12: the probe says gone twice. The prepare carries the first evidence, one
// successor is spawned, the commit carries a second, later observation, and
// the journal is removed.
func TestHandlerDeathRunnerRotatesWhenProbeSaysGone(t *testing.T) {
	f, p := deadRotationCLI(t, 11*time.Minute, handlerProbeGone)
	ctx := context.Background()
	held := f.request(t, f.old.Name, "Fixture request held by the dead handler")
	f.lastHeard(t, 11*time.Minute)
	f.tick(t, f.runner())
	rotations := f.rotations(t)
	if len(rotations) != 1 || p.calls != 2 || f.spawns != 1 || f.actions.Load() != 2 {
		t.Fatalf("rotations=%d probes=%d spawns=%d actions=%d", len(rotations), p.calls, f.spawns, f.actions.Load())
	}
	r := rotations[0]
	if r.State != api.HandlerRotationCommitted || r.Reason != api.HandlerRotationReasonDeadPrimary || r.Trigger != api.HandlerRotationTriggerRunner || r.DeathEvidence == nil ||
		r.CommitDeathEvidence == nil || !r.CommitDeathEvidence.ObservedAt.After(r.DeathEvidence.ObservedAt) || r.DeathEvidence.AgentID != f.old.ID || r.DeathEvidence.RunID != f.old.RunID ||
		r.Receipt == nil || r.Receipt.Reissued != 1 || r.Handoff == nil || r.Handoff.InFlight == nil || r.Handoff.InFlight.ActivityState != "working" || r.Handoff.Reissued[0].OldMessageSeq != held.Seq {
		t.Fatalf("rotation: %+v", r)
	}
	// Both probes were asked about the old handler's exact run.
	for _, a := range p.agents {
		if a.ID != f.old.ID || a.RunID != f.old.RunID {
			t.Fatalf("probed %s / %s", a.ID, a.RunID)
		}
	}
	detail, err := f.c.GetTask(ctx, f.task.ID)
	if err != nil || detail.Task.PrimaryHandlerID != r.SuccessorAgentID || detail.Task.HandlerRevision != 2 {
		t.Fatalf("primary: %+v %v", detail.Task, err)
	}
	if f.journalExists() {
		t.Fatal("the journal was left behind")
	}
	if n := f.subjects(t, "A dead primary database handler was replaced automatically"); n != 2 {
		t.Fatalf("dead primary notices: %d", n)
	}
	// Further ticks find nothing to do.
	f.tick(t, f.runner())
	if len(f.rotations(t)) != 1 || f.spawns != 1 || p.calls != 2 {
		t.Fatalf("a later tick acted again: rotations=%d spawns=%d probes=%d", len(f.rotations(t)), f.spawns, p.calls)
	}
}

// T13, T14: an alive or unknown probe sends the hub no rotation request and
// spawns nothing, tick after tick.
func TestHandlerDeathRunnerSendsNothingUnlessGone(t *testing.T) {
	for _, answer := range []string{handlerProbeAlive, handlerProbeUnknown} {
		t.Run(answer, func(t *testing.T) {
			f, p := deadRotationCLI(t, 11*time.Minute, answer)
			for i := 0; i < 3; i++ {
				f.tick(t, f.runner())
			}
			if p.calls != 3 || f.actions.Load() != 0 || f.spawns != 0 || len(f.rotations(t)) != 0 || f.journalExists() {
				t.Fatalf("probe %s: probes=%d actions=%d spawns=%d rotations=%d", answer, p.calls, f.actions.Load(), f.spawns, len(f.rotations(t)))
			}
			if open, total := f.handlers(t); open != 1 || total != 1 {
				t.Fatalf("handlers: %d open of %d", open, total)
			}
		})
	}
	// A host with no probe confirms nothing either.
	f, _ := deadRotationCLI(t, 11*time.Minute, handlerProbeGone)
	f.deps.probe = nil
	f.tick(t, f.runner())
	if f.actions.Load() != 0 || f.spawns != 0 {
		t.Fatalf("no probe: actions=%d spawns=%d", f.actions.Load(), f.spawns)
	}
}

// T15: until the hub reports the silence met, the host is not probed at all.
func TestHandlerDeathRunnerDoesNotProbeBeforeSilence(t *testing.T) {
	f, p := deadRotationCLI(t, 5*time.Minute, handlerProbeGone)
	list, err := f.c.HandlerRotationsDue(context.Background(), "fixture", "")
	if err != nil || len(list.Entries) != 1 || !list.Entries[0].DeadCandidate || list.Entries[0].SilenceMet || list.Entries[0].Online {
		t.Fatalf("due: %+v %v", list, err)
	}
	for i := 0; i < 3; i++ {
		f.tick(t, f.runner())
	}
	if p.calls != 0 || f.actions.Load() != 0 || f.spawns != 0 {
		t.Fatalf("5 minutes of silence: probes=%d actions=%d spawns=%d", p.calls, f.actions.Load(), f.spawns)
	}
	// An online handler, however busy, is never probed.
	f.live(t, f.old)
	f.activity(t, f.old, "working", "Bash", 0)
	f.tick(t, f.runner())
	if p.calls != 0 || f.actions.Load() != 0 {
		t.Fatalf("online busy handler: probes=%d actions=%d", p.calls, f.actions.Load())
	}
}

// T16: the second look decides. A second probe that is not gone, or a hub
// that saw activity after prepare, aborts the rotation: the successor is
// closed and cleaned up, the journal is removed, the old handler stays primary.
func TestHandlerDeathRunnerAbortsWhenSecondProbeIsNotGone(t *testing.T) {
	for _, second := range []string{handlerProbeAlive, handlerProbeUnknown, "hub-heartbeat"} {
		t.Run(second, func(t *testing.T) {
			answers := []string{handlerProbeGone, second}
			if second == "hub-heartbeat" {
				answers = []string{handlerProbeGone}
			}
			f, p := deadRotationCLI(t, 11*time.Minute, answers...)
			if second == "hub-heartbeat" {
				// The host still says gone, but the hub heard from the run.
				p.before[1] = func() {
					if _, err := f.db(t).Exec(`UPDATE agents SET last_seen_at=? WHERE id=?`, time.Now().UTC().Format(time.RFC3339Nano), f.old.ID); err != nil {
						t.Error(err)
					}
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			err := f.runner().tick(ctx, f.e, f.c, "fixture")
			if !errors.Is(err, errDeathNotConfirmed) {
				t.Fatalf("tick: %v", err)
			}
			rotations := f.rotations(t)
			wantActions := int32(2) // prepare, abort
			if second == "hub-heartbeat" {
				wantActions = 3 // prepare, refused commit, abort
			}
			if len(rotations) != 1 || rotations[0].State != api.HandlerRotationAborted || rotations[0].CommitDeathEvidence != nil || p.calls != 2 || f.spawns != 1 || f.actions.Load() != wantActions {
				t.Fatalf("rotations=%+v probes=%d spawns=%d actions=%d", rotations, p.calls, f.spawns, f.actions.Load())
			}
			if f.journalExists() {
				t.Fatal("the journal was left behind")
			}
			detail, err := f.c.GetTask(context.Background(), f.task.ID)
			if err != nil || detail.Task.PrimaryHandlerID != "" || detail.Task.HandlerRevision != 1 {
				t.Fatalf("task: %+v %v", detail.Task, err)
			}
			successor, err := f.c.GetAgent(context.Background(), f.task.ID, rotations[0].SuccessorAgentID)
			old, _ := f.c.GetAgent(context.Background(), f.task.ID, f.old.ID)
			if err != nil || successor.Status != api.AgentClosed || !successor.CleanupDone || old.Status == api.AgentClosed {
				t.Fatalf("successor %+v old %s %v", successor, old.Status, err)
			}
			if open, total := f.handlers(t); open != 1 || total != 2 {
				t.Fatalf("handlers: %d open of %d", open, total)
			}
			if n := f.subjects(t, "A dead primary database handler was replaced automatically"); n != 0 {
				t.Fatalf("an aborted rotation posted %d notice(s)", n)
			}
		})
	}
}

// relayTickDueCalls runs the relay's own rotation tick, with the fixture's
// dependencies in place of the production ones, and counts its due requests.
func relayTickDueCalls(t *testing.T, f *rotationCLI) int32 {
	t.Helper()
	saved, quiet := hostRotationRunner.deps, hostRotationRunner.quietTil
	hostRotationRunner.deps, hostRotationRunner.quietTil = f.deps, time.Time{}
	defer func() { hostRotationRunner.deps, hostRotationRunner.quietTil = saved, quiet }()
	before := f.dueCalls.Load()
	if err := relayHandlerRotationTick(context.Background()); err != nil {
		t.Logf("relay tick: %v", err)
	}
	return f.dueCalls.Load() - before
}

// T17: a host whose handler session is gone still ticks while it keeps a
// trace of a handler for this hub (a saved launch spec, a rotation journal or
// a handler's session receipt), and makes no hub request with none of them.
func TestHandlerDeathRelayTickRunsOnHandlerTrace(t *testing.T) {
	f := newRotationCLI(t, handlerTemplateDigest("handler assignment"))
	ctx := context.Background()
	t.Setenv(spawn.EnvHub, f.c.Base)
	if runs, err := hostRunsHandler(ctx, f.c.Base); err != nil || !runs {
		t.Fatalf("the fixture handler session: %v %v", runs, err)
	}
	if runs, err := hostRunsHandler(ctx, unreachableHub(t)); err != nil || runs {
		t.Fatalf("another hub's handler: %v %v", runs, err)
	}
	// The fixture's handler session is gone; its ownership receipt remains.
	if _, err := startupTmux(ctx, "kill-server"); err != nil {
		t.Fatal(err)
	}
	if runs, err := hostRunsHandler(ctx, f.c.Base); err != nil || runs {
		t.Fatalf("after the session died: %v %v", runs, err)
	}
	receipts, err := filepath.Glob(filepath.Join(relayDir(), "*.session.json"))
	if err != nil || len(receipts) == 0 || !hostHasHandlerTrace(f.c.Base) || hostHasHandlerTrace(unreachableHub(t)) {
		t.Fatalf("handler session receipt: %v %v", receipts, err)
	}
	if n := relayTickDueCalls(t, f); n != 1 {
		t.Fatalf("with a handler session receipt the tick made %d due requests", n)
	}
	// With no trace at all the tick asks the hub nothing.
	var kept []byte
	for _, path := range receipts {
		if kept, err = os.ReadFile(path); err != nil {
			t.Fatal(err)
		}
		if err = os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	if hostHasHandlerTrace(f.c.Base) {
		t.Fatal("a trace remains")
	}
	if n := relayTickDueCalls(t, f); n != 0 {
		t.Fatalf("with no trace the tick made %d due requests", n)
	}
	// A receipt of a session that is not a handler's is no trace.
	var other ownedSession
	if err = json.Unmarshal(kept, &other); err != nil {
		t.Fatal(err)
	}
	other.Name, other.Role = "builder", ""
	if err = writePrivateJSON(filepath.Join(relayDir(), "other.session.json"), other); err != nil {
		t.Fatal(err)
	}
	if hostHasHandlerTrace(f.c.Base) || relayTickDueCalls(t, f) != 0 {
		t.Fatal("a non-handler session receipt counted as a handler trace")
	}
	// A saved launch spec alone is a trace; one for another hub is not.
	foreign := f.spec
	foreign.Hub = unreachableHub(t)
	if err = writePrivateJSON(handlerSpecPath(foreign.Hub, f.task.ID), foreign); err != nil {
		t.Fatal(err)
	}
	if hostHasHandlerTrace(f.c.Base) {
		t.Fatal("another hub's spec counted")
	}
	if err = saveHandlerSpec(f.spec); err != nil {
		t.Fatal(err)
	}
	if n := relayTickDueCalls(t, f); n != 1 {
		t.Fatalf("with a saved spec the tick made %d due requests", n)
	}
	if err = os.Remove(handlerSpecPath(f.c.Base, f.task.ID)); err != nil {
		t.Fatal(err)
	}
	if n := relayTickDueCalls(t, f); n != 0 {
		t.Fatalf("after the spec was removed the tick made %d due requests", n)
	}
	// A rotation journal alone is a trace.
	journal := handlerRotationJournal{Version: 1, Hub: f.c.Base, Task: "tsk_0000000000000000", Phase: rotationPhasePreparing}
	if err = writePrivateJSON(handlerRotationJournalPath(f.c.Base, journal.Task), journal); err != nil {
		t.Fatal(err)
	}
	if n := relayTickDueCalls(t, f); n != 1 {
		t.Fatalf("with a rotation journal the tick made %d due requests", n)
	}
	if f.spawns != 0 || f.actions.Load() != 0 {
		t.Fatalf("the relay tick rotated: spawns=%d actions=%d", f.spawns, f.actions.Load())
	}
}

// T18: the hub now lists a project for a dead primary even with its limit
// policy off, so the runner itself must not act on a limit there.
func TestHandlerDeathRunnerIgnoresLimitsWhenPolicyDisabled(t *testing.T) {
	// A hub answer that lists an idle handler with a limit due under a
	// disabled limit policy: nothing is rotated.
	f := newRotationCLI(t, "")
	if err := saveHandlerSpec(f.spec); err != nil {
		t.Fatal(err)
	}
	var posts atomic.Int32
	listed := api.HandlerRotationDueList{Entries: []api.HandlerRotationDue{{TaskID: f.task.ID, HandlerRevision: 1, Agent: f.old, Idle: true, Online: true, ActivityState: "idle",
		Policy:     api.HandlerRotationPolicy{TaskID: f.task.ID, Enabled: false, MaxItems: 1, OnTemplateChange: true, DeadSilenceMinutes: 10},
		DueReasons: []string{api.HandlerRotationReasonItems}, FinishedItems: 5}}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/handler-rotations/due") {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(listed)
			return
		}
		// Anything else is the start of a rotation.
		posts.Add(1)
		http.Error(w, "unexpected request", http.StatusTeapot)
	}))
	t.Cleanup(srv.Close)
	fake, err := api.NewClient(srv.URL, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	spec := f.spec
	spec.Hub = fake.Base
	if err = saveHandlerSpec(spec); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err = f.runner().tick(ctx, env{hub: fake.Base}, fake, "fixture"); err != nil {
		t.Fatal(err)
	}
	if posts.Load() != 0 || f.spawns != 0 {
		t.Fatalf("disabled limit policy: rotation requests=%d spawns=%d", posts.Load(), f.spawns)
	}
	// The same answer with the policy enabled does act, so the guard is what
	// held it back.
	listed.Entries[0].Policy.Enabled = true
	_ = f.runner().tick(ctx, env{hub: fake.Base}, fake, "fixture")
	if posts.Load() == 0 {
		t.Fatal("the enabled control case made no request")
	}
	// Against the real hub: limits reached, limit policy off, and a dead
	// candidate that is not silent yet. Nothing rotates for the limits.
	f.finishedLeases(t, 2)
	setRotationPolicy(t, f, api.HandlerRotationPolicyRequest{Enabled: false, MaxItems: 1, OnTemplateChange: true})
	f.lastHeard(t, 5*time.Minute)
	p := &deadProbe{f: f, answers: []string{handlerProbeGone}}
	f.deps.probe = p.probe
	list, err := f.c.HandlerRotationsDue(context.Background(), "fixture", "")
	if err != nil || len(list.Entries) != 1 || list.Entries[0].Policy.Enabled || len(list.Entries[0].DueReasons) != 0 || !list.Entries[0].DeadCandidate {
		t.Fatalf("due: %+v %v", list, err)
	}
	for i := 0; i < 3; i++ {
		f.tick(t, f.runner())
	}
	if f.actions.Load() != 0 || f.spawns != 0 || p.calls != 0 {
		t.Fatalf("limit policy off: actions=%d spawns=%d probes=%d", f.actions.Load(), f.spawns, p.calls)
	}
}

// T19: the production probe, with the receipt reader, the session listing and
// the PID check injected. Every unreadable fact is unknown; a present session
// or process is alive; gone needs all three absent.
func TestHandlerDeathProbe(t *testing.T) {
	const hub = "http://hub.fixture"
	a := api.Agent{ID: "agt_00000000000000aa", TaskID: "tsk_00000000000000aa", RunID: "run_00000000000000aa", Session: "tt-handler-aa", Name: "db-handler"}
	exited := time.Date(2026, 10, 7, 13, 0, 0, 0, time.UTC)
	base := runtimeProcessReceipt{Hub: hub, Task: a.TaskID, Agent: a.ID, Run: a.RunID, Session: a.Session, Socket: "tt-socket", PID: 4242, Started: "start-4242", PanePID: 4200,
		PaneStarted: "start-4200", SessionID: "$7", SessionCreated: "1700000000", ExitedAt: exited}
	observed := time.Date(2026, 10, 7, 14, 0, 0, 0, time.UTC)
	type setup struct {
		receipt    runtimeProcessReceipt
		receiptErr error
		sessions   []ownedSession
		listErr    error
		pids       map[int]any // true: alive; error: check failed; absent: gone
		socket     string
		// noRosterSession probes an agent whose roster entry names no session.
		noRosterSession bool
	}
	cases := []struct {
		name   string
		change func(*setup)
		want   string
	}{
		{"all absent", func(*setup) {}, handlerProbeGone},
		{"no pane recorded", func(s *setup) { s.receipt.PanePID, s.receipt.PaneStarted = 0, "" }, handlerProbeGone},
		{"other sessions only", func(s *setup) {
			s.sessions = []ownedSession{{ID: "$9", Created: "1", Name: "tt-handler-bb", Hub: hub, Task: a.TaskID, Agent: "agt_00000000000000bb", Run: "run_00000000000000bb"}}
		}, handlerProbeGone},
		{"missing receipt", func(s *setup) { s.receiptErr = os.ErrNotExist }, handlerProbeUnknown},
		{"receipt of another run", func(s *setup) { s.receipt.Run = "run_00000000000000bb" }, handlerProbeUnknown},
		{"receipt of another hub", func(s *setup) { s.receipt.Hub = "http://other" }, handlerProbeUnknown},
		{"receipt of another project", func(s *setup) { s.receipt.Task = "tsk_00000000000000bb" }, handlerProbeUnknown},
		{"receipt of another agent", func(s *setup) { s.receipt.Agent = "agt_00000000000000bb" }, handlerProbeUnknown},
		{"receipt without a PID", func(s *setup) { s.receipt.PID = 0 }, handlerProbeUnknown},
		{"receipt without a start identity", func(s *setup) { s.receipt.Started = "" }, handlerProbeUnknown},
		{"socket mismatch", func(s *setup) { s.socket = "another-socket" }, handlerProbeUnknown},
		{"no session name in the receipt or the roster", func(s *setup) { s.receipt.Session, s.noRosterSession = "", true }, handlerProbeUnknown},
		{"session named by the roster only", func(s *setup) { s.receipt.Session = "" }, handlerProbeGone},
		{"lister error", func(s *setup) { s.listErr = errors.New("cannot verify tmux session identities") }, handlerProbeUnknown},
		{"session present by agent, another run", func(s *setup) {
			s.sessions = []ownedSession{{ID: "$8", Created: "2", Name: "renamed", Hub: hub, Task: a.TaskID, Agent: a.ID, Run: "run_00000000000000cc"}}
		}, handlerProbeAlive},
		{"session present by name", func(s *setup) { s.sessions = []ownedSession{{ID: "$8", Created: "2", Name: a.Session}} }, handlerProbeAlive},
		{"runtime PID alive", func(s *setup) { s.pids[4242] = true }, handlerProbeAlive},
		{"pane PID alive", func(s *setup) { s.pids[4200] = true }, handlerProbeAlive},
		{"runtime PID check error", func(s *setup) { s.pids[4242] = errors.New("runtime pid reused or creation identity changed") }, handlerProbeUnknown},
		{"pane PID check error", func(s *setup) { s.pids[4200] = errors.New("operation not permitted") }, handlerProbeUnknown},
		{"pane PID without identity", func(s *setup) { s.receipt.PaneStarted = "" }, handlerProbeUnknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := setup{receipt: base, pids: map[int]any{}, socket: "tt-socket"}
			c.change(&s)
			var checked []int
			p := deathProbe{
				receipt: func(h, agent, run string) (runtimeProcessReceipt, error) {
					if h != hub || agent != a.ID || run != a.RunID {
						t.Fatalf("receipt asked for %s %s %s", h, agent, run)
					}
					return s.receipt, s.receiptErr
				},
				sessions: func(context.Context) ([]ownedSession, error) { return s.sessions, s.listErr },
				pid: func(pid int, started string) (bool, error) {
					checked = append(checked, pid)
					if want := map[int]string{4242: "start-4242", 4200: "start-4200"}[pid]; started != want {
						t.Fatalf("pid %d checked with identity %q", pid, started)
					}
					switch v := s.pids[pid].(type) {
					case bool:
						return v, nil
					case error:
						return false, v
					}
					return false, nil
				},
				socket: func() string { return s.socket }, host: func() string { return "mini" }, now: func() time.Time { return observed },
			}
			probed := a
			if s.noRosterSession {
				probed.Session = ""
			}
			state, detail, ev := p.probe(context.Background(), hub+"/", probed)
			if state != c.want || detail == "" || (ev != nil) != (c.want == handlerProbeGone) {
				t.Fatalf("state %s (%s) evidence %+v, want %s", state, detail, ev, c.want)
			}
			if c.want != handlerProbeGone {
				return
			}
			want := api.HandlerDeathEvidence{Host: "mini", AgentID: a.ID, RunID: a.RunID, ObservedAt: observed, SessionName: a.Session, SessionID: "$7", SessionCreated: "1700000000",
				SessionState: api.HandlerDeathStateGone, PID: 4242, PanePID: s.receipt.PanePID, ProcessStarted: "start-4242", ProcessState: api.HandlerDeathStateGone, ExitedAt: &exited}
			if ev.ExitedAt == nil || !ev.ExitedAt.Equal(exited) {
				t.Fatalf("exit record: %+v", ev.ExitedAt)
			}
			got := *ev
			got.ExitedAt = want.ExitedAt
			if got != want {
				t.Fatalf("evidence %+v\n    want %+v", got, want)
			}
			if wantChecks := 1 + min(s.receipt.PanePID, 1); len(checked) != wantChecks {
				t.Fatalf("PID checks: %v", checked)
			}
		})
	}
}

// T20: a dead candidate on a host with no saved launch spec gets one owner
// notice per run; the host does not probe and rotates nothing.
func TestHandlerDeathRunnerWithoutSpecNotifiesOnce(t *testing.T) {
	f, p := deadRotationCLI(t, 11*time.Minute, handlerProbeGone)
	if err := os.Remove(handlerSpecPath(f.c.Base, f.task.ID)); err != nil {
		t.Fatal(err)
	}
	const subject = "An offline busy primary handler cannot be replaced because this host has no saved launch spec"
	r := f.runner()
	for i := 0; i < 3; i++ {
		f.tick(t, r)
	}
	f.tick(t, f.runner()) // a relay restart keeps the notice keyed to the run
	if n := f.subjects(t, subject); n != 1 {
		t.Fatalf("owner notices: %d", n)
	}
	if p.calls != 0 || f.actions.Load() != 0 || f.spawns != 0 {
		t.Fatalf("no saved spec: probes=%d actions=%d spawns=%d", p.calls, f.actions.Load(), f.spawns)
	}
	messages, err := f.c.ListMessages(context.Background(), f.task.ID, 0, "", 200)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range messages {
		if m.Envelope == nil || m.Envelope.Subject != subject {
			continue
		}
		for _, part := range []string{f.old.Name + " (" + f.old.ID + " / " + f.old.RunID + ")", "silent for the 10 minute(s)", "tt handler spec --task " + f.task.ID,
			"tt handler policy set --task " + f.task.ID + " --revision 0 --dead-silence-minutes 0"} {
			if !strings.Contains(m.Envelope.Body.Text, part) {
				t.Fatalf("notice lacks %q: %s", part, m.Envelope.Body.Text)
			}
		}
	}
	// With the spec saved the same candidate is probed and replaced.
	if err = saveHandlerSpec(f.spec); err != nil {
		t.Fatal(err)
	}
	f.tick(t, f.runner())
	if rotations := f.rotations(t); len(rotations) != 1 || rotations[0].State != api.HandlerRotationCommitted || p.calls != 2 {
		t.Fatalf("after the spec was saved: %+v probes=%d", rotations, p.calls)
	}
}

// The policy command sets and prints the silence, and an omitted flag keeps it.
func TestHandlerDeathSilencePolicyCommand(t *testing.T) {
	f := newRotationCLI(t, "")
	ctx := context.Background()
	run := func(args ...string) error {
		return cmdHandlerPolicy(f.e, append(args, "--task", f.task.ID))
	}
	if p, err := f.c.HandlerRotationPolicy(ctx, f.task.ID); err != nil || p.DeadSilenceMinutes != 10 {
		t.Fatalf("default: %+v %v", p, err)
	}
	if err := run("set", "--revision", "0", "--dead-silence-minutes", "0"); err != nil {
		t.Fatal(err)
	}
	if p, _ := f.c.HandlerRotationPolicy(ctx, f.task.ID); p.DeadSilenceMinutes != 0 || !p.Enabled || p.MaxItems != api.DefaultHandlerRotationMaxItems || p.Revision != 1 {
		t.Fatalf("off: %+v", p)
	}
	if err := run("set", "--revision", "1", "--max-items", "4"); err != nil {
		t.Fatal(err)
	}
	if p, _ := f.c.HandlerRotationPolicy(ctx, f.task.ID); p.DeadSilenceMinutes != 0 || p.MaxItems != 4 {
		t.Fatalf("an omitted flag changed the silence: %+v", p)
	}
	if err := run("set", "--revision", "2", "--dead-silence-minutes", "25"); err != nil {
		t.Fatal(err)
	}
	if p, _ := f.c.HandlerRotationPolicy(ctx, f.task.ID); p.DeadSilenceMinutes != 25 || p.MaxItems != 4 {
		t.Fatalf("25 minutes: %+v", p)
	}
	for _, bad := range []string{"-1", "1441"} {
		if err := run("set", "--revision", "3", "--dead-silence-minutes", bad); err == nil {
			t.Fatalf("--dead-silence-minutes %s was accepted", bad)
		}
	}
	if err := run("get"); err != nil {
		t.Fatal(err)
	}
}

// Exited primary rotation on the host (wi_1d987e7f296b6a2e, owner order
// #28426). The probe's readers and the runner's probe, spawn and cleanup are
// injected: these tests start no tmux server and no process, read no process
// table, and use a temporary hub database.

// a5: the probe of a handler whose wrapper reported exited. Its session is
// still present, holding the wrapper and its login shell.
func TestHandlerDeathProbeExited(t *testing.T) {
	const hub = "http://hub.fixture"
	a := api.Agent{ID: "agt_00000000000000aa", TaskID: "tsk_00000000000000aa", RunID: "run_00000000000000aa", Session: "tt-handler-aa", Name: "db-handler", Status: api.AgentExited}
	exited := time.Date(2026, 10, 7, 13, 0, 0, 0, time.UTC)
	observed := time.Date(2026, 10, 7, 14, 0, 0, 0, time.UTC)
	base := runtimeProcessReceipt{Hub: hub, Task: a.TaskID, Agent: a.ID, Run: a.RunID, Session: a.Session, Socket: "tt-socket", PID: 4242, Started: "start-4242",
		SessionID: "$7", SessionCreated: "1700000000", ExitedAt: exited}
	own := ownedSession{ID: "$7", Created: "1700000000", Name: a.Session, Hub: hub, Task: a.TaskID, Agent: a.ID, Run: a.RunID}
	type setup struct {
		agent      api.Agent
		receipt    runtimeProcessReceipt
		receiptErr error
		sessions   []ownedSession
		listErr    error
		pids       map[int]any // true: alive; error: check failed; absent: gone
		panes      []int
		panesErr   error
		// parents is the process table: pane 4200 is the wrapper, 4300 its shell.
		parents    map[int]int
		parentsErr error
	}
	cases := []struct {
		name   string
		change func(*setup)
		want   string
		// session is the evidence's session state when the answer is gone.
		session string
	}{
		{"idle shell", func(*setup) {}, handlerProbeGone, api.HandlerDeathStateIdleShell},
		{"receipt records no session identity", func(s *setup) { s.receipt.SessionID, s.receipt.SessionCreated = "", "" }, handlerProbeGone, api.HandlerDeathStateIdleShell},
		{"session absent", func(s *setup) { s.sessions = nil }, handlerProbeGone, api.HandlerDeathStateGone},
		{"process under the shell", func(s *setup) { s.parents[4400] = 4300 }, handlerProbeAlive, ""},
		{"runtime PID running", func(s *setup) { s.pids[4242] = true }, handlerProbeAlive, ""},
		{"runtime PID running, session absent", func(s *setup) { s.sessions, s.pids[4242] = nil, true }, handlerProbeAlive, ""},
		{"unreadable receipt", func(s *setup) { s.receiptErr = os.ErrNotExist }, handlerProbeUnknown, ""},
		{"receipt records no exit", func(s *setup) { s.receipt.ExitedAt = time.Time{} }, handlerProbeUnknown, ""},
		{"receipt records no exit, session absent", func(s *setup) { s.receipt.ExitedAt, s.sessions = time.Time{}, nil }, handlerProbeUnknown, ""},
		{"unreadable sessions", func(s *setup) { s.listErr = errors.New("cannot verify tmux session identities") }, handlerProbeUnknown, ""},
		{"unreadable panes", func(s *setup) { s.panesErr = errors.New("tmux failed") }, handlerProbeUnknown, ""},
		{"unreadable process table", func(s *setup) { s.parentsErr = errors.New("unreadable process table row") }, handlerProbeUnknown, ""},
		{"PID check error", func(s *setup) { s.pids[4242] = errors.New("runtime pid reused or creation identity changed") }, handlerProbeUnknown, ""},
		{"two panes", func(s *setup) { s.panes = []int{4200, 4201} }, handlerProbeUnknown, ""},
		{"no pane", func(s *setup) { s.panes = nil }, handlerProbeUnknown, ""},
		{"session of another run", func(s *setup) { s.sessions[0].Run = "run_00000000000000cc" }, handlerProbeUnknown, ""},
		{"session of another project", func(s *setup) { s.sessions[0].Task = "tsk_00000000000000bb" }, handlerProbeUnknown, ""},
		{"session of another hub", func(s *setup) { s.sessions[0].Hub = "http://other" }, handlerProbeUnknown, ""},
		{"session with another tmux ID", func(s *setup) { s.sessions[0].ID = "$8" }, handlerProbeUnknown, ""},
		{"session created at another time", func(s *setup) { s.sessions[0].Created = "1700000001" }, handlerProbeUnknown, ""},
		{"session renamed", func(s *setup) { s.sessions[0].Name = "renamed" }, handlerProbeUnknown, ""},
		{"untagged session with the handler's name", func(s *setup) { s.sessions = []ownedSession{{ID: "$7", Created: "1700000000", Name: a.Session}} }, handlerProbeUnknown, ""},
		{"two sessions name the handler", func(s *setup) {
			s.sessions = append(s.sessions, ownedSession{ID: "$9", Created: "2", Name: "copy", Hub: hub, Task: a.TaskID, Agent: a.ID, Run: a.RunID})
		}, handlerProbeUnknown, ""},
		{"pane process not in the table", func(s *setup) { delete(s.parents, 4200) }, handlerProbeUnknown, ""},
		{"wrapper without a shell", func(s *setup) { delete(s.parents, 4300) }, handlerProbeUnknown, ""},
		{"wrapper with two children", func(s *setup) { s.parents[4301] = 4200 }, handlerProbeUnknown, ""},
		// The roster status decides the branch: a handler that is not exited
		// is alive whenever its session is present, whatever the pane holds.
		{"not exited, session present", func(s *setup) { s.agent.Status = api.AgentRunning }, handlerProbeAlive, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := setup{agent: a, receipt: base, sessions: []ownedSession{own}, pids: map[int]any{}, panes: []int{4200}, parents: map[int]int{1: 0, 4200: 1, 4300: 4200, 9000: 1, 9001: 9000, 9002: 9001}}
			c.change(&s)
			readPanes, readTable := 0, 0
			p := deathProbe{
				receipt:  func(string, string, string) (runtimeProcessReceipt, error) { return s.receipt, s.receiptErr },
				sessions: func(context.Context) ([]ownedSession, error) { return s.sessions, s.listErr },
				pid: func(pid int, started string) (bool, error) {
					if pid != 4242 || started != "start-4242" {
						t.Fatalf("pid %d checked with identity %q", pid, started)
					}
					switch v := s.pids[pid].(type) {
					case bool:
						return v, nil
					case error:
						return false, v
					}
					return false, nil
				},
				panes: func(_ context.Context, id string) ([]int, error) {
					readPanes++
					if id != "$7" {
						t.Fatalf("panes read for session %q", id)
					}
					return s.panes, s.panesErr
				},
				processes: func(context.Context) (map[int]int, error) { readTable++; return s.parents, s.parentsErr },
				socket:    func() string { return "tt-socket" }, host: func() string { return "mini" }, now: func() time.Time { return observed },
			}
			state, detail, ev := p.probe(context.Background(), hub+"/", s.agent)
			if state != c.want || detail == "" || (ev != nil) != (c.want == handlerProbeGone) {
				t.Fatalf("state %s (%s) evidence %+v, want %s", state, detail, ev, c.want)
			}
			if s.agent.Status != api.AgentExited && (readPanes != 0 || readTable != 0) {
				t.Fatal("a handler that is not exited had its pane read")
			}
			if c.want != handlerProbeGone {
				return
			}
			want := api.HandlerDeathEvidence{Host: "mini", AgentID: a.ID, RunID: a.RunID, ObservedAt: observed, SessionName: a.Session, SessionID: "$7", SessionCreated: "1700000000",
				SessionState: c.session, PID: 4242, ProcessStarted: "start-4242", ProcessState: api.HandlerDeathStateGone}
			if c.session == api.HandlerDeathStateIdleShell {
				want.PaneRootPID = 4200
			}
			if ev.ExitedAt == nil || !ev.ExitedAt.Equal(exited) {
				t.Fatalf("exit record: %+v", ev.ExitedAt)
			}
			got := *ev
			got.ExitedAt = nil
			if got != want {
				t.Fatalf("evidence %+v\n    want %+v", got, want)
			}
		})
	}
	// With no pane or process reader the host confirms nothing.
	p := deathProbe{receipt: func(string, string, string) (runtimeProcessReceipt, error) { return base, nil },
		sessions: func(context.Context) ([]ownedSession, error) { return []ownedSession{own}, nil }, pid: func(int, string) (bool, error) { return false, nil },
		socket: func() string { return "tt-socket" }, host: func() string { return "mini" }, now: func() time.Time { return observed }}
	if state, _, ev := p.probe(context.Background(), hub, a); state != handlerProbeUnknown || ev != nil {
		t.Fatalf("no readers: %s %+v", state, ev)
	}
}

// a5: the process table is read whole or not at all.
func TestHandlerDeathProcessTableParse(t *testing.T) {
	got, err := parseProcessParents("    1     0\n 4200     1\n 4300  4200\n")
	if err != nil || len(got) != 3 || got[4300] != 4200 || got[1] != 0 {
		t.Fatalf("table: %v %v", got, err)
	}
	for _, raw := range []string{"", "1 0\nzsh 1\n", "1 0\n4200\n", "1 0\n4200 1 extra\n", "1 0\n1 0\n", "0 0\n", "7 -1\n"} {
		if got, err := parseProcessParents(raw); err == nil {
			t.Fatalf("table %q read as %v", raw, got)
		}
	}
}

// exitedRunner is a hub with one busy primary whose wrapper reported exited
// 11 minutes ago, and a runner whose probe, spawn and cleanup are scripted.
type exitedRunner struct {
	*rotationCLI
	exit    api.Event
	answers []string
	probes  []api.Agent
	// before runs ahead of probe n (from 0), for a change between probes.
	before  map[int]func()
	cleaned []string
	held    api.Message
}

func newExitedRunner(t *testing.T, answers ...string) *exitedRunner {
	t.Helper()
	t.Setenv("TAILTERM_RELAY_STATE", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	x := &exitedRunner{rotationCLI: &rotationCLI{dbPath: filepath.Join(t.TempDir(), "hub.sqlite")}, answers: answers, before: map[int]func(){}}
	f := x.rotationCLI
	var err error
	if f.st, err = store.Open(f.dbPath); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.st.Close() })
	handler := server.New(f.st, func(r *http.Request) (api.Caller, error) { return api.Caller{Node: "fixture", User: "owner"}, nil })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	if f.task, err = f.c.CreateTask(ctx, api.CreateTaskRequest{Name: "isolated exited primary test"}); err != nil {
		t.Fatal(err)
	}
	f.dir = t.TempDir()
	if f.old, err = f.c.AddAgent(ctx, f.task.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Role: api.AgentRoleDatabaseHandler, Name: "db-handler", Runtime: "generic", Host: "fixture",
		Session: "tt-handler-exited-fixture", Cwd: f.dir, TemplateDigest: handlerTemplateDigest("handler assignment")}); err != nil {
		t.Fatal(err)
	}
	f.live(t, f.old)
	if f.worker, err = f.c.AddAgent(ctx, f.task.ID, api.AddAgentRequest{Name: "builder", Host: "fixture", Session: "builder", Runtime: "generic"}); err != nil {
		t.Fatal(err)
	}
	f.live(t, f.worker)
	x.held = f.request(t, f.old.Name, "Fixture request held by the exited handler")
	f.activity(t, f.old, "working", "Bash", 0)
	if x.exit, err = f.c.PostEvent(ctx, f.task.ID, api.PostEventRequest{AgentID: f.old.ID, RunID: f.old.RunID, Kind: api.EventExited, Text: "Process exited (1)"}); err != nil {
		t.Fatal(err)
	}
	// The exit and everything else the hub recorded from the run was 11 minutes ago.
	at := time.Now().UTC().Add(-11 * time.Minute)
	x.exit.CreatedAt = at
	db := f.db(t)
	for _, q := range []struct {
		query string
		args  []any
	}{{`UPDATE agents SET last_seen_at=? WHERE id=?`, []any{at.Format(time.RFC3339Nano), f.old.ID}},
		{`UPDATE agent_activity SET observed_at=? WHERE agent_id=? AND run_id=?`, []any{at.Format(time.RFC3339Nano), f.old.ID, f.old.RunID}},
		{`UPDATE events SET created_at=? WHERE seq=?`, []any{at.Format(time.RFC3339Nano), x.exit.Seq}}} {
		if _, err := db.Exec(q.query, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	// The broker's tick after the exit closed the request the handler held.
	open, err := f.st.BrokerOpenObligations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	closed := 0
	for _, o := range open {
		if o.AgentID == f.old.ID {
			if err := f.st.BrokerCloseRecipientGone(ctx, o, at.Add(30*time.Second)); err != nil {
				t.Fatal(err)
			}
			closed++
		}
	}
	if closed != 1 {
		t.Fatalf("the broker closed %d obligation(s) of the exited handler", closed)
	}
	if f.spec, err = newHandlerSpec(f.c.Base, f.task.ID, []string{"--run", "sleep 300", "--runtime", "generic", "--cwd", f.dir, "--prompt", "handler assignment"}); err != nil {
		t.Fatal(err)
	}
	if err = saveHandlerSpec(f.spec); err != nil {
		t.Fatal(err)
	}
	f.deps = rotationDeps{host: func() string { return "fixture" }, online: 3 * time.Second, poll: 20 * time.Millisecond,
		// spawn registers the successor as tt spawn would; it starts nothing.
		spawn: func(_ env, args []string) error {
			f.spawns++
			flags := map[string]string{}
			for i := 0; i+1 < len(args); i++ {
				if args[i] == "--handler-successor" {
					continue
				}
				flags[args[i]] = args[i+1]
				i++
			}
			a, err := f.c.AddAgent(ctx, flags["--task"], api.AddAgentRequest{AgentID: flags["--agent-id"], Role: flags["--role"], Name: flags["--name"], Host: "fixture", Session: "tt-handler-successor-fixture",
				Runtime: flags["--runtime"], Cwd: flags["--cwd"], TemplateDigest: handlerTemplateDigest(flags["--prompt"])})
			if err != nil {
				return err
			}
			_, err = f.c.PostEvent(ctx, f.task.ID, api.PostEventRequest{AgentID: a.ID, RunID: a.RunID, Kind: api.EventStarted})
			return err
		},
		cleanup: func(_ context.Context, _ env, _, agent string) error {
			x.cleaned = append(x.cleaned, agent)
			return nil
		},
		probe: func(_ context.Context, hub string, a api.Agent) (string, string, *api.HandlerDeathEvidence) {
			if fn := x.before[len(x.probes)]; fn != nil {
				fn()
			}
			answer := x.answers[min(len(x.probes), len(x.answers)-1)]
			x.probes = append(x.probes, a)
			if hub != f.c.Base || answer != handlerProbeGone {
				return answer, "scripted " + answer, nil
			}
			return answer, "scripted idle shell", &api.HandlerDeathEvidence{Host: "fixture", AgentID: a.ID, RunID: a.RunID, ObservedAt: time.Now().UTC(), SessionName: a.Session,
				SessionState: api.HandlerDeathStateIdleShell, PID: 4242, ProcessStarted: "Wed Oct  7 06:00:00 2026", ProcessState: api.HandlerDeathStateGone, ExitedAt: &at, PaneRootPID: 4200}
		}}
	return x
}

// a6, a8: the runner replaces an exited dead candidate when its probe says
// gone twice, cleans up the old session, and the rotation prints its exit.
func TestHandlerDeathRunnerRotatesExitedPrimary(t *testing.T) {
	x := newExitedRunner(t, handlerProbeGone)
	f := x.rotationCLI
	ctx := context.Background()
	list, err := f.c.HandlerRotationsDue(ctx, "fixture", "")
	if err != nil || len(list.Entries) != 1 || list.Entries[0].Agent.ID != f.old.ID || list.Entries[0].Agent.Status != api.AgentExited || !list.Entries[0].DeadCandidate || !list.Entries[0].SilenceMet {
		t.Fatalf("due: %+v %v", list, err)
	}
	f.tick(t, f.runner())
	rotations := f.rotations(t)
	if len(rotations) != 1 || len(x.probes) != 2 || f.spawns != 1 || f.actions.Load() != 2 {
		t.Fatalf("rotations=%d probes=%d spawns=%d actions=%d", len(rotations), len(x.probes), f.spawns, f.actions.Load())
	}
	r := rotations[0]
	if r.State != api.HandlerRotationCommitted || r.Reason != api.HandlerRotationReasonDeadPrimary || r.OldAgentID != f.old.ID || r.OldRunID != f.old.RunID || r.Exit == nil || r.Exit.Code != 1 ||
		r.Exit.EventSeq != x.exit.Seq || r.DeathEvidence == nil || r.DeathEvidence.SessionState != api.HandlerDeathStateIdleShell || r.CommitDeathEvidence == nil ||
		!r.CommitDeathEvidence.ObservedAt.After(r.DeathEvidence.ObservedAt) || r.Receipt == nil || r.Receipt.Reissued != 1 || r.Handoff.Reissued[0].OldMessageSeq != x.held.Seq {
		t.Fatalf("rotation: %+v exit %+v", r, r.Exit)
	}
	for _, a := range x.probes {
		if a.ID != f.old.ID || a.RunID != f.old.RunID || a.Status != api.AgentExited {
			t.Fatalf("probed %s / %s (%s)", a.ID, a.RunID, a.Status)
		}
	}
	detail, err := f.c.GetTask(ctx, f.task.ID)
	old, _ := f.c.GetAgent(ctx, f.task.ID, f.old.ID)
	if err != nil || detail.Task.PrimaryHandlerID != r.SuccessorAgentID || detail.Task.HandlerRevision != 2 || old.Status != api.AgentClosed {
		t.Fatalf("primary: %+v old %s %v", detail.Task, old.Status, err)
	}
	// The old session, with its leftover shell, is cleaned up as in every rotation.
	if len(x.cleaned) != 1 || x.cleaned[0] != f.old.ID || f.journalExists() {
		t.Fatalf("cleanup %v journal %t", x.cleaned, f.journalExists())
	}
	if n := f.subjects(t, "A dead primary database handler was replaced automatically"); n != 2 {
		t.Fatalf("dead primary notices: %d", n)
	}
	out, err := captureStdout(t, func() error { return cmdHandlerRotation(f.e, []string{"get", r.ID, "--task", f.task.ID}) })
	exitLine := fmt.Sprintf("Wrapper exit report: status 1, reported %s (event %d)\n", r.Exit.ReportedAt.UTC().Format(time.RFC3339), r.Exit.EventSeq)
	if err != nil || !strings.Contains(out, exitLine) || strings.Count(out, "tmux session tt-handler-exited-fixture idle_shell, runtime process 4242") != 2 ||
		strings.Count(out, ", pane holds only wrapper process 4200 and its idle shell, wrapper exit recorded ") != 2 {
		t.Fatalf("rotation get: %v\n%s", err, out)
	}
	// Further ticks find nothing to do.
	f.tick(t, f.runner())
	if len(f.rotations(t)) != 1 || f.spawns != 1 || len(x.probes) != 2 {
		t.Fatalf("a later tick acted again: rotations=%d spawns=%d probes=%d", len(f.rotations(t)), f.spawns, len(x.probes))
	}
}

// a6: on any doubt the exited primary stays. An alive or unknown first probe
// sends the hub nothing; a second probe that is not gone, or a restart before
// it, aborts the prepared rotation.
func TestHandlerDeathRunnerLeavesExitedPrimaryOnDoubt(t *testing.T) {
	for _, answer := range []string{handlerProbeAlive, handlerProbeUnknown} {
		t.Run("first probe "+answer, func(t *testing.T) {
			x := newExitedRunner(t, answer)
			f := x.rotationCLI
			for i := 0; i < 3; i++ {
				f.tick(t, f.runner())
			}
			old, err := f.c.GetAgent(context.Background(), f.task.ID, f.old.ID)
			if len(x.probes) != 3 || f.actions.Load() != 0 || f.spawns != 0 || len(f.rotations(t)) != 0 || f.journalExists() || len(x.cleaned) != 0 || err != nil || old.Status != api.AgentExited {
				t.Fatalf("probes=%d actions=%d spawns=%d rotations=%d cleaned=%v old=%s %v", len(x.probes), f.actions.Load(), f.spawns, len(f.rotations(t)), x.cleaned, old.Status, err)
			}
		})
	}
	for _, second := range []string{handlerProbeAlive, handlerProbeUnknown, "restarted"} {
		t.Run("second probe "+second, func(t *testing.T) {
			answers := []string{handlerProbeGone, second}
			if second == "restarted" {
				answers = []string{handlerProbeGone}
			}
			x := newExitedRunner(t, answers...)
			f := x.rotationCLI
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			wantProbes := 2
			if second == "restarted" {
				// The exited handler is started again while the successor
				// starts: its new run is never probed with the old one's facts.
				wantProbes = 1
				f.deps.after = func(phase string) error {
					if phase != rotationPhaseSpawned {
						return nil
					}
					_, err := f.c.AddAgent(ctx, f.task.ID, api.AddAgentRequest{AgentID: f.old.ID, Role: api.AgentRoleDatabaseHandler, Name: f.old.Name, Runtime: f.old.Runtime, Host: f.old.Host,
						Session: f.old.Session, Cwd: f.old.Cwd, ExpectedRunID: f.old.RunID})
					return err
				}
			}
			if err := f.runner().tick(ctx, f.e, f.c, "fixture"); !errors.Is(err, errDeathNotConfirmed) {
				t.Fatalf("tick: %v", err)
			}
			rotations := f.rotations(t)
			if len(rotations) != 1 || rotations[0].State != api.HandlerRotationAborted || rotations[0].CommitDeathEvidence != nil || rotations[0].Exit == nil || len(x.probes) != wantProbes ||
				f.spawns != 1 || f.actions.Load() != 2 {
				t.Fatalf("rotations=%+v probes=%d spawns=%d actions=%d", rotations, len(x.probes), f.spawns, f.actions.Load())
			}
			detail, err := f.c.GetTask(context.Background(), f.task.ID)
			if err != nil || detail.Task.PrimaryHandlerID != "" || detail.Task.HandlerRevision != 1 || f.journalExists() {
				t.Fatalf("task: %+v journal %t %v", detail.Task, f.journalExists(), err)
			}
			successor, err := f.c.GetAgent(context.Background(), f.task.ID, rotations[0].SuccessorAgentID)
			old, _ := f.c.GetAgent(context.Background(), f.task.ID, f.old.ID)
			if err != nil || successor.Status != api.AgentClosed || old.Status == api.AgentClosed || (second != "restarted") != (old.Status == api.AgentExited) ||
				len(x.cleaned) != 1 || x.cleaned[0] != rotations[0].SuccessorAgentID {
				t.Fatalf("successor %s old %s cleaned %v %v", successor.Status, old.Status, x.cleaned, err)
			}
			if n := f.subjects(t, "A dead primary database handler was replaced automatically"); n != 0 {
				t.Fatalf("an aborted rotation posted %d notice(s)", n)
			}
		})
	}
}
