# Project Queue implementation report

## Recorded scope and provenance

This candidate implements only feature `wi_a222a4d8a69c1d53` revision 2 in project
`tsk_2cfcff70a0fbe967`, under work-order Board message `1640` and complete
supplement `1642`. The detailed preparation is retained in messages `1643`–`1646`,
and the lead-selected normative contract is retained in message `1647`. The
selected contract overrides preparation alternatives. The accepted B1 release
report and source baseline are retained in messages `1648`–`1651`; message `1652`
preserves original human and lead directions.

The immutable admitted context was read in full before implementation:

- `/tmp/tailterm-project-queue-context-1640-b2841e6d7a97.json`
- 104,079 bytes, mode `0400`
- SHA-256 `b2841e6d7a973174f51e8d389d0397ac4b74c8cb06bf6915fc174df40cd6da09`
- all two item revisions, zero gaps, and all 14 explicitly linked messages through
  message 1652 (binding context through message 1653)

Implementation began from exact clean commit
`0d3ecf7e20462a585a305ea85f64571f1ec5f10c` on `feat/project-queue` in the
isolated `project-queue` worktree. `docs/handoff.md` and
`docs/project-overview.md` were read before edits.

One concrete integration dependency was discovered: a separate Queue Start could
not be proven for a cross-project worker unless admission itself was pinned to the
claimed Queue entry and cycle. The discovery was returned before expanding scope
in messages `1668` and `1669`. Lead authorized the narrow seam in message `1672`,
and db-handler saved/read back the amendment in message `1673` as artifact
`nart_7836cfceadf067e8` version 1, 2,883 bytes, receipt
`nrr_c2fa9ae499bd1e4b`, SHA-256
`aa842246762ef5d694074d0357e8bdd9c0f2ac0e6d43a33393fc07cfa0a7cf10`.
The amendment permits only Queue-claim fields in the existing
work-context admission path; it does not broaden ordinary worker authority or
create a general launch API.

No helper identity was created. No live work-item read or write was performed by
this worker. No provider, production service, deployment, network, Tailscale,
TrueNAS, relay, profile, or live vault was used or changed.

### Independent review correction

Lead message `1679` rejected source candidate
`0c26e69cbfae105e41ebc21761e976ff2eed8876` after four isolated review
findings. That commit is retained as provenance only and is not an accepted
candidate. This same item/run corrected every finding:

- typed Queue system notices can no longer be presented as a human selection;
- actions capture their immutable connection and credential scope before any
  delayed persistence or request, and view epochs prevent late list, history, or
  mutation results from repainting a replacement connection/project;
- every unresolved action has a distinct durable identity, so a later priority
  or Pull cannot overwrite an older uncertain receipt/retry;
- frozen Queue list ordering uses scalar event metadata and SQL `LIMIT`, so a
  page never selects or deserializes every full description.

The supplied review-only fixtures remained in `/tmp`; they were not copied into
the product or used as live data. Permanent negative/positive Go and
Chromium/WebKit regressions cover the corrected behavior below.

The first corrected application/source candidate was exact commit
`f1605a2db0ce7d5d8e09399b10c741d24c709257`, tree
`5116f40916425aef43ad517dd6a28c1e4771749d`. Its complete 36-file
`+5143/-49` binary diff from baseline
`0d3ecf7e20462a585a305ea85f64571f1ec5f10c` has SHA-256
`ef385c14370eb96d028b57629f4639a11338240a527097c6227151289154adb3`.
The four-file `+670/-86` correction diff from rejected candidate `0c26e69…` has
SHA-256
`c1372844ec7e7eed078cc58dd08f795007158bc837b9db788ccfb54c9eed6059`.
Report-only commit `d7ac78e4a931ffb0778a25b022b4d0666339278f` recorded that
identity without changing its tested application tree.

Lead message `1685` rejected that first correction after two further isolated
findings. Before the second correction, the supplied complete review command
failed because an enqueue notice could be used as a bounded order and an exact
resumed recipient run could not recover an unavailable notice. This same
item/run corrected both findings:

- claim now requires a current audited Work message with the exact primary item
  revision and rejects typed system notices plus dispatch provenance even when
  an older binary left its enqueue message untyped; cross-project admission
  independently revalidates that retained order before an agent row can commit;
- explicit recipient reconciliation recovers one outstanding unavailable
  semantic notice as the next recipient generation of its original Queue event.
  The unavailable generation remains immutable, while the reconciliation action
  has its own distinct Queue event and keyed receipt. The same now-deliverable
  exact run and an explicitly selected replacement run are supported; a still
  retired run or an event already recovered is rejected.

The final second-correction application/source candidate is exact commit
`895b859960a1c771ea05ea23e5b41428ba9f26d2`, tree
`cfbdd34051102b579fd3434a962d32f4c20bc5ff`. Its complete 36-file
`+5399/-49` binary diff from baseline
`0d3ecf7e20462a585a305ea85f64571f1ec5f10c` has SHA-256
`807d3514cf140e09e00d3e3115227e53113b5026b2b49b49799ae8d83532dfc5`.
The three-file `+197/-15` second-correction diff from report commit
`d7ac78e4a931ffb0778a25b022b4d0666339278f` has SHA-256
`3c3f5142547771f9389ad277271d2bd8216b05bcb5ac190feaffe56300bec225`.
The following report-only commit records this final identity and does not change
the tested application tree.

## Delivered contract

### Durable Queue state

The additive store owns one stable entry for each
`(targetProject, sourceProject, item)` and retains immutable cycles and events.
Cycle states are `waiting`, `claimed`, `active`, `completed`, and `cancelled`.
Eligibility, stale, pending-update, review-needed, and reconciliation-needed are
orthogonal markers rather than invented states.

The migration creates:

- `queue_entries`, containing the current projection but no admitted private
  context document;
- `queue_cycles`, retaining terminal cycle outcomes;
- `queue_events`, immutable transition snapshots with causal actor and effective
  versus observed time, plus indexed scalar state/priority/enqueue fields used
  for bounded frozen listing;
- `queue_dispatch_links`, retaining every dispatch, exact offered item revision,
  original message, author, and timing;
- `queue_requests`, scoped keyed payload hashes and frozen mutation receipts;
- `queue_notifications`, durable typed delivery intent, causal author, exact
  recipient agent/run/generation, delivery state, and message reference.

Queue priority is independent of source priority. It is initialized once from the
first offer and uses Queue entry-revision CAS. Queue-only mutations do not change
the source item's task, status, priority, revision history, ownership, or accepted
report pin.

Source Done completes every live receiving cycle; dismissal cancels it. Both the
legacy update path and keyed recoverable item update path run the same Queue
terminal integration inside their transaction. Source or target project closure
cancels affected live cycles in the task-status transaction, preserves exact
claim/worker/run references, and does not close a run. Reopening a project or item
does not revive a terminal cycle; explicit eligible `requeue` creates the next
cycle.

### Atomic Send and replay

The existing work-item dispatch transaction now atomically commits the original
dispatch, full dispatch snapshot and provenance, Queue entry/event/link, additive
Queue result, and durable notification intent. Old response fields remain present.

Replay is checked before current status and CAS validation. Exact same-key replay
returns the frozen original Queue result and creates no new dispatch, event, or
notice. A new-key duplicate of the same item/project/revision retains its distinct
dispatch provenance while preserving a single live cycle, claim, priority, and
semantic notice. A newer waiting offer deliberately advances the offered revision;
a newer offer against a claimed or active cycle only marks a pending update and
cannot replace the pinned revision/order/context. Terminal work rejects a new Send
as non-enqueueable, while an old receipt continues to replay.

`Send`, response retry, read, priority, and `Pull` contain no provider or spawn
call. The Work Items dialog says that Send enqueues for deliberate review and never
starts an agent. A response from an older hub remains explicitly labelled legacy
rather than being presented as a Queue success.

### Selection, admission, and Start

`claim` is the durable Pull/selection operation, not execution. It requires exact
entry revision, cycle, current offered item revision, exact current claimant
agent/run, a retained human or current-orchestrator selection message, and a
separately retained exact bounded work-order message. Claim races are serialized
by SQLite plus entry CAS, so one winner is recorded and losing requests cannot
cause worker effects.

An enqueue/dispatch message is never a bounded order. Claim validates the
message's current audited Work classification and exact primary link, rejects
all typed system notices, and separately rejects the retained dispatch row so an
untyped older-binary enqueue cannot bypass the rule. Cross-project admission
repeats this validation against the claim's pinned order before persisting the
worker or binding. The complete separately prepared context must still contain
that exact valid order.

Human actions remain explicit. Agent Queue access requires the task's exact active
database-handler role and run. The selected claimant must be that receiving
project's actual current ordinary orchestrator. Ordinary workers cannot read or
mutate Queue through the CLI, and prose cannot supply authority. A retained
selection with a typed system-notice kind or ID is rejected even though its
message author is intentionally `system`; only an actual untyped human message or
the actual current orchestrator's exact agent/run message can authorize the
handler action.

Cross-project `tt spawn` admission accepts only the complete set of
`--queue-entry`, `--queue-cycle`, `--queue-revision`,
`--queue-claimant-agent`, and `--queue-claimant-run`. The store checks the live
claim, exact source/target/item/revision/order/context, active claimant role/run,
and open projects before pinning the admitted worker identity/run/context in the
same transaction. Unknown admission responses can be retried only with the exact
admitted worker ID; a second unidentified admission is refused. Same-project
admission remains compatible with the pre-Queue path.

`start` is a separate keyed Queue transition. It verifies the exact current worker
binding against the claim's item/order/context/run and, for cross-project work,
the admission pin. `launch_unknown` leaves the entry claimed with reconciliation
required. `release`, `transfer`, `withdraw`, and `requeue` require entry CAS, a
bounded reason, and exact retained-run reconciliation where a worker may exist.
There is no TTL expiry, heartbeat reassignment, duplicate-launch recovery, or
automatic launch.

### Notices, legacy reconciliation, and history

Queue change notices are typed system messages. They retain the original causal
author separately, bind the actual receiving orchestrator's exact agent and run,
and use a bounded semantic dedup identity. Automated notices never pretend to be
human and never trigger the human continuation/resume path. An unavailable,
retired, replaced, or mismatched recipient is recorded as pending/unavailable;
explicit recipient reconciliation creates a retained new generation. Reads and
delivery do not create Queue events or acknowledgement loops. Reconciliation
records its own action event/receipt, but the recovered notice remains linked to
the original unavailable semantic event with incremented recipient generation
and original causal author. Exact replay, including after store restart, returns
that frozen recovery receipt without another message or generation.

Inbox filtering has one narrow exception: a typed Queue notice may reach the
actual project orchestrator even when it is item-bound. It does not widen ordinary
worker inboxes or attach an unrelated work-item link.

Migration and every upgraded store open reconcile previously unseen
`work_item_dispatches` by immutable dispatch ID. Reconciliation is additive,
idempotent, notice-free, and separately timestamps observation. Known terminal
items/projects become terminal Queue history. An exact current work-item binding
may be retained as legacy-observed active; other outstanding history remains
review-needed and must be explicitly adopted before claim. Board text never
invents a claim. Late writes from an older binary are discovered on the next
upgraded store open.

### API, CLI, UI, and export

Capabilities advertise Queue contract version 1 and the exact page limits.
Typed endpoints provide frozen Queue lists, exact entries, frozen entry history,
incremental changes with independent cutoff/checkpoint, scoped actions, and exact
receipt recovery. Defaults are 32 rows, maximum 64 rows, and maximum 1 MiB
serialized pages. Existing 64 KiB request envelopes remain enforced; keys are
under 128 bytes and reasons are 1–1024 bytes. Page code shrinks a page as needed
and explicitly rejects an impossible single-row page instead of silently evicting
history.

A frozen list page first computes its cutoff and latest event IDs, filters and
orders using scalar `state`, `priority_rank`, `first_enqueued_at`, and `entry_seq`
columns, and asks SQLite for only `limit+1` snapshot blobs. Go therefore holds and
deserializes at most 65 full descriptions before enforcing the 1 MiB serialized
response bound. The metadata count/group/order scan still scales with the total
number of Queue entries and offset traversal can scan preceding scalar rows, but
it does not load those rows' description blobs into the response process. There
is no total-entry cap; the frozen cursor/cutoff retains complete deterministic
priority/enqueue traversal.

`tt queue` implements `list`, `get`, `history`, `changes`, `action`, and `receipt`.
Actions use complete typed JSON files and stable request identities. CLI reads and
mutations from an agent verify the exact active database-handler run. Queue-specific
coordination help says that Send means enqueue/review and that actual work requires
deliberate selection plus a bounded order.

The application adds Queue after Features with shortcut `8`; Files remains hidden.
The view retains the selected entry, scroll, terminal-history choice, action intent,
request identity, attempted payload, recovery error, and newer edits across
refresh/reload. It shows source/current/offered revision, source and Queue priority,
cycle/state, eligibility markers, current claim, and retained history. Selection
uses fill, controls are compact and aligned, and there is no decorative empty
filler. The UI offers priority and Pull but deliberately has no Start or spawn
control. Queue actions are disabled with a visible explanation when the hub does
not advertise Queue v1.

Queue intents are encrypted in the existing project/credential-scoped local vault,
excluded from portable backup, limited to 24 records, 64 KiB per record, and 1 MiB
total. Exact successful replay clears only the same request key plus submitted
generation/payload; a newer edit survives delayed cleanup and rotates identity on
its next deliberate attempt. Each action attempt has its own bounded durable ID,
including multiple unresolved actions of the same operation. Mutation submission
uses the client captured when the user created the action; it never calls a
replacement credential client with the old task/entry/payload. Persisted retries
first match the current connection hash and project. Confirmed cleanup always uses
the original intent scope. Late persistence, list, history, fetch, retry, or
cleanup completion may resolve that exact intent but cannot replace a newer
projection or delete another unresolved attempt.

Audit export now advertises formats 2 and 3. Format 3 includes complete Queue
entries, cycles, events, dispatch links, receipts, and notification metadata for
both receiving and source-project relevance plus an independent `queueEvents`
cutoff. It reuses B1's consistent SQLite snapshot, 32 MiB artifact, 256 KiB chunk,
seven-day retention, eight-ready, 128 MiB ready-content, replay, tombstone, and
no-recursive-export rules. Format 2 keeps its prior query set and envelope and does
not claim Queue coverage. Existing frozen v2 artifacts and request receipts are not
rewritten or regenerated.

## Isolated verification

All verification used temporary SQLite databases, loopback HTTP servers,
disposable Chromium/WebKit contexts, synthetic work items, synthetic agents/runs,
and encrypted disposable vaults.

Passed on the final candidate:

- `go test ./internal/store ./internal/server ./internal/api`
- `go test ./internal/server -run 'TestCapabilitiesAndAuditExportHTTP|TestQueue' -count=1 -v`
- `go test ./internal/store -run 'TestAuditExport|TestQueue' -count=1 -v`
- `go test -overlay=/tmp/tailterm-queue-review-overlay.json ./internal/store -run '^TestReviewQueue' -count=1`
- `go test ./cmd/tt -run 'TestQueue' -count=1 -v`
- `go test -race ./internal/store ./internal/server ./internal/api`
  (`internal/store` completed in 61.557 seconds, server in 23.605 seconds, and API
  passed)
- `go test -race ./internal/store -run 'TestQueueConcurrent|TestQueueDispatchCAS'`
- `go vet ./...`
- `go build ./...`
- `npm test` — 149/149
- `npm run build`
- `node tests/project-queue-browser.mjs` — Chromium and WebKit actual Work Items
  Send, exactly one Queue entry/notice, priority, committed-response exact retry,
  Pull, history, terminal filtering, unsupported capability, and unchanged agent
  count/no launch
- `node tests/project-queue-vault-browser.mjs` — Chromium and WebKit encrypted,
  bounded, credential/project-scoped, newer-edit-safe recovery across reload
- `node tests/project-queue-review-browser.mjs` — Chromium and WebKit delayed
  persistence/fetch/list/history, connection replacement, stale-view suppression,
  original-scope cleanup, distinct same-operation attempts, exact older replay,
  reload, and delayed cleanup retaining newer unresolved intents
- `node /tmp/tailterm-queue-review-client.mjs` — supplied credential replacement
  reproducer passes with the original action sent only through captured client A
- `node /tmp/tailterm-queue-review-uncertain.mjs` — supplied reproducer retains
  both request IDs and two exact retry controls
- `node tests/work-item-history-width-browser.mjs` — Chromium and WebKit, all 20
  Bug/Feature and 1366/1024/640/390/200%-zoom-equivalent cases
- `node tests/board-scroll-browser.mjs` — Chromium and WebKit wheel, smooth-motion,
  touch-hold, prepend/read/incoming/own-send anchors, focus and drafts
- `git diff --check`

Store coverage includes atomic Send and exact replay, new-key duplicate provenance,
same item across receiving projects, independent priorities, stale/newer offers,
concurrent dispatch and claim races, exact cross-project admission and retry,
separate Start, unknown launch, release/transfer/withdraw/requeue, both terminal
item paths, project closure/reopen, handler/role/run/selection rejection, retired
and replaced notice recipients, narrow bound-orchestrator delivery, restart dedup,
no acknowledgement loop, legacy migration/adoption/late writes, frozen lists and
history, incremental checkpoints, response-size bounds, and absence of private
context in Queue entries.

Work-order authority coverage includes a positive separately bounded exact order,
negative typed Queue and untyped legacy-dispatch enqueue references, and
admission-time revalidation proving an invalid retained order cannot commit a
cross-project worker. Recipient coverage includes same-run explicit recovery,
replacement-run reconciliation, immutable unavailable generation history,
separate semantic versus reconciliation event identity, still-retired rejection,
new-key duplicate rejection, exact replay after store restart, and no read-driven
acknowledgement loop.

Authority coverage explicitly rejects a typed Queue notice as the handler's
selection while retaining positive genuine-human and exact current-orchestrator
selection cases. List coverage creates 70 entries with 7 KiB descriptions,
traverses every frozen page exactly once below 1 MiB, and proves a deliberately
invalid far off-page snapshot is not selected or deserialized by a one-row first
page. The list query's full-blob materialization bound is therefore tested rather
than inferred only from response length.

Export coverage proves that v2 omits Queue streams/cutoffs, v3 contains all Queue
streams and an independent cutoff, a later Queue mutation cannot change frozen v3
bytes, the published digest matches the artifact, and existing export replay,
expiry, chunk, count, and byte limits continue to pass.

Repository-wide `go test ./cmd/tt -timeout 45s` is explicitly not claimed as a
pass. It reproduces the two accepted untouched B1 baseline failures:

- `TestPostHumanReplyAndLiteralHelp`: mock returns `500 unexpected route`;
- `TestAskPreservesIdentityContextAndReplayPayload`: its test server retains an
  active connection and the suite reaches the 45-second timeout.

The Queue-focused CLI tests pass. No Queue file changes those two tests or their
routes.

## Compatibility, migration, and rollback

The schema change is additive. The corrected migration also adds/backfills the
four scalar Queue-event list fields and their index from immutable event snapshots
when opening a database created by the first candidate. Older clients can continue calling Send against an
upgraded hub and receive their original fields while the hub atomically enqueues.
A new client on an older hub exposes unsupported Queue capability and does not
downgrade priority, Pull, or receipt intent into an item mutation or ordinary
message.

Rolling the binary back leaves Queue tables, receipts, notice columns, and history
intact; they must not be dropped because that would destroy retry identity and
provenance. An older binary will ignore this state and cannot enforce Queue claims,
atomic enqueue semantics, typed notice routing, or v3 export. Any dispatches it
writes are reconciled without notices on the next upgraded store open. Therefore
normal rollback is binary-only with additive data preserved, and a rollout should
not claim Queue enforcement while mixed binaries remain able to write.

Format 2 remains available for compatibility and is intentionally incomplete for
Queue. Format 3 is required for a complete queue-aware project snapshot. Previously
frozen artifacts of either format keep their identity and bytes.

## Scope and remaining release prerequisites

Owned implementation is confined to new Queue client/API/store/server/CLI modules,
Queue-specific tests and this report, plus the work-order-approved dispatch,
terminal/project-closure, notice delivery, capabilities, export v3, client wiring,
encrypted intent, policy text, and exact cross-project admission seams. Queue CSS
is isolated in `client/queue.css`. Research-owned
`docs/initial-agents-research.md` and
`docs/initial-agents-research-catalog.json`, Agents/team schema, B2/C1/AIV, Files,
unrelated UI, providers, deployment code, and production operations are untouched.

The trusted shared credential boundary remains explicit: server-side checks bind
the current recorded role/run/selection/claim, but this feature does not create
per-agent cryptographic credentials. Actual Queue release still requires lead
candidate review, db-handler revision-checked result/acceptance storage, a later
explicit root integration order, new isolated release builds/hashes/backups, hub
and CLI rollout, and TailOS/Mini frontend deployment verification. This work order
does not authorize deployment, changes to `tailterm.tailarr.com`, agent closure, or
self-acceptance.
