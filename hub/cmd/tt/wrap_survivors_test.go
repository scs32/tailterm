package main

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/server"
	"github.com/scs32/tailterm/hub/internal/spawn"
	"github.com/scs32/tailterm/hub/internal/store"
)

// wrapSurvivorHub is an isolated in-memory hub with one started agent, the
// wrapper's env for it, and a worker that can send it requests.
type wrapSurvivorHub struct {
	c             *api.Client
	dbPath        string
	task          api.Task
	agent, worker api.Agent
	e             env
}

func newWrapSurvivorHub(t *testing.T) *wrapSurvivorHub {
	t.Helper()
	t.Setenv("TAILTERM_RELAY_STATE", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	h := &wrapSurvivorHub{dbPath: filepath.Join(t.TempDir(), "hub.sqlite")}
	st, err := store.Open(h.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	srv := httptest.NewServer(server.New(st, func(*http.Request) (api.Caller, error) { return api.Caller{Node: "fixture", User: "owner"}, nil }))
	t.Cleanup(srv.Close)
	if h.c, err = api.NewClient(srv.URL, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if h.task, err = h.c.CreateTask(ctx, api.CreateTaskRequest{Name: "isolated wrap survivor test"}); err != nil {
		t.Fatal(err)
	}
	for _, a := range []*api.Agent{&h.agent, &h.worker} {
		name := "runtime"
		if a == &h.worker {
			name = "sender"
		}
		if *a, err = h.c.AddAgent(ctx, h.task.ID, api.AddAgentRequest{Name: name, Host: "fixture", Session: name, Runtime: "generic"}); err != nil {
			t.Fatal(err)
		}
		if _, err := h.c.PostEvent(ctx, h.task.ID, api.PostEventRequest{AgentID: a.ID, RunID: a.RunID, Kind: api.EventStarted}); err != nil {
			t.Fatal(err)
		}
	}
	h.e = env{hub: h.c.Base, task: h.task.ID, agent: h.agent.ID, agentName: h.agent.Name, runID: h.agent.RunID}
	return h
}

// exits returns the agent's exited events.
func (h *wrapSurvivorHub) exits(t *testing.T) []api.Event {
	t.Helper()
	events, err := h.c.Events(context.Background(), h.task.ID, 0, 0, 500)
	if err != nil {
		t.Fatal(err)
	}
	var out []api.Event
	for _, ev := range events.Events {
		if ev.AgentID == h.agent.ID && ev.Kind == api.EventExited {
			out = append(out, ev)
		}
	}
	return out
}

// notices returns the Board's survivor notices.
func (h *wrapSurvivorHub) notices(t *testing.T) []api.Message {
	t.Helper()
	msgs, err := h.c.ListMessages(context.Background(), h.task.ID, 0, "", 500)
	if err != nil {
		t.Fatal(err)
	}
	var out []api.Message
	for _, m := range msgs {
		if m.Envelope != nil && m.Envelope.Kind == api.EnvelopeKindNotice && strings.Contains(m.Envelope.Subject, "left processes running") {
			out = append(out, m)
		}
	}
	return out
}

// k3: an exit with no survivors posts the unchanged report and no notice.
func TestWrapExitWithoutSurvivorsIsUnchanged(t *testing.T) {
	h := newWrapSurvivorHub(t)
	if wrapperExited(context.Background(), h.e, 0, productionExitReporter(h.e)) {
		t.Fatal("a delivered exit report started a retry")
	}
	exits := h.exits(t)
	if len(exits) != 1 || exits[0].Text != "Process exited (0)" || exits[0].Data != nil {
		t.Fatalf("exit reports %+v, want one plain Process exited (0)", exits)
	}
	if n := len(h.notices(t)); n != 0 {
		t.Fatalf("%d survivor notices without survivors", n)
	}
}

// k3: survivors are named by pid and command in the exit report's data,
// bounded, with the report text unchanged, and in one Board notice.
func TestWrapExitReportsSurvivorsInDataAndOneNotice(t *testing.T) {
	h := newWrapSurvivorHub(t)
	var survivors []spawn.Survivor
	for i := range 12 {
		survivors = append(survivors, spawn.Survivor{PID: 7000 + i, Command: fmt.Sprintf("tool%d", i)})
	}
	wrapperExited(context.Background(), h.e, 143, productionExitReporter(h.e), survivors...)
	exits := h.exits(t)
	if len(exits) != 1 || exits[0].Text != "Process exited (143)" {
		t.Fatalf("exit reports %+v, want one with the text the exited-primary rule matches", exits)
	}
	data := exits[0].Data
	list, _ := data["survivors"].([]any)
	if len(list) != wrapSurvivorLimit || data["survivorsOmitted"] != float64(2) {
		t.Fatalf("exit data %v, want %d survivors and 2 omitted", data, wrapSurvivorLimit)
	}
	for i, entry := range list {
		s, _ := entry.(map[string]any)
		if len(s) != 2 || s["pid"] != float64(7000+i) || s["command"] != fmt.Sprintf("tool%d", i) {
			t.Fatalf("survivor %d is %v, want only pid and command", i, entry)
		}
	}
	notices := h.notices(t)
	if len(notices) != 1 {
		t.Fatalf("%d survivor notices, want one", len(notices))
	}
	n := notices[0]
	if n.From.AgentID != "" || n.To != "" {
		t.Fatalf("notice from %+v to %q, want an undirected post without the agent's identity", n.From, n.To)
	}
	text := n.Envelope.Body.Text
	for i := range wrapSurvivorLimit {
		if !strings.Contains(text, fmt.Sprintf("pid %d tool%d", 7000+i, i)) {
			t.Fatalf("notice does not name pid %d: %q", 7000+i, text)
		}
	}
	if strings.Contains(text, "tool10") || strings.Contains(text, "tool11") || !strings.Contains(text, "and 2 more") || !strings.Contains(text, h.agent.Name) {
		t.Fatalf("notice text %q, want the agent, ten survivors and 2 more", text)
	}
	if n.Envelope.Refs["agent"] != h.agent.ID || n.Envelope.Refs["run"] != h.agent.RunID {
		t.Fatalf("notice refs %v", n.Envelope.Refs)
	}
	// A repost for the same run returns the stored notice.
	again, err := postSurvivorNotice(h.e, survivors)
	if err != nil || again.Seq != n.Seq || len(h.notices(t)) != 1 {
		t.Fatalf("repost seq %d (%v), want the stored %d and still one notice", again.Seq, err, n.Seq)
	}
}

// k3: an agent with unacknowledged work cannot post, but the notice about
// its survivors still reaches the Board.
func TestWrapSurvivorNoticeIgnoresTheAgentsAckGate(t *testing.T) {
	h := newWrapSurvivorHub(t)
	ctx := context.Background()
	req := api.Envelope{Kind: api.EnvelopeKindRequest, To: h.agent.Name, Subject: "Synthetic request the stopped agent never acknowledged", Body: api.EnvelopeBody{Ask: "Please record this synthetic fixture item."}}
	m, err := h.c.PostMessage(ctx, h.task.ID, api.PostMessageRequest{Envelope: &req, Text: api.RenderText(req), To: h.agent.ID, AgentID: h.worker.ID, RunID: h.worker.RunID})
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", h.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE obligations SET created_at=? WHERE message_seq=?`, time.Now().UTC().Add(-api.ObligationAckGrace-time.Minute).Format(time.RFC3339Nano), m.Seq); err != nil {
		t.Fatal(err)
	}
	gated := api.Envelope{Kind: api.EnvelopeKindNotice, Subject: "A post the gate should refuse", Body: api.EnvelopeBody{Text: "text"}}
	if _, err := h.c.PostMessage(ctx, h.task.ID, api.PostMessageRequest{Envelope: &gated, Text: api.RenderText(gated), AgentID: h.agent.ID, RunID: h.agent.RunID}); err == nil {
		t.Fatal("the fixture's agent is not gated; the test proves nothing")
	}
	wrapperExited(ctx, h.e, 143, productionExitReporter(h.e), spawn.Survivor{PID: 73496, Command: "zsh"})
	notices := h.notices(t)
	if len(notices) != 1 || !strings.Contains(notices[0].Envelope.Body.Text, "pid 73496 zsh") {
		t.Fatalf("survivor notices %+v, want one naming pid 73496 zsh", notices)
	}
}
