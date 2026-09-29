# Queue releases: 2026-09-25 to 28

Items delivered by the shadow-week team queue (`tsk_e7af3c28a444b09a`) after the overnight release
[`cf80b46`](../overnight-cf80b46/README.md), each merged to `tasks-hub` and released from the
owner's Claude Code session on the Mini. On 2026-09-25 from 09:31 to 18:00, and on 2026-09-26
until 18:00, the owner authorized that session to make decisions, merges and deploys on its own
recommendation; from 19:37 that covered decisions only, until 23:59. On 2026-09-26 the owner also
said to release queued items as they were accepted, and at about 19:25 extended that to the whole
queue until the project's deployment agent takes over releases. Every queued item was built and
reviewed by Tailterm agent teams on the board. Two fixes were made out-of-band by the owner's
session, each announced on the board: `4b5adb2` (under the recommend-and-act window) and `c343819`
(chosen by the owner in the terminal).

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
| `7a49ec8` | Project work items (`wi_21a14f6e032ab182`) | The dispatch notice keeps its text when the ID-based fill runs | TailOS `ce71c43f` |
| `3f083e1` | Docs (`wi_9f1fdba7cc969c0d`) | Task docs cover `tt team launch`/`queue`, `tt withdraw`, `tt close --team` | none (docs only) |
| `0cab2fe` | Relay bindings (`wi_0f9f8dcdf0403899`) | The relay archives bindings of closed agents; 212 of 218 on the first pass | Mini `tt` |
| `7ab5100` | Review convergence (`wi_3d6a4e3d1bf99a08`) | Numbered criteria, at most two general review rounds, focused-fix verification | Hub `20260926-review-convergence-7ab5100`, Mini `tt`, TailOS `3cc48b1b` |
| `4b5adb2` | Owner-accept (out-of-band) | The owner can resolve an owner-decision review disposition | Hub `20260926-owner-accept-4b5adb2` |
| `34944cf` | Objective verification (`wi_f23f767415ef9b30`), known failures (`wi_f148716909c88f9d`) | An independent verifier runs the owner-approved check matrix; named known failures and bounded retries | Hub `20260926-verification-34944cf`, Mini `tt`, TailOS `e52775cd` |
| `8b1f4d4` | Owner obligations (`wi_deb2ce3cda1968d0`), browser fix (`wi_5a411a2a78e3ad9e`) | Requests to the owner are tracked with due times, shown as Discord cards and escalated | Hub `20260926-owner-obligations-8b1f4d4`, Mini `tt`, TailOS `892508b0` |
| `c343819` | Receipt size (`wi_4aab49054893c8ce`, out-of-band) | The verification route accepts 1 MiB, so a full receipt can be saved | Hub `20260927-verification-body-c343819` |
| `8e32dc0` | Deployment agent (`wi_c526e6f62370fbe9`) | Persistent project deployment role: release jobs, fenced release runner, rollback and receipts (not yet activated) | Hub `20260927-deployment-agent-8e32dc0`, Mini `tt`, TailOS `bff3ee3c` |
| `9c99ddc` | Reviewer model (out-of-band) | Planned-template reviewers launch on Opus 5.5 instead of Fable, which hit its usage limit at every launch | Mini `tt`, TailOS `5c9063a5` |
| `5480dae` | Queue acceptance (`wi_4656983381013e99`, out-of-band) | A verified team is accepted on its verified base and worktree, not the queue-time base | Hub `20260927-queue-accept-5480dae` |
| `acfe2d0` | Token accounting (`wi_ca6a62f5e74114b4`) | Per-request token ledger by item, phase and role, with optional prices; `tt usage` and a TailOS Usage view | Hub `20260927-token-accounting-acfe2d0`, Mini `tt`, TailOS `784c6588` |
| `3c2de08` | Claude wake (`wi_8401f9220e95bff5`) | The relay wakes an idle Claude Code agent inside its own tmux session when it is safely idle | Hub `20260928-claude-wake-3c2de08`, Mini `tt`, TailOS `111e8c19` |
| `f3b1c19` | Owner-accept for follow-ups (`wi_be41cf76f6148a9c`, out-of-band) | Owner-accept also resolves a follow-ups disposition once verification is ready | Hub `20260928-owner-accept-followups-f3b1c19` |
| `0ce60c0` | Repository identity (`wi_0842a90ecb62e82a`, out-of-band) | A plan naming the repository root matches the queue entry's `.git` | Hub `20260928-repository-identity-0ce60c0` |
| `2488e75` | Profile-sync order dependency (`wi_21ba42f8f0542dc0`) | Test binaries built once before checks; profile-sync passes first in a fresh checkout | none (tests and scripts only) |
| `172becd` | Verification criterion (`wi_def1f8fa52bec739`) | Verification-owned review criteria are judged by the eligible receipt; no owner-accept needed | Hub `20260928-verification-criterion-172becd`, Mini `tt`, TailOS `bc20b212` |
| `a50962c` | Team models (out-of-band, owner request) | Planned-team roles on GPT-6 Astra and Sol run on Claude Opus 5.5 high | Mini `tt`, TailOS (`a50962c` build) |
| `6692a57` | Runner integration (merge of `6195085`) | Verifier temporary homes are cleaned up; test binaries prepared for profile-sync | Mini `tt`, TailOS (`6692a57` build) |
| `1e8e8df` | Claude wake prompt area (`wi_7757e967b69a5ba6`, merge of `2a0bc33`) | Claude agents wake on needs_input, with prompt-suggestion text present, and dialog detection reads only the active prompt area | Mini `tt` |
| `66b55ed` | Automatic queue acceptance (`wi_b4ec031a206d1326`, merge of `6f4a660`) | The handler's done save also records the team queue acceptance, so a finished entry no longer waits for a separate step | Hub `20260928-queue-acceptance-66b55ed`, Mini `tt` |
| `0973558` | Flaky browser suites (`wi_a9a69169f732121d`, merge of `a577d78`) | Queue refresh races, Board focus loss and a hidden Board reload fixed; board-compose, board-decisions and project-queue pass reliably | TailOS `9749cde5` |
| `ffda442` | Handler rotation (`wi_611e4d3c6992a664`, merge of `839127c`) | A long-lived database handler can be replaced by a fresh session that takes over its obligations; default limits 10 items, 300M tokens or a prompt change (off for existing projects until enabled) | Hub `20260928-handler-rotation-ffda442`, Mini `tt` |

## Hub releases

| Release | Backup (before) | Size | SHA-256 | Integrity | Schema |
|---|---|---|---|---|---|
| `20260925-handler-save-cf6b1cf` | `before-handler-save-cf6b1cf.sqlite` | 120,373,248 | `2471c1e5…` | ok, 0 FK | new `work_order_scope_confirmations`, `work_order_bookkeeping` |
| `20260925-handler-priority-33d3ab7` | `before-handler-priority-33d3ab7.sqlite` | 120,373,248 | `8c83ce04…` | ok, 0 FK | none |
| `20260926-parallel-activity-6235924` | `before-parallel-activity-6235924.sqlite` | 120,373,248 | `1102ab37…` | ok, 0 FK | reservations rebuilt (transactional); single-active and single-handler unique indexes replaced or dropped; new queue settings, host policy and usage, item leads, agent activity tables |
| `20260926-review-convergence-7ab5100` | `before-review-convergence-7ab5100.sqlite` | 124,850,176 | `832c18b6…` | ok, 0 FK | new `review_convergence` |
| `20260926-owner-accept-4b5adb2` | `before-owner-accept-4b5adb2.sqlite` | 127,098,880 | `7d834802…` | ok, 0 FK | none |
| `20260926-verification-34944cf` | `before-verification-34944cf.sqlite` | 132,546,560 | `acac8c51…` | ok, 0 FK | new `verification_records`, `verification_enrollments` |
| `20260926-owner-obligations-8b1f4d4` | `before-owner-obligations-8b1f4d4.sqlite` | 134,545,408 | `af51a1cf…` | ok, 0 FK | new `owner_obligation_delegations`; `obligations.recipient_kind` column |
| `20260927-verification-body-c343819` | `before-verification-body-c343819-v2.sqlite` | 136,441,856 | `c9160ac1…` | ok, 0 FK | none |
| `20260927-deployment-agent-8e32dc0` | `before-deployment-agent-8e32dc0.sqlite` | 137,097,216 | `e876dfb9…` | ok, 0 FK | new `release_jobs`, `release_action_receipts`; unique index for one deployment agent per project |
| `20260927-queue-accept-5480dae` | `before-queue-accept-5480dae.sqlite` | 137,195,520 | `bcccb27e…` | ok, 0 FK | none |
| `20260927-token-accounting-acfe2d0` | `before-token-accounting-acfe2d0.sqlite` | 142,581,760 | `318c21f2…` | ok, 0 FK | seven new usage tables (turns, runs, receipts, prices, host usage) |
| `20260928-claude-wake-3c2de08` | `before-claude-wake-3c2de08.sqlite` | 183,001,088 | `afb40749…` | ok, 0 FK | none |
| `20260928-owner-accept-followups-f3b1c19` | `before-owner-accept-followups-f3b1c19.sqlite` | 204,869,632 | `80df2e50…` | ok, 0 FK | none |
| `20260928-repository-identity-0ce60c0` | `before-repository-identity-0ce60c0.sqlite` | 205,438,976 | `1e582fd3…` | ok, 0 FK | none |
| `20260928-verification-criterion-172becd` | `before-verification-criterion-172becd.sqlite` | 229,027,840 | `205ddf33…` | ok, 0 FK | none |
| `20260928-queue-acceptance-66b55ed` | `before-queue-acceptance-66b55ed.sqlite` | 271,040,512 | `ae2af9fe…` | ok, 0 FK | none |
| `20260928-handler-rotation-ffda442` | `before-handler-rotation-ffda442.sqlite` | 284,246,016 | `22ab27dd…` | ok, 0 FK | `tasks.primary_handler_id`, `tasks.handler_revision`; new `handler_runs`, `handler_rotations`, `handler_rotation_requests`, `handler_rotation_policy` |

Before `6235924`, the combined migration was rehearsed on a copy of the `33d3ab7` backup: the new
hub migrated it (integrity ok, no FK violations, 14 projects, queue at limit 1), and the
`33d3ab7` hub opened the migrated copy. The `34944cf`, `8b1f4d4`, `8e32dc0`, `acfe2d0` and `ffda442` migrations were rehearsed
the same way on a copy of the previous release's backup, including the old hub opening the
migrated copy. `172becd` fast-forwarded `tasks-hub` and shipped on its independent receipt without an owner-side rerun, per the deployment agent's rule. `6692a57`, `1e8e8df`, `66b55ed`, `0973558` and `ffda442` were merges, so each shipped only after a full check run on the merge commit. `1e8e8df` was integrated by the owner release step: the team's branch predated the queue's frozen base, and re-accepting the done item was refused, so its queue entry was failed and released. The first `c343819` deploy attempt was refused by plan validation before any
change (the plan carried the previous day's release name); the second, with a fresh backup
(`-v2`), succeeded. Rollback binaries: `.build/prev-*` (hub, bridge) and
`~/.local/bin/tt.prev-*` (Mini).

## Verification before each merge
Each merge ran, on the exact commit: `go vet ./...`, `go test ./...`, `npm test`, and the browser
suites the change touched (board-layout, hub-cache, task-form, project-compact-ui, and from
`6235924` also parallel-team and activity), in Chromium and WebKit; plus `-race` on the queue,
runner, migration and activity tests where they changed. The owner-side pre-merge run of the
activity monitor found a 390px roster clipping regression that the team's runs had missed; it was
fixed (`wi_f415c845559a65b9`) before release. From `7a49ec8`, every release that changed client
code ran all seven browser suites (npm 219 → 244 tests). The owner-side check of owner obligations found three browser suites
broken by an unhandled `/obligations` fetch; the fix (`wi_5a411a2a78e3ad9e`) shipped with it.
Merging it also exposed an undeclared browser suite in the verification matrix, added in
`8b1f4d4`. `c343819` is hub-only: `go vet`, `go test ./...`, `-race` on server and store, npm, and
a test that saves a full 69-check receipt over 64 KiB and fails on the old limit with the live
error.

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

- Claude reviewers repeatedly hit the Fable usage limit at launch and sat idle; each was switched
  to Opus 5.5 with `/model`. Detection is queued (`wi_72f41bd375032cf0`).
- A review ending in an `owner-decision` disposition had no way to be resolved; `4b5adb2` added
  the owner-only `owner-accept`.
- The deployment agent was the first team verified natively. Its verifier found that
  `profile-sync-browser` fails in any fresh checkout (it needs a binary a later suite builds;
  `wi_21ba42f8f0542dc0`), and that a full receipt (75,820 bytes) exceeded the hub's 64 KiB request
  limit, so no verified team could be accepted until `c343819`.

## Known open
- `profile-sync-browser` order dependency (`wi_21ba42f8f0542dc0`); verifiers build the test hub
  binary as a prerequisite until it is fixed.
- 14 browser suites fail on `tasks-hub` and are listed as known failures (`wi_be7bbed81d4009c1`,
  queued).
- The verification matrix changed in `8b1f4d4` (digest `58430970…`); candidates based on it need a
  new owner matrix approval.
