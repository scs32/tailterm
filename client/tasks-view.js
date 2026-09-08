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
    loading = false,
    reloadAgain = false,
    hasData = false,
    visible = false,
    generation = 0;
  let clock = null;
  function mount(container) {
    root = container;
  }
  async function show() {
    if (!root) return;
    visible = true;
    hasData = false;
    const token = generation + 1;
    clearInterval(clock);
    clock = setInterval(() => {
      if (visible && hasData) render();
    }, 30000);
    if (!client()) {
      root.innerHTML = `<div class="mode-empty"><span class="eyebrow">TASKS</span><h2>Connect a task hub.</h2><button id="tasks-configure" class="primary">Configure task hub</button></div>`;
      root.querySelector("#tasks-configure").onclick = configure;
      return;
    }
    await reload();
    if (visible && token === generation && !subscription)
      subscription = client().subscribe(
        "",
        async () => {
          await reload();
        },
        { after: 0, onError: () => {} },
      );
  }
  function hide() {
    visible = false;
    generation++;
    loading = false;
    clearInterval(clock);
    subscription?.stop();
    subscription = null;
  }
  async function reload() {
    if (!visible) return;
    if (loading) {
      reloadAgain = true;
      return;
    }
    reloadAgain = false;
    const token = ++generation;
    loading = true;
    try {
      const tasks = await client().listTasks();
      const next = await Promise.all(
        tasks.map((t) =>
          t.status === "open" || t.cleanupPending > 0
            ? client().getTask(t.id)
            : Promise.resolve({ task: t, agents: [] }),
        ),
      );
      if (!visible || token !== generation) return;
      details = next;
      hasData = true;
    } catch (error) {
      if (!visible || token !== generation) return;
      hasData = false;
      root.innerHTML = `<div class="mode-empty"><span class="eyebrow">TASKS</span><h2>Hub unavailable.</h2><p class="launcher-intro">${esc(error.message)}</p><button id="tasks-retry">Retry</button></div>`;
      root.querySelector("#tasks-retry").onclick = () => show();
      loading = false;
      return;
    }
    if (!visible || token !== generation) return;
    loading = false;
    render();
    if (reloadAgain) {
      reloadAgain = false;
      void reload();
    }
  }
  function boundTab(taskId) {
    const group = taskHub.groupOf(taskId);
    return group ? getTabs().find((t) => t.id === group.active) : null;
  }
  const agentState = (a) => {
    const label =
      {
        done: "Turn complete",
        retired: "Retired",
        needs_input:
          {
            permission: "Permission blocked",
            authentication: "Login required",
            tool: "Tool unavailable",
          }[a.blockedReason] || "Needs you",
        exited: "Exited",
      }[a.status] || a.status;
    const seen = Date.parse(a.lastSeenAt);
    const last = seen > 0 ? seen : Date.parse(a.createdAt);
    return a.runId &&
      !["exited", "closed", "retired"].includes(a.status) &&
      last > 0 &&
      Date.now() - last > 90000
      ? label + " · offline"
      : label;
  };
  function cleanupNotice(result, name) {
    notice(
      result.cleanupPending
        ? `Task ${name} closed · ${result.cleanupPending} session${result.cleanupPending === 1 ? "" : "s"} pending cleanup.${result.cleanupErrors?.length ? " " + result.cleanupErrors[0] : " Hosts will retry automatically."}`
        : `Task ${name} closed · agent sessions stopped.`,
    );
  }
  function render() {
    if (!visible) return;
    if (!root) return;
    const open = details.filter((d) => d.task.status === "open");
    const needs = open.flatMap((d) =>
      d.agents
        .filter((a) => a.status === "needs_input")
        .map((a) => ({ task: d.task, agent: a })),
    );
    const closed = details.filter((d) => d.task.status !== "open");
    const card = ({ task, agents }) => {
      const tab = boundTab(task.id);
      return `<article class="task-card" data-task-card="${esc(task.id)}"><header><h3>${esc(task.name)}</h3><span class="fine">${esc(taskRollup(agents))}</span></header>${task.goal ? `<p class="task-goal">${esc(task.goal)}</p>` : ""}<div class="task-agents">${agents
        .filter((a) => a.status !== "closed")
        .map(
          (a) =>
            `<button class="board-agent" data-task-agent="${esc(a.id)}" title="${esc(a.blockedText || a.host + " · " + a.session)}"><span class="status-dot ${STATUS_DOT[a.status] || ""}"></span><span>${esc(a.name)}</span><span class="fine">${esc(agentState(a))} · ${esc(a.host)}</span></button>`,
        )
        .join(
          "",
        )}</div><footer class="task-actions"><span class="fine">${task.allowAgentSpawn ? "Helpers allowed" : "Helpers off"}</span><button data-task-board="${esc(task.id)}">Open board →</button><button data-task-add="${esc(task.id)}">＋ Add agent</button><div class="task-more"><button type="button" data-task-more="${esc(task.id)}" aria-expanded="false" aria-controls="task-menu-${esc(task.id)}">More</button><div id="task-menu-${esc(task.id)}" class="task-menu" popover="auto" aria-label="More task actions"><button data-task-attach="${esc(task.id)}">Open terminals</button><button data-task-settings="${esc(task.id)}">Settings</button><button data-task-close="${esc(task.id)}" class="danger">Close task</button></div></div></footer></article>`;
    };
    root.innerHTML = `<div class="tasks-view"><div class="tasks-head"><div><h2>Tasks <span class="count-badge">${open.length}</span></h2></div><button id="tasks-new" class="primary">＋ New task</button></div>${needs.length ? `<section class="needs-you"><div class="view-heading"><h3>Needs you</h3><span class="count-badge">${needs.length}</span></div>${needs.map(({ task, agent }) => `<button class="attention-row" data-task-agent="${esc(agent.id)}"><strong>${esc(agent.name)}</strong><span>${esc(task.name)} · ${esc(agent.host)}</span><span>Open →</span></button>`).join("")}</section>` : ""}${
      open.length
        ? `<div class="task-grid">${open.map(card).join("")}</div>`
        : ""
    }${
      closed.length
        ? `<details class="dialog-details tasks-closed"><summary>${closed.length} closed task${closed.length === 1 ? "" : "s"}</summary>${closed
            .map(
              (d) =>
                `<div class="task-closed"><span>${esc(d.task.name)}</span><span class="fine" title="${esc(
                  d.agents
                    .filter((a) => a.cleanupError)
                    .map((a) => `${a.name}: ${a.cleanupError}`)
                    .join("; "),
                )}">${d.task.cleanupPending ? `${d.task.cleanupPending} session${d.task.cleanupPending === 1 ? "" : "s"} pending` : "Sessions closed"}</span><button data-task-board="${esc(d.task.id)}">View history</button>${d.task.cleanupPending ? `<button data-task-cleanup="${esc(d.task.id)}">Retry cleanup</button>` : ""}</div>`,
            )
            .join("")}</details>`
        : ""
    }</div>`;
    root.querySelectorAll("[data-task-more]").forEach((button) => {
      const menu = root.querySelector(`#task-menu-${button.dataset.taskMore}`);
      menu.addEventListener("toggle", () =>
        button.setAttribute(
          "aria-expanded",
          String(menu.matches(":popover-open")),
        ),
      );
      button.onclick = () => {
        if (menu.matches(":popover-open")) {
          menu.hidePopover();
          return;
        }
        menu.showPopover();
        const anchor = button.getBoundingClientRect(),
          bounds = menu.getBoundingClientRect();
        menu.style.left = `${Math.max(8, Math.min(anchor.right - bounds.width, innerWidth - bounds.width - 8))}px`;
        menu.style.top = `${Math.max(8, anchor.bottom + bounds.height + 4 <= innerHeight ? anchor.bottom + 4 : anchor.top - bounds.height - 4)}px`;
      };
      menu.addEventListener("click", (event) => {
        if (event.target.closest("button")) menu.hidePopover();
      });
    });
    root.querySelector("#tasks-new").onclick = () => taskHub.newTask();
    const emptyNew = root.querySelector("#tasks-empty-new");
    if (emptyNew) emptyNew.onclick = () => taskHub.newTask();
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
    root
      .querySelectorAll("[data-task-settings]")
      .forEach(
        (b) => (b.onclick = () => taskHub.settings(b.dataset.taskSettings)),
      );
    root.querySelectorAll("[data-task-close]").forEach(
      (b) =>
        (b.onclick = async () => {
          const d = details.find((x) => x.task.id === b.dataset.taskClose);
          if (
            !(await confirm(
              `Close task ${d.task.name}?`,
              "Stop all of this task’s agent tmux sessions, including retired agents and running work. Task history stays saved. Offline hosts will finish cleanup when their host service reconnects.",
            ))
          )
            return;
          try {
            b.disabled = true;
            const result = await taskHub.closeTask(d.task.id);
            cleanupNotice(result, d.task.name);
            await reload();
          } catch (error) {
            notice("Close failed: " + error.message);
            b.disabled = false;
          }
        }),
    );
    root.querySelectorAll("[data-task-cleanup]").forEach((b) => {
      b.onclick = async () => {
        b.disabled = true;
        try {
          const result = await taskHub.cleanupTask(b.dataset.taskCleanup);
          cleanupNotice(result, result.name);
          await reload();
        } catch (error) {
          notice("Cleanup pending: " + error.message);
          b.disabled = false;
        }
      };
    });
    root.querySelectorAll("[data-task-agent]").forEach((b) => {
      b.onclick = () => {
        const tab = getTabs().find(
          (t) => !t.disposed && t.task?.agentId === b.dataset.taskAgent,
        );
        if (tab) activate(tab.id);
        else {
          taskHub.revealAgent(b.dataset.taskAgent);
          notice("Agent pane requested. Attach the task to a tab if needed.");
        }
      };
    });
  }
  return { mount, show, hide, reload };
}
