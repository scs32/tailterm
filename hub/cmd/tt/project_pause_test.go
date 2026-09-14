package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/server"
	"github.com/scs32/tailterm/hub/internal/spawn"
	"github.com/scs32/tailterm/hub/internal/store"
)

func writeProjectPauseFixture(t *testing.T, name string, value any) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestProjectPauseCLIUsesExactSavedLifecycleRequests(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "project-pause-cli.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	by := api.Caller{Node: "fixture", User: "owner"}
	task, err := st.CreateTask(ctx, api.CreateTaskRequest{Name: "CLI pause fixture", Orchestrator: "lead"}, by)
	if err != nil {
		t.Fatal(err)
	}
	lead, err := st.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "lead", Host: "fixture", Session: "lead", Runtime: "codex"}, by)
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(server.New(st, func(*http.Request) (api.Caller, error) { return by, nil }))
	t.Cleanup(httpServer.Close)
	e := env{hub: httpServer.URL, task: task.ID}
	if err = cmdProjectPause(e, []string{"get"}); err != nil {
		t.Fatal(err)
	}
	pause := api.PauseProjectRequest{Version: 1, RequestID: "cli-pause-response-loss", ExpectedLifecycleGeneration: 0,
		Targets: []api.ProjectPauseTargetRequest{{AgentID: lead.ID, RunID: lead.RunID, ServiceDisposition: api.PauseServiceNone}}}
	pauseFile := writeProjectPauseFixture(t, "pause.json", pause)
	if err = cmdProjectPause(e, []string{"pause", "--file", pauseFile}); err != nil {
		t.Fatal(err)
	}
	if err = cmdProjectPause(e, []string{"pause", "--file", pauseFile}); err != nil {
		t.Fatalf("same saved pause request must replay: %v", err)
	}
	if _, err = st.ReportCleanup(ctx, lead.ID, api.CleanupRequest{RunID: lead.RunID}, by); err != nil {
		t.Fatal(err)
	}
	paused, err := st.ProjectPauseStatus(ctx, task.ID)
	if err != nil || paused.State != api.ProjectPausePaused || paused.RetainedHandoffDigest == "" {
		t.Fatalf("fully paused fixture: %+v err=%v", paused, err)
	}
	plannedLead := api.ProjectResumeOrchestrator{AgentID: api.NewID("agt"), RunID: api.NewID("run"), Name: "fresh-lead"}
	resume := api.ResumeProjectRequest{Version: 1, RequestID: "cli-resume-response-loss", ExpectedPauseGeneration: paused.PauseGeneration,
		ExpectedLifecycleGeneration: paused.LifecycleGeneration, RetainedHandoffDigest: paused.RetainedHandoffDigest,
		SelectedTeamID: "fixture-team", Orchestrator: plannedLead}
	resumeFile := writeProjectPauseFixture(t, "resume.json", resume)
	if err = cmdProjectPause(e, []string{"resume", "--file", resumeFile}); err != nil {
		t.Fatal(err)
	}
	if err = cmdProjectPause(e, []string{"resume", "--file", resumeFile}); err != nil {
		t.Fatalf("same saved resume request must replay: %v", err)
	}
	resuming, err := st.ResumeProject(ctx, task.ID, resume, by)
	if err != nil || !resuming.Replay || resuming.State != api.ProjectPauseResuming || resuming.ResumeAdmission == nil || resuming.Receipt == nil {
		t.Fatalf("resuming fixture: %+v err=%v", resuming, err)
	}
	fresh, err := st.AddAgent(ctx, task.ID, api.AddAgentRequest{AgentID: plannedLead.AgentID, ExpectedRunID: plannedLead.RunID,
		ResumeReceiptID: resuming.Receipt.ID, Name: plannedLead.Name, Host: "fixture", Session: "fresh-lead", Runtime: "codex",
		ExpectedLifecycleGeneration: resuming.LifecycleGeneration}, by)
	if err != nil {
		t.Fatal(err)
	}
	confirm := api.ConfirmProjectResumeRequest{Version: 1, RequestID: "cli-resume-confirm", ExpectedLifecycleGeneration: resuming.LifecycleGeneration,
		ResumeReceiptID: resuming.Receipt.ID, AgentID: fresh.ID, RunID: fresh.RunID}
	if _, err = st.ConfirmProjectResume(ctx, task.ID, confirm, by); err != nil {
		t.Fatal(err)
	}
	active, err := st.ProjectPauseStatus(ctx, task.ID)
	if err != nil || active.State != api.ProjectPauseActive || active.ResumeAdmission == nil || active.ResumeAdmission.Pending {
		t.Fatalf("fresh exact lead did not atomically clear resume barrier: %+v err=%v", active, err)
	}
}

func TestProjectPauseCLIRequiresBoundedJSONInput(t *testing.T) {
	e := env{task: api.NewID("tsk")}
	if err := cmdProjectPause(e, []string{"pause"}); err == nil {
		t.Fatal("pause without a saved request file was accepted")
	}
	bad := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(bad, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	var request api.PauseProjectRequest
	if err := readProjectPauseRequest(bad, &request); err == nil {
		t.Fatal("malformed request was accepted")
	}
}

func TestSpawnCarriesResumeGenerationAndReceiptThenConfirmsOwnedSession(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux unavailable")
	}
	t.Setenv("TT_TMUX_SOCKET", "tt-project-resume-"+strings.TrimPrefix(api.NewID("agt"), "agt_"))
	t.Setenv("TAILTERM_RELAY_STATE", t.TempDir())
	defer startupTmux(context.Background(), "kill-server")
	taskID, agentID, runID, receiptID := api.NewID("tsk"), api.NewID("agt"), api.NewID("run"), api.NewID("ppr")
	var mu sync.Mutex
	var admitted api.AddAgentRequest
	var confirmed api.ConfirmProjectResumeRequest
	var handlerErr string
	var httpServer *httptest.Server
	httpServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/tasks/"+taskID:
			json.NewEncoder(w).Encode(api.TaskDetail{Task: api.Task{ID: taskID, Name: "Resume spawn fixture", Status: api.TaskOpen, PauseState: api.ProjectPauseResuming, LifecycleGeneration: 2}})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/agents"):
			var req api.AddAgentRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			mu.Lock()
			admitted = req
			mu.Unlock()
			if _, err := startupTmux(context.Background(), "new-session", "-d", "-s", req.Session,
				"-e", "TAILTERM_HUB="+httpServer.URL, "-e", "TAILTERM_TASK="+taskID,
				"-e", "TAILTERM_AGENT="+agentID, "-e", "TAILTERM_RUN="+runID, "sleep 300"); err != nil {
				mu.Lock()
				handlerErr = err.Error()
				mu.Unlock()
				http.Error(w, "synthetic session setup failed", http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(api.Agent{ID: agentID, TaskID: taskID, RunID: runID, Name: req.Name, Host: req.Host, Session: req.Session, Runtime: req.Runtime, Status: api.AgentStarting})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/resume/confirm"):
			var req api.ConfirmProjectResumeRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			mu.Lock()
			confirmed = req
			mu.Unlock()
			json.NewEncoder(w).Encode(api.ProjectPauseStatus{Version: 1, TaskID: taskID, State: api.ProjectPauseActive, LifecycleGeneration: 2})
		default:
			http.NotFound(w, r)
		}
	}))
	defer httpServer.Close()
	err := cmdSpawn(env{}, []string{"--name", "fresh-lead", "--agent-id", agentID, "--expected-run-id", runID,
		"--expected-lifecycle-generation", "2", "--resume-receipt-id", receiptID,
		"--run", "sleep 300", "--runtime", "generic", "--task", taskID, "--hub", httpServer.URL})
	mu.Lock()
	defer mu.Unlock()
	if err != nil || handlerErr != "" {
		t.Fatalf("resume spawn: err=%v handler=%s", err, handlerErr)
	}
	if admitted.AgentID != agentID || admitted.ExpectedRunID != runID || admitted.ExpectedLifecycleGeneration != 2 || admitted.ResumeReceiptID != receiptID {
		t.Fatalf("resume admission fields: %+v", admitted)
	}
	if confirmed.ResumeReceiptID != receiptID || confirmed.AgentID != agentID || confirmed.RunID != runID || confirmed.ExpectedLifecycleGeneration != 2 {
		t.Fatalf("resume confirmation fields: %+v", confirmed)
	}
}

func TestFailedResumeSpawnLeavesBarrierUnconfirmedForExactRetry(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux unavailable")
	}
	t.Setenv("TT_TMUX_SOCKET", "tt-project-resume-fail-"+strings.TrimPrefix(api.NewID("agt"), "agt_"))
	t.Setenv("TAILTERM_RELAY_STATE", t.TempDir())
	taskID, agentID, runID, receiptID := api.NewID("tsk"), api.NewID("agt"), api.NewID("run"), api.NewID("ppr")
	confirmed, closed := 0, 0
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/tasks/"+taskID:
			json.NewEncoder(w).Encode(api.TaskDetail{Task: api.Task{ID: taskID, Status: api.TaskOpen, PauseState: api.ProjectPauseResuming, LifecycleGeneration: 2}})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/agents"):
			var req api.AddAgentRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(api.Agent{ID: agentID, TaskID: taskID, RunID: runID, Name: req.Name, Host: req.Host, Session: req.Session, Runtime: req.Runtime, Status: api.AgentStarting})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/resume/confirm"):
			confirmed++
			json.NewEncoder(w).Encode(api.ProjectPauseStatus{State: api.ProjectPauseActive})
		case r.Method == http.MethodDelete:
			closed++
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer httpServer.Close()
	priorTmux := spawn.Tmux
	spawn.Tmux = filepath.Join(t.TempDir(), "missing-tmux")
	defer func() { spawn.Tmux = priorTmux }()
	err := cmdSpawn(env{}, []string{"--name", "fresh-lead", "--agent-id", agentID, "--expected-run-id", runID,
		"--expected-lifecycle-generation", "2", "--resume-receipt-id", receiptID,
		"--run", "sleep 300", "--runtime", "generic", "--task", taskID, "--hub", httpServer.URL})
	if err == nil || confirmed != 0 || closed != 0 {
		t.Fatalf("failed external launch cleared or destroyed retry state: err=%v confirmed=%d closed=%d", err, confirmed, closed)
	}
}
