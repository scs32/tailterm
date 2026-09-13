package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

type deliveryFixture struct {
	s      *Store
	path   string
	by     api.Caller
	task   api.Task
	lead   api.Agent
	worker api.Agent
	item   api.WorkItem
	order  api.Message
}

func newDeliveryFixture(t *testing.T) deliveryFixture {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "delivery.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	fixed := time.Date(2026, 9, 13, 17, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return fixed }
	by := api.Caller{Node: "fixture", User: "owner"}
	task, err := s.CreateTask(ctx, api.CreateTaskRequest{Name: "Directive core", Orchestrator: "lead"}, by)
	if err != nil {
		t.Fatal(err)
	}
	lead, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "lead", Host: "fixture", Session: "lead", Runtime: "codex"}, by)
	if err != nil {
		t.Fatal(err)
	}
	item, err := s.CreateWorkItem(ctx, task.ID, api.CreateWorkItemRequest{Kind: "feature", Title: "Synthetic directive", AgentID: lead.ID, RequestID: "delivery-item"}, by)
	if err != nil {
		t.Fatal(err)
	}
	order := contextLinkedMessage(t, s, task, item, "Bounded work order", "delivery-order", nil)
	orderRef := api.MessageReference{TaskID: task.ID, Seq: order.Seq}
	bundle := syntheticPreparedContext(t, item, orderRef, syntheticHistory(item, order))
	worker, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "worker", Host: "fixture", Session: "worker", Runtime: "codex", WorkItem: &api.AgentWorkItemRequest{
		ItemTaskID: task.ID, ItemID: item.ID, ItemRevision: item.Revision, WorkOrderMessage: orderRef, ContextBundle: bundle,
	}}, by)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return deliveryFixture{s: s, path: path, by: by, task: task, lead: lead, worker: worker, item: item, order: order}
}

func (f deliveryFixture) directive(t *testing.T, text, key, kind string, expected int64, expectedID string) api.DeliveryMutation {
	t.Helper()
	message := contextLinkedMessage(t, f.s, f.task, f.item, text, key+"-message", &api.MessageReference{TaskID: f.task.ID, Seq: f.order.Seq})
	out, err := f.s.CreateRequiredDelivery(context.Background(), f.task.ID, api.CreateRequiredDeliveryRequest{
		RequestID: key, MessageSeq: message.Seq, Kind: kind, AgentID: f.worker.ID, RunID: f.worker.RunID,
		ItemTaskID: f.item.TaskID, ItemID: f.item.ID, ItemRevision: f.item.Revision,
		WorkOrderMessage:          api.MessageReference{TaskID: f.task.ID, Seq: f.order.Seq},
		ExpectedCurrentGeneration: expected, ExpectedCurrentDeliveryID: expectedID,
		ProducerAgentID: f.lead.ID, ProducerRunID: f.lead.RunID,
	}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestDirectiveSupersessionDirectAckAndReplay(t *testing.T) {
	f := newDeliveryFixture(t)
	ctx := context.Background()
	a := f.directive(t, "Assignment A", "delivery-a", api.DeliveryAssignment, 0, "")
	message := contextLinkedMessage(t, f.s, f.task, f.item, "Missing CAS identity", "delivery-missing-cas-message", &api.MessageReference{TaskID: f.task.ID, Seq: f.order.Seq})
	if _, err := f.s.CreateRequiredDelivery(ctx, f.task.ID, api.CreateRequiredDeliveryRequest{
		RequestID: "delivery-missing-cas", MessageSeq: message.Seq, Kind: api.DeliveryAmendment,
		AgentID: f.worker.ID, RunID: f.worker.RunID, ItemTaskID: f.item.TaskID, ItemID: f.item.ID,
		ItemRevision: f.item.Revision, WorkOrderMessage: api.MessageReference{TaskID: f.task.ID, Seq: f.order.Seq},
		ExpectedCurrentGeneration: 1, ProducerAgentID: f.lead.ID, ProducerRunID: f.lead.RunID,
	}, f.by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("supersession without exact current delivery ID should conflict: %v", err)
	}
	b := f.directive(t, "Changed assignment B", "delivery-b", api.DeliveryAmendment, 1, a.Delivery.ID)
	if b.Delivery.Generation != 2 || b.Delivery.SupersedesID != a.Delivery.ID || b.Delivery.Phase != api.DeliveryUnacknowledged {
		t.Fatalf("bad supersession: %+v", b)
	}
	current, err := f.s.CurrentAssignment(ctx, f.task.ID, f.worker.ID, f.worker.RunID)
	if err != nil || current.ID != b.Delivery.ID {
		t.Fatalf("current assignment: %+v %v", current, err)
	}
	if current.Message == nil || current.Message.Text != "Changed assignment B" || len(current.Events) != 1 || current.Events[0].Kind != "stored" || current.Events[0].Sequence != 1 {
		t.Fatalf("current assignment omitted immutable message/event evidence: %+v", current)
	}
	createA := api.CreateRequiredDeliveryRequest{
		RequestID: "delivery-a", MessageSeq: a.Delivery.MessageSeq, Kind: api.DeliveryAssignment,
		AgentID: f.worker.ID, RunID: f.worker.RunID, ItemTaskID: f.item.TaskID, ItemID: f.item.ID,
		ItemRevision: f.item.Revision, WorkOrderMessage: api.MessageReference{TaskID: f.task.ID, Seq: f.order.Seq},
		ProducerAgentID: f.lead.ID, ProducerRunID: f.lead.RunID,
	}
	createReplay, err := f.s.CreateRequiredDelivery(ctx, f.task.ID, createA, f.by)
	if err != nil || !createReplay.Replay || createReplay.Receipt.ID != a.Receipt.ID {
		t.Fatalf("historical create receipt replay: %+v %v", createReplay, err)
	}
	current, err = f.s.CurrentAssignment(ctx, f.task.ID, f.worker.ID, f.worker.RunID)
	if err != nil || current.ID != b.Delivery.ID {
		t.Fatalf("historical create replay revived obsolete execution: %+v %v", current, err)
	}
	createA.Kind = api.DeliveryReview
	if _, err = f.s.CreateRequiredDelivery(ctx, f.task.ID, createA, f.by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("changed create replay should conflict: %v", err)
	}
	ackA := api.DeliveryActionRequest{RequestID: "ack-a", AgentID: f.worker.ID, RunID: f.worker.RunID, ExpectedEpoch: 1}
	if _, err = f.s.AcknowledgeDelivery(ctx, f.task.ID, a.Delivery.ID, ackA, f.by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("stale A ack should conflict: %v", err)
	}
	ackB := api.DeliveryActionRequest{RequestID: "ack-b", AgentID: f.worker.ID, RunID: f.worker.RunID, ExpectedEpoch: 1}
	first, err := f.s.AcknowledgeDelivery(ctx, f.task.ID, b.Delivery.ID, ackB, f.by)
	if err != nil || first.Delivery.Phase != api.DeliveryAcknowledged || first.Event.Sequence != 2 {
		t.Fatalf("direct B ack with zero wake attempts: %+v %v", first, err)
	}
	replay, err := f.s.AcknowledgeDelivery(ctx, f.task.ID, b.Delivery.ID, ackB, f.by)
	if err != nil || !replay.Replay || replay.Receipt.ID != first.Receipt.ID || replay.Event.ID != first.Event.ID {
		t.Fatalf("ack replay: %+v %v", replay, err)
	}
	changed := ackB
	changed.Text = "changed payload"
	if _, err = f.s.AcknowledgeDelivery(ctx, f.task.ID, b.Delivery.ID, changed, f.by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("changed replay should conflict: %v", err)
	}
	var attempts int
	if err = f.s.db.QueryRow(`SELECT COUNT(*) FROM delivery_events WHERE delivery_id=? AND kind LIKE 'wake_%'`, b.Delivery.ID).Scan(&attempts); err != nil || attempts != 0 {
		t.Fatalf("manual ack unexpectedly required wake evidence: %d %v", attempts, err)
	}
	if err = f.s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	f.s = reopened
	t.Cleanup(func() { reopened.Close() })
	lostResponseReplay, err := reopened.AcknowledgeDelivery(ctx, f.task.ID, b.Delivery.ID, ackB, f.by)
	if err != nil || !lostResponseReplay.Replay || lostResponseReplay.Receipt.ID != first.Receipt.ID || lostResponseReplay.Event.ID != first.Event.ID {
		t.Fatalf("committed response-loss replay after restart: %+v %v", lostResponseReplay, err)
	}
}

func TestDirectiveBlockResolutionResumeAndResult(t *testing.T) {
	f := newDeliveryFixture(t)
	ctx := context.Background()
	d := f.directive(t, "Assignment", "delivery-flow", api.DeliveryAssignment, 0, "")
	ack, err := f.s.AcknowledgeDelivery(ctx, f.task.ID, d.Delivery.ID, api.DeliveryActionRequest{RequestID: "flow-ack", AgentID: f.worker.ID, RunID: f.worker.RunID, ExpectedEpoch: 1}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	progress, err := f.s.ProgressDelivery(ctx, f.task.ID, d.Delivery.ID, api.DeliveryActionRequest{RequestID: "flow-progress", AgentID: f.worker.ID, RunID: f.worker.RunID, ExpectedEpoch: 1, Text: "first checkpoint"}, f.by)
	if err != nil || progress.Delivery.Phase != api.DeliveryProgressing {
		t.Fatalf("progress: %+v %v", progress, err)
	}
	blocked, err := f.s.BlockDelivery(ctx, f.task.ID, d.Delivery.ID, api.DeliveryBlockRequest{RequestID: "flow-block", AgentID: f.worker.ID, RunID: f.worker.RunID, ExpectedEpoch: ack.Delivery.ExecutionEpoch, ReasonClass: "dependency", Text: "waiting on fixture"}, f.by)
	if err != nil || blocked.Block == nil || blocked.Delivery.Phase != api.DeliveryBlocked {
		t.Fatalf("block: %+v %v", blocked, err)
	}
	if _, err = f.s.ResumeDelivery(ctx, f.task.ID, d.Delivery.ID, api.DeliveryResumeRequest{RequestID: "early-resume", AgentID: f.worker.ID, RunID: f.worker.RunID, ExpectedEpoch: 1, ResolutionID: api.NewID("drsl")}, f.by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("unresolved resume should conflict: %v", err)
	}
	if _, err = f.s.ResolveDeliveryBlock(ctx, f.task.ID, d.Delivery.ID, blocked.Block.ID, api.DeliveryResolutionRequest{RequestID: "worker-dependency-resolve", AgentID: f.worker.ID, RunID: f.worker.RunID, ExpectedEpoch: 1, Text: "self-approved dependency"}, f.by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("worker must not resolve a dependency-class block: %v", err)
	}
	resolved, err := f.s.ResolveDeliveryBlock(ctx, f.task.ID, d.Delivery.ID, blocked.Block.ID, api.DeliveryResolutionRequest{RequestID: "flow-resolve", AgentID: f.lead.ID, RunID: f.lead.RunID, ExpectedEpoch: 1, Text: "dependency delivered"}, f.by)
	if err != nil || resolved.Block == nil || !resolved.Block.Resolved {
		t.Fatalf("resolve: %+v %v", resolved, err)
	}
	resolveReplay, err := f.s.ResolveDeliveryBlock(ctx, f.task.ID, d.Delivery.ID, blocked.Block.ID, api.DeliveryResolutionRequest{RequestID: "flow-resolve", AgentID: f.lead.ID, RunID: f.lead.RunID, ExpectedEpoch: 1, Text: "dependency delivered"}, f.by)
	if err != nil || !resolveReplay.Replay || resolveReplay.Receipt.ID != resolved.Receipt.ID || resolveReplay.Block.ResolutionID != resolved.Block.ResolutionID {
		t.Fatalf("resolution replay: %+v %v", resolveReplay, err)
	}
	if _, err = f.s.ResolveDeliveryBlock(ctx, f.task.ID, d.Delivery.ID, blocked.Block.ID, api.DeliveryResolutionRequest{RequestID: "flow-resolve", AgentID: f.lead.ID, RunID: f.lead.RunID, ExpectedEpoch: 1, Text: "changed resolution"}, f.by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("changed resolution replay should conflict: %v", err)
	}
	current, err := f.s.CurrentAssignment(ctx, f.task.ID, f.worker.ID, f.worker.RunID)
	if err != nil || current.CurrentBlock == nil || !current.CurrentBlock.Resolved || current.CurrentBlock.ResolutionID != resolved.Block.ResolutionID {
		t.Fatalf("current lookup omitted resolved block evidence: %+v %v", current, err)
	}
	resumed, err := f.s.ResumeDelivery(ctx, f.task.ID, d.Delivery.ID, api.DeliveryResumeRequest{RequestID: "flow-resume", AgentID: f.worker.ID, RunID: f.worker.RunID, ExpectedEpoch: 1, ResolutionID: resolved.Block.ResolutionID}, f.by)
	if err != nil || resumed.Delivery.ExecutionEpoch != 2 || resumed.Delivery.Phase != api.DeliveryAcknowledged {
		t.Fatalf("resume: %+v %v", resumed, err)
	}
	if _, err = f.s.ProgressDelivery(ctx, f.task.ID, d.Delivery.ID, api.DeliveryActionRequest{RequestID: "stale-progress", AgentID: f.worker.ID, RunID: f.worker.RunID, ExpectedEpoch: 1, Text: "obsolete epoch"}, f.by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("stale epoch progress should conflict: %v", err)
	}
	result, err := f.s.ResultDelivery(ctx, f.task.ID, d.Delivery.ID, api.DeliveryActionRequest{RequestID: "flow-result", AgentID: f.worker.ID, RunID: f.worker.RunID, ExpectedEpoch: 2, Text: "candidate evidence only"}, f.by)
	if err != nil || result.Delivery.Phase != api.DeliveryResult {
		t.Fatalf("result: %+v %v", result, err)
	}
	item, err := f.s.GetWorkItem(ctx, f.item.TaskID, f.item.ID)
	if err != nil || item.Status == "done" {
		t.Fatalf("delivery result must not complete item: %+v %v", item, err)
	}
}

func TestDirectiveRestartRetirementAndWrongRun(t *testing.T) {
	f := newDeliveryFixture(t)
	ctx := context.Background()
	d := f.directive(t, "Persistent assignment", "delivery-persist", api.DeliveryAssignment, 0, "")
	if _, err := f.s.UpdateAgent(ctx, f.worker.ID, api.UpdateAgentRequest{Status: ptr(api.AgentRetired)}, f.by); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.AcknowledgeDelivery(ctx, f.task.ID, d.Delivery.ID, api.DeliveryActionRequest{RequestID: "retired-ack", AgentID: f.worker.ID, RunID: f.worker.RunID, ExpectedEpoch: 1}, f.by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("retired run ack should conflict: %v", err)
	}
	if _, err := f.s.CurrentAssignment(ctx, f.task.ID, f.worker.ID, api.NewID("run")); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("wrong run lookup should conflict: %v", err)
	}
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	f.s = reopened
	t.Cleanup(func() { reopened.Close() })
	current, err := reopened.CurrentAssignment(ctx, f.task.ID, f.worker.ID, f.worker.RunID)
	if err != nil || current.ID != d.Delivery.ID || current.Generation != 1 {
		t.Fatalf("restart persistence: %+v %v", current, err)
	}
}

func TestDirectiveClosedRunAndTerminalReplayStayFenced(t *testing.T) {
	f := newDeliveryFixture(t)
	ctx := context.Background()
	d := f.directive(t, "Terminal assignment", "delivery-terminal", api.DeliveryAssignment, 0, "")
	ackReq := api.DeliveryActionRequest{RequestID: "terminal-ack", AgentID: f.worker.ID, RunID: f.worker.RunID, ExpectedEpoch: 1}
	ack, err := f.s.AcknowledgeDelivery(ctx, f.task.ID, d.Delivery.ID, ackReq, f.by)
	if err != nil {
		t.Fatal(err)
	}
	resultReq := api.DeliveryActionRequest{RequestID: "terminal-result", AgentID: f.worker.ID, RunID: f.worker.RunID, ExpectedEpoch: 1, Text: "saved result evidence"}
	result, err := f.s.ResultDelivery(ctx, f.task.ID, d.Delivery.ID, resultReq, f.by)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.CloseAgentRun(ctx, f.worker.ID, f.worker.RunID, f.by); err != nil {
		t.Fatal(err)
	}
	replay, err := f.s.ResultDelivery(ctx, f.task.ID, d.Delivery.ID, resultReq, f.by)
	if err != nil || !replay.Replay || replay.Receipt.ID != result.Receipt.ID {
		t.Fatalf("historical terminal receipt must replay after close: %+v %v", replay, err)
	}
	if _, err = f.s.ProgressDelivery(ctx, f.task.ID, d.Delivery.ID, api.DeliveryActionRequest{RequestID: "closed-progress", AgentID: f.worker.ID, RunID: f.worker.RunID, ExpectedEpoch: 1, Text: "must not run"}, f.by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("closed exact run progress should conflict: %v", err)
	}
	if _, err = f.s.CurrentAssignment(ctx, f.task.ID, f.worker.ID, f.worker.RunID); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("closed exact run should have no actionable current assignment: %v", err)
	}
	if ack.Delivery.Phase != api.DeliveryAcknowledged {
		t.Fatalf("original ack evidence changed: %+v", ack)
	}
}

func TestDirectiveRejectsUnadmittedOrderAndBindingDrift(t *testing.T) {
	f := newDeliveryFixture(t)
	ctx := context.Background()
	otherOrder := contextLinkedMessage(t, f.s, f.task, f.item, "Other valid item order", "delivery-other-order", nil)
	directiveMessage := contextLinkedMessage(t, f.s, f.task, f.item, "Unadmitted order directive", "delivery-other-order-message", &api.MessageReference{TaskID: f.task.ID, Seq: otherOrder.Seq})
	if _, err := f.s.CreateRequiredDelivery(ctx, f.task.ID, api.CreateRequiredDeliveryRequest{
		RequestID: "delivery-other-order-create", MessageSeq: directiveMessage.Seq, Kind: api.DeliveryAssignment,
		AgentID: f.worker.ID, RunID: f.worker.RunID, ItemTaskID: f.item.TaskID, ItemID: f.item.ID,
		ItemRevision: f.item.Revision, WorkOrderMessage: api.MessageReference{TaskID: f.task.ID, Seq: otherOrder.Seq},
		ProducerAgentID: f.lead.ID, ProducerRunID: f.lead.RunID,
	}, f.by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("directive must not relabel the admitted context with another linked order: %v", err)
	}
	d := f.directive(t, "Bound directive", "delivery-bound", api.DeliveryAssignment, 0, "")
	if _, err := f.s.db.Exec(`UPDATE required_deliveries SET context_digest='drifted' WHERE id=?`, d.Delivery.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.AcknowledgeDelivery(ctx, f.task.ID, d.Delivery.ID, api.DeliveryActionRequest{RequestID: "drifted-ack", AgentID: f.worker.ID, RunID: f.worker.RunID, ExpectedEpoch: 1}, f.by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("new mutation must join the retained exact binding: %v", err)
	}
	if _, err := f.s.CurrentAssignment(ctx, f.task.ID, f.worker.ID, f.worker.RunID); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("current pickup must reject incoherent binding provenance: %v", err)
	}
}

func TestDirectiveClosedBlockedRunRejectsNewResolution(t *testing.T) {
	f := newDeliveryFixture(t)
	ctx := context.Background()
	d := f.directive(t, "Blocked terminal assignment", "delivery-blocked-terminal", api.DeliveryAssignment, 0, "")
	if _, err := f.s.AcknowledgeDelivery(ctx, f.task.ID, d.Delivery.ID, api.DeliveryActionRequest{RequestID: "blocked-terminal-ack", AgentID: f.worker.ID, RunID: f.worker.RunID, ExpectedEpoch: 1}, f.by); err != nil {
		t.Fatal(err)
	}
	blocked, err := f.s.BlockDelivery(ctx, f.task.ID, d.Delivery.ID, api.DeliveryBlockRequest{RequestID: "blocked-terminal-block", AgentID: f.worker.ID, RunID: f.worker.RunID, ExpectedEpoch: 1, ReasonClass: "dependency", Text: "will outlive worker"}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.CloseAgentRun(ctx, f.worker.ID, f.worker.RunID, f.by); err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.ResolveDeliveryBlock(ctx, f.task.ID, d.Delivery.ID, blocked.Block.ID, api.DeliveryResolutionRequest{RequestID: "closed-run-resolution", AgentID: f.lead.ID, RunID: f.lead.RunID, ExpectedEpoch: 1, Text: "too late"}, f.by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("new resolution for closed recipient should conflict: %v", err)
	}
}

func ptr[T any](value T) *T { return &value }
