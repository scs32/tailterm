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

Feature `wi_899863352c81e3b0`, revision 1, owner order #28848; builder assignment #28954. Phase 1 of the token guardrails: each work item can carry a token estimate, shown against what the item actually used. Phase 2 adds a [warning](#warning) when an item passes a multiple of its estimate. Nothing holds or pauses on it.

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

**A warning never blocks usage.** The check runs inside the upload's transaction, after its turns are stored, with each item's warning in its own savepoint. If anything in a warning fails (a read, a post, the record), all of that item's warning is rolled back: no message, receipt, obligation, wake job or record remains. The upload still commits, and the next new or revised turn for the item tries again. Such a failure is not reported anywhere. Only a cancelled request or a failing savepoint statement fails the upload.

**Storage.** `usage_entry_warnings` holds one row per item, queue entry and estimate value: the level, the team figure and the lifetime figure with their states at the crossing, both recipients and their message numbers, and the time. `usage_budget_warnings` holds the warnings recorded before entries were compared, one row per item and estimate value with the lifetime figure that was compared then; it is read for the list and never written, and no row of it is copied, changed or deleted. `usage_warning_settings` holds one row per project that set a level. All three are additive, and an older binary ignores the ones it does not know. An entry that was already running at the upgrade has no row in the new table, so it warns once if its team is past the level at its next new or revised turn, even if the item warned before under the old rule.

## Verification boundary

Synthetic tests cover both runtimes, normalization, streaming/deduplication, resets, two-item persistent-role attribution, overhead conservation, phase/role averages, prices, receipt loss/restart/archive and relay budget. A loopback fixture exercises the real relay append path through HTTP into the store. Chromium and WebKit exercise Usage through Projects, including stale/offline/older-hub behavior, filters, prices, escaping, narrow layout and focus continuity.

The migration rehearsal builds a production-shaped **synthetic** database using the approved base binary, then opens candidate/base/candidate and checks retained records, ledger/prices, integrity and foreign keys. Its execution, and the full independent matrix, require their separate frozen-plan gate. Targeted builder checks do not imply independent matrix acceptance. No live task/profile data is a test fixture, and no deployment is part of this work order.

Time accounting adds synthetic Codex and Claude transcripts under `hub/cmd/tt/testdata/usage-time`, covering overlapping tool calls, blocking and non-blocking inbox checks, a turn ended by `turn_duration`, a turn spanning an owner wait and an order handled in the middle of a turn. Relay tests cover exact intervals past the chunk limit, the classification table, poll requests, turn boundaries, outbox restart, retirement, the capability gate and the span-only drain. Store tests cover the phase and role split with waiting, the team timeline, wait causes before and after reopening the database, the additive `usage_spans` table and chunk immutability. Chromium and WebKit render a report produced by that real path. The migration rehearsal with `usage_spans` remains a verifier matrix step.

Round-one correction provenance: REQUEST #12757 / disposition #12758, renewed own Start #12763, handler release #12764; fixes b1–b5 plus existing c6 top-phase finding `wi_687271d49143fe15`. Other review findings remain held and do not widen this implementation.

Final focused b1 provenance: final general review #12779, disposition #12780, focused order #12781, own Start #12783, saved handler release #12784. This correction preserves Claude output only; Codex’s existing unavailable-subset handling is unchanged. Lifetime two general rounds remain exhausted, with original-reviewer focused verification still required.

Token estimates add store tests for the unchanged revision and revision-bound records, refusals, clear and delayed replay, a queued and an accepted unreleased entry with the release gate, the budget against the unfiltered report (shared request, closed run, revised and partial turns, share rebuild), every queue list form across a history page, and a database returned to the base schema. CLI tests cover the estimate flags, the text forms, the queue list and the calibration read past both page limits. Chromium and WebKit render every budget form in the Usage disclosure. Opening a database built by the base binary, and the base binary reopening it, is a verifier matrix step.

Token estimate warnings add store tests on isolated databases for the crossing (at the level and one token past it), no estimate, replayed and repeated uploads, a raised estimate, an item without a team, a project with no recipient yet, the per-project level and its refusals, three injected faults that must leave the upload stored and nothing of the warning behind, wake and delivery of the notice, a retired owner helper, a batch of unchanged turns, and a shared partial request. One server test covers the two routes and one CLI test runs `tt usage warning` against a real hub over HTTP.

The compared-figure correction (`wi_12d5c1ef9a73ceae`) moves those cases onto a running entry with a bound team member, admitted on a whole second with its team bound half a second later, and adds store tests for an item with large earlier tokens and a team below, at and past the level; turns by an unbound agent, a bound database handler, backlog steward and owner helper, and an agent bound before the entry; an item with no running entry; a second entry on the same item and estimate, with the first entry's team and an agent bound between the two still spending; and a database holding an earlier `usage_budget_warnings` row, closed and reopened. CLI tests cover both warning line forms, the `this team` budget forms, and `tt usage warning get` and `tt usage --item` against a real hub over HTTP with and without a running entry.
