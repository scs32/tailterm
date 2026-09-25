package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"

	"github.com/scs32/tailterm/hub/internal/api"
)

func queueFixture(t *testing.T) (*Store, api.Task, []api.WorkItem, []api.Message) {
	t.Helper()
	ctx := context.Background()
	by := api.Caller{Node: "fixture", User: "owner"}
	s, err := Open(filepath.Join(t.TempDir(), "hub.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	task, err := s.CreateTask(ctx, api.CreateTaskRequest{Name: "Queue fixture"}, by)
	if err != nil {
		t.Fatal(err)
	}
	var items []api.WorkItem
	var orders []api.Message
	for i := 0; i < 2; i++ {
		item, err := s.CreateWorkItem(ctx, task.ID, api.CreateWorkItemRequest{Kind: "feature", Title: "Queued work", Priority: "normal", RequestID: api.NewID("req")}, by)
		if err != nil {
			t.Fatal(err)
		}
		order := contextLinkedMessage(t, s, task, item, "bounded order", api.NewID("req"), nil)
		items = append(items, item)
		orders = append(orders, order)
	}
	handler, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "database", Role: api.AgentRoleDatabaseHandler, AgentID: api.NewID("agt"), Host: "mini", Session: "database"}, by)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.db.Exec(`UPDATE agents SET last_seen_at=?,status='running' WHERE id=?`, ts(s.now()), handler.ID)
	if err != nil {
		t.Fatal(err)
	}
	return s, task, items, orders
}

func TestTeamQueueTwoRunnersAndManualLaunchRace(t *testing.T) {
	s, task, items, orders := queueFixture(t)
	ctx := context.Background()
	q, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "add", Operation: "add", ItemID: items[0].ID, OrderMessageSeq: orders[0].Seq, Host: "mini", Cwd: "/tmp"})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: []string{"runner-one", "runner-two"}[i], Operation: "claim", EntryID: q.ID, ExpectedRevision: q.Revision, Host: "mini"})
			results <- err
		}(i)
	}
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		}
	}
	if success != 1 {
		t.Fatalf("runner claims=%d", success)
	}
	if _, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "manual-other", Operation: "manual", ItemID: items[1].ID, OrderMessageSeq: orders[1].Seq}); err == nil {
		t.Fatal("manual launch crossed runner claim")
	}
	name := "other-lead"
	if _, err := s.UpdateTask(ctx, task.ID, api.UpdateTaskRequest{Orchestrator: &name}, api.Caller{Node: "fixture", User: "owner"}); err == nil {
		t.Fatal("unreserved lead assignment crossed runner claim")
	}
}

func TestTeamQueueClaimRefusesUnavailableHandlerLiveLeadAndPause(t *testing.T) {
	for _, condition := range []string{"offline-handler", "live-lead", "paused"} {
		t.Run(condition, func(t *testing.T) {
			s, task, items, orders := queueFixture(t)
			ctx := context.Background()
			q, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "add", Operation: "add", ItemID: items[0].ID, OrderMessageSeq: orders[0].Seq, Host: "mini", Cwd: "/tmp"})
			if err != nil {
				t.Fatal(err)
			}
			switch condition {
			case "offline-handler":
				_, err = s.db.Exec(`UPDATE agents SET last_seen_at='' WHERE task_id=? AND role=?`, task.ID, api.AgentRoleDatabaseHandler)
			case "live-lead":
				name := "manual-lead"
				_, err = s.UpdateTask(ctx, task.ID, api.UpdateTaskRequest{Orchestrator: &name}, api.Caller{Node: "fixture", User: "owner"})
			case "paused":
				_, err = s.db.Exec(`UPDATE tasks SET pause_state='paused',pause_generation=pause_generation+1 WHERE id=?`, task.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "claim", Operation: "claim", EntryID: q.ID, ExpectedRevision: q.Revision, Host: "mini"}); err == nil {
				t.Fatal("unsafe claim accepted")
			}
		})
	}
}

func TestTeamQueueReopensWithSavedEntry(t *testing.T) {
	s, task, items, orders := queueFixture(t)
	ctx := context.Background()
	q, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "add", Operation: "add", ItemID: items[0].ID, OrderMessageSeq: orders[0].Seq, Host: "mini", Cwd: "/tmp"})
	if err != nil {
		t.Fatal(err)
	}
	var seq int
	var name, path string
	if err := s.db.QueryRow(`PRAGMA database_list`).Scan(&seq, &name, &path); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	list, err := reopened.ListTeamQueue(ctx, task.ID)
	if err != nil || len(list.Entries) != 1 || list.Entries[0].ID != q.ID {
		t.Fatalf("reopened queue %+v %v", list, err)
	}
}

func TestTeamQueuePersistsOrderAndRefusesBadOrders(t *testing.T) {
	s, task, items, orders := queueFixture(t)
	ctx := context.Background()
	add := func(i int, request string) api.TeamQueueEntry {
		t.Helper()
		q, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: request, Operation: "add", ItemID: items[i].ID, OrderMessageSeq: orders[i].Seq, Template: "planned", Host: "mini", Cwd: "/tmp"})
		if err != nil {
			t.Fatal(err)
		}
		return q
	}
	if _, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "missing-order", Operation: "add", ItemID: items[0].ID, OrderMessageSeq: 999999, Host: "mini", Cwd: "/tmp"}); err == nil {
		t.Fatal("missing order accepted")
	}
	if _, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "wrong-order", Operation: "add", ItemID: items[0].ID, OrderMessageSeq: orders[1].Seq, Host: "mini", Cwd: "/tmp"}); err == nil {
		t.Fatal("wrong order accepted")
	}
	q1, q2 := add(0, "add-one"), add(1, "add-two")
	if replay, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "add-one", Operation: "add", ItemID: items[0].ID, OrderMessageSeq: orders[0].Seq, Template: "planned", Host: "mini", Cwd: "/tmp"}); err != nil || replay.ID != q1.ID {
		t.Fatalf("retry %+v %v", replay, err)
	}
	if _, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "duplicate", Operation: "add", ItemID: items[0].ID, OrderMessageSeq: orders[0].Seq, Host: "mini", Cwd: "/tmp"}); err == nil {
		t.Fatal("duplicate item accepted")
	}
	q2, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "reorder-two", Operation: "reorder", EntryID: q2.ID, ExpectedRevision: q2.Revision, BeforeID: q1.ID})
	if err != nil {
		t.Fatal(err)
	}
	list, err := s.ListTeamQueue(ctx, task.ID)
	if err != nil || len(list.Entries) != 2 || list.Entries[0].ID != q2.ID {
		t.Fatalf("reorder %+v %v", list, err)
	}
	if _, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "stale", Operation: "remove", EntryID: q2.ID, ExpectedRevision: 1}); err == nil {
		t.Fatal("stale revision accepted")
	}
	if _, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "remove-two", Operation: "remove", EntryID: q2.ID, ExpectedRevision: q2.Revision}); err != nil {
		t.Fatal(err)
	}
	list, err = s.ListTeamQueue(ctx, task.ID)
	if err != nil || len(list.Entries) != 1 || list.Entries[0].ID != q1.ID {
		t.Fatalf("remove %+v %v", list, err)
	}
}

func TestTeamQueueClaimAttemptFailureIsFrozen(t *testing.T) {
	s, task, items, orders := queueFixture(t)
	ctx := context.Background()
	q, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "add", Operation: "add", ItemID: items[0].ID, OrderMessageSeq: orders[0].Seq, Host: "mini", Cwd: "/tmp"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "wrong-host", Operation: "claim", EntryID: q.ID, ExpectedRevision: q.Revision, Host: "other"}); err == nil {
		t.Fatal("wrong host claimed")
	}
	q, err = s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "claim", Operation: "claim", EntryID: q.ID, ExpectedRevision: q.Revision, Host: "mini"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "manual", Operation: "manual", ItemID: items[1].ID, OrderMessageSeq: orders[1].Seq}); err == nil {
		t.Fatal("manual launch crossed reservation")
	}
	plan, _ := json.Marshal(map[string]any{"task": task.ID, "item": items[0].ID, "revision": items[0].Revision, "order": orders[0].Seq, "context": map[string]any{"version": 1}, "members": []any{map[string]any{"state": "unstarted", "runId": api.NewID("run"), "fields": map[string]any{"agentId": api.NewID("agt"), "name": "lead", "cwd": "/tmp"}}}})
	q, err = s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "freeze", Operation: "freeze", EntryID: q.ID, ExpectedRevision: q.Revision, LaunchJSON: plan})
	if err != nil {
		t.Fatal(err)
	}
	q, err = s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "attempt", Operation: "attempt", EntryID: q.ID, ExpectedRevision: q.Revision})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "attempt-again", Operation: "attempt", EntryID: q.ID, ExpectedRevision: q.Revision}); err == nil {
		t.Fatal("uncertain spawn reattempted")
	}
	failure := api.TeamQueueRequest{RequestID: "fail", Operation: "fail", EntryID: q.ID, ExpectedRevision: q.Revision, Failure: "uncertain exact session"}
	q, err = s.TeamQueueAction(ctx, task.ID, failure)
	if err != nil || q.EscalationSeq < 1 {
		t.Fatalf("failure %+v %v", q, err)
	}
	if again, err := s.TeamQueueAction(ctx, task.ID, failure); err != nil || again.EscalationSeq != q.EscalationSeq {
		t.Fatalf("failure replay %+v %v", again, err)
	}
	messages, err := s.ListMessages(ctx, task.ID, 0, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	notices := 0
	for _, message := range messages {
		if message.Seq == q.EscalationSeq {
			notices++
			if message.Envelope == nil || message.Envelope.Kind != api.EnvelopeKindNotice || message.Envelope.Refs["escalation"] != "owner" {
				t.Fatalf("untyped owner notice %+v", message)
			}
		}
	}
	if notices != 1 {
		t.Fatalf("owner notices=%d", notices)
	}
}
