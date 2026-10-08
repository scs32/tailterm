# Native objective verification

When an order or plan designates acceptance criteria for independent verification,
the lead lists their IDs in `body.verificationCriteria` on the frozen ASSIGN and
REVIEW. The general reviewer reports `pending-verification` for those IDs. The
handler-imported eligible receipt for the exact accepted candidate, current scope
and assignment determines whether they pass. Reviewer-owned criteria still need
passing verdicts, and a missing, stale or ineligible receipt blocks acceptance.

Feature `wi_f23f767415ef9b30`, owner order #11571, assignment #11766,
prospective Start #11775. Owner decisions #11753–11755 approve the full local
matrix, a separate fifth verifier and native evidence with AIV unsubmitted.
Documentation supplement #11790 and fixture supplement #11806 retain scope.
Round-one correction #11865, exact Start #11877/release #11881 and owner decision
#11866 add the immutable post-rollout enrollment boundary. Corrected ownership
#11887 is the 47-path manifest; initial 13385477 is diagnostic only.

The builder freezes a commit. The handler independently freezes a version 1
plan before the verifier runs it. Plan selection uses the union of the saved
concrete ownership manifest and every base-to-candidate changed path, including
both rename sides and deletions. `verification/matrix.json` inventories every
standalone local browser entrypoint; imported helpers are covered by those parents.
Only the two explicit deployed-site entrypoints are excluded. Unknown paths,
changed inventory, omitted commands and missing prerequisites refuse execution.
Six formerly Chromium-only entrypoints accept `TEST_BROWSER=chromium|webkit`.
The WebKit microphone fixtures return a real oscillator-backed MediaStream;
application recording and AudioWorklet processing remain real. Their prototype
override survives WebKit media-device wrapper collection, and track stop is
idempotent. Static WebKit keeps real native keyboard paste, while fixture API
clipboard reads grant the permission normally presented by Safari's native
popover. This does not test that native permission UI. Its imported pane-group
helper and all assertions remain unchanged; only that invocation translates
Tab to Safari's native Option+Tab through the test page keyboard, restored in
`finally`. [Apple documents this traversal](https://support.apple.com/guide/safari/cpsh003/mac).
Static voice cancellation leaves fixture fullscreen before sending real Escape;
the unchanged helper still checks microphone tracks end. Fullscreen coverage
remains in its separate stages. The fixture API clipboard permission is installed
per document so continuity reloads retain the same permission assumptions.
No host accessibility preferences or product keyboard code change.
Existing dual-engine entrypoints run both engines in one check. Narrow-layout
assertions remain in the full parent suites. Selected browser checks build and verify the local static package first.
Hub paths require vet, the full Go
suite and race checks on touched Go packages (all packages for directory ownership).
Every `hub/internal/store/` path selects migration rehearsal, covering schema SQL
outside migrate.go. Every Go check receives `VERIFICATION_BASE_COMMIT` from the
plan. Rehearsal creates the fixture database with that exact base implementation,
opens it with the candidate, reopens the base, reopens the candidate and checks
retained evidence plus integrity/foreign keys. The required check never uses an
item-specific hard-coded base. A direct unit invocation defaults to its checkout
HEAD; matrix execution always supplies the governing base. WASM changes build the test asset.

The normalized ownership manifest is handler-approved scope evidence, linked to
the raw assignment and its canonical ownership digest. The runner never parses
assignment prose. The handler must independently reproduce matrix selection from
that manifest and the full diff before saving a plan. The hub stores the approved
checks/digest independently of the producer receipt; it cannot inspect a remote
repository or attest command execution.

## Freeze, run and import

Create a plan input containing `itemTaskId`, `itemId`, `version`, `operationKey`, canonical `repository`,
exact `baseCommit` and `commit`, `itemRevision`, `scopeRevision`, `orderMessageSeq`,
`assignmentSeq`, `assignmentOwnershipDigest`, exact builder and verifier agent/run
IDs and `owned` (concrete paths), `approvedMatrixDigest` and
`matrixApprovalMessageSeq`. Approval must be a separate owner-authored Board
message whose entire trimmed text is `verification-matrix-approval:SHA256`.
The runner refuses candidate matrix bytes differing from that approved digest;
the native plan save verifies the owner source and token. A worker-authored token
or a candidate's replacement digest cannot authorize a weaker matrix. A new
matrix requires a new explicit owner approval.
An owner matrix approval covers any candidate, for any item, while
`verification/matrix.json` bytes are unchanged: no per-candidate re-approval.
This is declared shared-workspace
provenance, under the same trust boundary as other owner messages. Canonical digests use recursively sorted JSON
keys, UTF-8 bytes and SHA-256; matrixDigest hashes the exact matrix file bytes.

```sh
node scripts/verify-matrix.mjs plan context.json plan.json
# Database handler only, on the candidate CLI:
tt verification plan --item ITEM --file plan.json --request-id KEY --generation 0
# A distinct verifier, in a fresh detached checkout of the exact commit:
node scripts/verify-matrix.mjs run plan.json /absolute/external/log-directory
# Database handler only, after checking every log's exact digest:
tt verification receipt --item ITEM --file /absolute/external/log-directory/receipt.json --request-id KEY --generation 1
tt verification history --item ITEM
```

The runner creates a fresh temporary HOME/TMPDIR for each invocation and removes
it after writing external logs and the receipt, including on failed checks and
timeouts. It makes Go's read-only module-cache entries writable before removal;
cleanup walks only that invocation's home and does not follow symlinks. The
receipt's allowlisted environment records `VERIFICATION_KEEP_HOME=0` and the
exact `HOME` path. For local debugging, append `--keep-home` to retain that home;
the receipt then records `VERIFICATION_KEEP_HOME=1`, and the operator must remove
the retained directory when finished. `--min-free-bytes N` sets the free-space
reserve (default 2147483648 bytes). The runner checks available space before
allocating a home and before every check attempt, and refuses to start an
attempt below the reserve. A failed preflight reports the available and required
byte counts. This reserve guards attempt starts; a single attempt can still use
more than the available space. SIGINT or SIGTERM stops every active check process
group, saves each partial log, starts no further attempt or check, and leaves no
eligible receipt. If home removal fails, the runner removes `receipt.json`, writes
`cleanup-error.json` beside the logs, exits nonzero, and reports the retained
home path for manual recovery.

Go's `GOPATH`, `GOMODCACHE`, and `GOCACHE` all resolve inside the disposable
home for matrix checks. The binary preparation helper runs before those checks
with its existing host-cache environment; a signal during a preparatory Go build
is observed when that build returns, before any check or eligible receipt. We
decline the optional shared module download cache for this phase: it
would require a separately managed read-only cache with its own ownership,
integrity and cleanup policy. Keeping both module downloads and build artifacts
inside the run home for matrix checks preserves the current isolation boundary.

Plans and receipts are append-only. Every mutation has a stable request ID and
expected generation. Identical retries return the saved record after later
lifecycle changes; altered payloads or competing generations conflict without
partial writes. A new plan invalidates the prior receipt without deleting it.
Scope/assignment changes and any different accepted SHA fail eligibility.
Administrative priority/status revisions retain the original evidence provenance.
The handler must freeze a new plan whenever the candidate matrix changes.
One plan or receipt save may be up to 1 MiB (`api.MaxVerificationBody`); a full
69-check receipt with retries is about 76 KB. Other hub requests keep the shared
64 KiB limit.
Unannounced filesystem changes cannot be observed by the hub; the runner checks
exact detached HEAD and clean tracked/untracked status before and after execution.

The runner strips inherited environment, hub/task credentials, runtime settings,
vaults and tmux identity, and assigns a disposable HOME/TMPDIR. Commands run with
argv arrays and repository-relative cwd. The receipt retains the absolute worktree,
allowlisted environment, prerequisite digests, command start/end/duration, exit,
external log path and exact SHA-256. It runs every selected check and retains
failures. The approved matrix records a default ten-minute per-check timeout,
with explicit overrides (race: fifteen minutes; unit/build/release: two minutes).
Each planned check carries `VERIFICATION_TIMEOUT_MS` in its recorded environment.
A timeout sends SIGTERM, then SIGKILL after 250 ms, only to that check's newly
spawned POSIX process group; its receipt records exit 124 and
`failureReason: timeout`, and preserves the failed log and digest. A timeout or
known-baseline failure retains its raw failed status; only an approved known-failure entry changes gate eligibility. Browser checks requiring a fixed
port carry `VERIFICATION_REQUIRED_PORTS`; the runner refuses an occupied port
before spawning, recording the port and listener PID. It never terminates an
existing listener. Port inspection failure also refuses the check. These checks
are guardrails against pre-existing occupancy, not a reservation against a race
with another launcher. Required assets
include dependencies, installed Chromium/WebKit engines, test WASM, speech-fixture
WAV, Go module-license inventory and production WASM; copy the asset
from the root checkout only under the order's provenance allowance, and rebuild
when WASM source changes. Tests use disposable hubs, fake spawns and private tmux.

One transaction-level gate guards typed lead acceptance, PATCH done, keyed done
and queue acceptance. Bugs and features with a post-rollout team admission require a current passing
receipt. Enrollment is recorded atomically with the exact agent/run admission;
there is no request flag to opt out. Immutable SQL triggers refuse marker updates
or deletion. Rollout preserves pre-existing binding markers as
`legacy-pre-rollout`; unadmitted legacy work reports `legacy-no-team-admission`.
A new team on an existing item adds mandatory enrollment and cannot inherit a
legacy exemption. `tt verification enrollment --item ITEM` exposes that provenance
through the handler-only native endpoint. Legacy completion retains the previous
review/narrative requirements;
features also retain the existing complete narrative-report gate. Queue acceptance
also checks receipt repository/base against its accepted integration context.
Enrolled unknown review history cannot bypass the receipt requirement. Existing
saved historical completion remains readable. This changes completion after new admission;
it does not certify historical records or deploy a hub/CLI.

## Integrated plans keep accepted checks

An integrated plan never runs less than the accepted plan. A superset is
allowed; a narrower check never is. The hub contract stays strict: its release
import still refuses an integrated plan that omits or narrows an approved check.
The rule is applied where the plan is built, in `verify-matrix.mjs plan`.

The release runner builds the integrated plan from the accepted plan with the
integrated commit in place of the candidate, so the context carries the accepted
plan's `checks` and `checksDigest`. go-race is the one check selected from the
diff: it names the Go packages changed since the base, or `./...` when no Go
file changed. An item that changes only a non-Go file under `hub/` is accepted
on `./...`, and by integration the diff from its base also holds other items'
Go changes, so selection from that diff alone names only their packages. The
hub refused that plan (`integrated matrix omitted approved check go-race`).

When the context carries `checks`, the plan step selects as usual and then:

- keeps every accepted check that the selection reproduces exactly;
- merges go-race by package: the union of the accepted and the selected
  packages, and `./...` if either names it. Every other argument, the working
  directory and the environment must be identical;
- adds selected checks the accepted plan lacks;
- fails closed, naming the check, when an accepted check differs in any other
  way (`Accepted check differs from the selected one: ID`) or is not selected
  for this commit (`Accepted check is not selected for this commit: ID`), and
  when the carried checks do not match their `checksDigest`. With an unchanged
  matrix digest and owned list neither difference has an approved cause.

One difference does have an approved cause: the matrix-change case. When
tasks-hub gained an owner-approved matrix change after acceptance, the release
runner passes the accepted plan with the newer approval, so the carried
`matrixDigest` differs from the current matrix file's. The new matrix may
rebuild any check (timeouts, flags, environment), so for a differing id the
rebuilt check stands. A rebuilt go-race still names every accepted package, or
`./...` when the accepted one ran `./...`: the hub's matrix-change coverage
rule. An accepted go-race whose packages cannot be read, and an accepted check
the new matrix no longer selects, are still refused by name.

`checksDigest` and the matrix policy fields describe the final check list. The
plan gains no field: the hub binds a receipt to the digest of the plan fields
it knows. Plan mode instead writes a record beside the plan output,
`plan.preserved.json` for `plan.json`, and prints one line on stderr:

```json
{
  "version": 1,
  "acceptedChecksDigest": "<checksDigest of the carried checks>",
  "kept": ["go-race", "go-test", "go-vet"],
  "widened": [],
  "rebuilt": [],
  "added": [],
  "narrowerSelection": ["go-race"],
  "checksDigest": "<checksDigest of the plan>"
}
```

`kept` checks equal the accepted ones, `widened` are a go-race with more
packages than accepted, `rebuilt` are checks the newer approved matrix changed,
`added` are new, and `narrowerSelection` names the checks this commit alone
would have run on less. Every accepted id is in exactly one of `kept`,
`widened` and `rebuilt`. After a matrix change the record also carries
`acceptedMatrixDigest` and `matrixDigest`. A context without `checks`
plans exactly as before and writes no record; a stale record is removed.
Targeted plans ignore carried checks.

`run` re-derives its plan the same way and reads the plan's own checks as the
accepted ones, so an integrated plan that kept `./...` runs. A plan edited to
change, add or drop a check, or to drop a package this commit selects, is still
refused with `Altered or omitted required checks`. The run step cannot tell an
accepted `./...` from a go-race cut back to exactly this commit's selection;
the hub's import refuses that plan.

## Parallel scheduling and targeted runs

Feature `wi_82ed4c6924930bad`, order #13844. The runner executes independent
checks in parallel. `--jobs N` (1–16) bounds how many run at once. The default is
half the CPU cores or one job per 3 GiB of memory, whichever is lower, between 1
and 8. That gives 5 on the Mini. The receipt's allowlisted environment records
`VERIFICATION_JOBS`. `--jobs 1` runs strictly serially in plan order.

Scheduling is greedy. With more than one job, Go checks are offered first and
the rest follow in plan order. With one job, every check runs in plan order.
Each check holds locks while it runs, and two checks that share a lock never
overlap:

- `port:N` for each approved `requiredPorts` entry
- identical argv and cwd, so the Chromium and WebKit runs of one script (which
  share build output and screenshots) take turns
- the Go lane: go-race, go-test and migration rehearsal each saturate the CPU.
  `go-vet` (3 s, type checks only) runs outside it.
- the `SERIAL_SUITES` table in `scripts/verify-matrix.mjs`, for suites found to
  share a resource the rules above cannot see. Each entry names that resource.
  A three-run audit found none (item-message-compose and a 140 ms unit test
  flaked once each under load; that is test timing, not shared state).

`00-static-build`, `01-static-release-verify` and `wasm-test-build` rewrite inputs
that later non-Go checks read. Each runs alone among non-Go checks, only after
every earlier non-Go check has finished, and no later non-Go check starts until
it ends. Go checks read only the `hub/` module, so they neither wait for nor
block these barriers.

The Go rules come from receipt data. When they were set, go-race was the
longest check: 625 s alone, while no other single attempt exceeded 171 s. Its
`hub/internal/store` package then took 556–574 s against go test's default
600 s package timeout. With browser lanes beside it, that package timed out in
both measured parallel runs. Each time, the retried Go lane set a wall of more
than 21 minutes. So go-race starts at once and holds two job slots, which
leaves three for other checks while it runs. Non-Go check process groups also
run at `nice` 10 relative to the runner, so the CPU favors the Go lane when it
is saturated. Those timings are history: by 2026-10-06 the store package alone
took 27 to 31 minutes under race, and it now runs as shards (see "Store race
shards").

Scheduling alone cannot keep the store package under 600 s beside other work.
The owner therefore chose (#15114) to put the package timeout in the matrix.
`goTestFlags` is inserted into only the `go-test` and `go-race` argv, before
their packages. Only a `-timeout=Nm` flag is accepted, and a matrix without the
key keeps the previous argv. The first value was `-timeout=14m` under a
15-minute go-race check timeout; the matrix now has `-timeout=45m` under a
50-minute one (`checkTimeoutMs`), raised as the suite grew. Each change of the
matrix bytes needed a new owner approval of the digest.

The serialization rules live in runner code and derive from the plan, so they
never change `verification/matrix.json` or its approval. Attempts stay
sequential within a check. Receipts list checks in plan order with the same
fields, attempts, log names and status semantics as before.
`overlapViolations(receipt.checks)` in the runner reports any lock, exclusive or
barrier breach in a real receipt.

For a fix between two frozen candidates, run a targeted check set instead of the
full matrix:

```sh
# context.json: {"baseCommit": "<previous candidate>", "commit": "<fix>"}
node scripts/verify-matrix.mjs targeted context.json /absolute/external/log-directory [--jobs N]
```

Targeted mode selects checks only from the paths the fix changed, using the same
matrix rules: a docs-only fix runs `npm-unit`, a `hub/` fix runs the Go checks,
and a `client/` fix runs unit and browser checks. Unknown paths still refuse, and
so do a dirty or attached worktree and a fix equal to its previous candidate. The
run uses the same runner, retries and clean-detached checks. It writes
`targeted-receipt.json`, marked `targeted: true`, and never `receipt.json`. A
targeted receipt is advisory iteration evidence. It has no plan binding, and the
handler never imports it. The full plan runs once, on the final candidate.

Both `plan`/`run` and `targeted` refuse a base that the candidate does not
contain: `plan`/`run` with "candidate is not a fast-forward of its base; rebase
onto the current tip", and `targeted` by naming the fix candidate, the previous
candidate it does not contain and their merge base. Equal and linear commits are accepted. At acceptance,
`tt team queue accept` and the leased handler's done save read the item's current
verification plan. `--commit` must equal the plan commit, and the accepted
worktree must be a fast-forward of the plan base. That base is saved as the
acceptance base, so a candidate built on an older base is refused with "not a
fast-forward of the recorded base". Items without a plan keep the queue entry's
base. The team rebases onto the current tasks-hub tip before the handler freezes
the final plan, so the verified commit is exactly what merges.

## Store race shards

Feature `wi_d6b6387726c1f846`, owner order #27232. The `hub/internal/store`
package under race is one test process on one core. Measured on the Mini on
2026-10-06 (542 tests, commit `539683d`) it took 1645 s beside another matrix,
1645 s with the host 71% idle and 1851 s beside browser-heavy matrices. The
cost is flat: the slowest 20 tests are 29% of the time and the median test is
1.7 s, so removing all 20 would still leave about 19 minutes. The package
therefore runs as shards: several race processes that each run part of the
tests, inside the one `go-race` check.

**The key.** `verification/matrix.json` carries
`"goRaceShards": { "./internal/store": 4 }`: one package (a `./` path, not
`./...`) and K, an integer from 2 to 8. Anything else is refused as
`Invalid matrix goRaceShards`. Changing K, or the package, changes the matrix
bytes and needs a new owner approval of the digest, like any matrix change.

**The argv.** With the key, every go-race check has the same prefix, whatever
its packages:

```
node ../scripts/verify-matrix.mjs go-race -timeout=45m -shards=./internal/store=4 PACKAGE...
```

The cwd stays `hub`, the check ID stays `go-race`, and the packages are
selected as before. The prefix is the same for a store change, an API-only
change and `./...` on purpose: the hub and the release runner cover an approved
go-race by an integrated one only when the argv before the first package is
equal. Without the key the argv is `go test -race …` as before. `go-test` and
every other check are unchanged.

**What the command does.** It accepts only `-timeout=Nm`, `-shards=PKG=K` and
`./` packages.

- A list with neither the shard package nor `./...` runs one
  `go test -race -timeout=Nm PACKAGE...`. A Go change outside the store behaves
  as before.
- Otherwise `./...` is expanded with `go list`, the shard package is taken out,
  and two phases run one after the other, never together:
  1. The other packages, when there are any: one
     `go test -race -timeout=Nm OTHER...`, then the line
     `go-race other packages: N packages, S s, exit C`. If it fails, no shard
     starts and the check fails.
  2. The shards: `go test -race -json -count=1 -timeout=Nm (-run|-skip) REGEX
     ./internal/store`, at most `min(K, VERIFICATION_JOBS)` at once, so a
     one-job run takes them in turn. `-count=1` keeps a cached result from
     standing in for a run.

Each shard's whole test output is printed as plain text in shard order,
followed by `go-race shard I/K: N tests, S s, exit C`, so the log keeps every
`=== RUN` line and each shard's seconds. A failed shard fails the check, and a
retry reruns the whole check.

**The shard rule.** A test's shard is
`parseInt(sha256(name).slice(0, 8), 16) % K`: the first 8 hex digits of the
SHA-256 of its top-level name, modulo K. Line `I/K` of the log is hash value
`I - 1`. A test keeps its shard when other tests are added or removed. The rule
does not balance by duration (on the measured run the four shards summed to
305, 377, 518 and 444 s of test time); the per-shard seconds in every log show
drift.

**The catch-all.** Shards 1 to K-1 run `-run '^(their names)$'`. Shard K runs
`-skip '^(every name of the other shards)$'` instead of listing its own, so a
test that no list named still runs once, there. A shard other than the
catch-all with no names is not started.

**The three checks.** K and the package are frozen in the plan's argv; the
partition is not, because its digest changes whenever any item adds, removes or
renames a store test, and an approved plan and its integrated plan must keep
the same argv prefix. The partition follows from the test names at the plan's
commit and K, and the run refuses a worktree that is not at that commit. Three
checks hold it:

1. Plan creation reads the package's `_test.go` files from the commit with
   git, partitions the names and fails unless every name is in exactly one
   shard. It prints, on stderr,
   `go-race shards: ./internal/store K=4 tests=N partition sha256:DIGEST`.
2. The run lists the tests with `go test -race -list .` and fails, naming the
   difference, unless that list equals the names in the worktree's source. It
   prints the same line, with the same digest for the same commit.
3. After the shards end, the run fails unless every listed test started
   exactly once across all shards (top-level `run` events of `go test -json`)
   and no unlisted top-level test ran. Its last line is
   `go-race shards: ./internal/store K=4 tests=N ran-once=N partition sha256:DIGEST peak-shard-processes=P`.

`DIGEST` is the SHA-256 of the canonical JSON of the K sorted name lists. The
names are every top-level function in the package directory's `_test.go` files
matching `^func ((?:Test|Fuzz|Example)[A-Z_0-9]\w*|Test|Example)\(`, except
`TestMain`. The rule reads no build tags. A test behind a build tag, or an
`Example` with no `// Output:` comment (Go compiles it and never runs it), makes
check 2 stop the run; fix the test file or the rule, never rely on the
catch-all.

**Slot weight.** A go-race that will shard (its packages include the shard
package or `./...`) holds `max(2, ceil(K × 1 GiB / 3 GiB))` job slots, capped at
the job count: 2 for K up to 6, 3 for K 7 or 8. 1 GiB per race process is the
admission budget, above the 0.63 GB largest store race test process and the
0.9 GB largest process of the `go test` tree that were measured; 3 GiB is what
the default job count assumes per slot. A go-race that will not shard keeps
weight 2. Because the phases never overlap, the tree during the shard phase is
the K shard commands only, and a `./...` run never has more race test processes
at once than `go test -race ./...` had. The Go lane lock is unchanged.

**Timing.** A four-shard prototype of the same rule beside another matrix took
577 s (shards 378, 468, 577 and 528 s), each test run exactly once. A `./...`
or mixed run adds the other packages' time before the shards, about one
minute; a store-only change pays nothing.

**Changing K.** Edit the one number in `goRaceShards`, get the owner's approval
of the new matrix digest, and release it like any matrix change. A go-race
approved under the previous matrix is then covered by the integrated one only
as an approved matrix change, while it still tests every approved package (see
"Integrated plans keep accepted checks"); another K under one matrix is another
argv prefix and is refused. If a receipt's go-race passes 12 minutes as the
suite grows, K=5 is the next step: its longest shard is 385 s of the measured
test time against 518 s for K=4.

The hub's coverage rule (`goRaceCovers`, `matrixChangeCovers`) and the release
runner's copies read "prefix, then only `./` packages" and needed no change.
Tests: `tests/verification.test.js` (the shard rule, plan creation, argv, slot
weight, and the command on a real Go module) and
`TestReleaseImportCoversShardedGoRaceBySuperset` and
`TestReleaseImportCoversGoTestRaceByShardsOnlyAsMatrixChange` in
`hub/internal/store/releases_test.go`.

## Host lock and waitlist

Features `wi_6b4af2fb034ec36e`, order #18569 (phase 1) and
`wi_c5cb667695c3614c`, order #19197 (phase 2: the deployer, the queue list and
the holder cap), extended by `wi_af52350b616dd0e4@4`, order #21517,
ASSIGN #22512. One host admits two verification holders by default. Every
`verify-matrix.mjs run` and `targeted`, the deployer's integrated run, and every
supplemental run started through the `exec` wrapper below, takes one host lock
before it starts and releases its own lease when it ends. A run that finds
all slots occupied or its canonical worktree in use joins an ordered waitlist. Nobody checks `ps` or waits for a quiet period any
more: the lock file is the only thing a waiting run reads.

**Order.** A waiting release run first (see "The deployer" below), then
urgent, then high, then everything else, and first come first
served within a priority. Arrival order is a sequence number assigned when the
request joins, so two requests with the same timestamp keep their order. A run
that holds the lock is never interrupted for a more urgent one.

**Priority.** `--priority urgent|high|normal` on `run`, `targeted` and `exec`.
Without the flag, `TAILTERM_MATRIX_PRIORITY` with the same values. Without
either, `run` and `targeted` look up the item priority (low maps to normal),
falling back to normal with a warning; `exec` defaults to normal. Invalid
overrides are refused before the
run joins the list. Priority is self-declared and is not a plan field; the lock
file, journal, sidecar and receipt record it with its source (`flag`,
`environment`, `item` or `default`). Use the priority of the work item being verified.

**Files.** The lock and waitlist are one file,
`~/.local/state/tailterm-matrix/host.json`. Set `TAILTERM_MATRIX_HOST_LOCK` to
an absolute path to use another (tests do). The directory is mode 0700 and the
files 0600. Beside it are `host.journal.jsonl` (append-only history),
`host.json.lock` (a short-lived mutex for updates) and
`host.json.lock.reclaim` (a guard used while recovering a dead owner's mutex).

`TAILTERM_MATRIX_MAX_HOLDERS` is a decimal integer from 1 through 16;
unset or empty means 2. Invalid configuration refuses before enqueue or sidecar
creation. A v2 cohort freezes `holderLimit` while any holder or waiter remains;
a different limit refuses without joining. A drained cohort can change capacity.

**Memory-aware admission.** Bug `wi_fc5776e011eaabe1`, order #26885,
ASSIGN #26917. Slots alone let two runs start on a host that is already short
of memory, and then every run slows down. So before the head waiter is granted
a slot beside a running holder, it takes one memory-pressure reading for the
host:

- The reading is the percentage of memory the kernel does not count as
  available, 0 to 100, higher is worse. On macOS it is
  `100 - kern.memorystatus_level`, read with `sysctl -n kern.memorystatus_level`
  (found through `PATH`, then `/usr/sbin`) with a one-second timeout. Swap use
  is not the reading: swap stays allocated long after pressure falls.
- `TAILTERM_MATRIX_MEMORY_PRESSURE_MAX` is the limit: a whole number from 1
  through 99, or `off` to disable the rule. Unset or empty means 70, which holds
  the second slot of a 16 GB host only while under 30 percent (about 4.8 GB) is
  available. An invalid value refuses before the run joins the list or writes a
  sidecar. Each run reads its own environment; nothing is frozen in the file.
- While the reading is over the limit (strictly: a reading equal to the limit
  admits), the host admits one holder instead of `holderLimit`. The head waiter
  keeps its place, takes a fresh reading at every poll, and is granted when the
  reading is at or below the limit or the holder ends. Later waiters stay behind
  it; nobody is skipped.
- With no holder the head is always admitted and no reading is taken, so a
  high reading can never leave the host idle. A full host, a later waiter and
  a one-slot (`TAILTERM_MATRIX_MAX_HOLDERS=1` or v1) cohort take no reading
  either.
- A reading that cannot be taken (the command is missing, fails, times out or
  prints something else, or the platform is not macOS) falls back to the slot
  count alone, as before this rule. The run prints `matrix host: memory pressure
  unreadable (REASON), admitting by slot count` and journals `memory-unreadable`
  once.
- Running holders are never interrupted; the reading only decides new grants.
  The limit set too low costs one run at a time; set too high it is the slot
  count alone.

Each waiter's entry carries `heldReason`, rewritten at every poll: `memory`
when the head was refused a free slot by the reading, `slots` when no slot is
free (or its worktree is in use), and for a later waiter whatever holds the
head. Holders carry none. `status` appends `; held: memory` or `; held: slots`
to each waiter line and `status --json` shows the field, so a sweep can tell a
full host from a memory hold. A held head also says so in its waiting line.
When a hold starts the journal gets `memory-hold` with the `reading` and the
`max`; `acquire` gains `memoryHeldMs`. The sidecar `host-lock.json` gains
`memoryPressureMax` (the limit, or `off`), `memoryHeldMs` and, when a reading
was taken, the last one as `memoryReading`. The receipt keys do not change.
The default of 70 is not calibrated against a measured incident; these records
are the data for tuning it.

```json
{
  "version": 2,
  "host": "Stephens-Mini",
  "updatedAt": "2026-10-03T23:00:00.000Z",
  "requestSeq": 41,
  "grantSeq": 37,
  "holderLimit": 2,
  "holders": [{
    "id": "lease-a", "seq": 38, "pid": 4242, "kind": "run",
    "item": "wi_example", "agent": "verifier",
    "priority": "high", "prioritySource": "flag",
    "requestedAt": "2026-10-03T22:59:00.000Z",
    "startedAt": "2026-10-03T23:00:00.000Z", "runTimeoutMs": 7200000,
    "groups": [4250], "commit": "exact-sha",
    "worktree": "/abs/worktree-a", "output": "/abs/logs-a"
  }],
  "waiters": [{
    "id": "lease-b", "seq": 41, "pid": 4300, "kind": "run",
    "item": "wi_release", "agent": "deployer",
    "priority": "high", "prioritySource": "flag", "release": true,
    "requestedAt": "2026-10-03T23:00:30.000Z", "runTimeoutMs": 7200000,
    "grantSeqAtRequest": 37, "overtakenBy": 0,
    "nonUrgentOvertakes": 0, "overtakeLimit": 2,
    "heldReason": "memory",
    "worktree": "/abs/worktree-b", "output": "/abs/logs-b"
  }]
}
```

`release` (only ever `true` on the deployer's run) and `heldReason` (`slots` or
`memory`, waiters only) are optional: files and clients without them stay
valid, and any other value fails closed like other malformed state.

`holders` is empty when free; v2 has no singleton `holder` alias. Readers must
inspect all holders. The lease ID, PID and canonical output identify the run.
`waiters` is stored in grant order: release runs, then priority/FIFO. `kind` is `run`, `targeted`
or `exec`; each holder's `groups` tracks only its own check process groups.
Updates land by atomic rename under the existing mutex.

Occupied v1 files retain their original one-holder protocol until both holder
and waiters drain, then migrate atomically without resetting sequence counters.
Old clients refuse v2 rather than treating it as free. Malformed or unknown
state remains unchanged and fails closed. During a rollout, let old cohorts
drain and use compatible readers before admitting v2 clients; never delete a
live lock to force capacity changes.

**Worktree and output isolation.** The runner records canonical real worktree
and output paths, resolving existing symlink ancestors even for new output
leaves. Only one holder may use a worktree. A blocked head is never skipped to
fill a spare slot with a later waiter. Duplicate output/record roots are refused
before writing sidecars. `exec` uses its canonical current directory. Separate
worktrees are required for concurrent build/browser runs, as they write local
`.build`, generated binaries and previews.

**Reading the waitlist.**

```sh
node scripts/verify-matrix-host-lock.mjs status          # path, holder, live groups, ordered waiters
node scripts/verify-matrix-host-lock.mjs status --json   # the same file content with its path
```

A waiting run prints, and appends with a timestamp to `host-lock.log` in its
output directory, when it joins, whenever its position or the holder changes,
and at least once a minute:

```text
matrix host: waiting 2 of 3, holder wi_…/verifier-…/pid 4242
matrix host: waiting 1 of 1, holder wi_…/verifier-…/pid 4242 gone, check group 4250 still running
matrix host: waiting 1 of 2, holder wi_…/verifier-…/pid 4242; memory pressure over the limit, one holder admitted
matrix host: acquired after 184213 ms
```

**Wait bound.** `--host-wait-minutes N` (1–1440, default 240). At the bound the
run leaves the list, writes no receipt, names its position and the holder, and
exits 75. SIGINT or SIGTERM while waiting also leaves the list, with the usual
exit 130 or 143.

**When the lock moves on.** A waiter is granted the lock when it is first in
the list, a slot is free, no holder uses its canonical worktree and, beside a
running holder, the memory reading is not over the limit. A dead
holder frees only its own slot when its process and every recorded check group
are gone. The second case is journaled as
`stale-recovered` with reason `pid-gone`. A killed runner whose checks are
still running therefore keeps the lock until they end.

**Holder bound.** Each holder also stops itself. The timeout it asks for is
every planned check's timeout times the attempt limit plus a 30-minute build
allowance; for `exec` it is `--timeout-minutes`. That serial sum is the only
closed form that is an upper bound under every scheduling rule (exclusive
checks, shared ports, the Go lane), but it is far above what parallel lanes
take: about 36 hours for the full plan, whose runs take about seven minutes.
So every holder clamps its own timeout to the holder cap before it joins the
list: `TAILTERM_MATRIX_HOLDER_CAP_MINUTES` in whole minutes from 1 to 1440,
default 120. The clamp applies to `run`, `targeted` and `exec` alike, and the
deployer uses the same number for its integrated run. An invalid value is
refused before the run joins the list. The clamped value is what the lock file
shows as `runTimeoutMs` and what the holder stops itself at; the run prints
`matrix host: run timeout N ms clamped to the holder cap of M ms`, and the
sidecar keeps the asked-for value as `requestedRunTimeoutMs`. With the default
cap a full run is bounded at 7200000 ms, half the waiters' 240-minute bound. A
run that legitimately needs longer is stopped without a receipt
(`runTimeoutAbort` in the sidecar, `run-timeout-abort` in the journal); raise
the variable for that run.

Only a holder clamps itself. A waiter never shortens the deadline another
holder recorded, so a holder started from an older checkout, or under a larger
cap, keeps the deadline it wrote in the lock file and stops itself then.

At its run timeout the holder aborts through the normal interruption path: checks are stopped, the home is removed, no receipt is
written, the lock is released and `run-timeout-abort` is journaled. A
preparatory test-binary build blocks the runner, so the stop happens when that
build returns.

Normal two-holder admission records overlap=0. Exceptional overdue takeover
can exceed the physical concurrency limit: If a holder is still alive 120 seconds
after its run timeout, the first waiter takes the lock anyway. This is printed
as a warning, journaled as `overlap` with the process and groups that were
still alive, and recorded in the new run's sidecar and receipt
(`VERIFICATION_HOST_OVERLAP=1`). No run ever signals another run.

The first waiter takes over as many overdue holders as it needs to free one
slot, or none (owner amendment #26938). With both slots held and no memory
hold that is one holder, as before. Under a one-slot memory hold every overdue
holder in the way is taken over: a single overdue holder, or both of two
overdue holders, so hung runs cannot keep a waiter, the deployer in particular,
out until its wait bound. A holder that is alive and inside its run timeout
plus grace is never taken over: with one overdue and one in-time holder under
a hold nothing is taken over, the in-time holder's entry is untouched and the
waiter stays held for memory; when the reading falls it takes over the one
overdue holder. Each holder taken over gets its own warning line and its own
`overlap` journal event. In the sidecar `overlap` stays `1` and
`overlapDetails` is the first holder taken over; `overlaps` lists all of them.
After a double takeover three runs can be alive at once (two hung, one new) on
a host that is already over the limit; that is the price of not waiting for
hung runs.

A waiter whose process is gone is dropped (`waiter-dropped`). The journal
events are `request`, `acquire`, `release`, `withdrawn`, `wait-expired`,
`run-timeout-abort`, `stale-recovered`, `overlap` (one per holder taken over), `waiter-dropped`,
`mutex-recovered`, `guard-stale`, `corrupt-file`, `holder-update-failed`,
`memory-hold` and `memory-unreadable`.
Each line has the time, the request id, pid, item and agent. The journal is
not rotated yet.

**Cases that stop and need an operator.**

- `host.json` is unreadable, not JSON or not version 1: every request refuses
  with the file named and exits 78, journals `corrupt-file`, and leaves the
  file untouched. Read the file and `status`, confirm no verification is
  running, then move the file aside (`mv host.json host.json.bad`). A holder
  that meets this when it releases prints a warning; its receipt stands.
- `host.json.lock.reclaim` was left by a process that died while recovering a
  mutex: requests refuse with that file named (exit 78, `guard-stale`).
  Confirm no verification is running, then remove that file.
- A mutex whose owner is alive is never taken away, however long it is held.
  Waiters print `matrix host: lock file busy, mutex owner pid N` once a minute
  inside their wait bound. A holder that cannot update the file for 30 seconds
  stops its run. A mutex whose owner is gone is removed by exactly one process
  (`mutex-recovered`).

**What a run records.** The receipt's `environment` gains twelve string keys.
They are added to the receipt only; checks do not see them.

| Key | Value |
|---|---|
| `VERIFICATION_HOST_LOCK` | lock file path |
| `VERIFICATION_HOST_PRIORITY` | `urgent`, `high` or `normal` |
| `VERIFICATION_HOST_PRIORITY_SOURCE` | `flag`, `environment` or `default` |
| `VERIFICATION_HOST_WAIT_MS` | request to acquire |
| `VERIFICATION_HOST_QUEUE_POSITION` | position when queued; `0` when acquired at once |
| `VERIFICATION_HOST_QUEUE_LENGTH` | waitlist length when queued, this run included; `0` when acquired at once |
| `VERIFICATION_HOST_GRANTS_BEFORE_START` | runs granted between queueing and this run's start |
| `VERIFICATION_HOST_OVERTAKEN_BY` | of those, runs that queued later |
| `VERIFICATION_HOST_OVERLAP` | `1` when the lock was taken by the timeout exception, else `0` |
| `VERIFICATION_CHECK_SET` | `go-only`, `full` (Go and browser checks), `browser` or `unit` |
| `VERIFICATION_RUN_DURATION_MS` | acquire to receipt creation |
| `VERIFICATION_HOST_LOAD` | 1-minute load averages at acquire, every 60 s and at the end; newest 64 |

`host-lock.json` in the output directory holds the same values as numbers,
with the request id, sequence, pid, item, agent, request, acquire and release
times, the run timeout (`runTimeoutMs`, clamped, and `requestedRunTimeoutMs`), CPU count, timestamped 1- and 5-minute load samples and any recovery or
overlap details. It is written on every release, including failed, interrupted
and withdrawn runs, so the output directory always contains `host-lock.json`
and `host-lock.log`. The hub does not import these keys or the sidecar in this
phase; they are data for deciding later whether small runs can share the host.

**Supplemental runs.** Run a race suite or any other "run alone" command
through the wrapper so it joins the same list:

```sh
node scripts/verify-matrix-host-lock.mjs exec --item ITEM --timeout-minutes N --record /absolute/dir \
  [--priority urgent|high|normal] [--agent NAME] [--host-wait-minutes N] -- COMMAND [ARGS…]
```

It prints the same waiting lines, runs the command in its own process group
(recorded in `groups`), forwards SIGINT and SIGTERM to that group, stops the
command at `--timeout-minutes` or the holder cap, whichever is sooner (exit 124), and otherwise exits with the
command's own code. It writes `host-lock.json` (check set `supplemental`, plus
the argv and exit code) and `host-lock.log` to the record directory.

**The deployer.** The release runner's integrated run is a member of the same
list. The runner starts it detached with `--priority`, `--item` and
`--host-wait-minutes`, counts no processes, and polls. Its priority is the
release job's if the hub carries one, else the private config key
`matrixPriority`, else `high`; its wait bound is `matrixHostWaitMs` (default 2
hours); its holder bound is the clamped value above. The runner also sets
`TAILTERM_MATRIX_RELEASE=1` in that run's environment (argv is unchanged).
`verify-matrix.mjs run` then marks its lock entry `release: true`, and a
waiting release run is ordered ahead of every other waiter, urgent and
protected ones included, in arrival order among release runs; `status` tags it
`release`. Its grant counts in older waiters' `overtakenBy` but, like an urgent
grant, spends nobody's fairness budget. It is not exempt from the memory rule:
under a memory hold it takes the one slot next, when the holder ends or the
reading falls, so team runs cannot starve it. The variable must be `1`, unset or
empty; anything else refuses before the run joins. `targeted` and `exec` ignore
it, and it is for the deployer only. While it waits, the
deployer posts a notice with its position, the list length and the holder set each
time one of them changes. If it must stop its own run it signals only that run,
never a check group, and holds the job when it cannot confirm the run stopped.
[Project deployment](project-deployment.md), "Integrated-commit verification",
has the attempt record, the notices and the held state.

To see where a slow release stands, read `status` or `tt team queue list`, then
`<job journal>/<job>-integrated-verification/<commit>-rN/host-lock.log` for the
timestamped waiting lines, `run.out` and `run.err` there for the run's output,
and `host-lock.json` and `host.journal.jsonl` for `request` and `withdrawn`
with the wait time.

**The queue list.** `tt team queue list` (text output) reads the same file and
prints, under each entry whose work item has a run waiting, one line per
waiting run:

```text
  waiting-for-matrix position=2 of 3 holder=wi_…/verifier-…/pid 4242 priority=high kind=run agent=verifier-… since=2026-10-01T14:05:00.000Z
```

The position is the run's place in the file's order, as `status` prints it, and
`holder=none` means the host is free. A line is printed only for an entry on
this machine (the entry's host and the file's `host` compared by first DNS
label, ignoring case, so `Stephens-Mini` and `Stephens-Mini.local` match) that
has not failed. An entry on another host gets no line. The file is read from
`TAILTERM_MATRIX_HOST_LOCK`, else the default path. The list always prints and
exits 0, and `--json` is unchanged. Four cases show no waitlist:

- No file: nothing extra is printed (a free host).
- `TAILTERM_MATRIX_HOST_LOCK` is set but relative: `matrix host:
  TAILTERM_MATRIX_HOST_LOCK must be an absolute path; waitlist not shown`. The
  default path is not read instead.
- The file is unreadable, not JSON or not version 1: `matrix host: lock file
  PATH unusable (REASON); waitlist not shown`.
- The file's `host` is another machine: `matrix host: lock file PATH belongs to
  host X; waitlist not shown`.

The lock file lives on the machine that runs the matrix, so the list shows the
waitlist only when it is run there. The hub's idle-entry stall check does not
read it (`wi_772e88d71e5cd0e5`).

**Limits.** A runner in a checkout older than phase 1 takes no lock until
it is rebased, and one older than phase 2 records an unclamped run timeout,
which waiters honour. A waiter in a checkout older than memory-aware admission
ignores the memory rule and the release order: as head it takes a free slot
whatever the reading, it re-sorts the list without the release rule, and it
writes no `heldReason` for itself. Under a memory hold
the deployer waits for one whole holder. A hung holder is taken over at its
run timeout plus grace, by default at most 122 minutes after it started, and
the deployer's default wait bound is 2 hours from when it joined. So a holder
that started less than about two minutes before the deployer joined and then
runs or hangs for its whole cap, or one recorded under a larger cap, can still
end that release attempt with exit 75. The wait bound is unchanged. Builders' ad hoc test runs take no lock
and are not counted by the memory rule. A process or group id
reused by an unrelated process makes a dead run look alive; that only delays
the next run, up to the run timeout plus grace. There is a short window
between a check starting and its group reaching the file, and the children of
a preparatory test-binary build are not recorded, so a runner killed during
that build is recovered while a build child may still be running.

## Delivery lifecycle test

`TestDeliveryLifecycle` in `hub/cmd/tt/lifecycle_e2e_test.go` drives one
synthetic item through the real store, the real HTTP server and the `tt`
command entry points, from queue add to its release job. It exists so a hub
change that breaks the delivery path fails `go test` before a team meets it
live.

```sh
cd hub && go test ./cmd/tt -run '^TestDeliveryLifecycle' -count=1 -v
```

Each of its two subtests logs its steps in order: queue add (refused until the
handler confirms the scope), launch after `tasks-hub` moved, review round one
with a blocker, the one-line fix, round two resolving that blocker and filing a
follow-up, the lead's disposition while verification is pending, a full-size
receipt (69 checks, three attempts each, over the shared 64 KiB request limit),
the owner's `owner-accept`, the done save, queue acceptance on the newer base,
and the release job. Between them the subtests cover a plan that names the
builder worktree and one that names the repository root, a `follow-ups` and an
`owner-decision` disposition, and both acceptance paths: the handler's done
save carrying the acceptance, and the owner's done save followed by
`tt team queue accept`.

Acceptance creates the release job in the same transaction. The test therefore
expects exactly one verified job for the accepted commit, and expects a later
`tt deployment enqueue` of that entry to be refused because the entry already
has a release job, with a refusal that says to recover the original request
receipt.

It runs with no check of its own. The matrix rule for `hub/` selects the `go`
group, whose `go-test` check runs `go test ./...` in `hub`, so any change under
`hub/internal/store`, `hub/internal/server` or `hub/cmd/tt` runs it.
`TestDeliveryLifecycleIsInTheVerificationMatrix` fails if that rule goes away.
`go-race` runs it only when `hub/cmd/tt` itself changed.

Limits:

- The team is launched by a stub spawn that registers each member with its
  bound context. No agent runtime or tmux session starts.
- It stops at the enqueued release job. The deployer's claim, the matrix on the
  integrated commit and the deploy are not covered.
- The hub, repository, logs and matrix digest are fixtures under the test's
  temporary directories. Nothing reads a live hub, task or token.

`hub/cmd/tt/testdata/lifecycle/fails-before.md` records, once, that the test
fails when each of the five fixes it guards is taken out again. The patches
beside it are that evidence; no test applies them.

## AIV mapping boundary

`verification/receipt.schema.json` documents native v1. `operationKey` survives
native retries; `repository/baseCommit/commit`, matrix/check digests, environment,
check argv/cwd, times/exits and log URI/digest map to snapshot/run audit evidence.
Optional `aiv.service/repository/snapshot/extractor/run/audit` bindings are retained
verbatim with mandatory `state: unsubmitted`. Native fixtures exercise those
fields; no external wire compatibility, `record_run` submission, `audit_verify`
checkpoint, receipt or external idempotency is claimed. The existing spec lacks
that exact contract; external submission is the separately approved phase two.

The shared-workspace credential makes handler and verifier identity declared
native provenance, not cryptographic attestation. The handler checks roster/run,
item admission, saved order/assignment/scope, independence from builder/reviewer,
and approved check coverage. It cannot prove a producer actually executed a
command merely from submitted JSON. The owner-run bootstrap exception #11796
applies to this item's acceptance evidence only; future teams use the fifth member.

Race selection inspects Go package directories in the exact candidate Git tree.
Deleted and renamed-away paths remain in coverage selection, but are excluded
from command targets; existing moved destinations are selected. If no touched
package survives, the race check falls back to `./...`. Tests execute the real Go
race command after a complete package move and deletion.

## Known failures and retries

Successor Feature `wi_f148716909c88f9d` revision 1, owner order #11964,
assignment #12210 and separately saved Start #12214 (handler #12216) add
known failures and retries. The approved matrix bytes contain `maxAttempts: 3`
and `knownFailures`, each with `checkId`, `bugTaskId` and `bugId`. The initial
14 IDs come from owner #11950 and handler handoff #12195, linked to the open
baseline Bug `wi_be7bbed81d4009c1`; the flaky board-compose check is excluded.
Any list or retry-policy change requires a new owner matrix approval token.
The runner derives the selected exceptions and retry policy from those exact
bytes; the handler independently reproduces the plan and the store checks every
linked bug exists, is a bug, and remains open/in progress/blocked. Closing or
dismissing a linked bug removes eligibility for every other item until a new
plan resolves the entry, and a new plan can never list a closed bug.

One exception lets a bug close while the approved matrix still lists entries
that name it (Feature `wi_6fb81e10c366d7b4`, order #14489). The bug's own done
save, its queue acceptance, its release, and its integrated-commit import accept
those entries when the bug's current receipt, already bound to its plan and the
exact accepted SHA, shows each of them `status: pass` with `nowPassing`. An entry
that failed, or passed only on retry (`flaky`), keeps the bug open: a flaky self
entry is reported for removal yet still blocks its own bug's close until a later
receipt shows `pass`. Entries naming any other closed or dismissed bug are
refused as before. The queue acceptance records the resolved entries as
`resolvedKnownFailures` (receipt generation, receipt digest, matrix digest and
the exact entries), derived by the hub and never accepted from a client, so the
later matrix cleanup has an exact source. Plans and receipts are not rewritten.

Every failed check, including a listed check, runs up to two further attempts
with the identical argv, cwd, environment, timeout and process-group/port guards.
The first success stops retrying. Each attempt records its 1-based index,
start/end/duration, raw exit/failureReason, unique external log URI and digest.
Top-level check evidence matches the final attempt. `status` is `pass` for first
success, `flaky` for success on retry, and `fail` after three failed attempts.
`knownFailure` and `nowPassing` are separate flags; a listed check that succeeds
is reported for removal even if it succeeded only on retry.

Valid failed receipts are stored as evidence. Every completion path still calls
the shared eligibility gate: an exhausted unlisted failure blocks, a listed raw
failure is eligible, and flaky success remains explicitly flaky. CLI import hashes
every attempt's log, including the first failed attempt. Bounds, sequence,
stop-after-success, aggregate evidence, labels and flags are verified by the
native store. A new plan clears the current receipt while keeping old records.
Delivery reads derive summaries from the current plan and receipt; they display
check IDs, flaky status, known failures and now-passing flags. Missing receipts
show pending, changed scope/assignment shows stale, and blocked evidence never
shows passing. Optional new fields preserve existing v1 canonical digests and
legacy single-attempt receipts; legacy failures still block completion.

The full matrix must run on a clean detached frozen candidate with new owner
approval and a handler-saved plan. Its successful exit means gate eligibility,
not that every raw check passed. Independent execution and handler import/readback
remain required before acceptance. This successor introduces no AIV submission,
baseline test repairs, live data changes or deployment.

## Concurrent resource inventory and evidence

Feature `wi_af52350b616dd0e4@4`, order #21517 / ASSIGN #22512 preserves the
approved matrix digest `f57266be7f09c6f8b78443cf88c8fe3fd00ec07f01d3f84b9e8f1e0c4cf182c6`
(owner #16614). Its scope selects all eligible local browser entrypoints and
imports; deployed-browser and deployed-reset remain excluded. The source
inventory #21888 / final plan #22508 was refreshed for candidate `b9853e8`
by diagnosis #22770: 58 entrypoints and 79 modules, plus the npm unit suites
and their subprocess helpers. Correction order #22775 / saved #22788 covers
only this document, the runner and the host-lock test.

- Each real runner invocation creates a fresh disposable HOME, TMPDIR, GOPATH,
  GOMODCACHE and GOCACHE, plus a private TMUX_TMPDIR directory beneath its home.
  The home uses unique `tv-` plus six mkdtemp bytes beneath canonical `/tmp`
  (`/private/tmp` on the Mini). Its private `tmux` directory is mode 0700.
  macOS `sun_path[104]` leaves a conservative 103 payload bytes after NUL.
  The complete path is `TMUX_TMPDIR/tmux-<UID>/<socket-name>`: a 27-byte
  canonical namespace, 10 decimal UID bytes and the longest 55-byte menu name
  give 99 bytes; the current UID501 path uses 92. A cleanup-protected guard
  rejects an oversized canonical path before any checks or groups launch.
  The synthetic cancellation case forces a long outer TMPDIR and uses the
  unchanged menu socket expression, retains failed-check/cleanup logs in TAP,
  and checks actual peer survival and tmux server teardown. A separate real
  allocated-home rejection proves cleanup, lease release and no receipt.
  Credential stripping, pre-admission prerequisite checks, post-admission held
  hashes, process-group recording, cleanup and no-receipt-on-failure are retained.
- `tmux-menu-browser.mjs` uses `tailterm-menu-test-` (19 bytes) plus a
  36-byte UUID for all real `-L` calls; this is the longest participating name.
  `task-form-browser.mjs` uses `tf-`, up to 10 decimal positive PID bytes,
  a separator and eight UUID bytes (maximum 22) for all four references.
  `board-decisions-browser.mjs` configures a 29-byte never-started name for
  its ask fixture; it does not intend a tmux launch. The npm unit attachment
  fixture overrides the runner namespace with its own `/tmp/tt-XXXXXX` and
  uses implicit `default` (7) and `-L viewer` (6). Shared shell fixtures use
  fake tmux executables; `capture-pane -S -5000` is a line option, not a socket
  path. No participating real global `-S` socket is selected. Identically named
  sockets used by other fixtures live in separate TMUX_TMPDIR namespaces. Cleanup stays
  inside the owning home/socket/group; no peer is signalled by lock recovery.
- Browser listeners use the released per-run ephemeral ports. Matrix
  `requiredPorts` are occupancy checks, not reservations. Canonical worktree
  exclusion also protects each worktree's build and fixture files.
- Eleven absolute `/tmp` screenshot destinations in nine approved browser files
  now use worktree-local `.build` suite/entrypoint/engine paths. All captures and
  assertions remain. Existing relative screenshots are separated by worktree;
  upload `/tmp/fixture uploads/...` entries are in-memory SFTP fixture keys.
- The release consumer reads every v1/v2 holder, adopts only a unique matching
  output/lease/PID, and holds on ambiguity. Persisted process-start-instance
  proof still controls signalling, after-PID-gone snapshots still fence surviving
  groups, and holder-set notice keys remain stable across restart/order changes.

Builder focused tests exercise real A/B/C processes, both slot-release orders,
priority/FIFO, capacity configuration/cohort mismatch, v1 migration/old-reader
rejection, corrupt state, canonical aliases, per-ID group/release/recovery,
second-holder consumer adoption and ambiguity. Two real synthetic runPlan runs
use distinct fixture worktrees, actual ephemeral listeners and private tmux
servers; A cancellation must leave B's socket, process, home and receipt intact.
These checks do not replace the distinct verifier's full eligible native matrix
or the two-worktree/both-engine PNG decoding and peer-preservation probe in the
saved plan. Its browser/static SSH commands are synthetic; item composition
uses an isolated hub without real tmux. Its separate private `OUT/home-XXXXXX`
homes do not use the runner's allocator. The prepared short OUT would fit a
hypothetical default name, but its 116-byte worst-case menu path is not safe
and the menu suite is not selected there; refresh the budget if stages change.
Standalone `tests/real-tmux.py` and Go tmux test suites are outside this native
selection. The prior seven-byte synthetic name missed the real menu's 48-byte
length difference; retained native menu failures establish that recurrence.
The original earlier focused failure attribution remains unknown, and permanent
prevention remains open until independently verified and saved.
Keep runtime a1-a3 acceptance pending until that evidence, independent
review, lead acceptance and handler-saved completion are retained.
