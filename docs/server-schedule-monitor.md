# Server schedule monitor

Feature `wi_b6a72c2246a7a07b@5`, correction order **#2982**, is implemented
in the isolated `fix/server-monitor-r5` branch from preserved candidate
`72ed7b5ff22c17db9f472f53ceb5a5e30dd5ab65`. Builder
`agt_600e73b3cc7a4ad8/run_ea39dd665de494a7` consumed handler #3300:
Queue `que_2d7b54c8dc191ed2` cycle 1/revision 20, deliberate claim
`qrr_cfc84a743f3ce58a`, separate Start `qrr_11c1b3d0f4e181a7`.
Admission context through #3285 has digest
`42495465cb8a0c5fecb1a15250990674a3e4033f499e5cb3f80ca0feea0bb36e`
(full supplied history through #3280). Design checkpoint #3303 was accepted
by lead #3307/#3308 and saved/read back by handler #3309. The four correction
findings are in independent review #2974/#2975, retained report
`/tmp/server-monitor-current-review-2948.txt`. The correction was subsequently integrated and released as qualified Stage A
as recorded below. Overall feature completion is not claimed.

Original implementation order #2280/source #2274 used Start
`qrr_475f5dc7e8ba3b62`, event 120. That receipt belongs only to the original
revision-2 implementation. The later `72ed7b5` correction was reported after
its current-revision Start was rejected, followed by safety release event160.
Handler #2530/#2550 records that execution-provenance gap. Neither the new
builder Start nor the valid independent review retroactively repairs it.

## Stage-A hub release — September 12, 2026

The notification-only monitor was deployed at **2026-09-13 02:05:53 UTC**
(September 12, 19:05 Pacific), under release order **#3411** and lead Stage1
acceptance **#3445**. The same builder/run/context consumed handler **#3416**:
Queue cycle1/revision32, claim `qrr_4ba29a2805fce2ef`, separate Start
`qrr_ec092dbe43f55310`. Gate consumption and execution were delivered in
**#3448/#3452**, with runtime verification **#3456**.

Application source is `f450996d0b447c959a630c779f60d8ef30ce52df`, combining
`2ea73fb`, `72ed7b5`, and corrected `3d9c638` on the prior Capacity/context root
`4345d62`. Independent combined-source/package review **#3409**, saved by
handler **#3410**, gave a qualified Stage-A PASS. Focused monitor/store/hub
normal and race tests, Capacity/context HTTP regressions, vet and source/package
checks passed. This release required no CLI or frontend update.

The exact clean-source Linux amd64 binary is 27,357,346 bytes, SHA-256
`4711d3c0acc4d9ce17725e1c4fea74709ee945edb694d0f4f74bea74e0e107a3`.
Its embedded VCS revision is the application hash above, `vcs.modified=false`.
The nine-payload manifest hash is
`cb043a4f991a1eb246cf41a2ae71af74adaec7068456c428c5c9812ae0f56a7e`.
The runtime mounts the immutable binary at
`/mnt/deepfreeze/tailterm-hub/releases/20260913-monitor-stage-a-f450996/tailterm-hub`.
TrueNAS middleware reports one running container `29327f2b`; host process
`3655758` has the matching `/proc` executable hash. Authenticated capabilities
returned HTTP200 and matched the pre-release allocationIntent v1, Queue and
message-audit capabilities. A bounded middleware log read confirmed TCP startup
with token authentication. The sampled log did not expose a monitor outcome;
no outcome-log or end-to-end role-wake proof is inferred from it.

Only the binary mount changed. Image, UID/GID950, read-only root, dropped
capabilities, security options, CPU/memory limits, cap32, state/token mounts and
private TCP listener remained. Root `tasks-hub` fast-forwarded to application
source; the pre-existing owner screenshot hash and preview PID60799 remained.
No CLI/frontend/Cloudflare/relay/Tailscale/Files or worker lifecycle changes were
performed. No live work-item/profile test fixture was created.

Handler-owned consistent online backup before release:
`/mnt/deepfreeze/tailterm-hub/backups/before-stage1-release-3411-20260912T235805Z.sqlite`,
27,529,216 bytes, mode0600 UID/GID950, SHA-256
`ccdad59c89937cae206bf7aaf7966319f3f700827466b92c964ff23321646c32`.
Handler **#3441** corrected the baseline artifact, preserving the prior erroneous
artifact. Backup/live integrity and foreign keys passed with exact current
profile-table hash equality. Historical hashes from #3220 are not comparable
because its complete canonicalization record was unavailable. Current baseline
artifact `/tmp/tailterm-stage1-profile-baseline-3433.json` is 1,435 bytes, SHA-256
`070cca4391d80c01b6d2933fa837c097d462048735676bc647cef2a716a9593b`.

Handler Stage3 **#3457** (native `nart_7d7375617a01f295` v1) confirms
live integrity `ok`, zero foreign-key violations, both monitor tables, and exact
profile-table counts/hashes matching the corrected Stage1 baseline. Its native
989-byte content hashes to
`fde25784f52eb4f266d92562da09c7359ce21e5069a53f050e4ab5c883bbd136`;
the local export adds one final newline (990 bytes, different file hash).
Builder consumed and verified that content. Lead #3454/#3459 reported actual
notice #3451, as notification evidence only.

Runtime release is not whole-item completion. Full staged receipts,
preflight/runtime evidence and controlled update/rollback scripts are retained in
`/tmp/tailterm-monitor-release-3411`. The complete `release-receipt.json` is
14,544 bytes, SHA-256
`fdf447be66c5cac2f5bc158c6a1f8d7a4b46ae08a308809c49c17d6a4fc95b79`.
This documentation amendment is report-only;
it does not require rebuilding the application binary.

Compatible rollback, verified against the actual pre-release process, is the
NO-MONITOR Capacity/context binary
`/mnt/deepfreeze/tailterm-hub/releases/20260912-context-capacity-d64615b/tailterm-hub`,
27,295,906 bytes, SHA-256
`801f93172221821f1f0788698724d55b92617e4e0eade9667d457643841c58b7`.
Rollback changes only that middleware binary mount, retaining newer database
writes, additive monitor tables, generation allocator, messages and audit.
No rollback or database restore was performed.

Stage A is default-on across eligible open projects. It remains a Queue-age
notification heuristic. Required-unread assignments, actual progress/checkpoints,
exact handler wake, verified readiness/acknowledgement, escalation and
browser-closed/relay-restart/dead-lead guarantees remain outside this release
and incomplete. The provenance gap #2530/#2550 above remains unchanged.

## Configuration and observation

The hub runs one serial monitor loop: an immediate observation, then one per
interval. Positive Go duration environment variables configure it at startup:

| Variable suffix (prefix `TAILTERM_SCHEDULE_MONITOR_`) | Default |
| --- | --- |
| `INTERVAL` | `30s` |
| `STALL_AFTER` | `5m` |
| `WORKER_SILENCE` | `2m` |
| `INITIAL_BACKOFF` | `5m` |
| `MAX_BACKOFF` | `1h` |
| `NOTICE_RETENTION` | `168h` |

Maximum backoff must be at least initial backoff. Invalid configuration rejects
startup. Signal cancellation stops further ticks, and normal hub shutdown waits
for monitor completion before closing its store. It changes no networking,
relay or host process configuration.

For each open task with exactly one ordinary agent matching its configured
orchestrator, the monitor observes Queue/agent snapshots. It chooses the first
eligible overdue entry in stable Queue-ID order. Fingerprints include condition,
Queue ID, cycle, revision and worker run. Conditions are eligible waiting work,
claimed work without reconciliation pending, and active work with a missing or
stale exact worker run, an exited/closed worker, a silent worker, or an old Queue
update despite healthy heartbeats. An unrelated intentionally blocked or unknown
entry cannot hide an independently eligible entry.

Queue `UpdatedAt` is an age heuristic, not direct proof of work progress: legitimate
long work with no Queue update also becomes `active_stale`. Heartbeats demonstrate
process liveness only. Zero/future Queue timestamps and zero worker timestamps
fail closed for that entry. Empty Queue snapshots send nothing. Retired or
`needs_input` active workers are intentional blocks; retired, `needs_input`,
closed or exited leads are never delivery targets. Missing/ambiguous lead
identity or observation errors yield `unknown`.

## Atomic notification and retry contract

A dedicated store transaction rechecks the configured lead ID, exact run and
status immediately before delivery, serialized with lifecycle writes. A lead
retired or replaced after observation receives no new monitor mail. The
internal message insertion explicitly disables human resumption, independently
of that check. Ordinary explicit human-directed resumption remains unchanged.
Offline current leads that are otherwise eligible may receive durable unread
mail; online presence is not a new eligibility requirement.

The system-attributed, directed `GO!` message includes its fingerprint and states
that it is notification only. Its message, message event, posting receipt and
successful notice state commit together. A failed message/event/receipt write
rolls back that entire attempted delivery before saving failure metadata. Failure
to save notice state rolls back the outer transaction, including message and
generation allocation. No Queue, task, work-item, approval or agent lifecycle
record is changed. A stored message proves hub storage, not runtime consumption.

`schedule_monitor_notices` stores observation/attempt/success times, counts,
message sequence, last error, recipient ID/run and any pending request identity.
A singleton `schedule_monitor_generation` allocates monotonically increasing
`schedule-monitor:v2:N` request identities. It is never
pruned. Allocation and pending identity are transactional; the counter does not
wrap. Failed retries retain their pending identity and recipient/run across
restart. A successful commit clears the pending identity and preserves the saved
message sequence/receipt. A lost success response followed by a retry within
backoff returns suppression with that same saved message, without duplicate mail.
A later due notification gets a fresh generation.

Successive successes or failures double their respective backoff to the cap;
a failure sequence resets on success. Every observation refreshes retention even
when backoff suppresses delivery. Changing the current recipient ID or run starts
a new recipient-specific retry window and generation; it never reuses an old
recipient's request payload. Legacy notice rows preserve their success backoff
on upgrade; new attempts use v2 identities, never the legacy fingerprint counter.

Pruning deletes only notice rows not observed during the retention window. This
explicitly expires any failed retry window in those rows. Re-observation starts
with a fresh global generation, even after restart or recipient change. Because
mail and success state commit together, expired metadata cannot hide an orphan
successful delivery or replay a legacy receipt. Existing messages, receipt/audit
history and the generation allocator remain intact. Retention bounds inactive
metadata by age, not by an absolute row count; retained messages/audit history
continue to grow under their existing policies. Older monitor binaries must not
be run concurrently or used as a compatible rollback: they retain the retirement
race and legacy generation behavior, and cannot maintain these invariants.

Tick outcomes remain available to callers. Hub logs emit changed nonquiet outcomes
only: identical intentional-block and unknown states do not repeat each poll.
Recovery through no-work/suppressed clears the old condition quietly, allowing a
later recurrence to log. Suppression memory holds at most the current tasks plus
a global observation-error entry, drops disappeared/closed tasks on refresh, and
resets at process restart. A restart may therefore log the current condition once.

## Verification and limits

Run from `hub`:

```sh
go test -count=1 -timeout=120s ./internal/monitor ./internal/store ./cmd/tailterm-hub
go test -race -count=1 -timeout=180s ./internal/monitor ./internal/store ./cmd/tailterm-hub
go vet ./internal/monitor ./internal/store ./cmd/tailterm-hub
```

Committed synthetic tests cover deadline boundaries; claimed/missing/stale-run/
unavailable/retired/unknown workers; healthy-heartbeat stalls and mixed-entry
precedence; no work and unknown leads; unchanged log suppression/recovery;
configuration and overflow-safe backoff; controlled scheduler ticks and cancellation;
retirement between snapshot and delivery; recipient/run replacement and offline
eligibility; explicit human resumption; post-commit lost-response/reopen and
concurrent retries; message-event and success-state failure rollback; failed
restart with frozen retry identity; pruning/re-observation/recipient changes;
and migration from the original notice schema with retained legacy receipts.
Test clocks control scheduling/backoff. The online-human-resume guard uses a
fresh synthetic heartbeat because the existing agent scanner derives online
status from host time. Safety timeouts in cancellation tests detect a hang;
assertions do not depend on sleeping for a scheduling interval.

No live task/profile fixtures, provider or tmux sessions, relay-consumption proof,
network/Tailscale changes, deployment, root integration, long wall-clock soak,
or operating-system crash/power-loss injection are claimed. Transaction failures
and discarded committed responses are controlled simulations. The broad known
hanging `cmd/tt` suite is outside this focused correction validation.
