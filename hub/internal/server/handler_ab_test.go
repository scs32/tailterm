package server

import (
	"strings"
	"testing"

	"github.com/scs32/tailterm/hub/internal/api"
)

// Handler arm routes (wi_fc1396aef8a72a06, order #14869), synthetic data only.

func TestHandlerArmPolicyRoutes(t *testing.T) {
	c := newClient(t)
	task := c.task("handler-arms")
	digest := strings.Repeat("a", 64)
	var handler api.Agent
	if code := c.do("POST", "/v1/tasks/"+task.ID+"/agents", api.AddAgentRequest{Name: "handler-s", Role: api.AgentRoleDatabaseHandler, AgentID: api.NewID("agt"), Host: "devbox", Session: "tt-handler-s",
		Runtime: "claude", TemplateDigest: digest, HandlerModel: "claude-sonnet-5-5", HandlerReasoning: "high"}, &handler); code != 201 {
		t.Fatalf("add handler %d", code)
	}
	if code := c.do("POST", "/v1/tasks/"+task.ID+"/agents", api.AddAgentRequest{Name: "builder", Host: "devbox", Session: "tt-builder", Runtime: "claude", HandlerModel: "claude-sonnet-5-5"}, nil); code != 400 {
		t.Fatalf("handler model on a builder = %d", code)
	}
	path := "/v1/tasks/" + task.ID + "/handler-ab/policy"
	var view api.HandlerArmPolicyView
	if code := c.do("GET", path, nil, &view); code != 200 || view.Policy.Revision != 0 || len(view.Handlers) != 1 || view.Handlers[0].Model != "claude-sonnet-5-5" || view.Handlers[0].TemplateDigest != digest {
		t.Fatalf("get = %d %+v", code, view)
	}
	arms := []api.HandlerArm{{ID: "S", Runtime: "claude", Model: "claude-sonnet-5-5", Reasoning: "high", Weight: 1}, {ID: "O", Runtime: "codex", Model: "gpt-6.1-sol", Reasoning: "high", Weight: 1}}
	req := api.HandlerArmPolicyRequest{RequestID: "arms-http-1", Enabled: true, Seed: "K", TemplateDigest: digest, Arms: arms}
	var failure api.ErrorResponse
	agent := req
	agent.ActorAgentID = handler.ID
	if code := c.do("PUT", path, agent, &failure); code != 409 || failure.Code != api.HandlerArmRefusedAgentCaller {
		t.Fatalf("agent caller = %d %+v", code, failure)
	}
	invalid := req
	invalid.Arms = arms[:1]
	failure = api.ErrorResponse{}
	if code := c.do("PUT", path, invalid, &failure); code != 400 || !strings.Contains(failure.Error, "2 to 8 arms") {
		t.Fatalf("invalid = %d %+v", code, failure)
	}
	mismatch := req
	mismatch.TemplateDigest = strings.Repeat("b", 64)
	failure = api.ErrorResponse{}
	if code := c.do("PUT", path, mismatch, &failure); code != 409 || failure.Code != api.HandlerArmRefusedTemplate || !strings.Contains(failure.Error, handler.ID) {
		t.Fatalf("template mismatch = %d %+v", code, failure)
	}
	var saved api.HandlerArmPolicy
	if code := c.do("PUT", path, req, &saved); code != 200 || saved.Revision != 1 || !saved.Enabled {
		t.Fatalf("set = %d %+v", code, saved)
	}
	var replay api.HandlerArmPolicy
	if code := c.do("PUT", path, req, &replay); code != 200 || replay.Revision != 1 {
		t.Fatalf("replay = %d %+v", code, replay)
	}
	stale := req
	stale.RequestID = "arms-http-2"
	failure = api.ErrorResponse{}
	if code := c.do("PUT", path, stale, &failure); code != 409 || failure.Code != api.HandlerArmRefusedStalePolicy {
		t.Fatalf("stale = %d %+v", code, failure)
	}
	var report api.HandlerABReport
	if code := c.do("GET", "/v1/tasks/"+task.ID+"/handler-ab/report", nil, &report); code != 200 || report.Policy.Revision != 1 || len(report.Items) != 0 || len(report.Arms) != 2 {
		t.Fatalf("report = %d %+v", code, report)
	}
	if code := c.do("GET", "/v1/tasks/tsk_missing/handler-ab/report", nil, nil); code != 400 && code != 404 {
		t.Fatalf("missing project report = %d", code)
	}
}
