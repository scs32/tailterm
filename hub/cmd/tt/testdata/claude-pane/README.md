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

## Busy status line and interrupted captures (stalled turns)

For `TestClaudeStallCounterFixtures` and the stall tests in `claude_stall_test.go` (bug `wi_03ce50892a559767` revision 10, order #27407, assignment #27506, manifest step H1).

- Claude Code 2.1.292, tmux 3.7b, macOS, captured 2026-10-07 on Stephens-Mini from the disposable session described in `../claude-transcript/README.md` (private socket `tmux -L ttfix-03ce -f /dev/null`, 100x30, clean environment, bypass mode as agents run). The conversation was benign: essays on terminal emulators and on proofs that there are infinitely many primes, `sleep 32` and `true`.
- `NAME.ansi` is `tmux capture-pane -p -e`; `NAME.cursor` is `#{cursor_x} #{cursor_y}` at capture time. There are no joined captures: the stall rule never types.
- `spinner-stalled-1` and `spinner-stalled-2` are **edited**, not captured: they are `spinner-after-tool` with the elapsed time `7s` replaced by `16m 2s` and `17m 4s`. A real stalled turn cannot be produced on demand; the 2026-10-01 one was reported as `Deliberating` with its counter frozen at `9.5k`.

| Fixture | Pane state | Counter read |
| --- | --- | --- |
| `spinner-progress-1` | Busy: `✶ Schlepping… (3s · ↓ 14 tokens · thinking with high effort)` with a `⎿ Tip:` row under it. | `14` |
| `spinner-progress-2` | The same turn two seconds later: `(5s · ↓ 339 tokens · thought for 2s)`. With the first, a growing counter. | `339` |
| `spinner-after-tool` | Busy after a tool result: `✻ Marinating… (7s · ↓ 75 tokens · thinking with high effort)`. | `75` |
| `spinner-stalled-1`, `spinner-stalled-2` | `spinner-after-tool` with a later elapsed time (edited): the same counter in two captures. | `75` |
| `spinner-no-counter` | Busy, first thinking of a session: `✻ Prestidigitating… (15s · still thinking with high effort)`. | none: no token counter |
| `streaming-no-status` | Busy, answer text streaming. Claude Code draws no status line; the footer still says `esc to interrupt`. | none: no status line |
| `interrupted` | Idle after Escape during thinking: `⎿  Interrupted · What should Claude do instead?` above an empty input box. A wake is accepted here. | none: footer does not say `esc to interrupt` |
| `interrupted-prompt-restored` | Idle after Escape before the turn's first assistant record: Claude Code put the prompt back in the input box. Cursor x=93. A wake refuses (occupied input). | none: input not empty |

