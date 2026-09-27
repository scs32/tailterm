package broker

import (
	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/store"
	"testing"
	"time"
)

func TestOwnerTickDeadlineAndRestart(t *testing.T) {
	f := newFixture(t)
	item, err := f.st.CreateWorkItem(f.ctx, f.task.ID, api.CreateWorkItemRequest{Kind: "feature", Title: "Owner request fixture", RequestID: "owner-item"}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	m, err := f.st.PostMessage(f.ctx, f.task.ID, api.PostMessageRequest{AgentID: f.builder.ID, RunID: f.builder.RunID, RequestID: "owner-ask", Envelope: &api.Envelope{Kind: "request", To: "owner", Subject: "Approve the owner fixture request", Body: api.EnvelopeBody{Ask: "please"}}, WorkItems: []api.MessageWorkItem{{ItemTaskID: f.task.ID, ItemID: item.ID, ItemRevision: item.Revision, Relationship: "primary"}}}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	due := m.CreatedAt.Add(30 * time.Minute)
	for _, now := range []time.Time{m.CreatedAt.Add(time.Minute), due.Add(-time.Nanosecond), due, due.Add(time.Hour)} {
		b := Broker{Store: f.st}
		steps, err := b.Tick(f.ctx, now)
		if err != nil {
			t.Fatal(err)
		}
		if now.Equal(due) {
			if len(steps) != 1 || steps[0].Action != "escalate-owner" {
				t.Fatalf("due %+v", steps)
			}
		} else if len(steps) != 0 {
			t.Fatalf("unexpected agent/stall schedule %+v", steps)
		}
	}
	f.restart(t)
	steps, err := (&Broker{Store: f.st}).Tick(f.ctx, due.Add(24*time.Hour))
	if err != nil || len(steps) != 0 {
		t.Fatalf("restart %+v %v", steps, err)
	}
	all, _ := f.st.ListObligations(f.ctx, f.task.ID, store.ObligationFilter{OwnerOnly: true}, due)
	if len(all) != 1 || all[0].State == api.ObligationClosed || all[0].Escalation != 2 {
		t.Fatalf("owner %+v", all)
	}
}
