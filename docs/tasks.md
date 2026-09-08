# Tasks, Board and agent sessions

A task holds a shared objective and a team across machines. The coordination
hub owns task membership and message history. Each task has its own terminal group, named after the task. Agent panes are
kept together, separate from ordinary sessions and other tasks. Hiding a pane
does not remove the participant.

The deployed hub is a TrueNAS custom app using the machine's existing private
network address. It does not run or configure Tailscale. See
[hub setup and deployment](../hub/README.md) for the address and host setup.

## Start using it

1. Open Commands → **Task hub: configure**.
2. Select the Air or Mini and **Load configuration from server**.
3. **Test connection**, then **Save**. The token stays in the encrypted vault.
4. Open **Tasks** → **New task**, enter a name and objective, then **Create task**.
   The dialog closes and opens its board. To launch immediately, check
   **Start first agent now** and choose a server and agent app. Otherwise use
   **+ Agent** from the board when ready. If launch fails, retry uses the saved
   task; **Open created task** lets you continue without launching.
   **Allow agents to add other agents** is off by default. You can change it
   later under **Settings** on the Board or **More → Settings** in Tasks.
   You can always add agents yourself. New helpers join the task’s group
   automatically when their sessions start. **Terminals** on the board opens
   that group; it never attaches the task to an unrelated tab.
5. Use **Board** for announcements and targeted replies. **Needs you** lists
   agents reporting that they need input. Click an agent to reopen its pane.

**Model** accepts a model name or alias for Codex, Claude, Aider, or Gemini.
Leave it blank to keep that app’s configured default on the selected server.
The model must be available to the account used by that app; Tailterm does not
change credentials or validate provider access. Custom apps take model options
in **Command override**. From an agent, use `tt spawn --runtime codex --run codex --model MODEL --name helper`.

**Teams**, next to Tasks, replaces saved launch setups. Create a team of one
or up to eight members, each with a server, agent app, model, role, instructions,
command and working directory. Existing saved setups become one-member teams.
Use **New task** on a team, or select a team in the task creation dialog.
**Add to task** launches its members into an existing open task. Editing a
team changes future launches only. Roles and instructions are included in each
member's briefing. A failed partial launch retains progress; retry starts only
remaining members. An already-registered name requires inspection instead of
silently launching a duplicate. Teams travel with encrypted profile sync.

Every terminal tab is a group, including a single session. Drag a group to a
**tab edge** to reorder it. Task groups cannot be merged into any other group.
To add an ordinary session to a task group, drag its **pane header** onto that
task's tab or a pane. It joins visually as a guest, without becoming an agent.
Guests may leave again; task agents stay within their task. Closing a task
releases guest sessions into an ordinary group. Two tasks never combine.

Each new LLM agent receives the objective and instructions for communicating.
New tmux sessions retain the task environment so agents can spawn local helpers
with `tt spawn`. Remote launches use Tailterm's SSH connection; the hub itself
does not run commands on agent hosts.

The helper setting is checked by the hub on agent-originated `tt spawn`
requests, including parent membership and the task’s agent limit. Turning it
off leaves existing agents running. This is a coordination policy for the
existing trusted, single-owner setup; the shared hub credential is not an
isolation boundary against agents deliberately impersonating the owner.

## Messages and status

`tt inbox --unread --mark-read` retrieves pending messages. Read receipts record
retrieval, not task completion. `tt post 'message' --to reviewer --reply-to 17`
sends a targeted reply to an agent. To reply to a human such as `owner`, use
`tt post --reply-to 17 'message'` without `--to`; the reply appears on the shared
board. Human usernames are not agent recipients. Without either flag, a message
is a team announcement. `tt post --help` displays usage; use `tt post -- '--help'`
only if you actually want to post that literal text.

Codex agents resume automatically for directed messages and human board
announcements when the host inbox relay is running. The first `tt` command
inside Codex binds the exact thread UUID to the current agent run. The relay
uses `codex queue`, so the normal Codex terminal remains attached and runtime
approvals remain under Codex’s control. It never types into terminal panes or
marks messages read. Agent broadcasts are read at checkpoints rather than
waking every teammate.

The relay remembers which messages have triggered a wake across restarts,
ignores exited/offline agents and old runs, retries delivery failures, and
limits each agent to eight wake-ups per five minutes to contain reply loops.
A crash between Codex accepting a wake and the local progress write can cause
one repeated wake; the inbox read cursor still prevents reprocessing a message
that was already read. `tt relay --status` shows bindings, queue progress, and
errors. Unsupported runtimes continue to check the inbox themselves.

On Air and Mini, a per-user LaunchAgent runs the relay independently of the
browser. Install it with `python3 scripts/install-relay-macos.py` after
installing `tt`. On another host, run `tt relay` under its normal process
supervisor. The host’s `~/.config/tailterm/hub.json` must point to the same hub
as its agents. Bindings and queue progress are private files under
`~/.local/state/tailterm/relay`; credentials stay in the existing host config.
`tt bind --thread UUID` enrolls an existing agent when run with its task,
agent, and run environment. Never select a thread by a shared display name.

Other runtime hooks remain optional: `tt hooks claude` and `tt hooks codex`
print integration instructions without changing global runtime settings.

A completed turn, an exited process, and a request for human input have distinct
labels. The wrapper sends a heartbeat every 30 seconds; after 90 seconds without
one, the UI reports offline. Restarting an exited agent on the same machine
retains its name, ID and inbox, with a fresh run ID.

Closing a task records it closed and removes its mirrored panes. Remote tmux
sessions remain running until stopped on their hosts. A hidden pane stays
hidden until explicitly reopened. The hub does not manage git worktrees,
merge conflicts, distributed scheduling, or per-agent security boundaries.

## Views

**Terminals** keeps existing SSH sessions connected while switching modes.
**Board** keeps per-task drafts and shows delivery/read information and replies.
**Tasks** brings objectives, agents and attention requests together.
**Files** browses SFTP paths, uploads/downloads, creates folders, renames/deletes,
and opens a terminal in the current directory. Switching servers cancels the
remaining files in an upload batch. It does not reroute them to another host.

**Fullscreen** uses the browser Fullscreen API for the entire UI. **Expand**
hides the server sidebar to give the current view more room; it works independently
of fullscreen and leaves navigation available.

The same hub can optionally store [encrypted shared profiles](profile-sync.md).
Profile credentials are separate from the token used by task agents.
