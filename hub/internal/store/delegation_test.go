package store

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

type delegationFixture struct {
	deliveryFixture
	other api.Agent
	clock *time.Time
}

func newDelegationFixture(t *testing.T) delegationFixture {
	t.Helper()
	f := newDeliveryFixture(t)
	clock := f.s.now()
	f.s.now = func() time.Time { return clock }
	other, err := f.s.AddAgent(context.Background(), f.task.ID, api.AddAgentRequest{Name: "other", Host: "fixture", Session: "other", Runtime: "codex"}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	return delegationFixture{deliveryFixture: f, other: other, clock: &clock}
}

func (f delegationFixture) advance(d time.Duration) { *f.clock = f.clock.Add(d) }

func (f delegationFixture) open(t *testing.T, key, scope string, d time.Duration) api.DelegationWindow {
	t.Helper()
	out, err := f.s.OpenDelegationWindow(context.Background(), f.task.ID, api.OpenDelegationWindowRequest{Delegate: f.lead.Name, EndsAt: f.s.now().Add(d), Scope: scope, Source: &api.DelegationSource{Kind: api.DelegationSourceTT}, RequestID: key}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	if out.Window == nil || out.Window.State != api.DelegationOpen || out.Window.DelegateAgentID != f.lead.ID {
		t.Fatalf("open result %+v", out)
	}
	return *out.Window
}

// ownerRequest posts a worker REQUEST to the owner with an optional category and expected answer.
func (f delegationFixture) ownerRequest(t *testing.T, key, category, expected string) api.Obligation {
	t.Helper()
	env := &api.Envelope{Kind: api.EnvelopeKindRequest, To: "owner", Subject: "Choose the release order for the fixture", ExpectedAnswer: expected, Body: api.EnvelopeBody{Ask: "Which order?"}}
	if category != "" {
		env.Refs = map[string]string{"category": category}
	}
	m, err := f.s.PostMessage(context.Background(), f.task.ID, api.PostMessageRequest{AgentID: f.worker.ID, RunID: f.worker.RunID, To: "owner", RequestID: key, Envelope: env,
		WorkItems: []api.MessageWorkItem{{ItemTaskID: f.item.TaskID, ItemID: f.item.ID, ItemRevision: f.item.Revision, Relationship: "primary"}}}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	return ownerFor(t, f.deliveryFixture, m.Seq)
}

func (f delegationFixture) decision(t *testing.T, key, category string) api.Message {
	t.Helper()
	m, err := f.s.CreateDecision(context.Background(), f.task.ID, api.CreateDecisionRequest{AgentID: f.worker.ID, RequestID: key, DecisionRequest: api.DecisionRequest{
		Question: "Which rollout should the fixture use?", Category: category, RecommendedOptionID: "staged", RecommendationReason: "Limits impact.",
		Options: []api.DecisionOption{{ID: "staged", Label: "Staged", Description: "A few first."}, {ID: "all", Label: "All", Description: "Everyone at once."}}}}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func (f delegationFixture) window(t *testing.T) api.DelegationWindow {
	t.Helper()
	list, err := f.s.ListDelegationWindows(context.Background(), f.task.ID)
	if err != nil || len(list.Windows) == 0 {
		t.Fatalf("list %v %+v", err, list)
	}
	return list.Windows[0]
}

func routed(w api.DelegationWindow, kind string, seq int64) *api.DelegationRoute {
	for i := range w.Routes {
		if w.Routes[i].RequestKind == kind && w.Routes[i].RequestSeq == seq {
			return &w.Routes[i]
		}
	}
	return nil
}

// noticesTo counts hub notices directed to agentID that carry refs.delegation.
func (f delegationFixture) noticesTo(t *testing.T, agentID, windowID string) []api.Message {
	t.Helper()
	msgs, err := f.s.ListMessages(context.Background(), f.task.ID, 0, agentID, 500)
	if err != nil {
		t.Fatal(err)
	}
	var out []api.Message
	for _, m := range msgs {
		if m.Envelope != nil && m.Envelope.Kind == api.EnvelopeKindNotice && m.Envelope.Refs["delegation"] == windowID && m.To == agentID {
			out = append(out, m)
		}
	}
	return out
}

func (f delegationFixture) boardNotices(t *testing.T, windowID string) []api.Message {
	t.Helper()
	msgs, err := f.s.ListMessages(context.Background(), f.task.ID, 0, "", 500)
	if err != nil {
		t.Fatal(err)
	}
	var out []api.Message
	for _, m := range msgs {
		if m.Envelope != nil && m.To == "" && m.Envelope.Refs["delegation"] == windowID {
			out = append(out, m)
		}
	}
	return out
}

func (f delegationFixture) delegateAnswer(key, obligation, text, rationale string, agent api.Agent) (api.OwnerActionResult, error) {
	return f.s.AnswerObligation(context.Background(), f.task.ID, obligation, api.ObligationAnswerRequest{AgentID: agent.ID, RunID: agent.RunID, Text: text, Rationale: rationale, RequestID: key}, f.by)
}

func TestDelegationCategoriesAndScope(t *testing.T) {
	cases := []struct {
		env  *api.Envelope
		want string
	}{
		{&api.Envelope{Body: api.EnvelopeBody{Ask: "ok"}}, "decision"},
		{&api.Envelope{Refs: map[string]string{"category": "deploy"}}, "deploy"},
		{&api.Envelope{Refs: map[string]string{"category": "merge"}}, "merge"},
		{&api.Envelope{Refs: map[string]string{"category": "decision"}, ExpectedAnswer: "verification-matrix-approval:abc"}, "matrix"},
		{&api.Envelope{Refs: map[string]string{"category": "deploy"}, Body: api.EnvelopeBody{Ask: "please give verification-matrix-approval:abc"}}, "matrix"},
		{&api.Envelope{Refs: map[string]string{"category": "matrix"}}, "matrix"},
		{&api.Envelope{Refs: map[string]string{"category": "release"}}, "release"},
	}
	for _, c := range cases {
		if got := api.OwnerRequestCategory(c.env); got != c.want {
			t.Fatalf("category %+v = %s, want %s", c.env, got, c.want)
		}
	}
	d := api.DecisionRequest{Question: "Q", Options: []api.DecisionOption{{ID: "a", Label: "A", Description: "verification-matrix-approval:x"}}}
	if api.DecisionCategory(d) != "matrix" || api.DecisionCategory(api.DecisionRequest{Category: "merge"}) != "merge" || api.DecisionCategory(api.DecisionRequest{}) != "decision" {
		t.Fatal("decision category")
	}
	for _, c := range []struct {
		scope, category string
		want            bool
	}{
		{"decisions", "decision", true}, {"decisions", "merge", false}, {"decisions", "deploy", false}, {"decisions", "matrix", false},
		{"decisions_merges_deploys", "decision", true}, {"decisions_merges_deploys", "merge", true}, {"decisions_merges_deploys", "deploy", true},
		{"decisions_merges_deploys", "matrix", false}, {"decisions_merges_deploys", "release", false}, {"bogus", "decision", false},
	} {
		if api.DelegationScopeCovers(c.scope, c.category) != c.want {
			t.Fatalf("scope %s category %s", c.scope, c.category)
		}
	}
	if api.NormalizeDelegationScope("decisions-merges-deploys") != api.DelegationScopeDecisionsMergesDeploys {
		t.Fatal("normalize")
	}
	if err := api.ValidateDecisionRequest(api.DecisionRequest{Question: "Q", Category: "release", RecommendedOptionID: "a", RecommendationReason: "r",
		Options: []api.DecisionOption{{ID: "a", Label: "A", Description: "a"}, {ID: "b", Label: "B", Description: "b"}}}); !errors.Is(err, api.ErrInvalid) {
		t.Fatal("unknown decision category accepted")
	}
}

func TestDelegationOpenValidationReplayAndConflict(t *testing.T) {
	f := newDelegationFixture(t)
	ctx := context.Background()
	now := f.s.now()
	closed, err := f.s.AddAgent(ctx, f.task.ID, api.AddAgentRequest{Name: "gone", Host: "fixture", Session: "gone", Runtime: "codex"}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.Exec(`UPDATE agents SET status=? WHERE id=?`, api.AgentClosed, closed.ID); err != nil {
		t.Fatal(err)
	}
	bad := []api.OpenDelegationWindowRequest{
		{Delegate: f.lead.Name, EndsAt: now.Add(time.Hour), Scope: "everything"},
		{Delegate: f.lead.Name, EndsAt: now.Add(30 * time.Second), Scope: "decisions"},
		{Delegate: f.lead.Name, EndsAt: now.Add(8 * 24 * time.Hour), Scope: "decisions"},
		{Delegate: "nobody", EndsAt: now.Add(time.Hour), Scope: "decisions"},
		{Delegate: closed.ID, EndsAt: now.Add(time.Hour), Scope: "decisions"},
		{Delegate: "", EndsAt: now.Add(time.Hour), Scope: "decisions"},
		{Delegate: f.lead.Name, EndsAt: now.Add(time.Hour), Scope: "decisions", Source: &api.DelegationSource{Kind: "discord", ID: "x"}},
	}
	for i, req := range bad {
		req.RequestID = "bad-" + string(rune('a'+i))
		if _, err := f.s.OpenDelegationWindow(ctx, f.task.ID, req, f.by); !errors.Is(err, api.ErrInvalid) {
			t.Fatalf("bad %d accepted: %v", i, err)
		}
	}
	w := f.open(t, "open-1", api.DelegationScopeDecisions, 2*time.Hour)
	if !w.EndsAt.Equal(now.Add(2*time.Hour)) || w.Scope != "decisions" || w.OpenedSource.Kind != "tt" || w.OpenedBy.User != "owner" {
		t.Fatalf("window %+v", w)
	}
	replay, err := f.s.OpenDelegationWindow(ctx, f.task.ID, api.OpenDelegationWindowRequest{Delegate: f.lead.Name, EndsAt: now.Add(2 * time.Hour), Scope: api.DelegationScopeDecisions, Source: &api.DelegationSource{Kind: api.DelegationSourceTT}, RequestID: "open-1"}, f.by)
	if err != nil || !replay.Replay || replay.Window.ID != w.ID {
		t.Fatalf("replay %v %+v", err, replay)
	}
	if _, err := f.s.OpenDelegationWindow(ctx, f.task.ID, api.OpenDelegationWindowRequest{Delegate: f.other.Name, EndsAt: now.Add(time.Hour), Scope: "decisions", RequestID: "open-2"}, f.by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("second open: %v", err)
	}
	if n := len(f.noticesTo(t, f.lead.ID, w.ID)); n != 1 {
		t.Fatalf("delegate got %d open notices", n)
	}
	if _, err := f.s.CloseDelegationWindow(ctx, f.task.ID, "dlw_0000000000000000", api.CloseDelegationWindowRequest{RequestID: "close-missing"}, f.by); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("close missing: %v", err)
	}
}

func TestDelegationRoutingScopeAndSelf(t *testing.T) {
	f := newDelegationFixture(t)
	before := f.ownerRequest(t, "before", "", "")
	beforeDecision := f.decision(t, "before-decision", "")
	w := f.open(t, "open", api.DelegationScopeDecisions, time.Hour)
	decision := f.ownerRequest(t, "decision", "decision", "")
	merge := f.ownerRequest(t, "merge", "merge", "")
	deploy := f.ownerRequest(t, "deploy", "deploy", "")
	matrix := f.ownerRequest(t, "matrix", "decision", "verification-matrix-approval:"+strings.Repeat("a", 64))
	askMerge := f.decision(t, "ask-merge", "merge")
	ask := f.decision(t, "ask", "")
	own, err := f.s.PostMessage(context.Background(), f.task.ID, api.PostMessageRequest{AgentID: f.lead.ID, RunID: f.lead.RunID, To: "owner", RequestID: "self", Envelope: &api.Envelope{Kind: api.EnvelopeKindRequest, To: "owner", Subject: "The delegate asks the owner itself", Body: api.EnvelopeBody{Ask: "Mine?"}},
		WorkItems: []api.MessageWorkItem{{ItemTaskID: f.item.TaskID, ItemID: f.item.ID, ItemRevision: f.item.Revision, Relationship: "primary"}}}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	got := f.window(t)
	want := map[int64]bool{before.MessageSeq: true, beforeDecision.Seq: true, decision.MessageSeq: true, ask.Seq: true}
	for _, seq := range []int64{merge.MessageSeq, deploy.MessageSeq, matrix.MessageSeq, askMerge.Seq, own.Seq} {
		want[seq] = false
	}
	for seq, routedWant := range want {
		kind := api.DelegationRouteObligation
		if seq == beforeDecision.Seq || seq == ask.Seq || seq == askMerge.Seq {
			kind = api.DelegationRouteDecision
		}
		if (routed(got, kind, seq) != nil) != routedWant {
			t.Fatalf("request #%d routed=%v, want %v; routes %+v", seq, !routedWant, routedWant, got.Routes)
		}
	}
	notices := f.noticesTo(t, f.lead.ID, w.ID)
	if len(notices) != 1+4 { // the open notice plus one per routed request
		t.Fatalf("delegate notices %d", len(notices))
	}
	for _, n := range notices[1:] {
		if !strings.Contains(n.Text, "--rationale") || n.Envelope.Refs["onBehalfOf"] != "owner" {
			t.Fatalf("notice %q", n.Text)
		}
	}
	// A wider window routes merges and deploys, still never matrix.
	if _, err := f.s.CloseDelegationWindow(context.Background(), f.task.ID, w.ID, api.CloseDelegationWindowRequest{RequestID: "close"}, f.by); err != nil {
		t.Fatal(err)
	}
	wide := f.open(t, "wide", api.DelegationScopeDecisionsMergesDeploys, time.Hour)
	got = f.window(t)
	if got.ID != wide.ID || routed(got, "obligation", merge.MessageSeq) == nil || routed(got, "obligation", deploy.MessageSeq) == nil || routed(got, "decision", askMerge.Seq) == nil ||
		routed(got, "obligation", matrix.MessageSeq) != nil || routed(got, "obligation", own.Seq) != nil {
		t.Fatalf("wide routes %+v", got.Routes)
	}
}

func TestDelegatedAnswersAndRefusals(t *testing.T) {
	f := newDelegationFixture(t)
	ctx := context.Background()
	o := f.ownerRequest(t, "req", "", "")
	merge := f.ownerRequest(t, "merge", "merge", "")
	matrix := f.ownerRequest(t, "matrix", "", "verification-matrix-approval:"+strings.Repeat("b", 64))
	ask := f.decision(t, "ask", "")
	// No window: agents cannot answer.
	if _, err := f.delegateAnswer("no-window", o.ID, "yes", "because", f.lead); !errors.Is(err, api.ErrDelegationForbidden) {
		t.Fatalf("no window: %v", err)
	}
	if _, err := f.s.AnswerDecision(ctx, f.task.ID, ask.Seq, api.AnswerDecisionRequest{RequestID: "no-window", AgentID: f.lead.ID, RunID: f.lead.RunID, OptionID: "staged", Rationale: "r"}, f.by); !errors.Is(err, ErrDecisionAnswerForbidden) {
		t.Fatalf("decision no window: %v", err)
	}
	w := f.open(t, "open", api.DelegationScopeDecisions, time.Hour)
	for _, c := range []struct {
		key, rationale string
		agent          api.Agent
		want           error
	}{
		{"blank", "  ", f.lead, api.ErrInvalid},
		{"missing", "", f.lead, api.ErrInvalid},
		{"long", strings.Repeat("x", api.MaxDelegationRationale+1), f.lead, api.ErrInvalid},
		{"other", "because", f.other, api.ErrDelegationForbidden},
		{"stale", "because", api.Agent{ID: f.lead.ID, RunID: "run_0000000000000000"}, api.ErrDelegationForbidden},
		{"token", "verification-matrix-approval:" + strings.Repeat("b", 64), f.lead, api.ErrConflict},
	} {
		if _, err := f.delegateAnswer(c.key, o.ID, "Go with A", c.rationale, c.agent); !errors.Is(err, c.want) {
			t.Fatalf("%s: %v", c.key, err)
		}
	}
	if _, err := f.delegateAnswer("merge", merge.ID, "Merge it", "because", f.lead); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("out of scope: %v", err)
	}
	if _, err := f.delegateAnswer("matrix", matrix.ID, "approve", "because", f.lead); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("matrix: %v", err)
	}
	if _, err := f.s.AnswerObligation(ctx, f.task.ID, o.ID, api.ObligationAnswerRequest{Text: "x", Rationale: "owner has none", RequestID: "owner-rationale"}, f.by); !errors.Is(err, api.ErrInvalid) {
		t.Fatalf("owner rationale: %v", err)
	}
	out, err := f.delegateAnswer("ok", o.ID, "Go with A", "A keeps the release reversible.", f.lead)
	if err != nil {
		t.Fatal(err)
	}
	m := out.Message
	if m.From.AgentID != f.lead.ID || m.To != f.worker.ID || m.ReplyTo != o.MessageSeq || m.Envelope.Refs["delegated"] != "true" || m.Envelope.Refs["delegation"] != w.ID ||
		m.Envelope.Refs["onBehalfOf"] != "owner" || m.Envelope.Body.Reason != "A keeps the release reversible." || m.Envelope.Body.Answer != "Go with A" {
		t.Fatalf("delegated answer %+v %+v", m, m.Envelope)
	}
	if out.Obligation.State != api.ObligationClosed || !strings.Contains(out.Obligation.Reason, "delegate lead") {
		t.Fatalf("obligation %+v", out.Obligation)
	}
	again, err := f.delegateAnswer("ok", o.ID, "Go with A", "A keeps the release reversible.", f.lead)
	if err != nil || !again.Replay {
		t.Fatalf("replay %v", err)
	}
	if _, err := f.delegateAnswer("twice", o.ID, "B", "because", f.lead); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("second answer: %v", err)
	}
	// Decisions: rationale required, delegated text, recommendation recorded.
	if _, err := f.s.AnswerDecision(ctx, f.task.ID, ask.Seq, api.AnswerDecisionRequest{RequestID: "d-blank", AgentID: f.lead.ID, RunID: f.lead.RunID, OptionID: "staged"}, f.by); !errors.Is(err, api.ErrInvalid) {
		t.Fatalf("decision blank rationale: %v", err)
	}
	if _, err := f.s.AnswerDecision(ctx, f.task.ID, ask.Seq, api.AnswerDecisionRequest{RequestID: "d-other", AgentID: f.other.ID, RunID: f.other.RunID, OptionID: "staged", Rationale: "r"}, f.by); !errors.Is(err, ErrDecisionAnswerForbidden) {
		t.Fatalf("decision other: %v", err)
	}
	answer, err := f.s.AnswerDecision(ctx, f.task.ID, ask.Seq, api.AnswerDecisionRequest{RequestID: "d-ok", AgentID: f.lead.ID, RunID: f.lead.RunID, OptionID: "all", Rationale: "Everyone needs it today."}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	if answer.From.AgentID != f.lead.ID || answer.To != f.worker.ID || answer.DecisionAnswer == nil || answer.Envelope.Refs["delegated"] != "true" || !strings.Contains(answer.Text, "Rationale: Everyone needs it today.") {
		t.Fatalf("decision answer %+v", answer)
	}
	replayed, err := f.s.AnswerDecision(ctx, f.task.ID, ask.Seq, api.AnswerDecisionRequest{RequestID: "d-ok", AgentID: f.lead.ID, RunID: f.lead.RunID, OptionID: "all", Rationale: "Everyone needs it today."}, f.by)
	if err != nil || replayed.Seq != answer.Seq {
		t.Fatalf("decision replay %v", err)
	}
	if _, err := f.s.AnswerDecision(ctx, f.task.ID, ask.Seq, api.AnswerDecisionRequest{RequestID: "owner-late", OptionID: "staged"}, f.by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("owner after delegate: %v", err)
	}
	// Listed with request, delegate, answer, rationale and time, also after the window ends.
	if _, err := f.s.CloseDelegationWindow(ctx, f.task.ID, w.ID, api.CloseDelegationWindowRequest{RequestID: "close"}, f.by); err != nil {
		t.Fatal(err)
	}
	got := f.window(t)
	r := routed(got, "obligation", o.MessageSeq)
	d := routed(got, "decision", ask.Seq)
	if got.State != api.DelegationClosed || r == nil || r.AnswerSeq != m.Seq || r.Answer != "Go with A" || r.Rationale != "A keeps the release reversible." || r.AnsweredByName != "lead" || r.AnsweredAt == nil || r.ReturnedAt != nil ||
		d == nil || d.AnswerSeq != answer.Seq || d.FollowedRecommendation == nil || *d.FollowedRecommendation || d.Rationale != "Everyone needs it today." {
		t.Fatalf("listing %+v", got.Routes)
	}
}

func TestDelegationExpiryHandBack(t *testing.T) {
	f := newDelegationFixture(t)
	ctx := context.Background()
	o := f.ownerRequest(t, "req", "", "")
	answered := f.ownerRequest(t, "answered", "", "")
	ask := f.decision(t, "ask", "")
	w := f.open(t, "open", api.DelegationScopeDecisions, time.Hour)
	if _, err := f.delegateAnswer("answered", answered.ID, "fine", "because", f.lead); err != nil {
		t.Fatal(err)
	}
	f.advance(time.Hour + time.Second)
	// Refused at answer time, before any broker pass.
	if _, err := f.delegateAnswer("late", o.ID, "late", "because", f.lead); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("late answer: %v", err)
	}
	if _, err := f.s.AnswerDecision(ctx, f.task.ID, ask.Seq, api.AnswerDecisionRequest{RequestID: "late-d", AgentID: f.lead.ID, RunID: f.lead.RunID, OptionID: "staged", Rationale: "r"}, f.by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("late decision: %v", err)
	}
	// Not routed after the end, even before the broker expires the window.
	late := f.ownerRequest(t, "late-request", "", "")
	expired, err := f.s.BrokerExpireDelegationWindows(ctx, f.s.now())
	if err != nil || len(expired) != 1 || expired[0] != w.ID {
		t.Fatalf("expire %v %v", expired, err)
	}
	got := f.window(t)
	if got.State != api.DelegationExpired || got.EndedAt == nil || routed(got, "obligation", late.MessageSeq) != nil ||
		routed(got, "obligation", o.MessageSeq).ReturnedAt == nil || routed(got, "decision", ask.Seq).ReturnedAt == nil || routed(got, "obligation", answered.MessageSeq).ReturnedAt != nil {
		t.Fatalf("expired window %+v", got)
	}
	delegateNotices := f.noticesTo(t, f.lead.ID, w.ID)
	board := f.boardNotices(t, w.ID)
	if len(board) != 1 || !strings.Contains(board[0].Text, "#"+itoa(o.MessageSeq)) || !strings.Contains(board[0].Text, "#"+itoa(ask.Seq)) || strings.Contains(board[0].Text, "#"+itoa(answered.MessageSeq)+" ") {
		t.Fatalf("board notices %+v", board)
	}
	last := delegateNotices[len(delegateNotices)-1]
	if last.Envelope.Refs["state"] != api.DelegationExpired || !strings.Contains(last.Text, "Do not answer") {
		t.Fatalf("delegate hand-back %q", last.Text)
	}
	// Idempotent across repeated passes and a reopen.
	f.advance(time.Minute)
	if again, err := f.s.BrokerExpireDelegationWindows(ctx, f.s.now()); err != nil || len(again) != 0 {
		t.Fatalf("second expire %v %v", again, err)
	}
	f.s.Close()
	s, err := Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if again, err := s.BrokerExpireDelegationWindows(ctx, f.s.now().Add(time.Hour)); err != nil || len(again) != 0 {
		t.Fatalf("expire after reopen %v %v", again, err)
	}
	f.s = s
	if len(f.boardNotices(t, w.ID)) != 1 || len(f.noticesTo(t, f.lead.ID, w.ID)) != len(delegateNotices) {
		t.Fatal("repeated expiry posted again")
	}
	// The owner still answers the returned request.
	if _, err := s.AnswerObligation(ctx, f.task.ID, o.ID, api.ObligationAnswerRequest{Text: "owner answer", RequestID: "owner"}, f.by); err != nil {
		t.Fatal(err)
	}
}

func TestDelegationOwnerCloseAndOverride(t *testing.T) {
	f := newDelegationFixture(t)
	ctx := context.Background()
	w := f.open(t, "open", api.DelegationScopeDecisions, time.Hour)
	a := f.ownerRequest(t, "a", "", "")
	b := f.ownerRequest(t, "b", "", "")
	ask := f.decision(t, "ask", "")
	// The owner answers a routed request; the delegate is then refused.
	if _, err := f.s.AnswerObligation(ctx, f.task.ID, a.ID, api.ObligationAnswerRequest{Text: "owner decides", RequestID: "owner-a"}, f.by); err != nil {
		t.Fatal(err)
	}
	if _, err := f.delegateAnswer("after-owner", a.ID, "x", "because", f.lead); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("after owner: %v", err)
	}
	if _, err := f.s.AnswerDecision(ctx, f.task.ID, ask.Seq, api.AnswerDecisionRequest{RequestID: "owner-ask", OptionID: "staged"}, f.by); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.AnswerDecision(ctx, f.task.ID, ask.Seq, api.AnswerDecisionRequest{RequestID: "late", AgentID: f.lead.ID, RunID: f.lead.RunID, OptionID: "all", Rationale: "r"}, f.by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("decision after owner: %v", err)
	}
	f.advance(10 * time.Minute)
	out, err := f.s.CloseDelegationWindow(ctx, f.task.ID, w.ID, api.CloseDelegationWindowRequest{Reason: "back at my desk", Source: &api.DelegationSource{Kind: api.DelegationSourceTailOS}, RequestID: "close"}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	got := *out.Window
	if got.State != api.DelegationClosed || !strings.Contains(got.EndReason, "back at my desk") || got.ClosedSource.Kind != "tailos" ||
		routed(got, "obligation", a.MessageSeq).ReturnedAt != nil || routed(got, "decision", ask.Seq).ReturnedAt != nil || routed(got, "obligation", b.MessageSeq).ReturnedAt == nil {
		t.Fatalf("closed %+v", got)
	}
	if replay, err := f.s.CloseDelegationWindow(ctx, f.task.ID, w.ID, api.CloseDelegationWindowRequest{Reason: "back at my desk", Source: &api.DelegationSource{Kind: api.DelegationSourceTailOS}, RequestID: "close"}, f.by); err != nil || !replay.Replay {
		t.Fatalf("close replay %v", err)
	}
	if _, err := f.s.CloseDelegationWindow(ctx, f.task.ID, w.ID, api.CloseDelegationWindowRequest{RequestID: "close-again"}, f.by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("close again: %v", err)
	}
	if _, err := f.delegateAnswer("after-close", b.ID, "x", "because", f.lead); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("after close: %v", err)
	}
	board := f.boardNotices(t, w.ID)
	if len(board) != 1 || !strings.Contains(board[0].Text, b.ID) || strings.Contains(board[0].Text, a.ID) {
		t.Fatalf("board %+v", board)
	}
	if expired, err := f.s.BrokerExpireDelegationWindows(ctx, f.s.now().Add(2*time.Hour)); err != nil || len(expired) != 0 {
		t.Fatal("closed window expired again")
	}
	// A new request after close stays with the owner, and a new window may open.
	c := f.ownerRequest(t, "c", "", "")
	if routed(f.window(t), "obligation", c.MessageSeq) != nil {
		t.Fatal("routed after close")
	}
	next := f.open(t, "reopen", api.DelegationScopeDecisions, time.Hour)
	if routed(f.window(t), "obligation", c.MessageSeq) == nil || routed(f.window(t), "obligation", b.MessageSeq) == nil || next.ID == w.ID {
		t.Fatal("new window did not route the owner's open requests")
	}
}

func TestDelegationGoneDelegateAndLazyExpiry(t *testing.T) {
	f := newDelegationFixture(t)
	ctx := context.Background()
	w := f.open(t, "open", api.DelegationScopeDecisions, time.Hour)
	if _, err := f.s.db.Exec(`UPDATE agents SET status=? WHERE id=?`, api.AgentClosed, f.lead.ID); err != nil {
		t.Fatal(err)
	}
	o := f.ownerRequest(t, "req", "", "")
	if routed(f.window(t), "obligation", o.MessageSeq) != nil {
		t.Fatal("routed to a closed delegate")
	}
	// A new window replaces one past its end without waiting for the broker.
	f.advance(2 * time.Hour)
	out, err := f.s.OpenDelegationWindow(ctx, f.task.ID, api.OpenDelegationWindowRequest{Delegate: f.other.Name, EndsAt: f.s.now().Add(time.Hour), Scope: "decisions", RequestID: "next"}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	list, _ := f.s.ListDelegationWindows(ctx, f.task.ID)
	if len(list.Windows) != 2 || list.Windows[1].ID != w.ID || list.Windows[1].State != api.DelegationExpired || routed(*out.Window, "obligation", o.MessageSeq) == nil {
		t.Fatalf("lazy expiry %+v", list)
	}
}

func TestDelegatedAnswerIsNeverAMatrixApproval(t *testing.T) {
	f, h, p := verificationFixture(t)
	req, err := f.s.PostMessage(f.ctx, f.task.ID, api.PostMessageRequest{AgentID: f.builder.ID, RunID: f.builder.RunID, To: "owner", RequestID: "ask-owner",
		Envelope:  &api.Envelope{Kind: api.EnvelopeKindRequest, To: "owner", Subject: "Choose the fixture option for the owner", Body: api.EnvelopeBody{Ask: "Which?"}},
		WorkItems: []api.MessageWorkItem{{ItemTaskID: f.item.TaskID, ItemID: f.item.ID, ItemRevision: f.item.Revision, Relationship: "primary"}}}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	delegate, err := f.s.AddAgent(f.ctx, f.task.ID, api.AddAgentRequest{Name: "delegate", Host: "fixture", Session: "delegate"}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.OpenDelegationWindow(f.ctx, f.task.ID, api.OpenDelegationWindowRequest{Delegate: delegate.ID, EndsAt: f.s.now().Add(time.Hour), Scope: api.DelegationScopeDecisionsMergesDeploys, RequestID: "open"}, f.by); err != nil {
		t.Fatal(err)
	}
	var obligation string
	if err := f.s.db.QueryRow(`SELECT id FROM obligations WHERE message_seq=?`, req.Seq).Scan(&obligation); err != nil {
		t.Fatal(err)
	}
	token := matrixApprovalPrefix + p.MatrixDigest
	if _, err := f.s.AnswerObligation(f.ctx, f.task.ID, obligation, api.ObligationAnswerRequest{AgentID: delegate.ID, RunID: delegate.RunID, Text: token, Rationale: "Safer.", RequestID: "delegate"}, f.by); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "a delegate cannot give one") {
		t.Fatal("a delegate answered with the matrix approval token", err)
	}
	// The token can still reach the board on the delegate's own post, whose
	// text matches exactly, so only the owner-author rule refuses it.
	post, err := f.s.PostMessage(f.ctx, f.task.ID, api.PostMessageRequest{AgentID: delegate.ID, RunID: delegate.RunID, Text: token, RequestID: "delegate-token"}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	if post.From.AgentID != delegate.ID || strings.TrimSpace(post.Text) != token {
		t.Fatalf("delegate token post %+v", post)
	}
	p.MatrixApprovalMessageSeq = post.Seq
	if _, err := f.s.SaveVerification(f.ctx, f.task.ID, f.item.ID, api.VerificationRequest{RequestID: "delegated-approval", AgentID: h.ID, RunID: h.RunID, Plan: &p}); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "owner matrix approval source does not bind exact digest") {
		t.Fatal("a delegate's token served as the owner's matrix approval", err)
	}
}

// Opening a window routes what still waits on the owner: not a decision whose
// requester is closed or exited, and not a withdrawn or superseded owner
// request.
func TestDelegationOpenRoutesOnlyLiveRequests(t *testing.T) {
	f := newDelegationFixture(t)
	ctx := context.Background()
	gone, err := f.s.AddAgent(ctx, f.task.ID, api.AddAgentRequest{Name: "gone", Host: "fixture", Session: "gone", Runtime: "codex"}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	ask := func(key string, from api.Agent) api.Message {
		t.Helper()
		m, err := f.s.CreateDecision(ctx, f.task.ID, api.CreateDecisionRequest{AgentID: from.ID, RequestID: key, DecisionRequest: api.DecisionRequest{
			Question: "Which rollout should the fixture use?", RecommendedOptionID: "staged", RecommendationReason: "Limits impact.",
			Options: []api.DecisionOption{{ID: "staged", Label: "Staged", Description: "A few first."}, {ID: "all", Label: "All", Description: "Everyone at once."}}}}, f.by)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	closed, exited := ask("closed", f.other), ask("exited", gone)
	for id, status := range map[string]string{f.other.ID: api.AgentClosed, gone.ID: api.AgentExited} {
		if _, err := f.s.db.Exec(`UPDATE agents SET status=? WHERE id=?`, status, id); err != nil {
			t.Fatal(err)
		}
	}
	withdrawn := f.ownerRequest(t, "withdrawn", "", "")
	if _, err := f.s.WithdrawObligation(ctx, f.task.ID, withdrawn.ID, api.ObligationWithdrawRequest{AgentID: f.worker.ID, RunID: f.worker.RunID, Reason: "no longer needed", RequestID: "withdraw"}); err != nil {
		t.Fatal(err)
	}
	superseded := f.ownerRequest(t, "superseded", "", "")
	if _, err := f.s.db.Exec(`UPDATE obligations SET state=?,outcome=? WHERE id=?`, api.ObligationClosed, api.OutcomeSuperseded, superseded.ID); err != nil {
		t.Fatal(err)
	}
	f.advance(48 * time.Hour)
	live, waiting := ask("live", f.worker), f.ownerRequest(t, "waiting", "", "")
	w := f.open(t, "open", api.DelegationScopeDecisions, time.Hour)
	if routed(w, "decision", closed.Seq) != nil || routed(w, "decision", exited.Seq) != nil {
		t.Fatalf("routed a decision from a closed or exited requester: %+v", w.Routes)
	}
	if routed(w, "obligation", withdrawn.MessageSeq) != nil || routed(w, "obligation", superseded.MessageSeq) != nil {
		t.Fatalf("routed a withdrawn or superseded owner request: %+v", w.Routes)
	}
	if routed(w, "decision", live.Seq) == nil || routed(w, "obligation", waiting.MessageSeq) == nil || len(w.Routes) != 2 {
		t.Fatalf("live requests %+v", w.Routes)
	}
	// The left-out decision stays with the owner, who can still answer it.
	if _, err := f.s.AnswerDecision(ctx, f.task.ID, closed.Seq, api.AnswerDecisionRequest{RequestID: "owner", OptionID: "staged"}, f.by); err != nil {
		t.Fatal(err)
	}
}

// A request the window never routed: the delegate's stale run is refused as
// forbidden, its current run as a conflict.
func TestDelegationUnroutedRequestStaleRunIsForbidden(t *testing.T) {
	f := newDelegationFixture(t)
	f.open(t, "open", api.DelegationScopeDecisions, time.Hour)
	merge := f.ownerRequest(t, "merge", "merge", "")
	if routed(f.window(t), "obligation", merge.MessageSeq) != nil {
		t.Fatal("routed an out-of-scope request")
	}
	stale := api.Agent{ID: f.lead.ID, RunID: "run_0000000000000000"}
	if _, err := f.delegateAnswer("stale", merge.ID, "Merge it", "because", stale); !errors.Is(err, api.ErrDelegationForbidden) || errors.Is(err, api.ErrConflict) {
		t.Fatalf("stale run on an unrouted request: %v", err)
	}
	if _, err := f.delegateAnswer("current", merge.ID, "Merge it", "because", f.lead); !errors.Is(err, api.ErrConflict) || errors.Is(err, api.ErrDelegationForbidden) {
		t.Fatalf("current run on an unrouted request: %v", err)
	}
}

// ---- the reminder before the end (bug wi_9898f0ff7e8e1464) ----

// reminderStart puts the fixture's fake clock on a whole UTC minute, so the
// reminder slot's truncation is deterministic.
func reminderStart(f delegationFixture) time.Time {
	*f.clock = time.Date(2026, 11, 1, 18, 0, 0, 0, time.UTC)
	return *f.clock
}

// reminderWakes lists the project's authority-expiry wakes.
func (f delegationFixture) reminderWakes(t *testing.T) []api.OwnerHelperWake {
	t.Helper()
	list, err := f.s.ListOwnerHelperWakes(context.Background(), f.task.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []api.OwnerHelperWake
	for _, w := range list.Wakes {
		if w.Kind == "authority-expiry" {
			out = append(out, w)
		}
	}
	return out
}

func (f delegationFixture) wakeByID(t *testing.T, id string) api.OwnerHelperWake {
	t.Helper()
	w, err := scanOwnerWake(f.s.db.QueryRow(`SELECT `+ownerWakeCols+` FROM owner_helper_wakes WHERE id=?`, id))
	if err != nil {
		t.Fatal(err)
	}
	return w
}

// reminderNotices lists the board notices that set windowID's reminder.
func (f delegationFixture) reminderNotices(t *testing.T, windowID string) []api.Message {
	t.Helper()
	msgs, err := f.s.ListMessages(context.Background(), f.task.ID, 0, "", 500)
	if err != nil {
		t.Fatal(err)
	}
	var out []api.Message
	for _, m := range msgs {
		if m.Envelope != nil && m.Envelope.Refs["delegationReminder"] == windowID {
			out = append(out, m)
		}
	}
	return out
}

// wakeNotices lists every authority-expiry wake notice and owner report the
// broker posted.
func (f delegationFixture) wakeNotices(t *testing.T) []api.Message {
	t.Helper()
	msgs, err := f.s.ListMessages(context.Background(), f.task.ID, 0, "", 500)
	if err != nil {
		t.Fatal(err)
	}
	var out []api.Message
	for _, m := range msgs {
		if m.Envelope != nil && m.Envelope.Refs["ownerWake"] == "authority-expiry" && m.Envelope.Refs["delegationReminder"] == "" {
			out = append(out, m)
		}
	}
	return out
}

func (f delegationFixture) wakeSweep(t *testing.T, at time.Time) []OwnerHelperWakeStep {
	t.Helper()
	*f.clock = at
	steps, err := f.s.OwnerHelperWakeSweep(context.Background(), at)
	if err != nil {
		t.Fatal(err)
	}
	return steps
}

// tick runs the broker's two steps in Tick order: window expiry, then the
// wake sweep.
func (f delegationFixture) tick(t *testing.T, at time.Time) ([]string, []OwnerHelperWakeStep) {
	t.Helper()
	*f.clock = at
	expired, err := f.s.BrokerExpireDelegationWindows(context.Background(), at)
	if err != nil {
		t.Fatal(err)
	}
	return expired, f.wakeSweep(t, at)
}

// reopen is a hub restart over the same database with the same fake clock.
func (f *delegationFixture) reopen(t *testing.T) {
	t.Helper()
	f.s.Close()
	s, err := Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	clock := f.clock
	s.now = func() time.Time { return *clock }
	f.s = s
	t.Cleanup(func() { s.Close() })
}

// a1: opening a window saves one active authority-expiry wake at
// trunc_minute(end - min(24h, half the window)), with one board notice that
// says what to do; a window too short for a whole minute gets none.
func TestDelegationReminderSchedule(t *testing.T) {
	for _, c := range []struct {
		name   string
		window time.Duration
		lead   time.Duration // end - slot; 0 means no reminder
	}{
		{"seven days", 7 * 24 * time.Hour, 24 * time.Hour},
		{"three hours", 3 * time.Hour, 90 * time.Minute},
		{"truncated to the minute", 3*time.Hour + 30*time.Second, 90*time.Minute + 30*time.Second},
		{"two minutes", 2 * time.Minute, time.Minute},
		{"no whole minute", 61 * time.Second, 0},
		{"one minute", time.Minute, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newDelegationFixture(t)
			start := reminderStart(f)
			w := f.open(t, "open-reminder", api.DelegationScopeDecisions, c.window)
			wakes, notices := f.reminderWakes(t), f.reminderNotices(t, w.ID)
			if c.lead == 0 {
				if len(wakes) != 0 || len(notices) != 0 {
					t.Fatalf("short window saved %+v %+v", wakes, notices)
				}
				return
			}
			if len(wakes) != 1 || len(notices) != 1 {
				t.Fatalf("wakes %+v notices %+v", wakes, notices)
			}
			end := start.Add(c.window)
			at := end.Add(-c.lead)
			if at.Truncate(time.Minute) != at || !at.After(start) {
				t.Fatalf("case slot %s is not a whole minute after the open", at)
			}
			wk, n := wakes[0], notices[0]
			schedule := strconv.Itoa(at.Minute()) + " " + strconv.Itoa(at.Hour()) + " " + strconv.Itoa(at.Day()) + " " + strconv.Itoa(int(at.Month())) + " *"
			if wk.State != api.OwnerHelperWakeActive || wk.TimeZone != "UTC" || wk.Schedule != schedule || wk.NextDueAt == nil || !wk.NextDueAt.Equal(at) ||
				wk.ExpiresAt == nil || !wk.ExpiresAt.Equal(end) || wk.AgentID != "" || wk.RunID != "" || wk.Revision != 1 || wk.MessageSeq != n.Seq ||
				wk.RequestID != "delegation-reminder-"+w.ID {
				t.Fatalf("wake %+v; want slot %s schedule %q expiry %s message %d", wk, at, schedule, end, n.Seq)
			}
			env := n.Envelope
			wantRefs := map[string]string{"delegationReminder": w.ID, "ownerWake": "authority-expiry", "wake": wk.ID}
			if n.To != "" || n.From.Node != api.BrokerNode || env.Kind != api.EnvelopeKindNotice || env.Subject != "A reminder is set before the delegation window ends" || !sameRefs(env.Refs, wantRefs) {
				t.Fatalf("notice %+v %+v", n, env)
			}
			for _, part := range []string{w.ID, f.lead.Name, windowEnd(w), at.Format("Jan 2 15:04 UTC"), wk.ID,
				"tt owner delegation close " + w.ID, "tt owner delegation open --delegate " + f.lead.Name, "unanswered routed requests return to the owner"} {
				if !strings.Contains(env.Body.Text, part) {
					t.Fatalf("notice text lacks %q: %s", part, env.Body.Text)
				}
			}
			if got := f.boardNotices(t, w.ID); len(got) != 0 {
				t.Fatalf("the reminder notice must not count as a window notice: %+v", got)
			}
			out, err := f.s.OpenDelegationWindow(context.Background(), f.task.ID, api.OpenDelegationWindowRequest{Delegate: f.lead.Name, EndsAt: end, Scope: api.DelegationScopeDecisions, Source: &api.DelegationSource{Kind: api.DelegationSourceTT}, RequestID: "open-reminder"}, f.by)
			if err != nil || !out.Replay {
				t.Fatalf("replay %+v %v", out, err)
			}
			if len(f.reminderWakes(t)) != 1 || len(f.reminderNotices(t, w.ID)) != 1 {
				t.Fatal("a replayed open added a reminder")
			}
		})
	}
}

// a1: the reminder is not refused for the 20-wake cap, and counts toward it.
func TestDelegationReminderCap(t *testing.T) {
	f := newDelegationFixture(t)
	reminderStart(f)
	ctx := context.Background()
	m, err := f.s.PostMessage(ctx, f.task.ID, api.PostMessageRequest{Text: "synthetic wake instructions"}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < api.MaxOwnerHelperWakes; i++ {
		if _, _, err := f.s.SetOwnerHelperWake(ctx, f.task.ID, api.SetOwnerHelperWakeRequest{Kind: "hourly-update", Schedule: "14 7-20 * * *", MessageSeq: m.Seq, RequestID: "cap-" + strconv.Itoa(i)}, f.by); err != nil {
			t.Fatal(err)
		}
	}
	w := f.open(t, "open-cap", api.DelegationScopeDecisions, 3*time.Hour)
	if wakes := f.reminderWakes(t); len(wakes) != 1 || wakes[0].State != api.OwnerHelperWakeActive || wakes[0].RequestID != "delegation-reminder-"+w.ID {
		t.Fatalf("reminder at the cap %+v", wakes)
	}
	if _, _, err := f.s.SetOwnerHelperWake(ctx, f.task.ID, api.SetOwnerHelperWakeRequest{Kind: "other", Schedule: "0 9 * * *", MessageSeq: m.Seq, RequestID: "cap-over"}, f.by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("a wake past the cap with the reminder counted: %v", err)
	}
}

// a2: at the slot the existing wake sweep posts the standard owner-helper
// wake notice to the current helper once, across sweeps and a restart, and
// the wake expires. broker.Tick needs no new step.
func TestDelegationReminderDelivery(t *testing.T) {
	f := newDelegationFixture(t)
	start := reminderStart(f)
	helper := *registerHelper(t, f.s, f.task.ID, helperRequest("reminder-reg"), f.by).Agent
	w := f.open(t, "open-delivery", api.DelegationScopeDecisions, 3*time.Hour)
	at := start.Add(90 * time.Minute)
	wk := f.reminderWakes(t)[0]
	reminder := f.reminderNotices(t, w.ID)[0]
	if expired, steps := f.tick(t, at.Add(-time.Second)); len(expired) != 0 || len(steps) != 0 {
		t.Fatalf("before the slot %v %+v", expired, steps)
	}
	expired, steps := f.tick(t, at.Add(20*time.Second))
	if len(expired) != 0 || len(steps) != 1 || steps[0].Action != ownerWakeActionPosted || steps[0].WakeID != wk.ID {
		t.Fatalf("at the slot %v %+v", expired, steps)
	}
	got := f.wakeNotices(t)
	if len(got) != 1 || got[0].Seq != steps[0].MessageSeq || got[0].To != helper.ID {
		t.Fatalf("wake notices %+v", got)
	}
	env := got[0].Envelope
	wantRefs := map[string]string{"ownerWake": "authority-expiry", "wake": wk.ID, "due": at.Format(time.RFC3339), "message": strconv.FormatInt(reminder.Seq, 10)}
	if env.Kind != api.EnvelopeKindNotice || env.Subject != "Owner-helper wake: authority expiry due "+at.Format("15:04")+" UTC" || env.To != helper.Name || !sameRefs(env.Refs, wantRefs) ||
		!strings.Contains(env.Body.Text, "Do what Board message #"+strconv.FormatInt(reminder.Seq, 10)+" says.") {
		t.Fatalf("wake notice %+v", env)
	}
	if after := f.wakeByID(t, wk.ID); after.State != api.OwnerHelperWakeExpired || after.NextDueAt != nil {
		t.Fatalf("after firing %+v", after)
	}
	if steps := f.wakeSweep(t, at.Add(40*time.Second)); len(steps) != 0 {
		t.Fatalf("second sweep %+v", steps)
	}
	f.reopen(t)
	if steps := f.wakeSweep(t, at.Add(50*time.Second)); len(steps) != 0 || len(f.wakeNotices(t)) != 1 {
		t.Fatalf("after a restart %+v", steps)
	}
	// A reminder already fired stays expired when the window later ends.
	if expired, _ := f.tick(t, start.Add(3*time.Hour)); len(expired) != 1 || expired[0] != w.ID {
		t.Fatalf("window expiry %v", expired)
	}
	if after := f.wakeByID(t, wk.ID); after.State != api.OwnerHelperWakeExpired || after.Revision != wk.Revision {
		t.Fatalf("fired reminder after the end %+v", after)
	}
	toHelper := 0
	for _, m := range f.wakeNotices(t) {
		if m.To == helper.ID {
			toHelper++
		}
	}
	if toHelper != 1 {
		t.Fatalf("the helper got %d reminders", toHelper)
	}
}

// a2: with no live owner helper the slot is helper-absent and the owner gets
// one report naming the reminder's Board message.
func TestDelegationReminderHelperAbsent(t *testing.T) {
	f := newDelegationFixture(t)
	start := reminderStart(f)
	w := f.open(t, "open-absent", api.DelegationScopeDecisions, 7*24*time.Hour)
	at := start.Add(6 * 24 * time.Hour)
	wk := f.reminderWakes(t)[0]
	reminder := f.reminderNotices(t, w.ID)[0]
	_, steps := f.tick(t, at.Add(10*time.Second))
	if len(steps) != 1 || steps[0].Action != ownerWakeActionAbsent {
		t.Fatalf("steps %+v", steps)
	}
	got := f.wakeNotices(t)
	if len(got) != 1 || got[0].To != "" || got[0].Envelope.Refs["recipientKind"] != "owner" || got[0].Envelope.Refs["wake"] != wk.ID ||
		!strings.Contains(got[0].Envelope.Body.Text, "Board message #"+strconv.FormatInt(reminder.Seq, 10)) {
		t.Fatalf("owner report %+v", got)
	}
	if _, steps := f.tick(t, at.Add(time.Minute)); len(steps) != 0 || len(f.wakeNotices(t)) != 1 {
		t.Fatalf("again %+v", steps)
	}
	list, err := ownerWakeFirings(context.Background(), f.s.db, wk.ID, 10)
	if err != nil || len(list) != 1 || list[0].Outcome != api.OwnerHelperWakeHelperAbsent {
		t.Fatalf("firings %+v %v", list, err)
	}
}

// a3: ending the window before its slot cancels the reminder in the same
// transaction, and no reminder is posted for it.
func TestDelegationReminderCancel(t *testing.T) {
	ctx := context.Background()
	cancelled := func(t *testing.T, f delegationFixture, wk api.OwnerHelperWake, revision int64) {
		t.Helper()
		after := f.wakeByID(t, wk.ID)
		if after.State != api.OwnerHelperWakeCancelled || after.Revision != revision || after.NextDueAt != nil {
			t.Fatalf("reminder after the end %+v; want cancelled at revision %d", after, revision)
		}
	}
	t.Run("owner close", func(t *testing.T) {
		f := newDelegationFixture(t)
		start := reminderStart(f)
		registerHelper(t, f.s, f.task.ID, helperRequest("reminder-reg"), f.by)
		w := f.open(t, "open-close", api.DelegationScopeDecisions, 3*time.Hour)
		wk := f.reminderWakes(t)[0]
		f.advance(time.Hour)
		if _, err := f.s.CloseDelegationWindow(ctx, f.task.ID, w.ID, api.CloseDelegationWindowRequest{RequestID: "close-1"}, f.by); err != nil {
			t.Fatal(err)
		}
		cancelled(t, f, wk, 2)
		if _, steps := f.tick(t, start.Add(4*time.Hour)); len(steps) != 0 || len(f.wakeNotices(t)) != 0 {
			t.Fatalf("a closed window reminded %+v", steps)
		}
	})
	t.Run("hub down past the slot and the end", func(t *testing.T) {
		f := newDelegationFixture(t)
		start := reminderStart(f)
		registerHelper(t, f.s, f.task.ID, helperRequest("reminder-reg"), f.by)
		w := f.open(t, "open-down", api.DelegationScopeDecisions, 3*time.Hour)
		wk := f.reminderWakes(t)[0]
		f.reopen(t)
		expired, steps := f.tick(t, start.Add(3*time.Hour+time.Minute))
		if len(expired) != 1 || expired[0] != w.ID || len(steps) != 0 || len(f.wakeNotices(t)) != 0 {
			t.Fatalf("down past the end %v %+v", expired, steps)
		}
		cancelled(t, f, wk, 2)
	})
	t.Run("open over a past-end window", func(t *testing.T) {
		f := newDelegationFixture(t)
		start := reminderStart(f)
		first := f.open(t, "open-first", api.DelegationScopeDecisions, 3*time.Hour)
		wk := f.reminderWakes(t)[0]
		*f.clock = start.Add(3*time.Hour + time.Minute)
		second := f.open(t, "open-second", api.DelegationScopeDecisions, 3*time.Hour)
		cancelled(t, f, wk, 2)
		var active []api.OwnerHelperWake
		for _, x := range f.reminderWakes(t) {
			if x.State == api.OwnerHelperWakeActive {
				active = append(active, x)
			}
		}
		if len(active) != 1 || active[0].RequestID != "delegation-reminder-"+second.ID || active[0].ID == wk.ID {
			t.Fatalf("active reminders %+v (first %s)", active, first.ID)
		}
	})
	t.Run("close then open", func(t *testing.T) {
		f := newDelegationFixture(t)
		reminderStart(f)
		first := f.open(t, "open-a", api.DelegationScopeDecisions, 7*24*time.Hour)
		f.advance(6 * 24 * time.Hour)
		if _, err := f.s.CloseDelegationWindow(ctx, f.task.ID, first.ID, api.CloseDelegationWindowRequest{RequestID: "renew-close"}, f.by); err != nil {
			t.Fatal(err)
		}
		second := f.open(t, "open-b", api.DelegationScopeDecisions, 7*24*time.Hour)
		var active []api.OwnerHelperWake
		for _, x := range f.reminderWakes(t) {
			if x.State == api.OwnerHelperWakeActive {
				active = append(active, x)
			}
		}
		if len(active) != 1 || active[0].RequestID != "delegation-reminder-"+second.ID || !active[0].NextDueAt.Equal(f.s.now().Add(6*24*time.Hour)) {
			t.Fatalf("after a renewal %+v", active)
		}
	})
	t.Run("helper changed the wake", func(t *testing.T) {
		f := newDelegationFixture(t)
		start := reminderStart(f)
		helper := *registerHelper(t, f.s, f.task.ID, helperRequest("reminder-reg"), f.by).Agent
		w := f.open(t, "open-edit", api.DelegationScopeDecisions, 3*time.Hour)
		wk := f.reminderWakes(t)[0]
		later := start.Add(2 * time.Hour)
		edited, _, err := f.s.SetOwnerHelperWake(ctx, f.task.ID, api.SetOwnerHelperWakeRequest{WakeID: wk.ID, ExpectedRevision: wk.Revision, Kind: "authority-expiry",
			Schedule: strconv.Itoa(later.Minute()) + " " + strconv.Itoa(later.Hour()) + " " + strconv.Itoa(later.Day()) + " " + strconv.Itoa(int(later.Month())) + " *", TimeZone: "UTC",
			MessageSeq: wk.MessageSeq, ExpiresAt: wk.ExpiresAt, RequestID: "helper-edit", AgentID: helper.ID, RunID: helper.RunID}, f.by)
		if err != nil || edited.Revision != 2 || edited.RequestID != "helper-edit" || !edited.NextDueAt.Equal(later) {
			t.Fatalf("helper edit %+v %v", edited, err)
		}
		f.advance(time.Hour)
		if _, err := f.s.CloseDelegationWindow(ctx, f.task.ID, w.ID, api.CloseDelegationWindowRequest{RequestID: "close-edit"}, f.by); err != nil {
			t.Fatal(err)
		}
		cancelled(t, f, wk, 3)
		if _, steps := f.tick(t, start.Add(4*time.Hour)); len(steps) != 0 || len(f.wakeNotices(t)) != 0 {
			t.Fatalf("an edited reminder of a closed window fired %+v", steps)
		}
	})
}
