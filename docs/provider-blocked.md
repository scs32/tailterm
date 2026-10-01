# Provider-blocked agents

Bug `wi_72f41bd375032cf0` revision 1, owner order #11854, plan request #19860, builder assignment #19880.

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

- "Repeated" is two failed turns in a row with no model output between them. `TAILTERM_ACTIVITY_PROVIDER_REPEAT` (2 to 20) on the relay host changes it. One transient failure keeps today's `idle` or `finished_silent` with its turn-end reason.
- A different class starts a new block with a new `since`.
- Only evidence of success clears a block: for Claude an ordinary assistant record, for Codex model output or a `task_complete` without an error. A new prompt alone does not.
- Other API errors, such as an invalid request, are not provider blocks.
- A Codex usage limit shows as a modal prompt and stays with [runtime-prompts.md](runtime-prompts.md) (`codex_usage_limit`). `runtime_prompt` takes precedence over `provider_blocked`; `crashed` does too. `provider_blocked` takes precedence over every other state, including `stuck` and a `needs_input` status.
- Only the Codex 401 record was observed. The 403, 429 and 5xx forms follow the same `unexpected status NNN` shape and are covered by synthetic fixtures; a Codex release that words them differently will not be detected until the pattern is extended.

## Notifications

A block from its first report until the agent reports another state is one episode. A replayed report, a flap that returns with the same class and `since`, and a relay restart all find the same episode and notify no one again.

- **Item lead**: one directed notice when the episode opens, naming the agent, provider, class, model, `since` and the recovery action. A blocked lead is not told about itself.
- **Owner**: one notice per episode, sent by the broker tick once the episode is 90 seconds old (`TAILTERM_PROVIDER_BLOCK_GRACE`, 1 to 3600 seconds, on the hub). An episode that cleared inside that time still gets its notice, which says it has cleared.
- **Outage**: when, at that point, two or more live agents of one runtime in the project are blocked and none of that runtime is working, the owner gets one notice that names the runtime instead of one per agent. Every waiting episode of that runtime joins it, and agents that block while it lasts join without another notice. The outage lasts only while that condition holds. It ends when an agent in it recovers (a new completed or running turn), when a live agent of the runtime is working, or when no live agent of the runtime is blocked; a runtime prompt, an unknown probe or a crash is not recovery. A block that opens after it ended is judged afresh: it gets its own owner notice after the grace period, or one new outage notice if two or more are blocked and none is working again. The end is evaluated at the broker tick, so a block that opened during the outage but after the last tick before it ended is treated as a later one. Idle agents make no provider calls and count neither way. Agents of another runtime are handled separately.

The grace period is what makes the outage a single notice: reports arrive one agent at a time, so an immediate owner notice would always go out per agent before the outage is visible. Paused projects are swept too. The owner helper is the owner's own session and opens no episode.

The state shows as `activity=provider_blocked` with a `provider-blocked provider=… model=… class=… status=… since=…` line in `tt agents`, as `activity=provider_blocked provider=… class=…` on the member line of `tt team queue list`, and as "Provider blocked: Anthropic usage limit · claude-fable-5-1 · since 11:53" in TailOS.

## Recovery

Detection only notifies. Nothing types into an agent's terminal, switches a model or logs in.

| Class | Claude | Codex |
| --- | --- | --- |
| `usage_limit` | Wait for the reset, or run `/model` in the agent's terminal to pick a model with quota, or `/usage-credits`. | Answer the usage-limit prompt (see [runtime-prompts.md](runtime-prompts.md)). |
| `auth` | Run `/login` in the agent's terminal. | Run `codex login` on the host, then resume the agent. |
| `rate_limited` | Wait and prompt the agent again; run fewer agents in parallel if it repeats. | The same. |
| `server_error` | Check the provider's status and prompt the agent again when it recovers. | The same. |

After the fix, prompt the agent again. Its next successful reply clears the block.

Automatic model fallback is not built. Whether the harness may switch a blocked agent to another model, to which model, and who may apply it, is an owner decision (#19856); until the owner decides, recovery is manual.

## Compatibility

- A relay newer than the hub: the hub answers 400 to the new state once. The relay remembers that for the agent's run and reports the legacy form instead, which for Claude is `idle` or `finished_silent` with the turn-end reason and for Codex is `unknown` with reason `provider_blocked: <class>`. Later transitions are reported as usual.
- A hub newer than the relay keeps accepting the legacy form. Handler arm limits accept both ([handler-ab.md](handler-ab.md)).
- The hub adds two tables, `provider_block_episodes` and `provider_outages`. No existing table is rebuilt.

## Checks

From `hub/`: `go test ./cmd/tt -run 'TestActivityProviderBlocked|TestActivityCLI'`, `go test ./internal/store -run 'TestProviderBlock|TestHandlerArmLimit'` and `go test ./internal/broker -run TestBrokerProviderBlockSweep`. From the repository root: `node --test tests/activity-format.test.js` and `node tests/activity-browser.mjs`. The transcripts under `hub/cmd/tt/testdata/provider-blocked/` are written by hand with the observed error texts; the key in the Codex fixture is made up. No check reads a real transcript or touches the live hub or relay.
