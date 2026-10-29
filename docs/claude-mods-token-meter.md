# Token meter mod

`tools/claude-mods/token-meter` is a Claude Code mod for one session at a time.
It shows the tokens of the last turn and a running total for the session, in a
status line of its own, and `/token-meter` breaks them down. Like every mod in
`tools/claude-mods/`, it is a plain plugin folder: nothing installs it and
nothing loads it.

## What it does and does not do

It is **observe-only**. It sets no cap or budget, denies and rewrites no tool
call, prompt or answer, and posts nothing to the Board. Each turn it counts the
usage the engine hands it, sets its status line and passes the turn on
unchanged.

Its figures live only in that session's memory. They are not uploaded, not
written to a file and gone when the session ends. **It is not a ledger.** The
hub's usage ledger is the record and the authority for item, phase, team and
lifetime figures: `tt usage --item ITEM`, the budget line and the token
guardrail phases. The meter shows a live figure inside the session, turn by
turn, before the hub has any upload.

## What it shows

After each turn, its status line reads, for example:

```
tokens: turn 12.1k · session 1.24M over 14 turns · wi_0123456789abcdef
```

- **turn** is the last turn's tokens, **session** the sum of every turn so far.
  Each is the sum of the four classes the API reports: input, output, cache
  read and cache write. Units are those `tt usage` prints: `950`, `12.1k`,
  `6.71M`.
- The item label is `TAILTERM_WORK_ITEM` when the session has it. An owner
  session has none and shows no label. The session total is never split across
  items.
- **A turn with no usage.** A turn that ends on an interrupt or an API error
  reports no usage. It adds nothing to the total but still counts as a turn,
  and is never shown as `turn 0`:

  ```
  tokens: turn unavailable · session 1.24M over 15 turns (1 without usage)
  ```

`/token-meter` prints the last turn and the session total by class, the main
loop and subagents apart, the count of turns without usage, the item and agent
names when set, and the command that gives the hub's per-item and per-phase
figures: `tt usage --project TASK --item ITEM`, filled in from
`TAILTERM_TASK` and `TAILTERM_WORK_ITEM` when they are set. It makes no model
call.

### Subagents

A subagent's turns count in their own subtotal and in the session total. That
nothing is counted twice rests on the engine's **documented contract**, not on
a test of the engine. The types of Claude Code 2.1.312 say a turn's `usage` is
its own requests' counts summed, and that a subagent's turns are its own loop's
turns, apart from the main loop's. So the meter adds the two as separate. The
mod's test supplies its own turns, so it shows the meter's arithmetic, not how
the engine fills `usage`. If a later Claude Code changes that wording in its
types, the subagent subtotal may count a subagent twice; read the types at the
after-upgrade check.

### How its figures differ from `tt usage`

- They are the API's raw classes. The hub normalizes them (it splits Claude
  thinking out of output, for one) and attributes them to items and phases.
- They cover this one session only, with no phase.
- They are never uploaded.

So the two do not match exactly. Where they differ, the hub is right.

## Switch it on

Give the absolute path of the folder. This works from any folder. On the Mini
checkout:

```
claude --plugin-dir /Users/stephenspeicher/projects/tailterm/tools/claude-mods/token-meter
```

The Air uses the same command with its own checkout path once it has pulled.
The relative form, `claude --plugin-dir tools/claude-mods/token-meter`, works
only when `claude` is started from the repository root. From any other folder
Claude Code gives no error and opens a session without the mod.

## Switch it off

Start `claude` without the `--plugin-dir` flag. Nothing was installed and no
setting was changed, so there is nothing to undo. There is no in-session
switch.

## Agent sessions

Agent sessions never load it. Nothing in `tt host setup`, the hub, the relay,
the client or the launch scripts names the folder, and
`tests/token-meter-mod.test.js` checks that this stays true. Loading it in agent
sessions would be a later, separately ordered change.

Do not put the folder in `CLAUDE_CODE_PLUGIN_DIRS`. That environment variable
counts from any source: the shell, the `env` block of a settings file such as
`~/.claude/settings.json`, or a launch script. Every Claude Code session that
gets it, agent sessions included, would then load the mod.

## What has been checked

On Claude Code 2.1.312:

- `claude plugin validate --strict --json` passes with no error, warning or
  gating hook. It lists exactly three hooks: `session.start` (registers the
  command), `command.run{command=token-meter}` and `turn.complete`. It reads
  `TAILTERM_WORK_ITEM`, `TAILTERM_AGENT_NAME` and `TAILTERM_TASK` and writes no
  environment variable; its only other calls are its own state and
  `$.ui.status`.
- Under the engine's test kit (`claude plugin test` on a temporary copy), the
  test raises two main-loop turns, one subagent turn and one turn with no usage.
  Each turn sets exactly one status line with the exact turn and session
  figures, the no-usage turn reads `turn unavailable`, and `/token-meter`
  reports the exact class sums, main loop and subagents apart. A second run
  without an item shows no label. Each turn's answer passes through unchanged.

No check reads, starts, resumes or meters a live Claude session or transcript.

### Check it after each Claude Code upgrade

The mod API is early access and Claude Code updates itself, so the two tests
that need the installed Claude Code are opt-in. The default unit run (`npm
test`) skips both, with a reason that names `TOKEN_METER_CLAUDE_CHECK`, and
starts no `claude` process. From the repository root:

```
TOKEN_METER_CLAUDE_CHECK=1 node --test tests/token-meter-mod.test.js
```

Expect 4 pass and 0 skipped. With the variable set, a `claude` binary that is
missing or has no `plugin test` fails the two tests; it does not skip them.
Then read the `turn.complete` usage and `agentId` wording in the new types (see
Subagents).

### Not yet checked: how it looks

The checks show what the mod hands the engine, not what the terminal paints.
`$.ui.status` is the plugin's own line beside the engine's; the mod changes no
setting, so the host's status line command is unaffected. How the line looks in
a live terminal needs an interactive session and has not been tried.

### Known side effect

As with the clean view mod (`docs/claude-mods.md`), an interactive
`--plugin-dir` session writes editor files into the mod folder: a one-line
`tsconfig.json` and `.claude-plugin/types/`. Git ignores both and they are safe
to delete.
