package store

import (
	"context"
	"fmt"
	"github.com/scs32/tailterm/hub/internal/api"
	"testing"
	"time"
)

func TestOwnerRequestCreation(t *testing.T) {
	f := newDeliveryFixture(t)
	req := api.PostMessageRequest{AgentID: f.worker.ID, RunID: f.worker.RunID, RequestID: "owner-request-test", Envelope: &api.Envelope{Kind: api.EnvelopeKindRequest, Subject: "Approve the frozen verification bytes", Body: api.EnvelopeBody{Ask: "Please approve"}}, WorkItems: []api.MessageWorkItem{{ItemTaskID: f.item.TaskID, ItemID: f.item.ID, ItemRevision: f.item.Revision, Relationship: "primary"}}}
	m, err := f.s.PostMessage(context.Background(), f.task.ID, req, f.by)
	if err != nil {
		t.Fatal(err)
	}
	list, err := f.s.ListObligations(context.Background(), f.task.ID, ObligationFilter{OpenOnly: true}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("board-wide agent REQUEST produced %d obligations; want one owner obligation for #%d", len(list), m.Seq)
	}
}

func ownerFixtureRequest(t *testing.T, f deliveryFixture, to, key, due, expected string) api.Message {
	t.Helper()
	m, err := f.s.PostMessage(context.Background(), f.task.ID, api.PostMessageRequest{AgentID: f.worker.ID, RunID: f.worker.RunID, To: to, RequestID: key, Envelope: &api.Envelope{Kind: api.EnvelopeKindRequest, To: to, Subject: "Approve the frozen verification bytes", Due: due, ExpectedAnswer: expected, Body: api.EnvelopeBody{Ask: "Please approve"}}, WorkItems: []api.MessageWorkItem{{ItemTaskID: f.item.TaskID, ItemID: f.item.ID, ItemRevision: f.item.Revision, Relationship: "primary"}}}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	return m
}
func ownerFor(t *testing.T, f deliveryFixture, seq int64) api.Obligation {
	t.Helper()
	list, err := f.s.ListObligations(context.Background(), f.task.ID, ObligationFilter{OwnerOnly: true}, f.s.now())
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range list {
		if o.MessageSeq == seq {
			return o
		}
	}
	t.Fatalf("missing owner request %d", seq)
	return api.Obligation{}
}
func TestOwnerCreationRetryAndReopen(t *testing.T) {
	f := newDeliveryFixture(t)
	for _, to := range []string{"", "owner"} {
		key := "retry-" + to
		m := ownerFixtureRequest(t, f, to, key, "", "")
		again := ownerFixtureRequest(t, f, to, key, "", "")
		if again.Seq != m.Seq {
			t.Fatal("retry stored another message")
		}
		o := ownerFor(t, f, m.Seq)
		if o.RecipientKind != "owner" || o.AgentID != "" || o.DueAt.Sub(m.CreatedAt) != 30*time.Minute || o.Request.Text != m.Text {
			t.Fatalf("owner %+v", o)
		}
		if ObligationOverdue(o, o.DueAt.Add(-time.Nanosecond)) != "" || ObligationOverdue(o, o.DueAt) != "outcome" {
			t.Fatal("deadline boundary")
		}
	}
	m := ownerFixtureRequest(t, f, "owner", "override", "7m", "")
	o := ownerFor(t, f, m.Seq)
	if o.DueAt.Sub(m.CreatedAt) != 7*time.Minute {
		t.Fatal("override")
	}
	var wakes int
	f.s.db.QueryRow(`SELECT count(*) FROM wake_jobs`).Scan(&wakes)
	if wakes != 0 {
		t.Fatalf("owner queued %d wakes", wakes)
	}
	id := o.ID
	f.s.Close()
	s, err := Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	f.s = s
	if ownerFor(t, f, m.Seq).ID != id {
		t.Fatal("reopen changed id")
	}
}
func TestOwnerAnswerApprovalAndDelegate(t *testing.T) {
	f := newDeliveryFixture(t)
	ctx := context.Background()
	exact := " APPROVE 矩阵\nsha256:abc\n "
	m := ownerFixtureRequest(t, f, "owner", "approval", "", exact)
	o := ownerFor(t, f, m.Seq)
	// Ordinary agents' typed replies do not settle the owner obligation.
	_, err := f.s.PostMessage(ctx, f.task.ID, api.PostMessageRequest{AgentID: f.worker.ID, RunID: f.worker.RunID, ReplyTo: m.Seq, Envelope: &api.Envelope{Kind: api.EnvelopeKindAnswer, Subject: "An ordinary agent supplied an answer", Body: api.EnvelopeBody{Answer: "yes"}}}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	if ownerFor(t, f, m.Seq).State == api.ObligationClosed {
		t.Fatal("agent closed owner obligation")
	}
	if _, err := f.s.AnswerObligation(ctx, f.task.ID, o.ID, api.ObligationAnswerRequest{Text: "yes", DelegateSession: "ungranted", RequestID: "no-grant"}, f.by); err == nil {
		t.Fatal("ungranted delegate answered")
	}
	grant, err := f.s.DelegateOwnerObligation(ctx, f.task.ID, o.ID, api.OwnerDelegationRequest{Session: "owner-claude-session", AgentID: f.worker.ID, RunID: f.worker.RunID, AuthorizationRef: "owner explicit decision", RequestID: "grant"}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	req := api.ObligationAnswerRequest{Approve: true, DelegateSession: "owner-claude-session", AgentID: f.worker.ID, RunID: f.worker.RunID, RequestID: "approve"}
	stale := req
	stale.RunID = "stale"
	stale.RequestID = "stale"
	if _, err := f.s.AnswerObligation(ctx, f.task.ID, o.ID, stale, f.by); err == nil {
		t.Fatal("stale delegate answered")
	}
	result, err := f.s.AnswerObligation(ctx, f.task.ID, o.ID, req, f.by)
	if err != nil {
		t.Fatal(err)
	}
	if result.Message.Envelope.Body.Answer != exact || result.Message.Envelope.Refs["delegation"] != grant.DelegationID || result.Message.To != f.worker.ID || result.Obligation.State != api.ObligationClosed {
		t.Fatalf("answer %+v", result)
	}
	retry, err := f.s.AnswerObligation(ctx, f.task.ID, o.ID, req, f.by)
	if err != nil || !retry.Replay || retry.Message.Seq != result.Message.Seq {
		t.Fatalf("retry %+v %v", retry, err)
	}
}
func TestOwnerLinkedHumanReply(t *testing.T) {
	f := newDeliveryFixture(t)
	m := ownerFixtureRequest(t, f, "", "human-reply", "", "")
	reply, err := f.s.PostMessage(context.Background(), f.task.ID, api.PostMessageRequest{ReplyTo: m.Seq, Text: "Approved", RequestID: "human-answer"}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	o := ownerFor(t, f, m.Seq)
	if o.State != api.ObligationClosed || o.OutcomeSeq != reply.Seq || reply.To != f.worker.ID || len(reply.WorkItems) != 1 {
		t.Fatalf("reply %+v obligation %+v", reply, o)
	}
	list, err := f.s.ListObligations(context.Background(), f.task.ID, ObligationFilter{OpenOnly: true}, f.s.now())
	if err != nil || len(list) != 0 {
		t.Fatalf("reply created obligations %+v %v", list, err)
	}
}
func TestOwnerEscalationOnce(t *testing.T) {
	f := newDeliveryFixture(t)
	m := ownerFixtureRequest(t, f, "owner", "overdue", "", "")
	ctx := context.Background()
	open, err := f.s.BrokerOpenObligations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	o := open[0]
	if err := f.s.BrokerEscalateOwnerRequest(ctx, o, o.DueAt.Add(-time.Nanosecond)); err == nil {
		t.Fatal("escalated before due")
	}
	if err := f.s.BrokerEscalateOwnerRequest(ctx, o, o.DueAt); err != nil {
		t.Fatal(err)
	}
	if err := f.s.BrokerEscalateOwnerRequest(ctx, o, o.DueAt); err == nil {
		t.Fatal("stale scheduler escalated twice")
	}
	_, err = f.s.ExtendObligation(ctx, f.task.ID, o.ID, api.ObligationExtendRequest{For: "2h", Reason: "owner requested", RequestID: "extend"}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	next, _ := f.s.BrokerOpenObligations(ctx)
	if next[0].Escalation != 2 {
		t.Fatal("extension rearmed owner escalation")
	}
	msgs, _ := f.s.ListMessages(ctx, f.task.ID, 0, "", 0)
	count := 0
	for _, x := range msgs {
		if x.Envelope != nil && x.Envelope.Refs["escalation"] == "owner" {
			count++
			if len(x.WorkItems) != 1 || x.Envelope.Refs["message"] != fmt.Sprint(m.Seq) {
				t.Fatal("lost escalation links")
			}
		}
	}
	if count != 1 {
		t.Fatal(count)
	}
}

func TestOwnerControlsAndBoundContext(t *testing.T) {
	f := newDeliveryFixture(t)
	ctx := context.Background()
	// Exact run binding supplies the primary item without guesses or explicit links.
	m, err := f.s.PostMessage(ctx, f.task.ID, api.PostMessageRequest{AgentID: f.worker.ID, RunID: f.worker.RunID, RequestID: "bound-owner", Envelope: &api.Envelope{Kind: "request", Subject: "Answer the exact bound item request", Body: api.EnvelopeBody{Ask: "please"}}}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	if len(ownerFor(t, f, m.Seq).Request.WorkItems) != 1 {
		t.Fatal("bound item missing")
	}
	for i, req := range []api.PostMessageRequest{
		{Envelope: &api.Envelope{Kind: "request", Subject: "A human board request stays human", Body: api.EnvelopeBody{Ask: "please"}}},
		{AgentID: f.worker.ID, RunID: f.worker.RunID, Envelope: &api.Envelope{Kind: "notice", Subject: "A board notice stays ordinary news", Body: api.EnvelopeBody{Text: "news"}}},
	} {
		req.RequestID = fmt.Sprintf("control-%d", i)
		if _, err := f.s.PostMessage(ctx, f.task.ID, req, f.by); err != nil {
			t.Fatal(err)
		}
	}
	all, _ := f.s.ListObligations(ctx, f.task.ID, ObligationFilter{}, f.s.now())
	if len(all) != 1 {
		t.Fatalf("controls created %+v", all)
	}
	if _, err := f.s.NudgeObligation(ctx, f.task.ID, all[0].ID, "nudge-owner", f.by); err == nil {
		t.Fatal("owner request woke as an agent")
	}
	if _, err := f.s.ReassignObligation(ctx, f.task.ID, all[0].ID, api.ObligationReassignRequest{ToAgentID: f.lead.ID}, f.by); err == nil {
		t.Fatal("owner request became agent work")
	}
}
func TestOwnerConcurrentAnswersHaveOneOutcome(t *testing.T) {
	f := newDeliveryFixture(t)
	m := ownerFixtureRequest(t, f, "owner", "race", "", "")
	o := ownerFor(t, f, m.Seq)
	results := make(chan error, 2)
	for _, key := range []string{"answer-one", "answer-two"} {
		go func(key string) {
			_, err := f.s.AnswerObligation(context.Background(), f.task.ID, o.ID, api.ObligationAnswerRequest{Text: key, RequestID: key}, f.by)
			results <- err
		}(key)
	}
	success := 0
	for i := 0; i < 2; i++ {
		if <-results == nil {
			success++
		}
	}
	if success != 1 {
		t.Fatalf("successes %d", success)
	}
	all, _ := f.s.ListMessages(context.Background(), f.task.ID, 0, "", 0)
	answers := 0
	for _, x := range all {
		if x.ReplyTo == m.Seq {
			answers++
		}
	}
	if answers != 1 {
		t.Fatalf("answers %d", answers)
	}
}
func TestOwnerWithdrawPreservesAuthorityAndRetry(t *testing.T) {
	f := newDeliveryFixture(t)
	ctx := context.Background()
	m := ownerFixtureRequest(t, f, "owner", "withdraw", "", "")
	o := ownerFor(t, f, m.Seq)
	req := api.ObligationWithdrawRequest{AgentID: f.worker.ID, RunID: f.worker.RunID, Reason: "superseded owner request", RequestID: "withdraw"}
	wrong := req
	wrong.AgentID = f.lead.ID
	wrong.RunID = f.lead.RunID
	if _, err := f.s.WithdrawObligation(ctx, f.task.ID, o.ID, wrong); err == nil {
		t.Fatal("other sender withdrew")
	}
	stale := req
	stale.RunID = "run_0000000000000000"
	if _, err := f.s.WithdrawObligation(ctx, f.task.ID, o.ID, stale); err == nil {
		t.Fatal("stale sender withdrew")
	}
	got, err := f.s.WithdrawObligation(ctx, f.task.ID, o.ID, req)
	if err != nil || got.State != api.ObligationClosed || got.Outcome != api.OutcomeWithdrawn {
		t.Fatalf("withdraw %+v %v", got, err)
	}
	again, err := f.s.WithdrawObligation(ctx, f.task.ID, o.ID, req)
	if err != nil || again.ID != got.ID {
		t.Fatal("retry", err)
	}
	open, _ := f.s.BrokerOpenObligations(ctx)
	if len(open) != 0 {
		t.Fatalf("withdraw left broker obligations %+v", open)
	}
	msgs, _ := f.s.ListMessages(ctx, f.task.ID, 0, "", 0)
	notices := 0
	for _, x := range msgs {
		if x.Envelope != nil && x.Envelope.Refs["obligation"] == o.ID {
			notices++
			if len(x.WorkItems) != 1 || x.To != "" {
				t.Fatal("withdrawal notice lost links or invented recipient")
			}
		}
	}
	if notices != 1 {
		t.Fatal(notices)
	}
}
