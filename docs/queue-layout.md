# Queue layout repair

## Recorded scope and provenance

This candidate repairs Bug `wi_c36a4696771a2072` revision/scope 3 under bounded
Work Order Board message `1726` and lead selection `1717`. The immutable admitted
context was read in full before implementation:

- `/tmp/tailterm-queue-layout-context-1726-76fe4b649874.json`
- 51,518 bytes, SHA-256
  `76fe4b649874b94713c45a51218664a13dbca3b69d9a30ca0009bb77612aba9e`
- all three revisions, zero gaps, all four explicitly linked messages, and both
  full source messages (`1713` and `1717`)

Implementation began from exact clean commit
`4e8e6c5b38905b25b5173d0044560be45e5b6e88` on `fix/queue-layout` in the
isolated `queue-layout` worktree. The exact application/source candidate is
`APPLICATION_COMMIT_PENDING`; a later report-only commit records that immutable
identity without changing the tested application tree.

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

## Reproduction and root cause

The focused regression mounts the actual Queue view inside the production
`#workspace > main > #mode-view` hierarchy, retains the sidebar/header/mode
chrome, uses the complete application stylesheet order from `client/main.js`,
and supplies 18 synthetic Queue rows, multiple projects, a selected detail,
long titles and descriptions, unbroken identifiers, statuses, markers, and
controls.

The accepted baseline reproduced the three owner-observed failure classes in
both Chromium and WebKit:

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

This is a Queue-local production import/specificity mismatch. No global CSS
change is needed or included.

## Repair

`client/queue.css` now owns sufficiently specific Queue-only geometry after
global stylesheet composition:

- the outer Queue rail/thread and inner list/detail grids explicitly permit
  their tracks and children to shrink;
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

The fixture covers CSS viewports 1366×900, 1397×852, 1920×1080, 390×780, and a
683×450/DPR-2 constrained viewport modeling a 1366×900 display at 200% zoom.
The 1397/DPR-2 case is only a plausible comparison to the supplied raster, not a
claim about the owner's viewport. The zoom case is emulation, not native browser
zoom.

Representative WebKit measurements (pixels):

| Case            |         Baseline row right / detail left | Candidate row right / detail left | Baseline checkbox | Candidate checkbox | Candidate thread client / scroll height |
| --------------- | ---------------------------------------: | --------------------------------: | ----------------: | -----------------: | --------------------------------------: |
| 1366 desktop    |                         2730.70 / 830.42 |                   816.42 / 830.42 |             57.80 |                 14 |                              780 / 2471 |
| 1920 desktop    |                        2744.56 / 1054.80 |                 1040.80 / 1054.80 |             57.80 |                 14 |                              946 / 1885 |
| 390 narrow      | stacked baseline with 10px usable thread |          384 / 6 (vertical stack) |             57.80 |                 14 |                              501 / 5289 |
| 200%-equivalent | stacked baseline with 10px usable thread |          677 / 6 (vertical stack) |             57.80 |                 14 |                              217 / 3320 |

Every candidate case asserts:

- rail/thread, header/content, and list/detail rectangles do not overlap;
- document width stays bounded and visible non-rail controls stay inside the
  mode viewport;
- long Queue text has no horizontal scroll overflow and every measured text
  range stays within its assigned rail or thread region;
- the narrow rail is internally horizontally scrollable without document
  overflow, and list/detail becomes a vertical stack;
- the Queue thread has a usable vertical viewport and actually scrolls;
- keyboard focus has a visible outline, Enter selects exactly one row, pointer
  selection remains hit-testable, and selected fill is retained;
- the checkbox remains 14×14, within ten pixels of its label text, and the full
  history label remains visible.

Baseline artifacts:

- `.build/queue-layout/baseline-results.json`, SHA-256
  `8ecd575c5c3aea37c1bc713b9c78afa8942ba2f604f28c9f76ae16c1a16f4872`
- `.build/queue-layout/baseline-webkit-desktop-1366.png`, SHA-256
  `f41372c2ea3c364f7bf012592761ce73bf1b75fb16d623efda557136dfa69682`

Candidate artifacts:

- `.build/queue-layout/candidate-results.json`, SHA-256
  `5f1edf0b17429f75136ea0f33bbc7efedfa5b2642227ae074908a63f17703623`
- `.build/queue-layout/candidate-webkit-desktop-1366.png`, SHA-256
  `d789961c7698d0eb91ca9db71c496592890152daf0d6da50ecb3f4cab29c9571`

These ignored `.build` artifacts contain only synthetic fixture output. They are
review evidence, not release assets.

## Validation

The exact final commands and hashes are filled by the report-only commit after
the application/source commit is clean. The completed checks include:

- baseline and candidate focused Queue layout runs in Chromium and WebKit;
- the same focused suite against Vite's emitted production CSS composition;
- existing real isolated-hub Chromium/WebKit Queue Send, priority, uncertain
  retry, Pull, history, terminal-history, unsupported-hub, and no-launch checks;
- the complete JavaScript unit suite;
- static production build and release-manifest generation;
- formatting and whitespace checks.

One pre-candidate static build stopped because the fresh worktree had no generated
`wasm/tailserve.wasm`; the pinned `npm run build:wasm` prerequisite then passed.
A subsequent build emitted the production CSS and completed packaging after
`npm ci` supplied worktree-local license sources. `verify:release` correctly
rejected that dirty-tree package; it is not claimed as release-ready. Final clean
build/verification evidence follows after the source commit.

## Limitations and rollback

Playwright WebKit is not native Safari. No native Safari version, owner-device
viewport, zoom, or DPR is known, and no owner-device confirmation is claimed.
The source-level and emitted-production-CSS fixtures establish repeatable engine
geometry, not the exact cause of an already-open native Safari session.

Rollback is source-only: revert the Queue layout application/source commit
recorded above. No migration, database rollback, hub/CLI rollback, service
restart, profile cleanup, or production action is involved. Deployment remains
subject to independent lead review and a later explicit release order.
