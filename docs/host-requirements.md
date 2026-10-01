# Development host requirements

Feature `wi_d9a83da1ae847ba8`, work order #16518. Until now every role ran
only on the Mac mini, whose setup was implicit. This page lists what each
kind of machine needs, and `tt doctor --role` checks a machine against it.

```sh
tt doctor --role client                 # TailOS or tt read-only
tt doctor --role agent                  # runs agent teams
tt doctor --role test [--repo DIR]      # runs tests and the verification matrix
tt doctor --role release --deploy-config PRIVATE_PATH
tt doctor                               # unchanged: tmux, runtimes, hub
```

Each role prints one line per requirement, `STATUS  check  detail`, followed
by `fix: ...` when the line is not `ok`. Status is `ok`, `missing`, `wrong
version` or `warn`. The last line is `role R: ok` or `role R: N required
missing`, and the exit status is 1 when any required line is `missing` or
`wrong version` (2 for a usage error). `warn` never changes the exit status.
The agent, test and release roles each include the client checks.

Doctor is read-only. It runs only version and login-status commands (`tmux
-V`, `node --version`, `go version`, `go env`, `claude auth status`, `codex
login status`, `gh auth status`, `launchctl print`, `git rev-parse`),
discards their output apart from parsed version numbers and exit codes, and
reads files. It never runs `ssh`, `npx`, `wrangler` or an installer, never
takes a lock, and never prints a token, host name or config value. Each
command has a 10 second limit.

## Client (terminal only)

| Requirement | Version or file | macOS | Linux | Doctor line |
|---|---|---|---|---|
| Hub configured | `TAILTERM_HUB`, or `~/.config/tailterm/hub.json` with `url` and `token` | write the file | same | `hub configured` |
| hub.json private | not group/other readable | `chmod 600` | same | `hub.json private` |
| Hub reachable | `GET /v1/whoami` answers within 5 s | tailnet route to the hub | same | `hub reachable` |
| Tailscale CLI (optional) | on PATH | Tailscale app's CLI | `tailscale` package | `tailscale CLI` (warn) |

Without the Tailscale CLI, `spawn.Host()` falls back to the OS host name.
Queue entries, host policy, relay bindings and team runners are keyed by that
name, so a host must report the same name every time.

## Agent host

Client, plus:

| Requirement | Version or file | macOS | Linux | Doctor line |
|---|---|---|---|---|
| tmux | 3.2 or newer (`spawn.Create` refuses older) | `brew install tmux` | `apt install tmux` | `tmux >= 3.2` |
| tmux window size policy | tmux 3.2 features only | none | none | `tmux sizing policy` |
| tt install path | `~/.local/bin/tt`, executable | copy the release binary | same | `tt in ~/.local/bin` |
| git | any | Xcode command line tools | `apt install git` | `git` |
| Claude Code | on PATH | installer | installer | `claude` |
| Claude login | `claude auth status` exits 0 | run `claude`, `/login` | same | `claude login` |
| Codex CLI | on PATH | `brew install codex` | npm or release binary | `codex` |
| Codex login | `codex login status` exits 0 | `codex login` | same | `codex login` |
| Codex Stop hook | `~/.codex/hooks.json` (or `$CODEX_HOME`) has `tt hook stop` | `tt hooks codex --install` | same | `codex Stop hook` |
| Claude hooks (optional) | the four `tt hook ...` commands in `~/.claude/settings.json` or `settings.local.json` | `tt hooks claude` | same | `claude hooks` (warn) |
| Relay service installed | `~/Library/LaunchAgents/com.tailterm.inbox-relay.plist` | `python3 scripts/install-relay-macos.py` | none yet (to fix) | `relay service installed` |
| Relay running | `launchctl print gui/UID/com.tailterm.inbox-relay` shows `state = running` | `launchctl kickstart -k gui/UID/com.tailterm.inbox-relay` | none yet (to fix) | `relay running` |

Mapping notes:

- `tmux sizing policy` covers [team-launch.md "Agent window size"](team-launch.md#agent-window-size):
  `tt spawn` sets `default-size` and `window-size manual` on each new session,
  and TailOS attaches with `-f ignore-size`. All three need tmux 3.2. That
  section says no global tmux option is required or changed, so doctor reads
  no tmux configuration; the line is derived from the tmux version.
- `claude hooks` covers `adapters.ClaudeHooks` (`hub/internal/adapters/adapters.go`,
  lines 9-19): `SessionStart`, `UserPromptSubmit`, `Stop` and `Notification`
  each need a command ending in `hook session-start`, `hook prompt`, `hook
  stop` and `hook notification`. The Claude hooks are optional
  ([tasks.md](tasks.md): "remain optional"), and the Mini has none, so a
  missing hook is `warn`. The Codex Stop hook is required: broker phase 3.1
  relies on it to hold a Codex turn open while work is unacknowledged.
- The relay LaunchAgent runs only while the user is logged in, and its
  environment is only `PATH` (`wi_ec4c3154f55bf5db`).

### Login detection

Logins are judged by exit status only; the output is discarded. Verified on
the Mini on 2026-09-30 with claude 2.1.285 and codex-cli 0.159.0:
`claude auth status` exits 1 with an empty `CLAUDE_CONFIG_DIR` or `HOME` and
0 when logged in; `codex login status` exits 1 with an empty `CODEX_HOME`
and 0 when logged in. `gh auth status` exits non-zero when logged out.

Side effect to know about: on an **empty** home these CLIs write their own
bookkeeping files when run. `claude` creates `.claude.json` and
`backups/.claude.json.backup.*`; `codex` creates `tmp/arg0/codex-arg0*/.lock`.
On a logged-in home nothing observable changed. Doctor itself writes nothing.

## Test and verification host

Client, plus:

| Requirement | Version or file | macOS | Linux | Doctor line |
|---|---|---|---|---|
| git | any | Xcode command line tools | `apt install git` | `git` |
| tmux | 3.2 or newer (tests start tmux sessions on private sockets) | `brew install tmux` | `apt install tmux` | `tmux >= 3.2` |
| python3 | any (tests/task-form-browser.mjs seeds a database with it) | preinstalled or `brew install python` | `apt install python3` | `python3` |
| npm | on PATH | comes with Node | comes with Node | `npm` |
| node_modules | `node_modules/.package-lock.json` | `npm ci` | same | `node_modules installed` |
| Node.js | `^20.19.0 \|\| >=22.12.0` (vite's `engines.node` in package-lock.json; a test pins it) | `brew install node` | NodeSource or nvm | `node ^20.19.0 \|\| >=22.12.0` |
| Go | `hub/go.mod` `go` line (1.26.6), under the GOTOOLCHAIN rules below | `brew install go` | go.dev tarball | `go toolchain` |
| wasm/tailserve.wasm | built | `npm run build:wasm -- --test` | same | `fixture wasm/tailserve.wasm` |
| .build/test.wasm | built | `npm run build:wasm -- --test` | same | `fixture .build/test.wasm` |
| .build/go-modules.txt | built | `npm run build:wasm -- --test` | same | `fixture .build/go-modules.txt` |
| .build/speech-fixture.wav | downloaded | `curl -fL https://huggingface.co/datasets/Xenova/transformers.js-docs/resolve/main/jfk.wav -o .build/speech-fixture.wav` ([static-client.md](static-client.md)) | same | `fixture .build/speech-fixture.wav` |
| Playwright chromium | revision in `node_modules/playwright-core/browsers.json` | `npx playwright install chromium` | same (plus `--with-deps`) | `playwright chromium` |
| Playwright headless shell | same | `npx playwright install chromium-headless-shell` | same | `playwright chromium-headless-shell` |
| Playwright webkit | same, or a `revisionOverrides` value | `npx playwright install webkit` | same | `playwright webkit` |
| Free disk | 8192 MiB on the checkout's filesystem (the queue's admission reserve) | free space | same | `free disk >= 8 GiB` |

The four fixtures are the prerequisites `scripts/verify-matrix.mjs` refuses
to run without. `npm run build:wasm -- --test` downloads the pinned Tailscale
source into `.build/` first, so it needs network access once.

Playwright's browser cache is `PLAYWRIGHT_BROWSERS_PATH` when set (`0` means
`node_modules/playwright-core/.local-browsers`), else
`~/Library/Caches/ms-playwright` on macOS, else `$XDG_CACHE_HOME/ms-playwright`
or `~/.cache/ms-playwright` on Linux. Browser directories are named
`chromium-REV`, `chromium_headless_shell-REV` and `webkit-REV`.

### GOTOOLCHAIN

Doctor runs every `go` command with `GOTOOLCHAIN=local` from the home
directory, so it never selects or downloads a toolchain. It reads the user's
setting from `$GOTOOLCHAIN`, else the `GOTOOLCHAIN=` line of the file `go env
GOENV` names, else `$(go env GOROOT)/go.env`, else `auto`, and judges it
against `hub/go.mod`:

| Setting | Outcome |
|---|---|
| `local` | ok when the installed Go is at least go.mod's; else wrong version |
| `auto` (= `local+auto`) | ok; when the installed Go is older, the detail says `will download goX` |
| `goV` (pinned) | ok when V is at least go.mod's; else wrong version |
| `goV+auto` | ok (go selects the newer of V and go.mod's); names a download when both V and the installed Go are older |
| `path` (= `local+path`), `goV+path` | ok when the installed Go or V is at least go.mod's, or `goX` for go.mod's version is on PATH; else wrong version |
| anything else | wrong version, `unrecognized GOTOOLCHAIN` |

The Mini has Go 1.26.5 with the default `auto`, so its `go` commands in the
repo download and run go1.26.6.

### Go build cache

Follow [go-build-cache.md](go-build-cache.md) (`wi_96b23f0f7f151a81`): never
clear `~/Library/Caches/go-build` or `~/go/pkg`; experiments use a scratch
`GOCACHE`. The verification matrix already gives each run its own `GOPATH`,
`GOMODCACHE` and `GOCACHE` in a disposable home.

## Release host

Client, plus:

| Requirement | Version or file | macOS | Linux | Doctor line |
|---|---|---|---|---|
| GitHub CLI | on PATH | `brew install gh` | `apt install gh` | `gh` |
| GitHub login | `gh auth status` exits 0 | `gh auth login` | same | `gh login` |
| python3 | runs `scripts/deploy-truenas-hub.py` | preinstalled or `brew install python` | `apt install python3` | `python3` |
| Node.js | `>=22.12.0` (vite for `build:static` and wrangler's `>=22.0.0`) | `brew install node` | NodeSource or nvm | `node >=22.12.0` |
| wrangler | on PATH, or in npm's npx cache `~/.npm/_npx/*/node_modules/wrangler` (highest version; its `engines.node` must admit the installed Node) | `npx wrangler --version` (downloads it) | same | `wrangler` |
| wrangler credential | `CLOUDFLARE_API_TOKEN` set, or the OAuth file with an `oauth_token` that is unexpired or has a `refresh_token` | `npx wrangler login` | same | `wrangler credential` |
| SSH route | `Host truenas` with `HostName` in `~/.ssh/config` (the `truenas-ssh` route in `scripts/deploy-truenas-hub.py`) | edit `~/.ssh/config` | same | `ssh route truenas` |
| Deployer config | `--deploy-config PATH`: readable JSON, not group/other readable, `version` 1, `enabled` true, non-empty `cwd` and `journalDirectory`, and 40-character `baselines.hub`, `bridge`, `mini`, `tailos` (`scripts/release-runner.mjs` `serveDeployment` and `readBaselines`) | provided by the owner | same | `deploy config`, `deploy config private`, `deploy config <field>` |

Notes:

- The wrangler OAuth file is `~/Library/Preferences/.wrangler/config/default.toml`
  on macOS and `$XDG_CONFIG_HOME/.wrangler/config/default.toml` (default
  `~/.config`) on Linux. An expired access token with a refresh token is `ok`
  ("refreshable") because wrangler refreshes it on use; otherwise the Mini
  would fail an hour after every deploy. Presence is checked, never validity
  online.
- The SSH check reads the config text only. It supports `Keyword args` and
  `Keyword=args`, case-insensitive keywords, `Include` (relative to `~/.ssh`,
  globs, cycles skipped, depth 16), and `Host` patterns with `*`, `?` and
  `!negation`. `Match` blocks are never evaluated: with an alias present they
  make the line `warn` ("Match rules not evaluated"); confirm with `ssh -G
  truenas` yourself.
- Without `--deploy-config`, the role prints one `missing` deploy config line.
  The field lines name the field only, never a value.
- The deployer also keeps its own checkout with the four test fixtures
  (`wi_5b03fe47520b7c4f`); run the test role there too.

## macOS and Linux

| Topic | macOS (Mini, Air) | Linux (TrueNAS or another host) |
|---|---|---|
| Relay service | per-user LaunchAgent `com.tailterm.inbox-relay`, runs only while the user is logged in | none yet: run `tt relay` under the host's supervisor (systemd user unit to be written) |
| Relay check | `launchctl print` plus the plist | reports `missing` (to fix) |
| Playwright cache | `~/Library/Caches/ms-playwright` | `~/.cache/ms-playwright`; browsers may need `--with-deps` system libraries |
| wrangler login file | `~/Library/Preferences/.wrangler/config/default.toml` | `~/.config/.wrangler/config/default.toml` |
| PATH for tmux and SSH sessions | `tt` adds `~/.local/bin`, `/opt/homebrew/bin`, `/usr/local/bin`, `/usr/bin`, `/bin` | same list; the Homebrew entry is simply absent |
| Package manager | Homebrew | the distribution's; TrueNAS SCALE is an appliance that does not support installing host packages, so run a role there in a container or VM, and do not change TrueNAS's Tailscale |

## Mini-specific assumptions

Status: **portable** works on another macOS or Linux host as is; **macOS-only**
works only on macOS; **to fix** needs a change before another host can take
the role. Source: the item's inventory at 476eacc, rechecked at 10bc367.

| Assumption | Where | Status |
|---|---|---|
| `tt` appends `/opt/homebrew/bin` and `/usr/local/bin` to PATH | `hub/cmd/tt/main.go` `main` | portable (missing directories are harmless) |
| Verification fixture PATH is Homebrew and macOS paths | `hub/internal/testverification/fixture.go:47` | macOS-only (test fixture data) |
| Relay LaunchAgent PATH | `scripts/install-relay-macos.py:17` | macOS-only |
| Old static-site deploy calls `/opt/homebrew/bin/container` | `scripts/deploy-remote-static.py:44,105` | macOS-only (old site tooling) |
| Relay service is a LaunchAgent; no Linux service | `scripts/install-relay-macos.py` | to fix |
| Release probe reads the relay status with `launchctl print` | `scripts/release-probe.mjs:93` | macOS-only |
| Relay LaunchAgent environment is only PATH | `wi_ec4c3154f55bf5db` | to fix |
| Test fixtures are hand-made files (`.build/test.wasm`, `.build/speech-fixture.wav`, `.build/go-modules.txt`, `wasm/tailserve.wasm`); the deployer's checkout lacked them | `scripts/verify-matrix.mjs`, `wi_5b03fe47520b7c4f` | to fix (doctor now names each) |
| Go version implicit | `hub/go.mod` `go 1.26.6`, no `toolchain` line | portable (documented above) |
| No Node `engines` field; Playwright `^1.58.0` | `package.json` | to fix (doctor pins vite's range) |
| Playwright browsers installed by hand | `node_modules/playwright-core/browsers.json` | to fix (doctor checks revisions) |
| tmux 3.2 or newer | `hub/internal/spawn/spawn.go:136` | portable |
| Deployer private config, `truenas` SSH alias, gh and wrangler logins | `scripts/release-runner.mjs`, `scripts/deploy-truenas-hub.py:49-60`, `scripts/release-inputs.mjs`, `scripts/deploy-static.mjs` | portable (documented and checked above) |
| Queue refuses new teams below 8 GiB free | `tt team queue policy --min-free-disk-mib` (default 8192) | portable |
| Host identity is the host name (`Stephens-Mini`) for queue entries, host policy, relay bindings and team runners | `spawn.Host()` | portable, but each host must keep a stable name |
| Worktrees under `.build/worktrees`; artifacts in `~/projects/tailterm-artifacts` | queue runner, team templates | portable (home-relative) |
| `tt doctor` checked only tmux, runtimes and the hub | `hub/cmd/tt/coordination.go` `cmdDoctor` | fixed by `tt doctor --role` (plain `tt doctor` is unchanged) |
| Verifier Playwright path is macOS-only | `scripts/verify-matrix.mjs:881-883` (`~/Library/Caches/ms-playwright`) | to fix |
| wrangler is not in package.json; it runs from the npx cache | `scripts/deploy-static.mjs`, `~/.npm/_npx` | to fix (pin it) |
| No Linux relay service | `scripts/install-relay-macos.py` only | to fix |

## Limits

- `launchctl print` output is not a stable interface. Doctor reads only the
  first top-level `state = ...` line.
- Login checks trust each CLI's exit status; a future release could change it.
- The SSH check is a partial `ssh_config` reader (see above).
- A wrangler found on PATH is reported without its version.
