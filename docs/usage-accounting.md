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

## Verification boundary

Synthetic tests cover both runtimes, normalization, streaming/deduplication, resets, two-item persistent-role attribution, overhead conservation, phase/role averages, prices, receipt loss/restart/archive and relay budget. A loopback fixture exercises the real relay append path through HTTP into the store. Chromium and WebKit exercise Usage through Projects, including stale/offline/older-hub behavior, filters, prices, escaping, narrow layout and focus continuity.

The migration rehearsal builds a production-shaped **synthetic** database using the approved base binary, then opens candidate/base/candidate and checks retained records, ledger/prices, integrity and foreign keys. Its execution, and the full independent matrix, require their separate frozen-plan gate. Targeted builder checks do not imply independent matrix acceptance. No live task/profile data is a test fixture, and no deployment is part of this work order.

Round-one correction provenance: REQUEST #12757 / disposition #12758, renewed own Start #12763, handler release #12764; fixes b1–b5 plus existing c6 top-phase finding `wi_687271d49143fe15`. Other review findings remain held and do not widen this implementation.

Final focused b1 provenance: final general review #12779, disposition #12780, focused order #12781, own Start #12783, saved handler release #12784. This correction preserves Claude output only; Codex’s existing unavailable-subset handling is unchanged. Lifetime two general rounds remain exhausted, with original-reviewer focused verification still required.
