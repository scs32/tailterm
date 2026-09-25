package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

func workItemStore(t *testing.T) (*Store, context.Context, api.Caller) {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "hub.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, context.Background(), api.Caller{Node: "test-node", User: "owner"}
}

func workItemProject(t *testing.T, s *Store, ctx context.Context, by api.Caller, name, orchestrator string) (api.Task, api.Agent) {
	t.Helper()
	task, err := s.CreateTask(ctx, api.CreateTaskRequest{Name: name, Orchestrator: orchestrator}, by)
	if err != nil {
		t.Fatal(err)
	}
	var agent api.Agent
	if orchestrator != "" {
		agent, err = s.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: orchestrator, Host: "host", Session: orchestrator, Runtime: "codex"}, by)
		if err != nil {
			t.Fatal(err)
		}
	}
	return task, agent
}

func createWorkItem(t *testing.T, s *Store, ctx context.Context, by api.Caller, task api.Task, requestID string) api.WorkItem {
	t.Helper()
	item, err := s.CreateWorkItem(ctx, task.ID, api.CreateWorkItemRequest{Kind: "bug", Title: "Retry loses state", Description: "Reproduce with a dropped response.", RequestID: requestID}, by)
	if err != nil {
		t.Fatal(err)
	}
	return item
}

func TestWorkItemCRUDScopesRevisionsAndRetryReceipts(t *testing.T) {
	s, ctx, by := workItemStore(t)
	project, _ := workItemProject(t, s, ctx, by, "Source", "lead")
	other, _ := workItemProject(t, s, ctx, by, "Other", "otherlead")
	handler, err := s.AddAgent(ctx, project.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: "database", Host: "host", Session: "database", Runtime: "codex", Role: api.AgentRoleDatabaseHandler}, by)
	if err != nil {
		t.Fatal(err)
	}
	source, err := s.PostMessage(ctx, project.ID, api.PostMessageRequest{AgentID: handler.ID, Text: "Please record the retry bug"}, by)
	if err != nil {
		t.Fatal(err)
	}
	req := api.CreateWorkItemRequest{Kind: "bug", Title: "Retry loses state", Description: "Reproduce with a dropped response.", AgentID: handler.ID, SourceMessageSeq: source.Seq, RequestID: "source-1-item-1"}
	item, err := s.CreateWorkItem(ctx, project.ID, req, by)
	if err != nil {
		t.Fatal(err)
	}
	if !api.ValidID(item.ID, "wi") || item.Seq == 0 || item.TaskID != project.ID || item.Status != "open" || item.Priority != "normal" || item.Revision != 1 || item.CreatedBy.AgentID != handler.ID || item.SourceMessageSeq != source.Seq {
		t.Fatalf("created item: %+v", item)
	}
	replayed, err := s.CreateWorkItem(ctx, project.ID, req, by)
	if err != nil || replayed.ID != item.ID {
		t.Fatalf("create replay: %+v %v", replayed, err)
	}
	changedRequest := req
	changedRequest.Title = "Different payload"
	if _, err = s.CreateWorkItem(ctx, project.ID, changedRequest, by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("changed create replay = %v", err)
	}
	if _, err = s.GetWorkItem(ctx, other.ID, item.ID); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("cross-project get = %v", err)
	}

	feature, err := s.CreateWorkItem(ctx, other.ID, api.CreateWorkItemRequest{Kind: "feature", Title: "Compact filters", Priority: "urgent", RequestID: "feature-1"}, by)
	if err != nil {
		t.Fatal(err)
	}
	global, err := s.ListWorkItems(ctx, "", "", "", 0, 10)
	if err != nil || len(global.Items) != 2 || global.Next != feature.Seq {
		t.Fatalf("global list: %+v %v", global, err)
	}
	bugs, err := s.ListWorkItems(ctx, project.ID, "bug", "open", 0, 10)
	if err != nil || len(bugs.Items) != 1 || bugs.Items[0].ID != item.ID {
		t.Fatalf("scoped list: %+v %v", bugs, err)
	}
	page, err := s.ListWorkItems(ctx, "", "", "", item.Seq, 10)
	if err != nil || len(page.Items) != 1 || page.Items[0].ID != feature.ID {
		t.Fatalf("after cursor: %+v %v", page, err)
	}

	status, priority, title := "in_progress", "urgent", "Retry receipt is lost"
	updated, err := s.UpdateWorkItem(ctx, project.ID, item.ID, api.UpdateWorkItemRequest{Revision: 1, Status: &status, Priority: &priority, Title: &title, AgentID: handler.ID}, by)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Revision != 2 || updated.Status != status || updated.Priority != priority || updated.Title != title || updated.UpdatedBy.AgentID != handler.ID || updated.TaskID != project.ID || updated.Kind != item.Kind {
		t.Fatalf("updated item: %+v", updated)
	}
	if _, err = s.UpdateWorkItem(ctx, project.ID, item.ID, api.UpdateWorkItemRequest{Revision: 1, Status: &status}, by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("stale update = %v", err)
	}
	if _, err = s.CloseTask(ctx, project.ID, by); err != nil {
		t.Fatal(err)
	}
	if _, err = s.UpdateWorkItem(ctx, project.ID, item.ID, api.UpdateWorkItemRequest{Revision: 2, Status: &status}, by); !errors.Is(err, api.ErrClosed) {
		t.Fatalf("closed update = %v", err)
	}
	if _, err = s.CreateWorkItem(ctx, project.ID, api.CreateWorkItemRequest{Kind: "bug", Title: "Late", RequestID: "late"}, by); !errors.Is(err, api.ErrClosed) {
		t.Fatalf("closed create = %v", err)
	}
	// An already-committed retry receipt stays readable after closure.
	if got, err := s.CreateWorkItem(ctx, project.ID, req, by); err != nil || got.ID != item.ID {
		t.Fatalf("closed receipt replay: %+v %v", got, err)
	}
}

func TestWorkItemConcurrentRetriesAndRevisionCAS(t *testing.T) {
	s, ctx, by := workItemStore(t)
	project, _ := workItemProject(t, s, ctx, by, "Concurrent", "lead")
	req := api.CreateWorkItemRequest{Kind: "feature", Title: "Concurrent receipt", RequestID: "concurrent-create"}
	const attempts = 8
	ids := make(chan string, attempts)
	errs := make(chan error, attempts)
	var wg sync.WaitGroup
	for range attempts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			item, err := s.CreateWorkItem(ctx, project.ID, req, by)
			ids <- item.ID
			errs <- err
		}()
	}
	wg.Wait()
	close(ids)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	itemID := ""
	for id := range ids {
		if itemID == "" {
			itemID = id
		}
		if id != itemID {
			t.Fatalf("retry created multiple items: %s and %s", itemID, id)
		}
	}
	statuses := []string{"in_progress", "blocked"}
	updateErrs := make(chan error, len(statuses))
	for _, status := range statuses {
		wg.Add(1)
		go func(status string) {
			defer wg.Done()
			_, err := s.UpdateWorkItem(ctx, project.ID, itemID, api.UpdateWorkItemRequest{Revision: 1, Status: &status}, by)
			updateErrs <- err
		}(status)
	}
	wg.Wait()
	close(updateErrs)
	successes, conflicts := 0, 0
	for err := range updateErrs {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, api.ErrConflict):
			conflicts++
		default:
			t.Fatal(err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("revision CAS successes=%d conflicts=%d", successes, conflicts)
	}
}

func TestWorkItemSourceMessageMustBelongToProjectMember(t *testing.T) {
	s, ctx, by := workItemStore(t)
	project, _ := workItemProject(t, s, ctx, by, "Source", "lead")
	other, outsider := workItemProject(t, s, ctx, by, "Other", "outsider")
	valid, err := s.PostMessage(ctx, project.ID, api.PostMessageRequest{Text: "Human source"}, by)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.CreateWorkItem(ctx, project.ID, api.CreateWorkItemRequest{Kind: "feature", Title: "Human intake", SourceMessageSeq: valid.Seq, RequestID: "human-source"}, by); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CreateWorkItem(ctx, project.ID, api.CreateWorkItemRequest{Kind: "bug", Title: "Wrong sequence", SourceMessageSeq: 999999, RequestID: "bad-seq"}, by); !errors.Is(err, api.ErrInvalid) {
		t.Fatalf("missing source = %v", err)
	}
	now := ts(time.Now())
	result, err := s.db.ExecContext(ctx, `INSERT INTO messages(task_id,from_agent,from_node,from_user,to_agent,text,created_at,reply_to,broadcast) VALUES(?,?,?,?,?,?,?,?,?)`, project.ID, outsider.ID, by.Node, by.User, "", "forged cross-member", now, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	seq, _ := result.LastInsertId()
	if _, err = s.CreateWorkItem(ctx, project.ID, api.CreateWorkItemRequest{Kind: "bug", Title: "Cross member", SourceMessageSeq: seq, RequestID: "cross-member"}, by); !errors.Is(err, api.ErrInvalid) {
		t.Fatalf("cross-project source sender = %v", err)
	}
	_ = other
}

func TestWorkItemDispatchIsAtomicIdempotentAndResumesHumanTarget(t *testing.T) {
	s, ctx, by := workItemStore(t)
	source, sourceLead := workItemProject(t, s, ctx, by, "Source", "lead")
	target, targetLead := workItemProject(t, s, ctx, by, "Target", "targetlead")
	upper := "TARGETLEAD"
	if _, err := s.UpdateTask(ctx, target.ID, api.UpdateTaskRequest{Orchestrator: &upper}, by); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PostEvent(ctx, target.ID, api.PostEventRequest{AgentID: targetLead.ID, RunID: targetLead.RunID, Kind: api.EventHeartbeat}, by); err != nil {
		t.Fatal(err)
	}
	retired := api.AgentRetired
	if _, err := s.UpdateAgent(ctx, targetLead.ID, api.UpdateAgentRequest{Status: &retired}, by); err != nil {
		t.Fatal(err)
	}
	item := createWorkItem(t, s, ctx, by, source, "dispatch-item")
	req := api.DispatchWorkItemRequest{Revision: item.Revision, TargetTaskID: target.ID, RequestID: "dispatch-1"}
	result, err := s.DispatchWorkItem(ctx, source.ID, item.ID, req, by)
	if err != nil {
		t.Fatal(err)
	}
	if result.Dispatch.ItemID != item.ID || result.Dispatch.TargetTaskID != target.ID || result.Dispatch.TargetAgentID != targetLead.ID || result.Dispatch.MessageSeq == 0 || result.Item.LastDispatch == nil || result.Item.LastDispatch.ID != result.Dispatch.ID {
		t.Fatalf("dispatch: %+v", result)
	}
	resumed, err := s.GetAgent(ctx, targetLead.ID)
	if err != nil || resumed.Status != api.AgentDone || resumed.RunID != targetLead.RunID {
		t.Fatalf("human dispatch did not resume same target: %+v %v", resumed, err)
	}
	messages, err := s.ListMessages(ctx, target.ID, 0, "", 10)
	if err != nil || len(messages) != 1 || messages[0].To != targetLead.ID || messages[0].From.AgentID != "" {
		t.Fatalf("dispatch board message: %+v %v", messages, err)
	}
	replay, err := s.DispatchWorkItem(ctx, source.ID, item.ID, req, by)
	if err != nil || replay.Dispatch.ID != result.Dispatch.ID || replay.Dispatch.MessageSeq != result.Dispatch.MessageSeq {
		t.Fatalf("dispatch replay: %+v %v", replay, err)
	}
	messages, _ = s.ListMessages(ctx, target.ID, 0, "", 10)
	if len(messages) != 1 {
		t.Fatalf("dispatch replay duplicated message: %d", len(messages))
	}
	changed := req
	changed.TargetTaskID = source.ID
	if _, err = s.DispatchWorkItem(ctx, source.ID, item.ID, changed, by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("changed dispatch replay = %v", err)
	}
	if _, err = s.DispatchWorkItem(ctx, source.ID, item.ID, api.DispatchWorkItemRequest{Revision: item.Revision, TargetTaskID: target.ID, AgentID: sourceLead.ID, RequestID: "agent-cross"}, by); !errors.Is(err, api.ErrInvalid) {
		t.Fatalf("agent cross-project dispatch = %v", err)
	}
	if _, err = s.PostEvent(ctx, source.ID, api.PostEventRequest{AgentID: sourceLead.ID, RunID: sourceLead.RunID, Kind: api.EventHeartbeat}, by); err != nil {
		t.Fatal(err)
	}
	if _, err = s.UpdateAgent(ctx, sourceLead.ID, api.UpdateAgentRequest{Status: &retired}, by); err != nil {
		t.Fatal(err)
	}
	local, err := s.DispatchWorkItem(ctx, source.ID, item.ID, api.DispatchWorkItemRequest{Revision: item.Revision, AgentID: sourceLead.ID, RequestID: "agent-local"}, by)
	if err != nil || local.Dispatch.TargetTaskID != source.ID || local.Dispatch.TargetAgentID != sourceLead.ID {
		t.Fatalf("agent owning-project dispatch: %+v %v", local, err)
	}
	if got, err := s.GetAgent(ctx, sourceLead.ID); err != nil || got.Status != api.AgentRetired {
		t.Fatalf("agent dispatch resumed retired orchestrator: %+v %v", got, err)
	}
	if _, err = s.CloseTask(ctx, target.ID, by); err != nil {
		t.Fatal(err)
	}
	if _, err = s.DispatchWorkItem(ctx, source.ID, item.ID, api.DispatchWorkItemRequest{Revision: item.Revision, TargetTaskID: target.ID, RequestID: "closed-target"}, by); !errors.Is(err, api.ErrClosed) {
		t.Fatalf("closed target dispatch = %v", err)
	}
	wrongRoleTask, _ := workItemProject(t, s, ctx, by, "Wrong role", "")
	handler, err := s.AddAgent(ctx, wrongRoleTask.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: "database", Host: "host", Session: "database", Role: api.AgentRoleDatabaseHandler}, by)
	if err != nil {
		t.Fatal(err)
	}
	wrongOrchestrator := handler.Name
	if _, err = s.UpdateTask(ctx, wrongRoleTask.ID, api.UpdateTaskRequest{Orchestrator: &wrongOrchestrator}, by); err != nil {
		t.Fatal(err)
	}
	if _, err = s.DispatchWorkItem(ctx, source.ID, item.ID, api.DispatchWorkItemRequest{Revision: item.Revision, TargetTaskID: wrongRoleTask.ID, RequestID: "wrong-role"}, by); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "role database_handler") {
		t.Fatalf("orchestrator role mismatch = %v", err)
	}
}

func TestWorkItemWritesRollbackWithoutPartialReceipts(t *testing.T) {
	t.Run("create audit failure", func(t *testing.T) {
		s, ctx, by := workItemStore(t)
		project, _ := workItemProject(t, s, ctx, by, "Source", "lead")
		if _, err := s.db.ExecContext(ctx, `CREATE TRIGGER fail_item_change BEFORE INSERT ON work_item_changes BEGIN SELECT RAISE(ABORT, 'change failed'); END`); err != nil {
			t.Fatal(err)
		}
		if _, err := s.CreateWorkItem(ctx, project.ID, api.CreateWorkItemRequest{Kind: "bug", Title: "Atomic create", RequestID: "atomic-create"}, by); err == nil {
			t.Fatal("create unexpectedly succeeded")
		}
		for _, table := range []string{"work_items", "work_item_requests"} {
			var count int
			if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM `+table).Scan(&count); err != nil || count != 0 {
				t.Fatalf("%s retained rows: %d %v", table, count, err)
			}
		}
	})
	t.Run("dispatch message failure", func(t *testing.T) {
		s, ctx, by := workItemStore(t)
		source, _ := workItemProject(t, s, ctx, by, "Source", "lead")
		target, targetLead := workItemProject(t, s, ctx, by, "Target", "targetlead")
		if _, err := s.PostEvent(ctx, target.ID, api.PostEventRequest{AgentID: targetLead.ID, RunID: targetLead.RunID, Kind: api.EventHeartbeat}, by); err != nil {
			t.Fatal(err)
		}
		retired := api.AgentRetired
		if _, err := s.UpdateAgent(ctx, targetLead.ID, api.UpdateAgentRequest{Status: &retired}, by); err != nil {
			t.Fatal(err)
		}
		item := createWorkItem(t, s, ctx, by, source, "atomic-item")
		if _, err := s.db.ExecContext(ctx, `CREATE TRIGGER fail_dispatch_message BEFORE INSERT ON messages BEGIN SELECT RAISE(ABORT, 'message failed'); END`); err != nil {
			t.Fatal(err)
		}
		if _, err := s.DispatchWorkItem(ctx, source.ID, item.ID, api.DispatchWorkItemRequest{Revision: 1, TargetTaskID: target.ID, RequestID: "atomic-dispatch"}, by); err == nil {
			t.Fatal("dispatch unexpectedly succeeded")
		}
		var dispatches, requests int
		_ = s.db.QueryRowContext(ctx, `SELECT count(*) FROM work_item_dispatches`).Scan(&dispatches)
		_ = s.db.QueryRowContext(ctx, `SELECT count(*) FROM work_item_requests WHERE operation='dispatch'`).Scan(&requests)
		got, _ := s.GetAgent(ctx, targetLead.ID)
		if dispatches != 0 || requests != 0 || got.Status != api.AgentRetired {
			t.Fatalf("partial dispatch: dispatches=%d requests=%d target=%+v", dispatches, requests, got)
		}
	})
}

func TestDatabaseHandlerPoolAndRestartCASPreservesIdentity(t *testing.T) {
	s, ctx, by := workItemStore(t)
	project, _ := workItemProject(t, s, ctx, by, "Handlers", "")
	id := api.NewID("agt")
	req := api.AddAgentRequest{AgentID: id, Name: "database", Host: "host", Session: "handler-" + id, Runtime: "codex", Cwd: "/project", Role: api.AgentRoleDatabaseHandler}
	handler, err := s.AddAgent(ctx, project.ID, req, by)
	if err != nil {
		t.Fatal(err)
	}
	if handler.Role != api.AgentRoleDatabaseHandler || handler.ParentAgentID != "" {
		t.Fatalf("handler role: %+v", handler)
	}
	replay, err := s.AddAgent(ctx, project.ID, req, by)
	if err != nil || replay.ID != handler.ID || replay.RunID != handler.RunID {
		t.Fatalf("same attempt replay: %+v %v", replay, err)
	}
	changed := req
	changed.Cwd = "/other"
	if _, err = s.AddAgent(ctx, project.ID, changed, by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("changed handler replay = %v", err)
	}
	second, err := s.AddAgent(ctx, project.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: "database2", Host: "host", Session: "database2", Runtime: "codex", Role: api.AgentRoleDatabaseHandler}, by)
	if err != nil || second.ID == handler.ID {
		t.Fatalf("second handler = %+v %v", second, err)
	}
	retired := api.AgentRetired
	if _, err = s.UpdateAgent(ctx, handler.ID, api.UpdateAgentRequest{Status: &retired}, by); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AddAgent(ctx, project.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: "database2", Host: "host", Session: "database2", Runtime: "codex", Role: api.AgentRoleDatabaseHandler}, by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("duplicate handler name = %v", err)
	}
	if _, err = s.PostEvent(ctx, project.ID, api.PostEventRequest{AgentID: handler.ID, RunID: handler.RunID, Kind: api.EventExited}, by); err != nil {
		t.Fatal(err)
	}
	newIdentity := req
	newIdentity.AgentID = api.NewID("agt")
	if _, err = s.AddAgent(ctx, project.ID, newIdentity, by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("exited handler restarted through new identity = %v", err)
	}
	if _, err = s.AddAgent(ctx, project.ID, req, by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("exited restart without CAS = %v", err)
	}
	wrong := req
	wrong.ExpectedRunID = "run_0000000000000000"
	if _, err = s.AddAgent(ctx, project.ID, wrong, by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("wrong restart CAS = %v", err)
	}
	restart := req
	restart.ExpectedRunID = handler.RunID
	restarted, err := s.AddAgent(ctx, project.ID, restart, by)
	if err != nil || restarted.ID != handler.ID || restarted.RunID == handler.RunID || restarted.Status != api.AgentStarting || restarted.Role != handler.Role {
		t.Fatalf("handler restart: before=%+v after=%+v err=%v", handler, restarted, err)
	}
	if _, err = s.CloseAgent(ctx, restarted.ID, by); err != nil {
		t.Fatal(err)
	}
	replacement, err := s.AddAgent(ctx, project.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: "database3", Host: "host", Session: "database3", Runtime: "codex", Role: api.AgentRoleDatabaseHandler}, by)
	if err != nil || replacement.ID == handler.ID {
		t.Fatalf("closed handler replacement: %+v %v", replacement, err)
	}
}

func TestDatabaseHandlerRestartHonorsActiveAgentCap(t *testing.T) {
	s, ctx, by := workItemStore(t)
	project, _ := workItemProject(t, s, ctx, by, "Capacity", "")
	req := api.AddAgentRequest{AgentID: api.NewID("agt"), Name: "database", Host: "host", Session: "database", Runtime: "codex", Role: api.AgentRoleDatabaseHandler}
	handler, err := s.AddAgent(ctx, project.ID, req, by)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.PostEvent(ctx, project.ID, api.PostEventRequest{AgentID: handler.ID, RunID: handler.RunID, Kind: api.EventExited}, by); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"one", "two"} {
		if _, err = s.AddAgent(ctx, project.ID, api.AddAgentRequest{Name: name, Host: "host", Session: name}, by); err != nil {
			t.Fatal(err)
		}
	}
	s.MaxAgents = 2
	req.ExpectedRunID = handler.RunID
	if _, err = s.AddAgent(ctx, project.ID, req, by); !errors.Is(err, api.ErrLimit) {
		t.Fatalf("handler restart exceeded active cap: %v", err)
	}
	got, err := s.GetAgent(ctx, handler.ID)
	if err != nil || got.Status != api.AgentExited || got.RunID != handler.RunID {
		t.Fatalf("capacity failure changed handler: %+v %v", got, err)
	}
}

func TestWorkItemMigrationPreservesExistingAgentData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(schema); err != nil {
		t.Fatal(err)
	}
	now := ts(time.Now())
	taskID, agentID := api.NewID("tsk"), api.NewID("agt")
	if _, err = db.Exec(`INSERT INTO tasks(id,name,goal,status,created_at,created_node,created_user) VALUES(?,?,?,'open',?,?,?)`, taskID, "Legacy", "", now, "node", "user"); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO agents(id,task_id,name,host,session,status,created_at,last_event_at) VALUES(?,?,?,?,?,'done',?,?)`, agentID, taskID, "legacy", "host", "legacy", now, now); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	agent, err := s.GetAgent(context.Background(), agentID)
	if err != nil || agent.Name != "legacy" || agent.Role != "" {
		t.Fatalf("legacy agent: %+v %v", agent, err)
	}
	task, err := s.GetTask(context.Background(), taskID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.CreateWorkItem(context.Background(), task.ID, api.CreateWorkItemRequest{Kind: "feature", Title: "Migrated storage", RequestID: "migration"}, api.Caller{Node: "node", User: "user"}); err != nil {
		t.Fatal(err)
	}
}
