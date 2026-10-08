package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"path/filepath"
	"reflect"
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

// Token estimate and budget (wi_899863352c81e3b0, plan r2). An estimate is
// saved beside the item and never advances its revision; the budget shows it
// against the lifetime attributed tokens read from usage_item_shares.

func estimateRequest(revision int64, key string, tokens int64, basis string, by api.Agent) api.CreateWorkItemUpdate {
	req := api.CreateWorkItemUpdate{ExpectedRevision: revision, RequestID: key, AgentID: by.ID, RunID: by.RunID, EstimateTokens: &tokens}
	if basis != "" {
		req.EstimateBasis = &basis
	}
	return req
}

// tableDump is every row of a query as text, to prove a write left it alone.
func tableDump(t *testing.T, s *Store, query string, args ...any) string {
	t.Helper()
	rows, err := s.db.Query(query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	var out strings.Builder
	for rows.Next() {
		values := make([]any, len(cols))
		for i := range values {
			values[i] = new(sql.NullString)
		}
		if err := rows.Scan(values...); err != nil {
			t.Fatal(err)
		}
		for i, v := range values {
			out.WriteString(cols[i] + "=" + v.(*sql.NullString).String + "|")
		}
		out.WriteString("\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func estimateHistoryRows(t *testing.T, s *Store, item api.WorkItem) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM work_item_estimates WHERE task_id=? AND item_id=?`, item.TaskID, item.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func estimateHandler(t *testing.T, s *Store, ctx context.Context, by api.Caller, task api.Task) api.Agent {
	t.Helper()
	handler, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: "database", Host: "host", Session: "database", Runtime: "codex", Role: api.AgentRoleDatabaseHandler}, by)
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

// a1, a11: the handler's estimate shows on get while the item's revision,
// scope revision, updated time and updater, its revisions, its changes and a
// bound agent's context digest stay byte-identical.
func TestEstimateSaveKeepsItemRevisionAndHistory(t *testing.T) {
	s, ctx, by := workItemStore(t)
	task, lead := workItemProject(t, s, ctx, by, "Estimates", "lead")
	handler := estimateHandler(t, s, ctx, by, task)
	item := createWorkItem(t, s, ctx, by, task, "create")
	title := "Retry loses state after restart"
	if _, _, err := s.CreateWorkItemUpdate(ctx, task.ID, item.ID, api.CreateWorkItemUpdate{ExpectedRevision: 1, Title: &title, AgentID: lead.ID, RequestID: "retitle"}, by); err != nil {
		t.Fatal(err)
	}
	item.Revision = 2
	order := contextLinkedMessage(t, s, task, item, "bounded order", "order", nil)
	if _, err := s.db.Exec(`INSERT INTO agent_work_item_bindings(agent_id,run_id,item_task_id,item_id,item_revision,work_order_task_id,work_order_message_seq,context_through_message_seq,context_digest,context_json,team_role,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		lead.ID, lead.RunID, task.ID, item.ID, 2, task.ID, order.Seq, order.Seq, strings.Repeat("d", 64), `{"version":1}`, "member", ts(s.now())); err != nil {
		t.Fatal(err)
	}
	before, err := s.GetWorkItem(ctx, task.ID, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if before.Budget == nil || before.Budget.Estimate != nil || before.Budget.ActualState != "not measured" || before.Budget.ActualTokens != "0" || before.Budget.Ratio != "" {
		t.Fatalf("budget before any estimate: %+v", before.Budget)
	}
	snapshot := func() string {
		revisions, err := s.ListWorkItemRevisions(ctx, task.ID, item.ID, 0, api.DefaultWorkItemHistoryPage)
		if err != nil {
			t.Fatal(err)
		}
		encoded, _ := json.Marshal(revisions)
		return string(encoded) +
			tableDump(t, s, `SELECT * FROM work_item_revisions WHERE item_id=? ORDER BY revision`, item.ID) +
			tableDump(t, s, `SELECT * FROM work_item_changes WHERE item_id=? ORDER BY seq`, item.ID) +
			tableDump(t, s, `SELECT * FROM agent_work_item_bindings WHERE item_id=?`, item.ID) +
			tableDump(t, s, `SELECT * FROM work_order_scope_confirmations WHERE item_id=?`, item.ID) +
			tableDump(t, s, `SELECT `+workItemCols+` FROM work_items WHERE id=?`, item.ID)
	}
	frozen := snapshot()
	basis := "Small, 2 paths, median of 8 Small items"
	result, replay, err := s.CreateWorkItemUpdate(ctx, task.ID, item.ID, estimateRequest(before.Revision, "estimate-1", 12000000, basis, handler), by)
	if err != nil || replay {
		t.Fatalf("handler estimate save: %+v replay=%v err=%v", result, replay, err)
	}
	if result.Receipt.ResultRevision != before.Revision || result.Revision.Revision != before.Revision || result.Estimate == nil || result.Estimate.Tokens != 12000000 || result.Estimate.Basis != basis {
		t.Fatalf("estimate result: %+v", result)
	}
	after, err := s.GetWorkItem(ctx, task.ID, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	est := after.Budget.Estimate
	if est == nil || est.Tokens != 12000000 || est.Basis != basis || est.SetAt.IsZero() || est.SetBy.AgentID != handler.ID || est.SetBy.User != by.User {
		t.Fatalf("saved estimate: %+v", est)
	}
	if after.Revision != before.Revision || after.ScopeRevision != before.ScopeRevision || !after.UpdatedAt.Equal(before.UpdatedAt) || after.UpdatedBy != before.UpdatedBy {
		t.Fatalf("estimate save changed the item: before %+v after %+v", before, after)
	}
	if got := snapshot(); got != frozen {
		t.Fatalf("estimate save changed revision-bound records:\nbefore %s\nafter  %s", frozen, got)
	}
	binding, err := loadAgentWorkItemBinding(s.db, ctx, lead.ID, lead.RunID)
	if err != nil || binding == nil || binding.ContextDigest != strings.Repeat("d", 64) || binding.ItemRevision != after.Revision {
		t.Fatalf("bound agent's work context after the estimate: %+v %v", binding, err)
	}
	list, err := s.ListWorkItems(ctx, task.ID, "", "", 0, 0)
	if err != nil || len(list.Items) != 1 || list.Items[0].Budget == nil || list.Items[0].Budget.Estimate == nil || list.Items[0].Budget.Estimate.Tokens != 12000000 {
		t.Fatalf("list budget: %+v %v", list, err)
	}
	events, err := s.ListEvents(ctx, task.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	last := events[len(events)-1]
	data, _ := json.Marshal(last.Data)
	if last.Kind != "work_item_updated" || !strings.Contains(string(data), `"fields":["estimate"]`) || !strings.Contains(string(data), `"revision":2`) {
		t.Fatalf("estimate event: %+v", last)
	}
}

// a3: refusals store nothing; the owner succeeds; zero clears; each accepted
// save adds one history row; a delayed replay returns its own estimate.
func TestEstimateSaveRefusalsClearAndDelayedReplay(t *testing.T) {
	s, ctx, by := workItemStore(t)
	task, lead := workItemProject(t, s, ctx, by, "Estimates", "lead")
	handler := estimateHandler(t, s, ctx, by, task)
	steward, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{Role: api.AgentRoleBacklogSteward, AgentID: api.NewID("agt"), Name: "backlog-steward", Host: "host", Session: "tt-steward-fixture", Runtime: "claude", Cwd: "/tmp"}, by)
	if err != nil {
		t.Fatal(err)
	}
	item := createWorkItem(t, s, ctx, by, task, "create")
	// An update saved before estimates existed keeps its payload hash.
	priority := "high"
	old := api.CreateWorkItemUpdate{ExpectedRevision: 1, Priority: &priority, AgentID: lead.ID, RequestID: "old-update"}
	first, _, err := s.CreateWorkItemUpdate(ctx, task.ID, item.ID, old, by)
	if err != nil {
		t.Fatal(err)
	}
	var oldHash string
	if err = s.db.QueryRow(`SELECT payload_hash FROM work_item_update_requests WHERE receipt_id=?`, first.Receipt.ID).Scan(&oldHash); err != nil {
		t.Fatal(err)
	}
	// The hash of the request as the base commit's struct encoded it.
	type baseUpdate struct {
		ExpectedRevision int64                        `json:"expectedRevision"`
		Title            *string                      `json:"title,omitempty"`
		Description      *string                      `json:"description,omitempty"`
		Status           *string                      `json:"status,omitempty"`
		Priority         *string                      `json:"priority,omitempty"`
		AgentID          string                       `json:"agentId,omitempty"`
		RunID            string                       `json:"runId,omitempty"`
		RequestID        string                       `json:"requestId"`
		CompletionReport *api.NarrativeReportPin      `json:"completionReport,omitempty"`
		QueueAcceptance  *api.WorkItemQueueAcceptance `json:"queueAcceptance,omitempty"`
	}
	baseHash := requestHash(struct {
		ItemID  string     `json:"itemId"`
		Request baseUpdate `json:"request"`
	}{item.ID, baseUpdate{ExpectedRevision: 1, Priority: &priority, AgentID: lead.ID, RequestID: "old-update"}})
	if oldHash != baseHash {
		t.Fatalf("an update without estimate fields hashes differently from the base commit: %s vs %s", oldHash, baseHash)
	}
	const revision = 2
	frozen := tableDump(t, s, `SELECT * FROM work_items WHERE id=?`, item.ID) + tableDump(t, s, `SELECT * FROM work_item_update_requests ORDER BY receipt_id`)
	title, status, body, basis := "Other", "blocked", "body", "Small, 2 paths"
	mixed := func(change func(*api.CreateWorkItemUpdate)) api.CreateWorkItemUpdate {
		req := estimateRequest(revision, api.NewID("req"), 5, basis, handler)
		change(&req)
		return req
	}
	negative, huge := int64(-1), api.MaxEstimateTokens+1
	for name, c := range map[string]struct {
		req  api.CreateWorkItemUpdate
		want error
		text string
	}{
		"non-handler agent":     {estimateRequest(revision, "r1", 5, basis, lead), api.ErrConflict, "only the database handler sets an estimate"},
		"steward":               {estimateRequest(revision, "r2", 5, basis, steward), nil, "database handler"},
		"negative tokens":       {mixed(func(r *api.CreateWorkItemUpdate) { r.EstimateTokens = &negative }), api.ErrInvalid, ""},
		"tokens above the cap":  {mixed(func(r *api.CreateWorkItemUpdate) { r.EstimateTokens = &huge }), api.ErrInvalid, ""},
		"tokens without basis":  {estimateRequest(revision, "r5", 5, "", handler), api.ErrInvalid, ""},
		"clear with a basis":    {estimateRequest(revision, "r6", 0, basis, handler), api.ErrInvalid, ""},
		"basis without tokens":  {api.CreateWorkItemUpdate{ExpectedRevision: revision, RequestID: "r7", AgentID: handler.ID, EstimateBasis: &basis}, api.ErrInvalid, ""},
		"two-line basis":        {estimateRequest(revision, "r8", 5, "Small\n2 paths", handler), api.ErrInvalid, ""},
		"basis over 240 bytes":  {estimateRequest(revision, "r9", 5, strings.Repeat("b", api.MaxEstimateBasisBytes+1), handler), api.ErrInvalid, ""},
		"mixed with title":      {mixed(func(r *api.CreateWorkItemUpdate) { r.Title = &title }), api.ErrInvalid, ""},
		"mixed with body":       {mixed(func(r *api.CreateWorkItemUpdate) { r.Description = &body }), api.ErrInvalid, ""},
		"mixed with status":     {mixed(func(r *api.CreateWorkItemUpdate) { r.Status = &status }), api.ErrInvalid, ""},
		"mixed with priority":   {mixed(func(r *api.CreateWorkItemUpdate) { r.Priority = &priority }), api.ErrInvalid, ""},
		"stale revision":        {estimateRequest(revision-1, "r14", 5, basis, handler), api.ErrConflict, "revision changed"},
		"future revision":       {estimateRequest(revision+1, "r15", 5, basis, handler), api.ErrConflict, "revision changed"},
		"another handler's run": {mixed(func(r *api.CreateWorkItemUpdate) { r.RunID = "run_0000000000000000" }), api.ErrConflict, "agent run changed"},
	} {
		_, _, err := s.CreateWorkItemUpdate(ctx, task.ID, item.ID, c.req, by)
		if err == nil || (c.want != nil && !errors.Is(err, c.want)) || !strings.Contains(err.Error(), c.text) {
			t.Fatalf("%s: err = %v, want %v containing %q", name, err, c.want, c.text)
		}
		var refusal *api.StewardRefusal
		if name == "steward" && !errors.As(err, &refusal) {
			t.Fatalf("steward refusal lost its code: %v", err)
		}
	}
	// The legacy PATCH route has no estimate field and still needs a change.
	if _, err = s.UpdateWorkItem(ctx, task.ID, item.ID, api.UpdateWorkItemRequest{Revision: revision, AgentID: handler.ID}, by); !errors.Is(err, api.ErrInvalid) {
		t.Fatalf("empty legacy update = %v", err)
	}
	if got := tableDump(t, s, `SELECT * FROM work_items WHERE id=?`, item.ID) + tableDump(t, s, `SELECT * FROM work_item_update_requests ORDER BY receipt_id`); got != frozen || estimateHistoryRows(t, s, item) != 0 {
		t.Fatalf("a refused estimate stored something:\nbefore %s\nafter  %s", frozen, got)
	}

	// Save A by the handler, then B by the owner with no agent, both at the
	// same item revision.
	reqA := estimateRequest(revision, "KA", 12000000, "Small, 2 paths, median of 8 Small items", handler)
	a, _, err := s.CreateWorkItemUpdate(ctx, task.ID, item.ID, reqA, by)
	if err != nil {
		t.Fatal(err)
	}
	reqB := estimateRequest(revision, "KB", 30000000, "Planned, 9 paths", api.Agent{})
	b, _, err := s.CreateWorkItemUpdate(ctx, task.ID, item.ID, reqB, by)
	if err != nil || b.Estimate == nil || b.Estimate.SetBy.AgentID != "" || b.Estimate.SetBy.User != by.User {
		t.Fatalf("owner estimate save: %+v %v", b, err)
	}
	checkA := func(stage string) {
		t.Helper()
		again, replay, err := s.CreateWorkItemUpdate(ctx, task.ID, item.ID, reqA, by)
		if err != nil || !replay || again.Receipt.ID != a.Receipt.ID || again.Estimate == nil || again.Estimate.Tokens != 12000000 || again.Estimate.Basis != "Small, 2 paths, median of 8 Small items" || !again.Estimate.SetAt.Equal(a.Estimate.SetAt) {
			t.Fatalf("%s: delayed replay of A = %+v replay=%v err=%v", stage, again, replay, err)
		}
		read, err := s.GetWorkItemUpdateReceipt(ctx, task.ID, item.ID, "KA", handler.ID, by)
		if err != nil || read.Receipt.ID != a.Receipt.ID || read.Estimate == nil || read.Estimate.Tokens != 12000000 || read.Estimate.SetBy.AgentID != handler.ID {
			t.Fatalf("%s: receipt read of A = %+v %v", stage, read, err)
		}
	}
	checkA("after B")
	current, err := s.GetWorkItem(ctx, task.ID, item.ID)
	if err != nil || current.Budget.Estimate == nil || current.Budget.Estimate.Tokens != 30000000 || current.Budget.Estimate.Basis != "Planned, 9 paths" || current.Revision != revision {
		t.Fatalf("item after B: %+v %v", current, err)
	}
	if n := estimateHistoryRows(t, s, item); n != 2 {
		t.Fatalf("history rows after A, B and a replay = %d, want 2", n)
	}
	changed := estimateRequest(revision, "KA", 13000000, "Small, 2 paths, median of 8 Small items", handler)
	if _, _, err = s.CreateWorkItemUpdate(ctx, task.ID, item.ID, changed, by); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "different update data") {
		t.Fatalf("KA with a different payload = %v", err)
	}

	// A clear is a save too: the item shows no estimate, A still replays.
	cleared, _, err := s.CreateWorkItemUpdate(ctx, task.ID, item.ID, estimateRequest(revision, "KC", 0, "", handler), by)
	if err != nil || cleared.Estimate == nil || cleared.Estimate.Tokens != 0 || cleared.Estimate.Basis != "" {
		t.Fatalf("clear: %+v %v", cleared, err)
	}
	current, err = s.GetWorkItem(ctx, task.ID, item.ID)
	if err != nil || current.Budget == nil || current.Budget.Estimate != nil || current.Revision != revision {
		t.Fatalf("item after the clear: %+v %v", current.Budget, err)
	}
	checkA("after a clear")
	if n := estimateHistoryRows(t, s, item); n != 3 {
		t.Fatalf("history rows after the clear = %d, want 3", n)
	}
	// The update saved before estimates replays its own receipt, with none.
	again, replay, err := s.CreateWorkItemUpdate(ctx, task.ID, item.ID, old, by)
	if err != nil || !replay || again.Receipt.ID != first.Receipt.ID || again.Estimate != nil || again.Revision.Revision != revision {
		t.Fatalf("pre-estimate update replay = %+v replay=%v err=%v", again, replay, err)
	}
	// A later real change advances the revision and keeps the estimate state.
	if _, _, err = s.CreateWorkItemUpdate(ctx, task.ID, item.ID, estimateRequest(revision, "KD", 7, "Small, 1 path", handler), by); err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.CreateWorkItemUpdate(ctx, task.ID, item.ID, api.CreateWorkItemUpdate{ExpectedRevision: revision, Title: &title, AgentID: lead.ID, RequestID: "retitle"}, by); err != nil {
		t.Fatal(err)
	}
	current, err = s.GetWorkItem(ctx, task.ID, item.ID)
	if err != nil || current.Revision != revision+1 || current.Budget.Estimate == nil || current.Budget.Estimate.Tokens != 7 {
		t.Fatalf("estimate after a later revision: %+v %v", current, err)
	}
}

// a2: an estimate saved on an item with a queued entry, and on a done item
// with an accepted, unreleased entry, leaves both unblocked and releasable.
func TestEstimateSaveLeavesQueuedAndAcceptedEntries(t *testing.T) {
	t.Run("queued", func(t *testing.T) {
		f := newRebindFixture(t, false)
		if _, _, err := f.s.CreateWorkItemUpdate(f.ctx, f.task.ID, f.item.ID, estimateRequest(f.item.Revision, "estimate", 9000000, "Planned, 1 path", f.handler), f.by); err != nil {
			t.Fatal(err)
		}
		q := listedEntry(t, f.s, f.task.ID, f.entry.ID)
		if q.State != "queued" || q.Revision != f.entry.Revision || q.ItemRevision != f.item.Revision || strings.Contains(q.BlockReason, "Item is at revision") {
			t.Fatalf("queued entry after an estimate save: %+v", q)
		}
		if q.Budget == nil || q.Budget.Estimate == nil || q.Budget.Estimate.Tokens != 9000000 {
			t.Fatalf("queued entry budget: %+v", q.Budget)
		}
		claimed, err := f.s.TeamQueueAction(f.ctx, f.task.ID, api.TeamQueueRequest{RequestID: api.NewID("req"), Operation: "claim", EntryID: q.ID, ExpectedRevision: q.Revision, Host: "mini"})
		if err != nil || claimed.State != "launching" {
			t.Fatalf("claim after an estimate save: %+v %v", claimed, err)
		}
	})
	t.Run("accepted and unreleased", func(t *testing.T) {
		f := newAutoAcceptFixture(t, true)
		res, err := f.save("done-save", f.tuple(candidateB))
		if err != nil {
			t.Fatal(err)
		}
		gate := func() error {
			tx, err := f.s.db.BeginTx(f.ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			_, _, _, err = releaseCandidate(f.ctx, tx, f.task.ID, f.entry)
			return err
		}
		if err = gate(); err != nil {
			t.Fatal("release gate before the estimate", err)
		}
		before := f.queueEntry(t)
		jobs := tableDump(t, f.s, `SELECT * FROM release_jobs WHERE task_id=?`, f.task.ID)
		for i, tokens := range []int64{40000000, 0, 41000000} {
			basis := "Planned, 14 paths"
			if tokens == 0 {
				basis = ""
			}
			if _, _, err = f.s.CreateWorkItemUpdate(f.ctx, f.task.ID, f.item.ID, estimateRequest(res.Revision.Revision, "estimate-"+string(rune('a'+i)), tokens, basis, f.handler), f.by); err != nil {
				t.Fatal(err)
			}
		}
		item, after := f.current(t), f.queueEntry(t)
		if item.Revision != res.Revision.Revision || after.Acceptance == nil || after.Acceptance.ItemRevision != item.Revision || after.Revision != before.Revision || strings.Contains(after.BlockReason, "Item is at revision") {
			t.Fatalf("accepted entry after estimate saves: item %+v entry %+v", item, after)
		}
		if err = gate(); err != nil {
			t.Fatal("release gate after the estimate", err)
		}
		if got := tableDump(t, f.s, `SELECT * FROM release_jobs WHERE task_id=?`, f.task.ID); got != jobs || f.releaseJobs(t) != 1 {
			t.Fatalf("estimate save changed release jobs:\n%s\n%s", jobs, got)
		}
		if after.Budget == nil || after.Budget.Estimate == nil || after.Budget.Estimate.Tokens != 41000000 {
			t.Fatalf("accepted entry budget: %+v", after.Budget)
		}
	})
}

// summaryTokenTotal is the sum of the token classes of a usage summary.
func summaryTokenTotal(s api.UsageSummary) string {
	total := new(big.Rat)
	for _, class := range api.UsageClasses {
		if v, ok := s.Tokens[class]; ok {
			total.Add(total, usageRat(v))
		}
	}
	return total.RatString()
}

// assertBudgetsMatchReport compares every item's budget, on get and on the
// report, with its unfiltered usage summary.
func assertBudgetsMatchReport(t *testing.T, s *Store, task api.Task, items []api.WorkItem, stage string) map[string]*api.TokenBudget {
	t.Helper()
	ctx := context.Background()
	out := map[string]*api.TokenBudget{}
	for _, item := range items {
		report, err := s.Usage(ctx, task.ID, api.UsageQuery{Item: item.ID})
		if err != nil || len(report.Items) != 1 {
			t.Fatalf("%s: usage report: %+v %v", stage, report, err)
		}
		summary := report.Items[0].Summary
		got, err := s.GetWorkItem(ctx, task.ID, item.ID)
		if err != nil {
			t.Fatal(err)
		}
		for surface, b := range map[string]*api.TokenBudget{"get": got.Budget, "report": report.Items[0].Budget} {
			if b == nil || b.ActualTokens != summaryTokenTotal(summary) || b.ActualState != summary.State {
				t.Fatalf("%s: %s budget of %s = %+v, want %s %s", stage, surface, item.Title, b, summaryTokenTotal(summary), summary.State)
			}
		}
		out[item.ID] = got.Budget
	}
	return out
}

// a5, a7 (reads): the budget equals the unfiltered usage summary across a
// shared request, a closed run, a revised turn and a partial turn; the From/To
// filter leaves it alone; share rows rebuild; reads never touch usage_turns.
func TestTokenBudgetMatchesUsageReport(t *testing.T) {
	path := filepath.Join(t.TempDir(), "budget.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	ctx := context.Background()
	by := api.Caller{Node: "fixture", User: "owner"}
	task, err := s.CreateTask(ctx, api.CreateTaskRequest{Name: "Synthetic budget only", Swarm: true}, by)
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "shared", AgentID: api.NewID("agt"), Runtime: "codex", Role: api.AgentRoleDatabaseHandler, Host: "fixture", Session: "synthetic"}, by)
	if err != nil {
		t.Fatal(err)
	}
	items, handled := []api.WorkItem{}, []api.UsageEvidence{}
	at := time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)
	for i, title := range []string{"Item zero", "Item one", "Item never used"} {
		item, err := s.CreateWorkItem(ctx, task.ID, api.CreateWorkItemRequest{Kind: "bug", Title: title, RequestID: fmt.Sprintf("create-%d", i)}, by)
		if err != nil {
			t.Fatal(err)
		}
		items = append(items, item)
		if i == 2 {
			break
		}
		m, err := s.PostMessage(ctx, task.ID, api.PostMessageRequest{To: a.ID, RequestID: fmt.Sprintf("order-%d", i), WorkItems: []api.MessageWorkItem{{ItemTaskID: task.ID, ItemID: item.ID, ItemRevision: item.Revision, Relationship: "primary"}}, Envelope: &api.Envelope{Kind: "request", To: a.ID, Subject: "Handle synthetic evidence", Body: api.EnvelopeBody{Ask: "Record synthetic data"}}}, by)
		if err != nil {
			t.Fatal(err)
		}
		handled = append(handled, api.UsageEvidence{TaskID: task.ID, Seq: m.Seq, Operation: "ack", At: at})
	}
	turn := func(id string, evidence ...api.UsageEvidence) api.UsageTurn {
		x := syntheticUsageTurn(id)
		x.Handled = evidence
		return x
	}
	shared := turn("shared", handled...)   // half to each item
	zero := turn("zero-only", handled[0])  // revised below
	one := turn("one-partial", handled[1]) // partial, completed below
	one.Complete = false
	overhead := turn("overhead")
	if _, err = s.ReportUsage(ctx, task.ID, a.ID, usageBatch(a, "batch-1", shared, zero, one, overhead)); err != nil {
		t.Fatal(err)
	}
	budgets := assertBudgetsMatchReport(t, s, task, items, "first ingest")
	// 110 tokens per synthetic turn: item zero has a half share and a whole.
	if b := budgets[items[0].ID]; b.ActualTokens != "165" || b.ActualState != "measured" {
		t.Fatalf("item zero budget: %+v", b)
	}
	if b := budgets[items[1].ID]; b.ActualTokens != "165" || b.ActualState != "partial" {
		t.Fatalf("item one budget with a partial turn: %+v", b)
	}
	if b := budgets[items[2].ID]; b.ActualTokens != "0" || b.ActualState != "not measured" || b.Ratio != "" {
		t.Fatalf("unused item budget: %+v", b)
	}

	// An estimate gives a ratio; a partial actual keeps its state beside it.
	for i, item := range items {
		if _, _, err = s.CreateWorkItemUpdate(ctx, task.ID, item.ID, estimateRequest(item.Revision, "estimate-"+item.ID, 100, "Small, 1 path", api.Agent{}), by); err != nil {
			t.Fatal(i, err)
		}
	}
	budgets = assertBudgetsMatchReport(t, s, task, items, "with estimates")
	if b := budgets[items[0].ID]; b.Ratio != "33/20" {
		t.Fatalf("ratio of 165 over 100 = %q", b.Ratio)
	}
	if b := budgets[items[1].ID]; b.Ratio != "33/20" || b.ActualState != "partial" {
		t.Fatalf("partial item ratio: %+v", b)
	}
	if b := budgets[items[2].ID]; b.Ratio != "" || b.Estimate == nil {
		t.Fatalf("unmeasured item has a ratio: %+v", b)
	}

	// A revised turn replaces its share rows instead of adding to them.
	zero2 := zero
	zero2.Revision, zero2.Raw, zero2.Tokens = 2, map[string]int64{"input_tokens": 201, "cached_input_tokens": 80, "cache_write_input_tokens": 0, "output_tokens": 9, "reasoning_output_tokens": 2}, map[string]int64{"input": 121, "cached": 80, "cacheWrite": 0, "output": 7, "reasoning": 2}
	if _, err = s.ReportUsage(ctx, task.ID, a.ID, usageBatch(a, "batch-2", zero2)); err != nil {
		t.Fatal(err)
	}
	budgets = assertBudgetsMatchReport(t, s, task, items, "revised turn")
	if b := budgets[items[0].ID]; b.ActualTokens != "265" {
		t.Fatalf("item zero after a revision: %+v", b)
	}
	// The From/To filter changes the report rows, never the budget.
	filtered, err := s.Usage(ctx, task.ID, api.UsageQuery{From: at.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range filtered.Items {
		if row.Summary.State != "not measured" || row.Budget == nil || row.Budget.ActualTokens != budgets[row.ItemID].ActualTokens || row.Budget.ActualState != budgets[row.ItemID].ActualState {
			t.Fatalf("filtered report row %s: summary %+v budget %+v", row.Title, row.Summary, row.Budget)
		}
	}
	if filtered.Overhead.Budget != nil {
		t.Fatalf("project overhead has a budget: %+v", filtered.Overhead.Budget)
	}

	// Closing the run keeps every actual; a closed run still ingests.
	if _, err = s.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "spare", AgentID: api.NewID("agt"), Runtime: "codex", Role: api.AgentRoleDatabaseHandler, Host: "fixture", Session: "spare"}, by); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CloseAgent(ctx, a.ID, by); err != nil {
		t.Fatal(err)
	}
	closed := assertBudgetsMatchReport(t, s, task, items, "closed run")
	for id, b := range budgets {
		if closed[id].ActualTokens != b.ActualTokens || closed[id].ActualState != b.ActualState {
			t.Fatalf("closing the run changed %s: %+v to %+v", id, b, closed[id])
		}
	}
	one2 := one
	one2.Revision, one2.Complete = 2, true
	if _, err = s.ReportUsage(ctx, task.ID, a.ID, usageBatch(a, "batch-3", one2, turn("late", handled[1]))); err != nil {
		t.Fatal(err)
	}
	budgets = assertBudgetsMatchReport(t, s, task, items, "partial turn completed")
	if b := budgets[items[1].ID]; b.ActualTokens != "275" || b.ActualState != "measured" || b.Ratio != "11/4" {
		t.Fatalf("item one after its partial turn completed: %+v", b)
	}

	// A turn missing a token class is partial like an incomplete one.
	gap := turn("gap", handled[0])
	delete(gap.Raw, "reasoning_output_tokens")
	gap.Tokens, gap.Gap = api.NormalizeUsageTokens("codex", gap.Raw)
	if _, err = s.ReportUsage(ctx, task.ID, a.ID, usageBatch(a, "batch-4", gap)); err != nil {
		t.Fatal(err)
	}
	budgets = assertBudgetsMatchReport(t, s, task, items, "missing class")
	if b := budgets[items[0].ID]; b.ActualState != "partial" || b.ActualTokens != "366" {
		t.Fatalf("item zero with a missing class: %+v", b)
	}

	// Share rows rebuild at open: deleted rows, and a turn an older binary
	// revised without touching its shares.
	shares := func() string {
		return tableDump(t, s, `SELECT * FROM usage_item_shares ORDER BY task_id,agent_id,run_id,request_id,item_task_id,item_id`)
	}
	want := shares()
	if _, err = s.db.Exec(`DELETE FROM usage_item_shares WHERE request_id IN ('shared','gap')`); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`UPDATE usage_item_shares SET turn_revision=1,tokens=1 WHERE request_id='zero-only'`); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	if s, err = Open(path); err != nil {
		t.Fatal(err)
	}
	if got := shares(); got != want {
		t.Fatalf("share rows after a rebuild:\nwant %s\ngot  %s", want, got)
	}
	rebuilt := assertBudgetsMatchReport(t, s, task, items, "rebuilt shares")

	// Budget reads use the share table: they survive without usage_turns.
	if _, err = s.db.Exec(`DELETE FROM usage_turns`); err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		got, err := s.GetWorkItem(ctx, task.ID, item.ID)
		if err != nil || got.Budget.ActualTokens != rebuilt[item.ID].ActualTokens || got.Budget.ActualState != rebuilt[item.ID].ActualState {
			t.Fatalf("budget read needed usage_turns: %+v %v", got.Budget, err)
		}
	}
	rows, err := s.db.Query(`EXPLAIN QUERY PLAN SELECT denominator,sum(tokens),max(partial) FROM usage_item_shares WHERE item_task_id=? AND item_id=? AND task_id=? GROUP BY denominator`, task.ID, items[0].ID, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	indexed := false
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err = rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		indexed = indexed || strings.Contains(detail, "USING COVERING INDEX usage_item_shares_item")
	}
	if !indexed {
		t.Fatal("the budget read does not use the item index")
	}
}

// a7: every queue list form carries the budget, across a history page limit.
func TestTeamQueueListFormsCarryBudget(t *testing.T) {
	f := newRebindFixture(t, false)
	s, ctx := f.s, f.ctx
	if _, _, err := s.CreateWorkItemUpdate(ctx, f.task.ID, f.other.ID, estimateRequest(f.other.Revision, "estimate", 20000000, "Small, 2 paths", f.handler), f.by); err != nil {
		t.Fatal(err)
	}
	// Lifetime usage of the history item: one whole partial share and a half.
	for i, share := range []struct{ denominator, tokens, partial int }{{1, 25000000, 1}, {2, 1000001, 0}} {
		if _, err := s.db.Exec(`INSERT INTO usage_item_shares(task_id,agent_id,run_id,request_id,turn_revision,item_task_id,item_id,denominator,tokens,partial) VALUES(?,?,?,?,1,?,?,?,?,?)`,
			f.task.ID, f.handler.ID, f.handler.RunID, "turn-"+string(rune('a'+i)), f.task.ID, f.other.ID, share.denominator, share.tokens, share.partial); err != nil {
			t.Fatal(err)
		}
	}
	// Another project's turn naming this item is not this project's usage.
	if _, err := s.db.Exec(`INSERT INTO usage_item_shares(task_id,agent_id,run_id,request_id,turn_revision,item_task_id,item_id,denominator,tokens,partial) VALUES('tsk_0000000000000000',?,?,'foreign',1,?,?,1,999,0)`, f.handler.ID, f.handler.RunID, f.task.ID, f.other.ID); err != nil {
		t.Fatal(err)
	}
	const history = api.DefaultTeamQueueHistoryLimit + 5
	now := ts(s.now())
	for i := 0; i < history; i++ {
		if _, err := s.db.Exec(`INSERT INTO team_queue_entries(id,task_id,item_id,item_revision,order_seq,template,position,attempt,state,revision,created_at,updated_at,repository,base_commit,host) VALUES(?,?,?,?,?,'small',?,?,'finished',1,?,?,?,?,'mini')`,
			api.NewID("tqe"), f.task.ID, f.other.ID, f.other.Revision, f.otherOrder.Seq, 100+i, i+1, now, now, rebindRepository, strings.Repeat("a", 40)); err != nil {
			t.Fatal(err)
		}
	}
	check := func(form string, q api.TeamQueueEntry) {
		t.Helper()
		b := q.Budget
		if b == nil {
			t.Fatalf("%s: entry %s at position %d has no budget", form, q.ID, q.Position)
		}
		if q.ItemID == f.other.ID {
			if b.Estimate == nil || b.Estimate.Tokens != 20000000 || b.ActualTokens != "51000001/2" || b.ActualState != "partial" || b.Ratio != "51000001/40000000" {
				t.Fatalf("%s: history item budget %+v", form, b)
			}
		} else if b.Estimate != nil || b.ActualState != "not measured" || b.ActualTokens != "0" {
			t.Fatalf("%s: queued item budget %+v", form, b)
		}
	}
	page, err := s.TeamQueuePage(ctx, f.task.ID, api.TeamQueueListOptions{})
	if err != nil || len(page.Entries) != 1+api.DefaultTeamQueueHistoryLimit || page.History == nil || page.History.NextAfter == 0 {
		t.Fatalf("default page: %d entries %+v %v", len(page.Entries), page.History, err)
	}
	summaries := 0
	for _, q := range page.Entries {
		check("default", q)
		if q.Summary {
			summaries++
		}
	}
	if summaries != api.DefaultTeamQueueHistoryLimit {
		t.Fatalf("default page summaries = %d", summaries)
	}
	second, err := s.TeamQueuePage(ctx, f.task.ID, api.TeamQueueListOptions{After: page.History.NextAfter})
	if err != nil || len(second.Entries) != 1+5 || second.History.NextAfter != 0 {
		t.Fatalf("second history page: %d entries %+v %v", len(second.Entries), second.History, err)
	}
	for _, q := range second.Entries {
		check("second page", q)
	}
	active, err := s.TeamQueuePage(ctx, f.task.ID, api.TeamQueueListOptions{View: api.TeamQueueViewActive})
	if err != nil || len(active.Entries) != 1 {
		t.Fatalf("active view: %+v %v", active, err)
	}
	check("active", active.Entries[0])
	attempts, err := s.TeamQueuePage(ctx, f.task.ID, api.TeamQueueListOptions{Item: f.other.ID})
	if err != nil || len(attempts.Entries) != history {
		t.Fatalf("item attempts: %d %v", len(attempts.Entries), err)
	}
	for _, q := range attempts.Entries {
		check("item attempts", q)
	}
	live, err := s.TeamQueuePage(ctx, f.task.ID, api.TeamQueueListOptions{Item: f.item.ID})
	if err != nil || len(live.Entries) != 1 {
		t.Fatalf("live item attempt: %+v %v", live, err)
	}
	check("live item attempt", live.Entries[0])
	one, err := s.GetTeamQueueEntry(ctx, f.task.ID, attempts.Entries[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	check("entry get", one)
	// A queue action returns its entry as the entry get would.
	check("action result", f.entry)
	if same, err := s.GetTeamQueueEntry(ctx, f.task.ID, f.entry.ID); err != nil || !reflect.DeepEqual(same, f.entry) {
		t.Fatalf("entry get differs from the add result:\n%+v\n%+v %v", same, f.entry, err)
	}
	hosted, err := s.TeamQueuesByHost(ctx, "mini")
	if err != nil || len(hosted.Entries) != 1 {
		t.Fatalf("by host: %+v %v", hosted, err)
	}
	check("by host", hosted.Entries[0])
	legacy, err := s.ListTeamQueue(ctx, f.task.ID)
	if err != nil || len(legacy.Entries) != 1+history {
		t.Fatalf("full listing: %d %v", len(legacy.Entries), err)
	}
	for _, q := range legacy.Entries {
		check("full listing", q)
	}
}

// a4: a database with the base commit's schema (no estimate columns, no
// estimate history, no share rows) opens with this binary: items read "no
// estimate", actuals match the report, and a second open changes nothing.
func TestTokenBudgetMigratesBaseSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "base.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	ctx := context.Background()
	by := api.Caller{Node: "fixture", User: "owner"}
	task, err := s.CreateTask(ctx, api.CreateTaskRequest{Name: "Base schema", Swarm: true}, by)
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "shared", AgentID: api.NewID("agt"), Runtime: "codex", Role: api.AgentRoleDatabaseHandler, Host: "fixture", Session: "synthetic"}, by)
	if err != nil {
		t.Fatal(err)
	}
	item, err := s.CreateWorkItem(ctx, task.ID, api.CreateWorkItemRequest{Kind: "bug", Title: "Measured before the estimate", RequestID: "create"}, by)
	if err != nil {
		t.Fatal(err)
	}
	m, err := s.PostMessage(ctx, task.ID, api.PostMessageRequest{To: a.ID, RequestID: "order", WorkItems: []api.MessageWorkItem{{ItemTaskID: task.ID, ItemID: item.ID, ItemRevision: item.Revision, Relationship: "primary"}}, Envelope: &api.Envelope{Kind: "request", To: a.ID, Subject: "Handle synthetic evidence", Body: api.EnvelopeBody{Ask: "Record synthetic data"}}}, by)
	if err != nil {
		t.Fatal(err)
	}
	evidence := []api.UsageEvidence{{TaskID: task.ID, Seq: m.Seq, Operation: "ack", At: time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)}}
	whole, partial := syntheticUsageTurn("whole"), syntheticUsageTurn("partial")
	whole.Handled, partial.Handled, partial.Complete = evidence, evidence, false
	if _, err = s.ReportUsage(ctx, task.ID, a.ID, usageBatch(a, "batch", whole, partial, syntheticUsageTurn("overhead"))); err != nil {
		t.Fatal(err)
	}
	// Return the file to the base schema.
	for _, statement := range []string{`DROP TABLE usage_item_shares`, `DROP TABLE work_item_estimates`,
		`ALTER TABLE work_items DROP COLUMN estimate_tokens`, `ALTER TABLE work_items DROP COLUMN estimate_basis`, `ALTER TABLE work_items DROP COLUMN estimate_set_at`,
		`ALTER TABLE work_items DROP COLUMN estimate_agent`, `ALTER TABLE work_items DROP COLUMN estimate_node`, `ALTER TABLE work_items DROP COLUMN estimate_user`} {
		if _, err = s.db.Exec(statement); err != nil {
			t.Fatal(statement, err)
		}
	}
	reopen := func() {
		t.Helper()
		if err = s.Close(); err != nil {
			t.Fatal(err)
		}
		if s, err = Open(path); err != nil {
			t.Fatal(err)
		}
	}
	reopen()
	got, err := s.GetWorkItem(ctx, task.ID, item.ID)
	if err != nil || got.Budget == nil || got.Budget.Estimate != nil || got.Budget.ActualTokens != "220" || got.Budget.ActualState != "partial" || got.Revision != item.Revision {
		t.Fatalf("item after migration: %+v %v", got.Budget, err)
	}
	assertBudgetsMatchReport(t, s, task, []api.WorkItem{item}, "migrated")
	var integrity string
	if err = s.db.QueryRow(`PRAGMA integrity_check`).Scan(&integrity); err != nil || integrity != "ok" {
		t.Fatalf("integrity_check = %q %v", integrity, err)
	}
	if violations := tableDump(t, s, `PRAGMA foreign_key_check`); violations != "" {
		t.Fatalf("foreign_key_check: %s", violations)
	}
	dump := func() string {
		return tableDump(t, s, `SELECT * FROM work_items ORDER BY seq`) + tableDump(t, s, `SELECT * FROM usage_item_shares ORDER BY task_id,agent_id,run_id,request_id,item_task_id,item_id`) +
			tableDump(t, s, `SELECT * FROM work_item_estimates`) + tableDump(t, s, `SELECT * FROM work_item_revisions ORDER BY item_id,revision`) + tableDump(t, s, `SELECT * FROM usage_turns ORDER BY request_id`)
	}
	first := dump()
	reopen()
	if second := dump(); second != first {
		t.Fatalf("a second open changed the database:\n%s\n%s", first, second)
	}
}

// releaseEntryItem returns the bug behind a releaseEntry queue entry.
func releaseEntryItem(t *testing.T, s *Store, task api.Task, entry string) api.WorkItem {
	t.Helper()
	var itemID string
	if err := s.db.QueryRow(`SELECT item_id FROM team_queue_entries WHERE id=?`, entry).Scan(&itemID); err != nil {
		t.Fatal(err)
	}
	item, err := s.GetWorkItem(context.Background(), task.ID, itemID)
	if err != nil {
		t.Fatal(err)
	}
	return item
}

// A title or description change is refused by both update routes while the
// item's accepted candidate awaits release, and the refusal names the job.
func TestScopeEditRefusedWhileAcceptedCandidateAwaitsRelease(t *testing.T) {
	s, task, h, d, entry := releaseFixture(t)
	ctx := context.Background()
	by := api.Caller{Node: "fixture", User: "owner"}
	item := releaseEntryItem(t, s, task, entry)
	appended, retitled := item.Description+"\nRecurrence: seen again", "Renamed after acceptance"
	refused := func(when string, wants ...string) {
		t.Helper()
		_, legacy := s.UpdateWorkItem(ctx, task.ID, item.ID, api.UpdateWorkItemRequest{Revision: item.Revision, Description: &appended}, by)
		_, _, keyed := s.CreateWorkItemUpdate(ctx, task.ID, item.ID, api.CreateWorkItemUpdate{ExpectedRevision: item.Revision, Title: &retitled, RequestID: "refused-" + when}, by)
		for route, err := range map[string]error{"PATCH": legacy, "keyed": keyed} {
			if !errors.Is(err, api.ErrConflict) {
				t.Fatalf("%s %s: err=%v, want a conflict", when, route, err)
			}
			for _, want := range append(wants, "--scope-change", "tt work-items note", entry) {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("%s %s: %q does not name %q", when, route, err, want)
				}
			}
		}
		got, err := s.GetWorkItem(ctx, task.ID, item.ID)
		if err != nil || got.Revision != item.Revision || got.ScopeRevision != item.ScopeRevision || got.Description != item.Description || got.Title != item.Title {
			t.Fatalf("%s: a refused edit changed the item: %+v err=%v", when, got, err)
		}
	}
	refused("no-job", "no release job yet")
	// The marker means nothing without a title or description.
	high := "high"
	if _, _, err := s.CreateWorkItemUpdate(ctx, task.ID, item.ID, api.CreateWorkItemUpdate{ExpectedRevision: item.Revision, Priority: &high, ScopeChange: true, RequestID: "marker-alone"}, by); !errors.Is(err, api.ErrInvalid) {
		t.Fatalf("scope change marker without title or description: %v", err)
	}
	if _, err := s.UpdateWorkItem(ctx, task.ID, item.ID, api.UpdateWorkItemRequest{Revision: item.Revision, Priority: &high, ScopeChange: true}, by); !errors.Is(err, api.ErrInvalid) {
		t.Fatalf("PATCH scope change marker without title or description: %v", err)
	}

	j, err := s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: "enqueue", Operation: "enqueue", AgentID: h.ID, RunID: h.RunID, EntryID: entry})
	if err != nil {
		t.Fatal(err)
	}
	refused("verified", j.ID, "state "+j.State)
	action := func(op string) api.ReleaseRequest {
		return api.ReleaseRequest{RequestID: op, Operation: op, AgentID: d.ID, RunID: d.RunID, JobID: j.ID, ExpectedGeneration: j.Generation}
	}
	if j, err = s.ReleaseAction(ctx, task.ID, action("claim")); err != nil {
		t.Fatal(err)
	}
	refused("claimed", j.ID, "state claimed")
	merged := action("merged")
	merged.IntegratedCommit = j.Commit
	if j, err = s.ReleaseAction(ctx, task.ID, merged); err != nil {
		t.Fatal(err)
	}
	finish := action("finish")
	finish.Receipt = &api.ReleaseReceipt{Version: 1, JobID: j.ID, Commit: j.Commit, VerificationDigest: j.VerificationDigest, Outcome: "released", Targets: []api.ReleaseTargetReceipt{{Target: "tailos", Release: "fixture", ArtifactSHA256: strings.Repeat("a", 64), Outcome: "released"}}}
	if j, err = s.ReleaseAction(ctx, task.ID, finish); err != nil || j.State != "released" {
		t.Fatalf("finish: state=%q err=%v", j.State, err)
	}
	saved, err := s.UpdateWorkItem(ctx, task.ID, item.ID, api.UpdateWorkItemRequest{Revision: item.Revision, Description: &appended}, by)
	if err != nil || saved.Description != appended || saved.ScopeRevision != item.ScopeRevision+1 {
		t.Fatalf("PATCH after release: %+v err=%v", saved, err)
	}
	if _, _, err = s.CreateWorkItemUpdate(ctx, task.ID, item.ID, api.CreateWorkItemUpdate{ExpectedRevision: saved.Revision, Title: &retitled, RequestID: "after-release"}, by); err != nil {
		t.Fatalf("keyed update after release: %v", err)
	}
	if got, err := s.GetWorkItem(ctx, task.ID, item.ID); err != nil || got.Title != retitled || got.ScopeRevision != saved.ScopeRevision+1 {
		t.Fatalf("keyed update after release: %+v err=%v", got, err)
	}

	// An item with no accepted queue entry is never refused, queued or not.
	plain, err := s.CreateWorkItem(ctx, task.ID, api.CreateWorkItemRequest{Kind: "bug", Title: "Unaccepted", RequestID: "plain"}, by)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`INSERT INTO team_queue_entries(id,task_id,item_id,item_revision,order_seq,template,position,state,created_at,updated_at,repository,base_commit) VALUES(?,?,?,?,1,'planned',2,'running',?,?, 'fixture',?)`, api.NewID("tqe"), task.ID, plain.ID, plain.Revision, ts(time.Now()), ts(time.Now()), candidateA); err != nil {
		t.Fatal(err)
	}
	if plain, err = s.UpdateWorkItem(ctx, task.ID, plain.ID, api.UpdateWorkItemRequest{Revision: plain.Revision, Description: &appended}, by); err != nil {
		t.Fatalf("PATCH on an unaccepted item: %v", err)
	}
	if _, _, err = s.CreateWorkItemUpdate(ctx, task.ID, plain.ID, api.CreateWorkItemUpdate{ExpectedRevision: plain.Revision, Title: &retitled, RequestID: "plain-update"}, by); err != nil {
		t.Fatalf("keyed update on an unaccepted item: %v", err)
	}
}

// A deliberate scope change saves through the guard, and the release then
// refuses the accepted candidate as before.
func TestDeliberateScopeChangeStillBlocksRelease(t *testing.T) {
	for _, route := range []string{"PATCH", "keyed"} {
		t.Run(route, func(t *testing.T) {
			s, task, h, d, entry := releaseFixture(t)
			ctx := context.Background()
			by := api.Caller{Node: "fixture", User: "owner"}
			item := releaseEntryItem(t, s, task, entry)
			j, err := s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: "enqueue", Operation: "enqueue", AgentID: h.ID, RunID: h.RunID, EntryID: entry})
			if err != nil {
				t.Fatal(err)
			}
			amended := "Amended scope: also cover the second pane"
			var saved api.WorkItem
			if route == "PATCH" {
				saved, err = s.UpdateWorkItem(ctx, task.ID, item.ID, api.UpdateWorkItemRequest{Revision: item.Revision, Description: &amended, ScopeChange: true}, by)
			} else {
				if _, _, err = s.CreateWorkItemUpdate(ctx, task.ID, item.ID, api.CreateWorkItemUpdate{ExpectedRevision: item.Revision, Description: &amended, ScopeChange: true, RequestID: "amend"}, by); err == nil {
					saved, err = s.GetWorkItem(ctx, task.ID, item.ID)
				}
			}
			if err != nil || saved.Description != amended || saved.Revision != item.Revision+1 || saved.ScopeRevision != item.ScopeRevision+1 {
				t.Fatalf("deliberate scope change: %+v err=%v", saved, err)
			}
			_, err = s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: "claim", Operation: "claim", AgentID: d.ID, RunID: d.RunID, JobID: j.ID, ExpectedGeneration: j.Generation})
			if !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "exact accepted SHA and current verification required") {
				t.Fatalf("claim after a deliberate scope change: %v", err)
			}
		})
	}
}
