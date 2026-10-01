package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/scs32/tailterm/hub/internal/api"
)

func TestWorkOrderScopeHandlerIntakeAndBookkeepingKeepsQueueAndStarts(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "scope.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	by := api.Caller{Node: "scope-test", User: "owner"}
	task, err := s.CreateTask(ctx, api.CreateTaskRequest{Name: "scope test"}, by)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: "handler", Role: api.AgentRoleDatabaseHandler, Host: "fixture", Session: "handler"}, by)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PostEvent(ctx, task.ID, api.PostEventRequest{AgentID: handler.ID, RunID: handler.RunID, Kind: api.EventRunning}, by); err != nil {
		t.Fatal(err)
	}
	item, err := s.CreateWorkItem(ctx, task.ID, api.CreateWorkItemRequest{Kind: "bug", Title: "filed scope", Description: "owner acceptance and files", RequestID: "scope-item"}, by)
	if err != nil {
		t.Fatal(err)
	}
	order, err := s.PostMessage(ctx, task.ID, api.PostMessageRequest{Text: "bounded owner order", RequestID: "scope-order", WorkItems: []api.MessageWorkItem{{ItemTaskID: task.ID, ItemID: item.ID, ItemRevision: item.Revision, Relationship: "primary"}}}, by)
	if err != nil {
		t.Fatal(err)
	}
	add := api.TeamQueueRequest{RequestID: "scope-add", Operation: "add", ItemID: item.ID, OrderMessageSeq: order.Seq, Host: "fixture", Cwd: t.TempDir()}
	if _, err := s.TeamQueueAction(ctx, task.ID, add); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("unconfirmed queue accepted: %v", err)
	}
	if _, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "early-manual", Operation: "manual", ItemID: item.ID, OrderMessageSeq: order.Seq}); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("unconfirmed manual reservation accepted: %v", err)
	}
	confirm := api.ConfirmWorkOrderScopeRequest{RequestID: "intake", AgentID: handler.ID, RunID: handler.RunID, ExpectedRevision: item.Revision, ScopeRevision: item.ScopeRevision, OrderMessageSeq: order.Seq, Complete: true}
	if _, err := s.ConfirmWorkOrderScope(ctx, task.ID, item.ID, api.ConfirmWorkOrderScopeRequest{RequestID: "incomplete", AgentID: handler.ID, RunID: handler.RunID, ExpectedRevision: item.Revision, ScopeRevision: item.ScopeRevision, OrderMessageSeq: order.Seq}); err == nil {
		t.Fatal("incomplete assertion accepted")
	}
	if _, err := s.ConfirmWorkOrderScope(ctx, task.ID, api.NewID("wi"), confirm); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("wrong item: %v", err)
	}
	wrongRun := confirm
	wrongRun.RequestID, wrongRun.RunID = "wrong-handler-run", api.NewID("run")
	if _, err := s.ConfirmWorkOrderScope(ctx, task.ID, item.ID, wrongRun); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("wrong handler run: %v", err)
	}
	filed, err := s.ConfirmWorkOrderScope(ctx, task.ID, item.ID, confirm)
	if err != nil {
		t.Fatal(err)
	}
	if replay, err := s.ConfirmWorkOrderScope(ctx, task.ID, item.ID, confirm); err != nil || !reflect.DeepEqual(replay, filed) {
		t.Fatalf("confirmation retry %+v %v", replay, err)
	}
	changed := confirm
	changed.ScopeRevision++
	if _, err := s.ConfirmWorkOrderScope(ctx, task.ID, item.ID, changed); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("changed confirmation retry: %v", err)
	}
	queued, err := s.TeamQueueAction(ctx, task.ID, add)
	if err != nil {
		t.Fatal(err)
	}
	initial := queued
	// Model a queue row carried across an upgrade before the handler files its
	// confirmation. Intake must confirm in place, leaving that row unchanged.
	if _, err := s.db.Exec(`DELETE FROM work_order_scope_confirmations WHERE task_id=? AND item_id=?`, task.ID, item.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := requireConfirmedTeamOrder(ctx, s.db, task.ID, item.ID, item.Revision, order.Seq); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("preexisting queue bypassed missing confirmation: %v", err)
	}
	if _, err := s.ConfirmWorkOrderScope(ctx, task.ID, item.ID, confirm); err != nil {
		t.Fatal(err)
	}
	if same, err := s.GetTeamQueueEntry(ctx, task.ID, queued.ID); err != nil || !reflect.DeepEqual(same, initial) {
		t.Fatalf("intake altered preexisting queue %+v %v", same, err)
	}
	for _, kind := range []string{"order", "sequencing_note", "decision"} {
		source, err := s.PostMessage(ctx, task.ID, api.PostMessageRequest{Text: kind, RequestID: "source-" + kind, WorkItems: []api.MessageWorkItem{{ItemTaskID: task.ID, ItemID: item.ID, ItemRevision: item.Revision, Relationship: "primary"}}}, by)
		if err != nil {
			t.Fatal(err)
		}
		req := api.WorkOrderBookkeepingRequest{RequestID: "save-" + kind, AgentID: handler.ID, RunID: handler.RunID, ExpectedRevision: item.Revision, OrderMessageSeq: order.Seq, SourceMessageSeq: source.Seq, Kind: kind, QueueEntryID: queued.ID}
		receipt, err := s.SaveWorkOrderBookkeeping(ctx, task.ID, item.ID, req)
		if err != nil {
			t.Fatal(err)
		}
		if receipt.ItemRevision != item.Revision || receipt.ScopeRevision != item.ScopeRevision || receipt.SourceMessageSeq != source.Seq || receipt.QueueEntryID != queued.ID || len(receipt.Admissions) != 0 {
			t.Fatalf("queued receipt %+v", receipt)
		}
		if replay, err := s.SaveWorkOrderBookkeeping(ctx, task.ID, item.ID, req); err != nil || !reflect.DeepEqual(replay, receipt) {
			t.Fatalf("lost-response retry %+v %v", replay, err)
		}
		req.Kind = "decision"
		if kind == "decision" {
			req.Kind = "order"
		}
		if _, err := s.SaveWorkOrderBookkeeping(ctx, task.ID, item.ID, req); !errors.Is(err, api.ErrConflict) {
			t.Fatalf("changed bookkeeping retry: %v", err)
		}
	}
	current, err := s.GetWorkItem(ctx, task.ID, item.ID)
	if err != nil || current.Revision != item.Revision || current.ScopeRevision != item.ScopeRevision {
		t.Fatalf("bookkeeping edited item %+v %v", current, err)
	}
	queued, err = s.GetTeamQueueEntry(ctx, task.ID, queued.ID)
	if err != nil || !reflect.DeepEqual(queued, initial) {
		t.Fatalf("bookkeeping edited queue %+v %v", queued, err)
	}
	// A parentless admission captures an immutable Start context. The receipt
	// must reference its exact run and digest without rewriting that context.
	bundle := syntheticPreparedContext(t, item, api.MessageReference{TaskID: task.ID, Seq: order.Seq}, syntheticHistory(item, order))
	worker, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "worker", Host: "fixture", Session: "worker", WorkItem: &api.AgentWorkItemRequest{ItemTaskID: task.ID, ItemID: item.ID, ItemRevision: item.Revision, WorkOrderMessage: api.MessageReference{TaskID: task.ID, Seq: order.Seq}, ContextBundle: bundle}}, by)
	if err != nil {
		t.Fatal(err)
	}
	before, err := s.GetAgentWorkItemContext(ctx, task.ID, worker.ID, worker.RunID)
	if err != nil {
		t.Fatal(err)
	}
	source, err := s.PostMessage(ctx, task.ID, api.PostMessageRequest{Text: "admitted decision", RequestID: "admitted-decision", WorkItems: []api.MessageWorkItem{{ItemTaskID: task.ID, ItemID: item.ID, ItemRevision: item.Revision, Relationship: "primary"}}}, by)
	if err != nil {
		t.Fatal(err)
	}
	req := api.WorkOrderBookkeepingRequest{RequestID: "admitted-save", AgentID: handler.ID, RunID: handler.RunID, ExpectedRevision: item.Revision, OrderMessageSeq: order.Seq, SourceMessageSeq: source.Seq, Kind: "decision", QueueEntryID: queued.ID, Admissions: []api.WorkOrderAdmission{{AgentID: worker.ID, RunID: worker.RunID, ContextDigest: worker.WorkItem.ContextDigest}}}
	receipt, err := s.SaveWorkOrderBookkeeping(ctx, task.ID, item.ID, req)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(receipt.Admissions, req.Admissions) {
		t.Fatalf("admission links %+v", receipt.Admissions)
	}
	for _, kind := range []string{"order", "sequencing_note"} {
		msg, err := s.PostMessage(ctx, task.ID, api.PostMessageRequest{Text: "admitted " + kind, RequestID: "admitted-source-" + kind, WorkItems: []api.MessageWorkItem{{ItemTaskID: task.ID, ItemID: item.ID, ItemRevision: item.Revision, Relationship: "primary"}}}, by)
		if err != nil {
			t.Fatal(err)
		}
		other := req
		other.RequestID, other.SourceMessageSeq, other.Kind = "admitted-save-"+kind, msg.Seq, kind
		first, err := s.SaveWorkOrderBookkeeping(ctx, task.ID, item.ID, other)
		if err != nil {
			t.Fatal(err)
		}
		second, err := s.SaveWorkOrderBookkeeping(ctx, task.ID, item.ID, other)
		if err != nil || second.ID != first.ID || !reflect.DeepEqual(first.Admissions, req.Admissions) {
			t.Fatalf("admitted %s retry %+v %v", kind, second, err)
		}
	}
	bad := req
	bad.RequestID = "wrong-digest"
	bad.Admissions = []api.WorkOrderAdmission{{AgentID: worker.ID, RunID: worker.RunID, ContextDigest: "wrong"}}
	if _, err := s.SaveWorkOrderBookkeeping(ctx, task.ID, item.ID, bad); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("wrong digest accepted: %v", err)
	}
	for name, change := range map[string]func(*api.WorkOrderBookkeepingRequest){
		"queue":  func(r *api.WorkOrderBookkeepingRequest) { r.QueueEntryID = api.NewID("tqe") },
		"order":  func(r *api.WorkOrderBookkeepingRequest) { r.OrderMessageSeq++ },
		"run":    func(r *api.WorkOrderBookkeepingRequest) { r.Admissions[0].RunID = api.NewID("run") },
		"source": func(r *api.WorkOrderBookkeepingRequest) { r.SourceMessageSeq = order.Seq + 100000 },
	} {
		bad := req
		bad.RequestID = "wrong-" + name
		bad.Admissions = append([]api.WorkOrderAdmission(nil), req.Admissions...)
		change(&bad)
		if _, err := s.SaveWorkOrderBookkeeping(ctx, task.ID, item.ID, bad); !errors.Is(err, api.ErrConflict) {
			t.Fatalf("wrong %s accepted: %v", name, err)
		}
	}
	otherItem, err := s.CreateWorkItem(ctx, task.ID, api.CreateWorkItemRequest{Kind: "bug", Title: "other target", RequestID: "other-target"}, by)
	if err != nil {
		t.Fatal(err)
	}
	otherSource, err := s.PostMessage(ctx, task.ID, api.PostMessageRequest{Text: "other source", RequestID: "other-source", WorkItems: []api.MessageWorkItem{{ItemTaskID: task.ID, ItemID: otherItem.ID, ItemRevision: otherItem.Revision, Relationship: "primary"}}}, by)
	if err != nil {
		t.Fatal(err)
	}
	wrongTarget := req
	wrongTarget.RequestID, wrongTarget.SourceMessageSeq = "wrong-source-target", otherSource.Seq
	if _, err := s.SaveWorkOrderBookkeeping(ctx, task.ID, item.ID, wrongTarget); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("other item's source accepted: %v", err)
	}
	after, err := s.GetAgentWorkItemContext(ctx, task.ID, worker.ID, worker.RunID)
	if err != nil || !reflect.DeepEqual(after, before) {
		t.Fatalf("bookkeeping changed Start context: %v", err)
	}
	if _, err := s.PostMessage(ctx, task.ID, api.PostMessageRequest{Text: "same-order progress", RequestID: "post-bookkeeping-progress", AgentID: worker.ID, WorkItems: []api.MessageWorkItem{{ItemTaskID: task.ID, ItemID: item.ID, ItemRevision: item.Revision, Relationship: "primary"}}, WorkOrderMessage: &api.MessageReference{TaskID: task.ID, Seq: order.Seq}}, by); err != nil {
		t.Fatalf("same-order work after bookkeeping: %v", err)
	}
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if saved, err := s.GetWorkOrderBookkeepingReceipt(ctx, task.ID, item.ID, req.RequestID); err != nil || !reflect.DeepEqual(saved, receipt) {
		t.Fatalf("restart receipt %+v %v", saved, err)
	}
	if replay, err := s.SaveWorkOrderBookkeeping(ctx, task.ID, item.ID, req); err != nil || !reflect.DeepEqual(replay, receipt) {
		t.Fatalf("restart uncertain retry %+v %v", replay, err)
	}
	newDescription := "changed files and acceptance"
	updated, err := s.UpdateWorkItem(ctx, task.ID, item.ID, api.UpdateWorkItemRequest{Revision: item.Revision, Description: &newDescription}, by)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Revision == item.Revision || updated.ScopeRevision == item.ScopeRevision {
		t.Fatalf("scope edit did not advance %+v", updated)
	}
	stale := confirm
	stale.RequestID = "stale-intake"
	if _, err := s.ConfirmWorkOrderScope(ctx, task.ID, item.ID, stale); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("stale confirmation accepted: %v", err)
	}
	if err := requireConfirmedTeamOrder(ctx, s.db, task.ID, item.ID, updated.Revision, order.Seq); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("old confirmation authorized new scope: %v", err)
	}
	newContext := syntheticPreparedContext(t, updated, api.MessageReference{TaskID: task.ID, Seq: order.Seq}, syntheticHistory(updated, order))
	if _, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "stale-scope-worker", Host: "fixture", Session: "stale-scope-worker", WorkItem: &api.AgentWorkItemRequest{ItemTaskID: task.ID, ItemID: item.ID, ItemRevision: updated.Revision, WorkOrderMessage: api.MessageReference{TaskID: task.ID, Seq: order.Seq}, ContextBundle: newContext}}, by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("scope edit kept admission valid: %v", err)
	}
}

// a3: bookkeeping, a new admission and receipts work at the amended revision
// once the running entry is rebound; before that the refusal names the entry
// and the rebind. The item, its entry and the earlier receipt are untouched.
func TestWorkOrderBookkeepingAfterRebind(t *testing.T) {
	f := newRebindFixture(t, true)
	admissions := func() []api.WorkOrderAdmission {
		var out []api.WorkOrderAdmission
		for id, row := range f.bindings(t) {
			if id != f.gone.ID {
				out = append(out, api.WorkOrderAdmission{AgentID: row[0], RunID: row[1], ContextDigest: row[3]})
			}
		}
		sort.Slice(out, func(i, j int) bool { return out[i].AgentID < out[j].AgentID })
		return out
	}
	rawReceipt := func(requestID string) string {
		t.Helper()
		var raw string
		if err := f.s.db.QueryRow(`SELECT receipt_json FROM work_order_bookkeeping WHERE task_id=? AND request_id=?`, f.task.ID, requestID).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		return raw
	}
	decision := contextLinkedMessage(t, f.s, f.task, f.item, "decision before the amendment", "decision-before", &api.MessageReference{TaskID: f.task.ID, Seq: f.order.Seq})
	before := api.WorkOrderBookkeepingRequest{RequestID: "save-before", AgentID: f.handler.ID, RunID: f.handler.RunID, ExpectedRevision: f.item.Revision, OrderMessageSeq: f.order.Seq, SourceMessageSeq: decision.Seq, Kind: "decision", QueueEntryID: f.entry.ID, Admissions: admissions()}
	old, err := f.s.SaveWorkOrderBookkeeping(f.ctx, f.task.ID, f.item.ID, before)
	if err != nil || old.ItemRevision != f.item.Revision || len(old.Admissions) != 2 {
		t.Fatalf("receipt before the amendment %+v %v", old, err)
	}
	oldRaw := rawReceipt(before.RequestID)

	updated, source := f.amend(t, "one")
	f.confirm(t, updated)
	after := api.WorkOrderBookkeepingRequest{RequestID: "save-after", AgentID: f.handler.ID, RunID: f.handler.RunID, ExpectedRevision: updated.Revision, OrderMessageSeq: f.order.Seq, SourceMessageSeq: source.Seq, Kind: "decision", QueueEntryID: f.entry.ID, Admissions: admissions()}
	_, err = f.s.SaveWorkOrderBookkeeping(f.ctx, f.task.ID, f.item.ID, after)
	if !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), f.entry.ID) || !strings.Contains(err.Error(), "tt team queue rebind") {
		t.Fatalf("bookkeeping before the rebind = %v, want a conflict naming %s and the rebind command", err, f.entry.ID)
	}
	want := fmt.Sprintf("entry %s is bound to revision %d; the item is at revision %d", f.entry.ID, f.item.Revision, updated.Revision)
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("refusal %q lacks %q", err, want)
	}

	rebound, err := f.s.TeamQueueAction(f.ctx, f.task.ID, f.rebind(updated, source))
	if err != nil {
		t.Fatal(err)
	}
	after.Admissions = admissions()
	receipt, err := f.s.SaveWorkOrderBookkeeping(f.ctx, f.task.ID, f.item.ID, after)
	if err != nil {
		t.Fatalf("bookkeeping after the rebind: %v", err)
	}
	if receipt.ItemID != f.item.ID || receipt.ItemRevision != updated.Revision || receipt.ScopeRevision != updated.ScopeRevision || receipt.QueueEntryID != f.entry.ID || rebound.ID != f.entry.ID || !reflect.DeepEqual(receipt.Admissions, after.Admissions) || len(receipt.Admissions) != 2 {
		t.Fatalf("receipt after the rebind %+v", receipt)
	}
	for i, a := range receipt.Admissions {
		if a != old.Admissions[i] {
			t.Fatalf("admission identity changed: %+v was %+v", a, old.Admissions[i])
		}
	}
	// Start: a new member is admitted at the amended revision and counted.
	late := f.member(t, "late")
	if late.WorkItem == nil || late.WorkItem.ItemRevision != updated.Revision {
		t.Fatalf("late admission %+v", late.WorkItem)
	}
	next := after
	next.RequestID, next.Kind, next.Admissions = "save-after-start", "sequencing_note", admissions()
	if started, err := f.s.SaveWorkOrderBookkeeping(f.ctx, f.task.ID, f.item.ID, next); err != nil || len(started.Admissions) != 3 || started.ItemRevision != updated.Revision {
		t.Fatalf("receipt after a new admission %+v %v", started, err)
	}
	// A fresh allocation intent at the amended revision is accepted as well.
	digest := sha256.Sum256([]byte("prepared context"))
	if _, err := f.s.CreateAllocationIntent(f.ctx, f.task.ID, api.CreateAllocationIntentRequest{AgentID: api.NewID("agt"), TargetTaskID: f.task.ID, ItemTaskID: f.task.ID, ItemID: f.item.ID, ItemRevision: updated.Revision,
		WorkOrderMessage: api.MessageReference{TaskID: f.task.ID, Seq: f.order.Seq}, ContextDigest: hex.EncodeToString(digest[:]), TeamRole: api.TeamRoleMember, AuthorAgentID: f.handler.ID, AuthorRunID: f.handler.RunID, ExpectedRunID: api.NewID("run")}, f.by); err != nil {
		t.Fatalf("allocation intent at the amended revision: %v", err)
	}

	if saved, err := f.s.GetWorkOrderBookkeepingReceipt(f.ctx, f.task.ID, f.item.ID, before.RequestID); err != nil || !reflect.DeepEqual(saved, old) {
		t.Fatalf("earlier receipt %+v %v", saved, err)
	}
	if raw := rawReceipt(before.RequestID); raw != oldRaw {
		t.Fatalf("earlier receipt bytes changed\nbefore %s\nafter  %s", oldRaw, raw)
	}
	if replay, err := f.s.SaveWorkOrderBookkeeping(f.ctx, f.task.ID, f.item.ID, before); err != nil || !reflect.DeepEqual(replay, old) {
		t.Fatalf("earlier receipt replay %+v %v", replay, err)
	}
	var entries int
	if err := f.s.db.QueryRow(`SELECT count(*) FROM team_queue_entries WHERE task_id=?`, f.task.ID).Scan(&entries); err != nil || entries != 1 {
		t.Fatalf("entries %d %v: a rebind needs no replacement", entries, err)
	}
}
