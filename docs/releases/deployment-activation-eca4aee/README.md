# Deployment agent activation: `eca4aee`

The O1 hand release of `eca4aee` and the O2/a13 provisioning evidence for the deployment
agent, item `wi_d7010deecb20211f` (lead REQUEST #15363). Recorded under item
`wi_8123b327b0b87e7b`, work order #15389. Every value is copied from
`.build/activation-a13-evidence.md` (SHA-256 `dcd6c122…`), which the owner helper collected
on 2026-09-29, 22:27–22:44 PDT. Values are as of that collection, not current state. The
evidence holds no secret values, and none are added here.

## O1 hand release (intervention #15346)
- **Code:** `eca4aeee0abd9b30a142eaefbffdbb3d0c474d57` (owner-accept #15343).
- **Hub and bridge:** release `20260929-activation-eca4aee`.
  - Pre-release backup `before-activation-eca4aee.sqlite`, 428,347,392 bytes, SHA-256
    `6590e4f9…`, integrity ok, 0 FK.
  - Rollback binaries: `.build/prev-c6a8ec1`.
- **Mini `tt`:** `eca4aee`. Rollback binary: `~/.local/bin/tt.prev-c6a8ec1`.
- **TailOS:** `eca4aee` (Pages `b3de5519`). TailOS rollback: not recorded in the a13 evidence.
- **Backup storage location:** not recorded in the a13 evidence.

## O2 provisioning (a13)
- **Deployer (at collection):** `agt_11ee78b6de77e2da`, name `deployer`, role
  `deployment_agent`, runtime `generic`, status `running`.
- **cwd:** `/Users/stephenspeicher/projects/tailterm-deploy`, a dedicated detached worktree
  (not the main checkout, not `.build/worktrees`), at tasks-hub `37a0e36`.
  - `npm ci`; `wasm/tailserve.wasm` and `.build/go-modules.txt` copied (`build:static`
    requires the latter).
  - `build:static` and `verify:release` pass; status clean.
- **Run:** `tt deployment serve --config <private config>`.
- **Config:** `~/.config/tailterm/deployment-tsk_e7af3c28a444b09a.json`, mode 600.
  - Keys: baselines, cwd, enabled, inputs, journalDirectory, targets, tt, version.
  - Targets: bridge, hub, mini, tailos.
  - Credentials by reference only (truenas SSH alias, osxkeychain git helper, Wrangler login,
    tt token).
- **Journal:** `~/.local/state/tailterm-deploy/tsk_e7af3c28a444b09a`, mode 700.
- **TailOS retained dist seed:** `journal/tailos-dist-eca4aee…` (mode 700). Its
  `release.json` commit and files map equal the live `https://tailos.tailarr.com/release.json`.
- **Baselines (from live probes):** hub, bridge, mini, tailos =
  `eca4aeee0abd9b30a142eaefbffdbb3d0c474d57`.
- **Probe files** (name, SHA-256 prefix: contents), verbatim:
  ```text
  provision-probe-hub.json 80696a08ab19a5bd: {"commit": "eca4aeee0abd9b30a142eaefbffdbb3d0c474d57", "integrity": true, "hubResponds": true, "migrationsApplied": true, "containersRunning": true, "release": "20260929-activation-eca4aee"}
  provision-probe-bridge.json eca4bad237a85c46: {"commit": "eca4aeee0abd9b30a142eaefbffdbb3d0c474d57", "integrity": true, "hubResponds": true, "migrationsApplied": true, "containersRunning": true, "release": "20260929-activation-eca4aee"}
  provision-probe-mini.json d9eba9cdd9ca145e: {"commit": "eca4aeee0abd9b30a142eaefbffdbb3d0c474d57", "integrity": true, "relayRunning": true, "newErrors": 0}
  provision-probe-tailos.json a9af4c76d9d09092: {"commit": "eca4aeee0abd9b30a142eaefbffdbb3d0c474d57"}
  ```
- **Heartbeat** (`lastSeenAt` age, sampled every 60 s for 5 min): max 17 s (limit 90).
- **Main checkout detached:**
  `git worktree list --porcelain | grep -c '^branch refs/heads/tasks-hub$'` = 0.
- **Secret check:** hub token as a fixed-string pattern over journal + config: 0 files.
- **tt deployment handler** (deployer identity): `agt_daca5d83b722bf9d` db-handler-sonnet-2
  (non-retired).
- **Ledger** after the handler's supersede (#15373): superseded 24, verified 0.
