# Handler rotation

Feature `wi_611e4d3c6992a664`, work order #11573; plan
[handler-rotation-plan.md](handler-rotation-plan.md); default thresholds from
owner decision #14233. [Successor readiness](#successor-readiness) and
[a provider-blocked primary](#a-provider-blocked-primary): bug
`wi_ce7310979bc45479`, order #46760, assignment #46804.

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

Closing a recorded primary that has already exited is allowed
(`wi_fc9b01ea7a2545a2`, order #41795). `tt handler rotate` and every other
rotation skip an exited handler, so the refusal's advice could not be followed.
Only a [dead primary](#a-primary-whose-wrapper-reported-exited) rotation sees
it, and only when it is busy and its wrapper reported the exit; once such a
rotation is prepared it already lifts the refusal. The hub accepts the
close on `PATCH` and on `DELETE` with the handler's exact run. An exited
handler is already unavailable, so its close posts no owner notice. Unchanged:
a `closed` event for an exited agent is refused (`closed`) before the floor
runs, as for any exited agent; retiring an exited agent is refused the same
way; and closing a retired primary is still refused by the primary rule.

Closing a retired handler that is not the primary is always allowed, since it is
no longer available. The hub does not check its leases or obligations; rotation
and the queue own those.

`tt close` applies this rule in two steps (`wi_67fb6b7716a5c1b4`,
`wi_1eb0870a86771168`):

- A retired or exited handler: `tt close` sends the close to the hub with the
  handler's exact run, so the hub's floor decides. A non-primary one, or an
  exited primary, closes and its session is cleaned up. If the hub refuses, for
  example a retired primary, `tt close` prints the hub's reason
  (`hub: 409 conflict: <name> is the project's primary database handler; ...`)
  and leaves the session alone.
- A handler in any other status: `tt close` refuses before sending the close,
  with `the active database handler remains available while the project is open`.
  Retire it first (`tt retire`, which the floor checks) or rotate it. The
  deployment agent gets the same refusal in every status.

Its other checks still run first for a retired or exited handler: the session
must be on this host, and the project orchestrator and an item lead are not
closed this way.

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
tt handler rotate --task TASK --abort  # abort a prepared rotation, or release a held seat

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
resume needs no flags; the journal carries them. A rerun with
`--authorized-by` whose name or reason differs from the journal's is refused
before a spec is saved or anything is spawned, because the resume would replay
the journal's authorization and ignore the flags. The refusal names
`tt handler rotate --abort --task ID`: abort the journaled rotation, then rotate
again with the new authorization. The same check runs again under the journal
lock, for a journal written after the first check. An `aborted` journal is
never resumed, so it is not checked: the rotation finishes its cleanup, then
starts fresh with the flags given. `tt handler rotation get` prints
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
`spawned`, then `committed`, or `aborted` for a rotation the hub has aborted
whose successor cleanup is not yet confirmed.

1. **Prepare.** The hub checks that the old handler is idle (see refusals). It
   records the old run and a preallocated successor ID. The prepare names the
   old agent, its exact run and the handler revision it expects. The owner's
   `tt handler rotate` reads the current primary; a runner rotation names the
   exact agent, run and revision of the due entry it acts on. If the primary
   changed since, the hub refuses it (`handler_changed` or `not_primary`) and
   nothing is saved. The successor's name is
   the base name plus `-r<handler_revision+1>`, for example `db-handler-r2`. From
   then on, queue dispatch leases neither handler.
2. **Launch.** The host runs `tt spawn --role database_handler
   --handler-successor` with the saved spec. The successor gets the primary
   handler prompt, plus a line telling it to wait for the handoff NOTICE. Its
   template digest, model and reasoning are recorded for its exact run in the
   same transaction as the registration that starts the run.
3. **Wait.** The routine waits up to two minutes for the successor to come
   online, then for its first completed turn, up to 10 minutes from its launch
   (see [Successor readiness](#successor-readiness)). Until then the rotation
   stays prepared. The runner resumes it only up to a ceiling; see
   [A successor that never comes online](#a-successor-that-never-comes-online).
4. **Commit.** In one hub transaction, the hub:
   - re-checks that the old handler is idle
   - verifies the successor is online on the same host and directory, and on
     the same runtime and arm unless the rotation carries an authorization
   - verifies the successor's exact run has completed a turn with a model reply
     since the prepare
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
obligations. A new prepare is allowed afterwards. Once the hub confirms an
abort, the host saves the journal as `aborted`, then cleans up the successor
and removes the journal. If the cleanup fails, the `aborted` journal stays:
`--abort` again, a rerun of `tt handler rotate`, or the runner's next tick
finishes the cleanup first. An `aborted` journal is never resumed. With no
prepared rotation, `--abort` releases the
[successor hold](#successor-seat-hold) of the current primary run, whatever
its reason, and prints "Released the agent seat held for the REASON rotation
of NAME; nothing was prepared." With no hold either it fails with "no prepared
handler rotation to abort".

## Successor readiness

Bug `wi_ce7310979bc45479`, order #46760. On 2026-10-25 a successor came online
(its wrapper's heartbeat), the commit moved every obligation to it, and its
first turn then failed on a provider 429 before it ran any `tt` command. It
never bound to the relay, so no wake reached it and the obligations waited for
a person. Online is therefore not readiness.

- **At commit** the hub refuses `successor_not_ready`, and moves nothing,
  unless the successor's exact agent and current run shows one of two kinds
  of evidence since the prepare:
  - a completed turn: an activity receipt observed at or after the prepare
    with state `idle` or `finished_silent`, no provider block, a turn-end
    reason that is not a provider error (`turn ended by provider error`), and
    output tokens above zero; or
  - a post: a message it posted at or after the prepare (bug
    `wi_e09e377485f7f21a`), while its current activity is neither
    `provider_blocked` nor a turn that ended `turn ended by provider error`.
    Hub broker messages never count. A post is a model reply even when the
    relay never samples the turn idle. The hub reads the project's messages
    newest first and stops at the first one older than the prepare.

  Receipts and messages are read by agent and run, so the old handler's
  activity or posts, another agent's message to the successor and an earlier
  run of the successor never count. This applies to every reason, a dead
  primary included. A busy old handler refuses the commit only for a ready
  successor; one that is not ready is refused `successor_not_ready` whatever
  the old handler is doing, so the bound below still counts.
- **The host routine** asks the hub on each poll by attempting the commit; a
  refused commit writes nothing. `tt handler rotate` polls every 10 seconds;
  the runner tries once per tick. A dead-primary rotation runs its second
  death probe once a pass, and only for a successor that may be ready: its
  activity shows a completed turn or any output, or the hub, asked to commit
  without evidence, answers something other than `successor_not_ready`. The
  hub checks readiness before the death evidence, so that ask is answered
  without a probe, and an unknown probe under load cannot abort a rotation
  whose successor is still starting.
- **Until 10 minutes after the launch** (`spawnedAt` in the journal; a journal
  without it starts the 10 minutes at its first resume) a successor that is
  not ready is a wait: the rotation stays prepared, the runner neither counts
  nor logs it, and the old handler keeps every obligation. Nothing is sent to
  the successor, so the wait does not depend on a relay wake reaching it.
- **After 10 minutes** each resume that still finds it not ready is a counted
  failure ("did not complete a turn or post a message within 10m0s"), and at
  the [ceiling](#a-successor-that-never-comes-online) the runner stops with
  one stuck notice each to the owner helper and the handler.

The successor's briefing tells it to verify `tt` and its identity, post
exactly one NOTICE to the primary handler saying it is waiting for the
handoff, and end its turn. It never waits or polls for the handoff NOTICE
inside a turn and does not page its unread backlog first; the hub posts the
NOTICE when it commits, and the relay wakes the successor then.

Why posts count, and why the bound did not change: on 2026-10-30 three
rotations went stuck although each successor had posted within minutes that
it was waiting for the handoff. The relay samples a run's state, not its turn
ends, at most every 15 seconds, and each successor was idle for well under a
second between turns, so no sample saw a completed turn. Activity-report lag
was not the cause, and no longer window would fix a sampler that never sees
the idle state. The bound stays 10 minutes from `spawnedAt`. A successor that
never posts and is never sampled idle still ends stuck.

A prepared rotation can therefore last about 13 minutes: the two minute online
wait, the 10 minute readiness bound and three counted resumes a minute apart.
Queue dispatch leases neither handler meanwhile, as for any prepared rotation.
After that it is stuck, and the runner aborts it when the old handler recovers
or after 60 minutes; see
[Automatic abort of a stuck rotation](#automatic-abort-of-a-stuck-rotation).

## Refusals

A refused prepare or commit returns 409 with a named `code` and changes nothing.
The rotation row, successor and obligations stay as they were.

| Code | Meaning |
| --- | --- |
| `live_lease` | A team queue entry leased to the old handler's exact run is `launching`, `running`, or `failed` and not released. |
| `working` | The old run's activity is `working`, `hung_tool` or `looping`. For the runner, also any state other than `idle`, `finished_silent` or `provider_blocked`. |
| `pending_tool` | The old run has a pending tool call. |
| `rotation_open` | Another rotation of this project is still prepared. |
| `project_paused` | The project is paused. |
| `not_primary` | The named run is not the current primary handler. |
| `handler_changed` | The handler revision or the old run changed since the caller read it. |
| `name_taken` | The successor name is wrong or already used by an open agent. |
| `successor_unavailable` | At commit, the successor is not a registered, online handler on the old handler's host, runtime and directory. An [authorized change](#changing-the-primarys-runtime-or-model) may differ in runtime only. |
| `agent_caller` | The request carries an agent identity. |
| `arm_changed` | At commit, the project has a saved [handler arm policy](handler-ab.md), the old run belongs to one of its arms and the successor does not belong to the same arm. `tt handler rotate` refuses before spawning in that case when the saved spec's `--model` or `--reasoning` differs from the old run's. Without a policy, or for a run in no arm, a model change is accepted. An [authorized change](#changing-the-primarys-runtime-or-model) skips this check and does not write the policy. |
| `successor_not_ready` | At commit, for every reason: the successor's exact run has no report since the prepare of a completed turn with a model reply. See [Successor readiness](#successor-readiness). |
| `not_blocked` | A [provider-blocked](#a-provider-blocked-primary) prepare only: the old run is not online and idle with a `rate_limited` or `server_error` episode open for 20 minutes and an obligation that needs more than delivery, or the dead primary replacement is off. |
| `death_unconfirmed` | A [dead primary](#a-dead-primary) rotation only: the host's evidence is missing, stale, from another host or run, names no session, or does not state both the session and the process gone; or the hub had a heartbeat from the run within 90 seconds; or the handler is not busy; or the replacement is off for the project. For a [primary whose wrapper reported exited](#a-primary-whose-wrapper-reported-exited): the hub has no exit report from the wrapper as the run's newest start, heartbeat or exit, or that report changed since prepare; the evidence carries no exit from the host's receipt; or it states an `idle_shell` session without the pane's process. |
| `no_agent_slot` | Prepare only, for every reason and trigger, checked after the busy, primary and revision refusals: the project's open agents (every agent not closed or exited, done and retired ones and persistent roles included), plus the seats that admitted team launches still have to register, plus the successor, would exceed the project's agent cap (32). The detail carries the counts, such as `project agent cap: 32 open + 1 seats > 32`. Nothing is prepared, so queue dispatch is not held and the old handler stays primary. A `template` or `manual` prepare also records a [successor hold](#successor-seat-hold), and its detail ends "; the next free agent seat is held for the successor". See [Policy and the runner](#policy-and-the-runner). |
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
(`unknown`) run as idle. The runner requires an observed `idle`,
`finished_silent` or `provider_blocked` state.

## A dead primary

Bug `wi_b863e669073858f4`, owner order #28057.

A primary that dies while the hub records it as busy keeps that state and its
leases, so the refusals above never clear and every team waits. The host runner
replaces such a handler automatically, and only when **both** hold:

1. **Its own host confirms its process and session gone.** Not merely
   unreachable: a state the host cannot read is not confirmation. The host
   confirms two facts, listed under [The host's probe](#the-hosts-probe).
2. **The hub has recorded no activity from that exact run for the project's
   silence**, 10 minutes by default.

A busy primary whose runtime exited while its wrapper survived keeps its
session, so this rule never fires for it. It is covered by
[a primary whose wrapper reported exited](#a-primary-whose-wrapper-reported-exited)
once its wrapper's exit report reaches the hub, including through the
wrapper's [retry of a lost exit report](#a-lost-exit-report).

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
2. The receipt's tmux socket differs from the runner's: **unknown**. Neither
   the receipt nor the roster names the handler's tmux session: **unknown**,
   because there is then nothing to look for in the listing.
3. The tmux session listing cannot be read: **unknown**. Otherwise a session
   tagged with this agent (any run), or named as the receipt or the roster
   names the handler's session: **alive**.
4. The runtime process by PID and start identity: a failed check is
   **unknown**, including a PID now used by another process; running is
   **alive**.
5. Only when the receipt records a pane process: the same check for it, and a
   pane PID with no start identity is **unknown**.
6. Otherwise **gone**.

So gone confirms two facts read on the handler's own host: its named session
is absent from a readable listing, and its runtime process is absent by PID
and start identity. The wrapper records its session's pane process in the
receipt when it starts the runtime: the PID of the session's single pane and
that process's start identity, read once after the session identity. So for a
handler started by this release the pane is a third fact. A receipt with no
pane process skips step 5: one saved by a wrapper from before this release,
or one whose pane read failed, found no pane or more than one. The probe
signals and stops nothing. Only gone produces
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
| Both states are `gone`, with the session's name, a PID and a start identity. | `death_unconfirmed` |
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

### A primary whose wrapper reported exited

Bug `wi_1d987e7f296b6a2e`, owner order #28426.

A runtime that crashes usually leaves its `tt wrap` wrapper alive. The wrapper
posts `exited` with the text `Process exited (N)`, writes `exitedAt` to its
process receipt, and leaves a login shell in the pane, so the tmux session
stays. The rules above never fire for it, because the session is present. A
busy primary in that state is replaced through the same prepare and commit,
with these differences. Everything not listed here is unchanged: the silence,
the 90 second heartbeat check, fresh evidence from the handler's own host and
exact run, the second look, the single commit transaction and the off switch.

**Which handler.** Only a `dead_primary` prepare and the due list see an
exited handler as the primary: the explicit primary when it is not closed,
otherwise the oldest handler that is not closed and not a prepared successor,
when its status is `exited`. Every other rotation, and `tt handler rotate` by
hand, still skips an exited handler.

A `dead_primary` prepare uses the exited handler only when the request names
it or it is busy, the same condition as the due list. An idle exited primary
beside another open handler leaves that handler to the rules above.

**The exit report (hub).** The agent's newest `started`, `heartbeat` or
`exited` event must be an `exited` event whose text is exactly the wrapper's
`Process exited (N)`. The hub saves its status, time and event on the rotation
as `exit` (`code`, `reportedAt`, `eventSeq`). An event that is missing, was
pruned (the hub keeps the newest 10,000 events per project), or was posted by
hand with `tt event exited` is refused `death_unconfirmed`. The silence starts
at the exit report or at any later activity of the run.

**The evidence.** `processState` must be `gone`, the evidence must carry the
receipt's `exitedAt`, and `sessionState` is either `gone` or `idle_shell` with
`paneRootPid`, the pane's process. So the host's own receipt must record the
exit as well: an `exited` event alone is never enough. For a primary that is
not exited nothing changed: `idle_shell` is refused and both states must be
`gone`.

**The probe.** For a handler whose roster status is `exited`, steps 1 and 2
above run first, then:

1. The receipt records no exit: **unknown**.
2. The session listing cannot be read: **unknown**.
3. No session is tagged with this agent or named as the handler's session: the
   remaining steps above apply, and gone states the session `gone`.
4. Otherwise the runtime process by PID and start identity: a failed check is
   **unknown**, running is **alive**.
5. More than one such session, or one that is not tagged with this hub,
   project, agent and exact run, that does not carry the handler's session
   name, or whose tmux ID or creation time differs from what the receipt
   records: **unknown**.
6. The session's panes cannot be read, or it has no pane or more than one:
   **unknown**.
7. The receipt records no pane process, no pane start identity or no login
   shell, or the session's one pane is not the recorded pane process:
   **unknown**.
8. The process table (`ps`: process ID, parent ID, start time and executable)
   cannot be read, has a row that does not parse, lacks the pane's process, or
   gives it another start time than the receipt records: **unknown**.
9. The pane's process must have exactly one child, and that child none. A
   process below that child is **alive**
   (`process N runs under the pane's shell`). A child running another
   executable than the login shell the receipt records is **alive**
   (`process N in the pane is not the wrapper's login shell`). A child that
   started before the recorded exit, to the second, or whose start cannot be
   read, and any other shape, is **unknown**.
10. Otherwise **gone**, with `sessionState: idle_shell` and `paneRootPid`.

The wrapper records the login shell it leaves in the pane: `$SHELL`, or
`/bin/sh` when that is unset, the same choice it makes when it starts the
shell. Step 9 compares executable names, ignoring the directory and a login
shell's leading dash. Anything running under the leftover shell blocks the
replacement, which covers a runtime someone started again by hand in that
shell. So does a runtime started with `exec` in the shell: it keeps the
shell's process ID and start time, but not its executable.

**A restart cancels it.** Starting an exited handler again gives it a new run
and a status other than `exited`. A prepare for the old run is then refused
`not_primary`, a prepared rotation's commit is refused `handler_changed`, and
the runner's second probe is not run for another run, so it aborts. Any
activity recorded from the run after prepare refuses the commit `not_silent`.
The commit also needs the same exit event still to be the run's newest start,
heartbeat or exit. A rotation prepared without a saved exit never replaces a
handler that exited afterwards (`handler_changed`).

**What is handed off.** As above, plus one addition. The broker closes an
exited recipient's open obligations as `recipient_gone` within one tick (30
seconds), long before the silence is met. The commit therefore also re-issues
to the successor the old handler's obligations closed `recipient_gone` with
the reason `recipient agent is exited` at or after the exit report. An
obligation closed earlier or for any other reason is not re-issued, and a
re-issued one is marked superseded, so a replay re-issues nothing twice.
After the commit the runner cleans up the old session, which ends the leftover
shell, as in every rotation.

**The notice.** Same subject and the same two copies. Its evidence sentence
differs: it gives the exit status and time, says the runtime process is
absent, and says the tmux session `is absent` or
`holds only the wrapper (process P) and its idle shell`. It begins:

`Its runtime exited with status N at TIME, as reported by its wrapper.`

`tt handler rotation get` prints the saved exit report as:

`Wrapper exit report: status N, reported TIME (event S)`

**Not proven on a live host.** The strict pane rule is covered only by tests
with injected process tables. A login shell that keeps a helper process
running under it would make step 9 answer alive on every probe, and that
handler would wait for the owner, as before this change. What a real exited
handler pane on this project's host holds has not been observed yet.

#### A lost exit report

Bug `wi_b01a7afa10979dd0`, owner order #41362.

The wrapper's first `exited` post can fail, for example while the hub is
unreachable. When it fails and the wrapper's receipt records the exit, the
wrapper retries the same `Process exited (N)` post in the background after
15 seconds, 30 seconds, 1, 2 and 4 minutes, then every 5 minutes, for as long
as its login shell runs. It posts only while the receipt names this wrapper
as the pane process and the pane holds just the wrapper and its idle login
shell, the probe's step 9 rule above (the wrapper's own `ps` is left out of
the table it reads). So a runtime started again in that shell is never
reported exited; the retry waits and posts once the shell is idle again. The
receipt records the outcome: `exitReportedAt` when the hub accepted the
report, or showed with a 409 that it already holds this run's exit (an
earlier response was lost), or `exitReportStopped` with the reason when the
hub refused it for good (a 409 for a closed agent or another run, or any
other 4xx except 429). A 429, a 5xx, a lost connection or an unreadable
process table waits for the next attempt. The retry never posts without the
receipt's `exitedAt`, and the hub is unchanged: the accepted report is an
ordinary wrapper exit report, so the silence starts at its acceptance, not at
the exit.

## A provider-blocked primary

Bug `wi_ce7310979bc45479`, order #46760 (q2). A primary that a provider rate
limit or server error stops is idle, healthy and online, so neither the limits
nor the dead primary rule replace it. The hub first retries it with bounded
wakes ([provider-blocked.md](provider-blocked.md#retry-wakes)). If it is still
blocked after 20 minutes while it holds work, the host runner replaces it.

The hub lists the primary with due reason `provider_blocked` when all hold:

- the project's dead primary replacement is on (`deadSilenceMinutes` above 0);
  `enabled`, which governs the limits, does not matter
- the primary's current run is online by the hub's clock and idle for the
  runner (`provider_blocked` counts as idle)
- that run has an open `rate_limited` or `server_error` episode that opened at
  least 20 minutes ago (`api.HandlerProviderBlockedAfter`, frozen); `auth` and
  `usage_limit` keep their own recovery
- it holds at least one obligation that needs more than delivery

The due list keeps such a project with `enabled` false, and keeps listing a
prepared `provider_blocked` rotation so the runner can resume it.

The runner handles this reason before the limit policy and the limits. With a
saved spec it rotates with reason `provider_blocked` and trigger `runner`;
without one it posts the missing-spec notice. The prepare must come from the
runner, with no authorization and no death evidence (otherwise 400), and the
hub re-checks the conditions above for the exact old run (`not_blocked`
otherwise). The commit uses the ordinary idle check and
[successor readiness](#successor-readiness). A successor on the same account
that is rate limited too never becomes ready: the rotation ends in the stuck
notice and the old handler keeps its obligations, while its retry wakes go on.
When a retry wake lets the old handler complete a turn again, the runner aborts
the stuck rotation at its next tick, which lifts the dispatch hold; see
[Automatic abort of a stuck rotation](#automatic-abort-of-a-stuck-rotation).
If the old handler recovers before the commit, the rotation still completes as
an ordinary one once the successor is ready.

The commit posts, in its own transaction, one NOTICE "A provider-blocked
primary database handler was replaced automatically" to the project's owner
helper (to the Board when none is open). It names the old and new handler, the
block's class and since (and whether it cleared before the commit), the number
of obligations moved and the policy command that turns the replacement off. A
replayed commit returns the saved record and posts nothing; the receipt's
`ownerNoticeSeq` names the notice. Obligations move by the ordinary keyed
re-issue, once each.

The switch is the dead primary's: `tt handler policy set --task TASK
--revision N --dead-silence-minutes 0` turns both off.

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
  `tt spawn` records it for each handler run in `handler_runs`, with the run's
  model and reasoning, in the same transaction as the registration that starts
  the run: a failed record fails the registration, so no run is saved without
  the values its launch carried. A backlog steward run's digest is recorded the
  same way in `steward_runs`. A run with no
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
Before the request, the tick finishes every `aborted` journal this host keeps
for the hub, listed or not: it cleans up the successor's session, then removes
the journal; a failure is tried again at the next tick that asks for the due
list. For each listed project:

- Each rotation the runner starts prepares against the due entry's exact
  agent, run and handler revision. If the primary changed since the due list,
  the hub refuses the prepare as `handler_changed` or `not_primary`; the
  runner reports nothing, keeps no journal and reads the change with its next
  due request. Only a refused prepare is silent: when the same call reaches
  its commit and the hub refuses it as `handler_changed`, the tick reports it.
  The rotation stays prepared with its journal, and the refusal says to abort
  it.
- A local journal for the project: resume that rotation, unless it has
  reached the resume ceiling below, or is `aborted` (the sweep above finishes
  it). A stuck one is aborted automatically when its old handler recovers or
  after 60 minutes (below).
- Due and idle with a saved spec: rotate, with trigger `runner` and the first
  due reason.
- Due but busy: nothing this tick. It rotates on the first idle tick.
- Due, but the hub refuses the prepare as `no_agent_slot`: a wait, the same
  for the limits, `provider_blocked` and `dead_primary`. The runner keeps no
  journal, launches nothing and reports no error; it tries again at every
  tick and rotates at the first one after an agent is closed. It posts one
  board NOTICE, "A handler rotation waits because the project has no free
  agent slot", per exact old run and reason, keyed so a relay restart does
  not repeat it. The notice gives the hub's counts and names, up to ten, the
  done or retired ordinary agents an owner could close; a handler, deployer,
  steward or owner helper is never named. It closes nothing. Meanwhile queue
  admission and automatic handler provisioning hold one seat for the
  successor, so the next agent closed frees a slot for the rotation rather
  than for a new team or handler
  ([project-queue.md](project-queue.md#project-agent-cap), "Handler successor
  seat"). The notice says so. A rotation due only for a template change holds
  its seat through the [successor hold](#successor-seat-hold) its refused
  prepare recorded.
- A template [successor hold](#successor-seat-hold) for the primary's run
  that this host no longer finds due (the limit policy or
  `onTemplateChange` is off, or the run's recorded template is this host's
  saved prompt): released with `release_hold`, once, before the rest of the
  tick. The runner never releases a manual hold.
- Due with no saved spec: one board NOTICE per exact handler run, keyed so a
  relay restart does not repeat it. No spawn. Without a spec the host does not
  know the handler's prompt, so it reports a template change only for a legacy
  run with no recorded digest.
- A [dead candidate](#a-dead-primary) whose silence is met: probe this host
  and, only on gone, rotate with reason `dead_primary`. It is handled before
  the limits and never by them.
- Due reason `provider_blocked`: rotate with that reason, or post the
  missing-spec notice; see [A provider-blocked primary](#a-provider-blocked-primary).
  It is handled before the limits and whatever `enabled` says.
- Policy disabled: the project is listed only while its primary is a dead
  candidate or provider blocked, or a `provider_blocked` rotation is prepared,
  and the runner acts on no limit for it.

### Successor seat hold

The hub's own due rule cannot see two rotations: one due only for a template
change (only the runner knows its saved prompt) and a manual one. For these
the refused prepare is the report (bug wi_bbea3ea3ad6068a1, order #49564):

- **Recorded.** A prepare with reason `template` or `manual` that the hub
  refuses `no_agent_slot` saves one hold for the project, naming the exact old
  agent and run, the reason, the trigger and the handler revision, and still
  returns the 409. No rotation, request receipt or "Handler rotation prepared"
  event is saved. A retry for the same run keeps the hold, except that a
  manual refusal upgrades a template hold to manual (a template refusal never
  downgrades a manual one); a refusal for another run replaces it. The event "Handler successor
  seat held" marks a new hold only; an upgrade adds none. Other reasons record nothing: the due rule
  already holds their seat.
- **Counted.** With no prepared rotation, the hold holds the successor seat
  for queue admission and automatic handler provisioning while it names the
  current primary's exact agent and run, and, for a template hold, while the
  limit policy and `onTemplateChange` are on. A manual hold counts whatever
  the policy says. Prepare's own check never counts it, so one free slot
  prepares the rotation.
- **Ended.** A prepare that succeeds, for any reason, deletes the hold: the
  prepared rotation holds the seat from then until its successor registers.
  A hold for an earlier run of the primary no longer counts.
- **Released.** The operation `release_hold` on
  `POST /v1/tasks/{id}/handler-rotations` takes only `requestId`, `oldAgentId`
  and `oldRunId`. It deletes the hold of that exact run, records "Handler
  successor seat released" and returns an aborted rotation with no ID that
  names the hold. It keeps no receipt; with no hold for that run it is
  `404`. The runner releases a stale template hold (above); the owner
  releases any hold with `tt handler rotate --abort`. A refused
  `tt handler rotate` says so: "the next free agent seat is held for this
  rotation; rerun tt handler rotate --task ID once an agent is closed, or
  release the seat with tt handler rotate --abort --task ID".

With no hold, admission, provisioning and every refusal text are as before.

### A successor that never comes online

A prepared rotation whose successor cannot start, because of a bad launch spec
or a host out of capacity, is not resumed without end. The runner counts each
resume that ends with the launch unconfirmed, the successor not online
within the two minute wait, or the successor not
[ready](#successor-readiness) 10 minutes after its launch. The count is kept with the rotation in the host
journal (`resumeFailures`, `lastResumeError`), so a relay restart does not
reset it.

After 3 failed resumes of one rotation the runner stops resuming it:

- The rotation stays prepared, until the runner
  [aborts it automatically](#automatic-abort-of-a-stuck-rotation) or a person
  resumes or aborts it.
- The same successor identity is used for every attempt, and no other
  successor is launched.
- The old handler stays the primary, with its leases and obligations.
- The runner posts one NOTICE to the project's owner helper and one to the
  handler being replaced, once per rotation and not per tick. With no owner
  helper registered, that copy goes to the Board. Each names the project, the
  handler, the successor, the host, the number of failed resumes and the last
  error, the automatic abort rule for the rotation's reason, and the recovery
  advice for the rotation's state (below).
- Each notice's request identity names the rotation and its recipient: the
  owner helper's agent, the handler's agent, or the Board. While the
  rotation stays stuck, every tick reads the project's agents and tells only
  recipients not told before, with no restart needed: an owner helper that
  registers after the Board copy gets its own notice at the next tick, once,
  and when the helper has left, one copy goes to the Board. A tick that finds
  no new recipient posts nothing. A restarted relay posts no second copy to
  anyone, and tells the recipients that changed while it was down in the
  same way. If the notice would now read differently for a recipient
  already told (the rotation moved to another state, or the project was
  renamed), the hub refuses the reused identity and the runner treats that
  recipient as told; it does not retry. Any other failed post is tried again
  on a later tick.

The run that prepares the rotation is not a resume, so the successor gets
four chances in all. A busy refusal, a dead primary refusal, a `not_blocked`
refusal and a successor that is online but not yet ready within its 10 minutes
are waits and are not counted. A successor that comes online on a resume below the ceiling
commits as usual, and nothing is posted.

Recovery is by hand, on the handler's host, after fixing the launch (the saved
spec from `tt handler spec`, or the host's capacity). What a resume does
depends on how far the rotation got, and the notice gives the advice for that
state:

- **The launch was never confirmed** (journal phase `prepared`). A resume
  launches the successor again, so resume or abort:

  ```sh
  tt handler rotate --task TASK          # resume: launch the successor again
  tt handler rotate --abort --task TASK  # abort it; the old handler stays primary
  ```

- **The successor was launched but is not online** (journal phase `spawned`).
  A resume only waits two minutes for that same session again; it never
  launches another. Abort the rotation. A rotation that is still due then
  starts fresh with a new successor, at the runner's next tick or at once:

  ```sh
  tt handler rotate --abort --task TASK  # abort it; the old handler stays primary
  tt handler rotate --task TASK          # optional: start the fresh rotation now
  ```

  A resume finishes this rotation only if that launched session can still
  come online.

Both commands are accepted after the ceiling. A manual resume that fails again
leaves the runner stopped and posts nothing more. After an abort, a rotation
that is still due starts fresh with its own count.

### Automatic abort of a stuck rotation

Bug `wi_63793d7e551231f6`, order #46928. Queue dispatch leases neither handler
while a rotation is prepared. A stuck rotation therefore held a project's
dispatch until a person aborted it, even after the old handler had recovered,
for example a `provider_blocked` handler whose retry wake succeeded while its
same-account successor stayed rate limited.

The runner now aborts a stuck rotation itself, at the first tick that finds
either:

- **The old handler recovered** (`provider_blocked` rotations only). The due
  list's primary is the rotation's exact old agent and run, and its current
  activity shows a completed turn with a model reply (the same test as
  [successor readiness](#successor-readiness)) whose last event is after the
  rotation went stuck. A turn from before that time, another run of the old
  agent, or a report that is still provider blocked does not count. Other
  reasons do not use this trigger: their old handler never stopped, so its next
  ordinary turn would abort a rotation that is still due, and the runner would
  launch a new successor at once.
- **The stuck ceiling.** 60 minutes after the rotation went stuck, for any
  reason. The time it went stuck is saved in the host journal (`stuckAt`) when
  the resume ceiling is reached, so a relay restart keeps it. A journal stuck
  before this release has none, and its 60 minutes start when the runner first
  sees it stuck.

The abort is the one `tt handler rotate --abort` makes, keyed
`rotation-abort-<rotation id>`. It makes no commit and leaves the old handler
the primary with its leases and obligations; none moves to the successor and
none is created for it. It closes the successor and cleans up its session, and
removes the journal. With no rotation prepared, queue dispatch leases the old
handler again.

After an abort when the old handler recovered, a rotation that is still due
starts fresh at a later tick, with its own count and its own stuck notice.
After an abort at the stuck ceiling, the runner backs off instead of preparing
the same failing rotation again at its next tick. It saves a host record
(`handler-backoff-*.json` in the relay state, so a relay restart keeps it) of
the old agent, its exact run and the reason, and prepares no new rotation for
that same run and reason for 60 minutes. Each later ceiling abort of the same
run and reason doubles the interval, up to 24 hours. A due entry with another
run or another reason ignores the record and prepares at its next tick; a
committed rotation removes it; `tt handler rotate` ignores it; an abort when the
old handler recovered, or a manual `--abort`, records none. The stuck notice
states the back-off. A rotation that went stuck because its successor could
not register at the agent cap needs no back-off after a manual `--abort`:
while no slot is free, the runner's next prepare is refused as
`no_agent_slot`, so nothing is prepared and dispatch keeps running.

The stuck notice goes first: the runner aborts only at a tick where every
recipient has been told, and posts nothing about the rotation after the abort.
The notice states this rule. Before either trigger the rotation stays prepared
and the runner posts nothing more. A failed abort keeps the journal, and the
next tick that lists the project tries again with the same key. A failed
cleanup after the hub confirmed the abort keeps the journal as `aborted`, and
every later tick that asks for the due list retries the cleanup, then removes
the journal, even once the project is no longer listed (for example, a
recovered `provider_blocked` primary with its limit policy off).

## API

| Method and path | Purpose |
| --- | --- |
| `GET/PUT /v1/tasks/{id}/handler-rotation/policy` | Read, or save with `expectedRevision` |
| `POST /v1/tasks/{id}/handler-rotations` | `operation` `prepare`, `commit` or `abort`, keyed by `requestId`; `release_hold` ends a [successor hold](#successor-seat-hold) and keeps no receipt |
| `GET /v1/tasks/{id}/handler-rotations[/{rid}]` | List, or read one |
| `GET /v1/handler-rotations/due?host=H&templateDigest=D` | Runner due list, with `online`, `lastActivityAt`, `deadCandidate`, `silenceMet` and any `successorHold` of the exact run per primary; `dueReasons` may include `provider_blocked` |

Reusing a request ID with the same input returns the saved record without
writing anything. Reusing it with different input is refused.

## Rollback

The new columns (`tasks.primary_handler_id`, `tasks.handler_revision`) and
tables (`handler_rotation_policy`, `handler_rotations`,
`handler_rotation_requests`, `handler_runs`, `handler_successor_holds`) stay
in place, as do the
`handler_rotations` columns `authorized_by`, `authorization_reason`,
`old_runtime` and `successor_runtime`, which default to empty, and the dead
primary columns: `handler_rotation_policy.dead_silence_minutes` (default 10) and
`handler_rotations.evidence_json`, `commit_evidence_json`,
`last_activity_at` and `exit_json` (the saved exit report, empty for any other
rotation). An older hub binary
ignores them and returns to the legacy primary rules; it counts no
successor hold, and the next prepare that succeeds after an upgrade deletes
any hold left from before. A handler closed by a
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
  [dead primary](#a-dead-primary) when its session is gone, or as an
  [exited primary](#a-primary-whose-wrapper-reported-exited) once its
  wrapper's exit report reaches the hub, if need be through the
  [retry](#a-lost-exit-report).
- A dead primary is replaced only from its own host. If that host is down, has
  no saved launch spec, lost the run's process receipt, or finds the PID in
  use by another process, the probe is unknown and the handler waits for the
  owner.
- The probe checks the handler's session, its runtime process and its pane
  process, not every descendant. A run started before the wrapper recorded the
  pane process has none in its receipt, and its pane is not checked. An
  orphaned child that still writes counts as activity and delays or aborts the
  replacement.
- An [exited primary](#a-primary-whose-wrapper-reported-exited) waits for the
  owner when the wrapper's exit event was pruned or posted by hand, when the
  host's receipt records no exit, or when anything runs under the pane's
  leftover shell. The strict pane rule is not yet proven on a live host.
- An exited primary whose run started before the wrapper recorded the pane
  process and login shell has neither in its receipt. Its probe is unknown
  and it waits for the owner.
- The pane rule tells the wrapper's login shell from a runtime that replaced
  it with `exec` by executable name only. A runtime that is itself the
  recorded shell, such as `exec zsh` in a zsh pane, still looks like the idle
  shell.
- An obligation the broker closed is re-issued at commit, ten or more minutes
  later. A sender who re-sent the request in between reaches the successor
  twice. Follow-up filed.
- A [lost exit report](#a-lost-exit-report) is recovered only by the
  wrapper's retry. Until it reaches the hub, the handler is not exited, its
  session is present and the probe answers alive. A wrapper that dies before
  any post succeeded usually takes its session with it: `tmux kill-session`, a
  host restart, or a SIGKILL of the wrapper, which closes the session's only
  pane. The session is then absent, and the handler is replaced as a
  [dead primary](#a-dead-primary) once the silence passes, unless its probe is
  unknown for a reason in the limit above. These still wait for the owner: a
  wrapper that dies while its session stays present, such as a session with a
  second pane or window, or a tmux configuration with `remain-on-exit`; a
  receipt that could not record the exit, or that does not name the wrapper
  as the pane process or names no login shell (`exitReportStopped`); a hub
  that refuses the report for good, recorded in `exitReportStopped`; and
  anything that keeps running under the leftover shell, during which the
  retry waits.
- An exited primary that is not busy is not replaced by this rule; see the
  handler floor and follow-up `wi_42be87739d7bbfc9`.
- The queue's waiting reason for a project whose primary exited is unchanged.
- The resume ceiling counts only a successor that could not be launched or
  did not come online. A resume that fails another way, such as a hub that
  cannot be reached at commit, is still retried on every tick.
- The hub does not reprovision a handler when the count reaches zero; that is
  follow-up `wi_42be87739d7bbfc9`.
- Readiness comes from the relay's activity report for the successor's run or
  from a message the run posted. A Codex successor binds to the relay only
  through its own `tt` commands, so one whose first turns never run one and
  never post is never ready; it ends in the stuck notice and the old handler
  keeps its obligations. The relay samples state, not turn ends, so a
  successor that ignores its briefing, never posts and is never sampled idle
  ends stuck the same way.
- A runner whose due list was empty asks again only after 5 minutes, so a
  `provider_blocked` replacement in a project with nothing else listed can come
  up to 5 minutes after the 20 minute bound.
- `tt close` still refuses a database handler that is neither retired nor
  exited on the client side, including a non-primary one the hub would accept
  while another handler is available. Retire it first, then close it.
- Team launch cleanup cannot close a failed launch that is the project's only
  handler; the handler stays starting and the owner sees it.
- The `no_agent_slot` check runs at prepare; the successor registers later,
  after its launch. An agent that registers in between without a queue
  reservation, such as a manual `tt spawn` or a restarted owner helper, can
  take the last slot. The successor's registration then fails with `409 limit
  reached`, and the rotation follows the
  [resume ceiling](#a-successor-that-never-comes-online) and the
  [automatic abort](#automatic-abort-of-a-stuck-rotation). Queue admission
  and automatic handler provisioning hold one seat for a successor while a
  rotation is prepared and waiting for it, due on the hub's own rule (limits,
  `provider_blocked`, or `dead_primary` with the silence met), or held by a
  [successor hold](#successor-seat-hold), but registration outside the queue
  does not honor that seat
  ([project-queue.md](project-queue.md#project-agent-cap)).
- A manual rotation holds a seat only from the first `tt handler rotate` the
  hub refuses `no_agent_slot`; before that the hub cannot know the owner
  means to rotate. A [successor hold](#successor-seat-hold) does not expire:
  if its runner host stops ticking, or an owner never reruns or aborts a
  manual rotation, it holds the seat until `tt handler rotate --abort` or a
  new primary run.
- The `no_agent_slot` notice is posted once per exact old run and reason. If
  the project fills again later for the same run and reason, no second notice
  is posted.
