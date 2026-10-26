# Provider-blocked agents

Bug `wi_72f41bd375032cf0` revision 1, owner order #11854, plan request #19860, builder assignment #19880. The owner ping, the owner helper's episode and the wake after a restored login: bug `wi_1ef7c41da8711d4b` revision 2, owner order #31849, builder assignment #31882. Closing an episode on a model reply reported in another state: bug `wi_8204795815c04d72` revision 2, owner order #33182, builder assignment #33203. No block for a done or dead session, the report key and rotation: bug `wi_f3eed0595569da26` revision 3, owner order #38353, plan #38386, builder assignment #38388. A done agent's turn as recovery, a done agent woken into a block, and the idle rules: bug `wi_67f6c4fd414fac63` revision 2, owner order #39318, plan #39352, builder assignment #39355. A done agent's dropped block does not end an outage: bug `wi_9a62b8ab3a580cfe` revision 1, owner order #39958, owner-helper decision #39975, builder assignment #39976. Codex's retry-limit and overload turn ends, the retry wakes and the blocked handler's replacement: bug `wi_ce7310979bc45479` revision 3, order #46760, plan request #46769, builder assignment #46804.

On 2026-09-25 every Codex request failed with a 401 for about ninety minutes, and on 2026-09-26 a Claude reviewer hit a model usage limit before its first reply. Both looked idle. The relay activity monitor now reports a distinct state, `provider_blocked`, when the provider refuses an agent's requests. It is separate from the stall states (`stuck`, `hung_tool`, `looping`, `crashed`): the agent is healthy, and its turns fail until someone fixes the account, the login or the model.

## What is reported

The relay reads only typed fields of the agent's own transcript. The hub receives:

| Field | Values |
| --- | --- |
| `provider` | `anthropic` for Claude Code, `openai` for Codex |
| `runtime` | `claude` or `codex` |
| `model` | the last real model the transcript named, or `unknown` |
| `class` | `usage_limit`, `auth`, `rate_limited` or `server_error` |
| `code` | the runtime's short error code, for example `rate_limit` |
| `status` | the HTTP status when the runtime recorded one, otherwise absent |
| `since` | the time of the first failing record of this run of failures |

Class, code and status together are the exact error class. No provider message, key fragment, request ID or other transcript text is stored in the relay's state file or sent to the hub. The hub rejects a report whose fields are not one of the allowed values, patterns or ranges.

## Detection

| Runtime | Evidence | Class | Reported |
| --- | --- | --- | --- |
| Claude | API-error record with `error` `authentication_failed`, or status 401 or 403 | `auth` | at the first record |
| Claude | `error` `rate_limit` and the text starts with `You've reached your` or `You've hit your` | `usage_limit` | at the first record |
| Claude | `error` `rate_limit` otherwise, or status 429 | `rate_limited` | after repeated failures |
| Claude | `error` `server_error` or `overloaded`, or a 5xx status | `server_error` | after repeated failures |
| Codex | `task_complete` error message starts `unexpected status 401` or `403` | `auth` | at the first record |
| Codex | the same with status 429 | `rate_limited` | after repeated failures |
| Codex | the same with a 5xx status | `server_error` | after repeated failures |
| Codex | `codex_error_info` `{"response_too_many_failed_attempts":{"http_status_code":N}}`, or else a message that starts `exceeded retry limit, last status: N` | the class of status N (`rate_limited` for 429, `server_error` for 5xx, `auth` for 401 or 403), code `response_too_many_failed_attempts` | at the first record |
| Codex | `codex_error_info` `server_overloaded` (observed with the message `Selected model is at capacity. Please try a different model.`) | `server_error`, code `server_overloaded`, no status | after repeated failures |

- "Repeated" is two failed turns in a row with no model output between them. `TAILTERM_ACTIVITY_PROVIDER_REPEAT` (2 to 20) on the relay host changes it. One transient failure keeps today's `idle` or `finished_silent` with its turn-end reason.
- Codex retries a failing request itself, about a second in all, before it ends the turn with `exceeded retry limit`. That turn has already failed repeatedly, so it is a block at its first record (the relay marks the block `exhausted`). Both 2026-10-25 and 2026-10-26 handler stalls ended their turns this way, and neither was detected before this rule. Only the class, code and status are kept; nothing of the message is.
- A different class starts a new block with a new `since`.
- Only evidence of success clears a block: for Claude an ordinary assistant record, for Codex model output or a `task_complete` without an error. A new prompt alone does not.
- A block is reported only while the session can still make calls. A done agent, or one whose tmux session or runtime process is gone, reports its ordinary state instead, for example `idle` with the turn-end reason; closed and exited agents are not observed at all. The relay keeps the block, so an agent resumed with it unresolved reports it again.
- A done agent woken into a block is the exception. Codex agents are done between every turn (only their Stop hook sets a status), so a woken Codex agent whose turn fails is still done, and its block is a live one. A done agent whose session is live is reported blocked when input waits for it (unread messages or open obligations) and either the relay's check read a new failed turn of the block or the relay last reported it blocked. It stays reported while that input waits; with nothing waiting it reports its ordinary state again. A message that arrived before the wake ran is not a failed turn, so a stale block is not reported for it. A failure that extends an earlier block keeps its `since`, so the hub reopens that episode with no new notice: the lead and the owner heard of it when it opened.
- A running session's block has no time limit. Its turns keep failing until the cause is fixed, and the episode is what carries the owner ping and the wake after a restored login; a time limit would end both while the cause remains.
- A change of model, code or status within a block is a new report with the same `since`; the hub keeps the episode and notifies no one again. A steady block makes no new report.
- Other API errors, such as an invalid request, are not provider blocks.
- A Codex usage limit shows as a modal prompt and stays with [runtime-prompts.md](runtime-prompts.md) (`codex_usage_limit`). `runtime_prompt` takes precedence over `provider_blocked`; `crashed` does too. `provider_blocked` takes precedence over every other state, including `stuck` and a `needs_input` status.
- The Codex 401, retry-limit 429 and `server_overloaded` records were observed. The 403, 429 and 5xx forms of `unexpected status NNN` follow the 401 shape and are covered by synthetic fixtures; a Codex release that words them differently will not be detected until the pattern is extended.

## Notifications

A block from its first report until the agent recovers is one episode. A report of a completed or running turn (`working`, `idle` or `finished_silent`) by an agent that is not done is recovery. A done agent reports its turn state while its stale block is dropped (see Detection), so its turn says nothing about the provider: from a done agent a turn report is recovery only when it shows a model reply since the block (output above the baseline, below). Without one it closes the episode with no recovery wake and no "login restored" notice, and marks it dropped, so it does not end an outage either (owner-helper decision #39975, under Outage below). A model reply is recovery from any agent. A report in any other state (an unknown probe, which includes an agent whose status is `needs_input`, a crash, a stall state or a runtime prompt) says nothing about the provider by its state. It is recovery only when it shows a model reply since the block: its cumulative output tokens are above the baseline. Without that the episode stays open, so it still gets its ping and its recovery wake. A replayed report, a flap that returns with the same class and `since`, and a relay restart all find the same episode and notify no one again.

The baseline is the highest output total among the `provider_blocked` reports the hub stored for that block (same agent run, class and `since`), read from the activity receipts. A refused request produces no model output, so a higher total means the model answered afterwards. The rule has these limits:

- Equal or lower output is not recovery, and neither is a block with no stored `provider_blocked` report to compare with. Such an episode closes at the agent's next turn report, as before.
- The hub stores a report only when the state or its detail changes. A reply that arrives while the agent stays in the same non-turn state is seen at its next change of state.
- Only output tokens count. Input, cached and reasoning totals are ignored.

Who is told:

- **Item lead**: one directed notice when the episode opens, naming the agent, provider, class, model, `since` and the recovery action. A blocked lead is not told about itself.
- **Owner**: one notice per episode, sent by the broker tick once the episode is 90 seconds old (`TAILTERM_PROVIDER_BLOCK_GRACE`, 1 to 3600 seconds, on the hub). A block that still needs action pings the owner on Discord (`refs.escalation=owner`) and names the fix and the host, for example "run /login in any Claude session on mini". That includes a block whose run ended meanwhile, because the cause is still there for the next agent on that host. An episode that cleared inside the grace still gets its notice, which says it has cleared and is recorded without a ping. The ping is promised within five minutes of the block; a grace above about 240 seconds breaks that.
- **Outage**: the one outage notice is the one ping, and it states the fix and the host first. When, at that point, two or more live agents of one runtime in the project are blocked and none of that runtime is working, the owner gets one notice that names the runtime instead of one per agent. Every waiting episode of that runtime joins it, and agents that block while it lasts join without another notice. The outage lasts only while that condition holds. It ends when an agent in it recovers (a new completed or running turn, or a report in another state that closed its episode with a model reply after the outage opened; an episode that had already closed when it joined the outage does not count), when a live agent of the runtime is working, or when no live agent of the runtime is blocked; a runtime prompt, an unknown probe or a crash without a model reply is not recovery, nor is a done agent's block dropped without a model reply (see Notifications): it does not end an outage while a live agent stays blocked, so a later block still joins it with no new notice, until that agent runs a later turn when it is not done, and an agent whose block was replaced by one of a new class or `since` is still blocked. A block that opens after it ended is judged afresh: it gets its own owner notice after the grace period, or one new outage notice if two or more are blocked and none is working again. The end is evaluated at the broker tick, so a block that opened during the outage but after the last tick before it ended is treated as a later one. Idle agents make no provider calls and count neither way. Agents of another runtime are handled separately.

The grace period is what makes the outage a single notice: reports arrive one agent at a time, so an immediate owner notice would always go out per agent before the outage is visible. Paused projects are swept too. The owner helper's block opens an episode like any agent's, because the owner may be away: the notice is posted by the hub and pinged by the bridge, and does not depend on the helper's session. The helper has no item, so no lead is told.

### Rotation and queue stalls

A provider-blocked agent makes no progress and holds no tool call. Four rules treat `provider_blocked` as idle:

- Runner rotation of a database handler: `handlerBusy.refusal` in `hub/internal/store/handler_rotation.go` (35b962e).
- The idle-entry stall check of `tt team queue list`: `classifyStallBlocker` in `hub/internal/store/team_queue_stall.go` (35b962e).
- The queue wait idle check: `queueWaitTeamIdle` in `hub/internal/store/team_queue_wait.go` (94133bf).
- Steward rotation: `stewardBusy` in `hub/internal/store/backlog_steward.go` (94133bf).

A live team lease or a pending tool call still refuses rotation.

A primary database handler that a `rate_limited` or `server_error` block has held for 20 minutes while it holds an obligation is replaced by the host runner: see [A provider-blocked primary](handler-rotation.md#a-provider-blocked-primary). A rotation's successor is ready only after it completes a turn with a model reply, so a successor whose first turn fails at its provider never takes the obligations: see [Successor readiness](handler-rotation.md#successor-readiness).

### Retry wakes

Bug `wi_ce7310979bc45479`, order #46760 (q1). Codex's failures in a 429 or overload burst lasted minutes and recovered on a later turn, but before this rule nothing started that turn: an acknowledged obligation waited 30 minutes for the broker's nudge, and the stopped handler waited for a person. Now the hub retries an open `rate_limited` or `server_error` episode itself.

- The broker tick's provider sweep (`ProviderBlockSweep`) checks every open episode of those classes on its agent's current run. Attempt 1 is due 5 minutes after the episode opened, attempt 2 at 15 minutes and attempt 3 at 30 minutes, by the hub's clock, and each at least 5 minutes after the one before. There are at most 3 attempts per episode and one per sweep, so attempts that fall due together after the hub was down still go 5 minutes apart. These bounds are fixed.
- An attempt is made only when the agent can take it: it is not closed, exited or retired, is online, is not a deployment agent, its project is open and active, its team is not held past its token estimate, and it holds an obligation that needs more than delivery. Otherwise nothing is written and the next sweep checks again; the attempt is not used up.
- The attempt is one row in `provider_block_retries` (episode and attempt number as the key) and one ordinary wake job on the agent's oldest such obligation, due at once. The relay leases it like any obligation wake, so its prompt lists every open obligation. No message and no obligation are created, so a rotation has nothing extra to move. The key makes a repeated or concurrent sweep, or a second hub process on the same database, queue one wake per attempt.
- The agent's next successful turn is recovery, as before: it closes the episode and no further attempt follows. A woken turn that fails again extends the same episode.
- `auth` keeps its wake after a restored login (below); `usage_limit` gets no retry. Neither gets a retry wake.

### After a login is restored

When an agent's open `auth` episode closes on recovery (a completed or running turn by an agent that is not done, or any report that shows a model reply since the block, as under Notifications), its login works again. A done agent's turn report without a model reply closes its episode with no wake and no notice: it is not evidence that the login works. In the same transaction the hub looks for other live agents of the same project, runtime and host that still have an open `auth` episode.

- Each one whose wake the relay can lease (online, not a deployment agent, not held past its token estimate, in a project that is not paused) gets one directed notice, "Provider login restored; resume your work". A directed notice queues an ordinary broker wake job, so the relay prompts the agent without anyone typing into its terminal.
- The owner gets one notice, "Provider login restored on a host", naming the host, the runtime, the recovered agent (worded as having completed a turn, also when the evidence was a model reply), the agents woken and the sessions that were not woken, with the reason (project paused, deployment agent, offline, or held past its token estimate). It pings only when a session still needs a prompt typed.
- Each episode is woken at most once (table `provider_block_recoveries`), so a woken agent's own recovery starts no second round. With no other blocked agent nothing is posted.
- Other classes, other hosts, other runtimes and other projects are not woken. Agents of another project on that host are woken when an agent of their own project recovers.
- If a woken agent's login is still expired (the recovery was on a different account), its wake fails, it stays blocked and its episode keeps the ping it already had.

The state shows as `activity=provider_blocked` with a `provider-blocked provider=… model=… class=… status=… since=…` line in `tt agents`, as `activity=provider_blocked provider=… class=…` on the member line of `tt team queue list`, and as "Provider blocked: Anthropic usage limit · claude-fable-5-1 · since 11:53" in TailOS.

## Recovery

Nothing logs in or switches a model. After an `auth` block clears on one agent, the hub queues an ordinary broker wake for the others of that runtime and host (see "After a login is restored"). A `rate_limited` or `server_error` block of an agent with open obligations gets up to three [retry wakes](#retry-wakes), and a primary database handler still blocked after 20 minutes is [replaced](handler-rotation.md#a-provider-blocked-primary). Every other recovery is manual.

| Class | Claude | Codex |
| --- | --- | --- |
| `usage_limit` | Wait for the reset, or run `/model` in the agent's terminal to pick a model with quota, or `/usage-credits`. | Answer the usage-limit prompt (see [runtime-prompts.md](runtime-prompts.md)). |
| `auth` | Run `/login` in any Claude session on the host; one login restores every Claude agent there. | Run `codex login` on the host, then prompt the agent again. |
| `rate_limited` | The hub retries an agent with open obligations at 5, 15 and 30 minutes. Otherwise, or if those fail, wait and prompt the agent again; run fewer agents in parallel if it repeats. | The same. |
| `server_error` | The hub retries as for `rate_limited`. Otherwise check the provider's status and prompt the agent again when it recovers. | The same. |

After the fix, prompt the agent again. Its next successful reply clears the block. For `auth`, one agent's successful reply is enough: the hub wakes the others, and a manual prompt is still needed only for the sessions the owner notice names and for an agent with nothing recovered beside it.

Automatic model fallback is not built. Whether the harness may switch a blocked agent to another model, to which model, and who may apply it, is an owner decision (#19856); until the owner decides, recovery is manual.

## Compatibility

- A relay newer than the hub: the hub answers 400 to the new state once. The relay remembers that for the agent's run and reports the legacy form instead, which for Claude is `idle` or `finished_silent` with the turn-end reason and for Codex is `unknown` with reason `provider_blocked: <class>`. Later transitions are reported as usual.
- A hub newer than the relay keeps accepting the legacy form. Handler arm limits accept both ([handler-ab.md](handler-ab.md)).
- The hub adds four tables, `provider_block_episodes`, `provider_outages`, `provider_block_recoveries` and `provider_block_retries`. No existing table is rebuilt. A hub that already has `provider_block_episodes` gains its `dropped` column (`INTEGER NOT NULL DEFAULT 0`, set when a done agent's turn closes an episode without a model reply) by `ALTER TABLE ADD COLUMN`; episodes closed before it count as not dropped.
- The ping and the wake need no relay or bridge change: the bridge already pings a notice with `refs.escalation=owner`, and the relay already leases wake jobs.

## Checks

From `hub/`: `go test ./cmd/tt -run 'TestActivityProviderBlocked|TestActivityCLI'` (including `TestActivityProviderBlockedSessionEnd`, `TestActivityProviderBlockedWokenDoneAgent`, `TestActivityProviderBlockedReportKey`, `TestActivityProviderBlockedCodexRetryLimit` and `TestActivityProviderBlockedCodexServerOverloaded`), `go test ./internal/store -run 'TestProviderBlock|TestHandlerArmLimit'` (including `TestProviderBlockDoneAgentRecovery`, `TestProviderBlockOutageIgnoresDroppedDoneBlock`, `TestProviderBlockedHandlerRotation`, `TestProviderBlockedQueueStallIdleEntry`, `TestProviderBlockRetryWakeAfterBackoff`, `TestProviderBlockRetryBounds` and `TestProviderBlockRetryNoDuplicate`), `go test ./internal/store -run 'TestHandlerRotationSuccessorReadiness|TestHandlerRotationDueProviderBlocked|TestHandlerRotationProviderBlocked'`, `go test ./cmd/tt -run 'TestHandlerRotationStartupProviderFailure|TestHandlerRotationProviderBlockedFallback'`, `go test ./internal/store -run 'TestQueueWaitProviderBlockedMemberIsIdle|TestStewardRotationProviderBlockedIsIdle'` and `go test ./internal/broker -run 'TestBrokerProviderBlockSweep|TestBrokerProviderRecoveryWake'` and `go test ./internal/bridge -run TestProviderBlockPingsTheOwner`. From the repository root: `node --test tests/activity-format.test.js` and `node tests/activity-browser.mjs`. The transcripts under `hub/cmd/tt/testdata/provider-blocked/` are written by hand with the observed error texts; the key in the Codex fixture is made up. No check reads a real transcript or touches the live hub, relay or Discord channel: the bridge test uses a fake Discord. The live Discord check is done once after release, in the daytime with the owner helper present, in a disposable project with a synthetic `provider_blocked` report, including one sent as an owner helper.
