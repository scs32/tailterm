# Broker phase 2a — obligations, acknowledgement and escalation

```text
ASSIGN: Make the hub own delivery, acknowledgement and outcome obligations
Refs: design docs/message-broker.md (phase 2); phase 1 docs/broker-phase-1.md
Objective: Every obligating typed message creates obligations the hub tracks until a typed
           outcome closes them, with deterministic timers that re-wake, then escalate.
Owns: see "Ownership"
Acceptance: b1–b14 below
```

Hub record: Feature `wi_07c6b8b7201ae27c`, owner intake #8771, work-order message #8772.

Status: released September 24, 2026 (hub and Mini `tt` at `5610db7`, see
[release evidence](releases/broker-phase2a-5610db7/README.md)). Written September 24, 2026 from
[the broker design](message-broker.md#obligations-and-the-broker-state-machine).
It builds on phase 1, which is deployed: typed messages and `tt send`
exist. Phase 2b, the Discord bridge on TrueNAS, is a separate
[order](broker-phase-2b.md).

## Why

Phase 1 measures message form; it prevents no stalls. Every recorded incident is
a message that was stored, correctly addressed, and then not acted on, with
nothing noticing. Phase 2a makes "read" and "acted on" durable, checkable
states with timers.

## Scope

In scope:

1. **Obligations table and state machine.** States: queued, delivered, acknowledged,
   working, blocked, closed. Closed carries an outcome: result, answered, declined,
   superseded or cancelled.
2. **Obligating kinds, created in the message's own transaction:**
   - `assign`, `request` and `review` require an ack, then an outcome.
   - `question` requires an answer.
   - A directed `human` message requires an ack, then an outcome.
   - `notice` and `finding` create delivery-only obligations.
3. **Closing through replies.** A typed reply closes the obligation it references (`refs.repliesTo`
   or `--reply-to`):
   - `result` closes assign/request/review.
   - `answer` closes question.
   - `decline` closes any obligation, with a reason.
   - `block` pauses one, with a resume condition.
4. **Worker commands:** `tt ack <seq>`, `tt obligations [--json]` and `tt progress <seq> [--text]`.
   Only the addressed agent's current run can ack or progress.
5. **Timers** from the design's defaults: wake retries at 1, 3 and 7 min; ack deadline 10 min;
   progress silence 30 min; outcome due (`due`, or 2 h / 30 min); lead escalation after 15 min.
   The broker scheduler runs inside the hub and is restart-safe.
6. **Escalation.** It creates hub-authored `notice` messages to the project lead,
   then to the owner. Discord delivery of owner escalations is phase 2b. Until then, owner
   escalations are board messages plus a `tt obligations --overdue` listing.
7. **Wake adapter.** The relay leases wake jobs from a broker outbox and reports
   accepted, failed or ambiguous, replacing its own cursor/request-ID derivation for
   obligations. Existing directed-message wakes keep working.
8. **Claude turn-end.** `tt hook stop` blocks turn end while the run holds obligations
   in delivered or overdue_ack, and never for overdue outcomes. The `stop_hook_active`
   guard is kept.
9. **Project stall signal.** The broker raises one `project_stalled` notice per stall when
   obligations are overdue and nothing has changed for 15 minutes.

Out of scope:

- Discord (phase 2b).
- Resolving `role:` recipients (phase 3).
- Migrating or removing the directive core. It stays authoritative in 2a, and obligations run
  beside it (design migration phase 2).
- Rejecting free-text agent posts (phase 4).
- Any Jev gating.

## Design notes

- **One row per recipient.** An obligation is one row per (message, recipient), created in the
  message's transaction. It is fenced by agent/run/epoch like the directive core; a stale-run
  action conflicts with no side effects.
- **Mutation receipts.** Obligation mutations use request IDs and receipts, as `tt send` does, so
  retries are safe.
- **Evidence rules.** A wake acceptance moves queued→delivered only; heartbeats, inbox reads and
  turn ends never acknowledge anything.
- **Timers use hub time.** The scheduler is a single goroutine per hub that claims due timers in
  a transaction. Missed deadlines after a restart fire once, not once per missed tick.
- **Escalations are typed messages** from the hub (kind `notice`, `refs.obligation`), so they
  flow through phase-1 checks and inboxes.

## Ownership

| Member | Owns |
| --- | --- |
| builder | `hub/internal/store/obligations.go` (new), migration, message insert hook, scheduler (`hub/internal/broker/`), server routes, `hub/cmd/tt` ack/obligations/progress and the stop-hook change, relay wake adapter, tests |
| planner | Confirms the plan against the directive core and relay code first |
| reviewer | Two review rounds per the policy |
| database | Records item, order, review outcomes, release |
| lead | Routing, acceptance, release disposition |

## Acceptance

| # | Criterion (observable) |
| --- | --- |
| b1 | A typed `assign` to an agent creates exactly one obligation in the same transaction; a retry creates none. A `notice` creates a delivery-only obligation; free-text agent posts create none. |
| b2 | `tt ack <seq>` by the addressed current run moves delivered→acknowledged. By any other agent or a stale run it conflicts with no state change. |
| b3 | A `result` replying to an assign closes it with outcome `result`. An `answer` closes a question. A `decline` closes with its reason. A `block` pauses, and its resolution resumes. |
| b4 | Inbox reads, heartbeats, relay wake acceptance and turn-end events never acknowledge or close an obligation. |
| b5 | With a fake clock: no ack → re-wakes at 1/3/7 min → `overdue_ack` at 10 min → lead escalation notice → owner escalation notice at +15 min. Each fires exactly once, including across a hub restart in between. |
| b6 | Progress silence of 30 min nudges twice, then escalates. An outcome past `due` escalates to the lead. |
| b7 | Reassignment supersedes the old obligation and creates a new one in one transaction, keeping history. |
| b8 | The relay leases wake jobs, reports accepted/failed/ambiguous, and a request-ID collision can no longer starve a recipient (the `relay-stall-6852` reproduction). |
| b9 | `tt hook stop` blocks a Claude turn with an unacked obligation, allows it with none, never blocks on overdue outcomes, and respects `stop_hook_active`. |
| b10 | One `project_stalled` notice per stall; it clears on progress and fires again only after a new stall. |
| b11 | `tt obligations` lists the caller's open obligations with state, due time and source message. `--overdue` lists the project's overdue ones for the owner. |
| b12 | Existing directive-core, delivery and relay tests pass unchanged. Free-text and human posts behave as before apart from the new obligations for directed human messages. |
| b13 | Migration is additive; the phase-1 hub binary still starts on the migrated database (rollback). |
| b14 | `go vet ./...`, `go test ./...` (known pre-existing failures excepted) and `npm test` pass; the relay stall reproduction passes. |

## Deployment and exit

Hub first (TrueNAS, verified backup), then `tt` on the Mini with a relay restart,
then TailOS if any UI changes. Exit: during the shadow week, every overdue obligation
is surfaced by an escalation within its timer, and no incident-class stall goes
unnoticed.
