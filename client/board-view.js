// Board mode: every task's messages in one place, scoped by task, with a
// compose box. Subscribes to the hub's global feed while visible.
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
const time = (iso) =>
  new Date(iso).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });

export function createBoardView({
  client,
  getTabs,
  activate,
  notice,
  addAgent,
  attachTask,
  newTask,
  configure,
}) {
  let root = null,
    tasks = [],
    selected = null,
    detail = null,
    messages = [],
    subscription = null,
    loading = false;
  const names = new Map();

  function mount(container) {
    root = container;
  }
  async function show(taskId) {
    if (!root) return;
    if (!client()) {
      root.innerHTML = `<div class="mode-empty"><span class="eyebrow">BOARD</span><h2>Connect a task hub.</h2><p class="launcher-intro">Messages between agents live on the hub. Configure its tailnet address to see them here.</p><button id="board-configure" class="primary">Configure task hub</button></div>`;
      root.querySelector("#board-configure").onclick = configure;
      return;
    }
    if (taskId) selected = taskId;
    await reload();
    subscribe();
  }
  function hide() {
    subscription?.stop();
    subscription = null;
  }
  async function reload() {
    if (loading) return;
    loading = true;
    try {
      tasks = (await client().listTasks()).filter((t) => t.status === "open");
      if (!selected || !tasks.some((t) => t.id === selected))
        selected = tasks[0]?.id || null;
      if (selected) {
        [detail, messages] = await Promise.all([
          client().getTask(selected),
          client().listMessages(selected, { limit: 200 }),
        ]);
        for (const a of detail.agents) names.set(a.id, a.name);
      } else {
        detail = null;
        messages = [];
      }
    } catch (error) {
      root.innerHTML = `<div class="mode-empty"><span class="eyebrow">BOARD</span><h2>Hub unavailable.</h2><p class="launcher-intro">${esc(error.message)}</p><button id="board-retry">Retry</button></div>`;
      root.querySelector("#board-retry").onclick = () => show();
      loading = false;
      return;
    }
    loading = false;
    render();
  }
  function subscribe() {
    if (subscription) return;
    let cursor = 0;
    subscription = client().subscribe(
      "",
      async (events, next) => {
        cursor = next;
        const mine = events.filter((e) => e.taskId === selected);
        if (
          events.some((e) => ["task_created", "task_closed"].includes(e.kind))
        )
          tasks = (await client().listTasks()).filter(
            (t) => t.status === "open",
          );
        if (!selected) {
          selected = tasks[0]?.id || null;
          if (selected) await reload();
          return;
        }
        if (!mine.length) {
          render();
          return;
        }
        try {
          const last = messages.at(-1)?.seq || 0;
          const fresh = await client().listMessages(selected, {
            after: last,
            limit: 200,
          });
          addMessages(fresh);
          if (mine.some((e) => e.kind !== "message")) {
            detail = await client().getTask(selected);
            for (const a of detail.agents) names.set(a.id, a.name);
          }
          render();
        } catch {}
      },
      { after: 0, onError: () => {} },
    );
    void cursor;
  }
  const addMessages = (list) => {
    const seen = new Set(messages.map((m) => m.seq));
    for (const m of list)
      if (!seen.has(m.seq)) {
        messages.push(m);
        seen.add(m.seq);
      }
    messages.sort((a, b) => a.seq - b.seq);
  };

  function render() {
    if (!root) return;
    const keep = root.querySelector("#board-text")?.value || "";
    const rail = tasks.length
      ? tasks
          .map(
            (t) =>
              `<button data-board-task="${esc(t.id)}" aria-pressed="${t.id === selected}"><span class="board-task-name">${esc(t.name)}</span><span class="fine">${esc(t.goal || t.id)}</span></button>`,
          )
          .join("")
      : '<p class="fine">No open tasks.</p>';
    const agents = detail
      ? detail.agents
          .map(
            (a) =>
              `<button class="board-agent" data-board-agent="${esc(a.id)}" title="${esc(a.host)} · ${esc(a.session)} · ${esc(a.status)}"><span class="status-dot ${STATUS_DOT[a.status] || ""}"></span><span>${esc(a.name)}</span><span class="fine">${esc(a.status.replace("_", " "))}${a.unread ? ` · ${a.unread} unread` : ""}</span></button>`,
          )
          .join("")
      : "";
    const thread = messages.length
      ? messages
          .map((m) => {
            const from = m.from.agentId
              ? names.get(m.from.agentId) || m.from.agentId
              : m.from.user || "you";
            const to = m.to ? ` → ${names.get(m.to) || m.to}` : "";
            return `<div class="board-message"><span class="board-meta">${esc(from)}${esc(to)} · ${esc(time(m.createdAt))}</span><div class="board-text">${esc(m.text)}</div></div>`;
          })
          .join("")
      : '<p class="fine">No messages on this task yet.</p>';
    root.innerHTML = `<div class="board mode-board"><aside class="board-rail"><div class="board-rail-head"><span class="eyebrow">TASKS</span><button id="board-new-task" title="New task">＋</button></div>${rail}</aside><section class="board-thread">${
      detail
        ? `<div class="board-head"><h2>${esc(detail.task.name)}</h2><p class="fine">${esc(detail.task.goal || "")}</p><div class="board-agents-row">${agents || '<span class="fine">No agents yet.</span>'}<button id="board-add-agent">＋ Add agent</button><button id="board-attach">Attach to current tab</button></div></div><div id="board-messages" class="board-messages">${thread}</div><form id="board-compose"><select id="board-to" aria-label="Recipient"><option value="">Everyone</option>${detail.agents
            .filter((a) => a.status !== "closed")
            .map((a) => `<option value="${esc(a.id)}">${esc(a.name)}</option>`)
            .join(
              "",
            )}</select><textarea id="board-text" rows="2" placeholder="Message the task…" maxlength="8192" aria-label="Message">${esc(keep)}</textarea><button class="primary" type="submit">Post</button></form>`
        : `<div class="mode-empty"><span class="eyebrow">BOARD</span><h2>No task selected.</h2><p class="launcher-intro">Create a task to start a team.</p></div>`
    }</section></div>`;
    root.querySelectorAll("[data-board-task]").forEach(
      (b) =>
        (b.onclick = async () => {
          selected = b.dataset.boardTask;
          await reload();
        }),
    );
    root.querySelector("#board-new-task").onclick = () => newTask();
    root.querySelectorAll("[data-board-agent]").forEach((b) => {
      b.onclick = () => {
        const tab = getTabs().find(
          (t) => !t.disposed && t.task?.agentId === b.dataset.boardAgent,
        );
        if (tab) activate(tab.id);
        else notice("That agent has no open pane in this browser.");
      };
    });
    const add = root.querySelector("#board-add-agent");
    if (add) add.onclick = () => addAgent(selected);
    const attach = root.querySelector("#board-attach");
    if (attach) attach.onclick = () => attachTask(selected);
    const list = root.querySelector("#board-messages");
    if (list) list.scrollTop = list.scrollHeight;
    const form = root.querySelector("#board-compose");
    if (form)
      form.onsubmit = async (e) => {
        e.preventDefault();
        const text = root.querySelector("#board-text").value.trim();
        if (!text) return;
        try {
          const m = await client().postMessage(selected, {
            text,
            to: root.querySelector("#board-to").value,
          });
          root.querySelector("#board-text").value = "";
          addMessages([m]);
          render();
        } catch (error) {
          notice("Post failed: " + error.message);
        }
      };
  }
  return { mount, show, hide, reload, selected: () => selected };
}
