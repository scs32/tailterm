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
`--agent-id`, explicit host checkout and generic command. The handler launch
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
other unfinished journals require reconciliation. Exact-job host inputs also wait before publication for a handler-imported digest (`tt deployment inputs --job ID --generation N --commit SHA --file PRIVATE_MANIFEST`). The manifest at `journalDirectory/ID-inputs.json` binds version 1, jobId, acceptedCommit, integrated commit, verificationDigest and per-target inputs; its raw-byte SHA is immutable for that job. A reused manifest or different SHA is refused. A lost final-receipt response retains receipt_pending and retries the same write-once receipt without rolling back live-verified targets; saved hub receipts reconcile that local pending state. The daemon prioritizes its own waiting claim and refuses a later release while another claim or blocked fence remains. The release branch update
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
and starts no listener, broker, relay or Tailscale node. Per-target middleware
plans retain unchanged executable mounts. Mini builds then installs atomically,
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
than exiting; it never skips a real claimed/merged/blocked project fence.

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

`tt deployment supersede --job ID --generation N --released-commit SHA --release
NAME --repo CHECKOUT --request-id KEY` is handler-only. It applies only to a
`verified` job that no deployer ever claimed, reconciled or finished. The CLI first
checks, in `CHECKOUT`, that the job's commit is an ancestor of the released commit
and that the released commit is an ancestor of the local `refs/heads/tasks-hub`;
run it from a checkout whose `tasks-hub` is current. The hub saves
`supersession: {releasedCommit, release, agentId, runId}` as the job's receipt of
the hand release, keeps its history and retry identity, and sets the terminal
state `superseded`, shown as "superseded (released by hand)". Claim, the runner
and the project fence skip it. Each use needs its own recorded order; there is no
standing order to supersede.

### Probes and rollback programs

`scripts/release-probe.mjs` prints only the fields the runner reads:

- `live hub|bridge --config PRIVATE`: over the established `truenas` SSH route,
  read-only `midclt call app.config tailterm-hub` finds the mounted release
  binary, whose bytes give `artifactSHA256` and whose Go build info gives
  `commit` and `integrity` (`vcs.modified=false`). `containersRunning` comes from
  `app.get_instance`; `hubResponds` (and `migrationsApplied`, since the hub
  listens only after migrating) from `tt projects`. Output adds `release`.
- `live mini --config PRIVATE`: the installed `tt` hash and build info,
  `relayRunning` from `launchctl print gui/UID/LABEL`, and `newErrors` counted
  in the relay log after the offset the runner saves at deploy
  (`journalDirectory/mini-relay-offset.json`).
- `live tailos`: the public `release.json` commit.
- `rollback hub|bridge --expect-release NAME --expect-sha SHA`,
  `rollback mini [--expect-sha SHA]`, `rollback tailos --expect-commit SHA`:
  `{restored, databaseWritesPreserved}`. Hub/bridge are restored when the live
  release and hash equal the expected prior values; database writes are preserved
  when the state mount is unchanged and the hub responds.

A probe failure prints nothing and exits 1. `scripts/deploy-truenas-hub.py
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
targets.hub      {host: "truenas", liveProbe: [...]}
targets.bridge   {host: "truenas", liveProbe: [...]}
targets.mini     {installPath, relayRestart: [...], relayLog, relayLabel, liveProbe: [...]}
targets.tailos   {url (optional)}
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
2. `node scripts/release-inputs.mjs --config PRIVATE --job ID`. For each selected
   hub/bridge target it writes a per-target plan from `inputs.planTemplate`
   (`deployment.targets = [TARGET]`, release `ID-SHA12-TARGET`, backup
   `before-ID-TARGET.sqlite`, the other target's live mount retained), runs
   `truenas_release_preflight.py --plan --receipt-output` to create the backup,
   pins the receipt hash, copies the backup locally when the store schema changed,
   and names `--rollback-to` and rollback probe commands for the probed live
   release. Mini and TailOS get their rollback probe and retained-dist program.
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
and, once the agent is provisioned, a job the handler superseded naming that
release. `tt deployment list` shows which.

**Pause.** Only between releases: confirm `tt deployment list` has no `claimed`
or `merged` job, then `tt retire DEPLOYER`; every release action is refused while
it is retired. Resume with `tt resume DEPLOYER`. Do not retire during a release:
its next fence fails, the fenced rollback cannot run, and the job is left blocked
with a pending receipt for reconciliation. `tt project-pause pause` closes the
deployer with the rest of the project; resuming then needs provisioning again.

**Reconcile.** A `blocked` job, an ambiguous journal or a stale host lock needs
`tt deployment reconcile` with a typed inspection (above). Never delete the
journal or lock by hand.

**Manual rollback, per target.**
- hub or bridge: `python3 scripts/deploy-truenas-hub.py --rollback-to PRIOR_RELEASE
  --target hub|bridge --expect-sha256 PRIOR_SHA`, then `node
  scripts/release-probe.mjs rollback TARGET --expect-release PRIOR_RELEASE
  --expect-sha PRIOR_SHA --config PRIVATE`. The prior release and hash are in the
  job's manifest (`rollbackProgram`) or a previous receipt.
- Mini: copy `journalDirectory/ID-mini-before` over the installed `tt`
  atomically (copy to `tt.rollback`, then rename) and restart the relay with the
  configured command.
- TailOS: `npx wrangler pages deploy journalDirectory/tailos-dist-PRIOR
  --project-name tailos --branch main --commit-hash PRIOR --commit-dirty=false`,
  then `node scripts/release-probe.mjs rollback tailos --expect-commit PRIOR`.
- tasks-hub: if the D1 revert failed, commit the revert by hand in the dedicated
  checkout and move the ref with `git update-ref refs/heads/tasks-hub REVERT
  INTEGRATED` (never force).

**Hand release while the agent is provisioned.** Retire the deployer (pause
above), release by hand, then order the handler to supersede that item's job with
the released commit and release name, and resume the deployer.

**Detached main checkout (#15223).** The deployer's checkout is a detached
worktree (`git worktree add --detach PATH tasks-hub`) that shares objects with the
main checkout, so candidate commits resolve. At provisioning the main checkout
runs `git checkout --detach tasks-hub`; while any worktree has `tasks-hub` checked
out, the deployer refuses to publish. To update the main checkout later,
`git checkout --detach tasks-hub`. To release by hand from it (deployer retired):
build and deploy from the detached `HEAD`, then `git update-ref
refs/heads/tasks-hub NEW OLD` and `git push origin NEW:refs/heads/tasks-hub`,
without checking the branch out.

