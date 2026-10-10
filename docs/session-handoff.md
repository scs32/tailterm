# Session handoff record

Feature `wi_3dae763822d9c061`, stage A (build order #28607, design accepted in #28589). The design, with its reasons, is `.build/plans/wi_3dae763822d9c061-session-handoff-design.md`.

The owner helper session holds things the hub does not: the wakes it scheduled, the standing instructions it is working under, what it meant to do next. A Claude Code restart, `/clear` or compaction loses them. This feature keeps a small structured record of them on the host, and tells a fresh session that the record exists.

Stage A does three things:

- `tt handoff note`, `show` and `write` keep and print the record.
- `tt hook handoff`, a Claude Code settings hook, stamps the record at session end and before compaction, and at session start prints a short note that Claude Code adds to the session's context.
- `tt host setup` installs that hook.

Stage A changes no registration, tmux tag, relay binding or wake.

Stage B (feature `wi_e403542387af7deb`, owner order #31581) adds what a session runs after it has been told:

- `tt handoff restore` makes a new session in the helper's tmux pane the registered helper, behind seven guards and one conditional hub registration, and reports what is and is not restored.
- `tt handoff status` prints that report again, with each wake's state now.
- `tt handoff wake confirm` and `tt handoff wake due` record, for each wake, whether this session created its cron and when it last fired.

The hook still registers nothing: the session runs `tt handoff restore` when the note prompts it. `tt` never creates, lists or deletes a cron; the session does, with its own tools, and `tt` reports a wake as restored only on evidence it did not get from the session.

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
| The registered owner helper session | same | A stamp in the record at start, compaction and end; the note at start |
| Another interactive Claude session in the helper's tmux session, such as the new session after `/clear` | same | One line in the capture log marked `candidate`; the note at start, saying the session is not registered. The record is not changed |
| A one-shot Claude process in the helper's tmux session, such as `claude -p` run from the helper's pane | same | One line in the capture log marked `candidate`. No note. The record is not changed |

A session does not run the hook at all when it opts out of user settings or of hooks: `--bare`, `--setting-sources` without `user`, another `CLAUDE_CONFIG_DIR`, or a setting or policy that disables hooks. Another user account on the host is not affected. Codex sessions run no such hook; they can use the `tt handoff` commands.

## It cannot block or slow a session

Two separate bounds apply.

**The hook's own target: 500 ms.** The work, the stdin read included, runs in a goroutine the handler waits on for at most 400 ms. It starts at most two subprocesses, one after the other: a single `tmux display-message`, and at a matched `SessionStart` a single `ps` (see "Only the pane's interactive session"). Each runs in its own process group under the same deadline. If one has not answered when the handler's wait ends, the handler itself kills the group before it returns, so no child outlives the hook; `TestHandoffHookLeavesNoChild` runs the hook as a process 120 times against a `tmux` that never answers and finds none left. The hook makes no hub request and opens no connection. `TestHandoffHookBound` holds it to 500 ms with stdin held open, 10,000 unrelated files in the relay state, a `tmux` that never answers, an unwritable state directory and the record's lock held by another process.

**Claude Code's outside cut-off: 5 seconds.** The handler cannot bound what happens before it runs: process start, and the `PATH` and `hub.json` read every `tt` command does first. The settings entry carries `"timeout": 5` for that. The live check below shows Claude Code honours it on all three events: it gave a hung entry up 4.9 to 5.2 seconds after it started.

So a working hook holds a session for at most 500 ms, and a hung one for at most about 5 seconds per event. One thing a hung hook does leave behind: on `PreCompact`, Claude Code 2.1.293 shows the model one line saying the hook was cancelled. That is a known limit; see the live check's result.

It also cannot decide anything:

- The exit code is always 0. The handler has no return value.
- On `PreCompact` and `SessionEnd` it writes nothing to stdout or stderr, in every case including errors, so there is no decision for Claude Code to read.
- On `SessionStart` it prints at most 8,192 bytes of plain text. If it runs out of time after it has recognised the session and placed its process as interactive, it prints one fixed line: ``Tailterm handoff: not read in time; run `tt handoff show` ``.

## What is recorded, and where

`~/.local/state/tailterm/handoff/<helper agent id>/`, beside the relay state and the tool ledger:

| File | Contents |
| --- | --- |
| `record.json` | The current record. Replaced by write-to-temporary and rename, under a lock |
| `record.json.1` | The record before the last replacement. Nothing older is kept |
| `captures.jsonl` | One line per hook event; rotated at 1 MiB to `captures.jsonl.1` |
| `restore.json` | The last `tt handoff restore` report |
| `handoff.lock` | The lock |

Directories are 0700 and files 0600. The record is at most 64 KiB; a write that would pass that is refused and names the fullest list. `TAILTERM_HANDOFF_DIR` replaces the `handoff` directory; only tests use it. The record is keyed by the helper agent, not the session, so a new session or another runtime on the same host finds it. It stays on the host: it is not sent to the hub or copied to another host.

### The rule for every stored value

Every value in the record, the capture log and the restore report is one of two kinds:

1. **Generated by `tt`**: field names, entry ids (`w1`, `i2`), states, times, counts.
2. **A validated value**: a Tailterm id matching its pattern (`tsk_`, `agt_`, `run_`, `wi_`, `ohr_`, `dlw_`, `obl_`, `tqe_` and 16 hex digits), a Board message number of 1 to 12 digits, a time, a five-field cron schedule of numbers, ranges, lists and steps within each field's range, a UUID session id, a `toolu_` tool-use id, or a value from a fixed list.

No field accepts words. A note that needs to say what an instruction, a decision or a claim is points to the Board message that says it, by number. A value that fits neither kind is refused and nothing is written; nothing is trimmed or repaired into shape. The record therefore holds no text from the session, the Board, the transcript or the environment, and so no token, key, password or vault content. `TestHandoffNoText` offers a sentence, a password, a PEM block, a `Bearer` header, hex and base64 strings, a file path, malformed ids, malformed numbers and malformed schedules to every field of every note, and random printable strings besides.

Three identity values are names, checked against the patterns the hub already enforces: the helper's agent name and its tmux session name (letters, digits, `_` and `-`, at most 64), and the hub's address (scheme, host and port only).

A record is read back through the same check. A file holding any other value, such as one edited by hand, is not printed and not injected: the hook then prints one fixed line and `tt handoff show` refuses. The restore report is held to the same rule: a reason is a short name from a fixed list (`hub-refused`, `no-binding`, `earlier-start` and so on), never the hub's words, and `tt handoff status` refuses a report holding anything else.

What costs: the record alone does not say what an instruction is. The note and `tt handoff show` tell the reader to read each cited message, and a decision or instruction can be noted only once it is on the Board.

### The fields

**Identity**, copied from the host helper file (`docs/owner-helper.md`): hub, project, agent id and name, run, registration receipt id, runtime, runtime thread, tmux session name, id and creation time, and when it was copied. The helper file's request ids and hashes are not copied. It says what that file said at that time. It is not proof of a registration.

**Capture**, written by the hook: for the last session start, the last compaction and the last session end, the time, the `source`, `trigger` or `reason`, and the Claude Code session id; and whether a session is open, which is true from a start until the next end. Times are kept to the second and a `/clear` stamps an end and a start in the same second, so it is that flag, not the times, that tells the next start an end is missing. An unknown `source`, `trigger` or `reason` is stored as `other`.

*Who writes the stamps.* Only the helper's own runtime process does: an event whose payload carries a valid `session_id` equal to the thread the helper file registers. The environment's `CLAUDE_CODE_SESSION_ID` is not evidence of who sent an event, because a process started inside the helper's session inherits it. So:

- a payload naming another valid session id, from a process in the helper's tmux session, is written to `captures.jsonl` only, marked `candidate`. That is a successor session after `/clear`, and equally a `claude -p` run from the helper's own pane;
- a payload with no session id, or an invalid one, writes nothing at all, whatever the environment says. At a start an interactive session may still be shown the note.

`TestHandoffStampOwnership` runs a second process's start, compaction and end in each of those three forms and requires `record.json` to be byte-identical afterwards. A successor's own start reaches the record when `tt handoff restore` registers it.

From the hook's input only `hook_event_name`, `session_id`, `source`, `trigger` and `reason` are decoded; `cwd`, `transcript_path` and `custom_instructions` are not read into anything.

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
| `wake` | schedule; kind (`hourly-update`, `morning-summary`, `queue-check`, `obligation-check`, `authority-expiry`, `other`); the message that says what to do at the wake; expiry; and, written only by the wake commands, a receipt (restored or asserted, the session it is for, the tool-use id, the time), the last firing and a duplicate count | `active`, `expired`, `cancelled` |
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
tt handoff restore --task T
tt handoff status [--task T]
tt handoff wake confirm ID [--asserted] [--task T]
tt handoff wake due ID [--task T]
```

`tt --help` lists only `show`, `write` and `note` in this version; `tt handoff` with no arguments prints all of them.

- **`note`** makes no hub request. `set` changes the fields named. `close` cancels a wake, revokes an instruction, marks an order `done` (or `--state dropped`), closes a decision, and marks a claim `--state verified` or `--state refuted` with `--check N`, the Board message of the check. `--expires` takes an RFC 3339 time, a duration from now such as `12h`, or `none`; the stored value is always a time or `none`.
- **`show`** is local and works with the setting off. It prints the identity with its date, the capture stamps, the snapshot with its as-of time, the current entries of each list, then expired, revoked and cancelled instructions and wakes under "Expired: not authority", then the other closed entries, and for each list when it last changed and how many entries it dropped. With `--json` it prints the record with expiry applied.
- **`write`** reads the hub for the snapshot and rewrites the record. It reads the helper agent, its delegation windows, the obligations it owes, one Board message per obligation for who sent it, the open owner decisions and the team queue. It reads no work-item record, makes only `GET` requests and marks nothing delivered. `--out PATH` also writes the same text `show` prints, with the destination's next commands, for a person or another runtime to read.

- **`restore`**, **`status`** and the two **`wake`** commands are described under "Restoring a session" and "Wakes" below. `restore`, `wake confirm` and `wake due` need the setting on; `status` is local and works with it off. `status` and `wake confirm` judge receipts only when run inside the session they speak for; see "The latest start is read, not only captured". Changing a wake's schedule with `note wake set` clears its receipt.

`note`, `write` and the `wake` commands act for the helper registered from this runtime session, or with `--task` for that project's helper on this host. A Claude or Codex session other than the registered one is refused, and so is an agent that is not the helper. `show` also works from a new session in the helper's tmux session, and when the host has only one record.

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

The next action is one sentence: "Next: run `tt handoff restore --task T`." The note names no command this binary does not have; `TestHandoffInjection` takes every `tt` command out of the note and runs it through the binary's own dispatch.

### Only the pane's interactive session

The note ends with "run `tt handoff restore`", so it is printed only into the pane's interactive Claude Code session. A one-shot process started in the helper's pane, such as `claude -p` run by the helper's shell tool or typed at the pane's prompt, is matched by rules 1 and 2 like any other process there: it inherits `TMUX_PANE`, and with no session id in its payload it inherits the helper's session id too. It is logged as before and shown nothing.

The environment cannot tell the two apart, so the hook reads the host's process table once (`ps -A -o pid=,ppid=,pgid=,tpgid=,tty=,lstart=,args=`) and walks up from itself to the nearest Claude Code process. A runtime is recognised by its program: `claude`, wherever it is installed; the versioned binary that name links to (`.../claude/versions/2.1.296`), which is also how `ps` shows some sessions and how Claude Code starts its own children; and `node` running the CLI or the agent SDK's CLI. Because a one-shot under a name this list does not hold would be passed over for the session above it, the hook also reads `CLAUDE_PID`, which every Claude Code runtime sets for its own children: when it is set it must be the process found, or nothing is printed (`unrecognised-runtime`). That process is the pane's interactive session only when all three hold:

| Check | Refused as | What fails it |
| --- | --- | --- |
| No argument is `-p` or `--print`, alone, with a value (`--print=`), or inside a combined short flag such as `-cp` | `print-mode` | `claude -p`, wherever it was started |
| No other Claude Code process is above it | `nested-claude` | anything a Claude session started, with or without `-p` |
| It has a controlling terminal and is that terminal's foreground job | `no-terminal` | a process started by a shell tool (which has no terminal), or one in the background |

With no Claude Code process above the hook (`no-claude-process`), or when `ps` fails or does not answer in time, nothing is printed: a process that cannot be placed is not invited to restore. `tt handoff restore` makes the same check as guard g7.

The fixture is what `ps` showed on 2026-10-10 (Claude Code 2.1.296) for the start hook of a `claude -p` run from an interactive session's shell tool, in an isolated project with only that hook: the one-shot process had terminal `??` and foreground group 0 and sat under the interactive `claude`, which had terminal `ttys010` and was its foreground job. `TestHandoffOneShotProcess` holds those rows, `TestHandoffOneShotNote` runs the hook under each shape, and the live check's working run, run again on this change on 2026-10-10 at 08:33Z, showed the note still reaching a real interactive session at its start, after a compaction and, as a candidate, after `/clear`, with the host's own `ps`.

Not compared: the terminal against the pane's own. Guard g1 already places a restore in the pane, and a process that inherited the pane's environment from the helper is a descendant of the helper and fails the second or third check.

A session that is killed runs no `SessionEnd` hook. Nothing is lost by that: the notes were written when they were made. Only the end stamp is missing, and the next start says so. That holds for the registered helper session. A session that was never registered, such as a successor killed before it ran `tt handoff restore`, was never the helper and left no start stamp, so its missing end is not reported.

## Restoring a session

After a `/clear` or a restart the new session is shown the note and runs:

```sh
tt handoff restore --task tsk_...
```

### The guards

All seven are checked before anything is written to the hub, the helper file, the relay binding, the tmux tags or the record. A refusal names its guard, says what a person could run instead, prints nothing else, writes no `restore.json` and exits non-zero.

| Guard | Passes when |
| --- | --- |
| g1 In tmux, one pane | The command runs in the tmux pane `TMUX_PANE` names (the same check `tt helper register` makes), and that session has exactly one pane |
| g2 Exact candidate | Exactly one helper file on this host names the project, and either its thread is this session's or its tmux session id and creation time are this pane's. A file naming another tmux session means the helper lives elsewhere |
| g3 Current run | The hub's helper is the file's agent and its current run is the file's run |
| g4 Status | The hub's helper is running, done or needs-input. Closed or exited is refused, because registering would make a new agent or run. Retired is refused with "retired; only tt resume re-enables it" |
| g5 No competing successor | The helper file holds no pending registration from another session and no unfinished `tt helper register` of this session's own, and after restore has the record lock the file still names the run restore read first |
| g6 Project active | The project is not paused, cleaning up for a pause or resuming |
| g7 Interactive session | For a Claude Code session: the command runs under the pane's interactive Claude Code process, by the process table and the three checks under "Only the pane's interactive session". A `claude -p` or other one-shot process started in the helper's pane is refused with the reason: started in print mode, started by another Claude Code process, not the foreground job of a terminal, started by a runtime other than the one found (`CLAUDE_PID`), no Claude Code process above the command, or the process table could not be read. It is checked after g1, before the hub is asked anything |

When g3 refuses, restore prints the hub's current run and "host of the current registration", the host on the helper's agent row, which only a registration sets. It prints "registration time: unavailable": the agent row has no registration time, and the one route that returns the registration event pages through the whole project feed. It adds that if this session's own registration answer was lost, `tt helper register --task T` is the recovery. Restore itself never resends a request in order to take over a run it did not expect.

### The registration is conditional

Restore reads the helper file once at its start and fixes that file's run as its expected predecessor, before tmux, the hub or a lock is touched. It sends that run, and never a later one, as `expectedRunId` on the ordinary register request (`docs/owner-helper.md`). Inside the hub's one register transaction the run is replaced only if the helper's current run is still that one and the helper is running, done or needs-input. Otherwise the hub answers 409 with the reason and writes no agent change, receipt, event or saved request. So a restore that raced another registration, a retirement or a close loses cleanly: the guards are an early, readable refusal, and the transaction is what makes it safe.

On the host, every writer of the helper file is one function that holds an exclusive lock on `<helper file>.lock` from before it loads the file until it returns, so `tt helper register` and a restore cannot interleave. With an expected run that function also refuses, before any local write, a result the hub marks as a replay and a helper that is no longer running, done or needs-input when it is read back.

### What it reports

Once the guards pass, each step is printed as restored, already correct, or not restored with the reason:

1. **Registration.** Already correct when the helper file names this session; nothing is sent. Otherwise this session is registered, and the new run and the receipt id are printed. A hub refusal is printed as the hub worded it.
2. **Wake binding.** The relay binding is read back and must name this thread and run. Restore does not write it; the registration did.
3. **Handoff record**, after a registration only: the record takes the new identity, and this session's own start, which the hook had logged while the session was a candidate, becomes the last start.
4. **Hub state.** The helper's status, its delegation window, the obligations it owes, the open owner decisions and the queue entries launching or running are read live and printed, each list with "N closed since the record, M new" against the record's snapshot, and new rows marked. The stored snapshot is not replaced; only `tt handoff write` does that. A list that left rows out counts only the rows shown.
5. **Wakes**, as below.
6. **Instructions and claims.** Active instructions with their expiry, expired and revoked ones under "not authority", and claims still unverified. They are printed for the session to read and never fail a restore.

It writes `restore.json` and exits non-zero while the registration, the wake binding or the hub read is not restored, or any active wake is not restored or only session-asserted. `tt handoff status` prints the same steps later, with each wake's state at that moment, and exits 0.

Restore never opens or renews a delegation window, acknowledges or answers an obligation, changes a queue entry, resumes a retired helper or acts on an instruction. Its only request that can change the hub is the one registration; `TestHandoffRestoreScope` checks the request log and the hub before and after. `TestHandoffRestoreOneShot` runs restore from each one-shot shape in the helper's pane, with the successor's session id and with the helper's own: g7 refuses, the hub receives no request at all, the hub's state, the helper file, the bindings, the tags and the record are unchanged and no `restore.json` is written; the interactive session in the same pane then restores.

## Wakes

A wake is a cron the session created with its own tool. `tt` cannot create one or list them, so it does not take the session's word as proof. For the session asking, an active wake is in one of three states:

- **Restored.** A tool-ledger row (`docs/claude-wake.md`) shows this session calling `CronCreate` with outcome `ok`, later than this session's latest start, with an arguments digest equal to the digest of the exact arguments restore printed for that wake. One row backs one wake only; the receipt stores its tool-use id. It is printed as "this session created its cron at T". That is what the row proves: creation, at that time, not that the cron still exists.
- **Session-asserted.** The session ran `tt handoff wake confirm ID --asserted`. It is printed as "session-asserted, not verified" and counts as not restored for restore's exit code. The command itself exits 0.
- **Not restored.** Neither, with the reason.

Every wake is created with the same arguments: its stored schedule as `cron`, and the fixed prompt ``Tailterm handoff wake <id>. Run tt handoff wake due <id> --task <project>.`` Restore prints them as one JSON object per wake. Its instruction is: list this session's crons; if one already has that wake's prompt, keep it and run `tt handoff wake confirm ID --asserted`; otherwise create it with exactly the printed arguments and run `tt handoff wake confirm ID`. A cron that survived therefore shows as session-asserted, which is the truth: `tt` did not see it created.

`tt handoff wake confirm ID` without `--asserted` only searches the ledger. It accepts no cron id or other value from the session.

**A start resets them.** A receipt counts only for the session it names and only while it is later than that session's latest start. So after a start of any source (`startup`, `resume`, `clear`, `compact`, `fork`) every wake shows as not restored until confirmed again. Compaction is included on purpose: whether a session's crons survive one is not known. Another Claude process in the pane does not reset the helper's wakes.

**The latest start is read, not only captured.** The hook's stamp alone is not enough: a later start whose hook ran past its deadline, or ran while the state folder could not be written, leaves no stamp, and the receipts from before it would still count. So `restore`, `status` and `wake confirm` each read, for the session asking, evidence the hook does not write, and judge a receipt against the latest of all of it (owner decision in order #31789: fail closed):

- **The session's own transcript**, found by its session id under `~/.claude/projects` (or `$CLAUDE_CONFIG_DIR/projects`): the first row with a time (a new session's file begins at its start, and `/clear` begins a new file), every `SessionStart` hook row whatever its outcome (`hook_success`, `hook_cancelled`, `hook_non_blocking_error`), and every `compact_boundary` row. Only a row's type, subtype, hook event and time are decoded; nothing from the transcript is stored or printed.
- **The start time of the session's Claude Code process**, the nearest one above the command in the process table. A resume writes no row: on 2026-10-10 (Claude Code 2.1.296) a session resumed with `claude --resume`, interactively and with `-p`, ran its `SessionStart` hook with source `resume` and its transcript gained nothing for it, where its startup and its compaction had each left a hook row and the compaction a boundary row. `claude --resume` is a new process, so the process start covers it.
- **The captured start**, as before. A session with none captured still gets "no start was captured".

`wake confirm` searches the ledger from that same latest start, so a cron created after a lost start can be confirmed and one created before it cannot. When either reading fails nothing counts, with a named reason: `transcript-unreadable` (no transcript for the session, one that cannot be opened, or none with a timed row) or `session-process-unknown` (the process table could not be read, no Claude Code process is above the command, or the command was typed in a shell outside the session). `TestHandoffWakeFloor` covers a later start lost to a late hook and to an unwritable state folder, each kind of transcript row, a resume, and both failed readings, through `status`, `confirm` and `restore`.

**Expired and cancelled wakes** are printed and never offered: no arguments are printed for them and `confirm` and `due` refuse them.

**Duplicates.** `tt handoff wake due ID`, which every wake's cron runs, prints the wake's kind and Board message number and records the firing. A second firing within half the schedule's period prints "duplicate cron for wake ID: delete the extra one" and is counted in `tt handoff status`. A doubled cron is thus caught at its first double firing.

**Codex.** A Codex session has no session timer. Restore reports every wake as "not restored: this runtime has no session timer" and offers nothing; the wakes stay active in the record for a later return to Claude Code.

### The stage B cron check

Whether the ledger really sees a session create a cron, with a digest `tt` can predict, was checked before it was relied on (criterion b9, design condition c2):

```sh
cd hub
TT_LIVE_CLAUDE=1 go test ./cmd/tt -run '^TestHandoffLiveCronCheck$' -count=1 -v -timeout 20m
```

It is the live check's harness (below) with two additions. The project settings also hold the candidate `tt hook tool` entries for `PreToolUse`, `PostToolUse` and `PostToolUseFailure`, each a wrapper that runs the candidate under `env -i` with its ledger in the run directory. And the record holds one wake, on a schedule that cannot fire during the check. The session is asked for one cron with exactly the printed arguments, to run the candidate's `tt handoff wake confirm`, and then to delete the cron. Because a receipt is judged against the session's own transcript, the script the session runs first copies that one file from the real home into the run directory (the script does, not `tt`), points the candidate at the copy with `CLAUDE_CONFIG_DIR`, and after the confirm prints `tt handoff status` from inside the session. The harness's own confirm, outside the session and with no transcript, must be refused. The isolation proofs are the same three, with the candidate's own tool entries allowed.

**Result.** Run on 2026-10-10 at 04:21Z on the Mini with Claude Code **2.1.296**, on the stage B candidate. **It passed, with the first outcome: a real ledger row restored a wake, so ledger matching ships switched on.**

- The ledger recorded three tool calls, all `ok`: `ToolSearch`, `CronCreate` and `Bash`.
- The `CronCreate` row carried the session's id, tool-use id `toolu_01DaETr6BDNzVFRQZLXnWtdp` and arguments digest `e6cdb6e0c2d0033020f2979858bef80fad02ef9f06dd0bb85e933626fd26f0f1`, which is the digest of the printed arguments. The session sent `cron` and `prompt` and no optional argument.
- `tt handoff wake confirm w1` printed "wake w1 restored: this session created its cron at 2026-10-10T04:21:22Z" with that tool-use id, and `tt handoff status` showed it restored.
- The session deleted its cron through `CronDelete`.
- Isolation: 64 paths opened, all under the run directory; only the expected session entries ran; the real settings file's hash and the real handoff directory were unchanged.

That row, with its ids, tool name, outcome, time and digest and nothing else, is the fixture of `TestHandoffWakes`, subtest "the row of the live cron check", which also requires `tt` to print for that wake the arguments whose digest the session's call had.

**Run again on 2026-10-10 at 08:31Z** with the same Claude Code, on the candidate that judges a receipt against the transcript and the process start (bug `wi_4fb9a73b93635e68`, order #31789): it passed. Run from inside the real interactive session, `confirm` restored the wake from the `CronCreate` row and `status` showed it restored, so the host's own `ps` placed the command under the session's Claude Code process and the session's transcript was read. The harness's confirm outside the session was refused: "this session's transcript could not be read". 69 paths opened, all under the run directory.

Not covered by the check: a session that adds `recurring` or `durable` to the call. Its digest differs, so it gets no receipt and the wake stays not restored; it can be asserted. Also not covered: a cron firing, and a compaction, since the schedule was chosen not to fire.

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

4. The next compaction or `/clear` in that session shows the note. Follow its next action: `tt handoff restore --task tsk_...`. After a `/clear` it registers the new session; after a compaction it finds the registration already correct. Either way it then lists each wake to create again and confirm.

   Wake receipts need the tool ledger hooks installed as well (`docs/claude-wake.md`). Without them a wake can only be session-asserted.

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

What the model is shown is checked two ways: the pane is read for failed-hook lines, and the model is asked whether its context holds a hook error. A working hook must leave neither. A hung entry may leave at most the one line Claude Code prints when it cancels a hook.

### Result

Run on 2026-10-07 on the Mini with Claude Code **2.1.293**, on the stage A candidate. **It passes**, under criterion a16 as the owner helper amended it in #28733: a working hook shows the model no hook error, and a hung hook shows at most the one line Claude Code itself prints when it cancels it. The figures are from the run started at 20:23:15Z. Four earlier full runs the same day observed the same behaviour; see "History of this result".

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

**A working hook shows no hook error.** Asked after the start and again after the compaction, the model reported none, and the pane showed no failed hook.

**Hung entries, one at a time.** In each run one entry slept 100 seconds with `"timeout": 5`. The cut-off is the time from the entry's own start, which it logs in whole seconds, to the line in Claude Code's debug output that gives it up.

| Run | Hung event | Cut off after | What the session did | Against the working run | What the model was shown |
| --- | --- | --- | --- | --- | --- |
| H1 | `SessionStart` | 5.3 s (`timed out after 5000ms`) | The prompt was ready at once; the first reply waited for the cut-off | prompt ready 0.5 s against 0.7 s; first reply 7.1 s after launch against 2.7 s | Nothing; it reported no hook error |
| H2 | `PreCompact` | 5.5 s (`cancelled`) | The compaction completed | `/compact` to the end of the hook 5.0 s against 0.1 s, 4.9 s more | One line, `PreCompact [command] failed: Hook cancelled`; it reported a hook error |
| H3 | `SessionEnd` | 5.7 s (`cancelled`) | The process exited | exit 5.6 s against 0.3 s, 5.2 s more | The session was over |

All within the 7-second budget. After each run no hook process was left. For H2 the time is taken to the end of the hook and not to the end of the compaction: the summary a compaction writes took 13 to 26 seconds across these runs, which varies by more than the cut-off (26.1 s in H2 against 20.0 s in the working run).

**Failing entries (exit 1, text on stderr), one at a time.** F1 `SessionStart`, F2 `PreCompact`, F3 `SessionEnd`: the session started, compacted and exited as in the working run (prompt ready 0.6 s; exit 0.8 s against 0.3 s). In F1 the model reported no hook error. In F2 it was shown `PreCompact [command] failed:` followed by the entry's stderr text. This is recorded and not judged: `tt hook handoff` never exits non-zero and never writes to stderr.

**Known limit with Claude Code 2.1.293: the `PreCompact` line.** Claude Code prints the outcome of a `PreCompact` command hook under `/compact`, and that output is part of what the model then sees. Nothing in `tt hook handoff` can change it.

- If the hook process hangs past 5 seconds, Claude Code cancels it, the compaction completes, and the model sees one line: `PreCompact [command] failed: Hook cancelled`. The check allows exactly that line and fails on anything more.
- For a working hook Claude Code prints `PreCompact [command] completed successfully` in the same place. The model does not report that as an error.
- Not observed: an automatic compaction, where there is no `/compact` output.

**History of this result.** The criterion first read "the model is shown no hook error" in every run. Two full runs at 20:02Z and 20:05Z failed it on H2 for the line above, and the owner helper amended the criterion (#28733). A run at 20:20Z under the amended check failed on a different point, a timing that compared the whole compaction (25.5 s against 16.4 s) with the 7-second budget; that measure was replaced with the time to the end of the hook, as in the table. The run at 20:23Z is the first under the final check. After the round one review fixes (the handler kills its own `tmux` child, and the open-session flag), the check was run again at 20:31Z because the first changes the query that recognises a candidate successor after `/clear`: it passed with the same observations (cut-offs 5.3, 5.7 and 5.2 s).

**Other things learned.**

- Claude Code shows the prompt without waiting for `SessionStart` hooks. A hung one delays the first reply, not the prompt.
- A project directory under a folder the user already trusted showed no trust dialog.
- The debug output names the command for `PreCompact` and `SessionEnd` hooks and for a `SessionStart` hook that times out. For a `SessionStart` hook that succeeds it gives the event and the output without the command, so for those the check counts results against the wrapper log.
- Not tested: `SessionEnd` when the terminal or tmux session is killed, `source` `fork`, `trigger` `auto`, and settings picked up by a session that is already running.

## Known limits

- **Restore does not create wakes.** It registers the session and says which wakes to create again. The session creates each cron and confirms it.
- **A receipt proves creation, not that the cron still exists.** A cron deleted later is not seen. Claude Code's recurring session crons also expire after 7 days, by the tool's own description, so a wake confirmed once stops firing after that with nothing recording it.
- **A receipt needs the session's start to have been captured.** If the session's only start hook ran out of time, `confirm` says no start was captured and the wake can only be asserted. Times are kept to the second, so a row in the same second as a start does not count.
- **A later start the hook lost is found another way, and only from inside the session.** When a later `SessionStart` leaves no stamp (its hook ran past the deadline, or the state folder could not be written), the transcript and the process start still place it, and earlier receipts stop counting. That needs the command to run under the session's own Claude Code process: `status` or `wake confirm` typed in a plain shell shows every receipt as not restored, "not running under this session's Claude Code process". A long transcript is read from start to end each time.
- **The one-shot check reads arguments as `ps` prints them.** An interactive session started with a prompt argument holding a bare `-p` or `--print` word, or a word of letters after one dash that holds a `p`, is refused as print mode; start the helper without one.
- **A runtime is recognised by its program name.** A Claude Code under a name outside the list above is not recognised. As the session itself, restore refuses it: no Claude Code process is found. As a one-shot under the helper it is caught only by `CLAUDE_PID`, which Claude Code 2.1.296 sets for hooks and shell tools; a runtime that neither has a listed name nor sets `CLAUDE_PID` would be passed over and judged as the helper above it.
- **A session Claude Code runs under its own background host** (`claude --bg-pty-host`) has a Claude Code process above it and is refused as started by another one. The helper is a session started in its tmux pane.
- **An in-session `/resume`** switches conversation without a new process. Whether its transcript gains a row was not tested; if it does not and its start hook is also lost, receipts from that conversation's earlier life would still count.
- **The one-shot check is for Claude Code.** A Codex helper's restore is not checked by g7; a `codex exec` run from a Codex helper's pane is not covered.
- **g3 cannot say when the other registration happened**, only from which host. A route that returns the receipt for a run would be a follow-up.
- **The session keeps the notes current.** No hook can know that a session scheduled a wake or took a decision. Every list shows when it last changed, so a stale record looks stale.
- **The record is not proof.** Its identity is what one host file said at a time, and its snapshot is what the hub said at a time. Only `tt helper env` or `tt helper register` checks a registration with the hub.
- **A second interactive Claude session in the helper's tmux session** is shown the note as a candidate successor. The note says it is not registered. With two panes, g1 refuses a restore.
- **A wake's line in the note includes its schedule only when the line fits in 120 bytes.** `tt handoff show` always prints it.
- **Decisions stay current until closed.** A list of 50 recorded decisions refuses a new one until one is closed.
- **Claude Code 2.1.293 reports the `PreCompact` hook under `/compact`**, where the model sees it: "completed successfully" normally, and the one line "failed: Hook cancelled" if the hook process hung past 5 seconds. The compaction completes either way. See the live check's result.
- **`tt doctor` does not check the new hook.**
- **Claude Code only for capture and wakes.** Codex runs no capture hook and has no session timer. A Codex helper can use the commands, and restore verifies its registration.
- **One host.** The record is not uploaded or copied. Another host has its own.
- **Whether `SessionEnd` runs when a terminal or tmux session is killed** was not tested. The design assumes it may not, and the next start reports the missing end.
