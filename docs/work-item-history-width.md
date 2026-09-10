# Work-item History dialog width

## Recorded scope

This release implements bug `wi_f44641a045d49ded` revision 2 under bounded
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
- Candidate-stage `npm run build:static` did not reach bundling because the
  isolated builder worktree had no generated `wasm/tailserve.wasm`. The exact
  clean release source later rebuilt production WASM and passed static packaging
  and release verification; details follow below.

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

## Integration and release evidence

Lead accepted the bounded source candidate in Board message #1576 and issued the
concrete width-first release order in #1577. The shared `tasks-hub` branch had no
tracked or staged change at exact baseline
`58f9185981625b148b30ad4b5070a1a658e17f77`; three untracked owner screenshot
files were preserved and excluded. Because the candidate's parent was that exact
baseline, integration was a conflict-free fast-forward to application/source
commit `9cd38d37cb7466e8fc57ccf2da994d8457b8be24`. No B1 change was included.

The exact clean source and package are retained at
`.build/releases/history-width-9cd38d3`. Production WASM was rebuilt from the
unchanged pinned Tailscale 1.102.3 source using the verified prior release cache.
The module identity list exactly matched the prior verified inventory; rebuilding
regenerated its paths inside the new retained tree. Relevant hashes:

- pinned Tailscale source archive:
  `0e94d961c31ce7d33e8b7ce4ac6fdbec83ee5658784eed69eb7fce300729d717`
- raw production `wasm/tailserve.wasm`:
  `dc841019c8a28670b3a44e0657f3a1e081648dbb561d5072eb735e987e577cb8`
- regenerated `.build/go-modules.txt`:
  `2c33e75b00b437e19377989a0fd36a631dd4a9cbd94b8fd17833ea4496e6778b`
- packaged release manifest:
  `671872613c224d7d9fc57275c18bbd68732872136ae10cb9dcf4e0c3cf10ae6f`
- package-lock:
  `a4175ff8886d6c2ee8443e7a928bc1f6f9500cd3b2c24b55fb967175d9eb3f98`

`npm run build:wasm`, `npm run build:static`, and `npm run verify:release`
passed. The clean Node v26.5.1 build records 82 manifest entries. The main
stylesheet is `assets/index-B-MR83xR.css`, 97,190 bytes, SHA-256
`d6878b64e2bfe9d1271d74e4aff645f3d5c7f34a43ad309d989c4a8eb3006627`;
the production WASM gzip is `assets/tailserve-qM5zcVoK.wasm.gz`, 8,598,273
bytes, SHA-256
`3fbd89103e04af9fdafe9a7f38da9100f5b9c71d39a5c82c4e5fa59dc8bbdf82`.
The build emitted only Vite's existing large-chunk advisory.

The exact release-source History suite again reproduced all 20 baseline cases
and passed all 20 candidate cases in Chromium and WebKit. The package was
deployed with the explicit Wrangler command to Cloudflare Pages project
`tailos`, production branch `main`, commit hash `9cd38d37…`, and
`--commit-dirty=false`:

- deployment ID `08760468-ec79-4953-bd07-18f51564d232`
- immutable origin `https://08760468.tailos.pages.dev`
- custom origin `https://tailos.tailarr.com`

The byte-identical retained package was synchronized only to the existing Mini
served `dist-static` directory. Detached preview PID 60799, PPID 1, its September
9 start time, cwd, command, and listener were preserved; no preview restart
occurred. Immutable TailOS, custom TailOS, and Mini each matched the release JSON
and all 81 served public assets by exact size, SHA-256, and origin-appropriate
MIME with identity encoding. The first verifier request allowed Cloudflare's
normal Brotli negotiation and therefore could not assert a null encoding header;
the read-only verifier was corrected to request identity bytes, then all three
origins passed without a deployment change.

Fresh disposable Chromium contexts on all three origins started the real
production WASM, generated and restored an isolated synthetic vault/key, retained
matching application margins at 1440×900, 1024×768, and 1920×1080, and reported
no page errors. Their layout screenshots have identical SHA-256
`dee008e5bb73ff69cb91fb1e43b1dc1241bc26b73201661e1427f62041b71664`.

Immediate rollback is the independently reverified clean source/package
`.build/releases/dropdown-dialog-8d47e9b`, application
`8d47e9b4b6c34cc1b9331517ba893156d28e23e8`, deployment
`9a5a97b3-c3ad-4b34-bc16-59a66632c7b9` at
`https://9a5a97b3.tailos.pages.dev`, and manifest SHA-256
`9280148ec37f1dcbbf51300ed04f973597abfeb7956da3bfdb21c4380480b17e`.
Rollback deploys that retained `dist-static` explicitly to Pages project
`tailos`, then synchronizes it to Mini without restarting PID 60799.

## Limits and final acceptance boundary

Playwright WebKit is not native Safari. No native Safari, owner device/profile,
physical input, or native browser-zoom control was used. The device-scale case is
explicitly a repeatable zoom-equivalent emulation, not a claim of native 200%
browser zoom. Deployment browser checks used only disposable synthetic vault
state; no live work-item/profile fixture or owner storage was read or changed.

No hub, CLI, schema, database, Tailscale, networking, TrueNAS, relay, Air preview,
old `tailterm.tailarr.com` site, live task/profile, or agent-session change
accompanied this release. B1 remained isolated and unaccepted during the sole
shared integration window.

This is a builder-verified actual release, not self-acceptance or a Done claim.
Lead actual-release verification and database-handler revision-checked report
storage/readback plus final acceptance remain required before completing the bug.
