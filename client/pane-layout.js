import {
  DEFAULT_PANE_LIMIT,
  arrivalPart,
  cascade,
  continuationTree,
  planTemplate,
  reorderSeries,
  seriesKey,
  seriesParts,
} from "./pane-cap.js";
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

const stableKeys = (tree) => stableLeaves(tree).map(stableKey);
const stableLeaf = (key) =>
  key.startsWith("agent:")
    ? { agentId: key.slice(6) }
    : { tabId: key.slice(4) };
const templateTrees = (layout) =>
  layout ? [layout.tree, ...(layout.continued || [])].filter(Boolean) : [];
// Keeps base's shape for the keys it holds; other keys join before or after.
function shapeKeys(base, keys) {
  let tree = base ? stablePrune(base, new Set(keys)) : null;
  const held = new Set(stableKeys(tree));
  const first = keys.findIndex((key) => held.has(key));
  const front = keys.filter(
    (key, i) => !held.has(key) && (first < 0 || i < first),
  );
  for (const key of keys.filter((key, i) => !held.has(key) && i > first))
    if (first >= 0) tree = appendStable(tree, stableLeaf(key));
  for (const key of front.reverse())
    tree = tree ? split("x", stableLeaf(key), tree) : stableLeaf(key);
  return tree;
}
const stableContinuation = (agentIds) => {
  const convert = (tree) =>
    tree.tab
      ? { agentId: tree.tab }
      : { ...tree, a: convert(tree.a), b: convert(tree.b) };
  return convert(continuationTree(agentIds));
};
// New panes share the width with the panes already there.
const prependTabs = (tree, ids) =>
  !tree
    ? continuationTree(ids)
    : split(
        "x",
        continuationTree(ids),
        tree,
        ids.length / (ids.length + leaves(tree).length),
      );
const appendTab = (tree, id) =>
  split("x", tree, { tab: id }, 1 - 1 / (leaves(tree).length + 1));
const appendBelow = (tree, id) =>
  split("y", tree, { tab: id }, 1 - 1 / (leaves(tree).length + 1));

// The home area's default share of the body width.
export const HOME_RATIO = 0.4;
export const clampHomeRatio = (value) =>
  Number.isFinite(value) ? Math.min(0.8, Math.max(0.2, value)) : HOME_RATIO;

export class PaneGroups {
  groups = [];
  // The pinned home area: {tree, active, ratio}, or null when empty. Its panes
  // are never in groups, so every group operation refuses them.
  home = null;
  boundTabs = new Set();
  tabOrder = [];
  limit = DEFAULT_PANE_LIMIT;
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
    if (layout) this.#reconcileMembers(layout);
  }
  rememberActive(tab) {
    if (this.inHome(tab)) {
      this.home.active = tab;
      return;
    }
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
  // A project layout is one arrangement across every part of the project.
  customize(tab) {
    const group = this.group(tab);
    if (group?.taskId)
      for (const part of this.series(group)) part.taskLayout = "manual";
  }
  group(tab) {
    return this.groups.find((g) => leaves(g.tree).includes(tab));
  }
  // The original group first, then its "(continued)" parts.
  series(group) {
    return group ? seriesParts(this.groups, group) : [];
  }
  // taskOf(tabId) supplies the task a newly grouped tab belongs to, so a
  // singleton group created for an agent pane inherits its task binding.
  // homeOf(tabId) says whether a tab belongs in the home area; home tabs are
  // kept out of every group before the group rules run.
  sync(
    ids,
    taskOf = () => undefined,
    agentOf = () => undefined,
    homeOf = () => false,
  ) {
    this.boundTabs = new Set(ids.filter((id) => agentOf(id)));
    const homeIds = ids.filter((id) => homeOf(id));
    this.#syncHome(homeIds);
    ids = ids.filter((id) => !homeIds.includes(id));
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
      const next = {
        ...g,
        decoration: tree.tab ? undefined : g.decoration,
        tree,
        active: leaves(tree).includes(g.active) ? g.active : leaves(tree)[0],
      };
      this.#leaveSeries(next, leaves(g.tree).length);
      return [next];
    });
    for (const id of ids)
      if (!this.group(id)) {
        const group = { tree: { tab: id }, active: id };
        const taskId = taskOf(id);
        if (taskId && !this.groups.some((g) => g.taskId === taskId))
          group.taskId = taskId;
        this.groups.push(group);
      }
    this.#normalizeSeries();
  }
  // Split legacy mixed groups and gather each task's panes into its parts.
  isolateTasks(taskOf, agentOf = (id) => this.tabAgents.get(id)) {
    this.tabAgents = new Map(this.tabOrder.map((id) => [id, agentOf(id)]));
    this.tabTasks = new Map(this.tabOrder.map((id) => [id, taskOf(id)]));
    const pieces = [];
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
          if (group.taskId) delete part.part;
          pieces.push({ part });
          continue;
        }
        part.taskId = taskId;
        delete part.series;
        part.guests = leaves(tree).filter((id) => !taskOf(id));
        const ordered = this.tabOrder.filter((id) => leaves(tree).includes(id));
        const originalOrder = leaves(tree).every((id, i) => id === ordered[i]);
        part.taskLayout =
          group.taskLayout ||
          (!part.guests.length && originalOrder && legacyTaskTree(tree)
            ? "auto"
            : "manual");
        // Panes already in one of the task's groups stay in that part.
        const existing = group.taskId === taskId;
        if (!existing) delete part.part;
        pieces.push({ part, taskId, existing });
      }
    }
    const seen = new Set(pieces.filter((p) => p.existing).map((p) => p.taskId));
    for (const piece of pieces)
      if (piece.taskId && !seen.has(piece.taskId)) {
        piece.existing = true;
        seen.add(piece.taskId);
      }
    const result = [],
      parts = new Map(),
      arrivals = new Map();
    for (const { part, taskId, existing } of pieces) {
      if (!taskId) {
        result.push(part);
        continue;
      }
      if (!existing) {
        if (!arrivals.has(taskId)) arrivals.set(taskId, []);
        arrivals.get(taskId).push(part);
        continue;
      }
      const key = `${taskId}#${part.part || 0}`;
      const target = parts.get(key);
      if (target) {
        if (part.taskLayout === "manual") target.taskLayout = "manual";
        target.guests = [...(target.guests || []), ...part.guests];
        target.tree = {
          id: crypto.randomUUID(),
          axis: "x",
          ratio: 0.5,
          a: target.tree,
          b: part.tree,
        };
      } else {
        parts.set(key, part);
        result.push(part);
      }
    }
    this.groups = result;
    for (const [taskId, incoming] of arrivals) this.#arrive(taskId, incoming);
    this.#pinOrchestrators();
    this.#normalizeSeries();
    for (const group of this.groups) {
      if (!group.taskId || group.taskLayout !== "auto" || group.guests?.length)
        continue;
      const ids = this.#memberOrder(group);
      const tree = group.part ? continuationTree(ids) : taskTree(ids);
      // Stable membership preserves divider IDs/ratios and focused terminals.
      if (!sameShape(group.tree, tree)) group.tree = tree;
    }
    this.#applyProjectLayouts(taskOf, agentOf);
  }
  taskGroup(taskId) {
    return (
      this.groups.find((g) => g.taskId === taskId && !g.part) ||
      this.groups.find((g) => g.taskId === taskId)
    );
  }
  canMerge(source, target, whole = true) {
    const from = this.group(source),
      to = this.group(target);
    if (!from || !to || from === to) return false;
    if (whole) return !from.taskId && !to.taskId;
    // Project agents may move between the parts of their own project.
    if (from.taskId && from.taskId === to.taskId) return true;
    return !from.taskId || !!from.guests?.includes(source);
  }
  // Whether the target group has room for the incoming pane or group.
  canFit(source, target, whole = true) {
    const from = this.group(source),
      to = this.group(target);
    if (!from || !to) return false;
    if (from === to) return true;
    return (
      leaves(to.tree).length + (whole ? leaves(from.tree).length : 1) <=
      this.limit
    );
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
    if (
      !this.canMerge(source, target, whole) ||
      !this.canFit(source, target, whole)
    )
      return false;
    if (to.taskId && this.#guestIn(source, from, to)) {
      to.guests = [...(to.guests || []), source];
      to.taskLayout = "manual";
    }
    if (from.guests) from.guests = from.guests.filter((id) => id !== source);
    const count = leaves(from.tree).length;
    const incoming = whole ? from.tree : { tab: source };
    if (whole) this.groups = this.groups.filter((g) => g !== from);
    else this.#remove(from, source);
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
    if (!whole) this.#leaveSeries(from, count);
    this.#normalizeSeries();
    if (to.taskId) this.#capture(to);
    if (from.taskId && from.taskId !== to.taskId && this.groups.includes(from))
      this.#capture(from);
    return true;
  }
  // Puts a pane (or a whole plain group) into the earliest part of the
  // target's series with room, starting a "(continued)" part when none has.
  send(source, target, whole = false) {
    const from = this.group(source),
      to = this.group(target);
    if (!this.canMerge(source, target, whole)) return false;
    const count = whole ? leaves(from.tree).length : 1;
    const dest = this.series(to).find(
      (part) => part !== from && leaves(part.tree).length + count <= this.limit,
    );
    if (dest)
      return this.merge(source, leaves(dest.tree).at(-1), { whole, axis: "x" });
    const guest = this.#guestIn(source, from, to);
    const before = leaves(from.tree).length;
    const incoming = whole ? from.tree : { tab: source };
    if (from.guests) from.guests = from.guests.filter((id) => id !== source);
    if (whole) this.groups = this.groups.filter((g) => g !== from);
    else this.#remove(from, source);
    this.#continue(to, incoming, source, guest ? [source] : []);
    if (!whole) this.#leaveSeries(from, before);
    this.#normalizeSeries();
    if (to.taskId) this.#capture(to);
    if (from.taskId && from.taskId !== to.taskId && this.groups.includes(from))
      this.#capture(from);
    return true;
  }
  // The dragged pane takes the target pane's place; the target pane moves to
  // the earliest other part of its series with room, or a new continuation.
  makeRoom(source, target) {
    const from = this.group(source),
      to = this.group(target);
    if (!this.canMerge(source, target, false)) return false;
    const guest = this.#guestIn(source, from, to),
      targetGuest = !!to.guests?.includes(target);
    const before = leaves(from.tree).length;
    if (from.guests) from.guests = from.guests.filter((id) => id !== source);
    this.#remove(from, source);
    to.tree = replace(to.tree, target, { tab: source });
    to.guests = (to.guests || []).filter((id) => id !== target);
    if (to.taskId && guest) {
      to.guests.push(source);
      to.taskLayout = "manual";
    }
    if (!to.taskId) delete to.guests;
    to.active = source;
    const dest = this.series(to).find(
      (part) => part !== to && leaves(part.tree).length < this.limit,
    );
    if (dest) {
      dest.tree = appendTab(dest.tree, target);
      if (dest.taskId && targetGuest)
        dest.guests = [...(dest.guests || []), target];
    } else
      this.#continue(to, { tab: target }, target, targetGuest ? [target] : []);
    this.#leaveSeries(from, before);
    this.#normalizeSeries();
    if (to.taskId) this.#capture(to);
    if (from.taskId && from.taskId !== to.taskId && this.groups.includes(from))
      this.#capture(from);
    return true;
  }
  place(source, target, placement) {
    if (
      source === target ||
      !["right", "left", "above", "below"].includes(placement) ||
      !this.group(source) ||
      !this.group(target)
    )
      return false;
    const from = this.group(source),
      to = this.group(target),
      axis = placement === "right" || placement === "left" ? "x" : "y",
      before = placement === "above" || placement === "left";
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
    const count = leaves(group.tree).length;
    if (group.guests) group.guests = group.guests.filter((id) => id !== tab);
    group.tree = prune(
      group.tree,
      new Set(leaves(group.tree).filter((id) => id !== tab)),
    );
    if (group.tree.tab) delete group.decoration;
    if (group.active === tab) group.active = leaves(group.tree)[0];
    // The separated tab follows the whole series, never splits it.
    const last = this.series(group).at(-1);
    this.groups.splice(this.groups.indexOf(last) + 1, 0, {
      tree: { tab },
      active: tab,
    });
    this.#leaveSeries(group, count);
    this.#normalizeSeries();
    if (group.taskId) this.#capture(group);
    return true;
  }
  // A series moves as one unit and keeps its parts in order.
  reorder(source, target, after = false) {
    const from = this.group(source),
      to = this.group(target);
    if (!from || !to || from === to) return false;
    const next = reorderSeries(
      this.groups,
      this.series(from),
      this.series(to),
      after,
    );
    if (!next) return false;
    this.groups = next;
    return true;
  }

  inHome(tab) {
    return !!this.home && leaves(this.home.tree).includes(tab);
  }
  // Only an unbound terminal may move into home; agent panes never do.
  canHome(tab) {
    return !!this.group(tab) && !this.boundTabs.has(tab);
  }
  // Moves a plain terminal from its group into home: next to target when a
  // placement is given, otherwise at the bottom.
  toHome(tab, target, placement) {
    if (!this.canHome(tab)) return false;
    const group = this.group(tab),
      count = leaves(group.tree).length;
    if (group.guests) group.guests = group.guests.filter((id) => id !== tab);
    this.#remove(group, tab);
    if (group.tree) {
      if (group.tree.tab) delete group.decoration;
      this.#leaveSeries(group, count);
    }
    this.#normalizeSeries();
    if (group.taskId && this.groups.includes(group)) this.#capture(group);
    if (!this.home)
      this.home = { tree: { tab }, active: tab, ratio: HOME_RATIO };
    else if (
      this.inHome(target) &&
      ["right", "left", "above", "below"].includes(placement)
    )
      this.home.tree = replace(
        this.home.tree,
        target,
        this.#placed(tab, target, placement),
      );
    else this.home.tree = appendBelow(this.home.tree, tab);
    this.home.active = tab;
    return true;
  }
  // A plain home terminal becomes its own tab; the helper never leaves.
  leaveHome(tab) {
    if (!this.inHome(tab) || this.boundTabs.has(tab)) return false;
    const tree = prune(
      this.home.tree,
      new Set(leaves(this.home.tree).filter((id) => id !== tab)),
    );
    if (!tree) this.home = null;
    else {
      this.home.tree = tree;
      if (this.home.active === tab) this.home.active = leaves(tree)[0];
    }
    this.groups.push({ tree: { tab }, active: tab });
    return true;
  }
  homeSwap(source, target) {
    if (source === target || !this.inHome(source) || !this.inHome(target))
      return false;
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
    this.home.tree = exchange(this.home.tree);
    return true;
  }
  homePlace(source, target, placement) {
    if (
      source === target ||
      !this.inHome(source) ||
      !this.inHome(target) ||
      !["right", "left", "above", "below"].includes(placement)
    )
      return false;
    const remaining = prune(
      this.home.tree,
      new Set(leaves(this.home.tree).filter((id) => id !== source)),
    );
    this.home.tree = replace(
      remaining,
      target,
      this.#placed(source, target, placement),
    );
    this.home.active = source;
    return true;
  }
  #placed(source, target, placement) {
    const before = placement === "above" || placement === "left";
    return {
      id: crypto.randomUUID(),
      axis: placement === "right" || placement === "left" ? "x" : "y",
      ratio: 0.5,
      a: before ? { tab: source } : { tab: target },
      b: before ? { tab: target } : { tab: source },
    };
  }
  // Home keeps its arrangement for the tabs still there and adds new ones at
  // the bottom, each row sharing the height.
  #syncHome(ids) {
    if (!ids.length) {
      this.home = null;
      return;
    }
    let tree = this.home?.tree ? prune(this.home.tree, new Set(ids)) : null;
    for (const id of ids)
      if (!tree || !leaves(tree).includes(id))
        tree = tree ? appendBelow(tree, id) : { tab: id };
    const present = leaves(tree);
    this.home = {
      tree,
      active: present.includes(this.home?.active)
        ? this.home.active
        : present[0],
      ratio: clampHomeRatio(this.home?.ratio),
    };
  }

  #guestIn(source, from, to) {
    return !(
      from.taskId &&
      from.taskId === to.taskId &&
      !from.guests?.includes(source)
    );
  }
  #remove(group, tab) {
    group.tree = prune(
      group.tree,
      new Set(leaves(group.tree).filter((id) => id !== tab)),
    );
    if (!group.tree) this.groups = this.groups.filter((g) => g !== group);
    else if (group.active === tab) group.active = leaves(group.tree)[0];
  }
  // Appends a new continuation part holding tree to the series of group.
  #continue(group, tree, active, guests = []) {
    const parts = this.series(group);
    if (!group.taskId && !group.series) group.series = crypto.randomUUID();
    const part = { tree, active, part: parts.length };
    if (group.taskId) {
      part.taskId = group.taskId;
      part.taskLayout = guests.length ? "manual" : group.taskLayout || "auto";
      part.guests = guests;
    } else part.series = group.series;
    this.groups.splice(this.groups.indexOf(parts.at(-1)) + 1, 0, part);
    return part;
  }
  // A plain part that drops to one pane becomes an ordinary tab again.
  #leaveSeries(group, before) {
    if (
      group?.tree &&
      !group.taskId &&
      group.series &&
      before > 1 &&
      leaves(group.tree).length === 1
    ) {
      delete group.series;
      delete group.part;
    }
  }
  // Auto project parts order panes by arrival with the orchestrator first.
  #memberOrder(group) {
    const ids = leaves(group.tree);
    if (!group.taskId || group.taskLayout !== "auto" || group.guests?.length)
      return ids;
    const ordered = [
      ...this.tabOrder.filter((id) => ids.includes(id)),
      ...ids.filter((id) => !this.tabOrder.includes(id)),
    ];
    const anchor = this.taskOrchestrators.get(group.taskId);
    if (!group.part && ordered.includes(anchor))
      ordered.unshift(...ordered.splice(ordered.indexOf(anchor), 1));
    return ordered;
  }
  #liveKey(taskId, id) {
    const agentId = this.tabTasks.get(id) === taskId && this.tabAgents.get(id);
    return agentId ? `agent:${agentId}` : `tab:${id}`;
  }
  // The template part holding most of a live part's panes.
  #templateIndex(group, template) {
    const counts = new Map();
    for (const id of leaves(group.tree)) {
      const index = template.findIndex((keys) =>
        keys.includes(this.#liveKey(group.taskId, id)),
      );
      if (index >= 0) counts.set(index, (counts.get(index) || 0) + 1);
    }
    return [...counts].sort((a, b) => b[1] - a[1] || a[0] - b[0])[0]?.[0] ?? -1;
  }
  // New panes go to their recorded part if it has room, else the earliest
  // part with room, else a new part. Existing panes never move.
  #arrive(taskId, pieces) {
    const template = templateTrees(this.projectLayouts.get(taskId)).map(
      stableKeys,
    );
    if (pieces.some((piece) => piece.taskLayout === "manual"))
      for (const part of this.series(this.taskGroup(taskId)))
        part.taskLayout = "manual";
    const position = (id) =>
      this.tabOrder.includes(id) ? this.tabOrder.indexOf(id) : Infinity;
    const ids = pieces
      .flatMap((piece) => leaves(piece.tree))
      .sort((a, b) => position(a) - position(b));
    for (const id of ids) {
      const original = this.taskGroup(taskId);
      let parts = this.series(original);
      const recorded = template.findIndex((keys) =>
        keys.includes(this.#liveKey(taskId, id)),
      );
      let preferred = -1;
      if (recorded >= 0) {
        const indexes = parts.map((part) =>
          this.#templateIndex(part, template),
        );
        preferred = indexes.indexOf(recorded);
        if (preferred < 0) {
          // Restore the recorded part in its template position.
          const at = indexes.findIndex((index) => index > recorded);
          const part = {
            tree: { tab: id },
            active: id,
            taskId,
            taskLayout: original.taskLayout || "auto",
            guests: [],
          };
          const anchor = at < 0 ? parts.at(-1) : parts[at];
          this.groups.splice(
            this.groups.indexOf(anchor) + (at < 0 ? 1 : 0),
            0,
            part,
          );
          parts.splice(at < 0 ? parts.length : at, 0, part);
          this.#number(parts);
          continue;
        }
      }
      const index = arrivalPart(
        parts.map((part) => leaves(part.tree).length),
        this.limit,
        preferred,
      );
      if (index < 0) this.#continue(original, { tab: id }, id);
      else parts[index].tree = appendTab(parts[index].tree, id);
    }
  }
  #number(parts) {
    parts.forEach((part, index) => {
      if (index) part.part = index;
      else delete part.part;
    });
  }
  // In auto layout the orchestrator always sits in the project's first part.
  #pinOrchestrators() {
    for (const [taskId, anchor] of this.taskOrchestrators) {
      const original = this.taskGroup(taskId),
        group = this.group(anchor);
      if (
        !original ||
        !group ||
        group === original ||
        group.taskId !== taskId ||
        original.taskLayout === "manual" ||
        group.guests?.includes(anchor)
      )
        continue;
      this.#remove(group, anchor);
      original.tree = prependTabs(original.tree, [anchor]);
    }
  }
  // Keeps every series within the limit, numbered, adjacent and non-trivial.
  #normalizeSeries() {
    for (const group of this.groups) {
      if (group.taskId) delete group.series;
      else if (!group.series && leaves(group.tree).length > this.limit)
        group.series = crypto.randomUUID();
    }
    const done = new Set();
    for (const group of [...this.groups]) {
      const key = seriesKey(group);
      if (!key) {
        delete group.part;
        continue;
      }
      if (done.has(key)) continue;
      done.add(key);
      let parts = this.series(group);
      const lists = parts.map((part) => this.#memberOrder(part));
      if (lists.some((list) => list.length > this.limit)) {
        const next = cascade(lists, this.limit);
        next.forEach((ids, i) => {
          let part = parts[i];
          const incoming = ids.filter((id) => !lists[i]?.includes(id));
          const guests = incoming.filter((id) =>
            parts.some((p) => p.guests?.includes(id)),
          );
          for (const p of parts)
            if (p.guests)
              p.guests = p.guests.filter((id) => !incoming.includes(id));
          if (!part) {
            part = this.#continue(
              parts[0],
              continuationTree(ids),
              ids[0],
              guests,
            );
            parts = this.series(group);
            return;
          }
          const keep = new Set(ids.filter((id) => lists[i].includes(id)));
          part.tree = prune(part.tree, keep);
          if (incoming.length) part.tree = prependTabs(part.tree, incoming);
          if (!leaves(part.tree).includes(part.active))
            part.active = leaves(part.tree)[0];
          if (part.taskId && guests.length)
            part.guests = [...(part.guests || []), ...guests];
        });
        parts = this.series(group);
      }
      if (!group.taskId && parts.length === 1) {
        delete parts[0].series;
        delete parts[0].part;
        continue;
      }
      this.#number(parts);
      const first = this.groups.findIndex((g) => parts.includes(g));
      this.groups = [
        ...this.groups.slice(0, first),
        ...parts,
        ...this.groups.slice(first).filter((g) => !parts.includes(g)),
      ];
    }
  }

  #capture(group) {
    if (
      !group?.taskId ||
      !group.tree ||
      this.taskMembers.get(group.taskId)?.length === 0
    )
      return;
    const taskId = group.taskId;
    const previous = this.projectLayouts.get(taskId);
    if (!previous && !this.taskMembers.has(taskId)) return;
    // Legacy task groups without stable agent bindings continue to use the
    // existing auto-layout path; tab IDs alone cannot provide resume identity.
    if (
      !previous &&
      !this.series(this.taskGroup(taskId)).some((part) =>
        leaves(part.tree).some(
          (id) => this.tabTasks.get(id) === taskId && this.tabAgents.get(id),
        ),
      )
    )
      return;
    const activeAgentId =
      this.tabTasks.get(group.active) === taskId
        ? this.tabAgents.get(group.active)
        : undefined;
    const layout = {
      taskId,
      tree: previous?.tree,
      ...(previous?.continued ? { continued: previous.continued } : {}),
      taskLayout: group.taskLayout === "manual" ? "manual" : "auto",
      ...(activeAgentId
        ? { activeAgentId }
        : group.active
          ? { activeTabId: group.active }
          : {}),
    };
    this.#reconcileMembers(layout, true);
    if (layout.tree) this.projectLayouts.set(taskId, layout);
  }
  // Brings the template in line with the roster and the live parts: live
  // panes are recorded in their live part, absent members keep theirs within
  // the limit, removed members go and new members join the earliest part with
  // room. fromLive takes each live part's shape; otherwise the template's.
  #reconcileMembers(layout, fromLive = false) {
    const taskId = layout.taskId;
    const members = this.taskMembers.get(taskId);
    const parts = this.series(this.taskGroup(taskId));
    const previous = templateTrees(layout);
    const removed = new Set(),
      memberKeys = (members || []).map((id) => `agent:${id}`);
    if (members) {
      const memberSet = new Set(memberKeys);
      for (const key of previous.flatMap(stableKeys))
        if (key.startsWith("agent:") && !memberSet.has(key)) removed.add(key);
      for (const part of parts)
        for (const id of leaves(part.tree)) {
          const key = this.#liveKey(taskId, id);
          if (key.startsWith("agent:") && !memberSet.has(key)) removed.add(key);
        }
    }
    const plan = planTemplate({
      template: previous.map(stableKeys),
      live: parts.map((part) =>
        leaves(part.tree).map((id) => this.#liveKey(taskId, id)),
      ),
      limit: this.limit,
      removed,
      added: memberKeys,
    });
    let trees = plan.parts.map((keys, i) => {
      const live = plan.liveIndex.indexOf(i);
      let base = null;
      if (fromLive && live >= 0)
        base = stableTree(
          parts[live].tree,
          taskId,
          (id) => this.tabTasks.get(id),
          (id) => this.tabAgents.get(id),
        );
      else {
        let overlap = 0;
        for (const tree of previous) {
          const count = stableKeys(tree).filter((key) =>
            keys.includes(key),
          ).length;
          if (count > overlap) [overlap, base] = [count, tree];
        }
      }
      return shapeKeys(base, keys);
    });
    const guests = plan.parts.flat().some((key) => key.startsWith("tab:"));
    // Without a roster the auto order is unknown; keep the recorded shapes.
    if (members && layout.taskLayout === "auto" && !guests) {
      const anchor = this.tabAgents.get(this.taskOrchestrators.get(taskId));
      const rank = (key) => {
        const index = memberKeys.indexOf(key);
        return index < 0 ? Infinity : index;
      };
      trees = trees.map((tree, i) => {
        const ordered = [...plan.parts[i]]
          .sort((a, b) => rank(a) - rank(b))
          .map((key) => key.slice(6));
        if (i === 0 && ordered.includes(anchor))
          ordered.unshift(...ordered.splice(ordered.indexOf(anchor), 1));
        const next =
          i === 0 ? stableTaskTree(ordered) : stableContinuation(ordered);
        const same = (a, b) =>
          a.length === b.length && a.every((key, j) => key === b[j]);
        return same(stableKeys(tree), stableKeys(next)) ? tree : next;
      });
    }
    layout.tree = trees[0] || null;
    if (trees.length > 1) layout.continued = trees.slice(1);
    else delete layout.continued;
    if (
      members?.length &&
      layout.activeAgentId &&
      !members.includes(layout.activeAgentId)
    )
      layout.activeAgentId = members[0];
    return plan;
  }
  #applyProjectLayouts(taskOf, agentOf) {
    const tasks = [
      ...new Set(this.groups.map((g) => g.taskId).filter(Boolean)),
    ];
    for (const taskId of tasks) {
      if (this.taskMembers.get(taskId)?.length === 0) continue;
      if (!this.projectLayouts.has(taskId) && !this.taskMembers.has(taskId))
        continue;
      if (!this.projectLayouts.has(taskId))
        this.#capture(this.taskGroup(taskId));
      const layout = this.projectLayouts.get(taskId);
      if (!layout) continue;
      const plan = this.#reconcileMembers(layout);
      if (!layout.tree) {
        this.projectLayouts.delete(taskId);
        continue;
      }
      const trees = templateTrees(layout);
      this.series(this.taskGroup(taskId)).forEach((group, index) => {
        const template = trees[plan.liveIndex[index]];
        if (!template) return;
        const byKey = new Map();
        for (const id of leaves(group.tree)) {
          const agentId = taskOf(id) === taskId && agentOf(id);
          byKey.set(agentId ? `agent:${agentId}` : `tab:${id}`, id);
        }
        const projected =
          template && stablePrune(template, new Set(byKey.keys()));
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
        const represented = new Set(template ? stableKeys(template) : []);
        for (const [key, id] of byKey)
          if (!represented.has(key))
            tree = tree ? split("x", tree, { tab: id }) : { tab: id };
        if (!tree) return;
        group.tree = tree;
        group.taskLayout = layout.taskLayout;
        const preferred = layout.activeAgentId
          ? byKey.get(`agent:${layout.activeAgentId}`)
          : byKey.get(`tab:${layout.activeTabId}`);
        const ids = leaves(tree);
        group.active =
          preferred || (ids.includes(group.active) ? group.active : ids[0]);
        group.guests = (group.guests || []).filter((id) => ids.includes(id));
      });
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
