package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

// Broker phase 3.1 acknowledgement discipline (docs/broker-phase-3.1.md).

func (f phase3Fixture) assignTo(t *testing.T, to api.Agent) api.Message {
	t.Helper()
	m, err := f.post(t, api.PostMessageRequest{AgentID: f.lead.ID, RunID: f.lead.RunID, To: to.ID, Envelope: &api.Envelope{Kind: api.EnvelopeKindAssign, To: to.Name, Subject: "Fix the stale smoke test",
		Body: api.EnvelopeBody{Objective: "fix it", Owns: []string{"tests/x.mjs"}, Acceptance: map[string]string{"a1": "passes"}}}})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func notice(from api.Agent, text string) api.PostMessageRequest {
	return api.PostMessageRequest{AgentID: from.ID, RunID: from.RunID, Envelope: &api.Envelope{Kind: api.EnvelopeKindNotice, Subject: "Status of the smoke test work", Body: api.EnvelopeBody{Text: text}}}
}

// k1, k2, k5: past the grace, other writes are refused with the exact fix;
// inside it, or after tt ack, they succeed.
func TestAckGateRefusesUntilAcknowledged(t *testing.T) {
	f := newPhase3Fixture(t)
	start := time.Now().UTC()
	f.s.now = func() time.Time { return start }
	assign := f.assignTo(t, f.builder)
	if _, err := f.post(t, notice(f.builder, "working on it")); err != nil {
		t.Fatalf("inside the grace a post was refused: %v", err)
	}
	f.s.now = func() time.Time { return start.Add(api.ObligationAckGrace + time.Second) }
	_, err := f.post(t, notice(f.builder, "still working"))
	var unacked *api.UnacknowledgedError
	if !errors.As(err, &unacked) || unacked.Items[0].Seq != assign.Seq || unacked.Items[0].From != "lead-1" || !strings.Contains(err.Error(), "tt ack") {
		t.Fatalf("past the grace: %v", err)
	}
	if _, err := f.s.CreateWorkItem(f.ctx, f.task.ID, api.CreateWorkItemRequest{Kind: "bug", Title: "A new bug from the builder", AgentID: f.builder.ID, RequestID: "wi-gated"}, f.by); !errors.As(err, &unacked) {
		t.Fatalf("a work-item create was not gated: %v", err)
	}
	if _, err := f.s.ObligationAction(f.ctx, f.task.ID, assign.Seq, "ack", api.ObligationActionRequest{AgentID: f.builder.ID, RunID: f.builder.RunID}, f.s.now()); err != nil {
		t.Fatalf("tt ack was refused: %v", err)
	}
	if _, err := f.post(t, notice(f.builder, "acknowledged, now reporting")); err != nil {
		t.Fatalf("after tt ack the post was refused: %v", err)
	}
}

// k3, k4: replies are the way out and acknowledge; a stale run's reply does
// neither; people, the hub and delivery-only notices are never gated.
func TestAckGateExemptionsAndImplicitAck(t *testing.T) {
	f := newPhase3Fixture(t)
	start := time.Now().UTC()
	f.s.now = func() time.Time { return start }
	assign := f.assignTo(t, f.builder)
	f.s.now = func() time.Time { return start.Add(10 * time.Minute) }
	// People are never gated.
	if _, err := f.post(t, api.PostMessageRequest{To: f.builder.ID, Text: "owner: how is it going"}); err != nil {
		t.Fatalf("an owner post was gated: %v", err)
	}
	stale := api.PostMessageRequest{AgentID: f.builder.ID, RunID: "run_0000000000000000", To: f.lead.ID, ReplyTo: assign.Seq, Envelope: &api.Envelope{Kind: api.EnvelopeKindQuestion, To: "lead-1", Subject: "Which fixture should I use here", Body: api.EnvelopeBody{Question: "Static or live?"}}}
	var unacked *api.UnacknowledgedError
	if _, err := f.post(t, stale); !errors.As(err, &unacked) {
		t.Fatalf("a stale run's reply bypassed the gate: %v", err)
	}
	reply := stale
	reply.RunID = f.builder.RunID
	if _, err := f.post(t, reply); err != nil {
		t.Fatalf("a reply to the gating work was refused: %v", err)
	}
	if o := f.obligationFor(t, assign.Seq); o.State != api.ObligationAcknowledged || o.AckedAt == nil {
		t.Fatalf("the reply did not acknowledge: %+v", o)
	}
	if _, err := f.post(t, notice(f.builder, "now free to report")); err != nil {
		t.Fatalf("after the reply the builder is still gated: %v", err)
	}
}

// k6: the database handler is gated only by work addressed to it.
func TestAckGateIsPerRecipient(t *testing.T) {
	f := newPhase3Fixture(t)
	start := time.Now().UTC()
	f.s.now = func() time.Time { return start }
	f.assignTo(t, f.builder) // the builder's unacknowledged work
	f.s.now = func() time.Time { return start.Add(10 * time.Minute) }
	if _, err := f.s.CreateWorkItem(f.ctx, f.task.ID, api.CreateWorkItemRequest{Kind: "bug", Title: "Recorded by the handler", AgentID: f.handler.ID, RequestID: "wi-handler"}, f.by); err != nil {
		t.Fatalf("the handler was gated by the builder's work: %v", err)
	}
	f.s.now = func() time.Time { return start.Add(10 * time.Minute) }
	f.assignTo(t, f.handler)
	f.s.now = func() time.Time { return start.Add(20 * time.Minute) }
	var unacked *api.UnacknowledgedError
	if _, err := f.s.CreateWorkItem(f.ctx, f.task.ID, api.CreateWorkItemRequest{Kind: "bug", Title: "Recorded after its own assignment", AgentID: f.handler.ID, RequestID: "wi-handler-2"}, f.by); !errors.As(err, &unacked) {
		t.Fatalf("the handler's own unacknowledged work did not gate it: %v", err)
	}
}

// Round-one R1-C1: the gate sees every unacknowledged obligation, so the
// refusal names all of them and a reply to any one is let through.
func TestAckGateSeesEveryObligation(t *testing.T) {
	f := newPhase3Fixture(t)
	start := time.Now().UTC()
	f.s.now = func() time.Time { return start }
	var seqs []int64
	for i := 0; i < 11; i++ {
		seqs = append(seqs, f.assignTo(t, f.builder).Seq)
	}
	f.s.now = func() time.Time { return start.Add(api.ObligationAckGrace + time.Second) }
	var unacked *api.UnacknowledgedError
	if _, err := f.post(t, notice(f.builder, "reporting early")); !errors.As(err, &unacked) || len(unacked.Items) != len(seqs) {
		t.Fatalf("the refusal did not name all %d obligations: %v", len(seqs), err)
	}
	last := seqs[len(seqs)-1]
	reply := api.PostMessageRequest{AgentID: f.builder.ID, RunID: f.builder.RunID, To: f.lead.ID, ReplyTo: last, Envelope: &api.Envelope{Kind: api.EnvelopeKindQuestion, To: "lead-1", Subject: "Which fixture should I use here", Body: api.EnvelopeBody{Question: "Static or live?"}}}
	if _, err := f.post(t, reply); err != nil {
		t.Fatalf("a reply to the eleventh obligation was refused: %v", err)
	}
	if o := f.obligationFor(t, last); o.State != api.ObligationAcknowledged {
		t.Fatalf("the reply did not acknowledge the eleventh: %+v", o)
	}
}

// Round-one B2 and the k2 delivery-only clause: a decision request is gated
// like any board post (a stored one still replays), and delivery-only notices
// never gate.
func TestAckGateDecisionsAndDeliveryOnly(t *testing.T) {
	f := newPhase3Fixture(t)
	start := time.Now().UTC()
	f.s.now = func() time.Time { return start }
	early, err := f.s.CreateDecision(f.ctx, f.task.ID, decisionRequest(f.builder, "ask-early"), f.by)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.post(t, api.PostMessageRequest{AgentID: f.lead.ID, RunID: f.lead.RunID, To: f.builder.ID, Envelope: &api.Envelope{Kind: api.EnvelopeKindNotice, To: f.builder.Name, Subject: "The fixture moved to tests/y.mjs", Body: api.EnvelopeBody{Text: "fyi"}}}); err != nil {
		t.Fatal(err)
	}
	f.s.now = func() time.Time { return start.Add(10 * time.Minute) }
	if _, err := f.post(t, notice(f.builder, "a delivery-only notice does not gate")); err != nil {
		t.Fatalf("a delivery-only notice gated the builder: %v", err)
	}
	f.s.now = func() time.Time { return start.Add(10 * time.Minute) }
	f.assignTo(t, f.builder)
	f.s.now = func() time.Time { return start.Add(20 * time.Minute) }
	var unacked *api.UnacknowledgedError
	if _, err := f.s.CreateDecision(f.ctx, f.task.ID, decisionRequest(f.builder, "ask-gated"), f.by); !errors.As(err, &unacked) {
		t.Fatalf("a decision request bypassed the gate: %v", err)
	}
	if replay, err := f.s.CreateDecision(f.ctx, f.task.ID, decisionRequest(f.builder, "ask-early"), f.by); err != nil || replay.Seq != early.Seq {
		t.Fatalf("a stored decision request did not replay: %+v %v", replay, err)
	}
}

// Round-two R2-C3-Q, the store half of the latency contract: work
// acknowledged straight from the queue, by tt ack or by a reply, gets the
// same delivered_at and acked_at, which the summary reads as never
// delivered and counts from creation.
func TestQueueFirstAckStampsDeliveryAtAck(t *testing.T) {
	f := newPhase3Fixture(t)
	start := time.Now().UTC().Truncate(time.Second)
	f.s.now = func() time.Time { return start }
	byAck, byReply := f.assignTo(t, f.builder), f.assignTo(t, f.builder)
	for _, seq := range []int64{byAck.Seq, byReply.Seq} {
		if o := f.obligationFor(t, seq); o.State != api.ObligationQueued {
			t.Fatalf("#%d is not queued: %+v", seq, o)
		}
	}
	f.s.now = func() time.Time { return start.Add(9 * time.Minute) }
	if _, err := f.s.ObligationAction(f.ctx, f.task.ID, byAck.Seq, "ack", api.ObligationActionRequest{AgentID: f.builder.ID, RunID: f.builder.RunID}, f.s.now()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.post(t, api.PostMessageRequest{AgentID: f.builder.ID, RunID: f.builder.RunID, To: f.lead.ID, ReplyTo: byReply.Seq, Envelope: &api.Envelope{Kind: api.EnvelopeKindQuestion, To: "lead-1", Subject: "Which fixture should I use here", Body: api.EnvelopeBody{Question: "Static or live?"}}}); err != nil {
		t.Fatal(err)
	}
	for _, seq := range []int64{byAck.Seq, byReply.Seq} {
		o := f.obligationFor(t, seq)
		if o.AckedAt == nil || o.DeliveredAt == nil || !o.DeliveredAt.Equal(*o.AckedAt) || o.AckedAt.Sub(o.CreatedAt) != 9*time.Minute {
			t.Fatalf("#%d: created %v delivered %v acked %v", seq, o.CreatedAt, o.DeliveredAt, o.AckedAt)
		}
	}
}

// reportWakeFixture fires one hourly-update and one queue-check wake to the
// owner helper, then gives the helper an unacknowledged REQUEST past the
// grace (wi_475a3df9b345e13e g1/g2). Isolated store and fake clock only.
type reportWakeFixture struct {
	*wakeFixture
	lead, other        api.Agent
	hourly, queueCheck int64 // notice seqs
	request            api.Message
}

func newReportWakeFixture(t *testing.T) reportWakeFixture {
	t.Helper()
	f := wakeStore(t, true)
	ctx := context.Background()
	add := func(name string) api.Agent {
		a, err := f.s.AddAgent(ctx, f.task.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: name, Host: "h", Session: name, Runtime: "codex"}, wakeBy)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	r := reportWakeFixture{wakeFixture: f, lead: add("lead"), other: add("other")}
	hourly := f.create("report-hourly", "hourly-update", "0 * * * *")
	queue := f.create("report-queue", "queue-check", "0 * * * *")
	f.sweep(pst(2, 7, 0, 30))
	for _, c := range []struct {
		wake string
		seq  *int64
	}{{hourly.ID, &r.hourly}, {queue.ID, &r.queueCheck}} {
		got := f.firings(c.wake)
		if len(got) != 1 || got[0].NoticeSeq == 0 || got[0].RecipientID != f.helper.ID {
			t.Fatalf("wake %s firings %+v", c.wake, got)
		}
		*c.seq = got[0].NoticeSeq
	}
	ask := func(to api.Agent, key string) api.Message {
		m, err := f.s.PostMessage(ctx, f.task.ID, api.PostMessageRequest{AgentID: r.lead.ID, RunID: r.lead.RunID, To: to.ID, RequestID: key,
			Envelope: &api.Envelope{Kind: api.EnvelopeKindRequest, To: to.Name, Subject: "Send the overnight count", Body: api.EnvelopeBody{Ask: "count it"}}}, wakeBy)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	r.request = ask(f.helper, "ask-helper")
	ask(r.other, "ask-other")
	f.now = pst(2, 7, 0, 30).Add(api.ObligationAckGrace + time.Minute)
	return r
}

func (r reportWakeFixture) report(agent api.Agent, run string, replyTo int64, key string) (api.Message, error) {
	return r.s.PostMessage(context.Background(), r.task.ID, api.PostMessageRequest{AgentID: agent.ID, RunID: run, ReplyTo: replyTo, RequestID: key,
		Text: "**Owner report: Hourly** 2026-11-02 06:00–07:00 PST"}, wakeBy)
}

// a7 (g2): with an unacknowledged REQUEST open past the grace, the helper's
// scheduled report still posts as the reply to its hourly wake notice, and
// that reply closes the notice's delivery obligation. The REQUEST stays
// unacknowledged and keeps gating everything else.
func TestAckGateLetsTheReportReplyToItsWake(t *testing.T) {
	r := newReportWakeFixture(t)
	var unacked *api.UnacknowledgedError
	if _, err := r.report(r.helper, r.helper.RunID, 0, "plain-first"); !errors.As(err, &unacked) || unacked.Items[0].Seq != r.request.Seq {
		t.Fatalf("the helper was not gated before the report: %v", err)
	}
	m, err := r.report(r.helper, r.helper.RunID, r.hourly, "owner-report-hourly-x")
	if err != nil {
		t.Fatalf("the scheduled report was lost to the ack gate: %v", err)
	}
	if m.ReplyTo != r.hourly {
		t.Fatalf("the report is not the reply to its wake: %+v", m)
	}
	all, err := r.s.ListObligations(context.Background(), r.task.ID, ObligationFilter{}, r.now)
	if err != nil {
		t.Fatal(err)
	}
	var notice, request *api.Obligation
	for i := range all {
		switch all[i].MessageSeq {
		case r.hourly:
			notice = &all[i]
		case r.request.Seq:
			request = &all[i]
		}
	}
	if notice == nil || notice.State != api.ObligationClosed || notice.ClosedAt == nil {
		t.Fatalf("the report did not close its wake notice: %+v", notice)
	}
	if request == nil || (request.State != api.ObligationQueued && request.State != api.ObligationDelivered) {
		t.Fatalf("the report acknowledged the unrelated request: %+v", request)
	}
	if _, err := r.report(r.helper, r.helper.RunID, 0, "plain-after"); !errors.As(err, &unacked) {
		t.Fatalf("after the report the helper is no longer gated: %v", err)
	}
}

// a6 (g1): the rule is narrow. A plain post, a reply to a queue-check wake,
// a directed or enveloped reply to the report wake, another agent's reply to
// the helper's report wake, a stale or missing run, a work-item write, a
// reply to an older firing's notice and a second reply to the same report
// wake all stay refused while the REQUEST is unacknowledged.
func TestAckGateWakeReplyStaysNarrow(t *testing.T) {
	r := newReportWakeFixture(t)
	var unacked *api.UnacknowledgedError
	refused := func(what string, err error) {
		t.Helper()
		if !errors.As(err, &unacked) {
			t.Fatalf("%s bypassed the gate: %v", what, err)
		}
	}
	_, err := r.report(r.helper, r.helper.RunID, 0, "n-plain")
	refused("a plain post", err)
	_, err = r.report(r.helper, r.helper.RunID, r.queueCheck, "n-queue")
	refused("a reply to a queue-check wake", err)
	// Only the report's own shape passes (review b1): a directed reply or an
	// enveloped one to the report wake stays gated.
	ctx := context.Background()
	_, err = r.s.PostMessage(ctx, r.task.ID, api.PostMessageRequest{AgentID: r.helper.ID, RunID: r.helper.RunID, ReplyTo: r.hourly, To: r.other.ID, RequestID: "n-directed",
		Text: "**Owner report: Hourly** directed"}, wakeBy)
	refused("a directed reply to the report wake", err)
	_, err = r.s.PostMessage(ctx, r.task.ID, api.PostMessageRequest{AgentID: r.helper.ID, RunID: r.helper.RunID, ReplyTo: r.hourly, RequestID: "n-envelope",
		Envelope: &api.Envelope{Kind: api.EnvelopeKindNotice, Subject: "Status of something unrelated", Body: api.EnvelopeBody{Text: "unrelated"}}}, wakeBy)
	refused("an enveloped reply to the report wake", err)
	_, err = r.s.PostMessage(ctx, r.task.ID, api.PostMessageRequest{AgentID: r.helper.ID, RunID: r.helper.RunID, ReplyTo: r.hourly, To: r.other.ID, RequestID: "n-request",
		Envelope: &api.Envelope{Kind: api.EnvelopeKindRequest, To: r.other.Name, Subject: "Do something unrelated", Body: api.EnvelopeBody{Ask: "do it"}}}, wakeBy)
	refused("a directed request replying to the report wake", err)
	_, err = r.report(r.other, r.other.RunID, r.hourly, "n-other")
	refused("another agent's reply to the helper's wake", err)
	_, err = r.report(r.helper, "run_0000000000000000", r.hourly, "n-stale")
	refused("a stale run's reply", err)
	_, err = r.report(r.helper, "", r.hourly, "n-norun")
	refused("a reply without a run", err)
	_, err = r.s.CreateWorkItem(context.Background(), r.task.ID, api.CreateWorkItemRequest{Kind: "bug", Title: "Filed by the gated helper", AgentID: r.helper.ID, RequestID: "n-wi"}, wakeBy)
	refused("a work-item write", err)
	// The next slot fires: the 07:00 notice, never replied to, is now an
	// older firing's and no longer opens the gate (review b1).
	r.sweep(pst(2, 8, 0, 30))
	var latest int64
	if err := r.s.db.QueryRow(`SELECT f.notice_seq FROM owner_helper_wake_firings f JOIN owner_helper_wakes w ON w.id=f.wake_id WHERE w.request_id='report-hourly' ORDER BY f.due_at DESC LIMIT 1`).Scan(&latest); err != nil || latest == 0 || latest == r.hourly {
		t.Fatalf("the 08:00 slot did not fire a new notice: %d %v", latest, err)
	}
	_, err = r.report(r.helper, r.helper.RunID, r.hourly, "n-older")
	refused("a reply to an older firing's notice", err)
	if _, err = r.report(r.helper, r.helper.RunID, latest, "n-first"); err != nil {
		t.Fatalf("the first report reply to the latest wake was refused: %v", err)
	}
	_, err = r.report(r.helper, r.helper.RunID, latest, "n-second")
	refused("a second reply to the same report wake", err)
}
