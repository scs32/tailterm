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
that does not mount the new release for every target it changes. Mini builds, then runs the candidate's
own `tt host setup --from ARTIFACT` ([host setup](host-setup.md)), which installs atomically, keeps the
previous binary as `tt.previous`, updates the hooks, restarts the relay and runs doctor; the runner then
requires the installed binary to match the pinned artifact. TailOS
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
item/order/criterion. The prevention order may have been handler-confirmed at an
earlier item revision if the narrative scope has not changed since, including
when the prevention item has since been saved as done; dismissed items do not qualify.
Preserve immediate recovery separately from permanent prevention; keep
prevention work open until verified and handler-accepted. Retirement is not exit.
A claimed run must be exited/closed or have completed exact-run rotation; host
inspection must additionally prove no active execution, known journal state and
resolved release ref. Unknown outcomes or live predecessor processes refuse.

An inspected unpublished no-effect job may requeue against the unchanged eligible
candidate and current pause generation. A held job may instead be refused
terminally after effects/ref are resolved, releasing the project fence without
pretending it released. A refused job is terminal for that job ID; the handler
can give its entry a new job by retry (Retrying a refused release, below).
Recovery history and exact retry receipts stay immutable.
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

`tt deployment list` shows bounded identity and stage summaries; `tt deployment
get --job ID` and the read-only Delivery panel provide exact verification and
integrated commit links, targets and full receipt evidence. Completion of
this implementation still requires two review rounds, distinct full-matrix
verification with owner approval, and handler-saved acceptance. Host activation,
credential provisioning and any live release remain separately ordered work.

## Bounded release listing and exact detail

`tt deployment list` reads one bounded page of **active summaries** by default.
Terminal states (`released`, `rolled_back`, `refused`, `superseded`) belong to
explicit settled history; `blocked` remains active. The HTTP endpoint is
`GET /v1/tasks/{id}/releases`. Its response is
`{version:1,jobs:[...],page:{view,limit,snapshot,nextAfter}}`. The default limit
is 50; `--limit` / `?limit=` accepts 1–200. Summaries identify their job,
row order (`rowId`), exact commits, state, generation, claim/run, input pins,
publication and settlement time. Compact terminal receipt and supersession
fields retain commit/target/outcome baseline evidence. Plans, verification
records, reconciliations, retries, paths and full receipts require detail.

- `tt deployment list --view active --limit 200` reads active work.
- `tt deployment list --view settled --limit 200` starts settled history.
- Continue with the returned `--after CURSOR --snapshot TOKEN` until
  `page.nextAfter` is empty. A nonempty cursor is not completion.
- `tt deployment get --job ID` (HTTP `?job=ID`) returns the full saved record,
  including claimed-job matrix approval hints. It is read-only and takes no
  mutation request key.

A token covers metadata for the complete task ledger, shared across both views.
Each page observes its token and bounded rows in one SQLite transaction. Any
job insertion, generation change, old-row settlement or set-aside row move
invalidates continuation; stale tokens fail with a conflict. Cursors bind the
task and view. Malformed options, wrong-view/task cursors and unsupported legacy
array responses fail closed on production readers. Restart a read traversal
from its first page after a conflict; never treat a partial history as complete.

The runner and input builder traverse both views under one token before claims,
manifest writes, reconciliation or retention. They reject duplicate jobs,
nonmonotonic row order, repeated cursors and incomplete/changed pages, then
restore ledger order before calculating the four target baselines. Fractional
and equal settlement times and legacy fallback order retain their existing
meaning. They fetch exact detail for execution, approval/input checks and locally
relevant receipt/lock recovery. Lost-finish recovery compares the **full** receipt,
including artifact and backup pins; compact summaries cannot complete a journal.
Retention considers complete history, preserving its existing count/budget and
write-before-delete receipts. A failed poll preserves the prior files and fence.

The historical `api.Client.Releases` compatibility reader remains for existing
API transport fixtures. Production CLI, runner and input paths use strict page
and exact-detail readers; they do not fall back to that compatibility method.

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
(incident #15749; the host waitlist is `wi_c5cb667695c3614c`).

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
- **Bound.** The run's bound is the one every holder of the verification host
  records: the sum over its checks of `VERIFICATION_TIMEOUT_MS` times
  `maxAttempts`, plus 30 minutes for test-binary builds, clamped to the holder
  cap (`TAILTERM_MATRIX_HOLDER_CAP_MINUTES`, default 120 minutes; see
  [objective verification](objective-verification.md), "Host lock and
  waitlist"). The worked example uses the limits in `verification/matrix.json`:
  go-race 3000000 and npm-unit 480000 at 3 attempts are 10440000 ms, plus
  1800000 for builds is 12240000 ms, which the 120-minute holder cap clamps to
  a bound of 7200000 ms. The run stops itself at that bound; the per-check
  timers inside the matrix remain the real limit. Every other host command
  keeps the 600-second limit. The per-check ceiling is `MAX_CHECK_TIMEOUT_MS`,
  3600000 ms (`scripts/verify-matrix.mjs`; the runner imports the same
  constant): a check limit that is missing or above it refuses.
- **The shared order.** The run is a member of the verification host's ordered
  waitlist, exactly like a verifier's run: urgent first, then high, then
  normal, first come first served within a priority. The runner counts no
  processes and never waits for a quiet host. It starts the run detached as
  `node scripts/verify-matrix.mjs run PLAN DIR --priority P --item ITEM
--host-wait-minutes N`, and that run takes its turn on the list.
  - `P` is the job's `priority` if the hub ever carries one, else the private
    config key `matrixPriority`, else `high`. Any value other than `urgent`,
    `high` or `normal` refuses the job as `Invalid matrix priority` before a
    plan or run is made.
  - `N` is `matrixHostWaitMs` in whole minutes (default 2 hours, at least 1).
    A run still waiting then leaves the list and the job is refused as
    `Verification host wait expired`.
  - While the run waits, the deployer posts one notice per change of its place:
    "A release job is waiting for the verification host", with `Release JOB
waits for the verification host at position P of N at priority X behind
ITEM/AGENT/pid PID` (or `with no holder`). The request id is
    `JOB-matrix-wait-COMMIT-rN-pP-ofN-HOLDERID`, so an unchanged poll posts
    nothing. `run.json` counts the places the run has had; from the second
    place on the id ends in `-nK`, so a return to an earlier place is posted
    again while a restarted runner's resend is not. `tt team queue list` and `node scripts/verify-matrix-host-lock.mjs
status` show the same place.

The context, plan, receipt, logs and the attempt record live in
`journalDirectory/ID-integrated-verification/COMMIT-rN`, keyed by the integrated
commit and the job's reconciliation count, so a handler requeue starts a fresh
run instead of reusing an earlier attempt's receipt. The run's output is in
`run.out` and `run.err` there, with its `host-lock.log` and `host-lock.json`.

**The attempt record.** `run.json` in that directory says what the runner knows
about the attempt's one run. A run is never started twice in one directory.

| `state`    | Meaning                                                                                                                                                                                                                                                                              |
| ---------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `starting` | Saved before the run is spawned, with `launchedAt`, `priority`, `hostWaitMs` and `boundMs`. If this save fails nothing is started.                                                                                                                                                   |
| `started`  | The run exists: `pid`, `processStartedAt` (its process start time, or null when it could not be read), `groups`, every check group the runner has seen it record in the lock file, and `waitPlace` with `waitChange`, its last place on the waitlist and how many places it has had. |
| `ended`    | Final, with `reason` (`receipt`, `stopped`, `wait-expired`, `no-receipt` or `spawn-failed`) and, for a refusal, its name.                                                                                                                                                            |

The journal is saved as `waiting_matrix` before the run is started, and every
poll returns at once, so the host release lock is not held while the run waits
or runs. A runner restarted between polls finds the journal and `run.json` and
resumes the same run without starting another. A `starting` record with no pid
(the save after the spawn failed, or the runner stopped in between) is resolved
from the lock file: the entry whose `output` is the attempt directory is
adopted. The receipt is read only once the run is gone, and imported only when
eligible; the journal returns to `integrated` once the imported receipt is
found.

**Stopping a run.** The runner stops its own run only past
`launchedAt + hostWaitMs + boundMs + 120 s + 60 s`, or when a requeued job finds
an earlier attempt's run still alive. It takes one step per poll:

1. Process-instance proof first: this runner process started the run and still
   holds it, or the pid's current process start time equals `processStartedAt`.
   A matching pid and directory in the lock file is not proof. Without proof
   nothing is signalled.
2. SIGTERM to the run. The run's own interruption path stops its checks,
   removes its home and releases the host lock.
3. After 30 seconds, with the same proof, SIGKILL to the run's own process
   group. Check groups are separate groups and the runner never signals them.
4. The run is confirmed stopped when its pid is gone and either its own
   `host-lock.json` records that it left the list, or its lock entry, read
   after the pid was gone, shows no check group still alive (nor any group in
   `run.json`). Only then is the job refused, as `Integrated matrix run
exceeded its bound`.

**Held.** When a run cannot be confirmed stopped, the job is held, not
refused: it stays `waiting_matrix` with the project fence, the deployer keeps
choosing it first so no other job integrates, and one notice is posted, "A
release job is held until its matrix run is confirmed stopped" (`Release JOB is
held: its matrix run could not be confirmed stopped`, with the pid, the check
groups still alive and the reason; request id `JOB-matrix-held-COMMIT-rN`). The
reasons:

- `matrix run launch unconfirmed`: a `starting` record whose run has not
  appeared in the lock file within 60 seconds. No timer refuses it: it is held
  until the run appears (and is adopted) or its `host-lock.json` shows it
  left the list.
- `no process-instance proof for the matrix run pid`: the pid is alive but may
  be another process.
- `matrix run pid still alive after SIGKILL`.
- `a check group of the matrix run is still alive`: the notice names the groups.
- `check group state cannot be shown`: the run is gone without a release record
  and another waiter replaced its lock entry before the runner read it.
- `host lock file unreadable`, `attempt record unreadable`, and `attempt
records unreadable` when a requeued job's attempt directories cannot be
  listed (request id `JOB-matrix-held-attempts`).

A held job resumes by itself once the evidence is complete (the group ends, the
run appears). Otherwise a person acts: the handler or owner confirms that no
process of that run exists (`ps -p PID`, `pgrep -g GROUP`, and `status` for the
host lock), then moves the attempt's `run.json` to `run.json.set-aside`. The
runner treats a set-aside record as ended. Never delete it.

**Requeue.** Before a job with no resumable journal touches the checkout, the
runner settles every earlier attempt of that job whose `run.json` is neither
ended nor set aside: a `started` run still alive is stopped as above, and an
unconfirmed `starting` record is held. Until all are settled the job stays
`waiting_matrix`: no checkpoint, no integration, no new run, no refusal. For the
handler, `noActiveExecution` in a reconcile or set-aside record therefore
includes "no live run named in the attempt's `run.json`" as well as no runner
holding the host release lock.

A refusal before publication records `refusalReason` in the journal and posts
"Release refused before publication" with that reason. The six names from the
integrated run are `Integrated matrix run could not start`, `Verification host
wait expired`, `Integrated matrix run ended without a receipt`, `Integrated
matrix run exceeded its bound`, `Invalid matrix priority` and `Invalid matrix
host wait`. The others are `Missing matrix prerequisites: PATHS`, `Invalid
matrix check timeout`, `Integrated matrix receipt is not eligible`, `Matrix
digest changed OLD8 to NEW8; no owner approval covers it` (below),
`verify-matrix.mjs timeout` or `exit N` from the plan command, or `unclassified` for an
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
- **Hint, then proof.** `tt deployment get --job ID` gives a `claimed` job
  `matrixApprovals` (`digest`, `messageSeq`; the newest message per digest),
  derived on each detail read and never saved. The runner takes the seq from there.
  The handler's import does not trust it: for a changed digest the hub re-reads
  the cited message and applies the same rule.
- **No covering approval.** The runner refuses the job before any matrix run,
  with `Matrix digest changed OLD8 to NEW8; no owner approval covers it` (the
  first eight hex digits of each) instead of `verify-matrix.mjs exit 1`. An
  import citing no covering approval is refused as `matrix digest changed OLD8
-> NEW8; no approval`. A refused job is terminal: after the owner approves the
  new digest, the handler retries the entry (Retrying a refused release, below)
  or the owner releases it by hand. The runner does not wait for an approval or
  ask for one.
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

### The runner's own code (wi_2be015df9af54c5c)

A release can change the scripts the deployer itself runs. One process loads
them once, so after such a release the running deployer is older than what it
just published. It checks this itself, once per poll and before any claim.

**What is compared.** The watched set is `RUNNER_CODE_FILES`, six files under
`scripts/`: `release-inputs.mjs`, `release-probe.mjs`, `release-runner.mjs`,
`release-targets.mjs`, `verify-matrix-host-lock.mjs` and `verify-matrix.mjs`. A
code digest is the sha256 of their `name:sha256` lines. The loaded digest is
read once as the process starts, from the files beside the running module. The
published digest is read at every poll from the blobs of the commit that
`refs/heads/tasks-hub` names in the deployer's checkout, never from the working
tree. The deployer treats that local ref as published: it does not fetch it
while idle, so a `tasks-hub` that moved only on origin is not seen until the
local ref moves. A change to any other file does not count.

**States.** Each poll decides one of four:

- `current`: the digests are equal. Nothing new; it claims as usual.
- `draining`: the code is stale and the deployer is not idle. It claims no new
  release and changes nothing else. A release it already owns runs to its end:
  an active release is never interrupted.
- `restart`: the code is stale and the deployer is idle. It re-execs onto the
  published scripts (below).
- `refused`: it cannot compare or cannot restart. It claims no release, with
  one of the reasons below, and keeps polling.

Idle means all three: no job of the project is `claimed`, `merged` or
`blocked`, this process has no matrix run, and no host release lock exists. In
every state but `current` a `verified` job stays unclaimed and its journal is
not archived.

**Restart.** A deployer that was told to stop does not restart. Otherwise it
first prepares the checkout: `git status --porcelain` must be empty (ignored
build outputs do not count), `HEAD` is moved to the published commit with `git
checkout --detach` when it differs, and the six files on disk must then have
the published digest. It records and announces the restart, then replaces its
own process image with the same Node executable and arguments. The pid, agent
and run stay the same. The new image is told which published digest the
restart aimed at and which digest it came from. When it then finds itself
`current` it posts "Deployer now runs the published scripts".

**Refuse reasons and recovery.** A refusal is decided again at every poll, so
one whose cause is removed clears by itself, with three exceptions.
`loaded-unreadable` never clears in the same process, because the loaded
digest is read only once, as the process starts. `exec-failed` and
`restart-did-not-refresh` hold for as long as the published digest stays the
same.

| Reason                    | Meaning                                                                                                                   | Operator action                                                                                     |
| ------------------------- | ------------------------------------------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------- |
| `loaded-unreadable`       | The scripts beside the running module could not be read when the process started.                                         | Restore the checkout's `scripts/`, then re-provision.                                               |
| `published-unreadable`    | `refs/heads/tasks-hub` does not resolve to a commit in the checkout, or one of the six blobs is missing from it.          | Repair the local ref or the missing objects; the next poll compares again.                          |
| `checkout-dirty`          | `git status --porcelain` is not empty, so the checkout was not moved.                                                     | Inspect the checkout and remove or preserve what is there; the next poll retries.                   |
| `checkout-failed`         | `git status`, `git rev-parse` or the detached checkout failed.                                                            | Repair the worktree; the next poll retries.                                                         |
| `disk-mismatch`           | After the checkout the six files are unreadable or are not the published bytes.                                           | Restore the files to the published commit; the next poll retries.                                   |
| `exec-failed`             | Replacing the process image failed. This process does not try again for the same published digest.                        | Re-provision.                                                                                       |
| `restart-did-not-refresh` | The process was already restarted for this published digest and is still stale: the restart did not change what it loads. | Re-provision, and check that the deployer's command starts the runner from the configured checkout. |

Re-provision means the action every refusal notice names: run `tt deployment
setup` for the deployer with its current run as predecessor (the provisioning
order at the top of this document). No release is active in a refusal that
followed a restart attempt, because only an idle deployer attempts one.

**Where it shows.** The private record is `runner-code.json` in the journal
directory, mode 0600: `agentId`, `runId`, `pid`, `startedAt`, `checkedAt`,
`state`, `reason` when refused, `loaded` (`digest` and per-file `files`),
`current` (the published `commit`, `digest` and `files`; null when unreadable),
`changed` (the watched files that differ) and `restartedFrom` after a restart.
It is rewritten only when something other than `checkedAt` changes, so
`checkedAt` is when the current state was first seen. Each release job's
journal also records, in `code`, the `loaded` and `current` digests of the
runner that last ran it.

The Board gets one notice per state, loaded digest and published digest. Only
12-character prefixes of the digests and the commit, names from the watched
set and the fixed reason tokens reach its text:

- "Deployer code is out of date; it restarts itself after the current release"
  (`draining`).
- "Deployer is restarting itself onto the published scripts" (`restart`). If no
  "Deployer now runs the published scripts" notice follows, the deployer did
  not come back: re-provision.
- "Deployer is not claiming releases: its scripts are out of date", or for the
  two unreadable reasons "Deployer is not claiming releases: it cannot compare
  its scripts with the published ones" (`refused`, with `Reason: TOKEN`).
  `published-unreadable` is posted only when it withholds a claim.

The request id is `runner-code-RUN-LOADED-CURRENT-STATE`, with `-REASON` for a
refusal and no time, so a restarted process resends the same notice and an
unchanged poll posts nothing. The deployer's terminal prints one line for each
of `draining`, `restart` and `refused`.

**Failed CLI calls.** When a call of the configured `tt` fails, the journal
directory keeps the evidence in `cli-failures.json`, mode 0600: for each
failure `at`, `jobId`, `agentId`, `runId`, the exact `argv`, `cwd`, `exitCode`,
`signal`, `errorCode`, the last 16384 bytes of `stderr`, `stderrBytes`,
`stderrTruncated`, `count` and `lastAt`. The newest 20 distinct failures are
kept; a repeat raises `count` and `lastAt` only. `argv` can hold Board text and
private paths, so none of it leaves the file. The Board gets "A deployer CLI
call failed" with `Deployer CLI call failed: REASON (release JOB). The exact
argv, exit code and stderr are kept in cli-failures.json in the private journal
directory of the deployer.`, once per job and reason for a process. That notice
goes through the CLI that just failed, so it is best effort; the private record
and the terminal line remain.

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
`tt deployment hand-releases` lists the records.

**A TailOS hand release needs the retained rollback copy.** With `--target
tailos` the command also takes `--deploy-config PRIVATE_PATH` (required) and
`--dist PATH`, and records nothing unless
`journalDirectory/tailos-dist-RELEASED_COMMIT` is a valid copy of what TailOS
serves. Only the runner's retain step otherwise writes that directory, and the
next job's inputs mark TailOS rollback unsafe without it. The command reads the
live `release.json` once (`targets.tailos.url` in the config, default
`https://tailos.tailarr.com/release.json`), which must name the released commit,
and compares a directory with its file map: every file by size and sha256, none
missing and none extra, `release.json` itself excluded; the directory's own
`release.json` must name the released commit.

- Without `--dist`, the retained directory must already exist and match;
  otherwise the command refuses and names `--dist PATH`.
- With `--dist PATH`, it verifies PATH the same way and retains it as the runner
  does (copy to `.tmp`, mode 0700, rename). A mismatch retains and records
  nothing. An existing valid directory is kept; an existing one that does not
  match is refused: remove it and rerun with `--dist`.

To build the copy, check out the released commit in a clean worktree and build it
as the runner does (`npm ci`, then `npm run build:static`, which writes
`dist-static` with its `release.json`; the wasm and `.build` inputs the build
needs come from the main checkout), then pass `--dist WORKTREE/dist-static`:

```
tt deployment hand-release --intervention SEQ --released-commit SHA \
  --release NAME --target tailos --job REL --repo CHECKOUT --request-id KEY \
  --deploy-config PRIVATE_PATH --dist WORKTREE/dist-static
```

Other targets take neither flag and behave as before; `--dist` without a tailos
target is refused.

The handler, with its own agent identity, then supersedes each covered job:

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

**What the hub checks.** Supersede is handler-only and applies to two kinds of
job:

- A `verified` job that no deployer holds: never claimed, or set aside by the
  handler (below) and not claimed since. Its `set_aside` records prove it had no
  release effects, and they stay in its history.
- A `refused` or `rolled_back` job with nothing left live: no receipt and never
  published; or rolled back (its receipt shows every target restored); or
  refused by a handler reconcile whose record has the release ref resolved and
  the journal `restored`, or `no_effects` while no receipt target is `released`
  or has `rollback: blocked`. A failed receipt and the reconciliation stay in
  the job's record.

Anything else is refused with a reason that names the job:

| Job                                                                        | Refusal                                                                                                                                      |
| -------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------- |
| `claimed`, `merged` or `blocked` (including a set-aside job claimed again) | `job ID is STATE; reconcile it first`                                                                                                        |
| `verified` with a requeue reconciliation or bound inputs                   | `verified job ID has deployer history (DETAIL)`                                                                                              |
| `refused` with effects that are not restored                               | `job ID has release effects that are not restored (DETAIL)`, where DETAIL names the first target still released or whose rollback is blocked |
| `released` or `superseded`                                                 | `job ID is already STATE`                                                                                                                    |

The hub also refuses unless the cited record exists in the project (`recorded
hand release required`), its released commit equals `--released-commit`, its
covered commits include the job's commit (`hand release does not cover the
job's commit`), and any `--release` equals the record's. It saves
`supersession: {releasedCommit, release, handReleaseId, targets, agentId,
runId}` and `settledAt` (the record's time, replacing a rolled-back receipt's),
keeps the job's history and retry identity, and sets the terminal state
`superseded`, shown as "superseded (released by hand)". The targets the record
shipped advance to its released commit in the baselines. Claim, the runner and
the project fence skip the job. Each use needs its own recorded order; there is
no standing order to supersede.

Restoration is the handler's inspection, not something the hub can observe: the
hub has no view of the hosts, so a reconcile that records `restored` is trusted
as every reconcile is.

**Trust model.** The hub has no repository, so it cannot check git facts: it
trusts the commits and targets in the owner's record, and the CLI's local proof
is the git check. "Owner" means a request without an agent identity, the rule
owner interventions use, not an authentication boundary; a request that carries
an agent identity is refused. What the hub adds is that a handler or a direct
API caller can no longer supersede against an arbitrary commit: it needs a prior,
immutable, audited owner record that names that commit and covers that job.

### Retrying a refused release

A job that ends `refused` or `rolled_back` is terminal, and its queue entry
still points at it. `tt deployment enqueue --entry ENTRY` on that entry answers
`entry already has release job ID (refused); retry it with tt deployment retry
--entry ENTRY --job ID --generation N --reason TEXT`. For an entry whose latest
job is in any other state it answers `entry already has release job ID (STATE);
recover the original request receipt`.

The database handler, and only the handler, gives the entry a new job:

```
tt deployment retry --entry ENTRY --job ID --generation N --reason TEXT \
  [--restored TARGET=RELEASE ...] --request-id KEY
```

`--reason` is one line saying why a new attempt is expected to pass (for
example, the environment fault that refused the first one and the item that
fixed it). The CLI refuses a missing reason or a malformed, unknown or repeated
`--restored` before it calls the hub. The item is not copied and nothing is
released by hand.

**Conditions.** The hub checks, in this order, and names the job in each
refusal:

1. The job is the entry's latest job and `--generation` is its generation.
   Otherwise `entry's latest release job is ID (STATE)` or `generation changed`.
2. The job is `refused` or `rolled_back`. A `verified`, `claimed`, `merged` or
   `blocked` job answers `job ID is STATE; reconcile it first`. A `released` or
   `superseded` job answers `job ID already released`: its change shipped, and a
   retry would ship it twice.
3. Nothing of the job is still live, by the same rule as supersede (above): no
   receipt and never published, or rolled back, or refused by a reconcile that
   records the restoration. Otherwise `job ID has release effects that are not
restored (DETAIL); roll back, reconcile, or supersede with a hand release`.
   There is no automatic retry of a job with effects. While such a job is still
   held (`merged` or `blocked`), roll its targets back by hand and reconcile it
   with the restoration recorded; it is then retryable. A job already refused
   with a record that shows live effects can be neither retried nor superseded:
   its change ships by hand, with the config baseline fallback in the runbook.
4. `--restored` names the release each target in the job's receipt runs again,
   once per target and for no other target. A job without a receipt takes no
   `--restored`.
5. The candidate is unchanged: the entry is still accepted, its item is done,
   the current verification, known failures and review acceptance still pass,
   and the item revision, commit, base commit and verification are the ones the
   refused job had. Otherwise `candidate changed since ID`.
6. The project pause generation is the one the job had. Otherwise `project
generation changed since ID`: after a project pause the entry needs the
   owner's decision, not a retry.

**What it creates.** A new job with a new ID for the same entry, item and
commit: `verified`, generation 1, queued behind every waiting job. Its
`retryOf: {jobId, generation, state, reason, restored, agentId, runId, attempt,
createdAt}` links to the job it follows; `attempt` counts the entry's jobs. The
earlier job's record is not written at all: its state, receipt, reconciliations,
generation and request receipts stay as they were, and it stays in `tt
deployment list` ahead of the new job. The queue entry shows the new job. The
new job gets its own host journal, inputs manifest and receipt, because the
runner keys all three by job ID; the earlier job's journal is untouched.

The same `--request-id` with the same arguments returns the same new job; a
changed request under that ID is refused as `retry changed`; a second retry of
the same refused job under another ID is refused and names the newer job. A new
job that is refused in turn can be retried the same way.

A retried job is integrated like any other. If the earlier attempt published
the change and `tasks-hub` still carries it, the runner's cherry-pick is empty
and the new job is refused before publication; revert the change on `tasks-hub`
first, or supersede the job with a hand release instead.

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
- No integrated matrix run of the job is alive. The host release lock is not
  held while that run waits or runs, so the lock check below does not cover it.
  On the host, every `run.json` under
  `journalDirectory/ID-integrated-verification/` must say `ended` (or have been
  moved to `run.json.set-aside` by the held-run step); a `starting` or `started`
  record means the run may be using the checkout, and setting the job aside
  would let the next job move the checkout under it. Such a holder keeps the
  fence until its run ends or the runner refuses it.
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
claim with no effects it says the handler can set it aside, with two
exceptions. If any attempt record of the job (`run.json`) is not `ended`, or
cannot be read, it says the job keeps the fence because its integrated matrix
run has not ended and may be using the checkout, and its request id ends in
`-matrix`; this covers a run that is starting, waiting, running or held.
Otherwise, if the host release lock names that claim's run (or cannot be read),
it says the job keeps the fence and needs reconcile with the lock digest after
that run stops, and its request id ends in `-locked`. Any other holder keeps the fence until
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
redeploys that directory with Wrangler. A hand release of TailOS
retains the same directory through `tt deployment hand-release --dist` (see
"Superseding jobs released by hand"). `prepare(tailos)` runs `npm ci` first when
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
targets.mini     {installPath, relayRestart: [...], relayLog, relayLabel, liveProbe: [...], hostSetup, hostRollback (both optional argv; default: tt host setup)}
targets.tailos   {url (optional), switchWindowMs (optional, 0-300000, default 90000)}
inputs.planTemplate  path to the handler's last TrueNAS preflight plan (template)
retention        {releasedBackups (optional, integer >= 0, default 3), backupBudgetBytes (optional, integer >= 0, default 4294967296; 0 = no budget)}
matrixPriority   optional: urgent, high or normal; the integrated run's place class on the verification host (default high)
matrixHostWaitMs optional: how long the integrated run waits for the verification host (default 7200000)
```

`liveProbe` is `["node","scripts/release-probe.mjs","live",TARGET,"--config",PATH]`.
Credentials stay in the existing Mini stores (SSH keys, git helper, Wrangler,
tt token) and are referenced, never copied into this file.

On 2026-10-05 the `tt` key of the deployer's private config was changed from
the stopgap wrapper put in place on 10-02 and 10-04 to the installed `tt`. The
wrapper ran an older private CLI, and newly released runner code calling that
old CLI failed in the publish step. Keep `tt` pointed at the installed `tt`,
which a Mini release updates together with the scripts.

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
   the store schema changed. `tests/truenas-release-preflight.test.js` builds its
   fixtures with the process's group, so its results do not depend on the temp
   directory's group. Each hub/bridge manifest entry records its
   `planTargets` and names `--rollback-to` and rollback probe commands for that
   target's own probed live release. Mini and TailOS get their rollback probe and retained-dist program.
   The manifest `journalDirectory/ID-inputs.json` is written once, mode 0600.
3. Run the printed `tt deployment inputs --job ID --generation N --commit SHA
--file PATH --request-id KEY` and reply to the deployer's request with the
   saved result. The deployer resumes on its next pass.

A refused run prints only its own reason. A manifest that already exists is
never rewritten; a changed job needs handler reconciliation.

### Journal retention (wi_e83171b4c215f626)

**Where backups live.** The authoritative pre-release backup is on TrueNAS at
`/mnt/deepfreeze/tailterm-hub/backups/before-ID-NAME.sqlite` (NAME is `truenas`
for a paired plan, otherwise `hub` or `bridge`). The deployer has no code path
to it and never removes it. When the store schema changed, the handler's inputs
run also copies it to the Mini as `journalDirectory/ID-NAME-backup.sqlite`. That
local copy is read only by its own job's migration rehearsal; no rollback reads
it.

**Rehearsal copy.** The rehearsal migrates
`ID-NAME-backup.sqlite.rehearsal-ID`, a copy of the local copy, and removes it
and its SQLite sidecars (`-wal`, `-shm`, `-journal`) when the rehearsal ends,
pass or fail. A failed cleanup never changes the rehearsal's result. Before
copying it saves the marker `ID-NAME-backup.sqlite.rehearsal-ID.json` (0600):

```
{version: 1, jobId, backupSHA256, startedAt, outcome: "started"}
{... outcome: "passed" | "failed", endedAt, copyRemoved: true | false}   when it ends
```

The marker holds no path, output or error text. A job whose marker exists is
never rehearsed again ("Rehearsal already attempted; inspect prior attempt"),
whatever the outcome, including `started` left by a run stopped mid-rehearsal.
A rehearsal copy with no marker does not refuse; it is replaced.

**Retention rule.** At every poll, after reconciliation and before any claim,
the daemon sweeps the journal directory. It considers only jobs in
`tt deployment list` and only the exact names `ID-truenas-backup.sqlite`,
`ID-hub-backup.sqlite`, `ID-bridge-backup.sqlite` and their rehearsal copies,
as regular files directly in `journalDirectory`:

1. A job that is `verified`, `claimed`, `merged` or `blocked`, or in any state
   the runner does not know as terminal, keeps everything, whatever the policy.
2. A terminal job (`released`, `rolled_back`, `refused`, `superseded`) loses any
   leftover rehearsal copy and sidecars (reason `terminal`). A refused job is
   terminal for that job ID even when the handler retries its entry: the retry
   is a new job with its own journal files.
3. A terminal job that is not `released` loses its backup copy (reason
   `not-released`).
4. Released jobs are ordered newest first by `settledAt` compared as time; a job
   without one is the oldest, in list order. The newest
   `retention.releasedBackups` (default 3) keep their copies; older ones are
   removed (reason `count`).
5. While the bytes of all remaining listed backup copies, non-terminal jobs
   included, exceed `retention.backupBudgetBytes` (default 4294967296, 4 GiB;
   0 means no budget), the oldest kept released copy is removed (reason
   `budget`). A non-terminal job's copy is never removed, even over budget.

Files of a job not in the list, markers, journals, manifests, receipts, plans,
`ID-migration`, `ID-mini-before`, `tailos-dist-COMMIT` and anything that is not a
regular file are never touched. Both keys are optional; a value that is not a
non-negative integer stops the daemon at startup.

**Removal record.** Before each file is removed, one JSON line is appended and
synced to `journalDirectory/retention.jsonl` (0600):

```
{version: 1, at, jobId, file, kind: "backup" | "rehearsal", bytes, reason: "terminal" | "not-released" | "count" | "budget"}
```

`file` is the name inside the journal directory. A sweep with nothing to remove
writes nothing. A sweep that fails (for example the record cannot be written)
removes nothing further, prints "Journal retention sweep failed; remaining
backup copies kept." and the poll continues. The first poll after this change
is deployed removes the existing backlog under the same rule.

## Operator runbook (d7)

**Who is releasing.** A deployer release has a job with the deployer's
`agentId`/`runId`, a journal at `journalDirectory/ID.json` and a final receipt
from `tt deployment finish`. A hand release has an owner `release` intervention
and, once the agent is provisioned, a hand release record
(`tt deployment hand-releases`) and a job the handler superseded citing it.
`tt deployment list` shows which.

**Pause.** Only between releases: exhaust all active `tt deployment list` pages
under one snapshot and confirm none has a `claimed` or `merged` job, then `tt retire DEPLOYER`; every release action is refused while
it is retired. Resume with `tt resume DEPLOYER`. Do not retire during a release:
its next fence fails, the fenced rollback cannot run, and the job is left blocked
with a pending receipt for reconciliation. `tt project-pause pause` closes the
deployer with the rest of the project; resuming then needs provisioning again.

**Reconcile.** A `blocked` job, an ambiguous journal or a stale host lock needs
`tt deployment reconcile` with a typed inspection (above). The journal's
`failure` field says which step failed and why. Never delete the
journal or lock by hand. The inspection's "no active execution" covers the
integrated matrix run as well: read `run.json` in each attempt directory of the
job and confirm the pid it names is gone (above, "Requeue").

**Stale deployer code.** A notice "Deployer is not claiming releases: ..."
means the deployer runs scripts older than the published ones and could not
restart itself. Read its `Reason:` and `runner-code.json`, then follow "The
runner's own code" above. The other code notices need no action unless a
restart is not followed by "Deployer now runs the published scripts".

**Held matrix run.** A notice "A release job is held until its matrix run is
confirmed stopped" means the deployer will not refuse or move on by itself.
Follow "Held" under integrated-commit verification: confirm nothing of that
run is alive, then move that attempt's `run.json` to `run.json.set-aside`.

**Set aside.** When a fence-wait notice says a claim with no effects blocks a
later job, the handler, under a recorded order, first confirms that every
`run.json` in the job's attempt directories says `ended` (no integrated matrix
run is starting, waiting, running or held) and that no host release lock names
that claim, then sets it aside with `tt deployment set-aside` and a typed record
(above); no deployer stop, hand release or supersede is needed. A notice whose
request id ends in `-matrix` means the run has not ended: do not set the job
aside; wait for the run, or follow **Held matrix run** below. A locked claim
goes through **Reconcile** instead. The deployer claims the waiting job at its next poll.

**Manual rollback, per target.**

- hub or bridge: `python3 scripts/deploy-truenas-hub.py --rollback-to PRIOR_RELEASE
--target hub|bridge --expect-sha256 PRIOR_SHA`, then `node
scripts/release-probe.mjs rollback TARGET --expect-release PRIOR_RELEASE
--expect-sha PRIOR_SHA --config PRIVATE`. The prior release and hash are in the
  job's manifest (`rollbackProgram`) or a previous receipt. After a paired
  release roll back both, hub first (the bridge probe needs a responding hub),
  each to its own prior release.
- Mini: run `tt host setup --rollback`, then confirm the installed `tt` has the
  prior SHA-256. If it does not (no `tt.previous`, an older CLI, a failed
  doctor), copy `journalDirectory/ID-mini-before` over the installed `tt`
  atomically (copy to `tt.rollback`, then rename) and restart the relay with the
  configured command. The runner's rollback does the same.
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
shipped and every job it carries (`tt deployment hand-release`, above; a TailOS
hand release is recorded only with its retained rollback copy, so pass
`--deploy-config` and, unless the copy already exists, `--dist`), then
order the handler to supersede each of those jobs citing the record. The
baselines then advance by themselves: the deployer and the handler's input
builder overlay each superseded job's released commit on the targets its record
shipped, and each released receipt, in `settledAt` order, so the next job selects
no target the hand release already shipped. Only then resume the deployer.

The order matters because a superseded job settles at its record's time. Resumed
before the record exists, the deployer selects against the old baselines and
redeploys the hand-released targets; and a record made after a newer deployer
receipt settles later, so its older commit would win that target's baseline.

_Fallback: a config baseline edit._ Only for a hand release that no job carries,
or whose job cannot be superseded (requeued after a claim, or refused with
effects that are not restored): for each target
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
