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

## Actual frontend release

Lead accepted exact source `a974c98` in Board message `1761`. The Database
handler then committed and read back separately bounded frontend release order
`1763` under the same Bug revision and original implementation order `1726`.
Handler messages `1769` and `1770` verified receipt
`mpr_90930733b718d031` and the complete 3,758-byte order text, SHA-256
`baeeafc40a523485c5121be218a5a73da30a0697298b0b9f3c98e647c9b7dfdd`.
The retained verification file is
`/tmp/tailterm-queue-layout-release-order-verified-1766.json`, 12,790 bytes,
SHA-256 `475caae0eaf33ccf59b25f01b35ad1bd3e228cbbb62481275df154d061507cfd`.

### Clean package and rollback

A standalone, tracked-clean checkout at exact application `a974c98` is retained
at `.build/releases/queue-layout-a974c98`. Its ignored dependency tree was
clone-copied from the previous retained release only after both `package-lock.json`
files matched SHA-256
`a4175ff8886d6c2ee8443e7a928bc1f6f9500cd3b2c24b55fb967175d9eb3f98`.
No Agents-library source is present.

`npm run build:wasm` freshly fetched the pinned Tailscale 1.102.3 archive and
verified archive SHA-256
`0e94d961c31ce7d33e8b7ce4ac6fdbec83ee5658784eed69eb7fce300729d717`.
The build used the upstream Go 1.26.6 directive and produced:

- raw production WASM: 37,951,858 bytes, SHA-256
  `8e54cce586f20e9ae03dd7b8315bc100819c81818a8896b06bbd29aa183b4af6`;
- packaged `assets/tailserve-Dc3Y6ELU.wasm.gz`: 8,598,270 bytes, SHA-256
  `3b24253d0113365e41a1fec077ba68ea3da7cc3a6ca9e2277b9a0d1cb891680f`;
- 305-entry module inventory: 41,520 bytes, SHA-256
  `625c3372eeea0b69c0c73ddc4b5f21012facc705c87315e6ef961923e72e0b26`.
  Its release-path-normalized SHA-256
  `beccf5f0c0c61f0fdd462185e47a79ea2c85047fa6edfa41856b7f368bfb0282`
  exactly matches the previous retained release inventory.

The fresh static package passed `npm run verify:release`: exact clean commit
`a974c98`, 82 manifest entries, of which 81 are public files and `_headers` is
hosting configuration. Public bytes total 80,106,528. Exact release assets are:

- `release.json`: 14,861 bytes, SHA-256
  `7eb67bf14d940dee93792b624a2665ce3e53d155c6171956c90fe9f3faa30170`;
- `assets/index-D55uo5kC.css`: 103,270 bytes, SHA-256
  `4b0a3ac5459db6f96e28c0fe34bd879da9db411e45cbc70ff63602d88ff113c1`;
- `assets/index-Cuh6-kWE.js`: 863,312 bytes, SHA-256
  `28c567889867fcb723339e986acda2a559838b50bd31554a7e710d598ed417fb`.

Before publication, the retained previous package
`.build/releases/project-queue-cfb2172/dist-static` passed its own 82-entry
verification and matched the Mini live directory byte for byte. Its release
manifest SHA-256 is
`e7919f9448fa0dc9d5938315c6747dbc9c035645398c48c2ce0ff34e21a234a4`.
It is staged at `.build/static-before-queue-layout-cfb2172`; the actual swapped
live directory is additionally preserved at
`.build/static-live-before-queue-layout-cfb2172-swap`.

### Publication and origin verification

The exact retained package was published with the order-specified command:

```sh
npx wrangler pages deploy dist-static --project-name tailos --branch main --commit-hash a974c9870b64d0e4cb5387011d1a68515af6ce82 --commit-dirty=false
```

This created production deployment
`876448a7-d913-4e68-9033-522230a57558` at
`https://876448a7.tailos.pages.dev`. `--branch main` selected the existing
Cloudflare Pages production branch; Git stayed on `tasks-hub`. The custom origin
is `https://tailos.tailarr.com`. The old `tailterm` Pages project was not touched,
and `npm run deploy:static` was not used.

For Mini, an exact clone-copy of the retained package was staged beside the live
directory and `renameatx_np(RENAME_SWAP)` atomically exchanged them. The existing
preview remained PID 60799 / PPID 1, with the same listener on
`127.0.0.1:4318`; it was never stopped, restarted, or reparented.

The immutable Pages URL, custom TailOS domain, and Mini preview each returned the
exact release-manifest hash above. A complete verifier then fetched `release.json`,
canonical root index, and all 81 public assets from every origin with identity
encoding and checked HTTP success, byte count, SHA-256, and each origin's MIME
contract. `_headers` was correctly excluded. All 243 asset fetches and all three
root/release checks passed. The full synthetic evidence is
`.build/queue-layout-release/public-assets.json`, 73,179 bytes, SHA-256
`ef3c4b7df74d3c0c7a968eac5bd367c11a0a246c9c9ba48038e02faa49c1464d`.

Fresh disposable Chromium contexts on all three origins unlocked only a synthetic
local vault, generated and restored a synthetic key, verified layout margins,
started the production Tailscale WASM, and reported no page errors. No live hub
configuration or hub request was used. The script's `https:true` output field is
hard-coded even for an explicitly supplied origin; Mini remained correctly
verified as HTTP, not HTTPS.

Finally, the actual CSS was downloaded independently from each origin. All three
copies matched SHA-256
`4b0a3ac5459db6f96e28c0fe34bd879da9db411e45cbc70ff63602d88ff113c1`.
Each copy separately passed all 24 Chromium/WebKit cases in the accepted fixture:
real production composition, 761px/content-width stacking, long text/IDs,
selection fill, hover/focus retention, keyboard/pointer selection movement,
compact label-associated checkbox toggling, bounds, and scrolling. Results:

- immutable URL: 582,658 bytes, SHA-256
  `193840911e61c6dd8ef0dfa8a482bfd423a22d6952912de3990a9e05addcf051`;
- custom domain: 582,658 bytes, SHA-256
  `51c9b032ab4b1721d00e67453045e955aa453b3244f0fc0a8b0254f7e7918f29`;
- Mini: 582,656 bytes, SHA-256
  `ada9ff8a97cfe029253986c809bd09fe18806a35412da4c1ca60bfdbb3121d54`.

The complete machine-readable receipt is
`docs/releases/tailos-2026-09-10-queue-layout.json`.

### Release-stage corrections and nonpasses

- A read-only prerequisite hash command initially referenced nonexistent
  `wasm/go.mod` and `wasm/go.sum`. It stopped without mutation; the actual module
  inventory generated by `build-wasm.sh` was then verified.
- A direct module inventory diff initially differed because the retained files
  embed different absolute release-directory paths. Normalizing only that path
  prefix produced identical 305-entry inventories and the matching hash above.
- The first public-asset verifier pass expected `text/plain` for Mini license
  `.txt` files. The unchanged long-lived preview uses its octet-stream fallback
  for unmapped text extensions. The verifier was corrected to accept this
  existing per-origin MIME contract; no served file changed, and the full rerun
  passed.
- Vite retained its existing large-chunk advisory. The earlier three moderate
  npm audit advisories and every original implementation/build nonpass remain
  recorded above. No dependency or lockfile change was made.

## Limitations and rollback

Playwright WebKit is not native Safari. No native Safari version, owner-device
viewport, zoom, or DPR is known, and no owner-device confirmation is claimed.
The source-level and emitted-production-CSS fixtures establish repeatable engine
geometry, not the exact cause of an already-open native Safari session.

Frontend rollback publishes exact retained package
`.build/releases/project-queue-cfb2172/dist-static` to Pages project `tailos`,
then atomically swaps the identical staged package into Mini without stopping PID 60799. The rollback identity is application `cfb2172ae810...`, deployment
`552de3bf-9453-4992-b0fb-1bf12d84dcd3` /
`https://552de3bf.tailos.pages.dev`, release SHA-256
`e7919f9448fa0dc9d5938315c6747dbc9c035645398c48c2ce0ff34e21a234a4`.

Source rollback reverts selection correction `a974c987...`, geometry correction
`699ecd42...`, and original application commit `952f36bf...`; intervening report
commits are documentation only. No database, hub, CLI, schema, provider,
TrueNAS, Tailscale, network, relay, Air preview, profile, vault, old-site, or
agent-lifecycle change accompanied this release. Actual release remains pending
independent lead verification, handler evidence retention, and ordered completion.
