package store

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

func helperRequest(key string) api.RegisterOwnerHelperRequest {
	return api.RegisterOwnerHelperRequest{Host: "owner-host", Session: "owner", Runtime: "claude", Cwd: "/work/tailterm", RequestID: key}
}

func registerHelper(t *testing.T, s *Store, task string, req api.RegisterOwnerHelperRequest, by api.Caller) api.OwnerActionResult {
	t.Helper()
	out, err := s.RegisterOwnerHelper(context.Background(), task, req, by)
	if err != nil {
		t.Fatal(err)
	}
	if out.Agent == nil || out.Registration == nil {
		t.Fatalf("register result %+v", out)
	}
	return out
}

func helperEvents(t *testing.T, s *Store, task, agent string) []api.Event {
	t.Helper()
	events, err := s.ListEvents(context.Background(), task, 0, 500)
	if err != nil {
		t.Fatal(err)
	}
	var out []api.Event
	for _, e := range events {
		if e.AgentID == agent && e.Kind == api.EventAgentAdded {
			out = append(out, e)
		}
	}
	return out
}

func TestOwnerHelperRegister(t *testing.T) {
	f := newDeliveryFixture(t)
	ctx := context.Background()
	f.s.now = func() time.Time { return time.Now().UTC() } // Online is judged on the wall clock
	out := registerHelper(t, f.s, f.task.ID, helperRequest("reg-1"), f.by)
	a, r := *out.Agent, *out.Registration
	if a.Role != api.AgentRoleOwnerHelper || a.Name != api.DefaultOwnerHelperName || a.Runtime != "claude" || a.Host != "owner-host" || a.Session != "owner" || a.Cwd != "/work/tailterm" ||
		a.Status != api.AgentRunning || !a.Online || a.ParentAgentID != "" || !a.CleanupDone || !api.ValidID(a.ID, "agt") || !validRunID(a.RunID) {
		t.Fatalf("agent %+v", a)
	}
	got, err := f.s.GetAgent(ctx, a.ID)
	if err != nil || got.WorkItem != nil || got.RunID != a.RunID || got.Role != api.AgentRoleOwnerHelper {
		t.Fatalf("stored %+v %v", got, err)
	}
	// It reads from its registration on: earlier board history is not unread.
	if got.Unread != 0 {
		t.Fatalf("unread %d", got.Unread)
	}
	if r.Mode != api.OwnerHelperCreated || r.AgentID != a.ID || r.RunID != a.RunID || r.PreviousRunID != "" || r.RequestID != "reg-1" || !strings.HasPrefix(r.ID, "ohr_") || r.By.User != "owner" {
		t.Fatalf("receipt %+v", r)
	}
	list, err := f.s.ListOwnerHelperRegistrations(ctx, f.task.ID)
	if err != nil || len(list) != 1 || list[0].ID != r.ID || list[0].Session != "owner" || list[0].Mode != api.OwnerHelperCreated {
		t.Fatalf("receipts %+v %v", list, err)
	}
	events := helperEvents(t, f.s, f.task.ID, a.ID)
	if len(events) != 1 || events[0].Data["role"] != api.AgentRoleOwnerHelper || events[0].Data["runId"] != a.RunID || events[0].Data["registration"] != r.ID {
		t.Fatalf("events %+v", events)
	}
	for _, bad := range []api.RegisterOwnerHelperRequest{
		{Host: "h", Session: "owner", Runtime: "codex", RequestID: "bad-runtime"},
		{Host: "h", Session: "bad name", Runtime: "claude", RequestID: "bad-session"},
		{Session: "owner", Runtime: "claude", RequestID: "bad-host"},
		{Host: "h", Session: "owner", Runtime: "claude", Cwd: "relative", RequestID: "bad-cwd"},
		{Host: "h", Session: "owner", Runtime: "claude"},
	} {
		if _, err := f.s.RegisterOwnerHelper(ctx, f.task.ID, bad, f.by); !errors.Is(err, api.ErrInvalid) {
			t.Fatalf("%+v: %v", bad, err)
		}
	}
}

func TestOwnerHelperReregisterReplacesRun(t *testing.T) {
	f := newDeliveryFixture(t)
	ctx := context.Background()
	first := registerHelper(t, f.s, f.task.ID, helperRequest("reg-1"), f.by)
	req := helperRequest("reg-2")
	req.Session, req.Host = "owner-2", "owner-host-2"
	second := registerHelper(t, f.s, f.task.ID, req, f.by)
	if second.Agent.ID != first.Agent.ID || second.Agent.RunID == first.Agent.RunID || second.Agent.Session != "owner-2" || second.Agent.Host != "owner-host-2" {
		t.Fatalf("replace %+v", second.Agent)
	}
	if r := second.Registration; r.Mode != api.OwnerHelperReplaced || r.PreviousRunID != first.Agent.RunID || r.RunID != second.Agent.RunID {
		t.Fatalf("receipt %+v", r)
	}
	// The same request ID replays without a new run; other data under it conflicts.
	replay, err := f.s.RegisterOwnerHelper(ctx, f.task.ID, req, f.by)
	if err != nil || !replay.Replay || replay.Agent.RunID != second.Agent.RunID || replay.Registration.ID != second.Registration.ID {
		t.Fatalf("replay %+v %v", replay, err)
	}
	changed := req
	changed.Session = "owner-3"
	if _, err := f.s.RegisterOwnerHelper(ctx, f.task.ID, changed, f.by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("mismatch: %v", err)
	}
	current, _ := f.s.GetAgent(ctx, first.Agent.ID)
	if current.RunID != second.Agent.RunID {
		t.Fatalf("run %s", current.RunID)
	}
	list, _ := f.s.ListOwnerHelperRegistrations(ctx, f.task.ID)
	if len(list) != 2 || len(helperEvents(t, f.s, f.task.ID, first.Agent.ID)) != 2 {
		t.Fatalf("receipts %+v", list)
	}
	// The old run is refused wherever run identity is checked.
	if _, err := f.s.PostEvent(ctx, f.task.ID, api.PostEventRequest{Kind: api.EventHeartbeat, AgentID: first.Agent.ID, RunID: first.Agent.RunID}, f.by); !errors.Is(err, api.ErrInvalid) {
		t.Fatalf("old run heartbeat: %v", err)
	}
	// A different name for the live helper is refused.
	renamed := helperRequest("reg-rename")
	renamed.Name = "helper-two"
	if _, err := f.s.RegisterOwnerHelper(ctx, f.task.ID, renamed, f.by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("rename: %v", err)
	}
	// A retired helper stays retired: only tt resume undoes it.
	if _, err := f.s.db.Exec(`UPDATE agents SET status='retired' WHERE id=?`, first.Agent.ID); err != nil {
		t.Fatal(err)
	}
	retired := registerHelper(t, f.s, f.task.ID, helperRequest("reg-retired"), f.by)
	if retired.Agent.Status != api.AgentRetired || retired.Registration.Mode != api.OwnerHelperReplaced {
		t.Fatalf("retired %+v", retired.Agent)
	}
}

func TestOwnerHelperReattachExited(t *testing.T) {
	f := newDeliveryFixture(t)
	ctx := context.Background()
	first := registerHelper(t, f.s, f.task.ID, helperRequest("reg-1"), f.by)
	if _, err := f.s.PostEvent(ctx, f.task.ID, api.PostEventRequest{Kind: api.EventExited, AgentID: first.Agent.ID, RunID: first.Agent.RunID}, f.by); err != nil {
		t.Fatal(err)
	}
	second := registerHelper(t, f.s, f.task.ID, helperRequest("reg-2"), f.by)
	if second.Agent.ID != first.Agent.ID || second.Agent.RunID == first.Agent.RunID || second.Agent.Status != api.AgentRunning || !second.Agent.CleanupDone ||
		second.Registration.Mode != api.OwnerHelperReattached || second.Registration.PreviousRunID != first.Agent.RunID {
		t.Fatalf("reattach %+v %+v", second.Agent, second.Registration)
	}
}

func TestOwnerHelperClosedCreatesFresh(t *testing.T) {
	f := newDeliveryFixture(t)
	ctx := context.Background()
	first := registerHelper(t, f.s, f.task.ID, helperRequest("reg-1"), f.by)
	closed, err := f.s.CloseAgentRun(ctx, first.Agent.ID, first.Agent.RunID, f.by)
	if err != nil {
		t.Fatal(err)
	}
	// No host cleanup is owed for the owner's own terminal.
	if closed.Status != api.AgentClosed || !closed.CleanupDone {
		t.Fatalf("closed %+v", closed)
	}
	second := registerHelper(t, f.s, f.task.ID, helperRequest("reg-2"), f.by)
	if second.Agent.ID == first.Agent.ID || second.Registration.Mode != api.OwnerHelperCreated || second.Agent.Name != first.Agent.Name {
		t.Fatalf("fresh %+v %+v", second.Agent, second.Registration)
	}
}

func TestOwnerHelperOnePerProject(t *testing.T) {
	f := newDeliveryFixture(t)
	ctx := context.Background()
	first := registerHelper(t, f.s, f.task.ID, helperRequest("reg-1"), f.by)
	// The index refuses a second live helper whatever inserts it.
	if _, err := f.s.db.Exec(`INSERT INTO agents (`+agentCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		api.NewID("agt"), f.task.ID, "second-helper", "h", "s", "claude", "", "", api.AgentRoleOwnerHelper, api.AgentRunning, "", ts(f.s.now()), ts(f.s.now()), api.NewID("run"), "", "", "", true, ""); err == nil {
		t.Fatal("second live helper inserted")
	}
	// Concurrent registrations leave exactly one live helper.
	var wg sync.WaitGroup
	for i := range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = f.s.RegisterOwnerHelper(ctx, f.task.ID, helperRequest("race-"+string(rune('a'+i))), f.by)
		}()
	}
	wg.Wait()
	var live int
	if err := f.s.db.QueryRow(`SELECT count(*) FROM agents WHERE task_id=? AND role=? AND status NOT IN ('closed','exited')`, f.task.ID, api.AgentRoleOwnerHelper).Scan(&live); err != nil || live != 1 {
		t.Fatalf("live helpers %d %v", live, err)
	}
	// A live non-helper agent's name is not taken over.
	clash := helperRequest("clash")
	clash.Name = f.lead.Name
	if _, err := f.s.RegisterOwnerHelper(ctx, f.task.ID, clash, f.by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("name clash: %v", err)
	}
	// Another project has its own helper.
	other, err := f.s.CreateTask(ctx, api.CreateTaskRequest{Name: "Other project"}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	b := registerHelper(t, f.s, other.ID, helperRequest("reg-1"), f.by)
	if b.Agent.ID == first.Agent.ID || b.Registration.Mode != api.OwnerHelperCreated {
		t.Fatalf("project B %+v", b)
	}
	after, _ := f.s.GetAgent(ctx, first.Agent.ID)
	if after.Status != api.AgentRunning || after.TaskID != f.task.ID {
		t.Fatalf("project A helper %+v", after)
	}
	// Only the register route creates the role.
	if _, err := f.s.AddAgent(ctx, f.task.ID, api.AddAgentRequest{Name: "spawned-helper", Role: api.AgentRoleOwnerHelper, AgentID: api.NewID("agt"), Host: "h", Session: "spawned-helper", Runtime: "claude"}, f.by); !errors.Is(err, api.ErrInvalid) {
		t.Fatalf("addAgent owner_helper: %v", err)
	}
	// Spawning an ordinary agent under the helper's name does not take it over.
	if _, err := f.s.AddAgent(ctx, f.task.ID, api.AddAgentRequest{Name: api.DefaultOwnerHelperName, Host: "h", Session: "x", Runtime: "claude"}, f.by); err == nil {
		t.Fatal("ordinary agent took the helper name")
	}
}

func TestOwnerHelperRegisterRefusedWhilePaused(t *testing.T) {
	for _, state := range []string{api.ProjectPauseCleanupPending, api.ProjectPausePaused, api.ProjectPauseResuming} {
		t.Run(state, func(t *testing.T) {
			f := newDeliveryFixture(t)
			ctx := context.Background()
			if _, err := f.s.db.Exec(`UPDATE tasks SET pause_state=? WHERE id=?`, state, f.task.ID); err != nil {
				t.Fatal(err)
			}
			counts := func() [4]int {
				var c [4]int
				for i, q := range []string{`SELECT count(*) FROM agents WHERE task_id=?`, `SELECT count(*) FROM owner_helper_registrations WHERE task_id=?`, `SELECT count(*) FROM events WHERE task_id=?`, `SELECT count(*) FROM owner_actions WHERE task_id=?`} {
					if err := f.s.db.QueryRow(q, f.task.ID).Scan(&c[i]); err != nil {
						t.Fatal(err)
					}
				}
				return c
			}
			before := counts()
			if _, err := f.s.RegisterOwnerHelper(ctx, f.task.ID, helperRequest("paused-new"), f.by); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "paused") {
				t.Fatalf("paused register: %v", err)
			}
			if after := counts(); after != before {
				t.Fatalf("writes while paused %v -> %v", before, after)
			}
			// A live helper's run is not replaced while paused either.
			if _, err := f.s.db.Exec(`UPDATE tasks SET pause_state='active' WHERE id=?`, f.task.ID); err != nil {
				t.Fatal(err)
			}
			live := registerHelper(t, f.s, f.task.ID, helperRequest("live"), f.by)
			if _, err := f.s.db.Exec(`UPDATE tasks SET pause_state=? WHERE id=?`, state, f.task.ID); err != nil {
				t.Fatal(err)
			}
			before = counts()
			if _, err := f.s.RegisterOwnerHelper(ctx, f.task.ID, helperRequest("paused-replace"), f.by); !errors.Is(err, api.ErrConflict) {
				t.Fatalf("paused replace: %v", err)
			}
			if after := counts(); after != before {
				t.Fatalf("writes while paused %v -> %v", before, after)
			}
			if a, _ := f.s.GetAgent(ctx, live.Agent.ID); a.RunID != live.Agent.RunID {
				t.Fatalf("run replaced while paused")
			}
			if _, err := f.s.db.Exec(`UPDATE tasks SET pause_state='active' WHERE id=?`, f.task.ID); err != nil {
				t.Fatal(err)
			}
			if out := registerHelper(t, f.s, f.task.ID, helperRequest("resumed"), f.by); out.Registration.Mode != api.OwnerHelperReplaced {
				t.Fatalf("after resume %+v", out.Registration)
			}
		})
	}
}

// helperDelegationFixture registers the helper and opens a window for it by name.
func helperDelegationFixture(t *testing.T) (delegationFixture, api.Agent, api.DelegationWindow) {
	t.Helper()
	f := newDelegationFixture(t)
	helper := *registerHelper(t, f.s, f.task.ID, helperRequest("reg"), f.by).Agent
	out, err := f.s.OpenDelegationWindow(context.Background(), f.task.ID, api.OpenDelegationWindowRequest{Delegate: api.DefaultOwnerHelperName, EndsAt: f.s.now().Add(3 * time.Hour), Scope: api.DelegationScopeDecisions, Source: &api.DelegationSource{Kind: api.DelegationSourceTT}, RequestID: "open-helper"}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	if out.Window.DelegateAgentID != helper.ID {
		t.Fatalf("window %+v", out.Window)
	}
	return f, helper, *out.Window
}

func TestOwnerHelperDelegatedAnswer(t *testing.T) {
	f, helper, w := helperDelegationFixture(t)
	ctx := context.Background()
	o := f.ownerRequest(t, "req", "", "")
	ask := f.decision(t, "ask", "")
	got := f.window(t)
	if routed(got, api.DelegationRouteObligation, o.MessageSeq) == nil || routed(got, api.DelegationRouteDecision, ask.Seq) == nil {
		t.Fatalf("routes %+v", got.Routes)
	}
	// The open notice plus one directed NOTICE per routed request, in the helper's inbox.
	notices := f.noticesTo(t, helper.ID, w.ID)
	if len(notices) != 3 {
		t.Fatalf("helper notices %d", len(notices))
	}
	stored, _ := f.s.GetAgent(ctx, helper.ID)
	if stored.Unread < 3 {
		t.Fatalf("helper unread %d", stored.Unread)
	}
	for _, n := range notices[1:] {
		if n.Envelope.Kind != api.EnvelopeKindNotice || n.To != helper.ID || !strings.Contains(n.Text, "--rationale") {
			t.Fatalf("notice %+v", n)
		}
	}
	if _, err := f.delegateAnswer("blank", o.ID, "Go with A", "", helper); !errors.Is(err, api.ErrInvalid) {
		t.Fatalf("no rationale: %v", err)
	}
	// Re-registering replaces the run: the window and inbox carry over, the old run is refused.
	replaced := *registerHelper(t, f.s, f.task.ID, helperRequest("reg-again"), f.by).Agent
	if _, err := f.delegateAnswer("old-run", o.ID, "Go with A", "because", helper); !errors.Is(err, api.ErrDelegationForbidden) {
		t.Fatalf("old run: %v", err)
	}
	if _, err := f.s.AnswerDecision(ctx, f.task.ID, ask.Seq, api.AnswerDecisionRequest{RequestID: "d-old", AgentID: helper.ID, RunID: helper.RunID, OptionID: "staged", Rationale: "r"}, f.by); !errors.Is(err, ErrDecisionAnswerForbidden) {
		t.Fatalf("old run decision: %v", err)
	}
	out, err := f.delegateAnswer("ok", o.ID, "Go with A", "A keeps the release reversible.", replaced)
	if err != nil {
		t.Fatal(err)
	}
	if m := out.Message; m.From.AgentID != helper.ID || m.Envelope.Refs["delegated"] != "true" || m.Envelope.Refs["delegation"] != w.ID || m.Envelope.Refs["onBehalfOf"] != "owner" || m.Envelope.Body.Reason != "A keeps the release reversible." {
		t.Fatalf("answer %+v", out.Message)
	}
	answer, err := f.s.AnswerDecision(ctx, f.task.ID, ask.Seq, api.AnswerDecisionRequest{RequestID: "d-ok", AgentID: replaced.ID, RunID: replaced.RunID, OptionID: "staged", Rationale: "Limits impact."}, f.by)
	if err != nil || answer.From.AgentID != helper.ID || answer.Envelope.Refs["delegated"] != "true" {
		t.Fatalf("decision answer %+v %v", answer, err)
	}
}

func TestOwnerHelperMatrixNeverDelegated(t *testing.T) {
	f, helper, _ := helperDelegationFixture(t)
	matrix := f.ownerRequest(t, "matrix", "decision", "verification-matrix-approval:"+strings.Repeat("c", 64))
	o := f.ownerRequest(t, "req", "", "")
	got := f.window(t)
	if routed(got, api.DelegationRouteObligation, matrix.MessageSeq) != nil {
		t.Fatalf("matrix routed to the helper: %+v", got.Routes)
	}
	if _, err := f.delegateAnswer("matrix", matrix.ID, "approve", "because", helper); !errors.Is(err, api.ErrDelegationForbidden) && !errors.Is(err, api.ErrConflict) {
		t.Fatalf("matrix answer: %v", err)
	}
	if _, err := f.delegateAnswer("token", o.ID, "verification-matrix-approval:"+strings.Repeat("c", 64), "because", helper); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("token answer: %v", err)
	}
	if _, err := f.delegateAnswer("token-rationale", o.ID, "approve", "verification-matrix-approval:"+strings.Repeat("c", 64), helper); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("token rationale: %v", err)
	}
	// The matrix request is still the owner's.
	if ob := ownerFor(t, f.deliveryFixture, matrix.MessageSeq); ob.State == api.ObligationClosed {
		t.Fatalf("matrix obligation closed %+v", ob)
	}
}

func TestOwnerHelperExcludedFromQueueAndCloseout(t *testing.T) {
	s, task, item, lead, _, _, req := teamCloseFixture(t)
	ctx := context.Background()
	by := api.Caller{Node: "fixture", User: "owner"}
	helper := *registerHelper(t, s, task.ID, helperRequest("reg"), by).Agent
	// Never a queue handler: the free-handler lease and scope authority take handlers only.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`UPDATE agents SET status='closed' WHERE task_id=? AND role=?`, task.ID, api.AgentRoleDatabaseHandler); err != nil {
		t.Fatal(err)
	}
	chosen, err := freeQueueHandler(ctx, tx, task.ID, nil)
	if err != nil || chosen.ID != "" {
		t.Fatalf("helper leased as handler: %+v %v", chosen, err)
	}
	if err := queueScopeAuthority(ctx, tx, task.ID, api.TeamQueueEntry{State: "queued"}, api.TeamQueueRequest{HandlerAgentID: helper.ID, HandlerRunID: helper.RunID}); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("helper scope authority: %v", err)
	}
	_ = tx.Rollback()
	// Item-team close leaves the helper open and out of the member snapshot.
	if _, err := s.db.Exec(`INSERT INTO item_team_leads(task_id,item_id,agent_id,run_id,state) VALUES(?,?,?,?,'running')`, task.ID, item.ID, lead.ID, lead.RunID); err != nil {
		t.Fatal(err)
	}
	teamCloseTerminal(t, s, task, item, "dismissed")
	req.LeadRevision = 1
	result, err := s.CloseItemTeam(ctx, task.ID, req, by)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range result.Members {
		if m.AgentID == helper.ID {
			t.Fatalf("helper in team close %+v", result.Members)
		}
	}
	after, _ := s.GetAgent(ctx, helper.ID)
	if after.Status != api.AgentRunning || after.RunID != helper.RunID {
		t.Fatalf("helper after close %+v", after)
	}
}

func TestOwnerHelperNoActivityAlert(t *testing.T) {
	f := newDeliveryFixture(t)
	ctx := context.Background()
	helper := *registerHelper(t, f.s, f.task.ID, helperRequest("reg"), f.by).Agent
	before, _ := f.s.ListMessages(ctx, f.task.ID, 0, "", 500)
	for i, state := range []string{"crashed", "hung_tool", "looping"} {
		if _, err := f.s.ReportActivity(ctx, f.task.ID, helper.ID, api.ActivityReport{RequestID: "activity-" + state, RunID: helper.RunID,
			Activity: api.AgentActivity{State: state, ObservedAt: f.s.now().Add(time.Duration(i) * time.Second)}}); err != nil {
			t.Fatal(err)
		}
	}
	after, _ := f.s.ListMessages(ctx, f.task.ID, 0, "", 500)
	if len(after) != len(before) {
		t.Fatalf("alerts posted for the owner helper: %d -> %d", len(before), len(after))
	}
}
