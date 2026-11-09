# Hub wait share and CPU, 11-09 00Z (11-08 16:00-17:00 PST)

Bug `wi_1d2b7e16b7c17672` revision 1, work order #59523, builder ASSIGN #59610 and fix REQUEST #59630 from lead-b7c17672. Written 2026-11-08 PST by builder-b7c17672 on Stephens-Mini at tasks-hub `bcdfb63c`. It is a read-only measurement and changes nothing. It reads the request timing lines of `wi_83ad5dfb2fe99d95` (L1), released at about 07:21 PST on 11-08. Times are UTC unless marked PST (PST is UTC-8).

The measured hour is 00Z on 11-09 UTC. The file keeps the date 2026-11-08 because that hour is 16:00-17:00 PST on 11-08.

## Verdict

- **The time went to waiting for the store connection, not to admission.** In 00Z, timed requests took 21,033 s in all. The store's connection waits summed to 18,005 s (85.6% of that). Write-lock waits summed to 1,979 s (9.4%). Admission waits summed to 0.18 s (0.0008%).
- **The one connection was saturated.** It was in use at all 59 logged minute stamps. On average 5.09 callers were waiting for it and 5.94 timed requests were in flight.
- **The hub sat at its 1.0 CPU cap for most of the hour.** One continuous `app.stats` subscription gave 1,753 samples spanning 00:00:01-00:59:59Z.
  - The median was 5.04% of the host's 20 CPUs, which is 1.0 CPU, the hub container's cap. 82% of samples were within 4.8-5.3.
  - The mean was 4.74% (0.95 CPU). It is pulled down by short dips below 4.5, which occurred in 59 of the 60 minutes.
  - The host was about 88% idle.
- **Reading (a bound, not a finding):** requests queue behind the one SQLite connection, and the process doing that connection's work is held at its 1.0 CPU cap. The same pattern held in 19Z on 11-08, the comparison hour below. These reads cannot say how much of the connection's service time is CPU and how much is SQLite I/O (see Limits).

## The hour chosen (m1)

Per fix request #59630, the measured hour is H = 00Z, because the `app.stats` subscription began at 23:57:32Z and covered 00:00:00Z onward with no gap over 3 s. I read H's L1 lines at 01:01:06Z and 01:01:18Z, after it ended, and saved them before writing (NOTICE #59644).

**Busy.** A typical daytime hour is the median of the seven whole hours logged on 11-08, 16Z to 22Z: 22,199 requests (mean 21,927). 00Z had 25,463 requests in its 59 logged minutes. It meets the typical count without the missing minute.

**Speed.** Its mean request time was 826 ms. That is lower than any whole hour from 16Z to 22Z, so 00Z is busy but not the slowest hour. I kept 19Z, chosen in the first round as the slowest hour at or above the typical count, as a comparison.

| UTC hour | Requests | Slow (>1 s) | Mean ms | Slowest ms | Connection wait share | Write-lock wait share |
|---|---:|---:|---:|---:|---:|---:|
| 15Z (from 15:20) | 17,836 | 4,564 | 690 | 30,001 | 86.4% | 5.2% |
| 16Z | 20,139 | 7,359 | 1,413 | 30,001 | 79.9% | 17.8% |
| 17Z | 21,619 | 6,639 | 1,056 | 30,001 | 85.9% | 10.5% |
| 18Z | 22,317 | 6,755 | 1,019 | 30,001 | 85.9% | 10.2% |
| 19Z (comparison) | 22,199 | 6,817 | 1,046 | 53,626 | 86.1% | 10.2% |
| 20Z | 22,459 | 6,694 | 1,014 | 49,418 | 85.6% | 9.9% |
| 21Z | 22,166 | 6,652 | 1,007 | 62,678 | 86.6% | 9.3% |
| 22Z | 22,587 | 6,354 | 944 | 45,000 | 87.9% | 7.7% |
| **00Z (11-09, 59 min)** | **25,463** | **6,482** | **826** | **48,816** | **85.6%** | **9.4%** |

Shares are of the summed request time. All rows come from the `hub requests minute` lines.

### Time shares, 00Z and 19Z

From the minute lines: 00:00:57Z to 00:59:57Z for 00Z (59 lines; the 00:25:57 line was never printed, see Limits), and 19:00:57Z to 19:59:57Z for 19Z (60 lines).

| Measure | 00Z (measured) | 19Z (comparison) |
|---|---:|---:|
| Timed requests | 25,463 (6,482 over 1 s, 25.5%) | 22,199 (6,817, 30.7%) |
| Summed request time | 21,033.2 s (mean 826 ms) | 23,227.9 s (mean 1,046 ms) |
| Admission wait | 0.176 s (0.0008%); `waiting` 0 at every stamp | 0.131 s (0.0006%); `waiting` 0 at every stamp |
| Store connection waits | 153,111 waits, 18,005.1 s (85.6%); mean 117.6 ms, 6.01 per timed request | 136,187 waits, 19,990.6 s (86.1%); mean 146.8 ms, 6.13 per timed request |
| Write-lock waits | 2,248 locks, 1,979.3 s (9.4%); mean 880.5 ms | 2,296 locks, 2,359.5 s (10.2%); mean 1,027.7 ms |
| Remainder: a lower bound for timed requests' non-wait time, since the wait counters are store-wide upper bounds | 1,048.8 s (5.0%) | 877.8 s (3.8%) |
| Connection in use at the minute stamp | 59 of 59 | 60 of 60 |
| Waiters for the connection, on average | 5.09 (18,005.1 s over 3,540 s) | 5.55 (19,990.6 s over 3,600 s) |
| Timed requests in flight, on average | 5.94; minute stamps 1-12, median 7 | 6.45; minute stamps 1-11, median 7 |

### Slow requests by route, 00Z

These are the 3,525 printed `hub slow request` lines stamped 00:00:00-00:59:59Z. The minute lines count another 3,006 as held back by the rate limit. Those 6,531 exceed the minute lines' 6,482 by 49, because the missing 00:25:57 line's slow count is absent while that minute's printed lines are present.

All printed lines show `admitted=yes` and 0-1 ms admission. Statuses: 200 (2,798), 201 (551), 500 (67), 204 (59), 400 (37), 404 (8), 409 (5).

| Route | Printed slow lines | Share | Median ms | p95 ms | Max ms |
|---|---:|---:|---:|---:|---:|
| `GET /v1/tasks/{id}/agents/{aid}` | 1,231 | 34.9% | 2,420 | 4,012 | 8,232 |
| `POST /v1/tasks/{id}/events` | 611 | 17.3% | 2,755 | 5,000 | 7,443 |
| `GET /v1/tasks/{id}` | 371 | 10.5% | 2,500 | 3,340 | 18,781 |
| `GET /v1/tasks/{id}/pause` | 299 | 8.5% | 2,346 | 2,549 | 5,722 |
| `GET /v1/tasks/{id}/messages` | 264 | 7.5% | 2,304 | 3,059 | 4,798 |
| `GET /v1/tasks/{id}/obligations` | 235 | 6.7% | 2,346 | 3,367 | 4,937 |
| `GET /v1/tasks/{id}/team-queue` | 173 | 4.9% | 3,297 | 25,758 | 45,000 |
| `GET /v1/tasks/{id}/releases` | 82 | 2.3% | 2,542 | 3,681 | 5,449 |
| `POST /v1/tasks/{id}/agents/{aid}/wake-jobs/lease` | 59 | 1.7% | 2,402 | 3,444 | 4,775 |
| `GET /v1/tasks/{id}/work-items/{wid}` | 56 | 1.6% | 1,704 | 3,991 | 5,546 |
| `GET /v1/team-queues` | 47 | 1.3% | 2,546 | 8,105 | 8,339 |
| `GET /v1/bridge/settings` | 18 | 0.5% | 1,480 | 2,289 | 2,289 |
| `POST /v1/tasks/{id}/releases/actions` | 17 | 0.5% | 2,176 | 5,870 | 5,870 |
| `POST /v1/tasks/{id}/team-queue/actions` | 17 | 0.5% | 1,689 | 4,164 | 4,164 |
| `POST /v1/tasks/{id}/agents/{aid}/activity` | 7 | 0.2% | 2,752 | 10,454 | 10,454 |
| 12 other routes | 38 | 1.1% | | | 10,000 |

- **Across the printed lines:** the median was 2,413 ms and p95 was 4,998 ms. 38 lines were at or over 10 s, and 2 at or over 30 s.
- **Slowest route each minute:** `GET /v1/tasks/{id}/team-queue` in 55 of 59 minutes. The others were `GET /v1/team-queues` (2 minutes), `GET /v1/tasks/{id}/agents/{aid}` (1) and `GET /v1/tasks/{id}` (1).
- **Minutes at or over 30 s:** 4, including 48.8 s at 00:57:57Z and 45.0 s at 00:24:57Z.

**19Z comparison:** 3,525 printed lines and 3,282 held back, against the minute lines' 6,817 slow requests. That leaves 10 unaccounted (0.15%). The same seven routes led. `GET /v1/tasks/{id}/agents/{aid}` had 1,197 lines (34.0%), median 2,590 ms. The printed lines had a median of 2,522 ms and a p95 of 5,432 ms.

## CPU (m2)

**How middleware computes `cpu_usage`.** This is from the installed source at TrueNAS-25.10.4 (`system.version`). I read it over the `truenas` SSH route:
- `middlewared/plugins/apps/ix_apps/docker/stats.py:44` (sha256 `d7b8f929…`) sums Docker's `cpu_stats.cpu_usage.total_usage`, in nanoseconds, over every container of the app.
- `middlewared/plugins/apps/stats_util.py:30-32` (sha256 `02b41e75…`) computes `cpu_delta / (interval * 1e9 * cpu_info()['core_count']) * 100`.
- `middlewared/utils/cpu.py:40` (sha256 `c653b0a4…`) sets `core_count` to `os.sysconf('SC_NPROCESSORS_ONLN')`. That is 20 online CPUs on this host (`nproc`, `lscpu` and `/proc/cpuinfo` each give 20).
- `middlewared/plugins/apps/stats.py:26-36` (sha256 `fb8c9a94…`) sleeps `interval` between Docker reads and divides by that nominal interval.

So `cpu_usage` is a percentage of all 20 host CPUs, summed over the app's containers, and 5.0 means 1.0 CPU. The `tailterm-hub` app has two containers. `app.config` gives the limits: `hub` has `cpus: 1.0` and `mem_limit: 512m`; `discord-bridge` has `cpus: 0.5` and `mem_limit: 256m`. The app's ceiling is therefore 7.5.

**Readings for 00Z.** One `app.stats` subscription ran from 23:57:32Z. Each sample was stamped with its receive time on the Mini, about every 2.05 s, with no gap over 3 s. 00Z holds 1,753 samples, 00:00:01.6Z to 00:59:59.9Z, in all 60 minutes.

| Statistic | `cpu_usage` | CPUs |
|---|---:|---:|
| Median | 5.04 | 1.01 |
| Mean | 4.74 | 0.95 |
| p10 / p90 | 4.23 / 5.14 | 0.85 / 1.03 |
| Samples within 4.8-5.3 | 1,437 (82.0%) | 0.96-1.06 |
| Samples within 4.5-4.8 | 114 (6.5%) | |
| Samples below 4.5 | 197 (11.2%), 43 of them below 1.0 | |
| Samples above 5.3 | 5 (max 6.01) | |

- **Dips:** the samples below 4.5 fall in 59 of the 60 minutes, not in one quiet stretch. Per-minute means were 4.48-5.06. A sample below 1.0 was not followed by a compensating high one (the next sample averaged 3.99), so the dips look like short real pauses, not a pairing artifact of the 2 s window.
- **Memory:** 91-200 MiB.

**Host for 00Z.** From `reporting.netdata_get_data`:
- CPU: mean 11.5% of 20 CPUs (about 2.3 CPUs), maximum 32%.
- Load: mean 2.07, maximum 4.6.
- Pool disk `sda`: reads 5.3 and writes 341 on average, in the units middleware returns, with one short read burst to 4,938. 16Z-22Z on 11-08 were within 1.24-1.40 and 327-388.

**Did the hub sit at its 1.0 CPU cap? Yes, for most of 00Z,** as far as `app.stats` can show.
- The app's median is 1.0 CPU, exactly the hub container's quota, with 82% of samples at 0.96-1.06 CPU.
- `discord-bridge` is in the same figure, so the hub alone was at most 1.0 CPU and at least the reading minus the bridge's share. A plateau at exactly one CPU, while the host was about 88% idle, is what a hub held at its quota looks like.
- What is not shown is throttling itself. Neither the cgroup's throttle counters nor a per-container split is in these reads.

**19Z comparison.** No `app.stats` exists for 19Z itself: `app.stats` is live only, and `reporting.netdata_graphs` lists no per-app graph. 12 samples at 23:41-23:43Z on 11-08 read 4.18-5.25 (mean 4.99). They fall in the minute lines stamped 23:41:57 and 23:42:57, which show 365 and 484 requests, close to 19Z's average of 370 a minute.

## Where the time went (m3)

- **Admission:** no. It is 0.0008% of request time in 00Z and 0.0006% in 19Z.
- **Store connection and write lock:** yes. Together they are 95.0% of summed request time in 00Z and 96.2% in 19Z. The connection was in use at every logged minute stamp, with about 5 waiters on average.
- **CPU:** yes, as the limit on the connection's service rate, but not as a separate wait. The connection's work runs inside the hub process, which sat at its 1.0 CPU cap through 00Z. The host CPU and the pool disk had headroom.
- **Overall:** requests queue on one connection, served by a process held at one CPU. These reads cannot split the connection holder's time into CPU, SQLite I/O and Go scheduling. So they do not say whether more CPU alone would shorten the queue.

**Proposed owner decisions.** These are for the backlog steward to put to the owner with `tt ask`. This item changes nothing.
1. Read the hub container's cgroup `cpu.stat` (`nr_throttled`, `throttled_usec`) at the start and end of one busy hour. That would show directly how much the 1.0 cap throttles the hub. It is outside the middleware reads this order allows, so it needs its own order.
2. Raise the hub container's `cpus` from 1.0 to 2.0 for one day, then repeat this measurement. If the connection's wait share and the mean request time fall, CPU was the limit. If they do not, the single connection itself is (`db.SetMaxOpenConns(1)`, `hub/internal/store/store.go:89` at `bcdfb63c`). This is an app-config change, so it needs an owner decision and a release-style record.

## Limits

These come from L1's own documentation (`docs/project-overview.md`, request timing) and from this capture:
- **Store-wide counters.** The connection and write-lock counters cover every store caller, not just timed requests. That includes the events long polls, which are never timed, and background loops. So the wait shares are upper bounds for timed requests' own waits, and the remainder is a lower bound for their non-wait time. They are minute totals, not per-request figures.
- **Two kinds of wait, one queue.** While a writer holding the write lock waits for the connection, other writers wait for the lock. Each caller is in only one of the two waits at a time, so adding them does not count a caller twice. Both kinds are queueing behind the same connection.
- **Admission scope.** Admission wait also counts identity and routing before the slot. Requests refused before admission print `admitted=no` with no split. There were none in either hour's printed lines.
- **Printed slow lines are a sample.** At most 60 lines a minute print, the first in each window, so the route counts are of printed lines, not of all slow requests.
- **The missing 00:25:57 minute line.** It is absent from both reads, and both hold the 00:24:57 and 00:26:57 minute lines and the 00:25 slow lines between them. So it was not lost in transport: the hub did not print it. 00Z's figures cover the other 59 windows (3,540 s). The hub's code prints nothing for a minute with no timed requests, but that minute had printed slow lines, so why the line is missing is not established.
- **Capture loss.** `midclt subscribe` garbled about 2% of stream records, with stray bytes and split lines, in each whole-log read. I merged two reads each time and kept only lines matching L1's exact formats, with no timestamp conflicts. The 11-08 file has all 499 minute lines from 15:20:57Z to 23:38:57Z. The 10 unaccounted slow requests in 19Z (0.15%) fit the minute windows' 57 s offset from the clock hour; they may also be lines lost in both reads.
- **What `app.stats` cannot show.** It sums both containers and divides by the nominal 2 s interval, while the real gap between Docker reads also includes collection time. That can overstate values slightly.

## Read-only conduct (m4)

All probes were serial, over the existing `truenas` SSH route: from 23:30Z on 11-08 to 01:10Z on 11-09, plus the one `app.stats` subscription running from 23:57:32Z.
- **Middleware:**
  - `system.version`;
  - `app.get_instance tailterm-hub`, twice (container states and IDs; the hub container was the same at 23:37Z and 01:00Z);
  - `app.config tailterm-hub`, printing only the CPU and memory limits;
  - five `app.container_log_follow` reads of the hub container: two whole-log reads at 23:38Z, one 2,500-line tail at about 23:44Z, and two 12,000-line tails at 01:01Z;
  - three `app.stats` subscriptions: two of 6 samples at 23:41Z and 23:42Z, and the one continuous subscription of at most 4,000 samples;
  - `reporting.netdata_graphs` twice (graph names and disk identifiers);
  - three `reporting.netdata_get_data` reads: CPU and load, and pool disk `sda`, for 16Z-24Z; then CPU, load and disk for 00Z.
- **Files read in place on TrueNAS:** `cat`, `grep` and `sha256sum` of the four middleware source files above, plus `nproc`, `lscpu` and `/proc/cpuinfo`.
- **Hub:** `tt status`, `tt agents`, `tt inbox`, `tt progress`, and this item's coordination posts. No Board paging was needed.

Nothing was restarted, reconfigured or signalled: not the hub, its container, the relay, Tailscale on TrueNAS or any agent. No database was opened. Container log lines were read before any hub release recreated the container. Only lines matching L1's two formats were kept; they carry no path values, tokens or IDs. A scan of the saved files for `bearer`, `token`, `secret` and the disk serial found nothing. The disk identifier, which carries a serial number, was replaced by `sda` in the saved netdata files. This report contains no token or secret.

## Evidence

The raw files are saved outside the repository, under `/Users/stephenspeicher/projects/tailterm-artifacts/wi_1d2b7e16b7c17672/`:

| File | Contents | sha256 |
|---|---|---|
| `raw/hub-requests-minute-2026-11-09-00Z-read.log` | 199 minute lines, 21:41:57Z-01:00:57Z (00Z: 59) | `03bc195484775a6494a97cced49ee0a69f266b10d6e48db91e6339e4fd0fcf19` |
| `raw/hub-slow-request-2026-11-09-00Z-read.log` | 11,764 slow lines from the same reads (00Z: 3,525) | `e0f7a581a1e602c7ce4aa3843d0070b71997e174224dca0e4c1c2f893161d9aa` |
| `raw/app-stats-live-2026-11-09-00Z.jsonl` | the 1,753 receive-stamped 00Z `app.stats` samples | `684eaa30e0f981146ce3df57704ef410aadc3949cd30f4dc489ef457c0758563` |
| `raw/app-stats-live-20261108T235732Z.jsonl` | the whole continuous subscription, 23:57:32Z onward | listed in `SHA256SUMS` once it ended |
| `raw/netdata-cpu-load-disk-00Z.json` | host CPU, load and pool disk, 00Z | `dae40c61d1eb9212037cb77a7807670fdca7b8d786d3b2ace7257de646c741da` |
| `raw/hub-requests-minute-2026-11-08.log` | 499 minute lines, 15:20:57-23:38:57Z | `6ab1ed1b331f3ab013314cf623793c81651c59c1f9f73eee28e2bdf15f6426fb` |
| `raw/hub-slow-request-2026-11-08.log` | 29,394 slow lines, 15:19:59-23:39:09Z | `623375040b4592fc0d6c5a9dbecba4b8cfeb9ce5870214cd4eb388ad14c3617c` |
| `raw/hub-requests-minute-2026-11-08-tail-2339-2344.log` | 5 minute lines, 23:39:57-23:43:57Z | `3d04536e6c718373f033436e0d8b00539ace6f5f89c175a5097ba40160e8854d` |
| `raw/app-stats-2341Z.jsonl`, `raw/app-stats-2342Z.jsonl` | the 12 `app.stats` samples of 11-08 | `65c7bd31…`, `9a718c61…` |
| `raw/netdata-cpu-load-16-24Z.json`, `raw/netdata-disk-sda-16-24Z.json` | host CPU, load and pool disk, 16Z-24Z on 11-08 | `5e7d5a04…`, `ac096370…` |
| `tools/extract.py`, `tools/hour.py`, `tools/stamp.py` | the merge filter, the per-hour summary that produced the tables, and the receive-time stamper | |

`raw/SHA256SUMS` lists every raw file. To rerun the tables, from the folder above: `python3 -I tools/hour.py raw "2026-11-09 00"` for 00Z and `python3 -I tools/hour.py raw 19` for 19Z. `hour.py` reads every minute and slow file in `raw/` and drops duplicate lines.
