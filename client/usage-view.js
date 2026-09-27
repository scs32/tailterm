import "./usage.css";
import {
  escapeUsage as esc,
  formatUsageCost,
  usageSummary,
  sortUsageItems,
  sortUsagePhases,
  usageClasses,
} from "./usage-format.js";

// Optional disclosure owns its own requests. Slow usage reads never hold core
// Projects/roster/Board rendering, and every response pins client/project/epoch.
export function createUsageView({ client, notice = () => {} }) {
  let root = null,
    project = "",
    epoch = 0,
    mountedClient = null;
  const states = new Map();
  const state = () => {
    if (!states.has(project))
      states.set(project, {
        open: false,
        from: "",
        to: "",
        report: null,
        prices: null,
        error: "",
        editor: false,
        draft: [],
        expanded: new Set(),
      });
    return states.get(project);
  };
  const offline = () =>
    String(client()?.cacheStatus?.().label || "").includes("offline");
  function mount(node, id) {
    if (mountedClient !== client()) {
      states.clear();
      mountedClient = client();
    }
    root = node;
    project = id;
    epoch++;
    if (!node) return;
    render();
    if (state().open && !state().report) void load();
  }
  function render() {
    if (!root) return;
    const s = state();
    const scroll = root.scrollTop;
    const active = document.activeElement;
    const key = active?.dataset?.usageField;
    const start = active?.selectionStart,
      end = active?.selectionEnd;
    const groups = (label, rows = []) =>
      `<details data-usage-group="${esc(label)}" ${s.expanded.has(label) ? "open" : ""}><summary>${esc(label)}</summary><table class="usage-breakdown"><tbody>${rows.map((g) => `<tr><td>${esc(g.label)}</td><td>${esc(usageSummary(g.summary))}<br>${esc(formatUsageCost(g.summary))}</td></tr>`).join("")}</tbody></table></details>`;
    const item = (row) =>
      `<details class="usage-item" data-usage-item="${esc(row.itemId || "overhead")}" ${s.expanded.has(row.itemId || "overhead") ? "open" : ""}><summary>${esc(row.title)} · ${esc(formatUsageCost(row.summary))}</summary><p>${esc(usageSummary(row.summary))}</p>${groups("Phases · " + (row.itemId || "overhead"), sortUsagePhases(row.phases))}${groups("Roles · " + (row.itemId || "overhead"), row.roles)}${groups("Models · " + (row.itemId || "overhead"), row.models)}${groups("Phase and role · " + (row.itemId || "overhead"), row.phaseRoles)}</details>`;
    const coverage = [
      ...new Set(
        (s.report?.coverage || [])
          .map((c) => c.state)
          .filter((v) => v.startsWith("partial")),
      ),
    ].join(" · ");
    const input = (i, k, v, type = "text") =>
      `<label class="${k === "model" ? "usage-model" : ""}">${esc(k)}<input type="${type}" data-price-row="${i}" data-price-key="${k}" data-usage-field="price-${i}-${k}" value="${esc(v)}" placeholder="${k.startsWith("rate:") ? "unpriced" : ""}" ${offline() ? "disabled" : ""}></label>`;
    root.innerHTML = `<details class="project-usage" ${s.open ? "open" : ""} data-usage-disclosure><summary>Usage</summary><div class="usage-controls"><label>From UTC<input data-usage-field="from" placeholder="2026-09-27T00:00:00Z" value="${esc(s.from)}"></label><label>To UTC (exclusive)<input data-usage-field="to" placeholder="2026-09-28T00:00:00Z" value="${esc(s.to)}"></label><button data-usage-refresh>Apply</button><button data-usage-prices>Prices</button></div><p class="fine" role="status">${esc(s.error || (offline() ? "Saved data · offline" : s.report ? usageSummary(s.report.summary) : "Usage not loaded"))}</p>${s.report ? `${coverage ? `<p class="fine">${esc(coverage)}</p>` : ""}<div class="usage-items">${sortUsageItems(s.report.items).map(item).join("")}${item(s.report.overhead)}</div>` : ""}${s.editor ? `<section aria-label="Usage prices"><p class="fine">Owner prices per million tokens · revision ${s.prices?.revision ?? "unavailable"}. Blank rates remain unpriced.</p>${s.draft.map((p, i) => `<div class="usage-price-row">${input(i, "runtime", p.runtime)}${input(i, "model", p.model)}${input(i, "currency", p.currency)}${input(i, "effectiveAt", p.effectiveAt)}${usageClasses.map((k) => input(i, "rate:" + k, p.rates?.[k] ?? "")).join("")}<button data-price-remove="${i}" ${offline() ? "disabled" : ""}>Remove</button></div>`).join("")}<div class="usage-prices-actions"><button data-price-add ${offline() ? "disabled" : ""}>Add rate</button><button data-price-save ${offline() || !s.prices ? "disabled" : ""}>Save prices</button></div></section>` : ""}</details>`;
    root.querySelector("[data-usage-disclosure]").ontoggle = (e) => {
      s.open = e.target.open;
      if (s.open && !s.report && !s.error) void load();
    };
    for (const el of root.querySelectorAll(
      "[data-usage-item],[data-usage-group]",
    )) {
      el.ontoggle = () => {
        const k = el.dataset.usageItem || el.dataset.usageGroup;
        if (el.open) s.expanded.add(k);
        else s.expanded.delete(k);
      };
    }
    for (const k of ["from", "to"]) {
      root.querySelector(`[data-usage-field="${k}"]`).oninput = (e) =>
        (s[k] = e.target.value);
    }
    root.querySelector("[data-usage-refresh]").onclick = () => void load();
    root.querySelector("[data-usage-prices]").onclick = () => void editPrices();
    for (const el of root.querySelectorAll("[data-price-key]")) {
      el.oninput = () => {
        const p = s.draft[Number(el.dataset.priceRow)],
          k = el.dataset.priceKey;
        if (k.startsWith("rate:")) {
          const klass = k.slice(5);
          if (el.value === "") delete p.rates[klass];
          else p.rates[klass] = el.value;
        } else p[k] = el.value;
      };
    }
    for (const el of root.querySelectorAll("[data-price-remove]"))
      el.onclick = () => {
        s.draft.splice(Number(el.dataset.priceRemove), 1);
        render();
      };
    const add = root.querySelector("[data-price-add]");
    if (add)
      add.onclick = () => {
        s.draft.push({
          runtime: "codex",
          model: "",
          currency: "USD",
          effectiveAt: "",
          rates: {},
        });
        render();
      };
    const save = root.querySelector("[data-price-save]");
    if (save) save.onclick = () => void savePrices();
    root.scrollTop = scroll;
    if (key) {
      const target = [...root.querySelectorAll("[data-usage-field]")].find(
        (el) => el.dataset.usageField === key,
      );
      target?.focus({ preventScroll: true });
      try {
        target?.setSelectionRange(start, end);
      } catch {}
    }
  }
  async function load() {
    const s = state(),
      id = project,
      c = client(),
      token = ++epoch;
    s.error = "Loading usage…";
    render();
    try {
      if (typeof c.getUsage !== "function") {
        s.error = "Usage unsupported by this hub";
        render();
        return;
      }
      const report = await c.getUsage(id, { from: s.from, to: s.to });
      if (token !== epoch || id !== project || c !== client() || !root) return;
      s.report = report;
      s.error = "";
      render();
    } catch (e) {
      if (token !== epoch || id !== project || c !== client() || !root) return;
      s.error = [404, 405].includes(e.status)
        ? "Usage unsupported by this hub"
        : e.message || "Usage unavailable";
      render();
    }
  }
  async function editPrices() {
    const s = state(),
      id = project,
      c = client(),
      token = ++epoch;
    if (s.editor) {
      s.editor = false;
      render();
      return;
    }
    try {
      const prices = await c.getUsagePrices(id);
      if (token !== epoch || id !== project || c !== client() || !root) return;
      s.prices = prices;
      s.draft = structuredClone(prices.rows || []);
      s.editor = true;
      s.error = "";
      render();
    } catch (e) {
      if (token === epoch && id === project && c === client()) {
        s.error = e.message;
        render();
      }
    }
  }
  async function savePrices() {
    if (offline()) return;
    const s = state(),
      id = project,
      c = client(),
      token = ++epoch;
    const payload = {
      rows: structuredClone(s.draft),
      expectedRevision: s.prices.revision,
    };
    const signature = JSON.stringify(payload);
    if (s.retrySignature !== signature) {
      s.retrySignature = signature;
      s.retryKey = "usage-prices-" + crypto.randomUUID();
    }
    try {
      const prices = await c.setUsagePrices(id, {
        ...payload,
        requestId: s.retryKey,
      });
      if (token !== epoch || id !== project || c !== client() || !root) return;
      s.prices = prices;
      s.retrySignature = null;
      s.error = "Prices saved";
      render();
      await load();
    } catch (e) {
      if (token === epoch && id === project && c === client()) {
        s.error =
          e.status === 409
            ? "Prices changed; reload Prices before saving."
            : e.message;
        render();
        notice(s.error);
      }
    }
  }
  function dispose() {
    root = null;
    epoch++;
  }
  return { mount, dispose };
}
