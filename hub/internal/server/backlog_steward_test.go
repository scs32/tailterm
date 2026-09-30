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

func TestBacklogSummaryHTTPLimit(t *testing.T) {
	c := newClient(t)
	task := c.task("steward-summary")
	steward := c.steward(task, "backlog-steward")
	path := "/v1/tasks/" + task.ID + "/backlog-summary"
	// A 64 KiB body of a character JSON escapes as \u0001 (six bytes each)
	// fits the request limit; the summary limit is on the decoded body.
	escaped := strings.Repeat("\x01", api.MaxBacklogSummaryLen)
	var saved api.BacklogSummary
	if code := c.do("POST", path, api.SaveBacklogSummaryRequest{ExpectedRevision: 0, Body: escaped, RequestID: "escaped", AgentID: steward.ID, RunID: steward.RunID}, &saved); code != 201 || saved.Revision != 1 || saved.Bytes != api.MaxBacklogSummaryLen {
		t.Fatalf("64 KiB escaped summary = %d %+v", code, saved)
	}
	plain := strings.Repeat("s", api.MaxBacklogSummaryLen)
	if code := c.do("POST", path, api.SaveBacklogSummaryRequest{ExpectedRevision: 1, Body: plain, RequestID: "plain", AgentID: steward.ID, RunID: steward.RunID}, &saved); code != 201 || saved.Revision != 2 {
		t.Fatalf("64 KiB summary = %d %+v", code, saved)
	}
	if code := c.do("POST", path, api.SaveBacklogSummaryRequest{ExpectedRevision: 2, Body: plain + "s", RequestID: "over", AgentID: steward.ID, RunID: steward.RunID}, nil); code != 400 {
		t.Fatalf("64 KiB + 1 summary = %d", code)
	}
	if code := c.do("POST", path, api.SaveBacklogSummaryRequest{ExpectedRevision: 1, Body: "stale", RequestID: "stale", AgentID: steward.ID, RunID: steward.RunID}, nil); code != 409 {
		t.Fatalf("stale revision = %d", code)
	}
	var got api.BacklogSummary
	if code := c.do("GET", path+"?revision=1", nil, &got); code != 200 || got.Body != escaped {
		t.Fatalf("read revision 1 = %d (%d bytes)", code, len(got.Body))
	}
	var list struct {
		Revisions []api.BacklogSummary `json:"revisions"`
	}
	if code := c.do("GET", path+"/revisions", nil, &list); code != 200 || len(list.Revisions) != 2 {
		t.Fatalf("revisions = %d %+v", code, list)
	}
}

func TestStewardRefusedWorkItemWritesOverHTTP(t *testing.T) {
	c := newClient(t)
	task := c.task("steward-refused")
	steward := c.steward(task, "backlog-steward")
	var refusal api.ErrorResponse
	if code := c.do("POST", "/v1/tasks/"+task.ID+"/work-items", api.CreateWorkItemRequest{Kind: "bug", Title: "Drafted by the steward", AgentID: steward.ID, RequestID: "steward-create"}, &refusal); code != 409 || refusal.Code != api.StewardRefusedWrite {
		t.Fatalf("steward create = %d %+v", code, refusal)
	}
	var item api.WorkItem
	if code := c.do("POST", "/v1/tasks/"+task.ID+"/work-items", api.CreateWorkItemRequest{Kind: "bug", Title: "Filed by the owner", RequestID: "owner-create"}, &item); code != 201 {
		t.Fatalf("owner create = %d", code)
	}
	title := "Retitled by the steward"
	refusal = api.ErrorResponse{}
	if code := c.do("POST", "/v1/tasks/"+task.ID+"/work-items/"+item.ID+"/updates", api.CreateWorkItemUpdate{ExpectedRevision: item.Revision, Title: &title, AgentID: steward.ID, RunID: steward.RunID, RequestID: "steward-update"}, &refusal); code != 409 || refusal.Code != api.StewardRefusedWrite {
		t.Fatalf("steward update = %d %+v", code, refusal)
	}
	env := api.Envelope{Kind: api.EnvelopeKindNotice, Subject: "Backlog summary revision one is saved", Body: api.EnvelopeBody{Text: "Themes grouped."}}
	if code := c.do("POST", "/v1/tasks/"+task.ID+"/messages", api.PostMessageRequest{Envelope: &env, Text: api.RenderText(env), AgentID: steward.ID, RunID: steward.RunID}, nil); code != 201 {
		t.Fatalf("steward notice = %d", code)
	}
}
