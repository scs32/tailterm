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
    fill: Object.hasOwn(tabColors, value?.fill) ? value.fill : "default",
    font: Object.hasOwn(fonts, value?.font) ? value.font : "",
  };
}
export function showTabDecoration({
  tab,
  name,
  dialog,
  close,
  save,
  kind = "tab",
}) {
  const value = normalizeTabDecoration(tab.decoration);
  dialog(
    kind === "group" ? "Group appearance" : "Tab appearance",
    `<form id="tab-decoration-form">
    <p class="fine">${kind === "group" ? "This appearance belongs to the group. Each session keeps its own label and colors." : "Personal labels stay in this browser’s encrypted workspace. The remote tmux session name stays the same."}</p>
    <label>${kind === "group" ? "Group label" : "Tab label"}<input name="label" maxlength="40" placeholder="${kind === "group" ? "Use session labels" : "Use server name"}"></label>
    <label>Emoji or symbol<input name="emoji" maxlength="24" placeholder="e.g. 🔧"></label>
    <details class="dialog-details emoji-picker"><summary>Choose an emoji</summary><label>Find an emoji<input id="emoji-search" type="search" placeholder="Search: rocket, tools, home…" autocomplete="off"></label><div id="emoji-options" aria-label="Emoji choices"></div><p id="emoji-empty" class="fine" hidden>No matching emoji. You can also type or paste one above.</p><button type="button" id="clear-tab-emoji">No emoji</button></details>
    <div class="form-grid"><label>Outline color<select name="color"></select></label><label>Background fill<select name="fill"></select></label></div>
    <label>Tab typeface<select name="font"></select></label>
    <div class="tab-decoration-preview"><span id="tab-decoration-sample"></span></div>
    <div class="dialog-actions"><button type="button" id="reset-tab-decoration">Reset</button><button class="primary">Save</button></div>
  </form>`,
  );
  const form = document.querySelector("#tab-decoration-form");
  for (const [id, label] of Object.entries(tabColors))
    for (const field of ["color", "fill"])
      form.elements[field].add(new Option(label, id));
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
    el.dataset.tabFill = next.fill;
    for (const button of form.querySelectorAll("[data-emoji]"))
      button.setAttribute(
        "aria-pressed",
        String(button.dataset.emoji === next.emoji),
      );
    el.style.fontFamily = fonts[next.font]?.family || "var(--terminal-font)";
  };
  const fill = (next) => {
    for (const key of Object.keys(next)) form.elements[key].value = next[key];
    preview();
  };
  const picker = form.querySelector("#emoji-options");
  for (const [emoji, label] of tabEmojis) {
    const button = document.createElement("button");
    button.type = "button";
    button.dataset.emoji = emoji;
    button.dataset.search = label;
    button.textContent = emoji;
    button.setAttribute("aria-label", label);
    button.title = label;
    button.onclick = () => {
      form.elements.emoji.value = emoji;
      preview();
    };
    picker.append(button);
  }
  form.querySelector("#emoji-search").oninput = (e) => {
    const query = e.target.value.trim().toLowerCase();
    for (const button of picker.children)
      button.hidden =
        !button.dataset.search.toLowerCase().includes(query) &&
        !button.dataset.emoji.includes(query);
    form.querySelector("#emoji-empty").hidden = [...picker.children].some(
      (button) => !button.hidden,
    );
  };
  form.querySelector("#clear-tab-emoji").onclick = () => {
    form.elements.emoji.value = "";
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

// Local choices keep the picker available offline, with no third-party scripts.
const tabEmojis = [
  ["🔧", "Wrench tools"],
  ["🛠️", "Hammer and wrench build"],
  ["⚙️", "Gear settings"],
  ["🔨", "Hammer"],
  ["💻", "Laptop computer"],
  ["🖥️", "Desktop computer server"],
  ["⌨️", "Keyboard"],
  ["🖱️", "Mouse"],
  ["🍎", "Apple Mac"],
  ["🐧", "Penguin Linux"],
  ["🪟", "Window Windows"],
  ["📱", "Phone mobile"],
  ["🚀", "Rocket launch deploy"],
  ["🛰️", "Satellite network"],
  ["🌐", "Globe web network"],
  ["☁️", "Cloud"],
  ["🏠", "Home house"],
  ["🏢", "Office building work"],
  ["🗄️", "File cabinet storage NAS"],
  ["💾", "Floppy disk save backup"],
  ["📦", "Package container"],
  ["🐳", "Whale Docker"],
  ["🧪", "Test tube testing"],
  ["🔬", "Microscope research"],
  ["🔒", "Lock secure"],
  ["🔑", "Key SSH"],
  ["🛡️", "Shield security"],
  ["🔗", "Link connection"],
  ["⚡", "Lightning power fast"],
  ["🔥", "Fire hot"],
  ["❄️", "Snowflake cold"],
  ["🌙", "Moon night"],
  ["☀️", "Sun day"],
  ["⭐", "Star favorite"],
  ["✨", "Sparkles"],
  ["🌈", "Rainbow colors"],
  ["✅", "Check done success"],
  ["⚠️", "Warning caution"],
  ["🚨", "Siren alert"],
  ["🚧", "Construction work in progress"],
  ["🎯", "Target goal"],
  ["📌", "Pin"],
  ["📝", "Memo notes edit"],
  ["📚", "Books documentation"],
  ["🎵", "Music audio"],
  ["🎬", "Movie video transcode"],
  ["🎮", "Game controller"],
  ["📷", "Camera images photos"],
  ["🤖", "Robot automation AI"],
  ["🧠", "Brain intelligence"],
  ["👾", "Alien pixel"],
  ["👻", "Ghost"],
  ["🦊", "Fox"],
  ["🐈", "Cat"],
  ["🐕", "Dog"],
  ["🐝", "Bee"],
  ["🌲", "Tree forest"],
  ["🌱", "Seedling grow"],
  ["🌊", "Wave water"],
  ["🏔️", "Mountain"],
  ["☕", "Coffee"],
  ["🍕", "Pizza"],
  ["🎉", "Party celebration"],
  ["❤️", "Heart love"],
];
