# Hub wait share and CPU, 11-08 19Z (11:00-12:00 PST)

Bug `wi_1d2b7e16b7c17672` revision 1, work order #59523, builder ASSIGN #59610 from lead-b7c17672. Written 2026-11-08 PST by builder-b7c17672 on Stephens-Mini at tasks-hub `bcdfb63c`. It is a read-only measurement and changes nothing. It reads the request timing lines of `wi_83ad5dfb2fe99d95` (L1), released at about 07:21 PST on 11-08. Times are UTC unless marked PST (PST is UTC-8).

## Verdict

- **The time went to waiting for the store connection, not to admission.** In 19Z, timed requests took 23,228 s in all. The store's connection waits summed to 19,991 s (86.1% of that). Write-lock waits summed to 2,360 s (10.2%). Admission waits summed to 0.13 s (0.0006%).
- **The one connection was saturated.** It was in use at all 60 minute stamps. On average 5.55 callers were waiting for it and 6.45 timed requests were in flight.
- **CPU is bounded, not proven, for the hour itself.** No per-app CPU history exists, so 19Z has no `app.stats` reading. Under similar load at 23:41-23:43Z, `app.stats` read the hub app at 4.99% of the host's 20 CPUs on average (12 samples). That is 1.0 CPU, the hub container's cap. Host CPU was flat at 11.2-11.4% in every hour from 16Z to 22Z, and the pool disk was quiet in 19Z. So the host and the disk were not the limit.
- **Reading (a bound, not a finding):** requests queue behind the one SQLite connection. The process doing that connection's work runs at or near its 1.0 CPU cap. The two cannot be separated from these reads (see Limits).

## The hour chosen (m1)

I chose 19Z, 11:00-12:00 PST on 11-08. It was read at 23:38Z, after the hour ended.

| UTC hour | Requests | Slow (>1 s) | Mean ms | Slowest ms | Connection wait share | Write-lock wait share |
|---|---:|---:|---:|---:|---:|---:|
| 15Z (from 15:20) | 17,836 | 4,564 | 690 | 30,001 | 86.4% | 5.2% |
| 16Z | 20,139 | 7,359 | 1,413 | 30,001 | 79.9% | 17.8% |
| 17Z | 21,619 | 6,639 | 1,056 | 30,001 | 85.9% | 10.5% |
| 18Z | 22,317 | 6,755 | 1,019 | 30,001 | 85.9% | 10.2% |
| **19Z** | **22,199** | **6,817** | **1,046** | **53,626** | **86.1%** | **10.2%** |
| 20Z | 22,459 | 6,694 | 1,014 | 49,418 | 85.6% | 9.9% |
| 21Z | 22,166 | 6,652 | 1,007 | 62,678 | 86.6% | 9.3% |
| 22Z | 22,587 | 6,354 | 944 | 45,000 | 87.9% | 7.7% |

Shares are of the summed request time. All rows come from the `hub requests minute` lines.

Why 19Z:
- **Busy.** A typical daytime hour is the median of the seven whole hours logged, 16Z to 22Z: 22,199 requests (mean 21,927). 19Z had 22,199 requests, so it meets both.
- **Slow.** Of the hours at or above that median (18Z, 19Z, 20Z, 22Z), 19Z had the highest mean request time, 1,046 ms. Its slowest request in a minute line was 53.6 s.
- **Undisturbed.** Host CPU, load and pool disk writes were steady through 19Z.

Hours I did not choose:
- 16Z was slower (mean 1,413 ms), but it had 20,139 requests, below the typical hour.
- 23Z was the only 11-08 hour in which `app.stats` could be read. But the pool disk had heavy writes in it from another source: about 100 times the usual rate, at 23:11-23:16Z and 23:26-23:35Z. Host load reached 14.9. Its request count was also not known until after the due time.

### Time shares for 19Z

From the 60 `hub requests minute` lines, 19:00:57Z to 19:59:57Z:

| Measure | 19Z |
|---|---:|
| Timed requests | 22,199 (6,817 over 1 s, 30.7%) |
| Summed request time | 23,227.9 s (mean 1,046 ms) |
| Admission wait | 0.131 s (0.0006%); `waiting` was 0 at every minute stamp |
| Store connection waits | 136,187 waits, 19,990.6 s (86.1%); mean 146.8 ms each, 6.13 per timed request |
| Write-lock waits | 2,296 locks, 2,359.5 s (10.2%); mean 1,027.7 ms each |
| Remainder (work and everything else) | 877.8 s (3.8%) |
| Connection in use at the minute stamp | 60 of 60 |
| Waiters for the connection, on average | 5.55 (19,990.6 s of waiting over 3,600 s) |
| Timed requests in flight, on average | 6.45 (summed request time over 3,600 s); minute stamps 1-11, median 7 |

### Slow requests by route, 19Z

These are the printed `hub slow request` lines: 3,525 lines. The rate limit held back 3,239 more, and 53 of the minute lines' 6,817 are not accounted for (0.8%; see Limits). All printed lines show `admitted=yes` and 0 ms admission. Statuses: 200 (2,761), 201 (502), 500 (88), 204 (70), 400 (58), 404 (41), 409 (5).

| Route | Printed slow lines | Share | Median ms | p95 ms | Max ms |
|---|---:|---:|---:|---:|---:|
| `GET /v1/tasks/{id}/agents/{aid}` | 1,197 | 34.0% | 2,590 | 5,367 | 11,197 |
| `POST /v1/tasks/{id}/events` | 540 | 15.3% | 3,035 | 5,000 | 14,271 |
| `GET /v1/tasks/{id}` | 327 | 9.3% | 2,614 | 3,722 | 7,759 |
| `GET /v1/tasks/{id}/messages` | 324 | 9.2% | 2,457 | 3,438 | 7,305 |
| `GET /v1/tasks/{id}/pause` | 312 | 8.9% | 2,335 | 3,076 | 4,914 |
| `GET /v1/tasks/{id}/obligations` | 190 | 5.4% | 2,254 | 3,563 | 5,451 |
| `GET /v1/tasks/{id}/team-queue` | 160 | 4.5% | 4,468 | 30,000 | 45,000 |
| `POST /v1/tasks/{id}/releases/actions` | 95 | 2.7% | 2,421 | 4,792 | 6,241 |
| `GET /v1/tasks/{id}/releases` | 91 | 2.6% | 2,441 | 4,249 | 5,412 |
| `POST /v1/tasks/{id}/agents/{aid}/wake-jobs/lease` | 70 | 2.0% | 2,470 | 3,872 | 5,775 |
| `GET /v1/tasks/{id}/work-items/{wid}` | 45 | 1.3% | 2,485 | 3,834 | 4,943 |
| `GET /v1/team-queues` | 43 | 1.2% | 3,686 | 6,723 | 8,852 |
| `GET /v1/tasks/{id}/agents` | 36 | 1.0% | 2,612 | 4,016 | 5,019 |
| `GET /v1/bridge/settings` | 24 | 0.7% | 1,376 | 2,372 | 3,749 |
| `GET /v1/handler-rotations/due` | 16 | 0.5% | 1,945 | 5,136 | 5,136 |
| 11 other routes | 55 | 1.6% | | | 10,001 |

Across the printed lines, the median was 2,522 ms and p95 was 5,432 ms. 42 lines were at or over 10 s, and 15 at or over 30 s. The minute lines name `GET /v1/tasks/{id}/team-queue` as the slowest route in 57 of 60 minutes (`GET /v1/team-queues` in 2, `POST /v1/tasks/{id}/team-queue/actions` in 1). 26 minutes had a slowest request of 30 s or more, with 45.0 s, 45.0 s and 53.6 s at 19:30, 19:51 and 19:43Z.

## CPU (m2)

**How middleware computes `cpu_usage`.** This is from the installed source at TrueNAS-25.10.4 (`system.version`). I read it over the `truenas` SSH route:
- `middlewared/plugins/apps/ix_apps/docker/stats.py:44` (sha256 `d7b8f929…`) sums Docker's `cpu_stats.cpu_usage.total_usage`, in nanoseconds, over every container of the app.
- `middlewared/plugins/apps/stats_util.py:30-32` (sha256 `02b41e75…`) computes `cpu_delta / (interval * 1e9 * cpu_info()['core_count']) * 100`.
- `middlewared/utils/cpu.py:40` (sha256 `c653b0a4…`) sets `core_count` to `os.sysconf('SC_NPROCESSORS_ONLN')`, which is 20 online CPUs on this host (`nproc`, `lscpu` and `/proc/cpuinfo` each give 20).
- `middlewared/plugins/apps/stats.py:26-36` (sha256 `fb8c9a94…`) sleeps `interval` between Docker reads and divides by that nominal interval.

So `cpu_usage` is a percentage of all 20 host CPUs, summed over the app's containers. 5.0 means 1.0 CPU. The `tailterm-hub` app has two containers. `app.config` gives the limits: `hub` has `cpus: 1.0` and `mem_limit: 512m`; `discord-bridge` has `cpus: 0.5` and `mem_limit: 256m`. The app's ceiling is therefore 7.5.

**Readings.** Two `app.stats` subscriptions of 6 samples each, at 23:41:06-23:41:19Z and 23:42:43-23:42:56Z:
- Values: 5.00, 5.06, 4.97, 5.25, 5.15, 5.11; then 5.11, 5.00, 5.07, 5.15, 4.18, 4.86. The mean was 4.99 (0.998 CPU). Memory was 106-157 MiB.
- Load then was close to 19Z's. The minute lines for 23:41-23:43Z show 365, 484 and 405 requests a minute (19Z averaged 370), the connection in use at each stamp, and 287-391 s of connection wait a minute (19Z averaged 333 s).

**Host for 19Z.** From `reporting.netdata_get_data`, 10 s steps:
- CPU: mean 11.4% of 20 CPUs, about 2.3 CPUs, with a maximum of 26.8%. Every hour from 16Z to 22Z was 11.2-11.4%.
- Load: mean 2.0, maximum 4.7.
- Pool disk `sda`: reads 1.26 and writes 332.6 on average, in the units middleware returns. Every hour from 16Z to 22Z was within 1.24-1.34 and 327-388.

**Did the hub sit at its 1.0 CPU cap?** Not established for 19Z itself.
- At 23:41-23:43Z, under similar load, the hub app sat at 1.0 CPU, exactly the hub container's cap, in 10 of 12 samples (4.86-5.25).
- The app figure includes `discord-bridge`, so the hub alone was at most 1.0 CPU (its quota) and at least the app's reading minus whatever the bridge used.
- A flat plateau at exactly one CPU, with the host about 88% idle, fits the hub being held at its cap. That is a reading, not a measurement of throttling.

## Where the time went (m3)

- **Admission:** no. It is 0.0006% of request time in 19Z. Every printed slow line shows 0 ms admission.
- **Store connection and write lock:** yes, together 96.2% of summed request time. The connection was in use at every minute stamp, with on average 5.55 waiters.
- **CPU:** bounded. The connection's work runs inside the hub process. At similar load, that process (with the bridge) used one full CPU, its cap. The host and the pool disk had headroom.
- **Overall:** this is queueing on one connection whose service rate is probably set by the hub's 1.0 CPU. These reads cannot split the connection holder's time into CPU, SQLite I/O and Go scheduling.

**Proposed owner decisions.** These are for the backlog steward to put to the owner with `tt ask`. This item changes nothing.
1. Read the hub container's cgroup `cpu.stat` (`nr_throttled`, `throttled_usec`) at the start and end of one busy hour. That would show directly whether the 1.0 cap throttles the hub. It is outside the middleware reads this order allows, so it needs its own order.
2. Raise the hub container's `cpus` from 1.0 to 2.0 for one day, then repeat this measurement. If the connection's wait share and the mean request time fall, CPU was the limit. If they do not, the single connection itself is (`db.SetMaxOpenConns(1)`, `hub/internal/store/store.go:89` at `bcdfb63c`). This is an app-config change, so it needs an owner decision and a release-style record.

## Limits

These come from L1's own documentation (`docs/project-overview.md`, request timing) and from this capture:
- **Store-wide counters.** The connection and write-lock counters cover every store caller, not just timed requests. That includes the events long polls, which are never timed, and background loops. So 86.1% and 10.2% are upper bounds for timed requests' own waits. They are minute totals, not per-request figures.
- **Two kinds of wait, one queue.** While a writer holding the write lock waits for the connection, other writers wait for the lock. Each caller is in only one of the two waits at a time, so adding them does not count a caller twice. Both kinds are queueing behind the same connection.
- **Admission scope.** Admission wait also counts identity and routing before the slot. Requests refused before admission print `admitted=no` with no split. There were none in 19Z's printed lines.
- **Printed slow lines are a sample.** At most 60 lines a minute print, the first in each window, so the route counts are of printed lines, not of all slow requests.
- **Capture loss.** `midclt subscribe` printed about 2% of stream records garbled, with stray bytes and split lines, in each of two whole-log reads. I merged both reads and kept only lines that match L1's exact formats, with no timestamp conflicts. All 499 minute lines from 15:20:57Z to 23:38:57Z are present, with no gap. The 53 unaccounted slow lines in 19Z (0.8%) may be lines lost in both reads, or held-back counts printed in another hour.
- **No CPU history.** `app.stats` is live only, and `reporting.netdata_graphs` lists no per-app graph. The CPU figures for 19Z are host-wide. The app figures are 4 hours later.
- **Rounding.** `cpu_usage` divides by the nominal interval, while the real gap between Docker reads also includes collection time. That can overstate it slightly.

## Read-only conduct (m4)

All probes were serial, over the existing `truenas` SSH route, between 23:30Z and 23:46Z on 11-08:
- **Middleware:**
  - `system.version`;
  - `app.get_instance tailterm-hub` (container states and IDs);
  - `app.config tailterm-hub`, printing only the CPU and memory limits;
  - three `app.container_log_follow` reads of the hub container: two of the whole log (`tail_lines` 2,000,000, at 23:38:00Z and 23:38:57Z) and one of the last 2,500 lines (at about 23:44Z);
  - two `app.stats` subscriptions of 6 samples each;
  - `reporting.netdata_graphs` twice (graph names and disk identifiers);
  - two `reporting.netdata_get_data` reads (CPU and load; pool disk `sda`), for 16Z-24Z.
- **Files read in place on TrueNAS:** `cat`, `grep` and `sha256sum` of the four middleware source files above, plus `nproc`, `lscpu` and `/proc/cpuinfo`.
- **Hub:** `tt status`, `tt agents`, `tt inbox`, and this item's coordination posts. No Board paging was needed.

Nothing was restarted, reconfigured or signalled: not the hub, its container, the relay, Tailscale on TrueNAS or any agent. No database was opened. Container log lines were read before the next hub release. Only lines matching L1's two formats were kept; they carry no path values, tokens or IDs. A scan of the saved files for `bearer`, `token`, `secret` and the disk serial found nothing. The disk identifier, which carries a serial number, was replaced by `sda` in the saved netdata file. This report contains no token or secret.

## Evidence

The raw files are saved outside the repository, under `/Users/stephenspeicher/projects/tailterm-artifacts/wi_1d2b7e16b7c17672/`:

| File | Contents | sha256 |
|---|---|---|
| `raw/hub-requests-minute-2026-11-08.log` | 499 minute lines, 15:20:57-23:38:57Z | `6ab1ed1b331f3ab013314cf623793c81651c59c1f9f73eee28e2bdf15f6426fb` |
| `raw/hub-slow-request-2026-11-08.log` | 29,394 slow lines, 15:19:59-23:39:09Z | `623375040b4592fc0d6c5a9dbecba4b8cfeb9ce5870214cd4eb388ad14c3617c` |
| `raw/hub-requests-minute-2026-11-08-tail-2339-2344.log` | minute lines, 23:39:57-23:44:57Z | `3d04536e6c718373f033436e0d8b00539ace6f5f89c175a5097ba40160e8854d` |
| `raw/app-stats-2341Z.jsonl`, `raw/app-stats-2342Z.jsonl` | the 12 `app.stats` samples | `65c7bd31…`, `9a718c61…` |
| `raw/netdata-cpu-load-16-24Z.json`, `raw/netdata-disk-sda-16-24Z.json` | host CPU, load and pool disk, 16Z-24Z | `5e7d5a04…`, `ac096370…` |
| `tools/extract.py`, `tools/hour.py` | the merge filter and the per-hour summary that produced the tables | |

`raw/SHA256SUMS` lists every raw file. To rerun the tables for any hour: `python3 -I tools/hour.py raw 19`.
