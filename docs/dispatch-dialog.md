# Close the dispatch dialog after success

Bug `wi_47ef43c93987b091`, owner dispatch #734, bounded order
`wi_47ef43c93987b091-fix-1` (#739). UI candidate
`b51cc5165da6c606d11e1857fd6eb9c6800bf999`, based on `54b25f1`.

The previous success handler left the Send to project dialog open and changed
its submit button into Open board. Confirmed successful dispatch now closes the
originating dialog and shows the returned target and board message number in a
notice. It does not navigate. Both Bugs and Features use this handler.

Pending requests disable repeated submission. Failed requests retain the
destination, error and request identity for retry; changing destination creates
a new identity. If the original form was dismissed or replaced before a late
response, that response cannot close or alter the replacement dialog. Records
still refresh. Dispatch APIs, revision association and database behavior are
unchanged.

## Evidence and remaining release dependency

UI reported 121/121 unit tests and 16/16 focused browser scenarios. Independent
QA passed 28/28 scenarios across Chromium and WebKit, using real disposable hub
storage: both kinds, pending success, rejection and retry, committed-response
loss with the same request ID and exactly one durable dispatch/message, and late
completion after Close, Escape, a replacement editor or a replacement dispatch
dialog. Lead verified all 23 recorded source hashes against the candidate.
Evidence: `.build/dispatch-dialog-qa/candidate-b51cc516/` in the main checkout.
QA confirmed its dispatch-only browser, listeners and database were cleaned up.

Lead updated old success-screen assertions in the project work-item and Board
layout fixtures. The layout fixture also lacked the previously introduced
decisions endpoint; it now returns an empty synthetic decisions page. Neither
fixture correction changes product behavior. Integration checks run in the
isolated dispatch checkout, preserving the main dropdown manual candidate.
`node tests/project-work-items-browser.mjs` passed in both engines (CRUD, scopes,
retry, dispatch, closed state and handler recovery). `node
tests/board-layout-browser.mjs` passed Chromium/WebKit at 1440, 1024 and 390px,
with populated, empty and long-content scenarios. Both commands exited 0;
`git diff --check` passed. Layout setup needed the existing ignored compiled WASM
asset copied into the isolated checkout before its successful run.

No static build or deployment was performed for this bug. The branch includes
the separate dropdown candidate, whose native keyboard acceptance remains
pending. Release requires a separately recorded order and satisfaction of that
dependency; accepted implementation is not a completed release.
