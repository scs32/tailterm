# Work items first row and background refresh fetches, 2026-11-01

Bug `wi_782e0ea7627f9fca` revision 2, work order #53690, builder ASSIGN #53761 (plan by planner-627f9fca, RESULT #53760). Measured at tasks-hub `f03f6a56` on Stephens-Mini. It covers m1 (how the Bugs tab reaches its first row), m5 (what a background refresh fetches), the m2 verdict, and the timing after the phase-2 first-page paint (lead REQUEST #53826).

Everything ran on isolated synthetic state. The hub binary ran on 127.0.0.1 with a temporary `TAILTERM_STATE`. The harness stopped only that hub's own PID and removed its temporary directory. The browser pages used fresh Playwright contexts and an in-page synthetic transport. No live hub, profile, tmux pane or item text was read. Only counts come from the live hub (the 10-31 snapshot in the item text).

## Verdicts

- **m2: BUILD.** P6 rule: median time to first row of at least 200 ms in some case, with the tail fetch (first page resolved to last page resolved) at least 50% of it. Cases that meet it:

  | Case | Median first row | Tail fetch | Share |
  |---|---|---|---|
  | Chromium, cold, RTT 0 | 393 ms | 201 ms | 51% |
  | Chromium, cold, RTT 80 | 1,091 ms | 735 ms | 67% |
  | WebKit, cold, RTT 80 | 1,056 ms | 604 ms | 57% |

  WebKit cold at RTT 0 (402 ms, tail 27%) does not qualify. There, rendering all 1,089 rows is the larger share (270 ms). Every warm case is under 200 ms (88-136 ms): the saved cache serves all 7 pages, and the network refresh starts after paint. So the full read is the slow part of a cold load, and the owner's impression holds for an empty or evicted cache.
- **m2 after the fix:** the first row now waits for `/v1/tasks` and one page. Cold Bugs at RTT 80 drops from 1,091 to 216 ms in Chromium and from 1,056 to 209 ms in WebKit (see "After" below).
- **m5:** COST for the Bugs list and for the project detail with its full agents list (Board, Tasks). REPORT-ONLY for Board messages, which are already incremental, and for the Agents tab, which makes no hub reads (see P4). The COST verdicts are reported, not built.

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
- **Before row:** requests started before the row appeared (`start <= row seen`). In the harness at `efeb4b90` and later, a second count in parentheses gives the requests answered before the row (`end <= row seen`): the reads the row waited for. a7 is judged on that answered count. In this before-table every read is sequential and finished before the row, so the two counts coincide, and the single count shown is both.
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

## After: first page paint (a7)

Measured with the same harness and cases after the phase-2 change in `client/work-items-view.js` (lock 1293a29c, seq 2072, 12:40:37Z-12:48:27Z, harness and view at `a06aff7f`; the later commits change only tests/work-item-search-browser.mjs and this report). Both request counts are shown: requests started before the row, and in parentheses those answered before it.

| Engine | RTT ms | Tab | Cache | Time to first row ms (med / max) | Fetch ms | Render ms | Fetch share | First page resolved ms | Tail fetch ms (share) | Requests before row (answered) | Pages before row (answered) | KiB before row (answered) | Cache-served reads before row | Total requests | Total KiB | Rows |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| chromium | 0 | bugs | cold | 37 / 86 | 23 | 14 | 63% | 23 | 0 (0%) | 2 (2) | 1 (1) | 713 (713) | 0 of 1 | 7 | 3815 | 1089 |
| chromium | 0 | bugs | warm | 22 / 48 | 9 | 16 | 39% | 9 | 0 (0%) | 0 (0) | 0 (0) | 0 (0) | 1 of 1 | 7 | 3815 | 1089 |
| chromium | 0 | features | cold | 38 / 38 | 23 | 15 | 60% | 23 | 0 (0%) | 2 (2) | 1 (1) | 704 (704) | 0 of 1 | 3 | 872 | 247 |
| chromium | 0 | features | warm | 23 / 23 | 10 | 13 | 42% | 10 | 0 (0%) | 0 (0) | 0 (0) | 0 (0) | 1 of 1 | 3 | 872 | 247 |
| chromium | 80 | bugs | cold | 216 / 218 | 196 | 21 | 91% | 196 | 0 (0%) | 2 (2) | 1 (1) | 713 (713) | 0 of 1 | 7 | 3815 | 1089 |
| chromium | 80 | bugs | warm | 22 / 34 | 9 | 13 | 41% | 9 | 0 (0%) | 0 (0) | 0 (0) | 0 (0) | 1 of 1 | 7 | 3815 | 1089 |
| chromium | 80 | features | cold | 209 / 209 | 190 | 19 | 91% | 190 | 0 (0%) | 2 (2) | 1 (1) | 704 (704) | 0 of 1 | 3 | 872 | 247 |
| chromium | 80 | features | warm | 17 / 17 | 3 | 15 | 16% | 3 | 0 (0%) | 0 (0) | 0 (0) | 0 (0) | 1 of 1 | 3 | 872 | 247 |
| webkit | 0 | bugs | cold | 51 / 78 | 23 | 27 | 45% | 23 | 0 (0%) | 3 (2) | 2 (1) | 1403 (713) | 0 of 1 | 7 | 3815 | 1089 |
| webkit | 0 | bugs | warm | 34 / 38 | 7 | 27 | 21% | 7 | 0 (0%) | 0 (0) | 0 (0) | 0 (0) | 1 of 1 | 7 | 3815 | 1089 |
| webkit | 0 | features | cold | 48 / 48 | 22 | 26 | 46% | 22 | 0 (0%) | 3 (2) | 2 (1) | 872 (704) | 0 of 1 | 3 | 872 | 247 |
| webkit | 0 | features | warm | 25 / 25 | 22 | 3 | 88% | 3 | 19 (76%) | 0 (0) | 0 (0) | 0 (0) | 2 of 2 | 3 | 872 | 247 |
| webkit | 80 | bugs | cold | 209 / 210 | 182 | 27 | 87% | 182 | 0 (0%) | 3 (2) | 2 (1) | 1403 (713) | 0 of 1 | 7 | 3815 | 1089 |
| webkit | 80 | bugs | warm | 35 / 39 | 7 | 27 | 20% | 7 | 0 (0%) | 0 (0) | 0 (0) | 0 (0) | 1 of 1 | 7 | 3815 | 1089 |
| webkit | 80 | features | cold | 209 / 209 | 181 | 28 | 87% | 181 | 0 (0%) | 3 (2) | 2 (1) | 872 (704) | 0 of 1 | 3 | 872 | 247 |
| webkit | 80 | features | warm | 28 / 28 | 25 | 3 | 89% | 3 | 22 (79%) | 0 (0) | 0 (0) | 0 (0) | 2 of 2 | 3 | 872 | 247 |

What changed:

- **Cold Bugs, RTT 80:** 1,091 to 216 ms in Chromium, and 1,056 to 209 ms in WebKit.
- **Cold Bugs, RTT 0:** 393 to 37 ms in Chromium, and 402 to 51 ms in WebKit.
- **Warm Bugs:** 17-35 ms, against 88-136 ms before.

On every cold load, the requests answered before the first row are `/v1/tasks` and one work-items page. That page is 713 KiB for Bugs and 704 KiB for Features.

In WebKit the started count is one higher, a second page. Page 2 is requested in the same task that renders page 1, so it starts before the row is observed, but the row does not wait for it.

The tail share is 0%: no read finishes between the first page and the row.

The full read is now 7 requests, down from 8. The read stops at the short sixth page instead of reading an empty seventh. The rest of the list still loads, and all 1,089 rows are listed within the 5 s settle window in every case.

Warm Features in WebKit shows 2 of 2 cache-served reads before the row, with a 22 ms tail. Its second, 47-item page was already read from the cache before the row appeared.

In the before-table above, the two counts coincide. Its "cache-served reads before row" column shows all 7 page reads (0 of 7 from cache) finished before the row, and `/v1/tasks` finished before page 1 was requested. So the 8 requests started before the row were also the 8 answered before it.

## P4: background refresh (m5)

Measured under one exclusive hold, after the search browser rerun in the same hold: lock 3ec46deb, seq 2073, 12:54:38Z-13:05:26Z, at `16f5477c`.

Each tab was opened alone, at RTT 80 with the 13 ms page delay. After its data painted and 3 s passed, requests were counted for 60 s. Each second a live event changed one work item, posted one message and changed one agent's activity. The cached client's real 20 s refresh timer ran throughout.

A quiet run, with no events and only the timer, was added in Chromium for Bugs and Board. There were no page errors, unknown routes or writes.

The view measured is the phase-2 one. A background refresh of a complete list reads every page before showing it, as before. It now reads 6 pages, not 7: there is no trailing empty page.

| Engine | Tab | Live events | Route | Requests/min | KiB/min |
|---|---|---|---|---|---|
| chromium | bugs | 1 per 1000 ms | tasks list | 59 | 423 |
| chromium | bugs | 1 per 1000 ms | work-items | 354 | 224619 |
| chromium | board | 1 per 1000 ms | work-items (no kind) | 118 | 2 |
| chromium | board | 1 per 1000 ms | tasks list | 59 | 423 |
| chromium | board | 1 per 1000 ms | other | 177 | 4 |
| chromium | board | 1 per 1000 ms | task detail (with agents) | 59 | 91413 |
| chromium | board | 1 per 1000 ms | messages after cursor | 59 | 70 |
| chromium | agents | 1 per 1000 ms | (no hub reads) | 0 | 0 |
| chromium | tasks | 1 per 1000 ms | other | 239 | 5 |
| chromium | tasks | 1 per 1000 ms | tasks list | 59 | 423 |
| chromium | tasks | 1 per 1000 ms | task detail (with agents) | 118 | 92588 |
| chromium | bugs | none | tasks list | 1 | 7 |
| chromium | bugs | none | work-items | 9 | 5522 |
| chromium | board | none | tasks list | 1 | 7 |
| chromium | board | none | other | 3 | 0 |
| chromium | board | none | task detail (with agents) | 1 | 1549 |
| chromium | board | none | messages after cursor | 1 | 0 |
| webkit | bugs | 1 per 1000 ms | tasks list | 59 | 423 |
| webkit | bugs | 1 per 1000 ms | work-items | 354 | 224640 |
| webkit | board | 1 per 1000 ms | work-items (no kind) | 118 | 2 |
| webkit | board | 1 per 1000 ms | tasks list | 59 | 423 |
| webkit | board | 1 per 1000 ms | other | 177 | 4 |
| webkit | board | 1 per 1000 ms | task detail (with agents) | 59 | 91419 |
| webkit | board | 1 per 1000 ms | messages after cursor | 59 | 70 |
| webkit | agents | 1 per 1000 ms | (no hub reads) | 0 | 0 |
| webkit | tasks | 1 per 1000 ms | other | 239 | 5 |
| webkit | tasks | 1 per 1000 ms | tasks list | 59 | 423 |
| webkit | tasks | 1 per 1000 ms | task detail (with agents) | 118 | 92595 |

| Changed records per minute (events at 1 per s) | Records | KiB/min |
|---|---|---|
| Work items (Chromium / WebKit) | 60 / 59 | 209 / 206 |
| Messages | 60 / 59 | 71 / 69 |
| Agents | 60 / 59 | 29 / 29 |

Verdicts by the rule. A view is COST if a background refresh refetches a whole list and moves at least 1 MiB/min, or at least 10x the bytes of the records that changed. Otherwise it is REPORT-ONLY.

| Tab | Whole list or since cursor (code) | Measured | Verdict |
|---|---|---|---|
| Bugs | Whole list: `reload()` rereads every page (`client/work-items-view.js`), on every global feed notice (`subscribe("", ...)` in `show()`), through `client/cached-hub-client.js:706` | 354 page reads and about 219 MiB/min, about 1,070x the 209 KiB/min of changed work items. Quiet hub, timer only: 9 page reads and 5.4 MiB/min | **COST** |
| Board | Messages: since cursor. `listMessages(id, {limit: 200, latest: 1})` (`client/board-view.js:885`) becomes `after=<last seq>` (`client/cached-hub-client.js:173-195`). Project detail: whole list. `getTask(id)` (`client/board-view.js:882`) returns the project with all 3,199 agents (`hub/internal/server/server.go` `getTask` calls `ListAgents`) | Messages: 70 KiB/min, about 1x the changed messages. Project detail: about 89 MiB/min, about 3,100x the 29 KiB/min of changed agents. Quiet: 1.5 MiB/min | Messages: **REPORT-ONLY** (already incremental). Project detail: **COST** |
| Agents | The tab lists local vault definitions only (`client/agents-view.js`) and makes no hub reads. The sidebar task hub rereads the whole `listAgents` (`client/task-hub.js:481`, `client/cached-hub-client.js:688`; the hub route has no cursor), but this harness does not run it | No hub reads from the tab | **REPORT-ONLY** |
| Tasks (added) | Whole list: `getTask` for every open project (`client/tasks-view.js:262`) | about 90 MiB/min | **COST** |

What drives the rate:

- Every non-heartbeat feed event marks every read path stale (`client/cached-hub-client.js:356-360`, `:380`, `:407`), and each open view reloads.
- Every 20 s the client also refetches each path touched in the last 40 s (`:792-801`).
- One Bugs reload moves about 3.7 MiB, and one project detail about 1.5 MiB.

So the per-minute cost scales with the live hub's rate of non-heartbeat events, which this measurement did not read. The quiet run gives the floor: 5.4 MiB/min for an open Bugs tab and 1.5 MiB/min for an open Board.

As the order requires, the COST verdicts are reported, not built. A changes-since read for work items and agents (with a full reload kept as the fallback) needs a hub list parameter, which is outside this order's owned paths. It goes to the handler as an amendment or a new item.
