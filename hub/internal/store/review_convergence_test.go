package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/scs32/tailterm/hub/internal/api"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
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
	s                    *Store
	ctx                  context.Context
	by                   api.Caller
	task                 api.Task
	item                 api.WorkItem
	reviewer             api.Agent
	builder              api.Agent
	lead                 api.Agent
	criteria             map[string]string
	verificationCriteria []string
	path                 string
	serial               int
}

func newConvergenceFixture(t *testing.T) *convergenceFixture {
	return newConvergenceFixtureWithCriteria(t, map[string]string{"a1": "works", "a2": "retries"}, nil)
}
func newConvergenceFixtureWithCriteria(t *testing.T, criteria map[string]string, verificationCriteria []string) *convergenceFixture {
	t.Helper()
	f := &convergenceFixture{ctx: context.Background(), by: api.Caller{Node: "fixture", User: "owner"}, path: filepath.Join(t.TempDir(), "review.sqlite"), criteria: criteria, verificationCriteria: verificationCriteria}
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
	f.builder = f.reviewer
	if len(verificationCriteria) > 0 {
		f.lead, err = f.s.AddAgent(f.ctx, f.task.ID, api.AddAgentRequest{Name: "lead", Host: "fixture", Session: "lead"}, f.by)
		if err != nil {
			t.Fatal(err)
		}
		name := f.lead.Name
		if _, err = f.s.UpdateTask(f.ctx, f.task.ID, api.UpdateTaskRequest{Orchestrator: &name}, f.by); err != nil {
			t.Fatal(err)
		}
		f.builder, err = f.s.AddAgent(f.ctx, f.task.ID, api.AddAgentRequest{Name: "builder", Host: "fixture", Session: "builder"}, f.by)
		if err != nil {
			t.Fatal(err)
		}
	}
	env := api.Envelope{Kind: "assign", Subject: "Implement frozen fixture criteria", Body: api.EnvelopeBody{Objective: "Fixture", Owns: []string{"fixture"}, Acceptance: criteria, VerificationCriteria: verificationCriteria}}
	if _, err = f.post(env, f.builder.ID, 0, api.Agent{}); err != nil {
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
	m, err := f.post(api.Envelope{Kind: "review", Subject: "Review frozen fixture candidate", Body: api.EnvelopeBody{Candidate: candidate, Scope: "Fixture", Acceptance: f.criteria, VerificationCriteria: f.verificationCriteria}}, f.reviewer.ID, 0, api.Agent{})
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

func TestReviewConvergenceVerificationOwnership(t *testing.T) {
	unenrolled := newConvergenceFixtureWithCriteria(t, map[string]string{"a1": "code", "a2": "matrix"}, []string{"a2"})
	tx, err := unenrolled.s.db.BeginTx(unenrolled.ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	required, err := verificationRequired(unenrolled.ctx, tx, unenrolled.task.ID, unenrolled.item.ID)
	if err != nil || required {
		t.Fatal("expected unenrolled fixture", required, err)
	}
	if err := verificationReady(unenrolled.ctx, tx, unenrolled.item, candidateA); !errors.Is(err, api.ErrConflict) {
		t.Fatal("unenrolled designation bypassed receipt", err)
	}
	tx.Rollback()
	f, _, _ := verificationFixture(t, verificationOwnershipFixture{map[string]string{"a1": "code", "a2": "matrix"}, []string{"a2"}})
	state := f.state(t)
	if len(state.Scopes) != 1 || state.Scopes[0].VerificationCriteria[0] != "a2" {
		t.Fatal(state)
	}
	changed := api.Envelope{Kind: "assign", Subject: "Change frozen verification ownership", Body: api.EnvelopeBody{Objective: "fixture", Owns: []string{"fixture"}, Acceptance: f.criteria}}
	if _, err := f.post(changed, f.builder.ID, 0, api.Agent{}); !errors.Is(err, api.ErrConflict) {
		t.Fatal("reclassification", err)
	}
	bad := api.Envelope{Kind: "review", Subject: "Review with changed verification ownership", Body: api.EnvelopeBody{Candidate: candidateA, Scope: "fixture", Acceptance: f.criteria}}
	if _, err := f.post(bad, f.reviewer.ID, 0, api.Agent{}); !errors.Is(err, api.ErrConflict) {
		t.Fatal("review ownership", err)
	}
	r := f.review(t, candidateA)
	for _, status := range []map[string]string{{"a1": "pass", "a2": "partial"}, {"a1": "pending-verification", "a2": "pending-verification"}} {
		if _, err := f.post(f.resultEnv(candidateA, status, api.ReviewMetadata{Mode: "general"}), "", r.Seq, f.reviewer); !errors.Is(err, api.ErrConflict) {
			t.Fatal("invalid verdict ownership", err)
		}
	}
	if _, err := f.post(f.resultEnv(candidateA, map[string]string{"a1": "pass", "a2": "pending-verification"}, api.ReviewMetadata{Mode: "general"}), "", r.Seq, f.reviewer); err != nil {
		t.Fatal(err)
	}
	state = f.state(t)
	if state.Rounds[0].Verdicts["a2"] != "pending-verification" || state.Rounds[0].VerificationCriteria[0] != "a2" {
		t.Fatal(state)
	}
}

// wi_46d43d4798b646bf: a new scope keeps at least one reviewer-judged criterion.
func TestReviewConvergenceVerificationNeedsReviewerCriterion(t *testing.T) {
	const reason = "at least one criterion must stay reviewer-judged"
	t.Run("every criterion designated", func(t *testing.T) {
		f := newConvergenceFixture(t)
		f.rescope(t, "Designate every criterion")
		refusedWith(t, "all designated", f.assign(map[string]string{"a1": "builds", "a2": "matrix passes"}, []string{"a1", "a2"}), reason)
		if scopes := f.state(t).Scopes; len(scopes) != 1 {
			t.Fatal("refused ASSIGN saved a scope", scopes)
		}
	})
	t.Run("single designated criterion", func(t *testing.T) {
		f := newConvergenceFixture(t)
		f.rescope(t, "Designate the only criterion")
		refusedWith(t, "only criterion designated", f.assign(map[string]string{"a1": "matrix passes"}, []string{"a1"}), reason)
		if scopes := f.state(t).Scopes; len(scopes) != 1 {
			t.Fatal("refused ASSIGN saved a scope", scopes)
		}
	})
	t.Run("one reviewer criterion allowed", func(t *testing.T) {
		f := newConvergenceFixture(t)
		f.rescope(t, "Designate the matrix criterion")
		f.criteria = map[string]string{"a1": "builds", "a2": "matrix passes"}
		f.verificationCriteria = []string{"a2"}
		if err := f.assign(f.criteria, f.verificationCriteria); err != nil {
			t.Fatal(err)
		}
		// REVIEW repeats the ASSIGN's IDs, so it cannot widen the designation.
		f.verificationCriteria = []string{"a1", "a2"}
		refusedWith(t, "widened review", f.reviewErr(candidateA), "review must use frozen assignment criteria")
		f.verificationCriteria = []string{"a2"}
		r := f.review(t, candidateA)
		if _, err := f.post(f.resultEnv(candidateA, map[string]string{"a1": "pass", "a2": "pending-verification"}, api.ReviewMetadata{Mode: "general"}), "", r.Seq, f.reviewer); err != nil {
			t.Fatal(err)
		}
		state := f.state(t)
		if len(state.Scopes) != 2 || len(state.Rounds) != 1 || state.Rounds[0].Verdicts["a1"] != "pass" || state.Rounds[0].Verdicts["a2"] != "pending-verification" {
			t.Fatal(state)
		}
	})
}

func TestProbeReclassifiedBlockerWedge(t *testing.T) {
	f := newConvergenceFixture(t)
	r1 := f.review(t, candidateA)
	blocker := api.ReviewFinding{ID: "b1", Criterion: "a2", Title: "Retry loses state", File: "fixture.go", Line: 7}
	failed := map[string]string{"a1": "pass", "a2": "fail"}
	if _, err := f.post(f.resultEnv(candidateA, failed, api.ReviewMetadata{Mode: "general", Blockers: []api.ReviewFinding{blocker}}), "", r1.Seq, f.reviewer); err != nil {
		t.Fatal(err)
	}
	desc := "scope revision after first completed review"
	var err error
	f.item, err = f.s.UpdateWorkItem(f.ctx, f.task.ID, f.item.ID, api.UpdateWorkItemRequest{Revision: f.item.Revision, Description: &desc}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	badAssign := api.Envelope{Kind: "assign", Subject: "Attempt verification reclassification", Body: api.EnvelopeBody{Objective: "fixture", Owns: []string{"fixture"}, Acceptance: f.criteria, VerificationCriteria: []string{"a2"}}}
	if _, err := f.post(badAssign, f.builder.ID, 0, api.Agent{}); !errors.Is(err, api.ErrConflict) {
		t.Fatal("reclassified blocked criterion", err)
	}
	goodAssign := badAssign
	goodAssign.Body.VerificationCriteria = nil
	if _, err := f.post(goodAssign, f.builder.ID, 0, api.Agent{}); err != nil {
		t.Fatal(err)
	}
	badReview := api.Envelope{Kind: "review", Subject: "Attempt review reclassification", Body: api.EnvelopeBody{Candidate: candidateB, Scope: "fixture", Acceptance: f.criteria, VerificationCriteria: []string{"a2"}}}
	if _, err := f.post(badReview, f.reviewer.ID, 0, api.Agent{}); !errors.Is(err, api.ErrConflict) {
		t.Fatal("review reclassified blocked criterion", err)
	}
	r2 := f.review(t, candidateB)
	if _, err := f.post(f.resultEnv(candidateB, failed, api.ReviewMetadata{Mode: "general", Blockers: []api.ReviewFinding{blocker}}), "", r2.Seq, f.reviewer); err != nil {
		t.Fatal(err)
	}
	decision := api.Envelope{Kind: "notice", Subject: "Record owner decision for unresolved blocker", Body: api.EnvelopeBody{Text: "decision"}, Review: &api.ReviewMetadata{Mode: "disposition", Disposition: "owner-decision", Candidate: candidateB}}
	if _, err := f.post(decision, "", 0, api.Agent{}); err != nil {
		t.Fatal(err)
	}
	decision.Review.Disposition = "owner-accept"
	if _, err := f.post(decision, "", 0, api.Agent{}); err != nil {
		t.Fatal(err)
	}
	state := f.state(t)
	if len(state.Rounds) != 2 || state.Rounds[1].ResultSeq == 0 || state.Disposition == nil || state.Disposition.Kind != "owner-accept" {
		t.Fatal("owner path wedged", state)
	}
	if _, err := f.post(badReview, f.reviewer.ID, 0, api.Agent{}); !errors.Is(err, api.ErrConflict) {
		t.Fatal("third round", err)
	}
}

func TestReviewConvergenceVerificationReceiptAcceptance(t *testing.T) {
	f, h, p := verificationFixture(t, verificationOwnershipFixture{map[string]string{"a1": "code", "a2": "matrix"}, []string{"a2"}})
	r := f.review(t, candidateA)
	if _, err := f.post(f.resultEnv(candidateA, map[string]string{"a1": "pass", "a2": "pending-verification"}, api.ReviewMetadata{Mode: "general"}), "", r.Seq, f.reviewer); err != nil {
		t.Fatal(err)
	}
	accept := api.Envelope{Kind: "notice", Subject: "Accept verified fixture candidate", Body: api.EnvelopeBody{Text: "accept"}, Review: &api.ReviewMetadata{Mode: "disposition", Disposition: "accept", Candidate: candidateA}}
	if _, err := f.post(accept, "", 0, f.lead); !errors.Is(err, api.ErrConflict) {
		t.Fatal("missing receipt", err)
	}
	before := f.state(t)
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	f.s = reopened
	if got := f.state(t); got.Rounds[0].Verdicts["a2"] != "pending-verification" {
		t.Fatal("restart lost frozen verdict", got)
	}
	saveFixtureVerification(t, f, h, p, 0)
	if _, err := f.post(accept, "", 0, f.lead); err != nil {
		t.Fatal(err)
	}
	after := f.state(t)
	if after.Disposition == nil || after.Disposition.Kind != "accept" || len(after.Rounds) != 1 || after.Rounds[0].Verdicts["a2"] != "pending-verification" || before.Rounds[0].ResultSeq != after.Rounds[0].ResultSeq {
		t.Fatal("review history changed", after)
	}
	tx, err := f.s.db.BeginTx(f.ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := reviewCompletion(f.ctx, tx, f.item, candidateA); err != nil {
		t.Fatal(err)
	}
	tx.Rollback()
}

func TestReviewConvergenceVerificationReceiptRefusals(t *testing.T) {
	for _, tc := range []struct {
		name     string
		reviewer string
		receipt  bool
		stale    bool
	}{
		{"reviewer-partial", "partial", true, false}, {"reviewer-failed", "fail", true, false},
		{"missing-receipt", "pass", false, false}, {"stale-receipt", "pass", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, h, p := verificationFixture(t, verificationOwnershipFixture{map[string]string{"a1": "code", "a2": "matrix"}, []string{"a2"}})
			r := f.review(t, candidateA)
			status := map[string]string{"a1": tc.reviewer, "a2": "pending-verification"}
			if _, err := f.post(f.resultEnv(candidateA, status, api.ReviewMetadata{Mode: "general"}), "", r.Seq, f.reviewer); err != nil {
				t.Fatal(err)
			}
			if tc.receipt {
				if tc.stale {
					p.Commit = candidateC
				}
				saveFixtureVerification(t, f, h, p, 0)
			}
			if tc.stale {
				if err := reviewReady(f.state(t), f.item.ScopeRevision, candidateA); err != nil {
					t.Fatal("review gate should pass before stale receipt gate", err)
				}
			}
			accept := api.Envelope{Kind: "notice", Subject: "Attempt fixture candidate acceptance", Body: api.EnvelopeBody{Text: "accept"}, Review: &api.ReviewMetadata{Mode: "disposition", Disposition: "accept", Candidate: candidateA}}
			if _, err := f.post(accept, "", 0, f.lead); !errors.Is(err, api.ErrConflict) {
				t.Fatal("gate bypass", err)
			}
			if f.state(t).Disposition != nil {
				t.Fatal("failed acceptance wrote disposition")
			}
		})
	}
	t.Run("stale-scope-receipt", func(t *testing.T) {
		f, h, p := verificationFixture(t, verificationOwnershipFixture{map[string]string{"a1": "code", "a2": "matrix"}, []string{"a2"}})
		saveFixtureVerification(t, f, h, p, 0)
		desc := "scope revision invalidates prior receipt"
		var err error
		f.item, err = f.s.UpdateWorkItem(f.ctx, f.task.ID, f.item.ID, api.UpdateWorkItemRequest{Revision: f.item.Revision, Description: &desc}, f.by)
		if err != nil {
			t.Fatal(err)
		}
		assign := api.Envelope{Kind: "assign", Subject: "Assign current verification scope", Body: api.EnvelopeBody{Objective: "fixture", Owns: []string{"fixture"}, Acceptance: f.criteria, VerificationCriteria: f.verificationCriteria}}
		if _, err := f.post(assign, f.builder.ID, 0, f.lead); err != nil {
			t.Fatal(err)
		}
		r := f.review(t, candidateA)
		if _, err := f.post(f.resultEnv(candidateA, map[string]string{"a1": "pass", "a2": "pending-verification"}, api.ReviewMetadata{Mode: "general"}), "", r.Seq, f.reviewer); err != nil {
			t.Fatal(err)
		}
		if err := reviewReady(f.state(t), f.item.ScopeRevision, candidateA); err != nil {
			t.Fatal("current review gate should pass", err)
		}
		accept := api.Envelope{Kind: "notice", Subject: "Attempt current scope acceptance", Body: api.EnvelopeBody{Text: "accept"}, Review: &api.ReviewMetadata{Mode: "disposition", Disposition: "accept", Candidate: candidateA}}
		if _, err := f.post(accept, "", 0, f.lead); !errors.Is(err, api.ErrConflict) {
			t.Fatal("stale scope receipt accepted", err)
		}
	})
}

func TestReviewConvergenceVerificationHistoryFixtures(t *testing.T) {
	for _, history := range []struct {
		name  string
		count int
	}{{"DeploymentAgentA7", 7}, {"TokenAccountingA8", 8}} {
		t.Run(history.name, func(t *testing.T) {
			criteria := map[string]string{}
			status := map[string]string{}
			for i := 1; i <= history.count; i++ {
				id := fmt.Sprintf("a%d", i)
				criteria[id] = "synthetic criterion " + id
				status[id] = "pass"
			}
			owned := fmt.Sprintf("a%d", history.count)
			status[owned] = "pending-verification"
			f, h, p := verificationFixture(t, verificationOwnershipFixture{criteria, []string{owned}})
			for i := 0; i < 2; i++ {
				r := f.review(t, candidateA)
				if _, err := f.post(f.resultEnv(candidateA, status, api.ReviewMetadata{Mode: "general"}), "", r.Seq, f.reviewer); err != nil {
					t.Fatal(err)
				}
			}
			accept := api.Envelope{Kind: "notice", Subject: "Accept historical fixture candidate", Body: api.EnvelopeBody{Text: "accept"}, Review: &api.ReviewMetadata{Mode: "disposition", Disposition: "accept", Candidate: candidateA}}
			if _, err := f.post(accept, "", 0, f.lead); !errors.Is(err, api.ErrConflict) {
				t.Fatal("accepted without receipt", err)
			}
			before := f.state(t)
			saveFixtureVerification(t, f, h, p, 0)
			if _, err := f.post(accept, "", 0, f.lead); err != nil {
				t.Fatal(err)
			}
			after := f.state(t)
			if after.Disposition == nil || after.Disposition.Kind != "accept" || len(after.Rounds) != 2 || before.Rounds[1].Verdicts[owned] != after.Rounds[1].Verdicts[owned] {
				t.Fatal("history changed", after)
			}
			if _, err := f.post(api.Envelope{Kind: "review", Subject: "Attempt forbidden third general review", Body: api.EnvelopeBody{Candidate: candidateA, Scope: "fixture", Acceptance: criteria, VerificationCriteria: []string{owned}}}, f.reviewer.ID, 0, f.lead); !errors.Is(err, api.ErrConflict) {
				t.Fatal("third general review", err)
			}
		})
	}
	for _, variant := range []string{"partial", "ineligible-receipt"} {
		t.Run(variant, func(t *testing.T) {
			criteria := map[string]string{"a1": "code", "a2": "integration", "a3": "matrix"}
			f, h, p := verificationFixture(t, verificationOwnershipFixture{criteria, []string{"a3"}})
			status := map[string]string{"a1": "pass", "a2": "pass", "a3": "pending-verification"}
			if variant == "partial" {
				status["a2"] = "partial"
			}
			for i := 0; i < 2; i++ {
				r := f.review(t, candidateA)
				if _, err := f.post(f.resultEnv(candidateA, status, api.ReviewMetadata{Mode: "general"}), "", r.Seq, f.reviewer); err != nil {
					t.Fatal(err)
				}
			}
			if variant == "partial" {
				saveFixtureVerification(t, f, h, p, 0)
			} else {
				_, err := f.s.SaveVerification(f.ctx, f.task.ID, f.item.ID, api.VerificationRequest{RequestID: "ineligible-plan", AgentID: h.ID, RunID: h.RunID, Plan: &p})
				if err != nil {
					t.Fatal(err)
				}
				r := passingVerification(p)
				r.Checks[0].ExitCode = 1
				r.Checks[0].FailureReason = "exit"
				if _, err := f.s.SaveVerification(f.ctx, f.task.ID, f.item.ID, api.VerificationRequest{RequestID: "ineligible-receipt", AgentID: h.ID, RunID: h.RunID, ExpectedGeneration: 1, Receipt: &r}); err != nil {
					t.Fatal(err)
				}
			}
			accept := api.Envelope{Kind: "notice", Subject: "Reject historical fixture candidate", Body: api.EnvelopeBody{Text: "accept"}, Review: &api.ReviewMetadata{Mode: "disposition", Disposition: "accept", Candidate: candidateA}}
			if _, err := f.post(accept, "", 0, f.lead); !errors.Is(err, api.ErrConflict) {
				t.Fatal("gate bypass", err)
			}
		})
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
	seedPassingVerification(t, f.s, f.item, candidateC)
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
	// Reassignment saves the whole ledger, with its recorded stage.
	if len(state.Stages) != 1 || state.Stages[0].Number != 1 || state.Stages[0].AssignmentSeq != state.Scopes[0].AssignmentSeq {
		t.Fatal("reassignment lost the recorded stage", state.Stages)
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

// findingTitleReason is the reason a client reads after "invalid request: ":
// the text that follows api.ErrInvalid in a "%w: reason" refusal.
func findingTitleReason(t *testing.T, err error) string {
	t.Helper()
	var envelopeErr *api.EnvelopeError
	if !errors.Is(err, api.ErrInvalid) || errors.As(err, &envelopeErr) {
		t.Fatalf("want a store refusal wrapping api.ErrInvalid, got %T %v", err, err)
	}
	reason, ok := strings.CutPrefix(err.Error(), api.ErrInvalid.Error()+": ")
	if !ok || reason == "" {
		t.Fatalf("refusal has no named reason: %q", err)
	}
	for _, r := range reason {
		if r < 0x20 || r == 0x7f {
			t.Fatalf("reason carries a control character: %q", reason)
		}
	}
	return reason
}

// A round-two blocker that is new and not a regression is filed as a
// follow-up. The envelope check covers review.findings only, so its title
// first meets the work item title rule here, and the refusal names
// review.blockers, the field the client sent it in.
func TestReviewConvergenceRoundTwoBlockerTitleRefusalNamesFinding(t *testing.T) {
	long := strings.Repeat("x", 121)
	rule := "; a title is 1 to 120 characters"
	for name, tc := range map[string]struct{ title, want string }{
		"too long": {long, "review.blockers[b2].title: must be at most 120 characters, has 121"},
		"padded":   {" Retry drops receipt ", "review.blockers[b2].title: must not start or end with a space" + rule},
		"control":  {"Retry drops\x1b[31m receipt\x00", "review.blockers[b2].title: must not contain control characters" + rule},
	} {
		t.Run(name, func(t *testing.T) {
			f := newConvergenceFixture(t)
			r := f.review(t, candidateA)
			if _, err := f.post(f.resultEnv(candidateA, passConvergence, api.ReviewMetadata{Mode: "general"}), "", r.Seq, f.reviewer); err != nil {
				t.Fatal(err)
			}
			r2 := f.review(t, candidateB)
			before, _ := f.s.ListWorkItems(f.ctx, f.task.ID, "", "", 0, 100)
			b := api.ReviewFinding{ID: "b2", Criterion: "a2", Title: tc.title, File: "fixture.go", Line: 6}
			failed := map[string]string{"a1": "pass", "a2": "fail"}
			_, err := f.post(f.resultEnv(candidateB, failed, api.ReviewMetadata{Mode: "general", Blockers: []api.ReviewFinding{b}}), "", r2.Seq, f.reviewer)
			if reason := findingTitleReason(t, err); reason != tc.want || strings.Contains(reason, tc.title) {
				t.Fatalf("reason %q, want %q", reason, tc.want)
			}
			after, _ := f.s.ListWorkItems(f.ctx, f.task.ID, "", "", 0, 100)
			st := f.state(t)
			if len(before.Items) != len(after.Items) || st.Rounds[1].ResultSeq != 0 || len(st.FollowUps) != 0 {
				t.Fatal("refused result saved something", len(before.Items), len(after.Items), st)
			}
			// The same result with a title at the limit files the follow-up.
			b.Title = long[:120]
			if _, err = f.post(f.resultEnv(candidateB, failed, api.ReviewMetadata{Mode: "general", Blockers: []api.ReviewFinding{b}}), "", r2.Seq, f.reviewer); err != nil {
				t.Fatal(err)
			}
			st = f.state(t)
			if st.Rounds[1].ResultSeq == 0 || len(st.FollowUps) != 1 {
				t.Fatal("valid title not filed", st)
			}
			follow, err := f.s.GetWorkItem(f.ctx, f.task.ID, st.FollowUps[0].ItemID)
			if err != nil || follow.Title != b.Title || follow.Kind != "bug" || follow.Status != "open" {
				t.Fatal(follow, err)
			}
		})
	}
}

// A blank title is refused like any other title a work item cannot carry: the
// reason names the list the client sent the entry in, the finding ID and
// .title. An entry with no ID has nothing to name and keeps a plain refusal.
func TestReviewFindingBlankTitleRefusalNamesFieldAndFinding(t *testing.T) {
	rule := "; a title is 1 to 120 characters"
	criteria := map[string]string{"a1": "one", "a2": "two"}
	for name, tc := range map[string]struct {
		title   string
		blocker bool
		want    string
	}{
		"empty blocker": {"", true, "review.blockers[x1].title: must not be empty" + rule},
		"blank blocker": {" \t ", true, "review.blockers[x1].title: must not start or end with a space" + rule},
		"empty finding": {"", false, "review.findings[x1].title: must not be empty" + rule},
		"blank finding": {"  ", false, "review.findings[x1].title: must not start or end with a space" + rule},
	} {
		t.Run(name, func(t *testing.T) {
			finding := api.ReviewFinding{ID: "x1", Criterion: "a2", Title: tc.title, File: "fixture.go", Line: 6}
			if reason := findingTitleReason(t, evidenceFinding(finding, candidateA, criteria, tc.blocker)); reason != tc.want {
				t.Fatalf("reason %q, want %q", reason, tc.want)
			}
		})
	}
	for field, blocker := range map[string]bool{"review.blockers": true, "review.findings": false} {
		err := evidenceFinding(api.ReviewFinding{Criterion: "a2", Title: "Has no ID", File: "fixture.go", Line: 6}, candidateA, criteria, blocker)
		if !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), field+" entry needs a stable ID") {
			t.Fatalf("missing ID under %s: %v", field, err)
		}
	}
	// The posted path: a first-round blocker stays a blocker and is never
	// filed, so only this check sees its blank title.
	f := newConvergenceFixture(t)
	r := f.review(t, candidateA)
	failed := map[string]string{"a1": "pass", "a2": "fail"}
	for title, want := range map[string]string{
		"":   "review.blockers[b1].title: must not be empty" + rule,
		"  ": "review.blockers[b1].title: must not start or end with a space" + rule,
	} {
		b := api.ReviewFinding{ID: "b1", Criterion: "a2", Title: title, File: "fixture.go", Line: 6}
		_, err := f.post(f.resultEnv(candidateA, failed, api.ReviewMetadata{Mode: "general", Blockers: []api.ReviewFinding{b}}), "", r.Seq, f.reviewer)
		if reason := findingTitleReason(t, err); reason != want {
			t.Fatalf("posted reason %q, want %q", reason, want)
		}
	}
	noID := api.ReviewFinding{Criterion: "a2", Title: "Has no ID", File: "fixture.go", Line: 6}
	_, err := f.post(f.resultEnv(candidateA, failed, api.ReviewMetadata{Mode: "general", Blockers: []api.ReviewFinding{noID}}), "", r.Seq, f.reviewer)
	if !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "review.blockers entry needs a stable ID") {
		t.Fatal("posted missing ID", err)
	}
	if st := f.state(t); st.Rounds[0].ResultSeq != 0 || len(st.Rounds[0].Blockers) != 0 {
		t.Fatal("refused result saved something", st)
	}
}

// Every refusal that repeats a finding ID caps it at 64 bytes, cut on a
// character boundary, with control characters still replaced.
func TestReviewFindingIDEchoIsCapped(t *testing.T) {
	exact := strings.Repeat("a", 64)
	for name, tc := range map[string]struct{ id, want string }{
		"short":        {"f3", "f3"},
		"at the cap":   {exact, exact},
		"over the cap": {exact + "b", exact + "\u2026"},
		// 63 bytes then a two-byte character: the cut backs up to byte 63.
		"character boundary": {strings.Repeat("a", 63) + "\u00e9tail", strings.Repeat("a", 63) + "\u2026"},
		"multibyte":          {strings.Repeat("\u00e9", 40), strings.Repeat("\u00e9", 32) + "\u2026"},
		"control":            {"f\x1b3\n" + strings.Repeat("z", 70), "f 3 " + strings.Repeat("z", 60) + "\u2026"},
		"invalid utf8":       {"f\xff3", "f 3"},
	} {
		t.Run(name, func(t *testing.T) {
			got := reviewFindingIDEcho(tc.id)
			if got != tc.want || !utf8.ValidString(got) {
				t.Fatalf("echo %q, want %q", got, tc.want)
			}
			rule := "; a title is 1 to 120 characters"
			for _, field := range []string{"review.findings", "review.blockers"} {
				want := field + "[" + tc.want + "].title: must not be empty" + rule
				if reason := findingTitleReason(t, reviewFindingTitleRefusal(field, api.ReviewFinding{ID: tc.id})); reason != want {
					t.Fatalf("reason %q, want %q", reason, want)
				}
			}
		})
	}
	// A round-two result that drops a prior blocker repeats that blocker's ID.
	f := newConvergenceFixture(t)
	long := strings.Repeat("k", 200)
	r := f.review(t, candidateA)
	failed := map[string]string{"a1": "pass", "a2": "fail"}
	b := api.ReviewFinding{ID: long, Criterion: "a2", Title: "Retry drops receipt", File: "fixture.go", Line: 6}
	if _, err := f.post(f.resultEnv(candidateA, failed, api.ReviewMetadata{Mode: "general", Blockers: []api.ReviewFinding{b}}), "", r.Seq, f.reviewer); err != nil {
		t.Fatal(err)
	}
	r2 := f.review(t, candidateB)
	_, err := f.post(f.resultEnv(candidateB, passConvergence, api.ReviewMetadata{Mode: "general"}), "", r2.Seq, f.reviewer)
	if !errors.Is(err, api.ErrConflict) || !strings.HasSuffix(err.Error(), "explicitly resolve "+long[:64]+"\u2026") {
		t.Fatal("dropped blocker refusal", err)
	}
}

// fileReviewFollowUp is the last check before a finding becomes a work item,
// whatever path reached it.
func TestReviewFollowUpTitleRefusalNamesFieldAndFinding(t *testing.T) {
	f := newConvergenceFixture(t)
	file := func(finding api.ReviewFinding) error {
		t.Helper()
		tx, err := f.s.db.BeginTx(f.ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		_, err = f.s.fileReviewFollowUp(f.ctx, tx, f.item, finding, api.Message{TaskID: f.task.ID, Seq: 1}, f.by)
		return err
	}
	rule := "; a title is 1 to 120 characters"
	for name, tc := range map[string]struct{ id, title, want string }{
		"empty":      {"f3", "", "review.findings[f3].title: must not be empty" + rule},
		"too long":   {"f3", strings.Repeat("\u00e9", 121), "review.findings[f3].title: must be at most 120 characters, has 121"},
		"padded":     {"f3", "Padded title ", "review.findings[f3].title: must not start or end with a space" + rule},
		"control":    {"f3", "Tab\there", "review.findings[f3].title: must not contain control characters" + rule},
		"control id": {"f\x1b3\n", "", "review.findings[f 3 ].title: must not be empty" + rule},
		"long id":    {strings.Repeat("i", 65), "", "review.findings[" + strings.Repeat("i", 64) + "\u2026].title: must not be empty" + rule},
	} {
		t.Run(name, func(t *testing.T) {
			reason := findingTitleReason(t, file(api.ReviewFinding{ID: tc.id, Title: tc.title, File: "fixture.go", Line: 3}))
			if reason != tc.want || tc.title != "" && strings.Contains(reason, tc.title) {
				t.Fatalf("reason %q, want %q", reason, tc.want)
			}
		})
	}
	// An invalid kind keeps its bare refusal, even with an invalid title.
	for _, title := range []string{"Valid title", ""} {
		if err := file(api.ReviewFinding{ID: "f3", Kind: "task", Title: title, File: "fixture.go", Line: 3}); err != api.ErrInvalid {
			t.Fatalf("invalid kind with title %q: %v", title, err)
		}
	}
	if err := file(api.ReviewFinding{ID: "f3", Title: strings.Repeat("\u00e9", 120), File: "fixture.go", Line: 3}); err != nil {
		t.Fatal("title at the limit refused", err)
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
	seedPassingVerification(t, f.s, f.item, candidate)
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

func TestReviewConvergenceOwnerAcceptResolvesOwnerDecision(t *testing.T) {
	f := newConvergenceFixture(t)
	partial := map[string]string{"a1": "pass", "a2": "partial"}
	for _, candidate := range []string{candidateA, candidateB} {
		r := f.review(t, candidate)
		if _, err := f.post(f.resultEnv(candidate, partial, api.ReviewMetadata{Mode: "general"}), "", r.Seq, f.reviewer); err != nil {
			t.Fatal(err)
		}
	}
	disposition := func(kind, candidate string, from api.Agent) error {
		_, err := f.post(api.Envelope{Kind: "notice", Subject: "Record the review disposition", Review: &api.ReviewMetadata{Mode: "disposition", Disposition: kind, Candidate: candidate}, Body: api.EnvelopeBody{Text: kind}}, "", 0, from)
		return err
	}
	if err := disposition("owner-accept", candidateB, api.Agent{}); !errors.Is(err, api.ErrConflict) {
		t.Fatal("owner-accept without an owner-decision", err)
	}
	if err := disposition("accept", candidateB, api.Agent{}); !errors.Is(err, api.ErrConflict) {
		t.Fatal("partial criterion allowed ordinary acceptance", err)
	}
	if err := disposition("owner-decision", candidateB, api.Agent{}); err != nil {
		t.Fatal(err)
	}
	if err := disposition("accept", candidateB, api.Agent{}); !errors.Is(err, api.ErrConflict) {
		t.Fatal("second ordinary disposition", err)
	}
	if err := disposition("owner-accept", candidateB, f.reviewer); !errors.Is(err, api.ErrConflict) {
		t.Fatal("agent-authored owner-accept", err)
	}
	tx, err := f.s.db.BeginTx(f.ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = reviewCompletion(f.ctx, tx, f.item, candidateB); !errors.Is(err, api.ErrConflict) {
		t.Fatal("owner-decision alone completed", err)
	}
	tx.Rollback()
	if err = disposition("owner-accept", candidateB, api.Agent{}); err != nil {
		t.Fatal(err)
	}
	if err = disposition("owner-accept", candidateB, api.Agent{}); !errors.Is(err, api.ErrConflict) {
		t.Fatal("repeated owner-accept", err)
	}
	state := f.state(t)
	if len(state.Dispositions) != 2 || state.Dispositions[0].Kind != "owner-decision" || state.Disposition.Kind != "owner-accept" || len(state.Rounds) != 2 {
		t.Fatal("history not preserved", state.Dispositions, len(state.Rounds))
	}
	tx, err = f.s.db.BeginTx(f.ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err = reviewCompletion(f.ctx, tx, f.item, candidateA); !errors.Is(err, api.ErrConflict) {
		t.Fatal("owner-accept completed a different commit", err)
	}
	if err = reviewCompletion(f.ctx, tx, f.item, candidateB); err != nil {
		t.Fatal("owner-accepted exact commit", err)
	}
}

// A follow-ups disposition recorded while independent verification was pending
// left no acceptance path (wi_be41cf76f6148a9c). The owner may resolve it with
// owner-accept, but only once the candidate's verification is ready.
func TestReviewConvergenceOwnerAcceptResolvesFollowUpsAfterVerification(t *testing.T) {
	f := newConvergenceFixture(t)
	partial := map[string]string{"a1": "pass", "a2": "partial"}
	for _, candidate := range []string{candidateA, candidateB} {
		r := f.review(t, candidate)
		if _, err := f.post(f.resultEnv(candidate, partial, api.ReviewMetadata{Mode: "general"}), "", r.Seq, f.reviewer); err != nil {
			t.Fatal(err)
		}
	}
	disposition := func(kind, candidate string, from api.Agent) error {
		_, err := f.post(api.Envelope{Kind: "notice", Subject: "Record the review disposition", Review: &api.ReviewMetadata{Mode: "disposition", Disposition: kind, Candidate: candidate}, Body: api.EnvelopeBody{Text: kind}}, "", 0, from)
		return err
	}
	if err := disposition("follow-ups", candidateB, api.Agent{}); err != nil {
		t.Fatal(err)
	}
	if err := disposition("accept", candidateB, api.Agent{}); !errors.Is(err, api.ErrConflict) {
		t.Fatal("ordinary accept after follow-ups", err)
	}
	if err := disposition("owner-accept", candidateB, f.reviewer); !errors.Is(err, api.ErrConflict) {
		t.Fatal("agent-authored owner-accept", err)
	}
	if _, err := f.s.db.Exec(`INSERT INTO verification_enrollments(task_id,item_id,agent_id,run_id,required,provenance,created_at) VALUES(?,?,?,?,1,'fixture','2026-09-28T00:00:00Z')`, f.item.TaskID, f.item.ID, api.NewID("agt"), api.NewID("run")); err != nil {
		t.Fatal(err)
	}
	if err := disposition("owner-accept", candidateB, api.Agent{}); err == nil {
		t.Fatal("owner-accept resolved follow-ups without verification")
	}
	seedPassingVerification(t, f.s, f.item, candidateB)
	if err := disposition("owner-accept", candidateB, api.Agent{}); err != nil {
		t.Fatal("owner-accept after verification", err)
	}
	state := f.state(t)
	if len(state.Dispositions) != 2 || state.Dispositions[0].Kind != "follow-ups" || state.Disposition.Kind != "owner-accept" {
		t.Fatal("history not preserved", state.Dispositions)
	}
	tx, err := f.s.db.BeginTx(f.ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err = reviewCompletion(f.ctx, tx, f.item, candidateB); err != nil {
		t.Fatal("owner-accepted exact commit", err)
	}
}

// Rebase equivalence: a focused REQUEST/RESULT pair with no blocker IDs lets a
// candidate rebased after two converged general rounds be accepted.
const candidateD = "dddddddddddddddddddddddddddddddddddddddd"

func rebaseFix(reviewed string) string {
	return "Rebase of " + reviewed + " onto current base; tree identical"
}
func rebaseProof(reviewed, rebased string) map[string]api.Evidence {
	return map[string]api.Evidence{"e1": {Type: "command", Value: "git diff --quiet " + reviewed + " " + rebased + " -> exit 0"}}
}
func rebaseRequestEnv(meta api.ReviewMetadata) api.Envelope {
	meta.Mode = "focused"
	return api.Envelope{Kind: "request", Subject: "Verify the rebased candidate is equivalent", Review: &meta, Body: api.EnvelopeBody{Ask: "Verify the rebased candidate matches the reviewed one"}}
}
func rebaseResultEnv(meta api.ReviewMetadata, status map[string]string, evidence map[string]api.Evidence) api.Envelope {
	meta.Mode = "focused"
	return api.Envelope{Kind: "result", Subject: "Rebase equivalence verdict recorded", Review: &meta, Body: api.EnvelopeBody{Outcome: "done", Status: status}, Evidence: evidence}
}
func (f *convergenceFixture) rebaseRequest(candidate, fix string, from api.Agent) (api.Message, error) {
	return f.post(rebaseRequestEnv(api.ReviewMetadata{Candidate: candidate, Fix: fix}), f.reviewer.ID, 0, from)
}
func (f *convergenceFixture) rebaseResult(reply int64, candidate, fix, verdict string, evidence map[string]api.Evidence) error {
	_, err := f.post(rebaseResultEnv(api.ReviewMetadata{Candidate: candidate, Fix: fix}, map[string]string{"equivalence": verdict}, evidence), "", reply, f.reviewer)
	return err
}
func (f *convergenceFixture) acceptCandidate(candidate string, from api.Agent) error {
	_, err := f.post(api.Envelope{Kind: "notice", Subject: "Accept the exact verified candidate", Review: &api.ReviewMetadata{Mode: "disposition", Disposition: "accept", Candidate: candidate}, Body: api.EnvelopeBody{Text: "Accept"}}, "", 0, from)
	return err
}
func (f *convergenceFixture) generalRound(t *testing.T, candidate string, status map[string]string, meta api.ReviewMetadata) {
	t.Helper()
	meta.Mode = "general"
	r := f.review(t, candidate)
	if _, err := f.post(f.resultEnv(candidate, status, meta), "", r.Seq, f.reviewer); err != nil {
		t.Fatal(err)
	}
}

func TestReviewConvergenceCleanRebaseEquivalenceAcceptance(t *testing.T) {
	f := newConvergenceFixture(t)
	f.generalRound(t, candidateA, passConvergence, api.ReviewMetadata{})
	f.generalRound(t, candidateB, passConvergence, api.ReviewMetadata{})
	seedPassingVerification(t, f.s, f.item, candidateC)
	if err := f.acceptCandidate(candidateC, api.Agent{}); !errors.Is(err, api.ErrConflict) {
		t.Fatal("rebased candidate accepted without equivalence verification", err)
	}
	fix := rebaseFix(candidateB)
	requestEnv := rebaseRequestEnv(api.ReviewMetadata{Candidate: candidateC, Fix: fix})
	if problems := api.ValidateEnvelope(requestEnv); len(problems) != 0 {
		t.Fatal("request envelope invalid", problems)
	}
	request, err := f.post(requestEnv, f.reviewer.ID, 0, api.Agent{})
	if err != nil {
		t.Fatal("empty-blocker rebase request refused", err)
	}
	st := f.state(t)
	if len(st.Focused) != 1 || st.Focused[0].Candidate != candidateC || st.Focused[0].Passed || st.Focused[0].ResultSeq != 0 {
		t.Fatal("pending rebase verification", st)
	}
	if raw, _ := json.Marshal(st.Focused[0]); !strings.Contains(string(raw), `"blockerIds":[]`) {
		t.Fatal("stored blocker list is not an empty array", string(raw))
	}
	if err = f.acceptCandidate(candidateC, api.Agent{}); !errors.Is(err, api.ErrConflict) {
		t.Fatal("accepted before the equivalence verdict", err)
	}
	resultEnv := rebaseResultEnv(api.ReviewMetadata{Candidate: candidateC, Fix: fix}, map[string]string{"equivalence": "pass"}, rebaseProof(candidateB, candidateC))
	if problems := api.ValidateEnvelope(resultEnv); len(problems) != 0 {
		t.Fatal("result envelope invalid", problems)
	}
	if _, err = f.post(resultEnv, "", request.Seq, f.reviewer); err != nil {
		t.Fatal("equivalence result refused", err)
	}
	if err = f.acceptCandidate(candidateC, api.Agent{}); err != nil {
		t.Fatal("clean rebase not accepted", err)
	}
	st = f.state(t)
	if len(st.Rounds) != 2 || len(st.Focused) != 1 || !st.Focused[0].Passed || st.Disposition == nil || st.Disposition.Kind != "accept" || st.Disposition.Candidate != candidateC {
		t.Fatal("accepted state", st)
	}
	done := "done"
	if _, err = f.s.UpdateWorkItem(f.ctx, f.task.ID, f.item.ID, api.UpdateWorkItemRequest{Revision: 1, Status: &done}, f.by); err != nil {
		t.Fatal("item completion", err)
	}
	tx, err := f.s.db.BeginTx(f.ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err = reviewCompletion(f.ctx, tx, f.item, candidateB); !errors.Is(err, api.ErrConflict) {
		t.Fatal("team acceptance of the pre-rebase commit", err)
	}
	if err = reviewCompletion(f.ctx, tx, f.item, candidateC); err != nil {
		t.Fatal("team acceptance of the rebased commit", err)
	}
}

func TestReviewConvergenceCleanRebaseNeedsExactReceipt(t *testing.T) {
	f := newConvergenceFixtureWithCriteria(t, map[string]string{"a1": "code", "a2": "matrix"}, []string{"a2"})
	status := map[string]string{"a1": "pass", "a2": "pending-verification"}
	f.generalRound(t, candidateA, status, api.ReviewMetadata{})
	f.generalRound(t, candidateB, status, api.ReviewMetadata{})
	fix := rebaseFix(candidateB)
	request, err := f.rebaseRequest(candidateC, fix, f.lead)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.rebaseResult(request.Seq, candidateC, fix, "pass", rebaseProof(candidateB, candidateC)); err != nil {
		t.Fatal(err)
	}
	if err = f.acceptCandidate(candidateC, f.lead); !errors.Is(err, api.ErrConflict) {
		t.Fatal("accepted without any receipt", err)
	}
	seedPassingVerification(t, f.s, f.item, candidateB)
	if err = f.acceptCandidate(candidateC, f.lead); !errors.Is(err, api.ErrConflict) {
		t.Fatal("pre-rebase receipt covered the rebased commit", err)
	}
	seedPassingVerification(t, f.s, f.item, candidateC)
	if err = f.acceptCandidate(candidateC, f.lead); err != nil {
		t.Fatal("exact receipt and equivalence refused", err)
	}
	if st := f.state(t); len(st.Rounds) != 2 || st.Rounds[1].Verdicts["a2"] != "pending-verification" || st.Disposition == nil || st.Disposition.Candidate != candidateC {
		t.Fatal("accepted state", st)
	}
}

func TestReviewConvergenceRebaseNonEquivalentRefused(t *testing.T) {
	f := newConvergenceFixture(t)
	f.generalRound(t, candidateA, passConvergence, api.ReviewMetadata{})
	f.generalRound(t, candidateB, passConvergence, api.ReviewMetadata{})
	for _, c := range []string{candidateB, candidateC, candidateD} {
		seedPassingVerification(t, f.s, f.item, c)
	}
	fix := rebaseFix(candidateB)
	for name, bad := range map[string][2]string{
		"fix names no commit":         {candidateC, "Rebase onto current base; tree identical"},
		"fix names round one":         {candidateC, rebaseFix(candidateA)},
		"fix names a short commit":    {candidateC, rebaseFix(candidateB[:12])},
		"candidate is the reviewed":   {candidateB, fix},
		"candidate is not a commit":   {"cccccc", fix},
		"fix is empty":                {candidateC, " "},
		"fix names only the rebased":  {candidateC, rebaseFix(candidateC)},
		"candidate reviewed, no base": {candidateB, rebaseFix(candidateA)},
	} {
		if _, err := f.rebaseRequest(bad[0], bad[1], api.Agent{}); !errors.Is(err, api.ErrConflict) {
			t.Fatal(name, err)
		}
	}
	if len(f.state(t).Focused) != 0 {
		t.Fatal("refused request was stored", f.state(t))
	}
	request, err := f.rebaseRequest(candidateC, fix, api.Agent{})
	if err != nil {
		t.Fatal(err)
	}
	meta := api.ReviewMetadata{Candidate: candidateC, Fix: fix}
	proof := rebaseProof(candidateB, candidateC)
	pass := map[string]string{"equivalence": "pass"}
	command := func(value string) map[string]api.Evidence {
		return map[string]api.Evidence{"e1": {Type: "command", Value: value}}
	}
	for name, bad := range map[string]api.Envelope{
		"criterion status":       rebaseResultEnv(meta, map[string]string{"a1": "pass"}, proof),
		"two status keys":        rebaseResultEnv(meta, map[string]string{"equivalence": "pass", "a1": "pass"}, proof),
		"unknown verdict":        rebaseResultEnv(meta, map[string]string{"equivalence": "partial"}, proof),
		"record evidence only":   rebaseResultEnv(meta, pass, map[string]api.Evidence{"e1": {Type: "record", Value: "tree of " + candidateB + " equals tree of " + candidateC}}),
		"only the rebased":       rebaseResultEnv(meta, pass, command("git rev-parse "+candidateC+"^{tree} -> same tree")),
		"only the reviewed":      rebaseResultEnv(meta, pass, command("git rev-parse "+candidateB+"^{tree} -> same tree")),
		"commits split":          rebaseResultEnv(meta, pass, map[string]api.Evidence{"e1": {Type: "command", Value: "git rev-parse " + candidateB + "^{tree}"}, "e2": {Type: "command", Value: "git rev-parse " + candidateC + "^{tree}"}}),
		"short commits":          rebaseResultEnv(meta, pass, command("git diff --quiet "+candidateB[:12]+" "+candidateC[:12]+" -> exit 0")),
		"different candidate":    rebaseResultEnv(api.ReviewMetadata{Candidate: candidateD, Fix: fix}, pass, rebaseProof(candidateB, candidateD)),
		"different fix":          rebaseResultEnv(api.ReviewMetadata{Candidate: candidateC, Fix: fix + " again"}, pass, proof),
		"named blocker":          rebaseResultEnv(api.ReviewMetadata{Candidate: candidateC, Fix: fix, BlockerIDs: []string{"b1"}}, map[string]string{"b1": "pass"}, proof),
		"new blocker in verdict": rebaseResultEnv(api.ReviewMetadata{Candidate: candidateC, Fix: fix, Blockers: []api.ReviewFinding{{ID: "b1", Criterion: "a1", Title: "Changed", File: "fixture.go", Line: 1}}}, pass, proof),
	} {
		if _, err = f.post(bad, "", request.Seq, f.reviewer); !errors.Is(err, api.ErrConflict) {
			t.Fatal(name, err)
		}
	}
	if st := f.state(t); len(st.Focused) != 1 || st.Focused[0].ResultSeq != 0 {
		t.Fatal("refused result was stored", st)
	}
	if err = f.acceptCandidate(candidateC, api.Agent{}); !errors.Is(err, api.ErrConflict) {
		t.Fatal("accepted with no equivalence verdict", err)
	}
	if err = f.rebaseResult(request.Seq, candidateC, fix, "fail", proof); err != nil {
		t.Fatal("failed verdict must be recordable", err)
	}
	if st := f.state(t); st.Focused[0].Passed || st.Focused[0].ResultSeq == 0 {
		t.Fatal("failed verdict recorded as passed", st)
	}
	if err = f.acceptCandidate(candidateC, api.Agent{}); !errors.Is(err, api.ErrConflict) {
		t.Fatal("non-equivalent candidate accepted", err)
	}
	if err = f.rebaseResult(request.Seq, candidateC, fix, "pass", proof); !errors.Is(err, api.ErrConflict) {
		t.Fatal("verdict replaced", err)
	}
	// A later pass on C covers C only, and only while it is the latest verification.
	request, err = f.rebaseRequest(candidateC, fix, api.Agent{})
	if err != nil {
		t.Fatal(err)
	}
	if err = f.rebaseResult(request.Seq, candidateC, fix, "pass", proof); err != nil {
		t.Fatal(err)
	}
	if err = f.acceptCandidate(candidateD, api.Agent{}); !errors.Is(err, api.ErrConflict) {
		t.Fatal("equivalence of C accepted D", err)
	}
	request, err = f.rebaseRequest(candidateD, fix, api.Agent{})
	if err != nil {
		t.Fatal(err)
	}
	if err = f.acceptCandidate(candidateC, api.Agent{}); !errors.Is(err, api.ErrConflict) {
		t.Fatal("superseded candidate accepted while a later one is pending", err)
	}
	if err = f.rebaseResult(request.Seq, candidateD, fix, "fail", rebaseProof(candidateB, candidateD)); err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{candidateC, candidateD} {
		if err = f.acceptCandidate(c, api.Agent{}); !errors.Is(err, api.ErrConflict) {
			t.Fatal("accepted after a later failed equivalence", c, err)
		}
	}
	if err = f.acceptCandidate(candidateB, api.Agent{}); err != nil {
		t.Fatal("reviewed candidate itself remains acceptable", err)
	}
	if st := f.state(t); len(st.Rounds) != 2 || len(st.Focused) != 3 {
		t.Fatal("history", st)
	}
}

func TestReviewConvergenceRebaseCannotBypassBlocker(t *testing.T) {
	named := func(t *testing.T, f *convergenceFixture, candidate string) {
		t.Helper()
		meta := api.ReviewMetadata{Mode: "focused", Candidate: candidate, Fix: "Restore durable retry receipt", BlockerIDs: []string{"b1"}}
		request, err := f.post(api.Envelope{Kind: "request", Subject: "Verify precise retry receipt fix", Review: &meta, Body: api.EnvelopeBody{Ask: "Verify the receipt fix"}}, f.reviewer.ID, 0, api.Agent{})
		if err != nil {
			t.Fatal("named blocker request", err)
		}
		if _, err = f.post(f.resultEnv(candidate, map[string]string{"b1": "pass"}, meta), "", request.Seq, f.reviewer); err != nil {
			t.Fatal("named blocker result", err)
		}
	}
	refused := func(t *testing.T, f *convergenceFixture, candidate, reason string) {
		t.Helper()
		_, err := f.rebaseRequest(candidate, rebaseFix(candidateB), api.Agent{})
		if !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), reason) {
			t.Fatal("empty-blocker request", candidate, err)
		}
		if err = f.acceptCandidate(candidate, api.Agent{}); !errors.Is(err, api.ErrConflict) {
			t.Fatal("accept", candidate, err)
		}
	}
	seed := func(t *testing.T, f *convergenceFixture) {
		t.Helper()
		for _, c := range []string{candidateB, candidateC, candidateD} {
			seedPassingVerification(t, f.s, f.item, c)
		}
	}
	t.Run("retained criterion blocker", func(t *testing.T) {
		f := newConvergenceFixture(t)
		b := api.ReviewFinding{ID: "b1", Criterion: "a1", Title: "Retry failure", File: "fixture.go", Line: 7}
		failed := map[string]string{"a1": "fail", "a2": "pass"}
		f.generalRound(t, candidateA, failed, api.ReviewMetadata{Blockers: []api.ReviewFinding{b}})
		f.generalRound(t, candidateB, failed, api.ReviewMetadata{Blockers: []api.ReviewFinding{b}})
		seed(t, f)
		refused(t, f, candidateC, "rebase verification needs a converged review")
		if len(f.state(t).Focused) != 0 {
			t.Fatal("refused request was stored", f.state(t))
		}
		// The blocker is fix-verified on C; a later rebase to D is not covered.
		named(t, f, candidateC)
		refused(t, f, candidateD, "rebase verification needs a converged review")
		named(t, f, candidateD)
		if err := f.acceptCandidate(candidateD, api.Agent{}); err != nil {
			t.Fatal("named-blocker path on the rebased commit", err)
		}
	})
	t.Run("blocker verified on the reviewed commit only", func(t *testing.T) {
		f := newConvergenceFixture(t)
		b := api.ReviewFinding{ID: "b1", Regression: true, Baseline: candidateC, Candidate: candidateA, Title: "Regression", File: "fixture.go", Line: 1}
		f.generalRound(t, candidateA, passConvergence, api.ReviewMetadata{Blockers: []api.ReviewFinding{b}})
		b.Candidate = candidateB
		f.generalRound(t, candidateB, passConvergence, api.ReviewMetadata{Blockers: []api.ReviewFinding{b}})
		seed(t, f)
		refused(t, f, candidateD, "rebase verification needs a converged review")
		named(t, f, candidateB)
		// The reviewed commit now converges, but b1 is still open on D.
		refused(t, f, candidateD, "unresolved blockers on rebased candidate")
		named(t, f, candidateD)
		if err := f.acceptCandidate(candidateD, api.Agent{}); err != nil {
			t.Fatal("named-blocker path on the rebased commit", err)
		}
	})
	t.Run("partial criterion without blocker", func(t *testing.T) {
		f := newConvergenceFixture(t)
		partial := map[string]string{"a1": "pass", "a2": "partial"}
		f.generalRound(t, candidateA, partial, api.ReviewMetadata{})
		f.generalRound(t, candidateB, partial, api.ReviewMetadata{})
		seed(t, f)
		refused(t, f, candidateC, "rebase verification needs a converged review")
		if st := f.state(t); len(st.Focused) != 0 || len(st.Rounds) != 2 {
			t.Fatal("refused request changed state", st)
		}
	})
}

// A round-one blocker resolved by a clean round two leaves nothing to name, so
// a tree-identical rebase of round two is accepted through the empty-blocker path.
func TestReviewConvergenceCleanRoundTwoRebaseAcceptance(t *testing.T) {
	f := newConvergenceFixture(t)
	lead, err := f.s.AddAgent(f.ctx, f.task.ID, api.AddAgentRequest{Name: "lead", Host: "fixture", Session: "lead"}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	name := "lead"
	if _, err = f.s.UpdateTask(f.ctx, f.task.ID, api.UpdateTaskRequest{Orchestrator: &name}, f.by); err != nil {
		t.Fatal(err)
	}
	b := api.ReviewFinding{ID: "b1", Criterion: "a1", Title: "Retry failure", File: "fixture.go", Line: 7}
	f.generalRound(t, candidateA, map[string]string{"a1": "fail", "a2": "pass"}, api.ReviewMetadata{Blockers: []api.ReviewFinding{b}})
	f.generalRound(t, candidateB, passConvergence, api.ReviewMetadata{BlockerIDs: []string{"b1"}})
	seedPassingVerification(t, f.s, f.item, candidateC)
	if err = f.acceptCandidate(candidateC, lead); !errors.Is(err, api.ErrConflict) {
		t.Fatal("rebased candidate accepted without equivalence verification", err)
	}
	fix := rebaseFix(candidateB)
	request, err := f.rebaseRequest(candidateC, fix, lead)
	if err != nil {
		t.Fatal("empty-blocker rebase request refused", err)
	}
	if err = f.rebaseResult(request.Seq, candidateC, fix, "pass", rebaseProof(candidateB, candidateC)); err != nil {
		t.Fatal("equivalence result refused", err)
	}
	if err = f.acceptCandidate(candidateC, lead); err != nil {
		t.Fatal("clean round-two rebase not accepted", err)
	}
	// Round two's resolution of b1 is its own record on B; the rebase names none.
	st := f.state(t)
	if len(st.Rounds) != 2 || len(st.Rounds[0].Blockers) != 1 || len(st.Rounds[1].Blockers) != 0 || len(st.Focused) != 2 || st.Focused[0].Candidate != candidateB || !reflect.DeepEqual(st.Focused[0].BlockerIDs, []string{"b1"}) {
		t.Fatal("round-two resolution", st)
	}
	if rebase := st.Focused[1]; rebase.Candidate != candidateC || !rebase.Passed || len(rebase.BlockerIDs) != 0 || st.Disposition == nil || st.Disposition.Kind != "accept" || st.Disposition.Candidate != candidateC {
		t.Fatal("accepted state", st)
	}
}

// A refusal on the empty-blocker path names why the reviewed candidate is not
// converged, here a scope amended after round two.
func TestReviewConvergenceRebaseRefusalNamesCause(t *testing.T) {
	f := newConvergenceFixture(t)
	f.generalRound(t, candidateA, passConvergence, api.ReviewMetadata{})
	f.generalRound(t, candidateB, passConvergence, api.ReviewMetadata{})
	seedPassingVerification(t, f.s, f.item, candidateC)
	f.rescope(t, "Fixture, amended")
	if err := f.assign(map[string]string{"a1": "works", "a2": "retries", "a3": "reports"}, nil); err != nil {
		t.Fatal(err)
	}
	_, err := f.rebaseRequest(candidateC, rebaseFix(candidateB), api.Agent{})
	refusedWith(t, "rebase after a scope amendment", err, "rebase verification needs a converged review; name unresolved blocker IDs instead, and round-two partials in criterionIds (cause: latest general review is incomplete or stale)")
	if strings.Count(err.Error(), "review convergence:") != 1 {
		t.Fatal("doubled prefix", err)
	}
	if len(f.state(t).Focused) != 0 {
		t.Fatal("refused request was stored", f.state(t))
	}
}

func TestReviewConvergenceRebaseLifecycle(t *testing.T) {
	f := newConvergenceFixture(t)
	lead, err := f.s.AddAgent(f.ctx, f.task.ID, api.AddAgentRequest{Name: "lead", Host: "fixture", Session: "lead"}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	name := "lead"
	if _, err = f.s.UpdateTask(f.ctx, f.task.ID, api.UpdateTaskRequest{Orchestrator: &name}, f.by); err != nil {
		t.Fatal(err)
	}
	fix := rebaseFix(candidateB)
	request := func(candidate string, ids []string, to string, from api.Agent) (api.Message, error) {
		return f.post(rebaseRequestEnv(api.ReviewMetadata{Candidate: candidate, Fix: fix, BlockerIDs: ids}), to, 0, from)
	}
	result := func(reply int64, candidate string, ids []string, from api.Agent) error {
		_, err := f.post(rebaseResultEnv(api.ReviewMetadata{Candidate: candidate, Fix: fix, BlockerIDs: ids}, map[string]string{"equivalence": "pass"}, rebaseProof(candidateB, candidate)), "", reply, from)
		return err
	}
	f.generalRound(t, candidateA, passConvergence, api.ReviewMetadata{})
	if _, err = request(candidateC, nil, f.reviewer.ID, lead); !errors.Is(err, api.ErrConflict) {
		t.Fatal("rebase verification after one round", err)
	}
	r2 := f.review(t, candidateB)
	if _, err = request(candidateC, nil, f.reviewer.ID, lead); !errors.Is(err, api.ErrConflict) {
		t.Fatal("rebase verification during round two", err)
	}
	if _, err = f.post(f.resultEnv(candidateB, passConvergence, api.ReviewMetadata{Mode: "general"}), "", r2.Seq, f.reviewer); err != nil {
		t.Fatal(err)
	}
	seedPassingVerification(t, f.s, f.item, candidateD)
	stale := lead
	stale.RunID = "run_bbbbbbbbbbbbbbbb"
	for name, attempt := range map[string]struct {
		to   string
		from api.Agent
	}{
		"non-lead author":       {f.reviewer.ID, f.reviewer},
		"stale lead run":        {f.reviewer.ID, stale},
		"verifier not reviewer": {lead.ID, lead},
	} {
		if _, err = request(candidateC, nil, attempt.to, attempt.from); !errors.Is(err, api.ErrConflict) {
			t.Fatal(name, err)
		}
	}
	// Explicit empty list on the request, omitted field on the result.
	first, err := request(candidateC, []string{}, f.reviewer.ID, lead)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = request(candidateD, nil, f.reviewer.ID, lead); !errors.Is(err, api.ErrConflict) {
		t.Fatal("second focused request while one is pending", err)
	}
	staleReviewer := f.reviewer
	staleReviewer.RunID = "run_dddddddddddddddd"
	if err = result(first.Seq, candidateC, nil, staleReviewer); !errors.Is(err, api.ErrConflict) {
		t.Fatal("stale verifier run", err)
	}
	if err = result(first.Seq, candidateC, nil, lead); !errors.Is(err, api.ErrConflict) {
		t.Fatal("lead verified its own request", err)
	}
	if err = result(first.Seq, candidateC, nil, f.reviewer); err != nil {
		t.Fatal("omitted list did not match the empty list", err)
	}
	if err = result(first.Seq, candidateC, nil, f.reviewer); !errors.Is(err, api.ErrConflict) {
		t.Fatal("second verdict on one request", err)
	}
	if _, err = f.post(api.Envelope{Kind: "review", Subject: "Review frozen fixture candidate", Body: api.EnvelopeBody{Candidate: candidateC, Scope: "Fixture", Acceptance: f.criteria}}, f.reviewer.ID, 0, lead); !errors.Is(err, api.ErrConflict) {
		t.Fatal("third general round", err)
	}
	// Omitted field on the request, explicit empty list on the result.
	second, err := request(candidateD, nil, f.reviewer.ID, lead)
	if err != nil {
		t.Fatal(err)
	}
	if err = result(second.Seq, candidateD, []string{}, f.reviewer); err != nil {
		t.Fatal("empty list did not match the omitted list", err)
	}
	if err = f.acceptCandidate(candidateD, f.reviewer); !errors.Is(err, api.ErrConflict) {
		t.Fatal("non-lead accepted", err)
	}
	if err = f.acceptCandidate(candidateD, lead); err != nil {
		t.Fatal(err)
	}
	st := f.state(t)
	if len(st.Rounds) != 2 || len(st.Focused) != 2 || !st.Focused[0].Passed || !st.Focused[1].Passed || st.Disposition == nil || st.Disposition.Candidate != candidateD {
		t.Fatal("lifecycle state", st)
	}
	for _, focused := range st.Focused {
		if focused.BlockerIDs == nil || len(focused.BlockerIDs) != 0 || focused.ReviewerID != f.reviewer.ID || focused.ReviewerRun != f.reviewer.RunID {
			t.Fatal("stored rebase verification", focused)
		}
	}
}

// Settlement and lead-named fix: focused records that let the lead accept
// without the owner after two completed general rounds.
func focusedMeta(t *testing.T, raw string) api.ReviewMetadata {
	t.Helper()
	var meta api.ReviewMetadata
	if err := json.Unmarshal([]byte(raw), &meta); err != nil {
		t.Fatal(err)
	}
	return meta
}
func focusedRequestEnv(meta api.ReviewMetadata) api.Envelope {
	meta.Mode = "focused"
	return api.Envelope{Kind: "request", Subject: "Verify the named focused check", Review: &meta, Body: api.EnvelopeBody{Ask: "Verify exactly the named check"}}
}
func focusedResultEnv(meta api.ReviewMetadata, status map[string]string, evidence map[string]api.Evidence) api.Envelope {
	meta.Mode = "focused"
	return api.Envelope{Kind: "result", Subject: "Focused check verdict recorded", Review: &meta, Body: api.EnvelopeBody{Outcome: "done", Status: status}, Evidence: evidence}
}
func receiptProof(candidate string) map[string]api.Evidence {
	return map[string]api.Evidence{"e1": {Type: "record", Value: "passing verification receipt for " + candidate}}
}
func deltaFix(reviewed string) string {
	return "Test-only fix after " + reviewed + ": accept TEST_BROWSER=both in the new browser test"
}
func deltaProof(reviewed, changed string) map[string]api.Evidence {
	return map[string]api.Evidence{"e1": {Type: "command", Value: "git diff --stat " + reviewed + " " + changed + " -> 1 file changed, the named test"}}
}
func (f *convergenceFixture) thirdReviewRefused(t *testing.T, candidate string) {
	t.Helper()
	_, err := f.post(api.Envelope{Kind: "review", Subject: "Review frozen fixture candidate", Body: api.EnvelopeBody{Candidate: candidate, Scope: "Fixture", Acceptance: f.criteria, VerificationCriteria: f.verificationCriteria}}, f.reviewer.ID, 0, f.lead)
	if !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "third general review refused") {
		t.Fatal("third general review", err)
	}
	if st := f.state(t); len(st.Rounds) != 2 {
		t.Fatal("round count changed", st)
	}
}

var partialConvergence = map[string]string{"a1": "pass", "a2": "partial"}

// Case A (wi_0de79d818662efe9 c5): round two left a2 partial only because the
// matrix had not run; the passing receipt for the exact candidate settles it.
func TestReviewConvergencePartialSettledByReceipt(t *testing.T) {
	f := newConvergenceFixture(t)
	f.generalRound(t, candidateA, partialConvergence, api.ReviewMetadata{})
	f.generalRound(t, candidateB, partialConvergence, api.ReviewMetadata{})
	seedPassingVerification(t, f.s, f.item, candidateB)
	if err := f.acceptCandidate(candidateB, api.Agent{}); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "criterion a2 has not passed") {
		t.Fatal("accept with a round-two partial", err)
	}
	f.thirdReviewRefused(t, candidateB)
	// New route: the lead names the partial criterion; the original reviewer settles it.
	meta := focusedMeta(t, `{"candidate":"`+candidateB+`","fix":"Full matrix receipt covers a2","criterionIds":["a2"]}`)
	request, err := f.post(focusedRequestEnv(meta), f.reviewer.ID, 0, api.Agent{})
	if err != nil {
		t.Fatal("settlement request refused", err)
	}
	if err = f.acceptCandidate(candidateB, api.Agent{}); !errors.Is(err, api.ErrConflict) {
		t.Fatal("accepted before the settling verdict", err)
	}
	if _, err = f.post(focusedResultEnv(meta, map[string]string{"a2": "pass"}, receiptProof(candidateB)), "", request.Seq, f.reviewer); err != nil {
		t.Fatal("settling result refused", err)
	}
	f.thirdReviewRefused(t, candidateB)
	if err = f.acceptCandidate(candidateB, api.Agent{}); err != nil {
		t.Fatal("settled partial not accepted", err)
	}
	st := f.state(t)
	if len(st.Rounds) != 2 || st.Rounds[1].Verdicts["a2"] != "partial" || len(st.Focused) != 1 || !st.Focused[0].Passed || st.Disposition == nil || st.Disposition.Kind != "accept" || st.Disposition.Candidate != candidateB {
		t.Fatal("accepted state", st)
	}
	raw, _ := json.Marshal(st.Focused[0])
	var record struct {
		CriterionIDs      []string `json:"criterionIds"`
		BlockerIDs        []string `json:"blockerIds"`
		ReceiptGeneration int64    `json:"receiptGeneration"`
	}
	if err = json.Unmarshal(raw, &record); err != nil {
		t.Fatal(err)
	}
	if len(record.CriterionIDs) != 1 || record.CriterionIDs[0] != "a2" || record.ReceiptGeneration <= 0 || !strings.Contains(string(raw), `"blockerIds":[]`) {
		t.Fatal("settling record", string(raw))
	}
}

// Case B (wi_0de79d818662efe9 c4): verification found a test-only fix after a
// clean final round, so the candidate changed and no blocker ID exists.
func TestReviewConvergenceVerificationFoundFix(t *testing.T) {
	f := newConvergenceFixture(t)
	f.generalRound(t, candidateA, passConvergence, api.ReviewMetadata{})
	f.generalRound(t, candidateB, passConvergence, api.ReviewMetadata{})
	seedPassingVerification(t, f.s, f.item, candidateC)
	if err := f.acceptCandidate(candidateC, api.Agent{}); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "changed candidate needs exact focused verification") {
		t.Fatal("accept of the changed candidate", err)
	}
	named := focusedMeta(t, `{"candidate":"`+candidateC+`","fix":"`+deltaFix(candidateB)+`","blockerIds":["b1"]}`)
	if _, err := f.post(focusedRequestEnv(named), f.reviewer.ID, 0, api.Agent{}); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "must name distinct unresolved blocker IDs") {
		t.Fatal("focused request naming a blocker that does not exist", err)
	}
	f.thirdReviewRefused(t, candidateC)
	// New route: the lead names the fix; the original reviewer confirms the exact delta.
	meta := focusedMeta(t, `{"candidate":"`+candidateC+`","fix":"`+deltaFix(candidateB)+`","treeDiffers":true}`)
	request, err := f.post(focusedRequestEnv(meta), f.reviewer.ID, 0, api.Agent{})
	if err != nil {
		t.Fatal("lead-named fix request refused", err)
	}
	st := f.state(t)
	if raw, _ := json.Marshal(st.Focused); len(st.Focused) != 1 || !strings.Contains(string(raw), `"treeDiffers":true`) || !strings.Contains(string(raw), `"blockerIds":[]`) {
		t.Fatal("stored record does not say the tree differs", string(raw))
	}
	if err = f.acceptCandidate(candidateC, api.Agent{}); !errors.Is(err, api.ErrConflict) {
		t.Fatal("accepted before the delta verdict", err)
	}
	if _, err = f.post(focusedResultEnv(meta, map[string]string{"delta": "pass"}, deltaProof(candidateB, candidateC)), "", request.Seq, f.reviewer); err != nil {
		t.Fatal("delta result refused", err)
	}
	f.thirdReviewRefused(t, candidateC)
	if err = f.acceptCandidate(candidateC, api.Agent{}); err != nil {
		t.Fatal("lead-named fix not accepted", err)
	}
	st = f.state(t)
	if len(st.Rounds) != 2 || len(st.Focused) != 1 || !st.Focused[0].Passed || st.Disposition == nil || st.Disposition.Kind != "accept" || st.Disposition.Candidate != candidateC {
		t.Fatal("accepted state", st)
	}
	tx, err := f.s.db.BeginTx(f.ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err = reviewCompletion(f.ctx, tx, f.item, candidateB); !errors.Is(err, api.ErrConflict) {
		t.Fatal("team acceptance of the pre-fix commit", err)
	}
	if err = reviewCompletion(f.ctx, tx, f.item, candidateC); err != nil {
		t.Fatal("team acceptance of the fixed commit", err)
	}
}

func (f *convergenceFixture) focusedRequest(meta api.ReviewMetadata) (api.Message, error) {
	return f.post(focusedRequestEnv(meta), f.reviewer.ID, 0, f.lead)
}
func (f *convergenceFixture) focusedResult(reply int64, meta api.ReviewMetadata, status map[string]string, evidence map[string]api.Evidence) error {
	_, err := f.post(focusedResultEnv(meta, status, evidence), "", reply, f.reviewer)
	return err
}

// focusedCheck stores the request and its verdict, and fails on any refusal.
func (f *convergenceFixture) focusedCheck(t *testing.T, meta api.ReviewMetadata, status map[string]string, evidence map[string]api.Evidence) {
	t.Helper()
	request, err := f.focusedRequest(meta)
	if err != nil {
		t.Fatal("focused request", err)
	}
	if err = f.focusedResult(request.Seq, meta, status, evidence); err != nil {
		t.Fatal("focused result", err)
	}
}

// seedPlanWithoutReceipt replaces the current plan with a newer one that has
// no receipt yet, as a rerun of the matrix does.
func seedPlanWithoutReceipt(t *testing.T, f *convergenceFixture) {
	t.Helper()
	records, err := verificationRecords(f.ctx, f.s.db, f.task.ID, f.item.ID)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := currentVerification(records)
	if p == nil {
		t.Fatal("no plan to replace")
	}
	b, _ := json.Marshal(api.VerificationRecord{Generation: int64(len(records)) + 1, Kind: "plan", Plan: p})
	if _, err = f.s.db.Exec(`INSERT INTO verification_records VALUES(?,?,?,?,?,?,?)`, f.task.ID, f.item.ID, len(records)+1, "plan", api.NewID("req"), "synthetic-fixture", string(b)); err != nil {
		t.Fatal(err)
	}
}
func receiptRefused(t *testing.T, name string, err error) {
	t.Helper()
	if !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "passing receipt") {
		t.Fatal(name, err)
	}
}

func TestReviewConvergenceSettlementRefusals(t *testing.T) {
	t.Run("request", func(t *testing.T) {
		criteria := map[string]string{"a1": "code", "a2": "full package", "a3": "retries", "a4": "matrix"}
		f := newConvergenceFixtureWithCriteria(t, criteria, []string{"a4"})
		status := map[string]string{"a1": "pass", "a2": "partial", "a3": "fail", "a4": "pending-verification"}
		b := api.ReviewFinding{ID: "b1", Criterion: "a3", Title: "Retry failure", File: "fixture.go", Line: 7}
		f.generalRound(t, candidateA, status, api.ReviewMetadata{Blockers: []api.ReviewFinding{b}})
		f.generalRound(t, candidateB, status, api.ReviewMetadata{Blockers: []api.ReviewFinding{b}})
		settle := func(candidate string, ids ...string) error {
			_, err := f.focusedRequest(api.ReviewMetadata{Candidate: candidate, Fix: "Receipt settles the partial", CriterionIDs: ids})
			return err
		}
		receiptRefused(t, "no receipt", settle(candidateB, "a2"))
		seedPassingVerification(t, f.s, f.item, candidateC)
		receiptRefused(t, "receipt for another commit", settle(candidateB, "a2"))
		seedPassingVerification(t, f.s, f.item, candidateB)
		for name, ids := range map[string][]string{
			"unknown criterion":    {"a9"},
			"verification-owned":   {"a4"},
			"passed in round two":  {"a1"},
			"failed in round two":  {"a3"},
			"duplicated":           {"a2", "a2"},
			"one valid, one wrong": {"a2", "a1"},
		} {
			if err := settle(candidateB, ids...); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "criterionIds must name distinct round-two partial criteria") {
				t.Fatal(name, err)
			}
		}
		if _, err := f.post(focusedRequestEnv(api.ReviewMetadata{Candidate: candidateB, Fix: "Receipt settles the partial", CriterionIDs: []string{"a2"}}), f.reviewer.ID, 0, f.reviewer); !errors.Is(err, api.ErrConflict) {
			t.Fatal("non-lead settlement request", err)
		}
		if st := f.state(t); len(st.Focused) != 0 || len(st.Rounds) != 2 {
			t.Fatal("refused request was stored", st)
		}
		f.thirdReviewRefused(t, candidateB)
	})
	t.Run("criterion tied to an outstanding blocker", func(t *testing.T) {
		// Messages cannot produce a partial criterion with an open blocker: a
		// blocker's criterion must carry a failed verdict. The ledger is edited
		// directly to prove the request-time guard on its own.
		f := newConvergenceFixture(t)
		f.generalRound(t, candidateA, partialConvergence, api.ReviewMetadata{})
		f.generalRound(t, candidateB, partialConvergence, api.ReviewMetadata{})
		seedPassingVerification(t, f.s, f.item, candidateB)
		st := f.state(t)
		st.Rounds[1].Blockers = []api.ReviewFinding{{ID: "b1", Criterion: "a2", Title: "Retry failure", File: "fixture.go", Line: 7}}
		tx, err := f.s.db.BeginTx(f.ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err = saveReviewState(f.ctx, tx, f.task.ID, st); err != nil {
			t.Fatal(err)
		}
		if err = tx.Commit(); err != nil {
			t.Fatal(err)
		}
		_, err = f.focusedRequest(api.ReviewMetadata{Candidate: candidateB, Fix: "Receipt settles the partial", CriterionIDs: []string{"a2"}})
		if !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "criterion a2 has an unresolved blocker") {
			t.Fatal("settled a criterion with an open blocker", err)
		}
		if len(f.state(t).Focused) != 0 {
			t.Fatal("refused request was stored", f.state(t))
		}
	})
	t.Run("result", func(t *testing.T) {
		f := newConvergenceFixture(t)
		f.generalRound(t, candidateA, partialConvergence, api.ReviewMetadata{})
		f.generalRound(t, candidateB, partialConvergence, api.ReviewMetadata{})
		seedPassingVerification(t, f.s, f.item, candidateB)
		other, err := f.s.AddAgent(f.ctx, f.task.ID, api.AddAgentRequest{Name: "other", Host: "fixture", Session: "other"}, f.by)
		if err != nil {
			t.Fatal(err)
		}
		meta := api.ReviewMetadata{Candidate: candidateB, Fix: "Receipt settles the partial", CriterionIDs: []string{"a2"}}
		request, err := f.focusedRequest(meta)
		if err != nil {
			t.Fatal(err)
		}
		pass := map[string]string{"a2": "pass"}
		stale := f.reviewer
		stale.RunID = "run_dddddddddddddddd"
		for name, from := range map[string]api.Agent{"another agent": other, "stale reviewer run": stale} {
			if _, err = f.post(focusedResultEnv(meta, pass, receiptProof(candidateB)), "", request.Seq, from); !errors.Is(err, api.ErrConflict) {
				t.Fatal(name, err)
			}
		}
		for name, bad := range map[string]api.Envelope{
			"unknown verdict":      focusedResultEnv(meta, map[string]string{"a2": "partial"}, receiptProof(candidateB)),
			"another criterion":    focusedResultEnv(meta, map[string]string{"a1": "pass"}, receiptProof(candidateB)),
			"extra status key":     focusedResultEnv(meta, map[string]string{"a2": "pass", "a1": "pass"}, receiptProof(candidateB)),
			"equivalence key":      focusedResultEnv(meta, map[string]string{"a2": "pass", "equivalence": "pass"}, rebaseProof(candidateA, candidateB)),
			"criterion list drops": focusedResultEnv(api.ReviewMetadata{Candidate: candidateB, Fix: meta.Fix}, pass, receiptProof(candidateB)),
			"new finding":          focusedResultEnv(api.ReviewMetadata{Candidate: candidateB, Fix: meta.Fix, CriterionIDs: []string{"a2"}, Findings: []api.ReviewFinding{{ID: "f1", Title: "Later", File: "fixture.go", Line: 1}}}, pass, receiptProof(candidateB)),
		} {
			if _, err = f.post(bad, "", request.Seq, f.reviewer); !errors.Is(err, api.ErrConflict) {
				t.Fatal(name, err)
			}
		}
		if st := f.state(t); len(st.Focused) != 1 || st.Focused[0].ResultSeq != 0 {
			t.Fatal("refused result was stored", st)
		}
		if err = f.focusedResult(request.Seq, meta, map[string]string{"a2": "fail"}, receiptProof(candidateB)); err != nil {
			t.Fatal("failed settlement must be recordable", err)
		}
		if st := f.state(t); st.Focused[0].Passed || st.Focused[0].ReceiptGeneration != 0 {
			t.Fatal("failed settlement recorded as settled", st)
		}
		if err = f.acceptCandidate(candidateB, api.Agent{}); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "criterion a2 has not passed") {
			t.Fatal("accepted after a failed settlement", err)
		}
		// A settlement on B covers B only, and the latest verdict on B decides.
		f.focusedCheck(t, meta, pass, receiptProof(candidateB))
		if err = f.acceptCandidate(candidateC, api.Agent{}); !errors.Is(err, api.ErrConflict) {
			t.Fatal("settlement of B accepted C", err)
		}
		f.focusedCheck(t, meta, map[string]string{"a2": "fail"}, receiptProof(candidateB))
		if err = f.acceptCandidate(candidateB, api.Agent{}); !errors.Is(err, api.ErrConflict) {
			t.Fatal("an earlier pass outlived a later fail", err)
		}
		f.focusedCheck(t, meta, pass, receiptProof(candidateB))
		f.thirdReviewRefused(t, candidateB)
		if err = f.acceptCandidate(candidateB, api.Agent{}); err != nil {
			t.Fatal(err)
		}
		if st := f.state(t); len(st.Rounds) != 2 || len(st.Focused) != 4 || st.Rounds[1].Verdicts["a2"] != "partial" {
			t.Fatal("history", st)
		}
	})
}

// The exact receipt gates both new routes on an item with no enrollment and
// no verification-owned criterion, where an ordinary accept needs none.
func TestReviewConvergenceNewRoutesAlwaysNeedExactReceipt(t *testing.T) {
	unenrolled := func(t *testing.T, f *convergenceFixture) {
		t.Helper()
		tx, err := f.s.db.BeginTx(f.ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if required, err := verificationRequired(f.ctx, tx, f.task.ID, f.item.ID); err != nil || required {
			t.Fatal("expected an unenrolled fixture", required, err)
		}
	}
	completion := func(f *convergenceFixture, candidate string) error {
		tx, err := f.s.db.BeginTx(f.ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		return reviewCompletion(f.ctx, tx, f.item, candidate)
	}
	t.Run("lead-named fix", func(t *testing.T) {
		f := newConvergenceFixture(t)
		unenrolled(t, f)
		f.generalRound(t, candidateA, passConvergence, api.ReviewMetadata{})
		f.generalRound(t, candidateB, passConvergence, api.ReviewMetadata{})
		// The request needs no receipt yet: the fix may precede the matrix rerun.
		f.focusedCheck(t, api.ReviewMetadata{Candidate: candidateC, Fix: deltaFix(candidateB), TreeDiffers: true}, map[string]string{"delta": "pass"}, deltaProof(candidateB, candidateC))
		receiptRefused(t, "no receipt", f.acceptCandidate(candidateC, api.Agent{}))
		seedPassingVerification(t, f.s, f.item, candidateB)
		receiptRefused(t, "receipt for the pre-fix commit", f.acceptCandidate(candidateC, api.Agent{}))
		seedPassingVerification(t, f.s, f.item, candidateC)
		seedPlanWithoutReceipt(t, f)
		receiptRefused(t, "newer plan without a receipt", f.acceptCandidate(candidateC, api.Agent{}))
		if f.state(t).Disposition != nil {
			t.Fatal("refused acceptance wrote a disposition")
		}
		seedPassingVerification(t, f.s, f.item, candidateC)
		if err := f.acceptCandidate(candidateC, api.Agent{}); err != nil {
			t.Fatal("exact receipt and delta refused", err)
		}
		if err := completion(f, candidateC); err != nil {
			t.Fatal("completion with the exact receipt", err)
		}
		// Completion re-checks the receipt after the accept was saved.
		seedPlanWithoutReceipt(t, f)
		receiptRefused(t, "completion after the receipt was replaced", completion(f, candidateC))
	})
	t.Run("settled partial", func(t *testing.T) {
		f := newConvergenceFixture(t)
		unenrolled(t, f)
		f.generalRound(t, candidateA, partialConvergence, api.ReviewMetadata{})
		f.generalRound(t, candidateB, partialConvergence, api.ReviewMetadata{})
		meta := api.ReviewMetadata{Candidate: candidateB, Fix: "Receipt settles the partial", CriterionIDs: []string{"a2"}}
		_, err := f.focusedRequest(meta)
		receiptRefused(t, "request with no receipt", err)
		seedPassingVerification(t, f.s, f.item, candidateB)
		request, err := f.focusedRequest(meta)
		if err != nil {
			t.Fatal(err)
		}
		seedPlanWithoutReceipt(t, f)
		pass := map[string]string{"a2": "pass"}
		receiptRefused(t, "pass after the receipt was replaced", f.focusedResult(request.Seq, meta, pass, receiptProof(candidateB)))
		if st := f.state(t); st.Focused[0].ResultSeq != 0 {
			t.Fatal("refused result was stored", st)
		}
		seedPassingVerification(t, f.s, f.item, candidateB)
		if err = f.focusedResult(request.Seq, meta, pass, receiptProof(candidateB)); err != nil {
			t.Fatal(err)
		}
		seedPlanWithoutReceipt(t, f)
		receiptRefused(t, "accept with a newer plan and no receipt", f.acceptCandidate(candidateB, api.Agent{}))
		seedPassingVerification(t, f.s, f.item, candidateA)
		receiptRefused(t, "accept with a receipt for another commit", f.acceptCandidate(candidateB, api.Agent{}))
		seedPassingVerification(t, f.s, f.item, candidateB)
		if err = f.acceptCandidate(candidateB, api.Agent{}); err != nil {
			t.Fatal("exact receipt and settlement refused", err)
		}
		if err = completion(f, candidateB); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("ordinary accept is unchanged", func(t *testing.T) {
		f := newConvergenceFixture(t)
		f.generalRound(t, candidateA, passConvergence, api.ReviewMetadata{})
		f.generalRound(t, candidateB, passConvergence, api.ReviewMetadata{})
		if err := f.acceptCandidate(candidateB, api.Agent{}); err != nil {
			t.Fatal("an unenrolled clean review needed a receipt", err)
		}
	})
}

func TestReviewConvergenceNewRoutesCannotBypassBlocker(t *testing.T) {
	named := func(t *testing.T, f *convergenceFixture, candidate, id string) {
		t.Helper()
		f.focusedCheck(t, api.ReviewMetadata{Candidate: candidate, Fix: "Restore durable retry receipt", BlockerIDs: []string{id}}, map[string]string{id: "pass"}, receiptProof(candidate))
	}
	t.Run("failed criterion with a blocker", func(t *testing.T) {
		f := newConvergenceFixture(t)
		b := api.ReviewFinding{ID: "b1", Criterion: "a1", Title: "Retry failure", File: "fixture.go", Line: 7}
		status := map[string]string{"a1": "fail", "a2": "partial"}
		f.generalRound(t, candidateA, status, api.ReviewMetadata{Blockers: []api.ReviewFinding{b}})
		f.generalRound(t, candidateB, status, api.ReviewMetadata{Blockers: []api.ReviewFinding{b}})
		seedPassingVerification(t, f.s, f.item, candidateB)
		for name, tc := range map[string]struct {
			meta   api.ReviewMetadata
			reason string
		}{
			"lead-named fix over the blocker":       {api.ReviewMetadata{Candidate: candidateC, Fix: deltaFix(candidateB), TreeDiffers: true}, "treeDiffers verification needs a converged review"},
			"lead-named fix settling the partial":   {api.ReviewMetadata{Candidate: candidateC, Fix: deltaFix(candidateB), TreeDiffers: true, CriterionIDs: []string{"a2"}}, "treeDiffers verification needs a converged review"},
			"settle the blocker's criterion":        {api.ReviewMetadata{Candidate: candidateB, Fix: "Receipt settles the criterion", CriterionIDs: []string{"a1"}}, "criterionIds must name distinct round-two partial criteria"},
			"lead-named fix with blocker IDs":       {api.ReviewMetadata{Candidate: candidateC, Fix: deltaFix(candidateB), TreeDiffers: true, BlockerIDs: []string{"b1"}}, "treeDiffers verification names no blocker IDs"},
			"lead-named fix on the reviewed commit": {api.ReviewMetadata{Candidate: candidateB, Fix: deltaFix(candidateB), TreeDiffers: true, BlockerIDs: []string{"b1"}}, "treeDiffers verification names no blocker IDs"},
		} {
			if _, err := f.focusedRequest(tc.meta); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), tc.reason) {
				t.Fatal(name, err)
			}
		}
		if len(f.state(t).Focused) != 0 {
			t.Fatal("refused request was stored", f.state(t))
		}
		// Settling the partial does not clear the blocker.
		f.focusedCheck(t, api.ReviewMetadata{Candidate: candidateB, Fix: "Receipt settles the partial", CriterionIDs: []string{"a2"}}, map[string]string{"a2": "pass"}, receiptProof(candidateB))
		if err := f.acceptCandidate(candidateB, api.Agent{}); !errors.Is(err, api.ErrConflict) {
			t.Fatal("accepted with an unresolved blocker", err)
		}
		f.thirdReviewRefused(t, candidateB)
		named(t, f, candidateB, "b1")
		if err := f.acceptCandidate(candidateB, api.Agent{}); err != nil {
			t.Fatal("blocker verified by ID and partial settled", err)
		}
	})
	t.Run("blocker and partial in one request", func(t *testing.T) {
		f := newConvergenceFixture(t)
		b := api.ReviewFinding{ID: "b1", Criterion: "a1", Title: "Retry failure", File: "fixture.go", Line: 7}
		status := map[string]string{"a1": "fail", "a2": "partial"}
		f.generalRound(t, candidateA, status, api.ReviewMetadata{Blockers: []api.ReviewFinding{b}})
		f.generalRound(t, candidateB, status, api.ReviewMetadata{Blockers: []api.ReviewFinding{b}})
		seedPassingVerification(t, f.s, f.item, candidateC)
		meta := api.ReviewMetadata{Candidate: candidateC, Fix: "Restore durable retry receipt", BlockerIDs: []string{"b1"}, CriterionIDs: []string{"a2"}}
		request, err := f.focusedRequest(meta)
		if err != nil {
			t.Fatal(err)
		}
		if err = f.focusedResult(request.Seq, meta, map[string]string{"b1": "pass"}, receiptProof(candidateC)); !errors.Is(err, api.ErrConflict) {
			t.Fatal("result skipped the criterion", err)
		}
		if err = f.focusedResult(request.Seq, meta, map[string]string{"b1": "fail", "a2": "pass"}, receiptProof(candidateC)); err != nil {
			t.Fatal(err)
		}
		if err = f.acceptCandidate(candidateC, api.Agent{}); !errors.Is(err, api.ErrConflict) {
			t.Fatal("accepted with a failed blocker verdict", err)
		}
		f.focusedCheck(t, meta, map[string]string{"b1": "pass", "a2": "pass"}, receiptProof(candidateC))
		if err = f.acceptCandidate(candidateC, api.Agent{}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("criterion ID equal to a blocker ID", func(t *testing.T) {
		f := newConvergenceFixture(t)
		b := api.ReviewFinding{ID: "a2", Criterion: "a1", Title: "Retry failure", File: "fixture.go", Line: 7}
		status := map[string]string{"a1": "fail", "a2": "partial"}
		f.generalRound(t, candidateA, status, api.ReviewMetadata{Blockers: []api.ReviewFinding{b}})
		f.generalRound(t, candidateB, status, api.ReviewMetadata{Blockers: []api.ReviewFinding{b}})
		seedPassingVerification(t, f.s, f.item, candidateB)
		_, err := f.focusedRequest(api.ReviewMetadata{Candidate: candidateB, Fix: "Restore durable retry receipt", BlockerIDs: []string{"a2"}, CriterionIDs: []string{"a2"}})
		if !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "is also a named blocker ID") {
			t.Fatal("ambiguous status key", err)
		}
	})
	t.Run("blocker verified on the reviewed commit only", func(t *testing.T) {
		f := newConvergenceFixture(t)
		b := api.ReviewFinding{ID: "b1", Regression: true, Baseline: candidateC, Candidate: candidateA, Title: "Regression", File: "fixture.go", Line: 1}
		f.generalRound(t, candidateA, passConvergence, api.ReviewMetadata{Blockers: []api.ReviewFinding{b}})
		b.Candidate = candidateB
		f.generalRound(t, candidateB, passConvergence, api.ReviewMetadata{Blockers: []api.ReviewFinding{b}})
		seedPassingVerification(t, f.s, f.item, candidateD)
		named(t, f, candidateB, "b1")
		_, err := f.focusedRequest(api.ReviewMetadata{Candidate: candidateD, Fix: deltaFix(candidateB), TreeDiffers: true})
		if !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "unresolved blockers on rebased candidate") {
			t.Fatal("lead-named fix over a blocker open on the fixed commit", err)
		}
		if err = f.acceptCandidate(candidateD, api.Agent{}); !errors.Is(err, api.ErrConflict) {
			t.Fatal("accept", err)
		}
		named(t, f, candidateD, "b1")
		if err = f.acceptCandidate(candidateD, api.Agent{}); err != nil {
			t.Fatal("named-blocker path on the fixed commit", err)
		}
	})
}

func TestReviewConvergenceEquivalenceAndDeltaStayDistinct(t *testing.T) {
	f := newConvergenceFixture(t)
	f.generalRound(t, candidateA, passConvergence, api.ReviewMetadata{})
	f.generalRound(t, candidateB, passConvergence, api.ReviewMetadata{})
	for name, meta := range map[string]api.ReviewMetadata{
		"candidate is the reviewed one": {Candidate: candidateB, Fix: deltaFix(candidateB), TreeDiffers: true},
		"fix lacks the reviewed commit": {Candidate: candidateC, Fix: "Test-only fix", TreeDiffers: true},
		"fix names a short commit":      {Candidate: candidateC, Fix: deltaFix(candidateB[:12]), TreeDiffers: true},
		"fix names round one":           {Candidate: candidateC, Fix: deltaFix(candidateA), TreeDiffers: true},
		"candidate is not a commit":     {Candidate: "cccccc", Fix: deltaFix(candidateB), TreeDiffers: true},
		"settles a passed criterion":    {Candidate: candidateC, Fix: deltaFix(candidateB), TreeDiffers: true, CriterionIDs: []string{"a2"}},
	} {
		if _, err := f.focusedRequest(meta); !errors.Is(err, api.ErrConflict) {
			t.Fatal(name, err)
		}
	}
	if len(f.state(t).Focused) != 0 {
		t.Fatal("refused request was stored", f.state(t))
	}
	pass := func(key string) map[string]string { return map[string]string{key: "pass"} }
	refused := func(reply int64, cases map[string]api.Envelope) {
		t.Helper()
		for name, bad := range cases {
			if _, err := f.post(bad, "", reply, f.reviewer); !errors.Is(err, api.ErrConflict) {
				t.Fatal(name, err)
			}
		}
	}
	// An equivalence record never takes a delta verdict or the new fields.
	equivalence := api.ReviewMetadata{Candidate: candidateC, Fix: rebaseFix(candidateB)}
	request, err := f.focusedRequest(equivalence)
	if err != nil {
		t.Fatal(err)
	}
	if raw, _ := json.Marshal(f.state(t).Focused[0]); strings.Contains(string(raw), "treeDiffers") || strings.Contains(string(raw), "criterionIds") || strings.Contains(string(raw), "receiptGeneration") {
		t.Fatal("equivalence record gained new fields", string(raw))
	}
	refused(request.Seq, map[string]api.Envelope{
		"delta status":            focusedResultEnv(equivalence, pass("delta"), rebaseProof(candidateB, candidateC)),
		"delta beside the proof":  focusedResultEnv(equivalence, map[string]string{"equivalence": "pass", "delta": "pass"}, rebaseProof(candidateB, candidateC)),
		"result says treeDiffers": focusedResultEnv(api.ReviewMetadata{Candidate: candidateC, Fix: equivalence.Fix, TreeDiffers: true}, pass("delta"), rebaseProof(candidateB, candidateC)),
		"treeDiffers equivalence": focusedResultEnv(api.ReviewMetadata{Candidate: candidateC, Fix: equivalence.Fix, TreeDiffers: true}, pass("equivalence"), rebaseProof(candidateB, candidateC)),
		"result adds a criterion": focusedResultEnv(api.ReviewMetadata{Candidate: candidateC, Fix: equivalence.Fix, CriterionIDs: []string{"a2"}}, map[string]string{"equivalence": "pass", "a2": "pass"}, rebaseProof(candidateB, candidateC)),
	})
	if err = f.focusedResult(request.Seq, equivalence, pass("equivalence"), rebaseProof(candidateB, candidateC)); err != nil {
		t.Fatal(err)
	}
	// A treeDiffers record never takes an equivalence verdict.
	delta := api.ReviewMetadata{Candidate: candidateD, Fix: deltaFix(candidateB), TreeDiffers: true}
	request, err = f.focusedRequest(delta)
	if err != nil {
		t.Fatal(err)
	}
	command := func(value string) map[string]api.Evidence {
		return map[string]api.Evidence{"e1": {Type: "command", Value: value}}
	}
	refused(request.Seq, map[string]api.Envelope{
		"equivalence status":      focusedResultEnv(delta, pass("equivalence"), deltaProof(candidateB, candidateD)),
		"both keys":               focusedResultEnv(delta, map[string]string{"equivalence": "pass", "delta": "pass"}, deltaProof(candidateB, candidateD)),
		"criterion status":        focusedResultEnv(delta, pass("a1"), deltaProof(candidateB, candidateD)),
		"unknown verdict":         focusedResultEnv(delta, map[string]string{"delta": "partial"}, deltaProof(candidateB, candidateD)),
		"result drops the flag":   focusedResultEnv(api.ReviewMetadata{Candidate: candidateD, Fix: delta.Fix}, pass("delta"), deltaProof(candidateB, candidateD)),
		"flag dropped, old key":   focusedResultEnv(api.ReviewMetadata{Candidate: candidateD, Fix: delta.Fix}, pass("equivalence"), deltaProof(candidateB, candidateD)),
		"different candidate":     focusedResultEnv(api.ReviewMetadata{Candidate: candidateC, Fix: delta.Fix, TreeDiffers: true}, pass("delta"), deltaProof(candidateB, candidateC)),
		"record evidence only":    focusedResultEnv(delta, pass("delta"), map[string]api.Evidence{"e1": {Type: "record", Value: "diff of " + candidateB + " and " + candidateD}}),
		"only the fixed commit":   focusedResultEnv(delta, pass("delta"), command("git show --stat "+candidateD)),
		"only the reviewed":       focusedResultEnv(delta, pass("delta"), command("git show --stat "+candidateB)),
		"short commits":           focusedResultEnv(delta, pass("delta"), command("git diff --stat "+candidateB[:12]+" "+candidateD[:12])),
		"new blocker in verdict":  focusedResultEnv(api.ReviewMetadata{Candidate: candidateD, Fix: delta.Fix, TreeDiffers: true, Blockers: []api.ReviewFinding{{ID: "b1", Criterion: "a1", Title: "Changed", File: "fixture.go", Line: 1}}}, pass("delta"), deltaProof(candidateB, candidateD)),
		"result names a blocker":  focusedResultEnv(api.ReviewMetadata{Candidate: candidateD, Fix: delta.Fix, TreeDiffers: true, BlockerIDs: []string{"b1"}}, pass("b1"), deltaProof(candidateB, candidateD)),
		"result adds a criterion": focusedResultEnv(api.ReviewMetadata{Candidate: candidateD, Fix: delta.Fix, TreeDiffers: true, CriterionIDs: []string{"a2"}}, map[string]string{"delta": "pass", "a2": "pass"}, deltaProof(candidateB, candidateD)),
	})
	if st := f.state(t); len(st.Focused) != 2 || st.Focused[1].ResultSeq != 0 || !st.Focused[1].TreeDiffers {
		t.Fatal("refused result was stored", st)
	}
	// A failed delta is recordable and never accepted; the verdict is not replaced.
	if err = f.focusedResult(request.Seq, delta, map[string]string{"delta": "fail"}, deltaProof(candidateB, candidateD)); err != nil {
		t.Fatal(err)
	}
	if err = f.focusedResult(request.Seq, delta, pass("delta"), deltaProof(candidateB, candidateD)); !errors.Is(err, api.ErrConflict) {
		t.Fatal("verdict replaced", err)
	}
	seedPassingVerification(t, f.s, f.item, candidateD)
	for _, c := range []string{candidateC, candidateD} {
		if err = f.acceptCandidate(c, api.Agent{}); !errors.Is(err, api.ErrConflict) {
			t.Fatal("accepted after a later failed delta", c, err)
		}
	}
	f.thirdReviewRefused(t, candidateD)
	if st := f.state(t); len(st.Rounds) != 2 || len(st.Focused) != 2 || st.Focused[1].Passed {
		t.Fatal("history", st)
	}
}

// One request carries the changed candidate and the round-two partial: a held
// criterion part added after review, or a rebase with a partial to settle.
func TestReviewConvergenceChangedCandidateSettlesPartialInOneRequest(t *testing.T) {
	for name, tc := range map[string]struct {
		meta  api.ReviewMetadata
		key   string
		proof map[string]api.Evidence
	}{
		"lead-named fix": {api.ReviewMetadata{Candidate: candidateC, Fix: deltaFix(candidateB), TreeDiffers: true, CriterionIDs: []string{"a2"}}, "delta", deltaProof(candidateB, candidateC)},
		"equivalence":    {api.ReviewMetadata{Candidate: candidateC, Fix: rebaseFix(candidateB), CriterionIDs: []string{"a2"}}, "equivalence", rebaseProof(candidateB, candidateC)},
	} {
		t.Run(name, func(t *testing.T) {
			f := newConvergenceFixture(t)
			f.generalRound(t, candidateA, partialConvergence, api.ReviewMetadata{})
			f.generalRound(t, candidateB, partialConvergence, api.ReviewMetadata{})
			bare := tc.meta
			bare.CriterionIDs = nil
			if _, err := f.focusedRequest(bare); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "needs a converged review") {
				t.Fatal("changed candidate over an unsettled partial", err)
			}
			_, err := f.focusedRequest(tc.meta)
			receiptRefused(t, "request with no receipt on the changed commit", err)
			seedPassingVerification(t, f.s, f.item, candidateC)
			request, err := f.focusedRequest(tc.meta)
			if err != nil {
				t.Fatal(err)
			}
			st := f.state(t)
			if len(st.Focused) != 1 || st.Focused[0].TreeDiffers != tc.meta.TreeDiffers || len(st.Focused[0].CriterionIDs) != 1 || st.Focused[0].CriterionIDs[0] != "a2" || len(st.Focused[0].BlockerIDs) != 0 || st.Focused[0].BlockerIDs == nil {
				t.Fatal("stored request", st)
			}
			for label, status := range map[string]map[string]string{
				"proof only":     {tc.key: "pass"},
				"criterion only": {"a2": "pass"},
				"extra key":      {tc.key: "pass", "a2": "pass", "a1": "pass"},
			} {
				if err = f.focusedResult(request.Seq, tc.meta, status, tc.proof); !errors.Is(err, api.ErrConflict) {
					t.Fatal(label, err)
				}
			}
			if err = f.focusedResult(request.Seq, tc.meta, map[string]string{tc.key: "fail", "a2": "pass"}, tc.proof); err != nil {
				t.Fatal(err)
			}
			if err = f.acceptCandidate(candidateC, api.Agent{}); !errors.Is(err, api.ErrConflict) {
				t.Fatal("accepted with a failed proof", err)
			}
			f.focusedCheck(t, tc.meta, map[string]string{tc.key: "pass", "a2": "pass"}, tc.proof)
			f.thirdReviewRefused(t, candidateC)
			if err = f.acceptCandidate(candidateB, api.Agent{}); !errors.Is(err, api.ErrConflict) {
				t.Fatal("settlement on the changed commit accepted the reviewed one", err)
			}
			if err = f.acceptCandidate(candidateC, api.Agent{}); err != nil {
				t.Fatal(err)
			}
			st = f.state(t)
			if len(st.Rounds) != 2 || st.Rounds[1].Verdicts["a2"] != "partial" || len(st.Focused) != 2 || st.Focused[1].ReceiptGeneration <= 0 || st.Disposition.Candidate != candidateC {
				t.Fatal("accepted state", st)
			}
		})
	}
}

// wi_06611d359b2081ee v1: round two cites every reviewer-owned fail, or the
// RESULT is refused while the reviewer can still cite it. Round one may still
// leave a fail uncited; round two then cites it or passes it.
func TestReviewConvergenceRoundTwoUncitedFailRefused(t *testing.T) {
	regression := func(criterion string) api.ReviewFinding {
		return api.ReviewFinding{ID: "b1", Criterion: criterion, Regression: true, Baseline: candidateA, Candidate: candidateB, Title: "Retry drops the saved state", File: "fixture.go", Line: 7}
	}
	cited := api.ReviewFinding{ID: "b1", Criterion: "a2", Title: "Retry drops the saved state", File: "fixture.go", Line: 7}
	finding := func(id, criterion string) api.ReviewFinding {
		return api.ReviewFinding{ID: id, Criterion: criterion, Title: "Retry state is not reported", File: "fixture.go", Line: 9}
	}
	uncited := func(key string) string {
		return "conflict: review convergence: criterion " + key + " failed with no blocker or finding citing it; set criterion on the blocker for that failure (a regression may cite the criterion it breaks)"
	}
	failed := map[string]string{"a1": "pass", "a2": "fail"}
	for _, tc := range []struct {
		name           string
		round1         map[string]string
		round1Blockers []api.ReviewFinding
		round2         map[string]string
		meta           api.ReviewMetadata
		want           string // "" when the round is saved
	}{
		{name: "no blocker", round1: failed, round2: failed, want: uncited("a2")},
		{name: "regression with no criterion", round1: partialConvergence, round2: failed, meta: api.ReviewMetadata{Blockers: []api.ReviewFinding{regression("")}}, want: uncited("a2")},
		{name: "regression citing the criterion", round1: partialConvergence, round2: failed, meta: api.ReviewMetadata{Blockers: []api.ReviewFinding{regression("a2")}}},
		{name: "round-one blocker retained", round1: failed, round1Blockers: []api.ReviewFinding{cited}, round2: failed, meta: api.ReviewMetadata{Blockers: []api.ReviewFinding{cited}}},
		{name: "finding", round1: partialConvergence, round2: failed, meta: api.ReviewMetadata{Findings: []api.ReviewFinding{finding("f1", "a2")}}},
		{name: "new non-regression blocker filed as a finding", round1: partialConvergence, round2: failed, meta: api.ReviewMetadata{Blockers: []api.ReviewFinding{cited}}},
		{name: "two uncited fails name the lower", round1: partialConvergence, round2: map[string]string{"a1": "fail", "a2": "fail"}, want: uncited("a1")},
		{name: "a cited fail beside an uncited one", round1: partialConvergence, round2: map[string]string{"a1": "fail", "a2": "fail"}, meta: api.ReviewMetadata{Findings: []api.ReviewFinding{finding("f1", "a1")}}, want: uncited("a2")},
		{name: "round-one uncited fail passed in round two", round1: failed, round2: passConvergence},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newConvergenceFixture(t)
			f.generalRound(t, candidateA, tc.round1, api.ReviewMetadata{Blockers: tc.round1Blockers})
			r := f.review(t, candidateB)
			before, _ := f.s.ListWorkItems(f.ctx, f.task.ID, "", "", 0, 100)
			meta := tc.meta
			meta.Mode = "general"
			_, err := f.post(f.resultEnv(candidateB, tc.round2, meta), "", r.Seq, f.reviewer)
			if tc.want == "" {
				if err != nil {
					t.Fatal("round two refused", err)
				}
				return
			}
			if err == nil || err.Error() != tc.want {
				t.Fatalf("round two:\n got %v\nwant %s", err, tc.want)
			}
			after, _ := f.s.ListWorkItems(f.ctx, f.task.ID, "", "", 0, 100)
			st := f.state(t)
			if len(before.Items) != len(after.Items) || len(st.Rounds) != 2 || st.Rounds[1].ResultSeq != 0 || len(st.FollowUps) != 0 {
				t.Fatal("refused round two left writes behind", st)
			}
		})
	}
}

// wi_06611d359b2081ee v2: a regression that cites the criterion it breaks is
// passed by its focused verification, so a plain accept needs no owner.
func TestReviewConvergenceRegressionCitingCriterionAcceptsAfterFocusedPass(t *testing.T) {
	f := newConvergenceFixture(t)
	f.generalRound(t, candidateA, partialConvergence, api.ReviewMetadata{})
	b1 := api.ReviewFinding{ID: "b1", Criterion: "a2", Regression: true, Baseline: candidateA, Candidate: candidateB, Title: "Retry drops the saved state", File: "fixture.go", Line: 7}
	f.generalRound(t, candidateB, map[string]string{"a1": "pass", "a2": "fail"}, api.ReviewMetadata{Blockers: []api.ReviewFinding{b1}})
	if err := f.acceptCandidate(candidateB, api.Agent{}); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "criterion a2 has not passed") {
		t.Fatal("accept with the regression open", err)
	}
	f.focusedCheck(t, api.ReviewMetadata{Candidate: candidateC, Fix: "Keep the saved state on retry", BlockerIDs: []string{"b1"}}, map[string]string{"b1": "pass"}, receiptProof(candidateC))
	if err := f.acceptCandidate(candidateC, api.Agent{}); err != nil {
		t.Fatal("plain accept after the regression passed", err)
	}
	st := f.state(t)
	if st.Rounds[1].Verdicts["a2"] != "fail" || len(st.Focused) != 1 || !st.Focused[0].Passed || st.Disposition == nil || st.Disposition.Kind != "accept" || st.Disposition.Candidate != candidateC {
		t.Fatal("accepted state", st)
	}
}

// wi_06611d359b2081ee v3: after a blocker-only focused pass on C, a
// criterionIds-only request on C settles a round-two partial with the exact
// receipt. It is not a rebase of B: its RESULT reports only the criterion IDs.
// A candidate with no blocker pass there, or with a blocker still open there,
// keeps the rebase route and its refusal.
func TestReviewConvergenceCriterionIDsSettleAfterBlockerOnlyFocusedPass(t *testing.T) {
	setup := func(t *testing.T, blockers ...string) *convergenceFixture {
		f := newConvergenceFixture(t)
		f.generalRound(t, candidateA, partialConvergence, api.ReviewMetadata{})
		var round2 []api.ReviewFinding
		for i, id := range blockers {
			round2 = append(round2, api.ReviewFinding{ID: id, Regression: true, Baseline: candidateA, Candidate: candidateB, Title: "Retry drops the saved state", File: "fixture.go", Line: 7 + i})
		}
		f.generalRound(t, candidateB, partialConvergence, api.ReviewMetadata{Blockers: round2})
		f.focusedCheck(t, api.ReviewMetadata{Candidate: candidateC, Fix: "Keep the saved state on retry", BlockerIDs: []string{"b1"}}, map[string]string{"b1": "pass"}, receiptProof(candidateC))
		return f
	}
	settle := api.ReviewMetadata{Candidate: candidateC, Fix: "Full matrix receipt covers a2 on the fixed candidate", CriterionIDs: []string{"a2"}}
	rebaseRefusal := "conflict: review convergence: rebase verification needs a converged review; name unresolved blocker IDs instead, and round-two partials in criterionIds (cause: unresolved blockers on accepted candidate)"

	t.Run("settles on the blocker-verified candidate", func(t *testing.T) {
		f := setup(t, "b1")
		if err := f.acceptCandidate(candidateC, api.Agent{}); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "criterion a2 has not passed; a partial") {
			t.Fatal("accept before settling a2", err)
		}
		_, err := f.focusedRequest(settle)
		receiptRefused(t, "settle request with no receipt for C", err)
		seedPassingVerification(t, f.s, f.item, candidateC)
		request, err := f.focusedRequest(settle)
		if err != nil {
			t.Fatal("settle request refused", err)
		}
		for label, status := range map[string]map[string]string{
			"equivalence key": {"equivalence": "pass", "a2": "pass"},
			"delta key":       {"delta": "pass", "a2": "pass"},
			"blocker key":     {"b1": "pass", "a2": "pass"},
		} {
			if err = f.focusedResult(request.Seq, settle, status, receiptProof(candidateC)); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "Status reports pass or fail for exactly a2") {
				t.Fatal(label, err)
			}
		}
		if err = f.focusedResult(request.Seq, settle, map[string]string{"a2": "pass"}, receiptProof(candidateC)); err != nil {
			t.Fatal("settle result refused", err)
		}
		f.thirdReviewRefused(t, candidateC)
		if err = f.acceptCandidate(candidateB, api.Agent{}); !errors.Is(err, api.ErrConflict) {
			t.Fatal("settlement on C accepted the reviewed candidate", err)
		}
		if err = f.acceptCandidate(candidateC, api.Agent{}); err != nil {
			t.Fatal("plain accept of C", err)
		}
		st := f.state(t)
		if st.Rounds[1].Verdicts["a2"] != "partial" || len(st.Focused) != 2 || !st.Focused[1].Passed || st.Focused[1].ReceiptGeneration <= 0 || st.Focused[1].TreeDiffers || len(st.Focused[1].BlockerIDs) != 0 || st.Disposition == nil || st.Disposition.Kind != "accept" || st.Disposition.Candidate != candidateC {
			t.Fatal("accepted state", st)
		}
	})
	t.Run("failed settlement keeps the partial", func(t *testing.T) {
		f := setup(t, "b1")
		seedPassingVerification(t, f.s, f.item, candidateC)
		f.focusedCheck(t, settle, map[string]string{"a2": "fail"}, receiptProof(candidateC))
		if err := f.acceptCandidate(candidateC, api.Agent{}); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "criterion a2 has not passed; a partial") {
			t.Fatal("accept after a failed settlement", err)
		}
	})
	t.Run("candidate with no blocker pass keeps the rebase route", func(t *testing.T) {
		f := setup(t, "b1")
		seedPassingVerification(t, f.s, f.item, candidateD)
		other := settle
		other.Candidate, other.Fix = candidateD, "Full matrix receipt covers a2 after "+candidateB
		if _, err := f.focusedRequest(other); err == nil || err.Error() != rebaseRefusal {
			t.Fatalf("settle request on D:\n got %v\nwant %s", err, rebaseRefusal)
		}
	})
	t.Run("blocker still open on the candidate keeps the rebase route", func(t *testing.T) {
		f := setup(t, "b1", "b2")
		seedPassingVerification(t, f.s, f.item, candidateC)
		withFix := settle
		withFix.Fix = "Full matrix receipt covers a2 after " + candidateB
		if _, err := f.focusedRequest(withFix); err == nil || err.Error() != rebaseRefusal {
			t.Fatalf("settle request on C with b2 open:\n got %v\nwant %s", err, rebaseRefusal)
		}
		withFix.TreeDiffers = true
		if _, err := f.focusedRequest(withFix); err == nil || !strings.Contains(err.Error(), "treeDiffers verification needs a converged review") {
			t.Fatal("treeDiffers request on C with b2 open", err)
		}
	})
}

// The documented focused examples are valid on the wire as tt send reads them.
func TestReviewConvergenceDocumentedFocusedExamples(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", "message-broker.md"))
	if err != nil {
		t.Fatal(err)
	}
	_, section, found := strings.Cut(string(raw), "\n## Review convergence\n")
	if !found {
		t.Fatal("Review convergence section missing")
	}
	var settles, differs, equivalence int
	for i, block := range strings.Split(section, "```json\n") {
		if i == 0 {
			continue
		}
		example, _, _ := strings.Cut(block, "```")
		var meta api.ReviewMetadata
		dec := json.NewDecoder(strings.NewReader(example))
		dec.DisallowUnknownFields()
		if err = dec.Decode(&meta); err != nil {
			t.Fatal("example does not decode", err, example)
		}
		if meta.Mode != "focused" {
			continue
		}
		if !validGitCommit(meta.Candidate) || strings.TrimSpace(meta.Fix) == "" {
			t.Fatal("focused example lacks candidate or fix", example)
		}
		if problems := api.ValidateEnvelope(focusedRequestEnv(meta)); len(problems) != 0 {
			t.Fatal("request envelope invalid", problems, example)
		}
		status := map[string]string{}
		switch {
		case meta.TreeDiffers:
			differs++
			status["delta"] = "pass"
		case len(meta.CriterionIDs) > 0:
			settles++
		default:
			equivalence++
			status["equivalence"] = "pass"
		}
		for _, id := range meta.CriterionIDs {
			status[id] = "pass"
		}
		if meta.Candidate != candidateB && !strings.Contains(meta.Fix, candidateB) {
			t.Fatal("changed-candidate example does not name the reviewed commit", example)
		}
		if problems := api.ValidateEnvelope(focusedResultEnv(meta, status, deltaProof(candidateB, meta.Candidate))); len(problems) != 0 {
			t.Fatal("result envelope invalid", problems, example)
		}
	}
	if settles != 1 || differs != 1 || equivalence != 1 {
		t.Fatal("expected one settlement, one treeDiffers and one equivalence example", settles, differs, equivalence)
	}
}

// stageHandler adds the database handler that files scope confirmations.
func (f *convergenceFixture) stageHandler(t *testing.T) api.Agent {
	t.Helper()
	handler, err := f.s.AddAgent(f.ctx, f.task.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: "db-handler", Host: "fixture", Session: "db-handler", Role: api.AgentRoleDatabaseHandler}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

// rescope advances the item's scope revision with a revision-checked edit.
func (f *convergenceFixture) rescope(t *testing.T, description string) {
	t.Helper()
	var err error
	if f.item, err = f.s.UpdateWorkItem(f.ctx, f.task.ID, f.item.ID, api.UpdateWorkItemRequest{Revision: f.item.Revision, Description: &description}, f.by); err != nil {
		t.Fatal(err)
	}
}

// ownerOrder posts an owner order linked to the current item revision.
func (f *convergenceFixture) ownerOrder(t *testing.T, key string) api.Message {
	t.Helper()
	return unconfirmedOrder(t, f.s, f.task, f.item, key)
}

// confirmScope saves the handler's confirmation of the current scope under order.
func (f *convergenceFixture) confirmScope(t *testing.T, handler api.Agent, order api.Message, key string) {
	t.Helper()
	req := api.ConfirmWorkOrderScopeRequest{RequestID: key, AgentID: handler.ID, RunID: handler.RunID, ExpectedRevision: f.item.Revision, ScopeRevision: f.item.ScopeRevision, OrderMessageSeq: order.Seq, Complete: true}
	if _, err := f.s.ConfirmWorkOrderScope(f.ctx, f.task.ID, f.item.ID, req); err != nil {
		t.Fatal(err)
	}
}

func (f *convergenceFixture) assign(criteria map[string]string, verificationCriteria []string) error {
	_, err := f.post(api.Envelope{Kind: "assign", Subject: "Implement the staged fixture criteria", Body: api.EnvelopeBody{Objective: "Fixture", Owns: []string{"fixture"}, Acceptance: criteria, VerificationCriteria: verificationCriteria}}, f.builder.ID, 0, api.Agent{})
	return err
}

func (f *convergenceFixture) reviewErr(candidate string) error {
	_, err := f.post(api.Envelope{Kind: "review", Subject: "Review frozen fixture candidate", Body: api.EnvelopeBody{Candidate: candidate, Scope: "Fixture", Acceptance: f.criteria, VerificationCriteria: f.verificationCriteria}}, f.reviewer.ID, 0, api.Agent{})
	return err
}

func refusedWith(t *testing.T, name string, err error, reason string) {
	t.Helper()
	if !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), reason) {
		t.Fatalf("%s: want refusal %q, got %v", name, reason, err)
	}
}

func stageJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// findingsStage runs a first stage confirmed under its own owner order: two
// general reviews that leave a2 failed with blocker b1, a failed focused
// verification, a follow-up and a follow-ups disposition.
func findingsStage(t *testing.T) (*convergenceFixture, api.Agent) {
	t.Helper()
	f := newConvergenceFixture(t)
	handler := f.stageHandler(t)
	f.confirmScope(t, handler, f.ownerOrder(t, "findings-order"), "findings-scope")
	blocker := api.ReviewFinding{ID: "b1", Criterion: "a2", Title: "Findings stage retry gap", Command: "fixture retry", Output: "FAIL lost state"}
	failed := map[string]string{"a1": "pass", "a2": "fail"}
	f.generalRound(t, candidateA, failed, api.ReviewMetadata{Blockers: []api.ReviewFinding{blocker}, Findings: []api.ReviewFinding{{ID: "f1", Title: "Explain the findings stage", Kind: "feature", File: "fixture.go", Line: 3}}})
	f.generalRound(t, candidateA, failed, api.ReviewMetadata{Blockers: []api.ReviewFinding{blocker}})
	f.focusedCheck(t, api.ReviewMetadata{Mode: "focused", Candidate: candidateC, Fix: "Retry fix attempt", BlockerIDs: []string{"b1"}}, map[string]string{"b1": "fail"}, receiptProof(candidateC))
	if _, err := f.post(api.Envelope{Kind: "notice", Subject: "File the findings stage follow-ups", Review: &api.ReviewMetadata{Mode: "disposition", Disposition: "follow-ups", Candidate: candidateA}, Body: api.EnvelopeBody{Text: "Findings only"}}, "", 0, api.Agent{}); err != nil {
		t.Fatal(err)
	}
	refusedWith(t, "third review in the findings stage", f.reviewErr(candidateA), "third general review refused")
	return f, handler
}

// a1, a3 (wi_5225af9140ef7a19): after a findings stage used both general
// reviews, a scope confirmed under a new owner order takes an ASSIGN that
// reuses a1..aN with verification designation, then its own two reviews.
func TestReviewStageNewOwnerOrderOpensFreshReviews(t *testing.T) {
	f, handler := findingsStage(t)
	before := f.state(t)
	f.rescope(t, "Build order one")
	f.confirmScope(t, handler, f.ownerOrder(t, "build-order"), "build-scope")
	f.criteria, f.verificationCriteria = map[string]string{"a1": "builds", "a2": "matrix passes"}, []string{"a2"}
	if err := f.assign(f.criteria, f.verificationCriteria); err != nil {
		t.Fatal("build ASSIGN reusing a1..aN with verification designation", err)
	}
	// The new stage has its own blocker and finding IDs.
	blocker := api.ReviewFinding{ID: "b1", Criterion: "a1", Title: "Build stage defect", Command: "fixture build", Output: "FAIL build"}
	f.generalRound(t, candidateB, map[string]string{"a1": "fail", "a2": "pending-verification"}, api.ReviewMetadata{Blockers: []api.ReviewFinding{blocker}, Findings: []api.ReviewFinding{{ID: "f1", Title: "Explain the build stage", Kind: "feature", File: "build.go", Line: 9}}})
	f.generalRound(t, candidateC, map[string]string{"a1": "pass", "a2": "pending-verification"}, api.ReviewMetadata{BlockerIDs: []string{"b1"}, Fix: "fixed the build"})
	refusedWith(t, "third review in the build stage", f.reviewErr(candidateC), "third general review refused")

	after := f.state(t)
	if len(after.Rounds) != 4 || len(after.Scopes) != 2 {
		t.Fatalf("rounds %d scopes %d", len(after.Rounds), len(after.Scopes))
	}
	for i, want := range []struct {
		number int
		scope  int64
	}{{1, 1}, {2, 1}, {1, 2}, {2, 2}} {
		if r := after.Rounds[i]; r.Number != want.number || r.ScopeRevision != want.scope || r.ResultSeq == 0 {
			t.Fatalf("round %d: number %d scope %d result %d", i, r.Number, r.ScopeRevision, r.ResultSeq)
		}
	}
	// The findings stage is stored unchanged, with its scope revision.
	if got, want := stageJSON(t, after.Rounds[:2]), stageJSON(t, before.Rounds); got != want {
		t.Fatalf("earlier rounds changed\n got %s\nwant %s", got, want)
	}
	if got, want := stageJSON(t, after.Scopes[:1]), stageJSON(t, before.Scopes); got != want {
		t.Fatalf("earlier scope changed\n got %s\nwant %s", got, want)
	}
	if got, want := stageJSON(t, after.Focused[:len(before.Focused)]), stageJSON(t, before.Focused); got != want || len(before.Focused) != 1 || len(after.Focused) != 2 {
		t.Fatalf("earlier focused records changed\n got %s\nwant %s", got, want)
	}
	if got, want := stageJSON(t, after.FollowUps[:len(before.FollowUps)]), stageJSON(t, before.FollowUps); got != want || len(before.FollowUps) != 1 || len(after.FollowUps) != 2 {
		t.Fatalf("earlier follow-ups changed\n got %s\nwant %s", got, want)
	}
	if got, want := stageJSON(t, after.Dispositions), stageJSON(t, before.Dispositions); got != want || len(before.Dispositions) != 1 {
		t.Fatalf("earlier dispositions changed\n got %s\nwant %s", got, want)
	}
	// Inside the build stage a completed verdict still cannot be reclassified.
	f.rescope(t, "Build order one, amended")
	refusedWith(t, "reclassified build verdict", f.assign(f.criteria, []string{"a1", "a2"}), "cannot reclassify")
}

// a3: an earlier stage neither blocks nor satisfies acceptance of the new one.
func TestReviewStageAcceptanceIgnoresEarlierStages(t *testing.T) {
	t.Run("earlier failure does not block", func(t *testing.T) {
		f, handler := findingsStage(t)
		f.rescope(t, "Build order one")
		f.confirmScope(t, handler, f.ownerOrder(t, "build-order"), "build-scope")
		if err := f.assign(f.criteria, nil); err != nil {
			t.Fatal(err)
		}
		refusedWith(t, "accept before any build review", f.acceptCandidate(candidateA, api.Agent{}), "completed review required")
		f.generalRound(t, candidateB, passConvergence, api.ReviewMetadata{})
		// The findings stage left a2 failed with blocker b1 unresolved.
		if err := f.acceptCandidate(candidateB, api.Agent{}); err != nil {
			t.Fatal("earlier stage blocked acceptance", err)
		}
		done := "done"
		if _, err := f.s.UpdateWorkItem(f.ctx, f.task.ID, f.item.ID, api.UpdateWorkItemRequest{Revision: f.item.Revision, Status: &done}, f.by); err != nil {
			t.Fatal("earlier stage blocked the done save", err)
		}
	})
	t.Run("earlier pass does not satisfy", func(t *testing.T) {
		f := newConvergenceFixture(t)
		handler := f.stageHandler(t)
		f.confirmScope(t, handler, f.ownerOrder(t, "first-order"), "first-scope")
		f.generalRound(t, candidateA, passConvergence, api.ReviewMetadata{})
		if err := f.acceptCandidate(candidateA, api.Agent{}); err != nil {
			t.Fatal(err)
		}
		f.rescope(t, "Build order one")
		f.confirmScope(t, handler, f.ownerOrder(t, "build-order"), "build-scope")
		if err := f.assign(f.criteria, nil); err != nil {
			t.Fatal(err)
		}
		// Identical criteria: inside one stage these verdicts would stay usable.
		refusedWith(t, "accept on the earlier stage's pass", f.acceptCandidate(candidateA, api.Agent{}), "completed review required")
		done := "done"
		_, err := f.s.UpdateWorkItem(f.ctx, f.task.ID, f.item.ID, api.UpdateWorkItemRequest{Revision: f.item.Revision, Status: &done}, f.by)
		refusedWith(t, "done on the earlier stage's pass", err, "saved lead acceptance required")
		// A focused request cannot reach back into the earlier stage either.
		_, err = f.focusedRequest(api.ReviewMetadata{Mode: "focused", Candidate: candidateB, Fix: "fix b1", BlockerIDs: []string{"b1"}})
		refusedWith(t, "focused request on the earlier stage", err, "focused path requires two completed general reviews")
	})
}

// a2: a scope revision opens no fresh count unless its saved confirmation
// names a different owner order.
func TestReviewStageSameStageKeepsLimits(t *testing.T) {
	t.Run("no saved confirmation", func(t *testing.T) {
		f, _ := findingsStage(t)
		f.rescope(t, "Amended without a confirmation")
		refusedWith(t, "reclassified verdict", f.assign(f.criteria, []string{"a2"}), "cannot reclassify")
		if err := f.assign(f.criteria, nil); err != nil {
			t.Fatal(err)
		}
		refusedWith(t, "third review", f.reviewErr(candidateB), "third general review refused")
	})
	t.Run("same owner order", func(t *testing.T) {
		f := newConvergenceFixture(t)
		handler := f.stageHandler(t)
		order := f.ownerOrder(t, "findings-order")
		f.confirmScope(t, handler, order, "findings-scope")
		f.generalRound(t, candidateA, passConvergence, api.ReviewMetadata{})
		f.generalRound(t, candidateA, passConvergence, api.ReviewMetadata{})
		f.rescope(t, "Amended under the same order")
		f.confirmScope(t, handler, order, "amended-scope")
		refusedWith(t, "reclassified verdict", f.assign(f.criteria, []string{"a2"}), "cannot reclassify")
		if err := f.assign(f.criteria, nil); err != nil {
			t.Fatal(err)
		}
		refusedWith(t, "third review", f.reviewErr(candidateB), "third general review refused")
		if st := f.state(t); len(st.Rounds) != 2 || len(st.Scopes) != 2 {
			t.Fatal("same order changed the ledger", st)
		}
	})
	t.Run("first confirmation names no earlier order", func(t *testing.T) {
		f := newConvergenceFixture(t)
		handler := f.stageHandler(t)
		f.generalRound(t, candidateA, passConvergence, api.ReviewMetadata{})
		f.generalRound(t, candidateA, passConvergence, api.ReviewMetadata{})
		f.rescope(t, "First confirmed scope")
		f.confirmScope(t, handler, f.ownerOrder(t, "first-order"), "first-scope")
		if err := f.assign(f.criteria, nil); err != nil {
			t.Fatal(err)
		}
		refusedWith(t, "third review", f.reviewErr(candidateB), "third general review refused")
	})
	t.Run("new order before its assignment", func(t *testing.T) {
		f := newConvergenceFixture(t)
		handler := f.stageHandler(t)
		f.confirmScope(t, handler, f.ownerOrder(t, "findings-order"), "findings-scope")
		f.generalRound(t, candidateA, passConvergence, api.ReviewMetadata{})
		open := f.review(t, candidateA)
		f.rescope(t, "Build order one")
		f.confirmScope(t, handler, f.ownerOrder(t, "build-order"), "build-scope")
		// Only the ASSIGN opens the stage: the open review still takes its result.
		if _, err := f.post(f.resultEnv(candidateA, passConvergence, api.ReviewMetadata{Mode: "general"}), "", open.Seq, f.reviewer); err != nil {
			t.Fatal("open review result after a confirmed new order", err)
		}
		refusedWith(t, "review before the build assignment", f.reviewErr(candidateB), "third general review refused")
		if err := f.assign(f.criteria, nil); err != nil {
			t.Fatal(err)
		}
		f.generalRound(t, candidateB, passConvergence, api.ReviewMetadata{})
		if st := f.state(t); len(st.Rounds) != 3 || st.Rounds[1].Number != 2 || st.Rounds[1].ResultSeq == 0 || st.Rounds[2].Number != 1 {
			t.Fatal("stage opened before its assignment", st.Rounds)
		}
	})
}

// a3: a confirmation saved after a scope already continued a stage's
// numbering never splits or renumbers that stage; the next scope under the
// new order opens the fresh count.
func TestReviewStageLateConfirmationNeverRenumbers(t *testing.T) {
	f := newConvergenceFixture(t)
	handler := f.stageHandler(t)
	f.confirmScope(t, handler, f.ownerOrder(t, "findings-order"), "findings-scope")
	f.generalRound(t, candidateA, passConvergence, api.ReviewMetadata{})
	f.rescope(t, "Amended inside the stage")
	if err := f.assign(f.criteria, nil); err != nil {
		t.Fatal(err)
	}
	f.generalRound(t, candidateB, passConvergence, api.ReviewMetadata{})
	before := stageJSON(t, f.state(t).Rounds)
	build := f.ownerOrder(t, "build-order")
	f.confirmScope(t, handler, build, "late-scope")
	refusedWith(t, "third review after a late confirmation", f.reviewErr(candidateC), "third general review refused")
	if err := f.acceptCandidate(candidateB, api.Agent{}); err != nil {
		t.Fatal("late confirmation hid the stage's reviews", err)
	}
	if got := stageJSON(t, f.state(t).Rounds); got != before {
		t.Fatalf("rounds changed\n got %s\nwant %s", got, before)
	}
	f.rescope(t, "Build order one")
	f.confirmScope(t, handler, build, "build-scope")
	if err := f.assign(f.criteria, []string{"a2"}); err != nil {
		t.Fatal(err)
	}
	f.verificationCriteria = []string{"a2"}
	f.generalRound(t, candidateC, map[string]string{"a1": "pass", "a2": "pending-verification"}, api.ReviewMetadata{})
	if st := f.state(t); len(st.Rounds) != 3 || st.Rounds[2].Number != 1 || st.Rounds[2].ScopeRevision != 3 || stageJSON(t, st.Rounds[:2]) != before {
		t.Fatal("build stage numbering", st.Rounds)
	}
}

// a3: unknown legacy history gets no fresh count from a new owner order.
func TestReviewStageUnknownLegacyHistoryGetsNoFreshCount(t *testing.T) {
	f := newConvergenceFixture(t)
	handler := f.stageHandler(t)
	f.confirmScope(t, handler, f.ownerOrder(t, "legacy-order"), "legacy-scope")
	if _, err := f.s.db.Exec(`DELETE FROM review_convergence WHERE task_id=? AND item_id=?`, f.task.ID, f.item.ID); err != nil {
		t.Fatal(err)
	}
	task, err := f.s.GetTask(f.ctx, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := f.s.db.BeginTx(f.ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	legacy := f.req(api.Envelope{Kind: "review", Subject: "Legacy exact native fixture review", Body: api.EnvelopeBody{Candidate: candidateA, Scope: "Fixture", Acceptance: f.criteria}}, f.reviewer.ID, 0, api.Agent{})
	if _, err = f.s.insertMessage(f.ctx, tx, task, legacy, f.reviewer, f.by, false, false); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = f.assign(f.criteria, nil); err != nil {
		t.Fatal(err)
	}
	f.rescope(t, "Build order one")
	f.confirmScope(t, handler, f.ownerOrder(t, "build-order"), "build-scope")
	if err = f.assign(f.criteria, nil); err != nil {
		t.Fatal(err)
	}
	if f.state(t).History != "unknown" {
		t.Fatal("legacy history became known")
	}
	refusedWith(t, "fresh review on unknown history", f.reviewErr(candidateB), "legacy review history is unknown")
}

// doneSave saves the item done, which runs the review done check.
func (f *convergenceFixture) doneSave() error {
	done := "done"
	_, err := f.s.UpdateWorkItem(f.ctx, f.task.ID, f.item.ID, api.UpdateWorkItemRequest{Revision: f.item.Revision, Status: &done}, f.by)
	return err
}

// f1 merge (wi_44b17e10c6450225): the scope that opened stage two is later
// also confirmed under the findings order, at the same item revision, so the
// findings order becomes that scope's earliest confirmation. The recorded
// stage stays: an amendment under the build order continues stage two and
// gets no third general review.
func TestReviewStageLaterConfirmationUnderEarlierOrderKeepsStage(t *testing.T) {
	f, handler := findingsStage(t)
	f.rescope(t, "Build order one")
	build := f.ownerOrder(t, "build-order")
	f.confirmScope(t, handler, build, "build-scope")
	if err := f.assign(f.criteria, nil); err != nil {
		t.Fatal(err)
	}
	f.generalRound(t, candidateB, passConvergence, api.ReviewMetadata{})
	f.generalRound(t, candidateC, passConvergence, api.ReviewMetadata{})
	before := stageJSON(t, f.state(t))
	var findings api.Message
	if err := f.s.db.QueryRow(`SELECT order_seq FROM work_order_scope_confirmations WHERE task_id=? AND request_id='findings-scope'`, f.task.ID).Scan(&findings.Seq); err != nil {
		t.Fatal(err)
	}
	if findings.Seq >= build.Seq {
		t.Fatal("findings order is not the earlier order", findings.Seq, build.Seq)
	}
	f.confirmScope(t, handler, findings, "rebind-scope")
	refusedWith(t, "third review of stage two after the rebind", f.reviewErr(candidateC), "third general review refused")
	if got := stageJSON(t, f.state(t)); got != before {
		t.Fatalf("ledger changed\n got %s\nwant %s", got, before)
	}
	f.rescope(t, "Build order one, amended")
	f.confirmScope(t, handler, build, "amended-scope")
	if err := f.assign(f.criteria, nil); err != nil {
		t.Fatal(err)
	}
	refusedWith(t, "third review of stage two after its amendment", f.reviewErr(candidateC), "third general review refused")
	if err := f.acceptCandidate(candidateC, api.Agent{}); err != nil {
		t.Fatal("stage two's reviews no longer answer acceptance:", err)
	}
	if err := f.doneSave(); err != nil {
		t.Fatal("stage two's reviews no longer answer the done check:", err)
	}
}

// f1 split (wi_44b17e10c6450225): a scope assigned unconfirmed and accepted
// on its stage's reviews is confirmed under a new owner order afterwards. The
// confirmation opens nothing: the stage's reviews still answer the done check.
func TestReviewStageConfirmationAfterAssignNeverSplits(t *testing.T) {
	f := newConvergenceFixture(t)
	handler := f.stageHandler(t)
	f.confirmScope(t, handler, f.ownerOrder(t, "findings-order"), "findings-scope")
	f.generalRound(t, candidateA, passConvergence, api.ReviewMetadata{})
	f.generalRound(t, candidateA, passConvergence, api.ReviewMetadata{})
	f.rescope(t, "Amended inside the stage")
	if err := f.assign(f.criteria, nil); err != nil {
		t.Fatal(err)
	}
	if err := f.acceptCandidate(candidateA, api.Agent{}); err != nil {
		t.Fatal(err)
	}
	before := stageJSON(t, f.state(t))
	f.confirmScope(t, handler, f.ownerOrder(t, "build-order"), "late-scope")
	if err := f.doneSave(); err != nil {
		t.Fatal("confirmation after the scope's ASSIGN split the stage:", err)
	}
	if got := stageJSON(t, f.state(t)); got != before {
		t.Fatalf("ledger changed\n got %s\nwant %s", got, before)
	}
}

// The later-revision form of the f1 merge: a non-scope edit moves the item
// revision before the stage-two scope is confirmed again under the findings
// order. That row is never the scope's earliest, so it changed nothing before
// stages were recorded either; it must keep changing nothing.
func TestReviewStageLaterRevisionRebindKeepsStage(t *testing.T) {
	f, handler := findingsStage(t)
	f.rescope(t, "Build order one")
	f.confirmScope(t, handler, f.ownerOrder(t, "build-order"), "build-scope")
	if err := f.assign(f.criteria, nil); err != nil {
		t.Fatal(err)
	}
	f.generalRound(t, candidateB, passConvergence, api.ReviewMetadata{})
	f.generalRound(t, candidateC, passConvergence, api.ReviewMetadata{})
	if err := f.acceptCandidate(candidateC, api.Agent{}); err != nil {
		t.Fatal(err)
	}
	before := stageJSON(t, f.state(t))
	priority := "high"
	var err error
	if f.item, err = f.s.UpdateWorkItem(f.ctx, f.task.ID, f.item.ID, api.UpdateWorkItemRequest{Revision: f.item.Revision, Priority: &priority}, f.by); err != nil {
		t.Fatal(err)
	}
	if f.item.ScopeRevision != 2 {
		t.Fatal("priority edit moved the scope revision", f.item.ScopeRevision)
	}
	var findings api.Message
	if err = f.s.db.QueryRow(`SELECT order_seq FROM work_order_scope_confirmations WHERE task_id=? AND request_id='findings-scope'`, f.task.ID).Scan(&findings.Seq); err != nil {
		t.Fatal(err)
	}
	f.confirmScope(t, handler, findings, "rebind-scope")
	refusedWith(t, "third review of stage two after the rebind", f.reviewErr(candidateC), "third general review refused")
	if got := stageJSON(t, f.state(t)); got != before {
		t.Fatalf("ledger changed\n got %s\nwant %s", got, before)
	}
	if err = f.doneSave(); err != nil {
		t.Fatal(err)
	}
}

func wantStages(t *testing.T, name string, got []api.ReviewStage, want ...api.ReviewStage) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s: stages\n got %+v\nwant %+v", name, got, want)
	}
}

// v1 (wi_44b17e10c6450225): the stage is stored when its ASSIGN is saved, with
// its order, scope revision and assignment sequence. A refused ASSIGN leaves
// none behind, and a third stage is recorded the same way.
func TestReviewStageRecordedAtAssign(t *testing.T) {
	f := newConvergenceFixture(t)
	handler := f.stageHandler(t)
	first := f.state(t)
	wantStages(t, "first ASSIGN", first.Stages, api.ReviewStage{Number: 1, ScopeRevision: 1, AssignmentSeq: first.Scopes[0].AssignmentSeq})
	findings := f.ownerOrder(t, "findings-order")
	f.confirmScope(t, handler, findings, "findings-scope")
	f.generalRound(t, candidateA, passConvergence, api.ReviewMetadata{})
	f.generalRound(t, candidateA, passConvergence, api.ReviewMetadata{})
	// A confirmation saved after the ASSIGN does not rewrite the stored stage.
	wantStages(t, "after a late confirmation", f.state(t).Stages, first.Stages...)

	f.rescope(t, "Build order one")
	build := f.ownerOrder(t, "build-order")
	f.confirmScope(t, handler, build, "build-scope")
	// R1: an ASSIGN refused after the stage decision rolls the stage back.
	refusedWith(t, "non-contiguous criteria", f.assign(map[string]string{"a1": "builds", "a3": "skips"}, nil), "contiguous a1..aN")
	wantStages(t, "after a refused ASSIGN", f.state(t).Stages, first.Stages...)
	if err := f.assign(f.criteria, nil); err != nil {
		t.Fatal(err)
	}
	second := f.state(t)
	one := api.ReviewStage{Number: 1, OrderSeq: findings.Seq, ScopeRevision: 1, AssignmentSeq: first.Scopes[0].AssignmentSeq}
	two := api.ReviewStage{Number: 2, OrderSeq: build.Seq, ScopeRevision: 2, AssignmentSeq: second.Scopes[1].AssignmentSeq}
	wantStages(t, "second-stage ASSIGN", second.Stages, one, two)
	if !strings.Contains(stageJSON(t, second), fmt.Sprintf(`"stages":[{"number":1,"orderSeq":%d,"scopeRevision":1,"assignmentSeq":%d},{"number":2,"orderSeq":%d,"scopeRevision":2,"assignmentSeq":%d}]`, one.OrderSeq, one.AssignmentSeq, two.OrderSeq, two.AssignmentSeq)) {
		t.Fatal("stored stage shape", stageJSON(t, second))
	}
	if n, rounds, earlier := second.CurrentStage(); n != 2 || len(rounds) != 0 || earlier != 2 {
		t.Fatal("current stage after its ASSIGN", n, len(rounds), earlier)
	}
	f.generalRound(t, candidateB, passConvergence, api.ReviewMetadata{})
	f.generalRound(t, candidateB, passConvergence, api.ReviewMetadata{})
	// A re-ASSIGN of the same scope and an amendment under the same order
	// stay in stage two.
	if err := f.assign(f.criteria, nil); err != nil {
		t.Fatal(err)
	}
	f.rescope(t, "Build order one, amended")
	f.confirmScope(t, handler, build, "amended-scope")
	if err := f.assign(f.criteria, nil); err != nil {
		t.Fatal(err)
	}
	wantStages(t, "same order amendment", f.state(t).Stages, one, two)
	refusedWith(t, "third review of stage two", f.reviewErr(candidateB), "third general review refused")

	f.rescope(t, "Build order two")
	third := f.ownerOrder(t, "third-order")
	f.confirmScope(t, handler, third, "third-scope")
	if err := f.assign(f.criteria, nil); err != nil {
		t.Fatal(err)
	}
	f.generalRound(t, candidateC, passConvergence, api.ReviewMetadata{})
	st := f.state(t)
	wantStages(t, "third-stage ASSIGN", st.Stages, one, two, api.ReviewStage{Number: 3, OrderSeq: third.Seq, ScopeRevision: 4, AssignmentSeq: scopeFor(&st, 4).AssignmentSeq})
	if n, rounds, earlier := st.CurrentStage(); n != 3 || len(rounds) != 1 || earlier != 4 || rounds[0].Number != 1 {
		t.Fatal("current stage of three", n, len(rounds), earlier)
	}
	if err := f.acceptCandidate(candidateC, api.Agent{}); err != nil {
		t.Fatal(err)
	}
	if err := f.doneSave(); err != nil {
		t.Fatal(err)
	}
}

// A first stage recorded before its scope was confirmed is named, at the next
// scope's first ASSIGN, from the earliest confirmation among its own scopes;
// with none it takes the new scope's order and stays one stage.
func TestReviewStageUnnamedFirstStageIsNamedFromItsOwnScopes(t *testing.T) {
	t.Run("own confirmation saved after the ASSIGN", func(t *testing.T) {
		f := newConvergenceFixture(t)
		handler := f.stageHandler(t)
		findings := f.ownerOrder(t, "findings-order")
		f.confirmScope(t, handler, findings, "findings-scope")
		if st := f.state(t); st.Stages[0].OrderSeq != 0 {
			t.Fatal("confirmation after the ASSIGN named the stage early", st.Stages)
		}
		f.rescope(t, "Build order one")
		build := f.ownerOrder(t, "build-order")
		f.confirmScope(t, handler, build, "build-scope")
		if err := f.assign(f.criteria, nil); err != nil {
			t.Fatal(err)
		}
		st := f.state(t)
		if len(st.Stages) != 2 || st.Stages[0].OrderSeq != findings.Seq || st.Stages[1].OrderSeq != build.Seq {
			t.Fatal("stage one was not named from its own scope", st.Stages)
		}
	})
	t.Run("no confirmation of its own", func(t *testing.T) {
		f := newConvergenceFixture(t)
		handler := f.stageHandler(t)
		f.rescope(t, "First confirmed scope")
		order := f.ownerOrder(t, "first-order")
		f.confirmScope(t, handler, order, "first-scope")
		if err := f.assign(f.criteria, nil); err != nil {
			t.Fatal(err)
		}
		st := f.state(t)
		wantStages(t, "adopted order", st.Stages, api.ReviewStage{Number: 1, OrderSeq: order.Seq, ScopeRevision: 1, AssignmentSeq: st.Scopes[0].AssignmentSeq})
	})
}

// stripStages rewrites the stored ledger as one saved before stages were
// recorded.
func (f *convergenceFixture) stripStages(t *testing.T) {
	t.Helper()
	if _, err := f.s.db.Exec(`UPDATE review_convergence SET state_json=json_remove(state_json,'$.stages') WHERE task_id=? AND item_id=?`, f.task.ID, f.item.ID); err != nil {
		t.Fatal(err)
	}
	if st := f.state(t); len(st.Stages) != 0 {
		t.Fatal("stages not stripped", st.Stages)
	}
}

// v1 legacy (wi_44b17e10c6450225): a ledger stored without stages keeps the
// derived boundary, is pinned by its next saved transition and does not move
// afterwards. Unknown history records no stage.
func TestReviewStageLegacyLedgerIsDerivedOnceThenPinned(t *testing.T) {
	t.Run("two stages", func(t *testing.T) {
		f, handler := findingsStage(t)
		f.rescope(t, "Build order one")
		build := f.ownerOrder(t, "build-order")
		f.confirmScope(t, handler, build, "build-scope")
		if err := f.assign(f.criteria, nil); err != nil {
			t.Fatal(err)
		}
		f.generalRound(t, candidateB, passConvergence, api.ReviewMetadata{})
		recorded := f.state(t).Stages
		f.stripStages(t)
		// R3: a reader cannot derive, so the stripped ledger reads as one stage.
		if n, rounds, earlier := f.state(t).CurrentStage(); n != 1 || len(rounds) != 3 || earlier != 0 {
			t.Fatal("stripped ledger reader", n, len(rounds), earlier)
		}
		// A refusal and the done check derive in memory and store nothing (R6).
		refusedWith(t, "accept on the findings stage's candidate", f.acceptCandidate(candidateA, api.Agent{}), "changed candidate needs exact focused verification")
		refusedWith(t, "done check before acceptance", f.doneSave(), "saved lead acceptance required")
		if st := f.state(t); len(st.Stages) != 0 {
			t.Fatal("a refused transition stored stages", st.Stages)
		}
		// The next saved transition pins the derived stages: stage two still
		// has one review left, as it had before the strip.
		f.generalRound(t, candidateC, passConvergence, api.ReviewMetadata{})
		pinned := f.state(t)
		wantStages(t, "pinned", pinned.Stages, recorded...)
		if pinned.Rounds[3].Number != 2 {
			t.Fatal("derived stage numbering", pinned.Rounds)
		}
		refusedWith(t, "third review of the pinned stage", f.reviewErr(candidateC), "third general review refused")
		// Pinned, it no longer moves: the f1 merge row changes nothing.
		var findings api.Message
		if err := f.s.db.QueryRow(`SELECT order_seq FROM work_order_scope_confirmations WHERE task_id=? AND request_id='findings-scope'`, f.task.ID).Scan(&findings.Seq); err != nil {
			t.Fatal(err)
		}
		f.confirmScope(t, handler, findings, "rebind-scope")
		if err := f.acceptCandidate(candidateC, api.Agent{}); err != nil {
			t.Fatal(err)
		}
		wantStages(t, "after the rebind", f.state(t).Stages, recorded...)
		if err := f.doneSave(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("one scope", func(t *testing.T) {
		f := newConvergenceFixture(t)
		handler := f.stageHandler(t)
		findings := f.ownerOrder(t, "findings-order")
		f.confirmScope(t, handler, findings, "findings-scope")
		f.generalRound(t, candidateA, passConvergence, api.ReviewMetadata{})
		f.stripStages(t)
		f.generalRound(t, candidateA, passConvergence, api.ReviewMetadata{})
		if st := f.state(t); len(st.Stages) != 0 || len(st.Rounds) != 2 {
			t.Fatal("a one-scope ledger has nothing to derive", st.Stages)
		}
		f.rescope(t, "Build order one")
		build := f.ownerOrder(t, "build-order")
		f.confirmScope(t, handler, build, "build-scope")
		if err := f.assign(f.criteria, nil); err != nil {
			t.Fatal(err)
		}
		st := f.state(t)
		wantStages(t, "next scope's ASSIGN", st.Stages, api.ReviewStage{Number: 1, OrderSeq: findings.Seq, ScopeRevision: 1, AssignmentSeq: st.Scopes[0].AssignmentSeq}, api.ReviewStage{Number: 2, OrderSeq: build.Seq, ScopeRevision: 2, AssignmentSeq: st.Scopes[1].AssignmentSeq})
	})
	t.Run("unknown history", func(t *testing.T) {
		f := newConvergenceFixture(t)
		handler := f.stageHandler(t)
		f.confirmScope(t, handler, f.ownerOrder(t, "legacy-order"), "legacy-scope")
		if _, err := f.s.db.Exec(`UPDATE review_convergence SET state_json=json_set(json_remove(state_json,'$.stages'),'$.history','unknown') WHERE task_id=? AND item_id=?`, f.task.ID, f.item.ID); err != nil {
			t.Fatal(err)
		}
		f.rescope(t, "Build order one")
		f.confirmScope(t, handler, f.ownerOrder(t, "build-order"), "build-scope")
		if err := f.assign(f.criteria, nil); err != nil {
			t.Fatal(err)
		}
		if st := f.state(t); st.History != "unknown" || len(st.Stages) != 0 || len(st.Scopes) != 2 {
			t.Fatal("unknown history recorded a stage", st.History, st.Stages)
		}
	})
}

// v2 (wi_44b17e10c6450225): only an order the owner wrote opens a stage. A
// scope confirmed under a linked message an agent wrote stays in its stage.
func TestReviewStageOnlyOwnerWrittenOrderOpensStage(t *testing.T) {
	run := func(t *testing.T, ownerWritten bool) (*convergenceFixture, api.Agent, api.Message) {
		f, handler := findingsStage(t)
		f.rescope(t, "Build order one")
		var order api.Message
		if ownerWritten {
			order = f.ownerOrder(t, "build-order")
		} else {
			var err error
			order, err = f.s.PostMessage(f.ctx, f.task.ID, api.PostMessageRequest{Text: "build-order", RequestID: "build-order", AgentID: handler.ID, RunID: handler.RunID, WorkItems: []api.MessageWorkItem{{ItemTaskID: f.task.ID, ItemID: f.item.ID, ItemRevision: f.item.Revision, Relationship: "primary"}}}, f.by)
			if err != nil {
				t.Fatal(err)
			}
			if order.From.AgentID != handler.ID {
				t.Fatal("fixture order is not agent-written", order.From)
			}
		}
		f.confirmScope(t, handler, order, "build-scope")
		if err := f.assign(f.criteria, nil); err != nil {
			t.Fatal(err)
		}
		return f, handler, order
	}
	t.Run("agent-written", func(t *testing.T) {
		f, handler, _ := run(t, false)
		refusedWith(t, "third review under an agent-written order", f.reviewErr(candidateB), "third general review refused")
		if st := f.state(t); len(st.Stages) != 1 || len(st.Rounds) != 2 {
			t.Fatal("agent-written order opened a stage", st.Stages)
		}
		// Recovery: the owner posts the order and a new scope revision takes it.
		f.rescope(t, "Build order one, owner order")
		order := f.ownerOrder(t, "owner-build-order")
		f.confirmScope(t, handler, order, "owner-build-scope")
		if err := f.assign(f.criteria, nil); err != nil {
			t.Fatal(err)
		}
		f.generalRound(t, candidateB, passConvergence, api.ReviewMetadata{})
		if st := f.state(t); len(st.Stages) != 2 || st.Stages[1].OrderSeq != order.Seq || st.Stages[1].ScopeRevision != 3 {
			t.Fatal("owner order after an agent-written one", st.Stages)
		}
	})
	t.Run("owner-written", func(t *testing.T) {
		f, _, order := run(t, true)
		f.generalRound(t, candidateB, passConvergence, api.ReviewMetadata{})
		if st := f.state(t); len(st.Stages) != 2 || st.Stages[1].OrderSeq != order.Seq || len(st.Rounds) != 3 {
			t.Fatal("owner-written order opened no stage", st.Stages)
		}
	})
}

// s1 (wi_c4b7284e2cb46691): a typed notice primary-linked to the item and
// stored with no agent identity, such as the queue lane notice the owner's
// command posts or a notice the hub writes itself, is not an owner order. A
// scope confirmed under one stays in its stage; a real owner order still opens
// one.
func TestReviewStageHubWrittenNoticeIsNotOwnerOrder(t *testing.T) {
	for _, tc := range []struct {
		name string
		by   api.Caller
	}{
		{"queue lane notice under the owner credential", api.Caller{Node: "workspace", User: "owner"}},
		{"notice the hub writes", api.Caller{Node: "team_queue", User: "rebind"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, handler := findingsStage(t)
			f.rescope(t, "Build order one")
			notice := api.Envelope{Kind: api.EnvelopeKindNotice, Subject: "A bug was queued on Planned delivery with a recorded reason", Body: api.EnvelopeBody{Text: "Queue entry fixture puts the bug on template planned."}}
			order, err := f.s.PostMessage(f.ctx, f.task.ID, api.PostMessageRequest{Envelope: &notice, RequestID: "lane-notice", WorkItems: []api.MessageWorkItem{{ItemTaskID: f.task.ID, ItemID: f.item.ID, ItemRevision: f.item.Revision, Relationship: "primary"}}}, tc.by)
			if err != nil {
				t.Fatal(err)
			}
			if order.From.AgentID != "" || order.From.Node == "system" {
				t.Fatal("fixture notice is not stored without an agent identity", order.From)
			}
			f.confirmScope(t, handler, order, "notice-scope")
			if err := f.assign(f.criteria, nil); err != nil {
				t.Fatal(err)
			}
			refusedWith(t, "third review under a notice", f.reviewErr(candidateB), "third general review refused")
			if st := f.state(t); len(st.Stages) != 1 || len(st.Rounds) != 2 {
				t.Fatal("a notice opened a stage", st.Stages)
			}
			f.rescope(t, "Build order one, owner order")
			owner := f.ownerOrder(t, "owner-build-order")
			f.confirmScope(t, handler, owner, "owner-build-scope")
			if err := f.assign(f.criteria, nil); err != nil {
				t.Fatal(err)
			}
			f.generalRound(t, candidateB, passConvergence, api.ReviewMetadata{})
			if st := f.state(t); len(st.Stages) != 2 || st.Stages[1].OrderSeq != owner.Seq || st.Stages[1].ScopeRevision != 3 {
				t.Fatal("owner order after a notice", st.Stages)
			}
		})
	}
}

// s2 (wi_c4b7284e2cb46691): a scope confirmed again under an order an earlier
// stage already names opens no stage. Stage two keeps its count, and the
// next new owner order still opens stage three.
func TestReviewStageReturningEarlierOrderOpensNoStage(t *testing.T) {
	f, handler := findingsStage(t)
	f.rescope(t, "Build order one")
	build := f.ownerOrder(t, "build-order")
	f.confirmScope(t, handler, build, "build-scope")
	if err := f.assign(f.criteria, nil); err != nil {
		t.Fatal(err)
	}
	f.generalRound(t, candidateB, passConvergence, api.ReviewMetadata{})
	f.generalRound(t, candidateB, passConvergence, api.ReviewMetadata{})
	two := f.state(t).Stages
	findings := two[0].OrderSeq
	if len(two) != 2 || findings == 0 || findings == build.Seq {
		t.Fatal("stage one is not named by the findings order", two)
	}

	f.rescope(t, "Back to the findings order")
	f.confirmScope(t, handler, api.Message{Seq: findings}, "returning-scope")
	if err := f.assign(f.criteria, nil); err != nil {
		t.Fatal(err)
	}
	wantStages(t, "returning to the stage one order", f.state(t).Stages, two...)
	refusedWith(t, "third review of stage two after the return", f.reviewErr(candidateC), "third general review refused")

	f.rescope(t, "Build order two")
	third := f.ownerOrder(t, "third-order")
	f.confirmScope(t, handler, third, "third-scope")
	if err := f.assign(f.criteria, nil); err != nil {
		t.Fatal(err)
	}
	st := f.state(t)
	wantStages(t, "new order after the return", st.Stages, append(two, api.ReviewStage{Number: 3, OrderSeq: third.Seq, ScopeRevision: 4, AssignmentSeq: st.Scopes[3].AssignmentSeq})...)
}

// foldFile is the finding file the fold fixtures name.
const foldFile = "hub/internal/store/fold.go"

// foldTargets files one item that can take a fold of foldFile and one item
// per reason it cannot: each names foldFile unless the key says otherwise.
func foldTargets(t *testing.T, s *Store, task api.Task) map[string]api.WorkItem {
	t.Helper()
	ctx := context.Background()
	by := api.Caller{Node: "fixture", User: "owner"}
	out := map[string]api.WorkItem{}
	file := func(key, description string) api.WorkItem {
		item, err := s.CreateWorkItem(ctx, task.ID, api.CreateWorkItemRequest{Kind: "bug", Title: "Fold target " + key, Description: description, Priority: "normal", RequestID: "fold-" + key}, by)
		if err != nil {
			t.Fatal(err)
		}
		out[key] = item
		return item
	}
	owns := "Owned paths: " + foldFile + "."
	file("eligible", owns)
	file("second", "- "+foldFile+", hub/internal/store/other.go")
	done := file("done", owns)
	if _, _, err := s.CreateWorkItemUpdate(ctx, task.ID, done.ID, api.CreateWorkItemUpdate{ExpectedRevision: done.Revision, Status: ptr("done"), RequestID: "fold-done"}, by); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"accepted", "running", "queued"} {
		item := file(key, owns)
		order := contextLinkedMessage(t, s, task, item, "fold order "+key, "fold-order-"+key, nil)
		if _, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "fold-queue-" + key, Operation: "add", ItemID: item.ID, OrderMessageSeq: order.Seq, Host: "mini", Cwd: "/fold/" + key}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.Exec(`UPDATE team_queue_entries SET state='finished',acceptance_json='{"commit":"fixture"}' WHERE item_id=?`, out["accepted"].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE team_queue_entries SET state='running',released_at='' WHERE item_id=?`, out["running"].ID); err != nil {
		t.Fatal(err)
	}
	contextLinkedMessage(t, s, task, file("ordered", owns), "fold order", "fold-order-ordered", nil)
	file("other-file", "Owned paths: hub/internal/store/unrelated.go.")
	file("prefix", "Owned paths: hub/internal/store and hub/internal/store/fold.go.bak, x/"+foldFile)
	return out
}

// foldCounts counts the rows a refused fold must not write.
func foldCounts(t *testing.T, s *Store) [5]int {
	t.Helper()
	var n [5]int
	for i, table := range []string{"messages", "work_items", "work_item_revisions", "narrative_artifacts", "narrative_artifact_versions"} {
		if err := s.db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&n[i]); err != nil {
			t.Fatal(err)
		}
	}
	return n
}

type foldNote struct {
	namespace, kind, provenance, title, content, author string
}

func foldNotes(t *testing.T, s *Store, item string) []foldNote {
	t.Helper()
	rows, err := s.db.Query(`SELECT a.namespace,v.kind,v.provenance,v.title,v.content,v.original_author FROM narrative_artifacts a JOIN narrative_artifact_versions v ON v.artifact_id=a.id WHERE a.item_id=? ORDER BY v.narrative_seq`, item)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []foldNote
	for rows.Next() {
		var n foldNote
		if err := rows.Scan(&n.namespace, &n.kind, &n.provenance, &n.title, &n.content, &n.author); err != nil {
			t.Fatal(err)
		}
		out = append(out, n)
	}
	return out
}

func itemRevisionCount(t *testing.T, s *Store, item string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM work_item_revisions WHERE item_id=?`, item).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// a1: a finding whose into names an eligible item is saved there as one
// review-fold evidence note; no item is filed and the target is unchanged.
func TestReviewFoldSavesEvidenceNote(t *testing.T) {
	f := newConvergenceFixture(t)
	targets := foldTargets(t, f.s, f.task)
	target := targets["eligible"]
	revisions := itemRevisionCount(t, f.s, target.ID)
	before, _ := f.s.ListWorkItems(f.ctx, f.task.ID, "", "", 0, 100)
	r := f.review(t, candidateA)
	finding := api.ReviewFinding{ID: "f1", Title: "Fold fixture finding", File: foldFile, Line: 3, Into: target.ID}
	req := f.req(f.resultEnv(candidateA, passConvergence, api.ReviewMetadata{Mode: "general", Findings: []api.ReviewFinding{finding}}), "", r.Seq, f.reviewer)
	m, err := f.s.PostMessage(f.ctx, f.task.ID, req, f.by)
	if err != nil {
		t.Fatal(err)
	}
	// A retry of the same post replays it and writes nothing more.
	if again, err := f.s.PostMessage(f.ctx, f.task.ID, req, f.by); err != nil || again.Seq != m.Seq {
		t.Fatalf("replay %v %v", again.Seq, err)
	}
	after, _ := f.s.ListWorkItems(f.ctx, f.task.ID, "", "", 0, 100)
	if len(after.Items) != len(before.Items) {
		t.Fatalf("fold filed an item: %d -> %d", len(before.Items), len(after.Items))
	}
	got, err := f.s.GetWorkItem(f.ctx, f.task.ID, target.ID)
	if err != nil || got.Revision != target.Revision || got.ScopeRevision != target.ScopeRevision || !got.UpdatedAt.Equal(target.UpdatedAt) || got.Description != target.Description || itemRevisionCount(t, f.s, target.ID) != revisions {
		t.Fatalf("target changed: %+v -> %+v %v", target, got, err)
	}
	notes := foldNotes(t, f.s, target.ID)
	if len(notes) != 1 {
		t.Fatalf("notes %+v", notes)
	}
	n := notes[0]
	wantHead := fmt.Sprintf("Review finding f1 from %s, message #%d, folded as evidence.\n", f.item.ID, m.Seq)
	if n.namespace != "review-fold" || n.kind != "note" || n.provenance != "review-fold" || n.title != finding.Title || !strings.HasPrefix(n.content, wantHead) || !strings.Contains(n.author, f.reviewer.ID) {
		t.Fatalf("note %+v", n)
	}
	var saved api.ReviewFinding
	if err := json.Unmarshal([]byte(strings.TrimPrefix(n.content, wantHead)), &saved); err != nil || !reflect.DeepEqual(saved, finding) {
		t.Fatalf("note finding %+v %v", saved, err)
	}
	st := f.state(t)
	if len(st.FollowUps) != 1 || st.FollowUps[0].ItemID != target.ID || !st.FollowUps[0].Folded || st.FollowUps[0].MessageSeq != m.Seq || st.Rounds[0].ResultSeq != m.Seq {
		t.Fatalf("review state %+v", st)
	}
	var data string
	if err := f.s.db.QueryRow(`SELECT data FROM events WHERE task_id=? AND kind='review_finding_folded'`, f.task.ID).Scan(&data); err != nil {
		t.Fatal(err)
	}
	var event map[string]any
	if err := json.Unmarshal([]byte(data), &event); err != nil || event["itemId"] != target.ID || event["reviewParent"] != f.item.ID || event["findingId"] != "f1" || event["messageSeq"] != float64(m.Seq) {
		t.Fatalf("event %s %v", data, err)
	}
}

// a2, a12 (iii): each reason a fold is refused, with its exact text and error
// class; a refused result saves nothing.
func TestReviewFoldRefusals(t *testing.T) {
	type tc struct {
		finding func(f *convergenceFixture, targets map[string]api.WorkItem, other api.WorkItem) api.ReviewMetadata
		class   error
		reason  string
	}
	finding := func(into, file string) api.ReviewMetadata {
		return api.ReviewMetadata{Mode: "general", Findings: []api.ReviewFinding{{ID: "f1", Title: "Fold refusal fixture", File: file, Line: 3, Into: into}}}
	}
	byKey := func(key string) func(*convergenceFixture, map[string]api.WorkItem, api.WorkItem) api.ReviewMetadata {
		return func(_ *convergenceFixture, targets map[string]api.WorkItem, _ api.WorkItem) api.ReviewMetadata {
			return finding(targets[key].ID, foldFile)
		}
	}
	cases := map[string]tc{
		"no file": {func(_ *convergenceFixture, targets map[string]api.WorkItem, _ api.WorkItem) api.ReviewMetadata {
			return api.ReviewMetadata{Mode: "general", Findings: []api.ReviewFinding{{ID: "f1", Title: "Fold refusal fixture", Command: "go test", Output: "FAIL", Into: targets["eligible"].ID}}}
		}, api.ErrInvalid, "review.findings[f1].into: finding names no file"},
		"absolute file": {func(_ *convergenceFixture, targets map[string]api.WorkItem, _ api.WorkItem) api.ReviewMetadata {
			return finding(targets["eligible"].ID, "/"+foldFile)
		}, api.ErrInvalid, "review.findings[f1].into: finding file is not a repository-relative path"},
		"not an ID": {func(*convergenceFixture, map[string]api.WorkItem, api.WorkItem) api.ReviewMetadata {
			return finding("fold-target", foldFile)
		}, api.ErrInvalid, "review.findings[f1].into: must be a work item ID"},
		"other project": {func(_ *convergenceFixture, _ map[string]api.WorkItem, other api.WorkItem) api.ReviewMetadata {
			return finding(other.ID, foldFile)
		}, api.ErrConflict, "review.findings[f1].into: item is not in this project"},
		"reviewed item": {func(f *convergenceFixture, _ map[string]api.WorkItem, _ api.WorkItem) api.ReviewMetadata {
			return finding(f.item.ID, foldFile)
		}, api.ErrConflict, "review.findings[f1].into: item is the reviewed item"},
		"done":       {byKey("done"), api.ErrConflict, "review.findings[f1].into: item is done"},
		"accepted":   {byKey("accepted"), api.ErrConflict, "review.findings[f1].into: item is accepted and awaits release"},
		"running":    {byKey("running"), api.ErrConflict, "review.findings[f1].into: item is running"},
		"queued":     {byKey("queued"), api.ErrConflict, "review.findings[f1].into: item is queued"},
		"ordered":    {byKey("ordered"), api.ErrConflict, "review.findings[f1].into: item is ordered"},
		"other file": {byKey("other-file"), api.ErrConflict, "review.findings[f1].into: item does not name the finding's file"},
		"prefix":     {byKey("prefix"), api.ErrConflict, "review.findings[f1].into: item does not name the finding's file"},
		"blocker": {func(_ *convergenceFixture, targets map[string]api.WorkItem, _ api.WorkItem) api.ReviewMetadata {
			return api.ReviewMetadata{Mode: "general", Blockers: []api.ReviewFinding{{ID: "b1", Criterion: "a1", Title: "Fold refusal fixture", File: foldFile, Line: 3, Into: targets["eligible"].ID}}}
		}, api.ErrInvalid, "review.blockers[b1].into: only a finding can be folded"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := newConvergenceFixture(t)
			targets := foldTargets(t, f.s, f.task)
			otherTask, err := f.s.CreateTask(f.ctx, api.CreateTaskRequest{Name: "Other fold project"}, f.by)
			if err != nil {
				t.Fatal(err)
			}
			other, err := f.s.CreateWorkItem(f.ctx, otherTask.ID, api.CreateWorkItemRequest{Kind: "bug", Title: "Other project target", Description: foldFile, RequestID: "other"}, f.by)
			if err != nil {
				t.Fatal(err)
			}
			r := f.review(t, candidateA)
			before := foldCounts(t, f.s)
			status := passConvergence
			if name == "blocker" {
				status = map[string]string{"a1": "fail", "a2": "pass"}
			}
			_, err = f.post(f.resultEnv(candidateA, status, c.finding(f, targets, other)), "", r.Seq, f.reviewer)
			var envelopeErr *api.EnvelopeError
			if !errors.Is(err, c.class) || errors.As(err, &envelopeErr) || err.Error() != c.class.Error()+": "+c.reason {
				t.Fatalf("got %T %v, want %v: %s", err, err, c.class, c.reason)
			}
			if c.class == api.ErrInvalid && findingTitleReason(t, err) != c.reason {
				t.Fatalf("invalid reason %q", err)
			}
			if c.class == api.ErrConflict && errors.Is(err, api.ErrInvalid) {
				t.Fatal("conflict also wraps invalid")
			}
			st := f.state(t)
			if after := foldCounts(t, f.s); after != before || st.Rounds[0].ResultSeq != 0 || len(st.FollowUps) != 0 {
				t.Fatalf("refused result saved something: %v -> %v %+v", before, after, st)
			}
		})
	}
}

// a3: a saved finding ID cannot change its target; a result that folds one
// finding and files another writes exactly one note and one item; a new
// round-two blocker that is not a regression folds under review.blockers.
func TestReviewFoldStableIDAndMixedResult(t *testing.T) {
	f := newConvergenceFixture(t)
	targets := foldTargets(t, f.s, f.task)
	before := foldCounts(t, f.s)
	r := f.review(t, candidateA)
	folded := api.ReviewFinding{ID: "f1", Title: "Fold fixture finding", File: foldFile, Line: 3, Into: targets["eligible"].ID}
	filed := api.ReviewFinding{ID: "f2", Title: "File fixture finding", File: foldFile, Line: 4}
	if _, err := f.post(f.resultEnv(candidateA, passConvergence, api.ReviewMetadata{Mode: "general", Findings: []api.ReviewFinding{folded, filed}}), "", r.Seq, f.reviewer); err != nil {
		t.Fatal(err)
	}
	after := foldCounts(t, f.s)
	if after[1]-before[1] != 1 || after[3]-before[3] != 1 || after[4]-before[4] != 1 {
		t.Fatalf("mixed result: %v -> %v", before, after)
	}
	st := f.state(t)
	if len(st.FollowUps) != 2 || !st.FollowUps[0].Folded || st.FollowUps[1].Folded || st.FollowUps[1].ItemID == targets["eligible"].ID {
		t.Fatalf("follow-ups %+v", st.FollowUps)
	}
	r2 := f.review(t, candidateB)
	moved := folded
	moved.Into = targets["second"].ID
	if _, err := f.post(f.resultEnv(candidateB, passConvergence, api.ReviewMetadata{Mode: "general", Findings: []api.ReviewFinding{moved}}), "", r2.Seq, f.reviewer); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "follow-up ID changed meaning") {
		t.Fatalf("moved target: %v", err)
	}
	failed := map[string]string{"a1": "pass", "a2": "fail"}
	blocker := api.ReviewFinding{ID: "b2", Criterion: "a2", Title: "Round two fold fixture", File: foldFile, Line: 6, Into: targets["queued"].ID}
	if _, err := f.post(f.resultEnv(candidateB, failed, api.ReviewMetadata{Mode: "general", Blockers: []api.ReviewFinding{blocker}}), "", r2.Seq, f.reviewer); !errors.Is(err, api.ErrConflict) || err.Error() != "conflict: review.blockers[b2].into: item is queued" {
		t.Fatalf("demoted blocker refusal: %v", err)
	}
	blocker.Into = targets["second"].ID
	if _, err := f.post(f.resultEnv(candidateB, failed, api.ReviewMetadata{Mode: "general", Blockers: []api.ReviewFinding{blocker}}), "", r2.Seq, f.reviewer); err != nil {
		t.Fatal(err)
	}
	st = f.state(t)
	if len(st.FollowUps) != 3 || !st.FollowUps[2].Folded || st.FollowUps[2].ItemID != targets["second"].ID || len(foldNotes(t, f.s, targets["second"].ID)) != 1 {
		t.Fatalf("demoted blocker fold %+v", st.FollowUps)
	}
}

// a12 (i, ii): into reaches the store through the hub's plain JSON decode of a
// post, and tt send's strict review-file decode accepts it while still
// refusing an unknown field.
func TestReviewFoldWireDecode(t *testing.T) {
	f := newConvergenceFixture(t)
	target := foldTargets(t, f.s, f.task)["eligible"]
	r := f.review(t, candidateA)
	review := fmt.Sprintf(`{"mode":"general","candidate":%q,"findings":[{"id":"f1","title":"Wire fold fixture","file":%q,"line":3,"into":%q}]}`, candidateA, foldFile, target.ID)
	var meta api.ReviewMetadata
	dec := json.NewDecoder(strings.NewReader(review))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&meta); err != nil || len(meta.Findings) != 1 || meta.Findings[0].Into != target.ID {
		t.Fatalf("strict decode: %+v %v", meta, err)
	}
	dec = json.NewDecoder(strings.NewReader(strings.Replace(review, `"into"`, `"intoo"`, 1)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(new(api.ReviewMetadata)); err == nil || !strings.Contains(err.Error(), "intoo") {
		t.Fatalf("unknown sibling accepted: %v", err)
	}
	raw, err := json.Marshal(f.req(f.resultEnv(candidateA, passConvergence, api.ReviewMetadata{Mode: "general"}), "", r.Seq, f.reviewer))
	if err != nil {
		t.Fatal(err)
	}
	var shell map[string]json.RawMessage
	if err = json.Unmarshal(raw, &shell); err != nil {
		t.Fatal(err)
	}
	var env map[string]json.RawMessage
	if err = json.Unmarshal(shell["envelope"], &env); err != nil {
		t.Fatal(err)
	}
	env["review"] = json.RawMessage(review)
	if shell["envelope"], err = json.Marshal(env); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(shell)
	var req api.PostMessageRequest
	if err = json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.PostMessage(f.ctx, f.task.ID, req, f.by); err != nil {
		t.Fatal(err)
	}
	if notes := foldNotes(t, f.s, target.ID); len(notes) != 1 || notes[0].namespace != "review-fold" {
		t.Fatalf("wire post did not fold: %+v", notes)
	}
}

// The file test matches whole paths only.
func TestReviewFoldNamesFile(t *testing.T) {
	for text, want := range map[string]bool{
		foldFile:                        true,
		"owns " + foldFile + ".":        true,
		"`" + foldFile + "`, next":      true,
		`"file":"` + foldFile + `"`:     true,
		"(" + foldFile + ")":            true,
		"hub/internal/store":            false,
		foldFile + ".bak":               false,
		"x/" + foldFile:                 false,
		foldFile + "x":                  false,
		"a " + foldFile + "/b":          false,
		foldFile + "-x and " + foldFile: true,
	} {
		if got := namesFile(text, foldFile); got != want {
			t.Errorf("namesFile(%q) = %v, want %v", text, got, want)
		}
	}
}

// confirmOwned saves the handler's confirmation of the current scope under
// order with the given owned paths.
func (f *convergenceFixture) confirmOwned(t *testing.T, handler api.Agent, order api.Message, key string, owned []string) {
	t.Helper()
	req := api.ConfirmWorkOrderScopeRequest{RequestID: key, AgentID: handler.ID, RunID: handler.RunID, ExpectedRevision: f.item.Revision, ScopeRevision: f.item.ScopeRevision, OrderMessageSeq: order.Seq, Complete: true, Ownership: owned}
	if _, err := f.s.ConfirmWorkOrderScope(f.ctx, f.task.ID, f.item.ID, req); err != nil {
		t.Fatal(err)
	}
}

// queueOwned adds the fixture item to the queue under order with owned paths.
func (f *convergenceFixture) queueOwned(t *testing.T, order api.Message, key string, owned []string) {
	t.Helper()
	if _, err := f.s.TeamQueueAction(f.ctx, f.task.ID, api.TeamQueueRequest{RequestID: key, Operation: "add", ItemID: f.item.ID, OrderMessageSeq: order.Seq, Host: "mini", Cwd: "/fold/" + key, Ownership: owned}); err != nil {
		t.Fatal(err)
	}
}

// fileFoldItem files an open, held item with the given title and description.
func fileFoldItem(t *testing.T, f *convergenceFixture, key, title, description string) api.WorkItem {
	t.Helper()
	item, err := f.s.CreateWorkItem(f.ctx, f.task.ID, api.CreateWorkItemRequest{Kind: "bug", Title: title, Description: description, Priority: "normal", RequestID: "fold-candidate-" + key}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	return item
}

// foldCandidateNotices returns the fold-candidates notices after the REVIEW.
func foldCandidateNotices(t *testing.T, f *convergenceFixture, review int64) []api.Message {
	t.Helper()
	rows, err := f.s.db.Query(`SELECT seq FROM messages WHERE task_id=? AND seq>? AND json_valid(envelope) AND json_extract(envelope,'$.subject')=? ORDER BY seq`, f.task.ID, review, foldCandidatesSubject)
	if err != nil {
		t.Fatal(err)
	}
	var seqs []int64
	for rows.Next() {
		var seq int64
		if err := rows.Scan(&seq); err != nil {
			t.Fatal(err)
		}
		seqs = append(seqs, seq)
	}
	if err := closeRows(rows); err != nil {
		t.Fatal(err)
	}
	var out []api.Message
	for _, seq := range seqs {
		m, err := loadMessage(f.s.db, f.ctx, f.task.ID, seq)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

// foldCandidateLines checks the shape every notice shares and returns its
// candidate lines in order.
func foldCandidateLines(t *testing.T, f *convergenceFixture, review api.Message, notices []api.Message) []string {
	t.Helper()
	var lines []string
	for _, n := range notices {
		e := n.Envelope
		if e == nil || e.Kind != api.EnvelopeKindNotice || n.To != f.reviewer.ID || e.To != f.reviewer.Name || n.From.AgentID != "" || n.ReplyTo != 0 || len(e.Refs) != 1 || e.Refs["review"] != fmt.Sprint(review.Seq) {
			t.Fatalf("notice shape %+v %+v", n, e)
		}
		if len(n.WorkItems) != 1 || n.WorkItems[0].ItemID != f.item.ID || n.WorkItems[0].Relationship != "primary" || !reflect.DeepEqual(n.WorkOrderMessage, review.WorkOrderMessage) {
			t.Fatalf("notice links %+v %+v", n.WorkItems, n.WorkOrderMessage)
		}
		if body, err := json.Marshal(e.Body); err != nil || len(body) > api.MaxEnvelopeBodyBytes {
			t.Fatalf("notice body %d bytes %v", len(body), err)
		}
		parts := strings.Split(e.Body.Text, "\n")
		if parts[0] != foldCandidatesHeader || len(parts) < 2 {
			t.Fatalf("notice text %q", e.Body.Text)
		}
		lines = append(lines, parts[1:]...)
	}
	return lines
}

func foldCandidateLine(item api.WorkItem, path string) string {
	return "- " + item.ID + " names " + path + ": " + item.Title
}

func messageCount(t *testing.T, f *convergenceFixture) int {
	t.Helper()
	var n int
	if err := f.s.db.QueryRow(`SELECT count(*) FROM messages WHERE task_id=?`, f.task.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func foldSkipEvents(t *testing.T, f *convergenceFixture) []map[string]any {
	t.Helper()
	rows, err := f.s.db.Query(`SELECT data FROM events WHERE task_id=? AND kind='fold_candidates_skipped' ORDER BY seq`, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var data map[string]any
		if err := json.Unmarshal([]byte(raw), &data); err != nil {
			t.Fatal(err)
		}
		out = append(out, data)
	}
	if err := closeRows(rows); err != nil {
		t.Fatal(err)
	}
	return out
}

// heldFoldReview owns foldFile under an owner order, files the fold targets
// and posts a REVIEW.
func heldFoldReview(t *testing.T) (*convergenceFixture, map[string]api.WorkItem, api.Message) {
	t.Helper()
	f := newConvergenceFixture(t)
	targets := foldTargets(t, f.s, f.task)
	f.confirmOwned(t, f.stageHandler(t), f.ownerOrder(t, "fold-order"), "fold-scope", []string{foldFile})
	return f, targets, f.review(t, candidateA)
}

// c1, c5 (wi_a2f2865ae9b190ec): a REVIEW sends its reviewer one linked
// notice listing the held items that name an owned path, newest first, and
// none that is running, queued, accepted, ordered, done or names another file.
func TestReviewFoldCandidatesHeldVersusRunning(t *testing.T) {
	f, targets, r := heldFoldReview(t)
	notices := foldCandidateNotices(t, f, r.Seq)
	if len(notices) != 1 || notices[0].Seq != r.Seq+1 {
		t.Fatalf("want one notice right after the REVIEW, got %d", len(notices))
	}
	got := foldCandidateLines(t, f, r, notices)
	want := []string{foldCandidateLine(targets["second"], foldFile), foldCandidateLine(targets["eligible"], foldFile)}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("candidates\n got %q\nwant %q", got, want)
	}
	if events := foldSkipEvents(t, f); len(events) != 0 {
		t.Fatalf("skip events %v", events)
	}
}

// c1, c5: at most ten candidates, the newest.
func TestReviewFoldCandidatesCap(t *testing.T) {
	f := newConvergenceFixture(t)
	var items []api.WorkItem
	for i := 1; i <= 12; i++ {
		items = append(items, fileFoldItem(t, f, fmt.Sprint(i), fmt.Sprintf("Cap candidate %d", i), "Owned paths: "+foldFile+"."))
	}
	f.confirmOwned(t, f.stageHandler(t), f.ownerOrder(t, "fold-order"), "fold-scope", []string{foldFile})
	r := f.review(t, candidateA)
	var want []string
	for i := 11; i >= 2; i-- {
		want = append(want, foldCandidateLine(items[i], foldFile))
	}
	if got := foldCandidateLines(t, f, r, foldCandidateNotices(t, f, r.Seq)); !reflect.DeepEqual(got, want) {
		t.Fatalf("candidates\n got %q\nwant %q", got, want)
	}
}

// c1, c3 (wi_b16be34236f99e5c): an eligible item whose description names
// the reviewed item's ID and only a file outside the order is listed with
// that file, among the owned-path candidates newest first, also when an
// artifact path comes first (n1, wi_be87629a5af07575), and a fold of a
// finding on that file into it passes the unchanged fold rule. An item that
// names neither, the ID but no file, a longer ID, or the ID while ordered is
// not listed.
func TestReviewFoldCandidatesNamesReviewedItem(t *testing.T) {
	const outside = "docs/objective-verification.md"
	f := newConvergenceFixture(t)
	older := fileFoldItem(t, f, "older", "Owned path, older", "Owned paths: "+foldFile+".")
	named := fileFoldItem(t, f, "named", "Names the reviewed item", "Raised during "+f.item.ID+": see https://example.com/docs/a.md and /Users/x/notes.md, then "+outside+".")
	artifact := fileFoldItem(t, f, "artifact", "Names an artifact first", "Raised during "+f.item.ID+". Artifact: tailterm-artifacts/backlog-steward/fold-use-2026-10-21.md, then "+outside+".")
	fileFoldItem(t, f, "neither", "Names neither", "The gap is in "+outside+".")
	fileFoldItem(t, f, "no-file", "Names the ID but no file", "Raised during "+f.item.ID+", no file yet.")
	fileFoldItem(t, f, "longer", "Names a longer ID", "Raised during "+f.item.ID+"0 in "+outside+".")
	ordered := fileFoldItem(t, f, "ordered", "Names the ID, ordered", "Raised during "+f.item.ID+" in "+outside+".")
	contextLinkedMessage(t, f.s, f.task, ordered, "fold order", "fold-order-ordered", nil)
	newer := fileFoldItem(t, f, "newer", "Owned path, newer", "- "+foldFile+" and "+f.item.ID)
	f.confirmOwned(t, f.stageHandler(t), f.ownerOrder(t, "fold-order"), "fold-scope", []string{foldFile})
	r := f.review(t, candidateA)
	want := []string{foldCandidateLine(newer, foldFile), foldCandidateLine(artifact, outside), foldCandidateLine(named, outside), foldCandidateLine(older, foldFile)}
	if got := foldCandidateLines(t, f, r, foldCandidateNotices(t, f, r.Seq)); !reflect.DeepEqual(got, want) {
		t.Fatalf("candidates\n got %q\nwant %q", got, want)
	}
	finding := api.ReviewFinding{ID: "f1", Title: "Fold fixture finding", File: outside, Line: 3, Into: named.ID}
	if _, err := f.post(f.resultEnv(candidateA, passConvergence, api.ReviewMetadata{Mode: "general", Findings: []api.ReviewFinding{finding}}), "", r.Seq, f.reviewer); err != nil {
		t.Fatal("fold into the listed item:", err)
	}
	if notes := foldNotes(t, f.s, named.ID); len(notes) != 1 || notes[0].namespace != "review-fold" {
		t.Fatalf("notes %+v", notes)
	}
}

// c1: the reviewed ID counts only as a whole token, and the listed file is
// the first repository-relative path the description names that a fold
// accepts. n3 (wi_be87629a5af07575): an artifact path, a module path,
// "$HOME/..." and a version string are not repository files.
func TestReviewFoldCandidateNamedFile(t *testing.T) {
	const id = "wi_0123456789abcdef"
	for text, want := range map[string]bool{
		id:                           true,
		"(" + id + ").":              true,
		"tsk_0123456789abcdef/" + id: true,
		id + "0":                     false,
		"x" + id:                     false,
		"wi_0123456789abcde":         false,
	} {
		if got := namesItem(text, id); got != want {
			t.Errorf("namesItem(%q) = %v, want %v", text, got, want)
		}
	}
	for text, want := range map[string]string{
		"owns " + foldFile + ".":                          foldFile,
		"`" + foldFile + "`:12, then docs/x.md":           foldFile,
		"see https://example.com/a/b.md then docs/x.md":   "docs/x.md",
		"/Users/x/notes.md and docs/x.md":                 "docs/x.md",
		"hub/internal/store and Makefile then docs/x.md":  "docs/x.md",
		"a/../b.go, hub/.gitignore, hub/x. and docs/x.md": "docs/x.md",
		"no file here":        "",
		"hub/internal/store/": "",
		"Artifact: tailterm-artifacts/backlog-steward/fold-use-2026-10-21.md then hub/a.go": "hub/a.go",
		"github.com/scs32/tailterm/hub/x.go":                                                "",
		"$HOME/x.md and hub/e.go":                                                           "hub/e.go",
		"v1.2/3.4 then hub/c.go":                                                            "hub/c.go",
		"Artifact: tailterm-artifacts/backlog-steward/fold-use-2026-10-21.md.":              "",
	} {
		if got := firstNamedFile(text); got != want {
			t.Errorf("firstNamedFile(%q) = %q, want %q", text, got, want)
		}
	}
}

// c1, c5: no owned paths, or owned paths no eligible item names, send no
// notice and record nothing.
func TestReviewFoldCandidatesNone(t *testing.T) {
	for name, owned := range map[string][]string{
		"no ownership":           nil,
		"no item names the path": {"hub/internal/store/nothing.go"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newConvergenceFixture(t)
			foldTargets(t, f.s, f.task)
			f.confirmOwned(t, f.stageHandler(t), f.ownerOrder(t, "fold-order"), "fold-scope", owned)
			before := messageCount(t, f)
			r := f.review(t, candidateA)
			if after := messageCount(t, f); after != before+1 || len(foldCandidateNotices(t, f, r.Seq)) != 0 || len(foldSkipEvents(t, f)) != 0 {
				t.Fatalf("messages %d -> %d", before, after)
			}
		})
	}
}

// c2, c5: the reviewer folds into an offered candidate through the existing
// path, without acknowledging the notice; a fold into the running target is
// still refused with its reason.
func TestReviewFoldCandidatesFoldIntoOffered(t *testing.T) {
	f, targets, r := heldFoldReview(t)
	lines := foldCandidateLines(t, f, r, foldCandidateNotices(t, f, r.Seq))
	offered := strings.Fields(lines[0])[1]
	if offered != targets["second"].ID {
		t.Fatalf("offered %q", offered)
	}
	fold := func(into string) error {
		finding := api.ReviewFinding{ID: "f1", Title: "Fold fixture finding", File: foldFile, Line: 3, Into: into}
		_, err := f.post(f.resultEnv(candidateA, passConvergence, api.ReviewMetadata{Mode: "general", Findings: []api.ReviewFinding{finding}}), "", r.Seq, f.reviewer)
		return err
	}
	refusedWith(t, "fold into the running target", fold(targets["running"].ID), "review.findings[f1].into: item is running")
	if err := fold(offered); err != nil {
		t.Fatal("fold into an offered candidate:", err)
	}
	if notes := foldNotes(t, f.s, offered); len(notes) != 1 || notes[0].namespace != "review-fold" {
		t.Fatalf("notes %+v", notes)
	}
	if st := f.state(t); len(st.FollowUps) != 1 || st.FollowUps[0].ItemID != offered || !st.FollowUps[0].Folded {
		t.Fatalf("follow-ups %+v", st.FollowUps)
	}
}

// c5: a replayed REVIEW sends no second notice.
func TestReviewFoldCandidatesReplay(t *testing.T) {
	f := newConvergenceFixture(t)
	foldTargets(t, f.s, f.task)
	f.confirmOwned(t, f.stageHandler(t), f.ownerOrder(t, "fold-order"), "fold-scope", []string{foldFile})
	req := f.req(api.Envelope{Kind: "review", Subject: "Review frozen fixture candidate", Body: api.EnvelopeBody{Candidate: candidateA, Scope: "Fixture", Acceptance: f.criteria}}, f.reviewer.ID, 0, api.Agent{})
	r, err := f.s.PostMessage(f.ctx, f.task.ID, req, f.by)
	if err != nil {
		t.Fatal(err)
	}
	before := messageCount(t, f)
	again, err := f.s.PostMessage(f.ctx, f.task.ID, req, f.by)
	if err != nil || again.Seq != r.Seq {
		t.Fatalf("replay %d %v", again.Seq, err)
	}
	if after := messageCount(t, f); after != before || len(foldCandidateNotices(t, f, r.Seq)) != 1 {
		t.Fatalf("replay wrote a notice: %d -> %d", before, after)
	}
}

// c1, c5: ownership comes from the order the review stage is confirmed
// under, never from a later confirmation or queue entry under another order;
// a new owner-order stage takes its own order's paths.
func TestReviewFoldCandidatesTwoOrders(t *testing.T) {
	const pathA, pathB = "hub/internal/store/fold_a.go", "hub/internal/store/fold_b.go"
	f := newConvergenceFixture(t)
	a := fileFoldItem(t, f, "a", "Names path A", "Owned paths: "+pathA+".")
	b := fileFoldItem(t, f, "b", "Names path B", "Owned paths: "+pathB+".")
	handler := f.stageHandler(t)
	f.rescope(t, "Order one")
	one := f.ownerOrder(t, "order-one")
	f.confirmOwned(t, handler, one, "scope-one", []string{pathA})
	if err := f.assign(f.criteria, nil); err != nil {
		t.Fatal(err)
	}
	if st := f.state(t); st.Stages[len(st.Stages)-1].OrderSeq != one.Seq {
		t.Fatalf("stage order %+v", st.Stages)
	}
	two := f.ownerOrder(t, "order-two")
	f.confirmOwned(t, handler, two, "scope-two", []string{pathB})
	f.queueOwned(t, two, "queue-two", []string{pathB})
	r := f.review(t, candidateA)
	if got, want := foldCandidateLines(t, f, r, foldCandidateNotices(t, f, r.Seq)), []string{foldCandidateLine(a, pathA)}; !reflect.DeepEqual(got, want) {
		t.Fatalf("stage one candidates\n got %q\nwant %q", got, want)
	}
	if _, err := f.post(f.resultEnv(candidateA, passConvergence, api.ReviewMetadata{Mode: "general"}), "", r.Seq, f.reviewer); err != nil {
		t.Fatal(err)
	}
	// A new scope revision confirmed under a new owner order opens stage two.
	f.rescope(t, "Order three")
	three := f.ownerOrder(t, "order-three")
	f.confirmOwned(t, handler, three, "scope-three", []string{pathB})
	if err := f.assign(f.criteria, nil); err != nil {
		t.Fatal(err)
	}
	if st := f.state(t); len(st.Stages) < 2 || st.Stages[len(st.Stages)-1].OrderSeq != three.Seq {
		t.Fatalf("stage two %+v", st.Stages)
	}
	r = f.review(t, candidateB)
	if got, want := foldCandidateLines(t, f, r, foldCandidateNotices(t, f, r.Seq)), []string{foldCandidateLine(b, pathB)}; !reflect.DeepEqual(got, want) {
		t.Fatalf("stage two candidates\n got %q\nwant %q", got, want)
	}
}

// c1, c5: with no queue entry the stage order's confirmation gives the
// paths, and a queue entry under that order at an older item revision is
// never read.
func TestReviewFoldCandidatesScopeFallback(t *testing.T) {
	const pathA, pathB = "hub/internal/store/fold_a.go", "hub/internal/store/fold_b.go"
	t.Run("no queue entry", func(t *testing.T) {
		f := newConvergenceFixture(t)
		a := fileFoldItem(t, f, "a", "Names path A", "Owned paths: "+pathA+".")
		f.confirmOwned(t, f.stageHandler(t), f.ownerOrder(t, "order"), "scope", []string{pathA})
		r := f.review(t, candidateA)
		if got, want := foldCandidateLines(t, f, r, foldCandidateNotices(t, f, r.Seq)), []string{foldCandidateLine(a, pathA)}; !reflect.DeepEqual(got, want) {
			t.Fatalf("candidates\n got %q\nwant %q", got, want)
		}
	})
	t.Run("queue entry at an older item revision", func(t *testing.T) {
		f := newConvergenceFixture(t)
		a := fileFoldItem(t, f, "a", "Names path A", "Owned paths: "+pathA+".")
		fileFoldItem(t, f, "b", "Names path B", "Owned paths: "+pathB+".")
		order := f.ownerOrder(t, "order")
		f.confirmOwned(t, f.stageHandler(t), order, "scope", []string{pathA})
		f.queueOwned(t, order, "queue", []string{pathB})
		high := "high"
		var err error
		if f.item, err = f.s.UpdateWorkItem(f.ctx, f.task.ID, f.item.ID, api.UpdateWorkItemRequest{Revision: f.item.Revision, Priority: &high}, f.by); err != nil {
			t.Fatal(err)
		}
		var entryRevision int64
		if err = f.s.db.QueryRow(`SELECT item_revision FROM team_queue_entries WHERE item_id=?`, f.item.ID).Scan(&entryRevision); err != nil || entryRevision >= f.item.Revision {
			t.Fatalf("entry revision %d item revision %d %v", entryRevision, f.item.Revision, err)
		}
		r := f.review(t, candidateA)
		if got, want := foldCandidateLines(t, f, r, foldCandidateNotices(t, f, r.Seq)), []string{foldCandidateLine(a, pathA)}; !reflect.DeepEqual(got, want) {
			t.Fatalf("candidates\n got %q\nwant %q", got, want)
		}
	})
}

// c1, c5: with no confirmed order there are no owned paths: the REVIEW is
// accepted with no notice and no skip event.
func TestReviewFoldCandidatesMissingOrder(t *testing.T) {
	f := newConvergenceFixture(t)
	foldTargets(t, f.s, f.task)
	before := messageCount(t, f)
	r := f.review(t, candidateA)
	if after := messageCount(t, f); after != before+1 || len(foldCandidateNotices(t, f, r.Seq)) != 0 || len(foldSkipEvents(t, f)) != 0 {
		t.Fatalf("messages %d -> %d", before, after)
	}
	if st := f.state(t); len(st.Rounds) != 1 || st.Rounds[0].RequestSeq != r.Seq {
		t.Fatalf("rounds %+v", st.Rounds)
	}
}

// longTitleFoldReview files ten held items naming foldFile, each with a
// 120-character title of '<' (six bytes each in JSON), and posts a REVIEW.
func longTitleFoldReview(t *testing.T) (*convergenceFixture, []api.WorkItem, func() api.Message) {
	t.Helper()
	f := newConvergenceFixture(t)
	var items []api.WorkItem
	for i := 1; i <= 10; i++ {
		items = append(items, fileFoldItem(t, f, fmt.Sprint(i), strings.Repeat("<", 119)+fmt.Sprint(i%10), "Owned paths: "+foldFile+"."))
	}
	f.confirmOwned(t, f.stageHandler(t), f.ownerOrder(t, "fold-order"), "fold-scope", []string{foldFile})
	return f, items, func() api.Message { return f.review(t, candidateA) }
}

// c1, c5 (lead disposition of p2): lines are never cut; candidates that do
// not fit one notice are split across notices in order.
func TestReviewFoldCandidatesSplitAcrossNotices(t *testing.T) {
	f, items, review := longTitleFoldReview(t)
	r := review()
	notices := foldCandidateNotices(t, f, r.Seq)
	if len(notices) < 2 {
		t.Fatalf("want the candidates split across notices, got %d", len(notices))
	}
	var want []string
	for i := len(items) - 1; i >= 0; i-- {
		want = append(want, foldCandidateLine(items[i], foldFile))
	}
	got := foldCandidateLines(t, f, r, notices)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("candidates\n got %q\nwant %q", got, want)
	}
	for _, n := range notices {
		if strings.Contains(n.Envelope.Body.Text, "…") {
			t.Fatalf("notice cut a line: %q", n.Envelope.Body.Text)
		}
	}
	if events := foldSkipEvents(t, f); len(events) != 0 {
		t.Fatalf("skip events %v", events)
	}
}

// c1, c5 (lead disposition of p2): a candidate whose line cannot fit a
// notice even alone is left out and recorded, never cut; others are listed.
func TestReviewFoldCandidatesOversizedLineOmitted(t *testing.T) {
	segment := strings.Repeat("&", 200)
	big := strings.Join([]string{segment, segment, segment, segment, segment}, "/")
	if len(big) != 1004 {
		t.Fatal(len(big))
	}
	f := newConvergenceFixture(t)
	first := fileFoldItem(t, f, "big-1", "Names the big path", "Owned paths: "+big+".")
	normal := fileFoldItem(t, f, "normal", "Names the fold file", "Owned paths: "+foldFile+".")
	second := fileFoldItem(t, f, "big-2", "Also names the big path", "Owned paths: "+big+".")
	f.confirmOwned(t, f.stageHandler(t), f.ownerOrder(t, "fold-order"), "fold-scope", []string{big, foldFile})
	r := f.review(t, candidateA)
	if got, want := foldCandidateLines(t, f, r, foldCandidateNotices(t, f, r.Seq)), []string{foldCandidateLine(normal, foldFile)}; !reflect.DeepEqual(got, want) {
		t.Fatalf("candidates\n got %q\nwant %q", got, want)
	}
	events := foldSkipEvents(t, f)
	if len(events) != 1 || events[0]["reason"] != "candidate too large to list" || events[0]["itemId"] != f.item.ID || events[0]["messageSeq"] != float64(r.Seq) || !reflect.DeepEqual(events[0]["omitted"], []any{second.ID, first.ID}) {
		t.Fatalf("skip events %v", events)
	}
}

// c1, c5 (r2 p2): a failure while posting the notices undoes every notice,
// keeps the REVIEW and its round, and records the reason.
func TestReviewFoldCandidatesFailureKeepsReview(t *testing.T) {
	f, _, review := longTitleFoldReview(t)
	posts := 0
	foldCandidatePost = func(s *Store, ctx context.Context, tx *sql.Tx, task api.Task, to api.Agent, subject, text string, refs map[string]string, source api.Message, requestID string) error {
		posts++
		if posts == 2 {
			return errors.New("forced fold notice failure")
		}
		return s.postLinkedBrokerNotice(ctx, tx, task, to, subject, text, refs, source, requestID)
	}
	t.Cleanup(func() { foldCandidatePost = (*Store).postLinkedBrokerNotice })
	before := messageCount(t, f)
	r := review()
	if posts != 2 {
		t.Fatalf("posts %d", posts)
	}
	if after := messageCount(t, f); after != before+1 || len(foldCandidateNotices(t, f, r.Seq)) != 0 {
		t.Fatalf("messages %d -> %d", before, after)
	}
	if st := f.state(t); len(st.Rounds) != 1 || st.Rounds[0].RequestSeq != r.Seq {
		t.Fatalf("rounds %+v", st.Rounds)
	}
	events := foldSkipEvents(t, f)
	if len(events) != 1 || events[0]["reason"] != "forced fold notice failure" || events[0]["messageSeq"] != float64(r.Seq) {
		t.Fatalf("skip events %v", events)
	}
}

// withdrawLeadFixture is a convergence fixture whose REVIEWs come from an
// agent lead, which tt withdraw needs: only the sending run may withdraw.
func withdrawLeadFixture(t *testing.T) *convergenceFixture {
	t.Helper()
	f := newConvergenceFixture(t)
	var err error
	f.lead, err = f.s.AddAgent(f.ctx, f.task.ID, api.AddAgentRequest{Name: "lead", Host: "fixture", Session: "lead"}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	name := f.lead.Name
	if _, err = f.s.UpdateTask(f.ctx, f.task.ID, api.UpdateTaskRequest{Orchestrator: &name}, f.by); err != nil {
		t.Fatal(err)
	}
	return f
}
func (f *convergenceFixture) leadReview(candidate string) (api.Message, error) {
	return f.post(api.Envelope{Kind: "review", Subject: "Review frozen fixture candidate", Body: api.EnvelopeBody{Candidate: candidate, Scope: "Fixture", Acceptance: f.criteria}}, f.reviewer.ID, 0, f.lead)
}
func (f *convergenceFixture) obligationFor(t *testing.T, seq int64) string {
	t.Helper()
	obligations, err := f.s.ListObligations(f.ctx, f.task.ID, ObligationFilter{}, f.s.now())
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range obligations {
		if o.MessageSeq == seq {
			return o.ID
		}
	}
	t.Fatal("no obligation for message", seq)
	return ""
}
func (f *convergenceFixture) withdraw(t *testing.T, seq int64) error {
	t.Helper()
	_, err := f.s.WithdrawObligation(f.ctx, f.task.ID, f.obligationFor(t, seq), api.ObligationWithdrawRequest{AgentID: f.lead.ID, RunID: f.lead.RunID, Reason: "Candidate rebased"})
	return err
}

// wi_042fe7cb76b34bf0: a withdrawn REVIEW with no RESULT closes its round
// without a lifetime count; a RESULT on it is refused.
func TestReviewConvergenceWithdrawnReviewLeavesNoCount(t *testing.T) {
	f := withdrawLeadFixture(t)
	first, err := f.leadReview(candidateA)
	if err != nil {
		t.Fatal(err)
	}
	// Not withdrawn: an unanswered round still refuses a new REVIEW.
	if _, err = f.leadReview(candidateB); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "previous general review is incomplete") {
		t.Fatal("unanswered round accepted a new review", err)
	}
	if err = f.withdraw(t, first.Seq); err != nil {
		t.Fatal(err)
	}
	// The withdrawn request takes no RESULT, before or after the fresh REVIEW.
	if _, err = f.post(f.resultEnv(candidateA, passConvergence, api.ReviewMetadata{Mode: "general"}), "", first.Seq, f.reviewer); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "withdrawn") {
		t.Fatal("result on withdrawn review accepted", err)
	}
	fresh, err := f.leadReview(candidateB)
	if err != nil {
		t.Fatal("fresh review after withdrawal refused", err)
	}
	st := f.state(t)
	if len(st.Rounds) != 1 || st.Rounds[0].Number != 1 || st.Rounds[0].RequestSeq != fresh.Seq || st.Rounds[0].Candidate != candidateB {
		t.Fatal("fresh review did not take the withdrawn round's number", st.Rounds)
	}
	if _, err = f.post(f.resultEnv(candidateA, passConvergence, api.ReviewMetadata{Mode: "general"}), "", first.Seq, f.reviewer); !errors.Is(err, api.ErrConflict) {
		t.Fatal("late result on withdrawn review accepted", err)
	}
	if _, err = f.post(f.resultEnv(candidateB, passConvergence, api.ReviewMetadata{Mode: "general"}), "", fresh.Seq, f.reviewer); err != nil {
		t.Fatal(err)
	}
	// Round two withdrawn as well: it leaves no count either.
	second, err := f.leadReview(candidateC)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.withdraw(t, second.Seq); err != nil {
		t.Fatal(err)
	}
	again, err := f.leadReview(candidateC)
	if err != nil {
		t.Fatal("fresh round two after withdrawal refused", err)
	}
	st = f.state(t)
	if len(st.Rounds) != 2 || st.Rounds[0].ResultSeq == 0 || st.Rounds[1].Number != 2 || st.Rounds[1].RequestSeq != again.Seq {
		t.Fatal(st.Rounds)
	}
	if _, err = f.post(f.resultEnv(candidateC, passConvergence, api.ReviewMetadata{Mode: "general"}), "", again.Seq, f.reviewer); err != nil {
		t.Fatal(err)
	}
	f.thirdReviewRefused(t, candidateC)
}

func TestReviewConvergenceWithdrawAfterResultChangesNothing(t *testing.T) {
	f := withdrawLeadFixture(t)
	for _, candidate := range []string{candidateA, candidateB} {
		r, err := f.leadReview(candidate)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = f.post(f.resultEnv(candidate, passConvergence, api.ReviewMetadata{Mode: "general"}), "", r.Seq, f.reviewer); err != nil {
			t.Fatal(err)
		}
		before := f.state(t)
		// The RESULT closed the obligation, so withdrawal is refused and the
		// answered round stays as it was.
		if err = f.withdraw(t, r.Seq); !errors.Is(err, api.ErrConflict) {
			t.Fatal("withdraw after result", err)
		}
		if after := f.state(t); !reflect.DeepEqual(before.Rounds, after.Rounds) {
			t.Fatal("answered round changed", before.Rounds, after.Rounds)
		}
	}
	f.thirdReviewRefused(t, candidateC)
}

// A reassigned review is judged by its active request's obligation.
func TestReviewConvergenceWithdrawnReassignedReview(t *testing.T) {
	f := withdrawLeadFixture(t)
	r, err := f.leadReview(candidateA)
	if err != nil {
		t.Fatal(err)
	}
	replacement, err := f.s.AddAgent(f.ctx, f.task.ID, api.AddAgentRequest{Name: "reviewer-next", Host: "fixture", Session: "reviewer-next"}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	moved, err := f.s.ReassignObligation(f.ctx, f.task.ID, f.obligationFor(t, r.Seq), api.ObligationReassignRequest{ToAgentID: replacement.ID, Reason: "Exact fixture replacement"}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.withdraw(t, moved.Seq); err != nil {
		t.Fatal(err)
	}
	if _, err = f.post(f.resultEnv(candidateA, passConvergence, api.ReviewMetadata{Mode: "general"}), "", moved.Seq, replacement); !errors.Is(err, api.ErrConflict) {
		t.Fatal("result on withdrawn reassigned review accepted", err)
	}
	if _, err = f.leadReview(candidateB); err != nil {
		t.Fatal("fresh review after withdrawn reassignment refused", err)
	}
	if st := f.state(t); len(st.Rounds) != 1 || st.Rounds[0].Candidate != candidateB {
		t.Fatal(st.Rounds)
	}
}

// wi_9948c5e178a4ca71: the status read drops a withdrawn, unanswered last
// round at once, as the next transition would, while an answered round and an
// unwithdrawn one stay listed.
func TestReviewConvergenceStatusOmitsWithdrawnRoundAtOnce(t *testing.T) {
	f := withdrawLeadFixture(t)
	answered, err := f.leadReview(candidateA)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.post(f.resultEnv(candidateA, passConvergence, api.ReviewMetadata{Mode: "general"}), "", answered.Seq, f.reviewer); err != nil {
		t.Fatal(err)
	}
	// Withdrawal after the RESULT is refused and changes nothing.
	if err = f.withdraw(t, answered.Seq); !errors.Is(err, api.ErrConflict) {
		t.Fatal("withdraw after result", err)
	}
	open, err := f.leadReview(candidateB)
	if err != nil {
		t.Fatal(err)
	}
	before := f.state(t)
	if len(before.Rounds) != 2 || before.Rounds[1].RequestSeq != open.Seq || before.Rounds[1].ResultSeq != 0 {
		t.Fatal("unwithdrawn unanswered round not listed", before.Rounds)
	}
	if err = f.withdraw(t, open.Seq); err != nil {
		t.Fatal(err)
	}
	st := f.state(t)
	if len(st.Rounds) != 1 || st.Rounds[0].RequestSeq != answered.Seq || st.Rounds[0].ResultSeq == 0 {
		t.Fatal("status still lists the withdrawn round", st.Rounds)
	}
	// The read is read-only: stored state keeps the round until a transition.
	var raw string
	if err = f.s.db.QueryRowContext(f.ctx, `SELECT state_json FROM review_convergence WHERE task_id=? AND item_id=?`, f.task.ID, f.item.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(raw, fmt.Sprintf(`"requestSeq":%d`, open.Seq)) {
		t.Fatal("status read rewrote stored state", raw)
	}
	// The read matches enforcement: the fresh REVIEW takes the dropped number.
	fresh, err := f.leadReview(candidateC)
	if err != nil {
		t.Fatal(err)
	}
	if st = f.state(t); len(st.Rounds) != 2 || st.Rounds[1].Number != 2 || st.Rounds[1].RequestSeq != fresh.Seq {
		t.Fatal(st.Rounds)
	}
}

// Only the current stage's last round leaves the read: a withdrawn round of an
// earlier stage stays listed, as the transition path leaves it stored.
func TestReviewConvergenceStatusKeepsEarlierStageWithdrawnRound(t *testing.T) {
	f := withdrawLeadFixture(t)
	handler := f.stageHandler(t)
	f.confirmScope(t, handler, f.ownerOrder(t, "findings-order"), "findings-scope")
	earlier, err := f.leadReview(candidateA)
	if err != nil {
		t.Fatal(err)
	}
	f.rescope(t, "Build order one")
	f.confirmScope(t, handler, f.ownerOrder(t, "build-order"), "build-scope")
	if err = f.assign(f.criteria, nil); err != nil {
		t.Fatal("build ASSIGN", err)
	}
	if err = f.withdraw(t, earlier.Seq); err != nil {
		t.Fatal(err)
	}
	st := f.state(t)
	if len(st.Stages) != 2 || len(st.Rounds) != 1 || st.Rounds[0].RequestSeq != earlier.Seq {
		t.Fatal("earlier stage's withdrawn round left the read", st.Stages, st.Rounds)
	}
	current, err := f.leadReview(candidateB)
	if err != nil {
		t.Fatal(err)
	}
	if st = f.state(t); len(st.Rounds) != 2 || st.Rounds[0].RequestSeq != earlier.Seq || st.Rounds[1].RequestSeq != current.Seq || st.Rounds[1].Number != 1 {
		t.Fatal(st.Rounds)
	}
	if err = f.withdraw(t, current.Seq); err != nil {
		t.Fatal(err)
	}
	if st = f.state(t); len(st.Rounds) != 1 || st.Rounds[0].RequestSeq != earlier.Seq {
		t.Fatal("current stage's withdrawn round still listed", st.Rounds)
	}
}

// wi_18fec625e6133919: the latest ASSIGN with the same criteria becomes the
// scope's saved assignment, whether the lead withdraws the earlier one before
// or after posting it, so the plan binds it and the digest of its Owns. The
// earlier entry stays before it as history; criteria still freeze.
func TestReviewConvergenceAmendedAssignmentRebindsScope(t *testing.T) {
	for _, withdrawFirst := range []bool{true, false} {
		t.Run(map[bool]string{true: "withdraw-then-post", false: "post-then-withdraw"}[withdrawFirst], func(t *testing.T) {
			f, h, p := verificationFixture(t, verificationOwnershipFixture{map[string]string{"a1": "code", "a2": "matrix"}, []string{"a2"}})
			f.rescope(t, "Amend the owned files")
			order := f.ownerOrder(t, "amend-order")
			f.confirmScope(t, h, order, "amend-scope")
			assign := func(owns []string, criteria map[string]string) (api.Message, error) {
				return f.post(api.Envelope{Kind: "assign", Subject: "Implement the amended fixture scope", Body: api.EnvelopeBody{Objective: "Fixture", Owns: owns, Acceptance: criteria, VerificationCriteria: f.verificationCriteria}}, f.builder.ID, 0, f.lead)
			}
			first, err := assign([]string{"fixture"}, f.criteria)
			if err != nil {
				t.Fatal(err)
			}
			differ := map[string]string{"a1": "code", "a2": "matrix", "a3": "extra"}
			before := f.state(t)
			refusedWith(t, "changed criteria", func() error { _, err := assign([]string{"fixture"}, differ); return err }(), "criteria frozen")
			if withdrawFirst {
				if err = f.withdraw(t, first.Seq); err != nil {
					t.Fatal(err)
				}
				refusedWith(t, "changed criteria after withdrawal", func() error { _, err := assign([]string{"fixture", "plan.mjs"}, differ); return err }(), "criteria frozen")
			}
			if st := f.state(t); !reflect.DeepEqual(st.Scopes, before.Scopes) {
				t.Fatal("refused ASSIGN changed scopes", st.Scopes)
			}
			amended, err := assign([]string{"fixture", "plan.mjs"}, f.criteria)
			if err != nil {
				t.Fatal(err)
			}
			if !withdrawFirst {
				if err = f.withdraw(t, first.Seq); err != nil {
					t.Fatal(err)
				}
			}
			st := f.state(t)
			var history []int64
			for _, sc := range st.Scopes {
				if sc.ScopeRevision == f.item.ScopeRevision {
					history = append(history, sc.AssignmentSeq)
				}
			}
			if !reflect.DeepEqual(history, []int64{first.Seq, amended.Seq}) || scopeFor(&st, f.item.ScopeRevision).AssignmentSeq != amended.Seq {
				t.Fatal("amended assignment not saved after the first one", st.Scopes)
			}
			if !reflect.DeepEqual(st.Stages, before.Stages) {
				t.Fatal("amendment moved a stage", before.Stages, st.Stages)
			}
			plan := func(key string, assignment int64, owns []string) error {
				q := p
				q.OperationKey, q.ItemRevision, q.ScopeRevision, q.OrderMessageSeq = key, f.item.Revision, f.item.ScopeRevision, order.Seq
				q.AssignmentSeq, q.AssignmentOwnershipDigest, q.Owned = assignment, verificationDigest(owns), []string{"fixture", "plan.mjs"}
				_, err := f.s.SaveVerification(f.ctx, f.task.ID, f.item.ID, api.VerificationRequest{RequestID: key, AgentID: h.ID, RunID: h.RunID, Plan: &q})
				return err
			}
			refusedWith(t, "withdrawn assignment plan", plan("withdrawn-plan", first.Seq, []string{"fixture"}), "saved exact assignment required")
			refusedWith(t, "withdrawn ownership digest", plan("stale-digest-plan", amended.Seq, []string{"fixture"}), "ownership and builder must match assignment")
			if err = plan("amended-plan", amended.Seq, []string{"fixture", "plan.mjs"}); err != nil {
				t.Fatal("amended assignment plan refused", err)
			}
		})
	}
}

// roundOneFollowUps runs round one on candidateA with blocker b1 (a1 failed)
// and findings f1 and f2 (f2 cites a2, also failed), then opens round two on
// candidateB. It returns the round-one RESULT, the round-two REVIEW and the
// stubs the hub filed, by finding ID.
func roundOneFollowUps(t *testing.T, f *convergenceFixture) (api.Message, api.Message, map[string]api.WorkItem) {
	t.Helper()
	r1 := f.review(t, candidateA)
	meta := api.ReviewMetadata{Mode: "general",
		Blockers: []api.ReviewFinding{{ID: "b1", Criterion: "a1", Title: "Fixture retry loses state", Command: "fixture retry", Output: "FAIL lost state"}},
		Findings: []api.ReviewFinding{{ID: "f1", Title: "Explain the fixture retry", Kind: "feature", File: "fixture.go", Line: 3}, {ID: "f2", Criterion: "a2", Title: "Fixture retries skip the receipt", File: "fixture.go", Line: 5}}}
	m, err := f.post(f.resultEnv(candidateA, map[string]string{"a1": "fail", "a2": "fail"}, meta), "", r1.Seq, f.reviewer)
	if err != nil {
		t.Fatal(err)
	}
	stubs := map[string]api.WorkItem{}
	for _, follow := range f.state(t).FollowUps {
		item, err := f.s.GetWorkItem(f.ctx, f.task.ID, follow.ItemID)
		if err != nil {
			t.Fatal(err)
		}
		stubs[follow.Finding.ID] = item
	}
	if len(stubs) != 2 || !heldReviewFollowUp(stubs["f1"]) || !heldReviewFollowUp(stubs["f2"]) {
		t.Fatalf("stubs %+v", stubs)
	}
	return m, f.review(t, candidateB), stubs
}

func (f *convergenceFixture) roundTwoResult(status map[string]string, meta api.ReviewMetadata, reply int64) error {
	meta.Mode = "general"
	_, err := f.post(f.resultEnv(candidateB, status, meta), "", reply, f.reviewer)
	return err
}

// fixedLine is the line settleFixedFollowUp appends or saves as a note.
func fixedLine(parent api.WorkItem, candidate, how string, result, review int64, finding string) string {
	return fmt.Sprintf("Delivered by %s at %s: %s #%d recorded finding %s of review #%d fixed.", parent.ID, candidate, how, result, finding, review)
}

func latestResultSeq(t *testing.T, f *convergenceFixture) int64 {
	t.Helper()
	var seq int64
	if err := f.s.db.QueryRow(`SELECT max(seq) FROM messages WHERE task_id=?`, f.task.ID).Scan(&seq); err != nil {
		t.Fatal(err)
	}
	return seq
}

// a3: a round-two RESULT whose blockerIds names round-one follow-ups closes
// their unchanged stubs in the same transaction; only blocker IDs enter the
// focused record; each refusal leaves the stubs held.
func TestReviewFollowUpClosedByRoundTwo(t *testing.T) {
	f := newConvergenceFixture(t)
	r1, r2, stubs := roundOneFollowUps(t, f)
	held := func() {
		t.Helper()
		for id, stub := range stubs {
			got, err := f.s.GetWorkItem(f.ctx, f.task.ID, stub.ID)
			if err != nil || got.Revision != 1 || got.Status != "open" || got.Description != stub.Description {
				t.Fatalf("stub %s changed: %+v %v", id, got, err)
			}
		}
	}
	refusals := []struct {
		name   string
		status map[string]string
		meta   api.ReviewMetadata
		reason string
	}{
		{"unknown ID", passConvergence, api.ReviewMetadata{BlockerIDs: []string{"b1", "f9"}}, "resolved IDs must name distinct prior blockers or round-one follow-ups"},
		{"named twice", passConvergence, api.ReviewMetadata{BlockerIDs: []string{"b1", "f1", "f1"}}, "resolved IDs must name distinct prior blockers or round-one follow-ups"},
		{"criterion not passing", map[string]string{"a1": "pass", "a2": "fail"}, api.ReviewMetadata{BlockerIDs: []string{"b1", "f2"}}, "resolved follow-up f2 criterion must pass"},
		{"also retained as a blocker", passConvergence, api.ReviewMetadata{BlockerIDs: []string{"b1", "f1"}, Blockers: []api.ReviewFinding{{ID: "f1", Regression: true, Baseline: candidateA, Candidate: candidateB, Title: "Fixture regression", File: "fixture.go", Line: 8}}}, "resolved IDs must name distinct prior blockers or round-one follow-ups"},
		{"also retained as a finding", passConvergence, api.ReviewMetadata{BlockerIDs: []string{"b1", "f1"}, Findings: []api.ReviewFinding{{ID: "f1", Title: "Explain the fixture retry", Kind: "feature", File: "fixture.go", Line: 3}}}, "resolved IDs must name distinct prior blockers or round-one follow-ups"},
	}
	for _, tc := range refusals {
		refusedWith(t, tc.name, f.roundTwoResult(tc.status, tc.meta, r2.Seq), tc.reason)
		held()
	}
	if err := f.roundTwoResult(passConvergence, api.ReviewMetadata{BlockerIDs: []string{"b1", "f1", "f2"}, Fix: "fixed the retry"}, r2.Seq); err != nil {
		t.Fatal(err)
	}
	result := latestResultSeq(t, f)
	for _, id := range []string{"f1", "f2"} {
		got, err := f.s.GetWorkItem(f.ctx, f.task.ID, stubs[id].ID)
		want := stubs[id].Description + "\n\n" + fixedLine(f.item, candidateB, "round-two review", result, r1.Seq, id)
		if err != nil || got.Status != "dismissed" || got.Revision != 2 || got.Description != want || itemRevisionCount(t, f.s, got.ID) != 2 {
			t.Fatalf("stub %s not closed: %+v %v", id, got, err)
		}
	}
	var closed int
	if err := f.s.db.QueryRow(`SELECT count(*) FROM events WHERE task_id=? AND kind='review_follow_up_closed'`, f.task.ID).Scan(&closed); err != nil || closed != 2 {
		t.Fatalf("closed events %d %v", closed, err)
	}
	st := f.state(t)
	if len(st.Focused) != 1 || !reflect.DeepEqual(st.Focused[0].BlockerIDs, []string{"b1"}) || st.Focused[0].ResultSeq != result {
		t.Fatalf("focused record holds finding IDs: %+v", st.Focused)
	}
	if len(outstanding(st)) != 0 {
		t.Fatalf("outstanding %+v", outstanding(st))
	}
	tri, err := f.s.WorkItemTriage(f.ctx, f.task.ID, 0, "", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range tri.HeldForTriage {
		if h.Item.ID == stubs["f1"].ID || h.Item.ID == stubs["f2"].ID {
			t.Fatalf("closed stub still held: %+v", h)
		}
	}
	acceptCorrectionFixture(t, f, candidateB)
}

// a3: round two names only follow-ups: no focused record is appended.
func TestReviewFollowUpClosedByRoundTwoWithoutBlockers(t *testing.T) {
	f := newConvergenceFixture(t)
	r1 := f.review(t, candidateA)
	finding := api.ReviewFinding{ID: "f1", Title: "Explain the fixture retry", Kind: "feature", File: "fixture.go", Line: 3}
	if _, err := f.post(f.resultEnv(candidateA, passConvergence, api.ReviewMetadata{Mode: "general", Findings: []api.ReviewFinding{finding}}), "", r1.Seq, f.reviewer); err != nil {
		t.Fatal(err)
	}
	stub := f.state(t).FollowUps[0].ItemID
	r2 := f.review(t, candidateB)
	if err := f.roundTwoResult(passConvergence, api.ReviewMetadata{BlockerIDs: []string{"f1"}}, r2.Seq); err != nil {
		t.Fatal(err)
	}
	if got, err := f.s.GetWorkItem(f.ctx, f.task.ID, stub); err != nil || got.Status != "dismissed" {
		t.Fatalf("stub %+v %v", got, err)
	}
	if st := f.state(t); len(st.Focused) != 0 {
		t.Fatalf("focused %+v", st.Focused)
	}
}

// a3: round two of a later stage cannot name an earlier stage's follow-up.
func TestReviewFollowUpEarlierStageRefused(t *testing.T) {
	f, handler := findingsStage(t)
	stub := f.state(t).FollowUps[0]
	f.rescope(t, "Build order one")
	f.confirmScope(t, handler, f.ownerOrder(t, "build-order"), "build-scope")
	if err := f.assign(f.criteria, nil); err != nil {
		t.Fatal(err)
	}
	f.generalRound(t, candidateB, passConvergence, api.ReviewMetadata{})
	r2 := f.review(t, candidateC)
	_, err := f.post(f.resultEnv(candidateC, passConvergence, api.ReviewMetadata{Mode: "general", BlockerIDs: []string{"f1"}}), "", r2.Seq, f.reviewer)
	refusedWith(t, "earlier-stage follow-up", err, "resolved IDs must name distinct prior blockers or round-one follow-ups")
	if got, err := f.s.GetWorkItem(f.ctx, f.task.ID, stub.ItemID); err != nil || got.Revision != 1 || got.Status != "open" {
		t.Fatalf("earlier-stage stub changed: %+v %v", got, err)
	}
}

// a4: a refined follow-up is never closed. An open, unordered one gets one
// review-fixed evidence note; a queued one gets no note and no row change.
func TestReviewFollowUpRefinedGetsNote(t *testing.T) {
	f := newConvergenceFixture(t)
	r1, r2, stubs := roundOneFollowUps(t, f)
	refine := func(id, text string) api.WorkItem {
		t.Helper()
		item, err := f.s.UpdateWorkItem(f.ctx, f.task.ID, stubs[id].ID, api.UpdateWorkItemRequest{Revision: 1, Description: &text}, f.by)
		if err != nil {
			t.Fatal(err)
		}
		return item
	}
	open := refine("f1", "Refined by the handler: explain the fixture retry in fixture.go.")
	queued := refine("f2", "Refined by the handler: keep the receipt on fixture retries.")
	order := contextLinkedMessage(t, f.s, f.task, queued, "fixed follow-up order", "fixed-order", nil)
	if _, err := f.s.TeamQueueAction(f.ctx, f.task.ID, api.TeamQueueRequest{RequestID: "fixed-queue", Operation: "add", ItemID: queued.ID, OrderMessageSeq: order.Seq, Host: "mini", Cwd: "/fixed"}); err != nil {
		t.Fatal(err)
	}
	queued, _ = f.s.GetWorkItem(f.ctx, f.task.ID, queued.ID)
	queuedRevisions := itemRevisionCount(t, f.s, queued.ID)
	if err := f.roundTwoResult(passConvergence, api.ReviewMetadata{BlockerIDs: []string{"b1", "f1", "f2"}}, r2.Seq); err != nil {
		t.Fatal(err)
	}
	result := latestResultSeq(t, f)
	got, err := f.s.GetWorkItem(f.ctx, f.task.ID, open.ID)
	if err != nil || got.Status != "open" || got.Revision != open.Revision || got.Description != open.Description || !got.UpdatedAt.Equal(open.UpdatedAt) {
		t.Fatalf("refined item changed: %+v %v", got, err)
	}
	notes := foldNotes(t, f.s, open.ID)
	if len(notes) != 1 || notes[0].namespace != "review-fixed" || notes[0].kind != "note" || notes[0].content != fixedLine(f.item, candidateB, "round-two review", result, r1.Seq, "f1") || !strings.Contains(notes[0].author, f.reviewer.ID) {
		t.Fatalf("notes %+v", notes)
	}
	got, err = f.s.GetWorkItem(f.ctx, f.task.ID, queued.ID)
	if err != nil || got.Status != "open" || got.Revision != queued.Revision || !got.UpdatedAt.Equal(queued.UpdatedAt) || itemRevisionCount(t, f.s, queued.ID) != queuedRevisions || len(foldNotes(t, f.s, queued.ID)) != 0 {
		t.Fatalf("queued refined item written: %+v %v", got, err)
	}
}

// a5: a passing focused RESULT naming a demoted round-two blocker closes its
// stub; a failing one writes nothing.
func TestReviewFollowUpClosedByFocusedPass(t *testing.T) {
	f := newConvergenceFixture(t)
	f.generalRound(t, candidateA, passConvergence, api.ReviewMetadata{})
	r2 := f.review(t, candidateB)
	b := api.ReviewFinding{ID: "b2", Criterion: "a2", Title: "Retry drops receipt", Command: "fixture retry", Output: "FAIL missing receipt"}
	m2, err := f.post(f.resultEnv(candidateB, map[string]string{"a1": "pass", "a2": "fail"}, api.ReviewMetadata{Mode: "general", Blockers: []api.ReviewFinding{b}}), "", r2.Seq, f.reviewer)
	if err != nil {
		t.Fatal(err)
	}
	stub, err := f.s.GetWorkItem(f.ctx, f.task.ID, f.state(t).FollowUps[0].ItemID)
	if err != nil || !heldReviewFollowUp(stub) {
		t.Fatalf("demoted stub %+v %v", stub, err)
	}
	focus := api.ReviewMetadata{Mode: "focused", Candidate: candidateC, Fix: "Restore receipt on retry", BlockerIDs: []string{"b2"}}
	request := func() api.Message {
		t.Helper()
		m, err := f.post(api.Envelope{Kind: "request", Subject: "Verify exactly the demoted receipt failure", Review: &focus, Body: api.EnvelopeBody{Ask: "Verify exact receipt fix"}}, f.reviewer.ID, 0, api.Agent{})
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	if _, err = f.post(f.resultEnv(candidateC, map[string]string{"b2": "fail"}, focus), "", request().Seq, f.reviewer); err != nil {
		t.Fatal(err)
	}
	if got, err := f.s.GetWorkItem(f.ctx, f.task.ID, stub.ID); err != nil || got.Revision != 1 || got.Status != "open" || itemRevisionCount(t, f.s, stub.ID) != 1 || len(foldNotes(t, f.s, stub.ID)) != 0 {
		t.Fatalf("failing focused result wrote the stub: %+v %v", got, err)
	}
	pass, err := f.post(f.resultEnv(candidateC, map[string]string{"b2": "pass"}, focus), "", request().Seq, f.reviewer)
	if err != nil {
		t.Fatal(err)
	}
	got, err := f.s.GetWorkItem(f.ctx, f.task.ID, stub.ID)
	if err != nil || got.Status != "dismissed" || got.Revision != 2 || got.Description != stub.Description+"\n\n"+fixedLine(f.item, candidateC, "focused verification", pass.Seq, m2.Seq, "b2") {
		t.Fatalf("passing focused result did not close the stub: %+v %v", got, err)
	}
	acceptCorrectionFixture(t, f, candidateC)
}

// a7: settling is atomic with the RESULT. A RESULT refused after one stub was
// closed leaves every stub held, and a replay of the saved RESULT settles
// nothing twice.
func TestReviewFollowUpCloseRollback(t *testing.T) {
	f := newConvergenceFixture(t)
	_, r2, stubs := roundOneFollowUps(t, f)
	text := "Refined by the handler: keep the receipt on fixture retries."
	refined, err := f.s.UpdateWorkItem(f.ctx, f.task.ID, stubs["f2"].ID, api.UpdateWorkItemRequest{Revision: 1, Description: &text}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	// f1's stub closes first; f2's note then fails, refusing the whole RESULT.
	if _, err = f.s.db.Exec(`CREATE TRIGGER refuse_fixed_note BEFORE INSERT ON narrative_artifact_versions BEGIN SELECT RAISE(ABORT,'forced note failure'); END`); err != nil {
		t.Fatal(err)
	}
	counts := foldCounts(t, f.s)
	if err = f.roundTwoResult(passConvergence, api.ReviewMetadata{BlockerIDs: []string{"b1", "f1", "f2"}}, r2.Seq); err == nil || !strings.Contains(err.Error(), "forced note failure") {
		t.Fatalf("want forced failure, got %v", err)
	}
	if got, err := f.s.GetWorkItem(f.ctx, f.task.ID, stubs["f1"].ID); err != nil || got.Revision != 1 || got.Status != "open" || got.Description != stubs["f1"].Description {
		t.Fatalf("refused RESULT closed a stub: %+v %v", got, err)
	}
	if after := foldCounts(t, f.s); after != counts {
		t.Fatalf("refused RESULT wrote rows: %v -> %v", counts, after)
	}
	if st := f.state(t); st.Rounds[1].ResultSeq != 0 || len(st.Focused) != 0 {
		t.Fatalf("refused RESULT saved the round: %+v", st)
	}
	if _, err = f.s.db.Exec(`DROP TRIGGER refuse_fixed_note`); err != nil {
		t.Fatal(err)
	}
	meta := api.ReviewMetadata{Mode: "general", BlockerIDs: []string{"b1", "f1", "f2"}}
	req := f.req(f.resultEnv(candidateB, passConvergence, meta), "", r2.Seq, f.reviewer)
	m, err := f.s.PostMessage(f.ctx, f.task.ID, req, f.by)
	if err != nil {
		t.Fatal(err)
	}
	closed, _ := f.s.GetWorkItem(f.ctx, f.task.ID, stubs["f1"].ID)
	counts = foldCounts(t, f.s)
	if again, err := f.s.PostMessage(f.ctx, f.task.ID, req, f.by); err != nil || again.Seq != m.Seq {
		t.Fatalf("replay %v %v", again.Seq, err)
	}
	if after := foldCounts(t, f.s); after != counts {
		t.Fatalf("replay wrote rows: %v -> %v", counts, after)
	}
	if got, _ := f.s.GetWorkItem(f.ctx, f.task.ID, stubs["f1"].ID); got.Revision != 2 || got.Description != closed.Description || closed.Status != "dismissed" {
		t.Fatalf("replay changed the closed stub: %+v", got)
	}
	if notes := foldNotes(t, f.s, refined.ID); len(notes) != 1 {
		t.Fatalf("refined notes after replay %+v", notes)
	}
}

// a5, a6 (review #49256 b1): a round-two regression blocker that reuses a
// round-one follow-up's ID is verified by a focused pass; the pass names the
// blocker, so the follow-up is not settled: an unchanged stub stays open at
// revision 1, a refined copy gets no note, and triage does not list it.
func TestReviewFollowUpIDCollisionNotSettled(t *testing.T) {
	for _, refined := range []bool{false, true} {
		t.Run(fmt.Sprintf("refined=%v", refined), func(t *testing.T) {
			f := newConvergenceFixture(t)
			_, r2, stubs := roundOneFollowUps(t, f)
			stub := stubs["f1"]
			if refined {
				text := "Refined by the handler: explain the fixture retry in fixture.go."
				var err error
				if stub, err = f.s.UpdateWorkItem(f.ctx, f.task.ID, stub.ID, api.UpdateWorkItemRequest{Revision: 1, Description: &text}, f.by); err != nil {
					t.Fatal(err)
				}
			}
			reg := api.ReviewFinding{ID: "f1", Regression: true, Baseline: candidateA, Candidate: candidateB, Title: "Fixture regression", File: "fixture.go", Line: 8}
			if err := f.roundTwoResult(passConvergence, api.ReviewMetadata{BlockerIDs: []string{"b1"}, Blockers: []api.ReviewFinding{reg}}, r2.Seq); err != nil {
				t.Fatal(err)
			}
			focus := api.ReviewMetadata{Mode: "focused", Candidate: candidateC, Fix: "Fix the regression", BlockerIDs: []string{"f1"}}
			req, err := f.post(api.Envelope{Kind: "request", Subject: "Verify exactly the regression fix", Review: &focus, Body: api.EnvelopeBody{Ask: "Verify exact regression fix"}}, f.reviewer.ID, 0, api.Agent{})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = f.post(f.resultEnv(candidateC, map[string]string{"f1": "pass"}, focus), "", req.Seq, f.reviewer); err != nil {
				t.Fatal(err)
			}
			if st := f.state(t); len(st.Focused) != 2 || !st.Focused[1].Passed || len(outstandingOnCandidate(st, candidateC)) != 0 {
				t.Fatalf("regression blocker not verified: %+v", st.Focused)
			}
			got, err := f.s.GetWorkItem(f.ctx, f.task.ID, stub.ID)
			if err != nil || got.Status != "open" || got.Revision != stub.Revision || got.Description != stub.Description || itemRevisionCount(t, f.s, stub.ID) != int(stub.Revision) || len(foldNotes(t, f.s, stub.ID)) != 0 {
				t.Fatalf("colliding follow-up settled: %+v %v", got, err)
			}
			tri, err := f.s.WorkItemTriage(f.ctx, f.task.ID, 0, "", "")
			if err != nil {
				t.Fatal(err)
			}
			for _, r := range tri.AlreadyReleased {
				if r.Item.ID == stub.ID {
					t.Fatalf("colliding follow-up listed: %+v", r)
				}
			}
		})
	}
}

// dispose posts a lead disposition NOTICE of kind on candidate naming fixed.
func (f *convergenceFixture) dispose(kind, candidate string, fixed []string) (api.Message, error) {
	meta := api.ReviewMetadata{Mode: "disposition", Disposition: kind, Candidate: candidate, FixedFollowUpIDs: fixed}
	return f.post(api.Envelope{Kind: "notice", Subject: "Record the fixture review disposition", Review: &meta, Body: api.EnvelopeBody{Text: "Disposition"}}, "", 0, api.Agent{})
}

func eventCount(t *testing.T, f *convergenceFixture, kind string) int {
	t.Helper()
	var n int
	if err := f.s.db.QueryRow(`SELECT count(*) FROM events WHERE task_id=? AND kind=?`, f.task.ID, kind).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// t2: a lead disposition naming round-one follow-ups fixed in the item
// dismisses an unchanged held stub and notes a refined item, in the NOTICE's
// transaction.
func TestReviewDispositionSettlesFixedFollowUps(t *testing.T) {
	f := newConvergenceFixture(t)
	r1, r2, stubs := roundOneFollowUps(t, f)
	text := "Refined by the handler: keep the receipt on fixture retries."
	refined, err := f.s.UpdateWorkItem(f.ctx, f.task.ID, stubs["f2"].ID, api.UpdateWorkItemRequest{Revision: 1, Description: &text}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.roundTwoResult(passConvergence, api.ReviewMetadata{BlockerIDs: []string{"b1"}}, r2.Seq); err != nil {
		t.Fatal(err)
	}
	seedPassingVerification(t, f.s, f.item, candidateB)
	notice, err := f.dispose("accept", candidateB, []string{"f1", "f2"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := f.s.GetWorkItem(f.ctx, f.task.ID, stubs["f1"].ID)
	want := stubs["f1"].Description + "\n\n" + fixedLine(f.item, candidateB, "lead disposition", notice.Seq, r1.Seq, "f1")
	if err != nil || got.Status != "dismissed" || got.Revision != 2 || got.Description != want {
		t.Fatalf("unchanged stub not dismissed: %+v %v", got, err)
	}
	got, err = f.s.GetWorkItem(f.ctx, f.task.ID, refined.ID)
	if err != nil || got.Status != "open" || got.Revision != refined.Revision || got.Description != refined.Description {
		t.Fatalf("refined item changed: %+v %v", got, err)
	}
	notes := foldNotes(t, f.s, refined.ID)
	if len(notes) != 1 || notes[0].namespace != "review-fixed" || notes[0].content != fixedLine(f.item, candidateB, "lead disposition", notice.Seq, r1.Seq, "f2") {
		t.Fatalf("notes %+v", notes)
	}
	if closed, fixed := eventCount(t, f, "review_follow_up_closed"), eventCount(t, f, "review_follow_up_fixed"); closed != 1 || fixed != 1 {
		t.Fatalf("events closed=%d fixed=%d", closed, fixed)
	}
	if st := f.state(t); st.Disposition == nil || st.Disposition.Kind != "accept" || st.Disposition.MessageSeq != notice.Seq {
		t.Fatalf("disposition %+v", st.Disposition)
	}
	tri, err := f.s.WorkItemTriage(f.ctx, f.task.ID, 0, "", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range tri.HeldForTriage {
		if h.Item.ID == stubs["f1"].ID {
			t.Fatalf("dismissed stub still held: %+v", h)
		}
	}
}

// t2: a refused fixedFollowUpIds writes nothing; the field outside a
// disposition is refused, including on a general REVIEW, which then opens no
// round.
func TestReviewDispositionFixedFollowUpRefusals(t *testing.T) {
	f := newConvergenceFixture(t)
	_, r2, stubs := roundOneFollowUps(t, f)
	held := func(name string) {
		t.Helper()
		for id, stub := range stubs {
			got, err := f.s.GetWorkItem(f.ctx, f.task.ID, stub.ID)
			if err != nil || got.Revision != 1 || got.Status != "open" || got.Description != stub.Description {
				t.Fatalf("%s: stub %s changed: %+v %v", name, id, got, err)
			}
		}
	}
	counts := foldCounts(t, f.s)
	refusedWith(t, "general RESULT", f.roundTwoResult(passConvergence, api.ReviewMetadata{BlockerIDs: []string{"b1"}, FixedFollowUpIDs: []string{"f1"}}, r2.Seq), "fixedFollowUpIds is for a lead disposition")
	held("general RESULT")
	if err := f.roundTwoResult(passConvergence, api.ReviewMetadata{BlockerIDs: []string{"b1"}}, r2.Seq); err != nil {
		t.Fatal(err)
	}
	counts = foldCounts(t, f.s)
	for _, tc := range []struct {
		name, kind string
		fixed      []string
		reason     string
	}{
		{"unknown ID", "follow-ups", []string{"f9"}, "fixedFollowUpIds must name distinct round-one follow-ups"},
		{"named twice", "follow-ups", []string{"f1", "f1"}, "fixedFollowUpIds must name distinct round-one follow-ups"},
		{"round-one blocker", "follow-ups", []string{"b1"}, "fixedFollowUpIds must name distinct round-one follow-ups"},
	} {
		_, err := f.dispose(tc.kind, candidateB, tc.fixed)
		refusedWith(t, tc.name, err, tc.reason)
		held(tc.name)
		if st := f.state(t); st.Disposition != nil {
			t.Fatalf("%s: disposition saved %+v", tc.name, st.Disposition)
		}
		if after := foldCounts(t, f.s); after != counts {
			t.Fatalf("%s: refused NOTICE wrote rows: %v -> %v", tc.name, counts, after)
		}
	}
	if _, err := f.dispose("follow-ups", candidateB, []string{"f1"}); err != nil {
		t.Fatal(err)
	}
	if got, err := f.s.GetWorkItem(f.ctx, f.task.ID, stubs["f1"].ID); err != nil || got.Status != "dismissed" {
		t.Fatalf("f1 %+v %v", got, err)
	}
	seedPassingVerification(t, f.s, f.item, candidateB)
	counts = foldCounts(t, f.s)
	_, err := f.dispose("owner-accept", candidateB, []string{"f2"})
	refusedWith(t, "owner-accept", err, "owner-accept names no fixed follow-ups")
	if got, err := f.s.GetWorkItem(f.ctx, f.task.ID, stubs["f2"].ID); err != nil || got.Revision != 1 || got.Status != "open" {
		t.Fatalf("owner-accept wrote f2: %+v %v", got, err)
	}
	if after := foldCounts(t, f.s); after != counts {
		t.Fatalf("owner-accept wrote rows: %v -> %v", counts, after)
	}
	if _, err = f.dispose("owner-accept", candidateB, nil); err != nil {
		t.Fatal(err)
	}

	// A non-colliding follow-up whose criterion did not pass is refused off accept.
	g := newConvergenceFixture(t)
	_, r2, stubs = roundOneFollowUps(t, g)
	if err = g.roundTwoResult(map[string]string{"a1": "pass", "a2": "partial"}, api.ReviewMetadata{BlockerIDs: []string{"b1"}}, r2.Seq); err != nil {
		t.Fatal(err)
	}
	counts = foldCounts(t, g.s)
	_, err = g.dispose("follow-ups", candidateB, []string{"f2"})
	refusedWith(t, "criterion not passing", err, "fixed follow-up f2 criterion must pass")
	if got, err := g.s.GetWorkItem(g.ctx, g.task.ID, stubs["f2"].ID); err != nil || got.Revision != 1 || got.Status != "open" {
		t.Fatalf("criterion refusal wrote f2: %+v %v", got, err)
	}
	if after := foldCounts(t, g.s); after != counts || g.state(t).Disposition != nil {
		t.Fatalf("criterion refusal wrote rows: %v -> %v", counts, after)
	}
	if _, err = g.dispose("follow-ups", candidateB, nil); err != nil {
		t.Fatal(err)
	}

	// A general REVIEW carrying the field is refused before it opens a round.
	h := newConvergenceFixture(t)
	h.generalRound(t, candidateA, passConvergence, api.ReviewMetadata{})
	counts = foldCounts(t, h.s)
	_, err = h.post(api.Envelope{Kind: "review", Subject: "Review frozen fixture candidate", Review: &api.ReviewMetadata{Mode: "general", FixedFollowUpIDs: []string{"f1"}}, Body: api.EnvelopeBody{Candidate: candidateB, Scope: "Fixture", Acceptance: h.criteria}}, h.reviewer.ID, 0, api.Agent{})
	refusedWith(t, "general REVIEW", err, "fixedFollowUpIds is for a lead disposition")
	if st := h.state(t); len(st.Rounds) != 1 {
		t.Fatalf("refused REVIEW opened a round: %+v", st.Rounds)
	}
	if after := foldCounts(t, h.s); after != counts {
		t.Fatalf("refused REVIEW wrote rows: %v -> %v", counts, after)
	}
	h.review(t, candidateB)
	if st := h.state(t); len(st.Rounds) != 2 {
		t.Fatalf("REVIEW without the field opened no round: %+v", st.Rounds)
	}
}

// t2 (a141d251): a disposition naming a follow-up ID that a saved blocker of
// the stage reuses skips it, even when the follow-up cites a criterion that
// did not pass.
func TestReviewDispositionFollowUpIDCollisionSkipped(t *testing.T) {
	for _, refined := range []bool{false, true} {
		t.Run(fmt.Sprintf("refined=%v", refined), func(t *testing.T) {
			f := newConvergenceFixture(t)
			_, r2, stubs := roundOneFollowUps(t, f)
			stub := stubs["f1"]
			if refined {
				text := "Refined by the handler: explain the fixture retry in fixture.go."
				var err error
				if stub, err = f.s.UpdateWorkItem(f.ctx, f.task.ID, stub.ID, api.UpdateWorkItemRequest{Revision: 1, Description: &text}, f.by); err != nil {
					t.Fatal(err)
				}
			}
			reg := api.ReviewFinding{ID: "f1", Regression: true, Baseline: candidateA, Candidate: candidateB, Title: "Fixture regression", File: "fixture.go", Line: 8}
			if err := f.roundTwoResult(passConvergence, api.ReviewMetadata{BlockerIDs: []string{"b1"}, Blockers: []api.ReviewFinding{reg}}, r2.Seq); err != nil {
				t.Fatal(err)
			}
			focus := api.ReviewMetadata{Mode: "focused", Candidate: candidateC, Fix: "Fix the regression", BlockerIDs: []string{"f1"}}
			req, err := f.post(api.Envelope{Kind: "request", Subject: "Verify exactly the regression fix", Review: &focus, Body: api.EnvelopeBody{Ask: "Verify exact regression fix"}}, f.reviewer.ID, 0, api.Agent{})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = f.post(f.resultEnv(candidateC, map[string]string{"f1": "pass"}, focus), "", req.Seq, f.reviewer); err != nil {
				t.Fatal(err)
			}
			if _, err = f.dispose("follow-ups", candidateC, []string{"f1"}); err != nil {
				t.Fatal(err)
			}
			got, err := f.s.GetWorkItem(f.ctx, f.task.ID, stub.ID)
			if err != nil || got.Status != "open" || got.Revision != stub.Revision || got.Description != stub.Description || itemRevisionCount(t, f.s, stub.ID) != int(stub.Revision) || len(foldNotes(t, f.s, stub.ID)) != 0 {
				t.Fatalf("colliding follow-up settled: %+v %v", got, err)
			}
		})
	}
	t.Run("criterion", func(t *testing.T) {
		f := newConvergenceFixture(t)
		_, r2, stubs := roundOneFollowUps(t, f)
		reg := api.ReviewFinding{ID: "f2", Criterion: "a2", Regression: true, Baseline: candidateA, Candidate: candidateB, Title: "Fixture retry regression", File: "fixture.go", Line: 9}
		if err := f.roundTwoResult(map[string]string{"a1": "pass", "a2": "fail"}, api.ReviewMetadata{BlockerIDs: []string{"b1"}, Blockers: []api.ReviewFinding{reg}}, r2.Seq); err != nil {
			t.Fatal(err)
		}
		if _, err := f.dispose("follow-ups", candidateB, []string{"f2"}); err != nil {
			t.Fatal(err)
		}
		got, err := f.s.GetWorkItem(f.ctx, f.task.ID, stubs["f2"].ID)
		if err != nil || got.Status != "open" || got.Revision != 1 || len(foldNotes(t, f.s, got.ID)) != 0 {
			t.Fatalf("colliding criterion follow-up settled: %+v %v", got, err)
		}
		if closed, fixed := eventCount(t, f, "review_follow_up_closed"), eventCount(t, f, "review_follow_up_fixed"); closed != 0 || fixed != 0 {
			t.Fatalf("events closed=%d fixed=%d", closed, fixed)
		}
	})
}

// t2 (review #49814 f1): a disposition naming follow-ups round two already
// settled through its blockerIds writes nothing more: the dismissed stub keeps
// its revision and the refined item keeps its one note.
func TestReviewDispositionSkipsFollowUpSettledByRoundTwo(t *testing.T) {
	f := newConvergenceFixture(t)
	_, r2, stubs := roundOneFollowUps(t, f)
	text := "Refined by the handler: keep the receipt on fixture retries."
	refined, err := f.s.UpdateWorkItem(f.ctx, f.task.ID, stubs["f2"].ID, api.UpdateWorkItemRequest{Revision: 1, Description: &text}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.roundTwoResult(passConvergence, api.ReviewMetadata{BlockerIDs: []string{"b1", "f1", "f2"}}, r2.Seq); err != nil {
		t.Fatal(err)
	}
	closed, err := f.s.GetWorkItem(f.ctx, f.task.ID, stubs["f1"].ID)
	if err != nil || closed.Status != "dismissed" || len(foldNotes(t, f.s, refined.ID)) != 1 {
		t.Fatalf("round two did not settle: %+v %v", closed, err)
	}
	counts := foldCounts(t, f.s)
	if _, err = f.dispose("follow-ups", candidateB, []string{"f1", "f2"}); err != nil {
		t.Fatal(err)
	}
	counts[0]++
	if after := foldCounts(t, f.s); after != counts {
		t.Fatalf("disposition wrote follow-up rows: %v -> %v", counts, after)
	}
	if got, err := f.s.GetWorkItem(f.ctx, f.task.ID, closed.ID); err != nil || got.Revision != closed.Revision || got.Description != closed.Description {
		t.Fatalf("dismissed stub changed: %+v %v", got, err)
	}
	if got, err := f.s.GetWorkItem(f.ctx, f.task.ID, refined.ID); err != nil || got.Revision != refined.Revision || got.Status != "open" {
		t.Fatalf("refined item changed: %+v %v", got, err)
	}
	if notes := foldNotes(t, f.s, refined.ID); len(notes) != 1 {
		t.Fatalf("refined notes %+v", notes)
	}
	if c, x := eventCount(t, f, "review_follow_up_closed"), eventCount(t, f, "review_follow_up_fixed"); c != 1 || x != 1 {
		t.Fatalf("events closed=%d fixed=%d", c, x)
	}
}

// t2: a disposition without fixedFollowUpIds records exactly as before and
// writes no follow-up.
func TestReviewDispositionWithoutFixedFollowUpsWritesNoFollowUp(t *testing.T) {
	f := newConvergenceFixture(t)
	_, r2, stubs := roundOneFollowUps(t, f)
	if err := f.roundTwoResult(passConvergence, api.ReviewMetadata{BlockerIDs: []string{"b1"}}, r2.Seq); err != nil {
		t.Fatal(err)
	}
	counts := foldCounts(t, f.s)
	notice, err := f.dispose("follow-ups", candidateB, nil)
	if err != nil {
		t.Fatal(err)
	}
	after := foldCounts(t, f.s)
	counts[0]++
	if after != counts {
		t.Fatalf("disposition wrote follow-up rows: %v -> %v", counts, after)
	}
	for id, stub := range stubs {
		if got, err := f.s.GetWorkItem(f.ctx, f.task.ID, stub.ID); err != nil || got.Revision != 1 || got.Status != "open" {
			t.Fatalf("stub %s changed: %+v %v", id, got, err)
		}
	}
	if notice.Envelope == nil || notice.Envelope.Review == nil || notice.Envelope.Review.FixedFollowUpIDs != nil {
		t.Fatalf("notice metadata %+v", notice.Envelope.Review)
	}
	if st := f.state(t); st.Disposition == nil || st.Disposition.Kind != "follow-ups" || st.Disposition.MessageSeq != notice.Seq {
		t.Fatalf("disposition %+v", st.Disposition)
	}
}

// t2: settling is atomic with the NOTICE; a replay settles nothing twice.
func TestReviewDispositionFixedFollowUpRollback(t *testing.T) {
	f := newConvergenceFixture(t)
	_, r2, stubs := roundOneFollowUps(t, f)
	text := "Refined by the handler: keep the receipt on fixture retries."
	refined, err := f.s.UpdateWorkItem(f.ctx, f.task.ID, stubs["f2"].ID, api.UpdateWorkItemRequest{Revision: 1, Description: &text}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.roundTwoResult(passConvergence, api.ReviewMetadata{BlockerIDs: []string{"b1"}}, r2.Seq); err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.db.Exec(`CREATE TRIGGER refuse_fixed_note BEFORE INSERT ON narrative_artifact_versions BEGIN SELECT RAISE(ABORT,'forced note failure'); END`); err != nil {
		t.Fatal(err)
	}
	counts := foldCounts(t, f.s)
	if _, err = f.dispose("follow-ups", candidateB, []string{"f1", "f2"}); err == nil || !strings.Contains(err.Error(), "forced note failure") {
		t.Fatalf("want forced failure, got %v", err)
	}
	if got, err := f.s.GetWorkItem(f.ctx, f.task.ID, stubs["f1"].ID); err != nil || got.Revision != 1 || got.Status != "open" {
		t.Fatalf("refused NOTICE closed a stub: %+v %v", got, err)
	}
	if after := foldCounts(t, f.s); after != counts || f.state(t).Disposition != nil {
		t.Fatalf("refused NOTICE wrote rows: %v -> %v", counts, after)
	}
	if _, err = f.s.db.Exec(`DROP TRIGGER refuse_fixed_note`); err != nil {
		t.Fatal(err)
	}
	meta := api.ReviewMetadata{Mode: "disposition", Disposition: "follow-ups", Candidate: candidateB, FixedFollowUpIDs: []string{"f1", "f2"}}
	req := f.req(api.Envelope{Kind: "notice", Subject: "Record the fixture review disposition", Review: &meta, Body: api.EnvelopeBody{Text: "Disposition"}}, "", 0, api.Agent{})
	m, err := f.s.PostMessage(f.ctx, f.task.ID, req, f.by)
	if err != nil {
		t.Fatal(err)
	}
	counts = foldCounts(t, f.s)
	if again, err := f.s.PostMessage(f.ctx, f.task.ID, req, f.by); err != nil || again.Seq != m.Seq {
		t.Fatalf("replay %v %v", again.Seq, err)
	}
	if after := foldCounts(t, f.s); after != counts {
		t.Fatalf("replay wrote rows: %v -> %v", counts, after)
	}
	if got, _ := f.s.GetWorkItem(f.ctx, f.task.ID, stubs["f1"].ID); got.Revision != 2 || got.Status != "dismissed" {
		t.Fatalf("f1 after replay %+v", got)
	}
	if notes := foldNotes(t, f.s, refined.ID); len(notes) != 1 {
		t.Fatalf("refined notes after replay %+v", notes)
	}
}

// t2: fixedFollowUpIds decodes as tt send decodes --review-file, and a
// metadata without it encodes no key.
func TestReviewDispositionFixedFollowUpsWireDecode(t *testing.T) {
	dec := json.NewDecoder(strings.NewReader(`{"mode":"disposition","disposition":"follow-ups","candidate":"` + candidateB + `","fixedFollowUpIds":["f1"]}`))
	dec.DisallowUnknownFields()
	var meta api.ReviewMetadata
	if err := dec.Decode(&meta); err != nil || !reflect.DeepEqual(meta.FixedFollowUpIDs, []string{"f1"}) {
		t.Fatalf("decode %+v %v", meta, err)
	}
	raw, err := json.Marshal(api.ReviewMetadata{Mode: "disposition", Disposition: "follow-ups", Candidate: candidateB})
	if err != nil || strings.Contains(string(raw), "fixedFollowUpIds") {
		t.Fatalf("encode %s %v", raw, err)
	}
}
