package server

import (
	"github.com/scs32/tailterm/hub/internal/api"
	"net/http"
	"strings"
	"testing"
)

func TestReviewConvergenceHTTPRefusalRollsBackAndShowsUnknown(t *testing.T) {
	c := newClient(t)
	task := c.task("Review HTTP fixture")
	reviewer := c.agent(task, "reviewer")
	var item api.WorkItem
	if code := c.do("POST", "/v1/tasks/"+task.ID+"/work-items", api.CreateWorkItemRequest{Kind: "bug", Title: "HTTP cap fixture", RequestID: "item"}, &item); code != 201 {
		t.Fatal(code)
	}
	links := []api.MessageWorkItem{{ItemTaskID: task.ID, ItemID: item.ID, ItemRevision: 1, Relationship: "primary"}}
	path := "/v1/tasks/" + task.ID + "/messages"
	env := api.Envelope{Kind: "assign", Subject: "Implement exact HTTP fixture criteria", Body: api.EnvelopeBody{Objective: "Fixture", Owns: []string{"fixture"}, Acceptance: map[string]string{"a1": "passes"}}}
	var message api.Message
	if code := c.do("POST", path, api.PostMessageRequest{Envelope: &env, WorkItems: links, RequestID: "assign"}, &message); code != 201 {
		t.Fatal(code)
	}
	candidate := strings.Repeat("a", 40)
	for _, key := range []string{"one", "two"} {
		env := api.Envelope{Kind: "review", Subject: "Review exact HTTP fixture candidate", Body: api.EnvelopeBody{Candidate: candidate, Scope: "Fixture", Acceptance: map[string]string{"a1": "passes"}}}
		if code := c.do("POST", path, api.PostMessageRequest{Envelope: &env, WorkItems: links, To: reviewer.ID, RequestID: key}, &message); code != 201 {
			t.Fatal(code)
		}
		env = api.Envelope{Kind: "result", Subject: "HTTP review fixture passes checks", Review: &api.ReviewMetadata{Mode: "general", Candidate: candidate}, Body: api.EnvelopeBody{Outcome: "done", Status: map[string]string{"a1": "pass"}}, Evidence: map[string]api.Evidence{"e1": {Type: "command", Value: "fixture -> pass"}}}
		if code := c.do("POST", path, api.PostMessageRequest{Envelope: &env, WorkItems: links, AgentID: reviewer.ID, RunID: reviewer.RunID, ReplyTo: message.Seq, RequestID: key + "-result"}, &message); code != 201 {
			t.Fatal(code)
		}
	}
	env = api.Envelope{Kind: "review", Subject: "Third exact HTTP fixture review", Body: api.EnvelopeBody{Candidate: candidate, Scope: "Fixture", Acceptance: map[string]string{"a1": "passes"}}}
	var rejected api.ErrorResponse
	if code := c.do("POST", path, api.PostMessageRequest{Envelope: &env, WorkItems: links, To: reviewer.ID, RequestID: "three"}, &rejected); code != http.StatusConflict || !strings.Contains(rejected.Error, "third general review") {
		t.Fatal(code, rejected)
	}
	var summary []api.ReviewConvergence
	if code := c.do("GET", "/v1/tasks/"+task.ID+"/review-convergence", nil, &summary); code != 200 || len(summary) != 1 || len(summary[0].Rounds) != 2 {
		t.Fatal(code, summary)
	}
	done := "done"
	if code := c.do("PATCH", "/v1/tasks/"+task.ID+"/work-items/"+item.ID, api.UpdateWorkItemRequest{Revision: 1, Status: &done}, &rejected); code != http.StatusConflict {
		t.Fatal("PATCH bypass", code)
	}
	if code := c.do("POST", "/v1/tasks/"+task.ID+"/work-items/"+item.ID+"/updates", api.CreateWorkItemUpdate{ExpectedRevision: 1, Status: &done, RequestID: "done"}, &rejected); code != http.StatusConflict {
		t.Fatal("keyed bypass", code)
	}
}
