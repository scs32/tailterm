import { normalizeTabDecoration } from "./tab-decoration.js";
import { fonts } from "./appearance.js";
import {
  PaneGroups,
  leaves,
  tileLayout,
  paneNeighbor,
  prune,
} from "./pane-layout.js";
import { setupPaneDrag } from "./pane-drag.js";

export function setupPaneGroups({
  getTabs,
  getActive,
  isVisible = () => true,
  activate,
  close,
  upload,
  preferences,
  label,
}) {
  const model = new PaneGroups();
  const body = document.querySelector("#terminal-body"),
    strip = document.querySelector("#tabs");
  const chrome = document.createElement("div");
  chrome.className = "pane-chrome";
  body.append(chrome);
  const release = document.createElement("div");
  release.className = "pane-release";
  release.textContent = "Drop here to make a separate tab";
  release.hidden = true;
  document.querySelector(".terminal-tabs").append(release);
  let drag,
    layout,
    signature = "",
    resizing = false;
  const sync = () => model.sync(getTabs().map((t) => t.id));
  const members = (id) => {
    const g = model.group(id);
    return g
      ? leaves(g.tree).filter((id) =>
          getTabs().some((t) => t.id === id && isVisible(t)),
        )
      : [];
  };
  const project = (group) => {
    if (!group) return null;
    const tree = prune(
      group.tree,
      new Set(
        getTabs()
          .filter(isVisible)
          .map((t) => t.id),
      ),
    );
    if (!tree) return null;
    const ids = leaves(tree);
    return {
      ...group,
      tree,
      active: ids.includes(group.active) ? group.active : ids[0],
    };
  };
  const current = () => project(model.group(getActive()));
  function geometry(group) {
    const visible = project(group);
    return visible
      ? tileLayout(visible.tree, body.clientWidth, body.clientHeight)
      : { panes: [], dividers: [] };
  }
  function merge(source, target, whole = true) {
    sync();
    const g = model.group(target);
    if (!g) return;
    const boxes = geometry(g).panes;
    // Dropping on the tab splits its largest pane; a pane drop targets that pane.
    const box = boxes.find((p) => p.id === target);
    if (
      model.merge(source, target, {
        whole,
        axis: box.width > box.height ? "x" : "y",
      })
    )
      activate(source);
  }
  function detach(id) {
    if (model.detach(id)) activate(id);
  }
  function position(el, box) {
    Object.assign(el.style, {
      left: box.x + "px",
      top: box.y + "px",
      width: box.width + "px",
      height: box.height + "px",
    });
  }
  function resizeDivider(el, value) {
    const d = layout?.dividers.find((d) => d.node.id === el.dataset.divider);
    if (!d) return;
    const find = (tree) =>
      !tree || tree.tab
        ? null
        : tree.id === d.node.id
          ? tree
          : find(tree.a) || find(tree.b);
    const original = find(model.group(getActive())?.tree);
    if (original)
      original.ratio = Math.max(
        d.low / d.span,
        Math.min(d.high / d.span, value),
      );
    render();
  }
  function render() {
    const group = current(),
      ids = group ? leaves(group.tree) : [];
    if (group) model.group(getActive()).active = getActive();
    const grouped = group && leaves(model.group(getActive()).tree).length > 1;
    body.classList.toggle("has-panes", ids.length > 0);
    chrome.hidden = !ids.length;
    for (const t of getTabs()) {
      t.el.hidden = !ids.includes(t.id);
      t.el.classList.toggle("focused-pane", t.id === getActive());
      if (!ids.length) t.el.removeAttribute("style");
    }
    if (!ids.length) {
      signature = "";
      chrome.replaceChildren();
      return;
    }
    layout = geometry(group);
    const next = [
      grouped,
      ...ids,
      ...layout.dividers.map((d) => d.node.id),
    ].join("|");
    if (next !== signature) {
      signature = next;
      chrome.replaceChildren();
      for (const id of ids) {
        const header = document.createElement("div");
        header.className = "pane-header";
        header.dataset.pane = id;
        header.draggable = false;
        header.title = grouped
          ? "Drag to rearrange\nDrop onto another pane in this group to swap positions, or onto the tab bar to ungroup."
          : "Drag to group\nDrop onto another session tab to group these terminals.";
        const focus = document.createElement("button");
        focus.className = "pane-label";
        focus.onclick = () => activate(id);
        const detachButton = document.createElement("button");
        detachButton.textContent = "↗";
        detachButton.title =
          "Move to its own tab\nOr drag this pane’s header to the tab bar.";
        detachButton.setAttribute("aria-label", "Ungroup pane");
        detachButton.dataset.detach = id;
        detachButton.onclick = () => detach(id);
        const closeButton = document.createElement("button");
        closeButton.textContent = "×";
        closeButton.setAttribute("aria-label", "Close pane");
        closeButton.onclick = () => close(id);
        header.append(focus);
        if (upload) {
          const uploadButton = document.createElement("button");
          uploadButton.dataset.paneUpload = id;
          uploadButton.setAttribute(
            "aria-label",
            "Upload images to this session",
          );
          uploadButton.title =
            "Upload images\nChoose images to send to this session’s remote folder.";
          uploadButton.innerHTML =
            '<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5" aria-hidden="true"><rect x="3" y="3" width="18" height="18"/><circle cx="8" cy="8" r="2"/><path d="m3 18 6-6 4 4 3-3 5 5"/></svg>';
          uploadButton.onclick = () => upload(id);
          header.append(uploadButton);
        }
        if (grouped) header.append(detachButton);
        header.append(closeButton);
        chrome.append(header);
      }
      for (const d of layout.dividers) {
        const divider = document.createElement("div");
        divider.className = "pane-divider";
        divider.dataset.divider = d.node.id;
        divider.tabIndex = 0;
        divider.setAttribute("role", "separator");
        divider.setAttribute("aria-label", "Resize terminal panes");
        divider.title =
          "Resize panes\nDrag, or focus and use arrow keys. Double-click or press Enter to equalize.";
        divider.onpointerdown = (e) => {
          if (e.button !== 0) return;
          e.preventDefault();
          resizing = true;
          divider.setPointerCapture(e.pointerId);
          divider.focus();
        };
        divider.onpointermove = (e) => {
          if (!divider.hasPointerCapture(e.pointerId)) return;
          const live = layout.dividers.find(
            (d) => d.node.id === divider.dataset.divider,
          );
          const rect = body.getBoundingClientRect();
          const offset =
            live.axis === "x"
              ? e.clientX - rect.left + body.scrollLeft - live.box.x
              : e.clientY - rect.top + body.scrollTop - live.box.y;
          resizeDivider(divider, (offset - 4) / live.span);
        };
        divider.onpointerup = (e) => {
          if (divider.hasPointerCapture(e.pointerId))
            divider.releasePointerCapture(e.pointerId);
          resizing = false;
        };
        divider.onlostpointercapture = () => {
          resizing = false;
        };
        divider.ondblclick = () => resizeDivider(divider, 0.5);
        divider.onkeydown = (e) => {
          const live = layout.dividers.find(
            (d) => d.node.id === divider.dataset.divider,
          );
          const arrows =
            live.axis === "x"
              ? ["ArrowLeft", "ArrowRight"]
              : ["ArrowUp", "ArrowDown"];
          if (![...arrows, "Enter", "Home", "End"].includes(e.key)) return;
          e.preventDefault();
          e.stopPropagation();
          resizeDivider(
            divider,
            e.key === "Enter"
              ? 0.5
              : e.key === "Home"
                ? 0
                : e.key === "End"
                  ? 1
                  : live.ratio +
                    (e.key === arrows[0] ? -1 : 1) * (e.shiftKey ? 0.1 : 0.02),
          );
        };
        chrome.append(divider);
      }
    }
    chrome.style.width = layout.width + "px";
    chrome.style.height = layout.height + "px";
    for (const box of layout.panes) {
      const t = getTabs().find((t) => t.id === box.id);
      const header = chrome.querySelector(`[data-pane="${box.id}"]`);
      header.classList.toggle("active", box.id === getActive());
      const decoration = normalizeTabDecoration(t.decoration);
      header.dataset.tabColor = decoration.color;
      t.el.dataset.tabColor = decoration.color;
      header.dataset.tabFill = decoration.fill;
      header.style.setProperty(
        "--tab-font",
        fonts[decoration.font]?.family || "var(--terminal-font)",
      );
      const uploadButton = header.querySelector("[data-pane-upload]");
      if (uploadButton) uploadButton.disabled = t.status !== "Connected";
      const button = header.querySelector(".pane-label");
      button.textContent = `${label(t)} · ${t.tmux ? t.session : "SSH"} · ${t.status}`;
      button.title = `${t.server.username}@${t.server.host}\n${grouped ? "Drag onto another pane to swap positions; onto another group to move; or onto the tab bar to ungroup." : "Drag onto another session tab to group these terminals."}`;
      position(header, { ...box, height: 30 });
      position(t.el, { ...box, y: box.y + 30, height: box.height - 30 });
    }
    for (const d of layout.dividers) {
      const el = chrome.querySelector(`[data-divider="${d.node.id}"]`);
      position(el, d);
      el.dataset.axis = d.axis;
      el.setAttribute(
        "aria-orientation",
        d.axis === "x" ? "vertical" : "horizontal",
      );
      el.setAttribute("aria-valuemin", Math.round((d.low / d.span) * 100));
      el.setAttribute("aria-valuemax", Math.round((d.high / d.span) * 100));
      el.setAttribute("aria-valuenow", Math.round(d.ratio * 100));
    }
  }
  function dragTarget(element) {
    const direct = element?.closest("[data-pane], .tab, .pane-release");
    if (direct) return direct;
    const tab = getTabs().find((t) => !t.el.hidden && t.el.contains(element));
    return tab ? chrome.querySelector(`[data-pane="${tab.id}"]`) : null;
  }
  function clearDrag() {
    drag = null;
    release.hidden = true;
    document
      .querySelectorAll(".group-drop-target, .tab-drop-before, .tab-drop-after")
      .forEach((el) =>
        el.classList.remove(
          "group-drop-target",
          "tab-drop-before",
          "tab-drop-after",
        ),
      );
  }
  setupPaneDrag(document.querySelector(".terminal-shell"), {
    start(source) {
      const id =
        source.dataset.pane || source.querySelector("[data-tab]")?.dataset.tab;
      drag = { id, whole: !source.dataset.pane };
      release.hidden = !source.dataset.pane;
    },
    move(element, x) {
      document
        .querySelectorAll(
          ".group-drop-target, .tab-drop-before, .tab-drop-after",
        )
        .forEach((el) =>
          el.classList.remove(
            "group-drop-target",
            "tab-drop-before",
            "tab-drop-after",
          ),
        );
      const target = dragTarget(element);
      if (drag?.whole && target?.matches(".tab")) {
        const box = target.getBoundingClientRect(),
          position = (x - box.left) / box.width;
        drag.reorder =
          position < 0.25 ? "before" : position > 0.75 ? "after" : null;
        target.classList.add(
          drag.reorder ? "tab-drop-" + drag.reorder : "group-drop-target",
        );
      } else {
        if (drag) drag.reorder = null;
        target?.classList.add("group-drop-target");
      }
      if (element && strip.contains(element)) {
        const bounds = strip.getBoundingClientRect();
        if (x < bounds.left + 30) strip.scrollLeft -= 20;
        if (x > bounds.right - 30) strip.scrollLeft += 20;
      }
    },
    drop(element) {
      if (!drag) return;
      const target = dragTarget(element);
      let id =
        target?.dataset.pane ||
        target?.querySelector("[data-tab]")?.dataset.tab;
      if (id && !target.dataset.pane) {
        const group = model.group(id);
        id = geometry(group).panes.sort(
          (a, b) => b.width * b.height - a.width * a.height,
        )[0].id;
      }
      const source = drag;
      clearDrag();
      if (id && source.whole && source.reorder) {
        model.reorder(source.id, id, source.reorder === "after");
        activate(getActive());
      } else if (
        id &&
        !source.whole &&
        target.dataset.pane &&
        model.group(source.id) === model.group(id)
      ) {
        if (model.swap(source.id, id)) activate(source.id);
      } else if (id) merge(source.id, id, source.whole);
      else if (!source.whole && element?.closest(".terminal-tabs"))
        detach(source.id);
    },
    cancel: clearDrag,
  });
  // Selecting a pane updates all active-terminal controls, not just the DOM focus.
  body.addEventListener(
    "pointerdown",
    (e) => {
      const t = getTabs().find((t) => t.el.contains(e.target));
      if (t && t.id !== getActive() && !document.querySelector("dialog[open]"))
        activate(t.id);
    },
    true,
  );
  body.addEventListener(
    "pointermove",
    (e) => {
      if (
        e.pointerType !== "mouse" ||
        e.buttons ||
        drag ||
        resizing ||
        !preferences().focusFollowsMouse ||
        document.querySelector("dialog[open]")
      )
        return;
      const t = getTabs().find((t) => !t.el.hidden && t.el.contains(e.target));
      if (t && t.id !== getActive()) activate(t.id);
    },
    { passive: true },
  );
  new ResizeObserver(render).observe(body);
  return {
    model,
    sync,
    render,
    members,
    merge,
    detach,
    navigate(direction) {
      const group = current();
      if (!group) return;
      const next = paneNeighbor(geometry(group).panes, getActive(), direction);
      if (next) activate(next);
    },
    reorder(source, target, after) {
      model.reorder(source, target, after);
      activate(getActive());
    },
    entries: () =>
      model.groups
        .map(project)
        .filter(Boolean)
        .map((g) => ({
          tab: getTabs().find((t) => t.id === g.active),
          ids: leaves(g.tree),
          group: model.group(g.active),
        })),
  };
}
