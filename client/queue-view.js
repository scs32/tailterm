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
    subscriptionClient,
    activeClient,
    activeClientScope = "",
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
    subscriptionClient = null;
  }
  async function scopeKey(connected) {
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
  }
  async function reload() {
    if (!visible || !root) return;
    if (loading) {
      reloadAgain = true;
      generation++;
      return;
    }
    const connected = client();
    if (!connected) {
      generation++;
      activeClient = null;
      activeClientScope = "";
      subscription?.stop();
      subscription = null;
      subscriptionClient = null;
      root.innerHTML =
        '<div class="mode-empty"><span class="eyebrow">QUEUE</span><h2>Connect a project hub.</h2><button data-queue-configure>Configure project hub</button></div>';
      root.querySelector("button").onclick = configure;
      return;
    }
    loading = true;
    reloadAgain = false;
    const requestAt = ++generation,
      requestedScope = scope,
      requestedTerminal = includeTerminal,
      stillCurrent = () =>
        visible && requestAt === generation && client() === connected;
    try {
      let caps;
      try {
        caps = await connected.capabilities();
      } catch (error) {
        if (error.status === 404) caps = null;
        else throw error;
      }
      const nextTasks = await connected.listTasks();
      if (!stillCurrent()) return;
      const nextCapability = caps?.queue?.versions?.includes(1)
          ? caps.queue
          : null,
        nextScope = nextTasks.some((task) => task.id === requestedScope)
          ? requestedScope
          : nextTasks.find((task) => task.status === "open")?.id ||
            nextTasks[0]?.id ||
            "",
        nextPersistenceScope = await scopeKey(connected);
      if (!stillCurrent()) return;
      const savedIntents = await persistence.list(nextPersistenceScope),
        nextIntents = new Map(
          savedIntents.map((intent) => [intent.id, intent]),
        );
      if (!stillCurrent()) return;
      const next = [];
      if (nextScope && nextCapability) {
        let cursor = "";
        do {
          const page = await connected.listQueue(nextScope, {
            cursor,
            limit: 64,
            includeTerminal: requestedTerminal ? 1 : "",
          });
          if (!stillCurrent()) return;
          next.push(...page.entries);
          cursor = page.cursor || "";
        } while (cursor);
      }
      if (!stillCurrent()) return;
      capability = nextCapability;
      tasks = nextTasks;
      scope = nextScope;
      persistenceScope = nextPersistenceScope;
      intents.clear();
      for (const [id, intent] of nextIntents) intents.set(id, intent);
      entries = next;
      if (!entries.some((entry) => entry.id === selected))
        selected = entries[0]?.id || "";
      activeClient = connected;
      activeClientScope = nextPersistenceScope;
      if (subscriptionClient !== connected) {
        subscription?.stop();
        subscription = null;
        subscriptionClient = null;
      }
      if (!subscription && connected.subscribe) {
        subscription = connected.subscribe("", () => reload(), {
          onError: () => {},
        });
        subscriptionClient = connected;
      }
      render();
    } catch (error) {
      if (!stillCurrent()) return;
      root.innerHTML = `<div class="mode-empty"><span class="eyebrow">QUEUE</span><h2>Hub unavailable.</h2><p role="alert">${esc(error.message)}</p><button data-queue-retry>Retry</button></div>`;
      root.querySelector("button").onclick = reload;
    } finally {
      loading = false;
      if (visible && client() !== connected) reloadAgain = true;
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
    }</aside><section class="board-thread queue-main"><div class="board-head"><div class="view-heading"><div><span class="eyebrow">${esc(currentTask?.name || "QUEUE")}</span><h2>Queue <span class="count-badge">${entries.length}</span></h2></div><label class="queue-history-toggle"><input type="checkbox" data-queue-terminal ${includeTerminal ? "checked" : ""}><span>Terminal history</span></label></div><p class="fine">Priority is advisory. Send, read, priority and Pull never start an agent.</p></div>${capability ? `<div class="queue-layout"><div class="queue-list" aria-label="Queue entries">${entries.length ? entries.map((entry) => `<button type="button" class="queue-row" data-queue-entry="${esc(entry.id)}" aria-pressed="${entry.id === selected}"><span><strong>${esc(entry.item.title)}</strong><small>${esc(projectName(entry.sourceTaskId))} · ${esc(entry.item.kind)} ${esc(entry.itemId)}@${entry.offeredItemRevision}</small></span><span><strong>${esc(stateLabels[entry.state] || entry.state)}</strong><small>${esc(priorities[entry.queuePriority] || entry.queuePriority)}</small></span></button>`).join("") : '<p class="fine queue-empty">No entries match this project and history view.</p>'}</div><article class="queue-detail">${selectedEntry ? detail(selectedEntry) : ""}</article></div>` : `<div class="queue-unsupported"><p role="status">This hub does not advertise Queue v1. Queue actions are disabled; existing Send remains explicitly legacy.</p></div>`}</section></div>`;
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
      ),
      entryError =
        errors.get(entry.id) ||
        entryIntents.map((intent) => errors.get(intent.id)).find(Boolean);
    return `<header><span class="eyebrow">${esc(stateLabels[entry.state] || entry.state)} · CYCLE ${entry.cycle}</span><h3>${esc(entry.item.title)}</h3><p class="fine">${esc(entry.sourceTaskId)} / ${esc(entry.itemId)} · offered r${entry.offeredItemRevision} · current r${entry.currentItemRevision}</p></header>${markerList.length ? `<div class="queue-markers">${markerList.map((marker) => `<span>${esc(marker)}</span>`).join("")}</div>` : ""}<p>${esc(entry.item.description || "No description.")}</p><dl class="queue-facts"><div><dt>Source status</dt><dd>${esc(entry.item.status)}</dd></div><div><dt>Queue revision</dt><dd>${entry.revision}</dd></div><div><dt>Claimant</dt><dd>${esc(entry.claimantAgentId || "Not claimed")}</dd></div><div><dt>Worker run</dt><dd>${esc(entry.workerRunId || "Not started")}</dd></div></dl><div class="queue-actions"><label>Queue priority<select data-queue-priority ${["completed", "cancelled"].includes(entry.state) ? "disabled" : ""}>${Object.entries(
      priorities,
    )
      .map(
        ([id, label]) =>
          `<option value="${id}" ${entry.queuePriority === id ? "selected" : ""}>${label}</option>`,
      )
      .join(
        "",
      )}</select></label><button type="button" data-queue-pull class="primary" ${entry.state !== "waiting" || !entry.eligible || entry.stale || entry.reviewNeeded || !entry.orchestratorAgentId ? "disabled" : ""}>Pull</button><button type="button" data-queue-history>History</button></div><p class="fine">Pull records selection only. Starting requires a separately admitted exact worker/run/order/context binding through the Database handler.</p>${entryError ? `<p role="alert">${esc(entryError)}</p>` : ""}${entryIntents.map((intent) => `<div class="queue-recovery" role="status"><span>Uncertain ${esc(intent.payload.operation)} request · ${esc(intent.requestId)}</span><button type="button" data-queue-retry-intent="${esc(intent.id)}">Retry exact request</button><button type="button" data-queue-discard-intent="${esc(intent.id)}">Discard</button></div>`).join("")}`;
  }
  function canSend(session) {
    return (
      visible &&
      generation === session.generation &&
      scope === session.taskId &&
      persistenceScope === session.connectionScope
    );
  }
  function isCurrent(session) {
    return (
      canSend(session) &&
      client() === session.connected &&
      activeClient === session.connected &&
      activeClientScope === session.connectionScope
    );
  }
  async function saveIntent(id, entry, payload, session) {
    const intent = {
      scope: session.connectionScope,
      id,
      state: "uncertain",
      requestId: payload.requestId,
      entryId: entry.id,
      taskId: entry.targetTaskId,
      payload: structuredClone(payload),
      updatedAt: new Date().toISOString(),
    };
    await persistence.save(intent);
    if (isCurrent(session)) {
      intents.set(id, intent);
      render();
    }
    return intent;
  }
  async function submitIntent(intent, session) {
    if (!canSend(session)) return;
    try {
      const result = await session.connected.queueAction(
        intent.taskId,
        intent.entryId,
        intent.payload,
      );
      await persistence.remove(intent.scope, intent.id, intent.requestId);
      const current = intents.get(intent.id);
      if (
        current?.requestId === intent.requestId &&
        JSON.stringify(current.payload) === JSON.stringify(intent.payload)
      ) {
        if (
          persistenceScope === intent.scope &&
          intents.get(intent.id) === current
        )
          intents.delete(intent.id);
      }
      if (isCurrent(session)) {
        errors.delete(intent.id);
        entries = entries.map((entry) =>
          entry.id === result.entry.id &&
          result.entry.revision >= entry.revision
            ? result.entry
            : entry,
        );
        selected = result.entry.id;
        notice(
          `${stateLabels[result.entry.state] || result.entry.state} · Queue revision ${result.entry.revision}. No agent was started.`,
        );
      }
    } catch (error) {
      if (isCurrent(session)) errors.set(intent.id, error.message);
    }
    if (isCurrent(session)) render();
  }
  async function runNewAction(entry, operation, fields = {}) {
    if (!entry) return;
    const currentEntry = entries.find((value) => value.id === entry.id);
    if (
      !visible ||
      client() !== activeClient ||
      scope !== entry.targetTaskId ||
      currentEntry?.revision !== entry.revision ||
      currentEntry?.cycle !== entry.cycle
    ) {
      errors.set(entry.id, "Queue view changed. Refresh before acting.");
      render();
      return;
    }
    const session = {
        connected: activeClient,
        connectionScope: activeClientScope,
        generation,
        taskId: entry.targetTaskId,
      },
      requestId = crypto.randomUUID(),
      id = `${entry.targetTaskId}:${entry.id}:${operation}:${requestId}`;
    const payload = {
      operation,
      requestId,
      expectedRevision: entry.revision,
      cycle: entry.cycle,
      ...fields,
    };
    try {
      await submitIntent(
        await saveIntent(id, entry, payload, session),
        session,
      );
    } catch (error) {
      if (isCurrent(session)) {
        errors.set(entry.id, error.message);
        render();
      }
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
    const session = {
      connected: activeClient,
      connectionScope: activeClientScope,
      generation,
      taskId: entry.targetTaskId,
      entryId: entry.id,
    };
    if (!isCurrent(session) || selected !== entry.id) return;
    dialog(
      "Queue history",
      '<div class="queue-history"><p class="fine">Loading immutable transitions…</p></div>',
    );
    const panel = document.querySelector(".queue-history"),
      events = [];
    try {
      let cursor = "";
      do {
        const page = await session.connected.listQueueHistory(
          entry.targetTaskId,
          entry.id,
          { cursor, limit: 64 },
        );
        if (!isCurrent(session) || selected !== session.entryId) return;
        events.push(...page.events);
        cursor = page.cursor || "";
      } while (cursor && panel.isConnected);
      if (
        panel.isConnected &&
        isCurrent(session) &&
        selected === session.entryId
      )
        panel.innerHTML = events
          .map(
            (event) =>
              `<article><strong>${esc(event.kind)}</strong><span>event ${event.seq} · cycle ${event.cycle} · revision ${event.revision} · ${esc(event.effectiveAt)}</span>${event.reason ? `<p>${esc(event.reason)}</p>` : ""}</article>`,
          )
          .join("");
    } catch (error) {
      if (
        panel.isConnected &&
        isCurrent(session) &&
        selected === session.entryId
      )
        panel.innerHTML = `<p role="alert">${esc(error.message)}</p>`;
    }
  }
  async function retryIntent(id) {
    const intent = intents.get(id);
    if (!intent) return;
    const connected = client(),
      requestAt = generation,
      connectionScope = await scopeKey(connected);
    if (
      !visible ||
      generation !== requestAt ||
      client() !== connected ||
      connectionScope !== intent.scope ||
      persistenceScope !== intent.scope ||
      scope !== intent.taskId
    ) {
      if (visible && intents.get(id) === intent) {
        errors.set(
          id,
          "This request belongs to another connection or project.",
        );
        render();
      }
      return;
    }
    await submitIntent(intent, {
      connected,
      connectionScope,
      generation: requestAt,
      taskId: intent.taskId,
    });
  }
  async function discardIntent(id) {
    const intent = intents.get(id);
    if (!intent) return;
    intents.delete(id);
    await persistence.remove(intent.scope, id, intent.requestId);
    render();
  }
  return { mount, show, hide, reload };
}
