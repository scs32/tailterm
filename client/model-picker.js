// Suggestions, not an account entitlement list. Keep custom model IDs available.
// Sources and review date: docs/agent-models.md.
export const MODEL_OPTIONS = {
  codex: [
    ["gpt-5.3-codex", "GPT-5.3 Codex"],
    ["gpt-6-astra", "Astra (GPT-6)"],
    ["gpt-5.6-sol", "Sol (GPT-5.6)"],
    ["gpt-5.6-terra", "Terra (GPT-5.6)"],
    ["gpt-5.6-luna", "Luna (GPT-5.6)"],
    ["gpt-5.5", "GPT-5.5"],
    ["gpt-5.4-mini", "GPT-5.4 Mini"],
    ["gpt-5.3-codex-spark", "Codex Spark"],
  ],
  claude: [
    ["opus", "Opus"],
    ["sonnet", "Sonnet"],
    ["haiku", "Haiku"],
    ["opusplan", "Opus plan / Sonnet execution"],
  ],
  gemini: [
    ["gemini-3-pro-preview", "Gemini 3 Pro Preview"],
    ["gemini-3-flash-preview", "Gemini 3 Flash Preview"],
    ["gemini-2.5-pro", "Gemini 2.5 Pro"],
    ["gemini-2.5-flash", "Gemini 2.5 Flash"],
  ],
  aider: [],
};
const esc = (s) =>
  String(s ?? "").replace(
    /[&<>"']/g,
    (c) =>
      ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[
        c
      ],
  );
export function modelPickerHTML(id, runtime, value = "") {
  const options = MODEL_OPTIONS[runtime] || [],
    supported = Object.hasOwn(MODEL_OPTIONS, runtime);
  const selected = value
    ? options.some(([v]) => v === value)
      ? value
      : "__custom"
    : "";
  return `<div class="model-picker" data-model-picker="${esc(id)}"><label>Model<select id="${esc(id)}-choice" ${supported ? "" : "disabled"} aria-label="Model"><option value="">App default</option>${options.map(([v, label]) => `<option value="${esc(v)}" ${v === selected ? "selected" : ""}>${esc(label)}</option>`).join("")}<option value="__custom" ${selected === "__custom" ? "selected" : ""}>Custom model…</option></select></label><label data-custom-model ${selected === "__custom" ? "" : "hidden"}>Custom model<input id="${esc(id)}" data-field="model" value="${esc(value)}" maxlength="200" placeholder="Exact model ID" autocomplete="off" spellcheck="false" ${supported ? "" : "disabled"}></label></div>`;
}
export function wireModelPicker(root) {
  const choice = root.querySelector("select"),
    field = root.querySelector("input"),
    custom = root.querySelector("[data-custom-model]");
  choice.onchange = () => {
    const isCustom = choice.value === "__custom";
    custom.hidden = !isCustom;
    field.value = isCustom ? "" : choice.value;
    if (isCustom) field.focus();
    field.dispatchEvent(new Event("input", { bubbles: true }));
  };
}
