# Development handoff — September 8, 2026

## September 8 follow-up release

Commit `63a341c2de61d4945ac38a5c96e0a9f3d33ef284` is now deployed to
`https://tailos.tailarr.com` (`https://f0a7b147.tailos.pages.dev`). It adds local
QR Tailscale sign-in with the original link and automatic dialog closure,
full-height orchestrator/two-column worker stacking, and human-directed resumption
of online retired agents. The hub executable is now
`/mnt/deepfreeze/tailterm-hub/releases/20260908-owner-resume-63a341c2de61/tailterm-hub`.
Existing host CLIs/relays are compatible and were not restarted. No TrueNAS
Tailscale settings or live tasks were changed for testing. The current Mini has
a local static preview at `http://127.0.0.1:4318`; the older container inventory
below describes the earlier handoff machine.

[Release receipt](releases/tailos-2026-09-08-qr.json) records manifest/asset and hub
binary verification. The pre-update SQLite backup passed integrity verification
and is recorded there. All QR acceptance used synthetic URLs; a real phone scan
with the owner's account is still an owner acceptance check.

Current owner follow-ups under development: rename Tasks to Projects, add scoped
Bugs/Features with a database-handler agent and dispatch to a project, and physical
Left/Right Option drag placement (right/above respectively). The original restart
snapshot below is retained for context; it predates this follow-up release.

This is the restart guide for the next computer and developer/agent. Read
[Project overview](project-overview.md) for the product model and source map.
The repository root `AGENTS.md` captures the owner's persistent constraints.

## Current state

- Repository: `https://github.com/scs32/tailterm.git`.
- Working branch: **tasks-hub**. It contains the current implementation; `main`
  remains the older Tailterm release.
- At handoff, `origin` had **no tasks-hub branch** and the local branch had no
  upstream. A normal clone of origin alone will not recover this work.
- A portable, self-contained branch bundle is supplied alongside the repository:
  `tailterm-handoff-2026-09-08.bundle`. It includes the documentation commit and
  all ancestors of `tasks-hub`, not ignored credentials, dependencies or builds.
- Last deployed application commit:
  `56ded4133c7a9ec952004dd51fa11e94a6c7d382`.
- Handoff documentation is committed after that release. Production therefore
  intentionally trails the final branch HEAD by documentation-only changes.
  Do not mistake this for an unshipped code change.
- No known unfinished implementation fix was left by this session. No live
  services or user tasks were stopped as part of ending development.

### Deployment inventory

| Component | Location / current version |
| --- | --- |
| Current public application | `https://tailos.tailarr.com`, Cloudflare Pages project `tailos`, production branch setting `main` |
| Pages release | `https://5e02332e.tailos.pages.dev`, commit `56ded4133c7a9ec952004dd51fa11e94a6c7d382` |
| Original application, leave unchanged | `https://tailterm.tailarr.com`, project `tailterm`, commit `d9c884ed6d006d23de69ecb0a4b5f4980e12e40e` |
| Local frontend on old MacBook | `http://127.0.0.1:4318`, Apple container `tailterm-static`, image `tailterm-static:56ded4133c7a` |
| Local rollback image | `tailterm-static:c30436529ecb` |
| Coordination hub | TrueNAS custom app `tailterm-hub`, `http://100.116.238.37:18765` |
| Hub executable | `/mnt/deepfreeze/tailterm-hub/releases/20260908-task-cleanup/tailterm-hub` |
| Hub state | `/mnt/deepfreeze/tailterm-hub/state/hub.sqlite` |
| Hub token, private | `/mnt/deepfreeze/tailterm-hub/hub-token` |
| Host CLI on Air and Mini | `~/.local/bin/tt`, task-cleanup implementation from `c304365` |
| Host credential file, private | `~/.config/tailterm/hub.json` |
| Host service | per-user `com.tailterm.inbox-relay`, running `tt relay` |
| Host relay state | `~/.local/state/tailterm/relay/` |
| macOS relay log | `~/Library/Logs/Tailterm/inbox-relay.log` |

The [retained deployment receipt](releases/tailos-2026-09-08.json) records the
manifest SHA-256 and verification time. All 79 served assets matched the manifest;
the 80th inventory entry is `_headers`, a hosting configuration file. The deployed
frontend passed Chromium and WebKit smoke checks, including WASM startup.

### Last data backup and operational check

A consistent SQLite backup was created on TrueNAS using SQLite's backup API:

`/mnt/deepfreeze/tailterm-hub/backups/handoff-20260908T135025Z.sqlite`

It is mode 600 and passed `PRAGMA integrity_check`. At that snapshot, the hub had
**5 tasks, 12 agents, 1 encrypted profile, 1 open task, and 0 pending cleanups**.
These are real user records. Counts can change after this snapshot. In particular,
there is an open task: do not interpret ending this development session as
permission to close it or terminate its agent.

The earlier pre-cleanup backup remains at
`/mnt/deepfreeze/tailterm-hub/backups/before-task-cleanup-20260908T133133Z.sqlite`.
Keep state, token, and profile service identity together when planning any future
hub migration. This handoff moves development, not the live TrueNAS hub.

## Resume on another computer

### 1. Recover the complete source

Copy the bundle and clone it into a new directory:

```sh
git clone -b tasks-hub /path/to/tailterm-handoff-2026-09-08.bundle tailterm
cd tailterm
git remote rename origin handoff-bundle
git remote add origin https://github.com/scs32/tailterm.git
git status -sb
git log -5 --oneline
```

The renamed bundle remote is only a local recovery source. Do not replace this
checkout with `origin/main`. The owner can later publish the branch with
`git push -u origin tasks-hub`; the handoff did not publish the previously local
branch or merge it into main.

Copying the entire existing repository, including `.git`, is another option.
Avoid copying `node_modules`, generated `.build` caches, and platform-specific
binaries as substitutes for installation/rebuilding.

### 2. Install development prerequisites

Use a supported Node.js release at least 22.12, npm, Git, Python 3, curl, tar,
Go with automatic toolchain downloads enabled, and tmux for native integration
tests. `hub/go.mod` currently requests Go 1.26.6. The pinned Tailscale WASM build
also resolves its required toolchain automatically.

```sh
npm ci
npx playwright install chromium webkit
npm run build:wasm
npm run build:static
npm run preview:static
```

On Linux, browser system libraries may also need Playwright's documented OS
dependency installation. The preview listens on `127.0.0.1:4318`. If that port is
already occupied by an Apple container, use that instance or an explicit alternate
preview port; do not stop unrelated services.

`build:wasm` is required on a clean computer. Static packaging needs the generated
`wasm/tailserve.wasm`, `.build/go-modules.txt`, and its referenced Go module license
files. Copying only a compiled WASM file is insufficient for a fresh static build.
The build pins/downloads Tailscale 1.102.3 and verifies its source checksum. Speech
model/runtime assets are also pinned, downloaded as needed, verified and packaged
locally. Allow several gigabytes for dependencies, build caches and containers.

The old Mac repeatedly ran out of disk space during linking/container import.
Only obsolete Tailterm build artifacts/images were removed. The current local
image and one rollback remain. Do not prune other projects, credential files,
user vaults, or the live hub database to recover development disk space.

### 3. Restore the user's browser profile

Source code and a Git bundle do **not** contain the user's browser vault.

1. On the old browser, check **Profile sync** reports **Synced**; keep an encrypted
   Backup & restore export as an independent recovery copy.
2. Open `https://tailos.tailarr.com` on the new computer. Enter the same username
   and existing vault passphrase. The configured profile username is
   `stephenspeicher`; the passphrase is intentionally not recorded here.
3. Connect/authorize Tailscale for this new browser. Each browser requires its own
   device identity; do not copy the old browser's node identity.
4. The app discovers the profile service through the tailnet or can use the known
   hub address. Restore the existing profile. If the local browser already has
   saved data, choose the desired copy explicitly rather than overwriting it blindly.
5. Check saved servers, keys, Teams and hub configuration. Open task/history records
   come directly from the still-running hub. Current window layout and local
   scrollback do not sync.

A localhost vault and a TailOS vault are different origins. The same profile sync
or encrypted backup flow is needed when changing origins. Existing agent hosts
remain the old machines until their saved server entries are deliberately changed;
moving the development checkout does not move a running agent process or its files.

### 4. Set up host access only if needed

Using the public app does not require installing the hub or relay on the new
computer. If it will also be an agent host, install tmux, the chosen agent CLIs,
their account credentials, and `tt` for its OS/architecture.

```sh
npm run build:tt
```

That script creates Linux arm64/amd64 and Darwin arm64 binaries under `.build/tt/`.
Install the matching binary as `~/.local/bin/tt` with executable permissions.
For Intel macOS, build `GOOS=darwin GOARCH=amd64` explicitly. Provision
`~/.config/tailterm/hub.json` privately with the existing hub URL/token and mode
600. Never put the token into source, pasted prompts, logs or the static build.

On macOS, after installing `tt` and its configuration:

```sh
python3 scripts/install-relay-macos.py
tt doctor
tt relay --status
```

The installer uses a logged-in user's GUI launchd domain. For Linux or headless
hosts, supervise `tt relay` through that host's normal service manager. Confirm
the installed Codex supports the native `queue` operation before assuming idle
agents can be resumed automatically.

Development deployment also needs SSH aliases `mini` and `truenas`, suitable keys,
and account access. `truenas` currently uses `truenas_admin` (UID 950). Mini's OS
hostname is `Stephens-Mini`. Recreate those access settings through the user's
normal secure credential process; they are not included in Git. No changes to
TrueNAS Tailscale are necessary or authorized.

## Build, test and release

### Targeted regression commands

```sh
npm test
npm run test:hub
node tests/task-form-browser.mjs
node tests/teams-vault-browser.mjs
node tests/project-folder-browser.mjs
node tests/profile-sync-browser.mjs
```

Run suites relevant to changes; it is not necessary to repeat every suite for a
small edit. `task-form-browser.mjs` exercises a real isolated hub and private tmux
server, task launch/retry, permissions, retirement, closure/cleanup, and a 205-message
read-only archive/download. `task-history.test.js` checks export pagination across
separate message/event sequence spaces. The handoff release passed 69 JS unit tests
and the expanded task browser fixture in Chromium and WebKit. Hub vet/tests passed
for the last backend change (`c304365`).

For changes to WASM/SSH or wider terminal behavior:

```sh
./scripts/build-wasm.sh --test
npm run build:static
npm run test:static
```

The test-only WASM transport must never enter production. Other focused scripts
under `tests/` cover terminal links, context menus, tmux menus, appearance, speech,
clipboard and connection sizing. Use their documented fixture requirements.

### Publish the current frontend to TailOS

Commit changes first; release verification requires a clean tree and exact HEAD.
Because the handoff adds documentation after the last deployment, the existing
`dist-static/release.json` on the old machine is now intentionally stale for HEAD.
Rebuild before a future deployment; no redeployment was needed for this handoff.

```sh
npm run build:static
npm run verify:release
npx wrangler login
npx wrangler pages deploy dist-static --project-name tailos --branch main --commit-hash "$(git rev-parse HEAD)" --commit-dirty=false
```

`--branch main` here selects the existing Pages project's production deployment;
it does not require switching the source branch away from `tasks-hub` or merging
Git main. **Do not run `npm run deploy:static`: it targets the old `tailterm` project.**
Do not create a replacement Pages project or change DNS; TailOS already works.
Only `dist-static/` is public. Record the returned deployment URL, source hash and
release-manifest hash, and verify `/release.json` plus the served assets. `_headers`
is Pages configuration and is not available as a normal served asset.

Wrangler authenticated successfully on the old Mac through its private local OAuth
configuration; that file is not in the bundle. Authenticate afresh on the new
computer. A direct REST attempt using that local token failed while Wrangler
worked; use Wrangler rather than attempting to repair or expose the old token.

The old machine's detailed deployed-browser checks and receipts under `.build/`
are ignored convenience artifacts. The latest receipt has been copied into
`docs/releases/`. `tests/deployed-browser.mjs` accepts an explicit origin:

```sh
node tests/deployed-browser.mjs https://tailos.tailarr.com
```

Never rely on a script's default hostname for production testing/deployment.
Some browser scripts write preview screenshots; check `git status` afterward.

### Local Apple webpage

If Apple Container is installed and the existing `tailterm-static` container is
present, use:

```sh
python3 scripts/deploy-apple-web.py
```

It verifies the release, builds an image, checks a candidate on port 4319, then
replaces only port 4318's frontend container. It preserves the previous image.
On a new machine without this existing container, use `npm run preview:static`
first; the replacement script expects an existing instance. The hub is independent.

### TrueNAS hub updates

The hub is already running. Do not redeploy it just because development moved.
When a backend change requires an update, first take a consistent SQLite backup
and compile the Linux amd64 binary expected by the middleware deployment script:

```sh
cd hub
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o ../.build/ttbin/tailterm-hub-linux-amd64 ./cmd/tailterm-hub
cd ..
python3 scripts/deploy-truenas-hub.py UNIQUE_RELEASE_NAME --update
```

Use a new release name; do not overwrite a mounted executable. The script changes
only the `tailterm-hub` app through `midclt`, binds its existing private TCP address,
and retains state/token mounts. It must not enable embedded tsnet, mount a Tailscale
socket, install a second node, or modify host Tailscale. The app uses UID/GID 950,
a read-only root filesystem and a 32-agent cap. Rollback points the app at the
previous release path after considering migration compatibility; do not blindly
restore an older database over new user work.

Back up SQLite through its backup API (as done at handoff) or while stopped.
Copying a live `hub.sqlite` without its WAL is not a valid backup procedure. Keep
`profile_meta` and profile history so the sync service identity and revisions survive.

For a compatible hub/CLI rollout, update the hub first, then install the new CLI
atomically on each host and restart only its per-user relay. Preserve the old
binary for rollback. Do not restart all containers or unrelated services.

## Session closeout and next steps

Recent implementation sequence:

- `d277f3b`: retirement without destroying terminals.
- `1ecbcd7`, `0277d1e`: explicit project folders, inheritance and correction on retry.
- `c304365`: durable remote cleanup when closing tasks, pending/retry UI, ownership
  checks, run-scoped receipts, and relay integration.
- `56ded41`: remove the 01 badge/empty-state filler, expose closed conversations,
  read-only archives and complete history downloads.

All changes from this development session are committed. No background coding
agents were spawned. Test fixtures used temporary state and were stopped. Public
TailOS, the local frontend, the TrueNAS hub, and the user's host relays remain
running. Ending this coding conversation should not stop those services or the
open live task.

For the next coding assistant: read this file, the overview and root `AGENTS.md`,
check the branch/worktree, and ask what to work on next. Do not silently resume
old speculative desktop/IDE work, re-enable Files, rebuild the deployment topology,
or migrate the existing hub. The owner may choose transcript capture or other
improvements later; those are not unfinished work from this handoff.
