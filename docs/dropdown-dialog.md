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
  the trusted user activation to call `showPicker()`. A programmatic `click()`
  within that activation is the compatibility fallback; it is not claimed as a
  trusted native click. Enter reaching an already-open picker is left to the
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
- Candidate-stage `npm run build:static` did not pass in the isolated builder
  worktree because generated `wasm/tailserve.wasm` was absent. The accepted
  candidate was later integrated on the released root, and a clean detached
  source built pinned production WASM and passed the 82-entry release check.

After integration on `tasks-hub`, the affected dialog/presentation tests passed
19/19. The dropdown dialog, narrative reader, project work-item and active-scroll
browser suites passed in Chromium and WebKit. The clean exact application commit
`8d47e9b4b6c34cc1b9331517ba893156d28e23e8` is deployed to immutable TailOS,
the custom TailOS domain and Mini. Every origin matched `release.json` and all
81 served assets by size and SHA-256, with expected MIME behavior and no content
encoding. Fresh Chromium on every origin passed production WASM startup,
isolated synthetic-vault restoration and three-viewport layout checks.

Playwright pointer and keyboard events are real browser input, and `:open` is a
browser-observed state. `selectOption` is a synthetic DOM-level commit. Protocol
input does not select an option in the macOS native popup layer; WebKit also did
not expose an Enter-opened picker even though it retained the editor. There was
no native Safari, physical mouse/keyboard, owner-profile, live-data, or
owner-device pass. Exact physical pointer-option selection therefore remains an
acceptance limitation rather than a claimed reproduction.

## Release and remaining acceptance

Lead accepted the two demonstrated keyboard defects in Board message #1387 and
assigned release in #1401. Cloudflare deployment
`9a5a97b3-c3ad-4b34-bc16-59a66632c7b9` serves
`https://9a5a97b3.tailos.pages.dev` and `https://tailos.tailarr.com`; Mini serves
the byte-identical package at `http://127.0.0.1:4318` without restarting detached
listener PID 60799 / PPID 1. The clean source/package is retained at
`.build/releases/dropdown-dialog-8d47e9b`. Immediate rollback is the narrative
package `.build/releases/narrative-history-4cb6e6c` and deployment
`https://fa7bd9ec.tailos.pages.dev`.

Owner confirmation of the exact pulldown and physical interaction remains useful:
native macOS popup option selection was not automatable, so the original physical
pointer-option symptom is not claimed fixed by direct reproduction. The database
handler must still save the revision-checked final report and lead acceptance
before the item can be reported complete.
