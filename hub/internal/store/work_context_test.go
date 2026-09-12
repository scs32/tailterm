package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
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

func preparedContextFromAcceptedHistory(t *testing.T, s *Store, item api.WorkItem, order api.MessageReference) json.RawMessage {
	t.Helper()
	ctx := context.Background()
	exact, err := s.GetWorkItemRevision(ctx, item.TaskID, item.ID, item.Revision)
	if err != nil {
		t.Fatal(err)
	}
	revisions, err := s.ListWorkItemRevisions(ctx, item.TaskID, item.ID, 0, 64)
	if err != nil || revisions.NextAfter != 0 {
		t.Fatalf("fixture revisions: %+v %v", revisions, err)
	}
	gaps, err := s.ListWorkItemHistoryGaps(ctx, item.TaskID, item.ID, 0, 64)
	if err != nil || gaps.NextAfter != 0 {
		t.Fatalf("fixture gaps: %+v %v", gaps, err)
	}
	messages, err := s.ListWorkItemMessages(ctx, item.TaskID, item.ID, 0, 0, 64)
	if err != nil || messages.NextAfter != 0 {
		t.Fatalf("fixture messages: %+v %v", messages, err)
	}
	return syntheticPreparedContext(t, item, order, map[string]any{
		"revision": exact, "revisions": revisions.Revisions, "gaps": gaps.Gaps,
		"messages": messages.Links, "coverage": messages.Coverage,
	})
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

// authorIntentAndBind authors the durable pre-admission allocation intent a
// fresh parented member/extra binding now requires (independent review
// #2300/#2771/#2840/#2916 finding 5) -- as the given author, who must be
// the task's database_handler or, by name, its Orchestrator -- then admits
// a fresh agent under it, launched by parent (which may differ from
// author, exercising launcher-vs-author provenance). A replacement
// admission still uses a plain AddAgentRequest directly, since it is
// exempt (it inherits its predecessor's already-authorized classification).
func authorIntentAndBind(t *testing.T, s *Store, task api.Task, item api.WorkItem, name, parent string, author api.Agent, teamRole string, by api.Caller) (api.Agent, error) {
	t.Helper()
	ctx := context.Background()
	order := contextLinkedMessage(t, s, task, item, name+" order", name+"-order", nil)
	orderRef := api.MessageReference{TaskID: task.ID, Seq: order.Seq}
	agentID := api.NewID("agt")
	bundle := syntheticPreparedContext(t, item, orderRef, syntheticHistory(item, order))
	digestBytes := sha256.Sum256(bundle)
	digest := hex.EncodeToString(digestBytes[:])
	expectedRunID := api.NewID("run")
	if _, err := s.CreateAllocationIntent(ctx, task.ID, api.CreateAllocationIntentRequest{
		AgentID: agentID, TargetTaskID: task.ID, ItemTaskID: task.ID, ItemID: item.ID, ItemRevision: item.Revision, WorkOrderMessage: orderRef,
		ContextDigest: digest, TeamRole: teamRole, AuthorAgentID: author.ID, AuthorRunID: author.RunID, ExpectedRunID: expectedRunID,
	}, by); err != nil {
		t.Fatalf("author intent for %s: %v", name, err)
	}
	req := &api.AgentWorkItemRequest{ItemTaskID: task.ID, ItemID: item.ID, ItemRevision: item.Revision, WorkOrderMessage: orderRef, TeamRole: teamRole}
	req.ContextBundle = bundle
	return s.AddAgent(ctx, task.ID, api.AddAgentRequest{AgentID: agentID, Name: name, Host: "fixture", Session: name, Runtime: "codex", ParentAgentID: parent, WorkItem: req}, by)
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
	contextLinkedMessage(t, s, task, item, "Verification artifact", "context-evidence", orderRef)
	leak, err := s.PostMessage(ctx, task.ID, api.PostMessageRequest{Text: "UNRELATED LATE CONVERSATION"}, by)
	if err != nil {
		t.Fatal(err)
	}
	binding := &api.AgentWorkItemRequest{
		ItemTaskID: task.ID, ItemID: item.ID, ItemRevision: item.Revision,
		WorkOrderMessage: *orderRef,
		ContextBundle:    preparedContextFromAcceptedHistory(t, s, item, *orderRef),
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
	launchRevision := item.Revision
	advancedDescription := description + "; handler-confirmed follow-up"
	if _, replayed, updateErr := s.CreateWorkItemUpdate(ctx, task.ID, item.ID, api.CreateWorkItemUpdate{
		ExpectedRevision: item.Revision, Description: &advancedDescription, AgentID: legacy.ID, RequestID: "context-item-r2",
	}, by); updateErr != nil || replayed {
		t.Fatalf("keyed item advance: replayed=%v err=%v", replayed, updateErr)
	}
	item, err = s.GetWorkItem(ctx, task.ID, item.ID)
	if err != nil || item.Revision != launchRevision+1 {
		t.Fatalf("advanced item: %+v %v", item, err)
	}
	staleUnbound := api.PostMessageRequest{
		Text: "Unbound stale revision claim", AgentID: legacy.ID, RequestID: "context-unbound-stale",
		WorkItems:        []api.MessageWorkItem{{ItemTaskID: binding.ItemTaskID, ItemID: binding.ItemID, ItemRevision: binding.ItemRevision, Relationship: "primary"}},
		WorkOrderMessage: orderRef,
	}
	if _, err = s.PostMessage(ctx, task.ID, staleUnbound, by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("unbound stale revision claim accepted: %v", err)
	}
	relevant, err := s.PostMessage(ctx, task.ID, api.PostMessageRequest{
		Text: "Bound worker progress at its launch revision", AgentID: worker.ID, RequestID: "context-worker-progress",
		WorkItems:        []api.MessageWorkItem{{ItemTaskID: binding.ItemTaskID, ItemID: binding.ItemID, ItemRevision: binding.ItemRevision, Relationship: "primary"}},
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
	if err != nil || workerNow.Unread != 0 {
		t.Fatalf("bound unread count included unrelated traffic: %+v %v", workerNow, err)
	}
	if err = s.MarkRead(ctx, task.ID, api.MarkReadRequest{AgentID: worker.ID, UpTo: relevant.Seq}); err != nil {
		t.Fatal(err)
	}
	decisionRequest := api.CreateDecisionRequest{
		DecisionRequest: api.DecisionRequest{
			Question: "Use the bounded synthetic approach?",
			Options: []api.DecisionOption{
				{ID: "yes", Label: "Proceed", Description: "Keep the exact item scope."},
				{ID: "no", Label: "Stop", Description: "Do not make the change."},
			},
			RecommendedOptionID: "yes", RecommendationReason: "It preserves the recorded contract.",
		},
		AgentID: worker.ID, RequestID: "context-decision",
		WorkItems:        []api.MessageWorkItem{{ItemTaskID: binding.ItemTaskID, ItemID: binding.ItemID, ItemRevision: binding.ItemRevision, Relationship: "primary"}},
		WorkOrderMessage: orderRef,
	}
	decision, err := s.CreateDecision(ctx, task.ID, decisionRequest, by)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.MarkRead(ctx, task.ID, api.MarkReadRequest{AgentID: worker.ID, UpTo: decision.Seq}); err != nil {
		t.Fatal(err)
	}
	answerRequest := api.AnswerDecisionRequest{RequestID: "context-answer", OptionID: "yes", Text: "Approved for this item."}
	answer, err := s.AnswerDecision(ctx, task.ID, decision.Seq, answerRequest, api.Caller{Node: "browser", User: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	replayedAnswer, err := s.AnswerDecision(ctx, task.ID, decision.Seq, answerRequest, api.Caller{Node: "browser", User: "owner"})
	if err != nil || replayedAnswer.Seq != answer.Seq || answer.PostReceipt == nil || replayedAnswer.PostReceipt == nil ||
		len(answer.WorkItems) != 1 || answer.WorkItems[0].ItemID != item.ID || answer.WorkOrderMessage == nil || *answer.WorkOrderMessage != *orderRef {
		t.Fatalf("typed answer/replay changed: answer=%+v replay=%+v err=%v", answer, replayedAnswer, err)
	}
	if _, err = s.PostMessage(ctx, task.ID, api.PostMessageRequest{Text: "UNRELATED AFTER DECISION", To: worker.ID}, by); err != nil {
		t.Fatal(err)
	}
	allScoped, err := s.ListMessages(ctx, task.ID, worker.ReadUpTo, worker.ID, 50)
	if err != nil || len(allScoped) != 3 || allScoped[0].Seq != relevant.Seq || allScoped[1].Seq != decision.Seq || allScoped[2].Seq != answer.Seq {
		t.Fatalf("bound inbox did not retain exact progress/ask/answer evidence: %+v %v", allScoped, err)
	}
	decisionInbox, err := s.ListMessages(ctx, task.ID, decision.Seq, worker.ID, 50)
	if err != nil || len(decisionInbox) != 1 || decisionInbox[0].Seq != answer.Seq || decisionInbox[0].DecisionAnswer == nil {
		t.Fatalf("bound decision inbox leaked or lost answer: %+v %v", decisionInbox, err)
	}
	workerNow, err = s.GetAgent(ctx, worker.ID)
	if err != nil || workerNow.Unread != 1 {
		t.Fatalf("bound decision unread lost answer or counted unrelated: %+v %v", workerNow, err)
	}
	historyLinks, err := s.ListWorkItemMessages(ctx, task.ID, item.ID, 0, 0, 64)
	if err != nil {
		t.Fatal(err)
	}
	linkedRevisions := map[int64]int64{}
	for _, link := range historyLinks.Links {
		linkedRevisions[link.Message.Seq] = link.ItemRevision
	}
	for _, seq := range []int64{relevant.Seq, decision.Seq, answer.Seq} {
		if linkedRevisions[seq] != launchRevision {
			t.Fatalf("linked evidence %d lost launch revision %d: %+v", seq, launchRevision, historyLinks.Links)
		}
	}
	if _, err = s.PostEvent(ctx, task.ID, api.PostEventRequest{AgentID: worker.ID, RunID: worker.RunID, Kind: api.EventExited}, by); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: worker.Name, Host: worker.Host, Session: "legacy-restart", Runtime: "codex"}, by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("item-bound identity restarted without context: %v", err)
	}

	replacementRequest := api.AgentWorkItemRequest{
		ItemTaskID: item.TaskID, ItemID: item.ID, ItemRevision: item.Revision,
		WorkOrderMessage: *orderRef, ContextBundle: preparedContextFromAcceptedHistory(t, s, item, *orderRef),
		ReplacesAgentID: worker.ID,
	}
	failedReplacement, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{
		Name: "worker-context-failed", Host: "fixture", Session: "worker-context-failed", Runtime: "codex", WorkItem: &replacementRequest,
	}, by)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.CloseAgent(ctx, failedReplacement.ID, by); err != nil {
		t.Fatal(err)
	}
	replacement, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{
		Name: "worker-context-r2", Host: "fixture", Session: "worker-context-r2", Runtime: "codex", WorkItem: &replacementRequest,
	}, by)
	if err != nil {
		t.Fatal(err)
	}
	if replacement.ID == worker.ID || replacement.RunID == worker.RunID || replacement.WorkItem.ReplacesAgentID != worker.ID {
		t.Fatalf("replacement reused identity: before=%+v after=%+v", worker, replacement)
	}
	failedNow, err := s.GetAgent(ctx, failedReplacement.ID)
	if err != nil || failedNow.Status != api.AgentClosed || failedNow.RunID != failedReplacement.RunID || failedNow.WorkItem == nil {
		t.Fatalf("failed replacement attempt changed: %+v %v", failedNow, err)
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
	if err != nil || restored.Binding.ReplacesAgentID != worker.ID || !bytes.Equal(restored.Bundle, replacementRequest.ContextBundle) ||
		!bytes.Contains(restored.Bundle, []byte(relevant.Text)) || !bytes.Contains(restored.Bundle, []byte(decisionRequest.Question)) ||
		!bytes.Contains(restored.Bundle, []byte(answerRequest.Text)) || bytes.Contains(restored.Bundle, []byte("UNRELATED AFTER DECISION")) ||
		bytes.Contains(restored.Bundle, []byte(staleUnbound.Text)) {
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
	task, err := s.CreateTask(ctx, api.CreateTaskRequest{Name: "Synthetic failures", Orchestrator: "lead", AllowAgentSpawn: true, MaxNewAgents: &zero}, by)
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
	completeContext := append([]byte(nil), req.ContextBundle...)
	req.ContextBundle = make([]byte, maxAgentWorkItemContextBytes+1)
	if _, err = s.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "oversized", Host: "fixture", Session: "oversized", Runtime: "codex", WorkItem: req}, by); !errors.Is(err, api.ErrContextLimit) {
		t.Fatalf("oversized context did not fail explicitly: %v", err)
	}
	req.ContextBundle = completeContext
	// Classification is an explicit declaration, never inferred from
	// ParentAgentID or binding order (independent review #2300 finding 1).
	// A parented, item-bound request with no declared TeamRole is rejected
	// outright rather than guessed.
	unclassified := *req
	if _, err = s.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "unclassified", Host: "fixture", Session: "unclassified", Runtime: "codex", ParentAgentID: parent.ID, WorkItem: &unclassified}, by); !errors.Is(err, api.ErrInvalid) {
		t.Fatalf("parented item-bound request with no declared team role should be rejected: %v", err)
	}
	// Owner correction #2045/#2048: a parented agent explicitly declared as
	// this item's regular team member (TeamRoleMember) is admitted even at
	// MaxNewAgents=0, since it is not an "extra" -- regardless of arrival
	// order, unlike the old binding-order heuristic.
	builder, err := authorIntentAndBind(t, s, task, item, "builder", parent.ID, parent, api.TeamRoleMember, by)
	if err != nil || builder.ParentAgentID != parent.ID || builder.WorkItem.TeamRole != api.TeamRoleMember {
		t.Fatalf("item's declared regular team member rejected as an extra: %+v %v", builder, err)
	}
	// A parented agent explicitly declared as an extra (TeamRoleExtra) is
	// checked against the item's MaxNewAgents allowance, regardless of how
	// many regular team members are already bound to the item.
	if _, err = authorIntentAndBind(t, s, task, item, "helper", parent.ID, parent, api.TeamRoleExtra, by); !errors.Is(err, api.ErrAgentSpawnLimit) {
		t.Fatalf("item binding bypassed extra limit: %v", err)
	}
	base, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "base", Host: "fixture", Session: "base", Runtime: "codex", WorkItem: req}, by)
	if err != nil || base.ParentAgentID != "" || base.WorkItem.TeamRole != api.TeamRoleMember {
		t.Fatalf("parentless admission not resolved as a regular member: %+v %v", base, err)
	}
	agents, err := s.ListAgents(ctx, task.ID)
	if err != nil || len(agents) != 3 {
		t.Fatalf("failed admissions left records: %+v %v", agents, err)
	}
}

// TestItemExtraCapacityIsIsolatedPerItem proves the owner-required
// #2045/#2048 correction end to end: each item's first parented builder is
// admitted free of the extra allowance, a second (genuine extra) on that
// item is capped independently of any other item's extras, and closing an
// extra frees exactly one slot for its own item without touching another
// item's allowance.
func TestItemExtraCapacityIsIsolatedPerItem(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "isolation.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	by := api.Caller{Node: "fixture", User: "owner"}
	one := 1
	task, err := s.CreateTask(ctx, api.CreateTaskRequest{Name: "Isolation", Orchestrator: "lead", AllowAgentSpawn: true, MaxNewAgents: &one}, by)
	if err != nil {
		t.Fatal(err)
	}
	lead, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "lead", Host: "fixture", Session: "lead", Runtime: "codex"}, by)
	if err != nil {
		t.Fatal(err)
	}
	bind := func(name string, item api.WorkItem, parent, teamRole string) (api.Agent, error) {
		return authorIntentAndBind(t, s, task, item, name, parent, lead, teamRole, by)
	}
	itemA, err := s.CreateWorkItem(ctx, task.ID, api.CreateWorkItemRequest{Kind: "bug", Title: "Item A", AgentID: lead.ID, RequestID: "item-a"}, by)
	if err != nil {
		t.Fatal(err)
	}
	itemB, err := s.CreateWorkItem(ctx, task.ID, api.CreateWorkItemRequest{Kind: "bug", Title: "Item B", AgentID: lead.ID, RequestID: "item-b"}, by)
	if err != nil {
		t.Fatal(err)
	}
	// A parented agent explicitly declared as each item's regular team
	// member is admitted even though MaxNewAgents=1: membership, not
	// arrival order, is what exempts it.
	builderA, err := bind("builder-a", itemA, lead.ID, api.TeamRoleMember)
	if err != nil {
		t.Fatalf("item A builder rejected: %v", err)
	}
	if _, err = bind("builder-b", itemB, lead.ID, api.TeamRoleMember); err != nil {
		t.Fatalf("item B builder rejected: %v", err)
	}
	// Item A's single extra allowance is exhausted by one genuine extra...
	extraA, err := bind("extra-a", itemA, lead.ID, api.TeamRoleExtra)
	if err != nil {
		t.Fatalf("item A's own extra allowance rejected: %v", err)
	}
	// ...which must not affect item B's independent allowance.
	if _, err = bind("extra-b", itemB, lead.ID, api.TeamRoleExtra); err != nil {
		t.Fatalf("item B extra rejected due to item A's usage: %v", err)
	}
	// A second extra on item A is now over its own allowance...
	if _, err = bind("extra-a-2", itemA, lead.ID, api.TeamRoleExtra); !errors.Is(err, api.ErrAgentSpawnLimit) {
		t.Fatalf("exhausted item A extra allowance: %v", err)
	}
	// ...and item B's allowance is independently exhausted too, proving
	// isolation runs both ways.
	if _, err = bind("extra-b-2", itemB, lead.ID, api.TeamRoleExtra); !errors.Is(err, api.ErrAgentSpawnLimit) {
		t.Fatalf("exhausted item B extra allowance: %v", err)
	}
	// Closing item A's extra frees exactly one slot for item A only.
	if _, err = s.CloseAgent(ctx, extraA.ID, by); err != nil {
		t.Fatal(err)
	}
	if _, err = bind("extra-a-3", itemA, lead.ID, api.TeamRoleExtra); err != nil {
		t.Fatalf("closing item A's extra should free its own slot: %v", err)
	}
	if _, err = bind("extra-b-3", itemB, lead.ID, api.TeamRoleExtra); !errors.Is(err, api.ErrAgentSpawnLimit) {
		t.Fatalf("item A's freed slot leaked into item B: %v", err)
	}
	// Closing the item A builder has no false refund of item A's already
	// spent extra allowance (item A currently has one active extra again).
	if _, err = s.CloseAgent(ctx, builderA.ID, by); err != nil {
		t.Fatal(err)
	}
	if _, err = bind("extra-a-4", itemA, lead.ID, api.TeamRoleExtra); !errors.Is(err, api.ErrAgentSpawnLimit) {
		t.Fatalf("closing the builder falsely refunded item A's extra allowance: %v", err)
	}
}

// TestItemExtraCapacityDescendantsAndConcurrentRace covers two remaining
// #2050 acceptance cases: a descendant spawned by an extra (not directly by
// the item's builder) still counts correctly against its own item, and a
// grandchild bound to a different item does not interfere; and concurrent
// admission requests for the same item's extra allowance cannot exceed it.
func TestItemExtraCapacityDescendantsAndConcurrentRace(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "descendants.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	by := api.Caller{Node: "fixture", User: "owner"}
	two := 2
	task, err := s.CreateTask(ctx, api.CreateTaskRequest{Name: "Descendants", Orchestrator: "lead", AllowAgentSpawn: true, MaxNewAgents: &two}, by)
	if err != nil {
		t.Fatal(err)
	}
	lead, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "lead", Host: "fixture", Session: "lead", Runtime: "codex"}, by)
	if err != nil {
		t.Fatal(err)
	}
	bind := func(name string, item api.WorkItem, parent, teamRole string) (api.Agent, error) {
		return authorIntentAndBind(t, s, task, item, name, parent, lead, teamRole, by)
	}
	itemA, err := s.CreateWorkItem(ctx, task.ID, api.CreateWorkItemRequest{Kind: "bug", Title: "Item A", AgentID: lead.ID, RequestID: "desc-item-a"}, by)
	if err != nil {
		t.Fatal(err)
	}
	itemB, err := s.CreateWorkItem(ctx, task.ID, api.CreateWorkItemRequest{Kind: "bug", Title: "Item B", AgentID: lead.ID, RequestID: "desc-item-b"}, by)
	if err != nil {
		t.Fatal(err)
	}
	builderA, err := bind("builder-a", itemA, lead.ID, api.TeamRoleMember)
	if err != nil {
		t.Fatalf("item A builder rejected: %v", err)
	}
	// A descendant of the builder (parent is the builder, not lead), still
	// declared as an item-A extra, counts against item A's allowance
	// regardless of which agent spawned it.
	extraA, err := bind("extra-a", itemA, builderA.ID, api.TeamRoleExtra)
	if err != nil {
		t.Fatalf("descendant extra on item A rejected: %v", err)
	}
	// A grandchild of that extra, declared as item B's regular member, must
	// not be treated as an item-A extra merely because of its ancestry.
	if _, err = bind("builder-b", itemB, extraA.ID, api.TeamRoleMember); err != nil {
		t.Fatalf("item B's builder, descended from an item-A extra, wrongly rejected: %v", err)
	}
	// Item A now has one extra (extraA) against an allowance of 2: exactly
	// one more concurrent request for item A should succeed.
	var wg sync.WaitGroup
	results := make(chan error, 5)
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := fmt.Sprintf("race-extra-a-%d", i)
			_, e := bind(name, itemA, lead.ID, api.TeamRoleExtra)
			results <- e
		}(i)
	}
	wg.Wait()
	close(results)
	success := 0
	for e := range results {
		if e == nil {
			success++
		} else if !errors.Is(e, api.ErrAgentSpawnLimit) {
			t.Fatal(e)
		}
	}
	if success != 1 {
		t.Fatalf("item A's remaining single extra slot allowed %d concurrent admissions", success)
	}
	// Item B's independent allowance is untouched by item A's race.
	if _, err = bind("extra-b", itemB, lead.ID, api.TeamRoleExtra); err != nil {
		t.Fatalf("item B allowance affected by item A's concurrent race: %v", err)
	}
}

// TestItemExtraCapacityReplacementInheritsTeamRole addresses independent
// review #2300 finding 1(d): replacing an item's parented builder must not
// be misclassified as a fresh extra consuming a slot, and a replacement's
// TeamRole is inherited from the binding it replaces, not independently
// (re)declared.
func TestItemExtraCapacityReplacementInheritsTeamRole(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "replacement.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	by := api.Caller{Node: "fixture", User: "owner"}
	zero := 0
	task, err := s.CreateTask(ctx, api.CreateTaskRequest{Name: "Replacement", Orchestrator: "lead", AllowAgentSpawn: true, MaxNewAgents: &zero}, by)
	if err != nil {
		t.Fatal(err)
	}
	lead, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "lead", Host: "fixture", Session: "lead", Runtime: "codex"}, by)
	if err != nil {
		t.Fatal(err)
	}
	item, err := s.CreateWorkItem(ctx, task.ID, api.CreateWorkItemRequest{Kind: "bug", Title: "Replaced item", AgentID: lead.ID, RequestID: "replace-item"}, by)
	if err != nil {
		t.Fatal(err)
	}
	builder, err := authorIntentAndBind(t, s, task, item, "builder", lead.ID, lead, api.TeamRoleMember, by)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.PostEvent(ctx, task.ID, api.PostEventRequest{AgentID: builder.ID, RunID: builder.RunID, Kind: api.EventExited}, by); err != nil {
		t.Fatal(err)
	}
	replaceOrder := contextLinkedMessage(t, s, task, item, "replacement order", "replace-builder-2-order", nil)
	// A conflicting explicit declaration on a replacement is rejected
	// rather than silently overridden or silently accepted.
	conflicting := &api.AgentWorkItemRequest{ItemTaskID: task.ID, ItemID: item.ID, ItemRevision: item.Revision, WorkOrderMessage: api.MessageReference{TaskID: task.ID, Seq: replaceOrder.Seq}, ReplacesAgentID: builder.ID, TeamRole: api.TeamRoleExtra}
	conflicting.ContextBundle = syntheticPreparedContext(t, item, conflicting.WorkOrderMessage, syntheticHistory(item, replaceOrder))
	if _, err = s.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "replacement-bad", Host: "fixture", Session: "replacement-bad", Runtime: "codex", ParentAgentID: lead.ID, WorkItem: conflicting}, by); !errors.Is(err, api.ErrInvalid) {
		t.Fatalf("replacement with conflicting declared team role should be rejected: %v", err)
	}
	// Omitting TeamRole on a replacement inherits the prior binding's role
	// (member here) -- admitted even at MaxNewAgents=0, since it is not a
	// fresh extra allocation.
	inherited := &api.AgentWorkItemRequest{ItemTaskID: task.ID, ItemID: item.ID, ItemRevision: item.Revision, WorkOrderMessage: api.MessageReference{TaskID: task.ID, Seq: replaceOrder.Seq}, ReplacesAgentID: builder.ID}
	inherited.ContextBundle = syntheticPreparedContext(t, item, inherited.WorkOrderMessage, syntheticHistory(item, replaceOrder))
	replacement, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "replacement", Host: "fixture", Session: "replacement", Runtime: "codex", ParentAgentID: lead.ID, WorkItem: inherited}, by)
	if err != nil || replacement.WorkItem == nil || replacement.WorkItem.TeamRole != api.TeamRoleMember {
		t.Fatalf("replacement should inherit member role from the binding it replaces: %+v %v", replacement, err)
	}
}

// TestItemExtraCapacityRegularMemberIgnoresSpawnDisabled addresses
// independent review #2300 finding 2: a genuine regular team member bound
// to a work item must be launchable through the parented CLI path even
// while AllowAgentSpawn is disabled for the task, since disabling it is
// about extra-helper spawning, not ordinary team composition. Only a
// genuine extra is blocked.
func TestItemExtraCapacityRegularMemberIgnoresSpawnDisabled(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "spawn-disabled.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	by := api.Caller{Node: "fixture", User: "owner"}
	task, err := s.CreateTask(ctx, api.CreateTaskRequest{Name: "Spawn disabled", Orchestrator: "lead", AllowAgentSpawn: false}, by)
	if err != nil {
		t.Fatal(err)
	}
	lead, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "lead", Host: "fixture", Session: "lead", Runtime: "codex"}, by)
	if err != nil {
		t.Fatal(err)
	}
	item, err := s.CreateWorkItem(ctx, task.ID, api.CreateWorkItemRequest{Kind: "bug", Title: "Disabled-spawn item", AgentID: lead.ID, RequestID: "disabled-spawn-item"}, by)
	if err != nil {
		t.Fatal(err)
	}
	bind := func(name, teamRole string) (api.Agent, error) {
		return authorIntentAndBind(t, s, task, item, name, lead.ID, lead, teamRole, by)
	}
	if _, err = bind("builder", api.TeamRoleMember); err != nil {
		t.Fatalf("regular team member blocked by disabled agent spawning: %v", err)
	}
	if _, err = bind("extra", api.TeamRoleExtra); !errors.Is(err, api.ErrAgentSpawnDisabled) {
		t.Fatalf("genuine extra should still be blocked by disabled agent spawning: %v", err)
	}
}

// TestItemExtraCapacityReplacementRequiresExitedAndDoesNotDoubleCount
// addresses independent review #2300/#2771 finding 2: an active (not
// exited) extra must not be "replaced" by a concurrently admitted session
// -- that would double-reserve one logical slot as two active bindings --
// and once a genuinely exited extra IS replaced, the exited original and
// its replacement must count as exactly one active extra, not two, on
// subsequent admissions.
func TestItemExtraCapacityReplacementRequiresExitedAndDoesNotDoubleCount(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "replacement-lifecycle.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	by := api.Caller{Node: "fixture", User: "owner"}
	one := 1
	task, err := s.CreateTask(ctx, api.CreateTaskRequest{Name: "Replacement lifecycle", Orchestrator: "lead", AllowAgentSpawn: true, MaxNewAgents: &one}, by)
	if err != nil {
		t.Fatal(err)
	}
	lead, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "lead", Host: "fixture", Session: "lead", Runtime: "codex"}, by)
	if err != nil {
		t.Fatal(err)
	}
	item, err := s.CreateWorkItem(ctx, task.ID, api.CreateWorkItemRequest{Kind: "bug", Title: "Replacement lifecycle item", AgentID: lead.ID, RequestID: "replacement-lifecycle-item"}, by)
	if err != nil {
		t.Fatal(err)
	}
	bind := func(name, replaces string) (api.Agent, error) {
		if replaces == "" {
			return authorIntentAndBind(t, s, task, item, name, lead.ID, lead, api.TeamRoleExtra, by)
		}
		order := contextLinkedMessage(t, s, task, item, name+" order", name+"-order", nil)
		req := &api.AgentWorkItemRequest{ItemTaskID: task.ID, ItemID: item.ID, ItemRevision: item.Revision, WorkOrderMessage: api.MessageReference{TaskID: task.ID, Seq: order.Seq}, ReplacesAgentID: replaces}
		req.ContextBundle = syntheticPreparedContext(t, item, req.WorkOrderMessage, syntheticHistory(item, order))
		return s.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: name, Host: "fixture", Session: name, Runtime: "codex", ParentAgentID: lead.ID, WorkItem: req}, by)
	}
	extra, err := bind("extra", "")
	if err != nil {
		t.Fatalf("first extra rejected: %v", err)
	}
	// Still active (not exited): replacing it must be rejected outright,
	// not silently admitted as a second, concurrently active binding.
	if _, err = bind("replacement-too-early", extra.ID); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("replacing an active (non-exited) extra should be rejected: %v", err)
	}
	if _, err = s.PostEvent(ctx, task.ID, api.PostEventRequest{AgentID: extra.ID, RunID: extra.RunID, Kind: api.EventExited}, by); err != nil {
		t.Fatal(err)
	}
	replacement, err := bind("replacement", extra.ID)
	if err != nil {
		t.Fatalf("replacing a genuinely exited extra should be admitted: %v", err)
	}
	if replacement.WorkItem == nil || replacement.WorkItem.TeamRole != api.TeamRoleExtra {
		t.Fatalf("replacement lost its inherited extra role: %+v", replacement)
	}
	// The exited original and its live replacement must count as exactly
	// one active extra: with MaxNewAgents=1 already fully spent by that one
	// logical slot, a second fresh extra must still be rejected -- not
	// admitted because the original and replacement double-counted down to
	// zero, and not permanently blocked because they double-counted up to
	// two either.
	if _, err = bind("second-extra", ""); !errors.Is(err, api.ErrAgentSpawnLimit) {
		t.Fatalf("exited original + replacement should count as exactly one active extra: %v", err)
	}
}

// TestItemExtraCapacityFailedReplacementRetryNeverExceedsCeilingWithReuse is
// the exact acceptance case (B) from review #2840/#2844: E exits, its
// replacement R1 closes (a failed retry attempt), an unrelated fresh extra F
// takes real available capacity, and a later retry R2 (still naming E) must
// never push the item over its ceiling -- while remaining a valid retry when
// no capacity was actually reused by anyone else.
func TestItemExtraCapacityFailedReplacementRetryNeverExceedsCeilingWithReuse(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "failed-retry-reuse.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	by := api.Caller{Node: "fixture", User: "owner"}
	two := 2
	task, err := s.CreateTask(ctx, api.CreateTaskRequest{Name: "Failed retry reuse", Orchestrator: "lead", AllowAgentSpawn: true, MaxNewAgents: &two}, by)
	if err != nil {
		t.Fatal(err)
	}
	lead, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "lead", Host: "fixture", Session: "lead", Runtime: "codex"}, by)
	if err != nil {
		t.Fatal(err)
	}
	item, err := s.CreateWorkItem(ctx, task.ID, api.CreateWorkItemRequest{Kind: "bug", Title: "Failed retry reuse item", AgentID: lead.ID, RequestID: "failed-retry-reuse-item"}, by)
	if err != nil {
		t.Fatal(err)
	}
	bind := func(name, replaces string) (api.Agent, error) {
		if replaces == "" {
			return authorIntentAndBind(t, s, task, item, name, lead.ID, lead, api.TeamRoleExtra, by)
		}
		order := contextLinkedMessage(t, s, task, item, name+" order", name+"-order", nil)
		req := &api.AgentWorkItemRequest{ItemTaskID: task.ID, ItemID: item.ID, ItemRevision: item.Revision, WorkOrderMessage: api.MessageReference{TaskID: task.ID, Seq: order.Seq}, ReplacesAgentID: replaces}
		req.ContextBundle = syntheticPreparedContext(t, item, req.WorkOrderMessage, syntheticHistory(item, order))
		return s.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: name, Host: "fixture", Session: name, Runtime: "codex", ParentAgentID: lead.ID, WorkItem: req}, by)
	}
	e, err := bind("e", "")
	if err != nil {
		t.Fatalf("E rejected: %v", err)
	}
	if _, err = s.PostEvent(ctx, task.ID, api.PostEventRequest{AgentID: e.ID, RunID: e.RunID, Kind: api.EventExited}, by); err != nil {
		t.Fatal(err)
	}
	r1, err := bind("r1", e.ID)
	if err != nil {
		t.Fatalf("R1 replacement rejected: %v", err)
	}
	if _, err = s.CloseAgent(ctx, r1.ID, by); err != nil {
		t.Fatal(err)
	}
	// E now reverts to counting itself (R1's only successor is closed);
	// item is at 1 of 2. An unrelated fresh extra F takes the genuinely
	// available second slot.
	if _, err = bind("f", ""); err != nil {
		t.Fatalf("F should take real available capacity: %v", err)
	}
	// Now at ceiling (E + F = 2 of 2). A retry R2 naming E must not push the
	// item over the ceiling: E->R2 is slot-neutral (E excluded once R2 is
	// live, R2 counted instead), so it is still admitted without exceeding 2.
	r2, err := bind("r2", e.ID)
	if err != nil {
		t.Fatalf("valid retry R2 should still be admitted (slot-neutral, no ceiling breach): %v", err)
	}
	if r2.WorkItem == nil || r2.WorkItem.TeamRole != api.TeamRoleExtra {
		t.Fatalf("R2 lost its inherited extra role: %+v", r2)
	}
	// A further genuinely NEW extra now correctly finds the item at its
	// ceiling (R2 + F = 2 of 2).
	if _, err = bind("g", ""); !errors.Is(err, api.ErrAgentSpawnLimit) {
		t.Fatalf("item should be at its ceiling after R2 + F: %v", err)
	}
}

// TestAllocationIntentRequiredMatchedAndConsumedOnce addresses independent
// review #2300/#2771/#2840 finding 5, as clarified by #2844/#2850/#2867/#2870:
// a fresh parented member/extra admission must be authorized by a durable,
// pre-admission, handler/lead-authored allocation intent bound to the exact
// preallocated agent identity, item, revision, work-order message and team
// role -- not by the launch request's self-declaration alone. Missing or
// mismatched intent is rejected; a matching one is consumed exactly once.
func TestAllocationIntentRequiredMatchedAndConsumedOnce(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "allocation-intent.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	by := api.Caller{Node: "fixture", User: "owner"}
	task, err := s.CreateTask(ctx, api.CreateTaskRequest{Name: "Allocation intent", Orchestrator: "lead", AllowAgentSpawn: true}, by)
	if err != nil {
		t.Fatal(err)
	}
	lead, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "lead", Host: "fixture", Session: "lead", Runtime: "codex"}, by)
	if err != nil {
		t.Fatal(err)
	}
	outsider, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "outsider", Host: "fixture", Session: "outsider", Runtime: "codex"}, by)
	if err != nil {
		t.Fatal(err)
	}
	item, err := s.CreateWorkItem(ctx, task.ID, api.CreateWorkItemRequest{Kind: "bug", Title: "Intent item", AgentID: lead.ID, RequestID: "intent-item"}, by)
	if err != nil {
		t.Fatal(err)
	}
	order := contextLinkedMessage(t, s, task, item, "intent order", "intent-order", nil)
	orderRef := api.MessageReference{TaskID: task.ID, Seq: order.Seq}
	bundle := syntheticPreparedContext(t, item, orderRef, syntheticHistory(item, order))
	digestBytes := sha256.Sum256(bundle)
	digest := hex.EncodeToString(digestBytes[:])
	otherBundle := syntheticPreparedContext(t, item, orderRef, syntheticHistory(item, order, order))
	intentReq := func(agentID, teamRole, expectedRunID string) api.CreateAllocationIntentRequest {
		return api.CreateAllocationIntentRequest{
			AgentID: agentID, TargetTaskID: task.ID, ItemTaskID: task.ID, ItemID: item.ID, ItemRevision: item.Revision, WorkOrderMessage: orderRef,
			ContextDigest: digest, TeamRole: teamRole, AuthorAgentID: lead.ID, AuthorRunID: lead.RunID, ExpectedRunID: expectedRunID,
		}
	}
	addReq := func(agentID string) api.AddAgentRequest {
		return api.AddAgentRequest{
			AgentID: agentID, Name: "worker-" + agentID, Host: "fixture", Session: "worker-" + agentID, Runtime: "codex", ParentAgentID: lead.ID,
			WorkItem: &api.AgentWorkItemRequest{ItemTaskID: task.ID, ItemID: item.ID, ItemRevision: item.Revision, WorkOrderMessage: orderRef, ContextBundle: bundle, TeamRole: api.TeamRoleMember},
		}
	}

	// Authoring rejections.
	if _, err = s.CreateAllocationIntent(ctx, task.ID, intentReq(api.NewID("agt"), api.TeamRoleMember, api.NewID("run")), by); err != nil {
		t.Fatal(err)
	}
	unauthorized := intentReq(api.NewID("agt"), api.TeamRoleMember, api.NewID("run"))
	unauthorized.AuthorAgentID, unauthorized.AuthorRunID = outsider.ID, outsider.RunID
	if _, err = s.CreateAllocationIntent(ctx, task.ID, unauthorized, by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("an author who is neither database_handler nor the orchestrator should be rejected: %v", err)
	}
	staleAuthorRun := intentReq(api.NewID("agt"), api.TeamRoleMember, api.NewID("run"))
	staleAuthorRun.AuthorRunID = api.NewID("run")
	if _, err = s.CreateAllocationIntent(ctx, task.ID, staleAuthorRun, by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("a stale/mismatched author run should be rejected: %v", err)
	}

	// No preallocated identity at all: rejected outright.
	noAgentID := addReq("")
	noAgentID.AgentID = ""
	if _, err = s.AddAgent(ctx, task.ID, noAgentID, by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("fresh parented admission with no --agent-id should be rejected: %v", err)
	}
	// A preallocated identity with no recorded intent at all: rejected.
	missingIntentID := api.NewID("agt")
	if _, err = s.AddAgent(ctx, task.ID, addReq(missingIntentID), by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("missing allocation intent should be rejected: %v", err)
	}
	// A recorded intent for a DIFFERENT team role than requested: rejected.
	mismatchID := api.NewID("agt")
	if _, err = s.CreateAllocationIntent(ctx, task.ID, intentReq(mismatchID, api.TeamRoleExtra, api.NewID("run")), by); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AddAgent(ctx, task.ID, addReq(mismatchID), by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("mismatched allocation intent (extra recorded, member requested) should be rejected: %v", err)
	}
	// A second CreateAllocationIntent for the same agent identity (no
	// request-id) is a conflict, not an update -- an intent is authored once.
	if _, err = s.CreateAllocationIntent(ctx, task.ID, intentReq(mismatchID, api.TeamRoleMember, api.NewID("run")), by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("re-authoring an intent for the same agent identity should conflict: %v", err)
	}
	// A recorded intent whose ExpectedRunID is reused from another intent
	// is rejected at authoring time.
	reusedRun := api.NewID("run")
	if _, err = s.CreateAllocationIntent(ctx, task.ID, intentReq(api.NewID("agt"), api.TeamRoleMember, reusedRun), by); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CreateAllocationIntent(ctx, task.ID, intentReq(api.NewID("agt"), api.TeamRoleMember, reusedRun), by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("a reused expected-run-id should be rejected: %v", err)
	}
	// A context-digest mismatch at consumption is rejected.
	digestMismatchID := api.NewID("agt")
	if _, err = s.CreateAllocationIntent(ctx, task.ID, intentReq(digestMismatchID, api.TeamRoleMember, api.NewID("run")), by); err != nil {
		t.Fatal(err)
	}
	digestMismatchReq := addReq(digestMismatchID)
	digestMismatchReq.WorkItem.ContextBundle = otherBundle
	if _, err = s.AddAgent(ctx, task.ID, digestMismatchReq, by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("a changed-context admission attempt should be rejected: %v", err)
	}

	// A matching intent is admitted using the intent's ExpectedRunID as the
	// actual run (not a freshly generated one), and the intent record is
	// durably marked consumed by that exact run plus the actual launcher.
	matchID := api.NewID("agt")
	expectedRunID := api.NewID("run")
	if _, err = s.CreateAllocationIntent(ctx, task.ID, intentReq(matchID, api.TeamRoleMember, expectedRunID), by); err != nil {
		t.Fatal(err)
	}
	admitted, err := s.AddAgent(ctx, task.ID, addReq(matchID), by)
	if err != nil || admitted.WorkItem == nil || admitted.WorkItem.TeamRole != api.TeamRoleMember || admitted.RunID != expectedRunID {
		t.Fatalf("matching intent should admit using the preallocated expected run id: %+v %v", admitted, err)
	}
	intent, err := loadAllocationIntent(s.db, ctx, matchID)
	if err != nil || intent == nil || intent.ConsumedAt == nil || intent.ConsumedByRunID != admitted.RunID || intent.ConsumedByRunID != expectedRunID {
		t.Fatalf("intent should be marked consumed by the exact expected/resulting run: %+v %v", intent, err)
	}
	if intent.LauncherAgentID != lead.ID || intent.LauncherRunID != lead.RunID {
		t.Fatalf("intent should record the actual launcher identity: %+v", intent)
	}
	// GetAllocationIntent is a plain readback, available even after
	// consumption, and never mutates or re-consumes.
	readback, err := s.GetAllocationIntent(ctx, task.ID, matchID)
	if err != nil || readback.ConsumedAt == nil || readback.ExpectedRunID != expectedRunID {
		t.Fatalf("readback should show the consumed intent: %+v %v", readback, err)
	}

	// Idempotent retry: an identical retry (same RequestID and full
	// payload) returns the existing record, including after consumption --
	// never a second allowance. A retry reusing the RequestID with a
	// different payload conflicts.
	retryID := api.NewID("agt")
	retryRunID := api.NewID("run")
	retryReq := intentReq(retryID, api.TeamRoleMember, retryRunID)
	retryReq.RequestID = "intent-retry-1"
	first, err := s.CreateAllocationIntent(ctx, task.ID, retryReq, by)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.AddAgent(ctx, task.ID, addReq(retryID), by); err != nil {
		t.Fatalf("consuming the retry-keyed intent: %v", err)
	}
	replay, err := s.CreateAllocationIntent(ctx, task.ID, retryReq, by)
	if err != nil || replay.AgentID != first.AgentID || replay.ConsumedAt == nil {
		t.Fatalf("identical retry after consumption should return the durable consumed record, not error or reallocate: %+v %v", replay, err)
	}
	changedRetry := retryReq
	changedRetry.TeamRole = api.TeamRoleExtra
	if _, err = s.CreateAllocationIntent(ctx, task.ID, changedRetry, by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("a retry request-id reused with a different payload should conflict: %v", err)
	}
}

// TestAllocationIntentCrossTargetTaskRejected addresses independent review
// #2916: an intent authored for one target task must not authorize
// admission under a different one, even if every other field happens to
// match.
func TestAllocationIntentCrossTargetTaskRejected(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "cross-target-task.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	by := api.Caller{Node: "fixture", User: "owner"}
	task, err := s.CreateTask(ctx, api.CreateTaskRequest{Name: "Cross target task", Orchestrator: "lead", AllowAgentSpawn: true}, by)
	if err != nil {
		t.Fatal(err)
	}
	lead, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "lead", Host: "fixture", Session: "lead", Runtime: "codex"}, by)
	if err != nil {
		t.Fatal(err)
	}
	item, err := s.CreateWorkItem(ctx, task.ID, api.CreateWorkItemRequest{Kind: "bug", Title: "Cross target item", AgentID: lead.ID, RequestID: "cross-target-item"}, by)
	if err != nil {
		t.Fatal(err)
	}
	order := contextLinkedMessage(t, s, task, item, "cross-target order", "cross-target-order", nil)
	orderRef := api.MessageReference{TaskID: task.ID, Seq: order.Seq}
	bundle := syntheticPreparedContext(t, item, orderRef, syntheticHistory(item, order))
	digestBytes := sha256.Sum256(bundle)
	digest := hex.EncodeToString(digestBytes[:])
	agentID := api.NewID("agt")
	if _, err = s.CreateAllocationIntent(ctx, task.ID, api.CreateAllocationIntentRequest{
		AgentID: agentID, TargetTaskID: task.ID, ItemTaskID: task.ID, ItemID: item.ID, ItemRevision: item.Revision, WorkOrderMessage: orderRef,
		ContextDigest: digest, TeamRole: api.TeamRoleMember, AuthorAgentID: lead.ID, AuthorRunID: lead.RunID, ExpectedRunID: api.NewID("run"),
	}, by); err != nil {
		t.Fatal(err)
	}
	// Simulate a data-level cross-task mismatch directly: CreateAllocationIntent
	// itself always pins target_task_id to its own taskID parameter, so the
	// only way this field could ever diverge from the admitting task is a
	// row that did not go through that path (a bug elsewhere, or a future
	// regression). Store.AddAgent's consumption check must catch it
	// independently of the authoring-time guarantee, not rely on it alone.
	if _, err = s.db.ExecContext(ctx, `UPDATE agent_allocation_intents SET target_task_id=? WHERE agent_id=?`, "tsk_0000000000000099", agentID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AddAgent(ctx, task.ID, api.AddAgentRequest{
		AgentID: agentID, Name: "cross-target-worker", Host: "fixture", Session: "cross-target-worker", Runtime: "codex", ParentAgentID: lead.ID,
		WorkItem: &api.AgentWorkItemRequest{ItemTaskID: task.ID, ItemID: item.ID, ItemRevision: item.Revision, WorkOrderMessage: orderRef, ContextBundle: bundle, TeamRole: api.TeamRoleMember},
	}, by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("admission whose intent's recorded target task does not match the admitting task should be rejected: %v", err)
	}
}

// TestAllocationIntentLegacyEmptyFieldsCannotAuthorize addresses
// independent review #2916: an intent row upgraded from before the
// expected-run/context/author binding existed (new columns blank) must
// never authorize a fresh admission -- new optional-shaped columns cannot
// reopen the missing-intent bypass.
func TestAllocationIntentLegacyEmptyFieldsCannotAuthorize(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "legacy-intent.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	by := api.Caller{Node: "fixture", User: "owner"}
	task, err := s.CreateTask(ctx, api.CreateTaskRequest{Name: "Legacy intent", Orchestrator: "lead", AllowAgentSpawn: true}, by)
	if err != nil {
		t.Fatal(err)
	}
	lead, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "lead", Host: "fixture", Session: "lead", Runtime: "codex"}, by)
	if err != nil {
		t.Fatal(err)
	}
	item, err := s.CreateWorkItem(ctx, task.ID, api.CreateWorkItemRequest{Kind: "bug", Title: "Legacy intent item", AgentID: lead.ID, RequestID: "legacy-intent-item"}, by)
	if err != nil {
		t.Fatal(err)
	}
	order := contextLinkedMessage(t, s, task, item, "legacy intent order", "legacy-intent-order", nil)
	orderRef := api.MessageReference{TaskID: task.ID, Seq: order.Seq}
	bundle := syntheticPreparedContext(t, item, orderRef, syntheticHistory(item, order))
	agentID := api.NewID("agt")
	// Simulate a row written before #2916: only the original fields set,
	// new columns left at their migration DEFAULT ''.
	if _, err = s.db.ExecContext(ctx, `INSERT INTO agent_allocation_intents
(agent_id,item_task_id,item_id,item_revision,work_order_task_id,work_order_message_seq,team_role,created_by_node,created_by_user,created_at)
VALUES(?,?,?,?,?,?,?,?,?,?)`, agentID, task.ID, item.ID, item.Revision, orderRef.TaskID, orderRef.Seq, api.TeamRoleMember, by.Node, by.User, ts(s.now())); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AddAgent(ctx, task.ID, api.AddAgentRequest{
		AgentID: agentID, Name: "legacy-worker", Host: "fixture", Session: "legacy-worker", Runtime: "codex", ParentAgentID: lead.ID,
		WorkItem: &api.AgentWorkItemRequest{ItemTaskID: task.ID, ItemID: item.ID, ItemRevision: item.Revision, WorkOrderMessage: orderRef, ContextBundle: bundle, TeamRole: api.TeamRoleMember},
	}, by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("a legacy intent with empty expected-run/context/author fields should never authorize admission: %v", err)
	}
}

// TestItemExtraCapacityLegacyParentedBindingCountsConservatively addresses
// independent review #2300/#2771 finding 4: a legacy binding predating this
// correction (team_role=”, never reclassified) must not be silently
// excluded from the item's active-extra count -- that would let fresh
// extras stack on top of it past the owner's intended ceiling. A parented
// legacy binding is conservatively counted as an extra; a parentless one
// (never ambiguous -- it was never an extra) is not.
func TestItemExtraCapacityLegacyParentedBindingCountsConservatively(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "legacy-capacity.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	by := api.Caller{Node: "fixture", User: "owner"}
	one := 1
	task, err := s.CreateTask(ctx, api.CreateTaskRequest{Name: "Legacy capacity", Orchestrator: "lead", AllowAgentSpawn: true, MaxNewAgents: &one}, by)
	if err != nil {
		t.Fatal(err)
	}
	lead, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "lead", Host: "fixture", Session: "lead", Runtime: "codex"}, by)
	if err != nil {
		t.Fatal(err)
	}
	item, err := s.CreateWorkItem(ctx, task.ID, api.CreateWorkItemRequest{Kind: "bug", Title: "Legacy capacity item", AgentID: lead.ID, RequestID: "legacy-capacity-item"}, by)
	if err != nil {
		t.Fatal(err)
	}
	legacyExtra, err := authorIntentAndBind(t, s, task, item, "legacy-extra", lead.ID, lead, api.TeamRoleExtra, by)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a binding written before this correction ever ran: directly
	// blank its persisted team_role, exactly as a real pre-migration row
	// would read.
	if _, err = s.db.ExecContext(ctx, `UPDATE agent_work_item_bindings SET team_role='' WHERE agent_id=?`, legacyExtra.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = authorIntentAndBind(t, s, task, item, "fresh-extra", lead.ID, lead, api.TeamRoleExtra, by); !errors.Is(err, api.ErrAgentSpawnLimit) {
		t.Fatalf("legacy parented binding should conservatively count against the allowance: %v", err)
	}
	// A parentless legacy binding was never an extra and still isn't.
	baseOrder := contextLinkedMessage(t, s, task, item, "legacy base order", "legacy-base-order", nil)
	baseReq := &api.AgentWorkItemRequest{ItemTaskID: task.ID, ItemID: item.ID, ItemRevision: item.Revision, WorkOrderMessage: api.MessageReference{TaskID: task.ID, Seq: baseOrder.Seq}}
	baseReq.ContextBundle = syntheticPreparedContext(t, item, baseReq.WorkOrderMessage, syntheticHistory(item, baseOrder))
	base, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "legacy-base", Host: "fixture", Session: "legacy-base", Runtime: "codex", WorkItem: baseReq}, by)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.ExecContext(ctx, `UPDATE agent_work_item_bindings SET team_role='' WHERE agent_id=?`, base.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = authorIntentAndBind(t, s, task, item, "fresh-extra-2", lead.ID, lead, api.TeamRoleExtra, by); !errors.Is(err, api.ErrAgentSpawnLimit) {
		t.Fatalf("expected the same rejection (unaffected by the parentless legacy row): %v", err)
	}
}
