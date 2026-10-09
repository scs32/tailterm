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
tt host setup --remove-claude-usage                # take the usage capture out again
```

```
tt host setup [--hub URL] [--from PATH] [--service agent|daemon]
              [--check | --rollback | --remove-claude-usage] [--json]
```

Run it as the user the agents run as, never as root and never through `sudo`:
as root it refuses before changing anything, in every mode, because the binary
and hook files it writes would become root-owned.

## What it does

Each run works through seven steps and prints one line per step with its state.

| Step | What it installs or checks |
| --- | --- |
| `binary` | Installs `--from PATH` (default: the running executable) as `~/.local/bin/tt`. |
| `hub-config` | With `--hub URL`, writes `~/.config/tailterm/hub.json` when there is none. |
| `codex-hooks` | Merges the Tailterm Stop hook into `~/.codex/hooks.json` (or `$CODEX_HOME`). |
| `claude-hooks` | Merges the four Tailterm hooks into `~/.claude/settings.json` (or `$CLAUDE_CONFIG_DIR`). |
| `claude-usage` | Installs the [Claude usage capture](#claude-usage-capture) status line and its command. |
| `relay-service` | Installs the inbox relay as a launchd service and starts or restarts it. |
| `doctor` | Runs `tt doctor`: tmux, installed runtimes, the hub and whether the relay is running. |

States: `current` (nothing to do), `installed`, `updated`, `failed`, and from
`--check` also `missing`, `outdated` and `conflict`. The `claude-usage` step
reports `conflict` in a normal run too, and `--remove-claude-usage` reports
`removed`.

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

Claude Code also gets the tool-call ledger's `tt hook tool` on the three tool
events ([claude-wake.md](claude-wake.md)) and the session handoff's
`tt hook handoff` on `SessionStart`, `PreCompact` and `SessionEnd`
([session-handoff.md](session-handoff.md)), each with `"timeout": 5`. On
`SessionStart` the handoff hook is a second entry beside `tt hook session-start`,
which keeps no timeout. The handoff hook does nothing until the host's
`~/.config/tailterm/handoff.json` turns it on; host setup never creates or
changes that file. `tt host setup --rollback` does not touch hooks:
session-handoff.md has the commands that remove the handoff entries.

## Claude usage capture

The token budget needs to know how much of the Claude account's rate limits is
used ([usage-accounting.md](usage-accounting.md#provider-reading)). Claude
Code hands that to its status line command, so the `claude-usage` step
installs a capture-only status line:

- the command `~/.local/bin/tt-claude-usage-capture` (mode 755), which reads
  the status line JSON on stdin, saves `capturedAt`, `rate_limits` and
  `version` to `~/.local/state/tailterm/claude-usage.json`, and prints
  nothing, so no status line shows. Input without a `five_hour` or `seven_day`
  rate limit window leaves the file as it was;
- the `statusLine` entry in `~/.claude/settings.json` (or
  `$CLAUDE_CONFIG_DIR`): `{"type":"command","command":"<home>/.local/bin/tt-claude-usage-capture"}`.

The settings file is merged like the hooks: every other key is kept, and the
copy `<file>.before-tailterm` made before Tailterm first changed the file
stays as it is. A second run changes nothing. The relay on the host reads the
capture file and reports it to the hub; nothing else reads it. The command
needs `python3`; without it, it writes nothing and the reading is reported as
missing.

`--check` reports `missing` when the command or its entry is not there,
`outdated` when the installed command is an older version or not executable,
and `current` otherwise. A release that changes the command shows `outdated`
on every host until `tt host setup` is run there again; until then the host
keeps running the older command.

**A different status line is never replaced.** Claude Code has one
`statusLine`. If the settings already have another one, the step is a
`conflict`: the settings stay byte for byte as they are, the command is not
installed, the line shows the existing value, and the run exits 1. Claude
usage is then not captured on that host and admission there falls back to the
budget's allowance. To capture it, have your status line command also run
`~/.local/bin/tt-claude-usage-capture` with the same input, or remove your
`statusLine` and run `tt host setup` again. A `statusLine` that already is
this command, a hand-installed one included, is taken over.

Settings that the `claude-hooks` step refused (invalid JSON, a wrong shape)
are not touched by this step either. Without the Claude runtime the step is
`current` and writes nothing.

**Remove it** with one command:

```sh
tt host setup --remove-claude-usage
```

It removes the Tailterm `statusLine` entry and the command, and nothing else:
no other step runs, and a different `statusLine` is left unchanged and
reported. The last capture file stays; nothing refreshes it, so the hub stops
using it once it is older than the budget's staleness bound. The flag takes no
other option but `--json`. `tt host setup` installs the capture again.

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
