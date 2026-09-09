# Terminal activity notice repair

Work item `wi_539c0062cebde9b8`, revision 3; bounded work order
`wi_539c0062cebde9b8-activity-1`, Board message #1166.

## Diagnosis and behavior

The background-output path retained only a set of visible line strings. Any
application that repainted a fixed row with a changing spinner, progress value,
clock, or counter therefore introduced a new string and raised **New output**,
even though the terminal had not advanced and no content was added. The usual
bottom tmux status row and byte-for-byte redraws were already excluded, but
mutable in-place redraws were not.

Activity snapshots now retain rendered lines plus xterm scrollback and cursor
position. A novel line counts as genuine output when the terminal advances or
scrolls, when text is appended at the current cursor, when a visible row is
added, or when tmux scrolls retained content upward. A fixed-row replacement
with the cursor left in place stays quiet. The existing per-pane marker remains
the source for tab/group rollups. Bell, OSC 133 command completion, actionable
connection errors, activity priority, focus clearing, and the 500 ms throttle
are unchanged.

## Focused evidence

- A clean static build of base `310130f0b4564ab38dd796b4d2fd18247a6f7647`
  ran the focused Chromium SSH fixture. Repainting an inactive pane's
  `Activity progress 41%` row as `Activity progress 42%` set
  `has-new-output=true` and failed the expected-false assertion. The identical
  candidate run passed, then exited through the focused-test boundary without
  entering unrelated task-hub scenarios.
- After the implementation, the focused activity and reliability run passed
  11/11. It covers fixed-row progress and spinner redraws, same-line appended
  output, output advancing to a new row, xterm scrollback advancement, tmux
  upward scrolling with unchanged xterm base/cursor, the existing rendered-line
  contract, attention priority, and actionable connection errors.
- `npm test` passed 129/129.
- The static candidate built outside served bytes at
  `.build/activity-notices-candidate/dist-static` ran the real Chromium SSH
  fixture. It passed the new inactive-pane progress repaint check and the
  existing genuine output pulse, Bell, OSC 133 completion, per-tab clearing,
  grouped workspace, and restored-workspace sequence.

The first broad browser pass later failed because the initial test setup used a
carriage return that overwrote the suite's earlier `after-rename-probe`
scrollback sentinel; the test setup was corrected to create its mutable row on a
new line, without changing production code. The rerun passed that continuity
check and later timed out finding the unrelated `Task hub: configure` command.
That downstream task-hub palette failure is not reported as passing and did not
invalidate the already completed activity checks.

## Release state and boundaries

Lead message #1199 accepted candidate
`a121aa06072264cda24d9f7aea6e7d049990f65d`, and message #1207 assigned the
release slot after Board-scroll acceptance. The candidate was rebased without
conflict onto clean `tasks-hub` `641967ad1b43ca31633f68288b4144f4c51faeef`
as application commit `8ed8c65ef85006bac04b3d212f89b7b66622a25e`;
all six owned files are byte-identical to the accepted candidate.

The production/test WASM and static site were built only in retained detached
worktree `.build/releases/activity-notices-8ed8c65`, outside Mini's served
bytes. Focused unit/reliability checks passed 11/11, the focused integrated
Chromium fixture passed, and `npm run verify:release` verified all 82 manifest
entries. `release.json` SHA-256 is
`8b0760761535c03271a34899481f72795316ff62f88e3cbc8c4c80b8a3b2fb44`.

Cloudflare Pages project `tailos` deployed the exact package as
`c15f5470-310a-4dba-b87f-8a014c4a1794` at
`https://c15f5470.tailos.pages.dev`. That immutable origin and
`https://tailos.tailarr.com` each matched `release.json` and all 81 served-file
hashes. The exact package was then synchronized with `rsync -a --delete` to
Mini's existing served `dist-static`; PID 28664 was not restarted, and Mini
matched the manifest and all 81 files. Production WASM is
`/assets/tailserve-DMRvIrni.wasm.gz`, served as `application/gzip` without
`Content-Encoding` on all origins.

Real Chromium on TailOS and Mini started production WASM, created and restored
an isolated synthetic local vault/key, retained matching desktop margins and
reported no page errors. Their screenshots are byte-identical with SHA-256
`dee008e5bb73ff69cb91fb1e43b1dc1241bc26b73201661e1427f62041b71664`.

Rollback is Board-scroll application commit
`c8a7552b84460b6ba06dcb0561460a109f2bf5e0`, deployment
`https://123605a6.tailos.pages.dev`, and retained package
`.build/releases/board-scroll-c8a7552/dist-static`. No hub, CLI, relay, network,
Tailscale, TrueNAS, Air preview, old-site, live task/profile fixture,
shared CSS/dependency, owner-session or agent-lifecycle change was made. See the
[durable release receipt](releases/tailos-2026-09-09-activity-notices.json).
