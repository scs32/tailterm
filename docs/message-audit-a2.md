# Message audit A2 implementation report

This report covers A2 only for feature `wi_edd77038c629a76c` revision 2 under work-order message `1425` (`source-1421-a2-1`) in project `tsk_2cfcff70a0fbe967`. The acceptance contract is Board message `1426`, preserving the exact text of message `1416` (4,842 bytes, SHA-256 `d49f85c5b3fc81502719526e475d8a79153117925b0d2d2541502e9a78814610`). The implementation started from clean `tasks-hub` commit `c7730738611555bd5474ceca166a23c239ca1690`.

The A2 authority is additive and remains in observe mode. It does not implement Board UI, cached/project exports, required-mode enforcement, typed work orders, AIV integration, production backfill, deployment, or any later roadmap slice. It does not change work-item status and cannot complete the parent roadmap.

## Wire contract

`POST /v1/tasks/{task}/messages` gains the optional `auditKind` field:

- An omitted `auditKind` with no links is a legacy, unclassified post. It creates no audit row.
- `auditKind: "intake"` requires a stable `requestId`, forbids work-item links and a work-order message, and creates audit revision 1.
- `auditKind: "work"` requires a stable `requestId`, exactly one primary target, and zero through 15 distinct related targets. The old A1 form—omitted `auditKind` with one primary link—continues to mean work.
- Creation-time `workItems`, `workOrderMessage`, author, text, time, and posting receipt stay immutable. Corrections never alter the message or its A1 link. Posting-receipt replay always returns this frozen original context.

The current projection and immutable audit history use dedicated endpoints:

| Method and path | Contract |
| --- | --- |
| `GET /v1/tasks/{task}/message-audit/messages/{seq}` | Returns exact message reference, immutable `original`, and optional `current`. A legacy unlinked message has original classification `unclassified` and no current state. |
| `POST .../messages/{seq}/corrections` | Full-state CAS using `requestId`, `expectedRevision`, complete `desired` classification/link set, nonempty `reason`, exact `sources`, and optional exact `agentId`/`runId`. |
| `POST .../messages/{seq}/resolve` | Atomically resolves current Intake to exactly one existing primary item or one keyed new-item specification. The new-item `expectedRevision` is zero; the intake audit revision is supplied separately. |
| `GET .../messages/{seq}/history` | Frozen per-message history. Start with `afterVersion` and continue only with the returned opaque cursor. |
| `GET /v1/tasks/{task}/message-audit/receipts/{requestId}?operation=correct|resolve&agentId=...` | Recovers the original correction/resolution result in the same caller and claimed-agent scope. |
| `GET /v1/tasks/{task}/message-audit/changes` | Project-scoped, ordered audit-event feed. Optional `kind=intake|work`; `cursor` freezes pagination to one cutoff, while the final page's `checkpoint` starts a later incremental window after that cutoff. Both tokens are bound to project and filter. |
| `POST /v1/tasks/{task}/message-audit/associations` | Creates one durable, explicit foreign-item association from an exact destination-project source message and actor. |
| `GET .../associations/receipts/{requestId}?agentId=...` | Recovers the original association result in the same caller and claimed-agent scope. |

First successful mutation/association calls return HTTP 201; exact replays return HTTP 200. Malformed data returns 400, missing records 404, and CAS, closed-project writes, changed-key retries, inactive/stale runs, duplicate resolutions, and response-bound failures return 409. Reads and exact receipt replay remain available after closure.

Correction and resolution receipt fingerprints include the exact task, message sequence, operation, and normalized request. Reusing a key and body against a different message is therefore a changed-target conflict, never a replay of the first message's result.

Correction events contain the complete before and after projections, exact message identity, monotonically increasing audit revision, project audit cursor, operation/provenance, reason, source references, attributed caller and optional exact run, and event time. Link order is deterministic: primary first, then related links sorted by project/item/revision. Source references are also normalized deterministically. A correction that reproduces the current complete state is rejected rather than emitting a misleading event.

The exact message-audit read loads the message, immutable original, state header, and current links in one SQLite read transaction. A correction committed during that read is visible only to the next snapshot, so an audit revision/event pointer cannot be paired with another revision's links.

Agent-authored corrections and resolutions require a nonempty valid run equal to the agent's current run. New writes accept only starting, running, done, or needs-input agents; retired, closed, and exited runs cannot write. Receipt lookup occurs before current task, revision, or run-state checks, so a committed request remains recoverable after later changes, closure, or retirement.

An explicit foreign association can be authored by the exact human caller whose retained source message is cited, or by an active database-handler run whose retained message has the same exact agent and caller attribution. It pins a retained exact foreign item revision and cannot target the destination project itself. Validated dispatch records independently authorize their exact source item in the dispatch destination. Replies, mentions, ordinary agent claims, and correction sources do not create foreign authority.

## Storage and migration

A2 adds only new tables and indexes:

- `message_audit_originals`: immutable explicit creation classification, complete creation links, and creation work-order reference.
- `message_audit_states`: current revision/classification/event pointer.
- `message_audit_links`: normalized current projection, with unique item targets, ordinals 0–15, and at most one primary by index.
- `message_audit_events`: append-only full before/after changes with a global SQLite sequence used as the audit cursor.
- `message_audit_receipts`: scoped correction/resolution request hashes and frozen results.
- `message_audit_foreign_associations` and `message_audit_association_receipts`: explicit validated cross-project authority and replay.

Existing `messages`, `message_work_item_links`, `message_post_requests`, work-item history, narrative, dispatch, task, agent, and lifecycle tables are not rewritten or removed. A2 still records only the creation primary in the A1 link table so an old binary reads its original contract; the full creation link set lives in `message_audit_originals`, and current routing uses `message_audit_links`.

Legacy bound inbox reads continue to route through immutable `message_work_item_links`; they return the original link and retain historical visibility after a current-state move or detach. Current link consumption is deliberately limited to the explicit A2 reads and change feed until B1 supplies correction-aware clients.

Migration runs on every new-binary open after the existing work-item-history migrations. It creates a revision-1 `validated_a1_migration` baseline only for retained, validated A1 creation links lacking an A2 state. Unlinked legacy messages remain unclassified and receive no synthetic event. Reopening is idempotent. Because reconciliation runs on each open, an old binary may open the additive database and add an A1-linked message; the next new-binary open imports that exact link without relabeling unrelated messages.

There is intentionally no destructive down migration. Rollback is:

1. Stop the candidate and preserve a consistent online backup of the complete SQLite database.
2. Start the previously accepted binary against that database. It ignores the additive A2 tables and continues to use immutable A1 message links and posting receipts.
3. Keep the A2 tables during the rollback window. Do not drop them while a later new-binary retry or audit receipt may need recovery.
4. On returning to A2, reopen normally; migration reconciles A1 writes made by the old binary. Integrity and isolated compatibility checks must precede any release.

No production database was read, mutated, backfilled, or deployed during this work.

## Bounds and consistency

- Related targets: at most 15, plus exactly one primary for work.
- Correction source references: one through 16, all exact and existing.
- Page size: default 32, maximum 64 events.
- Serialized history and change-feed response: at most 1 MiB, including continuation metadata and trailing JSON newline at the HTTP boundary.
- Request bodies retain the server-wide 64 KiB cap. Reasons are nonempty and at most 2,048 bytes; caller node/user values stored in A2 events are at most 512 bytes each.
- The frozen cutoff is established on the first page. A continuation with a different project, message, filter, impossible future cutoff, or explicit `afterVersion` is rejected.
- A completed change-feed page, including an empty or filtered page, returns a checkpoint through its cutoff. Supplying that checkpoint without a pagination cursor takes a fresh cutoff and returns only later matching events. Pagination cursors and checkpoints are mutually exclusive.
- If one stored event cannot fit the response ceiling, the server returns a conflict instead of an empty self-repeating page.

Item creation during Intake resolution, the native work-item create receipt, revision/change/history rows, ordinary hub event, audit event/projection, and audit receipt share one SQLite transaction. Injected failure at each material boundary rolls back the item and leaves Intake unchanged. Store serialization plus projection revision CAS makes same-key races replay one result and makes different-key contenders yield one commit and one conflict.

## Isolated verification

All fixtures use temporary SQLite files, synthetic projects/agents, temporary binaries, and local loopback HTTP servers. They contain no live task or profile data.

Passed:

- `go test ./internal/store -run 'TestMessageAuditA2' -count=1 -v`
- `go test ./internal/server -run 'TestMessageAuditA2' -count=1 -v`
- `go test ./internal/store ./internal/server ./internal/api`
- `go vet ./...`
- `go test -race ./internal/store ./internal/server ./internal/api`
- `git diff --check`

The A2 tests cover original/current separation, frozen posting receipt replay, complete correction history, old-message correction without a new message, count/filter/byte-bounded frozen cursors, incremental checkpoints across empty and filtered pages, malformed and swapped tokens, Intake-to-existing and Intake-to-new resolution, native-create receipt agreement in both directions, same-key and competing races, wrong-message correction/resolution receipt rejection, injected transaction failures, related/duplicate/primary validation, current and retained item revisions, explicit human/handler and dispatch-derived foreign authority, arbitrary foreign rejection, exact active-run enforcement, recovery after retirement/closure, deterministic snapshot reads during a concurrent correction, legacy A/B-bound inbox behavior after move/detach without a new message, additive schema, reopen idempotence, and foreign-key integrity.

An executable compatibility check built the exact old hub at `c7730738611555bd5474ceca166a23c239ca1690` and the reviewed A2 hub separately with `go build -trimpath -buildvcs=false`. The new binary initialized a temporary A2 database; the old binary then opened it and posted a real A1-linked message over loopback HTTP; the new binary reopened it and exposed `work` revision 1 while the old posting receipt still returned the frozen original. Old-binary SHA-256 was `0ef28826c6eae870b85c288a6f6e2547435b3bbc56f31845a2ac58029c3941c1`; reviewed candidate-binary SHA-256 was `2317ff04d6e99c7ebfe82b0cfef9988f32dbc007e0ccd6f8509aca3b5da26e3e`.

The repository-wide `go test ./...` did not pass because two `hub/cmd/tt` tests already fail or deadlock on the untouched base: `TestPostHumanReplyAndLiteralHelp` receives its mock's `500 unexpected route`, and `TestAskPreservesIdentityContextAndReplayPayload` blocks its test server until the timeout. Re-running only those tests from a fresh archive of exact base `c7730738611555bd5474ceca166a23c239ca1690` reproduced both failures with no A2 source present. Every package built and all A2-owned and affected package suites passed.

## Remaining gates and limitations

Lead review and database-handler revision-checked result storage remain required before A2 is accepted. A separate recorded release order must identify the hub and any changed CLI target, backup/integrity procedure, rollback artifact, and deployment receipts. This implementation performs no live historical correction manifest, production backfill, required-mode switch, Board/cache/export integration, AIV work, or deployment. The shared preview service is independent and was not touched.
