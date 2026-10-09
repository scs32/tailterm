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

## Team queue handler arms

Under an enabled [handler arm policy](handler-ab.md), a team queue claim draws
an arm for the entry from the policy seed and the entry ID and leases a free
handler of that arm. The entry's `handlerArm` field records the current lease's
policy revision, draw, drawn arm, leased arm, fallback and reason, skipped
limited arms, handler agent and run, the run's and the policy's template
digests, and the lease and finish times. `tt team queue list` prints
`arm=S drawn=S fallback=no|busy`.

While a queued entry waits for an arm, its reason is `Waiting for a free handler
of arm S` or `Every handler arm is at a provider limit`, and the claim returns a
409 ending in `no free handler in the drawn arm`, which the runner treats as an
ordinary wait. With no policy, or a disabled one, entries lease the first free
handler as before.

## Handlers and the limit

Bug `wi_01b6d3afed81167c`, work order #27052; the full description is
[handler-ab.md](handler-ab.md), "Automatic provisioning".

**Rule.** The limit admits teams, and every team needs its own database
handler. When a queued entry is admissible except for a free handler, the
runner on its host adds one handler from the host's saved launch spec. The
hub allows it only when automatic handler provisioning is on, the matching
handlers are below the limit (with `--limit none`: below the active teams
plus one), no other provision is pending, the saved spec's runtime, model,
reasoning and prompt template digest equal the wanted ones exactly, and the
handler and the waiting team both fit under the project agent cap (`open +
reserved + team seats + 1 <= cap`). The hub then posts one Board notice,
`Automatic handler provision`; it is not an owner intervention. A reserved
handler that is not online after 10 minutes is abandoned, and the runner's
next pass reserves and starts a new one, with a new notice. Handlers are never
closed by this rule.

**Reasons.** The waiting entry's JSON carries `handlerNeed` (`provision`,
`arm`, `handlers`, `leased`, `reason`, `fix`). Its reason starts with `Waiting
for a free handler of arm S` under an arm policy, else `No free database
handler`, and continues, for example after `: 3 of 3 leased, limit 4`:

- `; the runner is adding one`
- `; automatic provisioning is off. Fix: tt team queue provision --task TSK --auto on`
- `; cannot add one: project agent cap: 23 open + 6 seats + 1 handler > 29.
  Fix: tt team queue limit --task TSK --limit 3`
- `; cannot add one: the saved launch spec on HOST is RUNTIME/MODEL/REASONING
  digest D1, arm S needs RUNTIME/MODEL/REASONING digest D2. Fix:
  PROMPT="$(cat PROMPT_FILE)" && tt handler spec --task TSK -- --run 'SAVED
  RUN' --cwd 'SAVED CWD' --runtime R --model M --reasoning E --prompt
  "$PROMPT" (PROMPT_FILE holds the handler prompt with that template digest),
  or: tt team queue limit --task TSK --limit 3`. The command repeats the saved
  spec's other launch flags and is safe to paste: it saves nothing if
  `PROMPT_FILE` cannot be read.
- the same with `the saved launch spec on HOST is missing` when the host has
  no saved spec, or when the saved spec starts another runtime; the command
  then has no `--run` (so `tt handler spec` refuses it) and the note says `add
  --run with the R launch command and the host's other launch flags`.

The reason stays the plain wait, and nothing is added, when the limit already
has its handlers, or when the project's first handler recorded no model or
template at spawn.

**Switch.** Automatic handler provisioning is on by default and stored per
project in `team_queue_settings.handler_provision`. `tt team queue provision
--task T --auto off` turns it off and `--auto on` back on; `tt team queue
list` prints `handler provisioning=on|off`. Because the default is on, a
released runner starts a handler the first time a team waits with room under
the limit and the cap.

**Raising the limit warns.** Of "warn or provision up to the limit" this
change warns: `tt team queue limit` saves the raised limit and prints
`warning: limit 4 exceeds 3 available database handlers; the runner adds one
per waiting team while the agent cap allows`, or with the switch off `…;
automatic provisioning is off, so teams will wait. Fix: tt team queue
provision --task TSK --auto on`.

## Team queue shared checkouts

Two teams never edit one working tree, so a queued entry whose `cwd` equals an
active entry's waits for it even when their ownership is disjoint. Entries
queued before parallel projects, or with `--cwd` or `--no-new-worktree`, often
use the main checkout and so run one at a time. `tt team queue list` prints
each entry's `cwd=`, and when a shared checkout is the only conflict the reason
is `Shares checkout DIR with active entry tqe_A; move it: tt team queue scope
--task ID --entry tqe_B --new-worktree`. An ownership overlap keeps its
ordinary `blocked-by` without that text, and an earlier reason (slots, host,
handler, unscoped or serial) takes precedence.

```sh
tt team queue scope --entry tqe_ID --new-worktree [--owns PATH...]
```

moves a queued entry into its own detached worktree,
`<repository root>/.build/worktrees/queue-<item8>`, created from the entry's
current checkout at its frozen base commit, not the checkout's newer `HEAD`.
The owner or any live database handler may move it, the same authority as
`scope`. It needs a `queued` entry, a frozen repository and base, ownership
(the entry's own unless `--owns` replaces it), and must run on the entry's
launch host. `--cwd` and `--no-new-worktree` are refused with it. The hub keeps
the entry's ID, position, item, revision, order, repository, base and history;
it saves the new `cwd` and ownership and increments the entry revision. It
refuses a launching, running or failed entry, a relative or unclean path, a
different repository, and a path another queued or active entry uses.

`--owns` takes one repository-relative path per flag: repeat it for each path
(`--owns hub/a.go --owns docs`). This holds for `tt team queue add`, `scope`
and `requeue` and for `tt work-items scope confirm`. A value containing a comma
is refused by the CLI before any request is sent (`ownership path "a,b"
contains a comma; repeat --owns once per path`); it is never split, because a
comma-joined list stored as one path matches no file and hides real overlaps.

Until the hub confirms the move, any failure, including a refusal, a stale
revision or an unreachable hub, removes the new worktree (`git worktree remove
--force`), so the command can be rerun. When the write's response is lost, the
CLI reads the entry back and keeps the worktree if the hub saved it. A path
that already exists is refused; remove it with `git worktree remove PATH`
first. A rerun on an entry that already uses its worktree prints `entry already
uses its own worktree PATH` and changes nothing.

If both the response and the read-back are lost, the CLI reports the error
and removes the worktree, although the hub may have saved the move. The entry
then names a missing directory, and its launch would fail. Rerun the same
command: when the entry's cwd is exactly its missing
`.build/worktrees/queue-<item8>` path, it recreates that worktree detached at
the entry's base, prints `worktree PATH recreated at BASE`, and makes no hub
write. By hand, the equivalent is `git -C <repository root> worktree add
--detach PATH BASE` (with `git worktree prune` first if Git still lists the
path). A missing cwd that is not the entry's queue worktree is refused. Like one made by `tt team
queue add --new-worktree`, the worktree stays after the entry finishes; remove
it with `git worktree remove PATH` once its candidate is integrated.

Release the hub before the CLI: a CLI that sends the move to an older hub
refuses when the saved entry lacks the new `cwd` and removes the worktree. The
older hub has still saved any `--owns` change and incremented the revision.

When a queued entry that only shares a checkout waits behind a stalled active
entry, its `Stalled:` reason, and the stall notice posted to the Board, ends
with `Or: Shares checkout DIR with active entry tqe_A; move it: ...`, since
moving frees it at once.

## Team queue listing

`GET /v1/tasks/{id}/team-queue` returns the project's active entries in full,
in position order, followed by one newest-first page of history entries as
summaries (wi_f6c458bfb60667c6). The queue keeps every entry, so a listing
that carried each one's launch context grew past 4 MiB on 2026-09-30 and
stalled `tt team queue list` and the queue runner.

A history entry is `finished`, or `failed` and released with no owner
integration. Every other entry is active: `queued`, `launching`, `running`, a
failed entry that is not released, and a failed entry the owner recorded as
integrated, whose team the runner still closes. Block reasons, `blockedBy`,
stalls and arm waits read only active entries, so they are unchanged.

A history summary has `summary: true` and carries no `launch`, `close` or
`activities`; `tokens` stays. `teamShape` (`plan-review` or `plan-only`)
replaces the launch the Delivery view read its shape from. `reviews` keeps its
history, disposition and each round's number, sequences, candidate, reviewer
and verdicts, without findings, blockers or criteria; `scopes` and `focused`
are empty; follow-ups keep their item, message and finding ID and a title of
at most 200 bytes. `verification` keeps its state, commit and only the checks
that did not pass, are known failures or now pass. `release` keeps its ID,
state, commits, digest, receipt, supersession, `published` and
`integratedCoverage`, and drops `integratedPlan`, `integratedVerification`
and `reconciliations`; `plan` is
emitted as a zero object because the release type always carries it (read the
full job from `GET /v1/tasks/{id}/releases`). Acceptance, integration and
owner-integration evidence is cut to 500 bytes, owner-integration
`changedFiles` to the first 20, and acceptance `resolvedKnownFailures` is
left out. A summary stays under 8 KiB; 200 of them stay under 256 KiB.

Query parameters, all optional; anything else is a 400:

| Parameter | Meaning |
|---|---|
| `view=active` | active entries only; no history and no `history` object |
| `item=wi_ID` | only that item's entries, one per attempt, newest first, in any state and in full |
| `limit=N` | history page size, 1 to 200; default 50 |
| `after=P` | history entries below position `P` |

The `history` object gives `total` history entries, the page `limit` and, when
older entries remain, `nextAfter`, the `after` for the next page. Positions of
entries that have left `queued` never change, so the cursor is stable while
entries finish between pages.

`GET /v1/tasks/{id}/team-queue/{entry}` returns one entry in full, now with
its release and member activities too, as does `item=`. The runner and
`tt team queue add` read `view=active`; the done-save acceptance check and
the briefing's handler lookup read `item=`. `tt team queue list` takes
`--active`, `--limit N`, `--after P` and `--item wi_ID`, and after the entries
prints `history: showing N of TOTAL` and, when older entries remain, the
`older:` command for the next page. The Delivery panel shows `Showing N of
TOTAL finished` when older entries are not listed.

Compatibility: no schema change or migration; rollback is a binary swap. An
older tt, bridge or TailOS on this hub gets the default listing: active
entries in full, as before, then the newest 50 history summaries. The old
runner acts only on active entries, so it behaves as before; an older tt
looking for an accepted entry that has left the newest 50 misses it until tt
is released, and an older TailOS drops the plan-review label from finished
rows. A newer client on an older hub gets every entry in full, since the hub
ignores the parameters; every caller still filters by state or item, so the
results are correct, and the client's 64 MiB listing cap
(`api.MaxTeamQueueListResponse`) remains the backstop.

## Team queue launch errors

A launch writes its progress to the hub with four operations: `freeze`,
`attempt`, `started` (after a spawn, or after an uncertain spawn is found
registered) and `running`. A fifth, `unattempt`, takes back an attempt whose
registration the hub refused at the project agent cap (see "Project agent
cap"). Before wi_a3ca8b64d12365c2 the runner returned any refusal of those
writes and repeated the same write on the next relay tick, with no failure and
no Board signal; on 2026-09-30 one entry repeated a 413 `freeze` 154 times in
about 45 minutes while holding a slot.

The runner now classifies a refused launch write:

- **Permanent**: status 400, 404, 413 or 422, or a 409 ending in `team queue
  retry differs` (the same request ID with a different payload). Repeating the
  write cannot change these.
- **Transient**: everything else, including a 409 `entry revision changed` (a
  stale snapshot), other 409s, 401 and 403 (a relay credential fault, not the
  entry's), 408, 429, 5xx and network errors or timeouts. Other launch errors
  (reads, the host lock, or a failure the launch has already recorded) count as
  transient.

After any launch error the entry waits before its next launch on this relay:
15 seconds after the first consecutive error, then 30 seconds, 1, 2 and 4
minutes, and 5 minutes from then on. While it waits, the runner makes no hub
call for it; other entries and projects continue, and a serial queue stays at
that entry as before. A launch that returns without error clears the count.

When the hub refuses the same step, at the same entry revision and with the
same status, three times in a row, the runner fails the entry with `launch
<step> refused permanently by the hub after 3 attempts: hub: <status>
<message>`. With the backoff that is about 45 seconds to a minute after the
first refusal. Any other outcome in between restarts the count. Transient
errors never fail an entry. The hub posts the usual `Team queue failed and
requires owner action` notice once, with refs entry, item and escalation, and
`tt team queue list` shows `failed` with the ordinary failed reason (`--json`
carries `failure`). A parallel failed entry with no live runs is released on
the runner's next pass; a serial one halts the queue with the `serial-halted`
stall on the entries behind it.

A launching entry with no live run whose last write, and the item's last run
exit if later, is older than the stall grace (5 minutes) is now a
`nothing-running` stall of its own. An earlier team's exit alone never dates
it, so a relaunched item is not stalled at its claim. The list shows
it on the entry: `reason=Stalled: tqe_ID (wi_ID): an entry holds its slot,
handler lease and ownership with nothing running for it. Fix: ...`, and
`--json` has `stall` with the entry as its blocker. Entries queued behind it
show the same stall as before. At the first tick after the grace, even while
the entry is backing off, the runner posts one Board NOTICE, `A team queue
launch has made no progress`, with refs entry, item, cause and blocker. Its
text ends with `Last launch error on the runner: <step>: <error>.` from this
relay, on one line, without control characters and at most 500 characters (the
runner sends at most 300).

The notice's retry identity is the stall's
`queue-stall-<entry>-nothing-running-<revision>`, which ignores the error text,
so later ticks, a queued entry behind the same launch, and a relay restart
replay it rather than post another. A launch that was already stalled when a
permanent refusal began gets both the stall notice and, later, the failure
notice: two events, each posted once.

The backoff and the refusal count live in the relay's memory. A relay restart
retries at once and allows at most three more permanent refusals before the
same fail, whose notice the hub still posts only once. Not covered: a launch
whose earlier members are live while a later member's write keeps failing
backs off but is not a stall, and the claim step and the close path keep their
own handling.

## Team queue limit changes

`tt team queue limit` runs the host admission gates only when the change can
add load (`wi_4a11a0c0e2a23c08`). The gates are the host's session, polling,
relay binding and request budgets, a fresh complete census, and the free-disk
reserve.

| Change | Example | Host admission gates |
|---|---|---|
| Decrease | 4 to 3, `none` to 3 | skipped |
| Unchanged | 3 to 3, `none` to `none` | run |
| Raise | 3 to 4, 3 to `none` | run |
| To 1 (serial) | any to 1 | never ran |

`none` has no cap, so `none` to N is a decrease and N to `none` is a raise. A
decrease never adds load, so it is saved even when the host is over budget,
below the disk reserve, or has an expired policy or a stale census. The next
claim is still gated. A change to 1 (serial) was never gated: the gates apply
to parallel limits only.

Every other refusal still applies to a decrease. It is refused with `active
safety reservations exceed requested limit` while more entries than N are
active, and a parallel limit still needs a frozen repository and base for every
outstanding entry.

## Small-change lane

Feature `wi_f8d48780626165cc` (order #16786) adds a queue-only team template,
`small`, for small mechanical bug fixes. A Planned bug team is five sessions
(lead, planner, builder, verifier, reviewer); the small-change team is three.

**Who chooses it.** `tt team queue add` picks it by default for a small bug
(next paragraph), and the owner or the owner helper can name it with
`tt team queue add --item wi_ID --order SEQ --template small --owns PATH...`.
TailOS **Add team** and `tt team launch` do not offer it: `tt team launch
--template small` fails with `the small-change lane is queue-only: use tt team
queue add --template small`. `tt team queue list` shows `template=planned` or
`template=small` on each entry.

**Default lane.** Owner decision #19235 (`wi_2430de4c12e43df4`): a bug whose
fix fits at most three owned paths, including the doc that describes the
changed behaviour, is ordered on the small-change lane. When `--template` is
omitted, `tt team queue add` reads the item kind and counts the entry's owned
paths as submitted (`--owns`, or the handler's scope confirmation), then prints
its choice and why before it creates a worktree or writes to the hub:

| Item and entry | Lane | Printed line |
| --- | --- | --- |
| bug owning one to three paths | `small` | `template small: bug owning 3 paths (default)` |
| bug owning more than three paths | `planned` | `template planned: bug owning 4 paths, more than three (default) reason=paths` |
| bug with `--serial` or no ownership | `planned` | `template planned: bug with no owned paths, which the small-change lane cannot admit (default) reason=unscoped` |
| feature | `planned` | `template planned: a feature stays on Planned delivery (default)` |

The second and third rows are the fallback: the hub's eligibility rules below
are unchanged, so a default the small-change lane cannot admit goes to Planned
delivery with the reason shown. Count the doc when scoping a bug: a file, its
test and the doc that describes the behaviour are three paths, and a fix that
needs a fourth is Planned.

An explicit `--template` always wins. `--template small` is sent as given and
the hub admits or refuses it. Planned delivery for a bug needs a stated reason,
one of three exceptions, passed as `--planned-reason`:

- `paths`: more than three owned paths (the default reason when that is so);
- `schema`: a schema or migration change;
- `risk:TEXT`: a cross-cutting risk, named in TEXT.

`--template planned` on a bug owning at most three paths is refused without
`--planned-reason schema` or `--planned-reason risk:TEXT`, before any entry or
worktree exists. `--planned-reason` is refused for a feature, for an entry that
ends up `small`, and on every subcommand but `add`. `tt team queue requeue`
keeps its own rule: it copies the failed entry's template unless `--template`
is given.

**Reason record.** The queue entry has no reason field, and `tt team queue
list` does not show the reason. For every bug that `add` queues on Planned
delivery it posts one typed NOTICE after the hub confirms the entry, linked to
the item at its current revision and to the order message, naming the entry,
the template and the reason (`reason=paths (more than three owned paths)`).
Small entries and features get no notice. If that post fails, the entry stays
queued and `add` exits non-zero with the entry ID and the exact notice text to
post by hand. With `--json`, stdout is only the entry JSON and the `template
...` line goes to stderr.

The steward's proposals follow the same rule
([backlog-steward.md](backlog-steward.md#proposals)). The database handler's
queue proposal guidance does not state it yet; that is a filed follow-up
(`wi_2b66e2a634afa39d`).

**Eligibility, enforced by the hub.** Queue add refuses `small` with a 409
naming the rule unless:

- the item is a `bug` (`the small-change lane admits only bugs; queue a feature
  as Planned delivery`);
- the entry declares explicit ownership with `--owns`, not `--serial` and not
  empty (`the small-change lane needs explicit ownership (--owns), not
  --serial`);
- it owns at most three paths, the size of a file, its test and a doc (`the
  small-change lane owns at most 3 paths; requeue the item as Planned
  delivery`).

Any other template name, including the gallery's `solo` and `pair`, is a 400
(`unknown queue template "solo"; use planned or small`). A `tt team queue
scope` on a small entry may narrow it but never widen it past three paths;
splitting an owned directory into the files under it counts as narrowing, so
the runner's post-acceptance narrowing still applies. The cap counts paths,
not size: an owned directory is one path, so the explicit choice is the guard.
The runner also refuses to launch a small entry whose item is not a bug, before
it plans or spawns anything, and fails the entry with `the small-change lane
launches only bugs; this item is a feature: requeue it as Planned delivery`.

**Roster.** Lead (orchestrator and distinct verifier), builder and reviewer,
launched in that order with item-scoped names. The builder and reviewer are the
Planned delivery seats, byte for byte. The lead writes the frozen criteria
itself (no planner or plan reviewer) and runs the approved matrix itself: the
hub only requires the verifier to be admitted to the exact item and distinct
from the builder and every review-round reviewer. Review keeps the Planned
rules: an exact frozen candidate, at most two rounds, one disposition, and
acceptance only with the hub-saved passing receipt for the exact final SHA
rebased on tasks-hub. The lead prompt is in [team-launch.md](team-launch.md).

**Records: s1 is bounded.** The lane saves its records through operations that
already exist: queue freeze, attempt, started and running with the exact
agent, run and context digest (the Start evidence), the typed ASSIGN with frozen
criteria and scope, typed REVIEW and RESULT rounds and the typed disposition.
The lead sends the handler no Start, plan or assignment gate REQUESTs, with one
exception: after a scope amendment it sends one Start REQUEST at the new
revision (see [Queue team Start evidence](#queue-team-start-evidence)). The
verification plan freeze, the receipt import and the done save with queue
acceptance are validated hub operations the lead calls itself; see
[Validated team operations](#validated-team-operations).

**Host capacity.** A small entry without a frozen launch is still charged as a
five-seat bug team (`team_host_capacity.go`). That is conservative; once the
launch freezes, its three members are what count.

**Requeue as Planned.** When the fix needs more than three paths, a design
choice or a plan, the lead asks the owner to requeue. A still-queued small
entry is removed with `tt team queue remove --entry ENTRY`; a launched team
first ends through its ordinary disposition and closeout, because queue add
refuses an item with a live team. Then queue it as Planned with its reason:
`tt team queue add --item wi_ID --order SEQ --template planned
--planned-reason schema` (or `risk:TEXT`); a bug that now owns more than three
paths goes Planned by default with `reason=paths`. A new or revised
order goes through the usual scope confirmation first. An item whose small
entry failed and was released keeps its entry as history: retry it as Planned
with `tt team queue requeue --entry ENTRY --template planned [--owns PATH...]`
(see [Team queue amendments and retries](#team-queue-amendments-and-retries)).

## Validated team operations

A queued team (Planned delivery or small change) freezes its verification
plan, imports its receipt and saves done with queue acceptance by calling the
hub directly (`wi_26c0698de7d3eef2`). The hub validates each call against the
item's confirmed scope, its saved assignment and its running queue entry, and
refuses any mismatch with a named reason. A valid call needs no database
handler turn. The handler keeps intake, scope confirmation, item and order
records, a feature's completion report and every case the hub refuses.

| Operation | Command | Who may call it |
| --- | --- | --- |
| Plan freeze | `tt verification plan --item ID --file plan.json --request-id KEY --generation N` | the item lead, or the database handler |
| Receipt import | `tt verification receipt --item ID --file receipt.json --request-id KEY --generation N` | the current plan's exact verifier run, the item lead, or the database handler |
| Done save with queue acceptance | `tt work-items update --revision N --request-id KEY --status done --worktree DIR --branch B --commit SHA WI_ID` | the item lead, or the entry's leased database handler |

`plan.json` comes from `node scripts/verify-matrix.mjs plan context.json
plan.json` and `receipt.json` from the matrix run, as in
[objective-verification.md](objective-verification.md). `--generation` is the
number of verification records the item already has: 0 for its first plan, and
the plan's generation for its receipt. A feature's done save also carries its
completion report pin (`--report-id`, `--report-version`, `--report-digest`,
`--report-scope-revision`); the handler writes that report.

**Authority.** One rule admits the caller of the plan freeze and the receipt
import. The exact available database handler run passes as before, with no
queue entry, binding or digest, so a manually launched team (`tt team launch`,
no queue entry) keeps the handler path. Any other caller is checked in this
order, and each failure is a 409 with its own reason:

| Check | Refusal |
| --- | --- |
| the item has a running team queue entry | `no running team queue entry for this item; ask the database handler` |
| the entry is at the item's current revision with confirmed scope | `entry ENTRY is bound to revision N; the item is at revision M. Rebind it: tt team queue rebind ...` |
| the call names the entry's order | `entry ENTRY runs under order #N, not #M` |
| the run is the agent's current live run | `agent run changed; refresh identity` |
| the run's binding names this item, the entry's order and the entry's revision | `this run is not admitted to the item under order #N at revision R` |
| the request's context digest is the binding's | `context digest differs from this run's admitted context` |
| a plan freeze comes from the lead | `only the item lead or the database handler may freeze a verification plan` |
| a receipt import comes from the plan's verifier or the lead | `only the plan's verifier, the item lead or the database handler may import a receipt` |

`tt verification` reads the calling run's context digest the way `tt context
--json` shows it (`binding.contextDigest`) and sends it; a run with no item
binding, such as a handler, sends none. A rebind moves the entry and every
live team binding to the amended revision together and changes no run or
digest, so the same lead and verifier pass again after it. `tt verification
history` and `enrollment` admit the handler or any live run bound to the item,
with no digest.

**What is unchanged.** `verification_records` stays append-only: one insert
per record, the generation check and one record per request ID. An exact retry
is looked up before any authority check, so it returns the stored record even
after its author's run was rotated, retired or closed; the same request ID
with a different agent, run, digest or payload is refused (`retry payload
changed`). Every plan and receipt check is the one the handler path already
ran. A record written by a lead or verifier has `authorRole` (`lead` or
`verifier`); a handler's has none. `handlerAgentId` and `handlerRunId` hold
the author. There is no schema change.

**Receipt notices.** Every receipt import, including a handler's, tells each
dependent actor once with a directed NOTICE from the hub (`verification`):

- the item lead;
- each distinct reviewer of the item's current scope;
- the handler: the running entry's leased handler. With no running entry, or
  when that handler is closed, exited or retired, every available database
  handler in the project is told instead, and the record still lists the
  gone lease as `skipped`.

The importing agent is left out. The subject is `Verification receipt
imported: passing` or `Verification receipt imported: blocked`; the refs name
the `item`, `commit`, `generation` and `state`, and a blocked notice lists the
failed checks. A notice is linked to the item, creates no obligation and ends
`No reply is needed.` The record lists what was sent as `notices`: each
entry's `role`, `agentId` and `messageSeq`, or for a closed, exited or retired
recipient `skipped` with that status and no message. `tt verification history`
prints them.

**Failure rule.** The notices are inserted in the importing transaction,
before the single record insert. If any notice cannot be saved the import is
refused (`receipt notice to ROLE AGENT failed: ...; nothing was saved, retry
with the same request id`) and the transaction rolls back, so no notice exists
without its record and no record without its notices. A replay of a saved
import posts nothing.

**Done save.** A done save that carries an acceptance from an agent that is
not a database handler is validated before any write, while the item is still
at the revision the caller expects: the named entry waits on acceptance, it is
at the item's current revision with confirmed scope (otherwise the rebind
refusal above), the agent is the item's running lead on its current run (`only
the item lead or the leased database handler may accept this entry`), and
that run is admitted under the entry's order and revision. The acceptance is
then recorded in the same transaction as the item save, with every check the
handler's save gets (review disposition, the passing receipt for the exact
candidate, the verified repository and base), and any refusal rolls the whole
save back. The entry stays at its revision and the acceptance names the
item's new one. A lead's done save without `--worktree`, `--branch` and
`--commit` is refused while its entry waits. `tt team queue accept` stays
handler-only as the recovery path, and a handler's save is unchanged.

**Order of release.** A new `tt` against an older hub gets the older hub's
handler-only refusal, so release the hub before the Mini's `tt` and the team
templates.

### Measuring handler token share

The item's fourth criterion is a reading taken after release, by the owner
helper, on the held follow-up item `wi_13988ac5fe352ceb`. It is not part of
the build's acceptance.

- **Release point T.** The time the deployment record shows this feature's hub
  build live and the Mini's `tt` and templates updated. Both are needed: an
  old `tt` or prompt still routes through the handler.
- **Eligible item.** A bug or feature in this project delivered by a queued
  Planned or small team: its queue entry reached `finished` with the item
  `done`. Owner-integrated, failed, released-failed and manually launched
  items are excluded.
- **Before cohort.** The 10 eligible items whose entries finished most
  recently before T.
- **After cohort.** The first 10 eligible items whose entries were claimed
  after T, in claim order, so the whole team ran on the new prompts. This
  feature itself is excluded.
- **Per item.** Run `tt usage --item WI_ID --json`. Token units of a row are
  `input + cached + cacheWrite + output + reasoning`. The share is the
  `database_handler` role row's units divided by the item total row's units.
  Record each row's coverage state; an item whose handler or total row is not
  `measured` is listed and flagged, and the cohort share is reported with and
  without flagged items.
- **Cohort result.** The pooled share (handler units over total units across
  the cohort), the median per-item share and the handler requests per item,
  before and after side by side, with each cohort's Planned and small mix.
  The 26.0% in the system review is context, not the baseline.
- **Record.** The owner helper writes the table to
  `tailterm-artifacts/wi_26c0698de7d3eef2/a4-handler-token-share.md` and asks
  the database handler to save it on the follow-up item. No threshold is set:
  the result is the two numbers and the owner judges them.

## Queue team Start evidence

Bug `wi_5bd7ba47f56fab7f` (order #28219). Team briefings said that queue
admission is the Start, while the work-audit text every agent and handler
receives asked for a separate Start. Two items were marked deviant for following
their own briefing. There is now one rule, stated in the same words in the
work-audit text, the handler role text and the Planned lead, Small lead and
builder briefings:

> Queue team: admission is Start evidence for the admitted revision; after a scope amendment, lead sends one Start REQUEST at the new revision before any builder ASSIGN on that scope.

For handlers, the work-audit text and the handler role text add:

> A handler accepts that admission and asks a queue team for no other Start.

What it means:

- **Admission is the Start.** Admitting a team queue entry records the queue
  freeze, the attempt, and started and running with the exact agent, run and
  context digest. That is the Start evidence for the item revision the team was
  admitted at. The lead sends no Start REQUEST for it, and a handler does not
  ask for one.
- **Scope amendment.** An item revision saved after admission that changes the
  owned paths, the criteria or the scope. A revision that changes none of these,
  such as a priority change or a note saved as a receipt, needs no Start.
- **Order after an amendment.** The handler saves the amendment, confirms scope
  and rebinds the entry ([Rebind after an amendment](#rebind-after-an-amendment)).
  A message can be linked at the new revision only after that rebind. The lead
  then sends one typed Start REQUEST linked at the new revision. The builder
  ASSIGN on the new scope follows it, and the builder begins only on that
  ASSIGN.
- **Precedence is by Board sequence.** The Start REQUEST must precede the
  builder ASSIGN. The lead does not wait for the handler's reply before
  assigning. The handler answers the REQUEST with a RESULT, as it does for any
  live-team gate.
- **Outside a team queue entry nothing changes.** A separate exact Start is
  still required before implementation.

The sentence is `queueTeamStartRule` in `hub/cmd/tt/coordination.go` and
`queueStartRule` in `client/team-examples.js`. Both are compared with the
`queue-team-start-evidence` scenario in `tests/handler-allocation-cases.json`,
by `TestQueueTeamStartRuleIsSharedByAuditHandlerAndTeamTexts` and by
`tests/team-examples.test.js`, so a change in one language fails a test.

A running agent keeps the briefing it was launched with. The rule reaches a
handler when it is next replaced.

## Team queue amendments and retries

Bug `wi_4f66c2a118639128` (order #17395). A queue entry freezes the item
revision it was queued at, and a team's bindings freeze the revision they were
admitted at. Any item update moves the item to a new revision, so before this
change an amendment after queueing stranded the work: the runner failed a
queued entry (`queued item changed before launch`), a running team's handler
calls were all 409, and because an item could have only one entry, ever, the
way out was a replacement item. Two queue operations now keep the work under
the same item.

### Rebind after an amendment

`tt team queue rebind --entry tqe_ID --source SEQ` moves an entry to the item's
current revision. `SEQ` is the message that records the amendment; it must be
linked to the item. The entry keeps its ID, position, order message, handler
lease, ownership and frozen launch.

| Entry state | Rebind | Why |
|---|---|---|
| `queued` | yes | No team exists; only the entry's item revision moves. |
| `running`, no acceptance saved | yes, with the team's live bindings | Entry and bindings move together, so bookkeeping, replace-lead and team close keep agreeing. |
| `launching` | refused | The frozen launch plan, host journal and spawn attempts carry the old revision. Let the launch fail, then requeue. |
| `running`, acceptance saved | refused | The accepted candidate is pinned to an item revision and completion report. |
| `running`, released by an owner integration | refused | The entry no longer holds its work. |
| `failed` | refused | Not live. Release it, then requeue. |
| `finished` | refused | Not live. |

Every refusal is a 409 that names the entry and, where one exists, the
supported command.

Who may rebind: the owner's unbound CLI, or a database handler session. Any
available handler may rebind a queued entry; only the entry's leased handler
may rebind a running one. The item lead and team members may not. Amending the
item itself is unchanged and is not refused: the item update does not know
about the queue.

The handler's sequence after an owner-approved amendment:

1. Save the amendment to the item (a new item revision) and post or identify
   its message.
2. Confirm scope for the entry's order at the new revision
   (`tt work-items scope confirm`). A rebind without it is refused with
   `confirm scope for revision N and order #SEQ, then tt team queue rebind ...`.
3. `tt team queue rebind --entry tqe_ID --source SEQ`.
4. Carry on at the new revision: bookkeeping, new admissions, allocation
   intents, acceptance, team close and finish all use it.
5. If the amendment changed scope, expect the lead's one Start REQUEST at the
   new revision and answer it with a RESULT; see
   [Queue team Start evidence](#queue-team-start-evidence).

Until step 3, the entry is not failed. A queued entry stays queued, is skipped
when the hub picks the queue head (so later entries still launch), and lists
`reason=Item is at revision M; entry is bound to N. tt team queue rebind
--task T --entry tqe_ID --source SEQ`; claiming it is refused with the same
command. For a running entry, bookkeeping is refused with `entry tqe_ID is
bound to revision N; the item is at revision M. Rebind it: ...`.

What a rebind records, and returns as `rebinds` on `GET
/v1/tasks/{id}/team-queue/{entry}` and the `item=` listing (history summaries
leave it out): the old and new item revision, the old and new scope revision,
the order, the amendment's message, who saved the amended revision
(`amendedBy`), who approved the rebind (`approvedBy`, the authenticated caller,
plus the handler agent and run when a handler asked), the entry's state and
the time. For a running entry it also records each moved binding with its
agent, run, context digest and both revisions, and posts one NOTICE to the
item lead, linked to the item at the new revision, telling the team to re-run
`tt context` and link new posts at the new revision. `tt team queue list`
shows `rebound N→M`.

What a rebind never rewrites: a binding's agent, run, context digest, stored
context and creation time; the entry's frozen launch; scope confirmations;
bookkeeping receipts; and the bindings of closed or exited members, which stay
at the revision they were admitted at. Only `item_revision` moves, on the
entry and on live bindings of the entry's order. A bound agent may still link
a post at any revision its binding held, the admitted one included; a revision
it never held is refused as before.

A rebind does not change the entry's work order or ownership. Use `tt team
queue scope` for ownership; a different order needs a new entry.

### Requeue a failed entry

`tt team queue requeue --entry tqe_ID [--order SEQ] [--template planned|small]
[--owns PATH...] [--cwd DIR]` retries an item whose entry failed. It adds a
new entry for the same item: a new entry ID at the end of the queue, `attempt`
one higher than the failed entry's, `retryOf` naming it, bound to the item's
current revision. The failed entry is not modified and stays as history with
its failure, launch and notice. A new row, not a reset, because the runner's
request IDs, launch lock and journal are per entry ID and would replay the old
attempt.

Preconditions, each refused with a 409 naming the entry:

- the entry is `failed` and released (`entry tqe_ID is failed and not released;
  release it first: tt team queue release --task T --entry tqe_ID`), and was
  not integrated by the owner;
- it is the item's latest attempt, and the item has no live entry;
- no item-bound run is still live or uncleaned;
- the item is not done or dismissed;
- the order is recorded for the item and its scope is confirmed at the item's
  current revision.

The attempt copies the failed entry's order, template, host, checkout,
repository, ownership and base commit unless a flag overrides them. `--cwd DIR`
names another checkout of the same repository on the launch host and takes its
HEAD as the base. The attempt passes the same checks as `add`: a `small`
attempt needs a bug and at most three owned paths, and `--template planned`
retries a failed small entry as Planned delivery. The owner's unbound CLI or
any available database handler may requeue.

`tt team queue add` and a manual team launch still take only an item's first
entry. For an item that already has one they are refused naming it, for
example `item already has entry tqe_ID (failed); retry it with tt team queue
requeue --task T --entry tqe_ID`. `tt team queue list --item wi_ID` prints
every attempt, newest first, and the list shows `attempt N retry-of=tqe_ID`.
When the new attempt launches it takes over the item's lead record, which the
released attempt closed.

### Schema, compatibility and rollback

`team_queue_entries` loses its inline `UNIQUE(task_id,item_id)` and gains
`attempt` (default 1) and `retry_of`. SQLite cannot drop an inline constraint,
so the first start of the new hub rebuilds the table in one transaction: it
copies every row, column by column, into a table without the constraint,
checks the row count, and stops the migration instead of dropping data if the
old table has a column it does not know. Two indexes replace the constraint:
attempts of an item are distinct (`task_id,item_id,attempt`), and an item has
at most one live entry (queued, launching, running, or failed and not
released). `team_queue_rebinds` and `team_queue_rebind_bindings` are new. A
second start changes nothing.

An older hub binary runs on the rebuilt table; it only lost a constraint. Do
not roll back to it once any item has two entries, because it assumes one.
Check first, and expect no rows:

```sql
SELECT item_id FROM team_queue_entries GROUP BY task_id,item_id HAVING count(*)>1;
```

An older tt on the new hub lacks the two commands, and an older queue runner
still fails a queued entry whose item changed; update the relay hosts' tt with
the hub. Entries in JSON now carry `attempt`, and `retryOf` and `rebinds` when
set; older clients ignore them. The audit export does not yet include the two
rebind tables, and the Board does not yet show attempts or rebinds.

Not covered: release follow-through for a done item with a pending or failed
release (`wi_1a0349b7bf5dce22`), rebinding a launching entry in place, and
changing the work order of a live entry.

## Project agent cap

A project holds at most 32 open agents (`api.MaxAgentsPerTask`; a hub started
with `TAILTERM_MAX_AGENTS` enforces that lower value instead). Agent
registration refuses the next one with 409 `limit reached`. Before
wi_91e0cf6fa1dedbef the queue never looked at this cap: on 2026-09-30 a raised
queue limit admitted a fourth team, one member's registration was refused,
the entry failed with other members already started, and the owner had to
close them and supersede the item.

**What counts.** Every agent of the project whose status is not `closed` or
`exited`, whatever its role: team members, and also the persistent roles
(database handlers, backlog steward, deployer, owner helper) and agents that
are `done` or `retired`. This is the same count registration uses. Closing
finished teams and retired handlers frees seats; marking an agent done or
retired does not.

**Seats.** An entry's team needs 3 seats on the small-change lane, 5 for a
Planned bug and 6 for a Planned feature. Launches already admitted keep their
seats reserved until each member registers: the members of a launching entry's
frozen plan that are not `started` (its whole team before the freeze), and the
team of a manual launch reservation. A member whose spawn is uncertain may
already be registered, so the reservation can be one too high per launch for a
tick; it errs toward waiting.

**Reason.** When open agents, reserved seats and the team's seats exceed the
cap, the hub answers 409 with

    project agent cap: N open + M seats > CAP
    project agent cap: N open + R reserved + M seats > CAP

the second form when R is above zero. `tt team queue list` shows the same text
as the queued entry's reason, and `--json` carries it as `blockReason`.

**Three check points.**

1. **Claim.** After the slot and host checks and before anything is written,
   in serial and parallel queues. The entry stays queued at the same revision,
   with no reservation and no handler lease. The runner treats the refusal as
   a wait, like `all team slots are reserved`: no failure and no notice, and
   it claims again on a later tick, so the wait ends by itself once agents
   close.
2. **Limit raise.** `tt team queue limit` refuses a raise (a larger number, or
   0 from a fixed number) while the next team does not fit: the first queued
   entry's seats, or 6 when nothing is queued. The limit is left unchanged.
   Lowering or keeping the limit is not checked against the cap.
3. **Attempt.** Agents registered outside the queue (`tt spawn`, a handler
   rotation) do not honor reserved seats, so a launch can still meet a full
   project. Each member's `attempt` is refused with `project agent cap: N open
   + 1 seats > CAP` when the project is full. The member stays `unstarted`,
   and the launch waits and retries with the launch error backoff.

**A refused registration.** If the project fills between a member's attempt
and its registration, the hub refuses the registration. Registration comes
before any host session, so nothing was started. The runner sends `unattempt`,
which returns the member to `unstarted` only while no agent has that member's
ID, and then backs off as for any transient launch error with

    spawn NAME: project agent cap: N open + 1 seats > 32: register agent: hub: 409 limit reached

The entry is not failed, no `Team queue failed and requires owner action`
notice is posted, and members already started are kept and not spawned again.
N is the runner's own roster count, and its text always says 32 even on a hub
with a lower cap; the hub's reasons use the enforced value. If the `unattempt`
write itself is lost, the member stays `uncertain` and the next launch fails
the entry under the unchanged never-respawn rule.

A partly launched team that is held this way keeps its slot, handler lease
and ownership until seats free. After the stall grace it shows as the
`nothing-running` stall only if none of its members is live; the stall notice
then carries the text above as the last launch error.

**Interaction with the concurrency limit.** The limit is a ceiling on teams,
the cap is a ceiling on agents in the project, and host capacity is a third,
per host. Each is checked on its own and the tightest one holds the queue.
Raising the limit never raises the cap: four Planned feature teams need 24
seats beside the persistent roles and every agent not yet closed. The queue
keeps its order, so a head entry that does not fit holds the entries behind it,
even a smaller team.

Update the hub and the relay hosts' tt together. An older runner on the new
hub reports the claim refusal as an error on every tick instead of waiting
silently (it does not fail the entry), and a new runner on an older hub has no
`unattempt` to call.

Not covered here: showing the cap in `tt team queue policy` and a per-project
cap setting (`wi_c8119b78f9045698`), reserving seats against registrations
made outside the queue, and letting a smaller team pass a held head.

## Team queue artifact and session temp retention

Closeout and `tt team queue sweep-worktrees` (described in
`docs/team-launch.md`) also retire two things a finished team leaves outside
its worktree: the verifier's clean checkouts under the artifacts tree, and
Claude Code's per-directory session temp folders. Receipts, logs and plan
files are never removed. The sweep, and only the sweep, also trims one
regenerable cache out of the checkouts it keeps (see "Cache trim").

```sh
tt team queue sweep-worktrees [--apply] [--json] [--min-idle 24h] [--accepted-after 24h] [--artifacts DIR] [--cwd DIR]
tt team queue sweep-worktrees --journal [--limit 20] [--json]
```

The relay runs the same pass with apply on a schedule on each host: hourly
with `--min-idle 6h` by default, set or switched off in
`~/.config/tailterm/relay.json` (`worktreeSweep`, `worktreeSweepInterval`,
`worktreeSweepMinIdle`, `worktreeSweepLowSpaceGiB`). Each attempt writes one
line to `worktree-sweep.jsonl`, which `--journal` prints, and the owner helper
gets one notice per low-space episode. `docs/team-launch.md` ("Scheduled
sweep") has the settings, the skip reasons, the matrix lock rules, the journal
fields and the notice rule. The same section describes the Go build cache
trim (`goBuildCacheTrim`, off by default), which writes its receipts to the
same journal.

Without `--apply` it is a dry run that writes nothing: no manifest, no
receipt, no ref. It lists each verifier checkout and session temp folder with
what it would do and, for a `would-remove` row, its size. `--apply` removes
exactly what the dry run under the same rules reports. Never run `--apply`
as a check; it deletes.

### Verifier checkouts

- A verifier checkout is a linked Git worktree with a detached `HEAD` under
  `<artifacts>/<itemId>/`, where the item folder is named `wi_` and 16 hex
  digits. A worktree there with a branch checked out (for example
  `<itemId>/builder`) is a working checkout and keeps the ordinary worktree
  rules. Folders not named for an item are never touched.
- The artifacts root is `<main checkout>-artifacts`, the sibling of the
  repository's main worktree. `TAILTERM_ARTIFACTS` overrides it, and on the
  sweep `--artifacts DIR` overrides both.
- A wrong root never frees a checkout. `--artifacts` and `TAILTERM_ARTIFACTS`
  must be absolute: the sweep refuses a relative one before it reads the hub,
  and closeout prints one line to stderr and leaves the artifacts tree and
  every session temp folder alone.
  A detached checkout under a folder named for an item but outside the root in
  use is kept, as `item-active` while its item runs and as `retention`
  otherwise, and the sweep warns on stderr with the count and the root it
  used.
- An item's checkouts go once the item is released, or once its acceptance is
  N old. Released means its newest finished queue entry has a release job in
  state `released`, a published release, or an owner integration. Accepted
  means that entry has a saved acceptance; its `acceptedAt` starts the wait.
  N defaults to 24 hours: `--accepted-after` on the sweep, and
  `TAILTERM_ARTIFACT_ACCEPTED_AFTER` (a Go duration such as `12h`) for
  closeout. Zero means no wait. A negative `--accepted-after` is a usage
  error; a negative or unreadable environment value prints one line to stderr
  and uses 24 hours.
- The checkout directory is the unit. Everything inside an eligible, clean
  checkout is deleted: tracked sources, `node_modules` and build output.
  Everything beside it in the item folder stays as it is: receipts, logs,
  plan files, summaries and the manifest.
- Referenced files are never removed. A checkout is kept whole, as
  `evidence`, when a receipt text names a path inside it that exists there.
  Receipt texts are queue acceptance, integration and owner-integration
  evidence, tracked files under `docs/` on `tasks-hub`, and the JSON files in
  the item's artifact folder, outside any checkout, whose name contains
  `receipt` or `plan`. A path is matched absolute, without the macOS
  `/private` prefix, and relative to the home directory (`~/...`). A reference
  whose tail is missing still counts through its longest existing leading
  part. A mention of the checkout directory alone, or of a sibling such as
  `<checkout>-logs/`, does not keep it. Logs are not read: they name every
  file a build touched. A receipt or plan file that cannot be read, or is
  larger than 4 MiB, keeps the item's checkouts as `remove-failed`.
- The commit need not be on `tasks-hub`: the checkout is a clean copy, and the
  candidate's own branch and worktree stay under the ordinary rules. When no
  branch or remote holds the commit, removal first points
  `refs/tailterm/retired/<itemId>/<checkout name>` at it, so a SHA a receipt
  names cannot be garbage-collected. These refs are not pruned.
- The removal is Git's `worktree remove`, never `--force`, re-checked
  immediately before it runs.

Before a checkout is removed, its manifest is written beside it as
`<artifacts>/<itemId>/<checkout name>.removed.json`, mode 0600 (a timestamped
`.removed-<time>.json` when that name is taken). If the manifest cannot be
written, nothing is removed; if the removal then keeps the checkout, the
manifest and a ref made for it are taken back. Fields: `version` (1), `at`,
`source` (`closeout` or `sweep`), `itemId`, `entryId`, `path`, `head`,
`pinnedRef` when one was needed, `basis` (`released` or `accepted`),
`basisAt`, `bytes`, `files`, and `entries`: one `{name, bytes, files,
tracked}` for each top-level entry of the checkout.

### Session temp folders

Claude Code keeps one folder per session directory:
`<temp>/claude-<uid>/<key>/`, where `<temp>` is `/tmp` (`/private/tmp` on
macOS) or `CLAUDE_CODE_TMPDIR`, and `<key>` is the directory with every
character that is not a letter or digit replaced by `-`. Codex and other
runtimes have no such folder, so there is nothing to remove for them.

Candidates are the folders of a team's directories on this host: its queue
entry's `cwd`, accepted worktree and integration worktree, and the cwds of its
item's agents. Closeout looks only at its own entry and that item's agents;
the sweep looks at every entry and item-bound agent. A folder is removed only
when all of these hold:

- It is a real directory directly under the temp root. A symlink there is not
  followed and not listed.
- Its directory is not the main checkout, and does not share the main
  checkout's key: that folder is shared by owner, handler and steward
  sessions and is never removed. A directory that still exists outside every
  linked worktree is treated as shared too.
- No directory in use has the same key (`in-use`). In use means the cwd or
  worktree of a queued, launching, running or unreleased failed entry, or the
  cwd of an agent that is not closed, and the linked worktree that contains
  such a path: a session started at a worktree's root is protected while any
  directory below that root is in use. Closeout and the sweep apply the same
  rule. The key is lossy, so two directories that differ only in punctuation
  share a folder and protect each other.
- Sweep only: nothing anywhere below the folder, at any depth, changed within
  `--min-idle` (`recent`). A folder the sweep cannot read through counts as
  changed. This covers a session that is not on the roster.
- Its items' receipt and plan files can be read (`retention` otherwise). With
  no artifacts root (a relative or unset one), or a root that is not the
  default `<main checkout>-artifacts` and is not a directory or has no folder
  for one of its items, every session temp folder is kept, at closeout and in
  the sweep, and the reason names the root. The default root is trusted: an
  item with no folder there has no receipt or plan file, so nothing can cite
  the folder.
- No receipt text names it (`evidence`): queue evidence, tracked docs, and the
  receipt and plan files of the items it belongs to.
- No Git repository or kept worktree remains inside it (`nested`). Scratchpad
  worktrees removed earlier in the same pass do not count.

When the sweep applies, it removes a folder in two steps: it renames the
folder to a tombstone beside it in the temp root, then deletes the tombstone
(see "Cache trim" for the tombstone rules). Closeout deletes the folder
directly, as before.

### Cache trim

The sweep trims a regenerable cache out of a checkout it keeps, so a checkout
held for its receipts or its branch stops holding hundreds of megabytes of
installed packages. The dry run reports it as `would-trim`, `--apply` and the
scheduled sweep as `trimmed`. Closeout never trims.

**Allowlist.** Exactly one path: `<checkout>/node_modules`. Nothing else is
trimmed anywhere: no Go build or module cache, no `home` directory a
verification run made, no `.build`, no `dist`.

**Never touched.** The sweep never deletes, and never deletes inside, the
shared user caches: `~/Library/Caches` (so `~/Library/Caches/go-build`),
`~/go` (so `~/go/pkg`), `~/.npm`, or the relay's own `GOCACHE`, `GOMODCACHE`,
`GOPATH` and `npm_config_cache`. Clearing the shared Go caches is a standing
owner rule: never. Nothing outside the worktrees root
(`<main checkout>/.build/worktrees`), the artifacts root and the session temp
root is ever a candidate. A checkout or cache that lies in, contains or is the
same file as one of those paths is kept `protected`.

**Finished.** A checkout qualifies only when all of these hold:

- It is a verifier checkout of the item, or the `cwd`, accepted worktree or
  integration worktree recorded on a `finished` queue entry of the item with a
  saved acceptance on this host; and it lies in the worktrees root or the
  artifacts root.
- The item's newest queue entry on any host, by position and of any state, is
  `finished` and is accepted or released. A newer failed entry, released or
  not, an abandoned entry, or a newer queued, launching or running retry
  disqualifies the item.
- No entry of the item is active, and no agent bound to it on any host is
  other than closed.
- The pass keeps the checkout as `retention`, `evidence`, `dirty`, `unpushed`
  or `nested`. A removable checkout is removed whole instead. A checkout kept
  as `in-use`, `locked`, `recent`, `item-active` or for an operation in
  progress is left alone, and the lock, operation and `--min-idle` rules are
  checked again directly, because the first reason reported can hide a later
  one.

**The cache itself.** `node_modules` is trimmed only when it is a real
directory, holds npm's install marker `node_modules/.package-lock.json`, is
ignored by Git with no tracked file inside, holds no `.git` entry, was last
changed more than `--min-idle` ago, and no receipt text names a path in it
(queue evidence, tracked `docs/` files, and the item's receipt and plan
files). Otherwise it is kept, with the cause in the detail: `unproven` (a
symlink, no marker, tracked or not ignored), `nested`, `recent`, `evidence`,
`locked`, `operation`, `protected`, `moved` or `remove-failed`. A
`node_modules` that is itself a symlink is kept and the link left in place.

**How it is deleted.** One function is the only route to a trim, and it never
deletes by pathname:

1. The checkout is opened as a pinned directory. It must be the same file
   that was classified, and its `.git` link must still name the same worktree;
   a checkout or parent folder swapped meanwhile is kept `moved`.
2. The intent is written and synced to `worktree-sweep-state.json` in the
   relay state directory: the checkout's path, its device and inode, the root
   it lies under, and a generated tombstone name (`.tt-sweep-trash-` and a
   random suffix). If that write fails, nothing is renamed.
3. Under the matrix lock's mutex (see `docs/team-launch.md`), `node_modules`
   is renamed to the tombstone inside the pinned directory.
4. The tombstone is deleted through the pinned directory, and the intent is
   cleared. A symlink inside the cache is unlinked with it; its target is not
   followed. Read-only directories are not made writable: a delete that fails
   is `remove-failed`, and the next pass tries again.

A pass that died between steps is settled by the next applying sweep of the
same roots, before it trims anything. It deletes the tombstone only when the
recorded directory is still the same file under the root it was recorded
under, and the tombstone carries the reserved prefix and is a real directory.
Anything else (the directory gone or replaced, the tombstone missing or a
symlink, `node_modules` never renamed) deletes nothing, drops the intent, and
is recorded once as kept `moved` with the cause. A tombstone with no intent is
never deleted by name.

A manual session that is not on the roster can lose `node_modules` in a
finished checkout it works in; `npm ci` restores it, and `--min-idle` and the
matrix lock protect a checkout in use.

### Receipts and report

All three kinds are recorded in `worktree-cleanup.jsonl` with the ordinary
worktree receipts, one line per decision, and kept outcomes once. Their lines
add `kind` (`artifact-checkout`, `session-temp` or `cache`), `bytes`, and for a
removed checkout `manifest`. A cache's `path` is the `node_modules` directory
and its action `trimmed`. `--json` rows add the same `kind`; existing keys and
totals keep their meaning, and `totals.actions` gains `would-trim` or
`trimmed`. The text report prints
`would-remove PATH (artifact checkout; item wi_x released; 1.7 GiB)`,
`would-remove PATH (session temp; 78.0 MiB)` and
`would-trim PATH (cache; regenerable cache in a finished checkout of wi_x; 412.0 MiB)`.
A folder that holds only empty directories reports `0 B`.

Keep reasons, in the order checked for a verifier checkout: `locked`,
`missing`, `in-use`, `item-active` (an entry of the item is queued, launching,
running or failed and not released, or an agent bound to the item is not
closed), `retention` (accepted less than N ago, the item has no queue
record: unknown is never treated as released, or the checkout is outside the
artifacts root in use), `nested`, `recent` (sweep
only), `evidence`, `operation`, `dirty`, and `remove-failed`. `unpushed`
applies only to ordinary worktrees and branch checkouts.

### Limits

- Closeout runs for finished, accepted entries on their launch host while the
  project has an active entry there. It re-reads the project at most every 15
  minutes for an entry that still has a worktree, a session temp folder or a
  detached checkout one level under its item folder. Failed, dismissed or
  older teams' folders, and checkouts nested deeper, go to the sweep.
- Closeout has no idle rule. It removes a temp folder only for a finished,
  accepted entry whose directories no live agent uses, so a manual Claude
  session started in a finished team's worktree, and not on the roster, can
  lose its temp folder. The sweep's `--min-idle` protects such a session.
- The sweep reads every page of each project's queue history, so an old item
  keeps its record. An item whose entries were never queued has none and its
  checkouts are kept as `retention`.
- The main checkout's session temp folder, artifact folders not named for an
  item, and build output that is not inside a checkout are out of scope.
- The cache trim does not reach Go build or module caches in a verification
  run's private home, `.build/go`, or any cache under a folder not named for
  an item: nothing on disk proves the harness made them.

## Team queue token budget

Feature `wi_1d2fbde2c149989e`, owner order #30088. The settings, sources and
commands are in [usage-accounting.md](usage-accounting.md#token-budget); this
section is what the queue does with them.

**Admission.** With at least one budget row for the project, the claim skips a
queued entry whose estimate the remaining budget cannot cover, the same way it
skips an entry that waits for a rebind. The skipped entry stays `queued`,
nothing is written, and its claim gets the existing "not queue head" conflict,
which the runner treats as quiet. In a parallel project a later entry that
fits is claimed past it. In a serial project the runner tries only the first
queued entry each pass, so a budget-held head keeps the later entries queued
until the budget covers it: the head skip helps only parallel projects.
The estimate is the item's saved estimate or, without one, the project's lane
default for the entry's template and Go race. The check runs only for queued
entries: a launching or running entry is never failed, held or changed by it.
With no budget row the queue admits and lists exactly as before.

Admission also reserves for the teams already admitted, so two entries that
each fit alone are not both admitted past the remaining budget. For every
entry that holds a slot (launching, running, or failed and not yet released)
it sets aside `max(0, estimate - team spend the source already reflects)` and
an entry must fit what is left after the reserve and that sum. On the
allowance source every reported turn of the team is reflected, so the
reservation falls as the team reports usage. On the provider source only the
team's turns at or before the reading's capture instant are reflected, so the
reservation shrinks only when a newer reading arrives. A failed entry reserves
until it is released; a queued entry reserves nothing. A queued entry that
only a failed entry's reservation holds keeps its stall behind that entry, so
the release it waits for stays visible. That holds with no shared path too: in
a parallel project with a free slot, a free handler and no path overlap it
gets a `failed-entry` stall naming the first failed entry, in queue order,
that still reserves on the entry's host and still has runs that are not closed
and cleaned; its reason is the stall's followed by
`Its reservation holds this entry:` and the budget reason. When no failed
entry has such a run, the runner releases it on its next pass, and the queued
entry shows the budget reason and no stall until then. An entry that an
earlier reason explains shows that earlier reason, an entry blocked by a
holding entry it overlaps keeps its budget reason, and neither gets this
stall. When
something is reserved the reason carries
`, 700.00K reserved for 1 admitted team: tqe_1f0c…` (or
`, 600.00K reserved for 2 admitted teams: tqe_1f0c…, tqe_8a52…`) before its
source, naming the reserving queue entries in queue order.

**The reason.** A budget-held entry shows its reason as `blockReason` when no
earlier reason (slots, host capacity, the agent cap, a handler, ownership)
already explains the wait:

```
Token budget (claude five_hour): needs about 44.00M tokens (default, planned); 31.20M remain before the reset at 2026-10-08T22:00:00Z, reserve 10% (source: provider reading)
Token budget (claude five_hour): no usable source. Provider reading for host mini is not reported and no reset time is set
Token budget: no estimate and no lane default; set one with tt usage defaults set
```

It is recomputed at every list and claim, so the entry is admitted after the
reset, a new provider reading or a changed budget with no other action.
Waiting for the budget is ordinary waiting, like host capacity: a budget-held
entry has no `stall`, and a stall notice for it is refused.

**The list.** An entry whose item has no saved estimate carries
`estimateDefault` (`{"tokens":70000000,"lane":"planned","goRace":true}`), and
its text `budget:` line ends ` · default estimate 70.00M (planned, Go race)`.
An item with a saved estimate carries none. Every entry carries `admittedAt`
once it has been claimed.

**The entry hold.** When a running entry's team has spent more than 3 times
its item's saved estimate, the entry carries `budgetHold` and the list prints:

```
  held: past 3 times its token estimate since 2026-10-08T19:00:00Z (team 36.10M, estimate 12.00M); no new turns start. Continue: tt usage hold continue --project tsk_… --entry tqe_…. Stop, from an unbound owner shell: tt team queue fail --task tsk_… --entry tqe_… --reason TEXT
```

The entry stays `running` and keeps its slot, handler lease and ownership. Its
reason starts `Held past 3 times its token estimate`. The hold is a row beside
the entry (`team_queue_budget_holds`); the project pause barrier is not used
and the project stays active. It refuses no write. The hub's wake-job lease
returns no job for a held run, and the relay delivers no inbox wake to one.

- **Continue** is the queue operation `budget_continue` (`tt usage hold
  continue`), allowed for the owner and the owner helper. The entry's revision
  advances and the hold becomes `continued`.
- **Stop** is the existing `fail` (`tt team queue fail`, from an unbound owner
  shell). The hold becomes `stopped` in the same transaction, and the team's
  runs stay unwoken while the entry is failed and not released. The failed
  entry is then reconciled and released as any other.
- A hold in force also ends as `stopped` when the entry finishes, is released,
  is recorded as owner-integrated, or its team closes with the item open.

A queued entry that would stall behind a held running entry (the held team is
idle, by design) has no `stall`. Its reason is `Waits behind entry tqe_…, held
past 3 times its token estimate until the owner helper continues or stops it`,
and no stall notice is posted: the hold's own ask already carries the
decision. If that entry is also blocked by something else, it shows the hold
wait until the hold is resolved.

**Admission time.** `team_queue_entries.admitted_at` is written by the claim.
The team's token figure, the warning and the hold count runs bound at or after
it. At the upgrade an entry that still has its launch reservation takes the
reservation's time; any other entry keeps an empty value and is read from its
creation time, as before. A backfilled entry's team figure can therefore be
smaller than before, never larger.

Limits: every budget row applies to every entry whatever its runtime; in a
serial project a budget-held head holds the whole queue; in a parallel one a
large held entry can be passed by smaller ones for as long as the budget does not
cover it; and stopping a held team does not end its sessions, which is the
failed-entry cleanup above.
