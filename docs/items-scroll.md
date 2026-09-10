# Bugs and Features active-scroll refresh

## Recorded scope

This work implements bug `wi_72084e89ad88fd3f` revision 2 under bounded work
order message #1294. The owner reported that the scrolling failure already fixed
on Board remained in both Bugs and Features. Candidate evidence was posted in
#1329, accepted by lead in #1331, and verified against the committed item/order by
the database handler in #1330. Lead #1323 separately authorized the narrow shared
focus-restoration change after the WebKit reproduction demonstrated its need.

Scope stayed limited to work-item list scrolling, its focused browser fixture,
and the shared `preventScroll` option. Board was a read-only behavioral reference.
No live work items, profiles or owner browser storage were used. The independent
narrative-history candidate was not integrated or modified; its row control is
automatically covered by the scroll helper's generic `data-item-*` focus identity
when that feature is integrated later.

## Diagnosis

`createWorkItemsView.render()` replaced the entire root projection after every
background refresh. Because `.work-items-main` is the actual scrolling element,
that replacement disconnected the browser's active wheel target and focused
control. Reusing an absolute `scrollTop` alone would also move the reader when a
new item or changed row geometry appeared above the viewport.

An isolated in-memory fixture on clean base
`d102ab01b085677eea92c8b4411cb2f9119272c2` reproduced the same failure for Bugs
and Features in Chromium and Playwright WebKit: all four cases disconnected and
replaced `.work-items-main` during browser-injected wheel motion. This is
repeatable automated protocol input, not an observation of the owner's physical
device.

The first candidate additionally exposed a WebKit-specific focus interaction.
Restoring an offscreen status filter with plain `focus()` asynchronously returned
the scrolling container to the top after its reading anchor had been restored.
The accepted correction uses `focus({ preventScroll: true })` in the existing
shared refresh-presentation boundary.

## Implementation

- `client/work-items-scroll.js` owns the scroll lifecycle. Wheel and scroll events
  hold the exact current element and restart a 120 ms idle timer. Touch remains
  held through `touchend` or `touchcancel`, after which inertia scroll events can
  extend the timer.
- Refreshes continue fetching and replace the in-memory `items` array while a
  repaint is held. The idle flush therefore renders the newest completed state,
  not the first queued snapshot.
- Before replacement the helper records the first visible immutable work-item ID,
  its intra-row offset and any focused `data-item-*` row action. After replacement
  it restores the absolute offset as a fallback, corrects against the retained
  item anchor, and restores the action with `preventScroll`.
- A project/status filter-context change, hide, missing client or read error
  interrupts the active hold. Existing view-presentation filter commit and native
  select semantics are unchanged.
- `client/work-items-view.js` only adds lifecycle hooks before/after render, on
  hide/error and after a completed reload. No list row markup, history viewer,
  draft, pagination or dispatch implementation was rewritten.

The accepted builder candidate was
`2414f95f583fdaa598a8539be4d1b58c4981b863`. Its five changed files were applied
to the clean `tasks-hub` base at application commit
`f59ce7c804b78a149689979e85a7bf1b2b50c620`; an exact file diff between candidate
and integrated commit was empty.

## Verification

### Focused causal browser fixture

`node tests/work-items-scroll-browser.mjs` passes Bugs and Features in Chromium
and WebKit. It verifies:

- browser-injected wheel motion retains the exact scrolling DOM and focused
  status filter through an arriving refresh;
- a second wheel input continues moving that same element;
- the latest item count appears only after idle, with the same visible item and
  sub-row offset;
- ordinary inactive refresh also preserves the reading anchor;
- browser-managed smooth scrolling continues through refresh;
- synthetic touch start/end retains the target and flushes after release idle;
- focused row actions survive repaint without moving the viewport;
- the committed status filter drains all four fixture pages; and
- history and dispatch actions remain wired after repaint.

Browser-managed smooth scrolling is a momentum surrogate and dispatched touch
events validate lifecycle logic only. They are not physical momentum/touch tests.

### Existing regression coverage

- `npm test`: 132/132 passed.
- `node tests/project-work-items-browser.mjs`: Chromium and WebKit passed
  encrypted draft/retry replay and discard, CRUD, 65-link history pagination,
  history selection-race safety, dispatch and closed-project reads.
- `node tests/work-item-dispatch-browser.mjs`: 16/16 passed across both kinds and
  engines.
- `node tests/work-item-draft-vault-browser.mjs`: both engines passed bounded,
  credential-scoped encrypted draft behavior.
- `node tests/board-scroll-browser.mjs`: Chromium and WebKit retained Board wheel,
  smooth-scroll, synthetic-touch, anchor, bottom-follow, focus and draft behavior
  after the shared `preventScroll` change.
- The integrated root reran the focused work-item scroll fixture, the 11 shared
  presentation unit checks and the Board scroll fixture successfully. The
  accepted and integrated changed files matched exactly.

## Release evidence

The clean detached source/package is retained at
`.build/releases/items-scroll-f59ce7c`. `npm ci`, `npm run build:wasm`,
`npm run build:static` and `npm run verify:release` completed; 82 manifest entries
match `release.json`. Release SHA-256:
`c2d110fc75b4a42f9dd36cfe1840290959a6d923431ce6155f288570cdd94ea3`.

Explicit Wrangler deployment to Cloudflare project `tailos`, production branch
`main`, created deployment `35a9032e-babb-4159-b13a-3cae1f9bbac5` at
`https://35a9032e.tailos.pages.dev`. That immutable origin and
`https://tailos.tailarr.com` each match the release JSON and all 81 served files.
The custom domain retains its previously documented `max-age=14400` override for
hashed assets; the immutable origin serves packaged `no-cache` headers.

The verified retained package was synchronized exactly to the existing Mini
served root. Listener PID 28664 and command
`node scripts/preview-static.mjs dist-static 127.0.0.1 4318` did not change. Mini
matches release JSON and all 81 files. JavaScript, CSS and production WASM MIME
checks pass on all origins; the `.wasm.gz` response is `application/gzip` without
`Content-Encoding`.

Fresh disposable Chromium contexts on TailOS and Mini loaded the real production
WASM, created and restored an isolated synthetic vault/key, retained matching
layout margins and reported no page errors. Their screenshots have identical
SHA-256 `dee008e5bb73ff69cb91fb1e43b1dc1241bc26b73201661e1427f62041b71664`.

## Limits, nonpasses and rollback

No physical wheel/trackpad/touch, native Safari or owner-device event trace is
claimed. During fixture construction, an inner-template syntax error and a
browser/Node scope error were corrected before the causal baseline. The first
WebKit candidate failure identified the real plain-focus scroll reset and led to
the authorized `preventScroll` correction. One integrated test orchestration
wrapper printed no results because its reporting variable was misspelled; the
identical commands were rerun and passed. There were no final candidate or release
test failures.

`npm ci` reported three existing moderate audit findings and install-script review
notices. Vite reported its existing large-chunk advisory. Installation, production
WASM/static builds and release verification completed without error.

Rollback is the retained package
`.build/releases/board-scroll-followup-8e78f55/dist-static`, application
`8e78f5508ddb0e15912ff39b397474f7df5b20fd`, deployment
`https://0d7863de.tailos.pages.dev`, release SHA-256
`7080d78e74573ceca7552af76f6783e7c123d62037439d5dffdde81a20b6f5a5`.
Before activation, TailOS and Mini both matched that exact retained rollback.
Deploy it explicitly to project `tailos` and synchronize it to Mini without
restarting PID 28664 if rollback is required.

This release did not change the hub, CLI, schema, database, Tailscale, TrueNAS,
relays, Air preview, `https://tailterm.tailarr.com`, live task/profile data,
owner browser storage or agent sessions.
