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

Lead accepted the complete B1 source candidate in message `1582`, accepted exact combined-width candidate `da3b6867dbf9c655888060ea7c497948cc94357a` in message `1615`, and issued the concrete release order in message `1616`. The actual release is recorded below. Database-handler storage and lead actual-release acceptance are still required before bounded item completion.

## Released stage

The shared `tasks-hub` branch was tracked-clean at exact width root `23d8371c97fb5be13f7268f5ed84e3e06047ac8a`, apart from three preserved owner screenshots excluded only through the repository-local Git exclude file. It fast-forwarded only accepted combined candidate `da3b6867dbf9c655888060ea7c497948cc94357a`. The exact clean source, static package, Linux hub, Darwin CLI, production WASM, and module inventory are retained at `.build/releases/message-audit-b1-da3b686`. Application/source commit and the following documentation commit remain distinct.

The Linux amd64 hub is 27,009,186 bytes with SHA-256 `5244f92ee1497c4e3ee7893d4dcf6eab34969e6cee8e6dcd4f9309ae446397cd`. Before deployment, the required A2 rollback binary `/mnt/deepfreeze/tailterm-hub/releases/20260910-message-audit-a2-eba02aa/tailterm-hub` matched SHA-256 `16786368ccd79221df58001a65b88894fd1f99611b9ba76c2c7f25dd3bf1fa01`. A new SQLite online backup was created through Python's backup API at `/mnt/deepfreeze/tailterm-hub/backups/before-message-audit-b1-20260910T172749Z.sqlite`; it is mode 0600, 12,509,184 bytes, SHA-256 `4a906c69573546d1c662619a4419fa964aca3a14edb4336f7857d15891eb634e`, integrity OK, with zero foreign-key violations.

Source-inspected `scripts/deploy-truenas-hub.py` updated only existing custom app `tailterm-hub` through TrueNAS middleware to unique release `20260910-message-audit-b1-da3b686`. Final inspection shows exactly one RUNNING `hub` container using `gcr.io/distroless/static-debian12:nonroot`, the same `100.116.238.37:18765` TCP listener, read-only new binary and token mounts, and the same writable state mount. The deployed binary matches the retained build. Live integrity remains OK with zero foreign-key violations and the two expected additive `audit_exports`/`audit_export_receipts` tables plus ready-export index. Authenticated capabilities report message-audit versions 1/2, audit-export v2 with the exact limits above, and policy `observe`; candidate and installed CLI doctor plus public agent reads pass, while unauthenticated `whoami` remains 403. No export, item, message, correction, backfill, or other live-content test was created.

The Darwin arm64 `tt` is 6,646,722 bytes with SHA-256 `3f00c6002476acb4f9dcac8b43e9fb16c9c201f96efb8d36d2ba1631f5885571`. Mini and Air first matched the required prior SHA-256 `a2a640d18e3d3014bcd8afdafbaf1f8994c5fbaf4623ea168014aeba7f68b49a`, retained exact mode-preserving rollback copies at `~/.local/bin/tt-before-message-audit-b1-da3b686`, and atomically installed the matching new binary. Both installed hashes, doctor, and authenticated capabilities pass. Existing relay PIDs 83457 and 912 remained PPID 1 and were not restarted.

The clean static package contains 82 manifest entries and `release.json` SHA-256 `61047c0e2153c07d51ad354f1114ef78de6033070b3ab32bc200fab61f74fc1d`; 81 entries are public served assets because `_headers` is Pages configuration. It reused the width release's independently verified production Tailscale 1.102.3 raw WASM, SHA-256 `dc841019c8a28670b3a44e0657f3a1e081648dbb561d5072eb735e987e577cb8`, and exact module inventory SHA-256 `2c33e75b00b437e19377989a0fd36a631dd4a9cbd94b8fd17833ea4496e6778b` after confirming tracked WASM inputs and dependency lock were unchanged. Full `build:static` packaging reverified the pinned speech model, runtime, modules, and licenses; `verify:release` passed from exact clean source.

Wrangler 4.131.0 published only that package to Pages project `tailos`, production branch `main`, commit `da3b6867dbf9c655888060ea7c497948cc94357a`, dirty false. Deployment `e59f1292-8c6f-41f6-95e1-37e98f13a686` is available at `https://e59f1292.tailos.pages.dev` and through `https://tailos.tailarr.com`. Mini serves the byte-identical root `dist-static` package through preserved detached PID 60799/PPID 1 at `http://127.0.0.1:4318`; it was not restarted. All three origins matched the exact manifest and all 81 served assets by identity SHA-256, size, status, canonical root index, and each origin's MIME contract. Fresh disposable Chromium contexts on all three origins started production WASM, created and restored only a synthetic local vault/key, passed three layout sizes, and reported no page errors. The exact-source Chromium/WebKit Board, cache, vault, and History-width evidence above remains the browser compatibility evidence; Playwright WebKit is not a native Safari claim.

Pre-publication rollback verification passed the retained width package `.build/releases/history-width-9cd38d3`, application `9cd38d37cb7466e8fc57ccf2da994d8457b8be24`, deployment `08760468-ec79-4953-bd07-18f51564d232` / `https://08760468.tailos.pages.dev`, and manifest SHA-256 `671872613c224d7d9fc57275c18bbd68732872136ae10cb9dcf4e0c3cf10ae6f`. Immediate frontend rollback republishes that exact package to `tailos` and synchronizes its `dist-static` contents to Mini without restarting PID 60799. CLI rollback atomically restores each verified `tt-before-message-audit-b1-da3b686` without restarting relays. Normal hub rollback points middleware at the retained A2 binary while keeping the additive current database and receipts; tables must not be dropped. The online backup is disaster recovery only because restoring it loses all writes after `2026-09-10T17:27:49Z`.

Operational corrections are explicit. A first read-only app-inspection command incorrectly used `midclt -j` for a non-job method and failed without mutation; the correct read-only call then verified topology. The local `mini` SSH alias was unavailable, so Mini's installed binary/processes were inspected and updated directly on the current `Stephens-Mini` host. Root's ignored raw WASM and module-inventory files were stale and therefore rejected before packaging; exact verified width production artifacts replaced only those ignored inputs before the full clean build. Because PID 60799 directly serves root `dist-static`, the required build made Mini serve the B1 package before the attempted rollback-origin comparison; this occurred after hub and CLI rollout, changed no process, and the exact package then passed all 81 asset checks. Immediately after Pages deployment, the custom domain exposed the new manifest while one newly named CSS path briefly returned the HTML fallback during propagation; no second deployment occurred, and the canonical path converged on the next five-second check before the full 81-asset and browser verification passed.

No Tailscale/network/listener configuration, relay, preview process, old `tailterm` Pages project, Air preview, live work-item content/state, profile/vault, historical correction/backfill, required-mode/B2, C1, E1/E2, AIV, or unrelated service/session was changed. Repository-wide `go test ./...` remains explicitly unclaimed for the documented untouched cmd/tt base failures. This release is builder-verified, not self-accepted; handler must save the complete report and lead must independently accept the actual release before the bounded child can be marked Done. Parent `wi_edd77038c629a76c` remains in progress with B2/C1/E1/E2 pending.
