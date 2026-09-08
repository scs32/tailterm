// Projects mode: start and manage tasks. Lists open and closed projects with their
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
  openWorkItems,
  configure,
}) {
  let root = null,
    details = [],
    subscription = null,
    loading = false,
    reloadAgain = false,
    hasData = false,
    visible = false,
    generation = 0,
    selected = "",
    hiddenTaskIds = new Set();
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
      root.innerHTML = `<div class="mode-empty"><span class="eyebrow">PROJECTS</span><h2>Connect a project hub.</h2><button id="tasks-configure" class="primary">Configure project hub</button></div>`;
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
    hiddenTaskIds = new Set(details.map((d) => d.task.id));
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
      const added = !hasData
          ? [...next]
              .reverse()
              .find(
                (d) =>
                  d.task.status === "open" && !hiddenTaskIds.has(d.task.id),
              )
          : null;
      details = next;
      if (added) selected = added.task.id;
      hasData = true;
    } catch (error) {
      if (!visible || token !== generation) return;
      hasData = false;
      root.innerHTML = `<div class="mode-empty"><span class="eyebrow">PROJECTS</span><h2>Hub unavailable.</h2><p class="launcher-intro">${esc(error.message)}</p><button id="tasks-retry">Retry</button></div>`;
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
        ? `Project ${name} closed · ${result.cleanupPending} session${result.cleanupPending === 1 ? "" : "s"} pending cleanup.${result.cleanupErrors?.length ? " " + result.cleanupErrors[0] : " Hosts will retry automatically."}`
        : `Project ${name} closed · agent sessions stopped.`,
    );
  }
  function render() {
    if (!visible) return;
    if (!root) return;
    const open = details.filter((d) => d.task.status === "open");
    const closed = details.filter((d) => d.task.status !== "open");
    let current = details.find((d) => d.task.id === selected);
    if (!current) current = open[0] || closed[0] || null;
    selected = current?.task.id || "";
    const projectButton = ({ task, agents }) =>
      `<button type="button" data-board-task="${esc(task.id)}" data-task-select="${esc(task.id)}" aria-pressed="${task.id === selected}" title="${esc(task.name)}"><span class="board-task-name">${esc(task.name)}</span><span class="fine">${esc(taskRollup(agents))}</span></button>`;
    const detail = (entry) => {
      if (!entry)
        return `<div class="board-head"><div class="view-heading"><div><span class="eyebrow">PROJECTS</span><h2>Projects</h2></div></div><p class="fine">No projects yet.</p></div>`;
      const { task, agents } = entry;
      if (task.status !== "open")
        return `<div class="board-head"><div class="view-heading"><div><span class="eyebrow">PROJECT</span><h2>${esc(task.name)}</h2></div><div class="view-actions"><span class="fine hub-sync-status" role="status">${esc(client()?.cacheStatus?.().label || "")}</span><button data-task-board="${esc(task.id)}">View history</button>${task.cleanupPending ? `<button data-task-cleanup="${esc(task.id)}">Retry cleanup</button>` : ""}</div></div><p class="fine">Closed · ${task.cleanupPending ? `${task.cleanupPending} session${task.cleanupPending === 1 ? "" : "s"} pending cleanup` : "Sessions closed"}</p></div><article class="task-card task-detail-card" data-task-card="${esc(task.id)}"><header><h3>${esc(task.name)}</h3></header>${task.goal ? `<p class="task-goal">${esc(task.goal)}</p>` : ""}</article>`;
      const needs = agents.filter((a) => a.status === "needs_input");
      const handler = agents.find((a) => a.role === "database_handler");
      return `<div class="board-head"><div class="view-heading"><div><span class="eyebrow">PROJECT</span><h2>${esc(task.name)}</h2></div><div class="view-actions"><span class="fine hub-sync-status" role="status">${esc(client()?.cacheStatus?.().label || "")}</span></div></div><p class="fine">${esc(task.goal || taskRollup(agents))}</p></div>${needs.length ? `<section class="needs-you"><div class="view-heading"><h3>Needs you</h3><span class="count-badge">${needs.length}</span></div>${needs.map((agent) => `<button class="attention-row" data-task-agent="${esc(agent.id)}"><strong>${esc(agent.name)}</strong><span>${esc(agent.host)}</span><span>Open →</span></button>`).join("")}</section>` : ""}<article class="task-card task-detail-card" data-task-card="${esc(task.id)}"><header><h3>Agents · ${esc(task.name)}</h3><span class="fine">${esc(taskRollup(agents))}</span></header><div class="task-agents">${agents
        .filter((a) => a.status !== "closed")
        .map(
          (a) =>
            `<button class="board-agent" data-task-agent="${esc(a.id)}" title="${esc(a.blockedText || a.host + " · " + a.session)}"><span class="status-dot ${STATUS_DOT[a.status] || ""}"></span><span>${esc(a.name)}${a.role === "database_handler" ? " · Database handler" : ""}</span><span class="fine">${esc(agentState(a))} · ${esc(a.host)}</span></button>`,
        )
        .join(
          "",
        )}</div><footer class="task-actions"><span class="fine">${task.allowAgentSpawn ? "Helpers allowed" : "Helpers off"}</span><button data-task-board="${esc(task.id)}">Open board →</button><button data-task-add="${esc(task.id)}">＋ Add agent</button>${!handler || handler.status === "exited" || (!handler.online && !["retired", "closed"].includes(handler.status)) ? `<button data-handler-setup="${esc(task.id)}">${handler ? (handler.status === "exited" ? "Restart" : "Check") : "Set up"} database handler</button>` : ""}${openWorkItems ? `<button data-project-bugs="${esc(task.id)}">Bugs</button><button data-project-features="${esc(task.id)}">Features</button>` : ""}<div class="task-more"><button type="button" data-task-more="${esc(task.id)}" aria-expanded="false" aria-controls="task-menu-${esc(task.id)}">More</button><div id="task-menu-${esc(task.id)}" class="task-menu" popover="auto" aria-label="More project actions"><button data-task-attach="${esc(task.id)}">Open terminals</button><button data-task-settings="${esc(task.id)}">Settings</button><button data-task-close="${esc(task.id)}" class="danger">Close project</button></div></div></footer></article>`;
    };
    root.innerHTML = `<div class="board mode-board tasks-view"><aside class="board-rail"><div class="board-rail-head"><span class="eyebrow">PROJECTS</span><button id="tasks-new" title="New project" aria-label="New project">＋</button></div>${open.map(projectButton).join("")}${closed.length ? `<details class="board-closed tasks-closed" ${current?.task.status !== "open" ? "open" : ""}><summary>Closed · ${closed.length}</summary>${closed.map(projectButton).join("")}</details>` : ""}</aside><section class="board-thread tasks-detail">${detail(current)}</section></div>`;
    root.querySelectorAll("[data-task-select]").forEach(
      (button) =>
        (button.onclick = () => {
          selected = button.dataset.taskSelect;
          render();
        }),
    );
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
    root
      .querySelectorAll("[data-handler-setup]")
      .forEach(
        (b) => (b.onclick = () => taskHub.setupHandler(b.dataset.handlerSetup)),
      );
    root
      .querySelectorAll("[data-project-bugs]")
      .forEach(
        (b) =>
          (b.onclick = () => openWorkItems?.("bugs", b.dataset.projectBugs)),
      );
    root
      .querySelectorAll("[data-project-features]")
      .forEach(
        (b) =>
          (b.onclick = () =>
            openWorkItems?.("features", b.dataset.projectFeatures)),
      );
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
              `Close project ${d.task.name}?`,
              "Stop all of this project’s agent tmux sessions, including retired agents and running work. Project history stays saved. Offline hosts will finish cleanup when their host service reconnects.",
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
          notice(
            "Agent pane requested. Attach the project to a tab if needed.",
          );
        }
      };
    });
  }
  return { mount, show, hide, reload };
}
