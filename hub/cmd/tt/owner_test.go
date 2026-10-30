package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

func TestOwnerCommandsReachTestHub(t *testing.T) {
	for _, tc := range []struct {
		name, kind  string
		args        []string
		wantState   string
		wantOutcome string
	}{
		{"extend", "assign", []string{"--for", "30m", "--request-id", "owner-extend-test"}, "", ""},
		{"answer", "question", []string{"--text", "Use the agreed option", "--request-id", "owner-answer-test"}, api.ObligationClosed, api.OutcomeAnswered},
		{"cancel", "assign", []string{"--reason", "No longer needed", "--request-id", "owner-cancel-test"}, api.ObligationClosed, api.OutcomeCancelled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, c, task, lead := cliWorkItemFixture(t)
			ctx := context.Background()
			body := api.EnvelopeBody{Objective: "Complete the task", Owns: []string{"artifact"}, Acceptance: map[string]string{"a1": "done"}}
			if tc.kind == "question" {
				body = api.EnvelopeBody{Question: "Which option should we use?"}
			}
			m, err := c.PostMessage(ctx, task.ID, api.PostMessageRequest{AgentID: lead.ID, To: e.agent, Envelope: &api.Envelope{
				Kind: tc.kind, To: e.agentName, Subject: "Owner command test obligation", Body: body,
			}})
			if err != nil {
				t.Fatal(err)
			}
			list, err := c.ListObligations(ctx, task.ID, e.agent, "", true, false)
			if err != nil || len(list) != 1 || list[0].MessageSeq != m.Seq {
				t.Fatalf("test hub obligation: %+v %v", list, err)
			}
			obligation := list[0]
			if tc.wantState == "" {
				tc.wantState = obligation.State
			}
			e.agent, e.agentName = "", "" // the owner CLI has no agent identity
			out, err := captureCLIOutput(t, func() error {
				return cmdOwner(e, append([]string{tc.name, obligation.ID}, tc.args...))
			})
			if err != nil || !strings.Contains(out, tc.name+" #") {
				t.Fatalf("owner %s output %q: %v", tc.name, out, err)
			}
			list, err = c.ListObligations(ctx, task.ID, "", "", false, false)
			if err != nil {
				t.Fatalf("persisted obligations: %+v %v", list, err)
			}
			var got api.Obligation
			for _, candidate := range list {
				if candidate.ID == obligation.ID {
					got = candidate
					break
				}
			}
			if got.ID != obligation.ID || got.State != tc.wantState || got.Outcome != tc.wantOutcome {
				t.Fatalf("persisted obligation: %+v", got)
			}
			if tc.name == "extend" {
				now := time.Now()
				if got.DueAt.Equal(obligation.DueAt) || !got.DueAt.After(now.Add(25*time.Minute)) || !got.DueAt.Before(now.Add(35*time.Minute)) {
					t.Fatalf("extension due_at = %s, original = %s, now = %s", got.DueAt, obligation.DueAt, now)
				}
			}
		})
	}
}

func TestOwnerCommandsRejectInvalidObligationIDsBeforeHub(t *testing.T) {
	for _, sub := range []string{"extend", "answer", "cancel"} {
		t.Run(sub, func(t *testing.T) {
			e, _, _, _ := cliWorkItemFixture(t)
			e.agent, e.agentName = "", ""
			for _, id := range []string{"obl_short", "obl_000000000000000g", "obl_0000000000000001/extra", "wi_0000000000000001", "tsk_0000000000000001"} {
				if err := cmdOwner(e, []string{sub, id}); err == nil || !strings.HasPrefix(err.Error(), "usage: tt owner") {
					t.Fatalf("%s %s: got %v, want usage", sub, id, err)
				}
			}
		})
	}
}

func TestOwnerIDValidationDoesNotWidenSharedValidID(t *testing.T) {
	id := "obl_0000000000000001"
	for _, prefix := range []string{"tsk", "agt", "wi", "obl"} {
		if api.ValidID(id, prefix) {
			t.Fatalf("shared ValidID unexpectedly accepted %s as %s", id, prefix)
		}
	}
}

// plainTerminal clears the runtime session variables: the tests themselves
// usually run inside Claude Code or Codex.
func plainTerminal(t *testing.T) {
	t.Helper()
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	t.Setenv("CODEX_THREAD_ID", "")
}

func TestOwnerInterveneRecordsAndListsInterventions(t *testing.T) {
	plainTerminal(t)
	e, c, task, _ := cliWorkItemFixture(t)
	e.agent, e.agentName = "", "" // the owner CLI has no agent identity
	ctx := context.Background()
	item, err := c.CreateWorkItem(ctx, task.ID, api.CreateWorkItemRequest{Kind: "feature", Title: "Concerned item", RequestID: "cli-concerned"})
	if err != nil {
		t.Fatal(err)
	}
	home, err := c.CreateTask(ctx, api.CreateTaskRequest{Name: "CLI product home"})
	if err != nil {
		t.Fatal(err)
	}
	product, err := c.CreateWorkItem(ctx, home.ID, api.CreateWorkItemRequest{Kind: "feature", Title: "Product fix", RequestID: "cli-product"})
	if err != nil {
		t.Fatal(err)
	}
	out, err := captureCLIOutput(t, func() error {
		return cmdOwner(e, []string{"intervene", "--kind", "nudge", "--item", item.ID, "--product-item", product.ID, "--text", "Nudged the stalled builder"})
	})
	if err != nil || !strings.HasPrefix(out, "intervention #") || !strings.Contains(out, "nudge on "+item.ID+" by owner (product fix "+product.ID+")") {
		t.Fatalf("intervene output %q: %v", out, err)
	}
	messages, err := c.ListMessages(ctx, task.ID, 0, "", 50)
	if err != nil || len(messages) != 1 {
		t.Fatalf("board: %+v %v", messages, err)
	}
	m := messages[0]
	want := api.Intervention{Kind: "nudge", ItemTaskID: task.ID, ItemID: item.ID, ProductTaskID: home.ID, ProductItemID: product.ID, Actor: "owner"}
	wantLinks := []api.MessageWorkItem{{ItemTaskID: task.ID, ItemID: item.ID, ItemRevision: item.Revision, Relationship: "primary"}}
	if m.Intervention == nil || *m.Intervention != want || !reflect.DeepEqual(m.WorkItems, wantLinks) || m.From.AgentID != "" || m.From.User != "owner" {
		t.Fatalf("recorded message: %+v", m)
	}
	// Same-minute identical retry derives the same request ID: no duplicate.
	if _, err = captureCLIOutput(t, func() error {
		return cmdOwner(e, []string{"intervene", "--kind", "nudge", "--item", item.ID, "--product-item", product.ID, "--text", "Nudged the stalled builder"})
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = captureCLIOutput(t, func() error {
		return cmdOwner(e, []string{"intervene", "--kind", "release", "--item", item.ID, "--text", "Released by hand", "--request-id", "cli-release"})
	}); err != nil {
		t.Fatal(err)
	}
	out, err = captureCLIOutput(t, func() error { return cmdOwner(e, []string{"interventions", "--tz", "UTC"}) })
	if err != nil || !strings.Contains(out, "2 interventions (UTC); 1 of 2 linked to a product item") || !strings.Contains(out, "by kind: release 1, nudge 1") || !strings.Contains(out, "unlinked:") || !strings.Contains(out, "owner  release  "+item.ID) || !strings.Contains(out, "by actor: owner 2, helper 0, unknown 0") {
		t.Fatalf("interventions output %q: %v", out, err)
	}
	if err = cmdOwner(e, []string{"interventions", "--tz", "Mars/Olympus"}); err == nil {
		t.Fatal("invalid time zone accepted")
	}
}

func TestOwnerInterveneRefusesAgentsAndBadInputBeforeAnyRequest(t *testing.T) {
	plainTerminal(t)
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "unexpected request", http.StatusTeapot)
	}))
	t.Cleanup(srv.Close)
	agent := env{hub: srv.URL, task: "tsk_0000000000000001", agent: "agt_0000000000000001", agentName: "builder"}
	valid := []string{"intervene", "--kind", "nudge", "--item", "wi_0000000000000001", "--text", "t"}
	for _, args := range [][]string{valid, {"interventions"}} {
		if err := cmdOwner(agent, args); err == nil || !strings.Contains(err.Error(), "not agent sessions") {
			t.Fatalf("agent %v: err=%v", args, err)
		}
	}
	owner := agent
	owner.agent, owner.agentName = "", ""
	for _, args := range [][]string{
		{"intervene", "--kind", "foo", "--item", "wi_0000000000000001", "--text", "t"},
		{"intervene", "--kind", "nudge", "--item", "wi_0000000000000001", "--text", "  "},
		{"intervene", "--kind", "nudge", "--text", "t"},
		{"intervene", "--kind", "nudge", "--item", "wi_0000000000000001", "--product-item", "bad", "--text", "t"},
		{"intervene", "--kind", "nudge", "--item", "wi_0000000000000001", "--text", "t", "extra"},
	} {
		if err := cmdOwner(owner, args); err == nil || !strings.Contains(err.Error(), "usage: tt owner intervene") {
			t.Fatalf("owner %v: err=%v", args, err)
		}
	}
	if n := requests.Load(); n != 0 {
		t.Fatalf("refused commands sent %d requests", n)
	}
}

// The owner's shell has no TAILTERM_TASK, so the usage must name --task.
func TestOwnerInterventionUsageNamesTaskWhenNoProjectIsSet(t *testing.T) {
	plainTerminal(t)
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "unexpected request", http.StatusTeapot)
	}))
	t.Cleanup(srv.Close)
	owner := env{hub: srv.URL}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"intervene", "--kind", "nudge", "--item", "wi_0000000000000001", "--text", "t"}, "usage: tt owner intervene --task ID "},
		{[]string{"interventions"}, "usage: tt owner interventions --task ID "},
		{nil, "intervene --task ID --kind KIND"},
		{nil, "interventions --task ID [--tz ZONE]"},
	} {
		err := cmdOwner(owner, tc.args)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%v: err=%v, want %q", tc.args, err, tc.want)
		}
		if len(tc.args) > 0 && !strings.Contains(err.Error(), "required unless TAILTERM_TASK is set") {
			t.Fatalf("%v: usage does not explain --task: %v", tc.args, err)
		}
	}
	// With --task the same command reaches the hub.
	if err := cmdOwner(owner, []string{"interventions", "--task", "tsk_0000000000000001", "--tz", "UTC"}); err == nil || requests.Load() != 1 {
		t.Fatalf("explicit --task: err=%v requests=%d", err, requests.Load())
	}
}

// A plain terminal is the owner in person; a Claude Code or Codex session must
// say whether it records the owner's act or its own.
func TestOwnerInterveneActorFollowsSession(t *testing.T) {
	plainTerminal(t)
	var requests atomic.Int32
	refusing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "unexpected request", http.StatusTeapot)
	}))
	t.Cleanup(refusing.Close)
	offline := env{hub: refusing.URL, task: "tsk_0000000000000001"}
	args := func(extra ...string) []string {
		return append([]string{"intervene", "--kind", "decision-on-behalf", "--item", "wi_0000000000000001", "--text", "Chose the release window"}, extra...)
	}
	refuse := func(name string, a []string, want string) {
		t.Helper()
		if err := cmdOwner(offline, a); err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "usage: tt owner intervene") {
			t.Fatalf("%s: err=%v, want %q", name, err, want)
		}
	}
	refuse("plain terminal claims helper", args("--actor", "helper"), "a plain terminal records the owner in person")
	refuse("bogus actor", args("--actor", "bogus"), "--actor must be owner or helper")
	refuse("explicit unknown", args("--actor", "unknown"), "--actor must be owner or helper")
	for _, v := range []string{"CLAUDE_CODE_SESSION_ID", "CODEX_THREAD_ID"} {
		t.Setenv(v, "session-under-test")
		refuse(v+" without --actor", args(), "pass --actor owner when recording what the owner did in person, or --actor helper")
		refuse(v+" bogus actor", args("--actor", "bogus"), "--actor must be owner or helper")
		t.Setenv(v, "")
	}
	if n := requests.Load(); n != 0 {
		t.Fatalf("refused commands sent %d requests", n)
	}

	e, c, task, _ := cliWorkItemFixture(t)
	e.agent, e.agentName = "", ""
	ctx := context.Background()
	item, err := c.CreateWorkItem(ctx, task.ID, api.CreateWorkItemRequest{Kind: "feature", Title: "Actor item", RequestID: "cli-actor-item"})
	if err != nil {
		t.Fatal(err)
	}
	// Derived request IDs are per minute; start clear of a minute boundary.
	if d := time.Until(time.Now().Truncate(time.Minute).Add(time.Minute)); d < 10*time.Second {
		time.Sleep(d)
	}
	record := func(want string, extra ...string) {
		t.Helper()
		a := append([]string{"intervene", "--kind", "decision-on-behalf", "--item", item.ID, "--text", "Chose the release window"}, extra...)
		out, err := captureCLIOutput(t, func() error { return cmdOwner(e, a) })
		if err != nil || !strings.Contains(out, "decision-on-behalf on "+item.ID+" by "+want+" (no product item)") {
			t.Fatalf("record %v: %q %v", extra, out, err)
		}
	}
	record("owner")                     // plain terminal, no flag
	record("owner", "--actor", "owner") // same minute, same actor: a retry, not a record
	t.Setenv("CLAUDE_CODE_SESSION_ID", "session-under-test")
	record("helper", "--actor", "helper") // same text and minute, other actor: a new record
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	t.Setenv("CODEX_THREAD_ID", "session-under-test")
	record("owner", "--actor", "owner")
	messages, err := c.ListMessages(ctx, task.ID, 0, "", 50)
	if err != nil || len(messages) != 2 {
		t.Fatalf("board: %d messages %v", len(messages), err)
	}
	var actors []string
	for _, m := range messages {
		actors = append(actors, m.Intervention.Actor)
	}
	if !reflect.DeepEqual(actors, []string{"owner", "helper"}) {
		t.Fatalf("recorded actors = %v", actors)
	}
}

// A hub older than the actor drops --actor and replies without one: the line
// says the actor was not recorded, never echoes the requested value.
func TestOwnerInterveneSaysActorNotRecordedOnOlderHub(t *testing.T) {
	for _, tc := range []struct{ name, reply string }{
		{"intervention without actor", `{"seq":9,"intervention":{"kind":"decision-on-behalf","itemTaskId":"tsk_0000000000000001","itemId":"wi_0000000000000001"}}`},
		{"no intervention", `{"seq":9}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plainTerminal(t)
			t.Setenv("CLAUDE_CODE_SESSION_ID", "session-under-test")
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.reply))
			}))
			t.Cleanup(srv.Close)
			out, err := captureCLIOutput(t, func() error {
				return cmdOwner(env{hub: srv.URL, task: "tsk_0000000000000001"}, []string{"intervene", "--kind", "decision-on-behalf",
					"--item", "wi_0000000000000001", "--text", "Chose the release window", "--actor", "helper"})
			})
			if err != nil || out != "intervention #9 recorded: decision-on-behalf on wi_0000000000000001 actor not recorded (no product item)\n" {
				t.Fatalf("older hub: %q %v", out, err)
			}
		})
	}
}

func TestOwnerInterventionsPrintsActorSplit(t *testing.T) {
	plainTerminal(t)
	e, c, task, _ := cliWorkItemFixture(t)
	e.agent, e.agentName = "", ""
	ctx := context.Background()
	item, err := c.CreateWorkItem(ctx, task.ID, api.CreateWorkItemRequest{Kind: "feature", Title: "Split item", RequestID: "cli-split-item"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.CreateIntervention(ctx, task.ID, api.CreateInterventionRequest{Kind: "nudge", ItemID: item.ID, Text: "Owner nudge", RequestID: "split-owner", Actor: "owner"}); err != nil {
		t.Fatal(err)
	}
	if _, err = c.CreateIntervention(ctx, task.ID, api.CreateInterventionRequest{Kind: "cleanup", ItemID: item.ID, Text: "Helper cleanup", RequestID: "split-helper", Actor: "helper"}); err != nil {
		t.Fatal(err)
	}
	out, err := captureCLIOutput(t, func() error { return cmdOwner(e, []string{"interventions", "--task", task.ID, "--tz", "UTC"}) })
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"by actor: owner 1, helper 1, unknown 0\n",
		"last 6 h: 2 (owner 1, helper 1, unknown 0)\n",
		"last 24 h: 2 (owner 1, helper 1, unknown 0)\n",
		"  2  linked 0  owner 1  helper 1  unknown 0  nudge 1, cleanup 1\n",
		"  helper  cleanup  " + item.ID,
		"  owner  nudge  " + item.ID,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output lacks %q:\n%s", want, out)
		}
	}
}

// A hub older than the actor sends no byActor or windows: the split is
// unavailable, never shown as zeros.
func TestOwnerInterventionsSaysActorSplitUnavailableOnOlderHub(t *testing.T) {
	plainTerminal(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"interventions":[],"summary":{"timeZone":"UTC","total":1,"linked":0,"byKind":{"nudge":1},` +
			`"days":[{"day":"2026-10-28","total":1,"linked":0,"byKind":{"nudge":1}}],` +
			`"unlinked":[{"seq":7,"kind":"nudge","itemId":"wi_0000000000000001","day":"2026-10-28"}]}}`))
	}))
	t.Cleanup(srv.Close)
	out, err := captureCLIOutput(t, func() error {
		return cmdOwner(env{hub: srv.URL}, []string{"interventions", "--task", "tsk_0000000000000001", "--tz", "UTC"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "by actor: unavailable") || strings.Contains(out, "owner 0") || strings.Contains(out, "last 6 h") ||
		!strings.Contains(out, "2026-10-28    1  linked 0  nudge 1\n") || !strings.Contains(out, "  #7  2026-10-28  nudge  wi_0000000000000001\n") {
		t.Fatalf("older hub output:\n%s", out)
	}
}
