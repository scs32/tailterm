import { fonts } from "./appearance.js";
export const tabColors = {
  default: "Default",
  mint: "Mint",
  blue: "Blue",
  violet: "Violet",
  amber: "Amber",
  rose: "Rose",
};
export function normalizeTabDecoration(value = {}) {
  const clean = (text, length) =>
    typeof text === "string"
      ? [
          ...text
            .replace(/[\p{Cc}\p{Cf}]/gu, (c) => (c === "\u200d" ? c : ""))
            .trim(),
        ]
          .slice(0, length)
          .join("")
      : "";
  return {
    label: clean(value?.label, 40),
    emoji: clean(value?.emoji, 12),
    color: Object.hasOwn(tabColors, value?.color) ? value.color : "default",
    font: Object.hasOwn(fonts, value?.font) ? value.font : "",
  };
}
export function showTabDecoration({ tab, name, dialog, close, save }) {
  const value = normalizeTabDecoration(tab.decoration);
  dialog(
    "Tab appearance",
    `<form id="tab-decoration-form">
    <p class="fine">Personal labels stay in this browser’s encrypted workspace. The remote tmux session name stays the same.</p>
    <label>Tab label<input name="label" maxlength="40" placeholder="Use server name"></label>
    <label>Emoji or symbol<input name="emoji" maxlength="24" placeholder="e.g. 🔧"></label>
    <label>Color marker<select name="color"></select></label>
    <label>Tab typeface<select name="font"></select></label>
    <div class="tab-decoration-preview"><span id="tab-decoration-sample"></span></div>
    <div class="dialog-actions"><button type="button" id="reset-tab-decoration">Reset</button><button class="primary">Save</button></div>
  </form>`,
  );
  const form = document.querySelector("#tab-decoration-form");
  for (const [id, label] of Object.entries(tabColors))
    form.elements.color.add(new Option(label, id));
  form.elements.font.add(new Option("Follow terminal font", ""));
  for (const [id, font] of Object.entries(fonts))
    form.elements.font.add(new Option(font.name, id));
  const read = () =>
    normalizeTabDecoration(Object.fromEntries(new FormData(form)));
  const preview = () => {
    const next = read(),
      el = form.querySelector("#tab-decoration-sample");
    el.textContent = [next.emoji, next.label || name].filter(Boolean).join(" ");
    el.dataset.tabColor = next.color;
    el.style.fontFamily = fonts[next.font]?.family || "var(--terminal-font)";
  };
  const fill = (next) => {
    for (const key of Object.keys(next)) form.elements[key].value = next[key];
    preview();
  };
  form.oninput = preview;
  form.onchange = preview;
  form.querySelector("#reset-tab-decoration").onclick = () =>
    fill(normalizeTabDecoration());
  form.onsubmit = (e) => {
    e.preventDefault();
    if (!tab.disposed) save(read());
    close();
  };
  fill(value);
}
