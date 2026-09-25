package main

import (
	"context"
	"strings"
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
			if tc.name == "extend" && !got.DueAt.After(time.Now().Add(25*time.Minute)) {
				t.Fatalf("extension did not persist: %s", got.DueAt)
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
