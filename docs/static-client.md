# Tailserve browser distribution

The static build targets `https://tailterm.tailarr.com/`. It needs an HTTPS static file host, Tailscale coordination/relay services, and reachable destination SSH servers. It has no Tailserve API, websocket SSH gateway, central vault, or application process. Ordinary SSH connections use Tailscale network transport; public/LAN raw TCP outside that transport is not available to a normal webpage.

## Build and serve

```sh
npm ci
npm run build:wasm
npm run build:static
npm run preview:static
```

Preview uses `http://127.0.0.1:4318/`, a browser secure-context exception for local development. Deploy the **contents of `dist-static/` only**. Never deploy the source directory, `data/`, or `.build/`. `deploy/Caddyfile` is an example static HTTPS host; `_headers` is provided for compatible static hosting platforms. Hashed assets cache for a year; HTML revalidates. JavaScript and CSS have precompressed alternatives. WASM ships only as a `.wasm.gz` file (about 8.5 MB); the browser fetches it, decompresses it with `DecompressionStream`, and streams it to the WASM compiler. Serve that file as gzip data without adding a `Content-Encoding` header. This keeps every uploaded file below Cloudflare’s 25 MiB limit. No service worker caches a stale credential-handling application release.

`npm run build` and `npm start` retain the original server-backed version for migration and regression checks. They are not the static deployment.

## Cloudflare deployment

The static client uses its own Cloudflare Pages project, `tailterm`, in the website’s account. Publish with `npx wrangler pages deploy dist-static --project-name tailterm --branch main`. The custom domain is `tailterm.tailarr.com`, pointing to `tailterm.pages.dev`. No Tailarr product server is involved.

Browser storage is origin-specific: existing localhost profiles and credentials do not automatically appear on the public domain. Use Backup & restore to move the encrypted profiles; authorize a new Tailscale browser identity on the public domain.

## Browser vault and identity

IndexedDB stores an AES-256-GCM encrypted envelope. PBKDF2-HMAC-SHA256 derives a non-extractable key using a random 16-byte salt and 600,000 iterations; each save uses a new random 12-byte nonce. Format/KDF metadata is authenticated. The password and unencrypted credentials are never stored in localStorage or shipped in the bundle. Appearance settings alone use localStorage.

One browser tab owns the unlocked vault using Web Locks. This avoids lost updates and concurrent use of the same node identity. Use terminal tabs inside that workspace. Lock or close it before opening the vault in another browser tab. The workspace locks after 15 minutes without local pointer/keyboard interaction. Closing/locking disconnects SSH; remote tmux sessions remain running.

The custom WASM registers a non-ephemeral browser node and accepts approved subnet routes. Each browser has its own device identity; Tailscale policy, device approval, revocation and expiration still apply. Persistence across an actual tailnet restart and live subnet routing require verification against your tailnet.

Backup & restore exports encrypted server profiles, credentials and bookmarks. It excludes Tailscale node state. Restore preserves this browser's current node identity and replaces its profiles/keys/bookmarks after confirmation. Browser storage can be cleared or evicted, so keep an encrypted backup. Same-origin application code can use unlocked secrets; the dedicated subdomain avoids sharing that origin with the main site.

### Move the original server vault

```sh
npm run export:vault -- data/vault.enc /path/to/tailserve-backup.json
```

This offline command prompts for the existing passphrase without echoing it and writes a portable encrypted backup using the same passphrase. It refuses to overwrite an existing output file and leaves the old vault unchanged. Create/unlock a browser vault, open Backup & restore, select the file, and supply the backup passphrase. The browser authorizes its own Tailscale device; the former server-stored node is not cloned. Migration has not been performed against your actual vault.

## WASM implementation

`wasm/upstream_js.go` derives from Tailscale v1.102.3's `cmd/tsconnect/wasm/wasm_js.go`; `wasm/ssh_js.go` implements the extensions. `scripts/build-wasm.sh` downloads a checksum-verified release tarball and uses that release's Go toolchain. `wasm_exec.js` comes from the same toolchain as the binary. The build retains the full dependency set, including route support, instead of applying the smaller upstream console's feature omissions.

Extensions: configurable ports, SSH private keys (including passphrase-protected keys), password authentication, keyboard-interactive challenges, strict SHA256 host fingerprints, separate stdout/stderr, exact command execution, exit statuses, binary terminal output, bounded input queuing, authentication timeouts, asynchronous close/resize, and release of per-connection JavaScript callbacks. Standard SSH first-use host trust is remembered; mismatches fail closed. Only a peer explicitly advertising Tailscale SSH on port 22 uses Tailscale's identity-based host trust.

Successful credentials are cached in memory for background session queries. Passwords persist only when Remember is checked. Keys are encrypted when explicitly imported; a key's association with a server is saved after successful authentication. Server endpoint changes during login prevent credentials being saved onto the changed profile.

For standard SSH without a saved credential, connect opens the key/password/interactive-login chooser before contacting the server. The browser cannot read your computer's `~/.ssh/config`, private keys, or SSH agent: use the actual hostname or IP and explicitly import a key or enter a password. Background discovery waits for a trusted host and a usable credential. Authentication banners appear as plain text, including server approval instructions. An EOF identifies whether the server closed before host verification or during sign-in; it does not automatically trigger password retries. Cancelling interactive verification is reported as a cancelled sign-in.

## Terminal interactions

- **Discover devices** is the entry point for saving servers. It opens a centered modal with search and a live device list. Selecting a device opens its SSH setup in the modal; selecting a saved device edits its existing profile. The launcher also provides Discover, including in fullscreen. Connections may use ordinary SSH or Tailscale SSH; hosts behind approved subnet routes can still be reached using an edited connection profile.
- **Expand** fills the browser window while retaining its tabs and address bar. **Restore** returns to the normal workspace. **Fullscreen** remains a separate display-filling mode. Tab/pane dragging uses pointer capture within the terminal, including fullscreen; dropping outside a valid target or cancelling a drag leaves the group unchanged.

- Drag a terminal tab onto another tab to combine their live terminals into one group. Dropping another tab onto the group splits its largest pane; dropping on a pane header splits that pane. Whole groups can be combined in the same way. Layouts use a binary split tree inspired by [Hyprland's dwindle layout](https://wiki.hypr.land/Configuring/Layouts/Dwindle-Layout/), with stacked panes on narrow screens.
- Drag a pane's header to the “Drop here to make a separate tab” target or empty tab-bar space to remove it from the group. The header's **↗** button does the same without disconnecting. **▦** in the terminal toolbar offers grouping and separation buttons, including for touch and keyboard users.
- Drag a divider to resize neighboring panes. Focus a divider with Tab and use its corresponding arrow keys (Shift makes larger adjustments). Double-click or press Enter for a 50/50 split; Home/End move to its allowed limits. Minimum sizes keep panes usable, with scrolling when necessary. Ratios are retained when switching groups and reconnecting.
- Clicking a pane, or hovering when focus-follows-mouse is enabled, makes it the destination for keyboard input, search, copy/paste, clear and reconnect. The border and header identify the focused pane. The group's tab shows the pane count; its × closes only the focused pane. Individual pane headers also have close buttons. A group with one remaining terminal becomes an ordinary tab. Grouping preserves connections and scrollback, works in fullscreen, and lasts for the current browser workspace; it does not create remote tmux splits or survive a page reload.

- The launcher presents server cards, designed for 5–10 servers. Selecting a card checks its remote tmux sessions without prompting for credentials. Start fresh creates/attaches a named session; a blank name becomes a random `tt-` identifier. Resume attaches by the exact remote session ID and reports a missing session instead of silently creating one. The launcher stays available via **+**, including fullscreen.
- Hover focus follows actual mouse movement over the active terminal. It does not steal focus just because a dialog closed over a stationary pointer. Disable it in Appearance if desired.
- Cmd/Ctrl + plus/minus changes terminal font size; Cmd/Ctrl + 0 restores 14px. Plain plus/minus remain shell input. Toolbar controls work too.
- Appearance previews themes, bundled fonts, cursor shape/blink, line height, padding, hover focus, selection copying, and remote clipboard integration. Settings apply to existing and future terminals and survive reloads.
- Copy with Cmd+C or Ctrl+Shift+C. Cmd+X copies selected output; terminal output cannot be deleted. Plain Ctrl+C still interrupts. Shift+drag selects browser terminal text even when tmux handles the mouse. Optional copy-on-selection is off by default.
- tmux copy mode emits OSC 52. The browser accepts UTF-8 clipboard writes (up to 1 MiB), never answers remote clipboard reads, and retains a Copy affordance if browser permissions prevent an automatic write. Clipboard state belongs to its terminal session.
- Native keyboard/context-menu paste and the Paste button share xterm's bracketed-paste path. Embedded control characters (other than tabs and newlines) are removed, so pasted ESC cannot terminate bracketed paste early. Multiline content gets a destination-specific review dialog; changing/closing the destination cancels the pending paste. If browser clipboard read access is unavailable, the Paste button offers a normal text field to paste into.
- Launch enables mouse handling for that tmux session, advertises clipboard capability with `-T clipboard` on tmux 3.2+, and changes a disabled runtime clipboard policy to `external`. It preserves an existing `on` setting and does not edit `.tmux.conf` or replace user key bindings. Older tmux relies on its terminfo `Ms` capability.
- Tooltips support hover and keyboard focus, include shortcut badges, constrain themselves to the viewport, and use the popover layer in fullscreen/dialogs. Escape dismisses them.

## Verification and limits

`npm test` covers vault encryption/tampering, clipboard parsing, shortcuts, naming/shell quoting, and existing real SSH gateway integration. `npm run test:browser` exercises the legacy application UI and actual upstream WASM startup. Build `./scripts/build-wasm.sh --test`, then `npm run test:static` for browser-vault and extended Go SSH checks. The latter serves static files and uses a **separately compiled test-only WASM transport** to reach a local SSH fixture; that bridge is not compiled into the production artifact and does not establish live Tailscale connectivity.

`python3 tests/real-tmux.py /path/to/tmux` checks the generated commands, OSC 52 payload, session mouse setting, and persistence using an isolated real tmux socket and PTY. The local test binary is not installed on any destination server.

Live tailnet authorization, subnet routing, persistent device reauthorization behavior, and deployment to the public hostname remain environment-dependent checks. This client does not provide background browser execution after closure, automatic cross-device sync, native ssh-agent access, local TCP listeners/port forwarding, or an SFTP file browser.
