package store

import (
	"testing"

	"github.com/scs32/tailterm/hub/internal/api"
)

// Diagnosis for wi_6b77641eabe55fc0 (order #41982): on wi_d7010deecb20211f a
// round-two blocker b1 passed structured focused verification (#15339), a
// plain lead accept was refused, and the item needed an owner-accept (#15340,
// #15343). These tests rebuild that shape for each blocker form the hub
// accepts. They first pinned the outcome at the diagnosis tip; the fix for
// wi_06611d359b2081ee (order #42472) changed the rows on purpose, and they now
// pin the fixed behaviour: a round-two fail must be cited, and a partial can be
// settled on the candidate where its blocker passed. See
// docs/incidents/review-blocker-focused-15340/report.md.
//
// The fixture stands in for the incident's criteria: a1 is verification-owned
// (a1-a10 and a12 were pending-verification) and a2 is the reviewed criterion
// the blocker concerns (a11, the runbook).

const diagRefusalPartial = "conflict: review convergence: criterion a2 has not passed; a partial with no blocker is settled by focused verification naming it in criterionIds"
const diagRefusalUncited = "conflict: review convergence: criterion a2 failed with no blocker or finding citing it; set criterion on the blocker for that failure (a regression may cite the criterion it breaks)"

func newDiagFixture(t *testing.T) *convergenceFixture {
	t.Helper()
	return newConvergenceFixtureWithCriteria(t, map[string]string{"a1": "matrix passes", "a2": "runbook step takes effect"}, []string{"a1"})
}

func (f *convergenceFixture) diagFocused(t *testing.T, meta api.ReviewMetadata, status map[string]string) {
	t.Helper()
	f.focusedCheck(t, meta, status, map[string]api.Evidence{"e1": {Type: "record", Value: "fixture: named fix confirmed on " + meta.Candidate}})
}

func diagOutcome(err error) string {
	if err == nil {
		return "accepted"
	}
	return err.Error()
}

// Every form: two general rounds, round two records blocker b1 on candidate B,
// structured focused verification passes b1 on candidate C, the matrix receipt
// for C is saved, and the lead posts a plain accept of C. A form whose round-two
// RESULT is now refused names that refusal in wantRound2 instead.
func TestReviewConvergenceDiagBlockerFormsAfterFocusedPass(t *testing.T) {
	regression := api.ReviewFinding{ID: "b1", Regression: true, Baseline: candidateA, Candidate: candidateB, Title: "Runbook baseline update never reaches the running deployer", File: "docs/project-deployment.md", Line: 316}
	citedRegression := regression
	citedRegression.Criterion = "a2"
	cited := api.ReviewFinding{ID: "b1", Criterion: "a2", Title: "Runbook baseline update never reaches the running deployer", File: "docs/project-deployment.md", Line: 316}
	forms := []struct {
		name       string
		round1     map[string]string
		blockers   []api.ReviewFinding // round one, retained in round two when set
		round2     map[string]string
		blocker    api.ReviewFinding
		wantRound2 string // the round-two RESULT refusal, when the form never reaches the accept
		want       string
	}{
		// The #15340 shape: a regression blocker with no criterion, a11 partial.
		// It still needs a2 in criterionIds; see the blocker-only tests below.
		{name: "regression blocker, criterion partial", round1: map[string]string{"a1": "pending-verification", "a2": "partial"}, round2: map[string]string{"a1": "pending-verification", "a2": "partial"}, blocker: regression, want: diagRefusalPartial},
		// Was accepted at round two and then refused at the accept with "criterion
		// a2 has not passed", leaving only owner-accept. The hub now refuses the
		// uncited fail when the reviewer can still cite it.
		{name: "regression blocker, criterion failed", round1: map[string]string{"a1": "pending-verification", "a2": "partial"}, round2: map[string]string{"a1": "pending-verification", "a2": "fail"}, blocker: regression, wantRound2: diagRefusalUncited},
		// The cited form of the row above: the regression's focused pass passes a2.
		{name: "regression blocker citing the criterion, criterion failed", round1: map[string]string{"a1": "pending-verification", "a2": "partial"}, round2: map[string]string{"a1": "pending-verification", "a2": "fail"}, blocker: citedRegression, want: "accepted"},
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
			if form.wantRound2 != "" {
				r := f.review(t, candidateB)
				_, err := f.post(f.resultEnv(candidateB, form.round2, api.ReviewMetadata{Mode: "general", Blockers: []api.ReviewFinding{blocker}}), "", r.Seq, f.reviewer)
				t.Logf("round-two result: %s", diagOutcome(err))
				if diagOutcome(err) != form.wantRound2 {
					t.Fatalf("round-two outcome changed:\n got %s\nwant %s", diagOutcome(err), form.wantRound2)
				}
				return
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

// diagBlockerOnlyFocusedPass replays the #15340 sequence: focused
// verification names only b1 on C and the plain accept is refused, because a2
// is still partial.
func diagBlockerOnlyFocusedPass(t *testing.T) *convergenceFixture {
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
	return f
}

// After the blocker-only pass, C settles without the owner: a criterionIds-only
// request on C is no longer routed as a rebase of B, because b1 passed on C and
// nothing is open there. At the diagnosis tip this request was refused with
// "rebase verification needs a converged review ... (cause: unresolved blockers
// on accepted candidate)" and only owner-decision and owner-accept were left.
// Naming b1 again stays refused: it already passed on C.
func TestReviewConvergenceDiagBlockerOnlyFocusedSettlesWithoutOwner(t *testing.T) {
	f := diagBlockerOnlyFocusedPass(t)
	again := api.ReviewMetadata{Candidate: candidateC, Fix: "Re-read baselines each poll", BlockerIDs: []string{"b1"}, CriterionIDs: []string{"a2"}}
	_, err := f.focusedRequest(again)
	t.Logf("b1 plus criterionIds request on C: %s", diagOutcome(err))
	if want := "conflict: review convergence: focused verification must name distinct unresolved blocker IDs"; diagOutcome(err) != want {
		t.Fatalf("repeat request outcome changed:\n got %s\nwant %s", diagOutcome(err), want)
	}
	f.diagFocused(t, api.ReviewMetadata{Candidate: candidateC, Fix: "Settle the runbook criterion on the fixed candidate", CriterionIDs: []string{"a2"}}, map[string]string{"a2": "pass"})
	got := diagOutcome(f.acceptCandidate(candidateC, f.lead))
	t.Logf("accept of C after criterionIds-only a2: %s", got)
	if got != "accepted" {
		t.Fatalf("accept outcome changed:\n got %s\nwant accepted", got)
	}
}

// A different commit D still settles too: b1 is open again on D, so one
// focused request can name b1 and a2 together.
func TestReviewConvergenceDiagBlockerOnlyFocusedNewCandidateSettles(t *testing.T) {
	f := diagBlockerOnlyFocusedPass(t)
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

// With a2 failed rather than partial, criterionIds cannot name it, so an
// uncited regression could never pass a2. At the diagnosis tip that round two
// was saved and only owner-accept was left. Now the round-two RESULT is refused
// while the reviewer can still cite a2, and the cited form settles.
func TestReviewConvergenceDiagFailedRegressionMustCite(t *testing.T) {
	f := newDiagFixture(t)
	f.generalRound(t, candidateA, map[string]string{"a1": "pending-verification", "a2": "partial"}, api.ReviewMetadata{})
	b1 := api.ReviewFinding{ID: "b1", Regression: true, Baseline: candidateA, Candidate: candidateB, Title: "Runbook baseline update never reaches the running deployer", File: "docs/project-deployment.md", Line: 316}
	failed := map[string]string{"a1": "pending-verification", "a2": "fail"}
	r := f.review(t, candidateB)
	_, err := f.post(f.resultEnv(candidateB, failed, api.ReviewMetadata{Mode: "general", Blockers: []api.ReviewFinding{b1}}), "", r.Seq, f.reviewer)
	t.Logf("round-two result with an uncited regression and a2 failed: %s", diagOutcome(err))
	if diagOutcome(err) != diagRefusalUncited {
		t.Fatalf("round-two outcome changed:\n got %s\nwant %s", diagOutcome(err), diagRefusalUncited)
	}
	b1.Criterion = "a2"
	if _, err = f.post(f.resultEnv(candidateB, failed, api.ReviewMetadata{Mode: "general", Blockers: []api.ReviewFinding{b1}}), "", r.Seq, f.reviewer); err != nil {
		t.Fatal("cited round two", err)
	}
	seedPassingVerification(t, f.s, f.item, candidateC)
	f.diagFocused(t, api.ReviewMetadata{Candidate: candidateC, Fix: "Re-read baselines each poll", BlockerIDs: []string{"b1"}}, map[string]string{"b1": "pass"})
	got := diagOutcome(f.acceptCandidate(candidateC, f.lead))
	t.Logf("accept of C after focused pass of the cited regression: %s", got)
	if got != "accepted" {
		t.Fatalf("accept outcome changed:\n got %s\nwant accepted", got)
	}
}
