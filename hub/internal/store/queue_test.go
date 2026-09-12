package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/scs32/tailterm/hub/internal/api"
)

func queueOrder(t *testing.T, s *Store, item api.WorkItem, text, key string) api.Message {
	t.Helper()
	message, err := s.PostMessage(context.Background(), item.TaskID, api.PostMessageRequest{
		Text: text, RequestID: key, AuditKind: api.MessageAuditWork,
		WorkItems: []api.MessageWorkItem{{ItemTaskID: item.TaskID, ItemID: item.ID, ItemRevision: item.Revision, Relationship: "primary"}},
	}, api.Caller{Node: "fixture", User: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	return message
}

func TestQueueConcurrentDispatchDedupAndClaimCAS(t *testing.T) {
	s, ctx, by := workItemStore(t)
	source, _ := workItemProject(t, s, ctx, by, "Concurrent source", "sourcelead")
	target, lead := workItemProject(t, s, ctx, by, "Concurrent target", "targetlead")
	item := createWorkItem(t, s, ctx, by, source, "concurrent-item")
	type sendResult struct {
		result api.WorkItemDispatchResult
		err    error
	}
	sends := make(chan sendResult, 3)
	var group sync.WaitGroup
	for _, key := range []string{"concurrent-a", "concurrent-b", "concurrent-c"} {
		group.Add(1)
		go func(key string) {
			defer group.Done()
			result, err := s.DispatchWorkItem(ctx, source.ID, item.ID, api.DispatchWorkItemRequest{Revision: item.Revision, TargetTaskID: target.ID, RequestID: key}, by)
			sends <- sendResult{result, err}
		}(key)
	}
	group.Wait()
	close(sends)
	entryID, messageSeq := "", int64(0)
	for send := range sends {
		if send.err != nil {
			t.Fatal(send.err)
		}
		if entryID == "" {
			entryID, messageSeq = send.result.Queue.Entry.ID, send.result.Dispatch.MessageSeq
		}
		if send.result.Queue.Entry.ID != entryID || send.result.Dispatch.MessageSeq != messageSeq {
			t.Fatalf("concurrent sends split identity: %+v", send.result)
		}
	}
	var entries, links, notices int
	if err := s.db.QueryRow(`SELECT (SELECT count(*) FROM queue_entries),(SELECT count(*) FROM queue_dispatch_links),(SELECT count(*) FROM queue_notifications)`).Scan(&entries, &links, &notices); err != nil || entries != 1 || links != 3 || notices != 1 {
		t.Fatalf("concurrent send counts entries=%d links=%d notices=%d err=%v", entries, links, notices, err)
	}
	entry, err := s.GetQueueEntry(ctx, target.ID, entryID)
	if err != nil {
		t.Fatal(err)
	}
	order := queueOrder(t, s, item, "Concurrent claim order", "concurrent-order")
	claims := make(chan error, 2)
	for _, key := range []string{"concurrent-claim-a", "concurrent-claim-b"} {
		group.Add(1)
		go func(key string) {
			defer group.Done()
			_, claimErr := s.QueueAction(ctx, target.ID, entry.ID, api.QueueActionRequest{Operation: "claim", RequestID: key, ExpectedRevision: entry.Revision, Cycle: entry.Cycle, ExpectedItemRevision: item.Revision, ClaimantAgentID: lead.ID, ClaimantRunID: lead.RunID, WorkOrderMessage: &api.MessageReference{TaskID: source.ID, Seq: order.Seq}}, by)
			claims <- claimErr
		}(key)
	}
	group.Wait()
	close(claims)
	wins, conflicts := 0, 0
	for claimErr := range claims {
		if claimErr == nil {
			wins++
		} else if errors.Is(claimErr, api.ErrConflict) {
			conflicts++
		} else {
			t.Fatalf("unexpected claim error: %v", claimErr)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatalf("claim CAS wins=%d conflicts=%d", wins, conflicts)
	}
}

func TestQueueClaimRejectsEnqueueNoticesAndAdmissionRevalidatesOrder(t *testing.T) {
	s, ctx, by := workItemStore(t)
	source, _ := workItemProject(t, s, ctx, by, "Order authority source", "sourcelead")
	target, lead := workItemProject(t, s, ctx, by, "Order authority target", "targetlead")
	item := createWorkItem(t, s, ctx, by, source, "order-authority")
	sent, err := s.DispatchWorkItem(ctx, source.ID, item.ID, api.DispatchWorkItemRequest{Revision: item.Revision, TargetTaskID: target.ID, RequestID: "order-authority-send"}, by)
	if err != nil {
		t.Fatal(err)
	}
	claimFromDispatch := func(key string) error {
		_, claimErr := s.QueueAction(ctx, target.ID, sent.Queue.Entry.ID, api.QueueActionRequest{Operation: "claim", RequestID: key, ExpectedRevision: sent.Queue.Entry.Revision, Cycle: sent.Queue.Entry.Cycle, ExpectedItemRevision: item.Revision, ClaimantAgentID: lead.ID, ClaimantRunID: lead.RunID, WorkOrderMessage: &api.MessageReference{TaskID: target.ID, Seq: sent.Dispatch.MessageSeq}}, by)
		return claimErr
	}
	if err = claimFromDispatch("order-authority-typed"); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("typed enqueue notice used as work order: %v", err)
	}
	// An older Queue-capability binary left dispatch messages untyped. Dispatch
	// provenance still prevents such a linked enqueue from becoming authority.
	if _, err = s.db.Exec(`UPDATE messages SET system_notice_kind='',system_notice_id='' WHERE task_id=? AND seq=?`, target.ID, sent.Dispatch.MessageSeq); err != nil {
		t.Fatal(err)
	}
	if err = claimFromDispatch("order-authority-legacy"); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("legacy dispatch message used as work order: %v", err)
	}

	order := queueOrder(t, s, item, "Separately bounded implementation order", "order-authority-valid")
	claim, err := s.QueueAction(ctx, target.ID, sent.Queue.Entry.ID, api.QueueActionRequest{Operation: "claim", RequestID: "order-authority-claim", ExpectedRevision: sent.Queue.Entry.Revision, Cycle: sent.Queue.Entry.Cycle, ExpectedItemRevision: item.Revision, ClaimantAgentID: lead.ID, ClaimantRunID: lead.RunID, WorkOrderMessage: &api.MessageReference{TaskID: source.ID, Seq: order.Seq}}, by)
	if err != nil || claim.Entry.State != api.QueueStateClaimed {
		t.Fatalf("separately bounded order rejected: %+v %v", claim, err)
	}
	bundle := syntheticPreparedContext(t, item, api.MessageReference{TaskID: source.ID, Seq: order.Seq}, syntheticHistory(item, order))
	work := api.AgentWorkItemRequest{ItemTaskID: source.ID, ItemID: item.ID, ItemRevision: item.Revision, WorkOrderMessage: api.MessageReference{TaskID: source.ID, Seq: order.Seq}, QueueClaim: &api.QueueAdmissionClaim{EntryID: claim.Entry.ID, Cycle: claim.Entry.Cycle, ExpectedRevision: claim.Entry.Revision, ClaimantAgentID: lead.ID, ClaimantRunID: lead.RunID}, ContextBundle: bundle}
	// Admission independently validates the retained order, preventing a legacy
	// or externally restored claimed row from bypassing the claim-time guard.
	if _, err = s.db.Exec(`UPDATE messages SET system_notice_kind=?,system_notice_id=? WHERE task_id=? AND seq=?`, api.QueueNoticeChanged, api.NewID("qnt"), source.ID, order.Seq); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AddAgent(ctx, target.ID, api.AddAgentRequest{Name: "invalid-order-worker", Host: "fixture", Session: "invalid-order-worker", Runtime: "codex", WorkItem: &work}, by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("invalid retained work order reached cross-project admission: %v", err)
	}
	entry, err := s.GetQueueEntry(ctx, target.ID, claim.Entry.ID)
	if err != nil || entry.State != api.QueueStateClaimed || entry.WorkerAgentID != "" || entry.WorkerRunID != "" {
		t.Fatalf("rejected admission changed claim: %+v %v", entry, err)
	}
}

func TestQueueDispatchCASClaimStartTerminalAndSourcePreservation(t *testing.T) {
	s, ctx, by := workItemStore(t)
	source, _ := workItemProject(t, s, ctx, by, "Queue source", "sourcelead")
	target, lead := workItemProject(t, s, ctx, by, "Queue target", "targetlead")
	handler, err := s.AddAgent(ctx, target.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: "database", Host: "fixture", Session: "database", Runtime: "codex", Role: api.AgentRoleDatabaseHandler}, by)
	if err != nil {
		t.Fatal(err)
	}
	item := createWorkItem(t, s, ctx, by, source, "queue-item")
	original := item
	dispatched, err := s.DispatchWorkItem(ctx, source.ID, item.ID, api.DispatchWorkItemRequest{Revision: item.Revision, TargetTaskID: target.ID, RequestID: "queue-send-1"}, by)
	if err != nil {
		t.Fatal(err)
	}
	if dispatched.Queue == nil || dispatched.Queue.Entry.State != api.QueueStateWaiting || !dispatched.Queue.Entry.Eligible || dispatched.Queue.Notification == nil || dispatched.Queue.Notification.MessageSeq != dispatched.Dispatch.MessageSeq {
		t.Fatalf("dispatch queue receipt: %+v", dispatched)
	}
	if dispatched.Queue.Notification.CausalAuthor.Node != by.Node || dispatched.Queue.Notification.CausalAuthor.User != by.User || dispatched.Queue.Notification.RecipientAgentID != lead.ID || dispatched.Queue.Notification.RecipientRunID != lead.RunID {
		t.Fatalf("Queue notice provenance/recipient: %+v", dispatched.Queue.Notification)
	}
	messages, err := s.ListMessages(ctx, target.ID, 0, "", 10)
	if err != nil || len(messages) != 1 || messages[0].SystemNotice == nil || messages[0].SystemNotice.Queue.EntryID != dispatched.Queue.Entry.ID || messages[0].From.Node != "system" {
		t.Fatalf("typed queue notice: %+v %v", messages, err)
	}
	replay, err := s.DispatchWorkItem(ctx, source.ID, item.ID, api.DispatchWorkItemRequest{Revision: item.Revision, TargetTaskID: target.ID, RequestID: "queue-send-1"}, by)
	if err != nil || replay.Queue == nil || !replay.Queue.Replay || replay.Dispatch.ID != dispatched.Dispatch.ID {
		t.Fatalf("dispatch replay: %+v %v", replay, err)
	}
	duplicate, err := s.DispatchWorkItem(ctx, source.ID, item.ID, api.DispatchWorkItemRequest{Revision: item.Revision, TargetTaskID: target.ID, RequestID: "queue-send-2"}, by)
	if err != nil || duplicate.Queue.Event.Kind != "dispatch_linked" || duplicate.Queue.Entry.ID != dispatched.Queue.Entry.ID || duplicate.Queue.Entry.Revision != dispatched.Queue.Entry.Revision || duplicate.Queue.Notification != nil {
		t.Fatalf("new-key duplicate: %+v %v", duplicate, err)
	}
	messages, err = s.ListMessages(ctx, target.ID, 0, "", 10)
	if err != nil || len(messages) != 1 || duplicate.Dispatch.MessageSeq != dispatched.Dispatch.MessageSeq {
		t.Fatalf("duplicate semantic Queue notice: messages=%d dispatch=%+v err=%v", len(messages), duplicate.Dispatch, err)
	}
	list, err := s.ListQueue(ctx, target.ID, "", 32, false)
	if err != nil || len(list.Entries) != 1 || !list.Complete {
		t.Fatalf("queue list: %+v %v", list, err)
	}
	entry := list.Entries[0]
	priority, err := s.QueueAction(ctx, target.ID, entry.ID, api.QueueActionRequest{Operation: "priority", RequestID: "priority-1", ExpectedRevision: entry.Revision, Cycle: entry.Cycle, Priority: "urgent"}, by)
	if err != nil || priority.Entry.QueuePriority != "urgent" || priority.Entry.Revision != entry.Revision+1 {
		t.Fatalf("priority: %+v %v", priority, err)
	}
	sourceAfterPriority, err := s.GetWorkItem(ctx, source.ID, item.ID)
	if err != nil || sourceAfterPriority.Revision != item.Revision || sourceAfterPriority.Priority != item.Priority || sourceAfterPriority.Status != item.Status {
		t.Fatalf("Queue priority changed source item: %+v %v", sourceAfterPriority, err)
	}
	priorityReplay, err := s.QueueAction(ctx, target.ID, entry.ID, api.QueueActionRequest{Operation: "priority", RequestID: "priority-1", ExpectedRevision: entry.Revision, Cycle: entry.Cycle, Priority: "urgent"}, by)
	if err != nil || !priorityReplay.Replay || priorityReplay.Event.Seq != priority.Event.Seq {
		t.Fatalf("priority replay: %+v %v", priorityReplay, err)
	}
	if _, err = s.QueueAction(ctx, target.ID, entry.ID, api.QueueActionRequest{Operation: "priority", RequestID: "priority-stale", ExpectedRevision: entry.Revision, Cycle: entry.Cycle, Priority: "low"}, by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("stale priority: %v", err)
	}

	order := queueOrder(t, s, item, "Bounded Queue order", "queue-order")
	claim, err := s.QueueAction(ctx, target.ID, entry.ID, api.QueueActionRequest{Operation: "claim", RequestID: "claim-1", ExpectedRevision: priority.Entry.Revision, Cycle: entry.Cycle, ExpectedItemRevision: item.Revision, ClaimantAgentID: lead.ID, ClaimantRunID: lead.RunID, WorkOrderMessage: &api.MessageReference{TaskID: source.ID, Seq: order.Seq}}, by)
	if err != nil || claim.Entry.State != api.QueueStateClaimed || claim.Entry.ClaimedItemRevision != item.Revision {
		t.Fatalf("claim: %+v %v", claim, err)
	}
	if _, err = s.QueueAction(ctx, target.ID, entry.ID, api.QueueActionRequest{Operation: "claim", RequestID: "claim-race", ExpectedRevision: priority.Entry.Revision, Cycle: entry.Cycle, ExpectedItemRevision: item.Revision, ClaimantAgentID: lead.ID, ClaimantRunID: lead.RunID, WorkOrderMessage: &api.MessageReference{TaskID: source.ID, Seq: order.Seq}}, by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("claim race: %v", err)
	}

	bundle := syntheticPreparedContext(t, item, api.MessageReference{TaskID: source.ID, Seq: order.Seq}, syntheticHistory(item, order))
	workerID := api.NewID("agt")
	workerRequest := api.AddAgentRequest{AgentID: workerID, Name: "queue-worker", Host: "fixture", Session: "queue-worker", Runtime: "codex", WorkItem: &api.AgentWorkItemRequest{ItemTaskID: source.ID, ItemID: item.ID, ItemRevision: item.Revision, WorkOrderMessage: api.MessageReference{TaskID: source.ID, Seq: order.Seq}, QueueClaim: &api.QueueAdmissionClaim{EntryID: entry.ID, Cycle: entry.Cycle, ExpectedRevision: claim.Entry.Revision, ClaimantAgentID: lead.ID, ClaimantRunID: lead.RunID}, ContextBundle: bundle}}
	worker, err := s.AddAgent(ctx, target.ID, workerRequest, by)
	if err != nil {
		t.Fatal(err)
	}
	admitted, err := s.GetQueueEntry(ctx, target.ID, entry.ID)
	if err != nil || admitted.WorkerAgentID != worker.ID || admitted.State != api.QueueStateClaimed {
		t.Fatalf("admission pin: %+v %v", admitted, err)
	}
	recovered, err := s.AddAgent(ctx, target.ID, workerRequest, by)
	if err != nil || recovered.ID != worker.ID || recovered.RunID != worker.RunID || recovered.WorkItem.ContextDigest != worker.WorkItem.ContextDigest {
		t.Fatalf("exact uncertain admission retry: %+v %v", recovered, err)
	}
	duplicateRequest := workerRequest
	duplicateRequest.AgentID, duplicateRequest.Name, duplicateRequest.Session = "", "queue-worker-duplicate", "queue-worker-duplicate"
	if _, err = s.AddAgent(ctx, target.ID, duplicateRequest, by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("duplicate admission after unknown result: %v", err)
	}
	start, err := s.QueueAction(ctx, target.ID, entry.ID, api.QueueActionRequest{Operation: "start", RequestID: "start-1", ExpectedRevision: admitted.Revision, Cycle: entry.Cycle, WorkerAgentID: worker.ID, WorkerRunID: worker.RunID, ContextDigest: worker.WorkItem.ContextDigest}, by)
	if err != nil || start.Entry.State != api.QueueStateActive || start.Entry.WorkerRunID != worker.RunID {
		t.Fatalf("start: %+v %v", start, err)
	}

	title := "Newer exact scope"
	item, err = s.UpdateWorkItem(ctx, source.ID, item.ID, api.UpdateWorkItemRequest{Revision: item.Revision, Title: &title}, by)
	if err != nil {
		t.Fatal(err)
	}
	stale, err := s.GetQueueEntry(ctx, target.ID, entry.ID)
	if err != nil || !stale.Stale || stale.OfferedItemRevision == stale.CurrentItemRevision || stale.State != api.QueueStateActive {
		t.Fatalf("stale active entry: %+v %v", stale, err)
	}
	newOffer, err := s.DispatchWorkItem(ctx, source.ID, item.ID, api.DispatchWorkItemRequest{Revision: item.Revision, TargetTaskID: target.ID, RequestID: "queue-send-new"}, by)
	if err != nil || !newOffer.Queue.Entry.PendingUpdate || newOffer.Queue.Entry.ClaimedItemRevision != original.Revision || newOffer.Queue.Entry.State != api.QueueStateActive {
		t.Fatalf("pending active offer: %+v %v", newOffer, err)
	}
	done := "done"
	item, err = s.UpdateWorkItem(ctx, source.ID, item.ID, api.UpdateWorkItemRequest{Revision: item.Revision, Status: &done}, by)
	if err != nil {
		t.Fatal(err)
	}
	terminal, err := s.GetQueueEntry(ctx, target.ID, entry.ID)
	if err != nil || terminal.State != api.QueueStateCompleted || terminal.WorkerRunID != worker.RunID || terminal.TerminalItemRevision != item.Revision {
		t.Fatalf("terminal entry: %+v %v", terminal, err)
	}
	current, err := s.GetWorkItem(ctx, source.ID, item.ID)
	if err != nil || current.Priority != original.Priority || current.Status != "done" || current.TaskID != source.ID {
		t.Fatalf("source changed by queue action: %+v %v", current, err)
	}
	history, err := s.ListQueueHistory(ctx, target.ID, entry.ID, "", 64)
	if err != nil || !history.Complete || len(history.Events) < 7 {
		t.Fatalf("queue history: %d %+v %v", len(history.Events), history, err)
	}

	selection, err := s.PostMessage(ctx, target.ID, api.PostMessageRequest{To: handler.ID, Text: "Retained human selection"}, by)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.QueueAction(ctx, target.ID, entry.ID, api.QueueActionRequest{Operation: "priority", RequestID: "handler-after-terminal", ExpectedRevision: terminal.Revision, Cycle: terminal.Cycle, Priority: "low", AgentID: handler.ID, RunID: handler.RunID, Selection: &api.QueueSelection{TaskID: target.ID, MessageSeq: selection.Seq}}, by); !errors.Is(err, api.ErrInvalid) {
		t.Fatalf("terminal handler mutation: %v", err)
	}
}

func TestCrossProjectQueueAdmissionRejectsAnythingButExactClaim(t *testing.T) {
	s, ctx, by := workItemStore(t)
	source, _ := workItemProject(t, s, ctx, by, "Admission source", "sourcelead")
	target, lead := workItemProject(t, s, ctx, by, "Admission target", "targetlead")
	item := createWorkItem(t, s, ctx, by, source, "admission-guards")
	order := queueOrder(t, s, item, "Exact admission order", "admission-order")
	bundle := syntheticPreparedContext(t, item, api.MessageReference{TaskID: source.ID, Seq: order.Seq}, syntheticHistory(item, order))
	dispatch, err := s.DispatchWorkItem(ctx, source.ID, item.ID, api.DispatchWorkItemRequest{Revision: item.Revision, TargetTaskID: target.ID, RequestID: "admission-send"}, by)
	if err != nil {
		t.Fatal(err)
	}
	baseWork := api.AgentWorkItemRequest{ItemTaskID: source.ID, ItemID: item.ID, ItemRevision: item.Revision, WorkOrderMessage: api.MessageReference{TaskID: source.ID, Seq: order.Seq}, ContextBundle: bundle}
	waitingWork := baseWork
	waitingWork.QueueClaim = &api.QueueAdmissionClaim{EntryID: dispatch.Queue.Entry.ID, Cycle: 1, ExpectedRevision: dispatch.Queue.Entry.Revision, ClaimantAgentID: lead.ID, ClaimantRunID: lead.RunID}
	if _, err = s.AddAgent(ctx, target.ID, api.AddAgentRequest{Name: "waiting-rejected", Host: "fixture", Session: "waiting-rejected", Runtime: "codex", WorkItem: &waitingWork}, by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("waiting dispatch admitted without claim: %v", err)
	}
	claim, err := s.QueueAction(ctx, target.ID, dispatch.Queue.Entry.ID, api.QueueActionRequest{Operation: "claim", RequestID: "admission-claim", ExpectedRevision: dispatch.Queue.Entry.Revision, Cycle: 1, ExpectedItemRevision: item.Revision, ClaimantAgentID: lead.ID, ClaimantRunID: lead.RunID, WorkOrderMessage: &baseWork.WorkOrderMessage}, by)
	if err != nil {
		t.Fatal(err)
	}
	exact := api.QueueAdmissionClaim{EntryID: claim.Entry.ID, Cycle: claim.Entry.Cycle, ExpectedRevision: claim.Entry.Revision, ClaimantAgentID: lead.ID, ClaimantRunID: lead.RunID}
	wrongClaims := []struct {
		name string
		edit func(*api.QueueAdmissionClaim)
	}{
		{"wrong-entry", func(value *api.QueueAdmissionClaim) { value.EntryID = api.NewID("que") }},
		{"wrong-cycle", func(value *api.QueueAdmissionClaim) { value.Cycle++ }},
		{"wrong-entry-revision", func(value *api.QueueAdmissionClaim) { value.ExpectedRevision++ }},
		{"wrong-claimant", func(value *api.QueueAdmissionClaim) { value.ClaimantAgentID = api.NewID("agt") }},
		{"wrong-claimant-run", func(value *api.QueueAdmissionClaim) { value.ClaimantRunID = api.NewID("run") }},
	}
	for _, test := range wrongClaims {
		t.Run(test.name, func(t *testing.T) {
			candidate := exact
			test.edit(&candidate)
			work := baseWork
			work.QueueClaim = &candidate
			_, addErr := s.AddAgent(ctx, target.ID, api.AddAgentRequest{Name: test.name, Host: "fixture", Session: test.name, Runtime: "codex", WorkItem: &work}, by)
			if addErr == nil {
				t.Fatal("inexact Queue claim admitted a worker")
			}
		})
	}
	if _, err = s.AddAgent(ctx, target.ID, api.AddAgentRequest{Name: "missing-claim", Host: "fixture", Session: "missing-claim", Runtime: "codex", WorkItem: &baseWork}, by); err == nil {
		t.Fatal("cross-project item admitted without Queue metadata")
	}
	if _, err = s.CloseAgent(ctx, lead.ID, by); err != nil {
		t.Fatal(err)
	}
	replacement, err := s.AddAgent(ctx, target.ID, api.AddAgentRequest{Name: lead.Name, Host: "fixture", Session: "admission-replacement", Runtime: "codex"}, by)
	if err != nil {
		t.Fatal(err)
	}
	transferred, err := s.QueueAction(ctx, target.ID, claim.Entry.ID, api.QueueActionRequest{Operation: "transfer", RequestID: "admission-transfer", ExpectedRevision: claim.Entry.Revision, Cycle: claim.Entry.Cycle, Reason: "orchestrator replaced", ClaimantAgentID: replacement.ID, ClaimantRunID: replacement.RunID}, by)
	if err != nil {
		t.Fatal(err)
	}
	oldClaimWork := baseWork
	oldClaimWork.QueueClaim = &exact
	if _, err = s.AddAgent(ctx, target.ID, api.AddAgentRequest{Name: "transferred-rejected", Host: "fixture", Session: "transferred-rejected", Runtime: "codex", WorkItem: &oldClaimWork}, by); err == nil {
		t.Fatal("transferred Queue claim admitted its former claimant")
	}
	newTitle := "new immutable revision"
	item, err = s.UpdateWorkItem(ctx, source.ID, item.ID, api.UpdateWorkItemRequest{Revision: item.Revision, Title: &newTitle}, by)
	if err != nil {
		t.Fatal(err)
	}
	staleWork := baseWork
	staleWork.QueueClaim = &api.QueueAdmissionClaim{EntryID: transferred.Entry.ID, Cycle: transferred.Entry.Cycle, ExpectedRevision: transferred.Entry.Revision + 1, ClaimantAgentID: replacement.ID, ClaimantRunID: replacement.RunID}
	if _, err = s.AddAgent(ctx, target.ID, api.AddAgentRequest{Name: "stale-rejected", Host: "fixture", Session: "stale-rejected", Runtime: "codex", WorkItem: &staleWork}, by); err == nil {
		t.Fatal("stale claimed revision admitted a worker")
	}
	if _, err = s.CloseTask(ctx, source.ID, by); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AddAgent(ctx, target.ID, api.AddAgentRequest{Name: "closed-source-rejected", Host: "fixture", Session: "closed-source-rejected", Runtime: "codex", WorkItem: &staleWork}, by); err == nil {
		t.Fatal("closed source project admitted a Queue worker")
	}
	current, err := s.GetWorkItem(ctx, source.ID, item.ID)
	if err != nil || current.Revision != item.Revision || current.TaskID != source.ID || current.Status != item.Status || current.Priority != item.Priority {
		t.Fatalf("rejected admissions changed source: %+v %v", current, err)
	}
}

func TestQueueCrossTargetClosureRequeueAndLegacyReconciliation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, by := context.Background(), api.Caller{Node: "fixture", User: "owner"}
	source, _ := workItemProject(t, s, ctx, by, "Source", "sourcelead")
	targetA, _ := workItemProject(t, s, ctx, by, "Target A", "alead")
	targetB, _ := workItemProject(t, s, ctx, by, "Target B", "blead")
	item := createWorkItem(t, s, ctx, by, source, "cross-target")
	a, err := s.DispatchWorkItem(ctx, source.ID, item.ID, api.DispatchWorkItemRequest{Revision: item.Revision, TargetTaskID: targetA.ID, RequestID: "send-a"}, by)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.DispatchWorkItem(ctx, source.ID, item.ID, api.DispatchWorkItemRequest{Revision: item.Revision, TargetTaskID: targetB.ID, RequestID: "send-b"}, by)
	if err != nil {
		t.Fatal(err)
	}
	if a.Queue.Entry.ID == b.Queue.Entry.ID {
		t.Fatal("cross-target entries shared identity")
	}
	pa, err := s.QueueAction(ctx, targetA.ID, a.Queue.Entry.ID, api.QueueActionRequest{Operation: "priority", RequestID: "pa", ExpectedRevision: a.Queue.Entry.Revision, Cycle: 1, Priority: "urgent"}, by)
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.GetQueueEntry(ctx, targetB.ID, b.Queue.Entry.ID)
	if err != nil || other.QueuePriority == pa.Entry.QueuePriority {
		t.Fatalf("cross-target priority: %+v %v", other, err)
	}
	if _, err = s.CloseTask(ctx, targetA.ID, by); err != nil {
		t.Fatal(err)
	}
	closed, err := s.GetQueueEntry(ctx, targetA.ID, a.Queue.Entry.ID)
	if err != nil || closed.State != api.QueueStateCancelled || closed.TerminalReason != "target_project_closed" {
		t.Fatalf("closure: %+v %v", closed, err)
	}
	open, err := s.GetQueueEntry(ctx, targetB.ID, b.Queue.Entry.ID)
	if err != nil || open.State != api.QueueStateWaiting {
		t.Fatalf("unrelated closure: %+v %v", open, err)
	}

	// Remove only derived Queue state to reproduce a database written by an
	// older binary. Re-open must rediscover the retained dispatch without a
	// notification or invented claim.
	if _, err = s.db.Exec(`DELETE FROM queue_notifications;DELETE FROM queue_dispatch_links;DELETE FROM queue_requests;DELETE FROM queue_events;DELETE FROM queue_cycles;DELETE FROM queue_entries`); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	legacy, err := s.ListQueue(ctx, targetB.ID, "", 32, true)
	if err != nil || len(legacy.Entries) != 1 {
		t.Fatalf("legacy list: %+v %v", legacy, err)
	}
	if !legacy.Entries[0].LegacyObserved || !legacy.Entries[0].ReviewNeeded || legacy.Entries[0].Eligible || legacy.Entries[0].State != api.QueueStateWaiting {
		t.Fatalf("legacy classification: %+v", legacy.Entries[0])
	}
	var notices int
	if err = s.db.QueryRow(`SELECT count(*) FROM queue_notifications`).Scan(&notices); err != nil || notices != 0 {
		t.Fatalf("legacy notifications=%d %v", notices, err)
	}
	adopted, err := s.QueueAction(ctx, targetB.ID, legacy.Entries[0].ID, api.QueueActionRequest{Operation: "adopt", RequestID: "legacy-adopt", ExpectedRevision: legacy.Entries[0].Revision, Cycle: legacy.Entries[0].Cycle, ExpectedItemRevision: item.Revision}, by)
	if err != nil || adopted.Entry.ReviewNeeded || !adopted.Entry.Eligible || adopted.Entry.State != api.QueueStateWaiting {
		t.Fatalf("legacy adoption: %+v %v", adopted, err)
	}
	lateSnapshot, _ := json.Marshal(item)
	if _, err = s.db.Exec(`INSERT INTO work_item_dispatches(id,item_id,item_revision,snapshot,target_task_id,target_agent_id,message_seq,agent_id,by_node,by_user,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, api.NewID("wid"), item.ID, item.Revision, string(lateSnapshot), targetB.ID, b.Dispatch.TargetAgentID, b.Dispatch.MessageSeq, "", by.Node, by.User, ts(s.now())); err != nil {
		t.Fatal(err)
	}
	// A second open is idempotent.
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var entries, links int
	if err = s.db.QueryRow(`SELECT (SELECT count(*) FROM queue_entries),(SELECT count(*) FROM queue_dispatch_links)`).Scan(&entries, &links); err != nil || entries != 2 || links != 3 {
		t.Fatalf("reconcile counts entries=%d links=%d err=%v", entries, links, err)
	}
	if err = s.db.QueryRow(`SELECT count(*) FROM queue_notifications`).Scan(&notices); err != nil || notices != 1 {
		t.Fatalf("restart/late-write notification dedup=%d err=%v", notices, err)
	}
}

func TestQueueNotificationUnavailableDoesNotResumeAndExplicitlyRetargets(t *testing.T) {
	s, ctx, by := workItemStore(t)
	source, _ := workItemProject(t, s, ctx, by, "Notice source", "sourcelead")
	target, lead := workItemProject(t, s, ctx, by, "Notice target", "targetlead")
	worker, err := s.AddAgent(ctx, target.ID, api.AddAgentRequest{Name: "ordinary-worker", Host: "fixture", Session: "ordinary-worker", Runtime: "codex"}, by)
	if err != nil {
		t.Fatal(err)
	}
	item := createWorkItem(t, s, ctx, by, source, "notice-item")
	sent, err := s.DispatchWorkItem(ctx, source.ID, item.ID, api.DispatchWorkItemRequest{Revision: item.Revision, TargetTaskID: target.ID, RequestID: "notice-send"}, by)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.MarkRead(ctx, target.ID, api.MarkReadRequest{AgentID: lead.ID, UpTo: sent.Dispatch.MessageSeq}); err != nil {
		t.Fatal(err)
	}
	var afterReadEvents, afterReadNotices int
	if err = s.db.QueryRow(`SELECT (SELECT count(*) FROM queue_events WHERE entry_id=?),(SELECT count(*) FROM queue_notifications WHERE entry_id=?)`, sent.Queue.Entry.ID, sent.Queue.Entry.ID).Scan(&afterReadEvents, &afterReadNotices); err != nil || afterReadEvents != 1 || afterReadNotices != 1 {
		t.Fatalf("Queue read created an ACK loop events=%d notices=%d err=%v", afterReadEvents, afterReadNotices, err)
	}
	retired := api.AgentRetired
	if _, err = s.UpdateAgent(ctx, lead.ID, api.UpdateAgentRequest{Status: &retired}, by); err != nil {
		t.Fatal(err)
	}
	priority, err := s.QueueAction(ctx, target.ID, sent.Queue.Entry.ID, api.QueueActionRequest{Operation: "priority", RequestID: "notice-priority", ExpectedRevision: sent.Queue.Entry.Revision, Cycle: 1, Priority: "high"}, by)
	if err != nil || priority.Notification == nil || priority.Notification.Status != "unavailable" || priority.Notification.MessageSeq != 0 {
		t.Fatalf("retired notification: %+v %v", priority.Notification, err)
	}
	leadAfter, err := s.GetAgent(ctx, lead.ID)
	if err != nil || leadAfter.Status != api.AgentRetired {
		t.Fatalf("automated Queue notice resumed lead: %+v %v", leadAfter, err)
	}
	workerMessages, err := s.ListMessages(ctx, target.ID, 0, worker.ID, 64)
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range workerMessages {
		if message.SystemNotice != nil {
			t.Fatalf("ordinary worker received orchestrator Queue exception: %+v", message)
		}
	}
	if _, err = s.CloseAgent(ctx, lead.ID, by); err != nil {
		t.Fatal(err)
	}
	replacement, err := s.AddAgent(ctx, target.ID, api.AddAgentRequest{Name: lead.Name, Host: "fixture", Session: "replacement-lead", Runtime: "codex"}, by)
	if err != nil {
		t.Fatal(err)
	}
	reconciled, err := s.QueueAction(ctx, target.ID, sent.Queue.Entry.ID, api.QueueActionRequest{Operation: "reconcile_recipient", RequestID: "notice-reconcile", ExpectedRevision: priority.Entry.Revision, Cycle: 1, Reason: "orchestrator run replaced"}, by)
	if err != nil || reconciled.Entry.OrchestratorAgentID != replacement.ID || reconciled.Notification == nil || reconciled.Notification.Status != "stored" || reconciled.Notification.RecipientRunID != replacement.RunID {
		t.Fatalf("recipient reconciliation: %+v %v", reconciled, err)
	}
	if reconciled.Notification.EventSeq != reconciled.Event.Seq || reconciled.Notification.RecipientGeneration != 1 || reconciled.Notification.EventSeq == priority.Notification.EventSeq {
		t.Fatalf("replacement retarget did not create its own semantic notice: %+v", reconciled)
	}
	var generations int
	if err = s.db.QueryRow(`SELECT count(*) FROM queue_notifications WHERE entry_id=?`, sent.Queue.Entry.ID).Scan(&generations); err != nil || generations != 3 {
		t.Fatalf("notification history=%d err=%v", generations, err)
	}
	var oldUnavailable int
	if err = s.db.QueryRow(`SELECT count(*) FROM queue_notifications WHERE id=? AND status='unavailable'`, priority.Notification.ID).Scan(&oldUnavailable); err != nil || oldUnavailable != 1 {
		t.Fatalf("replacement retarget rewrote prior unavailable notice=%d err=%v", oldUnavailable, err)
	}
}

func TestQueueReplacementRetargetsWithoutUnavailableNotice(t *testing.T) {
	s, ctx, by := workItemStore(t)
	source, _ := workItemProject(t, s, ctx, by, "Clean replacement source", "sourcelead")
	target, lead := workItemProject(t, s, ctx, by, "Clean replacement target", "targetlead")
	item := createWorkItem(t, s, ctx, by, source, "clean-replacement")
	sent, err := s.DispatchWorkItem(ctx, source.ID, item.ID, api.DispatchWorkItemRequest{Revision: item.Revision, TargetTaskID: target.ID, RequestID: "clean-replacement-send"}, by)
	if err != nil || sent.Queue.Notification == nil || sent.Queue.Notification.Status != "stored" {
		t.Fatalf("stored initial notice: %+v %v", sent.Queue.Notification, err)
	}
	if _, err = s.CloseAgent(ctx, lead.ID, by); err != nil {
		t.Fatal(err)
	}
	replacement, err := s.AddAgent(ctx, target.ID, api.AddAgentRequest{Name: lead.Name, Host: "fixture", Session: "clean-replacement", Runtime: "codex"}, by)
	if err != nil {
		t.Fatal(err)
	}
	request := api.QueueActionRequest{Operation: "reconcile_recipient", RequestID: "clean-replacement-retarget", ExpectedRevision: sent.Queue.Entry.Revision, Cycle: sent.Queue.Entry.Cycle, Reason: "explicit current orchestrator replacement"}
	retargeted, err := s.QueueAction(ctx, target.ID, sent.Queue.Entry.ID, request, by)
	if err != nil || retargeted.Entry.OrchestratorAgentID != replacement.ID || retargeted.Entry.OrchestratorRunID != replacement.RunID || retargeted.Notification == nil || retargeted.Notification.Status != "stored" {
		t.Fatalf("replacement retarget: %+v %v", retargeted, err)
	}
	if retargeted.Notification.EventSeq != retargeted.Event.Seq || retargeted.Notification.RecipientGeneration != 1 || retargeted.Notification.RecipientAgentID != replacement.ID {
		t.Fatalf("replacement notice is not its own semantic event: %+v", retargeted)
	}
	var initialRows int
	if err = s.db.QueryRow(`SELECT count(*) FROM queue_notifications WHERE event_seq=? AND status='stored' AND recipient_agent_id=?`, sent.Queue.Event.Seq, lead.ID).Scan(&initialRows); err != nil || initialRows != 1 {
		t.Fatalf("retarget rewrote original stored notice=%d err=%v", initialRows, err)
	}
	replay, err := s.QueueAction(ctx, target.ID, sent.Queue.Entry.ID, request, by)
	if err != nil || !replay.Replay || replay.Notification == nil || replay.Notification.ID != retargeted.Notification.ID {
		t.Fatalf("retarget replay: %+v %v", replay, err)
	}
	if _, err = s.DispatchWorkItem(ctx, source.ID, item.ID, api.DispatchWorkItemRequest{Revision: item.Revision, TargetTaskID: target.ID, RequestID: "clean-replacement-send-again"}, by); err != nil {
		t.Fatalf("Send after explicit retarget: %v", err)
	}
}

func TestQueueNotificationSameRunRecoveryReplayAndRestartDedup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue-recipient-recovery.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, by := context.Background(), api.Caller{Node: "fixture", User: "owner"}
	source, _ := workItemProject(t, s, ctx, by, "Recipient recovery source", "sourcelead")
	target, lead := workItemProject(t, s, ctx, by, "Recipient recovery target", "targetlead")
	item := createWorkItem(t, s, ctx, by, source, "recipient-recovery")
	sent, err := s.DispatchWorkItem(ctx, source.ID, item.ID, api.DispatchWorkItemRequest{Revision: item.Revision, TargetTaskID: target.ID, RequestID: "recipient-recovery-send"}, by)
	if err != nil {
		t.Fatal(err)
	}
	retired := api.AgentRetired
	if _, err = s.UpdateAgent(ctx, lead.ID, api.UpdateAgentRequest{Status: &retired}, by); err != nil {
		t.Fatal(err)
	}
	priority, err := s.QueueAction(ctx, target.ID, sent.Queue.Entry.ID, api.QueueActionRequest{Operation: "priority", RequestID: "recipient-recovery-priority", ExpectedRevision: sent.Queue.Entry.Revision, Cycle: 1, Priority: "high"}, by)
	if err != nil || priority.Notification == nil || priority.Notification.Status != "unavailable" {
		t.Fatalf("unavailable setup: %+v %v", priority, err)
	}
	if _, err = s.QueueAction(ctx, target.ID, sent.Queue.Entry.ID, api.QueueActionRequest{Operation: "reconcile_recipient", RequestID: "recipient-still-retired", ExpectedRevision: priority.Entry.Revision, Cycle: 1, Reason: "not yet resumed"}, by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("still-retired run reconciled an unavailable notice: %v", err)
	}
	var before int
	if err = s.db.QueryRow(`SELECT count(*) FROM queue_notifications WHERE event_seq=?`, priority.Notification.EventSeq).Scan(&before); err != nil || before != 1 {
		t.Fatalf("retired rejection changed notification history=%d err=%v", before, err)
	}
	if _, err = s.db.Exec(`UPDATE agents SET status=? WHERE id=? AND run_id=?`, lead.Status, lead.ID, lead.RunID); err != nil {
		t.Fatal(err)
	}
	reconcileRequest := api.QueueActionRequest{Operation: "reconcile_recipient", RequestID: "recipient-same-run", ExpectedRevision: priority.Entry.Revision, Cycle: 1, Reason: "exact original run resumed"}
	recovered, err := s.QueueAction(ctx, target.ID, sent.Queue.Entry.ID, reconcileRequest, by)
	if err != nil || recovered.Notification == nil || recovered.Notification.Status != "stored" || recovered.Notification.RecipientAgentID != lead.ID || recovered.Notification.RecipientRunID != lead.RunID {
		t.Fatalf("same-run recovery: %+v %v", recovered, err)
	}
	if recovered.Notification.EventSeq != priority.Notification.EventSeq || recovered.Notification.RecipientGeneration != 2 || recovered.Event.Seq == recovered.Notification.EventSeq || recovered.Notification.CausalAuthor != priority.Notification.CausalAuthor {
		t.Fatalf("same-run recovery lost semantic/generation identity: %+v", recovered)
	}
	var generations, unavailable, stored int
	if err = s.db.QueryRow(`SELECT count(*),sum(status='unavailable'),sum(status='stored') FROM queue_notifications WHERE event_seq=?`, priority.Notification.EventSeq).Scan(&generations, &unavailable, &stored); err != nil || generations != 2 || unavailable != 1 || stored != 1 {
		t.Fatalf("same-run generation history count=%d unavailable=%d stored=%d err=%v", generations, unavailable, stored, err)
	}
	var beforeReadEvents int
	if err = s.db.QueryRow(`SELECT count(*) FROM queue_events WHERE entry_id=?`, sent.Queue.Entry.ID).Scan(&beforeReadEvents); err != nil {
		t.Fatal(err)
	}
	if err = s.MarkRead(ctx, target.ID, api.MarkReadRequest{AgentID: lead.ID, UpTo: recovered.Notification.MessageSeq}); err != nil {
		t.Fatal(err)
	}
	var afterReadEvents, afterReadNotifications int
	if err = s.db.QueryRow(`SELECT (SELECT count(*) FROM queue_events WHERE entry_id=?),(SELECT count(*) FROM queue_notifications WHERE event_seq=?)`, sent.Queue.Entry.ID, priority.Notification.EventSeq).Scan(&afterReadEvents, &afterReadNotifications); err != nil || afterReadEvents != beforeReadEvents || afterReadNotifications != 2 {
		t.Fatalf("recovery notice read created an ACK loop events=%d/%d notifications=%d err=%v", beforeReadEvents, afterReadEvents, afterReadNotifications, err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	replay, err := s.QueueAction(ctx, target.ID, sent.Queue.Entry.ID, reconcileRequest, by)
	if err != nil || !replay.Replay || replay.Notification == nil || replay.Notification.ID != recovered.Notification.ID || replay.Notification.RecipientGeneration != 2 {
		t.Fatalf("restart replay created a different recovery: %+v %v", replay, err)
	}
	if err = s.db.QueryRow(`SELECT count(*) FROM queue_notifications WHERE event_seq=?`, priority.Notification.EventSeq).Scan(&generations); err != nil || generations != 2 {
		t.Fatalf("restart replay duplicated generation count=%d err=%v", generations, err)
	}
	if _, err = s.QueueAction(ctx, target.ID, sent.Queue.Entry.ID, api.QueueActionRequest{Operation: "reconcile_recipient", RequestID: "recipient-no-outstanding", ExpectedRevision: recovered.Entry.Revision, Cycle: 1, Reason: "duplicate explicit recovery"}, by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("new-key recovery without an outstanding notice: %v", err)
	}
}

func TestQueueUnknownLaunchReleaseWithdrawRequeueAndNoImplicitReopen(t *testing.T) {
	s, ctx, by := workItemStore(t)
	source, _ := workItemProject(t, s, ctx, by, "Cycles source", "sourcelead")
	target, lead := workItemProject(t, s, ctx, by, "Cycles target", "targetlead")
	item := createWorkItem(t, s, ctx, by, source, "cycles-item")
	order := queueOrder(t, s, item, "Cycles order", "cycles-order")
	sent, err := s.DispatchWorkItem(ctx, source.ID, item.ID, api.DispatchWorkItemRequest{Revision: item.Revision, TargetTaskID: target.ID, RequestID: "cycles-send"}, by)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := s.QueueAction(ctx, target.ID, sent.Queue.Entry.ID, api.QueueActionRequest{Operation: "claim", RequestID: "cycles-claim", ExpectedRevision: sent.Queue.Entry.Revision, Cycle: 1, ExpectedItemRevision: item.Revision, ClaimantAgentID: lead.ID, ClaimantRunID: lead.RunID, WorkOrderMessage: &api.MessageReference{TaskID: source.ID, Seq: order.Seq}}, by)
	if err != nil {
		t.Fatal(err)
	}
	unknown, err := s.QueueAction(ctx, target.ID, sent.Queue.Entry.ID, api.QueueActionRequest{Operation: "launch_unknown", RequestID: "cycles-unknown", ExpectedRevision: claim.Entry.Revision, Cycle: 1, Reason: "launch response lost"}, by)
	if err != nil || !unknown.Entry.ReconciliationNeeded || unknown.Entry.State != api.QueueStateClaimed {
		t.Fatalf("unknown launch: %+v %v", unknown, err)
	}
	released, err := s.QueueAction(ctx, target.ID, sent.Queue.Entry.ID, api.QueueActionRequest{Operation: "release", RequestID: "cycles-release", ExpectedRevision: unknown.Entry.Revision, Cycle: 1, Reason: "confirmed no worker exists"}, by)
	if err != nil || released.Entry.State != api.QueueStateWaiting || !released.Entry.Eligible || released.Entry.ClaimantAgentID != "" {
		t.Fatalf("release: %+v %v", released, err)
	}
	withdrawn, err := s.QueueAction(ctx, target.ID, sent.Queue.Entry.ID, api.QueueActionRequest{Operation: "withdraw", RequestID: "cycles-withdraw", ExpectedRevision: released.Entry.Revision, Cycle: 1, Reason: "owner withdrew offer"}, by)
	if err != nil || withdrawn.Entry.State != api.QueueStateCancelled {
		t.Fatalf("withdraw: %+v %v", withdrawn, err)
	}
	requeued, err := s.QueueAction(ctx, target.ID, sent.Queue.Entry.ID, api.QueueActionRequest{Operation: "requeue", RequestID: "cycles-requeue", ExpectedRevision: withdrawn.Entry.Revision, Cycle: 1, Reason: "owner deliberately reoffered"}, by)
	if err != nil || requeued.Entry.State != api.QueueStateWaiting || requeued.Entry.Cycle != 2 {
		t.Fatalf("requeue: %+v %v", requeued, err)
	}
	dismissed := "dismissed"
	item, err = s.UpdateWorkItem(ctx, source.ID, item.ID, api.UpdateWorkItemRequest{Revision: item.Revision, Status: &dismissed}, by)
	if err != nil {
		t.Fatal(err)
	}
	terminal, err := s.GetQueueEntry(ctx, target.ID, sent.Queue.Entry.ID)
	if err != nil || terminal.State != api.QueueStateCancelled || terminal.Cycle != 2 || terminal.TerminalReason != "source_dismissed" {
		t.Fatalf("dismissed: %+v %v", terminal, err)
	}
	open := "open"
	item, err = s.UpdateWorkItem(ctx, source.ID, item.ID, api.UpdateWorkItemRequest{Revision: item.Revision, Status: &open}, by)
	if err != nil {
		t.Fatal(err)
	}
	stillTerminal, err := s.GetQueueEntry(ctx, target.ID, sent.Queue.Entry.ID)
	if err != nil || stillTerminal.State != api.QueueStateCancelled || stillTerminal.Cycle != 2 {
		t.Fatalf("implicit reopen resurrected Queue: %+v %v", stillTerminal, err)
	}
	cycle3, err := s.QueueAction(ctx, target.ID, sent.Queue.Entry.ID, api.QueueActionRequest{Operation: "requeue", RequestID: "cycles-requeue-3", ExpectedRevision: stillTerminal.Revision, Cycle: 2, Reason: "explicitly adopt reopened source"}, by)
	if err != nil || cycle3.Entry.Cycle != 3 || cycle3.Entry.OfferedItemRevision != item.Revision {
		t.Fatalf("explicit reopen cycle: %+v %v", cycle3, err)
	}
}

func TestQueueAgentMutationsRequireExactHandlerRunAndRetainedSelection(t *testing.T) {
	s, ctx, by := workItemStore(t)
	source, _ := workItemProject(t, s, ctx, by, "Authority source", "sourcelead")
	target, lead := workItemProject(t, s, ctx, by, "Authority target", "targetlead")
	handler, err := s.AddAgent(ctx, target.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: "authority-database", Host: "fixture", Session: "authority-database", Runtime: "codex", Role: api.AgentRoleDatabaseHandler}, by)
	if err != nil {
		t.Fatal(err)
	}
	rogue, err := s.AddAgent(ctx, target.ID, api.AddAgentRequest{Name: "authority-worker", Host: "fixture", Session: "authority-worker", Runtime: "codex"}, by)
	if err != nil {
		t.Fatal(err)
	}
	item := createWorkItem(t, s, ctx, by, source, "authority-item")
	sent, err := s.DispatchWorkItem(ctx, source.ID, item.ID, api.DispatchWorkItemRequest{Revision: item.Revision, TargetTaskID: target.ID, RequestID: "authority-send"}, by)
	if err != nil {
		t.Fatal(err)
	}
	humanSelection, err := s.PostMessage(ctx, target.ID, api.PostMessageRequest{To: handler.ID, Text: "Human selected priority change"}, by)
	if err != nil {
		t.Fatal(err)
	}
	base := api.QueueActionRequest{Operation: "priority", RequestID: "authority-priority", ExpectedRevision: sent.Queue.Entry.Revision, Cycle: 1, Priority: "urgent", AgentID: handler.ID, RunID: handler.RunID, Selection: &api.QueueSelection{TaskID: target.ID, MessageSeq: humanSelection.Seq}}
	rogueRequest := base
	rogueRequest.RequestID, rogueRequest.AgentID, rogueRequest.RunID = "authority-rogue", rogue.ID, rogue.RunID
	if _, err = s.QueueAction(ctx, target.ID, sent.Queue.Entry.ID, rogueRequest, by); err == nil {
		t.Fatal("ordinary worker mutated Queue")
	}
	staleRun := base
	staleRun.RequestID, staleRun.RunID = "authority-stale-run", api.NewID("run")
	if _, err = s.QueueAction(ctx, target.ID, sent.Queue.Entry.ID, staleRun, by); err == nil {
		t.Fatal("stale database-handler run mutated Queue")
	}
	missingSelection := base
	missingSelection.RequestID, missingSelection.Selection = "authority-no-selection", nil
	if _, err = s.QueueAction(ctx, target.ID, sent.Queue.Entry.ID, missingSelection, by); err == nil {
		t.Fatal("handler mutated Queue without retained selection")
	}
	workerSelection, err := s.PostMessage(ctx, target.ID, api.PostMessageRequest{AgentID: rogue.ID, To: handler.ID, Text: "Builder prose is not authority"}, by)
	if err != nil {
		t.Fatal(err)
	}
	untrustedSelection := base
	untrustedSelection.RequestID = "authority-builder-prose"
	untrustedSelection.Selection = &api.QueueSelection{TaskID: target.ID, MessageSeq: workerSelection.Seq}
	if _, err = s.QueueAction(ctx, target.ID, sent.Queue.Entry.ID, untrustedSelection, by); err == nil {
		t.Fatal("builder prose authorized Queue mutation")
	}
	systemSelection := base
	systemSelection.RequestID = "authority-system-notice"
	systemSelection.Selection = &api.QueueSelection{TaskID: target.ID, MessageSeq: sent.Queue.Notification.MessageSeq}
	if _, err = s.QueueAction(ctx, target.ID, sent.Queue.Entry.ID, systemSelection, by); err == nil {
		t.Fatal("typed Queue system notice authorized Queue mutation as a human selection")
	}
	changed, err := s.QueueAction(ctx, target.ID, sent.Queue.Entry.ID, base, by)
	if err != nil || changed.Entry.QueuePriority != "urgent" {
		t.Fatalf("authorized handler mutation: %+v %v", changed, err)
	}
	leadSelection, err := s.PostMessage(ctx, target.ID, api.PostMessageRequest{AgentID: lead.ID, To: handler.ID, Text: "Current orchestrator selected a bounded Queue change"}, by)
	if err != nil {
		t.Fatal(err)
	}
	orchestratorSelected := base
	orchestratorSelected.RequestID, orchestratorSelected.ExpectedRevision, orchestratorSelected.Priority = "authority-orchestrator-selection", changed.Entry.Revision, "high"
	orchestratorSelected.Selection = &api.QueueSelection{TaskID: target.ID, MessageSeq: leadSelection.Seq}
	if selected, selectedErr := s.QueueAction(ctx, target.ID, sent.Queue.Entry.ID, orchestratorSelected, by); selectedErr != nil || selected.Entry.QueuePriority != "high" {
		t.Fatalf("current orchestrator selection: %+v %v", selected, selectedErr)
	}
}

func TestQueueNoticeNarrowlyReachesItemBoundOrchestrator(t *testing.T) {
	s, ctx, by := workItemStore(t)
	source, _ := workItemProject(t, s, ctx, by, "Bound notice source", "sourcelead")
	target, lead := workItemProject(t, s, ctx, by, "Bound notice target", "targetlead")
	unrelated := createWorkItem(t, s, ctx, by, target, "unrelated-binding")
	order := queueOrder(t, s, unrelated, "Unrelated local order", "unrelated-order")
	if _, err := s.db.Exec(`INSERT INTO agent_work_item_bindings(agent_id,run_id,item_task_id,item_id,item_revision,work_order_task_id,work_order_message_seq,context_through_message_seq,replaces_agent_id,context_digest,context_json,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, lead.ID, lead.RunID, target.ID, unrelated.ID, unrelated.Revision, target.ID, order.Seq, order.Seq, nil, "synthetic-digest", []byte(`{"synthetic":true}`), ts(s.now())); err != nil {
		t.Fatal(err)
	}
	item := createWorkItem(t, s, ctx, by, source, "bound-notice-item")
	sent, err := s.DispatchWorkItem(ctx, source.ID, item.ID, api.DispatchWorkItemRequest{Revision: item.Revision, TargetTaskID: target.ID, RequestID: "bound-notice-send"}, by)
	if err != nil {
		t.Fatal(err)
	}
	messages, err := s.ListMessages(ctx, target.ID, 0, lead.ID, 64)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, message := range messages {
		if message.SystemNotice != nil && message.SystemNotice.Queue.EntryID == sent.Queue.Entry.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("item-bound orchestrator did not receive its exact typed Queue notice")
	}
}

func TestQueueFrozenPagesChangesAndBounds(t *testing.T) {
	s, ctx, by := workItemStore(t)
	source, _ := workItemProject(t, s, ctx, by, "Frozen source", "sourcelead")
	target, _ := workItemProject(t, s, ctx, by, "Frozen target", "targetlead")
	for i := 0; i < 4; i++ {
		item, err := s.CreateWorkItem(ctx, source.ID, api.CreateWorkItemRequest{Kind: "bug", Title: "Frozen item " + string(rune('A'+i)), RequestID: "frozen-item-" + string(rune('a'+i))}, by)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.DispatchWorkItem(ctx, source.ID, item.ID, api.DispatchWorkItemRequest{Revision: 1, TargetTaskID: target.ID, RequestID: "frozen-send-" + string(rune('a'+i))}, by); err != nil {
			t.Fatal(err)
		}
	}
	first, err := s.ListQueue(ctx, target.ID, "", 2, false)
	if err != nil || first.Complete || first.Cursor == "" || len(first.Entries) != 2 {
		t.Fatalf("first: %+v %v", first, err)
	}
	mutated, err := s.QueueAction(ctx, target.ID, first.Entries[0].ID, api.QueueActionRequest{Operation: "priority", RequestID: "frozen-priority", ExpectedRevision: first.Entries[0].Revision, Cycle: 1, Priority: "urgent"}, by)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.ListQueue(ctx, target.ID, first.Cursor, 2, false)
	if err != nil || !second.Complete || second.Cutoff != first.Cutoff || len(second.Entries) != 2 {
		t.Fatalf("second: %+v %v", second, err)
	}
	for _, entry := range second.Entries {
		if entry.ID == mutated.Entry.ID {
			t.Fatal("snapshot mutation reordered entry into frozen second page")
		}
	}
	changes, err := s.ListQueueChanges(ctx, target.ID, 0, 0, 2)
	if err != nil || changes.Complete || len(changes.Events) != 2 {
		t.Fatalf("changes first: %+v %v", changes, err)
	}
	next, err := s.ListQueueChanges(ctx, target.ID, changes.Checkpoint, changes.Cutoff, 64)
	if err != nil || !next.Complete {
		t.Fatalf("changes next: %+v %v", next, err)
	}
	if _, err = s.ListQueue(ctx, target.ID, "", 65, false); !errors.Is(err, api.ErrInvalid) {
		t.Fatalf("oversize list=%v", err)
	}
}

func TestQueueListBoundsFullSnapshotMaterialization(t *testing.T) {
	s, ctx, by := workItemStore(t)
	source, _ := workItemProject(t, s, ctx, by, "Bounded source", "sourcelead")
	target, _ := workItemProject(t, s, ctx, by, "Bounded target", "targetlead")
	lastEntryID := ""
	for i := 0; i < 70; i++ {
		item, err := s.CreateWorkItem(ctx, source.ID, api.CreateWorkItemRequest{
			Kind:        "bug",
			Title:       fmt.Sprintf("Bounded item %03d", i),
			Description: strings.Repeat(string(rune('a'+i%26)), 7*1024),
			RequestID:   fmt.Sprintf("bounded-item-%03d", i),
		}, by)
		if err != nil {
			t.Fatal(err)
		}
		sent, err := s.DispatchWorkItem(ctx, source.ID, item.ID, api.DispatchWorkItemRequest{
			Revision: item.Revision, TargetTaskID: target.ID, RequestID: fmt.Sprintf("bounded-send-%03d", i),
		}, by)
		if err != nil {
			t.Fatal(err)
		}
		lastEntryID = sent.Queue.Entry.ID
	}
	seen := map[string]bool{}
	cursor := ""
	for {
		page, err := s.ListQueue(ctx, target.ID, cursor, api.MaxQueuePage, false)
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(page)
		if err != nil || len(encoded)+1 > api.MaxQueuePageBytes {
			t.Fatalf("unbounded page bytes=%d err=%v", len(encoded)+1, err)
		}
		for _, entry := range page.Entries {
			if seen[entry.ID] {
				t.Fatalf("duplicate entry %s", entry.ID)
			}
			seen[entry.ID] = true
		}
		if page.Complete {
			break
		}
		if page.Cursor == "" || page.Cursor == cursor {
			t.Fatalf("Queue cursor did not advance: %q", page.Cursor)
		}
		cursor = page.Cursor
	}
	if len(seen) != 70 {
		t.Fatalf("bounded traversal returned %d entries", len(seen))
	}
	// A later off-page full snapshot must not be selected or deserialized for a
	// one-row first page. The list query materializes at most limit+1 snapshot
	// blobs; its cutoff/group/order work uses only indexed scalar event fields.
	if _, err := s.db.Exec(`UPDATE queue_events SET snapshot='not-json' WHERE entry_id=?`, lastEntryID); err != nil {
		t.Fatal(err)
	}
	page, err := s.ListQueue(ctx, target.ID, "", 1, false)
	if err != nil || len(page.Entries) != 1 || page.Complete {
		t.Fatalf("first bounded page materialized an off-page snapshot: %+v %v", page, err)
	}
}

func TestQueueLegacySchemaHasNoPrivateContextInEntries(t *testing.T) {
	// Compile-time/shape guard: Queue snapshots expose only the digest and exact
	// binding identity, never the admitted private context bytes.
	b, err := json.Marshal(api.QueueEntry{ContextDigest: "abc"})
	if err != nil {
		t.Fatal(err)
	}
	if string(b) == "" || json.Valid(b) == false {
		t.Fatal("invalid Queue entry JSON")
	}
	var columns int
	s, _, _ := workItemStore(t)
	if err = s.db.QueryRow(`SELECT count(*) FROM pragma_table_info('queue_entries') WHERE name='context_json'`).Scan(&columns); err != nil || columns != 0 {
		t.Fatalf("private context column=%d %v", columns, err)
	}
}

// TestQueueExactReplayRejectsChangedTeamRole addresses independent review
// #2300/#2771 finding 3: an exact-existing Queue admission retry must not
// silently succeed if it changes the declared TeamRole from what was
// actually admitted -- a replay is a retry of the same prior admission, not
// a fresh reclassification.
func TestQueueExactReplayRejectsChangedTeamRole(t *testing.T) {
	s, ctx, by := workItemStore(t)
	source, _ := workItemProject(t, s, ctx, by, "Replay source", "sourcelead")
	target, lead := workItemProject(t, s, ctx, by, "Replay target", "targetlead")
	item := createWorkItem(t, s, ctx, by, source, "replay-item")
	dispatched, err := s.DispatchWorkItem(ctx, source.ID, item.ID, api.DispatchWorkItemRequest{Revision: item.Revision, TargetTaskID: target.ID, RequestID: "replay-send-1"}, by)
	if err != nil {
		t.Fatal(err)
	}
	entry := dispatched.Queue.Entry
	order := queueOrder(t, s, item, "Replay order", "replay-order")
	claim, err := s.QueueAction(ctx, target.ID, entry.ID, api.QueueActionRequest{Operation: "claim", RequestID: "replay-claim-1", ExpectedRevision: entry.Revision, Cycle: entry.Cycle, ExpectedItemRevision: item.Revision, ClaimantAgentID: lead.ID, ClaimantRunID: lead.RunID, WorkOrderMessage: &api.MessageReference{TaskID: source.ID, Seq: order.Seq}}, by)
	if err != nil {
		t.Fatal(err)
	}
	bundle := syntheticPreparedContext(t, item, api.MessageReference{TaskID: source.ID, Seq: order.Seq}, syntheticHistory(item, order))
	workerID := api.NewID("agt")
	memberRequest := api.AddAgentRequest{
		AgentID: workerID, Name: "replay-worker", Host: "fixture", Session: "replay-worker", Runtime: "codex", ParentAgentID: lead.ID,
		WorkItem: &api.AgentWorkItemRequest{
			ItemTaskID: source.ID, ItemID: item.ID, ItemRevision: item.Revision, WorkOrderMessage: api.MessageReference{TaskID: source.ID, Seq: order.Seq},
			QueueClaim:    &api.QueueAdmissionClaim{EntryID: entry.ID, Cycle: entry.Cycle, ExpectedRevision: claim.Entry.Revision, ClaimantAgentID: lead.ID, ClaimantRunID: lead.RunID},
			ContextBundle: bundle, TeamRole: api.TeamRoleMember,
		},
	}
	worker, err := s.AddAgent(ctx, target.ID, memberRequest, by)
	if err != nil || worker.WorkItem == nil || worker.WorkItem.TeamRole != api.TeamRoleMember {
		t.Fatalf("initial member admission: %+v %v", worker, err)
	}
	exactReplay := memberRequest
	if _, err = s.AddAgent(ctx, target.ID, exactReplay, by); err != nil {
		t.Fatalf("unchanged exact replay should succeed: %v", err)
	}
	changedRole := memberRequest
	changedWork := *memberRequest.WorkItem
	changedWork.TeamRole = api.TeamRoleExtra
	changedRole.WorkItem = &changedWork
	if _, err = s.AddAgent(ctx, target.ID, changedRole, by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("replay declaring a different team role should be rejected, not treated as the same exact admission: %v", err)
	}
}
