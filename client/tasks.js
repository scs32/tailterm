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

// A pane bound to an earlier run of this agent belongs to a session the hub
// has replaced. The restore preflight and the mirror must agree on this.
export const runChanged = (ref, a) => !!a.runId && ref?.runId !== a.runId;

// An open project whose team is present. A missing pauseState (older hubs)
// counts as active; resuming projects are coming back and stay bound.
export const liveProject = (task) =>
  task?.status === "open" &&
  [undefined, null, "", "active", "resuming"].includes(task.pauseState);

// The owner's helper session lives in the home area, never in a project group.
export const homeAgent = (a) => a?.role === "owner_helper";

// A project with an open owner helper shows only the helper until the owner
// pulls other agents in (wi_649926f70aaa762a).
export const helperProject = (agents = []) =>
  agents.some((a) => homeAgent(a) && openAgent(a));

// Whether an agent's terminal belongs open. A hidden agent (closed with ×
// in a project without a helper, or a helper the owner closed) stays closed.
// In a helper project every other agent opens only when the owner chose it
// (shown, keyed by agent id, so a new run of a chosen agent opens again).
// shown null or undefined applies no helper-only rule.
export function agentShown(agent, agents, { shown, hidden } = {}) {
  if (hidden?.has(agent?.id)) return false;
  if (!shown || homeAgent(agent) || !helperProject(agents)) return true;
  return shown.has(agent.id);
}

// What the login restore should do with a saved pane bound to a hub agent.
// detail is the hub's getTask result: undefined when the hub is unknown or
// unreachable (restore as before), null when the hub no longer has the task.
// shown, when given, is the owner's choice for helper projects.
//   keep | unknown | gone | closed | paused | run-changed | not-shown
// not-shown skips the reconnect but keeps the bookmark: the agent is alive.
export function restoreVerdict(binding, detail, shown) {
  if (detail === undefined) return "unknown";
  if (detail === null) return "gone";
  if (detail.task?.status !== "open") return "closed";
  if (!liveProject(detail.task)) return "paused";
  const agent = detail.agents?.find((a) => a.id === binding?.agentId);
  if (!agent || !openAgent(agent)) return "closed";
  if (runChanged(binding, agent)) return "run-changed";
  if (!agentShown(agent, detail.agents, { shown })) return "not-shown";
  return "keep";
}

// Finished agents may have left their tmux session; open them only when the
// session still exists.
export const sessionCheckNeeded = (a) => ["done", "exited"].includes(a?.status);

// reconcileTask compares the hub's agent list with the panes that exist.
//   open:     agents that need a pane, with the server to use
//   unknown:  open agents whose host matches no saved server
//   close:    panes bound to this task whose agent is closed or gone
//   adopt:    panes matching an agent by server and session but not yet bound
//   hide:     live panes of agents a helper project does not show (shown);
//             the agent keeps running, so they close without hiding it
// A launcher tab the owner opened on an agent's session is adopted even when
// the agent is not shown: the caller then shows it.
export function reconcileTask({
  taskId,
  agents,
  tabs,
  servers,
  hidden = new Set(),
  shown,
}) {
  const live = tabs.filter((t) => !t.disposed);
  const byAgent = new Map();
  for (const t of live)
    if (
      t.task?.taskId === taskId &&
      t.task.agentId &&
      !agents.some((a) => a.id === t.task.agentId && runChanged(t.task, a))
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
    if (!agentShown(a, agents, { shown })) continue;
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
        agents.some((a) => a.id === t.task.agentId && runChanged(t.task, a))),
  );
  const hide = [...byAgent.values()].filter((t) => {
    const a = agents.find((a) => a.id === t.task.agentId);
    return a && openAgent(a) && !agentShown(a, agents, { shown });
  });
  return { open, unknown, close, adopt, hide };
}

// A normalized binding that keeps the helper role; other roles are dropped.
export function bindingOf(value) {
  const ref = normalizeTaskRef(value);
  return ref && homeAgent(value) ? { ...ref, role: "owner_helper" } : ref;
}

export function taskBinding(taskId, agent) {
  return bindingOf({
    taskId,
    agentId: agent.id,
    agentName: agent.name,
    runId: agent.runId,
    role: agent.role,
  });
}

// The agents that fill a project's panes: the helper never takes a slot, and
// in a helper project only the agents the owner chose (shown) do.
export const taskMemberIds = (task, agents, shown) =>
  task?.status === "closed"
    ? []
    : agents
        .filter(
          (a) =>
            openAgent(a) && !homeAgent(a) && agentShown(a, agents, { shown }),
        )
        .map((a) => a.id);

// The ▾ terminal menu of a project: its open agents other than the helper, in
// one section per work item (roster order, the item lead first), then the
// agents with no work item. titles maps itemId -> title; the id stands in
// until the title is known. Each row says whether the agent is shown.
export function terminalMenuSections(
  agents = [],
  { shown, hidden, titles } = {},
) {
  const sections = new Map();
  const none = [];
  for (const a of agents) {
    if (!openAgent(a) || homeAgent(a)) continue;
    const row = {
      agentId: a.id,
      name: a.name,
      status: a.status,
      lead: !!a.itemLead,
      shown: agentShown(a, agents, { shown, hidden }),
    };
    const itemId = a.workItem?.itemId;
    if (!itemId) {
      none.push(row);
      continue;
    }
    if (!sections.has(itemId))
      sections.set(itemId, {
        itemId,
        itemTaskId: a.workItem.itemTaskId || a.taskId || "",
        title: titles?.get(itemId) || itemId,
        rows: [],
      });
    sections.get(itemId).rows.push(row);
  }
  const out = [...sections.values()].map((section) => ({
    ...section,
    rows: [
      ...section.rows.filter((r) => r.lead),
      ...section.rows.filter((r) => !r.lead),
    ],
  }));
  if (none.length) out.push({ itemId: "", title: "No work item", rows: none });
  return out;
}

// Whether a pane belongs in the home area: only the owner helper's pane does.
//   binding: the pane's task binding, if any
//   role:    the hub role when the roster is known ("" for an ordinary
//            agent); undefined when unknown, which keeps the saved membership
//   saved:   restored membership
// A terminal the owner opens is never in home, whatever was saved.
export function homePlacement({ binding, role = binding?.role, saved } = {}) {
  if (!binding) return false;
  return role === undefined ? !!saved : role === "owner_helper";
}

// The helper window's name, from the hub role rather than the tmux session.
export const helperLabel = (binding, projectName) =>
  binding?.role === "owner_helper"
    ? projectName
      ? `Owner helper - ${projectName}`
      : "Owner helper"
    : "";

// Every task-bound attach ignores size, so it never resizes other clients.
export const attachOptions = (binding) => ({
  ignoreSize: !!binding,
  ...(binding?.role === "owner_helper" &&
  /^tsk_[0-9a-f]{16}$/.test(binding.taskId || "") &&
  /^agt_[0-9a-f]{16}$/.test(binding.agentId || "") &&
  /^run_[0-9a-f]{16}$/.test(binding.runId || "")
    ? {
        helperBinding: {
          taskId: binding.taskId,
          agentId: binding.agentId,
          runId: binding.runId,
          role: "owner_helper",
        },
      }
    : {}),
});
// This records the command's binding, not a claim of remote policy success.
export const helperAttachKey = (binding) => {
  const helper = attachOptions(binding).helperBinding;
  return helper ? `${helper.taskId}/${helper.agentId}/${helper.runId}` : "";
};

// A helper adopted from a plain or legacy ignore-size attach needs the native
// policy for its current identity, whether it is connecting or connected.
export const helperReattach = (tab) =>
  tab?.task?.role === "owner_helper" &&
  !!tab.tmux &&
  (tab.attachIgnoresSize === false ||
    (!!helperAttachKey(tab.task) &&
      tab.attachHelperKey !== helperAttachKey(tab.task)));

// The replace path keeps the tab id, binding and home membership and always
// attaches to the existing session, never creating one.
export const reattachOptions = (tab) => ({
  replace: tab,
  resumeOnly: true,
  target: tab.target,
  task: tab.task,
  home: true,
});

const STATUS_LABEL = {
  starting: "starting",
  running: "running",
  done: "done",
  retired: "retired",
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
  for (const status of [
    "needs_input",
    "running",
    "done",
    "retired",
    "starting",
    "exited",
  ])
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
    if (
      a?.status === "retired" &&
      ["started", "running", "done", "needs_input"].includes(e.kind)
    )
      continue;
    switch (e.kind) {
      case "retired":
      case "resumed":
        if (a) {
          a.status = e.kind === "retired" ? "retired" : "done";
          a.blockedReason = "";
          a.blockedText = "";
        }
        break;
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
