package server

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/store"
)

type oblFixture struct {
	c             *client
	task          api.Task
	lead, builder api.Agent
	ctx           context.Context
}

func newOblFixture(t *testing.T) oblFixture {
	c := newClient(t)
	var task api.Task
	if code := c.do("POST", "/v1/tasks", api.CreateTaskRequest{Name: "Obligations", Orchestrator: "lead"}, &task); code != 201 {
		t.Fatalf("create task: %d", code)
	}
	f := oblFixture{c: c, task: task, lead: c.agent(task, "lead"), builder: c.agent(task, "builder"), ctx: context.Background()}
	if f.builder.RunID == "" {
		t.Fatal("fixture agent has no run")
	}
	return f
}

func (f oblFixture) post(t *testing.T, req api.PostMessageRequest) api.Message {
	t.Helper()
	m, err := f.c.st.PostMessage(f.ctx, f.task.ID, req, f.c.who)
	if err != nil {
		t.Fatalf("post %+v: %v", req, err)
	}
	return m
}

func (f oblFixture) list(t *testing.T, filter store.ObligationFilter, now time.Time) []api.Obligation {
	t.Helper()
	out, err := f.c.st.ListObligations(f.ctx, f.task.ID, filter, now)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func assignFrom(lead api.Agent, to string) *api.Envelope {
	return &api.Envelope{Kind: "assign", To: to, Subject: "Reject the empty recipient in tt post",
		Body: api.EnvelopeBody{Objective: "fail fast", Owns: []string{"hub/cmd/tt/main.go"}, Acceptance: map[string]string{"a1": "exits 2"}}}
}

// b1: obligations are created with the message, once, only for obligating kinds.
func TestObligationsCreatedWithTheirMessage(t *testing.T) {
	f := newOblFixture(t)
	req := api.PostMessageRequest{AgentID: f.lead.ID, To: f.builder.ID, RequestID: "assign-1", Envelope: assignFrom(f.lead, "builder")}
	assign := f.post(t, req)
	f.post(t, req) // retry
	notice := f.post(t, api.PostMessageRequest{AgentID: f.lead.ID, To: f.builder.ID, Envelope: &api.Envelope{Kind: "notice", To: "builder", Subject: "Integration window closes today", Body: api.EnvelopeBody{Text: "rebase"}}})
	f.post(t, api.PostMessageRequest{AgentID: f.lead.ID, To: f.builder.ID, Text: "free text from an agent"})
	f.post(t, api.PostMessageRequest{AgentID: f.lead.ID, Envelope: &api.Envelope{Kind: "notice", Subject: "Board-wide notice to everyone", Body: api.EnvelopeBody{Text: "x"}}})
	human := f.post(t, api.PostMessageRequest{To: f.builder.ID, Text: "owner asks for a status update"})

	all := f.list(t, store.ObligationFilter{}, time.Now())
	got := map[int64]string{}
	for _, o := range all {
		got[o.MessageSeq] = o.Needs
		if o.AgentID != f.builder.ID || o.State != api.ObligationQueued {
			t.Errorf("obligation %+v", o)
		}
	}
	want := map[int64]string{assign.Seq: api.ObligationNeedsOutcome, notice.Seq: api.ObligationNeedsDelivery, human.Seq: api.ObligationNeedsOutcome}
	if len(got) != len(want) {
		t.Fatalf("obligations %v, want %v", got, want)
	}
	for seq, needs := range want {
		if got[seq] != needs {
			t.Errorf("message %d needs %q, want %q", seq, got[seq], needs)
		}
	}
}

// b2, b4: only the recipient's current run acknowledges; reads never do.
func TestOnlyTheRecipientRunAcknowledges(t *testing.T) {
	f := newOblFixture(t)
	m := f.post(t, api.PostMessageRequest{AgentID: f.lead.ID, To: f.builder.ID, Envelope: assignFrom(f.lead, "builder")})
	now := time.Now()
	for _, bad := range []api.ObligationActionRequest{{AgentID: f.lead.ID, RunID: f.lead.RunID}, {AgentID: f.builder.ID, RunID: "run_0000000000000000"}, {AgentID: f.builder.ID}} {
		if _, err := f.c.st.ObligationAction(f.ctx, f.task.ID, m.Seq, "ack", bad, now); err == nil {
			t.Errorf("ack by %+v accepted", bad)
		}
	}
	// Inbox reads and event traffic do not acknowledge.
	if _, err := f.c.st.ListMessages(f.ctx, f.task.ID, 0, f.builder.ID, 50); err != nil {
		t.Fatal(err)
	}
	if err := f.c.st.MarkObligationsDelivered(f.ctx, f.task.ID, f.builder.ID, f.builder.RunID, now); err != nil {
		t.Fatal(err)
	}
	if o := f.list(t, store.ObligationFilter{}, now)[0]; o.State != api.ObligationDelivered || o.AckedAt != nil {
		t.Fatalf("delivery changed ack state: %+v", o)
	}
	o, err := f.c.st.ObligationAction(f.ctx, f.task.ID, m.Seq, "ack", api.ObligationActionRequest{AgentID: f.builder.ID, RunID: f.builder.RunID}, now)
	if err != nil || o.State != api.ObligationAcknowledged || o.AckedAt == nil {
		t.Fatalf("ack: %v %+v", err, o)
	}
	if o, err := f.c.st.ObligationAction(f.ctx, f.task.ID, m.Seq, "ack", api.ObligationActionRequest{AgentID: f.builder.ID, RunID: f.builder.RunID}, now); err != nil || o.State != api.ObligationAcknowledged {
		t.Fatalf("repeat ack should be a no-op: %v %+v", err, o)
	}
	if o, err := f.c.st.ObligationAction(f.ctx, f.task.ID, m.Seq, "progress", api.ObligationActionRequest{AgentID: f.builder.ID, RunID: f.builder.RunID, Text: "halfway"}, now); err != nil || o.State != api.ObligationWorking {
		t.Fatalf("progress: %v %+v", err, o)
	}
}

// b3: typed replies from the recipient close or pause; others do not.
func TestTypedRepliesCloseObligations(t *testing.T) {
	f := newOblFixture(t)
	assign := f.post(t, api.PostMessageRequest{AgentID: f.lead.ID, To: f.builder.ID, Envelope: assignFrom(f.lead, "builder")})
	question := f.post(t, api.PostMessageRequest{AgentID: f.lead.ID, To: f.builder.ID, Envelope: &api.Envelope{Kind: "question", To: "builder", Subject: "Which status code should we use", Body: api.EnvelopeBody{Question: "422 or 400?"}}})
	request := f.post(t, api.PostMessageRequest{AgentID: f.lead.ID, To: f.builder.ID, Envelope: &api.Envelope{Kind: "request", To: "builder", Subject: "Please rebase onto the release root", Body: api.EnvelopeBody{Ask: "rebase"}}})
	review := f.post(t, api.PostMessageRequest{AgentID: f.lead.ID, To: f.builder.ID, Envelope: &api.Envelope{Kind: "review", To: "builder", Subject: "Review the redaction candidate", Body: api.EnvelopeBody{Candidate: "abc1234", Scope: "jev", Acceptance: map[string]string{"a1": "no leaks"}}}})

	// The lead replying does not close the builder's obligation.
	f.post(t, api.PostMessageRequest{AgentID: f.lead.ID, To: f.builder.ID, ReplyTo: assign.Seq, Envelope: &api.Envelope{Kind: "notice", To: "builder", Subject: "Adding context to the assignment", Body: api.EnvelopeBody{Text: "x"}}})
	reply := func(seq int64, e *api.Envelope) {
		f.post(t, api.PostMessageRequest{AgentID: f.builder.ID, To: f.lead.ID, ReplyTo: seq, Envelope: e})
	}
	reply(assign.Seq, &api.Envelope{Kind: "result", To: "lead", Subject: "Empty recipient check passes its tests", Body: api.EnvelopeBody{Outcome: "done", Status: map[string]string{"a1": "pass"}},
		Evidence: map[string]api.Evidence{"e1": {Type: "command", Value: "go test", Outcome: "ok"}}})
	reply(question.Seq, &api.Envelope{Kind: "answer", To: "lead", Subject: "Use 422 for rejected posts", Body: api.EnvelopeBody{Answer: "422"}})
	reply(request.Seq, &api.Envelope{Kind: "decline", To: "lead", Subject: "Declining the rebase for now", Body: api.EnvelopeBody{Reason: "release window closed"}})
	reply(review.Seq, &api.Envelope{Kind: "block", To: "lead", Subject: "Waiting for the frozen candidate", Body: api.EnvelopeBody{Reason: "no commit yet", Needs: "lead", ResumeWhen: "commit is frozen"}})

	byMsg := map[int64]api.Obligation{}
	for _, o := range f.list(t, store.ObligationFilter{AgentID: f.builder.ID}, time.Now()) {
		byMsg[o.MessageSeq] = o
	}
	check := func(seq int64, state, outcome string) {
		t.Helper()
		if o := byMsg[seq]; o.State != state || o.Outcome != outcome {
			t.Errorf("message %d: %s/%s, want %s/%s", seq, o.State, o.Outcome, state, outcome)
		}
	}
	check(assign.Seq, api.ObligationClosed, api.OutcomeResult)
	check(question.Seq, api.ObligationClosed, api.OutcomeAnswered)
	check(request.Seq, api.ObligationClosed, api.OutcomeDeclined)
	check(review.Seq, api.ObligationBlocked, "")
	if byMsg[request.Seq].Reason != "release window closed" || byMsg[review.Seq].Reason != "no commit yet" {
		t.Errorf("reasons %+v %+v", byMsg[request.Seq], byMsg[review.Seq])
	}
	// The blocked review resumes on ack by its recipient.
	if o, err := f.c.st.ObligationAction(f.ctx, f.task.ID, review.Seq, "ack", api.ObligationActionRequest{AgentID: f.builder.ID, RunID: f.builder.RunID}, time.Now()); err != nil || o.State != api.ObligationAcknowledged || o.Reason != "" {
		t.Fatalf("resume: %v %+v", err, o)
	}
	// A closed obligation cannot be acknowledged again.
	if _, err := f.c.st.ObligationAction(f.ctx, f.task.ID, assign.Seq, "ack", api.ObligationActionRequest{AgentID: f.builder.ID, RunID: f.builder.RunID}, time.Now()); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("ack on closed: %v", err)
	}
}

// b7: reassignment supersedes and re-issues atomically, keeping history.
func TestReassignmentSupersedesAndReissues(t *testing.T) {
	f := newOblFixture(t)
	reviewer := f.c.agent(f.task, "reviewer")
	assign := f.post(t, api.PostMessageRequest{AgentID: f.lead.ID, To: f.builder.ID, Envelope: assignFrom(f.lead, "builder")})
	old := f.list(t, store.ObligationFilter{AgentID: f.builder.ID}, time.Now())[0]
	m, err := f.c.st.ReassignObligation(f.ctx, f.task.ID, old.ID, api.ObligationReassignRequest{ToAgentID: reviewer.ID, Reason: "builder is offline"}, f.c.who)
	if err != nil {
		t.Fatal(err)
	}
	if m.To != reviewer.ID || m.Envelope == nil || m.Envelope.Refs["reassignedFrom"] == "" || m.From.Node != api.BrokerNode {
		t.Fatalf("reissued %+v", m)
	}
	superseded := f.list(t, store.ObligationFilter{AgentID: f.builder.ID}, time.Now())[0]
	moved := f.list(t, store.ObligationFilter{AgentID: reviewer.ID}, time.Now())
	if superseded.Outcome != api.OutcomeSuperseded || superseded.Reason != "builder is offline" || len(moved) != 1 || moved[0].MessageSeq != m.Seq || moved[0].Needs != api.ObligationNeedsOutcome {
		t.Fatalf("superseded %+v moved %+v", superseded, moved)
	}
	if _, err := f.c.st.ReassignObligation(f.ctx, f.task.ID, old.ID, api.ObligationReassignRequest{ToAgentID: reviewer.ID}, f.c.who); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("reassigning a closed obligation: %v", err)
	}
	_ = assign
}

// b11: overdue flags come from the recorded times.
func TestOverdueFlags(t *testing.T) {
	f := newOblFixture(t)
	f.post(t, api.PostMessageRequest{AgentID: f.lead.ID, To: f.builder.ID, Envelope: assignFrom(f.lead, "builder")})
	created := f.list(t, store.ObligationFilter{}, time.Now())[0].CreatedAt
	if o := f.list(t, store.ObligationFilter{Overdue: true}, created.Add(5*time.Minute)); len(o) != 0 {
		t.Fatalf("overdue too early: %+v", o)
	}
	if o := f.list(t, store.ObligationFilter{Overdue: true}, created.Add(11*time.Minute)); len(o) != 1 || o[0].Overdue != "ack" {
		t.Fatalf("ack overdue: %+v", o)
	}
}

// b8: the hub leases wakes; merged, stale and duplicate reports can never
// starve a recipient (the relay-stall-6852 failure class).
func TestWakeJobLeasing(t *testing.T) {
	f := newOblFixture(t)
	first := f.post(t, api.PostMessageRequest{AgentID: f.lead.ID, To: f.builder.ID, Envelope: assignFrom(f.lead, "builder")})
	f.post(t, api.PostMessageRequest{AgentID: f.lead.ID, To: f.builder.ID, Envelope: &api.Envelope{Kind: "question", To: "builder", Subject: "Which status code should we use", Body: api.EnvelopeBody{Question: "422?"}}})
	now := time.Now().UTC().Add(time.Second)
	job, err := f.c.st.LeaseWakeJob(f.ctx, f.task.ID, f.builder.ID, f.builder.RunID, now)
	if err != nil || job == nil || job.MessageSeq != first.Seq || !containsAll(job.Prompt, "#"+itoa(first.Seq), "tt ack") {
		t.Fatalf("lease: %v %+v", err, job)
	}
	// Both due wakes were merged into one; nothing else is due.
	if again, err := f.c.st.LeaseWakeJob(f.ctx, f.task.ID, f.builder.ID, f.builder.RunID, now); err != nil || again != nil {
		t.Fatalf("second lease: %v %+v", err, again)
	}
	// Another run cannot lease the builder's wakes.
	if _, err := f.c.st.LeaseWakeJob(f.ctx, f.task.ID, f.builder.ID, "run_0000000000000000", now); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("stale run lease: %v", err)
	}
	report := api.WakeJobReport{LeaseToken: job.LeaseToken, Status: "accepted"}
	if err := f.c.st.ReportWakeJob(f.ctx, f.task.ID, job.ID, report, now); err != nil {
		t.Fatal(err)
	}
	for _, o := range f.list(t, store.ObligationFilter{AgentID: f.builder.ID}, now) {
		if o.State != api.ObligationDelivered || o.AckedAt != nil {
			t.Fatalf("accepted wake must deliver, not acknowledge: %+v", o)
		}
	}
	// A duplicate or late report conflicts but blocks nothing.
	if err := f.c.st.ReportWakeJob(f.ctx, f.task.ID, job.ID, report, now); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("duplicate report: %v", err)
	}
	later := f.post(t, api.PostMessageRequest{AgentID: f.lead.ID, To: f.builder.ID, Envelope: &api.Envelope{Kind: "request", To: "builder", Subject: "Please rebase onto the release root", Body: api.EnvelopeBody{Ask: "rebase"}}})
	next, err := f.c.st.LeaseWakeJob(f.ctx, f.task.ID, f.builder.ID, f.builder.RunID, time.Now().UTC().Add(2*time.Second))
	if err != nil || next == nil || next.MessageSeq != later.Seq {
		t.Fatalf("next wake after a conflicting report: %v %+v", err, next)
	}
	// An expired lease is leased again, with the lapse recorded.
	retaken, err := f.c.st.LeaseWakeJob(f.ctx, f.task.ID, f.builder.ID, f.builder.RunID, time.Now().UTC().Add(10*time.Minute))
	if err != nil || retaken == nil || retaken.ID != next.ID || retaken.LeaseToken == next.LeaseToken {
		t.Fatalf("expired lease: %v %+v", err, retaken)
	}
	if err := f.c.st.ReportWakeJob(f.ctx, f.task.ID, next.ID, api.WakeJobReport{LeaseToken: next.LeaseToken, Status: "accepted"}, now); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("report under the lapsed token: %v", err)
	}
	// Closed obligations cancel their pending wakes.
	f.post(t, api.PostMessageRequest{AgentID: f.builder.ID, To: f.lead.ID, ReplyTo: later.Seq, Envelope: &api.Envelope{Kind: "decline", To: "lead", Subject: "Declining the rebase for now", Body: api.EnvelopeBody{Reason: "window closed"}}})
	if err := f.c.st.ReportWakeJob(f.ctx, f.task.ID, retaken.ID, api.WakeJobReport{LeaseToken: retaken.LeaseToken, Status: "failed", Detail: "codex exited"}, now); err != nil {
		t.Fatal(err)
	}
}

func containsAll(s string, parts ...string) bool {
	for _, p := range parts {
		if !strings.Contains(s, p) {
			return false
		}
	}
	return true
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
