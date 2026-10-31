package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

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

func TestSendPendingVerification(t *testing.T) {
	e, c, task, lead := cliWorkItemFixture(t)
	_ = lead
	args := []string{"--kind", "assign", "--subject", "Assign verification criterion fixture", "--to", "lead", "--objective", "fixture", "--owns", "fixture", "--acceptance", "a1=code", "--acceptance", "a2=matrix", "--verification-criterion", "a2"}
	if _, err := captureCLIOutput(t, func() error { return cmdSend(e, args) }); err != nil {
		t.Fatal(err)
	}
	msgs, err := c.ListMessages(context.Background(), task.ID, 0, "", 10)
	if err != nil || len(msgs) != 1 || len(msgs[0].Envelope.Body.VerificationCriteria) != 1 || msgs[0].Envelope.Body.VerificationCriteria[0] != "a2" || !strings.Contains(msgs[0].Text, "Verification-criteria: a2") {
		t.Fatal(msgs, err)
	}
	file := filepath.Join(t.TempDir(), "pending.json")
	raw := `{"kind":"result","subject":"Independent verification criterion pending","body":{"outcome":"done","status":{"a1":"pass","a2":"pending-verification"}},"evidence":{"e1":{"type":"command","value":"fixture check","outcome":"ok"}}}`
	if err := os.WriteFile(file, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := captureCLIOutput(t, func() error { return cmdSend(e, []string{"--file", file}) }); err != nil {
		t.Fatal(err)
	}
	msgs, err = c.ListMessages(context.Background(), task.ID, 0, "", 10)
	if err != nil || len(msgs) != 2 || msgs[1].Envelope.Body.Status["a2"] != "pending-verification" {
		t.Fatal(msgs, err)
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
	var coded *exitError
	if err := cmdSend(e, []string{"--file", file, "--to", "database"}); !errors.As(err, &coded) || coded.code != 2 {
		t.Errorf("conflicting recipients: err %v, want exit 2", err)
	}
	// Broker phase 3: a role recipient is resolved by the hub to the current lead.
	if _, err := captureCLIOutput(t, func() error {
		return cmdSend(e, []string{"--kind", "notice", "--to", "role:lead", "--subject", "Integration window closes today", "--text", "x"})
	}); err != nil {
		t.Fatalf("role recipient: %v", err)
	}
	msgs, err = c.ListMessages(context.Background(), task.ID, 0, "", 50)
	if err != nil || len(msgs) != 2 || msgs[1].To != lead.ID || msgs[1].Envelope.To != "role:lead" {
		t.Fatalf("role:lead not resolved to the lead: %v %+v", err, msgs)
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

func TestReviewConvergenceSendMetadataFile(t *testing.T) {
	e, c, task, _ := cliWorkItemFixture(t)
	// Disposition without a native item link is refused at the real endpoint.
	path := filepath.Join(t.TempDir(), "review.json")
	if err := os.WriteFile(path, []byte(`{"mode":"disposition","candidate":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","disposition":"accept"}`), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := captureCLIOutput(t, func() error {
		return cmdSend(e, []string{"--kind", "notice", "--subject", "Record exact review fixture disposition", "--text", "Accept", "--review-file", path})
	})
	if err == nil || !strings.Contains(err.Error(), "native primary item link") {
		t.Fatal(err)
	}
	messages, err := c.ListMessages(context.Background(), task.ID, 0, "", 100)
	if err != nil || len(messages) != 0 {
		t.Fatal("rejected CLI wrote a message", messages, err)
	}
	if err = os.WriteFile(path, []byte(`{"mode":"invalid"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err = cmdSend(e, []string{"--kind", "notice", "--subject", "Invalid review fixture transition", "--text", "Invalid", "--review-file", path}); err == nil || !strings.Contains(err.Error(), "review.mode") {
		t.Fatal(err)
	}
}

// A finding title the hub would refuse when filing the follow-up is refused
// before any request, naming the finding and the limit.
func TestSendRefusesBadReviewFindingTitleLocally(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("hub contacted: %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()
	e := env{hub: srv.URL, task: "tsk_0000000000000001", agent: "agt_0000000000000001"}
	candidate := strings.Repeat("a", 40)
	for name, tc := range map[string]struct{ title, want string }{
		"over limit":   {strings.Repeat("t", 121), "review.findings[f3].title: must be at most 120 characters, has 121"},
		"multibyte":    {strings.Repeat("é", 121), "review.findings[f3].title: must be at most 120 characters, has 121"},
		"empty":        {"", "review.findings[f3].title: must not be empty; a title is 1 to 120 characters"},
		"space padded": {" Padded title", "review.findings[f3].title: must not start or end with a space; a title is 1 to 120 characters"},
		"control":      {"Tab\tin title", "review.findings[f3].title: must not contain control characters; a title is 1 to 120 characters"},
	} {
		review := api.ReviewMetadata{Mode: "general", Candidate: candidate, Findings: []api.ReviewFinding{{ID: "f2", Title: "A fine title"}, {ID: "f3", Title: tc.title}}}
		reviewPath := filepath.Join(t.TempDir(), "review.json")
		raw, _ := json.Marshal(review)
		if err := os.WriteFile(reviewPath, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		envelope := api.Envelope{Kind: "result", Subject: "Review fixture passes its checks", Review: &review, Body: api.EnvelopeBody{Outcome: "done", Status: map[string]string{"a1": "pass"}}, Evidence: map[string]api.Evidence{"e1": {Type: "command", Value: "fixture", Outcome: "pass"}}}
		envelopePath := filepath.Join(t.TempDir(), "msg.json")
		raw, _ = json.Marshal(envelope)
		if err := os.WriteFile(envelopePath, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		for form, args := range map[string][]string{
			"review-file": {"--kind", "result", "--subject", "Review fixture passes its checks", "--outcome", "done", "--status", "a1=pass", "--evidence", "e1: fixture -> pass", "--review-file", reviewPath},
			"file":        {"--file", envelopePath},
		} {
			err := cmdSend(e, args)
			var exit *exitError
			if !errors.As(err, &exit) || exit.code != 2 {
				t.Fatalf("%s via --%s: want exit 2, got %v", name, form, err)
			}
			if !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "findings[f2]") {
				t.Fatalf("%s via --%s: error %q", name, form, err)
			}
		}
	}
}

// A 120-character finding title is at the limit: the review result posts and
// the hub files the follow-up under that title.
func TestSendPostsReviewFindingTitleAtLimit(t *testing.T) {
	e, c, task, _ := cliWorkItemFixture(t)
	ctx := context.Background()
	reviewer, err := c.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "reviewer", Host: "host", Session: "reviewer", Runtime: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	item, err := c.CreateWorkItem(ctx, task.ID, api.CreateWorkItemRequest{Kind: "bug", Title: "Finding title limit fixture", RequestID: "item"})
	if err != nil {
		t.Fatal(err)
	}
	links := []api.MessageWorkItem{{ItemTaskID: task.ID, ItemID: item.ID, ItemRevision: 1, Relationship: "primary"}}
	assign := api.Envelope{Kind: "assign", Subject: "Implement exact title fixture criteria", Body: api.EnvelopeBody{Objective: "Fixture", Owns: []string{"fixture"}, Acceptance: map[string]string{"a1": "passes"}}}
	order, err := c.PostMessage(ctx, task.ID, api.PostMessageRequest{Envelope: &assign, WorkItems: links, RequestID: "assign"})
	if err != nil {
		t.Fatal(err)
	}
	candidate := strings.Repeat("a", 40)
	request := api.Envelope{Kind: "review", Subject: "Review exact title fixture candidate", Body: api.EnvelopeBody{Candidate: candidate, Scope: "Fixture", Acceptance: map[string]string{"a1": "passes"}}}
	review, err := c.PostMessage(ctx, task.ID, api.PostMessageRequest{Envelope: &request, WorkItems: links, To: reviewer.ID, RequestID: "review"})
	if err != nil {
		t.Fatal(err)
	}
	title := strings.Repeat("é", 120)
	raw, _ := json.Marshal(api.ReviewMetadata{Mode: "general", Candidate: candidate, Findings: []api.ReviewFinding{{ID: "f1", Title: title, File: "fixture.go", Line: 1}}})
	path := filepath.Join(t.TempDir(), "review.json")
	if err = os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	e.agent, e.agentName, e.runID = reviewer.ID, reviewer.Name, reviewer.RunID
	out, err := captureCLIOutput(t, func() error {
		return cmdSend(e, []string{"--kind", "result", "--subject", "Title fixture review passes checks", "--outcome", "done", "--status", "a1=pass",
			"--evidence", "e1: fixture -> pass", "--review-file", path, "--reply-to", fmt.Sprint(review.Seq),
			"--work-item", item.ID, "--work-item-revision", "1", "--work-order-message", fmt.Sprint(order.Seq), "--request-id", "result"})
	})
	if err != nil || !strings.HasPrefix(out, "posted #") {
		t.Fatalf("at-limit title: %q %v", out, err)
	}
	items, err := c.ListWorkItems(ctx, task.ID, "", "", 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	filed := false
	for _, it := range items.Items {
		filed = filed || it.Title == title
	}
	if !filed {
		t.Fatalf("follow-up not filed under the at-limit title: %+v", items.Items)
	}
}

// Incident #14080: an unbound sender's typed reply to an item-linked parent
// takes the parent's exact link, keeps an explicit one, refuses a parent whose
// item has moved on, and links nothing when the parent is unlinked.
func TestSendReplyInheritsParentItemLink(t *testing.T) {
	e, c, task, lead := cliWorkItemFixture(t)
	ctx := context.Background()
	newItem := func(title string) api.WorkItem {
		t.Helper()
		item, err := c.CreateWorkItem(ctx, task.ID, api.CreateWorkItemRequest{Kind: "bug", Title: title, RequestID: strings.ReplaceAll(title, " ", "-")})
		if err != nil {
			t.Fatal(err)
		}
		return item
	}
	post := func(req api.PostMessageRequest) api.Message {
		t.Helper()
		m, err := c.PostMessage(ctx, task.ID, req)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	linkedParent := func(item api.WorkItem, key string) (api.Message, api.Message) {
		t.Helper()
		links := []api.MessageWorkItem{{ItemTaskID: task.ID, ItemID: item.ID, ItemRevision: item.Revision, Relationship: "primary"}}
		order := post(api.PostMessageRequest{Text: "WORK ORDER " + key, WorkItems: links, RequestID: key + "-order"})
		ask := api.Envelope{Kind: "request", Subject: "Record the start gate for this item", Body: api.EnvelopeBody{Ask: "Save the gate"}}
		parent := post(api.PostMessageRequest{Envelope: &ask, Text: api.RenderText(ask), AgentID: lead.ID, To: e.agent, WorkItems: links,
			WorkOrderMessage: &api.MessageReference{TaskID: task.ID, Seq: order.Seq}, RequestID: key + "-request"})
		return order, parent
	}
	reply := func(parent int64, extra ...string) error {
		args := append([]string{"--kind", "result", "--to", "lead", "--reply-to", fmt.Sprint(parent), "--subject", "Saved the start gate and read it back",
			"--outcome", "done", "--status", "a1=pass", "--evidence", "e1: readback -> saved"}, extra...)
		_, err := captureCLIOutput(t, func() error { return cmdSend(e, args) })
		return err
	}
	last := func() api.Message {
		t.Helper()
		msgs, err := c.LatestMessages(ctx, task.ID, 1)
		if err != nil || len(msgs) != 1 {
			t.Fatalf("latest: %v %+v", err, msgs)
		}
		return msgs[0]
	}

	// Exact parent link: the reply carries the parent's task, item, revision
	// and work order, and tt work-items messages lists it with its reply-to.
	item := newItem("Reply link fixture")
	order, parent := linkedParent(item, "exact")
	if err := reply(parent.Seq); err != nil {
		t.Fatal(err)
	}
	got := last()
	if got.ReplyTo != parent.Seq || len(got.WorkItems) != 1 || got.WorkItems[0] != parent.WorkItems[0] ||
		got.WorkOrderMessage == nil || *got.WorkOrderMessage != (api.MessageReference{TaskID: task.ID, Seq: order.Seq}) {
		t.Fatalf("reply lost the parent link: %+v", got)
	}
	list, err := c.ListWorkItemMessages(ctx, task.ID, item.ID, 0, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	listed := false
	for _, l := range list.Links {
		listed = listed || (l.Message.Seq == got.Seq && l.Message.ReplyTo == parent.Seq && l.ItemRevision == item.Revision && l.Relationship == "primary")
	}
	if !listed {
		t.Fatalf("reply #%d missing from the item's messages: %+v", got.Seq, list.Links)
	}

	// Stale parent revision: refused before posting, never re-pointed.
	description := "revised"
	revised, err := c.UpdateWorkItem(ctx, task.ID, item.ID, api.UpdateWorkItemRequest{Revision: item.Revision, Description: &description})
	if err != nil || revised.Revision != item.Revision+1 {
		t.Fatalf("revise: %v %+v", err, revised)
	}
	before := last().Seq
	err = reply(parent.Seq)
	if err == nil || !strings.Contains(err.Error(), "now at revision 2") || !strings.Contains(err.Error(), "--work-item-revision 2") {
		t.Fatalf("stale parent: %v", err)
	}
	if last().Seq != before {
		t.Fatal("stale parent reply was posted")
	}

	// An explicit link to a different item is kept as given.
	other := newItem("Other reply link fixture")
	otherOrder := post(api.PostMessageRequest{Text: "WORK ORDER other",
		WorkItems: []api.MessageWorkItem{{ItemTaskID: task.ID, ItemID: other.ID, ItemRevision: other.Revision, Relationship: "primary"}}, RequestID: "other-order"})
	_, fresh := linkedParent(revised, "explicit")
	if err := reply(fresh.Seq, "--work-item", other.ID, "--work-item-revision", fmt.Sprint(other.Revision), "--work-order-message", fmt.Sprint(otherOrder.Seq)); err != nil {
		t.Fatal(err)
	}
	if got := last(); len(got.WorkItems) != 1 || got.WorkItems[0].ItemID != other.ID || got.WorkOrderMessage == nil || got.WorkOrderMessage.Seq != otherOrder.Seq {
		t.Fatalf("explicit link replaced: %+v", got)
	}

	// An unlinked parent gives nothing to inherit and posts as before.
	plain := post(api.PostMessageRequest{AgentID: lead.ID, To: e.agent, Text: "Is the gate saved?"})
	if err := reply(plain.Seq); err != nil {
		t.Fatal(err)
	}
	if got := last(); got.ReplyTo != plain.Seq || len(got.WorkItems) != 0 || got.WorkOrderMessage != nil {
		t.Fatalf("unlinked parent inherited a link: %+v", got)
	}
}

// claimHub is a stand-in hub that records every request. A strict hub fails
// the test on any request at all.
type claimHub struct {
	url    string
	mu     sync.Mutex
	bodies [][]byte
	paths  []string
}

func newClaimHub(t *testing.T, strict bool) *claimHub {
	t.Helper()
	h := &claimHub{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strict {
			t.Errorf("hub contacted: %s %s", r.Method, r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		h.mu.Lock()
		h.paths = append(h.paths, r.Method+" "+r.URL.Path)
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/messages") {
			h.bodies = append(h.bodies, body)
		}
		h.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"seq":7,"taskId":"tsk_0000000000000001"}`))
	}))
	t.Cleanup(srv.Close)
	h.url = srv.URL
	return h
}

func (h *claimHub) posts() [][]byte {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.bodies)
}

// claimSend runs one send and returns its stdout, stderr and error.
func claimSend(t *testing.T, e env, args ...string) (string, string, error) {
	t.Helper()
	var stderr strings.Builder
	restore := captureStderr(t, &stderr)
	out, err := captureCLIOutput(t, func() error { return cmdSend(e, args) })
	restore()
	return out, stderr.String(), err
}

// claimLedgerSnapshot lists every file under the ledger root with its size
// and modification time.
func claimLedgerSnapshot(t *testing.T, root string) string {
	t.Helper()
	var b strings.Builder
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		fmt.Fprintf(&b, "%s %d %d %s\n", path, info.Size(), info.ModTime().UnixNano(), info.Mode())
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// The rows a checked send in these tests is compared with: one failing run
// of the cited command, so the report has something to say.
func claimSendLedger(t *testing.T) string {
	t.Helper()
	return claimLedgerDir(t, claimEvidenceRow("go test ./cmd/tt", claimExit(1), claimCommitA, time.Now()))
}

// Criterion a1: a checked RESULT and a lead's accept print the report on
// stderr and post the same request body as a send with an empty ledger.
func TestSendClaimCheckWarnsAndPostsUnchanged(t *testing.T) {
	review := filepath.Join(t.TempDir(), "review.json")
	if err := os.WriteFile(review, []byte(`{"mode":"disposition","candidate":"`+claimCommitA+`","disposition":"accept"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, args := range map[string][]string{
		"result": {"--kind", "result", "--subject", "Tests pass for the claim checker", "--outcome", "done", "--status", "a1=pass",
			"--evidence", "e1: go test ./cmd/tt -> ok", "--ref", "commit=" + claimCommitA[:8]},
		"accept": {"--kind", "notice", "--subject", "Accept the claim checker candidate", "--text", "Accept",
			"--review-file", review},
	} {
		e := env{task: "tsk_0000000000000001", agent: claimTestAgent}
		empty := filepath.Join(t.TempDir(), "empty-ledger")
		t.Setenv("TAILTERM_TOOL_LEDGER_DIR", empty)
		hub := newClaimHub(t, false)
		e.hub = hub.url
		out, stderr, err := claimSend(t, e, args...)
		if err != nil || stderr != "" || out != "posted #7\n" {
			t.Fatalf("%s, empty ledger: %q %q %v", name, out, stderr, err)
		}
		claimSendLedger(t)
		out, stderr, err = claimSend(t, e, args...)
		// The result's command contradicts the failing run; the accept's
		// candidate matches the run's commit.
		want := map[string]string{"result": "claim check (your local tool ledger; warning only, the message is sent unchanged):\n  contradicted  e1 go test ./cmd/tt -> ok",
			"accept": "claim check: 1 cited claim matches your tool ledger\n"}[name]
		if err != nil || out != "posted #7\n" || !strings.HasPrefix(stderr, want) {
			t.Fatalf("%s, with a ledger: %q %q %v", name, out, stderr, err)
		}
		posts := hub.posts()
		if len(posts) != 2 || !bytes.Equal(posts[0], posts[1]) || !bytes.Contains(posts[0], []byte(`"requestId"`)) {
			t.Fatalf("%s: posted bodies differ:\n%s\n%s", name, posts[0], posts[len(posts)-1])
		}
	}
}

// Criterion a5: ledger-less senders and other kinds see nothing and post what
// they post today, and no send touches the ledger directory.
func TestSendClaimCheckSkipsLedgerlessAndOtherKinds(t *testing.T) {
	result := []string{"--kind", "result", "--subject", "Tests pass for the claim checker", "--outcome", "done", "--status", "a1=pass",
		"--evidence", "e1: go test ./cmd/tt -> ok", "--ref", "commit=" + claimCommitA[:8]}
	review := filepath.Join(t.TempDir(), "review.json")
	if err := os.WriteFile(review, []byte(`{"mode":"disposition","candidate":"`+claimCommitA+`","disposition":"follow-ups"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	kinds := map[string][]string{
		"assign": {"--kind", "assign", "--subject", "Build the claim checker now", "--objective", "Build it", "--owns", "hub/cmd/tt/claim_check.go",
			"--acceptance", "a1=go test ./cmd/tt passes", "--ref", "commit=" + claimCommitA[:8]},
		"request":  {"--kind", "request", "--subject", "Confirm the claim checker order", "--ask", "Confirm it", "--ref", "commit=" + claimCommitA[:8]},
		"review":   {"--kind", "review", "--subject", "Review the claim checker candidate", "--candidate", claimCommitA, "--scope", "all", "--acceptance", "a1=tests pass"},
		"question": {"--kind", "question", "--subject", "Which ledger does the check read", "--question", "Whose ledger?"},
		"notice":   {"--kind", "notice", "--subject", "Follow ups for the claim checker", "--text", "Follow-ups", "--review-file", review},
		"plain":    {"--kind", "notice", "--subject", "The claim checker is under way", "--text", "go test ./cmd/tt -> ok"},
	}
	baseline := func(args []string) []byte {
		t.Helper()
		t.Setenv("TAILTERM_TOOL_LEDGER_DIR", filepath.Join(t.TempDir(), "empty-ledger"))
		hub := newClaimHub(t, false)
		if _, _, err := claimSend(t, env{hub: hub.url, task: "tsk_0000000000000001", agent: claimTestAgent}, args...); err != nil {
			t.Fatal(err)
		}
		return hub.posts()[0]
	}
	check := func(name string, e env, args []string, want []byte) {
		t.Helper()
		hub := newClaimHub(t, false)
		e.hub = hub.url
		out, stderr, err := claimSend(t, e, args...)
		if err != nil || stderr != "" || out != "posted #7\n" {
			t.Errorf("%s: %q %q %v", name, out, stderr, err)
			return
		}
		if posts := hub.posts(); len(posts) != 1 || !bytes.Equal(posts[0], want) {
			t.Errorf("%s: posted body differs from today's", name)
		}
	}
	wants := map[string][]byte{"result": baseline(result)}
	for name, args := range kinds {
		wants[name] = baseline(args)
	}
	root := claimSendLedger(t)
	before := claimLedgerSnapshot(t, root)
	e := env{task: "tsk_0000000000000001", agent: claimTestAgent}
	for name, args := range kinds {
		check(name, e, args, wants[name])
	}
	// The checked send itself reads the ledger and leaves it as it was.
	if _, stderr, err := claimSend(t, env{hub: newClaimHub(t, false).url, task: e.task, agent: claimTestAgent}, result...); err != nil || stderr == "" {
		t.Fatalf("checked send: %q %v", stderr, err)
	}
	// Ledger-less: another agent with no files under the root.
	other := e
	other.agent = "agt_0000000000000d0e"
	t.Setenv("TAILTERM_TOOL_LEDGER_DIR", filepath.Join(t.TempDir(), "empty-ledger"))
	otherWant := func() []byte {
		hub := newClaimHub(t, false)
		if _, _, err := claimSend(t, env{hub: hub.url, task: e.task, agent: other.agent}, result...); err != nil {
			t.Fatal(err)
		}
		return hub.posts()[0]
	}()
	t.Setenv("TAILTERM_TOOL_LEDGER_DIR", root)
	check("no ledger files", other, result, otherWant)
	// Not even a timeout is reported to a sender with no ledger.
	old := claimCheckBudget
	claimCheckBudget = time.Nanosecond
	check("no ledger files, 1 ns budget", other, result, otherWant)
	claimCheckBudget = old
	// No agent identity at all.
	t.Setenv("TAILTERM_TOOL_LEDGER_DIR", filepath.Join(t.TempDir(), "empty-ledger"))
	anonymousWant := func() []byte {
		hub := newClaimHub(t, false)
		if _, _, err := claimSend(t, env{hub: hub.url, task: e.task}, result...); err != nil {
			t.Fatal(err)
		}
		return hub.posts()[0]
	}()
	t.Setenv("TAILTERM_TOOL_LEDGER_DIR", root)
	check("no agent", env{task: e.task}, result, anonymousWant)
	if after := claimLedgerSnapshot(t, root); after != before {
		t.Fatalf("a send changed the ledger directory:\n%s\nbefore:\n%s", after, before)
	}
}

// Criterion a6, the dry run: --claims-only contacts no hub, prints the report
// on stdout and exits 1 when any claim is not matched or on timeout.
func TestSendClaimsOnly(t *testing.T) {
	hub := newClaimHub(t, true)
	e := env{hub: hub.url, task: "tsk_0000000000000001", agent: claimTestAgent}
	at := time.Now()
	claimLedgerDir(t,
		claimEvidenceRow("go test ./cmd/tt", claimExit(0), claimCommitA, at),
		claimEvidenceRow("go vet ./cmd/tt", nil, claimCommitA, at))
	send := func(extra ...string) []string {
		return append([]string{"--claims-only", "--kind", "result", "--subject", "Tests pass for the claim checker", "--outcome", "done",
			"--status", "a1=pass", "--ref", "commit=" + claimCommitA[:8]}, extra...)
	}
	exitCode := func(err error) int {
		var coded *exitError
		if err == nil {
			return 0
		}
		if errors.As(err, &coded) {
			return coded.code
		}
		return -1
	}
	out, stderr, err := claimSend(t, e, send("--evidence", "e1: go test ./cmd/tt -> ok")...)
	if exitCode(err) != 0 || stderr != "" || out != "claim check: 2 cited claims match your tool ledger\n" {
		t.Fatalf("all matched: %q %q %v", out, stderr, err)
	}
	out, _, err = claimSend(t, e, send("--evidence", "e1: go test ./cmd/tt -> ok", "--evidence", "e2: go vet ./cmd/tt -> clean")...)
	if exitCode(err) != 1 || !strings.Contains(out, "dry run, nothing is sent") || !strings.Contains(out, "unproven      e2 go vet ./cmd/tt -> clean: exit not recorded") ||
		!strings.Contains(out, "matched       e1 go test ./cmd/tt -> ok") {
		t.Fatalf("one unproven claim prints the full list: %q %v", out, err)
	}
	out, _, err = claimSend(t, e, send("--evidence", "e1: go test ./internal/store -> ok")...)
	if exitCode(err) != 1 || !strings.Contains(out, "unmatched     e1 go test ./internal/store -> ok") {
		t.Fatalf("unmatched: %q %v", out, err)
	}
	// With --file, the same dry run.
	file := filepath.Join(t.TempDir(), "msg.json")
	msg := `{"kind":"result","subject":"Tests pass for the claim checker","refs":{"commit":"` + claimCommitA[:8] + `"},` +
		`"body":{"outcome":"done","status":{"a1":"pass"}},"evidence":{"e1":{"type":"command","value":"go test ./cmd/tt","outcome":"FAIL"}}}`
	if err := os.WriteFile(file, []byte(msg), 0o600); err != nil {
		t.Fatal(err)
	}
	out, _, err = claimSend(t, e, "--file", file, "--claims-only")
	if exitCode(err) != 1 || !strings.Contains(out, "contradicted  e1 go test ./cmd/tt -> FAIL: your latest run exited 0") {
		t.Fatalf("--file: %q %v", out, err)
	}
	// A kind that is not checked, and an agent with no ledger, exit 0.
	out, _, err = claimSend(t, e, "--claims-only", "--kind", "question", "--subject", "Which ledger does the check read", "--question", "Whose?")
	if exitCode(err) != 0 || !strings.Contains(out, "is not checked") {
		t.Fatalf("question: %q %v", out, err)
	}
	none := e
	none.agent = "agt_0000000000000d0e"
	out, _, err = claimSend(t, none, send("--evidence", "e1: go test ./cmd/tt -> ok")...)
	if exitCode(err) != 0 || !strings.Contains(out, "no tool ledger") {
		t.Fatalf("no ledger: %q %v", out, err)
	}
	// Timeout: nothing was proven.
	old := claimCheckBudget
	claimCheckBudget = time.Nanosecond
	defer func() { claimCheckBudget = old }()
	out, _, err = claimSend(t, e, send("--evidence", "e1: go test ./cmd/tt -> ok")...)
	if exitCode(err) != 1 || out != "claim check skipped: over 1ns\n" {
		t.Fatalf("timeout: %q %v", out, err)
	}
}
