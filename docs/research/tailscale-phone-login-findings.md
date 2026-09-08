# Tailscale phone login / QR findings

Researched September 8, 2026 for owner request #61 and lead assignment #63.
Research only; no credentials, live vaults, real authentication attempts,
installations, configuration changes or service restarts. Lead owns Tailterm's
local login/WASM integration inspection.

**Recommendation:** yes—offer a QR code for the exact pending Tailscale client
authorization URL. The owner scans it and completes login in a trusted phone
browser while the computer's Tailterm tab stays open. This is an officially
documented Tailscale workflow, not a novel authentication protocol.

## Documented support

Tailscale's [Add a device using a QR code](https://tailscale.com/docs/features/access-control/device-management/how-to/set-up-qr-code)
guide explicitly describes displaying the QR on the device being added, scanning
with a phone/tablet, opening Tailscale Login in that device's browser, choosing
the identity provider and tailnet, and having the original device automatically
log in. It also documents a built-in QR link on the Tailscale login page and
automatic QR display for Apple TV's initial connection.

The scanner prerequisites are a trusted QR-capable device and a browser; the
guide does not require a Tailscale app or existing tailnet connection on the
phone. **Conclusion from that documented flow:** ordinary phone browser internet
access to Tailscale and the identity provider suffices. An organization-specific
IdP access policy could impose additional requirements. Scanning authorizes the
original pending device; it does not install Tailscale or enroll the phone.

The official [`tailscale up` reference](https://tailscale.com/docs/reference/tailscale-cli/up)
also documents `--qr` for encoding the web login URL. The upstream
[v1.102.3 CLI source](https://github.com/tailscale/tailscale/blob/v1.102.3/cmd/tailscale/cli/up.go)
encodes the received `BrowseToURL`/`AuthURL` directly and watches state until
`Running`, treating `NeedsMachineAuth` as a separate condition. This confirms
that a QR is another presentation of the pending client URL. It does not imply
that Tailterm needs to run a native CLI or that its embedded client bundles a
QR renderer.

## Which device and identity are authenticated?

Tailscale binds a user identity to a device through its cryptographic node and
machine identity; private keys remain on the originating device. Embedded
`tsnet` applications can each be their own node even on the same machine.
[Tailscale identity](https://tailscale.com/docs/concepts/tailscale-identity)

For Tailterm, the existing [project overview](../project-overview.md) describes
the browser's own Tailscale identity. The QR should therefore be labelled
**“Connect this browser using your phone”** or similarly precise language. It
does not sign the whole computer's native Tailscale installation into an account,
copy the phone's node identity, or unlock Tailterm's encrypted browser vault.
Those are separate actions and credentials.

Use the correct existing identity provider/account and intended tailnet. Tailscale
delegates SSO and MFA to the IdP and supports passkeys; a QR changes where login
happens, not those requirements. The phone's existing browser session may make
login easier, but skipping fresh MFA cannot be promised.
[Identity providers](https://tailscale.com/docs/integrations/identity),
[MFA](https://tailscale.com/docs/multifactor-auth)

## Policy and connectivity constraints

| Condition | Effect on this flow |
| --- | --- |
| Device approval enabled | The new device cannot exchange tailnet traffic until an Owner, Admin or IT admin approves it. Approval takes effect without a restart. Phone login success can therefore precede usable connectivity. [Device approval](https://tailscale.com/docs/features/access-control/device-management/device-approval) |
| Tailnet Lock enabled | New node keys require a trusted signing node. The documented app signing flow supports macOS, Windows and iOS; mobile QR signing is iOS-only, with Android signing unsupported in the reviewed guide. This separate signing QR opens the Tailscale app and does require a trusted signing device. It must not be confused with the ordinary login QR. Tailnet Lock and device approval are documented as mutually exclusive. [Tailnet Lock](https://tailscale.com/docs/features/tailnet-lock) |
| Account/SSO restrictions | QR cannot bypass membership, consent restrictions or IdP failures. Tailscale specifically documents organization app-consent restrictions for Google Workspace and Entra. [Google](https://tailscale.com/docs/integrations/identity/google-sso), [Entra](https://tailscale.com/docs/integrations/identity/entra) |
| Tailnet access policy | Authentication does not guarantee authorization to reach a particular host/service. Tailscale evaluates user and node identity for access. [Identity and access](https://tailscale.com/docs/concepts/tailscale-identity) |

The general Tailscale documentation establishes these constraints; whether the
particular embedded WASM build exposes every approval/lock health state remains
lead's integration question. Do not promise to resolve those policies within a
QR modal.

## Source evidence: waiting, expiry, and reuse

The upstream [v1.102.3 control client](https://github.com/tailscale/tailscale/blob/v1.102.3/control/controlclient/direct.go)
implements `WaitLoginURL` as a long poll waiting for authentication at the URL.
The [registration protocol](https://github.com/tailscale/tailscale/blob/main/tailcfg/tailcfg.go)
includes the pending `AuthURL` and a `Followup` request tied to registration.
**Inference:** the original client learns completion through its control
connection; it does not need a callback from the phone, shared browser cookies,
a localhost redirect reached by the phone, or a new public Tailterm endpoint.

The [v1.102.3 local backend](https://github.com/tailscale/tailscale/blob/v1.102.3/ipn/ipnlocal/local.go#L4335)
and current upstream `main` both say server-side auth URLs expire after **seven
days**, and reuse a pending URL received less than **six days, 23 hours** ago.
The backend clears its stored URL on transition to `Running`.

This is **source evidence, not a guaranteed public service TTL or a live test**.
No reviewed public guide promises indefinite reuse, post-completion reuse, or
that clicking Retry immediately invalidates an older link. Do not describe the
QR as expiring after 180 seconds, hardcode a countdown, or promise a new URL on
every retry. Use whichever current URL the client emits and handle replacement,
completion and errors. Link lifetime is separate from node-key expiry and
admin-console session expiry, which Tailscale documents separately.
[Key expiry](https://tailscale.com/docs/features/access-control/key-expiry)

## Integration prerequisites and handling recommendations

1. Use the exact current client authorization URL, not the generic login page,
   an IdP redirect copied from another browser, a phone enrollment URL or a
   Tailnet Lock signing URL. Preserve its opaque path/query unchanged. Apply the
   existing HTTPS/Tailscale-host validation that lead identified.
2. Render QR locally with a bundled encoder; retain Open link and Copy link
   alternatives. Do not send the login URL to an external QR-image service.
   Keep sufficient contrast/quiet zone and a usable scan size.
3. Keep the originating browser client alive and subscribed to its state while
   the phone authenticates. Clear the QR and finish the login UI on the matching
   `Running` transition; distinguish pending approval/error from successful login.
   A transport or SSH wait timeout is not evidence the URL expired.
4. Treat the URL/QR as sensitive authorization material. Avoid logs, telemetry,
   persistent history and shared screenshots. Possession alone does not bypass
   IdP login, but it exposes a pending node-authorization transaction. Hiding the
   QR is not proof the server has revoked the URL. These are implementation
   recommendations derived from the cross-device authorization behavior.
5. Tell the owner to keep the Tailterm tab open and verify the intended device,
   account and tailnet on the phone. The computer still receives tailnet access;
   phone authentication is not a way to make an untrusted computer trustworthy.

## Verification and remaining dependencies

Verified by reading current official documentation and public upstream source,
including the project's documented v1.102.3 version. No real device/tailnet was
used. The existing local handoff/overview were already read; no duplicate
Tailterm implementation inspection was performed.

Before calling the feature verified, use an authorized isolated node to test:
phone scanning with Tailscale absent/off; correct account/tailnet selection;
automatic completion on the original browser; approval-pending presentation;
network interruption; updated/stale URL handling; modal close/reopen; and delayed
phone completion beyond any application wait timeout. Cover iOS Safari and
Android Chrome as available. Retrying, dismissing or timing out must not display
an obsolete success state or unexpectedly restart a different login attempt.

Remaining dependency: lead's QR renderer/UI/state integration and an owner
authorized real-phone acceptance run. No Tailscale installation, TrueNAS
networking changes, new credentials or custom auth service are required by the
recommended design.
