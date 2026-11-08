# Hub request latency, 11-06 12:30-13:30 PST

Bug `wi_44607de09ce84283` revision 1, work order #59227, builder ASSIGN #59250 from lead-9ce84283. Written 2026-11-07 PST by builder-9ce84283 on Stephens-Mini at tasks-hub `b08a3157`. It is a read-only diagnosis and changes nothing.

November 6 is after the end of daylight saving time, so PST is UTC-8. The window 12:30-13:30 PST is 20:30-21:30Z. Times below are UTC unless marked PST.

## Verdict

The cause is **bounded, not established**. The hub logs no per-request timing, and its 11-06 container log no longer exists, so no hub-side read can say where the time went.

What the reads establish:

- **Trivial calls were slow too.** The failures were client deadlines expiring on single-row calls such as `GET /agents/{id}`, `GET /pause` and `POST /activity`. These calls took 5-13 s (R1). The relay gives its activity tick 5 s, and most `tt` commands give a call 10 s (R10). So slowness was not limited to large pages.
- **Every caller was affected.** Timeouts hit all six relay bindings that had activity in the window, plus the runner, the steward and the handler (R1, R2, intake). The pattern is a shared slowdown, not one client's problem.
- **The onset was sharp, and the slowdown never ended.** Before 20:32Z there were 0-2 relay timeouts an hour. Then came 30 (20Z), 121 (21Z) and 28 (22Z). The rate stayed raised through 11-07, with 128 at 11-07 22Z. It was still 42-83 an hour at 11-08 03-04Z (R1), after a hub restart at 11-08 02:35Z (R7).
- **Ruled out for 11-06:**
  - a hub release: none is journaled between 11-03 10:08 and 11-07 16:24 PST (R8);
  - a step in database size: 3.03 GB on 11-03, 3.06 GB on 11-07 (R8);
  - TrueNAS CPU, load or memory pressure: flat across the onset (R4).
- **The network path on 11-06 is not ruled out** (Unknowns, item 8). All 201 relay errors that day were `context deadline exceeded`. None was a dial, refused, reset, unreachable or i/o-timeout error (R1). So the hub was reachable but slow to answer, which bounds but does not exclude a network delay. The 1 ms round trip (R9) was measured on 11-08 and says nothing about 11-06.
- **Agent activity rose at the same moment.** Board posts went from about 6 an hour (15-19Z) to 32 in 20Z and 41 in 21:00-21:30Z alone (R3). db-handler-r81 went from 2 tool calls an hour to 46 and 73, starting in the 20:30Z bucket. Its calls were strictly serial (R6). The steward and owner-helper made 94 and 91 tool calls in 20Z, and 60 and 51 in 21Z (R5).
- **Pool writes rose in the same window.** On the single-HDD pool that holds the hub database, mean writes rose about 35% and peaks doubled, 20:30-21:30Z, then fell back by 21:45Z. The same rise appears at the 11-07 22Z timeout peak and is absent on the 11-05 baseline (R4). These graphs cannot attribute the writes to the hub.

Most consistent explanation (a hypothesis, not a finding): requests queued inside the hub. Every store call shares one SQLite connection (R10), in a container capped at 1.0 CPU (R7). At that point, a burst of agent reads and writes on top of the relay's steady polling of 19-21 bindings (R2) turned queueing delay into seconds. Two facts do not fit it cleanly:

- The raised rate persisted overnight on 11-07, when agent activity was low (R5, R6).
- The current CPU sample (R7) cannot be interpreted without knowing how middleware normalizes it.

No cause is claimed from co-occurrence alone.

## Reads (R1-R10)

| ID | Read | What it gave |
|---|---|---|
| R1 | Mini relay log `~/Library/Logs/Tailterm/inbox-relay.log` (18.5 MB, local file) | `context deadline exceeded` lines per route and binding; `slow pass` lines with the slowest hub call and its time |
| R2 | Same log, `slow pass` lines | bindings per pass (19 then 21), pass phase times |
| R3 | Board, two serial pages: `tt inbox --board --after 58740 --limit 50 --json`, then `--after 58790` | 100 messages, 15:28-21:30Z: sender, kind, time |
| R4 | TrueNAS middleware `reporting.netdata_get_data`: load, cpu, memory, arcsize and disk `sda` for 11-06 19-23Z; disk `sda` for 11-05 19-23Z and 11-07 20Z-11-08 00Z (4 calls) | host load, CPU, memory, ARC and pool disk I/O per 15 min |
| R5 | Claude tool ledgers `~/.local/state/tailterm/tool-ledger/*/ledger.jsonl` (24 files touched since 11-05) | tool calls per hour per agent; no arguments (the ledger keeps a digest only) |
| R6 | db-handler-r81's Codex rollout (thread `01a1a19e`, found from its relay binding file) | tool calls per 10 min, `tt` subcommand counts, `--limit` values, overlap. Counts only; no arguments or content copied |
| R7 | Middleware `app.config tailterm-hub` (only volume paths and resource limits printed), `app.get_instance`, `app.container_log_follow` (newest 60 lines, redacted), `app.stats` (6 samples, now), `pool.query` (pool and disk names) | 1.0 CPU / 512 MiB cap; state on pool `deepfreeze` = one HDD `sda`; container log starts 11-08 02:35Z; current CPU sample |
| R8 | Deployer journal `~/.local/state/tailterm-deploy/tsk_e7af3c28a444b09a` (file times and numeric size fields of preflight receipts) | hub release times; database size at each TrueNAS release |
| R9 | `ping -c 5 100.116.238.37`; three serial `tt status` | measured 11-08 about 04:3xZ: 0.58-1.14 ms RTT; 0.01 / 1.29 / 1.99 s; no bearing on 11-06 |
| R10 | Source at `b08a3157` | `hub/internal/store/store.go:42` `busy_timeout=5000`; `store.go:87` `SetMaxOpenConns(1)`; `hub/cmd/tt/relay.go:2080` 5 s activity tick; `relay.go:1368-1369` slow pass above 30 s, logged at most once per 2 min; CLI deadlines: of the 106 `e.client(...)` call sites in non-test `hub/cmd/tt/*.go`, 57 pass 10 s, 14 pass 20 s and 10 pass 5 s (counted with `grep -ho 'e\.client([^)]*)'`; call sites, not commands) |

Also used:

- the steward intake `intake-partial-launch-timeouts-2026-11-06.md` (SHA-256 `ca8c2ade…`);
- r81's incident file `58808-incident.txt`;
- the r81 RESULT #59257 with correlation note `nart_7945930d25127048@1` and an existing-item search.

## Per-route latency from the hub's logs or metrics

**None exist.**

- The hub writes no access or timing log. At `b08a3157` its log calls cover startup, the broker, jev and the schedule monitor (R10, `hub/cmd/tailterm-hub/main.go`).
- The hub exposes no metrics route.
- The container was recreated at 11-08 02:35Z by the 11-07 release. Its log now begins at that start (R7), so even the 11-06 broker lines are gone. `docker logs --since` would need sudo on TrueNAS and was not used.

The client-side substitute is the relay (R1, R2).

Timeouts per hour (relay lines):

| Hour (Z) | 11-05 | 11-06 | 11-07 |
|---|---|---|---|
| 07-12 | 1 (09Z), 17 (11Z), 1 (12Z) | 1, 2, 1 (07, 09, 11Z) | 16-19 each hour |
| 20 | 1 | **30** | 8 |
| 21 | 0 | **121** | 33 |
| 22 | 0 | 28 | **128** |
| 23 | 0 | 18 | 81 |

The rate on 11-08 was 29, 30, 63, 83 and 42 for 00Z through 04Z.

Method: relay error lines carry no timestamp. Each is placed at the nearest timestamped line above it. Timestamped wake-skip lines are seconds to minutes apart, so the hour placement is close.

The first timeout of the window is attributed to 20:32:07Z. The densest stretch is 21:03-21:23Z, with up to 9 timeouts between two timestamped lines.

Routes that timed out on 11-06 (all hours, 201 relay lines):

| Count | Route | Caller in the relay |
|---|---|---|
| 62 | `GET /v1/tasks/{task}/agents/{agent}` | activity tick |
| 57 | `GET /v1/tasks/{task}/obligations?agentId=…&open=1` | activity tick |
| 51 | `POST /v1/tasks/{task}/agents/{agent}/activity` | activity tick |
| 20 | `POST /v1/tasks/{task}/agents/{agent}/usage` | usage upload (deferred) |
| 5 | `GET /v1/tasks/{task}/agents/{agent}` | binding pass |
| 2 | `GET /v1/tasks/{task}/messages?after=N&limit=200&to=…` | binding pass |
| 1 | `GET /v1/tasks/{task}/agents/{agent}/work-context?runId=…` | the spawn of builder-06cacc1f (runner #58804) |
| 1 each | `GET /v1/tasks/{task}`, `GET /team-queue?view=active`, `GET /pause` | team queue and binding pass |

Slowest hub call of each slow pass, 20-21Z (13 passes):

- 10 were `GET /agents/{agent}`: median 6.6 s, maximum 12.9 s.
- The others were `POST /usage` (5.0 s), `GET /pause` (5.2 s) and `POST /activity` (4.9 s).

Passes ran 30-51 s against a 30 s threshold. Slow-pass lines are rate-limited to one per 2 min, so these counts are lower bounds.

Limit: the relay logs only failures and slow passes. It gives no latency distribution for successful calls, and no hub-side split between waiting and work.

## Concurrent callers and request rates

**Agents on the board (R3).** Messages per hour were 3 (15Z, from 15:28Z only), then 6, 6, 6 and 6 for 16Z through 19Z, then 32 (20Z). The second page ends at #58840 (21:30:39Z), so 41 is the count for 21:00-21:30Z only, not the full 21Z hour.

From 19:42Z to 20:28Z only broker and relay notices were posted. At 20:32:23Z owner-helper posted #58772, and the owner-helper, r81 and steward exchange began. Of the 67 messages from 20:30Z to 21:30Z:

| Messages | Sender |
|---|---|
| 17 | db-handler-r81 |
| 16 | owner-helper |
| 10 | steward |
| 7 | owner (work orders) |
| 6 | lead-06cacc1f |
| 2 | planner-06cacc1f |
| 9 | broker, relay and runner notices |

**db-handler-r81 (R6).**

- Its tool calls per 10 min were 1 (19:10), 1 (19:40) and 1 (20:10), then 19, 8, 18, 21, 21, 18 and 13 from 20:30 to 21:30.
- Each call is a script that may run several `tt` commands. The scripts mention `tt` 24-68 times per 10 min, mostly `inbox`, `work-items get`, `obligations`, `work-items narrative` and `work-items scope`. A mention is not always an executed call, so this is an upper bound.
- `--limit` values in its scripts, 19Z-22Z: 50 (4 uses), 100 (4), 200 (2) and 64 (2).
- Its 118 calls from 20:30Z to 21:40Z never overlapped (maximum concurrency 1). Wall time per call: median 6.7 s, p90 14.3 s, maximum 26.5 s.
- This rollout has no tool calls from 22Z on 11-06 to 06Z on 11-07.

**Claude agents (R5).**

- Tool calls were 185 (20Z) and 186 (21Z).
- In 20Z they came from the steward (94) and owner-helper (91).
- In 21Z they came from the steward (60), owner-helper (51), planner-06cacc1f (39) and lead-06cacc1f (36).
- From 22Z on 11-06 to 09Z on 11-07 there were 3-13 an hour.
- The ledger does not say which calls were `tt` calls.

**Relay (R2).** It polled 19 bindings per pass in 20Z and 21 from 21Z, when the new team's lead and planner were spawned. On 11-07 it polled 21-24.

Each binding's pass touches at least `GET /agents/{agent}`, `GET /obligations`, `POST /activity` and `POST /usage`, and sometimes `GET /messages` (R1). That steady background runs whatever the agents are doing.

**No looping or parallel caller.** No agent in R5 or R6 averaged more than about 2 tool calls a minute in any hour or 10-minute bucket. r81's busiest 10 minutes had 21 calls, and its calls were serial.

**Unknown:**

- Request rates from the browser (TailOS tabs), the Discord bridge, the deployer and the broker. None of them logs its calls in a form read here.
- How many HTTP requests each `tt` command makes.
- The relay's request rate when passes are not slow.

## Single-connection database contention

Code at `b08a3157` (R10):

- The store opens one `*sql.DB` with `SetMaxOpenConns(1)` and `busy_timeout=5000`. Every hub request that touches the store therefore waits for that one connection.
- The hub's own background loops share it. Among them is the obligation broker every 30 s (`hub/cmd/tailterm-hub/main.go:138`).

What fits contention:

- Trivial single-row reads took 5-13 s (R1).
- Every caller and route was affected.
- The onset matches the start of heavy handler and agent traffic (R3, R6).

What it cannot show:

- how long any request held or waited for the connection;
- which statement was slow;
- whether the time went to the database, to CPU (R7, below) or to the Go HTTP server.

Several slowest calls sit at 4.9-5.2 s, close to the 5 s `busy_timeout`. That is suggestive, but the 5 s relay tick produces the same cluster, so it is not evidence. Contention stays a **hypothesis**.

## Page size (limit 200 versus 50/60)

- **Evidence that page size matters (intake):** the steward's `work-items list --limit 200` reads timed out, and the same reads at `--limit 60` succeeded. `team queue list --limit 200` timed out where `--limit 50` succeeded.
- **Evidence that it is not needed for a timeout (R1):** most timeouts in the window were single-row calls. One `GET /messages … limit=200` per agent also timed out.
- **Earlier measurement:** a full 200-item work-items page took 13-15 ms on loopback against a synthetic database (`docs/measurements/work-items-fetch-2026-11-01.md`, P1). That was not the production database, CPU cap or disk.

So a large page is a plausible way to make a queue worse, by holding the one connection longer, and it is the likeliest call to miss its own deadline. It does not explain why small calls were slow.

No limit-200 probe was run, because order #59227 allows only 50/60 pages. The production cost of a 200-row page is **unknown**.

## TrueNAS host load

Host values are 15-min mean/max (R4):

| Window (Z) | Load (1 min) | CPU (all 20 cores) | Memory available | ARC |
|---|---|---|---|---|
| 11-06 19:00-20:15 | 1.5-2.1 mean, max 4.0 | 10.0-10.7% | 5.24-5.28 GB | 14.54-14.55 GB |
| 11-06 20:30-21:30 | 1.8-2.0, max 3.7 | 10.6-11.6% | 5.19-5.22 GB | 14.56 GB |
| 11-06 21:45-22:45 | 1.6-1.9, max 2.9 | 10.9-11.2% | 5.19-5.23 GB | 14.57 GB |

So TrueNAS was not CPU-, load- or memory-bound.

Pool disk `sda` (the only disk of pool `deepfreeze`, an HDD that holds the hub's `/state`, R7), values as returned by the API:

| Window (Z) | reads mean/max | writes mean/max |
|---|---|---|
| 11-05 19:00-22:45 (baseline) | 0.5-1.8 / ≤71 | 302-325 / ≤1,789 |
| 11-06 19:00-20:15 | 1.0-2.2 / ≤84 | 319-329 / ≤1,855 |
| **11-06 20:30-21:15** | 1.8-5.5 / ≤312 | **423-438 / ≤3,465** |
| 11-06 21:30 | 1.5 / 86 | 368 / 2,638 |
| 11-06 21:45-22:45 | 1.1-1.6 / ≤88 | 315-335 / ≤1,848 |
| 11-07 19:45-21:15 | 0.6-1.7 / ≤51 | 307-320 / ≤1,452 |
| **11-07 21:30-23:30** | 0.9-2.8 / ≤154 | **326-451 / ≤3,952** (5 of 9 buckets 386-451) |

Reads stay near zero because the 14.5 GB ARC serves them. Write bursts line up with the two timeout peaks and are absent on 11-05.

The graph covers the whole pool. The reads did not establish whether the hub, the Discord bridge or another app on `deepfreeze` wrote those bytes, or whether the writes are a cause (synchronous SQLite commits on an HDD) or an effect (more agent writes).

**Hub container (R7).**

- It is capped at `cpus: 1.0` and `mem_limit: 512m`.
- `app.stats` was sampled on 11-08 between 04:30Z and 04:37Z, while timeouts were still raised. The sample itself carries no timestamp. The bound comes from my own tool ledger (R5): the middleware probes ran after 04:30:37Z, and the report file was first written at 04:36:53Z. It read `cpu_usage` 4.97-5.18 in all 6 samples, with memory at 86-140 MB.
- Hub release `rel_5fa94cf666da9cc2` began its TrueNAS preflight at 04:39:25Z (lead REQUEST #59270). By that bound the sample came before the preflight. The job's journal record was written at 04:28Z (R8), though, so the job existed during the sample. Whether it put load on the hub before its preflight is unknown, so the sample may not represent ordinary load.
- If `cpu_usage` is a share of all 20 host CPUs, 5% is exactly one CPU: the hub would be at its cap. The tight plateau fits that reading.
- The normalization was not confirmed from middleware documentation or source, so this is **unverified**.
- No CPU history for the container exists for 11-06; only host graphs are kept.

## Unknowns

1. Where request time went inside the hub (connection wait, SQL, CPU throttling, encoding). No hub-side timing exists.
2. Whether the hub sits at its 1.0 CPU cap, then or now (normalization of `app.stats` unknown; no 11-06 history).
3. Which process wrote the extra pool bytes.
4. Request rates from the browser, the Discord bridge, the deployer and the broker, and how many HTTP requests each `tt` command makes.
5. Why the raised rate persisted overnight on 11-07 with low agent activity, and through the 11-08 restart.
6. The production cost of 100- and 200-row pages.
7. Whether each uncertain POST in the intake later succeeded. Not reconciled here; the handler noted it as open.
8. The Mini-to-TrueNAS network path on 11-06. No round-trip or packet-loss record from that day was read. The relay errors were all deadline expiries, with no dial or connection errors (R1). That bounds an outage but not a slow path.

## Prevention (t2)

Nothing below is ordered or changed by this report. Each step needs its own intake, filing and work order through the backlog steward and db-handler-r81.

**L1. Hub request timing.** This is the prerequisite for any root cause.

- Accountable owner: backlog-steward, who files it as a BUG; db-handler-r81 records it.
- Order shape: Small, about 3 paths in the hub API middleware and store.
- What it adds: one log line per request over a threshold (for example 1 s) with route template, status, total time and time spent waiting for the store connection. It also adds a periodic in-flight and waiting count. No request bodies, tokens or IDs beyond the route template.
- Check: a store test in which a request queued behind a held connection logs its wait time. After release, a read-only 24 h report gives per-route p50/p99 and wait share for one busy hour.

**L2. Measure the CPU cap and contention, after L1.**

- Accountable owner: backlog-steward files a read-only measurement item. Any cap change is an owner decision (`tt ask`), because it changes the TrueNAS app config.
- Order shape: Small, read-only, one `docs/measurements/` report plus one docs-only matrix receipt.
- Check: for one busy hour, the report gives L1's wait share and `app.stats` CPU with its normalization confirmed. It says whether the time is connection wait or CPU.

**L3. Sample slow-but-successful calls on the client.**

- Accountable owner: backlog-steward intake.
- Order shape: Small, the relay or the hub reachability watch.
- The watch only opens an episode after 3 min of failures. Slow successes, like every call in this window, are invisible to it. A small timed probe each pass, with an hourly count of calls over 2 s in the relay log, would give a dated latency series without hub changes.
- Check: a unit test of the counter, and one relay log line per hour.

**Pre-retry check for `wi_dabbb869f9767ac4` p2.** It can use today's reads without new code. Before a retry, require both:

- fewer than 5 `deadline exceeded` lines in the relay log in the last 60 min;
- three serial `tt status` calls each under 2 s.

This is a gate, not a fix.

**Existing items.** The match search was done by db-handler-r81 in RESULT #59257, covering open bugs only; done, dismissed and feature items were not searched exhaustively.

| Item | Status | Relation |
|---|---|---|
| `wi_44607de09ce84283` | open | this diagnosis |
| `wi_8154255ce0120652` | open r2 | recovery when a spawn's context read and close both time out; a symptom, not the latency |
| `wi_0cf7c5232a7d6636` | done r4 | earlier retry and recovery control |
| `wi_dabbb869f9767ac4` | open | its retry precondition p2 depends on this item |

No open bug covers single-connection contention, hub request timing or page-size timeouts. L1-L3 are new.

## Read-only conduct (t3)

Probes, all serial:

- **Middleware over the existing `truenas` SSH route:**
  - `system.version`, `reporting.netdata_graphs` and `pool.query`;
  - `app.config tailterm-hub`, printing only volume paths and limits;
  - `app.get_instance` twice;
  - 4 calls to `reporting.netdata_get_data`;
  - one `app.stats` subscription (6 samples);
  - two `app.container_log_follow` reads of 60 lines. The first printed nothing because of a parsing error on my side.
- **Hub:** 2 board pages at `--limit 50`, 3 `tt status`, and coordination posts.
- **Network:** one 5-packet ping.

Nothing was restarted, reconfigured or signalled: not the hub, its container, Tailscale on TrueNAS or any agent. No live task or profile database was opened; hub data came only through `tt`. Local files on the Mini were read in place, and counts only were taken from agent transcripts.

The disk serial number was removed from printed output. Container log lines were redacted before printing. The report contains no token or secret.
