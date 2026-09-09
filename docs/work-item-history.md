# Work-item revision history

Status: build accepted; not released

Work item: `wi_f0d39d54a76011c7`

Build order: `wi_f0d39d54a76011c7-build-1`

Accepted implementation candidate:
`9ea86535e2e401e879d4fa1316483772de5e674e` on
`fix/work-item-history-f0d39d54`

This change adds immutable, readable revision history and recoverable keyed edits
for project-owned Bugs and Features. It does not claim to diagnose or repair the
owner's separately reported Bugs-only create/edit failure, which remains
unreproduced. No production hub, CLI, static target, live task/profile data, or
Tailscale configuration was changed during the build.

## User-visible behavior

- Every new work item and every future legacy or keyed edit stores a full immutable
  snapshot of the resulting revision in the same transaction as the current item.
- Bugs and Features share the same compact History control. It lists every
  materialized revision, opens exact historical content, reports known gaps, and
  shows only messages explicitly linked to the selected revision.
- History remains readable after a project closes. New edits remain disallowed.
- The editor stores unfinished values and the complete keyed request in the
  encrypted, hub-credential-scoped local vault. A response lost after commit can
  therefore replay the original request ID and original `expectedRevision` after
  a full page reload.
- Ordinary dialog close and Bugs/Features navigation retain unfinished drafts.
  A confirmed save or the explicit **Discard draft** action removes them.
- Changing an attempted edit rotates its request key. A definitive revision
  conflict also rotates the key while retaining the user's values, allowing a
  reopened editor to target the newly loaded revision deliberately.
- Linked-message reads drain every server page. A delayed response for an older
  selection cannot overwrite the currently selected revision.

## Storage and reconstruction

The migration is additive. It creates:

- `work_item_revisions`: immutable columnar snapshots keyed by owning task, item,
  and revision;
- `work_item_history_gaps`: durable, explicit ranges for evidence that cannot be
  reconstructed or verified;
- `work_item_history_state`: the observed current revision, latest materialized
  revision, coverage status, and last check;
- `work_item_update_requests`: task/caller/declared-agent-scoped keyed update
  receipts, exact payload hashes, and the declared run used by the write.

Startup reconciliation classifies old evidence before inserting snapshots. It
replays the valid change prefix, verifies it against the current projection, and
uses an explicitly labelled current-row checkpoint when the disputed final
revision cannot be proven. Missing, malformed, duplicate, mismatched, or colliding
evidence becomes a gap. An already materialized immutable row is never overwritten.
Reconciliation resumes from its durable checkpoint after restart.

The creation source message and structured A1 work-item links are the only
conversation evidence. Text, replies, or nearby discussion are never inferred as
revision links. Cross-project dispatches retain the actual destination task and
message coordinate while the item remains owned by its source project. Coverage
labels distinguish verified snapshots, checkpoints, and gaps.

## HTTP and client contract

Existing create, legacy `PATCH`, dispatch, and list/get shapes remain compatible.
New clients use:

| Operation | Route |
| --- | --- |
| Keyed edit | `POST /v1/tasks/{task}/work-items/{item}/updates` |
| Recover edit receipt | `GET /v1/tasks/{task}/work-items/{item}/updates/receipts/{requestId}` |
| List revisions | `GET /v1/tasks/{task}/work-items/{item}/revisions` |
| Read exact revision | `GET /v1/tasks/{task}/work-items/{item}/revisions/{revision}` |
| List reconstruction gaps | `GET /v1/tasks/{task}/work-items/{item}/history-gaps` |
| List explicit messages | `GET /v1/tasks/{task}/work-items/{item}/messages` |

A keyed edit includes `expectedRevision`, the changed core fields, `requestId`,
and optional declared `agentId`/`runId`. The store checks an identical receipt
before lifecycle rejection, so an exact retry can recover after closure. Reusing
the key with different content or scope returns conflict. A successful first write
atomically updates the mutable projection, legacy change evidence, immutable
snapshot, receipt, event, and history state. Failure injection at each write
boundary proves that no partial update or notification escapes.

Exact-revision reads return conflict plus the matching gap when the requested
revision is known to be unavailable. Malformed cursors and limits return a bad
request. Authorization and the existing shared-workspace actor trust boundary are
unchanged; recorded agent/run claims are provenance, not cryptographic authorship.

The Go client exposes every history and keyed-update route. The `tt work-items`
CLI exposes keyed `update`/`receipt`, `get --revision`, `revisions`, and
`messages`; the browser and cached hub clients also consume gap reads.

## Hard limits

- History page default: 32 records.
- History page maximum: 64 records.
- Encoded response maximum: 3 MiB, including the trailing newline and the final
  `nextAfter` cursor.
- Normal Go client response allowance: 4 MiB.
- Browser draft retention: at most 24 entries, each with a maximum serialized
  JSON length of 24,000.
- Existing work-item limits remain 120 characters for title and 8,192 characters
  for description.

If adding the final cursor would cross 3 MiB, the server removes another record,
sets the cursor to the last returned record, and measures again. It fails instead
of returning an unpageable oversized single record.

## Verification

The complete build evidence is retained outside version control at:

`/Users/stephenspeicher/projects/tailterm/.build/work-item-history/verification.md`

Accepted checks on candidate `9ea86535e2e401e879d4fa1316483772de5e674e`:

- `cd hub && go vet ./... && go test ./...`: pass.
- `cd hub && go test -race ./...`: pass.
- `npm test`: 121/121 pass.
- `npm run build`: pass.
- `node tests/work-item-draft-vault-browser.mjs`: Chromium and WebKit pass,
  including an encrypted full intent with an 8,192-character description and
  exact expected revision across reload.
- `node tests/project-work-items-browser.mjs`: Chromium and WebKit pass against
  a real throwaway hub at 390 by 720 pixels. It covers commit-before-503, full
  reload and exact replay, ordinary close/navigation retention, explicit discard,
  65 links over two pages, a delayed older-selection race, existing Bugs/Features
  CRUD and dispatch, mobile layout, cross-project links, and closed reads.
- Tight-boundary server fixtures prove revision, gap, and linked-message response
  sizing includes `nextAfter` at the exact 3 MiB boundary.

The unchanged foundation candidate `d8269f4` also passed a fresh actual
old/new/old/new executable matrix against baseline
`4de29d028842a24b391f3e9dc6086e398c10ea7f`: the old binary created and patched;
the new binary reconciled and keyed-edited; the old binary patched again; and the
new binary reconciled the suffix and recovered the original receipt. SQLite
integrity returned `ok` and the foreign-key check returned no rows. The accepted
correction changes no schema, migration, store write, or receipt semantics, so
that matrix remains attributed to `d8269f4` rather than being represented as a
new run.

## Release validation

Release requires a separate recorded order. Before activation:

1. Take and retain a consistent mode-0600 SQLite backup. Record the running hub,
   installed CLI, static application commit, artifact hashes, and rollback paths.
2. Build the hub and CLI from the exact accepted/integrated commit. Package the
   TailOS static application from the same reviewed source; do not use
   `npm run deploy:static`, which targets the old site.
3. Use TrueNAS middleware and the existing TCP listener for the hub. Do not install,
   configure, restart, or otherwise change Tailscale.
4. After activation, verify the process identity, authenticated read-only
   readiness, additive tables, `PRAGMA integrity_check`, and
   `PRAGMA foreign_key_check`.
5. Exercise isolated or explicitly authorized synthetic create, keyed edit,
   receipt replay, exact revision, paginated messages, closed history, and old CLI
   compatibility. Verify served asset hashes at `https://tailos.tailarr.com` and
   the required local preview.
6. Record exact deployed identities, hashes, commands, results, backup, and cleanup
   receipts. Do not use live task/profile rows as fixtures.

## Rollback

The safe application rollback is to restore the previously recorded static
package at TailOS and the local preview. The safe hub/CLI rollback is to replace
the binaries with their recorded predecessors while retaining the additive
database tables. Old binaries ignore those tables and continue using legacy
create/`PATCH`/dispatch behavior; keyed-update and history routes are unavailable
until re-upgrade.

Do not drop the new tables during ordinary rollback. Re-upgrade restarts
reconciliation from the retained state and preserves gaps and keyed receipts.
Restore the pre-release SQLite backup only for explicit disaster recovery because
doing so discards writes made after that backup. After either rollback path,
repeat process/version checks, authenticated readiness, SQLite integrity and
foreign-key checks, legacy write compatibility, and served-asset verification.
