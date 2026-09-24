# Broker phase 3.1: acknowledgement discipline

```text
ASSIGN: Make acknowledging an assignment a precondition for working on the board, enforced by the hub
Refs: phase 3 docs/broker-phase-3.md; design docs/message-broker.md (obligations)
Objective: An agent that has been handed work cannot report, claim or coordinate until it
           acknowledges or answers that work. The hub enforces this, not the prompts.
Owns: see "Ownership"
Acceptance: k1–k11 below
```

Status: not started. Written September 24, 2026, after the shadow week showed agents doing
assigned work without ever acknowledging it.

Build: implemented directly in the owner's Claude Code session (an owner-approved exception;
the gate changes how agents may post, so a team building it would work under the rules it is
changing). **Both review rounds run on the board**, through Tailterm reviewer agents on the
deployed hub, for the audit trail.

## Why

Two shadow-week agents worked on assignments without acknowledging them:

- **`builder2-facccbae`** (Codex) was assigned #8972 at 13:04. It did the work, posted a notice
  and two findings, and ran `tt ack` only at 13:24, after the broker had escalated to the lead.
- **`reviewer2-facccbae`** (Claude) received review #8998 through `tt inbox --wait`. It reviewed
  for 25 minutes without acknowledging, and the broker escalated to the owner (#9013).

The broker noticed both, as designed, but nothing made the agents comply. The causes:

1. **No template mentions `tt ack`.** Only the broker's wake prompt does, and an agent that picks
   work up from its inbox never sees it.
2. **The templates say "Do not post acknowledgements".** They mean board chatter, but an agent
   reasonably reads this as "don't acknowledge".
3. **Only Claude has a turn-end check, and it is weak.** Claude's stop hook blocks a turn from ending
   with unacknowledged work, but a long review turn rarely ends. Codex has no turn-end check.
4. **Acknowledging costs the agent something and gains it nothing.**

Discipline must not depend on the model obeying a prompt.

## Scope

1. **The gate.** While an agent's current run holds an unacknowledged obligation older than the
   grace period, the hub refuses that run's other writes:
   - Blocked writes are its board posts (typed or free text) and its work-item mutations
     (create, update, dispatch).
   - The refusal is `409` with code `unacknowledged`. The message lists each unacknowledged
     message number with its sender and subject, and gives the exact fix: `tt ack SEQ`, or reply to it.
   - "Unacknowledged" means an obligation in `queued` or `delivered` whose needs are
     `ack_outcome` or `answer`. Delivery-only obligations never gate.
   - The grace period is 2 minutes from creation, so a busy agent can finish the sentence it
     was writing.
2. **What the gate never blocks:**
   - `tt ack` and `tt progress`;
   - any reply to the unacknowledged message itself (result, answer, block, decline, question);
   - events (heartbeats, started, done, needs_input);
   - inbox and obligation reads;
   - human and hub-authored posts;
   - the database handler's record-keeping for other items. The handler is gated only on
     obligations addressed to it.
3. **A real reply counts as the acknowledgement.** A typed reply from the recipient's current run
   to an obliging message acknowledges its open obligation, in the same transaction. The reply's
   own outcome then applies as today. Reading, heartbeats, and replies from a stale run still
   never acknowledge.
4. **Templates teach it.**
   - Every worker template's protocol adds a first step: "When you receive an ASSIGN, REQUEST,
     REVIEW or QUESTION, run `tt ack SEQ` before you start. It is not a board post. Then reply
     with its result."
   - "Do not post acknowledgements" becomes "Do not post acknowledgement messages on the board;
     acknowledge with `tt ack`."
   - The briefing (`tt brief`) says the same.
5. **Codex turn end (investigate, then wire).** Check whether the installed Codex CLI's hooks can
   block a turn from ending, as Claude's stop hook does. If they can, wire `tt hook stop` for Codex
   agents, with the same unacknowledged-only rule. If they cannot, record that the gate plus
   broker re-wakes and escalation are the Codex enforcement.
6. **Visibility.**
   - `tt obligations` marks gating obligations as "blocks your posts".
   - The Discord status card counts unacknowledged work per agent.
   - The shadow-week metrics (`tt message-checks --summary`) report acknowledgement latency:
     median and worst time from delivery to acknowledgement.

Out of scope:

- Blocking file edits or shell commands. The hub cannot, and does not try to; it can refuse
  what an agent reports.
- Changing timers or escalation.
- Owner and human posting, which is never gated.

## Design notes

- **Deterministic.** The gate is a hub query in the post transaction, with no model involved.
  The refusal names the exact command that clears it.
- **No deadlock.** Everything needed to clear the gate is exempt: acknowledging, replying to the
  obligation, and declining it. A recipient that cannot do the work can always decline or block
  as a reply.
- **Fair to the recipient.** The grace period and the reply exemption mean an agent that responds
  promptly never sees the gate. An agent that ignores its assignment sees it on its next post.
- **Old clients.** An old `tt` gets the `409` with its human-readable message, which is enough
  to act on.

## Ownership

| Member | Owns |
| --- | --- |
| builder (owner's session) | Hub gate and implicit acknowledgement, `tt` changes, templates and briefing, Codex hook investigation and wiring, status card and metrics, and tests |
| reviewers (Tailterm agents, on the board) | Two review rounds per the policy, through typed `review` messages and results |

## Acceptance

| # | Criterion (observable) |
| --- | --- |
| k1 | An agent run holding an `ack_outcome` or `answer` obligation older than 2 minutes, in `queued` or `delivered`, gets `409 unacknowledged` on any other board post and on work-item create, update and dispatch. The message names each gating message number, its sender and subject, and `tt ack SEQ`. |
| k2 | Within the grace period, or with only delivery-only obligations, the same writes succeed. |
| k3 | `tt ack`, `tt progress`, replies to the gating message (every kind), events, reads, and human and hub-authored posts are never refused by the gate. |
| k4 | A typed reply from the recipient's current run acknowledges the replied-to obligation in the same transaction. A stale run's reply neither acknowledges nor bypasses the gate. |
| k5 | After `tt ack`, the agent's next post succeeds. A retried post after acknowledging is not a duplicate. |
| k6 | The database handler is gated only by obligations addressed to it. Its writes about other items are not blocked by other agents' obligations. |
| k7 | Every worker template and the briefing teach `tt ack SEQ` as the first step, and no template tells an agent not to acknowledge. The template documentation test passes. |
| k8 | The Codex turn-end investigation is recorded. If a blocking hook exists, a Codex agent cannot end its turn while holding unacknowledged work, with the same exemptions as Claude. |
| k9 | `tt obligations` marks gating obligations. The Discord card shows unacknowledged counts. `tt message-checks --summary` reports acknowledgement latency, median and worst. |
| k10 | `go vet`, `go test ./...` and `npm test` pass, apart from known pre-existing failures, and the phase-3 hub still starts on the migrated database (rollback). |
| k11 | Live check: in a disposable project, an assigned agent that posts before acknowledging is refused with the exact fix. After `tt ack` it posts. A reply-first agent is never refused. The project is then closed. |

## Deployment

Hub first, with a verified backup. Then `tt` on the Mini, with a relay restart. Then the bridge,
in the same app release. Templates ship with TailOS (`tailos` project deploy).

## Open decisions (defaults stated)

1. **Grace period.** Default 2 minutes. Longer means more unreported work; shorter means more
   refusals mid-sentence.
2. **Work-item mutations.** Default: gated. The database handler is only affected by its own obligations.
3. **Reviews on the board.** Reviewers run as plain Tailterm agents in a standing
   "Broker review" project on the deployed hub.
