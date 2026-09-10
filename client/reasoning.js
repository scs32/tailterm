export const REASONING_INHERIT = "";

// This is deliberately an allowlist, not a model-name heuristic. Each entry is
// backed by the current OpenAI model page and the Codex config reference. Codex
// currently accepts minimal..xhigh as config values; values documented only by
// an API model page (for example `none` or `max`) are not advertised here.
export const CODEX_REASONING_MODELS = Object.freeze({
  "gpt-5.3-codex": ["low", "medium", "high", "xhigh"],
  "gpt-6-astra": ["low", "medium", "high", "xhigh"],
  "gpt-5.6-sol": ["low", "medium", "high", "xhigh"],
  "gpt-5.6-terra": ["low", "medium", "high", "xhigh"],
  "gpt-5.6-luna": ["low", "medium", "high", "xhigh"],
});

export function reasoningOptions(runtime, model) {
  return runtime === "codex" ? CODEX_REASONING_MODELS[model] || [] : [];
}

export function validateReasoning(runtime, model, value = "") {
  if (typeof value !== "string") throw new Error("Invalid reasoning level.");
  if (!value) return "";
  const supported = reasoningOptions(runtime, model);
  if (!supported.includes(value))
    throw new Error(
      runtime !== "codex"
        ? "Explicit reasoning is not supported for this agent app. Choose Inherit."
        : !model
          ? "Choose a documented Codex model before setting reasoning, or use Inherit."
          : "This model and reasoning level are not in Tailterm’s verified Codex support matrix. Choose Inherit.",
    );
  return value;
}
