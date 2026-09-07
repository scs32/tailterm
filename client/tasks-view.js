// Tasks mode: start and manage tasks. Lists open and closed tasks with their
// agents, and offers attach, board, add agent, and close actions.
import { taskRollup } from "./tasks.js";

const esc = (s) =>
  String(s ?? "").replace(
    /[&<>"']/g,
    (c) =>
      ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[
        c
      ],
  );
const STATUS_DOT = {
  running: "online",
  needs_input: "attention",
  done: "done",
};

export function createTasksView({
  client,
  taskHub,
  getTabs,
  activate,
  notice,
  confirm,
  openBoard,
  configure,
}) {
  let root = null,
    details = [],
    subscription = null,
    loading = false;
  function mount(container) {
    root = container;
  }
  async function show() {
    if (!root) return;
    if (!client()) {
      root.innerHTML = `<div class="mode-empty"><span class="eyebrow">TASKS</span><h2>Connect a task hub.</h2><p class="launcher-intro">Tasks bind a team of agent sessions to a terminal tab. They live on a hub that runs as its own Tailscale node.</p><button id="tasks-configure" class="primary">Configure task hub</button></div>`;
      root.querySelector("#tasks-configure").onclick = configure;
      return;
    }
    await reload();
    if (!subscription)
      subscription = client().subscribe(
        "",
        async () => {
          await reload();
        },
        { after: 0, onError: () => {} },
      );
  }
  function hide() {
    subscription?.stop();
    subscription = null;
  }
  async function reload() {
    if (loading) return;
    loading = true;
    try {
      const tasks = await client().listTasks();
      details = await Promise.all(
        tasks.map((t) =>
          t.status === "open"
            ? client().getTask(t.id)
            : Promise.resolve({ task: t, agents: [] }),
        ),
      );
    } catch (error) {
      root.innerHTML = `<div class="mode-empty"><span class="eyebrow">TASKS</span><h2>Hub unavailable.</h2><p class="launcher-intro">${esc(error.message)}</p><button id="tasks-retry">Retry</button></div>`;
      root.querySelector("#tasks-retry").onclick = () => show();
      loading = false;
      return;
    }
    loading = false;
    render();
  }
  function boundTab(taskId) {
    const group = taskHub.groupOf(taskId);
    return group ? getTabs().find((t) => t.id === group.active) : null;
  }
  function render() {
    if (!root) return;
    const open = details.filter((d) => d.task.status === "open");
    const closed = details.filter((d) => d.task.status !== "open");
    const card = ({ task, agents }) => {
      const tab = boundTab(task.id);
      return `<article class="task-card" data-task-card="${esc(task.id)}"><header><h3>${esc(task.name)}</h3><span class="fine">${esc(taskRollup(agents))}</span></header>${task.goal ? `<p class="task-goal">${esc(task.goal)}</p>` : ""}<div class="task-agents">${
        agents
          .filter((a) => a.status !== "closed")
          .map(
            (a) =>
              `<button class="board-agent" data-task-agent="${esc(a.id)}" title="${esc(a.host)} · ${esc(a.session)}"><span class="status-dot ${STATUS_DOT[a.status] || ""}"></span><span>${esc(a.name)}</span><span class="fine">${esc(a.runtime || "")} · ${esc(a.host)}</span></button>`,
          )
          .join("") || '<span class="fine">No agents yet.</span>'
      }</div><footer class="task-actions"><span class="fine">${tab ? `Mirrored in tab · ${esc(tab.session || tab.server.name)}` : "Not attached to a tab"}</span><button data-task-board="${esc(task.id)}">Board</button><button data-task-add="${esc(task.id)}">＋ Agent</button><button data-task-attach="${esc(task.id)}">${tab ? "Attach elsewhere" : "Attach to current tab"}</button><button data-task-close="${esc(task.id)}" class="danger">Close task</button></footer></article>`;
    };
    root.innerHTML = `<div class="tasks-view"><div class="tasks-head"><div><span class="eyebrow">TASKS</span><h2>Teams of agents, one tab each.</h2><p class="launcher-intro">A task binds agent sessions to a terminal tab. Every agent on the task can message the others; the tab gains and loses panes as agents come and go.</p></div><button id="tasks-new" class="primary">＋ New task</button></div>${
      open.length
        ? `<div class="task-grid">${open.map(card).join("")}</div>`
        : `<p class="fine">No open tasks. Start one to spawn the first agent.</p>`
    }${
      closed.length
        ? `<details class="dialog-details tasks-closed"><summary>${closed.length} closed task${closed.length === 1 ? "" : "s"}</summary>${closed
            .map(
              (d) =>
                `<div class="task-closed"><span>${esc(d.task.name)}</span><span class="fine">${esc(d.task.closedAt ? new Date(d.task.closedAt).toLocaleString() : "")}</span></div>`,
            )
            .join("")}</details>`
        : ""
    }</div>`;
    root.querySelector("#tasks-new").onclick = () => taskHub.newTask();
    root
      .querySelectorAll("[data-task-board]")
      .forEach((b) => (b.onclick = () => openBoard(b.dataset.taskBoard)));
    root
      .querySelectorAll("[data-task-add]")
      .forEach((b) => (b.onclick = () => taskHub.addAgent(b.dataset.taskAdd)));
    root
      .querySelectorAll("[data-task-attach]")
      .forEach(
        (b) =>
          (b.onclick = () => taskHub.attachToCurrent(b.dataset.taskAttach)),
      );
    root.querySelectorAll("[data-task-close]").forEach(
      (b) =>
        (b.onclick = async () => {
          const d = details.find((x) => x.task.id === b.dataset.taskClose);
          if (
            !(await confirm(
              `Close task ${d.task.name}?`,
              "Every agent on the task is marked closed and its panes leave the tab. Remote tmux sessions keep running until their hosts stop them.",
            ))
          )
            return;
          try {
            await client().closeTask(d.task.id);
            notice(`Task ${d.task.name} closed.`);
            await reload();
          } catch (error) {
            notice("Close failed: " + error.message);
          }
        }),
    );
    root.querySelectorAll("[data-task-agent]").forEach((b) => {
      b.onclick = () => {
        const tab = getTabs().find(
          (t) => !t.disposed && t.task?.agentId === b.dataset.taskAgent,
        );
        if (tab) activate(tab.id);
        else notice("That agent has no open pane in this browser yet.");
      };
    });
  }
  return { mount, show, hide, reload };
}
