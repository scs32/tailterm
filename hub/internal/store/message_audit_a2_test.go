package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

func a2Item(t *testing.T, s *Store, ctx context.Context, by api.Caller, task api.Task, key string) api.WorkItem {
	t.Helper()
	item, err := s.CreateWorkItem(ctx, task.ID, api.CreateWorkItemRequest{Kind: "bug", Title: "A2 " + key, RequestID: key}, by)
	if err != nil {
		t.Fatal(err)
	}
	return item
}

func a2Source(t *testing.T, s *Store, ctx context.Context, by api.Caller, task api.Task, text string) api.Message {
	t.Helper()
	message, err := s.PostMessage(ctx, task.ID, api.PostMessageRequest{Text: text}, by)
	if err != nil {
		t.Fatal(err)
	}
	return message
}

func TestMessageAuditA2FullStateHistoryFeedAndFrozenPostReplay(t *testing.T) {
	s, ctx, by := workItemStore(t)
	task, _ := workItemProject(t, s, ctx, by, "A2 corrections", "lead")
	source := a2Source(t, s, ctx, by, task, "Evidence source")
	primary := a2Item(t, s, ctx, by, task, "primary")
	related := a2Item(t, s, ctx, by, task, "related")
	request := api.PostMessageRequest{Text: "Immutable original", AuditKind: api.MessageAuditWork, RequestID: "a2-original-post",
		WorkItems: []api.MessageWorkItem{
			{ItemTaskID: task.ID, ItemID: primary.ID, ItemRevision: primary.Revision, Relationship: "primary"},
			{ItemTaskID: task.ID, ItemID: related.ID, ItemRevision: related.Revision, Relationship: "related"},
		}}
	original, err := s.PostMessage(ctx, task.ID, request, by)
	if err != nil {
		t.Fatal(err)
	}
	record, err := s.GetMessageAudit(ctx, task.ID, original.Seq)
	if err != nil || record.Current == nil || record.Current.Revision != 1 || record.Current.Classification != api.MessageAuditWork ||
		!reflect.DeepEqual(record.Original.WorkItems, orderAuditLinks(request.WorkItems)) || !reflect.DeepEqual(record.Current.WorkItems, orderAuditLinks(request.WorkItems)) {
		t.Fatalf("initial audit: %+v err=%v", record, err)
	}
	updatedTitle := "A2 primary after original message"
	if _, err := s.UpdateWorkItem(ctx, task.ID, primary.ID, api.UpdateWorkItemRequest{Revision: primary.Revision, Title: &updatedTitle}, by); err != nil {
		t.Fatal(err)
	}
	correct := api.CorrectMessageAuditRequest{RequestID: "a2-correct-primary", ExpectedRevision: 1, Reason: "The related item is the actual owner.",
		Sources: []api.MessageReference{{TaskID: task.ID, Seq: source.Seq}},
		Desired: api.MessageAuditDesiredState{Classification: api.MessageAuditWork, WorkItems: []api.MessageWorkItem{
			{ItemTaskID: task.ID, ItemID: related.ID, ItemRevision: related.Revision, Relationship: "primary"},
			{ItemTaskID: task.ID, ItemID: primary.ID, ItemRevision: primary.Revision, Relationship: "related"},
		}}}
	changed, replay, err := s.CorrectMessageAudit(ctx, task.ID, original.Seq, correct, by)
	if err != nil || replay || changed.Event.Before == nil || changed.Event.Before.Revision != 1 || changed.State.Current.Revision != 2 {
		t.Fatalf("correction: %+v replay=%v err=%v", changed, replay, err)
	}
	postReplay, err := s.PostMessage(ctx, task.ID, request, by)
	if err != nil || !reflect.DeepEqual(postReplay, original) {
		t.Fatalf("post receipt changed after correction: replay=%+v original=%+v err=%v", postReplay, original, err)
	}
	if !reflect.DeepEqual(changed.State.Original.WorkItems, orderAuditLinks(request.WorkItems)) || reflect.DeepEqual(changed.State.Original.WorkItems, changed.State.Current.WorkItems) {
		t.Fatalf("original/current distinction lost: %+v", changed.State)
	}

	firstHistory, err := s.ListMessageAuditHistory(ctx, task.ID, original.Seq, 0, "", 1)
	if err != nil || len(firstHistory.Events) != 1 || firstHistory.Cutoff != 2 || firstHistory.NextCursor == "" {
		t.Fatalf("first frozen history: %+v err=%v", firstHistory, err)
	}
	toIntake := api.CorrectMessageAuditRequest{RequestID: "a2-to-intake", ExpectedRevision: 2, Reason: "Evidence shows this remains intake.",
		Sources: []api.MessageReference{{TaskID: task.ID, Seq: source.Seq}}, Desired: api.MessageAuditDesiredState{Classification: api.MessageAuditIntake}}
	intake, replay, err := s.CorrectMessageAudit(ctx, task.ID, original.Seq, toIntake, by)
	if err != nil || replay || intake.State.Current.Revision != 3 || len(intake.State.Current.WorkItems) != 0 {
		t.Fatalf("work to intake: %+v replay=%v err=%v", intake, replay, err)
	}
	secondHistory, err := s.ListMessageAuditHistory(ctx, task.ID, original.Seq, 0, firstHistory.NextCursor, 2)
	if err != nil || len(secondHistory.Events) != 1 || secondHistory.Events[0].Revision != 2 || secondHistory.Cutoff != firstHistory.Cutoff || secondHistory.NextCursor != "" {
		t.Fatalf("frozen history included later correction: %+v err=%v", secondHistory, err)
	}
	allHistory, err := s.ListMessageAuditHistory(ctx, task.ID, original.Seq, 0, "", api.MaxMessageAuditPage)
	if err != nil || len(allHistory.Events) != 3 || allHistory.Cutoff != 3 {
		t.Fatalf("complete history: %+v err=%v", allHistory, err)
	}

	feed, err := s.ListMessageAuditChanges(ctx, task.ID, "", "", 1, "")
	if err != nil || len(feed.Events) != 1 || feed.NextCursor == "" || feed.Cutoff < intake.Event.Cursor {
		t.Fatalf("first feed: %+v err=%v", feed, err)
	}
	frozen := feed.Cutoff
	seen := len(feed.Events)
	for feed.NextCursor != "" {
		feed, err = s.ListMessageAuditChanges(ctx, task.ID, feed.NextCursor, "", 1, "")
		if err != nil || feed.Cutoff != frozen {
			t.Fatalf("continued feed: %+v err=%v", feed, err)
		}
		seen += len(feed.Events)
	}
	if seen != 3 || feed.Checkpoint == "" {
		t.Fatalf("feed event count=%d checkpoint=%q want=3 and resumable", seen, feed.Checkpoint)
	}
	if _, err := s.ListMessageAuditChanges(ctx, task.ID, encodeMessageAuditCursor(messageAuditCursor{Task: task.ID, High: frozen + 99, After: 0}), "", 1, ""); !errors.Is(err, api.ErrInvalid) {
		t.Fatalf("future cursor accepted: %v", err)
	}

	noOp := toIntake
	noOp.RequestID = "a2-noop"
	noOp.ExpectedRevision = 3
	if _, _, err := s.CorrectMessageAudit(ctx, task.ID, original.Seq, noOp, by); !errors.Is(err, api.ErrInvalid) {
		t.Fatalf("no-op correction=%v", err)
	}
	missingPrimary := correct
	missingPrimary.RequestID = "a2-missing-primary"
	missingPrimary.ExpectedRevision = 3
	missingPrimary.Desired.WorkItems = []api.MessageWorkItem{{ItemTaskID: task.ID, ItemID: primary.ID, ItemRevision: 1, Relationship: "related"}}
	if _, _, err := s.CorrectMessageAudit(ctx, task.ID, original.Seq, missingPrimary, by); !errors.Is(err, api.ErrInvalid) {
		t.Fatalf("work without primary=%v", err)
	}
	duplicate := correct
	duplicate.RequestID = "a2-duplicate"
	duplicate.ExpectedRevision = 3
	duplicate.Desired.WorkItems = []api.MessageWorkItem{
		{ItemTaskID: task.ID, ItemID: primary.ID, ItemRevision: 1, Relationship: "primary"},
		{ItemTaskID: task.ID, ItemID: primary.ID, ItemRevision: 1, Relationship: "related"},
	}
	if _, _, err := s.CorrectMessageAudit(ctx, task.ID, original.Seq, duplicate, by); !errors.Is(err, api.ErrInvalid) {
		t.Fatalf("duplicate target=%v", err)
	}
	invalidSource := correct
	invalidSource.RequestID = "a2-invalid-source"
	invalidSource.ExpectedRevision = 3
	invalidSource.Sources = []api.MessageReference{{TaskID: task.ID, Seq: original.Seq + 9999}}
	if _, _, err := s.CorrectMessageAudit(ctx, task.ID, original.Seq, invalidSource, by); !errors.Is(err, api.ErrInvalid) {
		t.Fatalf("missing source reference=%v", err)
	}
	tooManySources := invalidSource
	tooManySources.RequestID = "a2-too-many-sources"
	tooManySources.Sources = make([]api.MessageReference, api.MaxMessageAuditSources+1)
	for index := range tooManySources.Sources {
		tooManySources.Sources[index] = api.MessageReference{TaskID: task.ID, Seq: source.Seq}
	}
	if _, _, err := s.CorrectMessageAudit(ctx, task.ID, original.Seq, tooManySources, by); !errors.Is(err, api.ErrInvalid) {
		t.Fatalf("too many sources=%v", err)
	}
	oversizedCaller := by
	oversizedCaller.Node = strings.Repeat("n", 513)
	validCallerCheck := invalidSource
	validCallerCheck.RequestID = "a2-oversized-caller"
	validCallerCheck.Sources = []api.MessageReference{{TaskID: task.ID, Seq: source.Seq}}
	if _, _, err := s.CorrectMessageAudit(ctx, task.ID, original.Seq, validCallerCheck, oversizedCaller); !errors.Is(err, api.ErrInvalid) {
		t.Fatalf("oversized correction caller=%v", err)
	}
	var lastMessageBefore int64
	if err := s.db.QueryRowContext(ctx, `SELECT max(seq) FROM messages WHERE task_id=?`, task.ID).Scan(&lastMessageBefore); err != nil {
		t.Fatal(err)
	}
	resumeCorrection := correct
	resumeCorrection.RequestID = "a2-resume-after-cutoff"
	resumeCorrection.ExpectedRevision = 3
	resumedEvent, _, err := s.CorrectMessageAudit(ctx, task.ID, original.Seq, resumeCorrection, by)
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := s.ListMessageAuditChanges(ctx, task.ID, "", feed.Checkpoint, api.MaxMessageAuditPage, "")
	if err != nil || len(resumed.Events) != 1 || resumed.Events[0].Cursor != resumedEvent.Event.Cursor || resumed.Cutoff <= frozen || resumed.Checkpoint == "" {
		t.Fatalf("incremental resume: %+v err=%v", resumed, err)
	}
	emptyResume, err := s.ListMessageAuditChanges(ctx, task.ID, "", resumed.Checkpoint, api.MaxMessageAuditPage, "")
	if err != nil || len(emptyResume.Events) != 0 || emptyResume.Checkpoint == "" {
		t.Fatalf("empty incremental resume: %+v err=%v", emptyResume, err)
	}
	intakeFeed, err := s.ListMessageAuditChanges(ctx, task.ID, "", "", api.MaxMessageAuditPage, api.MessageAuditIntake)
	if err != nil || len(intakeFeed.Events) != 1 || intakeFeed.Checkpoint == "" {
		t.Fatalf("initial filtered feed: %+v err=%v", intakeFeed, err)
	}
	workOnly := resumeCorrection
	workOnly.RequestID = "a2-filtered-empty-resume"
	workOnly.ExpectedRevision = 4
	workOnly.Desired.WorkItems = workOnly.Desired.WorkItems[:1]
	if _, _, err := s.CorrectMessageAudit(ctx, task.ID, original.Seq, workOnly, by); err != nil {
		t.Fatal(err)
	}
	emptyFiltered, err := s.ListMessageAuditChanges(ctx, task.ID, "", intakeFeed.Checkpoint, api.MaxMessageAuditPage, api.MessageAuditIntake)
	if err != nil || len(emptyFiltered.Events) != 0 || emptyFiltered.Cutoff <= intakeFeed.Cutoff || emptyFiltered.Checkpoint == "" {
		t.Fatalf("empty filtered resume: %+v err=%v", emptyFiltered, err)
	}
	backToIntake := toIntake
	backToIntake.RequestID = "a2-filtered-later-intake"
	backToIntake.ExpectedRevision = 5
	intakeAgain, _, err := s.CorrectMessageAudit(ctx, task.ID, original.Seq, backToIntake, by)
	if err != nil {
		t.Fatal(err)
	}
	filteredResume, err := s.ListMessageAuditChanges(ctx, task.ID, "", emptyFiltered.Checkpoint, api.MaxMessageAuditPage, api.MessageAuditIntake)
	if err != nil || len(filteredResume.Events) != 1 || filteredResume.Events[0].Cursor != intakeAgain.Event.Cursor || filteredResume.Checkpoint == "" {
		t.Fatalf("filtered incremental resume: %+v err=%v", filteredResume, err)
	}
	var lastMessageAfter int64
	if err := s.db.QueryRowContext(ctx, `SELECT max(seq) FROM messages WHERE task_id=?`, task.ID).Scan(&lastMessageAfter); err != nil || lastMessageAfter != lastMessageBefore {
		t.Fatalf("audit resume required new message: before=%d after=%d err=%v", lastMessageBefore, lastMessageAfter, err)
	}

	if _, err := s.CloseTask(ctx, task.ID, by); err != nil {
		t.Fatal(err)
	}
	replayedCorrection, replay, err := s.CorrectMessageAudit(ctx, task.ID, original.Seq, toIntake, by)
	if err != nil || !replay || replayedCorrection.Receipt.ID != intake.Receipt.ID {
		t.Fatalf("closed correction replay: %+v replay=%v err=%v", replayedCorrection, replay, err)
	}
	changedKey := toIntake
	changedKey.Reason = "Changed payload"
	if _, _, err := s.CorrectMessageAudit(ctx, task.ID, original.Seq, changedKey, by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("changed replay payload=%v", err)
	}
}

func TestMessageAuditA2IntakeResolutionAtomicityAndRaces(t *testing.T) {
	t.Run("existing and new item", func(t *testing.T) {
		s, ctx, by := workItemStore(t)
		task, _ := workItemProject(t, s, ctx, by, "A2 resolve", "lead")
		source := a2Source(t, s, ctx, by, task, "Resolution evidence")
		item := a2Item(t, s, ctx, by, task, "existing")
		intake, err := s.PostMessage(ctx, task.ID, api.PostMessageRequest{Text: "Needs classification", AuditKind: api.MessageAuditIntake, RequestID: "intake-existing"}, by)
		if err != nil {
			t.Fatal(err)
		}
		req := api.ResolveMessageAuditRequest{RequestID: "resolve-existing", ExpectedRevision: 1, Reason: "Handler selected the retained item.",
			Sources:      []api.MessageReference{{TaskID: task.ID, Seq: source.Seq}},
			ExistingItem: &api.MessageWorkItem{ItemTaskID: task.ID, ItemID: item.ID, ItemRevision: item.Revision, Relationship: "primary"}}
		result, replay, err := s.ResolveMessageAudit(ctx, task.ID, intake.Seq, req, by)
		if err != nil || replay || result.Item == nil || result.Item.ID != item.ID || result.State.Current.Revision != 2 || result.State.Current.Classification != api.MessageAuditWork {
			t.Fatalf("existing resolution: %+v replay=%v err=%v", result, replay, err)
		}
		title := "Later item revision"
		if _, err := s.UpdateWorkItem(ctx, task.ID, item.ID, api.UpdateWorkItemRequest{Revision: item.Revision, Title: &title}, by); err != nil {
			t.Fatal(err)
		}
		if _, err := s.CloseTask(ctx, task.ID, by); err != nil {
			t.Fatal(err)
		}
		recovered, replay, err := s.ResolveMessageAudit(ctx, task.ID, intake.Seq, req, by)
		if err != nil || !replay || recovered.Receipt.ID != result.Receipt.ID || recovered.Item.Revision != 1 {
			t.Fatalf("lost response after change/closure: %+v replay=%v err=%v", recovered, replay, err)
		}
	})

	t.Run("same-key and competing races", func(t *testing.T) {
		s, ctx, by := workItemStore(t)
		task, _ := workItemProject(t, s, ctx, by, "A2 resolve races", "lead")
		source := a2Source(t, s, ctx, by, task, "Resolution source")
		intake, err := s.PostMessage(ctx, task.ID, api.PostMessageRequest{Text: "Create an item", AuditKind: api.MessageAuditIntake, RequestID: "race-intake"}, by)
		if err != nil {
			t.Fatal(err)
		}
		req := api.ResolveMessageAuditRequest{RequestID: "race-resolve", ExpectedRevision: 1, Reason: "Create exactly once.",
			Sources: []api.MessageReference{{TaskID: task.ID, Seq: source.Seq}},
			NewItem: &api.ResolveMessageAuditNewItem{RequestID: "race-new-item", Kind: "feature", Title: "Atomic intake item", Priority: "normal"}}
		var wg sync.WaitGroup
		results := make(chan api.MessageAuditMutationResult, 2)
		errs := make(chan error, 2)
		for range 2 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				result, _, err := s.ResolveMessageAudit(ctx, task.ID, intake.Seq, req, by)
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
		var ids []string
		for result := range results {
			ids = append(ids, result.Item.ID)
		}
		if len(ids) != 2 || ids[0] != ids[1] {
			t.Fatalf("same-key race created different items: %v", ids)
		}
		createReplay, err := s.CreateWorkItem(ctx, task.ID, api.CreateWorkItemRequest{Kind: "feature", Title: "Atomic intake item", Priority: "normal", SourceMessageSeq: intake.Seq, RequestID: "race-new-item"}, by)
		if err != nil || createReplay.ID != ids[0] {
			t.Fatalf("native create receipt disagrees with resolve create: %+v err=%v", createReplay, err)
		}
		var count int
		if err := s.db.QueryRow(`SELECT count(*) FROM work_items WHERE task_id=? AND source_message_seq=?`, task.ID, intake.Seq).Scan(&count); err != nil || count != 1 {
			t.Fatalf("created items=%d err=%v", count, err)
		}

		intake2, err := s.PostMessage(ctx, task.ID, api.PostMessageRequest{Text: "Competing create", AuditKind: api.MessageAuditIntake, RequestID: "compete-intake"}, by)
		if err != nil {
			t.Fatal(err)
		}
		requests := []api.ResolveMessageAuditRequest{
			{RequestID: "compete-a", ExpectedRevision: 1, Reason: "First contender.", Sources: req.Sources, NewItem: &api.ResolveMessageAuditNewItem{RequestID: "compete-item-a", Kind: "bug", Title: "Contender A"}},
			{RequestID: "compete-b", ExpectedRevision: 1, Reason: "Second contender.", Sources: req.Sources, NewItem: &api.ResolveMessageAuditNewItem{RequestID: "compete-item-b", Kind: "bug", Title: "Contender B"}},
		}
		successes, conflicts := 0, 0
		for _, contender := range requests {
			if _, _, err := s.ResolveMessageAudit(ctx, task.ID, intake2.Seq, contender, by); err == nil {
				successes++
			} else if errors.Is(err, api.ErrConflict) {
				conflicts++
			} else {
				t.Fatal(err)
			}
		}
		if successes != 1 || conflicts != 1 {
			t.Fatalf("competing resolution successes=%d conflicts=%d", successes, conflicts)
		}
		if err := s.db.QueryRow(`SELECT count(*) FROM work_items WHERE task_id=? AND source_message_seq=?`, task.ID, intake2.Seq).Scan(&count); err != nil || count != 1 {
			t.Fatalf("competing create leaked item: count=%d err=%v", count, err)
		}

		intake3, err := s.PostMessage(ctx, task.ID, api.PostMessageRequest{Text: "Existing create receipt", AuditKind: api.MessageAuditIntake, RequestID: "existing-create-intake"}, by)
		if err != nil {
			t.Fatal(err)
		}
		precreated, err := s.CreateWorkItem(ctx, task.ID, api.CreateWorkItemRequest{Kind: "bug", Title: "Precreated for intake", Priority: "normal", SourceMessageSeq: intake3.Seq, RequestID: "existing-create-key"}, by)
		if err != nil {
			t.Fatal(err)
		}
		precreatedResolve := api.ResolveMessageAuditRequest{RequestID: "existing-create-resolve", ExpectedRevision: 1, Reason: "Reuse the exact native create receipt.", Sources: req.Sources,
			NewItem: &api.ResolveMessageAuditNewItem{RequestID: "existing-create-key", Kind: "bug", Title: "Precreated for intake", Priority: "normal"}}
		precreatedResult, _, err := s.ResolveMessageAudit(ctx, task.ID, intake3.Seq, precreatedResolve, by)
		if err != nil || precreatedResult.Item == nil || precreatedResult.Item.ID != precreated.ID {
			t.Fatalf("resolve disagrees with existing create receipt: %+v err=%v", precreatedResult, err)
		}
	})

	for _, tc := range []struct {
		name    string
		trigger string
	}{
		{"item", `CREATE TRIGGER fail_a2_item BEFORE INSERT ON work_items BEGIN SELECT RAISE(ABORT,'item'); END`},
		{"change", `CREATE TRIGGER fail_a2_change BEFORE INSERT ON work_item_changes BEGIN SELECT RAISE(ABORT,'change'); END`},
		{"revision", `CREATE TRIGGER fail_a2_revision BEFORE INSERT ON work_item_revisions BEGIN SELECT RAISE(ABORT,'revision'); END`},
		{"history state", `CREATE TRIGGER fail_a2_history BEFORE INSERT ON work_item_history_state BEGIN SELECT RAISE(ABORT,'history'); END`},
		{"item event", `CREATE TRIGGER fail_a2_item_event BEFORE INSERT ON events WHEN NEW.kind='work_item_created' BEGIN SELECT RAISE(ABORT,'event'); END`},
		{"audit event", `CREATE TRIGGER fail_a2_audit_event BEFORE INSERT ON message_audit_events WHEN NEW.operation='resolve' BEGIN SELECT RAISE(ABORT,'audit'); END`},
		{"receipt", `CREATE TRIGGER fail_a2_receipt BEFORE INSERT ON message_audit_receipts BEGIN SELECT RAISE(ABORT,'receipt'); END`},
	} {
		t.Run("rollback "+tc.name, func(t *testing.T) {
			s, ctx, by := workItemStore(t)
			task, _ := workItemProject(t, s, ctx, by, "A2 rollback "+tc.name, "lead")
			source := a2Source(t, s, ctx, by, task, "Rollback evidence")
			intake, err := s.PostMessage(ctx, task.ID, api.PostMessageRequest{Text: "Rollback intake", AuditKind: api.MessageAuditIntake, RequestID: "rollback-intake"}, by)
			if err != nil {
				t.Fatal(err)
			}
			var beforeItems, beforeChanges, beforeEvents, beforeAudit int
			if err := s.db.QueryRow(`SELECT (SELECT count(*) FROM work_items),(SELECT count(*) FROM work_item_changes),(SELECT count(*) FROM events),(SELECT count(*) FROM message_audit_events)`).Scan(&beforeItems, &beforeChanges, &beforeEvents, &beforeAudit); err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec(tc.trigger); err != nil {
				t.Fatal(err)
			}
			req := api.ResolveMessageAuditRequest{RequestID: "rollback-resolve", ExpectedRevision: 1, Reason: "Must be atomic.",
				Sources: []api.MessageReference{{TaskID: task.ID, Seq: source.Seq}}, NewItem: &api.ResolveMessageAuditNewItem{RequestID: "rollback-new-item", Kind: "bug", Title: "Must roll back"}}
			if _, _, err := s.ResolveMessageAudit(ctx, task.ID, intake.Seq, req, by); err == nil {
				t.Fatal("injected resolution failure committed")
			}
			var items, changes, events, auditEvents, createReceipts, auditReceipts int
			if err := s.db.QueryRow(`SELECT (SELECT count(*) FROM work_items),(SELECT count(*) FROM work_item_changes),(SELECT count(*) FROM events),(SELECT count(*) FROM message_audit_events),
(SELECT count(*) FROM work_item_requests WHERE request_id='rollback-new-item'),(SELECT count(*) FROM message_audit_receipts WHERE request_id='rollback-resolve')`).Scan(
				&items, &changes, &events, &auditEvents, &createReceipts, &auditReceipts); err != nil {
				t.Fatal(err)
			}
			if items != beforeItems || changes != beforeChanges || events != beforeEvents || auditEvents != beforeAudit || createReceipts != 0 || auditReceipts != 0 {
				t.Fatalf("partial resolution items=%d/%d changes=%d/%d events=%d/%d audit=%d/%d createReceipt=%d auditReceipt=%d",
					items, beforeItems, changes, beforeChanges, events, beforeEvents, auditEvents, beforeAudit, createReceipts, auditReceipts)
			}
			record, err := s.GetMessageAudit(ctx, task.ID, intake.Seq)
			if err != nil || record.Current == nil || record.Current.Revision != 1 || record.Current.Classification != api.MessageAuditIntake {
				t.Fatalf("intake changed after rollback: %+v err=%v", record, err)
			}
		})
	}
}

func TestMessageAuditA2ExplicitAndDispatchForeignAuthority(t *testing.T) {
	s, ctx, by := workItemStore(t)
	sourceTask, _ := workItemProject(t, s, ctx, by, "Foreign source", "sourcelead")
	destination, _ := workItemProject(t, s, ctx, by, "Foreign destination", "destlead")
	foreign := a2Item(t, s, ctx, by, sourceTask, "foreign")
	handler, err := s.AddAgent(ctx, destination.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: "db-handler-a2", Host: "host", Session: "db-handler-a2", Runtime: "codex", Role: api.AgentRoleDatabaseHandler}, by)
	if err != nil {
		t.Fatal(err)
	}
	ordinary, err := s.AddAgent(ctx, destination.ID, api.AddAgentRequest{Name: "ordinary-a2", Host: "host", Session: "ordinary-a2", Runtime: "codex"}, by)
	if err != nil {
		t.Fatal(err)
	}
	humanSource := a2Source(t, s, ctx, by, destination, "Human explicitly associates the foreign item")
	handlerSource, err := s.PostMessage(ctx, destination.ID, api.PostMessageRequest{Text: "Handler explicitly associates the foreign item", AgentID: handler.ID}, by)
	if err != nil {
		t.Fatal(err)
	}
	ordinarySource, err := s.PostMessage(ctx, destination.ID, api.PostMessageRequest{Text: "Ordinary agent assertion", AgentID: ordinary.ID}, by)
	if err != nil {
		t.Fatal(err)
	}
	base := api.CreateMessageAuditAssociationRequest{RequestID: "foreign-explicit", Item: api.MessageAuditItemReference{ItemTaskID: sourceTask.ID, ItemID: foreign.ID, ItemRevision: foreign.Revision},
		Source: api.MessageReference{TaskID: destination.ID, Seq: handlerSource.Seq}, Reason: "The handler verified this shared-workspace relationship.", AgentID: handler.ID, RunID: handler.RunID}
	retiredStatus := api.AgentRetired
	if _, err := s.UpdateAgent(ctx, handler.ID, api.UpdateAgentRequest{Status: &retiredStatus}, by); err != nil {
		t.Fatal(err)
	}
	retiredAssociation := base
	retiredAssociation.RequestID = "foreign-retired-handler"
	if _, _, err := s.CreateMessageAuditAssociation(ctx, destination.ID, retiredAssociation, by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("retired handler association=%v", err)
	}
	doneStatus := api.AgentDone
	if _, err := s.UpdateAgent(ctx, handler.ID, api.UpdateAgentRequest{Status: &doneStatus}, by); err != nil {
		t.Fatal(err)
	}
	stale := base
	stale.RequestID = "foreign-stale-run"
	stale.RunID = api.NewID("run")
	if _, _, err := s.CreateMessageAuditAssociation(ctx, destination.ID, stale, by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("stale handler run=%v", err)
	}
	wrongSource := base
	wrongSource.RequestID = "foreign-wrong-source"
	wrongSource.Source.Seq = humanSource.Seq
	if _, _, err := s.CreateMessageAuditAssociation(ctx, destination.ID, wrongSource, by); !errors.Is(err, api.ErrInvalid) {
		t.Fatalf("handler claimed human source=%v", err)
	}
	wrongCaller := base
	wrongCaller.RequestID = "foreign-wrong-caller"
	if _, _, err := s.CreateMessageAuditAssociation(ctx, destination.ID, wrongCaller, api.Caller{Node: "different-node", User: by.User}); !errors.Is(err, api.ErrInvalid) {
		t.Fatalf("handler claimed another caller's source=%v", err)
	}
	ordinaryClaim := base
	ordinaryClaim.RequestID = "foreign-ordinary"
	ordinaryClaim.AgentID, ordinaryClaim.RunID, ordinaryClaim.Source.Seq = ordinary.ID, ordinary.RunID, ordinarySource.Seq
	if _, _, err := s.CreateMessageAuditAssociation(ctx, destination.ID, ordinaryClaim, by); !errors.Is(err, api.ErrInvalid) {
		t.Fatalf("ordinary agent established authority=%v", err)
	}
	association, replay, err := s.CreateMessageAuditAssociation(ctx, destination.ID, base, by)
	if err != nil || replay || association.Association.Actor.RunID != handler.RunID || association.Association.Source.Seq != handlerSource.Seq {
		t.Fatalf("handler association: %+v replay=%v err=%v", association, replay, err)
	}
	recovered, err := s.GetMessageAuditAssociationReceipt(ctx, destination.ID, base.RequestID, handler.ID, by)
	if err != nil || !recovered.Replay || recovered.Receipt.ID != association.Receipt.ID {
		t.Fatalf("association recovery: %+v err=%v", recovered, err)
	}
	closedStatus := api.AgentClosed
	if _, err := s.UpdateAgent(ctx, handler.ID, api.UpdateAgentRequest{Status: &closedStatus}, by); err != nil {
		t.Fatal(err)
	}
	closedReplay, replay, err := s.CreateMessageAuditAssociation(ctx, destination.ID, base, by)
	if err != nil || !replay || closedReplay.Receipt.ID != association.Receipt.ID {
		t.Fatalf("closed-handler association replay: %+v replay=%v err=%v", closedReplay, replay, err)
	}
	foreignTitle := "Foreign item revision two"
	foreignCurrent, err := s.UpdateWorkItem(ctx, sourceTask.ID, foreign.ID, api.UpdateWorkItemRequest{Revision: foreign.Revision, Title: &foreignTitle}, by)
	if err != nil {
		t.Fatal(err)
	}

	intake, err := s.PostMessage(ctx, destination.ID, api.PostMessageRequest{Text: "Foreign intake", AuditKind: api.MessageAuditIntake, RequestID: "foreign-intake"}, by)
	if err != nil {
		t.Fatal(err)
	}
	correction := api.CorrectMessageAuditRequest{RequestID: "foreign-correction", ExpectedRevision: 1, Reason: "Use the explicit association.",
		Sources: []api.MessageReference{{TaskID: destination.ID, Seq: handlerSource.Seq}}, AgentID: ordinary.ID, RunID: ordinary.RunID,
		Desired: api.MessageAuditDesiredState{Classification: api.MessageAuditWork, WorkItems: []api.MessageWorkItem{{ItemTaskID: sourceTask.ID, ItemID: foreign.ID, ItemRevision: foreign.Revision, Relationship: "primary"}}}}
	if result, _, err := s.CorrectMessageAudit(ctx, destination.ID, intake.Seq, correction, by); err != nil || result.State.Current.WorkItems[0].ItemID != foreign.ID {
		t.Fatalf("authorized foreign correction: %+v err=%v", result, err)
	}
	revisionTwoIntake, err := s.PostMessage(ctx, destination.ID, api.PostMessageRequest{Text: "Foreign revision two intake", AuditKind: api.MessageAuditIntake, RequestID: "foreign-r2-intake"}, by)
	if err != nil {
		t.Fatal(err)
	}
	revisionTwoCorrection := correction
	revisionTwoCorrection.RequestID = "foreign-r2-correction"
	revisionTwoCorrection.Desired.WorkItems[0].ItemRevision = foreignCurrent.Revision
	if _, _, err := s.CorrectMessageAudit(ctx, destination.ID, revisionTwoIntake.Seq, revisionTwoCorrection, by); !errors.Is(err, api.ErrInvalid) {
		t.Fatalf("revision-one association authorized revision two=%v", err)
	}

	dispatchDestination, _ := workItemProject(t, s, ctx, by, "Dispatch destination", "dispatchlead")
	dispatch, err := s.DispatchWorkItem(ctx, sourceTask.ID, foreign.ID, api.DispatchWorkItemRequest{Revision: foreignCurrent.Revision, TargetTaskID: dispatchDestination.ID, RequestID: "foreign-dispatch"}, by)
	if err != nil {
		t.Fatal(err)
	}
	dispatchAudit, err := s.GetMessageAudit(ctx, dispatchDestination.ID, dispatch.Dispatch.MessageSeq)
	if err != nil || dispatchAudit.Current == nil || dispatchAudit.Current.WorkItems[0].ItemID != foreign.ID || dispatchAudit.Current.Classification != api.MessageAuditWork {
		t.Fatalf("dispatch baseline: %+v err=%v", dispatchAudit, err)
	}
	dispatchSource := a2Source(t, s, ctx, by, dispatchDestination, "Dispatch-derived correction evidence")
	dispatchIntake, err := s.PostMessage(ctx, dispatchDestination.ID, api.PostMessageRequest{Text: "Dispatch follow-up", AuditKind: api.MessageAuditIntake, RequestID: "dispatch-intake"}, by)
	if err != nil {
		t.Fatal(err)
	}
	dispatchCorrection := api.CorrectMessageAuditRequest{RequestID: "dispatch-correction", ExpectedRevision: 1, Reason: "Existing dispatch proves the project relationship.",
		Sources: []api.MessageReference{{TaskID: dispatchDestination.ID, Seq: dispatchSource.Seq}}, Desired: correction.Desired}
	if _, _, err := s.CorrectMessageAudit(ctx, dispatchDestination.ID, dispatchIntake.Seq, dispatchCorrection, by); err != nil {
		t.Fatalf("dispatch-derived authority=%v", err)
	}
}

func TestMessageAuditA2AgentMutationRequiresActiveExactRun(t *testing.T) {
	s, ctx, by := workItemStore(t)
	task, _ := workItemProject(t, s, ctx, by, "A2 run guards", "lead")
	agent, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "audit-actor", Host: "host", Session: "audit-actor", Runtime: "codex"}, by)
	if err != nil {
		t.Fatal(err)
	}
	source := a2Source(t, s, ctx, by, task, "Run guard source")
	item := a2Item(t, s, ctx, by, task, "run-guard-item")
	intake, err := s.PostMessage(ctx, task.ID, api.PostMessageRequest{Text: "Run guarded correction", AuditKind: api.MessageAuditIntake, RequestID: "run-guard-intake"}, by)
	if err != nil {
		t.Fatal(err)
	}
	correction := api.CorrectMessageAuditRequest{RequestID: "run-guard-correction", ExpectedRevision: 1, Reason: "Attribute the exact active run.", AgentID: agent.ID,
		Sources: []api.MessageReference{{TaskID: task.ID, Seq: source.Seq}}, Desired: api.MessageAuditDesiredState{Classification: api.MessageAuditWork,
			WorkItems: []api.MessageWorkItem{{ItemTaskID: task.ID, ItemID: item.ID, ItemRevision: item.Revision, Relationship: "primary"}}}}
	if _, _, err := s.CorrectMessageAudit(ctx, task.ID, intake.Seq, correction, by); !errors.Is(err, api.ErrInvalid) {
		t.Fatalf("missing correction run=%v", err)
	}
	correction.RunID = api.NewID("run")
	if _, _, err := s.CorrectMessageAudit(ctx, task.ID, intake.Seq, correction, by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("stale correction run=%v", err)
	}
	correction.RunID = agent.RunID
	result, replay, err := s.CorrectMessageAudit(ctx, task.ID, intake.Seq, correction, by)
	if err != nil || replay {
		t.Fatalf("current correction run: %+v replay=%v err=%v", result, replay, err)
	}
	retired := api.AgentRetired
	if _, err := s.UpdateAgent(ctx, agent.ID, api.UpdateAgentRequest{Status: &retired}, by); err != nil {
		t.Fatal(err)
	}
	recovered, replay, err := s.CorrectMessageAudit(ctx, task.ID, intake.Seq, correction, by)
	if err != nil || !replay || recovered.Receipt.ID != result.Receipt.ID {
		t.Fatalf("retired exact correction replay: %+v replay=%v err=%v", recovered, replay, err)
	}
	newCorrection := api.CorrectMessageAuditRequest{RequestID: "run-guard-new", ExpectedRevision: 2, Reason: "Retired runs cannot write.", AgentID: agent.ID, RunID: agent.RunID,
		Sources: correction.Sources, Desired: api.MessageAuditDesiredState{Classification: api.MessageAuditIntake}}
	if _, _, err := s.CorrectMessageAudit(ctx, task.ID, intake.Seq, newCorrection, by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("retired new correction=%v", err)
	}
	done := api.AgentDone
	if _, err := s.UpdateAgent(ctx, agent.ID, api.UpdateAgentRequest{Status: &done}, by); err != nil {
		t.Fatal(err)
	}
	resolveIntake, err := s.PostMessage(ctx, task.ID, api.PostMessageRequest{Text: "Run guarded resolution", AuditKind: api.MessageAuditIntake, RequestID: "run-resolve-intake"}, by)
	if err != nil {
		t.Fatal(err)
	}
	resolve := api.ResolveMessageAuditRequest{RequestID: "run-resolve", ExpectedRevision: 1, Reason: "Resolve from the exact run.", AgentID: agent.ID,
		Sources: correction.Sources, ExistingItem: &api.MessageWorkItem{ItemTaskID: task.ID, ItemID: item.ID, ItemRevision: item.Revision, Relationship: "primary"}}
	if _, _, err := s.ResolveMessageAudit(ctx, task.ID, resolveIntake.Seq, resolve, by); !errors.Is(err, api.ErrInvalid) {
		t.Fatalf("missing resolve run=%v", err)
	}
	resolve.RunID = api.NewID("run")
	if _, _, err := s.ResolveMessageAudit(ctx, task.ID, resolveIntake.Seq, resolve, by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("stale resolve run=%v", err)
	}
	resolve.RunID = agent.RunID
	resolved, replay, err := s.ResolveMessageAudit(ctx, task.ID, resolveIntake.Seq, resolve, by)
	if err != nil || replay {
		t.Fatalf("current resolve run: %+v replay=%v err=%v", resolved, replay, err)
	}
	closed := api.AgentClosed
	if _, err := s.UpdateAgent(ctx, agent.ID, api.UpdateAgentRequest{Status: &closed}, by); err != nil {
		t.Fatal(err)
	}
	closedResolve, replay, err := s.ResolveMessageAudit(ctx, task.ID, resolveIntake.Seq, resolve, by)
	if err != nil || !replay || closedResolve.Receipt.ID != resolved.Receipt.ID {
		t.Fatalf("closed exact resolve replay: %+v replay=%v err=%v", closedResolve, replay, err)
	}
}

func TestMessageAuditA2ReconcilesOldBinaryWritesAfterMigration(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "a2-old-binary.sqlite")
	by := api.Caller{Node: "old-binary-node", User: "old-binary-user"}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	task, _ := workItemProject(t, s, ctx, by, "A2 old binary", "lead")
	item := a2Item(t, s, ctx, by, task, "old-binary-item")
	now := ts(s.now())
	result, err := s.db.ExecContext(ctx, `INSERT INTO messages(task_id,from_agent,from_node,from_user,to_agent,text,created_at,reply_to,broadcast)
VALUES(?,?,?,?,?,'Old binary contextual post',?,0,0)`, task.ID, "", by.Node, by.User, "", now)
	if err != nil {
		t.Fatal(err)
	}
	linkedSeq, _ := result.LastInsertId()
	if _, err := s.db.ExecContext(ctx, `INSERT INTO message_work_item_links
(message_seq,message_task_id,item_task_id,item_id,item_revision,relationship,created_at) VALUES(?,?,?,?,?,'primary',?)`,
		linkedSeq, task.ID, task.ID, item.ID, item.Revision, now); err != nil {
		t.Fatal(err)
	}
	result, err = s.db.ExecContext(ctx, `INSERT INTO messages(task_id,from_agent,from_node,from_user,to_agent,text,created_at,reply_to,broadcast)
VALUES(?,?,?,?,?,'Old binary unlinked post',?,0,0)`, task.ID, "", by.Node, by.User, "", now)
	if err != nil {
		t.Fatal(err)
	}
	unlinkedSeq, _ := result.LastInsertId()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	linked, err := s.GetMessageAudit(ctx, task.ID, linkedSeq)
	if err != nil || linked.Current == nil || linked.Current.Revision != 1 || linked.Current.Classification != api.MessageAuditWork ||
		linked.Original.Classification != api.MessageAuditWork || linked.Original.WorkItems[0].ItemID != item.ID {
		t.Fatalf("old binary link not reconciled: %+v err=%v", linked, err)
	}
	unlinked, err := s.GetMessageAudit(ctx, task.ID, unlinkedSeq)
	if err != nil || unlinked.Current != nil || unlinked.Original.Classification != api.MessageAuditUnclassified {
		t.Fatalf("old binary unlinked row relabelled: %+v err=%v", unlinked, err)
	}
	historical := api.CorrectMessageAuditRequest{RequestID: "historical-explicit-correction", ExpectedRevision: 0,
		Reason: "Retained exact evidence classifies this old message.", Sources: []api.MessageReference{{TaskID: task.ID, Seq: linkedSeq}},
		Desired: api.MessageAuditDesiredState{Classification: api.MessageAuditWork, WorkItems: []api.MessageWorkItem{{ItemTaskID: task.ID, ItemID: item.ID, ItemRevision: item.Revision, Relationship: "primary"}}}}
	historicalResult, replay, err := s.CorrectMessageAudit(ctx, task.ID, unlinkedSeq, historical, by)
	if err != nil || replay || historicalResult.State.Original.Classification != api.MessageAuditUnclassified || historicalResult.State.Current.Revision != 1 || historicalResult.Event.Before != nil {
		t.Fatalf("historical correction: %+v replay=%v err=%v", historicalResult, replay, err)
	}
	history, err := s.ListMessageAuditHistory(ctx, task.ID, linkedSeq, 0, "", 10)
	if err != nil || len(history.Events) != 1 || history.Events[0].Operation != "baseline" || history.Events[0].Provenance != "validated_a1_migration" || history.Events[0].Actor != (api.MessageAuditActor{}) || len(history.Events[0].Sources) != 1 || history.Events[0].Sources[0].Seq != linkedSeq {
		t.Fatalf("migration provenance: %+v err=%v", history, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var baselines int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM message_audit_events WHERE message_task_id=? AND message_seq=?`, task.ID, linkedSeq).Scan(&baselines); err != nil || baselines != 1 {
		t.Fatalf("reopen duplicated baseline count=%d err=%v", baselines, err)
	}
	var violations int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM pragma_foreign_key_check`).Scan(&violations); err != nil || violations != 0 {
		t.Fatalf("foreign key violations=%d err=%v", violations, err)
	}
}

func TestMessageAuditA2RelatedLimit(t *testing.T) {
	s, ctx, by := workItemStore(t)
	task, _ := workItemProject(t, s, ctx, by, "A2 related limit", "lead")
	links := make([]api.MessageWorkItem, 0, api.MaxMessageAuditRelated+2)
	for index := 0; index < api.MaxMessageAuditRelated+2; index++ {
		item := a2Item(t, s, ctx, by, task, fmt.Sprintf("limit-%02d", index))
		relationship := "related"
		if index == 0 {
			relationship = "primary"
		}
		links = append(links, api.MessageWorkItem{ItemTaskID: task.ID, ItemID: item.ID, ItemRevision: item.Revision, Relationship: relationship})
	}
	valid := api.PostMessageRequest{Text: "Maximum related", AuditKind: api.MessageAuditWork, RequestID: "related-valid", WorkItems: links[:api.MaxMessageAuditRelated+1]}
	if _, err := s.PostMessage(ctx, task.ID, valid, by); err != nil {
		t.Fatalf("maximum legal related links=%v", err)
	}
	invalid := valid
	invalid.RequestID = "related-too-many"
	invalid.WorkItems = links
	if _, err := s.PostMessage(ctx, task.ID, invalid, by); !errors.Is(err, api.ErrInvalid) {
		t.Fatalf("too many related links=%v", err)
	}
}

// Ensure the additive migration remains openable through database/sql without
// requiring the new application to rewrite or remove any A1 table.
func TestMessageAuditA2SchemaIsAdditive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a2-additive.sqlite")
	s, err := Open(path)
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
	defer db.Close()
	for _, table := range []string{"messages", "message_work_item_links", "message_post_requests", "message_audit_originals", "message_audit_states", "message_audit_links", "message_audit_events", "message_audit_receipts", "message_audit_foreign_associations"} {
		var found string
		if err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&found); err != nil {
			t.Fatalf("missing table %s: %v", table, err)
		}
	}
}

func TestMessageAuditA2ExactReadUsesOneSnapshot(t *testing.T) {
	s, ctx, by := workItemStore(t)
	s.db.SetMaxOpenConns(2)
	task, _ := workItemProject(t, s, ctx, by, "A2 snapshot read", "lead")
	source := a2Source(t, s, ctx, by, task, "Snapshot correction evidence")
	itemA := a2Item(t, s, ctx, by, task, "snapshot-a")
	itemB := a2Item(t, s, ctx, by, task, "snapshot-b")
	message, err := s.PostMessage(ctx, task.ID, api.PostMessageRequest{Text: "Snapshot original", AuditKind: api.MessageAuditWork, RequestID: "snapshot-original",
		WorkItems: []api.MessageWorkItem{{ItemTaskID: task.ID, ItemID: itemA.ID, ItemRevision: itemA.Revision, Relationship: "primary"}}}, by)
	if err != nil {
		t.Fatal(err)
	}
	correction := api.CorrectMessageAuditRequest{RequestID: "snapshot-correction", ExpectedRevision: 1, Reason: "Move to item B while the old snapshot is open.",
		Sources: []api.MessageReference{{TaskID: task.ID, Seq: source.Seq}}, Desired: api.MessageAuditDesiredState{Classification: api.MessageAuditWork,
			WorkItems: []api.MessageWorkItem{{ItemTaskID: task.ID, ItemID: itemB.ID, ItemRevision: itemB.Revision, Relationship: "primary"}}}}

	headerRead := make(chan struct{})
	releaseRead := make(chan struct{})
	var hookOnce sync.Once
	readCtx := context.WithValue(ctx, messageAuditReadStateHookKey{}, func() {
		hookOnce.Do(func() { close(headerRead) })
		<-releaseRead
	})
	type readResult struct {
		record api.MessageAuditRecord
		err    error
	}
	readDone := make(chan readResult, 1)
	go func() {
		record, readErr := s.GetMessageAudit(readCtx, task.ID, message.Seq)
		readDone <- readResult{record: record, err: readErr}
	}()
	select {
	case <-headerRead:
	case <-time.After(2 * time.Second):
		close(releaseRead)
		t.Fatal("snapshot read did not reach the state/link boundary")
	}
	writeDone := make(chan error, 1)
	go func() {
		_, _, writeErr := s.CorrectMessageAudit(ctx, task.ID, message.Seq, correction, by)
		writeDone <- writeErr
	}()
	select {
	case err := <-writeDone:
		if err != nil {
			close(releaseRead)
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		close(releaseRead)
		t.Fatal("concurrent correction did not commit while read snapshot was open")
	}
	close(releaseRead)
	old := <-readDone
	if old.err != nil || old.record.Current == nil || old.record.Current.Revision != 1 || len(old.record.Current.WorkItems) != 1 || old.record.Current.WorkItems[0].ItemID != itemA.ID {
		t.Fatalf("torn snapshot result: %+v err=%v", old.record, old.err)
	}
	current, err := s.GetMessageAudit(ctx, task.ID, message.Seq)
	if err != nil || current.Current == nil || current.Current.Revision != 2 || current.Current.WorkItems[0].ItemID != itemB.ID {
		t.Fatalf("fresh snapshot did not see correction: %+v err=%v", current, err)
	}
}

func TestMessageAuditA2PreservesLegacyBoundInboxScope(t *testing.T) {
	s, ctx, by := workItemStore(t)
	task, _ := workItemProject(t, s, ctx, by, "A2 legacy bound routing", "lead")
	itemA := a2Item(t, s, ctx, by, task, "legacy-a")
	itemB := a2Item(t, s, ctx, by, task, "legacy-b")
	orderA := contextLinkedMessage(t, s, task, itemA, "Item A work order", "legacy-order-a", nil)
	orderB := contextLinkedMessage(t, s, task, itemB, "Item B work order", "legacy-order-b", nil)
	workerA, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "legacy-worker-a", Host: "fixture", Session: "legacy-worker-a", Runtime: "codex", WorkItem: &api.AgentWorkItemRequest{
		ItemTaskID: task.ID, ItemID: itemA.ID, ItemRevision: itemA.Revision, WorkOrderMessage: api.MessageReference{TaskID: task.ID, Seq: orderA.Seq},
		ContextBundle: preparedContextFromAcceptedHistory(t, s, itemA, api.MessageReference{TaskID: task.ID, Seq: orderA.Seq}),
	}}, by)
	if err != nil {
		t.Fatal(err)
	}
	workerB, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "legacy-worker-b", Host: "fixture", Session: "legacy-worker-b", Runtime: "codex", WorkItem: &api.AgentWorkItemRequest{
		ItemTaskID: task.ID, ItemID: itemB.ID, ItemRevision: itemB.Revision, WorkOrderMessage: api.MessageReference{TaskID: task.ID, Seq: orderB.Seq},
		ContextBundle: preparedContextFromAcceptedHistory(t, s, itemB, api.MessageReference{TaskID: task.ID, Seq: orderB.Seq}),
	}}, by)
	if err != nil {
		t.Fatal(err)
	}
	target, err := s.PostMessage(ctx, task.ID, api.PostMessageRequest{Text: "Immutable item A message", AuditKind: api.MessageAuditWork, RequestID: "legacy-scope-target",
		WorkItems: []api.MessageWorkItem{{ItemTaskID: task.ID, ItemID: itemA.ID, ItemRevision: itemA.Revision, Relationship: "primary"}}}, by)
	if err != nil {
		t.Fatal(err)
	}
	move := api.CorrectMessageAuditRequest{RequestID: "legacy-scope-move", ExpectedRevision: 1, Reason: "Current audit authority moves to B.",
		Sources: []api.MessageReference{{TaskID: task.ID, Seq: orderA.Seq}}, Desired: api.MessageAuditDesiredState{Classification: api.MessageAuditWork,
			WorkItems: []api.MessageWorkItem{{ItemTaskID: task.ID, ItemID: itemB.ID, ItemRevision: itemB.Revision, Relationship: "primary"}}}}
	if _, _, err := s.CorrectMessageAudit(ctx, task.ID, target.Seq, move, by); err != nil {
		t.Fatal(err)
	}
	aInbox, err := s.ListMessages(ctx, task.ID, target.Seq-1, workerA.ID, 10)
	if err != nil || len(aInbox) != 1 || aInbox[0].Seq != target.Seq || aInbox[0].WorkItems[0].ItemID != itemA.ID {
		t.Fatalf("original A-bound inbox changed after move: %+v err=%v", aInbox, err)
	}
	bInbox, err := s.ListMessages(ctx, task.ID, target.Seq-1, workerB.ID, 10)
	if err != nil || len(bInbox) != 0 {
		t.Fatalf("B-bound legacy inbox gained current-only message: %+v err=%v", bInbox, err)
	}
	detach := api.CorrectMessageAuditRequest{RequestID: "legacy-scope-detach", ExpectedRevision: 2, Reason: "Current audit authority returns to Intake.",
		Sources: []api.MessageReference{{TaskID: task.ID, Seq: orderA.Seq}}, Desired: api.MessageAuditDesiredState{Classification: api.MessageAuditIntake}}
	if _, _, err := s.CorrectMessageAudit(ctx, task.ID, target.Seq, detach, by); err != nil {
		t.Fatal(err)
	}
	aInbox, err = s.ListMessages(ctx, task.ID, target.Seq-1, workerA.ID, 10)
	if err != nil || len(aInbox) != 1 || aInbox[0].WorkItems[0].ItemID != itemA.ID {
		t.Fatalf("original A-bound inbox changed after detach: %+v err=%v", aInbox, err)
	}
	bInbox, err = s.ListMessages(ctx, task.ID, target.Seq-1, workerB.ID, 10)
	if err != nil || len(bInbox) != 0 {
		t.Fatalf("B-bound legacy inbox changed after detach: %+v err=%v", bInbox, err)
	}
	for _, worker := range []api.Agent{workerA, workerB} {
		later, listErr := s.ListMessages(ctx, task.ID, target.Seq, worker.ID, 10)
		if listErr != nil || len(later) != 0 {
			t.Fatalf("correction leaked through message cursor for %s: %+v err=%v", worker.Name, later, listErr)
		}
	}
	replayed, err := s.GetMessagePostReceipt(ctx, task.ID, "legacy-scope-target", "", by)
	current, currentErr := s.GetMessageAudit(ctx, task.ID, target.Seq)
	if err != nil || currentErr != nil || replayed.WorkItems[0].ItemID != itemA.ID || current.Current == nil || current.Current.Classification != api.MessageAuditIntake || len(current.Current.WorkItems) != 0 {
		t.Fatalf("legacy/original versus explicit current mismatch: replay=%+v current=%+v err=%v currentErr=%v", replayed, current, err, currentErr)
	}
}
