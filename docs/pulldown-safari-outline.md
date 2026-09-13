# Work-item dropdown focus outline

Bug `wi_85a66e159b50a309` revision 11, reproduction order #3478,
direct workspace assignment #3514, and bounded CSS supplement #3562,
saved by database handler #3564. Baseline is `19d7f7a` on `tasks-hub`.

The owner identified Safari and then reported: “When you select it the first
time an odd second outline shows up around the option.” These clarifications
are preserved in #3531 and #3555. The exact native option versus closed-control
distinction has not been confirmed on the owner's device.

The actual Bugs editor Status select reproduces a detached second frame on its
first pointer activation in isolated Chromium and WebKit. The existing global
focus rule gives it a 2px solid outline with a 3px outside offset, in addition
to its 1px border. Native appearance is already disabled for the closed select;
the detached outline is authored CSS. This establishes a matching visual defect,
not the cause of the separately reported Safari popup dismissal.

The repair changes only `#work-item-form select:focus-visible` in
`client/work-items.css`, setting `outline-offset: -2px`. The existing 2px focus
outline now overlaps the control border inside its bounds. Width, color, native
popup behavior, value changes, keyboard handling and control dimensions retain
their existing definitions. The selector covers Project, Status and Priority in
the Bugs and Features editors; list filters and unrelated controls are unchanged.

## Verification

All fixtures use synthetic in-memory records, disposable localhost servers and
new browser contexts. Production data, owner browser storage and useful services
were not used or changed.

- Before/after runtime-style comparison: ten pointer-open, refresh and outside
  dismissal cycles for each variant in Chromium and WebKit. Baseline offset is
  3px; candidate offset is -2px. The existing 215×36 Status dimensions, open
  dialog and keyboard focus remain. WebKit PNGs were visually inspected.
- Actual source stylesheet, without injected candidate CSS:
  `OUTLINE_SOURCE=1 node .build/outline-probe.mjs` passes 120 cycles across both
  engines, Bugs/Features and Status/Priority/new-item Project. Every control has
  a visible 2px inset outline on first, second and tenth activation. Keyboard
  Tab/Shift-Tab reaches each control with a visible focus indicator.
- The pre-change command `PULLDOWN_MODE=bugs node tests/pulldown-recurrence-browser.mjs`
  passed existing two-engine cancellation, refresh, synthetic commits and draft
  retention. No JavaScript behavior changed in this CSS correction.

Reproduction scripts and complete JSON/PNG evidence are retained in the isolated
checkout `.build/worktrees/root-pulldown-20260913/.build/`, in `outline-probe`
(before/after) and `outline-source` (actual source). The disposable runner imports
the existing recurrence fixture and production views/styles. Its results are
local execution evidence, not external CI or a native Safari pass.

## Remaining acceptance

Actual Safari control remains blocked by pending Computer Use Accessibility and
Screen Recording permissions. Installed local Safari reports 26.5.2; the affected
owner device version is not established. Playwright WebKit is not native Safari,
and protocol pointer coordinates do not commit the macOS native popup options.
No original pointer-option dismissal fix is claimed. Keep the Bug open for that
remaining behavior and owner verification. This candidate is not deployed;
independent review and a recorded release handoff remain separate.
