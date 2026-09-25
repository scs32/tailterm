import { readProjectAuditExport, readTaskHistory } from "./task-history.js";
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
const emptyDraft = () => ({
  text: "",
  to: "",
  replyTo: 0,
  auditKind: "",
  primaryTask: "",
  primaryItem: "",
  primaryRevision: "",
  itemTitle: "",
  itemDirect: false,
  related: "",
  orderTask: "",
  orderSeq: "",
});
function parseItemReference(value, relationship) {
  const match = String(value || "")
    .trim()
    .match(/^(tsk_[0-9a-f]{16})\/(wi_[0-9a-f]{16})@(\d+)$/);
  if (!match || Number(match[3]) < 1)
    throw new Error(
      `${relationship === "primary" ? "Primary" : "Related"} references must use TASK/ITEM@REVISION.`,
    );
  return {
    itemTaskId: match[1],
    itemId: match[2],
    itemRevision: Number(match[3]),
    relationship,
  };
}
function parseSources(value) {
  const lines = String(value || "")
    .split(/\n+/)
    .map((line) => line.trim())
    .filter(Boolean);
  if (!lines.length || lines.length > 16)
    throw new Error("Supply 1–16 exact correction sources as TASK#MESSAGE.");
  return lines.map((line) => {
    const match = line.match(/^(tsk_[0-9a-f]{16})#(\d+)$/);
    if (!match || Number(match[2]) < 1)
      throw new Error("Sources must use TASK#MESSAGE.");
    return { taskId: match[1], seq: Number(match[2]) };
  });
}
function auditLinksText(links = []) {
  return links
    .map(
      (link) =>
        `${link.relationship}: ${link.itemTaskId}/${link.itemId}@${link.itemRevision}`,
    )
    .join(" · ");
}
function auditProjectionText(projection) {
  if (!projection) return "none";
  const refs = auditLinksText(projection.workItems);
  return `${projection.classification}${projection.revision ? ` r${projection.revision}` : ""}${refs ? ` · ${refs}` : ""}`;
}
function auditContextMarkup(record) {
  if (!record?.original)
    return '<div class="message-audit audit-unknown" data-audit-kind="unknown"><span>Audit context unknown · exact read required</span></div>';
  const original = record.original;
  const current = record.current;
  const classification = current?.classification || original.classification;
  const order = original.workOrderMessage;
  return `<div class="message-audit" data-audit-kind="${esc(classification)}"><span>Original: ${esc(auditProjectionText(original))}</span>${order ? `<span>Order ${esc(order.taskId)}#${order.seq}</span>` : ""}${current ? `<span>Current: ${esc(auditProjectionText(current))}</span>` : "<span>Current: no classified projection</span>"}</div>`;
}
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
  intentPersistence,
}) {
  let root,
    visible = false,
    epoch = 0,
    selected = null,
    renderedTask = null,
    renderedClient = null,
    tasks = [],
    detail = null,
    messages = [],
    decisions = [],
    decisionsTask = null,
    decisionLoadError = "",
    capabilities = undefined,
    capabilitiesClient = null,
    audits = {},
    composeItems = [],
    composeItemsError = "",
    subscription = null;
  let messageScrollActive = false,
    messageTouchActive = false,
    messageScrollTimer = null,
    renderHeldForMessageScroll = false,
    ignoredScrollElement = null,
    ignoredScrollTop = 0;
  // View-local choices never write to the hub or vault. A changed client or
  // project lifecycle starts a fresh disclosure epoch.
  let teamClient = null;
  const teamChoices = new Map();
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
  const sending = new Set();
  const decisionSending = new Set();
  const decisionErrors = new Map();
  const revealedAnswers = new Set();
  const decisionDrafts = createDecisionDrafts();
  const decisionPresentation = new Map();
  const completeConversations = new Map();
  const drafts = new Map();
  const uncertain = new Map();
  const restoredIntents = new Set();
  const intentScopes = new Map();
  const auditEditors = new Map();
  const auditHistories = new Map();
  const auditSending = new Set();
  const pendingTokens = new Set();
  const reloadAgainTokens = new Set();
  const persistence = intentPersistence || {
    list: async () => [],
    save: async () => {},
    remove: async () => {},
  };
  const clientIdentity = (taskId, actionClient = client()) =>
    JSON.stringify([
      actionClient?.base || "",
      actionClient?.token || "",
      taskId,
    ]);
  const auditEditorKey = (taskId, seq, actionClient = client()) =>
    `${clientIdentity(taskId, actionClient)}:${seq}`;
  const uncertainKey = (taskId, actionClient = client()) =>
    clientIdentity(taskId, actionClient);
  const draftKey = uncertainKey;
  const sameClient = (actionClient) => {
    const current = client();
    return (
      current === actionClient ||
      (current &&
        actionClient &&
        !current.base &&
        !current.token &&
        !actionClient.base &&
        !actionClient.token)
    );
  };
  const currentAction = (taskId, token, actionClient) =>
    visible &&
    selected === taskId &&
    epoch === token &&
    sameClient(actionClient);
  async function intentScope(taskId, actionClient = client()) {
    const identity = JSON.stringify([
      actionClient?.base || "",
      actionClient?.token || "",
      taskId,
    ]);
    if (!intentScopes.has(identity))
      intentScopes.set(
        identity,
        crypto.subtle
          .digest("SHA-256", new TextEncoder().encode(identity))
          .then((bytes) =>
            [...new Uint8Array(bytes)]
              .map((byte) => byte.toString(16).padStart(2, "0"))
              .join(""),
          ),
      );
    return intentScopes.get(identity);
  }
  async function restoreIntents(taskId, actionClient = client()) {
    if (!taskId) return;
    const scope = await intentScope(taskId, actionClient);
    if (restoredIntents.has(scope)) return;
    const records = await persistence.list(scope);
    const savedDraft = records.find(
      (record) => record.state === "draft" && record.id === `draft:${taskId}`,
    );
    if (savedDraft?.values)
      drafts.set(draftKey(taskId, actionClient), {
        ...emptyDraft(),
        ...savedDraft.values,
      });
    for (const record of records.filter(
      (entry) => entry.state === "draft" && entry.id.startsWith("editor:"),
    )) {
      const seq = Number(record.id.slice("editor:".length));
      if (seq > 0 && record.values)
        auditEditors.set(auditEditorKey(taskId, seq, actionClient), {
          ...record.values,
          taskId,
          seq,
          generation: record.values.generation || crypto.randomUUID(),
        });
    }
    uncertain.set(
      uncertainKey(taskId, actionClient),
      records.filter((record) => record.state === "uncertain"),
    );
    restoredIntents.add(scope);
  }
  async function persistDraft(taskId, value, actionClient = client()) {
    if (!taskId) return;
    const scope = await intentScope(taskId, actionClient);
    await persistence.save({
      scope,
      id: `draft:${taskId}`,
      state: "draft",
      requestId: value.requestId || "",
      values: structuredClone(value),
      updatedAt: new Date().toISOString(),
    });
  }
  async function persistAuditEditor(
    taskId,
    seq,
    value,
    actionClient = client(),
  ) {
    const scope = await intentScope(taskId, actionClient);
    await persistence.save({
      scope,
      id: `editor:${seq}`,
      state: "draft",
      requestId: value.requestId || "",
      values: structuredClone(value),
      updatedAt: new Date().toISOString(),
    });
  }
  async function removeAuditEditor(taskId, seq, actionClient = client()) {
    await persistence.remove(
      await intentScope(taskId, actionClient),
      `editor:${seq}`,
    );
    auditEditors.delete(auditEditorKey(taskId, seq, actionClient));
  }
  const auditEditorMatches = (editor, record) =>
    Boolean(
      editor &&
      record?.editorGeneration &&
      editor.generation === record.editorGeneration &&
      editor.requestId === record.requestId,
    );
  async function clearConfirmedAuditEditor(
    taskId,
    seq,
    record,
    actionClient = client(),
  ) {
    const key = auditEditorKey(taskId, seq, actionClient);
    if (!auditEditorMatches(auditEditors.get(key), record)) return false;
    await persistence.remove(
      await intentScope(taskId, actionClient),
      `editor:${seq}`,
    );
    const current = auditEditors.get(key);
    if (!auditEditorMatches(current, record)) {
      if (current) await persistAuditEditor(taskId, seq, current, actionClient);
      return false;
    }
    auditEditors.delete(key);
    return true;
  }
  async function retainUncertain(
    taskId,
    operation,
    requestId,
    payload,
    seq = 0,
    actionClient = client(),
    editorGeneration = "",
  ) {
    const scope = await intentScope(taskId, actionClient);
    const record = {
      scope,
      id: `uncertain:${operation}:${requestId}`,
      state: "uncertain",
      operation,
      requestId,
      seq,
      ...(editorGeneration ? { editorGeneration } : {}),
      payload: structuredClone(payload),
      updatedAt: new Date().toISOString(),
    };
    await persistence.save(record);
    const key = uncertainKey(taskId, actionClient);
    uncertain.set(key, [
      ...(uncertain.get(key) || []).filter((entry) => entry.id !== record.id),
      record,
    ]);
  }
  async function removeIntent(taskId, record, actionClient = client()) {
    await persistence.remove(
      await intentScope(taskId, actionClient),
      record.id,
    );
    const key = uncertainKey(taskId, actionClient);
    uncertain.set(
      key,
      (uncertain.get(key) || []).filter((entry) => entry.id !== record.id),
    );
  }
  function postIntentMatchesDraft(taskId, record, actionClient = client()) {
    const value = drafts.get(draftKey(taskId, actionClient));
    if (!value || value.requestId !== record.requestId) return false;
    try {
      const { requestId: _, ...attempted } = record.payload;
      return JSON.stringify(messageBody(value)) === JSON.stringify(attempted);
    } catch {
      return false;
    }
  }
  async function clearConfirmedPostDraft(
    taskId,
    record,
    actionClient = client(),
  ) {
    const key = draftKey(taskId, actionClient);
    if (!postIntentMatchesDraft(taskId, record, actionClient)) return false;
    await persistence.remove(
      await intentScope(taskId, actionClient),
      `draft:${taskId}`,
    );
    if (!postIntentMatchesDraft(taskId, record, actionClient)) {
      const current = drafts.get(key);
      if (current) await persistDraft(taskId, current, actionClient);
      return false;
    }
    drafts.set(key, { ...emptyDraft(), to: drafts.get(key)?.to || "" });
    if (visible && renderedTask === taskId && renderedClient === actionClient) {
      const input = root.querySelector("#board-text");
      if (input) input.value = "";
      const kind = root.querySelector("#board-audit-kind");
      if (kind) kind.value = "";
    }
    return true;
  }
  function messageBody(value) {
    const body = {
      text: value.text.trim(),
      to: value.to,
      replyTo: value.replyTo,
    };
    if (value.auditKind === "intake") {
      body.auditKind = "intake";
    } else if (value.auditKind === "item") {
      if (!value.primaryItem)
        throw new Error(
          "Choose a bug or feature before sending an item message.",
        );
      body.workItems = [
        parseItemReference(
          `${value.primaryTask}/${value.primaryItem}@${value.primaryRevision}`,
          "primary",
        ),
      ];
    } else if (value.auditKind === "work") {
      const primary = parseItemReference(
        `${value.primaryTask}/${value.primaryItem}@${value.primaryRevision}`,
        "primary",
      );
      const related = String(value.related || "")
        .split(/\n+/)
        .map((line) => line.trim())
        .filter(Boolean)
        .map((line) => parseItemReference(line, "related"));
      if (related.length > 15)
        throw new Error("At most 15 related items are allowed.");
      const seen = new Set([`${primary.itemTaskId}/${primary.itemId}`]);
      for (const link of related) {
        const identity = `${link.itemTaskId}/${link.itemId}`;
        if (seen.has(identity))
          throw new Error("Primary and related items must be distinct.");
        seen.add(identity);
      }
      const orderSeq = Number(value.orderSeq);
      if (!/^tsk_[0-9a-f]{16}$/.test(value.orderTask || "") || orderSeq < 1)
        throw new Error("Work messages require an exact order TASK#MESSAGE.");
      body.auditKind = "work";
      body.workItems = [primary, ...related];
      body.workOrderMessage = { taskId: value.orderTask, seq: orderSeq };
    }
    return body;
  }
  const beginMessage = (taskId, value, actionClient) => {
    const body = messageBody(value);
    const attemptedPayload = JSON.stringify(body);
    if (
      !value.requestId ||
      (value.attemptedPayload && value.attemptedPayload !== attemptedPayload)
    )
      value.requestId = crypto.randomUUID();
    value.attemptedPayload = attemptedPayload;
    drafts.set(draftKey(taskId, actionClient), value);
    return { requestId: value.requestId, ...body };
  };
  const presentation = createViewRefreshPresentation({
    render: () => {
      if (visible && !pendingTokens.size) render();
    },
  });
  function flushHeldMessageRender() {
    if (
      !renderHeldForMessageScroll ||
      messageScrollActive ||
      !visible ||
      pendingTokens.size
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
  const draft = (taskId = selected, actionClient = client()) =>
    drafts.get(draftKey(taskId, actionClient)) || emptyDraft();
  function saveDraft() {
    if (!root?.querySelector("#board-text")) return;
    const actionClient = renderedClient || client();
    const key = draftKey(renderedTask, actionClient);
    drafts.set(key, {
      ...(drafts.get(key) || emptyDraft()),
      text: root.querySelector("#board-text").value,
      to: root.querySelector("#board-to").value,
      auditKind:
        root.querySelector("#board-audit-kind")?.value ??
        drafts.get(key)?.auditKind ??
        "",
      primaryTask:
        root.querySelector("#board-primary-task")?.value ??
        drafts.get(key)?.primaryTask ??
        "",
      primaryItem:
        root.querySelector("#board-primary-item")?.value ??
        drafts.get(key)?.primaryItem ??
        "",
      primaryRevision:
        root.querySelector("#board-primary-revision")?.value ??
        drafts.get(key)?.primaryRevision ??
        "",
      related: root.querySelector("#board-related")?.value || "",
      orderTask: root.querySelector("#board-order-task")?.value || "",
      orderSeq: root.querySelector("#board-order-seq")?.value || "",
    });
    void persistDraft(renderedTask, drafts.get(key), actionClient).catch(
      (error) => notice("Draft not saved: " + error.message),
    );
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
    interruptMessageScroll();
    presentation.interrupt({ capture: wasVisible });
  }
  async function show(taskId, itemContext) {
    saveDraft();
    visible = true;
    if (taskId && taskId !== selected) {
      interruptMessageScroll();
      saveDraft();
      saveDecisionDrafts();
      saveDecisionPresentation();
      selected = taskId;
    }
    const token = ++epoch;
    subscription?.stop();
    subscription = null;
    if (teamClient !== client()) {
      teamClient = client();
      teamChoices.clear();
    }
    if (!client()) {
      presentation.interrupt();
      root.innerHTML =
        '<div class="mode-empty"><span class="eyebrow">BOARD</span><h2>Connect a project hub.</h2><button id="board-configure" class="primary">Configure project hub</button></div>';
      root.querySelector("#board-configure").onclick = configure;
      return;
    }
    const actionClient = client();
    const requestedContext = itemContext && { ...itemContext };
    const loaded = await reload(token, () => {
      if (sameClient(actionClient)) void show(taskId, requestedContext);
    });
    // A failed or superseded load must not apply direct context to old detail.
    if (!loaded || !currentAction(loaded.id, token, actionClient)) return;
    if (
      itemContext &&
      itemContext.taskId === selected &&
      detail?.task.id === selected &&
      detail?.task.status === "open"
    ) {
      interruptMessageScroll();
      const value = {
        ...draft(selected, actionClient),
        auditKind: "item",
        primaryTask: itemContext.taskId,
        primaryItem: itemContext.id,
        primaryRevision: String(itemContext.revision),
        itemTitle: itemContext.title,
        itemDirect: true,
        related: "",
        orderTask: "",
        orderSeq: "",
      };
      drafts.set(draftKey(selected, actionClient), value);
      presentation.interrupt();
      render(false);
      root.querySelector("#board-text")?.focus({ preventScroll: true });
      await persistDraft(taskId, value, actionClient).catch((error) =>
        notice("Draft not saved: " + error.message),
      );
      if (!currentAction(taskId, token, actionClient)) return;
    }
    subscription?.stop();
    subscription = client().subscribe("", () => reload(epoch), {
      after: 0,
      onError: () => {},
    });
  }
  async function reload(token = epoch, retry = () => show()) {
    if (!visible) return;
    if (pendingTokens.has(token)) {
      reloadAgainTokens.add(token);
      return;
    }
    reloadAgainTokens.delete(token);
    pendingTokens.add(token);
    const actionClient = client();
    try {
      const list = await actionClient.listTasks();
      if (!visible || token !== epoch || !sameClient(actionClient)) return;
      tasks = list;
      if (!tasks.some((t) => t.id === selected)) {
        saveDraft();
        selected = tasks.find((t) => t.status === "open")?.id || null;
      }
      const id = selected;
      await restoreIntents(id, actionClient).catch((error) =>
        notice("Saved Board intents unavailable: " + error.message),
      );
      if (!currentAction(id, token, actionClient)) return;
      let loadedCapabilities =
        capabilitiesClient === actionClient ? capabilities : undefined;
      if (loadedCapabilities === undefined) {
        if (!actionClient.capabilities) loadedCapabilities = null;
        else {
          try {
            loadedCapabilities = await actionClient.capabilities();
          } catch (error) {
            if (error.status === 404) loadedCapabilities = null;
            else throw error;
          }
        }
      }
      if (!currentAction(id, token, actionClient)) return;
      if (tasks.find((t) => t.id === id)?.status === "open")
        completeConversations.delete(id);
      const result = id
        ? await Promise.all([
            actionClient.getTask(id),
            completeConversations.has(id)
              ? Promise.resolve(completeConversations.get(id))
              : actionClient.listMessages(id, { limit: 200, latest: 1 }),
            readAllDecisions(actionClient, id).then(
              (value) => ({ value }),
              (error) => ({ error }),
            ),
          ])
        : [null, [], { value: [] }];
      if (!currentAction(id, token, actionClient)) return;
      const loadedDetail = result[0];
      if (id && loadedDetail?.task.id !== id)
        throw new Error("Loaded project does not match the selected project.");
      const loadedMessages = result[1];
      let loadedAudits = {};
      if (loadedCapabilities?.messageAudit?.versions?.includes(2)) {
        if (actionClient.loadMessageAudits)
          loadedAudits = await actionClient.loadMessageAudits(
            id,
            loadedMessages,
          );
        else {
          const records = await Promise.all(
            loadedMessages.map((message) =>
              actionClient.getMessageAudit(id, message.seq),
            ),
          );
          loadedAudits = Object.fromEntries(
            records.map((record) => [String(record.message.seq), record]),
          );
        }
      }
      let loadedItems = [],
        itemsError = "";
      if (id && actionClient.listWorkItems) {
        try {
          let after = 0;
          do {
            // Eligibility must come from the hub, not a saved read-cache snapshot.
            const page = actionClient.request
              ? await actionClient.request(
                  `/v1/work-items?${new URLSearchParams({ taskId: id, status: "in_progress", after, limit: 200 })}`,
                  { timeoutMs: 15000 },
                )
              : await actionClient.listWorkItems({
                  taskId: id,
                  status: "in_progress",
                  after,
                  limit: 200,
                });
            if (!currentAction(id, token, actionClient)) return;
            loadedItems.push(
              ...page.items.filter(
                (item) => item.taskId === id && item.status === "in_progress",
              ),
            );
            if (!page.next) break;
            if (page.next <= after)
              throw new Error("Item list did not advance.");
            after = page.next;
          } while (true);
        } catch (error) {
          itemsError = error.message;
          loadedItems = [];
        }
      }
      if (!currentAction(id, token, actionClient)) return;
      composeItems = loadedItems;
      composeItemsError = itemsError;
      capabilities = loadedCapabilities;
      capabilitiesClient = actionClient;
      detail = loadedDetail;
      messages = loadedMessages;
      audits = loadedAudits;
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
      return { id };
    } catch (e) {
      if (visible && token === epoch && sameClient(actionClient)) {
        saveDraft();
        interruptMessageScroll();
        presentation.interrupt();
        root.innerHTML = `<div class="mode-empty"><h2>Hub unavailable</h2><p>${esc(e.message)}</p><button id="board-retry">Retry connection</button></div>`;
        root.querySelector("#board-retry").onclick = retry;
      }
    } finally {
      pendingTokens.delete(token);
      if (reloadAgainTokens.delete(token) && visible && token === epoch)
        void reload(token);
      else if (token === epoch) flushHeldMessageRender();
    }
  }
  function render(captureDraft = true) {
    if (!visible) return;
    if (renderedTask !== selected) interruptMessageScroll();
    // A send owns the visible compose controls until it settles. Defer a
    // background refresh so it cannot replace the disabled form mid-request.
    if (sending.has(selected) && renderedTask === selected) return;
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
    if (captureDraft && !sending.has(renderedTask)) saveDraft();
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
    const auditSupported = capabilities?.messageAudit?.versions?.includes(2);
    const exportSupported = capabilities?.auditExport?.versions?.includes(2);
    const renderAuditEditor = (message) => {
      const editor = auditEditors.get(
        auditEditorKey(selected, message.seq, client()),
      );
      if (!editor) return "";
      const busy = auditSending.has(
        auditEditorKey(selected, message.seq, client()),
      );
      if (editor.mode === "resolve")
        return `<form class="message-audit-editor" data-audit-resolve="${message.seq}"><strong>Resolve Intake · pinned r${editor.expectedRevision}</strong><label>Target<select name="target"><option value="existing" ${editor.target !== "new" ? "selected" : ""}>Existing item</option><option value="new" ${editor.target === "new" ? "selected" : ""}>New item</option></select></label><div class="audit-fields"><label>Existing item<input name="existing" value="${esc(editor.existing || "")}" placeholder="wi_…"></label><label>Revision<input name="existingRevision" type="number" min="1" value="${esc(editor.existingRevision || "")}"></label><label>New kind<select name="kind"><option value="bug" ${editor.kind !== "feature" ? "selected" : ""}>Bug</option><option value="feature" ${editor.kind === "feature" ? "selected" : ""}>Feature</option></select></label><label>New title<input name="title" maxlength="120" value="${esc(editor.title || "")}"></label></div><label>Reason<input name="reason" maxlength="2048" required value="${esc(editor.reason || "")}"></label><label>Exact sources<textarea name="sources" rows="2" required>${esc(editor.sources)}</textarea></label><p class="fine" role="alert">${esc(editor.error || "")}</p><div class="message-audit-actions"><button type="button" data-audit-cancel="${message.seq}">Cancel</button><button class="primary" type="submit" ${busy ? "disabled" : ""}>${busy ? "Resolving…" : "Resolve"}</button></div></form>`;
      return `<form class="message-audit-editor" data-audit-correct="${message.seq}"><strong>Correct current context · pinned r${editor.expectedRevision}</strong><label>Classification<select name="classification"><option value="intake" ${editor.classification === "intake" ? "selected" : ""}>Intake</option><option value="work" ${editor.classification === "work" ? "selected" : ""}>Work</option></select></label><div class="audit-fields"><label>Primary<input name="primary" value="${esc(editor.primary || "")}" placeholder="tsk_…/wi_…@revision"></label><label>Related<textarea name="related" rows="2" placeholder="One TASK/ITEM@REVISION per line">${esc(editor.related || "")}</textarea></label></div><label>Reason<input name="reason" maxlength="2048" required value="${esc(editor.reason || "")}"></label><label>Exact sources<textarea name="sources" rows="2" required>${esc(editor.sources)}</textarea></label><p class="fine" role="alert">${esc(editor.error || "")}</p><div class="message-audit-actions"><button type="button" data-audit-cancel="${message.seq}">Cancel</button><button class="primary" type="submit" ${busy ? "disabled" : ""}>${busy ? "Saving…" : "Save correction"}</button></div></form>`;
    };
    const auditHistoryMarkup = (record, history) => {
      if (!history) return "";
      return `<div class="message-audit-history"><strong>Audit history · original ${esc(auditProjectionText(record?.original))}</strong>${history.events
        .map((event) => {
          const actor = event.actor?.agentId
            ? `${event.actor.agentId}${event.actor.runId ? `/${event.actor.runId}` : ""}`
            : `${event.actor?.caller?.node || "unknown"}/${event.actor?.caller?.user || "unknown"}`;
          const sources = (event.sources || [])
            .map((source) => `${source.taskId}#${source.seq}`)
            .join(" · ");
          return `<details><summary>r${event.revision} · ${esc(event.operation)} · ${esc(event.reason || event.provenance)}</summary><span>Before: ${esc(auditProjectionText(event.before))}</span><span>After: ${esc(auditProjectionText(event.after))}</span><span>Actor: ${esc(actor)}${event.provenance ? ` · ${esc(event.provenance)}` : ""}</span><span>Sources: ${esc(sources || "none")}</span></details>`;
        })
        .join("")}</div>`;
    };
    const messageMarkup = messages
      .map((message) => {
        const record = audits[String(message.seq)];
        const history = auditHistories.get(
          auditEditorKey(selected, message.seq, client()),
        );
        const currentKind = record?.current?.classification || "unknown";
        const auditActions = !auditSupported
          ? ""
          : !record?.original
            ? `<button data-audit-retry="${message.seq}">Retry audit</button>`
            : `<button data-audit-history="${message.seq}">History</button>${archived ? "" : `<button data-audit-correct-open="${message.seq}">Correct</button>${currentKind === "intake" ? `<button data-audit-resolve-open="${message.seq}">Resolve</button>` : ""}`}`;
        return `<article class="board-message" data-message="${message.seq}"><div class="board-meta"><strong>${esc(name(message))}</strong><span>${message.to ? "to " + esc(names.get(message.to) || message.to) : "Team announcement"}${message.broadcast ? " · Swarm broadcast" : ""} · ${esc(archived ? new Date(message.createdAt).toLocaleString() : new Date(message.createdAt).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" }))}</span></div>${message.replyTo ? `<div class="reply-context">Reply to #${message.replyTo}: ${esc(messages.find((candidate) => candidate.seq === message.replyTo)?.text.slice(0, 100) || "Earlier message")}</div>` : ""}<div class="board-text">${esc(message.text)}</div>${auditSupported ? auditContextMarkup(record) : ""}<div class="message-footer"><span>${receipt(message)}</span>${archived ? "" : `<button data-reply="${message.seq}">Reply</button>`}${auditActions}</div>${auditHistoryMarkup(record, history)}${archived ? "" : renderAuditEditor(message)}</article>`;
      })
      .join("");
    const recoveryMarkup = (
      uncertain.get(uncertainKey(selected, client())) || []
    )
      .map(
        (record) =>
          `<div class="board-intent-recovery" data-intent="${esc(record.id)}"><span>Uncertain ${esc(record.operation)} · ${esc(record.requestId)}</span><button data-intent-recover="${esc(record.id)}">Recover receipt</button><button data-intent-retry="${esc(record.id)}">Retry exact intent</button><button data-intent-discard="${esc(record.id)}">Discard</button></div>`,
      )
      .join("");
    const auditComposer = auditSupported
      ? `<details class="board-audit-compose" ${d.auditKind && d.auditKind !== "item" ? "open" : ""}><summary>Audit context · ${esc(d.auditKind || "ordinary")}</summary><label>Classification<select id="board-audit-kind"><option value="" ${!d.auditKind ? "selected" : ""}>Ordinary / unclassified</option><option value="item" ${d.auditKind === "item" ? "selected" : ""}>Item message</option><option value="work" ${d.auditKind === "work" ? "selected" : ""}>Work</option><option value="intake" ${d.auditKind === "intake" ? "selected" : ""}>Intake</option></select></label>${d.auditKind === "work" ? `<div class="audit-fields"><label>Primary project<input id="board-primary-task" value="${esc(d.primaryTask || selected)}" placeholder="tsk_…"></label><label>Primary item<input id="board-primary-item" value="${esc(d.primaryItem)}" placeholder="wi_…"></label><label>Revision<input id="board-primary-revision" type="number" min="1" value="${esc(d.primaryRevision)}"></label><label>Order project<input id="board-order-task" value="${esc(d.orderTask || selected)}" placeholder="tsk_…"></label><label>Order message<input id="board-order-seq" type="number" min="1" value="${esc(d.orderSeq)}"></label></div><label>Related exact items<textarea id="board-related" rows="2" placeholder="One TASK/ITEM@REVISION per line">${esc(d.related)}</textarea></label>` : ""}<p class="fine">Typed context is submitted exactly as shown. Replies only propose context; sending is the deliberate choice.</p></details>`
      : "";
    const selectedItem = composeItems.find(
      (item) =>
        item.id === d.primaryItem &&
        String(item.revision) === String(d.primaryRevision),
    );
    const itemComposer =
      auditSupported || d.auditKind === "item"
        ? d.auditKind === "item"
          ? `<div class="board-item-compose"><label>Bug or feature<select id="board-message-item" data-view-control="message-item"><option value="">${d.primaryItem ? "Choose another in-progress item…" : "Choose an in-progress item…"}</option>${composeItems.map((item) => `<option value="${esc(item.id)}@${item.revision}" ${selectedItem?.id === item.id ? "selected" : ""}>${esc(item.kind === "bug" ? "Bug" : "Feature")} · ${esc(item.title)} · r${item.revision}</option>`).join("")}</select></label><p class="fine" role="status">${esc(d.primaryItem ? `${d.itemTitle || d.primaryItem} · ${d.primaryTask}/${d.primaryItem}@${d.primaryRevision}${!selectedItem ? (d.itemDirect ? " · Selected from item page; checked when sending." : " · Selection changed or is no longer in progress. Choose the current item explicitly.") : ""}` : "Select a bug or feature for this message.")}${composeItemsError ? ` · ${esc(composeItemsError)}` : ""}</p></div>`
          : '<div class="board-item-compose"><button type="button" id="board-message-item-mode">Message about a bug or feature</button></div>'
        : "";
    const roster = agents.filter((a) => archived || a.status !== "closed");
    const rosterAgent = (a) =>
      `<button class="board-agent" ${archived ? "disabled" : `data-board-agent="${esc(a.id)}"`} title="${esc(a.blockedText || a.host + " · " + a.session)}"><span class="status-dot ${a.status === "running" ? "online" : a.status === "needs_input" ? "attention" : ""}"></span>${esc(a.name)}<span class="fine">${esc(status(a))}</span></button>`;
    const headActions = `${exportSupported ? '<button id="board-audit-export">Export audit</button>' : '<button id="board-download">Legacy JSON · non-snapshot</button>'}${archived ? `<span class="fine">Closed · ${esc(new Date(detail.task.closedAt).toLocaleDateString())}</span>${exportSupported ? '<button id="board-download">Legacy JSON · non-snapshot</button>' : ""}` : '<button id="board-attach">Terminals</button><button id="board-settings" title="Project settings">Settings</button>'}`;
    renderedTask = selected;
    renderedClient = client();
    root.innerHTML = `<div class="board mode-board"><aside class="board-rail"><div class="board-rail-head"><span class="eyebrow">PROJECTS</span><button id="board-new-task" title="New project">＋</button></div>${tasks
      .filter((t) => t.status === "open")
      .map(taskButton)
      .join(
        "",
      )}${closedTasks.length ? `<details class="board-closed" data-view-disclosure="closed" ${archived ? "open" : ""}><summary>Closed projects · ${closedTasks.length}</summary>${closedTasks.map(taskButton).join("")}</details>` : ""}</aside><section class="board-thread">${
      detail
        ? `<div class="board-head"><div class="view-heading"><div><span class="eyebrow">BOARD</span><h2>${esc(detail.task.name)}</h2></div><div class="view-actions">${headActions}</div></div><p class="fine">${esc(detail.task.goal)}</p>${cacheFeedback(client()?.cacheStatus?.().label) ? `<span class="fine hub-sync-status" role="status">${esc(cacheFeedback(client()?.cacheStatus?.().label))}</span>` : ""}<div class="project-team-heading">${roster.length ? `<button type="button" class="project-team-toggle" data-team-toggle data-view-control="team" aria-label="${teamExpanded(detail.task, roster.length) ? "Hide team" : "Show team"}" aria-expanded="${teamExpanded(detail.task, roster.length)}" aria-controls="board-team-roster"><span class="project-team-chevron" aria-hidden="true">›</span><span>Team · ${roster.length}</span></button>` : ""}${archived ? "" : '<button id="board-add-agent">＋ Agent</button>'}</div>${
            roster.some((a) => a.status === "needs_input")
              ? `<div class="board-agents-row project-team-attention" aria-label="Needs you">${roster
                  .filter((a) => a.status === "needs_input")
                  .map(rosterAgent)
                  .join("")}</div>`
              : ""
          }<div id="board-team-roster" class="board-agents-row" data-team-roster ${teamExpanded(detail.task, roster.length) ? "" : "hidden"}>${roster
            .filter((a) => a.status !== "needs_input")
            .map(rosterAgent)
            .join(
              "",
            )}</div></div>${renderDecisionPanel({ records: decisions, taskId: selected, archived, name, drafts: decisionDrafts, sending: decisionSending, errors: decisionErrors, revealedAnswers, historyOpen: decisionPresentation.get(selected)?.historyOpen, loadError: decisionLoadError })}${recoveryMarkup}<div id="board-messages" class="board-messages">${messages.length === 200 && !completeConversations.has(selected) ? `<p class="fine">Latest 200 messages.${archived ? ' <button id="board-full-history">Show full conversation</button>' : " Full history remains on the hub."}</p>` : ""}${messageMarkup}</div>${
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
                  )}</select></label><textarea ${sending.has(selected) ? "disabled" : ""} id="board-text" rows="2" maxlength="8192" placeholder="Write a message…" aria-label="Message">${esc(d.text)}</textarea><button class="primary" type="submit" ${sending.has(selected) ? "disabled" : ""}>${sending.has(selected) ? "Sending…" : "Send"}</button>${itemComposer}${auditComposer}</form>`
          }`
        : ""
    }</section></div>`;
    presentation.afterRender(selected);
    bindTeamDisclosure(selected);
    root.querySelector("#board-new-task").onclick = () => newTask();
    root.querySelectorAll("[data-board-task]").forEach(
      (b) =>
        (b.onclick = () => {
          saveDraft();
          saveDecisionDrafts();
          saveDecisionPresentation();
          selected = b.dataset.boardTask;
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
          const record = audits[String(m.seq)];
          const projection = record?.current;
          const original = record?.original;
          const links = projection?.workItems || original?.workItems || [];
          const primary = links.find((link) => link.relationship === "primary");
          const related = links.filter(
            (link) => link.relationship === "related",
          );
          const previous = draft();
          drafts.set(draftKey(selected, client()), {
            ...previous,
            itemDirect: false,
            replyTo: m.seq,
            to: m.from.agentId || "",
            auditKind:
              (projection?.classification || original?.classification) ===
              "unclassified"
                ? ""
                : projection?.classification || original?.classification || "",
            primaryTask: primary?.itemTaskId || "",
            primaryItem: primary?.itemId || "",
            primaryRevision: primary?.itemRevision || "",
            related: related
              .map(
                (link) =>
                  `${link.itemTaskId}/${link.itemId}@${link.itemRevision}`,
              )
              .join("\n"),
            orderTask: original?.workOrderMessage?.taskId || "",
            orderSeq: original?.workOrderMessage?.seq || "",
          });
          if (previous.auditKind === "item") {
            Object.assign(draft(), {
              auditKind: "item",
              primaryTask: previous.primaryTask,
              primaryItem: previous.primaryItem,
              primaryRevision: previous.primaryRevision,
              itemTitle: previous.itemTitle,
              itemDirect: previous.itemDirect,
              related: "",
              orderTask: "",
              orderSeq: "",
            });
          }
          void persistDraft(selected, draft(), client()).catch((error) =>
            notice("Draft not saved: " + error.message),
          );
          interruptMessageScroll();
          presentation.interrupt();
          root.querySelector("#board-to").value = m.from.agentId || "";
          render(false);
          root.querySelector("#board-text").focus();
        }),
    );
    const captureAuditEditor = (form, taskId, seq, actionClient) => {
      const key = auditEditorKey(taskId, seq, actionClient);
      const editor = auditEditors.get(key);
      if (!editor) return null;
      const value = (name) => form.elements.namedItem(name)?.value || "";
      const updated = {
        ...editor,
        generation: crypto.randomUUID(),
        ...(editor.mode === "correct"
          ? {
              classification: value("classification"),
              primary: value("primary"),
              related: value("related"),
            }
          : {
              target: value("target"),
              existing: value("existing"),
              existingRevision: value("existingRevision"),
              kind: value("kind"),
              title: value("title"),
            }),
        reason: value("reason"),
        sources: value("sources"),
        error: "",
      };
      auditEditors.set(key, updated);
      void persistAuditEditor(taskId, seq, updated, actionClient).catch(
        (error) => notice("Audit editor not saved: " + error.message),
      );
      return updated;
    };
    root.querySelectorAll("[data-audit-retry]").forEach((button) => {
      button.onclick = async () => {
        const taskId = selected;
        const seq = Number(button.dataset.auditRetry);
        const actionClient = client();
        const actionEpoch = epoch;
        button.disabled = true;
        try {
          const record = await actionClient.getMessageAudit(taskId, seq);
          if (!currentAction(taskId, actionEpoch, actionClient)) return;
          audits[String(seq)] = record;
          render();
        } catch (error) {
          if (currentAction(taskId, actionEpoch, actionClient))
            notice("Audit context unavailable: " + error.message);
        } finally {
          if (button.isConnected) button.disabled = false;
        }
      };
    });
    root.querySelectorAll("[data-audit-history]").forEach((button) => {
      button.onclick = async () => {
        const taskId = selected;
        const seq = Number(button.dataset.auditHistory);
        const actionClient = client();
        const actionEpoch = epoch;
        button.disabled = true;
        try {
          const events = [];
          let cursor = "";
          let afterVersion = 0;
          for (;;) {
            const page = await actionClient.listMessageAuditHistory(
              taskId,
              seq,
              {
                ...(cursor ? { cursor } : { afterVersion }),
                limit: 64,
              },
            );
            if (!currentAction(taskId, actionEpoch, actionClient)) return;
            events.push(...page.events);
            if (!page.nextCursor) break;
            cursor = page.nextCursor;
          }
          auditHistories.set(auditEditorKey(taskId, seq, actionClient), {
            events,
          });
          render();
        } catch (error) {
          if (currentAction(taskId, actionEpoch, actionClient))
            notice("Audit history unavailable: " + error.message);
        } finally {
          if (button.isConnected) button.disabled = false;
        }
      };
    });
    root.querySelectorAll("[data-audit-correct-open]").forEach((button) => {
      button.onclick = () => {
        const taskId = selected;
        const seq = Number(button.dataset.auditCorrectOpen);
        const record = audits[String(seq)];
        const state = record?.current;
        const primary = state?.workItems?.find(
          (link) => link.relationship === "primary",
        );
        const editor = {
          mode: "correct",
          taskId,
          seq,
          expectedRevision: state?.revision || 0,
          classification: state?.classification || "intake",
          primary: primary
            ? `${primary.itemTaskId}/${primary.itemId}@${primary.itemRevision}`
            : "",
          related: (state?.workItems || [])
            .filter((link) => link.relationship === "related")
            .map(
              (link) =>
                `${link.itemTaskId}/${link.itemId}@${link.itemRevision}`,
            )
            .join("\n"),
          sources: `${taskId}#${seq}`,
          generation: crypto.randomUUID(),
        };
        const actionClient = client();
        auditEditors.set(auditEditorKey(taskId, seq, actionClient), editor);
        void persistAuditEditor(taskId, seq, editor, actionClient).catch(
          (error) => notice("Audit editor not saved: " + error.message),
        );
        render();
      };
    });
    root.querySelectorAll("[data-audit-resolve-open]").forEach((button) => {
      button.onclick = () => {
        const taskId = selected;
        const seq = Number(button.dataset.auditResolveOpen);
        const editor = {
          mode: "resolve",
          taskId,
          seq,
          expectedRevision: audits[String(seq)]?.current?.revision,
          target: "existing",
          kind: "bug",
          sources: `${taskId}#${seq}`,
          generation: crypto.randomUUID(),
        };
        const actionClient = client();
        auditEditors.set(auditEditorKey(taskId, seq, actionClient), editor);
        void persistAuditEditor(taskId, seq, editor, actionClient).catch(
          (error) => notice("Audit editor not saved: " + error.message),
        );
        render();
      };
    });
    root.querySelectorAll("[data-audit-cancel]").forEach((button) => {
      button.onclick = () => {
        const taskId = selected;
        const seq = Number(button.dataset.auditCancel);
        const actionClient = client();
        auditEditors.delete(auditEditorKey(taskId, seq, actionClient));
        void removeAuditEditor(taskId, seq, actionClient).catch((error) =>
          notice("Audit editor not removed: " + error.message),
        );
        render();
      };
    });
    root
      .querySelectorAll("[data-audit-correct],[data-audit-resolve]")
      .forEach((form) => {
        const taskId = selected;
        const seq = Number(
          form.dataset.auditCorrect || form.dataset.auditResolve,
        );
        const actionClient = client();
        form.oninput = form.onchange = () =>
          captureAuditEditor(form, taskId, seq, actionClient);
      });
    root.querySelectorAll("[data-audit-correct]").forEach((form) => {
      form.onsubmit = async (event) => {
        event.preventDefault();
        const taskId = selected;
        const seq = Number(form.dataset.auditCorrect);
        const actionClient = client();
        const actionEpoch = epoch;
        const key = auditEditorKey(taskId, seq, actionClient);
        if (auditSending.has(key)) return;
        let editor = captureAuditEditor(form, taskId, seq, actionClient);
        auditSending.add(key);
        try {
          const classification = editor.classification;
          const workItems = [];
          if (classification === "work") {
            workItems.push(parseItemReference(editor.primary, "primary"));
            for (const line of String(editor.related || "")
              .split(/\n+/)
              .map((value) => value.trim())
              .filter(Boolean))
              workItems.push(parseItemReference(line, "related"));
          }
          const attempted = JSON.stringify({
            expectedRevision: editor.expectedRevision,
            desired: { classification, workItems },
            reason: editor.reason,
            sources: parseSources(editor.sources),
          });
          if (editor.attemptedPayload && editor.attemptedPayload !== attempted)
            editor = { ...editor, requestId: "" };
          editor = {
            ...editor,
            requestId: editor.requestId || crypto.randomUUID(),
            attemptedPayload: attempted,
          };
          auditEditors.set(key, editor);
          await persistAuditEditor(taskId, seq, editor, actionClient);
          if (
            (uncertain.get(uncertainKey(taskId, actionClient)) || []).some(
              (entry) =>
                entry.operation === "correct" &&
                entry.seq === seq &&
                entry.requestId === editor.requestId,
            )
          )
            throw new Error(
              "This exact correction is uncertain. Recover, retry, or discard it before submitting again.",
            );
          const body = {
            requestId: editor.requestId,
            ...JSON.parse(attempted),
          };
          await retainUncertain(
            taskId,
            "correct",
            editor.requestId,
            body,
            seq,
            actionClient,
            editor.generation,
          );
          const result = await actionClient.correctMessageAudit(
            taskId,
            seq,
            body,
          );
          const saved = (
            uncertain.get(uncertainKey(taskId, actionClient)) || []
          ).find((entry) => entry.requestId === editor.requestId);
          if (saved) await removeIntent(taskId, saved, actionClient);
          await clearConfirmedAuditEditor(
            taskId,
            seq,
            saved || {
              requestId: editor.requestId,
              editorGeneration: editor.generation,
            },
            actionClient,
          );
          if (currentAction(taskId, actionEpoch, actionClient)) {
            audits[String(seq)] = result.state;
            render();
          }
        } catch (error) {
          editor = auditEditors.get(key) || editor;
          if (editor) {
            editor = { ...editor, error: error.message };
            auditEditors.set(key, editor);
            void persistAuditEditor(taskId, seq, editor, actionClient).catch(
              () => {},
            );
          }
          if (currentAction(taskId, actionEpoch, actionClient)) render();
        } finally {
          auditSending.delete(key);
          if (currentAction(taskId, actionEpoch, actionClient)) render();
        }
      };
    });
    root.querySelectorAll("[data-audit-resolve]").forEach((form) => {
      form.onsubmit = async (event) => {
        event.preventDefault();
        const taskId = selected;
        const seq = Number(form.dataset.auditResolve);
        const actionClient = client();
        const actionEpoch = epoch;
        const key = auditEditorKey(taskId, seq, actionClient);
        if (auditSending.has(key)) return;
        let editor = captureAuditEditor(form, taskId, seq, actionClient);
        auditSending.add(key);
        try {
          const intent = {
            expectedRevision: editor.expectedRevision,
            reason: editor.reason,
            sources: parseSources(editor.sources),
          };
          if (editor.target === "existing") {
            if (
              !/^wi_[0-9a-f]{16}$/.test(editor.existing || "") ||
              Number(editor.existingRevision) < 1
            )
              throw new Error(
                "Existing resolution needs an exact item and revision.",
              );
            intent.existingItem = {
              itemTaskId: taskId,
              itemId: editor.existing,
              itemRevision: Number(editor.existingRevision),
              relationship: "primary",
            };
          } else {
            if (!String(editor.title || "").trim())
              throw new Error("New item title is required.");
            intent.newItem = {
              expectedRevision: 0,
              kind: editor.kind,
              title: String(editor.title).trim(),
            };
          }
          const attempted = JSON.stringify(intent);
          if (editor.attemptedPayload && editor.attemptedPayload !== attempted)
            editor = { ...editor, requestId: "", newItemRequestId: "" };
          editor = {
            ...editor,
            requestId: editor.requestId || crypto.randomUUID(),
            newItemRequestId:
              editor.target === "new"
                ? editor.newItemRequestId || crypto.randomUUID()
                : "",
            attemptedPayload: attempted,
          };
          auditEditors.set(key, editor);
          await persistAuditEditor(taskId, seq, editor, actionClient);
          if (
            (uncertain.get(uncertainKey(taskId, actionClient)) || []).some(
              (entry) =>
                entry.operation === "resolve" &&
                entry.seq === seq &&
                entry.requestId === editor.requestId,
            )
          )
            throw new Error(
              "This exact resolution is uncertain. Recover, retry, or discard it before submitting again.",
            );
          const body = { requestId: editor.requestId, ...intent };
          if (body.newItem) body.newItem.requestId = editor.newItemRequestId;
          await retainUncertain(
            taskId,
            "resolve",
            editor.requestId,
            body,
            seq,
            actionClient,
            editor.generation,
          );
          const result = await actionClient.resolveMessageAudit(
            taskId,
            seq,
            body,
          );
          const saved = (
            uncertain.get(uncertainKey(taskId, actionClient)) || []
          ).find((entry) => entry.requestId === editor.requestId);
          if (saved) await removeIntent(taskId, saved, actionClient);
          await clearConfirmedAuditEditor(
            taskId,
            seq,
            saved || {
              requestId: editor.requestId,
              editorGeneration: editor.generation,
            },
            actionClient,
          );
          if (currentAction(taskId, actionEpoch, actionClient)) {
            audits[String(seq)] = result.state;
            render();
          }
        } catch (error) {
          editor = auditEditors.get(key) || editor;
          if (editor) {
            editor = { ...editor, error: error.message };
            auditEditors.set(key, editor);
            void persistAuditEditor(taskId, seq, editor, actionClient).catch(
              () => {},
            );
          }
          if (currentAction(taskId, actionEpoch, actionClient)) render();
        } finally {
          auditSending.delete(key);
          if (currentAction(taskId, actionEpoch, actionClient)) render();
        }
      };
    });
    const cancel = root.querySelector("#board-cancel-reply");
    if (cancel)
      cancel.onclick = () => {
        saveDraft();
        drafts.set(draftKey(selected, client()), { ...draft(), replyTo: 0 });
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
    const findIntent = (taskId, actionClient, id) =>
      (uncertain.get(uncertainKey(taskId, actionClient)) || []).find(
        (record) => record.id === id,
      );
    const downloadAuditResult = (result) => {
      const name =
        result.document.streams.task?.[0]?.name
          ?.replace(/[^a-zA-Z0-9_-]+/g, "-")
          .slice(0, 80) || "project";
      downloadBlob(
        new Blob([result.bytes], { type: "application/json" }),
        `${name}-audit-v2.json`,
      );
    };
    root.querySelectorAll("[data-intent-discard]").forEach((button) => {
      button.onclick = async () => {
        const taskId = selected;
        const actionClient = client();
        const actionEpoch = epoch;
        const record = findIntent(
          taskId,
          actionClient,
          button.dataset.intentDiscard,
        );
        if (!record) return;
        try {
          await removeIntent(taskId, record, actionClient);
          if (currentAction(taskId, actionEpoch, actionClient)) render();
        } catch (error) {
          if (currentAction(taskId, actionEpoch, actionClient))
            notice("Intent not discarded: " + error.message);
        }
      };
    });
    root.querySelectorAll("[data-intent-recover]").forEach((button) => {
      button.onclick = async () => {
        const taskId = selected;
        const actionClient = client();
        const actionEpoch = epoch;
        const record = findIntent(
          taskId,
          actionClient,
          button.dataset.intentRecover,
        );
        if (!record) return;
        button.disabled = true;
        try {
          let result;
          if (record.operation === "post")
            result = await actionClient.getMessagePostReceipt(
              taskId,
              record.requestId,
            );
          else if (record.operation === "export") {
            const metadata = await actionClient.createAuditExport(taskId, {
              requestId: record.requestId,
              formatVersion: 2,
            });
            await retainUncertain(
              taskId,
              "export",
              record.requestId,
              { ...record.payload, metadata },
              0,
              actionClient,
            );
            if (currentAction(taskId, actionEpoch, actionClient)) {
              notice(
                "Export receipt recovered. Retry the same frozen export to download it.",
              );
              render();
            }
            return;
          } else
            result = await actionClient.getMessageAuditReceipt(
              taskId,
              record.requestId,
              {
                operation: record.operation,
              },
            );
          if (
            currentAction(taskId, actionEpoch, actionClient) &&
            result?.state &&
            record.seq
          )
            audits[String(record.seq)] = result.state;
          await removeIntent(taskId, record, actionClient);
          if (record.operation === "post")
            await clearConfirmedPostDraft(taskId, record, actionClient);
          else if (["correct", "resolve"].includes(record.operation))
            await clearConfirmedAuditEditor(
              taskId,
              record.seq,
              record,
              actionClient,
            );
          if (currentAction(taskId, actionEpoch, actionClient)) {
            notice("Committed intent recovered without creating a duplicate.");
            await reload(actionEpoch);
          }
        } catch (error) {
          if (currentAction(taskId, actionEpoch, actionClient))
            notice("Receipt not recovered: " + error.message);
        } finally {
          if (button.isConnected) button.disabled = false;
        }
      };
    });
    root.querySelectorAll("[data-intent-retry]").forEach((button) => {
      button.onclick = async () => {
        const taskId = selected;
        const actionClient = client();
        const actionEpoch = epoch;
        const record = findIntent(
          taskId,
          actionClient,
          button.dataset.intentRetry,
        );
        if (!record) return;
        button.disabled = true;
        try {
          let result;
          if (record.operation === "post")
            result = await actionClient.postMessage(taskId, record.payload);
          else if (record.operation === "correct")
            result = await actionClient.correctMessageAudit(
              taskId,
              record.seq,
              record.payload,
            );
          else if (record.operation === "resolve")
            result = await actionClient.resolveMessageAudit(
              taskId,
              record.seq,
              record.payload,
            );
          else if (record.operation === "export") {
            result = await readProjectAuditExport(actionClient, taskId, {
              requestId: record.requestId,
              metadata: record.payload.metadata,
              onMetadata: async (metadata) =>
                retainUncertain(
                  taskId,
                  "export",
                  record.requestId,
                  { ...record.payload, metadata },
                  0,
                  actionClient,
                ),
            });
            downloadAuditResult(result);
          } else throw new Error("Unknown saved Board intent operation.");
          if (
            currentAction(taskId, actionEpoch, actionClient) &&
            result?.state &&
            record.seq
          )
            audits[String(record.seq)] = result.state;
          await removeIntent(taskId, record, actionClient);
          if (record.operation === "post")
            await clearConfirmedPostDraft(taskId, record, actionClient);
          else if (["correct", "resolve"].includes(record.operation))
            await clearConfirmedAuditEditor(
              taskId,
              record.seq,
              record,
              actionClient,
            );
          if (currentAction(taskId, actionEpoch, actionClient)) {
            notice("Exact intent confirmed.");
            await reload(actionEpoch);
          }
        } catch (error) {
          if (currentAction(taskId, actionEpoch, actionClient))
            notice("Exact retry failed: " + error.message);
        } finally {
          if (button.isConnected) button.disabled = false;
        }
      };
    });
    const auditExportButton = root.querySelector("#board-audit-export");
    if (auditExportButton)
      auditExportButton.onclick = async () => {
        const taskId = selected;
        const actionClient = client();
        const actionEpoch = epoch;
        auditExportButton.disabled = true;
        try {
          if (
            (uncertain.get(uncertainKey(taskId, actionClient)) || []).some(
              (entry) => entry.operation === "export",
            )
          )
            throw new Error(
              "Recover, retry, or discard the uncertain export before creating a replacement.",
            );
          const requestId = crypto.randomUUID();
          await retainUncertain(
            taskId,
            "export",
            requestId,
            { requestId, formatVersion: 2 },
            0,
            actionClient,
          );
          const result = await readProjectAuditExport(actionClient, taskId, {
            requestId,
            onMetadata: async (metadata) =>
              retainUncertain(
                taskId,
                "export",
                requestId,
                { requestId, formatVersion: 2, metadata },
                0,
                actionClient,
              ),
          });
          downloadAuditResult(result);
          const record = findIntent(
            taskId,
            actionClient,
            `uncertain:export:${requestId}`,
          );
          if (record) await removeIntent(taskId, record, actionClient);
          if (currentAction(taskId, actionEpoch, actionClient))
            notice(`Audit export verified · ${result.metadata.sha256}`);
        } catch (error) {
          if (currentAction(taskId, actionEpoch, actionClient)) {
            notice("Audit export unavailable: " + error.message);
            render();
          }
        } finally {
          if (auditExportButton.isConnected) auditExportButton.disabled = false;
        }
      };
    for (const [selector, download] of [
      ["#board-download", true],
      ["#board-full-history", false],
    ]) {
      const button = root.querySelector(selector);
      if (!button) continue;
      button.onclick = async () => {
        const id = selected,
          token = epoch,
          actionClient = client();
        button.disabled = true;
        try {
          const history = await readTaskHistory(actionClient, id);
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
          } else if (currentAction(id, token, actionClient)) {
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
          body = { ...draft() },
          actionClient = client(),
          actionEpoch = epoch;
        if (!body.text.trim() || sending.has(id)) return;
        const submittedInput = form.querySelector("#board-text");
        const submittedSelection =
          document.activeElement === submittedInput
            ? [submittedInput.selectionStart, submittedInput.selectionEnd]
            : null;
        let failed = false;
        sending.add(id);
        const recipient = form.querySelector("#board-to");
        const button = form.querySelector("[type=submit]");
        submittedInput.disabled = true;
        recipient.disabled = true;
        button.disabled = true;
        try {
          const message = beginMessage(id, body, actionClient);
          await persistDraft(id, body, actionClient);
          const exactRetry = (
            uncertain.get(uncertainKey(id, actionClient)) || []
          ).some(
            (entry) =>
              entry.operation === "post" &&
              JSON.stringify(entry.payload) === JSON.stringify(message),
          );
          if (body.auditKind === "item" && !exactRetry) {
            if (!capabilities?.messageAudit?.versions?.includes(2))
              throw new Error(
                "Item messaging requires an available audit-capable hub.",
              );
            if (body.primaryTask !== id)
              throw new Error("The selected item belongs to another project.");
            const current = actionClient.request
              ? await actionClient.request(
                  `/v1/tasks/${id}/work-items/${body.primaryItem}`,
                  { timeoutMs: 15000 },
                )
              : await actionClient.getWorkItem(id, body.primaryItem);
            if (current.revision !== Number(body.primaryRevision))
              throw new Error(
                `Selected revision ${body.primaryRevision} is stale; current revision is ${current.revision}. Choose the current item explicitly. Your text is retained.`,
              );
            if (
              !(
                body.itemDirect
                  ? ["open", "blocked", "in_progress"]
                  : ["in_progress"]
              ).includes(current.status)
            )
              throw new Error(
                "The selected item is no longer eligible. Your text is retained.",
              );
          }
          await retainUncertain(
            id,
            "post",
            message.requestId,
            message,
            0,
            actionClient,
          );
          await actionClient.postMessage(id, message);
          const saved = (
            uncertain.get(uncertainKey(id, actionClient)) || []
          ).find((entry) => entry.requestId === message.requestId);
          let cleared = false;
          if (saved) await removeIntent(id, saved, actionClient);
          cleared = await clearConfirmedPostDraft(
            id,
            saved || {
              requestId: message.requestId,
              payload: message,
            },
            actionClient,
          );
          if (currentAction(id, actionEpoch, actionClient)) {
            if (cleared) {
              root.querySelector("#board-text").value = "";
              const auditKind = root.querySelector("#board-audit-kind");
              if (auditKind) auditKind.value = "";
            }
            await reload(actionEpoch);
          }
        } catch (error) {
          failed = true;
          notice("Post failed: " + error.message);
        } finally {
          sending.delete(id);
          if (currentAction(id, actionEpoch, actionClient)) render(false);
          if (
            failed &&
            currentAction(id, actionEpoch, actionClient) &&
            submittedSelection
          ) {
            const restoredInput = root.querySelector("#board-text");
            restoredInput?.focus();
            restoredInput?.setSelectionRange(...submittedSelection);
          }
          if (submittedInput.isConnected) submittedInput.disabled = false;
          if (recipient.isConnected) recipient.disabled = false;
          if (button.isConnected) button.disabled = false;
        }
      };
      const itemMode = form.querySelector("#board-message-item-mode");
      if (itemMode)
        itemMode.onclick = () => {
          saveDraft();
          Object.assign(draft(), {
            auditKind: "item",
            primaryTask: selected,
            primaryItem: "",
            primaryRevision: "",
            itemTitle: "",
            itemDirect: false,
          });
          void persistDraft(selected, draft()).catch((error) =>
            notice("Draft not saved: " + error.message),
          );
          interruptMessageScroll();
          presentation.interrupt();
          render(false);
          root
            .querySelector("#board-message-item")
            ?.focus({ preventScroll: true });
        };
      const itemPicker = form.querySelector("#board-message-item");
      if (itemPicker)
        itemPicker.onchange = () => {
          saveDraft();
          const item = composeItems.find(
            (entry) => `${entry.id}@${entry.revision}` === itemPicker.value,
          );
          Object.assign(draft(), {
            primaryTask: selected,
            primaryItem: item?.id || "",
            primaryRevision: item ? String(item.revision) : "",
            itemTitle: item?.title || "",
            itemDirect: false,
          });
          void persistDraft(selected, draft()).catch((error) =>
            notice("Draft not saved: " + error.message),
          );
          interruptMessageScroll();
          presentation.interrupt();
          render(false);
          root
            .querySelector("#board-message-item")
            ?.focus({ preventScroll: true });
        };
      const auditKind = form.querySelector?.("#board-audit-kind");
      if (auditKind)
        auditKind.onchange = () => {
          saveDraft();
          if (auditKind.value === "item") {
            Object.assign(draft(), {
              primaryTask: selected,
              primaryItem: "",
              primaryRevision: "",
              itemTitle: "",
              itemDirect: false,
            });
            void persistDraft(selected, draft()).catch((error) =>
              notice("Draft not saved: " + error.message),
            );
          }
          interruptMessageScroll();
          presentation.interrupt();
          render(false);
        };
      form
        .querySelectorAll?.(
          "#board-primary-task,#board-primary-item,#board-primary-revision,#board-related,#board-order-task,#board-order-seq",
        )
        ?.forEach((control) => {
          control.onchange = saveDraft;
          control.oninput = saveDraft;
        });
      const input = form.querySelector?.("#board-text");
      if (input) {
        let composing = false;
        input.oninput = saveDraft;
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
      const recipient = form.querySelector?.("#board-to");
      if (recipient) recipient.onchange = saveDraft;
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
