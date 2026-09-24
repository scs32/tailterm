# Broker phase 3: cutover to obligations

```text
ASSIGN: Make broker obligations the only follow-through mechanism, and finish the owner's controls
Refs: design docs/message-broker.md (migration step 3); phase 2a docs/broker-phase-2a.md; phase 2b docs/broker-phase-2b.md
Objective: Retire the legacy required-delivery follow-through, resolve role recipients, give
           the owner the remaining unstall controls, and fix what the first shadow-week traffic exposed.
Owns: see "Ownership"
Acceptance: c1–c16 below
```

Hub record: Feature `wi_6329a9f143468664`, owner intake #8960, work-order message #8961.

Status: built on tasks-hub, in review (not deployed). Written September 24, 2026 after phases 1, 2a and 2b were released,
and after the first shadow-week item ran through a Planned delivery team.

## What changed from the design

The design (migration step 3) assumed live directives would need compatibility shims and
a one-to-one migration "with their epochs and receipts". Production shows that is not needed:

- **Nothing creates deliveries on its own.** The only way to create a required delivery is
  an explicit `tt delivery create`, and no prompt or template tells an agent to run it.
  The last delivery was created on September 15.
- **Nothing live is left to move.** Production has 107 required deliveries: 91 with a
  result, 7 superseded, 6 blocked and 3 still progressing. All 9 open ones belong to agents
  that are already closed (`lead-recovery`, `queue-phase-qa-mini`), in Tailterm
  Development, which has been paused since September 17.
- **The legacy lead escalation does nothing on its own.** It fires only while a Codex relay
  polls `follow-through/check`; the hub has no timer for deliveries.
- **Operational records are unused.** The operational-record tables have 0 rows, but
  committing a record still requires a `dly_` delivery (`store/operational_records.go`).
- **The schedule monitor watches Queue stalls, not deliveries.** It posts free-text directed
  notices that create no obligation, and the Discord bridge renders them as "owner". It last
  posted on September 15.
- **The other obligation systems are not on `tasks-hub`.** `lead_disposition_obligations`,
  `phase_successor_obligations` and `cleanup_obligations` exist only on side branches. The
  production database has 5 stale `lead_disposition_obligations` rows from those builds.

So phase 3 **retires** the legacy path rather than shimming it:
- new deliveries are refused;
- the stale ones are closed with provenance;
- the relay's follow-through path is removed;
- history stays readable for audit exports.

Legacy tables and read endpoints are removed in phase 4, after one release with no legacy traffic.

## Scope

In scope:

1. **Freeze the legacy writers.** `POST /v1/tasks/{id}/required-deliveries` and the
   follow-through check and report routes return `410 Gone`, with a message that names
   `tt send` and obligations. `tt delivery create` says the same without calling the hub.
   Reads stay available: current-assignment, coverage, and delivery history in audit exports.
   Capabilities keep advertising the versions, so old and new `tt` keep reading, and add
   `writesRetired: true`. Every remaining delivery write (ack, progress, block, resume,
   result, incident, resolve) also answers 410, since no delivery is left open.
2. **Close out the stale deliveries.** A one-time, idempotent migration closes every
   `current=1` delivery that is not in `result`. It records its own terminal state with a
   delivery event naming the phase-3 retirement and the recipient's status. It also closes the
   5 stale `lead_disposition_obligations` rows the same way, if the table exists. It writes a
   board notice in each affected project listing the work items those deliveries referenced,
   so nothing is silently dropped when Tailterm Development resumes. Paused projects stay paused.
3. **Remove the relay's legacy path.** Delete `relayFollowThrough` and its status fields. The
   relay becomes the broker wake adapter plus the inbox wake for non-obligating directed
   messages. Also fix phase-2a follow-up R2: a directed owner message that created an
   obligation is woken once, by the broker, never also by the inbox path.
4. **Retire operational-record writes along with deliveries** (changed while building). The
   original plan re-anchored them on obligations. But their data carries a delivery ID,
   generation and epoch, and every record kind is gated on a delivery phase, so re-anchoring
   would mean redesigning a feature with no records in production. Propose and commit now answer
   410; reads stay available. Knock-on effect: the pause dialog's `transferred` and `detached`
   service dispositions need an operational `result` record as evidence, so they are unavailable
   until that evidence is based on the target run's typed result message instead (a follow-up).
   No pause has ever used them.
5. **Resolve role recipients.**
   - `role:lead` resolves to the project orchestrator's current agent, and
     `role:database_handler` to the project's current handler. Resolution happens at post time,
     in the message transaction. The resolved agent is stored as the message's `to`, and the
     envelope keeps the role.
   - An unresolvable role is refused with an actionable error.
   - `tt send --to role:lead` is allowed.
   - When the lead is replaced, the outgoing lead's open obligations that came from `role:lead`
     are reassigned to the new lead, with history kept.
6. **Owner controls (hub endpoints, `tt`, Discord):**
   - **extend** moves an obligation's due time, recorded with owner provenance and the new due time.
   - **answer** lets the owner close a question or block on the recipient's behalf. It posts a
     `human` answer that settles the obligation.
   - **cancel** closes an obligation with outcome `cancelled` and a reason.
   - **resume** re-enables a retired agent through a narrow owner route
     (`POST …/agents/{aid}/resume`), rather than giving the bridge general agent edits.
   - Discord commands: `/extend`, `/answer`, `/cancel` and `/resume`, plus an **Extend 30m**
     button on owner escalations and `/stalled` rows. The bridge's route allowlist grows to
     exactly these routes. **`/pause` and `/unpause` were dropped** (changed while building): a
     project pause takes an explicit list of every live run to stop, and resuming needs a freshly
     launched lead that a host must spawn. Discord cannot drive either, so both stay in TailOS.
7. **Make the schedule monitor's notices typed.** Its Queue-stall notices become `notice`
   envelopes with `refs.queue`. They keep their `system/schedule-monitor` provenance, and the
   bridge renders any `system/*` sender by name instead of as "owner". Whether to fold Queue stalls into broker timers is decided in phase 4.
8. **Shadow-week fixes not already filed as bugs:**
   - A BLOCK that is not a reply notifies its recipient; it obliges only when sent to the
     project lead or the owner. A worker told to "wait" by a BLOCK is therefore not escalated.
     Filed bug B is the prompt-side fix.
   - The Discord bridge renders any other `system/*` sender by its node name.

Out of scope:
- Rejecting free-text agent posts, and moving Jev from log-only to a gate (phase 4). Jev's
  readability concern currently fires on 61% of valid typed posts and must be recalibrated
  before any gating.
- Removing the legacy tables, read routes or the schedule monitor (phase 4).
- A Claude wake adapter, which the Opus 5.5 lead needs. This is a separate order.
- The shadow-week bugs already filed:
  - A: escalation subjects drop item-scoped agent names;
  - B: templates use BLOCK for waits;
  - C: Add team leaves the lead on the base name.
- Launch-path gaps: a CLI-created project has no database handler.
- **Item revision binding** (found September 24 on bug A): recording a wider file scope moved
  the bug from revision 1 to 4. That froze every admitted worker at revision 1, and a
  fresh builder and reviewer had to be launched. This needs a supported way to refresh a
  worker's binding in place, which is a separate order on work-item admission.

## Design notes

- **One follow-through mechanism.** After this phase, obligations and broker timers are the
  only thing that wakes, nudges or escalates. Legacy rows remain as history only.
- **Refusal instead of silent drops.** Every retired writer answers `410` with a pointer to
  its replacement, so an old `tt` or a stale prompt fails loudly.
- **The close-out is data, not deletion.** Each retired delivery gets a terminal event with
  provenance (`retired-phase3`), and its referenced work items are listed on the board.
- **Role resolution is not addressing by name.** An envelope keeps `role:lead`, so a reader
  sees the intent. The obligation is held by the agent that was lead when the message was
  posted, and moves if the lead changes.
- **Owner actions carry provenance.** extend, answer and cancel record who acted and why, in
  the same receipt style as ack and progress, and repeat safely with request IDs.

## Ownership

| Member | Owns |
| --- | --- |
| builder | Hub: legacy route freeze, close-out migration, operational-record anchor, role resolution and lead-change reassignment, owner extend/answer/cancel, typed monitor notices, BLOCK obligation rule. `tt`: delivery refusal, relay follow-through removal, R2, `send --to role:`. Bridge: new commands and allowlist, system sender rendering. Tests for all of it |
| planner | First confirms the legacy callers against the map in this order, and the migration's exact row selection on a copy of the production database |
| reviewer | Two review rounds per the policy |
| database | Records the item, order, review outcomes and release |
| lead | Routing, acceptance, release disposition |

## Acceptance

| # | Criterion (observable) |
| --- | --- |
| c1 | Creating a required delivery (HTTP or `tt delivery create`) and the follow-through check and report routes answer 410 with a pointer to `tt send`. Current-assignment and coverage reads still work, and audit exports still include delivery history. |
| c2 | On a copy of the production database, the close-out migration closes exactly the 9 open deliveries and the 5 lead-disposition rows. Each gets a `retired-phase3` event. Running it again changes nothing, and each affected project gets one board notice listing the referenced work items. |
| c3 | The relay has no follow-through path; `tt relay --status` shows broker wakes and inbox wakes only. The existing broker and inbox relay tests pass. |
| c4 | (R2) A directed owner message wakes a Codex recipient exactly once, through its obligation's wake job. A directed result or answer, which creates no obligation, still wakes it through the inbox path. |
| c5 | Proposing or committing an operational record answers 410 (HTTP), and `tt operational-record propose/commit` refuses without calling the hub. Reads still work, and no template tells an agent to use them. |
| c6 | `to: role:lead` creates an obligation for the current orchestrator agent, and `role:database_handler` for the current handler. An unresolvable role is refused with a message naming the role. `tt send --to role:lead` works. |
| c7 | Replacing the lead reassigns the outgoing lead's open `role:lead` obligations to the new lead in one transaction, with superseded history. Obligations addressed to the old lead by name are untouched. |
| c8 | extend moves `due_at` and records the owner and reason. A retry with the same request ID returns the original result, and extending a closed obligation conflicts. |
| c9 | answer posts a `human` answer that closes a question or a block obligation with outcome `answered`, and the recipient's next wake sees it. cancel closes any open obligation with outcome `cancelled` and a reason. Both are idempotent by request ID. |
| c10 | The bridge's `/extend`, `/answer`, `/cancel` and `/resume`, and its Extend 30m button, work through hub calls with request IDs. They pass the same owner, guild and channel checks, and stale-state checks, as the phase-2b commands. The bridge token reaches exactly the added routes. |
| c11 | A schedule-monitor Queue-stall notice is a typed `notice` with `refs.queue`. The bridge names `system/*` senders by name, never "owner", and the existing monitor tests pass. |
| c12 | A BLOCK sent (not as a reply) to a worker notifies it without an ack obligation. A BLOCK to the project lead or to the owner obliges as before, and a BLOCK reply still pauses the obligation it replies to. |
| c13 | Nothing in the hub, `tt`, the client or the templates tells an agent to use `tt delivery` or `tt current-assignment`. Capabilities mark reliable delivery and operational records `writesRetired`. |
| c14 | Migrations are additive apart from the close-out rows, and the phase-2b hub (`d6d5155`) still starts on the migrated database (rollback). |
| c15 | `go vet ./...`, `go test ./...` and `npm test` pass, apart from known pre-existing failures. The legacy tests are either removed together with the code they cover, or kept where they cover read paths that remain. |
| c16 | Live check on production: a disposable project exercises `role:lead`, owner extend, answer and cancel from Discord, and a BLOCK-as-wait that does not escalate. The project is then closed. |

## Deployment and exit

1. **Hub first,** with a verified backup. The close-out migration runs on start, and its
   result is recorded in the release evidence.
2. **Then `tt` on the Mini,** with a relay restart. Watch for any 429 or error burst, as
   after the phase-2a incident.
3. **Then the bridge,** which ships in the same app release.

Exit: one week with no legacy delivery writes attempted, no owner escalation caused by a
BLOCK-as-wait, and every escalation from the week handled from Discord.

## Open decisions (defaults stated)

1. **BLOCK obligation rule (c12).** Default: a non-reply BLOCK obliges only the lead or the
   owner. The alternative is to rely on the template fix (bug B) alone and keep the phase-2a rule.
2. **Paused Tailterm Development.** Default: leave it paused. The close-out notice lists its
   referenced work items for when the owner resumes it.
3. **Queue stalls.** Default: keep the schedule monitor, with typed notices, in phase 3. Folding
   it into broker timers is a phase-4 decision.
