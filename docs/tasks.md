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
   **Start first agent now** and choose a server and runtime. Otherwise use
   **+ Agent** from the board when ready. If launch fails, retry uses the saved
   task; **Open created task** lets you continue without launching.
   **Allow agents to add other agents** is off by default. You can change it
   later under **Settings** on the Board or **More → Settings** in Tasks.
   You can always add agents yourself. New helpers join the task’s group
   automatically when their sessions start. **Terminals** on the board opens
   that group; it never attaches the task to an unrelated tab.
5. Use **Board** for announcements and targeted replies. **Needs you** lists
   agents reporting that they need input. Click an agent to reopen its pane.

Launch profiles save a server, command and working directory for reuse.
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

There is no terminal input injection. A stopped or idle agent is not
unconditionally woken by a new message. It reads at checkpoints, on a new
prompt, or via optional runtime hooks. This avoids treating a message as shell
code after the LLM exits, or as an answer to an unrelated permission prompt.
`tt hooks claude` and `tt hooks codex` print integration instructions; existing
global runtime settings are not modified automatically.

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
