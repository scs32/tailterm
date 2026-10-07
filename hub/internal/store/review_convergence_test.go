package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/scs32/tailterm/hub/internal/api"
	"path/filepath"
	"strings"
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
