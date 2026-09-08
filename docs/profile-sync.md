# Shared encrypted profiles

Tailterm still starts with a local encrypted vault. Enter a username and the
existing vault passphrase on the landing page to identify a shared profile.
A username is optional for local-only use and required for profile sync.
Usernames are case-insensitive, 1–64 letters, digits, dots, dashes or underscores.
This version keeps one active local profile per browser origin.

## First setup

1. Unlock your current local vault and connect Tailscale.
2. Configure the existing coordination hub under Tasks → Configure task hub.
3. Open **Profile sync** in the sidebar or Commands. Enter a username and your
   current vault passphrase if the profile is not already named and unlocked.
4. Keep the task hub address and select **Enable sync**.

The hub token is used only to enroll a new username. It cannot read or update
existing profiles. Enrollment refuses to overwrite a username that already exists.

## Another computer

Enter the same username and passphrase to create the browser’s initial local
vault, then connect Tailscale. Tailterm looks for profile services on online
Tailscale peers at port 18765 and tries the configured task hub if present.
With one discovered service and a matching account, an empty local profile is
restored automatically. Multiple services require a choice in Profile sync.
For a custom port, enter the address and select **Restore my profile**.
A browser with saved local settings requires an explicit choice before replacement.
Each browser enrolls its own Tailscale identity; node credentials are never synced.

Once paired, unlocking connects Tailscale and checks the saved server. Updates
save locally first, sync after a short debounce, and are checked every 15 seconds
while the page remains open. No background execution is promised after closing
the browser. A disconnected device retains unsent changes across lock/reload.

Shared data: server definitions, saved SSH credentials and keys, session
bookmarks, task hub configuration, team templates (including roles, models and instructions), and appearance preferences.
Local data: Tailscale identity, open window/pane layout, temporary authentication,
clipboard, terminal scrollback, and voice state. Tasks/messages already live on
the coordination hub and are not duplicated in a profile blob.

## Conflicts and recovery

Each update requires the current revision. If another device saved first,
Tailterm retains local changes and pauses for **Use server copy** or **Keep this
device’s copy**. Download the local backup before choosing if desired. Applying
a server copy retains the previous local copy in the encrypted local vault;
**What travels with me? → Download previous local copy** exports it.

The server keeps ten prior encrypted revisions in `profile_history` for
administrator recovery, alongside the current row in `profiles`. Database backups
must include `profile_meta`, which holds the stable service identity. A changed
identity or an older revision pauses an already-paired browser. Stopping sync
leaves the server copy intact and disables automatic rejoining on that device.

## Encryption and authorization

The browser derives a non-exportable HKDF master from the passphrase using
PBKDF2-SHA256 (600,000 iterations), with a versioned username-specific salt.
HKDF separates the profile authentication credential and AES-256-GCM encryption
key by purpose and stable hub instance ID. Encryption authenticates the format,
username and hub instance ID as additional data, with a random 96-bit IV per save.
The server stores only a SHA-256 verifier of the authentication credential and
an opaque encrypted envelope. The passphrase and encryption keys are never sent.

These are separate from the local vault's random-salt encryption keys and from
agent coordination tokens. Requests travel inside the browser’s Tailscale node.
A username is a locator, not an authentication credential; read/write requests
need a passphrase-derived Profile credential. The service advertises its ID but
never lists usernames. New username enrollment needs the existing hub credential.
The current limits are 32 profiles and 2 MiB per encrypted envelope.

This adds profile isolation to the shared coordination hub; it does not turn
its existing task/message API into a separate workspace for each username.

Verification: `npm test`, `npm run test:hub`, and
`node tests/profile-sync-browser.mjs` cover encryption, wrong credentials,
profile isolation, revision conflicts, history, restart persistence, and separate
browser vaults in Chromium and WebKit. Tests use isolated databases.
