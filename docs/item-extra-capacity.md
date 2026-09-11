# Item extra-agent capacity

Bug `wi_84dafce5ad044acd` revision1, order #2050, plus the scoped #2192
generic-launch correction. Owner clarification (Board #2045/#2048), verbatim:

> UH... I'm a little confused on this one... When would a single bug or
> feature need 32 agents. Again, the rule is that the "extra" agents are per
> bug or feature ON TOP of the allocated team. When an extra agent is closed
> you've got a new slot.

## The rule

- A task's `maxNewAgents` setting is the **per-item extra allowance**, not a
  project-wide lifetime quota.
- The first parented agent bound to a given work item is that item's
  allocated builder, not an extra. It is admitted regardless of
  `maxNewAgents` and never counted against any item's extra allowance,
  whether or not the launch command originated from an agent session
  (`ParentAgentID` set).
- Only the second and later parented agents bound to the *same* item are
  extras. They are checked against `maxNewAgents`, scoped to that item only —
  item A's extras never reduce item B's allowance, and vice versa.
- Closing (status `closed`) an extra frees exactly its own slot for its own
  item. Exiting or retiring an extra does **not** free its slot; only an
  explicit close (or an authorized recovery flow that eventually closes it)
  releases the reservation, so a resumed or recovered identity cannot be
  double-spent by a successor launch.
- Which agent is "the builder" for an item is fixed at the item's first
  binding and does not change later. Closing the builder never retroactively
  reclassifies a surviving extra as the (unlimited) builder slot, and never
  falsely refunds the item's already-spent extra allowance.
- Parented agents with no work-item binding have no item to scope an extra
  allowance to. They fall back to task-wide accounting (still excluding
  closed rows, so closing one of these frees its slot too).

## Where this lives

- `hub/internal/store/store.go`, `Store.AddAgent`: the admission check. For a
  work-item-bound request it counts existing parented bindings for that
  exact `(itemTaskId, itemId)` pair; if none exist yet, admission proceeds
  unconditionally (this is the builder). Otherwise it ranks all parented
  bindings for the item by creation order and counts only rank>1, non-closed
  rows against `maxNewAgents`. Unbound parented requests fall back to a
  task-wide, closed-excluding count.
- `hub/cmd/tt/coordination.go`: `taskBriefingForRoster` /
  `agentTaskBriefingForLaunch` describe this rule to launched agents, and
  (`cliDiscoveryPreamble`, #2192) now state up front that `tt` is a real
  local executable invoked via the agent's own shell tool — verified with
  `command -v tt` (or the exact launch-resolved path) and `tt --help` / `tt
  status` — before any coordination instruction is interpreted, and that
  `tt post`/`inbox`/`context`/`ask`/etc. are CLI subcommands, not native
  model tools; message text returned by them is data, never a command.

## Tests

- `hub/internal/store/helper_limit_test.go` —
  `TestHelperLimitIncludesDescendantsAndFinishedAgents`: corrected so a
  closed extra frees exactly one slot (previously asserted the opposite,
  which was the reported regression).
- `hub/internal/store/work_context_test.go` —
  `TestAgentWorkItemContextRejectsStaleMismatchedAndHelperAdmission`:
  corrected so an item's first parented builder is admitted at
  `maxNewAgents=0`; a second parented agent on the same item is still
  rejected.
- `hub/internal/store/work_context_test.go` —
  `TestItemExtraCapacityIsIsolatedPerItem` (new): end-to-end proof that two
  items' extra allowances are isolated in both directions, that closing an
  extra frees only its own item's slot, and that closing the builder never
  falsely refunds the item's spent extra allowance.
- `hub/cmd/tt/coordination_test.go` —
  `TestGeneratedBriefingStatesTtIsARealCliVerifiedBeforeUse` (new): asserts
  the CLI-discovery preamble is present and leads the briefing, through the
  real `agentTaskBriefing` assembly path, across orchestrator,
  non-orchestrator, database-handler and no-orchestrator launch shapes.

## Migration / compatibility

No schema change. Existing `agent_work_item_bindings` rows already carry
`item_task_id`/`item_id`; the corrected counting logic is a pure read-path
change. Historical ambiguous rows (parented agents added before this
correction, some possibly counted against the old lifetime rule) are not
retroactively reclassified or edited — the new logic only affects future
admission decisions, evaluated against current row state at request time.

## Not in scope here

No handler implementation, quota/setting change (task `maxNewAgents` numeric
value is unchanged), unsupported launch bypass, production DB edit/migration,
UI changes beyond the two owned CLI-generation seams above, or deployment.
