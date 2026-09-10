# Work-item History dialog width

## Recorded scope

This candidate implements bug `wi_f44641a045d49ded` revision 2 under bounded
work order Board message #1551 and its complete supplement #1552. The admitted
context SHA-256 is
`9f544cf2768cebba7b4efa648d8d32ca8d7f6a1b9cb4901ed9ea16bedbaad753`;
the authorized branch baseline is
`58f9185981625b148b30ad4b5070a1a658e17f77`.

The owner's reference screenshot is
`/Users/stephenspeicher/projects/tailterm/Screenshot 2026-09-10 at 7.16.15 AM.png`,
SHA-256
`ff2971f69f5832d9a94a5f1253e977af8919d32b0b73dceab3f32f7e34bdf40c`.
It is reference evidence only. Its live history/profile content was not copied
into the fixture.

## Diagnosis and repair

The shared global dialog width is 480px. The classic Bugs/Features History view
places a 120–180px revision rail beside its detail, leaving little readable
space. More importantly, the rendered detail is `[data-history-detail]` with a
plain `pre`, and explicit links are `.work-item-history-messages button`. The old
`.work-item-history-detail`, `.work-item-history-description`, and
`.work-item-history-message p` rules did not match those elements. The browser
therefore retained `pre` formatting and let long identifiers and message text
overflow beyond the right edge.

`client/work-items.css` now:

- gives only `#dialog:has(.work-item-history)` a 900px preferred width, still
  clamped to 24px inside the CSS viewport;
- gives the actual history detail, descriptions, titles, metadata, and linked
  message buttons shrinkable widths and anywhere wrapping, while preserving
  description line breaks;
- lays explicit messages out as compact, left-aligned, auto-height controls; and
- keeps narrow revision choices at a usable, horizontally scrollable width
  after the existing one-column breakpoint.

No stored content, selection logic, History pagination, dialog JavaScript,
global stylesheet, editor, or Feature History & Report code changed.

## Failing-first and candidate evidence

`tests/work-item-history-width-browser.mjs` imports the actual shared
`createWorkItemsView`, full `client/style.css`, and either the candidate CSS or
the exact `client/work-items.css` read from baseline commit `58f9185`. It uses
only synthetic descriptions, unbroken IDs/URLs, linked messages, and disposable
browser contexts. The existing real throwaway-hub suite supplies the database
integration check.

The matrix covers classic Bug History and classic Feature History in Chromium
and Playwright WebKit at CSS viewports 1366×900, 1024×768, 640×720, and 390×720.
It also covers a 683×450 CSS viewport at device scale 2, equivalent in CSS
viewport/device-pixel terms to a 1366×900 display at 200% zoom. Each candidate
case checks geometry and rendered text ranges rather than chosen CSS values:
full exact detail/message text, no horizontal document/dialog/detail/text
overflow, viewport clamping, revision selection, linked-message navigation,
vertical scrolling, a reachable sticky close control, close-button and Escape
dismissal, and connected focus state. It also verifies the work-item editor and
the separate Feature History & Report retain the generic dialog width.

- `HISTORY_WIDTH_BASELINE=1 node tests/work-item-history-width-browser.mjs`:
  reproduced horizontal clipping/overflow in all 20 Bug/Feature × viewport ×
  Chromium/WebKit cases. At desktop widths both engines measured the 480px
  dialog's detail as 8,082px scroll width inside a 250px client width.
- `node tests/work-item-history-width-browser.mjs`: all 20 candidate cases
  passed wrapping, readability, selection, scrolling, close/Escape/focus, and
  dialog-scope checks. Desktop detail scroll/client width was 670/670px in the
  900px dialog; the 390px viewport measured 336/336px, and the
  200%-equivalent case measured 621/621px in both engines.
- `node tests/project-work-items-browser.mjs`: the real isolated throwaway hub
  passed Chromium and WebKit work-item CRUD, encrypted reload/retry/drafts,
  revision selection/race handling, 65-link pagination, dispatch, and closed
  reads.
- `node tests/narrative-history-browser.mjs`: Chromium and WebKit passed the
  separate safe Feature History & Report reader, reload, completed-feature
  access, coverage gaps, and draft retention.
- `node tests/dropdown-dialog-browser.mjs`: the unrelated Bug and Feature
  editors passed select, draft, focus, ordinary Escape, and close-button checks
  in Chromium and WebKit.
- `npm test`: 141/141 JavaScript tests passed.
- `npx prettier --check client/work-items.css tests/work-item-history-width-browser.mjs docs/work-item-history-width.md`:
  all owned files match project formatting.
- `git diff --check`: passed.
- `npm run build:static`: did not reach bundling because this isolated worktree
  has no generated `wasm/tailserve.wasm`. No production/static build pass is
  claimed, and no retained or deployed artifact was copied into the worktree.

The browser test writes ignored screenshots under `.build/history-width/`.
Selected evidence SHA-256 values:

- baseline Chromium Bug at 1366×900:
  `01d3733dbcf1e271e8b900c314b31c2383d4a5c667508ee72da157dfabd2c8be`
- candidate Chromium Bug at 1366×900:
  `4e206d186f677ba821f2381e733938556ede11d9a8728521149f1a16ed54406f`
- baseline WebKit Feature at 390×720:
  `a812bf650ec46cd10409d03d5e9498e512f2cd55e73265ae5caa055bcc49a913`
- candidate WebKit Feature at 390×720:
  `72c3f394d9e054a78365c409c3201ff028a77438ee05931ce8b940ae8c1a2a1b`
- baseline Chromium 200%-equivalent Bug:
  `65ad4989e8108915c08811a449550544dac132f8adeb427e92cde193de458cfd`
- candidate Chromium 200%-equivalent Bug:
  `a9d543a580cee268ff6109db5162c3326246ce9059acaa74226997a266c6365a`

The sorted 40-file baseline/candidate screenshot hash listing has aggregate
SHA-256
`07b4eaaf48801a134f6ddf9eec7eff7df46b84452deba7af969c5c6e276e4c05`.
Final formatted source SHA-256 values before commit are:

- `client/work-items.css`:
  `a4cf2b023e05544ad0117724cc35ed38dc23495e66859c3deeef9927261dac52`
- `tests/work-item-history-width-browser.mjs`:
  `7636fdd2bdc0edc9c7ee425d6d07344312e2dd9cd8a0409f0e86a279e2baa6f5`

## Limits and release boundary

Playwright WebKit is not native Safari. No native Safari, owner device/profile,
physical input, native browser-zoom control, production service, local preview,
or live work-item/profile data was used. The device-scale case is explicitly a
repeatable zoom-equivalent emulation, not a claim of native 200% browser zoom.
Static packaging remains unverified in this worktree because its generated WASM
prerequisite is absent; the browser fixture loaded the owned production CSS and
view source directly through Vite.

This is an unaccepted builder candidate. It was not integrated, deployed,
self-accepted, or marked Done. Lead review and database-handler revision-checked
result/acceptance storage remain required. Any later integration must serialize
with the accepted B1 shared changes and rerun the scoped CSS/UI checks on that
exact integration candidate. A separate reviewed release order must name the
TailOS and local-preview targets and cleanup ownership.
