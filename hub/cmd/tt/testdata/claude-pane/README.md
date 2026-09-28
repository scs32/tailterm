# Claude Code pane captures

Live captures for `TestClaudeInputPromptSuggestionFixtures` (bug `wi_9201e1f3901d6fdc` revision 2, order #14020, k5-k6).

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
