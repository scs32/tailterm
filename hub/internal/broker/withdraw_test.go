package broker

import (
	"errors"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/store"
)

func TestWithdrawStopsGateWakeAndEscalation(t *testing.T) {
	f := newFixture(t)
	created := time.Now().UTC().Add(-3 * time.Hour)
	f.st.SetClockForTest(func() time.Time { return created })
	m, err := f.st.PostMessage(f.ctx, f.task.ID, api.PostMessageRequest{AgentID: f.lead.ID, RunID: f.lead.RunID, To: f.builder.ID, Envelope: &api.Envelope{
		Kind: api.EnvelopeKindRequest, To: f.builder.Name, Subject: "Old request needs withdrawal", Body: api.EnvelopeBody{Ask: "Please do it"}}}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	all, err := f.st.BrokerOpenObligations(f.ctx)
	if err != nil || len(all) != 1 {
		t.Fatalf("snapshot: %+v %v", all, err)
	}
	o := all[0]
	f.st.SetClockForTest(func() time.Time { return time.Now().UTC() })
	post := api.PostMessageRequest{AgentID: f.builder.ID, RunID: f.builder.RunID, To: f.lead.ID, Text: "status"}
	if _, err := f.st.PostMessage(f.ctx, f.task.ID, post, f.by); err == nil {
		t.Fatal("aged unacknowledged request did not gate recipient")
	}
	if _, err := f.st.WithdrawObligation(f.ctx, f.task.ID, o.ID, api.ObligationWithdrawRequest{AgentID: f.lead.ID, RunID: f.lead.RunID, Reason: "superseded"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.PostMessage(f.ctx, f.task.ID, post, f.by); err != nil {
		t.Fatalf("withdrawn request still gated recipient: %v", err)
	}
	if err := f.st.BrokerWake(f.ctx, o, 2, time.Now().UTC()); !errors.Is(err, store.ErrBrokerStale) {
		t.Fatalf("stale broker action: %v", err)
	}
	for _, step := range f.tick(t, o.Obligation, time.Now().UTC().Add(24*time.Hour)) {
		if step == "wake" || step == "escalate" {
			t.Fatalf("withdrawn source triggered %s", step)
		}
	}
	late := api.PostMessageRequest{AgentID: f.builder.ID, RunID: f.builder.RunID, To: f.lead.ID, ReplyTo: m.Seq, Envelope: &api.Envelope{Kind: api.EnvelopeKindDecline, To: f.lead.Name, Subject: "The old request is declined", Body: api.EnvelopeBody{Reason: "late response"}}}
	if _, err := f.st.PostMessage(f.ctx, f.task.ID, late, f.by); err != nil {
		t.Fatal(err)
	}
	rows, err := f.st.ListObligations(f.ctx, f.task.ID, store.ObligationFilter{FromSeq: m.Seq, ToSeq: m.Seq}, time.Now())
	if err != nil || len(rows) != 1 || rows[0].Outcome != api.OutcomeWithdrawn {
		t.Fatalf("late reply reopened withdrawal: %+v %v", rows, err)
	}
}
