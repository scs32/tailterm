// Pure rendering helpers for the Projects "Interventions" disclosure. The hub
// computes the whole-project summary; these only order and escape it.

// Mirrors api.InterventionKinds; the hub is the authority on which are valid.
export const INTERVENTION_KINDS = [
  "release",
  "nudge",
  "decision-on-behalf",
  "gate-fix",
  "cleanup",
  "diagnosis",
  "status",
  "other",
];

export const escapeIntervention = (s) =>
  String(s ?? "").replace(
    /[&<>"']/g,
    (c) =>
      ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[
        c
      ],
  );

// Kind counts in vocabulary order; unknown kinds from a newer hub sort last.
export function kindCounts(byKind = {}) {
  const rank = (kind) => {
    const i = INTERVENTION_KINDS.indexOf(kind);
    return i < 0 ? INTERVENTION_KINDS.length : i;
  };
  return Object.entries(byKind || {})
    .filter(([, count]) => count > 0)
    .sort(([a], [b]) => rank(a) - rank(b) || a.localeCompare(b))
    .map(([kind, count]) => ({ kind, count }));
}

export const formatKindCounts = (byKind) =>
  kindCounts(byKind)
    .map(({ kind, count }) => `${kind} ${count}`)
    .join(" · ");

export function interventionHeadline(summary) {
  const total = summary?.total || 0;
  if (!total) return "No interventions recorded";
  const linked = summary.linked || 0;
  return `${total} intervention${total === 1 ? "" : "s"} · ${linked} of ${total} linked to a product item`;
}

// Returns the disclosure body for a loaded summary.
export function interventionsHtml(summary) {
  const esc = escapeIntervention;
  const total = summary?.total || 0;
  let html = `<p class="interventions-headline" data-interventions-headline>${esc(interventionHeadline(summary))}${summary?.timeZone ? ` <span class="fine">· days in ${esc(summary.timeZone)}</span>` : ""}</p>`;
  if (!total) return html;
  html += `<table class="interventions-table interventions-kinds" data-interventions-kinds><caption>By kind</caption><tbody>${kindCounts(
    summary.byKind,
  )
    .map(
      ({ kind, count }) =>
        `<tr><td>${esc(kind)}</td><td>${esc(count)}</td></tr>`,
    )
    .join("")}</tbody></table>`;
  html += `<table class="interventions-table interventions-days" data-interventions-days><caption>By day</caption><thead><tr><th scope="col">Day</th><th scope="col">Total</th><th scope="col">Linked</th><th scope="col">Kinds</th></tr></thead><tbody>${(
    summary.days || []
  )
    .map(
      (day) =>
        `<tr data-intervention-day="${esc(day.day)}"><td>${esc(day.day)}</td><td>${esc(day.total)}</td><td>${esc(day.linked)}</td><td>${esc(formatKindCounts(day.byKind))}</td></tr>`,
    )
    .join("")}</tbody></table>`;
  const unlinked = summary.unlinked || [];
  if (unlinked.length) {
    const hidden = total - (summary.linked || 0) - unlinked.length;
    html += `<section class="interventions-unlinked" aria-label="Interventions without a product item"><p class="fine">Without a product item</p><ul>${unlinked
      .map(
        (ref) =>
          `<li data-intervention-unlinked="${esc(ref.seq)}">#${esc(ref.seq)} · ${esc(ref.kind)} · ${esc(ref.itemId)} · ${esc(ref.day)}</li>`,
      )
      .join(
        "",
      )}</ul>${hidden > 0 ? `<p class="fine">${esc(hidden)} older not shown · tt owner interventions --json lists all</p>` : ""}</section>`;
  }
  return html;
}
