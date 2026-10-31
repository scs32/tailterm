# Owner report

`tt owner report` renders the owner's hourly, morning and evening progress
report from one fixed template filled with hub data. The product writes the
report, not the session that sends it, so it has the same shape whichever
model or person runs the command.

Source: owner principle #51832 ("This needs to be templeted and structured"),
bug `wi_91bdb5b6f0f46f97`, work order #53136. The command only renders and
prints. Sending the report on a schedule is not part of it: that is held
follow-up `wi_546279658420371a`, which waits for `wi_8d18`.

## Command

```
tt owner report hourly|morning|evening [--task ID] [--tz ZONE] [--host H] [--now T] [--since T]
```

- `--task`: the project ID. Required unless `TAILTERM_TASK` is set.
- `--tz`: the IANA time zone for the window and every printed time. Default:
  this machine's zone (`TZ`, then `/etc/localtime`, then `UTC`).
- `--host`: the host whose provider usage reading is shown. Default: this
  machine's tailnet name, or its short hostname.
- `--now`: the window's end, RFC3339. Default: the current time.
- `--since`: the window's start, RFC3339, in place of the kind's default
  start. It must be before `--now`.

The command reads from the hub and changes nothing. The owner terminal and
agent sessions (such as the owner helper) may both run it. Other `tt owner`
subcommands still refuse agent sessions.

## Windows

Each report covers the half-open window `[start, end)`, where `end` is
`--now`.

| Kind | Default start |
|---|---|
| hourly | one hour before `--now` |
| morning | the latest 21:00 local time strictly before `--now` |
| evening | the latest 07:00 local time at or before `--now` |

A release counts in the window when its `settledAt` is in it. An
intervention counts when its message's `createdAt` is in it.

## Template

Every report has these nine sections, in this order. Each header is always
printed, even when the section is empty or could not be read.

```
**Owner report: <Hourly|Morning|Evening>** <YYYY-MM-DD HH:MM>–<HH:MM> <ZONE>
**Shipped** (<n>)
- <item id> <title>: <delivered work>. Limits: <limitations>
**Releases**
- released <r>, rolled back <b>, refused <f>, superseded <s> in this window; in line now: <a>
**Running teams** (<n> of <limit>)
- <item id> <title> (<template>, <state>)
**Up next**
- not yet available (waits for wi_35e9 / wi_475a)
**What went wrong** (<n>)
- release <job id> for <item id> rolled back at <step>: <reason>
- release <job id> for <item id> refused at <step>: <reason>
- intervention #<seq> <kind> on <item id> by <actor>
**Opened vs closed**
- not yet available (waits for wi_35e9 / wi_475a)
**Usage**
- <runtime> <window>: <used>% used, resets <Day HH:MM> (reading <age> old)
- <runtime> <window>: <state>
- project tokens in window: <class> <n>, <class> <n>, ...
**Host health**
- not yet available (waits for wi_35e9 / wi_475a)
**Needs you** (<n>)
- #<seq> <source kind>: <subject or needs> (due <HH:MM>[, overdue <overdue>])
```

When the window's start and end fall on different local days, the header
shows the end's date too (`2026-10-30 21:00–2026-10-31 07:00 UTC`).

### Sections and their sources

| Section | Hub read | What it shows |
|---|---|---|
| Shipped | settled releases (`GET /v1/tasks/{id}/releases?view=settled`, every page) and each item's narrative (`GET /v1/tasks/{id}/work-items/{wid}/narrative`, then the pinned completion report version) | Each item with a `released` or `superseded` job in the window, once, ordered by its first time in the window and then by item ID. Title from the work item; delivered work and limitations from the item's pinned completion report. |
| Releases | settled releases, plus every page of `view=active` | Settled jobs in the window by state. "In line now" counts every job not yet settled. |
| Running teams | `GET /v1/tasks/{id}/team-queue?view=active` | Entries in state `launching` or `running`, ordered by queue position; the limit is the project's concurrency limit. |
| Up next | none yet | A fixed line. |
| What went wrong | settled releases and `GET /v1/tasks/{id}/interventions` (every page) | Refused and rolled-back jobs in the window, with the runner's fixed failure step and reason tag (never command output), and owner interventions in the window. Ordered by time, then ID. The actor reads `unknown` when the hub recorded none. |
| Opened vs closed | none yet | A fixed line. |
| Usage | `GET /v1/provider-usage?host=H` and `GET /v1/tasks/{id}/usage?from=&to=` | Each stored provider reading for the host, ordered by runtime and then window. A reading that is not `ok` prints its state. Then the project's token classes in the window, sorted by class, exactly as the hub reports them. |
| Host health | none yet | A fixed line. |
| Needs you | `GET /v1/tasks/{id}/obligations?owner=1&open=1` | Every open owner obligation, ordered by due time and then message number. The subject is shown, or what the obligation needs when it has no subject. A due time on another day than the window's end shows its date (`Oct 30 15:04`). |

The narrative is read once per item, even when an item is both shipped and
running.

Saved free text (titles, delivered work, limitations, subjects) is cleaned
the same way every time. Runs of whitespace become single spaces, the ends are
trimmed, and anything longer than 280 characters is cut and ends in `…`. A
trailing full stop on delivered work is dropped before `. Limits:`.

## Fixed lines

These lines are printed exactly as shown.

| When | Line |
|---|---|
| Up next, Opened vs closed and Host health (always, until their data lands) | `- not yet available (waits for wi_35e9 / wi_475a)` |
| No item shipped in the window | `- none in this window` |
| A shipped item has no pinned completion report | `- <item id> <title>: no saved completion report` |
| A delivered-work or limitations field is empty | `none recorded` in its place |
| No team is launching or running | `- none running` |
| Nothing went wrong in the window | `- none recorded (sources: refused or rolled-back releases, owner interventions)` |
| No provider reading for the host | `- no provider reading for host <host>` |
| No project tokens recorded in the window | `- project tokens in window: none recorded` |
| No open owner obligation | `- nothing waiting on you` |
| A read failed | `- unavailable: could not read <source>` |

The sources a failed read names are `the release ledger`, `the team queue`,
`owner interventions`, `provider usage`, `project usage`,
`owner obligations` and, for a single shipped item,
`the work item narrative` (as `- <item id> <title>: unavailable: could not
read the work item narrative`). A section that could not be read prints its
header without a count.
A running team whose narrative cannot be read is listed without its title,
and the read counts as a failure (see Exit status).

## Exit status

The report is always printed to standard output. When every read succeeds,
the command exits 0. When any read fails, the failed section shows its
unavailable line, every other section is printed as usual, each failure is
written to standard error, and the command exits 1.

Bad arguments (an unknown kind, an unparsable `--now`, `--since` or `--tz`,
or `--since` not before `--now`) exit 1 before any hub read.

## Known limits

- Releases are counted as the hub's release ledger records them. A job
  released as a member of a batch carries its own `released` receipt and the
  lead's settled time, so it is counted and shipped like any other job.
- A job the deployer refused before publishing has no settled time in the
  hub, so no window can contain it. It is left out of Releases and What went
  wrong. Only a refusal recorded through the handler's reconcile carries a
  time.
- Up next, opened vs closed counts (bugs vs features) and host health wait
  for `wi_35e9` and `wi_475a`. Until then they print the fixed line above.
  Filling them, and scheduled sending, are tracked in held item
  `wi_546279658420371a`.
