# Keep work-item dropdowns inside their editor

Bug `wi_85a66e159b50a309` revision 2, bounded work order
`wi_85a66e159b50a309-dropdown-dialog-1` in Board message #1358.

## Reported and demonstrated behavior

The owner reported that a Bugs pulldown was still hard to select without its
dialog closing first. The report did not identify the exact control, input
method, browser, or device, so the original physical pointer-selection path is
not claimed as reproduced.

The released `fb37de2` behavior did reproduce two related failures with the
actual Bugs and Features editor controls in isolated Chromium and WebKit:

- Focusing the enabled Status select and pressing Enter implicitly submitted
  the editor, saved the unchanged item, and closed the dialog before a choice
  could be made.
- Pressing Escape while Status or Priority was visibly `:open` sent the same
  close request to the containing modal, closing the whole editor instead of
  only dismissing the native picker.

The focused baseline fixture reproduced both failures for Bugs and Features in
both engines. Pointer activation itself left the editor open. A click at true
backdrop coordinates also left it open because the shared dialog has no
backdrop-dismiss handler; this change does not add, remove, or reinterpret that
behavior. DOM-level `selectOption` commits remained live and did not submit.

## Repair

`client/dialog-interaction.js` now installs the shared native-select interaction
boundary used by `client/main.js`:

- Enter on a closed single-select prevents implicit form submission, then uses
  the trusted user activation to call `showPicker()`, with `click()` as the
  compatibility fallback. Enter reaching an already-open picker is left to the
  browser for native commit.
- A modal cancellation request is intercepted only when focus is inside that
  modal and the single-select actually matches `:open`. The handler prevents
  the containing dialog close, blurs the select to dismiss its picker, and
  restores focus on the next frame with `{ preventScroll: true }`.
- Ordinary Escape, the close button, form submission, value commits, draft
  persistence, and every non-select dialog control keep their existing paths.

No work-item view, refresh presentation, scroll logic, form payload, storage,
hub, CLI, schema, or deployment code changed.

## Verification

- `DROPDOWN_DIALOG_BASELINE=1 node tests/dropdown-dialog-browser.mjs`: reproduced
  both premature-close paths for Chromium/WebKit × Bugs/Features (4/4 views).
- `node tests/dropdown-dialog-browser.mjs`: Chromium/WebKit × Bugs/Features pass
  actual editor Status/Priority cancellation, dialog liveness, focus without
  scroll, ordinary Escape and close-button dismissal, retained drafts, and
  synthetic committed values/submission (4/4 views).
- `node --test tests/dialog-interaction.test.js tests/view-refresh-presentation.test.js`:
  targeted shared-dialog and retained refresh behavior pass.
- `npm test`: 140/140 complete JavaScript tests passed on the candidate.
- `node tests/work-item-draft-vault-browser.mjs`: encrypted, bounded,
  credential-scoped draft behavior passed in Chromium and WebKit.
- `node tests/dropdown-continuity-browser.mjs`: 38/38 existing dropdown,
  disclosure, consumed-pointer and shared-view cases passed.
- `node tests/project-work-items-browser.mjs`: Chromium and WebKit CRUD,
  encrypted retry/draft, history, dispatch, and closed-read behavior passed. A
  concurrent first WebKit run logged one access-control fetch error; the
  isolated rerun passed without a code change.
- `node tests/work-items-scroll-browser.mjs`: Bugs/Features active-scroll,
  pagination, focus, history, and dispatch checks passed in both engines on an
  isolated rerun. An earlier concurrent run passed its scenarios while Vite
  logged an `ENOTEMPTY` dependency-cache rename warning.
- `npm run build:static`: not passed in this isolated worktree because the
  generated `wasm/tailserve.wasm` prerequisite is absent. No production package
  or deployment is claimed at candidate stage.

Playwright pointer and keyboard events are real browser input, and `:open` is a
browser-observed state. `selectOption` is a synthetic DOM-level commit. Protocol
input does not select an option in the macOS native popup layer; WebKit also did
not expose an Enter-opened picker even though it retained the editor. There was
no native Safari, physical mouse/keyboard, owner-profile, live-data, or
owner-device pass. Exact physical pointer-option selection therefore remains an
acceptance limitation rather than a claimed reproduction.

## Remaining dependencies

Lead candidate acceptance and a sequenced integration/release slot remain
required. Integration must preserve the then-current lifecycle, narrative,
status-filter and active-scroll changes, generate the clean WASM/static package,
and verify TailOS plus the unchanged Mini listener. The database handler must
save revision-checked result/acceptance links before the item can be reported
complete. Owner confirmation of the exact dropdown and physical interaction is
still useful for deployed acceptance.
