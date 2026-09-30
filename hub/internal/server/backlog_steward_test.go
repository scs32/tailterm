package server

import (
	"strings"
	"testing"

	"github.com/scs32/tailterm/hub/internal/api"
)

// Backlog steward route tests (wi_5b4b94dbc9a11e8b, order #14942) use an
// isolated hub per test and synthetic agents only.

func (c *client) steward(task api.Task, name string) api.Agent {
	c.t.Helper()
	id := api.NewID("agt")
	var a api.Agent
	if code := c.do("POST", "/v1/tasks/"+task.ID+"/agents", api.AddAgentRequest{Role: api.AgentRoleBacklogSteward, AgentID: id, Name: name, Host: "devbox",
		Session: "tt-steward-" + strings.TrimPrefix(id, "agt_"), Runtime: "claude", Cwd: "/tmp"}, &a); code != 201 {
		c.t.Fatalf("add steward: %d", code)
	}
	if code := c.do("POST", "/v1/tasks/"+task.ID+"/events", api.PostEventRequest{AgentID: a.ID, RunID: a.RunID, Kind: api.EventStarted}, nil); code != 201 && code != 200 {
		c.t.Fatalf("steward started: %d", code)
	}
	return a
}

func TestTriageStewardRoute(t *testing.T) {
	c := newClient(t)
	task := c.task("steward-triage")
	steward := c.steward(task, "backlog-steward")
	worker := c.agent(task, "worker")
	path := "/v1/tasks/" + task.ID + "/work-items/triage?"
	var got api.WorkItemTriage
	if code := c.do("GET", path+"agentId="+steward.ID+"&runId="+steward.RunID, nil, &got); code != 200 || got.HeldForTriage == nil {
		t.Fatalf("steward triage = %d %+v", code, got)
	}
	for name, query := range map[string]string{
		"stale run":      "agentId=" + steward.ID + "&runId=" + api.NewID("run"),
		"ordinary agent": "agentId=" + worker.ID + "&runId=" + worker.RunID,
	} {
		if code := c.do("GET", path+query, nil, nil); code != 409 {
			t.Fatalf("%s triage = %d", name, code)
		}
	}
	// A second steward is refused with the named reason in the body.
	var refusal api.ErrorResponse
	id := api.NewID("agt")
	if code := c.do("POST", "/v1/tasks/"+task.ID+"/agents", api.AddAgentRequest{Role: api.AgentRoleBacklogSteward, AgentID: id, Name: "backlog-steward-2", Host: "devbox",
		Session: "tt-steward-" + strings.TrimPrefix(id, "agt_"), Runtime: "claude", Cwd: "/tmp"}, &refusal); code != 409 || refusal.Code != api.StewardRefusedActive {
		t.Fatalf("second steward = %d %+v", code, refusal)
	}
	var status api.BacklogStewardStatus
	if code := c.do("GET", "/v1/tasks/"+task.ID+"/backlog-steward", nil, &status); code != 200 || status.Steward == nil || status.Steward.ID != steward.ID {
		t.Fatalf("steward status = %d %+v", code, status)
	}
}
