# Broker phase 1 hub release with Jev scoring: `06c915a`

Feature `wi_de84224a37cbef70`, owner intake #8766, work order #8767. This is an owner-directed
release on September 24, 2026, run under the out-of-harness exception in `AGENTS.md`,
with the same handler-role note as the [first phase-1 release](../broker-phase1-99c7deb/README.md).

## What changed from `99c7deb`
- **Deploy scripts:** `06c915a` adds an optional plan field, `deployment.typesafeKeyPath`
  (`scripts/deploy-truenas-hub.py`, `scripts/truenas_release_preflight.py`, tested in
  `tests/truenas-release-preflight.test.js`). The field is pinned to
  `/mnt/deepfreeze/tailterm-hub/typesafe-key`. The deploy checks the file is non-empty and
  owned by UID 950 before updating, mounts it read-only at `/run/typesafe-key`, and sets
  `TAILTERM_TYPESAFE_KEY_FILE`.
- **Hub code:** unchanged since `e0641c9`. The hub was rebuilt from clean `06c915a`:
  27,992,226 bytes, SHA-256 `65f2b05fdd5b615a1b3397087f8ca7b2f054ec5d0041a1400232e60e8b7d6871`.
- **Key:** transferred over SSH on stdin (never printed). The file is owned 950:950, mode 400,
  109 bytes.
- **Backup:** `before-broker-phase1-jev-06c915a.sqlite` (67,411,968 bytes). Integrity ok,
  zero foreign-key violations. Receipt SHA-256 `280ef43b…`.
- **Deploy:** [result](deployment-result.json) `deployed`/`success`. The app is RUNNING with
  release `20260924-broker-phase1-jev-06c915a`, the key mount and the env var.

## Production check
- **Setup:** a one-off project, `tsk_963a913619a1d2d7` ("Broker phase 1 production check"), with a
  placeholder agent (`verification.invalid` host, no session). It posted one free-text and one
  typed message.
- **Result:** both check rows came back `scored` by `jev-1.13.0` in about 200 ms. The typed post
  also got `subject_plain`. The agent and project were closed right after. History is kept, and
  the agent shows `cleanupDone: false` because it never had a session.

## Rollback
- **Keep phase 1, turn Jev off:** redeploy with the `99c7deb` plan shape (no `typesafeKeyPath`).
- **Earlier binary:** point the app at `releases/20260924-broker-phase1-99c7deb`.
- **Before phase 1:** `releases/20260914-routing-review-ba37ee7`.
- The key file can be removed with `rm /mnt/deepfreeze/tailterm-hub/typesafe-key` once no release mounts it.
