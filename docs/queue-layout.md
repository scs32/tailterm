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
candidate is `a974c9870b64d0e4cb5387011d1a68515af6ce82`, tree
`71f78a5bb789ce84b484900670a5a92078361bb0`. Its complete four-file
`+1003/-21` binary diff from the accepted baseline has SHA-256
`dd5941a284d7323a83b15ebbb95e525e924b0610e976f61a297003d30cda0ec0`.
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

Lead review Board message `1751` independently accepted that geometry correction
after all 24 exact baseline, source, and built cases passed in an isolated clone,
but rejected `699ecd4` as a source candidate because selected and unselected Queue
rows still computed the same background. The later global button background won
over the low-specificity Queue selection rule, so selection was only a border.
The test asserted ARIA selection but had not truthfully asserted the reported
selection fill.

Commit `a974c9870b64d0e4cb5387011d1a68515af6ce82` adds a Queue-scoped selection
fill and interaction assertions. Resting selected and unselected fills must
differ; unselected hover may change only the border outline; selected fill must
survive hover and keyboard focus; keyboard Enter and a real pointer click must
move the fill with the selected row. Clicking the compact history label must also
toggle its associated checkbox on and off. This remains within Queue CSS/test
scope and changes no functional Queue code.

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
- Queue row selection uses the shared green fill at Queue-specificity strong
  enough to survive production CSS order; selected and unselected hover/focus
  change the border outline without changing their respective fills;
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
  selection remains hit-testable, selected/resting and unselected/resting fills
  differ, and selection fill moves with Enter and pointer selection;
- selected fill survives hover/focus, while unselected hover retains its resting
  fill and changes only the border outline;
- the checkbox remains 14×14, within ten pixels of its label text, the full
  history label remains visible, and clicking the label toggles it on and off.

At the 320px Chromium viewport, selected rest computes to
`color(srgb 0.19902 0.407843 0.32451)` and unselected rest to
`color(srgb 0.100784 0.113725 0.104902)`. Unselected hover keeps the latter fill
while its border changes from `color(srgb 0.306196 0.32549 0.311373)` to
`rgb(185, 236, 196)`. Selected hover/focus keeps the selected fill.

Exact-baseline artifacts (24 cases, exact baseline CSS and Queue view):

- `.build/queue-layout/baseline-results.json`, 578,259 bytes, SHA-256
  `ad2cdd098f9a704aadc5e426280c99508de815b8a13e480396e825f403c12540`
- `.build/queue-layout/baseline-webkit-boundary-761.png`, 205,161 bytes,
  SHA-256 `214295c0be45eaebb4233ee453bbf091e33a0c81226ff71c78a3438014e1f81a`

Corrected candidate artifacts (24 cases):

- `.build/queue-layout/candidate-results.json`, 582,558 bytes, SHA-256
  `98634d7b835c9a4e05e77b4fad1b33b5f834dac45fa0a1b7bce3953ee6d75d5c`
- `.build/queue-layout/candidate-webkit-boundary-761.png`, 172,367 bytes,
  SHA-256 `2cd1375e43150e1592dc3f671e1f5b6729a554a1ce63eccae6b782b8fb2c5df3`

Built-production artifacts (24 cases):

- `.build/queue-layout/built-results.json`, 582,418 bytes, SHA-256
  `8e370d5afa82f1e9323fde957a432995c9caf6c846b7af521280f184574a1b24`
- `.build/queue-layout/built-webkit-boundary-761.png`, 172,367 bytes,
  SHA-256 `2cd1375e43150e1592dc3f671e1f5b6729a554a1ce63eccae6b782b8fb2c5df3`

These ignored `.build` artifacts contain only synthetic fixture output. They are
review evidence, not release assets.

## Validation

Exact clean source `a974c9870b64d0e4cb5387011d1a68515af6ce82` passed:

- `QUEUE_LAYOUT_BASELINE=1 node tests/queue-layout-browser.mjs` — 24/24
  Chromium/WebKit cases reproduced all three owner-observed baseline failure
  classes plus the missing selection fill; this run pins both exact baseline
  Queue-owned files.
- `node tests/queue-layout-browser.mjs` — 24/24 candidate cases had no geometry,
  browser, text/control overflow, selection-fill, hover/focus, keyboard/pointer,
  checkbox, hit-test, or scroll failure.
- `npm run build:static` — clean Vite production build and packaging passed;
  `npm run verify:release` verified all 82 assets for exact commit `a974c98`.
- `QUEUE_LAYOUT_BUILT_CSS=dist-static/assets/index-D55uo5kC.css node
tests/queue-layout-browser.mjs` — 24/24 Chromium/WebKit cases passed against
  the emitted 103,270-byte production stylesheet, SHA-256
  `4b0a3ac5459db6f96e28c0fe34bd879da9db411e45cbc70ff63602d88ff113c1`.
- `npx prettier --check client/queue.css client/queue-view.js
tests/queue-layout-browser.mjs`, `git diff --check`, and JavaScript syntax
  checks passed.

Unchanged functional-regression evidence retained from geometry candidate
`699ecd4` (the later correction changes only presentation CSS and the focused
test, and lead message `1751` did not require broad repetition):

- `node tests/project-queue-browser.mjs` — Chromium/WebKit existing real
  isolated-hub Send, priority, exact uncertain retry, Pull, history,
  terminal-history, unsupported-hub, and no-launch checks passed.
- `node tests/project-queue-review-browser.mjs` — Chromium/WebKit Queue
  connection/view epochs and distinct uncertain actions passed.
- `node tests/project-queue-vault-browser.mjs` — Chromium/WebKit encrypted,
  bounded, connection-scoped, newer-edit-safe Queue intent checks passed.
- `npm test` — 149/149 JavaScript unit tests passed.

The clean release manifest is 14,861 bytes, SHA-256
`3fea9a25abc471e00dbcb0120aaab3a7e8073284749ff42fcab32c3452e4b82d`.
The source files tested at the application commit are:

- `client/queue.css`: 6,350 bytes, SHA-256
  `69efe8e60bacafe342efec5e010d91018967c360d08565f74a0ba31a9e4464b1`
- `client/queue-view.js`: 20,158 bytes, SHA-256
  `71fbd80d5a5a87681ffa07ec5c9e3b75005e0a8b6948dc9ea240a0d82aa68b93`
- `tests/queue-layout-browser.mjs`: 23,807 bytes, SHA-256
  `9b1e564005a4cffe74d09a3c45af13ee1df56fafa9fcb3d7f775a0f616ad6da3`

One pre-candidate static build stopped because the fresh worktree had no generated
`wasm/tailserve.wasm`; the pinned `npm run build:wasm` prerequisite then passed.
A subsequent build emitted production CSS and completed packaging only after
`npm ci` supplied worktree-local license sources. `verify:release` correctly
rejected that dirty-tree package; it is not claimed as release-ready. The final
clean build and verification then passed for exact `a974c98` as recorded above.
The static build retained Vite's pre-existing large-chunk advisory, and `npm ci`
reported three moderate dependency advisories. No dependency or lockfile change
was authorized or made. No nonpass remains in the final corrected matrix.

## Limitations and rollback

Playwright WebKit is not native Safari. No native Safari version, owner-device
viewport, zoom, or DPR is known, and no owner-device confirmation is claimed.
The source-level and emitted-production-CSS fixtures establish repeatable engine
geometry, not the exact cause of an already-open native Safari session.

Rollback is source-only: revert selection correction `a974c987...`, geometry
correction `699ecd42...`, and original application commit `952f36bf...`;
`2d056055...`, `b5c86af3...`, and this final report are documentation only. No
migration, database rollback, hub/CLI rollback, service restart, profile cleanup,
or production action is involved. Deployment remains subject to independent lead
review and a later explicit release order.
