# Session handoff record

Feature `wi_3dae763822d9c061`, stage A (build order #28607, design accepted in #28589). The design, with its reasons, is `.build/plans/wi_3dae763822d9c061-session-handoff-design.md`.

The owner helper session holds things the hub does not: the wakes it scheduled, the standing instructions it is working under, what it meant to do next. A Claude Code restart, `/clear` or compaction loses them. This feature keeps a small structured record of them on the host, and tells a fresh session that the record exists.

Stage A does three things:

- `tt handoff note`, `show` and `write` keep and print the record.
- `tt hook handoff`, a Claude Code settings hook, stamps the record at session end and before compaction, and at session start prints a short note that Claude Code adds to the session's context.
- `tt host setup` installs that hook.

Stage A changes no registration, tmux tag, relay binding or wake. A session that starts after a `/clear` is told what the previous one left and what to run; it still registers with `tt helper register` and re-creates its wakes by hand. Restoring those automatically is stage B, which is a proposal and not built.

It ships switched off. Until a host turns it on, the hook reads one small file and exits.

## The setting

One key in one file per host, beside `hub.json`:

```json
{"sessionHandoff": "on"}
```

at `~/.config/tailterm/handoff.json`.

- A missing file, an unreadable file, malformed JSON, a missing key, a key in another case (`sessionhandoff`, `SessionHandoff`) and any value other than the exact string `"on"` all mean off.
- It is read at every hook run and every `tt handoff` command. A change needs no restart and takes effect at the next event.
- The hook reads it first, before stdin, the relay state or tmux.
- With it off, `tt handoff note` and `tt handoff write` refuse and name the file and the key. `tt handoff show` still prints a record that exists, so switching it off never hides state.
- No `tt` command switches it on, and `tt host setup` never creates or changes the file. The owner or the owner helper edits it by hand.
- `TAILTERM_HANDOFF_CONFIG` names another file. Only tests use it.

## Which sessions run the hook

`tt host setup` merges the hook into the user-level settings of the user who runs it (`~/.claude/settings.json`, or `$CLAUDE_CONFIG_DIR`). After that, every Claude Code session that user starts on that host and that loads user settings runs `tt hook handoff` on three events: `SessionStart`, `PreCompact` and `SessionEnd`. That includes the owner's own sessions, every Tailterm agent session and `claude -p` runs. Each entry carries `"timeout": 5`. On `SessionStart` it is a second entry beside `tt hook session-start`, which keeps no timeout.

What each session then experiences:

| Session | Setting off (the default) | Setting on |
| --- | --- | --- |
| Any session | The hook reads one small file, exits 0, prints nothing and writes nothing | as below |
| A Tailterm agent (`TAILTERM_AGENT` set) | same | Nothing: it returns before reading stdin |
| A session that is not the helper and not in the helper's tmux session | same | Nothing, after one directory listing and at most one `tmux` query |
| The registered owner helper session | same | A stamp at end and compaction; the note at start |
| A new Claude session in the helper's tmux session | same | The note, saying the session is not registered |

A session does not run the hook at all when it opts out of user settings or of hooks: `--bare`, `--setting-sources` without `user`, another `CLAUDE_CONFIG_DIR`, or a setting or policy that disables hooks. Another user account on the host is not affected. Codex sessions run no such hook; they can use the `tt handoff` commands.

## It cannot block or slow a session

Two separate bounds apply.

**The hook's own target: 500 ms.** The work, the stdin read included, runs in a goroutine the handler waits on for at most 400 ms. Its one possible subprocess, a single `tmux display-message`, runs under the same deadline and is killed with it. The hook makes no hub request and opens no connection. `TestHandoffHookBound` holds it to 500 ms with stdin held open, 10,000 unrelated files in the relay state, a `tmux` that never answers, an unwritable state directory and the record's lock held by another process.

**Claude Code's outside cut-off: 5 seconds.** The handler cannot bound what happens before it runs: process start, and the `PATH` and `hub.json` read every `tt` command does first. The settings entry carries `"timeout": 5` for that. The live check below shows Claude Code honours it on all three events: it gave a hung entry up 4.9 to 5.2 seconds after it started.

So a working hook holds a session for at most 500 ms, and a hung one for at most about 5 seconds per event. One thing a hung hook does leave behind: on `PreCompact`, Claude Code shows the model a line saying the hook was cancelled (see the live check's result).

It also cannot decide anything:

- The exit code is always 0. The handler has no return value.
- On `PreCompact` and `SessionEnd` it writes nothing to stdout or stderr, in every case including errors, so there is no decision for Claude Code to read.
- On `SessionStart` it prints at most 8,192 bytes of plain text. If it runs out of time after it has recognised the session, it prints one fixed line: ``Tailterm handoff: not read in time; run `tt handoff show` ``.

## What is recorded, and where

`~/.local/state/tailterm/handoff/<helper agent id>/`, beside the relay state and the tool ledger:

| File | Contents |
| --- | --- |
| `record.json` | The current record. Replaced by write-to-temporary and rename, under a lock |
| `record.json.1` | The record before the last replacement. Nothing older is kept |
| `captures.jsonl` | One line per hook event; rotated at 1 MiB to `captures.jsonl.1` |
| `handoff.lock` | The lock |

Directories are 0700 and files 0600. The record is at most 64 KiB; a write that would pass that is refused and names the fullest list. `TAILTERM_HANDOFF_DIR` replaces the `handoff` directory; only tests use it. The record is keyed by the helper agent, not the session, so a new session or another runtime on the same host finds it. It stays on the host: it is not sent to the hub or copied to another host.

### The rule for every stored value

Every value in the record and the capture log is one of two kinds:

1. **Generated by `tt`**: field names, entry ids (`w1`, `i2`), states, times, counts.
2. **A validated value**: a Tailterm id matching its pattern (`tsk_`, `agt_`, `run_`, `wi_`, `ohr_`, `dlw_`, `obl_`, `tqe_` and 16 hex digits), a Board message number of 1 to 12 digits, a time, a five-field cron schedule of numbers, ranges, lists and steps within each field's range, a UUID session id, or a value from a fixed list.

No field accepts words. A note that needs to say what an instruction, a decision or a claim is points to the Board message that says it, by number. A value that fits neither kind is refused and nothing is written; nothing is trimmed or repaired into shape. The record therefore holds no text from the session, the Board, the transcript or the environment, and so no token, key, password or vault content. `TestHandoffNoText` offers a sentence, a password, a PEM block, a `Bearer` header, hex and base64 strings, a file path, malformed ids, malformed numbers and malformed schedules to every field of every note, and random printable strings besides.

Three identity values are names, checked against the patterns the hub already enforces: the helper's agent name and its tmux session name (letters, digits, `_` and `-`, at most 64), and the hub's address (scheme, host and port only).

A record is read back through the same check. A file holding any other value, such as one edited by hand, is not printed and not injected: the hook then prints one fixed line and `tt handoff show` refuses.

What costs: the record alone does not say what an instruction is. The note and `tt handoff show` tell the reader to read each cited message, and a decision or instruction can be noted only once it is on the Board.

### The fields

**Identity**, copied from the host helper file (`docs/owner-helper.md`): hub, project, agent id and name, run, registration receipt id, runtime, runtime thread, tmux session name, id and creation time, and when it was copied. The helper file's request ids and hashes are not copied. It says what that file said at that time. It is not proof of a registration.

**Capture**, written by the hook: for the last session start, the last compaction and the last session end, the time, the `source`, `trigger` or `reason`, and the Claude Code session id. An unknown `source`, `trigger` or `reason` is stored as `other`. From the hook's input only `hook_event_name`, `session_id`, `source`, `trigger` and `reason` are decoded; `cwd`, `transcript_path` and `custom_instructions` are not read into anything.

**Hub snapshot**, written only by `tt handoff write`, with one as-of time:

- the helper's status on the hub;
- its delegation window: id, scope, end time, and whether it is open, expired or closed;
- obligations the helper owes: obligation id, message number, kind, sender, due time, state;
- open owner decisions: message number, sender, state;
- team queue entries that are launching or running: entry id, item id and revision, work-order message number, state.

Each list holds at most 50 rows and counts what it left out. A sender is an agent id, `human` or `unknown`. No message text is stored. A decision's request id is not stored, because the requester chooses it and it can hold words.

**Session notes**, written by the session with `tt handoff note`:

| Note | Fields | States |
| --- | --- | --- |
| `wake` | schedule; kind (`hourly-update`, `morning-summary`, `queue-check`, `obligation-check`, `authority-expiry`, `other`); the message that says what to do at the wake; expiry | `active`, `expired`, `cancelled` |
| `instruction` | the message that gave it; who gave it (`owner`, `delegate`); when; expiry or `none` | `active`, `expired`, `revoked` |
| `order` | item id and revision; work-order message; last and expected next action (`send-order`, `await-start`, `await-result`, `review`, `verify`, `release`, `answer-owner`, `other`); optional message | `open`, `done`, `dropped` |
| `decision` | the message that records it; authority (`owner`, `delegated`, `helper`) | `recorded`, `closed` |
| `claim` | the message it came from; the message of the check | `unverified`, `verified`, `refuted` |

Expiry is worked out from the stored time whenever the record is read. Nothing has to run for an instruction to show as expired.

Each list holds at most 50 entries. When a full list gets a new entry, the oldest entry that is no longer current (expired, revoked, cancelled, done, dropped, closed, verified or refuted) is dropped and the list's `dropped` count goes up. Current entries are never dropped: a list of 50 current entries refuses the add.

## The commands

```sh
tt handoff show [--task T] [--json]
tt handoff write --task T [--out PATH]
tt handoff note wake add --schedule '0 * * * *' --kind hourly-update --message 28526 [--expires TIME|DURATION|none]
tt handoff note instruction add --message 28526 --by owner [--given TIME] [--expires TIME|DURATION|none]
tt handoff note order add --item wi_... --revision 4 --order 28607 --last send-order --next await-result [--message N]
tt handoff note decision add --message 28589 --authority delegated
tt handoff note claim add --message 28700
tt handoff note KIND set ID [the same flags]
tt handoff note KIND close ID [--state S] [--check N]
```

- **`note`** makes no hub request. `set` changes the fields named. `close` cancels a wake, revokes an instruction, marks an order `done` (or `--state dropped`), closes a decision, and marks a claim `--state verified` or `--state refuted` with `--check N`, the Board message of the check. `--expires` takes an RFC 3339 time, a duration from now such as `12h`, or `none`; the stored value is always a time or `none`.
- **`show`** is local and works with the setting off. It prints the identity with its date, the capture stamps, the snapshot with its as-of time, the current entries of each list, then expired, revoked and cancelled instructions and wakes under "Expired: not authority", then the other closed entries, and for each list when it last changed and how many entries it dropped. With `--json` it prints the record with expiry applied.
- **`write`** reads the hub for the snapshot and rewrites the record. It reads the helper agent, its delegation windows, the obligations it owes, one Board message per obligation for who sent it, the open owner decisions and the team queue. It reads no work-item record, makes only `GET` requests and marks nothing delivered. `--out PATH` also writes the same text `show` prints, with the destination's next commands, for a person or another runtime to read.

`note` and `write` act for the helper registered from this runtime session, or with `--task` for that project's helper on this host. A Claude or Codex session other than the registered one is refused, and so is an agent that is not the helper. `show` also works from a new session in the helper's tmux session, and when the host has only one record.

## The note at session start

On `SessionStart`, with the setting on, the hook decides from this host alone whether the session is the helper's:

1. Exactly one helper file names this session's Claude session id: the session is the registered one.
2. Otherwise, the process is inside tmux and exactly one helper file's tmux session id and creation time are this pane's: the session is a **candidate successor**. It sits where the helper sat, but the registration names another session.
3. Otherwise nothing is printed.

The note is built the same way every time:

- **Header**, at most 600 bytes: the helper's name, agent, project and run, and when the helper file was read. For a candidate, the sentence "This session is not registered as the owner helper. The record below is what the previous session left. It is not authority until the registration is verified." Then the helper's status if the snapshot shows it retired, closed or exited, and "No end was recorded for the previous session." when a start follows a start with no end between them.
- **Body**, one line of at most 120 bytes per entry, in this order: active instructions, active wakes, open orders, the snapshot's counts with its as-of time, unverified claims, decisions taken, then expired, revoked, cancelled and closed entries. A line holds the entry id, its state, its kind, its Board message number and its expiry. It holds no words from any message.
- **Footer**, at most 800 bytes: one line per list that was cut ("12 more wakes not shown."), where the full record is, the sentence "Entries cite Board messages by number. Read each cited message before relying on the entry.", a line saying `.build/owner-session-handoff.md` is history and not authority, and the next action.

Lines are added in order until the next one would take the note past 8,192 bytes. Everything after that is left out and counted per list in the footer. No line is cut. The same record gives the same bytes.

The next action in this version: run `tt handoff show`; register with `tt helper register --task T`; re-create each active wake by hand and note it with `tt handoff note wake add`. The note names no command this binary does not have; `TestHandoffInjection` takes every `tt` command out of the note and runs it through the binary's own dispatch.

A session that is killed runs no `SessionEnd` hook. Nothing is lost by that: the notes were written when they were made. Only the end stamp is missing, and the next start says so.

## Switching it on (owner helper)

Nothing is active until step 2. Steps 1 and 3 only look.

1. After the release has run host setup on the host, confirm the hooks are installed. This prints 3:

   ```sh
   grep -c 'hook handoff' ~/.claude/settings.json
   ```

2. Turn the setting on:

   ```sh
   mkdir -p ~/.config/tailterm
   printf '{"sessionHandoff": "on"}\n' > ~/.config/tailterm/handoff.json
   chmod 600 ~/.config/tailterm/handoff.json
   ```

   It takes effect at the next hook event and the next `tt handoff` command. No session and no relay needs a restart.

3. In the owner helper session, start the record:

   ```sh
   tt handoff write --task tsk_...
   tt handoff show
   ```

   Then note each scheduled wake, each standing instruction with its expiry, and each order in flight, citing Board message numbers.

4. The next compaction or `/clear` in that session shows the note. After a `/clear`, follow its next action: `tt helper register --task tsk_...`, then re-create the wakes by hand.

For other sessions on the host nothing visible changes. Agent sessions and the owner's other Claude sessions run the hook on the same three events and it returns without output. A second Claude session started inside the helper's tmux session is shown the note; keep that session to one pane, as `docs/owner-helper.md` already asks.

## Switching it off

Remove the setting. This is immediate and enough in almost every case:

```sh
rm ~/.config/tailterm/handoff.json
```

From the next event the hook prints nothing and writes nothing, and `note` and `write` refuse. The record stays on disk and `tt handoff show` still prints it. To delete the record as well: `rm -r ~/.local/state/tailterm/handoff`.

### Removing the hooks

Do this only when the hook process must not run at all, for example before rolling `tt` back to a version without it:

```sh
f="$HOME/.claude/settings.json"
cp -p "$f" "$f.before-handoff-hook-removal"
/usr/bin/jq '
  def events: ["SessionStart", "PreCompact", "SessionEnd"];
  if (.hooks | type) == "object" then
    .hooks |= with_entries(
      if (.key as $k | events | index($k)) and (.value | type) == "array" then
        .value |= (map(if (.hooks | type) == "array"
                       then .hooks |= map(select(((.command? // "") | tostring | test("(^|/)tt.? +hook +handoff *$")) | not))
                       else . end)
                   | map(select((.hooks | type) != "array" or (.hooks | length) > 0)))
      else . end)
    | .hooks |= with_entries(select((.key as $k | events | index($k) | not) or (.value | length) > 0))
  else . end' "$f.before-handoff-hook-removal" > "$f.tmp" && cat "$f.tmp" > "$f" && rm "$f.tmp"
grep -q 'hook handoff' "$f" || echo "handoff hooks removed"
```

This is the ledger's removal filter (`docs/claude-wake.md`) with the events and the hook name changed. It removes only entries whose command is a `tt` binary followed by `hook handoff`, then the groups and events that leaves empty.

**Result of running it, 2026-10-07, jq 1.7.1, against a sandbox settings file** (never the real one). The file held the nine events `tt hooks claude` writes, the user's own `SessionStart`, `PreCompact` (with a `manual` matcher and its own timeout) and `SessionEnd` entries, a `PreToolUse` entry with a `Bash` matcher, another top-level key and a 20-digit number:

- It printed `handoff hooks removed`. A key-sorted comparison of the file before and after showed exactly three groups gone, the three `tt hook handoff` groups, and no other difference.
- Kept unchanged: the user's three entries on those events, `tt hook session-start` on `SessionStart`, the six other tt entries, the other key, and the 20-digit number digit for digit.
- The file kept mode 0600.
- A second run changed nothing: the file's hash was the same.

`tt host setup --rollback` does not touch hooks; `TestHostSetupInstallsHandoffHooks` asserts the settings file is byte-identical before and after it. Any later `tt host setup` from a version with this feature adds the hooks again, so after a removal use the rolled-back `tt` for setup until the release is retried.

A `tt` from before this feature, left with the hooks installed, has no `handoff` hook. By its hook dispatch it prints `tt: unknown hook "handoff"` and exits 1 in Tailterm agent sessions on these three events, and exits 0 silently elsewhere; this was read from the code and not run with an old binary. The live check below shows Claude Code carries on after an entry that exits 1 on each of the three events; on `PreCompact` it shows the model the entry's stderr line. Removing the hooks first avoids that and a failing process on every start, compaction and exit.

## The live check

The unit tests prove the handler. They cannot prove what Claude Code does around it. One opt-in check does, and its recorded result is required before a release (criterion a16):

```sh
cd hub
TT_LIVE_CLAUDE=1 go test ./cmd/tt -run '^TestHandoffLivePrivateSession$' -count=1 -v -timeout 30m
```

It starts disposable `claude` sessions and types into them. What keeps it away from everything real:

- Each session is started with `--setting-sources project` in a new project directory whose `.claude/settings.json` holds only the three entries under test.
- Each entry is a wrapper script. It logs the **names**, never the values, of the variables it was given, then runs the candidate `tt` by absolute path under `env -i` with a temporary `HOME` (holding a made-up `hub.json` with an unreachable hub and a made-up token), temporary handoff, setting and relay-state paths, and only `CLAUDE_CODE_SESSION_ID`, `TMUX` and `TMUX_PANE` passed through. `TMUX` names the private server's socket, so the hook asks that server, as it would ask the owner's in real use.
- `TAILTERM_HANDOFF_ACCESS_LOG` makes the handoff code write every path it opens, lists, creates or renames to a file. The variable does nothing in any other hook.
- The session runs in a new tmux session on a private socket, with a made-up helper file naming an invented agent, project and run.
- The `claude` process keeps the real `HOME` only so that the CLI stays logged in.

After every run it requires three proofs, and fails on any miss: every path in the access log is under the run's directory; the wrapper logs show only the expected entries ran, and Claude Code's debug output names no hook command but the run's own and no event a user-level `tt` hook fires on; the SHA-256 of the real `~/.claude/settings.json` is unchanged (the file is hashed, never parsed or printed) and the real handoff directory is as it was.

The run directories are created under the repository's ignored `.build` directory, so Claude Code treats them as part of a folder the user already trusted. If it shows a trust dialog anyway, the check stops without answering it.

Hung entries are tested one event at a time, because a compaction runs `PreCompact` and then `SessionStart`, and a clear runs `SessionEnd` and then `SessionStart`: hanging all three would stack two cut-offs. In each of runs H1 to H3 exactly one entry sleeps 100 seconds with `"timeout": 5` and the other two work. Runs F1 to F3 do the same with an entry that exits 1 with text on stderr. Each run's time for that step is compared with the same step in the working run; the budget is 7 seconds more. A hung entry also logs its own start, and the check times from there to the line in Claude Code's debug output that gives it up.

### Result

Run on 2026-10-07 on the Mini with Claude Code **2.1.293**, on the stage A candidate, twice in full with the same outcome (and once more as far as run H2). The figures are from the last run (started 20:05:20Z). **The check fails on one point, the first under "What failed" below.** Everything else it requires was observed.

**Isolation, all eight runs.** Every path the hook opened was under its run directory (94 in the working run, 10 or 34 in the others). The wrapper logs showed exactly the expected entries and counts. Claude Code's debug output named no hook command but the run's own, and no `UserPromptSubmit`, tool, `Stop` or `Notification` hook, so no user-level hook ran: `--setting-sources project` keeps them out. The real settings file's hash was unchanged, and the real handoff directory did not exist before or after.

**Variables the hook was given** (names only): `CLAUDE_CODE_SESSION_ID`, `TMUX` and `TMUX_PANE` were present on all three events, with `CLAUDECODE`, `CLAUDE_PROJECT_DIR`, `CLAUDE_PID`, `CLAUDE_ENV_FILE`, `CLAUDE_CODE_ENTRYPOINT`, `CLAUDE_CODE_CHILD_SESSION`, `CLAUDE_CODE_SESSION_ATTENDED`, `CLAUDE_CODE_MESSAGING_SOCKET`, `CLAUDE_CODE_MESSAGING_TOKEN`, `AI_AGENT` and the usual shell and terminal variables. The wrapper passed on only the first three.

**Triggers, in one working session:**

| Step | Captured | Session id | Note reached the model |
| --- | --- | --- | --- |
| Launch with `--session-id` | `SessionStart`, `source` `startup` | the given id | Yes: it repeated the note's first line |
| `/compact` | `PreCompact`, `trigger` `manual`; then `SessionStart`, `source` `compact` | unchanged | Yes: it repeated the registered sentence |
| `/clear` | `SessionEnd`, `reason` `clear`; then `SessionStart`, `source` `clear` | **changed** | Yes: it repeated the not-registered sentence, so the new session was matched through tmux as a candidate successor |
| `/exit` | `SessionEnd`, `reason` `prompt_input_exit` | | |
| `claude --resume` | `SessionStart`, `source` `resume` | unchanged | |

**Hung entries, one at a time.** In each run one entry slept 100 seconds with `"timeout": 5`. The cut-off is the time from the entry's own start to the line in Claude Code's debug output that gives it up.

| Run | Hung event | Cut off after | What the session did | Against the working run |
| --- | --- | --- | --- | --- |
| H1 | `SessionStart` | 4.9 s (`timed out after 5000ms`) | The prompt was ready at once; the first reply waited for the cut-off | prompt ready 0.7 s against 0.7 s; first reply 7.1 s after launch against 2.5 s |
| H2 | `PreCompact` | 5.2 s (`cancelled`) | The compaction completed | 22.8 s against 25.8 s: the summary itself takes about 20 s and varies by more than the 5 s, so the cut-off is the measure here |
| H3 | `SessionEnd` | 4.9 s (`cancelled`) | The process exited | exit 5.5 s against 0.3 s, 5.2 s more |

All within the 7-second budget. After each run no hook process was left. In H1 the model, asked, reported no hook error in its context.

**Failing entries (exit 1, text on stderr), one at a time.** F1 `SessionStart`, F2 `PreCompact`, F3 `SessionEnd`: the session started, compacted and exited as in the working run (prompt ready 0.7 s; exit 0.8 s against 0.3 s). In F1 the model reported no hook error.

**What failed.** The criterion asks that the model is shown no hook error in any run. For `PreCompact` it is shown one:

- Claude Code prints the `PreCompact` hook's outcome under `/compact`, and that output is part of what the model then sees. For a working hook the line is `PreCompact [command] completed successfully`. In H2 it was `PreCompact [command] failed: Hook cancelled`, and in F2 `PreCompact [command] failed:` followed by the entry's stderr text. Asked, the model reported a hook error in both.
- This is how Claude Code 2.1.293 reports any `PreCompact` command hook on a manual compaction. Nothing in `tt hook handoff` can change it. The compaction still completed and the session carried on in both runs.
- What it means in use: `tt hook handoff` never exits non-zero and never writes to stderr, so the F2 case does not arise from it. The H2 case arises only if the hook process hangs past 5 seconds, and then the model sees one line saying a `PreCompact` hook was cancelled. On every manual compaction the model sees one line saying the hook completed.
- Not observed: an automatic compaction, where there is no `/compact` output.

Whether that one line is acceptable is the owner's decision; the check keeps failing on it until the criterion or Claude Code changes.

**Other things learned.**

- Claude Code shows the prompt without waiting for `SessionStart` hooks. A hung one delays the first reply, not the prompt.
- A project directory under a folder the user already trusted showed no trust dialog.
- The debug output names the command for `PreCompact` and `SessionEnd` hooks and for a `SessionStart` hook that times out. For a `SessionStart` hook that succeeds it gives the event and the output without the command, so for those the check counts results against the wrapper log.
- Not tested: `SessionEnd` when the terminal or tmux session is killed, `source` `fork`, `trigger` `auto`, and settings picked up by a session that is already running.

## Known limits

- **Stage A restores nothing.** It records, stamps and tells. Registration and wakes are restored by hand.
- **The session keeps the notes current.** No hook can know that a session scheduled a wake or took a decision. Every list shows when it last changed, so a stale record looks stale.
- **The record is not proof.** Its identity is what one host file said at a time, and its snapshot is what the hub said at a time. Only `tt helper env` or `tt helper register` checks a registration with the hub.
- **A second Claude session in the helper's tmux session** is shown the note as a candidate successor. The note says it is not registered.
- **A wake's line in the note includes its schedule only when the line fits in 120 bytes.** `tt handoff show` always prints it.
- **Decisions stay current until closed.** A list of 50 recorded decisions refuses a new one until one is closed.
- **Claude Code reports the `PreCompact` hook under `/compact`**, where the model sees it: "completed successfully" normally, "failed: Hook cancelled" if the hook hung. See the live check's result.
- **`tt doctor` does not check the new hook.**
- **Claude Code only.** Codex runs no capture hook. A Codex helper can use the commands.
- **One host.** The record is not uploaded or copied. Another host has its own.
- **Whether `SessionEnd` runs when a terminal or tmux session is killed** was not tested. The design assumes it may not, and the next start reports the missing end.
