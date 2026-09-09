package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
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

func TestWorkItemHTTPHistoryKeyedUpdateAndReceipt(t *testing.T) {
	c := newClient(t)
	task := c.task("history-http")
	agent := c.agent(task, "history-agent")
	var item api.WorkItem
	if code := c.do("POST", "/v1/tasks/"+task.ID+"/work-items", api.CreateWorkItemRequest{Kind: "feature", Title: "Before", RequestID: "history-http-create"}, &item); code != 201 {
		t.Fatalf("create=%d", code)
	}
	title := "After"
	req := api.CreateWorkItemUpdate{ExpectedRevision: 1, Title: &title, AgentID: agent.ID, RunID: agent.RunID, RequestID: "history-http-update"}
	var first api.WorkItemUpdateResult
	path := "/v1/tasks/" + task.ID + "/work-items/" + item.ID + "/updates"
	if code := c.do("POST", path, req, &first); code != 201 {
		t.Fatalf("first=%d %+v", code, first)
	}
	var replay api.WorkItemUpdateResult
	if code := c.do("POST", path, req, &replay); code != 200 || !reflect.DeepEqual(replay, first) {
		t.Fatalf("replay=%d %+v", code, replay)
	}
	var receipt api.WorkItemUpdateResult
	if code := c.do("GET", path+"/receipts/"+req.RequestID+"?agentId="+agent.ID, nil, &receipt); code != 200 || !reflect.DeepEqual(receipt, first) {
		t.Fatalf("receipt=%d %+v", code, receipt)
	}
	var list api.WorkItemRevisionList
	if code := c.do("GET", "/v1/tasks/"+task.ID+"/work-items/"+item.ID+"/revisions?after=0&limit=1", nil, &list); code != 200 || len(list.Revisions) != 1 || list.NextAfter != 1 || !list.Coverage.Complete {
		t.Fatalf("revisions=%d %+v", code, list)
	}
	var exact api.WorkItemRevision
	if code := c.do("GET", "/v1/tasks/"+task.ID+"/work-items/"+item.ID+"/revisions/2", nil, &exact); code != 200 || exact.Title != title || exact.UpdatedRunID != agent.RunID {
		t.Fatalf("exact=%d %+v", code, exact)
	}
	if code := c.do("GET", "/v1/tasks/"+task.ID+"/work-items/"+item.ID+"/revisions?after=nope", nil, nil); code != 400 {
		t.Fatalf("bad cursor=%d", code)
	}
	if code := c.do("GET", "/v1/tasks/"+task.ID+"/work-items/"+item.ID+"/revisions?limit=65", nil, nil); code != 400 {
		t.Fatalf("bad limit=%d", code)
	}
	if code := c.do("DELETE", "/v1/tasks/"+task.ID, nil, nil); code != 200 {
		t.Fatalf("close=%d", code)
	}
	if code := c.do("POST", path, req, &replay); code != 200 || !reflect.DeepEqual(replay, first) {
		t.Fatalf("closed replay=%d %+v", code, replay)
	}
	if code := c.do("GET", "/v1/tasks/"+task.ID+"/work-items/"+item.ID+"/revisions/1", nil, &exact); code != 200 {
		t.Fatalf("closed history=%d", code)
	}
}

func TestWorkItemHTTPHistoryPagesStayUnderNormalClientLimit(t *testing.T) {
	c := newClient(t)
	task := c.task("bounded-history")
	item, err := c.st.CreateWorkItem(context.Background(), task.ID, api.CreateWorkItemRequest{Kind: "bug", Title: "Escaping", Description: strings.Repeat("<", api.MaxTextLen), RequestID: "bounded-create"}, c.who)
	if err != nil {
		t.Fatal(err)
	}
	for revision := int64(1); revision <= 104; revision++ {
		description := strings.Repeat("<", api.MaxTextLen-8) + string(rune('a'+revision%26))
		item, err = c.st.UpdateWorkItem(context.Background(), task.ID, item.ID, api.UpdateWorkItemRequest{Revision: revision, Description: &description}, c.who)
		if err != nil {
			t.Fatalf("update %d: %v", revision, err)
		}
	}
	url := c.srv.URL + "/v1/tasks/" + task.ID + "/work-items/" + item.ID + "/revisions?limit=64"
	res, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(res.Body)
	res.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != 200 || len(body) > api.MaxWorkItemHistoryBytes {
		t.Fatalf("status=%d bytes=%d", res.StatusCode, len(body))
	}
	var first api.WorkItemRevisionList
	if err := json.Unmarshal(body, &first); err != nil {
		t.Fatal(err)
	}
	if first.NextAfter == 0 || len(first.Revisions) >= 64 {
		t.Fatalf("byte bound did not cut page: count=%d next=%d bytes=%d", len(first.Revisions), first.NextAfter, len(body))
	}
	client, err := api.NewClient(c.srv.URL, 0)
	if err != nil {
		t.Fatal(err)
	}
	after, seen := int64(0), map[int64]bool{}
	for {
		page, err := client.ListWorkItemRevisions(context.Background(), task.ID, item.ID, after, 64)
		if err != nil {
			t.Fatal(err)
		}
		for _, revision := range page.Revisions {
			if seen[revision.Revision] {
				t.Fatalf("duplicate revision %d", revision.Revision)
			}
			seen[revision.Revision] = true
		}
		if page.NextAfter == 0 {
			break
		}
		if page.NextAfter <= after {
			t.Fatalf("cursor did not advance: %d <= %d", page.NextAfter, after)
		}
		after = page.NextAfter
	}
	if len(seen) != 105 {
		t.Fatalf("saw %d revisions", len(seen))
	}
}

func TestWorkItemHTTPMessagePagesStayUnderNormalClientLimit(t *testing.T) {
	c := newClient(t)
	task := c.task("bounded-history-messages")
	item, err := c.st.CreateWorkItem(context.Background(), task.ID, api.CreateWorkItemRequest{Kind: "feature", Title: "Messages", RequestID: "bounded-message-create"}, c.who)
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 105; index++ {
		_, err := c.st.PostMessage(context.Background(), task.ID, api.PostMessageRequest{Text: strings.Repeat("<", api.MaxTextLen-16) + fmt.Sprintf("-%03d", index), RequestID: fmt.Sprintf("bounded-message-%03d", index), WorkItems: []api.MessageWorkItem{{ItemTaskID: task.ID, ItemID: item.ID, ItemRevision: 1, Relationship: "primary"}}}, c.who)
		if err != nil {
			t.Fatalf("message %d: %v", index, err)
		}
	}
	url := c.srv.URL + "/v1/tasks/" + task.ID + "/work-items/" + item.ID + "/messages?limit=64"
	res, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(res.Body)
	res.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != 200 || len(body) > api.MaxWorkItemHistoryBytes {
		t.Fatalf("status=%d bytes=%d", res.StatusCode, len(body))
	}
	var first api.WorkItemMessageList
	if err := json.Unmarshal(body, &first); err != nil {
		t.Fatal(err)
	}
	if first.NextAfter == 0 || len(first.Links) >= 64 {
		t.Fatalf("byte bound did not cut messages: count=%d next=%d bytes=%d", len(first.Links), first.NextAfter, len(body))
	}
	client, err := api.NewClient(c.srv.URL, 0)
	if err != nil {
		t.Fatal(err)
	}
	after, seen := int64(0), map[int64]bool{}
	for {
		page, err := client.ListWorkItemMessages(context.Background(), task.ID, item.ID, 1, after, 64)
		if err != nil {
			t.Fatal(err)
		}
		for _, link := range page.Links {
			if seen[link.Message.Seq] {
				t.Fatalf("duplicate message %d", link.Message.Seq)
			}
			seen[link.Message.Seq] = true
		}
		if page.NextAfter == 0 {
			break
		}
		if page.NextAfter <= after {
			t.Fatalf("message cursor did not advance: %d", page.NextAfter)
		}
		after = page.NextAfter
	}
	if len(seen) != 105 {
		t.Fatalf("saw %d linked messages", len(seen))
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
