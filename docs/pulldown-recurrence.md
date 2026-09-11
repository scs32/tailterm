# Repeated picker Escape and Board composer focus

Bug `wi_85a66e159b50a309`, admitted revision 8/scope 6. Recorded bounded
order **#1888**, original lead **#1886**, selection #1878. Dedicated worker
`pulldown-recurrence`, agent `agt_769593e945e4452d`, run
`run_92705aafd2c988e3`, on Stephens-Mini.localdomain in
`.build/worktrees/pulldown-recurrence`, branch `fix/pulldown-recurrence`.
Admitted context cutoff #1893, SHA-256
`cff23897a58e317196e04d8bc70b8acb47f917463446df480345bd411ca45de6`.
Handler #1903 confirmed separate Queue Start receipt
`qrr_58c3af3a3e0b940d`, entry `que_bda73d2596f58590`, cycle 1/revision 7,
ACTIVE, preserving this exact item/order/agent/run binding. Diagnosis began only
after that confirmation. Required AGENTS.md, handoff and overview were read.

Baseline: `84daa9c7ea46bf28cf407ce43d60eb4d87d05816`, initially clean.
Application/test candidate: `b10bcd462396661ea688e7eb49a1c4d9b906bec2`.
This is a candidate report, with no integration, deployment, owner-device pass,
item completion or database completion confirmation claimed.

## Original evidence and scope

The owner's recurrence report, relayed by lead #1473, was:

> Can you please either file or reactive the pulldown bug. It's better and usually works the first time, but afterwards it gets glitchty again. Also there appears to be an extra set of lines. Please see the screenshot

The owner separately supplied “an example of the odd double line issue that is
happening around the UI,” relayed by lead #1560 and retained by handler #1564.
The exact supplied image is
`/Users/stephenspeicher/projects/tailterm/Screenshot 2026-09-10 at 9.14.39 AM.png`,
159955 bytes, SHA-256
`dbcedf3da21edc7b2bbf45f7472ecd4fa64729e5b2ca9a1a5e2b71e4e98a7c1b`.
This worker visually inspected it: Board reply composer, Replying to #1539,
recipient lead, an orange inner border and separate outer focus stroke.
Those messages are attributed relays of owner evidence, not original human Board
posts or proof of a shared picker/CSS cause. The related Random Flashes item was
not investigated.

The prior accepted Enter/Escape repair and its complete historical reports,
acceptance and releases remain intact in admitted history (original order #1358,
acceptance #1387/#1415, report revisions 3/4 and bounded Done revision 5).
That work did not reproduce physical native option selection; this report does
not upgrade that claim. The new reproduction concerns repeated Escape cancellation.

## Demonstrated repeated failure

The new fixture imports the actual Board and Bugs/Features views, actual shared
dialog interaction helper and production styles. Dialog construction mirrors the
shared wrapper; bootstrap/main.js is not loaded. All client records are in-memory
synthetic values, network requests are restricted to the disposable local fixture,
and browser contexts are new. No work-item or Queue API, live hub, profile,
owner storage or database was used.

The fixture clicks the native editor Status/Priority select, observes actual
`:open` and focus, refreshes the underlying actual view, presses Escape, then
checks dialog liveness. Successful cycles also use explicitly synthetic
`selectOption` commits. On premature baseline closure it reopens the editor and
verifies draft recovery before continuing. Each control is attempted ten times.

| Baseline view | First failed Status cycle | Premature closes / 20 attempts |
| --- | --- | --- |
| Chromium Bugs | 3 | 9 |
| Chromium Features | 3 | 9 |
| WebKit Bugs | 2 | 10 |
| WebKit Features | 2 | 10 |

The decisive browser trace at each first failure is:

1. Before and after refresh: focused `work-item-status`, picker `:open=true`.
2. Escape `keydown`: `cancelable=true`, focused picker remains open.
3. Dialog `cancel`: **`cancelable=false`**, focused picker remains open.
4. Existing guard calls blur; `focusout` follows, but the dialog closes anyway.

Earlier surviving cycles have `cancel.cancelable=true`. Thus the old guard was
not simply missing the open state: preventing default on the later non-cancelable
cancel event cannot stop closure. Browser internal reasons for switching the
cancelability flag are not asserted. This event-level reproduction occurred in
both engines and does not require a hypothesized shared CSS cause.

Separately, all ten cycles of Board recipient, Bugs project/status and Features
project/status (100 cycles over two engines) preserve the control and open picker
through queued refresh. Outside dismissal eventually displays the latest state.
Labelled synthetic pointerdown-without-pointerup plus option commits preserve
selected values and unblock repaint. First, second and tenth traces are retained.
Protocol Escape alone is observed separately; outside click is used for reliable
nonmodal dismissal. The mobile-only project select is tested at 600px; other
controls use 1100px. No refresh-helper defect was established, so it is unchanged.

## Narrow repairs

`client/dialog-interaction.js` reuses the existing open-single-select guard on
**Escape keydown**, before the browser's dialog close default. It blurs the open
picker and restores focus on the next frame using `{preventScroll:true}`. A
closed picker does not consume Escape, so subsequent ordinary Escape closes the
dialog. The original cancel fallback and Enter/showPicker/programmatic-click
paths remain. No form submission, view refresh, storage or bootstrap code changed.

`client/style.css` changes only the Board composer textarea focus outline offset
to `-2px`. The existing solid 2px accent outline now overlaps the existing border
instead of forming a separate outer rectangle. Baseline computed geometry in
both engines: 1px accent border, 2px accent outline, 3px outer offset, no shadow.
Candidate: same border and 2px visible outline, -2px offset. The textarea bounds
remain 854×70 CSS pixels in the 1100px fixture. Unfocused outline style remains
`none`. Keyboard Tab focus, draft text and selection survive refresh. The fixture
uses the default green theme rather than the owner's private orange settings;
the double-stroke geometry matches. Baseline and candidate screenshots were
visually inspected. This does not remove focus from other UI controls or claim
a broader Random Flashes repair.

Only those two production files, focused tests and this report changed.
No main.js, task-hub.js, bootstrap, schema/types, briefing files, networking,
Tailscale, TrueNAS, relay, deployment, lifecycle, helper or live-data changes.

## Verification and nonpasses

- New browser baseline matrix completed on exact baseline source: **38/80**
  premature editor closes observed; both Board focus geometries reproduced.
- Candidate `node tests/pulldown-recurrence-browser.mjs`: Chromium and WebKit
  both pass all **80 editor open/Escape attempts**, synthetic value commits,
  draft retention, ordinary final Escape, **100 nonmodal cycles**, queued/latest
  refresh, focused/unfocused composer geometry and keyboard focus/selection.
  No page errors. First/second/tenth observations are retained.
- New keydown unit test fails on baseline (8 old tests pass, new test fails),
  then passes on candidate. Focused dialog/presentation unit tests: **20/20**.
- Existing `node tests/dropdown-dialog-browser.mjs`: **4/4** Bugs/Features ×
  Chromium/WebKit. Retains Enter behavior, synthetic commits, drafts, focus
  without scrolling, ordinary Escape and close-button dismissal. WebKit protocol
  Enter still does not expose an opened picker; no improvement is claimed there.
- `npm test`: **156/156** pass. `git diff --check` and focused formatting pass.
- **Nonpass:** extra `node tests/work-item-draft-vault-browser.mjs` aborted in
  Chromium with `page.evaluate: TypeError: Cannot read properties of undefined
  (reading 'includes')`, call site line 129. No encrypted draft-vault two-engine
  pass is claimed. No storage code changed; discovery was routed to lead #1914
  and db-handler #1915, without expanding this repair into storage/fixture work.
- Lead follow-up **#1916** requested baseline comparison within the same order.
  Extracting exact `84daa9c` with `git archive` into
  `.build/pulldown-recurrence/baseline-source` and running the unchanged command
  with the same host dependencies/new isolated context produces the identical
  Chromium exception. Precise failure: `JSON.stringify(envelope).includes(...)`
  in the encrypted-at-rest result fields; `envelope` is undefined. The fixture
  reads IndexedDB object-store `vault` key `encrypted`, whereas the unchanged
  current vault implementation writes/reads `encrypted-v2`. Both baseline and
  candidate fixture SHA-256 are
  `d7ecc5bb2e8c139061f8103a8b94694ee92ebc0e92334acd1120bd05dc760417`;
  both `client/local-vault.js` hashes are
  `114ac4c4401c37c9e6c8461f0017b9ec15a565b1185a4fa255dc1df733f1f13e`.
  This prevents the evaluated result from returning and the subsequent
  reload/restoration assertions and WebKit execution. First-stage assertions ran,
  but the full encrypted-restoration check remains unverified. It is not an
  observed draft-loss regression. No fixture/storage repair was made. Raw
  baseline log: `/tmp/pulldown-draft-vault-baseline.txt`.
- Fixture development corrections: the first Tab assertion assumed recipient
  followed textarea; DOM order differs. WebKit's default Tab path also skips the
  submit button, so the final cross-engine check tabs from recipient to textarea.
  The desktop-hidden project control initially timed out; its checks now use the
  real mobile layout. These were fixture assumptions, not product fixes.
- Vite reoptimized dependencies for the existing dialog suite and selected an
  alternate occupied default port; it then passed. Temporary fixture servers and
  browser contexts closed. No persistent service was launched or changed.
- No static package build or deployed acceptance was attempted under this
  deployment-excluded order.

Evidence retained relative to this worktree:

| Artifact | SHA-256 |
| --- | --- |
| `.build/pulldown-recurrence/baseline/evidence.json` | `bdc94f2123d4286f055ff96123616a6f6564409f79df9870f595ac70c16201bc` |
| `.build/pulldown-recurrence/candidate/evidence.json` | `5734b9103597efe638cb89311a0a4fe2ce7c411edee393a4d883e17af0933000` |

Each directory also contains Chromium/WebKit composer focus screenshots.
Raw logs remain `/tmp/pulldown-full-baseline.txt`,
`/tmp/pulldown-full-candidate.txt`, `/tmp/pulldown-baseline-trace.txt`,
`/tmp/pulldown-unit-baseline.txt`, `/tmp/pulldown-retained-dialog.txt`,
`/tmp/pulldown-npm-test.txt`, and `/tmp/pulldown-draft-vault.txt`.
These local convenience artifacts accompany the durable report's complete
findings, trace contract and counts; they are not external CI or owner evidence.
For baseline reproduction, run the new fixture against baseline source with
`PULLDOWN_BASELINE=1`; that flag changes expected results, not served source.
`PULLDOWN_MODE` and `PULLDOWN_ENGINE` optionally narrow diagnostics.

## Remaining reproduction and release dependencies

Playwright clicks/keys are protocol browser input, `:open` and cancelability are
browser observations, and `selectOption`/consumed pointer sequences are synthetic.
Native macOS popup option selection, physical keyboard/mouse/trackpad, native
Safari and owner-device confirmation remain untested. No owner Safari preferences,
Remote Automation settings or browser profiles were changed. The exact original
pointer-option symptom is still not directly reproduced or claimed fixed.

To resolve that remaining report, obtain the exact browser/version/OS, control
(Bugs/Features filter or editor Status/Priority, or Board recipient), pointer or
keyboard sequence, whether the option changes or is reselected, and first through
third interactions including dismissal method and whether the containing dialog
closes or only the picker does. A native Safari recording of those steps and the
visible refresh timing would distinguish this demonstrated Escape defect from a
separate pointer-commit failure. Preserve drafts while collecting that evidence.

Lead must review/accept this bounded candidate, and db-handler must save/read back
its report and attributed acceptance. Handler owns status/Queue records; neither
candidate success nor this report declares the reopened item complete. The failed
extra encrypted-vault suite and original native-pointer limitation remain visible.
Any integration/release requires a separate concrete recorded order and slot.
Then integrate on the current accepted tasks-hub root, preserve independent
handler-allocation work, run affected integrated checks, build a clean retained
package outside served dist-static and verify it. Only an authorized later release
may target explicit Cloudflare project `tailos` / `https://tailos.tailarr.com`
and the existing Mini preview, with compatible rollback, exact asset evidence and
no service restart assumptions. No deployment is authorized by this report.

## Release preparation under #1935 — prepared, not deployed

Lead accepted the demonstrated source scope in **#1934**; handler saved the full
attributed acceptance in #1938 and confirmed it in #1939. Complete earlier report
bytes were retained/read back through native revision-8-linked chunks #1930/#1931
and confirmation #1932. The Bug remains In progress revision 8/scope 6, with
original order #1888 and this exact run/context/ACTIVE Queue binding unchanged.
Lead then issued bounded preparation **#1935**. Handler **#1940** verified its
native current-primary record and full text, receipt `mpr_775431ac749ba41e`, before
package work began. This order permits preparation only; publication, served-file
swaps, root branch mutation and lifecycle changes remain excluded.

The clean retained source/package is
`/Users/stephenspeicher/projects/tailterm/.build/releases/pulldown-recurrence-43c1da3`.
It is a detached checkout at accepted source
`43c1da3a81e7e30c11efaa87b69f3903004cfbc2`, tree
`c128cd143ff89b48f7f72a58f04a804c3080f335`, containing application/tests
`b10bcd462396661ea688e7eb49a1c4d9b906bec2`. Root tasks-hub is still exactly
`84daa9c7ea46bf28cf407ce43d60eb4d87d05816`, the candidate's merge base. Its
unrelated untracked owner screenshot was preserved. Independent handler-allocation
work was neither included nor edited. This later report/plan commit deliberately
differs from the package's fixed accepted source; the package was not rebuilt
from documentation changes.

The complete machine-readable review plan is
[tailos-2026-09-10-pulldown-recurrence-plan.json](releases/tailos-2026-09-10-pulldown-recurrence-plan.json).
It contains exact source/package/toolchain inputs, build-log and evidence hashes,
critical asset hashes, compatible rollback identity, current Mini observation,
explicit proposed commands/targets and unfulfilled publication/acceptance steps.
It has no actual deployment ID and is not a release receipt.

### Package inputs and results

`npm ci`, `npm run build:wasm`, `npm run build:static` and
`npm run verify:release` all exited zero in the retained checkout. Node is
v26.5.1, npm 11.17.0. The host Go command is 1.26.5; the pinned Tailscale source's
`go.mod` selected/downloaded Go 1.26.6 automatically. The v1.102.3 upstream archive
passed the build script's pinned SHA-256 check
`0e94d961c31ce7d33e8b7ce4ac6fdbec83ee5658784eed69eb7fce300729d717`.
No dependency upgrade, audit fix, build-script change or test-WASM build occurred.
The module inventory has 305 lines, SHA-256
`9a762eb01e9bace18169a092a4eb2bac84835b420d5c8ecb3fa389f0fc2d1e57`;
its absolute module paths refer to this retained build. Production raw WASM
SHA-256 is `d61326d3bd19c5b486d9c53f36b435eef54e56a0dcab2d242fcb6b51f26c8e3a`.
Package-lock SHA-256 is
`a4175ff8886d6c2ee8443e7a928bc1f6f9500cd3b2c24b55fb967175d9eb3f98`.

The package manifest is `dist-static/release.json`, 14861 bytes, SHA-256
**`a560d5db9ce36d0eda86732b1765b82a5db60960ccc4de96d03149f767ca54da`**.
It records clean exact source 43c1da3 and build time
`2026-09-11T03:21:55.704Z`. All **82 manifest entries** match; 81 are public assets
and `_headers` is hosting configuration. Main JavaScript is
`assets/index-enCwsUvz.js`, 887069 bytes, SHA-256
`b189088d305c690346c61bba74d1cad3696d9c64d13a5cc7822a4168bdd2a7f4`.
Stylesheet is `assets/index-pPDH5eKA.css`, 103716 bytes, SHA-256
`f9cf645a47285b74bc63b0d97d8363f46e76dd567f1fb2b36670b19fed1a8906`.
The served production transport is `assets/tailserve-DiCacG8h.wasm.gz`, 8598265
bytes, SHA-256 `ac0d47cff00d0f26ab7e03faad505a8352700c84fa6a2168b8aea0550a418ae7`.

### Isolated preparation verification

- A disposable localhost server using the existing `createStaticPreviewServer`
  served the retained package. **81/81** public assets matched SHA-256 and size;
  responses were 200 without Content-Encoding, with the JS/CSS/WASM MIME contract.
- Existing `tests/deployed-browser.mjs`, explicitly pointed at the disposable
  loopback origin, passed real production bundle/WASM startup, isolated synthetic
  vault/key restoration, clipboard and three viewport layouts without page errors.
  It prints `https:true` even for an explicit localhost origin; this preparation
  evidence is correctly described as **HTTP loopback**, not TLS or deployment.
- A retained, ignored copy of the accepted recurrence runner routes its stylesheet
  requests to the exact compiled CSS bytes and suppresses duplicate source CSS.
  Chromium/WebKit pass **80 editor open/Escape attempts and 100 nonmodal cycles**,
  first/second/tenth observations, focus geometry, draft/selection and latest
  refresh. The fixture uses actual source view/helper imports plus production
  compiled CSS; it does not extract the helper from the minified application
  bundle. The separate production startup check above exercises that bundle.
- `QUEUE_LAYOUT_BUILT_CSS=dist-static/assets/index-pPDH5eKA.css node tests/queue-layout-browser.mjs`
  passes **24/24 Chromium/WebKit** cases, including boundaries, narrow viewports,
  keyboard/selection/scrolling and zoom-equivalent layout.
- Retained-source dialog, refresh and Agents library units pass **26/26**. The
  full 156/156 source suite and prior 4/4 dialog browser results remain the
  accepted candidate evidence; they were not redundantly rerun in preparation.
- Package/lock, Agents/Teams schemas, local-vault, Queue view/styles, task-hub,
  production WASM source seams and the build script are byte-identical to root
  baseline outside the two explicitly changed production seams. Their hashes
  are in the plan. No v2 Agents/Teams/Queue or hub/CLI contract change occurred.

The preparation runners/logs and distinct outputs are retained under the clean
package checkout's `.build/pulldown-preparation/`; Queue output is separately
`.build/queue-layout/built-results.json`. Earlier worker baseline/candidate evidence
was not overwritten. The disposable package server and all browser contexts
closed. No long-lived descendant service was created.

All earlier native Safari/physical option-selection limits and the identical
baseline/candidate encrypted-vault fixture nonpass remain. The separate production
synthetic-vault check passes but does not replace the failed work-item draft-vault
suite's unexecuted assertions. Build notices: npm reports three moderate audit
findings, deprecated `boolean` and six pending install-script approvals; Vite
reports its >500 kB chunk advisory. These did not fail the build, and no dependency
or host settings were changed to suppress them.

### Compatible rollback and proposed release

The retained rollback at
`.build/releases/agents-library-22ab000/dist-static` was independently verified
read-only with its existing release checker: clean source
`22ab0005795c9a1201766159e7b910fcff8e2a03`, **82/82 entries**, manifest SHA-256
`bd9c2ac5753ec8455f3c85ce5a94f5fe6d4955d049e57ce210ca59204dd886f3`.
Its accepted deployment is `53780e9d-bd87-43e9-8b21-a59c62b43430`,
`https://53780e9d.tailos.pages.dev`. Keep this v2-compatible Agents/Teams/Queue
frontend and the accepted Agents hub/CLI versions as the rollback boundary;
do not restore v1-only clients or overwrite later live data.

Read-only observations confirm existing Mini PID **60799 / PPID 1** still listens
on `127.0.0.1:4318` and serves that exact accepted Agents manifest. Its command is
`node scripts/preview-static.mjs dist-static 127.0.0.1 4318`. No served file,
process, hub/CLI install, network, Tailscale, relay or service setting changed.

After a separately recorded actual-release order and assigned slot, recheck the
current root before integrating only the accepted source/report, preserving any
independently accepted work. The proposed command runs in the retained clean
43c1da3 checkout:

```sh
npx wrangler pages deploy dist-static --project-name tailos --branch main --commit-hash 43c1da3a81e7e30c11efaa87b69f3903004cfbc2 --commit-dirty=false
```

The Pages branch flag selects its existing production deployment; Git tasks-hub
is not switched to main. The target is **tailos** / `https://tailos.tailarr.com`,
never tailterm. Record the returned immutable deployment ID/origin at that time.
For Mini, stage the byte-identical package beside the served directory, verify
all entries, then perform a macOS atomic directory swap while preserving the
existing listener and retaining the old directory. Reverify all served manifest/
asset hashes, MIME, the process identity and production synthetic-browser checks
on immutable/custom/Mini origins. No staging of served files, swap or publication
has been performed under preparation #1935. Actual-release evidence, lead
acceptance and handler saved confirmation are still required before completion.

## Actual release under #1970 — verified, pending final acceptance

Lead's **#1970** accepted the reviewed preparation/report and assigned this exact
worker the exclusive TailOS/Mini publication slot. Handler **#1976** verified the
native current-primary Work record and complete original text, receipt
`mpr_0af9104232f5fcb5`, before mutation. Original admission #1888, Start #1903,
item revision 8/scope 6 and this run/context remain unchanged. The complete
machine-readable actual receipt is
[tailos-2026-09-10-pulldown-recurrence.json](releases/tailos-2026-09-10-pulldown-recurrence.json).
The earlier preparation plan remains intact as historical pre-publication evidence.

Before publication, root was exactly 84daa9c and tracked-clean with the unrelated
owner screenshot preserved. Custom TailOS and Mini both returned the accepted
Agents22ab000 manifest `bd9c2ac5753ec8455f3c85ce5a94f5fe6d4955d049e57ce210ca59204dd886f3`;
retained compatible rollback passed all 82 entries again. No intervening release
was observed. Root `tasks-hub` was fast-forwarded only to accepted source/report
`9d1d57c553434c30d2b6095f6c239d10ef06fc76`. No handler-allocation changes were
integrated. The exact clean retained 43c1da3 package passed its release checker
again and was published without rebuilding.

Wrangler **4.131.0** executed the exact reviewed command above, successfully
uploading 21 files (61 already uploaded) and the packaged `_headers`. Cloudflare
Pages project **tailos** now serves deployment
**`621eb35c-7578-4c8c-9d4a-e16eea01d196`** at
**`https://621eb35c.tailos.pages.dev`** and **`https://tailos.tailarr.com`**.
The full ID/origin/source were read back using the Pages deployment list. The
published source is `43c1da3a81e7e30c11efaa87b69f3903004cfbc2`, manifest
**`a560d5db9ce36d0eda86732b1765b82a5db60960ccc4de96d03149f767ca54da`**.
The old tailterm site was not targeted. Deployment identity/package hashes were
returned promptly to lead in #1978 for parallel read-only acceptance.

### Mini activation and retained rollback

Read-only process inspection confirmed PID60799/PPID1's working directory was
`/Users/stephenspeicher/projects/tailterm`, serving its relative `dist-static`.
The exact retained package was clone-copied to sibling
`.dist-static-pulldown-recurrence-1970`; all 82 staged entries and the previous
live Agents package were verified. A same-filesystem
`renameatx_np(..., RENAME_SWAP)` atomically exchanged the stage/live directories.
The old served directory was then retained at
`.build/releases/pulldown-recurrence-previous-mini-1970`. Both new live and retained
old directories passed all 82 entries afterward. No listener stop/restart occurred:
**PID60799 / PPID1** remains the listener at **`http://127.0.0.1:4318`**.
Swap receipt time is `2026-09-11T03:33:03.070881+00:00`.

Immediate allowed rollback remains the exact v2-compatible Agents22ab000 static
package/deployment and that preserved previous Mini directory, whose manifest is
`bd9c2ac5753ec8455f3c85ce5a94f5fe6d4955d049e57ce210ca59204dd886f3`.
No rollback was needed or performed. Accepted Agents hub/CLIs, v2 profile
readability, all newer data, relay/network/Tailscale configuration, Air preview
and service processes remain unchanged. No backup restoration or live fixture
operation occurred.

### Actual deployed verification and nonpass correction

Each of immutable TailOS, custom TailOS and Mini passes **83 checks**: exact
release manifest, canonical index and all **81 public assets**, with expected
sizes, SHA-256 hashes and MIME types. Canonical requests explicitly use
`Accept: */*` and `Accept-Encoding: identity`; they have no Content-Encoding.
This is **249 successful origin checks**. Cloudflare versus Mini's existing
`.txt`/`.mjs` MIME differences are preserved in the checker, not changed in the
server. Precompressed `.wasm.gz` files remain `application/gzip` without an added
encoding. `_headers` is the verified package configuration, not a fetched asset.

Fresh isolated Chromium on **all three origins** passes production bundle/WASM
startup, synthetic vault/key restoration, clipboard and three viewport layouts,
without page errors. Public origins are HTTPS; Mini is HTTP loopback. Synthetic
browser contexts were separate and closed afterward; owner vaults/profiles and
live work-item data were not used. The exact served CSS/JS hashes match the
retained package whose compiled/source affected checks already passed. No
additional broad or unrelated test rerun was performed.

The first default-request asset probe was a **nonpass**, retained in the receipt:
ordinary `assets/browser-ssh-BI4XGVJZ.js` negotiated Brotli (`Content-Encoding: br`)
from Cloudflare. Its decoded bytes already matched the package, but the checker
incorrectly demanded no content encoding for every default request. Correcting
canonical requests to explicitly ask for identity encoding produced the passing
249 checks. Normal browser-negotiated JS compression is not package corruption;
no production header/configuration change or rollback was made for that assertion.
Lead was notified in #1979. This does not relax the precompressed WASM requirement.

Independent lead review **#1980** reports its own passing 249 origin checks,
fresh custom-origin Chromium production-WASM/synthetic-vault/clipboard/layout
check, and exact packaged CSS with actual-view two-engine **80 editor attempts
plus 100 nonmodal cycles**, preserving drafts/focus/selections. These checks
retain lead attribution, separate from builder execution. That review requested
the final report/receipt before final actual acceptance.

The identical baseline/candidate legacy-key encrypted-draft-vault suite nonpass
remains explicitly unresolved, despite successful separate production synthetic
vault checks. No native Safari, physical native macOS option selection or
owner-device pass is claimed. The demonstrated repair is repeated keyboard
Escape cancellation plus the independent composer focus geometry; the owner's
original exact pointer-option sequence remains unconfirmed.

Actual logs/results are retained under the package checkout's
`.build/pulldown-actual/`, including Pages log/readback, Mini swap, complete
per-origin asset/browser results, first probe nonpass and corrected runner.
Their exact paths, sizes and hashes are in the actual receipt. Production source
and packaged bytes stay fixed at43c1da3; this report, actual receipt and current
handoff are a later documentation-only commit.

### Exact worker preflight and remaining acceptance

The roster confirms `agt_769593e945e4452d` / `run_92705aafd2c988e3`, session
`pulldown-recurrence`, with original item/order/context. Tmux identity is **$53**,
creation **1789095768**, pane PID **26569**. Process inspection at
`2026-09-11T03:35:47.819378+00:00` found only the tt/node/Codex/code-mode host and
transient inspection processes in that tree, **no TCP listeners**. Mini60799 is
outside the tree, PPID1. No useful long-lived service was started by this worker,
and no worker/project closure was performed. The exact receipt is retained for
lead to reverify before any later lifecycle action.

Remaining: lead final actual-release acceptance, handler exact full report/receipt
and attributed acceptance save/readback, then handler-owned completion tracking.
This worker has not marked the item/Queue done and will not self-close under this
order. All original provenance, admitted history, limitations and cleanup ownership
remain intact.
