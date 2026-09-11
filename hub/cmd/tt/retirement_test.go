package main

import (
	"encoding/json"
	"github.com/scs32/tailterm/hub/internal/api"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRetireAndResumeRemoteAgent(t *testing.T) {
	a := api.Agent{ID: "agt_0123456789abcdef", Name: "worker", Host: "different-machine", Session: "worker", Status: api.AgentDone}
	writes := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PATCH" {
			var req api.UpdateAgentRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Error(err)
			}
			if req.Status == nil {
				t.Fatal("status missing")
			}
			a.Status = *req.Status
			writes++
		}
		json.NewEncoder(w).Encode(a)
	}))
	defer server.Close()
	e := env{hub: server.URL, task: "tsk_0123456789abcdef", agent: a.ID}
	for _, op := range []string{"retire", "retire", "resume"} {
		if err := cmdRetirement(e, op, nil); err != nil {
			t.Fatal(op, err)
		}
	}
	if writes != 2 || a.Status != api.AgentDone {
		t.Fatal("retirement not idempotent or resume failed", writes, a)
	}
	if err := cmdRetirement(e, "resume", nil); err == nil {
		t.Fatal("resumed non-retired member")
	}
}
func TestBriefingClosesAcceptedWorkersAndPreservesIntentionalRetirement(t *testing.T) {
	task := api.Task{ID: "tsk_0123456789abcdef", Name: "Task", Orchestrator: "lead"}
	lead := taskBriefing(task, "lead", "")
	for _, want := range []string{"tt close NAME", "tt retire NAME only for intentional temporary retention", "fresh session and identity for a new item", "leave yourself available for the owner"} {
		if !strings.Contains(lead, want) {
			t.Fatal("missing closeout policy", want)
		}
	}
	worker := taskBriefing(task, "worker", "")
	for _, want := range []string{"If retired, finish quietly", "Workers must post results", "run tt close for yourself", "intentional temporary retention", "dedicated to exactly one bug or feature"} {
		if !strings.Contains(worker, want) {
			t.Fatal("worker closeout policy missing", want)
		}
	}
}
