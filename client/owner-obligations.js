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
  } = {},
) {
  const open = obligations.filter(
    (o) => o.recipientKind === "owner" && o.state !== "closed",
  );
  if (!open.length && !error) return "";
  return `<section class="decision-panel" data-owner-requests><h3>Owner requests <span class="count-badge">${open.length}</span></h3>${error ? `<p role="alert">${esc(error)}</p>` : ""}${open.map((o) => `<article class="decision-request" data-owner-request="${esc(o.id)}"><strong>${esc(o.subject)}</strong><p class="fine">#${o.messageSeq} · Due ${esc(new Date(o.dueAt).toLocaleString())} · Waiting <span data-owner-age="${esc(o.createdAt)}">${ownerAge(o.createdAt)}</span>${o.escalation ? " · Overdue escalation sent" : ""}</p><pre style="white-space:pre-wrap;overflow-wrap:anywhere">${esc(o.request?.text || "")}</pre>${archived ? "" : `<form data-owner-answer="${esc(o.id)}"><label>Answer<textarea name="answer" required maxlength="4000" placeholder="Answer this request…" ${sending.has(o.id) ? "disabled" : ""}></textarea></label><button type="submit" ${sending.has(o.id) ? "disabled" : ""}>Send answer</button>${o.request?.envelope?.expectedAnswer ? `<button type="button" data-owner-approve="${esc(o.id)}" ${sending.has(o.id) ? "disabled" : ""}>Approve</button>` : ""}</form>`}${errors.get(o.id) ? `<p role="alert">${esc(errors.get(o.id))}</p>` : ""}</article>`).join("")}</section>`;
}
