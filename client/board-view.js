// The hub owns messages; view changes only affect presentation and drafts.
const esc = (s) =>
  String(s ?? "").replace(
    /[&<>"']/g,
    (c) =>
      ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[
        c
      ],
  );
const status = (a) => {
  const label =
    { done: "Turn complete", needs_input: "Needs you", exited: "Exited" }[
      a.status
    ] || a.status;
  const seen = Date.parse(a.lastSeenAt);
  const last = seen > 0 ? seen : Date.parse(a.createdAt);
  return a.runId &&
    !["exited", "closed"].includes(a.status) &&
    last > 0 &&
    Date.now() - last > 90000
    ? label + " · offline"
    : label;
};
export function createBoardView({
  client,
  getTabs,
  activate,
  notice,
  addAgent,
  revealAgent,
  attachTask,
  newTask,
  configure,
}) {
  let root,
    visible = false,
    epoch = 0,
    selected = null,
    renderedTask = null,
    tasks = [],
    detail = null,
    messages = [],
    subscription = null,
    pending = false;
  const drafts = new Map();
  const draft = () => drafts.get(selected) || { text: "", to: "", replyTo: 0 };
  function saveDraft() {
    if (!root?.querySelector("#board-text")) return;
    drafts.set(renderedTask, {
      ...(drafts.get(renderedTask) || { text: "", to: "", replyTo: 0 }),
      text: root.querySelector("#board-text").value,
      to: root.querySelector("#board-to").value,
    });
  }
  function mount(container) {
    root = container;
  }
  function hide() {
    saveDraft();
    visible = false;
    epoch++;
    subscription?.stop();
    subscription = null;
    pending = false;
  }
  async function show(taskId) {
    visible = true;
    if (taskId && taskId !== selected) {
      saveDraft();
      selected = taskId;
    }
    const token = ++epoch;
    pending = false;
    subscription?.stop();
    subscription = null;
    if (!client()) {
      root.innerHTML =
        '<div class="mode-empty"><span class="eyebrow">BOARD</span><h2>One conversation for your team.</h2><p class="launcher-intro">Connect your coordination hub to see tasks and exchange messages.</p><button id="board-configure" class="primary">Configure task hub</button></div>';
      root.querySelector("#board-configure").onclick = configure;
      return;
    }
    await reload(token);
    if (!visible || token !== epoch) return;
    subscription?.stop();
    subscription = client().subscribe("", () => reload(epoch), {
      after: 0,
      onError: () => {},
    });
  }
  async function reload(token = epoch) {
    if (!visible || pending) return;
    pending = true;
    try {
      const list = await client().listTasks();
      if (!visible || token !== epoch) return;
      tasks = list.filter((t) => t.status === "open");
      if (!tasks.some((t) => t.id === selected)) {
        saveDraft();
        selected = tasks[0]?.id || null;
      }
      const id = selected;
      const result = id
        ? await Promise.all([
            client().getTask(id),
            client().listMessages(id, { limit: 200, latest: 1 }),
          ])
        : [null, []];
      if (!visible || token !== epoch || id !== selected) return;
      [detail, messages] = result;
      render();
    } catch (e) {
      if (visible && token === epoch) {
        root.innerHTML = `<div class="mode-empty"><h2>Hub unavailable</h2><p>${esc(e.message)}</p><button id="board-retry">Retry connection</button></div>`;
        root.querySelector("#board-retry").onclick = () => show();
      }
    } finally {
      if (token === epoch) pending = false;
    }
  }
  function render() {
    if (!visible) return;
    saveDraft();
    const d = draft();
    const focus = root.querySelector("#board-text") === document.activeElement;
    const selection = focus
      ? [
          document.activeElement.selectionStart,
          document.activeElement.selectionEnd,
        ]
      : null;
    const old = root.querySelector("#board-messages"),
      scroll = old?.scrollTop || 0,
      bottom = !old || old.scrollHeight - old.clientHeight - scroll < 60;
    const agents = detail?.agents || [],
      names = new Map(agents.map((a) => [a.id, a.name]));
    const name = (m) =>
      m.from.agentId
        ? names.get(m.from.agentId) || m.from.agentId
        : m.from.user || "You";
    const receipt = (m) => {
      const recipients = agents.filter(
        (a) =>
          a.id !== m.from.agentId &&
          a.status !== "closed" &&
          (!m.to || a.id === m.to),
      );
      if (!recipients.length) return "Stored";
      const read = recipients.filter((a) => a.readUpTo >= m.seq).length;
      return read ? `Read by ${read}/${recipients.length}` : "Stored · unread";
    };
    renderedTask = selected;
    root.innerHTML = `<div class="board mode-board"><aside class="board-rail"><div class="board-rail-head"><span class="eyebrow">CONVERSATIONS</span><button id="board-new-task" title="New task">＋</button></div>${tasks.map((t) => `<button data-board-task="${esc(t.id)}" aria-pressed="${t.id === selected}"><span class="board-task-name">${esc(t.name)}</span><span class="fine">${esc(t.goal || "No objective yet")}</span></button>`).join("") || '<p class="fine">No open tasks.</p>'}</aside><section class="board-thread">${
      detail
        ? `<div class="board-head"><div class="view-heading"><div><span class="eyebrow">TEAM BOARD</span><h2>${esc(detail.task.name)}</h2></div><button id="board-attach">Show terminals</button></div><p class="fine">${esc(detail.task.goal)}</p><div class="board-agents-row">${agents
            .filter((a) => a.status !== "closed")
            .map(
              (a) =>
                `<button class="board-agent" data-board-agent="${esc(a.id)}" title="${esc(a.host)} · ${esc(a.session)}"><span class="status-dot ${a.status === "running" ? "online" : a.status === "needs_input" ? "attention" : ""}"></span>${esc(a.name)}<span class="fine">${esc(status(a))}</span></button>`,
            )
            .join(
              "",
            )}<button id="board-add-agent">＋ Agent</button></div></div><div id="board-messages" class="board-messages">${messages.length === 200 ? '<p class="fine">Latest 200 messages. Full history remains on the hub.</p>' : ""}${messages.map((m) => `<article class="board-message" data-message="${m.seq}"><div class="board-meta"><strong>${esc(name(m))}</strong><span>${m.to ? "to " + esc(names.get(m.to) || m.to) : "Team announcement"} · ${esc(new Date(m.createdAt).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" }))}</span></div>${m.replyTo ? `<div class="reply-context">Reply to #${m.replyTo}: ${esc(messages.find((x) => x.seq === m.replyTo)?.text.slice(0, 100) || "Earlier message")}</div>` : ""}<div class="board-text">${esc(m.text)}</div><div class="message-footer"><span>${receipt(m)} · #${m.seq}</span><button data-reply="${m.seq}">Reply</button></div></article>`).join("") || '<div class="board-empty"><h3>Start the conversation.</h3><p class="fine">Send an assignment to an agent or an announcement to the team.</p></div>'}</div><form id="board-compose">${d.replyTo ? `<div class="compose-reply">Replying to #${d.replyTo}<button type="button" id="board-cancel-reply">Cancel reply</button></div>` : ""}<label class="compose-recipient">To<select id="board-to" aria-label="Recipient"><option value="">Everyone · announcement</option>${agents
            .filter((a) => a.status !== "closed")
            .map(
              (a) =>
                `<option value="${esc(a.id)}" ${d.to === a.id ? "selected" : ""}>${esc(a.name)}</option>`,
            )
            .join(
              "",
            )}</select></label><textarea id="board-text" rows="2" maxlength="8192" placeholder="Write a message…" aria-label="Message">${esc(d.text)}</textarea><button class="primary" type="submit">Post</button><p class="compose-note fine">Saved on the hub. Read means retrieved, not completed.</p></form>`
        : '<div class="mode-empty"><h2>Bring a team together.</h2><p class="launcher-intro">Create a task with a shared objective, then add agents.</p></div>'
    }</section></div>`;
    root.querySelector("#board-new-task").onclick = () => newTask();
    root.querySelectorAll("[data-board-task]").forEach(
      (b) =>
        (b.onclick = () => {
          saveDraft();
          selected = b.dataset.boardTask;
          pending = false;
          void show();
        }),
    );
    root.querySelectorAll("[data-board-agent]").forEach(
      (b) =>
        (b.onclick = () => {
          const t = getTabs().find(
            (t) => !t.disposed && t.task?.agentId === b.dataset.boardAgent,
          );
          if (t) activate(t.id);
          else {
            revealAgent?.(b.dataset.boardAgent);
            notice(
              "Agent pane requested. Attach this task to a terminal tab if needed.",
            );
          }
        }),
    );
    const add = root.querySelector("#board-add-agent");
    if (add) add.onclick = () => addAgent(selected);
    const attach = root.querySelector("#board-attach");
    if (attach) attach.onclick = () => attachTask(selected);
    root.querySelectorAll("[data-reply]").forEach(
      (b) =>
        (b.onclick = () => {
          saveDraft();
          const m = messages.find((m) => m.seq === Number(b.dataset.reply));
          drafts.set(selected, {
            ...draft(),
            replyTo: m.seq,
            to: m.from.agentId || "",
          });
          root.querySelector("#board-to").value = m.from.agentId || "";
          render();
          root.querySelector("#board-text").focus();
        }),
    );
    const cancel = root.querySelector("#board-cancel-reply");
    if (cancel)
      cancel.onclick = () => {
        saveDraft();
        drafts.set(selected, { ...draft(), replyTo: 0 });
        render();
      };
    const form = root.querySelector("#board-compose");
    if (form)
      form.onsubmit = async (e) => {
        e.preventDefault();
        saveDraft();
        const id = selected,
          body = { ...draft() };
        if (!body.text.trim()) return;
        const button = form.querySelector("[type=submit]");
        button.disabled = true;
        try {
          await client().postMessage(id, {
            text: body.text.trim(),
            to: body.to,
            replyTo: body.replyTo,
          });
          drafts.set(id, { text: "", to: body.to, replyTo: 0 });
          if (visible && id === selected) {
            root.querySelector("#board-text").value = "";
            await reload();
          }
        } catch (error) {
          notice("Post failed: " + error.message);
        } finally {
          if (button.isConnected) button.disabled = false;
        }
      };
    const list = root.querySelector("#board-messages");
    if (list) list.scrollTop = bottom ? list.scrollHeight : scroll;
    if (focus) {
      const input = root.querySelector("#board-text");
      input?.focus();
      if (selection) input?.setSelectionRange(...selection);
    }
  }
  return { mount, show, hide, reload, selected: () => selected };
}
