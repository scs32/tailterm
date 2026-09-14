package server

import (
	"net/http"
	"testing"

	"github.com/scs32/tailterm/hub/internal/api"
)

func TestProjectPauseHTTPContractAndGenerationFence(t *testing.T) {
	c := newClient(t)
	task := c.task("synthetic HTTP pause")
	lead := c.agent(task, "lead")
	var handler api.Agent
	handlerRequest := api.AddAgentRequest{AgentID: api.NewID("agt"), Name: "db-handler", Host: "fixture", Session: "handler", Runtime: "codex", Role: api.AgentRoleDatabaseHandler}
	if code := c.do("POST", "/v1/tasks/"+task.ID+"/agents", handlerRequest, &handler); code != http.StatusCreated {
		t.Fatalf("handler = %d", code)
	}
	var capabilities api.Capabilities
	if code := c.do("GET", "/v1/capabilities", nil, &capabilities); code != http.StatusOK || !capabilities.ProjectPause.Supported || len(capabilities.ProjectPause.Versions) != 1 || capabilities.ProjectPause.Versions[0] != 1 {
		t.Fatalf("capability = %d %+v", code, capabilities.ProjectPause)
	}
	var initial api.ProjectPauseStatus
	if code := c.do("GET", "/v1/tasks/"+task.ID+"/pause", nil, &initial); code != http.StatusOK || initial.State != api.ProjectPauseActive || initial.LifecycleGeneration != 0 {
		t.Fatalf("initial = %d %+v", code, initial)
	}
	request := api.PauseProjectRequest{Version: 1, RequestID: "http-pause", Targets: []api.ProjectPauseTargetRequest{{AgentID: lead.ID, RunID: lead.RunID, ServiceDisposition: api.PauseServiceNone}}}
	if code := c.do("POST", "/v1/tasks/"+task.ID+"/pause", request, nil); code != http.StatusConflict {
		t.Fatalf("partial team pause = %d", code)
	}
	request.Targets = append(request.Targets, api.ProjectPauseTargetRequest{AgentID: handler.ID, RunID: handler.RunID, ServiceDisposition: api.PauseServiceNone})
	var paused api.ProjectPauseStatus
	if code := c.do("POST", "/v1/tasks/"+task.ID+"/pause", request, &paused); code != http.StatusOK || paused.State != api.ProjectPauseCleanupPending || paused.Receipt == nil {
		t.Fatalf("pause = %d %+v", code, paused)
	}
	var detail api.TaskDetail
	if code := c.do("GET", "/v1/tasks/"+task.ID, nil, &detail); code != http.StatusOK || detail.Task.Status != api.TaskOpen || detail.Task.PauseState != api.ProjectPauseCleanupPending || detail.Task.PauseCleanupPending != 2 {
		t.Fatalf("detail = %d %+v", code, detail.Task)
	}
	staleAdmission := api.AddAgentRequest{Name: "stale", Host: "fixture", Session: "stale"}
	if code := c.do("POST", "/v1/tasks/"+task.ID+"/agents", staleAdmission, nil); code != http.StatusConflict {
		t.Fatalf("paused admission = %d", code)
	}
	for _, agent := range []api.Agent{lead, handler} {
		if code := c.do("POST", "/v1/tasks/"+task.ID+"/agents/"+agent.ID+"/cleanup", api.CleanupRequest{RunID: agent.RunID}, nil); code != http.StatusOK {
			t.Fatalf("cleanup %s = %d", agent.ID, code)
		}
	}
	if code := c.do("GET", "/v1/tasks/"+task.ID+"/pause", nil, &paused); code != http.StatusOK || paused.State != api.ProjectPausePaused || paused.CleanupPending != 0 {
		t.Fatalf("fully paused = %d %+v", code, paused)
	}
	plannedLead := api.ProjectResumeOrchestrator{AgentID: api.NewID("agt"), RunID: api.NewID("run"), Name: "fresh-lead"}
	resume := api.ResumeProjectRequest{Version: 1, RequestID: "http-resume", ExpectedPauseGeneration: 1, ExpectedLifecycleGeneration: 1, RetainedHandoffDigest: paused.RetainedHandoffDigest, SelectedTeamID: "synthetic-team", Orchestrator: plannedLead}
	var resumed api.ProjectPauseStatus
	if code := c.do("POST", "/v1/tasks/"+task.ID+"/resume", resume, &resumed); code != http.StatusOK || resumed.State != api.ProjectPauseResuming || resumed.LifecycleGeneration != 2 || resumed.Receipt == nil {
		t.Fatalf("resume = %d %+v", code, resumed)
	}
	staleAdmission.ExpectedLifecycleGeneration = 1
	if code := c.do("POST", "/v1/tasks/"+task.ID+"/agents", staleAdmission, nil); code != http.StatusConflict {
		t.Fatalf("stale resumed admission = %d", code)
	}
	freshAdmission := api.AddAgentRequest{AgentID: plannedLead.AgentID, ExpectedRunID: plannedLead.RunID, ResumeReceiptID: resumed.Receipt.ID, Name: plannedLead.Name, Host: "fixture", Session: "fresh", ExpectedLifecycleGeneration: 2}
	var fresh api.Agent
	if code := c.do("POST", "/v1/tasks/"+task.ID+"/agents", freshAdmission, &fresh); code != http.StatusCreated {
		t.Fatalf("fresh resumed admission = %d", code)
	}
	if code := c.do("GET", "/v1/tasks/"+task.ID+"/pause", nil, &resumed); code != http.StatusOK || resumed.State != api.ProjectPauseResuming || resumed.ResumeAdmission == nil || !resumed.ResumeAdmission.Pending || resumed.Receipt == nil || resumed.Receipt.Operation != "pause" {
		t.Fatalf("unconfirmed resumed admission = %d %+v", code, resumed)
	}
	confirm := api.ConfirmProjectResumeRequest{Version: 1, RequestID: "http-resume-confirm", ExpectedLifecycleGeneration: 2, ResumeReceiptID: freshAdmission.ResumeReceiptID, AgentID: fresh.ID, RunID: fresh.RunID}
	if code := c.do("POST", "/v1/tasks/"+task.ID+"/resume/confirm", confirm, &resumed); code != http.StatusOK || resumed.State != api.ProjectPauseActive || resumed.Receipt == nil || resumed.Receipt.Operation != "resume_confirm" {
		t.Fatalf("resume confirmation = %d %+v", code, resumed)
	}
}
