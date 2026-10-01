# Persistent project deployment

Implementation item `wi_c526e6f62370fbe9`, owner order #11932, assignment
#12432, exact builder Start #12437. Base is
`34944cf9d87a6d31bb479c824d55821650ae0dde`. This candidate does not activate
or deploy production. Owner choices #12424–#12426 select existing private Mini
credential stores, a dedicated `tasks-hub` checkout, and bounded readiness
checks (60 seconds startup, then three failures five seconds apart; hard identity
or integrity failures immediately; Mini relay must remain clean for 30 seconds).

`tt deployment template` emits the persistent generic-runtime role template.
An authorized project provisioning order uses `tt deployment setup` with a stable
`--agent-id`, explicit host checkout (`--cwd`), `--prerequisites-from` and generic
command; setup provisions the matrix prerequisites in that checkout before it
spawns (see "Integrated-commit verification" below). The handler launch
journal preserves exact process/run identity across response loss and retries;
rotation of an exited role requires its exact predecessor run. Only one unclosed
deployment identity exists per project. Item cleanup does not include this unbound
project role. Project Pause closes it with the rest of the project and blocks
all release operations; resumption requires explicit provisioning/reconciliation.
Retirement never authorizes execution or silently restarts a process.

Queue handler acceptance atomically creates a release job only when a native,
independent, eligible verification receipt binds the accepted SHA and current
scope. Legacy acceptance without that receipt remains unreleased. The handler
can also use `tt deployment enqueue --entry ... --request-id ...` for an existing
accepted entry. Deployer reads only release jobs; it does not read work-item APIs.
Jobs preserve item revision/scope/order, queue acceptance, repository/base/SHA,
plan and verification digest. Claim revalidates those sources. Exact retries
recover their original immutable action receipts, even after a later transition.

The project fence has no automatic lease expiry. A disconnected runner may still
be executing. Claimed, merged and blocked jobs exclude another release, and a
rotated run cannot use its predecessor's fence. A project-wide host lock also
excludes two local runners for the same checkout. A stale lock or ambiguous
journal is an inspection dependency, not permission to delete it and replay.
Every side effect has a saved attempting/completed journal state. Uncertain
execution is never automatically redeployed.

The release runner detaches the dedicated checkout, fast-forwards where possible
or cherry-picks the complete linear base-to-candidate series. It refuses unknown
bases, conflicts, verification mismatches and release-ref races. A changed SHA
waits before publication for a separate handler-imported release verification
receipt covering the approved matrix on that exact integrated commit. The
waiting-matrix journal can resume only with the same clean detached commit;
other unfinished journals require reconciliation. Exact-job host inputs also wait before publication for a handler-imported digest (`tt deployment inputs --job ID --generation N --commit SHA --file PRIVATE_MANIFEST`). The manifest at `journalDirectory/ID-inputs.json` binds version 1, jobId, acceptedCommit, integrated commit, verificationDigest and per-target inputs; its raw-byte SHA is immutable for that job. A reused manifest or different SHA is refused. A lost final-receipt response retains receipt_pending and retries the same write-once receipt without rolling back live-verified targets; saved hub receipts reconcile that local pending state. The daemon prioritizes its own waiting claim and refuses a later release while another claim or blocked fence remains; when a later job is waiting it posts a fence-wait notice, and a waiting claim with no effects can be set aside (see "Setting aside a job that holds the fence"). The release branch update
uses Git compare-and-swap, and after every selected target is live-verified the
runner fast-forward pushes `tasks-hub` to `origin` (see "Effects on tasks-hub").

Targets are selected from each target's last successful commit through the
integrated commit, including both sides of renames and deleted paths. Shared Go
API/store/server changes select hub, bridge and Mini; browser assets select
TailOS. Unknown paths refuse release. Each configured target has pinned artifacts
and retained rollback artifacts before activation. Hub/bridge consume
handler-created online backups and externally saved receipt pins; the deployer
never manufactures a backup pin from its input. Schema changes require
`tailterm-hub --migrate-only BACKUP_COPY`; this path opens only the supplied copy
and starts no listener, broker, relay or Tailscale node. A release that selects
both hub and bridge deploys them from ONE TrueNAS plan (`deployment.targets
["hub","bridge"]`, one release directory `ID-SHA12-truenas`, one backup and
preflight): two plans made before either deploy would each pin the other's
pre-release mount, and the second deploy would put the old partner back. The
runner prepares both, journals both effects as attempting, runs
`deploy-truenas-hub.py` once, then live-checks hub and then bridge; any failure
from the deploy on rolls back hub, then bridge, each with its own rollback
program, so both return to the prior pair. The hub goes first because the
bridge's rollback probe needs a responding hub. A plan for one target retains the
other target's live mount, and `deploy-truenas-hub.py --update` refuses it (stage
`partner-mount`, before any mutation) when that mount is not the one live now.
The runner refuses unpaired hub and bridge inputs before publication, and a plan
that does not mount the new release for every target it changes. Mini builds then installs atomically,
retains a rollback binary and restarts only its configured user relay. TailOS
builds, verifies and invokes Wrangler explicitly for project `tailos`.

Private host activation configuration is version 1 with `enabled: true`, dedicated
`cwd`, `journalDirectory`, installed `tt` path, all four last-successful
`baselines`, and stable target host/probe references. Release names are derived
from job ID, integrated SHA and target; static release/backup/rollback fields in
persistent configuration are not consumed. Each handler-pinned per-job target
record supplies fresh backup identity/hash, plan/receipt paths and external pin,
compatibility decision and rollback program. Hub/bridge backup identities must
include this job ID and name `backupJobId`; their plan binds the exact derived
release and backup destination. Module outputs are built from the exact clean
integrated checkout. Schema changes are detected conservatively from the
last-successful hub baseline through the integrated diff. Rehearsal builds a
native host hub binary from that integrated commit, verifies its hash, and
migrates a fresh copy of the handler-imported hash-verified backup. A configured
old migration binary or static schema flag cannot replace those inputs.

Mini preparation captures the currently installed binary into an exclusive
job-specific rollback file and pins its SHA before the install effect. Install
and rollback both verify that exact prior-live copy; a second release captures
the binary installed by the first rather than reusing an older rollback file.
Existing private stores contain credentials. Manifest/receipt references never
contain credential values or arbitrary captured program output.

Handler recovery uses `tt deployment reconcile --job ID --generation N --file
PRIVATE_INSPECTION --request-id KEY`. The typed inspection binds the held job,
prior exact agent/run and pause generation, incident bug and hashed incident/
journal evidence, last action and stop timestamps, expected next action, cause,
contributing conditions/unresolved questions, and an accountable prevention
item/order/criterion. The prevention order must have a current handler-confirmed
scope. Preserve immediate recovery separately from permanent prevention; keep
prevention work open until verified and handler-accepted. Retirement is not exit.
A claimed run must be exited/closed or have completed exact-run rotation; host
inspection must additionally prove no active execution, known journal state and
resolved release ref. Unknown outcomes or live predecessor processes refuse.

An inspected unpublished no-effect job may requeue against the unchanged eligible
candidate and current pause generation. A held job may instead be refused
terminally after effects/ref are resolved, releasing the project fence without
pretending it released. Recovery history and exact retry receipts stay immutable.
The runner archives a no-effect predecessor journal only when its bytes, job and
old exact run match the handler inspection; a changed or published journal does
not qualify. A stale host lock is removed only when its saved job/run and raw
hash match a handler inspection with no active execution and resolved effects/ref.
Conflict or ref race before publication is refused without a blocked project
fence; post-publication failures still retain the existing rollback/recovery gates.
The daemon skips an unclaimable verified job and continues to later jobs rather
than exiting; it never skips a real claimed/merged/blocked project fence. It
names such a fence in a notice when a later job waits behind it. Besides
reconcile, a claimed fence is freed only by its own run's refusal before
publication or by the handler's set-aside.

The live probe contract checks exact commit/artifact identity, integrity,
containers, hub response and completed migrations; Mini adds relay status and new
error count. TailOS checks the public `release.json` commit. Rollback uses retained
target artifacts in reverse order once, without replacing the live database.
Unsafe downgrade or failed rollback retains a blocked fence and emits one
escalation attempt. A failed release retains target outcomes and rollback results
in its write-once final receipt; it is never displayed as released.

`tt deployment list` and the read-only Delivery panel show release job, exact
verification/integrated commit links, targets and truthful stages. Completion of
this implementation still requires two review rounds, distinct full-matrix
verification with owner approval, and handler-saved acceptance. Host activation,
credential provisioning and any live release remain separately ordered work.

## Activation (wi_d7010deecb20211f)

Item `wi_d7010deecb20211f` (order #15262, carrying `wi_93629e4ce61658cb`
scope revision 2 and owner decisions D1, D2, #15223 and #15232) makes the agent
safe to run live. None of this activates it; provisioning, the no-effect dry run
and the first release are separate operational steps.

### Effects on tasks-hub

- **Worktree guard.** Publication and the rollback revert both refuse, before
  any effect, while any worktree of the repository has `refs/heads/tasks-hub`
  checked out (`git worktree list --porcelain`). Moving the ref under such a
  checkout would leave its working tree showing the release reversed. Before
  publication this is the ordinary refusal: one escalation, no fence.
- **Push (D2).** After every selected target is live-verified, the journal moves
  to `pushing` and the runner runs `git push origin INTEGRATED:refs/heads/tasks-hub`
  with terminal prompts disabled and credentials from the host's git helper. It
  never forces. A zero-target release still pushes. The receipt records
  `push: {remote, commit, outcome: pushed|failed}`. A failed push is escalated once
  (request `JOB-push-failure`), the outcome stays `released`, and live targets are
  not rolled back. A run stopped in `pushing` resumes the push, then finishes.
  The rollback path never pushes.
- **Revert (D1).** After a rollback of a published release (outcome
  `rolled_back` or `blocked`), the runner creates one commit whose tree is the
  pre-release `tasks-hub` tree and whose parent is the integrated commit, with
  `Release-Job:` and `Work-Item:` trailers and the dedicated checkout's configured
  Git identity (set the noreply identity there). It moves `tasks-hub` to it by
  compare-and-swap from the integrated commit. Another commit on `tasks-hub` in
  the meantime leaves the ref unchanged and the revert `failed`, which the single
  failure escalation names. It then sends one request (`JOB-rollback-bug`) asking
  the project handler to file a bug linked to the item. The receipt records
  `revert: {commit, outcome: committed|failed, bugRequestId}`; the hub accepts a
  revert only on `rolled_back` or `blocked` receipts. The next release integrates
  on top of the revert, so the rolled-back change does not ship again.
- **Failure receipt retry (f3).** The failure receipt is written like the
  success receipt: its generation is saved and the journal waits in
  `receipt_pending`, so a lost response retries the identical receipt and
  generation with no second rollback, escalation or bug request.

### Integrated-commit verification (wi_5b03fe47520b7c4f)

A cherry-picked candidate is verified by the runner itself: `verify-matrix.mjs
plan` then `run` in the deployer's checkout, then an import request to the
handler. Three things make that run succeed or fail for a named reason
(incident #15749).

- **Prerequisites.** The five files `verify-matrix.mjs` requires are
  `node_modules/.package-lock.json`, `wasm/tailserve.wasm`, `.build/test.wasm`,
  `.build/speech-fixture.wav` and `.build/go-modules.txt`; the deployer needs
  them as much as a verifier does. `tt deployment setup --cwd CHECKOUT
  --prerequisites-from SOURCE ...` runs `node scripts/release-runner.mjs
  --provision-prerequisites --from SOURCE` in the checkout before any spawn:
  `npm ci` when the install marker is missing, then each other missing file
  copied from `SOURCE` (the owner's root checkout,
  `/Users/stephenspeicher/projects/tailterm`), never overwriting an existing
  file. All five are gitignored, and a checkout left dirty is refused. It prints
  each path with its sha256 and `present`, `copied` or `installed`, and fails,
  spawning nothing, with `Missing matrix prerequisites: PATHS` naming every file
  missing from both checkouts. It is idempotent. The Playwright engines stay a
  host prerequisite, and `.build/go-modules.txt` names module directories in the
  source checkout's `.build/go`, which must remain there. Rebuild the source's
  WASM files (`npm run build:wasm -- --test`) when WASM source changes; the
  matrix's `wasm-test-build` check rebuilds them anyway when that group is
  selected. For an existing deployer checkout, run the provisioning command
  above in it (deployer retired between releases). Before the matrix runs, the
  runner checks the same five files and refuses a missing one by name.
- **Timeout.** The `run` call gets its own bound from the plan: the sum over
  its checks of `VERIFICATION_TIMEOUT_MS` times `maxAttempts`, plus 30 minutes
  for test-binary builds, with no further cap (the worked example: go-race
  1800000 and npm-unit 120000 at 3 attempts is 7560000 ms). The per-check timers
  inside the matrix remain the real limit. Every other host command keeps the
  600-second limit. A check limit that is missing or above 1800000 refuses.
- **Other matrix runs.** Verifiers run the matrix alone, so the runner waits
  while `pgrep -f '^[^ ]*node[^ ]* ([^ ]*/)?scripts/verify-matrix\.mjs
  (run|targeted) '` counts any process. The pattern is anchored at the node
  program, because agent processes whose prompt quotes the matrix command would
  otherwise count. Only the count is used: no other process's argv,
  environment, files or output is read, and nothing is signalled. While busy the
  job stays `waiting_matrix`, no plan, run or import request is made, and the
  next poll (about 30 seconds) retries. The first busy time is kept in
  `host-wait.json` in the attempt directory (below); after 2 hours (private
  config key `matrixHostWaitMs`) the job is refused and the record removed. The probe narrows, but does not close,
  the race with a verifier starting at the same moment; the matrix still refuses
  an occupied port.

The context, plan, receipt, logs and host wait live in
`journalDirectory/ID-integrated-verification/COMMIT-rN`, keyed by the integrated
commit and the job's reconciliation count, so a handler requeue starts a fresh
wait and a fresh run instead of reusing an earlier attempt's receipt.
The journal is saved as `waiting_matrix` before the run starts and returns to
`integrated` once the imported receipt is found, so a runner stopped during a
long run resumes the same job (its host lock still needs the usual inspection).
A receipt such a run leaves behind is imported only when eligible. A refusal
before publication records `refusalReason` in the journal and posts "Release
refused before publication" with that reason: `Missing matrix prerequisites:
PATHS`, `Host busy with another verify-matrix run`, `Matrix host probe
unavailable`, `Invalid matrix check timeout`, `Integrated matrix receipt is not
eligible`, `Matrix digest changed OLD8 to NEW8; no owner approval covers it`
(below), `verify-matrix.mjs timeout` or `exit N`, or `unclassified` for an
untagged error. It never claims a rollback was attempted.

**A matrix changed after approval (wi_715ea343cc605b1c).** A job is approved
under one `verification/matrix.json` digest. When tasks-hub gains a matrix
change before the job is released, the integrated commit carries another digest,
and the job's own approval does not cover it.

- **Which digest the integrated plan binds.** Always the integrated commit's.
  The runner hashes the matrix file in its checkout before the plan. Unchanged:
  the context is the job's plan with only the commit and verifier changed, as
  before. Changed: the context's `approvedMatrixDigest` is the integrated digest
  and its `matrixApprovalMessageSeq` is the owner approval of that digest.
- **What covers a digest.** An owner-authored Board message, not from an agent
  or the system, whose whole trimmed text is
  `verification-matrix-approval:SHA256` for exactly that digest (the candidate
  plan rule in [objective verification](objective-verification.md)). The
  approval of another digest, the job's included, never carries over, and
  nothing is inferred.
- **Hint, then proof.** `tt deployment list` gives each `claimed` job
  `matrixApprovals` (`digest`, `messageSeq`; the newest message per digest),
  derived on each read and never saved. The runner takes the seq from there.
  The handler's import does not trust it: for a changed digest the hub re-reads
  the cited message and applies the same rule.
- **No covering approval.** The runner refuses the job before any matrix run,
  with `Matrix digest changed OLD8 to NEW8; no owner approval covers it` (the
  first eight hex digits of each) instead of `verify-matrix.mjs exit 1`. An
  import citing no covering approval is refused as `matrix digest changed OLD8
  -> NEW8; no approval`. A refused job is terminal: after the owner approves the
  new digest, release it by hand or from a new entry. The runner does not wait
  for an approval or ask for one.
- **What the job records.** An accepted import saves `integratedMatrix`
  (`approvedDigest`, `integratedDigest`, `approvalMessageSeq`) on the job. The
  job's approved `plan` is never rewritten. A requeue or set-aside clears
  `integratedMatrix` with the rest of the integrated verification.

The handler's import (`verification` release operation) binds the integrated
plan to the approved one and requires every approved check in it. Every check
must match exactly except `go-race`: the runner builds the integrated plan with
only the commit changed, so `verify-matrix.mjs` derives go-race packages from
every path changed since the approved base, which includes the tasks-hub
commits a cherry-pick lands on. An approved `go-race` is covered by an
integrated one with the same id, cwd, environment and flags (every argv element
before the first `./` package) whose packages are a superset of the approved
list, or `./...`. Each approved check covered that way is recorded on the job
as `integratedCoverage` (`checkId`, `approvedDigest`, `integratedDigest`,
`relation: "superset"`); exact matches are not listed, and a requeue clears it
with the rest of the integrated verification. Under an owner-approved matrix
change (above) the new matrix may rebuild any check (timeouts, flags,
environment), so an approved check with no exact match is covered by the
integrated check with the same ID and recorded with `relation:
"matrix_changed"`; a `go-race` must still test every approved package, or
`./...`. Anything else is refused as
`integrated matrix omitted approved check ID`, naming the check. An approved
`./...` is covered only by `./...`, so an item approved on all packages whose
integrated run narrows to a list is refused; that needs a runner-side union, not
a retry. A request body the hub cannot decode answers 400 `invalid scope
metadata request: REASON` with a matching `code`: `body exceeds N bytes`
(`body-too-large`; 1048576 for release and item verification requests, 65536
for other scope metadata), `unknown field "NAME"` (`unknown-field`, the name
bounded to 64 bytes), `wrong type for field "NAME"` (`wrong-type`), `malformed
JSON` (`malformed-json`) or `trailing data after the JSON object`
(`trailing-data`).

### Handler requests and gating

- **Project handler (f5).** `tt deployment handler` (hub release operation
  `handler`, deployer only) prints the project database handler `{id, name}`: the
  explicit primary, else the newest open handler that is not a prepared successor.
  Retired, exited and closed handlers are excluded; with none the call is a
  conflict. The runner addresses the integrated-matrix import, job inputs and D1
  bug requests to that exact agent ID, because a finished entry holds no item
  lease and role addressing is refused in parallel mode.
- **Acknowledgement gate (f2).** A `deployment_agent` is exempt from the
  unacknowledged-work gate on release actions and posts. It cannot acknowledge
  messages; exact run and generation fence it instead.
- **Delivery (f7).** The `merged` operation sets `published`, which survives a
  later block, so Delivery shows "→ merged" for a job blocked after publication.
  Delivery also shows the receipt's push and revert.

### Superseding jobs released by hand

A supersede must cite a **hand release record** (`handReleaseId`, `hrl_…`) that
the owner recorded in the hub. The record cites an owner `release` intervention
(`tt owner intervene --kind release`) in the same project and names the released
tasks-hub commit, the targets the hand release shipped and the accepted job
commits it carries. One record per intervention; records are immutable.

The owner records it, from a checkout whose `tasks-hub` is current:

```
tt deployment hand-release --intervention SEQ --released-commit SHA \
  --release NAME --target hub --target bridge --job REL [--job REL ...] \
  --repo CHECKOUT --request-id KEY
```

Run it as the owner from a shell without an agent identity: the CLI sends
`TAILTERM_AGENT`/`TAILTERM_RUN` when they are set and the hub then refuses (409),
so from an agent or owner-helper session use
`env -u TAILTERM_AGENT -u TAILTERM_RUN tt deployment hand-release ...`.
`tt deployment hand-releases` lists the records. The handler, with its own agent
identity, then supersedes each covered job:

```
tt deployment supersede --job ID --generation N --hand-release HRL \
  --released-commit SHA [--release NAME] --repo CHECKOUT --request-id KEY
```

**Coverage.** Both commands first prove, in `CHECKOUT`, that the released commit
is an ancestor of the local `refs/heads/tasks-hub`, and that every patch of the
job's range `baseCommit..commit` is in the released commit: by ancestry, or else
by patch equivalence (`git cherry RELEASED COMMIT BASE` with no `+` line), so a
hand release integrated by cherry-pick onto a moved tasks-hub qualifies. A
missing patch is refused and named. A range that contains a merge needs ancestry,
because `git cherry` skips merge commits. Patch equivalence does not see a patch
that a later tasks-hub commit reverted.

**What the hub checks.** Supersede is handler-only and applies to a `verified`
job that no deployer ever claimed, reconciled or finished, or to a `refused` job
with no receipt; a `claimed`, `merged` or `blocked` job needs reconcile instead.
A job the handler set aside (below) is not supersedable: it is `verified` again
but keeps its `set_aside` record, so the deployer claims it again; use the
config baseline fallback in the runbook for the targets its hand release shipped.
The hub refuses unless the cited record exists in the project, its released
commit equals `--released-commit`, its covered commits include the job's commit,
and any `--release` equals the record's. It saves `supersession: {releasedCommit,
release, handReleaseId, targets, agentId, runId}` and `settledAt` (the record's
time), keeps the job's history and retry identity, and sets the terminal state
`superseded`, shown as "superseded (released by hand)". Claim, the runner and the
project fence skip it. Each use needs its own recorded order; there is no
standing order to supersede.

**Trust model.** The hub has no repository, so it cannot check git facts: it
trusts the commits and targets in the owner's record, and the CLI's local proof
is the git check. "Owner" means a request without an agent identity, the rule
owner interventions use, not an authentication boundary; a request that carries
an agent identity is refused. What the hub adds is that a handler or a direct
API caller can no longer supersede against an arbitrary commit: it needs a prior,
immutable, audited owner record that names that commit and covers that job.

### Setting aside a job that holds the fence

A claimed job that cannot progress (for example its integrated import was
refused and the fix is the next queued job) would hold the project fence
forever, because the runner resumes its own claim first. `tt deployment
set-aside --job ID --generation N --file RECORD --request-id KEY` is
handler-only and moves it aside:

- The job must be `claimed` with no effects, proven from the hub record: not
  published, no receipt and no inputs binding. The runner publishes only after
  the handler binds inputs, so such a claim has not changed tasks-hub or any
  target. `merged`, `blocked`, inputs-bound, `verified` and terminal jobs keep the
  fence and use `reconcile`.
- At least one `verified` job must be queued after it; otherwise it would only
  claim the same job again.
- No host release lock names the job's claim (its job id, agent and run). The
  hub cannot see the host, so the handler checks this when it inspects the host.
  Set-aside clears the job's run, after which no reconcile record matches that
  lock and the next job's release would find the host locked. Such a holder (for
  example a deployer stopped mid-run) keeps the fence and uses `reconcile` with
  the lock digest once its run has stopped.
- `RECORD` is the typed reconcile record with `disposition: "set_aside"`. It binds
  the job id, the claim's exact agent/run and pause generation, the incident bug,
  incident digest, stop and last-action times, stop reason, cause, contributing
  conditions, unresolved questions, and a prevention item whose order has a
  handler-confirmed scope (usually the waiting fix). `lockDigest` must be empty.
  The claim's run need not have exited.

The hub clears the claim and integrated/inputs fields, keeps the job `verified`,
appends the record to its reconciliation history, bumps its generation and moves
it behind every queued job. The waiting job claims the fence next. The set-aside
job is claimed again once the jobs ahead of it clear, so it is re-planned: it
integrates on the tasks-hub tip of that time (which by then carries the fix) and
runs a fresh integrated matrix. It may wait behind every job queued before the
move. Before that claim the runner archives its old journal as
`ID.json.set-aside-gN` (N is the set-aside job's generation) when the journal
belongs to exactly that claim and shows no effects and no publication; any other
journal is held and the job skipped for handler reconciliation.

When a job holds the fence and a later job is `verified`, the deployer posts one
notice per holder, waiting job and reason: it names the holding job, why it holds
the fence (waiting for an integrated verification import, waiting for inputs,
blocked, merged, or claimed by another deployer run) and the waiting job. For a
claim with no effects it says the handler can set it aside, unless the host
release lock names that claim's run (or cannot be read): then it says the job
keeps the fence and needs reconcile with the lock digest after that run stops,
and its request id ends in `-locked`. Any other holder keeps the fence until
handler reconciliation. Its request id is `HOLDER-fence-wait-WAITING-REASON`, so
a restart's resend returns the original.

### Probes and rollback programs

`scripts/release-probe.mjs` prints only the fields the runner reads:

- `live hub|bridge --config PRIVATE`: over the established `truenas` SSH route,
  read-only `midclt call app.config tailterm-hub` finds the mounted release
  binary, whose bytes give `artifactSHA256` and whose Go build info gives
  `commit` and `integrity` (`vcs.modified=false`). `containersRunning` comes from
  `app.get_instance`; `hubResponds` (and `migrationsApplied`, since the hub
  listens only after migrating) from `tt projects`. Output adds `release`,
  `waitedMs` and `polls`, and `capture` when the readiness window (below) ends
  not ready.
- `live mini --config PRIVATE`: the installed `tt` hash and build info,
  `relayRunning` from `launchctl print gui/UID/LABEL`, and `newErrors` counted
  in the relay log after the offset the runner saves at deploy
  (`journalDirectory/mini-relay-offset.json`).
- `live tailos`: the public `release.json` commit, from one read.
- `rollback hub|bridge --expect-release NAME --expect-sha SHA`,
  `rollback mini [--expect-sha SHA]`, `rollback tailos --expect-commit SHA`:
  `{restored, databaseWritesPreserved}`. Hub/bridge are restored when the live
  release and hash equal the expected prior values; database writes are preserved
  when the state mount is unchanged and the hub responds. `rollback hub|bridge`
  adds `waitedMs` and `polls`, and `capture` when it ends not ready. `rollback
  tailos` adds `lastCommit` (the last 40-hex commit read, or null) and `waitedMs`.

**TailOS switch window.** After `wrangler pages deploy`, Cloudflare's custom
domain keeps serving the previous deployment for a few seconds, so one read
right after a deploy or rollback can see the old commit. The runner's TailOS
live check and `rollback tailos` therefore poll `release.json` (`cache:
"no-store"`, 3 s apart, 10 s timeout per fetch) until it shows the expected
commit or `targets.tailos.switchWindowMs` ends (default 90000; an integer from 0
to 300000, where 0 means one read). Only then does a mismatch fail: the live
check fails as an identity mismatch, which `liveCheck` does not retry again, and
the rollback probe reports `restored: false`. A read that fails, is not OK or
has no 40-hex `commit` counts as no read; nothing else from `release.json`
reaches output, the journal or a message. An invalid `switchWindowMs` stops the
deployer at start, before any `tt deployment list` or claim. The last commit
seen and the wait go into the journal effect for TailOS (`liveCheck`,
`rollbackCheck`: `{lastCommit, waitedMs}`) and into the failure escalation text
(for example "TailOS live check last saw COMMIT after 90 s."). The hub receipt
does not carry them: `tt deployment finish` keeps only the known receipt fields,
and the pending-receipt recovery compares the journal's receipt with the hub's.
The pinned rollback probe argv from `release-inputs.mjs` has no `--config`, so
the automated TailOS rollback uses the default 90 s window.

**Hub and bridge readiness window.** A TrueNAS app restart, and the hub's
migrations on a large database, take longer than one read, so the hub and bridge
live and rollback probes wait for the app. The mounted release, its hash and
build info are read once (30 s timeout per read) and still fail the probe if
unreadable. Then
`containersRunning` (`app.get_instance`) and `hubResponds` (`tt projects`) are
polled 5 s apart, 20 s timeout per command, until both hold or
`targets.hub.readyWindowMs` / `targets.bridge.readyWindowMs` ends (default
240000; an integer from 0 to 300000, where 0 means one read). With 0 the live
check is a true single read: the runner's own startup retry does not apply to a
probe that reports its wait, so nothing waits for a slow start. A readiness read
that fails counts as not ready. The probe reports `waitedMs` and `polls`. An
invalid `readyWindowMs` makes the probe exit 1 and stops the deployer at start,
before any `tt deployment list` or claim. Both pinned probe commands carry
`--config`, so the configured window applies to the automated rollback too, and
a rollback probe run by hand waits the same window.

- Budget: a probe is one runner command with a 600 s timeout, and every command
  the probe runs has its own timeout. At the 300 s cap the worst case is the
  identity reads (3 x 30 s), the window, one last poll that starts as the window
  ends (2 x 20 s) and the capture (45 s): at most 475 s. At the default it is at
  most 415 s.
- Live check: a probe that waited and is still not ready fails the live check
  once (`check` returns `readiness`, which `liveCheck` does not retry), so a
  failed hub takes one window before rollback starts, not three. A probe that
  exits 1 keeps the generic `liveCheck` retry.
- Rollback: the rollback probe waits the same window before it reports
  `restored: false`, so a hub that comes up late is `restored`, not `blocked`.
  A wrong release, hash, build or state mount is not waited on: it is read once.
- Journal: the effect's `liveCheck` and `rollbackCheck` hold `{waitedMs, polls}`
  for hub and bridge, plus `capture` when the probe ended not ready. If the
  rollback program itself fails, the target is not restored, and the rollback
  probe still runs once so `rollbackCheck` records what the app looked like.

**What is captured.** When a hub or bridge probe ends not ready, `capture` holds:

- `app`: the `state` and, for up to 8 containers, `{service, state, id}` (id cut
  to 12 hex) from `app.get_instance`, or `"unavailable"` when it cannot be read.
  Nothing else from that call is kept (no `config`, `portals`, `notes`, volumes
  or ports).
- `logs`: for up to 4 containers, `{service, lines}` with the newest 40 lines
  from `midclt subscribe -n 40 -t 8 app.container_log_follow` (read as the
  probe's SSH user; `docker logs` would need sudo), or `{service, unavailable}`
  with one of `log read failed`, `no log lines`, `invalid container id` (only a
  64-hex id is placed in a command) or `time budget` (the capture's 45 s are
  used up). Each capture command has a 15 s timeout, cut to what is left of the
  45 s, so the whole capture stays inside that budget.
- Each line has control characters removed, then is redacted and cut to 300
  characters. Redacted, in order: every environment value of 6 or more
  characters from `app.config`; `Bearer` values and `token`, `secret`,
  `password`, `passwd`, `authorization` or `api key` followed by `=` or `:` and
  a value; and any run of 24 or more of `A-Za-z0-9+/_=.-` with both a letter and
  a digit (this also hides commit hashes and container ids).

The capture is best effort: it runs after the verdict is fixed and cannot change
it, the exit status or `waitedMs`. The runner reduces whatever the probe printed
to this shape and redacts it again before saving it, only in the private job
journal (`effects[].liveCheck.capture`, `effects[].rollbackCheck.capture`). It
reaches neither the hub receipt nor any message. Limitation: a container that
has already exited may return no log lines.

A probe failure prints nothing and exits 1. When a target step fails, the
journal's `failure` field keeps the first one as `{step, target, reason}`: step
is `prepare`, `rehearse`, `deploy` or `live-check`. The reason is at most 160
characters and is only a fixed message the runner or adapter tagged (for example
`live verification failed` or `release fence lost`) or, for a host program, its
name, exit status and the `classification`/`stage` of its last JSON line (for
example `deploy-truenas-hub.py exit 2: remote-operation-failed at
bridge-binary-upload`); anything else is `unclassified`. Program stderr,
messages, paths and credentials never reach it. `scripts/deploy-truenas-hub.py
--rollback-to RELEASE --target hub|bridge --expect-sha256 SHA` verifies the
retained binary at `BASE/releases/RELEASE/` over SSH, then changes only that
target's executable mount in the live app definition (`app.update`, `app.start`).
It makes no backup and touches no token, database or Tailscale state, and refuses
a missing or mismatched binary or an unrecognized live definition before any
mutation. After a verified TailOS deploy the runner keeps `dist-static` as
`journalDirectory/tailos-dist-COMMIT` (0700); the next job's TailOS rollback
redeploys that directory with Wrangler. `prepare(tailos)` runs `npm ci` first when
`package-lock.json` changed since the TailOS baseline.

### Private host configuration (keys only)

Mode 0600, never committed, never posted:

```
version: 1, enabled: true
cwd              dedicated detached tasks-hub worktree
journalDirectory 0700 directory for journals, manifests, receipts, retained artifacts
tt               installed tt path
baselines        {hub, bridge, mini, tailos}: probed live commits at provisioning
targets.hub      {host: "truenas", liveProbe: [...], readyWindowMs (optional, 0-300000, default 240000)}
targets.bridge   {host: "truenas", liveProbe: [...], readyWindowMs (optional, 0-300000, default 240000)}
targets.mini     {installPath, relayRestart: [...], relayLog, relayLabel, liveProbe: [...]}
targets.tailos   {url (optional), switchWindowMs (optional, 0-300000, default 90000)}
inputs.planTemplate  path to the handler's last TrueNAS preflight plan (template)
```

`liveProbe` is `["node","scripts/release-probe.mjs","live",TARGET,"--config",PATH]`.
Credentials stay in the existing Mini stores (SSH keys, git helper, Wrangler,
tt token) and are referenced, never copied into this file.

### Handler inputs procedure (d2)

When the deployer asks for inputs (subject "Import immutable inputs for this
release job"), the project handler, on the Mini in the dedicated checkout:

1. `node scripts/release-inputs.mjs --config PRIVATE --job ID --dry-run` and check
   the job, generation, commit, selected targets and release names.
2. `node scripts/release-inputs.mjs --config PRIVATE --job ID`. It writes one
   TrueNAS plan from `inputs.planTemplate`: when hub and bridge are both
   selected, `ID-truenas-plan.json` with `deployment.targets = ["hub","bridge"]`,
   release `ID-SHA12-truenas` for both binaries and backup
   `before-ID-truenas.sqlite`; when only one is selected, `ID-TARGET-plan.json`
   with `deployment.targets = [TARGET]`, release `ID-SHA12-TARGET`, backup
   `before-ID-TARGET.sqlite` and the other target's live mount retained. For
   each plan it runs `truenas_release_preflight.py --plan --receipt-output` once
   to create the backup, pins the receipt hash and copies the backup locally when
   the store schema changed. Each hub/bridge manifest entry records its
   `planTargets` and names `--rollback-to` and rollback probe commands for that
   target's own probed live release. Mini and TailOS get their rollback probe and retained-dist program.
   The manifest `journalDirectory/ID-inputs.json` is written once, mode 0600.
3. Run the printed `tt deployment inputs --job ID --generation N --commit SHA
   --file PATH --request-id KEY` and reply to the deployer's request with the
   saved result. The deployer resumes on its next pass.

A refused run prints only its own reason. A manifest that already exists is
never rewritten; a changed job needs handler reconciliation.

## Operator runbook (d7)

**Who is releasing.** A deployer release has a job with the deployer's
`agentId`/`runId`, a journal at `journalDirectory/ID.json` and a final receipt
from `tt deployment finish`. A hand release has an owner `release` intervention
and, once the agent is provisioned, a hand release record
(`tt deployment hand-releases`) and a job the handler superseded citing it.
`tt deployment list` shows which.

**Pause.** Only between releases: confirm `tt deployment list` has no `claimed`
or `merged` job, then `tt retire DEPLOYER`; every release action is refused while
it is retired. Resume with `tt resume DEPLOYER`. Do not retire during a release:
its next fence fails, the fenced rollback cannot run, and the job is left blocked
with a pending receipt for reconciliation. `tt project-pause pause` closes the
deployer with the rest of the project; resuming then needs provisioning again.

**Reconcile.** A `blocked` job, an ambiguous journal or a stale host lock needs
`tt deployment reconcile` with a typed inspection (above). The journal's
`failure` field says which step failed and why. Never delete the
journal or lock by hand.

**Set aside.** When a fence-wait notice says a claim with no effects blocks a
later job, the handler, under a recorded order, first confirms no host release
lock names that claim, then sets it aside with `tt deployment set-aside` and a
typed record (above); no deployer stop, hand release or supersede is needed. A
locked claim goes through **Reconcile** instead. The deployer claims the waiting job at its next poll.

**Manual rollback, per target.**
- hub or bridge: `python3 scripts/deploy-truenas-hub.py --rollback-to PRIOR_RELEASE
  --target hub|bridge --expect-sha256 PRIOR_SHA`, then `node
  scripts/release-probe.mjs rollback TARGET --expect-release PRIOR_RELEASE
  --expect-sha PRIOR_SHA --config PRIVATE`. The prior release and hash are in the
  job's manifest (`rollbackProgram`) or a previous receipt. After a paired
  release roll back both, hub first (the bridge probe needs a responding hub),
  each to its own prior release.
- Mini: copy `journalDirectory/ID-mini-before` over the installed `tt`
  atomically (copy to `tt.rollback`, then rename) and restart the relay with the
  configured command.
- TailOS: `npx wrangler pages deploy journalDirectory/tailos-dist-PRIOR
  --project-name tailos --branch main --commit-hash PRIOR --commit-dirty=false`,
  then `node scripts/release-probe.mjs rollback tailos --expect-commit PRIOR
  --config PRIVATE`. The probe polls for up to the switch window (above; the
  default 90 s without `--config`) before it reports `restored: false`.
- tasks-hub: if the D1 revert failed, commit the revert by hand in the dedicated
  checkout and move the ref with `git update-ref refs/heads/tasks-hub REVERT
  INTEGRATED` (never force).

**A blocked receipt with a committed revert.** When a rollback could not run or
did not restore a target (for example the fence was lost), the receipt outcome is
`blocked`, that target shows `rollback: blocked`, and `revert.outcome` may still be
`committed`. Then `tasks-hub` no longer contains the release while that target
still runs the released code; the escalation says so only in this case. A
`blocked` outcome with no target rollback blocked (for example a failure right
after publication, before any deploy) has no live code to restore. Roll the blocked targets
back by hand (above) and confirm with their rollback probes before reconciling
the job, so live code and `tasks-hub` agree again.

**Hand release while the agent is provisioned.** Retire the deployer (pause
above) and release by hand. Before resuming the deployer, record the owner
`release` intervention, then the hand release record with every target it
shipped and every job it carries (`tt deployment hand-release`, above), then
order the handler to supersede each of those jobs citing the record. The
baselines then advance by themselves: the deployer and the handler's input
builder overlay each superseded job's released commit on the targets its record
shipped, and each released receipt, in `settledAt` order, so the next job selects
no target the hand release already shipped. Only then resume the deployer.

The order matters because a superseded job settles at its record's time. Resumed
before the record exists, the deployer selects against the old baselines and
redeploys the hand-released targets; and a record made after a newer deployer
receipt settles later, so its older commit would win that target's baseline.

*Fallback: a config baseline edit.* Only for a hand release that no job carries,
or whose job was set aside (so nothing can be superseded): for each target
released by hand, run `node scripts/release-probe.mjs live TARGET --config
PRIVATE` (`live tailos` for TailOS) and set `baselines.TARGET` in the private
config to the printed `commit`. The running deployer re-reads `baselines` from
the private config at every poll (about every 30 seconds), so the edit applies to
its next pass with no restart; an unreadable file or a baseline that is not a full
commit holds the poll before any claim. Other config keys are read only when the
process starts. Limit: config baselines carry no time, and every released receipt
or superseded job overlays them, so a target that has any deployer receipt keeps
that receipt's commit and the edit has no effect for it.

**Detached main checkout (#15223).** The deployer's checkout is a detached
worktree (`git worktree add --detach PATH tasks-hub`) that shares objects with the
main checkout, so candidate commits resolve. At provisioning the main checkout
runs `git checkout --detach tasks-hub`; while any worktree has `tasks-hub` checked
out, the deployer refuses to publish. To update the main checkout later,
`git checkout --detach tasks-hub`. To release by hand from it (deployer retired):
build and deploy from the detached `HEAD`, then `git update-ref
refs/heads/tasks-hub NEW OLD` and `git push origin NEW:refs/heads/tasks-hub`,
without checking the branch out.

