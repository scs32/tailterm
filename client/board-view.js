import { readTaskHistory } from "./task-history.js";
import { downloadBlob } from "./terminal-extras.js";
import {
  captureDecisionPresentation,
  createDecisionDrafts,
  readAllDecisions,
  renderDecisionPanel,
  restoreDecisionPresentation,
} from "./board-decisions.js";
import {
  createViewRefreshPresentation,
  shouldReleaseViewPointer,
} from "./view-refresh-presentation.js";
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
export const shouldReleaseRailPointer = shouldReleaseViewPointer;
const BOARD_BOTTOM_FOLLOW_DISTANCE = 60;
const BOARD_SCROLL_IDLE_MS = 120;
const captureMessageScroll = (list) => {
  if (!list) return { followBottom: true, scrollTop: 0 };
  const scrollTop = Number(list.scrollTop) || 0;
  const distanceBottom =
    (Number(list.scrollHeight) || 0) -
    (Number(list.clientHeight) || 0) -
    scrollTop;
  if (distanceBottom < BOARD_BOTTOM_FOLLOW_DISTANCE)
    return { followBottom: true, scrollTop };
  const bounds = list.getBoundingClientRect?.();
  const anchor = bounds
    ? [...(list.querySelectorAll?.("[data-message]") || [])].find((row) => {
        const rect = row.getBoundingClientRect();
        return rect.bottom > bounds.top && rect.top < bounds.bottom;
      })
    : null;
  return {
    followBottom: false,
    scrollTop,
    anchorSeq: anchor?.dataset.message || "",
    anchorOffset: anchor
      ? anchor.getBoundingClientRect().top - bounds.top
      : null,
  };
};
const restoreMessageScroll = (list, saved) => {
  if (!list) return;
  if (!saved || saved.followBottom) {
    list.scrollTop = list.scrollHeight;
    return;
  }
  list.scrollTop = saved.scrollTop;
  const anchor = [...(list.querySelectorAll?.("[data-message]") || [])].find(
    (row) => row.dataset.message === saved.anchorSeq,
  );
  if (!anchor || saved.anchorOffset === null) return;
  const bounds = list.getBoundingClientRect();
  list.scrollTop +=
    anchor.getBoundingClientRect().top - bounds.top - saved.anchorOffset;
};
export function createBoardView({
  client,
  getTabs,
  activate,
  notice,
  addAgent,
  settings = () => {},
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
    decisions = [],
    decisionsTask = null,
    decisionLoadError = "",
    subscription = null,
    pending = false;
  let reloadAgain = false,
    messageScrollActive = false,
    messageTouchActive = false,
    messageScrollTimer = null,
    renderHeldForMessageScroll = false,
    ignoredScrollElement = null,
    ignoredScrollTop = 0;
  const sending = new Set();
  const decisionSending = new Set();
  const decisionErrors = new Map();
  const revealedAnswers = new Set();
  const decisionDrafts = createDecisionDrafts();
  const decisionPresentation = new Map();
  const completeConversations = new Map();
  const drafts = new Map();
  const beginMessage = (taskId, value) => {
    const body = {
      text: value.text.trim(),
      to: value.to,
      replyTo: value.replyTo,
    };
    const attemptedPayload = JSON.stringify(body);
    if (
      !value.requestId ||
      (value.attemptedPayload && value.attemptedPayload !== attemptedPayload)
    )
      value.requestId = crypto.randomUUID();
    value.attemptedPayload = attemptedPayload;
    drafts.set(taskId, value);
    return { requestId: value.requestId, ...body };
  };
  const presentation = createViewRefreshPresentation({
    render: () => {
      if (visible && !pending) render();
    },
  });
  function flushHeldMessageRender() {
    if (
      !renderHeldForMessageScroll ||
      messageScrollActive ||
      !visible ||
      pending
    )
      return;
    renderHeldForMessageScroll = false;
    render();
  }
  function scheduleMessageScrollIdle() {
    clearTimeout(messageScrollTimer);
    messageScrollTimer = null;
    if (messageTouchActive) return;
    messageScrollTimer = setTimeout(() => {
      messageScrollTimer = null;
      messageScrollActive = false;
      flushHeldMessageRender();
    }, BOARD_SCROLL_IDLE_MS);
  }
  function holdMessageScroll() {
    messageScrollActive = true;
    scheduleMessageScrollIdle();
  }
  function currentMessageList(event) {
    const list = event.currentTarget;
    return list === root?.querySelector?.("#board-messages") ? list : null;
  }
  function noteMessageScroll(event) {
    const list = currentMessageList(event);
    if (!list) return;
    if (
      event.type === "scroll" &&
      list === ignoredScrollElement &&
      Math.abs(list.scrollTop - ignoredScrollTop) < 0.5
    ) {
      ignoredScrollElement = null;
      return;
    }
    ignoredScrollElement = null;
    holdMessageScroll();
  }
  function startMessageTouch(event) {
    if (!currentMessageList(event)) return;
    messageTouchActive = true;
    messageScrollActive = true;
    clearTimeout(messageScrollTimer);
    messageScrollTimer = null;
  }
  function endMessageTouch(event) {
    if (!currentMessageList(event)) return;
    messageTouchActive = false;
    if (messageScrollActive) scheduleMessageScrollIdle();
  }
  function interruptMessageScroll() {
    clearTimeout(messageScrollTimer);
    messageScrollTimer = null;
    messageScrollActive = false;
    messageTouchActive = false;
    renderHeldForMessageScroll = false;
    ignoredScrollElement = null;
  }
  const draft = () => drafts.get(selected) || { text: "", to: "", replyTo: 0 };
  function saveDraft() {
    if (!root?.querySelector("#board-text")) return;
    drafts.set(renderedTask, {
      ...(drafts.get(renderedTask) || { text: "", to: "", replyTo: 0 }),
      text: root.querySelector("#board-text").value,
      to: root.querySelector("#board-to").value,
    });
  }
  function captureDecisionForm(form, clearError = false) {
    const requestSeq = Number(form?.dataset.decisionForm || 0);
    if (!requestSeq || !renderedTask) return;
    decisionDrafts.update(renderedTask, requestSeq, {
      optionId:
        form.querySelector(
          'input[name="decision-choice"]:checked:not([data-decision-custom-choice])',
        )?.value || "",
      customMode: Boolean(
        form.querySelector("[data-decision-custom-choice]:checked"),
      ),
      custom: form.querySelector("[data-decision-custom]")?.value || "",
      explanation:
        form.querySelector("[data-decision-explanation]")?.value || "",
    });
    if (clearError) decisionErrors.delete(requestSeq);
  }
  function saveDecisionDrafts() {
    root
      ?.querySelectorAll?.("[data-decision-form]")
      .forEach((form) => captureDecisionForm(form));
  }
  function saveDecisionPresentation() {
    if (!renderedTask) return;
    decisionPresentation.set(
      renderedTask,
      captureDecisionPresentation(root, decisionPresentation.get(renderedTask)),
    );
  }
  function revealDecisionAnswer(taskId, requestSeq) {
    revealedAnswers.add(requestSeq);
    const state = decisionPresentation.get(taskId) || {};
    decisionPresentation.set(taskId, { ...state, historyOpen: true });
    if (visible && renderedTask === taskId) {
      const history = root?.querySelector?.(".decision-history");
      if (history) history.open = true;
    }
  }
  function mount(container) {
    root = container;
    presentation.mount(container);
  }
  function hide() {
    const wasVisible = visible;
    saveDraft();
    saveDecisionDrafts();
    saveDecisionPresentation();
    visible = false;
    epoch++;
    subscription?.stop();
    subscription = null;
    pending = false;
    interruptMessageScroll();
    presentation.interrupt({ capture: wasVisible });
  }
  async function show(taskId) {
    visible = true;
    if (taskId && taskId !== selected) {
      interruptMessageScroll();
      saveDraft();
      saveDecisionDrafts();
      saveDecisionPresentation();
      selected = taskId;
    }
    const token = ++epoch;
    pending = false;
    subscription?.stop();
    subscription = null;
    if (!client()) {
      presentation.interrupt();
      root.innerHTML =
        '<div class="mode-empty"><span class="eyebrow">BOARD</span><h2>Connect a project hub.</h2><button id="board-configure" class="primary">Configure project hub</button></div>';
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
    if (!visible) return;
    if (pending) {
      reloadAgain = true;
      return;
    }
    reloadAgain = false;
    pending = true;
    try {
      const list = await client().listTasks();
      if (!visible || token !== epoch) return;
      tasks = list;
      if (!tasks.some((t) => t.id === selected)) {
        saveDraft();
        selected = tasks.find((t) => t.status === "open")?.id || null;
      }
      const id = selected;
      if (tasks.find((t) => t.id === id)?.status === "open")
        completeConversations.delete(id);
      const result = id
        ? await Promise.all([
            client().getTask(id),
            completeConversations.has(id)
              ? Promise.resolve(completeConversations.get(id))
              : client().listMessages(id, { limit: 200, latest: 1 }),
            readAllDecisions(client(), id).then(
              (value) => ({ value }),
              (error) => ({ error }),
            ),
          ])
        : [null, [], { value: [] }];
      if (!visible || token !== epoch || id !== selected) return;
      [detail, messages] = result;
      const decisionResult = result[2];
      if (decisionResult.error) {
        if (decisionsTask !== id) decisions = [];
        decisionLoadError = decisionResult.error.message;
      } else {
        decisions = decisionResult.value;
        decisionLoadError = "";
      }
      decisionsTask = id;
      render();
    } catch (e) {
      if (visible && token === epoch) {
        presentation.interrupt();
        root.innerHTML = `<div class="mode-empty"><h2>Hub unavailable</h2><p>${esc(e.message)}</p><button id="board-retry">Retry connection</button></div>`;
        root.querySelector("#board-retry").onclick = () => show();
      }
    } finally {
      if (token === epoch) {
        pending = false;
        if (reloadAgain && visible) {
          reloadAgain = false;
          void reload();
        } else flushHeldMessageRender();
      }
    }
  }
  function render() {
    if (!visible) return;
    if (renderedTask !== selected) interruptMessageScroll();
    if (
      messageScrollActive &&
      renderedTask === selected &&
      root?.querySelector?.("#board-messages")
    ) {
      renderHeldForMessageScroll = true;
      return;
    }
    renderHeldForMessageScroll = false;
    if (!presentation.beforeRender(selected)) return;
    saveDraft();
    saveDecisionDrafts();
    saveDecisionPresentation();
    const d = draft();
    const active = document.activeElement;
    const activeDecision = active?.closest?.("[data-decision-request]");
    let decisionFocus = null;
    if (activeDecision && root.contains(active)) {
      let selector = "";
      if (active.matches("[data-decision-custom]"))
        selector = "[data-decision-custom]";
      else if (active.matches("[data-decision-explanation]"))
        selector = "[data-decision-explanation]";
      else if (active.matches("[data-decision-custom-choice]"))
        selector = "[data-decision-custom-choice]";
      else if (active.matches("[data-decision-submit]"))
        selector = "[data-decision-submit]";
      else if (active.dataset.decisionOption)
        selector = `[data-decision-option="${active.dataset.decisionOption}"]`;
      if (selector)
        decisionFocus = {
          requestSeq: activeDecision.dataset.decisionRequest,
          selector,
          selection:
            typeof active.selectionStart === "number"
              ? [active.selectionStart, active.selectionEnd]
              : null,
        };
    }
    const focus = root.querySelector("#board-text") === document.activeElement;
    const selection = focus
      ? [
          document.activeElement.selectionStart,
          document.activeElement.selectionEnd,
        ]
      : null;
    const old =
        renderedTask === selected
          ? root.querySelector("#board-messages")
          : null,
      messageScroll = captureMessageScroll(old);
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
          (detail?.task.status === "closed" || a.status !== "closed") &&
          (m.broadcast || !m.to || a.id === m.to),
      );
      if (!recipients.length) return "Stored";
      const read = recipients.filter((a) => a.readUpTo >= m.seq).length;
      return read ? `Read by ${read}/${recipients.length}` : "Stored · unread";
    };
    const archived = detail?.task.status === "closed";
    const taskButton = (t) =>
      `<button data-board-task="${esc(t.id)}" aria-pressed="${t.id === selected}"><span class="board-task-name">${esc(t.name)}</span>${t.goal ? `<span class="fine">${esc(t.goal)}</span>` : ""}</button>`;
    const closedTasks = tasks.filter((t) => t.status === "closed");
    renderedTask = selected;
    root.innerHTML = `<div class="board mode-board"><aside class="board-rail"><div class="board-rail-head"><span class="eyebrow">PROJECTS</span><button id="board-new-task" title="New project">＋</button></div>${tasks
      .filter((t) => t.status === "open")
      .map(taskButton)
      .join(
        "",
      )}${closedTasks.length ? `<details class="board-closed" data-view-disclosure="closed" ${archived ? "open" : ""}><summary>Closed projects · ${closedTasks.length}</summary>${closedTasks.map(taskButton).join("")}</details>` : ""}</aside><section class="board-thread">${
      detail
        ? `<div class="board-head"><div class="view-heading"><div><span class="eyebrow">BOARD</span><h2>${esc(detail.task.name)}</h2></div><div class="view-actions">${archived ? `<span class="fine">Closed · ${esc(new Date(detail.task.closedAt).toLocaleDateString())}</span><button id="board-download">Download history</button>` : '<button id="board-attach">Terminals</button><button id="board-settings" title="Project settings">Settings</button>'}</div></div><p class="fine">${esc(detail.task.goal)}</p><span class="fine hub-sync-status" role="status">${esc(client()?.cacheStatus?.().label || "")}</span><div class="board-agents-row">${agents
            .filter((a) => archived || a.status !== "closed")
            .map(
              (a) =>
                `<button class="board-agent" ${archived ? "disabled" : `data-board-agent="${esc(a.id)}"`} title="${esc(a.blockedText || a.host + " · " + a.session)}"><span class="status-dot ${a.status === "running" ? "online" : a.status === "needs_input" ? "attention" : ""}"></span>${esc(a.name)}<span class="fine">${esc(status(a))}</span></button>`,
            )
            .join(
              "",
            )}${archived ? "" : '<button id="board-add-agent">＋ Agent</button>'}</div></div>${renderDecisionPanel({ records: decisions, taskId: selected, archived, name, drafts: decisionDrafts, sending: decisionSending, errors: decisionErrors, revealedAnswers, historyOpen: decisionPresentation.get(selected)?.historyOpen, loadError: decisionLoadError })}<div id="board-messages" class="board-messages">${messages.length === 200 && !completeConversations.has(selected) ? `<p class="fine">Latest 200 messages.${archived ? ' <button id="board-full-history">Show full conversation</button>' : " Full history remains on the hub."}</p>` : ""}${messages.map((m) => `<article class="board-message" data-message="${m.seq}"><div class="board-meta"><strong>${esc(name(m))}</strong><span>${m.to ? "to " + esc(names.get(m.to) || m.to) : "Team announcement"}${m.broadcast ? " · Swarm broadcast" : ""} · ${esc(archived ? new Date(m.createdAt).toLocaleString() : new Date(m.createdAt).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" }))}</span></div>${m.replyTo ? `<div class="reply-context">Reply to #${m.replyTo}: ${esc(messages.find((x) => x.seq === m.replyTo)?.text.slice(0, 100) || "Earlier message")}</div>` : ""}<div class="board-text">${esc(m.text)}</div><div class="message-footer"><span>${receipt(m)}</span>${archived ? "" : `<button data-reply="${m.seq}">Reply</button>`}</div></article>`).join("")}</div>${
            archived
              ? ""
              : `<form id="board-compose">${d.replyTo ? `<div class="compose-reply">Replying to #${d.replyTo}<button type="button" id="board-cancel-reply">Cancel reply</button></div>` : ""}<label class="compose-recipient">To<select ${sending.has(selected) ? "disabled" : ""} id="board-to" data-view-control="recipient" aria-label="Recipient"><option value="">Everyone</option>${agents
                  .filter((a) => a.status !== "closed")
                  .map(
                    (a) =>
                      `<option value="${esc(a.id)}" ${d.to === a.id ? "selected" : ""}>${esc(a.name)}</option>`,
                  )
                  .join(
                    "",
                  )}</select></label><textarea ${sending.has(selected) ? "disabled" : ""} id="board-text" rows="2" maxlength="8192" placeholder="Write a message…" aria-label="Message">${esc(d.text)}</textarea><button class="primary" type="submit" ${sending.has(selected) ? "disabled" : ""}>${sending.has(selected) ? "Sending…" : "Send"}</button><p class="compose-note fine">${detail.task.swarm ? "Swarm: everyone receives each message. To names the agent responsible for acting." : "Messages wait until agents check their inbox."}</p></form>`
          }`
        : ""
    }</section></div>`;
    presentation.afterRender(selected);
    root.querySelector("#board-new-task").onclick = () => newTask();
    root.querySelectorAll("[data-board-task]").forEach(
      (b) =>
        (b.onclick = () => {
          saveDraft();
          saveDecisionDrafts();
          saveDecisionPresentation();
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
              "Agent pane requested. Attach this project to a terminal tab if needed.",
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
    const settingsButton = root.querySelector("#board-settings");
    if (settingsButton) settingsButton.onclick = () => settings(selected);
    const decisionRetry = root.querySelector("[data-decisions-retry]");
    if (decisionRetry)
      decisionRetry.onclick = async () => {
        const id = selected;
        decisionRetry.disabled = true;
        try {
          await client().refreshDecisions?.(id);
        } catch {
          // reload renders the authoritative client error with a retry action.
        }
        if (visible && selected === id) await reload();
      };
    restoreDecisionPresentation(root, decisionPresentation.get(selected));
    const decisionHistory = root.querySelector(".decision-history");
    if (decisionHistory) {
      const historyTask = selected;
      decisionHistory.ontoggle = () => {
        const state = decisionPresentation.get(historyTask) || {};
        decisionPresentation.set(historyTask, {
          ...state,
          historyOpen: decisionHistory.open,
        });
      };
    }
    root.querySelectorAll("[data-decision-form]").forEach((form) => {
      const requestSeq = Number(form.dataset.decisionForm);
      const record = decisions.find(
        (candidate) => candidate.request.seq === requestSeq,
      );
      if (archived) return;
      const sync = () => {
        const customMode = Boolean(
          form.querySelector("[data-decision-custom-choice]:checked"),
        );
        const selected = form.querySelector("[data-decision-option]:checked");
        const custom = form.querySelector("[data-decision-custom-field]");
        const explanation = form.querySelector(
          "[data-decision-explanation-field]",
        );
        if (custom) custom.hidden = !customMode;
        if (explanation) explanation.hidden = customMode || !selected;
      };
      form.oninput = () => {
        captureDecisionForm(form, true);
        const error = form.querySelector("[data-decision-error]");
        if (error) error.textContent = "";
      };
      form.onchange = () => {
        captureDecisionForm(form, true);
        sync();
        const error = form.querySelector("[data-decision-error]");
        if (error) error.textContent = "";
      };
      sync();
      form.onsubmit = async (event) => {
        event.preventDefault();
        if (!record || decisionSending.has(requestSeq)) return;
        captureDecisionForm(form);
        let body;
        try {
          body = decisionDrafts.begin(selected, record);
        } catch (error) {
          decisionErrors.set(requestSeq, error.message);
          const output = form.querySelector("[data-decision-error]");
          if (output) output.textContent = error.message;
          return;
        }
        const id = selected;
        decisionErrors.delete(requestSeq);
        decisionSending.add(requestSeq);
        render();
        try {
          await client().answerDecision(id, requestSeq, body);
          decisionDrafts.resolve(id, requestSeq);
          revealDecisionAnswer(id, requestSeq);
          notice("Answer stored and sent to the requesting worker.");
          try {
            await client().refreshDecisions?.(id);
          } catch (refreshError) {
            if (visible && selected === id)
              decisionLoadError = refreshError.message;
          }
          if (visible && selected === id) await reload();
        } catch (error) {
          if (error.status === 409) {
            let latestRecords = null;
            try {
              if (client().refreshDecisions)
                await client().refreshDecisions(id);
              else client().invalidateDecisions?.(id);
              latestRecords = await readAllDecisions(client(), id);
              if (visible && selected === id) {
                decisions = latestRecords;
                decisionsTask = id;
                decisionLoadError = "";
              }
            } catch (refreshError) {
              if (visible && selected === id)
                decisionLoadError = refreshError.message;
            }
            const winner = (latestRecords || []).find(
              (candidate) =>
                candidate.request.seq === requestSeq && candidate.answer,
            );
            if (winner) {
              decisionDrafts.resolve(id, requestSeq);
              revealDecisionAnswer(id, requestSeq);
            } else decisionErrors.set(requestSeq, error.message);
          } else decisionErrors.set(requestSeq, error.message);
        } finally {
          decisionSending.delete(requestSeq);
          if (visible && selected === id) render();
        }
      };
    });
    for (const [selector, download] of [
      ["#board-download", true],
      ["#board-full-history", false],
    ]) {
      const button = root.querySelector(selector);
      if (!button) continue;
      button.onclick = async () => {
        const id = selected,
          token = epoch;
        button.disabled = true;
        try {
          const history = await readTaskHistory(client(), id);
          if (download) {
            const name =
              history.task.name.replace(/[^a-zA-Z0-9_-]+/g, "-").slice(0, 80) ||
              "task";
            downloadBlob(
              new Blob([JSON.stringify(history, null, 2)], {
                type: "application/json",
              }),
              `${name}-history.json`,
            );
          } else if (visible && token === epoch && selected === id) {
            completeConversations.set(id, history.messages);
            messages = history.messages;
            render();
          }
        } catch (error) {
          notice("History unavailable: " + error.message);
        } finally {
          if (button.isConnected) button.disabled = false;
        }
      };
    }
    const form = root.querySelector("#board-compose");
    if (form) {
      form.onsubmit = async (e) => {
        e.preventDefault();
        saveDraft();
        const id = selected,
          body = { ...draft() };
        if (!body.text.trim() || sending.has(id)) return;
        const message = beginMessage(id, body);
        const submittedInput = form.querySelector("#board-text");
        const submittedSelection =
          document.activeElement === submittedInput
            ? [submittedInput.selectionStart, submittedInput.selectionEnd]
            : null;
        let failed = false;
        sending.add(id);
        const button = form.querySelector("[type=submit]");
        button.disabled = true;
        try {
          await client().postMessage(id, message);
          drafts.set(id, { text: "", to: body.to, replyTo: 0 });
          if (visible && id === selected) {
            root.querySelector("#board-text").value = "";
            await reload();
          }
        } catch (error) {
          failed = true;
          notice("Post failed: " + error.message);
        } finally {
          sending.delete(id);
          if (visible && selected === id) render();
          if (failed && visible && selected === id && submittedSelection) {
            const restoredInput = root.querySelector("#board-text");
            restoredInput?.focus();
            restoredInput?.setSelectionRange(...submittedSelection);
          }
          if (button.isConnected) button.disabled = false;
        }
      };
      const input = form.querySelector?.("#board-text");
      if (input) {
        let composing = false;
        input.addEventListener("compositionstart", () => {
          composing = true;
        });
        input.addEventListener("compositionend", () => {
          composing = false;
        });
        input.onkeydown = (event) => {
          if (event.key !== "Enter" || event.shiftKey) return;
          if (event.isComposing || composing || event.keyCode === 229) return;
          event.preventDefault();
          if (event.repeat || input.disabled || sending.has(selected)) return;
          form.requestSubmit();
        };
      }
    }
    const list = root.querySelector("#board-messages");
    restoreMessageScroll(list, messageScroll);
    if (list) {
      ignoredScrollElement = list;
      ignoredScrollTop = list.scrollTop;
      list.addEventListener?.("scroll", noteMessageScroll, { passive: true });
      list.addEventListener?.("wheel", noteMessageScroll, { passive: true });
      list.addEventListener?.("touchstart", startMessageTouch, {
        passive: true,
      });
      list.addEventListener?.("touchend", endMessageTouch, { passive: true });
      list.addEventListener?.("touchcancel", endMessageTouch, {
        passive: true,
      });
    }
    if (focus) {
      const input = root.querySelector("#board-text");
      input?.focus();
      if (selection) input?.setSelectionRange(...selection);
    }
    if (decisionFocus) {
      const input = root.querySelector(
        `[data-decision-request="${decisionFocus.requestSeq}"] ${decisionFocus.selector}`,
      );
      input?.focus();
      if (decisionFocus.selection)
        input?.setSelectionRange?.(...decisionFocus.selection);
    }
  }
  return { mount, show, hide, reload, selected: () => selected };
}
