# Item extra-agent capacity

Bug `wi_84dafce5ad044acd` revision1, order #2050, the scoped #2192
generic-launch correction, and two independent-review correction rounds
(#2300, then #2771 against the round-1 candidate 18b62c2). Owner
clarification (Board #2045/#2048), verbatim:

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
replacement of the builder was itself charged as rank>1.

Round 1 (candidate 18b62c2) replaced arrival order with a persisted,
explicit `teamRole` declaration. Independent review #2771 found that
correct as far as it went, but caught five further defects specific to the
new design: a migration-ordering bug that crashed on any real existing
database, a replacement-lifecycle gap that could double-reserve or
double-count one logical extra slot, an exact-replay path that ignored
`teamRole` entirely, a legacy-accounting choice that silently refunded the
owner's ceiling, and a residual singular "allocated builder" UI wording.
This document describes the design as corrected by both rounds.

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
  continues an already-reserved slot rather than creating a new one. The
  replaced binding must belong to a genuinely **exited** agent — replacing
  a still-active one is rejected, since that would double-reserve one
  logical slot as two concurrently active bindings (review #2771 finding
  2). Once replaced, the exited original and its live replacement count as
  exactly **one** active extra on later admissions, not two — a binding
  superseded by another (`replaces_agent_id` points at it) is excluded from
  the count; only the live end of a replacement chain counts.
- An **exact replay** of a prior admission (a duplicate/retried request
  reusing the same `AgentID`, including a cross-project Queue admission
  retry) must declare the same `teamRole` as what was actually admitted; a
  replay that changes it is rejected as a conflict, not silently treated as
  the same exact prior admission (review #2771 finding 3).
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
  one). The index on that column is created **only after** the column is
  guaranteed to exist (review #2771 finding 1: creating it inside the same
  `CREATE TABLE IF NOT EXISTS` block as the fresh-database schema crashed
  migration on any real existing database, since `CREATE TABLE IF NOT
  EXISTS` is a no-op there and the index reference to a nonexistent column
  aborted before the `ALTER TABLE` repair ever ran).
- `hub/internal/store/work_context.go`: `validateAgentWorkItemRequest` now
  also resolves and returns the binding's `teamRole` — inherited from the
  replaced binding for a replacement (rejecting a conflicting explicit
  value, and rejecting the replacement outright if the replaced agent is
  not exited), forced to `member` for a parentless request, and required to
  be exactly `member`/`extra` otherwise; `insertAgentWorkItemBinding`
  persists it; `loadAgentWorkItemBinding` reads it back.
  `exactExistingQueueAdmission`'s exact-replay comparison now also compares
  the request's `teamRole` against the persisted binding's.
- `hub/internal/store/store.go`, `Store.AddAgent`: the admission check no
  longer ranks bindings by arrival order at all. For a parented, work-item-
  bound, fresh (non-replacement) request resolved as `extra`, it gates on
  `AllowAgentSpawn` and counts existing non-closed, non-superseded bindings
  for that exact `(itemTaskId, itemId)` against `maxNewAgents`, where a
  binding counts if it is explicitly `extra`-classified, **or** if it is
  parented with `team_role=''` (an unclassified legacy row — counted
  conservatively so it cannot silently refund the owner's ceiling; see
  Migration below). A `member` classification, and any replacement, is
  admitted without that gate. An unbound parented request falls back to
  task-wide accounting (still excluding closed rows), still gated by
  `AllowAgentSpawn`.
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
  - `TestItemExtraCapacityRegularMemberIgnoresSpawnDisabled`: a
    `--team-role member` launch is admitted through the parented CLI path
    even while `AllowAgentSpawn` is disabled; a genuine `extra` is still
    blocked.
  - `TestItemExtraCapacityReplacementRequiresExitedAndDoesNotDoubleCount`
    (new, review #2771 finding 2): replacing a still-active extra is
    rejected; replacing a genuinely exited one is admitted and inherits its
    role; the exited original plus its live replacement count as exactly
    one active extra on a later admission, not two.
  - `TestItemExtraCapacityLegacyParentedBindingCountsConservatively` (new,
    finding 4): a parented binding with `team_role=''` (simulating a row
    written before this correction) counts against the item's allowance; a
    parentless one does not.
- `hub/internal/store/migrate_test.go` —
  `TestMigrateAddsTeamRoleToExistingBindingsTable` (new, finding 1): the
  exact reproduction the review described — a database whose
  `agent_work_item_bindings` table predates `team_role` migrates cleanly
  (previously aborted), and the migrated database can then admit a real
  team-role-classified binding.
- `hub/internal/store/queue_test.go` —
  `TestQueueExactReplayRejectsChangedTeamRole` (new, finding 3): an
  unchanged exact replay of a cross-project Queue admission succeeds; the
  same replay declaring a different `teamRole` is rejected as a conflict.
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
`team_role=''`; it is never retroactively reclassified, backfilled, or
edited by this migration — no production data migration or reinterpretation
of history is included.

Round 1 excluded every such row from admission counting entirely (there is
no more binding-order ranking of any kind, so the original design's risk of
a legacy row permanently occupying or vacating a privileged position is
gone). Review #2771 (finding 4) correctly identified that this went too far
in the other direction: excluding an open legacy extra from the count
silently refunds the owner's ceiling, letting `maxNewAgents` fresh extras
stack on top of it. The corrected, conservative policy: a **parented**
legacy binding (`team_role=''`, `ParentAgentID` set) counts against the
item's allowance exactly as an `extra` would, until an authorized migration
assigns it a real classification; a **parentless** legacy binding is
unambiguous either way (never an extra, then or now) and does not count.
This preserves the owner's per-item ceiling for still-open historical rows
without guessing whether any given one was originally intended as a member
or an extra. An authorized owner/handler decision to run an actual
backfill (assigning explicit roles to still-open legacy rows from
historical launch records, where recoverable) remains open and is not
performed here.

### Old/new client compatibility

- **New hub, old CLI/API client**: a parented, item-bound request with no
  `teamRole` field is rejected outright (`api.ErrInvalid`) rather than
  silently guessing a classification. An old client cannot make a fresh
  parented item-bound admission against a corrected hub until it is
  updated to send `--team-role`/`teamRole`; a parentless (browser) request
  is unaffected either way, since it never carried or needed the field.
- **New CLI, old hub**: an old hub has no `team_role` column and ignores
  the field entirely, reverting to whatever accounting that old hub
  version implements. This is a hub-version compatibility boundary, not
  something the CLI can detect or paper over from the client side.
- **Recommended rollout order**: deploy the corrected hub (including the
  migration) before any client that sends `--team-role` is relied upon for
  a parented item-bound launch; a parentless (browser) launch path is
  unaffected by rollout order in either direction.

## Round 3 (review #2840, against candidate 07aad12)

A second independent review found the round-2 explicit-classification design
itself still had two live bugs, plus reopened finding 5 as a functional
(not security) requirement. Corrections:

- **Replacement duplicates and failed-retry double-spend (finding 1)**: a
  second live successor could replace the same predecessor (concurrently or
  sequentially) since nothing checked for an already-live one, and a closed
  (failed) successor excluded its predecessor from the active-extras count
  forever, letting an unrelated fresh extra take a phantom freed slot before
  a further replacement retry — still capacity-exempt — over-admitted.
  Fixed: reject a second live successor outright; only a *live* successor
  excludes its predecessor from the count, so a closed one reverts the
  predecessor to counting as itself, and a replacement is then inherently
  slot-neutral (predecessor excluded, successor counted, net zero change).
- **Omitted-role exact replay (finding 2)**: a retry of an originally
  explicit fresh admission could omit `teamRole` and still replay as an
  exact match. Fixed: an explicit match is required for a fresh, parented
  original admission's replay; a replacement's or a parentless admission's
  replay may still omit it (nothing was ever a real caller choice there).
- **Functional recorded-allocation consistency (finding 5, reopened)**:
  clarified (#2844/#2850, tightened again #2867/#2870) that disclosing the
  shared-token limitation was correct but insufficient, and that an
  *optional* field on a shared work-order link is not a durable per-agent
  binding. Implemented `AllocationIntent`: a durable record authored via a
  new pre-admission call (`tt allocation-intent create` / `POST
  .../allocation-intents`), bound to an exact preallocated `--agent-id`,
  item, revision, work-order message and team role — authored once (a
  second intent for the same identity conflicts), consumed exactly once,
  atomically, in the same transaction as the admission it authorizes. A
  fresh (non-replacement) parented member or extra admission now requires a
  matching, unconsumed intent; absence or mismatch is rejected outright. A
  replacement remains exempt (it inherits, not declares). This is layered
  on top of, not a replacement for, the `--team-role` declaration/inheritance
  rules above. It remains, honestly, a consistency check between two
  separately-authored records on a single-shared-token hub, not a
  cryptographic guarantee that the two authors were actually different
  parties — that limitation is unchanged and still disclosed above.

New tests: `TestItemExtraCapacityReplacementRequiresExitedAndDoesNotDoubleCount`,
`TestItemExtraCapacityFailedReplacementRetryNeverExceedsCeilingWithReuse`
(the exact E/R1/F/R2 acceptance case from #2844),
`TestAllocationIntentRequiredMatchedAndConsumedOnce`, and
`TestAllocationIntentEndpoint` (the same through the real HTTP API).

## Round 4 (review #2893, design review #2916, against candidate c1r28)

A third independent review found the round-3 `AllocationIntent` design
(finding 5) itself still under-specified in four ways, all required before
implementation:

- **Real preallocated expected-run binding, not just transactional atomicity
  (correction 1)**: round 3 consumed an intent atomically with admission, but
  never pinned *which* run id the admission would produce — an intent
  authored for one candidate could still be consumed by admitting a
  different, unrelated run under the same `--agent-id`. `AllocationIntent`
  now carries `ExpectedRunID`, generated at authoring time and validated
  unique against both live agent run ids and every other intent's
  `ExpectedRunID`; `Store.AddAgent` sets the admitted agent's actual
  `RunID` *from* the intent's `ExpectedRunID` for a fresh parented
  item-bound admission (`a.RunID = intent.ExpectedRunID`), rather than
  generating a fresh one and merely checking the intent existed. The binding
  is now a real preallocation, not an after-the-fact atomicity claim.
- **Checked (not just stored) author authority, with launcher provenance
  recorded (correction 2)**: round 3 recorded `AuthorAgentID`/`AuthorRunID`
  fields but never validated who was allowed to author an intent.
  `CreateAllocationIntent` now looks up the author agent's live task, run,
  status and role and rejects unless the author is a current
  `database_handler` on this exact task, or the task's recorded
  `Orchestrator` by name — a closed/exited author, a mismatched task/run, or
  an unauthorized role is rejected outright. Separately, consumption now
  records the actual launcher identity (`ParentAgentID`/its live `RunID` at
  admission time) into `LauncherAgentID`/`LauncherRunID` on the intent row,
  so the authored-by and launched-by identities are both durable and
  independently auditable, distinct concepts (the database_handler that
  authors an intent for a builder to consume is not the same agent that
  performs the launch).
- **Idempotent retry scoped exactly, with durable post-consumption readback
  (correction 3)**: `CreateAllocationIntent` accepts an optional
  `RequestID`; a retry of the exact same `(TargetTaskID, AuthorAgentID,
  AuthorRunID, RequestID)` key returns the original intent unchanged
  (`loadAllocationIntentByRetryKey`, backed by a
  `CREATE UNIQUE INDEX ... WHERE request_id<>''`) rather than conflicting or
  double-authoring, but a retry with the same key and *different* payload is
  rejected as a conflict rather than silently returning the stale record.
  `GetAllocationIntent` (`GET
  /v1/tasks/{id}/allocation-intents/{agentId}`, `tt allocation-intent get`)
  provides a plain, non-mutating readback that works both before and after
  consumption, so an uncertain caller can always confirm what was actually
  recorded/consumed without re-authoring.
- **Both directions of mixed-version compatibility, honestly documented
  (correction 4)**: see the expanded "Old/new client compatibility" section
  below. `Capabilities.AllocationIntent` (`/v1/capabilities`) is now
  advertised by the hub; `cmdSpawn` queries it before a fresh parented
  item-bound launch and fails closed with an explicit, actionable error if
  the hub does not report `Supported: true` — rather than proceeding and
  either 404ing confusingly at admission or silently falling back to weaker
  accounting. This check only detects a version mismatch; it does not
  coordinate a rollout by itself (see below).

### Where this lives (round 4 additions)

- `hub/internal/api/allocation_intent.go`: `AllocationIntent` and
  `CreateAllocationIntentRequest` extended with `TargetTaskID`,
  `ContextDigest`, `AuthorAgentID`, `AuthorRunID`, `ExpectedRunID`,
  `RequestID`, `LauncherAgentID`, `LauncherRunID`.
- `hub/internal/api/audit_export.go`: `Capabilities.AllocationIntent{Supported,
  Versions}`; `CurrentCapabilities()` reports `Supported: true`.
- `hub/internal/store/migrate.go`: the new `agent_allocation_intents` columns
  above, added via `CREATE TABLE IF NOT EXISTS` (fresh database) plus a
  targeted `ALTER TABLE` repair loop for an existing one, with the
  idempotent-retry unique index created only after that repair — the same
  migration-ordering discipline review #2771 required for `team_role`.
- `hub/internal/store/allocation_intent.go`: author-authority check,
  `ExpectedRunID`/`ContextDigest` validation, `loadAllocationIntentByRetryKey`,
  `GetAllocationIntent`.
- `hub/internal/store/store.go`, `Store.AddAgent`: `a.RunID` is set from the
  intent's `ExpectedRunID` for a fresh parented item-bound admission (not
  generated independently); the consumption `UPDATE` also records
  `launcher_agent_id`/`launcher_run_id`.
- `hub/internal/server/server.go`: `GET
  /v1/tasks/{id}/allocation-intents/{agentId}` → `GetAllocationIntent`.
- `hub/internal/api/client.go`: `Client.GetAllocationIntent`.
- `hub/cmd/tt/main.go`: `cmdAllocationIntentCreate` auto-populates
  `AuthorAgentID`/`AuthorRunID` from the CLI's own session identity (not a
  spoofable flag) and computes `ContextDigest` itself from
  `--work-context-file`/`--work-context-json`; `cmdAllocationIntentGet`
  (`tt allocation-intent get --agent-id`) added; `cmdSpawn` queries
  `Capabilities` before a fresh parented item-bound launch and fails closed
  if `AllocationIntent.Supported` is false or the call errors.

### Old/new client compatibility (round 4, supersedes round 3's version)

- **New hub, old CLI/API client (no allocation intent authored)**: a fresh
  parented item-bound admission with no matching, unconsumed intent is
  rejected outright by `Store.AddAgent` with an actionable error naming the
  missing/mismatched allocation intent. Verified end-to-end (real HTTP
  server, real work item and order message) by
  `TestSpawnOldStyleRequestWithoutIntentRejectedByCorrectedHub`.
- **New CLI, old hub (no `AllocationIntent` capability)**: `cmdSpawn` fails
  closed before ever attempting admission, with an explicit
  "hub does not support allocation-intent" error, whether the old hub omits
  the `allocationIntent` capabilities key entirely or a newer-but-disabled
  hub explicitly reports `Supported: false`. Verified by
  `TestSpawnFailsClosedAgainstHubMissingAllocationIntentCapability` and
  `TestSpawnFailsClosedWhenHubExplicitlyReportsAllocationIntentUnsupported`;
  the control case (`TestSpawnProceedsPastCapabilityGateWhenHubSupportsAllocationIntent`)
  confirms the gate does not itself block a launch against an up-to-date hub.
- **Honest framing**: there is no single-message "hub-first" ordering that
  makes a mixed deployment transparently safe in both directions at once — a
  corrected hub with any not-yet-upgraded CLI already rejects fresh
  item-bound launches (by design), and an upgraded CLI against any
  not-yet-upgraded hub now also refuses to attempt one. The actual supported
  unit of deployment is a **coordinated rollout window**: upgrade the hub and
  every launch-capable CLI together, expecting fresh parented item-bound
  launches to be unavailable for the duration for whichever side lags, not
  silently degraded to weaker accounting on either side.

### Tests (round 4)

- `hub/internal/store/work_context_test.go` —
  `TestAllocationIntentRequiredMatchedAndConsumedOnce` (rewritten,
  comprehensive): unauthorized author rejected; stale author run rejected;
  `ExpectedRunID` reuse rejected; context-digest mismatch rejected; launcher
  identity recorded distinctly from author identity on consumption;
  `GetAllocationIntent` readback correct both before and after consumption;
  idempotent retry returns the original record unchanged, a payload-mismatch
  retry on the same key conflicts.
  `TestAllocationIntentCrossTargetTaskRejected` (new): a data-level
  target-task mismatch (simulated via direct SQL, since the creation API
  path always pins it correctly) is rejected at admission.
  `TestAllocationIntentLegacyEmptyFieldsCannotAuthorize` (new): a
  pre-round-4-shaped intent row (missing the new required fields, simulated
  via direct SQL insert) cannot authorize an admission.
- `hub/internal/server/server_test.go` — `TestAllocationIntentEndpoint`
  extended with a `GetAllocationIntent` readback assertion through the real
  HTTP API.
- `hub/cmd/tt/coordination_test.go` (new) —
  `TestSpawnFailsClosedAgainstHubMissingAllocationIntentCapability`,
  `TestSpawnFailsClosedWhenHubExplicitlyReportsAllocationIntentUnsupported`,
  `TestSpawnProceedsPastCapabilityGateWhenHubSupportsAllocationIntent`,
  `TestSpawnOldStyleRequestWithoutIntentRejectedByCorrectedHub`: the
  mixed-version compatibility gate, both directions, end-to-end through a
  real HTTP server and a real work item/order message (not a synthetic
  shallow-JSON stand-in).
- Full suites re-run clean after all round-4 changes: `go build/vet`,
  `gofmt -l` clean; `go test ./internal/store/... ./internal/server/...
  -race -count=1` clean; `go test ./cmd/tt/...` clean for every test this
  round touched (the same pre-existing, unrelated baseline nonpasses noted
  under round 2/3 remain and were independently reconfirmed present on the
  unmodified baseline this round via `git stash`).

## Round 5 (review #3003/#3010, against candidate f778e37)

A fourth independent review of frozen f778e37 found seven remaining gaps in
the round-4 `AllocationIntent` implementation, all now closed:

1. **Intended launcher not bound before admission**: `CreateAllocationIntentRequest`
   had no expected-launcher fields; `Store.AddAgent` recorded whichever
   `ParentAgentID` happened to consume the intent, rather than checking it
   was the one authorized to. Fixed: `ExpectedLauncherAgentID`/`ExpectedLauncherRunID`
   are now part of the authored tuple (defaulting to the author when no
   explicit delegate is named, and validated live/on-task at authoring time
   when a delegate is named); `Store.AddAgent` now requires the actual
   `ParentAgentID`/its live run to match the intent's expected launcher
   exactly before consuming it, and a legacy intent with these fields blank
   can never authorize an admission.
2. **CLI retry not actually idempotent**: `tt allocation-intent create
   --request-id K` auto-generated a fresh `--expected-run-id` on every
   invocation when the flag was omitted, so a lost-response retry of the
   unchanged command sent a different `ExpectedRunID` and the store's
   retry-key lookup correctly rejected it as a payload mismatch. Fixed:
   `--expected-run-id` is now required whenever `--request-id` is set, so
   the caller freezes it once and an unchanged retry replays identically.
3. **Generated guidance on the old partial contract**: `coordination.go`'s
   emitted MAIN/handler/worker briefings and the top-level `tt` usage string
   described an intent bound only to identity/item/revision/order/role.
   Both are now updated to describe the full tuple (prepared-context digest,
   preallocated expected run, intended launcher, request receipt/readback
   via `tt allocation-intent get`, and the coordinated mixed-version rollout
   window).
4. **Retired authors remained authorized**: `CreateAllocationIntent` checked
   only closed/exited author status; a retired author (a real, existing
   agent, just not a live acting session) was still accepted. Fixed:
   retired is now rejected exactly like closed/exited, for both the author
   and any explicitly named delegate launcher.
5. **Exact max=1 acceptance case and concurrency unproved**: the E->R1(close)->F(reject)->R2
   acceptance case was previously proved only at max=2, and the "at most one
   live successor" invariant (round 3, finding 1) was proved only by code
   inspection, not an actual concurrency test. Both are now covered.
6. **Capability gate ignored advertised `Versions`**: `cmdSpawn` checked only
   `Capabilities.AllocationIntent.Supported`, so a hub reporting
   `Supported: true` for some future incompatible revision would pass the
   gate. Fixed: the gate now also requires `AllocationIntentCapabilityVersion`
   (1) to appear in the advertised `Versions` list.
7. **Legacy audit format 2 silently gained the intent stream**: `agentAllocationIntents`
   had been added directly to the shared `exportQueries` list used by both
   format 2 and format 3, so format 2's schema silently changed when this
   correction landed -- exactly the kind of unannounced legacy-consumer
   breakage format 2 exists to prevent. Fixed: moved to a new
   `allocationIntentExportQueries` list included only for format 3 (the
   same pattern the Queue streams already used), with both formats tested.

### Where this lives (round 5 additions)

- `hub/internal/api/allocation_intent.go`: `AllocationIntent` and
  `CreateAllocationIntentRequest` gain `ExpectedLauncherAgentID`/`ExpectedLauncherRunID`.
- `hub/internal/store/migrate.go`: `expected_launcher_agent_id`/`expected_launcher_run_id`
  columns, added via the same fresh-table/`ALTER TABLE`-repair sequencing as
  every other `agent_allocation_intents` column.
- `hub/internal/store/allocation_intent.go`: defaults the expected launcher
  to the author when omitted; validates an explicit delegate is live and
  on-task; rejects a retired author or delegate; includes the launcher
  fields in the retry-key payload comparison.
- `hub/internal/store/store.go`, `Store.AddAgent`: requires the actual
  `ParentAgentID`/its live run to equal the intent's expected launcher
  exactly before consuming it; a legacy intent with the launcher fields
  blank cannot authorize admission.
- `hub/internal/store/audit_export.go`: `allocationIntentExportQueries`
  (format-3-only), replacing the entry previously inside the shared
  `exportQueries`; its query now also selects the new launcher columns.
- `hub/cmd/tt/main.go`: `cmdAllocationIntentCreate` gains `--launcher-agent-id`/`--launcher-run-id`
  and requires `--expected-run-id` whenever `--request-id` is set; `cmdSpawn`'s
  capability gate checks `Versions` for a compatible entry, not `Supported`
  alone; top-level usage string describes the full `allocation-intent
  create`/`get` contract.
- `hub/cmd/tt/coordination.go`: emitted MAIN/handler/worker guidance
  describes the full tuple, retry/readback mechanics and the coordinated
  rollout window.

### Tests (round 5)

- `hub/internal/store/work_context_test.go` --
  `TestAllocationIntentRequiredMatchedAndConsumedOnce` (extended): a wrong
  actual launcher is rejected even when the preallocated identity and every
  other field match; an explicit delegate launcher distinct from the author
  is honored (and only that delegate may consume it, not the author);
  authoring an intent naming an unknown/off-task delegate is rejected; a
  retired author is rejected.
  `TestItemExtraCapacityFailedReplacementRetryExactMaxOneCase` (new): the
  exact max=1 E->R1(close)->F(reject)->R2 case.
  `TestItemExtraCapacityConcurrentDuplicateReplacementOnlyOneSucceeds`
  (new): 8 concurrent replacement attempts against the same exited
  predecessor -- exactly one succeeds, the rest conflict.
- `hub/internal/store/audit_export_test.go` --
  `TestAuditExportAllocationIntentStreamOnlyInV3` (new): format 2 has no
  `agentAllocationIntents` stream at all; format 3 has it, with the
  launcher/author/expected-run fields all present.
- `hub/cmd/tt/coordination_test.go` (new) --
  `TestAllocationIntentCreateRequestIDRequiresFrozenExpectedRunID`: using
  `--request-id` without `--expected-run-id` is rejected before contacting
  the hub. `TestAllocationIntentCreateLostResponseRetryReplaysIdentically`:
  an actual `cmdAllocationIntentCreate` call, repeated with the identical
  frozen `--expected-run-id`/`--request-id`, replays successfully (not a
  conflict) and a durable `GetAllocationIntent` readback confirms exactly
  one record.
  `TestSpawnFailsClosedWhenAllocationIntentVersionsExcludesCompatibleVersion`
  (new): `Supported: true` with a `Versions` list excluding 1 still fails
  closed.
- Full suites re-run clean after all round-5 changes: `go build/vet`,
  `gofmt -l`, `git diff --check` clean; `go test ./internal/store/...
  ./internal/server/... -race -count=1` clean; `go test ./cmd/tt/...` clean
  for every test this round touched or added (the same three pre-existing,
  unrelated baseline nonpasses named in the round-5 handoff --
  `TestPostHumanReplyAndLiteralHelp`, `TestAskAmbiguousFailureRetainsRecoveryKey`,
  `TestWorkItemsCLIUsesBodyFilesAndDurableReceipts` -- remain, and the known
  long-hanging `TestAskPreservesIdentityContextAndReplayPayload` was
  skipped, not rerun, per the handoff's explicit instruction to preserve
  both); `npm test` 170/170.

## Not in scope here

No handler implementation, quota/setting change (task `maxNewAgents`
numeric value is unchanged), unsupported launch bypass, production DB
edit/migration, cryptographic per-agent authentication (see the honest
limitation above), UI changes beyond the owned CLI-generation seams above,
or deployment.
