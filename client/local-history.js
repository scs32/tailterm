import { renderHistoryLinks } from "./terminal-links.js";
export function setupLocalHistory(t, { capture }) {
  let panel,
    pending = false,
    generation = 0;
  const toggle = document.createElement("button");
  toggle.className = "power-scroll-toggle";
  toggle.type = "button";
  t.el.append(toggle);
  const sync = () => {
    const on = !!panel;
    toggle.textContent = on ? "Scroll on" : "Scroll off";
    toggle.setAttribute("aria-pressed", String(on));
    toggle.setAttribute(
      "aria-label",
      on ? "Turn off power scrolling" : "Turn on power scrolling",
    );
    toggle.title = on
      ? "Power scrolling on · cached history. Click to return to live."
      : "Power scrolling off · click to scroll history locally.";
    toggle.disabled = !on && (!t.target || t.status !== "Connected");
  };
  const clear = () => {
    generation++;
    pending = false;
    panel?.remove();
    panel = null;
    sync();
  };
  const close = () => {
    clear();
    if (!t.disposed) t.term.focus();
  };
  async function open() {
    if (panel || !t.tmux || !t.target || t.disposed || t.status !== "Connected")
      return;
    const token = ++generation;
    const view = (panel = document.createElement("section"));
    view.className = "local-history history-loading";
    view.setAttribute("aria-label", "Power scrolling history");
    view.innerHTML =
      '<pre tabindex="0" aria-label="Cached tmux output"></pre><span class="history-status" role="status">Loading history...</span>';
    const output = view.querySelector("pre"),
      status = view.querySelector("span");
    output.style.fontFamily = t.term.options.fontFamily;
    output.style.fontSize = t.term.options.fontSize + "px";
    output.style.lineHeight = String(t.term.options.lineHeight);
    t.el.append(view);
    view.onkeydown = (e) => {
      if (e.key === "Escape") {
        e.preventDefault();
        e.stopPropagation();
        close();
      }
    };
    pending = true;
    sync();
    try {
      const text = await capture(t);
      if (token !== generation || t.disposed) return;
      renderHistoryLinks(output, text);
      status.textContent = `History snapshot · ${new Date().toLocaleTimeString()}`;
      view.classList.remove("history-loading");
      output.scrollTop = output.scrollHeight;
      output.focus();
    } catch (e) {
      if (token === generation) status.textContent = e.message;
    } finally {
      if (token === generation) {
        pending = false;
        sync();
      }
    }
  }
  toggle.onclick = () => (panel ? close() : void open());
  sync();
  return {
    open,
    clear,
    close,
    sync,
    isOpen: () => !!panel,
    focus: () =>
      pending ? toggle.focus() : panel?.querySelector("pre").focus(),
  };
}
