# Project overview

Updated September 8, 2026. Start here for the architecture and product model;
use [the handoff guide](handoff.md) for the actual deployment and migration.

## Product and direction

Tailterm is a browser terminal workspace that makes multiple Tailscale-connected
servers feel like one place. SSH connections and the Tailscale client run in the
browser through Go WebAssembly; xterm.js renders terminals. Remote tmux keeps
sessions alive independently of a page or SSH connection.

The current development branch adds coordinated AI-agent teams, projects, scoped
Bugs and Features, shared messages, and encrypted cross-device profiles. It is published at
**https://tailos.tailarr.com**. The original **https://tailterm.tailarr.com** is a
separate, older release and must not be overwritten by this work.

The longer-term TailOS idea is a unified desktop across a tailnet, with a compact,
Omarchy-inspired appearance and eventually cross-machine file operations. That
is a direction, not a description of a finished operating system. The current
product is the terminal and agent-coordination workspace. Files is intentionally
hidden while its interface is reconsidered; a WASM IDE and desktop app platform
are not implemented.

## Architecture

```text
HTTPS static site (Cloudflare Pages project: tailos)
  └─ Browser
      ├─ UI: terminal groups, Projects, Board, Teams, Bugs, Features, settings
      ├─ encrypted local vault and optional encrypted profile sync
      ├─ xterm.js + WebGL / optional DOM rendering
      └─ Go WASM: Tailscale transport, SSH, SFTP
          ├─ remote SSH account → tmux sessions → agent CLI processes
          └─ existing private TrueNAS address → coordination hub → SQLite

Each agent host
  ├─ tt: spawn, briefing, inbox, messages, events, retirement, cleanup
  ├─ tt wrap: process lifecycle and heartbeats
  ├─ tt relay: native Codex inbox wake-up and closed-task cleanup
  └─ locally installed agent runtimes and their own credentials/configuration
```

The static web host does not hold SSH credentials or proxy terminal traffic.
The hub stores shared coordination data and encrypted profile envelopes; it does
not execute arbitrary commands on agent machines. Browser actions launch remote
agents through SSH. An agent's `tt spawn` starts helpers locally on its own host.
There is no distributed job scheduler, automatic repository distribution, or
cross-machine git merge service.

The repository also retains an older optional Node.js SSH gateway under
`server/`. `npm run dev`, `npm run build`, and `npm start` relate to that mode.
The deployed frontend uses `npm run build:static` instead.

## Main concepts and behavior

### Connections, sessions, tabs, and groups

- Tailscale sign-in displays a locally generated QR code for phone login and
  keeps the original browser login link available. The sign-in dialog closes
  when the browser node connects; pending discovery can continue in its place.
  The phone authorizes this browser node, while vault and SSH login stay separate.
- A saved server describes SSH access to a tailnet destination.
- A tmux session lives on a remote machine. Browser panes attach to it.
- Every main terminal tab is a group, even if it contains one session.
- Ordinary groups can be rearranged and combined; panes can be resized.
- Ordinary pane dragging swaps positions. Holding physical Left Option places
  the dragged pane to the target's right; Right Option places it above. The
  preview follows modifier changes while preserving project-group boundaries.
- Each task owns a dedicated group named after the task. Agent registration
  causes its pane to appear in that group, including agents spawned later.
- Task groups cannot merge into another task or ordinary group. An ordinary
  session can join a task group visually as a guest without becoming an agent.
- Default task layouts keep the orchestrator full-height on the left and add
  workers across two columns on the right, stacking the fourth/fifth agents
  beneath the second/third. Later workers extend those columns. Manual divider
  resizing, swaps and guest layouts are retained; narrow screens still stack.
- Closing or hiding a browser pane is distinct from terminating a remote session.
  Closing the page disconnects SSH but leaves ordinary remote tmux sessions alive.
- Closing a task explicitly terminates its owned agent sessions. Guest sessions
  survive and return to an ordinary group.

Server matching includes aliases learned from agent launch responses and a
bounded `hostname -s` probe. This matters because Mini reports `Stephens-Mini`,
which differs from some saved Tailscale/DNS names. Do not simplify matching back
to literal hostname equality.

Terminal layouts and bound project groups are local to each browser origin.
After restoring a profile on another origin, use Board → project → Terminals
or Projects → More → Open terminals to attach the existing agents. A same-origin
saved project binding is restored even if its previous workspace had zero panes.
Project layouts also retain their split tree, divider proportions, pane order and
focused agent in the encrypted local workspace. Templates use project and agent
IDs, so changed browser pane IDs and out-of-order reconnects preserve the layout.
Temporary missing/hidden agents retain their positions; a confirmed hub roster
removes deleted agents, while new agents extend a customized layout. Closing or
forgetting a project clears its template. This remains local to the browser origin.

### Projects, Bugs, and Features

Projects is the UI name for the existing task hub; task IDs, routes, CLI commands,
and environment variables remain compatible. Bugs and Features are durable,
project-owned records with status, priority, revision checks, and retry receipts.
Both tabs can filter by project or show all projects. Closed projects stay readable.
Projects, Teams, Bugs and Features reuse Board's full-height split layout and
boxed left selectors. Projects and Teams show one selected record's details and
actions on the right; closed projects remain selectable for history and cleanup.
Bugs/Features retain All projects and status filters. Narrow screens use Board's
horizontal selector rail.

Previously visited hub views use an encrypted local read cache and refresh in
the background, including small incremental Board message reads. The cache is
bounded to 100 entries/4 MiB/seven days and isolated by hub credential. Saved data
remains readable while the hub is offline; hub mutations are never queued.
Terminal lifecycle operations continue using live authoritative reads.

Send to project saves a directed board message to the selected open project's
orchestrator. It defaults to the owning project; choosing another does not move
the record or start a team. The receipt confirms message storage, not execution.

Browser-created projects launch a database handler after the orchestrator. The
handler inherits its resolved launch settings and owns all agent work-item database
reads/writes. Agents require a durable bug/feature and a bounded work order before
implementation, investigation, validation or deployment. Intake/coordination may
establish that record first. Missing/unavailable handlers are explicit dependencies,
not permission for direct agent CLI/API access. Human UI access remains available;
this instruction/audit workflow does not add API authorization enforcement.
Existing and headless-created projects offer explicit handler setup because the
hub does not launch remote processes. Encrypted local launch plans and verified
host receipts make retries safe; ambiguous interrupted launches require inspection.
See [Projects, Bugs, and Features](project-work-items.md) for recovery and dispatch.

### Teams and launching

Teams are reusable templates. They supersede older saved launch profiles; a team
of one covers the original single-agent setup. Templates contain up to 32 members
with names, runtime, model, role, instructions, optional machine/folder overrides,
command overrides, and permissions/tool settings. Editing a team changes future
launches, not running agents.

New-project launches reserve one of the 32 active-agent slots for the database
handler, allowing at most 31 ordinary template members for that launch.

A member without an assigned machine uses the **Main machine** selected for that
launch. A member without a folder override uses the explicit project folder
chosen for its machine in the launch dialog. Folder selection uses a read-only
SFTP browser. Paths do not silently transfer between different machines. Helpers
inherit their parent's launch folder unless an explicit `--cwd` is supplied.

Partial launch failure retains the task and completed launches. Retrying launches
only remaining members; their folder selections can be corrected. Existing agent
names are not silently duplicated. These behaviors fixed real failures and need
to remain covered by browser checks.

Models can be selected for supported runtimes or entered explicitly. Installed
runtime versions, account/model access, credentials, and host configuration remain
host responsibilities. The bundled model suggestions and nine researched team
examples are starting points, not provider availability guarantees. See
[model selection](agent-models.md) and [team examples](team-examples.md).

### Orchestrators, helpers, and swarm mode

A team can name a main orchestrator. It launches first, and new workers/helpers
are instructed to introduce themselves to it. Workers report substantive results
and blockers on the board. The orchestrator decides, plans, routes work and reviews
evidence; builders own implementation, including shared schemas/types and
integration code. The sole implementation exception requires both that the
orchestrator is the only non-database team member and that agent spawning is off.
Cost, capacity, worker availability or helper quota does not create an exception.
These are generated role instructions, not a runtime sandbox or API authorization
boundary. An implementation worker session belongs to exactly one bug or feature.
After accepting its result and resolving its dependencies, the orchestrator closes
that worker session after handing off or detaching and reverifying any useful
long-lived descendant services; a new item gets a fresh identity and context. Retirement is
only for intentional temporary retention of the same item context. The
orchestrator and active database handler stay available while the project remains
open.

**Allow agents to add other agents** controls agent-originated helper launches.
**Max new agents** is a separate lifetime allowance for additional identities,
including descendants and finished helpers. It defaults to two. Manual additions
do not consume it; the deployed hub's per-project active-agent cap is 32. Retired
agents still occupy open-agent slots because their sessions and identities remain;
verified individual closeout releases the slot without closing the parent project.

**Enable swarm** broadcasts new messages to all members while preserving an
addressed recipient as the person responsible for acting. Delivery scope is saved
per message; toggling the setting does not rewrite older messages. Instructions
explicitly discourage duplicate work, acknowledgements of acknowledgements, and
reply loops. The starter swarm uses Astra with four Terra workers; ten workers
is not a mandatory default or evidence of better performance.

### Messages, wake-up, and lifecycle

The hub supports optional primary work-item and recorded order-message references,
plus request-key posting receipts. Exact retries recover the original committed
message without repeating events or agent resumption. Human cross-project work-item
dispatch includes its source-owned link. Existing clients may still post unlinked
messages; UI/CLI controls and strict enforcement are later slices. See
[message audit foundation](message-audit.md).

The hub durably stores shared messages. `tt inbox --unread --mark-read` retrieves
messages; the read cursor means retrieval, not completion. `tt post --to NAME`
addresses an agent. To reply to the human owner, use `--reply-to SEQ` without
`--to owner`; humans are not agent roster entries.

A prompt telling an agent to check its inbox cannot wake an idle agent. The host
relay handles this for compatible Codex installations: the first task-aware `tt`
command inside a Codex thread binds the exact thread UUID and run ID, and the
relay uses native `codex queue` to notify that thread. It does not inject text
into tmux or mark messages read. Delivery is bounded to eight wake attempts per
agent per five minutes and persists progress across relay restarts.

Ordinary directed messages and human announcements can wake Codex. In swarm mode,
broadcast messages can also wake peers. Retired, closed, exited, offline, or stale
runs are excluded. A direct human message resumes its explicitly addressed retired
agent if its current session is online, allowing the existing eligible run to
receive the message; broadcasts and agent-authored messages do not resume it.
Other runtimes use their supported hooks/checkpoints and do
not have equivalent guaranteed automatic resumption.

`running`, `done` (turn complete), `needs_input`, `retired`, `exited`, and `closed`
are different states. The wrapper reports heartbeats every 30 seconds; the UI
shows offline after approximately 90 seconds without a heartbeat. New runs have
new IDs, preventing stale lifecycle hooks from overwriting the current run.

### Retirement, closure, and saved history

Retirement stops automatic inbox wake-ups and preserves the terminal for review.
A running/queued turn can finish. Late runtime hooks cannot silently unretire a
worker. Explicit Resume or a direct human message to that online retired worker
makes it available again. See [retirement](agent-retirement.md).

Individual closeout records the exact agent/run as closed, then asks only its
saved host to stop the tmux session whose hub/task/agent/run, stable tmux ID and
creation time all match. The parent project stays open. Closure intent and the
host cleanup receipt are separate, so an offline host or lost response remains
visibly retryable. Renamed owned sessions still close; absent sessions can be
confirmed; a reused name, changed run or mismatched tmux identity is preserved.
Late hooks cannot reopen the closed run, and a late cleanup failure cannot undo
an already successful receipt.

Close task records durable closure on the hub. The browser requests immediate
cleanup over each saved SSH host, and the host relay also processes closure
independently. Offline hosts retry when their relay reconnects. Successful cleanup
receipts are separate from task status and cannot be undone by a racing failure.

Private local session receipts identify hub, task, agent, run, tmux session ID and
creation time. Cleanup verifies ownership before killing; a reused session name
is not enough. Renamed owned sessions still close. If receipt delivery fails
after termination, the host retries the receipt without needing the old session.
Older sessions are adopted; already-absent legacy sessions can be confirmed on
the corresponding saved SSH host through Retry cleanup.

Closed task rows expose **View history**. Board also has **Closed projects**. The
closed board is read-only; **Download history** exports JSON containing the task,
roster, all retained messages and activity events. The board previews the latest
200 messages and can load the full conversation; exports paginate independently.
Message and event sequence spaces are separate and must not be conflated.

Full terminal scrollback and private agent-runtime conversations are **not**
archived by this feature. A persistent full transcript would require a separate
capture/retention design. See [task cleanup and history](task-cleanup.md).

Workers can post structured Board decisions with `tt ask --request-id KEY --file
request.json`. Requests contain explained choices and one reasoned recommendation;
the owner explicitly submits a listed choice or custom answer. Pending decisions
remain reachable beyond the latest 200 messages. Answers are immutable directed
human replies with recoverable receipts; recommendations and ordinary text replies
do not resolve a request. Closed projects retain readable decision history. See
[Board decisions](board-decisions.md) for retry behavior and the API contract.

### Permissions and tools

Launch defaults preserve host settings. Explicit supported presets can choose
workspace-write/no approvals or full-machine/no approvals for Codex, and supported
Claude permission modes/preapproval rules. These change the runtime's permissions,
not the hub's network authorization. New helpers inherit applicable launch settings.

Folder selection is not directory trust. Codex may still require its first-use
trust prompt even with a no-approval launch preset. Tailterm detects the visible
trust prompt in owned panes and surfaces **Permission blocked**; it never answers
it automatically. Authentication, tool failures, OS permissions and incompatible
runtime versions can still require intervention.

Tool inspection is bounded and reports host/runtime information or the exact
Codex thread's inventory where supported. It does not promise advance knowledge
of every tool a runtime might dynamically expose. See [permissions](agent-permissions.md)
and [project folders](project-folders.md).

### Vaults and shared profiles

The browser vault encrypts credentials at rest and holds unlocked credentials only
in memory. One browser tab owns the unlocked vault. Browser origins have separate
storage: localhost and TailOS do not automatically share a vault or Tailscale node.

Profile sync is opt-in and reuses the existing hub after Tailscale connectivity is
established. Username locates a profile; passphrase-derived credentials authorize
it, and the browser encrypts its envelope. The server never receives the passphrase
or plaintext profile. Enrollment and ongoing profile authentication are separate
from the shared task token.

Synced: saved servers and SSH credentials/keys, bookmarks, hub configuration,
teams, appearance. Local only: Tailscale identity, current pane/window layout,
scrollback, clipboard, transient authentication, voice state, and project handler
recovery plans, and the encrypted hub read cache. Projects, work items, and messages
already live on the hub. Conflicts pause sync for an explicit local/server choice;
it does not silently merge divergent vaults. The server retains ten prior encrypted
profile revisions. See [profile sync](profile-sync.md).

This is one active local profile per browser origin. Separate encrypted profiles
on the hub do **not** make the task/message API a multi-tenant workspace. The task
hub currently uses one shared trusted-workspace credential.

## Source map

| Area                                               | Main locations                                                                                                             |
| --------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------- |
| Application bootstrap, static/gateway integration  | `client/main.js`                                                                                                           |
| Task synchronization, launch dialogs, host aliases | `client/task-hub.js`                                                                                                       |
| Task/group reconciliation and saved layouts        | `client/tasks.js`, `client/pane-groups.js`, `client/pane-layout.js`, `client/workspace-state.js`                           |
| Projects, messages, teams                          | `client/tasks-view.js`, `client/board-view.js`, `client/teams-view.js`                                                     |
| Bugs/Features and handler launch plans             | `client/work-items-view.js`, `client/work-items.css`, `client/project-handler.js`, `hub/internal/api/work_items.go`        |
| Encrypted cached hub reads                         | `client/cached-hub-client.js`, `client/hub-read-cache.js`, `client/local-vault.js`                                         |
| History pagination/export                          | `client/task-history.js`                                                                                                   |
| Team schema, presets, models, permissions UI       | `client/teams.js`, `client/team-examples.js`, `client/model-picker.js`, `client/agent-controls.js`                         |
| Remote folder selection                            | `client/project-folder.js`                                                                                                 |
| Hub transport and API                              | `client/hub-client.js`, `hub/internal/api/`, `hub/internal/server/`                                                        |
| Durable state and migrations                       | `hub/internal/store/`                                                                                                      |
| Host CLI and relay                                 | `hub/cmd/tt/`, especially `relay.go`, `cleanup.go`, `startup.go`                                                           |
| Process/tmux creation and runtime adapters         | `hub/internal/spawn/`, `hub/internal/adapters/`                                                                            |
| Vault/profile encryption and synchronization       | `client/local-vault.js`, profile-related client files, `hub/internal/server/profiles.go`, `hub/internal/store/profiles.go` |
| WASM transport and SSH/SFTP extensions             | `wasm/`, `client/browser-ssh.js`, `scripts/build-wasm.sh`                                                                  |
| Static release packaging and verification          | `scripts/package-static.mjs`, `scripts/release-manifest.mjs`, `scripts/verify-release.mjs`                                 |
| Deployment                                         | `scripts/deploy-apple-web.py`, `scripts/deploy-truenas-hub.py`; explicit Wrangler command for TailOS                       |
| Regression coverage                                | `tests/`, Go `*_test.go` alongside hub/CLI code                                                                            |

## Design conventions

Use compact, consistent controls: selected state has background fill; hover uses
an outline. Dropdowns align in height with adjacent inputs/buttons. Placeholder
text must look like a placeholder. Avoid decorative filler, oversized action
buttons, motivational empty-state text, and expanding menus that become huge
panels. Projects/Board/Teams intentionally leave empty areas quiet.

Fullscreen uses the browser Fullscreen API for the whole UI. Expand changes the
workspace view by hiding the sidebar. The two controls are independent. Files is
hidden, but SFTP remains in use for project-folder selection and uploads.

## Validation and known limits

Unit tests cover encryption, schemas, matching, lifecycle, command quoting, and
history pagination. Real Go hub + private-tmux browser fixtures cover task/team
launching, partial retries, folders, permissions, retirement, cleanup, and history
in Chromium and WebKit. Other browser suites cover the static transport, profile
sync, menus, clipboard, appearance, uploads, speech, and vault lifecycle.

Use isolated databases, test-only tmux sockets and disposable browser contexts.
Never create smoke/demo tasks in the live hub or use a user's unlocked vault as a
test fixture. Run targeted checks first and broaden when changes warrant it.

Important remaining limits:

- Reliable unattended launch still depends on host trust, authentication and
  runtime capabilities. No promise that permission prompts can all be eliminated.
- Native inbox wake-up depends on compatible `codex queue` behavior and exact
  thread binding; other runtimes require their own integrations.
- Closing the browser does not run background profile sync. Host relay work is
  independent of the browser only while the host service is running.
- No full agent-session transcript archive, central scheduler, automatic worktree
  isolation, distributed merge coordination, or multi-user task authorization.
- Files/desktop work is intentionally deferred. Model catalog entries need
  periodic verification against the actual installed CLIs/accounts.
- WASM/runtime upgrades require rebuilding and verifying the shipped assets,
  not merely changing an npm version or copying an old `.build` directory.

There was no known unfinished code fix at handoff. The next work should come from
the owner's priorities; these limitations are not authorization to expand scope.
