package server

import (
	"net/http"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

func TestOwnerHelperRegisterAndDelegateHTTP(t *testing.T) {
	h := newTokenHub(t)
	task, builder, _ := h.project()
	base := "/v1/tasks/" + task.ID
	reg := api.RegisterOwnerHelperRequest{Host: "owner-mac", Session: "owner", Runtime: "claude", Cwd: "/work/tailterm", RequestID: "ohreg-1"}
	var first api.OwnerActionResult
	if code, _ := h.do(ownerToken, "POST", base+"/owner-helper", reg, &first); code != http.StatusCreated || first.Agent == nil || first.Agent.Role != api.AgentRoleOwnerHelper || first.Registration == nil || first.Registration.Mode != api.OwnerHelperCreated {
		t.Fatalf("register %d %+v", code, first)
	}
	var replay api.OwnerActionResult
	if code, _ := h.do(ownerToken, "POST", base+"/owner-helper", reg, &replay); code != http.StatusOK || !replay.Replay || replay.Agent.RunID != first.Agent.RunID {
		t.Fatalf("replay %d %+v", code, replay)
	}
	changed := reg
	changed.Session = "elsewhere"
	if code, _ := h.do(ownerToken, "POST", base+"/owner-helper", changed, nil); code != http.StatusConflict {
		t.Fatalf("reused request id %d", code)
	}
	for name, bad := range map[string]api.RegisterOwnerHelperRequest{
		"runtime": {Host: "h", Session: "owner", Runtime: "unsupported", RequestID: "bad-runtime"},
		"session": {Host: "h", Session: "no spaces", Runtime: "claude", RequestID: "bad-session"},
	} {
		if code, _ := h.do(ownerToken, "POST", base+"/owner-helper", bad, nil); code != http.StatusBadRequest {
			t.Fatalf("%s: %d", name, code)
		}
	}
	// The Discord bridge credential may not register a helper.
	bridged := reg
	bridged.RequestID = "ohreg-bridge"
	if code, _ := h.do(bridgeToken, "POST", base+"/owner-helper", bridged, nil); code != http.StatusForbidden && code != http.StatusNotFound {
		t.Fatalf("bridge register %d", code)
	}
	again := reg
	again.RequestID = "ohreg-2"
	var second api.OwnerActionResult
	if code, _ := h.do(ownerToken, "POST", base+"/owner-helper", again, &second); code != http.StatusCreated || second.Agent.ID != first.Agent.ID || second.Registration.PreviousRunID != first.Agent.RunID {
		t.Fatalf("replace %d %+v", code, second)
	}
	helper := *second.Agent

	// A window for the helper by name routes an owner request; the helper answers with a rationale.
	open := api.OpenDelegationWindowRequest{Delegate: api.DefaultOwnerHelperName, EndsAt: time.Now().Add(3 * time.Hour), Scope: api.DelegationScopeDecisions, Source: &api.DelegationSource{Kind: "tt"}, RequestID: "open-helper"}
	var opened api.OwnerActionResult
	if code, _ := h.do(ownerToken, "POST", base+"/delegation-windows", open, &opened); code != http.StatusCreated || opened.Window.DelegateAgentID != helper.ID {
		t.Fatalf("open %d %+v", code, opened)
	}
	var item api.WorkItem
	if code, _ := h.do(ownerToken, "POST", base+"/work-items", api.CreateWorkItemRequest{Kind: "feature", Title: "Owner helper HTTP fixture", RequestID: "item"}, &item); code != 201 {
		t.Fatal(code)
	}
	var m api.Message
	req := api.PostMessageRequest{AgentID: builder.ID, RunID: builder.RunID, To: "owner", RequestID: "ask", Envelope: &api.Envelope{Kind: "request", To: "owner", Subject: "Choose the helper fixture rollout order", Body: api.EnvelopeBody{Ask: "Which?"}},
		WorkItems: []api.MessageWorkItem{{ItemTaskID: task.ID, ItemID: item.ID, ItemRevision: item.Revision, Relationship: "primary"}}}
	if code, _ := h.do(ownerToken, "POST", base+"/messages", req, &m); code != 201 {
		t.Fatalf("ask %d", code)
	}
	var list api.ObligationList
	h.do(ownerToken, "GET", base+"/obligations?owner=1&open=1", nil, &list)
	var o api.Obligation
	for _, ob := range list.Obligations {
		if ob.MessageSeq == m.Seq {
			o = ob
		}
	}
	answer := func(agent api.Agent, key, rationale string) int {
		code, _ := h.do(ownerToken, "POST", base+"/obligations/"+o.ID+"/answer", api.ObligationAnswerRequest{AgentID: agent.ID, RunID: agent.RunID, Text: "Staged first", Rationale: rationale, RequestID: key}, nil)
		return code
	}
	if code := answer(helper, "blank", ""); code != http.StatusBadRequest {
		t.Fatalf("no rationale %d", code)
	}
	if code := answer(*first.Agent, "old-run", "because"); code != http.StatusForbidden {
		t.Fatalf("old run %d", code)
	}
	if code := answer(helper, "ok", "Staged limits the blast radius."); code != http.StatusCreated {
		t.Fatalf("helper answer %d", code)
	}
}

func TestOwnerHelperRegisterRefusedWhilePaused(t *testing.T) {
	c := newClient(t)
	task := c.task("synthetic helper pause")
	lead := c.agent(task, "lead")
	base := "/v1/tasks/" + task.ID
	register := func(key string) (int, api.OwnerActionResult) {
		var out api.OwnerActionResult
		code := c.do("POST", base+"/owner-helper", api.RegisterOwnerHelperRequest{Host: "owner-mac", Session: "owner", Runtime: "claude", RequestID: key}, &out)
		return code, out
	}
	code, helper := register("before")
	if code != http.StatusCreated {
		t.Fatalf("register %d", code)
	}
	// The helper is a pause target like any agent; it owes no host cleanup.
	request := api.PauseProjectRequest{Version: 1, RequestID: "pause", Targets: []api.ProjectPauseTargetRequest{
		{AgentID: lead.ID, RunID: lead.RunID, ServiceDisposition: api.PauseServiceNone},
		{AgentID: helper.Agent.ID, RunID: helper.Agent.RunID, ServiceDisposition: api.PauseServiceNone}}}
	var paused api.ProjectPauseStatus
	if code := c.do("POST", base+"/pause", request, &paused); code != http.StatusOK || paused.State != api.ProjectPauseCleanupPending || paused.CleanupPending != 1 {
		t.Fatalf("pause %d %+v", code, paused)
	}
	if code, _ := register("cleanup-pending"); code != http.StatusConflict {
		t.Fatalf("cleanup_pending register %d", code)
	}
	if code := c.do("POST", base+"/agents/"+lead.ID+"/cleanup", api.CleanupRequest{RunID: lead.RunID}, nil); code != http.StatusOK {
		t.Fatalf("cleanup %d", code)
	}
	if code := c.do("GET", base+"/pause", nil, &paused); code != http.StatusOK || paused.State != api.ProjectPausePaused {
		t.Fatalf("paused %d %+v", code, paused)
	}
	if code, _ := register("paused"); code != http.StatusConflict {
		t.Fatalf("paused register %d", code)
	}
	planned := api.ProjectResumeOrchestrator{AgentID: api.NewID("agt"), RunID: api.NewID("run"), Name: "fresh-lead"}
	var resumed api.ProjectPauseStatus
	if code := c.do("POST", base+"/resume", api.ResumeProjectRequest{Version: 1, RequestID: "resume", ExpectedPauseGeneration: 1, ExpectedLifecycleGeneration: 1, RetainedHandoffDigest: paused.RetainedHandoffDigest, SelectedTeamID: "synthetic-team", Orchestrator: planned}, &resumed); code != http.StatusOK || resumed.State != api.ProjectPauseResuming {
		t.Fatalf("resume %d %+v", code, resumed)
	}
	if code, _ := register("resuming"); code != http.StatusConflict {
		t.Fatalf("resuming register %d", code)
	}
	admission := api.AddAgentRequest{AgentID: planned.AgentID, ExpectedRunID: planned.RunID, ResumeReceiptID: resumed.Receipt.ID, Name: planned.Name, Host: "fixture", Session: "fresh", ExpectedLifecycleGeneration: 2}
	var fresh api.Agent
	if code := c.do("POST", base+"/agents", admission, &fresh); code != http.StatusCreated {
		t.Fatalf("admission %d", code)
	}
	confirm := api.ConfirmProjectResumeRequest{Version: 1, RequestID: "confirm", ExpectedLifecycleGeneration: 2, ResumeReceiptID: resumed.Receipt.ID, AgentID: fresh.ID, RunID: fresh.RunID}
	if code := c.do("POST", base+"/resume/confirm", confirm, &resumed); code != http.StatusOK || resumed.State != api.ProjectPauseActive {
		t.Fatalf("confirm %d %+v", code, resumed)
	}
	// After resume the owner registers again: pause closed the old helper, so it is a fresh agent.
	code, after := register("after-resume")
	if code != http.StatusCreated || after.Agent.ID == helper.Agent.ID || after.Registration.Mode != api.OwnerHelperCreated {
		t.Fatalf("after resume %d %+v", code, after)
	}
}
