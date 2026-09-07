package main

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/scs32/tailterm/hub/internal/api"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestRelayTargetsUnreadOnceAndPreservesReadReceipts(t *testing.T) {
	b := runtimeBinding{Hub: "http://hub", Task: "tsk_0000000000000001", Agent: "agt_0000000000000001", Run: "run_0000000000000001", Thread: "00000000-0000-4000-8000-000000000001", Codex: "/usr/local/bin/codex"}
	a := api.Agent{ID: b.Agent, RunID: b.Run, Status: api.AgentDone, Online: true, Unread: 1}
	messages := []api.Message{{Seq: 8, To: b.Agent, From: api.Sender{AgentID: "agt_0000000000000002"}, Text: "$(touch /tmp/should-never-run)"}}
	reads := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Errorf("relay must not mutate hub/read receipts: %s", r.Method)
			http.Error(w, "unexpected", 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/messages") {
			after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
			var result []api.Message
			for _, m := range messages {
				if m.Seq > after {
					result = append(result, m)
				}
			}
			json.NewEncoder(w).Encode(api.MessageList{Messages: result})
		} else {
			reads++
			json.NewEncoder(w).Encode(a)
		}
	}))
	defer server.Close()
	b.Hub = server.URL
	c, _ := api.NewClient(server.URL, time.Second)
	p := relayProgress{}
	now := time.Now()
	calls := 0
	queue := func(ctx context.Context, got runtimeBinding, prompt string) error {
		calls++
		if got.Thread != b.Thread || !strings.Contains(prompt, "through message #8") || strings.Contains(prompt, "touch /tmp") {
			t.Fatalf("misdirected/raw payload: %+v %s", got, prompt)
		}
		return nil
	}
	if err := relayOne(context.Background(), b, &p, c, now, queue); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || p.Through != 8 || a.ReadUpTo != 0 {
		t.Fatalf("delivery: %d %+v", calls, p)
	}
	// Persist/reload progress while the same message remains unread.
	raw, _ := json.Marshal(p)
	p = relayProgress{}
	json.Unmarshal(raw, &p)
	if err := relayOne(context.Background(), b, &p, c, now.Add(time.Minute), queue); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("same unread message woke twice")
	}
	for _, status := range []string{api.AgentClosed, api.AgentExited} {
		a.Status = status
		p.Through = 0
		relayOne(context.Background(), b, &p, c, now.Add(time.Minute), queue)
	}
	a.Status = api.AgentDone
	a.RunID = "run_0000000000000002"
	relayOne(context.Background(), b, &p, c, now.Add(time.Minute), queue)
	a.RunID = b.Run
	a.Online = false
	relayOne(context.Background(), b, &p, c, now.Add(time.Minute), queue)
	if calls != 1 || reads == 0 {
		t.Fatal("inactive/stale agent was resumed")
	}
	a.Online = true
	failed := func(context.Context, runtimeBinding, string) error { return errors.New("offline") }
	if err := relayOne(context.Background(), b, &p, c, now.Add(2*time.Minute), failed); err == nil || p.Through != 0 {
		t.Fatal("failed queue advanced cursor")
	}
	if err := relayOne(context.Background(), b, &p, c, now.Add(3*time.Minute), queue); err != nil || calls != 2 {
		t.Fatalf("retry: %v %d", err, calls)
	}
}
func TestWakeEligibility(t *testing.T) {
	for _, tc := range []struct {
		from, to string
		want     bool
	}{
		{"other", "self", true}, {"", "self", true}, {"", "", true}, {"other", "", false}, {"self", "self", false}, {"other", "someone-else", false},
	} {
		through, wake := wakeThrough([]api.Message{{Seq: 10, From: api.Sender{AgentID: tc.from}, To: tc.to}}, "self")
		if through != 10 || wake != tc.want {
			t.Fatalf("%+v -> %d %v", tc, through, wake)
		}
	}
}
