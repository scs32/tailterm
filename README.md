# Tailterm

A browser SSH terminal that connects through Tailscale. Run it from a static website: SSH, the Tailscale node, and the encrypted vault run in your browser through WebAssembly.

**Current development release: [tailos.tailarr.com](https://tailos.tailarr.com)**

**Moving computers or taking over development?** Start with the
[handoff guide](docs/handoff.md) and [project overview](docs/project-overview.md).
The current implementation is on `tasks-hub`; the original Tailterm site and
`main` remain a separate older release. A clone of `origin/main` does not include
this branch's work.


## Features

- Discover tailnet devices and save their SSH connection settings.
- Generate Ed25519 SSH keys in the browser or import existing private keys; copy the matching public key for your server account.
- Use Tailscale SSH or standard SSH authentication with keys, passwords, and interactive verification.
- Keep credentials and Tailscale identity in an encrypted browser vault, with encrypted backup and restore.
- Start or resume remote tmux sessions. Blank session names receive random IDs.
- Select multiple servers in the sidebar to filter open sessions; **All** shows every server. Hidden sessions stay connected, and mixed-server groups retain their layout. Use **+** to choose a server when opening a session.
- Drag terminal tabs together into groups, resize tiled panes, and move panes back into separate tabs.
- Use fullscreen or expand the terminal inside the browser window.
- Open terminal URLs with Command-click on Mac or Ctrl-click on Windows/Linux.
- Copy and paste through tmux, including bracketed paste, multiline confirmation, and OSC 52 clipboard support.
- Customize colors, fonts, cursor, spacing, and focus-following behavior.
- Render terminals on the GPU with WebGL, with a toggle for ligature-friendly DOM rendering.
- Run teams of AI coding agents as **tasks**: a terminal tab mirrors every agent on the task, agents message each other through a shared board, and lifecycle events surface as tab activity. See [tasks](docs/tasks.md).
- Switch between Terminals, Board, Tasks, and Teams. Files is intentionally hidden; SFTP still powers project-folder selection and uploads.
- Save reusable teams with explicit models, roles, prompts, permissions, orchestrators, helper limits and optional swarm messaging.
- Retire workers while preserving their terminals, or close a task to clean up its owned remote tmux sessions with durable host retries.
- Read closed task conversations and download their retained messages, roster, objective and activity history.
- Optionally sync encrypted profiles across computers through the existing coordination hub.

The terminal renderer is xterm.js. The appearance controls take inspiration from Ghostty configurations; this does not embed Ghostty.

## Run the static client locally

Requires Node.js 22.12 or later, npm, Go with automatic toolchain downloads enabled, and a POSIX shell with curl and tar. The WASM build downloads a checksum-verified Tailscale source release and its required Go toolchain.

```sh
npm ci
npm run build:wasm
npm run build:static
npm run preview:static
```

Open **http://127.0.0.1:4318**. Create a vault passphrase, connect Tailscale, then choose **Discover devices**. Configure the device's SSH username and credentials, and use the terminal's **+** launcher to start or resume a session.

tmux must be installed on the destination for persistent sessions. Standard SSH host keys are verified on first connection and pinned for subsequent connections. The browser cannot automatically read your computer's SSH keys or ssh-agent; import keys explicitly.

## Hosting

Deploy only `dist-static/` to an HTTPS static host. This branch uses Cloudflare Pages project **tailos**. From a clean committed checkout:

```sh
npm run build:static
npm run verify:release
npx wrangler login
npx wrangler pages deploy dist-static --project-name tailos --branch main --commit-hash "$(git rev-parse HEAD)" --commit-dirty=false
```

The Pages production branch setting is `main`; the source checkout remains
`tasks-hub`. **Do not use `npm run deploy:static` for this branch**: that older
script targets `tailterm.tailarr.com`. Do not overwrite the original site.
The existing TailOS custom domain is already configured. For forks, select your
own project/domain. The included `_headers` configures security and cache headers.
See the [handoff](docs/handoff.md) for exact current release and deployment details.

The approximately 37.5 MB WASM module ships as an approximately 8.5 MB gzip asset, below Cloudflare's 25 MiB per-file limit. The browser decompresses it before streaming it into the WASM compiler. Serve `.wasm.gz` as gzip data without adding `Content-Encoding`; the client handles decompression.

No SSH gateway backend is needed for the static terminal deployment. Tasks, messaging and optional profile sync use the separate coordination hub. It still uses Tailscale coordination and relay services. Standard SSH also travels through Tailscale, directly to a device or through an approved subnet route.

## Data and session behavior

- Credentials are encrypted at rest in IndexedDB. Unlocking makes them available to this app in memory.
- Vaults belong to their browser origin. Moving computers or origins uses encrypted backup/restore or optional [profile sync](docs/profile-sync.md), plus authorization of a new browser Tailscale identity.
- One browser tab owns an unlocked vault; use terminal tabs and groups inside the app.
- Closing the page disconnects SSH. Remote tmux sessions continue. The encrypted workspace remembers tabs, groups, and server filters and restores them after unlocking; plain SSH tabs open fresh shells. Local scrollback does not survive a reload.
- SFTP supports folder selection and uploads; the Files workspace remains hidden. No local TCP forwarding/listener or native ssh-agent integration is provided. Profile sync is optional and does not copy browser Tailscale identities or current window layout.
- Closing a task terminates its agent tmux sessions and preserves shared board history. Full terminal output/private agent chats are not archived.

See [the project overview](docs/project-overview.md) for the complete architecture and current limits, [the static-client guide](docs/static-client.md) for architecture, controls, deployment, and validation limits, [tasks](docs/tasks.md) for the agent-team hub, and [appearance references](docs/appearance.md) for theme sources.

## Development and tests

```sh
npm test
npm run test:hub
npm run test:links
npm run test:context-menu
npx playwright install chromium
npm run build
npm run test:browser
./scripts/build-wasm.sh --test
npm run build:static
npm run test:static
```

The static browser suite uses a separately compiled test-only WASM transport connected to a local SSH fixture. That transport is excluded from the production module. It tests real SSH authentication, terminal input, tmux flows, grouping, encrypted storage, task mirroring against an in-memory hub, SFTP file operations, and UI behavior without requiring your tailnet credentials. `npm run test:hub` runs the Go hub and `tt` CLI tests.

To test the original client with Safari's browser engine:

```sh
npx playwright install webkit
TEST_BROWSER=webkit npm run test:browser
```

A deployed-site smoke check is available as `node tests/deployed-browser.mjs https://your-site.example`; it uses a temporary browser vault and stops before authorizing a real Tailscale login.

## Repository layout

- `client/`: browser UI, encrypted vault, SSH orchestration, and WASM loader.
- `wasm/`: Tailscale-derived Go WASM source and the SSH extensions.
- `shared/`: remote tmux command generation.
- `scripts/`: reproducible WASM builds, static packaging, deployment, and the apple/container dev stack.
- `tests/`: unit, SSH integration, browser, and real-tmux checks.
- `hub/`: the Go task hub (`tailterm-hub`) and agent CLI (`tt`). See [tasks](docs/tasks.md) and [`hub/README.md`](hub/README.md).
- `server/`: the original optional Node.js SSH gateway, retained for development and migration. See [server-backed mode](docs/server-backed.md).

Generated builds, vault files, credentials, local Cloudflare state, and test screenshots are excluded from version control. Tailscale and Go notices are retained in `wasm/`; static packaging includes the licenses of linked dependencies.

`npm run test:tmux-menu` additionally requires `tmux` and Python 3 on PATH. It uses a separate temporary tmux server to verify actual menu selection in Chromium and WebKit without changing existing sessions.
