# Board active-scroll refresh follow-up

Work item `wi_a1b959960975589a` revision 7, bounded follow-up
`wi_a1b959960975589a-board-scroll-2`, was dispatched in Board message #1237
after the owner's message #1232 reported that Board scrolling remained broken.
This report preserves the earlier accepted prepend-anchor fix and describes only
the demonstrated follow-up cause and candidate.

## Reproduction and diagnosis

The unchanged `tasks-hub` base
`819471883d48faba8b29ccf9bd03918d3b974c97` still passed the earlier exact
message/intra-row anchor cases. It nevertheless reproduced a different failure
in isolated Chromium and Playwright WebKit: while the message viewport was
actively receiving browser-injected wheel input, a Board refresh replaced the
whole thread through `root.innerHTML`. The previously active `#board-messages`
element and focused `#board-text` element both became disconnected; the new
composer was then focused. Preserving the final numeric/message anchor did not
preserve the browser scroll target or its in-progress motion.

The baseline command was:

```sh
BOARD_SCROLL_REPLACEMENT_BASELINE=1 node tests/board-scroll-browser.mjs
```

Both engines observed the scrolling element and focused composer being replaced
during the active refresh, while the retained prepend/read/incoming/own-send
anchor assertions continued to pass. This distinguishes the remaining problem
from the already-fixed prepend jump.

## Candidate behavior

`client/board-view.js` now treats message-list wheel, scroll, and touch gesture
events as a short-lived presentation interaction. A same-project repaint
received during motion is coalesced while scroll events continue and released
after 120 milliseconds of scroll silence. A touch remains held until touch end
or cancellation, after which the same idle window allows inertial scroll events
to extend it. The latest already-fetched Board state is then rendered using the
existing first-visible-message/intra-row anchor restoration. Hiding the view or
changing projects clears the hold and timer.

The hold is limited to the message viewport and does not delay data reads. It
does not alter the existing less-than-60-pixel bottom-follow policy, history
fallback, own-send behavior, composer/decision drafts, or shared refresh
presentation code. The programmatic scroll used during anchor restoration is
ignored so it does not create a repaint loop.

## Focused verification

`node tests/board-scroll-browser.mjs` passes in isolated Chromium and WebKit.
For each engine it verifies:

- browser-injected wheel deltas continue against the same connected scroll
  element across an intervening refresh;
- the focused composer remains the same connected element during active motion;
- the latest message is held out of the DOM only until scroll idle, then appears
  with the reading anchor unchanged;
- a browser-managed smooth-scroll sequence continues after the refresh request
  and repaints at idle;
- a synthetic touch start holds the same DOM and focus through refresh, with
  repaint occurring only after touch release and scroll idle;
- prepend, read-receipt height change, incoming message, own-send, near-bottom
  follow, composer draft, and structured-decision draft behavior remains intact.

The relevant lifecycle, decision, and refresh-presentation unit selection passes
28/28:

```sh
node --test tests/view-lifecycle.test.js \
  tests/view-refresh-presentation.test.js \
  tests/board-decisions.test.js
```

Prettier checks for the two changed source/test files and `git diff --check`
pass. An initial focused unit run found one test-double compatibility failure
because the fake message list did not implement `addEventListener`; the listener
attachment was made capability-safe, and the identical 28-test selection then
passed. No browser candidate failure remains.

## Evidence limits and release state

Playwright's wheel injection is browser input automation, not a physical mouse or
trackpad observation. The smooth-scroll case is browser-managed motion used as a
repeatable momentum surrogate. This host has no automated native touch gesture
or native Safari session, so no hardware trackpad, touch inertia, Safari soft
keyboard, or owner-device pass is claimed. All those input paths ultimately
produce message-list scroll events, but that common event path is code inspection,
not a substitute for native-device evidence. The old focused input replacement
was observed; any resulting native soft-keyboard interruption is an inference.

Fixtures are in-memory and use disposable browser contexts, not live project or
profile data. No deployment, served package, Mini listener, hub, CLI, database,
network, Tailscale, TrueNAS, relay, old site, Air preview, or live session was
changed. TailOS/Mini release remains gated on lead candidate acceptance and an
assigned release slot under Board order #1237.
