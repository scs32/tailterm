# Handler rotation

Feature `wi_611e4d3c6992a664`, work order #11573; plan
[handler-rotation-plan.md](handler-rotation-plan.md); default thresholds from
owner decision #14233.

A project's database handler is the one long-lived role. Rotation replaces it
with a fresh session that starts with the current prompt. The fresh session takes
over every open obligation and a durable handoff, and the old session closes.
Nothing in flight is lost or duplicated.

The [backlog steward](backlog-steward.md#rotation) rotates separately, with its own
policy, records, commands (`tt steward rotate`) and relay tick, and hands over its
backlog summary. A steward's arrival does not change the handler template digest,
so it never makes a handler rotation due.

## Primary handler

`tasks.primary_handler_id` names the project's primary handler once a rotation
commits, and `tasks.handler_revision` counts rotations (it starts at 1). While the
primary is empty, the legacy rules apply:

- Briefings treat the oldest open handler as primary.
- `role:database_handler` resolves to the newest open handler.

Once the primary is set, briefings name it and `role:database_handler` resolves
to it, even when an auxiliary handler is newer. A successor of a prepared
rotation never holds the role and never counts as the legacy primary. Item-leased
role resolution is unchanged.

## Handler floor

While a project is open and active, the hub keeps at least one database handler
available (`wi_96295d2375d72618`). An available handler is one whose status is
not closed, exited or retired; a handler that is still starting counts. The
check runs in the store for every retire or close, whether it comes from
`tt retire` (`PATCH .../agents/{aid}`), `tt close` (`DELETE .../agents/{aid}`) or a
`closed` event. It refuses two changes with HTTP 409:

- Retiring or closing the recorded primary:
  `<name> is the project's primary database handler; rotate it with tt handler rotate before retiring or closing it`.
  This holds even when the primary is already retired.
- Retiring or closing the last available handler:
  `<name> is the project's last available database handler; start or resume another handler first`.

A ready successor lifts both refusals: a prepared rotation away from that
handler, or a committed rotation chain that ends at an open handler.

Closing a retired handler that is not the primary is always allowed, since it is
no longer available. The hub does not check its leases or obligations; rotation
and the queue own those.

`tt close` applies this rule in two steps (`wi_67fb6b7716a5c1b4`):

- A retired handler: `tt close` sends the close to the hub with the handler's
  exact run. A non-primary one closes and its session is cleaned up. If the hub
  refuses, for example a retired primary, `tt close` prints the hub's reason
  (`hub: 409 conflict: <name> is the project's primary database handler; ...`)
  and leaves the session alone.
- A handler in any other status: `tt close` refuses before contacting the hub,
  with `the active database handler remains available while the project is open`.
  Retire it first (`tt retire`, which the floor checks) or rotate it.

Its other checks still run first for a retired handler: the session must be on
this host, and the project orchestrator and an item lead are not closed this way.

An `exited` report is never refused. When it (or a retire or close with a ready
successor) takes the available count to zero, the hub posts one board-wide owner
NOTICE, `Owner attention: the project has no available database handler`, with
refs `escalation=owner` and `cause=no-database-handler`. The text names the
project and the handler that left. Changes to handlers that are already
unavailable add no notice, so each drop to zero posts exactly one.

Exempt, because the owner or a rotation deliberately ends the handler: project
pause, closing the project (`CloseTask`), a rotation commit or abort, and
`tt close --team`, which never closes handlers.

## Commands (owner only)

These commands refuse to run inside an agent session. The hub also refuses any
request that carries an agent identity (`agent_caller`).

```sh
# Save the handler's launch settings on its host (no identity, no token).
tt handler spec --task TASK -- --run codex --cwd /path --prompt "..." [--model M --reasoning R ...]
tt handler spec --task TASK            # show the saved spec

# Rotate now, below any threshold. Spawn flags after -- replace the saved spec.
tt handler rotate --task TASK [-- <launch flags>]
tt handler rotate --task TASK --abort  # abort a prepared rotation

# Move the primary to another runtime, model or arm (see below).
tt handler rotate --task TASK --authorized-by NAME --authorization-reason "..." \
  -- --run claude --runtime claude --model M --reasoning R --cwd /path --prompt "..."

tt handler rotation list --task TASK
tt handler rotation get ROTATION --task TASK   # handoff snapshot and receipt

tt handler policy get --task TASK
tt handler policy set --task TASK --revision N [--enabled=BOOL] [--max-items N] \
  [--max-total-tokens N] [--on-template-change=BOOL] [--dead-silence-minutes N]
```

A launch spec accepts only `--run`, `--cwd`, `--prompt`, `--runtime`, `--model`,
`--reasoning`, `--permission-mode`, `--approval-mode`, `--sandbox-mode` and
`--allowed-tools-json`. It is stored at mode 0600 under the relay state directory
and keyed by hub and project. Its directory must match the old handler's. Its
runtime must match too, unless the rotation is an
[owner-authorized change](#changing-the-primarys-runtime-or-model).

Run `tt handler rotate` on the old handler's host. That host must stop the old
session, and the successor must run on the same host and directory, and on the
same runtime unless the change is authorized.

## Changing the primary's runtime or model

An ordinary rotation keeps the primary on its runtime and, under a
[handler arm policy](handler-ab.md), inside its arm. Use an authorized change
when the owner decides to move the primary to another runtime, model or arm, for
example from Codex to Claude (`wi_f250a91c85367e6f`, order #20511):

```sh
tt handler rotate --task TASK \
  --authorized-by "owner" --authorization-reason "Owner decision #N: Sonnet-only handlers" \
  -- --run claude --runtime claude --model claude-sonnet-5-5 --reasoning high \
     --cwd /path --prompt "..."
```

- `--authorized-by` says who authorized the change (at most 200 bytes) and
  `--authorization-reason` says why (at most 1000 bytes). Give both or neither;
  one alone, or either with `--abort`, is a usage error before any hub request.
  The hub refuses an authorization from the runner or with a blank field (400).
- It is the same rotation: prepare, launch, wait, commit and cleanup, with the
  same idle checks, journal resume, obligation re-issue, handoff snapshot,
  directed NOTICE and close of the old handler. Only two checks are lifted: the
  runtime match and `arm_changed`.
- The host and directory must still match the old handler's. An authorization
  never covers another directory or host.
- The arm policy is not changed. If the new run matches an arm, it is in that arm
  from then on and later ordinary rotations keep it there.
- The flags after `--` are saved as the host's launch spec before the checks
  run, so later runner rotations launch the new runtime. A refused attempt
  leaves that spec saved too: a runner rotation that comes due is then refused
  for the runtime mismatch and spawns nothing, until you retry with the
  authorization or save the old spec again with `tt handler spec`.
- Without the two flags nothing changes: a spec with another runtime is refused
  before any spawn, and the hub refuses the commit (`successor_unavailable`,
  `arm_changed`).

The authorization is saved on the rotation at prepare, so a rerun or a runner
resume needs no flags; the journal carries them. `tt handler rotation get` prints
`Authorized by NAME: REASON (runtime OLD -> NEW)`, `tt handler rotation list`
appends `authorized-by="NAME"`, and the JSON record has `authorization`
(`authorizedBy`, `reason`, `oldRuntime`, `successorRuntime`; the runtimes are
saved at commit). The `Handler rotation prepared` and `Handler rotation
committed` events carry `authorizedBy` and `authorizationReason`, the committed
event also carries `oldRuntime` and `successorRuntime`, and the handoff NOTICE
names who authorized the change and why.

## How a rotation runs

A rotation is a keyed, two-phase hub record (`handler_rotations`). The host
routine keeps a journal of its phases next to the spec: `preparing`, `prepared`,
`spawned`, then `committed`.

1. **Prepare.** The hub checks that the old handler is idle (see refusals). It
   records the old run and a preallocated successor ID. The successor's name is
   the base name plus `-r<handler_revision+1>`, for example `db-handler-r2`. From
   then on, queue dispatch leases neither handler.
2. **Launch.** The host runs `tt spawn --role database_handler
   --handler-successor` with the saved spec. The successor gets the primary
   handler prompt, plus a line telling it to wait for the handoff NOTICE. Its
   template digest is recorded for its exact run.
3. **Wait.** The routine waits up to two minutes for the successor to come
   online. If it does not, the rotation stays prepared.
4. **Commit.** In one hub transaction, the hub:
   - re-checks that the old handler is idle
   - verifies the successor is online on the same host and directory, and on
     the same runtime and arm unless the rotation carries an authorization
   - re-issues every open obligation the old handler holds (by role or by name)
     to the successor, keeping `via_role` and item links; each old one closes as
     `superseded`
   - snapshots the handoff and makes the successor primary
   - posts one directed NOTICE to the successor, citing the rotation
   - closes the old agent and saves the receipt
5. **Cleanup.** The host stops the old owned tmux session and writes its cleanup
   receipt, then removes the journal.

Rerunning `tt handler rotate`, or the next runner tick, resumes from the
journal. The rerun never launches a second successor (the successor ID is fixed
and `ensureHandler` is idempotent), never re-issues twice (commit is keyed), and
never cleans up twice. A prepare whose response was lost is replayed with the same
key. `--abort` works only from prepared. It closes a registered successor and
cleans up its session. It leaves the old handler primary and open and moves no
obligations. A new prepare is allowed afterwards.

## Refusals

A refused prepare or commit returns 409 with a named `code` and changes nothing.
The rotation row, successor and obligations stay as they were.

| Code | Meaning |
| --- | --- |
| `live_lease` | A team queue entry leased to the old handler's exact run is `launching`, `running`, or `failed` and not released. |
| `working` | The old run's activity is `working`, `hung_tool` or `looping`. For the runner, also any state other than `idle` or `finished_silent`. |
| `pending_tool` | The old run has a pending tool call. |
| `rotation_open` | Another rotation of this project is still prepared. |
| `project_paused` | The project is paused. |
| `not_primary` | The named run is not the current primary handler. |
| `handler_changed` | The handler revision or the old run changed since the caller read it. |
| `name_taken` | The successor name is wrong or already used by an open agent. |
| `successor_unavailable` | At commit, the successor is not a registered, online handler on the old handler's host, runtime and directory. An [authorized change](#changing-the-primarys-runtime-or-model) may differ in runtime only. |
| `agent_caller` | The request carries an agent identity. |
| `arm_changed` | At commit, the project has a saved [handler arm policy](handler-ab.md), the old run belongs to one of its arms and the successor does not belong to the same arm. `tt handler rotate` refuses before spawning in that case when the saved spec's `--model` or `--reasoning` differs from the old run's. Without a policy, or for a run in no arm, a model change is accepted. An [authorized change](#changing-the-primarys-runtime-or-model) skips this check and does not write the policy. |

| `death_unconfirmed` | A [dead primary](#a-dead-primary) rotation only: the host's evidence is missing, stale, from another host or run, or does not state both the session and the process gone; or the hub had a heartbeat from the run within 90 seconds; or the handler is not busy; or the replacement is off for the project. |
| `not_silent` | A dead primary rotation only: the run's last recorded activity is more recent than the project's silence, the run has no recorded activity at all, or it recorded activity after the rotation was prepared. |

Commit repeats the idle checks. A handler that became busy after prepare gets a
409, and the rotation stays prepared until a later attempt finds it idle.

These idle checks have one exception, by owner order #28057: a
[dead primary](#a-dead-primary). No other rotation, and never
`tt handler rotate` by hand, moves a handler that holds a live lease, has a
pending tool or is recorded as working.

"Finish an already-started atomic record" has no hub record. Its observable
signal is the activity monitor's exact-run state and pending tool. Activity
snapshots are transitions, so a handler that just started a turn can look idle
for up to the monitor's working window. The owner command treats an unobserved
(`unknown`) run as idle. The runner requires an observed `idle` or
`finished_silent` state.

## A dead primary

Bug `wi_b863e669073858f4`, owner order #28057.

A primary that dies while the hub records it as busy keeps that state and its
leases, so the refusals above never clear and every team waits. The host runner
replaces such a handler automatically, and only when **both** hold:

1. **Its own host confirms its process and session gone.** Not merely
   unreachable: a state the host cannot read is not confirmation.
2. **The hub has recorded no activity from that exact run for the project's
   silence**, 10 minutes by default.

A busy primary whose process is alive, or whose state cannot be confirmed, is
never rotated automatically. `tt handler rotate` by hand is unchanged and is
still refused for a busy primary: the exception exists only for the runner,
with evidence.

### Turning it off

The silence is the policy field `deadSilenceMinutes`. It is 10 for every
project, existing and new, and does not depend on `enabled`, which governs only
the item, token and template limits.

```sh
tt handler policy get --task TASK                                  # prints dead-silence-minutes
tt handler policy set --task TASK --revision N --dead-silence-minutes 0    # off
tt handler policy set --task TASK --revision N --dead-silence-minutes 15   # 1 to 1440
```

`0` turns the automatic replacement off for the project: the runner no longer
probes, and the hub refuses a dead primary prepare or commit
(`death_unconfirmed`). A `policy set` without the flag keeps the saved value.
With it off, a dead busy primary waits for the owner, as before this change.

### The host's probe

The runner on the handler's host probes only when the hub's due list says the
primary is a dead candidate (not online, busy, replacement on) and the silence
is met. The steps run in order and the first that decides wins:

1. Read the wrapper's process receipt for the exact hub, agent and run.
   Missing, unreadable, naming another hub, project, agent or run, or with no
   PID or no start identity: **unknown**.
2. The receipt's tmux socket differs from the runner's: **unknown**.
3. The tmux session listing cannot be read: **unknown**. Otherwise a session
   tagged with this agent (any run), or named as the receipt or the roster
   names the handler's session: **alive**.
4. The runtime process by PID and start identity: a failed check is
   **unknown**, including a PID now used by another process; running is
   **alive**. The same for the pane process when the receipt has one; a pane
   PID with no start identity is **unknown**.
5. Otherwise **gone**.

So gone needs three facts read on the handler's own host: its recorded session
is absent from a readable listing, its runtime process is absent, and its pane
process is absent. The probe signals and stops nothing. Only gone produces
evidence; alive and unknown send the hub nothing. An unreachable host sends
nothing at all, because only that host's runner probes.

A host with no saved launch spec cannot start a successor. It does not probe,
and posts one board notice per handler run:
`An offline busy primary handler cannot be replaced because this host has no saved launch spec`.

### What the hub checks itself

The runner sends a prepare with reason `dead_primary`, trigger `runner` and the
evidence (`deathEvidence`). The hub accepts it only when all of these hold,
besides the guards every rotation has:

| Check | Refusal |
| --- | --- |
| The trigger is `runner`, there is no owner authorization, and evidence is present. Evidence with any other reason is refused too. | 400 |
| The replacement is on for the project. | `death_unconfirmed` |
| The evidence names the old handler's recorded host and the primary's exact agent and run. | `death_unconfirmed` |
| Both states are `gone`, with a PID and a start identity. | `death_unconfirmed` |
| The observation is at most 2 minutes old and at most 30 seconds ahead of the hub clock. | `death_unconfirmed` |
| The hub itself has no heartbeat from the run within 90 seconds. | `death_unconfirmed` |
| The run is busy: the runner's ordinary refusal applies. An idle offline primary takes the ordinary rotation. | `death_unconfirmed` |
| The silence rule below. | `not_silent` |

The evidence is a claim by a caller that may write rotations, the same trust
as the runner trigger. The heartbeat and silence checks use only the hub's own
records and clock, which that claim cannot change.

**Silence.** The run's last recorded activity is the latest of: its wrapper's
heartbeat (`agents.last_seen_at`), its observed activity
(`agent_activity.observed_at`), the newest message it sent, and the newest
work-item revision it saved. Events and broker reminders do not count, because
other actors write those about the handler. A run with none of the four is not
silent: no record is not evidence of silence. The hub's clock decides.

### The second look before commit

The successor takes up to two minutes to come online. Before the commit the
runner probes again and sends that second observation. The commit needs it to
be valid, fresh and later than the first, the hub still to see no heartbeat,
the replacement still on, and **no activity recorded from the run since
prepare**. If the second probe is not gone, or the hub refuses the commit with
`death_unconfirmed` or `not_silent`, the runner aborts the rotation through the
ordinary abort: the successor is closed and its session cleaned up, the old
handler stays primary, and nothing has moved.

### What is handed off

The same two-phase, keyed rotation, journal and abort as any other. In the one
commit transaction, in addition to what every commit does:

- **Obligations** move as always: every open obligation the dead handler held
  is re-issued to the successor.
- **Leases** move. Every team queue entry the dead run holds (`launching`,
  `running`, or `failed` and not released) takes the successor's agent, run
  and a new lease generation, in queue order. Leases are never released: a
  released lease would leave a running team with no handler. The handler
  identity in an entry's frozen launch plan is rewritten to match, every other
  byte kept, so the team runner's identity check still passes; a plan that
  cannot be read or that names a third handler fails the whole commit and
  nothing moves. The entry's [arm assignment](handler-ab.md#rotation-within-an-arm)
  follows the lease. `handoff.liveLeases` lists each entry with its old and new
  generation.
- **In-flight work is reported, not recovered**, because nothing can be read
  from a dead process. `handoff.inFlight` has the run's last activity state,
  its pending tool, its last recorded activity, the silence applied, and
  `recentWrites`: the work-item revisions that run saved since its oldest open
  obligation was created (or in the 30 minutes before its last activity when it
  held none), newest first, at most 50, with a count of any left out. The
  successor compares each moved request with these before repeating a save.

The old handler is then closed, so the hub's closed-agent checks apply to any
late write from an orphaned child of the dead run.

### The notice

A committed dead primary rotation posts one notice,
`A dead primary database handler was replaced automatically`, written in the
commit transaction so a replay never repeats it. The same text goes once to the successor, in place of
the ordinary handoff notice, and once to the project's owner helper; with no
open owner helper that second copy is posted board-wide. It names the dead
handler, the host's evidence and both observation times, the hub's last
recorded activity and the silence applied, what was handed off and what is
only reported, and the command that turns the replacement off. The receipt has
`noticeSeq`, `ownerNoticeSeq` and `leasesMoved`, and `tt handler rotation get`
prints the evidence, the moved leases and the in-flight section.

## Handoff contents

`tt handler rotation get` shows the snapshot saved at commit:

- **reissued**: each old obligation and message seq, paired with its new
  obligation and message on the successor, and the old state and role
- **pendingScopeConfirmations**: queued team entries whose exact
  item/revision/order has no scope confirmation
- **liveLeases**: empty, because commit refuses otherwise; a
  [dead primary](#a-dead-primary) rotation lists the leases it moved
- **inFlight**: a dead primary rotation only; see above
- **authoredOpen**: open obligations on other agents for messages the old
  handler authored. Replies to those messages route to the successor.
- **allocationIntents**: unconsumed allocation intents the old run authored
- **queueClaims**: deliberate Queue entries claimed by the old run
- **requiredDeliveries**: current legacy required deliveries for the old run.
  These are listed, not moved (see limits).

The record survives a hub restart.

## Routing after a rotation

- `role:database_handler` resolves to the primary.
- `tt post --to`, `tt send --to` and `tt reassign --to` forward the
  closed old handler's name, or its agent ID, along the rotation chain to the
  live successor. `tt send` rewrites the stated recipient to the successor's name.
- Lifecycle commands (`tt retire`, `tt resume`, `tt close`) never follow a
  rotation.
- A reply with no explicit recipient, to a message a rotated handler authored,
  goes to its live successor.
- New briefings name the successor. An auxiliary handler's briefing still says
  auxiliary.
- `Agent.successorId` is set on a handler closed by a committed rotation.

## Policy and the runner

`handler_rotation_policy` holds `enabled`, `max_items`, `max_total_tokens`,
`on_template_change` and a revision. A zero limit is off. Rotation fires on
whichever limit is reached first. It also holds `dead_silence_minutes`, which
is separate from the limits and from `enabled`; see
[A dead primary](#a-dead-primary).

Defaults (owner decision #14233, option A): 10 finished leased items, 300M total
tokens, template change on. Rotation is enabled for projects created after this
release. The migration saves a disabled policy with the same limits for every
existing project, so the owner turns rotation on for those with `tt handler
policy set`. A stale revision is refused.

- **Items**: `team_queue_entries` in state `finished` whose lease names the exact
  primary run.
- **Tokens**: the exact run's activity `tokens.total`. Only `total` counts the
  same way for Codex (cached input included) and Claude.
- **Template**: the handler template digest is SHA-256 over the static primary
  handler guidance, the queue acceptance briefing and the saved `--prompt`.
  `tt spawn` records it for each handler run in `handler_runs`. A run with no
  recorded digest (a legacy run) counts as changed. The runner compares against
  the digest of its host's saved prompt.

The host relay runs one rotation tick per loop, after the team queue tick and
under the same request budget and backoff. The tick asks the hub nothing unless
a `tt-handler-*` session for that hub runs on the host, or the host keeps a
trace of one: a saved launch spec, a rotation journal, or a handler's session
receipt. A host whose handler died therefore keeps ticking. It then makes one
`GET /v1/handler-rotations/due?host=H&templateDigest=D` request. The relay
loops every 3 seconds, so the tick spaces these requests: at most one a minute
while a listed policy is enabled, and an empty answer is cached for 5 minutes.
For each listed project:

- A local journal for the project: resume that rotation.
- Due and idle with a saved spec: rotate, with trigger `runner` and the first
  due reason.
- Due but busy: nothing this tick. It rotates on the first idle tick.
- Due with no saved spec: one board NOTICE per exact handler run, keyed so a
  relay restart does not repeat it. No spawn. Without a spec the host does not
  know the handler's prompt, so it reports a template change only for a legacy
  run with no recorded digest.
- A [dead candidate](#a-dead-primary) whose silence is met: probe this host
  and, only on gone, rotate with reason `dead_primary`. It is handled before
  the limits and never by them.
- Policy disabled: the project is listed only while its primary is a dead
  candidate, and the runner acts on no limit for it.

## API

| Method and path | Purpose |
| --- | --- |
| `GET/PUT /v1/tasks/{id}/handler-rotation/policy` | Read, or save with `expectedRevision` |
| `POST /v1/tasks/{id}/handler-rotations` | `operation` `prepare`, `commit` or `abort`, keyed by `requestId` |
| `GET /v1/tasks/{id}/handler-rotations[/{rid}]` | List, or read one |
| `GET /v1/handler-rotations/due?host=H&templateDigest=D` | Runner due list, with `online`, `lastActivityAt`, `deadCandidate` and `silenceMet` per primary |

Reusing a request ID with the same input returns the saved record without
writing anything. Reusing it with different input is refused.

## Rollback

The new columns (`tasks.primary_handler_id`, `tasks.handler_revision`) and
tables (`handler_rotation_policy`, `handler_rotations`,
`handler_rotation_requests`, `handler_runs`) stay in place, as do the
`handler_rotations` columns `authorized_by`, `authorization_reason`,
`old_runtime` and `successor_runtime`, which default to empty, and the dead
primary columns: `handler_rotation_policy.dead_silence_minutes` (default 10) and
`handler_rotations.evidence_json`, `commit_evidence_json` and
`last_activity_at`. An older hub binary
ignores them and returns to the legacy primary rules. A handler closed by a
rotation stays closed. An older CLI does not forward the old name, so address the
successor by its new name. Host specs and journals under the relay state
directory are inert without the new CLI.

## Known limits

- Legacy required deliveries stay pinned to the old run. They are listed in the
  handoff, not moved. A follow-up item is needed if any are live.
- Idleness comes from activity snapshots, not a hub record of atomic work (see
  refusals).
- Browser UI, auxiliary-handler rotation, deployment-agent rotation and automatic
  resume of retired handlers are out of scope.
- The owner notice for a missing spec is posted board-wide. The hub has no
  directed owner NOTICE.
- The handler floor learns of a crash only from an `exited` report. A handler
  that dies without one still counts as available, and no notice is posted. A
  primary that died busy is the exception: it is replaced as a
  [dead primary](#a-dead-primary).
- A dead primary is replaced only from its own host. If that host is down, has
  no saved launch spec, lost the run's process receipt, or finds the PID in
  use by another process, the probe is unknown and the handler waits for the
  owner.
- The probe checks the runtime and pane processes, not every descendant. An
  orphaned child that still writes counts as activity and delays or aborts the
  replacement.
- A dead primary rotation whose successor cannot start stays prepared and is
  resumed from the journal, with no retry ceiling.
- The hub does not reprovision a handler when the count reaches zero; that is
  follow-up `wi_42be87739d7bbfc9`.
- `tt close` still refuses a database handler that is not retired on the client
  side, including a non-primary one the hub would accept while another handler
  is available. Retire it first, then close it.
- Team launch cleanup cannot close a failed launch that is the project's only
  handler; the handler stays starting and the owner sees it.
