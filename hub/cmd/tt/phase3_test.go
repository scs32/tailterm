package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/server"
	"github.com/scs32/tailterm/hub/internal/store"
)

// Broker phase 3 (docs/broker-phase-3.md).

// c1, c13: retired writers refuse locally, without calling the hub.
func TestRetiredWritersRefuseLocally(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("hub contacted: %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()
	e := env{hub: srv.URL, task: "tsk_0000000000000001"}
	for name, run := range map[string]func() error{
		"delivery create":            func() error { return cmdDelivery(e, []string{"create", "--file", "x.json"}) },
		"delivery ack":               func() error { return cmdDelivery(e, []string{"ack", "dly_0000000000000000"}) },
		"operational-record propose": func() error { return cmdOperationalRecord(e, []string{"propose", "--file", "x.json"}) },
		"operational-record commit":  func() error { return cmdOperationalRecord(e, []string{"commit", "opr_0000000000000000"}) },
	} {
		if err := run(); !errors.Is(err, errRetiredWriter) {
			t.Errorf("%s: %v, want the retirement error", name, err)
		}
	}
}

// c4 (R2): a directed owner message wakes its recipient once, through the
// broker; a lead-assignment style notice without an obligation still wakes
// through the inbox.
func TestRelayWakesDirectedOwnerMessagesOnce(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	by := api.Caller{Node: "workspace", User: "owner"}
	ctx := context.Background()
	task, _ := st.CreateTask(ctx, api.CreateTaskRequest{Name: "P", Orchestrator: "lead"}, by)
	worker, _ := st.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "worker", Host: "h", Session: "worker", Runtime: "codex"}, by)
	srv := httptest.NewServer(server.New(st, func(*http.Request) (api.Caller, error) { return by, nil }))
	t.Cleanup(srv.Close)
	c, _ := api.NewClient(srv.URL, 0)
	if _, err := st.PostEvent(ctx, task.ID, api.PostEventRequest{Kind: api.EventHeartbeat, AgentID: worker.ID, RunID: worker.RunID}, by); err != nil {
		t.Fatal(err)
	}
	b := runtimeBinding{Hub: srv.URL, Task: task.ID, Agent: worker.ID, Run: worker.RunID, Thread: "00000000-0000-0000-0000-000000000000", Codex: "/bin/codex"}
	inbox := func() bool {
		t.Helper()
		p := &relayProgress{Run: worker.RunID, Thread: b.Thread, BrokerWakes: true}
		called := false
		if err := relayOne(ctx, b, p, c, time.Now(), func(context.Context, runtimeBinding, string) error { called = true; return nil }); err != nil {
			t.Fatal(err)
		}
		return called
	}
	if _, err := st.PostMessage(ctx, task.ID, api.PostMessageRequest{To: worker.ID, Text: "owner: please rebase"}, by); err != nil {
		t.Fatal(err)
	}
	if inbox() {
		t.Fatal("the inbox woke for an owner message the broker already wakes for (R2)")
	}
	queued := 0
	if _, err := relayWakeJob(ctx, b, &relayProgress{}, c, time.Now(), func(context.Context, runtimeBinding, string) error { queued++; return nil }); err != nil || queued != 1 {
		t.Fatalf("the broker did not wake for the owner message: %d %v", queued, err)
	}
	// Owner free text posted without an obligation (as a lead assignment
	// notice is) must still wake through the inbox.
	if err := st.PostSystemTextForTest(ctx, task.ID, worker.ID, "Owner assigned you as project lead."); err != nil {
		t.Fatal(err)
	}
	if !inbox() {
		t.Fatal("a directed owner notice without an obligation no longer wakes the agent")
	}
}

// b1 (round one): the legacy reads still reach a phase-3 hub.
func TestLegacyReadsStillWork(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	by := api.Caller{Node: "workspace", User: "owner"}
	ctx := context.Background()
	task, _ := st.CreateTask(ctx, api.CreateTaskRequest{Name: "P", Orchestrator: "lead"}, by)
	worker, _ := st.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "worker", Host: "h", Session: "worker", Runtime: "codex"}, by)
	hub := server.New(st, func(*http.Request) (api.Caller, error) { return by, nil })
	reached := map[string]bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached[r.URL.Path] = true
		hub.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	e := env{hub: srv.URL, task: task.ID, agent: worker.ID, runID: worker.RunID}
	_ = cmdCurrentAssignment(e, nil)
	_ = cmdDelivery(e, []string{"coverage"})
	_ = cmdOperationalRecord(env{hub: srv.URL, task: task.ID}, []string{"get", "opr_0000000000000000"})
	for _, path := range []string{
		"/v1/tasks/" + task.ID + "/agents/" + worker.ID + "/current-assignment",
		"/v1/tasks/" + task.ID + "/agents/" + worker.ID + "/delivery-coverage",
		"/v1/tasks/" + task.ID + "/operational-records/opr_0000000000000000",
	} {
		if !reached[path] {
			t.Errorf("the read never reached the hub: %s (reached %v)", path, reached)
		}
	}
}

// Round one F7: a fresh binding's first broker wake does not let the inbox
// path wake the same message again.
func TestFreshBindingDoesNotDoubleWake(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	by := api.Caller{Node: "workspace", User: "owner"}
	ctx := context.Background()
	task, _ := st.CreateTask(ctx, api.CreateTaskRequest{Name: "P", Orchestrator: "lead"}, by)
	worker, _ := st.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "worker", Host: "h", Session: "worker", Runtime: "codex"}, by)
	srv := httptest.NewServer(server.New(st, func(*http.Request) (api.Caller, error) { return by, nil }))
	t.Cleanup(srv.Close)
	c, _ := api.NewClient(srv.URL, 0)
	if _, err := st.PostEvent(ctx, task.ID, api.PostEventRequest{Kind: api.EventHeartbeat, AgentID: worker.ID, RunID: worker.RunID}, by); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PostMessage(ctx, task.ID, api.PostMessageRequest{To: worker.ID, Text: "owner: please rebase"}, by); err != nil {
		t.Fatal(err)
	}
	b := runtimeBinding{Hub: srv.URL, Task: task.ID, Agent: worker.ID, Run: worker.RunID, Thread: "00000000-0000-0000-0000-000000000000", Codex: "/bin/codex"}
	p := &relayProgress{} // a brand-new progress record, as after install
	wakes := 0
	queue := func(context.Context, runtimeBinding, string) error { wakes++; return nil }
	if _, err := relayWakeJob(ctx, b, p, c, time.Now(), queue); err != nil {
		t.Fatal(err)
	}
	if err := relayOne(ctx, b, p, c, time.Now(), queue); err != nil {
		t.Fatal(err)
	}
	if wakes != 1 {
		t.Fatalf("%d wakes for one owner message, want 1", wakes)
	}
}
