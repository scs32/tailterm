# Discord task channels — integration proposal

Research date: September 8, 2026. Repository baseline: `tasks-hub` at
`9d87aabf4ac709c4dbca1760952c7a403c6a4936`. This is a design proposal, not an
implemented or deployed integration. Owner request: board message #52.

## Recommended user flow

Run a dedicated Discord bot bridge on an always-on host with existing access to
the private Tailterm hub. Keep the hub authoritative for tasks, messages, agent
identity and cleanup. The bridge opens an outbound Discord Gateway connection and
uses Discord REST for channel/message operations. Discord supports receiving
interactions over that Gateway too, so this design does not require making the
hub public or adding an incoming public webhook endpoint. See the official
[Gateway](https://docs.discord.com/developers/events/gateway) and
[interaction transport](https://docs.discord.com/developers/interactions/receiving-and-responding)
documentation. The existing TrueNAS Tailscale installation needs no changes.

1. A Tailterm task creates a private Discord text channel beneath a configured
   active-task category. A status message shows its objective, roster and a link
   to TailOS. Use the task ID in the durable mapping; names alone are not unique.
2. Board messages appear with author, recipient, sequence and reply context.
   Represent agents through one clearly labelled bot initially.
3. The allowlisted owner's normal channel messages become human board messages.
   Default to the board's Everyone recipient; a proposed `/task say agent:lead`
   command offers directed messages. A Discord reply to a mirrored agent message
   can select that agent and preserve the original board reply sequence; this
   routing must be displayed clearly. Task swarm rules still apply.
4. A compact status message tracks meaningful changes and blockers. Do not flood
   the conversation with heartbeats. `/task status`, `/task retire`, and
   `/task resume` can use existing hub capabilities through validated handlers.
5. Agent done/retired states leave the task channel open. Explicit task closure
   makes it read-only and moves it to a closed-task category. Ordinary text
   channels have no thread-style native archive flag: the bridge must implement
   this using channel permissions and category placement. Preserve history by
   default; deletion/retention is a separate owner decision.
6. A proposed `/task close` action shows a confirmation tied to the current task:
   closing initiates real agent-session cleanup. Discord channel deletion must
   never implicitly close the Tailterm task. Channel cleanup and remote terminal
   cleanup have separate status and retry handling.

## What can be done from Discord

| Capability | Fit and implementation boundary |
| --- | --- |
| Read/send board messages and reply to agents | Core bridge scope; preserves human authorship, recipients and reply IDs. |
| See agent status, blockers and history | Existing hub API; add concise Discord presentation and paginated export. |
| Change task name/objective/settings | Existing task update API; explicit commands with validated options. |
| Retire/resume an agent | Existing update API; retirement keeps terminal/run identity and does not close the task. |
| Close task | Existing close API; relay handles owned-session cleanup, possibly later for offline hosts. |
| Create task record | Existing create API; creating a record does not start agents. |
| Launch a saved team, add agents on selected machines, retry partial launch | Further host-launch design required: current launch flow uses browser-held teams, folders and SSH access. |
| Interactive terminals, vault/profile unlock, SSH trust/authentication, folder picker | Continue in TailOS initially; a chat bridge cannot supply these existing browser functions. |

Sending instructions to an existing lead through Discord can use the same
conversation workflow as the board. It does not remove host permissions, helper
allowances, trust prompts or the need for an actual launch service for standalone
Discord team launch. The bot must not treat arbitrary chat text as shell commands.

## Verified Tailterm integration points

The current code supports a substantial bridge, with several reliability gaps:

- `hub/internal/server/server.go:37`: task CRUD, roster, agent updates, messages,
  and per-task/global event feeds already exist. `client/hub-client.js` shows
  long-polling and pagination usage; the bridge should use its own durable cursors.
- `hub/internal/api/types.go:188`: messages have a sequence, human/agent sender,
  recipient, reply sequence and saved swarm delivery flag. No external origin,
  Discord user/message ID or idempotency key exists in `PostMessageRequest`.
- `hub/internal/store/store.go:521`: message insertion validates task openness,
  recipient membership and same-task reply references. It emits an event holding
  the message sequence plus a short preview; that preview is not the full body.
- `hub/internal/store/store.go:732`: events can be read globally. Events are
  pruned per task (10,000 retained; `api/types.go`), so an event cursor alone is
  insufficient after extended downtime. Reconcile current tasks and page full
  messages with separate per-task message cursors. Message and event sequences
  are separate spaces.
- `hub/internal/server/token.go` and `hub/cmd/tailterm-hub/main.go:70`: current
  bearer authentication resolves to the single workspace human `owner`. A
  generic bridge holding this token has workspace authority; do not mistake this
  for individual Discord-user authentication or multi-tenant authorization.
- `hub/cmd/tt/relay.go:158`: human announcements and directed messages can wake
  eligible exact Codex runs. The bridge must leave `agentId` empty for humans and
  never bind itself as a worker, move read cursors, or revive retired agents.
- `client/task-hub.js:645`: team launch depends on the browser's local data and
  remote command path. `client/task-hub.js:1232` adds immediate browser cleanup
  after hub closure; Discord closure would primarily rely on existing host relays.

## Small interface contract and state ownership

**Inbound input:** verified Discord guild/channel/user IDs, message or interaction
ID, text, reply reference and an optional validated agent recipient. Only configured
guilds, mapped channels and allowlisted users may write or invoke actions.
Display names and channel names are never authorization keys.

**Inbound output:** an accepted hub message sequence or a clear rejected/pending
delivery state. Add a bridge-scoped ingestion capability with durable source IDs
and provenance; atomically insert the source receipt, human message and associated
event in one hub transaction. Repeated delivery returns the original result.
Interaction actions also need stable request IDs, especially create/close.
Existing browser/CLI message shapes must remain compatible.

**Outbound input:** hub events plus reconciliation snapshots/full messages.
**Outbound output:** Discord channel, message part and status-message IDs, with
durable delivery receipts. Store task-to-channel mappings, board-to-Discord reply
mappings, per-part outbox state, independent cursors and pending lifecycle actions
in a bridge-owned database. Do not access the live hub SQLite file directly.

Use a single active bridge writer or explicit lease. Discord channel creation has
an ambiguous timeout window: reconcile a task marker before retrying creation.
Persist intended sends before network calls; use deterministic message nonces
with `enforce_nonce=true`,
reconcile uncertain delivery and only advance delivery state after acceptance.
Discord's nonce deduplication only covers the preceding few minutes, so it cannot
replace durable receipts or guarantee exactly-once delivery across arbitrary
outages. Bot message content is limited to 2,000 characters; split long hub
messages with stable part IDs and preserve the complete original. Disable
automatic mass/role mentions in mirrored text. These limits and options are
documented in [Create Message](https://docs.discord.com/developers/resources/message#create-message).

For the first version, preserve accepted messages as immutable instructions.
Discord edits/deletes must not silently rewrite commands an agent already acted
on; show an explicit bridge policy and use correction replies. Attachment bytes
are outside the initial text contract; do not claim URLs are durable archives or
silently discard unsupported content. Closed-task writes must reject visibly.

Bridge failures should leave Tailterm usable: queue during outages, obey returned
rate-limit delays, report revoked permissions/missing channels and reconcile after
reconnection. Discord's [rate-limit documentation](https://docs.discord.com/developers/topics/rate-limits)
requires handling route-specific limits rather than assuming fixed throughput.

## Delivery plan and acceptance checks

Planning estimate for one experienced engineer, subject to implementation and
deployment access: **1–2 engineering days for a narrow working prototype; roughly
5–10 engineering days total for a reliable first release** with lifecycle, audited
identity, recovery, tests and basic task controls. Full saved-team launch from
Discord needs a separately scoped host-execution design and is excluded.

Suggested ownership when implementation is authorized: integration lead owns API
schema/auth/idempotency and deployment contract; one builder owns the standalone
bridge; a reviewer owns isolated acceptance fixtures. UI changes can remain small
(bridge health/channel link) or follow later.

Acceptance scenarios must exercise: task creation without duplicate channels;
messages and replies in both directions; correct human/agent and swarm routing;
long Unicode messages; bot echo suppression; unauthorized users; invalid recipient
and cross-task reply; duplicate inbound deliveries; crashes around accepted sends;
expired Gateway sessions and message backfill; missed/pruned hub events; 429s;
permission revocation and deleted channels; task closure racing with inbound text;
retirement without channel closure; offline-host cleanup followed by durable
receipts; history preservation; and browser/CLI compatibility. Use isolated hub
databases, disposable browser contexts and private tmux sockets. Any live Discord
acceptance needs a dedicated test guild/category after bot setup is authorized.

Research verification is source/code inspection only. No bot was registered, no
Discord resources were created, no implementation was tested and no production
service or networking was changed. Existing unrelated QA baseline reports are
inconclusive and are not evidence for this proposed feature.

See [Discord platform findings](discord-platform-findings.md) for official platform
sources, setup permissions, intent requirements and capacity caveats. Before
implementation, choose the Discord server/category, authorize the bot identity,
identify permitted owner user IDs and privately provision its token. Recommended
defaults are ordinary task channels, read-only closed history, and board/task
control scope first.

Enable Message Content for ordinary owner messages; the current
[privileged-intent guide](https://docs.discord.com/developers/gateway/getting-started-with-privileged-intent-review)
uses a 10,000-user review threshold, superseding older 100-server guidance.
The bot needs channel creation plus view/history/send permissions; changing
permission overwrites for closure also requires `MANAGE_ROLES`, as documented
in [channel permissions](https://docs.discord.com/developers/resources/channel#edit-channel-permissions).
Enforce closed-task rejection in the bridge even for Discord administrators,
who can bypass channel overwrites.

Plan category overflow and retention before accumulating archives: Discord allows
50 channels per category and 500 channels per server, including categories and
other channel types. Read-only channels still count. Threads are an alternative
if scale warrants their different archive/reopen behavior. See the official
[server caps](https://support.discord.com/hc/en-us/articles/33694251638295-Discord-Account-Caps-Server-Caps-and-More).
