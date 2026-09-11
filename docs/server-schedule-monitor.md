# Server schedule monitor

Feature `wi_b6a72c2246a7a07b` revision 3, original bounded work order Board
message `#2280` (owner source `#2274`) and current review `#2352`, is
implemented as a deterministic hub-local schedule enforcer. The correction
scope is reviewer FAIL `#2419`, delivered as source `#2507` and handler
authority `#2521`. Its separate Queue Start is `que_2d7b54c8dc191ed2` cycle 1
revision 3, receipt `qrr_475f5dc7e8ba3b62`, event `120`.

The monitor starts with the hub process. Defaults are a 30-second observation
interval, a five-minute Queue-stall deadline, a two-minute active-worker
silence deadline, a five-minute initial notification backoff, and a one-hour
maximum backoff. These can be changed only at process configuration time with
positive Go durations in `TAILTERM_SCHEDULE_MONITOR_INTERVAL`,
`TAILTERM_SCHEDULE_MONITOR_STALL_AFTER`,
`TAILTERM_SCHEDULE_MONITOR_WORKER_SILENCE`,
`TAILTERM_SCHEDULE_MONITOR_INITIAL_BACKOFF`, and
`TAILTERM_SCHEDULE_MONITOR_MAX_BACKOFF`. Notice records are retained for seven
days by default; `TAILTERM_SCHEDULE_MONITOR_NOTICE_RETENTION` can set a
different positive duration.

For each open task with exactly one ordinary agent matching its configured
orchestrator name, it reads durable Queue and agent snapshots. A candidate is
one of: an eligible waiting entry past its deadline, a claimed entry past its
deadline, an active entry whose exact worker is missing/unavailable, or an
active worker that has been silent past both deadlines. An active Queue entry
past its deadline is also stalled even if its worker continues healthy
heartbeats: heartbeat is liveness, while the Queue's durable update time is the
progress signal. The directed message to the lead starts with `GO!` and names a
stable Queue fingerprint.

No Queue entry, worker, task, agent status, allocation, work-item revision, or
approval is changed. Empty Queue snapshots emit no notification. Retired or
`needs_input` leads, and retired or `needs_input` active workers, are preserved
as intentional blocking states. Eligibility is per entry: an unrelated blocked
or unknown entry cannot hide another independently actionable one.
Missing/ambiguous orchestrator identity, incomplete timestamps, or failed
snapshot reads are reported as `unknown` and fail closed for that entry. A
closed-task delivery failure is retained as delivery failure and also makes no
lifecycle change.

`schedule_monitor_notices` persists first/last observation, last successful
delivery attempt, successful delivery, message sequence, failure/error state,
and delivery count per task/fingerprint.
It survives a hub restart. Successful messages use a distinct deterministic
request identity per backoff generation, so retries are attributable and do not
duplicate a generation. Both failed attempts and successful notifications use
the same doubling backoff from the configured initial duration to the cap, so a
closed/unavailable delivery target cannot cause polling-rate retries. Records
with no observation in the bounded retention period are pruned; pruning removes
only scheduler-local metadata. The existing directed-lead message path is the
only delivery boundary; this feature does not inspect or alter relay behavior.

Focused isolated checks are `go test ./internal/monitor ./internal/store
./cmd/tailterm-hub` and `go vet` for the same packages. The monitor tests use a
temporary SQLite database and controlled clock to cover waiting/claimed/active
stalls including healthy-heartbeat/no-progress work, silent/missing workers,
per-entry intentional/unknown eligibility, directed delivery, backoff
suppression, restart recovery, failed-delivery backoff, and bounded retention.
No live
task/profile data, production deployment, relay, network, Tailscale, or Mini
preview process is used.

Limitations: a stored message proves hub delivery, not that a runtime consumed
it; an offline but non-retired lead can receive durable unread mail later. The
monitor deliberately cannot repair a worker, decide whether an item should be
started, or infer an owner decision from silence.
