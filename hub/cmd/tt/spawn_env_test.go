package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/spawn"
)

// A Claude launch turns off Claude Code's prompt suggestion in the session
// environment (wi_9201e1f3901d6fdc); other runtimes are left unchanged.
func TestSpawnDisablesClaudePromptSuggestionOnly(t *testing.T) {
	for _, tc := range []struct {
		runtime string
		want    bool
	}{{"claude", true}, {"codex", false}} {
		t.Run(tc.runtime, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("TAILTERM_RELAY_STATE", filepath.Join(dir, "relay"))
			// Real tmux reads (session adoption) go to a private, empty server.
			t.Setenv("TT_TMUX_SOCKET", "tt-spawn-env-"+strings.TrimPrefix(api.NewID("agt"), "agt_"))
			log := filepath.Join(dir, "tmux-args")
			fake := filepath.Join(dir, "tmux")
			script := "#!/bin/sh\nfor a; do if [ \"$a\" = -V ]; then echo 'tmux 3.4'; exit 0; fi; done\nprintf '%s\\n' \"$@\" >> '" + log + "'\n"
			if err := os.WriteFile(fake, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			prior := spawn.Tmux
			spawn.Tmux = fake
			defer func() { spawn.Tmux = prior }()
			taskID, agentID, runID := api.NewID("tsk"), api.NewID("agt"), api.NewID("run")
			hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/v1/tasks/"+taskID:
					_ = json.NewEncoder(w).Encode(api.TaskDetail{Task: api.Task{ID: taskID, Status: api.TaskOpen, PauseState: api.ProjectPauseActive}})
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/agents"):
					var req api.AddAgentRequest
					_ = json.NewDecoder(r.Body).Decode(&req)
					w.WriteHeader(http.StatusCreated)
					_ = json.NewEncoder(w).Encode(api.Agent{ID: agentID, TaskID: taskID, RunID: runID, Name: req.Name, Host: req.Host, Session: req.Session, Runtime: req.Runtime, Status: api.AgentStarting})
				default:
					http.NotFound(w, r)
				}
			}))
			defer hub.Close()
			if err := cmdSpawn(env{}, []string{"--name", "suggestion-" + tc.runtime, "--run", tc.runtime, "--task", taskID, "--hub", hub.URL}); err != nil {
				t.Fatalf("spawn: %v", err)
			}
			data, err := os.ReadFile(log)
			if err != nil {
				t.Fatal(err)
			}
			args := strings.Split(string(data), "\n")
			created, found := false, false
			for i, arg := range args {
				created = created || arg == "new-session"
				if arg == "-e" && i+1 < len(args) && strings.HasPrefix(args[i+1], "CLAUDE_CODE_ENABLE_PROMPT_SUGGESTION=") {
					found = args[i+1] == "CLAUDE_CODE_ENABLE_PROMPT_SUGGESTION=0"
					if !found {
						t.Fatalf("unexpected suggestion setting %q", args[i+1])
					}
				}
			}
			if !created || found != tc.want {
				t.Fatalf("%s launch: new-session=%v suggestion-off=%v, want %v\n%s", tc.runtime, created, found, tc.want, data)
			}
		})
	}
}
