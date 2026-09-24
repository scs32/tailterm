# Broker phase 2b — Discord bridge v1

```text
ASSIGN: Give the owner a per-project Discord channel that mirrors the board and can unstall work
Refs: design docs/message-broker.md (Discord section, migration phase 2); phase 2a docs/broker-phase-2a.md
Objective: A bridge on TrueNAS mirrors each open project's board to its own Discord channel,
           turns the owner's Discord messages into human board messages, and surfaces owner
           escalations with buttons that nudge or reassign, all through deterministic hub calls.
Owns: see "Ownership"
Acceptance: d1–d16 below
```

Hub record: Feature `wi_5a203621825434d1`, owner intake #8796, work-order message #8799.

Status: released September 24, 2026 (hub and bridge at `d6d5155`, see
[release evidence](releases/broker-phase2b-d6d5155/README.md)). Written September 24, 2026 after the owner registered the bot.
It builds on phase 2a, which is released: obligations, timers, lead and owner
escalation notices and the project-stall notice exist on the board today. Phase 2b
delivers them to Discord and adds the first unstall controls. The rest of the
command set (`/extend`, `/answer`, `/cancel`, `/resume`, `/pause`) is phase 3.

## Owner setup (done)

Done September 24, 2026:

- **Bot.** "Tailterm Broker" is registered with Public Bot off, the Message Content intent
  on, and the Presence and Server Members intents off.
- **Server.** The bot is installed in the owner's private "Tailterm" server with the `bot`
  and `applications.commands` scopes.
- **Permissions (`2252126231284816`).** View Channels, Send Messages, Send Messages in Threads,
  Create Public Threads, Manage Threads, Read Message History, Embed Links, Add Reactions,
  Manage Channels and Pin Messages (added after the first live run, since pinning the status
  card needs it). It does not have Administrator or Manage Roles.
- **Token.** It is at `/mnt/deepfreeze/tailterm-hub/discord-token`, owned 950:950, mode 0400,
  mounted read-only into the bridge. The Mini also has a copy at `~/.config/discord/token`.
  Only one process may connect with it at a time: every Gateway session receives every
  interaction, so a second bridge races the first.
- **Guild and owner IDs.** The owner holds them, and deployment configuration carries them.
  They stay out of this public repository.

Two setup facts shape the design:

1. **No Manage Roles, so no permission-overwrite edits.** A closed project's channel moves
   to an archive category, and the bridge itself rejects writes there (d12). The research
   already required bridge-side rejection because server administrators bypass overwrites.
2. **The bot inherits `@everyone` defaults, including Mention Everyone.** Every send
   therefore sets `allowed_mentions` explicitly (d5).

## Why

Phase 2a makes stalls visible, but only on the board, and the owner isn't watching the board
when work stalls. The phase-2 exit criterion is that no incident-class stall goes
unnoticed. That needs a channel the owner sees on their phone, plus a one-tap way to act on
what it shows.

## Scope

In scope:

1. **Bridge process.** A new Go binary, `hub/cmd/tailterm-discord`. It has its own SQLite
   state and one active writer. Discord access is an outbound Gateway WebSocket for events
   and interactions, plus REST for everything else. Nothing listens publicly.
2. **Hub: bridge credential.** A second, bridge-scoped bearer token. It resolves to the owner
   identity with node `discord-bridge`, restricted to an allowlist of routes: list/get
   projects, read messages and agents, post human messages, list obligations, reassign,
   nudge. Every other route returns 403. The workspace owner token is never given to the bridge.
3. **Hub: message provenance.** Human posts may carry a source, `{kind: "discord",
   messageId, userId}`, which is stored with the message. Retries are idempotent through
   the existing request ID (`discord-msg-<messageId>`).
4. **Hub: owner nudge.** An owner-only action on an open obligation queues an immediate
   wake outside the retry schedule. It's rate-limited to one per obligation per 2 minutes
   and recorded with provenance.
5. **Hub: escalation level in refs.** Escalation notices carry `refs.escalation` = `lead` or
   `owner`, and the project-stall notice carries `stall`, so the bridge doesn't parse subjects.
6. **Channel per project.** Each open project gets a text channel in a configured active
   category, and each closed project's channel moves to an archive category. The project ID
   lives in the channel topic as a marker, which is how an ambiguous create is reconciled
   before any retry.
7. **Mirror.** Board messages render as one compact line built from the envelope, for example
   `lead → builder · ASSIGN · Fix the stale smoke test`. The body goes in a collapsed embed,
   refs become TailOS links, and long free text is split into stable parts. Routine
   `notice` bookkeeping and wake chatter update the status card instead of posting lines.
8. **Pinned status card.** One per channel, edited in place: roster with states, open
   obligations per agent, the overdue count and the last change. Edits are coalesced.
9. **Owner escalations.** An owner-level escalation or project-stall notice posts a
   message that mentions only the owner. It has **Nudge**, **Reassign…** and **Open in
   TailOS** buttons, and the stall message lists every overdue obligation.
10. **Owner input.** A plain message from an allowlisted user in a mapped channel becomes a
    `human` board message. A Discord reply to a mirrored agent message is directed to that
    agent with `replyTo` set, so a directed human obligation is created as in phase 2a.
11. **Slash commands**, registered per guild:
    - `/status` shows the status card on demand.
    - `/stalled` lists overdue obligations, oldest first, each with buttons.
    - `/nudge obligation-or-agent` nudges.
    - `/say [agent] text` posts a human message.
    - `/reassign obligation agent` reassigns.

Out of scope:

- `/extend`, `/answer`, `/cancel`, `/resume`, `/pause` and `/unpause` (phase 3).
- Launching teams or agents from Discord.
- Attachments and Discord edit/delete propagation.
- Per-agent webhook identities.
- `role:` recipients.
- Any change to relay wake behavior other than consuming the new nudge jobs through the
  existing lease path.
- Tailscale changes on TrueNAS.

## Design notes

- **The hub stays authoritative.** The bridge never reads the hub database. It uses the HTTP
  API with its own credential and keeps its own cursors, mappings and outbox.
- **Outbound delivery.** Every send is persisted first, with a deterministic nonce and
  `enforce_nonce`. State advances only after Discord accepts the send. After an outage the
  bridge reconciles an uncertain send by looking for its visible source marker, not by
  resending blindly.
- **Inbound idempotency.** A Discord message ID maps to one board message through the
  request ID. An interaction ID maps to one action receipt in bridge state.
- **Interactions.** An interaction is deferred within 3 seconds. Authority checks run on
  every action: configured guild, a mapped channel for an open project, an allowlisted
  user ID, and current obligation state. Buttons carry the obligation ID and a state
  version, and a stale button answers "already handled" with no side effects.
- **Mentions.** Mirrored text sends with `allowed_mentions: {parse: []}`. An owner
  escalation adds exactly `users: [owner]`.
- **Echo suppression.** The bridge's own messages and messages from other bots or webhooks
  are never ingested.
- **Rate limits.** The bridge follows per-route buckets and `retry_after`. Status-card
  edits are coalesced to at most one per channel per 30 seconds. A 429 never loses a queued
  send.
- **Failure isolation.** A bridge outage leaves Tailterm fully usable. The bridge catches up
  from its cursors, and the hub's escalation timers are unaffected.
- **Library.** Prefer the standard library plus `github.com/coder/websocket`, which is
  already in the module graph, over adding a Discord SDK. The planner confirms this or
  argues for an alternative.
- **Deployment.** The default is a second service in the `tailterm-hub` TrueNAS app. It
  reaches the hub over the app's internal network, uses a separate read-only binary mount and
  a private state volume, mounts the Discord token at `/run/discord-token` and the bridge hub
  token at `/run/bridge-token`, and takes guild, owner and category settings from plan
  fields. This follows the `deploy-truenas-hub.py` and preflight pattern, including
  verified backups. The planner confirms the network path before any code depends on it.

## Ownership

| Member | Owns |
| --- | --- |
| builder | Hub scope items 2–5: token, provenance, nudge, escalation refs, their routes and tests. Bridge scope items 1 and 6–11: `hub/cmd/tailterm-discord/`, its state, a fake Discord REST and Gateway for tests. Deploy-script and preflight plan fields, with their tests |
| planner | Confirms the hub routes the bridge needs, the TrueNAS network path, and the library choice first |
| reviewer | Two review rounds per the policy |
| database | Records the item, order, review outcomes and release |
| lead | Routing, acceptance and release disposition |

## Acceptance

| # | Criterion (observable) |
| --- | --- |
| d1 | The bridge token can read projects, messages, agents and obligations, post human messages, reassign and nudge. Any other route returns 403. A wrong or missing token is refused (403, as today). The owner token keeps working unchanged. |
| d2 | A human post with a Discord source stores the source. Repeating it with the same request ID returns the original message and creates no duplicate message or obligation. |
| d3 | An owner nudge on an open obligation creates one immediate wake job. A second nudge within 2 minutes is refused with a retry-after and no job. A nudge on a closed obligation conflicts with no side effects. |
| d4 | Lead, owner and stall escalation notices carry `refs.escalation`. Existing phase-2a escalation tests pass unchanged. |
| d5 | Against the fake Discord: a new open project gets exactly one channel in the active category, even when the create response is lost and retried. The topic carries the project marker. No send ever omits `allowed_mentions`. |
| d6 | Typed and free-text board messages mirror in order as compact lines with embeds and TailOS links. A 5,000-character Unicode message arrives as ordered parts with code fences intact. |
| d7 | Routine notices and wake traffic change only the status card. Card edits are coalesced to at most one per channel per 30 seconds under a burst of 50 messages. |
| d8 | An owner-level escalation posts one message that mentions only the owner, with Nudge, Reassign… and Open in TailOS. A lead-level escalation mirrors as a line with no mention. |
| d9 | A plain owner message becomes a `human` board message. A Discord reply to a mirrored agent message becomes a directed human message with `replyTo` and creates its obligation. |
| d10 | Messages from a non-allowlisted user, from another bot, from the bridge itself, or in an unmapped channel create nothing. An interaction from a non-allowlisted user is refused with an ephemeral reply. |
| d11 | A duplicate Gateway delivery of the same message or interaction produces one board message or one action. A stale button, for an obligation already closed or reassigned, answers "already handled" with no side effects. |
| d12 | Closing a project moves its channel to the archive category. Later owner messages there are rejected visibly and create nothing on the board. |
| d13 | `/status`, `/stalled`, `/nudge`, `/say` and `/reassign` each do what scope item 11 says, through hub calls with request IDs. `/stalled` matches `tt obligations --overdue`. |
| d14 | Kill the bridge mid-send, restart it, and take the hub down for 5 minutes: nothing is lost or duplicated. Uncertain sends are reconciled by marker. A 429 with `retry_after` is honored. An expired Gateway session re-identifies and backfills from cursors. |
| d15 | Live check in the owner's test server on the deployed bridge. The shadow-week project mirrors. A forced overdue obligation on a disposable production-check project reaches the owner's phone as an escalation with working buttons. The project is then closed and its channel archives. |
| d16 | `go vet ./...`, `go test ./...` and `npm test` pass, apart from known pre-existing failures. The deploy preflight tests cover the new plan fields. The phase-2a hub binary still starts on the migrated database, which gives rollback. |

## Deployment and exit

1. **Hub first.** Deploy to TrueNAS with a verified backup; it gains the bridge-token
   route scope, provenance, nudge and escalation refs.
2. **Bridge next.** Deploy the bridge service with the Discord and bridge-token mounts, then
   do the live check (d15).
3. **Rollback.** Remove the bridge service. The hub changes are additive and inert without it.

Exit: for the rest of the shadow week, every owner-level escalation reaches Discord within
one minute of its board notice, and the owner can nudge or reassign from the phone
without opening TailOS.

## Open decisions (defaults stated)

1. **Channel or thread per project.** Default: a channel per project, which fits under the
   50-per-category limit at current volume. Revisit threads if archives approach it.
2. **Archive retention.** Default: keep archived channels indefinitely. Deletion is a separate
   owner decision.
3. **Manage Roles.** Default: not needed in 2b. If truly read-only archives are wanted later,
   the owner re-invites the bot with Manage Roles added (`326686035024`).
