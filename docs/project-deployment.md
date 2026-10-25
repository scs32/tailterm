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
other unfinished journals require reconciliation. Exact-job host inputs also wait before publication for a handler-imported digest (`tt deployment inputs --job ID --generation N --commit SHA --file PRIVATE_MANIFEST`). The manifest at `journalDirectory/ID-gN-inputs.json` (N is the job generation the handler's inputs run read) binds version 1, jobId, that generation, the attempt, acceptedCommit, integrated commit, verificationDigest and per-target inputs; its raw-byte SHA is immutable for that job generation. The runner uses the generation-named manifest whose bytes hash to the imported digest and refuses one that records another generation or attempt, and refuses with a digest-mismatch reason when no generation-named manifest hashes to that digest (see "Handler inputs procedure"). A lost final-receipt response retains receipt_pending and retries the same write-once receipt without rolling back live-verified targets; saved hub receipts reconcile that local pending state. The daemon prioritizes its own waiting claim and refuses a later release while another claim or blocked fence remains; when a later job is waiting it posts a fence-wait notice, and a waiting claim with no effects can be set aside (see "Setting aside a job that holds the fence"). The release branch update
uses Git compare-and-swap, and after every selected target is live-verified the
runner fast-forward pushes `tasks-hub` to `origin` (see "Effects on tasks-hub").

Targets are selected from each target's last successful commit through the
integrated commit, including both sides of renames and deleted paths. Shared Go
API/store/server changes select hub, bridge and Mini; browser assets select
TailOS. Unknown paths refuse release. Each configured target has pinned artifacts
and retained rollback artifacts before activation. Hub/bridge consume
handler-created online backups and externally saved receipt pins; the deployer
never manufactures a backup pin from its input. Schema changes require
`tailterm-hub --migrate-only BACKUP_COPY`, run in a container under the
production hub constraints (see "Migration rehearsal container"); this path
opens only the supplied copy and starts no listener, broker, relay or Tailscale
node. A release that selects
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
Linux hub binary from that integrated commit (`GOOS=linux`, the release host's
own architecture), verifies its hash, and migrates a fresh copy of the
handler-imported hash-verified backup inside a container. The binary never runs
on the release host itself. A configured old migration binary or static schema
flag cannot replace those inputs.

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

One stop needs none of this: a clean stop (SIGTERM or SIGINT to the runner)
while the run's own claim has no effects. The runner then gives the claim back
with `tt deployment unclaim`, an exact-run record the hub builds itself
(disposition `unclaimed`, no incident or prevention item), and exits; the job is
`verified` again in its queue place ("Pause", in the operator runbook below).
An unclean exit (a crash, SIGKILL or host loss), a stop after any effect, and
every other held claim still need the typed reconcile above.

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
posts one notice for the skipped job (**Held claim**, below) and names a real
fence in a notice when a later job waits behind it. A claim that fails only in
transport is not a held claim: the poll ends there and the transport wait
covers it. Besides
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
`{version:1,jobs:[...],page:{view,limit,snapshot,nextAfter},window}`, where
`window` is the project's release quiet window with `open` and `until`, absent
when none is set (see "Release quiet window"). The default limit
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

While the quiet window is open, a list first holds every covered verified job
(W2 in "Release quiet window"). That write is a ledger change like any other: a
traversal that crosses the window's start fails as stale and restarts.

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
- **Target selection.** Targets are selected from the integrated commit before
  a single job's integrated matrix run, the inputs request and the push. A
  batch is selected the same way, on the batch's integrated commit before the
  batch's matrix run starts. A batch whose selection fails starts no matrix
  run: it is dropped at the hub, its members are marked to release alone, and
  the journal keeps `batchFallback` with reason `batch-selection-refused` and
  the selection's reason as `detail`. Each job is then selected alone, so the
  job that has the path is refused as below and the others release. When the
  hub refuses the drop, the lead is refused with the selection's reason. A
  changed path that
  no map knows is the ordinary refusal before publication: one escalation whose
  reason is `Unknown release path: PATH` (with `and N more` for further paths),
  journal step `select`, no revert and no bug request. A path the running map
  does not know is accepted when the candidate's own
  `scripts/release-targets.mjs` maps it, so a new folder and its rule ship in
  one release; that release selects all four targets, because the map file
  changed. The running map's rules are the floor: the candidate's map is asked
  only about paths no running rule matches, adds targets and never removes one,
  and cannot relax the invalid path rule. The candidate's map runs in a short
  child process in the dedicated checkout, only while that checkout is at the
  integrated commit with `scripts/` and `tests/` unmodified. When it cannot be
  evaluated (no map in the commit, another checkout state, a failed child or
  output of another shape), the path stays unknown.
- **No-change result (wi_d46b5de7ad79c8c3).** An accepted result that
  `tasks-hub` already carries integrates as nothing. It is carried when its
  commit equals its base (after the tip has moved on), or when its commit is
  an ancestor of the tip and every path it changes has the commit's content at
  the tip. Ancestry alone is not enough: after a rollback the revert keeps the
  commit an ancestor without its content, so a retry is cherry-picked and
  ships the change. A carried result has no cherry-pick, no changed
  paths, and the checkout stays on the tip it found. Nothing is selected,
  verified, published, deployed or pushed, and `tasks-hub` does not move. The
  hub records the job `merged` at the job's own commit, which needs no
  integrated matrix receipt, and it settles `released` with a receipt naming
  that commit, no targets and no push. The journal saves `mergedSent` before
  that call, so a call that fails or loses its reply ends `blocked` for
  handler reconciliation, as after publication, and is never refused. The
  journal keeps the reason as
  `noChange: "candidate-already-on-tasks-hub"`; the hub's receipt has no field
  for it. Whatever the tip carries that is not yet deployed ships with the
  next release that has a change. A commit that only fast-forwards the tip
  (including one equal to it) integrates as before, and a merge commit in a
  series that must be cherry-picked is refused as `Nonlinear candidate series`.
- **Push (D2).** After every selected target is live-verified, the journal moves
  to `pushing` and the runner runs `git push origin INTEGRATED:refs/heads/tasks-hub`
  with terminal prompts disabled and credentials from the host's git helper. It
  never forces. A zero-target release still pushes. The push is limited to
  5 minutes (`PUSH_TIMEOUT_MS`, 300000 ms; it was an unstated 120 s until a
  hand push on 2026-10-19 took more than 60 s), and there is no retry. The
  receipt records `push: {remote, commit, outcome: pushed|failed}`; a
  successful push keeps exactly that shape. A failed push adds typed, bounded
  fields, never captured output: `timedOut` (a push stopped by the limit is
  `failed` with `timedOut: true`), `exitCode` (1-255) or `signal` (lowercase,
  such as `sigterm`), `elapsedMs`, `reason` (one stable tag: `push-timed-out`,
  `push-killed`, `push-rejected`, `push-auth-failed`, `push-network-failed` or
  `push-failed`), `remoteCheck` and `stderrSHA256`. The hub refuses any other
  value. The push's stderr, with URL credentials and credential-shaped text
  redacted and cut to its last 2 KB, is kept only in the private job journal
  (`pushAttempts`, the last five); the receipt holds at most its sha256. git
  runs as its own process-group leader. After any failed push, including one
  stopped by the limit, the runner signals the whole group, so a transport
  child (ssh or a remote helper) cannot land the push after the re-check:
  SIGTERM, then SIGKILL after 5 s (`PUSH_STOP_GRACE_MS`), and it waits at most
  15 s (`PUSH_STOP_WAIT_MS`) for no process to be left in the group. Only once
  the group is gone does the runner read origin's `tasks-hub`, once
  (`git ls-remote`, 30 s): `landed` when it is the released commit, `absent`
  when it is any other commit or missing, `unknown` when the read fails. A
  group still present after the wait is not read and counts as `unknown`; the
  journal attempt records `stopped: false`. A push that `landed` keeps
  its failure fields and is not escalated. Otherwise it is escalated once
  (request `JOB-push-failure`), and the notice names the cause from the typed
  fields only, for example `timed out after 300 s (push-timed-out)` or
  `exit 128 after 2 s (push-auth-failed)`, the remote check, and the first 12
  characters of the stderr hash. The outcome stays `released`, and live targets
  are not rolled back. A hub or `tt` older than these fields drops them; the
  runner then accepts the saved receipt without them. Before it waits on the
  push, the runner saves the group in the private job journal (`pushGroup`:
  the group id, the leader's `ps` start time and the host's boot identity,
  Linux's `boot_id` or macOS's `kern.boottime`); it never reaches the
  receipt, and it is cleared once the group is gone. A run stopped in
  `pushing` resumes the push, then finishes. The resume first settles a
  journaled group (wi_56e50fb1bc6bbdaa). A group that is gone, or whose id now
  belongs to another start time or boot, is over: it is never signalled and
  the push and re-check proceed as above. A group still present whose leader
  has the recorded start time on the recorded boot is the earlier push: it is
  stopped with the same bounds (SIGTERM, SIGKILL after 5 s, at most 15 s),
  and once it is gone origin's `tasks-hub` is read before any new push; a
  `landed` commit needs no new push and is recorded `pushed`. A group that
  outlives the bound, or is present without that proof, is not signalled
  again or read and nothing is pushed: the receipt records `failed`,
  `reason: push-failed`, `remoteCheck: unknown`, escalated once as above. A
  clean runner stop (SIGTERM or SIGINT) during a push does not end it: the
  push runs to its result or its 5-minute limit and the release finishes as
  above before the runner exits, so only a runner killed mid-push (a crash,
  SIGKILL or host loss) leaves a journaled group for the resume. The rollback
  path never pushes.
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
  on top of the revert, so the rolled-back change does not ship again. A failed
  release with no receipt (published on tasks-hub, but the hub's `merged` reply
  never confirmed) records its revert on its block instead; see "Revert on the
  block" below.
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
  missing from both checkouts. It is idempotent. A copied
  `.build/go-modules.txt` brings the source's `.build/go-licenses/` licence
  copies with it, whole, only when the source's
  `.build/go-licenses/go-modules.txt` matches the copied list byte for byte and
  the checkout has no `.build/go-licenses/` yet; it is never written over.
  Otherwise none of that tree is copied and the command says why on stderr (the
  matrix run prints it too). Then the licence texts come from the module
  directories the list names in the source checkout's `.build/go`, which must
  remain there. The Playwright engines stay a host prerequisite. Rebuild the
  source's WASM files (`npm run build:wasm -- --test`) when WASM source
  changes; the matrix's `wasm-test-build` check rebuilds them anyway when that
  group is selected. For an existing deployer checkout, run the provisioning command
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

**Which paths the integrated run selects its checks from.** The runner
integrates a candidate onto the current tasks-hub tip, and the integrated plan
selects checks from the diff between that tip and the integrated commit, plus
each job's own approved changed and owned paths. The tip was verified by the
release that published it, so releases that landed after the job's base no
longer add their paths: a docs-only job on an old base runs the docs checks, not
the whole matrix. A batch uses its combined diff, the tip to the batch's
integrated commit, plus every joined job's changed and owned paths. Nothing
else moves: the plan's `baseCommit` stays the job's approved base (the hub
binds it), Go checks keep `VERIFICATION_BASE_COMMIT` at that base, every
approved check is still kept, and the team's own verification before release
is unchanged.

- **Which tip is honoured.** Only a commit that all of these hold for: the
  checkout can read it; the approved base is its ancestor; it is an ancestor
  of the integrated commit; and the published branch in that checkout,
  `refs/heads/tasks-hub`, already holds it. The last condition is the safety
  rule: the runner integrates onto that branch's tip and moves the branch only
  after the integrated run is imported, so the tip it names is always
  published, while a candidate can never name itself or any other unpublished
  commit to drop the checks for its own changed paths. The latest tip any
  context or record can name is the published one, and the diff from there
  holds the whole candidate.
- **Fallback.** With any of those conditions unmet, the published branch
  unreadable, a job's approved plan missing its `changed` or `owned` paths
  (for a batch, any joined member's record included), or a selection path that
  is malformed or that the matrix has no rule for, the plan selects from the
  diff between the job's base and the integrated commit, as it did before this
  rule. A fallback is never a refusal.
- **Where the rule is recorded.** In the attempt directory
  (`JOB-integrated-verification/COMMIT-rN/`): `context.json` carries
  `selectionBaseCommit` (the tip) and `selectionPaths` when the runner named a
  tip; `plan.preserved.json` carries `selection` with `rule`
  (`integration-tip` or `job-base`), `baseCommit`, and either
  `selectionBaseCommit` and `selectionPaths` or the fallback `reason`
  (`invalid-selection-base`, `invalid-selection-paths`,
  `unreadable-selection-base`, `selection-base-off-line`,
  `unreadable-published-ref`, `selection-base-unpublished`). The plan command
  prints the same on stderr. `plan.json` itself gains no field, because the
  hub binds the receipt to the digest of the plan fields it knows.
- **The record's role in the run.** `verify-matrix.mjs run` re-derives the
  plan before any check starts and refuses a difference. It reads the
  selection from `plan.preserved.json` beside the plan file and applies the
  same tip conditions, the published branch included. With that record
  missing, unreadable, malformed or naming a tip that fails the conditions, it
  re-derives from the job's base, so a narrowed plan is refused as `Altered or
  omitted required checks`, never run on trust. An edited record therefore
  cannot narrow a run below the published tip's diff. Keep the record with its plan; do not
  edit or delete it in a live attempt directory.

The handler's import (`verification` release operation) binds the integrated
plan to the approved one and requires every approved check in it. Every check
must match exactly except `go-race`: `verify-matrix.mjs` derives go-race
packages from the paths the integrated plan selects from (above), which under
the fallback rule are every path changed since the approved base, including
the tasks-hub commits a cherry-pick lands on. An approved `go-race` is covered
by an
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

### Release batching (wi_50e80d45647b342a, wi_a72bba736a8c1daf, wi_8193aff1439e9b20)

With `releaseBatch.maxJobs` above 1 in the private config, the job the deployer
claims leads a batch: jobs queued behind it are integrated on top of it, share
one matrix run and are released together. The rule:

**A job joins a batch when its checks can share the lead's matrix run; the
batch's one run then keeps the union of every joined job's approved checks.**

A queued job can join only when all of this holds:

- it is `verified` and unclaimed, and queued after the lead;
- it has the lead's base commit, repository and pause generation, and is not
  marked to release alone after a failed batch (`batch-solo.json`);
- its plan names the lead's approved matrix (digest and approval message);
- every accepted check in its plan whose id is already in the batch (the
  lead's, or one an earlier member brought) is unchanged. Only `go-race` may
  differ, and only in its packages (the batch tests the union, or `./...`). A
  check whose id the batch lacks is added to it, so a Go-only lead carries a
  job that also needs browser checks.

The first three rules are unchanged by the union; only the last one compares
checks. The union is the floor of the batch's one run: before that run starts
the deployer proves the plan covers every joined job's approved checks, and
the hub proves the same for each job at import, so a member's checks are never
narrowed by riding with another job. A batch whose members bring other checks
runs longer than the lead would alone.

The deployer reads at most `2*(maxJobs-1)` candidates, in queue order, and takes
the first that fit until the batch is full.

Every candidate whose full record the deployer tried to read and left out, and
every candidate past the read limit, is recorded once in the lead's journal,
`journalDirectory/ID.json` on the deployer host, as `batchDropped: [{jobId, reason}]`,
whether or not a batch forms. Candidates are the queue summaries that are
verified, unclaimed, after the lead, of its base commit and pause generation,
and not marked batch-solo; a job passed over before that gets no entry, and
neither does one that fit but met a full batch, so an empty or missing list
does not tell these cases apart. Later polls of the same claim add nothing. When a
batch forms, its receipt `journalDirectory/bat_ID-receipt.json` carries the same list as
`dropped`. The hub's release record and `tt deployment get` do not show it.

| Reason | Meaning |
| --- | --- |
| `batch-plan-mismatch` | a check has the id of one already in the batch (the lead's or an earlier member's) and differs from it other than `go-race` packages |
| `batch-other-matrix` | another approved matrix digest or approval message |
| `batch-ineligible` | the full record is no longer verified, unclaimed, or of the lead's base, repository or pause generation |
| `detail-unreadable` | the job's full record could not be read |
| `over-limit` | queued past the `2*(maxJobs-1)` candidates that are read |

The same list also keeps the jobs dropped while the batch was built: a
cherry-pick failure by its reason, `batch-no-change` (the batch's head already
carries it, as above, a range the hub refuses because it adds no commit; the job
releases alone later as a no-change result), `batch-open-refused` and
`batch-add-refused`. A lead with no change opens no batch and releases alone.
To see why a job did not ride with the one before it:

```sh
jq .batchDropped journalDirectory/LEAD_ID.json
```

A released batch's receipt also records the union: `checks` (its check ids, in
the order the run was handed them) and `checksDigest` (the digest of those
checks, as handed to the matrix plan), and for each entry of `jobs` the job's
approved `checkIds` and, for a member, `contributed`: its check ids the lead's
plan lacks (`[]` when none).

```sh
jq '{checks, jobs: [.jobs[] | {jobId, contributed}]}' journalDirectory/bat_ID-receipt.json
```

When the batch's matrix run fails, the batch falls back as before: it is
dropped at the hub, every member is marked to release alone
(`batch-solo.json`), the lead goes on alone, and no `bat_` receipt is written.
When the run's receipt names its failed checks, `batchFallback` in the lead's
journal attributes them: `failed: [{checkId, jobs}]` lists, for each failed
check, the jobs whose approved checks include it, the lead first (`[]` for a
check only the integrated selection added); `unaffected` lists the jobs, the
lead included, that own none of them; and `unnamed` counts failed checks whose
id was not recorded (an id that is not 1 to 120 characters of
`[A-Za-z0-9_./-]`, or one past the first 50). With `unnamed` above 0, `unaffected` covers only the
named checks. Other fallbacks (`batch-plan-uncovered`, `batch-import-timeout`,
`batch-selection-refused`, a run that could not start) carry no attribution.
Neither does a receipt refused only for the matrix home cleanup error
(`VERIFICATION_HOME_CLEANUP_ERROR`, with no failed and no unnamed check):
`batchFallback` then has no `failed` or `unaffected` list and names the cause
as `cause: "matrix-home-cleanup-error"`, without the error's own text.

```sh
jq .batchFallback journalDirectory/LEAD_ID.json
```

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
and the terminal line remain. A failure that is a transport error is told once
per transport wait instead (A lasting transport wait, below).

**Transport errors are retried (wi_9cb499ac2a14a1a0).** A call of the
configured `tt` that fails because the hub was not reached is made again
before anything is refused. One slow call on a busy host must not refuse a
release that has passed verification (incident of 2026-10-13).

- **Bound.** At most 3 tries: the second 5 s after the first fails, the third
  20 s after the second fails. No retry starts later than 60 s after the first
  call began, and a retried call runs under at most 60 s. A call that hung for
  its whole command timeout (600 s by default) is therefore made once. The
  deployer waits synchronously, so nothing else in it runs during a wait.
- **Transport error (wi_f2e585b63f57b6f8).** Read from the last line of the
  call's stderr. It is one only for the hub line `hub: 502`, `hub: 503` or
  `hub: 504`, or, on a line with no hub status, one of these phrases of the Go
  HTTP client and the network: `context deadline exceeded`,
  `connection refused`, `connection reset`, `i/o timeout`,
  `Client.Timeout exceeded`, `broken pipe`, `network is unreachable`,
  `no route to host`, `host is down`, `connect: operation timed out` or
  `connect: connection timed out`, `no such host`, `TLS handshake timeout`,
  `server closed idle connection`, and `unexpected EOF` or `EOF` ending the
  line in the client's form `METHOD "URL": EOF` (wi_27a082e16c37d3cd). A bare
  `tt: unexpected EOF` or `tt: EOF` is not one: `tt` also reads a local
  `--file`, so it may be its own parse error. Each phrase is named in the
  reason by a fixed cause (for example `no route to host`,
  `connection closed: EOF`, `timed out: connect`). Any other
  answer of the hub is final and is never retried: a refusal or a validation
  answer (4xx with a reason) and a 500, whatever words it holds. A line that
  merely says `timeout` or `timed out` is not a transport error: the call is
  made once and fails as any other call does. One case has no line at all: a
  call the runner itself stopped at its command timeout. It stays a transport
  error (`no answer before the command timeout`), because the hub gave no
  answer and so decided nothing; treating it otherwise would refuse a release
  over a hung fence check. It is retried only inside the bound above.
- **Idempotent call.** Only these are retried: a `tt send` or a
  `tt deployment` call that carries `--request-id` (the hub answers a repeat of
  the id with the original), and the reads `tt deployment get`, `list` and
  `handler` and `tt agents`. Any other call of the CLI is made once. No other
  program is ever retried: not a build, a deploy, `wrangler` or a rollback
  program. Publication to `tasks-hub` is a git step and is not repeated.
- **A request sent at every poll waits.** The inputs request ("Import
  immutable inputs for this release job") and the integrated verification
  request ("Import verification for the integrated release commit") are sent
  again at every poll under one request id, each after a read of the job. When
  such a read or request still fails in transport after its tries, the job is
  not refused: it stays in `waiting_inputs` or `waiting_matrix` and the next
  poll, 30 s later, sends it again. The terminal prints "Release waits: a hub
  call failed in transport; the next poll retries." and the Board gets the "A
  deployer CLI call failed" notice above, once per job and reason for a
  process. A refusal of such a request still fails the job as before.
- **A fence check before publication waits (wi_f2e585b63f57b6f8).** Every
  poll of a claimed job starts with `tt deployment check`, and the check is
  repeated before integration, before each read of verification or inputs and
  before publication. When a check still fails in transport after its tries
  and nothing is published and no target effect is journaled, the job is not
  refused: the release returns `waiting_transport`, the job stays claimed, and
  the next poll starts again from its journal. A new job has no journal yet
  and gets none; a job already in `waiting_matrix` or `waiting_inputs` keeps
  its phase; a check that failed just before publication leaves
  `waiting_inputs`, with the checkout on the integrated commit. The terminal
  prints the same "Release waits: ..." line. A check the hub answers is
  unchanged: a lost fence or a generation that is no longer this claim's still
  refuses the job. When the hub answers `generation changed` and the re-read
  of the job then fails in transport after its tries, the job waits the same
  way; a re-read that fails for any other reason is a lost fence. Once `tasks-hub` is published or an effect is journaled, a
  check that fails in transport is a failure as before: the rollback path
  runs, with one fence attempt per effect, and nothing is published twice.
- **A lasting transport wait (wi_f2e585b63f57b6f8).** A transport wait is the
  run of polls in which some call of the CLI spent its tries on a transport
  error. It is kept in `transport-wait.json` in the journal directory, mode
  0600 (`jobId`, `since`, `lastAt`, `polls`, the first and the latest cause in
  the fixed words above, `routine`, `due`, `escalated`), so it belongs to the
  job and a restarted or re-executed deployer continues it. A poll that fails
  before any job is read continues the open wait, or opens one with no job. A
  failure of another job begins a new wait. The wait ends, and the file is
  removed, at the first poll in which no call ends in a transport error.
  - A claim that spends its tries on a transport error is part of the wait
    and nothing else: it is not a held claim, so no "skipped" notice is sent
    and no "reconciliation required" line is printed. The poll ends at that
    job without claiming a later one, and the next poll claims it again. If
    the hub did record the claim and only its answer was lost, the next
    poll's list shows the job claimed by this run and the runner goes on
    with it as its own, with no second claim.
  - One "A deployer CLI call failed" notice (`wake=routine`) per wait, for its
    first failure. Its request id is built from when the wait began.
  - **Bound: 10 minutes.** Once the wait has lasted that long, one "The
    deployer has not reached the hub for longer than its bound" notice
    (`wake=attention`, request id `transport-wait-HASH` of the start time and
    the job) names the job, the start time and the cause at that poll. The
    terminal prints "Transport wait passed its bound; attention notice
    posted."
  - Both notices go through the hub that is failing. Each is tried once per
    poll until the hub takes it (posts it, or refuses it with an answer of
    its own). At the poll in which the hub answers again, each notice still
    owed is tried, each on its own whatever the other did, and the wait ends
    only once neither is still unreached; until then the file stays and each
    outcome is kept in it. So a wait is always reported once by its routine
    notice, and a wait that outlasted the bound once by its attention notice.
  - **Notices skip retries during a wait.** Every notice of the daemon (fence
    wait, host wait, code, held claim, failed call) is sent once instead of up
    to three times in three cases: a wait is open from an earlier poll; a
    read of this poll's own (the job list, a job detail, an earlier notice)
    spent its tries; or the release this poll ran recorded such a failure.
    Each failed notice would otherwise add up to 25 s to the poll. A fence
    wait, host wait, code or held claim notice that fails is sent again by
    the next poll under the same request id. A failed call notice is not: it
    is sent again only when that failure recurs. A claim that fails in
    transport does not by itself turn the retries off: in the first poll of
    a wait its held claim notices are still tried up to three times each.
    Notices a release posts itself (a refusal, a failure, a request to the
    handler) keep their retries.
  - **What to do.** Check that `tt status` on the deployer's host reaches the
    hub. Nothing needs to be requeued: an unpublished job continues at the
    first poll that reaches it.
- **Any other call.** Once its tries are spent the job fails as it did
  before the retry existed: refused when nothing was published, rolled back
  otherwise. A rollback is delayed by at most the bound above per call. The
  reason in the job journal (`refusalReason`, `failureDetail`) and in the
  "Release refused before publication" notice names the cause and the number
  of tries, for example `tt send timed out: context deadline exceeded, 3
  tries` or `tt deployment merged hub answered 503, 3 tries`, in place of
  `tt send exit 1`. A fence check that fails this way after publication is
  named so, not as `release fence lost`. A transport error of a call that is not retried reads
  `..., 1 try`. Only these fixed words reach the journal and the Board, never
  the stderr line.
- **Private record.** Every failed try is recorded in `cli-failures.json`,
  including one a later try made good: a call that timed out once and then
  succeeded leaves one record with `count` 1 and no notice, and three failed
  tries of one call leave one record with `count` 3.

### Which notices wake (wi_991125f66a787459)

Every notice the deployer posts with no recipient carries one ref, `wake`,
with the value `routine` or `attention`. The Board shows it in `Refs:`. A
notice with no recipient is in every agent's inbox. The inbox relay wakes two
agents for one from the deployer: the project's primary database handler and
the owner helper, each once per notice. It wakes neither for `wake=routine`;
that notice stays unread until they next read their inbox.

| Notice | Ref |
|---|---|
| A release job is waiting behind a held project fence, when the holder waits for the handler to import integrated verification or to bind inputs | `wake=routine` |
| A release job is waiting for the verification host | `wake=routine` |
| Deployer code is out of date; it restarts itself after the current release | `wake=routine` |
| Deployer is restarting itself onto the published scripts, with or without a held job | `wake=routine` |
| Deployer now runs the published scripts | `wake=routine` |
| A deployer CLI call failed | `wake=routine` |
| The deployer has not reached the hub for longer than its bound | `wake=attention` |
| A release job is waiting behind a held project fence, when the holder is blocked, merged or claimed by another deployer run | `wake=attention` |
| A release job is held until its matrix run is confirmed stopped | `wake=attention` |
| Release refused before publication | `wake=attention` |
| Release is live but the tasks-hub push failed | `wake=attention` |
| Release failed and requires recovery | `wake=attention` |
| Deployer is not claiming releases: ..., for every reason | `wake=attention` |
| A release job is skipped because ..., the Board copy sent when a recipient was not reached | `wake=attention` |

- A routine notice repeats what the handler already has or needs no action. For
  the two routine fence waits the handler holds the deployer's directed request
  for that step (Handler requests and gating, below).
- Only the exact value `routine` is quiet. A notice with no `wake` ref, as a
  runner older than this change posts it, or with any other value wakes as an
  `attention` notice does.
- The ref is read only on a typed notice a deployment agent posted with no
  recipient. It does not stop or start the wake of a directed message, a
  broadcast, an owner post or another agent's notice. The directed copies of
  the held claim notice carry no `wake` ref; a directed message wakes its
  recipient.
- A restart is routine whether or not "Deployer now runs the published scripts"
  follows. A deployer that does not come back after its restart notice is
  reported by the hub instead (A silent deployer, below). A restart the
  deployer cannot make is "Deployer is not claiming releases: ..." and wakes.
- Request ids do not include the class. A notice an older runner posted
  untagged and a newer runner resends under the same request id is refused by
  the hub as a reused id with other data: the original stays on the Board, the
  deployer retries once per poll while the condition lasts (for a fence wait,
  a host wait or a code notice it prints "... not posted; the next poll
  retries." each time), and `cli-failures.json` keeps one counted record. This can only happen for a wait or hold that is still active across
  the restart onto the tagging runner.

### A silent deployer (wi_d8ff05d1989ff279)

The hub itself notices a deployer that has died or that restarted and did not
come back while a release job waits. The check is part of the hub's 30 second
scheduling tick. It reads only saved rows: the deployer's heartbeat, its
messages and the release jobs. It reads no inbox and no unread position, and it
runs for every open project that is not paused.

**Rule.** With N the bound (10 minutes unless set, below), the hub posts when
both hold:

- **A job waits.** At least one release job has stood in `verified`, in
  `claimed` or in `merged` for N without changing state. `merged` is the job's
  prepare, deploy and live check, so a deployer that dies or hangs during the
  deploy of a lone release is reported too (wi_27a082e16c37d3cd). A job that
  changes state (`verified` to `claimed`, `claimed` to `merged`) starts its N
  again. `held` and `blocked` jobs never count: a held job is the owner's and a
  blocked one has been handed to a person with its own attention notice, so
  neither waits on the deployer, and an owner hold alone is never reported. A
  `verified` job queued behind a blocked one still counts. The hub times a job from the first
  tick that sees it in its state, so the notice is never early and at most one
  tick late. The timing does not carry across a gap in the checks: when a
  project has gone unchecked for two minutes or more (for N or more when N is
  under two minutes) because the hub was down, the project was paused or it
  had no deployer, every standing job starts its N afresh at the next tick. No
  heartbeat is recorded in such a gap either, so the first tick after it never
  posts. A shorter gap keeps the timing.
- **The deployer is not alive.** Either of:
  - *Silent.* Nothing was heard from it for N: no heartbeat from its `tt wrap`
    wrapper and no message from its current run. This covers a dead wrapper, a
    dead host and an exited deployer.
  - *Restart not completed.* Its current run posted "Deployer is restarting
    itself onto the published scripts" (with or without a held job) N or more
    ago and has posted neither "Deployer now runs the published scripts" nor
    "Deployer is not claiming releases: ..." since. The wrapper keeps its
    heartbeat through such a restart, so the heartbeat does not hide it.
  - *Hung.* It is heard from, but its release runner has reported no progress
    for the progress bound (A hung runner, below).

The deployer is the project's newest deployment agent that is neither retired
nor closed. Retiring or closing it is deliberate and is not reported. A deployer
on a long verification run keeps its heartbeat, and its runner keeps reporting
at every poll, so it is not reported, however long its claimed job stands.

**Notice.** One directed notice per recipient: the primary database handler,
unless it is retired, and the owner helper. A directed notice wakes its
recipient once. The subject is "Deployer has gone silent while release jobs
wait", "Deployer announced a restart and did not come back" or "Deployer
runner has made no progress while release jobs wait", with refs
`activity=deployer_liveness`, `reason=silent`, `reason=restart` or
`reason=hung`, `deployer=AGENT_ID` and `run=RUN_ID`. The text names the deployer, its agent,
run and status, its last heartbeat, its last message (number, time and
subject), how many jobs are `verified`, `claimed` and `merged` ("Waiting: 1
verified, 0 claimed and 0 merged release jobs"), the oldest of them, the bound
and the recovery.

**Once per episode.** An episode opens with the first notice and each
recipient gets one copy in it, however long the silence lasts. A recipient
that was absent when it opened (no handler, a retired handler, no open owner
helper) gets its copy at the first tick that finds it present while the
episode is open and a job still waits; the other recipient is not sent a
second one. A handler rotation inside an episode does not send a second
handler copy. The episode ends, and the check re-arms, at the first tick that
finds the deployer alive: a fresh heartbeat or message for a silent deployer,
"Deployer now runs the published scripts" for a restart, a new progress report
for a hung runner. A new deployer run also ends it.

**Recovery.** The hub detects and wakes only. It never restarts, signals,
replaces or messages the deployer, and it changes no release job. Whoever is
woken checks the deployer's host and terminal, then runs `tt deployment setup`
for the deployer with its current run as predecessor (the start of this
document; the run is in the notice's `run` ref).

**Setting.** `TAILTERM_DEPLOYER_LIVENESS_MINUTES` in the hub's environment
sets N, a whole number of minutes from 1 to 1440. Any other value, or none,
gives 10. One N is used for the silence, the restart and the job.

**Hub outage and clock.** After the hub was down, or its clock jumped forward,
for two minutes or more, a deployer is reported only if it is still not heard
from once its jobs have stood for N again; a deployer that heartbeats again in
that time is not reported. A gap shorter than that keeps the job timing, and
is too short by itself to make a deployer silent.

**Claim freshness.** Separately, the hub refuses a claim, and the deployer's
other fenced calls, when the deployer's heartbeat is more than 90 seconds old
("deployment heartbeat stale"). Both rules read the hub's own clock.

#### A hung runner

A release runner can stop while its `tt wrap` wrapper goes on sending the
heartbeat, with no restart announced. The heartbeat alone cannot show that, so
the runner reports its own progress and the check above reads it
(wi_95b575f0454c7052).

**Signal.** The runner (`scripts/release-runner.mjs`, when started by
`tt deployment serve`, for `--once` too) calls `tt deployment progress` once
when it starts, at each new phase and step of a release, and at the end of
every poll, a held poll included. The calls are made between steps and never
from a timer, so a runner that stops also stops reporting. The hub keeps the
last report of the deployer's current run, with its own clock as the time, in
`release_deployer_progress` (one row per project). The wrapper heartbeat
never writes it, and a report is not a release action: it writes no receipt,
changes no job and wakes nobody.

**Bound.** A deployer that is otherwise alive is *hung* when the last report of
its current run is 10 minutes old or older. `TAILTERM_DEPLOYER_PROGRESS_MINUTES`
in the hub's environment sets that bound, a whole number of minutes from 1 to
1440; any other value, or none, gives 10. Everything else is the check above:
a job must have stood in `verified`, `claimed` or `merged` for N, there is one notice
per recipient and episode, silence and an unfinished restart come first so one
cause opens one episode, and nothing is restarted, signalled or changed. The
notice adds the time, phase, step, job and wait of the last report.

**A wait is information only.** Each poll's report names the wait the poll
ended in: `matrix` (with the pid of the run when the runner knows it),
`inputs`, `host_lock`, `fence`, `owner_hold` or `transport`. A job the
runner check of the release quiet window holds (`waiting_window`) is reported
as `owner_hold`, since the hub knows no window wait. The hub decides
nothing by it. A matrix run is a detached process and the runner goes on
polling every 30 seconds beside it, reporting each time, so a long matrix run
is never reported. A runner that stops reporting during a matrix run is
reported at the bound like any other, and the notice names the matrix wait and
its pid.

**A runner that sends no reports.** With no report of the deployer's current
run on record, only the silent and restart rules apply: a deployer from before
this change is never reported as hung, and its hang is not detected. The same
holds for a row left by an earlier run.

**Rollback inside one deployer run.** A re-exec keeps the run, so a run can
report and later lose the ability. Neither case is read as a hang, and neither
needs a step by hand:

- *The runner is rolled back.* A report older than the run's newest
  "Deployer is restarting itself onto the published scripts", "Deployer now
  runs the published scripts" or "Deployer is not claiming releases: ..."
  notice does not count: the scripts that sent it may no longer run. A runner
  that reports again after such a notice counts again within one poll. Between
  such a notice and the run's next report (its next phase or step report, or
  the report at the end of the poll) the hung rule does not see a hang; only
  the silent rule applies then, and the restart rule too after a restart
  notice.
- *`tt` on the deployer's host is rolled back under a runner that still
  reports.* The older `tt` rejects the report; the runner then sends one
  withdraw (`tt deployment progress --request-id KEY`, a form the older `tt`
  forwards), which clears the report and keeps the run judged as one that
  never reported. A withdraw that fails (in transport, or refused) is sent
  again after the next rejected report, until one succeeds; after a success it
  is not repeated until a report has succeeded again. Reports are still tried
  at every point and resume by themselves.

A hub rolled back to before this change leaves the table unused and refuses
the calls; the runner ignores a failed report: it is tried once for at most ten
seconds and is never recorded, retried or posted.

**Limit.** A single step that runs longer than the bound without the runner
getting back to a poll (one command may run ten minutes, the prerequisite
provisioning fifteen) gives one notice for a healthy runner. Since a `merged`
job counts, this holds for the prepare, rehearse, deploy and live-check steps
of every release, a lone one included. Raise the bound
if that recurs. A hang of the hub's own tick is not covered.

**Command.** `tt deployment progress` prints the stored record: `agentId`,
`runId`, `reportedAt`, `jobId`, `phase`, `step` and `wait`, all empty with
nothing on record. A read needs no deployer identity, so the handler and the
owner helper can read it. The deployer's run alone may write: `--phase P [--job J] [--step S] [--wait KIND] [--pid N]`
reports and `--withdraw` withdraws. The hub refuses a write from any other
agent or run, or with a heartbeat older than 90 seconds.

**The manual check.** Until this check has passed its live check after
release, the owner helper's sweep keeps flagging a release claimed for more
than 50 minutes. Once the live check passes (a read shows the deployer's run
with a `reportedAt` under a minute old, a later read shows a later time, and a
normal release posts no hung notice), that manual 50-minute check can retire;
the owner helper decides when.

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

### A generation that advances under a claim (wi_395d07d432a93bc7)

Every native call names the job generation the runner last saw. Another
writer can advance it while the runner still holds the claim: the handler's
import of integrated verification or of inputs does. The hub then answers the
runner's next call with 409 `release: generation changed`. That alone is not a
lost fence.

**Adopted.** When the fence check (`tt deployment check`) gets that answer, the
runner reads the job once more (`tt deployment get`, with the exact-detail
checks). It adopts the new generation and sends the check once more when all of
these hold:

- the job is still `claimed` by this exact agent and run;
- its pause generation, immutable pins, integrated commit and input pins are
  unchanged, and its generation is later than the one the runner held;
- nothing is published and the journal has no effect.

The release then goes on as if the check had passed. A refuse call
(`tt deployment refuse`) that gets the same answer is treated the same way: one
re-read, then one more refuse at the new generation.

**A lost fence.** Anything else is a lost fence, as before: a claim that now
names another agent or run, a job that is no longer claimed, a changed pin, a
failed re-read, any other check error, a second `generation changed` after the
one re-read, and any generation change once the release is published or an
effect is journaled. The runner never re-reads twice for one call, and it never
continues or refuses for another agent or run. The one re-read after
publication is the final block of a job with no receipt, which only stops the
job (see "Revert on the block").

**A refusal left unfinished.** A refuse call that still fails leaves the journal
in phase `refusing` with `refuseFailure` beside `refusalReason`, nothing
published and no effects. The next poll of the same claim resumes it: the job is
read at the hub's generation, the refuse is sent again, and the job ends
`refused` with the original reason. A `refusing` journal of another run, with an
effect, with `published` set or with no `refuseFailure` is not resumed; it still
needs handler reconciliation.

What an operator sees:

| Case                                             | Terminal                                                  | Journal                                                                                                       | Board                                                                                                    |
| ------------------------------------------------ | --------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------- |
| Adopted, release continues                       | nothing                                                   | `generationAdoptions` counts the adoptions; the phases go on as usual                                         | nothing; the rejected call is kept in `cli-failures.json` and is named only if the release is later held |
| Lost fence at the poll's first check             | `Release held; inspect handler fence and private journal` | unchanged                                                                                                     | "A deployer CLI call failed"                                                                             |
| Lost fence later, nothing published              | the same line                                             | `refusalReason: release fence lost`; `refused`, or `refusing` with `refuseFailure` when the refuse failed too | "Release refused before publication" once refused, and "A deployer CLI call failed"                      |
| Refuse retried in the same run                   | the same line, for the refusal itself                     | `refused` and `generationAdoptions`                                                                           | "Release refused before publication" with the original reason                                            |
| Refusal resumed by a later poll                  | the same line at each poll until it completes             | `refused`; `refuseRecovered` holds the earlier `refuseFailure`                                                | "Release refused before publication" with the original reason                                            |
| Generation change after publication or an effect | the same line                                             | the existing revert, rollback and `blocked` records                                                           | the existing rollback notices                                                                            |

A refused job is terminal; the handler retries its entry (Retrying a refused
release, above).

### Setting aside a job that holds the fence

A claimed job that cannot progress (for example its integrated import was
refused and the fix is the next queued job) would hold the project fence
forever, because the runner resumes its own claim first. If the deployer is to
stop anyway, a clean stop of its runner frees such a claim by itself and keeps
the job's queue place ("Pause", below); set-aside is for a runner that keeps
running. `tt deployment
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
journal is held and the job skipped for handler reconciliation. The same check
archives the journal of an unclaimed job as `ID.json.unclaimed-gN` when the
stopping runner could not rename it itself.

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
a restart's resend returns the original. The notice is `wake=routine` while the
holder waits for the handler's import or inputs, and `wake=attention` for a
blocked, merged or otherwise claimed holder (Which notices wake, above).

### Holding a verified job (owner hold)

The deployer is an automated runner (`scripts/release-runner.mjs`). It reads the
release ledger and nothing else: a notice, request or block addressed to it on
the board is not read, so a message cannot keep a job from releasing. An owner
decision that a verified job must wait is saved on the job itself, where the
hub enforces it. Both commands are handler-only:

```
tt deployment hold   --job ID --generation N --reason TEXT [--until-items wi_a,wi_b] --request-id KEY
tt deployment unhold --job ID --generation N --reason TEXT --request-id KEY
```

`hold` moves a `verified` job that no deployer has claimed to state `held` and
saves its `hold`: the reason, the release condition (`untilItems`), the handler
agent and run, and the time. While the job is `held`:

- A claim of it is refused (`job ID is on an owner hold (REASON)`), and so is
  adding it to a batch. The runner picks only `verified` jobs, so it passes over
  the held job and claims the next verified one; the release line does not stop.
- It keeps its place in the queue. It does not count as a waiting job: the
  deployer posts no fence-wait notice for it, and it does not make a claimed job
  ahead of it eligible for set-aside.
- `reconcile` and `supersede` are refused until the hold is released.
- `tt deployment list` shows it in the active view with state `held` and its
  `hold`; `tt deployment get` also shows `holdHistory`.

`unhold` returns the job to `verified` in the same queue position, so it is
claimed in its original order. The hub also lifts a hold by itself when it
names items: once every item in `untilItems` has a release job that is
`released` or `superseded`, the request that settled the last one returns the
held job to `verified`. A hold with no items ends only with `unhold`.

Every transition is appended to the job's `holdHistory` and never rewritten: a
`hold` event, then a `release` event whose `cause` is `handler` (an unhold, with
the handler's agent, run and reason) or `items_released` (with `triggerJobId`,
the job whose release met the condition). Each carries the job generation after
the event and the hub time.

Rules the hub checks:

- The reason is one line of at most 512 characters. `--until-items` names at
  most 16 existing work items of the project, none twice and not the job's own
  item. A condition that is already met is refused.
- Only a `verified`, unclaimed job can be held. A claimed job with no effects is
  set aside first and then held; the refusal says so. A job whose latest
  reconcile record carries a host lock digest (a requeue) cannot be held: the
  runner frees that lock only while the job is `verified`, and the hub cannot
  see the host.
- Both commands need the job's exact generation and replay by request ID.

After the hub saves either command, the CLI posts one board notice linked to
the job's item ("A release job is on hold" or "A release job hold was
released"). If that post fails the hold or release still stands; the command
prints the notice text to post by hand. A hold the hub lifts by itself writes
the history and the list but posts no notice. A hold saved between the runner's
list read and its claim makes that one claim fail: the runner posts its usual
"the hub refused its claim" notice once and goes on to the next job. That is
for a claim the hub refuses; a claim that fails only in transport is not a
held claim (A lasting transport wait, above).

Which of the four to use:

| | Applies to | Effect | Ends when |
|---|---|---|---|
| Owner hold (`hold`) | a `verified`, unclaimed job | state `held`; never claimed or batched; queue position kept | `unhold`, or its named items release |
| Release quiet window (`tt deployment window set`, owner) | every covered `verified`, unclaimed job while the window is open, including one the ledger check covers | the hub saves a window hold (state `held`); claim and batch-add of a covered job are refused | `unhold` after the window ends, or after the owner clears it |
| Set-aside (`set-aside`) | a `claimed` job with no effects while a later verified job waits | back to `verified` behind every queued job; claimed again when its turn comes | it is not a hold: nothing stops the next claim |
| Refuse (`refuse`, or `reconcile` with `refuse`) | a claimed job that must not release as it is | terminal state `refused` | never; a new job needs `tt deployment retry` |

"Held" is also used for three other things, none of which is an owner hold: a
release batch in state `held` (published or blocked, outcome not restored), the
runner's notice for a job it skips because the hub refused its claim or its
set-aside journal is held, and reconcile's "job is not held" for a job that is
already terminal.

### Release quiet window (wi_03dc6f7d365d6528)

The release quiet window is an owner-set daily span of local time, such as
22:00-07:00, in which the deployer releases no covered job. The hub saves it
and enforces it itself, so it does not depend on a handler's turn coming
before the runner's next poll, and a board message is not a window.

Two checks enforce it, in this order. The hub holds first: it holds and
refuses to claim the covered jobs it can see (W1-W3 below, with the ledger
check). The runner checks second, after its target selection, against the
targets the integrated commit actually deploys (Runner check below), so a
change the hub cannot see still waits.

```
tt deployment window
env -u TAILTERM_AGENT -u TAILTERM_RUN tt deployment window set --zone America/Los_Angeles \
  --start 22:00 --end 07:00 --targets hub,bridge,mini --reason TEXT --request-id KEY
env -u TAILTERM_AGENT -u TAILTERM_RUN tt deployment window clear --reason TEXT --request-id KEY
```

With no operation the command prints the window, or `null`. `set` replaces any
window already set. `set` and `clear` belong to the owner: like a hand release,
they are refused from a shell that carries an agent identity, so run them with
the identity removed as shown. They replay by request ID, and a changed payload
under the same ID is refused. Both print the window read back. The flags are
checked before any hub call.

What the hub checks:

- `--zone` is an IANA name such as `America/Los_Angeles`, never empty and never
  `Local`.
- `--start` and `--end` are 24-hour `HH:MM` from 00:00 to 23:59 and differ. A
  start later than the end crosses midnight.
- `--targets` is exactly one of `hub,bridge,mini`, `hub,bridge,mini,tailos` or
  `tailos`, each named once. The hub cannot tell hub, bridge and mini apart
  before the runner's checkout, so they are covered together or not at all.
  The deployer's own code counts as a service path, so it is covered with them.
- `--reason` is one line of at most 512 characters.

**Time.** Membership uses wall-clock time in the window's zone. The start
minute is inside the window and the end minute is outside it. On the fall-back
night 22:00-07:00 lasts ten real hours, and both repeated 01:00 hours are
inside it. On the spring-forward night it lasts eight. A bound in a skipped
hour still divides the clock: a window that starts at 02:30 is open at 03:00 on
that night. `until` is the next end time after now, on the next calendar day
when today's end has passed, RFC 3339 with its offset (for example
`2026-11-01T07:00:00-08:00`).

**Coverage.** The hub has no target list for a job before the runner's claim.
It sorts the changed paths in the job's verification plan (`plan.changed`) into
three classes. The lists are a subset of `scripts/release-targets.mjs` and may
only stay a subset, so a missing rule holds a job and never releases one:

| Class | Paths | Covered by |
|---|---|---|
| none | `docs/`, `tests/`, `verification/`, any `*_test.go`, any `AGENTS.md`, root `LICENSE`, root `README.md`, `hub/README.md` | no window |
| tailos | `client/`, `wasm/`, `public/`, `shared/`, root `package.json`, `package-lock.json`, `index.html` and `vite*.js` | a window that names `tailos` |
| service | every other path, including unknown and invalid ones | every window |

A job is covered when any of its paths is covered, or when it has no changed
paths at all (unknown counts as covered). A window of `tailos` alone still
holds a job that changes hub code, because service paths are held by every
window.

**Ledger check (wi_d87f594a6ae13ec9).** A job whose own paths are all uncovered
(docs or tests only, or TailOS only under a window without `tailos`) can still
deploy covered targets: the runner selects each target from its last released
commit through the integrated tasks-hub commit, so whatever tasks-hub holds
unreleased ships with it. The hub has no git, so such a job is covered too
unless the hub's own records show that every covered target is current at the
job's `baseCommit` and nothing the hub knows of on tasks-hub is unreleased.

A *sync commit* is a commit at which every covered target is current:

- S1: the commit of a `released` receipt that carries `push`. The runner
  selected every target from its own baseline through that commit. The
  no-change finish has no `push` and never selects, so it is not one.
- S2: the `releasedCommit` of a hand release whose targets include every target
  the window covers.

The first of these that applies covers the job:

- U1: a job of the project is `published`, not `released` or `superseded`,
  neither its receipt nor the job (a block's revert, wi_816228d539a80c48)
  records a committed revert (whatever its outcome), its own paths or
  its integrated plan's paths are covered, and no job of its queue entry is
  `released` or `superseded`. The integrated plan counts because a batch
  publishes its members' changes with its lead, while the members stay
  unpublished until they release. Example: a job blocked after publishing
  that waits for a hand release, or a docs-only lead blocked with a hub member
  in its batch. The recorded revert is read first, before any order proof. A
  job that is `refused` or `rolled_back` with no committed revert stops
  counting once an S1 job settled after it left the project fence
  (wi_fc763af18f58129f): jobs pass the fence one at a time, so that S1 job was
  claimed after it, merged onto a tasks-hub that already carried the
  publication and any revert, and its push selected every target past them.
  The hub proves "after" by parsed times, never row numbers (set-aside moves
  job rows and VACUUM may renumber any): the newest S1 `settledAt` must be
  later than the job's own `settledAt` (its final receipt's hub time, or the
  hub time of the reconcile that refused it, wi_816228d539a80c48). A refusal
  saved before refused jobs carried `settledAt` uses its last refuse
  reconcile's `observedAt`, the handler's clock; an S1 job claimed after a
  refusal settles minutes later, so a skew of seconds cannot change the
  answer. An equal or unknown time keeps the job a cause. An S1 job released
  before the refusal does not count, and a `blocked`, `merged` or `claimed`
  job still holds the fence, so no later claim exists.
- U2: a hand release that omits a covered target was recorded after the newest
  sync record (the receipt's `settledAt` or the hand release's `createdAt`,
  compared as times).
- U3: the job's `baseCommit` is empty or is not a sync commit. Example: an owner
  commit pushed to tasks-hub before its hand release is recorded.

So a docs- or tests-only job based on a pushed release, or on a full hand
release, still releases at night. The hub reads these records only while the
window is open and only for a job whose own paths are uncovered. Its limits:

- The hub does not know the runner's config baselines. A hand release recorded
  only by a config edit is unknown to it, so a job based on that commit is held,
  never passed.
- A commit pushed to tasks-hub after the job's base that no job or hand release
  records is not visible to the hub, so the hub lets the job be claimed. The
  runner check (below) catches it: the job's selection then includes that
  commit's targets, and it waits.
- A published job whose entry never settles holds every such job each night
  until the handler settles it (a supersession citing the hand release, or a
  released retry of the same entry) or, once it is refused or rolled back,
  until a later S1 release. That includes a published job whose block never
  reached the hub, or reached it bare, so that it records no revert.
- Equality with recorded commits holds too many jobs, never too few: a job on a
  revert commit, a no-change finish, or a legacy receipt without `push` is held
  and waits for the morning unhold.

**What the window does while it is open:**

- **W1, on write.** A handler operation that leaves its job `verified` and
  unclaimed (`enqueue`, `retry`, `unhold` of a hold that is not the window's,
  a requeue `reconcile` and `set-aside`) saves a covered job as `held`
  instead, in the same request.
- **W2, on list.** `tt deployment list` (the runner's read before every claim)
  first holds every covered `verified` job that no deployer holds and that is
  not in an active batch. That covers a job verified before the window opened,
  and jobs that an unclaim, a dissolved batch or a lifted hold return to
  `verified` at night. `tt deployment get` does not write.
- **W3, at claim.** A claim of a covered `verified` job is refused with
  `job ID is inside the release quiet window START-END ZONE covers TARGETS
  until UNTIL ...`, and so is adding one to a batch. This matters only between
  a list and a claim at the moment the window opens: the runner posts its one
  "the hub refused its claim" notice (no reconcile is needed), and its next list
  holds the job.

A window hold is an owner hold that names the window: `hold.window` carries the
zone, start, end, targets and `until`, its reason reads `release quiet window
22:00-07:00 America/Los_Angeles covers hub, bridge, mini; held until an explicit
unhold after UNTIL`, and its `holdHistory` event has `cause` `window`. When only
the ledger check covers the job, the reason and the W3 refusal end with `; `
and the cause: `job ID was published and is not released` (U1), `hand release
ID names only TARGETS` (U2) or `base SHA12 is not a recorded release commit`
(U3). It has no
agent, run or named items. The runner treats it like any hold: it passes the
job over and its poll ends with the wait `owner_hold`, not a failure.

**Runner check (wi_3427bf81094d2225).** On every pass of a claimed job, after
it selects the job's targets (from each target's last released commit through
the integrated commit) and after its integrated matrix is imported, and before
the inputs request binds anything or anything is published, the runner reads
the window with `tt deployment window`. A target is covered when the window is
open and names it: exactly the selected targets in the window's `targets`,
with no service-path rule, so a Mini-only selection under a `tailos` window
goes on. While any selected target is covered, the job does not go on: its
journal phase is `waiting_window` with the covered targets and `until`, the
pass ends, and the next poll checks again. The job keeps its claim and its
integrated run; nothing is bound, refused, published or deployed. The runner
posts one notice per job and window end, "A release job waits for the release
quiet window", naming the covered targets and `until`, and its progress
report names the wait `owner_hold`. No unhold is needed: the first pass after
the window ends goes on. A job whose selection is empty (docs or tests only, on
current baselines) never reads the window and still releases at night. A
window that cannot be read (a failed call, such as from a `tt` without the
command, or output of another shape) counts every selected target as covered:
the job waits as above with the read failure named instead of `until`, and the
notice needs attention. A pass that read the window closed goes straight on to
its inputs and publication, seconds later; the window is not read again once
publication began.

**At the end.** Nothing is released by itself. Window holds stay until the
handler runs `tt deployment unhold` for each job, which returns it to
`verified` in its original queue order. While a window is open, `unhold` of a
window hold is refused (`job ID is on a release quiet window hold until UNTIL;
unhold it after the window ends, or the owner clears the window first`), and
`unhold` of any other hold releases it and holds it again by the window in the
same request. Both events stay in the history.

**Releasing at night.** The owner clears the window (`tt deployment window
clear`), then the handler unholds the jobs. Clearing releases no hold already
saved.

**Exceptions.**

- A job claimed before the window opened keeps its claim, its batch and its
  integrated matrix run, but while the window is open and covers a selected
  target it waits before its inputs are bound (Runner check above), at every
  pass, until the window ends. Effects that already began (publication,
  deploys, live checks, the push and the finish) finish.
- A job whose latest reconcile record carries a host lock digest (a requeue)
  is never held, because the runner frees that lock only while the job is
  `verified`. Its claim is still refused while the window is open, and it
  becomes claimable at the end without an unhold.

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
(for example "TailOS live check last saw COMMIT after 90 s."). A rolled_back or
blocked hub receipt carries them too, per target, with a null left out (see
"Failure diagnostics in the receipt" below).
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
  for hub and bridge, plus `capture` when the probe ended not ready. A failed
  receipt gets only `waitedMs` from them; `polls` and `capture` stay in the
  private journal. If the
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
is `prepare`, `rehearse`, `deploy` or `live-check`. A failed target selection
is kept as step `select`, with no target (see "Effects on tasks-hub"). A failed
rehearsal container
run adds `rehearsal` (see "Migration rehearsal container"). The reason is at most 160
characters and is only a fixed message the runner or adapter tagged (for example
`live verification failed` or `release fence lost`) or, for a host program, its
name, exit status and the `classification`/`stage` of its last JSON line (for
example `deploy-truenas-hub.py exit 2: remote-operation-failed at
bridge-binary-upload`); anything else is `unclassified`. Program stderr,
messages, paths and credentials never reach it, except that the
`Unknown release path` reason names one changed repository path made only of
letters, digits and `_ . / -`. `scripts/deploy-truenas-hub.py
--rollback-to RELEASE --target hub|bridge --expect-sha256 SHA` verifies the
retained binary at `BASE/releases/RELEASE/` over SSH, then changes only that
target's executable mount in the live app definition (`app.update`, `app.start`).
It makes no backup and touches no token, database or Tailscale state, and refuses
a missing or mismatched binary or an unrecognized live definition before any
mutation. After a verified TailOS deploy the runner keeps `dist-static` as
`journalDirectory/tailos-dist-COMMIT` (0700); the next job's TailOS rollback
redeploys that directory with Wrangler. A hand release of TailOS
retains the same directory through `tt deployment hand-release --dist` (see
"Superseding jobs released by hand"). Before publication, when TailOS is
selected, the runner runs `npm ci` if `package-lock.json` changed since the
TailOS baseline and refuses the job unpublished (`Pinned wrangler not
installed; run npm ci`) if the wrangler pinned in `package.json` is not
installed in the checkout; `prepare(tailos)` runs `npm ci` first only when that
check did not already run it for the commit. The deploy and the recorded
rollback run that pinned `node_modules/.bin/wrangler`, never npx
(docs/host-requirements.md).

**Failure diagnostics in the receipt** (wi_d4e96831ab3a0038). A rolled_back or
blocked receipt that `tt deployment finish` saves carries, beside its outcomes:

- `failure: {step, target, reason}`: the journal's first failed step. `reason`
  is the runner's stable failure tag, the same text the journal keeps, so a
  receipt names the cause without the private journal; the one exception is a
  tag containing a secret word (below), which the receipt carries as
  `unclassified` while the journal keeps the tag. `target` is left out when the
  step had none.
- On each target: `reason` (only on the target `failure` names, equal to its
  reason), and `liveCheck` / `rollbackCheck` `{lastCommit, waitedMs}` from the
  effect's probe waits. Only a 40-hex `lastCommit` and a `waitedMs` from 0 to
  3600000 are sent; a null or anything else is left out, and so is a wait with
  nothing left. A probe's `polls` and `capture` never leave the private journal.

The hub refuses (400, the job stays merged and unreceipted) a `failure` on a
released receipt, a step that is not a lowercase token, an unknown target, a
target `reason` that does not match `failure`, a probe wait out of range, and a
reason outside the 160-character tag set above or containing, in any case,
`token`, `secret`, `password`, `passwd`, `bearer`, `authorization`, `api key`,
`apikey` or `private key`. The runner sends such a reason as `unclassified`, so
the hub never refuses a real receipt; no fixed tag contains those words. A
released receipt keeps its earlier shape, with none of these fields. `tt
deployment list` (the settled summaries) and `tt deployment get` show them.

All of these fields are optional, so a receipt without them, from an older
runner or saved before them, keeps its request JSON, request hash and digest
byte for byte, and finishes, replays and reconciles as before. The runner
accepts the hub's saved receipt (on finish and in the pending-receipt
recovery) when it equals the one sent, or, only when the saved receipt has none
of these fields, when it equals the one sent without them: what a hub or `tt`
older than them saves. Every other field is still compared exactly.

Rollout: the hub (with the bridge and the Mini `tt`) before the runner. An
older hub refuses a receipt with these fields (unknown field) and an older `tt`
drops them, so do not deploy the runner alone. A release of this change does
it in order by itself: the old runner deploys the hub, bridge, Mini and TailOS
and finishes with an old-shape receipt, and the deployer re-execs onto the new
runner afterwards, with no other release in flight.

**Refusal reason on the job** (wi_9b18784bb4bffb14). A job the runner refuses
before publication has no receipt, so its reason is kept on the hub job
itself: `refusal: {step, target, reason}`, shown by `tt deployment get` and in
the settled `tt deployment list` summaries. The runner sends it with `tt
deployment refuse --reason TAG [--step STEP] [--target TARGET]`, built from the
journal: `reason` is the journal's `refusalReason`, and `step` (with its
`target`, if any) is the first failed step when one was running, such as
`integrate` or `select`; a failure outside a step sends the reason alone. The
hub checks it with the receipt `failure` rules above: a reason outside the
tag set or containing a secret word, a step that is not a lowercase token, an
unknown target, or a target without a step is refused (400, the job stays
claimed), and so is a refusal on any operation but refuse. The runner sends a
reason with a secret word as `unclassified`, as it does for a receipt. The
private journal still keeps `refusalReason` and `failureDetail`, and the
refusal notice still names the journal's reason.

The refusal is optional. A job refused before this change, by an older runner,
or by the handler's reconcile has none, and its saved row and a bare refuse's
request hash are unchanged. When the hub refuses the refusal (400: an older
hub's unknown field, or a refusal it holds invalid), an older `tt` does not
know `--target`, or the hub already saved a bare
refuse under the same request ID, the runner sends that refuse once more bare,
with the same job, generation and request ID, and the job is refused without a
reason. The first, failed call is recorded as a deployer CLI failure. So this
runner may run beside an older hub or `tt`; deploy the hub before the runner
as usual to keep the reason.

**Revert on the block** (wi_816228d539a80c48). A release that fails after
publication with no receipt (the runner published tasks-hub, but the hub's
`merged` reply never reached it) ends with `tt deployment block`, not a
finish. That block carries the journaled tasks-hub revert: `tt deployment
block --revert-outcome committed|failed [--revert-commit SHA]
[--revert-bug-request ID]`, from the journal's `revert` (its commit when one
was made, and the bug request ID when the request was sent). The hub checks it
by the receipt revert rules (`committed` needs its commit, any commit must be
a full SHA, the bug request ID is at most 128 bytes with no space or line
break), refuses a revert on any operation but block (400), and saves it on the
job as `revert: {outcome, commit, bugRequestId}`, shown by `tt deployment get`.
The handler's later refuse reconcile keeps it, and also records the job's
`settledAt` (the hub time of that reconcile), which the quiet-window ledger
reads.

When the hub answers that block `generation changed`, the runner reads the job
once (`tt deployment get`) and sends the same block at the hub's generation
only when the job is `merged` and `published` under this exact agent and run,
with unchanged pins, at the runner's own integrated commit and a later
generation: the hub saved `merged` and its reply was lost. Anything else, or a
failed read, fails as before. This re-read only blocks the job; it never
resumes a release.

The revert is optional. A bare block, from an older runner or with no revert
journaled, is saved exactly as before, with no `revert`: that revert is
unknown, never read as `failed` or `committed`. When the hub refuses the revert
(400: an older hub's unknown field, or a revert it holds invalid), an older
`tt` does not know `--revert-outcome`, or the hub already saved a bare block
under the same request ID (409 `retry changed`), the runner sends that block
once more bare, with the same job, generation and request ID. A transport
failure or any other refusal sends no bare block. Deploy the hub before the
runner as usual to keep the revert.

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
retention        {releasedBackups (optional, integer >= 0, default 3), backupBudgetBytes (optional, integer >= 0, default 4294967296; 0 = no budget), releasedDists (optional, integer >= 0, default 3), attemptDays (optional, integer >= 0, default 3)}
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
   selected, `ID-gN-truenas-plan.json` with `deployment.targets = ["hub","bridge"]`,
   release `ID-SHA12-truenas` for both binaries and backup
   `before-ID-gN-truenas.sqlite`; when only one is selected, `ID-gN-TARGET-plan.json`
   with `deployment.targets = [TARGET]`, release `ID-SHA12-TARGET`, backup
   `before-ID-gN-TARGET.sqlite` and the other target's live mount retained. For
   each plan it runs `truenas_release_preflight.py --plan --receipt-output` once
   to create the backup, pins the receipt hash and copies the backup locally when
   the store schema changed. `tests/truenas-release-preflight.test.js` builds its
   fixtures with the process's group, so its results do not depend on the temp
   directory's group. Each hub/bridge manifest entry records its
   `planTargets` and names `--rollback-to` and rollback probe commands for that
   target's own probed live release. Mini and TailOS get their rollback probe and retained-dist program.
   The manifest `journalDirectory/ID-gN-inputs.json` is written once, mode 0600.
3. Run the printed `tt deployment inputs --job ID --generation N --commit SHA
--file PATH --request-id KEY` and reply to the deployer's request with the
   saved result. The deployer resumes on its next pass.

A refused run prints only its own reason. A manifest that already exists is
never rewritten.

**Names carry the job generation (wi_f1f0ec6eda73d8eb).** `ID-gN` is the job id
and the generation N the inputs run read from the hub, the same N it prints as
`--generation N`. NAME is `truenas` for a paired plan, otherwise `hub` or
`bridge`.

| Artifact or identity | Name |
|---|---|
| Manifest | `journalDirectory/ID-gN-inputs.json` |
| Plan | `journalDirectory/ID-gN-NAME-plan.json` |
| Preflight receipt | `journalDirectory/ID-gN-NAME-preflight.json` |
| Local backup copy (schema change only) | `journalDirectory/ID-gN-NAME-backup.sqlite` |
| TrueNAS backup | `backups/before-ID-gN-NAME.sqlite` |
| Preflight request id | `ID-gN-NAME-backup` |
| Hub import request id | `ID-gN-inputs-SHA12` |
| Deployer's request to the handler | names `ID-gN-inputs.json`, request id `ID-gN-inputs-SHA` |

The release name `ID-SHA12-NAME`, the journal `ID.json`, `ID-receipt.json`,
`ID-migration` and `ID-mini-before` are unchanged. The manifest body also
records `generation` (N) and `attempt` (the number of times the job was set
aside or requeued, the length of its `reconciliations`).

- **A job set aside and claimed again** is at a later generation, so its inputs
  run writes a complete fresh set and a fresh TrueNAS backup. No file is
  renamed or moved by hand. The earlier attempt's files and its TrueNAS backup
  are never opened for writing, renamed or removed; they stay as evidence.
  No retention removes earlier attempts' manifests, plans, receipts or TrueNAS
  backups. The local backup copies of every generation, the migration binary,
  the TailOS copies and the matrix attempt directories follow "Journal
  retention" below.
- **Running the inputs command again at the same generation is safe.** When the
  manifest exists it returns the same path, digest, targets and import command
  and makes no probe, backup or backup copy, and changes no file; a manifest whose binding differs
  is refused ("Job manifest already written for this generation with a
  different binding"). When an earlier run stopped before the manifest, it
  reuses the plan it left (refused as "Job plan already written with different
  inputs" if the plan made now differs), sends preflight the same request id
  and destination, which the tool answers `already-satisfied`, and replaces
  only that generation's own unfinished backup copy.
- **Once inputs are bound the command refuses** ("Job inputs already bound; set
  the job aside before preparing new inputs"); `--dry-run` still prints.
- **Which manifest the deployer reads.** The hub keeps the imported digest, not
  the generation it was imported at, and the import itself moves the job on.
  So the runner lists `ID-gN-inputs.json` files and uses the one whose bytes
  hash to the job's `inputsDigest`. It refuses with "Input manifest bound to
  another generation" when that file records a generation other than its
  name's, a generation that is not below the job's current one, or another
  attempt. When generation-named manifests exist but none hashes to the
  digest, for example a bound manifest whose bytes changed, the refusal is
  "Input manifest digest mismatch"; with no generation-named manifest at all
  it is "Handler input digest binding required". A file named
  `ID-inputs.json` (from scripts before names carried a generation) is never
  used as the manifest. When it is the only file with the bound digest, the refusal is "Input
  manifest has no generation in its name". The manifest must also name a
  backup containing `ID-gN-`.

### Migration rehearsal container (wi_78521ba68a8ad197)

A release whose store schema changed rehearses the candidate migration on a copy
of the imported backup before any deploy step, in a container built from the
production hub service. The migration binary is never run on the release host:
a host run passed a migration that then failed in the read-only production
container (no writable temporary directory, 2026-09-30).

**Requirement.** The release host needs apple/container: `container` on PATH
(or the path in the private config's optional `rehearsal.runtime`) with its
service running (`container system status` exits 0). No setting selects a host
run or skips the rehearsal. See [host requirements](host-requirements.md).

**Constraints.** The runner asks the integrated checkout's own
`scripts/deploy-truenas-hub.py` for the hub service that `hub_compose` builds
from this job's plan and release (after `_deployment_plan` validates the plan),
and maps it:

| Composed field | Container argument |
|---|---|
| `image` | the image, exactly as composed |
| `user` (`950:950`) | `--user 950:950` |
| `read_only: true` | `--read-only` |
| `cap_drop: ["ALL"]` | `--cap-drop ALL` |
| `mem_limit` (`512m`) | `--memory 512M` |
| `cpus` (`1.0`) | `--cpus 1` (rounded up to a whole CPU) |
| `environment` | one `-e NAME=VALUE` for every entry, `SQLITE_TMPDIR=/state` included |
| `entrypoint` | `--entrypoint /opt/tailterm-hub`, with the candidate binary mounted there read-only |
| the `/state` volume | the job's private rehearsal state directory, holding the copy as `hub.sqlite` |

The run is `container run --rm --progress none --name tt-rehearsal-ID ...
IMAGE --migrate-only /state/hub.sqlite`. Only those two mounts exist. The
composed token and key volumes are not mounted, so no production path or secret
is an argument; their environment variables name files that are absent, which
`--migrate-only` never opens. The rehearsal never contacts TrueNAS, the live
database or the production container.

A service the runner does not fully understand is refused, never run with less:
an unknown field (for example a `tmpfs`), `read_only` not true, a `cap_drop`
other than `["ALL"]`, a missing `no-new-privileges`, a missing or unreadable
`image`, `user`, `mem_limit`, `cpus`, `environment` or `entrypoint`, no `/state`
volume, or a second writable volume. A change to `hub_compose` of that kind must
change the runner's mapping in the same release.

**Refusals.** All of these stop the release at step `rehearse`, before any
deploy step:

| Reason | Meaning | Marker saved |
|---|---|---|
| `Rehearsal container runtime unavailable` | the runtime could not be started (not installed, not on PATH) | no |
| `Rehearsal container runtime not running` | `container system status` exited non-zero | no |
| `Rehearsal compose unsupported` | the composed hub service is not one the mapping above accepts | no |
| `python3 exit N` | the candidate's plan validation or compose function failed | no |
| `Rehearsal container exit N` | the migration failed in the container (`exit 137` is the out-of-memory kill) | yes |
| `Rehearsal container signal NAME`, `timeout`, `not started` | the run was killed, exceeded ten minutes, or could not start | yes |

A refusal with no marker can be retried by the same job once the host is fixed.
A failed container run cannot: its marker refuses a second attempt, as before.

**What the journal keeps.** A failed container run adds to the private job
journal's `failure`:

```
rehearsal: {runtime, image, exit: "exit N" | "signal NAME" | "timeout" | "not started", lines: [...]}
```

`lines` is at most the last 40 output lines (stderr first), each redacted by
the same rules as a probe capture (composed environment values, labelled
credentials and long opaque runs become `[redacted]`) and cut to 300 characters.
A record of any other shape is dropped. It reaches no message, notice or hub
receipt; the marker still holds no output.

**Known differences from production.**

1. Architecture: the rehearsal binary is built for the release host's
   architecture (linux/arm64 on the Mini); production runs the linux/amd64
   artifact of the same commit. A fault that depends on the filesystem, user,
   memory or environment is caught; an amd64-only fault is not.
2. `no-new-privileges` has no apple/container flag and is not enforced.
3. Memory and CPU are the size of a small virtual machine, not a cgroup limit:
   the guest kernel shares the 512 MiB, so the rehearsal is somewhat stricter
   than production, and the guest may see two CPUs. There is no override; a
   migration killed here (`exit 137`) is refused and needs an owner decision.
4. Working directory: the run sets `--workdir /`, because apple/container will
   not start uid 950 in the image's own working directory (`/home/nonroot`,
   another user's private directory). Both are unwritable to the hub.
5. The image is used by tag, so it may resolve to a different digest than the
   one on TrueNAS, and a host without it pulls it on the first rehearsal.

**Leftover container.** `--rm` removes the container when the run ends. After
a timeout, a signal or a failed start the runner also removes it by name, best
effort. A runner killed mid-rehearsal can leave `tt-rehearsal-ID` behind
(`container ls -a`); the `started` marker refuses that job, and the container is
removed by hand with `container delete -f tt-rehearsal-ID`.

### Storage cleanup at the first start (wi_e7748bbeb64236db)

The first hub start after this release rewrites the rows stored before each
usage turn, upload batch and launch plan was kept once. It is the last
migration step, so the rehearsal container above runs it on the copy of the
imported backup before any deploy step, and the hub does not listen until it
has finished.

**What it does.** Four steps in a fixed order, each walking its table by rowid
in transactions of 500 rows:

| Step | Table | Change |
|---|---|---|
| `revisions` | `usage_turn_revisions` | deletes a row that `usage_turns` also holds: same key, same revision, the same bytes. Superseded revisions stay. |
| `turn-payload` | `usage_turns` | empties `payload` when it equals the `turn` member of the row's `projection`, byte for byte. |
| `receipt-batch` | `usage_receipts` | replaces the stored upload batch with `sha256:` and its hash. This is the approved loss: the batch itself is gone, a retry of it still gets its receipt. |
| `queue-launch` | `team_queue_requests` | takes the launch plan out of a stored result that re-encodes to its stored bytes, leaving what a new request stores. |

A row that is not provably convertible is left as it is and counted. No step
writes a column a usage report, a token budget, a warning or the audit export
reads, and none touches `release_action_receipts`: old release receipts keep
their check lists (owner decision D1).

**Resumable.** Each transaction also writes the step's cursor and counters to
the `storage_migrations` table, so a start that is killed resumes at the next
batch on the following start. A step recorded `done` is never entered again; a
later start reads four rows and changes nothing. The table holds step names,
counts, byte totals and times only.

**The file does not shrink.** Freed pages go to SQLite's free list and are
reused by later writes. The cleanup never compacts the file; returning the
space to the disk is a separate, explicitly triggered step (order three of the
same item). The first start may add one page for the ledger table.

**Measured.** On clones of the production backup of 2026-10-09 (2.41 GB, 589,295
pages, 124,907 turns), measured before release:

| Run | Whole start | Of which cleanup |
|---|---|---|
| Mac mini host | 53 to 68 s | 28 to 40 s |
| Rehearsal container (512 MiB, one CPU) | 123 to 187 s | 70 to 107 s |
| A later start, host | 32 s | none |

It deleted 115,105 duplicate revisions and kept 82,953 superseded ones,
emptied 115,105 turn payloads, replaced 58,722 upload batches with their
hashes and took the launch plan out of 3,611 queue results; 96 queue results
did not re-encode to their stored bytes and were left. 182,373 pages (747 MB)
went to the free list, the write-ahead log peaked at 7.7 MB, and the file grew
by one page, for the ledger. The usage digest was the same before and after on
every project and table line. Not measured: the TrueNAS disk and the
linux/amd64 build.

**Release hold (owner decision D4).** A release that contains this cleanup is
put on owner hold when its candidate is accepted, and the owner or owner helper
releases it when no teams are running, because the hub is down for the length
of the first start. A measured time over 300 seconds returns to the owner
helper before any release. The rehearsal itself refuses a migration that runs
over its limit.

**Rollback.** The previous hub opens a cleaned database and reports the same
usage. The cleanup is not reversed; the data rollback is the release's own
backup.

**Usage digest.** `tailterm-hub --usage-digest PATH` proves that a database
reports the same usage as another, for example a backup copy before and after
the cleanup. It opens the file read-only and immutable, runs no migration,
creates no file, starts no listener and reads no state directory, and it
refuses a file whose write-ahead log is not empty, so it is for a copy or a
stopped hub's file, never the live one. It prints hashes, counts, sizes and
times only:

| Line | Content |
|---|---|
| `file` | size, page count, free pages |
| `table NAME` | row count and SHA-256 of the rows: every column for the tables the cleanup does not write, the kept columns and rows for the four it does |
| `form NAME` | rows in the single-copy form and in the form before it |
| `step NAME` | the ledger: state, rows examined, converted and left, bytes before and after, peak write-ahead log, free pages before and after, seconds |
| `export omits=…` | the one column the export hash leaves out |
| `project HASH` | turn count and SHA-256 of the usage report, the item token budgets, the warnings, the queue entry budgets and the audit export's rows |

Two runs on one file print the same bytes. To compare two files, compare their
`project` and `table` lines:

```sh
tailterm-hub --usage-digest before.sqlite | grep -E '^(project|table) ' > a
tailterm-hub --usage-digest after.sqlite | grep -E '^(project|table) ' > b
cmp a b
```

Every ordinary start writes its own time to
`work_item_history_state.checked_at`, which the audit export includes, so the
export of one database differs between any two starts in that column alone.
The digest's export hash leaves out that one column and nothing else (owner
clarification #31372).

### Journal retention (wi_e83171b4c215f626, wi_d203eb94ef27d8f8, wi_c7c79e078554a2ad)

**Where backups live.** The authoritative pre-release backup is on TrueNAS at
`/mnt/deepfreeze/tailterm-hub/backups/before-ID-gN-NAME.sqlite` (N is the job
generation of that inputs run, NAME is `truenas` for a paired plan, otherwise
`hub` or `bridge`; before `wi_f1f0ec6eda73d8eb` the names had no `gN-`). The deployer has no code path
to it and never removes it. When the store schema changed, the handler's inputs
run also copies it to the Mini as `journalDirectory/ID-gN-NAME-backup.sqlite`. That
local copy is read only by its own job's migration rehearsal; no rollback reads
it.

**Rehearsal copy.** The rehearsal migrates `hub.sqlite` in the state directory
`ID-gN-NAME-backup.sqlite.rehearsal-ID.state/` (0700), a copy of the local copy
mounted into the container as `/state`, and removes the whole directory (the
copy, its SQLite sidecars `-wal`, `-shm`, `-journal` and any temporary file)
when the rehearsal ends, pass or fail. A failed cleanup never changes the
rehearsal's result. Before copying it saves the marker
`ID-gN-NAME-backup.sqlite.rehearsal-ID.json` (0600):

```
{version: 1, jobId, backupSHA256, startedAt, outcome: "started"}
{... outcome: "passed" | "failed", endedAt, copyRemoved: true | false}   when it ends
```

The marker holds no path, output or error text. A job whose marker exists is
never rehearsed again ("Rehearsal already attempted; inspect prior attempt"),
whatever the outcome, including `started` left by a run stopped mid-rehearsal.
A state directory with no marker does not refuse; it is replaced. The marker is
named for one generation's copy, so a job that was set aside and claimed again,
whose inputs run made a new copy, is rehearsed on that copy.

**Retention rule.** At every poll, after reconciliation and before any claim,
the daemon sweeps the journal directory. It considers only jobs in
`tt deployment list`. For backups it considers only the names `ID-gN-truenas-backup.sqlite`,
`ID-gN-hub-backup.sqlite`, `ID-gN-bridge-backup.sqlite` of every generation N,
the same names without `gN-` from before generations were named, and their
rehearsal copies,
as regular files directly in `journalDirectory` or directly in a job's
rehearsal state directory:

1. A job that is `verified`, `claimed`, `merged` or `blocked`, or in any state
   the runner does not know as terminal, keeps everything, whatever the policy.
2. A terminal job (`released`, `rolled_back`, `refused`, `superseded`) loses any
   leftover rehearsal state directory: every regular file directly in it, then
   the directory once it is empty (reason `terminal`, one record per file). A
   link or directory inside it is neither followed nor removed, and then the
   state directory stays. A flat `ID-NAME-backup.sqlite.rehearsal-ID` copy and
   sidecars left by an earlier runner are removed the same way. A refused job is
   terminal for that job ID even when the handler retries its entry: the retry
   is a new job with its own journal files.
3. A terminal job loses its migration binary `ID-migration`, the Linux hub
   build its rehearsal runs (kind `migration`, reason `terminal`), only as a
   regular file directly in `journalDirectory`; a link or directory of that
   name is neither followed nor removed. No released job keeps it, whatever
   `retention.releasedBackups`: only its own job's rehearsal reads it, the
   marker allows that rehearsal once, and no rollback reads it (rollback uses
   the retained prior release and the inputs manifest's rollback program).
4. A terminal job that is not `released` loses its backup copy (reason
   `not-released`).
5. Released jobs are ordered newest first by `settledAt` compared as time; a job
   without one is the oldest, in list order. The newest
   `retention.releasedBackups` (default 3) keep their copies; older ones are
   removed (reason `count`).
6. While the bytes of all remaining listed backup copies, non-terminal jobs
   included, exceed `retention.backupBudgetBytes` (default 4294967296, 4 GiB;
   0 means no budget), the oldest kept released copy is removed (reason
   `budget`). A non-terminal job's copy is never removed, even over budget.

**TailOS copies.** The same sweep then considers every directory directly in
`journalDirectory` named exactly `tailos-dist-COMMIT` (40 hex), the copy the
retain step makes at each TailOS release (kind `dist`). The released TailOS
commits come from the job list by the rule the baselines use: a job whose
receipt is `released` with a `released` `tailos` target gives its receipt's
commit, and a superseded job whose hand release shipped `tailos` gives that
record's released commit. They are ordered newest first by `settledAt`, as the
backups are.

1. These copies always stay: the newest released TailOS commit's; the current
   TailOS baseline's (so a baseline that came from the config is covered); and,
   for every job that is not terminal, the copy of its accepted and integrated
   commits and every `tailos-dist-COMMIT` its inputs manifests
   (`ID-gN-inputs.json`, `ID-inputs.json`) name. The next job's TailOS rollback
   program and probe are in that manifest, so the copy a pending rollback needs
   stays. When such a manifest is not a readable regular file, no TailOS copy
   is removed in that sweep.
2. The newest `retention.releasedDists` (default 3) released TailOS commits
   keep their copies; at 0 only rule 1 keeps any. An older released commit's
   copy is removed (reason `count`, `jobId` the job that released it).
3. A copy whose commit is the accepted, integrated or receipt commit of a
   terminal job and is not a released TailOS commit, for example a rolled back
   job's, is removed once that job settled before the oldest release that
   keeps its copy (reason `not-released`, `jobId` that job). Until then it
   stays.

A copy is removed whole, after one record giving the bytes of its regular
files. A copy holding anything that is neither a regular file nor a directory
(a link, for example) stays whole with no record. A link named like a copy,
`tailos-dist-COMMIT.tmp`, a copy whose commit no listed job carries, and every
copy while the list has no released TailOS job are never touched.

**Matrix attempt directories.** Last, the sweep considers
`ID-integrated-verification/COMMIT-rN` of every listed job (kind `attempt`).
Measured on the Mini on 2026-10-08, three compiled test binaries
(`tailterm-hub-test`, `tailterm-historical-hub-test`, `tailterm-tt-test`, about
61 MiB per attempt together) were 98% of 7.5 GiB in 240 attempt directories;
everything else was about 67 MiB.

1. A job that is not terminal loses nothing from any attempt.
2. An attempt of a terminal job whose `run.json` says `starting` or `started`,
   or is unreadable or not a regular file, loses nothing. An attempt with an
   `ended` record, a `run.json.set-aside` or no record at all counts as ended.
3. In an ended attempt of a terminal job, each regular file directly in the
   attempt directory is removed once it is `retention.attemptDays` days old by
   its own modification time (default 3; 0 removes it at the first sweep after
   the job is terminal), reason `age`, one record per file. These names always
   stay: `receipt.json`, `context.json`, `plan.json`, `plan.preserved.json`,
   `run.json`, `run.json.set-aside` and every file ending `.log` (the check
   logs the receipt cites by `logURI` and `logDigest`).

The attempt directory and its parent always stay; no whole attempt is removed
at any age. A link or directory inside an attempt, an attempt or parent that is
itself a link, a name that is not `COMMIT-rN`, and files directly in the parent
(the layout from before attempts) are neither followed nor removed.

Files of a job not in the list, markers, journals, manifests, receipts, plans,
`ID-mini-before` and, outside a TailOS copy, anything that is
not a regular file are never touched. All four keys are optional; a value that
is not a non-negative integer stops the daemon at startup.

**Removal record.** Before each removal, one JSON line is appended and synced
to `journalDirectory/retention.jsonl` (0600):

```
{version: 1, at, jobId, file, kind: "backup" | "rehearsal" | "migration" | "dist" | "attempt", bytes, reason: "terminal" | "not-released" | "count" | "budget" | "age"}
```

`file` is the name inside the journal directory; a file of a rehearsal state
directory is `DIRECTORY/NAME`, a TailOS copy is `tailos-dist-COMMIT` and an
attempt file is `ID-integrated-verification/COMMIT-rN/NAME`. Kind `migration` has
reason `terminal`; kind `dist` has reason `count` or `not-released`; kind
`attempt` has reason `age`. A sweep with nothing to remove writes nothing. A
sweep first removes each terminal job's rehearsal files, migration binary and
`not-released` backup copies, job by job in list order; then released jobs'
backup copies by `count` and then by `budget`; then TailOS copies; then attempt
files. A sweep that fails (for example the record cannot be written) removes
nothing further, prints "Journal retention sweep failed; remaining backup
copies kept." and the poll continues. The first poll
after a change to this rule is deployed removes the existing backlog under it;
a TailOS copy whose removal was interrupted is recorded and removed again at
the next sweep.

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

A deployer that holds a `claimed` job with no effects (waiting for its inputs
binding or for an integrated matrix import; not published, no receipt, no inputs
bound) is stopped cleanly instead: send SIGTERM to the runner's `pid` in
`journalDirectory/runner-code.json`. SIGTERM or SIGINT to the
`tt deployment serve` process reaches the runner too: serve passes both on to
it and exits with the runner's status. SIGTERM or SIGINT to the pane's
`tt wrap` wrapper is forwarded once to its command's process group, which
holds serve and the runner; a member still alive 5 s later gets SIGKILL with
the whole group, which can cut an unclaim short. Descendants outside that
group (in their own group or session) are never signalled: the wrapper names
each one still running by pid and command name in its exit report and in a
Board notice. Ctrl-C in the pane goes to the command's group, which holds the
terminal, and not to the wrapper. The runner treats SIGINT the same way as
SIGTERM, but prefer SIGTERM to the runner's pid. The runner ends its
wait at once, gives the claim back with `tt deployment unclaim` (an exact-run
record, disposition `unclaimed`, with no incident or prevention item), archives
the claim's journal as `ID.json.unclaimed-gN` and exits with the line "Release
ID returned to verified at stop; journal archived." The job is `verified` again
in its queue place and is claimed first by the next run: close the deployer, or
provision a new run with `tt deployment setup`. The runner unclaims only when
the ledger's one active job is that claim of its own exact run, its journal is
of exactly that claim in `waiting_inputs`, `waiting_matrix` or `waiting_window`
with no effects,
never published, sent as merged or part of a batch, no integrated matrix run is
unsettled, the process has no matrix child and no host release lock exists, and
the hub accepts the unclaim. Otherwise it prints "Stopped while holding release
ID; handler reconciliation required." and the job stays held: a merged, blocked,
published, inputs-bound or batch job, a stop in any other phase, an unclean exit
(crash, SIGKILL, host loss) and a refused or unreachable unclaim all need
`reconcile` (below). Retiring or closing a deployer that holds a `claimed` or
`merged` job is refused, and the refusal names this stop; a run that is already
dead has its exit recorded (the hub never refuses an exit) and the job
reconciled. A self-restart onto new code keeps the same agent and run and
resumes its claim, as before.

**Reconcile.** A `blocked` job, an ambiguous journal or a stale host lock needs
`tt deployment reconcile` with a typed inspection (above). A claimed no-effect
job of a deployer that is to stop needs none: stop its runner cleanly
("Pause"). The journal's `failure` field says which step failed and why. Never
delete the
journal or lock by hand. The inspection's "no active execution" covers the
integrated matrix run as well: read `run.json` in each attempt directory of the
job and confirm the pid it names is gone (above, "Requeue").

**Stale deployer code.** A notice "Deployer is not claiming releases: ..."
means the deployer runs scripts older than the published ones and could not
restart itself. Read its `Reason:` and `runner-code.json`, then follow "The
runner's own code" above. It is the one code notice that wakes the primary
database handler and the owner helper. The other code notices are
`wake=routine` and wake no one (Which notices wake, above); they need no
action. A restart that is not followed by "Deployer now runs the published
scripts" is reported by the hub (next).

**Silent deployer.** A notice "Deployer has gone silent while release jobs
wait" or "Deployer announced a restart and did not come back" comes from the
hub, not the deployer: a release job has waited for the bound while the
deployer was not heard from, or after a restart it did not finish (A silent
deployer, above). Nothing was restarted. Check the deployer's host and
terminal, then run `tt deployment setup` for the deployer with the run named
in the notice as predecessor. There is one notice per recipient until the
deployer is seen alive again. A third notice, "Deployer runner has made no
progress while release jobs wait", means the wrapper still heartbeats but the
runner has sent no progress report for its bound (A hung runner, above); the
recovery is the same. A runner from before that change sends no reports and
gets no such notice.

**Held claim.** A notice "A release job is skipped because the hub refused its
claim" or "A release job is skipped because its set-aside journal is held"
means a `verified` job is not being released and will not be until someone
acts: the deployer skips it at every poll, prints `Release claim held; handler
reconciliation required.` (or `Set-aside release journal held; ...`) in its own
terminal and goes on to later jobs. Nothing of that job was integrated,
published or deployed, and it holds no project fence. A claim that fails only
in transport (the hub gave no answer) is not a held claim: it sends no such
notice and belongs to the transport wait.

- The text names the job, its item, the first 12 characters of its commit and a
  `Reason:`; the refs carry `release-job`, `item` and the full `commit`. For a
  refused claim the reason is the last line the hub's refusal printed, followed
  by the failed call in brackets, for example `hub: 409 conflict: release:
  candidate changed since enqueue (tt deployment claim exit 1)`. Only plain
  words, numbers of at most six digits and hub ids are repeated; every other
  word (a path, a token, a `NAME=value` pair) reads `(removed)`. The exact
  stderr stays in `cli-failures.json` (Failed CLI calls, above). For a held
  journal the reason is fixed: the journal is unreadable, shows effects or is
  not of exactly the set-aside claim.
- It is sent directed, so the recipient is woken, to two agents: the project
  database handler (the one `tt deployment handler` names) and the owner helper
  (the one agent of the roster, `tt agents --json`, whose role is
  `owner_helper` and whose status is neither `closed` nor `exited`). Each gets
  it once per job per deployer run; later polls of the same run send nothing
  more, even if the reason changes.
- **Fallback.** While a recipient cannot be resolved (no handler, or not
  exactly one live owner helper) or its post fails, the deployer prints `Held
  claim notice not delivered to every recipient; the next poll retries.`,
  posts the same notice once per job per run on the Board with no recipient,
  and tries that recipient again at every poll. A Board notice is in every
  agent's inbox. This one is `wake=attention`, so it wakes the primary database
  handler and the owner helper when they are live (Which notices wake, above),
  and no other agent; read it as "someone was not reached". No failure here
  changes the skip.
- Request ids are `JOB-claim-held-RUN-HASH-handler`, `-helper` and `-board`.
  `HASH` is eight hex characters of the case and the reason. A restart under
  the same run with the same reason resends the same ids, and the hub answers
  with the originals, so nothing new is posted; with another reason the ids
  differ and each recipient gets a new notice.
- **Who acts: the database handler.** Read the reason and `tt deployment get
  --job JOB`, and record what is found before changing anything. `candidate
  changed since enqueue` means the entry's accepted revision, commit or
  verification no longer match the job, so the job cannot be claimed as it is.
  `project generation changed` and the deployer-run refusals (`available exact
  deployment run required`, `deployment heartbeat stale`) concern the project
  or the deployer, not the job. A held journal goes through **Reconcile** with
  the journal as inspected evidence; never delete or edit it by hand. The
  deployer needs no restart: when the hub accepts the claim, or the journal no
  longer holds, the next poll takes the job. A job that can never be claimed
  stays `verified` and skipped until the handler closes it; if no recorded
  step fits, the handler asks the owner.
- **The owner helper is informed** and does not act on the job; it follows up
  only when the notice stays unanswered.

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
- TailOS: from the deployer checkout, `DEPLOYER_CHECKOUT/node_modules/.bin/wrangler
pages deploy journalDirectory/tailos-dist-PRIOR --project-name tailos --branch
main --commit-hash PRIOR --commit-dirty=false`,
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
