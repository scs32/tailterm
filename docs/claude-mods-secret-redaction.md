# Secret redaction mod for Claude sessions

`tools/claude-mods/secret-redaction` is a Claude Code mod (feature
`wi_627ff8a50b091502`). It closes the gaps the secret redaction settings hook
(`wi_79001641d97cd64c`, see the secret redaction section of
`docs/claude-wake.md`) cannot close. Before a row is stored or sent to the
model, it has `tt` replace recognizable tokens and keys in:

- a typed or pasted prompt;
- a prompt typed while a turn is running (a queued prompt), and its queue
  records;
- a file attachment, the text of an `@file` mention;
- the output of a **failed** tool call.

The settings hook already covers the output of a successful tool call, so the
mod leaves those rows alone. It also leaves alone tool input, the model's
responses, hook context, compaction summaries and every other delivery (a
peer's message, a relay event).

**Nothing loads it.** No launch path, setup step or release installs the
folder, and nothing is switched on (decision MD3). The release ships only the
`tt hook redact` handler inside `tt`, which does nothing until a session loads
the mod and the host setting says so. Codex sessions are not covered: the mod is
for Claude Code only. Today's owner helper is a Codex session.

## How it works

The mod hooks `session.append`, which fires once for each row before it is
stored. For a covered row it collects the row's text and runs
`tt hook redact`, with a 1000 ms limit:

```
{"v":1,"door":"prompt","tool":"","texts":["..."]}      -> standard input
{"v":1,"texts":["... [tt-redacted:github-token] ..."]} <- standard output, redact mode only
```

The text goes on standard input only, never in the command line or the
environment, which other processes can read. The mod passes the session id
(`CLAUDE_CODE_SESSION_ID`) so `tt` can find the owner helper registered from
that session; a mod's child does not inherit it.

`tt` does everything else, with the hook's own code in
`hub/cmd/tt/tool_redact.go`:

- **Scope.** The same as the tool-call ledger: a Tailterm agent's environment
  identity, or the owner helper registered on this host from this exact Claude
  session. Any other session gets no answer and no ledger row, and `tt` does
  not even read its standard input. The owner's unrelated sessions are never
  changed.
- **Setting.** The hook's own key (decision MD1), read on every covered row.
- **Patterns.** The hook's one definition: host values (group A), prefixed
  tokens and private keys (group B), counted-only context (group C). The mod
  holds no pattern; `tests/secret-redaction-mod.test.js` checks that.
- **Answer.** In `redact` mode, with a group A or B match, the same number of
  strings, each match replaced by `[tt-redacted:<pattern name>]` and every other
  byte unchanged. Group C is counted, never replaced. In every other case `tt`
  prints nothing.

The mod uses the answer only when `tt` exits 0 and prints exactly that object
with one string per text. Otherwise it stores the row exactly as Claude Code
made it.

## The setting

The host setting is the hook's: `claudeSecretRedaction` in
`~/.config/tailterm/relay.json`. One switch governs both. A missing file, a
missing key and any value other than these mean off:

```
{"claudeSecretRedaction": "report"}
```

- **off** (the default): `tt` reads nothing and writes nothing. The cost is one
  `tt` start per covered row, about 10 to 25 ms.
- **report**: counts only. Each covered row adds one `mod_scan` row to the
  agent's private tool ledger
  (`~/.local/state/tailterm/tool-ledger/<agent>/ledger.jsonl`). The row
  holds the door in `reportTag` (`prompt`, `delivery`, `queue`, `attachment` or
  `tool-result`), the tool for a failed call, `redactMode`, `redactCount`,
  `redactKinds` and any `redactSkip`. It never holds the text, nor any part,
  length, position or hash of it.
- **redact**: also rewrites. **Do not switch it on until the owner has answered
  D7.** A file attachment the model sees with a placeholder can be written back
  over the real value by a whole-file write or an edit that copies the
  placeholder. The owner decides whether to accept that risk after a report
  week. `redact` is never set without the owner's explicit yes.

## Fails open, within bounded time

Every failure keeps the row as Claude Code made it:

- `tt` is missing or cannot start, or is still running at 1000 ms;
- `tt` exits non-zero, prints nothing (off, out of scope, report mode, no
  match), prints something other than the one expected object, or the wrong
  number of strings;
- the mod's own hook fails: its `.catch` stores the row unchanged.

`tt` itself gives up within the ledger's 150 ms deadline, scans no input over
512 KiB (`redactSkip: "oversize"`) and stops a scan at 100 ms
(`redactSkip: "timeout"`). It never writes to standard error and always exits 0.

An older `tt` does nothing: it has no `redact` hook, so it exits non-zero or
prints nothing, and the mod keeps the row.

## What it does not protect

The model and the API request receive only the rewritten form. Some local copies
keep the original (decision MD2, accepted 10-29):

- **The transcript file still holds the original** in three places that a mod
  cannot rewrite:
  - a failed tool call's structured record, `toolUseResult`;
  - an attached file's payload, `attachment.content.file.content`;
  - a queued prompt's payload, `attachment.prompt`.

  The rows the model reads (`message.content`), the recorded API request and
  `lastPrompt` hold the placeholder.
- **Prompt history**, `~/.claude/history.jsonl`, keeps every typed, queued or
  pasted prompt of an interactive session exactly as typed. No mod event
  reaches it.
- **Claude Code telemetry is not protected.** Nothing here was tested against
  it.
- The owner's own screen may draw a queued or pasted prompt as typed, before the
  rewrite.

So the mod protects what leaves the host for the model. It does not make the
host's own files clean.

## Loading it (not done; needs its own item)

Activation is decision MD3 and needs its own item and order. For the record:

- An agent session would need `--plugin-dir <absolute path of a copy of the
  folder>` on the `claude` command tt composes, from a copy that tracks the
  released `tt` rather than the Mini checkout.
- A Claude owner helper session would be started by hand with the same flag.
- **Never** put the folder in `CLAUDE_CODE_PLUGIN_DIRS`, in a shell or in a
  settings file's `env` block. Every Claude Code session on the host would then
  load the mod, the owner's unrelated ones included (tt would still leave their
  rows unchanged).
- Point `--plugin-dir` at a copy, not the repository folder. An interactive
  session writes editor files into the folder it loads (see the clean view
  section of `docs/claude-mods.md`). The tests below load a temporary copy.

## What has been checked

On Claude Code 2.1.312, on the Mini, on 10-29:

- `go test ./cmd/tt -run Redact -count=1` (from `hub/`): the handler's tests
  for off and malformed settings, scope (no identity leaves standard input
  unread and the ledger root untouched; the owner helper of this session acts
  as an agent), report rows, the redact answer byte for byte, the size cap, the
  scan budget and a held standard input, beside the hook's 19 existing tests.
- `claude plugin validate --strict --json tools/claude-mods/secret-redaction`
  passes with no error or warning. Its notes list `session.append` and
  `$.process.run`. The directory findings (no LICENSE, no icon) are reported
  for clean view too and do not fail it.
- The engine's test kit, on a temporary copy with a stand-in for `tt`: the five
  covered row kinds (a multi-line pasted prompt, a `queued_command` delivery, a
  queue record, a file attachment, a failed tool result as a string and as
  text blocks) are stored rewritten; a successful tool result, another
  delivery, a response and hook context never reach `tt` and are stored
  unchanged; a rejected run, a timeout, exit 1, empty output, malformed JSON, a
  wrong count and a non-string each store the row unchanged.
- Sandbox probes, with a freshly built `tt`, disposable `claude -p` sessions
  with the mod loaded from a temporary copy, an environment built from nothing
  and every Tailterm path under one temporary root. `HOME` is the real one,
  because Claude Code needs its login. Each value was a random GitHub-token
  shape, generated for the run and never printed.
  - **Agent, redact:** a note in the prompt, an `@file` attachment and a Bash
    command that printed a value and exited 3. The reply held none of the
    three values and three placeholders. The sandbox ledger had one `mod_scan`
    row for each of `queue`, `prompt`, `attachment` and `tool-result` (tool
    `Bash`), each counting one `github-token`.
  - **Agent, report:** the same three values reached the reply, with no
    placeholder, and the same four rows had `redactMode: "report"`.
  - **Owner helper of this session:** no agent identity, a Claude owner-helper
    file registered under the session's id (`--session-id`). The reply held
    the placeholder, not the value; the rows are in that helper's ledger with
    its task and run. So `$.session.id()` is the id the helper registers
    under.
  - **No helper file, or a helper file for another session:** the reply held
    the value and no ledger row was written.
- Transcript findings, as observed (not passed or failed):
  - Redact: the prompt's value was nowhere in the transcript. The file's value
    remained in `attachment.content.file.content` and the printed value in the
    failed call's `toolUseResult`. Everything else held the placeholder.
  - Report, for comparison: the values were in `message.content`, the queue
    record, the recorded API request, `lastPrompt`, the attachment's
    `rendered` text and the model's reply.
  - `paste-cache`, `file-history`, `session-env`, `shell-snapshots`, `todos`
    and `sessions` gained no copy. The non-interactive probes do not read
    `~/.claude/history.jsonl`; the design's probe found that `claude -p` wrote
    nothing there.
  - Cleanup: none of the generated values was in the working tree, and the
    temporary root and the probe's transcript folder were removed.
- Not checked: Claude Code's `--debug` log, and telemetry.

## Check it after each Claude Code upgrade

The mod API is early access and Claude Code updates itself, so the tests that
need the installed Claude Code are opt-in. The default unit run (`npm test`)
runs the manifest, "nothing loads it" and "one definition" tests and skips the
rest, with a reason naming `SECRET_REDACTION_CLAUDE_CHECK`. It starts no
`claude` process.

Run the opted-in check on purpose after each Claude Code upgrade. From the
repository root:

```
SECRET_REDACTION_CLAUDE_CHECK=1 node --test tests/secret-redaction-mod.test.js
```

Expect 11 pass, 0 fail and 1 skipped (the interactive probe). It prints the
Claude Code version, the pre-start sandbox assertions and the transcript
findings. It takes about a minute and makes five short `haiku` calls. With the
variable set, a missing `claude` fails the tests; it does not skip them.

### The interactive probe (not run yet)

Queued and pasted prompts only happen in an interactive session. The probe for
them starts one on a private tmux socket, in redact mode. It queues a prompt
during a running `sleep` and makes a bracketed paste, each carrying the sandbox
`TAILTERM_TOKEN`, a random string of 24 lowercase letters and digits that `tt`
matches as a host value. It then checks that both replies in the transcript
hold `[tt-redacted:host-credential]` and not the value, and kills its tmux
server.

It has not been run in this build (decision MD4, 50833). An interactive
session writes every prompt to the owner's real `~/.claude/history.jsonl`, which
is live profile data, so it runs only when the owner says so:

```
SECRET_REDACTION_CLAUDE_INTERACTIVE=1 node --test tests/secret-redaction-mod.test.js
```

The probe never changes `~/.claude/history.jsonl`. It prints how many lines it
left there holding its random value; expect 2 (the queued and the pasted
prompt). They hold only that random string, not a secret. To remove them, when
no Claude Code session is running on the host, drop the lines holding the
probe's own wording. First count them with `grep -c` on the same two phrases;
the count should match what the probe printed.

```
(umask 077 && grep -v -e 'Reply with the token in this queued note' -e 'Reply with only the key= line from my pasted config' ~/.claude/history.jsonl > ~/.claude/history.jsonl.new && chmod 600 ~/.claude/history.jsonl.new && mv ~/.claude/history.jsonl.new ~/.claude/history.jsonl)
```

The command keeps the file private: the rewritten copy is created owner-only
(mode 600) before it replaces the original.

The design probe of 10-29 used the same wording, so this also removes its lines.
