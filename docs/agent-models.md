# Agent model dropdowns

Team members and individual task agents share a model selector. App default
omits the model flag. Custom model accepts an exact server/provider model ID.
The dropdown is a set of suggestions, not a check of an account's access.
Aider supports different providers, so it retains App default and Custom model.
Changing agent apps clears the previous model selection. Existing custom values
remain editable and survive member switching, saving, and encrypted profile sync.

Sources checked September 7, 2026:

- [Codex models](https://learn.chatgpt.com/docs/models), also checked against the
  installed CLI's visible model catalog. Friendly names submit exact CLI IDs,
  such as Astra → `gpt-6-astra`.
- [Claude Code model configuration](https://code.claude.com/docs/en/model-config):
  `opus`, `sonnet`, `haiku`, and `opusplan` aliases follow the server's configuration.
  Exact IDs `claude-opus-5-5`, `claude-fable-5-1` and `claude-sonnet-5` were added
  September 23, 2026 so explicit effort can be set (see [agents library](agents-library.md)).
- GPT-6 Sol (`gpt-6-sol`) and Luna (`gpt-6-luna`) were added September 23, 2026 from the
  [Codex models page](https://learn.chatgpt.com/docs/models); served to the owner's
  ChatGPT-account Codex from `codex-cli` 0.156.1 (0.154.0 rejected it).
- [Gemini CLI model selection](https://geminicli.com/docs/cli/model/): explicit
  Gemini 3 preview and Gemini 2.5 Pro/Flash IDs.

`node tests/teams-vault-browser.mjs` verifies eight-member team saves against the
real encrypted vault, persistence after reload, preset/custom models, and visible
validation for an invalid member that is not currently selected.
