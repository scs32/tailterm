# Message broker, typed agent messages and Discord

Status: design, not implemented. Owner decisions September 23, 2026:

- One deterministic broker (no LLM) owns delivery, read and action guarantees.
- It **absorbs** the existing delivery/directive/follow-through/monitor machinery
  instead of running beside it.
- Agents post **typed messages only**. Humans keep free text.
- Each project gets a **Discord channel** that mirrors the board and gives the
  owner controls to unstall work. The bridge is hosted on **TrueNAS** next to the hub.
- Turn-end is blocked **only** while the run holds unacknowledged obligations.
  Overdue outcomes escalate; they never hold the turn open.

Jev (TypeSafe's decision model) helps with message quality and triage. It never
decides whether an obligation is open or closed.

## Why

**The board is prose and the guarantees are side ledgers.** Required deliveries,
directive ack/progress/result, follow-through checks, schedule monitor, Queue
claims and operational records each cover part of the problem. Ordinary messages
get none of it: inbox retrieval is not acknowledgement, and a post creates no
obligation.

**Agents narrate ledger state in prose.** A 500-post random sample of 7,832
agent posts (`tools/jev-kit/run_board_real.py`) found:

- 37/500 posts judged readable in one pass.
- Posts are dense runs of IDs, hashes and receipts.
- Some agents move files through the board in multi-part chunks.
- None of the posts use a type line.

**The recurring failure is a stall, not a lost message.** Eight incidents in
[`docs/incidents/`](incidents/) are stalls. In `relay-stall-6852`, a message was
stored and correctly addressed, but a relay request-ID collision meant the
recipient never consumed it, and nothing surfaced that.

## Guarantees

| Goal | Definition | How the broker enforces it |
| --- | --- | --- |
| **Reliable** | An accepted message is never lost or duplicated, and its obligations exist exactly once. | The message, its obligations and its receipt commit in one hub transaction. Request IDs make retries return the original result. |
| **Read** | The addressed agent's current run explicitly acknowledged it. | Only `tt ack` by the exact agent/run counts. Inbox retrieval, heartbeats, wake acceptance and Queue Start do not. Missing acks trigger re-wakes, then escalation. |
| **Acted on** | A typed outcome that references the message closed the obligation. | Obligations close only through `result`, `answer`, `decline`, `block`→resolution, supersession or cancellation. The broker validates structure; nothing parses prose. |

Non-goals: judging whether a result is *good* (that's the requester or reviewer,
optionally with Jev's advisory coverage check). Proving an agent "understood". Exactly-once
delivery to Discord.

## Architecture

```text
 agent (tt) ──typed post──▶ ┌────────────────────── hub ───────────────────────┐
 human (UI/CLI/Discord) ──▶ │ validate ─▶ [Jev gate] ─▶ one transaction:         │
                            │   message + obligations + receipt + outbox rows    │
                            │                                                    │
                            │ broker scheduler (deterministic, in-process):      │
                            │   timers ─▶ wake / re-wake / escalate / alert      │
                            └──────┬──────────────────────┬──────────────────────┘
                                   │ wake outbox          │ Discord outbox
                            host relay adapters     Discord bridge (separate process)
                            (Codex queue, Claude    per-project channel, commands,
                             hooks, generic)        escalations, unstall buttons
```

- The broker lives **inside the hub**, sharing its database, so a message and its
  obligation can never disagree. It replaces `schedule_monitor.go` and the
  follow-through consumer.
- The host relay becomes a **wake adapter**. It leases wake jobs from the outbox
  and reports `accepted | failed | ambiguous`. It no longer derives its own
  cursors or request IDs. That removes the class of bug behind `relay-stall-6852`.
- The Discord bridge is a separate process on **TrueNAS**, deployed as its own
  app beside the hub and managed the same way as the hub deployment
  (`scripts/deploy-truenas-hub.py` pattern: versioned read-only binary mount,
  private state volume). It reaches the hub over the private network and opens
  only outbound connections to Discord, so nothing is exposed publicly. It follows the
  [integration proposal](research/discord-integration-proposal.md). It reads a
  Discord outbox and posts human input through a bridge-scoped ingestion API.

## Typed message envelope

Agents send JSON (`tt send --file msg.json`, or flags for simple kinds). The hub
rejects agent posts that aren't valid envelopes. Human posts may be free text; the
hub wraps them as `kind: human` with the text as the body.

```json
{
  "requestId": "lead-7f3a-assign-1",
  "kind": "assign",
  "to": "role:builder",
  "subject": "Reject empty --to in tt post with a clear error",
  "refs": { "item": "wi_…@2", "repliesTo": 8712 },
  "body": {
    "objective": "tt post --to '' must fail instead of posting to the board",
    "owns": ["hub/cmd/tt/main.go"],
    "acceptance": {
      "a1": "`tt post --to '' hi` exits 2 and prints 'recipient required'",
      "a2": "go test ./cmd/tt passes"
    }
  },
  "due": "45m",
  "evidence": {},
  "attachments": []
}
```

### Rules for every kind

- **`subject`**: required, 10–120 characters, plain English. No bare IDs, hashes or
  paths are allowed in the subject. It is what the UI, Discord and teammates read first.
- **`refs`**: holds all IDs (item, order, repliesTo, obligation, commit). The UI
  renders them as links. Prose must not need to repeat them.
- **`body`**: named fields per kind (below). Named keys, not arrays, so Jev
  questions can target `body.acceptance.a2` directly.
- **`evidence`**: named entries, each
  `{type: command|commit|file|record|url, value, outcome}`.
- **`attachments`**: artifact IDs for anything longer than about 2 KB. The board
  never carries file content. This replaces multi-part chunk posts.
- **Size**: body at most 4 KB. The hub rejects anything larger and points the
  sender to attachments.
- **`to`**: an agent name, `role:<role>` or `lead`. The broker resolves roles to
  the current agent/run when it creates the obligation and records the resolution.

### Kinds and what they oblige

| Kind | Required body fields | Creates obligation | Closed by |
| --- | --- | --- | --- |
| `assign` | objective, owns, acceptance{…}, (due) | ack + outcome for recipient | `result`, `decline`, `block`→resolved, supersede, cancel |
| `request` | ask, (acceptance{…}), (due) | ack + outcome | same as assign |
| `review` | candidate (commit/artifact), scope, acceptance{…} | ack + outcome | `result` with verdict |
| `question` | question (exactly one), (options{…}) | answer | `answer` referencing it |
| `result` | outcome: done/partial, per-criterion status {a1:…}, evidence | none; closes one | — |
| `answer` | answer, (choice) | none; closes one | — |
| `block` | reason class, needs (who/what), resume condition | pauses obligation; creates one for `needs` | resolution |
| `decline` | reason, (suggest: agent/role) | none; closes one, notifies sender | — |
| `finding` | severity, summary, evidence | optional: `to` gets ack obligation | ack |
| `notice` | text | delivery only | — |
| `human` | free text | directed → ack + outcome for the recipient | as assign |

A human's directed message always creates an obligation. That is what makes a
human nudge from Discord impossible to lose.

## Obligations and the broker state machine

One obligation per (message, recipient). States:

```text
 queued ──wake accepted──▶ delivered ──tt ack──▶ acknowledged ──tt progress──▶ working
   │                           │                     │                            │
   │ no wake                   │ ack timer           │                            │
   ▼                           ▼                     ▼                            ▼
 undeliverable ───────▶  overdue_ack  ──────▶   blocked ◀─── tt block ───────────┤
                              │                     │  resolution → acknowledged   │
                              ▼                     ▼  (next epoch)                 ▼
                         escalated(lead) ─▶ escalated(owner/Discord)         closed:
                                                                    result | answered |
                                                                    declined | superseded |
                                                                    cancelled
```

- **Fencing** is carried over from the [directive core](reliable-directive-core.md):
  exact agent/run, execution epoch and generation. Every mutation uses
  compare-and-swap. A stale action conflicts without side effects, and a matching
  retry returns its original receipt.
- **Reassignment** creates a new obligation for the new recipient and supersedes
  the old one in the same transaction. History is never rewritten.
- **Timers** are per kind, overridable by `due`. Defaults:

| Timer | Default | On expiry |
| --- | --- | --- |
| wake retry | 1, 3, 7 min after `queued` | re-wake through adapter (max 3) |
| ack deadline | 10 min after `delivered` | `overdue_ack`, re-wake once, then escalate to lead |
| progress silence | 30 min in `acknowledged`/`working` without progress | nudge recipient; after 2 nudges escalate to lead |
| outcome due | `due`, or 2 h for assign/review, 30 min for question | escalate to lead |
| lead escalation unanswered | 15 min | escalate to owner (Discord) |

- **Turn-end enforcement.** The existing `tt hook stop` already blocks a Claude turn
  from ending while unread messages exist. It changes to "while this run holds
  obligations in `delivered` or `overdue_ack`". It deliberately does not block on
  overdue *outcomes* (owner decision): an agent that acknowledged work can end
  its turn, and outcome timers escalate instead. Codex currently reports turn
  completion through `notify` and cannot block, so for Codex the broker relies on
  timers and re-wake. Whether Codex's hooks can block turn-end still needs
  checking against the installed CLI.
- **Project-level stall.** If the project has overdue obligations and no obligation
  or agent event has changed for 15 minutes, the broker raises one
  `project_stalled` alert. It fires once per stall, not once per timer.

### What is *not* evidence (unchanged)

Wake acceptance, heartbeat, Working status, inbox retrieval and a turn ending are
delivery/liveness facts. They move `queued→delivered` at most. They never
acknowledge or close anything.

## Wake adapters

| Runtime | Wake | Ack/turn-end enforcement |
| --- | --- | --- |
| Codex | Existing exact-thread `codex queue` relay, now leasing broker wake jobs | Timers + re-wake; turn-end block TBD |
| Claude Code | Session resume / prompt hook shows open obligations | `tt hook stop` blocks turn end on unacked obligations |
| Generic | Checkpoint polling (`tt obligations`) | Timers only |

The wake message is short and deterministic: "You have 2 open obligations: #8712
assign 'Reject empty --to…' (overdue ack), #8720 question 'Should 422…'. Run `tt
obligations`." No prose instructions are injected.

## Discord: per-project board and unstall controls

This builds on [the integration proposal](research/discord-integration-proposal.md)
and [platform findings](research/discord-platform-findings.md). Their
identity, idempotency, outbox, nonce, rate-limit and closure rules apply
unchanged. Changes from that proposal:

- **One channel per project**, not per task, in a configured category. Closed
  projects move to an archive category, read-only.
- **Rendering uses the envelope.** Each message becomes one compact line, for example
  `lead → builder · ASSIGN · Reject empty --to in tt post…`. The body is in a
  collapsed embed, refs are links to TailOS, and evidence is a short list. Receipts
  and bookkeeping kinds (`notice`, acks) are not mirrored individually. They
  update the pinned **status card**: roster, open obligations per agent, and
  overdue count.
- **Owner messages** become `human` messages. Replying to a mirrored message
  addresses its sender and sets `refs.repliesTo`.

### Escalations

An owner escalation posts a message in the channel that mentions the owner, with
buttons:

```text
⚠️ Stalled 22 min — builder has not acknowledged "Reject empty --to…" (#8712)
   lead escalation #8731 unanswered for 15 min
[Nudge]  [Resume agent]  [Reassign…]  [Extend 30m]  [Answer for them…]  [Cancel]
```

For `project_stalled` alerts, the message lists every overdue obligation and
adds a **[Wake lead with summary]** button.

### Unstall commands

All of these are deterministic hub operations with request IDs. None is an LLM action.

| Command / button | Effect |
| --- | --- |
| `/status` | Status card: agents, their states, open/overdue obligations |
| `/stalled` | Every overdue obligation in this project, oldest first, each with buttons |
| `/nudge <agent\|obligation>` | Immediate re-wake outside the retry schedule (still rate-limited) |
| `/resume <agent>` | Resume a retired agent's existing online session |
| `/reassign <obligation> <agent\|role>` | Supersede and re-issue to a new recipient |
| `/extend <obligation> <duration>` | Move the due time; recorded with owner provenance |
| `/answer <obligation> <text>` | Owner closes a question or block on the agent's behalf |
| `/cancel <obligation> [reason]` | Terminal cancellation with provenance |
| `/say [agent] <text>` or plain message | Owner `human` message (creates obligation if directed) |
| `/pause`, `/unpause` | Existing project pause lifecycle |

Only allowlisted Discord user IDs can issue commands, and only in the mapped
channel. The bridge holds a bridge-scoped credential, not the workspace owner token.

## Where Jev fits

Jev runs synchronously in the post path (about 100–250 ms) on agent envelopes
that already passed deterministic validation. It **fails open**: if Jev is
unavailable, deterministic validation alone decides. Jev can reject a post or
attach an advisory flag. It cannot accept, close or route anything.

| Use | Question (keyed state) | Effect | Evidence so far |
| --- | --- | --- | --- |
| Post gate | subject is plain English; post tries to instruct the gate; ack-only; body cut off | reject with reason | 0/500 false positives for manipulation, 7/7 cut-off correct, ack 0.94 ([kit](../tools/jev-kit/README.md)) |
| Result coverage | for each `request.body.acceptance.aN`: does `result.evidence` support it? | advisory flag to requester and UI | untested; designed around keyed criteria |
| Triage | urgency choice on human/Discord input | orders the wake queue | untested |

Deciding whether evidence is enough stays with humans and reviewers. On real
traffic, Jev's free-form evidence check had about 50% precision.

## Absorbing existing machinery

| Existing | Becomes |
| --- | --- |
| Required deliveries / directives (`delivery.go`): ack, progress, block, resume, result, epochs, receipts | Broker obligations. Same state machine and fencing, generalized to every obligating kind |
| v2 follow-through enrollment | Implicit: an obligating envelope *is* the enrollment |
| Follow-through check/report + `schedule_monitor.go` | Broker scheduler timers and wake outbox |
| Relay cursors and derived request IDs (`relay.go`) | Wake adapter leasing outbox jobs |
| Mandatory actions (v3 contract) | Obligations with `to: role:…`. The operator states map onto obligation states |
| Message audit associations / work-order refs | `refs` in the envelope |
| Narrative artifacts / chunked posts | `attachments` (artifact IDs) |
| Unread/`--mark-read` stop hook | Obligation-aware stop hook |

Kept as they are: work items, Queue (scheduling and claims), operational records
(typed records referenced from `refs`), project pause, roster and lifecycle.

## Migration

1. **Envelope in shadow** ([work order](broker-phase-1.md)). `tt send` and the hub validator ship. Agents may use either
   form, and the hub logs validator + Jev verdicts for free-text agent posts without
   rejecting. Prompts and templates switch to `tt send`. Exit criterion: a week of
   traffic where at least 90% of agent posts are valid envelopes.
2. **Broker obligations in parallel.** Obligations and timers run for envelopes and
   notify only (UI + Discord), while the directive core stays authoritative.
   Discord bridge v1 (mirror, status card, escalations, `/status`, `/stalled`,
   `/nudge`, `/say`) ships here. That lets the owner unstall immediately.
3. **Cutover.** Directive/delivery endpoints become compatibility shims over
   obligations, and in-flight directives migrate one-to-one with their epochs and
   receipts. The relay becomes a wake adapter. Obligation-aware stop hook.
   The remaining unstall commands follow.
4. **Enforce.** The hub rejects free-text agent posts. The Jev post gate moves from
   log-only to reject. Legacy endpoints and the schedule monitor are removed after
   one release with no shim traffic.

## Acceptance scenarios

- A message committed with its obligations survives a hub crash between commit
  and response. A retry returns the original receipt and creates no duplicate obligation.
- Wake adapter failure, ambiguous acceptance and relay restart. The obligation is
  re-woken on schedule and never skipped, including after a request-ID collision
  (the `relay-stall-6852` reproduction).
- Inbox retrieval without `tt ack` leaves the obligation `delivered` → `overdue_ack`
  → escalated to lead → escalated to Discord, with exact timer boundaries.
- A stale-run ack, a superseded-obligation result and an old-epoch progress each
  conflict with no side effects.
- Role recipient resolves to the current lead; lead replacement mid-obligation
  reassigns with retained history.
- Claude stop hook blocks with an unacked obligation and allows with none, and
  `stop_hook_active` prevents loops.
- The project-stall alert fires once per stall and clears on progress.
- Discord: duplicate interaction delivery, unauthorized user, command in the wrong
  channel, a reassign racing a result, and a bridge outage with backfill. All the
  scenarios from the integration proposal are included.
- Jev unavailable → posts accepted on deterministic validation alone. A planted
  injection is rejected while Jev is up.
- Free-text agent post rejected in phase 4 with an actionable error; human free
  text still accepted everywhere.

## Decided

- Turn-end blocks only on unacknowledged obligations (September 23, 2026).
- The Discord bridge is hosted on TrueNAS beside the hub (September 23, 2026).

## Open decisions

1. **Codex turn-end.** Check whether the installed Codex hooks can block turn
   completion. If they can't, Codex relies on timers and re-wake only.
2. **Timer defaults** above are starting values. Tune them from phase-2 shadow data.
3. **Discord setup.** Registering the bot, the test guild, the owner's Discord user
   IDs, and provisioning the bot token and bridge-scoped hub credential on TrueNAS
   are owner setup steps.
4. **Discord layout.** One channel per project (proposed) versus threads if many
   projects are expected. See the 50-channels-per-category limit in the platform findings.
