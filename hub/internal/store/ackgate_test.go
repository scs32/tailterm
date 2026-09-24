package store

import (
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
