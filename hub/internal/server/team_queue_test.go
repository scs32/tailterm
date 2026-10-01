package server

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/store"
)

// The team queue listing trims history to summaries over HTTP; one entry,
// or one item, still reads in full, and bad parameters are refused.
func TestTeamQueueListingHTTPSummariesDetailAndParameters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.sqlite")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	c := &client{t: t, st: st, who: api.Caller{Node: "devbox", User: "stephen@example.com"}}
	c.srv = httptest.NewServer(New(st, func(*http.Request) (api.Caller, error) { return c.who, nil }))
	t.Cleanup(c.srv.Close)
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { raw.Close() })
	task := c.task("team-queue-listing")
	item := func(key string) api.WorkItem {
		var it api.WorkItem
		if code := c.do("POST", "/v1/tasks/"+task.ID+"/work-items", api.CreateWorkItemRequest{Kind: "bug", Title: "Listing fixture", RequestID: key}, &it); code != 201 {
			t.Fatalf("item %s = %d", key, code)
		}
		return it
	}
	finishedItem, runningItem := item("finished"), item("running")
	launch := `{"members":[{"fields":{"name":"lead","role":"Delivery lead and orchestrator"}},{"fields":{"name":"builder","role":"Implementation"}}],"pad":"` + strings.Repeat("l", 8192) + `"}`
	closeJSON := `{"pad":"` + strings.Repeat("c", 4096) + `"}`
	now := time.Now().UTC().Format(time.RFC3339Nano)
	finished, running := api.NewID("tqe"), api.NewID("tqe")
	for _, row := range []struct {
		id, item, state string
		position        int
	}{{finished, finishedItem.ID, "finished", 1}, {running, runningItem.ID, "running", 2}} {
		if _, err := raw.Exec(`INSERT INTO team_queue_entries(id,task_id,item_id,item_revision,order_seq,template,position,state,revision,host,cwd,launch_json,close_json,created_at,updated_at) VALUES(?,?,?,1,1,'planned',?,?,2,'fixture','/tmp',?,?,?,?)`, row.id, task.ID, row.item, row.position, row.state, launch, closeJSON, now, now); err != nil {
			t.Fatal(err)
		}
	}
	release := `{"id":"rel_listing","taskId":"` + task.ID + `","entryId":"` + finished + `","itemId":"` + finishedItem.ID + `","state":"released","plan":{"itemId":"` + finishedItem.ID + `","checks":[{"id":"a1","argv":["go","test"],"cwd":"hub","environment":{}}]}}`
	if _, err := raw.Exec(`INSERT INTO release_jobs(task_id,id,entry_id,state,generation,record_json) VALUES(?,?,?,?,?,?)`, task.ID, "rel_listing", finished, "released", 1, release); err != nil {
		t.Fatal(err)
	}
	hub, err := api.NewClient(c.srv.URL, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	list, err := hub.ListTeamQueue(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Entries) != 2 || list.Entries[0].ID != running || list.Entries[1].ID != finished || list.History == nil || list.History.Total != 1 || list.History.Limit != api.DefaultTeamQueueHistoryLimit {
		t.Fatalf("default listing %+v", list)
	}
	if active := list.Entries[0]; active.Summary || string(active.LaunchJSON) != launch || string(active.CloseJSON) != closeJSON {
		t.Fatal("the running entry is not in full")
	}
	sum := list.Entries[1]
	if !sum.Summary || len(sum.LaunchJSON) != 0 || len(sum.CloseJSON) != 0 || sum.TeamShape != "plan-only" || sum.Release == nil || sum.Release.ID != "rel_listing" || len(sum.Release.Plan.Checks) != 0 {
		t.Fatalf("history summary %+v", sum)
	}

	full, err := hub.GetTeamQueueEntry(ctx, task.ID, finished)
	if err != nil || full.Summary || string(full.LaunchJSON) != launch || string(full.CloseJSON) != closeJSON || full.Release == nil || len(full.Release.Plan.Checks) != 1 {
		t.Fatalf("entry detail %+v %v", full, err)
	}
	byItem, err := hub.ListTeamQueuePage(ctx, task.ID, api.TeamQueueListOptions{Item: finishedItem.ID})
	if err != nil || len(byItem.Entries) != 1 || byItem.Entries[0].ID != finished || byItem.Entries[0].Summary || string(byItem.Entries[0].LaunchJSON) != launch || byItem.History != nil {
		t.Fatalf("item listing %+v %v", byItem, err)
	}
	activeOnly, err := hub.ListTeamQueuePage(ctx, task.ID, api.TeamQueueListOptions{View: api.TeamQueueViewActive})
	if err != nil || len(activeOnly.Entries) != 1 || activeOnly.Entries[0].ID != running || activeOnly.History != nil {
		t.Fatalf("active listing %+v %v", activeOnly, err)
	}

	base := "/v1/tasks/" + task.ID + "/team-queue"
	for _, query := range []string{"?view=all", "?view=", "?item=tqe_x", "?item=", "?limit=0", "?limit=201", "?limit=x", "?after=-1", "?after=x"} {
		if code := c.do("GET", base+query, nil, nil); code != 400 {
			t.Fatalf("GET %s = %d", query, code)
		}
	}
	var page api.TeamQueueList
	if code := c.do("GET", base+"?limit=1&after=2", nil, &page); code != 200 || len(page.Entries) != 2 || page.History == nil || page.History.Limit != 1 || page.History.NextAfter != 0 {
		t.Fatalf("paged = %d %+v", code, page)
	}
}

// a11: rebind and requeue over HTTP. Refusals are 409 and carry the entry
// ID; a rebind's approver is the authenticated caller, never the body.
func TestTeamQueueRebindAndRequeueHTTP(t *testing.T) {
	c := newClient(t)
	st, ctx := c.st, context.Background()
	c.who = api.Caller{Node: "owner-laptop", User: "owner@example.com"}
	task := c.task("team-queue-rebind")
	handler, err := st.AddAgent(ctx, task.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: "handler", Role: api.AgentRoleDatabaseHandler, Host: "fixture", Session: "handler"}, c.who)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.PostEvent(ctx, task.ID, api.PostEventRequest{AgentID: handler.ID, RunID: handler.RunID, Kind: api.EventRunning}, c.who); err != nil {
		t.Fatal(err)
	}
	var item api.WorkItem
	if code := c.do("POST", "/v1/tasks/"+task.ID+"/work-items", api.CreateWorkItemRequest{Kind: "bug", Title: "Amended after queueing", Description: "first scope", RequestID: "item"}, &item); code != 201 {
		t.Fatalf("item = %d", code)
	}
	linked := func(key string, revision int64) api.Message {
		t.Helper()
		m, err := st.PostMessage(ctx, task.ID, api.PostMessageRequest{Text: key, RequestID: key, WorkItems: []api.MessageWorkItem{{ItemTaskID: task.ID, ItemID: item.ID, ItemRevision: revision, Relationship: "primary"}}}, c.who)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	confirm := func(current api.WorkItem, order int64) {
		t.Helper()
		if _, err := st.ConfirmWorkOrderScope(ctx, task.ID, item.ID, api.ConfirmWorkOrderScopeRequest{RequestID: api.NewID("req"), AgentID: handler.ID, RunID: handler.RunID, ExpectedRevision: current.Revision, ScopeRevision: current.ScopeRevision, OrderMessageSeq: order, Complete: true}); err != nil {
			t.Fatal(err)
		}
	}
	order := linked("bounded-order", item.Revision)
	confirm(item, order.Seq)
	hub, err := api.NewClient(c.srv.URL, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	// conflict runs an action the hub must refuse with 409, naming the entry.
	conflict := func(req api.TeamQueueRequest, entry string, want string) {
		t.Helper()
		req.RequestID = api.NewID("req")
		_, err := hub.TeamQueueAction(ctx, task.ID, req)
		var response *api.HTTPError
		if !errors.As(err, &response) || response.Status != http.StatusConflict || !strings.Contains(response.Msg, entry) || !strings.Contains(response.Msg, want) {
			t.Fatalf("%s = %v, want 409 naming %s with %q", req.Operation, err, entry, want)
		}
	}
	action := func(req api.TeamQueueRequest) api.TeamQueueEntry {
		t.Helper()
		req.RequestID = api.NewID("req")
		q, err := hub.TeamQueueAction(ctx, task.ID, req)
		if err != nil {
			t.Fatalf("%s: %v", req.Operation, err)
		}
		return q
	}
	queued := action(api.TeamQueueRequest{Operation: "add", ItemID: item.ID, OrderMessageSeq: order.Seq, Host: "fixture", Cwd: t.TempDir()})
	if queued.Attempt != 1 || queued.ItemRevision != item.Revision {
		t.Fatalf("queued %+v", queued)
	}

	description := "amended scope"
	updated, err := st.UpdateWorkItem(ctx, task.ID, item.ID, api.UpdateWorkItemRequest{Revision: item.Revision, Description: &description}, api.Caller{Node: "owner-laptop", User: "amender@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	source := linked("scope-amendment", updated.Revision)
	rebind := api.TeamQueueRequest{Operation: "rebind", EntryID: queued.ID, ExpectedRevision: queued.Revision, ItemRevision: updated.Revision, SourceMessageSeq: source.Seq}
	conflict(rebind, queued.ID, "confirm scope for revision")
	confirm(updated, order.Seq)

	// The body cannot name the approver: an unknown approver field is ignored
	// and the authenticated caller is recorded.
	c.who = api.Caller{Node: "handler-host", User: "approver@example.com"}
	var rebound api.TeamQueueEntry
	body := map[string]any{"requestId": "rebind-http", "operation": "rebind", "entryId": queued.ID, "expectedRevision": queued.Revision, "itemRevision": updated.Revision, "sourceMessageSeq": source.Seq,
		"caller": map[string]string{"node": "forged", "user": "forged"}, "Caller": map[string]string{"Node": "forged", "User": "forged"}}
	if code := c.do("POST", "/v1/tasks/"+task.ID+"/team-queue/actions", body, &rebound); code != 200 {
		t.Fatalf("rebind = %d", code)
	}
	if rebound.ID != queued.ID || rebound.ItemRevision != updated.Revision || len(rebound.Rebinds) != 1 {
		t.Fatalf("rebound %+v", rebound)
	}
	history := rebound.Rebinds[0]
	if history.ApprovedBy != (api.Sender{Node: "handler-host", User: "approver@example.com"}) || history.AmendedBy != (api.Sender{Node: "owner-laptop", User: "amender@example.com"}) ||
		history.FromItemRevision != item.Revision || history.ToItemRevision != updated.Revision || history.SourceMessageSeq != source.Seq || history.EntryState != "queued" {
		t.Fatalf("rebind history %+v", history)
	}
	c.who = api.Caller{Node: "owner-laptop", User: "owner@example.com"}
	full, err := hub.GetTeamQueueEntry(ctx, task.ID, queued.ID)
	if err != nil || len(full.Rebinds) != 1 || !reflect.DeepEqual(full.Rebinds[0], history) {
		t.Fatalf("entry detail %+v %v", full, err)
	}
	conflict(api.TeamQueueRequest{Operation: "rebind", EntryID: queued.ID, ExpectedRevision: rebound.Revision, ItemRevision: updated.Revision, SourceMessageSeq: source.Seq}, queued.ID, "already bound")

	// A live entry is not requeued; a failed one must be released first.
	conflict(api.TeamQueueRequest{Operation: "requeue", EntryID: queued.ID, ExpectedRevision: rebound.Revision}, queued.ID, "only a released failed entry")
	failed := action(api.TeamQueueRequest{Operation: "fail", EntryID: queued.ID, ExpectedRevision: rebound.Revision, Failure: "fixture failure"})
	conflict(api.TeamQueueRequest{Operation: "requeue", EntryID: queued.ID, ExpectedRevision: failed.Revision}, queued.ID, "tt team queue release")
	conflict(api.TeamQueueRequest{Operation: "rebind", EntryID: queued.ID, ExpectedRevision: failed.Revision, ItemRevision: updated.Revision, SourceMessageSeq: source.Seq}, queued.ID, "tt team queue requeue")
	released := action(api.TeamQueueRequest{Operation: "release", EntryID: queued.ID, ExpectedRevision: failed.Revision})
	conflict(api.TeamQueueRequest{Operation: "add", ItemID: item.ID, OrderMessageSeq: order.Seq, Host: "fixture", Cwd: t.TempDir()}, queued.ID, "tt team queue requeue")
	retry := action(api.TeamQueueRequest{Operation: "requeue", EntryID: queued.ID, ExpectedRevision: released.Revision})
	if retry.ID == queued.ID || retry.ItemID != item.ID || retry.Attempt != 2 || retry.RetryOf != queued.ID || retry.State != "queued" || retry.ItemRevision != updated.Revision {
		t.Fatalf("retry %+v", retry)
	}
	conflict(api.TeamQueueRequest{Operation: "requeue", EntryID: queued.ID, ExpectedRevision: released.Revision}, queued.ID, retry.ID)

	byItem, err := hub.ListTeamQueuePage(ctx, task.ID, api.TeamQueueListOptions{Item: item.ID})
	if err != nil || len(byItem.Entries) != 2 || byItem.Entries[0].ID != retry.ID || byItem.Entries[1].ID != queued.ID || byItem.Entries[1].Summary || len(byItem.Entries[1].Rebinds) != 1 || byItem.Entries[1].Attempt != 1 {
		t.Fatalf("item listing %+v %v", byItem, err)
	}
	// A history summary leaves the rebind detail to the entry.
	list, err := hub.ListTeamQueue(ctx, task.ID)
	if err != nil || len(list.Entries) != 2 || list.Entries[0].ID != retry.ID || !list.Entries[1].Summary || len(list.Entries[1].Rebinds) != 0 {
		t.Fatalf("listing %+v %v", list, err)
	}
}
