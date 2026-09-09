# Board history scroll stability

Work item `wi_a1b959960975589a`, revision 3  
Bounded work order: project message `#1167`  
Source base: `310130f0b4564ab38dd796b4d2fd18247a6f7647`

## Diagnosis

Board refreshes replace the complete thread DOM. The previous implementation
remembered only the message list's numeric `scrollTop` and applied that same
number to the replacement list. That is not a stable reading position when
history is prepended or content above the viewport changes height, such as a
read-receipt presentation update.

The isolated regression was first run against the unchanged source base. After
20 older messages were prepended, Chromium's top visible message changed from
message `#31` to `#11`:

```text
AssertionError: chromium prepend: visible message changed
11 !== 31
```

## Implementation

`client/board-view.js` now captures the first visible message and its exact
offset from the scroll viewport before replacing the thread. After rendering,
it finds that immutable message sequence again and adjusts `scrollTop` so the
same part of the same message remains under the reader's eye.

The existing bottom policy is preserved: a new thread starts at the bottom, and
a refresh follows the bottom only when the prior viewport was within 60 pixels
of it. A reader farther up remains anchored, including after their own send. If
the captured message is no longer in the bounded latest-message window, the
implementation falls back to the prior numeric position.

No shared presentation, CSS, dependency, integration, hub, CLI, networking,
profile, live task, or release file changed.

## Focused acceptance

`tests/board-scroll-browser.mjs` uses an in-memory project/history fixture and
disposable Chromium and WebKit contexts. It verifies real viewport geometry,
message identity, and intra-message offset for:

- a 20-message history prepend;
- a read-receipt update whose receipt layout changes height;
- an incoming message while reading away from the bottom;
- an incoming message from within the 60-pixel follow zone;
- an owner send both away from and at the bottom; and
- composer and structured-decision draft retention across repaint.

Post-fix results:

```text
chromium: stable prepend/read/incoming/own-send anchors, near-bottom follow, and retained drafts
webkit: stable prepend/read/incoming/own-send anchors, near-bottom follow, and retained drafts
```

Adjacent regression results:

- `npm test`: 128 passed, 0 failed.
- `node tests/board-compose-browser.mjs`: Chromium and WebKit passed real
  isolated-hub Enter/send, pending, failure, draft, selection, and retry checks.
- `node tests/board-decisions-browser.mjs`: Chromium and WebKit passed all
  decision history, refresh, draft, retry, concurrency, pagination, mobile, and
  closed-history checks with no uncaught browser errors.
- `npx prettier --check client/board-view.js tests/board-scroll-browser.mjs` and
  `git diff --check`: passed.

There are no post-fix test nonpasses. Browser evidence is automated Chromium
and Playwright WebKit; it does not claim native Safari observation. Tests use no
live project/profile data. No build was written over the Mini preview's served
`dist-static`, and no deployment was attempted. Release packaging and both
authorized targets remain pending lead candidate acceptance and release
sequencing under order `#1167`.
