// Pure rendering helpers for the Projects "Handler A/B" disclosure
// (docs/handler-ab.md). The hub computes the report; these only format and
// escape it.

export const escapeHandlerAB = (s) =>
  String(s ?? "").replace(
    /[&<>"']/g,
    (c) =>
      ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[
        c
      ],
  );

// The Delivery row suffix for an entry leased under a handler arm policy.
export function handlerArmText(arm) {
  if (!arm?.arm) return "";
  return ` · arm ${arm.arm}${arm.fallback ? ` (fallback from ${arm.drawnArm})` : ""}`;
}

export function formatMillis(ms) {
  if (ms === null || ms === undefined) return "–";
  const s = Math.round(ms / 1000);
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m ${s % 60}s`;
  return `${Math.floor(m / 60)}h ${m % 60}m`;
}

// Rational strings from the hub ("1/2") read as short decimals.
export function formatRate(value) {
  if (value === null || value === undefined || value === "") return "–";
  const [n, d] = String(value).split("/");
  const x = Number(n) / (d === undefined ? 1 : Number(d));
  if (!Number.isFinite(x)) return String(value);
  return Number.isInteger(x) ? String(x) : x.toFixed(2);
}

export const NOISE_LABELS = {
  insufficient: "too few items",
  within_noise: "within noise",
  difference: "difference",
};

export const COUNT_LABELS = {
  handlerBlocks: "Blocks to handler",
  handlerAuthoredBlocks: "Handler blocks",
  refusedSaves: "Refused saves",
  incorrectSaves: "Incorrect saves",
  ownerCorrections: "Owner corrections",
  gateFixes: "Gate fixes",
  linkCorrections: "Link corrections",
  interventions: "Interventions",
  limitEvents: "Limit events",
};

const METRIC_LABELS = {
  responseMillis: "Response time",
  launchToDoneMillis: "Launch to done",
  handlerTokens: "Handler tokens",
  ...COUNT_LABELS,
};

export function handlerABHeadline(report) {
  const n = report?.items?.length || 0;
  const policy = report?.policy;
  const state = !policy?.revision
    ? "No arm policy saved"
    : `Policy revision ${policy.revision} · ${policy.enabled ? "enabled" : "disabled"}`;
  return `${state} · ${n} finished item${n === 1 ? "" : "s"}`;
}

// Returns the disclosure body for a loaded report.
export function handlerABHtml(report) {
  const esc = escapeHandlerAB;
  const arms = report?.arms || [];
  const items = report?.items || [];
  let html = `<p class="handler-ab-headline" data-handler-ab-headline>${esc(handlerABHeadline(report))}</p>`;
  if (!arms.length) return html;
  const count = (arm, key) => {
    const c = arm.counts?.[key];
    return c ? `${esc(c.total)} <span class="fine">(${esc(formatRate(c.rate))}/item)</span>` : "–";
  };
  html += `<div class="handler-ab-scroll"><table class="handler-ab-table" data-handler-ab-arms><caption>By arm</caption><thead><tr><th scope="col">Metric</th>${arms
    .map(
      (a) =>
        `<th scope="col" data-handler-ab-arm="${esc(a.arm)}">${esc(a.arm)}${a.model ? `<span class="fine"> ${esc(a.model)} · ${esc(a.reasoning)}</span>` : ""}</th>`,
    )
    .join("")}</tr></thead><tbody>`;
  const row = (label, cell) =>
    `<tr><th scope="row">${esc(label)}</th>${arms.map((a) => `<td>${cell(a)}</td>`).join("")}</tr>`;
  html += row("Items", (a) => `${esc(a.n)}${a.fallbacks ? ` <span class="fine">(${esc(a.fallbacks)} fallback)</span>` : ""}`);
  html += row("Response median", (a) => esc(formatMillis(a.responseMedianMillis)));
  html += row("Response p90", (a) => `${esc(formatMillis(a.responseP90Millis))} <span class="fine">(${esc(a.requests)} requests)</span>`);
  html += row("Launch to done median", (a) => esc(formatMillis(a.launchToDoneMedianMillis)));
  html += row("Handler tokens median", (a) => esc(a.handlerTokensMedian ?? "–"));
  html += row("Input tokens per request", (a) => esc(formatRate(a.meanInputTokensPerRequest)));
  html += row("Rotations", (a) => esc(a.rotations));
  for (const key of Object.keys(COUNT_LABELS)) html += row(COUNT_LABELS[key], (a) => count(a, key));
  html += row("Template differs", (a) => esc(a.digestFlaggedItems));
  html += `</tbody></table></div>`;
  const comparisons = report.comparisons || [];
  if (comparisons.length)
    html += `<ul class="handler-ab-flags" data-handler-ab-flags>${comparisons
      .map(
        (c) =>
          `<li data-handler-ab-flag="${esc(c.flag)}">${esc(METRIC_LABELS[c.metric] || c.metric)} · ${esc(c.a)} vs ${esc(c.b)}: ${esc(NOISE_LABELS[c.flag] || c.flag)}</li>`,
      )
      .join("")}</ul><p class="fine">Flags are a heuristic at small n, not a significance test.</p>`;
  if (items.length)
    html += `<div class="handler-ab-scroll"><table class="handler-ab-table" data-handler-ab-items><caption>Finished items</caption><thead><tr><th scope="col">Item</th><th scope="col">Arm</th><th scope="col">Requests</th><th scope="col">Launch to done</th><th scope="col">Tokens</th><th scope="col">Blocks</th><th scope="col">Refused</th><th scope="col">Incorrect</th><th scope="col">Limits</th></tr></thead><tbody>${items
      .map(
        (it) =>
          `<tr data-handler-ab-item="${esc(it.itemId)}"><td>${esc(it.title || it.itemId)}${(it.digestFlags || []).length ? ` <span class="fine">· template differs (${esc(it.digestFlags.join(", "))})</span>` : ""}</td><td>${esc(it.arm)}${it.fallback ? ` <span class="fine">(fallback from ${esc(it.drawnArm)})</span>` : ""}</td><td>${esc((it.responseMillis || []).length)}</td><td>${it.launchToDoneMillis === null || it.launchToDoneMillis === undefined ? "not done" : esc(formatMillis(it.launchToDoneMillis))}</td><td>${esc(it.handlerTokens)}</td><td>${esc(it.handlerBlocks)}</td><td>${esc(it.refusedSaves)}</td><td>${esc(it.incorrectSaves)}</td><td>${esc(it.limitEvents?.total ?? 0)}</td></tr>`,
      )
      .join("")}</tbody></table></div>`;
  return html;
}
