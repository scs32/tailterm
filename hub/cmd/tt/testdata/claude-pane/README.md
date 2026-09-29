# Claude Code pane captures

Live captures for `TestClaudeInputPromptSuggestionFixtures` (bug `wi_9201e1f3901d6fdc` revision 2, order #14020, k5-k6) and, in the second table, for the prompt-area tests (bug `wi_7757e967b69a5ba6` revision 1, order #14126, k7-k8).

- Claude Code 2.1.284 (`claude --version`), tmux 3.7b, macOS, captured 2026-09-28 on Stephens-Mini.
- A disposable session on a private tmux socket (`tmux -L ttfix-9201`, 100x30) in a clean environment (`env -i` with only HOME, PATH, USER, LANG and TERM=xterm-256color), with prompt suggestions enabled only for that process: `CLAUDE_CODE_ENABLE_PROMPT_SUGGESTION=1 claude --settings '{"promptSuggestionEnabled":true}'`. User settings were not changed. The conversation was benign: two short tool-free answers and one `ls hub/cmd/tt | head -3`.
- `NAME.ansi` is `tmux capture-pane -p -e`; `NAME.joined.ansi` is `capture-pane -p -e -J`, which the relay uses before Enter. `NAME.cursor` is `#{cursor_x} #{cursor_y}` at capture time.

| Fixture | Pane state | Relay result |
| --- | --- | --- |
| `suggestion` | Idle after a turn. Claude Code shows the suggestion `only test files` in faint (SGR 2) text after `❯` and a no-break space (U+00A0, as on every input line). Cursor x=2. | wakes |
| `try-placeholder` | Fresh session, faint `Try "fix lint errors"` placeholder. Cursor x=2. | wakes |
| `typed-prefix` | `only` typed. The suggestion disappears as soon as a key is typed. Cursor x=6. | refuses |
| `typed` | `hello there` typed. Cursor x=13. | refuses |
| `typed-full-suggestion` | The suggestion's exact words typed. The plain text matches `suggestion`; only the attribute and cursor x=17 differ. | refuses |
| `wake-prompt-typed` | A wake prompt typed. The empty check refuses it; the joined capture matches it exactly before Enter. | refuses when empty is required; matches before Enter |

In these captures, typing replaced the suggestion immediately. The owner observed on 2026-09-28 that Enter alone does not submit a suggestion; these captures did not test that.

## Prompt-area captures (k7-k8)

- Claude Code 2.1.284, tmux 3.7b, macOS, captured 2026-09-28 on Stephens-Mini with the same method: private socket `tmux -L ttfix-7757 -f /dev/null`, 100x30, `env -i` with only HOME, PATH, USER, LANG and TERM=xterm-256color, `CLAUDE_CODE_ENABLE_PROMPT_SUGGESTION=0`, a disposable folder under the session scratchpad. User settings were not changed (`~/.claude/settings.json` sha256 and mtime identical before and after).
- Two sessions. `claude --permission-mode default` (per process) for the dialogs: the folder trust prompt at start, then a Bash permission prompt (`touch <folder>/x`, dismissed with Esc, not run) and an AskUserQuestion prompt (dismissed with Esc). Plain `claude` (the user default, bypass mode, as agents run) for the scrollback captures, because `⏸ manual mode on` is not on the idle footer allowlist. Neither dialog left the idle `──`/`❯`/`──` box visible.
- The scrollback prompt was: `Do not use any tools. Reply with exactly these six lines, then stop: 1. The permission dialog appeared. 2. Its footer said Esc to cancel. 3. It asked Allow this tool? 4. Then: Do you want to proceed? 5. A menu said Select an option. 6. The shell asked (y/n).`

| Fixture | Pane state | Relay result |
| --- | --- | --- |
| `scrollback-dialog-words` | Idle, empty input box under the answer quoting all six phrases. Cursor x=2. | wakes |
| `scrollback-dialog-words-typed` | The same screen with `Tailterm messages #14142. Run tt inbox --unread --mark-read.` typed, not submitted. Cursor x=62. | refuses when empty is required; the joined capture matches it before Enter |
| `permission-dialog` | Live Bash permission prompt: a rule, `Bash command`, `Do you want to proceed?`, `❯ 1. Yes` … `4. No`, `Esc to cancel · Tab to amend`. No input box. Cursor 1,23. | refuses (whole capture, "esc to cancel") |
| `selection-dialog` | Live AskUserQuestion prompt: `☐ Choice`, `❯ 1. A` … `4. Chat about this`, `Enter to select · ↑/↓ to navigate · Esc to cancel`. Cursor 0,21. | refuses |
| `trust-dialog` | Folder trust prompt at start: `❯ No, exit`, `Yes, I trust this folder`, `Enter to confirm · Esc to cancel`. Cursor 1,15. | refuses |
