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
