// One delegated, keyboard-accessible tooltip. Popover puts it above fullscreen/dialogs.
export function setupTooltips() {
  const tip = document.createElement("div");
  tip.id = "workspace-tooltip";
  tip.className = "workspace-tooltip";
  tip.setAttribute("role", "tooltip");
  tip.setAttribute("popover", "manual");
  document.body.append(tip);
  let target, timer;
  const hide = () => {
    clearTimeout(timer);
    if (tip.matches(":popover-open")) tip.hidePopover();
    target?.removeAttribute("aria-describedby");
    target = null;
  };
  const show = (el, delay = 350) => {
    if (target === el) return;
    hide();
    target = el;
    timer = setTimeout(() => {
      if (!el.isConnected) return hide();
      const lines = (el.dataset.tooltip || el.title || "").split("\n");
      if (!lines[0]) return;
      tip.replaceChildren();
      for (let i = 0; i < lines.length; i++) {
        const row = document.createElement(i ? "span" : "strong");
        row.textContent = lines[i];
        tip.append(row);
      }
      if (el.dataset.shortcut) {
        const key = document.createElement("kbd");
        key.textContent = el.dataset.shortcut;
        tip.append(key);
      }
      el.setAttribute("aria-describedby", tip.id);
      tip.showPopover();
      const r = el.getBoundingClientRect(),
        box = tip.getBoundingClientRect();
      tip.style.left =
        Math.max(
          8,
          Math.min(
            innerWidth - box.width - 8,
            r.left + r.width / 2 - box.width / 2,
          ),
        ) + "px";
      tip.style.top =
        Math.max(
          8,
          r.bottom + box.height + 10 < innerHeight
            ? r.bottom + 8
            : r.top - box.height - 8,
        ) + "px";
    }, delay);
  };
  const convert = (root) => {
    const nodes = root.querySelectorAll?.("[title]") || [];
    for (const el of [root, ...nodes])
      if (el.hasAttribute?.("title")) {
        el.dataset.tooltip = el.getAttribute("title");
        el.removeAttribute("title");
        if (el.tagName === "BUTTON" && !el.hasAttribute("aria-label"))
          el.setAttribute("aria-label", el.dataset.tooltip.split("\n")[0]);
      }
  };
  convert(document.body);
  new MutationObserver((records) => {
    for (const r of records) {
      if (r.type === "attributes") convert(r.target);
      else
        for (const node of r.addedNodes)
          if (node.nodeType === 1 && node !== tip && !tip.contains(node))
            convert(node);
    }
    if (target && !target.isConnected) hide();
  }).observe(document.body, {
    childList: true,
    subtree: true,
    attributes: true,
    attributeFilter: ["title"],
  });
  document.addEventListener("pointerover", (e) => {
    if (target === document.activeElement) return;
    const el = e.target.closest("[data-tooltip]");
    if (el) show(el);
    else hide();
  });
  document.addEventListener("pointerout", (e) => {
    if (
      target &&
      target !== document.activeElement &&
      !target.contains(e.relatedTarget)
    )
      hide();
  });
  document.addEventListener("focusin", (e) => {
    const el = e.target.closest("[data-tooltip]");
    if (el) show(el, 0);
  });
  document.addEventListener("focusout", hide);
  document.addEventListener("keydown", (e) => {
    if (e.key === "Escape") hide();
  });
  document.addEventListener("pointerdown", hide);
  document.addEventListener("scroll", hide, true);
  document.addEventListener("fullscreenchange", hide);
}
