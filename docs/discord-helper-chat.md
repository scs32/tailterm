# Talking with the owner helper from Discord

Feature `wi_2de2c1273e34473a` revision 1, work order #14828, lead ASSIGN #15862,
handler Start #15865. Plan and amendments:
`tailterm-artifacts/wi_2de2c1273e34473a/plan.md` and `plan-amendments.md` (lead,
after plan review #15856). Base `tasks-hub` at `8a99f36`. The order forbids
push, deploy and merge.

## What it does

The owner talks with their [owner helper](owner-helper.md) (their own Claude
Code session, registered as the project's `owner_helper` agent) from Discord,
the way they do in the terminal:

- A **plain message** from a configured owner, in the owner's **DM with the
  bot** or in the designated **helper channel**, goes to the helper as a
  directed owner message on the helper project's board.
- The helper's **reply** comes back to the same DM, or to the same thread in
  the helper channel. It is headed `🤖 **owner-helper** · owner helper` and is
  never shown as the owner.
- **`/status`** answers from Tailterm's records alone, so it works while the
  helper is busy, offline or not registered.
- **`/digest`** posts an hourly summary, plus a line when queued work finishes
  or something needs the owner, from the same records.

Slash commands in project channels are unchanged. `/bug` and `/feature` are a
separate item (`wi_88d921fdf8ab454e`).

## Configure

Two environment variables on the bridge (`hub/cmd/tailterm-discord`):

| Variable | Meaning |
| --- | --- |
| `DISCORD_HELPER_TASK` | The project whose owner helper the owner talks with. Unset: the feature is off. |
| `DISCORD_HELPER_CHANNEL` | Optional. A guild text channel for helper conversations. Without it, only the DM works. |

With `DISCORD_HELPER_TASK` unset, the bridge behaves exactly as before. It asks
for no DM intent, registers no global commands, adds no `/digest`, runs no helper
loops and handles no DMs. If an earlier enabled run left the DM commands
registered, the next start clears them.

The bridge refuses to start when:

- `DISCORD_HELPER_CHANNEL` is a project's mapped channel;
- `DISCORD_HELPER_CHANNEL` is set without `DISCORD_HELPER_TASK`;
- `DISCORD_HELPER_TASK` is not a project ID.

This way a misconfiguration can't change project-channel behavior.

While it is on, the bridge:

- adds the `DIRECT_MESSAGES` Gateway intent (not privileged);
- registers global `/status` and `/digest` for the bot DM only (`contexts: [1]`,
  `integration_types: [0]`), so the guild never shows two `/status` commands;
- adds `/digest` to the guild commands;
- opens each owner's DM channel at startup, so DMs sent while the bridge was down
  are read later.

The TrueNAS deployment plan does not set these variables yet. That follow-up is
backlog intake #15842 and must land before release.

## Conversations

| Where the owner writes | Conversation | Where the helper's reply goes |
| --- | --- | --- |
| DM with the bot | the DM, as one conversation | the DM |
| Top level of the helper channel | a new one; its ID is the message's | a thread started from that message when it is received |
| Inside a conversation thread | that thread's | that thread |

A follow-up in a conversation is posted with `replyTo` set to the helper's latest
reply in that conversation. That makes "and the other one?" land in context.
Threads are independent of each other.

Discord gives a thread started from a message that message's ID. A retried
thread start answered with code 160004 ("already created") counts as success. A
thread that could not be started at receipt is started before the first send
into it.

A thread made by hand from another message is not a conversation, and the bridge
ignores it.

## What the owner sees

| Helper state when the message arrives | Posted to the board? | In Discord |
| --- | --- | --- |
| Online, idle (activity idle, finished_silent or unknown) | yes | ✅ reaction |
| Online, busy (working, hung_tool, looping, runtime_prompt, stuck) | yes | "Received. The helper is busy and will answer when its current turn ends." |
| Registered but offline (not online, exited, or retired) | yes | "Received and saved, but the helper is offline…" and a pointer to `/status` |
| No open helper | **no** | "No helper session is registered, so nothing was sent." and a pointer to `/status` |

A posted message is an ordinary directed owner message. It creates an
`ack_outcome` obligation for the helper, and the relay's
[safe Claude wake](claude-wake.md) wakes the helper for it. The relay itself is
unchanged. An unanswered message escalates on the usual obligation timers, which
happens more often while the helper is offline.

## For the helper: answering

```sh
tt helper inbox --task tsk_...                              # unread, marked read
tt helper reply --task tsk_... SEQ --text "your answer"     # goes back to Discord
```

`tt helper inbox` shows a `tt helper reply` hint on each message the owner sent
from Discord. `tt helper reply` posts as the verified helper (the registered agent
and its current run) with `replyTo=SEQ`. It refuses when this host's helper
registration is missing, closed or replaced. Its default request ID comes from the
project, SEQ and text, so running the same reply again after a timeout posts once.
Pass `--request-id` to choose one.

What reaches Discord:

- A helper post is routed when it replies to a conversation's message. That can
  be `tt helper reply`, or `tt send --reply-to SEQ` run as the helper; a typed
  message renders like the project mirror, as a kind and subject header plus body.
- Owner-authored posts, other agents' posts, and helper posts with no such reply
  target stay on the board only.
- A post made as the owner, for example with the helper environment missing, is
  not the helper's and is not routed.

**Reply or `tt ack` every Discord message.** Each one is an obligation. Two
minutes after it arrives, the hub's acknowledgement gate refuses the helper's
other writes until it is answered or acknowledged. Those writes include board
posts, `tt send` and `tt ask` requests, and work item, verification and release
writes. Replies to any pending message and delegated answers still pass.

## Board and project channel

The owner's Discord messages are board messages in the helper project. TailOS
shows them with their Discord source. The bridge does not mirror its own posts, so
they do not appear again in that project's Discord channel. The helper's replies
are mirrored there as ordinary board lines, redacted like their DM or thread copy.

## Long replies and safety

- **Splitting.** Replies are split into parts of at most 2,000 characters, with
  code fences balanced and `-# #SEQ · part i/n` markers, sent in order. Each
  conversation (and the digest) has its own outbox lane, so a DM that fails never
  holds back a project channel.
- **Redaction.** Before a helper reply is sent to Discord, credential-shaped text
  becomes `[redacted]`:
  - Discord bot tokens, PEM private key blocks, `sk-ant-…` and `sk-…` keys,
    GitHub tokens, Slack tokens, AWS access keys and `Authorization: Bearer`
    values;
  - the value of any key ending in `token`, `password`, `passwd`, `secret`,
    `api_key` or `access_key` followed by `=` or `:`. That includes environment
    variables such as `GITHUB_TOKEN=`, `DB_PASSWORD=` or `AWS_SECRET_ACCESS_KEY=`,
    and JSON keys such as `"token": "…"`. The word must end the key, so
    `tokens=12` and `max_tokens: 100` are kept.

  A bare 40- or 64-character hex string (a commit SHA or digest) is kept, but the
  same hex as a secret key's value is redacted. The same redaction applies when an
  owner helper's messages are mirrored to the project channel; other agents'
  lines mirror as before. The board text is never changed. `/status` and the
  digest render only names, IDs, titles, states and durations, and redact the free
  text they show (titles, request subjects, block reasons, stall causes).
- **Owners only.**
  - Only configured owner IDs reach the helper.
  - Anyone else gets one polite refusal per 10 minutes and nothing is posted.
  - Bots, webhooks and the bridge itself are ignored.
  - A non-owner's DM command is refused ephemerally.
- **Text is data.** Message text is trimmed and posted verbatim. It is never
  parsed for commands or routed on its content, and every send uses
  `allowed_mentions.parse=[]`.
- **Rate limit.** Each owner may send 10 helper messages a minute, measured by
  when each message was written (its Discord ID), so a backfill after an outage is
  not throttled. Extra ones are not posted, with one "slow down" note a minute.
- **Idempotent.** The Discord message ID is the request ID (`discord-msg-<id>`).
  - A redelivered event, a backfilled copy or a retry after a crash gives one
    board message and one note or reaction.
  - Notes and replies are sent through the outbox with fixed keys and markers,
    so an uncertain send is found rather than repeated.

## `/status`

In the DM, the helper channel or a conversation thread, `/status` replies
ephemerally with, over all open projects (the helper's project first):

- **Running**: queue entries with their stage and how long since they last changed.
  The stage is releasing, ready to integrate, the verification state, the review
  round, or the entry state.
- **Next queued**: each project's next entry, with its block reason.
- **Waiting on you**: open owner requests and decisions, with their age.
- **Stuck**: overdue obligations, queue stalls with how long, and agents whose
  activity is stuck, hung_tool, crashed or looping, with how long.
- **Helper**: idle, busy, offline or not registered.

Each section shows at most 5 lines, then "+N more". It posts nothing to the board
and creates no obligation.

Titles come from the bridge's one read-only work item route
(`GET /v1/tasks/{id}/work-items/{wid}`). A hub that refuses it (403) gets item IDs
instead. In a project channel, `/status` is still the project's card.

## `/digest`

`/digest on [for:8h]` in the DM or the helper channel sets that channel as the one
digest destination. `/digest off` stops it. In a project channel, `/digest`
points to the DM and the helper channel.

While it is on:

- **The hourly summary.** Once per UTC hour, the bridge sends the same summary
  as `/status`.
- **Event lines.** The bridge sends one line when a queue entry closes, fails or
  is released, and one line for each new open owner request.

On `on`, everything current is recorded as already seen, so nothing from before is
sent. Every send has a fixed key (`digest:<channel>:<hour>`,
`event:entry:<id>:<state>`, `event:owner:<obligation>`), so neither a repeated
tick nor a restart sends anything twice.

The digest is hub-only in this item and costs zero agent turns. A helper-written
digest is a recorded scope note (h5 is optional).

**Hub calls per digest tick** (once a minute):

- one project list;
- a read of any project whose [status-card](broker-phase-2b.md) snapshot is older
  than two minutes (task, open obligations and queue; the card pass normally
  refreshes these every 30 seconds);
- once an hour, the helper project's agents;
- a work item title the first time it is shown.

## Storage

The storage is the bridge's own SQLite state, not the hub's:

- tables `helper_conversations` (conversation, reply channel, thread started, the
  helper's latest reply, backfill cursor) and `helper_messages` (board message to
  conversation);
- an `outbox.channel_id` column for helper lanes;
- kv keys `helper:*`, `global-commands` and `helper:digest`.

The hub gains one allowlisted bridge route, `GET /v1/tasks/{id}/work-items/{wid}`.
The bridge can read an item, but it still cannot update, dispatch or queue one.

## Verification

From `hub/`:

```sh
go test ./internal/bridge ./internal/discord -run 'Helper|DM|Thread|Digest|GlobalCommand' -count=1 -v
go test ./internal/bridge -count=1
go test ./internal/server -run 'Bridge' -count=1
go test ./cmd/tt -run 'Helper' -count=1
git diff --stat 8a99f36.. -- cmd/tt/relay.go cmd/tt/claude_wake.go   # empty
```

The tests use:

- a real in-process hub, with the helper registered through
  `POST /v1/tasks/{id}/owner-helper` and activity reported through the activity
  route;
- a fake Discord REST API whose message reads carry no `guild_id`, as Discord's
  don't;
- Gateway events injected through `Bridge.Dispatch`.

No token file, live hub or network is used.

Still unproven by the fake, so a live check (owner DM plus the helper channel) is
needed at release:

- `contexts: [1]` for a guild-installed app in the bot DM;
- a thread's ID equaling its starter message's;
- code 160004.
