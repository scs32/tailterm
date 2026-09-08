// Pure task logic: mapping hub agents to saved servers, deciding which panes a
// task tab should gain or lose, and summarizing agent status.
import { normalizeTaskRef } from "./task-ref.js";

export const MAX_TASK_PANES = 32;
const shortHost = (value) =>
  String(value || "")
    .trim()
    .toLowerCase()
    .split(".")[0];

// A hub agent reports the tailnet name of its host; find the saved server
// that reaches it. Tailnet servers win over plain SSH profiles; exact host
// matches win over name matches.
export function matchServer(host, servers) {
  const want = shortHost(host);
  if (!want) return null;
  const candidates = servers.filter(
    (s) =>
      shortHost(s.host) === want ||
      s.agentHosts?.some((alias) => shortHost(alias) === want) ||
      String(s.name || "")
        .trim()
        .toLowerCase() === want,
  );
  return (
    candidates.find((s) => s.tailnet && shortHost(s.host) === want) ||
    candidates.find((s) => shortHost(s.host) === want) ||
    candidates[0] ||
    null
  );
}

export const openAgent = (a) => a.status !== "closed";

// reconcileTask compares the hub's agent list with the panes that exist.
//   open:     agents that need a pane, with the server to use
//   unknown:  open agents whose host matches no saved server
//   close:    panes bound to this task whose agent is closed or gone
//   adopt:    panes matching an agent by server and session but not yet bound
export function reconcileTask({
  taskId,
  agents,
  tabs,
  servers,
  hidden = new Set(),
}) {
  const live = tabs.filter((t) => !t.disposed);
  const byAgent = new Map();
  for (const t of live)
    if (
      t.task?.taskId === taskId &&
      t.task.agentId &&
      !agents.some(
        (a) => a.id === t.task.agentId && a.runId && t.task.runId !== a.runId,
      )
    )
      byAgent.set(t.task.agentId, t);
  const open = [],
    unknown = [],
    adopt = [];
  let bound = byAgent.size;
  for (const a of agents) {
    if (
      !openAgent(a) ||
      (a.status === "starting" && a.runId) ||
      hidden.has(a.id)
    )
      continue;
    if (byAgent.has(a.id)) continue;
    const server = matchServer(a.host, servers);
    const existing =
      server &&
      live.find(
        (t) =>
          t.tmux &&
          t.server.id === server.id &&
          t.session === a.session &&
          !t.task,
      );
    if (existing) {
      adopt.push({ tab: existing, agent: a });
      bound++;
      continue;
    }
    if (!server) {
      unknown.push(a);
      continue;
    }
    if (bound + open.length >= MAX_TASK_PANES) continue;
    open.push({ agent: a, server });
  }
  const ids = new Set(agents.filter(openAgent).map((a) => a.id));
  const close = live.filter(
    (t) =>
      t.task?.taskId === taskId &&
      (!ids.has(t.task.agentId) ||
        agents.some(
          (a) => a.id === t.task.agentId && a.runId && t.task.runId !== a.runId,
        )),
  );
  return { open, unknown, close, adopt };
}

export function taskBinding(taskId, agent) {
  return normalizeTaskRef({
    taskId,
    agentId: agent.id,
    agentName: agent.name,
    runId: agent.runId,
  });
}

const STATUS_LABEL = {
  starting: "starting",
  running: "running",
  done: "done",
  needs_input: "needs input",
  exited: "exited",
  closed: "closed",
};

export function taskRollup(agents, unknownHosts = 0) {
  const counts = {};
  let total = 0;
  for (const a of agents) {
    if (!openAgent(a)) continue;
    total++;
    counts[a.status] = (counts[a.status] || 0) + 1;
  }
  const parts = [`${total} agent${total === 1 ? "" : "s"}`];
  for (const status of ["needs_input", "running", "done", "starting", "exited"])
    if (counts[status]) parts.push(`${counts[status]} ${STATUS_LABEL[status]}`);
  if (unknownHosts)
    parts.push(
      `${unknownHosts} on unknown host${unknownHosts === 1 ? "" : "s"}`,
    );
  return parts.join(" · ");
}

// Apply a batch of hub events to a cached agent list. Returns the attention
// labels to raise on panes, keyed by agent id, and whether the agent list
// needs refetching (a new agent appeared).
export function applyEvents(agents, events) {
  const attention = new Map();
  let refresh = false;
  const byId = new Map(agents.map((a) => [a.id, a]));
  for (const e of events) {
    const a = byId.get(e.agentId);
    switch (e.kind) {
      case "agent_added":
        refresh = true;
        break;
      case "started":
      case "running":
        if (a) {
          a.status = "running";
          a.blockedReason = "";
          a.blockedText = "";
        }
        break;
      case "done":
        if (a?.status === "needs_input" && e.data?.runtimeStop) break;
        if (a) {
          a.status = "done";
          a.blockedReason = "";
          a.blockedText = "";
        }
        attention.set(e.agentId, "Command finished");
        break;
      case "needs_input":
        if (a) {
          a.status = "needs_input";
          a.blockedReason =
            e.data?.reason ||
            [
              ["Permission blocked", "permission"],
              ["Login required", "authentication"],
              ["Tool unavailable", "tool"],
            ].find(([prefix]) => e.text?.startsWith(prefix))?.[1] ||
            "";
          a.blockedText = e.text || "";
        }
        attention.set(e.agentId, "Needs attention");
        break;
      case "heartbeat":
        if (a) {
          a.lastSeenAt = e.createdAt;
          a.online = true;
        }
        break;
      case "exited":
        if (a) {
          a.status = "exited";
          a.online = false;
        }
        attention.set(e.agentId, "Agent exited");
        break;
      case "closed":
        if (a) a.status = "closed";
        break;
      case "message":
        if (e.agentId && !e.data?.to) attention.set(e.agentId, "Task message");
        break;
      case "task_closed":
        for (const agent of agents) agent.status = "closed";
        break;
    }
  }
  return { attention, refresh };
}
