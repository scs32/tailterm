// Header mode selector: Terminals (the existing workspace), Board, Files, and
// Tasks. Non-terminal modes render into #mode-view while the terminal shell
// stays mounted but hidden, so connections and scrollback survive switching.
export const MODES = [
  ["terminals", "Terminals", "1"],
  ["board", "Board", "2"],
  ["files", "Files", "3"],
  ["tasks", "Tasks", "4"],
];
export function setupModes({ header, main, onChange, available = () => true }) {
  let current = "terminals";
  const control = document.createElement("div");
  control.className = "segmented mode-switch";
  control.setAttribute("role", "tablist");
  control.setAttribute("aria-label", "Workspace mode");
  control.innerHTML = MODES.map(
    ([id, label, key]) =>
      `<button type="button" role="tab" data-mode="${id}" aria-pressed="false" aria-selected="false" title="${label}\nCtrl/Cmd + ${key}" data-shortcut="⌘${key}">${label}</button>`,
  ).join("");
  header.prepend(control);
  const view = document.createElement("section");
  view.id = "mode-view";
  view.hidden = true;
  main.append(view);
  const shell = () => main.querySelector(".terminal-shell");
  function set(mode, { silent = false } = {}) {
    if (!MODES.some(([id]) => id === mode) || !available(mode)) return current;
    const changed = mode !== current;
    current = mode;
    for (const b of control.querySelectorAll("[data-mode]")) {
      const on = b.dataset.mode === mode;
      b.setAttribute("aria-pressed", on);
      b.setAttribute("aria-selected", on);
    }
    const terminals = mode === "terminals";
    if (shell()) shell().hidden = !terminals;
    view.hidden = terminals;
    document.body.dataset.mode = mode;
    if (changed && !silent) onChange?.(mode, view);
    return current;
  }
  control.addEventListener("click", (e) => {
    const b = e.target.closest("[data-mode]");
    if (b) set(b.dataset.mode);
  });
  window.addEventListener("keydown", (e) => {
    if (!(e.metaKey || e.ctrlKey) || e.altKey || e.shiftKey) return;
    if (document.querySelector("dialog[open]")) return;
    const hit = MODES.find(([, , key]) => e.key === key);
    if (!hit) return;
    e.preventDefault();
    set(hit[0]);
  });
  set("terminals", { silent: true });
  return {
    set,
    get: () => current,
    view,
    refresh: () => {
      for (const b of control.querySelectorAll("[data-mode]"))
        b.disabled = !available(b.dataset.mode);
    },
  };
}
