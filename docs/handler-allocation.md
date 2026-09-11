# Handler allocation and project continuation

Feature `wi_207f20d6eefcfa09`, In progress revision 10/scope 9, project
`tsk_2cfcff70a0fbe967`. Implementation order **#1880**, recording original
lead order #1877; deliberate selection #1872. This is a source candidate,
not deployed work or a completed feature.

## Provenance, admission and ownership

The owner’s requirement #1540 and lead #1542B/C reactivated the previously
accepted audit feature. Prior revision 8 acceptance #421 and its original
sources remain valid. Owner #1868 and lead #1869–1872 identified the separate
post-closeout idle gap: an open project still had pending work after its last
builder finished. This candidate implements the current instruction slice only.

Builder `handler-allocation` is dedicated to this feature, agent
`agt_8b6c897c46d942f1`, run `run_3cc227dcad338504`, on
`Stephens-Mini.localdomain` in
`/Users/stephenspeicher/projects/tailterm/.build/worktrees/handler-allocation`,
branch `feat/handler-allocation`. Exact clean starting baseline:
`84daa9c7ea46bf28cf407ce43d60eb4d87d05816`.

Admitted item context ends at message #1893; digest
`5e1c128c67912dce602881df0c60d548120d4db8e9130d681b715dff37e36855`.
Introduction #1894 and initial checkpoint #1897 preceded implementation.
Handler #1901 independently confirmed separate Start, authorized by lead #1895:
Queue `que_8769903ac990728d`, cycle 1/revision 6 ACTIVE, receipt
`qrr_e96956878d7e48f5`, preserving the exact worker/run, revision 10, order
#1880 and context digest. No worker item or Queue database access was used.

Lead owns orchestration, launch/release review and acceptance. The builder owns
only assigned implementation, tests and report; `db-handler` alone owns live
item reads/writes, scope recording and completion readback. No helpers were
spawned. The separate pulldown worker’s files and item were untouched.

## Resulting instructions

The handler now owns readiness, dependency, priority, occupancy and assignment
preparation in addition to audited database operations. Intake, completion and
meaningful state transitions require a readiness pass. When an independent ready
item fits capacity while a builder works, the handler supplies a second bounded
allocation and complete context to lead without another owner prompt. With no
ready work it reports the actual constraint and next meaningful checkpoint.

Before allocation the handler verifies current native revision, complete history
and explicit sources, dependencies, worker and file ownership, ordinary-member
capacity, helper lifetime allowance, open slots and admitted context limits.
Capacity must be evaluated for the actual launch path: ordinary/manual additions
and helper allowance are distinct, but an ordinary `tt spawn` inside an agent
session sets the invoking agent as `ParentAgentID` and consumes the lifetime
additional-agent allowance even with a fresh item binding. An open slot, fresh
name or item binding is no exemption. Report a real limit and use only a separately
authorized supported launch path or owner-approved allowance change. Clearing or
spoofing identity, reusing closed workers and bypassing denials are prohibited.
Context must never be truncated or replaced with another revision/run/order.
Priority informs selection among ready items, without overriding those conditions
or forcing FIFO. Duplicates reconcile against existing assignments and stable
receipts. Stale revision/context requires refreshed verification. Partial retry
identities stay frozen; no filler, duplicate worker or continuous polling is
authorized.

A complete handoff contains item/current revision/status, recorded order,
implementation owner, scope, files/artifacts, exclusions, acceptance checks,
dependencies, fresh normal worker/worktree and full admitted context. Deliberate
selection, order, admission and exact separate Start retain their distinct roles.
Send/notification alone cannot claim, launch, reassign or close work.

MAIN now requests and consumes the handler readiness pass after accepted closeout.
It deliberately selects another authorized bounded item or states the actual
constraint before yielding. It retains important message sequence, recipient/run,
checkpoint, read, Start and concrete progress evidence. A missed required progress
checkpoint gets one follow-up linked to the existing order, then explicit
escalation if still unresolved. Current roster state and exact run determine the
follow-up: message available running/done workers with the existing order and
verify read/progress, without calling `tt resume`; resume only authorized,
intentionally retained retired same-item workers while respecting explicit owner
retirement. Exited/offline workers require a dependency report and authorized
supported exact-run launch/recovery handling, preserving item identity and never
retrying unchanged Resume failures. Resolved dependencies must be consumed. Lead
continues planning/review; assigned builders implement and integrate.

Workers retain their assigned builder/read-only roles. They report checkpoint
progress/blockers and consume resolved handoffs within their recorded order.
Results carry validation, dependencies and message/receipt references. Same-item
corrections stay with the assigned worker; a new item requires fresh identity
and context. No unfinished worker/task may be closed to make space. Existing
retirement, useful descendant-service handoff, cleanup receipts and continuing
lead/handler availability remain in effect.

## Exact changed-file boundary

| File | Change |
| --- | --- |
| `client/project-handler.js` | Extend only the generated handler assignment string. |
| `hub/cmd/tt/coordination.go` | Extend existing MAIN, common work-audit/worker and handler briefing strings; no control-flow changes. |
| `tests/handler-allocation-cases.json` | Sixteen synthetic scenario requirements shared by JS/browser and Go emitter checks; thirteen apply to handler output. |
| `tests/handler-allocation.test.js` | Handler prompt contracts, real shell argument capture with fake tt, and preserved old/new recovery prompts. |
| `hub/cmd/tt/handler_allocation_test.go` | Actual isolated `cmdBrief` output for lead/handler/worker and parity with launch role emitter; task-GET-only assertion. |
| `tests/project-handler-vault-browser.mjs` | Browser-generated prompt contracts, encrypted reload checks, and focused fixture corrections described below. |
| `docs/project-work-items.md` | Matching operational guidance and delivery limits. |
| `docs/handler-allocation.md` | This complete report. |

No edits to `client/task-hub.js` or `hub/cmd/tt/main.go` were necessary:
they already carry the required fields and invoke the role emitter. The ordered
`shared/tmux.js` does not exist at this baseline. The actual
`shared/tmux-command.js` only validates and shell-quotes spawn arguments, including
the prompt; it needed no edit. These traced limits were reported in #1905/#1912.
No schema, Queue business rule, scheduler, timer/deadline, relay, network,
Tailscale, service, UI or model/provider behavior changed.

## Actual instruction delivery

| Surface | What receives the candidate text | Limits |
| --- | --- | --- |
| New browser project | `withDatabaseHandler` creates the handler assignment and inserts it after lead. `task-hub.js` passes fields into `agentSpawnCommand`, which quotes `--prompt` and `--role database_handler` for the host CLI. | Frontend source alone does not install a CLI; the host version determines common role guidance. |
| Browser handler setup/recovery | The setup flow forwards saved/user-entered prompt and handler role. An updated CLI adds handler role guidance even if that assignment is empty. | Existing encrypted saved plans preserve their previous assignment and identity. They are not silently rewritten. |
| Native CLI launch | `cmdSpawn` in `main.go` calls `agentTaskBriefingForLaunch`, appends permission/reasoning intent, assignment and admitted work context where applicable. Non-generic runtimes receive the quoted briefing argument. | This candidate changes the role strings, not launch or admission mechanics. Native model execution/obedience is not tested. |
| Generic/custom command | Unbound generic commands retain their explicit command plus assignment; handler launch options also carry `TAILTERM_BRIEFING`. Item-bound ordinary launches append full context/briefing through the existing path. | A custom command must consume the supplied argument/environment or explicitly read `tt brief`; environment availability is not proof of delivery to a model. |
| `tt brief` | The installed CLI resolves task/roster and actual database-handler role/name, then prints current common/role instructions. | It does not replay the saved assignment or admitted context, mutate allocation, or automatically instruct an existing thread. |
| Already-running threads | Only an explicitly delivered message or an actual invocation/read of a new briefing can introduce updated guidance. | Replacing a binary, changing a definition or updating browser code does not retroactively change the prompt already received. Frozen retry assignments remain frozen. |
| Host configuration/tool inventory | Runtime defaults and installed-command/tool listings describe that host. | They neither prove live-thread tools nor prove that these instructions were consumed. Initial `tt tools` observation was labelled host inventory; no provider/tool availability claim follows. |

Generated contracts do not implement deterministic allocation, a persisted
scheduler, a watchdog, authenticated per-agent permissions, or filesystem
isolation. They cannot guarantee progress while every agent is idle, nor that a
model obeys the instruction after delivery. Existing wake-up infrastructure is
unchanged and was not investigated.

## Synthetic acceptance

The corrected sixteen-scenario matrix checks clauses in actual emitted text, not a simulated
decision engine. Scenario facts are synthetic requirements; they are not fed into
a scheduler and their expected actions are not claimed as observed model output.

| Scenario | Required decision instruction verified |
| --- | --- |
| Active A + independent ready B + free capacity | Present B’s second bounded allocation/context to lead without another prompt. |
| Accepted A + pending ready B | Handler next readiness pass and MAIN consumption before yield. |
| No ready items | Actual no-ready reason and next checkpoint; no filler/poll loop. |
| Blocked dependency / later resolution | Verify dependencies, report blocker, consume resolved handoff and report resumed progress. |
| Shared-file overlap | Check ownership; serialize the actual conflict only. |
| No open slots / exhausted helper allowance | Check actual launch-path capacity and preserve unfinished workers. |
| Duplicate Send/allocation or uncertain response | Reconcile assignment/retry receipts; no automatic execution or duplicate worker. |
| Stale revision/incomplete context/partial retry | Reverify current complete context; preserve provenance and frozen identity. |
| Priority change | Choose among ready work; no override of dependency/ownership/capacity or mandatory FIFO. |
| Same-item correction / different new item | Keep correction with original worker; use fresh normal identity/context for the new item. |
| Full handoff/result/receipt | Complete scope/order/context, exact Start and progress, one follow-up/escalation, saved completion evidence. |
| New host/browser code vs existing thread | Explicit prompt/configuration/current-thread distinction and no scheduler/obedience guarantee. |
| Agent-originated item-bound launch at exhausted allowance | Parented `tt spawn` consumes lifetime allowance; fresh identity/open slot is no exemption; require separately authorized supported path or owner-approved allowance change. |
| Available running/done worker | Existing-order follow-up and read/progress verification, without `tt resume`. |
| Retained retired same-item worker | Resume only when authorized; respect explicit owner retirement. |
| Exited/offline worker | Report dependency and use authorized supported exact-run handling without changing item identity or repeating unchanged Resume failures. |

Validation commands and original/corrected outcomes are recorded below. All fixtures use
synthetic data, disposable browser storage and test-only hub/tmux resources;
no live tasks, profiles, keys, vaults or work-item/Queue data served as fixtures.

### Original candidate validation (a1c0534)

The original candidate evidence below is preserved; corrected-candidate checks
are reported separately.

- `node --test tests/handler-allocation.test.js tests/project-handler.test.js tests/tmux-command.test.js`: 25/25 pass, including twelve scenario contracts and real shell delivery to a fake `tt` that only prints arguments.
- `npm test`: 168/168 pass.
- `node tests/project-handler-vault-browser.mjs`: Chromium and WebKit pass all twelve handler contract checks each, plus encrypted write/reload/failure/recovery/export/removal checks.
- Expanded focused Go selection: 18 top-level tests passed (58 including
  subtests); one pre-existing dispatch test failed. The package run is a
  **nonpass**, not a passing full CLI suite. See the baseline comparison below.
- Final instruction/recovery/Queue selection after the last wording review:
  **16 top-level tests pass (56 including subtests), zero failures**. Command:
  `go test ./cmd/tt -run 'Test(Handler|Agent.*(Briefing|WorkAudit)|OrchestratorImplementation|RetiredIdle|WorkerBriefing|FormatWorkItemContext|BriefingCloses|QueueCLI)' -count=1 -timeout=60s -json`,
  with the same isolated environment. This narrower passing run does not erase
  the expanded selection’s baseline nonpass.
- `go vet ./cmd/tt`: pass.
- `git diff --check` and focused JavaScript/JSON formatting checks: pass.

The original candidate JavaScript, browser and focused Go runs were repeated after
its last capacity/context wording change and passed with the outcomes above. The broader
known-failing dispatch case was not silently removed or repaired.

The Go emitter fixture calls real `cmdBrief` against a synthetic HTTP task/roster,
rejects any operation except the three expected task GETs, and compares output
with the same role emitter used by `cmdSpawn`. This verifies emitted contracts
and their role ownership without launching provider agents or changing live state.

### Nonpasses and fixture corrections

The initial browser command failed before assertions because this fresh worktree
has no generated `wasm/tailserve.wasm`. The focused fixture now intercepts only
its test server’s WASM module with throwing exports: any actual WASM use fails.
The test exercises WebCrypto/IndexedDB and instructions, not SSH key generation,
Tailscale, production WASM or the application build.

After that, the pre-existing fixture read the obsolete IndexedDB `encrypted`
key and received undefined from a new v2 vault. Within the assigned focused tests,
it was corrected to require the existing `encrypted-v2` envelope. Discovery
and proposed correction were sent to handler #1912 before the edit. No storage
implementation changed. Both browser engines then passed. These first failures
are retained as nonpasses, not presented as initial passes.

### Existing CLI dispatch nonpass

The expanded selection used this command with inherited `TAILTERM_*` and
`TT_TMUX_SOCKET` removed; handler lifecycle fixtures supplied their own private
tmux socket and temporary state:

```sh
cd hub
go test ./cmd/tt -run 'Test(Handler|Agent.*(Briefing|WorkAudit)|OrchestratorImplementation|RetiredIdle|WorkerBriefing|FormatWorkItemContext|BriefingCloses|QueueCLI|WorkItemsCLI)' -count=1 -timeout=60s -json
```

`TestWorkItemsCLIUsesBodyFilesAndDurableReceipts` fails at
`work_items_test.go:127`: its old sender assertion rejects the current system
Queue dispatch notice. Eighteen other top-level tests pass (58 including
subtests). Handler discovery #1918 records the nonpass; no dispatch code or
unrelated test repair is included. The unchanged failing test also fails with a
Go source overlay substituting exact baseline `coordination.go` from
`84daa9c7ea46bf28cf407ce43d60eb4d87d05816`; no working file was reset.
Thus this is a reproduced baseline mismatch, not a candidate regression claim.
The full Go package/repository suite was not claimed or run to completion.

## Same-order review corrections #1926

Lead independently passed the original candidate’s focused JS and actual
`cmdBrief` test, then requested two instruction corrections before acceptance.
Handler #1927 saved and independently read back the full original report as
`nart_ed8c608e7819f4d4` v1, 16,438 bytes, digest
`2381be0a4f086319c9faf6f0059f8c30cc2ac086bcfa3900a5d25200cb1cb6be`, receipt
`nrr_488a137c35bdf836`, from exact source
`a1c05341ba4b5ca1b019b25103856eccea42f83e`. Original builder handoff #1922 was
separately retained as `nart_6e30429345f990d6` v1. No completion pin, revision,
scope or Queue state changed. The builder sent correction context to handler
#1929 before editing; these changes remain under #1880, with no new source-file
or runtime scope.

The initial normal/helper distinction was too broad for a parented agent launch.
Lead #1926 supplied the actual observation: a fresh item-bound launch was rejected
HTTP 409 under #1886 and needed explicit owner #1890 to increase allowance from
2 to 4. This report attributes that live observation to lead; the builder did not
query the live Queue or repeat the launch. Local source inspection confirms
`cmdSpawn` assigns the invoking agent to `ParentAgentID` for ordinary workers.
Both handler and MAIN now require checking actual launch-path eligibility and
preserve the real block, allowing only separately authorized supported launch
paths or an owner-approved allowance change. No identity clearing/spoofing,
closed-worker reuse or denial bypass is allowed.

The initial MAIN text also implied unconditional Resume before follow-up. Local
`cmdRetirement` and its existing isolated test confirm Resume accepts retired
agents only. MAIN now distinguishes available running/done, intentionally retained
retired, and exited/offline cases. The generic helper sentence is qualified too.
One follow-up/escalation, exact-run handling, explicit owner retirement and
same-item ownership remain intact. No registration, capacity, retirement or
recovery runtime behavior changed.

The shared matrix adds one actual-launch-path case and three continuation-state
cases (sixteen total; thirteen handler cases). The Go emitter test rejects both
obsolete unconditional Resume phrases and the old unqualified capacity phrase.
A separate six-state emitted-guidance check covers running, done, retired,
exited, offline and closed workers, including an exhausted allowance. It validates
state-aware instructions, not simulated lifecycle actions or model obedience.

Corrected-candidate targeted validation (same-order #1926):

- `node --test tests/handler-allocation.test.js tests/project-handler.test.js tests/tmux-command.test.js`: **26/26 pass**.
- `node tests/project-handler-vault-browser.mjs`: Chromium and WebKit each
  pass **13 handler instruction contracts** and the encrypted-plan recovery
  checks, with the same explicit test-only WASM boundary.
- `go test ./cmd/tt -run 'Test(HandlerAllocation|Agent.*(Briefing|WorkAudit)|OrchestratorImplementation|RetiredIdle|WorkerBriefing|FormatWorkItemContext|BriefingCloses|RetireAndResume)' -count=1 -timeout=60s -json`:
  **10 top-level tests pass (58 including subtests)**, zero failures, with
  inherited live coordination environment removed. Includes actual `cmdBrief`
  output, sixteen scenario contracts, six-state guidance and the existing
  retirement/Resume test. No lifecycle changes were made to live workers.
- `go vet ./cmd/tt`, focused Prettier checks and `git diff --check`: pass.

The full JavaScript suite’s 168/168 result above belongs to the original
candidate. Correction validation was targeted to the changed instruction/delivery
surfaces; no new full-suite claim replaces the recorded initial evidence or its
baseline dispatch nonpass.

## Remaining dependencies and separately bounded release proposal

No deployment is authorized by #1880. Source-candidate acceptance, any required
same-item correction, and handler revision-checked report/result storage remain
separate from actual release acceptance and feature completion.

A later recorded release order should identify the reviewed integration commit,
builder-owned integration/files, exact TailOS static package and target host
CLIs (Mini and Air if still required by that order). It should require a clean
source/package build, served hash checks on `https://tailos.tailarr.com` and
the appropriate existing local preview, atomic CLI install with retained rollback,
and synthetic installed `tt brief`/launch-output verification on each required
host. A hub redeploy is not implied by these client/CLI string changes.
No relay/network/Tailscale restart is needed for these strings.

The release must distinguish new launch prompt delivery from saved plans and
active threads. Lead should explicitly deliver the current policy to the active
handler and lead/worker threads that need it, citing item/order and recording
sequence, recipient, read, Start/progress checkpoints where applicable. A
message read alone is not acceptance or execution. Keep host version and actual
emitted text evidence separate from tool inventory/configuration.

Production remains the previously accepted Agents application
`22ab0005795c9a1201766159e7b910fcff8e2a03`. This worker did not deploy,
start a long-lived service, shut down production, close a live item/task/agent or
alter the active lead/handler policy by pretending this source candidate is
installed. Only db-handler can confirm saved feature completion after the
required lead acceptance and evidence.

