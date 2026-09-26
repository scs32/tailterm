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
   preserve the review count. Scope revisions preserve the same lifetime two-round cap; no new cycle
   or owner override resets the counter (owner decisions #11650/#11651). Existing qualified releases keep moving;
   historical reviews without adequate evidence remain unknown.

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
A revision-checked title/description update permits a new scope snapshot, but
never another general review beyond two. Follow-ups are native open work items
with source-message provenance and a parent relationship in the review ledger;
they remain outside the delivery queue pending deliberate triage.
