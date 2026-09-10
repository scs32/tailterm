# Feature narrative history implementation report

This report covers `wi_0535c67103994980`, bounded implementation order
`narrative-build-1` recorded at revision 5 in Board message 1235. The acceptance
baseline is the exact contract in Board message 1229. Current feature scope is
revision 6 after lead's Board message 1395 resolved the retained audit-source
link boundary.

## Requested outcome

Store and query a feature's original request, evolving decisions, deliberately
captured artifacts, coverage gaps and final write-up without conflating storage
with proof. Keep full reports in the hub, expose them through a safe reloadable
Features reader and require a current verified report before a feature becomes
Done. Preserve existing work-item, routing, history and closed-project behavior.

## Delivered work

The hub now has additive authorities for artifact identities and immutable
versions, relationship events and retractions, coverage revisions, structured
full report revisions, narrative timeline entries and keyed operation receipts.
Artifacts distinguish stored content from references, missing and unavailable
material; provenance, original source identity and time, ingester identity,
server and supplied digests, availability, and correction ancestry are retained.
Internal links validate exact retained targets and project scope. External PR,
CI, supporting and opaque AIV evidence are accepted only as deliberately
submitted captures or references; nothing crawls remote sources.

The HTTP and Go clients expose overview, stable paged timeline and metadata
queries, exact artifact/report versions, links, coverage and receipts. The `tt`
CLI exposes the same reads and JSON-file writes. Request IDs, actor identity,
canonical-payload receipts and expected-version checks provide response-loss
recovery and conflict safety. Frozen cursors separate stable traversal order
from source timestamps. Metadata is byte-bounded and continues through a frozen
cursor, including overview coverage. Content is fetched separately, capped at 1
MiB of decoded UTF-8 and never silently truncated; escaped transport bytes use
the separate larger request/response bounds. Report and coverage references
have a 384 KiB aggregate logical-JSON limit; the transport bounds include its
worst legal encoding together with a full report.

Feature completion now atomically validates a complete structured report and
its exact ID, version, digest and current scope revision on both update paths.
Scope edits stale an older report; metadata edits do not. Existing Done records
are not reopened or backfilled, bugs are unchanged, and later report revisions
do not alter the original completion pin.

Each Features entry now has a labelled **History & report** reader. It displays
full report sections and references, work-item revision history, explicitly
linked retained messages and decisions, artifact corrections/content, coverage
claims and gaps, and the narrative chronology. Stored text is escaped, external
resources are not fetched or embedded, stale requests are ignored, draft state
is retained and the feature-specific URL reloads the same reader. Latest report
corrections are shown alongside the preserved completion pin and every exact
report version remains selectable. Coverage shows captured IDs, gaps, as-of
time, declaration author, assessment verifier and exact evidence versions;
retracted links remain visible as history but no longer resolve as current
sources. No change was made to the separately owned `client/board-view.js`.

## Verification and scope

All verification used isolated temporary databases, mock browser contexts and
throwaway hub processes; live tasks and the retained audit were not fixtures.
Focused store tests cover artifact corrections, exact retry after later
versions, payload conflicts, link retraction, coverage, reports larger than
8192 bytes, decoded/escaped 1 MiB boundaries, evidence-required assessments,
report-before-Done through both update forms, stale scope, immutable completion
pins, closed reads, concurrent CAS and count/byte-bounded frozen pagination.
Server and CLI tests cover request limits, exact full-content reads, structured
completion errors, retries and unsafe locators. Chromium and WebKit exercise
safe rendering, post-Done corrections and historical report selection,
retraction semantics, provenance, stale artifact failures, reload, coverage
gaps, distinct Board/reviser attribution and retained drafts. The actual Go HTTP
client also round-trips a full escaped report plus the largest supported
reference set and rejects the next reference without truncation. The broader
internal Go, race, CLI, JavaScript and existing work-item browser suites are part
of the candidate verification handoff.

Lead accepted candidate `c9024893dfe7510d6c80859887d833844911b3fa` in
Board message 1352 after two read-only review rounds. The accepted commits were
then rebased without conflict onto lifecycle release root
`9f700e90e6f76ca111d6eafad0e7d7cc41e2e3b8`; the exact integrated application
commit is `4cb6e6c6a28cf5d4b94e0f8a572be0eec62ae16a`. Focused integration checks after
that rebase passed the narrative, agent-closeout, cleanup and retirement store,
server and CLI paths plus `go vet` on the affected packages. Chromium and WebKit
passed the narrative reader, Bugs/Features active-scroll and existing work-item
reader fixtures. The affected lifecycle JavaScript checks passed 6/6. The
accepted broader-suite evidence was not rerun for unchanged code, as directed by
lead.

## Release and operational verification

Lead assigned the release slot in Board message 1378. A clean detached source
and package is retained at
`.build/releases/narrative-history-4cb6e6c`. Its `release.json` records exact
commit `4cb6e6c6a28cf5d4b94e0f8a572be0eec62ae16a`, a clean tree and 82 manifest
entries; all hashes pass, and the manifest SHA-256 is
`a90d44e82dabd0ecbb7514468fd19c18195cdb001f61a8cb5b4fefaec5f8baa1`.
The production WASM was rebuilt without the test tag. The main JavaScript is
`assets/index-D0DTVFVO.js` at SHA-256
`0e4e39c279c9bc31a0b19c195f4acc8a4ddb0befa0c7bb79910d2ed4e414a354`,
the stylesheet is `assets/index-Ck2a0xYT.css` at SHA-256
`109c57856f3279a9210d2dbee8d9c5079ec774e7a67142be8a797ef696c3fd3a`,
and the production WASM gzip is `assets/tailserve-dLtbYZ-Y.wasm.gz` at
SHA-256 `7dd988b0bd79a1a28088d3d66f5371a32dcb3db8e9583ebc05ca21719402349d`.

The exact package is deployed to Cloudflare Pages project `tailos` at immutable
URL `https://fa7bd9ec.tailos.pages.dev` (deployment
`fa7bd9ec-545d-4f07-aefe-4f574d568e9f`) and the custom
`https://tailos.tailarr.com` domain. The same bytes are served on Mini at
`http://127.0.0.1:4318`; detached preview PID 60799 / PPID 1 was preserved.
Each origin returned all 81 public manifest assets with the exact size and
SHA-256, non-empty appropriate MIME, no unexpected content encoding and the
same release commit. Fresh Chromium on the immutable deployment, custom domain
and Mini started the production WASM, created and restored an isolated synthetic
vault/key, retained matching layout margins and reported no page errors.

Before the hub update, SQLite online backup
`/mnt/deepfreeze/tailterm-hub/backups/before-narrative-history-20260910T034609Z.sqlite`
was created through the backup API. It is mode 0600, 9,367,552 bytes, SHA-256
`e82c421c38229a21e74c2a44d4e97d8a941a20b08ee54df3c3df280a3c3f5113`,
passes `integrity_check` and has no foreign-key violations. TrueNAS middleware
updated only the existing `tailterm-hub` app to named release
`20260909-narrative-history-4cb6e6c`. It is RUNNING as one container on the
unchanged `100.116.238.37:18765` TCP listener, mounts the existing state/token
paths, and serves exact Linux amd64 hub SHA-256
`49591cd6083a590c0afca68c05f50ba416c1e98f01ae006e92bb341e956b5475`.
Post-migration integrity and foreign keys pass and all 12 additive narrative
tables are present. Authenticated CLI health passes while unauthenticated
`/v1/whoami` remains 403.

The matching Darwin arm64 `tt` SHA-256
`a2a640d18e3d3014bcd8afdafbaf1f8994c5fbaf4623ea168014aeba7f68b49a`
is installed atomically on Mini and Air. Both retain rollback binary
`tt-before-narrative-history-4cb6e6c` at SHA-256
`6ad14fa7e8562b782faf2632e878f2371c5f66c69b70610ec93884fb37c6ed29`,
both pass `tt doctor`, and relay PIDs 83457 and 912 were not restarted. Matching
Linux amd64 and arm64 build hashes are respectively
`1331a98ec1c3eec1f572c27c33c431a58e3f78319efb9cdd90be2a806ad26842`
and `649cf4ff07944f74f640b402f546804f1db41ed47eef91b7ed1573bf8ca11bdc`.

Immediate frontend rollback is the retained lifecycle package/application
`9c7bb254a590c073e9a1f6901757dc7448a7478c` and immutable deployment
`https://45be5874.tailos.pages.dev`. Hub rollback points middleware to retained
release `20260909-lifecycle-closeout-1d9d75d`, SHA-256
`f55018914dd7fa0b3eb454c452c1cae4749ce6b297ffddca3ce8e77c41d66d18`,
while preserving the additive database; the online backup is disaster-recovery
evidence and is not a normal rollback because restoring it would lose later
writes. CLI rollback uses the exact per-host files above without restarting
relays.

Operational corrections are recorded rather than hidden: the first clean
static build stopped before packaging because an ignored production WASM was
absent, after which the pinned production build script generated it without the
test tag and the clean build passed. An initial read-only TrueNAS preflight used
macOS `stat` flags on Linux, and an initial read-only `midclt app.query` used job
mode; both failed without mutation and were rerun with the correct forms. The
first deployment-list projection guessed lowercase field names and returned
nulls; the unchanged result was re-read with its actual capitalized schema.
`npm ci` reported three existing moderate audit findings and install-script
approval notices, and Vite reported its existing large-chunk advisory; build,
package and deployment verification all completed.

The real instruction-audit discovery sources were inspected read-only and
verified against their supplied hashes. The immutable audit remains Done at
revision 3 with `reconstructed_change_log` provenance and
`shared_workspace_claim` attribution. Original Board message 606 remains
distinct and lead-authored. The handler initially proved that the retained
message was not item-linked, so the gap was reported rather than hidden. After
lead recorded scope amendment 1395, the handler added exactly one explicit
native-message relationship using keyed request `source-606-audit-link-1`.
Link `nlnk_d6a962ac7361eb16` revision 1 / narrative sequence 1 and receipt
`nrr_3bf6fcc960fdb262` were read back through the links, receipt and filtered
timeline APIs; the link resolves the unchanged native message 606 with its
original lead identity and time. The audit remains Done r3/scope r3 with the
same description digest and an explicit missing-dedicated-report warning. No
report, artifact, coverage declaration, content import, inferred match or
work-item revision was invented, and default PR/CI coverage stays
not-ingested/unverified/unknown.

## Limitations

Coverage is limited to exact internal records that are explicitly linked and
external material that an authorized caller deliberately submits. Similar text
or titles do not establish a relationship. The hub does not authenticate to,
crawl, synchronize or continuously check external sources, and a coverage
declaration is an attributed claim rather than a guarantee. Reports cannot
prove their prose truthful or prove that tests and deployments ran. Reports
larger than 1 MiB require a future explicit limit change.

## Remaining work

The Database handler must store this complete dedicated report against current
feature scope revision 6 through the released narrative API, retrieve and verify
its exact version/digest, and pin that current report in the separate
feature-Done update. Lead must accept the actual release evidence before that
completion. No product or release work remains. No production service outside
the explicit TailOS, Mini, hub and matching CLI targets changed; no networking,
Tailscale, old-site, Air preview or AIV change is included.

## References

- Work item `wi_0535c67103994980`, current scope revision 6
- Work order Board message 1235
- Contract Board message 1229
- Original owner request Board message 626
- Audit work item `wi_630de1aee298c3c7`, immutable Done revision 3
- Original audit Board message 606
- Candidate acceptance Board message 1352
- Release assignment Board message 1378
- Audit-link scope amendment Board message 1395
- Audit-link verification Board message 1397
- Integrated application commit `4cb6e6c6a28cf5d4b94e0f8a572be0eec62ae16a`
