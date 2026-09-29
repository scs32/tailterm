package server

import (
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/scs32/tailterm/hub/internal/api"
)

func (c *client) workItem(task api.Task, key string) api.WorkItem {
	c.t.Helper()
	var item api.WorkItem
	if code := c.do("POST", "/v1/tasks/"+task.ID+"/work-items", api.CreateWorkItemRequest{Kind: "feature", Title: "Synthetic " + key, RequestID: key}, &item); code != 201 {
		c.t.Fatalf("create work item %s: %d", key, code)
	}
	return item
}

func TestInterventionHTTPCreateReplayAndRejections(t *testing.T) {
	c := newClient(t)
	task := c.task("intervention routes")
	home := c.task("intervention product home")
	other := c.task("intervention other")
	item, product, otherItem := c.workItem(task, "concerned"), c.workItem(home, "product"), c.workItem(other, "other")
	path := "/v1/tasks/" + task.ID + "/interventions"
	req := api.CreateInterventionRequest{Kind: "nudge", ItemID: item.ID, ProductItemID: product.ID, Text: "Nudged the stalled builder", RequestID: "http-intervention"}

	var created, replay api.Message
	if code := c.do("POST", path, req, &created); code != 201 || created.Intervention == nil || created.PostReceipt == nil {
		t.Fatalf("create = %d %+v", code, created)
	}
	want := api.Intervention{Kind: "nudge", ItemTaskID: task.ID, ItemID: item.ID, ProductTaskID: home.ID, ProductItemID: product.ID}
	wantLinks := []api.MessageWorkItem{{ItemTaskID: task.ID, ItemID: item.ID, ItemRevision: item.Revision, Relationship: "primary"}}
	if *created.Intervention != want || created.From != (api.Sender{Node: c.who.Node, User: c.who.User}) || !reflect.DeepEqual(created.WorkItems, wantLinks) {
		t.Fatalf("created = %+v", created)
	}
	if code := c.do("POST", path, req, &replay); code != 201 || replay.Seq != created.Seq || replay.PostReceipt.ID != created.PostReceipt.ID {
		t.Fatalf("replay = %d %+v", code, replay)
	}
	changed := req
	changed.Kind = "release"
	if code := c.do("POST", path, changed, nil); code != http.StatusConflict {
		t.Fatalf("changed payload = %d", code)
	}
	var board api.MessageList
	if code := c.do("GET", "/v1/tasks/"+task.ID+"/messages", nil, &board); code != 200 || len(board.Messages) != 1 || board.Messages[0].Intervention == nil || *board.Messages[0].Intervention != want || !reflect.DeepEqual(board.Messages[0].WorkItems, wantLinks) {
		t.Fatalf("board = %d %+v", code, board)
	}

	for _, tc := range []struct {
		name   string
		mutate func(*api.CreateInterventionRequest)
		want   int
	}{
		{"unknown kind", func(r *api.CreateInterventionRequest) { r.Kind = "foo" }, 400},
		{"blank text", func(r *api.CreateInterventionRequest) { r.Text = "  " }, 400},
		{"unknown item", func(r *api.CreateInterventionRequest) { r.ItemID = "wi_0000000000000001" }, 404},
		{"other project item", func(r *api.CreateInterventionRequest) { r.ItemID = otherItem.ID }, 404},
		{"unknown product item", func(r *api.CreateInterventionRequest) { r.ProductItemID = "wi_0000000000000001" }, 404},
		{"agent identity", func(r *api.CreateInterventionRequest) { r.AgentID = "agt_0000000000000001" }, 403},
	} {
		bad := req
		bad.RequestID = "http-reject-" + strings.ReplaceAll(tc.name, " ", "-")
		tc.mutate(&bad)
		if code := c.do("POST", path, bad, nil); code != tc.want {
			t.Fatalf("%s = %d, want %d", tc.name, code, tc.want)
		}
	}
	if code := c.do("POST", path, `{"kind":`, nil); code != 400 {
		t.Fatalf("malformed JSON = %d", code)
	}
	board = api.MessageList{}
	if code := c.do("GET", "/v1/tasks/"+task.ID+"/messages", nil, &board); code != 200 || len(board.Messages) != 1 {
		t.Fatalf("rejections wrote messages: %d %+v", code, board)
	}

	// Interventions are immutable: there is no route to change or remove one.
	for _, method := range []string{"PATCH", "PUT", "DELETE"} {
		if code := c.do(method, path, map[string]any{}, nil); code != http.StatusMethodNotAllowed {
			t.Fatalf("%s interventions = %d", method, code)
		}
	}
	if code := c.do("DELETE", path+"/1", nil, nil); code != http.StatusNotFound {
		t.Fatalf("DELETE one intervention = %d", code)
	}

	if code := c.do("DELETE", "/v1/tasks/"+task.ID, nil, nil); code != 200 {
		t.Fatalf("close task = %d", code)
	}
	closed := req
	closed.RequestID = "http-closed"
	if code := c.do("POST", path, closed, nil); code != http.StatusConflict {
		t.Fatalf("closed project = %d", code)
	}
}

func TestInterventionHTTPListBucketsByTimeZone(t *testing.T) {
	c := newClient(t)
	task := c.task("intervention list")
	item := c.workItem(task, "listed")
	path := "/v1/tasks/" + task.ID + "/interventions"
	for i, kind := range []string{"nudge", "status", "nudge"} {
		req := api.CreateInterventionRequest{Kind: kind, ItemID: item.ID, Text: "Synthetic intervention", RequestID: "list-" + kind + string(rune('a'+i))}
		if code := c.do("POST", path, req, nil); code != 201 {
			t.Fatalf("create %s = %d", kind, code)
		}
	}
	var page api.InterventionList
	if code := c.do("GET", path+"?tz=America/Los_Angeles&limit=2", nil, &page); code != 200 || len(page.Interventions) != 2 || page.NextAfter != page.Interventions[1].Seq {
		t.Fatalf("first page = %d %+v", code, page)
	}
	if page.Summary.TimeZone != "America/Los_Angeles" || page.Summary.Total != 3 || page.Summary.Linked != 0 || page.Summary.ByKind["nudge"] != 2 || page.Summary.ByKind["status"] != 1 || len(page.Summary.Unlinked) != 3 {
		t.Fatalf("summary = %+v", page.Summary)
	}
	var rest api.InterventionList
	if code := c.do("GET", path+"?tz=America/Los_Angeles&limit=2&after="+strconv.FormatInt(page.NextAfter, 10), nil, &rest); code != 200 || len(rest.Interventions) != 1 || rest.NextAfter != 0 || !reflect.DeepEqual(rest.Summary, page.Summary) {
		t.Fatalf("second page = %d %+v", code, rest)
	}
	for _, query := range []string{"?tz=Mars/Olympus", "?tz=Local", "?limit=0", "?limit=101", "?after=bad"} {
		if code := c.do("GET", path+query, nil, nil); code != 400 {
			t.Fatalf("GET %s = %d", query, code)
		}
	}
	if code := c.do("GET", "/v1/tasks/tsk_0000000000000001/interventions", nil, nil); code != 404 {
		t.Fatalf("unknown project = %d", code)
	}
}

// The Discord bridge credential cannot record or read interventions.
func TestInterventionRoutesAreNotBridgeRoutes(t *testing.T) {
	h := newTokenHub(t)
	task, _, _ := h.project()
	base := "/v1/tasks/" + task.ID + "/interventions"
	for _, method := range []string{"POST", "GET"} {
		if code, _ := h.do(bridgeToken, method, base, map[string]any{}, nil); code != http.StatusForbidden {
			t.Fatalf("bridge %s interventions = %d", method, code)
		}
	}
}
