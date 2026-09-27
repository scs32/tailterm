export const usageClasses = [
  "input",
  "cached",
  "cacheWrite",
  "output",
  "reasoning",
];
export const escapeUsage = (v) =>
  String(v ?? "").replace(
    /[&<>"']/g,
    (c) =>
      ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[
        c
      ],
  );
export function rational(value) {
  if (typeof value !== "string" || !/^-?\d+(\/\d+)?$/.test(value)) return null;
  const [n, d = "1"] = value.split("/");
  try {
    const denominator = BigInt(d);
    return denominator > 0n ? [BigInt(n), denominator] : null;
  } catch {
    return null;
  }
}
export function compareRational(a, b) {
  const x = rational(a),
    y = rational(b);
  if (!x || !y) return 0;
  const delta = x[0] * y[1] - y[0] * x[1];
  return delta > 0n ? 1 : delta < 0n ? -1 : 0;
}
export function formatQuantity(value, digits = 1) {
  const r = rational(value);
  if (!r) return "unavailable";
  return (Number(r[0]) / Number(r[1])).toLocaleString(undefined, {
    maximumFractionDigits: digits,
  });
}
export function formatUsageCost(summary) {
  const costs = Object.entries(summary?.pricedSubtotal || {});
  if (!costs.length) return "Tokens only";
  const value = costs
    .map(([currency, n]) => `${currency} ${formatQuantity(n, 6)}`)
    .join(" · ");
  return `${summary.costComplete ? "Estimated" : "Partially priced subtotal"} ${value}`;
}
export function sortUsageItems(items = []) {
  return [...items].sort((a, b) => {
    const x = a.summary,
      y = b.summary;
    if (!!x.costComplete !== !!y.costComplete) return x.costComplete ? -1 : 1;
    const xc = Object.keys(x.pricedSubtotal || {}),
      yc = Object.keys(y.pricedSubtotal || {});
    if (
      x.costComplete &&
      y.costComplete &&
      xc.length === 1 &&
      yc.length === 1
    ) {
      if (xc[0] !== yc[0]) return xc[0].localeCompare(yc[0]);
      const c = compareRational(
        y.pricedSubtotal[yc[0]],
        x.pricedSubtotal[xc[0]],
      );
      if (c) return c;
    }
    return String(a.title).localeCompare(String(b.title));
  });
}
export function usageSummary(summary = {}) {
  if (summary.state === "not measured") return "Not measured";
  const tokens = usageClasses
    .filter((k) => summary.tokens?.[k] !== undefined)
    .map((k) => `${k}: ${formatQuantity(summary.tokens[k])}`)
    .join(" · ");
  // Percentage uses floating presentation only; source quantities remain exact.
  const r = rational(summary.cachedShare);
  const cached = r
    ? `${((Number(r[0]) / Number(r[1])) * 100).toFixed(1)}%`
    : "unavailable";
  return `${summary.state === "partial" ? "Partial coverage · " : ""}${summary.requests || 0} requests · ${formatQuantity(summary.allocatedTurns)} allocated turns · average context ${formatQuantity(summary.averageContext)} · cached input ${cached}${tokens ? " · " + tokens : ""}`;
}

// Rank top phases using exact estimated cost where comparable. Keep currencies
// distinct; unpriced phases use the sum of reported disjoint token classes.
export function sortUsagePhases(phases = []) {
  const total = (summary) =>
    usageClasses.reduce(
      (sum, k) => {
        const r = rational(summary.tokens?.[k]);
        if (!r) return sum;
        return [sum[0] * r[1] + r[0] * sum[1], sum[1] * r[1]];
      },
      [0n, 1n],
    );
  return [...phases].sort((a, b) => {
    const ac = Object.keys(a.summary.pricedSubtotal || {}).sort(),
      bc = Object.keys(b.summary.pricedSubtotal || {}).sort();
    if (!!ac.length !== !!bc.length) return ac.length ? -1 : 1;
    if (ac.length === 1 && bc.length === 1) {
      if (ac[0] !== bc[0]) return ac[0].localeCompare(bc[0]);
      const cost = compareRational(
        b.summary.pricedSubtotal[bc[0]],
        a.summary.pricedSubtotal[ac[0]],
      );
      if (cost) return cost;
    }
    const x = total(a.summary),
      y = total(b.summary),
      d = y[0] * x[1] - x[0] * y[1];
    if (d !== 0n) return d > 0n ? 1 : -1;
    return String(a.key || a.label).localeCompare(String(b.key || b.label));
  });
}
