# Project roles

A Tailterm project has a few long-lived roles plus one fresh team per delivered
bug or feature. This page says what each owns and how work moves between them.
Owner decisions shape it: per-item teams with persistent records and deployment
roles (September 29), and the backlog steward (`wi_5b4b94dbc9a11e8b`, order
#14942).

| Role | Agent role | How many | Lifetime | Owns |
|---|---|---|---|---|
| Owner session helper | `owner_helper` | one per project, optional | the owner's own Claude Code session | the owner's conversation; relaying owner decisions; delegate of a delegation window; out-of-band help the owner asks for |
| Backlog steward | `backlog_steward` | one per project | persistent; rotated with a summary handoff | intake research and drafts; held follow-ups and Discord-filed items; batch, queue-order, scope and triage proposals; the backlog summary |
| Primary database handler | `database_handler` | one primary per project | persistent; [rotated](handler-rotation.md) | all work-item records: filing, work orders, scope confirmation, completion and acceptance records; readiness passes |
| Lane database handlers | `database_handler` (auxiliary) | as many as parallel lanes | persistent; leased to one queue entry at a time | the records of the one item leased to their exact run |
| Deployment agent | `deployment_agent` | one per project | persistent | consuming release jobs after project activation |
| Item team | ordinary agents bound to the item | one team per item | fresh identities per item; closed with `tt close --team` | planning, implementation, review and verification of one bug or feature |

## Boundaries

- **Owner session helper.** It is the owner's own session, so it talks with the
  owner and relays decisions. It is never leased, never a team member and never
  closed with an item team. See [Owner helper](owner-helper.md).
- **Backlog steward.** Reads the backlog directly (owner decision #15466) and
  writes no work-item records; it saves only its own backlog summary. Every
  record goes through the primary handler, and the hub refuses steward writes
  as an audit guard. It does no per-item records,
  merges, releases or acceptance, and never changes the queue itself. It is
  never leased, never a team member or item lead, and `tt team close` leaves it
  open. See [Backlog steward](backlog-steward.md).
- **Database handlers.** The only agents that write work-item records. The
  primary takes unallocated intake and project-level records; a lane handler
  works only on the item leased to its exact run. See
  [Handler allocation](handler-allocation.md).
- **Handler floor.** While the project is open and active, the hub keeps at
  least one database handler available. It refuses to retire or close the
  recorded primary or the last available handler without a ready successor, and
  alerts the owner once if an exit leaves none. See
  [Handler floor](handler-rotation.md#handler-floor).
- **Deployment agent.** Consumes release jobs the handler's saved acceptance
  enqueues. It does not accept items or change records. See
  [Project deployment](project-deployment.md).
- **Item team.** A lead, a planner, a builder, a reviewer and a verifier (the
  Planned delivery template), plus a plan reviewer on another model for a
  feature. A bug team has no plan reviewer. Each session serves exactly one item; a new item
  gets fresh identities. See [Team examples](team-examples.md).

## Questions

Nobody reads an agent's terminal, so an agent must never wait on a question
there (wi_2c46d9d964b335da).

- Every Claude Code agent that `tt spawn` starts (item team members, database
  handlers, the backlog steward, the deployment agent and helpers) runs with
  AskUserQuestion disallowed (`--disallowedTools=AskUserQuestion`).
- Agents ask the owner with `tt ask` (a Board decision) and a teammate with
  `tt send --kind question`. Every role template says so.
- The owner session helper is the owner's own session. `tt` does not spawn it,
  so its tools are unchanged.
- If a Claude selection dialog still appears in an agent's pane, the relay's
  runtime-prompt escalation posts a notice to the owner, with the item in its
  refs, and sends a directed copy to the item lead unless the lead is the one
  waiting.
- Sessions that are already running keep their launch flags and briefing until
  they are relaunched.

## Intake flow

1. The owner asks for something: in the owner session, helper chat, or Discord
   `/bug` and `/feature`. Agents find follow-ups outside their bound item, and
   review convergence files deferred blockers as held follow-ups.
2. **Owner session and helper chat** send the request to the steward with
   `tt send --to role:backlog_steward`. **Agents** do the same with their
   follow-ups; their briefings say so while a steward is active. **Discord**
   files a raw owner item directly. **Review convergence** files a held
   follow-up directly.
3. **The steward** researches each one: evidence or a reproduction, likely files
   and ownership, acceptance criteria, related and duplicate items. It asks the
   owner one question only when intent is unclear.
4. **The steward** sends the primary handler a typed REQUEST with the draft and
   the owner's source message. For a raw Discord item or a held follow-up, it
   sends a refinement or a dismissal proposal.
5. **The primary handler** files or updates the item through the normal intake
   path, with owner provenance (`--source-seq`) and a stable request ID.
6. **The steward** proposes batches, queue order, ownership scopes and triage
   outcomes as `tt ask` decisions. The owner answers, or the delegate under an
   open delegation window.
7. After an answer, **the steward** asks the handler to record scope and
   dismissals, and asks the owner (or delegate) to reorder the queue.
8. **The queue** leases a handler to the next ready entry and launches a fresh
   item team; the lead routes the work and the handler records its completion.
9. After acceptance, **the deployment agent** consumes the release job.

Without an active steward the flow is unchanged from before: intake goes to the
primary handler and the owner orders the queue.

## Who sends what to whom

| From | To | What |
|---|---|---|
| Owner session, helper chat | `role:backlog_steward` | new bug and feature requests |
| Any agent | `role:backlog_steward` | follow-ups outside its bound item |
| Any agent | its handler | scope changes to its bound item; Start, plan and result gates |
| Steward | primary handler | drafts to file; refinements and dismissals; scope records after a decision |
| Steward | owner (`tt ask`) | batch, queue-order, scope and triage proposals |
| Steward | owner or window delegate | queue reorder after a decision |
| Primary handler | steward | raw intake that reached the handler first |
| Handler | item lead | work orders and saved-record results |
| Handler (saved acceptance) | deployment agent | release jobs |
