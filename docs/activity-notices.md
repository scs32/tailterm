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

No deployment or production-service change was made. The isolated candidate is
retained for lead review. TailOS and the existing Mini preview remain eligible
only after lead accepts and sequences a release slot. No hub, CLI, relay,
network, Tailscale, TrueNAS, live task/profile fixture, shared CSS/dependency, or
owner-session change was made.
