# Bugs and Features live search candidate

Work item `wi_1b3c05505232a68e` revision 1, bounded order message `#2213`.

The Bugs and Features views now keep a compact search field beside their existing
filters. Matching is case-insensitive and requires every whitespace-separated
term. It covers the current title and description immediately, then enriches the
results from every retained work-item revision and every explicitly linked Board
message available through the hub history APIs. Clearing the field restores the
complete result set selected by the existing project and status filters.

History is fetched only after a query is entered, at no more than four work items
concurrently, and cached by item revision for the life of the view. The status
line distinguishes in-progress history loading, final match counts, and records
whose history could not be loaded. Narrative report/artifact bodies are not part
of this candidate index: they use separate feature-only APIs and are not available
for Bugs. The current description and all retained descriptions remain searchable.

Focused verification lives in `tests/work-item-search.test.js`; the existing
isolated work-item browser suite remains the integration regression check for
filtering, navigation, editing, and history actions.
