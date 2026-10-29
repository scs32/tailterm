# Claude wake delivery, 2026-10-26 through 2026-10-28 (UTC)

The after-release measurement for the wake-lag fix (bug `wi_b6b91a5200151bdf`) and the relay pass split (bug `wi_44e5b080b7e7d846`). Item `wi_7c80e947014908b4` revision 4, order #50087 with supplement #50111, assignment #50228. Window [2026-10-26T00:00Z, 2026-10-29T00:00Z). Both parents were released to the Mac mini on 10-25, and the relay restarted onto each build (#50086).

Everything was read only: the relay log, the relay state directory and the owner-helper handoff notes. No pane was typed into, the relay was not signalled and nothing took the host lock.

## Summary

| UTC day | Pre-Enter failures (class) | Broker delay n / median / p90 | Inbox delay n / median / p90 | m2 broker | m2 inbox | Swap sample | fseventsd sample |
|---|---|---|---|---|---|---|---|
| 10-26 | 2: `input empty` 1, `input unreadable` 1 (both broker) | 2 / 20 s / 20 s | 0 | pass | no data | 38.4 of 38.9 GB at 14:20Z | 35 GB at 14:20Z |
| 10-27 | 0 | 0 | 0 | no data | no data | no sample | no sample |
| 10-28 | 0 | 0 | 0 | no data | no data | 9.3 of 10.2 GB at 04:22Z | no sample |

| UTC day | Slow-pass lines (held back) | Slow team-queue-pass lines (held back) | Longest revisit, lower bound | 60 s target | Dominant phase |
|---|---|---|---|---|---|
| 10-26 | 67 (53), lines 76219-77341 | 18 (2), lines 76227-77188 | at least 155.6 s | miss | bindings 66, start 1 |
| 10-27 | 133 (129), lines 77343-79140 | 30 (6), lines 77365-78992 | at least 168.1 s | miss | bindings 133 |
| 10-28 | 97 (100), lines 79145-81142 | 25 (6), lines 79181-81143 | at least 119.5 s | miss | bindings 97 |

Findings:

- **m2: no miss, but mostly no data.** Pre-Enter failures fell from 131 on 10-24 to 2 in the whole window. The retry watch pressed Enter for both 20 s later, and that Enter retry is what the delay measures.
  - The 00:36:36Z retry (line 76217) was followed by `Claude new user turn did not confirm within five seconds` (line 76218). The agent's next wake at 00:41:42Z (line 76224) did not confirm either.
  - The 20:43:19Z retry (line 77106) has no such line.
  - Five of the six day-and-path cells have no failures, so they have no data, not a pass.
- **m4: miss on all three days.** Agents are visited one after another, and that is now the slow phase of a relay pass. On every day, at least one live binding waited more than twice the 60 s target between visits.
- **The team queue claim is not confirmed.** The slow-pass lines name the binding visits, not the team queue (see m4).

## Sources and day boundaries

- Relay log: `~/Library/Logs/Tailterm/inbox-relay.log` on Stephens-Mini. It was read on 2026-10-28 at about 18:45 PDT, when it held 81,535 lines and was still growing.
  - Lines 1-81155 hash to `3d1c7ca3ae72c9c5fdf1b37fd62aa2a89226f70c7d70afdf03aa32e910817e30` (`sed -n '1,81155p' LOG | shasum -a 256`).
  - The window lines, 76144-81155, hash to `c6ef2e15d11451ed0db881b00773038fc774195595a3f1c44f85cfeed6a0dc73`.
  - The log is append-only, so these line numbers stay valid until it is rotated.
- Day ranges are inclusive:

  | UTC day | Lines | First timestamped line | Last timestamped line |
  |---|---|---|---|
  | 10-26 | 76144-77341 | 76144, 00:05:12Z | 77341, 23:58:43Z |
  | 10-27 | 77342-79144 | 77342, 00:00:17Z | 79140, 23:48:46Z |
  | 10-28 | 79145-81155 | 79145, 00:15:59Z | 81154, 23:52:13Z |

  The lines on either side of the window are 76143 (10-25T23:56:03Z) and 81156 (10-29T00:01:26Z).
- Rule for untimestamped lines, such as `connection refused` lines and the second line of each wake report: an untimestamped line belongs to the day of the nearest timestamped line above it.
  - Only two stretches of untimestamped lines sit where a day changes. Lines 79141-79144 are four `worktree cleanup` lines, and line 81155 is the second line of the 23:52:13Z report.
  - Neither stretch holds a failure, delivery or slow-pass line, so the rule changes no count.
- Every counted line carries its own timestamp. Untimestamped lines are never counted.
- Timestamps are not strictly in order: some lines are written up to 18 s after a later-stamped line, for example lines 78895-78896. No timestamp falls outside its line's day range. This was checked for every timestamped line in 76144-81155.

## m1: pre-Enter failures by class

A failure is one line that matches the regex below:

```
[tt relay] <time>Z <agent> wake skipped: [broker wake <wake_id> ambiguous: ]Claude input verification before Enter failed; did not confirm: <class> after <time>...
```

The relay writes one more line for each failure, without a timestamp: `<agent> broker wake: ...` or `<agent>: Claude input verification ...`. That line repeats the first one and is not counted.

The path comes from the line:

- **Broker:** the line contains `broker wake <wake_id> ambiguous:`.
- **Inbox:** the line does not contain it.

`grep -c 'Claude input verification before Enter failed'` over each day's range returns 4, 0 and 0 lines. That is two failures, each written as two lines.

| UTC day | Line | Agent | Path | Class |
|---|---|---|---|---|
| 10-26 | 76215 | `agt_ee9b2aa6fb6dff29` | broker | `input empty after 2.975s: Claude input is occupied, prompting, or unknown` |
| 10-26 | 77104 | `agt_d7675a9d13bc353c` | broker | `input unreadable after 3.077s: Claude transcript busy, incomplete, or unknown: turn in progress` |
| 10-27 | none | | | |
| 10-28 | none | | | |

Both checks ran for the new bound of about 3 s; until 10-24 the bound was 1 s. One of the two is a new class. On 10-24 every failure was `input empty`.

Excluded on purpose: `Claude new user turn did not confirm within five seconds` and `Claude is not safely idle` lines. These are failures after Enter or before typing, not the check before Enter.

## m1: delay from failure to delivery

**Pairing**, the same as the baseline as far as the baseline wrote it down:

- Each agent's first failure is held open until that agent's next delivery line.
- Later failures for that agent while it is open are not paired again.
- Each pair is reported under the UTC day and path of its failure.

**Delivery** means the first later line that matches `<time>Z <agent> Claude wake retry N/4: Enter on own unsubmitted text`, with or without ` (retry watch)`.

- The log has no line that marks a confirmed wake. No line contains `confirmed` other than `unconfirmed` and `did not confirm`. So an Enter retry is the only delivery line there is.
- `Claude wake retry N/4: retyped after lost text` is a retype, not an Enter on the relay's own text, and is not counted.

**Percentiles:** the median is the usual median. p90 is nearest rank: the ⌈0.9·n⌉-th smallest value.

| UTC day | Path | Failure line | Delivery line | Delivery text | Delay |
|---|---|---|---|---|---|
| 10-26 | broker | 76215 (00:36:16Z) | 76217 (00:36:36Z) | `agt_ee9b2aa6fb6dff29 Claude wake retry 1/4: Enter on own unsubmitted text (retry watch)` | 20 s |
| 10-26 | broker | 77104 (20:42:59Z) | 77106 (20:43:19Z) | `agt_d7675a9d13bc353c Claude wake retry 1/4: Enter on own unsubmitted text (retry watch)` | 20 s |

| UTC day | Broker n / median / p90 | Inbox n / median / p90 |
|---|---|---|
| 10-26 | 2 / 20 s / 20 s | 0 |
| 10-27 | 0 | 0 |
| 10-28 | 0 | 0 |

Both deliveries came from the retry watch, 20 s after the failed check: the 3 s check, the 15 s backoff and the 2 s watch tick.

Delivery here means the Enter retry, as defined above. It does not mean the new turn was confirmed:

- **`agt_ee9b2aa6fb6dff29`:** the 00:36:36Z retry (line 76217) was followed by `[tt relay] 2026-10-26T00:36:42Z agt_ee9b2aa6fb6dff29 Claude retry watch: Claude new user turn did not confirm within five seconds` (line 76218). The agent's next wake, at 00:41:42Z (line 76224, `wake skipped: Claude new user turn did not confirm within five seconds seqs=[46651]`), did not confirm either.
- **`agt_d7675a9d13bc353c`:** the 20:43:19Z retry (line 77106) has no such line. No later line for that agent in the window reports an unconfirmed turn.

The delay figures stay as defined.

**Calibration against the baseline.** The same script, run over 10-24 (lines 73607-74960), gives:

| 10-24 count | This method | Baseline (#45774) |
|---|---|---|
| Failures | 131 (85 broker, 46 inbox) | 119 |
| Broker n / median / p90 / max | 81 / 268 s / 427 s / 1,388 s | 80 / 264 s / 449 s / 1,388 s |
| Inbox n / median / p90 / max | 46 / 305.5 s / 499 s / 849 s | 46 / 300 s / 499 s / 2,763 s |

The baseline did not record its exact script. The small gaps in day counts and order statistics do not touch this window, where both pairs are unambiguous.

Script, run as `python3 -I pair.py ~/Library/Logs/Tailterm/inbox-relay.log 76144 81155`. The first and last arguments bound the failure lines; delivery lines are searched to the end of the file.

```python
# Pair each agent's first pre-Enter failure with its next Enter retry.
# Usage: python3 -I pair.py LOG FIRST LAST   (inclusive 1-based failure line range)
import re, sys, statistics
from datetime import datetime
log, first, last = sys.argv[1], int(sys.argv[2]), int(sys.argv[3])
TS = r'\[tt relay\] (\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ) (agt_[0-9a-f]+) '
FAIL = re.compile(TS + r'wake skipped: (broker wake wake_[0-9a-f]+ ambiguous: )?Claude input verification before Enter failed; did not confirm: (input \w+) after')
DELIV = re.compile(TS + r'Claude wake retry \d/4: Enter on own unsubmitted text')
t = lambda s: datetime.strptime(s, '%Y-%m-%dT%H:%M:%SZ')
open_, pairs, unpaired, classes = {}, [], [], {}
with open(log, errors='replace') as f:
    for n, line in enumerate(f, 1):
        m = FAIL.search(line)
        if m and first <= n <= last:
            path = 'broker' if m.group(3) else 'inbox'
            day = m.group(1)[:10]
            classes[(day, path, m.group(4))] = classes.get((day, path, m.group(4)), 0) + 1
            if m.group(2) not in open_:
                open_[m.group(2)] = (n, m.group(1), path)
            continue
        m = DELIV.search(line)
        if m and m.group(2) in open_:
            fn, fts, path = open_.pop(m.group(2))
            pairs.append((fts[:10], path, (t(m.group(1)) - t(fts)).total_seconds(), fn, n))
for k, v in sorted(classes.items()): print('class', *k, v)
for p in pairs: print('pair', *p)
for a, (fn, fts, path) in open_.items(): print('unpaired', fts, path, a, fn)
def p90(xs):  # nearest rank
    xs = sorted(xs); return xs[max(0, -(-9 * len(xs) // 10) - 1)]
for day in sorted({p[0] for p in pairs}):
    for path in ('broker', 'inbox'):
        xs = [p[2] for p in pairs if p[0] == day and p[1] == path]
        if xs: print('stats', day, path, 'n=%d median=%g p90=%g max=%g' % (len(xs), statistics.median(xs), p90(xs), max(xs)))
```

Output for 76144-81155:

```
class 2026-10-26 broker input empty 1
class 2026-10-26 broker input unreadable 1
pair 2026-10-26 broker 20.0 76215 76217
pair 2026-10-26 broker 20.0 77104 77106
stats 2026-10-26 broker n=2 median=20 p90=20 max=20
```

## m1: host load samples

The relay log records neither swap nor fseventsd. The only records found for the window are the owner-helper handoff notes, `/Users/stephenspeicher/projects/tailterm/.build/owner-session-handoff.md`. That file is outside the repository and still being edited. Line numbers below are as read on 10-28 at about 18:45 PDT, so each source is also quoted by its update heading. Note times there are PDT (UTC−7).

I also searched `~/projects/tailterm-artifacts` for numeric swap or fseventsd figures, in `.md`, `.txt` and `.log` files changed 10-26 to 10-29. It found only the same 08:44 PDT fseventsd restart on 10-26 (`backlog-steward/summary-archive-10-26.md:21`).

| UTC day | Swap | fseventsd | Source |
|---|---|---|---|
| 10-26 | 38.4 of 38.9 GB at 14:20Z | 35 GB at 14:20Z | Line 1258, "Update 07:20 10-26: ... Swap 38.4/38.9 GB, fseventsd 35 GB" |
| 10-27 | no sample | no sample | No note dated between 17:00 PDT 10-26 and 17:00 PDT 10-27 gives a swap or fseventsd figure. Line 1293, "Update 17:14 10-26" (00:14Z), records only that the owner did not run killall. |
| 10-28 | 9.3 of 10.2 GB at 04:22Z | no sample | Line 1339, "Update 21:22 10-27: ... Swap 9.3/10.2 GB (vmware-vmx 7.5 GB; fseventsd not top 5)". It gives no fseventsd size, so the fseventsd cell has no sample. |

Context for the 10-26 sample: fseventsd restarted at 08:44 PDT (15:44Z), and swap fell from 40 to 8.3 GB (line 1268, "Update 09:20 10-26"). A separate record dated 10-28 gives swap 9.0 of 10.2 GB at 19:19Z on 10-28 (line 1363, "Update 12:19 10-28"). It is not used, because the 10-28 sample in the table is the earlier 04:22Z one.

## m2: verdict

The thresholds are a median of at most 30 s and a p90 of at most 60 s. A path and day with n=0 is reported as no data.

| UTC day | Broker | Inbox |
|---|---|---|
| 10-26 | pass (median 20 s, p90 20 s, n=2) | no data |
| 10-27 | no data | no data |
| 10-28 | no data | no data |

No day or path misses. Five of the six cells have no failures to time, so m2 cannot be called a pass on both paths every day. With n=2 even 10-26's broker pass is thin. Failure counts are reported, not judged: 2, 0 and 0, against 131 on 10-24 by the same rule.

## m4: relay passes

### Slow-pass lines

Bug `wi_44e5b080b7e7d846` revision 1 added these lines. Their format is `<time>Z slow pass <total>s: start <s>s, bindings <s>s (<k> bindings), end <s>s; slowest step <step> <s>s; slowest hub call ...; <m> slow passes not logged`.

- A pass of 30 s or less is never logged.
- At most one line prints per 2 minutes.
- "Held back" is the sum of the `not logged` counts.

| UTC day | Lines (count, held back) | Median logged pass | Bindings per pass | Logged passes over 60 s | Longest pass |
|---|---|---|---|---|---|
| 10-26 | 76219-77341 (67, 53) | 46.3 s | 23 (11-28) | 20 | 163.2 s, line 76774, 18:33:36Z |
| 10-27 | 77343-79140 (133, 129) | 46.7 s | 25 (13-31) | 44 | 172.7 s, line 78891, 21:26:50Z |
| 10-28 | 79145-81142 (97, 100) | 42.4 s | 21 (8-26) | 23 | 124.7 s, line 81034, 22:40:26Z |

Bindings per pass is the median, with the range in brackets.

Day 3 includes the slow passes of incident I-49964 (bug `wi_ca88a4f24edfd5c1`), which were diagnosed in #49976 as "relay passes 30-125 s with hub calls timing out". The longest passes that day were:

| Line | Time | Pass |
|---|---|---|
| 81029 | 22:38:18Z | 107.0 s |
| 81034 | 22:40:26Z | 124.7 s |
| 80169 | 17:41:39Z | 101.5 s |
| 79440 | 02:04:40Z | 99.0 s |

### Slow team-queue-pass lines

Format: `<time>Z slow team queue pass <s>s[; <m> slow team queue passes not logged]`.

| UTC day | Lines (count, held back) | Longest |
|---|---|---|
| 10-26 | 76227-77188 (18, 2) | 89.8 s |
| 10-27 | 77365-78992 (30, 6) | 123.2 s |
| 10-28 | 79181-81143 (25, 6) | 133.2 s |

Together these account for every `slow pass` (297) and `slow team queue pass` (73) line in 76144-81155. The script reports no unparsed lines.

### Longest interval between visits to a live binding

**Limits.** The progress files in `~/.local/state/tailterm/relay` (`*.progress.json`) hold only the current state. Their mtime and `lastHeartbeat` give the last visit, not a history, so they cannot give intervals for a window that has already closed. They were not used for any figure here. The relay log has no line for each visit. Passes of 30 s or less and held-back slow passes have no duration. So the longest interval can be bounded from below, not measured.

**Bound.** At `hub/cmd/tt/relay.go` (base `e1d3b551`), the loop works like this:

- It visits every binding once per pass, one after another, in `filepath.Glob` order, which is sorted (lines 1739-1776).
- It then runs the end steps and sleeps 3 s (`relayLoopPause`, line 1332).
- Since `wi_44e5` r2, the team queue tick runs in its own pass and does not delay the next pass.

Take the binding visited first in pass k. If it is still live in pass k+1, its visit starts after the bindings and end phases of pass k, the 3 s pause and the start phase of pass k+1. So its interval is at least bindings_k + end_k + 3 s. The largest such value among logged passes is a lower bound on the day's longest revisit interval.

| UTC day | Lower bound | Logged passes over 60 s | 60 s target |
|---|---|---|---|
| 10-26 | 155.6 s (line 76774) | 19 | miss |
| 10-27 | 168.1 s (line 78891) | 43 | miss |
| 10-28 | 119.5 s (line 81034) | 23 | miss |

The bound equals the logged pass total minus its start phase, plus 3 s. On 10-25, before the fix, a sampler measured a longest interval of 462 s (`docs/claude-wake.md`, "Relay passes").

### Which phase the slow-pass lines name

Each line was classed two ways: by the largest of its start, bindings and end times, and by its `slowest step`.

| UTC day | Largest phase | Slowest step |
|---|---|---|
| 10-26 | bindings 66, start 1 | an agent visit (`binding agt_…`) 66, `retirement recovery` 1 |
| 10-27 | bindings 133 | agent visit 122, `cleanup` 10, `retirement recovery` 1 |
| 10-28 | bindings 97 | agent visit 94, `cleanup` 1, `pending bindings` 1, `retirement recovery` 1 |

The agents most often named as the slowest visit were:

| UTC day | Agent (times named) |
|---|---|
| 10-26 | `agt_08208b2950673047` (8), `agt_ee9b2aa6fb6dff29` (8), `agt_df3e1a0b91d3a680` (6) |
| 10-27 | `agt_df3e1a0b91d3a680` (16), `agt_08208b2950673047` (14), `agt_1c44c7c1f6ac6e04` (11) |
| 10-28 | `agt_df3e1a0b91d3a680` (17), `agt_08208b2950673047` (14), `agt_0afba319087fc030` (13) |

Context from the same script over 10-25 (lines 74961-76143), the day the lines started:

- 81 slow-pass lines; the start phase was largest in 73.
- `retirement recovery` was the slowest step in 73. Bug `wi_b3227351ffa5331e` later fixed this.
- 12 slow team-queue-pass lines, the longest 70.5 s.

**Verdict on "the gap was the team queue tick": not confirmed, and it is not the whole explanation.**

- A slow-pass line can name the team queue only under `--once`. In the live loop the tick runs in its own pass, so these lines cannot attribute the 10-25 gap to it.
- What they do show is that after the split, the binding visits themselves are the largest phase in 296 of 297 lines. They push revisit intervals past 60 s on all three days.
- Team-queue passes still reach 133.2 s, near their 2-minute budget. So an inline tick could have added up to 2 minutes per pass, which fits the claim in part. But it cannot explain the 10-25 gaps of up to 381 s alone.
- The 10-25 lines name `retirement recovery` as the slowest step, so that also took time inside the pass on 10-25.

Script, run as `python3 -I slow.py ~/Library/Logs/Tailterm/inbox-relay.log 76144 81155`:

```python
# Summarise slow-pass and slow team-queue-pass lines per UTC day.
# Usage: python3 -I slow.py LOG FIRST LAST   (inclusive 1-based line range)
import re, sys, collections
log, first, last = sys.argv[1], int(sys.argv[2]), int(sys.argv[3])
SP = re.compile(r'\] (\d{4}-\d\d-\d\d)T(\S+)Z slow pass ([\d.]+)s: start ([\d.]+)s, bindings ([\d.]+)s \((\d+) bindings\), end ([\d.]+)s(?:; slowest step (.+?) ([\d.]+)s)?(?:; slowest hub call .+? [\d.]+s)?(?:; (\d+) slow pass(?:es)? not logged)?$')
SQ = re.compile(r'\] (\d{4}-\d\d-\d\d)T(\S+)Z slow team queue pass ([\d.]+)s(?:; (\d+) slow team queue pass(?:es)? not logged)?$')
d = collections.defaultdict(lambda: dict(sp=[], sq=[], phase=collections.Counter(), step=collections.Counter(), held=0, qheld=0))
other = 0
with open(log, errors='replace') as f:
    for n, line in enumerate(f, 1):
        if n < first or n > last: continue
        line = line.rstrip('\n')
        m = SP.search(line)
        if m:
            day, tot, s, b, e = m.group(1), *map(float, (m.group(3), m.group(4), m.group(5), m.group(7)))
            x = d[day]; x['sp'].append((n, m.group(2), tot, s, b, e, b + e + 3))
            x['phase'][max((('start', s), ('bindings', b), ('end', e)), key=lambda p: p[1])[0]] += 1
            step = m.group(8) or '-'
            x['step']['binding' if step.startswith('binding ') else step] += 1
            x['held'] += int(m.group(10) or 0); continue
        m = SQ.search(line)
        if m:
            x = d[m.group(1)]; x['sq'].append((n, m.group(2), float(m.group(3)))); x['qheld'] += int(m.group(4) or 0); continue
        if 'slow pass' in line or 'slow team queue pass' in line: other += 1; print('UNPARSED', n, line[:160])
for day in sorted(d):
    x = d[day]; sp, sq = x['sp'], x['sq']
    if sp:
        big = max(sp, key=lambda r: r[2]); lb = max(sp, key=lambda r: r[6])
        print(f"{day} slow-pass n={len(sp)} lines {sp[0][0]}-{sp[-1][0]} held-back={x['held']} max-pass={big[2]}s@L{big[0]} {big[1]} "
              f"max(bindings+end+3)={lb[6]:.1f}s@L{lb[0]} {lb[1]} passes>60s={sum(r[2] > 60 for r in sp)} lb>60s={sum(r[6] > 60 for r in sp)}")
        print(f"  dominant phase {dict(x['phase'])} slowest step {dict(x['step'].most_common())}")
    if sq:
        print(f"{day} slow-queue n={len(sq)} lines {sq[0][0]}-{sq[-1][0]} held-back={x['qheld']} max={max(r[2] for r in sq)}s")
print('unparsed', other)
```

The median pass, the bindings-per-pass range and the most-named agents come from a short companion pass over the same lines. It parses `slow pass <total>s` and `(<k> bindings)`, and counts the agent in `slowest step binding <agent>`.

## m3: note texts for the handler to save after acceptance

These are drafts. The handler saves them as native notes after lead accepts this report. This report writes no records.

**On `wi_7c80e947014908b4`:**

> Measured 2026-10-26..28 UTC from the Mini relay log, lines 76144-81155 (report `docs/measurements/claude-wake-delivery-2026-10-26-through-28.md`).
>
> - m1/m2: pre-Enter failures 2/0/0, against 131 on 10-24. Both 10-26 failures were broker wakes (`input empty` 2.975 s, `input unreadable` 3.077 s), each delivered 20 s later by a retry-watch Enter. Broker 10-26: n=2, median 20 s, p90 20 s: pass. The other five day-path cells are no data, so there is no miss, but no full pass either.
> - Delivery is the Enter retry, not a confirmed turn. The 00:36:36Z retry (L76217) was followed by "Claude new user turn did not confirm within five seconds" (L76218), and that agent's next wake at 00:41:42Z (L76224) also did not confirm. The 20:43:19Z retry (L77106) has no such line.
> - Load: 10-26 swap 38.4/38.9 GB and fseventsd 35 GB; 10-27 no sample; 10-28 swap 9.3/10.2 GB, fseventsd no sample.
> - m4: miss on all three days. The longest revisit interval is at least 155.6, 168.1 and 119.5 s, against the 60 s target. Slow-pass lines name the bindings phase in 296 of 297. The team-queue claim is not confirmed. Recurrence of `wi_44e5b080b7e7d846`; owner-helper decides the next step.

**On `wi_b6b91a5200151bdf`:**

> After-release measurement (`wi_7c80e947014908b4`, 2026-10-26..28 UTC): no m2 miss. Pre-Enter failures were 2, 0 and 0 per day, against 131 on 10-24. Both were delivered by the retry watch's Enter 20 s after the failed check (median 20 s, p90 20 s, broker n=2). The inbox path and days 10-27 and 10-28 had no failures to time. Not a recurrence of the delay.
>
> The Enter retry is not a confirmed turn. After the 00:36:36Z retry (L76217) the relay logged "Claude new user turn did not confirm within five seconds" (L76218), and the agent's next wake at 00:41:42Z (L76224) also did not confirm. The 20:43:19Z retry (L77106) has no such line.

**On `wi_44e5b080b7e7d846`:**

> Recurrence from `wi_7c80e947014908b4` m4 (2026-10-26..28 UTC). The live-binding revisit target of 60 s is missed every day.
>
> - Lower bounds from the slow-pass lines (bindings + end + 3 s pause): 155.6 s (line 76774), 168.1 s (line 78891) and 119.5 s (line 81034).
> - 19, 43 and 23 logged passes exceed 60 s by that bound alone, and 53, 129 and 100 slow passes were held back.
> - Slow-pass lines name the bindings phase in 296 of 297, and an agent visit as the slowest step in 282.
> - Slow team-queue passes still reach 89.8, 123.2 and 133.2 s, but run beside the visits.
> - The doc's "the gap was the team queue tick" is not confirmed by these lines; the visits are now the bottleneck.
>
> Owner-helper decides the next step.
