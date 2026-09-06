# Tailterm

A browser SSH terminal that connects through Tailscale. Run it from a static website: SSH, the Tailscale node, and the encrypted vault run in your browser through WebAssembly.

**Try it: [tailterm.tailarr.com](https://tailterm.tailarr.com)**


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

Deploy only `dist-static/` to an HTTPS static host. The production site uses Cloudflare Pages:

```sh
npx wrangler login
npx wrangler pages project create tailterm --production-branch main
npx wrangler pages deploy dist-static --project-name tailterm --branch main
```

Choose your own project name when deploying a fork. Connect your custom domain through Cloudflare Pages and DNS. The included `_headers` file configures security and cache headers.

The approximately 37.5 MB WASM module ships as an approximately 8.5 MB gzip asset, below Cloudflare's 25 MiB per-file limit. The browser decompresses it before streaming it into the WASM compiler. Serve `.wasm.gz` as gzip data without adding `Content-Encoding`; the client handles decompression.

No application backend is needed for the static deployment. It still uses Tailscale coordination and relay services. Standard SSH also travels through Tailscale, directly to a device or through an approved subnet route.

## Data and session behavior

- Credentials are encrypted at rest in IndexedDB. Unlocking makes them available to this app in memory.
- Vaults belong to their browser origin. Moving from localhost to another domain requires encrypted backup/restore and authorization of a new browser Tailscale identity.
- One browser tab owns an unlocked vault; use terminal tabs and groups inside the app.
- Closing the page disconnects SSH. Remote tmux sessions continue. The encrypted workspace remembers tabs, groups, and server filters and restores them after unlocking; plain SSH tabs open fresh shells. Local scrollback does not survive a reload.
- This version does not provide SFTP, port forwarding, native ssh-agent integration, or automatic cross-device synchronization.

See [the static-client guide](docs/static-client.md) for architecture, controls, deployment, and validation limits, and [appearance references](docs/appearance.md) for theme sources.

## Development and tests

```sh
npm test
npm run test:links
npx playwright install chromium
npm run build
npm run test:browser
./scripts/build-wasm.sh --test
npm run build:static
npm run test:static
```

The static browser suite uses a separately compiled test-only WASM transport connected to a local SSH fixture. That transport is excluded from the production module. It tests real SSH authentication, terminal input, tmux flows, grouping, encrypted storage, and UI behavior without requiring your tailnet credentials.

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
- `scripts/`: reproducible WASM builds, static packaging, and deployment helpers.
- `tests/`: unit, SSH integration, browser, and real-tmux checks.
- `server/`: the original optional Node.js SSH gateway, retained for development and migration. See [server-backed mode](docs/server-backed.md).

Generated builds, vault files, credentials, local Cloudflare state, and test screenshots are excluded from version control. Tailscale and Go notices are retained in `wasm/`; static packaging includes the licenses of linked dependencies.
