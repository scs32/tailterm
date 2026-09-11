# Item extra-agent capacity

Bug `wi_84dafce5ad044acd` revision1, order #2050, the scoped #2192
generic-launch correction, and the #2300 independent-review correction round.
Owner clarification (Board #2045/#2048), verbatim:

> UH... I'm a little confused on this one... When would a single bug or
> feature need 32 agents. Again, the rule is that the "extra" agents are per
> bug or feature ON TOP of the allocated team. When an extra agent is closed
> you've got a new slot.

## History: the binding-order heuristic was rejected

The first candidate (order #2050) inferred a work-item binding's
classification from arrival order: the earliest parented agent bound to an
item was treated as its "builder" (free), and only later ones as "extras"
(charged). Independent review #2300 rejected this (finding 1): it is not an
auditable allocated-team model, it lets a genuine extra claim the free
"builder" slot merely by arriving first, it charges a legitimate second
regular member as an extra, and it ignores `--replaces-agent`, so a
replacement of the builder was itself charged as rank>1. This document
describes the corrected, explicit-classification design that replaced it.

## The rule

- A task's `maxNewAgents` setting is the **per-item extra allowance**, not a
  project-wide lifetime quota.
- Classification (`teamRole`: `member` or `extra`) is a **persisted,
  explicit declaration** on a work-item binding — never inferred from
  `ParentAgentID` or binding/creation order.
  - `member`: this item's allocated regular team member. Never charged
    against the extra allowance, however many members are already bound to
    the item.
  - `extra`: on top of that allocation. Checked against `maxNewAgents`,
    scoped to that item only — item A's extras never reduce item B's
    allowance, and vice versa.
- A **fresh** (non-replacement) binding admitted through a **parented**
  launch (an agent session's `tt spawn`) must declare `--team-role member`
  or `--team-role extra`; the request is rejected outright if it doesn't.
- A **replacement** (`--replaces-agent`) **inherits** the role of the
  binding it replaces. It is not a fresh allocation decision: a
  caller-supplied `--team-role` that conflicts with the inherited role is
  rejected (not silently overridden), and a replacement of an extra is
  exempt from the allowance/`AllowAgentSpawn` check entirely, since it
  continues an already-reserved slot rather than creating a new one.
- A **parentless** (browser/manual) admission is always resolved as
  `member`: there is no ambiguity to declare, since it never consumes the
  extra allowance either way.
- `AllowAgentSpawn` gates only fresh `extra` admissions (and the unbound
  parented fallback below). A genuine `member` launch through the parented
  CLI path is **not** blocked by it, since disabling agent-originated
  spawning is about extra-helper spawning, not ordinary team composition.
- Closing (status `closed`) an extra frees exactly its own slot for its own
  item. Exiting or retiring an extra does **not** free its slot; only an
  explicit close (or an authorized recovery flow that eventually closes it)
  releases the reservation, so a resumed or recovered identity cannot be
  double-spent by a successor launch.
- Parented agents with no work-item binding have no item to classify a
  membership against, and no field to declare one on (`teamRole` lives on
  the work-item request). This is a disclosed, known limitation, not
  silently resolved: such requests fall back to task-wide accounting under
  `AllowAgentSpawn`, excluding closed rows. Prefer a work-item-bound,
  explicitly classified launch whenever one is possible.

## Honest limitation: classification is not cryptographically enforceable

The private hub authenticates with a single shared workspace token
(`hub/internal/server/token.go`); it has no per-agent authentication layer.
Server-side, `Store.AddAgent` cannot distinguish "the lead agent, acting in
good faith" from "any other caller with hub access, declaring `member` when
the launch is really an extra." The explicit `teamRole` field records what a
caller *declared*, auditable after the fact (it is persisted and exported),
but it does not — and cannot, on this transport — prove the declaration was
honest. This is the same limitation the original order's bootstrapping
section already flagged for `ParentAgentID` provenance; it now also applies
to `teamRole`. Nothing in this fix claims otherwise. The concrete
mitigation available today is auditability (every admission decision and
its declared classification is a durable, reviewable record) and coordinated
process (`tt spawn --team-role` is meant to be used honestly by the
coordinating agents this hub already trusts), not access control.

## Where this lives

- `hub/internal/api/work_context.go`: `TeamRoleMember` / `TeamRoleExtra`
  constants; `AgentWorkItemRequest.TeamRole` (caller-declared) and
  `AgentWorkItemBinding.TeamRole` (persisted, resolved) fields.
- `hub/internal/store/migrate.go`: additive `team_role TEXT NOT NULL
  DEFAULT ''` column on `agent_work_item_bindings` (in the `CREATE TABLE`
  for a fresh database, plus a targeted `ALTER TABLE` check for an existing
  one), and a matching index
  `agent_work_item_bindings_item_team_role(item_task_id,item_id,team_role,created_at)`.
- `hub/internal/store/work_context.go`: `validateAgentWorkItemRequest` now
  also resolves and returns the binding's `teamRole` — inherited from the
  replaced binding for a replacement (rejecting a conflicting explicit
  value), forced to `member` for a parentless request, and required to be
  exactly `member`/`extra` otherwise; `insertAgentWorkItemBinding` persists
  it; `loadAgentWorkItemBinding` reads it back.
- `hub/internal/store/store.go`, `Store.AddAgent`: the admission check no
  longer ranks bindings by arrival order at all. For a parented, work-item-
  bound, fresh (non-replacement) request resolved as `extra`, it gates on
  `AllowAgentSpawn` and counts existing non-closed `extra`-classified
  bindings for that exact `(itemTaskId, itemId)` against `maxNewAgents`. A
  `member` classification, and any replacement, is admitted without that
  gate. An unbound parented request falls back to task-wide accounting
  (still excluding closed rows), still gated by `AllowAgentSpawn`.
- `hub/cmd/tt/main.go`, `cmdSpawn`: new `--team-role member|extra` flag,
  required for a fresh parented item-bound launch (validated client-side
  before contacting the hub, and enforced again server-side); threaded into
  the `AgentWorkItemRequest`.
- `hub/cmd/tt/coordination.go`: `taskBriefingForRoster` /
  `agentTaskBriefingForLaunch` describe the corrected explicit-declaration
  rule to launched agents (previously described binding-order inference,
  which was itself a bug the review caught after the first counting fix).
  `cliDiscoveryPreamble` (#2192) is now a function of the launching
  process's own resolved executable path (`selfPath()`, threaded from
  `cmdSpawn`/`cmdBrief`): when known, the exact path is interpolated
  verbatim as the PATH-discovery fallback instead of only naming the
  concept in the abstract; when not known (a generic host-configuration
  display with no live process to resolve it from), an honest generic
  fallback is used instead of a false claim.
- `client/project-handler.js` (browser-generated handler prompt) and
  `client/task-hub.js` (task-settings "Max new agents" labels): mirrored
  prose corrections.
- `tests/handler-allocation-cases.json`: the shared JS/Go scenario-contract
  fixture's `parented-item-bound-launch-limit` and `capacity-exhausted`
  cases updated to match the explicit-declaration model and corrected
  scenario (a genuine second, declared-extra binding, not an inferred one).
- `docs/project-overview.md`: the "Max new agents" description corrected
  from "a separate lifetime allowance for additional identities" to the
  per-item extra model, including the `--team-role` requirement and the
  honest provenance-enforcement limitation above.

## Tests

- `hub/internal/store/helper_limit_test.go` —
  `TestHelperLimitIncludesDescendantsAndFinishedAgents`: closed extra frees
  exactly one slot (unbound task-wide fallback path).
- `hub/internal/store/work_context_test.go`:
  - `TestAgentWorkItemContextRejectsStaleMismatchedAndHelperAdmission`: a
    parented item-bound request with no declared `teamRole` is rejected
    outright; `--team-role member` is admitted at `maxNewAgents=0`;
    `--team-role extra` is checked against the allowance; a parentless
    admission is always resolved as `member`.
  - `TestItemExtraCapacityIsIsolatedPerItem`: two items' extra allowances
    isolated in both directions; closing an extra frees only its own
    item's slot; closing a member has no false refund of the item's spent
    extra allowance.
  - `TestItemExtraCapacityDescendantsAndConcurrentRace`: a descendant
    spawned by an extra (not directly by the member) still counts against
    its own item by its own declaration, not ancestry; a further
    descendant declared as a different item's member is not misclassified
    by ancestry; concurrent admissions against one item's single remaining
    extra slot cannot exceed it, without affecting another item's
    allowance.
  - `TestItemExtraCapacityReplacementInheritsTeamRole` (new): a replacement
    with a conflicting declared `teamRole` is rejected; omitting it
    inherits the prior binding's role and is admitted even at
    `maxNewAgents=0`, since it is not a fresh extra allocation.
  - `TestItemExtraCapacityRegularMemberIgnoresSpawnDisabled` (new): a
    `--team-role member` launch is admitted through the parented CLI path
    even while `AllowAgentSpawn` is disabled; a genuine `extra` is still
    blocked.
- `hub/cmd/tt/coordination_test.go` —
  `TestGeneratedBriefingStatesTtIsARealCliVerifiedBeforeUse`: the
  CLI-discovery preamble leads the briefing, through the real
  `agentTaskBriefing` assembly path, across orchestrator, non-orchestrator,
  database-handler and no-orchestrator launch shapes; a known self path is
  interpolated verbatim, and an unknown one falls back to an honest generic
  sentence rather than a false claim.
- `tests/handler-allocation.test.js` (14/14) and
  `hub/cmd/tt/handler_allocation_test.go`'s full contract set, re-run
  against the corrected fixture and the corrected `cmdBrief`/launch-emitter
  parity check (which now also resolves and compares `selfPath()`).
- Full suites: `go build/vet/gofmt` clean; `go test
  ./internal/store/... ./internal/server/...` clean, including `-race`;
  `go test ./cmd/tt/...` clean except two confirmed pre-existing baseline
  nonpasses unrelated to this change (`TestAskAmbiguousFailureRetainsRecoveryKey`,
  flaky transient-failure injection; `TestWorkItemsCLIUsesBodyFilesAndDurableReceipts`,
  a documented dispatch-notice sender-assertion mismatch against the current
  system Queue dispatch format) and the documented long-hanging
  `TestAskPreservesIdentityContextAndReplayPayload` (skipped, not run to
  completion); `npm test` 170/170.

## Launch-path coverage (API / CLI / browser)

- **API**: `Store.AddAgent` is the single admission seam; every launch path
  below calls through it, so the correction applies uniformly.
- **CLI (`tt spawn`)**: real caller provenance is unchanged —
  `ParentAgentID` is still set to the invoking agent when launched from an
  agent session (`cmdSpawn`, `parent := e.agent`). Classification is now a
  separate, explicit `--team-role` declaration, not inferred from that
  provenance; nothing clears or spoofs identity. A regular team member can
  be launched through this same path even while `AllowAgentSpawn` is
  disabled (finding 2), since `--team-role member` is exempt from that
  gate — proven by `TestItemExtraCapacityRegularMemberIgnoresSpawnDisabled`.
- **Browser regular-team launch** (per the #2050 bootstrapping note: a
  fresh SSH `tt spawn` outside an agent parent environment,
  `ParentAgentID=""`): unaffected — unparented requests are always resolved
  as `member` and were never subject to the extra-allowance check. Verified
  by the parentless case in
  `TestAgentWorkItemContextRejectsStaleMismatchedAndHelperAdmission` and by
  `TestHelperLimitIncludesDescendantsAndFinishedAgents`'s `manual` case.
- **Descendants/retries**: covered by
  `TestItemExtraCapacityDescendantsAndConcurrentRace` (declared
  classification, not ancestry, decides which item a binding counts
  against) and `TestItemExtraCapacityReplacementInheritsTeamRole`
  (replacement inheritance).
- **Forged classification**: see the honest-limitation section above —
  this is a disclosed, unresolved gap in a single-shared-token deployment,
  not a claimed guarantee.

## Migration / compatibility

Additive schema change: `agent_work_item_bindings.team_role`, default `''`
(empty). A pre-existing binding written before this correction has
`team_role=''`. The extra-count query filters on `team_role='extra'`
specifically, so an empty-string legacy row is **never** counted as an
extra and never receives special "builder" treatment either — there is no
more binding-order ranking of any kind, so the prior design's risk of a
legacy row permanently occupying (or vacating) a privileged position is
gone entirely. A legacy binding is simply unclassified; it is not
retroactively reclassified, backfilled, or edited by this migration. No
production data migration, backfill script, or reinterpretation of history
is included or required for this to be correct going forward — a decision
made explicitly, not left ambiguous: unclassified rows have no effect on
current or future admission decisions either way.

## Not in scope here

No handler implementation, quota/setting change (task `maxNewAgents`
numeric value is unchanged), unsupported launch bypass, production DB
edit/migration, cryptographic per-agent authentication (see the honest
limitation above), UI changes beyond the owned CLI-generation seams above,
or deployment.
