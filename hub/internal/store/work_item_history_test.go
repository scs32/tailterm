package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/scs32/tailterm/hub/internal/api"
)

func TestWorkItemHistoryNativeKeyedReceiptsAndExplicitMessages(t *testing.T) {
	s, ctx, by := workItemStore(t)
	project, _ := workItemProject(t, s, ctx, by, "History", "lead")
	handler, err := s.AddAgent(ctx, project.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: "database", Host: "host", Session: "db", Runtime: "codex", Role: api.AgentRoleDatabaseHandler}, by)
	if err != nil {
		t.Fatal(err)
	}
	source, err := s.PostMessage(ctx, project.ID, api.PostMessageRequest{Text: "Human source"}, by)
	if err != nil {
		t.Fatal(err)
	}
	item, err := s.CreateWorkItem(ctx, project.ID, api.CreateWorkItemRequest{Kind: "bug", Title: "Initial", Description: "before", AgentID: handler.ID, SourceMessageSeq: source.Seq, RequestID: "history-create"}, by)
	if err != nil {
		t.Fatal(err)
	}
	linked, err := s.PostMessage(ctx, project.ID, api.PostMessageRequest{AgentID: handler.ID, Text: "Explicit revision one", RequestID: "history-message", WorkItems: []api.MessageWorkItem{{ItemTaskID: project.ID, ItemID: item.ID, ItemRevision: 1, Relationship: "primary"}}}, by)
	if err != nil {
		t.Fatal(err)
	}
	title, empty, status := "Unicode こんにちは", "", "in_progress"
	wrongRun := api.CreateWorkItemUpdate{ExpectedRevision: 1, Status: &status, AgentID: handler.ID, RunID: "run_0000000000000000", RequestID: "wrong-run"}
	if _, _, err := s.CreateWorkItemUpdate(ctx, project.ID, item.ID, wrongRun, by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("stale run = %v", err)
	}
	req := api.CreateWorkItemUpdate{ExpectedRevision: 1, Title: &title, Description: &empty, Status: &status, AgentID: handler.ID, RunID: handler.RunID, RequestID: "history-update"}
	result, replay, err := s.CreateWorkItemUpdate(ctx, project.ID, item.ID, req, by)
	if err != nil || replay {
		t.Fatalf("first update: %+v replay=%v err=%v", result, replay, err)
	}
	if result.Revision.Revision != 2 || result.Revision.Title != title || result.Revision.Description != "" || result.Revision.UpdatedRunID != handler.RunID || len(result.Revision.ChangedFields) != 3 {
		t.Fatalf("revision: %+v", result.Revision)
	}
	result2, replay, err := s.CreateWorkItemUpdate(ctx, project.ID, item.ID, req, by)
	if err != nil || !replay || !reflect.DeepEqual(result2, result) {
		t.Fatalf("replay: %+v replay=%v err=%v", result2, replay, err)
	}
	changed := req
	priority := "urgent"
	changed.Priority = &priority
	if _, _, err := s.CreateWorkItemUpdate(ctx, project.ID, item.ID, changed, by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("changed intent = %v", err)
	}

	revisions, err := s.ListWorkItemRevisions(ctx, project.ID, item.ID, 0, 65)
	if err != nil || len(revisions.Revisions) != 2 || !revisions.Coverage.Complete || revisions.Coverage.SnapshotCount != 2 {
		t.Fatalf("history: %+v %v", revisions, err)
	}
	if revisions.Revisions[0].Title != "Initial" || revisions.Revisions[1].Title != title {
		t.Fatalf("versions: %+v", revisions.Revisions)
	}
	messages, err := s.ListWorkItemMessages(ctx, project.ID, item.ID, 0, 0, 65)
	if err != nil || len(messages.Links) != 2 {
		t.Fatalf("messages: %+v %v", messages, err)
	}
	if !messages.Links[0].Source || messages.Links[0].Message.Seq != source.Seq || messages.Links[1].Message.Seq != linked.Seq || messages.Links[1].ItemRevision != 1 {
		t.Fatalf("links: %+v", messages.Links)
	}
	if messages.Coverage.ConversationLinks != "explicit_only" || messages.Coverage.SourceMessageCount != 1 || messages.Coverage.ExplicitMessageCount != 1 {
		t.Fatalf("coverage: %+v", messages.Coverage)
	}
	target, _ := workItemProject(t, s, ctx, by, "History target", "target-lead")
	if _, err := s.DispatchWorkItem(ctx, project.ID, item.ID, api.DispatchWorkItemRequest{Revision: 2, TargetTaskID: target.ID, RequestID: "history-cross-dispatch"}, by); err != nil {
		t.Fatal(err)
	}
	dispatchMessages, err := s.ListWorkItemMessages(ctx, project.ID, item.ID, 2, 0, 65)
	if err != nil || len(dispatchMessages.Links) != 1 || dispatchMessages.Links[0].Message.TaskID != target.ID || dispatchMessages.Links[0].ItemRevision != 2 || dispatchMessages.Coverage.DispatchCount != 1 {
		t.Fatalf("cross-project message: %+v err=%v", dispatchMessages, err)
	}
	if _, err := s.GetWorkItemUpdateReceipt(ctx, project.ID, item.ID, req.RequestID, handler.ID, api.Caller{Node: "other-node", User: by.User}); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("cross-caller receipt = %v", err)
	}
	other, err := s.CreateWorkItem(ctx, project.ID, api.CreateWorkItemRequest{Kind: "feature", Title: "Other item", RequestID: "other-item"}, by)
	if err != nil {
		t.Fatal(err)
	}
	otherReq := req
	otherReq.ExpectedRevision = 1
	if _, _, err := s.CreateWorkItemUpdate(ctx, project.ID, other.ID, otherReq, by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("cross-item request key = %v", err)
	}

	if _, err := s.CloseTask(ctx, project.ID, by); err != nil {
		t.Fatal(err)
	}
	closedReplay, replay, err := s.CreateWorkItemUpdate(ctx, project.ID, item.ID, req, by)
	if err != nil || !replay || !reflect.DeepEqual(closedReplay, result) {
		t.Fatalf("closed replay: %+v %v %v", closedReplay, replay, err)
	}
	receipt, err := s.GetWorkItemUpdateReceipt(ctx, project.ID, item.ID, req.RequestID, handler.ID, by)
	if err != nil || !reflect.DeepEqual(receipt, result) {
		t.Fatalf("receipt: %+v %v", receipt, err)
	}
}

func TestWorkItemKeyedConcurrentExactReplay(t *testing.T) {
	s, ctx, by := workItemStore(t)
	project, _ := workItemProject(t, s, ctx, by, "Concurrent history", "lead")
	item := createWorkItem(t, s, ctx, by, project, "concurrent-history-create")
	status := "in_progress"
	req := api.CreateWorkItemUpdate{ExpectedRevision: 1, Status: &status, RequestID: "same-update"}
	const attempts = 8
	results := make(chan api.WorkItemUpdateResult, attempts)
	errs := make(chan error, attempts)
	var wg sync.WaitGroup
	for range attempts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, _, err := s.CreateWorkItemUpdate(ctx, project.ID, item.ID, req, by)
			results <- result
			errs <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var first api.WorkItemUpdateResult
	for result := range results {
		if first.Receipt.ID == "" {
			first = result
		}
		if !reflect.DeepEqual(result, first) {
			t.Fatalf("non-identical replay: %+v != %+v", result, first)
		}
	}
	var changes, snapshots, receipts int
	for query, target := range map[string]*int{
		`SELECT count(*) FROM work_item_changes WHERE item_id=? AND revision=2`:   &changes,
		`SELECT count(*) FROM work_item_revisions WHERE item_id=? AND revision=2`: &snapshots,
		`SELECT count(*) FROM work_item_update_requests WHERE item_id=?`:          &receipts,
	} {
		if err := s.db.QueryRowContext(ctx, query, item.ID).Scan(target); err != nil {
			t.Fatal(err)
		}
	}
	if changes != 1 || snapshots != 1 || receipts != 1 {
		t.Fatalf("changes=%d snapshots=%d receipts=%d", changes, snapshots, receipts)
	}
}

func TestWorkItemHistoryReconciliationUsesGapAndCheckpoint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.sqlite")
	ctx := context.Background()
	by := api.Caller{Node: "migration-node", User: "owner"}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	project, _ := workItemProject(t, s, ctx, by, "Migration", "lead")
	item := createWorkItem(t, s, ctx, by, project, "migration-create")
	title := "from change"
	item, err = s.UpdateWorkItem(ctx, project.ID, item.ID, api.UpdateWorkItemRequest{Revision: 1, Title: &title}, by)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`PRAGMA foreign_keys=OFF; DROP TABLE work_item_update_requests; DROP TABLE work_item_history_state; DROP TABLE work_item_history_gaps; DROP TABLE work_item_revisions; UPDATE work_items SET title='projection diverged' WHERE id=?`, item.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	revisions, err := s.ListWorkItemRevisions(ctx, project.ID, item.ID, 0, 65)
	if err != nil {
		t.Fatal(err)
	}
	if len(revisions.Revisions) != 2 || revisions.Revisions[0].Provenance != "reconstructed_change_log" || revisions.Revisions[1].Provenance != "current_row_checkpoint" || revisions.Coverage.Complete {
		t.Fatalf("reconciled history: %+v", revisions)
	}
	gaps, err := s.ListWorkItemHistoryGaps(ctx, project.ID, item.ID, 0, 65)
	if err != nil || len(gaps.Gaps) != 1 || gaps.Gaps[0].ReasonCode != "final_projection_mismatch" {
		t.Fatalf("gaps: %+v %v", gaps, err)
	}
	if _, err := s.GetWorkItemRevision(ctx, project.ID, item.ID, 3); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("out of range = %v", err)
	}
}

func TestWorkItemKeyedUpdateRollsBackEveryWriteBoundary(t *testing.T) {
	s, ctx, by := workItemStore(t)
	project, _ := workItemProject(t, s, ctx, by, "Atomic history", "lead")
	item := createWorkItem(t, s, ctx, by, project, "atomic-create")
	var baseChanges, baseSnapshots, baseReceipts, baseEvents int
	if err := s.db.QueryRow(`SELECT count(*) FROM work_item_changes WHERE item_id=?`, item.ID).Scan(&baseChanges); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT count(*) FROM work_item_revisions WHERE item_id=?`, item.ID).Scan(&baseSnapshots); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT count(*) FROM work_item_update_requests WHERE item_id=?`, item.ID).Scan(&baseReceipts); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT count(*) FROM events`).Scan(&baseEvents); err != nil {
		t.Fatal(err)
	}
	boundaries := []struct{ name, table, timing, operation string }{
		{"projection", "work_items", "BEFORE", "UPDATE"},
		{"change", "work_item_changes", "BEFORE", "INSERT"},
		{"snapshot", "work_item_revisions", "BEFORE", "INSERT"},
		{"receipt", "work_item_update_requests", "BEFORE", "INSERT"},
		{"event", "events", "BEFORE", "INSERT"},
	}
	for _, boundary := range boundaries {
		t.Run(boundary.name, func(t *testing.T) {
			trigger := "fail_history_" + boundary.name
			statement := fmt.Sprintf(`CREATE TRIGGER %s %s %s ON %s BEGIN SELECT RAISE(ABORT,'injected'); END`, trigger, boundary.timing, boundary.operation, boundary.table)
			if _, err := s.db.Exec(statement); err != nil {
				t.Fatal(err)
			}
			changed := s.Changed(project.ID)
			status := "in_progress"
			_, _, updateErr := s.CreateWorkItemUpdate(ctx, project.ID, item.ID, api.CreateWorkItemUpdate{ExpectedRevision: 1, Status: &status, RequestID: "fail-" + boundary.name}, by)
			if updateErr == nil {
				t.Fatal("injected failure succeeded")
			}
			if _, err := s.db.Exec(`DROP TRIGGER ` + trigger); err != nil {
				t.Fatal(err)
			}
			select {
			case <-changed:
				t.Fatal("failed transaction emitted a notification")
			default:
			}
			current, err := s.GetWorkItem(ctx, project.ID, item.ID)
			if err != nil || current.Revision != 1 {
				t.Fatalf("current=%+v err=%v", current, err)
			}
			var changes, snapshots, receipts, events int
			if err := s.db.QueryRow(`SELECT count(*) FROM work_item_changes WHERE item_id=?`, item.ID).Scan(&changes); err != nil {
				t.Fatal(err)
			}
			if err := s.db.QueryRow(`SELECT count(*) FROM work_item_revisions WHERE item_id=?`, item.ID).Scan(&snapshots); err != nil {
				t.Fatal(err)
			}
			if err := s.db.QueryRow(`SELECT count(*) FROM work_item_update_requests WHERE item_id=?`, item.ID).Scan(&receipts); err != nil {
				t.Fatal(err)
			}
			if err := s.db.QueryRow(`SELECT count(*) FROM events`).Scan(&events); err != nil {
				t.Fatal(err)
			}
			if changes != baseChanges || snapshots != baseSnapshots || receipts != baseReceipts || events != baseEvents {
				t.Fatalf("partial write changes=%d snapshots=%d receipts=%d events=%d", changes, snapshots, receipts, events)
			}
		})
	}
}

func TestWorkItemExactRevisionDistinguishesGapFromNotFound(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.sqlite")
	ctx := context.Background()
	by := api.Caller{Node: "gap-node", User: "owner"}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	project, _ := workItemProject(t, s, ctx, by, "Gap migration", "lead")
	item := createWorkItem(t, s, ctx, by, project, "gap-migration-create")
	title := "revision two"
	item, err = s.UpdateWorkItem(ctx, project.ID, item.ID, api.UpdateWorkItemRequest{Revision: 1, Title: &title}, by)
	if err != nil {
		t.Fatal(err)
	}
	description := "revision three"
	item, err = s.UpdateWorkItem(ctx, project.ID, item.ID, api.UpdateWorkItemRequest{Revision: 2, Description: &description}, by)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`PRAGMA foreign_keys=OFF; DROP TABLE work_item_update_requests; DROP TABLE work_item_history_state; DROP TABLE work_item_history_gaps; DROP TABLE work_item_revisions; DELETE FROM work_item_changes WHERE item_id=? AND revision=2`, item.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.GetWorkItemRevision(ctx, project.ID, item.ID, 2); err == nil {
		t.Fatal("missing revision returned as a snapshot")
	} else {
		var gap *WorkItemHistoryGapError
		if !errors.As(err, &gap) || gap.Gap.ReasonCode != "missing_revision" {
			t.Fatalf("gap error=%T %v", err, err)
		}
	}
	checkpoint, err := s.GetWorkItemRevision(ctx, project.ID, item.ID, 3)
	if err != nil || checkpoint.Provenance != "current_row_checkpoint" {
		t.Fatalf("checkpoint=%+v %v", checkpoint, err)
	}
	if _, err := s.GetWorkItemRevision(ctx, project.ID, item.ID, 4); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("future revision=%v", err)
	}
}

func TestWorkItemHistoryReconciliationContainsMalformedItems(t *testing.T) {
	for _, tc := range []struct {
		name, reason, mutation string
	}{
		{"invalid-json", "invalid_change_json", `UPDATE work_item_changes SET fields='{' WHERE item_id=? AND revision=2`},
		{"duplicate", "duplicate_revision", `INSERT INTO work_item_changes(item_id,revision,kind,fields,agent_id,by_node,by_user,created_at) SELECT item_id,revision,kind,fields,agent_id,by_node,by_user,created_at FROM work_item_changes WHERE item_id=? AND revision=2`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "hub.sqlite")
			ctx := context.Background()
			by := api.Caller{Node: "malformed-node", User: "owner"}
			s, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			project, _ := workItemProject(t, s, ctx, by, "Malformed "+tc.name, "lead")
			bad := createWorkItem(t, s, ctx, by, project, "malformed-create")
			title := "revision two"
			bad, err = s.UpdateWorkItem(ctx, project.ID, bad.ID, api.UpdateWorkItemRequest{Revision: 1, Title: &title}, by)
			if err != nil {
				t.Fatal(err)
			}
			good := createWorkItem(t, s, ctx, by, project, "unrelated-create")
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = db.Exec(`PRAGMA foreign_keys=OFF; DROP TABLE work_item_update_requests; DROP TABLE work_item_history_state; DROP TABLE work_item_history_gaps; DROP TABLE work_item_revisions`); err != nil {
				t.Fatal(err)
			}
			if _, err = db.Exec(tc.mutation, bad.ID); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			s, err = Open(path)
			if err != nil {
				t.Fatalf("one malformed item aborted the hub: %v", err)
			}
			defer s.Close()
			gaps, err := s.ListWorkItemHistoryGaps(ctx, project.ID, bad.ID, 0, 65)
			if err != nil || len(gaps.Gaps) != 1 || gaps.Gaps[0].ReasonCode != tc.reason {
				t.Fatalf("gaps=%+v err=%v", gaps, err)
			}
			checkpoint, err := s.GetWorkItemRevision(ctx, project.ID, bad.ID, 2)
			if err != nil || checkpoint.Provenance != "current_row_checkpoint" {
				t.Fatalf("checkpoint=%+v err=%v", checkpoint, err)
			}
			status := "done"
			if _, err := s.UpdateWorkItem(ctx, project.ID, good.ID, api.UpdateWorkItemRequest{Revision: 1, Status: &status}, by); err != nil {
				t.Fatalf("unrelated item disabled: %v", err)
			}
		})
	}
}

func TestWorkItemHistoryPreservesCollisionAndSnapshotsProspectiveEdit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.sqlite")
	ctx := context.Background()
	by := api.Caller{Node: "collision-node", User: "owner"}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	project, _ := workItemProject(t, s, ctx, by, "Collision", "lead")
	item := createWorkItem(t, s, ctx, by, project, "collision-create")
	if _, err := s.db.Exec(`UPDATE work_items SET title='different current projection' WHERE id=?`, item.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	gaps, err := s.ListWorkItemHistoryGaps(ctx, project.ID, item.ID, 0, 65)
	if err != nil || len(gaps.Gaps) != 1 || gaps.Gaps[0].ReasonCode != "immutable_snapshot_collision" {
		t.Fatalf("collision gaps=%+v err=%v", gaps, err)
	}
	preserved, err := s.GetWorkItemRevision(ctx, project.ID, item.ID, 1)
	if err != nil || preserved.Title != "Retry loses state" {
		t.Fatalf("preserved=%+v err=%v", preserved, err)
	}
	title := "prospective native revision"
	updated, err := s.UpdateWorkItem(ctx, project.ID, item.ID, api.UpdateWorkItemRequest{Revision: 1, Title: &title}, by)
	if err != nil {
		t.Fatal(err)
	}
	prospective, err := s.GetWorkItemRevision(ctx, project.ID, item.ID, 2)
	if err != nil || prospective.Title != updated.Title || prospective.Provenance != "native" {
		t.Fatalf("prospective=%+v err=%v", prospective, err)
	}
	history, err := s.ListWorkItemRevisions(ctx, project.ID, item.ID, 0, 65)
	if err != nil || history.Coverage.Complete || history.Coverage.GapCount != 1 {
		t.Fatalf("coverage=%+v err=%v", history.Coverage, err)
	}
}
