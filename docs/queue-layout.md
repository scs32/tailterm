# Queue layout repair

## Recorded scope and provenance

This candidate repairs Bug `wi_c36a4696771a2072` revision/scope 3 under bounded
Work Order Board message `1726` and lead selection `1717`. The immutable admitted
context was read in full before implementation:

- `/tmp/tailterm-queue-layout-context-1726-76fe4b649874.json`
- 51,518 bytes, mode 0400, SHA-256
  `76fe4b649874b94713c45a51218664a13dbca3b69d9a30ca0009bb77612aba9e`
- all three revisions, zero gaps, all four explicitly linked messages, and both
  full source messages (`1713` and `1717`)

Implementation began from exact clean commit
`4e8e6c5b38905b25b5173d0044560be45e5b6e88` on `fix/queue-layout` in the
isolated `queue-layout` worktree. The superseding exact application/source
candidate is `699ecd42eca6193e81b5e10ffbcd6884b6e2423a`, tree
`68b1c9b14e0a91f58eab0460b8f99207f06609bb`. Its complete four-file
`+861/-16` binary diff from the accepted baseline has SHA-256
`6d20ffe9ecc46ed60ae00278117878cc58d0cedba23505553fe2a890210c63ac`.
This later report-only commit records that immutable identity without changing
the tested application tree.

The owner evidence is
`/Users/stephenspeicher/projects/tailterm/Screenshot 2026-09-10 at 1.31.31 PM.png`,
779,927 bytes, SHA-256
`52fd51f6aee8d779346eb954b148ce9045d6661cfd7f6caaf888f1312887737e`.
It was inspected at original 2794×1704 raster resolution before edits. Those are
image dimensions, not an established CSS viewport, device-pixel ratio, browser
zoom, or Safari version. Board message `1713` is the lead's relay and inspection;
no separate original human Board sequence was supplied. The image is the new
Queue overlap report, not the earlier reply-field double-line report.

No live work-item, Queue, profile, vault, provider, SSH, tmux, production origin,
network, Tailscale, TrueNAS, relay, service, or agent-lifecycle state was read or
changed. No deployment was performed. All browser data and contexts were
synthetic and disposable. Files remains hidden.

## Independent review correction

The first application commit `952f36bf5867dfe84d2bc3c99a98c491a46c51b6`
and its report-only child `2d0560557f29417f919b07bdd4fae60e24e3eb9b`
are retained as review provenance, not as accepted candidates. Lead review Board
message `1742` rejected that candidate after reproducing a 761px viewport seam:
the persistent 190px project rail left a 326px main region (310px Queue layout),
but the inner grid still required 504px and the selected detail escaped by about
194px. Review also found that the initial baseline run used baseline Queue CSS
with current Queue markup rather than pinning both owned baseline files.

Commit `699ecd42eca6193e81b5e10ffbcd6884b6e2423a` corrects both findings without a
scope expansion:

- the Queue main establishes an inline-size container, and list/detail stack
  when the actual Queue thread is at most 540px wide, independent of viewport;
- the regression now uses both `client/queue.css` and `client/queue-view.js` from
  exact baseline `4e8e6c5...` in baseline mode;
- the matrix adds 320, 760, 761, 768, 800, 900, and 1024px boundaries to the
  original desktop, narrow, and zoom-equivalent cases.

The corrected 761px WebKit screenshot was visually inspected alongside the
exact-baseline screenshot. The baseline detail paints through the rows; the
candidate list and detail are a vertical stack within the 310px Queue thread.

## Reproduction and root cause

The focused regression mounts the actual Queue view inside the production
`#workspace > main > #mode-view` hierarchy, retains the sidebar/header/mode
chrome, uses the complete application stylesheet order from `client/main.js`,
and supplies 18 synthetic Queue rows, multiple projects, a selected detail,
long titles and descriptions, unbroken identifiers, statuses, markers, and
controls.

The exact accepted baseline reproduced the three owner-observed failure classes
in both Chromium and WebKit:

- Queue rows kept the later global `#mode-view button { white-space: nowrap; }`
  rule. Their intrinsic width escaped the first grid track; on desktop the
  selected detail intercepted row pointer input and painted through the list.
- Queue rail buttons use `data-queue-task`, while shared Board geometry targets
  `data-board-task`. The Queue title and `Receiving Queue` subtitle therefore
  stayed inline and collided.
- The later global `input { width: 100% }` rule stretched the checkbox away from
  its text instead of keeping a compact label/control pair.
- The shared narrow data-view workspace selector did not include Queue, leaving
  it without the intended mobile viewport/sidebar behavior.

This is a Queue-local production import/specificity mismatch plus an
intermediate-width grid minimum. No global CSS change is needed or included.

## Repair

`client/queue.css` now owns sufficiently specific Queue-only geometry after
global stylesheet composition:

- the outer Queue rail/thread and inner list/detail grids explicitly permit
  their tracks and children to shrink;
- list/detail stack from actual Queue-main inline size when the desktop rail
  leaves insufficient room, closing the 761–1024px transition seam;
- Queue rows, detail headers, paragraphs, facts, titles, metadata, and unbroken
  identifiers wrap within their assigned region;
- `data-queue-task` buttons receive the intended stacked rail geometry, with
  readable wrapping on desktop and a bounded two-line title in the narrow
  horizontal rail;
- the history checkbox is fixed at 14×14 pixels beside a presentation-only text
  span, while the existing label remains the accessible association and click
  target;
- Queue receives the same compact narrow workspace, hidden global sidebar,
  horizontal project rail, one-column list/detail stack, and independently
  scrolling thread behavior used by the other data modes.

The only markup change is the `Terminal history` text span. No Queue events,
state, networking, actions, retry identities, receipts, selection data, drafts,
subscription epochs, admission, export, or launch behavior changed.

## Measured browser evidence

The fixture covers twelve CSS viewports per engine: 320×720, 390×780, 760×900,
761×900, 768×900, 800×900, 900×900, 1024×900, 1366×900, 1397×852/DPR-2,
1920×1080, and 683×450/DPR-2. The last case models a constrained viewport such
as 200% zoom; it is not native browser zoom. The 1397/DPR-2 case is only a
plausible comparison to the supplied raster, not a claim about the owner's
viewport.

Representative WebKit measurements after the fixture scroll (pixels):

| Case            | Baseline row right / detail left |                       Candidate geometry | Baseline / candidate checkbox | Candidate thread client / scroll height |
| --------------- | -------------------------------: | ---------------------------------------: | ----------------------------: | --------------------------------------: |
| 761 boundary    |                    2679.56 / 655 | list bottom 2727.39 / detail top 2741.39 |                     57.8 / 14 |                              794 / 6587 |
| 1366 desktop    |                 2730.70 / 830.42 |    row right 816.42 / detail left 830.42 |                     57.8 / 14 |                              780 / 2471 |
| 1920 desktop    |                2744.56 / 1054.80 |  row right 1040.80 / detail left 1054.80 |                     57.8 / 14 |                              946 / 1885 |
| 390 narrow      |      baseline 10px usable thread | list bottom 2238.09 / detail top 2252.09 |                     57.8 / 14 |                              501 / 5289 |
| 200%-equivalent |      baseline 10px usable thread | list bottom 1437.59 / detail top 1451.59 |                     57.8 / 14 |                              217 / 3320 |

Every candidate case asserts:

- rail/thread, header/content, and list/detail rectangles do not overlap;
- document width stays bounded and visible non-rail controls stay inside the
  mode viewport;
- long Queue text has no horizontal scroll overflow and every measured text
  range stays within its assigned rail or thread region;
- the narrow rail is internally horizontally scrollable without document
  overflow, and list/detail becomes a vertical stack when viewport or actual
  thread width requires it;
- the Queue thread has a usable vertical viewport and actually scrolls;
- keyboard focus has a visible outline, Enter selects exactly one row, pointer
  selection remains hit-testable, and selected fill is retained;
- the checkbox remains 14×14, within ten pixels of its label text, and the full
  history label remains visible.

Exact-baseline artifacts (24 cases, exact baseline CSS and Queue view):

- `.build/queue-layout/baseline-results.json`, 536,811 bytes, SHA-256
  `d4bf7dffeda8f90b89f8e2af7bb32c293cd5b7d895388ac34691a238c1199f3e`
- `.build/queue-layout/baseline-webkit-boundary-761.png`, 205,161 bytes,
  SHA-256 `214295c0be45eaebb4233ee453bbf091e33a0c81226ff71c78a3438014e1f81a`

Corrected candidate artifacts (24 cases):

- `.build/queue-layout/candidate-results.json`, 533,269 bytes, SHA-256
  `df7c2e128348f4b2d1e8336377e16357c550b499a773cad4b9dc8e1cd1cb253d`
- `.build/queue-layout/candidate-webkit-boundary-761.png`, 173,054 bytes,
  SHA-256 `ef7d53369a1298fbe3dc7286799a4db97d57b68fa4795c701f4aea446efdb914`

Built-production artifacts (24 cases):

- `.build/queue-layout/built-results.json`, 533,110 bytes, SHA-256
  `3d961f738c6667bb43c44d18b62e888a173122993869d8eecb11b2b3bd32b3e0`
- `.build/queue-layout/built-webkit-boundary-761.png`, 173,054 bytes,
  SHA-256 `ef7d53369a1298fbe3dc7286799a4db97d57b68fa4795c701f4aea446efdb914`

These ignored `.build` artifacts contain only synthetic fixture output. They are
review evidence, not release assets.

## Validation

Exact clean source `699ecd42eca6193e81b5e10ffbcd6884b6e2423a` passed:

- `QUEUE_LAYOUT_BASELINE=1 node tests/queue-layout-browser.mjs` — 24/24
  Chromium/WebKit cases reproduced all three owner-observed baseline failure
  classes; this run pins both exact baseline Queue-owned files.
- `node tests/queue-layout-browser.mjs` — 24/24 candidate cases had no geometry,
  browser, text/control overflow, selection, focus, hit-test, or scroll failure.
- `npm run build:static` — clean Vite production build and packaging passed;
  `npm run verify:release` verified all 82 assets for exact commit `699ecd4`.
- `QUEUE_LAYOUT_BUILT_CSS=dist-static/assets/index-fPZ6XyzZ.css node
tests/queue-layout-browser.mjs` — 24/24 Chromium/WebKit cases passed against
  the emitted 102,887-byte production stylesheet, SHA-256
  `a1094a0a8d4d4b768274449555ac94018ad6e27f67ba135e9540b674ffdc62b4`.
- `node tests/project-queue-browser.mjs` — Chromium/WebKit existing real
  isolated-hub Send, priority, exact uncertain retry, Pull, history,
  terminal-history, unsupported-hub, and no-launch checks passed.
- `node tests/project-queue-review-browser.mjs` — Chromium/WebKit Queue
  connection/view epochs and distinct uncertain actions passed.
- `node tests/project-queue-vault-browser.mjs` — Chromium/WebKit encrypted,
  bounded, connection-scoped, newer-edit-safe Queue intent checks passed.
- `npm test` — 149/149 JavaScript unit tests passed.
- `npx prettier --check client/queue.css client/queue-view.js
tests/queue-layout-browser.mjs`, `git diff --check`, and JavaScript syntax
  checks passed.

The clean release manifest is 14,861 bytes, SHA-256
`515720a5d2757bbe15ce34fb2f74c1b679ccbae1c884f710e34833bfac050550`.
The source files tested at the application commit are:

- `client/queue.css`: 5,841 bytes, SHA-256
  `1ae85bdd8eee520aa7cf960069b130725fd9e69686852c58f20d794b43606195`
- `client/queue-view.js`: 20,158 bytes, SHA-256
  `71fbd80d5a5a87681ffa07ec5c9e3b75005e0a8b6948dc9ea240a0d82aa68b93`
- `tests/queue-layout-browser.mjs`: 20,513 bytes, SHA-256
  `e822ff89798a14300cea7ee6d48002bf839107563e92779e9708ceadf491e7dd`

The static build retained Vite's pre-existing large-chunk advisory. `npm ci`
reported three moderate dependency advisories while preparing the original
candidate; no dependency or lockfile change was authorized or made. No nonpass
remains in the corrected matrix or relevant regressions.

## Limitations and rollback

Playwright WebKit is not native Safari. No native Safari version, owner-device
viewport, zoom, or DPR is known, and no owner-device confirmation is claimed.
The source-level and emitted-production-CSS fixtures establish repeatable engine
geometry, not the exact cause of an already-open native Safari session.

Rollback is source-only: revert correction commit `699ecd42e...` and original
application commit `952f36bf...`; `2d056055...` is documentation only. No
migration, database rollback, hub/CLI rollback, service restart, profile cleanup,
or production action is involved. Deployment remains subject to independent lead
review and a later explicit release order.
