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

func TestQueueCLIUsesTypedRoutesAndExactCurrentAgentRun(t *testing.T) {
	const task = "tsk_0000000000000001"
	const entry = "que_0000000000000002"
	var action api.QueueActionRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "GET" && r.URL.Path == "/v1/tasks/"+task+"/agents/agt_0000000000000003":
			_ = json.NewEncoder(w).Encode(api.Agent{ID: "agt_0000000000000003", TaskID: task, Role: api.AgentRoleDatabaseHandler, RunID: "run_0000000000000004", Status: api.AgentRunning})
		case r.Method == "GET" && r.URL.Path == "/v1/tasks/"+task+"/queue":
			_ = json.NewEncoder(w).Encode(api.QueueList{Entries: []api.QueueEntry{{ID: entry, TargetTaskID: task, State: api.QueueStateWaiting, QueuePriority: "normal", Cycle: 1, Revision: 1, OfferedItemRevision: 2, Item: api.QueueItemSummary{Title: "Synthetic Queue"}}}, Complete: true})
		case r.Method == "POST" && r.URL.Path == "/v1/tasks/"+task+"/queue/"+entry+"/actions":
			if err := json.NewDecoder(r.Body).Decode(&action); err != nil {
				t.Error(err)
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(api.QueueActionResult{Entry: api.QueueEntry{ID: entry, State: api.QueueStateClaimed, Cycle: 1, Revision: 2}, Receipt: api.QueueReceipt{ID: "qrr_synthetic"}})
		default:
			http.Error(w, "unexpected route", http.StatusInternalServerError)
		}
	}))
	defer server.Close()
	e := env{hub: server.URL, task: task, agent: "agt_0000000000000003", runID: "run_0000000000000004"}
	out, err := captureCLIOutput(t, func() error { return cmdQueue(e, []string{"list", "--limit", "17"}) })
	if err != nil || !strings.Contains(out, "Synthetic Queue") {
		t.Fatalf("list output=%q err=%v", out, err)
	}
	file := filepath.Join(t.TempDir(), "action.json")
	body := `{"operation":"claim","requestId":"queue-cli-action","expectedRevision":1,"cycle":1,"expectedItemRevision":2,"selection":{"taskId":"tsk_0000000000000001","messageSeq":7},"claimantAgentId":"agt_0000000000000005","claimantRunId":"run_0000000000000006","workOrderMessage":{"taskId":"tsk_0000000000000001","seq":8}}`
	if err = os.WriteFile(file, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = captureCLIOutput(t, func() error { return cmdQueue(e, []string{"action", "--file", file, entry}) }); err != nil {
		t.Fatal(err)
	}
	if action.AgentID != e.agent || action.RunID != e.runID || action.Operation != "claim" || action.RequestID != "queue-cli-action" {
		t.Fatalf("action identity/payload: %+v", action)
	}
	if _, err = queueProject(e, "tsk_0000000000000009"); err == nil {
		t.Fatal("agent CLI crossed project Queue boundary")
	}
}

func TestQueueCLIRejectsStaleExplicitActionIdentityBeforeMutation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Fatal("stale identity reached mutation HTTP")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(api.Agent{ID: "agt_0000000000000002", TaskID: "tsk_0000000000000001", Role: api.AgentRoleDatabaseHandler, RunID: "run_0000000000000003", Status: api.AgentRunning})
	}))
	defer server.Close()
	e := env{hub: server.URL, task: "tsk_0000000000000001", agent: "agt_0000000000000002", runID: "run_0000000000000003"}
	file := filepath.Join(t.TempDir(), "action.json")
	if err := os.WriteFile(file, []byte(`{"operation":"priority","requestId":"stale","expectedRevision":1,"cycle":1,"priority":"high","agentId":"agt_0000000000000002","runId":"run_0000000000000004"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := cmdQueue(e, []string{"action", "--file", file, "que_0000000000000004"}); err == nil || !strings.Contains(err.Error(), "exact run") {
		t.Fatalf("stale run error=%v", err)
	}
}

func TestQueueCLIRejectsOrdinaryAgentReads(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(api.Agent{ID: "agt_0000000000000002", TaskID: "tsk_0000000000000001", RunID: "run_0000000000000003", Status: api.AgentRunning})
	}))
	defer server.Close()
	e := env{hub: server.URL, task: "tsk_0000000000000001", agent: "agt_0000000000000002", runID: "run_0000000000000003"}
	if err := cmdQueue(e, []string{"list"}); err == nil || !strings.Contains(err.Error(), "database-handler") {
		t.Fatalf("ordinary agent Queue read error=%v", err)
	}
}
