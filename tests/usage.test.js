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
