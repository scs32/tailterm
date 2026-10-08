package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/server"
	"github.com/scs32/tailterm/hub/internal/store"
)

// This file uses only symbols and flags that exist before the fix
// (wi_a31c079e98518efc), so TestInbox11759 can be copied onto the base
// commit and fail there on its assertions.

var inboxLine = regexp.MustCompile(`(?m)^#[0-9]+ `)

// inboxHub is cliWorkItemFixture that keeps the store, so fixtures can post
// in bulk without the hub's per-node write rate limit.
type inboxHub struct {
	e       env // the unbound database handler
	c       *api.Client
	st      *store.Store
	task    api.Task
	lead    api.Agent
	handler api.Agent
	by      api.Caller
}

func newInboxHub(t *testing.T) inboxHub {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	h := inboxHub{st: st, by: api.Caller{Node: "cli-test", User: "owner"}}
	ctx := context.Background()
	if h.task, err = st.CreateTask(ctx, api.CreateTaskRequest{Name: "CLI inbox", Orchestrator: "lead"}, h.by); err != nil {
		t.Fatal(err)
	}
	if h.lead, err = st.AddAgent(ctx, h.task.ID, api.AddAgentRequest{Name: "lead", Host: "host", Session: "lead", Runtime: "codex"}, h.by); err != nil {
		t.Fatal(err)
	}
	if h.handler, err = st.AddAgent(ctx, h.task.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: "database", Host: "host", Session: "database", Runtime: "codex", Role: api.AgentRoleDatabaseHandler}, h.by); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(server.New(st, func(*http.Request) (api.Caller, error) { return h.by, nil }))
	t.Cleanup(srv.Close)
	if h.c, err = api.NewClient(srv.URL, 0); err != nil {
		t.Fatal(err)
	}
	h.e = env{hub: srv.URL, task: h.task.ID, agent: h.handler.ID, agentName: h.handler.Name, runID: h.handler.RunID}
	return h
}

// post stores n messages directly, bypassing the HTTP rate limit.
func (h inboxHub) post(t *testing.T, req api.PostMessageRequest, n int) []api.Message {
	t.Helper()
	text, key := req.Text, req.RequestID
	out := make([]api.Message, 0, n)
	for i := 0; i < n; i++ {
		req.Text = fmt.Sprintf("%s %d", text, i)
		if key != "" {
			req.RequestID = fmt.Sprintf("%s-%d", key, i)
		}
		m, err := h.st.PostMessage(context.Background(), h.task.ID, req, h.by)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

// bind admits a work-item-bound agent and returns its CLI environment and
// the link that makes a message visible to it.
func (h inboxHub) bind(t *testing.T, name string) (env, api.Agent, []api.MessageWorkItem, *api.MessageReference) {
	t.Helper()
	ctx := context.Background()
	if _, err := h.st.PostEvent(ctx, h.task.ID, api.PostEventRequest{AgentID: h.handler.ID, RunID: h.handler.RunID, Kind: api.EventRunning}, h.by); err != nil {
		t.Fatal(err)
	}
	item, err := h.c.CreateWorkItem(ctx, h.task.ID, api.CreateWorkItemRequest{Kind: "bug", Title: "inbox fixture bug", RequestID: "inbox-item"})
	if err != nil {
		t.Fatal(err)
	}
	link := []api.MessageWorkItem{{ItemTaskID: h.task.ID, ItemID: item.ID, ItemRevision: item.Revision, Relationship: "primary"}}
	order, err := h.c.PostMessage(ctx, h.task.ID, api.PostMessageRequest{Text: "bounded fixture order", RequestID: "inbox-order", WorkItems: link})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = h.c.ConfirmWorkOrderScope(ctx, h.task.ID, item.ID, api.ConfirmWorkOrderScopeRequest{RequestID: "inbox-scope", AgentID: h.handler.ID, RunID: h.handler.RunID,
		ExpectedRevision: item.Revision, ScopeRevision: item.ScopeRevision, OrderMessageSeq: order.Seq, Complete: true}); err != nil {
		t.Fatal(err)
	}
	orderRef := &api.MessageReference{TaskID: h.task.ID, Seq: order.Seq}
	bound, err := h.c.AddAgent(ctx, h.task.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: name, Host: "fixture", Session: name, Runtime: "codex",
		WorkItem: &api.AgentWorkItemRequest{ItemTaskID: h.task.ID, ItemID: item.ID, ItemRevision: item.Revision, WorkOrderMessage: *orderRef, ContextBundle: teamCloseCLIContext(t, item, order)}})
	if err != nil {
		t.Fatal(err)
	}
	return env{hub: h.e.hub, task: h.e.task, agent: bound.ID, agentName: bound.Name, runID: bound.RunID}, bound, link, orderRef
}

// TestInbox11759 reproduces #11759: an unlinked directed owner message to a
// work-item-bound agent whose read cursor is stale, behind 250 older
// messages, must reach every inbox view and name its full-text command.
func TestInbox11759(t *testing.T) {
	h := newInboxHub(t)
	be, bound, link, orderRef := h.bind(t, "lead-bound")
	c := h.c
	ctx := context.Background()
	h.post(t, api.PostMessageRequest{Text: "older linked progress", AgentID: h.lead.ID, RequestID: "older", WorkItems: link, WorkOrderMessage: orderRef}, 250)
	text := strings.Repeat("Owner instruction for the lead: keep the queue moving. ", 3) + "FULL-TAIL-11759"
	if len([]rune(text)) < 150 {
		t.Fatalf("fixture text too short: %d runes", len([]rune(text)))
	}
	directed, err := c.PostMessage(ctx, be.task, api.PostMessageRequest{Text: text, To: bound.ID})
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"--unread"}, {"--unread", "--limit", "200"}, {}} {
		out, err := captureCLIOutput(t, func() error { return cmdInbox(be, args) })
		if err != nil || !strings.Contains(out, "FULL-TAIL-11759") {
			t.Errorf("tt inbox %s missed the directed message #%d (err %v); output has %d messages", strings.Join(args, " "), directed.Seq, err, len(inboxLine.FindAllString(out, -1)))
		}
	}
	if got, err := c.GetAgent(ctx, be.task, bound.ID); err != nil || got.Unread != 251 {
		t.Errorf("unread = %d (%v), want 251", got.Unread, err)
	}
	out, err := captureCLIOutput(t, func() error { return cmdObligations(be, nil) })
	if want := fmt.Sprintf("tt inbox --seq %d", directed.Seq); err != nil || !strings.Contains(out, want) {
		t.Errorf("tt obligations does not name %q (err %v): %s", want, err, out)
	}
}

func captureCLIStderr(t *testing.T, fn func()) string {
	t.Helper()
	out, _ := captureStream(t, &os.Stderr, func() error {
		fn()
		return nil
	})
	return out
}

// captureStream points *stream (os.Stdout or os.Stderr) at a pipe while fn
// runs and returns what fn wrote there, with fn's own error.
func captureStream(t *testing.T, stream **os.File, fn func() error) (string, error) {
	t.Helper()
	old := *stream
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	// Read while fn runs: a pipe holds only one buffer (64 KiB on macOS), so a
	// command that prints more would otherwise block in write forever.
	type captured struct {
		data []byte
		err  error
	}
	done := make(chan captured, 1)
	go func() {
		defer r.Close()
		data, err := io.ReadAll(r)
		done <- captured{data, err}
	}()
	*stream = w
	runErr := func() error {
		defer func() {
			*stream = old
			_ = w.Close()
		}()
		return fn()
	}()
	out := <-done
	if out.err != nil {
		t.Fatal(out.err)
	}
	return string(out.data), runErr
}

func TestCaptureCLIStderrReturnsOutputLargerThanAPipeBuffer(t *testing.T) {
	var want strings.Builder
	for i := 0; want.Len() <= 1<<20; i++ {
		fmt.Fprintf(&want, "line %07d of stderr larger than one pipe buffer\n", i)
	}
	old := os.Stderr
	got := captureCLIStderr(t, func() {
		if _, err := os.Stderr.WriteString(want.String()); err != nil {
			t.Errorf("write to the captured stderr: %v", err)
		}
	})
	if os.Stderr != old {
		t.Fatal("os.Stderr was not restored")
	}
	if got != want.String() {
		t.Fatalf("captured %d bytes, want %d bytes returned intact", len(got), want.Len())
	}
}

func TestInboxNewestAndPaging(t *testing.T) {
	h := newInboxHub(t)
	e := h.e
	msgs := h.post(t, api.PostMessageRequest{Text: "board", AgentID: h.lead.ID}, 7)
	out, err := captureCLIOutput(t, func() error { return cmdInbox(e, []string{"--limit", "3"}) })
	if err != nil || !strings.Contains(out, "board 6") || !strings.Contains(out, "board 4") || strings.Contains(out, "board 3") ||
		!strings.Contains(out, fmt.Sprintf("(older: tt inbox --before %d)", msgs[4].Seq)) {
		t.Fatalf("newest page = %q, %v", out, err)
	}
	if strings.Index(out, "board 4") > strings.Index(out, "board 6") {
		t.Fatalf("newest page is not oldest first: %q", out)
	}
	out, err = captureCLIOutput(t, func() error { return cmdInbox(e, []string{"--limit", "3", "--before", fmt.Sprint(msgs[4].Seq)}) })
	if err != nil || !strings.Contains(out, "board 1") || !strings.Contains(out, "board 3") || strings.Contains(out, "board 0") || strings.Contains(out, "board 4") {
		t.Fatalf("older page = %q, %v", out, err)
	}
	out, err = captureCLIOutput(t, func() error { return cmdInbox(e, []string{"--limit", "3", "--after", fmt.Sprint(msgs[0].Seq)}) })
	if err != nil || !strings.Contains(out, "board 1") || !strings.Contains(out, "board 3") || strings.Contains(out, "board 4") ||
		!strings.Contains(out, fmt.Sprintf("(newer: tt inbox --after %d)", msgs[3].Seq)) {
		t.Fatalf("newer page = %q, %v", out, err)
	}
	out, err = captureCLIOutput(t, func() error { return cmdInbox(e, []string{"--after", fmt.Sprint(msgs[5].Seq)}) })
	if err != nil || !strings.Contains(out, "board 6") || strings.Contains(out, "(newer:") {
		t.Fatalf("last page = %q, %v", out, err)
	}
	for _, args := range [][]string{{"--unread", "--before", "5"}, {"--mark-read"}, {"--limit", "0"}, {"--no-such-flag"}} {
		if _, err := captureCLIOutput(t, func() error { return cmdInbox(e, args) }); err == nil {
			t.Errorf("tt inbox %v accepted", args)
		}
	}
}

func TestInboxLimitClamp(t *testing.T) {
	h := newInboxHub(t)
	e := h.e
	h.post(t, api.PostMessageRequest{Text: "clamp", AgentID: h.lead.ID}, 205)
	var out string
	var err error
	stderr := captureCLIStderr(t, func() {
		out, err = captureCLIOutput(t, func() error { return cmdInbox(e, []string{"--limit", "20000"}) })
	})
	if err != nil || !strings.Contains(stderr, "--limit capped at 200 per page; page with --before/--after") {
		t.Fatalf("clamp note = %q, %v", stderr, err)
	}
	if n := len(inboxLine.FindAllString(out, -1)); n != 200 || !strings.Contains(out, "clamp 204") || !strings.Contains(out, "(older: tt inbox --before") {
		t.Fatalf("clamped page printed %d messages: %q", n, out)
	}
}

func TestInboxUnreadExtrasAndMarkRead(t *testing.T) {
	h := newInboxHub(t)
	e, c, task := h.e, h.c, h.task
	ctx := context.Background()
	cursor := func() int64 {
		t.Helper()
		a, err := c.GetAgent(ctx, task.ID, e.agent)
		if err != nil {
			t.Fatal(err)
		}
		return a.ReadUpTo
	}
	board := h.post(t, api.PostMessageRequest{Text: "board", AgentID: h.lead.ID}, 5)
	direct := h.post(t, api.PostMessageRequest{Text: "direct", AgentID: h.lead.ID, To: e.agent}, 2)

	out, err := captureCLIOutput(t, func() error { return cmdInbox(e, []string{"--unread", "--limit", "3"}) })
	if err != nil || !strings.Contains(out, "board 2") || strings.Contains(out, "board 3") ||
		!strings.Contains(out, "-- 2 newer message(s) addressed to you beyond this page --") || !strings.Contains(out, "direct 0") || !strings.Contains(out, "direct 1") ||
		!strings.Contains(out, fmt.Sprintf("More unread after #%d", board[2].Seq)) {
		t.Fatalf("unread page with extras = %q, %v", out, err)
	}
	if got := cursor(); got != 0 {
		t.Fatalf("unread without --mark-read moved the cursor to %d", got)
	}
	var js []api.Message
	out, err = captureCLIOutput(t, func() error { return cmdInbox(e, []string{"--unread", "--limit", "3", "--json"}) })
	if err != nil || json.Unmarshal([]byte(out), &js) != nil || len(js) != 5 || js[2].Seq != board[2].Seq || js[3].Seq != direct[0].Seq || js[4].Seq != direct[1].Seq {
		t.Fatalf("unread JSON = %q, %v", out, err)
	}
	if _, err = captureCLIOutput(t, func() error { return cmdInbox(e, []string{"--unread", "--mark-read", "--limit", "3"}) }); err != nil {
		t.Fatal(err)
	}
	if got := cursor(); got != board[2].Seq {
		t.Fatalf("--mark-read set the cursor to %d, want the page end %d (never an extra)", got, board[2].Seq)
	}
	// Repeating --unread --mark-read drains the backlog and shows every message.
	var drained strings.Builder
	for i := 0; i < 10 && !strings.Contains(drained.String(), "(no messages)"); i++ {
		out, err = captureCLIOutput(t, func() error { return cmdInbox(e, []string{"--unread", "--mark-read", "--limit", "3"}) })
		if err != nil {
			t.Fatal(err)
		}
		drained.WriteString(out)
	}
	for _, want := range []string{"board 3", "board 4", "direct 0", "direct 1", "(no messages)"} {
		if !strings.Contains(drained.String(), want) {
			t.Fatalf("drain missed %q: %q", want, drained.String())
		}
	}
	if got := cursor(); got != direct[1].Seq {
		t.Fatalf("drained cursor = %d, want %d", got, direct[1].Seq)
	}
	// A page of this agent's own messages still advances the cursor.
	own := h.post(t, api.PostMessageRequest{Text: "own", AgentID: e.agent}, 3)
	later := h.post(t, api.PostMessageRequest{Text: "later", AgentID: h.lead.ID}, 1)
	out, err = captureCLIOutput(t, func() error { return cmdInbox(e, []string{"--unread", "--mark-read", "--limit", "3"}) })
	if err != nil || strings.Contains(out, "own 0") || strings.Contains(out, "later 0") {
		t.Fatalf("self-sent page = %q, %v", out, err)
	}
	if got := cursor(); got != own[2].Seq {
		t.Fatalf("self-sent page left the cursor at %d, want %d", got, own[2].Seq)
	}
	out, err = captureCLIOutput(t, func() error { return cmdInbox(e, []string{"--unread", "--mark-read", "--limit", "3"}) })
	if err != nil || !strings.Contains(out, "later 0") || cursor() != later[0].Seq {
		t.Fatalf("page after the self-sent tail = %q, %v", out, err)
	}
}

func TestInboxSeqPrintsFullMessage(t *testing.T) {
	h := newInboxHub(t)
	e, c, task, lead := h.e, h.c, h.task, h.lead
	ctx := context.Background()
	text := strings.Repeat("A long owner request that runs past the obligation subject. ", 3) + "SEQ-TAIL"
	m, err := c.PostMessage(ctx, task.ID, api.PostMessageRequest{Text: text, To: lead.ID})
	if err != nil {
		t.Fatal(err)
	}
	out, err := captureCLIOutput(t, func() error { return cmdInbox(e, []string{"--seq", fmt.Sprint(m.Seq)}) })
	if err != nil || !strings.Contains(out, text) || len(inboxLine.FindAllString(out, -1)) != 1 {
		t.Fatalf("--seq output = %q, %v", out, err)
	}
	var one api.Message
	out, err = captureCLIOutput(t, func() error { return cmdInbox(e, []string{"--seq", fmt.Sprint(m.Seq), "--json"}) })
	if err != nil || json.Unmarshal([]byte(out), &one) != nil || one.Seq != m.Seq || one.Text != text {
		t.Fatalf("--seq --json = %q, %v", out, err)
	}
	missing := m.Seq + 1000
	if _, err = captureCLIOutput(t, func() error { return cmdInbox(e, []string{"--seq", fmt.Sprint(missing)}) }); err == nil ||
		!strings.Contains(err.Error(), fmt.Sprintf("no message #%d", missing)) {
		t.Fatalf("missing --seq error = %v", err)
	}
	if _, err = captureCLIOutput(t, func() error { return cmdInbox(e, []string{"--seq", fmt.Sprint(m.Seq), "--unread"}) }); err == nil {
		t.Fatal("--seq with --unread accepted")
	}
	// Every obligation line names the command printing its full text.
	le := env{hub: e.hub, task: e.task, agent: lead.ID, agentName: lead.Name, runID: lead.RunID}
	out, err = captureCLIOutput(t, func() error { return cmdObligations(le, nil) })
	lines := inboxLine.FindAllStringIndex(out, -1)
	if err != nil || len(lines) != 1 {
		t.Fatalf("obligations = %q, %v", out, err)
	}
	for _, line := range strings.Split(out, "\n") {
		if inboxLine.MatchString(line) && !strings.HasSuffix(line, fmt.Sprintf("(full text: tt inbox --seq %d)", m.Seq)) {
			t.Fatalf("obligation line lacks the full-text command: %q", line)
		}
	}
}

// wi_e1b6ca74b7e58414 (v5): tt inbox --unread --wait returns for an unlinked
// message addressed to a work-item-bound agent, and still waits out its
// timeout when only traffic the agent cannot see arrives.
func TestInboxWaitBoundAgentUnlinkedDirected(t *testing.T) {
	h := newInboxHub(t)
	be, bound, _, _ := h.bind(t, "wait-bound")
	if err := h.c.MarkRead(context.Background(), h.task.ID, api.MarkReadRequest{AgentID: bound.ID, UpTo: bound.ReadUpTo}); err != nil {
		t.Fatal(err)
	}
	old := inboxPollInterval
	inboxPollInterval = 20 * time.Millisecond
	t.Cleanup(func() { inboxPollInterval = old })

	h.post(t, api.PostMessageRequest{Text: "unlinked board-wide chatter"}, 1)
	h.post(t, api.PostMessageRequest{Text: "unlinked message to the lead", To: h.lead.ID}, 1)
	start := time.Now()
	out, err := captureCLIOutput(t, func() error { return cmdInbox(be, []string{"--unread", "--wait", "200ms"}) })
	if err != nil || !strings.Contains(out, "(no messages)") || time.Since(start) < 150*time.Millisecond {
		t.Fatalf("wait with nothing visible = %q, %v after %v", out, err, time.Since(start))
	}

	go func() {
		time.Sleep(100 * time.Millisecond)
		_, _ = h.st.PostMessage(context.Background(), h.task.ID, api.PostMessageRequest{Text: "unlinked owner instruction WAIT-TAIL", To: bound.ID}, h.by)
	}()
	start = time.Now()
	out, err = captureCLIOutput(t, func() error { return cmdInbox(be, []string{"--unread", "--wait", "5s"}) })
	if err != nil || !strings.Contains(out, "WAIT-TAIL") {
		t.Fatalf("wait output = %q, %v", out, err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("wait did not return promptly: %v", time.Since(start))
	}
}
