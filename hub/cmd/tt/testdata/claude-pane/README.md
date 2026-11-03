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


## Claude Code 2.1.316 captures

For `TestClaudeInputV2316Fixtures` and `TestClaudeWakeSkipSavesInputSnapshot` (bug `wi_8ebd8c723dec5777` revision 2, order #56805, assignment #57072, f1/f2).

- Claude Code 2.1.316 (`claude --version`), tmux 3.7b, macOS, captured 2026-11-02 on Stephens-Mini, the same version the owner helper ran that day.
- A disposable session on a private tmux socket (`tmux -L ttfix-8ebd -f /dev/null`), `env -i` with only HOME, PATH, USER, LANG and TERM=xterm-256color, in a disposable folder under the session scratchpad (the banner's path), plain `claude` (bypass mode, as agents run). The live helper pane was never read, typed into or restarted. User settings were not changed: `~/.claude/settings.json` sha256 `a80cb4e6…6830a0` and mtime Oct 7 16:47:56 before and after.
- Prompt suggestions: 2.1.316 backs off from suggestions after 20 unused ones (`promptSuggestionUnusedStreak` in `~/.claude.json`; the owner's holds 21) unless `CLAUDE_CODE_ENABLE_PROMPT_SUGGESTION` is exactly `true`, and generates none for an unfocused terminal. The suggestion captures therefore ran `CLAUDE_CODE_ENABLE_PROMPT_SUGGESTION=true claude --settings '{"promptSuggestionEnabled":true}'` with `focus-events on` and a focus-in sequence (`ESC [ I`) sent to the pane. Two of the sessions also wrote `--debug-file` to the scratchpad, to confirm `prompt.suggest (suggestion): … shown`; no fixture below shows the debug banner.
- The conversation was benign: three colors, three files of a small Go project with "the full list or only the test files?", one line appended to a disposable `notes.txt`, `sleep 20`, and a pasted list of twelve neutral lines.
- `NAME.ansi` is `capture-pane -p -e`; `NAME.joined.ansi` is `capture-pane -p -e -J`; `NAME.cursor` is `#{cursor_x} #{cursor_y}`. Pane 137x23 (the owner helper's size) unless stated.

| Fixture | Pane state | Relay result |
| --- | --- | --- |
| `v2316-suggestion` | Idle after a turn, faint suggestion `only the test files` after `❯` and a space. Cursor 2,19. | wakes |
| `v2316-try-placeholder` | Fresh session, faint `Try "edit <filepath> to..."` placeholder; a `● high · /effort` row above the top rule. Cursor 2,19. | wakes |
| `v2316-try-placeholder-100x30` | The same at 100x30. Cursor 2,26. | wakes |
| `v2316-partial-draft` | `only the` typed over the suggestion, which vanished at the first key. Cursor 10,19. | refuses: `Claude cursor is not at an empty input` (held `only the`); the joined capture matches `only the` before Enter |
| `v2316-cursor-off-row` | The draft above, then ` \` and Enter (a new line) and `second line`: a two-line draft with the cursor on its second row, off the `❯` row. Cursor 13,19. | refuses: `cursor is not at an empty input` (held `only the second line`) |
| `v2316-diff-panel` | After `/diff` (Toggle the diff panel), in the folder made a git repository: the panel (`2 +two`, a faint `│` border on a dark background, then normal text) is drawn beside the transcript rows only; the rules and the input row keep the full width. Cursor 2,19. | wakes |
| `v2316-suggestion-panel` | Idle with the panel open, faint suggestion `full list`. Cursor 2,19. | wakes |
| `v2316-midturn-typed` | During `sleep 20`, `next question` typed and not submitted; the footer reads `⏵⏵ bypass permissions on · 1 shell`. Cursor 15,19. | refuses: `cursor is not at an empty input` (held `next question`) |
| `v2316-pasted-draft` | A twelve-line paste shown as `[Pasted text #1 +11 lines]`, then ` please summarize` typed; the footer reads `paste again to expand`. Cursor 45,19. | refuses: `cursor is not at an empty input`; the box's text is not read, because that footer is not a known one |

`helper-1455-midturn` is an **edited excerpt**, not a reproduction: the owner helper's own capture at 14:55 PST on 2026-11-02 (`tailterm-artifacts/owner-helper/helper-pane-1455-2026-11-02.txt`, `capture-pane -p -e`, 137x23, cursor 2,19), taken during a turn and saved through `cat -v`. It was decoded back to bytes (re-encoding with `LC_ALL=C cat -v` gives the saved text byte for byte), and the six transcript rows' words were replaced with neutral text of the same width; every escape, the diff panel, the spinner, the `Update installed · /restart to apply` row, the rules, the `❯` row (U+276F, U+00A0) and the footer (`… · ← for agents · 1 feedback draft`) are as captured. It holds the last 14 of the pane's 23 rows, so its cursor is 2,10. The pane check passes it; the relay would have refused it as `turn in progress`.

Tried and not reproduced in these sessions (no fixture): faint text followed by normal text inside the input box (`Claude input mixes faint and typed text`). A space, a Right arrow (which accepts the suggestion as normal text), a bracketed paste and typing all replaced the suggestion; resizing to 60 and 30 columns and back with a suggestion showing redrew cleanly. No idle screen with the update notice, a second footer row or the suggestion back-off hint could be produced.
