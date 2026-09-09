import { createViewRefreshPresentation } from "./view-refresh-presentation.js";

const esc = (s) =>
  String(s ?? "").replace(
    /[&<>"']/g,
    (c) =>
      ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[
        c
      ],
  );
const statuses = {
  open: "Open",
  in_progress: "In progress",
  blocked: "Blocked",
  done: "Done",
  dismissed: "Dismissed",
};
const priorities = {
  low: "Low",
  normal: "Normal",
  high: "High",
  urgent: "Urgent",
};
const options = (values, selected) =>
  Object.entries(values)
    .map(
      ([id, label]) =>
        `<option value="${esc(id)}" ${id === selected ? "selected" : ""}>${esc(label)}</option>`,
    )
    .join("");

// The hub owns records. This view retains only filters and in-flight form drafts.
export function createWorkItemsView({
  kind,
  client,
  dialog,
  closeDialog,
  notice,
  configure,
  openBoard,
}) {
  const plural = kind === "bug" ? "Bugs" : "Features",
    singular = kind === "bug" ? "Bug" : "Feature";
  let root,
    visible = false,
    generation = 0,
    subscription,
    tasks = [],
    items = [],
    scope = "",
    state = "",
    loading = false,
    again = false;
  const presentation = createViewRefreshPresentation({
    render: () => {
      if (visible && !loading) render();
    },
  });
  const project = (id) => tasks.find((t) => t.id === id);
  function mount(container) {
    root = container;
    presentation.mount(container);
  }
  function hide() {
    const wasVisible = visible;
    visible = false;
    generation++;
    loading = false;
    subscription?.stop();
    subscription = null;
    presentation.interrupt({ capture: wasVisible });
  }
  async function show(taskId) {
    if (taskId !== undefined) scope = taskId || "";
    visible = true;
    await reload();
    if (visible && client() && !subscription)
      subscription = client().subscribe("", () => reload(), {
        onError: () => {},
      });
  }
  async function reload() {
    if (!visible || !root) return;
    if (loading) {
      again = true;
      return;
    }
    if (!client()) {
      presentation.interrupt();
      root.innerHTML = `<div class="mode-empty"><span class="eyebrow">${plural.toUpperCase()}</span><h2>Connect a project hub.</h2><button data-items-configure>Configure project hub</button></div>`;
      root.querySelector("button").onclick = configure;
      return;
    }
    loading = true;
    again = false;
    const token = ++generation;
    const filter = { taskId: scope, status: state };
    try {
      const nextTasks = await client().listTasks(),
        next = [];
      if (!visible || token !== generation) return;
      let after = 0;
      do {
        const page = await client().listWorkItems({
          kind,
          ...filter,
          after,
          limit: 200,
        });
        next.push(...page.items);
        if (!page.items.length || !page.next || page.next <= after) break;
        after = page.next;
      } while (visible && token === generation);
      if (!visible || token !== generation) return;
      if (filter.taskId !== scope || filter.status !== state) {
        again = true;
        return;
      }
      tasks = nextTasks;
      items = next;
      render();
    } catch (error) {
      if (!visible || token !== generation) return;
      presentation.interrupt();
      root.innerHTML = `<div class="mode-empty"><span class="eyebrow">${plural.toUpperCase()}</span><h2>Hub unavailable.</h2><p class="launcher-intro" role="alert">${esc(error.message)}</p><button data-items-retry>Retry</button></div>`;
      root.querySelector("button").onclick = reload;
    } finally {
      if (token === generation) {
        loading = false;
        if (again) void reload();
      }
    }
  }
  function render() {
    const open = tasks.filter((task) => task.status === "open"),
      closed = tasks.filter((task) => task.status === "closed"),
      selectedProject = project(scope),
      syncLabel = client()?.cacheStatus?.().label || "",
      projectButton = (task) =>
        `<button type="button" data-board-task="${esc(task.id)}" data-items-scope="${esc(task.id)}" aria-pressed="${scope === task.id}" title="${esc(task.name)}"><span class="board-task-name">${esc(task.name)}</span><span class="fine">${esc(task.goal || (task.status === "closed" ? "Closed project" : "Open project"))}</span></button>`;
    if (!presentation.beforeRender(scope || "all")) return;
    root.innerHTML = `<div class="board mode-board work-items-view"><aside class="board-rail work-items-rail" aria-label="Projects"><div class="board-rail-head"><span class="eyebrow">PROJECTS</span><button type="button" data-items-new-rail title="New ${singular.toLowerCase()}" aria-label="New ${singular.toLowerCase()}" ${!open.length || selectedProject?.status === "closed" ? "disabled" : ""}>＋</button></div><button type="button" data-board-task="all" data-items-scope="" aria-pressed="${!scope}"><span class="board-task-name">All projects</span><span class="fine">${open.length} open project${open.length === 1 ? "" : "s"}</span></button>${open.map(projectButton).join("")}${closed.length ? `<details class="board-closed work-items-closed" data-view-disclosure="closed" ${selectedProject?.status === "closed" ? "open" : ""}><summary>Closed · ${closed.length}</summary>${closed.map(projectButton).join("")}</details>` : ""}</aside><section class="board-thread work-items-main"><div class="board-head work-items-head"><div class="view-heading"><div class="work-items-heading"><span class="eyebrow">${selectedProject ? "PROJECT" : "ALL PROJECTS"}</span><h2>${plural} <span class="count-badge">${items.length}</span></h2></div><div class="work-items-controls"><label class="work-items-project-select"><span>Project</span><select data-items-project data-view-control="project"><option value="">All projects</option>${tasks.map((t) => `<option value="${esc(t.id)}" ${scope === t.id ? "selected" : ""}>${esc(t.name)}${t.status === "closed" ? " · closed" : ""}</option>`).join("")}</select></label><label class="work-items-status-select"><span>Status</span><select data-items-status data-view-control="status"><option value="">All statuses</option>${options(statuses, state)}</select></label><span class="work-items-sync fine" data-work-items-sync role="status" aria-live="polite">${esc(syncLabel)}</span><button data-items-new class="primary" ${!open.length || selectedProject?.status === "closed" ? "disabled" : ""}>＋ New ${singular.toLowerCase()}</button></div></div><p class="fine">${esc(selectedProject?.name || "All projects")}</p></div><div class="work-items-list">${items.length ? items.map((item) => `<article class="work-item" data-work-item="${esc(item.id)}"><div class="work-item-copy"><button class="work-item-title" data-item-edit="${esc(item.id)}">${esc(item.title)}</button><span class="fine">${esc(project(item.taskId)?.name || item.taskId)} · ${esc(statuses[item.status])} · ${esc(priorities[item.priority])}${item.lastDispatch ? ` · Sent to ${esc(project(item.lastDispatch.targetTaskId)?.name || item.lastDispatch.targetTaskId)}` : ""}</span></div><button data-item-send="${esc(item.id)}" ${project(item.taskId)?.status === "closed" ? "disabled" : ""}>Send to project</button></article>`).join("") : `<p class="work-items-empty fine">No ${plural.toLowerCase()} match these filters.</p>`}</div></section></div>`;
    presentation.afterRender(scope || "all");
    root.querySelectorAll("[data-items-scope]").forEach(
      (button) =>
        (button.onclick = () => {
          scope = button.dataset.itemsScope;
          void reload();
        }),
    );
    root.querySelector("[data-items-project]").onchange = (e) => {
      presentation.commit(e.target, { flush: false });
      scope = e.target.value;
      void reload();
    };
    root.querySelector("[data-items-status]").onchange = (e) => {
      presentation.commit(e.target, { flush: false });
      state = e.target.value;
      void reload();
    };
    root.querySelector("[data-items-new]").onclick = () => edit();
    root.querySelector("[data-items-new-rail]").onclick = () => edit();
    root
      .querySelectorAll("[data-item-edit]")
      .forEach(
        (b) =>
          (b.onclick = () =>
            edit(items.find((i) => i.id === b.dataset.itemEdit))),
      );
    root
      .querySelectorAll("[data-item-send]")
      .forEach(
        (b) =>
          (b.onclick = () =>
            dispatch(items.find((i) => i.id === b.dataset.itemSend))),
      );
  }
  function edit(item) {
    const readonly = item && project(item.taskId)?.status === "closed";
    dialog(
      item ? singular : `New ${singular.toLowerCase()}`,
      `<form id="work-item-form"><label>Project<select id="work-item-project" ${item ? "disabled" : ""}>${tasks
        .filter((t) => t.status === "open" || t.id === item?.taskId)
        .map(
          (t) =>
            `<option value="${esc(t.id)}" ${t.id === (item?.taskId || scope) ? "selected" : ""}>${esc(t.name)}</option>`,
        )
        .join(
          "",
        )}</select></label><label>Title<input id="work-item-title" required maxlength="120" value="${esc(item?.title || "")}" ${readonly ? "disabled" : ""}></label><label>Description<textarea id="work-item-description" rows="5" maxlength="8192" ${readonly ? "disabled" : ""}>${esc(item?.description || "")}</textarea></label><div class="work-item-fields"><label>Status<select id="work-item-status" ${!item || readonly ? "disabled" : ""}>${options(statuses, item?.status || "open")}</select></label><label>Priority<select id="work-item-priority" ${readonly ? "disabled" : ""}>${options(priorities, item?.priority || "normal")}</select></label></div><p id="work-item-error" class="fine" role="status">${readonly ? "This project is closed. Its records are read-only." : ""}</p><div class="dialog-actions">${readonly ? "" : '<button type="submit" class="primary">Save</button>'}${item && !readonly ? '<button type="button" data-item-dispatch>Send to project</button>' : ""}</div></form>`,
    );
    const form = document.querySelector("#work-item-form"),
      error = form.querySelector("#work-item-error");
    let pending = false,
      key = crypto.randomUUID(),
      lastPayload = "";
    form
      .querySelector("[data-item-dispatch]")
      ?.addEventListener("click", () => dispatch(item));
    form.onsubmit = async (event) => {
      event.preventDefault();
      if (pending || readonly) return;
      const taskId =
        item?.taskId || form.querySelector("#work-item-project").value;
      const body = {
        title: form.querySelector("#work-item-title").value.trim(),
        description: form.querySelector("#work-item-description").value,
        priority: form.querySelector("#work-item-priority").value,
      };
      if (!taskId || !body.title) {
        error.textContent = "Choose a project and enter a title.";
        return;
      }
      const payload = JSON.stringify({ taskId, ...body });
      if (lastPayload && lastPayload !== payload) key = crypto.randomUUID();
      lastPayload = payload;
      pending = true;
      form.querySelectorAll("button").forEach((b) => (b.disabled = true));
      error.textContent = "Saving…";
      try {
        if (item)
          await client().updateWorkItem(taskId, item.id, {
            ...body,
            status: form.querySelector("#work-item-status").value,
            revision: item.revision,
          });
        else
          await client().createWorkItem(taskId, {
            ...body,
            kind,
            requestId: key,
          });
        if (form.isConnected) closeDialog();
        notice(`${singular} saved.`);
        await reload();
      } catch (e) {
        if (form.isConnected)
          error.textContent =
            e.status === 409
              ? `${e.message} Reopen this item to load its latest version; your text is retained here.`
              : e.message;
      } finally {
        pending = false;
        form.querySelectorAll("button").forEach((b) => (b.disabled = false));
      }
    };
    if (!readonly) form.querySelector("#work-item-title").focus();
  }
  function dispatch(item) {
    if (!item || project(item.taskId)?.status === "closed") return;
    dialog(
      "Send to project",
      `<form id="work-item-dispatch"><p>${esc(item.title)}</p><label>Project<select id="work-item-target">${tasks
        .filter((t) => t.status === "open")
        .map(
          (t) =>
            `<option value="${esc(t.id)}" ${t.id === item.taskId ? "selected" : ""}>${esc(t.name)}${t.orchestrator ? " · " + esc(t.orchestrator) : " · no orchestrator"}</option>`,
        )
        .join(
          "",
        )}</select></label><p class="fine">Send this revision to the project’s orchestrator. The item stays in ${esc(project(item.taskId)?.name || item.taskId)}.</p><p id="work-item-dispatch-status" class="fine" role="status"></p><div class="dialog-actions"><button type="submit" class="primary">Send to project</button></div></form>`,
    );
    const form = document.querySelector("#work-item-dispatch"),
      button = form.querySelector("button"),
      target = form.querySelector("select"),
      status = form.querySelector("[role=status]");
    let key = crypto.randomUUID();
    target.onchange = () => {
      key = crypto.randomUUID();
    };
    form.onsubmit = async (e) => {
      e.preventDefault();
      if (button.disabled) return;
      button.disabled = true;
      target.disabled = true;
      status.textContent = "Sending…";
      try {
        const result = await client().dispatchWorkItem(item.taskId, item.id, {
          revision: item.revision,
          targetTaskId: target.value,
          requestId: key,
        });
        if (form.isConnected) {
          status.textContent = `Sent to the project board · message #${result.dispatch.messageSeq}.`;
          button.type = "button";
          button.textContent = "Open board";
          button.onclick = () => {
            closeDialog();
            openBoard(result.dispatch.targetTaskId);
          };
          form.onsubmit = (e) => e.preventDefault();
        }
        await reload();
      } catch (error) {
        if (form.isConnected) {
          status.textContent = error.message;
          target.disabled = false;
        }
      } finally {
        button.disabled = false;
      }
    };
  }
  return { mount, show, hide, reload };
}
