# Structured audit and AIV implementation plan

Planning revision 2 — September 8, 2026. Owner recommendations approved in #445;
handler record #454 activates A1 only. Later implementation/release orders remain
separate gates.
Feature `wi_abc84eb23688d903`; planning order `source-426-plan-1`, activated by
owner dispatch #433 and lead #435, recorded by db-handler #437 at item revision 3.
Owner: lead. Database/audit owner: db-handler. Planning was accepted in #442.
Owner #445 approved the recommendations; db-handler #454 recorded decisions D1–D6
and activated A1. The roadmap remains in progress. Later slices are proposed orders
and do not become active merely because their design defaults are approved.

Baseline: Tailterm `6abd105` on `tasks-hub`, following deployed application
`955bf43358c0d600028295f5466275cbae9e4714`; working tree was clean before this
planning-only document. AIV checkout HEAD was confirmed as
`5089cecd43971756cdc7a0889da48249b5e47d4e`, matching the earlier research. Inspection
was read-only; no AIV DB/service/provider calls, runtime changes or deployment.

Design inputs: [audit specification](work-item-audit-spec.md),
[AIV source research](research/aiv-project-agents-proposal.md),
[current project workflow](project-work-items.md) and [release inventory](handoff.md).
This plan turns those proposals into assignable slices; it does not redefine the
completed instruction/spec feature `wi_207f20d6eefcfa09`.

## Recommended sequence and decision ledger

Build typed message links and recoverable posting first. Add intake/correction
history, clients and consistent exports before requiring context. Add typed orders
and evidence next. Only then attach exact, retryable AIV evidence through the
handler. The AIV contract extension can be developed independently once its own
recorded order exists, but cannot certify the Tailterm pilot before both sides
meet the binding contract.

| Decision | Recommendation and consequence | Owner disposition / gate |
| --- | --- | --- |
| D1: messages before an item exists | Explicit temporary Intake; preserve the original message and atomically attach the eventual item. Strict mode rejects work claims without a link but still accepts intake. Alternative: require a pre-existing intake feature, which needs a separate bootstrap path. | Approved #445/#454; before intake UX/required-mode implementation |
| D2: project-wide messages | One ordinary, ongoing Project coordination feature created by the handler, for introductions and genuine operational coordination. Actual operational work still needs bounded orders; unrelated development gets its own item. Bootstrap remains Intake under D1. | Approved #445/#454; before automatic coordination-item creation |
| D3: links and cross-project work | Exactly one primary item and up to 15 related items. Initial build supports same-project primary only; later cross-project context requires an existing dispatch or explicit attributed handler/human linkage. Text mentions and replies never grant linkage. | Approved #445/#454; primary semantics before A1; broader links before A2 |
| D4: rollout enforcement | Observe mode first, with explicit capability discovery and visible unlinked state. Enable required mode only after TailOS, Mini, Air and active supported clients pass compatibility checks. No automatic deadline-based switch. | Approved #445/#454; observe compatibility assumption before A1, activation before B2 |
| D5: attribution and access | Preserve the current trusted workspace; label agent identity as a claim under the shared credential. Handler-only agent database use remains a workflow rule. Do not claim authenticated per-agent enforcement. Scoped credentials are a separately designed feature if the owner requires that stronger boundary now. | Approved #445/#454; before exposing new mutation interfaces/AIV credentials |
| D6: first AIV acceptance target | An isolated synthetic Go module representing one logged Tailterm issue; native tools execute against an exact checkout. Record repository/commit/snapshot/check/environment and measured population. Defer JavaScript assurance, DSH hooks and automatic test interception. | Approved #445/#454; before the AIV pilot order and any MCP configuration |

D1–D4 define product behavior; D5 defines the trust boundary; D6 selects pilot
scope. These recommendations were explicitly approved in owner #445 and recorded
by db-handler #454. Any subsequent changed decision must update this document and
affected orders before implementation; workers do not infer changes independently.

## Current implementation boundaries that matter

- [Message types](../hub/internal/api/types.go) have text/reply/sender/project but
  no item context or posting retry key. [Server routes](../hub/internal/server/server.go)
  delegate posting to [Store.PostMessage](../hub/internal/store/store.go).
  `insertMessage` also emits message events and implements direct-human resume of
  an online retired recipient. New posting/retry paths must preserve that behavior
  and must not emit a second resume/event on request replay.
- [Work-item store](../hub/internal/store/work_items.go) already performs creation
  and dispatch request deduplication, source validation and revision checks.
  Dispatch calls `insertMessage` inside its transaction. Ordinary posts and dispatch
  must share link insertion logic; a side write after dispatch would break atomicity.
- [Migration](../hub/internal/store/migrate.go) preserves existing tables and has
  item change/dispatch/request records. Reuse that authority; new message receipt
  keys need operation/project/actor scoping rather than borrowing a create key.
- [CLI posting](../hub/cmd/tt/main.go), [argument normalization](../hub/cmd/tt/coordination.go)
  and [hub client](../client/hub-client.js) are distinct entry points. Server
  validation remains authoritative. New clients must not silently retry a rejected
  linked request without its links against an old server.
- [Cached hub reads](../client/cached-hub-client.js) assume message bodies are
  immutable and fetch only newer message sequences. Later link corrections to an
  old message need a separate audit-change cursor or a visible-window refresh;
  new-message polling alone cannot update them. Preserve original immutable text.
- [History export](../client/task-history.js) currently builds version 1 by separate
  client requests. It cannot promise a transactionally consistent audit export.
  Add a server materialized snapshot artifact and keep legacy export compatibility.

## Stage ownership, contracts and exit evidence

Decision IDs are D1–D6; implementation slice IDs are A1/A2/B1/B2/C1/E1/E2.
Names below are proposed lanes, not active assignments. Lead owns shared API
contracts, migration coordination and integration. API owns store/server behavior;
UI owns client rendering/state; QA owns isolated acceptance. db-handler owns all
live work-item administration. Only resume a worker with a recorded bounded order;
use explicit file ownership or isolated worktrees for shared-file overlap.

| Slice | Dependency and files/artifacts | Contract and acceptance |
| --- | --- | --- |
| A1: same-project primary links and posting receipts | D3/D4/D5; lead: `hub/internal/api/types.go`, proposed `api/message_audit.go`; API: `store/migrate.go`, `store/store.go`, `store/work_items.go`, proposed `store/message_audit.go`, `server/server.go` and isolated tests | Optional typed context in observe mode, atomic ordinary/dispatch links, message request receipt replay. No typed order tables, correction API, strict switch or client UI yet. Detailed first order below. |
| A2: intake, related links and corrections | A1 + D1/D2/D3; API store/server and proposed `api/message_audit.go` additions; lead owns final schema/contract | Resolve intake atomically with item creation/selection; append-only attach/detach/reclassify and primary-change events with link revision CAS. Add audit-change cursor and evidence-backed historical backfill. Cross-project links require validated dispatch/explicit linkage. |
| B1: CLI and Board context, versioned export | A2; UI: `client/board-view.js`, `hub-client.js`, `cached-hub-client.js`, `task-history.js`, relevant styles; CLI: `hub/cmd/tt/main.go`, `coordination.go`, proposed `message_audit.go`; API: export/capabilities routes | Composer primary chip and explicit intake, deliberate reply context, work-order message reference, receipt recovery; cached correction updates; immutable export artifact from one SQLite read snapshot. Chromium/WebKit + mixed-client fixtures. |
| B2: required-context rollout | B1 + D4 and verified host/client inventory; lead deployment + docs, API policy/capability validation | Required context rejects unlinked work with actionable errors; explicit intake remains possible. Human UI does not lose drafts. Record policy revision, exact versions and rollback criteria. Unreachable clients remain blockers. |
| C1: typed orders, assignments and native evidence | A2; B1 timeline/UI extension when ready; lead API/schema; API proposed `work_orders.go`, `work_evidence.go`; UI timeline rendering | Versioned order snapshots, explicit assignment transfers, exact-run results, accepted/stale/rejected evidence, per-target deployment receipts. Typed-order capability is separate from item-link enforcement; preserve earlier `workOrderMessage` references. |
| E1: exact AIV submission contract | D5/D6 + separate AIV repo order; proposed AIV `src/verify.ts`, `src/serve/mcp.ts`, new DB migration and isolated run-submission tests; no Tailterm worker edits AIV without explicit ownership | Require explicit repository/snapshot/extractor/commit/check/environment; durable operation key and lookup; same request recovers same receipt, changed payload conflicts. Reuse core verifier and audit, not a second run ledger. |
| E2: handler adapter and native Go pilot | C1 + E1 + decision D6; new adapter package/path chosen in its order, Tailterm mapping/outbox tables and isolated integration fixtures | Handler-only work administration, host-session attribution, durable submission/reconciliation, AIV outage recovery. Native shell/editor preserved. One logged issue traced from source message to accepted exact evidence and completion. |

These names replace ambiguous numbers: B1/B2 correspond to client/enforcement work
in the audit spec; E1/E2 implement exact evidence binding/retry safety from the
older AIV proposal. C1 can proceed alongside B1 after contracts stabilize. B2 must
advertise only capabilities that are actually installed; typed orders cannot be
required merely because item linkage is required.

## First implementation order proposed for activation

Active key `wi_abc84eb23688d903-a1`; activated in #446/#454. Root owns shared API
and integration; api owns store/server/migration; cache-qa owns independent HTTP
acceptance under #449/#450 and fixed contract #452/#453. Objective: persist a verified
same-project primary item relationship on an ordinary message and recover a
committed post without duplicating its events. Existing validated cross-project
work-item dispatch remains supported and emits its owning-item relationship;
A1 does not add arbitrary cross-project ordinary posting. Candidate implementation base must be a clean,
identified commit after this planning document; recheck shared ownership then.

Inputs: existing `PostMessageRequest` plus optional `workItems` containing exactly
one `{itemTaskId,itemId,itemRevision,relationship:"primary"}`, optional
`workOrderMessage:{taskId,seq}`, and caller-supplied `requestId`. Supplying typed
context requires a request ID; legacy requests without both remain accepted in
observe mode and visibly classified as unlinked. Explicit `auditKind` values and
strict rejection arrive with A2/B2, so A1 must not invent silent intake semantics.

Outputs: the existing committed message with additive creation-time context and
an operation receipt identity. Add a narrow receipt recovery read for the same
project and attributed actor scope. `itemRevision` pins a verifiable existing
revision; if historical contents cannot be validated from retained history, do
not accept a fabricated revision. A1 may require the current item revision for
new contextual posts, while exact replays return their original receipt.

Tables: additive `message_work_item_links` and `message_post_requests`, with
foreign-key/project validation, unique primary linkage and unique request scope.
Record original context on insertion. A2 will introduce link corrections and
revisioned projections; avoid a public in-place update API in A1. Retain immutable
creation context so later correction history can distinguish original from current.

Transaction: check existing receipt by key/hash before rejecting a replay because
of later closure or revision changes; otherwise validate task, sender, recipient,
reply, item project/revision and order-message project. Commit message, item link,
existing event/resume effects and receipt together. Dispatch supplies its item's
verified revision and owning project directly through the same transaction helper,
including the already-supported cross-project dispatch path. The helper must not
confuse item ownership with the destination board's project. A failed insert
or validation leaves no message, link, event, resume transition or receipt.

Explicit exclusions: related links or new cross-project linking/posting operations
(existing dispatch is preserved), automatic coordination feature,
intake resolution/corrections, CLI/UI flags, strict policy, typed orders/evidence,
AIV, deployments and unrelated cursor fixes. This slice is reviewable source plus
isolated tests; no production schema migration without a separate release order.

Acceptance owned by QA using synthetic tasks/items/agents and temporary SQLite:

1. Legacy posting/replies preserve shape, visibility, event sequence and lifecycle.
2. A valid primary post and dispatch return a real relationship with the correct
   owning project and revision, including existing human cross-project dispatch;
   malformed/missing items or foreign ordinary-post item/reply references fail.
   Cross-project human dispatch keeps source item ownership and target recipient,
   and exact dispatch receipt replay remains unchanged. Agent-authored
   cross-project dispatch must still be rejected.
3. Exact replay, concurrent duplicate submissions and lost-response recovery yield
   one message/link/event set and the same original receipt. Changed body, target,
   context or order under that key conflicts. Requests declaring different actor/project scopes cannot recover another scope's
   receipt merely by guessing its key. This tests scoping under the existing trust
   model, not cryptographic prevention of forged actor labels.
4. Replaying after closure or later item revision returns the original receipt;
   a genuinely new closed-project request still fails. Human-retired-recipient
   resume occurs once; replay never wakes a newly retired or different run again.
5. Failure injection between each write rolls back every coupled row. Reopening
   the temporary database preserves links/receipts and foreign-key integrity.
6. An old-schema fixture migrates without rewriting messages/authorship, item
   histories, task status, retirement, profile data or cleanup receipts. No test
   uses live project/vault/relay state.

Suggested commands: `go test ./internal/store ./internal/server ./cmd/tt` and
`go vet ./internal/store ./internal/server ./cmd/tt`, followed by the relevant
existing isolated coordination/work-item browser regressions if their exercised
wire behavior changes. Tests must cover state/transaction results, not merely
string presence. Deliver source commit, owned-file diff, commands/outcomes and
failure/replay evidence to lead and db-handler. Exit: accepted A1 only; roadmap
and remaining slices stay in progress.

## Cross-stage consistency and failure handling

A2 exposes an audit change stream with its own cursor, bounded query and explicit
message identity. B1 overlays those current-link changes onto cached immutable
message bodies. A lock/profile change invalidates pending responses exactly as
existing cached hub reads do. Fixtures must correct a currently visible older
message while no new message arrives, then repeat after offline reload and scope
change. Do not reuse the message read cursor as the audit cursor or change agent
retrieval status to represent execution.

B1 export creation materializes all original messages, current projections,
immutable changes, orders and evidence inside one consistent SQLite read snapshot.
The resulting artifact has version, creation time, exact stream cutoffs and digest;
browser pagination reads that artifact, not live mutable projections. Test a link
correction and item/assignment status transition between download pages. Exports
must remain internally consistent and exclude post-snapshot changes. Expired
artifacts fail explicitly; a new export is a new snapshot, not a silent continuation.

Mutation failures retain drafts. The browser does not automatically queue writes.
A received operation key permits an intentional exact retry/recovery; it does not
turn arbitrary edits into queued mutations. Handler outage preserves explicit
intake and reports a dependency once; no bypass, duplicate handler or busy polling.
Conflicts require read/review through the handler and a new intent/key where needed.

## AIV contract work that cannot be skipped

The inspected AIV [recordRun implementation](/Users/stephenspeicher/projects/aidevelopment/aiv/src/verify.ts)
accepts optional `commitSha`, uses the latest snapshot otherwise and allocates a
new run identity. Its [MCP schema](/Users/stephenspeicher/projects/aidevelopment/aiv/src/serve/mcp.ts)
does not expose commit/check-version selection or a durable submission key. Passing
only a repository name therefore cannot establish exact binding/retry safety.

E1 must select and validate an explicit repository ID and snapshot ID together with
commit/extractor identity, retain check-version and environment digests and store
a canonical request fingerprint and response in the same transaction as the run.
An exact receipt lookup must reconcile a response lost after commit. Same commit
with a different extractor snapshot is a distinct binding. Empty executed
population cannot produce a pass; submitted verdict is an attributed claim,
separate from derived/calibrated status. Extend the core and MCP wrapper together.

E2 stores Tailterm-to-AIV mapping by service/repository identity, not checkout
basename. Record host/absolute checkout/commit separately. An outbox operation
retains exact target/binding/payload/key and pending/submitted/reconciled/failed
state. Network failure does not trigger a second check execution or a blind
`record_run` call. Findings link to Tailterm items; their statuses are not mirrored
bidirectionally. Pilot checks run in isolated repos/databases/native sessions;
current JS extraction gaps and multi-module Go materialization remain explicit.

No MCP inventory, credentials or runtime configuration changes happen during this
planning order. A later adapter order must specify packaging, host installation,
actual callable-tool checks and rollback; native tool availability is not inferred
from research documentation alone.

## Release, rollback and acceptance gates

Each implementation slice ends with a source/evidence handoff, not an automatic
production update. A separate recorded release order names required targets and
component versions. Hub changes use TrueNAS middleware with the existing TCP
listener; do not touch Tailscale. CLI updates cover Mini and Air with atomic
replacement and a verified previous binary. Frontend updates target TailOS only
and both localhost previews, verifying every served manifest entry except hosting
configuration. Preserve live task/profile state and existing container rollback
semantics from the handoff.

Before migration: consistent SQLite backup, integrity check, isolated upgrade
fixture and an explicit old-binary/new-database compatibility test. Additive tables
remain on application rollback; do not destroy audit history. Downgrade from
required mode must be explicit and recorded before deploying incompatible clients.
If the old binary can write unlinked records, observe-mode rollback labels that
gap and reconciles it later rather than claiming continuous strict enforcement.
AIV migrations/backups and evidence receipts need equivalent isolated recovery
checks under their own deployment order.

Completion evidence identifies exact source and candidate commits, schema/policy
versions, target versions, command outcomes, measured populations, artifact hashes,
reviewer acceptance and unresolved dependencies. A successful upload, retrieved
assignment or stored AIV claim is not completion. db-handler records stage outcomes
and closes the roadmap only after its chosen acceptance scope is actually met.

## Planning handoff

Delivered artifact: this plan. Verification for planning is source-path/contract
inspection and document/link consistency, not a claim that proposed behavior was
tested. No product files were edited and no runtime/deployment action occurred.
Planning acceptance and owner decisions are recorded in #442/#445/#454. A1 source
and verification were accepted by lead in #480; see the
[implemented contract and evidence](message-audit.md). The hub-only deployment
ran under release order #483; its [receipt](releases/tailos-2026-09-08-message-audit.json)
records exact versions and checks. Later slices still require their own work orders. Return
results and changes through db-handler; preserve the roadmap feature rather than
marking it complete after the plan or A1 alone.
