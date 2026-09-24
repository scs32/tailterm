package server

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/scs32/tailterm/hub/internal/api"
)

func testEnvelope() *api.Envelope {
	return &api.Envelope{Kind: "result", To: "lead", Subject: "Empty recipient fix passes its checks",
		Refs:     map[string]string{"commit": "abc1234"},
		Body:     api.EnvelopeBody{Outcome: "done", Status: map[string]string{"a1": "pass"}},
		Evidence: map[string]api.Evidence{"e1": {Type: "command", Value: "go test ./cmd/tt", Outcome: "ok"}}}
}

func TestPublicMessageSubjectsStillRejectHashes(t *testing.T) {
	c := newClient(t)
	task := c.task("strict subjects")
	agent := c.agent(task, "builder-41b1c632")
	path := "/v1/tasks/" + task.ID + "/messages"
	for _, subject := range []string{
		"Escalation for builder-41b1c632",
		"Released candidate abc1234 today",
		"Released candidate " + strings.Repeat("a", 20) + strings.Repeat("1", 20) + " today",
	} {
		for _, agentID := range []string{"", agent.ID} {
			e := &api.Envelope{Kind: api.EnvelopeKindNotice, Subject: subject, Body: api.EnvelopeBody{Text: "check"}}
			var rejected api.ErrorResponse
			if code := c.do("POST", path, api.PostMessageRequest{AgentID: agentID, Envelope: e}, &rejected); code != http.StatusBadRequest || rejected.Code != "invalid-envelope" {
				t.Fatalf("agent %q subject %q: status %d response %+v", agentID, subject, code, rejected)
			}
		}
	}
}

// Broker phase 1, criteria a3–a5 and a7 (docs/broker-phase-1.md).
func TestTypedMessagesStoreEnvelopeAndRenderedText(t *testing.T) {
	c := newClient(t)
	task := c.task("typed")
	lead := c.agent(task, "lead")
	builder := c.agent(task, "builder")
	path := "/v1/tasks/" + task.ID + "/messages"

	var posted api.Message
	req := api.PostMessageRequest{AgentID: builder.ID, To: lead.ID, Envelope: testEnvelope()}
	if code := c.do("POST", path, req, &posted); code != 201 {
		t.Fatalf("typed post: %d", code)
	}
	want := api.RenderText(*testEnvelope())
	if posted.Text != want || posted.Envelope == nil || posted.Envelope.Subject != testEnvelope().Subject {
		t.Fatalf("posted %+v", posted)
	}
	var list api.MessageList
	c.do("GET", path+"?after=0", nil, &list)
	if len(list.Messages) != 1 || list.Messages[0].Envelope == nil || list.Messages[0].Text != want ||
		list.Messages[0].Envelope.Evidence["e1"].Outcome != "ok" {
		t.Fatalf("listed %+v", list.Messages)
	}

	// Text that disagrees with the envelope is rejected with a problem list.
	var rejected api.ErrorResponse
	req.Text = "RESULT: something else entirely"
	if code := c.do("POST", path, req, &rejected); code != http.StatusBadRequest || rejected.Code != "invalid-envelope" ||
		len(rejected.Problems) != 1 || rejected.Problems[0].Field != "text" {
		t.Fatalf("mismatched text: %d %+v", code, rejected)
	}
	// Text equal to the rendering is accepted.
	req.Text = want
	if code := c.do("POST", path, req, nil); code != 201 {
		t.Fatalf("matching text: %d", code)
	}

	// An invalid envelope lists every problem.
	bad := testEnvelope()
	bad.Subject = "Released 43c1da3"
	bad.Body.Status = nil
	rejected = api.ErrorResponse{}
	if code := c.do("POST", path, api.PostMessageRequest{AgentID: builder.ID, Envelope: bad}, &rejected); code != http.StatusBadRequest {
		t.Fatalf("invalid envelope: %d", code)
	}
	fields := map[string]bool{}
	for _, p := range rejected.Problems {
		fields[p.Field] = true
	}
	if !fields["subject"] || !fields["body.status"] {
		t.Fatalf("problems %+v", rejected.Problems)
	}

	// Retries return the original message; a changed envelope under the same key conflicts.
	keyed := api.PostMessageRequest{AgentID: builder.ID, To: lead.ID, RequestID: "builder-result-1", Envelope: testEnvelope()}
	var first, second api.Message
	if code := c.do("POST", path, keyed, &first); code != 201 {
		t.Fatalf("keyed post: %d", code)
	}
	if code := c.do("POST", path, keyed, &second); code != 201 || second.Seq != first.Seq {
		t.Fatalf("retry: %d first %d second %d", code, first.Seq, second.Seq)
	}
	changed := keyed
	changed.Envelope = testEnvelope()
	changed.Envelope.Body.Status["a1"] = "fail"
	if code := c.do("POST", path, changed, nil); code != http.StatusConflict {
		t.Fatalf("changed envelope under same key: %d", code)
	}

	// Free-text agent posts are unchanged.
	var plain api.Message
	if code := c.do("POST", path, api.PostMessageRequest{AgentID: builder.ID, Text: "hey can someone look at the relay"}, &plain); code != 201 || plain.Envelope != nil {
		t.Fatalf("free text: %d %+v", code, plain)
	}
}

// Criterion a6: every post gets exactly one check row with the right form.
func TestMessageChecksClassifyEveryPost(t *testing.T) {
	c := newClient(t)
	task := c.task("checks")
	lead := c.agent(task, "lead")
	builder := c.agent(task, "builder")
	path := "/v1/tasks/" + task.ID + "/messages"
	post := func(req api.PostMessageRequest) api.Message {
		t.Helper()
		var m api.Message
		if code := c.do("POST", path, req, &m); code != 201 {
			t.Fatalf("post %+v: %d", req, code)
		}
		return m
	}
	typed := post(api.PostMessageRequest{AgentID: builder.ID, To: lead.ID, RequestID: "typed-1", Envelope: testEnvelope()})
	post(api.PostMessageRequest{AgentID: builder.ID, To: lead.ID, RequestID: "typed-1", Envelope: testEnvelope()}) // retry
	convention := post(api.PostMessageRequest{AgentID: builder.ID, Text: "QUESTION: Which status code for rejected posts\nQuestion: Should rejected posts return 422?"})
	invalid := post(api.PostMessageRequest{AgentID: builder.ID, Text: "RESULT: Released 43c1da3\nOutcome: done"})
	free := post(api.PostMessageRequest{AgentID: builder.ID, Text: "SAVED activation release under exact lead8726"})
	human := post(api.PostMessageRequest{Text: "please look at the relay"})

	c.st.EnableJevScoring()
	pending := post(api.PostMessageRequest{AgentID: builder.ID, Text: "hey lead, done"})

	checks, err := c.st.ListMessageChecks(context.Background(), task.ID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := map[int64][2]string{
		typed.Seq:      {api.CheckFormTyped, api.JevStatusDisabled},
		convention.Seq: {api.CheckFormTextConvention, api.JevStatusDisabled},
		invalid.Seq:    {api.CheckFormTextConventionInvalid, api.JevStatusDisabled},
		free.Seq:       {api.CheckFormFreeText, api.JevStatusDisabled},
		human.Seq:      {api.CheckFormHuman, api.JevStatusSkipped},
		pending.Seq:    {api.CheckFormFreeText, api.JevStatusPending},
	}
	if len(checks) != len(want) {
		t.Fatalf("got %d check rows, want %d: %+v", len(checks), len(want), checks)
	}
	for _, check := range checks {
		w := want[check.Seq]
		if check.Form != w[0] || check.JevStatus != w[1] {
			t.Errorf("seq %d: form %s jev %s, want %v", check.Seq, check.Form, check.JevStatus, w)
		}
		if check.Seq == invalid.Seq {
			fields := map[string]bool{}
			for _, p := range check.Problems {
				fields[p.Field] = true
			}
			if !fields["subject"] || !fields["body.status"] || !fields["evidence"] {
				t.Errorf("invalid convention problems %+v", check.Problems)
			}
		}
		if check.Seq == human.Seq && check.AgentID != "" || check.Seq == typed.Seq && check.AgentID != builder.ID {
			t.Errorf("seq %d agent %q", check.Seq, check.AgentID)
		}
	}
}

// Round-one blocker R2, hub side: the stated recipient must match the routing.
func TestTypedRecipientMustMatchRouting(t *testing.T) {
	c := newClient(t)
	task := c.task("recipients")
	lead := c.agent(task, "lead")
	builder := c.agent(task, "builder")
	path := "/v1/tasks/" + task.ID + "/messages"
	for _, req := range []api.PostMessageRequest{
		{AgentID: builder.ID, To: builder.ID, Envelope: testEnvelope()}, // envelope says lead
		{AgentID: builder.ID, Envelope: testEnvelope()},                 // unaddressed
	} {
		var rejected api.ErrorResponse
		if code := c.do("POST", path, req, &rejected); code != http.StatusBadRequest || len(rejected.Problems) != 1 || rejected.Problems[0].Field != "to" {
			t.Errorf("to=%q: %d %+v", req.To, code, rejected)
		}
	}
	byID := testEnvelope()
	byID.To = lead.ID
	if code := c.do("POST", path, api.PostMessageRequest{AgentID: builder.ID, To: lead.ID, Envelope: byID}, nil); code != 201 {
		t.Fatalf("recipient by id: %d", code)
	}
}

// Round-one blocker b1/R1 end to end: a typed message between two agents bound
// to the same item reaches the recipient's filtered inbox when it carries the
// item link tt send now adds, and is hidden without it.
func TestTypedMessageReachesBoundRecipient(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	var task api.Task
	if code := c.do("POST", "/v1/tasks", api.CreateTaskRequest{Name: "Bound typed", Orchestrator: "lead"}, &task); code != 201 {
		t.Fatalf("create task: %d", code)
	}
	lead := c.agent(task, "lead")
	item, err := c.st.CreateWorkItem(ctx, task.ID, api.CreateWorkItemRequest{Kind: "feature", Title: "Typed fixture", AgentID: lead.ID, RequestID: "typed-item"}, c.who)
	if err != nil {
		t.Fatal(err)
	}
	link := []api.MessageWorkItem{{ItemTaskID: task.ID, ItemID: item.ID, ItemRevision: item.Revision, Relationship: "primary"}}
	order, err := c.st.PostMessage(ctx, task.ID, api.PostMessageRequest{Text: "Typed order", RequestID: "typed-order", WorkItems: link}, c.who)
	if err != nil {
		t.Fatal(err)
	}
	orderRef := api.MessageReference{TaskID: task.ID, Seq: order.Seq}
	bind := func(name, role string) api.Agent {
		t.Helper()
		var a api.Agent
		req := api.AddAgentRequest{AgentID: api.NewID("agt"), Name: name, Host: "fixture", Session: name, Runtime: "codex",
			WorkItem: &api.AgentWorkItemRequest{ItemTaskID: task.ID, ItemID: item.ID, ItemRevision: item.Revision, WorkOrderMessage: orderRef,
				ContextBundle: syntheticServerTestContext(t, item, orderRef, order), TeamRole: role}}
		if code := c.do("POST", "/v1/tasks/"+task.ID+"/agents", req, &a); code != 201 {
			t.Fatalf("bind %s: %d", name, code)
		}
		return a
	}
	builder := bind("builder", "")
	reviewer := bind("reviewer", api.TeamRoleMember)

	env := testEnvelope()
	env.To = "reviewer"
	unlinked, err := c.st.PostMessage(ctx, task.ID, api.PostMessageRequest{AgentID: builder.ID, To: reviewer.ID, Envelope: env}, c.who)
	if err != nil {
		t.Fatal(err)
	}
	linked, err := c.st.PostMessage(ctx, task.ID, api.PostMessageRequest{AgentID: builder.ID, To: reviewer.ID, Envelope: env,
		RequestID: "typed-linked", AuditKind: api.MessageAuditWork, WorkItems: link, WorkOrderMessage: &orderRef}, c.who)
	if err != nil {
		t.Fatal(err)
	}
	inbox, err := c.st.ListMessages(ctx, task.ID, order.Seq, reviewer.ID, 50)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[int64]bool{}
	for _, m := range inbox {
		seen[m.Seq] = true
	}
	if !seen[linked.Seq] || seen[unlinked.Seq] {
		t.Fatalf("bound inbox: linked %v (want true), unlinked %v (want false)", seen[linked.Seq], seen[unlinked.Seq])
	}
}
