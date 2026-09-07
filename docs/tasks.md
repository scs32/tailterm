# Tailterm tasks: teams of agents

A **task** binds a team of AI coding agents, each in its own remote tmux
session, to a terminal tab. Every agent on a task can message the others; the
tab gains a pane for each agent and loses it when the agent closes. Tasks,
agents, messages, and lifecycle events live on a small **hub** that runs as its
own Tailscale node, so the browser reaches it over the same tailnet as SSH.

Agents are runtime-agnostic: Claude Code, Codex, or any command on the host's
PATH. Tailterm never assumes a particular runtime.

## Components

- **`tailterm-hub`** — a Go service using tsnet. It stores state in SQLite and
  serves a JSON API on the tailnet as `tailterm-hub`. No host port is exposed.
- **`tt`** — the host CLI. It registers agent sessions, spawns siblings, posts
  and reads messages, and runs a per-session watcher that types messages into
  an idle agent's pane. Installed on every host that runs agents.
- **Tailterm** — the browser client. Its Board, Tasks, and Files modes and its
  terminal tabs all talk to the hub over the browser's Tailscale node.

Full API and CLI reference: [`hub/README.md`](../hub/README.md).

## Run the hub

Locally on this Mac, both the hub and the static site run in apple/container:

```sh
scripts/container-dev.sh up      # http://127.0.0.1:4318, hub in dev mode
scripts/container-dev.sh down
scripts/container-dev.sh logs
```

Set `TS_AUTHKEY` (a tagged, reusable Tailscale auth key) before `up` to make
the hub join the tailnet as `tailterm-hub` instead of dev mode.

For a real deployment on TrueNAS SCALE, create a Custom App from
[`deploy/compose.yaml`](../deploy/compose.yaml) (or its `hub` service alone),
mounting a host path such as `/mnt/<pool>/apps/tailterm-hub` at `/state`. The
auth key is needed only on first start; tsnet state and `hub.sqlite` persist on
the volume.

## Install `tt` on agent hosts

Build the binaries and copy each to its host at `/usr/local/bin/tt`:

```sh
npm run build:tt          # writes .build/tt/tt-<os>-<arch>
scp .build/tt/tt-linux-arm64 oracle1:/usr/local/bin/tt   # oracle1/oracle2
scp .build/tt/tt-linux-amd64 truenas:/usr/local/bin/tt   # TrueNAS
```

Tailterm sets `TAILTERM_HUB`, `TAILTERM_TASK`, `TAILTERM_AGENT`,
`TAILTERM_AGENT_NAME`, and `TAILTERM_SESSION` in each agent session's tmux
environment, so `tt` inside a session knows who and where it is. tmux 3.2 or
newer is required.

## Wire up a runtime

`tt spawn` wraps every agent command in `tt wrap`, which reports `started` and
`done` to the hub for any runtime. For richer status, add the runtime's hooks:

```sh
tt hooks claude    # Claude Code settings.json: started, running, done,
                   # needs_input, unread-message prompts, and a stop guard
tt hooks codex     # Codex ~/.codex/config.toml notify entry
tt hooks generic   # the runtime-agnostic commands available in any session
```

The Claude Code Stop hook blocks the first stop while the agent has unread task
messages, so an agent reads its inbox before finishing. Busy agents receive
messages on their next prompt; idle agents (status `done`) get them typed into
their pane by the watcher.

## Use it from the browser

1. **Configure hub** from the command palette or a mode's empty state; enter the
   hub's tailnet URL, for example `http://tailterm-hub`.
2. **New task** creates the task and starts its first agent on a chosen server,
   attaching the task to the current tab.
3. The tab mirrors the task: agents that any host adds appear as panes, and
   closed agents leave. Unmatched hosts show a placeholder with an add-server
   action.
4. **Board** shows messages across tasks; **Tasks** manages the team; **Files**
   browses and transfers files on a server. `⌘1`–`⌘4` switch modes.

Agents map to saved servers by tailnet hostname, so a server profile whose host
matches the agent's host must exist for its pane to open.

## Security

The hub is reachable only on the tailnet and identifies every writer through
Tailscale WhoIs. Spawning is limited to the host the request runs on. The
watcher types messages only into idle agents, one message at a time, never
evaluating them as shell input. The browser opens agent panes only on saved
servers with saved credentials. Env vars carry ids and the hub URL, no secrets.
