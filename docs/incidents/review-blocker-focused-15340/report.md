# Review blocker passed by focused verification still needed an owner-accept (#15340)

Diagnosis for bug `wi_6b77641eabe55fc0` revision 2, work order #41982
(assignment #41987). Diagnosis only: no service, prompt or docs change outside
this report and `hub/internal/store/review_convergence_diag_test.go`. Base tip
`115b593d`. All line numbers below are `hub/internal/store/review_convergence.go`
at that tip unless another file is named.

## What happened on 09-29

Item `wi_d7010deecb20211f`, read serially from the Board (#15296-#15344):

- Round two (#15305, candidate `61c5229`) recorded blocker `b1` as
  `regression:true` with baseline `784de12` and **no `criterion`**. Status left
  `a11` (the runbook) **`partial`**; `a1`-`a10` and `a12` were
  `pending-verification`. Finding `f6` had no criterion either.
- The lead asked for one focused fix (#15307) and sent an unstructured focused
  request (#15311). Its RESULT #15313 said "a11 now pass" in prose only.
- The lead then asked for a structured focused RESULT (#15338) naming only
  `blockerIds:["b1"]` on `eca4aee`. #15339 reported `b1=pass`.
- The plain accept was refused, so the lead posted an owner-decision (#15340)
  and the owner posted owner-accept (#15343).

The original claim (#15341) was that `b1` "was recorded without a criterion
link". That is right, but `b1` was a regression blocker, which is a form the hub
accepts. The refusal came from `a11` still being `partial`, not from the
missing link alone.

## Code path at `115b593d`

- Recording. `evidenceFinding` (:424) refuses a blocker with neither a
  criterion nor a regression: "blocker must cite failed criterion or
  regression" (:441-442). A regression needs distinct exact baseline and
  candidate (:444-446). A blocker that cites a criterion needs that criterion to
  be `fail` (:1163-1164). In round two a new blocker that is not a regression
  is filed as a finding (:1170-1174) and still fails its criterion.
- Focused pass. `outstanding` (:461) drops every blocker ID a passing focused
  record names (:502-508), so `b1` stops blocking.
- Accept. The disposition `accept` branch (:911-922) calls `reviewReady`
  (:511), that is `reviewReadySettling` (:518). A passing focused record turns
  a criterion to `pass` only through the named blocker's own `Criterion`
  (:541-560: `b.Criterion != ""` at :547 and :557). A regression with no
  criterion maps to nothing. A `partial` is settled only by a focused record
  naming it in `criterionIds` (:566-578). Otherwise the criterion loop refuses
  (:579-595): a partial with the criterionIds hint (:591), or a fail with plain
  "has not passed" (:594).
- Settlement. `criterionIds` (added by `67d552cd`, 10-07, bug
  `wi_0de79d818662efe9`) must name round-two `partial` criteria that are not
  verification-owned (:984-988). A criterion with an open blocker citing it is
  refused (:989-993). A regression with no criterion does not count, so `b1`
  plus `a11` in one request is allowed. A request naming no blocker IDs on a
  candidate other than the reviewed one is routed as a rebase or delta, which
  needs the **reviewed** candidate converged (:1005-1017). Named blocker IDs
  must still be open on the candidate (:1018-1023).
- Owner path. `owner-accept` resolves an `owner-decision` (:886-907) and
  `reviewCompletion` then skips `reviewReady` (:694-696).

## What the diagnosis test shows

`go test ./hub/internal/store -run 'ReviewConvergenceDiag' -count=1 -v`. The
fixture has `a1` verification-owned (for the incident's pending-verification
criteria) and `a2` reviewer-owned (for `a11`). Every form below uses two
general rounds, blocker `b1` recorded in round two on B, `b1` passed by
structured focused verification on C, the receipt for C saved, then a plain
lead accept of C.

| Round-two blocker form | `a2` in round two | Plain accept of C after focused `b1=pass` |
| --- | --- | --- |
| Regression, no criterion (the #15340 shape) | partial | refused: `conflict: review convergence: criterion a2 has not passed; a partial with no blocker is settled by focused verification naming it in criterionIds` |
| Regression, no criterion | fail | refused: `conflict: review convergence: criterion a2 has not passed` |
| Regression, no criterion | pass | accepted |
| Cites `a2`, new in round two (filed as a finding) | fail | accepted |
| Cites `a2`, carried from round one | fail | accepted |
| Neither criterion nor regression | partial | round-two RESULT refused: `conflict: review convergence: blocker must cite failed criterion or regression` |

Follow-on cases:

- `TestReviewConvergenceDiagPartialRegressionSettledInOneFocused`: one focused
  request naming `blockerIds:["b1"]` and `criterionIds:["a2"]` on C, both
  passed, is accepted without the owner.
- `diagBlockerOnlyFocusedRefused`, the 09-29 order of steps: after a
  b1-only focused pass on C, C cannot be settled any more.
  A criterionIds-only request on C is refused with `conflict: review convergence:
  rebase verification needs a converged review; name unresolved blocker IDs
  instead, and round-two partials in criterionIds (cause: unresolved blockers
  on accepted candidate)`, because `b1` is still open on B. Naming `b1` again is
  refused with `conflict: review convergence: focused verification must name
  distinct unresolved blocker IDs`, because it already passed on C.
  `TestReviewConvergenceDiagBlockerOnlyFocusedNeedsOwnerAccept` shows only
  owner-decision and owner-accept remain for C.
  `TestReviewConvergenceDiagBlockerOnlyFocusedNewCandidateSettles` shows a
  different commit D escapes: `b1` is open again on D, so `b1` plus `a2` in one
  request is accepted.
- `TestReviewConvergenceDiagFailedRegressionCannotSettle`: with `a2` `fail`,
  `criterionIds` cannot name it (`conflict: review convergence: criterionIds must
  name distinct round-two partial criteria that are not verification-owned`).
  No focused request maps the regression's pass to `a2`, on any candidate, so
  only owner-accept is left.

## Verdict

**Partly fixed.** The exact #15340 shape (regression blocker without a
criterion, criterion `partial`) no longer *has* to cost an owner-accept. Since
`67d552cd` (10-07) the lead can name `b1` and `a11` together in one focused
request with the exact receipt, and the accept refusal text points to
`criterionIds`. Two gaps remain, and both still force an owner-accept:

1. **Failed criterion with a regression blocker that cites no criterion.** The
   hub records it (:441-446, completeVerdicts at :1140 checks only the verdict
   values). Nothing can then turn that criterion to `pass`: the focused mapping
   needs `b.Criterion` (:547, :557), and `criterionIds` takes only partials
   (:988). This is the original claim r1, and it still reproduces.
2. **Partial criterion after a blocker-only focused pass on the same
   candidate.** This is exactly what the lead did on 09-29 (#15338). Once `b1`
   passed on C, settling `a11` on C is refused (:1005-1017 and :1018-1023).
   The only ways out are an owner-accept or a new commit. The lead prompt
   (`client/team-examples.js:122`) says focused verification names "the exact
   candidate, fix and blocker IDs" and does not mention `criterionIds`.

## Fix options for a later bounded bug order

Both options leave the owner-accept route as it is.

**Option A (recommended): map a passing regression blocker to the criteria it
blocks, and refuse a failed criterion that nothing cites.**

- `hub/internal/store/review_convergence.go`: in the general RESULT path,
  after the blocker and finding checks and before the round is saved
  (:1140-1250), refuse a round where a reviewer-owned criterion is `fail` and no
  saved blocker or finding cites it. The refusal names the criterion and says to
  set `criterion` on the blocker. A regression may also cite the criterion it
  breaks, as `evidenceFinding` already allows.
- `hub/internal/store/review_convergence_test.go` and the diagnosis test:
  change the "regression blocker, criterion failed" row on purpose.
- `docs/message-broker.md` (:527-532), plus the reviewer prompt
  `client/team-examples.js:95` with its mirror `docs/team-examples.md`: say a
  regression that breaks a criterion names that criterion.
- Proposed criteria: (a) a round-two RESULT with a reviewer-owned criterion
  `fail` and no blocker or finding citing it is refused with a reason naming the
  criterion; (b) a regression blocker citing `aN` that passes focused
  verification lets a plain lead accept succeed with `aN` passed (store test);
  (c) existing review convergence tests pass, or any change is explained;
  (d) the reviewer prompt and its mirror doc say it, and the mirror test passes.
- Size: 4-5 paths, so Planned rather than Small. Risk: a reviewer who marks a
  criterion `fail` without a blocker today gets a new refusal.

**Option B (smaller): let `criterionIds` settle a partial on a candidate whose
blockers are all resolved there.**

- `hub/internal/store/review_convergence.go` (:1005-1017): when the request
  names no blocker IDs but has `criterionIds`, and no blocker is open on that
  candidate because earlier focused records passed them there, check
  convergence on that candidate (with the named partials settling) instead of
  routing it as a rebase of the reviewed one.
- `hub/internal/store/review_convergence_test.go` and the diagnosis test:
  `diagBlockerOnlyFocusedRefused` then succeeds on C.
- `docs/message-broker.md` (:587-606): say a partial can be settled after its
  blockers pass on the same candidate.
- Proposed criteria: (a) after a blocker-only focused pass on C, a
  criterionIds-only request on C settles the round-two partial with the exact
  receipt, and a plain accept of C succeeds (store test); (b) the rebase and
  `treeDiffers` refusals for a changed candidate with open blockers are
  unchanged (existing tests); (c) the docs describe it.
- Size: 3 paths, Small. It closes gap 2 only. Gap 1 needs Option A.

A prompt-only stopgap for gap 2 (the lead names round-two partials in
`criterionIds` on the same focused request as their blocker IDs) would touch
`client/team-examples.js` and `docs/team-examples.md`. Recorded here as a
note, not a recommendation, because it repeats a rule the hub could enforce.

## Limits

- The test uses the store API with fake messages in a temporary SQLite store.
  It does not replay the incident's actual envelopes, and it does not cover
  findings `f1`-`f6`, which did not affect the refusal.
- The incident's 09-29 hub was at a commit before `67d552cd`, so the
  criterionIds route was not available to that lead. Gap 2 is about what the
  same steps would do today.
