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

The Go rules come from receipt data. go-race is the longest check: 625 s alone,
while no other single attempt exceeded 171 s. Its `hub/internal/store` package
takes 570–573 s against go test's 600 s package timeout. With four browser lanes
beside it, that package timed out and the retried Go lane set a 23.4 min wall.
So go-race starts at once and holds two job slots, which leaves three for other
checks while it runs. That is still enough to finish the roughly 19 minutes of
browser work inside go-race's window. Non-Go check process groups also run at
`nice` 10, so the CPU favors the Go lane when it is saturated.

The serialization rules live in runner code and derive from the plan, so
`verification/matrix.json` and its approval are unchanged. Attempts stay
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
contain, with "candidate is not a fast-forward of its base; rebase onto the
current tip". Equal and linear commits are accepted. At acceptance,
`tt team queue accept` and the leased handler's done save read the item's current
verification plan. `--commit` must equal the plan commit, and the accepted
worktree must be a fast-forward of the plan base. That base is saved as the
acceptance base, so a candidate built on an older base is refused with "not a
fast-forward of the recorded base". Items without a plan keep the queue entry's
base. The team rebases onto the current tasks-hub tip before the handler freezes
the final plan, so the verified commit is exactly what merges.

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
