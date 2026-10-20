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
Before that check, a run or targeted run in a clean linked git worktree first
provisions any missing prerequisite from the repository's main worktree through
the step `release-runner.mjs --provision-prerequisites` uses: npm ci when
`node_modules/.package-lock.json` is missing, otherwise a copy that never
overwrites. This happens before the host lock and prints each path's action and
sha256. It refuses when the main worktree lacks a file or the copy dirties the
checkout. A dirty or non-worktree checkout is not provisioned.
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
HEAD; matrix execution always supplies the governing base. WASM changes build the test asset:
a `wasm/` path or `scripts/build-wasm.sh` (an exact-path rule; other `scripts/`
files select only unit and browser) selects `wasm-test-build`. `hub/go.mod` is not
mapped to it: the build compiles the pinned Tailscale tarball's own module, and the
runner reads `hub/go.mod` only to check the script's Tailscale version, a mismatch
falling back to per-home modules rather than changing the build.
`wasm-test-build` leads the plan and is exclusive, so it finishes before
`00-static-build` packs `wasm/tailserve.wasm` and before any browser suite loads
`.build/test.wasm` (`wi_bc1e7d15925ee533`). A plan in which one of these WASM
readers precedes it is refused before admission with
`WASM check order: <id> runs before wasm-test-build`.

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

The plan's `assignmentSeq` must be the scope's saved assignment: the latest
ASSIGN of that scope revision. A later ASSIGN with the same criteria becomes
the saved assignment (one with changed criteria is refused), and the earlier
plan and its receipt then fail with `assignment changed` and show stale on
the queue entry. After the verification plan freezes, the lead sends the
builder corrections, focused fixes and rebase asks as REQUEST,
never a new ASSIGN. If a new ASSIGN is unavoidable, freeze a new plan naming
it before the next run.

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

Before its first Go command the runner writes a provenance marker,
`tailterm-cache.json`, at the home's root: producer
`scripts/verify-matrix.mjs@<commit>`, item, run, creation time, the cache
directories it owns (`go-build`, `go/pkg/mod`), `keepHome`, its pid and the
output directory. A run that is killed leaves its home behind; the worktree
sweep removes a marked home whose run is gone, and a home kept with
`--keep-home` is never removed whole but loses its owned caches. An agent
that makes its own Go home for ad hoc verification writes the same marker
with its own producer and item, under the item's artifact folder or an
ignored folder of its checkout, so the sweep can trim its caches once the item
is finished (`docs/project-queue.md`, "Verifier homes"). Unmarked homes and
Go caches are only reported, never deleted.

`GOPATH` always resolves inside the disposable home. `GOMODCACHE` and `GOCACHE`
come from the shared verification caches described next when the plan can run
Go, and otherwise resolve inside the home as before. The prepared test binaries
are built by a child process whose whole environment is the verifier's (its
home, Go caches and toolchain); `tests/test-binaries.mjs` reads the build
environment per call, never at import. The child leads its own process group,
recorded like a check's: a signal sends SIGTERM to the group, then SIGKILL after
250 ms, so `go build` and the compilers it started stop with it, before any
check or eligible receipt. Each build's `go env -json` for `GOVERSION`,
`GOROOT`, `GOPATH`, `GOMODCACHE`, `GOCACHE`, `GOFLAGS`, `GOPROXY`,
`GOTOOLCHAIN`, `GOOS` and `GOARCH` is recorded as `buildLog.goEnv` in
`<output>/test-binary-builds.json`.

### Agent CLI stubs and the home process sweep

No test run or matrix check starts a real agent CLI. The live checks gated by
`TT_LIVE_CLAUDE=1` or `TT_LIVE_RUNTIME_PROMPT=1` start one only when an
operator sets that variable by hand, and never under the matrix: a check's
environment is the runner's own (`PATH`, the disposable home and the cache
settings above) plus the check's own settings from the plan, not the caller's,
so those variables are never set there.

**Verifier stubs.** Before the first check, the runner writes a failing stub
into `<home>/.verifier-bin` for each name in `AGENT_CLIS`: `claude`, `codex`,
`aider`, `gemini`, `opencode`, `goose`, `cursor-agent`, `amp` and `copilot`,
the same list `spawn.Runtimes` in `hub/internal/spawn/spawn.go` looks up (a
test in `tests/verification.test.js` keeps the two equal). That directory is
first on every check's `PATH`, so a check that reaches an agent CLI gets the
stub, never the host's binary. A real `codex` starts a setsid'd managed daemon
under the home that no process-group signal reaches (bug
`wi_39bd40d33a4d8acc`), and a real `claude` session spends tokens (bug
`wi_1c5e64268e8f77b9`). A stub prints `tailterm verifier: <name> is disabled
during matrix checks` to stderr and exits 1 at once. Each call appends one
tab-separated line to `<output>/<name>-stub-calls.log`: UTC time, pid, parent
pid, working directory and the arguments (newlines and tabs as spaces, at
most 500 characters). The file exists only when some check reached that CLI,
so it is the audit record of which check did.

**The task form browser test's own stubs.** `tests/task-form-browser.mjs`
launches agents through its `/exec` fixture: the project database handler
runs `codex` and the form's default first agent runs `claude`. It writes its
own `codex` and `claude` stubs into its state directory, each `exec sleep
300`, and puts that directory first on the `/exec` `PATH`. They shadow the
verifier's failing stubs too, so its launched panes stay alive like the
catalog's sleep command, in a matrix run and in a direct run where a real
`claude` is installed.

**Home process sweep.** After the checks, kept home or not, the runner stops
every process of this user that still lives in or holds files under the home:
found by `HOME`, `TMPDIR` or `PWD` in its environment, by an `argv[0]` under
the home, or by any open file there (`ps` and `lsof`). The runner and its
ancestors are never listed. It sends SIGTERM, then SIGKILL to survivors, and
rescans for children that appear later. A failing `ps` or `lsof`, a process
that survives SIGKILL, or a process whose environment cannot be read and
cannot be proven older than the home fails the cleanup: the home is kept, no
receipt stands, and the error names the processes. Artifacts beside the logs:
`home-processes.json` lists each stopped process (pid, the reasons it matched
and its command, cut to 300 characters), and `home-sweep-unreadable.json`
lists the processes whose environment could not be read but which started
before the home, so the sweep left them alone (pid, error code, start time and
command, at most 200).

## Shared verification caches

Until wi_3c81378998da5acd (order #33917) every run downloaded the Go toolchain
and modules and built cold inside its home, then deleted it: on 10-13 one run
of this item's 69-check plan left 42,056 files and 1.17 GB in its kept home and
raised 182,351 file-system notifications under it, and every fresh checkout
downloaded the speech model again. The runner now keeps those inputs in one
harness-owned cache root shared by every checkout on the host.

**Root.** `TAILTERM_VERIFICATION_CACHE` when it is an absolute path; otherwise
`~/Library/Caches/tailterm-verification` on macOS and
`${XDG_CACHE_HOME:-~/.cache}/tailterm-verification` elsewhere, from the
runner's own `HOME`. Before anything is created the runner resolves real paths
and refuses a root that is inside, equal to or contains the user's `~/go`,
`~/Library/Caches/go-build`, or the `GOPATH`, `GOMODCACHE` or `GOCACHE` that
`go env` reports for the runner's environment, or that lies inside the
checkout. A root that exists without a valid marker and holds anything but the
cache's own entries is refused too. A refusal changes nothing: the run uses
per-home caches.

**Which runs share.** A plan shares when the checkout has a `hub/go.sum` and
any check belongs to a family `verification/matrix.json` defines (go-vet,
go-test, go-race, migration-rehearsal, wasm-test-build, npm-unit, the two
static checks, or a `tests/*.mjs` browser suite), because each of those can run
`go`: npm-unit builds tt in the release-runner tests and browser suites build
test binaries (lead answer #33960). A synthetic plan in a checkout with no
`hub/go.sum` keeps per-home caches.

**Layout.**

```
<root>/tailterm-cache.json                     root marker
<root>/mod/current.json                        pointer to the module generation in use
<root>/mod/gen-<16 hex>/                       GOMODCACHE: modules and, if needed, the toolchain
<root>/mod/gen-<16 hex>.stamps/<sha256>.json   one stamp per go.sum seeded into it
<root>/build/<goversion>-<os>-<arch>/          GOCACHE for that toolchain, with its own marker
<root>/speech-model/<revision>/<model>/<file>  the static build's speech model
<root>/downloads/<sha256>.tar.gz               scripts/build-wasm.sh's Tailscale release tarball
```

Markers, pointer and stamps carry the marker item's fields
(wi_60dbbbe5b36bf66b): `version: 1`, `kind` (`root`, `mod`, `stamp` or
`build`), `producer` (`scripts/verify-matrix.mjs@<commit>`), `item`, `run`,
`createdAt` and `owns`, the owned directories relative to the root; stamps add
`goSumSha256`, `source`, `toolchain` and `toolchainTreeSha256`. That item trims
listed directories under worktree and artifact roots; the shared root is under
neither, so its trimming never applies here. They are written to a temporary
name and renamed.

**Modules.** Sources are the candidate's `hub` module; when the historical
hub binary is prepared, that commit's `hub` (extracted into the home); and when
the plan has `wasm-test-build`, Tailscale's `cmd/tsconnect/wasm` as
`scripts/build-wasm.sh` builds it. The runner reads that script without running
it: its `tailscale-<version>` must be the version `hub/go.mod` requires, and its
`cp wasm/… "$source/cmd/tsconnect/wasm/…"` lines become a `go list -overlay`.
Go refuses an overlay of files beneath `GOMODCACHE`, so the module is copied
once per generation to `mod/gen-<hex>.wasm-src/` (an APFS clone on macOS,
about 27 MB) and listed there for `GOOS=js GOARCH=wasm`, with and without
`-tags=tailserve_test`. Before any go command runs in the copy, its `go.mod`
must name `module tailscale.com` and its tree digest (sorted path, size,
sha256) must equal that of the generation's `tailscale.com@v<version>`, which
the hub source's `go mod verify` covers first. A copy that differs or cannot be
read is moved aside, removed and copied again; one that still differs leaves the
check on per-home modules. `cache-report.json` records the copy as `made`,
`verified` or `replaced`, with `copyCause`. A script line that looks like a copy
into the wasm package (a `cp` naming `wasm/`, or any line naming
`cmd/tsconnect/wasm/<file>`) but is not of that exact shape is never skipped:
the cause is `build-wasm.sh line <n> is a copy line the runner cannot read:
<line>`. Comment lines are ignored. Every
run validates the generation `current.json` names, with `GOPROXY=off`:
`go list -deps -test ./...` for completeness and `go mod verify` for integrity,
per source. All pass: the run is `warm` and has written nothing. A missing
module: the runner (never a check) adds it with the network, by
`go list -m all` and `go list -deps -test ./...` with `-mod=readonly` (so
`go.sum` never changes), and validates again; Go's own module-cache locks make
this safe beside a reader. A failed `go mod verify`, a changed toolchain or an
unreadable pointer: the runner seeds a new `gen-<random>`, validates it and
renames `current.json` to it. A pointer whose `dir` is not a single
`gen-<16 hex>` directly inside `mod/` is refused. Seeding or validation that
still fails makes the run private, and so does preparation (validation and
seeding) that runs past ten minutes: it runs under the host lock and outside
every check timeout, so a stalled proxy cannot hold a slot. One generation
serves every go.sum: modules
are stored by path and version, so it grows only by new versions.

**Toolchain.** A host Go at least as new as `hub/go.mod` (its `toolchain` line
if newer) is used as it is. An older host gets the toolchain from the module
generation: the runner downloads it once with `GOTOOLCHAIN=<version>`, puts
its `bin` first on every check's `PATH`, and stamps its tree digest (sorted
path, size, sha256); a tree that later differs from any stamp of the
generation, whatever go.sum that stamp was written for, is corruption.
Every check runs with `GOTOOLCHAIN=local`, so no check downloads or switches
toolchains.

**What checks get.** `GOMODCACHE=<generation>`, `GOCACHE=<build dir>`,
`GOPROXY=off`, `GOFLAGS=-count=1 -mod=readonly`, `GOTOOLCHAIN=local`,
`TAILTERM_VERIFICATION_CACHE=<root>`, under each check's own recorded
environment, so check records, argv, timeouts and `verification/matrix.json`
are unchanged. `HOME`, `TMPDIR`, `TMUX_TMPDIR` and `GOPATH` stay in the home.
`wasm-test-build` reads the same generation with the proxy off. Only when the
script cannot be read that way (no version or copy lines, or a version
`hub/go.mod` does not require) or the wasm seed fails without corruption does
that check alone keep a per-home module cache with the network (about 0.5 GB,
18,000 files), and the line names the cause.

**Tailscale tarball.** When `TAILTERM_VERIFICATION_CACHE` is absolute and
`GOCACHE` and `GOMODCACHE` are both set, as the runner sets them,
`scripts/build-wasm.sh` uses those Go caches, not the checkout's `.build/go` and
`.build/cache`, and keeps the release tarball at
`<root>/downloads/<sha256>.tar.gz`, so a warm run downloads nothing. It
extracts only a private copy that passes the sha256 check: a wrong or partial
cached file is removed and downloaded again. A download goes to a temporary
name, is checked and is renamed into place; one that fails the check exits
non-zero and leaves nothing. Extraction goes to a temporary directory that
becomes `.build/tailscale-<version>` only when complete. A plain local build
(neither variable set that way, as in the Docker static build) keeps
`.build/go`, `.build/cache` and `.build/tailscale.tar.gz`. The script prints
`build-wasm: GOCACHE=<dir> GOMODCACHE=<dir> tarball=<path> (cached|downloaded|extracted-present)`
to stderr.

**No test result from the cache.** `GOFLAGS=-count=1` makes every go test the
checks start execute, including the go-race command's plain and sharded
`go test` calls (the shards also pass `-count=1`). After a go-test, go-race or
migration-rehearsal check, a result line marked `(cached)` in its log, plain or
inside `-json` output, throws `Go test result came from the build cache: <id>`
and leaves no receipt. `go vet` may reuse cached analysis, which is not a test
result.

**Build cache.** Before its first Go command the run scans its `GOCACHE`. Go
names a stored output by the sha256 of its bytes and only checks its size when
reading it, so a same-size flip would be used. Every `<2hex>/<id>-d` file whose
sha256 is not `<id>` is removed, an `<id>-d/` executable directory with such a
file is removed whole, and a malformed `-a` index entry is removed; Go then
rebuilds. A mismatch is removed only once its size and mtime stay put (a peer
writes outputs in place, last byte last). A directory with a missing or invalid
marker, or holding anything Go would not write there, is skipped for the next
free `.2`, `.3` suffix. A build directory that is a symlink or whose real path
is not directly in `<root>/build` is refused, as a module pointer is. Limit: bytes that go bad after the scan in the same run
are not caught; no test verdict is read from the cache either way.

**Concurrency.** Two runs on the host's two slots share the root. Two runs on an
empty root may each seed a generation; the last rename wins and both are
complete. A run reads `current.json` once and keeps that generation to its end.

**No pruning by the runner.** The runner never removes or renames a
generation or a build directory, so nothing a live run uses can disappear. A
generation holds the union of module versions ever seeded (about 0.5 GB for the
hubs, 0.29 GB per toolchain); a new one appears only after detected
corruption. The line names the count when there is more than one generation.
Manual cleanup of generations, with no matrix run live: `chmod -R u+w` and
remove any `gen-*` (and its `.stamps`) that `current.json` does not name, or
the whole root.

**Build cache trim by the relay** (wi_dad4e23eb9f43ae1). Go's own five-day
trim let `build/go1.26.6-darwin-arm64` reach 50 GB by 10-17 and the free-disk
floor refused every queue launch. On each scheduled sweep pass (the
`worktreeSweepInterval`, default 1h), the relay unlinks build entries untouched
for longer than `verificationCacheTrimMaxAge` (default `24h`, floor `2h`).
Go refreshes a used entry's mtime once it is an hour old, so anything a run used
in the last day stays. The trim is on by default because the root is the
harness's own. Set `"verificationCacheTrim": "off"` in
`~/.config/tailterm/relay.json` to stop it; a value other than `on` or `off`,
or a relay.json that cannot be read, skips the pass. It is separate from
`goBuildCacheTrim`, which covers the user's own `GOCACHE`.

- **Root.** Resolved as the runner resolves it, with the same overlap
  refusals. It must carry a valid root marker; the relay has no checkout, so
  the inside-the-checkout rule does not apply. A refused, unmarked or absent
  root is left alone.
- **Build directories.** Only directories directly in `<root>/build` that are
  not symlinks, carry a valid `build` marker, hold only what Go and the runner
  write there, and have Go's README. Others are left alone and named in the
  line.
- **Entries.** The rules of the user trim apply: only old regular `-a` and
  `-d` files directly inside the two-hex directories. The README, `trim.txt`,
  `testexpire.txt`, the markers, `mod/`, `speech-model/` and `downloads/` are
  never touched.
- **Lock rule.** Any holder or waiter in the matrix host lock skips the pass
  (reason `matrix-lock`), and so does a refused lock path or a lock file that
  cannot be read or trusted. The lock is read again before each two-hex
  directory, so a run that registers mid-pass stops the walk at the next
  one. In the directory already being trimmed, only entries untouched for a
  day go, and Go takes a missing entry as a miss and rebuilds it.
- **Receipt.** Each attempt writes one line to the sweep journal (`tt team
  queue sweep-worktrees --journal`) with source `go-cache-trim`, the root in
  `lowestVolume`, and either the entries removed and bytes freed or the skip
  reason. The text form prints it as `verification-cache-trim root=<root>`.
  The low-space notice carries the last trimmed run.

**Speech model.** `scripts/package-speech-model.mjs` reads each file from
`<root>/speech-model/`, then from the checkout's own `.build/speech-source/`
(copying a good one up), and only then downloads it, writing by temporary file
and rename. Every source passes the manifest's size and sha256 check, and a
cached file that fails it is fetched again. A failed download throws one error
naming the file and its cause after a cache miss (for example
`HTTP 429 Too Many Requests`). It prints
`Speech model: files=N shared=N checkout=N downloaded=N bytes=B cache=<dir>`.
The release runner's own static build and a developer's `npm run build:static`
use the same default root.

**Report.** One line per run, through the runner's printer:
`matrix run: go caches: modules <gen> (warm|seeded N modules|rebuilt: <cause>), toolchain <version> (host|cached; host <version> is older than go.mod), build <dir> scanned <n> outputs in <ms> ms, removed <k>`,
followed by `, wasm-test-build: shared modules` or `, wasm-test-build: per-home modules (<cause>)` when the plan has that check,
or `matrix run: go caches: private: <cause>`. `<output>/cache-report.json`
holds the same facts with the download counters.

**Why the sweep leaves it alone.** `tt team queue sweep-worktrees` reads queue
state and removes worktrees under `.build/worktrees` and run directories under
the artifacts root; its `neverTouchPaths()` refuses anything in or containing
`~/Library/Caches`. The default root is under neither sweep root and inside
`~/Library/Caches`, so the existing keep rules already treat it as
harness-owned and no keep-rule change is needed. Check it without queue state:
`realpath` of the root is inside `~/Library/Caches` and not inside any path
from `git worktree list --porcelain`, `~/projects/tailterm-artifacts` or
`/private/tmp`.

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
external log path and exact SHA-256. When the plan builds WASM and has a WASM
reader, the `wasm/tailserve.wasm` and `.build/test.wasm` prerequisite entries
record the built digest as `sha256`, the pre-run digest as `priorSha256`, and
`source: "wasm-test-build"`; a build that passes without writing one of them fails
with `wasm-test-build passed without writing <path>`. Each browser check and
`00-static-build` records in `wasm` the `{ path, sha256 }` of the two WASM files,
hashed just before it ran; a file it cannot read fails the run with
`Unreadable WASM before <id>: <path>`. Those digests must equal the build's
output, or, in a plan without the build, the prerequisite digests. The files are
hashed before each such reader and once more after all checks have finished. A
mismatch fails the run with no receipt:
`WASM changed after wasm-test-build: <path>`, or without the build
`WASM prerequisite changed during verification: <path>`. It runs every selected
check and retains failures. The approved matrix records a default ten-minute
per-check timeout, with explicit overrides in `checkTimeoutMs` (go-race: fifty
minutes; go-test: twenty minutes; npm-unit: eight minutes; the two static
checks: two minutes).
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

## An acceptance rests on the current receipt

Bug `wi_6ef7f8a5d4f160c4`, order #34700, policy answer #33145. A saved
acceptance cannot stand over a failed or superseded receipt:

- `owner-accept` resolving an `owner-decision` or a `follow-ups` disposition
  needs the candidate's passing current receipt wherever verification is
  required, as a lead `accept` does. The owner judges criteria and scope
  questions; there is no override of a failed receipt. A refused
  `owner-accept` leaves the disposition and its history unchanged.
- Saving a new verification plan clears a standing `accept` or `owner-accept`,
  because the plan supersedes the receipt the acceptance rested on. The
  disposition history keeps the cleared entry unchanged. The acceptance is
  recorded again on the new receipt: a lead `accept`, or a lead
  `owner-decision` followed by the owner's `owner-accept`. Until then, wherever
  verification is required, the done saves report that a passing receipt for
  the exact accepted candidate is required; a non-enrolled item whose scope
  names no verification criteria has no receipt gate, and its completion keeps
  the previous review/narrative requirements.
  `owner-decision` and `follow-ups` are not acceptances and stay;
  the `owner-accept` that resolves them reads the new receipt. A same-commit
  re-verification after acceptance, such as a retried matrix, therefore needs
  a fresh acceptance.
- The done saves and the queue gate are unchanged. They still refuse a failed
  current receipt, including under an acceptance row saved before this rule.

This fits the re-binding of an acceptance to a verified correction
(`wi_c7a7b97e72fc151b`): the cleared acceptance stays in the disposition
history, where that re-bind can read it and bind it to the correction's
passing receipt.

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
block these barriers. `wasm-test-build` is the first check in plan order, ahead of
the id-sorted rest: it rewrites the WASM that `00-static-build` packs and the
browser suites load, so as a barrier it holds every WASM reader until it ends.

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

go-test kept the ten-minute default until `hub/cmd/tt` outgrew it. On
2026-10-09 a go-test attempt from a cold build cache was killed at 600254 ms
with `hub/cmd/tt` still running, and the retry passed in 473660 ms with every
other package cached and `hub/cmd/tt` taking 467.4 s alone (#30899, #30901).
That left about 126 s of margin on a warm cache and none on a cold one, and
each team verifies in a fresh worktree. So `checkTimeoutMs` now gives go-test
1200000 ms (20 minutes): twice the killed cold attempt, rounded down to the
minute, which is 2.53 times the warm run and 726340 ms above it. The cold
total is not known, because that attempt never finished, and no new cold
timing was run beside other matrix work; if a receipt's go-test passes 15
minutes, measure it alone and raise the limit. `goTestFlags` is unchanged:
go-test already carries `-timeout=45m`, so go test's 600 s package default
cannot trip under the 20-minute check limit. This matrix change also needed a
new owner approval of the digest.

**npm-unit file budget.** npm-unit is `node --test tests/*.test.js`: the test
files run in parallel and the tests inside one file run one after another, so
the check lasts about as long as its slowest file. The rule: no test file may
take more than half of `checkTimeoutMs["npm-unit"]` in a passing npm-unit
attempt (240 s under the present 480000 ms), and a full npm-unit run alone on
the host finishes within that same half. When a file passes the half, split
it; do not raise the limit for it.

The two slowest files were split under this rule on 2026-10-12
(`wi_ae63ff4bc52842dc`), with no matrix change, because new `tests/*.test.js`
files are already selected by the `tests/` rule and the `npm test` glob.
`tests/release-runner.test.js` (475 s in a passing attempt beside the rest of
the suite, 315 s alone) is now that file plus `tests/release-runner-2.test.js`,
`tests/release-runner-3.test.js` and `tests/release-runner-4.test.js`, with
shared helpers in `tests/release-runner-fixtures.mjs`.
`tests/verification.test.js` (319 s in that attempt, 282 s alone) became that
file plus `tests/verification-2.test.js` and `tests/verification-3.test.js`,
with helpers in `tests/verification-fixtures.mjs`. On 2026-10-16
`wi_2b82df2dbac73bc0` split `tests/verification-2.test.js` again, moving its
shared cache and wasm build tests to `tests/verification-4.test.js` and its
preflight, host lock and budget warning tests to
`tests/verification-5.test.js`, so the verification tests are now those five
files. In that item's imported verification receipt at ab1fab4 (integrated
as 5f7edb5; generation 2, request `lead-bac73bc0-receipt-ab1fab4-r2`)
npm-unit passed in 209 s, run beside the matrix's browser checks. Tests that
Go comments name (`wake a1`, the batch id vector) stay in
`tests/release-runner.test.js`. A fixtures module is a `.mjs` file under
`tests/`, so the browser inventory reads it: it must not contain the text
`.launch(` or a `verify-matrix:` marker line. One test, `held s1`, takes about
50 s alone because its 41 cases each build a git repository and run two polls,
about 2,200 child processes; it waits on no timer.

**Budget warnings.** A run warns when a check passed close to its time limit,
so a check that is about to start timing out is seen before it does (npm-unit
ran at 477 s of its 480 s for days unnoticed). The rule: a check gets a warning
when its slowest passing attempt in this run (exit code 0) took strictly more
than 70 percent of the check's planned timeout, which is its `checkTimeoutMs`
entry or else `defaultTimeoutMs` (the planned `VERIFICATION_TIMEOUT_MS`).
Exactly 70 percent gives no warning. A failed or timed-out attempt never
counts, however slow, so a check with no passing attempt gives no warning.
Only this run's results are read: there is no history file and no comparison
with earlier runs.

When the run has written its receipt (`run` and `targeted` alike, failed checks
included) it writes `budget-warnings.json` beside it and prints one line per
warned check:

```text
matrix run: budget warning: npm-unit passed in 477000 ms, 99.38% of its 480000 ms timeout
```

```json
[
  { "check": "npm-unit", "durationMs": 477000, "timeoutMs": 480000, "percent": 99.38 }
]
```

A run with no warnings writes an empty list (`[]`) and prints no such line; a
run that writes no receipt (interrupted or refused) writes no list. The
warning is advisory and is not in the receipt: `receipt.json`, the receipt
schema and the hub's receipt import are unchanged, and a warning never changes
a check's status, the run's verdict or the exit code. A failure to write the
file is printed (`matrix run: could not write budget-warnings.json: ...`) and
changes nothing else. When a check warns, look at what made it slow and split
or speed it up; raising its timeout is a matrix change that needs the owner's
approval of the new digest. `budgetWarnings(receipt.checks)` in the runner
gives the same list for any saved receipt.

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

`hub/cmd/tt` followed (bug `wi_af64945097c67c68`, order #35357): unsharded it
took 1559 to 1828 s under race inside the other-packages command, and with the
store shards after it go-race passed at 2901 s of its 3000 s timeout on
2026-10-15 and timed out twice that day. It has 940 top-level tests, none
parallel, and a running `tt.test` uses about one core.

**The key.** `verification/matrix.json` carries
`"goRaceShards": { "./cmd/tt": 5, "./internal/store": 4 }`: one or more
packages, each a `./` path (not `./...`) with its own K, an integer from 2 to
8. An empty object, a non-object, or any bad entry is refused as
`Invalid matrix goRaceShards`. Changing any K, or adding or removing a
package, changes the matrix bytes and needs a new owner approval of the
digest, like any matrix change.

**The argv.** With the key, every go-race check has the same prefix, whatever
its packages: one `-shards=PKG=K` flag per package, in package order.

```
node ../scripts/verify-matrix.mjs go-race -timeout=45m -shards=./cmd/tt=5 -shards=./internal/store=4 PACKAGE...
```

The cwd stays `hub`, the check ID stays `go-race`, and the packages are
selected as before. The prefix is the same for a store change, an API-only
change and `./...` on purpose: the hub and the release runner cover an approved
go-race by an integrated one only when the argv before the first package is
equal. Without the key the argv is `go test -race …` as before. `go-test` and
every other check are unchanged.

**Locally.** To run the store race yourself, from `hub/`:
`node ../scripts/verify-matrix.mjs go-race -timeout=45m -shards=./internal/store=4 ./internal/store`,
and the tt race:
`node ../scripts/verify-matrix.mjs go-race -timeout=45m -shards=./cmd/tt=5 ./cmd/tt`.
The unsharded `go test -race -timeout 30m ./internal/store` no longer fits 30 minutes.

**What the command does.** It accepts only one `-timeout=Nm`, at most one
`-shards=PKG=K` per package (a second flag for a package is a usage error
whatever its K), and `./` packages.

- A list with no shard package and no `./...` runs one
  `go test -race -timeout=Nm PACKAGE...`. A Go change outside the shard
  packages behaves as before.
- Otherwise `./...` is expanded with `go list` and the shard packages are
  taken out. Then:
  1. The preflight, before any test runs: each shard package the list tests,
     in flag order, is listed and partitioned (the three checks below) and
     prints its start line. Any failure stops the check with nothing started.
  2. One pool runs the jobs in queue order, at most
     `min(jobs in the queue, VERIFICATION_JOBS)` at once: first the other
     packages, when there are any, as one
     `go test -race -p=1 -timeout=Nm OTHER...` (one race test process at a
     time), then each shard package's shards in flag order, each
     `go test -race -json -count=1 -timeout=Nm (-run|-skip) REGEX PKG`. A
     one-job run takes them in turn. `-count=1` keeps a cached result from
     standing in for a run. After any job fails no further job starts; the
     running ones finish, and the line `go-race: N jobs not started after a
     failure` says how many were left.

Two serial phases, the other packages and then the shards, put the slow
packages end to end: on 2026-10-15 cmd/tt took 1822 to 1826 s in the
other-packages command and the store shards were then cut at 1148 to 1149 s,
twice (#35433). In the pool the other packages run beside the shards.

Each job's whole output is printed in queue order once it and every earlier
job have ended: the other packages' job prints through, then
`go-race other packages: N packages, S s, exit C`; a shard prints its test
output as plain text followed by `go-race shard PKG I/K: N tests, S s, exit C`,
so the log keeps every `=== RUN` line and each shard's seconds. A failed job
fails the check (a failed other-packages job gives its own exit code, and the
shards it left unstarted are not reported missing), and a retry reruns the
whole check.

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

**The three checks.** Each package and its K are frozen in the plan's argv;
the partition is not, because its digest changes whenever any item adds,
removes or renames a test there, and an approved plan and its integrated plan
must keep the same argv prefix. The partition follows from the test names at
the plan's commit and K, and the run refuses a worktree that is not at that
commit. Three checks hold it, per shard package:

1. Plan creation reads the package's `_test.go` files from the commit with
   git, partitions the names and fails unless every name is in exactly one
   shard. It prints, on stderr, one line per package the plan's go-race
   tests, such as
   `go-race shards: ./internal/store K=4 tests=N partition sha256:DIGEST`.
2. The preflight lists the tests with `go test -race -list .` and fails,
   naming the difference, unless that list equals the names in the worktree's
   source. It prints the same line, with the same digest for the same commit.
3. After the pool ends, the run fails unless every listed test started
   exactly once across the package's shards (top-level `run` events of
   `go test -json`) and no unlisted top-level test ran. It prints, per
   package whose shards all ran,
   `go-race shards: ./internal/store K=4 tests=N ran-once=N partition sha256:DIGEST peak-shard-processes=P`,
   where P is the pool's peak count of shard processes (the other packages'
   job is not counted).

`DIGEST` is the SHA-256 of the canonical JSON of the K sorted name lists. The
names are every top-level function in the package directory's `_test.go` files
matching `^func ((?:Test|Fuzz|Example)[A-Z_0-9]\w*|Test|Example)\(`, except
`TestMain`. The rule reads no build tags. A test behind a build tag, or an
`Example` with no `// Output:` comment (Go compiles it and never runs it), makes
check 2 stop the run; fix the test file or the rule, never rely on the
catch-all.

**Slot weight.** A go-race that will shard (its packages include a shard
package or `./...`) holds `max(2, ceil(min(1 + sum of K, jobs) × 1 GiB / 3 GiB))`
job slots, capped at the job count: the pool is the other packages' job plus
every shard, at most the job count at once, so on the Mini's 5 jobs it is 2
and at 8 jobs with cmd/tt and the store it is 3. 1 GiB per race process is the
admission budget, above the 0.63 GB largest store race test process and the
0.9 GB largest process of the `go test` tree that were measured; 3 GiB is what
the default job count assumes per slot. A go-race that will not shard keeps
weight 2. The tree never holds more race test processes than the pool size:
the other packages' job is one, because of `-p=1`. The Go lane lock is
unchanged.

**Timing.** A four-shard prototype of the same rule beside another matrix took
577 s (shards 378, 468, 577 and 528 s), each test run exactly once; loaded full
matrices on 2026-10-15 put the slowest store shard at 1025 to 1149 s. cmd/tt at
K=5 splits its 940 tests 188, 183, 191, 184 and 194 (commit `ca69981`); at the
worst measured loaded rate (1826 s unsharded) the largest shard is about 377 s
of test time. The other packages' job (about 525 s of `internal/server` and
100 s of small packages, one at a time) runs beside the shards, so it is not
on the critical path. A store-only or tt-only change pays only its own shards.

**Changing K.** Edit any package's number in `goRaceShards`, or add or remove
a package; each changes the matrix bytes, so get the owner's approval of the
new matrix digest and release it like any matrix change. A go-race approved
under the previous matrix is then covered by the integrated one only as an
approved matrix change, while it still tests every approved package (see
"Integrated plans keep accepted checks"); another K or another package set
under one matrix is another argv prefix and is refused. If a receipt's store
shards pass 12 minutes as the suite grows, store K=5 is the next step: its
longest shard is 385 s of the measured test time against 518 s for K=4. Keep
the sum of K in mind: the pool runs at most the job count at once, so on a
5-job host more shards queue rather than run together.

The hub's coverage rule (`goRaceCovers`, `matrixChangeCovers`) and the release
runner's copies read "prefix, then only `./` packages" and needed no change.
Tests: `tests/verification.test.js` (the shard rule, plan creation, argv, slot
weight, and the command on a real Go module), `tests/verification-2.test.js`
("goRaceShards accepts several packages, each with its own K", "go-race
command shards two packages in one pool after listing both" and "plan
creation partitions each shard package and prints one line per package") and
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

**An isolated HOME.** Bug `wi_28a18cbc08d3d598`, order #35681. Without
`TAILTERM_MATRIX_HOST_LOCK` the default path is under the account's own home
from the user database (`os.userInfo().homedir`), not `HOME`. When `HOME` is
set and is not that directory (an `env -i HOME=/tmp/…` run, or a matrix check,
whose home is private and whose environment drops the override), `exec`,
`status`, `run` and `targeted` refuse before joining the list, with a message
naming `TAILTERM_MATRIX_HOST_LOCK` and the shared path; `exec` and `status`
exit 78 and `exec` writes no sidecar. They neither take a private lock beside
the real holders nor fall back to the shared one: a run nested in a matrix
check would then wait on the lock its own matrix holds. A run that means the
shared lock sets the override to that path; a test sets it to a temporary one.
An explicit absolute override works as before, whatever `HOME` is.

The Go readers of the file resolve the default the same way since 954f41a
(bug `wi_ee29358d0c7791ab`): the relay's matrix wait
([agent-activity.md](agent-activity.md)), the scheduled sweep and the
`tt team queue list` waitlist. Under another `HOME`, or when the account home
cannot be read or is not absolute, there is no path, only a reason that names
`TAILTERM_MATRIX_HOST_LOCK`; none of them reads the file under `HOME`. The
sweep skips its run as `matrix-lock` with that reason on its journal line
(7d7e7e7, bug `wi_b52092ee3adcf6be`), so a private, absent file is never taken
for a free host. The relay's matrix wait is an unknown read. The waitlist
prints the reason ("The queue list" below). An absolute override still wins in
all three. A relative one is refused, and the default is not read instead.

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
  high reading can never leave the host idle. A full host, a later waiter
  (except a backfill behind an exclusive head) and a one-slot (`TAILTERM_MATRIX_MAX_HOLDERS=1` or v1) cohort take no reading
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
head. The waiter directly behind an exclusive head is judged as a head would
be, since it may backfill a free slot (see "Exclusive holds"). Holders carry
none. `status` appends `; held: memory` or `; held: slots`
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
    "polls": 12, "polledAt": "2026-10-03T22:59:58.000Z",
    "worktree": "/abs/worktree-a", "output": "/abs/logs-a"
  }],
  "waiters": [{
    "id": "lease-b", "seq": 41, "pid": 4300, "kind": "exec",
    "item": "wi_measure", "agent": "builder",
    "priority": "normal", "prioritySource": "default",
    "exclusive": true, "exclusiveCapMs": 900000,
    "requestedAt": "2026-10-03T23:00:30.000Z", "runTimeoutMs": 900000,
    "grantSeqAtRequest": 37, "overtakenBy": 0,
    "nonUrgentOvertakes": 0, "overtakeLimit": 2,
    "heldReason": "slots",
    "polls": 15, "polledAt": "2026-10-03T23:01:00.000Z",
    "idleSlotsSince": "2026-10-03T23:00:30.000Z", "idleSlots": 1,
    "worktree": "/abs/worktree-b", "output": "/abs/logs-b"
  }]
}
```

`release` (only ever `true` on the deployer's run) and `heldReason` (`slots` or
`memory`, waiters only) are optional: files and clients without them stay
valid, and any other value fails closed like other malformed state. So are the
waiter keys of bug `wi_ad0818fa3ea0287d` (order #36413): `polls`, a count each
waiter raises on every poll, `polledAt`, the time of that poll, and an
exclusive head's open idle-slot period, `idleSlotsSince` and `idleSlots` (see
"Exclusive holds"). They are optional keys with no new value for `version`,
`kind`, `priority` or `heldReason`, so a client older than them reads the file
as before and keeps them on entries it rewrites. A count or slot number that is
not a whole number in range, or a time that does not parse, fails closed. The
grant removes all four from the new holder entry; a reader still accepts them on
a holder, as on `lease-a` above.

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
fill a spare slot with a later waiter; the one exception is the backfill behind
an exclusive head (see "Exclusive holds"), and never into a worktree in use. Duplicate output/record roots are refused
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
the list (or second, behind an exclusive head, under the backfill rule in
"Exclusive holds"), a slot is free, no holder uses its canonical worktree and,
beside a running holder, the memory reading is not over the limit. A dead
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
preparatory test-binary build is stopped with its process group, as a check is.

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

**A waiter that is gone or stops polling.** A waiter whose process is gone is
dropped at once (`waiter-dropped` with `reason: "pid-gone"`). Bug
`wi_ad0818fa3ea0287d`, order #36413: a waiter whose process is alive but no
longer polls (it was suspended, or it was killed and its pid now belongs to
another process) is dropped too, after five minutes (`WAITER_STALE_MS`),
about 150 missed polls at the 2-second interval. Every waiter raises its own
`polls` count on each poll, and each waiter watches the others' counts. One
whose count has not changed for five minutes of another waiter's continuous
watching is dropped, journaled as `waiter-dropped` with `reason: "stale"`,
`polls`, `polledAt` and `observedMs` (how long it was watched unchanged), and
`by` naming the run that dropped it. A watcher whose own polls stopped for
more than half the bound (it was paused itself, or the whole host slept)
starts its watch again, so a pause never makes the others look stale. A live
waiter polls every two seconds, so it is never dropped. An entry without
`polls` comes from a client older than this rule; it is never judged stale and
blocks the list as before until that client is gone. The rule reads the file
only, like the rest of the lock: it does not compare process start times. A
dropped waiter that resumes finds its request gone and fails with `Host lock
request ID is no longer in FILE: another run dropped it (see
waiter-dropped in JOURNAL), as it does a waiter that stops polling`, with no
receipt. A drop removes the entry rather
than skipping it, so a reused pid never stays in the list or counts in other
runs' queue position and length.

The journal events are `request`, `acquire`, `release`, `withdrawn`,
`wait-expired`, `run-timeout-abort`, `stale-recovered`, `overlap` (one per
holder taken over), `waiter-dropped`, `mutex-recovered`, `guard-stale`,
`corrupt-file`, `holder-update-failed`, `memory-hold`, `memory-unreadable`,
`exclusive-overlap` and `idle-slots` (see "Exclusive holds" below).
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
- The same guard with no readable owner (empty or not JSON, as a crash between
  creating it and writing its owner leaves it on a filesystem without hard
  links) that is more than 60 seconds old: requests refuse with that file named
  and its age (exit 78, `guard-stale` with `guardPid: null`, reason
  `unreadable-owner` and `ageMs`). Bug `wi_6be7e47bd2358699` (x7, merged from
  `wi_5d6e865ee7638fc2`); before it the guard silently blocked every recovery.
  A younger one is treated as a create in progress. It is never removed
  automatically, because two processes that both judged it stale could each
  replace it. Confirm no verification is running, then remove that file.
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
| `VERIFICATION_HOST_QUEUE_POSITION` | position when queued; `0` only when acquired at once on an empty host; a run acquired at once beside a live holder records `1` |
| `VERIFICATION_HOST_QUEUE_LENGTH` | waitlist length when queued, this run included; `0` only when acquired at once on an empty host |
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
and withdrawn runs, so a run that joined the list leaves `host-lock.json` and
`host-lock.log` in its output directory. A request refused before it joins (an
invalid setting, an isolated `HOME`, an output directory already in use)
writes neither. The hub does not import these keys or the sidecar in this
phase; they are data for deciding later whether small runs can share the host.

**Supplemental runs.** Run a race suite or any other "run alone" command
through the wrapper so it joins the same list:

```sh
node scripts/verify-matrix-host-lock.mjs exec --item ITEM --timeout-minutes N --record /absolute/dir \
  [--priority urgent|high|normal] [--agent NAME] [--host-wait-minutes N] [--exclusive] -- COMMAND [ARGS…]
```

It prints the same waiting lines, runs the command in its own process group
(recorded in `groups`), forwards SIGINT and SIGTERM to that group, stops the
command at `--timeout-minutes` or the holder cap, whichever is sooner (exit 124), and otherwise exits with the
command's own code. It writes `host-lock.json` (check set `supplemental`, plus
the argv and exit code) and `host-lock.log` to the record directory. The
sidecar's `environment` object keeps the values that change what the run means,
for the names that are set: `GOMAXPROCS`, `GOFLAGS`, `GOGC`, `GOMEMLIMIT`,
`GODEBUG`, `GOEXPERIMENT` and `CGO_ENABLED` (`{}` when none is), so an order's
`GOMAXPROCS=2` can be checked from the record. No other variable is recorded,
so neither are secrets (bug `wi_28a18cbc08d3d598`).

**Exclusive holds.** Bug `wi_6be7e47bd2358699`, order #36061. A normal holder
shares the host: a second run, a release run included, can start beside it at
any time. `exec --exclusive` holds the host alone. Use it only for checks whose
result depends on a quiet host: a live-session check, or a timing-sensitive
test. Keep the command short with a small `--timeout-minutes`, because every
team run and the deployer wait while it holds, and give it a short
`--host-wait-minutes`, because it waits for every holder to leave. `run` and
`targeted` have no exclusive mode.

- *Admission.* An exclusive request is granted only when no holder is live. It
  never takes over an overdue holder, which may still be running; it waits until
  that holder ends or its own wait bound. While it holds, it fills every slot,
  and no other request is granted beside it, at any priority.
- *The cap.* After `TAILTERM_MATRIX_EXCLUSIVE_CAP_MINUTES` (whole minutes, 1 to
  1440; unset or empty means 15) from its start, a waiting release run may join
  it, under the slot count and the memory rule as beside any holder (so with two
  slots, exactly one release run). Nothing else joins, before or after the cap.
  Each exclusive request freezes its cap on its entry (`exclusiveCapMs`), so a
  waiting release reads the holder's cap, not its own environment. Only
  exclusive requests read the variable; an invalid value refuses before the run
  joins the list or writes a sidecar, as does an exclusive request marked as
  a release run. The cap is not the hold's bound: the hold still ends at its own
  `--timeout-minutes`, clamped by the holder cap, and an overdue exclusive
  holder is taken over (with every slot it fills) as a recorded `overlap`, like
  any holder.
- *Waiting.* The holders drain and the exclusive head is granted when the last
  one leaves. Only the requests the order above puts ahead of it are granted
  first: a waiting release run, an urgent run, and a high run until its
  fairness budget is spent. It keeps its place and its `heldReason` is `slots`,
  as is that of anyone blocked by an exclusive holder.
- *Backfill.* Bug `wi_ad0818fa3ea0287d`, order #36413. While an exclusive
  request waits at the head, the waiter directly behind it (position 2 only)
  may take a free slot, but only when its own run timeout ends no later than
  the latest run timeout of the live holders. Every holder stops itself at its
  run timeout and an exclusive head takes nobody over, so that deadline is the
  latest the exclusive request can start, with or without the backfill: the
  backfill never moves it. Its actual start can still come later than it
  would have, when the holders finish early and the backfill is still running.
  There is no allowance for the 120 seconds of grace, which would let a backfill
  run past holders that stopped on time. The backfill must not be exclusive
  itself, needs at least one live holder, a free slot, a worktree no holder
  uses, and, beside a holder, a memory reading within the limit. The exclusive
  request keeps first place; the grant counts in its `overtakenBy` but, like a
  release grant, spends none of its fairness budget. Any other older waiter is
  charged for it as for any later grant. A waiting release run is
  always ordered ahead of an exclusive head, so no backfill happens while one
  waits, and one that arrives later is granted at the next free slot as
  before. Because every matrix run is clamped to the same holder cap, a later
  matrix run almost never ends before a running one, so a backfill is mostly a
  short `exec` run. The `acquire` journal line and the sidecar carry `backfill`
  (`behind`, the exclusive request's id; `endsBy`, the backfill's own deadline;
  `holdersEndBy`, the live holders' latest one). A waiter refused only by the
  deadline adds `; behind an exclusive request, no backfill: run timeout N ms
  ends after the holders' TIME` to its waiting line, and its `heldReason` stays
  `slots`.
- *Idle slots.* While it waits at the head, an exclusive request records the
  slots free by count (the memory reading aside) on its own entry as
  `idleSlots` since `idleSlotsSince`. When the count changes, when it is
  granted, leaves, or is no longer the head, it journals `idle-slots` with
  `start`, `end`, `ms`, `slots` and `reason`: `backfill-or-holder` (a backfill
  or another holder took the free slots), `slots-changed` (a different number
  of slots is free, and a new period starts), `granted`, `withdrawn`,
  `wait-expired` or `not-head`. A run that drops it (gone or no longer polling)
  journals its open period with `reason: "dropped"`. Times are the head's own
  polls, so within one poll. These lines measure how much slot time an
  exclusive request leaves idle.
- *Slot fillers.* While an exclusive holder holds, `holders` also lists one
  filler per slot no other holder uses: a copy of the holder (pid, groups,
  worktree, start and run timeout) with the id `ID:slot-N`, without its
  exclusive fields, and with `fillerOf` naming it. They make the host look full
  to a client older than this mode, such as a team branch's matrix run. They are
  rebuilt on every write, follow the holder's check groups and leave with it; a
  filler naming no current holder is dropped. A dead exclusive holder is one
  `stale-recovered`. A reader counting runs counts entries without `fillerOf`.
  With one slot (`TAILTERM_MATRIX_MAX_HOLDERS=1` or a v1 file) the exclusive
  holder is simply the holder, and no release can join it.
- *Records.* The entry carries `exclusive: true` and `exclusiveCapMs`; `request`
  and `acquire` journal lines carry `exclusive: true`. A release run that joins
  after the cap appends `{ at, id, item, agent, pid, kind }` to the holder's
  `overlapsAfterCap`, journals `exclusive-overlap` (`kind`, `holder`,
  `holderItem`, `holderAgent`, `holderPid`, `capAt`), and records
  `joinedExclusive` in its own sidecar. When an exclusive holder is taken over,
  the `overlap` line that names it carries its `overlapsAfterCap`, and at its
  release the holder reads that line from the journal (skipping any line that
  does not parse), so its sidecar keeps the release runs that joined it; if the
  journal cannot be read it prints a warning and records `[]`. The exclusive run's sidecar has `exclusive`, `exclusiveCapMs`,
  `overlapsAfterCap` and `alone`: `true` only when at release its entry was
  still on file, nobody had joined and no other holder was present; `false`
  otherwise (a takeover included); `null` when the release could not update the
  file. A shared hold prints `matrix host: WARNING exclusive hold shared after
  its cap with ITEM/AGENT/pid N from TIME`. The receipt keys do not change.
  Never claim a run was alone because it started on an empty lock; read
  `alone`.
- *Status.* `status` prints the exclusive holder once, tagged `exclusive, fills
  N of M slots, release runs may join after TIME`, then `; shared after cap
  with ITEM/AGENT/pid N at TIME` for each release that joined; an exclusive
  waiter is tagged `exclusive`, and an exclusive head with free slots adds `;
  idle slots N since TIME`. A waiter line shows `; polled TIME` once that waiter
  has polled.
  `status --json` is the file as written, fillers included. An exclusive head's waiting line ends `; exclusive, waiting for
  every holder to leave`; a head blocked by an exclusive holder adds `; holder
  is exclusive`, and a release head before the cap ` until TIME`.

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
has not failed. An entry on another host gets no line.

A run that names no agent (empty or `unknown`) and no valid `wi_` item, such
as a bare `exec` without `--item`, belongs to no entry. After the entries, the
list prints one unattributed line for each such holder and waiter whose
process is alive, holders first, then waiters in the file's order:

```text
matrix unattributed holder pid=4250 kind=exec priority=normal since=2026-10-01T14:02:00.000Z
matrix unattributed waiter position=1 of 3 pid=4251 kind=exec priority=high since=2026-10-01T14:06:00.000Z
```

`since` is the holder's `startedAt` or the waiter's `requestedAt`. A run with a
valid item, even with no agent, keeps only its entry's `waiting-for-matrix`
line, so no run is printed twice. This is the relay's rule for agentless runs
(wi_6128966a8025aaad): such a run is not counted as any member's activity, so
this line is where it shows. The unattributed lines appear only when the
file's host is this machine and the queue has entries.

The file is read from `TAILTERM_MATRIX_HOST_LOCK`, else the default path. The
list always prints and exits 0, and `--json` is unchanged. Five cases show no
waitlist:

- No file: nothing extra is printed (a free host).
- `TAILTERM_MATRIX_HOST_LOCK` is set but relative: `matrix host:
  TAILTERM_MATRIX_HOST_LOCK must be an absolute path; waitlist not shown`. The
  default path is not read instead.
- The override is unset and the default is refused ("An isolated HOME"
  above): `matrix host: REASON; waitlist not shown`, where the reason names
  `HOME` and the account home, or says the account home cannot be read.
- The file is unreadable, not JSON or not version 1 or 2: `matrix host: lock
  file PATH unusable (REASON); waitlist not shown`.
- The file's `host` is another machine: `matrix host: lock file PATH belongs to
  host X; waitlist not shown`.

The lock file lives on the machine that runs the matrix, so the list shows the
waitlist only when it is run there. The hub's idle-entry stall check does not
read the file; it reads each member's reported activity, and a member whose
`matrixWait` holds or waits for the host lock is not idle until 4 hours 30
minutes after the earlier of that wait's `since` and the report's
`observedAt`; a retired member's saved wait does not count
(`wi_772e88d71e5cd0e5`, `wi_b4bdb09aa82c5264`;
[agent-activity.md](agent-activity.md)).

**When your run ends.** Feature `wi_a598ffa55a326699`, orders #33442 and
#33457. The agent that started a run is told when it ends: within about a
minute of the run leaving the host lock (finished, failed, withdrawn or
killed), the hub sends that agent a directed notice with the subject
`Your matrix run has ended`, which wakes it like any directed message. A
watcher shell and a message to yourself are not needed. A background shell can
be killed under memory pressure, and the hub refuses a self-addressed message
with `a directed message to yourself wakes nobody; the hub sends you a notice
when your matrix run ends, and to wait for anything else run tt inbox --unread
--wait 9m in your turn or ask a teammate to message you`. The notice names the
item and the run's start time and carries no path and no result: read
`receipt.json` (`targeted-receipt.json` for a targeted run) in the log
directory you gave the run, and with no receipt read `runner.json` and
`host-lock.log` there and record why the run stopped before anything else. It
never authorises another run. It needs the relay on the host to be running
and the lock entry to belong to the agent: an entry naming the agent
(`TAILTERM_AGENT_NAME`) or the agent's ID is that agent's, and an entry whose
agent is empty or `unknown` and whose item is a valid `wi_` item is the run of
every agent bound to that item, never of an unbound agent or one on another
item (wi_6128966a8025aaad); who is told when such a run ends is in
[stall-watch.md](stall-watch.md), "Who gets it". A run that belongs to no
agent gets no notice: for example one that names neither an agent nor a valid
item, one that names an agent whose bound item is not the entry's item, or an
agentless one whose valid item has no live bound agent. Of these, only a run
with neither an agent nor a valid item is printed as an unattributed line by
the queue list above (wi_d6d7da393cea9c08, landed as 1d9b2828). A run that
joins, runs and ends wholly inside one 15-second check of the relay gets no
notice, and an unknown read of the lock file ends no wait: the relay repeats
the last confirmed wait the hub holds, up to the 4 hours 30 minutes bound.
Details and limits: [stall-watch.md](stall-watch.md), "When a matrix run
ends".

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
between a check or a preparatory test-binary build starting and its group
reaching the file.

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

- Each real runner invocation creates a fresh disposable HOME, TMPDIR and
  GOPATH (GOMODCACHE and GOCACHE too when its plan does not share the
  verification caches), plus a private TMUX_TMPDIR directory beneath its home.
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
