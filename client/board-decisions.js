const esc = (value) =>
  String(value ?? "").replace(
    /[&<>"']/g,
    (character) =>
      ({
        "&": "&amp;",
        "<": "&lt;",
        ">": "&gt;",
        '"': "&quot;",
        "'": "&#39;",
      })[character],
  );

const keyFor = (taskId, requestSeq) => `${taskId}\n${requestSeq}`;

export async function readAllDecisions(client, taskId, { limit = 100 } = {}) {
  const decisions = [];
  let after = 0;
  for (;;) {
    const page = await client.listDecisions(taskId, { after, limit });
    decisions.push(...(page.decisions || []));
    const next = Number(page.nextAfter || 0);
    if (!next) return decisions;
    if (!Number.isSafeInteger(next) || next <= after)
      throw new Error("The hub returned an invalid decision cursor.");
    after = next;
  }
}

export function createDecisionDrafts({
  requestId = () => crypto.randomUUID(),
} = {}) {
  const drafts = new Map();
  const get = (taskId, requestSeq) => {
    const key = keyFor(taskId, requestSeq);
    if (!drafts.has(key))
      drafts.set(key, {
        optionId: "",
        customMode: false,
        custom: "",
        explanation: "",
        requestId: requestId(),
        attemptedPayload: "",
      });
    return drafts.get(key);
  };
  return {
    get,
    update(taskId, requestSeq, values) {
      Object.assign(get(taskId, requestSeq), values);
    },
    begin(taskId, record) {
      const requestSeq = record.request.seq;
      const draft = get(taskId, requestSeq);
      let body;
      if (draft.customMode) {
        const text = draft.custom.trim();
        if (!text) throw new Error("Enter a custom answer before submitting.");
        body = { text };
      } else {
        const option = record.request.decisionRequest.options.find(
          ({ id }) => id === draft.optionId,
        );
        if (!option) throw new Error("Choose an answer before submitting.");
        body = { optionId: option.id };
        const text = draft.explanation.trim();
        if (text) body.text = text;
      }
      const payload = JSON.stringify(body);
      if (draft.attemptedPayload && draft.attemptedPayload !== payload)
        draft.requestId = requestId();
      draft.attemptedPayload = payload;
      return { requestId: draft.requestId, ...body };
    },
    resolve(taskId, requestSeq) {
      drafts.delete(keyFor(taskId, requestSeq));
    },
    discardTask(taskId) {
      for (const key of drafts.keys())
        if (key.startsWith(`${taskId}\n`)) drafts.delete(key);
    },
  };
}

export function captureDecisionPresentation(root, previous = {}) {
  const panel = root?.querySelector?.("#board-decisions");
  if (!panel) return previous;
  const history = panel.querySelector?.(".decision-history");
  return {
    scrollTop: panel.scrollTop || 0,
    historyOpen: history ? history.open : previous.historyOpen,
  };
}

export function restoreDecisionPresentation(root, presentation = {}) {
  const panel = root?.querySelector?.("#board-decisions");
  if (!panel) return;
  panel.scrollTop = presentation.scrollTop || 0;
  const history = panel.querySelector?.(".decision-history");
  if (history && typeof presentation.historyOpen === "boolean")
    history.open = presentation.historyOpen;
}

const optionMarkup = (option, request, draft, disabled) => {
  const recommended = option.id === request.recommendedOptionId;
  return `<label class="decision-option ${recommended ? "recommended" : ""}"><input type="radio" name="decision-choice" value="${esc(option.id)}" data-decision-option="${esc(option.id)}" ${!draft.customMode && draft.optionId === option.id ? "checked" : ""} ${disabled ? "disabled" : ""}><span class="decision-option-copy"><span class="decision-option-label">${esc(option.label)}</span><span class="fine">${esc(option.description)}</span>${recommended ? `<span class="decision-recommendation"><strong>Recommended</strong><span>${esc(request.recommendationReason)}</span></span>` : ""}</span></label>`;
};

function pendingCard({
  record,
  taskId,
  name,
  archived,
  draft,
  sending,
  error,
}) {
  const message = record.request;
  const request = message.decisionRequest;
  const readonly = archived || sending;
  return `<article class="decision-card" data-decision-request="${message.seq}" data-decision-state="${archived ? "read-only" : "pending"}"><div class="decision-meta"><span>Decision #${message.seq}</span><span>Requested by ${esc(name(message))}</span></div><h4>${esc(request.question)}</h4><form data-decision-form="${message.seq}"><fieldset ${readonly ? "disabled" : ""}><legend class="visually-hidden">Answer decision #${message.seq}</legend><div class="decision-options">${request.options.map((option) => optionMarkup(option, request, draft, readonly)).join("")}</div>${archived ? '<p class="fine decision-readonly">This project is closed. Its decisions are read-only.</p>' : `<label class="decision-option decision-custom-choice"><input type="radio" name="decision-choice" value="" data-decision-custom-choice ${draft.customMode ? "checked" : ""} ${sending ? "disabled" : ""}><span class="decision-option-copy"><span class="decision-option-label">Custom answer</span><span class="fine">Give the worker a different direction.</span></span></label><label class="decision-answer-field" data-decision-custom-field>Custom answer<textarea rows="2" maxlength="4000" data-decision-custom placeholder="Enter your answer…" ${sending ? "disabled" : ""}>${esc(draft.custom)}</textarea></label><label class="decision-answer-field" data-decision-explanation-field>Optional explanation<textarea rows="2" maxlength="4000" data-decision-explanation placeholder="Add context for the selected choice…" ${sending ? "disabled" : ""}>${esc(draft.explanation)}</textarea></label><div class="decision-actions"><p class="fine" role="alert" aria-live="assertive" data-decision-error>${esc(error || "")}</p><button type="submit" class="primary" data-decision-submit ${sending ? "disabled" : ""}>${sending ? "Submitting…" : "Submit answer"}</button></div><p class="fine decision-consent-note">Nothing is sent until you submit an answer. The recommendation is not consent.</p>`}</fieldset></form></article>`;
}

function answeredCard({ record, name, revealed }) {
  const message = record.request;
  const request = message.decisionRequest;
  const answer = record.answer;
  const structured = answer.decisionAnswer || {};
  const option = request.options.find(({ id }) => id === structured.optionId);
  const answerLabel = option ? option.label : "Custom answer";
  return `<article class="decision-card answered ${revealed ? "decision-just-answered" : ""}" data-decision-request="${message.seq}" data-decision-state="answered"><div class="decision-meta"><span>Decision #${message.seq}</span><span>Requested by ${esc(name(message))}</span></div><h4>${esc(request.question)}</h4><div class="decision-answer" data-decision-answer>${revealed ? '<span class="decision-answer-notice" data-decision-conflict>Stored answer</span>' : ""}<strong>${esc(answerLabel)}</strong>${structured.text ? `<p>${esc(structured.text)}</p>` : ""}<span class="fine">Answered by ${esc(name(answer))} · reply #${answer.seq}</span></div></article>`;
}

export function renderDecisionPanel({
  records,
  taskId,
  archived,
  name,
  drafts,
  sending,
  errors,
  revealedAnswers = new Set(),
  historyOpen,
  loadError = "",
}) {
  if (!records.length && !loadError) return "";
  for (const record of records)
    if (record.answer) drafts.resolve(taskId, record.request.seq);
  const pending = records.filter((record) => !record.answer);
  const answered = records.filter((record) => record.answer);
  const heading = `<div class="decision-panel-head"><h3 id="board-decisions-title">Decisions</h3>${pending.length ? `<span class="count-badge">${pending.length} pending</span>` : ""}</div>`;
  const error = loadError
    ? `<div class="decision-load-error" role="status"><span class="fine">Decisions unavailable: ${esc(loadError)}</span><button type="button" data-decisions-retry>Retry</button></div>`
    : "";
  const open = pending
    .map((record) =>
      pendingCard({
        record,
        taskId,
        name,
        archived,
        draft: drafts.get(taskId, record.request.seq),
        sending: sending.has(record.request.seq),
        error: errors.get(record.request.seq),
      }),
    )
    .join("");
  const history = answered.length
    ? `<details class="decision-history" ${(historyOpen ?? answered.some((record) => revealedAnswers.has(record.request.seq))) ? "open" : ""}><summary>Answered decisions · ${answered.length}</summary>${answered.map((record) => answeredCard({ record, name, revealed: revealedAnswers.has(record.request.seq) })).join("")}</details>`
    : "";
  return `<section id="board-decisions" aria-labelledby="board-decisions-title">${heading}${error}${open}${history}</section>`;
}
