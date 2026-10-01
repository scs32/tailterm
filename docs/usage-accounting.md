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

JSON quantities and costs are exact rational strings, such as `11/2`; display formatting happens only at the UI edge. Reports include measured-class counts, source coverage by enrolled run and explicit partial states. Historical work completed before collection cannot be reconstructed from lifetime totals.

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

Each projection has one phase: intake/planning, build, numbered review round (or explicitly unavailable round), corrections, verification, handler bookkeeping, or hand-offs. Handler bookkeeping takes precedence for handlers. Otherwise the latest applicable handled request/order at or before the request determines phase; an opening request can inherit the activation's first handled request. Message sequence breaks ties. Authoritative Planned member role text is normalized to planner/builder/reviewer/verifier while retaining its original source. Bounded ancestry stops without discarding already validated item links; a bad evidence entry cannot erase other valid links or a worker’s exact admitted binding. A result does not overwrite the request phase. Review rounds come from retained native review state; corrections require preceding blocker evidence.

A batch is frozen to private disk before upload, replayed unchanged until its matching receipt, and revision-checked by the ledger. Enrolled run provenance and request/revision/receipt records survive lifecycle closure and binding archival. Retirement freezes any pending outbox without rescanning archived transcripts or polling archived agents. Capability discovery is cached per host. Collection shares the relay request budget after delivery, adds no prompts or agent turns, and caps append records, batch bytes and local outbox size. Capacity, parse and partial-tail gaps remain explicit. A fresh activation with a reconciled first request records measured coverage. Historical run gaps stay in run coverage; they do not relabel later fully measured requests. Permanent 400/404/409 upload rejection preserves the immutable numeric batch and status in a private local quarantine with explicit partial coverage. It is never called an uploaded receipt. Later live requests may continue; frozen delivery rotates past errors or throttle, and drained cursor files are removed. Quarantine files remain for diagnosis/recovery; a rejected batch is not included in hub totals until it is successfully ingested. No owner Claude session is enrolled.

## Time

Feature `wi_95335cabc56442f5`, revision 1, owner order #13091; builder assignment #19086, Start #19088 and handler release #19100. Next to an item's tokens, the report says where its wall time went, what the waiting was for, and how many model requests only re-checked the inbox. It reads the same transcripts as the token ledger and adds no agent turn.

```
tt usage --project PROJECT_ID --item ITEM_ID
tt usage --project PROJECT_ID --item ITEM_ID --json | jq '.items[0].time'
```

The text form prints one `time:` line per item (wall time, the model, tool and waiting shares, poll requests), a `timeline:` line, one `time agent`, `time phase` and `time role` line each, and the longest waits with their Board message number. In Projects, the Usage disclosure shows the same inside each item: wall time and shares, the team timeline, the top waits, and a `Time:` line in each phase and role row. An item with no spans says **Time not measured**. A hub that does not report time (`timeVersion` absent) is shown as before.

### What is measured

All times are transcript record timestamps, UTC, as half-open intervals.

- **Turn.** Codex: `task_started` to `task_complete`. Claude: a real user prompt to the turn end (`end_turn`, `result`, `turn_duration` or an API error). A turn with no end record closes at the next turn start or at retirement, at the last record seen, and says so in its gap.
- **Tool time.** From a tool call record to its output record. Overlapping or touching calls are merged into their exact union; a gap is never bridged. A call with no output closes at the turn end.
- **Waiting.** Everything outside turns, plus a blocking inbox wait inside a turn (`tt wait`, `tt inbox --wait`). Codex `sleep` and shell `sleep N` are tool time.
- **Model time.** The turn minus tool, waiting and unmeasured intervals.
- **Unmeasured.** A tool call that runs a blocking inbox wait together with other work (`tt inbox --wait 9m && go test ./...`, or a wait piped into another command) cannot be split by timestamps. It is reported as unmeasured, never as tool or waiting. So is the remainder of a turn with more than 64 chunks of intervals.

A tool call is classified by the commands it executes, never by a pattern over its text. The relay decodes the call into shell strings (a Claude `Bash` command, a Codex `function_call` `cmd`/`command`, or each `tools.exec_command({cmd: "…"})` in a Codex `exec` snippet), splits each into simple commands, skips `VAR=value` words and the wrappers `env`, `command`, `exec`, `nohup`, `time` and `timeout N`, and looks one level into `bash|sh|zsh -c`. Only a command word whose basename is exactly `tt` counts. Quoted text is only ever an argument, so `echo tt wait` is ordinary tool time. Anything it cannot decode (command substitution, a heredoc, a `cmd` that is not a plain string literal, another `tools.NAME(` call) stays ordinary tool time. A wait beside only `cd` and other `tt` commands is still waiting. Only the class is kept: command text never enters the cursor, the upload or the database.

### Requests, segments and polls

Each metered model request is marked at the timestamp the token ledger gives it. A request's **segment** is the time after the previous request (or the turn start) up to its mark; the last one runs to the turn end. A segment is attributed exactly as its request's tokens are: same item shares, phase, review round and role, resolved at the same timestamp from the same handled Board evidence. A request and the time in its segment therefore never land in different phases, and an order handled in the middle of a turn moves later time to the new phase just as it moves later tokens.

A request is a **poll** when every tool call it issued is an inbox command (blocking or not) and the next request in the turn that issued any tool call is the same. A check that leads to other work is not a poll. `polls` counts them; `pollMs` is the model time inside their segments.

### Collection

A completed turn is uploaded once, as chunks of at most 32 intervals and segment pieces that together cover it exactly. Nothing is merged to fit; an interval crossing a cut is split there, and every segment piece names its own request, so the result does not depend on where the turn was cut. An open turn is absent from the report until it ends. Chunks ride the usage batch, so the frozen outbox, receipts, replay, retirement freeze and quarantine apply to them unchanged. A stored chunk is immutable: an identical replay is a no-op and a different payload under the same id is a conflict. The relay sends chunks only to a hub that advertises `usage.time`; until then they stay in the local outbox (at most 256, with explicit partial coverage beyond that), and a frozen run keeps its state file until its chunks have a receipt.

### The report

`time` is absent for an item with no spans. Durations are exact milliseconds as strings, a rational such as `60000` or `3001/2`.

- **Window.** From the earliest to the latest segment attributed to the item, clipped to `--from`/`--to`. `wallMs` is its length.
- **Per agent.** Model and tool time from its segments attributed to the item; the rest of the window is waiting. Model, tool, waiting and unmeasured sum to the window. A request that served several items gives each its share of model and tool time, and the remainder is waiting for that item. So a shared handler's work on other items is waiting here, as in the owner's own measurement.
- **Per phase and role.** The same keys as tokens. Model and tool time take the phase and role of their segment, and so does a blocking wait inside it. Waiting outside the agent's item work takes the phase and role of its latest item segment that ended before it, or of its first one. Each breakdown sums to the agents' total.
- **Team timeline.** A sweep over the window: some model working, only tools running, nobody active. A stretch some agent could not split is left out as `unmeasuredMs`; the four sum to the window.
- **Waits.** Each waiting stretch is split wherever something relevant opened or closed and labelled by the first rule that matches: `owner` (an owner obligation or decision request the agent authored), `handler` (the agent's request to a database handler: save, read-back, Start release), `teammate` (its request to anyone else), `owner` again (an owner request or decision linked to the item by any author), otherwise `unknown`. Open means the recorded interval: an obligation from creation to close, a decision from its request to its answer. Current state is never consulted, so the report is the same after a hub restart and long after the answer. `waits` lists the five largest by cause and Board message, with the agents that waited; `causes` totals all of them.

Known limits: a long open turn is invisible until it ends; a transitive wait (a builder idle while its lead waits for the owner) is `owner` only through the item link, otherwise `unknown`; work done before collection started is not reconstructed.

## Verification boundary

Synthetic tests cover both runtimes, normalization, streaming/deduplication, resets, two-item persistent-role attribution, overhead conservation, phase/role averages, prices, receipt loss/restart/archive and relay budget. A loopback fixture exercises the real relay append path through HTTP into the store. Chromium and WebKit exercise Usage through Projects, including stale/offline/older-hub behavior, filters, prices, escaping, narrow layout and focus continuity.

The migration rehearsal builds a production-shaped **synthetic** database using the approved base binary, then opens candidate/base/candidate and checks retained records, ledger/prices, integrity and foreign keys. Its execution, and the full independent matrix, require their separate frozen-plan gate. Targeted builder checks do not imply independent matrix acceptance. No live task/profile data is a test fixture, and no deployment is part of this work order.

Time accounting adds synthetic Codex and Claude transcripts under `hub/cmd/tt/testdata/usage-time`, covering overlapping tool calls, blocking and non-blocking inbox checks, a turn ended by `turn_duration`, a turn spanning an owner wait and an order handled in the middle of a turn. Relay tests cover exact intervals past the chunk limit, the classification table, poll requests, turn boundaries, outbox restart, retirement, the capability gate and the span-only drain. Store tests cover the phase and role split with waiting, the team timeline, wait causes before and after reopening the database, the additive `usage_spans` table and chunk immutability. Chromium and WebKit render a report produced by that real path. The migration rehearsal with `usage_spans` remains a verifier matrix step.

Round-one correction provenance: REQUEST #12757 / disposition #12758, renewed own Start #12763, handler release #12764; fixes b1–b5 plus existing c6 top-phase finding `wi_687271d49143fe15`. Other review findings remain held and do not widen this implementation.

Final focused b1 provenance: final general review #12779, disposition #12780, focused order #12781, own Start #12783, saved handler release #12784. This correction preserves Claude output only; Codex’s existing unavailable-subset handling is unchanged. Lifetime two general rounds remain exhausted, with original-reviewer focused verification still required.
