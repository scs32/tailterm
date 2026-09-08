import { leaves } from "./pane-layout.js";
import { normalizeTabDecoration } from "./tab-decoration.js";
import { AGENT_ID_RE, normalizeTaskRef, normalizeTaskId } from "./task-ref.js";
const MAX_PROJECT_LAYOUTS = 30;
const MAX_PROJECT_MEMBERS = 32;
export const normalizeSessionFontSize = (value) =>
  Number.isInteger(value) && value >= 10 && value <= 32 ? value : undefined;
export const endpointKey = (server) =>
  JSON.stringify([
    server.host,
    server.port,
    server.username,
    server.tmuxPath || "",
  ]);
export const sameTarget = (a, b) =>
  !!a && !!b && a.id === b.id && String(a.created) === String(b.created);

function projectLayoutsFromGroups(tabs, groups) {
  const byId = new Map(tabs.map((tab) => [tab.id, tab]));
  const convert = (tree, taskId) => {
    if (tree.tab) {
      const tab = byId.get(tree.tab),
        agentId = tab?.task?.taskId === taskId && tab.task.agentId;
      return agentId ? { agentId } : { tabId: tree.tab };
    }
    return {
      id: tree.id,
      axis: tree.axis,
      ratio: tree.ratio,
      a: convert(tree.a, taskId),
      b: convert(tree.b, taskId),
    };
  };
  return groups
    .filter((group) => normalizeTaskId(group.taskId) && group.tree)
    .map((group) => {
      const active = byId.get(group.active),
        activeAgentId =
          active?.task?.taskId === group.taskId
            ? active.task.agentId
            : undefined;
      return {
        taskId: group.taskId,
        tree: convert(group.tree, group.taskId),
        taskLayout: group.taskLayout === "manual" ? "manual" : "auto",
        ...(activeAgentId
          ? { activeAgentId }
          : group.active
            ? { activeTabId: group.active }
            : {}),
      };
    });
}

export function normalizeProjectLayouts(value) {
  if (!Array.isArray(value)) return [];
  const tasks = new Set();
  return value
    .slice(0, MAX_PROJECT_LAYOUTS)
    .map((layout) => {
      const taskId = normalizeTaskId(layout?.taskId);
      if (!taskId || tasks.has(taskId)) return null;
      const used = new Set();
      let members = 0;
      const tree = (node, depth = 0) => {
        if (!node || depth > 30 || members >= MAX_PROJECT_MEMBERS) return null;
        if (AGENT_ID_RE.test(node.agentId || "")) {
          const key = `agent:${node.agentId}`;
          if (used.has(key)) return null;
          used.add(key);
          members++;
          return { agentId: node.agentId };
        }
        if (
          typeof node.tabId === "string" &&
          node.tabId.length > 0 &&
          node.tabId.length <= 64
        ) {
          const key = `tab:${node.tabId}`;
          if (used.has(key)) return null;
          used.add(key);
          members++;
          return { tabId: node.tabId };
        }
        const ratio = node.ratio === undefined ? 0.5 : node.ratio;
        if (!Number.isFinite(ratio) || ratio <= 0 || ratio >= 1) return null;
        const a = tree(node.a, depth + 1),
          b = tree(node.b, depth + 1);
        if (!a || !b) return null;
        return {
          id:
            typeof node.id === "string" && node.id.length <= 64
              ? node.id
              : crypto.randomUUID(),
          axis: node.axis === "y" ? "y" : "x",
          ratio,
          a,
          b,
        };
      };
      const normalizedTree = tree(layout.tree);
      if (!normalizedTree) return null;
      tasks.add(taskId);
      const activeAgentId = AGENT_ID_RE.test(layout.activeAgentId || "")
          ? layout.activeAgentId
          : undefined,
        activeTabId =
          typeof layout.activeTabId === "string" &&
          layout.activeTabId.length <= 64
            ? layout.activeTabId
            : undefined;
      return {
        taskId,
        tree: normalizedTree,
        taskLayout: layout.taskLayout === "manual" ? "manual" : "auto",
        ...(activeAgentId && used.has(`agent:${activeAgentId}`)
          ? { activeAgentId }
          : activeTabId && used.has(`tab:${activeTabId}`)
            ? { activeTabId }
            : {}),
      };
    })
    .filter(Boolean);
}

export function workspaceSnapshot(
  tabs,
  groups,
  active,
  serverFilter = null,
  tasks = [],
  hiddenAgents = [],
  projectLayouts,
) {
  return {
    hiddenAgents: hiddenAgents
      .filter((id) => /^agt_[0-9a-f]{16}$/.test(id))
      .slice(0, 400),
    tasks: [...new Set(tasks.map(normalizeTaskId).filter(Boolean))].slice(
      0,
      30,
    ),
    tabs: tabs
      .filter((t) => !t.disposed && t.wasConnected !== false)
      .map((t) => ({
        id: t.id,
        serverId: t.server.id,
        endpoint: endpointKey(t.server),
        tmux: t.tmux,
        session: t.session,
        target: t.target,
        decoration: normalizeTabDecoration(t.decoration),
        fontSize: normalizeSessionFontSize(t.fontSize),
        task: normalizeTaskRef(t.task) || undefined,
      })),
    serverFilter: serverFilter === null ? null : [...serverFilter],
    groups: structuredClone(groups),
    projectLayouts: normalizeProjectLayouts(
      projectLayouts || projectLayoutsFromGroups(tabs, groups),
    ),
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
      decoration: normalizeTabDecoration(t.decoration),
      fontSize: normalizeSessionFontSize(t.fontSize),
      session: t.tmux ? t.session : "",
      task: normalizeTaskRef(t.task) || undefined,
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
    const ratio = node.ratio === undefined ? 0.5 : node.ratio;
    if (!Number.isFinite(ratio) || ratio <= 0 || ratio >= 1) return null;
    const a = tree(node.a, depth + 1),
      b = tree(node.b, depth + 1);
    if (!a || !b) return a || b;
    return {
      id: typeof node.id === "string" ? node.id : crypto.randomUUID(),
      axis: node.axis === "y" ? "y" : "x",
      ratio,
      a,
      b,
    };
  }
  const groups = (Array.isArray(value.groups) ? value.groups : [])
    .slice(0, 30)
    .map((g) => ({
      tree: tree(g?.tree),
      active: ids.has(g?.active) ? g.active : null,
      decoration: normalizeTabDecoration(g?.decoration),
      taskId: normalizeTaskId(g?.taskId),
      taskLayout: ["auto", "manual"].includes(g?.taskLayout)
        ? g.taskLayout
        : undefined,
      guests: Array.isArray(g?.guests)
        ? g.guests.filter((id) => ids.has(id)).slice(0, 30)
        : [],
    }))
    .filter((g) => g.tree)
    .map((g) => ({
      ...g,
      guests: g.taskId
        ? g.guests.filter(
            (id) =>
              leaves(g.tree).includes(id) &&
              !tabs.find((t) => t.id === id)?.task,
          )
        : [],
    }));
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
    projectLayouts: normalizeProjectLayouts(
      value.projectLayouts || projectLayoutsFromGroups(tabs, groups),
    ),
    tasks: [
      ...new Set(
        (Array.isArray(value.tasks) ? value.tasks : [])
          .map(normalizeTaskId)
          .filter(Boolean),
      ),
    ].slice(0, 30),
    hiddenAgents: (Array.isArray(value.hiddenAgents) ? value.hiddenAgents : [])
      .filter((id) => /^agt_[0-9a-f]{16}$/.test(id))
      .slice(0, 400),
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
