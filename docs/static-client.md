# Tailterm browser distribution

The static build targets `https://tailterm.tailarr.com/`. It needs an HTTPS static file host, Tailscale coordination/relay services, and reachable destination SSH servers. It has no Tailterm API, websocket SSH gateway, central vault, or application process. Ordinary SSH connections use Tailscale network transport; public/LAN raw TCP outside that transport is not available to a normal webpage.

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

The static client uses its own Cloudflare Pages project, `tailterm`, in the website’s account. After building and testing a clean committed main checkout, publish with `npm run deploy:static`. It verifies the release inventory before invoking Wrangler for the existing project. The custom domain is `tailterm.tailarr.com`, pointing to `tailterm.pages.dev`. No Tailarr product server is involved.

Browser storage is origin-specific: existing localhost profiles and credentials do not automatically appear on the public domain. Use Backup & restore to move the encrypted profiles; authorize a new Tailscale browser identity on the public domain.

## Browser vault and identity

IndexedDB stores an AES-256-GCM encrypted envelope. PBKDF2-HMAC-SHA256 derives a non-extractable key using a random 16-byte salt and 600,000 iterations; each save uses a new random 12-byte nonce. Format/KDF metadata is authenticated. The password and unencrypted credentials are never stored in localStorage or shipped in the bundle. Appearance settings alone use localStorage.

One browser tab owns the unlocked vault using Web Locks. This avoids lost updates and concurrent use of the same node identity. Use terminal tabs inside that workspace. Lock or close it before opening the vault in another browser tab. The workspace locks after 15 minutes without local pointer/keyboard interaction by default. Appearance → Security offers 5, 15, 30, or 60 minutes. The deadline is checked on focus, visibility changes, page restoration, and before accepting fresh input, so the first interaction after sleep cannot renew an expired deadline. Closing/locking disconnects SSH; remote tmux sessions remain running.

The custom WASM registers a non-ephemeral browser node and accepts approved subnet routes. Each browser has its own device identity; Tailscale policy, device approval, revocation and expiration still apply. Persistence across an actual tailnet restart and live subnet routing require verification against your tailnet.

Backup & restore exports encrypted server profiles, credentials and bookmarks. It excludes Tailscale node state. Restore preserves this browser's current node identity and replaces its profiles/keys/bookmarks after confirmation. Browser storage can be cleared or evicted, so keep an encrypted backup. Same-origin application code can use unlocked secrets; the dedicated subdomain avoids sharing that origin with the main site.

### Forgotten passphrase

On the locked screen, choose **Forgot passphrase? Reset this browser’s vault**, type `RESET`, and choose **Delete local vault**. No old passphrase is required. This permanently removes this website’s local encrypted vault, including saved servers, SSH keys/passwords, bookmarks, and Tailscale device identity. Close or lock any other tab using the vault first. Create a new vault and authorize Tailscale again afterward.

Remote servers and tmux sessions, other browser/origin vaults, appearance preferences, and downloaded backups are preserved. Backups still need their original passphrase; reset does not decrypt or recover forgotten credentials.

### Move the original server vault

```sh
npm run export:vault -- data/vault.enc /path/to/tailterm-backup.json
```

This offline command prompts for the existing passphrase without echoing it and writes a portable encrypted backup using the same passphrase. It refuses to overwrite an existing output file and leaves the old vault unchanged. Create/unlock a browser vault, open Backup & restore, select the file, and supply the backup passphrase. The browser authorizes its own Tailscale device; the former server-stored node is not cloned. Migration has not been performed against your actual vault.

## WASM implementation

`wasm/upstream_js.go` derives from Tailscale v1.102.3's `cmd/tsconnect/wasm/wasm_js.go`; `wasm/ssh_js.go` implements the extensions. `scripts/build-wasm.sh` downloads a checksum-verified release tarball and uses that release's Go toolchain. `wasm_exec.js` comes from the same toolchain as the binary. The build retains the full dependency set, including route support, instead of applying the smaller upstream console's feature omissions.

Extensions: configurable ports, SSH private keys (including passphrase-protected keys), password authentication, keyboard-interactive challenges, strict SHA256 host fingerprints, separate stdout/stderr, exact command execution, exit statuses, binary terminal output, bounded input queuing, authentication timeouts, asynchronous close/resize, and release of per-connection JavaScript callbacks. Standard SSH first-use host trust is remembered; mismatches fail closed. Only a peer explicitly advertising Tailscale SSH on port 22 uses Tailscale's identity-based host trust.

Temporary passwords are cached in memory while an interactive connection uses them and for up to five minutes after its final connection closes. Background session queries can use that grace period but cannot extend it. Lock, forget-device, and pagehide clear this cache; late connection callbacks cannot repopulate a cleared cache. Cached key selections refer to the encrypted vault by ID instead of duplicating private-key material. JavaScript cannot guarantee immediate memory zeroization. Passwords persist only when Remember is checked. Keys are encrypted when explicitly imported; a key's association with a server is saved after successful authentication. Server endpoint changes during login prevent credentials being saved onto the changed profile.

For standard SSH without a saved credential, connect opens the key/password/interactive-login chooser before contacting the server. The browser cannot read your computer's `~/.ssh/config`, private keys, or SSH agent: use the actual hostname or IP and explicitly import a key or enter a password. Background discovery waits for a trusted host and a usable credential. Authentication banners appear as plain text, including server approval instructions. An EOF identifies whether the server closed before host verification or during sign-in; it does not automatically trigger password retries. Cancelling interactive verification is reported as a cancelled sign-in.

## Terminal interactions

### Workspace continuity

After unlocking, the browser restores previously connected tabs, active tab, group layout, split proportions, and tab order from the encrypted vault. Server endpoints must still match the saved workspace. Plain SSH tabs start a fresh shell; tmux tabs resume the existing remote session. SSH passwords that were not remembered must be entered again. No vault key or passphrase is stored to bypass unlocking. Workspace changes save shortly after interaction; a refresh during the final fraction of a second of a change may retain the preceding layout.

Verified tmux targets include the session ID and creation timestamp. Rename, resume, and reconnect can follow the same session after its name changes, and reject a missing or stale target. Remote rename changes are reflected when the session list is refreshed.

SSH keepalives detect broken connections. Previously connected tmux tabs retry transient transport failures up to five times with increasing delays, reusing their terminal and scrollback. Authentication, changed host keys, missing sessions, and normal remote exits require manual action. Closing a tab or locking cancels retries. Network recovery and returning after sleep retry connections already known to be interrupted; returning to the page does not reconnect healthy sessions. Automatic retries and waiting for network recovery do not raise **Needs attention** notifications. Failures requiring manual action still do, and reconnecting clears that warning. The inactivity lock remains in force.

Drag to the left/right edge of a tab to reorder it; a highlighted edge previews placement. Drop in the center to group. The tab menu also offers **Move left** and **Move right**. Groups move as a unit.

**Diagnostics** shows network, Tailscale, SSH, host-verification, and tmux status plus the last error and retry state. **Save output** downloads the terminal's currently retained scrollback as UTF-8 text. It does not retrieve remote tmux history that the browser has never received.

### Image uploads

Drop up to ten images onto a connected terminal to review an upload. Tailterm queries the verified tmux session's active pane directory; the destination is visible and editable. Plain SSH shells require an explicit absolute folder. Each image is limited to 20 MiB and each batch to 100 MiB.

Uploads use SFTP directly over the existing browser SSH authentication path and Tailscale transport; the destination SSH server must offer SFTP. Files are written in chunks to private temporary files, then renamed into place using non-overwriting SFTP rename. Existing filenames are rejected. Cancellation or a broken connection can leave a temporary `.tailterm-upload-*.part` file if remote cleanup cannot complete.

After success, Tailterm can insert shell-quoted paths into the same active terminal without pressing Enter, or leave the paths available to copy. The upload dialog never runs the image or submits a terminal command. Other panes are not used for path insertion if focus changes.

### Backups and recovery

Backups are available on demand without automatic reminders. **Backup & restore** displays the last backup-download date. Downloading a backup does not prove it was retained elsewhere; keep the file and its passphrase. Workspace layout and Tailscale node identity stay browser-local and are excluded from portable backups.

- Use a tmux tab’s **...** session menu, or **Rename** beside a session in the launcher, to rename the real remote session. The dialog starts with its current name and accepts 1–64 letters, numbers, underscores, or dashes. Tmux rejects duplicates. Attached terminals and running processes continue; matching open-tab labels and encrypted bookmarks update after success. Other devices see the remote name when they refresh their session list. Sign in to the server once before using session management.

- Tmux launch and resume explicitly enable UTF-8 output, even when SSH does not provide a UTF-8 locale. After updating from an older client, reload the page and resume the existing tmux session to restore symbols and punctuation that appeared as underscores.

- **Discover devices** is the entry point for saving servers. It opens a centered modal with search and a live device list. Selecting a device opens its SSH setup in the modal; selecting a saved device edits its existing profile. The launcher also provides Discover, including in fullscreen. Connections may use ordinary SSH or Tailscale SSH; hosts behind approved subnet routes can still be reached using an edited connection profile.
- **Expand** fills the browser window while retaining its tabs and address bar. **Restore** returns to the normal workspace. **Fullscreen** remains a separate display-filling mode. Tab/pane dragging uses pointer capture within the terminal, including fullscreen; dropping outside a valid target or cancelling a drag leaves the group unchanged.

- Drag a terminal tab onto another tab to combine their live terminals into one group. Dropping another tab onto the group splits its largest pane; dropping on a pane header splits that pane. Whole groups can be combined in the same way. Layouts use a binary split tree inspired by [Hyprland's dwindle layout](https://wiki.hypr.land/Configuring/Layouts/Dwindle-Layout/), with stacked panes on narrow screens.
- Drag a pane's header to the “Drop here to make a separate tab” target or empty tab-bar space to remove it from the group. The header's **↗** button does the same without disconnecting. **▦** in the terminal toolbar offers grouping and separation buttons, including for touch and keyboard users.
- Drag a divider to resize neighboring panes. Focus a divider with Tab and use its corresponding arrow keys (Shift makes larger adjustments). Double-click or press Enter for a 50/50 split; Home/End move to its allowed limits. Minimum sizes keep panes usable, with scrolling when necessary. Ratios are retained when switching groups and reconnecting.
- Clicking a pane, or hovering when focus-follows-mouse is enabled, makes it the destination for keyboard input, search, copy/paste, clear and reconnect. The border and header identify the focused pane. The group's tab shows the pane count; its × closes only the focused pane. Individual pane headers also have close buttons. A group with one remaining terminal becomes an ordinary tab. Grouping preserves connections and scrollback, works in fullscreen, and is restored with the encrypted browser workspace after unlocking; it does not create remote tmux splits.

- Sidebar servers are multi-select filters for open sessions. Click one to filter to it, then toggle others to show their sessions together; **All** clears the filter. Deselecting every server shows an empty state. Filtering never disconnects terminals or removes group members: mixed-server groups show only matching panes and recover their full layout when shown again. The browser vault remembers the filters across refreshes. Session-switch commands and the mobile session list follow the same filters. Starting a connection through **+** adds its server to the selected filters.

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

## Everyday workspace controls

- **Commands** (Ctrl/Cmd + Shift + P) searches session switching, rename, grouping/splitting existing terminals, reconnect, uploads, output download, diagnostics, and settings. Arrow keys select a result; Enter runs it. The palette does not execute arbitrary shell commands.
- **Uploads** accepts dropped images, clipboard screenshots, or the palette’s image picker. Hide the upload dialog with × or Escape and reopen it from the persistent Uploads button. Cancel explicitly stops the active transfer. Uploads continue while the page stays open; locking, refreshing, or closing the page stops them. Completed paths are inserted only when the review dialog and original destination terminal are still active; otherwise reopen Uploads to copy them.
- Inactive tabs show **New output**, **Bell**, **Needs attention**, or **Command finished**. Completion requires the remote shell to emit an OSC 133 command-finished marker; silence is not treated as completion. Selecting a tab clears its indicator.
- On narrow screens, an open terminal uses the available viewport with a session switcher and Esc, Tab, Ctrl-C, and arrow controls. **Select text** opens retained output in a native selectable text area. The viewport adapts when the on-screen keyboard changes its height.
- **Forget this device** requires typing FORGET and deletes this origin’s local vault and appearance settings. It keeps remote tmux sessions and downloaded backups. Removing the browser from the tailnet is a separate action in the [Tailscale Machines page](https://login.tailscale.com/admin/machines); follow [Tailscale’s removal instructions](https://tailscale.com/kb/1260/device-remove). The dialog links to device management and offers a backup first.

### Optional local history and browser-tab activity

Each tmux terminal has a small **Scroll off / Scroll on** toggle in its upper-right corner. It starts off; ordinary scrolling keeps working normally. Click to turn power scrolling on and fetch a local snapshot of up to 5,000 history lines plus the visible pane, bounded to 1 MiB. The view uses the terminal’s font and spacing and scrolls locally. Click again (or press Escape) to return to live input. Each activation fetches fresh history. The Commands palette also offers the toggle. History stays in memory and is discarded when switched off, closed, locked, or refreshed. Live output continues behind the snapshot. A program’s private history outside tmux’s retained pane buffer is not available through this feature.

New-output activity compares rendered text instead of raw SSH traffic. Identical redraws, cursor-only updates, and changes limited to the usual bottom tmux status row do not count. Background comparisons are throttled; bells, explicit command completion, and connection errors take priority. The browser title shows an unread-session count and the highest-priority activity. Returning to a session clears its indicator. Applications that visibly change their content may still generate activity; this is not a semantic notification feed.

### Keyboard navigation between grouped panes

Use **Option + Shift + arrow** on Mac, or **Ctrl + Alt + arrow** on Windows/Linux, to focus a pane in that direction within the current group. Navigation follows the visible layout, including portrait stacking, and stops at the outside edge. It does not wrap or switch groups. These shortcuts do not send keystrokes to SSH and are inactive in dialogs and ordinary form fields. The Commands palette also includes **Focus pane left/right/up/down**.

### Local voice dictation

With a terminal connected, press **Option + Space** or **Shift + Option + V** on Mac (**Alt + Space** or **Alt + Shift + V** elsewhere), click **Mic**, or use **Voice dictation** in Commands. The model warms up after unlock without accessing the microphone. Opening the compact popup starts recording automatically, with a waveform driven by microphone volume and a matching recording progress bar. Press **Stop** to transcribe and insert directly into the selected terminal; the popup closes automatically. There is no live transcript or review step. Insertion never presses Enter; newlines and tabs become spaces. Closing the popup or pressing Escape cancels without inserting anything. Recording stops automatically at 60 seconds.

If recording fails, **Record again** allows a retry. The destination is pinned when the popup opens; if it changes or disconnects, the text is preserved in the popup for copying rather than sent to another terminal.

The English Whisper tiny.en q8 model runs locally in a dedicated WASM worker. The pinned model and runtime are served from Tailterm’s own origin; the browser does not fetch speech assets from Hugging Face. The build downloads missing source files from the pinned upstream revision and checks their SHA-256 hashes against `client/speech-model-manifest.json`. Files are packaged in parts of at most 12 MiB to fit Cloudflare Pages limits. The worker verifies part and complete-file hashes before inference, including on every cache read. Corrupted cached files are discarded; bad network bytes fail closed. Model files are cached in `tailterm-speech-v1`. Audio and transcripts are not persisted or uploaded. Closing the popup, locking, or leaving the page stops recording and terminates the worker. Browser storage eviction may require another model download. HTTPS and microphone permission are required.

`node tests/speech-browser.mjs` verifies real browser WASM inference with the public Transformers.js JFK speech fixture (`.build/speech-fixture.wav`), the verified model cache, same-origin GET requests only, and an AudioWorklet under the production CSP. The static browser suite uses a fake microphone and deterministic speech worker for permission denial, cancellation, automatic recording, waveform response, shortcut handling, and direct Stop insertion. The pinned ORT runtime uses basic graph optimization because its extended QDQ pass fails on Whisper's tied embeddings.

Fetch the public test recording before the speech test:

```sh
curl -fL https://huggingface.co/datasets/Xenova/transformers.js-docs/resolve/main/jfk.wav -o .build/speech-fixture.wav
node tests/speech-browser.mjs
```

Pass a deployed origin as the optional argument to run the same inference and microphone-worklet checks against published assets. The recording is decoded in the test browser; it is never uploaded to that origin.

The dictation waveform responds to microphone volume. Option + Space is handled when the browser receives it; an OS launcher using that combination takes precedence. Shift + Option + V remains available.

`node tests/dictation-safety-browser.mjs` checks single insertion without Enter, preservation of unsent text when the destination changes, and cancellation during transcription.

### Popup presentation

Dialogs share theme colors, compact fields, consistent headers and action rows, and sizes suited to their task. Advanced SSH options, restore controls and extended help expand on demand. Destructive actions still require explicit confirmation; their themed confirmation preserves the underlying form when canceled. Phone layouts use a single column with touch-sized controls and scroll inside the dialog.

`npm run test:static` includes desktop/phone checks for the main popup families and captures screenshots in `.build/popup-*.png`. `node tests/popup-prompts-browser.mjs` covers host trust, SSH credentials, verification codes and confirmation dialogs in dark and light themes. Clipboard, upload, rename, vault reset and mobile flows remain covered by the browser regression suites.

### Generate or copy an SSH key

Open **SSH keys** in the sidebar, enter a name (for example, TrueNAS), and choose **Generate & save key**. Tailterm generates an Ed25519 key locally using the browser WASM runtime and saves the private key in the encrypted vault. No terminal command or separate computer is needed. Generated private keys rely on the vault's encryption; they do not have a separate key passphrase.

Choose **Copy public key** beside the saved key and paste it into the destination account's SSH public key field (for TrueNAS, edit the user account). The same button derives the public key from imported private keys, including passphrase-protected keys already in the vault. If clipboard access is blocked, the public key is displayed and selected for manual copying. Only the public key is displayed or copied.

Then open **Edit server**, choose **Standard SSH · key or password**, select the key and save. Use **+ → Open plain SSH shell** to test. **Import an existing key** still accepts private key files or pasted keys and an optional existing key passphrase. In the optional gateway deployment, generation and private-key storage take place on the gateway server.

### Clickable terminal URLs

Hover over an HTTP or HTTPS URL to see its destination. **Command-click** on Mac or **Ctrl-click** on Windows/Linux opens it in a new browser tab. Plain clicks retain normal terminal behavior. Wrapped URLs and OSC 8 labeled hyperlinks are supported; for labeled links the hover hint shows the destination. Links in cached power-scroll history use ordinary clicks, taps, or keyboard activation. Opening a link uses the browser's network connection.

When a remote application such as tmux enables mouse reporting, right-click opens its menu without the browser menu appearing over it. **Shift-right-click** opens the browser menu and keeps that click out of the remote application. The browser menu remains available normally in plain shells without mouse reporting, cached history, and the rest of the page.

Tmux menus use a hold-and-release gesture: hold the right mouse button, move onto an item, then release to select it. Tailterm preserves this gesture when tmux changes mouse reporting modes while opening its menu.

## Release and account security

See [security operations](security-operations.md) for the repository checks, release verification, account controls, and the pending tailnet/SSH access review. `npm run build:static` writes `dist-static/release.json` with the source commit, dirty-source flag, package-lock hash, Node version, and a SHA-256 inventory of shipped assets. Commit first, build, test, then run `npm run verify:release` before deployment. Verify refuses dirty, stale, or modified builds. Keep the manifest alongside the deployment record, independently of the hosted copy; hashes hosted beside an app do not protect against compromise of that app’s origin.
