# Reliable directive core

Feature `wi_618c8ff87e6b8061` revision 5, first-core work order #3699,
owns this bounded manual execution protocol. It builds on the accepted API design
from #3578. It does not implement transport wake delivery, keep-going signals,
the periodic watchdog, the handler backlog consumer (#3694), provider hooks, or
UI presentation.

## What the core records

A required delivery attaches one immutable, already-stored Board message to the
exact current item-worker binding. The server derives the retained context
digest from that binding; callers do not supply it as evidence. The record keeps
the exact task, item revision, work-order message, agent, run, context digest,
directive kind, generation, execution epoch, phase, timestamps, and optional
superseded delivery.

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
tt delivery ack --request-id KEY --expected-epoch N DELIVERY
tt delivery progress --request-id KEY --expected-epoch N --text TEXT DELIVERY
tt delivery block --request-id KEY --expected-epoch N --reason CLASS --text TEXT DELIVERY
tt delivery resolve --request-id KEY --block-id BLOCK --expected-epoch N --text TEXT DELIVERY
tt delivery resume --request-id KEY --expected-epoch N --resolution-id RESOLUTION DELIVERY
tt delivery result --request-id KEY --expected-epoch N --text TEXT DELIVERY
```

The hub advertises `reliableDelivery` capability version 1. Before any new
current-assignment or delivery request, the CLI reads capabilities. An old,
unknown, unsupported, or incompatible hub fails closed with upgrade guidance;
there is no fallback to ordinary messages or lifecycle events. Legacy commands
remain unchanged.

## Explicit limits and follow-on work

This core provides durable manual selection and protocol fencing only. It has no
wake-attempt ledger, relay retry, delivery deadline, keep-going signal, tool
wrapper fence, provider acknowledgment, automatic escalation, or global handler
assessment. Zero wake attempts is therefore both valid and expected for direct
manual pickup. The capability advertises none of those deferred behaviors.

Protocol rejection cannot undo shell, filesystem, provider, or external-service
effects already started from obsolete instructions, and the core does not claim
exactly-once external execution. Callers must re-read current assignment before
starting an unwrapped side effect; later transport/tool integration must add its
own pre-tool CAS and reconciliation evidence.

The handler backlog-assessment consumer in order #3694 must reuse this ledger in
a later bounded slice. UI, transport, relay, watchdog, deployment, root
integration, and production rollout likewise require separate orders and
acceptance. Tests use only temporary SQLite databases and synthetic identities.
