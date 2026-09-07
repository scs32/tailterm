# tailterm hub and `tt`

The hub is the shared state for tailterm **tasks**: teams of agent sessions
that message each other and report lifecycle events. It runs as its own
Tailscale node (via tsnet), stores everything in SQLite, and exposes a small
JSON API on the tailnet. `tt` is the host CLI that agents and tailterm use.

## Build

```sh
cd hub && go test ./... && CGO_ENABLED=0 go build ./cmd/...
```

Cross-compile `tt` for agent hosts:

```sh
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o tt-linux-arm64 ./cmd/tt   # oracle1/oracle2
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o tt-linux-amd64 ./cmd/tt   # TrueNAS
GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build -o tt-darwin-arm64 ./cmd/tt
```

## Run the hub

Container (recommended; the image is `FROM scratch`):

```sh
docker build -t tailterm-hub hub/
docker run -d --name tailterm-hub -v tailterm-hub-state:/state \
  -e TS_AUTHKEY=tskey-auth-... -e TS_HOSTNAME=tailterm-hub tailterm-hub
```

Use a **tagged, reusable** auth key so the node is not tied to a user. The key
is needed only on first start; tsnet state and `hub.sqlite` live in `/state`.
No host port is published: the hub is reachable as `http://tailterm-hub` from
every tailnet device, including tailterm's browser node.

Dev mode without a tailnet: `TAILTERM_DEV_LISTEN=127.0.0.1:8765 ./tailterm-hub`
serves plain HTTP with a fixed identity. `scripts/container-dev.sh` in the
repo root runs both the hub and the static tailterm site in apple/container.

TrueNAS SCALE: create a Custom App from `deploy/compose.yaml` (or its hub
service alone) with a host path such as `/mnt/<pool>/apps/tailterm-hub`
mounted at `/state`.

## API

All paths under `/v1`, JSON bodies up to 64 KiB. Identity comes from Tailscale
WhoIs and is recorded on every write.

| Method | Path | Purpose |
|---|---|---|
| GET | `/whoami` | caller identity |
| POST / GET | `/tasks` | create `{name, goal}` / list |
| GET / PATCH / DELETE | `/tasks/{id}` | detail with agents / update / close |
| POST / GET | `/tasks/{id}/agents` | register `{agentId?, name, host, session, runtime, cwd, parentAgentId}` / list |
| GET / PATCH / DELETE | `/tasks/{id}/agents/{aid}` | detail / status or title / close |
| POST / GET | `/tasks/{id}/messages` | `{text, to?, agentId?}` / `?after=&to=&limit=` |
| POST | `/tasks/{id}/messages/read` | `{agentId, upTo}` |
| POST | `/tasks/{id}/events` | `{kind, agentId?, text?, data?}` with kind in started, running, done, needs_input, closed |
| GET | `/tasks/{id}/events?after=&wait=25s` | long-poll one task |
| GET | `/events?after=&wait=25s` | long-poll every task |

Limits: 200 open tasks, 32 open agents per task, 10,000 events per task, 20
writes per second per node.

## `tt`

Agent sessions carry `TAILTERM_HUB`, `TAILTERM_TASK`, `TAILTERM_AGENT`,
`TAILTERM_AGENT_NAME`, and `TAILTERM_SESSION` in their tmux environment.

```
tt new-task --name N [--goal G]        create a task, print its id
tt spawn --name N --run CMD [--cwd D] [--prompt P]
                                       start an agent session on this host
tt agents | tt tasks | tt status       inspect
tt post "text" [--to agent]            message the task or one agent
tt inbox [--unread] [--mark-read]      read messages
tt event needs_input --text "..."      report lifecycle events
tt close [agent]                       close a session on this host
tt hooks claude | codex | generic      integration snippets
```

`tt spawn` creates a detached tmux session with two windows: `agent`, running
the command through `tt wrap` so `started` and `done` are reported for any
runtime, and `tt-watch`, which long-polls the hub and types messages into the
agent window when the agent is idle (`done`). Busy agents receive messages
through the pull path: the Claude Code hooks from `tt hooks claude` report
`started`, `running`, `done`, and `needs_input`, surface unread messages on
each prompt, and block the first stop while messages are unread so the agent
reads them. Codex uses `notify` (`tt hooks codex`). Other commands work with
`tt wrap` alone.

Spawning is limited to the local host; tailterm runs `tt spawn` over SSH for
remote hosts. tmux 3.2 or newer is required. `TT_TMUX_SOCKET=name` points `tt`
at a private tmux server for experiments.
