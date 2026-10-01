import test from "node:test";
import assert from "node:assert/strict";
import { createHubClient } from "../client/hub-client.js";
import {
  compareRational,
  formatQuantity,
  formatUsageCost,
  sortUsageItems,
  sortUsagePhases,
  usageSummary,
  formatUsageDuration,
  usageTimeSummary,
  usageTimeline,
  usageTimeSplit,
  usageWait,
} from "../client/usage-format.js";

test("Usage client sends exact paths, methods, filters and price body", async () => {
  const calls = [];
  const client = createHubClient({
    baseURL: "https://hub.example",
    fetchImpl: async (url, init) => {
      calls.push({ url, init });
      return { status: 200, text: async () => "{}" };
    },
  });
  await client.getUsage("tsk_fixture", { from: "2026-09-01", to: "2026-09-27" });
  await client.getUsagePrices("tsk_fixture");
  const prices = { expectedRevision: 1, rows: [{ runtime: "codex", model: "test" }] };
  await client.setUsagePrices("tsk_fixture", prices);

  assert.deepEqual(
    calls.map(({ url, init }) => [url, init.method]),
    [
      ["https://hub.example/v1/tasks/tsk_fixture/usage?from=2026-09-01&to=2026-09-27", "GET"],
      ["https://hub.example/v1/tasks/tsk_fixture/usage/prices", "GET"],
      ["https://hub.example/v1/tasks/tsk_fixture/usage/prices", "PUT"],
    ],
  );
  assert.deepEqual(JSON.parse(calls[2].init.body), prices);
  assert.equal(calls[2].init.headers["Content-Type"], "application/json");
});
test("exact fractional comparison and unknown presentation", () => {
  assert.equal(compareRational("21/2", "10"), 1);
  assert.equal(compareRational("9007199254740993", "9007199254740992"), 1);
  assert.equal(formatQuantity(null), "unavailable");
  assert.equal(
    formatUsageCost({ pricedSubtotal: {}, costComplete: false }),
    "Tokens only",
  );
  assert.match(
    formatUsageCost({
      pricedSubtotal: { USD: "127/1000000" },
      costComplete: false,
    }),
    /Partially priced subtotal/,
  );
  assert.equal(usageSummary({ state: "not measured" }), "Not measured");
  assert.match(
    usageSummary({
      state: "partial",
      requests: 1,
      allocatedTurns: "1/2",
      tokens: { input: "21/2" },
      averageContext: "101",
      cachedShare: "80/101",
    }),
    /average context 101 · cached input 79.2%/,
  );
});
test("unknown cost never sorts as free; currencies remain distinct", () => {
  const item = (title, cost, complete = true) => ({
    title,
    summary: {
      costComplete: complete,
      pricedSubtotal: cost === null ? {} : { USD: cost },
    },
  });
  assert.deepEqual(
    sortUsageItems([
      item("Unknown", null, false),
      item("Low", "1"),
      item("High", "2"),
      item("Partial", "10", false),
    ]).map((x) => x.title),
    ["High", "Low", "Partial", "Unknown"],
  );
});

test("top phases use exact estimated costs or measured tokens with stable ties", () => {
  const phase = (key, cost, tokens) => ({
    key,
    label: key,
    summary: {
      pricedSubtotal: cost === null ? {} : { USD: cost },
      tokens: { input: tokens },
    },
  });
  assert.deepEqual(
    sortUsagePhases([
      phase("A low", "1", "99"),
      phase("Z high", "2", "1"),
      phase("Unpriced", null, "100"),
    ]).map((p) => p.key),
    ["Z high", "A low", "Unpriced"],
  );
  assert.deepEqual(
    sortUsagePhases([
      phase("A low", null, "1"),
      phase("Z high", null, "9007199254740993"),
      phase("B low", null, "1"),
    ]).map((p) => p.key),
    ["Z high", "A low", "B low"],
  );
});

test("time: wall, shares, timeline, phase split and waits", () => {
  assert.equal(formatUsageDuration("4812000"), "1h 20m");
  assert.equal(formatUsageDuration("305000"), "5m 5s");
  assert.equal(formatUsageDuration("38000"), "38s");
  assert.equal(formatUsageDuration("1001/2"), "501ms");
  assert.equal(formatUsageDuration("0"), "0s");
  assert.equal(formatUsageDuration(undefined), "unavailable");
  const time = {
    wallMs: "4812000",
    modelMs: "38000",
    toolMs: "32000",
    waitingMs: "4742000",
    unmeasuredMs: "0",
    polls: 5,
    pollMs: "11000",
    timeline: {
      modelMs: "38000",
      toolsOnlyMs: "32000",
      idleMs: "4742000",
      unmeasuredMs: "0",
    },
  };
  assert.equal(
    usageTimeSummary(time),
    "Wall time 1h 20m · model 0.8% · tool 0.7% · waiting 98.5% · 5 poll requests (11s)",
  );
  assert.equal(
    usageTimeline(time),
    "Team timeline: some model working 0.8% · only tools running 0.7% · nobody active 98.5%",
  );
  assert.equal(usageTimeSummary(null), "Time not measured");
  assert.equal(usageTimeSummary(undefined), "Time not measured");
  // Shares are of four agents' time in the window, not of the wall clock.
  assert.match(
    usageTimeSummary({
      wallMs: "3600000",
      modelMs: "1920000",
      toolMs: "1020000",
      waitingMs: "10860000",
      unmeasuredMs: "600000",
      polls: 1,
      pollMs: "1000",
    }),
    /^Wall time 1h 0m · model 13\.3% · tool 7\.1% · waiting 75\.4% · unmeasured 4\.2% · 1 poll request \(1s\)$/,
  );
  assert.equal(
    usageTimeSplit({ modelMs: "480000", toolMs: "120000", waitingMs: "3000000" }),
    "Time: model 8m 0s · tool 2m 0s · waiting 50m 0s",
  );
  assert.deepEqual(
    usageWait({
      cause: "owner",
      messageSeq: 12620,
      subject: "Approve the matrix",
      awaitedBy: "builder, reviewer",
      ms: "5760000",
    }),
    {
      cause: "Waiting on owner",
      target: "Approve the matrix #12620",
      detail: "1h 36m · builder, reviewer",
    },
  );
  assert.deepEqual(usageWait({ cause: "unknown", ms: "305000", awaitedBy: "builder" }), {
    cause: "Waiting on unknown",
    target: "Nothing the Board shows",
    detail: "5m 5s · builder",
  });
});
