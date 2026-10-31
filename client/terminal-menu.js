// The ▾ terminal menu (wi_649926f70aaa762a): an anchored menu whose rows open
// sub-menus beside them (inline under the row below 600 px). Rows are either
// sub-menu openers ({key, label, note, children: () => rows}) or check rows
// ({key, label, note, checked, select: () => Promise}). Choosing a check row
// leaves the menu open, so several terminals can be added in turn; refresh()
// redraws every open level from its row source.
//
// Keyboard: ArrowDown/ArrowUp move, Enter/Space/ArrowRight open a sub-menu,
// ArrowLeft or Escape return to its row, Escape on the first level closes the
// menu and returns focus to the button that opened it. Tab or a click
// outside closes it.
const esc = (s) =>
  String(s ?? "").replace(
    /[&<>"']/g,
    (c) =>
      ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[
        c
      ],
  );

export function createTerminalMenu({ root = () => document.body } = {}) {
  // levels[i]: {el, rows: () => rows, opener: key of the row in levels[i-1]}
  let levels = [];
  let anchor = null;
  let label = "";
  let onClose = null;

  const narrow = () => window.innerWidth < 600;
  const isOpen = () => levels.length > 0;

  function close({ focusAnchor = false } = {}) {
    if (!isOpen()) return;
    for (const level of levels) level.el.remove();
    levels = [];
    document.removeEventListener("pointerdown", outside, true);
    anchor?.setAttribute("aria-expanded", "false");
    if (focusAnchor && anchor?.isConnected) anchor.focus();
    anchor = null;
    const done = onClose;
    onClose = null;
    done?.();
  }
  function outside(event) {
    if (levels.some((level) => level.el.contains(event.target))) return;
    if (anchor?.contains(event.target)) return;
    close();
  }

  function rowHTML(row) {
    const note = row.note
      ? `<span class="terminal-menu-note">${esc(row.note)}</span>`
      : "";
    if (row.children)
      return `<button type="button" role="menuitem" aria-haspopup="menu" aria-expanded="false" data-menu-key="${esc(row.key)}"><span class="terminal-menu-label">${esc(row.label)}</span>${note}<span class="terminal-menu-more" aria-hidden="true">›</span></button>`;
    return `<button type="button" role="menuitemcheckbox" aria-checked="${!!row.checked}" data-menu-key="${esc(row.key)}"><span class="terminal-menu-check" aria-hidden="true">${row.checked ? "✓" : ""}</span><span class="terminal-menu-label">${esc(row.label)}</span>${note}</button>`;
  }
  function drawLevel(index) {
    const level = levels[index];
    const rows = level.rows();
    level.current = rows;
    const html = rows.length
      ? rows.map(rowHTML).join("")
      : `<p class="terminal-menu-empty">${esc(level.empty || "No other terminals")}</p>`;
    // Unchanged rows keep their elements, so a redraw of the tab strip never
    // replaces a row under the pointer or the keyboard.
    if (html === level.html) return;
    level.html = html;
    level.el.innerHTML = html;
    for (const button of level.el.querySelectorAll("[data-menu-key]")) {
      const key = button.dataset.menuKey;
      button.onclick = () =>
        choose(
          index,
          levels[index]?.current?.find((r) => r.key === key),
          button,
          false,
        );
      if (levels[index + 1]?.opener === key)
        button.setAttribute("aria-expanded", "true");
    }
  }
  function place(index) {
    const level = levels[index];
    const el = level.el;
    let x, y;
    if (index === 0) {
      const box = anchor.getBoundingClientRect();
      // An anchor on a tab that is no longer focused may be undisplayed:
      // the menu then keeps its place.
      if (!box.width && !box.height && level.at) [x, y] = level.at;
      else {
        x = box.left;
        y = box.bottom;
      }
      level.at = [x, y];
    } else {
      const parent = levels[index - 1];
      const row = parent.el.querySelector(
        `[data-menu-key="${CSS.escape(level.opener)}"]`,
      );
      const box = (row || parent.el).getBoundingClientRect();
      const parentBox = parent.el.getBoundingClientRect();
      if (narrow()) {
        x = parentBox.left;
        y = box.bottom;
      } else {
        x = parentBox.right - 1;
        y = box.top;
      }
    }
    el.style.left = "0px";
    el.style.top = "0px";
    const own = el.getBoundingClientRect();
    const maxX = Math.max(0, window.innerWidth - own.width - 4);
    const maxY = Math.max(0, window.innerHeight - own.height - 4);
    if (index > 0 && !narrow() && x > maxX) {
      // No room on the right: open on the left of the parent.
      x = levels[index - 1].el.getBoundingClientRect().left - own.width + 1;
    }
    el.style.left = `${Math.round(Math.min(Math.max(0, x), maxX))}px`;
    el.style.top = `${Math.round(Math.min(Math.max(0, y), maxY))}px`;
  }
  function addLevel(rows, opener, title, empty) {
    const el = document.createElement("div");
    el.className = "terminal-menu";
    el.setAttribute("role", "menu");
    el.setAttribute("aria-label", title);
    el.dataset.level = String(levels.length);
    el.addEventListener("keydown", keydown);
    root().append(el);
    levels.push({ el, rows, opener, empty });
    const index = levels.length - 1;
    drawLevel(index);
    place(index);
    return index;
  }
  function closeFrom(index) {
    for (const level of levels.splice(index)) level.el.remove();
    const parent = levels[index - 1];
    parent?.el
      .querySelectorAll('[aria-expanded="true"]')
      .forEach((b) => b.setAttribute("aria-expanded", "false"));
  }
  function openChild(index, row, button, focusFirst) {
    closeFrom(index + 1);
    button.setAttribute("aria-expanded", "true");
    const child = addLevel(row.children, row.key, row.label, row.empty);
    if (focusFirst) levels[child].el.querySelector("[data-menu-key]")?.focus();
  }
  async function choose(index, row, button, keyboard) {
    if (!row) return;
    if (row.children) {
      openChild(index, row, button, keyboard);
      return;
    }
    closeFrom(index + 1);
    button.disabled = true;
    const refocus = () =>
      levels[index]?.el
        .querySelector(`[data-menu-key="${CSS.escape(row.key)}"]`)
        ?.focus({ preventScroll: true });
    try {
      await row.select?.();
    } finally {
      button.disabled = false;
      if (isOpen()) {
        refresh();
        // Opening a pane focuses it; a keyboard choice keeps the menu's focus.
        if (keyboard || levels[index]?.el.contains(document.activeElement)) {
          refocus();
          requestAnimationFrame(() => requestAnimationFrame(refocus));
        }
      }
    }
  }
  function keydown(event) {
    const el = event.currentTarget;
    const index = Number(el.dataset.level);
    const buttons = [...el.querySelectorAll("[data-menu-key]")];
    const at = buttons.indexOf(document.activeElement);
    const row = (key) => levels[index]?.current?.find((r) => r.key === key);
    const step = (delta) => {
      if (!buttons.length) return;
      buttons[(at + delta + buttons.length) % buttons.length].focus();
    };
    switch (event.key) {
      case "ArrowDown":
        step(at < 0 ? 0 : 1);
        break;
      case "ArrowUp":
        step(at < 0 ? 0 : -1);
        break;
      case "Home":
        buttons[0]?.focus();
        break;
      case "End":
        buttons.at(-1)?.focus();
        break;
      case "ArrowRight":
      case "Enter":
      case " ": {
        const button = buttons[at];
        const target = button && row(button.dataset.menuKey);
        if (!target) return;
        if (event.key === "ArrowRight" && !target.children) return;
        void choose(index, target, button, true);
        break;
      }
      case "ArrowLeft":
      case "Escape": {
        if (index === 0) {
          if (event.key === "Escape") close({ focusAnchor: true });
          else return;
          break;
        }
        const opener = levels[index].opener;
        closeFrom(index);
        levels[index - 1].el
          .querySelector(`[data-menu-key="${CSS.escape(opener)}"]`)
          ?.focus();
        break;
      }
      case "Tab":
        close();
        return;
      default:
        return;
    }
    event.preventDefault();
    event.stopPropagation();
  }

  // Redraw every open level, keeping the open sub-menus and focus.
  function refresh() {
    if (!isOpen()) return;
    const focused = document.activeElement?.dataset?.menuKey;
    const focusedLevel = levels.findIndex((l) =>
      l.el.contains(document.activeElement),
    );
    for (let i = 0; i < levels.length; i++) {
      if (i > 0) {
        const parentRow = levels[i - 1].current?.find(
          (r) => r.key === levels[i].opener,
        );
        if (!parentRow?.children) {
          closeFrom(i);
          break;
        }
        levels[i].rows = parentRow.children;
      }
      drawLevel(i);
      place(i);
    }
    if (focused && focusedLevel >= 0)
      levels[Math.min(focusedLevel, levels.length - 1)]?.el
        .querySelector(`[data-menu-key="${CSS.escape(focused)}"]`)
        ?.focus({ preventScroll: true });
  }

  // Open under button. rows: () => first-level rows. keyboard focuses the
  // first row. Opening the menu that is already open from this button closes it.
  function open(button, { rows, title, empty, keyboard = false, closed } = {}) {
    if (isOpen() && anchor === button) {
      close();
      return;
    }
    close();
    anchor = button;
    label = title || "Terminals";
    onClose = closed || null;
    button.setAttribute("aria-expanded", "true");
    addLevel(rows, "", label, empty);
    document.addEventListener("pointerdown", outside, true);
    if (keyboard) {
      // A pane activated just before may focus its terminal a frame later.
      const first = () => {
        const level = levels[0];
        if (level && !level.el.contains(document.activeElement))
          level.el.querySelector("[data-menu-key]")?.focus();
      };
      first();
      requestAnimationFrame(() => requestAnimationFrame(first));
    }
  }

  // The tab strip redrew: keep the open menu on the same button's successor.
  function setAnchor(button) {
    if (!isOpen() || !button) return;
    anchor = button;
    button.setAttribute("aria-expanded", "true");
  }

  return {
    open,
    close,
    refresh,
    setAnchor,
    isOpen,
    anchor: () => anchor,
  };
}
