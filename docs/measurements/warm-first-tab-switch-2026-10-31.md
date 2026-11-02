# Warm-first tab switch timing on a loaded and a quiet host, 2026-11-01

Bug `wi_d69dee767211ca54` revision 1, work order #54502, builder ASSIGN #54662 from lead-7211ca54. Measurement only: no product or test code changed. Measured on Stephens-Mini at tasks-hub `ab429d9d7dae02c618950afbefd0a02ae59e1a2c` (pinned by lead NOTICE #54705; it contains the launch base `7afd864d` and `26456e6c`, the landed form of the accepted wi_0b8f candidate `1c1680f6`; `1c1680f6` itself is not an ancestor of `ab429d9d`, but `tests/tab-switch-browser.mjs`, `tests/static-browser.mjs`, `client/main.js`, `client/board-view.js` and `client/work-items-view.js` are identical between the two). `git diff 7afd864d..ab429d9d` touches neither `tests/tab-switch-browser.mjs` nor `client/main.js`.

Everything ran on isolated synthetic state: fresh Playwright browsers and contexts, the test's own in-page synthetic hub transport and a Vite server on 127.0.0.1. No live hub, profile, task data, relay or tmux pane was touched, and no synthetic load was added.

## Verdict

- **No miss on either host.** Set A (normal hold, the host's real load) and set B (exclusive hold, quiet host) each produced 40 valid engine rows, 20 runs per engine. None of the 640 warm-first samples was over 200 ms. The slowest were 129 ms (set A, Chromium, Tasks) and 103 ms (set B, Chromium Tasks and WebKit Board). The parent failure's 305 ms Features sample is about 4x the slowest Features sample here (77 ms).
- **Host load versus the switch itself is not decided at the loads observed.** Set A's real load was close to set B's quiet host: 1-minute load 2.59-5.9 (median 3.65) against 1.63-3.72 (median 2.79), with 15-22% free memory in both. At those loads the switch never missed in either engine (max 129 ms in set A, 103 ms in set B); set A's tail is slightly longer (10 samples over 100 ms against 2). The parent miss came under a heavier load. Its matrix hold (`tailterm-artifacts/wi_0b8f0005264a8360/matrix-1c1680f-logs/host-lock.json`, seq 2035, verifier-264a8360) sampled a 1-minute load of 12.73, 12.65, 10.62, 9.53 and 10.69 at 11:21-11:25Z on 10-31, the five minutes before the failing attempt-1 log was written (11:25:33Z), after a peak of 33.94 at 11:17Z. During the passing retry (attempt-2 log written 11:30:09Z) it was 8.22, 7.25, 7.74, 7.59 and 7.8 (11:26-11:30Z). So the miss came at about 1.6-2.2x set A's highest load, the retry passed at about 7.5, and no set here reached either load. That fits load-driven outliers but does not prove them.
- **The check has no margin for one outlier.** Each tab's "median" is a single switch (`tests/tab-switch-browser.mjs:696-699`, `:740-746`), so one slow switch fails the whole test, while the per-tab medians measured here are 2-90 ms.
- **The root emptying paints the loading shell on every warm switch of a shell tab.** `client/main.js:906` (line 878 at `1c1680f6`) empties the view on every switch. In every one of the 80 rows, 5 of the 8 warm-first switches showed the shell before data (100/160 per engine per set): Bugs, Board, Projects, Features and Queue, the tabs that have a shell. The parent log shows 109/160 across the whole test. Painting the shell is not enough by itself to cause a miss: the data time is measured from the click to the data, and it stayed under 130 ms on every shell-tab switch, each of which painted the shell.
- **Proposed fix (separate build item, not filed here; lead routes it):** take several warm-first samples per tab and hold the median to 200 ms (see "Proposed fix").

## What the order's evidence showed

The parent failure (verifier matrix #52783, wi_0b8f candidate `1c1680f6`, `tests/static-browser.mjs` Chromium attempt 1) logged, at line 5 of `tailterm-artifacts/wi_0b8f0005264a8360/matrix-1c1680f-logs/fcb14610….attempt-1.log`:

```
chromium warm-first: median ms {"bugs":144,"board":99,"tasks":86,"agents":8,"teams":17,"features":305,"queue":46,"terminals":5} samples {"bugs":[144],"board":[99],"tasks":[86],"agents":[8],"teams":[17],"features":[305],"queue":[46],"terminals":[5]}
```

Every tab has one sample, so the "median" is one switch. In the test, `tests/tab-switch-browser.mjs:696-699` records `warm-first` once per tab, and `:740-746` fails any non-cold-first case whose median exceeds `TARGET_MS` (200, `:13`). The same log's line 9 reads `chromium: shells on warm switches 109/160`.

## Method

### Harness

`node tests/tab-switch-browser.mjs` cannot do 40 runs inside a 10-minute exclusive hold. After warm-first it also runs 5 repeat rounds and a revisit case with 105 one-second dwells per engine. So lead NOTICE #54663 approved a warm-first-only harness kept outside the repo:

- `tailterm-artifacts/wi_d69dee767211ca54/warm-first-harness.mjs`, sha256 `50498c0b1936eb125619322ecd1e9242bc573331888ad62440fffe3fda19465b`. Its lines 29-715 are `tests/tab-switch-browser.mjs` lines 13-699 at `ab429d9d`, byte for byte (`cmp` checked before each set): the constants, `fixturePage`, `html`, `median` and `measure` through the warm-first loop. So the latency (80 ms), backlog (30,000 events), live event interval (1 s), dwell (1 s), selectors, `switchTo` timing and loading-shell counting are the test's own.
- Omitted: the repeat and revisit-after-two cases and the end-of-test assertions (dispose, coverage, cache loads). The cold pass and the reload before warm-first are kept, because warm-first needs the cache they fill.
- One harness invocation runs N cycles. Each cycle is Chromium then WebKit, each with a fresh browser and context, as in the test's main block. One Vite server serves the whole invocation (the test starts one per engine). Each engine run prints one JSON row with its start time, duration, 1-minute load averages and free memory before and after.
- Gate (lead RESULT #54991 P4, handler REQUEST #55002): a row fails if it has an error, or if any of the 8 tabs has no warm-first sample or a sample that is not a finite number >= 0. The set stops after cycle 1 when both rows fail, and exits 1 when any row fails. A browser-free check of this gate passed 17/17 (`gate-check-55002/`).
- Runner `run-set.sh`, sha256 `50655c496b948a1a74a5f60b2bc01f236468158a4097aeb8e1b564d219e27a45`, records `uptime`, `memory_pressure -Q` and `verify-matrix-host-lock.mjs status` before and after the set.

### Prerequisite

The queue worktree had no `wasm/tailserve.wasm` (gitignored, `.gitignore:22`). `client/wasm-runtime.js:2` imports it, so Vite failed every page (attempt 2 below). Per lead NOTICE #54989 it was copied with `cp -c` from the main checkout. Both copies are 37,950,982 bytes, mtime 2026-10-19T22:21:34-0700, sha256 `2ea5bf57af44f27cba2dfec694e0004891a1ee9eb5e5fe2f66af2b43455aef75`. **It was built on Oct 19, not from the `ab429d9d` sources.** Lead's reading (#54991) is that the fixture uses only vault unlock and teams, and the runtime loads only on `/keys` routes, so the file has to resolve but never runs. A no-listen Vite preflight then resolved all 53 fixture modules (`module-preflight-54989/`); before the copy it failed on `client/wasm-runtime.js`. The tracked checkout stayed clean (`git status --porcelain` empty).

### Commands

From `/Users/stephenspeicher/projects/tailterm/.build/worktrees/queue-d69dee76`:

```sh
# Set A: normal (non-exclusive) hold, the host's real load
node scripts/verify-matrix-host-lock.mjs exec --item wi_d69dee767211ca54 --agent builder-7211ca54 \
  --timeout-minutes 30 --record /Users/stephenspeicher/projects/tailterm-artifacts/wi_d69dee767211ca54/setA/lock \
  -- /bin/zsh /Users/stephenspeicher/projects/tailterm-artifacts/wi_d69dee767211ca54/run-set.sh setA 20
# Set B: one exclusive hold with a hard 10-minute cap, quiet host
node scripts/verify-matrix-host-lock.mjs exec --item wi_d69dee767211ca54 --agent builder-7211ca54 \
  --exclusive --timeout-minutes 10 --record /Users/stephenspeicher/projects/tailterm-artifacts/wi_d69dee767211ca54/setB/lock \
  -- /bin/zsh /Users/stephenspeicher/projects/tailterm-artifacts/wi_d69dee767211ca54/run-set.sh setB 20
```

Raw outputs (`runs.jsonl`, `stderr.log`, host before/after, lock record, `summary.md`) are in `tailterm-artifacts/wi_d69dee767211ca54/setA/` and `setB/`.

## Holds

| Set | Admission | Lease | Hold | Acquired, released (UTC) | Held | Exit | Rows valid |
|---|---|---|---|---|---|---|---|
| A | o4 #55082 (primary #55073) | `fc7665f6` seq 2110 | normal, 30 min timeout | 05:01:13.131, 05:09:17.004 | 483.9 s | 0 | 40/40 |
| B | o4 #55227 (primary #55219) | `0f780a68` seq 2112 | exclusive, hard 10 min | 05:57:09.818, 06:04:57.847 | 468.0 s | 0 | 40/40 |

Both holds ended with no harness, runner or browser process left and the host lock empty. Each cycle (Chromium then WebKit) took 23-27 s.

### Setup failures (not samples)

- **Attempt 1** (lease `f49e37ce`, seq 2105, 429 ms, exit 1, zero browser runs): the first harness (`c6535e52`) read `chromium` from the namespace of `@playwright/test`'s CommonJS entry, where the exports sit on `default`. Fixed in `8bcd68f9` (lead #54902), proven by a no-browser import check (handler #54904). Records: `setA-attempt1-import-failure/`.
- **Attempt 2** (lease `cfc66d99`, seq 2107, 20.6 s, exit 0): all 40 rows failed at page load because `wasm/tailserve.wasm` was missing (see "Prerequisite"). The harness then had no usable-sample gate, so the wrapper still exited 0. Fixed by the copy, the module preflight and the gate above (lead #54989, #54991; handler #55002). Records: `setA-attempt2-missing-wasm/`.

## Set A versus set B, per tab (ms, 20 samples each)

| tab | engine | set A median | set A max | set B median | set B max |
|---|---|---|---|---|---|
| bugs | chromium | 40 | 64 | 33 | 49 |
| board | chromium | 68 | 89 | 58 | 83 |
| tasks | chromium | 90 | 129 | 81 | 103 |
| agents | chromium | 3 | 16 | 3 | 12 |
| teams | chromium | 4 | 9 | 4 | 8 |
| features | chromium | 35 | 62 | 49 | 77 |
| queue | chromium | 11 | 21 | 11 | 28 |
| terminals | chromium | 2 | 8 | 2 | 5 |
| bugs | webkit | 51 | 94 | 44 | 52 |
| board | webkit | 74 | 115 | 76 | 103 |
| tasks | webkit | 62 | 92 | 62 | 94 |
| agents | webkit | 4 | 7 | 6 | 10 |
| teams | webkit | 4 | 8 | 5 | 8 |
| features | webkit | 40 | 66 | 47 | 74 |
| queue | webkit | 19 | 47 | 23 | 39 |
| terminals | webkit | 3 | 5 | 3 | 4 |

Medians use the test's own rule (`tests/tab-switch-browser.mjs:626-629`: the upper middle of the sorted values). The p90 columns below take the sorted value at index floor(0.9n), the 19th of 20. Samples over 100 ms: set A 10 of 320, set B 2 of 320. Over 150 ms: none in either set.

## Every sample

Per run: each tab's warm-first sample (time from the tab click to its data, as `switchTo` measures it), misses over 200 ms, loading shells seen on the 8 warm-first switches, 1-minute load average and free memory before and after the row, and the row's duration.

### Set A (normal hold, real load): 40 engine rows, 40 valid, 0 failing

#### Set A (normal hold, real load) chromium: every warm-first sample (ms), one switch per tab per run

| run | start (UTC) | bugs | board | tasks | agents | teams | features | queue | terminals | misses > 200 | warm shells | load 1m before/after | mem free % before/after | s |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| 1 | 05:01:13 | 41 | 83 | 103 | 16 | 8 | 28 | 17 | 2 | none | 5/8 | 5.9/5.22 | 15/18 | 12.298 |
| 2 | 05:01:40 | 60 | 82 | 79 | 3 | 4 | 35 | 6 | 6 | none | 5/8 | 4.58/4.18 | 19/18 | 11.358 |
| 3 | 05:02:04 | 46 | 58 | 90 | 4 | 5 | 23 | 12 | 2 | none | 5/8 | 3.4/3.39 | 21/19 | 11.51 |
| 4 | 05:02:28 | 40 | 69 | 129 | 5 | 9 | 31 | 19 | 8 | none | 5/8 | 3.44/3.45 | 20/20 | 11.604 |
| 5 | 05:02:53 | 64 | 68 | 119 | 4 | 3 | 26 | 9 | 2 | none | 5/8 | 3.19/3.1 | 19/19 | 11.658 |
| 6 | 05:03:18 | 54 | 69 | 125 | 3 | 4 | 39 | 12 | 6 | none | 5/8 | 4.19/4.08 | 19/18 | 11.637 |
| 7 | 05:03:43 | 44 | 60 | 97 | 2 | 4 | 41 | 5 | 4 | none | 5/8 | 3.75/3.66 | 18/19 | 11.544 |
| 8 | 05:04:07 | 40 | 77 | 90 | 8 | 2 | 32 | 4 | 6 | none | 5/8 | 3.25/3.65 | 19/19 | 11.67 |
| 9 | 05:04:32 | 38 | 48 | 93 | 5 | 1 | 19 | 12 | 1 | none | 5/8 | 3.63/3.82 | 22/20 | 11.572 |
| 10 | 05:04:56 | 37 | 55 | 77 | 2 | 1 | 46 | 10 | 2 | none | 5/8 | 4.13/3.65 | 21/19 | 11.373 |
| 11 | 05:05:20 | 52 | 51 | 68 | 4 | 2 | 24 | 5 | 1 | none | 5/8 | 3.58/3.88 | 21/19 | 11.345 |
| 12 | 05:05:44 | 29 | 55 | 74 | 2 | 2 | 44 | 21 | 1 | none | 5/8 | 3.73/3.3 | 18/19 | 11.246 |
| 13 | 05:06:08 | 35 | 58 | 108 | 4 | 5 | 30 | 9 | 1 | none | 5/8 | 3.19/3.25 | 20/19 | 11.248 |
| 14 | 05:06:31 | 35 | 43 | 84 | 5 | 2 | 36 | 11 | 1 | none | 5/8 | 2.9/2.69 | 19/18 | 11.189 |
| 15 | 05:06:55 | 32 | 78 | 118 | 2 | 7 | 32 | 17 | 1 | none | 5/8 | 3.4/3.67 | 20/20 | 11.393 |
| 16 | 05:07:18 | 46 | 67 | 64 | 2 | 6 | 32 | 9 | 1 | none | 5/8 | 3.9/4.19 | 19/20 | 11.303 |
| 17 | 05:07:42 | 45 | 72 | 68 | 2 | 3 | 49 | 5 | 1 | none | 5/8 | 3.92/4.41 | 21/18 | 11.381 |
| 18 | 05:08:06 | 34 | 74 | 67 | 2 | 4 | 45 | 9 | 2 | none | 5/8 | 4.11/4.11 | 21/18 | 11.258 |
| 19 | 05:08:29 | 33 | 51 | 58 | 2 | 3 | 35 | 16 | 1 | none | 5/8 | 3.64/3.23 | 20/18 | 11.22 |
| 20 | 05:08:53 | 35 | 89 | 70 | 1 | 7 | 62 | 11 | 2 | none | 5/8 | 2.97/2.59 | 19/18 | 11.355 |

#### Set A (normal hold, real load) chromium: per tab over 20 runs

| tab | median | p90 | max | runs over 200 |
|---|---|---|---|---|
| bugs | 40 | 60 | 64 | 0 |
| board | 68 | 83 | 89 | 0 |
| tasks | 90 | 125 | 129 | 0 |
| agents | 3 | 8 | 16 | 0 |
| teams | 4 | 8 | 9 | 0 |
| features | 35 | 49 | 62 | 0 |
| queue | 11 | 19 | 21 | 0 |
| terminals | 2 | 6 | 8 | 0 |

Loading shells seen on warm-first switches: 100/160. Shell failures: none. Page errors: none.
Runs where the test's per-tab check would fail (any tab over 200): 0/20.

#### Set A (normal hold, real load) webkit: every warm-first sample (ms), one switch per tab per run

| run | start (UTC) | bugs | board | tasks | agents | teams | features | queue | terminals | misses > 200 | warm shells | load 1m before/after | mem free % before/after | s |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| 1 | 05:01:25 | 49 | 80 | 48 | 2 | 7 | 34 | 8 | 4 | none | 5/8 | 5.22/4.58 | 18/19 | 14.407 |
| 2 | 05:01:51 | 65 | 72 | 54 | 6 | 6 | 37 | 16 | 1 | none | 5/8 | 4.18/3.4 | 18/21 | 12.583 |
| 3 | 05:02:15 | 67 | 62 | 46 | 6 | 8 | 53 | 18 | 3 | none | 5/8 | 3.39/3.44 | 19/20 | 12.718 |
| 4 | 05:02:40 | 55 | 71 | 81 | 4 | 2 | 29 | 17 | 2 | none | 5/8 | 3.45/3.19 | 20/19 | 13.626 |
| 5 | 05:03:05 | 94 | 74 | 57 | 2 | 2 | 32 | 13 | 5 | none | 5/8 | 3.1/4.19 | 19/19 | 12.96 |
| 6 | 05:03:30 | 51 | 74 | 70 | 3 | 7 | 29 | 14 | 4 | none | 5/8 | 4.08/3.75 | 18/18 | 13.03 |
| 7 | 05:03:54 | 55 | 95 | 55 | 4 | 5 | 40 | 16 | 4 | none | 5/8 | 3.66/3.25 | 19/19 | 12.664 |
| 8 | 05:04:18 | 53 | 115 | 86 | 4 | 2 | 28 | 9 | 1 | none | 5/8 | 3.65/3.63 | 19/22 | 13.939 |
| 9 | 05:04:44 | 50 | 64 | 53 | 2 | 2 | 61 | 30 | 4 | none | 5/8 | 3.82/4.13 | 20/21 | 12.458 |
| 10 | 05:05:08 | 44 | 112 | 57 | 3 | 8 | 62 | 12 | 2 | none | 5/8 | 3.65/3.58 | 19/21 | 12.337 |
| 11 | 05:05:32 | 55 | 109 | 90 | 5 | 2 | 38 | 26 | 5 | none | 5/8 | 3.88/3.73 | 19/18 | 12.461 |
| 12 | 05:05:55 | 43 | 69 | 92 | 3 | 3 | 45 | 25 | 3 | none | 5/8 | 3.3/3.19 | 19/20 | 12.269 |
| 13 | 05:06:19 | 43 | 74 | 62 | 4 | 7 | 48 | 19 | 4 | none | 5/8 | 3.25/2.9 | 19/19 | 12.336 |
| 14 | 05:06:42 | 42 | 65 | 52 | 2 | 1 | 30 | 35 | 3 | none | 5/8 | 2.69/3.4 | 18/20 | 12.358 |
| 15 | 05:07:06 | 51 | 67 | 67 | 4 | 2 | 43 | 47 | 3 | none | 5/8 | 3.67/3.9 | 20/19 | 12.338 |
| 16 | 05:07:30 | 45 | 79 | 51 | 2 | 4 | 29 | 32 | 1 | none | 5/8 | 4.19/3.92 | 20/21 | 12.296 |
| 17 | 05:07:53 | 44 | 65 | 54 | 5 | 2 | 56 | 16 | 2 | none | 5/8 | 4.41/4.11 | 18/21 | 12.454 |
| 18 | 05:08:17 | 57 | 76 | 62 | 7 | 6 | 44 | 19 | 2 | none | 5/8 | 4.11/3.64 | 18/20 | 12.047 |
| 19 | 05:08:40 | 43 | 80 | 81 | 3 | 7 | 26 | 36 | 1 | none | 5/8 | 3.23/2.97 | 18/19 | 12.149 |
| 20 | 05:09:04 | 50 | 107 | 63 | 4 | 3 | 66 | 37 | 4 | none | 5/8 | 2.59/2.78 | 18/19 | 12.481 |

#### Set A (normal hold, real load) webkit: per tab over 20 runs

| tab | median | p90 | max | runs over 200 |
|---|---|---|---|---|
| bugs | 51 | 67 | 94 | 0 |
| board | 74 | 112 | 115 | 0 |
| tasks | 62 | 90 | 92 | 0 |
| agents | 4 | 6 | 7 | 0 |
| teams | 4 | 8 | 8 | 0 |
| features | 40 | 62 | 66 | 0 |
| queue | 19 | 37 | 47 | 0 |
| terminals | 3 | 5 | 5 | 0 |

Loading shells seen on warm-first switches: 100/160. Shell failures: none. Page errors: none.
Runs where the test's per-tab check would fail (any tab over 200): 0/20.

Load average (1 min) before each row: min 2.59, median 3.65, max 5.9. Set wall time from first start to last end: 483.3 s.

### Set B (exclusive hold, quiet host): 40 engine rows, 40 valid, 0 failing

#### Set B (exclusive hold, quiet host) chromium: every warm-first sample (ms), one switch per tab per run

| run | start (UTC) | bugs | board | tasks | agents | teams | features | queue | terminals | misses > 200 | warm shells | load 1m before/after | mem free % before/after | s |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| 1 | 05:57:10 | 44 | 58 | 93 | 1 | 4 | 26 | 13 | 2 | none | 5/8 | 2.29/2.24 | 16/18 | 11.975 |
| 2 | 05:57:36 | 34 | 55 | 81 | 1 | 2 | 77 | 11 | 2 | none | 5/8 | 2.6/3.12 | 19/19 | 11.477 |
| 3 | 05:58:00 | 30 | 64 | 71 | 12 | 8 | 34 | 10 | 1 | none | 5/8 | 2.79/2.95 | 19/19 | 11.296 |
| 4 | 05:58:23 | 31 | 59 | 78 | 4 | 4 | 57 | 11 | 3 | none | 5/8 | 2.37/3.05 | 19/19 | 11.326 |
| 5 | 05:58:46 | 34 | 52 | 94 | 5 | 4 | 31 | 6 | 1 | none | 5/8 | 3.27/2.98 | 19/18 | 11.301 |
| 6 | 05:59:10 | 30 | 49 | 69 | 1 | 7 | 73 | 6 | 2 | none | 5/8 | 2.84/2.88 | 20/19 | 11.349 |
| 7 | 05:59:33 | 36 | 51 | 80 | 2 | 3 | 43 | 10 | 1 | none | 5/8 | 2.92/2.71 | 21/19 | 11.251 |
| 8 | 05:59:57 | 31 | 83 | 76 | 5 | 3 | 62 | 17 | 1 | none | 5/8 | 3.31/3.52 | 18/18 | 11.228 |
| 9 | 06:00:20 | 38 | 48 | 67 | 2 | 1 | 59 | 8 | 3 | none | 5/8 | 3.52/3.51 | 17/19 | 11.254 |
| 10 | 06:00:44 | 40 | 73 | 81 | 5 | 4 | 29 | 28 | 1 | none | 5/8 | 3.02/2.71 | 20/18 | 11.46 |
| 11 | 06:01:07 | 32 | 52 | 62 | 8 | 4 | 34 | 21 | 1 | none | 5/8 | 2.53/2.79 | 19/19 | 11.234 |
| 12 | 06:01:31 | 32 | 52 | 99 | 3 | 5 | 32 | 10 | 2 | none | 5/8 | 2.52/3.13 | 19/19 | 11.214 |
| 13 | 06:01:54 | 28 | 68 | 89 | 1 | 4 | 49 | 11 | 2 | none | 5/8 | 3.16/2.75 | 21/20 | 11.252 |
| 14 | 06:02:17 | 33 | 74 | 103 | 2 | 2 | 28 | 15 | 1 | none | 5/8 | 2.86/2.93 | 18/18 | 11.279 |
| 15 | 06:02:41 | 49 | 67 | 81 | 4 | 2 | 56 | 8 | 2 | none | 5/8 | 2.71/2.44 | 19/19 | 11.201 |
| 16 | 06:03:04 | 39 | 50 | 94 | 4 | 4 | 45 | 15 | 5 | none | 5/8 | 2.26/1.99 | 20/18 | 11.248 |
| 17 | 06:03:27 | 27 | 44 | 72 | 3 | 6 | 53 | 11 | 2 | none | 5/8 | 1.84/1.63 | 20/19 | 11.211 |
| 18 | 06:03:49 | 29 | 52 | 72 | 2 | 3 | 20 | 11 | 2 | none | 5/8 | 1.71/2.19 | 20/19 | 11.111 |
| 19 | 06:04:12 | 39 | 58 | 86 | 3 | 2 | 59 | 13 | 2 | none | 5/8 | 2.6/2.51 | 20/18 | 11.225 |
| 20 | 06:04:34 | 33 | 72 | 62 | 5 | 3 | 59 | 24 | 2 | none | 5/8 | 3.72/3.62 | 19/18 | 11.197 |

#### Set B (exclusive hold, quiet host) chromium: per tab over 20 runs

| tab | median | p90 | max | runs over 200 |
|---|---|---|---|---|
| bugs | 33 | 44 | 49 | 0 |
| board | 58 | 74 | 83 | 0 |
| tasks | 81 | 99 | 103 | 0 |
| agents | 3 | 8 | 12 | 0 |
| teams | 4 | 7 | 8 | 0 |
| features | 49 | 73 | 77 | 0 |
| queue | 11 | 24 | 28 | 0 |
| terminals | 2 | 3 | 5 | 0 |

Loading shells seen on warm-first switches: 100/160. Shell failures: none. Page errors: none.
Runs where the test's per-tab check would fail (any tab over 200): 0/20.

#### Set B (exclusive hold, quiet host) webkit: every warm-first sample (ms), one switch per tab per run

| run | start (UTC) | bugs | board | tasks | agents | teams | features | queue | terminals | misses > 200 | warm shells | load 1m before/after | mem free % before/after | s |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| 1 | 05:57:22 | 44 | 68 | 60 | 6 | 2 | 27 | 21 | 1 | none | 5/8 | 2.24/2.6 | 18/19 | 14.228 |
| 2 | 05:57:47 | 41 | 65 | 54 | 3 | 2 | 63 | 26 | 2 | none | 5/8 | 3.12/2.79 | 20/19 | 12.205 |
| 3 | 05:58:11 | 40 | 100 | 56 | 5 | 8 | 62 | 19 | 3 | none | 5/8 | 2.95/2.37 | 19/19 | 12.133 |
| 4 | 05:58:34 | 43 | 57 | 50 | 7 | 7 | 39 | 18 | 3 | none | 5/8 | 3.05/3.27 | 19/19 | 12.042 |
| 5 | 05:58:58 | 41 | 83 | 46 | 7 | 4 | 74 | 28 | 4 | none | 5/8 | 2.98/2.84 | 18/20 | 12.211 |
| 6 | 05:59:21 | 45 | 59 | 69 | 5 | 6 | 30 | 20 | 2 | none | 5/8 | 2.88/2.92 | 19/21 | 12.056 |
| 7 | 05:59:45 | 46 | 100 | 65 | 6 | 6 | 55 | 29 | 4 | none | 5/8 | 2.71/3.31 | 19/18 | 12.146 |
| 8 | 06:00:08 | 47 | 73 | 94 | 8 | 4 | 44 | 11 | 2 | none | 5/8 | 3.52/3.52 | 18/17 | 12.313 |
| 9 | 06:00:32 | 45 | 75 | 46 | 5 | 1 | 59 | 32 | 2 | none | 5/8 | 3.51/3.02 | 19/20 | 12.248 |
| 10 | 06:00:55 | 44 | 103 | 55 | 2 | 6 | 48 | 19 | 3 | none | 5/8 | 2.71/2.53 | 18/19 | 12.122 |
| 11 | 06:01:19 | 52 | 95 | 67 | 8 | 3 | 47 | 29 | 1 | none | 5/8 | 2.79/2.52 | 19/19 | 12.005 |
| 12 | 06:01:42 | 51 | 64 | 62 | 10 | 2 | 43 | 16 | 3 | none | 5/8 | 3.13/3.16 | 19/21 | 12.138 |
| 13 | 06:02:05 | 43 | 68 | 55 | 2 | 3 | 32 | 39 | 3 | none | 5/8 | 2.75/2.86 | 20/18 | 12.049 |
| 14 | 06:02:29 | 49 | 77 | 85 | 2 | 8 | 58 | 23 | 4 | none | 5/8 | 2.93/2.71 | 18/19 | 12.2 |
| 15 | 06:02:52 | 42 | 99 | 63 | 8 | 6 | 45 | 35 | 4 | none | 5/8 | 2.44/2.26 | 19/20 | 11.514 |
| 16 | 06:03:15 | 43 | 88 | 62 | 7 | 3 | 30 | 24 | 2 | none | 5/8 | 1.99/1.84 | 18/20 | 11.793 |
| 17 | 06:03:38 | 41 | 98 | 92 | 4 | 8 | 51 | 29 | 2 | none | 5/8 | 1.63/1.71 | 19/19 | 11.598 |
| 18 | 06:04:01 | 44 | 62 | 43 | 7 | 2 | 47 | 16 | 3 | none | 5/8 | 2.19/2.6 | 19/20 | 11.289 |
| 19 | 06:04:23 | 44 | 76 | 75 | 2 | 5 | 28 | 8 | 3 | none | 5/8 | 2.51/3.72 | 18/19 | 11.389 |
| 20 | 06:04:46 | 43 | 58 | 63 | 7 | 7 | 44 | 23 | 2 | none | 5/8 | 3.62/3.22 | 18/20 | 11.467 |

#### Set B (exclusive hold, quiet host) webkit: per tab over 20 runs

| tab | median | p90 | max | runs over 200 |
|---|---|---|---|---|
| bugs | 44 | 51 | 52 | 0 |
| board | 76 | 100 | 103 | 0 |
| tasks | 62 | 92 | 94 | 0 |
| agents | 6 | 8 | 10 | 0 |
| teams | 5 | 8 | 8 | 0 |
| features | 47 | 63 | 74 | 0 |
| queue | 23 | 35 | 39 | 0 |
| terminals | 3 | 4 | 4 | 0 |

Loading shells seen on warm-first switches: 100/160. Shell failures: none. Page errors: none.
Runs where the test's per-tab check would fail (any tab over 200): 0/20.

Load average (1 min) before each row: min 1.63, median 2.79, max 3.72. Set wall time from first start to last end: 467.4 s.

## w2: the loading shell on warm switches

The tab change in `client/main.js` empties the view on every switch: `view.replaceChildren()` at `client/main.js:906` (the order cites line 878, which was its place at `1c1680f6`). The fixture copies this at `tests/tab-switch-browser.mjs:505`. Each tab with a loading shell then paints the shell whenever its root is empty, before its first await:

- `client/board-view.js:778` (Board)
- `client/tasks-view.js:208` (Projects)
- `client/queue-view.js:105` (Queue)
- `client/work-items-view.js:132` (Bugs and Features)

Agents, Teams and Terminals have no shell (`SHELL_FREE` in the test, `tests/tab-switch-browser.mjs:47`). That makes 5 shell tabs of 8: Bugs, Board, Projects (`tasks`), Features and Queue. That matches the 5/8 shells counted in every row of both sets. The harness counts shells per row, not per tab, so which 5 tabs is read from the code and the matching count, not measured tab by tab. So yes: with a warm cache, every switch to a shell tab still paints the loading shell for a moment before the cached data replaces it. In these runs the shell was always followed by data within 130 ms, the test's shell checks (`shellFailures`, a shell left after data) never fired, and no page errors appeared.

The shell alone does not explain the misses: it painted on all 400 warm switches to a shell tab across both sets (5 per row, 80 rows), and every one of them reached its data in under 130 ms. Whether a warm switch should show the shell at all is a separate UI question, not proposed here.

## Proposed fix (separate build item)

**Several warm-first samples per tab, median held to 200 ms.** In `tests/tab-switch-browser.mjs`, repeat the reload and warm-first pass three times (or five, like `ROUNDS`) and record each tab's warm-first sample on every pass, so `median()` takes the middle of 3-5 switches instead of one. A single slow switch would then not fail the test, while a switch that is really slow on most passes still would. Evidence: 0 of 640 warm-first samples over 200 ms on either host, with per-tab medians of 2-90 ms. The recorded failures were single samples: 305 ms Chromium Features in #52783, with a 45 ms pass on the retry, and one WebKit sample in #52573. Reviewer #52226 also noted a single 183 ms WebKit Bugs sample, 207 once. Cost: about 10 s per extra pass per engine (8 tabs with a 1 s dwell, plus a reload). The test's other cases already take 5 samples per tab.

A product change, keeping the view's root on warm switches so the shell does not paint, is not needed for the 200 ms target on this evidence. It belongs to the shell design, not to this bug.

## Limits

- Set A started as the host came down from a busier spell: `setA/host-before.txt` reads load averages 6.16 10.07 6.62 (1, 5 and 15 minutes) at 05:01Z. Its rows then ran at a 1-minute load of 2.59-5.9, close to set B. Neither set reached the 9.5-12.7 recorded before the parent miss, or the 7.25-8.22 of its passing retry (see Verdict). No load was added artificially, by design (w1).
- Set B's "quiet host" means the exclusive host lock: no other matrix or test hold (overlap 0), at a recorded 1-minute load of 1.63-3.72. It is not proven idle. Three resident test fixture process groups from September were found later and stopped at 06:38Z on 11-02, after set B ended at 06:04:57Z (FINDING #55315, owner cleanup #55325). They were PGID 21127 and 53829 (`isolated-runner-launch-supervisor.mjs`, 09-13) and PGID 86125 (`timeout-fixture.mjs` with a child listening on localhost, 09-29). Their activity during sets A and B was not sampled, so no effect on these timings is claimed either way.
- The harness runs only the cold pass, the reload and the warm-first switches of the test. The repeat and revisit cases that follow warm-first in the test, and their effect on later timing, were not run.
- `wasm/tailserve.wasm` came from an Oct 19 build in the main checkout, not from the `ab429d9d` sources. The fixture needs the file to resolve and, by lead's reading of `client/local-vault.js`, never runs it.
- 20 runs per engine per set show no miss in 160 tab switches per engine per set. They do not give a miss rate for a loaded host.
