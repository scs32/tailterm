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

The product fix changed no shared presentation, CSS, dependency, integration,
hub, CLI, networking, profile, live task, or pre-existing release file.

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
live project/profile data.

## Verified release

Lead message `#1193` accepted builder candidate
`1e3ab79be2f3318ee8b721c7a717ae1d4f20c17a` and assigned its first release
slot. The candidate was cherry-picked without conflict into clean `tasks-hub` as
application commit `c8a7552b84460b6ba06dcb0561460a109f2bf5e0`; the accepted
source, focused test and this report were byte-identical after integration.
Database-handler message `#1194` confirms the candidate result link and keeps
the item in progress pending actual-release acceptance.

The production WASM and static package were built only in the retained detached
worktree `.build/releases/board-scroll-c8a7552`, never in Mini's served root.
`npm run verify:release` passed all 82 manifest entries. `release.json` SHA-256
is `c239f7bf341b23cb4266cd4bcdd97b03912a6b5a2b71f8739538a30edb2bb489`;
the main script is `/assets/index-pOG8b-C3.js`, and production WASM is
`/assets/tailserve-BxE9iBHL.wasm.gz` served as `application/gzip` without
`Content-Encoding`.

Cloudflare Pages project `tailos` deployed the exact package as
`123605a6-bf6e-4c57-9271-04cd349eb4af`, available at
`https://123605a6.tailos.pages.dev`. The immutable origin and
`https://tailos.tailarr.com` each matched `release.json` and all 81 served file
hashes. The exact retained package was then synchronized with
`rsync -a --delete` to Mini's existing served `dist-static`; PID 28664 was not
restarted, and Mini also matched all 81 files and the manifest.

Real Chromium on TailOS and Mini started production WASM, created and restored
an isolated synthetic local vault/key, retained matching desktop margins, and
reported no page errors. The two layout captures are byte-identical with SHA-256
`dee008e5bb73ff69cb91fb1e43b1dc1241bc26b73201661e1427f62041b71664`.
The focused Board-scroll fixture was rerun from the retained integrated source
and passed in Chromium and WebKit.

Rollback is application commit `37bfbd25085ef1706d6893891471142ec03ce984`,
deployment `https://a2069721.tailos.pages.dev`, and retained package
`.build/releases/safari-css-37bfbd2/dist-static`. It reverts this scroll fix and
retains the separately documented custom-domain cache-rule limitation. No Air,
hub, CLI, database, schema, Tailscale, TrueNAS, relay, live task/profile,
owner-browser or agent-session change accompanied this release. See the
[durable release receipt](releases/tailos-2026-09-09-board-scroll.json).
