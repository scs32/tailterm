# Phase-successor and admission package candidate

This is a package-creation and qualification candidate for Feature
`wi_618c8ff87e6b8061` revision 5, immutable binding order **#3699** and
governing packaging order **#5775**. Handler instruction **#5785** is enrolled
as native delivery `dly_f233954c4607d46d`, generation 5 / execution epoch 1,
for exact agent `agt_ac3ad2691a275f59`, run
`run_43657fe460aec447`, and context digest
`3832346f3517a146c69b717a0faa629323316c0e423a5a45d947d7251b3c9354`.
The direct acknowledgment receipt is `drr_50cf7b9e42902d17`; first concrete
packaging progress is `drr_4e906358522b594d`.

The source is exact commit
`5793de112e1b4c0a08d55fba17a7736e0bf2e0bf`, tree
`43b026c59c40dcc2eb3dae75b397340007ff3225`, from a clean detached private
clone. Its installed accepted baseline is mandatory-v3 commit
`fae9f7ff34627449f716ef6439fe1eed5ab8ead5`, released under order #5669.
Historical `19970cea` remains provenance for an earlier rollback, not the
current comparison baseline.

The candidate composes two separately accepted inputs:

- Phase successor Bug `wi_82b4346d30ddf3f5` revision 1 / order #4753,
  source integration #5592 and acceptance #5594. The retained upstream
  integration manifest is `upstream-phase-successor-integration-manifest.json`,
  6,543 bytes, SHA-256
  `e6a0905e275f18488e4c4191d3b6323201653ddb737b14530066db78baa98970`.
- Admission/no-op Bug `wi_9c8d5eadb13c75fa` revision 1 / order #5041,
  integration #5624, independent QA #5654, handler acceptance #5655 and lead
  acceptance #5777. Its retained upstream integration manifest is
  `upstream-admission-integration-manifest.json`, 5,138 bytes, SHA-256
  `b48a5b04b39064cebfc47758b980979ae747888bacc51cd7ce246f66e10f7bca`.

Those manifests are upstream source/acceptance evidence only. They are not this
new package's independent QA or release acceptance.

The accepted mandatory-v3 package at
`/tmp/mandatory-v3-package-successor2.xR3pGU/package` and its repository
receipt/acceptance graph under `docs/releases/mandatory-v3-fae9f7f` were not
modified, replaced or relabeled.

## Artifacts and reproducibility

- `source-5793de1.tar`: complete repository archive, 11,550,720 bytes,
  SHA-256 `28b6db51774ed56af5da2959647108031406dc526bcb8c972c5a09cf51c44ba0`.
  A separately generated second archive is byte-identical; it contains 660
  entries.
- `tailterm-hub-linux-amd64`: static stripped Linux amd64 executable,
  27,873,406 bytes, SHA-256
  `1d9e97679b24428a9784430e3c96d908debc3bac71c869ac1e7f4b73a232d232`.
- `tt-darwin-arm64`: stripped Darwin arm64 executable, 6,919,234 bytes,
  SHA-256
  `2c23f9975462afb0bb587d7e213ae6272cae6062e6cf9a09bfba451133b1b378`.
- Both binaries were built twice with Go 1.26.6, `CGO_ENABLED=0`, exact target
  OS/architecture, `-buildvcs=true -trimpath -ldflags='-s -w -buildid='`.
  Each second build is byte-identical. `go-version-m.txt` records exact VCS
  revision `5793de1` and `vcs.modified=false` for both.
- `source-changes.txt` and `source-file-sha256.txt` retain all 21 paths changed
  from installed `fae9f7f`; `source-lineage.txt` retains all 11 intervening
  commits. A clean archive extraction passed all 21 changed-file pins.
- Package copies of `truenas_release_preflight.py` and
  `deploy-truenas-hub.py` are byte-identical to the exact source files. Their
  hashes are pinned in `verified-host-script-pins.txt`. These copies are
  qualified inputs only; this order did not invoke a host preflight or deploy.

## Isolated verification

All logs and smoke outputs remain outside the frozen package under
`/tmp/successor-admission-package.XYEusu/logs` and `evidence`.

- `go test ./internal/... -count=1` passed all internal packages, including
  full store and server suites.
- Focused `cmd/tt` delivery, current-assignment, operational-record,
  phase-successor, native-reader and relay checks passed (28 top-level tests).
- Focused store directive, operational, phase-successor and admission/no-op
  checks passed (24 top-level tests).
- Focused server operational, phase-successor and work-item HTTP checks passed
  (7 top-level tests).
- Selected `-race` store/server/CLI phase-successor, admission/no-op,
  directive and operational tests passed.
- `go vet ./internal/... ./cmd/tt`, changed-Go-file `gofmt -l`, and
  `git diff --check` passed with empty output.
- `node --test tests/truenas-release-preflight.test.js` passed 9/9.
- Python compilation for verified-host and all packaged smoke scripts passed
  with bytecode externalized.
- Dual builds, dual archive creation, clean VCS status, archive extraction and
  21/21 source-pin verification passed.

Five packaged Darwin CLI smokes passed using only loopback HTTP, isolated
HOME/config/output paths and synthetic identities:

- mandatory-v3 regression: 8 cases / 43 HTTP requests;
- follow-through regression: 7 cases / 27 requests, no actual Codex queue;
- operational-record regression: 5 cases / 9 requests;
- native evidence reader: 5 GET-only requests;
- new phase-successor serialization: 4 cases / 6 requests, proving
  `operationalRecords` v2 accepted-phase proposal and obligation lookup wire
  shapes plus v1 fail-closed behavior before unsafe requests.

These smokes prove packaged CLI capability gating and HTTP serialization. They
do not prove live runtime/provider continuation or server-side exactly-once
execution. Atomic obligation creation, exact evidence fencing, no-op admission,
CAS/replay and lifecycle behavior are covered by the focused Go tests and still
require independent frozen-package QA.

The established broad `cmd/tt` model/decision-mock expectation failures and
loopback server-close hang were not rerun or masked; no unfiltered broad CLI
suite pass is claimed.

## Source delta and behavior

Relative to installed `fae9f7f`, the candidate adds explicit accepted-phase
successor obligations. An accepted nonterminal operational phase atomically
creates one responsibility bound to exact item/scope/candidate/acceptance,
predecessor delivery/order and current lead/run. Assignment-stored,
acknowledged, progressing and owned-dependency states remain distinct. Exact
current-epoch worker acknowledgment/progress may satisfy the linked obligation;
predecessor self-link, stale epochs, wrong identity and unrelated activity do
not. Lead transfer, dependency/resume, replay, sibling action keys and missing
dependency owners remain fenced by native CAS/receipts.

The admission correction makes legacy or keyed work-item updates that change no
effective field preserve item/scope revision and any existing accepted-phase
successor authorization. Keyed response-loss replay returns the same receipt.
Actual status/scope/content mutation still advances the applicable revision and
requires fresh exact delivery authorization. No-op handling does not fabricate
Queue progress or new delivery evidence.

## Schema delta and rollback facts

The installed `fae9f7f` baseline already contains mandatory-v3 delivery columns,
sibling-action uniqueness, recovery incidents and operational-record v1 tables.
This package does not present those earlier migrations as new.

The exact new schema delta is in `migration-delta.patch`:

- additive `phase_successor_obligations`, keyed uniquely by task plus accepted
  record/version and retaining exact item/scope/candidate/acceptance,
  predecessor, lead, successor and dependency fields;
- additive indexes for lead/run/state and successor delivery lookup;
- additive `phase_successor_lead_transfers`, keyed by obligation/sequence and
  retaining exact from/to agent/run/delivery and request identity.

The admission/no-op change alters transaction behavior but adds no schema.
Profiles and legacy rows are outside these writes and were not touched by
packaging tests.

The tables are additive, so the accepted `fae9f7f` executable remains a binary
rollback candidate only before v2 phase-successor state is relied upon. Once
accepted-phase obligations or transfers exist, the older binary does not
understand or enforce them; semantic downgrade compatibility is not promised.
An operational rollback must retain the current database and account for
intervening writes. This package never restores an older database over newer
state; any host backup/preflight or restore remains handler-owned.

## Boundaries and next gates

This frozen candidate is not installation, deployment, activation, operational
release, Bug completion or parent Feature completion. It includes no
cleanup-obligation/fixture successor sources, frontend, Air CLI, relay reload,
Tailscale/network change, live task/profile/work-item/Queue fixture, real
enrollment or lifecycle action.

Before any operational release, the existing Feature QA must independently
qualify this exact frozen directory and the existing UI reviewer must verify its
source/manifest/migration/operator semantics. The database handler must save and
read back builder/reviewer results and exact package acceptance. A later bounded
release order must pin these bytes, after which handler-owned verified-host
backup/integrity/profile preflight and postcheck remain separate from lead-owned
installation and release receipt creation.
