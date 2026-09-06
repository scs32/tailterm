export const endpointKey = (server) =>
  JSON.stringify([
    server.host,
    server.port,
    server.username,
    server.tmuxPath || "",
  ]);
export const sameTarget = (a, b) =>
  !!a && !!b && a.id === b.id && String(a.created) === String(b.created);

export function workspaceSnapshot(tabs, groups, active, serverFilter = null) {
  return {
    tabs: tabs
      .filter((t) => !t.disposed && t.wasConnected !== false)
      .map((t) => ({
        id: t.id,
        serverId: t.server.id,
        endpoint: endpointKey(t.server),
        tmux: t.tmux,
        session: t.session,
        target: t.target,
      })),
    serverFilter: serverFilter === null ? null : [...serverFilter],
    groups: structuredClone(groups),
    active,
  };
}

export function normalizeWorkspace(value) {
  if (!value || !Array.isArray(value.tabs)) return null;
  const tabs = value.tabs
    .slice(0, 30)
    .filter(
      (t) =>
        t &&
        typeof t.id === "string" &&
        t.id.length <= 64 &&
        typeof t.serverId === "string" &&
        typeof t.endpoint === "string" &&
        t.endpoint.length < 1024 &&
        (!t.tmux || /^[a-zA-Z0-9_-]{1,64}$/.test(t.session || "")),
    )
    .map((t) => ({
      id: t.id,
      serverId: t.serverId,
      endpoint: t.endpoint,
      tmux: !!t.tmux,
      session: t.tmux ? t.session : "",
      target:
        /^\$\d+$/.test(t.target?.id) && /^\d+$/.test(String(t.target?.created))
          ? { id: t.target.id, created: String(t.target.created) }
          : undefined,
    }));
  const ids = new Set(tabs.map((t) => t.id));
  const used = new Set();
  function tree(node, depth = 0) {
    if (!node || depth > 30) return null;
    if (node.tab) {
      if (!ids.has(node.tab) || used.has(node.tab)) return null;
      used.add(node.tab);
      return { tab: node.tab };
    }
    const a = tree(node.a, depth + 1),
      b = tree(node.b, depth + 1);
    if (!a || !b) return a || b;
    return {
      id: typeof node.id === "string" ? node.id : crypto.randomUUID(),
      axis: node.axis === "y" ? "y" : "x",
      ratio: Number.isFinite(node.ratio)
        ? Math.max(0.1, Math.min(0.9, node.ratio))
        : 0.5,
      a,
      b,
    };
  }
  const groups = (Array.isArray(value.groups) ? value.groups : [])
    .slice(0, 30)
    .map((g) => ({
      tree: tree(g?.tree),
      active: ids.has(g?.active) ? g.active : null,
    }))
    .filter((g) => g.tree);
  return {
    tabs,
    serverFilter: Array.isArray(value.serverFilter)
      ? [
          ...new Set(
            value.serverFilter.filter(
              (id) => typeof id === "string" && id.length <= 80,
            ),
          ),
        ].slice(0, 200)
      : null,
    groups,
    active: ids.has(value.active) ? value.active : tabs[0]?.id,
  };
}

export function reconnectable(error) {
  return (
    !/fingerprint|host key|sign.in|authenticat|password|private key|verification|original tmux session|no longer exists|cancelled|permission denied/i.test(
      String(error),
    ) &&
    /timeout|timed out|network|connection|closed|EOF|broken pipe|unreachable|reset by peer|dial/i.test(
      String(error),
    )
  );
}

export function createReconnectController({
  attempt,
  eligible,
  changed,
  setTimer = setTimeout,
  clearTimer = clearTimeout,
}) {
  const pending = new Map();
  function cancel(id) {
    const item = pending.get(id);
    if (item) clearTimer(item.timer);
    pending.delete(id);
  }
  function schedule(tab, count = 0) {
    cancel(tab.id);
    if (!eligible(tab) || count >= 5) {
      changed(tab, "Reconnect manually");
      return;
    }
    const delay = Math.min(30000, 1000 * 2 ** count);
    changed(tab, `Reconnecting in ${delay / 1000}s`);
    const item = { tab, count };
    item.timer = setTimer(async () => {
      pending.delete(tab.id);
      if (!eligible(tab)) return;
      try {
        await attempt(tab, count + 1);
      } catch (e) {
        if (reconnectable(e.message)) schedule(tab, count + 1);
        else changed(tab, e.message);
      }
    }, delay);
    pending.set(tab.id, item);
  }
  return {
    schedule,
    cancel,
    clear() {
      [...pending.keys()].forEach(cancel);
    },
    wake() {
      for (const { tab, count } of [...pending.values()]) schedule(tab, count);
    },
  };
}
