package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/scs32/tailterm/hub/internal/api"
)

func syntheticPreparedContext(t *testing.T, item api.WorkItem, order api.MessageReference, history any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(map[string]any{
		"version": 1, "itemTaskId": item.TaskID, "itemId": item.ID, "itemRevision": item.Revision,
		"workOrderMessage": order, "history": history,
	})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func syntheticHistory(item api.WorkItem, messages ...api.Message) map[string]any {
	revisions := make([]api.WorkItemRevision, 0, item.Revision)
	for revision := int64(1); revision <= item.Revision; revision++ {
		revisions = append(revisions, api.WorkItemRevision{
			ItemID: item.ID, TaskID: item.TaskID, Kind: item.Kind, Title: item.Title, Description: item.Description,
			Status: item.Status, Priority: item.Priority, ItemSeq: item.Seq, Revision: revision,
			CreatedBy: item.CreatedBy, UpdatedBy: item.UpdatedBy, CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt,
			AttributionKind: "shared_workspace_claim", ChangeKind: "updated", Provenance: "native",
		})
	}
	links := make([]api.WorkItemMessageLink, 0, len(messages))
	for _, message := range messages {
		link := api.WorkItemMessageLink{Message: message, RevisionCoverage: "verified"}
		if len(message.WorkItems) == 1 {
			link.ItemRevision = message.WorkItems[0].ItemRevision
			link.Relationship = message.WorkItems[0].Relationship
		} else {
			link.ItemRevision = 1
			link.Source = true
		}
		links = append(links, link)
	}
	return map[string]any{
		"revision": revisions[len(revisions)-1], "revisions": revisions, "messages": links,
		"coverage": api.HistoryCoverage{Complete: true, ObservedCurrentRevision: item.Revision, LatestMaterialized: item.Revision, SnapshotCount: item.Revision, ConversationLinks: "explicit_only"},
	}
}

func contextLinkedMessage(t *testing.T, s *Store, task api.Task, item api.WorkItem, text, key string, order *api.MessageReference) api.Message {
	t.Helper()
	message, err := s.PostMessage(context.Background(), task.ID, api.PostMessageRequest{
		Text: text, RequestID: key,
		WorkItems:        []api.MessageWorkItem{{ItemTaskID: item.TaskID, ItemID: item.ID, ItemRevision: item.Revision, Relationship: "primary"}},
		WorkOrderMessage: order,
	}, api.Caller{Node: "fixture", User: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	return message
}

func TestAgentWorkItemContextAdmissionRestorationAndReplacement(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "context.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	by := api.Caller{Node: "fixture", User: "owner"}
	task, err := s.CreateTask(ctx, api.CreateTaskRequest{Name: "Synthetic routing", AllowAgentSpawn: true}, by)
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "lead", Host: "fixture", Session: "lead", Runtime: "codex"}, by)
	if err != nil || legacy.WorkItem != nil || legacy.ReadUpTo != 0 {
		t.Fatalf("legacy admission changed: %+v %v", legacy, err)
	}
	source, err := s.PostMessage(ctx, task.ID, api.PostMessageRequest{Text: "Authoritative owner intake"}, by)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.PostMessage(ctx, task.ID, api.PostMessageRequest{Text: "UNRELATED PRIVATE HISTORY"}, by); err != nil {
		t.Fatal(err)
	}
	item, err := s.CreateWorkItem(ctx, task.ID, api.CreateWorkItemRequest{
		Kind: "bug", Title: "Synthetic isolated context", Description: "Initial requirement", AgentID: legacy.ID,
		SourceMessageSeq: source.Seq, RequestID: "context-item",
	}, by)
	if err != nil {
		t.Fatal(err)
	}
	description := "Current requirements, decisions, artifacts, evidence, ownership and dependencies"
	item, err = s.UpdateWorkItem(ctx, task.ID, item.ID, api.UpdateWorkItemRequest{Revision: item.Revision, Description: &description, AgentID: legacy.ID}, by)
	if err != nil {
		t.Fatal(err)
	}
	order := contextLinkedMessage(t, s, task, item, "Bounded saved work order", "context-order", nil)
	orderRef := &api.MessageReference{TaskID: task.ID, Seq: order.Seq}
	evidence := contextLinkedMessage(t, s, task, item, "Verification artifact", "context-evidence", orderRef)
	leak, err := s.PostMessage(ctx, task.ID, api.PostMessageRequest{Text: "UNRELATED LATE CONVERSATION"}, by)
	if err != nil {
		t.Fatal(err)
	}
	binding := &api.AgentWorkItemRequest{
		ItemTaskID: task.ID, ItemID: item.ID, ItemRevision: item.Revision,
		WorkOrderMessage: *orderRef,
		ContextBundle:    syntheticPreparedContext(t, item, *orderRef, syntheticHistory(item, source, order, evidence)),
	}
	worker, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{
		Name: "worker-context", Host: "fixture", Session: "worker-context", Runtime: "codex", WorkItem: binding,
	}, by)
	if err != nil {
		t.Fatal(err)
	}
	if worker.WorkItem == nil || worker.WorkItem.RunID != worker.RunID || worker.ReadUpTo != leak.Seq || worker.Unread != 0 {
		t.Fatalf("fresh boundary/binding: %+v", worker)
	}
	workContext, err := s.GetAgentWorkItemContext(ctx, task.ID, worker.ID, worker.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(workContext.Bundle, []byte(description)) || !bytes.Contains(workContext.Bundle, []byte("Verification artifact")) ||
		bytes.Contains(workContext.Bundle, []byte("UNRELATED PRIVATE HISTORY")) || bytes.Contains(workContext.Bundle, []byte("UNRELATED LATE CONVERSATION")) {
		t.Fatalf("stored prepared context changed or leaked history: %s", workContext.Bundle)
	}
	if _, err = s.GetAgentWorkItemContext(ctx, task.ID, worker.ID, api.NewID("run")); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("stale run restored context: %v", err)
	}
	relevant, err := s.PostMessage(ctx, task.ID, api.PostMessageRequest{
		Text: "New item-scoped direction", To: worker.ID, RequestID: "context-new-direction",
		WorkItems:        []api.MessageWorkItem{{ItemTaskID: item.TaskID, ItemID: item.ID, ItemRevision: item.Revision, Relationship: "primary"}},
		WorkOrderMessage: orderRef,
	}, by)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.PostMessage(ctx, task.ID, api.PostMessageRequest{Text: "UNRELATED DIRECT MESSAGE", To: worker.ID}, by); err != nil {
		t.Fatal(err)
	}
	inbox, err := s.ListMessages(ctx, task.ID, worker.ReadUpTo, worker.ID, 50)
	if err != nil || len(inbox) != 1 || inbox[0].Seq != relevant.Seq {
		t.Fatalf("bound inbox leaked or lost messages: %+v %v", inbox, err)
	}
	workerNow, err := s.GetAgent(ctx, worker.ID)
	if err != nil || workerNow.Unread != 1 {
		t.Fatalf("bound unread count included unrelated traffic: %+v %v", workerNow, err)
	}
	if _, err = s.PostEvent(ctx, task.ID, api.PostEventRequest{AgentID: worker.ID, RunID: worker.RunID, Kind: api.EventExited}, by); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: worker.Name, Host: worker.Host, Session: "legacy-restart", Runtime: "codex"}, by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("item-bound identity restarted without context: %v", err)
	}

	replacementRequest := *binding
	replacementRequest.ReplacesAgentID = worker.ID
	replacement, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{
		Name: "worker-context-r2", Host: "fixture", Session: "worker-context-r2", Runtime: "codex", WorkItem: &replacementRequest,
	}, by)
	if err != nil {
		t.Fatal(err)
	}
	if replacement.ID == worker.ID || replacement.RunID == worker.RunID || replacement.WorkItem.ReplacesAgentID != worker.ID {
		t.Fatalf("replacement reused identity: before=%+v after=%+v", worker, replacement)
	}
	preserved, err := s.GetAgent(ctx, worker.ID)
	if err != nil || preserved.RunID != worker.RunID || preserved.Session != worker.Session || preserved.WorkItem == nil {
		t.Fatalf("original changed: %+v %v", preserved, err)
	}

	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := s.GetAgentWorkItemContext(ctx, task.ID, replacement.ID, replacement.RunID)
	if err != nil || restored.Binding.ReplacesAgentID != worker.ID || !bytes.Equal(restored.Bundle, binding.ContextBundle) {
		t.Fatalf("restart lost context/history: %+v %v", restored, err)
	}
}

func TestAgentWorkItemContextRejectsStaleMismatchedAndHelperAdmission(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "invalid.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	by := api.Caller{Node: "fixture", User: "owner"}
	zero := 0
	task, err := s.CreateTask(ctx, api.CreateTaskRequest{Name: "Synthetic failures", AllowAgentSpawn: true, MaxNewAgents: &zero}, by)
	if err != nil {
		t.Fatal(err)
	}
	parent, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "lead", Host: "fixture", Session: "lead", Runtime: "codex"}, by)
	if err != nil {
		t.Fatal(err)
	}
	item, err := s.CreateWorkItem(ctx, task.ID, api.CreateWorkItemRequest{Kind: "feature", Title: "Bound context", AgentID: parent.ID, RequestID: "fail-item"}, by)
	if err != nil {
		t.Fatal(err)
	}
	unlinked, err := s.PostMessage(ctx, task.ID, api.PostMessageRequest{Text: "Not a recorded item order"}, by)
	if err != nil {
		t.Fatal(err)
	}
	req := &api.AgentWorkItemRequest{ItemTaskID: task.ID, ItemID: item.ID, ItemRevision: item.Revision, WorkOrderMessage: api.MessageReference{TaskID: task.ID, Seq: unlinked.Seq}}
	req.ContextBundle = syntheticPreparedContext(t, item, req.WorkOrderMessage, syntheticHistory(item, unlinked))
	if _, err = s.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "bad-order", Host: "fixture", Session: "bad-order", Runtime: "codex", WorkItem: req}, by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("unlinked order accepted: %v", err)
	}
	order := contextLinkedMessage(t, s, task, item, "Linked order", "fail-order", nil)
	req.WorkOrderMessage.Seq = order.Seq
	req.ContextBundle = syntheticPreparedContext(t, item, req.WorkOrderMessage, syntheticHistory(item, order))
	req.ItemRevision++
	staleItem := item
	staleItem.Revision = req.ItemRevision
	req.ContextBundle = syntheticPreparedContext(t, staleItem, req.WorkOrderMessage, syntheticHistory(staleItem, order))
	if _, err = s.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "stale", Host: "fixture", Session: "stale", Runtime: "codex", WorkItem: req}, by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("stale revision accepted: %v", err)
	}
	req.ItemRevision = item.Revision
	req.ContextBundle = syntheticPreparedContext(t, item, req.WorkOrderMessage, syntheticHistory(item, order))
	if _, err = s.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "helper", Host: "fixture", Session: "helper", Runtime: "codex", ParentAgentID: parent.ID, WorkItem: req}, by); !errors.Is(err, api.ErrAgentSpawnLimit) {
		t.Fatalf("item binding bypassed helper limit: %v", err)
	}
	base, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "base", Host: "fixture", Session: "base", Runtime: "codex", WorkItem: req}, by)
	if err != nil || base.ParentAgentID != "" {
		t.Fatalf("parentless admission counted as helper: %+v %v", base, err)
	}
	agents, err := s.ListAgents(ctx, task.ID)
	if err != nil || len(agents) != 2 {
		t.Fatalf("failed admissions left records: %+v %v", agents, err)
	}
}
