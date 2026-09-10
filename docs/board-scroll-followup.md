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
changed during candidate work.

Lead accepted candidate `65b7375549fcc274140c6068fe9f393c94ebca19` in Board
message #1281. It was integrated without conflict as application commit
`8e78f5508ddb0e15912ff39b397474f7df5b20fd`, preserving the prerequisite and
all current fixes. The exact clean source/package is retained at
`.build/releases/board-scroll-followup-8e78f55`; its 82-entry manifest passed
local verification and has SHA-256
`7080d78e74573ceca7552af76f6783e7c123d62037439d5dffdde81a20b6f5a5`.

Cloudflare Pages project `tailos` deployment
`0d7863de-37c3-4463-a504-1d4e8e9c4c91` is available at
`https://0d7863de.tailos.pages.dev` and `https://tailos.tailarr.com`. The same
package was synchronized to Mini `http://127.0.0.1:4318`; preview PID 28664 was
not restarted. The immutable URL, custom domain and Mini each matched the
manifest and all 81 served files. JavaScript, CSS and compressed production WASM
statuses/MIME passed; the `.wasm.gz` response remained `application/gzip`
without `Content-Encoding` on both required targets.

Real production Chromium on TailOS and Mini started WASM, created and restored
an isolated synthetic vault/key, retained matching desktop margins and reported
no page errors. The two layout screenshots are byte-identical with SHA-256
`dee008e5bb73ff69cb91fb1e43b1dc1241bc26b73201661e1427f62041b71664`.
The exact retained integrated source also passed the focused scrolling fixture
in Chromium and WebKit after packaging.

Rollback is the reverified retained status-filter package
`.build/releases/status-filter-0c1a3ce` at application commit
`0c1a3ce62301c415ab51d2c0c3438893702622db`, manifest SHA-256
`e591b6c3d9d5ea58b9aea3a52e77c3b7832439988214dcdd790e2a48902cddea`,
and deployment `https://ea5b1191.tailos.pages.dev`. Explicitly deploy that
package to Cloudflare project `tailos`, then exact-sync it to Mini without
restarting PID 28664. Rollback removes only this follow-up while retaining the
previously accepted Board anchor, activity and status-filter fixes.

No hub, CLI, database, schema, network, Tailscale, TrueNAS, relay, old-site, Air
preview, live task/profile, owner storage, agent lifecycle or existing session
change accompanied the release. Browser contexts and fixture servers closed;
the exact release worktree and evidence are retained. Physical trackpad/touch,
native Safari and owner-device behavior remain explicit acceptance limits, so
the release is not described as a confirmed reproduction or fix on the owner's
unknown browser/device.
