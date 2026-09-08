package server

import (
	"testing"

	"github.com/scs32/tailterm/hub/internal/api"
)

func TestWorkItemHTTPCRUDListsAndDispatch(t *testing.T) {
	c := newClient(t)
	task := c.task("work-items")
	lead := c.agent(task, "lead")
	orchestrator := "lead"
	if code := c.do("PATCH", "/v1/tasks/"+task.ID, api.UpdateTaskRequest{Orchestrator: &orchestrator}, nil); code != 200 {
		t.Fatalf("set orchestrator = %d", code)
	}
	request := api.CreateWorkItemRequest{Kind: "bug", Title: "Message receipt is lost", Description: "Retry should return the same record.", RequestID: "http-create-1"}
	var item api.WorkItem
	if code := c.do("POST", "/v1/tasks/"+task.ID+"/work-items", request, &item); code != 201 {
		t.Fatalf("create = %d", code)
	}
	if item.Status != "open" || item.Priority != "normal" || item.Revision != 1 {
		t.Fatalf("create defaults: %+v", item)
	}
	var replay api.WorkItem
	if code := c.do("POST", "/v1/tasks/"+task.ID+"/work-items", request, &replay); code != 201 || replay.ID != item.ID {
		t.Fatalf("create replay = %d %+v", code, replay)
	}
	request.Title = "Changed payload"
	if code := c.do("POST", "/v1/tasks/"+task.ID+"/work-items", request, nil); code != 409 {
		t.Fatalf("changed replay = %d", code)
	}
	var list api.WorkItemList
	if code := c.do("GET", "/v1/work-items?taskId="+task.ID+"&kind=bug&status=open&limit=10", nil, &list); code != 200 || len(list.Items) != 1 || list.Items[0].ID != item.ID {
		t.Fatalf("list = %d %+v", code, list)
	}
	var nested api.WorkItemList
	if code := c.do("GET", "/v1/tasks/"+task.ID+"/work-items?kind=bug", nil, &nested); code != 200 || len(nested.Items) != 1 {
		t.Fatalf("nested list = %d %+v", code, nested)
	}
	status := "in_progress"
	var updated api.WorkItem
	if code := c.do("PATCH", "/v1/tasks/"+task.ID+"/work-items/"+item.ID, api.UpdateWorkItemRequest{Revision: 1, Status: &status}, &updated); code != 200 || updated.Revision != 2 {
		t.Fatalf("update = %d %+v", code, updated)
	}
	if code := c.do("PATCH", "/v1/tasks/"+task.ID+"/work-items/"+item.ID, api.UpdateWorkItemRequest{Revision: 1, Status: &status}, nil); code != 409 {
		t.Fatalf("stale update = %d", code)
	}
	var dispatched api.WorkItemDispatchResult
	if code := c.do("POST", "/v1/tasks/"+task.ID+"/work-items/"+item.ID+"/dispatch", api.DispatchWorkItemRequest{Revision: 2, RequestID: "http-dispatch-1"}, &dispatched); code != 201 {
		t.Fatalf("dispatch = %d", code)
	}
	if dispatched.Dispatch.TargetAgentID != lead.ID || dispatched.Dispatch.MessageSeq == 0 {
		t.Fatalf("dispatch receipt: %+v", dispatched)
	}
	var got api.WorkItem
	if code := c.do("GET", "/v1/tasks/"+task.ID+"/work-items/"+item.ID, nil, &got); code != 200 || got.LastDispatch == nil || got.LastDispatch.ID != dispatched.Dispatch.ID {
		t.Fatalf("get after dispatch = %d %+v", code, got)
	}
	if code := c.do("GET", "/v1/tasks/"+task.ID+"/work-items/wi_bad", nil, nil); code != 404 {
		t.Fatalf("invalid item id = %d", code)
	}
}

func TestWorkItemHTTPClosedProjectIsReadOnly(t *testing.T) {
	c := newClient(t)
	task := c.task("closed-items")
	var item api.WorkItem
	if code := c.do("POST", "/v1/tasks/"+task.ID+"/work-items", api.CreateWorkItemRequest{Kind: "feature", Title: "Read history", RequestID: "closed-create"}, &item); code != 201 {
		t.Fatalf("create = %d", code)
	}
	if code := c.do("DELETE", "/v1/tasks/"+task.ID, nil, nil); code != 200 {
		t.Fatalf("close = %d", code)
	}
	status := "done"
	if code := c.do("PATCH", "/v1/tasks/"+task.ID+"/work-items/"+item.ID, api.UpdateWorkItemRequest{Revision: 1, Status: &status}, nil); code != 409 {
		t.Fatalf("closed update = %d", code)
	}
	var got api.WorkItem
	if code := c.do("GET", "/v1/tasks/"+task.ID+"/work-items/"+item.ID, nil, &got); code != 200 || got.ID != item.ID {
		t.Fatalf("closed get = %d %+v", code, got)
	}
}
