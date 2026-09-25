package store

import (
	"context"
	"strings"
	"testing"

	"github.com/scs32/tailterm/hub/internal/api"
)

func TestParallelAllocationIntentRequiresExactItemLeadRun(t *testing.T) {
	s, task, items, orders := queueFixture(t)
	ctx := context.Background()
	by := api.Caller{Node: "fixture", User: "owner"}
	var leads []api.Agent
	for i := range items {
		lead, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: []string{"allocation-lead-a", "allocation-lead-b"}[i], AgentID: api.NewID("agt"), Host: "mini", Session: "lead"}, by)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`UPDATE agents SET status='running',last_seen_at=? WHERE id=?`, ts(s.now()), lead.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`INSERT INTO item_team_leads(task_id,item_id,agent_id,run_id,revision,state) VALUES(?,?,?,?,1,'running')`, task.ID, items[i].ID, lead.ID, lead.RunID); err != nil {
			t.Fatal(err)
		}
		leads = append(leads, lead)
	}
	intent := func(author api.Agent, item int) api.CreateAllocationIntentRequest {
		return api.CreateAllocationIntentRequest{AgentID: api.NewID("agt"), ItemTaskID: task.ID, ItemID: items[item].ID, ItemRevision: items[item].Revision, WorkOrderMessage: api.MessageReference{TaskID: task.ID, Seq: orders[item].Seq}, TeamRole: api.TeamRoleMember, TargetTaskID: task.ID, ContextDigest: strings.Repeat("a", 64), AuthorAgentID: author.ID, AuthorRunID: author.RunID, ExpectedRunID: api.NewID("run")}
	}
	if _, err := s.CreateAllocationIntent(ctx, task.ID, intent(leads[0], 0), by); err != nil {
		t.Fatalf("same-item lead refused: %v", err)
	}
	if _, err := s.CreateAllocationIntent(ctx, task.ID, intent(leads[0], 1), by); err == nil {
		t.Fatal("item A lead authored item B intent")
	}
	stale := intent(leads[1], 1)
	stale.AuthorRunID = api.NewID("run")
	if _, err := s.CreateAllocationIntent(ctx, task.ID, stale, by); err == nil {
		t.Fatal("stale lead run authored intent")
	}
	if _, err := s.db.Exec(`UPDATE item_team_leads SET agent_id=?,run_id=?,revision=revision+1 WHERE task_id=? AND item_id=?`, leads[0].ID, leads[0].RunID, task.ID, items[1].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateAllocationIntent(ctx, task.ID, intent(leads[1], 1), by); err == nil {
		t.Fatal("replaced item B lead retained intent authority")
	}
}
