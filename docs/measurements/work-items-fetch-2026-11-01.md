# Work items first row and background refresh fetches, 2026-11-01

Bug `wi_782e0ea7627f9fca` revision 2, work order #53690, builder ASSIGN #53761 (plan by planner-627f9fca, RESULT #53760). Measured at tasks-hub `f03f6a56` on Stephens-Mini. This is the measure-first phase: m1 (how the Bugs tab reaches its first row), m5 (what a background refresh fetches) and the m2 verdict. No client code was changed.

Everything ran on isolated synthetic state. The hub binary ran on 127.0.0.1 with a temporary `TAILTERM_STATE`. The harness stopped only that hub's own PID and removed its temporary directory. The browser pages used fresh Playwright contexts and an in-page synthetic transport. No live hub, profile, tmux pane or item text was read. Only counts come from the live hub (the 10-31 snapshot in the item text).

## Verdicts

- **m2: BUILD.** P6 rule: median time to first row of at least 200 ms in some case, with the tail fetch (first page resolved to last page resolved) at least 50% of it. Cases that meet it:

  | Case | Median first row | Tail fetch | Share |
  |---|---|---|---|
  | Chromium, cold, RTT 0 | 393 ms | 201 ms | 51% |
  | Chromium, cold, RTT 80 | 1,091 ms | 735 ms | 67% |
  | WebKit, cold, RTT 80 | 1,056 ms | 604 ms | 57% |

  WebKit cold at RTT 0 (402 ms, tail 27%) does not qualify. There, rendering all 1,089 rows is the larger share (270 ms). Every warm case is under 200 ms (88-136 ms): the saved cache serves all 7 pages, and the network refresh starts after paint. So the full read is the slow part of a cold load, and the owner's impression holds for an empty or evicted cache.
- **m5:** see P4 below.

## Fixture (counts only)

The same formulas as `tests/tab-switch-browser.mjs`:

- 16 projects, 2 of them open.
- 3,199 agents in the first project, 30-40 in each other.
- 200 messages per project.
- 1,336 work items: 1,089 bugs and 247 features, 35 of them open, the rest done or dismissed. Descriptions run about 1,500-4,700 characters, and every 20th is 8,039.

Text is generated lorem. With the default "All statuses" filter the Bugs tab lists all 1,089 bugs.

In the P1 hub seed every item is open, because done and dismissed would need status updates through the hub's gates. This does not change what the default filter returns: the same 1,089 bugs in 6 pages. A page's size does not depend on status.

The browser transport's `/v1/work-items` follows the hub's paging (`hub/internal/store/work_items.go` `ListWorkItems`). `next` is the last `seq` of every non-empty page, so the view always makes one extra, empty request at the end. Each page is delayed by the RTT plus the hub page time measured in P1 (13 ms). Every other route is delayed by the RTT only.

## Commands

```sh
# P1 + P3 (run 1 stopped at its 14 min hold limit after P1; P3 rerun alone below)
node scripts/verify-matrix-host-lock.mjs exec --item wi_782e0ea7627f9fca --agent builder-627f9fca \
  --timeout-minutes 14 --record DIR --exclusive -- \
  env TEST_BROWSER=both MEASURE_PHASES=hub,first MEASURE_OUT=FILE node tests/measure/work-items-fetch.mjs
# P3
... exec ... -- env TEST_BROWSER=both MEASURE_PHASES=first MEASURE_PAGE_MS=13 MEASURE_OUT=FILE node tests/measure/work-items-fetch.mjs
# P4
... exec ... -- env TEST_BROWSER=both MEASURE_PHASES=refresh MEASURE_PAGE_MS=13 MEASURE_OUT=FILE node tests/measure/work-items-fetch.mjs
```

`TEST_BROWSER=chromium|webkit|both` picks the engines. `MEASURE_SAMPLES` (default 5) and `MEASURE_REFRESH_SECONDS` (default 60) size the run. The harness prints the tables below.

Host-lock holds (all exclusive, on an otherwise empty host):

| Lock | Seq | Phase | Acquired | Released | Outcome |
|---|---|---|---|---|---|
| (first try, record not kept) | not recorded | P1 | 10:53:32Z (broker #53768) | 10:53:55Z | harness error: the hub refused a seed title with a trailing space; fixed, nothing measured |
| b8b3d156 | 2068 | P1 + P3 | 10:56:24Z | 11:10:24Z | P1 complete; P3 stopped at the 840 s run timeout with 2 WebKit cases left, superseded below |
| 84c80281 | 2069 | P3 | 11:11:02Z | 11:15:52Z | complete, exit 0 |

## P1: real hub page cost

Seeding 16 projects and 1,336 items through the API took 549 s. Each page was then read 5 times in sequence over loopback:

| Kind | Page | Items | KiB | ms (median / max of 5) |
|---|---|---|---|---|
| bug | 1 | 200 | 739 | 13.0 / 62.8 |
| bug | 2 | 200 | 723 | 14.8 / 24.8 |
| bug | 3 | 200 | 729 | 12.0 / 26.7 |
| bug | 4 | 200 | 740 | 14.5 / 23.9 |
| bug | 5 | 200 | 729 | 14.7 / 19.7 |
| bug | 6 | 89 | 325 | 6.4 / 9.3 |
| bug | 7 | 0 | 0 | 0.4 / 0.7 |
| feature | 1 | 200 | 729 | 12.3 / 17.1 |
| feature | 2 | 47 | 175 | 3.6 / 4.4 |
| feature | 3 | 0 | 0 | 0.3 / 0.4 |

- The full Bugs read is 7 requests and 3,985 KiB from the real hub.
- The hub's per-item `loadLastDispatch` and `loadTokenBudget` queries (`work_items.go` `ListWorkItems`) cost about 13-15 ms per 200-item page. That is small next to an 80 ms RTT, so they are not the slow part.
- The browser fixture's items lack those hub-only fields: its 7 pages are 3,815 KiB, 4% less.
- The 13 ms page delay used in P3 and P4 sits within the full pages' medians (12.0-14.8 ms).

## P3: Bugs and Features tab, time to first painted row

Five samples per Bugs case and one per Features case. Each sample is a fresh browser context: a cold load on an empty cache, then a reload of the same unlocked vault with its saved cache (warm). The table gives medians, plus the max for time to first row. The live feed carried no events during P3.

How each column is measured:

- **Time to first row:** from the tab click to the first `.work-item` row, then one animation frame.
- **Fetch:** click to the last `listWorkItems` read the first render waited for. It includes `/v1/tasks`.
- **Render:** from that read to paint.
- **First page resolved:** click to the read with `after=0` resolving.
- **Tail fetch:** first page resolved to last page resolved.
- **Before row:** requests started before the row appeared.
- **Cache-served:** `listWorkItems` calls that the read cache answered without a network request.
- **Totals:** include the 5 s after paint, so the warm background refresh is counted.

| Engine | RTT ms | Tab | Cache | First row ms (med / max) | Fetch ms | Render ms | Fetch share | First page resolved ms | Tail fetch ms (share) | Requests before row | Pages before row | KiB before row | Cache-served reads before row | Total requests | Total KiB | Rows |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| chromium | 0 | bugs | cold | 393 / 417 | 231 | 161 | 59% | 24 | 201 (51%) | 8 | 7 | 3815 | 0 of 7 | 8 | 3815 | 1089 |
| chromium | 0 | bugs | warm | 91 / 94 | 10 | 79 | 11% | 8 | 2 (2%) | 0 | 0 | 0 | 7 of 7 | 8 | 3815 | 1089 |
| chromium | 0 | features | cold | 110 / 110 | 72 | 38 | 66% | 28 | 45 (41%) | 4 | 3 | 872 | 0 of 3 | 4 | 872 | 247 |
| chromium | 0 | features | warm | 28 / 28 | 3 | 25 | 12% | 3 | 0 (0%) | 0 | 0 | 0 | 3 of 3 | 4 | 872 | 247 |
| chromium | 80 | bugs | cold | 1091 / 1132 | 928 | 162 | 85% | 190 | 735 (67%) | 8 | 7 | 3815 | 0 of 7 | 8 | 3815 | 1089 |
| chromium | 80 | bugs | warm | 88 / 90 | 11 | 78 | 12% | 9 | 2 (2%) | 0 | 0 | 0 | 7 of 7 | 8 | 3815 | 1089 |
| chromium | 80 | features | cold | 454 / 454 | 414 | 41 | 91% | 197 | 217 (48%) | 4 | 3 | 872 | 0 of 3 | 4 | 872 | 247 |
| chromium | 80 | features | warm | 23 / 23 | 3 | 20 | 14% | 3 | 0 (0%) | 0 | 0 | 0 | 3 of 3 | 4 | 872 | 247 |
| webkit | 0 | bugs | cold | 402 / 432 | 132 | 270 | 33% | 26 | 107 (27%) | 8 | 7 | 3815 | 0 of 7 | 8 | 3815 | 1089 |
| webkit | 0 | bugs | warm | 136 / 142 | 12 | 124 | 9% | 8 | 4 (3%) | 0 | 0 | 0 | 7 of 7 | 8 | 3815 | 1089 |
| webkit | 0 | features | cold | 112 / 112 | 54 | 58 | 48% | 24 | 30 (27%) | 4 | 3 | 872 | 0 of 3 | 4 | 872 | 247 |
| webkit | 0 | features | warm | 35 / 35 | 4 | 31 | 11% | 4 | 0 (0%) | 0 | 0 | 0 | 3 of 3 | 4 | 872 | 247 |
| webkit | 80 | bugs | cold | 1056 / 1065 | 785 | 268 | 74% | 182 | 604 (57%) | 8 | 7 | 3815 | 0 of 7 | 8 | 3815 | 1089 |
| webkit | 80 | bugs | warm | 133 / 141 | 11 | 123 | 8% | 7 | 3 (2%) | 0 | 0 | 0 | 7 of 7 | 8 | 3815 | 1089 |
| webkit | 80 | features | cold | 435 / 435 | 375 | 60 | 86% | 182 | 193 (44%) | 4 | 3 | 872 | 0 of 3 | 4 | 872 | 247 |
| webkit | 80 | features | warm | 31 / 31 | 3 | 28 | 10% | 2 | 1 (3%) | 0 | 0 | 0 | 3 of 3 | 4 | 872 | 247 |

Warm cache coverage: in all 24 warm samples, every bug or feature page the cold load read was still in the saved cache (0 missing of 7 bug pages, 0 of 3 feature pages). There were no page errors.

Reading the cold rows:

- The first page is ready at about 190 ms at RTT 80. That is `/v1/tasks` and then page 1, in sequence.
- The remaining 6 page requests run one after another and add 600-735 ms.
- The 1,089-row render then takes 160 ms in Chromium and 270 ms in WebKit.
- A first-page paint would show rows at about first page resolved plus a 200-row render.
- The trailing empty page costs one RTT on every full read (B4 in the plan).

Render share, a separate finding for the steward and not built here: WebKit spends 123-270 ms rendering all 1,089 rows, both warm and cold.

## P4: background refresh (m5), pending

The 60 s measurement per tab has not run yet. It waits for host admission from db-handler-o5 (#53812) behind another team's matrix. The m5 verdicts below are therefore **pending**. Two inputs exist so far:

- what the code fetches, read at `f03f6a56`;
- one 5 s Chromium smoke run of the same harness (RTT 80, page delay 10 ms, one live event per second). It is preliminary and is not the a2 measurement.

| Tab | What a refresh reads (code) | Whole list or since cursor | Smoke, 5 s Chromium (per minute) |
|---|---|---|---|
| Bugs | `reload()` rereads every page of `listWorkItems` (`client/work-items-view.js:141-152`) on every global feed notice (`:112`), through the cached client (`client/cached-hub-client.js:706`) | whole list (7 pages) | 336 work-items requests, about 178 MiB |
| Board | `getTask(id)`, the project detail with its full agents list (`client/board-view.js:882`; `hub/internal/server/server.go` `getTask` returns `ListAgents`), plus `listMessages(id, {limit: 200, latest: 1})` (`:885`). The cached client turns that into `after=<last seq>` (`client/cached-hub-client.js:173-195`) | messages: since cursor; project detail with 3,199 agents: whole list | 48 project-detail reads, about 73 MiB; messages after cursor, 53 KiB |
| Agents | The Agents tab lists local vault definitions only (`client/agents-view.js`) and makes no hub reads. The hub roster is reread elsewhere: whole `listAgents` in the sidebar task hub (`client/task-hub.js:481`, `client/cached-hub-client.js:688`; `server.go` route has no cursor) and the project detail above | none from the tab | no hub reads |
| Tasks | `getTask` for every open project (`client/tasks-view.js:262`) | whole list (project detail with agents) | 96 project-detail reads, about 74 MiB |

What drives the rate:

- Every non-heartbeat feed event marks every read path stale (`client/cached-hub-client.js:356-360`, `:380`, `:407`). Each view then reloads.
- The cached client also refetches every path touched in the last 40 s every 20 s (`:792-801`), with or without events.

In the smoke run the changed records were about 255 KiB/min for work items, 71 KiB/min for messages and 29 KiB/min for agents. So the Bugs and project-detail rereads were far above 10x the changed bytes.

The rule (COST if a whole list is refetched and it moves at least 1 MiB/min or 10x the changed records' bytes) would mark Bugs, Board (project detail) and Tasks as COST, and Agents as REPORT-ONLY. Those verdicts wait for the real P4 run, which also covers WebKit and a quiet hub (timer only).
