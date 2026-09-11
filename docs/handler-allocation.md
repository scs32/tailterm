# Handler allocation and project continuation

Feature `wi_207f20d6eefcfa09`, In progress revision 10/scope 9, project
`tsk_2cfcff70a0fbe967`. Implementation order **#1880**, recording original
lead order #1877; deliberate selection #1872. The corrected candidate and
combined preparation were accepted; actual release #2004 is now deployed and
verified, pending final lead acceptance and handler saved completion. Earlier
source/preparation sections preserve their historical checkpoint status; the
actual-release section below records the current result.

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

## Combined integration and release preparation #1948

Lead accepted corrected source in #1945 and assigned same-worker preparation
#1948. Handler #1955 independently read back the current-primary native Work
order, receipt `mpr_8112a85234f83f3f`. Revision 10/scope 9, original order #1880,
Start #1901, agent/run and context remain unchanged. This stage authorizes only
isolated integration, build, validation, report and installation/rollback planning.
It does not authorize publication, CLI installation, hub changes or closure.

An isolated worktree was created at
`/Users/stephenspeicher/projects/tailterm/.build/worktrees/handler-allocation-integration`,
branch `integration/handler-allocation-prep`. A normal merge preserving both
histories combines accepted allocation
`f646981c594b96a11607d209a025717c267c9385` with independently accepted pulldown
source/report `43c1da3a81e7e30c11efaa87b69f3903004cfbc2` (Bug
`wi_85a66e159b50a309` revision 8, order #1888, acceptance #1934). Exact combined
application/source is **`bd9a1ad18f3a45be1e8ac043cbe5189a0c863741`**.
No conflicts or production edits were needed. All five pulldown files were
compared byte-for-byte against accepted `43c1da3`; both allocation production
emitters were compared against accepted `f646981`. They match. The integration
checkout remains clean at that application commit; preparation-report changes
are committed separately on the original allocation branch. Root `tasks-hub`
and the pulldown worker’s checkout/package were not mutated.

### Retained package and exact digests

The clean combined package, source archive, Darwin arm64 CLI, raw logs, synthetic
CLI outputs and browser fixtures/screenshots are retained at:

`/Users/stephenspeicher/projects/tailterm/.build/worktrees/handler-allocation-integration/.build/retained-handler-allocation-bd9a1ad`

| Artifact | Bytes | SHA-256 |
| --- | ---: | --- |
| `dist-static/release.json` | 14,861 | `db40f2f55299ae903151455a40d8475da16ce1d4dedc6f6eff3f20143945270d` |
| `tt-darwin-arm64` | 6,698,210 | `8fb4b792d2945126e90794f8ce0e597ee12fa528599de349cb191d2ba3035ddb` |
| `source-bd9a1ad.tar` | 9,809,920 | `5e506bf6659ddc06588cfd0fa62e727d6e41a67d906f8d793584335589da87d8` |
| Rebuilt `wasm/tailserve.wasm` in integration checkout | 37,951,858 | `d61326d3bd19c5b486d9c53f36b435eef54e56a0dcab2d242fcb6b51f26c8e3a` |

`npm run verify:release` passes all 82 inventory entries, including `_headers`
configuration (81 served files). The retained copy was independently rehashed
against the same manifest. Package lock and reviewed source bytes are unchanged.
Machine-readable preparation/installation/rollback plan:
[handler-allocation-preparation.json](releases/handler-allocation-preparation.json).
It records exact paths, hashes, checks, nonpasses and the pending publication slot.

Existing matching Node/Go/speech caches were reused without installing or upgrading
dependencies. Pinned Tailscale 1.102.3 source was copied into the isolated build
area before the normal build script overlaid this commit’s WASM sources. The
first offline invocation failed before compilation because automatic toolchain
verification rejected `GOSUMDB=off`. The rerun explicitly used the already-cached
Go 1.26.6 binary with `GOTOOLCHAIN=local` and `GOPROXY=off` and passed. No checksum
mismatch, source upgrade or host-toolchain installation occurred. Production
`npm run build:wasm` and `npm run build:static` passed. The static build’s existing
large-chunk advisory remains a warning, not a failed build. Pinned speech source
and chunk hashes passed the packaging checks. Darwin arm64 CLI build used
`CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags='-s -w'`.

### Combined validation and limits

- Focused handler, shell-transport, dialog and presentation command: **35/35 JS
  tests pass** on the combined source.
- Handler encrypted-plan browser fixture: **13 instruction contracts per engine**
  plus persistence/recovery pass in Chromium and WebKit.
- Accepted pulldown source fixture: both engines pass all **80 editor Escape/open
  attempts and 100 nonmodal cycles**, draft/selection/focus and queued refresh.
- Exact packaged CSS loaded into that same accepted source view/dialog fixture:
  both engines again pass **80 editor attempts and 100 nonmodal cycles**. The
  ignored preparation adapter changes only the stylesheet delivery and evidence
  directory. It preserves production CSS bytes; this is compiled CSS with source
  JavaScript, not claimed full production-bootstrap picker automation.
- Existing dropdown-dialog fixture: **4/4** Bugs/Features × Chromium/WebKit pass.
  WebKit protocol Enter still does not expose its native picker; the earlier
  limitation remains explicit.
- Focused Go role/audit/availability/context/retirement checks: **10 top-level tests
  pass (58 including subtests)**. `go vet ./cmd/tt` passes. No inherited live
  coordination identity or shared tmux socket was used.
- The actual newly built CLI ran `brief` for lead, custom-named database handler
  and worker against a synthetic local HTTP task/roster fixture. Exactly three
  task GETs occurred; no allocation/lifecycle request. Role output and capacity/
  state guidance passed, and full emitted bytes/hashes are in
  `evidence/cli-output.json` and `evidence/brief-*.txt`. This is built-binary
  output, not installed-host or model-execution evidence.
- A temporary loopback server served the exact production package to fresh
  Chromium. Production WASM started; an isolated generated key survived vault
  lock/reopen; layout margins passed and no page errors occurred. The terminal
  reached the Tailscale sign-in state without login. This was HTTP on loopback,
  not deployed HTTPS, owner-profile access, authenticated tailnet/SSH testing,
  native Safari or physical native-picker acceptance. The temporary server and
  browser closed; no useful long-lived descendant service remains.

Raw commands/logs and the two ignored preparation adapters are retained with the
package. No full combined Go repository or full combined JavaScript-suite pass
is claimed beyond the listed checks. Original candidate failures and their
baseline comparisons above remain unchanged. Pulldown’s separate extra
draft-vault fixture nonpass (obsolete v1 key lookup) remains attributed to its
accepted report; integration does not repair or hide it.

### Publication coordination and proposed rollback

Coordination #1954 asked the pulldown worker for its preparation #1935 state;
#1971 was the single follow-up after roster evidence showed it still unread.
Lead #1974 assigned **exclusive current TailOS/Mini publication to pulldown
release order #1970**, allowing root to advance separately to its accepted report
`9d1d57c`. This builder continues preparation only. Before a later allocation
release, lead must confirm pulldown’s verified release, release the publication
slot, and record the then-current root and exact served manifests/rollback.
The combined source includes accepted pulldown `43c1da3` already; later integration
must also preserve any intervening accepted reports or corrections. No older
package may silently overwrite that release.

The proposed later order installs the exact combined static package to Pages
project `tailos` / `https://tailos.tailarr.com` and the preserved Mini preview at
`http://127.0.0.1:4318`, verifying all served hashes without restarting the
preview. It atomically installs the matching CLI at `~/.local/bin/tt` on Mini and
Air, retaining the exact pre-install binary and checking current bytes again at
installation. Read-only preparation verified both current installed Agents CLI
hashes as `465498d97465994aaecdfb44ba9c7725b55dc5858547495ed0bcfaffc8d9346d`;
exact local rollback copies are retained with the package. Air was read through
SSH/SCP only; neither host executable was replaced. Installed role/launch output
must later be tested with synthetic task/roster/provider fixtures, and lead must
explicitly deliver policy to active lead/handler/relevant workers with sequence,
recipient/run and read/progress evidence. Frozen prompts are not rewritten.

Once verified, the intervening pulldown package becomes immediate frontend
rollback. Until then, the last accepted fallback supplied to this order is
Agents `22ab0005795c9a1201766159e7b910fcff8e2a03`, manifest
`bd9c2ac5753ec8455f3c85ce5a94f5fe6d4955d049e57ce210ca59204dd886f3`.
The actual release order must fill in the immediate rollback’s exact path/hash;
this preparation does not pretend to have verified a release still underway.
Keep v2-readable frontend/hub and additive data intact. No hub deployment,
database restore, relay restart, Tailscale/network/service change, CLI install,
served-package swap, publication, live record or lifecycle mutation occurred.

## Remaining dependencies and separately bounded release proposal

Source-candidate acceptance is confirmed in #1945/#1951. Preparation #1948 is
now built and verified as described above; it still needs independent lead review
and handler report/plan readback. Neither #1880 nor #1948 authorizes deployment.
Actual release acceptance and feature completion remain separate.

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

The latest completed production state supplied at original source handoff was
Agents `22ab0005795c9a1201766159e7b910fcff8e2a03`; lead #1974 now places pulldown
release #1970 underway in the publication slot. This worker did not deploy,
start a long-lived service, shut down production, close a live item/task/agent or
alter the active lead/handler policy by pretending this source candidate is
installed. Only db-handler can confirm saved feature completion after the
required lead acceptance and evidence.

## Actual release — order #2004 (verification complete; final acceptance pending)

This section supersedes the preparation-only deployment status above while
preserving that historical evidence and the immutable preparation plan. Feature
`wi_207f20d6eefcfa09` remains revision10/scope9 pending lead acceptance and handler
save/readback. Original recorded implementation order #1880 (lead #1877), exact
worker `agt_8b6c897c46d942f1` / `run_3cc227dcad338504`, context digest
`5e1c128c67912dce602881df0c60d548120d4db8e9130d681b715dff37e36855`, and separate
Start #1901 are unchanged. Handler #2009 verified native release order #2004,
receipt `mpr_dcfdf79f085d0667`. Preparation acceptance #1985/#1986 and pulldown
actual acceptance #2001 released the exclusive publication slot. This actual
release is authorized separately from source/preparation work.

### Exact integration and publication

Root `tasks-hub` was tracked-clean at
`47d3f5cfc60b3478a290d1c526379647225c0c97`. Two mechanical, conflict-free merges
integrated accepted application `bd9a1ad18f3a45be1e8ac043cbe5189a0c863741` and
preparation report `207846c4eed6019f3b05217943b968c1301401d6`, producing
`c9f8b4e7f11adaaf61f30474a8b9c3369c365a91`. Intervening pulldown report, receipt
and handoff were preserved. Differences from the frozen application were
exclusively documentation. The unrelated untracked owner screenshot was preserved.
No package rebuild, candidate edit or dependency upgrade accompanied release.

The exact retained package remains at
`.build/worktrees/handler-allocation-integration/.build/retained-handler-allocation-bd9a1ad`.
Its `dist-static/release.json` is 14,861 bytes, SHA-256
`db40f2f55299ae903151455a40d8475da16ce1d4dedc6f6eff3f20143945270d`, with82 inventory
entries including `_headers` and81 public assets. Darwin arm64 CLI is6,698,210
bytes, SHA-256 `8fb4b792d2945126e90794f8ce0e597ee12fa528599de349cb191d2ba3035ddb`.
The source archive is9,809,920 bytes, SHA-256
`5e506bf6659ddc06588cfd0fa62e727d6e41a67d906f8d793584335589da87d8`.
All retained bytes were reverified before publication.

Executed from the retained directory:

```sh
npx wrangler pages deploy dist-static --project-name tailos --branch main --commit-hash bd9a1ad18f3a45be1e8ac043cbe5189a0c863741 --commit-dirty=false
```

Exit0; cached Wrangler4.131.0 uploaded14 files with68 existing. Native deployment
list readback confirms Production/main source `bd9a1ad`, deployment
`4cd51bdc-ea7b-48c0-829c-2adb6673aec6`, immutable origin
`https://4cd51bdc.tailos.pages.dev`. Custom origin is
`https://tailos.tailarr.com`; Mini remains `http://127.0.0.1:4318`. Checkpoint #2014
provided exact deployment/manifest information for independent lead verification.
The old `tailterm` Pages project was not targeted.

Before publication, custom and Mini both served accepted pulldown `43c1da3`,
manifest `a560d5db9ce36d0eda86732b1765b82a5db60960ccc4de96d03149f767ca54da`.
Both the live Mini directory and retained pulldown package passed all82 local
inventory hashes. A verified clone of the new package was staged beside Mini's
live directory, then exchanged using same-filesystem macOS `renameatx_np`
`RENAME_SWAP`. The exact old directory is retained at
`.build/releases/handler-allocation-previous-mini-2004`. New and old inventories
were checked after exchange. Preview **PID60799/PPID1** stayed unchanged; no
preview or other production service was restarted.

### Served frontend and independent checks

The worker independently checked all three origins with identity encoding:
manifest bytes/hash, canonical `/` index,81 public asset sizes/SHA-256 hashes,
applicable MIME types and absence of unexpected Content-Encoding. Raw `.wasm.gz`
bytes retain the gzip MIME contract. All249 origin checks passed. Each origin
also passed a fresh disposable Chromium production-bundle/WASM smoke: synthetic
vault/key creation, clipboard copy, lock/reopen key recovery, three viewport
layouts, Tailscale sign-in readiness without login, and no page errors.

Lead #2015 independently executed249 origin checks and a fresh custom-origin
production Chromium/WASM/vault/clipboard/layout run. Its logs are
`/tmp/tailterm-handler-release-assets-review.log` and
`/tmp/tailterm-handler-lead-deployed.log`; these are independent evidence, not
worker rerun attribution. The worker's raw evidence, per-asset receipts and
screenshots are retained under package `actual/{immutable,custom,mini}` and
`actual/origins.json`.

These production checks complement the accepted combined preparation checks:
focused JS35/35; handler clauses13 per Chromium/WebKit plus encrypted-v2 recovery;
pulldown source and exact compiled-CSS fixtures total80 editor attempts and100
nonmodal cycles across Chromium+WebKit (40/50 per engine); existing dropdown4/4;
Go10 top-level tests,58 total including subtests, and vet.
Compiled-CSS fixtures use accepted source interaction modules, not full compiled
bootstrap picker automation. No native Safari, physical pointer or owner-device
verification is claimed.

### Installed CLI and actual launch delivery

Mini and Air each still had the expected Agents CLI SHA-256
`465498d97465994aaecdfb44ba9c7725b55dc5858547495ed0bcfaffc8d9346d` immediately
before replacement. Exact backups were retained on each host at
`~/.local/bin/tt-before-handler-allocation-bd9a1ad`; conflicting backups would have
stopped installation. The reviewed candidate was staged in `~/.local/bin`,
verified executable and SHA-256, and atomically renamed to `tt`. Mini installation
was2026-09-11T03:54:39.999808Z; Air2026-09-11T03:54:43.306793Z. Both installed
binaries now hash `8fb4b792d2945126e90794f8ce0e597ee12fa528599de349cb191d2ba3035ddb`.
No installed relay or running agent process was restarted.

Each installed binary was exercised against a disposable loopback HTTP protocol
stub with a private tmux socket, explicit empty tmux config, isolated relay state
and fake Python provider. Minimal test environments did not inherit live thread,
hub configuration or credentials. Three installed `brief` calls and three actual
`spawn`/`wrap` executions captured lead, database-handler and ordinary worker
arguments. Each provider received exactly one complete prompt argument. Both hosts
produced byte-identical role briefings and launch captures. The ordinary worker
retained exact synthetic task/item revision3/order17/run/context and item environment;
the two project-role fixtures correctly remained unbound. Only the synthetic hub
received registrations/events. No real model, live work item or live helper allowance
was used. This protocol stub verifies installed command delivery, not an additional
production-hub admission or authorization test.

| Installed role fixture | Brief bytes | Brief SHA-256 |
| --- | ---: | --- |
| Lead | 11,627 | `51d08b8bab2998aa7799a927f41dc315ae193f238f6c0cc9a5dc0236d5b7bb52` |
| Handler | 11,506 | `bfdd011dd102a109b7d351b815069ac2890cb3ff1f26875ec0ea93a14df0cd41` |
| Worker | 7,566 | `47984d244d61c96e295949d93b7b06f4eff4f54de1a4b522968cf287933c1614` |

Complete prompts, selected exact identity environment, synthetic request log and
receipts are in package `actual/cli-mini` and `actual/cli-air`. Private tmux
servers and fake providers were stopped and verified absent. Air temporary scripts,
candidate transfer and evidence directory were removed only after exact local copy
verification; installed CLI and rollback backup remain. No useful long-lived test
service remains.

### Active-thread delivery and follow-through

Installed-policy checkpoint #2016 supplied exact role texts/digests to lead.
Lead #2018 explicitly delivered worker policy to this exact existing worker/run,
requiring actual installed `tt brief` and substantive final evidence instead of an
acknowledgement. The worker read its real project-bound installed briefing:
7,012 bytes, SHA-256 `17b482b2ca8a9356fcc82fbff05666e4ce17eeb196aa2735e125d7c537f1d01f`,
retained in package `actual/current-worker-brief.txt`. The original work-item
binding, Start and scope are preserved; synthetic identities are not adopted.
Finishing this report/receipt and cleanup inventory is the required concrete
same-item progress checkpoint.

Lead also read its real installed briefing at
`/tmp/tailterm-lead-installed-brief-2016.txt`,11,062 bytes, SHA-256
`53e56702eae59948a08ec3e55354bf2bc5939618b104394e5921a15772de183d`, as reported in
#2018. Handler delivery/readiness follow-through is recorded by lead #2022 below. These facts
establish particular deliveries and actions, not deterministic model obedience.
Startup strings do not rewrite frozen saved launch plans, automatically alter
active model contexts, install a persisted scheduler, or establish per-agent
technical authentication/filesystem isolation.

Lead #2022 verifies concrete handler delivery and the resulting readiness action:
recipient `agt_3760a4361c514a03` / `run_2dd77d68cb7bcb39` received #2017,
read through #2017, and read its actual installed briefing (10,941 bytes, SHA-256
`1a71327756f9d8b893904f16b9433cc70a49371d766ddeec90e4399a89ba5107`). Following
one same-order progress follow-up #2020, handler returned substantive native
readiness/capacity result #2021. Lead independently hashed the559,523-byte
readiness archive as
`42c79e339a68754d3238a4098c9b029d64cf9b253fc326b1428e211b2a1e3d4e` and accepted
the concrete no-launch-capacity disposition: maxNewAgents4, spawning enabled,
four parented lifetime identities and three open agents. Potential search,
Random Flashes and wizard work is not selected/admitted/started. Next allocation
requires recorded capacity authorization and refreshed handler handoff; closed
identity reuse, clearing parent identity, allowance changes or an inferred launch
path are not authorized. This worker consumes the attributed result/disposition
without accessing live item/Queue records or taking over allocation.

Lead's exact current recipient is `agt_1186df8d710c0fd7` /
`run_9e0ddb28df5ae63e`. It applied the policy by delivery, one follow-up,
independent review and the explicit readiness disposition. Worker #2019 is the
substantive report/preflight response to #2018; this final artifact incorporates
#2022. Read snapshots reported by lead are
`/tmp/tailterm-policy-roster-after-delivery.json` and
`/tmp/tailterm-roster-2019.json`. Lead accepts delivery step6 as satisfied in
#2022, receipt `mpr_deaa41dea3a145f8`; final actual release acceptance remains
separate. Handler readiness facts are attributed to #2021/#2022 rather than
claimed as worker database verification. No duplicate readiness loop is needed
before a meaningful change.

### Nonpasses, rollback and remaining acceptance

The initial prepublication Python urllib request received HTTP403. Canonical
curl against the same URL passed without credentials or configuration changes.
The first Mini installed fixture checked the internal literal `database_handler`
inside human-readable prose; the emitter correctly says `durable Database handler`.
Only that harness assertion changed, failed evidence was retained, and fresh
isolated Mini/Air runs passed. Initial process preflight used a session-only display
target without a pane PID; corrected `list-panes` yielded the exact process tree.
Lead #2015 separately reports a duplicated relative runner path corrected before
its successful browser run. The browser test's raw Mini log inherits an `https:true`
label; the actual tested origin is HTTP and receipts record `http:` correctly.
None of these harness/transport nonpasses required a product change or rollback.
Earlier preparation/toolchain and baseline Go/pulldown fixture nonpasses remain
explicitly preserved above and in the actual receipt. No broad full-suite pass is
inferred from the focused evidence.

Immediate frontend rollback is the accepted pulldown package
`.build/releases/pulldown-recurrence-43c1da3/dist-static`, deployment
`621eb35c-7578-4c8c-9d4a-e16eea01d196`, manifest
`a560d5db9ce36d0eda86732b1765b82a5db60960ccc4de96d03149f767ca54da`, plus the exact
previous Mini directory named above. The older Agents frontend is not the immediate
rollback. CLI rollback uses each verified host-local backup. Retain v2-compatible
frontend/hub and newer data; no database restoration is part of this release.
Rollback was not used.

Fresh exact-run inventory identifies worker session `$52`, created1789095750,
pane PID26335. Its seven freshly inspected descendants had no TCP listeners; preview
PID60799/PPID1 is outside that tree. Only this worker's test resources were cleaned.
No hub deployment, Air frontend change, network/Tailscale/relay modification,
production service shutdown or item/task/agent closure occurred.

The new [actual release receipt](releases/tailos-2026-09-10-handler-allocation.json)
is separate from the unchanged preparation plan. Final report/handoff edits are
documentation only and do not alter published bytes. Lead final acceptance,
handler revision-checked saved report/completion/Queue readback, and any subsequent
explicit worker release remain pending. This worker does not self-close or claim
saved feature completion from successful tests.
