# Queue releases: 2026-09-25 and 26

Items delivered by the shadow-week team queue (`tsk_e7af3c28a444b09a`) after the overnight release
[`cf80b46`](../overnight-cf80b46/README.md), each merged to `tasks-hub` and released from the
owner's Claude Code session on the Mini. On 2026-09-25 from 09:31 to 18:00, and on 2026-09-26
until 15:00, the owner authorized that session to make decisions, merges and deploys on its own
recommendation. On 2026-09-26 the owner also said to release the queued items as they were
accepted. Every item was built and reviewed by Tailterm agent teams on the board.

The Discord server, application and owner IDs are redacted from the plan and receipt copies in
the `hub-*` folders. The unredacted files are in `.build/*-release-*/`, and each pinned receipt
SHA-256 is of the unredacted receipt.

## Releases

| Tasks-hub | Item | What it does | Released |
|---|---|---|---|
| `e36515f` | sr-only CSS (`wi_2e5375ae99db3d26`) | Hides the Search labels; restores 8 skipped layout checks | TailOS `1d38b9a6` |
| `256dc65` | Relay status (`wi_3a2b456f2e997eb4`) | `tt relay --status` clears errors after a successful pass | Mini `tt` |
| `68de44e` | Features wrap (`wi_9e4d75eddd8d1212`) | Features header on one row at 1024px | TailOS `239b2b81` |
| `cf6b1cf` | Handler-save (`wi_657ebc148560e5e9`) | Scope confirmation before admission; bookkeeping saves keep Starts | Hub `20260925-handler-save-cf6b1cf`, Mini `tt`, TailOS `0d595b00` |
| `33d3ab7` | Handler priority (`wi_7149909a1085b653`) | Live-team gates first; handler latency in `tt message-checks` | Hub `20260925-handler-priority-33d3ab7`, Mini `tt`, TailOS `64b64c0c` |
| `6235924` | Parallel dispatch (`wi_0e910b68c82e91d6`), activity monitor (`wi_c738cdd5f9ef637b`), 390px roster fix (`wi_f415c845559a65b9`) | Up to N teams per project (default 1); relay agent activity states and token totals | Hub `20260926-parallel-activity-6235924`, Mini `tt`, TailOS `53b19023` |
| `91a55e0` | Activity handler fix (`wi_24005da18408ab12`) | Classifies the database handler; no stale first report | Mini `tt` |

## Hub releases

| Release | Backup (before) | Size | SHA-256 | Integrity | Schema |
|---|---|---|---|---|---|
| `20260925-handler-save-cf6b1cf` | `before-handler-save-cf6b1cf.sqlite` | 120,373,248 | `2471c1e5…` | ok, 0 FK | new `work_order_scope_confirmations`, `work_order_bookkeeping` |
| `20260925-handler-priority-33d3ab7` | `before-handler-priority-33d3ab7.sqlite` | 120,373,248 | `8c83ce04…` | ok, 0 FK | none |
| `20260926-parallel-activity-6235924` | `before-parallel-activity-6235924.sqlite` | 120,373,248 | `1102ab37…` | ok, 0 FK | reservations rebuilt (transactional); single-active and single-handler unique indexes replaced or dropped; new queue settings, host policy and usage, item leads, agent activity tables |

Before `6235924`, the combined migration was rehearsed on a copy of the `33d3ab7` backup: the new
hub migrated it (integrity ok, no FK violations, 14 projects, queue at limit 1), and the
`33d3ab7` hub opened the migrated copy. Rollback binaries: `.build/prev-*` (hub, bridge) and
`~/.local/bin/tt.prev-*` (Mini).

## Verification before each merge
Each merge ran, on the exact commit: `go vet ./...`, `go test ./...`, `npm test`, and the browser
suites the change touched (board-layout, hub-cache, task-form, project-compact-ui, and from
`6235924` also parallel-team and activity), in Chromium and WebKit; plus `-race` on the queue,
runner, migration and activity tests where they changed. The owner-side pre-merge run of the
activity monitor found a 390px roster clipping regression that the team's runs had missed; it was
fixed (`wi_f415c845559a65b9`) before release.

## Found in production
- The docs queue entry failed with "queued item changed before launch": the handler bumped
  the item revision after queueing. Fixed by handler-save.
- Parallel dispatch requires the leased handler to run `tt team queue accept`. A handler
  session started before `6235924` doesn't know to, so its entry waits in "Waiting for handler
  acceptance" until told.
- Codex requests failed with 401 from 15:40 to about 17:13 on 2026-09-25, then recovered on the
  provider side.
- The activity monitor could not classify the database handler and posted a stale first
  snapshot. Fixed in `91a55e0`; verified live (`working`, current `lastEventAt`).

## Known open
- `project-work-items-browser` dispatch-notice failure: product cause found in
  `client/task-hub.js` setupHandler; fix in progress (`wi_21a14f6e032ab182`).
