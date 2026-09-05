# Original server-backed mode

This document describes the earlier Node.js gateway implementation. Some UI descriptions are historical; the current UI uses Discover and the terminal launcher. For the production static app, use [the main README](../README.md).

# Tailterm

A self-hosted, single-owner SSH workspace with Tailscale's official browser WASM client, an encrypted server vault, saved hosts, terminal tabs, and persistent tmux sessions. This first edition uses xterm.js; transport and terminal code are separated enough to evolve toward a Ghostty-like experience. It does not embed Ghostty.

## Run

Requires Node.js 22.12+ and npm.

```sh
npm ci
npm run build
npm start
```

Open **http://127.0.0.1:4317**. Create a vault passphrase of at least 14 characters. No default credentials are provided. The vault is created only when you submit that form.

For development, use `npm run dev`. Tests: `npm test`. Browser checks: `npx playwright install chromium`, then `npm run test:browser` (build first).

## Connect

Add a hostname, SSH username and port, leave **Automatic · Tailscale or standard SSH** selected, then click **Connect**. Discovery also creates automatic profiles. You can explicitly choose **Standard SSH · key or password** or **Prefer Tailscale SSH** when needed.

- For a known tailnet address without saved standard credentials, Tailterm starts Tailscale sign-in and resumes the connection automatically after authorization.
- When an active browser tailnet reports Tailscale SSH support, Tailterm uses the browser WASM client. If that SSH authentication is rejected, it continues with standard SSH in the same terminal tab, preserving the tmux setting and session name. Other failures, including host verification failures, do not trigger credential fallback.
- Devices reporting Tailscale SSH disabled, ordinary network hosts and custom ports use standard SSH. Saved keys/passwords are tried automatically; otherwise an inline prompt lets you choose or import a private key, enter a password, or complete keyboard-interactive verification such as an OTP. Tailterm follows the methods advertised by the SSH server and limits prompted authentication attempts.
- The first standard SSH connection shows the destination's host fingerprint. Verify it through a trusted console and select **Trust & continue**. Later connections reuse the saved fingerprint; unexpected changes fail closed before credentials are offered. You can also enter a verified fingerprint in the profile in advance.
- **Remember in encrypted vault** saves an SSH password only after successful authentication. Without that checkbox the entered password is used only for the connection. Verification-code responses are not saved. Selected keys are associated with the server after successful login. Edit a server to forget its saved password; changing its host, port or username also drops the saved password.

For a trusted host-fingerprint check, run this from the destination's console:

```sh
ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub
```

Use the fingerprint of the host-key algorithm your SSH server negotiates if it differs from Ed25519.

### Connectivity

Tailscale authentication and tailnet network access are different. **Browser Tailscale SSH** runs the official `@tailscale/connect` WASM node and SSH client in your browser. The destination must enable Tailscale SSH and permit this browser identity and SSH username in its policy. It uses port 22 and does not require the application host to join the tailnet. The initial WASM asset is approximately 26 MB uncompressed.

**Standard SSH** uses the backend so private keys remain on the server. The application host must resolve and reach the destination; for a tailnet-only destination, install/sign in to Tailscale on the application host or provide an equivalent route and DNS setup. The browser WASM package does not expose arbitrary TCP tunnels or private-key authentication, so signing the browser into Tailscale alone cannot provide a standard-SSH route for the backend. Connection failures explain this requirement. No system Tailscale installation or tailnet policy is changed automatically.

A lease allows only one browser tab at a time to load the persisted Tailscale node identity. Close the previous tab and wait 45 seconds when switching browsers. A sleeping tab reloads if its lease has expired. SSH agent forwarding, SFTP, password-change requests, and port forwarding are not implemented.

## Tabs and tmux

Selecting a terminal tab synchronizes the sidebar and session checks with that connection. Connection controls live in the terminal launcher; the redundant upper connection banner and command-center heading have been removed. Discover devices is in the left sidebar. Edit server is available in the terminal toolbar, and the SSH destination appears in its status bar. Selecting a sidebar server activates its most recent open tab; if it has none, the app displays a new-connection view and hides other terminals. The tab-bar **+** opens the session launcher without connecting immediately. Its server picker, blank optional name, Start session button, remote-session Resume buttons and plain-SSH option are all inside the fullscreen terminal area. Adding a server is a secondary action. Selecting a sidebar server starts a background tmux check; in-flight checks are shared, and errors appear inline without interrupting the active terminal. A live authenticated connection is reused when available; otherwise discovery needs saved credentials or an already connected browser tailnet. Sign in using a session or plain shell when prompted, then discovery retries after SSH connects.

**Open shell** creates a plain SSH connection. **Reconnect** replaces the active connection in the same tab and position, using the latest saved profile. Its local scrollback resets. Closing a background tab leaves the current tab selected; closing the active tab selects its neighbor. Deleting a saved server closes that profile's local connections and removes its bookmarks, without killing remote tmux sessions.

Tabs shrink as more connections are opened, with long names and status text truncated using ellipses. At the minimum readable width, left/right scroll buttons appear. Trackpad horizontal scrolling and the mouse wheel also scroll the tab row. Selecting or opening a tab brings it into view. Focus a tab and use Left/Right or Home/End to select another; hover to see its full connection title. Each tab has a connection number and explicit status. The saved server name updates after a rename. The terminal status bar identifies the original SSH destination; its tooltip includes any terminal-provided title. SSHing onward from inside a shell does not change that original destination. Edits to a profile's host/username apply to new connections and reconnects, not existing shells; the connection hint calls out that difference.

### Start or resume tmux

1. Select a server or terminal tab.
2. Session-name fields start blank. A blank name generates a unique random identifier such as `tt-57c18a091db3e422`; entering a name uses that name. Use the primary **Start session** button in the terminal launcher. Named existing workspaces are resumed; blank-name starts create new workspaces.
3. Tailterm requests `tmux new-session -A -s work`. Tailterm locates tmux in the SSH PATH or common Homebrew, Linux, Nix, MacPorts and per-user installation directories. For other locations, set **tmux executable** in Edit server to the absolute path returned by `command -v tmux` in a working SSH session. Launch and discovery use the same executable resolver. An existing tmux process that fails to start reports its actual error and exit status; it is not described as a missing installation. Names accept 1–64 letters, digits, dashes and underscores.
4. Use a different name for an independent workspace. Opening the same named workspace or bookmark focuses its existing connected tab, instead of attaching a duplicate client. Use Reconnect to replace a connection.
5. Close the terminal tab or detach with Ctrl+B then D when leaving. Remote tmux normally continues running. Use **Open plain SSH shell** in the launcher when you do not want tmux.

A tab's **launch** label records the tmux name requested when that connection started. It does not claim to follow session changes made manually inside tmux. Until a remote query finds the requested session, the launch is marked **unverified** and no bookmark is created. Verification confirms the named session exists; it does not identify an individual client's current pane/session after manual changes.

**TMUX BOOKMARKS** are saved shortcuts, not live remote state. Their × button forgets the shortcut only. **Remote tmux sessions** is a separate, timestamped snapshot from **Check remote tmux**, showing remote window and attached-client counts. Queries prefer the active connection and reuse standard SSH authentication, including one-time passwords/OTP. Without a live connection, standard SSH discovery needs saved credentials. Failed checks are reported as failures rather than as an empty success. Selecting a server refreshes its snapshot automatically. The check/refresh buttons also refresh it; there is no continuous polling.

The **Connections** count measures open SSH connections in this browser, not unique remote sessions. Reloading the page drops local tabs/connections; profiles and bookmarks persist. A remote host reboot or exiting the last tmux pane can end its session.

Use **How tmux works** in the app for the workflow and common shortcuts. Ctrl+B then C creates a tmux window; Ctrl+B then N/P switches windows. tmux windows and panes remain inside the terminal, rather than becoming Tailterm tabs.

### Terminal tools

Tabs provide resizing, 256 colors, 15,000 lines of local scrollback, search, font sizing, clipboard controls, clear, fullscreen and reconnect. Copy uses the active selection. Paste asks before inserting multiple lines. Clear affects the local terminal, not the remote process or tmux history. Font sizing applies to all tabs.

Command/Ctrl + Shift + K opens/focuses a connection with the current launch settings; Command/Ctrl + Shift + F opens search; Command/Ctrl + Shift + L locks the vault. Ordinary Ctrl combinations go to terminal applications.

## Storage and security boundaries

The backend stores one AES-256-GCM encrypted file, `data/vault.enc`, containing server profiles, private keys and key passphrases, optionally remembered SSH passwords, Tailscale node state, and session bookmarks. A random salt and scrypt (N=32768, r=8, p=1) derive the encryption key from the vault passphrase. Writes use a temporary file and atomic rename, directory mode 0700, file mode 0600. Back up the encrypted file; the passphrase cannot be recovered or reset without losing the vault. Keep the passphrase separately.

The unlock passphrase is not stored. The decrypted vault and encryption key exist in server memory while unlocked. Tailscale node secrets also exist in the authorized browser's WASM/JavaScript memory while active; they are not stored in browser localStorage or IndexedDB. Server-side SSH private keys are never sent back to the browser. This is protection for storage at rest, not protection from a compromised application server or authorized browser. JavaScript memory cannot be reliably zeroized; locking clears references and reloads the browser.

Authentication uses random, HttpOnly, SameSite=Strict cookies with an eight-hour absolute lifetime. Login attempts are limited, HTTP mutations and WebSocket upgrades check the configured origin, host headers are checked, host keys are pinned, and CSP disallows framing and inline scripts. Locking ends all live connections and clears all browser sessions for this single-owner vault. Restarting the server locks the vault. Closing a browser alone does not immediately lock the backend; it remains unlocked until explicit lock or the last login expires. An application crash or outage does not kill remote tmux sessions.

No remote hosts, cloud resources, tailnet policy, or user SSH configuration are modified during setup. There is no telemetry or external font/CDN dependency in the UI. Tailscale itself needs its coordination and relay endpoints.

## Remote hosting

The default binds only to loopback. For remote access, place the app behind a TLS reverse proxy, forward WebSocket upgrades, preserve the public Host header, and configure the exact public origin:

```sh
HOST=0.0.0.0 APP_ORIGIN=https://terminal.example.com PORT=4317 npm start
```

The app refuses a non-loopback bind without an HTTPS origin. This setting does not itself enable TLS: the reverse proxy must provide it. Never expose the unencrypted backend port directly. Tailscale Serve is another TLS proxy option. Run one application process with one vault file; multiple workers/replicas and multiple independent users are not supported. Access to the unlocked SSH proxy is equivalent to access to the configured hosts.

A Dockerfile is included. Bind its port only on the Docker host's loopback, configure `APP_ORIGIN` to match your HTTPS proxy, and persist `/app/data` with ownership writable by UID 1000. For server-side tailnet SSH, the container must also have a route and DNS access to those destinations.

## Validation and limits

Automated tests cover encrypted persistence, wrong-passphrase rejection, shell injection rejection, login/origin checks, actual SSH fixture input/output, tmux execution and discovery, host-key mismatch rejection, node-identity lease conflicts, password retry and encrypted persistence, keyboard-interactive OTP, automatic transport choice, and lock behavior. Browser tests cover vault and server forms, desktop/mobile layout, reload persistence, rendered host-trust/password prompts, real SSH terminal input, remembered-password tmux reconnect and discovery, and instantiation of the real WASM module.

Live tailnet authorization and a connection to your own remote tmux host require your account and are not performed by these tests. This is a functional first edition, not a security audit. Split panes, richer connection profiles, SFTP, and a Ghostty-style rendering/interaction pass remain follow-up work.

Upstream references: [Tailscale SSH Console](https://tailscale.com/docs/features/tailscale-ssh/tailscale-ssh-console), [official WASM API types](https://github.com/tailscale/tailscale/blob/main/cmd/tsconnect/src/types/wasm_js.d.ts), [tsconnect package implementation](https://github.com/tailscale/tailscale/tree/main/cmd/tsconnect).
