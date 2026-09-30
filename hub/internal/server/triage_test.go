package server

import (
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

// a9 (c4): the triage route lists duplicates, already-delivered and stale
// suggestions, rejects a bad window, refuses a non-handler agent session and
// changes nothing.
func TestWorkItemTriageRoute(t *testing.T) {
	c := newClient(t)
	task := c.task("triage-http")
	now := time.Now().UTC()
	c.st.SetClockForTest(func() time.Time { return now })
	file := func(key, title string) api.WorkItem {
		var item api.WorkItem
		if code := c.do("POST", "/v1/tasks/"+task.ID+"/work-items", api.CreateWorkItemRequest{Kind: "bug", Title: title, RequestID: "triage-" + key}, &item); code != 201 {
			t.Fatalf("item %s = %d", key, code)
		}
		return item
	}
	idle := file("idle", "Remove the unused legacy export button")
	now = now.Add(10 * 24 * time.Hour)
	a := file("dup-a", "Board scroll jumps after a new message")
	b := file("dup-b", "Board scroll jumps after new messages arrive")
	shipped := file("shipped", "Relay status keeps showing cleared errors")
	again := file("again", "Relay status keeps showing cleared errors")
	done := "done"
	if code := c.do("POST", "/v1/tasks/"+task.ID+"/work-items/"+shipped.ID+"/updates", api.CreateWorkItemUpdate{ExpectedRevision: shipped.Revision, Status: &done, RequestID: "triage-done"}, nil); code != 201 && code != 200 {
		t.Fatalf("done = %d", code)
	}
	var before, after api.WorkItemList
	c.do("GET", "/v1/tasks/"+task.ID+"/work-items", nil, &before)
	var got api.WorkItemTriage
	if code := c.do("GET", "/v1/tasks/"+task.ID+"/work-items/triage?staleDays=7", nil, &got); code != 200 {
		t.Fatalf("triage = %d", code)
	}
	if len(got.Duplicates) != 1 || got.Duplicates[0].Items[0].ID != a.ID || got.Duplicates[0].Items[1].ID != b.ID {
		t.Fatalf("duplicates %+v", got.Duplicates)
	}
	if len(got.AlreadyReleased) != 1 || got.AlreadyReleased[0].Item.ID != again.ID || got.AlreadyReleased[0].Done.ID != shipped.ID {
		t.Fatalf("already released %+v", got.AlreadyReleased)
	}
	if len(got.Stale) != 1 || got.Stale[0].Item.ID != idle.ID || got.StaleDays != 7 {
		t.Fatalf("stale %+v", got.Stale)
	}
	c.do("GET", "/v1/tasks/"+task.ID+"/work-items", nil, &after)
	if len(before.Items) != len(after.Items) {
		t.Fatalf("triage changed items: %d -> %d", len(before.Items), len(after.Items))
	}
	for i := range before.Items {
		if before.Items[i].Revision != after.Items[i].Revision {
			t.Fatalf("triage changed %s", before.Items[i].ID)
		}
	}
	if code := c.do("GET", "/v1/tasks/"+task.ID+"/work-items/triage?staleDays=0", nil, nil); code != 400 {
		t.Fatalf("zero-day window = %d", code)
	}
	var worker api.Agent
	if code := c.do("POST", "/v1/tasks/"+task.ID+"/agents", api.AddAgentRequest{AgentID: api.NewID("agt"), Name: "worker", Host: "fixture", Session: "worker", Runtime: "codex", Cwd: "/tmp"}, &worker); code != 201 {
		t.Fatalf("worker = %d", code)
	}
	if code := c.do("GET", "/v1/tasks/"+task.ID+"/work-items/triage?agentId="+worker.ID+"&runId="+worker.RunID, nil, nil); code != 409 {
		t.Fatalf("worker triage = %d", code)
	}
}
