package store

import (
	"context"
	"errors"
	"reflect"
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
		{Host: "h", Session: "owner", Runtime: "unsupported", RequestID: "bad-runtime"},
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

// helperWrites is everything a registration can write, for the conditional
// registration's "a refusal writes nothing" checks.
type helperWrites struct {
	agents, actions int
	agent           api.Agent
	receipts        []string
	events          []int64
}

func helperWritesOf(t *testing.T, s *Store, task, agent string) helperWrites {
	t.Helper()
	ctx := context.Background()
	var w helperWrites
	if err := s.db.QueryRow(`SELECT count(*) FROM agents WHERE task_id=?`, task).Scan(&w.agents); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT count(*) FROM owner_actions WHERE task_id=?`, task).Scan(&w.actions); err != nil {
		t.Fatal(err)
	}
	a, err := s.GetAgent(ctx, agent)
	if err != nil {
		t.Fatal(err)
	}
	// Online and Unread are derived on read; the stored row is what must not move.
	a.Online, a.Unread = false, 0
	w.agent = a
	list, err := s.ListOwnerHelperRegistrations(ctx, task)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range list {
		w.receipts = append(w.receipts, r.ID+" "+r.RunID+" "+r.PreviousRunID+" "+r.Host)
	}
	events, err := s.ListEvents(ctx, task, 0, 500)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		w.events = append(w.events, e.Seq)
	}
	return w
}

func (w helperWrites) same(t *testing.T, after helperWrites, what string) {
	t.Helper()
	if w.agents != after.agents || w.actions != after.actions || !reflect.DeepEqual(w.agent, after.agent) ||
		!reflect.DeepEqual(w.receipts, after.receipts) || !reflect.DeepEqual(w.events, after.events) {
		t.Fatalf("%s wrote something:\nbefore %+v\nafter  %+v", what, w, after)
	}
}

func expectedRequest(key, run string) api.RegisterOwnerHelperRequest {
	req := helperRequest(key)
	req.ExpectedRunID = run
	return req
}

func setHelperStatus(t *testing.T, s *Store, agent, status string) {
	t.Helper()
	if _, err := s.db.Exec(`UPDATE agents SET status=? WHERE id=?`, status, agent); err != nil {
		t.Fatal(err)
	}
}

// refusedExpectedRun registers with an expected run and requires a conflict
// naming the reason and no write of any kind.
func refusedExpectedRun(t *testing.T, s *Store, task, agent string, req api.RegisterOwnerHelperRequest, by api.Caller, reason string) {
	t.Helper()
	before := helperWritesOf(t, s, task, agent)
	out, err := s.RegisterOwnerHelper(context.Background(), task, req, by)
	if !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), reason) {
		t.Fatalf("expected run %s: want a conflict naming %q, got %+v %v", req.ExpectedRunID, reason, out, err)
	}
	if out.Agent != nil || out.Registration != nil {
		t.Fatalf("refusal returned a result %+v", out)
	}
	before.same(t, helperWritesOf(t, s, task, agent), "refused registration ("+reason+")")
}

func TestOwnerHelperExpectedRun(t *testing.T) {
	ctx := context.Background()
	t.Run("matching run on an eligible helper registers", func(t *testing.T) {
		for _, status := range []string{api.AgentRunning, api.AgentDone, api.AgentNeedsInput} {
			f := newDeliveryFixture(t)
			first := registerHelper(t, f.s, f.task.ID, helperRequest("reg-1"), f.by)
			setHelperStatus(t, f.s, first.Agent.ID, status)
			req := expectedRequest("restore-1", first.Agent.RunID)
			req.Session, req.Host = "owner-2", "owner-host-2"
			second := registerHelper(t, f.s, f.task.ID, req, f.by)
			if second.Replay || second.Agent.ID != first.Agent.ID || second.Agent.RunID == first.Agent.RunID || second.Agent.Status != api.AgentRunning || second.Agent.Session != "owner-2" ||
				second.Registration.Mode != api.OwnerHelperReplaced || second.Registration.PreviousRunID != first.Agent.RunID || second.Registration.RunID != second.Agent.RunID {
				t.Fatalf("%s: %+v %+v", status, second.Agent, second.Registration)
			}
			list, _ := f.s.ListOwnerHelperRegistrations(ctx, f.task.ID)
			if len(list) != 2 || len(helperEvents(t, f.s, f.task.ID, first.Agent.ID)) != 2 {
				t.Fatalf("%s: receipts %+v", status, list)
			}
			// The same request replays; the run it named is no longer current, and
			// the replay still does not register again.
			replay, err := f.s.RegisterOwnerHelper(ctx, f.task.ID, req, f.by)
			if err != nil || !replay.Replay || replay.Agent.RunID != second.Agent.RunID {
				t.Fatalf("%s: replay %+v %v", status, replay, err)
			}
			if list, _ := f.s.ListOwnerHelperRegistrations(ctx, f.task.ID); len(list) != 2 {
				t.Fatalf("%s: replay wrote a receipt %+v", status, list)
			}
		}
	})
	t.Run("a different run is refused", func(t *testing.T) {
		f := newDeliveryFixture(t)
		first := registerHelper(t, f.s, f.task.ID, helperRequest("reg-1"), f.by)
		refusedExpectedRun(t, f.s, f.task.ID, first.Agent.ID, expectedRequest("restore-1", api.NewID("run")), f.by, "run changed")
		// A refusal saved no owner action, so the same request ID is judged again.
		refusedExpectedRun(t, f.s, f.task.ID, first.Agent.ID, expectedRequest("restore-1", api.NewID("run")), f.by, "run changed")
		if a, _ := f.s.GetAgent(ctx, first.Agent.ID); a.RunID != first.Agent.RunID || a.Status != api.AgentRunning {
			t.Fatalf("helper %+v", a)
		}
	})
	t.Run("no helper at all is refused", func(t *testing.T) {
		f := newDeliveryFixture(t)
		refusedExpectedRun(t, f.s, f.task.ID, f.lead.ID, expectedRequest("restore-1", api.NewID("run")), f.by, "run changed")
		var helpers int
		if err := f.s.db.QueryRow(`SELECT count(*) FROM agents WHERE task_id=? AND role=?`, f.task.ID, api.AgentRoleOwnerHelper).Scan(&helpers); err != nil || helpers != 0 {
			t.Fatalf("helpers %d %v", helpers, err)
		}
	})
	t.Run("the right run on a retired helper is refused", func(t *testing.T) {
		f := newDeliveryFixture(t)
		first := registerHelper(t, f.s, f.task.ID, helperRequest("reg-1"), f.by)
		setHelperStatus(t, f.s, first.Agent.ID, api.AgentRetired)
		refusedExpectedRun(t, f.s, f.task.ID, first.Agent.ID, expectedRequest("restore-1", first.Agent.RunID), f.by, "retired; only tt resume re-enables it")
		if a, _ := f.s.GetAgent(ctx, first.Agent.ID); a.RunID != first.Agent.RunID || a.Status != api.AgentRetired {
			t.Fatalf("helper %+v", a)
		}
	})
	t.Run("the right run on an exited helper is refused", func(t *testing.T) {
		f := newDeliveryFixture(t)
		first := registerHelper(t, f.s, f.task.ID, helperRequest("reg-1"), f.by)
		if _, err := f.s.PostEvent(ctx, f.task.ID, api.PostEventRequest{Kind: api.EventExited, AgentID: first.Agent.ID, RunID: first.Agent.RunID}, f.by); err != nil {
			t.Fatal(err)
		}
		refusedExpectedRun(t, f.s, f.task.ID, first.Agent.ID, expectedRequest("restore-1", first.Agent.RunID), f.by, "exited")
		if a, _ := f.s.GetAgent(ctx, first.Agent.ID); a.RunID != first.Agent.RunID || a.Status != api.AgentExited {
			t.Fatalf("helper %+v", a)
		}
	})
	t.Run("the right run on a closed helper is refused", func(t *testing.T) {
		f := newDeliveryFixture(t)
		first := registerHelper(t, f.s, f.task.ID, helperRequest("reg-1"), f.by)
		if _, err := f.s.CloseAgentRun(ctx, first.Agent.ID, first.Agent.RunID, f.by); err != nil {
			t.Fatal(err)
		}
		refusedExpectedRun(t, f.s, f.task.ID, first.Agent.ID, expectedRequest("restore-1", first.Agent.RunID), f.by, "closed")
		if a, _ := f.s.GetAgent(ctx, first.Agent.ID); a.RunID != first.Agent.RunID || a.Status != api.AgentClosed {
			t.Fatalf("helper %+v", a)
		}
	})
	t.Run("a malformed expected run is invalid", func(t *testing.T) {
		f := newDeliveryFixture(t)
		first := registerHelper(t, f.s, f.task.ID, helperRequest("reg-1"), f.by)
		before := helperWritesOf(t, f.s, f.task.ID, first.Agent.ID)
		for _, bad := range []string{"run_1", "agt_0123456789abcdef", "run_0123456789ABCDEF", first.Agent.RunID + "0", " " + first.Agent.RunID} {
			if _, err := f.s.RegisterOwnerHelper(ctx, f.task.ID, expectedRequest("bad-run", bad), f.by); !errors.Is(err, api.ErrInvalid) {
				t.Fatalf("%q: %v", bad, err)
			}
		}
		before.same(t, helperWritesOf(t, f.s, f.task.ID, first.Agent.ID), "invalid expected run")
	})
	t.Run("an absent field keeps a retired helper's replacement", func(t *testing.T) {
		// Today's behaviour, which only the field changes: without it a retired
		// helper's run is replaced and it stays retired.
		f := newDeliveryFixture(t)
		first := registerHelper(t, f.s, f.task.ID, helperRequest("reg-1"), f.by)
		setHelperStatus(t, f.s, first.Agent.ID, api.AgentRetired)
		second := registerHelper(t, f.s, f.task.ID, helperRequest("reg-2"), f.by)
		if second.Agent.Status != api.AgentRetired || second.Agent.RunID == first.Agent.RunID {
			t.Fatalf("plain register on a retired helper %+v", second.Agent)
		}
	})
}

// The store serialises writers, so each interleaving is a fixed sequence: a
// read of the helper's run R stands for a restore whose guards have passed.
func TestOwnerHelperExpectedRunInterleavings(t *testing.T) {
	ctx := context.Background()
	t.Run("i two restores started together", func(t *testing.T) {
		f := newDeliveryFixture(t)
		first := registerHelper(t, f.s, f.task.ID, helperRequest("reg-1"), f.by)
		r := first.Agent.RunID
		type result struct {
			out api.OwnerActionResult
			err error
		}
		results := make([]result, 2)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := range results {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				req := expectedRequest("restore-"+string(rune('a'+i)), r)
				req.Session = "owner-" + string(rune('a'+i))
				results[i].out, results[i].err = f.s.RegisterOwnerHelper(ctx, f.task.ID, req, f.by)
			}()
		}
		close(start)
		wg.Wait()
		winner, loser := -1, -1
		for i, got := range results {
			switch {
			case got.err == nil:
				winner = i
			case errors.Is(got.err, api.ErrConflict) && strings.Contains(got.err.Error(), "run changed"):
				loser = i
			default:
				t.Fatalf("restore %d: %+v %v", i, got.out, got.err)
			}
		}
		if winner < 0 || loser < 0 {
			t.Fatalf("want one new run and one refusal: %+v", results)
		}
		r2 := results[winner].out.Agent.RunID
		if r2 == r || results[winner].out.Registration.PreviousRunID != r {
			t.Fatalf("winner %+v", results[winner].out.Registration)
		}
		// The loser, sent again with R under its own request ID and under a new
		// one, is refused again: it never registers over the winner.
		for _, key := range []string{"restore-" + string(rune('a'+loser)), "restore-again"} {
			req := expectedRequest(key, r)
			req.Session = "owner-" + string(rune('a'+loser))
			refusedExpectedRun(t, f.s, f.task.ID, first.Agent.ID, req, f.by, "run changed")
		}
		a, _ := f.s.GetAgent(ctx, first.Agent.ID)
		list, _ := f.s.ListOwnerHelperRegistrations(ctx, f.task.ID)
		if a.RunID != r2 || a.Session != "owner-"+string(rune('a'+winner)) || len(list) != 2 || list[0].RunID != r2 || len(helperEvents(t, f.s, f.task.ID, first.Agent.ID)) != 2 {
			t.Fatalf("after the race: agent %+v receipts %+v", a, list)
		}
	})
	t.Run("ii retired in the gap", func(t *testing.T) {
		f := newDeliveryFixture(t)
		first := registerHelper(t, f.s, f.task.ID, helperRequest("reg-1"), f.by)
		r := first.Agent.RunID
		status := api.AgentRetired
		if _, err := f.s.UpdateAgent(ctx, first.Agent.ID, api.UpdateAgentRequest{Status: &status}, f.by); err != nil {
			t.Fatal(err)
		}
		refusedExpectedRun(t, f.s, f.task.ID, first.Agent.ID, expectedRequest("restore-1", r), f.by, "retired")
		if a, _ := f.s.GetAgent(ctx, first.Agent.ID); a.Status != api.AgentRetired || a.RunID != r {
			t.Fatalf("helper %+v", a)
		}
	})
	t.Run("iii another host registers in the gap", func(t *testing.T) {
		f := newDeliveryFixture(t)
		first := registerHelper(t, f.s, f.task.ID, helperRequest("reg-1"), f.by)
		r := first.Agent.RunID
		other := helperRequest("other-host")
		other.Host, other.Session = "other-host", "owner-other"
		won := registerHelper(t, f.s, f.task.ID, other, f.by)
		refusedExpectedRun(t, f.s, f.task.ID, first.Agent.ID, expectedRequest("restore-1", r), f.by, "run changed")
		a, _ := f.s.GetAgent(ctx, first.Agent.ID)
		list, _ := f.s.ListOwnerHelperRegistrations(ctx, f.task.ID)
		if a.RunID != won.Agent.RunID || a.Host != "other-host" || a.Session != "owner-other" || len(list) != 2 || list[0].ID != won.Registration.ID || list[0].Host != "other-host" {
			t.Fatalf("the other registration does not stand: agent %+v receipts %+v", a, list)
		}
	})
	t.Run("iv closed in the gap", func(t *testing.T) {
		f := newDeliveryFixture(t)
		first := registerHelper(t, f.s, f.task.ID, helperRequest("reg-1"), f.by)
		r := first.Agent.RunID
		if _, err := f.s.CloseAgentRun(ctx, first.Agent.ID, r, f.by); err != nil {
			t.Fatal(err)
		}
		refusedExpectedRun(t, f.s, f.task.ID, first.Agent.ID, expectedRequest("restore-1", r), f.by, "closed")
		var helpers int
		if err := f.s.db.QueryRow(`SELECT count(*) FROM agents WHERE task_id=? AND role=?`, f.task.ID, api.AgentRoleOwnerHelper).Scan(&helpers); err != nil || helpers != 1 {
			t.Fatalf("a closed helper's restore made an agent: %d %v", helpers, err)
		}
		// A fresh helper registered deliberately afterwards is not the expected
		// run either, and the reason is still the closed one.
		fresh := registerHelper(t, f.s, f.task.ID, helperRequest("fresh"), f.by)
		refusedExpectedRun(t, f.s, f.task.ID, fresh.Agent.ID, expectedRequest("restore-2", r), f.by, "closed")
	})
}
