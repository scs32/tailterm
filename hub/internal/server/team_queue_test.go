package server

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
