package store

import (
	"context"
	"errors"
	"fmt"
	"github.com/scs32/tailterm/hub/internal/api"
	"path/filepath"
	"testing"
)

func TestReviewConvergenceBaselineCap(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "fixture.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	by := api.Caller{Node: "fixture", User: "owner"}
	task, err := s.CreateTask(ctx, api.CreateTaskRequest{Name: "Review fixture"}, by)
	if err != nil {
		t.Fatal(err)
	}
	reviewer, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "reviewer", Host: "fixture", Session: "reviewer"}, by)
	if err != nil {
		t.Fatal(err)
	}
	item, err := s.CreateWorkItem(ctx, task.ID, api.CreateWorkItemRequest{Kind: "bug", Title: "Review cap fixture", RequestID: "fixture-item"}, by)
	if err != nil {
		t.Fatal(err)
	}
	links := []api.MessageWorkItem{{ItemTaskID: task.ID, ItemID: item.ID, ItemRevision: 1, Relationship: "primary"}}
	env := api.Envelope{Kind: "assign", Subject: "Implement fixture acceptance", Body: api.EnvelopeBody{Objective: "Fixture", Owns: []string{"fixture"}, Acceptance: map[string]string{"a1": "Fixture works"}}}
	if _, err = s.PostMessage(ctx, task.ID, api.PostMessageRequest{Envelope: &env, To: reviewer.ID, WorkItems: links, RequestID: "assign"}, by); err != nil {
		t.Fatal(err)
	}
	env.Kind = "review"
	env.Body.Candidate = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	env.Body.Scope = "Fixture"
	for i := 0; i < 3; i++ {
		_, err = s.PostMessage(ctx, task.ID, api.PostMessageRequest{Envelope: &env, To: reviewer.ID, WorkItems: links, RequestID: fmt.Sprintf("review-%d", i)}, by)
		if i == 0 && err != nil {
			t.Fatal(err)
		}
		if i > 0 && !errors.Is(err, api.ErrConflict) {
			t.Fatalf("third general review must be refused; got %v", err)
		}
	}
}

type convergenceFixture struct {
	s        *Store
	ctx      context.Context
	by       api.Caller
	task     api.Task
	item     api.WorkItem
	reviewer api.Agent
	path     string
	serial   int
}

func newConvergenceFixture(t *testing.T) *convergenceFixture {
	t.Helper()
	f := &convergenceFixture{ctx: context.Background(), by: api.Caller{Node: "fixture", User: "owner"}, path: filepath.Join(t.TempDir(), "review.sqlite")}
	var err error
	f.s, err = Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.s.Close() })
	f.task, err = f.s.CreateTask(f.ctx, api.CreateTaskRequest{Name: "Convergence fixture"}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	f.item, err = f.s.CreateWorkItem(f.ctx, f.task.ID, api.CreateWorkItemRequest{Kind: "bug", Title: "Convergence fixture", RequestID: "item"}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	f.reviewer, err = f.s.AddAgent(f.ctx, f.task.ID, api.AddAgentRequest{Name: "reviewer", Host: "fixture", Session: "reviewer"}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	env := api.Envelope{Kind: "assign", Subject: "Implement frozen fixture criteria", Body: api.EnvelopeBody{Objective: "Fixture", Owns: []string{"fixture"}, Acceptance: map[string]string{"a1": "works", "a2": "retries"}}}
	if _, err = f.post(env, f.reviewer.ID, 0, api.Agent{}); err != nil {
		t.Fatal(err)
	}
	return f
}
func (f *convergenceFixture) req(env api.Envelope, to string, reply int64, from api.Agent) api.PostMessageRequest {
	f.serial++
	return api.PostMessageRequest{Envelope: &env, To: to, ReplyTo: reply, AgentID: from.ID, RunID: from.RunID, RequestID: fmt.Sprintf("transition-%d", f.serial), WorkItems: []api.MessageWorkItem{{ItemTaskID: f.task.ID, ItemID: f.item.ID, ItemRevision: f.item.Revision, Relationship: "primary"}}}
}
func (f *convergenceFixture) post(env api.Envelope, to string, reply int64, from api.Agent) (api.Message, error) {
	return f.s.PostMessage(f.ctx, f.task.ID, f.req(env, to, reply, from), f.by)
}
func (f *convergenceFixture) review(t *testing.T, candidate string) api.Message {
	t.Helper()
	m, err := f.post(api.Envelope{Kind: "review", Subject: "Review frozen fixture candidate", Body: api.EnvelopeBody{Candidate: candidate, Scope: "Fixture", Acceptance: map[string]string{"a1": "works", "a2": "retries"}}}, f.reviewer.ID, 0, api.Agent{})
	if err != nil {
		t.Fatal(err)
	}
	return m
}
func (f *convergenceFixture) resultEnv(candidate string, status map[string]string, meta api.ReviewMetadata) api.Envelope {
	meta.Candidate = candidate
	return api.Envelope{Kind: "result", Subject: "Fixture review verdicts recorded", Review: &meta, Body: api.EnvelopeBody{Outcome: "done", Status: status}, Evidence: map[string]api.Evidence{"e1": {Type: "command", Value: "fixture check -> recorded verdicts"}}}
}
func (f *convergenceFixture) state(t *testing.T) api.ReviewConvergence {
	t.Helper()
	v, err := f.s.ListReviewConvergence(f.ctx, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range v {
		if i.ItemID == f.item.ID {
			return i
		}
	}
	t.Fatal("missing item")
	return api.ReviewConvergence{}
}

var passConvergence = map[string]string{"a1": "pass", "a2": "pass"}

const candidateA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const candidateB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
const candidateC = "cccccccccccccccccccccccccccccccccccccccc"

func TestReviewConvergenceCompletedCapRetryRestartAndScope(t *testing.T) {
	f := newConvergenceFixture(t)
	for i := 0; i < 2; i++ {
		r := f.review(t, candidateA)
		req := f.req(f.resultEnv(candidateA, passConvergence, api.ReviewMetadata{Mode: "general"}), "", r.Seq, f.reviewer)
		m, err := f.s.PostMessage(f.ctx, f.task.ID, req, f.by)
		if err != nil {
			t.Fatal(err)
		}
		again, err := f.s.PostMessage(f.ctx, f.task.ID, req, f.by)
		if err != nil || again.Seq != m.Seq {
			t.Fatal("result replay", again, err)
		}
	}
	if len(f.state(t).Rounds) != 2 {
		t.Fatal("round count")
	}
	raw := f.req(api.Envelope{}, f.reviewer.ID, 0, api.Agent{})
	raw.Envelope = nil
	raw.Text = "REVIEW: Attempt another general fixture review\nCandidate: " + candidateA + "\nScope: Fixture\nAcceptance: a1: works; a2: retries"
	if _, err := f.s.PostMessage(f.ctx, f.task.ID, raw, f.by); !errors.Is(err, api.ErrConflict) {
		t.Fatal("text convention bypass", err)
	}

	f.s.Close()
	var err error
	f.s, err = Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	third := api.Envelope{Kind: "review", Subject: "Third general fixture review", Body: api.EnvelopeBody{Candidate: candidateB, Scope: "Fixture", Acceptance: map[string]string{"a1": "works", "a2": "retries"}}}
	before, _ := f.s.ListMessages(f.ctx, f.task.ID, 0, "", 100)
	if _, err = f.post(third, f.reviewer.ID, 0, api.Agent{}); !errors.Is(err, api.ErrConflict) {
		t.Fatal("third", err)
	}
	after, _ := f.s.ListMessages(f.ctx, f.task.ID, 0, "", 100)
	if len(before) != len(after) {
		t.Fatal("refusal wrote a message")
	}
	desc := "New revision retains lifetime count"
	f.item, err = f.s.UpdateWorkItem(f.ctx, f.task.ID, f.item.ID, api.UpdateWorkItemRequest{Revision: f.item.Revision, Description: &desc}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	changed := api.Envelope{Kind: "assign", Subject: "Implement changed fixture criteria", Body: api.EnvelopeBody{Objective: "Fixture", Owns: []string{"fixture"}, Acceptance: map[string]string{"a1": "changed"}}}
	if _, err = f.post(changed, "", 0, api.Agent{}); err != nil {
		t.Fatal(err)
	}
	if _, err = f.post(third, f.reviewer.ID, 0, api.Agent{}); !errors.Is(err, api.ErrConflict) {
		t.Fatal("scope reset", err)
	}
	if len(f.state(t).Scopes) != 2 || len(f.state(t).Rounds) != 2 {
		t.Fatal("scope history lost")
	}
}

func TestReviewConvergenceFrozenCriteriaAndConcurrentStarts(t *testing.T) {
	f := newConvergenceFixture(t)
	env := api.Envelope{Kind: "assign", Subject: "Attempt changed frozen criteria", Body: api.EnvelopeBody{Objective: "Fixture", Owns: []string{"fixture"}, Acceptance: map[string]string{"a1": "changed"}}}
	if _, err := f.post(env, "", 0, api.Agent{}); !errors.Is(err, api.ErrConflict) {
		t.Fatal(err)
	}
	env.Body.Acceptance = map[string]string{"a2": "gap"}
	if _, err := f.post(env, "", 0, api.Agent{}); !errors.Is(err, api.ErrConflict) {
		t.Fatal(err)
	}
	env = api.Envelope{Kind: "review", Subject: "Concurrent frozen fixture review", Body: api.EnvelopeBody{Candidate: candidateA, Scope: "Fixture", Acceptance: map[string]string{"a1": "works", "a2": "retries"}}}
	requests := []api.PostMessageRequest{f.req(env, f.reviewer.ID, 0, api.Agent{}), f.req(env, f.reviewer.ID, 0, api.Agent{})}
	ch := make(chan error, 2)
	for _, req := range requests {
		go func(r api.PostMessageRequest) { _, e := f.s.PostMessage(f.ctx, f.task.ID, r, f.by); ch <- e }(req)
	}
	successes := 0
	for range requests {
		if err := <-ch; err == nil {
			successes++
		} else if !errors.Is(err, api.ErrConflict) {
			t.Fatal(err)
		}
	}
	if successes != 1 || len(f.state(t).Rounds) != 1 {
		t.Fatal("concurrent reservations", successes)
	}
	req := requests[0]
	state := f.state(t)
	if state.Rounds[0].RequestSeq == 0 {
		t.Fatal(state)
	}
	// Whichever won replays without increasing the count.
	for _, req = range requests {
		if m, err := f.s.PostMessage(f.ctx, f.task.ID, req, f.by); err == nil && m.Seq != state.Rounds[0].RequestSeq {
			t.Fatal("replay changed round")
		}
	}
}

func TestReviewConvergenceBlockersFollowUpsFocusedAndCompletion(t *testing.T) {
	f := newConvergenceFixture(t)
	r1 := f.review(t, candidateA)
	blocker := api.ReviewFinding{ID: "b1", Criterion: "a1", Title: "Fixture regression", Command: "fixture retry", Output: "FAIL lost state"}
	meta := api.ReviewMetadata{Mode: "general", Blockers: []api.ReviewFinding{blocker}, Findings: []api.ReviewFinding{{ID: "f1", Title: "Improve fixture explanation", Kind: "feature", File: "fixture.go", Line: 3}}}
	bad := meta
	bad.Blockers = []api.ReviewFinding{{ID: "b1", Criterion: "a9", Title: "Unknown criterion", File: "fixture.go", Line: 4}}
	if _, err := f.post(f.resultEnv(candidateA, map[string]string{"a1": "fail", "a2": "pass"}, bad), "", r1.Seq, f.reviewer); !errors.Is(err, api.ErrConflict) {
		t.Fatal("unknown", err)
	}
	bad = meta
	bad.Blockers = []api.ReviewFinding{{ID: "b1", Criterion: "a1", Title: "No evidence"}}
	if _, err := f.post(f.resultEnv(candidateA, map[string]string{"a1": "fail", "a2": "pass"}, bad), "", r1.Seq, f.reviewer); !errors.Is(err, api.ErrConflict) {
		t.Fatal("no evidence", err)
	}
	req := f.req(f.resultEnv(candidateA, map[string]string{"a1": "fail", "a2": "pass"}, meta), "", r1.Seq, f.reviewer)
	if _, err := f.s.PostMessage(f.ctx, f.task.ID, req, f.by); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.PostMessage(f.ctx, f.task.ID, req, f.by); err != nil {
		t.Fatal(err)
	}
	state := f.state(t)
	if len(state.FollowUps) != 1 {
		t.Fatal("duplicate follow-up")
	}
	follow, err := f.s.GetWorkItem(f.ctx, f.task.ID, state.FollowUps[0].ItemID)
	if err != nil || follow.SourceMessageSeq != state.Rounds[0].ResultSeq || follow.Status != "open" || follow.Kind != "feature" {
		t.Fatal(follow, err)
	}
	queue, err := f.s.ListTeamQueue(f.ctx, f.task.ID)
	if err != nil || len(queue.Entries) != 0 {
		t.Fatal("follow-up auto-enqueued", queue, err)
	}
	r2 := f.review(t, candidateB)
	newNonRegression := api.ReviewFinding{ID: "b2", Criterion: "a2", Title: "New nonregression finding", File: "fixture.go", Line: 6}
	round2 := api.ReviewMetadata{Mode: "general", Blockers: []api.ReviewFinding{blocker, newNonRegression}}
	if _, err = f.post(f.resultEnv(candidateB, map[string]string{"a1": "fail", "a2": "fail"}, round2), "", r2.Seq, f.reviewer); err != nil {
		t.Fatal(err)
	}
	state = f.state(t)
	if len(state.Rounds[1].Blockers) != 1 || len(state.FollowUps) != 2 {
		t.Fatal("round-two scope", state)
	}
	done := "done"
	if _, err = f.s.UpdateWorkItem(f.ctx, f.task.ID, f.item.ID, api.UpdateWorkItemRequest{Revision: 1, Status: &done}, f.by); !errors.Is(err, api.ErrConflict) {
		t.Fatal("PATCH bypass", err)
	}
	if _, _, err = f.s.CreateWorkItemUpdate(f.ctx, f.task.ID, f.item.ID, api.CreateWorkItemUpdate{ExpectedRevision: 1, RequestID: "done-attempt", Status: &done}, f.by); !errors.Is(err, api.ErrConflict) {
		t.Fatal("keyed bypass", err)
	}
	focus := api.ReviewMetadata{Mode: "focused", Candidate: candidateC, Fix: "Exact retry fix", BlockerIDs: []string{"b1"}}
	fm, err := f.post(api.Envelope{Kind: "request", Subject: "Verify the exact retry fixture fix", Review: &focus, Body: api.EnvelopeBody{Ask: "Verify exactly this fix"}}, f.reviewer.ID, 0, api.Agent{})
	if err != nil {
		t.Fatal(err)
	}
	wrong := focus
	wrong.Candidate = candidateA
	if _, err = f.post(f.resultEnv(candidateA, map[string]string{"b1": "pass"}, wrong), "", fm.Seq, f.reviewer); !errors.Is(err, api.ErrConflict) {
		t.Fatal("wrong candidate", err)
	}
	stale := f.reviewer
	stale.RunID = "run_bbbbbbbbbbbbbbbb"
	if _, err = f.post(f.resultEnv(candidateC, map[string]string{"b1": "pass"}, focus), "", fm.Seq, stale); !errors.Is(err, api.ErrConflict) {
		t.Fatal("wrong run", err)
	}
	if _, err = f.post(f.resultEnv(candidateC, map[string]string{"b1": "pass"}, focus), "", fm.Seq, f.reviewer); err != nil {
		t.Fatal(err)
	}
	disposition := api.ReviewMetadata{Mode: "disposition", Candidate: candidateC, Disposition: "accept"}
	if _, err = f.post(api.Envelope{Kind: "notice", Subject: "Accept verified fixture candidate", Review: &disposition, Body: api.EnvelopeBody{Text: "Verified acceptance"}}, "", 0, api.Agent{}); !errors.Is(err, api.ErrConflict) {
		t.Fatal("failed a2 allowed acceptance", err)
	}
	// The new non-regression finding was a failed verdict, so acceptance remains blocked.
	if len(f.state(t).Rounds) != 2 || !f.state(t).Focused[0].Passed {
		t.Fatal("focused verification changed round count")
	}
}

func TestReviewConvergenceExactFocusedAcceptance(t *testing.T) {
	f := newConvergenceFixture(t)
	b := api.ReviewFinding{ID: "b1", Criterion: "a1", Title: "Retry failure", File: "fixture.go", Line: 7}
	for _, candidate := range []string{candidateA, candidateB} {
		r := f.review(t, candidate)
		if _, err := f.post(f.resultEnv(candidate, map[string]string{"a1": "fail", "a2": "pass"}, api.ReviewMetadata{Mode: "general", Blockers: []api.ReviewFinding{b}}), "", r.Seq, f.reviewer); err != nil {
			t.Fatal(err)
		}
	}
	accept := func(candidate string) error {
		_, err := f.post(api.Envelope{Kind: "notice", Subject: "Accept exact verified candidate", Review: &api.ReviewMetadata{Mode: "disposition", Disposition: "accept", Candidate: candidate}, Body: api.EnvelopeBody{Text: "Accept"}}, "", 0, api.Agent{})
		return err
	}
	if err := accept(candidateC); !errors.Is(err, api.ErrConflict) {
		t.Fatal("unverified acceptance", err)
	}
	meta := api.ReviewMetadata{Mode: "focused", Candidate: candidateC, Fix: "Restore durable retry receipt", BlockerIDs: []string{"b1"}}
	fm, err := f.post(api.Envelope{Kind: "request", Subject: "Verify precise retry receipt fix", Review: &meta, Body: api.EnvelopeBody{Ask: "Verify the receipt fix"}}, f.reviewer.ID, 0, api.Agent{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.post(f.resultEnv(candidateC, map[string]string{"b1": "pass"}, meta), "", fm.Seq, f.reviewer); err != nil {
		t.Fatal(err)
	}
	if err = accept(candidateC); err != nil {
		t.Fatal(err)
	}
	done := "done"
	changedTitle := "Changed scope cannot piggyback completion"
	if _, err = f.s.UpdateWorkItem(f.ctx, f.task.ID, f.item.ID, api.UpdateWorkItemRequest{Revision: 1, Status: &done, Title: &changedTitle}, f.by); !errors.Is(err, api.ErrConflict) {
		t.Fatal("scope-plus-Done PATCH bypass", err)
	}
	if _, _, err = f.s.CreateWorkItemUpdate(f.ctx, f.task.ID, f.item.ID, api.CreateWorkItemUpdate{ExpectedRevision: 1, RequestID: "scope-plus-done", Status: &done, Title: &changedTitle}, f.by); !errors.Is(err, api.ErrConflict) {
		t.Fatal("scope-plus-Done keyed bypass", err)
	}
	if _, err = f.s.UpdateWorkItem(f.ctx, f.task.ID, f.item.ID, api.UpdateWorkItemRequest{Revision: 1, Status: &done}, f.by); err != nil {
		t.Fatal("verified PATCH", err)
	}
	tx, err := f.s.db.BeginTx(f.ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err = reviewCompletion(f.ctx, tx, f.item, candidateB); !errors.Is(err, api.ErrConflict) {
		t.Fatal("team acceptance wrong commit", err)
	}
	if err = reviewCompletion(f.ctx, tx, f.item, candidateC); err != nil {
		t.Fatal("team acceptance exact commit", err)
	}
}

func TestReviewConvergenceRoundTwoResolutionAndRegression(t *testing.T) {
	f := newConvergenceFixture(t)
	r := f.review(t, candidateA)
	b := api.ReviewFinding{ID: "b1", Regression: true, Baseline: candidateB, Candidate: candidateA, Title: "Regression", File: "fixture.go", Line: 8}
	if _, err := f.post(f.resultEnv(candidateA, passConvergence, api.ReviewMetadata{Mode: "general", Blockers: []api.ReviewFinding{b}}), "", r.Seq, f.reviewer); err != nil {
		t.Fatal(err)
	}
	r = f.review(t, candidateC)
	if _, err := f.post(f.resultEnv(candidateC, passConvergence, api.ReviewMetadata{Mode: "general"}), "", r.Seq, f.reviewer); !errors.Is(err, api.ErrConflict) {
		t.Fatal("silent resolution", err)
	}
	if _, err := f.post(f.resultEnv(candidateC, passConvergence, api.ReviewMetadata{Mode: "general", BlockerIDs: []string{"b1"}}), "", r.Seq, f.reviewer); err != nil {
		t.Fatal(err)
	}
	if len(outstanding(f.state(t))) != 0 {
		t.Fatal("regression not resolved")
	}
}

func TestReviewConvergenceReassignmentRetainsRoundAndExactIdentity(t *testing.T) {
	f := newConvergenceFixture(t)
	r := f.review(t, candidateA)
	replacement, err := f.s.AddAgent(f.ctx, f.task.ID, api.AddAgentRequest{Name: "reviewer-next", Host: "fixture", Session: "reviewer-next"}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	obligations, err := f.s.ListObligations(f.ctx, f.task.ID, ObligationFilter{}, f.s.now())
	if err != nil {
		t.Fatal(err)
	}
	var obligation string
	for _, o := range obligations {
		if o.MessageSeq == r.Seq {
			obligation = o.ID
		}
	}
	moved, err := f.s.ReassignObligation(f.ctx, f.task.ID, obligation, api.ObligationReassignRequest{ToAgentID: replacement.ID, Reason: "Exact fixture replacement"}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.post(f.resultEnv(candidateA, passConvergence, api.ReviewMetadata{Mode: "general"}), "", r.Seq, f.reviewer); !errors.Is(err, api.ErrConflict) {
		t.Fatal("old reviewer accepted", err)
	}
	if _, err = f.post(f.resultEnv(candidateA, passConvergence, api.ReviewMetadata{Mode: "general"}), "", moved.Seq, replacement); err != nil {
		t.Fatal(err)
	}
	state := f.state(t)
	if len(state.Rounds) != 1 || state.Rounds[0].RequestSeq != r.Seq || state.Rounds[0].ActiveRequestSeq != moved.Seq || state.Rounds[0].ReviewerRun != replacement.RunID {
		t.Fatal(state)
	}
}

func TestReviewConvergenceLeadIdentityAndLegacyUnknown(t *testing.T) {
	f := newConvergenceFixture(t)
	lead, err := f.s.AddAgent(f.ctx, f.task.ID, api.AddAgentRequest{Name: "lead", Host: "fixture", Session: "lead"}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	name := "lead"
	if _, err = f.s.UpdateTask(f.ctx, f.task.ID, api.UpdateTaskRequest{Orchestrator: &name}, f.by); err != nil {
		t.Fatal(err)
	}
	env := api.Envelope{Kind: "review", Subject: "Lead starts exact fixture review", Body: api.EnvelopeBody{Candidate: candidateA, Scope: "Fixture", Acceptance: map[string]string{"a1": "works", "a2": "retries"}}}
	if _, err = f.post(env, f.reviewer.ID, 0, f.reviewer); !errors.Is(err, api.ErrConflict) {
		t.Fatal("non-lead started review", err)
	}
	stale := lead
	stale.RunID = "run_bbbbbbbbbbbbbbbb"
	if _, err = f.post(env, f.reviewer.ID, 0, stale); !errors.Is(err, api.ErrConflict) {
		t.Fatal("stale lead", err)
	}
	if _, err = f.post(env, f.reviewer.ID, 0, lead); err != nil {
		t.Fatal("current lead", err)
	}
	item, err := f.s.CreateWorkItem(f.ctx, f.task.ID, api.CreateWorkItemRequest{Kind: "bug", Title: "Legacy unknown fixture", RequestID: "legacy"}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	list, err := f.s.ListReviewConvergence(f.ctx, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if list[len(list)-1].ItemID != item.ID || list[len(list)-1].History != "unknown" {
		t.Fatal(list)
	}
}

func TestReviewConvergenceFollowUpRollbackIsAtomic(t *testing.T) {
	f := newConvergenceFixture(t)
	r := f.review(t, candidateA)
	before, _ := f.s.ListWorkItems(f.ctx, f.task.ID, "", "", 0, 100)
	metadata := api.ReviewMetadata{Mode: "general", Findings: []api.ReviewFinding{{ID: "f1", Title: "First valid followup", File: "fixture.go", Line: 3}, {ID: "f2", Title: "Second missing evidence"}}}
	if _, err := f.post(f.resultEnv(candidateA, passConvergence, metadata), "", r.Seq, f.reviewer); !errors.Is(err, api.ErrConflict) {
		t.Fatal(err)
	}
	after, _ := f.s.ListWorkItems(f.ctx, f.task.ID, "", "", 0, 100)
	if len(before.Items) != len(after.Items) || f.state(t).Rounds[0].ResultSeq != 0 || len(f.state(t).FollowUps) != 0 {
		t.Fatal("partial follow-up commit")
	}
}

func TestReviewConvergenceVerifierMustBeBoundToLinkedItem(t *testing.T) {
	f := newConvergenceFixture(t)
	b := api.ReviewFinding{ID: "b1", Criterion: "a1", Title: "Retry failure", File: "fixture.go", Line: 7}
	for _, candidate := range []string{candidateA, candidateB} {
		r := f.review(t, candidate)
		if _, err := f.post(f.resultEnv(candidate, map[string]string{"a1": "fail", "a2": "pass"}, api.ReviewMetadata{Mode: "general", Blockers: []api.ReviewFinding{b}}), "", r.Seq, f.reviewer); err != nil {
			t.Fatal(err)
		}
	}
	verifier, err := f.s.AddAgent(f.ctx, f.task.ID, api.AddAgentRequest{Name: "verifier", Host: "fixture", Session: "verifier"}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	verification, err := f.s.CreateWorkItem(f.ctx, f.task.ID, api.CreateWorkItemRequest{Kind: "bug", Title: "Exact fix verification fixture", RequestID: "verification"}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	order, err := f.post(api.Envelope{Kind: "notice", Subject: "Bounded exact verifier fixture order", Body: api.EnvelopeBody{Text: "Verify precise fix"}}, "", 0, api.Agent{})
	if err != nil {
		t.Fatal(err)
	}
	// Synthetic binding, never a live agent/context. Admission itself is tested separately.
	_, err = f.s.db.Exec(`INSERT INTO agent_work_item_bindings(agent_id,run_id,item_task_id,item_id,item_revision,work_order_task_id,work_order_message_seq,context_through_message_seq,context_digest,context_json,created_at) VALUES(?,?,?,?,1,?,?,0,'fixture','{}',?)`, verifier.ID, verifier.RunID, f.task.ID, verification.ID, f.task.ID, order.Seq, ts(f.s.now()))
	if err != nil {
		t.Fatal(err)
	}
	meta := api.ReviewMetadata{Mode: "focused", Candidate: candidateC, Fix: "Precise retry fix", BlockerIDs: []string{"b1"}, VerificationItemID: verification.ID}
	env := api.Envelope{Kind: "request", Subject: "Verify exact assigned fixture fix", Review: &meta, Body: api.EnvelopeBody{Ask: "Verify precise fix"}}
	if _, err = f.post(env, verifier.ID, 0, api.Agent{}); !errors.Is(err, api.ErrConflict) {
		t.Fatal("unlinked verifier", err)
	}
	req := f.req(env, verifier.ID, 0, api.Agent{})
	req.WorkItems = append(req.WorkItems, api.MessageWorkItem{ItemTaskID: f.task.ID, ItemID: verification.ID, ItemRevision: 1, Relationship: "related"})
	request, err := f.s.PostMessage(f.ctx, f.task.ID, req, f.by)
	if err != nil {
		t.Fatal("linked verifier", err)
	}
	wrong := meta
	wrong.VerificationItemID = f.item.ID
	if _, err = f.post(f.resultEnv(candidateC, map[string]string{"b1": "pass"}, wrong), "", request.Seq, verifier); !errors.Is(err, api.ErrConflict) {
		t.Fatal("wrong verification item", err)
	}
	if _, err = f.post(f.resultEnv(candidateC, map[string]string{"b1": "pass"}, meta), "", request.Seq, verifier); err != nil {
		t.Fatal("exact verifier", err)
	}
}

func TestReviewConvergenceLegacyReviewHistoryNeverBecomesZero(t *testing.T) {
	f := newConvergenceFixture(t)
	// Reconstruct a pre-enforcement native history only in this isolated fixture.
	if _, err := f.s.db.Exec(`DELETE FROM review_convergence WHERE task_id=? AND item_id=?`, f.task.ID, f.item.ID); err != nil {
		t.Fatal(err)
	}
	env := api.Envelope{Kind: "review", Subject: "Legacy exact native fixture review", Body: api.EnvelopeBody{Candidate: candidateA, Scope: "Fixture", Acceptance: map[string]string{"a1": "works", "a2": "retries"}}}
	req := f.req(env, f.reviewer.ID, 0, api.Agent{})
	task, err := f.s.GetTask(f.ctx, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := f.s.db.BeginTx(f.ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.insertMessage(f.ctx, tx, task, req, f.reviewer, f.by, false, false); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	env = api.Envelope{Kind: "assign", Subject: "Assign revised legacy fixture criteria", Body: api.EnvelopeBody{Objective: "Fixture", Owns: []string{"fixture"}, Acceptance: map[string]string{"a1": "works", "a2": "retries"}}}
	if _, err = f.post(env, "", 0, api.Agent{}); err != nil {
		t.Fatal(err)
	}
	if f.state(t).History != "unknown" {
		t.Fatal("legacy history became known zero")
	}
	env = api.Envelope{Kind: "review", Subject: "Attempt fresh legacy fixture review", Body: api.EnvelopeBody{Candidate: candidateA, Scope: "Fixture", Acceptance: map[string]string{"a1": "works", "a2": "retries"}}}
	if _, err = f.post(env, f.reviewer.ID, 0, api.Agent{}); !errors.Is(err, api.ErrConflict) {
		t.Fatal(err)
	}
	done := "done"
	if _, err = f.s.UpdateWorkItem(f.ctx, f.task.ID, f.item.ID, api.UpdateWorkItemRequest{Revision: 1, Status: &done}, f.by); !errors.Is(err, api.ErrConflict) {
		t.Fatal("unknown managed history accepted", err)
	}
}

func acceptCorrectionFixture(t *testing.T, f *convergenceFixture, candidate string) {
	t.Helper()
	meta := api.ReviewMetadata{Mode: "disposition", Disposition: "accept", Candidate: candidate}
	if _, err := f.post(api.Envelope{Kind: "notice", Subject: "Accept corrected fixture candidate", Review: &meta, Body: api.EnvelopeBody{Text: "Accept verified candidate"}}, "", 0, api.Agent{}); err != nil {
		t.Fatal(err)
	}
	done := "done"
	if _, err := f.s.UpdateWorkItem(f.ctx, f.task.ID, f.item.ID, api.UpdateWorkItemRequest{Revision: f.item.Revision, Status: &done}, f.by); err != nil {
		t.Fatal(err)
	}
}

func TestReviewConvergenceCorrectionFrozenScopeDuringOpenRound(t *testing.T) {
	f := newConvergenceFixture(t)
	r := f.review(t, candidateA)
	desc := "Description clarified while exact candidate is under review"
	var err error
	f.item, err = f.s.UpdateWorkItem(f.ctx, f.task.ID, f.item.ID, api.UpdateWorkItemRequest{Revision: f.item.Revision, Description: &desc}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	// RESULT completes against its requested snapshot before a new ASSIGN exists.
	if _, err = f.post(f.resultEnv(candidateA, passConvergence, api.ReviewMetadata{Mode: "general"}), "", r.Seq, f.reviewer); err != nil {
		t.Fatal(err)
	}
	assign, err := f.post(api.Envelope{Kind: "assign", Subject: "Rebind identical corrected fixture scope", Body: api.EnvelopeBody{Objective: "Fixture", Owns: []string{"fixture"}, Acceptance: map[string]string{"a1": "works", "a2": "retries"}}}, "", 0, api.Agent{})
	if err != nil {
		t.Fatal(err)
	}
	st := f.state(t)
	if len(st.Rounds) != 1 || st.Rounds[0].ScopeRevision != 1 || st.Rounds[0].Criteria["a2"] != "retries" || st.Scopes[1].AssignmentSeq != assign.Seq {
		t.Fatal("frozen provenance/count lost", st)
	}
	acceptCorrectionFixture(t, f, candidateA)
}

func TestReviewConvergenceCorrectionDroppedBlockerCriterion(t *testing.T) {
	f := newConvergenceFixture(t)
	r := f.review(t, candidateA)
	b := api.ReviewFinding{ID: "b1", Criterion: "a2", Title: "Retry drops receipt", File: "fixture.go", Line: 7}
	if _, err := f.post(f.resultEnv(candidateA, map[string]string{"a1": "pass", "a2": "fail"}, api.ReviewMetadata{Mode: "general", Blockers: []api.ReviewFinding{b}}), "", r.Seq, f.reviewer); err != nil {
		t.Fatal(err)
	}
	desc := "Owner removes retries from bounded scope"
	var err error
	f.item, err = f.s.UpdateWorkItem(f.ctx, f.task.ID, f.item.ID, api.UpdateWorkItemRequest{Revision: f.item.Revision, Description: &desc}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.post(api.Envelope{Kind: "assign", Subject: "Assign explicitly reduced fixture scope", Body: api.EnvelopeBody{Objective: "Fixture", Owns: []string{"fixture"}, Acceptance: map[string]string{"a1": "works"}}}, "", 0, api.Agent{}); err != nil {
		t.Fatal(err)
	}
	r2, err := f.post(api.Envelope{Kind: "review", Subject: "Review explicitly reduced fixture scope", Body: api.EnvelopeBody{Candidate: candidateB, Scope: "Reduced scope", Acceptance: map[string]string{"a1": "works"}}}, f.reviewer.ID, 0, api.Agent{})
	if err != nil {
		t.Fatal(err)
	}
	meta := api.ReviewMetadata{Mode: "general", BlockerIDs: []string{"b1"}}
	if _, err = f.post(f.resultEnv(candidateB, map[string]string{"a1": "pass"}, meta), "", r2.Seq, f.reviewer); !errors.Is(err, api.ErrConflict) {
		t.Fatal("silent removal", err)
	}
	meta.Fix = "Scope revision 2 and its ASSIGN explicitly remove retries; b1 is out of scope, not passed"
	if _, err = f.post(f.resultEnv(candidateB, map[string]string{"a1": "pass"}, meta), "", r2.Seq, f.reviewer); err != nil {
		t.Fatal(err)
	}
	st := f.state(t)
	if len(st.Rounds) != 2 || len(outstanding(st)) != 0 || st.Rounds[1].Verdicts["a2"] != "" || st.Focused[0].Fix == "" || len(st.Focused[0].ScopeResolvedIDs) != 1 {
		t.Fatal("scope resolution provenance", st)
	}
	acceptCorrectionFixture(t, f, candidateB)
}

func TestReviewConvergenceCorrectionDemotedCriterionExactVerification(t *testing.T) {
	for _, asBlocker := range []bool{true, false} {
		t.Run(fmt.Sprintf("blocker=%v", asBlocker), func(t *testing.T) {
			f := newConvergenceFixture(t)
			r := f.review(t, candidateA)
			if _, err := f.post(f.resultEnv(candidateA, passConvergence, api.ReviewMetadata{Mode: "general"}), "", r.Seq, f.reviewer); err != nil {
				t.Fatal(err)
			}
			r2 := f.review(t, candidateB)
			b := api.ReviewFinding{ID: "b2", Criterion: "a2", Title: "Retry drops receipt", Command: "fixture retry", Output: "FAIL missing receipt"}
			meta := api.ReviewMetadata{Mode: "general"}
			if asBlocker {
				meta.Blockers = []api.ReviewFinding{b}
			} else {
				meta.Findings = []api.ReviewFinding{b}
			}
			if !asBlocker {
				if _, err := f.post(f.resultEnv(candidateB, passConvergence, meta), "", r2.Seq, f.reviewer); !errors.Is(err, api.ErrConflict) {
					t.Fatal("false passing criterion accepted", err)
				}
			}
			if _, err := f.post(f.resultEnv(candidateB, map[string]string{"a1": "pass", "a2": "fail"}, meta), "", r2.Seq, f.reviewer); err != nil {
				t.Fatal(err)
			}
			st := f.state(t)
			if len(st.Rounds[1].Blockers) != 0 || len(st.FollowUps) != 1 || len(outstanding(st)) != 1 {
				t.Fatal("demotion semantics", st)
			}
			focus := api.ReviewMetadata{Mode: "focused", Candidate: candidateC, Fix: "Restore receipt on retry", BlockerIDs: []string{"b2"}}
			fm, err := f.post(api.Envelope{Kind: "request", Subject: "Verify exactly the demoted receipt failure", Review: &focus, Body: api.EnvelopeBody{Ask: "Verify exact receipt fix"}}, f.reviewer.ID, 0, api.Agent{})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = f.post(f.resultEnv(candidateC, map[string]string{"b2": "pass"}, focus), "", fm.Seq, f.reviewer); err != nil {
				t.Fatal(err)
			}
			st = f.state(t)
			if st.Rounds[1].Verdicts["a2"] != "fail" || len(st.Rounds) != 2 || !st.Focused[0].Passed {
				t.Fatal("history rewritten", st)
			}
			acceptCorrectionFixture(t, f, candidateC)
		})
	}
}

func TestReviewConvergenceCorrectionLegacyReconciliation(t *testing.T) {
	for _, count := range []int{1, 2, 3} {
		t.Run(fmt.Sprintf("requests=%d", count), func(t *testing.T) {
			f := newConvergenceFixture(t)
			if _, err := f.s.db.Exec(`DELETE FROM review_convergence WHERE task_id=? AND item_id=?`, f.task.ID, f.item.ID); err != nil {
				t.Fatal(err)
			}
			ids := []int64{}
			for i := 0; i < count; i++ {
				env := api.Envelope{Kind: "review", Subject: "Legacy native fixture review request", Body: api.EnvelopeBody{Candidate: candidateA, Scope: "Fixture", Acceptance: map[string]string{"a1": "works", "a2": "retries"}}}
				req := f.req(env, f.reviewer.ID, 0, api.Agent{})
				task, _ := f.s.GetTask(f.ctx, f.task.ID)
				tx, err := f.s.db.BeginTx(f.ctx, nil)
				if err != nil {
					t.Fatal(err)
				}
				m, err := f.s.insertMessage(f.ctx, tx, task, req, f.reviewer, f.by, false, false)
				if err != nil {
					tx.Rollback()
					t.Fatal(err)
				}
				if err = tx.Commit(); err != nil {
					t.Fatal(err)
				}
				ids = append(ids, m.Seq)
			}
			if _, err := f.post(api.Envelope{Kind: "assign", Subject: "Assign correction of legacy fixture", Body: api.EnvelopeBody{Objective: "Fixture", Owns: []string{"fixture"}, Acceptance: map[string]string{"a1": "works", "a2": "retries"}}}, "", 0, api.Agent{}); err != nil {
				t.Fatal(err)
			}
			if f.state(t).History != "unknown" {
				t.Fatal("guessed zero")
			}
			meta := api.ReviewMetadata{Mode: "reconcile", LegacyRequests: ids, Fix: "Enumerate native legacy requests; retain all lifetime slots and reattest original frozen candidates"}
			env := api.Envelope{Kind: "notice", Subject: "Reconcile source backed legacy review rounds", Review: &meta, Body: api.EnvelopeBody{Text: "Preserve exact source history"}, Evidence: map[string]api.Evidence{"e1": {Type: "record", Value: fmt.Sprintf("Native source requests %v", ids)}}}
			if count > 1 {
				bad := env
				badMeta := meta
				badMeta.LegacyRequests = ids[:1]
				bad.Review = &badMeta
				if _, err := f.post(bad, "", 0, api.Agent{}); !errors.Is(err, api.ErrConflict) {
					t.Fatal("count reset accepted", err)
				}
			}
			reconcileReq := f.req(env, "", 0, api.Agent{})
			reconciliation, err := f.s.PostMessage(f.ctx, f.task.ID, reconcileReq, f.by)
			if count > 2 {
				if !errors.Is(err, api.ErrConflict) || f.state(t).History != "unknown" {
					t.Fatal("legacy cap override", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			again, replayErr := f.s.PostMessage(f.ctx, f.task.ID, reconcileReq, f.by)
			if replayErr != nil || again.Seq != reconciliation.Seq {
				t.Fatal("reconciliation replay", replayErr)
			}
			st := f.state(t)
			if len(st.Rounds) != count || st.Reconciliations[0].MessageSeq != reconciliation.Seq || st.Rounds[0].ReconciliationSeq != reconciliation.Seq {
				t.Fatal("source/count lost", st)
			}
			if count == 2 {
				if _, err := f.post(f.resultEnv(candidateA, passConvergence, api.ReviewMetadata{Mode: "general"}), "", ids[1], f.reviewer); !errors.Is(err, api.ErrConflict) {
					t.Fatal("skipped first result", err)
				}
			}
			for _, id := range ids {
				if _, err := f.post(f.resultEnv(candidateA, passConvergence, api.ReviewMetadata{Mode: "general"}), "", id, f.reviewer); err != nil {
					t.Fatal(err)
				}
			}
			if count == 1 {
				r2 := f.review(t, candidateA)
				if _, err := f.post(f.resultEnv(candidateA, passConvergence, api.ReviewMetadata{Mode: "general"}), "", r2.Seq, f.reviewer); err != nil {
					t.Fatal(err)
				}
			}
			// Restart retains imported counts, sources, and exact current reviewer identity.
			f.s.Close()
			f.s, err = Open(f.path)
			if err != nil {
				t.Fatal(err)
			}
			if len(f.state(t).Rounds) != 2 {
				t.Fatal("restart count")
			}
			env2 := api.Envelope{Kind: "review", Subject: "Try forbidden third legacy review", Body: api.EnvelopeBody{Candidate: candidateA, Scope: "Fixture", Acceptance: passConvergence}}
			if _, err = f.post(env2, f.reviewer.ID, 0, api.Agent{}); !errors.Is(err, api.ErrConflict) {
				t.Fatal("third admitted", err)
			}
			acceptCorrectionFixture(t, f, candidateA)
		})
	}
}

func TestReviewConvergenceFocusedLegacyReplacementPreservesSources(t *testing.T) {
	f := newConvergenceFixture(t)
	if _, err := f.s.db.Exec(`DELETE FROM review_convergence WHERE task_id=? AND item_id=?`, f.task.ID, f.item.ID); err != nil {
		t.Fatal(err)
	}
	sources := []int64{}
	for i := 0; i < 2; i++ {
		env := api.Envelope{Kind: "review", Subject: "Legacy review from the now closed team", Body: api.EnvelopeBody{Candidate: candidateA, Scope: "Fixture", Acceptance: map[string]string{"a1": "works", "a2": "retries"}}}
		req := f.req(env, f.reviewer.ID, 0, api.Agent{})
		task, _ := f.s.GetTask(f.ctx, f.task.ID)
		tx, err := f.s.db.BeginTx(f.ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		m, err := f.s.insertMessage(f.ctx, tx, task, req, f.reviewer, f.by, false, false)
		if err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
		if err = tx.Commit(); err != nil {
			t.Fatal(err)
		}
		sources = append(sources, m.Seq)
	}
	if _, err := f.s.db.Exec(`UPDATE agents SET status=? WHERE id=?`, api.AgentClosed, f.reviewer.ID); err != nil {
		t.Fatal(err)
	}
	fresh, err := f.s.AddAgent(f.ctx, f.task.ID, api.AddAgentRequest{Name: "reviewer-fresh", Host: "fixture", Session: "reviewer-fresh"}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.post(api.Envelope{Kind: "assign", Subject: "Fresh team assignment preserves legacy criteria", Body: api.EnvelopeBody{Objective: "Fixture", Owns: []string{"fixture"}, Acceptance: map[string]string{"a1": "works", "a2": "retries"}}}, "", 0, api.Agent{}); err != nil {
		t.Fatal(err)
	}
	meta := api.ReviewMetadata{Mode: "reconcile", LegacyRequests: sources, Fix: "Original team closed; explicitly bind fresh exact reviewer while retaining both source slots"}
	env := api.Envelope{Kind: "notice", Subject: "Reconcile legacy requests with the fresh reviewer", Review: &meta, Body: api.EnvelopeBody{Text: "Preserve legacy source identity"}, Evidence: map[string]api.Evidence{"e1": {Type: "record", Value: "Both native legacy source REVIEW requests"}}}
	if _, err = f.post(env, "", 0, api.Agent{}); !errors.Is(err, api.ErrConflict) {
		t.Fatal("missing replacement accepted", err)
	}
	for _, seq := range sources {
		meta.LegacyReviewers = append(meta.LegacyReviewers, api.LegacyReviewerBinding{RequestSeq: seq, ReviewerID: fresh.ID, ReviewerRun: fresh.RunID})
	}
	staleMeta := meta
	staleMeta.LegacyReviewers = append([]api.LegacyReviewerBinding{}, meta.LegacyReviewers...)
	staleMeta.LegacyReviewers[0].ReviewerRun = "run_bbbbbbbbbbbbbbbb"
	bad := env
	bad.Review = &staleMeta
	if _, err = f.post(bad, "", 0, api.Agent{}); !errors.Is(err, api.ErrConflict) {
		t.Fatal("stale replacement accepted", err)
	}
	reset := meta
	reset.LegacyRequests = sources[:1]
	bad.Review = &reset
	if _, err = f.post(bad, "", 0, api.Agent{}); !errors.Is(err, api.ErrConflict) {
		t.Fatal("count reset accepted", err)
	}
	env.Review = &meta
	notice, err := f.post(env, "", 0, api.Agent{})
	if err != nil {
		t.Fatal(err)
	}
	st := f.state(t)
	if len(st.Rounds) != 2 || len(st.Reconciliations[0].Reviewers) != 2 || st.Reconciliations[0].MessageSeq != notice.Seq {
		t.Fatal("reconciliation provenance", st)
	}
	for i, r := range st.Rounds {
		if r.RequestSeq != sources[i] || r.SourceReviewerID != f.reviewer.ID || r.ReviewerID != fresh.ID || r.ReviewerRun != fresh.RunID || r.Candidate != candidateA {
			t.Fatal("original source lost", r)
		}
	}
	wrong := fresh
	wrong.RunID = "run_cccccccccccccccc"
	if _, err = f.post(f.resultEnv(candidateA, passConvergence, api.ReviewMetadata{Mode: "general"}), "", sources[0], wrong); !errors.Is(err, api.ErrConflict) {
		t.Fatal("stale result accepted", err)
	}
	if _, err = f.post(f.resultEnv(candidateA, passConvergence, api.ReviewMetadata{Mode: "general"}), "", sources[0], f.reviewer); !errors.Is(err, api.ErrConflict) {
		t.Fatal("closed source result accepted", err)
	}
	for _, seq := range sources {
		if _, err = f.post(f.resultEnv(candidateA, passConvergence, api.ReviewMetadata{Mode: "general"}), "", seq, fresh); err != nil {
			t.Fatal(err)
		}
	}
	env = api.Envelope{Kind: "review", Subject: "Refuse a third review after replacement", Body: api.EnvelopeBody{Candidate: candidateA, Scope: "Fixture", Acceptance: map[string]string{"a1": "works", "a2": "retries"}}}
	if _, err = f.post(env, fresh.ID, 0, api.Agent{}); !errors.Is(err, api.ErrConflict) {
		t.Fatal("replacement reset count", err)
	}
	acceptCorrectionFixture(t, f, candidateA)
}

func TestReviewConvergenceFocusedSequentialFinalCandidateReverification(t *testing.T) {
	f := newConvergenceFixture(t)
	r1 := f.review(t, candidateA)
	b1 := api.ReviewFinding{ID: "b1", Regression: true, Baseline: candidateC, Candidate: candidateA, Title: "Regression one", File: "fixture.go", Line: 1}
	b2 := api.ReviewFinding{ID: "b2", Regression: true, Baseline: candidateC, Candidate: candidateA, Title: "Regression two", File: "fixture.go", Line: 2}
	if _, err := f.post(f.resultEnv(candidateA, passConvergence, api.ReviewMetadata{Mode: "general", Blockers: []api.ReviewFinding{b1, b2}}), "", r1.Seq, f.reviewer); err != nil {
		t.Fatal(err)
	}
	r2 := f.review(t, candidateB)
	b1.Candidate = candidateB
	b2.Candidate = candidateB
	if _, err := f.post(f.resultEnv(candidateB, passConvergence, api.ReviewMetadata{Mode: "general", Blockers: []api.ReviewFinding{b1, b2}}), "", r2.Seq, f.reviewer); err != nil {
		t.Fatal(err)
	}
	const d = "dddddddddddddddddddddddddddddddddddddddd"
	const e = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	verify := func(candidate, id, fix string) {
		t.Helper()
		meta := api.ReviewMetadata{Mode: "focused", Candidate: candidate, Fix: fix, BlockerIDs: []string{id}}
		request, err := f.post(api.Envelope{Kind: "request", Subject: "Verify exactly this regression fix", Review: &meta, Body: api.EnvelopeBody{Ask: "Verify exact fix on named candidate"}}, f.reviewer.ID, 0, api.Agent{})
		if err != nil {
			t.Fatal(err)
		}
		stale := f.reviewer
		stale.RunID = "run_dddddddddddddddd"
		if _, err = f.post(f.resultEnv(candidate, map[string]string{id: "pass"}, meta), "", request.Seq, stale); !errors.Is(err, api.ErrConflict) {
			t.Fatal("wrong verifier run", err)
		}
		if _, err = f.post(f.resultEnv(candidate, map[string]string{id: "pass"}, meta), "", request.Seq, f.reviewer); err != nil {
			t.Fatal(err)
		}
	}
	verify(d, "b1", "Fix regression one")
	verify(e, "b2", "Fix regression two")
	accept := func(candidate string) error {
		meta := api.ReviewMetadata{Mode: "disposition", Disposition: "accept", Candidate: candidate}
		_, err := f.post(api.Envelope{Kind: "notice", Subject: "Accept exact focused verification candidate", Review: &meta, Body: api.EnvelopeBody{Text: "Accept"}}, "", 0, api.Agent{})
		return err
	}
	if err := accept(e); !errors.Is(err, api.ErrConflict) {
		t.Fatal("earlier b1 verification silently carried forward", err)
	}
	verify(e, "b1", "Reverify regression one on final candidate after second fix")
	if err := accept(d); !errors.Is(err, api.ErrConflict) {
		t.Fatal("stale candidate accepted", err)
	}
	if err := accept(candidateC); !errors.Is(err, api.ErrConflict) {
		t.Fatal("unrelated candidate accepted", err)
	}
	st := f.state(t)
	if len(st.Rounds) != 2 || len(st.Focused) != 3 || len(outstandingOnCandidate(st, e)) != 0 {
		t.Fatal("focused count/evidence", st)
	}
	acceptCorrectionFixture(t, f, e)
}
