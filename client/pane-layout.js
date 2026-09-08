export const leaves = (tree) =>
  tree.tab ? [tree.tab] : [...leaves(tree.a), ...leaves(tree.b)];
export function paneNeighbor(panes, id, direction) {
  const source = panes.find((p) => p.id === id);
  if (!source || !["left", "right", "up", "down"].includes(direction))
    return null;
  const horizontal = direction === "left" || direction === "right";
  const forward = direction === "right" || direction === "down";
  const axis = horizontal ? "x" : "y",
    size = horizontal ? "width" : "height";
  const cross = horizontal ? "y" : "x",
    crossSize = horizontal ? "height" : "width";
  return (
    panes
      .filter((p) => p.id !== id)
      .map((p) => ({
        id: p.id,
        gap: forward
          ? p[axis] - source[axis] - source[size]
          : source[axis] - p[axis] - p[size],
        overlap:
          Math.min(p[cross] + p[crossSize], source[cross] + source[crossSize]) -
          Math.max(p[cross], source[cross]),
        offset: Math.abs(
          p[cross] + p[crossSize] / 2 - source[cross] - source[crossSize] / 2,
        ),
      }))
      .filter((p) => p.gap >= -1 && p.overlap > 0)
      .sort((a, b) => a.gap - b.gap || a.offset - b.offset)[0]?.id || null
  );
}
export function prune(tree, ids) {
  if (tree.tab) return ids.has(tree.tab) ? tree : null;
  const a = prune(tree.a, ids),
    b = prune(tree.b, ids);
  return a && b ? { ...tree, a, b } : a || b;
}
function replace(tree, id, replacement) {
  if (tree.tab) return tree.tab === id ? replacement : tree;
  return {
    ...tree,
    a: replace(tree.a, id, replacement),
    b: replace(tree.b, id, replacement),
  };
}
const split = (axis, a, b, ratio = 0.5) => ({
  id: crypto.randomUUID(),
  axis,
  ratio,
  a,
  b,
});
const sameShape = (a, b) =>
  a.tab || b.tab
    ? a.tab === b.tab
    : a.axis === b.axis && sameShape(a.a, b.a) && sameShape(a.b, b.b);
const legacyTaskTree = (tree) =>
  !!tree.tab ||
  (tree.axis === "x" &&
    tree.ratio === 0.5 &&
    legacyTaskTree(tree.a) &&
    legacyTaskTree(tree.b));
function taskTree(ids) {
  const column = (tabs) =>
    tabs.length === 1
      ? { tab: tabs[0] }
      : split("y", { tab: tabs[0] }, column(tabs.slice(1)), 1 / tabs.length);
  if (ids.length === 1) return { tab: ids[0] };
  if (ids.length === 2) return split("x", { tab: ids[0] }, { tab: ids[1] });
  const workers = ids.slice(1);
  return split(
    "x",
    { tab: ids[0] },
    split(
      "x",
      column(workers.filter((_, i) => i % 2 === 0)),
      column(workers.filter((_, i) => i % 2 === 1)),
    ),
    1 / 3,
  );
}

const stableLeaves = (tree) =>
  !tree
    ? []
    : tree.agentId
      ? [{ agentId: tree.agentId }]
      : tree.tabId
        ? [{ tabId: tree.tabId }]
        : [...stableLeaves(tree.a), ...stableLeaves(tree.b)];
const stableKey = (leaf) =>
  leaf?.agentId ? `agent:${leaf.agentId}` : `tab:${leaf?.tabId}`;
const stablePrune = (tree, keep) => {
  if (!tree) return null;
  if (tree.agentId || tree.tabId)
    return keep.has(stableKey(tree)) ? tree : null;
  const a = stablePrune(tree.a, keep),
    b = stablePrune(tree.b, keep);
  return a && b ? { ...tree, a, b } : a || b;
};
const stableTaskTree = (agentIds) => {
  const convert = (tree) =>
    tree.tab
      ? { agentId: tree.tab }
      : { ...tree, a: convert(tree.a), b: convert(tree.b) };
  return convert(taskTree(agentIds));
};
const stableTree = (tree, taskId, taskOf, agentOf) => {
  if (tree.tab) {
    const agentId = taskOf(tree.tab) === taskId && agentOf(tree.tab);
    return agentId ? { agentId } : { tabId: tree.tab };
  }
  return {
    id: tree.id,
    axis: tree.axis,
    ratio: tree.ratio,
    a: stableTree(tree.a, taskId, taskOf, agentOf),
    b: stableTree(tree.b, taskId, taskOf, agentOf),
  };
};
const appendStable = (tree, leaf) =>
  tree ? split("x", tree, leaf) : structuredClone(leaf);

export class PaneGroups {
  groups = [];
  tabOrder = [];
  taskOrchestrators = new Map();
  projectLayouts = new Map();
  taskMembers = new Map();
  tabTasks = new Map();
  tabAgents = new Map();
  loadProjectLayouts(layouts = []) {
    this.projectLayouts.clear();
    for (const layout of layouts)
      if (layout?.taskId && layout.tree)
        this.projectLayouts.set(layout.taskId, structuredClone(layout));
  }
  projectLayoutSnapshot() {
    return [...this.projectLayouts.values()].map((layout) =>
      structuredClone(layout),
    );
  }
  // A successful hub roster is authoritative. An empty roster deliberately
  // removes the template and prevents stale live panes from recreating it.
  setTaskMembers(taskId, agentIds) {
    const members = [...new Set(agentIds.filter(Boolean))].slice(0, 32);
    this.taskMembers.set(taskId, members);
    if (!members.length) {
      this.projectLayouts.delete(taskId);
      return;
    }
    const layout = this.projectLayouts.get(taskId);
    if (layout) this.#reconcileMembers(layout, members);
  }
  rememberActive(tab) {
    const group = this.group(tab);
    if (!group?.taskId || this.taskMembers.get(group.taskId)?.length === 0)
      return;
    const layout = this.projectLayouts.get(group.taskId);
    if (!layout) return;
    const agentId = this.tabAgents.get(tab);
    if (this.tabTasks.get(tab) === group.taskId && agentId) {
      layout.activeAgentId = agentId;
      delete layout.activeTabId;
    } else {
      layout.activeTabId = tab;
      delete layout.activeAgentId;
    }
    group.active = tab;
  }
  remember(tab) {
    const group = this.group(tab);
    if (group?.taskId) this.#capture(group);
  }
  setTaskOrchestrator(taskId, tabId) {
    if (tabId) this.taskOrchestrators.set(taskId, tabId);
    else this.taskOrchestrators.delete(taskId);
  }
  customize(tab) {
    const group = this.group(tab);
    if (group?.taskId) group.taskLayout = "manual";
  }
  group(tab) {
    return this.groups.find((g) => leaves(g.tree).includes(tab));
  }
  // taskOf(tabId) supplies the task a newly grouped tab belongs to, so a
  // singleton group created for an agent pane inherits its task binding.
  sync(ids, taskOf = () => undefined, agentOf = () => undefined) {
    this.tabOrder = [...ids];
    this.tabTasks = new Map(ids.map((id) => [id, taskOf(id)]));
    this.tabAgents = new Map(ids.map((id) => [id, agentOf(id)]));
    for (const group of this.groups)
      if (
        group.taskId &&
        !this.projectLayouts.has(group.taskId) &&
        this.taskMembers.get(group.taskId)?.length > 0
      )
        this.#capture(group);
    const valid = new Set(ids);
    this.groups = this.groups.flatMap((g) => {
      const tree = prune(g.tree, valid);
      if (!tree) return [];
      return [
        {
          ...g,
          decoration: tree.tab ? undefined : g.decoration,
          tree,
          active: leaves(tree).includes(g.active) ? g.active : leaves(tree)[0],
        },
      ];
    });
    for (const id of ids)
      if (!this.group(id)) {
        const group = { tree: { tab: id }, active: id };
        const taskId = taskOf(id);
        if (taskId && !this.groups.some((g) => g.taskId === taskId))
          group.taskId = taskId;
        this.groups.push(group);
      }
  }
  // Split legacy mixed groups and gather each task's panes into one group.
  isolateTasks(taskOf, agentOf = (id) => this.tabAgents.get(id)) {
    const result = [],
      tasks = new Map();
    for (const group of this.groups) {
      const ids = leaves(group.tree);
      const membership = (id) =>
        taskOf(id) || (group.guests?.includes(id) ? group.taskId : undefined);
      const keys = [...new Set(ids.map(membership))];
      for (const taskId of keys) {
        const tree = prune(
          group.tree,
          new Set(ids.filter((id) => membership(id) === taskId)),
        );
        const part = {
          ...group,
          tree,
          active: leaves(tree).includes(group.active)
            ? group.active
            : leaves(tree)[0],
        };
        if (!taskId) {
          delete part.taskId;
          delete part.taskName;
          delete part.guests;
          delete part.taskLayout;
          result.push(part);
          continue;
        }
        part.taskId = taskId;
        part.guests = leaves(tree).filter((id) => !taskOf(id));
        const ordered = this.tabOrder.filter((id) => leaves(tree).includes(id));
        const originalOrder = leaves(tree).every((id, i) => id === ordered[i]);
        part.taskLayout =
          group.taskLayout ||
          (!part.guests.length && originalOrder && legacyTaskTree(tree)
            ? "auto"
            : "manual");
        const target = tasks.get(taskId);
        if (target) {
          if (part.taskLayout === "manual") target.taskLayout = "manual";
          target.guests = [...(target.guests || []), ...part.guests];
          target.tree = {
            id: crypto.randomUUID(),
            axis: "x",
            ratio: 0.5,
            a: target.tree,
            b: tree,
          };
        } else {
          tasks.set(taskId, part);
          result.push(part);
        }
      }
    }
    this.groups = result;
    for (const group of tasks.values()) {
      if (group.taskLayout !== "auto" || group.guests.length) continue;
      const members = leaves(group.tree);
      const ids = [
        ...this.tabOrder.filter((id) => members.includes(id)),
        ...members.filter((id) => !this.tabOrder.includes(id)),
      ];
      const anchor = this.taskOrchestrators.get(group.taskId);
      if (ids.includes(anchor))
        ids.unshift(...ids.splice(ids.indexOf(anchor), 1));
      const tree = taskTree(ids);
      // Stable membership preserves divider IDs/ratios and focused terminals.
      if (!sameShape(group.tree, tree)) group.tree = tree;
    }
    this.#applyProjectLayouts(taskOf, agentOf);
  }
  taskGroup(taskId) {
    return this.groups.find((g) => g.taskId === taskId);
  }
  canMerge(source, target, whole = true) {
    const from = this.group(source),
      to = this.group(target);
    if (!from || !to || from === to) return false;
    if (whole) return !from.taskId && !to.taskId;
    return !from.taskId || !!from.guests?.includes(source);
  }
  canDetach(tab) {
    const group = this.group(tab);
    return (
      !!group &&
      leaves(group.tree).length > 1 &&
      (!group.taskId || !!group.guests?.includes(tab))
    );
  }
  merge(source, target, { whole = true, axis = "x", before = false } = {}) {
    const from = this.group(source),
      to = this.group(target);
    if (!this.canMerge(source, target, whole)) return false;
    if (to.taskId) {
      to.guests = [...(to.guests || []), source];
      to.taskLayout = "manual";
    }
    if (from.guests) from.guests = from.guests.filter((id) => id !== source);
    const incoming = whole ? from.tree : { tab: source };
    if (whole) this.groups = this.groups.filter((g) => g !== from);
    else {
      from.tree = prune(
        from.tree,
        new Set(leaves(from.tree).filter((id) => id !== source)),
      );
      if (!from.tree) this.groups = this.groups.filter((g) => g !== from);
      else if (from.active === source) from.active = leaves(from.tree)[0];
    }
    if (to.tree.tab) delete to.decoration;
    if (from.tree?.tab) delete from.decoration;
    to.tree = replace(to.tree, target, {
      id: crypto.randomUUID(),
      axis,
      ratio: 0.5,
      a: before ? incoming : { tab: target },
      b: before ? { tab: target } : incoming,
    });
    to.active = source;
    if (to.taskId) this.#capture(to);
    if (from.taskId && from !== to && this.groups.includes(from))
      this.#capture(from);
    return true;
  }
  place(source, target, placement) {
    if (
      source === target ||
      !["right", "above"].includes(placement) ||
      !this.group(source) ||
      !this.group(target)
    )
      return false;
    const from = this.group(source),
      to = this.group(target),
      axis = placement === "right" ? "x" : "y",
      before = placement === "above";
    if (from !== to)
      return this.merge(source, target, { whole: false, axis, before });
    this.customize(source);
    const remaining = prune(
      from.tree,
      new Set(leaves(from.tree).filter((id) => id !== source)),
    );
    from.tree = replace(remaining, target, {
      id: crypto.randomUUID(),
      axis,
      ratio: 0.5,
      a: before ? { tab: source } : { tab: target },
      b: before ? { tab: target } : { tab: source },
    });
    from.active = source;
    if (from.taskId) this.#capture(from);
    return true;
  }
  swap(source, target) {
    const group = this.group(source);
    if (!group || source === target || this.group(target) !== group)
      return false;
    this.customize(source);
    // Replace leaves together: divider geometry and group identity stay intact.
    const exchange = (tree) =>
      tree.tab
        ? {
            ...tree,
            tab:
              tree.tab === source
                ? target
                : tree.tab === target
                  ? source
                  : tree.tab,
          }
        : { ...tree, a: exchange(tree.a), b: exchange(tree.b) };
    group.tree = exchange(group.tree);
    if (group.taskId) this.#capture(group);
    return true;
  }
  detach(tab) {
    const group = this.group(tab);
    if (!this.canDetach(tab)) return false;
    if (group.guests) group.guests = group.guests.filter((id) => id !== tab);
    group.tree = prune(
      group.tree,
      new Set(leaves(group.tree).filter((id) => id !== tab)),
    );
    if (group.tree.tab) delete group.decoration;
    if (group.active === tab) group.active = leaves(group.tree)[0];
    this.groups.splice(this.groups.indexOf(group) + 1, 0, {
      tree: { tab },
      active: tab,
    });
    if (group.taskId) this.#capture(group);
    return true;
  }
  reorder(source, target, after = false) {
    const from = this.group(source),
      to = this.group(target);
    if (!from || !to || from === to) return false;
    this.groups.splice(this.groups.indexOf(from), 1);
    this.groups.splice(this.groups.indexOf(to) + (after ? 1 : 0), 0, from);
    return true;
  }

  #capture(group) {
    if (
      !group?.taskId ||
      !group.tree ||
      this.taskMembers.get(group.taskId)?.length === 0
    )
      return;
    let tree = stableTree(
      group.tree,
      group.taskId,
      (id) => this.tabTasks.get(id),
      (id) => this.tabAgents.get(id),
    );
    const previous = this.projectLayouts.get(group.taskId);
    if (!previous && !this.taskMembers.has(group.taskId)) return;
    // Legacy task groups without stable agent bindings continue to use the
    // existing auto-layout path; tab IDs alone cannot provide resume identity.
    if (!previous && !stableLeaves(tree).some((leaf) => leaf.agentId)) return;
    const present = new Set(stableLeaves(tree).map(stableKey));
    for (const leaf of stableLeaves(previous?.tree))
      if (!present.has(stableKey(leaf))) tree = appendStable(tree, leaf);
    const activeAgentId =
      this.tabTasks.get(group.active) === group.taskId
        ? this.tabAgents.get(group.active)
        : undefined;
    const layout = {
      taskId: group.taskId,
      tree,
      taskLayout: group.taskLayout === "manual" ? "manual" : "auto",
      ...(activeAgentId
        ? { activeAgentId }
        : group.active
          ? { activeTabId: group.active }
          : {}),
    };
    this.projectLayouts.set(group.taskId, layout);
    const members = this.taskMembers.get(group.taskId);
    if (members) this.#reconcileMembers(layout, members);
  }
  #reconcileMembers(layout, members) {
    const memberSet = new Set(members);
    const guestKeys = stableLeaves(layout.tree)
      .filter((leaf) => leaf.tabId)
      .map(stableKey);
    if (layout.taskLayout === "auto" && !guestKeys.length) {
      const anchor = this.tabAgents.get(
        this.taskOrchestrators.get(layout.taskId),
      );
      const ordered = [...members];
      if (ordered.includes(anchor))
        ordered.unshift(...ordered.splice(ordered.indexOf(anchor), 1));
      const current = stableLeaves(layout.tree)
        .filter((leaf) => leaf.agentId)
        .map((leaf) => leaf.agentId);
      if (
        current.length !== ordered.length ||
        current.some((id, index) => id !== ordered[index])
      )
        layout.tree = stableTaskTree(ordered);
    } else {
      const keep = new Set([
        ...guestKeys,
        ...members.map((id) => `agent:${id}`),
      ]);
      layout.tree = stablePrune(layout.tree, keep);
      const present = new Set(stableLeaves(layout.tree).map(stableKey));
      for (const agentId of members)
        if (!present.has(`agent:${agentId}`))
          layout.tree = appendStable(layout.tree, { agentId });
    }
    if (layout.activeAgentId && !memberSet.has(layout.activeAgentId))
      layout.activeAgentId = members[0];
  }
  #applyProjectLayouts(taskOf, agentOf) {
    for (const group of this.groups) {
      if (!group.taskId || this.taskMembers.get(group.taskId)?.length === 0)
        continue;
      if (
        !this.projectLayouts.has(group.taskId) &&
        !this.taskMembers.has(group.taskId)
      )
        continue;
      if (!this.projectLayouts.has(group.taskId)) this.#capture(group);
      const layout = this.projectLayouts.get(group.taskId);
      if (!layout) continue;
      const members = this.taskMembers.get(group.taskId);
      if (members) this.#reconcileMembers(layout, members);
      const byKey = new Map();
      for (const id of leaves(group.tree)) {
        const agentId = taskOf(id) === group.taskId && agentOf(id);
        byKey.set(agentId ? `agent:${agentId}` : `tab:${id}`, id);
      }
      const keep = new Set(byKey.keys());
      const projected = stablePrune(layout.tree, keep);
      const materialize = (tree) => {
        if (tree.agentId || tree.tabId)
          return { tab: byKey.get(stableKey(tree)) };
        return {
          id: tree.id,
          axis: tree.axis,
          ratio: tree.ratio,
          a: materialize(tree.a),
          b: materialize(tree.b),
        };
      };
      let tree = projected && materialize(projected);
      const represented = new Set(stableLeaves(layout.tree).map(stableKey));
      for (const id of leaves(group.tree)) {
        const agentId = taskOf(id) === group.taskId && agentOf(id);
        const key = agentId ? `agent:${agentId}` : `tab:${id}`;
        if (!represented.has(key))
          tree = tree ? split("x", tree, { tab: id }) : { tab: id };
      }
      if (!tree) continue;
      group.tree = tree;
      group.taskLayout = layout.taskLayout;
      const preferred = layout.activeAgentId
        ? byKey.get(`agent:${layout.activeAgentId}`)
        : byKey.get(`tab:${layout.activeTabId}`);
      const ids = leaves(tree);
      group.active =
        preferred || (ids.includes(group.active) ? group.active : ids[0]);
      group.guests = (group.guests || []).filter((id) => ids.includes(id));
    }
  }
}

// Keep every pane usable; small displays scroll rather than shrinking text to nothing.
export function tileLayout(tree, width, height) {
  const rotate = !tree.tab && width < height && tree.axis === "x";
  const compact = width < 540;
  const axis = (n) =>
    compact ? "y" : rotate ? (n.axis === "x" ? "y" : "x") : n.axis;
  const min = (n) => {
    if (n.tab) return { width: 180, height: 120 };
    const a = min(n.a),
      b = min(n.b);
    return axis(n) === "x"
      ? { width: a.width + b.width + 8, height: Math.max(a.height, b.height) }
      : { width: Math.max(a.width, b.width), height: a.height + b.height + 8 };
  };
  const minimum = min(tree);
  width = Math.max(width, minimum.width);
  height = Math.max(height, minimum.height);
  const panes = [],
    dividers = [];
  function walk(n, box) {
    if (n.tab) {
      panes.push({ id: n.tab, ...box });
      return;
    }
    const horizontal = axis(n) === "x",
      dimension = horizontal ? "width" : "height";
    const span = box[dimension] - 8,
      low = min(n.a)[dimension],
      high = span - min(n.b)[dimension];
    const extent = Math.max(low, Math.min(high, span * n.ratio));
    const a = { ...box, [dimension]: extent };
    const b = {
      ...box,
      [dimension]: span - extent,
      [horizontal ? "x" : "y"]: box[horizontal ? "x" : "y"] + extent + 8,
    };
    dividers.push({
      node: n,
      axis: horizontal ? "x" : "y",
      box,
      span,
      low,
      high,
      ratio: extent / span,
      x: horizontal ? box.x + extent : box.x,
      y: horizontal ? box.y : box.y + extent,
      width: horizontal ? 8 : box.width,
      height: horizontal ? box.height : 8,
    });
    walk(n.a, a);
    walk(n.b, b);
  }
  walk(tree, { x: 0, y: 0, width, height });
  return { panes, dividers, width, height };
}
