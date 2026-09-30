package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/scs32/tailterm/hub/internal/api"
)

// Backlog steward tests (wi_5b4b94dbc9a11e8b, order #14942) use an isolated
// SQLite hub per test and synthetic agents only.

var stewardBy = api.Caller{Node: "fixture", User: "owner"}

func stewardStore(t *testing.T) (*Store, api.Task) {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "hub.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	task, err := s.CreateTask(context.Background(), api.CreateTaskRequest{Name: "Steward fixture"}, stewardBy)
	if err != nil {
		t.Fatal(err)
	}
	return s, task
}

func stewardRequest(id, name string) api.AddAgentRequest {
	return api.AddAgentRequest{Role: api.AgentRoleBacklogSteward, AgentID: id, Name: name, Host: "mini", Session: "tt-steward-" + strings.TrimPrefix(id, "agt_"), Runtime: "claude", Cwd: "/tmp"}
}

func addSteward(t *testing.T, s *Store, task, name string) api.Agent {
	t.Helper()
	a, err := s.AddAgent(context.Background(), task, stewardRequest(api.NewID("agt"), name), stewardBy)
	if err != nil {
		t.Fatalf("add steward %s: %v", name, err)
	}
	return a
}

func setAgentStatus(t *testing.T, s *Store, id, status string) {
	t.Helper()
	if _, err := s.db.Exec(`UPDATE agents SET status=? WHERE id=?`, status, id); err != nil {
		t.Fatal(err)
	}
}

func wantStewardRefusal(t *testing.T, err error, code string) {
	t.Helper()
	var refusal *api.StewardRefusal
	if !errors.As(err, &refusal) || refusal.Code != code || !errors.Is(err, api.ErrConflict) {
		t.Fatalf("want steward refusal %s, got %v", code, err)
	}
}

func TestStewardSpawnRegistersPersistentRole(t *testing.T) {
	s, task := stewardStore(t)
	ctx := context.Background()
	a := addSteward(t, s, task.ID, "backlog-steward")
	if a.Role != api.AgentRoleBacklogSteward || a.ParentAgentID != "" || a.WorkItem != nil || a.Status != api.AgentStarting {
		t.Fatalf("steward registration: %+v", a)
	}
	// Persistent-role admission rules: a stable ID, no parent, no item.
	bad := stewardRequest("", "steward-no-id")
	if _, err := s.AddAgent(ctx, task.ID, bad, stewardBy); !errors.Is(err, api.ErrInvalid) {
		t.Fatalf("steward without a stable ID: %v", err)
	}
	bad = stewardRequest(api.NewID("agt"), "steward-parented")
	bad.ParentAgentID = a.ID
	if _, err := s.AddAgent(ctx, task.ID, bad, stewardBy); !errors.Is(err, api.ErrInvalid) {
		t.Fatalf("parented steward: %v", err)
	}
	// Replaying the exact registration is idempotent.
	again, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{Role: a.Role, AgentID: a.ID, Name: a.Name, Host: a.Host, Session: a.Session, Runtime: a.Runtime, Cwd: a.Cwd}, stewardBy)
	if err != nil || again.ID != a.ID || again.RunID != a.RunID {
		t.Fatalf("replayed registration: %+v %v", again, err)
	}
	// A template digest is recorded for the steward's exact run.
	digest := strings.Repeat("ab", 32)
	b := stewardRequest(api.NewID("agt"), "other-project-steward")
	b.TemplateDigest = digest
	other, err := s.CreateTask(ctx, api.CreateTaskRequest{Name: "Other"}, stewardBy)
	if err != nil {
		t.Fatal(err)
	}
	ob, err := s.AddAgent(ctx, other.ID, b, stewardBy)
	if err != nil {
		t.Fatal(err)
	}
	var saved string
	if err := s.db.QueryRow(`SELECT template_digest FROM steward_runs WHERE run_id=? AND agent_id=?`, ob.RunID, ob.ID).Scan(&saved); err != nil || saved != digest {
		t.Fatalf("steward run digest: %q %v", saved, err)
	}
}

func TestStewardOnePerProject(t *testing.T) {
	s, task := stewardStore(t)
	ctx := context.Background()
	first := addSteward(t, s, task.ID, "backlog-steward")
	second := stewardRequest(api.NewID("agt"), "backlog-steward-2")
	for _, status := range []string{api.AgentStarting, api.AgentRunning, api.AgentDone, api.AgentRetired, api.AgentExited} {
		setAgentStatus(t, s, first.ID, status)
		_, err := s.AddAgent(ctx, task.ID, second, stewardBy)
		wantStewardRefusal(t, err, api.StewardRefusedActive)
	}
	// The exited steward restarts under its own identity by expected run; it
	// is the same slot holder, and a second identity is still refused.
	restarted, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{Role: first.Role, AgentID: first.ID, ExpectedRunID: first.RunID, Name: first.Name, Host: first.Host,
		Session: first.Session, Runtime: first.Runtime, Cwd: first.Cwd}, stewardBy)
	if err != nil || restarted.RunID == first.RunID || restarted.Status != api.AgentStarting {
		t.Fatalf("exited steward restart: %+v %v", restarted, err)
	}
	_, err = s.AddAgent(ctx, task.ID, second, stewardBy)
	wantStewardRefusal(t, err, api.StewardRefusedActive)
	// A restart by name under a new identity is refused.
	setAgentStatus(t, s, first.ID, api.AgentExited)
	byName := stewardRequest(api.NewID("agt"), first.Name)
	if _, err := s.AddAgent(ctx, task.ID, byName, stewardBy); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("restart by name without the stable ID: %v", err)
	}
	// Another project is unaffected.
	other, err := s.CreateTask(ctx, api.CreateTaskRequest{Name: "Other"}, stewardBy)
	if err != nil {
		t.Fatal(err)
	}
	addSteward(t, s, other.ID, "backlog-steward")
	// A close frees the slot.
	setAgentStatus(t, s, first.ID, api.AgentClosed)
	if a, err := s.AddAgent(ctx, task.ID, second, stewardBy); err != nil || a.ID != second.AgentID {
		t.Fatalf("steward after close: %+v %v", a, err)
	}
	// The index backs the rule: a raw second holder row is a constraint error,
	// which admission maps to the named refusal.
	_, rawErr := s.db.Exec(`INSERT INTO agents (`+agentCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		api.NewID("agt"), task.ID, "raw", "mini", "raw", "claude", "/tmp", "", api.AgentRoleBacklogSteward, api.AgentRunning, "", ts(s.now()), ts(s.now()), api.NewID("run"), "", "", "", false, "")
	if rawErr == nil {
		t.Fatal("the one-steward index accepted a second holder")
	}
	wantStewardRefusal(t, stewardUniqueViolation(ctx, s.db, task.ID, rawErr), api.StewardRefusedActive)
}

func TestStewardOnePerProjectConcurrentAdmission(t *testing.T) {
	s, task := stewardStore(t)
	ctx := context.Background()
	const n = 4
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := s.AddAgent(ctx, task.ID, stewardRequest(api.NewID("agt"), "steward-"+string(rune('a'+i))), stewardBy)
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	ok := 0
	for err := range errs {
		if err == nil {
			ok++
			continue
		}
		wantStewardRefusal(t, err, api.StewardRefusedActive)
	}
	var holders int
	if err := s.db.QueryRow(`SELECT count(*) FROM agents WHERE task_id=? AND role=? AND status<>'closed'`, task.ID, api.AgentRoleBacklogSteward).Scan(&holders); err != nil {
		t.Fatal(err)
	}
	if ok != 1 || holders != 1 {
		t.Fatalf("concurrent admissions: %d succeeded, %d stewards", ok, holders)
	}
}

// liveSteward registers a steward that is online, running and idle.
func liveSteward(t *testing.T, s *Store, task, name string) api.Agent {
	t.Helper()
	a := addSteward(t, s, task, name)
	if _, err := s.db.Exec(`UPDATE agents SET last_seen_at=?,status='running' WHERE id=?`, ts(s.now()), a.ID); err != nil {
		t.Fatal(err)
	}
	now := s.now()
	if _, err := s.ReportActivity(context.Background(), task, a.ID, api.ActivityReport{RequestID: api.NewID("act"), RunID: a.RunID, Activity: api.AgentActivity{State: "idle", ObservedAt: now, LastEventAt: now}}); err != nil {
		t.Fatal(err)
	}
	a, err := s.GetAgent(context.Background(), a.ID)
	if err != nil || !a.Online {
		t.Fatalf("live steward: %+v %v", a, err)
	}
	return a
}

func TestStewardNeverLeased(t *testing.T) {
	f := newChoresQueue(t, 1, 1, 0)
	// The fixture's only database handler closes: an idle steward remains.
	for _, h := range f.handlers {
		setAgentStatus(t, f.s, h.ID, api.AgentClosed)
	}
	steward := liveSteward(t, f.s, f.task.ID, "backlog-steward")
	q := f.add(t, 0, "src")
	// The queue reports the missing handler (the stall check names it).
	if got := listedEntry(t, f.s, f.task.ID, q.ID); !strings.Contains(got.BlockReason, "no available database handler") {
		t.Fatalf("queued entry with only a steward: %+v", got)
	}
	if _, err := claimEntry(f.s, f.task, q); err == nil || !strings.Contains(err.Error(), "no available database handler lease") {
		t.Fatalf("claim leased without a handler: %v", err)
	}
	if got := listedEntry(t, f.s, f.task.ID, q.ID); got.State != "queued" || got.HandlerID != "" {
		t.Fatalf("entry after refused claim: %+v", got)
	}
	if a, _ := f.s.GetAgent(context.Background(), steward.ID); a.Status != api.AgentRunning {
		t.Fatalf("steward after refused claim: %+v", a)
	}
}

func TestStewardNeverTeamClosed(t *testing.T) {
	s, task, item, _, _, _, req := teamCloseFixture(t)
	ctx := context.Background()
	steward := liveSteward(t, s, task.ID, "backlog-steward")
	teamCloseTerminal(t, s, task, item, "dismissed")
	result, err := s.CloseItemTeam(ctx, task.ID, req, stewardBy)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range result.Members {
		if m.AgentID == steward.ID {
			t.Fatalf("team close included the steward: %+v", result.Members)
		}
	}
	if a, err := s.GetAgent(ctx, steward.ID); err != nil || a.Status == api.AgentClosed {
		t.Fatalf("steward after team close: %+v %v", a, err)
	}
}

func TestStewardPauseClosesSteward(t *testing.T) {
	f := newPauseFixture(t, false)
	steward := liveSteward(t, f.s, f.task.ID, "backlog-steward")
	f.agents = append(f.agents, steward)
	if _, err := f.s.PauseProject(f.ctx, f.task.ID, f.pauseRequest("pause-with-steward"), f.by); err != nil {
		t.Fatal(err)
	}
	a, err := f.s.GetAgent(f.ctx, steward.ID)
	if err != nil || a.Status != api.AgentClosed || a.RunID != steward.RunID {
		t.Fatalf("steward after pause: %+v %v", a, err)
	}
	if _, held, err := stewardSlotHolder(f.ctx, f.s.db, f.task.ID); err != nil || held {
		t.Fatalf("pause left a slot holder: %v %v", held, err)
	}
}

// stewardRequestMessage sends a typed REQUEST from an agent to role:backlog_steward.
func stewardRequestMessage(s *Store, task string, from api.Agent, subject string) (api.Message, error) {
	env := api.Envelope{Kind: api.EnvelopeKindRequest, To: "role:" + api.RoleBacklogSteward, Subject: subject, Body: api.EnvelopeBody{Ask: "Please research and draft this synthetic intake."}}
	return s.PostMessage(context.Background(), task, api.PostMessageRequest{Envelope: &env, Text: api.RenderText(env), AgentID: from.ID, RunID: from.RunID}, stewardBy)
}

func TestStewardRoleRecipient(t *testing.T) {
	f := newChoresQueue(t, 1, 1, 0) // parallel mode: no fixed cap
	ctx := context.Background()
	if parallel, err := projectQueueParallel(ctx, f.s.db, f.task.ID); err != nil || !parallel {
		t.Fatalf("fixture is not parallel: %v %v", parallel, err)
	}
	worker, err := f.s.AddAgent(ctx, f.task.ID, api.AddAgentRequest{Name: "worker", Host: "mini", Session: "worker", Runtime: "claude"}, stewardBy)
	if err != nil {
		t.Fatal(err)
	}
	// No steward: the role cannot be resolved.
	if _, err = stewardRequestMessage(f.s, f.task.ID, worker, "Intake before any steward exists"); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "role:backlog_steward cannot be resolved: no running agent holds it") {
		t.Fatalf("no steward: %v", err)
	}
	steward := liveSteward(t, f.s, f.task.ID, "backlog-steward")
	// A pending rotation successor exists beside the holder.
	pending := api.NewID("agt")
	if _, err = f.s.db.Exec(`INSERT INTO agents (`+agentCols+`,steward_pending) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,1)`,
		pending, f.task.ID, "backlog-steward-r2", "mini", "tt-steward-pending", "claude", "/tmp", "", api.AgentRoleBacklogSteward, api.AgentRunning, "", ts(f.s.now()), ts(f.s.now()), api.NewID("run"), ts(f.s.now()), "", "", false, ""); err != nil {
		t.Fatal(err)
	}
	// An item-less request in a parallel project lands on the steward.
	m, err := stewardRequestMessage(f.s, f.task.ID, worker, "Intake outside the bound item")
	if err != nil || m.To != steward.ID {
		t.Fatalf("role:backlog_steward: %+v %v", m, err)
	}
	obligations, err := f.s.ListObligations(ctx, f.task.ID, ObligationFilter{AgentID: steward.ID, OpenOnly: true}, f.s.now())
	if err != nil || len(obligations) != 1 || obligations[0].MessageSeq != m.Seq {
		t.Fatalf("steward obligations: %+v %v", obligations, err)
	}
	// An item link does not change the recipient.
	env := api.Envelope{Kind: api.EnvelopeKindRequest, To: "role:" + api.RoleBacklogSteward, Subject: "Follow-up found while delivering an item", Body: api.EnvelopeBody{Ask: "Please research this synthetic follow-up."}}
	linked, err := f.s.PostMessage(ctx, f.task.ID, api.PostMessageRequest{Envelope: &env, Text: api.RenderText(env), AgentID: worker.ID, RunID: worker.RunID,
		WorkItems:        []api.MessageWorkItem{{ItemTaskID: f.task.ID, ItemID: f.items[0].ID, ItemRevision: f.items[0].Revision, Relationship: "primary"}},
		WorkOrderMessage: &api.MessageReference{TaskID: f.task.ID, Seq: f.orders[0].Seq}, RequestID: api.NewID("req")}, stewardBy)
	if err != nil || linked.To != steward.ID {
		t.Fatalf("item-linked role:backlog_steward: %+v %v", linked, err)
	}
	if pendingObligations, _ := f.s.ListObligations(ctx, f.task.ID, ObligationFilter{AgentID: pending}, f.s.now()); len(pendingObligations) != 0 {
		t.Fatalf("pending successor received role work: %+v", pendingObligations)
	}
	// An exited holder keeps the slot but receives nothing; the pending
	// successor is never chosen instead.
	setAgentStatus(t, f.s, steward.ID, api.AgentExited)
	if _, err = stewardRequestMessage(f.s, f.task.ID, worker, "Intake while the steward is exited"); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("exited steward: %v", err)
	}
	status, err := f.s.BacklogStewardStatus(ctx, f.task.ID)
	if err != nil || status.Steward != nil || status.Holder == nil || status.Holder.ID != steward.ID || status.PendingSuccessorID != pending {
		t.Fatalf("status with an exited holder: %+v %v", status, err)
	}
}
