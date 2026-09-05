export const leaves = (tree) =>
  tree.tab ? [tree.tab] : [...leaves(tree.a), ...leaves(tree.b)];
function prune(tree, ids) {
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
export class PaneGroups {
  groups = [];
  group(tab) {
    return this.groups.find((g) => leaves(g.tree).includes(tab));
  }
  sync(ids) {
    const valid = new Set(ids);
    this.groups = this.groups.flatMap((g) => {
      const tree = prune(g.tree, valid);
      if (!tree) return [];
      return [
        {
          ...g,
          tree,
          active: leaves(tree).includes(g.active) ? g.active : leaves(tree)[0],
        },
      ];
    });
    for (const id of ids)
      if (!this.group(id)) this.groups.push({ tree: { tab: id }, active: id });
  }
  merge(source, target, { whole = true, axis = "x" } = {}) {
    const from = this.group(source),
      to = this.group(target);
    if (!from || !to || from === to) return false;
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
    to.tree = replace(to.tree, target, {
      id: crypto.randomUUID(),
      axis,
      ratio: 0.5,
      a: { tab: target },
      b: incoming,
    });
    to.active = source;
    return true;
  }
  detach(tab) {
    const group = this.group(tab);
    if (!group || leaves(group.tree).length < 2) return false;
    group.tree = prune(
      group.tree,
      new Set(leaves(group.tree).filter((id) => id !== tab)),
    );
    if (group.active === tab) group.active = leaves(group.tree)[0];
    this.groups.splice(this.groups.indexOf(group) + 1, 0, {
      tree: { tab },
      active: tab,
    });
    return true;
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
