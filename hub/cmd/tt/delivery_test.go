package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/server"
	"github.com/scs32/tailterm/hub/internal/store"
)

func TestDeliveryCLIHTTPStoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "delivery-cli.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	by := api.Caller{Node: "fixture", User: "owner"}
	task, err := st.CreateTask(ctx, api.CreateTaskRequest{Name: "Delivery CLI", Orchestrator: "lead"}, by)
	if err != nil {
		t.Fatal(err)
	}
	lead, err := st.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "lead", Host: "fixture", Session: "lead", Runtime: "codex"}, by)
	if err != nil {
		t.Fatal(err)
	}
	item, err := st.CreateWorkItem(ctx, task.ID, api.CreateWorkItemRequest{Kind: "feature", Title: "CLI directive", AgentID: lead.ID, RequestID: "cli-delivery-item"}, by)
	if err != nil {
		t.Fatal(err)
	}
	order, err := st.PostMessage(ctx, task.ID, api.PostMessageRequest{Text: "CLI order", RequestID: "cli-delivery-order", WorkItems: []api.MessageWorkItem{{ItemTaskID: task.ID, ItemID: item.ID, ItemRevision: item.Revision, Relationship: "primary"}}}, by)
	if err != nil {
		t.Fatal(err)
	}
	orderRef := api.MessageReference{TaskID: task.ID, Seq: order.Seq}
	revision := api.WorkItemRevision{ItemID: item.ID, TaskID: item.TaskID, Kind: item.Kind, Title: item.Title, Status: item.Status, Priority: item.Priority, ItemSeq: item.Seq, Revision: item.Revision, CreatedBy: item.CreatedBy, UpdatedBy: item.UpdatedBy, CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt, AttributionKind: "shared_workspace_claim", ChangeKind: "created", Provenance: "native"}
	bundle, err := json.Marshal(map[string]any{"version": 1, "itemTaskId": item.TaskID, "itemId": item.ID, "itemRevision": item.Revision, "workOrderMessage": orderRef, "history": map[string]any{"revision": revision, "revisions": []api.WorkItemRevision{revision}, "messages": []api.WorkItemMessageLink{{Message: order, ItemRevision: item.Revision, RevisionCoverage: "verified", Relationship: "primary"}}, "coverage": api.HistoryCoverage{Complete: true, ObservedCurrentRevision: item.Revision, LatestMaterialized: item.Revision, SnapshotCount: 1, ConversationLinks: "explicit_only"}}})
	if err != nil {
		t.Fatal(err)
	}
	worker, err := st.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "worker", Host: "fixture", Session: "worker", Runtime: "codex", WorkItem: &api.AgentWorkItemRequest{ItemTaskID: task.ID, ItemID: item.ID, ItemRevision: item.Revision, WorkOrderMessage: orderRef, ContextBundle: bundle}}, by)
	if err != nil {
		t.Fatal(err)
	}
	directive, err := st.PostMessage(ctx, task.ID, api.PostMessageRequest{Text: "Act on this exact directive", RequestID: "cli-delivery-message", WorkItems: []api.MessageWorkItem{{ItemTaskID: task.ID, ItemID: item.ID, ItemRevision: item.Revision, Relationship: "primary"}}, WorkOrderMessage: &orderRef}, by)
	if err != nil {
		t.Fatal(err)
	}
	instructionSHA := fmt.Sprintf("%x", sha256.Sum256([]byte(directive.Text)))
	created, err := st.CreateRequiredDelivery(ctx, task.ID, api.CreateRequiredDeliveryRequest{RequestID: "cli-delivery-create", MessageSeq: directive.Seq, Kind: api.DeliveryAssignment, AgentID: worker.ID, RunID: worker.RunID, ItemTaskID: item.TaskID, ItemID: item.ID, ItemRevision: item.Revision, WorkOrderMessage: orderRef, GoverningOrderMessage: orderRef, InstructionSHA256: instructionSHA, InstructionBytes: int64(len([]byte(directive.Text))), EnrollmentVersion: api.ReliableFollowThroughCapabilityVersion, ProducerAgentID: lead.ID, ProducerRunID: lead.RunID}, by)
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(server.New(st, func(*http.Request) (api.Caller, error) { return by, nil }))
	defer httpServer.Close()
	e := env{hub: httpServer.URL, task: task.ID, agent: worker.ID, runID: worker.RunID}
	out, err := captureCLIOutput(t, func() error { return cmdCurrentAssignment(e, nil) })
	if err != nil || !strings.Contains(out, created.Delivery.ID) || !strings.Contains(out, "phase unacknowledged") || !strings.Contains(out, "execution epoch 1") || !strings.Contains(out, directive.Text) {
		t.Fatalf("current assignment output=%q err=%v", out, err)
	}
	out, err = captureCLIOutput(t, func() error { return cmdDelivery(e, []string{"coverage"}) })
	if err != nil || !strings.Contains(out, "covered:") {
		t.Fatalf("delivery coverage output=%q err=%v", out, err)
	}
	out, err = captureCLIOutput(t, func() error {
		return cmdDelivery(e, []string{"ack", "--request-id", "cli-direct-ack", "--expected-epoch", "1", created.Delivery.ID})
	})
	if err != nil || !strings.Contains(out, "phase=acknowledged") {
		t.Fatalf("delivery ack output=%q err=%v", out, err)
	}
	current, err := st.CurrentAssignment(ctx, task.ID, worker.ID, worker.RunID)
	if err != nil || current.Phase != api.DeliveryAcknowledged {
		t.Fatalf("CLI mutation did not reach store: %+v %v", current, err)
	}
	staleEnv := e
	staleEnv.runID = api.NewID("run")
	if _, err = captureCLIOutput(t, func() error { return cmdCurrentAssignment(staleEnv, nil) }); err == nil {
		t.Fatal("stale CLI identity unexpectedly fetched current assignment")
	}
	if _, err = captureCLIOutput(t, func() error {
		return cmdDelivery(e, []string{"block", "--request-id", "cli-block", "--expected-epoch", "1", "--reason", "dependency", "--text", "fixture dependency", created.Delivery.ID})
	}); err != nil {
		t.Fatal(err)
	}
	current, err = st.CurrentAssignment(ctx, task.ID, worker.ID, worker.RunID)
	if err != nil || current.CurrentBlock == nil {
		t.Fatalf("CLI block did not reach store: %+v %v", current, err)
	}
	leadEnv := e
	leadEnv.agent, leadEnv.runID = lead.ID, lead.RunID
	if _, err = captureCLIOutput(t, func() error {
		return cmdDelivery(leadEnv, []string{"resolve", "--request-id", "cli-resolve", "--expected-epoch", "1", "--block-id", current.CurrentBlock.ID, "--text", "fixture resolved", created.Delivery.ID})
	}); err != nil {
		t.Fatal(err)
	}
	current, err = st.CurrentAssignment(ctx, task.ID, worker.ID, worker.RunID)
	if err != nil || current.CurrentBlock == nil || current.CurrentBlock.ResolutionID == "" {
		t.Fatalf("CLI resolution did not reach store: %+v %v", current, err)
	}
	if _, err = captureCLIOutput(t, func() error {
		return cmdDelivery(e, []string{"resume", "--request-id", "cli-resume", "--expected-epoch", "1", "--resolution-id", current.CurrentBlock.ResolutionID, created.Delivery.ID})
	}); err != nil {
		t.Fatal(err)
	}
	out, err = captureCLIOutput(t, func() error { return cmdCurrentAssignment(e, nil) })
	if err != nil || !strings.Contains(out, "phase acknowledged") || !strings.Contains(out, "execution epoch 2") {
		t.Fatalf("resumed current-assignment output=%q err=%v", out, err)
	}
	if _, err = captureCLIOutput(t, func() error {
		return cmdDelivery(e, []string{"progress", "--request-id", "cli-stale-progress", "--expected-epoch", "1", "--text", "obsolete", created.Delivery.ID})
	}); err == nil {
		t.Fatal("CLI accepted a fresh stale pre-resume progress action")
	}
	current, err = st.CurrentAssignment(ctx, task.ID, worker.ID, worker.RunID)
	if err != nil || current.ExecutionEpoch != 2 || current.Phase != api.DeliveryAcknowledged {
		t.Fatalf("stale CLI action mutated resumed state: %+v %v", current, err)
	}
	if _, err = captureCLIOutput(t, func() error {
		return cmdDelivery(e, []string{"progress", "--request-id", "cli-current-progress", "--expected-epoch", "2", "--text", "current", created.Delivery.ID})
	}); err != nil {
		t.Fatalf("current CLI progress failed: %v", err)
	}
	current, err = st.CurrentAssignment(ctx, task.ID, worker.ID, worker.RunID)
	if err != nil {
		t.Fatal(err)
	}
	incidentReq := api.DeliveryRecoveryIncidentRequest{RequestID: "cli-causal-incident", AgentID: lead.ID, RunID: lead.RunID,
		ExpectedGeneration: current.Generation, ExpectedEpoch: current.ExecutionEpoch, CauseStatus: api.DeliveryCauseEstablished,
		LastSubstantiveAction: "recorded current progress", LastSubstantiveAt: time.Now().UTC().Add(-time.Second),
		ExpectedNextAction: "continue exact action", StopReason: "synthetic unexpected pause",
		CausalEvidence: []string{"no later substantive event was recorded"}, ContributingConditions: []string{"synthetic turn ended"},
		Prevention: api.DeliveryPreventionAction{OwnerAgentID: lead.ID, OwnerRunID: lead.RunID, WorkOrderMessage: orderRef, VerificationCriterion: "exact action continues or escalates durably"}}
	incidentRaw, err := json.Marshal(incidentReq)
	if err != nil {
		t.Fatal(err)
	}
	incidentPath := filepath.Join(t.TempDir(), "incident.json")
	if err = os.WriteFile(incidentPath, incidentRaw, 0600); err != nil {
		t.Fatal(err)
	}
	out, err = captureCLIOutput(t, func() error { return cmdDelivery(leadEnv, []string{"incident", "--file", incidentPath, current.ID}) })
	if err != nil || !strings.Contains(out, "cause=established") || !strings.Contains(out, "incident dinc_") {
		t.Fatalf("delivery incident output=%q err=%v", out, err)
	}
}

func TestDeliveryCLIFailsClosedOnOldHub(t *testing.T) {
	actions := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/capabilities" {
			_ = json.NewEncoder(w).Encode(map[string]any{"schemaVersion": 1})
			return
		}
		actions++
		http.Error(w, "unsafe action reached old hub", 500)
	}))
	defer srv.Close()
	e := env{hub: srv.URL, task: "tsk_0000000000000001", agent: "agt_0000000000000001", runID: "run_0000000000000001"}
	err := cmdDelivery(e, []string{"ack", "--request-id", "old-hub-ack", "dly_0000000000000001"})
	if err == nil || !strings.Contains(err.Error(), "unsupported") || actions != 0 {
		t.Fatalf("old hub fallback was not fail-closed: err=%v actions=%d", err, actions)
	}
	err = cmdCurrentAssignment(e, nil)
	if err == nil || !strings.Contains(err.Error(), "unsupported") || actions != 0 {
		t.Fatalf("old hub current-assignment was not fail-closed: err=%v actions=%d", err, actions)
	}
}

func TestDeliveryCLIV2EnrollmentFailsClosedOnV1Hub(t *testing.T) {
	actions := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/capabilities" {
			caps := api.CurrentCapabilities()
			caps.ReliableDelivery.Versions = []int{api.ReliableDeliveryCapabilityVersion}
			_ = json.NewEncoder(w).Encode(caps)
			return
		}
		actions++
		http.Error(w, "unsafe v2 enrollment reached v1 hub", 500)
	}))
	defer srv.Close()
	req := api.CreateRequiredDeliveryRequest{RequestID: "v2-old-hub", EnrollmentVersion: api.ReliableFollowThroughCapabilityVersion}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "enrollment.json")
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	e := env{hub: srv.URL, task: "tsk_0000000000000001", agent: "agt_0000000000000001", runID: "run_0000000000000001"}
	err = cmdDelivery(e, []string{"create", "--file", path})
	if err == nil || !strings.Contains(err.Error(), "version 2") || actions != 0 {
		t.Fatalf("v2 enrollment was not fail-closed on v1 hub: err=%v actions=%d", err, actions)
	}
}

func TestDeliveryCLIV3EnrollmentFailsClosedOnV2Hub(t *testing.T) {
	actions := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/capabilities" {
			caps := api.CurrentCapabilities()
			caps.ReliableDelivery.Versions = []int{api.ReliableDeliveryCapabilityVersion, api.ReliableFollowThroughCapabilityVersion}
			_ = json.NewEncoder(w).Encode(caps)
			return
		}
		actions++
		http.Error(w, "unsafe v3 enrollment reached v2 hub", 500)
	}))
	defer srv.Close()
	req := api.CreateRequiredDeliveryRequest{RequestID: "v3-old-hub", EnrollmentVersion: api.ReliableMandatoryActionCapabilityVersion}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "mandatory-action.json")
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	e := env{hub: srv.URL, task: "tsk_0000000000000001", agent: "agt_0000000000000001", runID: "run_0000000000000001"}
	err = cmdDelivery(e, []string{"create", "--file", path})
	if err == nil || !strings.Contains(err.Error(), "version 3") || actions != 0 {
		t.Fatalf("v3 enrollment was not fail-closed on v2 hub: err=%v actions=%d", err, actions)
	}
	err = cmdCurrentAssignment(e, []string{"--item", "wi_0000000000000001", "--action-key", "shared-action"})
	if err == nil || !strings.Contains(err.Error(), "version 3") || actions != 0 {
		t.Fatalf("v3 action selection was not fail-closed on v2 hub: err=%v actions=%d", err, actions)
	}
}
