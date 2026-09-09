package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/scs32/tailterm/hub/internal/api"
)

func askFixture() askFileInput {
	return askFileInput{DecisionRequest: api.DecisionRequest{
		Question: "Which approach? Quotes: ' \" and $(literal)",
		Options: []api.DecisionOption{
			{ID: "staged", Label: "Staged rollout", Description: "Limit exposure.\nCheck before expanding."},
			{ID: "all", Label: "All at once", Description: "Faster, but affects everyone."},
		},
		RecommendedOptionID: "staged", RecommendationReason: "Easier rollback.",
	}}
}

func askFixtureFile(t *testing.T, input any) string {
	t.Helper()
	data, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "decision.json")
	if err := os.WriteFile(file, data, 0600); err != nil {
		t.Fatal(err)
	}
	return file
}

func TestAskRejectsBadInputWithoutContactingHub(t *testing.T) {
	var calls atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "unexpected network call", 500)
	}))
	defer s.Close()
	e := env{hub: s.URL, task: "tsk_0000000000000001", agent: "agt_0000000000000001"}
	file := askFixtureFile(t, askFixture())
	if err := cmdAsk(e, []string{"--help"}); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"--unknown"}, {"--file", file}, {"--request-id", "bad/key", "--file", file},
		{"--request-id", "key", "--file", file, "extra"},
		{"--request-id", "key", "--file", file, "--task", "tsk_0000000000000002"},
	} {
		if err := cmdAsk(e, args); err == nil {
			t.Fatalf("accepted malformed flags: %v", args)
		}
	}
	withoutAgent := e
	withoutAgent.agent = ""
	if err := cmdAsk(withoutAgent, []string{"--request-id", "key", "--file", file}); err == nil {
		t.Fatal("accepted missing worker identity")
	}
	var spoof map[string]any
	data, _ := json.Marshal(askFixture())
	_ = json.Unmarshal(data, &spoof)
	spoof["agentId"] = "agt_0000000000000002"
	if err := cmdAsk(e, []string{"--request-id", "key", "--file", askFixtureFile(t, spoof)}); err == nil {
		t.Fatal("accepted identity from file")
	}
	for _, mutate := range []func(*askFileInput){
		func(in *askFileInput) { in.Question = " \n " },
		func(in *askFileInput) { in.Options = in.Options[:1] },
		func(in *askFileInput) { in.Options[1].ID = in.Options[0].ID },
		func(in *askFileInput) { in.Options[0].Description = "" },
		func(in *askFileInput) { in.RecommendedOptionID = "missing" },
		func(in *askFileInput) { in.RecommendationReason = "" },
		func(in *askFileInput) { in.Question = strings.Repeat("x", 2001) },
		func(in *askFileInput) { in.WorkOrderMessage = &api.MessageReference{TaskID: e.task, Seq: 1} },
	} {
		input := askFixture()
		mutate(&input)
		if err := cmdAsk(e, []string{"--request-id", "key", "--file", askFixtureFile(t, input)}); err == nil {
			t.Fatal("accepted invalid decision input")
		}
	}
	if _, err := decodeAskFile(append(data, []byte(" {}")...)); err == nil {
		t.Fatal("accepted trailing JSON")
	}
	if calls.Load() != 0 {
		t.Fatalf("invalid input contacted hub %d times", calls.Load())
	}
}

func TestAskPreservesIdentityContextAndReplayPayload(t *testing.T) {
	requests := make(chan api.CreateDecisionRequest, 3)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/v1/tasks/tsk_0000000000000001/decisions" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		var input api.CreateDecisionRequest
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
		}
		requests <- input
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(201)
		_ = json.NewEncoder(w).Encode(api.Message{Seq: 7, DecisionRequest: &input.DecisionRequest})
	}))
	defer s.Close()
	e := env{hub: s.URL, task: "tsk_0000000000000001", agent: "agt_0000000000000001"}
	input := askFixture()
	input.WorkItems = []api.MessageWorkItem{{ItemTaskID: e.task, ItemID: "wi_0000000000000001", ItemRevision: 3, Relationship: "primary"}}
	input.WorkOrderMessage = &api.MessageReference{TaskID: e.task, Seq: 2}
	file := askFixtureFile(t, input)
	args := []string{"--request-id", "decision:stable.1", "--file", file}
	for i := 0; i < 2; i++ {
		if err := cmdAsk(e, args); err != nil {
			t.Fatal(err)
		}
	}
	first, second := <-requests, <-requests
	if !reflect.DeepEqual(first, second) || first.RequestID != "decision:stable.1" || first.AgentID != e.agent || !reflect.DeepEqual(first.DecisionRequest, input.DecisionRequest) || !reflect.DeepEqual(first.WorkItems, input.WorkItems) || *first.WorkOrderMessage != *input.WorkOrderMessage {
		t.Fatal("CLI changed retry intent, question/context or sender identity")
	}
	stdin, err := os.Open(file)
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	previous := os.Stdin
	os.Stdin = stdin
	defer func() { os.Stdin = previous }()
	if err := cmdAsk(e, []string{"--request-id", "decision:stable.1", "--file", "-", "--json"}); err != nil {
		t.Fatal(err)
	}
	if third := <-requests; !reflect.DeepEqual(first, third) {
		t.Fatal("stdin and file commands differ")
	}
}

func TestBoundAskInheritsExactSessionItemContext(t *testing.T) {
	binding := &api.AgentWorkItemBinding{
		ItemTaskID: "tsk_0000000000000001", ItemID: "wi_0000000000000002", ItemRevision: 4,
		WorkOrderMessage: api.MessageReference{TaskID: "tsk_0000000000000001", Seq: 814},
	}
	var posted api.CreateDecisionRequest
	posts := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/agents/agt_0000000000000001"):
			_ = json.NewEncoder(w).Encode(api.Agent{ID: "agt_0000000000000001", WorkItem: binding})
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/decisions"):
			posts++
			if err := json.NewDecoder(r.Body).Decode(&posted); err != nil {
				t.Error(err)
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(api.Message{Seq: 9})
		default:
			http.Error(w, "unexpected route", http.StatusInternalServerError)
		}
	}))
	defer s.Close()
	t.Setenv("TAILTERM_WORK_ITEM", binding.ItemID)
	e := env{hub: s.URL, task: binding.ItemTaskID, agent: "agt_0000000000000001", runID: "run_0000000000000003"}
	if err := cmdAsk(e, []string{"--request-id", "bound-decision", "--file", askFixtureFile(t, askFixture())}); err != nil {
		t.Fatal(err)
	}
	wantItem := api.MessageWorkItem{ItemTaskID: binding.ItemTaskID, ItemID: binding.ItemID, ItemRevision: binding.ItemRevision, Relationship: "primary"}
	if len(posted.WorkItems) != 1 || posted.WorkItems[0] != wantItem || posted.WorkOrderMessage == nil || *posted.WorkOrderMessage != binding.WorkOrderMessage {
		t.Fatalf("bound decision lost inherited context: %+v", posted)
	}
	mismatched := askFixture()
	mismatched.WorkItems = []api.MessageWorkItem{{ItemTaskID: binding.ItemTaskID, ItemID: "wi_0000000000000003", ItemRevision: binding.ItemRevision, Relationship: "primary"}}
	mismatched.WorkOrderMessage = &binding.WorkOrderMessage
	if err := cmdAsk(e, []string{"--request-id", "wrong-bound-decision", "--file", askFixtureFile(t, mismatched)}); err == nil || posts != 1 {
		t.Fatalf("bound decision accepted a different item or posted it: posts=%d err=%v", posts, err)
	}
}

func TestAskAmbiguousFailureRetainsRecoveryKey(t *testing.T) {
	var calls atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "temporary failure", http.StatusServiceUnavailable)
	}))
	defer s.Close()
	e := env{hub: s.URL, task: "tsk_0000000000000001", agent: "agt_0000000000000001"}
	err := cmdAsk(e, []string{"--request-id", "retain:this.1", "--file", askFixtureFile(t, askFixture())})
	if err == nil || !strings.Contains(err.Error(), "--request-id retain:this.1") || !strings.Contains(err.Error(), "unchanged file") || calls.Load() != 1 {
		t.Fatalf("uncertain response lost retry identity or reposted: calls=%d err=%v", calls.Load(), err)
	}
}
