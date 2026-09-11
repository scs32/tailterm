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
