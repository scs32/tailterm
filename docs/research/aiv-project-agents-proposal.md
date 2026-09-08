# AIV-backed project agents: reuse proposal

Research date: 2026-09-08. Requested by owner #277; assignment #284.
Read-only source assessment on Mini: AIV `5089cecd43971756cdc7a0889da48249b5e47d4e` (clean), Tailterm `4b2c95c` plus concurrent, unrelated cache/UI changes. No AIV commands, tests, DB connections, service probes, provider configuration changes or agent launches were performed. Documentation describes earlier deployments; this report does not certify the current live AIV service. This artifact is frozen for handoff.

**Recommendation:** keep Codex and Claude Code running as native processes in Tailterm. Extend them with a small Tailterm MCP interface and AIV's existing evidence tools. Keep the current `database_handler` as the durable intake role; assign an independent verification agent when actual verification work exists. AIV can contribute substantial code memory and evidence handling, but cannot replace Tailterm's project/message/task lifecycle unchanged.

## What the code actually provides

The README and September 2 takeover audit are useful history, but understate this checkout. Source registers **19 MCP tools**, including findings; `environmentDigest` now crosses the MCP boundary, and the extractor revision is `v9.3-subtype-guard`. The earlier audit's missing-findings/environment-field claims are superseded in source. Evidence: [MCP registrations](/Users/stephenspeicher/projects/aidevelopment/aiv/src/serve/mcp.ts:31), [regression for environment forwarding](/Users/stephenspeicher/projects/aidevelopment/aiv/test/serve.test.ts:332), [extractor revision](/Users/stephenspeicher/projects/aidevelopment/aiv/src/extract/parser.ts:50). Test files were inspected, not executed.

| Capability, verified in source | Reuse decision |
| --- | --- |
| PostgreSQL repository/entity/snapshot ledger, filesystem content-addressed store, Git ingest, typed evidence invalidation, calibrated status projection | Reuse as a separate evidence service. Keep its data ownership and observation scope. See [ingest](/Users/stephenspeicher/projects/aidevelopment/aiv/src/ingest.ts:66), [status projection](/Users/stephenspeicher/projects/aidevelopment/aiv/src/projections.ts:104). |
| Standard MCP server with stdio and Streamable HTTP; shared PostgreSQL pool, bearer authentication when configured | Reuse server/core without DSH. Native clients can attach directly. See [transports](/Users/stephenspeicher/projects/aidevelopment/aiv/src/serve.ts:134). |
| `repo_list`, `status_get`, `entity_get`, `entity_status`, `impact`, `review_brief`, `interface_satisfiers`, `decompose`, `migration_status` | Reuse reads for investigation. Return snapshot/commit and coverage gaps with the explanation. Decomposition/migration are optional specialist tools, not required intake steps. |
| `policy_get`, `policy_list`, `policy_check` | Reuse ideas and scoped policy reads after mapping the project. Current policy queries are global; `policy_check` matches target substrings, not a general action evaluator. See [policy tools](/Users/stephenspeicher/projects/aidevelopment/aiv/src/serve/mcp.ts:292). |
| `finding_record`, `finding_list`, `finding_update` | Useful for repository evidence findings. Do not make them a second editable copy of every Tailterm bug/feature. See [finding schema](/Users/stephenspeicher/projects/aidevelopment/aiv/db/migrations/009_findings.sql:13). |
| `record_run`, `audit_verify`, `checkpoint`, `checkpoint_verify` | Reuse audit/checkpoint machinery. Adapt verification submission before treating it as an automatic, retryable check result for a specific project revision. |

AIV is an observational service. It records claims and computes evidence status; it does not itself provide a native-agent scheduler, project inbox, terminal launcher, browser vault, task retirement or bug-to-agent dispatch. The MCP registrations and [build boundary](/Users/stephenspeicher/projects/aidevelopment/aiv/docs/BUILD-SPEC-000.md:109) support that distinction. Its CLI check runners execute checks against materialized snapshots; that is different from launching and supervising native conversational agents.

## What is coupled to DSH

The core package does not require a DSH runtime. [Main TypeScript build](/Users/stephenspeicher/projects/aidevelopment/aiv/tsconfig.json:21) excludes the older `src/dsh-plugin.ts` prototype; the separate [DSH build configuration](/Users/stephenspeicher/projects/aidevelopment/aiv/tsconfig.dsh.json:1) names DSH packages. The three current JavaScript plugins depend on DSH/Cordis hook contracts:

| Adapter | Useful idea | Tailterm recommendation |
| --- | --- | --- |
| [Intent guard](/Users/stephenspeicher/projects/aidevelopment/aiv/dsh-plugin/index.js:707) | Persistent decisions; refreshable rules; explicit policy provenance | Drop `ctx.tools.guard()` interception and Tailarr-specific path bindings from this integration. Present project decisions as attributed context. Continue honoring the owner's runtime permissions and repository instructions. An AIV label is not authority to override them. |
| [Auto-recorder](/Users/stephenspeicher/projects/aidevelopment/aiv/dsh-plugin-autorecord/index.js:243) | Capture actual exit status, check population and environment | Adapt to an explicit evidence submission contract. Do not copy DSH `tools/result` observation, command regexes and fire-and-forget writes as the initial native integration. Current plugin failures are logged without durable retry, and its fingerprint is host/Node-based, not a complete test-environment identity. |
| [Evidence context injector](/Users/stephenspeicher/projects/aidevelopment/aiv/dsh-plugin-evidence/index.js:240) | Show evidence affected by an edit | Reuse `review_brief`/`entity_status` explicitly. Drop DSH `tools/post-execute`, waterfall continuation and synthetic `UserMessage` injection. Preserve source attribution; evidence is tool data, not a new human instruction. |

These are concrete coupling points, not a claim that DSH has no other capabilities. The recommendation preserves the native runtime rather than porting its tools into another harness. It also avoids inheriting Tailarr-only assumptions such as `web/app.py`, `public-release/`, basename-based repository identity and Go-only completion wording.

## Native runtime and MCP boundary

Official Codex documentation supports local stdio and remote Streamable HTTP MCP servers, including environment-based bearer credentials and per-server configuration. That establishes a supported attachment route; it does not establish which tools/accounts are available in any particular running thread. [Codex MCP documentation](https://learn.chatgpt.com/docs/extend/mcp?surface=cli).

Claude Code likewise supports remote HTTP and local stdio MCP, with project and user scopes. No provider-specific replacement execution loop is required for an agent to call these tools. [Claude Code MCP documentation](https://code.claude.com/docs/en/mcp).

**Design recommendation:** roles define responsibilities and handoff formats, not a reduced imitation of Codex/Claude. Keep their normal shell, editing, search, installed plugins, native context management and supported delegation available under the user's existing settings. Do not promise every tool on every host, automatic permission bypass, or identical idle wake-up across providers. Tailterm currently resumes exact Codex threads through its host relay; Claude notification/resumption needs its own verified path. MCP tool availability alone is not that path. See [Tailterm relay](/Users/stephenspeicher/projects/tailterm/hub/cmd/tt/relay.go:1).

Start with a host-local stdio Tailterm adapter calling the existing hub API. It can resolve hub/project/agent/run context from the already established host session, keeping browser-vault access out of the agent tool contract. AIV can remain a separate remote MCP connection. These are proposals; no adapter or client setup was created here.

## Existing handler/CLI versus a minimal MCP interface

Tailterm already has the important durable semantics: bugs/features, original message references, create/dispatch request IDs, revision-checked edits, dispatch receipts, stable handler identity and guarded restart. [API shapes](/Users/stephenspeicher/projects/tailterm/hub/internal/api/work_items.go:7), [store](/Users/stephenspeicher/projects/tailterm/hub/internal/store/work_items.go:208), [handler instructions](/Users/stephenspeicher/projects/tailterm/hub/cmd/tt/coordination.go:66), [host recovery](/Users/stephenspeicher/projects/tailterm/hub/cmd/tt/handler_spawn.go:116).

| Surface | Contract |
| --- | --- |
| Existing `tt inbox`, `tt post`, `tt work-items ...` | Keep as a usable fallback and debugging interface. They already act on the authoritative hub records. |
| Proposed `project_context`, `message_list` | Return explicit project/agent/run identities, roster, source authorship, sequences and pagination. Reading is not processing completion. |
| Proposed `work_item_list`, `work_item_get`, `work_item_create`, `work_item_update`, `work_item_dispatch` | Thin wrappers over existing API rules; structured item/revision/dispatch receipts, same request IDs and conflict errors. Do not duplicate store logic in prompts. |
| Proposed `message_post` | Reply with committed item/evidence references. Durable idempotent acknowledgements need a message request-ID/outbox extension; ordinary posting does not currently supply that guarantee. |

These eight tools are a starting surface, not a new authorization system. Bind actual caller identity in the adapter; model-supplied IDs are attribution data unless independently authenticated. The hub currently trusts a shared workspace credential. AIV's optional `agentId` is also a submitted label, not proof of the actor.

Return `structuredContent` plus compatible JSON text, with `isError` and actionable conflict/unavailable details. MCP specifies these result/error mechanisms; protocol request IDs alone do not implement application deduplication. [MCP tools specification](https://modelcontextprotocol.io/specification/2025-11-25/server/tools).

## One small end-to-end flow

**Pilot:** one owner message reports a reproducible hub bug, one native intake agent records it, and one independently assigned native verifier checks the relevant revision. Start with a Go backend issue because AIV currently skips Tailterm's JavaScript frontend.

| Step | Inputs and responsibility | Durable output / failure behavior |
| --- | --- | --- |
| 1. Intake | Owner message sequence + project; `database_handler` reads original text and authorship, asks only for missing reproduction essentials. | Hub retains original message. Agent stages exact create payload and stable `source-SEQ-item-N` request ID; repeated delivery reuses both. Similar titles alone do not establish duplication. |
| 2. Record | Handler calls existing CLI or proposed MCP create. | Hub commits item ID, revision, source sequence and authorship. Lost response retries exact request; changed payload under that ID conflicts. Acknowledge only after receiving/recovering the receipt. An acknowledgement failure must not create another item. |
| 3. Assign | Lead/owner selects work; dispatch includes item revision, target project and request ID. An implementer may be assigned if fixing is authorized. | Hub dispatch receipt and directed message. Ownership stays with original project. Failed dispatch remains visible as incomplete; it is not evidence that an agent started. |
| 4. Verify | Separate native verifier receives item ID/revision, explicit repository mapping, candidate commit, reproduction and acceptance criteria. It uses native tools to run a bounded check in an isolated checkout and reads AIV context. | Initially: board evidence receipt containing commit, command/check identity, environment, measured population, result and artifact location. Label this a verifier report, not AIV-certified status. AIV unavailable/stale/unobserved does not lose intake; report the gap. |
| 5. Attach AIV evidence, after binding work below | Ingest the exact candidate, select an exact snapshot/extractor, execute against that snapshot, and submit with a durable operation key. | AIV run/snapshot/audit receipt linked back to the work item. A lost AIV response requires request-key lookup/reconciliation; no blind repeat of current `record_run`. |
| 6. Resolve | Handler/lead reviews evidence and latest item revision. | Revision-checked status update plus source-linked explanation. A code fix or green check does not silently equal deployed/accepted behavior. Conflicting edits require reread/review. |

State ownership: **hub** owns conversations, work items, dispatch and agent lifecycle; **AIV** owns repository snapshots, findings/evidence and audit; **Git/test artifacts** own exact code and raw execution evidence; **browser vault** owns host credentials and local recovery/cache. The bridge should store reference mappings and retry receipts, not another editable bug database. Initially put evidence references in the existing item description/reply; typed evidence links/outbox are a later schema addition. Do not mirror AIV triage and Tailterm workflow status bidirectionally.

Lifecycle: intake remains `done` and available while its project is open, respecting explicit retirement. Verification is a bounded assignment; report result/dependencies, then retire when released. Retrying a tool operation must not create another agent. Reuse Tailterm's stable handler ID/run reconciliation and cleanup receipts. General verifier launch still has ordinary Tailterm lifecycle semantics; do not claim handler-specific retry protection covers every worker. No continuous inbox polling or new scheduler is needed for this pilot.

## Gaps that materially affect the design

1. **Exact evidence binding.** `RecordRunInput` has `commitSha`/check-version fields internally, but MCP `record_run` exposes neither; it defaults to latest ingest and allocates a new UUID on each call. Even internal commit selection does not explicitly choose among extractor snapshots. CLI `go-check` selects a snapshot, then records without passing that commit, allowing concurrent ingest to change the binding. Add explicit snapshot/extractor/check/environment identity and durable submission keys before automated evidence writes. [Submission core](/Users/stephenspeicher/projects/aidevelopment/aiv/src/verify.ts:20), [MCP write schema](/Users/stephenspeicher/projects/aidevelopment/aiv/src/serve/mcp.ts:216), [Go check CLI](/Users/stephenspeicher/projects/aidevelopment/aiv/src/cli.ts:101).
2. **Coverage.** Indexed extensions are only `.ts`, `.tsx`, `.py`, `.go`; Tailterm's `client/*.js` is outside extraction. Go materialization/module selection also needs a Tailterm fixture because the repository has multiple modules and non-Go assets. A backend pilot does not establish frontend verification. [Extension set](/Users/stephenspeicher/projects/aidevelopment/aiv/src/extract/snapshot.ts:13), [module selection](/Users/stephenspeicher/projects/aidevelopment/aiv/src/checks/go.ts:36).
3. **Findings are not full work items.** AIV has severity and four triage statuses, but no feature kind, project dispatch, message provenance, assignee or revision-CAS fields. Re-recording preserves triage status but overwrites content/defaulted origin and appends an audit event every time. Stable finding identity is not exactly-once mutation. [Finding mutations](/Users/stephenspeicher/projects/aidevelopment/aiv/src/findings.ts:65).
4. **Evidence is still a claim.** `recordRun` currently stores submitted verdict as both claim and derived verdict; the later status projection filters evidence by binding/calibration. A successful write proves recording, not that the test actually executed or the product is correct. `review_brief.evidenceBearingFiles` currently classifies files with entities, even if their statuses are unknown; inspect the statuses, not the label. [Run insertion](/Users/stephenspeicher/projects/aidevelopment/aiv/src/verify.ts:168), [brief classification](/Users/stephenspeicher/projects/aidevelopment/aiv/src/review-brief.ts:62).
5. **Repository/policy scope.** Ingest uses directory basename as repository name, and policies are global. Define explicit project→repository/checkout mapping, handle same-name repositories, and avoid presenting Tailarr-specific rules as Tailterm authority. [Repository identity](/Users/stephenspeicher/projects/aidevelopment/aiv/src/ingest.ts:73).
6. **Operational boundary.** Source exposes all AIV tools under one shared credential when configured, not per-project RBAC. Deployment revision, tool catalog, real native client loading, retained evidence, offline recovery and Claude wake-up remain unverified. Keep the first integration on the existing trusted workspace boundary; broader exposure is a separate design.

## Phased recommendation and decisions

**Phase 1:** keep the existing handler/CLI workflow, add the small Tailterm MCP adapter, and exercise the one-message pilot with AIV read tools. Preserve full native runtimes and visible pending/failed operations. Acceptance: repeated message creates one item; revision conflict is explicit; unavailable AIV preserves intake; native editor/shell tools remain available; retirement stays respected.

**Phase 2:** make AIV evidence submissions exact and idempotent, add explicit project/repository mapping plus typed evidence receipts, and prove crash/retry behavior with isolated DBs/fixtures. Include Tailterm's frontend extraction and required materialization assets before extending assurance claims beyond the Go pilot.

**Phase 3, only if useful after the pilot:** adapt native runtime hooks for durable auto-recording, add specialized adversarial/review roles, and surface evidence in Tailterm. Avoid porting DSH's interception layer, introducing automatic merge/deploy gates, or building a general orchestrator merely to record bugs.

Decisions for lead/owner: confirm hub authority for work items; choose initial Go issue/repository and one native client for acceptance; choose explicit assignment versus automatic intake routing; define what evidence permits `done`; decide whether strict authenticated per-agent attribution is required beyond the current trusted workspace.

**Effort estimate, not a commitment:** roughly 2–4 engineer-days for the adapter and isolated intake pilot; another 4–8 for exact evidence submission/retry receipts and a tested Go integration. JavaScript extraction, general hook portability and production hardening need separate estimates after fixtures expose the required scope. No implementation or deployment is authorized by this proposal itself.
