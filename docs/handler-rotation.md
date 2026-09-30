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

tt handler rotation list --task TASK
tt handler rotation get ROTATION --task TASK   # handoff snapshot and receipt

tt handler policy get --task TASK
tt handler policy set --task TASK --revision N [--enabled=BOOL] [--max-items N] \
  [--max-total-tokens N] [--on-template-change=BOOL]
```

A launch spec accepts only `--run`, `--cwd`, `--prompt`, `--runtime`, `--model`,
`--reasoning`, `--permission-mode`, `--approval-mode`, `--sandbox-mode` and
`--allowed-tools-json`. It is stored at mode 0600 under the relay state directory
and keyed by hub and project. Its runtime and directory must match the old
handler's.

Run `tt handler rotate` on the old handler's host. That host must stop the old
session, and the successor must run on the same host, runtime and directory.

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
   - verifies the successor is online on the same host, runtime and directory
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
| `successor_unavailable` | At commit, the successor is not a registered, online handler on the old handler's host, runtime and directory. |
| `agent_caller` | The request carries an agent identity. |

Commit repeats the idle checks. A handler that became busy after prepare gets a
409, and the rotation stays prepared until a later attempt finds it idle.

"Finish an already-started atomic record" has no hub record. Its observable
signal is the activity monitor's exact-run state and pending tool. Activity
snapshots are transitions, so a handler that just started a turn can look idle
for up to the monitor's working window. The owner command treats an unobserved
(`unknown`) run as idle. The runner requires an observed `idle` or
`finished_silent` state.

## Handoff contents

`tt handler rotation get` shows the snapshot saved at commit:

- **reissued**: each old obligation and message seq, paired with its new
  obligation and message on the successor, and the old state and role
- **pendingScopeConfirmations**: queued team entries whose exact
  item/revision/order has no scope confirmation
- **liveLeases**: always empty; commit refuses otherwise
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
whichever limit is reached first.

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
a `tt-handler-*` session for that hub runs on the host. It then makes one
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
- Policy disabled: the project is not listed, so no rotation request is made.

## API

| Method and path | Purpose |
| --- | --- |
| `GET/PUT /v1/tasks/{id}/handler-rotation/policy` | Read, or save with `expectedRevision` |
| `POST /v1/tasks/{id}/handler-rotations` | `operation` `prepare`, `commit` or `abort`, keyed by `requestId` |
| `GET /v1/tasks/{id}/handler-rotations[/{rid}]` | List, or read one |
| `GET /v1/handler-rotations/due?host=H&templateDigest=D` | Runner due list |

Reusing a request ID with the same input returns the saved record without
writing anything. Reusing it with different input is refused.

## Rollback

The new columns (`tasks.primary_handler_id`, `tasks.handler_revision`) and
tables (`handler_rotation_policy`, `handler_rotations`,
`handler_rotation_requests`, `handler_runs`) stay in place. An older hub binary
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
