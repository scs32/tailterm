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

func historyBoundaryPadding(t *testing.T, build func(string) any) string {
	t.Helper()
	body, err := json.Marshal(build(""))
	if err != nil {
		t.Fatal(err)
	}
	one, err := json.Marshal(build("x"))
	if err != nil {
		t.Fatal(err)
	}
	fieldOverhead := len(one) - len(body) - 1
	padding := api.MaxWorkItemHistoryBytes - len(body) - fieldOverhead - 1
	if padding < 1 {
		t.Fatalf("boundary fixture base is too large: %d", len(body))
	}
	value := strings.Repeat("x", padding)
	body, err = json.Marshal(build(value))
	if err != nil {
		t.Fatal(err)
	}
	if len(body)+1 != api.MaxWorkItemHistoryBytes {
		t.Fatalf("boundary fixture bytes=%d", len(body)+1)
	}
	return value
}

func TestWorkItemHistoryBoundsMeasureFinalCursor(t *testing.T) {
	revisionBuild := func(padding string) any {
		return api.WorkItemRevisionList{Revisions: []api.WorkItemRevision{
			{Revision: 1},
			{Revision: 922337203685477580, Description: padding},
		}}
	}
	revisionPadding := historyBoundaryPadding(t, revisionBuild)
	revisionCandidate := revisionBuild(revisionPadding).(api.WorkItemRevisionList)
	revisionCandidate.NextAfter = revisionCandidate.Revisions[1].Revision
	if fitsHistoryBody(revisionCandidate) {
		t.Fatal("revision fixture must cross the bound only when nextAfter is encoded")
	}
	revisionCandidate.Revisions = append(revisionCandidate.Revisions, api.WorkItemRevision{Revision: 922337203685477581})
	revisions, ok := boundRevisionList(revisionCandidate, 2)
	if !ok || len(revisions.Revisions) != 1 || revisions.NextAfter != 1 || !fitsHistoryBody(revisions) {
		t.Fatalf("revision bound=%+v ok=%v bytesFit=%v", revisions, ok, fitsHistoryBody(revisions))
	}

	gapBuild := func(padding string) any {
		return api.HistoryGapList{Gaps: []api.HistoryGap{
			{Seq: 1},
			{Seq: 922337203685477580, Detail: padding},
		}}
	}
	gapPadding := historyBoundaryPadding(t, gapBuild)
	gapCandidate := gapBuild(gapPadding).(api.HistoryGapList)
	gapCandidate.NextAfter = gapCandidate.Gaps[1].Seq
	if fitsHistoryBody(gapCandidate) {
		t.Fatal("gap fixture must cross the bound only when nextAfter is encoded")
	}
	gapCandidate.Gaps = append(gapCandidate.Gaps, api.HistoryGap{Seq: 922337203685477581})
	gaps, ok := boundGapList(gapCandidate, 2)
	if !ok || len(gaps.Gaps) != 1 || gaps.NextAfter != 1 || !fitsHistoryBody(gaps) {
		t.Fatalf("gap bound=%+v ok=%v bytesFit=%v", gaps, ok, fitsHistoryBody(gaps))
	}

	messageBuild := func(padding string) any {
		return api.WorkItemMessageList{Links: []api.WorkItemMessageLink{
			{Message: api.Message{Seq: 1}},
			{Message: api.Message{Seq: 922337203685477580, Text: padding}},
		}}
	}
	messagePadding := historyBoundaryPadding(t, messageBuild)
	messageCandidate := messageBuild(messagePadding).(api.WorkItemMessageList)
	messageCandidate.NextAfter = messageCandidate.Links[1].Message.Seq
	if fitsHistoryBody(messageCandidate) {
		t.Fatal("message fixture must cross the bound only when nextAfter is encoded")
	}
	messageCandidate.Links = append(messageCandidate.Links, api.WorkItemMessageLink{Message: api.Message{Seq: 922337203685477581}})
	messages, ok := boundMessageList(messageCandidate, 2)
	if !ok || len(messages.Links) != 1 || messages.NextAfter != 1 || !fitsHistoryBody(messages) {
		t.Fatalf("message bound=%+v ok=%v bytesFit=%v", messages, ok, fitsHistoryBody(messages))
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

// Refused work-item writes by a database handler are recorded with a fixed
// code (wi_fc1396aef8a72a06, order #14869); other callers are not.
func TestHandlerWriteRefusalHTTPRecording(t *testing.T) {
	c := newClient(t)
	task := c.task("refusals")
	var handler api.Agent
	if code := c.do("POST", "/v1/tasks/"+task.ID+"/agents", api.AddAgentRequest{Name: "database", Role: api.AgentRoleDatabaseHandler, AgentID: api.NewID("agt"), Host: "devbox", Session: "tt-database", Runtime: "claude"}, &handler); code != 201 {
		t.Fatalf("handler %d", code)
	}
	builder := c.agent(task, "builder")
	var item api.WorkItem
	if code := c.do("POST", "/v1/tasks/"+task.ID+"/work-items", api.CreateWorkItemRequest{Kind: "bug", Title: "Refusal fixture", RequestID: "refusal-create"}, &item); code != 201 {
		t.Fatalf("create %d", code)
	}
	title := "Stale"
	base := "/v1/tasks/" + task.ID + "/work-items/" + item.ID
	// PATCH: a stale handler write is recorded each time (no request ID).
	for i := 0; i < 2; i++ {
		if code := c.do("PATCH", base, api.UpdateWorkItemRequest{Revision: 9, Title: &title, AgentID: handler.ID}, nil); code != 409 {
			t.Fatalf("stale patch = %d", code)
		}
	}
	// /updates: a replayed refused request records once.
	update := api.CreateWorkItemUpdate{ExpectedRevision: 9, Title: &title, AgentID: handler.ID, RunID: handler.RunID, RequestID: "refused-update"}
	for i := 0; i < 2; i++ {
		if code := c.do("POST", base+"/updates", update, nil); code != 409 {
			t.Fatalf("stale update = %d", code)
		}
	}
	// dispatch: an invalid handler dispatch is recorded.
	if code := c.do("POST", base+"/dispatch", api.DispatchWorkItemRequest{Revision: 9, AgentID: handler.ID, RequestID: "refused-dispatch"}, nil); code < 400 || code >= 500 {
		t.Fatalf("refused dispatch = %d", code)
	}
	// Not recorded: the owner, a builder, and a denied (403) caller.
	if code := c.do("PATCH", base, api.UpdateWorkItemRequest{Revision: 9, Title: &title}, nil); code != 409 {
		t.Fatalf("owner stale patch = %d", code)
	}
	if code := c.do("PATCH", base, api.UpdateWorkItemRequest{Revision: 9, Title: &title, AgentID: builder.ID}, nil); code != 409 {
		t.Fatalf("builder stale patch = %d", code)
	}
	denied, _ := json.Marshal(api.UpdateWorkItemRequest{Revision: 9, Title: &title, AgentID: handler.ID})
	req, _ := http.NewRequest("PATCH", c.srv.URL+base, strings.NewReader(string(denied)))
	req.Header.Set("X-Test-Deny", "1")
	if res, err := http.DefaultClient.Do(req); err != nil || res.StatusCode != 403 {
		t.Fatalf("denied patch %v %v", res, err)
	} else {
		res.Body.Close()
	}
	var good api.WorkItemUpdateResult
	if code := c.do("POST", base+"/updates", api.CreateWorkItemUpdate{ExpectedRevision: 1, Title: &title, AgentID: handler.ID, RunID: handler.RunID, RequestID: "saved-update"}, &good); code != 201 {
		t.Fatalf("saved update = %d", code)
	}
	if code := c.do("POST", base+"/updates", api.CreateWorkItemUpdate{ExpectedRevision: 1, Title: &title, AgentID: handler.ID, RunID: handler.RunID, RequestID: "saved-update"}, nil); code != 200 {
		t.Fatalf("replayed saved update = %d", code)
	}
	refusals, err := c.st.HandlerWriteRefusals(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range refusals {
		if r.AgentID != handler.ID || r.RunID != handler.RunID || r.ItemID != item.ID {
			t.Fatalf("refusal identity %+v", r)
		}
		got = append(got, r.Route+"/"+r.Code+"/"+fmt.Sprint(r.Status))
	}
	if len(got) != 4 || got[0] != "update/stale_revision/409" || got[1] != "update/stale_revision/409" || got[2] != "updates/stale_revision/409" || !strings.HasPrefix(got[3], "dispatch/") {
		t.Fatalf("refusals %v", got)
	}
}
