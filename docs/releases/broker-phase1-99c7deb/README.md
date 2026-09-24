# Broker phase 1 hub release: `99c7deb`

Feature `wi_de84224a37cbef70`, owner intake #8766, work order #8767
([work order](../../broker-phase-1.md)). Owner-directed release on September 24,
2026, run from a Claude Code session under the out-of-harness exception in
`AGENTS.md`. No database handler was running, so that session ran the
handler-owned backup preflight on the owner's instruction. The plan's
`databaseOwner` field is the required role label.

## What was deployed
- **Hub:** Linux amd64, clean source `99c7deb`
  (`CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w'`).
  27,992,226 bytes, SHA-256 `da38517fcae4ab7e93fc6a5409d79adbae48e02200802643a7b29998c8847995`.
  Installed at `/mnt/deepfreeze/tailterm-hub/releases/20260924-broker-phase1-99c7deb/tailterm-hub`.
  The hash on TrueNAS matches the local build.
- **Backup** ([receipt](preflight-receipt.json), receipt SHA-256 `76b906c5…`):
  `before-broker-phase1-99c7deb.sqlite`, 67,399,680 bytes, SHA-256 `209d940e…`.
  Integrity ok and zero foreign-key violations in both source and backup. Profile snapshots match.
- **Deploy:** [result](deployment-result.json) `deployed`/`success`.
  The `tailterm-hub` app is RUNNING with the new read-only binary mount. The state and token mounts are unchanged.
- **Mini CLI:** `tt` from `56b00ad`, recorded in the [work order](../../broker-phase-1.md).
  The phase-1 CLI surface is unchanged since then; `99c7deb` only touched hub redaction and docs.

## Verification after deploy
- `tt doctor` reaches the hub.
- Project `Tailterm Development` is readable (paused), and board messages through #8767 are intact.
- `GET /v1/tasks/{id}/message-checks` answers (empty until new posts arrive).
- Feature `wi_de84224a37cbef70` reads back at revision 2, in progress.

## State and rollback
- **Jev scoring is off.** No `TAILTERM_TYPESAFE_KEY_FILE` is configured, so agent posts record
  `jev_status: disabled`. Enabling it needs the key mounted read-only into the app and the env var
  set; that is a separate owner step.
- **Migration** is additive only: a `messages.envelope` column and a `message_checks` table.
- **Rollback:** point the app back at `releases/20260914-routing-review-ba37ee7` (the previous
  running release). The older binary ignores the new column and table. Do not restore the backup
  over newer work.
- **Deferred:** a real wake of a registered agent thread through the relay on Codex 0.156.1,
  and the TailOS rebuild.
