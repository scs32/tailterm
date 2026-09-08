# Phone sign-in by QR code — Tailterm proposal

September 8, 2026; owner request #61. Research baseline: `tasks-hub` at
`9d87aabf4ac709c4dbca1760952c7a403c6a4936`. No implementation or deployment.

## Finding and proposed flow

**Yes.** Tailscale documents scanning a QR code on a trusted phone, completing
identity-provider login there, and automatically authenticating the original
device. Its hosted login page already offers a QR link, providing a possible
workaround before Tailterm adds its own display. See
[Add a device using a QR code](https://tailscale.com/docs/features/access-control/device-management/how-to/set-up-qr-code).
The native CLI also supports encoding its web login URL with `--qr`; that is
supporting evidence, not a command to run against this project's existing hosts.
[CLI reference](https://tailscale.com/docs/reference/tailscale-cli/up).

Recommended Tailterm experience:

1. Choose **Connect Tailscale** on the computer.
2. Tailterm shows **Scan to sign in on your phone**, a locally rendered QR code,
   the existing **Continue to Tailscale** link, and a waiting status.
3. Scan with the phone camera and complete normal Tailscale/identity-provider
   login, including MFA and tailnet selection when requested.
4. Keep the computer tab open. Its existing Tailscale client receives the result;
   dismiss the sign-in dialog when appropriate, show connection status, and
   continue any pending discovery/connection flow.

The computer's Tailterm browser node joins the tailnet. The phone serves as the
authentication browser; this does not enroll the phone, transfer node keys,
unlock the computer's vault, or replace later SSH authentication. The official
QR instructions require a scanner and web browser, not an already enrolled
phone. Particular organization/identity-provider policies remain applicable.

## Repository evidence and interface contract

- `client/main.js:2169`, `safeAuthURL`: validates HTTPS and exact
  `login.tailscale.com` hostname before displaying the authorization link.
- `client/main.js:2263`, `notifyBrowseToURL`: already receives the exact login
  URL for this browser node. Encode this URL without reconstructing or modifying
  its path/query. A generic Tailscale homepage URL would lose the enrollment link.
- `wasm/upstream_js.go:335`: forwards Tailscale's `BrowseToURL` to that callback;
  `login()` starts the existing interactive backend login. No new OAuth flow,
  inbound callback server or WASM API is indicated by this inspection.
- `client/main.js:2231`, `notifyState`: already tracks `Running`, updates the
  connection indicator, refreshes the task hub and starts connection recovery.
- `client/main.js:1976`, `waitForTailscale`: an SSH-initiated sign-in waits up to
  180 seconds. That is a local connection timeout, not evidence for Tailscale's
  authorization-link expiry. A user finishing later may need to retry SSH.
- `client/main.js:2004`: a shared dialog hosts login and discovery. The current
  header-only sign-in does not always dismiss its login dialog on `Running`;
  implement explicit dialog ownership and completion handling.
- `package.json`: no QR renderer is declared. Bundle a small reviewed encoder
  locally, including its license in static packaging, and render with crisp
  modules, a white quiet zone and scan-friendly contrast across themes.

**Input:** latest validated URL from the current runtime's login callback and
runtime state changes. **Output:** a QR and ordinary link for that same URL;
connected, waiting-for-approval, or retry guidance derived from actual state.
Keep the pending URL in transient UI state. Replace it when a newer callback
arrives; do not render an older async result after dialog replacement or locking.

**Ownership:** the existing Tailscale runtime owns registration, key material,
authorization progress and persistence. The frontend owns presentation only.
Continue normal persistence through the existing encrypted vault. Do not call
`tailscale up`, alter TrueNAS networking, add auth keys, or transmit node identity
to the phone to implement this feature.

**Failure/compatibility:** retain same-device login if QR rendering fails. Show
pending approval instead of treating completed IdP login as guaranteed network
access. Clear transient QR content on dismissal/lock/completion. Do not claim
dismissal revokes the server-side link. A retry requests interactive login and
uses whichever URL Tailscale emits; do not promise a new URL every time or invent
an expiry countdown. Preserve ongoing discovery, connection retry and vault
ownership behavior. Keep the existing HTTPS/hostname check; reject embedded
URL credentials and unexpected ports as part of a focused validator review.

Render locally: an external QR-image service would receive the authorization
URL. Do not place real login URLs or QR images in logs, test fixtures, screenshots,
board posts or research artifacts. This is ordinary handling of a transient
authorization link, not a proposal for a new approval process.

## Effort and acceptance

Planning estimate: **about 1–2 engineering days** for frontend integration,
focused browser checks and a real phone acceptance pass, assuming deployment
access. No hub changes are expected. The main acceptance dependency is the owner's
actual phone/identity-provider/tailnet policy, not documentation availability.

Use synthetic URLs and a mocked IPN callback/state source in isolated browser
contexts. Verify the rendered QR decodes exactly to the supplied URL; replacement,
malformed URL rejection, fallback link, scan contrast, narrow layout, dismissal,
lock, stale callbacks, approval states, `Running` cleanup and the existing
180-second connection-timeout behavior. Exercise Chromium and WebKit. These tests
must not authenticate test-created nodes into the live tailnet.

After implementation is authorized, one owner-assisted real phone scan should
verify the intended computer browser node/account, IdP/MFA success, any approval
requirements, and automatic computer transition. Public documentation establishes
feasibility but does not replace that end-to-end check of Tailterm's pinned WASM
build. Do not record the live QR in artifacts.

See [platform findings](tailscale-phone-login-findings.md) for independently
researched Tailscale sources and limitations. Research verification consists of
repository and public-source inspection; no live authentication, browser-node
registration, implementation tests, service changes or production deployment
were performed.
