# Backlog steward

Feature `wi_5b4b94dbc9a11e8b`, work order #14942 (plan r2). One persistent agent
per project whose context is the backlog. It takes intake and turns it into
well-formed drafts the database handler files. It proposes batches, queue order,
ownership scopes and triage outcomes as owner decisions. It keeps a backlog
summary, and rotation hands that summary, not chat history, to a fresh session.

[Project roles](project-roles.md) shows where the steward sits beside the owner
session, the database handlers, the deployment agent and item teams.

## Provisioning

The steward is the `backlog_steward` role, one per project. Its template is
`PROJECT_ROLE_TEMPLATES.backlog_steward` in `client/team-examples.js`: GPT-6.1
Sol (`gpt-6.1-sol`, app `codex`) at `high` reasoning, because the work is research
and judgment (owner decision October 3, 2026; it ran Claude Opus 5.5 before). The exact prompt is in [Team examples](team-examples.md#project-roles).
The CLI reads the template through the embedded plan bundle
(`hub/internal/teamplan/plan.mjs`, action `project-role`).

```sh
# Print the template and its digest.
tt steward template [--json]
# Launch (or relaunch) the project's steward from the owner's shell.
tt steward setup --task tsk_ID --cwd /path/to/repo [--name backlog-steward] [--permission-mode MODE]
```

`setup` runs `tt spawn --role backlog_steward --agent-id agt_… --run codex
--model gpt-6.1-sol --reasoning high --prompt <template>` with the project's
current lifecycle generation. `tt spawn --role backlog_steward` also works
directly. Both are owner commands and refuse inside an agent session. The hub
admits a steward like the other persistent roles: a stable agent ID, no parent,
no work item, restart of an exited run by its expected run ID, and no restart by
name. It records the run's template digest (SHA-256 over the Go guidance, model,
reasoning and prompt) in `steward_runs`. The session is `tt-steward-<id>`.

### One steward per project

A partial unique index (`agents_one_backlog_steward`) and a check in the
admission transaction keep one slot holder per project. Starting, running,
done, retired and exited stewards all hold the slot. A second identity gets 409
`steward_active` naming the holder. Only a close or a committed rotation frees
the slot. A rotation successor is admitted as a pending steward
(`agents.steward_pending=1`) that does not hold the slot.

### Setup identity file

`setup` keeps `<relay state>/<hub hash>-<task>.steward.json` (mode 0600, no
credential) with the agent ID, name, directory and permission mode.

- No file: a fresh `agt_` ID, named `--name` or `backlog-steward`. The file is
  written before the hub is called.
- File present, hub answers 404 for the ID: the registration outcome was lost,
  so the same ID is retried.
- File present, agent not closed: the same identity is reused; `setup` is
  idempotent. An exited steward restarts with its current run as the expected
  run.
- File present, agent closed (a project pause, an owner close, the old side of a
  rotation): a fresh ID. The name is kept unless an open agent holds it, then
  `backlog-steward-N` with the lowest free N.
- A committed `tt steward rotate` rewrites the file to the successor.

### Closing

`tt close` refuses the steward: it remains available while the project is open;
rotate it instead. TailOS has no close action for agents. The owner can close it
with the hub route `DELETE /v1/tasks/{id}/agents/{agent}?runId=RUN` or by pausing
the project, which closes every open agent. The steward is never leased to a
queue entry, never an item-team member or item lead, and `tt team close` leaves
it open.

## Intake and follow-ups

`role:backlog_steward` resolves to the one active steward (not closed or exited,
not pending) in serial and parallel projects, with or without an item link.
With none, the send is refused with 409 `role:backlog_steward cannot be
resolved: no running agent holds it`.

- **Owner requests:** the owner session and helper chat send them with
  `tt send --to role:backlog_steward`.
- **Agents' follow-ups:** while a steward is active, ordinary agents' briefings
  say to send new bug/feature intake and follow-ups found outside their bound
  item to `role:backlog_steward`. Scope changes to their bound item stay with
  their handler.
- **Primary handler:** its briefing says to forward raw intake to the steward,
  record the steward's drafts as normal intake with the original owner message
  as `--source-seq` and a stable `--request-id`, and accept its refine and
  dismiss requests as ordinary updates. This line sits outside the handler
  template digest, so a steward's arrival never rotates live handlers.

The steward's procedure for each intake:

1. Read the source.
2. Find evidence or a reproduction; read-only repository inspection, no edits.
3. Name the likely files and ownership.
4. Write observable acceptance criteria.
5. Search `tt work-items list` and `tt work-items triage` for related and
   duplicate items.
6. Only when intent is unclear, ask the owner one question
   (`tt send --kind question --to owner`).
7. Send the primary handler one typed REQUEST with the draft in a body file,
   `--ref source=SEQ`, and related or duplicate items as `--related` refs and in
   the description. The handler creates the item; the steward never does.

### Held follow-ups and Discord-filed items

At each readiness pass the steward also refines items that reached the backlog
without research:

- **Review follow-ups.** Review convergence files each blocker it defers as an
  open item whose description starts `Review follow-up from <parent>, message
  #N. Held for triage.` `tt work-items triage` lists those still at revision 1
  under `heldForTriage`; a handler refinement raises the revision and takes the
  item off the list.
- **Discord `/bug` and `/feature`.** The Discord bridge files raw items directly
  from the owner's text: an intake message (source kind `discord`) and an item
  created by node `discord-bridge` with that message as its source. The steward
  finds them with `tt work-items list --status open --json` (`createdBy.node` is
  `discord-bridge` and `revision` is 1). No hub endpoint was added for them.

For each one, the steward researches it and sends the handler either a refine
REQUEST (title, description with evidence and criteria, related and duplicate
links) or a dismissal proposal: a `tt ask` decision when it needs owner
judgment, or a handler REQUEST citing triage's duplicate or already-delivered
evidence.

`tt work-items triage` admits the exact run of the active steward, besides the
owner and database handlers. A retired or stale steward run, a pending successor
and ordinary agents are refused.

## Proposals

Batches of related small items, queue order, ownership scopes and triage
outcomes are owner decisions. The steward proposes each as one `tt ask` decision
(category `decision`) with work-item refs, options and a recommendation. An open
delegation window routes it to the delegate, whose answer reaches the steward as
a directed message. A window whose delegate is the steward never routes the
steward's own decision.

The steward applies nothing itself. After an answer it sends a REQUEST citing
the decision:

- to the handler, for records, scope confirmation (`tt work-items scope confirm
  --owns`) and dismissals;
- to the owner, or the delegate of an open window, for queue reorder. Item leads
  never reorder the project queue.

For an approved batch, the steward records it in the summary and asks the
handler for one shared work-order message naming every item and the combined
ownership. Until multi-item queue entries exist (`wi_faeef2716575d0eb`), a batch
is queued as adjacent entries.

## Backlog summary

The summary is the steward's durable state: an append-only list of revisions in
`backlog_summaries`. The prompt asks for sections Themes, Open questions,
Batches, Pending proposals and Held follow-ups, saved after each meaningful
change.

```sh
tt steward summary get [--revision N] [--json]
tt steward summary set --revision N --body-file F --request-id KEY   # N is the current revision, 0 for the first
tt steward summary history [--json]
```

- A body is at most 64 KiB (`api.MaxBacklogSummaryLen`) of UTF-8.
- Only the exact run of the active steward, or the owner, saves. A stale expected
  revision is 409. A replayed request ID with the same input returns the saved
  row; with different input it is 409.
- Any caller reads the latest revision, a named revision and the list.
- Every steward briefing names the latest revision and says to read it first.
  After a pause and resume, the next `tt steward setup` admits a fresh identity
  whose briefing names the same revision.

## Rotation

Rotation replaces the steward with a fresh session that has the current template.
It is two-phase, like [handler rotation](handler-rotation.md), and separate from
it.

```sh
tt steward rotate --task tsk_ID [--reason manual|tokens|template]   # on the steward's host
tt steward rotate --task tsk_ID --abort
tt steward rotation list|get ID [--json]
tt steward policy get|set --task tsk_ID [--revision N --enabled=BOOL --max-total-tokens N --on-template-change=BOOL]
```

1. **Prepare** (owner or host runner; an agent identity gets 409
   `agent_caller`). The named run must be the active steward (`not_steward`),
   idle under the handler rules (the owner treats an unobserved run as idle; the
   runner needs an observed `idle` or `finished_silent`, and no pending tool:
   `working`, `pending_tool`), with a saved summary (`summary_missing`) and no
   other open rotation (`rotation_open`). Prepare snapshots the latest summary
   revision and digest and preallocates the successor `<base>-r<N>`.
2. **Launch.** The host runs `tt spawn --role backlog_steward
   --steward-successor --agent-id <successor>` with the current template and the
   old steward's host, runtime, directory and permission mode. The hub admits
   that exact ID as a pending steward. It holds no slot, does not receive
   `role:backlog_steward`, and cannot run triage. Its briefing says to read the
   summary revision and the handoff first and to take no intake until the
   handoff notice arrives.
3. **Commit** (keyed, one transaction). It re-checks that the old steward is
   idle and that the successor is online, pending, and on the same host, runtime
   and directory (`successor_unavailable`). It closes the old steward, then
   makes the successor the holder, so there are never two holders and never
   none. It re-issues every open obligation of the old steward to the successor
   and saves the handoff: summary revision and digest, whether the old run saved
   it, moved obligations, and decisions the old steward proposed that are still
   unanswered. It sends the successor one directed notice naming the rotation
   and the summary revision, and saves the receipt.
4. **Cleanup.** The host stops the old steward's owned session, writes its
   cleanup receipt and rewrites the setup identity file.

`tt steward rotate` keeps a journal
(`<relay state>/steward-rotation-<key>.json`: preparing, prepared, spawned,
committed, cleaned). A rerun resumes from it. It never launches a second
successor, never re-issues twice and never cleans up twice.

**Launch failure.** If the successor spawn fails or it never comes online within
two minutes, the rotation stays prepared and the old steward stays the one live
steward, keeping the role and its obligations. Commit refuses
`successor_unavailable`. `tt steward rotate --abort` closes a registered pending
successor and stops its session. A new rotation then gets a new successor ID.
**Stale rotation.** A prepared rotation whose old steward has closed (a project
pause, an owner close) can never commit, and it holds nothing open:

- It never blocks a fresh setup. The hub refuses to admit its successor ID with
  409 `rotation_stale`, so the successor can't take the slot while waiting for a
  handoff that won't come.
- Rerunning `tt steward rotate` on the host whose journal names it aborts the
  rotation, cleans up a registered successor's session and removes the journal.
  It then reports the abort and says to run `tt steward setup`, which admits one
  fresh steward.
- Otherwise the next prepare aborts it.

Recovery after a pause interrupted a rotation: resume the project, rerun
`tt steward rotate --task ID` (or `--abort`) to clear the stale rotation, then
`tt steward setup --task ID`.

**Policy and runner.** `steward_rotation_policy` holds `enabled`,
`max_total_tokens` (default 300M, as for handlers) and `on_template_change`
(default on). There is no item limit. Rotation is enabled for projects created
after the migration and disabled for projects that existed before it. The relay
runs `relayStewardRotationTick` next to the handler tick. It asks the hub nothing
unless a `tt-steward-*` session for that hub runs on the host. Otherwise it makes
at most one `GET /v1/steward-rotations/due?host=H&templateDigest=D` a minute and
caches an empty answer for five minutes. A due, idle steward is rotated with
trigger `runner`; a journal on the host is resumed.

## Boundaries

The steward does no per-item records, merges, releases or acceptance, and never
adds, reorders or removes queue entries. The owner session remains the owner's
conversation partner and relays owner decisions.

The steward reads work items, their history, the queue and triage directly
(owner decision #15466). Every write goes through the database handler. The hub
refuses these with 409 `steward_write` ("the backlog steward files through the
database handler") when the declared agent is the steward:

- work-item create, update, keyed update and dispatch;
- message-audit correction and resolution.

Scope confirmation, verification plans and receipts, release enqueue and
message-audit associations already require the exact handler run, so they refuse
the steward too. Plain messages and reads are unaffected.

These refusals are an audit guard for tt-CLI writes, not access control. The
steward runs under the owner's credential, and a call that declares no agent
identity is not checked, which matches the existing handler rule. Queue add,
reorder and remove requests carry no caller identity, so the hub cannot tell the
steward from the owner there; the prompt forbids it.

## Capacity

- The steward is 1 of the 32 open agents a project may have.
- In host admission it costs 1 session and 1 relay binding, 80 requests a
  minute and a burst of 6. That counts against the host policy's
  `MaxSessions`, `MaxRelayBindings`, `MaxRequestsPerMinute` and `MaxBurst`
  (less headroom). Read the Mini's current host policy with
  `tt team queue list --task tsk_ID`.
- A rotation briefly adds a second session and binding (the pending successor)
  until the old session is cleaned up.

## Known limits

- **Running handlers keep their launch briefing.** A handler already running
  when the steward is first set up learns about it only after its next rotation
  or restart. When you first set up a steward, send the running primary handler
  a one-line NOTICE: "The backlog steward is NAME; forward raw intake to
  role:backlog_steward and record its drafts with the owner's source seq."
- **Multi-item queue entries** belong to `wi_faeef2716575d0eb`.
- **TailOS** does not yet provision, rotate or display the steward or its summary,
  and counts it as a team member.

## API

| Route | Purpose |
|---|---|
| `GET /v1/tasks/{id}/backlog-steward` | Active steward, slot holder, pending successor, latest summary revision |
| `GET /v1/tasks/{id}/backlog-summary[?revision=N]` | Latest or named summary |
| `GET /v1/tasks/{id}/backlog-summary/revisions` | Revision list without bodies |
| `POST /v1/tasks/{id}/backlog-summary` | Save the next revision (body up to 6 × 64 KiB + 4 KiB, so escaped characters fit) |
| `GET/PUT /v1/tasks/{id}/steward-rotation/policy` | Rotation policy |
| `GET/POST /v1/tasks/{id}/steward-rotations` | List; prepare, commit or abort |
| `GET /v1/tasks/{id}/steward-rotations/{rid}` | One rotation with handoff and receipt |
| `GET /v1/steward-rotations/due?host=H&templateDigest=D` | Runner's due list |

Work-item triage gains `heldForTriage`; the existing fields are unchanged.

## Storage

- `agents.steward_pending` and the partial unique index
  `agents_one_backlog_steward`
- `steward_runs` (template digest per run)
- `backlog_summaries` (append-only; unique request ID per project)
- `steward_rotation_policy`, `steward_rotations` (one prepared per project),
  `steward_rotation_requests`

All are additive migrations in `migrateBacklogSteward`.

## Rollback

Revert the feature's commits and redeploy the hub and CLI. The added tables and
column are additive and ignored by older code. A steward agent row left open under
an older hub is an unknown role there; close it (`DELETE
/v1/tasks/{id}/agents/{agent}`) before rolling back, and stop its
`tt-steward-*` session. Summaries stay in the database for a later redeploy.

## Verification

```sh
cd hub
go test ./internal/store -run 'Steward|BacklogSummary|TriageSteward|TriageHeldForTriage'
go test ./internal/server -run 'Steward|BacklogSummary|TriageSteward'
go test ./cmd/tt -run 'Steward|TriageHeldForTriage'
cd ..
node --test tests/backlog-steward.test.js tests/team-examples.test.js tests/team-launch-plan.test.js
node scripts/build-team-plan.mjs --check
```

The CLI tests use a temporary hub, a private tmux socket, a temporary `HOME` and
relay state, and a fake `tt` in the steward's tmux session.
