package store

import (
	"context"
	"testing"

	"github.com/scs32/tailterm/hub/internal/api"
)

func TestParallelBrokerEscalatesToExactItemLead(t *testing.T) {
	s, task, items, orders := queueFixture(t)
	ctx := context.Background()
	by := api.Caller{Node: "fixture", User: "owner"}
	var leads []api.Agent
	for i := 0; i < 2; i++ {
		lead, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: []string{"lead-a", "lead-b"}[i], AgentID: api.NewID("agt"), Host: "mini", Session: "lead"}, by)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`INSERT INTO item_team_leads(task_id,item_id,agent_id,run_id,revision,state) VALUES(?,?,?,?,1,'running')`, task.ID, items[i].ID, lead.ID, lead.RunID); err != nil {
			t.Fatal(err)
		}
		leads = append(leads, lead)
	}
	worker, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "worker-b", AgentID: api.NewID("agt"), Host: "mini", Session: "worker"}, by)
	if err != nil {
		t.Fatal(err)
	}
	message, err := s.PostMessage(ctx, task.ID, api.PostMessageRequest{RequestID: "parallel-overdue-b", To: worker.ID, WorkItems: []api.MessageWorkItem{{ItemTaskID: task.ID, ItemID: items[1].ID, ItemRevision: items[1].Revision, Relationship: "primary"}}, WorkOrderMessage: &api.MessageReference{TaskID: task.ID, Seq: orders[1].Seq}, Envelope: &api.Envelope{Kind: api.EnvelopeKindRequest, To: worker.Name, Subject: "Check item B", Body: api.EnvelopeBody{Ask: "Check B."}}}, by)
	if err != nil {
		t.Fatal(err)
	}
	obligations, err := s.BrokerOpenObligations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, obligation := range obligations {
		if obligation.MessageSeq != message.Seq {
			continue
		}
		if err := s.BrokerEscalate(ctx, obligation, 1, "synthetic overdue", s.now()); err != nil {
			t.Fatal(err)
		}
		messages, err := s.ListMessages(ctx, task.ID, message.Seq, "", 20)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range messages {
			if m.To == leads[1].ID && m.Text != "" {
				return
			}
		}
		t.Fatalf("item B escalation missed its own lead: %+v", messages)
	}
	t.Fatal("item B obligation was not recorded")
}
