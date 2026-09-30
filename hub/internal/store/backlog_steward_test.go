package store

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

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
	// Online compares with the wall clock, even under a fixture clock.
	if _, err := s.db.Exec(`UPDATE agents SET last_seen_at=?,status='running' WHERE id=?`, ts(time.Now().UTC()), a.ID); err != nil {
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

func TestStewardDecisionRoutedToDelegate(t *testing.T) {
	f := newDelegationFixture(t)
	ctx := context.Background()
	steward := liveSteward(t, f.s, f.task.ID, "backlog-steward")
	propose := func(key string) api.Message {
		t.Helper()
		m, err := f.s.CreateDecision(ctx, f.task.ID, api.CreateDecisionRequest{AgentID: steward.ID, RequestID: key,
			WorkItems: []api.MessageWorkItem{{ItemTaskID: f.item.TaskID, ItemID: f.item.ID, ItemRevision: f.item.Revision, Relationship: "primary"}},
			DecisionRequest: api.DecisionRequest{Question: "Should these two small items ship as one batch?", Category: "decision", RecommendedOptionID: "batch",
				RecommendationReason: "They touch the same files.", Options: []api.DecisionOption{{ID: "batch", Label: "Batch", Description: "One team delivers both."},
					{ID: "separate", Label: "Separate", Description: "Queue them apart."}}}}, f.by)
		if err != nil {
			t.Fatalf("steward decision %s: %v", key, err)
		}
		return m
	}
	// A steward-authored decision with item refs is stored.
	first := propose("steward-batch-1")
	if first.DecisionRequest == nil || first.From.AgentID != steward.ID || len(first.WorkItems) != 1 || first.WorkItems[0].ItemID != f.item.ID {
		t.Fatalf("stored decision: %+v", first)
	}
	// An open decisions window routes it to the delegate.
	w := f.open(t, "window-to-lead", api.DelegationScopeDecisions, time.Hour)
	if routed(f.window(t), api.DelegationRouteDecision, first.Seq) == nil {
		t.Fatalf("steward decision not routed: %+v", f.window(t).Routes)
	}
	answer, err := f.s.AnswerDecision(ctx, f.task.ID, first.Seq, api.AnswerDecisionRequest{RequestID: "delegate-answer", AgentID: f.lead.ID, RunID: f.lead.RunID, OptionID: "batch", Rationale: "Same files; one team."}, f.by)
	if err != nil || answer.To != steward.ID || answer.From.AgentID != f.lead.ID {
		t.Fatalf("delegate answer: %+v %v", answer, err)
	}
	// A window whose delegate is the steward never routes the steward's own decision.
	if _, err = f.s.CloseDelegationWindow(ctx, f.task.ID, w.ID, api.CloseDelegationWindowRequest{RequestID: "close-lead-window"}, f.by); err != nil {
		t.Fatal(err)
	}
	out, err := f.s.OpenDelegationWindow(ctx, f.task.ID, api.OpenDelegationWindowRequest{Delegate: steward.Name, EndsAt: f.s.now().Add(time.Hour), Scope: api.DelegationScopeDecisions,
		Source: &api.DelegationSource{Kind: api.DelegationSourceTT}, RequestID: "window-to-steward"}, f.by)
	if err != nil || out.Window == nil || out.Window.DelegateAgentID != steward.ID {
		t.Fatalf("steward window: %+v %v", out, err)
	}
	own := propose("steward-batch-2")
	list, err := f.s.ListDelegationWindows(ctx, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, win := range list.Windows {
		if routed(win, api.DelegationRouteDecision, own.Seq) != nil {
			t.Fatalf("the steward's own decision was routed to itself: %+v", win.Routes)
		}
	}
}

func TestTriageStewardAccess(t *testing.T) {
	s, task := stewardStore(t)
	ctx := context.Background()
	steward := liveSteward(t, s, task.ID, "backlog-steward")
	if _, err := s.WorkItemTriage(ctx, task.ID, 0, steward.ID, steward.RunID); err != nil {
		t.Fatalf("active steward triage: %v", err)
	}
	worker, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "worker", Host: "mini", Session: "worker", Runtime: "claude"}, stewardBy)
	if err != nil {
		t.Fatal(err)
	}
	pending := api.NewID("agt")
	pendingRun := api.NewID("run")
	if _, err = s.db.Exec(`INSERT INTO agents (`+agentCols+`,steward_pending) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,1)`,
		pending, task.ID, "backlog-steward-r2", "mini", "tt-steward-pending", "claude", "/tmp", "", api.AgentRoleBacklogSteward, api.AgentRunning, "", ts(s.now()), ts(s.now()), pendingRun, ts(time.Now().UTC()), "", "", false, ""); err != nil {
		t.Fatal(err)
	}
	for name, who := range map[string][2]string{
		"stale steward run": {steward.ID, api.NewID("run")},
		"pending successor": {pending, pendingRun},
		"ordinary agent":    {worker.ID, worker.RunID},
		"missing run":       {steward.ID, ""},
	} {
		if _, err := s.WorkItemTriage(ctx, task.ID, 0, who[0], who[1]); !errors.Is(err, api.ErrConflict) {
			t.Fatalf("%s triage: %v", name, err)
		}
	}
	setAgentStatus(t, s, steward.ID, api.AgentRetired)
	if _, err := s.WorkItemTriage(ctx, task.ID, 0, steward.ID, steward.RunID); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("retired steward triage: %v", err)
	}
	// The owner's unbound session is unaffected.
	if _, err := s.WorkItemTriage(ctx, task.ID, 0, "", ""); err != nil {
		t.Fatalf("owner triage: %v", err)
	}
}

// fileHeldFollowUp files a review follow-up the way review convergence does.
func fileHeldFollowUp(t *testing.T, s *Store, task api.Task, title string) api.WorkItem {
	t.Helper()
	ctx := context.Background()
	parent, err := s.CreateWorkItem(ctx, task.ID, api.CreateWorkItemRequest{Kind: "feature", Title: "Reviewed parent " + title, Priority: "normal", RequestID: api.NewID("req")}, stewardBy)
	if err != nil {
		t.Fatal(err)
	}
	m, err := s.PostMessage(ctx, task.ID, api.PostMessageRequest{Text: "synthetic review result for " + title}, stewardBy)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	out, err := s.fileReviewFollowUp(ctx, tx, parent, api.ReviewFinding{ID: "f1", Kind: "bug", Title: title}, m, stewardBy)
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	item, err := s.GetWorkItem(ctx, task.ID, out.ItemID)
	if err != nil {
		t.Fatal(err)
	}
	return item
}

func TestTriageHeldForTriageListsUnrefinedFollowUps(t *testing.T) {
	s, task := stewardStore(t)
	ctx := context.Background()
	held := fileHeldFollowUp(t, s, task, "Refuse an empty recipient in the review fixture")
	refined := fileHeldFollowUp(t, s, task, "Keep the scroll position after a new message")
	ordinary, err := s.CreateWorkItem(ctx, task.ID, api.CreateWorkItemRequest{Kind: "bug", Title: "An ordinary open item", Priority: "normal", RequestID: api.NewID("req")}, stewardBy)
	if err != nil {
		t.Fatal(err)
	}
	title := "Keep the scroll position after new messages arrive"
	if _, err = s.UpdateWorkItem(ctx, task.ID, refined.ID, api.UpdateWorkItemRequest{Revision: refined.Revision, Title: &title}, stewardBy); err != nil {
		t.Fatal(err)
	}
	got, err := s.WorkItemTriage(ctx, task.ID, 0, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.HeldForTriage) != 1 || got.HeldForTriage[0].Item.ID != held.ID || got.HeldForTriage[0].SourceMessageSeq != held.SourceMessageSeq {
		t.Fatalf("held for triage: %+v (ordinary %s, refined %s)", got.HeldForTriage, ordinary.ID, refined.ID)
	}
	// The existing fields keep their names and shapes.
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"taskId", "staleDays", "generatedAt", "duplicates", "alreadyReleased", "stale", "heldForTriage"} {
		if _, ok := fields[key]; !ok {
			t.Fatalf("triage JSON lacks %s: %s", key, raw)
		}
	}
}

func TestBacklogSummaryRevisionsReplayAndWriters(t *testing.T) {
	s, task := stewardStore(t)
	ctx := context.Background()
	steward := liveSteward(t, s, task.ID, "backlog-steward")
	save := func(key string, expected int64, body string, who api.Agent) (api.BacklogSummary, error) {
		return s.SaveBacklogSummary(ctx, task.ID, api.SaveBacklogSummaryRequest{ExpectedRevision: expected, Body: body, RequestID: key, AgentID: who.ID, RunID: who.RunID}, stewardBy)
	}
	if _, err := s.BacklogSummary(ctx, task.ID, 0); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("summary before any save: %v", err)
	}
	r1, err := save("summary-1", 0, "## Themes\nsynthetic theme one", steward)
	if err != nil || r1.Revision != 1 || r1.AgentID != steward.ID || r1.RunID != steward.RunID || r1.Digest != summaryDigest(r1.Body) {
		t.Fatalf("revision 1: %+v %v", r1, err)
	}
	r2, err := save("summary-2", 1, "## Themes\nsynthetic theme two", steward)
	if err != nil || r2.Revision != 2 {
		t.Fatalf("revision 2: %+v %v", r2, err)
	}
	if _, err = save("summary-stale", 1, "stale edit", steward); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("stale revision: %v", err)
	}
	if replay, err := save("summary-1", 0, "## Themes\nsynthetic theme one", steward); err != nil || replay.Revision != 1 || replay.Digest != r1.Digest {
		t.Fatalf("replay: %+v %v", replay, err)
	}
	if _, err = save("summary-1", 0, "a changed payload", steward); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("changed payload under a used request ID: %v", err)
	}
	worker, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "worker", Host: "mini", Session: "worker", Runtime: "claude"}, stewardBy)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = save("summary-worker", 2, "not the steward", worker); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("ordinary agent save: %v", err)
	}
	// The owner may save; a retired steward may not.
	if r3, err := save("summary-owner", 2, "owner correction", api.Agent{}); err != nil || r3.Revision != 3 || r3.AgentID != "" {
		t.Fatalf("owner save: %+v %v", r3, err)
	}
	setAgentStatus(t, s, steward.ID, api.AgentRetired)
	if _, err = save("summary-retired", 3, "retired steward", steward); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("retired steward save: %v", err)
	}
	latest, err := s.BacklogSummary(ctx, task.ID, 0)
	if err != nil || latest.Revision != 3 || latest.Body != "owner correction" {
		t.Fatalf("latest: %+v %v", latest, err)
	}
	named, err := s.BacklogSummary(ctx, task.ID, 2)
	if err != nil || named.Body != r2.Body {
		t.Fatalf("named revision: %+v %v", named, err)
	}
	list, err := s.BacklogSummaryRevisions(ctx, task.ID)
	if err != nil || len(list) != 3 || list[0].Revision != 1 || list[2].Revision != 3 || list[0].Body != "" || list[1].Bytes != len(r2.Body) {
		t.Fatalf("revisions: %+v %v", list, err)
	}
	status, err := s.BacklogStewardStatus(ctx, task.ID)
	if err != nil || status.SummaryRevision != 3 || status.SummaryDigest != latest.Digest {
		t.Fatalf("status summary pointer: %+v %v", status, err)
	}
	if _, err = save("summary-empty", 3, "   ", api.Agent{}); !errors.Is(err, api.ErrInvalid) {
		t.Fatalf("empty summary: %v", err)
	}
}

func TestStewardRefusedRecordsWritesWhileHandlerSucceeds(t *testing.T) {
	s, task := stewardStore(t)
	ctx := context.Background()
	handler, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: "db-handler", Role: api.AgentRoleDatabaseHandler, Host: "mini", Session: "handler"}, stewardBy)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.PostEvent(ctx, task.ID, api.PostEventRequest{AgentID: handler.ID, RunID: handler.RunID, Kind: api.EventRunning}, stewardBy); err != nil {
		t.Fatal(err)
	}
	steward := liveSteward(t, s, task.ID, "backlog-steward")
	other, err := s.CreateTask(ctx, api.CreateTaskRequest{Name: "Dispatch target"}, stewardBy)
	if err != nil {
		t.Fatal(err)
	}
	item, err := s.CreateWorkItem(ctx, task.ID, api.CreateWorkItemRequest{Kind: "bug", Title: "Filed by the owner", Description: "owner acceptance and files", Priority: "normal", RequestID: "owner-item"}, stewardBy)
	if err != nil {
		t.Fatal(err)
	}
	source, err := s.PostMessage(ctx, task.ID, api.PostMessageRequest{Text: "Owner intake evidence", RequestID: "source"}, stewardBy)
	if err != nil {
		t.Fatal(err)
	}
	link := []api.MessageWorkItem{{ItemTaskID: task.ID, ItemID: item.ID, ItemRevision: item.Revision, Relationship: "primary"}}
	order, err := s.PostMessage(ctx, task.ID, api.PostMessageRequest{Text: "bounded owner order", AuditKind: api.MessageAuditWork, RequestID: "order", WorkItems: link}, stewardBy)
	if err != nil {
		t.Fatal(err)
	}
	wantWrite := func(name string, err error) {
		t.Helper()
		if stewardCode(err) != api.StewardRefusedWrite || !strings.Contains(err.Error(), "files through the database handler") {
			t.Fatalf("%s by the steward: %v", name, err)
		}
	}
	title := "Retitled by the steward"
	_, err = s.CreateWorkItem(ctx, task.ID, api.CreateWorkItemRequest{Kind: "bug", Title: "Drafted by the steward", Priority: "normal", AgentID: steward.ID, RequestID: "steward-create"}, stewardBy)
	wantWrite("create", err)
	_, err = s.UpdateWorkItem(ctx, task.ID, item.ID, api.UpdateWorkItemRequest{Revision: item.Revision, Title: &title, AgentID: steward.ID}, stewardBy)
	wantWrite("update", err)
	_, _, err = s.CreateWorkItemUpdate(ctx, task.ID, item.ID, api.CreateWorkItemUpdate{ExpectedRevision: item.Revision, Title: &title, AgentID: steward.ID, RunID: steward.RunID, RequestID: "steward-keyed-update"}, stewardBy)
	wantWrite("keyed update", err)
	_, err = s.DispatchWorkItem(ctx, task.ID, item.ID, api.DispatchWorkItemRequest{Revision: item.Revision, TargetTaskID: other.ID, AgentID: steward.ID, RequestID: "steward-dispatch"}, stewardBy)
	wantWrite("dispatch", err)
	correct := api.CorrectMessageAuditRequest{RequestID: "steward-correct", ExpectedRevision: 1, Reason: "steward correction",
		Sources: []api.MessageReference{{TaskID: task.ID, Seq: source.Seq}}, Desired: api.MessageAuditDesiredState{Classification: api.MessageAuditIntake}, AgentID: steward.ID, RunID: steward.RunID}
	_, _, err = s.CorrectMessageAudit(ctx, task.ID, order.Seq, correct, stewardBy)
	wantWrite("message-audit correction", err)
	_, _, err = s.ResolveMessageAudit(ctx, task.ID, order.Seq, api.ResolveMessageAuditRequest{RequestID: "steward-resolve", ExpectedRevision: 1, Reason: "steward resolve",
		ExistingItem: &link[0], Sources: []api.MessageReference{{TaskID: task.ID, Seq: source.Seq}}, AgentID: steward.ID, RunID: steward.RunID}, stewardBy)
	wantWrite("message-audit resolution", err)
	// Handler-run records already require the exact handler run.
	if _, _, err = s.CreateMessageAuditAssociation(ctx, task.ID, api.CreateMessageAuditAssociationRequest{RequestID: "steward-associate", Item: api.MessageAuditItemReference{ItemTaskID: task.ID, ItemID: item.ID, ItemRevision: item.Revision},
		Source: api.MessageReference{TaskID: task.ID, Seq: source.Seq}, Reason: "steward association", AgentID: steward.ID, RunID: steward.RunID}, stewardBy); err == nil {
		t.Fatal("message-audit association by the steward")
	}
	scope := api.ConfirmWorkOrderScopeRequest{RequestID: "steward-scope", AgentID: steward.ID, RunID: steward.RunID, ExpectedRevision: item.Revision, ScopeRevision: item.ScopeRevision, OrderMessageSeq: order.Seq, Complete: true}
	if _, err = s.ConfirmWorkOrderScope(ctx, task.ID, item.ID, scope); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("scope confirm by the steward: %v", err)
	}
	if _, err = s.SaveVerification(ctx, task.ID, item.ID, api.VerificationRequest{RequestID: "steward-plan", AgentID: steward.ID, RunID: steward.RunID, Plan: &api.VerificationPlan{}}); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("verification plan by the steward: %v", err)
	}
	if _, err = s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: "steward-release", Operation: "enqueue", AgentID: steward.ID, RunID: steward.RunID, EntryID: api.NewID("tqe")}); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("release enqueue by the steward: %v", err)
	}
	if got, _ := s.GetWorkItem(ctx, task.ID, item.ID); got.Revision != item.Revision || got.Title != item.Title {
		t.Fatalf("a refused steward write changed the item: %+v", got)
	}
	// The same calls from the handler still succeed.
	if _, err = s.CreateWorkItem(ctx, task.ID, api.CreateWorkItemRequest{Kind: "bug", Title: "Drafted by the steward, filed by the handler", Priority: "normal", AgentID: handler.ID, SourceMessageSeq: source.Seq, RequestID: "handler-create"}, stewardBy); err != nil {
		t.Fatalf("handler create: %v", err)
	}
	if _, err = s.ConfirmWorkOrderScope(ctx, task.ID, item.ID, api.ConfirmWorkOrderScopeRequest{RequestID: "handler-scope", AgentID: handler.ID, RunID: handler.RunID, ExpectedRevision: item.Revision, ScopeRevision: item.ScopeRevision, OrderMessageSeq: order.Seq, Complete: true}); err != nil {
		t.Fatalf("handler scope confirm: %v", err)
	}
	correct.RequestID, correct.AgentID, correct.RunID = "handler-correct", handler.ID, handler.RunID
	if _, _, err = s.CorrectMessageAudit(ctx, task.ID, order.Seq, correct, stewardBy); err != nil {
		t.Fatalf("handler message-audit correction: %v", err)
	}
	if updated, err := s.UpdateWorkItem(ctx, task.ID, item.ID, api.UpdateWorkItemRequest{Revision: item.Revision, Title: &title, AgentID: handler.ID}, stewardBy); err != nil || updated.Title != title {
		t.Fatalf("handler update: %+v %v", updated, err)
	}
	// The steward still posts ordinary messages.
	env := api.Envelope{Kind: api.EnvelopeKindNotice, Subject: "Backlog summary revision two is saved", Body: api.EnvelopeBody{Text: "Themes regrouped."}}
	if _, err = s.PostMessage(ctx, task.ID, api.PostMessageRequest{Envelope: &env, Text: api.RenderText(env), AgentID: steward.ID, RunID: steward.RunID}, stewardBy); err != nil {
		t.Fatalf("steward notice: %v", err)
	}
}

// Steward rotation store tests (wi_5b4b94dbc9a11e8b, order #14942): an
// isolated SQLite hub per test and synthetic agents only.

type stewardRotationFixture struct {
	s      *Store
	task   api.Task
	old    api.Agent
	worker api.Agent
}

func newStewardRotationFixture(t *testing.T, summary bool) *stewardRotationFixture {
	t.Helper()
	s, task := stewardStore(t)
	f := &stewardRotationFixture{s: s, task: task, old: liveSteward(t, s, task.ID, "backlog-steward")}
	var err error
	if f.worker, err = s.AddAgent(context.Background(), task.ID, api.AddAgentRequest{Name: "worker", Host: "mini", Session: "worker", Runtime: "claude"}, stewardBy); err != nil {
		t.Fatal(err)
	}
	if summary {
		if _, err = s.SaveBacklogSummary(context.Background(), task.ID, api.SaveBacklogSummaryRequest{Body: "## Themes\nsynthetic", RequestID: "summary-1", AgentID: f.old.ID, RunID: f.old.RunID}, stewardBy); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func (f *stewardRotationFixture) prepare(key, successor string) (api.StewardRotation, error) {
	return f.s.StewardRotationAction(context.Background(), f.task.ID, api.StewardRotationRequest{Operation: api.StewardRotationPrepare, RequestID: key, OldAgentID: f.old.ID, OldRunID: f.old.RunID,
		SuccessorAgentID: successor, SuccessorName: api.StewardSuccessorName(f.old.Name), Reason: api.StewardRotationReasonManual, Trigger: api.StewardRotationTriggerOwner}, stewardBy)
}

func (f *stewardRotationFixture) act(op, key, rotation string) (api.StewardRotation, error) {
	return f.s.StewardRotationAction(context.Background(), f.task.ID, api.StewardRotationRequest{Operation: op, RequestID: key, RotationID: rotation}, stewardBy)
}

// admit registers the prepared successor on the old steward's host, runtime
// and directory; online also marks it live.
func (f *stewardRotationFixture) admit(t *testing.T, id string, online bool) api.Agent {
	t.Helper()
	a, err := f.s.AddAgent(context.Background(), f.task.ID, stewardRequest(id, api.StewardSuccessorName(f.old.Name)), stewardBy)
	if err != nil {
		t.Fatalf("admit successor: %v", err)
	}
	if online {
		if _, err = f.s.db.Exec(`UPDATE agents SET last_seen_at=?,status='running' WHERE id=?`, ts(time.Now().UTC()), a.ID); err != nil {
			t.Fatal(err)
		}
	}
	return a
}

func (f *stewardRotationFixture) holders(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.s.db.QueryRow(`SELECT count(*) FROM agents WHERE task_id=? AND role=? AND status<>'closed' AND steward_pending=0`, f.task.ID, api.AgentRoleBacklogSteward).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (f *stewardRotationFixture) activity(t *testing.T, a api.Agent, state, tool string, tokens int64) {
	t.Helper()
	now := time.Now().UTC()
	activity := api.AgentActivity{State: state, ObservedAt: now, LastEventAt: now, PendingTool: tool, Tokens: api.TokenTotals{Total: tokens}}
	if tool != "" {
		activity.PendingSince = now
	}
	if _, err := f.s.ReportActivity(context.Background(), f.task.ID, a.ID, api.ActivityReport{RequestID: api.NewID("act"), RunID: a.RunID, Activity: activity}); err != nil {
		t.Fatal(err)
	}
}

func stewardCode(err error) string {
	var refusal *api.StewardRefusal
	if errors.As(err, &refusal) {
		return refusal.Code
	}
	return ""
}

func TestStewardRotationPrepareRefusals(t *testing.T) {
	bare := newStewardRotationFixture(t, false)
	if _, err := bare.prepare("no-summary", api.NewID("agt")); stewardCode(err) != api.StewardRefusedSummaryMissing {
		t.Fatalf("without a summary: %v", err)
	}
	f := newStewardRotationFixture(t, true)
	ctx := context.Background()
	f.activity(t, f.old, "working", "", 0)
	if _, err := f.prepare("busy", api.NewID("agt")); stewardCode(err) != api.StewardRefusedWorking {
		t.Fatalf("busy steward: %v", err)
	}
	f.activity(t, f.old, "idle", "Bash", 0)
	if _, err := f.prepare("pending-tool", api.NewID("agt")); stewardCode(err) != api.StewardRefusedPendingTool {
		t.Fatalf("pending tool: %v", err)
	}
	// Activity reports only change on a new state.
	f.activity(t, f.old, "working", "", 0)
	f.activity(t, f.old, "idle", "", 0)
	bad := api.StewardRotationRequest{Operation: api.StewardRotationPrepare, RequestID: "wrong-name", OldAgentID: f.old.ID, OldRunID: f.old.RunID, SuccessorAgentID: api.NewID("agt"),
		SuccessorName: "someone-else", Reason: api.StewardRotationReasonManual, Trigger: api.StewardRotationTriggerOwner}
	if _, err := f.s.StewardRotationAction(ctx, f.task.ID, bad, stewardBy); stewardCode(err) != api.StewardRefusedNameTaken {
		t.Fatalf("wrong successor name: %v", err)
	}
	bad.RequestID, bad.SuccessorName, bad.OldRunID = "stale-run", api.StewardSuccessorName(f.old.Name), api.NewID("run")
	if _, err := f.s.StewardRotationAction(ctx, f.task.ID, bad, stewardBy); stewardCode(err) != api.StewardRefusedNotSteward {
		t.Fatalf("stale old run: %v", err)
	}
	for _, op := range []string{api.StewardRotationPrepare, api.StewardRotationCommit, api.StewardRotationAbort} {
		if _, err := f.s.StewardRotationAction(ctx, f.task.ID, api.StewardRotationRequest{Operation: op, RequestID: "agent-" + op, ActorAgentID: f.worker.ID}, stewardBy); stewardCode(err) != api.StewardRefusedAgentCaller {
			t.Fatalf("agent caller %s: %v", op, err)
		}
	}
	r, err := f.prepare("first", api.NewID("agt"))
	if err != nil || r.State != api.StewardRotationPrepared || r.SummaryRevision != 1 || r.SuccessorName != "backlog-steward-r2" {
		t.Fatalf("prepare: %+v %v", r, err)
	}
	if replay, err := f.prepare("first", r.SuccessorAgentID); err != nil || replay.ID != r.ID {
		t.Fatalf("prepare replay: %+v %v", replay, err)
	}
	if _, err = f.prepare("second", api.NewID("agt")); stewardCode(err) != api.StewardRefusedRotationOpen {
		t.Fatalf("second prepare: %v", err)
	}
	// While prepared, any steward admission but the named successor is refused.
	if _, err = f.s.AddAgent(ctx, f.task.ID, stewardRequest(api.NewID("agt"), "intruder"), stewardBy); stewardCode(err) != api.StewardRefusedActive {
		t.Fatalf("other steward while prepared: %v", err)
	}
	if f.holders(t) != 1 {
		t.Fatal("a refused prepare changed the holder")
	}
}

func TestStewardRotationCommitMovesObligationsAndHandsOffSummary(t *testing.T) {
	f := newStewardRotationFixture(t, true)
	ctx := context.Background()
	byRole, err := stewardRequestMessage(f.s, f.task.ID, f.worker, "Intake before the rotation")
	if err != nil {
		t.Fatal(err)
	}
	env := api.Envelope{Kind: api.EnvelopeKindQuestion, To: f.old.Name, Subject: "Which batch does this belong to", Body: api.EnvelopeBody{Question: "Which batch?"}}
	byName, err := f.s.PostMessage(ctx, f.task.ID, api.PostMessageRequest{Envelope: &env, Text: api.RenderText(env), To: f.old.ID, AgentID: f.worker.ID, RunID: f.worker.RunID}, stewardBy)
	if err != nil {
		t.Fatal(err)
	}
	ask, err := f.s.CreateDecision(ctx, f.task.ID, api.CreateDecisionRequest{AgentID: f.old.ID, RequestID: "steward-ask", DecisionRequest: api.DecisionRequest{
		Question: "Should the two scroll items ship together?", RecommendedOptionID: "yes", RecommendationReason: "Same file.",
		Options: []api.DecisionOption{{ID: "yes", Label: "Yes", Description: "One team."}, {ID: "no", Label: "No", Description: "Apart."}}}}, stewardBy)
	if err != nil {
		t.Fatal(err)
	}
	r, err := f.prepare("prepare", api.NewID("agt"))
	if err != nil {
		t.Fatal(err)
	}
	// Launch failure: no successor is registered, so commit refuses and the
	// old steward keeps the role and its obligations.
	if _, err = f.act(api.StewardRotationCommit, "commit-early", r.ID); stewardCode(err) != api.StewardRefusedSuccessor {
		t.Fatalf("commit without a successor: %v", err)
	}
	successor := f.admit(t, r.SuccessorAgentID, false)
	status, err := f.s.BacklogStewardStatus(ctx, f.task.ID)
	if err != nil || status.Steward == nil || status.Steward.ID != f.old.ID || status.PendingSuccessorID != successor.ID || f.holders(t) != 1 {
		t.Fatalf("status while prepared: %+v %v", status, err)
	}
	during, err := stewardRequestMessage(f.s, f.task.ID, f.worker, "Intake while the successor is pending")
	if err != nil || during.To != f.old.ID {
		t.Fatalf("role:backlog_steward while prepared: %+v %v", during, err)
	}
	if _, err = f.s.WorkItemTriage(ctx, f.task.ID, 0, successor.ID, successor.RunID); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("pending successor triage: %v", err)
	}
	if _, err = f.act(api.StewardRotationCommit, "commit-offline", r.ID); stewardCode(err) != api.StewardRefusedSuccessor {
		t.Fatalf("commit with an offline successor: %v", err)
	}
	if _, err = f.s.db.Exec(`UPDATE agents SET last_seen_at=?,status='running' WHERE id=?`, ts(time.Now().UTC()), successor.ID); err != nil {
		t.Fatal(err)
	}
	committed, err := f.act(api.StewardRotationCommit, "commit", r.ID)
	if err != nil || committed.State != api.StewardRotationCommitted || committed.Receipt == nil || committed.Handoff == nil {
		t.Fatalf("commit: %+v %v", committed, err)
	}
	if again, err := f.act(api.StewardRotationCommit, "commit", r.ID); err != nil || again.Receipt == nil || again.Receipt.NoticeSeq != committed.Receipt.NoticeSeq {
		t.Fatalf("commit replay: %+v %v", again, err)
	}
	old, _ := f.s.GetAgent(ctx, f.old.ID)
	if old.Status != api.AgentClosed || f.holders(t) != 1 {
		t.Fatalf("after commit: old %s, holders %d", old.Status, f.holders(t))
	}
	status, err = f.s.BacklogStewardStatus(ctx, f.task.ID)
	if err != nil || status.Steward == nil || status.Steward.ID != successor.ID || status.PendingSuccessorID != "" {
		t.Fatalf("status after commit: %+v %v", status, err)
	}
	h := committed.Handoff
	if h.SummaryRevision != 1 || h.SummaryDigest != summaryDigest("## Themes\nsynthetic") || !h.SummaryFromOldRun || len(h.OpenDecisions) != 1 || h.OpenDecisions[0].MessageSeq != ask.Seq {
		t.Fatalf("handoff: %+v", h)
	}
	moved := map[int64]bool{}
	for _, p := range h.Reissued {
		moved[p.OldMessageSeq] = true
	}
	if len(h.Reissued) != 3 || !moved[byRole.Seq] || !moved[byName.Seq] || !moved[during.Seq] {
		t.Fatalf("reissued: %+v", h.Reissued)
	}
	open, err := f.s.ListObligations(ctx, f.task.ID, ObligationFilter{AgentID: successor.ID, OpenOnly: true}, f.s.now())
	reassigned := 0
	for _, o := range open {
		if strings.HasPrefix(o.Subject, "Reassigned to "+successor.Name) {
			reassigned++
		}
	}
	if err != nil || reassigned != 3 || len(open) != 4 { // three moved plus the handoff notice
		t.Fatalf("successor obligations: %+v %v", open, err)
	}
	if left, _ := f.s.ListObligations(ctx, f.task.ID, ObligationFilter{AgentID: f.old.ID, OpenOnly: true}, f.s.now()); len(left) != 0 {
		t.Fatalf("obligations left on the old steward: %+v", left)
	}
	msgs, err := f.s.ListMessages(ctx, f.task.ID, 0, successor.ID, 500)
	if err != nil {
		t.Fatal(err)
	}
	notices := 0
	for _, m := range msgs {
		if m.Seq == committed.Receipt.NoticeSeq && m.To == successor.ID && strings.Contains(m.Text, r.ID) && strings.Contains(m.Text, "summary revision 1") {
			notices++
		}
	}
	if notices != 1 {
		t.Fatalf("handoff notice to the successor: %d", notices)
	}
	if after, err := stewardRequestMessage(f.s, f.task.ID, f.worker, "Intake after the rotation"); err != nil || after.To != successor.ID {
		t.Fatalf("role:backlog_steward after commit: %+v %v", after, err)
	}
}

func TestStewardRotationLaunchFailureAbortAndRetry(t *testing.T) {
	f := newStewardRotationFixture(t, true)
	ctx := context.Background()
	intake, err := stewardRequestMessage(f.s, f.task.ID, f.worker, "Intake before the failed launch")
	if err != nil {
		t.Fatal(err)
	}
	r, err := f.prepare("prepare-1", api.NewID("agt"))
	if err != nil {
		t.Fatal(err)
	}
	// The successor registered but never came online.
	successor := f.admit(t, r.SuccessorAgentID, false)
	if _, err = f.act(api.StewardRotationCommit, "commit-1", r.ID); stewardCode(err) != api.StewardRefusedSuccessor {
		t.Fatalf("commit with a successor that never came online: %v", err)
	}
	if got, _ := f.s.GetStewardRotation(ctx, f.task.ID, r.ID); got.State != api.StewardRotationPrepared {
		t.Fatalf("rotation after refused commit: %+v", got)
	}
	if m, err := stewardRequestMessage(f.s, f.task.ID, f.worker, "Intake after the failed launch"); err != nil || m.To != f.old.ID {
		t.Fatalf("old steward lost the role: %+v %v", m, err)
	}
	if open, _ := f.s.ListObligations(ctx, f.task.ID, ObligationFilter{AgentID: f.old.ID, OpenOnly: true}, f.s.now()); len(open) != 2 || open[0].MessageSeq != intake.Seq {
		t.Fatalf("old steward's obligations changed: %+v", open)
	}
	aborted, err := f.act(api.StewardRotationAbort, "abort-1", r.ID)
	if err != nil || aborted.State != api.StewardRotationAborted {
		t.Fatalf("abort: %+v %v", aborted, err)
	}
	if a, _ := f.s.GetAgent(ctx, successor.ID); a.Status != api.AgentClosed {
		t.Fatalf("pending successor after abort: %+v", a)
	}
	if a, _ := f.s.GetAgent(ctx, f.old.ID); a.Status != api.AgentRunning || f.holders(t) != 1 {
		t.Fatalf("old steward after abort: %+v", a)
	}
	if _, err = f.act(api.StewardRotationCommit, "commit-aborted", r.ID); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("commit after abort: %v", err)
	}
	// A new rotation with a new successor ID succeeds.
	r2, err := f.prepare("prepare-2", api.NewID("agt"))
	if err != nil || r2.ID == r.ID {
		t.Fatalf("second prepare: %+v %v", r2, err)
	}
	f.admit(t, r2.SuccessorAgentID, true)
	if done, err := f.act(api.StewardRotationCommit, "commit-2", r2.ID); err != nil || done.State != api.StewardRotationCommitted || f.holders(t) != 1 {
		t.Fatalf("second rotation: %+v %v", done, err)
	}
}

func TestStewardRotationBusyCommitStaysPrepared(t *testing.T) {
	f := newStewardRotationFixture(t, true)
	ctx := context.Background()
	r, err := f.prepare("prepare", api.NewID("agt"))
	if err != nil {
		t.Fatal(err)
	}
	f.admit(t, r.SuccessorAgentID, true)
	f.activity(t, f.old, "working", "", 0)
	if _, err = f.act(api.StewardRotationCommit, "commit-busy", r.ID); stewardCode(err) != api.StewardRefusedWorking {
		t.Fatalf("commit while working: %v", err)
	}
	if got, _ := f.s.GetStewardRotation(ctx, f.task.ID, r.ID); got.State != api.StewardRotationPrepared {
		t.Fatalf("rotation after busy commit: %+v", got)
	}
	f.activity(t, f.old, "idle", "", 0)
	if done, err := f.act(api.StewardRotationCommit, "commit-idle", r.ID); err != nil || done.State != api.StewardRotationCommitted {
		t.Fatalf("commit when idle: %+v %v", done, err)
	}
}

// A pause closes the old steward mid-rotation; the prepared record then holds
// nothing open, and the next steward and rotation proceed.
func TestStewardRotationStaleAfterOldStewardCloses(t *testing.T) {
	f := newStewardRotationFixture(t, true)
	ctx := context.Background()
	r, err := f.prepare("prepare", api.NewID("agt"))
	if err != nil {
		t.Fatal(err)
	}
	f.admit(t, r.SuccessorAgentID, false)
	setAgentStatus(t, f.s, f.old.ID, api.AgentClosed)
	setAgentStatus(t, f.s, r.SuccessorAgentID, api.AgentClosed)
	fresh := liveSteward(t, f.s, f.task.ID, "backlog-steward")
	f.old = fresh
	r2, err := f.prepare("prepare-after-close", api.NewID("agt"))
	if err != nil {
		t.Fatalf("prepare after the old steward closed: %v", err)
	}
	if stale, _ := f.s.GetStewardRotation(ctx, f.task.ID, r.ID); stale.State != api.StewardRotationAborted || r2.State != api.StewardRotationPrepared {
		t.Fatalf("stale rotation: %+v", stale)
	}
}

func TestStewardRotationPolicyDefaultsRevisionAndOwnerOnly(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "hub.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	existing, err := s.CreateTask(ctx, api.CreateTaskRequest{Name: "existing project"}, stewardBy)
	if err != nil {
		t.Fatal(err)
	}
	// A hub that predates steward rotation: the first migration turns it off
	// for projects that already exist.
	if _, err = s.db.Exec(`DROP TABLE steward_rotation_policy`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if s, err = Open(path); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	p, err := s.StewardRotationPolicy(ctx, existing.ID)
	if err != nil || p.Enabled || p.Revision != 1 || p.MaxTotalTokens != 300_000_000 || !p.OnTemplateChange {
		t.Fatalf("existing project must be opt-in: %+v %v", p, err)
	}
	fresh, err := s.CreateTask(ctx, api.CreateTaskRequest{Name: "new project"}, stewardBy)
	if err != nil {
		t.Fatal(err)
	}
	p, err = s.StewardRotationPolicy(ctx, fresh.ID)
	if err != nil || !p.Enabled || p.Revision != 0 || p.MaxTotalTokens != api.DefaultHandlerRotationMaxTotalTokens || !p.OnTemplateChange {
		t.Fatalf("new project default: %+v %v", p, err)
	}
	set := api.StewardRotationPolicyRequest{ExpectedRevision: 0, Enabled: true, MaxTotalTokens: 5000, OnTemplateChange: false}
	agentSet := set
	agentSet.ActorAgentID = api.NewID("agt")
	if _, err = s.SetStewardRotationPolicy(ctx, fresh.ID, agentSet, stewardBy); stewardCode(err) != api.StewardRefusedAgentCaller {
		t.Fatalf("agent caller: %v", err)
	}
	stale := set
	stale.ExpectedRevision = 4
	if _, err = s.SetStewardRotationPolicy(ctx, fresh.ID, stale, stewardBy); stewardCode(err) != api.StewardRefusedStalePolicy {
		t.Fatalf("stale revision: %v", err)
	}
	saved, err := s.SetStewardRotationPolicy(ctx, fresh.ID, set, stewardBy)
	if err != nil || saved.Revision != 1 || saved.MaxTotalTokens != 5000 || saved.OnTemplateChange || !saved.Enabled {
		t.Fatalf("saved: %+v %v", saved, err)
	}
	if _, err = s.SetStewardRotationPolicy(ctx, fresh.ID, set, stewardBy); stewardCode(err) != api.StewardRefusedStalePolicy {
		t.Fatalf("reused revision: %v", err)
	}
}

func TestStewardRotationDueListReasons(t *testing.T) {
	f := newStewardRotationFixture(t, true)
	ctx := context.Background()
	current := strings.Repeat("c", 64)
	if _, err := f.s.db.Exec(`INSERT INTO steward_runs(task_id,agent_id,run_id,template_digest,created_at) VALUES(?,?,?,?,?)`, f.task.ID, f.old.ID, f.old.RunID, current, ts(f.s.now())); err != nil {
		t.Fatal(err)
	}
	due := func(host, digest string) []api.StewardRotationDue {
		t.Helper()
		list, err := f.s.StewardRotationsDue(ctx, host, digest)
		if err != nil {
			t.Fatal(err)
		}
		return list.Entries
	}
	got := due("mini", current)
	if len(got) != 1 || len(got[0].DueReasons) != 0 || !got[0].Idle || !got[0].DigestMatches || got[0].SummaryRevision != 1 || got[0].Agent.ID != f.old.ID {
		t.Fatalf("current steward: %+v", got)
	}
	if got = due("mini", strings.Repeat("d", 64)); len(got) != 1 || len(got[0].DueReasons) != 1 || got[0].DueReasons[0] != api.StewardRotationReasonTemplate {
		t.Fatalf("template change: %+v", got)
	}
	f.activity(t, f.old, "working", "", 0)
	f.activity(t, f.old, "idle", "", api.DefaultHandlerRotationMaxTotalTokens)
	if got = due("mini", current); len(got) != 1 || len(got[0].DueReasons) != 1 || got[0].DueReasons[0] != api.StewardRotationReasonTokens || got[0].TotalTokens != api.DefaultHandlerRotationMaxTotalTokens {
		t.Fatalf("token limit: %+v", got)
	}
	f.activity(t, f.old, "working", "", api.DefaultHandlerRotationMaxTotalTokens)
	if got = due("mini", current); len(got) != 1 || got[0].Idle || got[0].ActivityState != "working" {
		t.Fatalf("busy steward reported idle: %+v", got)
	}
	if got = due("other-host", current); len(got) != 0 {
		t.Fatalf("another host: %+v", got)
	}
	p, err := f.s.StewardRotationPolicy(ctx, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.SetStewardRotationPolicy(ctx, f.task.ID, api.StewardRotationPolicyRequest{ExpectedRevision: p.Revision, Enabled: false, MaxTotalTokens: p.MaxTotalTokens, OnTemplateChange: true}, stewardBy); err != nil {
		t.Fatal(err)
	}
	if got = due("mini", current); len(got) != 0 {
		t.Fatalf("disabled policy: %+v", got)
	}
}
