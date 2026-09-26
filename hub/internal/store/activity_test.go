package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

func TestActivityTransitionReceiptsAndSnapshots(t *testing.T) {
	ctx := context.Background()
	by := api.Caller{Node: "fixture", User: "owner"}
	path := filepath.Join(t.TempDir(), "hub.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	task, err := s.CreateTask(ctx, api.CreateTaskRequest{Name: "Activity fixture"}, by)
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: "builder", Host: "mini", Session: "fake", Runtime: "codex", Cwd: t.TempDir()}, by)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 25, 20, 0, 0, 0, time.UTC)
	report := api.ActivityReport{RequestID: "activity-one", RunID: a.RunID, Activity: api.AgentActivity{State: "working", ObservedAt: now, Tokens: api.TokenTotals{Input: 10, Output: 5, Total: 15}}}
	if _, err := s.ReportActivity(ctx, task.ID, a.ID, report); err != nil {
		t.Fatal(err)
	}
	newer := report
	newer.RequestID = "activity-steady"
	newer.Activity.Tokens.Total = 25
	got, err := s.ReportActivity(ctx, task.ID, a.ID, newer)
	if err != nil || got.Tokens.Total != 15 {
		t.Fatalf("steady write: %+v %v", got, err)
	}
	if _, err := s.ReportActivity(ctx, task.ID, a.ID, report); err != nil {
		t.Fatalf("retry: %v", err)
	}
	changed := report
	changed.Activity.Tokens.Total = 26
	if _, err := s.ReportActivity(ctx, task.ID, a.ID, changed); err == nil {
		t.Fatal("changed retry accepted")
	}
	gotAgent, err := s.GetAgent(ctx, a.ID)
	if err != nil || gotAgent.Activity == nil || gotAgent.Activity.Tokens.Total != 15 {
		t.Fatalf("agent snapshot %+v %v", gotAgent.Activity, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	gotAgent, err = s.GetAgent(ctx, a.ID)
	if err != nil || gotAgent.Activity == nil || gotAgent.Activity.Tokens.Total != 15 {
		t.Fatalf("restart snapshot %+v %v", gotAgent.Activity, err)
	}
	if _, err := s.CloseAgent(ctx, a.ID, by); err != nil {
		t.Fatal(err)
	}
	gotAgent, err = s.GetAgent(ctx, a.ID)
	if err != nil || gotAgent.Status != api.AgentClosed || gotAgent.Activity == nil || gotAgent.Activity.Tokens.Total != 15 {
		t.Fatalf("closed-run snapshot %+v %v", gotAgent, err)
	}
	var receipts int
	if err := s.db.QueryRow(`SELECT count(*) FROM agent_activity_receipts`).Scan(&receipts); err != nil || receipts != 1 {
		t.Fatalf("transition receipts %d %v", receipts, err)
	}
}

func TestActivityAlertRoutingAndTeamIsolation(t *testing.T) {
	s, task, items, orders := queueFixture(t)
	ctx := context.Background()
	by := api.Caller{Node: "fixture", User: "owner"}
	for i := range items {
		if _, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: []string{"activity-team-one", "activity-team-two"}[i], Operation: "add", ItemID: items[i].ID, OrderMessageSeq: orders[i].Seq, Host: "mini", Cwd: "/tmp"}); err != nil {
			t.Fatal(err)
		}
	}
	add := func(name string, item int) api.Agent {
		t.Helper()
		a, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: name, Host: "mini", Session: name, Runtime: "codex", Cwd: t.TempDir()}, by)
		if err != nil {
			t.Fatal(err)
		}
		_, err = s.db.Exec(`INSERT INTO agent_work_item_bindings(agent_id,run_id,item_task_id,item_id,item_revision,work_order_task_id,work_order_message_seq,context_through_message_seq,context_digest,context_json,created_at) VALUES(?,?,?,?,1,?,?,0,?,?,?)`, a.ID, a.RunID, task.ID, items[item].ID, task.ID, orders[item].Seq, "digest", []byte("{}"), ts(s.now()))
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	lead := add("lead-one", 0)
	member := add("member-one", 0)
	other := add("lead-two", 1)
	for i, a := range []api.Agent{lead, other} {
		if _, err := s.db.Exec(`INSERT INTO item_team_leads(task_id,item_id,agent_id,run_id,revision,state) VALUES(?,?,?,?,1,'running')`, task.ID, items[i].ID, a.ID, a.RunID); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Date(2026, 9, 25, 20, 0, 0, 0, time.UTC)
	transition := func(a api.Agent, id, state string) {
		t.Helper()
		_, err := s.ReportActivity(ctx, task.ID, a.ID, api.ActivityReport{RequestID: id, RunID: a.RunID, Activity: api.AgentActivity{State: state, ObservedAt: now, Tokens: api.TokenTotals{Total: 12}}})
		if err != nil {
			t.Fatal(err)
		}
	}
	var leadAlerts, otherAlerts, ownerAlerts int
	count := func() {
		t.Helper()
		for _, v := range []struct {
			to string
			n  *int
		}{{lead.ID, &leadAlerts}, {other.ID, &otherAlerts}, {"", &ownerAlerts}} {
			if err := s.db.QueryRow(`SELECT count(*) FROM messages WHERE task_id=? AND from_node=? AND to_agent=?`, task.ID, api.BrokerNode, v.to).Scan(v.n); err != nil {
				t.Fatal(err)
			}
		}
	}
	transition(member, "uncertain-process", "unknown")
	count()
	if leadAlerts != 0 || otherAlerts != 0 || ownerAlerts != 0 {
		t.Fatalf("uncertain process alerted lead=%d other=%d owner=%d", leadAlerts, otherAlerts, ownerAlerts)
	}
	transition(member, "alert-hung", "hung_tool")
	count()
	if leadAlerts != 1 || otherAlerts != 0 || ownerAlerts != 0 {
		t.Fatalf("hung routing lead=%d other=%d owner=%d", leadAlerts, otherAlerts, ownerAlerts)
	}
	transition(member, "alert-hung-steady", "hung_tool")
	count()
	if leadAlerts != 1 {
		t.Fatal("steady state alerted again")
	}
	transition(member, "alert-crashed", "crashed")
	count()
	if leadAlerts != 2 || ownerAlerts != 1 || otherAlerts != 0 {
		t.Fatalf("crash routing lead=%d other=%d owner=%d", leadAlerts, otherAlerts, ownerAlerts)
	}
	transition(lead, "alert-lead-loop", "looping")
	count()
	if ownerAlerts != 2 || leadAlerts != 2 {
		t.Fatalf("lead routing lead=%d owner=%d", leadAlerts, ownerAlerts)
	}
	queue, err := s.ListTeamQueue(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(queue.Entries) != 2 || queue.Entries[0].Tokens.Total != 24 || queue.Entries[1].Tokens.Total != 0 {
		t.Fatalf("team totals not isolated: %+v", queue.Entries)
	}
}
