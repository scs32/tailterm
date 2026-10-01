# Host setup

`tt host setup` makes a machine an agent host in one step, and updates it in
place on every later run. It replaces the hand steps that used to follow each
release: copying the binary, installing the hooks, installing the relay service
and restarting it.

```sh
npm run build:tt
.build/tt/tt-darwin-arm64 host setup --hub URL     # a new host
tt host setup --from .build/tt/tt-darwin-arm64     # an update
tt host setup --check                              # report only, change nothing
tt host setup --rollback                           # back to the previous binary
```

```
tt host setup [--hub URL] [--from PATH] [--service agent|daemon]
              [--check | --rollback] [--json]
```

Run it as the user the agents run as, never as root and never through `sudo`:
as root it refuses before changing anything, in every mode, because the binary
and hook files it writes would become root-owned.

## What it does

Each run works through six steps and prints one line per step with its state.

| Step | What it installs or checks |
| --- | --- |
| `binary` | Installs `--from PATH` (default: the running executable) as `~/.local/bin/tt`. |
| `hub-config` | With `--hub URL`, writes `~/.config/tailterm/hub.json` when there is none. |
| `codex-hooks` | Merges the Tailterm Stop hook into `~/.codex/hooks.json` (or `$CODEX_HOME`). |
| `claude-hooks` | Merges the four Tailterm hooks into `~/.claude/settings.json` (or `$CLAUDE_CONFIG_DIR`). |
| `relay-service` | Installs the inbox relay as a launchd service and starts or restarts it. |
| `doctor` | Runs `tt doctor`: tmux, installed runtimes, the hub and whether the relay is running. |

States: `current` (nothing to do), `installed`, `updated`, `failed`, and from
`--check` also `missing`, `outdated` and `conflict`.

Exit codes: 0 when every step is good; 1 when a step or doctor failed, or when
`--check` found anything missing, outdated or in conflict; 2 for a usage error.

Running it again is safe. A host that is already current is left byte for byte
as it is: no file is rewritten and launchd is only asked for the service state.

`--json` prints the same steps as JSON together with `paths`: every file the run
may write, with symlinks followed. `tt host setup --check --json` therefore shows
where a real run would write before anything is written.

## Binary and rollback

The new binary is compared with the installed one by SHA-256. When they differ,
the installed binary is first copied to `~/.local/bin/tt.previous`, then the new
one is written to `tt.new` and renamed over `tt`. The output names both hashes
and the rollback path. A run that finds the binary already current does not
touch `tt.previous`, so the rollback copy is always the binary before the last
real update. A first install has no rollback copy.

`tt host setup --rollback` swaps `tt` and `tt.previous`, restarts the relay in
whichever launchd domain it is loaded, and runs doctor. A second rollback undoes
the first. With no `tt.previous` it exits 1 and changes nothing. Rollback does
not touch the hooks or the service definition.

## Hub credential

A host needs `~/.config/tailterm/hub.json` with the hub URL and token, mode 600.
`--hub URL` writes that file with the URL only, and only when the file does not
exist. Add the token yourself, privately:

```json
{"url": "https://hub.example", "token": "..."}
```

Never put the token into source, pasted prompts, logs or the static build. The
token is not accepted on the command line and `tt host setup` never prints the
file. If `hub.json` already names a different hub, the run fails and changes
nothing, because the stored token belongs to the stored URL; edit or remove the
file to change hubs.

## Hooks

Hooks are merged, never replaced. Every other key, event, group and hook in the
file is kept, and the file keeps its mode. Before Tailterm first changes a file
it keeps one copy beside it as `<file>.before-tailterm`.

For each event it manages (Codex `Stop`; Claude Code `SessionStart`,
`UserPromptSubmit`, `Stop` and `Notification`) the run ends with exactly one
Tailterm entry that names the installed `tt` by absolute path:

- An existing Tailterm entry with another path (a bare `tt`, an old location) is
  rewritten in place; only its command changes.
- A Tailterm entry inside a group with a `matcher` would not run for every
  event, so it is removed from that group. The group and its other hooks stay.
- Duplicate Tailterm entries are removed.

The whole file is validated before anything is written. Invalid JSON, content
after the JSON document, or a managed event with the wrong shape fails the step
and leaves the file untouched, with no backup. `tt hooks codex --install` uses
the same validation. A `settings.json` that is a symlink (into a dotfiles
repository, for example) stays a link and its target is updated; a dangling
link is an error.

Known limits:

- The file is rewritten with sorted keys and two-space indentation. Meaning is
  preserved; key order and spacing are not.
- A `tt` path with a space or other unusual character is written single-quoted,
  which needs the runtime to run hook commands through a shell. Ordinary paths
  are written bare, as before.
- A runtime that is not installed is skipped. The hooks do nothing outside a
  Tailterm agent session.

## Relay service: agent or daemon

On macOS the relay runs under launchd, label `com.tailterm.inbox-relay`, with
its log in `~/Library/Logs/Tailterm/inbox-relay.log`. Choose the mode with
`--service`; with no flag an installed mode is kept, and a new host gets
`agent`.

| | `--service agent` (default) | `--service daemon` |
| --- | --- | --- |
| For | a laptop, or a host with auto-login | an always-on host that must work before anyone logs in |
| launchd domain | `gui/UID` | `system` |
| Definition | `~/Library/LaunchAgents/com.tailterm.inbox-relay.plist`, mode 600 | `/Library/LaunchDaemons/com.tailterm.inbox-relay.plist`, root:wheel 644, with `UserName` set to you |
| Runs | only while you are logged in | from boot, as your user, with no login |
| Privilege | none | `sudo` for the plist and every `launchctl` change |

Caveats:

- An agent stops when you log out and does not start after a reboot until you
  log in.
- A daemon has no GUI session: it cannot use the login Keychain, and it cannot
  read a FileVault-locked home directory until the disk has been unlocked once
  after boot.
- A daemon needs `sudo` for every privileged launchd operation: installing or
  changing the plist, **every relay restart** (after each update and each
  rollback), and removing the daemon when switching back to an agent. An
  unattended release on a daemon host therefore needs non-interactive `sudo`
  for those commands; without it the restart fails and the run exits 1.
  Auto-login with the agent is the alternative.
- `tt` itself always runs as your user, so the binary and the hooks stay
  user-owned. Only the plist install and the `launchctl` calls go through
  `sudo`.
- `tt doctor --role agent` does not yet recognise a daemon-mode relay: its
  relay checks look only at the LaunchAgent. On a daemon host use plain
  `tt doctor`, which checks that a relay is running in either mode.
- Daemon mode is covered by tests with a fake `sudo` and `launchctl` only. Its
  first use on a real host should be watched.

Switching modes boots the old service out, waits until launchd no longer has
it, removes its plist, and only then installs and starts the new one. If the old
service cannot be stopped or removed the new one is not started and the run
exits 1. A run never reports success with both services loaded; `--check`
reports that as `conflict`.

When the binary changes and the service definition does not, the relay is
restarted with `launchctl kickstart -k`. When the definition changes, the
service is booted out and bootstrapped again.

A restart that fails is still owed. From the moment a new binary is installed
until the relay has been restarted on it, the file
`~/.local/state/tailterm/host-setup-restart-pending` exists. While it does,
`--check` reports `relay-service` as `outdated`, and the next run restarts the
relay even though the binary is already current. The same applies after a
rollback whose restart failed.

After a run starts or restarts the relay, doctor gives it up to ten seconds to
come up. It first looks two seconds after the restart and then every two
seconds, never sooner, so its look at the relay's lock cannot collide with a
relay that is still starting. A run that restarts nothing looks once, at once.

On Linux or any other system the step does nothing: supervise `tt relay` with
that host's own service manager.

## Releases

The release runner's Mini target runs the candidate's own host setup
(`ARTIFACT host setup --from ARTIFACT`) instead of copying the binary and
restarting the relay, then requires the installed binary to match the pinned
artifact. Its rollback runs `tt host setup --rollback` and, unless that leaves
exactly the prior binary, restores the job's journal copy and restarts the relay
with the configured `relayRestart`. See `docs/project-deployment.md`.

The first run on a host that was set up by hand makes these one-time changes:

- The relay plist is rewritten (the bytes differ from the old installer's), so
  the service is booted out and bootstrapped once instead of restarted.
- A bare `tt hook stop` in Codex's `hooks.json` becomes the absolute path.
- A host with no Claude Code hooks gets the four hooks in
  `~/.claude/settings.json`, so its Claude agents start getting the session
  briefing and the turn-end check.

## Testing against fakes

`TAILTERM_LAUNCHCTL` and `TAILTERM_SUDO` name the commands to run instead of
`launchctl` and `sudo`, and `TAILTERM_HOST_SETUP_ROOT` replaces `/` for the
daemon plist. With `HOME`, `CODEX_HOME`, `CLAUDE_CONFIG_DIR` and
`TAILTERM_RELAY_STATE` they let a built binary run entirely inside a temporary
directory.
