# Usage accounting

Feature `wi_ca6a62f5e74114b4`, revision 2, owner order #12620; builder assignment #12732, renewed Start #12737 and handler release #12740. This feature measures metered model requests, rather than whole agent activations. Existing activity totals and lifecycle transitions retain their meanings.

## Read a report

```
tt usage --project PROJECT_ID
tt usage --project PROJECT_ID --item ITEM_ID --json
tt usage --project PROJECT_ID --from 2026-09-27T00:00:00Z --to 2026-09-28T00:00:00Z --json
tt usage prices get --project PROJECT_ID
```

Request timestamps use UTC and half-open intervals: From is inclusive, To is exclusive. Projects includes an optional Usage disclosure, available for closed projects too. Expand an item for phases, roles, models and phase/role combinations. Top phases rank by comparable estimated cost, or by reported tokens without prices, with deterministic ties and currencies kept separate. Project overhead remains a separate row. Usage reads do not hold up the core Projects view; older hubs show an unsupported message. Cached reads remain available offline, with price writes disabled.

An item with no measured requests says **not measured**. Missing token classes remain unavailable, rather than measured zero. Reports distinguish unique participating requests from allocated turn equivalents. A request serving two items contributes one participating request and one-half allocated turn to each. Project totals count the request once. Average context is allocated uncached input plus cached input divided by allocated turns; it is unavailable when a contributing request lacks either input class. Cache-write tokens are reported separately. Cached-input share uses cached divided by uncached-plus-cached input.

JSON quantities and costs are exact rational strings, such as `11/2`; display formatting happens only at the UI edge and in `tt usage` text, which prints token counts in units (`950`, `12.1k`, `6.71M`), the average context in tokens, the cached-input share as a percentage and an absent token class as `unavailable`. Reports include measured-class counts, source coverage by enrolled run and explicit partial states. Historical work completed before collection cannot be reconstructed from lifetime totals.

## Owner prices

The feature ships with an empty price table. No provider prices are built in. Use Prices in the disclosure, or replace the table with a JSON array:

```
tt usage prices set --project PROJECT_ID --file prices.json --expected-revision 0 --request-id stable-edit-key
```

Each row has `runtime`, exact `model`, three-letter `currency`, UTC `effectiveAt` and a `rates` object. Rates are nonnegative decimal strings per million tokens. Supported classes are `input`, `cached`, `cacheWrite`, `output` and `reasoning`. Blank or missing rates remain unpriced. A synthetic example (not a provider quote):

```json
[{"runtime":"codex","model":"synthetic-model","currency":"USD","effectiveAt":"2026-09-27T00:00:00Z","rates":{"input":"2","cached":"0.2"}}]
```

The latest matching runtime/model row effective at the request timestamp applies. Missing prices or token classes produce an incomplete estimated subtotal, never a complete zero cost. Currencies remain separate. Changes use revision checks and stable retry identities; after an uncertain response, repeat the same payload/key. A conflict requires reloading the table before editing again.

## Collection and attribution

The host relay fans usage parsing out from the existing bounded transcript append pass. Codex last-request usage must reconcile with cumulative checkpoints; repeated checkpoints do not add requests. Unexplained first lifetime totals, resets and mismatched session identities produce coverage gaps. Codex input and output include cached input and reasoning respectively, so normalization subtracts those subsets only when available. The actual cache-write field is `cache_write_input_tokens`. A reported zero supports the usual input split; a nonzero value retains the cache-write quantity and raw inclusive input, but leaves normalized uncached input unavailable because its overlap has not been established. Claude assistant message IDs identify requests; streaming usage and later output update that same request monotonically. `output_tokens_details.thinking_tokens` is retained as reasoning and subtracted from inclusive output. If Claude thinking details are absent or null, reported output stays inclusive and reasoning remains unavailable; an explicit normalization gap and incomplete pricing preserve that uncertainty. No zero reasoning is inferred or charged in addition. Later reported thinking can replace that request’s inclusive quantity with the disjoint split, without adding another request. Cache read/write stay separate. Raw numeric classes and a source digest remain auditable; transcript text and tool arguments never enter the ledger.

Successful CLI ack/progress and authored post/send operations record a small private local journal. Inbox reads alone do not establish handling. Activation boundaries collect handled evidence, including the opening model request preceding the first acknowledgement. The hub resolves actual Board links and exact refs, retains provenance, splits several valid items evenly, and sends unattributable work to project overhead. Persistent handler/project lead/deployment roles do not inherit an unrelated bound item. Worker roles may use their exact admitted item binding when no handled item exists.

Each projection has one phase: intake/planning, build, numbered review round (or explicitly unavailable round), corrections, verification, handler bookkeeping, or hand-offs. Handler bookkeeping takes precedence for handlers. Otherwise a request is classified by what its agent was doing when it made the request, the **open-order rule**: the governing order is the assign, review or request addressed to that agent whose obligation was open at the request time, from the moment it was posted until the agent's typed result, a decline, a withdrawal or a reassignment closed it. Everything inside that window belongs to the order, including activations that only exchanged messages with the database handler or handled no Board message at all. An agent's own messages never govern: a builder's Start request to the handler does not change the builder's phase. When several orders are open, an assign or review outranks a plain request; then the latest wins, and message sequence breaks ties.

| Role and governing order | Phase |
| --- | --- |
| database handler | handler bookkeeping |
| no open order | hand-offs |
| focused review, or any order held by a verifier | verification |
| review, or any order held by a reviewer | review round N from retained native review state, else round unavailable |
| any order held by a planner, or refs `phase` of `planning` or `intake` | intake and planning |
| assign after a review round whose result had blockers | corrections |
| assign | build |
| plain request (lead, builder, other roles) | hand-offs |

When no obligation window covers the request, the handled order addressed to the agent is used instead: the opening request inherits the activation's first handled order, later requests the latest at or before the request. This covers orders older than obligations and a host clock slightly behind the hub, where the opening request precedes the order it acknowledged. An order whose window has already ended is not revived by handled evidence.

Phases are recomputed on read. Ingest stores the phase it computed as a snapshot, and every report applies the current rule to each recorded request, so requests recorded under an earlier rule move with it. A report never rewrites or drops a stored row, and request counts, token totals and item attribution are unchanged; only the phase label can differ from the stored snapshot. Anything reading `usage_turns.projection` directly sees the ingest-time value.

Known limit: an order that was never closed (for example a result posted without a reply to its order, in older history) stays open until its team closes, so the agent's later requests keep that order's phase.

Authoritative Planned member role text is normalized to planner/builder/reviewer/verifier while retaining its original source. Bounded ancestry stops without discarding already validated item links; a bad evidence entry cannot erase other valid links or a worker’s exact admitted binding.

A batch is frozen to private disk before upload, replayed unchanged until its matching receipt, and revision-checked by the ledger. Enrolled run provenance and request/revision/receipt records survive lifecycle closure and binding archival. Retirement freezes any pending outbox without rescanning archived transcripts or polling archived agents. Capability discovery is cached per host. Collection shares the relay request budget after delivery, adds no prompts or agent turns, and caps append records, batch bytes and local outbox size. Capacity, parse and partial-tail gaps remain explicit. A fresh activation with a reconciled first request records measured coverage. Historical run gaps stay in run coverage; they do not relabel later fully measured requests. Permanent 400/404/409 upload rejection preserves the immutable numeric batch and status in a private local quarantine with explicit partial coverage. It is never called an uploaded receipt. Later live requests may continue; frozen delivery rotates past errors or throttle, and drained cursor files are removed. Quarantine files remain for diagnosis/recovery; a rejected batch is not included in hub totals until it is successfully ingested. No owner Claude session is enrolled.

## Time

Feature `wi_95335cabc56442f5`, revision 1, owner order #13091; builder assignment #19086, Start #19088 and handler release #19100. Next to an item's tokens, the report says where its wall time went, what the waiting was for, and how many model requests only re-checked the inbox. It reads the same transcripts as the token ledger and adds no agent turn.

```
tt usage --project PROJECT_ID --item ITEM_ID
tt usage --project PROJECT_ID --item ITEM_ID --json | jq '.items[0].time'
```

The text form prints one `time:` line per item (wall time, the model, tool and waiting shares, poll requests), a `timeline:` line, one `time agent`, `time phase` and `time role` line each, and the longest waits with their Board message number. In Projects, the Usage disclosure shows the same inside each item: wall time and shares, the team timeline, the top waits, and a `Time:` line in each phase and role row. An item with no spans says **Time not measured**. A hub that does not report time (`timeVersion` absent) is shown as before.

### What is measured

All times are transcript record timestamps, UTC, as half-open intervals. Records are not always written in timestamp order; each keeps its own time. A turn carries the gap `record timestamps out of order; clamped` only where that changed a measured number: an output stamped before its call counts no time, and a turn cannot end before its last call, output or request.

- **Turn.** Codex: `task_started` to `task_complete`. Claude: a real user prompt to the turn end (`end_turn`, `result`, `turn_duration` or an API error). A turn with no end record closes at the next turn start or at retirement, at the last record seen, and says so in its gap.
- **Tool time.** From a tool call record to its output record. Overlapping or touching calls are merged into their exact union; a gap is never bridged. A call with no output closes at the turn end.
- **Waiting.** Everything outside turns, plus a blocking inbox wait inside a turn (`tt wait`, `tt inbox --wait`). Codex `sleep` and shell `sleep N` are tool time.
- **Model time.** The turn minus tool, waiting and unmeasured intervals.
- **Unmeasured.** A tool call that runs a blocking inbox wait together with other work (`tt inbox --wait 9m && go test ./...`, or a wait piped into a program) cannot be split by timestamps. It is reported as unmeasured, never as tool or waiting. So is the remainder of a turn with more than 64 chunks of intervals.

A tool call is classified by the commands it executes, never by a pattern over its text. The relay decodes the call into shell strings (a Claude `Bash` command, a Codex `function_call` `cmd`/`command`, or each `tools.exec_command({cmd: "…"})` in a Codex `exec` snippet), splits each into simple commands, skips `VAR=value` words and the wrappers `env`, `command`, `exec`, `nohup`, `time` and `timeout N`, and looks one level into `bash|sh|zsh -c`. Only a command word whose basename is exactly `tt` counts. Quoted text is only ever an argument, so `echo tt wait` is ordinary tool time. Anything it cannot decode (command substitution, a heredoc, a `cmd` that is not a plain string literal, another `tools.NAME(` call) stays ordinary tool time. A wait is still waiting beside `cd` and other `tt` commands, when its own output only passes through pure output filters (`head`, `tail`, `cut`, `cat`, `grep`, `awk`, `jq`, `wc`, `sed -n`, with or without `2>&1`), and with a trailing `|| true`: `tt inbox --wait 9m 2>&1 | tail -5` is a wait. `tee`, a filter that redirects to a file, a filter on another command's pipe, and any other command joined with `&&` or `;` make the call mixed. The same filters after a non-blocking `tt inbox` leave it an inbox check. Only the class is kept: command text never enters the cursor, the upload or the database.

### Requests, segments and polls

Each metered model request is marked at the timestamp the token ledger gives it. A request's **segment** is the time after the previous request (or the turn start) up to its mark; the last one runs to the turn end. A segment is attributed when it is recorded, by the rule its request's tokens get at ingest: same item shares, phase, review round and role, resolved at the same timestamp. Token phases are then recomputed on every read (see the open-order rule above); a segment keeps the phase stored when it was recorded. A request and the time in its segment therefore agree when both were recorded under the current rule, and an order that opens in the middle of a turn moves later time to the new phase just as it moves later tokens. A segment recorded under an earlier rule keeps that rule's phase, so for that period an item's tokens can show under `build` while its time shows under `hand-offs`.

A request is a **poll** when every tool call it issued is an inbox command (blocking or not) and the next request in the turn that issued any tool call is the same. A check that leads to other work is not a poll. `polls` counts them; `pollMs` is the model time inside their segments.

### Collection

A completed turn is uploaded once, as chunks of at most 32 intervals and segment pieces that together cover it exactly. Nothing is merged to fit; an interval crossing a cut is split there, and every segment piece names its own request, so the result does not depend on where the turn was cut. An open turn is absent from the report until it ends. Chunks ride the usage batch, so the frozen outbox, receipts, replay, retirement freeze and quarantine apply to them unchanged. A stored chunk is immutable: an identical replay is a no-op and a different payload under the same id is a conflict. The relay sends chunks only to a hub that advertises `usage.time`; until then they stay in the local outbox (at most 256, with explicit partial coverage beyond that), and a frozen run keeps its state file until its chunks have a receipt. If a hub acknowledges a batch without a span count (it was rolled back to a build without span storage after the capability was cached), the batch's token turns are done, its chunks stay in the outbox, and no chunk is sent again until the capability is read anew.

### The report

`time` is absent for an item with no spans. Durations are exact milliseconds as strings, a rational such as `60000` or `3001/2`. The report reads the spans of its own project's agents only, and compares each wait only with what the waiting agent wrote and the owner requests linked to the item, so its cost does not grow with the rest of the Board.

- **Window.** From the earliest to the latest segment attributed to the item, clipped to `--from`/`--to`. `wallMs` is its length.
- **Per agent.** Model and tool time from its segments attributed to the item; the rest of the window is waiting. Model, tool, waiting and unmeasured sum to the window. A request that served several items gives each its share of model and tool time, and the remainder is waiting for that item. So a shared handler's work on other items is waiting here, as in the owner's own measurement.
- **Per phase and role.** The same keys as tokens. Model and tool time take the phase and role of their segment, and so does a blocking wait inside it. Waiting outside the agent's item work takes the phase and role of its latest item segment that ended before it, or of its first one. Each breakdown sums to the agents' total.
- **Team timeline.** A sweep over the window: some model working, only tools running, nobody active. A stretch some agent could not split is left out as `unmeasuredMs`; the four sum to the window.
- **Waits.** Each waiting stretch is split wherever something relevant opened or closed and labelled by the first rule that matches: `owner` (an owner obligation or decision request the agent authored), `handler` (the agent's request to a database handler: save, read-back, Start release), `teammate` (its request to anyone else), `owner` again (an owner request or decision linked to the item by any author), otherwise `unknown`. Open means the recorded interval: an obligation from creation to close, a decision from its request to its answer. Current state is never consulted, so the report is the same after a hub restart and long after the answer. `waits` lists the five largest by cause and Board message, with the agents that waited; `causes` totals all of them.

Known limits: a long open turn is invisible until it ends; a transitive wait (a builder idle while its lead waits for the owner) is `owner` only through the item link, otherwise `unknown`; work done before collection started is not reconstructed.

## Token estimate and budget

Feature `wi_899863352c81e3b0`, revision 1, owner order #28848; builder assignment #28954. Phase 1 of the token guardrails: each work item can carry a token estimate, shown against what the item actually used. Phase 2 adds a [warning](#warning) when an item passes a multiple of its estimate. Neither holds nor pauses anything; phase 3, the [token budget](#token-budget), does.

### Set an estimate

```
tt work-items update --revision N --request-id KEY --estimate-tokens 12000000 --estimate-basis "Small, 2 paths, median of 8 Small items" WI_ID
tt work-items update --revision N --request-id KEY --estimate-tokens 0 WI_ID
```

An estimate is a token count from 1 to 1,000,000,000,000 with a one-line basis of at most 240 bytes; `0` clears it and takes no basis. Only the project's database handler, or the owner with no agent identity, may save one: any other agent gets "only the database handler sets an estimate", and the backlog steward its usual write refusal. The steward proposes an estimate with every filing and ranking, and the handler records it ([backlog-steward.md](backlog-steward.md#proposals)).

`--revision` is the item's current revision, and **the save does not change it**. The item keeps its revision, scope revision, updated time and updater, gets no new revision in `tt work-items revisions`, and no queue entry is re-synced. A queued, running or accepted entry, a frozen verification plan, a bound agent's work context and a pending release are therefore unaffected. For the same reason an estimate is saved alone: combining the flags with `--title`, `--body-file`, `--status`, `--priority`, a completion report or queue acceptance is refused. The legacy PATCH update cannot set one.

Each accepted save appends one row to the estimate history (`work_item_estimates`) and returns a receipt whose result revision is that unchanged revision. Repeating a request with the same key and payload, or reading it with `tt work-items receipt --request-id KEY WI_ID`, returns the estimate **that request** saved, even after a later save or clear replaced it on the item. A project event `work_item_updated` with `fields: ["estimate"]` and the unchanged revision announces the save.

### Read the budget

Work item get and list, every team queue listing, entry read and action result, and each item of the usage report carry a `budget`:

```json
{"estimate":{"tokens":12000000,"basis":"Small, 2 paths, median of 8 Small items","setAt":"2026-10-07T16:00:00Z","setBy":{"agentId":"agt_…","node":"…","user":"owner"}},
 "actualTokens":"15300000","actualState":"measured","ratio":"51/40"}
```

- `estimate` is absent when the item has none ("no estimate").
- `actualTokens` is the item's **lifetime** attributed tokens: the sum of every reported token class over all of its runs, open and closed, as an exact rational string. It is the `tokens` figure of an unfiltered `tt usage --item ID`. `--from`/`--to`, and From/To in the Usage disclosure, change the report rows and never the budget.
- `actualState` is `measured`, `partial` or `not measured`, the state that unfiltered report gives the item. It is `partial` when any contributing request was incomplete, carried a normalization gap or lacked a token class. A partial actual is a **lower bound**, and so is its ratio.
- `ratio` is actual divided by estimate, absent without an estimate or when not measured.
- `team` is present only while the item has a running queue entry: `{"entryId":"tqe_…","actualTokens":"3100000","actualState":"measured","ratio":"31/120"}`. It is what that entry's team has spent on the item, the figure the [warning](#warning) compares, with the same state and ratio rules. The lifetime fields above keep their meaning and values beside it.
- A missing `budget` key means an older hub. Project overhead has no budget.

Text forms say the same in one line. `tt work-items get` prints `Budget: estimate 12.00M · lifetime actual 15.30M · 1.28×` and the basis with who set it; `tt team queue list` and `tt usage` print a `budget:` line under each entry and item. A partial actual reads `lifetime actual at least 15.30M (partial) · at least 1.28×`, never a bare ratio. Without usage the line ends `lifetime actual not measured`. While the item has a running queue entry whose team has measured tokens on it, the line continues with that team's figure, ` · this team 3.10M · 0.26×` (`this team at least 3.10M · at least 0.26×` when partial); with no running entry the line is unchanged. The Usage disclosure shows the line in each item's summary and, inside the item, with the basis. The queue entry's older `tokens` field is unchanged: it sums the activity of agents currently bound to the item, not the lifetime ledger.

### Calibrate from history

```
tt usage --calibration --project PROJECT_ID
tt usage --calibration --project PROJECT_ID --json
```

One line per done item, by item sequence: lane (the template of the item's latest team queue entry, `small` or `planned`, or `none` if it was never queued), owned-path count, team shape (`plan-review` or `plan-only`), lifetime actual with its state, and the saved estimate and ratio. The read follows the done items and the queue history to their last pages, so no item is cut off at a page limit. Usage before collection began (2026-09-27) is not in the ledger, so older items read "not measured".

### Storage

`work_items` gains six `estimate_*` columns. Budgets do not scan the ledger: each stored request also writes its per-item shares to `usage_item_shares` (reported tokens, share denominator, partial flag), and a budget is one indexed read by item. Share rows are derived data. At every open the hub fills them for any stored request that has none at its current revision, which covers requests stored before the table existed and requests an older binary wrote or revised after a rollback. Both tables and the columns are additive, and an older binary ignores them. The audit export's `workItems` rows include the estimate columns; the estimate history table is not exported yet.

### Warning

Feature `wi_3228104e700006c5`, revision 1, owner order #29142; builder assignment #29186. Phase 2 of the token guardrails. The compared figure was corrected by bug `wi_12d5c1ef9a73ceae`, owner order #29774, builder assignment #29812.

**Rule.** When a usage upload takes the tokens that the team of an item's running queue entry has spent on the item past the project's warning level times the item's saved estimate, the hub posts a notice. The level is `1.5` unless the project sets its own. "Past" is strictly greater, compared exactly: 1,500,000 tokens against an estimate of 1,000,000 has not passed 1.5, and one more token has. A partial actual is a lower bound, so passing the level on it is a real crossing; the notice then says "at least". An item with no estimate never warns and stays listed as "no estimate" in `tt usage`, `tt work-items get` and the queue list.

**The compared figure is the team's, not the item's lifetime.** An estimate is for one team's run at the item, so it is compared with that team's tokens: the budget's `team.actualTokens`, never its lifetime `actualTokens`. The phase 3 pause (`wi_1d2fbde2c149989e`) compares this same team figure.

- The entry is the item's running queue entry (state `running`, not released). An item has at most one. An item with no running entry has no team figure and never warns, however large its lifetime total.
- The team is the runs bound to the item whose agent has no project role and whose binding is not older than the entry. A database handler, the backlog steward and the owner helper never count, bound or not. An agent with no binding to the item never counts. Replacement leads and agents added during the entry count.
- Counting starts at the entry's admission, which is the entry's creation time. A binding carries its agent's creation time, so the agents of an earlier entry stay excluded even if they work on the item again, and every turn of a counted run is after admission: the run did not exist before it. The two instants are compared as times.
- So intake and refinement by the steward, saves and confirmations by a handler, orders by the owner helper and the work of earlier entries are in the lifetime figure only. The lifetime figure remains the budget's headline and is shown beside the team figure wherever a warning appears.

**Nothing is paused or held.** The warning is a message and nothing else: no turn, team, queue entry or admission changes, and nobody owes a reply.

**Recipients.** One directed notice to the item's running lead (the lead of its running team, not the project lead) and one to the project's owner helper. With no running lead, only the owner helper gets one and the text says the entry is running without a running lead. If the same agent is both, it gets one. If there is neither, or the project is closed, nothing is posted or recorded, and the next eligible upload looks again. A retired or exited owner helper still gets the notice in its inbox; the notice does not change its status and no wake is leased to it.

The notice is of kind `notice`, with the subject "An item has passed its token estimate warning level", sent by `system` / `usage-warning`:

```
Item wi_… (Title): the team of queue entry tqe_… has used 18300000 tokens on it against an estimate of 12000000: 1.53 times, past the warning level of 1.5. The item's lifetime total is 41200000 tokens, which includes work before this entry. Running team: lead lead-…. Nothing is paused or held. No reply is needed.
```

"at least" precedes each figure that is partial, by its own state. Its refs carry `item`, `entry`, `estimateTokens`, `threshold`, the compared team figure as `actualTokens`, `actualState` and `ratio`, the item's lifetime figure as `lifetimeTokens` and `lifetimeState` (quantities are exact rational strings), and `lead` when the entry has a running lead. The item is named in refs rather than linked, so the notice is not a work message. It carries a delivery-only obligation: the broker wakes a live recipient once, the obligation closes when the recipient fetches what it owes, and it never becomes overdue.

**Once per entry and estimate value.** An item warns once for each queue entry and estimate value, recorded in `usage_entry_warnings`. Repeated uploads, replayed batches and further usage post nothing more. A later entry on the same item is a new team: it can warn again, on its own tokens, whether or not an earlier entry warned at the same estimate. Saving a different estimate re-arms the warning for the running entry; the save itself posts nothing. The check runs when the next **new or revised** turn attributed to the item is stored: a batch whose turns are all unchanged checks nothing, so a team that is already past a newly saved estimate warns at its next new or revised turn. A lowered estimate is also a new value and can warn. Within one entry, returning to a value that already warned does not warn again, and neither does changing the level.

**Set the level.**

```
tt usage warning get --project PROJECT_ID
tt usage warning get --project PROJECT_ID --json
tt usage warning set --project PROJECT_ID --threshold 2
```

The level is a plain decimal from 1 to 100 with at most three decimals. The owner sets it; an agent may only if it is the project's owner helper, and any other agent gets "only the owner helper sets the warning threshold". `get` prints the level, whether it is the default, and one line per warning posted, with the Board numbers of its notices:

```
Warning level: 1.5× estimate (default)
  wi_…: estimate 12.00M · lifetime actual 18.36M · 1.53× · level 1.5× · lead #29301 · owner helper #29302 · 2026-10-08T04:00:00Z
  wi_…: entry tqe_… · estimate 12.00M · team actual 18.36M · 1.53× · lifetime 41.20M · level 1.5× · lead #29901 · owner helper #29902 · 2026-10-08T16:00:00Z
```

A line with `entry` is a warning on that entry's team: `team actual` and the ratio are the compared figure and `lifetime` is the item's total at that moment. A line without one was recorded before entries were compared, when the item's lifetime figure was itself the compared one; it keeps its `lifetime actual` label. In JSON a warning's `actualTokens`, `actualState` and `ratio` are the compared figure, and a warning on an entry also has `entryId`, `lifetimeTokens` and `lifetimeState`.

Over HTTP it is `GET` and `PUT /v1/tasks/{id}/usage/warning` with `{"threshold":"2"}`. A saved level of `1.5` reads as set, not as the default.

A warning compares only an estimate saved on the item: an item admitted on a [lane default](#lane-defaults) never warns. The team is counted from the entry's admission, not its enqueue ([Hold past 3 times](#hold-past-3-times)).

**A warning never blocks usage.** The check runs inside the upload's transaction, after its turns are stored, with each item's warning in its own savepoint. If anything in a warning fails (a read, a post, the record), all of that item's warning is rolled back: no message, receipt, obligation, wake job or record remains. The upload still commits, and the next new or revised turn for the item tries again. Such a failure is not reported anywhere. Only a cancelled request or a failing savepoint statement fails the upload.

**Storage.** `usage_entry_warnings` holds one row per item, queue entry and estimate value: the level, the team figure and the lifetime figure with their states at the crossing, both recipients and their message numbers, and the time. `usage_budget_warnings` holds the warnings recorded before entries were compared, one row per item and estimate value with the lifetime figure that was compared then; it is read for the list and never written, and no row of it is copied, changed or deleted. `usage_warning_settings` holds one row per project that set a level. All three are additive, and an older binary ignores the ones it does not know. An entry that was already running at the upgrade has no row in the new table, so it warns once if its team is past the level at its next new or revised turn, even if the item warned before under the old rule.

## Token budget

Feature `wi_1d2fbde2c149989e`, revision 4, owner order #30088 with clarifications #30095 and #30120 and path decision #30165; builder assignment #30197. Phase 3 of the token guardrails. It delivers `wi_eed9581348ad7f40` (team tokens counted from admission) as part of the hold. Two things are added, and neither interrupts a turn, closes a session or refuses a write:

- **Admission.** A queued team is admitted only when the remaining budget covers its estimate plus a reserve. Otherwise it stays queued with a stated reason, so no team is cut off mid-item. A running team is never touched by the budget.
- **Hold.** A running team that has spent more than 3 times its item's **saved** estimate is held: no new turn is started for it, and the owner helper is asked to continue or stop.

With no budget row the queue admits exactly as before, and `tt usage budget get` prints `Token budget: not configured`. The hold does not depend on a budget row.

### Budget rows

```
tt usage budget set --runtime claude --window five_hour --allowance 200000000 --reserve 10
tt usage budget set --runtime claude --window seven_day --allowance 900000000 --reserve 10
tt usage budget get [--host H]
tt usage budget clear --runtime claude --window five_hour
```

A row is per project, runtime (`codex` or `claude`) and provider window (`five_hour` or `seven_day`).

- `--allowance` is required: **the tokens that 100 percent of that window represents.** A provider percentage is converted with it, so the estimate is always compared in tokens. There is no percent-only mode.
- `--reserve` is the percent of the allowance, 0 to 90, kept back from admission.
- `--reset-at` is an optional reset instant (RFC 3339). It is used only when no provider reading gives one, and rolls forward by whole windows.
- `--stale` is how old a provider reading may be and still decide, 15 minutes unless set. Keep it above the clock difference you expect between the agent host and the hub.

The owner, the owner helper and a database handler of the project may set or clear a row; any other agent is refused with "only the owner, the owner helper or a database handler sets the token budget". A missing or zero allowance, an unknown runtime or window, a reserve above 90 and a malformed reset time are refused with the value named. `get` prints each row with the source in use for a host (this host unless `--host` names another), the remaining tokens, the reset time and, when the provider reading is not the source, why. Over HTTP it is `GET`, `PUT` and `DELETE /v1/tasks/{id}/usage/budget`.

### Sources

For each row, admission uses the first source that applies. Remaining is before the reserve is taken off.

1. **Provider reading.** The reading reported for the entry's host is `ok`, not older than the staleness bound, and its reset is still ahead. Remaining is the allowance times the unused percentage; the reset is the reading's.
2. **Allowance, reset from the reading.** The reading is `ok` but stale, or its reset has passed. Remaining is the allowance minus the project's reported tokens of that runtime since the window start. The window start is the reading's reset minus the window length while that reset is ahead; once it has passed it is that reset, or the window length ago if that is later. A passed reset is never counted as zero use.
3. **Allowance, owner reset.** There is no `ok` reading (never reported, or invalidated) and the row has `--reset-at`. The window start is the latest reset that is not after now.
4. **None.** The entry is held. Unknown is never treated as plenty.

The fit is `remaining − allowance × reserve ÷ 100 ≥ estimate`, and an entry must fit **every** row of the project. The check is recomputed at every claim and every list, so a reset, a new reading, a changed row or changed lane defaults admits a held entry with no other action.

### Admission reasons

A held entry shows one of these as its `blockReason` in `tt team queue list`, when nothing earlier (slots, host capacity, the agent cap, a handler) already explains the wait:

- `Token budget (claude five_hour): needs about 44.00M tokens (default, planned); 31.20M remain before the reset at 2026-10-08T22:00:00Z, reserve 10% (source: provider reading)`
- `Token budget (codex five_hour): needs about 750.00K tokens; 800.00K remain before the reset at …, reserve 10% (source: allowance minus reported usage; provider reading for host mini is stale (captured …))`. The reading part reads `is stale (captured T)`, `has a reset that passed at T`, `is not reported`, `is missing`, `is unreadable` or `is malformed`.
- `Token budget (claude five_hour): no usable source. Provider reading for host mini is not reported and no reset time is set` (or `is missing`, `is unreadable`, `is malformed`).
- `Token budget: no estimate and no lane default; set one with tt usage defaults set`

`(default, planned)` or `(default, planned, Go race)` after the figure means the item has no saved estimate and a lane default was compared.

### Lane defaults

```
tt usage defaults set --small 18000000 --small-race 17000000 --planned 44000000 --planned-race 70000000
tt usage defaults get
```

An item with no saved estimate is compared with its lane default: the figure for the entry's template (`small` or `planned`) and for whether its verification includes the Go race check. The four figures are a per-project setting, not constants in the code; the backlog steward recomputes them from `tt usage --calibration` and sets them again. The same three kinds of caller as for a budget row may set them. Over HTTP it is `GET` and `PUT /v1/tasks/{id}/usage/defaults`.

**With Go race** follows the verification matrix: a path under `hub/` selects the Go group, which always runs the race check. So an entry is with Go race when any owned path is `hub` or starts with `hub/` (a file such as `hub/go.mod`, or a directory such as `hub/internal/store`), and when it declares no ownership at all, because a serial or unscoped entry is verified against everything. An entry that owns only other paths, docs for example, is without.

A lane default is labelled in the queue list (`default estimate 70.00M (planned, Go race)`, JSON `estimateDefault`), and it is used for admission only. **It never warns at 1.5 times and never holds at 3 times**: both compare only an estimate saved on the item.

### Hold past 3 times

When a usage upload changes an item's attributed tokens and the team of its running queue entry has spent **strictly more than 3 times** the item's saved estimate, the hub records one hold for that entry and estimate value. At exactly 3 times there is none.

- **What is counted.** The same figure the [warning](#warning) compares: runs bound to the item by agents with no project role, **bound at or after the entry's admission** (its claim for launch, `admittedAt`), not its enqueue. An agent bound while the entry waited in the queue is not counted. The database handler, backlog steward, deployment agent and owner helper carry a role, so they are neither counted nor held. An entry admitted before the admission time was recorded takes its launch reservation's time at the upgrade, and one without a reservation is read from its creation, as before.
- **What it does.** The hub refuses nothing: a held agent's posts, acknowledgements, work-item writes and usage uploads are stored, so a turn in progress and any write finish. Wakes are withheld in two places. The hub's wake-job lease returns no job for a held run, so nothing is written and the job stays due; it is leased on the first attempt after the hold is lifted. This binds every relay, on any host and version. And before the relay delivers an inbox wake it asks the hub, fresh every time, whether that exact run is held, and if so delivers nothing; `tt relay --status` shows `skip="team held past 3 times its token estimate"`, and the withheld attempt does not count toward the wake limit. A job leased before the hold existed is still delivered, and a lease that expires during the hold is not renewed. The relay's stalled-turn pass is skipped for a run whose last inbox attempt was withheld, so that a held team's quiet turn is not treated as a stall; the relay makes no read to learn of a hold it has not met on the inbox path, so a held run that only ever had broker wakes still gets the usual stall handling.
- **The ask.** One `question` to the owner helper, subject "A team has passed three times its token estimate and is held", from `system` / `usage-hold`, with the item, entry, estimate, team tokens and both commands. It obliges an answer: the owner helper runs `tt ack SEQ`, acts, then answers it with `tt send --kind answer --reply-to SEQ`. Until it is acknowledged the helper's other posts are refused after the usual grace period. With no owner helper the Board gets a notice with the same figures and `escalation=owner`.
- **Once.** There is no second hold or ask for the same entry and estimate value, however often uploads repeat or replay. Like a warning, a failed hold leaves the upload stored and nothing of the hold behind, and the next new or revised turn tries again.
- **The list.** `tt team queue list` prints a `held:` line under the entry, and its reason starts `Held past 3 times its token estimate`. `tt usage hold list` lists the holds. A queued entry that waits behind a held team shows `Waits behind entry tqe_…, held past 3 times its token estimate until the owner helper continues or stops it` instead of a stall, and no stall notice is posted for it.

**Continue.**

```
tt usage hold continue --project PROJECT_ID --entry tqe_ID
```

Only the owner, with no agent identity, or the project's owner helper may; anyone else gets "only the owner or the owner helper continues a held team". The hub leases the team's due jobs again and the relay delivers again on its next pass. Continue writes no estimate. To arm the hold again, have the handler save a larger one (`tt work-items update --estimate-tokens`): the team is then held when it passes 3 times the new value. Without a new estimate the entry is never held again at that value.

**Stop.**

```
tt team queue fail --task PROJECT_ID --entry tqe_ID --reason TEXT
```

Stop is the existing owner fail of the entry. Run it from an **unbound owner shell**: the CLI refuses owner-side queue changes from a session that carries an agent identity, the owner helper's included. The entry becomes `failed` with the reason, the usual "Team queue failed and requires owner action" notice is posted, and the hold is resolved as stopped in the same step. The stopped team still starts no new turn: its runs stay listed as held while the entry is failed and not released. What follows is the ordinary failed-entry cleanup. The list shows `Failed; N item-bound runs are still live or uncleaned`; close the members with `tt close NAME` on their host, close the lead by the owner's team close once its open obligations are answered or cancelled, and the runner releases the entry. The team's own close is not the stop: only the item lead may run it, it refuses while team obligations are open, and a held lead is not woken to run it.

A hold also ends, as stopped, when its entry finishes, is released, is recorded as owner-integrated or its team closes with the item open. Over HTTP the holds are `GET /v1/tasks/{id}/usage/holds`; with `?agent=AGENT&run=RUN` the answer says only whether that exact run is held. Continue is the `budget_continue` operation of `POST /v1/tasks/{id}/team-queue/actions`, with `agentId` naming an agent caller.

### Provider reading

Claude Code gives its status line command the account's rate limits. `tt host setup` installs a capture-only status line ([host-setup.md](host-setup.md#claude-usage-capture)) that saves them to `~/.local/state/tailterm/claude-usage.json` and prints nothing. The relay on that host reads the file every pass and reports it to the hub at once when its state, a percentage or a reset differs from its last report. Claude Code rewrites the file every few seconds with the same figures; that is not a change, and while it goes on the relay sends one keep-fresh report at most every 5 minutes, so the hub can tell a live reading from a stale one. Keep a row's `--stale` above 5 minutes for that reason. A capture time or file time that moved alone is never a reason to report, a missing file is not reported when no good reading ever was, and the last report is kept in the relay state directory, so a restarted relay does not send it again. The hub keeps the reading by host and runtime, and admission reads the rows for the entry's host.

A file that is missing, unreadable or malformed (not JSON, no capture time, no `rate_limits`, or a window without a numeric `used_percentage` from 0 to 100 and a `resets_at`) is reported too, as an **invalidation**: the hub overwrites both windows with that state and no figure, so nothing of the earlier reading remains. Admission then uses the allowance source, or holds with the no-source reason. `GET /v1/provider-usage?host=H` shows what the hub holds; the relay writes with `PUT /v1/provider-usage`.

The file is refreshed only while some Claude Code session on the host is active. A quiet host therefore goes stale, and admission falls to the allowance source, which sees only this project's reported usage.

### Rollout

Nothing is enforced until step 3. After the release, in this order:

1. On each agent host run `tt host setup`, then `tt host setup --check`. On a host where the capture status line was installed by hand with the same command, this takes it over.
2. `tt usage defaults set --project PROJECT_ID --small 18000000 --small-race 17000000 --planned 44000000 --planned-race 70000000` (the owner's figures in order #30088), then `tt usage defaults get`. The steward repeats this with recomputed figures.
3. `tt usage budget set --project PROJECT_ID --runtime claude --window five_hour --allowance N --reserve 10`, and the `seven_day` row. To calibrate N, divide the project's reported Claude tokens over a recent window by the change in `used_percentage` the capture file showed over the same time. Then read `tt usage budget get` and `tt team queue list` before relying on the reasons.
4. To switch admission off again, `tt usage budget clear` each row.

An entry that is already running at the upgrade is held at once if its team is past 3 times a saved estimate, so the owner helper may be asked on the day of the deploy.

### Limits

- **Every row applies to every entry**, whatever runtime its team uses. A Claude row holds a Codex team too; set rows only for runtimes that teams spend. Every reason names the runtime and window.
- **A held entry is passed at the head.** Like an entry waiting for a rebind it does not hold the queue, so a later entry that fits launches first, and a large item can be passed by smaller ones until the budget covers it. The claim of a held entry gets the quiet "not queue head" answer.
- **Codex has no provider reading.** The reading is the Claude capture only; a Codex row uses the allowance source, with `--reset-at`.
- The pause multiple is 3 and is not a setting.
- The allowance source counts reported usage of this project only, and reads the window's stored requests at each claim and list.
- **Relay requests.** The broker path adds none: the lease itself answers for a held run. The inbox path adds one read, fresh and never cached, only immediately before a delivery to an agent with no project role. The provider usage report goes out when a figure changed and otherwise at most every 5 minutes while the file is still being rewritten. A pass with nothing to deliver and no changed reading makes no added request.

### Storage

`usage_budgets` holds one row per project, runtime and window; `usage_provider_readings` one per host, runtime and window; `usage_estimate_defaults` one per project; `team_queue_budget_holds` one per project, entry and estimate value, with its state (`held`, `continued`, `stopped`), the compared figure, the ask and who resolved it. `team_queue_entries` gains `admitted_at`. All are additive and an older binary ignores them.

## Verification boundary

Synthetic tests cover both runtimes, normalization, streaming/deduplication, resets, two-item persistent-role attribution, overhead conservation, phase/role averages, prices, receipt loss/restart/archive and relay budget. A loopback fixture exercises the real relay append path through HTTP into the store. Chromium and WebKit exercise Usage through Projects, including stale/offline/older-hub behavior, filters, prices, escaping, narrow layout and focus continuity.

The migration rehearsal builds a production-shaped **synthetic** database using the approved base binary, then opens candidate/base/candidate and checks retained records, ledger/prices, integrity and foreign keys. Its execution, and the full independent matrix, require their separate frozen-plan gate. Targeted builder checks do not imply independent matrix acceptance. No live task/profile data is a test fixture, and no deployment is part of this work order.

Time accounting adds synthetic Codex and Claude transcripts under `hub/cmd/tt/testdata/usage-time`, covering overlapping tool calls, blocking and non-blocking inbox checks, a turn ended by `turn_duration`, a turn spanning an owner wait and an order handled in the middle of a turn. Relay tests cover exact intervals past the chunk limit, the classification table, poll requests, turn boundaries, outbox restart, retirement, the capability gate and the span-only drain. Store tests cover the phase and role split with waiting, the team timeline, wait causes before and after reopening the database, the additive `usage_spans` table and chunk immutability. Chromium and WebKit render a report produced by that real path. The migration rehearsal with `usage_spans` remains a verifier matrix step.

Round-one correction provenance: REQUEST #12757 / disposition #12758, renewed own Start #12763, handler release #12764; fixes b1–b5 plus existing c6 top-phase finding `wi_687271d49143fe15`. Other review findings remain held and do not widen this implementation.

Final focused b1 provenance: final general review #12779, disposition #12780, focused order #12781, own Start #12783, saved handler release #12784. This correction preserves Claude output only; Codex’s existing unavailable-subset handling is unchanged. Lifetime two general rounds remain exhausted, with original-reviewer focused verification still required.

Token estimates add store tests for the unchanged revision and revision-bound records, refusals, clear and delayed replay, a queued and an accepted unreleased entry with the release gate, the budget against the unfiltered report (shared request, closed run, revised and partial turns, share rebuild), every queue list form across a history page, and a database returned to the base schema. CLI tests cover the estimate flags, the text forms, the queue list and the calibration read past both page limits. Chromium and WebKit render every budget form in the Usage disclosure. Opening a database built by the base binary, and the base binary reopening it, is a verifier matrix step.

Token estimate warnings add store tests on isolated databases for the crossing (at the level and one token past it), no estimate, replayed and repeated uploads, a raised estimate, an item without a team, a project with no recipient yet, the per-project level and its refusals, three injected faults that must leave the upload stored and nothing of the warning behind, wake and delivery of the notice, a retired owner helper, a batch of unchanged turns, and a shared partial request. One server test covers the two routes and one CLI test runs `tt usage warning` against a real hub over HTTP.

The compared-figure correction (`wi_12d5c1ef9a73ceae`) moves those cases onto a running entry with a bound team member, admitted on a whole second with its team bound half a second later, and adds store tests for an item with large earlier tokens and a team below, at and past the level; turns by an unbound agent, a bound database handler, backlog steward and owner helper, and an agent bound before the entry; an item with no running entry; a second entry on the same item and estimate, with the first entry's team and an agent bound between the two still spending; and a database holding an earlier `usage_budget_warnings` row, closed and reopened. CLI tests cover both warning line forms, the `this team` budget forms, and `tt usage warning get` and `tt usage --item` against a real hub over HTTP with and without a running entry.

The token budget adds store tests on isolated databases with an injected clock for budget rows and their refusals, each source at both sides of the staleness bound and of a reset, an invalidation, lane defaults and the Go race rule, admission that fits, does not fit, fits after a reset and after a changed row, a held head passed by a later entry, a project with no row, the hold at exactly 3 times and one token past it, replayed uploads, two injected faults, acknowledge then answer, continue, stop by entry fail with open lead obligations, the excluded roles, an agent bound before admission through the store's own queue calls, and the two stall cases. Server and CLI tests run every route and `tt usage budget`, `defaults` and `hold` against a real hub over HTTP. Store tests cover the lease guard: a due job before, during and after a hold, a job leased before it, an expired lease, another run id, a stopped team and a bound role agent. Relay tests cover the inbox gate with a fake hub and a recording queue function, and the capture reader and report rule with fake files under a temporary home; the pinned relay request counts are unchanged. No test starts or pauses an agent, and none opens the real `~/.claude/settings.json` or state file.
