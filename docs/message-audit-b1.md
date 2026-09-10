# Message audit B1 implementation report

This report covers only bounded feature `wi_6148d93f013c1e1f` revision 2 under work-order Board message `1460` (`source-1421-b1-1`) in project `tsk_2cfcff70a0fbe967`. The mandatory contract is lead messages `1456` and `1457`, retained verbatim by handler messages `1461` and `1462`; allocation `1459` is retained in `1463`. The accepted A2 source and release baseline are retained in `1464`–`1467`. Implementation started from exact clean commit `58f9185981625b148b30ad4b5070a1a658e17f77` on `feat/audit-clients-b1`. Lead accepted complete source candidate `42e323a872f94db42ce6cc6cd755b6c414e62579` in message `1582`, then message `1594` ordered the same commit rebased onto exact released History-width root baseline `23d8371c97fb5be13f7268f5ed84e3e06047ac8a` (application `9cd38d37cb7466e8fc57ccf2da994d8457b8be24`). The rebase preserved the width CSS, browser fixture, report, release handoff, and B1 record; only the adjacent handoff sections required a textual conflict resolution.

B1 is additive and remains in observe mode. It implements audit-aware Board and CLI clients, a correction-aware encrypted cache overlay, scoped encrypted intent recovery, capability discovery, and immutable consistent v2 project exports. It does not activate B2 required-mode enforcement, add C1 typed-order/evidence structures, implement AIV, create coordination items, backfill live history, deploy, or complete parent `wi_edd77038c629a76c`; B2/C1/E1/E2 remain pending.

## Capability and export wire contract

Authenticated `GET /v1/capabilities` returns schema version 1, actual `messageAudit.versions: [1,2]`, `messageAudit.maxRelated: 15`, `auditExport.versions: [2]`, the export limits below, and `policy.messageAudit: "observe"`. A 404 from an older hub is treated as explicit unsupported/unknown capability. Authentication and connectivity failures remain errors and do not downgrade or strip a typed intent.

`POST /v1/tasks/{task}/audit-exports` accepts exactly:

```json
{ "requestId": "stable-key", "formatVersion": 2 }
```

It returns an immutable export ID, schema `tailterm-project-audit`, format version, source project, caller recorder, creation/expiration times, byte count, canonical SHA-256, per-stream cutoffs, availability, and a project/caller-scoped creation receipt. First creation returns 201. Exact replay returns 200 with the same frozen ID, digest, metadata, and artifact; a changed request under the same key returns 409. Exact replay after expiration returns the same unavailable tombstone and never regenerates bytes. Closed projects remain explicitly exportable.

`GET /v1/tasks/{task}/audit-exports/{exportId}?offset=N&limit=N` returns base64 JSON chunks with export ID, exact offset/next offset, total byte count, canonical digest, and completion marker. A missing or cross-caller artifact returns 404. An expired/unavailable artifact returns 410 with code `export-expired`.

The canonical UTF-8 JSON document has this envelope:

```json
{
  "schema": "tailterm-project-audit",
  "formatVersion": 2,
  "sourceProject": "tsk_…",
  "recordedBy": { "node": "…", "user": "…" },
  "createdAt": "…",
  "expiresAt": "…",
  "streams": {},
  "streamCutoffs": {}
}
```

One SQLite read/write transaction captures and atomically persists the bytes, metadata, digest, cutoffs, and receipt. Creation writes bounded canonical fragments into an uncommitted SQLite BLOB while updating the SHA-256 incrementally; it never collects a project stream or the complete document in a Go heap buffer. String fields are escaped incrementally, so even a single oversized row stops at the 32 MiB boundary. The receipt is inserted only after the complete artifact and final metadata exist, and any error rolls the placeholder row back. Downloads use SQLite `substr` to read only the requested bounded chunk. No transaction spans download requests. Row order and JSON map-key order are deterministic. Cutoffs include independent high-water marks for messages, hub events, work-item changes and message-audit events, row counts for other streams, and independent narrative sequence cutoffs per item.

The snapshot contains the supported project-owned streams as they exist now:

- project metadata; public agent/run/lifecycle/cleanup fields; public immutable work-item bindings without their admitted private context JSON;
- original messages, message posting receipts, hub events, decisions, original immutable message/work-item links;
- current work items, revisions, changes, history gaps/state, update receipts, dispatches and dispatch receipts;
- A2 immutable originals, current projections/links, complete events, correction/resolution receipts, explicit foreign associations and their receipts;
- narrative state, artifacts and all versions, exact link/retraction versions, coverage and versions, full reports and versions, timeline, receipts, and completion pins.

It excludes vaults, profiles, credentials, private admitted context/runtime contents, local working directories, private transcripts, external fetches, future C1 structures, and audit-export tables themselves. Later exports therefore cannot recursively contain earlier export artifacts.

Limits are exact: canonical content at most 32 MiB; decoded chunks at most 256 KiB; seven-day retention; at most eight ready exports and 128 MiB of ready content per project. A complete export that cannot fit fails before either artifact or receipt commits. A new creation beyond ready count/bytes fails without evicting a valid artifact. Lazy expiry deletes only content bytes and retains immutable identity/receipt metadata as a tombstone.

The additive migration creates `audit_exports` and `audit_export_receipts` plus the ready-export lookup index. Existing tables and A2 compatibility are not rewritten. Normal rollback keeps these additive tables and their receipts while an older accepted hub ignores them; dropping tables would destroy retry identity and is not a supported rollback.

## Board, cache, vault, and CLI

The compact Board now shows immutable original versus current classification, exact primary/related revisions, order references, correction provenance and history. Compose explicitly chooses ordinary/unclassified, Intake, or Work. Work requires one exact primary, at most 15 distinct related exact revisions, and an exact order message. Reply visibly proposes the current parent context and order while leaving the submitted values to the user. Human correction uses full-state revision CAS, reason, and exact sources; Intake resolution deliberately selects an existing exact item or keyed new item. Conflicts leave inputs and recoverable intent intact. Foreign association remains an explicit API/CLI operation and is never inferred from reply text or mentions.

The existing encrypted local vault now retains credential + hub + project-scoped Board drafts, complete correction/resolution editors, and uncertain post/correct/resolve/export attempts. Editors pin the displayed expected revision and retain all classification, link, target kind/title, reason, source, attempted-payload, nested request-key, and editor-generation fields across refresh, failure, project changes, and reload. It stores complete canonical attempted payloads and stable keys, distinct from subsequent unsent edits. A confirmed success, receipt, or exact retry clears an editor only when both its request key and submitted generation still match; an older operation cannot delete a newer edit. Composer cleanup similarly requires equality with the complete attempted payload, never request-key equality alone. If content changes while a persistence removal is pending, the cleanup detects the newer in-memory generation/payload and re-persists it after the delayed removal before returning. The changed draft keeps the previous attempted identity only as recovery provenance; its next deliberate submission detects the changed payload and rotates to a new request key. Recovery is only by explicit receipt lookup, exact retry, or discard—there is no background mutation queue. Limits are 32 retained records, 1 MiB total serialized storage, and 64 KiB per record. Overflow is visible and never truncates or evicts an uncertain operation. Vault lock, credential/profile changes, and stale persistence handles fail closed. Board intents are excluded from portable backups and shared profiles.

The encrypted read cache retains immutable message bodies and legacy bound inbox behavior, while a separate project-scoped current-audit overlay consumes the unfiltered A2 feed. It applies frozen pages idempotently in memory and persists projection plus checkpoint only after the entire window, including an empty window, is complete. Partial page failure keeps the prior checkpoint. Per-project job/epoch guards prevent an older overlapping response from replacing newer state. Visible historical messages lacking immutable original context are bootstrapped through exact reads independently of the feed checkpoint; missing or evicted records remain unknown until that read. Existing 4 MiB and seven-day encrypted cache bounds, offline/stale labels, and lock/profile isolation remain unchanged.

The Board offers verified v2 export actions for open and closed projects. Before creation it retains the stable request key as an uncertain encrypted intent; after metadata arrives it also retains the immutable ID/digest/size. Lost creation responses are recovered with the same key, and interrupted chunk downloads retry only the same frozen artifact. A replacement requires deliberate discard. It validates ID/boundaries/total/digest, 32 MiB document and 256 KiB chunk bounds, forward progress, and schema/project identity before emitting bytes. Legacy `tailterm-task-history` v1 remains readable and now says `snapshot:false` and `consistency:"non-snapshot-legacy"`; capability-absent hubs expose that choice explicitly. Merely expanding the ordinary full conversation continues to use legacy reads and never creates an audit export.

CLI additions are:

- `tt post --intake` and repeatable `--related TASK/ITEM@REVISION`, preserving exact primary/order/bound-context flags. `--request-id` alone is a keyed unclassified post, not linked work.
- `tt capabilities [--json]`.
- `tt message-audit get|history|changes|correct|resolve|associate|receipt`; mutation JSON files are bounded by 64 KiB and current agent execution binds the exact current agent/run rather than accepting a stale or different claim.
- `tt audit-export create --request-id KEY [--output FILE]` and `tt audit-export download --id ID --output FILE`; downloads use mode 0600 same-directory temporary files, validate immutable export ID, stable total/digest, positive progress, 32 MiB/256 KiB bounds, completeness and final SHA-256, then use an atomic no-clobber link and refuse a destination that existed or appeared during download.

The established literal help, reply, posting-receipt, identity, and legacy command paths remain in place.

## Isolated verification

All executed fixtures used temporary SQLite databases, loopback-only HTTP listeners, disposable browser contexts, or temporary extracted source. No live task/profile data, credentials, vault contents, production service, shared preview, or production network was used.

Passed:

- `go test ./internal/store ./internal/server ./internal/api`
- `go test ./internal/server -run '^TestCapabilitiesAndAuditExportHTTP$' -count=1 -v`
- `go vet ./...`
- `go test -race ./internal/store ./internal/server ./internal/api`
- `go build ./...`
- `go test ./cmd/tt -run 'TestDownloadAuditExport' -count=1 -v`
- `npm test` — 147/147
- `npm run build`
- `node tests/work-item-history-width-browser.mjs` — Chromium and WebKit, Bug and Feature dialogs at desktop 1366/1024, narrow 640/390, and 200%-zoom-equivalent viewports
- `node tests/board-compose-browser.mjs` — Chromium and WebKit
- `node tests/board-audit-async-browser.mjs` — Chromium and WebKit
- `node tests/board-intent-cleanup-browser.mjs` — Chromium and WebKit
- `node tests/board-audit-old-hub-browser.mjs` — Chromium and WebKit against exact extracted A2 baseline `58f9185981625b148b30ad4b5070a1a658e17f77`
- `node tests/work-item-draft-vault-browser.mjs` — Chromium and WebKit
- `node tests/board-scroll-browser.mjs` — Chromium and WebKit
- `git diff --check`

After rebasing onto `23d8371c97fb5be13f7268f5ed84e3e06047ac8a`, the complete width browser matrix, Board cleanup/compose/async fixtures, encrypted vault fixture, 11/11 cache tests, the independent newer-editor reproducer, 147/147 JavaScript suite, production build, and diff check were rerun on the exact combined source. Both browsers retained `remainingEditorCount: 1` and the exact newer unsent reason. The width tests passed all 20 Bug/Feature and viewport combinations. The rebase changed no Go file relative to accepted source candidate `42e323a872f94db42ce6cc6cd755b6c414e62579`, so the backend evidence above remains the exact tested B1 backend rather than a claim of redundant post-rebase execution.

The snapshot tests create the export and download its first chunk, then add an agent and binding with private context, post another message, update an item, correct an audit link/classification, and close the project before downloading the remaining pages. The verified frozen bytes retain only the pre-cutoff task/item/message/audit/binding projections with no missing/duplicate rows or leaked private context. Tests also cover exact replay, changed intent, digest and offset checks, caller isolation, chunk and past-end rejection, both single-row and many-row 32 MiB early hard failure with no artifact/receipt, eight-ready and 128 MiB project limits, closed-project creation, expiry cleanup, and tombstone replay without regeneration.

Browser acceptance covers explicit Intake, Work, related links, reply proposal and deliberate reset, existing-item and new-feature resolution, pinned correction/resolution editors through refresh/error/page reload, exact retry without a duplicate, old-message correction without a new message, original/current links and detailed before/after/actor/source history including a closed project, and an unavailable projection shown as unknown with exact retry and no correction action. A separate failing-before/passing-after Chromium/WebKit cleanup fixture submits a correction, resolution, and main post, edits each while its response is pending, and proves the newer values remain visible and durable after the older success and a page reload. It also proves an older uncertain correction receipt and an older exact retry cannot remove a newer editor generation; both editor and composer values survive removal deliberately paused until after a newer persistence save; and the next deliberate post uses a new request key for the changed payload. Lead's independent `/tmp/tailterm-b1-review-newer-editor.mjs` changed from `remainingEditorCount:0, unsentValue:null` in both engines to `remainingEditorCount:1` with the exact newer value.

The remaining browser acceptance covers open/closed verified export, committed export-create response loss and same-key receipt recovery, interrupted chunk recovery against the same frozen ID without replacement creation, ordinary Enter, Shift+Enter, synthetic composition/repeat guards, recipient/cancel retention, compact native select behavior, scrolling/focus continuity, encrypted reload/scope/full-store rejection/removal, lock invalidation, wrong password, and wrong local profile. Delayed audit and vault reads are switched across projects and credential/client replacement without stale repaint in both engines. The exact old A2 hub returns capability 404; both browsers hide typed controls, post an ordinary message without downgrade, and download explicitly labelled non-snapshot v1 history.

Cache unit tests cover a failed middle page followed by replay from the old checkpoint, empty completed windows, exact historical bootstrap, encrypted offline reload, credential isolation, lock during refresh, disposal, and an older r1 bootstrap completing after r2 without changing either the returned or durable r2 projection. CLI tests cover related parsing, duplicate/shape behavior through the server contract, request-key-only classification, exact current-run binding, verified multi-chunk download, wrong export ID, changing total, zero progress, oversized chunk, an output path created during download, and absence of partial files. Existing A2 suites continue to cover CAS, receipts, changed-key and changed-target conflicts, agent state/run enforcement, feed pagination/filter/cutoffs, association authority, concurrency, additive migration, and legacy A/B-bound reads.

## Explicit nonpasses and remaining gates

Repository-wide `go test ./...` is not claimed as passing. The untouched accepted baseline already has two `hub/cmd/tt` failures documented in the final A2 report: `TestPostHumanReplyAndLiteralHelp` receives its mock's `500 unexpected route`, and `TestAskPreservesIdentityContextAndReplayPayload` blocks its test server until timeout. A full `go test ./cmd/tt` attempt in this B1 session reproduced that route failure/hang and was interrupted; B1-focused CLI tests and every affected backend suite pass. Native macOS popup option selection and physical IME behavior are not claimed beyond actual Chromium/WebKit application flows plus synthetic composition events.

Lead accepted the complete B1 source candidate in message `1582`; the exact combined-width candidate still requires lead integration review and database-handler revision-checked result storage before bounded item completion. Deployment to the hub, Mini/Air CLI, TailOS, or Mini frontend requires a separately recorded release order with backup, integrity, compatibility, rollback, and target receipts. No deployment, live backfill, B2 activation, AIV/C1 work, Tailscale/network change, root integration, or preview PID 60799 change occurred.
