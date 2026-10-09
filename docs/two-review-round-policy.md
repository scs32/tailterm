# Two review rounds and release disposition

Owner-approved policy, September 14, 2026. Feature `wi_74c050c73f7ff0b4`
revision 1; owner intake #6516, bounded implementation order #6518,
database-handler saved confirmation #6519. Related execution Feature:
`wi_618c8ff87e6b8061`.

Review-convergence source work: wi_3d6a4e3d1bf99a08, order #11569, assignment #11656.
Status: transactional source enforcement under validation; installation and saved acceptance remain separate. This document is an operating policy and implementation
contract, not evidence that the server already enforces it.

The owner's proposal was “you get 2 code reviews to get it right because we're
shipping it after 2.” The agreed rule is two full general review rounds followed
by a concrete lead-owned release disposition. Required checks and unresolved
reproducible defects remain release gates.

1. Start review on a frozen candidate commit/tree, acceptance scope and passing
   required checks. Persist item, revision, work order, cycle, round, reviewer
   identity, timestamps and evidence. Inline feedback on unfinished code does
   not constitute a completed full review; a full review cannot be renamed to
   evade the limit.
2. Round one produces one consolidated blocker list. Each blocker has a stable
   ID, violated requirement or reproducible defect, evidence, severity, fix
   owner, status and verification evidence. Preferences and unrelated
   improvements become linked follow-up items.
3. Round two checks the fixes and regressions without expanding scope or
   reopening preferences. Newly discovered real defects remain visible.
4. After round two the lead owns exactly one next disposition: release when
   checks pass and blockers are resolved; a focused fix and verification;
   explicit scope reduction with acceptance mapping; or an evidence-backed
   release block with owner, next action and deadline or resume condition.
   There is no third general review or additional approval tour. Focused
   verification references specific blocker IDs and the changed candidate.
5. Retries, correction commits, new candidate hashes, restarts and reassignment
   preserve the review count. Inside one review stage, scope revisions preserve
   the same two-round cap; no new cycle or owner override resets the counter
   (owner decisions #11650/#11651). Only a new owner order opens a new stage with
   its own two rounds; see [Review stages](#review-stages). Existing qualified
   releases keep moving; historical reviews without adequate evidence remain
   unknown.

The lead owns delivery and release disposition. The database handler owns native
records, source links, revision checks, retry receipts and saved acceptance.
The item implementation team owns typed storage, API/CLI transitions and the
existing item/Queue presentation. Independent QA verifies the contract.

Durable enforcement belongs in existing operational records, delivery and phase
ownership infrastructure. The server must reject invalid transitions; Board
prose cannot substitute for committed state. Existing scheduling handles missed
deadlines. This work does not introduce another watcher or activate AIV/MCP.

Acceptance requires tests proving:

- A third general review is rejected; retries and concurrent writes cannot
  double-increment or reset the count or confuse candidate provenance.
- Restart and agent replacement preserve rounds, blockers and disposition.
- Completing round two creates exactly one lead-owned next action.
- Real unresolved blockers and failed required checks prevent release; passing
  checks and resolved blockers permit release without a third review.
- Scope reduction retains requirement mapping; subjective follow-ups do not
  block release; new real defects receive bounded verification.
- Partial or legacy records display unknown and the UI agrees with committed
  native state.

Record implementation, independent verification, installation and acceptance
separately. Keep the Feature open until the handler saves verified completion.

Typed transitions use `tt send --review-file PATH`, or `review` in an envelope
file. See [the message contract](message-broker.md#review-convergence).
Criteria are frozen as contiguous a1..aN at the first linked ASSIGN per scope.
The lead also freezes any criteria designated by the order or plan for independent
verification as `verificationCriteria` on ASSIGN and REVIEW. The reviewer records
`pending-verification` for exactly those IDs, rather than partial or pass. The
database-handler-imported eligible receipt judges them for the exact candidate,
scope and assignment. Other criteria retain the reviewer pass/fail/partial gate.
An item with verification-owned criteria needs that receipt even if older
enrollment says verification was optional. Existing completed rounds and verdicts
remain immutable; their owner-accept route is preserved without a third round.
A revision-checked title/description update permits a new scope snapshot, but
never another general review beyond two inside the same review stage. Follow-ups are native open work items
with source-message provenance and a parent relationship in the review ledger;
they remain outside the delivery queue pending deliberate triage.

### Review stages

Bug `wi_5225af9140ef7a19`, owner order #30603. The policy above was written for
one build per item. Staged orders (measure, then design, then build) run several
on one item, and a findings stage that used both general reviews left the later
build with no review and no way to reuse a1..aN.

A review stage is the run of scope revisions worked under one owner order. A
scope revision opens a new stage when the database handler's saved scope
confirmation for it names a different owner order message than the stage before
it. A scope revision with no saved confirmation, or one confirmed under the same
order, stays in the current stage. The first saved confirmation of an item names
its first stage, so it opens no new one. The stage opens at that scope
revision's first ASSIGN: until then an open review of the current stage still
takes its result, and afterwards the earlier stage takes no further result.

The stage start is recorded, not recomputed (bug `wi_44b17e10c6450225`, owner
order #31258). When a scope revision's first ASSIGN is saved, the hub reads that
scope's confirmation once, decides the stage and stores it in the item's review
ledger as a `stages` entry: stage number, order message, scope revision and the
ASSIGN's sequence. Every later transition, the done check and every reader take
the boundary from that entry. A refused ASSIGN stores nothing.

The order must be owner-written. The check is made at that first ASSIGN, the one
place a stage is created (`openReviewStage` in
`hub/internal/store/review_convergence.go`): the order message must carry no
agent identity and must not be a system message, the same rule as an owner matrix
approval. A scope confirmed under a linked message that an agent wrote stays in
its stage, and the build meets the usual "third general review refused". To
recover, the owner posts the order and the handler confirms a new scope revision
under it. The check is on the order's authorship because a stage grants two more
general reviews; the handler's confirmation alone is not the control.

A first stage whose scope had no confirmation at its ASSIGN is recorded without
an order. It is named once, at the next scope revision's first ASSIGN, from the
earliest confirmation among its own scope revisions; if it has none, it takes
that scope's order and stays one stage. A recorded order, and any stage after
the first, is never rewritten.

The two-round cap, the frozen verdicts and the no-reclassification rule,
blockers, focused verification, follow-up IDs and acceptance are all evaluated
inside the current stage. A new stage starts at round one with its own a1..aN,
verification designation and blocker IDs. Earlier stages stay in the ledger
unchanged, each round with its scope revision; they neither block nor satisfy the
new stage's acceptance. Inside one stage nothing changes: a third general review
and a reclassified completed verdict are still refused.

The key is the owner order, not the scope revision, because the scope revision
also moves on every revision-checked title or description edit inside one build.
Keying on it would hand two fresh reviews to any mid-build amendment or scope
reduction and reverse point five. The owner order is what starts a stage. The
alternative, requiring a new linked item for each staged order, was not chosen:
it keeps the cost that prompted this change (question #29943) and splits one
item's history across several.

Limits. A new owner order needs its own scope revision: the handler records the
order in the item with a revision-checked edit, then confirms that scope. An
order confirmed on an unchanged scope revision opens no stage. Unknown legacy
history still gets no fresh count and records no stage. A confirmation saved
after a scope revision's first ASSIGN opens, moves and erases nothing, whichever
order it names: the stage that scope was assigned into is already recorded, and
the next scope revision under the new order opens the stage. A reviewer of any
stage still cannot be the item's independent verifier.

Ledgers saved before stages were recorded. A recorded ledger with two or more
scope revisions and no `stages` entry has its stages derived once, by the
earlier rule (the earliest confirmation of each scope revision), and the next
saved transition stores them; after that they do not move. Such a ledger with
three or more stages is stored as two, everything before the latest boundary
being one earlier stage: the count against the limit is the same and only the
stage number shown is lower. Readers do not derive, so until its next
transition a two-stage ledger of this kind still shows its summed total.

Totals are per stage. `tt message-checks --summary` and the Board's Delivery row
show the current stage's review count against the limit, with earlier stages
named but not summed: `stage 2: reviews 1/2 (earlier stages: 2)` and
`Stage 2 · Reviews 1/2 · Earlier stages 2`. The queue summary carries the
recorded stages so the Board can make that split. An item with one
stage reads as before, `reviews: 2/2`. Follow-ups stay a total across stages:
they are open work items whichever stage filed them. The usage report labels an
ASSIGN "corrections" only when a completed round of its own stage left
blockers. The helper's round label and the usage report's "review round N" key
are not stage-aware yet.

### Scope and legacy reconciliation

An open general RESULT uses the request's frozen criteria, candidate and exact
reviewer run, even if the item description changes meanwhile. The lead must
ASSIGN the current revision before acceptance. If those criteria are identical,
the existing verdicts remain usable without consuming another round. Round two
may explicitly resolve a blocker whose criterion was removed or changed by a
revision-checked scope edit: name it in `blockerIds`, put the scope-change reason
in `fix`, and attach evidence. This records a scope resolution, not a passing
verdict for a removed criterion. Retaining or silently dropping it does not
permit acceptance.

A round-two criterion failure stays failed when its finding is demoted into a
held-for-triage follow-up. Its finding ID can be named by the existing exact
focused REQUEST/RESULT path. Acceptance needs that verified fix on the exact
candidate; the general verdict history is never rewritten. Encoding a criterion
failure under `findings` also requires a failed verdict. Improvements unrelated
to failed criteria should omit `criterion`.

For native typed reviews predating enforcement, the lead or owner can post a
NOTICE with review metadata `mode: "reconcile"`, `legacyRequests: [SEQ, ...]`, a
source-backed explanation in `fix`, and Evidence. The hub requires every linked
native REVIEW sequence in chronological order and reserves all those lifetime
slots. It copies each immutable source candidate and criteria, and binds its
original reviewer to the current available run by default; it records the reconciliation
NOTICE and author/run alongside the source requests. Unknown legacy verdicts are
not inferred: each imported request needs a structured RESULT reattesting that
same frozen candidate and criteria, in round order. This completes an existing
round, rather than allocating a fresh general REVIEW. Ordinary acceptance and
Done checks then apply. Replays retain the same ledger. A subset, missing exact
source fields, unavailable reviewer without an explicit replacement, or more than two historical
requests is refused, with unknown history retained; there is no cap override.

When the original reviewer is unavailable, the reconciliation NOTICE may explicitly
provide `legacyReviewers: [{requestSeq: SEQ, reviewerId: ID, reviewerRun: RUN}, ...]`.
Every binding must name a distinct imported request and an available current exact
run on the same project. The ledger retains the original source recipient in
`sourceReviewerId` and the native request, while recording the replacement binding
and reconciliation author/run. The replacement reattests the same frozen candidate
and criteria on that existing request. Closure or replacement never resets the
round count. An unavailable original reviewer without an explicit binding remains
a refusal.

Sequential focused fixes on different commits require exact verification on the
final accepted candidate. A blocker passed on an earlier commit may be named again
in a focused REQUEST on the final commit. Request eligibility and acceptance share
the same candidate-specific unresolved-finding projection. Earlier evidence is
retained, but does not silently clear failures on an unrelated commit. After all
required fixes are verified on the final candidate, acceptance and Done can
proceed without a third general review.
