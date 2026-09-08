# Discord platform findings

Researched September 8, 2026 for owner request #52. Research only: no bot,
credentials, Discord resources, or services were installed/accessed/changed.
Scope is Discord feasibility; lead owns Tailterm API mapping and the integrated
proposal. Repository baseline: `tasks-hub`, `9d87aabf4ac709c4dbca1760952c7a403c6a4936`.

**Recommendation:** use one guild-installed bot with an outbound Gateway
connection and one private text channel per task. Mirror board conversation,
accept authorized owner replies, and expose supported task actions through
commands/buttons. Keep Tailterm as the durable record. Treat channel deletion
and remote execution as separate product decisions.

## Verified platform facts

### Bot, webhook, transport, and message access

- An incoming webhook posts into a channel using its generated token; it is not
  a subscription to owner replies or a channel-management identity. Webhooks can
  override display username/avatar and manage their own messages. Creating them
  requires `MANAGE_WEBHOOKS`. A webhook alone cannot meet this objective.
  [Webhook resource](https://docs.discord.com/developers/resources/webhook)
- The Gateway is a persistent secure WebSocket opened by the application;
  resource operations generally use REST. `GUILDS` and `GUILD_MESSAGES` cover
  relevant channel/message events. Ordinary guild message text additionally
  requires `MESSAGE_CONTENT`, enabled in the portal and requested in code.
  Without it, user content fields are empty except for cases including bot
  mentions, DMs, the app's own messages, and message-context-command targets.
  [Gateway](https://docs.discord.com/developers/events/gateway)
- Interactions can arrive through Gateway **or** an HTTP endpoint; these delivery
  modes are mutually exclusive. Gateway interaction replies still use outbound
  HTTP. Initial response/defer must occur within **3 seconds**; interaction tokens
  last **15 minutes**. Thus the bot can receive both conversation and actions
  without a public inbound HTTP listener. This deployment conclusion follows
  from the documented transports; actual host egress remains untested.
  [Receiving/responding](https://docs.discord.com/developers/interactions/receiving-and-responding)
- **Current policy correction:** Discord's June 10, 2026 announcement changed
  privileged-intent review from the old 100-server threshold to **10,000 unique
  users with access across installed servers**, with annual reapplication for
  reviewed access. Under 10,000 users, enabling the portal toggle remains the
  documented path. A small private server therefore does not inherently require
  review. Verification status/server count alone is no longer a sufficient rule.
  Older Gateway/FAQ pages still describe 100 servers; use the newer dedicated
  guide and portal notices. Approval at larger scale is not guaranteed.
  [June announcement](https://discord.com/blog/updated-requirements-to-how-apps-access-data-in-servers),
  [current developer notice](https://support-dev.discord.com/hc/en-us/articles/40281523410967-Changes-to-Privileged-Intent-Access-for-Discord-Apps),
  [review guide](https://docs.discord.com/developers/gateway/getting-started-with-privileged-intent-review)

### Channels, permissions, and lifecycle choices

| Choice | Verified behavior and consequence |
| --- | --- |
| Text channel per task | Create with `MANAGE_CHANNELS`; accepts parent category, topic and permission overwrites. Names are 1–100 characters. [Guild resource](https://docs.discord.com/developers/resources/guild#create-guild-channel) |
| Read-only text history | Ordinary text channels have no thread-style `archived` flag. Permission changes can deny new messages and thread creation while retaining viewing/history. Editing channel overwrites requires `MANAGE_ROLES`; do not assume `MANAGE_CHANNELS` alone suffices. [Channel resource](https://docs.discord.com/developers/resources/channel) |
| Delete on closure | Guild-channel deletion is irreversible. This removes the Discord conversation location; retained Tailterm history is a separate concern. [Channel deletion](https://docs.discord.com/developers/resources/channel#deleteclose-channel) |
| Thread per task | Threads inherit parent permissions and use `SEND_MESSAGES_IN_THREADS`. They avoid the normal channel cap but have active-thread limits and auto-archive behavior. Unlocked threads can reopen when someone sends a message. `archived=true` plus `locked=true` restricts reopening to thread managers; archived threads are not fully synced at Gateway startup and cannot run application commands. [Threads](https://docs.discord.com/developers/topics/threads) |

Discord documents **500 channels per server**, including categories/voice/text,
and **50 channels per category**. Threads allow **1,000 participants**. The API
documents an active-thread-cap error but the reviewed primary pages do not give
a dependable numeric active-thread cap; do not confuse the participant limit
with it. Read-only text archives still consume channel slots.
[Server caps](https://support.discord.com/hc/en-us/articles/33694251638295-Discord-Account-Caps-Server-Caps-and-More),
[API errors](https://docs.discord.com/developers/topics/opcodes-and-status-codes)

Permission overwrites have precedence rules, and administrators bypass them.
Consequently a read-only channel is not an immutable audit archive; denying only
`@everyone` is insufficient if another effective role/member overwrite allows
posting. Restrict visibility to the intended participants, retain bot access,
and enforce closed-task rejection inside the bridge even for the server owner.
This enforcement is a recommendation based on
[Discord permissions](https://docs.discord.com/developers/topics/permissions).

### Replies, identity, edits, and limits

- Messages carry author/channel/message IDs and optional `webhook_id`.
  Replies use `message_reference`; bot replies require history access.
  Bot-authored text is editable by that bot, but the bot cannot rewrite a human's
  message text. Deleting others' messages requires `MANAGE_MESSAGES`.
  [Messages](https://docs.discord.com/developers/resources/message)
- Gateway emits create/update/delete events. Delete payloads identify the deleted
  message and channel, without its former content. Do not depend on a deletion
  event to reconstruct the original author/text.
  [Gateway events](https://docs.discord.com/developers/events/gateway-events)
- Bot message content is limited to **2,000 characters**; up to 10 rich embeds
  share a **6,000-character** limit. History retrieval supports `before`/`after`
  pagination, up to **100 messages** per request, with view/history permissions.
  `nonce` with `enforce_nonce` deduplicates creates only over the past few minutes,
  not indefinitely. [Message API](https://docs.discord.com/developers/resources/message)
- REST limits vary by route/bucket; read headers and honor `retry_after` on 429.
  The documented bot global ceiling is **50 requests/second**, independent of
  route limits; interaction endpoints are exempt from that global limit. Invalid
  401/403/429 requests can also trigger restrictions. Do not hardcode guessed
  channel-send rates. [Rate limits](https://docs.discord.com/developers/topics/rate-limits)
- Slash commands support structured options/subcommands and guild registration.
  Buttons can send a `custom_id` interaction; link buttons navigate without an
  interaction. These support explicit actions and a TailOS link without requiring
  ordinary message-content access.
  [Commands](https://docs.discord.com/developers/interactions/application-commands),
  [components](https://docs.discord.com/developers/components/reference)

### Recovery facts and engineering consequences

Gateway clients must heartbeat, reconnect on missing acknowledgements, and retain
session ID, resume URL and sequence. Successful Resume replays missed events in
order; an expired/invalid session requires a fresh Identify. Replay is therefore
not a permanent event archive.
[Gateway recovery](https://docs.discord.com/developers/events/gateway#resuming)

**Design requirements inferred from these facts:**

- Persist both directions' delivery state and task/channel/message mappings;
  deduplicate ingress by Discord message ID and actions by interaction ID.
  Serialize provisioning per task; recover an ambiguous channel-create response
  before creating another channel. Use stable task IDs, never names, as identity.
- Reconcile retained channel messages after non-resumable outages. Pagination of
  new messages cannot discover every historical edit/deletion. Define a bounded
  edit reconciliation policy; do not promise perfect offline change recovery.
- A durable outbound queue plus a stable visible source identifier helps recover
  ambiguous sends beyond Discord's short nonce window. Exactly-once delivery
  cannot be assumed from a WebSocket or a successful local enqueue.
- Ignore bridge-originated/bot/webhook echoes; preserve source identity, reply
  references and chunk mappings. Render agent labels under one bot identity for
  MVP; webhook impersonation-like display names add little value initially.
- Chunk long board posts, preserve code fences, disable automatic mentions, and
  coalesce routine status updates. A terminal token stream is a poor channel feed.
- Gate every action by configured guild, mapped channel, authorized Discord user
  ID and current task state. Display names and button visibility are not authority.
  Reject stale buttons and defer slow work before the response deadline.

## Actionable MVP recommendation and prerequisites

Owner/admin setup later: create a Discord application/bot, install into the chosen
guild with `bot` and `applications.commands`, select a private task category and
owner user ID, enable Message Content, and provision the bot token privately.
Discord's getting-started guide documents the application, token and installation
steps. [Bot setup](https://docs.discord.com/developers/quick-start/getting-started)

Proposed permissions: view/history/send plus channel management; include overwrite
management for closure/privacy changes. Add embed/file permissions only when
those features are used. Avoid Administrator. Thread management is needed only
if choosing threads. Runtime needs supervised execution, durable state, outbound
Discord HTTPS/WebSocket access and existing private hub access. No TrueNAS
Tailscale change or public hub exposure is implied.

MVP: task-channel provisioning/reconciliation; board text both ways; reply routing;
status/roster display; an explicit message-to-agent command; supported
retire/resume/close actions with application authorization; visible pending/failure
states; and read-only channel closure. Keep destructive close distinct from
retirement. The owner must settle whether “down” means read-only retention or
deletion, and the retention/capacity policy. Default recommendation is read-only
retention with a TailOS history link, then a separately chosen deletion policy.

For edits/deletes, initially keep already-delivered instructions durable and
append a source-change notice rather than silently changing work already acted
upon. This is a recommendation, not a Discord requirement; lead must decide the
Tailterm audit/history contract. MVP can exclude attachment mirroring.

Discord cannot itself unlock Tailterm's private browser vault, establish its
browser SSH sessions, present an interactive terminal, or ensure a host runtime
is trusted/authenticated. A launch button can request work only when a separately
authorized host execution path exists. “Everything in Discord” is realistic for
implemented coordination actions; terminal use, credentials and remote launching
need separate designs. This boundary follows the repository's
[project overview](../project-overview.md), not a newly inspected API mapping.

**Rough effort estimate, not a commitment:** one experienced engineer, about
**5–9 working days** for a reliable private-server MVP: 1–2 for Discord
transport/provisioning, 2–3 for durable two-way messaging/recovery and necessary
hub integration, 1–2 for actions/authorization/lifecycle, and 1–2 for isolated
tests and operational setup. A throwaway messaging demo could take 1–2 days.
Full edit/delete/attachment fidelity or unattended remote launch is additional
scope, potentially several more weeks depending on execution/security decisions.
Estimate depends on lead's API plan and excludes waiting for owner setup/review.

Verification: primary Discord pages were read live; the June 2026 intent change
was cross-checked against the blog, developer notice and current guide. No Discord
API calls, credentials, live-task fixtures, implementation tests or deployment
were used. Remaining dependencies: lead's integration contract; owner choices
for guild/access, archive/delete and action scope; later isolated end-to-end
tests of permissions, outages, duplicate deliveries and lifecycle races.
