# hub/cmd/tt test isolation

Bug `wi_49c93507dbce6cc2`, work order #42064 (earlier: `wi_fc17a11cbecb5af7`,
order #29641). The hub/cmd/tt tests must never read or write the home of the
user running them: a child test binary once read the host's redaction setting
and lost ledger rows. `TestMain` in `hub/cmd/tt/main_test.go` enforces this.

## What TestMain isolates

- **HOME.** `runWithOwnHome` points HOME at an empty `tt-home-*` directory under
  the temporary directory and removes it after the run. relay.json, hub.json,
  handoff.json, the relay state, the tool ledger and every other home path then
  resolve inside it. A caller that already gave the run a home that is not the
  user's keeps it, under the guard.
- **TMUX_TMPDIR.** `runWithOwnTmuxDir` gives the run its own `tt-tmux-*`
  directory under `/tmp` and removes it afterwards; the shared per-user tmux
  directory is never touched.
- **Settings dropped.** Every `TAILTERM_*` variable, `CODEX_HOME` and
  `CLAUDE_CONFIG_DIR` inherited from the shell is unset, because an agent's
  shell points them at the user's own directories.
- **Go caches kept.** GOCACHE, GOMODCACHE and GOPATH are pinned to the values
  `go env` gave before HOME changed, so tests that build a binary do not
  rebuild the module or download its dependencies.
- A child that is this test binary again keeps what it inherited (its
  temporary home and tmux directory); it gets no second one.

## The home seam

Product code in hub/cmd/tt reads the home only through `hub/cmd/tt/home.go`:

- `homeDir()` replaces `os.UserHomeDir()`.
- `homeSetting(name)` replaces `os.Getenv(name)` for the seven variables that
  name a home path's replacement: `TAILTERM_RELAY_STATE`,
  `TAILTERM_HANDOFF_CONFIG`, `TAILTERM_HANDOFF_DIR`, `TAILTERM_TOOL_LEDGER_DIR`,
  `TAILTERM_REDACT_SETTING_FILE`, `CODEX_HOME` and `CLAUDE_CONFIG_DIR`.
- `homeSeen(path)` reports a home resolved some other way;
  `verificationCacheRoot` uses it for the HOME its injected getenv returned.

Both return exactly what the os call returns, so the binary resolves as
before. They also pass each value to `homeWatch`, which the binary leaves nil.
Tests point the seam at a temporary root through HOME (and the settings), as
before; there is no second override. New code that reads the home or one of
these settings must use the seam: `TestHomeSeamOnlyCaller` fails, naming
file:line, on any `os.UserHomeDir` call, or any `os.Getenv`/`os.LookupEnv` of
the seven names, outside home.go.

## The real-home guard

`TT_TEST_REAL_HOMES` lists the user's real homes, as the first test process
found them (`user.Current`, which a temporary HOME does not hide). A child
that is this test binary inherits it and guards against the same homes. Set
but empty, it turns the guard off (the live runs below).

The guard has three parts:

1. **Before the run**, `realHomeGuard` fails the run if HOME is a real home, or
   if the redaction setting, relay state, handoff config, handoff directory,
   tool ledger or Codex home resolves anywhere below one.
2. **During the run**, `guardedRun` installs `homeGuardWatch` as `homeWatch`.
   Each path the seam returns that lies inside a real home, and outside the
   temporary directory and a home that is not the user's, fails the test that
   resolved it. The watch finds the test on the reading goroutine's stack, or
   by following Go's `created by F in goroutine N` lines through the
   goroutines that started it, and panics with one line:

   ```
   real-home guard: TestX resolved /Users/me/.local/state/tailterm/relay inside the real user home, through relayDir (relay.go:140); a test and every process it starts must use a temporary home (see runWithOwnHome and docs/test-isolation.md)
   ```

   In the test's own goroutine `testing` prints `--- FAIL: TestX` before the
   panic; in a goroutine the test started, the binary exits with the panic and
   its traceback. A temporary `t.Setenv` of HOME or a setting is caught at the
   read, even though it is restored before the run ends.
3. **After the run**, `guardedRun` fails the run with
   `after the tests: real-home guard: …` for the first real-home read it kept
   and `(and N more)`. That covers a panic product code recovered (the
   activity observer and the handoff worker do) and a read with no test to charge, reported as
   `no running test`. Then `realHomeGuard` runs again, for an environment
   change a test left behind.

`TestHomeGuardNamesTest` proves each case with a temporary directory declared
as the real home, so no test reads the user's.

## Exceptions

Three runs keep the caller's home and are not guarded:

- **`TT_LIVE_CLAUDE=1`.** The opt-in live tests start a real Claude Code
  session, which needs the caller's home for its own login. An empty home is
  not logged in.
- **`TT_LIVE_RUNTIME_PROMPT=1`.** The live runtime prompt check starts a
  real Claude Code session for the same reason, and checks that it leaves the
  real `~/.claude/settings.json` unchanged.
- **The redaction probe helper (`TT_REDACT_PROBE_HELPER`).** It is not a test:
  it is that live Claude session's hook, run as this test binary, and exits
  inside its test function. A temporary home of its own would be left behind
  on every hook call.

With either live variable a top-level run prints that the guard is off. The helper
returns before any guard is set up.

One plain-run read of the real home is by design. The handoff tests
(`newHandoffBox` in `handoff_test.go`) take a read-only snapshot of the real
`~/.local/state/tailterm/handoff` once per run, through `user.Current`, to
prove the tests left it unchanged; the live handoff check also hashes the real
`~/.claude/settings.json` without parsing or printing it. Set
`TT_HANDOFF_TEST_REAL_DIR` to a stand-in directory to have them read that
instead. These reads bypass the seam on purpose.

## Known limits

- The guard names the top-level test function, not a `t.Run` subtest.
- A guard panic leaves what any test panic leaves: the `tt-home-*` and
  `tt-tmux-*` directories, and a panic in a goroutine the test started also
  skips `t.Cleanup`.
- Following starters relies on Go's traceback line
  `created by F in goroutine N` (Go 1.21 and later). If a goroutine in the
  chain has already exited, the read is reported after the run as
  `no running test`, naming the last starter found. The goroutine case of
  `TestHomeGuardNamesTest` fails if the format changes.
- Test code is not behind the seam: a test that reads a real-home path itself,
  without product code, is caught only by the before and after checks.
