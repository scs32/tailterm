const esc = (value) =>
  String(value ?? "").replace(
    /[&<>"']/g,
    (char) =>
      ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[
        char
      ],
  );

const stateLabels = {
  waiting: "Waiting",
  claimed: "Claimed",
  active: "Active",
  completed: "Completed",
  cancelled: "Cancelled",
};
const priorities = {
  low: "Low",
  normal: "Normal",
  high: "High",
  urgent: "Urgent",
};

export function createQueueView({
  client,
  configure,
  notice,
  dialog,
  closeDialog,
  intentPersistence,
}) {
  let root,
    visible = false,
    generation = 0,
    subscription,
    tasks = [],
    entries = [],
    scope = "",
    includeTerminal = false,
    selected = "",
    capability = undefined,
    persistenceScope = "",
    loading = false,
    reloadAgain = false;
  const errors = new Map(),
    intents = new Map(),
    memory = new Map();
  const persistence = intentPersistence || {
    list: async (key) =>
      [...memory.values()].filter((value) => value.scope === key),
    save: async (value) =>
      memory.set(`${value.scope}\0${value.id}`, structuredClone(value)),
    remove: async (key, id, requestId) => {
      const current = memory.get(`${key}\0${id}`);
      if (!current || (requestId && current.requestId !== requestId)) return;
      memory.delete(`${key}\0${id}`);
    },
  };

  function mount(container) {
    root = container;
  }
  function hide() {
    visible = false;
    generation++;
    subscription?.stop();
    subscription = null;
  }
  async function scopeKey() {
    const connected = client();
    if (!connected) return "";
    const bytes = await crypto.subtle.digest(
      "SHA-256",
      new TextEncoder().encode(
        JSON.stringify([
          connected.base || "",
          connected.token || "",
          "queue-v1",
        ]),
      ),
    );
    return [...new Uint8Array(bytes)]
      .map((byte) => byte.toString(16).padStart(2, "0"))
      .join("");
  }
  async function show(taskId) {
    if (taskId !== undefined) scope = taskId || scope;
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
      reloadAgain = true;
      return;
    }
    const connected = client();
    if (!connected) {
      root.innerHTML =
        '<div class="mode-empty"><span class="eyebrow">QUEUE</span><h2>Connect a project hub.</h2><button data-queue-configure>Configure project hub</button></div>';
      root.querySelector("button").onclick = configure;
      return;
    }
    loading = true;
    reloadAgain = false;
    const requestAt = ++generation;
    try {
      let caps;
      try {
        caps = await connected.capabilities();
      } catch (error) {
        if (error.status === 404) caps = null;
        else throw error;
      }
      const nextTasks = await connected.listTasks();
      if (!visible || requestAt !== generation) return;
      capability = caps?.queue?.versions?.includes(1) ? caps.queue : null;
      tasks = nextTasks;
      if (!scope || !tasks.some((task) => task.id === scope))
        scope =
          tasks.find((task) => task.status === "open")?.id ||
          tasks[0]?.id ||
          "";
      persistenceScope = await scopeKey();
      intents.clear();
      for (const intent of await persistence.list(persistenceScope))
        intents.set(intent.id, intent);
      const next = [];
      if (scope && capability) {
        let cursor = "";
        do {
          const page = await connected.listQueue(scope, {
            cursor,
            limit: 64,
            includeTerminal: includeTerminal ? 1 : "",
          });
          next.push(...page.entries);
          cursor = page.cursor || "";
        } while (cursor && visible && requestAt === generation);
      }
      if (!visible || requestAt !== generation) return;
      entries = next;
      if (!entries.some((entry) => entry.id === selected))
        selected = entries[0]?.id || "";
      render();
    } catch (error) {
      if (!visible || requestAt !== generation) return;
      root.innerHTML = `<div class="mode-empty"><span class="eyebrow">QUEUE</span><h2>Hub unavailable.</h2><p role="alert">${esc(error.message)}</p><button data-queue-retry>Retry</button></div>`;
      root.querySelector("button").onclick = reload;
    } finally {
      loading = false;
      if (reloadAgain && visible) queueMicrotask(() => void reload());
    }
  }

  function markers(entry) {
    return [
      entry.reviewNeeded && "Review needed",
      entry.stale && "Stale offer",
      entry.pendingUpdate && "Pending update",
      entry.reconciliationNeeded && "Launch reconciliation needed",
      !entry.eligible && entry.state === "waiting" && "Not eligible",
    ].filter(Boolean);
  }
  function projectName(id) {
    return tasks.find((task) => task.id === id)?.name || id;
  }
  function render() {
    if (!visible || !root) return;
    const scroll = root.querySelector(".queue-main")?.scrollTop || 0;
    const selectedEntry = entries.find((entry) => entry.id === selected);
    const currentTask = tasks.find((task) => task.id === scope);
    const railButton = (task) =>
      `<button type="button" data-queue-task="${esc(task.id)}" aria-pressed="${task.id === scope}"><span class="board-task-name">${esc(task.name)}</span><span class="fine">${task.status === "closed" ? "Closed project" : "Receiving Queue"}</span></button>`;
    root.innerHTML = `<div class="board mode-board work-items-view queue-view"><aside class="board-rail work-items-rail" aria-label="Projects"><div class="board-rail-head"><span class="eyebrow">PROJECTS</span></div>${tasks
      .filter((task) => task.status === "open")
      .map(railButton)
      .join("")}${
      tasks.some((task) => task.status === "closed")
        ? `<details class="board-closed" ${currentTask?.status === "closed" ? "open" : ""}><summary>Closed · ${tasks.filter((task) => task.status === "closed").length}</summary>${tasks
            .filter((task) => task.status === "closed")
            .map(railButton)
            .join("")}</details>`
        : ""
    }</aside><section class="board-thread queue-main"><div class="board-head"><div class="view-heading"><div><span class="eyebrow">${esc(currentTask?.name || "QUEUE")}</span><h2>Queue <span class="count-badge">${entries.length}</span></h2></div><label class="queue-history-toggle"><input type="checkbox" data-queue-terminal ${includeTerminal ? "checked" : ""}> Terminal history</label></div><p class="fine">Priority is advisory. Send, read, priority and Pull never start an agent.</p></div>${capability ? `<div class="queue-layout"><div class="queue-list" aria-label="Queue entries">${entries.length ? entries.map((entry) => `<button type="button" class="queue-row" data-queue-entry="${esc(entry.id)}" aria-pressed="${entry.id === selected}"><span><strong>${esc(entry.item.title)}</strong><small>${esc(projectName(entry.sourceTaskId))} · ${esc(entry.item.kind)} ${esc(entry.itemId)}@${entry.offeredItemRevision}</small></span><span><strong>${esc(stateLabels[entry.state] || entry.state)}</strong><small>${esc(priorities[entry.queuePriority] || entry.queuePriority)}</small></span></button>`).join("") : '<p class="fine queue-empty">No entries match this project and history view.</p>'}</div><article class="queue-detail">${selectedEntry ? detail(selectedEntry) : ""}</article></div>` : `<div class="queue-unsupported"><p role="status">This hub does not advertise Queue v1. Queue actions are disabled; existing Send remains explicitly legacy.</p></div>`}</section></div>`;
    root.querySelectorAll("[data-queue-task]").forEach(
      (button) =>
        (button.onclick = () => {
          scope = button.dataset.queueTask;
          selected = "";
          errors.clear();
          void reload();
        }),
    );
    root
      .querySelector("[data-queue-terminal]")
      ?.addEventListener("change", (event) => {
        includeTerminal = event.target.checked;
        selected = "";
        void reload();
      });
    root.querySelectorAll("[data-queue-entry]").forEach(
      (button) =>
        (button.onclick = () => {
          selected = button.dataset.queueEntry;
          render();
        }),
    );
    root
      .querySelector("[data-queue-priority]")
      ?.addEventListener("change", (event) =>
        runNewAction(selectedEntry, "priority", {
          priority: event.target.value,
        }),
      );
    root
      .querySelector("[data-queue-pull]")
      ?.addEventListener("click", () => pull(selectedEntry));
    root
      .querySelector("[data-queue-history]")
      ?.addEventListener("click", () => history(selectedEntry));
    root
      .querySelectorAll("[data-queue-retry-intent]")
      .forEach(
        (button) =>
          (button.onclick = () => retryIntent(button.dataset.queueRetryIntent)),
      );
    root
      .querySelectorAll("[data-queue-discard-intent]")
      .forEach(
        (button) =>
          (button.onclick = () =>
            discardIntent(button.dataset.queueDiscardIntent)),
      );
    const main = root.querySelector(".queue-main");
    if (main) main.scrollTop = scroll;
  }
  function detail(entry) {
    const markerList = markers(entry),
      entryIntents = [...intents.values()].filter(
        (intent) => intent.entryId === entry.id,
      );
    return `<header><span class="eyebrow">${esc(stateLabels[entry.state] || entry.state)} · CYCLE ${entry.cycle}</span><h3>${esc(entry.item.title)}</h3><p class="fine">${esc(entry.sourceTaskId)} / ${esc(entry.itemId)} · offered r${entry.offeredItemRevision} · current r${entry.currentItemRevision}</p></header>${markerList.length ? `<div class="queue-markers">${markerList.map((marker) => `<span>${esc(marker)}</span>`).join("")}</div>` : ""}<p>${esc(entry.item.description || "No description.")}</p><dl class="queue-facts"><div><dt>Source status</dt><dd>${esc(entry.item.status)}</dd></div><div><dt>Queue revision</dt><dd>${entry.revision}</dd></div><div><dt>Claimant</dt><dd>${esc(entry.claimantAgentId || "Not claimed")}</dd></div><div><dt>Worker run</dt><dd>${esc(entry.workerRunId || "Not started")}</dd></div></dl><div class="queue-actions"><label>Queue priority<select data-queue-priority ${["completed", "cancelled"].includes(entry.state) ? "disabled" : ""}>${Object.entries(
      priorities,
    )
      .map(
        ([id, label]) =>
          `<option value="${id}" ${entry.queuePriority === id ? "selected" : ""}>${label}</option>`,
      )
      .join(
        "",
      )}</select></label><button type="button" data-queue-pull class="primary" ${entry.state !== "waiting" || !entry.eligible || entry.stale || entry.reviewNeeded || !entry.orchestratorAgentId ? "disabled" : ""}>Pull</button><button type="button" data-queue-history>History</button></div><p class="fine">Pull records selection only. Starting requires a separately admitted exact worker/run/order/context binding through the Database handler.</p>${errors.get(entry.id) ? `<p role="alert">${esc(errors.get(entry.id))}</p>` : ""}${entryIntents.map((intent) => `<div class="queue-recovery" role="status"><span>Uncertain ${esc(intent.payload.operation)} request · ${esc(intent.requestId)}</span><button type="button" data-queue-retry-intent="${esc(intent.id)}">Retry exact request</button><button type="button" data-queue-discard-intent="${esc(intent.id)}">Discard</button></div>`).join("")}`;
  }
  async function saveIntent(id, entry, payload) {
    const intent = {
      scope: persistenceScope,
      id,
      state: "uncertain",
      requestId: payload.requestId,
      entryId: entry.id,
      taskId: entry.targetTaskId,
      payload: structuredClone(payload),
      updatedAt: new Date().toISOString(),
    };
    intents.set(id, intent);
    await persistence.save(intent);
    render();
    return intent;
  }
  async function submitIntent(intent) {
    errors.delete(intent.entryId);
    try {
      const result = await client().queueAction(
        intent.taskId,
        intent.entryId,
        intent.payload,
      );
      const current = intents.get(intent.id);
      if (
        current?.requestId === intent.requestId &&
        JSON.stringify(current.payload) === JSON.stringify(intent.payload)
      ) {
        intents.delete(intent.id);
        await persistence.remove(persistenceScope, intent.id, intent.requestId);
      }
      entries = entries.map((entry) =>
        entry.id === result.entry.id ? result.entry : entry,
      );
      selected = result.entry.id;
      notice(
        `${stateLabels[result.entry.state] || result.entry.state} · Queue revision ${result.entry.revision}. No agent was started.`,
      );
    } catch (error) {
      errors.set(intent.entryId, error.message);
    }
    render();
  }
  async function runNewAction(entry, operation, fields = {}) {
    if (!entry) return;
    const id = `${entry.targetTaskId}:${entry.id}:${operation}`;
    const payload = {
      operation,
      requestId: crypto.randomUUID(),
      expectedRevision: entry.revision,
      cycle: entry.cycle,
      ...fields,
    };
    try {
      await submitIntent(await saveIntent(id, entry, payload));
    } catch (error) {
      errors.set(entry.id, error.message);
      render();
    }
  }
  function pull(entry) {
    if (!entry) return;
    dialog(
      "Pull from Queue",
      `<form id="queue-pull"><p><strong>${esc(entry.item.title)}</strong></p><p class="fine">Pull reserves this exact offered revision for the current orchestrator. It does not launch or resume a worker.</p><label>Work-order project<input name="orderTask" value="${esc(entry.sourceTaskId)}" required></label><label>Work-order message sequence<input name="orderSeq" type="number" min="1" required placeholder="1640"></label><p role="status" class="fine"></p><div class="dialog-actions"><button type="submit" class="primary">Pull only</button></div></form>`,
    );
    const form = document.querySelector("#queue-pull"),
      status = form.querySelector("[role=status]");
    form.onsubmit = async (event) => {
      event.preventDefault();
      const orderSeq = Number(form.elements.orderSeq.value);
      if (!Number.isSafeInteger(orderSeq) || orderSeq < 1) return;
      const payload = {
        expectedItemRevision: entry.offeredItemRevision,
        claimantAgentId: entry.orchestratorAgentId,
        claimantRunId: entry.orchestratorRunId,
        workOrderMessage: {
          taskId: form.elements.orderTask.value,
          seq: orderSeq,
        },
      };
      try {
        closeDialog();
        await runNewAction(entry, "claim", payload);
      } catch (error) {
        status.textContent = error.message;
      }
    };
  }
  async function history(entry) {
    if (!entry) return;
    dialog(
      "Queue history",
      '<div class="queue-history"><p class="fine">Loading immutable transitions…</p></div>',
    );
    const panel = document.querySelector(".queue-history"),
      events = [];
    try {
      let cursor = "";
      do {
        const page = await client().listQueueHistory(
          entry.targetTaskId,
          entry.id,
          { cursor, limit: 64 },
        );
        events.push(...page.events);
        cursor = page.cursor || "";
      } while (cursor && panel.isConnected);
      if (panel.isConnected)
        panel.innerHTML = events
          .map(
            (event) =>
              `<article><strong>${esc(event.kind)}</strong><span>event ${event.seq} · cycle ${event.cycle} · revision ${event.revision} · ${esc(event.effectiveAt)}</span>${event.reason ? `<p>${esc(event.reason)}</p>` : ""}</article>`,
          )
          .join("");
    } catch (error) {
      if (panel.isConnected)
        panel.innerHTML = `<p role="alert">${esc(error.message)}</p>`;
    }
  }
  async function retryIntent(id) {
    const intent = intents.get(id);
    if (intent) await submitIntent(intent);
  }
  async function discardIntent(id) {
    const intent = intents.get(id);
    if (!intent) return;
    intents.delete(id);
    await persistence.remove(persistenceScope, id, intent.requestId);
    render();
  }
  return { mount, show, hide, reload };
}
