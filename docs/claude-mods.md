# Claude Code mods

Mods kept in this repository live under `tools/claude-mods/`. Each is a plain
plugin folder. Nothing installs them and nothing loads them: a mod runs only in
a Claude Code session that someone starts with `--plugin-dir`.

## Clean view

`tools/claude-mods/clean-view` is a viewing aid for the owner's own session. It
hides the tool call, tool result, tool group and tool progress rows in the
transcript, so the session shows the conversation without the tool traffic.

It changes only what is drawn. The model reads and does exactly what it would
without the mod, and the tool-call ledger still records every call. Approval
dialogs are a different component and still show.

### Switch it on

From the repository root:

```
claude --plugin-dir tools/claude-mods/clean-view
```

From anywhere else, give the absolute path of the folder. The session starts
with clean view **on**.

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

Do not put the folder in `CLAUDE_CODE_PLUGIN_DIRS` in `~/.claude/settings.json`.
Every Claude Code session on the host, agent sessions included, would then load
it.

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
That check is pending the owner's first interactive try.

### Known side effect

By the engine's documentation, an interactive `--plugin-dir` session writes
editor types into `tools/claude-mods/clean-view/.claude-plugin/types/`. The team
has not observed this, because the non-interactive checks do not do it. The
files are untracked and safe to delete.
