# Runtime prompt pane captures

Fixtures for `TestRuntimePromptClassify`, `TestRuntimePromptAnswer` and
`TestRuntimePromptTmuxReplay` (bug `wi_7d5050c57193db78` revision 1, order
#13871, plan a1, a5, a6 and a11). The Claude dialogs are reused by path from
`../claude-pane/` (see its README for their provenance).

## Method

- Codex CLI 0.156.1 (`codex --version`), tmux 3.7b, macOS, captured 2026-09-28
  on Stephens-Mini.
- A disposable session on a private tmux socket (`tmux -L ttfix-7d50 -f
  /dev/null`, 100x30), started with `env -i` holding only HOME, PATH, USER,
  LANG and TERM=xterm-256color, plus `CODEX_HOME` set to a new empty folder in
  the session scratchpad. That folder held only an `auth.json` with a fake API
  key (`sk-fake-fixture-not-a-key`), so Codex skipped its sign-in screen and
  no request reached a model. Choices were made only in that disposable home.
  No Codex process ran with the owner's `~/.codex`; it was only read (the
  config hash below and `models_cache.json`, to find a model with an upgrade).
- The working folder was a new empty folder in the session scratchpad.
- `NAME.ansi` is `tmux capture-pane -p -e`; `NAME.cursor` is `#{cursor_x}
  #{cursor_y}` at capture time.
- Owner configuration before and after, unchanged (sha256, mtime, size):
  - `~/.codex/config.toml` 003209d7f82b44a427cd11f0a1ba84e27973cda0412fed03a9dbe2a0fe4a960e, 1790616371, 3934 bytes (before and after)
  - `~/.claude/settings.json` ac8f3d16292b40647bdcace1cc7e745b700f086b944480bb4adcd06531f7335c, 1790638091, 4128 bytes (before and after)

## Fixtures

| Fixture | How it was produced | Classifier result |
| --- | --- | --- |
| `codex-trust` | Live. First start in the new folder: `Folder access`, `Trust this folder?`, `› 1. Trust and continue`, `2. Quit`, `enter continue · esc quit`. This layout no longer matches `startup.go` `codexTrustPrompt`, which looks for the older `Do you trust the contents of this directory?` text. | `codex_trust` |
| `codex-idle-composer` | Live. After choosing Trust and continue in the disposable home: the idle composer `› Ask Codex to do anything` and its status line. | no prompt |
| `codex-update` | Live. Next start: `Update available · 0.156.1 → 0.158.0`, `› 1. Update now …`, `2. Skip`, `3. Skip until next version`, `enter continue · esc skip`. Dismissed with Esc. | `unknown` |
| `codex-model-migration` | Live. With `model = "gpt-5.5"` in the disposable `config.toml`: `Meet GPT-6 Sol`, `› 1. Try new model`, `2. Use existing model`, `enter/esc confirm · ctrl+c quit`. | `codex_model_migration` (selected option 0) |
| `codex-model-migration-down` | Live. The same prompt after one `Down`: the marker is on `2. Use existing model`. The next `Enter` kept GPT-5.5 and wrote `[notice.model_migrations] "gpt-5.5" = "gpt-6-sol"` to the disposable config only. | `codex_model_migration` (selected option 1, same fingerprint) |
| `codex-scrollback-quote` | Live. A local shell command (`!printf …`, no model call) printed `Approaching rate limits`, `› 3. Keep current model (never show again)`, `Keep current model` and `Press enter to confirm or esc to go back` into the transcript above the idle composer. | no prompt |
| `codex-model-picker` | Live. `/model` opened Codex's bottom-pane selection list under that transcript: `Select Model and Effort`, seven numbered models with `› 7. GPT-5.5 (current)` selected, `enter select · esc back`. | `unknown` |
| `codex-rate-limit.reconstructed` | **Reconstructed, not captured.** Codex shows this menu only at 90% or more of a rate limit, and a fake key has none, so it could not be triggered on demand. Rows 1-18 are the real transcript rows of `codex-model-picker`. The menu below them uses that capture's bottom-pane list layout and attributes, with the title, subtitle, option labels and descriptions from the Codex 0.156.1 binary strings (`rate-limit-switch-prompt`, `Approaching rate limits`, `Switch to <model> for lower credit usage?`, `Uses fewer credits for upcoming turns.`, `Keep current model`, `Keep current model (never show again)`, `Hide future rate limit reminders about switching models.`). The option order follows those strings, the first option selected. The footer is a guess copied from the `/model` list. Replace this file when a live occurrence is captured. | `codex_rate_limit_switch` (selected option 0; target option 2) |

## Derived Claude fixtures (review #14417 b1)

Each is a byte-for-byte copy of a live `../claude-pane/` capture with only the
input text replaced, to show that an idle input box is never read as a dialog.

| Fixture | Source and change | Classifier result |
| --- | --- | --- |
| `claude-typed-numbered.derived` | `typed.ansi`, typed `hello there` → `1. rename the helper` | no prompt |
| `claude-typed-allow.derived` | `typed.ansi`, typed `hello there` → `please allow this change` (holds the dialog phrase `allow this`) | no prompt |
| `claude-suggestion-numbered.derived` | `suggestion.ansi`, faint suggestion `only test files` → `1. rename the helper` | no prompt |

A Codex prompt is recognised only when the last non-blank row is a key hint
matching `codexPromptFooter` (it starts with `press enter`, `enter WORD`,
`enter/esc WORD` or `esc WORD`, in any letter case). A footer that does not
match reads as no prompt, so the guessed footer of the reconstructed
rate-limit menu decides whether that fixture is seen at all. A footer that
differs but still matches leaves the kind unchanged and changes only the
fingerprint, which hashes the footer row with the rest of the menu.

A known kind also needs its exact title text, option labels or both. A label
ends at the first run of two or more spaces, so column spacing does not change
the result while that gap stays at least two spaces wide; a single space joins
the description to the label, which then no longer matches. A menu with a
matching footer whose title or labels do not match, as after a label change in
a future Codex release, is `unknown`, which escalates.
