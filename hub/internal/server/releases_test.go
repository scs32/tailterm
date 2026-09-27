package server

import (
	"github.com/scs32/tailterm/hub/internal/api"
	"testing"
)

func TestReleaseHTTPHandlerBoundaryAndEmptyRead(t *testing.T) {
	c := newClient(t)
	task := c.task("release-http")
	path := "/v1/tasks/" + task.ID + "/releases"
	var jobs []api.ReleaseJob
	if code := c.do("GET", path, nil, &jobs); code != 200 || len(jobs) != 0 {
		t.Fatal(code, jobs)
	}
	req := api.ReleaseRequest{RequestID: "fixture", Operation: "enqueue", AgentID: api.NewID("agt"), RunID: api.NewID("run"), EntryID: api.NewID("tqe")}
	if code := c.do("POST", path+"/actions", req, nil); code != 409 {
		t.Fatal("nonhandler enqueue", code)
	}
}
