package server

import (
	"context"
	"crypto/sha256"
	"fmt"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

func TestReliableDeliveryHTTPRoundTrip(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	var task api.Task
	if code := c.do("POST", "/v1/tasks", api.CreateTaskRequest{Name: "Delivery HTTP", Orchestrator: "lead"}, &task); code != 201 {
		t.Fatalf("create task: %d", code)
	}
	lead := c.agent(task, "lead")
	item, err := c.st.CreateWorkItem(ctx, task.ID, api.CreateWorkItemRequest{Kind: "feature", Title: "Delivery fixture", AgentID: lead.ID, RequestID: "http-delivery-item"}, c.who)
	if err != nil {
		t.Fatal(err)
	}
	order, err := c.st.PostMessage(ctx, task.ID, api.PostMessageRequest{Text: "HTTP order", RequestID: "http-delivery-order", WorkItems: []api.MessageWorkItem{{ItemTaskID: task.ID, ItemID: item.ID, ItemRevision: item.Revision, Relationship: "primary"}}}, c.who)
	if err != nil {
		t.Fatal(err)
	}
	orderRef := api.MessageReference{TaskID: task.ID, Seq: order.Seq}
	workerID := api.NewID("agt")
	bundle := syntheticServerTestContext(t, item, orderRef, order)
	var worker api.Agent
	if code := c.do("POST", "/v1/tasks/"+task.ID+"/agents", api.AddAgentRequest{AgentID: workerID, Name: "worker", Host: "fixture", Session: "worker", Runtime: "codex", WorkItem: &api.AgentWorkItemRequest{ItemTaskID: task.ID, ItemID: item.ID, ItemRevision: item.Revision, WorkOrderMessage: orderRef, ContextBundle: bundle}}, &worker); code != 201 {
		t.Fatalf("add worker: %d", code)
	}
	directive, err := c.st.PostMessage(ctx, task.ID, api.PostMessageRequest{Text: "Current HTTP directive", RequestID: "http-directive-message", WorkItems: []api.MessageWorkItem{{ItemTaskID: task.ID, ItemID: item.ID, ItemRevision: item.Revision, Relationship: "primary"}}, WorkOrderMessage: &orderRef}, c.who)
	if err != nil {
		t.Fatal(err)
	}
	instructionBytes := int64(len([]byte(directive.Text)))
	instructionSHA := fmt.Sprintf("%x", sha256.Sum256([]byte(directive.Text)))
	create := api.CreateRequiredDeliveryRequest{RequestID: "http-delivery-create", MessageSeq: directive.Seq, Kind: api.DeliveryAssignment, AgentID: worker.ID, RunID: worker.RunID, ItemTaskID: task.ID, ItemID: item.ID, ItemRevision: item.Revision, WorkOrderMessage: orderRef, GoverningOrderMessage: orderRef, InstructionSHA256: instructionSHA, InstructionBytes: instructionBytes, EnrollmentVersion: api.ReliableFollowThroughCapabilityVersion, ProducerAgentID: lead.ID, ProducerRunID: lead.RunID}
	var created api.DeliveryMutation
	if code := c.do("POST", "/v1/tasks/"+task.ID+"/required-deliveries", create, &created); code != 201 || created.Delivery.ContextDigest == "" {
		t.Fatalf("create delivery: %d %+v", code, created)
	}
	var current api.RequiredDelivery
	if code := c.do("GET", "/v1/tasks/"+task.ID+"/agents/"+worker.ID+"/current-assignment?runId="+worker.RunID, nil, &current); code != 200 || current.ID != created.Delivery.ID {
		t.Fatalf("current assignment: %d %+v", code, current)
	}
	var coverage api.DeliveryCoverage
	if code := c.do("GET", "/v1/tasks/"+task.ID+"/agents/"+worker.ID+"/delivery-coverage?runId="+worker.RunID, nil, &coverage); code != 200 || coverage.Status != "covered" || coverage.Delivery == nil {
		t.Fatalf("delivery coverage: %d %+v", code, coverage)
	}
	ack := api.DeliveryActionRequest{RequestID: "http-delivery-ack", AgentID: worker.ID, RunID: worker.RunID, ExpectedEpoch: 1}
	var acknowledged api.DeliveryMutation
	if code := c.do("POST", "/v1/tasks/"+task.ID+"/deliveries/"+created.Delivery.ID+"/ack", ack, &acknowledged); code != 200 || acknowledged.Delivery.Phase != api.DeliveryAcknowledged {
		t.Fatalf("ack delivery: %d %+v", code, acknowledged)
	}
	check := api.DeliveryFollowThroughCheckRequest{RequestID: "http-followthrough-check", AgentID: worker.ID, RunID: worker.RunID, ExpectedGeneration: 1, ExpectedEpoch: 1, Observation: api.DeliveryRuntimeObservation{State: api.DeliveryObservationUnknown, Source: "http-test", ObservedAt: time.Now().UTC()}}
	var decision api.DeliveryFollowThroughDecision
	if code := c.do("POST", "/v1/tasks/"+task.ID+"/deliveries/"+created.Delivery.ID+"/follow-through/check", check, &decision); code != 200 || decision.Classification != api.DeliveryFollowThroughAwaitingProgress || decision.Execute {
		t.Fatalf("follow-through check: %d %+v", code, decision)
	}
	var replay api.DeliveryMutation
	if code := c.do("POST", "/v1/tasks/"+task.ID+"/deliveries/"+created.Delivery.ID+"/ack", ack, &replay); code != 200 || !replay.Replay || replay.Receipt.ID != acknowledged.Receipt.ID {
		t.Fatalf("ack replay: %d %+v", code, replay)
	}
	bad := ack
	bad.RunID = api.NewID("run")
	bad.ExpectedEpoch = 1
	if code := c.do("POST", "/v1/tasks/"+task.ID+"/deliveries/"+created.Delivery.ID+"/progress", bad, nil); code != 409 {
		t.Fatalf("wrong-run progress: %d", code)
	}
	blockReq := api.DeliveryBlockRequest{RequestID: "http-delivery-block", AgentID: worker.ID, RunID: worker.RunID, ExpectedEpoch: 1, ReasonClass: "dependency", Text: "fixture dependency"}
	var blocked api.DeliveryMutation
	if code := c.do("POST", "/v1/tasks/"+task.ID+"/deliveries/"+created.Delivery.ID+"/blocks", blockReq, &blocked); code != 200 || blocked.Block == nil {
		t.Fatalf("block delivery: %d %+v", code, blocked)
	}
	resolveReq := api.DeliveryResolutionRequest{RequestID: "http-delivery-resolve", AgentID: lead.ID, RunID: lead.RunID, ExpectedEpoch: 1, Text: "fixture resolved"}
	var resolved api.DeliveryMutation
	if code := c.do("POST", "/v1/tasks/"+task.ID+"/deliveries/"+created.Delivery.ID+"/blocks/"+blocked.Block.ID+"/resolutions", resolveReq, &resolved); code != 200 || resolved.Block == nil || resolved.Block.ResolutionID == "" {
		t.Fatalf("resolve delivery: %d %+v", code, resolved)
	}
	resumeReq := api.DeliveryResumeRequest{RequestID: "http-delivery-resume", AgentID: worker.ID, RunID: worker.RunID, ExpectedEpoch: 1, ResolutionID: resolved.Block.ResolutionID}
	if code := c.do("POST", "/v1/tasks/"+task.ID+"/deliveries/"+created.Delivery.ID+"/resume", resumeReq, nil); code != 200 {
		t.Fatalf("resume delivery: %d", code)
	}
	staleResult := api.DeliveryActionRequest{RequestID: "http-stale-result", AgentID: worker.ID, RunID: worker.RunID, ExpectedEpoch: 1, Text: "obsolete result"}
	if code := c.do("POST", "/v1/tasks/"+task.ID+"/deliveries/"+created.Delivery.ID+"/result", staleResult, nil); code != 409 {
		t.Fatalf("stale pre-resume result: %d", code)
	}
	var afterStale api.RequiredDelivery
	if code := c.do("GET", "/v1/tasks/"+task.ID+"/agents/"+worker.ID+"/current-assignment?runId="+worker.RunID, nil, &afterStale); code != 200 || afterStale.ExecutionEpoch != 2 || afterStale.Phase != api.DeliveryAcknowledged {
		t.Fatalf("stale result mutated resumed execution: %d %+v", code, afterStale)
	}
	var caps api.Capabilities
	if code := c.do("GET", "/v1/capabilities", nil, &caps); code != 200 || !caps.ReliableDelivery.Supported || len(caps.ReliableDelivery.Versions) != 2 || caps.ReliableDelivery.Versions[0] != api.ReliableDeliveryCapabilityVersion || caps.ReliableDelivery.Versions[1] != api.ReliableFollowThroughCapabilityVersion {
		t.Fatalf("delivery capability: %d %+v", code, caps.ReliableDelivery)
	}
}
