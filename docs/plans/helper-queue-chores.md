# Plan r2: the product absorbs the helper's queue chores

Feature `wi_de078c0ecd9c846b` revision 1, work order #14809 (project `tsk_e7af3c28a444b09a`).
Planner: planner-cd9c846b, for lead request #15188 (location per #15189). r2 answers lead
REQUEST #15216 on review #15215. r1 was sha256 `3e81e1ef…` (RESULT #15204).
Host Stephens-Mini, worktree `/Users/stephenspeicher/projects/tailterm/.build/worktrees/queue-de078c0e`.
Queue entry `tqe_17a8c1d391f7da58`, frozen base `ef117cf`. Plan base: `c6a8ec1` (lead #15195),
which descends from `ef117cf`.

## Changes in r2

The numbering a1–a12 is unchanged. Items that changed are tagged **[r2]** below.

- **b1:**
  - `phase3.go` `resolveRole` keeps resolving the item's non-finished entry, whether released
    or not.
  - `allocation_intent.go` is dropped.
  - The host-capacity polls and `TeamQueuesByHost` keep including released entries that still
    need finishing.
  - a4 and a11 assert that `role:database_handler` sends still work.
- **b2:** a failed entry with an owner-integration record now has a defined end state. The
  runner closes the team and cleans it up, and the hub's `finish` marks the entry `finished`.
  a11 asserts this end state.
- **b3:**
  - The handler briefing gets a plain-done sentence for owner-integrated entries, and a10
    asserts it.
  - The done-save refusal names the integration record.
  - The `handlerTemplateDigest` impact is documented.
- **b4:**
  - New cause `idle-entry`.
  - Slot fullness is computed without counting stall blockers.
  - New a6 cases for both.
- **b5:**
  - `integrated` is refused while a release job for the entry is not terminal.
  - New a4 negative.
  - Risk 8 is rewritten.
- **f1:** the field is renamed `owner_integration_json` / `OwnerIntegration`.
- **f2:** `Since` is defined and durable.
- **f3:** narrowing a3 is described as a close-window optimization, and it uses the merge-base
  diff.
- **f6:** `integrated` is allowed in serial projects too; the effect is documented.
- **f7:** the docs say that post-release fixes become new items.
- **Unchanged:** f4 and f5 stay follow-ups.
- **Owned files:** adds the triage tests, `docs/message-broker.md` and the lead's additions.

## Objective

Turn five recurring owner-session chores into product behavior:

- queue-time ownership from intake (c1);
- queue entries that give way once integrated (c2);
- a one-shot stall explainer, and no owner escalation for blocks that wait on a queued fix (c3);
- read-only backlog triage suggestions (c4).

Document the new split of chores and update the templates (c5). Tests cover all of it,
including the 2026-09-29 deadlock (c6).

## What exists today (read before building)

- **Queue store.** `hub/internal/store/team_queue.go` has `TeamQueueAction`, `ListTeamQueue`,
  `queueReleaseSafe`, `pendingTeamQueueAcceptance` and `doneSaveQueueGate`.
  `team_queue_scope.go` has `canonicalQueueOwnership` and `queueEntryConflicts`.
  - An entry holds a slot, a handler lease and its ownership when
    `state IN ('launching','running') OR (state='failed' AND released_at='')`.
  - That condition is repeated in `team_queue.go` and `handler_rotation.go:275`, and in the
    Go loops of `ListTeamQueue` and `claim`.
  - `phase3.go:249` (`resolveRole`) uses the same text for a different purpose: routing a
    `role:database_handler` message to the item's handler.
- **Runner.** `hub/cmd/tt/team_runner.go` has `tick`, `advance`, `releaseFailed` and `finish`.
  - `tick` skips an entry that is failed and released (lines 166–181).
  - `releaseFailed` needs every item-bound run to be closed and cleaned first.
  - `finish` waits for a terminal item and, for repository entries, for handler acceptance.
  - Member cleanup only happens inside `finish`.
  - The hub's `finish` op requires `state='running'` (`team_queue.go:1311`).
- **Team close.** `store/team_close.go` `CloseItemTeam` refuses a non-terminal item, and
  `tt close` on an item lead is refused (`cmd/tt/main.go:1328`). Together with the release
  rule, this is the 09-29 deadlock: day log `.build/day-2026-09-27.md` 12:0x–12:5x, #14714,
  #14740, #14751.
- **Acceptance.** Acceptance happens only inside a done save, or in `accept` once the item is
  done, and `enqueueAcceptedRelease` then creates a `release_jobs` row in state `verified`.
  - Release-job states: `verified` → `claimed` → `merged` → `released` or `rolled_back`, with
    `blocked` as a fence state.
  - Terminal states: `released` and `rolled_back`.
- **CLI.**
  - `hub/cmd/tt/team_queue.go` has `add`, `scope`, `fail`, `release` and the list output.
  - `team_scope.go` has `queueRepositoryScope`, `verifyAcceptedGit` and `acceptanceBase`.
- **Intake.** `tt work-items scope confirm` (`store/work_order_scope.go`) is the handler's
  assertion that the filing has acceptance and owned files.
  - It stores no paths.
  - There is one confirmation per (item, revision, order).
  - `team queue add` already requires it.
- **Broker.** `hub/internal/broker/broker.go` `Tick` escalates overdue obligations.
  - The lead is level 1 and the owner is level 2; a lead-held obligation goes straight to
    level 2.
  - A BLOCK reply sets the obligation to `blocked` (`store/obligations.go:212`), but the
    overdue escalation still happens.
- **Templates.**
  - The lead text lives in `client/team-examples.js`, and `hub/internal/teamplan/plan.mjs` is
    generated from it (`npm run build:team-plan` and `npm run check:team-plan`).
  - The handler's queue briefing is `queueHandlerAcceptanceBriefing` in
    `hub/cmd/tt/coordination.go:155`. It is part of `handlerTemplateDigest`
    (`coordination.go:166`), which handler rotation compares when `OnTemplateChange` is on.

## Owned files [r2]

The entry was narrowed by the lead (#15206, #15216). It now holds:

```
hub/internal/api/team_queue.go          hub/internal/api/work_order_scope.go
hub/internal/api/triage.go (new)        hub/internal/triage (new package)
hub/internal/store/team_queue.go        hub/internal/store/team_queue_scope.go
hub/internal/store/team_queue_stall.go (new)
hub/internal/store/team_queue_chores_test.go (new)
hub/internal/store/work_order_scope.go  hub/internal/store/work_order_scope_test.go
hub/internal/store/broker.go            hub/internal/store/triage.go (new)
hub/internal/store/triage_test.go (new) hub/internal/store/migrate.go
hub/internal/store/migrate_test.go      hub/internal/store/store.go
hub/internal/store/handler_rotation.go  hub/internal/store/phase3.go
hub/internal/store/team_host_capacity.go
hub/internal/broker/broker.go           hub/internal/broker/broker_test.go
hub/internal/server/server.go           hub/internal/server/triage.go (new)
hub/internal/server/triage_test.go (new) hub/internal/server/team_queue.go
hub/internal/server/work_order_scope.go hub/internal/server/work_order_scope_test.go
hub/cmd/tt/team_queue.go                hub/cmd/tt/team_queue_test.go
hub/cmd/tt/team_runner.go               hub/cmd/tt/team_runner_chores_test.go (new)
hub/cmd/tt/team_scope.go                hub/cmd/tt/work_items.go
hub/cmd/tt/triage_test.go (new)         hub/cmd/tt/coordination.go
hub/cmd/tt/main.go                      hub/cmd/tt/handler_rotation_test.go
client/team-examples.js                 hub/internal/teamplan/plan.mjs
tests/team-examples.test.js
docs/team-launch.md                     docs/team-examples.md
docs/owner-helper.md                    docs/message-broker.md
docs/plans/helper-queue-chores.md (this plan, if committed)
```

`allocation_intent.go` is dropped: it only reads `item_team_leads`. Two store files are
touched only as the step 1 audit says:

- `handler_rotation.go`: the slot and lease predicate.
- `team_host_capacity.go`: no predicate change (see step 1). Listed only in case a count needs
  the helper.

## Steps (each small enough to review on its own)

1. **One "holds queue resources" predicate, no behavior change. [r2]**
   - Add `queueEntryHoldsResources(e)` in Go and one SQL fragment constant in
     `team_queue_scope.go`: `state IN ('launching','running','failed') AND released_at=''`.
     This is identical today, because only failed entries get `released_at`.
   - Use it only where an entry holds a slot, a handler lease or ownership:
     - `ListTeamQueue`: the active count, `freeQueueHandler` input, `BlockedBy` and
       failed-entry reasons;
     - `set_limit` counts;
     - the `claim` active set;
     - the `scope` overlap query;
     - `handler_rotation.go:275`.
   - Do **not** use it in the following places:
     - `phase3.go:249` `resolveRole`: change it to the item's newest entry with
       `state NOT IN ('queued','finished')`, released or not. A released but live team can
       still address `role:database_handler` with its item link, and the handler that served
       it answers.
     - `team_host_capacity.go:135` polls and `TeamQueuesByHost`. Those must keep returning
       entries the runner still has to finish (see step 5).
   - Existing tests must pass unchanged.
2. **Schema and API (additive only). [r2 names]** Add these with `ALTER TABLE … ADD COLUMN`,
   in the existing idempotent style:
   - `team_queue_entries.serial INTEGER NOT NULL DEFAULT 0`
   - `team_queue_entries.owner_integration_json TEXT NOT NULL DEFAULT ''`
   - `work_order_scope_confirmations.ownership_json TEXT NOT NULL DEFAULT '[]'`

   Add these API fields:
   - `TeamQueueEntry.Serial`
   - `TeamQueueEntry.OwnerIntegration *TeamQueueOwnerIntegration{Commit, BaseCommit, Evidence, ChangedFiles, At}`
     (kept distinct from the existing `Integration *TeamIntegrationReady`, which is the
     "Ready to integrate" snapshot)
   - `TeamQueueEntry.Stall *TeamQueueStall{Cause, BlockerEntryID, Fix, Since}`
   - `TeamQueueEntry.UpdatedAt`
   - `TeamQueueRequest.Serial`, `.OwnerIntegrationCommit`, `.ChangedFiles`
   - `ConfirmWorkOrderScopeRequest.Ownership` and `WorkOrderScopeConfirmation.Ownership`
3. **c1: ownership from intake.**
   - `tt work-items scope confirm … --owns PATH` is repeatable. The store canonicalizes the
     paths with `canonicalQueueOwnership`, saves them, and includes them in the retry hash.
     The GET route returns them.
   - `tt team queue add` without `--owns` reads the confirmation for the item's current
     revision and `--order`. It validates the paths with `queueRepositoryScope` before it
     creates any worktree, then sends them.
   - New `--serial` flag.
     - In a parallel project, an add with no `--owns`, no intake ownership and no `--serial`
       is refused before any worktree exists. The error names all three fixes.
     - The hub also refuses an empty-ownership add without `Serial` when the limit is not 1.
     - A serial project (limit 1) saves `serial=1` implicitly, so its behavior is unchanged.
   - The list prints `owns=serial (runs alone)`, or `unscoped (legacy)` for old entries.
   - `scope` keeps working on queued, launching and running entries.
4. **c2a: narrow after acceptance. [r2 f3]**
   - Acceptance happens only once the item is done, so this narrowing only matters while the
     team is closing and cleaning up. Post-release checks on an open item rely on step 5.
   - For a running entry with `Acceptance` set, the runner computes
     `git --git-dir=<Repository> diff --name-only --no-renames <Acceptance.BaseCommit>...<Acceptance.Commit>`.
     The three-dot form is a merge-base diff, so a branch that merged tasks-hub is not charged
     with tasks-hub's files.
   - It keeps only the changed files under the declared ownership; a serial entry keeps them
     all.
   - If that set is non-empty and differs from the current ownership, it sends one owner
     `scope` (request `queue-narrow-<entry>-<revision>`).
   - A git error or an empty set is logged and skipped, and never fails the entry.
5. **c2b: give way once integrated. [r2 b2, b3, b5, f1, f6]**

   New owner-only operation
   `tt team queue integrated --task T --entry E --commit SHA [--evidence TEXT]`, hub op
   `owner_integrated`, run from an unbound CLI.
   - **CLI:** checks that SHA is in the frozen repository and descends from the entry base,
     then computes changed files as in step 4 (merge-base diff).
   - **Hub accepts** a `running` entry, or a `failed` entry with no `released_at`.
   - **Hub refuses:**
     - queued, launching, finished or already-released entries;
     - agent callers;
     - failed entries with uncertain launch members (the existing `release` proof path
       applies to those);
     - **[b5]** any entry that has a `release_jobs` row in a non-terminal state (`verified`,
       `claimed`, `merged`, `blocked`). The message is
       `release job REL is STATE; the deployment path owns this candidate`.
   - **One transaction:**
     - store `owner_integration_json`;
     - set ownership to the changed files, or keep the old list if none changed;
     - set `released_at`;
     - delete the launch reservation;
     - leave untouched the state (`running` or `failed`), the item and its revision, the
       bindings, the `item_team_leads` row and the `tasks.orchestrator`.
   - **Done save [b3].**
     - `pendingTeamQueueAcceptance` skips released entries.
     - `doneSaveQueueGate` refuses a save that carries acceptance flags for an item whose
       entry has an owner-integration record, with
       `entry E was integrated by the owner at SHA; save done without --worktree/--branch/--commit`.
     - A plain done save from any available handler is accepted. The lease has ended.
   - **Finishing [b2].** A released entry with an owner-integration record, whether running or
     failed, still gets finished.
     - `TeamQueuesByHost` also returns failed entries that have `owner_integration_json<>''`.
     - The runner's `tick` sends those entries to `finish` instead of skipping them. It does
       not use that project's serial turn for them (`continue`, not `break`).
     - Runner `finish`: when the item is terminal, it freezes and runs the team close (the
       item lead's exact snapshot, as today), cleans up every member on the host, then sends
       hub `finish` with no integration snapshot.
     - Hub `finish` accepts `state IN ('running','failed')` when the owner-integration record
       exists. It skips the reservation and acceptance checks for such an entry, keeps the
       team-close and cleanup receipt checks, and sets `state='finished'`. `OwnerIntegration`
       is kept and `Integration` stays nil.
     - The end state is `finished`, with `releasedAt` and `ownerIntegration` set.
   - **Serial projects [f6].** `integrated` is allowed. It records the integration and frees
     the reservation and ownership, but the next serial launch still waits for the project
     lead slot. That slot frees at the lead's team close, which needs the item to be terminal.
     The docs say this.
   - **Scoping failed entries.** Owner `scope` is now allowed on a failed, unreleased entry,
     narrow-only: each new path must lie under an old path.
6. **c3: stall explainer. [r2 b4, f2]** Lives in `ListTeamQueue`, via the new
   `team_queue_stall.go`.
   - **Stall blockers** are resource-holding entries of these kinds:
     - `failed-entry`: a failed, unreleased entry. This includes a parallel one with live runs.
       One with no live runs is not a stall, because the runner releases it on its next pass.
     - `nothing-running`: a launching or running entry with no live (not closed or exited)
       item-bound runs.
     - `idle-entry` **[b4]**: a running entry that meets all of these:
       - it has live runs;
       - every live member's activity state (`agent_activity.state`) is `idle` or
         `finished_silent`, or the member's agent status is `done`;
       - no open non-delivery obligation is held or sent by its non-handler members;
       - that condition has held for at least the idle threshold (default 30 minutes,
         injectable).
       Its fix is `tt team queue integrated …` if the candidate shipped, otherwise
       `tt team queue scope --entry E --owns <changed files>`.
   - **When a queued entry gets a `Stall`:** it has no host-capacity or disk block, and one of
     these holds:
     - every entry in its `BlockedBy` is a stall blocker;
     - slots are full, but they stay full only because stall blockers are counted
       **[b4]** (fullness is recomputed with stall blockers excluded);
     - its only block is `no-handler`: no online, non-retired handler exists, as opposed to
       every handler being leased. The fix is Projects → Set up database handler, or
       `tt resume <retired handler>`.
     - `serial-halted`: a serial queue is halted by a failed entry.
   - **Not stalls:**
     - an overlap with a working live team;
     - slots full of working teams;
     - host capacity or disk;
     - handlers that are all leased.
   - **`BlockReason`** carries the explanation and the fix command.
   - **`Since` is durable [f2]:**

     | Cause | `Since` is… |
     |---|---|
     | `failed-entry` | the blocker's `updated_at` |
     | `nothing-running` | the latest `last_event_at` among the item's closed or exited runs, else the blocker's `updated_at` |
     | `idle-entry` | the latest of the members' `agent_activity.observed_at`, the latest obligation `changed_at` for the team, and the blocker's `updated_at` |
     | `no-handler` | the latest status change among the project's handler agents, else the queued entry's `updated_at` |
     | `serial-halted` | the failed entry's `updated_at` |

     `idle-entry` requires `now - Since ≥ idle threshold` before it counts as a stall.
   - **Notice op `stall_notice`:**
     - the hub recomputes the stall and refuses one that is stale or cleared;
     - it requires `now - Since ≥ 5m` (injectable);
     - it inserts one NOTICE from `team_queue/runner`, addressed to no one, with refs
       `{entry, item, blocker, cause}`, no `escalation` ref and no obligation;
     - dedupe is through `team_queue_requests` with request ID
       `queue-stall-<blocker or entry>-<cause>-<blocker revision>`, and the expected revision
       is left out of the hash.
7. **c3: no owner escalation for a queued fix.** Unchanged from r1.
   - In the broker path, skip every owner-level escalation of an obligation that meets all of
     these:
     - it is `blocked`;
     - the holder's latest BLOCK reply to it links an item: `primary` in
       `message_work_item_links`, or `related` in `message_audit_links`, whichever
       `tt send --related` persists;
     - that item has a queued, launching or running entry with no `released_at`.
   - A lead-held obligation, which would jump straight to the owner, is skipped too.
   - Update `docs/message-broker.md` where it describes owner escalation.
   - Keep this to one predicate and its test, so that `wi_e76872a6abc6d78e` can extend it.
8. **c4: triage suggestions (read-only).** Unchanged from r1.
   - New package `hub/internal/triage`: tokenize, Jaccard similarity, criteria-line extraction
     and a markdown release-row parser.
   - `store/triage.go` plus `GET /v1/tasks/{id}/work-items/triage?staleDays=N` return three
     lists:
     - `duplicates`: title ≥ 0.6, or criteria ≥ 0.7;
     - `alreadyReleased`: open items matched against done items;
     - `stale`: no activity for N days (default 7). Items with a queued or resource-holding
       entry are excluded.
   - `tt work-items triage --project T [--stale-days N] [--release-record FILE ...] [--json]`
     adds the markdown matches and prints "Suggestions only — nothing was changed; confirm
     with the owner."
   - The owner (unbound) and database handlers may run it; other agent sessions are refused.
   - Tests: `store/triage_test.go`, `server/triage_test.go`, `cmd/tt/triage_test.go`.
9. **c5: documentation and templates. [r2 b3, f6, f7]**
   - **`docs/owner-helper.md`:** add a section "Queue chores: product vs owner helper". It
     should have:
     - a table of each chore from the 09-28/29 log, each marked as now automatic or now a
       supported command;
     - a list of what stays with the owner session: owner decisions and verbatim relays,
       diagnosis, judgment calls, releases until the deployment agent, matrix approvals, and
       disk cleanup until `wi_b39698a5238560d3`;
     - **[f7]** after `integrated`, the entry no longer protects its files, so post-release
       fixes are filed as new items and queued, not made by the still-live team.
   - **`docs/team-launch.md`:** cover `--serial`, intake ownership, narrowing, `integrated`
     (including the serial effect from f6 and the end state from b2), stall notices and
     causes, and scoping a failed entry. Add the rollback note.
   - **Handler briefing (`queueHandlerAcceptanceBriefing`):**
     - record `--owns` at scope confirmation;
     - mention triage;
     - drop "scope an unscoped queued entry";
     - **[b3]** add: "If the item's entry shows an owner integration (`tt team queue list`:
       `owner-integrated`), save done with a plain `tt work-items update … --status done`;
       do not pass `--worktree`, `--branch` or `--commit`, and no leased run is required."
   - **Digest effect [b3].** Changing this briefing changes `handlerTemplateDigest`. After
     release:
     - a primary handler whose rotation policy has `OnTemplateChange` becomes due for a
       `template` rotation on the next rotation tick;
     - handlers without that policy (the current A/B arms) keep their old briefing until
       they are re-spawned.
     The release note tells the owner session to relay the plain-done sentence to live
     handlers, or to rotate them. `handler_rotation_test.go` asserts that the new digest
     differs from the old one.
   - **Lead template:** narrow the entry to the plan's owned files right after the plan
     freezes. Regenerate `plan.mjs` and sync `docs/team-examples.md`.
   - **Usage line:** update `main.go:101`.
10. **c6: tests.** Write each alongside its step. The 09-29 deadlock regression (a11) goes last,
    at runner level with fake spawns.

## Acceptance criteria (final text)

Each criterion is observable by running the named command or test, or the CLI against an
isolated hub.

- **a1 (c1)**
  - Run `tt work-items scope confirm … --owns hub/x.go --owns docs/y.md`. The confirmation GET
    returns that ownership.
  - Then run `tt team queue add --item … --order …` with no `--owns`, in a parallel project.
    The entry is saved with exactly those paths, and `list` shows `owns=hub/x.go,docs/y.md`.
  - A retry of the confirm with different owns is refused as a changed retry.
  - Tests: the store test for confirm-ownership, and the cmd/tt test for add-from-intake.
- **a2 (c1)**
  - In a parallel project, run an add with no `--owns`, no intake ownership and no `--serial`.
    It is refused before any worktree is created (no `.build/worktrees/queue-*` is left), and
    the error names `--owns`, the handler's `--owns` and `--serial`.
  - With `--serial`, the entry is saved with `serial=true` and lists as
    `owns=serial (runs alone)`.
  - The hub refuses an add with empty ownership that is not serial.
  - Adds in a serial project are unchanged, and the existing scope tests pass.
- **a3 (c2) [r2]**
  - The runner narrows a running entry with saved acceptance, in one `scope` write, to the
    files in `git diff --name-only <acceptance base>...<commit>` (merge-base) that fall under
    its declared ownership.
  - A second tick writes nothing.
  - A queued entry that overlapped only unchanged files claims before the team closes.
  - A branch that merged a later base is charged only with its own files.
  - A missing commit or a git error leaves the entry running and not failed.
  - Test: cmd/tt with a real temporary git repository.
- **a4 (c2) [r2]** Run `tt team queue integrated --entry E --commit SHA` on a running entry
  that has live item-bound runs and an open item.
  - It records `ownerIntegration` (commit and changed files) and sets `releasedAt`.
  - Afterwards:
    - `list` shows no `blockedBy` that points at E;
    - `freeQueueHandler` shows the lease free;
    - the slot count drops;
    - the item's status and revision are unchanged, and its members stay bound;
    - a `tt send --to role:database_handler --work-item <E's item>` from a live member of E
      is delivered to that entry's handler, not refused with a 409.
  - The same command on a failed, unreleased entry also releases it.
  - It is refused for:
    - queued, launching, finished and already-released entries;
    - calls from an agent session;
    - a SHA that does not descend from the base;
    - a failed entry with uncertain launch members;
    - **[b5]** an entry whose `release_jobs` row is `verified`, `claimed`, `merged` or
      `blocked`. The error names the job and its state.
- **a5 (c2) [r2]** After a4:
  - a plain `tt work-items update … --status done` from an available handler is accepted;
  - a done save that carries `--worktree/--branch/--commit` for that item is refused, and the
    error names the owner integration;
  - once the item is terminal, the runner closes the team, cleans up, and marks the entry
    `finished` without an integration snapshot;
  - an owner `scope` on a failed, unreleased entry narrows it, and a widening is refused.
- **a6 (c3) [r2]**
  - `list --json` shows the stall and its fix, also in `blockReason`, for each cause:
    `failed-entry`, `nothing-running`, `idle-entry` and `no-handler`.
  - The `idle-entry` case: every live member's activity `idle` or `finished_silent`, or agent status `done`, no open work
    obligations, held past the injected threshold.
  - `serial-halted` gets the same check.
  - A limit-2 project whose two slots are held by one failed entry and one idle entry shows a
    stall on the queued entry, not "All team slots are reserved".
  - No stall is shown for:
    - slots full of working teams;
    - an overlap with a working live team;
    - an `idle-entry` candidate below the threshold, or one holding an open obligation;
    - a block on host capacity or disk;
    - a failed parallel entry with no live runs;
    - handlers that are all leased.
  - One store test per case.
- **a7 (c3) [r2]**
  - A stall produces exactly one Board NOTICE once its durable `Since` is at least 5 minutes
    old. It is posted with the reference times, and a hub restart between ticks does not reset
    them.
  - The NOTICE names the blocked entry and item, the blocker and the fix command. It carries no
    `escalation` ref and creates no obligation.
  - Further ticks and retries add no message. A new blocker revision may post once more if the
    stall persists.
  - A stall that clears before the grace period posts nothing, and a `stall_notice` for a stale
    stall is refused.
- **a8 (c3)**
  - An overdue obligation gets no owner escalation from `broker.Tick`, however late, when all
    of these hold:
    - it is `blocked`;
    - the holder's latest BLOCK links (primary or `--related`) an item;
    - that item has an unreleased entry that is queued, launching or running.
  - This applies whether the holder is a worker or the lead.
  - Once that entry finishes, is removed, fails or is released, the next tick escalates
    normally.
  - `docs/message-broker.md` states the rule.
  - Test: a broker test.
- **a9 (c4)** On a fixture with two near-duplicate open items, one open item that matches a done
  item, and one open item idle for 10 days:
  - `tt work-items triage --stale-days 7 --json` and the GET route list exactly those three
    suggestions, and nothing for an idle item that has a queued entry;
  - `--release-record` adds a match from a markdown row;
  - the counts of work items, revisions, messages and queue entries are the same before and
    after;
  - a non-handler agent session is refused.
  - Tests: store, server and cmd/tt.
- **a10 (c5) [r2]**
  - `docs/owner-helper.md` has the "Queue chores: product vs owner helper" section, including
    the rule that post-release fixes become new items. There is no new parallel doc.
  - `docs/team-launch.md` covers everything in step 9, including the serial effect of
    `integrated` and its `finished` end state.
  - `npm run check:team-plan` passes.
  - `tests/team-examples.test.js` asserts the new lead sentence.
  - `handler_rotation_test.go`:
    - asserts the handler briefing's `--owns` sentence and its plain-done sentence for
      owner-integrated entries;
    - asserts that `handlerTemplateDigest` changed.
- **a11 (c6) [r2]** A regression test for the 09-29 deadlock, at runner level with fake spawns
  and limit 3.
  - Setup:
    - entry A has coarse ownership and is running;
    - the owner has released its candidate;
    - a post-release a13 REQUEST to its verifier is still open;
    - A is failed "as integrated";
    - its lead's self-close is refused;
    - queued entry B overlaps A.
  - After `tt team queue integrated --entry A --commit SHA`:
    - the next tick claims B;
    - A's item stays open with the same revision and record;
    - A's verifier can still `tt send --to role:database_handler --work-item A`.
  - Then:
    1. a13 gets its RESULT;
    2. the handler saves a plain done;
    3. the lead runs `tt close --team`, or the runner freezes and runs the close;
    4. the runner cleans up every member of A;
    5. A ends `state=finished`, with `releasedAt` and `ownerIntegration` set, `integration`
       nil, and every member of A closed with its cleanup done.
  - Needed at no point: item dismissal, `tt owner cancel`, an owner team close, or
    `tt team queue release`.
- **a12 (all)**
  - The approved matrix passes on the frozen candidate:
    - the go group (`go test` and race, with `-timeout=14m`);
    - the migration group;
    - the npm unit and browser groups (because `client/team-examples.js` changes).
  - A store test opens a database created without the new columns and migrates it twice, which
    must be idempotent. It reads legacy entries as `serial=false` with no owner integration.

**Independent verification:** a1–a9, a11 and a12 go through the approved matrix. The verifier
also runs a1, a2 and a4 as CLI checks against an isolated hub, with a private tmux socket and a
temporary HOME. a10 goes to the reviewer.

**No new browser suite and no matrix change.** Only the existing rules fire:

- `hub/` → go
- `hub/internal/store/` → migration
- `client/` and `tests/` → unit+browser
- `docs/` → unit

The release needs the owner's migration rehearsal (three additive columns) and the handler
template note from step 9.

## Risks and the check that exposes each

1. **A missed resource-predicate site, or a wrong one [r2 b1].** A released entry could keep a
   slot or lease, or the runner could stop seeing it. Checks:
   - the step 1 audit (grep for `released_at|state IN ('launching','running')`);
   - a4 and a11, which assert claim, `freeQueueHandler`, `set_limit`, scope overlap,
     role-handler routing and finishing a failed, released entry.
2. **The done-save gate or the briefing leaves a handler stuck [r2 b3].** Check: the a5 accept
   and refuse cases, and the briefing assertion in a10.
3. **Releasing while the team is live** weakens the old "wait for close" safety. Only an
   explicit owner record can trigger it, never with uncertain spawns, and never while a
   release job is live. Check: the a4 negatives. Post-release fixes become new items (f7).
4. **Rollback to an older hub.** A running entry with `released_at` counts as active there,
   which is more conservative. Such a failed entry is ignored by the old runner and stays
   failed and released until the new hub returns. The old hub ignores the new columns.
   Checks: the owner's migration rehearsal and the a12 migration test.
5. **Notice spam or false stalls, including an idle team that is really waiting on someone
   [r2 b4].** Checks:
   - the durable `Since`, the grace period and the idle threshold;
   - the open-obligation exclusion;
   - recompute-on-post;
   - the a6 negatives and a7.
6. **Git history unavailable when narrowing.** Check: the a3 error case.
7. **Overlap with `wi_e76872a6abc6d78e`** (progress-aware escalation) in `broker.go`,
   `store/broker.go` and `docs/message-broker.md`. The overlap is one predicate and one test
   (a8), and the escalation levels are not restructured.
8. **Overlap with `wi_93629e4ce61658cb`** (deployment agent) [r2 b5]. This plan does not touch
   `delivery*.go`, `deployment.go`, `releases.go`, `client/team-delivery-view.js` or the
   deploy scripts. It does read `release_jobs` state: `integrated` refuses while a job for the
   entry is non-terminal, so the deployment agent keeps sole ownership of candidates it may
   merge.
   - If that item adds release-job states, it must add the new non-terminal ones to this
     refusal.
   - Check: the a4 negative with a `verified` job and one with a `claimed` job.
9. **Overlap with `wi_b39698a5238560d3`** (closeout cleanup). This plan changes no worktree or
   disk cleanup code. Member session cleanup reuses the existing `r.cleanup`.
10. **The template edit runs the full browser group.** That is expected, and it is not a matrix
    change.

## Out of scope

- The following items, except the single a8 predicate:
  - automatic release (`wi_93629e4ce61658cb`);
  - closeout disk cleanup (`wi_b39698a5238560d3`);
  - status digests;
  - progress-aware escalation (`wi_e76872a6abc6d78e`).
- Scheduling triage daily. That belongs to the steward, `wi_5b4b94dbc9a11e8b`.
- TailOS Delivery panel changes. The panel already shows `blockReason`.
- Re-adding an item after its entry failed before launch. This needs a table rebuild; file it
  as a follow-up item.
- Detecting owner integration from git automatically, and refusing acceptance when the changed
  files fall outside ownership. Both are follow-up candidates.
- **f4:** obligations that a BLOCK reply creates on its recipient. Left for
  `wi_e76872a6abc6d78e`.
- **f5:** naming legacy unscoped queued entries in the stall explainer. A follow-up.

## Decisions (lead confirmed D1–D5 in #15206; r2 decisions from #15216)

- **D1.** Releasing an entry while its team is live applies only to entries with an
  owner-integration record, running or failed. Other failed entries keep "release after the
  team is closed and cleaned".
- **D2.** "Without owner surgery" means one supported owner command that records the
  integration commit.
- **D3.** Intake ownership is set with `--owns` at scope confirmation. It is optional for older
  handlers; without it, the add needs `--owns` or `--serial`.
- **D4.** Triage defaults: stale after 7 days, title Jaccard ≥ 0.6, criteria ≥ 0.7.
- **D5.** The base is `c6a8ec1`.
- **D6 [r2].** A failed entry with an owner integration ends `finished` after the runner's team
  close and cleanup (b2).
- **D7 [r2].** `idle-entry` threshold defaults to 30 minutes, and the notice grace to 5 minutes.
  Both are injectable.

## Implementation notes (builder, ASSIGN #15225)

The plan above is r2 as reviewed (sha256 `5c712685…`). These notes record where
the build differs from its wording, each reported to the lead:

- **Handler routing (a4, lead #15246).** `resolveRole` routes an item's
  `role:database_handler` messages to the handler of its launching, running or
  failed entry that is unreleased **or carries an owner integration**. A plainly
  released failed entry's team is closed, so it stops routing, which keeps the
  existing `TestParallelQueueSkipsConflictAndLeasesDistinctHandlers` assertion.
- **`nothing-running` grace.** A freshly claimed launching entry has no runs yet,
  so `nothing-running` counts only after its durable `Since` has held for the
  5-minute notice grace; `idle-entry` already waits for its own threshold.
- **Ownership.** The lead added `hub/cmd/tt/work_order_scope.go` (where
  `scope confirm` lives), `hub/cmd/tt/work_order_scope_test.go` and
  `hub/internal/store/team_queue_uncapped_test.go` (#15238). Two uncapped tests
  now add their unscoped entries as serial; the legacy-unscoped display keeps its
  coverage through a seeded `serial=0` row.
- **Lead template sentence.** The Planned lead prompt has an 8192-character cap
  including its role line, so the sentence reads "Once the plan freezes, narrow
  the queue entry to its owned files: tt team queue scope --entry ENTRY --owns
  PATH (repeat); widening may wait."
- **a8 scope.** The obligation a BLOCK reply creates on its own recipient is a
  separate obligation (follow-up f4) and may still escalate; the a8 test counts
  only the blocked obligation's own owner escalations.
- **Handler template digest.** `handlerTemplateDigest("handler assignment")`
  changes from `4556752…` to `6c6a04d…`. After release, a primary handler with
  `OnTemplateChange` becomes due for a template rotation; other live handlers keep
  the old briefing until they are re-spawned, so relay the plain-done sentence to
  them or rotate them.
