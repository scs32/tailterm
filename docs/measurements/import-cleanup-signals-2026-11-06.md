# Did import job cleanup signal the 11-03 agent exits?

Read-only diagnosis for bug `wi_e4578e89a217972f` r1, work order #58797
(corrects #58791). Source: owner-helper partial diagnosis #58773 (reply to
handler request #58177). Prepared by builder-a217972f on Stephens-Mini,
2026-11-07, at tasks-hub `926ff13f`.

This order covers criteria a1–a3 only. The isolated unrelated-sleeper test
(the item's saved a4) was not run and is not claimed. No test, matrix,
cleanup run, signal, pane capture, restart or live database read was made.
The only process commands run were `pgrep` (which lists and sends nothing)
and `ps`.

## Answer

**No. The import job cleanup code did not cause the exits.** At 13:23:41.795Z
on 11-03, builder-0937d6d9 ran this by hand to remove its own leftover test
`sleep`:

```
pkill -f "^sleep 303$" -u $(id -u) 2>/dev/null
```

macOS `pkill` stops reading options at the first pattern. So `-u` and `501`
became two more patterns, ORed with the first. With `-f`, every process of the
user whose full command line contains `-u` or `501` got SIGTERM. Every
tt-launched Claude and Codex agent carries its briefing in argv, and the
briefing says `tt inbox --unread`, so each one matched. `pkill` skips its own
ancestors by default, so the builder that ran it survived.

17 agent runtimes exited between 13:23:42.157Z and 13:23:44.916Z. The burst
began 0.36 s after the call was issued and while it was still running (its
result returned at 13:23:42.188Z). Desktop apps died in the same second.

There is no kernel record of the sender: the default unified log does not log
signal senders. The attribution rests on the four points under "Why this
establishes causality" below.

## Sources and capture times

| Source | What it gives | Time basis |
| --- | --- | --- |
| Board, read with `tt inbox --board --after 58040 --limit 150 --json` on 11-07 | posts 58041–58190 | Board `createdAt` (hub commit time, UTC) |
| `tt agents --json`, read 11-07 | `createdAt`, `lastSeenAt`, `lastEventAt`, status | hub record times; `lastSeenAt` for these runs is the hub accepting the wrapper's exit report, not the exit |
| Relay process receipts `~/.local/state/tailterm/relay/*.process.json` | pid, start time, `exitedAt`, `exitReportedAt` per run | `exitedAt` is written by the run's own wrapper when its child's Wait returns (`hub/cmd/tt/activity_process.go:171`, called from `wrapperExited`, `hub/cmd/tt/main.go:1754`). Closest available to the true exit time |
| Claude transcripts (`~/.claude/projects/...queue-89d463b1/*.jsonl`) and r80's Codex rollout | tool calls and results with timestamps | runtime's own event times (UTC) |
| macOS unified log, `/usr/bin/log show --start '2026-11-03 05:23:40' --end '2026-11-03 05:23:46'` | 100 lines, exit 0 | local time (PST = UTC−8). Still retained on 11-07 |
| Source #58773 | the times it quotes | Board time of #58773 is 2026-11-06 20:33:57Z (12:33:57 PST). Its text says "read 11-06 12:4x PST", which is after its own post time. Its capture time is therefore not established. Its exit times (13:21:57Z, 13:22:47Z, 12:15:42Z) are not reproduced by any source above (see a1) |

Missing captures: r80's tmux session and final pane are gone (as #58773
says). lead-0937d6d9's "status 143" pane text comes only from #58773, and this
order forbids re-capturing panes. The argv of the dead processes was never
captured. The backlog steward's run (pid 73112) has no transcript path in its
relay activity record.

Raw extracts are kept outside the repository, in
`/Users/stephenspeicher/projects/tailterm-artifacts/wi_e4578e89a217972f/raw/`.

## a1. Chronology

### Each agent and run

The 17 wrapper-recorded exits (`exitedAt`) between 11:00Z and 15:00Z on 11-03
are all in one burst. No exit is recorded at 12:15Z, 13:21Z or 13:22Z.

| Agent (runtime, pid, started) | Last substantive action | Expected next action | Exit evidence and time |
| --- | --- | --- | --- |
| db-handler-r80 (codex, 60241, 11-02 22:17:24 PST) | Was mid-intake of steward REQUEST 58163: a `python3` exec started 13:23:41.779Z, and its rollout ends at 13:23:41.825Z with no output. Last Board post 58122 at 13:02:17Z | file the low bug, then reply to the steward | `exitedAt` 13:23:42.211Z. Wrapper exit 0 (#58773 and rotation notice 58176) |
| db-handler-o3, o4, o5, o6, o7 (codex; pids 57313, 57569, 93476, 47750, 76227; started 10-03 to 10-05) | o4: last post 58159 13:18:09Z (wi_89d4 gate); o3: 58062 12:13:55Z | wait for wi_89d4 commits / the owner decision | `exitedAt` 13:23:42.176–.219Z. No pane receipt (`panePid` absent) |
| plan-reviewer-0937d6d9 (codex, 7233) | not checked further | idle | `exitedAt` 13:23:42.157Z |
| backlog-steward agt_ee9b (claude, 73112, 09-30) | REQUEST 58163 13:22:26Z and NOTICE 58164 13:22:34Z to r80 | wait for r80's RESULT | `exitedAt` 13:23:43.876Z. The source's 13:21:57Z is contradicted by its own posts 29 s and 37 s later |
| lead-0937d6d9 (claude, 6939, 11-03 03:05:56 PST) | RESULT 58160 13:18:17Z; last tool call `tt inbox --unread` 13:21:33Z; turn complete, last transcript entry 13:21:42.265Z | wait for builder's identity fix (58155) and verifier sweep | `exitedAt` 13:23:43.715Z. Pane "status 143" per #58773. The source's 13:22:47Z is not reproduced |
| verifier-0937d6d9 (claude, 7707) | RESULT 58161 13:21:26Z, FINDING 58162 13:21:36Z, `tt progress` 13:21:39Z | wait for the new commit (lead 58156) | `exitedAt` 13:23:43.475Z |
| reviewer-0937d6d9 (claude, 8010) | RESULT 58151 13:16:45Z; last tool call 13:16:49Z | wait for the next REVIEW | `exitedAt` 13:23:44.916Z |
| planner-0937d6d9 (claude, 7076) | not checked further | idle | `exitedAt` 13:23:43.994Z |
| lead-2280253c, wi_faa1 (claude, 81718, 11-02 19:02:09 PST) | last tool call `tt inbox --unread` 12:14:05Z; last transcript entry 12:14:13.9Z, after o3's NOTICE 58062 (owner-decision gate) | wait for the owner decision | `exitedAt` 13:23:43.846Z. **Not an earlier exit:** the source's 12:15:42Z matches the time it went idle, not its death |
| planner, builder, reviewer, verifier-2280253c (claude; 8222, 34198, 19098, 79140) | idle under the same gate | wait | `exitedAt` 13:23:43.732–.957Z |
| **builder-0937d6d9** (claude) | ran the `pkill` | — | **survived** (still running 11-07) |

Also alive afterwards: owner-helper, the deployer, every `tt` wrapper and the
tmux server.

### Cleanup and signal invocation windows

These are wi_89d4's signal-sending actions on 11-03, from transcripts and the
Board. The first and second columns give UTC times.

| When | Who | Action | Signals sent | Exits within ±60 s |
| --- | --- | --- | --- | --- |
| 12:16:56–12:17:42 (04:16 PST) | builder | two background Bash tasks, queried by o4 in 58065 | none: 58066 says "inbox waits", exited cleanly | none |
| 12:30:58–12:31:55, 12:46:58–12:47:47 | builder | browser runs 1–2 (58077–58083, 58103–58111), before any import cleanup code existed. Teardown is `tmux -L <private socket> kill-server` | private tmux server only | none |
| 13:14:07 | — | commit 6ad64840 adds stopImportJobs | — | — |
| 13:18:11–13:18:47 | builder | `go test ./cmd/tt -run TestProjectImportLostCommittedJob` (runs stopImportJobs) → ok | its own test job groups | none |
| 13:19:32–13:19:33 | builder | terminal-v6.sh self-test with an own `sleep 300` and a fake pid 1 record | no group signal: both identities mismatched. `kill $SP` on its own sleep; the call returned 143 from `wait` | none |
| 13:20:06–13:20:35 | builder | go vet plus 5 `go test` selectors (go-runs-4) → ok | its own test job groups | none |
| 13:20:39 / 13:20:47 | verifier | sweep self-test: `kill -TERM -29380`, `kill -KILL -31024` on its own setsid fakes | own fakes | none |
| 13:21:25 | builder | terminal-v6.sh depth check: "identity matches: SIGTERM -31862", its own setsid `sleep 301` | own fake | none |
| 13:21:36 → 13:23:36 | builder | lsof check `(exec 3>held.lock; sleep 303) & … kill %1; wait`. `kill %1` did nothing (no job control), `wait` hung, and the call was moved to background b94eux373 at the 120 s timeout | none effective | — |
| 13:23:40.805 | builder | TaskStop b94eux373 (result 13:23:40.852) | its own background shell | none in the next 1.3 s |
| **13:23:41.795 → 13:23:42.188** | builder | **`pkill -f "^sleep 303$" -u $(id -u)`** | **SIGTERM to every user process whose argv contains `-u` or `501`, except its own ancestors** | **all 17, from 13:23:42.157** |

From 13:22:30Z to 13:23:45Z, across all 32 Claude transcripts written in that
window, the only tool calls are this TaskStop and this `pkill`. The only Codex
call is r80's intake exec.

## a2. Signal paths and causality

### Every signal path in the candidates (read with `git show` / `git grep`)

| Commit | Site | Target, signal | Where the target comes from | Identity checks | Could it reach an unrelated agent? |
| --- | --- | --- | --- | --- | --- |
| b754f64d (04:55) | `hub/cmd/tt/project_import.go:1398` | none: `Setsid: true` makes each job its own group (PGID = PID) | — | — | no signal path. e569cf95, 08ba2b06 and fd908cae add no signal sites in hub/cmd/tt or tests |
| 6ad64840 (05:14) | `hub/cmd/tt/project_import_test.go:81` and `:87` | `syscall.Kill(-pgid, SIGTERM)`, then SIGKILL after 10 s | `*.pid` under the test's `importJobDir()` (isolated HOME). Records are written by the job via `syscall.Getpgid(0)` (`project_import.go:1420`) | `pid > 1 && pgid > 1`. "Alive" is `Wait4` for own children, else `Kill(pid, 0)`. **No start-time check** | Only through a stale record. A SIGTERM'd Go job leaves its `.pid` (defers do not run). The second cleanup (`t.Cleanup` at `:1448`, after the explicit call at `:1537`) can then see `Kill(pid, 0)` succeed on a **reused** PID and signal its group. Every tt-wrapped agent is a group leader (`hub/internal/spawn/spawn.go:382`, `Setpgid: true`), so a reused PID would hit a whole agent. This needs a PID reuse within one test (macOS PIDs are sequential), and it can reach at most one group, never 17 |
| 6ad64840 | `tests/task-form-browser.mjs:136` and `:142` | `process.kill(-job.pgid, SIGTERM / SIGKILL)` | `*.pid` under `importHome` inside this run's `mkdtemp` state | `pid > 1 && pgid > 1`; group alive by `kill(-pgid, 0)`; **no start-time check** | Same stale-record PID-reuse gap, at most one group. Runs only in the browser test's `finally` (`:2198`), and no browser run happened after 12:47:47Z |
| 74cc93fc (05:20) | `project_import_test.go:81` and `:87`; `task-form-browser.mjs:171` and `:177` | same | the browser test reads creator-written `KEY.start` | browser test: PID/PGID/lstart match before SIGTERM | gap narrowed in the browser test |
| 9419d90d (05:30) | `project_import_test.go:123` and `:130` (through the `importJobSignal` seam, `:72`); `task-form-browser.mjs:199` and `:206` | `-pgid` SIGTERM, then SIGKILL only if still the same job | `*.pid` (Go) / `*.start` (browser) under the test's own state | `sameImportJob`: `Getpgid(pid) == pgid` and the same `lstart`, rechecked before SIGKILL; JS `matches()` is the same | A reused PID now fails the start-time check. Remaining gap: the check proves the PID is the recorded process, not that every member of `-pgid` belongs to the job |
| (all) | `task-form-browser.mjs:2199`, `backend.kill("SIGTERM")` | its own hub child | `spawn` handle | — | no |

None of these signal by name or pattern, none uses `pkill`/`killall`/`kill -1`
(`git grep` at 9419d90d over the three changed code files finds none), and none ran between 13:21:26Z and
13:23:42Z.

### What did signal the agents

- **The call.** In the builder transcript, tool_use
  `toolu_01At7a1VJERi4Rds6fJNyC2h` at 13:23:41.795Z is
  `pkill -f "^sleep 303$" -u $(id -u) 2>/dev/null; pgrep -fl "^sleep 30[0-9]$" || echo "no test sleeps left"`.
  Its result at 13:23:42.188Z is "no test sleeps left".
- **The parse.** `man pkill` (macOS 26.5.2) gives the synopsis
  `pkill [-signal] [-ILafilnovx] … [-u euid] pattern ...`. `/usr/bin/pkill`
  and `/usr/bin/pgrep` are one binary (same inode). Read-only demonstration on
  11-07:
  - `pgrep -f "^zz-never-matches-PID$" -u 501` matched **22** live processes:
    4 claude, 1 codex, 1 node, the Codex and Claude desktop helpers,
    chrome_crashpad_handler and others.
  - With the options first, `pgrep -u 501 -f "^zz-never-matches-PID$"` matched
    **0**.
  - Each matching claude/codex agent's argv contains `--unread`, from the
    briefing text. Two also contain `501`.
- **The ancestor exclusion.** The man page says: "By default, the current
  pgrep or pkill process and all of its ancestors are excluded". A
  tt-launched agent's Bash call runs under zsh → claude → zsh → `tt` wrapper →
  tmux server (read for this session on 11-07; builder-0937d6d9's own chain
  on 11-03 was not captured). So the builder's claude, its wrapper and the
  tmux server were spared. The `tt` wrappers' argv does not
  match (no `tt` row among the 22), so they survived. That fits the panes
  printing "[tt] agent command exited with status 143; this pane is now a
  shell". `Wrap` prints that only when the command ends on its own, not when
  the wrapper is signalled (`hub/internal/spawn/spawn.go:403`–`409` versus
  `:439`).
- **Host-wide corroboration (unified log, PST).**
  - WindowServer closed client connections for PIDs 3213, 76731, 90707 and
    90754 at 05:23:41.909–.962.
  - loginwindow logged app death for ChatGPT Computer Use and Microsoft Edge
    at 05:23:42.059–.070, plus a third app at 05:23:45.471.
  - powerd logged `caffeinate.32281 ClientDied` at 05:23:42.403.
  - New zsh processes started from 05:23:42.426: the wrappers' login shells.
  - Nothing at all is logged between 05:23:40.0 and 05:23:41.23, the TaskStop
    window.
  - No memorystatus/jetsam or kernel lines appear in the window.
- **r80's exit 0** fits Codex ending cleanly on SIGTERM. Its last exec started
  13:23:41.779Z, and its exit was recorded 0.43 s later.

### Why this establishes causality

1. **Timing.** All 17 recorded exits fall within 3.1 s that begin 0.36 s
   after the `pkill` call, before its result returned.
2. **Mechanism.** The parse and the match set were demonstrated read-only
   on the same binary.
3. **Selectivity.**
   - Killed: exactly the processes with briefing text in argv (Claude and
     Codex agents across two teams and five handlers), plus unrelated GUI
     apps.
   - Survived: exactly the `pkill` caller's ancestors and the processes with
     neither `-u` nor `501` in argv.
4. **No competitor.** No other tool call ran on the host in that window.
   No cleanup code ran between 13:21:26Z and the burst.

### Alternative hypotheses

| Hypothesis | For | Against |
| --- | --- | --- |
| H1, from #58773: wi_89d4 cleanup code signalled unrelated agents | timing within the 05:14–05:30 PST work | Every cleanup run (13:18, 13:19, 13:20, 13:21Z) produced no exits. The paths can reach at most one group by PID reuse. 17 deaths at one instant need a pattern or broadcast signal |
| Claude Code self-update / restart | panes showed "Update installed · /restart to apply" (#58773) | It killed Codex handlers and Edge too. An installed update does not end running processes. The burst was not tied to any update time |
| Host kill, OOM or memory pressure | the Mini has had memory pressure before | no memorystatus or jetsam lines. The tmux server and the wrappers survived. A selective match by argv |
| tmux or relay action | — | sessions and wrappers survived (panes became shells); the relay does not send signals |
| TaskStop of b94eux373 at 13:23:40.805Z | 1 s before the burst | Nothing logged or recorded during the next 1.3 s. TaskStop targets only its own task's shell |
| The verifier's sweep script (sha256 `7f7e1a9d…`) | it signals `-PGID` for matches | Only its self-test ran (13:20:39–47Z, own fakes, no exits). Targets need argv `project import` + `--job` and a cwd or open file under a root |
| lead-2280253c's earlier, separate exit (#58773) | source time 12:15:42Z | its wrapper recorded 13:23:43.846Z; 12:14:13.9Z was its last activity |

Counterexamples checked:
- A cleanup signal with no exit, five times (above).
- Agents launched in September and October with no import code in them died
  the same way.
- The only wi_89d4 survivor is the one agent whose ancestors `pkill` excludes.

### Remaining uncertainty

- No kernel or audit record names the sender.
- The dead processes' argv was never captured. That they carried `--unread`
  is inferred from the same launch path, which the live agents show.
- The source's 13:21:57Z, 13:22:47Z and 12:15:42Z remain unexplained as exit
  times. Each is contradicted by a later post or by the wrapper receipts.
- The third app death at 05:23:45.471 PST was not identified.

## a3. Prevention proposal

### Existing guards

These are in code or on record:

- 9419d90d identity checks (PID, PGID and start time before each signal, Go
  and JS).
- `pid/pgid > 1` filters (6ad64840 on).
- `Setsid` job groups.
- `Wrap`'s group forwarding and survivor report.
- The owner's standing rule "No host-wide pkill" (owner-session memory only).
  It is not in agent briefings or the product, and it did not reach the
  builder.

### Untested assumptions

- That no agent tool ever signals by pattern.
- That macOS `pkill`/`pgrep` read options after the pattern. **They do not.**
- That an agent's argv is safe to match. **It holds the whole briefing.**
- That a group named by a verified PID contains only the job.
- That terminal-v6.sh's start-time check is enough. It accepts any recorded
  PGID, with no `pid == pgid` or `pgid > 1` test, and its matrix form scans
  the per-user temp folder.

### Proposed prevention items

Each needs its own intake through `role:backlog_steward` and handler-recorded
orders. None is started here.

| ID | Scope | Accountable owner (route) | Bounded order shape | Check |
| --- | --- | --- | --- | --- |
| P1 (permanent) | Product refusal of name- or pattern-based kills inside agent sessions: `pkill`, `killall`, `kill -1`, `kill -- -1`, and `pkill`/`pgrep` with options after a pattern. Applies to Claude Bash calls and Codex execs, through tt's runtime hooks (`tt hook …`). The refusal text points to PID-exact stops (`$!`, a recorded PID with a start-time check) | owner-helper as requester; the steward files the bug, the handler orders it; implementation by a fresh queue team | Planned bug; paths chosen by its planner (hook code, hook tests, briefing text and its mirror docs) | a hook test feeds the exact 11-03 command and `killall node` and expects a refusal, and `kill <own pid>` and `kill -TERM -<own setsid pgid>` and expects them allowed. One matrix |
| P2 (defense in depth) | Stop putting the full briefing in agent argv (pass it by file or stdin), so pattern matches cannot select agents | same route | Planned bug | test: a launched agent's argv has no briefing text, and `pgrep -f -- --unread` finds no agent |
| P3 (wi_89d4 scripts) | terminal-v6.sh and import-job-sweep.mjs: group-signal only when `pid == pgid > 1`; refuse a root equal to the per-user temp folder | wi_89d4's team via db-handler-r81 (lead-0937d6d9 exited) | same-item correction before any further wi_89d4 test or matrix admission | dry check: a record `pid=1 pgid=1` with launchd's real lstart, and one whose PGID differs from its PID, both get "no signal" |
| P4 (this item's a4, still open) | the isolated sleeper test: an unrelated sleeper whose argv contains `--unread` survives import cleanup while the owned job is stopped | handler, by a separate isolated-test admission | as saved on this item | as saved |

Demonstrated prevention stays open:
- None of P1–P4 exists or has passed.
- a4 was not run.
- This report proves no prevention and accepts nothing.
- The parent incident `wi_4767177db9fe4f95` and this item stay open until a
  verified fix is saved and accepted.
