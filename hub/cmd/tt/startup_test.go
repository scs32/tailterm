package main

import (
	"context"
	"encoding/json"
	"github.com/scs32/tailterm/hub/internal/api"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestStartupTrustRecognition(t *testing.T) {
	prompt := "Do you trust the contents of this directory?\n1. Yes, continue\n2. No, quit\nPress enter to continue"
	if !codexTrustPrompt(prompt) {
		t.Fatal("missed observed startup prompt")
	}
	for _, text := range []string{"", "Do you trust the contents of this directory?", "1. Yes, continue\n2. No, quit", "Press enter to continue"} {
		if codexTrustPrompt(text) {
			t.Fatal("ordinary output classified as trust prompt")
		}
	}
}
func TestStartupBlockerChecksRunAndDeduplicates(t *testing.T) {
	a := api.Agent{ID: "agt_0123456789abcdef", RunID: "run_0123456789abcdef", Session: "orchestrator", Runtime: "codex", Status: api.AgentRunning}
	posts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			json.NewEncoder(w).Encode(a)
			return
		}
		var req api.PostEventRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		if req.Kind != api.EventNeedsInput || req.Data["reason"] != "permission" || req.RunID != a.RunID || req.Text != trustBlocker {
			t.Errorf("bad blocker %+v", req)
		}
		posts++
		a.Status = api.AgentNeedsInput
		a.BlockedText = req.Text
		json.NewEncoder(w).Encode(api.Event{})
	}))
	defer server.Close()
	c, _ := api.NewClient(server.URL, time.Second)
	report := func(run, session string) {
		t.Helper()
		if err := reportTrustBlocker(context.Background(), c, "tsk_0123456789abcdef", a.ID, run, session); err != nil {
			t.Fatal(err)
		}
	}
	report("run_ffffffffffffffff", a.Session)
	report(a.RunID, "another-session")
	if posts != 0 {
		t.Fatal("reported wrong run/session")
	}
	report(a.RunID, a.Session)
	report(a.RunID, a.Session)
	if posts != 1 {
		t.Fatal("missing or duplicate event", posts)
	}
	a.Status = api.AgentExited
	report(a.RunID, a.Session)
	if posts != 1 {
		t.Fatal("reported exited agent")
	}
}
