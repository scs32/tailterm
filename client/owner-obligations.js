const esc = (value) =>
  String(value ?? "").replace(
    /[&<>"']/g,
    (c) =>
      ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[
        c
      ],
  );
export function ownerWaitSummary(
  obligations = [],
  taskId,
  itemId,
  now = Date.now(),
) {
  const open = obligations.filter(
    (o) =>
      o.recipientKind === "owner" &&
      o.state !== "closed" &&
      o.taskId === taskId &&
      o.request?.workItems?.some(
        (l) =>
          l.itemTaskId === taskId &&
          l.itemId === itemId &&
          l.relationship === "primary",
      ),
  );
  if (!open.length) return "";
  const oldest = Math.min(...open.map((o) => Date.parse(o.createdAt)));
  return `<span class="fine" data-owner-wait>Waiting on owner · ${open.length} request${open.length === 1 ? "" : "s"} · <span data-owner-age="${esc(new Date(oldest).toISOString())}">${ownerAge(oldest, now)}</span></span>`;
}
export function ownerAge(created, now = Date.now()) {
  const seconds = Math.max(
    0,
    Math.floor((now - new Date(created).getTime()) / 1000),
  );
  return seconds < 60
    ? `${seconds}s`
    : `${Math.floor(seconds / 60)}m ${seconds % 60}s`;
}
export function startOwnerAgeClock(root) {
  const timer = setInterval(
    () =>
      root?.querySelectorAll("[data-owner-age]").forEach((el) => {
        el.textContent = ownerAge(el.dataset.ownerAge);
      }),
    1000,
  );
  return () => clearInterval(timer);
}
export function renderOwnerRequests(
  obligations = [],
  {
    archived = false,
    error = "",
    sending = new Set(),
    errors = new Map(),
    windows = [],
    now = Date.now(),
  } = {},
) {
  const open = obligations.filter(
    (o) => o.recipientKind === "owner" && o.state !== "closed",
  );
  if (!open.length && !error) return "";
  const delegatedTo = (o) => {
    const w = activeDelegationWindow(windows, now);
    return w?.routes?.some(
      (r) => r.obligationId === o.id && !r.returnedAt && !r.answerSeq,
    )
      ? ` · <span class="delegated-tag" data-delegated-to="${esc(w.delegateName)}">Delegated to ${esc(w.delegateName)}</span>`
      : "";
  };
  return `<section class="decision-panel" data-owner-requests><h3>Owner requests <span class="count-badge">${open.length}</span></h3>${error ? `<p role="alert">${esc(error)}</p>` : ""}${open.map((o) => `<article class="decision-request" data-owner-request="${esc(o.id)}"><strong>${esc(o.subject)}</strong><p class="fine">#${o.messageSeq}${delegatedTo(o)} · Due ${esc(new Date(o.dueAt).toLocaleString())} · Waiting <span data-owner-age="${esc(o.createdAt)}">${ownerAge(o.createdAt)}</span>${o.escalation ? " · Overdue escalation sent" : ""}</p><pre style="white-space:pre-wrap;overflow-wrap:anywhere">${esc(o.request?.text || "")}</pre>${archived ? "" : `<form data-owner-answer="${esc(o.id)}"><label>Answer<textarea name="answer" required maxlength="4000" placeholder="Answer this request…" ${sending.has(o.id) ? "disabled" : ""}></textarea></label><button type="submit" ${sending.has(o.id) ? "disabled" : ""}>Send answer</button>${o.request?.envelope?.expectedAnswer ? `<button type="button" data-owner-approve="${esc(o.id)}" ${sending.has(o.id) ? "disabled" : ""}>Approve</button>` : ""}</form>`}${errors.get(o.id) ? `<p role="alert">${esc(errors.get(o.id))}</p>` : ""}</article>`).join("")}</section>`;
}

// Owner delegation windows (docs/owner-delegation-windows.md).
const scopeLabels = {
  decisions: "Decisions",
  decisions_merges_deploys: "Decisions, merges and deploys",
};
export function activeDelegationWindow(windows = [], now = Date.now()) {
  return (
    windows.find((w) => w.state === "open" && Date.parse(w.endsAt) > now) ||
    null
  );
}
export function delegatedAnswers(windows = []) {
  return windows
    .flatMap((w) =>
      (w.routes || [])
        .filter((r) => r.answerSeq)
        .map((r) => ({ ...r, delegateName: w.delegateName })),
    )
    .sort((a, b) => Date.parse(b.answeredAt) - Date.parse(a.answeredAt));
}
// localInputValue formats a time for a datetime-local control.
export function localInputValue(time) {
  const d = new Date(time);
  const pad = (n) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
}
export function renderDelegationStrip(
  windows = [],
  {
    agents = [],
    archived = false,
    sending = false,
    error = "",
    draft = {},
    formOpen = false,
    answersOpen = false,
    now = Date.now(),
  } = {},
) {
  const active = activeDelegationWindow(windows, now);
  const answers = delegatedAnswers(windows);
  if (archived && !answers.length) return "";
  const when = (t) =>
    new Date(t).toLocaleString([], {
      month: "short",
      day: "numeric",
      hour: "2-digit",
      minute: "2-digit",
    });
  let control = "";
  if (active) {
    control = `<div class="delegation-state" data-delegation-window="${esc(active.id)}"><span>Delegated to <strong>${esc(active.delegateName)}</strong> until <time datetime="${esc(active.endsAt)}">${esc(when(active.endsAt))}</time> · ${esc(scopeLabels[active.scope] || active.scope)}</span>${archived ? "" : `<button type="button" data-delegation-end="${esc(active.id)}" ${sending ? "disabled" : ""}>${sending ? "Ending…" : "End now"}</button>`}</div>`;
  } else if (!archived) {
    const live = agents.filter(
      (a) => a.status !== "closed" && a.status !== "exited",
    );
    const endsAt = draft.endsAt || localInputValue(now + 2 * 3600 * 1000);
    control = `<details class="delegation-open" data-delegation-toggle ${formOpen ? "open" : ""}><summary data-view-control="delegate">Delegate decisions…</summary><form data-delegation-open><label>Delegate<select name="delegate" required ${sending ? "disabled" : ""}><option value="" ${draft.delegate ? "" : "selected"} disabled>Choose an agent</option>${live.map((a) => `<option value="${esc(a.id)}" ${draft.delegate === a.id ? "selected" : ""}>${esc(a.name)}</option>`).join("")}</select></label><label>Until<input type="datetime-local" name="endsAt" required value="${esc(endsAt)}" ${sending ? "disabled" : ""}></label><label>Scope<select name="scope" ${sending ? "disabled" : ""}>${Object.entries(
      scopeLabels,
    )
      .map(
        ([value, label]) =>
          `<option value="${value}" ${(draft.scope || "decisions") === value ? "selected" : ""}>${label}</option>`,
      )
      .join(
        "",
      )}</select></label><button type="submit" ${sending ? "disabled" : ""}>${sending ? "Opening…" : "Open"}</button></form><p class="fine">Matrix approvals always stay with you.</p></details>`;
  }
  const list = answers.length
    ? `<details class="delegated-answers" data-delegated-answers ${answersOpen ? "open" : ""}><summary data-view-control="delegated-answers">Delegated answers (${answers.length})</summary><ul>${answers.map((r) => `<li data-delegated-answer="${r.answerSeq}"><span class="fine">#${r.requestSeq} · ${esc(r.delegateName)} · <time datetime="${esc(r.answeredAt)}">${esc(when(r.answeredAt))}</time></span><strong>${esc(r.subject || "Request #" + r.requestSeq)}</strong><p>${esc(r.answer)}</p><p class="fine">Rationale: ${esc(r.rationale)}</p></li>`).join("")}</ul></details>`
    : "";
  return `<section class="delegation-strip" data-delegation>${control}${list}${error ? `<p role="alert">${esc(error)}</p>` : ""}</section>`;
}
