# Owner report

`tt owner report` renders the owner's hourly, morning and evening progress
report from one fixed template filled with hub data. The product writes the
report, not the session that sends it, so it has the same shape whichever
model or person runs the command.

Source: owner principle #51832 ("This needs to be templeted and structured"),
bug `wi_91bdb5b6f0f46f97`, work order #53136. The Up next section, the
`--post` step that sends the report on the hub wake schedule and the short
Shipped line come from bug `wi_546279658420371a`, work order #53584. The
hub's owner status call (Opened vs closed, Host health, the daily 21:00
baseline), the plain Shipped line and the scheduled post's ack-gate rule
come from bug `wi_475a3df9b345e13e` (`wi_475a`), work order #55055.

## Command

```
tt owner report hourly|morning|evening [--task ID] [--tz ZONE] [--host H] [--now T] [--since T] [--post]
```

- `--task`: the project ID. Required unless `TAILTERM_TASK` is set.
- `--tz`: the IANA time zone for the window and every printed time. Default:
  this machine's zone (`TZ`, then `/etc/localtime`, then `UTC`).
- `--host`: the host whose provider usage reading is shown. Default: this
  machine's tailnet name, or its short hostname.
- `--now`: the window's end, RFC3339. Default: the current time.
- `--since`: the window's start, RFC3339, in place of the kind's default
  start. It must be before `--now`.
- `--post`: end the window at the kind's latest slot at or before `--now`
  and post the printed report to the Board once for that slot (see "Sending
  on the hub schedule").

Without `--post` the command reads from the hub and changes nothing. The
owner terminal and agent sessions (such as the owner helper) may both run
it, with or without `--post`. Other `tt owner` subcommands still refuse
agent sessions. `tt owner report` with a bad argument prints the full usage.

## Windows

Each report covers the half-open window `[start, end)`, where `end` is
`--now`.

| Kind | Default start |
|---|---|
| hourly | one hour before `--now` |
| morning | the latest 21:00 local time strictly before `--now` |
| evening | the latest 07:00 local time at or before `--now` |

With `--post`, `--now` (or the current time) is first moved back to the
kind's latest slot at or before it, and the window is computed from that
end:

| Kind | Slot (window end) |
|---|---|
| hourly | the top of the local hour |
| morning | the latest 07:00 local time |
| evening | the latest 21:00 local time |

So a wake read late, or run again, still reports exactly its slot's window:
`tt owner report hourly --post` at 14:25 reports 13:00–14:00.

A release counts in the window when its `settledAt` is in it. An
intervention counts when its message's `createdAt` is in it.

## Template

Every report has these nine sections, in this order. Each header is always
printed, even when the section is empty or could not be read.

```
**Owner report: <Hourly|Morning|Evening>** <YYYY-MM-DD HH:MM>–<HH:MM> <ZONE>
**Shipped** (<n>)
- <title>: <what changed>. Limits: <limits>
- <title>. Limits: <limits>
**Releases**
- released <r>, rolled back <b>, refused <f>, superseded <s> in this window; in line now: <a>
- since the <Mon D> 21:00 baseline: released <n>
**Running teams** (<n> of <limit>)
- <item id> <title> (<template>, <state>)
**Up next** (<n>)
- <n>. <item id> <title> (<template>)
**What went wrong** (<n>)
- release <job id> for <item id> rolled back at <step>: <reason>
- release <job id> for <item id> refused at <step>: <reason>
- intervention #<seq> <kind> on <item id> by <actor>
**Opened vs closed**
- opened <n> bugs, <n> features; closed <n> bugs, <n> features in this window
- open now: <n> bugs, <n> features (since the <Mon D> 21:00 baseline: <±n> bugs, <±n> features)
**Usage**
- <runtime> <window>: <used>% used, resets <Day HH:MM> (reading <age> old)
- <runtime> <window>: <state>
- project tokens in window: <class> <n>, <class> <n>, ...
**Host health**
- <host>: <n> GiB free (reserve <n> GiB), memory pressure <n>[ (strained, over <n>)], matrix waiters <n> (reading <age> old)
- queue: admitting
- queue: held: <hold>
- stalled: <item id> <cause>
**Needs you** (<n>)
- #<seq> <source kind>: <subject or needs> (due <HH:MM>[, overdue <overdue>])
```

When the window's start and end fall on different local days, the header
shows the end's date too (`2026-10-30 21:00–2026-10-31 07:00 UTC`).

### Sections and their sources

| Section | Hub read | What it shows |
|---|---|---|
| Shipped | settled releases (`GET /v1/tasks/{id}/releases?view=settled`, every page) and each item's narrative (`GET /v1/tasks/{id}/work-items/{wid}/narrative`, then the pinned completion report version) | Each item with a `released` or `superseded` job in the window, once, ordered by its first time in the window and then by item ID. The item's title, then one plain sentence of what changed and of its limitations from the item's pinned completion report (see "The Shipped summary"). |
| Releases | settled releases, plus every page of `view=active`; the owner status call for the baseline line | Settled jobs in the window by state. "In line now" counts every job not yet settled. The second line counts jobs released since the latest stored 21:00 baseline before the window's end, and is left out when there is no baseline yet. |
| Running teams | `GET /v1/tasks/{id}/team-queue?view=active` | Entries in state `launching` or `running`, ordered by queue position; the limit is the project's concurrency limit. |
| Up next | the same `GET /v1/tasks/{id}/team-queue?view=active` read, and each item's narrative for its title | Every `queued` entry with an up-next number (`upNext`, 1..N in the order the queue admits them; `docs/project-queue.md`, "Team queue lanes and up next"), ordered by that number and then by item ID, with its template. An entry whose narrative cannot be read is listed without its title, and the read counts as a failure. |
| What went wrong | settled releases and `GET /v1/tasks/{id}/interventions` (every page) | Refused and rolled-back jobs in the window, with the runner's fixed failure step and reason tag (never command output), and owner interventions in the window. Ordered by time, then ID. The actor reads `unknown` when the hub recorded none. |
| Opened vs closed | the owner status call (`GET /v1/tasks/{id}/owner-status`) | Bugs and features opened and closed in the window, those open at the window's end, and the change since the latest 21:00 baseline, or `no 21:00 baseline yet` (see "Owner status call"). |
| Usage | `GET /v1/provider-usage?host=H` and `GET /v1/tasks/{id}/usage?from=&to=` | Each stored provider reading for the host, ordered by runtime and then window. A reading that is not `ok` prints its state. Then the project's token classes in the window, sorted by class, exactly as the hub reports them. |
| Host health | the same owner status call | The host census the team queue stores: free disk and its reserve, memory pressure (with the strain threshold when it is over it) and matrix waiters, each shown as not observed when the runner did not read it, and the reading's age. Then whether the queue's lanes admit or hold (with the hold reason), and one line per stalled queue entry with its cause. |
| Needs you | `GET /v1/tasks/{id}/obligations?owner=1&open=1` | Every open owner obligation, ordered by due time and then message number. The subject is shown, or what the obligation needs when it has no subject. A due time on another day than the window's end shows its date (`Oct 30 15:04`). |

The narrative is read once per item, even when an item appears in more than
one of Shipped, Running teams and Up next.

Saved free text (titles, delivered work, limitations, subjects) is cleaned
the same way every time. Runs of whitespace become single spaces, the ends are
trimmed, and anything longer than 280 characters is cut and ends in `…`.

### The Shipped summary

A shipped item's line is plain words the owner can read: the item's title,
then one sentence of what changed, then one sentence of its limits. It
never prints message numbers, commit SHAs, branch names or worktree paths.

- What changed: the first plain sentence of the completion report's
  delivered work, cut at 160 runes. When no sentence is plain (or the field
  is empty), the line is the title alone: `- <title>. Limits: <limits>`.
- Limits: the first plain sentence of its limitations, cut at 120 runes.
  An empty field prints `none recorded`; a field with no plain sentence
  prints `see the completion report`.

Whitespace is collapsed first. A sentence ends at the first `.`, `!` or `?`
followed by a space or the end of the text, so a file name such as
`owner_report.go` or a version such as `v1.2` does not end one, while an
abbreviation followed by a space (`e.g. `) does. Text longer than its limit
is cut there, trailing spaces are trimmed, and it ends in `…`. A trailing
full stop on the what-changed sentence is dropped before `. Limits:`.

A sentence is plain when it contains none of these:

- `#` followed by a digit, a run of five or more digits, or `order`,
  `request`, `message`, `msg` or `seq` (any case, optionally plural)
  followed by three or more digits (message numbers such as `#55055`,
  `REQUEST54571` or `order 4335`);
- a word starting `wi_` (item references and branch names such as
  `wi_475a-owner-status`, also inside `fix-wi_8237-pathless-wait`);
- a run of 7 to 40 hexadecimal characters with at least one digit and one
  letter (commit SHAs, also inside a word such as `landed05f1a040`, and the
  hex in IDs such as `wi_475a3df9b345e13e` or `queue-475a3df9`);
- a `/` (paths and branches such as `origin/main`);
- the branch name `tasks-hub`, in any case.

So `owner_report.go`, `v1.2`, a year and a count such as `1245 tests` stay
plain. The source is the
completion report's delivered work and limitations; when bug
`wi_977caae754fc263e` adds a one-line result for bug done saves, that line
can become the source instead.

## Fixed lines

These lines are printed exactly as shown.

| When | Line |
|---|---|
| No stored 21:00 baseline before the window's end yet | `(no 21:00 baseline yet)` after the open counts, and no `since the` line under Releases |
| The hub stores no host census | `- no host reading` |
| A census reading the runner did not observe | `free disk not observed` in place of `<n> GiB free`, or `not observed` in place of a number |
| The queue's lanes admit (or the project has no lane policy) | `- queue: admitting` |
| A shipped item's limitations have no plain sentence | `see the completion report` |
| No queued entry has an up-next number | `- nothing queued` |
| No item shipped in the window | `- none in this window` |
| A shipped item has no pinned completion report | `- <title>: no saved completion report` |
| A delivered-work or limitations field is empty | `none recorded` in its place |
| No team is launching or running | `- none running` |
| Nothing went wrong in the window | `- none recorded (sources: refused or rolled-back releases, owner interventions)` |
| No provider reading for the host | `- no provider reading for host <host>` |
| No project tokens recorded in the window | `- project tokens in window: none recorded` |
| No open owner obligation | `- nothing waiting on you` |
| A read failed | `- unavailable: could not read <source>` |

The sources a failed read names are `the release ledger`, `the team queue`,
`owner interventions`, `provider usage`, `project usage`,
`owner obligations`, `the owner status` and, for a single shipped item,
`the work item narrative` (as `- <item id>: unavailable: could not read the
work item narrative`; a Shipped line shows the item ID only when its title
could not be read). A section that could not be read prints its header
without a count. A failed team queue read shows the unavailable line under
both Running teams and Up next. A failed owner status read shows it under
both Opened vs closed and Host health, and leaves out the Releases baseline
line.
A running team whose narrative cannot be read is listed without its title,
and the read counts as a failure (see Exit status).

## Owner status call

The hub computes the report's figures itself, so no session keeps them in
its notes or in a script on one machine:

```
GET /v1/tasks/{id}/owner-status?from=T&to=T&tz=ZONE
```

`from` and `to` are RFC3339 times, the half-open window `[from, to)`, at
most 31 days long; `tz` is an IANA zone (not `Local`). A missing or bad
parameter gives 400, an unknown project 404. The client call is
`api.Client.OwnerStatus`; `tt owner report` sends its own window and `--tz`.
Every caller may read it, including an agent holding unacknowledged work:
it is a read, and its one write (the baseline below) never passes the ack
gate. One body holds:

- `releases`: release jobs whose `settledAt` is in the window, by state
  (`released`, `rolledBack`, `refused`, `superseded`); `inLine`, every job
  not yet settled; `releasedSinceBaseline`, jobs released with `settledAt`
  from the baseline's time to `to`.
- `items`: work items by kind. `opened` is items created in the window, by
  their first saved kind. `closed` is closings in the window: a revision
  whose status is `done` or `dismissed` while the item's previous saved
  revision's status was neither, or an item created closed. Reopening is
  not a closing, and a second terminal status is not a second closing.
  `openNow` is items open at `to`: each item's latest saved revision before
  `to`, when its status is `open`, `in_progress` or `blocked`.
  `netSinceBaseline` is `openNow` minus the baseline's open counts.
- `baseline`: the stored daily baseline of the latest 21:00 in `tz`
  strictly before `to`, absent when that time is still ahead of the hub's
  clock.
- `host`: the host census the team queue stores, of the host its lane
  summary reads (the first queued entry's host, else the first launching or
  running entry's), else of the most recently observed host: free disk and
  the free-disk reserve, memory pressure, the lanes' strain threshold
  (default 70) and whether pressure is over it, matrix waiters and when the
  census was observed. Absent when the hub stores no census.
- `teams`: launching and running entries, the concurrency limit, the lanes'
  hold (empty when they admit) and every stalled entry with its cause.

### Daily baselines

The first owner status read whose `to` is after a day's 21:00 in `tz`, once
that 21:00 is past on the hub's clock, stores that day's baseline for the
project and zone: released jobs with `settledAt` before 21:00 and bugs and
features open at 21:00, computed from the release ledger and the saved work
item revisions as of 21:00. The row is written once and never changed:
later reads, reads at the same time and a restarted hub return the same
row, even after more releases and closures. The same day in another zone is
its own row. The hub stores nothing for a 21:00 that has not passed.

## Sending on the hub schedule

The report is sent by the product, not written by the session that sends
it. The hub's wake scheduler (`wi_8d18e0fe8bdf2ad6`; `docs/session-handoff.md`,
"Wakes on the hub") delivers each due slot to the owner helper as a notice
that says "Do what Board message #N says". Each kind's Board message is one
fixed command, so the helper runs the same step every time and the report's
shape never depends on the model. There is no second scheduler.

Set up once per project, as the owner or the registered owner helper:

1. Post three Board messages whose whole text is the command for one kind:

   ```
   tt owner report hourly --post
   tt owner report morning --post
   tt owner report evening --post
   ```

2. Schedule one hub wake per kind, naming each message's number:

   ```
   tt handoff wake schedule --kind hourly-update --schedule '0 * * * *' --message N1 --request-id owner-report-hourly-wake
   tt handoff wake schedule --kind morning-summary --schedule '0 7 * * *' --message N2 --request-id owner-report-morning-wake
   tt handoff wake schedule --kind evening-summary --schedule '0 21 * * *' --message N3 --request-id owner-report-evening-wake
   ```

   The zone defaults to `America/Los_Angeles`; give `--zone` to change it,
   and run the report with the same `--tz`. Check with `tt handoff wake list`.

The hub wakes deliver nothing until their activation, `wi_3b7daaaab7d0f188`,
is released and switched on. Until then the command can be run by hand.

### The scheduled post and the ack gate

The hub refuses a post from an agent that holds acknowledgement work (an
assignment, request, review or question) past its grace period
(`docs/broker-phase-3.1.md`), and a scheduled report must not be lost to
that. The rule: an agent's post passes the gate when it is the first reply,
from the agent's current run, to the notice of its own firing of an
`hourly-update`, `morning-summary` or `evening-summary` wake, that firing
is the wake's latest, and the post has the report's own shape: plain text,
with no envelope, no recipient and no work-item links, as `--post` sends it. The hub checks
its own firing record (the notice, the recipient and the wake's kind), so no
message kind, text or request ID can claim the rule. The reply closes the
notice's delivery obligation, so the report is the reply that closes the
wake notice. The gate is not weakened for anything else: a plain post, a directed or
enveloped (typed) reply even to the report's wake, a reply to any other wake
(such as `queue-check`) or notice, a reply to an older firing's notice once
the next slot has fired, a second reply to the same wake, another agent's
reply, a stale or missing run and every work-item, verification, release or
decision write stay refused until the work is acknowledged or answered. The unacknowledged work itself stays open.

`--post` from an agent reads the project's wakes and replies to the notice
of the caller's own firing of the kind's wake whose due time falls in the
reported slot. The owner, or an agent with no such firing, posts without a
reply, as before.

Recovery when a post is refused: the command prints the report, then "the
report was not posted" with the hub's list of unacknowledged work, and
exits 1. Acknowledge or answer each listed message (`tt ack SEQ`, or reply
to it), then run the same command again. For an earlier slot, add
`--now <that slot's end>`: with `--post` the window snaps to the slot at or
before `--now`, and the slot's request ID posts it once. Once a later slot
has fired, an earlier slot's report no longer passes the gate by its wake,
so acknowledge the listed work first.

What `--post` does:

- It moves the window end to the kind's slot (see "Windows"), renders the
  report and prints it exactly as without `--post`.
- It posts the same bytes to the Board as one plain message with no
  recipient, as the caller: the owner helper's agent and run, or the owner.
  From an agent it is the reply to the caller's own wake notice for the
  slot when there is one (see above).
- The post's request ID is `owner-report-<kind>-` followed by a hash of the
  project, kind and window. Before posting, the command looks that ID up in
  the caller's post receipts; when the slot was already posted it posts
  nothing and prints `already posted #<seq> for this slot`, even when the
  data has changed since. Otherwise it prints `posted #<seq>`. A run that
  loses a race with another run of the same slot finds the other run's post
  the same way.
- A report with an unavailable section is still posted, so the owner sees
  what could not be read, and the command still exits 1. A refused or failed
  post prints the report, then exits 1 with "the report was not posted".

## Exit status

The report is always printed to standard output. When every read succeeds,
the command exits 0. When any read fails, the failed section shows its
unavailable line, every other section is printed as usual, each failure is
written to standard error, and the command exits 1. With `--post`, a post
that fails also exits 1.

Bad arguments (an unknown kind, an unparsable `--now`, `--since` or `--tz`,
or `--since` not before `--now`) exit 1 before any hub read.

## Known limits

- Releases are counted as the hub's release ledger records them. A job
  released as a member of a batch carries its own `released` receipt and the
  lead's settled time, so it is counted and shipped like any other job.
- A refused job carries `settledAt`, the hub time of its refusal by the
  deployer or by the handler's reconcile, so it counts in the window that
  holds it. Jobs refused before `wi_bd0310b7bbcd4a52` have no settled time,
  so no window can contain them; they are left out of Releases and What went
  wrong.
- Host health shows what the runner's census stores: free disk, memory
  pressure and matrix waiters. Swap, load average, `fseventsd` and the names
  of host-lock holders are not stored by the hub and are not shown.
- Opened, closed and open counts read the saved work item revisions. An
  item whose early history the hub kept only as a checkpoint of its current
  row counts from that checkpoint's time, and a checkpoint is never counted
  as a closing.
- Up next uses the queue's up-next numbers (`wi_35e9`).
- The hub refuses a Board message over 8 KiB, so a report that long (a very
  long queue or many open owner obligations) is printed but not posted.
- The ack-gate rule checks the report's shape, not its text: a gated agent
  can still send one plain, undirected message of any text as the first
  reply to its own latest report wake notice.
- Post receipts are per caller: the same slot posted once by the owner
  helper and once from the owner terminal gives two posts.
