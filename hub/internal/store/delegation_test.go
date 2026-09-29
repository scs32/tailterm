package store

import (
	"context"
	"errors"
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
	out, err := f.s.AnswerObligation(f.ctx, f.task.ID, obligation, api.ObligationAnswerRequest{AgentID: delegate.ID, RunID: delegate.RunID, Text: "Option A", Rationale: "Safer.", RequestID: "delegate"}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	p.MatrixApprovalMessageSeq = out.Message.Seq
	if _, err := f.s.SaveVerification(f.ctx, f.task.ID, f.item.ID, api.VerificationRequest{RequestID: "delegated-approval", AgentID: h.ID, RunID: h.RunID, Plan: &p}); !errors.Is(err, api.ErrConflict) {
		t.Fatal("a delegated answer served as the owner's matrix approval", err)
	}
}
