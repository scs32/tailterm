# Bugs and Features status-filter commit

Bug `wi_a65c688c4b09f458`, revision 3, bounded work order
`wi_a65c688c4b09f458-status-filter-1` recorded in message #1165. The clean source
base is `310130f0b4564ab38dd796b4d2fd18247a6f7647` on `fix/status-filter` in the
dedicated Stephens-Mini worktree.

## Diagnosis and implementation

The Bugs and Features status filter listened only for `change`. A native picker
can expose its committed value through `input` before deferring `change` until
blur, so that event sequence left the old rows visible until the next outside
click. The refresh-presentation layer was behaving as designed: it retained the
active native control to protect it from background refreshes.

`client/work-items-view.js` now applies status selection on either `input` or
`change`. The shared handler commits the presentation boundary, compares the
selected value with current filter state, and reloads only for a new value. A
later duplicate `change` therefore neither repeats the read nor disturbs the
native-control continuity contract. No presentation-helper, CSS, dependency,
backend, schema, cache/history, or global integration code changed.

## Focused evidence

The expanded isolated-hub fixture in `tests/dropdown-continuity-browser.mjs`
adds two cases for each of Bugs and Features:

- a labelled synthetic `input`-only sequence, with no `change`, blur, or outside
  click, reproduces the delayed filter boundary precisely;
- real Playwright engine keyboard typeahead selects In progress on the focused,
  closed native select and verifies the filtered result.

Against clean base `310130f0b4564ab38dd796b4d2fd18247a6f7647`, both Chromium
input-only cases failed because the original item stayed visible. Against the
candidate, all eight focused cases pass across Chromium and WebKit (two views,
two interaction paths, two engines). The complete dropdown/disclosure regression
passes 46/46 across both engines, including native pointer activation, retained
control identity/focus during background refresh, outside dismissal, mobile
layout, disclosures, Board and Projects. The refresh-presentation unit suite
passes 8/8 and the full JavaScript unit suite passes 128/128.

The synthetic input-only sequence is causal regression evidence, not a claim of
native OS menu automation. Playwright cannot reliably select an option inside
the macOS-native popup layer on this host. The existing suite directly verifies
pointer opening/continuity/outside dismissal and directly verifies closed-select
keyboard typeahead in Chromium and WebKit; no `selectOption()` result is presented
as native acceptance.

Ignored evidence is retained under `.build/dropdown-continuity/` for the
`baseline-before`, `candidate-focused-final`, and `status-filter-final` runs. All
hub state, browser contexts, and temporary databases were isolated; fixture
processes were stopped. No live task/profile data or owner browser state was used.

`node tests/board-layout-browser.mjs` was also attempted, but this clean worktree
does not contain the generated `wasm/tailserve.wasm` required by that fixture's
Vite bootstrap. Its first case timed out before the changed view loaded, so it is
recorded as an environment/setup nonpass and not as product evidence. The repeated
cases were stopped; no served package or production process was changed.

## Release state

No deployment was performed. Work order #1165 permits TailOS and the existing
Mini preview only after lead accepts and sequences an exact candidate release.
The existing Mini PID 28664 and its served `dist-static` bytes were not touched.
Air, TrueNAS, Tailscale, hub/CLI/relay configuration, the old Tailterm site, live
tasks, and owner profiles were unchanged.
