# Work-item audit trail specification

Status: proposed structured-audit design, September 8, 2026. Source feature
`wi_207f20d6eefcfa09`; owner messages #396 and #403; handler work orders #399,
#408 and #410 (record revision 4). Lead owns this specification; db-handler owns
its durable record and completion tracking. The instruction changes in this
feature implement the immediate workflow. The storage, API, CLI, UI and AIV
changes below require subsequent recorded implementation work orders.

## Outcome and recommended decisions

Every piece of agent work should be traceable from the owner's request through a
bug or feature, a bounded assignment, the exact code and verification, acceptance
and deployment. Board messages need real database relationships, not just item
IDs embedded in text. Opening a feature should show this complete timeline;
opening any message should explain what work it belongs to and why.

Recommended defaults, for owner review before schema implementation:

1. A work message has exactly one primary bug/feature and at most 15 related
   items. Multiple unrelated assignments should be separate messages. Replies
   inherit a proposed context in the composer; the saved reference is explicit.
2. Permit a visible, temporary `intake` classification before a bug/feature exists.
   The handler attaches the eventual record through an auditable link operation.
   Never invent a feature merely to let someone report a feature.
3. Give each project one ordinary, handler-created **Project coordination** feature
   for introductions, roster recovery and project-wide housekeeping. Label it as
   ongoing, require bounded orders for actual operational work, and do not use it
   to hide unrelated development. Bootstrap messages remain intake until this
   feature exists. Historical messages remain explicitly `legacy_unlinked` until
   linked with evidence; do not fabricate historical intent.
4. Introduce server-validated links first, then require them for new work messages
   once clients are upgraded. Strict mode permits intake, but never permits an
   unlinked message to masquerade as an implementation assignment or completion.
5. Keep Tailterm authoritative for work items and conversations. Add AIV as a
   separately owned evidence service, initially accessed by the database handler
   through a narrow adapter. Keep native agents and their existing tools.

Decisions 2 and 3 are explicit exceptions to a literal requirement that every
message already have a bug/feature at send time. They preserve an audit record
without blocking intake or falsely attributing old conversations. If the owner
prefers no exceptions, the alternative is a handler-created intake feature before
project chat is enabled; that still needs a bootstrap path for a missing handler.
This specification recommends explicit intake rather than silently choosing that
policy. These decisions do not block writing or reviewing this spec.

## Current source and immediate workflow

[Message and request types](../hub/internal/api/types.go) carry project, sequence,
sender, recipient, reply target and text. They currently have no typed work-item
reference, work-order reference, message request key or posting run ID.
[Work-item types](../hub/internal/api/work_items.go) already carry revisions,
source message sequence and dispatch receipts. The
[store](../hub/internal/store/work_items.go) retains dispatch snapshots and request
receipts; [migration tables](../hub/internal/store/migrate.go) retain item changes.
A dispatch currently connects one item revision to a saved message, but ordinary
follow-up messages have no corresponding structured link. The proposed tables
extend these records rather than replacing them.

The immediate policy is instructional: all agent implementation, investigation,
validation and deployment requires a durable bug/feature and bounded work order.
All agent work-item list/get/create/update/dispatch operations go through the
actual database handler roster name. Intake and board/inbox/roster coordination
can establish the record first. An unavailable handler is an explicit dependency;
there is no direct agent CLI/API/database fallback. The handler records provenance,
assignments, scope changes and accepted results, verifies committed revisions and
confirms completion. Existing threads receive this policy on the board; a new
startup prompt cannot retroactively change a running native thread.

Humans can continue using Bugs/Features directly. The shared workspace credential
is not authenticated per-agent access control. Prompt rules do not prevent an
agent with that credential from calling the API. Do not claim mechanical role
enforcement until an independently scoped credential design is implemented.

## User flow

1. The owner reports a problem in Board. Choosing an existing item fills its chip;
   choosing **New intake** saves the original message immediately and labels it
   **Awaiting item**. No draft is discarded if the handler is offline.
2. The handler reads the intake, checks existing records, creates or explicitly
   selects a bug/feature, and links it back to the original message. It returns the
   verified item ID/revision and a work order. Missing critical details remain a
   visible intake dependency. Similar titles alone do not prove duplication.
3. The lead assigns a bounded part of that work order. The message carries the
   primary item, exact order revision and named recipient. It records owned
   artifacts, acceptance criteria and dependencies. The recipient's retrieval
   cursor remains independent from assignment status.
4. Workers reply with the same explicit context, source commit/worktree and test
   evidence. New findings go to the handler as intake or a scope-change request;
   they do not automatically expand the current assignment.
5. The verifier reports evidence for the exact candidate. The lead accepts or
   rejects it with reasons. The handler appends the outcome and updates item status
   with a revision check. Required deployment targets get distinct receipts.
6. The feature timeline shows request → record → order → assignment → result →
   verification → acceptance → deployment → completion, including retries,
   reassignments, rejected results, unresolved dependencies and link corrections.

## Interface and state ownership

| Record | Required identity and fields | Owner and rules |
| --- | --- | --- |
| Message | Existing `(taskId, seq)`, original sender/recipient/text/reply; `auditKind`, `requestId`, optional sender `runId`, typed work links and `auditRevision` | Hub commits content and creation-time context together; original content is immutable |
| Work link | Owning `itemTaskId`, `itemId`, observed `itemRevision`, relationship `primary` or `related`; optional `workOrderId`, `workOrderRevision` | Validated hub relationship; never inferred from text; project ownership derives from the item |
| Work order | New stable `wo_` ID, revision, item ID/revision, original source message, issuer, lead/worker ownership, scope, artifacts, acceptance checks, dependencies, status | Handler issues and revises; immutable revision snapshots; no duplicate item database |
| Assignment | Stable `wa_` ID, work-order revision, assignment message `(taskId, seq)`, named agent and optional expected run, dependency links | Lead assigns; supersession/transfer is explicit and retained |
| Audit event | Stable event ID and cursor, actor and attribution assurance, server time, item/order/assignment references, action, original source references, operation key | Append-only hub record; projections never replace original events |
| Evidence receipt | Stable `ev_` ID, item/order/assignment, candidate commit, repository identity, producer/verifier/run, check/command, environment, population, exit/result, artifact URI and digest | Hub retains declared facts and links; external artifact availability and verification status are separate |
| Deployment receipt | Exact commit/build/manifest digest, target, observed version, verification evidence, previous version, time, status | Lead supplies measured evidence; handler records per-target outcome |
| AIV binding | Explicit Tailterm project/repository identity, AIV service/repository/snapshot/extractor IDs, run/finding/checkpoint references and submission receipt | Adapter maps references; AIV owns its evidence and projection semantics |

Native thread IDs, Tailterm agent IDs, run IDs, tmux IDs, project IDs and Git commits
remain separate fields. A message's stored run is a point-in-time attribution,
not a live lookup of the agent's newest run. Record whether attribution is only
`shared_workspace_claim` or verified by a future scoped credential; do not present
it as cryptographic proof. No tokens, vault contents or private keys enter records.

`itemRevision` means the revision actually considered, not a floating pointer to
latest. New work orders and scope changes require current-revision checks. A late
result for an older order is retained as stale evidence and cannot satisfy a newer
order without explicit reviewer acceptance. Reading historical items never
silently upgrades their linked revisions.

## Proposed persistence and transactions

Use additive SQLite migrations. Suggested logical tables (final SQL belongs to
the implementation work order):

- Extend `messages` with `audit_kind`, creation `request_id`, `sender_run_id` and
  current link `audit_revision`. Keep the existing global sequence and task ID.
- `message_work_item_links` stores the current projection, keyed by message and
  item. Enforce one primary per message and no duplicate item reference. Keep
  source item project separate from destination message project.
- `message_link_events` appends attach, detach, reclassify and primary-change
  operations with before/after references, actor, reason, timestamp and expected
  audit revision. Removing a wrong link never erases who made it or when.
- `work_orders` plus immutable `work_order_revisions`; `work_assignments` plus
  immutable transition events. Existing item changes and dispatch snapshots
  remain authoritative for their current functions.
- `work_evidence`, `work_item_audit_events` and `audit_operation_receipts` retain
  typed evidence, causal references and recoverable request outcomes. Add a unique
  `(operation, project, actor-scope, request_id)` key and canonical payload hash.
  Link existing item changes/dispatches rather than copying mutable status.

Ordinary linked posting commits message, validated links, audit event and request
receipt in one transaction. Handler intake resolution commits item creation (or
validated existing-item selection), source link/correction, audit event and
receipt together through a dedicated resolve-intake operation. A failed operation
leaves the original intake message intact and no partial relationship. Dispatch
commits its existing receipt and message with the structured primary link in the
same transaction. Work-order creation and its announcement follow the same rule.

A repeated operation key with the same canonical payload returns the original
receipt, even if a later lifecycle/status change would prohibit a new operation.
A changed payload with that key returns conflict. Include normalized context,
recipient, reply and text in the posting hash. Never blindly repost after a lost
response. Recovery queries receipts by operation key. Snapshot queries and cursors
must distinguish messages, events, item revisions and assignment transitions.

Appending corrections is mandatory; deleting/recreating history is not an edit
mechanism. A database transaction/append-only API is useful audit integrity but
not tamper-proof storage against a database administrator. A later signed export
or externally anchored digest could provide additional assurance; ordinary hashes
only detect changes when a trusted prior digest exists.

## API, CLI and future tool contract

Proposed additions, not commands available in the current release:

```json
{
  "text": "Verification passed on the candidate commit.",
  "to": "agt_0000000000000001",
  "replyTo": 420,
  "requestId": "verify-result-operation-1",
  "runId": "run_0000000000000002",
  "auditKind": "work",
  "workItems": [{
    "itemTaskId": "tsk_0000000000000003",
    "itemId": "wi_0000000000000004",
    "itemRevision": 7,
    "relationship": "primary",
    "workOrderId": "wo_0000000000000005",
    "workOrderRevision": 2
  }]
}
```

The hub returns the committed message plus audit revision and operation receipt.
Missing items/orders, malformed IDs, duplicate references, a mismatched item
project, invalid reply scope and absent primary on `work` fail validation before
any insert. Stale link/order mutations return conflict with current revision;
unavailable storage returns an actionable failure without a success receipt.

Typed work-order fields are capability-gated until phase 3. During phase 2,
`workOrderMessage: {taskId, seq}` explicitly references the existing immutable
recorded order message; phase 2 validates item linkage and that message reference.
It does not require a not-yet-existing work-order table. When typed orders become
available, messages may use the typed ID/revision; supplied typed IDs must validate
and cannot silently fall back to message references. Preserve the original message
link when migrating an order to typed revisions.

Extend post/list/export API responses additively. Proposed `tt post --work-item
ID --item-revision N --work-order ID --order-revision N --request-id KEY` carries
context explicitly; repeated `--related-work-item` adds related references.
`--intake` is explicit. Context may be loaded from a recorded assignment, but it
must be scoped to the exact agent/run and visible in the resolved request. Never
use the most recent board message or another terminal's global default.

Handler-only workflow tools resolve intake, look up records, issue/revise orders,
attach/correct links, register evidence and reconcile completion. Other agents
post contextual messages normally; this is not work-item database administration.
Handler request replies can include the verified context needed to post without
requiring workers to call item lookup themselves. CLI/API/MCP validation must share
one hub implementation, rather than three independent policy implementations.

Cross-project dispatch preserves the owning item project and records the target
project/agent. A target project's work message may reference that item only through
an existing dispatch relationship or an explicit handler/human linkage operation
recorded with reason. A reply does not create cross-project permission. Item link
reads follow the existing trusted-workspace visibility boundary; introducing tenant
isolation would require a separate authorization design.

## Board, timelines and exports

The composer shows a compact primary item chip, related-item count and work-order
revision. Reply preselects the parent's context; changing it is deliberate and
saved, never inferred from edited text. Missing context shows **Choose work item**
or **New intake**. New-project introductions use the coordination feature when
available, otherwise the visible bootstrap intake route.

Messages show an item chip and intake/stale/corrected indicator where applicable.
Selecting a chip opens the item's timeline with paginated messages, order revisions,
assignments, results, acceptance and deployment receipts. Provide filters for
unresolved intake, work item, assignment and evidence state. Preserve current
recipient ownership, swarm visibility, cached reads and draft retention.

Download history adds a versioned audit section containing original messages,
current links, full correction events, order revisions, assignments, item change
references and evidence/deployment receipts. Export against an upper sequence
bound for each independent stream so concurrent activity neither duplicates nor
silently skips records. Bounds alone do not freeze mutable projections: produce
the export in one consistent SQLite read transaction across all streams and
projections, materialize an immutable export artifact, then page/download that
artifact. Never hold a transaction open across arbitrary browser page requests.
An alternative implementation may reconstruct every projection from immutable
revisions/events at the captured cutoffs; it must not read current mutable rows.
An isolated fixture must correct a link and change an assignment/item status
between pages: exported projections must match included history and exclude all
post-cutoff changes. Preserve legacy fields for older readers. Artifact hashes
and references travel in the export; do not embed secrets or full private terminal
transcripts. Mark unavailable or expired artifacts explicitly.

## Enforcement, failures and rollout

Distinguish these states in UI and tools: **stored** is a committed receipt;
**retrieved** is a delivery cursor; **started** is an agent's reported transition;
**executed** has a command/run result; **verified** is evidence with its scope;
**accepted** is the named reviewer's decision; **completed** is the handler's
revision-checked status update after required dependencies resolve. None implies
the next. Automated status projections cannot silently close an item.

Stage structured audit as `observe` then `required`. In observe mode, old clients
continue posting visibly legacy/unclassified messages; no silent link inference.
Advertise capabilities and per-project policy version. In required mode reject
old/unlinked work submissions with a specific `audit_context_required` or
`client_upgrade_required` response. Intake and receipt recovery remain possible.
The owner enables required mode only after real required host/client compatibility
checks. An unreachable host is a recorded rollout dependency, not successful
coverage. Existing messages and read-only closed-project histories always load.

Handler offline: save explicit intake, show the dependency, preserve drafts and
existing data; pause dependent work. Never automatically queue browser mutations.
A future adapter may retry its explicitly recorded pending operation by the same
key, with a visible receipt state; that is distinct from replaying arbitrary drafts.
AIV offline: retain native test receipts and a pending external attachment. Do not
rerun tests or create replacement agents merely to retry a receipt.

Migrate existing `sourceMessageSeq` and dispatch receipts only when the stored
project/message/item relationship validates; record migration provenance for every
created link. Do not parse text mentions into trusted history. Legacy rows keep
original authorship/time; item historical revisions unavailable from retained
records remain unknown. Take an isolated migration fixture and consistent backup
before production migration. Down-level application rollback must not drop audit
tables; disable strict mode explicitly if a compatible rollback requires it.

Close/retire/reconnect semantics, helper limits, run/thread guards, task-owned
panes and cleanup receipts remain unchanged. Closed projects reject new work and
mutations; historical audit remains readable. Later correction of a closed project
would require an explicit owner maintenance operation, not silent reopening.

## AIV groundwork

Use the [existing source research](research/aiv-project-agents-proposal.md), pinned
to AIV commit `5089cecd43971756cdc7a0889da48249b5e47d4e`, as a design input. This
spec does not re-certify a deployed AIV service or current native MCP inventory.
The [native CLI study](research/llm-cli-control-reference.md) supports preserving
normal native runtimes; runtime permissions and callable tools still need actual
host/thread verification in an implementation pilot.

| Reuse | Proposed handler integration | Prerequisite / boundary |
| --- | --- | --- |
| Repository/snapshot ledger and code context | Explicit project-to-repository mapping; context tools such as status, entity, impact and review brief | Stable repository ID, exact commit/snapshot/extractor, coverage gaps returned |
| Findings | Link AIV finding IDs to the owning Tailterm bug/feature as evidence | Tailterm owns workflow status; no bidirectional editable duplicate bug database |
| Runs, audits and checkpoints | Attach typed AIV receipts to a Tailterm evidence record | Exact snapshot/check/environment binding and durable submission deduplication first |
| Policy provenance | Store attributed policy references with a decision | AIV policy labels do not override owner instructions or runtime permissions |
| Evidence invalidation | Append stale/superseded evidence states after source changes | Preserve original result and reviewer decisions; never rewrite a prior pass as a new run |

A first adapter exposes project context, contextual message post, work-item
intake/order/evidence operations and receipt recovery to the database handler.
Bind project/agent/run from the native session rather than trusting arbitrary model
arguments; credentials stay in host configuration. Return structured committed
receipts or explicit conflict/unavailable errors. Other workers continue sending
work-item administration requests to the handler. Do not silently install MCPs,
change native configurations or replace native shell/editor/permission flows.

For external evidence writes, add a durable outbox row containing the exact
payload, target service/repository/snapshot and stable operation key. Track
pending/submitted/reconciled/failed, with retry count and last error. Completion
requires an external receipt or reconciliation lookup, not just sending a request.
The researched AIV `record_run` allocates fresh IDs and does not expose all exact
binding fields at its MCP boundary; automatic retries are unsafe until that
contract is extended. Do not claim idempotency from an MCP protocol request ID.

The initial evidence pilot should be a logged Go issue in an isolated repository
fixture. The research found JavaScript outside AIV's indexed extension set and
Tailterm has multiple Go modules; frontend coverage and exact snapshot
materialization need separate acceptance. Do not copy DSH tool interception,
regex-based auto-recording, synthetic user-message injection, global policy scope
or basename-based repository identity. Native hook automation and broader verifier
roles are later work, not requirements to record an honest first evidence receipt.

## Example audit chain

Synthetic example; IDs below are illustrative, not live records:

- Owner message `(project P, #10)` reports reconnect layout loss as intake `I1`.
- Handler operation `source-10-item-1` atomically creates feature F revision 1 and
  links #10. Its work order W revision 1 requires exact splits after reconnect.
- Lead message #12 assigns W to agent A/run R; assignment X records files and
  acceptance. Delivery cursor reaches #12; X is still only retrieved.
- A reports candidate commit C in #14 with command, exit status, fixture population
  and artifact digest E1. A separate verifier reports E2 against the same C/W.
- Lead #16 accepts E2. A deployment receipt proves public and Mini are at C, while
  Air is unreachable. F remains in progress with that required dependency.
- After Air verification, handler records E3 and updates F with its expected
  revision. A stale competing update conflicts. Read-back confirms Done and its
  exact revision; the final message links F/W and all accepted evidence.
- An AIV receipt may be attached later to E2. It does not change the commit tested,
  rewrite original timestamps or imply that AIV observed the browser tests.

## Phases and independent acceptance

| Phase | Owned artifacts / expected acceptance |
| --- | --- |
| 0: this feature | Generated instructions, persistent project policy and this spec; focused role/availability tests, installed CLI verification, handler-confirmed record trail. Publish existing-thread policy. No schema/MCP implementation |
| 1: structured relationships | Hub migration/types/store/server; tests for atomic post/intake/dispatch, primary/related validation, cross-project checks, wrong revisions, correction history and no fabricated legacy links |
| 2: clients and requirement rollout | CLI, Board, cache/history/export and capabilities; Chromium/WebKit compose/reply/intake/error cases, old/new client matrix, lost-response retry, offline drafts, pagination under concurrent writes; verify all required hosts before strict mode |
| 3: orders and evidence | Typed work orders/assignments/evidence and state transitions; out-of-order/stale run results, ownership transfers, review rejection, per-target deployment blockers and deterministic export/replay fixtures |
| 4: AIV pilot | Handler adapter, explicit repository mapping and receipt outbox; native tool preservation, exact binding, duplicate/lost submission, crash recovery, unavailable service and coverage-limit checks against isolated AIV fixtures |

All phases use synthetic data, isolated databases/browser contexts/tmux sockets
and identifiable candidate commits. Test forbidden combinations and interrupted
transactions, not only successful JSON round trips. No production migrations,
credential changes, schema edits or live MCP setup are authorized by this document.
