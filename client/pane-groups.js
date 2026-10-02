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
import { continuedSuffix, normalizePaneLimit } from "./pane-cap.js";

const escapeHTML = (value) =>
  String(value ?? "").replace(
    /[&<>"']/g,
    (c) =>
      ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[
        c
      ],
  );

export function setupPaneGroups({
  getTabs,
  getActive,
  isVisible = () => true,
  activate,
  close,
  upload,
  dialog,
  closeDialog,
  preferences,
  label,
  groupName = (group) => group.taskId || "Group",
  changed = () => {},
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
  const placementPreview = document.createElement("div");
  placementPreview.className = "pane-drop-preview";
  placementPreview.setAttribute("aria-hidden", "true");
  placementPreview.hidden = true;
  let drag,
    layout,
    signature = "",
    resizing = false,
    shownGroup = null;
  const tab = (id) => getTabs().find((t) => t.id === id);
  const sync = () => {
    model.limit = normalizePaneLimit(preferences().paneGroupLimit);
    const taskOf = (id) => tab(id)?.task?.taskId,
      agentOf = (id) => tab(id)?.task?.agentId,
      homeOf = (id) => !!tab(id)?.home;
    model.sync(
      getTabs().map((t) => t.id),
      taskOf,
      agentOf,
      homeOf,
    );
    model.isolateTasks(taskOf, agentOf);
  };
  const visibleIds = () =>
    new Set(
      getTabs()
        .filter(isVisible)
        .map((t) => t.id),
    );
  const projectHome = () => {
    if (!model.home) return null;
    const tree = prune(model.home.tree, visibleIds());
    if (!tree) return null;
    const ids = leaves(tree);
    return {
      ...model.home,
      tree,
      active: ids.includes(model.home.active) ? model.home.active : ids[0],
    };
  };
  const members = (id) => {
    if (model.inHome(id)) {
      const home = projectHome();
      return home ? leaves(home.tree) : [];
    }
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
  // Which region shows: home alone at full width while a home pane is focused,
  // otherwise the active group alone. Neither is ever beside the other.
  function regions() {
    const active = getActive();
    if (!model.inHome(active)) {
      const group = current();
      shownGroup = group ? active : null;
      return { home: null, group, focus: group ? "group" : null };
    }
    shownGroup = null;
    const home = projectHome();
    return { home, group: null, focus: home ? "home" : null };
  }
  function arrangement(r = regions()) {
    const width = body.clientWidth,
      height = body.clientHeight;
    if (r.home) {
      const home = tileLayout(r.home.tree, width, height);
      return {
        ...home,
        dividers: home.dividers.map((d) => ({ ...d, home: true })),
      };
    }
    return r.group
      ? tileLayout(r.group.tree, width, height)
      : { panes: [], dividers: [], width: 0, height: 0 };
  }
  function geometry(group) {
    const visible = project(group);
    return visible
      ? tileLayout(visible.tree, body.clientWidth, body.clientHeight)
      : { panes: [], dividers: [] };
  }
  // Name of a series part: its base name plus "(continued k)".
  const partName = (group, rank = model.series(group).indexOf(group)) => {
    const parts = model.series(group);
    const own = !group.taskId && rank > 0 && group.decoration?.label;
    return (
      (own ? groupName(group) : groupName(parts[0] || group)) +
      continuedSuffix(Math.max(0, rank))
    );
  };
  // A full target offers to send the pane on, or to move a pane out.
  function full(source, target, whole) {
    if (!dialog) return;
    const from = model.group(source),
      to = model.group(target),
      parts = model.series(to);
    const count = whole ? leaves(from.tree).length : 1;
    const sendable = !parts.includes(from);
    const sendRank = parts.findIndex(
      (part) => leaves(part.tree).length + count <= model.limit,
    );
    const roomRank = parts.findIndex(
      (part) =>
        part !== to &&
        leaves(part.tree).length - (part === from ? 1 : 0) < model.limit,
    );
    const moved = getTabs().find((t) => t.id === target);
    const name = partName(to);
    dialog(
      `${name} is full`,
      `<p>${escapeHTML(name)} is full (${leaves(to.tree).length} of ${model.limit} panes).</p><div class="dialog-menu">${sendable ? `<button data-pane-full="send">Send to ${escapeHTML(partName(to, sendRank < 0 ? parts.length : sendRank))}</button>` : ""}${whole ? "" : `<button data-pane-full="room">Make room: move ${escapeHTML(moved ? label(moved) : target)} to ${escapeHTML(partName(to, roomRank < 0 ? parts.length : roomRank))}</button>`}<button data-pane-full="cancel">Cancel</button></div>`,
    );
    for (const button of document.querySelectorAll("[data-pane-full]"))
      button.onclick = () => {
        const action = button.dataset.paneFull;
        closeDialog();
        sync();
        if (
          action === "send"
            ? model.send(source, target, whole)
            : action === "room" && model.makeRoom(source, target)
        ) {
          changed();
          activate(source);
        }
      };
  }
  const refused = (source, target, whole) =>
    model.canMerge(source, target, whole) &&
    !model.canFit(source, target, whole);
  function merge(source, target, whole = true) {
    sync();
    // Home holds only the owner helper: nothing joins it and it joins nothing.
    if (model.inHome(target) || model.inHome(source)) return;
    const g = model.group(target);
    if (!g) return;
    if (refused(source, target, whole)) return full(source, target, whole);
    const boxes = geometry(g).panes;
    // Dropping on the tab splits its largest pane; a pane drop targets that pane.
    const box = boxes.find((p) => p.id === target);
    if (
      model.merge(source, target, {
        whole,
        axis: box.width > box.height ? "x" : "y",
      })
    ) {
      changed();
      activate(source);
    }
  }
  function place(source, target, placement) {
    sync();
    if (model.inHome(target)) {
      if (model.homePlace(source, target, placement)) {
        changed();
        activate(source);
      }
      return;
    }
    if (model.inHome(source)) return;
    if (refused(source, target, false)) return full(source, target, false);
    if (model.place(source, target, placement)) {
      changed();
      activate(source);
    }
  }
  function detach(id) {
    if (model.detach(id)) {
      changed();
      activate(id);
    }
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
    const owner = d.home ? null : shownGroup;
    const original = find(d.home ? model.home?.tree : model.group(owner)?.tree);
    if (original) {
      if (owner) model.customize(owner);
      original.ratio = Math.max(
        d.low / d.span,
        Math.min(d.high / d.span, value),
      );
      if (owner) model.remember(owner);
      changed();
    }
    render();
  }
  function render() {
    const r = regions(),
      group = r.group,
      homeIds = r.home ? leaves(r.home.tree) : [],
      ids = [...homeIds, ...(group ? leaves(group.tree) : [])];
    if (r.focus === "group" && !model.projectLayouts.has(group.taskId))
      model.group(getActive()).active = getActive();
    if (r.focus === "home") model.home.active = getActive();
    const grouped = !!group && leaves(model.group(shownGroup).tree).length > 1;
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
    layout = arrangement(r);
    const next = [
      grouped,
      group?.taskId || "",
      ...(group?.guests || []),
      ...ids,
      ...layout.dividers.map((d) => d.node.id),
      ...homeIds.map((id) => "home:" + id),
    ].join("|");
    if (next !== signature) {
      signature = next;
      chrome.replaceChildren();
      for (const id of ids) {
        const inHome = homeIds.includes(id);
        const header = document.createElement("div");
        header.className = "pane-header";
        header.dataset.pane = id;
        if (inHome) header.dataset.home = "";
        header.draggable = false;
        header.title = inHome
          ? "Owner helper: stays in Home."
          : group?.taskId && !group.guests?.includes(id)
            ? "Drag to rearrange within this project. Project agents stay in their project group.\nLeft Option: split right · Shift + Left Option: split above."
            : grouped
              ? "Drag to rearrange\nDrop onto another pane in this group to swap positions, or onto the tab bar to ungroup.\nLeft Option: split right · Shift + Left Option: split above."
              : "Drag to group\nDrop onto another session tab to group these terminals.";
        const focus = document.createElement("button");
        focus.className = "pane-label";
        focus.onclick = () => {
          model.rememberActive(id);
          changed();
          activate(id);
        };
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
        if (!inHome && grouped && model.canDetach(id))
          header.append(detachButton);
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
      chrome.append(placementPreview);
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
  const homeTarget = (target) =>
    !!target &&
    (target.matches(".home-tab") ||
      (!!target.dataset.pane && model.inHome(target.dataset.pane)));
  // Whether a drag may end on target, when home is involved (null when it is
  // not): home panes only rearrange among themselves, and nothing else enters.
  function homeDrop(source, target, targetId) {
    const into = homeTarget(target);
    if (source.home)
      return into && !!target.dataset.pane && targetId !== source.id;
    return into ? false : null;
  }
  function homeDropped(source, target, targetId, gesture) {
    if (!source.home || !homeTarget(target)) return;
    if (!target.dataset.pane || targetId === source.id) return;
    if (
      gesture?.placement
        ? model.homePlace(source.id, targetId, gesture.placement)
        : model.homeSwap(source.id, targetId)
    ) {
      changed();
      activate(source.id);
    }
  }
  function dragTarget(element) {
    const direct = element?.closest("[data-pane], .tab, .pane-release");
    if (direct) return direct;
    const tab = getTabs().find((t) => !t.el.hidden && t.el.contains(element));
    return tab ? chrome.querySelector(`[data-pane="${tab.id}"]`) : null;
  }
  const canPlace = (source, target) => {
    if (!source || !target || source === target) return false;
    const from = model.group(source),
      to = model.group(target);
    return (
      !!from && !!to && (from === to || model.canMerge(source, target, false))
    );
  };
  function clearDropFeedback() {
    document
      .querySelectorAll(".group-drop-target, .tab-drop-before, .tab-drop-after")
      .forEach((el) =>
        el.classList.remove(
          "group-drop-target",
          "tab-drop-before",
          "tab-drop-after",
        ),
      );
    placementPreview.hidden = true;
    delete placementPreview.dataset.placement;
  }
  function showPlacement(target, placement) {
    const box = layout?.panes.find((pane) => pane.id === target);
    if (!box) return;
    const gap = 8;
    if (placement === "right") {
      const first = (box.width - gap) / 2;
      position(placementPreview, {
        x: box.x + first + gap,
        y: box.y,
        width: box.width - gap - first,
        height: box.height,
      });
      placementPreview.textContent = "Split right";
    } else {
      position(placementPreview, {
        x: box.x,
        y: box.y,
        width: box.width,
        height: (box.height - gap) / 2,
      });
      placementPreview.textContent = "Split above";
    }
    placementPreview.dataset.placement = placement;
    placementPreview.hidden = false;
  }
  function clearDrag() {
    drag = null;
    release.hidden = true;
    clearDropFeedback();
  }
  setupPaneDrag(document.querySelector(".terminal-shell"), {
    start(source) {
      // Home is not a group: its tab never drags.
      if (source.matches(".home-tab")) {
        drag = null;
        return;
      }
      const id =
        source.dataset.pane || source.querySelector("[data-tab]")?.dataset.tab;
      drag = { id, whole: !source.dataset.pane, home: model.inHome(id) };
      release.hidden =
        !source.dataset.pane || drag.home || !model.canDetach(id);
    },
    move(element, x, _y, gesture) {
      clearDropFeedback();
      const target = dragTarget(element);
      const targetId =
        target?.dataset.pane ||
        target?.querySelector("[data-tab]")?.dataset.tab;
      if (drag) {
        drag.reorder = null;
        drag.placement = null;
      }
      const home = drag && homeDrop(drag, target, targetId);
      if (drag && home !== null && (drag.home || homeTarget(target))) {
        if (home && target) {
          if (target.dataset.pane && gesture?.placement) {
            drag.placement = gesture.placement;
            showPlacement(targetId, drag.placement);
          } else target.classList.add("group-drop-target");
        }
      } else if (drag?.whole && target?.matches(".tab")) {
        const box = target.getBoundingClientRect(),
          position = (x - box.left) / box.width;
        drag.reorder =
          position < 0.25 ? "before" : position > 0.75 ? "after" : null;
        target.classList.add(
          drag.reorder ? "tab-drop-" + drag.reorder : "group-drop-target",
        );
      } else if (
        drag &&
        !drag.whole &&
        target?.dataset.pane &&
        gesture?.placement &&
        canPlace(drag.id, targetId)
      ) {
        drag.placement = gesture.placement;
        showPlacement(targetId, drag.placement);
      } else {
        const sameGroupPane =
          drag &&
          !drag.whole &&
          target?.dataset.pane &&
          drag.id !== targetId &&
          model.group(drag.id) === model.group(targetId);
        if (
          drag &&
          targetId &&
          drag.id !== targetId &&
          (sameGroupPane || model.canMerge(drag.id, targetId, drag.whole))
        )
          target.classList.add("group-drop-target");
      }
      if (element && strip.contains(element)) {
        const bounds = strip.getBoundingClientRect();
        if (x < bounds.left + 30) strip.scrollLeft -= 20;
        if (x > bounds.right - 30) strip.scrollLeft += 20;
      }
    },
    drop(element, gesture) {
      if (!drag) return;
      const target = dragTarget(element);
      let id =
        target?.dataset.pane ||
        target?.querySelector("[data-tab]")?.dataset.tab;
      if (id && !target.dataset.pane && model.group(id)) {
        const group = model.group(id);
        id = geometry(group).panes.sort(
          (a, b) => b.width * b.height - a.width * a.height,
        )[0].id;
      }
      const source = drag;
      clearDrag();
      if (source.home || homeTarget(target)) {
        homeDropped(source, target, id, gesture);
        return;
      }
      if (
        id &&
        !source.whole &&
        target.dataset.pane &&
        gesture?.placement &&
        canPlace(source.id, id)
      ) {
        place(source.id, id, gesture.placement);
      } else if (id && source.whole && source.reorder) {
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
      if (
        t &&
        t.id !== getActive() &&
        !document.querySelector("dialog[open]")
      ) {
        model.rememberActive(t.id);
        changed();
        activate(t.id);
      }
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
      if (t && t.id !== getActive()) {
        model.rememberActive(t.id);
        changed();
        activate(t.id);
      }
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
    place,
    detach,
    showMove(id) {
      const others = members(id).filter((other) => other !== id);
      if (!dialog || !others.length) return;
      dialog(
        "Move pane",
        '<label>Next to<select id="pane-move-target"></select></label><div class="dialog-menu"><button data-pane-move="above">Move above</button><button data-pane-move="below">Move below</button><button data-pane-move="left">Move left</button><button data-pane-move="right">Move right</button></div>',
      );
      const select = document.querySelector("#pane-move-target");
      for (const other of others)
        select.append(
          new Option(label(getTabs().find((t) => t.id === other)), other),
        );
      for (const button of document.querySelectorAll("[data-pane-move]"))
        button.onclick = () => {
          const target = select.value;
          closeDialog();
          place(id, target, button.dataset.paneMove);
        };
      select.focus();
    },
    // Programmatic placement never prompts: the earliest part with room.
    send(source, target, whole = false) {
      sync();
      if (model.send(source, target, whole)) {
        changed();
        activate(source);
      }
    },
    partName,
    navigate(direction) {
      const next = paneNeighbor(arrangement().panes, getActive(), direction);
      if (next) activate(next);
    },
    reorder(source, target, after) {
      model.reorder(source, target, after);
      changed();
      activate(getActive());
    },
    inHome: (id) => model.inHome(id),
    // The pinned Home tab: home's focused visible pane and its members.
    homeEntry() {
      const home = projectHome();
      return home
        ? { tab: tab(home.active), ids: leaves(home.tree), home: true }
        : null;
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
