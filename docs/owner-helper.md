# Owner helper

Feature `wi_8d912169e30db643` revision 1 (scope revision 2), work order #15013,
lead ASSIGN #15073, plan `.build/plans/wi_8d912169-plan.md` r2. Base `tasks-hub`
at `e9504a7`, after owner delegation windows (`ee47773`).

## What it is

The owner's own Claude Code or Codex session (the owner's out-of-band helper) can join a
project as that project's one **owner helper**: an agent with role
`owner_helper`. Being an agent lets it:

- be a [delegation window](owner-delegation-windows.md) delegate, and answer the
  owner's routed requests with a rationale;
- be woken by the host relay through native `codex queue --thread` for Codex,
  or the existing safe transcript/pane wake for Claude Code.

It is not a team member. It is never leased to a queue entry, never counted as a
team member or database handler, and never closed with an item team. The
owner can also talk with it from Discord (see below).

When the project has a [backlog steward](backlog-steward.md), the owner session
sends new bug and feature intake to it with `tt send --to role:backlog_steward`
instead of filing items itself. The steward researches the request, drafts the
item and has the database handler file it with the owner's source message; it
proposes batches, queue order, scopes and triage outcomes as `tt ask` decisions.
Confirming those decisions, including triage outcomes, stays the owner's; the
owner session relays them. See [Project roles](project-roles.md).

## Register

Run this in the owner's Claude Code or Codex session, in a dedicated one-pane tmux session:

```sh
tt helper register --task tsk_...            # default agent name: owner-helper
tt helper register --task tsk_... --name N   # another name (first registration only)
```

It checks, before writing anything:

- Claude Code supplies `CLAUDECODE` and its real `CLAUDE_CODE_SESSION_ID`, with
  the transcript `~/.claude/projects/*/<session id>.jsonl` present; or Codex
  supplies its real `CODEX_THREAD_ID`, with no Claude identity variables. Mixed
  identities are refused. Codex registration resolves the installed `codex`
  executable and optional `CODEX_HOME` to absolute paths before writing state;
- it is not an agent session, unless that session carries this project's helper
  identity (a pane of the helper's own tmux session);
- the tmux session name is a valid agent session name (rename it if not);
- the tmux session is not already another project's live owner helper (see
  [One project per tmux session](#one-project-per-tmux-session));
- the project is active: registration is refused (409) while the project is paused,
  cleaning up for a pause or resuming. Register again after resume.

The hub (`POST /v1/tasks/{id}/owner-helper`, owner only) then, in one transaction:

| Project's helper now      | Result                                            |
| ------------------------- | ------------------------------------------------- |
| none, or only closed ones | a new agent (`created`)                           |
| exited                    | the same agent with a new run (`reattached`)      |
| live (any open status)    | the same agent with its run replaced (`replaced`) |

A retired helper keeps `retired` when its run is replaced; only `tt resume` undoes
retirement. The helper's name is fixed while it is open: another `--name` is refused
until the helper is closed. A name held by any other open agent is refused.

Every registration writes a receipt (`ohr_…`: agent, run, previous run, mode, host,
session, runtime, cwd, request ID, who, when) and an `agent_added` event. A new
helper's read cursor starts at the latest message, so earlier Board history is not
unread input for it. At most one helper per project is open (not closed or exited):
a partial unique index and the transaction check both enforce it.

Then, on this host, `tt helper register`:

1. verifies the returned agent is the live `owner_helper` whose current run is the
   returned run, and otherwise writes nothing;
2. tags the tmux session in **one** tmux command, `TAILTERM_ROLE=owner_helper`
   first, then `TAILTERM_HUB`, `TAILTERM_TASK`, `TAILTERM_AGENT`, `TAILTERM_RUN`;
3. unsets those tags on the previous helper session when it registered from another
   tmux session, only if that exact session (ID and creation time) still names this
   helper;
4. writes the relay's wake binding, whose thread is the **real** runtime session/thread ID
   (not a derived or spoofed Claude ID), with runtime `claude` or `codex` and
   role `owner_helper`. Codex bind/auto-bind preserves this registration-owned
   binding and refuses a foreign thread or stale run;
5. writes a private helper file (`<relay state>/<hub hash>-<task>.owner-helper.json`,
   0600, no token).

Outside tmux it registers with session `terminal`, writes no binding and prints
_registered without wake_: the relay cannot wake that session and it shows offline.

**Retries.** Without `--request-id`, each registration gets a new request ID. It is
saved before the call; if the outcome is unknown (timeout, lost reply), running the
command again from the same runtime/thread replays that same ID, so the hub
returns the original result instead of registering twice. Pending retries include
the exact runtime/thread; an explicit key cannot move the saved registration to
another thread. A definite refusal or a verified result clears it. So a
deliberate registration after a confirmed one is always a new action: after a close,
a fresh agent; after an exit, a new run; while live, a replaced run. An explicit
`--request-id` keeps plain replay semantics.

**Re-register after a Claude restart or `/clear`**: both change the Claude session
ID, and the old binding's transcript no longer moves. The wake then fails closed
(transcript unavailable) until you register again.

**What the session was holding** (its scheduled wakes, standing instructions and
orders in flight) can be kept in a host-local record that a new session is told
about at start: see [session-handoff.md](session-handoff.md). It is off until the
host turns it on, and in this version it registers nothing and restores nothing:
the new session still runs `tt helper register` and re-creates its wakes by hand.

## Hand off between Claude Code and Codex

Feature `wi_f771e8367facf50f`, order #22120, builder assignment #22213 adds
both-runtime support. Deployment requires a separately ordered coordinated hub,
Mini `tt`, and relay update; source changes alone do not update a running relay.

1. Open the destination runtime in its own dedicated one-pane tmux session.
   Use the real runtime-provided identity: `CODEX_THREAD_ID` for Codex or
   `CLAUDE_CODE_SESSION_ID`/`CLAUDECODE` for Claude. Do not copy the old runtime's
   variables or manufacture a Claude identity for Codex.
2. In that destination session, run `tt helper register --task tsk_...` with
   a fresh request key (or omit `--request-id`). Registration replaces the same
   open helper's run and keeps one active helper. Tags on the previous local
   session are cleared only after exact ownership verification.
3. Run `tt helper inbox --task tsk_...` in the destination session. The relay
   follows its newly registered exact thread/run; the previous run cannot wake,
   heartbeat or reply by adopting the host's newer helper file.
4. To hand back, repeat these steps in the other runtime. Re-register after a
   Codex restart/new thread as well as after a Claude restart or `/clear`.

With the session handoff record switched on ([session-handoff.md](session-handoff.md)),
run `tt handoff write --task tsk_... --out PATH` in the outgoing session before
step 1, and `tt handoff show --task tsk_...` in the destination after step 2. The
record is not proof of registration; step 2 is.

Codex uses its registered executable and `CODEX_HOME` for native queue delivery.
Both inbox and broker wakes name `tt helper inbox --task T`. Outside tmux either
runtime still registers without automatic wake and shows offline. Existing
retirement, exact pane/process, cleanup and one-project-per-session rules apply.

## Acting as the helper

The runtime's shell environment may not persist, and the owner's session stays the
owner for ordinary `tt` commands. To act as the helper for one command:

```sh
eval "$(tt helper env --task tsk_...)" && tt owner answer OBLIGATION_ID --text "..." --rationale "..."
tt helper inbox --task tsk_...     # the helper's unread messages, marked read
```

`tt helper env` prints `export TAILTERM_HUB=… TAILTERM_TASK=… TAILTERM_AGENT=…
TAILTERM_RUN=… TAILTERM_AGENT_NAME=…` after checking that the hub's helper is live
and its current run is the one registered on this host. It never prints the token;
the owner's own configuration supplies it. Both commands refuse when the helper was
registered again elsewhere, closed or exited, or when a caller supplies a
stale run or a different runtime/thread.

## Talking from Discord

With the bridge's `DISCORD_HELPER_TASK` set to this project, the owner's plain
messages in their DM with the bot, or in the helper channel, reach the helper as
directed owner messages. `tt helper reply --task tsk_... SEQ --text "..."` sends
the answer back to the same DM or thread. `/status` and `/digest` work there from
hub data even while the helper is offline. Reply to, or `tt ack`, each Discord
message: each one is an obligation. See [Discord helper chat](discord-helper-chat.md).

## Opening a window for the helper

```sh
tt owner delegation open --delegate owner-helper --for 3h --scope decisions
tt owner delegation open --delegate owner-helper --until 2026-09-30T18:00:00-07:00 --scope decisions-merges-deploys
```

TailOS's _Delegate decisions…_ strip and Discord `/delegate` name the same agent.
Routed owner requests and `tt ask` decisions reach the helper as directed NOTICEs.
It answers as described in [owner delegation windows](owner-delegation-windows.md):

```sh
eval "$(tt helper env --task tsk_...)" && tt owner answer OBLIGATION_ID --text "..." --rationale "..."
eval "$(tt helper env --task tsk_...)" && tt ask answer SEQ --option ID --rationale "..."
```

The window follows the agent, not the run: re-registering keeps the window and the
inbox, and the replaced run is refused (403) like any stale run.

**Matrix approvals stay with the owner.** Any request mentioning
`verification-matrix-approval:` is never routed to the helper, and a helper answer
or rationale containing the token is refused. A delegated answer is agent-authored,
so it can never satisfy the verification plan's owner approval binding.

## Where the helper's pane opens

In TailOS the helper's pane always opens in the pinned **Home** area, outside the
agent groups (see [project overview](project-overview.md)). It
is identified by its hub role, `owner_helper`, never by name. Home is not counted
by Panes per group, and the helper is not a project member, so it never takes a
project slot or creates a "(continued)" group. It cannot be dragged out of Home.

On each helper attach, TailOS verifies the exact tmux session ID and creation
time plus its task, agent, run and `owner_helper` tags, resolves the selected
window's stable ID, and sets only that window to `window-size latest`. It refuses
the policy if the identity differs or the window is linked to another session.
It does not change global window sizing or the ordinary-agent sizing controller.

The pane then attaches with `attach-session -f ignore-size` on tmux 3.2+.
When TailOS is the only attached client, the helper window follows that pane's
PTY dimensions. When the owner's normal terminal is attached too, tmux excludes
TailOS from sizing and uses the owner's terminal dimensions. Owner attach/detach
transitions reconcile natively, without a browser command or client-count poll.
A historical manual 200×50 window is corrected on attach, including when the
owner is present; its resulting dimensions come from the owner, not TailOS.
Program rows exclude tmux's status rows: a 137×24 client with one status row
produces a 137×23 program window. The helper's native policy has no 80×24 floor.

Reconnect reapplies the guarded policy and keeps the binding and size flag. If
the launcher opened the helper before the hub reported it, adoption moves that
tab into Home and replaces its plain attach with the guarded helper attach,
whether still connecting or connected. It keeps the tab ID and attaches to the
existing session without creating one. A changed server endpoint blocks helper
replacement until the original profile is restored. Hidden replacements keep
the previous PTY dimensions; visible panes send their fitted dimensions.

The private-socket regression executes these transitions on tmux 3.7b. Older
than tmux 3.2, the existing compatibility fallback attaches plainly; owner
precedence through `ignore-size` is unavailable and is not guaranteed there.

A workspace saved before Home kept the helper inside its project group; the login
restore moves it into Home once the hub roster is read (if the hub is unreachable,
at the next adoption or reload).

## Wake and offline

The relay must run on the owner's host (`tt relay`). For the helper it:

- heartbeats at most every 30 seconds, only while the exact tmux pane, its session
  tags and one process of the registered runtime under that pane all verify. With the session gone
  there is no heartbeat, the helper is offline within 90 seconds, and the relay's
  usual _agent offline_ skip applies;
- uses native queue delivery to the registered exact Codex thread, with the
  helper inbox command in both inbox and broker prompts;
- for Claude, wakes it only when that tmux session has **exactly one pane** (otherwise the wake
  is refused as _ambiguous owned runtime pane_ and nothing is typed), with
  `Tailterm messages N. Run tt helper inbox --task T.` or
  `Tailterm obligations N. Run tt helper inbox --task T. Wake J.`;
- never rebinds it from the tmux tags, never resizes its windows, never stops its
  session, and never reports it `crashed` or pane-size `stuck`: a missing session is
  activity `unknown`, _owner session offline_, and posts no alert.

`tt agents` shows `role=owner_helper activity=offline`; the TailOS roster shows
_· Owner helper_ and _Offline_ in place of the activity label.

The relay wakes the helper for the same messages as any other agent (directed
messages, broadcasts and unaddressed human posts), so a busy Board wakes it often.

## Keep the helper session dedicated

New windows and panes in the tagged tmux session inherit the helper's identity:
owner-only commands there refuse, and `tt post` there is authored as the helper.
A second pane also stops wake-ups. Keep one pane in that session; re-registering
from another session moves the tags.

### One project per tmux session

A tmux session carries one project's helper. Its tags name one task, agent and run,
so tagging it for a second project would stop the first project's wake-ups and
heartbeat and leave that helper offline with no message. To be the helper of
several projects, register each from its own tmux session.

`tt helper register` reads the session's current tags first. When they name the
owner helper of another project (or another hub) that is still live, it refuses
before registering with the hub or writing tags, state or a binding:

```text
tmux session owner is the owner helper of project tsk_…; one project per tmux
session: register from another tmux session, or pass --take-session to move this
one (that project's helper wake stops)
```

`--take-session` registers anyway and warns on stderr that the other project's
wake-ups stop; that helper shows offline until it registers from another tmux
session. The check does not apply when the tags name this project (a re-register),
or a helper that is closed, exited, gone from the hub or registered again elsewhere.
A helper on another hub, or one whose lookup fails, counts as live.

## Queue chores: product vs owner helper

Feature `wi_de078c0ecd9c846b` (order #14809) moved the recurring queue chores the
owner session did by hand on 2026-09-28/29 (#14646–#14798) into the product. See
[team launch](team-launch.md#queue-chores-the-product-handles) for the details.

| Chore the helper did by hand | Now |
|---|---|
| Scoping each queued entry with `tt team queue scope --owns …` (5+ times) | Automatic: the handler records `--owns` at `tt work-items scope confirm`, and `tt team queue add` takes it. An entry with no ownership must be `--serial`. |
| Narrowing a finished team's coarse ownership so queued work could start | Automatic: the runner narrows an accepted entry to the files its candidate changed. |
| Freeing a released item's slot, handler lease and ownership while post-release checks kept it open, which needed item dismissal or owner team-close surgery | Supported command: `tt team queue integrated --entry E --commit SHA`. The item, its team and its records stay; the runner closes, cleans and finishes the entry once the item is terminal. |
| Noticing that queued work was stuck behind a failed, dead or idle entry, or a missing handler, and working out the fix | Automatic: the queue list explains the stall and its fix command, and the runner posts one Board NOTICE per stall. |
| Answering owner escalations for obligations blocked on a fix that was already queued | Automatic: the broker holds the owner escalation while the fix is queued, launching or running. |
| Scanning the backlog for duplicates, already-released items and stale items | Supported command: `tt work-items triage [--release-record FILE]` lists suggestions only; nothing changes until the owner confirms. |

Still the owner session's:

- owner decisions, and relaying the owner's words verbatim;
- diagnosis of unexpected behavior, and judgment calls the product cannot make;
- releases, until the deployment agent (`wi_93629e4ce61658cb`) is activated,
  including running `tt team queue integrated` for a candidate the owner
  released;
- verification matrix approvals (never delegated);
- disk cleanup, until closeout cleanup (`wi_b39698a5238560d3`) ships;
- confirming or rejecting triage suggestions.

After `integrated` an entry no longer protects its files. A fix found by the
post-release checks is filed as a new item and queued; the still-live team does
not make it.

## Limits and accounting

- The helper counts toward the project's open-agent cap (`MaxAgents`, 32) and the
  host capacity check, like any open agent.
- A project pause includes the helper as a pause target with a service disposition,
  like every open agent. It owes no host cleanup (`cleanup_done=1`), so it never
  holds a pause in cleanup. Pause closes it; register again after resume, which
  creates a fresh helper.
- Closing it (`tt close owner-helper` or TailOS) stops nothing on the host.
- Routing to the helper still depends on the requester's declared category, exactly
  as for any delegate.

## Storage

Additive: table `owner_helper_registrations` and the partial unique index
`agents_one_owner_helper` on `agents(task_id)` where `role='owner_helper'` and the
agent is not closed or exited. Both are created after the `agents.role` column
exists (chained from the activity migration). Older binaries ignore them. Only the
register route creates the role: `tt spawn --role` and agent admission refuse it,
and the Discord bridge credential cannot call the route.

## Verification

```sh
cd hub
go test ./internal/store ./internal/server -run 'OwnerHelper' -count=1
go test ./cmd/tt -run 'Helper|OwnerHelper|WindowSizeSkipsOwnerHelper|CleanupSkipsOwnerHelper|RetryClaudeBindingSkipsOwnerHelper' -count=1
cd ..
node --test tests/owner-helper.test.js
```

All tests use temporary hubs and databases, private tmux sockets (`TT_TMUX_SOCKET`),
a temporary `HOME` with synthetic transcripts and fake runtime processes.
Codex fixtures also use exported `Bridge.Run`/`Dispatch`, temporary bridge state,
fake Discord REST and native queue argv to verify DM/thread replies and retries.
Both runtime handback sequences exercise stale CLI/run/thread refusals. None
touches the live hub or the owner's session.
