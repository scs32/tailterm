# Bugs and Features live search

Work item `wi_1b3c05505232a68e` revision 3, integration-preparation order
message `#2519` (original implementation order `#2213`, correction order
`#2350`).

The Bugs and Features views now keep a compact search field beside their existing
filters. Matching is case-insensitive and requires every whitespace-separated
term. It covers the current title and description immediately, then enriches the
results from every retained work-item revision and every explicitly linked Board
message available through the hub history APIs. Features additionally search all
stored report sections and the title, stored content, or durable locator of every
retained artifact version. Clearing the field restores the complete result set
selected by the existing project and status filters.

History is fetched only after a query is entered and at no more than four work
items concurrently. Successful hub reloads advance a search generation and
invalidate enrichment even when the work-item revision is unchanged, so linked
messages and Feature narrative writes become searchable after refresh. Stale
asynchronous results cannot overwrite the newer generation, overlapping refreshes
coalesce, and a cached fetch failure is retried after later query input. The
status line distinguishes in-progress history loading, final match counts, and
records whose history or narrative content could not be loaded. The hub permits
narrative writes only for Features, so Bugs have no valid report/artifact bodies
to query; their current and retained descriptions plus linked messages remain
searchable.

The search control retains its query, focus, caret, and selection direction when
the hub refreshes and replaces the view.

Focused verification lives in `tests/work-item-search.test.js` and
`tests/work-item-search-browser.mjs`; the isolated work-item scroll and project
browser suites provide compatibility coverage for filtering, navigation,
editing, refresh recovery, and history actions.

## Release

Supplemental release order `#2607` fast-forwarded only the accepted Search
application into `tasks-hub` and published exact clean commit
`8a6862e5aed5000529cc7f96d4d81f3f11eab706` to Cloudflare Pages project
`tailos`. Deployment `d19b7090-5727-4ad5-8cf4-1754d9dcf79d`, the custom TailOS
domain, and Mini port 4318 all match release-manifest SHA-256
`6aa5857a4f821e1e79f991e52f042014cec17e41ed463d0704b567ca47656497` and
all 81 public assets. Mini PID 60799 / PPID 1 was preserved by an atomic
same-filesystem directory exchange. The exact prior Mini package is retained at
`.build/releases/live-search-previous-mini-2607`.

Fresh production Chromium checks on all three origins started the packaged WASM
and created/restored only a synthetic vault key. The focused Search and scroll
fixtures passed Chromium and WebKit before publication. Native Safari remains
untested. The pre-existing project-work-items fixture still times out after its
covered assertions while waiting for legacy `board message #` notice text; prior
review established identical behavior on the accepted baseline.
