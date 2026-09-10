package main

import (
	"context"
	"encoding/json"
	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/spawn"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCleanupStopsOnlyOwnedClosedTaskAndRetriesReceipt(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux unavailable")
	}
	t.Setenv("TT_TMUX_SOCKET", "tt-cleanup-"+strings.TrimPrefix(api.NewID("agt"), "agt_"))
	t.Setenv("TAILTERM_RELAY_STATE", t.TempDir())
	ctx := context.Background()
	defer startupTmux(ctx, "kill-server")
	task := api.Task{ID: api.NewID("tsk"), Status: api.TaskOpen}
	a := api.Agent{ID: api.NewID("agt"), TaskID: task.ID, RunID: api.NewID("run"), Session: "owned", Status: api.AgentRetired}
	failAck := false
	failRead := false
	acks := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			var req api.CleanupRequest
			json.NewDecoder(r.Body).Decode(&req)
			if req.RunID != a.RunID {
				t.Error("wrong run")
			}
			acks++
			a.CleanupDone = req.Error == ""
			if failAck {
				// The hub committed the receipt, but the response was lost.
				http.Error(w, "offline after commit", 503)
				return
			}
			json.NewEncoder(w).Encode(a)
			return
		}
		if failRead {
			http.Error(w, "offline", 503)
			return
		}
		json.NewEncoder(w).Encode(api.TaskDetail{Task: task, Agents: []api.Agent{a}})
	}))
	defer server.Close()
	e := env{hub: server.URL}
	run := func(args ...string) {
		t.Helper()
		if _, err := startupTmux(ctx, args...); err != nil {
			t.Fatal(args, err)
		}
	}
	run("new-session", "-d", "-s", "ordinary", "sleep 300")
	run("new-session", "-d", "-s", "owned", "-e", "TAILTERM_HUB="+e.hub, "-e", "TAILTERM_TASK="+task.ID, "-e", "TAILTERM_AGENT="+a.ID, "-e", "TAILTERM_RUN="+a.RunID, "sleep 300")
	if _, err := cleanupSessions(ctx, e, "", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := startupTmux(ctx, "has-session", "-t", "=owned"); err != nil {
		t.Fatal("retirement killed session")
	}
	// Renaming does not lose ownership; unavailable hub never implies closure.
	run("rename-session", "-t", "=owned", "renamed")
	a.Status = api.AgentClosed
	failRead = true
	cleanupSessions(ctx, e, "", nil)
	if _, err := startupTmux(ctx, "has-session", "-t", "=renamed"); err != nil {
		t.Fatal("unreachable hub killed session")
	}
	failRead = false
	failAck = true
	result, err := cleanupSessions(ctx, e, "", nil)
	if err != nil || len(result.Errors) == 0 {
		t.Fatal(result, err)
	}
	if _, err := startupTmux(ctx, "has-session", "-t", "=renamed"); err == nil {
		t.Fatal("closed task session survived")
	}
	records, _ := filepath.Glob(filepath.Join(relayDir(), "*.session.json"))
	if len(records) != 1 {
		t.Fatal("lost pending receipt")
	}
	if _, err := startupTmux(ctx, "has-session", "-t", "=ordinary"); err != nil {
		t.Fatal("ordinary session killed")
	}
	failAck = false
	result, err = cleanupSessions(ctx, e, "", nil)
	if err != nil || result.Confirmed != 1 || acks != 2 {
		t.Fatal(result, err, acks)
	}
	records, _ = filepath.Glob(filepath.Join(relayDir(), "*.session.json"))
	if len(records) != 0 {
		t.Fatal("receipt not cleared")
	}
	// A registry entry referring to a reused ID may never stop its replacement.
	current, _ := localSessions(ctx)
	s := current[0]
	s.Hub = e.hub
	s.Task = task.ID
	s.Agent = a.ID
	s.Run = a.RunID
	if err := stopOwnedSession(ctx, s); err == nil {
		t.Fatal("accepted unrelated replacement")
	}
	if _, err := startupTmux(ctx, "has-session", "-t", "=ordinary"); err != nil {
		t.Fatal("replacement killed")
	}
	// An older run for the same agent is not part of individual closeout while
	// the parent stays open. It remains pending rather than being guessed away.
	a.CleanupDone = false
	oldRun := api.NewID("run")
	run("new-session", "-d", "-s", "older-run", "-e", "TAILTERM_HUB="+e.hub, "-e", "TAILTERM_TASK="+task.ID, "-e", "TAILTERM_AGENT="+a.ID, "-e", "TAILTERM_RUN="+oldRun, "sleep 300")
	result, err = cleanupSessions(ctx, e, task.ID, []string{a.ID})
	if err != nil || len(result.Errors) == 0 {
		t.Fatal("mismatched run was not left pending", result, err)
	}
	if _, err := startupTmux(ctx, "has-session", "-t", "=older-run"); err != nil {
		t.Fatal("individual close killed an older run")
	}
	run("kill-session", "-t", "=older-run")
	// No running server is a confirmed absence, not an execution failure.
	run("kill-server")
	time.Sleep(30 * time.Millisecond)
	result, err = cleanupSessions(ctx, e, task.ID, []string{a.ID})
	if err != nil || result.Confirmed != 1 || !a.CleanupDone {
		t.Fatal(result, err)
	}
	t.Setenv("PATH", t.TempDir())
	if _, err = localSessions(ctx); err == nil {
		t.Fatal("missing tmux executable treated as confirmed absence")
	}
}

func TestCloseUsesExactRunIntentAndOwnedSessionReceipt(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux unavailable")
	}
	t.Setenv("TT_TMUX_SOCKET", "tt-close-"+strings.TrimPrefix(api.NewID("agt"), "agt_"))
	t.Setenv("TAILTERM_RELAY_STATE", t.TempDir())
	ctx := context.Background()
	defer startupTmux(ctx, "kill-server")
	task := api.Task{ID: api.NewID("tsk"), Status: api.TaskOpen}
	a := api.Agent{ID: api.NewID("agt"), TaskID: task.ID, RunID: api.NewID("run"), Name: "worker", Session: "owned", Host: spawn.Host(), Status: api.AgentDone}
	closeRun := ""
	acks := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "DELETE":
			closeRun = r.URL.Query().Get("runId")
			if closeRun != a.RunID {
				http.Error(w, "stale", http.StatusConflict)
				return
			}
			a.Status = api.AgentClosed
			json.NewEncoder(w).Encode(a)
		case r.Method == "POST":
			acks++
			a.CleanupDone = true
			json.NewEncoder(w).Encode(a)
		case strings.HasSuffix(r.URL.Path, "/agents/"+a.ID):
			json.NewEncoder(w).Encode(a)
		default:
			json.NewEncoder(w).Encode(api.TaskDetail{Task: task, Agents: []api.Agent{a}})
		}
	}))
	defer server.Close()
	e := env{hub: server.URL, task: task.ID}
	if _, err := startupTmux(ctx, "new-session", "-d", "-s", a.Session, "-e", "TAILTERM_HUB="+e.hub, "-e", "TAILTERM_TASK="+task.ID, "-e", "TAILTERM_AGENT="+a.ID, "-e", "TAILTERM_RUN="+a.RunID, "sleep 300"); err != nil {
		t.Fatal(err)
	}
	if err := cmdClose(e, []string{a.ID}); err != nil {
		t.Fatal(err)
	}
	if closeRun != a.RunID || acks != 1 || !a.CleanupDone {
		t.Fatalf("close/receipt mismatch: run=%q acks=%d agent=%+v", closeRun, acks, a)
	}
	if task.Status != api.TaskOpen {
		t.Fatal("close changed parent")
	}
	if _, err := startupTmux(ctx, "has-session", "-t", "="+a.Session); err == nil {
		t.Fatal("owned session survived exact close")
	}
}

func TestCloseProtectsContinuingProjectRoles(t *testing.T) {
	task := api.Task{ID: api.NewID("tsk"), Status: api.TaskOpen, Orchestrator: "lead"}
	for _, tc := range []struct {
		name string
		role string
		want string
	}{
		{name: "lead", want: "orchestrator"},
		{name: "records", role: api.AgentRoleDatabaseHandler, want: "database handler"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := api.Agent{ID: api.NewID("agt"), TaskID: task.ID, RunID: api.NewID("run"), Name: tc.name, Role: tc.role, Host: spawn.Host(), Status: api.AgentDone}
			writes := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" {
					writes++
				}
				if strings.HasSuffix(r.URL.Path, "/agents/"+a.ID) {
					json.NewEncoder(w).Encode(a)
					return
				}
				json.NewEncoder(w).Encode(api.TaskDetail{Task: task, Agents: []api.Agent{a}})
			}))
			defer server.Close()
			err := cmdClose(env{hub: server.URL, task: task.ID}, []string{a.ID})
			if err == nil || !strings.Contains(err.Error(), tc.want) || writes != 0 {
				t.Fatalf("protected close = %v, writes=%d", err, writes)
			}
		})
	}
}

func TestResolveCloseAgentRejectsSameNameReplacementAmbiguity(t *testing.T) {
	task := api.NewID("tsk")
	name := "worker"
	agents := []api.Agent{
		{ID: api.NewID("agt"), Name: name, Status: api.AgentClosed, CleanupDone: false},
		{ID: api.NewID("agt"), Name: name, Status: api.AgentDone},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(api.AgentList{Agents: agents})
	}))
	defer server.Close()
	c, err := api.NewClient(server.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = resolveCloseAgent(context.Background(), c, task, name); err == nil || !strings.Contains(err.Error(), "exact agent ID") {
		t.Fatalf("ambiguous name close = %v", err)
	}
	agents[0].CleanupDone = true
	resolved, err := resolveCloseAgent(context.Background(), c, task, name)
	if err != nil || resolved != agents[1].ID {
		t.Fatalf("resolved replacement = %q, %v", resolved, err)
	}
}
