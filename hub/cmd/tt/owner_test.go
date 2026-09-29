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

func TestOwnerInterveneRecordsAndListsInterventions(t *testing.T) {
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
	if err != nil || !strings.HasPrefix(out, "intervention #") || !strings.Contains(out, "nudge on "+item.ID+" (product fix "+product.ID+")") {
		t.Fatalf("intervene output %q: %v", out, err)
	}
	messages, err := c.ListMessages(ctx, task.ID, 0, "", 50)
	if err != nil || len(messages) != 1 {
		t.Fatalf("board: %+v %v", messages, err)
	}
	m := messages[0]
	want := api.Intervention{Kind: "nudge", ItemTaskID: task.ID, ItemID: item.ID, ProductTaskID: home.ID, ProductItemID: product.ID}
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
	if err != nil || !strings.Contains(out, "2 interventions (UTC); 1 of 2 linked to a product item") || !strings.Contains(out, "by kind: release 1, nudge 1") || !strings.Contains(out, "unlinked:") || !strings.Contains(out, "release  "+item.ID) {
		t.Fatalf("interventions output %q: %v", out, err)
	}
	if err = cmdOwner(e, []string{"interventions", "--tz", "Mars/Olympus"}); err == nil {
		t.Fatal("invalid time zone accepted")
	}
}

func TestOwnerInterveneRefusesAgentsAndBadInputBeforeAnyRequest(t *testing.T) {
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
