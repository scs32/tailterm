import test from "node:test";
import assert from "node:assert/strict";
import {
  compareRational,
  formatQuantity,
  formatUsageCost,
  sortUsageItems,
  usageSummary,
} from "../client/usage-format.js";
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
