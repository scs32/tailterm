package main

import (
	"context"
	"encoding/json"
	"github.com/scs32/tailterm/hub/internal/api"
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
			if failAck {
				http.Error(w, "offline", 503)
				return
			}
			var req api.CleanupRequest
			json.NewDecoder(r.Body).Decode(&req)
			if req.RunID != a.RunID {
				t.Error("wrong run")
			}
			acks++
			a.CleanupDone = req.Error == ""
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
	task.Status = api.TaskClosed
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
	if err != nil || result.Confirmed != 1 || acks != 1 {
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
	// No running server is a confirmed absence, not an execution failure.
	run("kill-server")
	time.Sleep(30 * time.Millisecond)
	result, err = cleanupSessions(ctx, e, task.ID, []string{a.ID})
	if err != nil {
		t.Fatal(result, err)
	}
	t.Setenv("PATH", t.TempDir())
	if _, err = localSessions(ctx); err == nil {
		t.Fatal("missing tmux executable treated as confirmed absence")
	}
}
