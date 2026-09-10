package server

import (
	"net/http"
	"testing"

	"github.com/scs32/tailterm/hub/internal/api"
)

func TestAgentCloseRequiresCurrentRunAndLeavesTaskOpen(t *testing.T) {
	c := newClient(t)
	task := c.task("open closeout")
	agent := c.agent(task, "worker")
	path := "/v1/tasks/" + task.ID + "/agents/" + agent.ID
	if code := c.do("DELETE", path, nil, nil); code != http.StatusBadRequest {
		t.Fatalf("missing run close = %d", code)
	}
	if code := c.do("DELETE", path+"?runId="+api.NewID("run"), nil, nil); code != http.StatusConflict {
		t.Fatalf("stale run close = %d", code)
	}
	var closed api.Agent
	if code := c.do("DELETE", path+"?runId="+agent.RunID, nil, &closed); code != http.StatusOK || closed.Status != api.AgentClosed || closed.CleanupDone {
		t.Fatalf("exact close = %d %+v", code, closed)
	}
	if code := c.do("POST", path+"/cleanup", api.CleanupRequest{RunID: agent.RunID}, &closed); code != http.StatusOK || !closed.CleanupDone {
		t.Fatalf("open-parent cleanup = %d %+v", code, closed)
	}
	var detail api.TaskDetail
	if code := c.do("GET", "/v1/tasks/"+task.ID, nil, &detail); code != http.StatusOK || detail.Task.Status != api.TaskOpen {
		t.Fatalf("parent changed = %d %+v", code, detail.Task)
	}
}
