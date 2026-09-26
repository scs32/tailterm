# Tailterm coordination hub

The hub stores tasks (shared objectives), named agents, durable messages and
lifecycle events in SQLite. Tailterm presents that state across servers;
`tt` starts local tmux sessions and gives agents access to the same inbox.

For architecture and migration, see the [project overview](../docs/project-overview.md)
and [handoff](../docs/handoff.md).

## This installation

- TrueNAS App: `tailterm-hub`, deployed with `midclt app.create/app.update`.
- Hub: `http://100.116.238.37:18765` on the existing TrueNAS private address.
- Persistent database: `/mnt/deepfreeze/tailterm-hub/state/hub.sqlite`.
- Access token: `/mnt/deepfreeze/tailterm-hub/hub-token` (mode 600).
- No embedded Tailscale instance, enrollment, socket mount or host Tailscale changes.
- The deployed cap is 32 open agents per task. Exited and closed agents do not consume it; retired agents do. Helper creation has a separate lifetime allowance.

The normal TCP listener requires `TAILTERM_TCP_LISTEN`, `TAILTERM_TOKEN_FILE`
and `TAILTERM_STATE`. The token must be at least 32 characters. This is a
single trusted workspace: holders of the token share access to all tasks.
It does not claim per-user or per-agent authorization. Publish its port only
on an existing private interface; use HTTPS if the transport is untrusted.

Without `TAILTERM_TCP_LISTEN`, the optional tsnet mode remains available for
other installations. `TAILTERM_DEV_LISTEN` uses an unauthenticated fixed
identity and is only for isolated tests.

## Connect Tailterm and agent hosts

`tt` is installed at `~/.local/bin/tt` on the MacBook Air and Mini. Each has a
mode-600 `~/.config/tailterm/hub.json` containing `{"url":"…","token":"…"}`.
The CLI reads this automatically. Explicit environment values override it;
a saved token is used only when the saved URL matches the requested URL.

In Tailterm, choose **Task hub: configure** from Commands. Select the Air or
Mini, click **Load configuration from server**, then **Test connection** and
**Save**. This uses the saved SSH connection and saves the hub token inside
the encrypted browser vault. No secret is embedded in the static website.

Create a task with an objective, select an agent host and runtime, and launch.
Save reusable agents, roles, models and launch settings as Teams. A team of one replaces an old launch profile. Choose an explicit project folder at launch.

`tt team launch` starts local Planned delivery members for a recorded item/order
using the existing database handler; see [local team launch](../docs/team-launch.md).
`tt team queue` manages recorded item orders for the supervised launch-host
runner; see [project team queue](../docs/team-launch.md).

```sh
tt doctor
tt new-task --name example --goal 'Review the API changes'
# Inside a task session, environment carries task, agent, run and hub identity:
tt agents
tt inbox --unread --mark-read
tt post 'Review complete' --to planner --reply-to 17
tt event needs_input --text 'Which migration should we keep?'
tt spawn --name reviewer --run codex --cwd /absolute/project
```

## Delivery and lifecycle

Messages stay in the durable inbox until retrieved; they are never typed
into a terminal. Agents receive the task briefing and instructions to check
the inbox at checkpoints. On hosts with a compatible Codex CLI and `tt relay`,
the relay binds exact thread/run identities and uses native `codex queue` to wake
eligible idle agents. Other runtimes depend on their hooks/checkpoints. See
[delivery details](../docs/tasks.md). The relay also performs durable cleanup of
individually closed agents and of all agents belonging to closed tasks.

`tt hooks claude` prints settings for SessionStart, UserPromptSubmit, Stop and
Notification. Merge these with your existing Claude settings if desired.
They surface pending messages at prompt/stop boundaries. `tt hooks codex`
prints the optional notify setting for turn-complete events. Global runtime
configuration is not changed automatically.

`tt wrap -- PROGRAM ARG...` preserves argument boundaries.
`tt wrap --shell 'COMMAND'` explicitly invokes shell syntax (used by spawn).
The wrapper reports started/exited and heartbeats every 30 seconds. A completed
LLM turn is **Turn complete**; process termination is **Exited**. Last-seen
older than 90 seconds is displayed as offline. Killing an entire tmux session
may prevent an exit event; the heartbeat expires instead.

An exited named agent on the same host can be launched again with its stable
identity and inbox, but a new run ID. Events carrying the previous run ID are
rejected. Closing/hiding a pane does not remove the agent from its task, and
hidden panes stay hidden until reopened. Closing a task records it closed and
removes mirrored panes and requests termination of the task's owned tmux sessions,
including retired agents. An accepted worker can be closed separately with an
exact run-scoped `tt close` while the project stays open. Browser SSH actions and
the host relay perform cleanup; offline hosts remain pending until confirmation.
Before individual closeout, move any useful long-lived descendant service to a
durable owner or detach and reverify it; remaining tmux descendants terminate
with the worker. Retirement alone preserves the session for intentional temporary retention. See
[cleanup and history](../docs/task-cleanup.md).

`tt close --team` closes a terminal item team after its obligations resolve,
workers before lead, preserving the database handler; see [team examples](../docs/team-examples.md).

Replies reference the original message sequence. Read receipts mean retrieval,
not completion or acceptance. The Board previews the latest 200 messages. Closed tasks can load the full
conversation and export retained messages, metadata and activity events as JSON. Spawn requests are local to the CLI host; Tailterm
launches remote agents through SSH. No distributed scheduler or automatic git
workspace/merge management is provided.

`tt withdraw SEQ --reason TEXT` withdraws your own superseded open request so it
stops obliging its recipient; see [team examples](../docs/team-examples.md).

## Build, checks and deployment

```sh
npm test
npm run test:hub
npm run build:tt
cd hub
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o ../.build/ttbin/tailterm-hub-linux-amd64 ./cmd/tailterm-hub
cd ..
python3 scripts/deploy-truenas-hub.py RELEASE --update
```

The deployment script stages a versioned binary and updates only this custom
app via middleware. SQLite and the token remain in persistent host storage.
Rollback uses a previous release path in the app configuration. Back up SQLite
with its backup API or while the hub is stopped; do not copy a live database
without its WAL. The local Apple container serves only the compiled webpage.
