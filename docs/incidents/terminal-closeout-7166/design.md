# Terminal closeout repair proposal

Bug wi_3043122c24f547e2@2; source/order #7166, handoff #7168. Diagnosis complete; implementation has not started. Installed hub source ba37ee7 is the inspected contract.

## Prevention

Use one shared transactional completion precondition in both UpdateWorkItem and UpdateWorkItemKeyed, for Bugs and Features. Before changing an open item to Done, reject when a current required item action remains unfinished or a canonical lead disposition is unresolved. Inspect outstanding obligations across item revisions so an earlier revision cannot disappear from this check. Return structured conflict details identifying exact delivery/action/obligation and expected versions. An already saved identical retry must return its original receipt before new-state checks; existing Feature narrative evidence and CAS remain mandatory. Do not increment item revision, append history, or change Queue on rejection.

The supported order becomes result -> typed acceptance/disposition -> explicit release-action result -> Done. Existing atomic acceptance resolves its automatic lead action. Completion does not falsely mark retained teams cleaned up or erase independent cleanup obligations. Reconcile legacy-unknown disposition explicitly; do not count unknown as accepted.

## Existing terminal-item reconciliation

Add a dedicated typed API/CLI operation rather than weakening normal lifecycle/revision validation. Its request pins request ID, current terminal item revision and scope, historical completion receipt, predecessor delivery/generation/epoch/result identity, canonical obligation/version, exact current lead agent/run, automatic action and explicit release action generations/epochs, and structured acceptance/release evidence references plus digests. All references must be loaded and verified by the server, never inferred from Board prose or accepted solely because a caller supplies a hash.

Within one transaction, validate the saved transition really was the expected open-to-Done revision without a scope change; validate the pinned historical item/result and current unsuperseded lead identities; validate the release and typed candidate/verification/result/acceptance chain with existing content/authority rules. If the historical typed chain is absent, accept explicit typed reconciliation records with full provenance and normal evidence validation as part of this dedicated operation; never synthesize passing QA from a Done label. The handler must supply the actual chain before implementation finalizes that request schema.

Atomically save an immutable reconciliation receipt and typed disposition, resolve only the two explicitly enumerated lead actions, and retain all predecessor/history/acceptance/release records. Preserve reader Done@2 and its original completion receipt. Reuse cleanup/retention producer rules where terminal acceptance requires them; reconciliation cannot claim cleanup occurred. Reject omitted/extra unrelated actions, changed scope, wrong predecessor/result, stale obligation version, replaced/retired lead, superseded generation, changed epoch, dismissed item, or conflicting acceptance. No broad allow-stale option, reopen, SQL repair, fake worker identity, or restart.

A matching retry returns the original immutable receipt even after later state changes. Reusing the request ID with changed bytes is a conflict. A partial validation failure leaves no new records, resolved actions, or item mutations.

## Bounded validation and handoff

Synthetic store and real API/CLI regression must reproduce the original accepted-result -> Done -> 409 trap against the pre-fix source. Validate both Done update paths reject that ordering atomically, and the normal disposition-first ordering succeeds. Seed the legacy terminal shape only in an isolated fixture; verify reconciliation preserves Done/revision/history, resolves both exact actions with native receipts, and cannot resolve another sibling. Cover same-request replay, changed-payload retry, wrong/stale/replaced identities and rollback on failed evidence validation. Include concurrent Done-versus-disposition attempts to establish serialization and no stranded new state.

Root implements in an isolated checkout after handler-recorded design scope. Lead independently qualifies the candidate; handler records all live reads/writes, assignment, evidence and saved acceptance. This design is not a completion or deployment claim. Other Queue and cleanup work remains with existing owners.
