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
)

// Instruction contracts, not a test scheduler or evidence of model obedience.
// The actual brief command is allowed only synthetic task/roster GETs: merely
// printing these instructions must not perform allocation or lifecycle writes.
func TestHandlerAllocationEmittedBriefingContracts(t *testing.T) {
	var scenarios []struct {
		Name, Situation       string
		Handler, Lead, Worker []string
	}
	data, err := os.ReadFile("../../../tests/handler-allocation-cases.json")
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(data, &scenarios); err != nil {
		t.Fatal(err)
	}
	task := api.Task{ID: "tsk_0000000000000001", Name: "Synthetic allocation project", Goal: "Synthetic work only", Status: api.TaskOpen, Orchestrator: "lead", AllowAgentSpawn: true, MaxNewAgents: 2}
	agents := []api.Agent{
		{ID: "agt_0000000000000001", Name: "lead", Status: api.AgentRunning},
		{ID: "agt_0000000000000002", Name: "records-custom", Role: api.AgentRoleDatabaseHandler, Status: api.AgentDone},
		{ID: "agt_0000000000000003", Name: "builder", Status: api.AgentRunning},
	}
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != http.MethodGet || r.URL.Path != "/v1/tasks/"+task.ID {
			t.Errorf("brief attempted unexpected operation: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected operation", 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"task": task, "agents": agents})
	}))
	defer srv.Close()
	briefings := map[string]string{}
	for i, role := range []string{"lead", "handler", "worker"} {
		agent := agents[i]
		e := env{hub: srv.URL, task: task.ID, agent: agent.ID, agentName: agent.Name}
		// A file avoids the pipe-capacity limit when a full briefing grows.
		out, err := os.Create(filepath.Join(t.TempDir(), "briefing.txt"))
		if err != nil {
			t.Fatal(err)
		}
		old := os.Stdout
		os.Stdout = out
		err = cmdBrief(e)
		os.Stdout = old
		_ = out.Close()
		if err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(out.Name())
		if err != nil {
			t.Fatal(err)
		}
		got := string(data)
		// cmdSpawn uses this same emitter before assignment/context append.
		// cmdBrief resolves its own real executable path (selfPath()) for
		// the CLI-discovery preamble, so the comparison must too.
		if got != agentTaskBriefingForLaunch(task, agent.Name, agent.Role, selfPath(), agents, 0) {
			t.Fatal("CLI brief and launch role emission diverged")
		}
		briefings[role] = got
		if role != "handler" && !strings.Contains(got, "Database handler is records-custom") {
			t.Fatal("lost actual handler roster name")
		}
		if role != "handler" && strings.Contains(got, "Own backlog readiness") {
			t.Fatal("handler allocation ownership leaked into another role")
		}
		if role != "lead" && strings.Contains(got, "You are the MAIN ORCHESTRATOR") {
			t.Fatal("lead ownership leaked into another role")
		}
	}
	if requests != 3 {
		t.Fatalf("expected three task GETs, got %d", requests)
	}
	for _, obsolete := range []string{
		"make one explicit choice to tt resume NAME",
		"and tt resume NAME before assigning more same-item work",
		"do not treat exhausted helper allowance as exhausted normal-member capacity",
	} {
		for role, briefing := range briefings {
			if strings.Contains(briefing, obsolete) {
				t.Errorf("%s still emits ambiguous instruction %q", role, obsolete)
			}
		}
	}
	for _, scenario := range scenarios {
		t.Run(scenario.Name, func(t *testing.T) {
			for role, clauses := range map[string][]string{"handler": scenario.Handler, "lead": scenario.Lead, "worker": scenario.Worker} {
				for _, clause := range clauses {
					if !strings.Contains(briefings[role], clause) {
						t.Errorf("%s: %s briefing missing %q", scenario.Situation, role, clause)
					}
				}
			}
		})
	}
}

func TestHandlerAllocationContinuationGuidanceAcrossWorkerStates(t *testing.T) {
	task := api.Task{ID: "tsk_0000000000000001", Name: "Synthetic continuation", Orchestrator: "lead", AllowAgentSpawn: true, MaxNewAgents: 0}
	for _, state := range []struct {
		name, status, required string
		online                 bool
	}{
		{"running", api.AgentRunning, "For available running/done workers, deliver one follow-up", true},
		{"done", api.AgentDone, "verify read and concrete progress; do not call tt resume", true},
		{"retired", api.AgentRetired, "only for an intentionally retained retired same-item worker when authorized; respect explicit owner retirement", true},
		{"exited", api.AgentExited, "authorized supported exact-run launch/recovery handling without changing item identity or retrying unchanged resume failures", false},
		{"offline", api.AgentDone, "For exited/offline workers, report the actual dependency", false},
		{"closed", api.AgentClosed, "Never clear or spoof identity, reuse closed workers, or bypass a denial", false},
	} {
		t.Run(state.name, func(t *testing.T) {
			agents := []api.Agent{{Name: "lead", Status: api.AgentRunning, Online: true}, {Name: "builder", Status: state.status, Online: state.online}}
			got := agentTaskBriefingForLaunch(task, "lead", "", "", agents, 0)
			if !strings.Contains(got, state.required) {
				t.Fatalf("missing state-aware instruction %q", state.required)
			}
			if !strings.Contains(got, "report the real limit") || !strings.Contains(got, "one follow-up tied to the existing order; the broker escalates overdue work itself") {
				t.Fatal("lost capacity or broker-owned overdue escalation instruction")
			}
		})
	}
}
