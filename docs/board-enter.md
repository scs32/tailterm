# Board composer keyboard submission

Bug `wi_fc695f0cbc3f7a42`, owner report #795, bounded build
`wi_fc695f0cbc3f7a42-build-1` (work-order message #820). The database handler
verified the authoritative revision and order in #836 and the dedicated builder
assignment in #828. Lead #858 and handler #874 added this closeout report and
release-evidence template to the same bounded build; they did not authorize a
deployment.

## Problem and result

The Board message composer previously required activating its Send button because
Enter in the message textarea only inserted a newline. The candidate makes a bare,
non-repeated Enter submit through the form's existing send handler. Shift+Enter
keeps the textarea's native newline behavior.

The shortcut applies only to `#board-text`. It does not change decision forms,
work-item editors, dropdowns, dispatch dialogs or other text fields. It calls
`requestSubmit()` so the existing empty-text validation, recipient/reply values,
pending guard, error notice and refresh path remain authoritative.

Composition confirmation does not invoke submission when the keyboard event has
`isComposing`, while a tracked composition session is active, or when the legacy
IME key code is 229. Repeated bare-Enter keydowns are suppressed. The existing
per-project sending guard prevents a second request while a send is pending.

Ordinary Board posts now carry a browser-generated request ID. An unchanged retry
retains that identity and exact normalized message payload; changing the text,
recipient or reply target after an attempted send rotates the identity. This uses
the hub's existing post-receipt contract, so a committed message whose success
response was lost can be retried without adding a second message. A failed send
retains the draft, recipient, reply target, focus and text selection.

## Candidate and ownership

- Application candidate: `c045440f511d534efdacb2b02d942463191bb715`
- Base: `4de29d028842a24b391f3e9dc6086e398c10ea7f`
- Branch: `fix/board-enter-fc695f0c`
- Isolated checkout:
  `/Users/stephenspeicher/projects/tailterm/.build/worktrees/board-enter`
- Product file: `client/board-view.js`
- Focused acceptance: `tests/board-compose-browser.mjs`

The documentation commit intentionally follows the frozen application candidate.
Product and focused-test source remain unchanged while independent QA reviews the
candidate.

## Builder verification

All successful browser fixtures used disposable synthetic projects, browser
contexts and a compiled local hub with a temporary SQLite database. They did not
read or write the live project hub, profiles, work items, tmux sockets or user
vaults.

| Command                                                                                            | Result                                                                                                                                                                                                                                                                                                                                                                                                                                              | Retained transcript (SHA-256)                                                                                                |
| -------------------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------- |
| `node tests/board-compose-browser.mjs`                                                             | Passed in Chromium and WebKit. Real browser keyboard input covered Enter and Shift+Enter multiline content. Synthetic DOM events covered `isComposing`, active composition and repeat flags. Empty, disabled and pending states produced no extra request. A deliberately lost response after a real hub commit retained draft/selection and retried the same request ID; the hub stored exactly one message with the original recipient and reply. | `.build/board-enter-evidence/board-compose-browser.log` (`052650b49fa531f7523c0d4056c00a9644b10afb312340c5320036a0f762f29c`) |
| `npm test`                                                                                         | Passed 121/121.                                                                                                                                                                                                                                                                                                                                                                                                                                     | `.build/board-enter-evidence/unit-tests.log` (`64b4126ddee923bcee8e5652af05da4060fa1a62c5e48759f91ba2c5d8a3e7d5`)            |
| `node tests/board-layout-browser.mjs`                                                              | Passed Chromium/WebKit at 1440, 1024 and 390 pixels for populated, empty and long-content cases, including the existing Board failure-draft path.                                                                                                                                                                                                                                                                                                   | `.build/board-enter-evidence/board-layout-browser.log` (`44391f93467ead052df872419f59387dc5bcc4c2b2e4699608d0564d57828f79`)  |
| `npx prettier --check client/board-view.js tests/board-compose-browser.mjs` and `git diff --check` | Passed before the application candidate commit.                                                                                                                                                                                                                                                                                                                                                                                                     | Builder result message #850, corrected by #862; exact candidate saved by handler #868.                                       |

`node tests/task-form-browser.mjs` is not reported as passing. Its Board composer
coverage ran before the command later timed out waiting for a team-launch retry
dialog to close at `tests/task-form-browser.mjs:485`. A second unchanged attempt
reproduced the same unrelated timeout. Its retained transcript is
`.build/board-enter-evidence/task-form-browser.log`, SHA-256
`2cd048390c7590b8fea3b740fbb54c26c6d0bb4dc32371d0d501a5211625ca65`.
Lead #873 explicitly kept this as a nonpass limitation rather than expanding the
bug to repair the separate team-launch fixture.

## Independent evidence and limitations

Lead #858 assigned `board-enter-qa` to read-only review of the exact application
candidate. QA #876 accepted it, and lead #879 accepted that review. The focused
composer command independently exited 0 in 10.10 seconds in Chromium and WebKit.
QA also ran an in-memory browser supplement in both engines: synthetic key code
229 did not send, and editing a failed draft before using the unchanged Send
button rotated the request ID and payload. QA independently reran the Board
layout regression and changed no product or tracked test file.

The complete independent report is
`.build/board-enter-independent/qa-report.md`, SHA-256
`2074f8cde68640f7244c8e89342bb04b85215e5eb9e3b396c836e1cb702affc1`.
It distinguishes independently rerun commands from builder evidence that QA only
reviewed.

Chromium and WebKit exercised real Enter and Shift+Enter key input. Composition
state and repeated-key flags were injected as synthetic DOM events inside those
real engines. No native macOS IME session or physical held-key autorepeat was
observed, so this report does not claim native OS IME coverage. The product guard
is nevertheless checked at the browser event boundary for `isComposing`, tracked
composition sessions, key code 229 and `repeat`.

The focused test closes both browsers, its HTTP server and the compiled hub in a
`finally` block, then removes its temporary state directory. The retained evidence
contains only synthetic data. No deployment, installation, hub/CLI/schema change,
network change, Tailscale change or live-session stop occurred during the build.

## Release preparation

Handler #885 verified `wi_fc695f0cbc3f7a42-release-1`, release-order message
#883, at work-item revision 11. It authorizes builder-owned integration, packaging,
frontend deployment and verification after the accepted candidate is integrated.
The intended frontend targets are:

1. TailOS at `https://tailos.tailarr.com`, Cloudflare Pages project `tailos`, using
   the explicit Wrangler command from `docs/handoff.md`.
2. The Stephens-Mini preview at `http://127.0.0.1:4318`, using the documented
   local frontend replacement flow when appropriate.

Do not deploy to `https://tailterm.tailarr.com`, run `npm run deploy:static`,
recreate the retired Air preview, or change the TrueNAS hub/Tailscale setup for
this frontend-only bug.

The current rollback application is
`fd8d10273a46877b6bd7a6eb0c1dd9b2bc228bce`, with its clean retained package at
`/Users/stephenspeicher/projects/tailterm/.build/releases/dropdown-dispatch-fd8d102/dist-static`
and TailOS deployment `https://14bee768.tailos.pages.dev`. The release operator
must verify the retained package and current target state rather than assuming a
rollback is needed.

The reviewable release template is
`docs/releases/tailos-2026-09-09-board-enter.template.json`. It deliberately leaves
the integrated application commit, manifests, deployments, served asset checks
and final cleanup confirmation unset. Those fields may be filled only from
observed release evidence.

## Verified release

The accepted application bytes and this report/template were integrated by
fast-forward into `tasks-hub` as
`d441722a8f7f06d8a097621a24a5e58dcba4ea95`. Git confirmed that the accepted
product and focused-test files at `c045440f511d534efdacb2b02d942463191bb715`
were unchanged in the integrated commit.

`npm run build:static` created a clean package with 82 manifest entries, and
`npm run verify:release` passed. Its `release.json` SHA-256 is
`4da7c372e2dde9813c1fd63afb590aead8f6033360c7a6fb2e3456b63dc15a34`.
The clean source and package are retained at
`.build/releases/board-enter-d441722`.

TailOS production deployment `cd03fb64-9b8f-48c3-b341-8dad7fa5f87b` is
`https://cd03fb64.tailos.pages.dev`. Both that immutable origin and
`https://tailos.tailarr.com` matched the release manifest and all 81 served
asset hashes; `_headers` is the unserved hosting configuration entry. The public
main script is `/assets/index-DAdzlxlR.js`. Public Chromium loaded the production
WASM, restored an isolated synthetic local vault/key and reported no page errors.
Retained logs:

- `.build/board-enter-release/asset-verification.log`, SHA-256
  `1b4d9f647faf04494658e26c97e13456f0c7a1c35abe55c9161135bcd91847f8`
- `.build/board-enter-release/public-smoke.log`, SHA-256
  `5eae24c801dd7a6c78f4f0b2743324990d3c012e3db0f5e7dbc20b72e0f56fc7`
- `.build/board-enter-release/public-layout.png`

The first Mini command used the documented Apple Container replacement script,
but failed before a candidate image or preview replacement because BuildKit could
not start without Rosetta. No Rosetta, container, service or network change was
made. Lead #914 authorized the actual existing preview path: the already-running
Node listener on `127.0.0.1:4318` serves the root `dist-static` directory. Because
the clean build was made in that served directory, the read-only inventory already
showed the new commit. The exact retained package was then reapplied with the
authorized `rsync -a --delete` path, without restarting the listener. Mini matched
the manifest and all 81 served asset hashes. Its receipt is
`.build/board-enter-release/mini.json`, SHA-256
`dc85aaaf4bfd9a92b4b80e5ac7dc58499eabbf03b0bfc0e4a4defb78d3fe864a`.

The failed Apple Container staging copy was removed after its explicit path was
verified. It was a generated duplicate and is not recoverable; the source
package remains intact in the retained release worktree. The prior rollback
package was reverified clean with 82 manifest entries and its original
`ec83e3e7faf22b7c15d9c5e499cb0db579537513f4c8669bf39d7e06dc401e2d`
manifest hash. No Air, hub, CLI, schema, Tailscale, network, profile, task or agent
session was changed.

See [the durable release receipt](releases/tailos-2026-09-09-board-enter.json).
