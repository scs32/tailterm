// Projects mode: start and manage tasks. Lists open and closed projects with their
// agents, and offers attach, board, add agent, and close actions.
import { taskRollup } from "./tasks.js";
import { pauseStateLabel, projectPauseState } from "./project-pause.js";
import { createViewRefreshPresentation } from "./view-refresh-presentation.js";
import { renderTeamDelivery } from "./team-delivery-view.js";

const cacheFeedback = (label = "") =>
  label === "Saved data" || label === "Saved data · refreshing"
    ? ""
    : label === "Saved data · offline"
      ? "Offline · showing cached data"
      : label;
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
  let capabilities = null;
  let capabilitiesClient = null;
  let clock = null;
  // View-local choices never write to the hub or vault. A changed client or
  // project lifecycle starts a fresh disclosure epoch.
  let teamClient = null;
  const teamChoices = new Map();
  const deliveries = new Map();
  async function loadDelivery(taskId, actionClient = client()) {
    if (!taskId || !actionClient?.listTeamDelivery) return;
    try {
      const queue = await actionClient.listTeamDelivery(taskId);
      if (!visible || client() !== actionClient) return;
      deliveries.set(taskId, queue);
      if (selected === taskId) render();
    } catch { deliveries.delete(taskId); }
  }
  function teamExpanded(task, count) {
    if (teamClient !== client()) {
      teamClient = client();
      teamChoices.clear();
    }
    const epoch = JSON.stringify([
      task.createdAt,
      task.lifecycleGeneration,
      task.pauseGeneration,
    ]);
    let choice = teamChoices.get(task.id);
    if (!choice || choice.epoch !== epoch) {
      choice = { epoch, expanded: null };
      teamChoices.set(task.id, choice);
    }
    return choice.expanded ?? count <= 4;
  }
  function bindTeamDisclosure(taskId) {
    const button = root.querySelector("[data-team-toggle]");
    if (!button) return;
    button.onclick = () => {
      const expanded = button.getAttribute("aria-expanded") !== "true";
      teamChoices.get(taskId).expanded = expanded;
      button.setAttribute("aria-expanded", String(expanded));
      button.setAttribute("aria-label", expanded ? "Hide team" : "Show team");
      root.querySelector("[data-team-roster]").hidden = !expanded;
    };
  }
  const presentation = createViewRefreshPresentation({
    render: () => {
      if (visible && !loading) render();
    },
  });
  function mount(container) {
    root = container;
    presentation.mount(container);
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
    if (teamClient !== client()) {
      teamClient = client();
      teamChoices.clear();
    }
    if (!client()) {
      presentation.interrupt();
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
    const wasVisible = visible;
    visible = false;
    hiddenTaskIds = new Set(details.map((d) => d.task.id));
    generation++;
    loading = false;
    clearInterval(clock);
    subscription?.stop();
    subscription = null;
    presentation.interrupt({ capture: wasVisible });
  }
  async function reload() {
    if (!visible) return;
    if (loading) {
      reloadAgain = true;
      return;
    }
    reloadAgain = false;
    const token = ++generation;
    const actionClient = client();
    const current = () =>
      visible && token === generation && client() === actionClient;
    const abandon = () => {
      if (token !== generation) return;
      loading = false;
      if (visible && client() !== actionClient) {
        subscription?.stop();
        subscription = null;
        void show();
      }
    };
    loading = true;
    try {
      const tasks = await actionClient.listTasks();
      const next = await Promise.all(
        tasks.map((t) =>
          t.status === "open" || t.cleanupPending > 0
            ? actionClient.getTask(t.id)
            : Promise.resolve({ task: t, agents: [] }),
        ),
      );
      if (!current()) return abandon();
      const added = !hasData
        ? [...next]
            .reverse()
            .find(
              (d) => d.task.status === "open" && !hiddenTaskIds.has(d.task.id),
            )
        : null;
      details = next;
      if (added) selected = added.task.id;
      hasData = true;
      // Saved task reads may paint while live capability discovery is held.
      // Keep known controls on a routine reload, but wait on a new client.
      if (capabilitiesClient !== actionClient) capabilities = null;
      render();
      void loadDelivery(selected, actionClient);
      const nextCapabilities = await actionClient
        .capabilities()
        .catch(() => null);
      if (!current()) return abandon();
      capabilities = nextCapabilities;
      capabilitiesClient = actionClient;
    } catch (error) {
      if (!current()) return abandon();
      hasData = false;
      presentation.interrupt();
      root.innerHTML = `<div class="mode-empty"><span class="eyebrow">PROJECTS</span><h2>Hub unavailable.</h2><p class="launcher-intro">${esc(error.message)}</p><button id="tasks-retry">Retry</button></div>`;
      root.querySelector("#tasks-retry").onclick = () => show();
      loading = false;
      return;
    }
    if (!current()) return abandon();
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
    if (!presentation.beforeRender(selected)) return;
    const lifecycleState = (task) => projectPauseState(task, capabilities);
    const projectButton = ({ task, agents }) =>
      `<button type="button" data-board-task="${esc(task.id)}" data-task-select="${esc(task.id)}" aria-pressed="${task.id === selected}" title="${esc(task.name)}"><span class="board-task-name">${esc(task.name)}</span><span class="fine">${esc(pauseStateLabel(task, capabilities, taskRollup(agents)))}</span></button>`;
    const detail = (entry) => {
      if (!entry)
        return `<div class="board-head"><div class="view-heading"><div><span class="eyebrow">PROJECTS</span><h2>Projects</h2></div></div><p class="fine">No projects yet.</p></div>`;
      const { task, agents } = entry;
      const resumePending = taskHub.hasPendingResume?.(task.id) === true;
      if (task.status !== "open")
        return `<div class="board-head"><div class="view-heading"><div><span class="eyebrow">PROJECT</span><h2>${esc(task.name)}</h2></div><div class="view-actions">${cacheFeedback(client()?.cacheStatus?.().label) ? `<span class="fine hub-sync-status" role="status">${esc(cacheFeedback(client()?.cacheStatus?.().label))}</span>` : ""}<button data-task-board="${esc(task.id)}">View history</button>${task.cleanupPending ? `<button data-task-cleanup="${esc(task.id)}">Retry cleanup</button>` : ""}</div></div><p class="fine">Closed · ${task.cleanupPending ? `${task.cleanupPending} session${task.cleanupPending === 1 ? "" : "s"} pending cleanup` : "Sessions closed"}</p></div><article class="task-card task-detail-card" data-task-card="${esc(task.id)}"><header><h3>${esc(task.name)}</h3></header>${task.goal ? `<p class="task-goal">${esc(task.goal)}</p>` : ""}</article>`;
      const pauseState = lifecycleState(task);
      if (
        ["cleanup_pending", "paused", "resuming", "unknown"].includes(
          pauseState,
        )
      ) {
        const pending = task.pauseCleanupPending;
        const stateText =
          pauseState === "cleanup_pending"
            ? `Pause cleanup pending · ${Number.isSafeInteger(pending) ? `${pending} exact-run cleanup${pending === 1 ? "" : "s"} pending` : "exact cleanup count unavailable"}. Resume stays blocked until every cleanup receipt and verified service handoff is resolved.`
            : pauseState === "paused"
              ? "Paused · the project, Board history, Bugs, Features, unfinished work and evidence are preserved. No team member can wake, schedule or be admitted until Resume."
              : pauseState === "resuming"
                ? "Resume pending · the exact fresh orchestrator admission is saved. Automatic wake and scheduling remain blocked until that new run is admitted."
                : "Pause status unavailable · lifecycle actions are blocked because this hub did not return complete projectPause v1 state.";
        return `<div class="board-head"><div class="view-heading"><div><span class="eyebrow">PROJECT</span><h2>${esc(task.name)}</h2></div><div class="view-actions">${cacheFeedback(client()?.cacheStatus?.().label) ? `<span class="fine hub-sync-status" role="status">${esc(cacheFeedback(client()?.cacheStatus?.().label))}</span>` : ""}<button data-task-board="${esc(task.id)}">Open board →</button>${pauseState === "cleanup_pending" ? `<button data-task-pause-handoff="${esc(task.id)}" data-testid="resolve-pause-handoff">Resolve service handoff…</button><button data-task-pause-cleanup="${esc(task.id)}" data-testid="pause-cleanup-pending">Retry exact cleanup</button>` : ""}${pauseState === "paused" || pauseState === "resuming" ? `<button data-task-resume="${esc(task.id)}" class="primary" data-testid="${pauseState === "resuming" ? "continue-project-resume" : "resume-project"}">${pauseState === "resuming" ? "Continue resume…" : "Resume…"}</button>` : ""}</div></div><p class="fine project-pause-state" role="status" data-testid="${pauseState === "paused" ? "project-paused" : pauseState === "cleanup_pending" ? "project-pause-cleanup-pending" : pauseState === "resuming" ? "project-resuming" : "project-pause-unknown"}">${esc(stateText)}</p></div><article class="task-card task-detail-card" data-task-card="${esc(task.id)}"><header><h3>Project retained</h3><span class="fine">Lifecycle generation ${Number.isSafeInteger(task.lifecycleGeneration) ? task.lifecycleGeneration : "unknown"}</span></header>${task.goal ? `<p class="task-goal">${esc(task.goal)}</p>` : ""}<footer class="task-actions"><span class="fine">${pauseState === "resuming" ? "Fresh team admission pending" : "Team dismissed · unfinished items unchanged"}</span>${openWorkItems ? `<button data-project-bugs="${esc(task.id)}">Bugs</button><button data-project-features="${esc(task.id)}">Features</button>` : ""}<div class="task-more"><button type="button" data-task-more="${esc(task.id)}" aria-expanded="false" aria-controls="task-menu-${esc(task.id)}">More</button><div id="task-menu-${esc(task.id)}" class="task-menu" popover="auto" aria-label="More project actions"><button data-task-close="${esc(task.id)}" class="danger">Close project</button></div></div></footer></article>`;
      }
      const needs = agents.filter((a) => a.status === "needs_input");
      const handler = agents.find((a) => a.role === "database_handler");
      return `<div class="board-head"><div class="view-heading"><div><span class="eyebrow">PROJECT</span><h2>${esc(task.name)}</h2></div><div class="view-actions">${cacheFeedback(client()?.cacheStatus?.().label) ? `<span class="fine hub-sync-status" role="status">${esc(cacheFeedback(client()?.cacheStatus?.().label))}</span>` : ""}${resumePending ? `<button data-task-resume="${esc(task.id)}" class="primary" data-testid="continue-project-resume">Continue resume…</button>` : ""}</div></div><p class="fine">${esc(task.goal || taskRollup(agents))}</p></div>${needs.length ? `<section class="needs-you"><div class="view-heading"><h3>Needs you</h3><span class="count-badge">${needs.length}</span></div>${needs.map((agent) => `<button class="attention-row" data-task-agent="${esc(agent.id)}"><strong>${esc(agent.name)}</strong><span>${esc(agent.host)}</span><span>Open →</span></button>`).join("")}</section>` : ""}<article class="task-card task-detail-card" data-task-card="${esc(task.id)}"><header class="project-team-heading"><h3>Team</h3>${agents.some((a) => a.status !== "closed") ? `<button type="button" class="project-team-toggle" data-team-toggle data-view-control="team" aria-label="${teamExpanded(task, agents.filter((a) => a.status !== "closed").length) ? "Hide team" : "Show team"}" aria-expanded="${teamExpanded(task, agents.filter((a) => a.status !== "closed").length)}" aria-controls="project-team-roster"><span class="project-team-chevron" aria-hidden="true">›</span><span>${agents.filter((a) => a.status !== "closed").length}</span></button>` : ""}<span class="fine">${esc(taskRollup(agents))}</span></header><div id="project-team-roster" class="task-agents" data-team-roster ${teamExpanded(task, agents.filter((a) => a.status !== "closed").length) ? "" : "hidden"}>${agents
        .filter((a) => a.status !== "closed")
        .map(
          (a) =>
            `<button class="board-agent" data-task-agent="${esc(a.id)}" title="${esc(a.blockedText || a.host + " · " + a.session)}"><span class="status-dot ${STATUS_DOT[a.status] || ""}"></span><span>${esc(a.name)}${a.role === "database_handler" ? " · Database handler" : ""}</span><span class="fine">${esc(agentState(a))} · ${esc(a.host)}</span></button>`,
        )
        .join(
          "",
        )}</div><footer class="task-actions"><span class="fine">${resumePending ? "Fresh-team launch incomplete" : task.allowAgentSpawn ? "Helpers allowed" : "Helpers off"}</span><button data-task-board="${esc(task.id)}">Open board →</button><button data-task-add="${esc(task.id)}" ${resumePending ? "disabled" : ""}>＋ Add agent</button><button data-task-lead="${esc(task.id)}" ${resumePending ? "disabled" : ""}>Replace lead</button>${!resumePending && (!handler || handler.status === "exited" || (!handler.online && !["retired", "closed"].includes(handler.status))) ? `<button data-handler-setup="${esc(task.id)}">${handler ? (handler.status === "exited" ? "Restart" : "Check") : "Set up"} database handler</button>` : ""}${openWorkItems ? `<button data-project-bugs="${esc(task.id)}">Bugs</button><button data-project-features="${esc(task.id)}">Features</button>` : ""}<div class="task-more"><button type="button" data-task-more="${esc(task.id)}" aria-expanded="false" aria-controls="task-menu-${esc(task.id)}">More</button><div id="task-menu-${esc(task.id)}" class="task-menu" popover="auto" aria-label="More project actions"><button data-task-attach="${esc(task.id)}">Open terminals</button><button data-task-settings="${esc(task.id)}">Settings</button><button data-task-pause="${esc(task.id)}" data-testid="pause-project" ${pauseState === "active" && !resumePending ? "" : `disabled aria-disabled="true" title="${resumePending ? "Finish the saved Resume first" : "Update the hub to projectPause v1"}"`}>Pause project…</button>${pauseState === "legacy" ? '<span class="fine" data-testid="pause-project-legacy">Pause unavailable · hub update required</span>' : ""}<button data-task-close="${esc(task.id)}" class="danger">Close project</button></div></div></footer></article>${renderTeamDelivery(deliveries.get(task.id), agents)}`;
    };
    root.innerHTML = `<div class="board mode-board tasks-view"><aside class="board-rail"><div class="board-rail-head"><span class="eyebrow">PROJECTS</span><button id="tasks-new" title="New project" aria-label="New project">＋</button></div>${open.map(projectButton).join("")}${closed.length ? `<details class="board-closed tasks-closed" data-view-disclosure="closed" ${current?.task.status !== "open" ? "open" : ""}><summary>Closed · ${closed.length}</summary>${closed.map(projectButton).join("")}</details>` : ""}</aside><section class="board-thread tasks-detail">${detail(current)}</section></div>`;
    presentation.afterRender(selected);
    bindTeamDisclosure(selected);
    root.querySelectorAll("[data-task-select]").forEach(
      (button) =>
        (button.onclick = () => {
          selected = button.dataset.taskSelect;
          render();
          void loadDelivery(selected);
        }),
    );
    root.querySelectorAll("[data-task-more]").forEach((button) => {
      const menu = root.querySelector(`#task-menu-${button.dataset.taskMore}`);
      menu.addEventListener("toggle", () => {
        button.setAttribute(
          "aria-expanded",
          String(menu.matches(":popover-open")),
        );
        presentation.settle();
      });
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
      .querySelectorAll("[data-task-lead]")
      .forEach(
        (b) => (b.onclick = () => taskHub.replaceLead(b.dataset.taskLead)),
      );
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
    root
      .querySelectorAll("[data-task-pause]")
      .forEach(
        (b) =>
          (b.onclick = () =>
            taskHub.pauseTask(b.dataset.taskPause, () => reload())),
      );
    root.querySelectorAll("[data-task-pause-cleanup]").forEach((b) => {
      b.onclick = async () => {
        b.disabled = true;
        try {
          const result = await taskHub.cleanupPauseTask(
            b.dataset.taskPauseCleanup,
          );
          notice(
            result.task.pauseState === "paused"
              ? `Project ${result.task.name} is fully paused.`
              : `Pause cleanup remains pending.${result.errors.length ? " " + result.errors[0] : ""}`,
          );
          await reload();
        } catch (error) {
          notice("Pause cleanup pending: " + error.message);
          b.disabled = false;
        }
      };
    });
    root
      .querySelectorAll("[data-task-pause-handoff]")
      .forEach(
        (b) =>
          (b.onclick = () =>
            taskHub.resolvePauseHandoff(b.dataset.taskPauseHandoff, () =>
              reload(),
            )),
      );
    root
      .querySelectorAll("[data-task-resume]")
      .forEach(
        (b) =>
          (b.onclick = () =>
            taskHub.resumeTask(b.dataset.taskResume, () => reload())),
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
