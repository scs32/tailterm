export const REASONING_INHERIT = "";

// This is deliberately an allowlist, not a model-name heuristic. Each entry is
// backed by the current OpenAI model page and the Codex config reference. Codex
// currently accepts minimal..xhigh as config values; values documented only by
// an API model page (for example `none` or `max`) are not advertised here.
export const CODEX_REASONING_MODELS = Object.freeze({
  "gpt-5.3-codex": ["low", "medium", "high", "xhigh"],
  "gpt-6-astra": ["low", "medium", "high", "xhigh"],
  "gpt-6-sol": ["low", "medium", "high", "xhigh"],
  "gpt-6-luna": ["low", "medium", "high", "xhigh"],
  "gpt-5.6-sol": ["low", "medium", "high", "xhigh"],
  "gpt-5.6-terra": ["low", "medium", "high", "xhigh"],
  "gpt-5.6-luna": ["low", "medium", "high", "xhigh"],
});

// Claude Code `--effort` levels per exact model ID (Anthropic models reference).
// Aliases such as `opus` follow the server's configuration, so they stay
// inherit-only; Haiku 4.5 has no effort control.
export const CLAUDE_EFFORT_MODELS = Object.freeze({
  "claude-opus-5-5": ["low", "medium", "high", "xhigh", "max"],
  "claude-fable-5-1": ["low", "medium", "high", "xhigh", "max"],
  "claude-opus-5": ["low", "medium", "high", "xhigh", "max"],
  "claude-sonnet-5": ["low", "medium", "high", "xhigh", "max"],
});

const REASONING_MODELS = { codex: CODEX_REASONING_MODELS, claude: CLAUDE_EFFORT_MODELS };

export function reasoningOptions(runtime, model) {
  return REASONING_MODELS[runtime]?.[model] || [];
}

export function validateReasoning(runtime, model, value = "", run = "") {
  if (typeof value !== "string") throw new Error("Invalid reasoning level.");
  if (!value) return "";
  const supported = reasoningOptions(runtime, model);
  if (!supported.includes(value))
    throw new Error(
      !REASONING_MODELS[runtime]
        ? "Explicit reasoning is not supported for this agent app. Choose Inherit."
        : !model
          ? "Choose a documented model before setting reasoning, or use Inherit."
          : `This model and reasoning level are not in Tailterm’s verified ${runtime === "codex" ? "Codex" : "Claude"} support matrix. Choose Inherit.`,
    );
  if (run.trim() !== runtime)
    throw new Error(
      `Explicit reasoning requires Tailterm’s verified native ${runtime} command. Custom command overrides must use Inherit.`,
    );
  return value;
}
