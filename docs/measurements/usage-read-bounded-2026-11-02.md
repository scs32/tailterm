# Bounded usage read: live-size measurement, 2026-11-02

Bug `wi_dd392009b4f40b18` r4, owner order #56299 with correction #56312, builder assignment #56358,
lead answer #56508. Builder builder-b4f40b18 on Stephens-Mini (Apple M4, go1.26.5).
Every store is synthetic and lives in a temp directory the benchmark removes when it ends. No live hub store was read.

## Candidate

- Measured commit `be7c9573774ab18a561144e98521a14c2790951b` on tasks-hub `0ba953cd04c20e48fe8be710da4ab994f05479b3`.
  It is two commits: 6481c62d, the side tables, the share totals and the indexed listing; and be7c9573, the faster span backfill.
- The first rehearsal ran at `0dcb391d221511e3df3d7d56f3d710360efa5f3c` on `ec82f5e7`. That commit is the same as 6481c62d.
- Before figures are the parent's runs at the same read code:
  - 1x at `f3adb7bb`, record `tailterm-artifacts/builder-4555aec5/wi_f8f4-bench-after-out/m2.txt`.
  - 2x at `9eb80197`, record `tailterm-artifacts/builder-4555aec5/wi_f8f4-bench-2x.stdout.txt` and lock `wi_f8f4-bench-2x/host-lock.json`.
  - They were not run again.

## Host holds (all via db-handler-o5 admission and primary selection)

| Hold | Purpose | Commit | Lock seq | Held (UTC) | Exit | Record |
|---|---|---|---|---|---|---|
| H1 | m3 rehearsal | 0dcb391d | 2136 | 20:20:24.930-20:22:30.773 | 0 | `tailterm-artifacts/builder-b4f40b18/wi_dd39-backfill/` |
| H1 re-run | m3 rehearsal with breakdown | be7c9573 | 2138 | 21:37:38.957-21:40:16.664 | 0 | `.../wi_dd39-backfill-r2/` |
| H2 | m4, 1x and 2x in one hold | be7c9573 | 2139 | 21:50:07.539-21:58:52.879 | 0 | `.../wi_dd39-bench/` |

- Each hold was `verify-matrix-host-lock.mjs exec --exclusive --timeout-minutes 10` with `go test -timeout 9m`.
  Each started on an empty host (no holder, no waiters) with no synthetic load, and none of my other processes ran during it.
- Host load average and memory pressure were not captured at the holds. At the first H1 admission (#56466) the handler recorded pressure 84, which allows one holder on an empty host.
- Combined outputs: `wi_dd39-backfill-out.txt`, `wi_dd39-backfill-r2-out.txt` and `wi_dd39-bench-out.txt` in the same directory.
- **H2 held for 8 min 45 s.** That is over the 8-minute pre-start estimate, though inside the hard 10 minutes. I had estimated about 4 minutes.
  The 2x seed took 3 min 34 s, against the parent's 2 min 10 s. The side-table rows and the share totals trigger cost more at 2x than I projected from 1x.
  Both scales ran once, with no retry.

## Seeded stores

- 1x: 1,457 items, 3,200 agents, 54,500 messages, 30,000 obligations, 40,000 item links, 250,000 turns and their totals and share rows, 250,000 spans.
  - The new tables: 250,000 span instants, 75,000 poll rows and 1,457 share totals.
- 2x: 54,500 obligations and 54,500 item links (the messages cap them), 500,000 turns, shares and spans.
  - The new tables: 500,000 span instants and 1,457 share totals.
- Seeding took 64.4 s at 1x and 213.8 s at 2x. Seeding is not part of any stage.
- Both scales keep the same 35-day history, so at 2x the measured hour holds twice the spans and turns.

## m4: per-stage time and rows read, 1h window (N=3 median; N=1 in brackets)

| Stage | 1x before | 1x after | 2x before | 2x after |
|---|---|---|---|---|
| Whole read | 2.181 s | 0.286 s (0.748 s) | 5.978 s | 0.530 s (1.409 s) |
| S0 prices/items | 3 ms | 3 ms | 6 ms | 3 ms |
| S1 turns | 207 ms | 79 ms | 639 ms | 156 ms |
| S2 spans | 1.204 s | 41 ms (91 ms) | 3.709 s | 96 ms (193 ms) |
| S2 rows decoded / span keys read | 300 / 250,000 | 300 / 300 | 601 / 500,000 | 601 / 601 |
| S2 queries | 602 | 602 | 1,205 | 1,205 |
| S3 waits | 178 ms | 112 ms | 408 ms | 219 ms |
| S4 agents | 3 ms | 1 ms | 3 ms | 1 ms |
| S5 budgets | 582 ms | 46 ms (45 ms) | 1.203 s | 48 ms (56 ms) |
| S5 rows read (derived) | 250,000 share rows | 1,457 totals rows | 500,000 share rows | 1,457 totals rows |
| S5 queries | 4,371 | 4,371 | 4,371 | 4,371 |

- S2's span keys read is the `scanned` count. Before the change it was every span of the project; it now equals the spans the window lists.
- S5 rows read is counted outside the timed reads. "Before" is what the earlier per-denominator sum over share rows would read for the report's 1,457 items.
- The before runs printed S5 rows as 0, as the stage then counted only queries.
- S1 and S3 are outside this item's change. Their differences between the runs are not attributed to it.

### S2 plus S5, 2x against 1x (criterion: at most 1.2)

- After, N=3 medians: 2x = 96 + 48 = 144 ms; 1x = 41 + 46 = 87 ms; **ratio 1.66. Fails the 1.2 bound.**
- After, N=1: 2x = 193 + 56 = 249 ms; 1x = 91 + 45 = 136 ms; ratio 1.83.
- Before, N=3: 2x = 3,709 + 1,203 = 4,912 ms; 1x = 1,204 + 582 = 1,786 ms; ratio 2.75.

S5 no longer grows: 46 ms at 1x and 48 ms at 2x, with the same 4,371 queries and 1,457 rows.
S2 no longer reads history: span keys read equal the window's spans at both scales.
The window itself doubles, from 300 spans to 601, because the fixture keeps 35 days at 2x. S2 decodes and classifies each of those spans with two queries, so S2 roughly doubles: 41 to 96 ms.
Per window span, S2 is 0.137 ms at 1x and 0.160 ms at 2x.
The plan's risk R1 predicted this (S2 doubles with the window's spans, S5 constant, ratio 1.1-1.25). The measured ratio is higher, because S5 fell further (to about 46 ms) than the plan's 50-80 ms.

## m3: migration rehearsal on a copy of an isolated 1x store

Method (`BenchmarkUsageBoundedBackfillLiveSize/1x`):
1. Seed 1x.
2. Record the 1h report, all 1,457 budgets (each checked against the earlier per-row sum) and two span listings (each checked against the earlier text scan).
3. Drop the new tables and triggers, which leaves the 60c680e7 schema. Checkpoint and close.
4. Copy the file twice.
5. Open copy 1 and check the counts, then every golden.
6. Reopen copy 1.
7. Open copy 2 in stages.

| | H1 (0dcb391d) | H1 re-run (be7c9573) |
|---|---|---|
| Spans examined by the first open | 250,000 | 250,000 |
| Span instants / poll rows | 250,000 / 75,000 | 250,000 / 75,000 |
| NULL instants / NULL polls / instants unequal to the parsed text | 0 / 0 / 0 | 0 / 0 / 0 |
| Share totals rows / sum of share rows / sum of tokens | 1,457 / 250,000 / 27,500,000 | 1,457 / 250,000 / 27,500,000 |
| Golden 1h report, every budget, span listings after the backfill | equal | equal |
| First open of the copy | **35.67 s** | **18.44 s** |
| Second open: spans examined, new tables' digest | 675 ms: 0, unchanged | 672 ms: 0, unchanged |

- The expected counts are exact for the fixture: one instants row per span, a poll for span i when i mod 10 < 3, and one totals row per item at denominator 1.
- Tokens are 110 per turn.

First open breakdown at be7c9573 (copy 2, opened in stages):
- span backfill 16.009 s
- share backfill with the totals recompute 0.747 s
- the rest of the open 4.431 s: the turn totals backfill of 7,142 listed turns and the other migrations, not split further
- 21.19 s in all

The H1 open was not split. The span-only diagnosis that led to be7c9573 is in `tailterm-artifacts/builder-b4f40b18/wi_dd39-first-open-finding.md`:
- A span-only store's backfill grew from 1.8 s at 60,000 spans to 14 s at 250,000.
- Its profile was 94% system calls: page-cache misses, and a commit per 500 spans.
- The fix uses one connection with a 64 MiB page cache, batches of 5,000 and prepared statements.

### Hub start at the upgrade

The first open after the upgrade runs the backfill once, before the hub serves.

- On this host at 1x it takes 18.4 s, which leaves 221.6 s of the TrueNAS hub readiness window. That window is 240 s (`TRUENAS_READY_WINDOW_MS`, `scripts/release-probe.mjs`).
- It was not measured on TrueNAS. A host five times slower would take about 92 s, still inside the window.
- The live store holds about 35 days (about 1x). Later opens examine only spans an older binary wrote, which is 0 after a normal upgrade.

## Limits

- m4's third bullet fails: the ratio is 1.66 against the 1.2 bound (above).
- A window deep in history still reads the end-index entries of every later span (integer index entries, no payload). The measured window is the trailing hour.
- Admission's team spend and the running team's figure still read share rows per run (plan v0 Q3).
- H2's 8 min 45 s hold exceeded the 8-minute pre-start estimate. A repeat at 2x needs a larger estimate.
