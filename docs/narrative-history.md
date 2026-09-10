# Feature narrative history implementation report

This report covers `wi_0535c67103994980` revision 5, bounded implementation
order `narrative-build-1` recorded in Board message 1235. The acceptance
baseline is the exact contract in Board message 1229.

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
the separate larger request/response bounds.

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
gaps and retained drafts. The broader internal Go,
race, CLI, JavaScript and existing work-item browser suites are part of the
candidate verification handoff.

The real instruction-audit discovery sources were inspected read-only and
verified against their supplied hashes. The immutable audit remains Done at
revision 3 with `reconstructed_change_log` provenance and
`shared_workspace_claim` attribution. Original Board message 606 remains
distinct and lead-authored. No report was invented, imported or used as test
data.

## Limitations

Coverage is limited to exact internal records that are explicitly linked and
external material that an authorized caller deliberately submits. Similar text
or titles do not establish a relationship. The hub does not authenticate to,
crawl, synchronize or continuously check external sources, and a coverage
declaration is an attributed claim rather than a guarantee. Reports cannot
prove their prose truthful or prove that tests and deployments ran. Reports
larger than 1 MiB require a future explicit limit change.

## Remaining work

Lead must review and accept the exact candidate and assign a release slot.
Deployment remains pending to TailOS, the existing Mini and the hub through the
approved TrueNAS path, with backup, additive migration and rollback evidence.
The Database handler must store and verify the final report/result and confirm
the completed work-item update after those release dependencies are satisfied.
No production service, networking, Tailscale, old-site or AIV change is included.

## References

- Work item `wi_0535c67103994980`, revision 5
- Work order Board message 1235
- Contract Board message 1229
- Original owner request Board message 626
- Audit work item `wi_630de1aee298c3c7`, immutable Done revision 3
- Original audit Board message 606
