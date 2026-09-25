package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/server"
	"github.com/scs32/tailterm/hub/internal/store"
)

func TestReplyWakeFromStoredRecipient(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	by := api.Caller{Node: "workspace", User: "owner"}
	task, err := st.CreateTask(ctx, api.CreateTaskRequest{Name: "Reply wake", Orchestrator: "lead"}, by)
	if err != nil {
		t.Fatal(err)
	}
	add := func(name string) api.Agent {
		a, err := st.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: name, Host: "h", Session: name, Runtime: "codex"}, by)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	lead, handler := add("lead"), add("handler")
	for _, a := range []api.Agent{lead, handler} {
		if _, err := st.PostEvent(ctx, task.ID, api.PostEventRequest{AgentID: a.ID, RunID: a.RunID, Kind: api.EventHeartbeat}, by); err != nil {
			t.Fatal(err)
		}
	}
	// The lead waits after asking the handler; its read cursor covers the
	// request. The handler answers board-wide, exactly like #9678/#9680.
	request, err := st.PostMessage(ctx, task.ID, api.PostMessageRequest{AgentID: lead.ID, RunID: lead.RunID, To: handler.ID, Text: "Record the correction order"}, by)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.MarkRead(ctx, task.ID, api.MarkReadRequest{AgentID: lead.ID, UpTo: request.Seq}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PostEvent(ctx, task.ID, api.PostEventRequest{AgentID: lead.ID, RunID: lead.RunID, Kind: api.EventDone}, by); err != nil {
		t.Fatal(err)
	}
	reply, err := st.PostMessage(ctx, task.ID, api.PostMessageRequest{AgentID: handler.ID, RunID: handler.RunID, ReplyTo: request.Seq, Text: "Correction order recorded"}, by)
	if err != nil || reply.To != lead.ID {
		t.Fatalf("reply routing: %+v %v", reply, err)
	}
	httpServer := httptest.NewServer(server.New(st, func(*http.Request) (api.Caller, error) { return by, nil }))
	t.Cleanup(httpServer.Close)
	client, err := api.NewClient(httpServer.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	binding := runtimeBinding{Hub: httpServer.URL, Task: task.ID, Agent: lead.ID, Run: lead.RunID, Thread: "00000000-0000-4000-8000-000000000001", Codex: "/bin/codex"}
	progress := relayProgress{Run: lead.RunID, Thread: binding.Thread, BrokerWakes: true}
	wakes := 0
	queue := func(_ context.Context, b runtimeBinding, prompt string) error {
		wakes++
		if b != binding || !strings.Contains(prompt, "through message #"+strconv.FormatInt(reply.Seq, 10)) {
			t.Fatalf("wrong wake binding/prompt: %+v %q", b, prompt)
		}
		return nil
	}
	now := time.Now()
	if err := relayOne(ctx, binding, &progress, client, now, queue); err != nil {
		t.Fatal(err)
	}
	if wakes != 1 || progress.Through != reply.Seq {
		t.Fatalf("idle sender delivery: wakes=%d progress=%+v", wakes, progress)
	}
	// Persist and reload the progress while the inbox remains unread.
	raw, _ := json.Marshal(progress)
	progress = relayProgress{}
	if err := json.Unmarshal(raw, &progress); err != nil {
		t.Fatal(err)
	}
	if err := relayOne(ctx, binding, &progress, client, now.Add(time.Minute), queue); err != nil || wakes != 1 {
		t.Fatalf("repeated wake: wakes=%d err=%v", wakes, err)
	}
	after, err := st.GetAgent(ctx, lead.ID)
	if err != nil || after.ReadUpTo != request.Seq {
		t.Fatalf("relay changed read cursor: %+v %v", after, err)
	}
	// A typed question in the same reply shape creates an obligation. Its
	// broker wake is the only wake for that message (phase-3 R2).
	question, err := st.PostMessage(ctx, task.ID, api.PostMessageRequest{AgentID: handler.ID, RunID: handler.RunID, ReplyTo: request.Seq,
		Envelope: &api.Envelope{Kind: api.EnvelopeKindQuestion, Subject: "Choose the correction fixture", Body: api.EnvelopeBody{Question: "Use the saved fixture?"}}}, by)
	if err != nil || question.To != lead.ID {
		t.Fatalf("question routing: %+v %v", question, err)
	}
	if err := relayOne(ctx, binding, &progress, client, now.Add(2*time.Minute), queue); err != nil || wakes != 1 || progress.Through != question.Seq {
		t.Fatalf("broker-covered reply woke through inbox: wakes=%d progress=%+v err=%v", wakes, progress, err)
	}
	brokerWakes := 0
	brokerQueue := func(_ context.Context, got runtimeBinding, _ string) error {
		brokerWakes++
		if got != binding {
			t.Fatalf("broker woke wrong binding: %+v", got)
		}
		return nil
	}
	if handled, err := relayWakeJob(ctx, binding, &progress, client, now.Add(2*time.Minute), brokerQueue); err != nil || !handled || brokerWakes != 1 {
		t.Fatalf("broker did not wake once: handled=%v wakes=%d err=%v", handled, brokerWakes, err)
	}
	if handled, err := relayWakeJob(ctx, binding, &progress, client, now.Add(3*time.Minute), brokerQueue); err != nil || handled || brokerWakes != 1 {
		t.Fatalf("broker repeated wake: handled=%v wakes=%d err=%v", handled, brokerWakes, err)
	}
}

func TestReplyWakeEligibilityPreservesExistingPaths(t *testing.T) {
	for _, tc := range []struct {
		name string
		msg  api.Message
		want bool
	}{
		{"stored agent reply", api.Message{To: "lead", From: api.Sender{AgentID: "handler"}, ReplyTo: 1}, true},
		{"alternate recipient", api.Message{To: "other", From: api.Sender{AgentID: "handler"}, ReplyTo: 1}, false},
		{"person reply board-wide", api.Message{From: api.Sender{AgentID: "handler"}, ReplyTo: 1}, false},
		{"human board post", api.Message{From: api.Sender{}}, true},
		{"swarm reply", api.Message{From: api.Sender{AgentID: "handler"}, Broadcast: true, ReplyTo: 1}, true},
		{"self post", api.Message{To: "lead", From: api.Sender{AgentID: "lead"}, ReplyTo: 1}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, got := wakeThrough([]api.Message{tc.msg}, "lead"); got != tc.want {
				t.Fatalf("wakeThrough(%+v) = %v, want %v", tc.msg, got, tc.want)
			}
		})
	}
}
