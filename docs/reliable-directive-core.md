# Reliable directive core

Feature `wi_618c8ff87e6b8061` revision 5, first-core work order #3699,
owns the durable manual execution protocol. Order #4053 adds the bounded
execution follow-through consumer on the same ledger. It does not implement
general transport correction, keep-going signals, the periodic global watchdog,
the handler backlog consumer (#3694), provider hooks, or UI presentation.

The exact source `6d2ab09ce50480bea47462a3e392f694cbaed5af` is deployed under
release order #3951 to the TrueNAS hub and Mini CLI. Lead #3982 and handler #3984
accepted this qualified manual-core phase. The immutable operator receipt and a
separate acceptance record are retained under
[`docs/releases/reliable-directive-core-6d2ab09`](releases/reliable-directive-core-6d2ab09/).
That acceptance does not complete the parent Feature or any deferred behavior.

## What the core records

A required delivery attaches one immutable, already-stored Board message to the
exact current item-worker binding. The server derives the retained context
digest from that binding; callers do not supply it as evidence. The record keeps
the exact task, item revision, work-order message, agent, run, context digest,
directive kind, generation, execution epoch, phase, timestamps, and optional
superseded delivery.

Follow-through requires an explicit version-2 enrollment. The producer supplies
the exact governing-order message plus the UTF-8 byte count and SHA-256 of the
full instruction message. The server loads both immutable messages, verifies
their exact item/revision associations, requires the instruction message's
recorded work-order reference to equal the governing order, recomputes the
bytes/hash, and retains all of that beside the original admission work order.
A Board post, checkpoint, summary, Queue state, or apparent roster assignment is
never parsed into an obligation. `GET .../delivery-coverage` and
`tt delivery coverage` report an exact bound run with no v2 enrollment as
`uncovered_unverified`; migrated v1 rows are
`legacy_enrollment_unverified`. The handler/coordinator owns explicit enrollment
during activation.

The supported directive kinds are `assignment`, `amendment`, and `review`.
Creating a later directive requires both the exact expected current delivery ID
and generation. Creation and supersession commit in one transaction, and only
one current directive can exist for an exact item/agent/run. Ordinary messages,
inbox reads, Queue Start, roster status, heartbeats, and generic lifecycle events
do not acknowledge or progress a directive.

Execution events and operation receipts are separate append-only records. A
stable request ID plus an unchanged payload returns the original receipt. Reuse
of the key with changed data conflicts. Receipt lookup happens before current
lifecycle and generation checks, so a response lost after commit can be recovered
without making an old directive or run current again.

## State and fencing

The manual execution state is:

```text
unacknowledged -> acknowledged -> progressing -> result
                         |              |
                         +----> blocked ----> resolved ----> acknowledged
                                                        (next execution epoch)

current directive A ---- CAS supersession ----> current directive B
        `---- no new ack/progress/block/resume/result is accepted
```

Ack, progress, block, resume, and result are exact-worker/run operations.
Ack, progress, and result carry the expected execution epoch. A block records a
bounded reason class at the current epoch. A resolution is a separate decision;
it does not resume execution or change roster retirement. Resume requires the
exact current resolution and epoch, then increments the execution epoch. A late,
previously uncommitted action from an older epoch conflicts without an event,
receipt, or state change. An identical action that committed earlier can still
recover its historical receipt.

The current project lead, database handler, or human caller may produce a
directive and resolve a block. A worker may resolve only its own `transient` or
`tool` block. These are integrity checks over the existing shared-workspace
credential; they are not cryptographic proof of a distinct human or agent.

Every new mutation transaction joins the retained delivery back to the current
saved item/run/order/context binding. Current-assignment lookup reads the roster,
binding, delivery, immutable message, current block, and event evidence in one
database snapshot. A naturally later supersession can still make that completed
snapshot stale, so the subsequent mutation remains the authoritative CAS fence.

## HTTP API

- `POST /v1/tasks/{task}/required-deliveries` attaches a stored message and
  creates or supersedes the current directive with a keyed receipt.
- `GET /v1/tasks/{task}/agents/{agent}/current-assignment?runId={run}` returns
  the coherent current directive, stored message, current block, and events.
- `GET /v1/tasks/{task}/agents/{agent}/delivery-coverage?runId={run}` reports
  covered, legacy-unverified, or uncovered exact-run enrollment.
- `POST /v1/tasks/{task}/deliveries/{delivery}/follow-through/check` performs
  deadline classification and transactionally leases at most one side effect.
- `POST /v1/tasks/{task}/deliveries/{delivery}/follow-through/report` stores the
  exact lease's accepted, failed, or ambiguous native-queue outcome.
- `POST /v1/tasks/{task}/deliveries/{delivery}/ack`
- `POST /v1/tasks/{task}/deliveries/{delivery}/progress`
- `POST /v1/tasks/{task}/deliveries/{delivery}/blocks`
- `POST /v1/tasks/{task}/deliveries/{delivery}/blocks/{block}/resolutions`
- `POST /v1/tasks/{task}/deliveries/{delivery}/resume`
- `POST /v1/tasks/{task}/deliveries/{delivery}/result`

Producer creation requires the exact linked item revision and the worker's
admitted work order. Worker operations require the delivery's exact agent/run.
Result records execution evidence only: it does not update the work item, accept
the result, advance Queue, release a worker, or close anything.

## CLI

`tt current-assignment` reads the task, agent, and run from the current
environment and prints the selected stored directive as task data. JSON is
available with `--json`.

`tt delivery` supports:

```text
tt delivery create --file REQUEST.json
tt delivery coverage
tt delivery ack --request-id KEY --expected-epoch N DELIVERY
tt delivery progress --request-id KEY --expected-epoch N --text TEXT DELIVERY
tt delivery block --request-id KEY --expected-epoch N --reason CLASS --text TEXT DELIVERY
tt delivery resolve --request-id KEY --block-id BLOCK --expected-epoch N --text TEXT DELIVERY
tt delivery resume --request-id KEY --expected-epoch N --resolution-id RESOLUTION DELIVERY
tt delivery result --request-id KEY --expected-epoch N --text TEXT DELIVERY
```

The hub advertises manual core version 1 and execution follow-through version 2.
Before any new
current-assignment or delivery request, the CLI reads capabilities. An old,
unknown, unsupported, or incompatible hub fails closed with upgrade guidance;
there is no fallback to ordinary messages or lifecycle events. A version-2
create request is never sent to a version-1 hub, which prevents an old JSON
decoder from silently discarding its enrollment evidence. Legacy commands remain
unchanged.

## Deadline and follow-through contract

Each v2 enrollment persists these UTC wall-clock defaults: 2 minutes to initial
ack, 10 minutes from ack or substantive progress to the next checkpoint, 2
minutes after block resolution to explicit resume, 2 minutes after native queue
acceptance to exact ack/progress, 30 seconds for the lease holder to report its
transport outcome, a 30-minute hard active-tool bound, and at most two queue
attempts. These are implementation defaults accepted for order #4053, not an
owner-facing service-level promise. Tests inject the store clock; persisted RFC
3339 timestamps make restart behavior deterministic. Wall-clock adjustments can
make a deadline fire earlier or later, so no monotonic-clock claim crosses a
process restart.

Only exact directive ack, progress, block, resolution/resume, or result is
substantive. Heartbeats, `Working`, online status, Queue Start, inbox reads, and a
native `codex queue` receipt do not reset a deadline or prove consumption. The
classification keeps recent progress, unresolved block, resolved-awaiting-resume,
retired/terminal run, overdue idle prompt, fresh active tool, and unknown runtime
evidence distinct. Runtime observations are accepted for at most two minutes;
a stale `active_tool` cannot suppress action indefinitely. The installed Codex
CLI exposes queue acceptance but no supported active-tool/idle-prompt query, so
the production relay reports `unknown` rather than fabricating either state.

When an enrolled directive is overdue, the store transactionally leases one
exact-thread queue attempt with a stable request receipt. Replay returns the
receipt with `execute=false`. The relay re-reads delivery coverage immediately
before calling Codex; a changed run/generation/epoch/current-item revision reports
an `invalidated` outcome without invoking the runtime. The last read also requires
an actionable lifecycle, unchanged directive phase, and the exact still-pending
lease, so intervening progress, block, result, retirement, closure or exit wins
the race and is preserved. The database lease and external subprocess
cannot be atomic, so a narrow recheck-to-call window remains. Queue acceptance is
stored separately and stays unconfirmed until exact directive evidence arrives.

If the relay crashes before the call, or after the call but before its durable
report, the 30-second lease expires to one ambiguous escalation; the external
queue action is never repeated from that lease. A report response lost after
commit is retried from the relay's private state with the same key and payload,
without repeating the external call. A definitive stale-report conflict is
converted once to a separately keyed durable invalidation, then removed from
private retry state so it cannot starve later follow-through or ordinary inbox
delivery. Accepted prompts with no later consumption
may receive the second bounded attempt, then escalate. Escalation is one durable
Board message to the current actionable orchestrator and one ledger event.
Retired, replaced, exited, closed, superseded, result-complete, or explicitly
blocked work is never automatically resumed or manipulated.

## Explicit limits and follow-on work

The bounded consumer adds deadline state, exact-thread queue leases/outcomes and
explicit escalation to the existing directive ledger. It does not claim that a
native queue acceptance started a turn, that the runtime is idle/active when no
supported observer says so, or that an unenrolled Board assignment is protected.
Zero wake attempts remains valid for direct manual pickup. There is no tool
wrapper fence, provider execution acknowledgment, TUI Escape automation, process
restart/replacement/closure, or global handler assessment in this slice.

Protocol rejection cannot undo shell, filesystem, provider, or external-service
effects already started from obsolete instructions, and the core does not claim
exactly-once external execution. Callers must re-read current assignment before
starting an unwrapped side effect; later transport/tool integration must add its
own pre-tool CAS and reconciliation evidence.

The handler backlog-assessment consumer in order #3694 must reuse this ledger in
a later bounded slice. UI, transport, relay, watchdog, typed operational authority,
Air CLI and frontend rollout likewise require separate orders and acceptance.
Tests use only temporary SQLite databases and synthetic identities. The deployed
hub/Mini release did not change the frontend, Air, relay, network, Tailscale,
preview PID60799/PPID1, or existing profile contents.
