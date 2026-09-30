// Pure rules for capping terminal groups. A group that would exceed the limit
// continues in linked "(continued)" groups that share its series.
export const PANE_LIMITS = [4, 6, 8, 10, 12, 16];
export const DEFAULT_PANE_LIMIT = 8;
export const normalizePaneLimit = (value) =>
  PANE_LIMITS.includes(Number(value)) ? Number(value) : DEFAULT_PANE_LIMIT;

// Rank 0 is the original group; rank 1 "(continued)", rank k "(continued k)".
export const continuedSuffix = (rank) =>
  !rank ? "" : rank === 1 ? " (continued)" : ` (continued ${rank})`;

// Project groups form one series per task; plain groups only once they overflow.
export const seriesKey = (group) =>
  group?.taskId
    ? `task:${group.taskId}`
    : group?.series
      ? `series:${group.series}`
      : null;

// The original (no part) first, then continuations in part order.
export function seriesParts(groups, group) {
  const key = seriesKey(group);
  if (!key) return group && groups.includes(group) ? [group] : [];
  return groups
    .filter((g) => seriesKey(g) === key)
    .map((g, index) => ({ g, index }))
    .sort((a, b) => (a.g.part || 0) - (b.g.part || 0) || a.index - b.index)
    .map(({ g }) => g);
}

const node = (axis, a, b, ratio = 0.5) => ({
  id: crypto.randomUUID(),
  axis,
  ratio,
  a,
  b,
});
const column = (ids) =>
  ids.length === 1
    ? { tab: ids[0] }
    : node("y", { tab: ids[0] }, column(ids.slice(1)), 1 / ids.length);
// A continuation has no orchestrator: just the two worker columns.
export function continuationTree(ids) {
  if (ids.length === 1) return { tab: ids[0] };
  return node(
    "x",
    column(ids.filter((_, i) => i % 2 === 0)),
    column(ids.filter((_, i) => i % 2 === 1)),
  );
}

// Where an arriving pane goes: its recorded part if that has room, else the
// earliest part with room, else a new part (-1). sizes are live pane counts.
export function arrivalPart(sizes, limit, preferred = -1) {
  if (preferred >= 0 && (sizes[preferred] ?? 0) < limit) return preferred;
  const index = sizes.findIndex((size) => size < limit);
  return index;
}

// Trailing items of an overfull list move, in order, to the front of the next.
export function cascade(lists, limit) {
  const result = lists.map((list) => [...list]);
  for (let i = 0; i < result.length; i++)
    if (result[i].length > limit) {
      const overflow = result[i].splice(limit);
      if (i + 1 === result.length) result.push([]);
      result[i + 1].unshift(...overflow);
    }
  return result;
}

// Decides which template part holds each key. Live keys follow their live
// part; absent keys keep their part unless it would exceed the limit, when the
// last absent keys move to the front of the next part (fresh keys move last).
// Removed keys disappear; added keys join the earliest part with live room.
// Returns the parts plus the template part index of every live part.
export function planTemplate({
  template,
  live,
  limit,
  removed = new Set(),
  added = [],
}) {
  const parts = template.map((keys) => keys.filter((key) => !removed.has(key)));
  live = live.map((keys) => keys.filter((key) => !removed.has(key)));
  const liveSet = new Set(live.flat());
  const partOf = new Map();
  parts.forEach((keys, index) => keys.forEach((key) => partOf.set(key, index)));
  const liveIndex = [];
  for (const keys of live) {
    if (!keys.length) {
      liveIndex.push(-1);
      continue;
    }
    const counts = new Map();
    for (const key of keys)
      if (partOf.has(key))
        counts.set(partOf.get(key), (counts.get(partOf.get(key)) || 0) + 1);
    const best = [...counts]
      .filter(([index]) => !liveIndex.includes(index))
      .sort((a, b) => b[1] - a[1] || a[0] - b[0])[0];
    if (best) liveIndex.push(best[0]);
    else {
      // A live part without template members sits after the previous one.
      const at = Math.max(-1, ...liveIndex) + 1;
      parts.splice(at, 0, []);
      for (let i = 0; i < liveIndex.length; i++)
        if (liveIndex[i] >= at) liveIndex[i]++;
      liveIndex.push(at);
    }
  }
  live.forEach((keys, i) => {
    const target = parts[liveIndex[i]];
    if (!target) return;
    for (const key of keys) {
      if (target.includes(key)) continue;
      for (const other of parts) {
        const at = other.indexOf(key);
        if (at >= 0) other.splice(at, 1);
      }
      target.push(key);
    }
  });
  const fresh = new Set();
  const present = new Set(parts.flat());
  for (const key of added) {
    if (present.has(key)) continue;
    const liveCount = (keys) => keys.filter((k) => liveSet.has(k)).length;
    let index = parts.findIndex((keys) => liveCount(keys) < limit);
    if (index < 0) {
      parts.push([]);
      index = parts.length - 1;
    }
    parts[index].push(key);
    present.add(key);
    fresh.add(key);
  }
  for (let i = 0; i < parts.length; i++) {
    let excess = parts[i].length - limit;
    if (excess <= 0) continue;
    const absent = parts[i].filter((key) => !liveSet.has(key));
    const order = [
      ...absent.filter((key) => fresh.has(key)),
      ...absent.filter((key) => !fresh.has(key)),
    ];
    const evict = new Set();
    for (let j = order.length - 1; j >= 0 && excess > 0; j--, excess--)
      evict.add(order[j]);
    const moving = parts[i].filter((key) => evict.has(key));
    parts[i] = parts[i].filter((key) => !evict.has(key));
    if (i + 1 === parts.length) parts.push([]);
    parts[i + 1].unshift(...moving);
  }
  const keep = parts.map((keys) => keys.length > 0);
  const shift = keep.map((_, i) => keep.slice(0, i).filter((k) => !k).length);
  return {
    parts: parts.filter((keys) => keys.length > 0),
    liveIndex: liveIndex.map((index) =>
      index < 0 ? -1 : index - shift[index],
    ),
  };
}

// Moves every group of one series, in order, before or after another series.
export function reorderSeries(groups, moving, anchor, after = false) {
  if (
    !moving.length ||
    !anchor.length ||
    moving.some((g) => anchor.includes(g))
  )
    return null;
  const rest = groups.filter((g) => !moving.includes(g));
  const at = after
    ? rest.indexOf(anchor[anchor.length - 1]) + 1
    : rest.indexOf(anchor[0]);
  rest.splice(at, 0, ...moving);
  return rest;
}
