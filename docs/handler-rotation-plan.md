# Handler rotation plan

Feature `wi_611e4d3c6992a664` revision 1, work order #11573, lead request #14229.
Planner `planner-6992a664` (read-only). Base: `tasks-hub` at `66b55ed`.

## Objective

Let the owner, or the host runner once a configured limit is reached, replace a
project's long-lived database handler with a fresh session that has the current
prompt. The fresh session takes over every open obligation and a durable
handoff, and the old session closes, all without losing or duplicating work.

## What exists today (read before building)

| Fact | Where |
| --- | --- |
| Handlers launch through `ensureHandler` with a stable `agt_` ID, a host journal and an owned tmux session `tt-handler-<id>`. A closed handler cannot be relaunched. | `hub/cmd/tt/handler_spawn.go` |
| The "primary" handler is implicit and inconsistent. Briefings treat the **oldest** open handler as primary and any other as an owner-provisioned *auxiliary* (`agentTaskBriefingForLaunch`). `role:database_handler` resolves to the **newest** open handler (`resolveRole`, `ORDER BY created_at DESC`). Queue dispatch leases the oldest free online handler. | `hub/cmd/tt/coordination.go:157-185`, `hub/internal/store/phase3.go:238-280`, `hub/internal/store/team_queue.go:938-980` |
| A fresh handler launched while the old one is open gets the **auxiliary** prompt, and would immediately win `role:database_handler`. Rotation has to fix both. | same |
| Lead replacement is the model to copy: `AssignLead` checks the expected revision and exact previous run, moves `role:lead` obligations with `handOffRoleObligations` → `reissueObligation`, posts a directed notice, writes an event and a keyed receipt, all in one transaction. | `hub/internal/store/lead.go`, `hub/internal/store/phase3.go:294-320`, `hub/internal/store/obligations.go:641-681` |
| Closing a handler that still holds obligations makes the broker close them as `recipient_gone`. That is the loss path. | `hub/internal/store/broker.go:130` |
| A team lease is a `team_queue_entries` row with `handler_id`/`handler_run_id`/`handler_lease_generation` in state `launching`, `running`, or `failed` with `released_at=''`. | `hub/internal/store/team_queue.go:353,891,975-980` |
| Pending scope confirmations are queued entries whose exact item/revision/order has no `work_order_scope_confirmations` row. | `hub/internal/store/work_order_scope.go` |
| "Finish an already-started atomic record" is only a prompt rule today; the hub has no record of it. Its observable signal is the activity monitor's exact-run state and pending tool. | `docs/handler-priority.md`, `hub/internal/store/activity.go` |
| Activity token totals are last-transition snapshots per exact run. Codex `input` includes cached tokens and Claude's does not, so `total` is the only runtime-neutral measure. Live example: Codex `db-handler` total 1.49B since 2026-09-24; one Claude builder item ~40M. | `docs/agent-activity.md`, `tt agents --json` |
| CLI name resolution for recipients is one function, `resolveAgent` (open agents only). It is also used by retire/resume. | `hub/cmd/tt/main.go:414`, callers in `send.go`, `obligations.go`, `retirement.go` |
| No prompt or template version exists anywhere. | grep for `templateVersion`/`prompt_digest`: none |
| Host runner: the relay calls `relayTeamQueueTick` every loop with a request budget and backoff. Dependencies are injectable for tests. | `hub/cmd/tt/relay.go:680-690`, `hub/cmd/tt/team_runner.go` |
| The hub has one shared credential. "Owner action" means a request with no agent identity; the refusal of agent callers is an audit rule, not authorization. | `hub/internal/server/lead.go` |

## Design in one paragraph

Add an explicit **primary handler** to the project (`tasks.primary_handler_id`
plus `handler_revision`; empty means today's legacy rules). A rotation is a
two-phase, keyed record. **Prepare** checks the old handler is idle (h2), then
stops new leases to it. The host CLI then launches the successor with
`ensureHandler`, using a preallocated ID and the saved launch settings.
**Commit** re-checks idleness, reissues every open obligation to the successor,
and snapshots the handoff. It makes the successor primary, records the
old→successor alias, posts one handoff notice, closes the old agent and writes
the receipt, all in one transaction. The CLI then stops the old session with
the existing cleanup receipt. The same routine runs from an owner command or
from a relay tick when the project policy says rotation is due.

## Files the builder owns

New:
- `hub/internal/api/handler_rotation.go`: request, response, policy, handoff and due-list types.
- `hub/internal/store/handler_rotation.go`: migration, policy, prepare/commit/abort/get, due list, handoff snapshot.
- `hub/internal/store/handler_rotation_test.go`
- `hub/internal/server/handler_rotation.go`: routes.
- `hub/cmd/tt/handler_rotation.go`: `tt handler rotate|rotation|policy`, saved launch spec, rotation routine, relay tick.
- `hub/cmd/tt/handler_rotation_test.go`
- `docs/handler-rotation.md`: operator doc (replaces this plan's design section when shipped).

Modified (only the parts named):
- `hub/internal/api/types.go`: `Task.PrimaryHandlerID`, `Task.HandlerRevision`, `Agent.SuccessorID` (read-only, set for rotated handlers), `AddAgentRequest.TemplateDigest`.
- `hub/internal/api/client.go`: client methods for the new routes.
- `hub/internal/store/migrate.go`: register the migration and add the task columns.
- `hub/internal/store/store.go`: `taskCols`/scan; `AddAgent` records the handler template digest for the exact run; reply routing to a rotated handler goes to its current successor (around line 1129).
- `hub/internal/store/phase3.go`: `resolveRole` for `database_handler` prefers the live primary.
- `hub/internal/store/team_queue.go`: dispatch skips a handler with an open rotation (as old or successor).
- `hub/internal/server/server.go`: route registration only.
- `hub/cmd/tt/coordination.go`: primary designation from `Task.PrimaryHandlerID`; extract the primary handler guidance into a constant; `handlerTemplateDigest()`; successor launch gets the primary prompt plus a line saying to wait for the handoff notice.
- `hub/cmd/tt/main.go`: command dispatch; `cmdSpawn` sends `TemplateDigest` for handler role; new `resolveRecipient` (forwards a closed rotated handler's name to its live successor) used for message recipients only.
- `hub/cmd/tt/send.go`, `hub/cmd/tt/obligations.go`: switch recipient lookup to `resolveRecipient`. `retirement.go` keeps `resolveAgent`.
- `hub/cmd/tt/relay.go`: call the rotation tick after the team queue tick, under the same budget/backoff.
- Existing tests that assert briefing text (`coordination_test.go`, `handler_allocation_test.go`) only where the primary rule changes.
- `docs/project-overview.md`: a short pointer entry.

Out of the builder's files: `client/**` (browser), `hub/internal/teamplan/**`, the deployment agent role, relay wake logic, and anything live.

## Ordered steps (each one reviewable on its own)

1. **Types and schema.** Add the API types. Add the migration with these tables:
   - `handler_rotation_policy(task_id PK, enabled, max_items, max_total_tokens, on_template_change, revision, updated_at)`
   - `handler_rotations(id, task_id, request_id UNIQUE per task, payload_hash, state prepared|committed|aborted, old_agent_id, old_run_id, successor_agent_id, successor_name, successor_run_id, reason manual|items|tokens|template, handoff_json, receipt_json, created_at, updated_at)`
   - `handler_runs(task_id, agent_id, run_id PK, template_digest, created_at)`

   Add the `tasks.primary_handler_id` and `tasks.handler_revision` columns. Allow at most one non-terminal rotation per task, enforced by a partial unique index.
2. **Primary designation.** Store: `resolveRole` and a new helper `primaryHandler(tx, task)` use the live primary when it is set, and legacy rules otherwise. CLI: the briefing uses the primary for "This project's Database handler is X" and the auxiliary/primary choice. Test the case where an auxiliary handler is older than a successor.
3. **Template digest.** `handlerTemplateDigest(assignment)` = SHA-256 of the static primary handler guidance, `queueHandlerAcceptanceBriefing()` and the saved assignment prompt. `cmdSpawn` sends it for handler role, and `AddAgent` writes a `handler_runs` row for that exact run. A legacy run has no row and counts as "changed".
4. **Policy.** `GET/PUT /v1/tasks/{id}/handler-rotation/policy` with an expected revision; a request carrying an agent identity is refused. CLI: `tt handler policy get|set`, which refuses to run inside an agent session.
5. **Prepare.** `POST /v1/tasks/{id}/handler-rotations` with operation `prepare`, a request ID, the expected handler revision, the old agent/run, a preallocated successor ID and name, and a reason. It refuses with 409 and a named reason (`live_lease`, `working`, `pending_tool`, `rotation_open`, `project_paused`, `not_primary`) and changes nothing. The successor name is the base name plus `-r<handler_revision+1>`. A retry with the same key and payload returns the same row.
6. **Lease exclusion.** Dispatch skips both agents of an open rotation. Prove that a queued entry is not leased to the old handler after prepare.
7. **Commit** (one transaction, keyed):
   - Re-run the h2 checks. If busy, return 409 and leave the rotation prepared.
   - Verify the successor is registered with role `database_handler`, is online, and has the same host/runtime/cwd as the old handler.
   - Reissue every obligation held by the old agent whose state is not `closed`, whether addressed by role or by name, with `keepRole`. Build the handoff JSON:
     - pairs of old and new obligation/message seqs
     - pending scope confirmations
     - live leases (must be empty)
     - open obligations the old handler authored
     - open allocation intents it authored
     - deliberate-Queue entries claimed by its run
     - current legacy required deliveries for its run (listed, not moved)
   - Set the primary to the successor and increment `handler_revision`.
   - Post one directed NOTICE to the successor that cites the rotation ID and `tt handler rotation get`.
   - Mark the old agent closed with an event, save the receipt, and commit.
8. **Abort.** Allowed only from prepared. It closes a registered successor, leaves the primary unchanged, and is keyed.
9. **Routing continuity.** `Agent.SuccessorID` comes from committed rotations. `resolveRecipient` follows the chain from a closed rotated handler's name to the live one. Hub reply routing to a closed rotated author goes to the live successor.
10. **CLI rotation routine** (`tt handler rotate`). Save the launch spec (spawn flags without the token) under `relayDir()` at 0600, keyed by hub and task. Keep a host journal of phases (`prepared` → `spawned` → `committed` → `cleaned`). Then prepare, run `ensureHandler(successor)`, wait until the successor is online (bounded), commit, stop the old owned session and write its cleanup receipt. Rerunning resumes from the journal and never spawns a second successor. With `--abort`, abort the rotation.
11. **Runner trigger.** `GET /v1/handler-rotations/due?host=H&templateDigest=D` returns, only for projects whose policy is enabled and whose primary handler is on H: the exact run, finished leased item count, `tokens.total`, whether the digest matches, and whether the handler is idle. The relay tick calls it once per loop and only after a policy is enabled on this host (cache a negative answer for 5 minutes). For a due and idle handler with a saved spec, it runs the step 10 routine. Due but busy: do nothing this tick. Due but no saved spec: post one owner notice per rotation-due episode, keyed so it is not repeated.
12. **Docs.** Operator doc, then the overview pointer.

## Acceptance criteria

"(IV)" marks criteria the distinct verifier must judge from its own matrix run. The reviewer reports those as pending-verification.

- **a1 (h1, IV). Owner rotation end to end.** Against a test hub with a private tmux socket or fake spawn, `tt handler rotate --task T <spawn flags>` produces:
  - a successor with role `database_handler`, a fresh `agt_`/`run_`, the old host/runtime/cwd, and the name `<base>-r2`
  - a successor briefing that contains the primary handler guidance, not the auxiliary sentence, and whose `handler_runs` digest equals `handlerTemplateDigest`
  - `Task.PrimaryHandlerID` equal to the successor and `HandlerRevision` incremented
  - the old agent closed, its owned session gone, and its cleanup receipt written
  - `tt handler rotation get` showing state `committed` with the handoff snapshot
- **a2 (h1, IV). No lost or duplicated obligations.** Seed the old handler with open obligations in every non-closed state (queued, delivered, acknowledged, working, blocked), addressed both by `role:database_handler` and by name, some carrying item links. After commit:
  - each one is closed once with outcome `superseded`
  - each has exactly one open reissued obligation on the successor, keeping `via_role` and its item links
  - none is closed `recipient_gone`
  - the count of open obligations on the old handler plus the successor is unchanged

  Replaying commit with the same request ID returns the identical receipt and creates no messages or obligations.
- **a3 (h1). Durable handoff.** The successor gets exactly one directed handoff NOTICE. The stored handoff lists each category from step 7 using seeded fixtures: reissue pairs, a pending scope confirmation, an authored open request, an open allocation intent, a claimed deliberate-Queue entry and a legacy required delivery. The live-lease list is empty. The hub restarts and the record reads back unchanged.
- **a4 (h1). Routing continuity.** After commit:
  - `role:database_handler` resolves to the successor, including when an auxiliary handler was created between the old handler and the successor
  - `tt send --to <old name>` and `tt post --to <old name>` deliver to the successor
  - `tt retire <old name>` is not forwarded
  - a RESULT replying to a request the old handler authored is addressed to the successor
  - a new worker briefing names the successor
  - the auxiliary handler's briefing still says auxiliary
- **a5 (h2, IV). Refusal while leased or working.** Prepare is refused with `live_lease` for each of these entries leased to the exact old run: `launching`, `running`, and `failed` with `released_at=''`. It is refused with `working` or `pending_tool` when the old run's activity is `working`, `hung_tool` or `looping`, or has a pending tool. When refused, nothing changes: no rotation row, no successor launch, no obligation moved. Commit re-checks: making the handler busy between prepare and commit gets a 409, the rotation stays prepared and the successor is not primary.
- **a6 (h2). Resumes when idle.** After the lease releases and activity returns to `idle`, the same `tt handler rotate` rerun, or the next runner tick, completes the rotation with no second successor and no second reissue.
- **a7 (h1/h2). Crash-safe and abortable.** Kill the routine after each journal phase (prepared, spawned, committed-before-cleanup). The rerun finishes with exactly one successor, one reissue set and one old-session cleanup. `--abort` from prepared closes the successor, keeps the old handler primary and open, and moves no obligations. After abort, a new `prepare` is allowed.
- **a8 (h3). Policy.** `tt handler policy set` persists enabled, max items, max total tokens and template-change with a revision. A stale revision is refused, and so is a request carrying an agent identity. `get` shows the owner-decided defaults.
- **a9 (h3, IV). Runner triggers.** With a saved spec and an enabled policy, one relay tick rotates the handler for each trigger tested alone:
  - the finished leased item count for the exact run reaches `max_items`
  - the exact-run `tokens.total` reaches `max_total_tokens`
  - the recorded digest differs from the current one, including a legacy run with no digest

  Just below each limit, nothing happens. Due but busy: nothing happens that tick, and the rotation succeeds on the first idle tick. Due with no saved spec: exactly one owner notice across several ticks, and no spawn. With the policy disabled, the tick makes no rotation request.
- **a10 (h3). Owner command overrides thresholds.** `tt handler rotate` rotates below every threshold, and still obeys a5.
- **a11 (h4, IV). Test suite and no live access.** All new tests use isolated SQLite, an `httptest` hub, `t.TempDir()` for `TAILTERM_RELAY_STATE`, and a private tmux socket or injected spawn. The following pass on the frozen SHA:
  - `go test ./internal/store -run HandlerRotation -count=1`
  - `go test ./cmd/tt -run 'HandlerRotation|Handler|Coordination|TeamRunner' -count=1`
  - `go test ./internal/... -count=1`
  - `go vet ./...`
  - `npm test`

  Grepping the new tests finds no hub URL, token or tmux default socket.
- **a12. Docs.** `docs/handler-rotation.md` describes the commands, policy, refusal reasons, handoff contents, rollback (the new columns and tables stay; an old binary ignores them) and the limits below.

## Owner decision: default rotation thresholds

Proposed for `tt ask`. Rotation fires on whichever limit is reached first.

| Option | Enabled | Max items | Max total tokens | Template change |
| --- | --- | --- | --- | --- |
| **A (recommended)** | on for new projects; the owner turns it on for existing ones | 10 finished leased items | 300M | on |
| B, conservative | same | 25 | 1B | on |
| C, prompt only | same | off | off | on |

Why A:
- The incident handler read 710M input tokens in about 2 days and now reads 1.49B, yet it still missed a prompt change. The template trigger alone fixes the incident; the count and token limits bound the context age.
- `total` is the only runtime-neutral measure: Codex counts cached tokens as input and Claude does not.
- A single Claude team member uses 1–40M tokens per item, so 300M is well above one item of handler work and below a day at the incident rate.
- Ten items keeps the startup cost to one fresh handler per ten items, which the owner accepted as a per-item cost.

## Risks and the check that exposes each

- **Primary rule changes break an existing auxiliary setup.** Exposed by the a4 auxiliary fixture plus the unchanged `handler_allocation_test.go`/`coordination_test.go` legacy cases, which must pass with `PrimaryHandlerID` empty.
- **Activity snapshot is stale.** The 120 s working window can miss a handler that just started a turn. Exposed by the a5 commit re-check test. Remaining limit: activity `unknown` is treated as idle only for the owner command. The runner requires `idle` or `finished_silent`. State this in the doc.
- **Reissue spam or loops for role-addressed work.** Exposed by the a2 replay and count invariants.
- **The relay request budget.** See the 2026-09-24 429 incident. Exposed by the a9 disabled-policy test, which asserts zero due requests, and one due request per tick when enabled.
- **Successor launched but never online.** Exposed by an a7 case: a fake spawn that never registers online produces a bounded wait, the rotation stays prepared, and `--abort` recovers.
- **Name forwarding misdirects a lifecycle command.** Exposed by the a4 retire case.
- **Legacy required deliveries stay pinned to the old run.** They are listed in the handoff, not moved. This is a known limit and needs a follow-up item if any are live.

## Out of scope

- Browser UI (Projects/handler setup) and `client/project-handler.js`.
- The deployment agent role.
- Rotating auxiliary handlers.
- Automatic resume of retired handlers.
- Moving legacy required deliveries.
- Any live hub, relay, queue or agent operation, install, deploy, push or merge.
- Rotating the live `db-handler`. That is a separate owner-ordered operation after release.
