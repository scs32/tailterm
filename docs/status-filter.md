# Bugs and Features status-filter commit

Bug `wi_a65c688c4b09f458`, revision 3, bounded work order
`wi_a65c688c4b09f458-status-filter-1` recorded in message #1165. The clean source
base is `310130f0b4564ab38dd796b4d2fd18247a6f7647` on `fix/status-filter` in the
dedicated Stephens-Mini worktree.

## Diagnosis and implementation

The status filter already handled the native `change` event and asked the shared
refresh-presentation layer to commit the control. That commit released the native
select hold, but not the initiating pointer hold. A platform picker can consume
the pointer release that commits its option, leaving no document `pointerup`.
The filtered data then arrived but its repaint remained queued until the next
outside click supplied another pointer sequence. This consumed-release path is a
synthetic causal reproduction; the exact owner browser's event trace was not
captured.

`client/view-refresh-presentation.js` now treats an explicit select commit as the
end of both the native-select and initiating-pointer holds. It clears any pending
pointer-release delay before flushing/reloading, while the existing matching
document `pointerup` path remains unchanged. `client/work-items-view.js` retains
its native `change` handler. No CSS, dependency, backend, schema, cache/history,
or global integration code changed.

## Focused evidence

The expanded isolated-hub fixture in `tests/dropdown-continuity-browser.mjs`
adds three cases for each of Bugs and Features:

- a labelled synthetic pointer activation followed by `input`/`change`, with no
  document `pointerup`, models the release consumed by the platform picker;
- the ordinary matching document `pointerup` path verifies the same filtered rows;
- real Playwright engine keyboard typeahead selects In progress on the focused,
  closed native select and verifies the filtered result.

Against clean base `310130f0b4564ab38dd796b4d2fd18247a6f7647`, the consumed-release
cases fail because the original item remains visible until a later pointer
sequence. Against the corrected candidate, all twelve focused cases pass across
Chromium and WebKit (two views, three interaction paths, two engines). The complete
dropdown/disclosure regression passes 50/50 across both engines, including native
pointer activation, retained control identity/focus during background refresh,
outside dismissal, mobile layout, disclosures, Board and Projects. The updated
refresh-presentation unit suite passes 10/10 and the full JavaScript unit suite
passes 130/130 on the corrected candidate.

The consumed-release event sequence is synthetic causal regression evidence, not
a claim of native OS menu automation or an observed trace from the owner's browser.
Playwright cannot reliably select an option inside the macOS-native popup layer on
this host. The suite directly verifies pointer opening/continuity/outside dismissal
and closed-select keyboard typeahead in Chromium and WebKit; no `selectOption()`
result is presented as native acceptance.

Ignored evidence is retained under `.build/dropdown-continuity/` for the
`pointer-baseline-before`, `status-pointer-focused`, and the final corrected
`status-pointer-final` candidate run. All hub state, browser contexts, and
temporary databases were isolated; fixture processes were stopped. No live
task/profile data or owner browser state was used.

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
