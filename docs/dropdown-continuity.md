# Dropdown and disclosure continuity

Bug `wi_b1d07b59cdf519eb`, owner report #658, work order
`wi_b1d07b59cdf519eb-fix-1` (#661). Baseline application
`39896c9c3a1779bdd1b6eb77ad2edab6d447cacc`, repository HEAD
`c0396e10dfd446572978058f764209571a6eb20f`, branch `tasks-hub` on
Stephens-Mini at `/Users/stephenspeicher/projects/tailterm`.

## Required behavior

Opening a native dropdown or a disclosure such as Closed must remain a deliberate
user action. Routine cache status changes, event delivery, changed server data,
and the Projects status clock must not close or flicker those controls. Explicit
selection, Escape, outside dismissal and clicking the disclosure summary retain
their normal browser behavior. Keyboard and mobile interactions remain usable.
Projects More is included after QA reproduced the same refresh closure (#674).
Its open state and `aria-expanded` must agree, and explicit outside dismissal,
Escape and action selection must continue to close it normally.

The hub remains authoritative for project, agent, message and work-item data.
Control identity, focus, pending native selection and manual disclosure state
belong to the view. Keeping an interaction usable must not stop data reads or
silently discard updates. A removed/invalid choice, navigation, view hide and
connection failure require deliberate handling rather than indefinite stale UI.
Focus alone is not evidence that a native popup remains open. Escape, selecting
the current value and outside dismissal must allow subsequent visible updates,
even if the select retains focus. Preserve the Board's existing protection for
pointer-down through project selection, including cancellation and outside release.
Existing drafts, decision retries, history, project navigation and closed-project
read-only behavior remain compatible.

Disclosure state is scoped to its view and relevant project context. Navigating
explicitly to a closed project may reveal its entry. A later background update
must preserve the user's subsequent open/closed choice. No cross-origin or
cross-device preference synchronization is required.

## Ownership and verification

UI owns Board, Projects and work-item view changes, plus a named client-only
presentation helper (`client/view-refresh-presentation.js`) and focused tests
(`tests/view-refresh-presentation.test.js`). Other client files require an
explicit ownership extension. Lead owns this contract, integration and release.
Cache QA owns `tests/dropdown-continuity-browser.mjs` and ignored artifacts under
`.build/dropdown-continuity`; product files remain read-only for QA.

Independent Chromium and WebKit checks must distinguish native-popup observation
from programmatic selection. Use real pointer/keyboard input with deterministic
background refresh and evidence that changed data was received. Check native
selects and Closed disclosures in Board, Projects, Bugs and Features; preserve
normal dismissal, selection, live updates, drafts and navigation. Exercise a
390-pixel viewport and report limitations of headless OS-native popup visibility.
Capture baseline failures separately from final candidate acceptance. Test data,
databases and browser/vault state must be isolated from the live workspace.

QA's bare-form control experiments (#674, #677) found that both headed and
headless automation could open native menus but could not reliably commit or
dismiss them with keyboard input. Retain that evidence separately from product
results. Pointer-open state, node continuity and outside dismissal remain
observable; native keyboard commit and Escape require additional evidence before
release acceptance. Shared-helper tests do not replace that browser evidence.

## Candidate evidence

The baseline at `c0396e1` reproduced 26 failures among 34 cases across Chromium
and WebKit; eight existing regressions passed. The first candidate run was
invalidated by a source edit after a premature freeze announcement. Its seven
passing cases do not constitute acceptance.

The subsequent frozen candidate passed all 38 independent browser cases
(19 per engine), including all-view shared-root navigation and dismissal after
the selected recipient was removed. Lead checked the recorded source hashes
against the working tree. Evidence is retained under
`.build/dropdown-continuity/candidate-final/`; native keyboard limitations above
remain separate from these passing observations. Focused helper/lifecycle tests
passed 15/15; UI reported the full unit suite passed 121/121.

The existing cache browser test failed on both baseline and candidate because
its synthetic transport omitted the decisions endpoint added earlier. QA's
one-line response correction passed the entire suite in Chromium and WebKit;
lead integrated the identical correction in `tests/hub-cache-browser.mjs`.
This was a test fixture gap, not a dropdown regression. Diagnostic logs and the
passing companion remain under `.build/dropdown-continuity/`.

A candidate static build updated `dist-static`, which the existing Mini preview
serves. TailOS has not been deployed for this change. Final release acceptance
and native keyboard evidence remain pending.

This work excludes the separate bug-creation/edit-history request, narrative/AIV
features, server/CLI changes and dependency upgrades. Deployment requires a later
recorded release order after acceptance. Current frontend targets are TailOS and
Mini preview; Air's preview remains intentionally retired.
