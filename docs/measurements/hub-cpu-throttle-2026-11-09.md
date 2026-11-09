# Hub container CPU throttling over one busy hour (03Z, 2026-11-09)

Bug `wi_5ca21392cd79295c` r1, work order #59688. Owner gate: delegated answer #59669 (option a) to decision #59665. Method follows `docs/measurements/hub-wait-share-2026-11-08.md` (wi_1d2b).

The measured hour is 03:00-04:00Z on 2026-11-09 (19:00-20:00 PST on 11-08).

## Verdict (t3)

**The gate is met.** The hub container was throttled in 28.99% of its scheduling periods over the hour (10,088 of 34,804). That is at least 25%, so at least 1 in every 4 periods.

This report proposes nothing further and changed nothing.

## The hour chosen (t1)

**Busy.** The bar is the wi_1d2b report's typical daytime hour: 22,199 requests, from L1's minute lines. 03Z had **27,916 requests** in its 60 minute lines, stamped 03:00:57Z through 03:59:57Z. All 60 lines are present. Each line covers the minute ending at its stamp, so they span 02:59:57-03:59:57Z.

**Counter reads.** Each read cats the cgroup files in one SSH call, then reads `app.get_instance tailterm-hub` through middleware. The times below are TrueNAS's clock at the `cpu.stat` read.

| Read | Time (UTC) | Offset from hour edge |
|---|---|---|
| Start | 2026-11-09T03:00:06.209Z | +6.2 s |
| End | 2026-11-09T04:00:05.613Z | +5.6 s |

There were 3,599.404 s between the reads.

**Container read.**
- Hub container ID, from `app.get_instance tailterm-hub` `active_workloads.container_details` (service `hub`): `e2cd4d109abdb986fd24420a7371cf6f9a8526a032dd7e56300809d51d1515a4`.
- Cgroup path read (cgroup v2): `/sys/fs/cgroup/docker/e2cd4d109abdb986fd24420a7371cf6f9a8526a032dd7e56300809d51d1515a4`. Files read: `cpu.max` and `cpu.stat`.
- Only the hub container's cgroup was read. `app.get_instance` also lists the discord-bridge container (`6d1c592c…`), but its cgroup was not read.

**Same container, no restart between the reads.**

| Check | Start read | End read |
|---|---|---|
| `app.get_instance` app state / hub container state | RUNNING / running | RUNNING / running |
| Hub container ID | `e2cd4d10…1515a4` | `e2cd4d10…1515a4` (same) |
| Cgroup path | as above | same |
| `cgroup.procs` | one PID, 3656818 | one PID, 3656818 |
| Process start (`/proc/3656818/stat` starttime + `btime`) | 2026-11-08T15:19:53.1Z | 2026-11-08T15:19:53.1Z |
| `cpu.stat` counters | — | every delta ≥ 0 |

A restart would give the process a new PID and start time, and the cgroup counters would start again from zero. None of that happened. The hub process has run since the 11-08 ~15:20Z recreation that the wi_1d2b report names. The lead asked the handler to hold hub releases through the hour (NOTICE #59706).

## Counters for the hour (t2)

| | Start (03:00:06Z) | End (04:00:05Z) | Delta |
|---|---|---|---|
| `cpu.max` | `100000 100000` | `100000 100000` | — |
| `usage_usec` | 39,250,650,934 | 42,587,032,473 | 3,336,381,539 |
| `user_usec` | 4,467,230,591 | 4,864,798,883 | 397,568,292 |
| `system_usec` | 34,783,420,343 | 37,722,233,589 | 2,938,813,246 |
| `nr_periods` | 407,118 | 441,922 | 34,804 |
| `nr_throttled` | 118,518 | 128,606 | 10,088 |
| `throttled_usec` | 657,357,106 | 712,331,931 | 54,974,825 |
| `nr_bursts`, `burst_usec` | 0, 0 | 0, 0 | 0, 0 |

- **Quota (`cpu.max`):** 100,000 µs of CPU per 100,000 µs period, which is **1.0 CPU**. It was the same at both reads.
- **Throttled period share:** 10,088 / 34,804 = **28.99%**.
- **Throttled time:** 54,974,825 µs = **54.97 s**. That is **1.53%** of the 3,600 s hour, and a mean of 5.45 ms per throttled period.
- **Average CPUs used:** 3,336,381,539 µs / 3,600 s = **0.927 CPU**. User time was 11.9% of usage and system time 88.1%.
- **Periods counted:** `nr_periods` counts only periods in which the cgroup had runnable work. 34,804 is 96.7% of the 35,994 periods of 100 ms that fit between the reads.

**Context from L1's minute lines, same hour** (`tools/hour.py`, as in wi_1d2b):

| UTC hour | Requests | Slow (>1 s) | Mean request ms | Connection wait share | Write-lock wait share | Admission share |
|---|---|---|---|---|---|---|
| 03Z 11-09 | 27,916 | 6,701 | 796 | 84.0% | 11.1% | 0.01% |

The shares are of summed request time (22,215,928 ms). Store connection waits were 18,670,256 ms over 165,436 waits.

## Limits

- **Clocks.** The read times come from TrueNAS's `date` only. The Mini stamped 03:00:06.159Z after the start SSH call returned, 50 ms before TrueNAS's 03:00:06.209Z for the read itself. So the Mini's clock runs at least 50 ms behind TrueNAS's, and neither read time depends on it.
- **Hour edges.** The counter interval (03:00:06-04:00:05Z) and the minute-line windows (02:59:57-03:59:57Z) differ by about 9 s. The interval is 0.6 s short of 3,600 s. Following t2, average CPUs divides by 3,600 s; divided by the measured interval it is also 0.927.
- **What the share counts.** Throttled period share follows t2's definition. Throttled time is the wall time the cgroup spent waiting for its next period, which is a different quantity. This report states both and draws no conclusion from the gap between them.
- **Capture loss.** As in wi_1d2b, `midclt subscribe` garbled a few records in each log read: 23 and 22 rejected out of about 11,960. Two reads were merged, keeping only exact L1-format lines, with 0 timestamp conflicts. The 03Z slow lines leave 1 slow request unaccounted for against the minute lines (6,701 slow = 3,520 printed + 3,180 held back + 1).

## Read-only conduct (t4)

All probes were serial, over the existing `truenas` SSH route, from 02:06Z to 04:01Z on 11-09.

- **Files read in place on TrueNAS, with `cat` only (plus `ls`/`find` to locate the cgroup directory):**
  - the hub cgroup's `cpu.max`, `cpu.stat` and `cgroup.procs`;
  - `/proc/<pid>/stat`, `/proc/uptime` and `btime` from `/proc/stat`;
  - `getconf CLK_TCK` and `date`.

  No cgroup or other file was written.
- **Middleware read calls:**
  - `app.get_instance tailterm-hub`, five times: two trials at 02:06-02:08Z, the start read, the end read, and one first look;
  - `app.container_log_follow` for the hub container, as 12,000-line tails: two at 02:08Z (trial) and two at 04:00Z;
  - one 5-line tail at 02:07Z.
- **Trial reads.** Two pre-hour trial reads at 02:07:35Z and 02:08:33Z tested the script. They are kept beside the evidence, under `trial/`, and are not used in any figure above.
- **Nothing changed.** Nothing was restarted, reconfigured or signalled: not the hub, its container, the relay, the app config, Tailscale on TrueNAS or any agent. No database was opened.
- **tt calls.** These were only this item's own messages and status: ack, progress, NOTICE #59754 and this RESULT. There was no Board paging.
- **No secrets.** The full-log reads were deleted after the L1 lines were extracted, and only exact L1-format lines were kept. A scan of the saved files for `bearer`, `token`, `secret`, `password` and `api_key` found only bind-mount path names in `app.get_instance` (for example `/run/hub-token`), with no values. Its `config` field is null. This report contains no token or secret.

## Evidence

Raw files are saved outside the repository, under `/Users/stephenspeicher/projects/tailterm-artifacts/wi_5ca21392cd79295c/`. `raw/SHA256SUMS` lists every raw file.

| File | Contents | sha256 |
|---|---|---|
| `raw/counters-start-03Z.txt` | start read: path, times, `cpu.max`, `cpu.stat`, PID, `/proc` stat | `8ce2dd1662fbe227506c1a533c5b3bd07f04321667d8824a9592bea6c2f852f4` |
| `raw/counters-end-04Z.txt` | end read, same fields | `bbde2cf966ed69f162159a656f3c0a6773dd05bf24773960cddda6668ddfb784` |
| `raw/instance-start-03Z.json` | `app.get_instance tailterm-hub` at 03:00:06Z | `7c38b488dad3a25e7d8a9c06cb890ae074ba4d07ce430bef129a16c8c0cc8f41` |
| `raw/instance-end-04Z.json` | `app.get_instance tailterm-hub` at 04:00:05Z | `7c84cb6b6577a5af0546bd1dd2f9d5d4b5393416afeb956fb7b98d9162c93f5b` |
| `raw/hub-requests-minute-2026-11-09-03Z-read.log` | 200 minute lines up to 03:59:57Z (03Z: 60) | `302a8dcc124c4321fdb2516f05c0850c976e12f3bf09aa9decd563d634669ada` |
| `raw/hub-slow-request-2026-11-09-03Z-read.log` | 11,784 slow lines from the same reads (03Z: 3,520) | `e56ce35b53eb0920f1a33aaedbe43ca5ce5b9319a98f35e280a0f11f2291b871` |
| `tools/read-counters.sh` | the timed read, one script per read | `c844ce6fc72a4d58ab15e6cd83aadf53bc19e23fcb3bf633404703fb438bec94` |
| `tools/analyze.py` | counter deltas and restart checks | `43e849fd0e59486db628d81448937056e190abf93adfda3cb4109aa7bb7d3096` |
| `tools/fetch-logs.sh` | the two log tails and the L1 merge | `bead1dc00c1be7d8b6aad9c8f387f9e2c947437254cca96f90a6af0ceb48b52a` |
| `tools/extract.py`, `tools/hour.py` | copied unchanged from wi_1d2b's `tools/` | `14acb67f…`, `2f88b865…` |

To rerun, from the folder above:
- the counters: `python3 -I tools/analyze.py start-03Z end-04Z`;
- the L1 table: `python3 -I tools/hour.py raw "2026-11-09 03"`.
