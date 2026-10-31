# Claude Code mods

Mods kept in this repository live under `tools/claude-mods/`. Each is a plain
plugin folder. Nothing installs them and nothing loads them: a mod runs only in
a Claude Code session that someone starts with `--plugin-dir`.

## Pages

Clean view is described below. Each later Claude Code mod has its own page:

- [Token meter mod](claude-mods-token-meter.md): the tokens of the last turn
  and the session, in a status line. A plugin folder.
- [Secret redaction mod for Claude sessions](claude-mods-secret-redaction.md):
  closes the gaps of the secret redaction settings hook. A plugin folder.
- [Verification evidence capture for Claude sessions](claude-mods-evidence-capture.md):
  the command, exit code, output digest and commit of each verification run,
  on its tool-call ledger row. Part of `tt hook tool`, not a plugin folder.
- [Claim checker for Claude sessions](claude-mods-claim-checker.md): before
  `tt send` posts a RESULT or an accept, a warning when its cited commits,
  commands or receipts do not match the sender's own ledger. Part of
  `tt send`, not a plugin folder.

## Clean view

`tools/claude-mods/clean-view` is a viewing aid for the owner's own session. It
hides the tool call, tool result, tool group and tool progress rows in the
transcript, so the session shows the conversation without the tool traffic.

It changes only what is drawn. The model reads and does exactly what it would
without the mod, and the tool-call ledger still records every call. Approval
dialogs are a different component and still show.

### Switch it on

Give the absolute path of the folder. This works from any folder. On the Mini
checkout:

```
claude --plugin-dir /Users/stephenspeicher/projects/tailterm/tools/claude-mods/clean-view
```

The relative form works only when `claude` is started from the repository root:

```
claude --plugin-dir tools/claude-mods/clean-view
```

Claude Code gives no error when the folder is not found. Started from any other
folder, the relative form opens an ordinary session without the mod. The owner
hit exactly this on 10-10.

The session starts with clean view **on**. The first `/clean-view` turns it
off, which shows the tool calls.

### Switch it off

- In the session, type `/clean-view`. It answers
  `Clean view off: tool calls are shown.` Type it again to hide them again. The
  choice lasts for that session.
- For good, start `claude` without the `--plugin-dir` flag. Nothing was
  installed and no setting was changed, so there is nothing to undo.

### Agent sessions

Agent sessions never load it. Nothing in `tt host setup`, the hub, the relay,
the client or the launch scripts loads the folder. Its only mention there is the
release target map, which says nothing ships from it.
`tests/clean-view-mod.test.js` checks that this stays true.

Do not put the folder in `CLAUDE_CODE_PLUGIN_DIRS`. It is an environment
variable, and it counts from any source: the shell, the `env` block of a
settings file such as `~/.claude/settings.json`, or a launch script. Every
Claude Code session that gets the variable, agent sessions included, would then
load the mod.

### What has been checked

On Claude Code 2.1.296:

- `claude plugin validate --strict tools/claude-mods/clean-view` passes with no
  warning.
- A non-interactive session loads the mod and answers the command, with no
  model call:

  ```
  claude -p "/clean-view" --plugin-dir <absolute path>/tools/claude-mods/clean-view --output-format json
  ```

  The result has `"is_error":false`, `"num_turns":0` and
  `"result":"clean-view: Clean view off: tool calls are shown."`.
- `CLEAN_VIEW_CLAUDE_CHECK=1 node --test tests/clean-view-mod.test.js` runs the
  validate command and then the mod under the engine's own test kit
  (`claude plugin test`): each of the four row kinds is drawn as an empty box
  while clean view is on, `/clean-view` brings the row back, and a second
  `/clean-view` hides it again.

### Check it after each Claude Code upgrade

The mod API is early access and may change between Claude Code releases, and
Claude Code updates itself. So the two tests that need the installed Claude
Code are opt-in. The default unit run (`npm test`) skips both, with a reason
that names `CLEAN_VIEW_CLAUDE_CHECK`, and starts no `claude` process. Its result
does not depend on the installed version. The manifest and "nothing loads it"
tests always run.

Run the opted-in check on purpose after each Claude Code upgrade, before relying
on the mod. From the repository root:

```
CLEAN_VIEW_CLAUDE_CHECK=1 node --test tests/clean-view-mod.test.js
```

Expect 4 pass and 0 skipped. With the variable set, a `claude` binary that is
missing or has no `plugin test` fails the two tests; it does not skip them.

### Not yet checked: how it looks

The checks above show what the mod returns to the engine, not what the terminal
paints. Whether the rows vanish cleanly, with no blank lines left behind, and
whether the toggle redraws rows already on screen, needs an interactive session.

The owner helper tried it on 10-10, on Claude Code 2.1.296, in two throwaway
interactive sessions. With the mod, the folded `Ran 1 shell command` row is
hidden and the assistant text stays. `/clean-view` shows in the command list and
brings the row back. Blank lines and the redraw of rows already on screen were
not part of that try and are still unchecked.

### Known side effect

An interactive `--plugin-dir` session writes editor files into the mod folder.
The non-interactive checks do not. An interactive session on 10-10 left two
things in `tools/claude-mods/clean-view/`:

- `tsconfig.json` at the mod root. It is one line that extends
  `./.claude-plugin/types/tsconfig.json`.
- `.claude-plugin/types/`, the editor types. The folder carries its own
  `.gitignore`.

Git ignores both, so `git status` stays clean afterwards. The root `.gitignore`
covers `tools/claude-mods/*/tsconfig.json`. The files are safe to delete.
