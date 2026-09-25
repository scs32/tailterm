package server

import (
	"testing"

	"github.com/scs32/tailterm/hub/internal/api"
)

func TestWorkOrderScopeHTTPRejectsItemFieldsAndUnconfirmedQueue(t *testing.T) {
	c := newClient(t)
	task := c.task("scope-http")
	var handler api.Agent
	if code := c.do("POST", "/v1/tasks/"+task.ID+"/agents", api.AddAgentRequest{AgentID: api.NewID("agt"), Name: "handler", Role: api.AgentRoleDatabaseHandler, Host: "fixture", Session: "handler", Runtime: "codex", Cwd: "/tmp"}, &handler); code != 201 {
		t.Fatalf("handler = %d", code)
	}
	var item api.WorkItem
	if code := c.do("POST", "/v1/tasks/"+task.ID+"/work-items", api.CreateWorkItemRequest{Kind: "bug", Title: "filed scope", Description: "owner acceptance and files", RequestID: "scope-http-item"}, &item); code != 201 {
		t.Fatalf("item = %d", code)
	}
	var order api.Message
	if code := c.do("POST", "/v1/tasks/"+task.ID+"/messages", api.PostMessageRequest{Text: "bounded order", RequestID: "scope-http-order", WorkItems: []api.MessageWorkItem{{ItemTaskID: task.ID, ItemID: item.ID, ItemRevision: item.Revision, Relationship: "primary"}}}, &order); code != 201 {
		t.Fatalf("order = %d", code)
	}
	base := "/v1/tasks/" + task.ID + "/work-items/" + item.ID
	if code := c.do("POST", base+"/order-bookkeeping", map[string]any{"requestId": "early", "title": "changed"}, nil); code != 400 {
		t.Fatalf("unknown item field in bookkeeping = %d", code)
	}
	if code := c.do("POST", base+"/order-scope/confirm", map[string]any{"requestId": "bad", "complete": true, "description": "changed"}, nil); code != 400 {
		t.Fatalf("unknown item field in confirmation = %d", code)
	}
	if code := c.do("POST", "/v1/tasks/"+task.ID+"/team-queue/actions", api.TeamQueueRequest{RequestID: "early-queue", Operation: "add", ItemID: item.ID, OrderMessageSeq: order.Seq, Host: "fixture", Cwd: "/tmp"}, nil); code != 409 {
		t.Fatalf("unconfirmed queue = %d", code)
	}
	var saved api.WorkOrderScopeConfirmation
	if code := c.do("POST", base+"/order-scope/confirm", api.ConfirmWorkOrderScopeRequest{RequestID: "intake", AgentID: handler.ID, RunID: handler.RunID, ExpectedRevision: item.Revision, ScopeRevision: item.ScopeRevision, OrderMessageSeq: order.Seq, Complete: true}, &saved); code != 201 || saved.ItemID != item.ID {
		t.Fatalf("intake = %d %+v", code, saved)
	}
	if code := c.do("POST", "/v1/tasks/"+task.ID+"/team-queue/actions", api.TeamQueueRequest{RequestID: "admitted-queue", Operation: "add", ItemID: item.ID, OrderMessageSeq: order.Seq, Host: "fixture", Cwd: "/tmp"}, nil); code != 200 {
		t.Fatalf("confirmed queue = %d", code)
	}
}
