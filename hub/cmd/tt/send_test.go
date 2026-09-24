package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/scs32/tailterm/hub/internal/api"
)

// Criterion a2: invalid messages exit 2 without contacting the hub.
func TestSendRejectsInvalidMessagesLocally(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("hub contacted: %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()
	e := env{hub: srv.URL, task: "tsk_0000000000000001", agent: "agt_0000000000000001"}
	file := filepath.Join(t.TempDir(), "msg.json")
	if err := os.WriteFile(file, []byte(`{"kind":"notice","subject":"A plain subject line","body":{"text":"x"},"surprise":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"--kind", "result", "--subject", "Released 43c1da3", "--outcome", "done"},
		{"--kind", "assign", "--subject", "Reject the empty recipient", "--acceptance", "no-equals-sign"},
		{"--kind", "notice", "--subject", "A plain subject line", "--text", "x", "stray"},
		{"--file", file},
		{"--file", file, "--subject", "An override attempt here"},
	} {
		err := cmdSend(e, args)
		var coded *exitError
		if !errors.As(err, &coded) || coded.code != 2 {
			t.Errorf("%v: err %v, want exit 2", args, err)
		}
	}
	err := cmdSend(e, []string{"--kind", "result", "--subject", "Released 43c1da3", "--outcome", "done"})
	for _, want := range []string{"subject: must not contain hashes", "body.status", "evidence:"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("error %v missing %q", err, want)
		}
	}
}

// Criterion a3 from the CLI side: a valid send stores the envelope and text.
func TestSendPostsTypedMessage(t *testing.T) {
	e, c, task, lead := cliWorkItemFixture(t)
	args := []string{"--to", "lead", "--kind", "result", "--subject", "Tests pass for the empty recipient check",
		"--outcome", "done", "--status", "a1=pass", "--evidence", "e1: go test ./cmd/tt -> ok", "--ref", "commit=abc1234"}
	out, err := captureCLIOutput(t, func() error { return cmdSend(e, args) })
	if err != nil || !strings.HasPrefix(out, "posted #") {
		t.Fatalf("send: %q %v", out, err)
	}
	again, err := captureCLIOutput(t, func() error { return cmdSend(e, args) })
	if err != nil || again != out {
		t.Fatalf("identical resend should return the original: %q vs %q (%v)", again, out, err)
	}
	msgs, err := c.ListMessages(context.Background(), task.ID, 0, "", 50)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("messages %v %+v", err, msgs)
	}
	m := msgs[0]
	if m.To != lead.ID || m.Envelope == nil || m.Envelope.To != "lead" || m.Text != api.RenderText(*m.Envelope) ||
		m.Envelope.Evidence["e1"] != (api.Evidence{Type: "command", Value: "go test ./cmd/tt", Outcome: "ok"}) {
		t.Fatalf("stored %+v", m)
	}
}

// Criterion a11: the summary counts forms, validity and senders.
func TestMessageChecksSummary(t *testing.T) {
	e, c, task, lead := cliWorkItemFixture(t)
	ctx := context.Background()
	post := func(agent, text string) {
		t.Helper()
		if _, err := c.PostMessage(ctx, task.ID, api.PostMessageRequest{AgentID: agent, Text: text}); err != nil {
			t.Fatal(err)
		}
	}
	if err := cmdSend(e, []string{"--kind", "notice", "--subject", "Integration window closes today", "--text", "rebase first"}); err != nil {
		t.Fatal(err)
	}
	post(lead.ID, "QUESTION: Which status code for rejected posts\nQuestion: Should rejected posts return 422?")
	post(lead.ID, "RESULT: Released 43c1da3\nOutcome: done")
	post(lead.ID, "SAVED activation release under exact lead8726")
	post("", "human note")
	out, err := captureCLIOutput(t, func() error { return cmdMessageChecks(e, []string{"--summary", "--since", "0"}) })
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Agent posts: 4", "typed                    1", "text_convention          1", "text_convention_invalid  1", "free_text                1",
		"Valid (typed + convention): 50% (2 of 4", "subject: must not contain hashes", "Jev: 0 scored, 0 unavailable, 0 pending, 4 disabled",
		"lead                     1/3", "database                 1/1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("summary missing %q:\n%s", want, out)
		}
	}
}

// Round-one blocker R1: a bound sender's typed message carries its item link,
// exactly like tt post, so bound recipients see it.
func TestBoundAgentSendCarriesItsRecordedItem(t *testing.T) {
	var posted api.PostMessageRequest
	binding := &api.AgentWorkItemBinding{
		ItemTaskID: "tsk_0000000000000001", ItemID: "wi_0000000000000002", ItemRevision: 3,
		WorkOrderMessage: api.MessageReference{TaskID: "tsk_0000000000000001", Seq: 814},
	}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "GET" && strings.Contains(r.URL.Path, "/agents/"):
			_ = json.NewEncoder(w).Encode(api.Agent{ID: "agt_0000000000000001", WorkItem: binding})
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/messages"):
			if err := json.NewDecoder(r.Body).Decode(&posted); err != nil {
				t.Error(err)
			}
			_ = json.NewEncoder(w).Encode(api.Message{Seq: 1})
		default:
			http.Error(w, "unexpected route", 500)
		}
	}))
	defer s.Close()
	t.Setenv("TAILTERM_WORK_ITEM", binding.ItemID)
	e := env{hub: s.URL, task: binding.ItemTaskID, agent: "agt_0000000000000001", runID: "run_0000000000000003"}
	if err := cmdSend(e, []string{"--kind", "notice", "--subject", "Build finished on the isolated worktree", "--text", "ready"}); err != nil {
		t.Fatal(err)
	}
	if posted.Envelope == nil || posted.RequestID == "" || posted.AuditKind != api.MessageAuditWork || len(posted.WorkItems) != 1 ||
		posted.WorkItems[0].ItemID != binding.ItemID || posted.WorkOrderMessage == nil || *posted.WorkOrderMessage != binding.WorkOrderMessage {
		t.Fatalf("bound send lost durable context: %+v", posted)
	}
	// The rendered text is sent too, so a hub that predates envelopes still
	// accepts the post as plain text; newer hubs verify it matches.
	if posted.Text != api.RenderText(*posted.Envelope) {
		t.Fatalf("text not rendered client-side: %q", posted.Text)
	}
}

// Round-one blocker R2: one recipient, taken from --to or the file, never both disagreeing.
func TestSendRecipientComesFromFlagOrFile(t *testing.T) {
	e, c, task, lead := cliWorkItemFixture(t)
	file := filepath.Join(t.TempDir(), "msg.json")
	if err := os.WriteFile(file, []byte(`{"kind":"notice","to":"lead","subject":"Integration window closes today","body":{"text":"rebase first"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := captureCLIOutput(t, func() error { return cmdSend(e, []string{"--file", file}) }); err != nil {
		t.Fatal(err)
	}
	msgs, err := c.ListMessages(context.Background(), task.ID, 0, "", 50)
	if err != nil || len(msgs) != 1 || msgs[0].To != lead.ID || msgs[0].Envelope.To != "lead" {
		t.Fatalf("file recipient not routed: %v %+v", err, msgs)
	}
	for _, args := range [][]string{
		{"--file", file, "--to", "database"},
		{"--kind", "notice", "--to", "role:lead", "--subject", "Integration window closes today", "--text", "x"},
	} {
		var coded *exitError
		if err := cmdSend(e, args); !errors.As(err, &coded) || coded.code != 2 {
			t.Errorf("%v: err %v, want exit 2", args, err)
		}
	}
}

// Round-one blocker R8: help succeeds without contacting the hub.
func TestMessageChecksHelpSucceeds(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("hub contacted: %s", r.URL.Path)
	}))
	defer srv.Close()
	if err := cmdMessageChecks(env{hub: srv.URL, task: "tsk_0000000000000001"}, []string{"--help"}); err != nil {
		t.Fatalf("help returned %v", err)
	}
}

// The two-entries-in-one---evidence guard (a non-blocking follow-up) was
// removed: every heuristic version misjudged quoted commands. The flag is
// always one entry, and escaping keeps it exact on the board.
func TestSendKeepsSemicolonsInsideOneEvidenceEntry(t *testing.T) {
	e, c, task, _ := cliWorkItemFixture(t)
	if err := cmdSend(e, []string{"--kind", "result", "--subject", "Quoted command output is stable", "--outcome", "done",
		"--status", "a1=pass", "--evidence", "e1 (command): printf 'ok; note: stable' -> ok"}); err != nil {
		t.Fatal(err)
	}
	msgs, err := c.ListMessages(context.Background(), task.ID, 0, "", 50)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("messages %v %d", err, len(msgs))
	}
	want := api.Evidence{Type: "command", Value: "printf 'ok; note: stable'", Outcome: "ok"}
	if got := msgs[0].Envelope.Evidence["e1"]; got != want {
		t.Fatalf("evidence %+v", got)
	}
	parsed, _ := api.ParseTextConvention(msgs[0].Text)
	if parsed.Evidence["e1"] != want {
		t.Fatalf("rendered text re-parses as %+v", parsed.Evidence)
	}
}
