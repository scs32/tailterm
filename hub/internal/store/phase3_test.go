package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

// Broker phase 3 (docs/broker-phase-3.md).

// c2: the legacy close-out retires every open delivery and pending
// lead-disposition row once, with provenance and one board notice.
func TestRetireLegacyFollowThrough(t *testing.T) {
	f := newDeliveryFixture(t)
	ctx := context.Background()
	open := f.directive(t, "Assignment that never finished", "retire-open", api.DeliveryAssignment, 0, "")
	if _, err := f.s.db.Exec(`CREATE TABLE IF NOT EXISTS lead_disposition_obligations (id TEXT PRIMARY KEY, task_id TEXT NOT NULL, state TEXT NOT NULL, updated_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.Exec(`INSERT INTO lead_disposition_obligations VALUES ('ldo_1',?,'pending','')`, f.task.ID); err != nil {
		t.Fatal(err)
	}
	// Pretend this database predates phase 3.
	if _, err := f.s.db.Exec(`DELETE FROM broker_migrations`); err != nil {
		t.Fatal(err)
	}
	f.s.Close()
	for round := 0; round < 2; round++ {
		s, err := Open(f.path)
		if err != nil {
			t.Fatal(err)
		}
		var phase, ldo string
		var current int
		if err := s.db.QueryRow(`SELECT phase,current FROM required_deliveries WHERE id=?`, open.Delivery.ID).Scan(&phase, &current); err != nil {
			t.Fatal(err)
		}
		_ = s.db.QueryRow(`SELECT state FROM lead_disposition_obligations WHERE id='ldo_1'`).Scan(&ldo)
		var events, notices int
		_ = s.db.QueryRow(`SELECT count(*) FROM delivery_events WHERE delivery_id=? AND kind=?`, open.Delivery.ID, RetiredPhase3).Scan(&events)
		msgs, _ := s.ListMessages(ctx, f.task.ID, 0, "", 0)
		for _, m := range msgs {
			if m.Envelope != nil && m.Envelope.Refs["migration"] == RetiredPhase3 {
				notices++
				if !strings.Contains(m.Text, f.item.ID) {
					t.Errorf("the notice does not name the referenced item: %q", m.Text)
				}
			}
		}
		if phase != api.DeliverySuperseded || current != 0 || events != 1 || ldo != RetiredPhase3 || notices != 1 {
			t.Fatalf("round %d: phase=%s current=%d events=%d ldo=%s notices=%d", round, phase, current, events, ldo, notices)
		}
		s.Close()
	}
}

type phase3Fixture struct {
	s                      *Store
	ctx                    context.Context
	by                     api.Caller
	task                   api.Task
	lead, handler, builder api.Agent
	lead2                  api.Agent
}

func newPhase3Fixture(t *testing.T) phase3Fixture {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "hub.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	f := phase3Fixture{s: s, ctx: context.Background(), by: api.Caller{Node: "workspace", User: "owner"}}
	if f.task, err = s.CreateTask(f.ctx, api.CreateTaskRequest{Name: "Phase three", Orchestrator: "lead-1"}, f.by); err != nil {
		t.Fatal(err)
	}
	add := func(name, role string) api.Agent {
		a, err := s.AddAgent(f.ctx, f.task.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: name, Host: "h", Session: name, Runtime: "codex", Role: role}, f.by)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	f.lead, f.handler, f.builder, f.lead2 = add("lead-1", ""), add("db-handler", api.AgentRoleDatabaseHandler), add("builder", ""), add("lead-2", "")
	return f
}

func (f phase3Fixture) post(t *testing.T, req api.PostMessageRequest) (api.Message, error) {
	t.Helper()
	return f.s.PostMessage(f.ctx, f.task.ID, req, f.by)
}

func (f phase3Fixture) obligationFor(t *testing.T, seq int64) api.Obligation {
	t.Helper()
	all, err := f.s.ListObligations(f.ctx, f.task.ID, ObligationFilter{}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range all {
		if o.MessageSeq == seq {
			return o
		}
	}
	t.Fatalf("no obligation for #%d", seq)
	return api.Obligation{}
}

func request(from api.Agent, to, subject string) api.PostMessageRequest {
	return api.PostMessageRequest{AgentID: from.ID, RunID: from.RunID, Envelope: &api.Envelope{Kind: api.EnvelopeKindRequest, To: to, Subject: subject, Body: api.EnvelopeBody{Ask: "please"}}}
}

// c6, c7: role recipients resolve at post time and follow a lead change.
func TestRoleRecipients(t *testing.T) {
	f := newPhase3Fixture(t)
	toLead, err := f.post(t, request(f.builder, "role:lead", "Review the fixture plan please"))
	if err != nil || toLead.To != f.lead.ID || toLead.Envelope.To != "role:lead" {
		t.Fatalf("role:lead = %+v %v", toLead, err)
	}
	var viaRole string
	_ = f.s.db.QueryRow(`SELECT via_role FROM obligations WHERE message_seq=?`, toLead.Seq).Scan(&viaRole)
	if viaRole != api.RoleLead {
		t.Fatalf("via_role = %q", viaRole)
	}
	toHandler, err := f.post(t, request(f.builder, "role:database_handler", "Record the plan revision please"))
	if err != nil || toHandler.To != f.handler.ID {
		t.Fatalf("role:database_handler = %+v %v", toHandler, err)
	}
	named := request(f.builder, "lead-1", "Addressed to the lead by name")
	named.To = f.lead.ID
	byName, err := f.post(t, named)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.post(t, request(f.builder, "role:reviewer", "Nobody holds this role here")); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "role") {
		t.Fatalf("unknown role: %v", err)
	}
	mismatch := request(f.builder, "role:lead", "Routed to the wrong agent")
	mismatch.To = f.builder.ID
	if _, err := f.post(t, mismatch); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("to that disagrees with the role: %v", err)
	}
	// The lead changes: role work follows; work addressed by name stays.
	name := "lead-2"
	if _, err := f.s.UpdateTask(f.ctx, f.task.ID, api.UpdateTaskRequest{Orchestrator: &name}, f.by); err != nil {
		t.Fatal(err)
	}
	old := f.obligationFor(t, toLead.Seq)
	if old.State != api.ObligationClosed || old.Outcome != api.OutcomeSuperseded {
		t.Fatalf("the role obligation did not move: %+v", old)
	}
	all, _ := f.s.ListObligations(f.ctx, f.task.ID, ObligationFilter{AgentID: f.lead2.ID, OpenOnly: true}, time.Now())
	if len(all) != 1 {
		t.Fatalf("the new lead holds %d open obligations, want the reissued role one", len(all))
	}
	_ = f.s.db.QueryRow(`SELECT via_role FROM obligations WHERE id=?`, all[0].ID).Scan(&viaRole)
	if viaRole != api.RoleLead {
		t.Fatalf("the reissued obligation lost its role: %q", viaRole)
	}
	if kept := f.obligationFor(t, byName.Seq); kept.State == api.ObligationClosed || kept.AgentID != f.lead.ID {
		t.Fatalf("an obligation addressed by name moved: %+v", kept)
	}
	noLead := ""
	if _, err := f.s.UpdateTask(f.ctx, f.task.ID, api.UpdateTaskRequest{Orchestrator: &noLead}, f.by); err != nil {
		t.Fatal(err)
	}
	if _, err := f.post(t, request(f.builder, "role:lead", "Nobody leads this project now")); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("role:lead with no lead: %v", err)
	}
}

// c12: a BLOCK obliges the lead, not a worker told to wait.
func TestBlockObligesOnlyTheLead(t *testing.T) {
	f := newPhase3Fixture(t)
	block := func(from, to api.Agent) api.PostMessageRequest {
		return api.PostMessageRequest{AgentID: from.ID, RunID: from.RunID, To: to.ID, Envelope: &api.Envelope{Kind: api.EnvelopeKindBlock, To: to.Name, Subject: "Waiting for the reviewed plan",
			Body: api.EnvelopeBody{Reason: "plan not reviewed", Needs: "the plan", ResumeWhen: "the plan arrives"}}}
	}
	toWorker, err := f.post(t, block(f.lead, f.builder))
	if err != nil {
		t.Fatal(err)
	}
	if o := f.obligationFor(t, toWorker.Seq); o.Needs != api.ObligationNeedsDelivery {
		t.Fatalf("a BLOCK to a worker obliges it: %+v", o)
	}
	toLead, err := f.post(t, block(f.builder, f.lead))
	if err != nil {
		t.Fatal(err)
	}
	if o := f.obligationFor(t, toLead.Seq); o.Needs != api.ObligationNeedsOutcome {
		t.Fatalf("a BLOCK to the lead does not oblige it: %+v", o)
	}
	// A BLOCK in reply still pauses the obligation it answers.
	assign, err := f.post(t, api.PostMessageRequest{AgentID: f.lead.ID, RunID: f.lead.RunID, To: f.builder.ID, Envelope: &api.Envelope{Kind: api.EnvelopeKindAssign, To: "builder", Subject: "Fix the stale smoke test",
		Body: api.EnvelopeBody{Objective: "fix it", Owns: []string{"tests/x.mjs"}, Acceptance: map[string]string{"a1": "passes"}}}})
	if err != nil {
		t.Fatal(err)
	}
	reply := block(f.builder, f.lead)
	reply.ReplyTo = assign.Seq
	if _, err := f.post(t, reply); err != nil {
		t.Fatal(err)
	}
	if o := f.obligationFor(t, assign.Seq); o.State != api.ObligationBlocked {
		t.Fatalf("a BLOCK reply did not pause the assignment: %+v", o)
	}
}

// c8, c9: the owner's extend, answer, cancel and resume.
func TestOwnerActions(t *testing.T) {
	f := newPhase3Fixture(t)
	now := time.Now().UTC()
	f.s.now = func() time.Time { return now }
	ask := request(f.lead, "builder", "Run the smoke suite please")
	ask.To = f.builder.ID
	req, err := f.post(t, ask)
	if err != nil {
		t.Fatal(err)
	}
	o := f.obligationFor(t, req.Seq)
	if _, err := f.s.db.Exec(`UPDATE obligations SET escalation=2,nudges=1 WHERE id=?`, o.ID); err != nil {
		t.Fatal(err)
	}
	ext := api.ObligationExtendRequest{For: "2h", Reason: "waiting on CI", RequestID: "ext-1"}
	out, err := f.s.ExtendObligation(f.ctx, f.task.ID, o.ID, ext, f.by)
	if err != nil || !out.Obligation.DueAt.Equal(now.Add(2*time.Hour)) || !out.Obligation.AckDueAt.Equal(now.Add(2*time.Hour)) || out.Obligation.Escalation != 0 || out.Obligation.Nudges != 0 {
		t.Fatalf("extend = %+v %v", out.Obligation, err)
	}
	again, err := f.s.ExtendObligation(f.ctx, f.task.ID, o.ID, ext, f.by)
	if err != nil || !again.Replay {
		t.Fatalf("a retried extend = %+v %v", again, err)
	}
	ext.For = "3h"
	if _, err := f.s.ExtendObligation(f.ctx, f.task.ID, o.ID, ext, f.by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("a reused request ID with other data: %v", err)
	}
	for _, bad := range []string{"30s", "200h", "soon"} {
		if _, err := f.s.ExtendObligation(f.ctx, f.task.ID, o.ID, api.ObligationExtendRequest{For: bad, RequestID: "ext-bad"}, f.by); !errors.Is(err, api.ErrInvalid) {
			t.Errorf("extend for %q: %v", bad, err)
		}
	}
	// Only a question or a block can be answered.
	if _, err := f.s.AnswerObligation(f.ctx, f.task.ID, o.ID, api.ObligationAnswerRequest{Text: "yes", RequestID: "ans-0"}, f.by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("answering a request: %v", err)
	}
	question, err := f.post(t, api.PostMessageRequest{AgentID: f.builder.ID, RunID: f.builder.RunID, To: f.lead.ID, Envelope: &api.Envelope{Kind: api.EnvelopeKindQuestion, To: "lead-1", Subject: "Which fixture should the test use",
		Body: api.EnvelopeBody{Question: "Static or live?"}}})
	if err != nil {
		t.Fatal(err)
	}
	q := f.obligationFor(t, question.Seq)
	answered, err := f.s.AnswerObligation(f.ctx, f.task.ID, q.ID, api.ObligationAnswerRequest{Text: "Use the static fixture.", RequestID: "ans-1"}, f.by)
	if err != nil || answered.Obligation.Outcome != api.OutcomeAnswered || answered.Message == nil || answered.Message.To != f.builder.ID || answered.Message.ReplyTo != question.Seq || answered.Message.Envelope.Kind != api.EnvelopeKindAnswer {
		t.Fatalf("answer = %+v %v", answered, err)
	}
	if n, _ := f.s.ListObligations(f.ctx, f.task.ID, ObligationFilter{AgentID: f.builder.ID, OpenOnly: true}, now); len(n) != 1 {
		t.Fatalf("the owner's answer obliged the asker: %d open", len(n))
	}
	// Cancel closes and tells the recipient.
	cancelled, err := f.s.CancelObligation(f.ctx, f.task.ID, o.ID, api.ObligationCancelRequest{Reason: "no longer needed", RequestID: "can-1"}, f.by)
	if err != nil || cancelled.Obligation.Outcome != api.OutcomeCancelled || !strings.Contains(cancelled.Obligation.Reason, "no longer needed") {
		t.Fatalf("cancel = %+v %v", cancelled, err)
	}
	msgs, _ := f.s.ListMessages(f.ctx, f.task.ID, 0, f.builder.ID, 0)
	told := false
	for _, m := range msgs {
		told = told || (m.To == f.builder.ID && strings.Contains(m.Text, "cancelled"))
	}
	if !told {
		t.Fatal("the recipient was not told about the cancellation")
	}
	if _, err := f.s.ExtendObligation(f.ctx, f.task.ID, o.ID, api.ObligationExtendRequest{For: "1h", RequestID: "ext-closed"}, f.by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("extending a closed obligation: %v", err)
	}
	// Resume only a retired agent.
	if _, err := f.s.ResumeAgent(f.ctx, f.task.ID, f.builder.ID, api.AgentResumeRequest{RequestID: "res-0"}, f.by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("resuming an active agent: %v", err)
	}
	retired := api.AgentRetired
	if _, err := f.s.UpdateAgent(f.ctx, f.builder.ID, api.UpdateAgentRequest{Status: &retired}, f.by); err != nil {
		t.Fatal(err)
	}
	resumed, err := f.s.ResumeAgent(f.ctx, f.task.ID, f.builder.ID, api.AgentResumeRequest{RequestID: "res-1"}, f.by)
	if err != nil || resumed.Agent.Status != api.AgentDone {
		t.Fatalf("resume = %+v %v", resumed, err)
	}
}
