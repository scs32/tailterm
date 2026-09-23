# Frozen successor/admission package — independent UI/operator review

## Verdict

**PASS for the exact frozen candidate and the bounded qualification scope.**

This review covers Feature `wi_618c8ff87e6b8061` revision 5, immutable UI
binding order `#3584`, governing package order `#5775`, instruction `#5822`,
and delivery `dly_0b9a667a17b2d89a` generation 3 / execution epoch 1 for
`agt_b3987b5101f7f149/run_359f07871e71e412`. Direct acknowledgment receipt:
`drr_756695c6bd34d96f`; first progress receipt: `drr_7390da660cc96e46`.

No blocking or misleading operator defect was found. This PASS qualifies only
the package named below. It is not handler acceptance, installation,
activation, deployment, a live-provider continuation result, Feature
completion, Queue completion, cleanup readiness, or permission to restore a
database.

## Exact candidate

- Frozen directory: `/tmp/successor-admission-package.XYEusu/package`, mode
  `0555`; all payload files are `0444` or `0555`. The enclosing directory is
  mode `0700`.
- `manifest.json`: 10,103 bytes,
  SHA-256 `0d0f5bd985adee2be099dd3ae839786c34d0883b82a57881b08d4afc6de6346d`.
- `checksums.txt`: 1,946 bytes,
  SHA-256 `fd1f253c703a67d6303c3b4a636b12e7123c12358a983863b9737e70059d6684`.
- `package-report.md`: 9,170 bytes,
  SHA-256 `bb1766efc3ce8426e230797f0121a4238f9356639002324682379d1557fd01e8`.
- External `builder-result.json`: 4,856 bytes,
  SHA-256 `ea95e3db5e53597317adf7e2a787a99c9992afe14aadbb81b907c58db97ccb41`.
- Linux/amd64 hub: 27,873,406 bytes,
  SHA-256 `1d9e97679b24428a9784430e3c96d908debc3bac71c869ac1e7f4b73a232d232`.
- Darwin/arm64 CLI: 6,919,234 bytes,
  SHA-256 `2c23f9975462afb0bb587d7e213ae6272cae6062e6cf9a09bfba451133b1b378`.
- Source archive: 11,550,720 bytes,
  SHA-256 `28b6db51774ed56af5da2959647108031406dc526bcb8c972c5a09cf51c44ba0`.

There are 22 files in the package: 21 checksum subjects plus
`checksums.txt`. All 21 checksum entries passed. The manifest contains 20
payload entries because the checksum file is the extra package index. Every
manifest-pinned external log and smoke file exists at the declared byte size
and digest.

## Source, lineage, and reproducibility

`git get-tar-commit-id` returns exact source
`5793de112e1b4c0a08d55fba17a7736e0bf2e0bf`; the archive has 660 entries.
Local object inspection returns exact tree
`43b026c59c40dcc2eb3dae75b397340007ff3225`. Installed baseline
`fae9f7ff34627449f716ef6439fe1eed5ab8ead5` is an ancestor with exactly 11
intervening commits and 21 changed paths. The changed-path list equals the 21
source pin paths, and all 21 extracted source hashes pass.

The current installed Mini CLI independently reports VCS revision `fae9f7f`,
`vcs.modified=false`, and SHA-256
`7a63670a9210c7f5208cb500845e53d2f9fd42cc4f33d175c3221cf5d9d060c1`,
matching `/Users/stephenspeicher/projects/tailterm/docs/releases/mandatory-v3-fae9f7f/package-pins.json`.
The accepted baseline release receipt is
`docs/releases/mandatory-v3-fae9f7f/release-receipt.json`, SHA-256
`f78bed835eb0124ced015efff91864749d5654755b8913ab4ca02e69655dc1fb`,
and its saved acceptance is
`docs/releases/mandatory-v3-fae9f7f/release-acceptance.json`, SHA-256
`1dc7f00a80c67a060790c9507ac174abe46e5aba5b161781286001ff39b4facc`.
That proves `fae9f7f`, not historical `19970cea`, is the operator comparison
baseline.

Both packaged binaries report Go 1.26.6, `CGO_ENABLED=0`, `-trimpath`, exact
VCS revision `5793de1`, and `vcs.modified=false`; formats/targets are static
ELF linux/amd64 and Mach-O darwin/arm64. The three files under
`/tmp/successor-admission-package.XYEusu/repro2` are byte-identical to the hub,
CLI, and source archive. The two packaged verified-host scripts are
byte-identical to their exact archive source and match
`verified-host-script-pins.txt`.

The retained upstream manifests pin 14 phase-successor source files and six
admission source files; every byte/hash pin matches the archive. The manifests
are historical integration snapshots and retain their then-current “acceptance
pending” limitations. They are not, by themselves, final acceptance. The
package manifest/report correctly adds the later exact acceptance references:
phase successor integration/handler `#5592/#5594`, and admission integration,
QA, handler, and lead `#5624/#5654/#5655/#5777`. Those references and handler
readback remain part of the later release evidence chain.

## Behavior and operator contract

The source delta composes only the two accepted inputs and contains no
frontend, cleanup-obligation, fixture-successor, relay, network, Tailscale, or
deployment-state change.

Typed phase acceptance is correctly distinct from generic Done, prose, or a
delivery result:

- `hub/internal/store/operational_records.go:463-486` validates the typed
  acceptance and its referenced result/verifications.
- `hub/internal/store/operational_records.go:490-632` revalidates exact item,
  scope, current delivery generation/epoch, actor, and state in one transaction.
  Only an accepted, nonterminal record with an explicit `phase` calls the
  successor creator at lines 601-614. A generic result only marks its delivery
  result at lines 615-618.
- `hub/internal/store/phase_successor.go:233-336` atomically creates the lead
  message, version-3 independent lead action, pending obligation, event, and
  acceptance transaction state. Rejected, terminal, and legacy v1 acceptance
  create no obligation (`phase_successor_test.go:114-185`).
- `hub/internal/store/phase_successor.go:339-416` fences dispositions by stable
  request identity, obligation version, acceptance/candidate/phase, exact
  accountable lead run, current item/scope, and current lead delivery.
- `hub/internal/store/phase_successor.go:537-599` requires a distinct current
  item-worker delivery with exact work order/agent/run and native current-epoch
  acknowledgment or progress. Stored assignment is visible but not fulfilled;
  stale or unrelated activity does not count.
- `hub/internal/store/phase_successor.go:600-664` keeps an owned dependency and
  exact resume separate from successful successor execution and clears stale
  successor selectors before returning to pending.
- `hub/internal/store/delivery.go:560-692` retains immutable message/order,
  current item/run/context, generation CAS, action identity, and unresolved
  successor responsibility when attaching/superseding delivery.

Admission no-op behavior is narrow and preserves exact authorization:

- Legacy PATCH returns the unchanged item without row/history/Queue/event
  mutation at `hub/internal/store/work_items.go:349-356`.
- Keyed updates retain a response-loss receipt at the unchanged revision, with
  no manufactured work-item revision, at
  `hub/internal/store/work_items.go:568-583`.
- Real field differences still advance revision and only real title/description
  differences advance scope at `hub/internal/store/work_items.go:585-641`.
- The cross-feature integration test at
  `hub/internal/store/admission_phase_integration_test.go:11-48` proves legacy
  and keyed no-op updates preserve an accepted obligation, exact replay returns
  the same receipt, and a later exact successor acknowledgment fulfills it.

Capability behavior is truthful: `operationalRecords` advertises versions 1
and 2 (`hub/internal/api/audit_export.go:82`); accepted-phase and successor CLI
operations require v2 and fail closed before an unsafe request on old/unknown
hubs (`hub/cmd/tt/operational_records.go:15-28,74-79,100-111,135-188`). Nothing
in this package advertises provider continuation, unattended activation,
exactly-once external execution, cleanup, frontend rollout, or parent Feature
completion.

## Migration and rollback

`migration-delta.patch` is byte-identical to the exact `git diff -U5` of
`hub/internal/store/operational_records.go` from `fae9f7f` to `5793de1`. It adds
only `phase_successor_obligations`, its two indexes, and
`phase_successor_lead_transfers`, plus typed phase validation/creation logic.
The admission no-op correction adds no schema. The complete internal suite and
fresh migration tests pass.

The package report gives the correct operator warning at lines 148-154: the
accepted `fae9f7f` executable is a reasonable binary rollback only before v2
successor state is relied upon; after v2 writes it does not enforce the new
invariant and semantic downgrade compatibility is not promised. Any rollback
must retain the current database and account for intervening writes. An older
backup must never replace newer state.

The next-gate language at lines 164-170 is also correct: a later, separately
bounded release must pin these exact bytes, and handler-owned verified-host
backup/integrity/profile preflight plus an independently pinned receipt must
precede lead-owned TrueNAS/TailOS installation. This review performed no host
preflight, backup, restore, installation, activation, or deployment.

## Independent executed checks

All commands used the frozen package, the extracted archive under a private
`/tmp` directory, loopback-only synthetic servers, and private output paths.

- `shasum -a 256 -c checksums.txt`: 21/21 PASS.
- Archive commit/tree/entry, ancestry, lineage, changed-path, source-pin,
  upstream-pin, binary format/VCS and `repro2` byte comparisons: PASS.
- `go test ./internal/... -count=1`: all six internal packages PASS. Log:
  `/tmp/successor-admission-ui-review.iczS7F/go-test-internal.log`, SHA-256
  `54ca6cb31abe333c3da9a7a6c347267ff5c05b10ccb909f1bb6a58c2b685a169`.
- Focused admission/phase-successor store suite: PASS. Log:
  `/tmp/successor-admission-ui-review.iczS7F/go-test-phase-admission.log`,
  SHA-256
  `39842933f7199793ea6d1b1fd212e9f0a0b3539f8a10362c58217f2785dabce0`.
- Focused operational CLI HTTP/capability suite: PASS. Log:
  `/tmp/successor-admission-ui-review.iczS7F/go-test-cli-gating.log`, SHA-256
  `c02c6da13f339b12ce4638290c90d1ef254f50dbf1c457a06269fda14f3141b4`.
- Verified-host Node suite: 9/9 PASS. Log:
  `/tmp/successor-admission-ui-review.iczS7F/node-preflight.log`, SHA-256
  `2b3c599a8edb54d0d1b597a26a1fe83e0ceb1fd608dfc70c2fdc748554fafc84`.
- Fresh packaged CLI smokes: mandatory-v3 8 cases/43 requests; follow-through
  7/27 with fake Codex only and no provider queue; operational 5/9;
  evidence-reader five GET-only requests; phase-successor 4/6. Evidence root:
  `/tmp/successor-admission-ui-smokes.mTSF4X` (mode `0700`). Result digests,
  respectively:
  `067ba9cc86acfcafff5ebffa68efce7fbc228956ad742a9c7c09e72b3f63f305`,
  `fa14b6024678aeb23a1233e14f64be4a3236749ec2976b5954a1063442cc1b80`,
  `28d98027d6e727242c136d7e99383ef08b3a0637cd40cdfa7116d1afbe115ced`,
  `41b9ae4b5a51f78de1b14187a2e936c90a57d65c31a0110ed288804ea9841292`,
  and `e8f0ce98e8015222130dcc00c9994cea5bb9c01ff527f56086e0000c140c2a1a`.

The fresh raw follow-through JSON differs from the builder-retained raw JSON
only in its new runtime `observedAt` value; the cases, requests, safety flags,
and limits are unchanged. The builder-retained evidence itself matches its
manifest pin exactly.

## Preserved limits and next gates

- The known broad `cmd/tt` model/decision-mock expectation failures and
  loopback close hang were not rerun, hidden, or claimed fixed. No unfiltered
  broad CLI PASS is claimed.
- Synthetic CLI serialization is not source/store proof, provider execution,
  or exactly-once external execution. The independent source tests above are
  separate evidence.
- No real task/profile/work-item/Queue/database/provider/tmux fixture was used.
- No package or source byte/mode was changed. The reviewer worktree remains at
  baseline `ad89388`; its pre-existing untracked order-3584 report is preserved.
- Handler-saved package acceptance and a later exact operational release remain
  mandatory. The Feature remains open.

