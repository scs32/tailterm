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

The second-correction application/source candidate was exact commit
`895b859960a1c771ea05ea23e5b41428ba9f26d2`, tree
`cfbdd34051102b579fd3434a962d32f4c20bc5ff`. Its complete 36-file
`+5399/-49` binary diff from baseline
`0d3ecf7e20462a585a305ea85f64571f1ec5f10c` has SHA-256
`807d3514cf140e09e00d3e3115227e53113b5026b2b49b49799ae8d83532dfc5`.
The three-file `+197/-15` second-correction diff from report commit
`d7ac78e4a931ffb0778a25b022b4d0666339278f` has SHA-256
`3c3f5142547771f9389ad277271d2bd8216b05bcb5ac190feaffe56300bec225`.
Report-only commit `c705432053d72dadb7417dd6c7c35b5db9b8d3f9` recorded that
identity without changing its tested application tree.

Lead message `1692` confirmed all prior review cases and hashes, then rejected a
replacement-recipient regression introduced by the second correction. The exact
expanded overlay reproduced that a deliberately installed replacement
orchestrator could not be pinned unless an unrelated unavailable notice existed.
The bounded third correction distinguishes the two operations:

- when the actual current orchestrator ID/run differs from the pinned recipient,
  explicit reconciliation updates the pin and emits a generation-1 notice owned
  by the new reconciliation event; previously stored or unavailable messages
  remain pinned and immutable;
- when the actual current orchestrator is the already pinned ID/run, explicit
  reconciliation remains recovery-only and requires an outstanding unavailable
  notice, whose original semantic event receives the next recipient generation.

The final third-correction application/source candidate is exact commit
`cfb2172ae81095c035c58eab3c345ed4e489241e`, tree
`35e01c8f3a3118ac6a3ba563c489637ef1609352`. Its complete 36-file
`+5493/-49` binary diff from baseline
`0d3ecf7e20462a585a305ea85f64571f1ec5f10c` has SHA-256
`03509d4e855fd17ac2fcaaf077eaa9d15974287cfbcd4bf64c42d64ce5aaa033`.
The two-file `+47/-3` third-correction diff from report commit
`c705432053d72dadb7417dd6c7c35b5db9b8d3f9` has SHA-256
`c0f76ac225aae23a2996c242d69ab8cd215c9637b4bb1b9f80ffb25a2be5d085`.
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
delivery do not create Queue events or acknowledgement loops. Same-recipient
recovery records its own action event/receipt, but its notice remains linked to
the original unavailable semantic event with incremented recipient generation
and original causal author. Changed-recipient retargeting instead keeps all old
messages immutable and creates a new notice linked to the reconciliation event.
Exact replay, including after store restart, returns the frozen receipt without
another message or generation. Same-recipient reconciliation without an
outstanding unavailable notice remains an explicit no-op conflict.

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
  (`internal/store` completed in 61.158 seconds, server in 22.058 seconds, and API
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

The full backend, race, vet, build, expanded external overlay, and focused
recipient tests were rerun on exact application commit `cfb2172…`. The npm and
Chromium/WebKit results were run on `895b859…`; the third correction changes only
`hub/internal/store/queue.go` and its Go test, so the tested frontend tree and
browser fixtures are byte-identical in `cfb2172…`.

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

Changed-recipient coverage additionally starts with a successfully stored Send
notice, closes that lead, installs the exact ordinary replacement, retargets
without manufacturing an unrelated failure, preserves the old stored message,
returns the same keyed receipt on replay, and proves a subsequent Send succeeds.
The expanded supplied overlay case and the permanent regression both pass.

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
per-agent cryptographic credentials. At candidate handoff, actual Queue release
still required lead review, db-handler revision-checked result/acceptance storage,
a later explicit root integration order, new isolated release builds/hashes and
backups, hub and CLI rollout, and TailOS/Mini frontend deployment verification.
The implementation order itself did not authorize deployment, changes to
`tailterm.tailarr.com`, agent closure, or self-acceptance; the separate release
order and its result follow below.

## Released stage

Lead accepted exact source `cfb2172ae81095c035c58eab3c345ed4e489241e`
in message `1697`; db-handler independently saved and read back the complete
source acceptance in message `1701`. Lead then issued the concrete integration
and production release order in message `1702` for this same item, agent and run.
The tracked-clean root `tasks-hub` branch fast-forwarded from accepted baseline
`0d3ecf7e20462a585a305ea85f64571f1ec5f10c` through only the accepted Queue
application and report commit
`b5adb1dd41d588ff5ea88613da8107714e2f5708`. Research-owned documents and
unrelated pending work were not integrated, and the preserved owner screenshots
were untouched.

The clean retained release source and complete package are at
`.build/releases/project-queue-cfb2172`, detached at exact application commit
`cfb2172…`. Its Linux amd64 hub is 27,226,274 bytes, SHA-256
`150b53846b2fecfa20b2ced36789de0a1d3bcb2152b67ccd9f46ab448a9af1d6`;
its Darwin arm64 CLI is 6,698,098 bytes, SHA-256
`ec8bf996bbde9761563ed67c200c65641d8769aa1f504843653b0980a671d493`.
Both binaries embed exact `cfb2172…` with `vcs.modified=false`. The complete
82-entry static manifest is 14,861 bytes, SHA-256
`e7919f9448fa0dc9d5938315c6747dbc9c035645398c48c2ce0ff34e21a234a4`,
and identifies commit `cfb2172…`, dirty false. The dependency lock and tracked
WASM inputs/scripts were unchanged from B1, so the build reused the exact
independently verified Tailscale 1.102.3 production WASM: raw SHA-256
`dc841019c8a28670b3a44e0657f3a1e081648dbb561d5072eb735e987e577cb8`,
gzip asset SHA-256
`3fbd89103e04af9fdafe9a7f38da9100f5b9c71d39a5c82c4e5fa59dc8bbdf82`,
and module inventory SHA-256
`2c33e75b00b437e19377989a0fd36a631dd4a9cbd94b8fd17833ea4496e6778b`.

Before mutation, the exact B1 hub, both CLIs, retained package, immutable
deployment and all 81 served rollback assets were reverified. The new online
SQLite backup is mode `0600` at
`/mnt/deepfreeze/tailterm-hub/backups/before-project-queue-20260910T201142Z.sqlite`,
14,503,936 bytes, SHA-256
`b2ffb31f9867a3bb175ef59f85770c66347d994e12065414604e84d7a4b995a8`;
its integrity is OK with zero foreign-key violations. Focused synthetic migration,
legacy reconciliation, frozen-page bound, and v2/v3 export tests passed without
using the backup or any live database as fixture.

TrueNAS middleware updated only `tailterm-hub` to unique release
`20260910-project-queue-cfb2172`. Final inspection shows one RUNNING `hub`
container, the unchanged distroless image and UID/GID `950:950`, private
`100.116.238.37:18765` listener, read-only exact binary/token mounts and the same
writable state mount. Installed binary hash matches the retained build. Live
integrity remains OK with zero foreign-key violations and the six expected Queue
tables plus their eight indexes. Authenticated capabilities advertise Queue v1,
message audit v1/v2, export v2/v3 and observe policy; doctor passes and
unauthenticated `whoami` remains 403. No live Queue/content mutation was used.

Mini and Air atomically installed the exact matching CLI and retain mode-preserved
B1 rollback copies named `tt-before-project-queue-cfb2172`, SHA-256
`3f00c6002476acb4f9dcac8b43e9fb16c9c201f96efb8d36d2ba1631f5885571`.
Both installed hashes, doctor and capabilities pass. No relay was restarted or
changed, and Air preview was untouched.

Wrangler 4.131.0 published the exact retained package to Pages project `tailos`,
production branch `main`, commit `cfb2172…`, dirty false. Deployment
`552de3bf-9453-4992-b0fb-1bf12d84dcd3` is available at
`https://552de3bf.tailos.pages.dev` and through
`https://tailos.tailarr.com`. A staged local replacement made Mini serve the
byte-identical package at `http://127.0.0.1:4318` while preserving detached
PID 60799 / PPID 1. All three origins match the exact manifest, canonical index,
and all 81 public assets by status, size, SHA-256 and origin-appropriate MIME.
Fresh disposable Chromium contexts on all three origins started production WASM,
generated and restored only a synthetic local vault/key, passed three layout
sizes, and reported no page errors.

### Release command corrections and nonpasses

- A first retained checkout made with `git worktree` caused Go to embed root
  report commit `b5adb1d…`. Nothing was deployed. Only that new checkout was
  replaced with a standalone local clone; the final binaries embed exact
  `cfb2172…` and a clean VCS stamp.
- The first standalone setup was invoked from `hub`, creating only new misplaced
  dependency/build links before its copy failed. Those exact new paths were
  removed, and setup restarted from release root. A temporary `node_modules`
  symlink was also removed because it dirtied the Go stamp; the existing ignored
  dependency tree was clone-copied without any install for static packaging.
- `npm run test:static` required a separate test-only `.build/test.wasm`. The
  fixture never entered the package and was removed afterward. The suite reached
  its browser section, where unchanged retained B1 fixture
  `tests/tasks-browser.mjs` still searches for `Task hub: configure` while the
  unchanged product text is `Project hub: configure`. Earlier workspace, popup
  and dictation sections passed. This baseline mismatch is not claimed as a pass;
  the release itself passed the 82-entry package check and deployed browser test.
- A verification command accidentally run from report-only root correctly
  rejected the `cfb2172…` manifest against root `b5adb1d…`; the identical command
  passed in the clean retained release checkout.
- One read-only live SQLite recheck first named a nonexistent database file and
  one quoted query failed. Corrected read-only checks of `hub.sqlite` and the
  backup passed. No database mutation resulted.
- The deployment helper has no help parser. A final `--help` attempt uploaded the
  exact Queue binary into a new stray `releases/--help` directory, then failed at
  duplicate app creation before changing the running app. The exact stray binary
  was hash-verified and removed with its empty directory; final middleware
  inspection proved the actual Queue app and topology unchanged.
- One `lsof` diagnostic used broader OR semantics than intended and displayed
  unrelated processes. It changed no relay, listener or process. No relay was
  subsequently inspected or restarted.
- Repository-wide `go test ./cmd/tt` remains unclaimed for the two documented,
  untouched B1 baseline failures. Queue-focused CLI tests pass. Native Safari,
  physical IME and native macOS popup automation remain outside the evidence.

Normal rollback uses the retained B1 hub release
`20260910-message-audit-b1-da3b686`, each host's saved B1 CLI, and exact B1
frontend package/deployment `e59f1292-8c6f-41f6-95e1-37e98f13a686`, while
preserving additive Queue tables and receipts. Mini's pre-release B1 package is
also staged at `.build/static-before-project-queue-da3b686`. The online backup is
disaster recovery only because restoring it loses every write after
`2026-09-10T20:11:42Z`.

The complete machine-readable receipt is
`docs/releases/tailos-2026-09-10-project-queue.json`. This release is
builder-verified, not self-accepted. Lead actual-release acceptance and
db-handler revision-checked storage/readback of this complete report remain
required before item completion or agent closure.
