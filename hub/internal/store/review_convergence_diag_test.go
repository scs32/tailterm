package store

import (
	"testing"

	"github.com/scs32/tailterm/hub/internal/api"
)

// Diagnosis for wi_6b77641eabe55fc0 (order #41982): on wi_d7010deecb20211f a
// round-two blocker b1 passed structured focused verification (#15339), a
// plain lead accept was refused, and the item needed an owner-accept (#15340,
// #15343). These tests rebuild that shape at the current tip for each blocker
// form the hub accepts and pin today's outcome, so a fix changes them on
// purpose. See docs/incidents/review-blocker-focused-15340/report.md.
//
// The fixture stands in for the incident's criteria: a1 is verification-owned
// (a1-a10 and a12 were pending-verification) and a2 is the reviewed criterion
// the blocker concerns (a11, the runbook).

const diagRefusalPartial = "conflict: review convergence: criterion a2 has not passed; a partial with no blocker is settled by focused verification naming it in criterionIds"
const diagRefusalFailed = "conflict: review convergence: criterion a2 has not passed"

func newDiagFixture(t *testing.T) *convergenceFixture {
	t.Helper()
	return newConvergenceFixtureWithCriteria(t, map[string]string{"a1": "matrix passes", "a2": "runbook step takes effect"}, []string{"a1"})
}

func (f *convergenceFixture) diagFocused(t *testing.T, meta api.ReviewMetadata, status map[string]string) {
	t.Helper()
	f.focusedCheck(t, meta, status, map[string]api.Evidence{"e1": {Type: "record", Value: "fixture: named fix confirmed on " + meta.Candidate}})
}

func (f *convergenceFixture) diagDisposition(kind, candidate string, from api.Agent) error {
	_, err := f.post(api.Envelope{Kind: "notice", Subject: "Record the review disposition", Review: &api.ReviewMetadata{Mode: "disposition", Disposition: kind, Candidate: candidate}, Body: api.EnvelopeBody{Text: kind}}, "", 0, from)
	return err
}

func diagOutcome(err error) string {
	if err == nil {
		return "accepted"
	}
	return err.Error()
}

// Every form: two general rounds, round two records blocker b1 on candidate B,
// structured focused verification passes b1 on candidate C, the matrix receipt
// for C is saved, and the lead posts a plain accept of C.
func TestReviewConvergenceDiagBlockerFormsAfterFocusedPass(t *testing.T) {
	regression := api.ReviewFinding{ID: "b1", Regression: true, Baseline: candidateA, Candidate: candidateB, Title: "Runbook baseline update never reaches the running deployer", File: "docs/project-deployment.md", Line: 316}
	cited := api.ReviewFinding{ID: "b1", Criterion: "a2", Title: "Runbook baseline update never reaches the running deployer", File: "docs/project-deployment.md", Line: 316}
	forms := []struct {
		name     string
		round1   map[string]string
		blockers []api.ReviewFinding // round one, retained in round two when set
		round2   map[string]string
		blocker  api.ReviewFinding
		want     string
	}{
		// The #15340 shape: a regression blocker with no criterion, a11 partial.
		{name: "regression blocker, criterion partial", round1: map[string]string{"a1": "pending-verification", "a2": "partial"}, round2: map[string]string{"a1": "pending-verification", "a2": "partial"}, blocker: regression, want: diagRefusalPartial},
		{name: "regression blocker, criterion failed", round1: map[string]string{"a1": "pending-verification", "a2": "partial"}, round2: map[string]string{"a1": "pending-verification", "a2": "fail"}, blocker: regression, want: diagRefusalFailed},
		{name: "regression blocker, criterion passed", round1: map[string]string{"a1": "pending-verification", "a2": "partial"}, round2: map[string]string{"a1": "pending-verification", "a2": "pass"}, blocker: regression, want: "accepted"},
		// New in round two, so the hub files it as a finding; it still fails a2.
		{name: "criterion blocker new in round two", round1: map[string]string{"a1": "pending-verification", "a2": "partial"}, round2: map[string]string{"a1": "pending-verification", "a2": "fail"}, blocker: cited, want: "accepted"},
		{name: "criterion blocker carried from round one", round1: map[string]string{"a1": "pending-verification", "a2": "fail"}, blockers: []api.ReviewFinding{cited}, round2: map[string]string{"a1": "pending-verification", "a2": "fail"}, blocker: cited, want: "accepted"},
	}
	for _, form := range forms {
		t.Run(form.name, func(t *testing.T) {
			f := newDiagFixture(t)
			f.generalRound(t, candidateA, form.round1, api.ReviewMetadata{Blockers: form.blockers})
			blocker := form.blocker
			if blocker.Regression {
				blocker.Candidate = candidateB
			}
			f.generalRound(t, candidateB, form.round2, api.ReviewMetadata{Blockers: []api.ReviewFinding{blocker}})
			f.diagFocused(t, api.ReviewMetadata{Candidate: candidateC, Fix: "Re-read baselines each poll", BlockerIDs: []string{"b1"}}, map[string]string{"b1": "pass"})
			seedPassingVerification(t, f.s, f.item, candidateC)
			got := diagOutcome(f.acceptCandidate(candidateC, f.lead))
			t.Logf("accept after focused pass of b1: %s", got)
			if got != form.want {
				t.Fatalf("accept outcome changed:\n got %s\nwant %s", got, form.want)
			}
		})
	}
	t.Run("blocker with neither criterion nor regression", func(t *testing.T) {
		f := newDiagFixture(t)
		f.generalRound(t, candidateA, map[string]string{"a1": "pending-verification", "a2": "partial"}, api.ReviewMetadata{})
		r := f.review(t, candidateB)
		bare := api.ReviewFinding{ID: "b1", Title: "Runbook baseline update never reaches the running deployer", File: "docs/project-deployment.md", Line: 316}
		_, err := f.post(f.resultEnv(candidateB, map[string]string{"a1": "pending-verification", "a2": "partial"}, api.ReviewMetadata{Mode: "general", Blockers: []api.ReviewFinding{bare}}), "", r.Seq, f.reviewer)
		t.Logf("round-two result with a bare blocker: %s", diagOutcome(err))
		if want := "conflict: review convergence: blocker must cite failed criterion or regression"; diagOutcome(err) != want {
			t.Fatalf("bare blocker outcome changed:\n got %s\nwant %s", diagOutcome(err), want)
		}
	})
}

// diagBlockerOnlyFocusedRefused replays the #15340 sequence: focused
// verification names only b1 on C, the plain accept is refused, and the lead
// cannot then settle a2 on C. A criterionIds-only request on C is a
// changed-candidate route that needs the reviewed candidate converged, and b1
// is still open on B; naming b1 again is refused because it passed on C.
func diagBlockerOnlyFocusedRefused(t *testing.T) *convergenceFixture {
	t.Helper()
	f := newDiagFixture(t)
	f.generalRound(t, candidateA, map[string]string{"a1": "pending-verification", "a2": "partial"}, api.ReviewMetadata{})
	b1 := api.ReviewFinding{ID: "b1", Regression: true, Baseline: candidateA, Candidate: candidateB, Title: "Runbook baseline update never reaches the running deployer", File: "docs/project-deployment.md", Line: 316}
	f.generalRound(t, candidateB, map[string]string{"a1": "pending-verification", "a2": "partial"}, api.ReviewMetadata{Blockers: []api.ReviewFinding{b1}})
	f.diagFocused(t, api.ReviewMetadata{Candidate: candidateC, Fix: "Re-read baselines each poll", BlockerIDs: []string{"b1"}}, map[string]string{"b1": "pass"})
	seedPassingVerification(t, f.s, f.item, candidateC)
	if got := diagOutcome(f.acceptCandidate(candidateC, f.lead)); got != diagRefusalPartial {
		t.Fatalf("accept outcome changed:\n got %s\nwant %s", got, diagRefusalPartial)
	}
	settle := api.ReviewMetadata{Candidate: candidateC, Fix: "Re-read baselines each poll after " + candidateB, CriterionIDs: []string{"a2"}}
	_, err := f.focusedRequest(settle)
	t.Logf("criterionIds-only request on C: %s", diagOutcome(err))
	want := "conflict: review convergence: rebase verification needs a converged review; name unresolved blocker IDs instead, and round-two partials in criterionIds (cause: unresolved blockers on accepted candidate)"
	if diagOutcome(err) != want {
		t.Fatalf("settling request outcome changed:\n got %s\nwant %s", diagOutcome(err), want)
	}
	again := api.ReviewMetadata{Candidate: candidateC, Fix: "Re-read baselines each poll", BlockerIDs: []string{"b1"}, CriterionIDs: []string{"a2"}}
	_, err = f.focusedRequest(again)
	t.Logf("b1 plus criterionIds request on C: %s", diagOutcome(err))
	if want := "conflict: review convergence: focused verification must name distinct unresolved blocker IDs"; diagOutcome(err) != want {
		t.Fatalf("repeat request outcome changed:\n got %s\nwant %s", diagOutcome(err), want)
	}
	return f
}

// For C itself only the owner-decision and owner-accept path is left, as on
// 09-29 (#15340, #15343).
func TestReviewConvergenceDiagBlockerOnlyFocusedNeedsOwnerAccept(t *testing.T) {
	f := diagBlockerOnlyFocusedRefused(t)
	if err := f.diagDisposition("owner-decision", candidateC, f.lead); err != nil {
		t.Fatal("owner-decision", err)
	}
	if err := f.diagDisposition("owner-accept", candidateC, api.Agent{}); err != nil {
		t.Fatal("owner-accept", err)
	}
}

// A different commit D escapes without the owner: b1 is open again on D, so
// one focused request can name b1 and a2 together.
func TestReviewConvergenceDiagBlockerOnlyFocusedNewCandidateSettles(t *testing.T) {
	f := diagBlockerOnlyFocusedRefused(t)
	seedPassingVerification(t, f.s, f.item, candidateD)
	f.diagFocused(t, api.ReviewMetadata{Candidate: candidateD, Fix: "Re-read baselines each poll", BlockerIDs: []string{"b1"}, CriterionIDs: []string{"a2"}}, map[string]string{"b1": "pass", "a2": "pass"})
	got := diagOutcome(f.acceptCandidate(candidateD, f.lead))
	t.Logf("accept of D after focused pass of b1 and a2: %s", got)
	if got != "accepted" {
		t.Fatalf("accept outcome changed:\n got %s\nwant accepted", got)
	}
}

// The route added by 67d552cd: one focused request names b1 and the partial
// a2 together on C, the reviewer passes both, and the plain accept succeeds.
func TestReviewConvergenceDiagPartialRegressionSettledInOneFocused(t *testing.T) {
	f := newDiagFixture(t)
	f.generalRound(t, candidateA, map[string]string{"a1": "pending-verification", "a2": "partial"}, api.ReviewMetadata{})
	b1 := api.ReviewFinding{ID: "b1", Regression: true, Baseline: candidateA, Candidate: candidateB, Title: "Runbook baseline update never reaches the running deployer", File: "docs/project-deployment.md", Line: 316}
	f.generalRound(t, candidateB, map[string]string{"a1": "pending-verification", "a2": "partial"}, api.ReviewMetadata{Blockers: []api.ReviewFinding{b1}})
	seedPassingVerification(t, f.s, f.item, candidateC)
	f.diagFocused(t, api.ReviewMetadata{Candidate: candidateC, Fix: "Re-read baselines each poll", BlockerIDs: []string{"b1"}, CriterionIDs: []string{"a2"}}, map[string]string{"b1": "pass", "a2": "pass"})
	got := diagOutcome(f.acceptCandidate(candidateC, f.lead))
	t.Logf("accept after focused pass of b1 and a2: %s", got)
	if got != "accepted" {
		t.Fatalf("accept outcome changed:\n got %s\nwant accepted", got)
	}
}

// With a2 failed rather than partial, criterionIds cannot name it, so no
// focused request maps the regression's pass to a2.
func TestReviewConvergenceDiagFailedRegressionCannotSettle(t *testing.T) {
	f := newDiagFixture(t)
	f.generalRound(t, candidateA, map[string]string{"a1": "pending-verification", "a2": "partial"}, api.ReviewMetadata{})
	b1 := api.ReviewFinding{ID: "b1", Regression: true, Baseline: candidateA, Candidate: candidateB, Title: "Runbook baseline update never reaches the running deployer", File: "docs/project-deployment.md", Line: 316}
	f.generalRound(t, candidateB, map[string]string{"a1": "pending-verification", "a2": "fail"}, api.ReviewMetadata{Blockers: []api.ReviewFinding{b1}})
	seedPassingVerification(t, f.s, f.item, candidateC)
	both := api.ReviewMetadata{Candidate: candidateC, Fix: "Re-read baselines each poll", BlockerIDs: []string{"b1"}, CriterionIDs: []string{"a2"}}
	_, err := f.focusedRequest(both)
	t.Logf("b1 plus criterionIds request with a2 failed: %s", diagOutcome(err))
	if want := "conflict: review convergence: criterionIds must name distinct round-two partial criteria that are not verification-owned"; diagOutcome(err) != want {
		t.Fatalf("settling request outcome changed:\n got %s\nwant %s", diagOutcome(err), want)
	}
	f.diagFocused(t, api.ReviewMetadata{Candidate: candidateC, Fix: "Re-read baselines each poll", BlockerIDs: []string{"b1"}}, map[string]string{"b1": "pass"})
	if got := diagOutcome(f.acceptCandidate(candidateC, f.lead)); got != diagRefusalFailed {
		t.Fatalf("accept outcome changed:\n got %s\nwant %s", got, diagRefusalFailed)
	}
	if err = f.diagDisposition("owner-decision", candidateC, f.lead); err != nil {
		t.Fatal("owner-decision", err)
	}
	if err = f.diagDisposition("owner-accept", candidateC, api.Agent{}); err != nil {
		t.Fatal("owner-accept", err)
	}
}
