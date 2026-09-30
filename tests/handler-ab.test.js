import test from "node:test";
import assert from "node:assert/strict";
import { createHubClient } from "../client/hub-client.js";
import {
  formatMillis,
  formatRate,
  handlerABHeadline,
  handlerABHtml,
  handlerArmText,
} from "../client/handler-ab-format.js";
import { createHandlerABView } from "../client/handler-ab-view.js";
import { renderTeamDelivery } from "../client/team-delivery-view.js";

// Handler A/B (wi_fc1396aef8a72a06, order #14869), synthetic data only.

const report = {
  taskId: "tsk_fixture",
  policy: { revision: 2, enabled: true },
  arms: [
    {
      arm: "O",
      model: "gpt-6.1-sol",
      reasoning: "high",
      n: 3,
      fallbacks: 1,
      requests: 4,
      responseMedianMillis: 6500,
      responseP90Millis: 8000,
      launchToDoneMedianMillis: 1800000,
      handlerTokensMedian: 110,
      meanInputTokensPerRequest: "101",
      rotations: 0,
      digestFlaggedItems: 1,
      counts: { refusedSaves: { total: 1, rate: "1/3" } },
    },
    {
      arm: "S",
      model: "claude-sonnet-5-5",
      reasoning: "high",
      n: 2,
      fallbacks: 0,
      requests: 5,
      responseMedianMillis: 3000,
      responseP90Millis: 9000,
      launchToDoneMedianMillis: null,
      handlerTokensMedian: 165,
      meanInputTokensPerRequest: "303/2",
      rotations: 2,
      digestFlaggedItems: 0,
      counts: { refusedSaves: { total: 1, rate: "1/2" } },
    },
  ],
  items: [
    {
      itemId: "wi_1111111111111111",
      title: "First item",
      arm: "O",
      drawnArm: "S",
      fallback: true,
      responseMillis: [7000],
      launchToDoneMillis: null,
      handlerTokens: 110,
      handlerBlocks: 0,
      refusedSaves: 0,
      incorrectSaves: 1,
      limitEvents: { total: 2 },
      digestFlags: ["policy", "other_arm"],
    },
  ],
  comparisons: [
    { metric: "responseMillis", a: "O", b: "S", flag: "insufficient" },
    { metric: "refusedSaves", a: "O", b: "S", flag: "within_noise" },
    { metric: "handlerTokens", a: "O", b: "S", flag: "difference" },
  ],
};

test("Handler A/B client reads the project report route", async () => {
  const calls = [];
  const client = createHubClient({
    baseURL: "https://hub.example",
    fetchImpl: async (url, init) => {
      calls.push([url, init.method]);
      return { status: 200, text: async () => "{}" };
    },
  });
  await client.handlerABReport("tsk_fixture");
  assert.deepEqual(calls, [
    ["https://hub.example/v1/tasks/tsk_fixture/handler-ab/report", "GET"],
  ]);
});

test("Formatting helpers read hub values", () => {
  assert.equal(formatMillis(null), "–");
  assert.equal(formatMillis(6500), "7s");
  assert.equal(formatMillis(1800000), "30m 0s");
  assert.equal(formatMillis(5400000), "1h 30m");
  assert.equal(formatRate("1/2"), "0.50");
  assert.equal(formatRate("101"), "101");
  assert.equal(formatRate(null), "–");
  assert.equal(handlerABHeadline({ items: [] }), "No arm policy saved · 0 finished items");
  assert.equal(handlerABHeadline(report), "Policy revision 2 · enabled · 1 finished item");
});

test("Report renders the per-arm table, item rows and noise flags", () => {
  const html = handlerABHtml(report);
  assert.match(html, /data-handler-ab-arms/);
  assert.match(html, /data-handler-ab-arm="O">O<span class="fine"> gpt-6.1-sol · high/);
  assert.match(html, /<th scope="row">Response median<\/th><td>7s<\/td><td>3s<\/td>/);
  assert.match(html, /<th scope="row">Launch to done median<\/th><td>30m 0s<\/td><td>–<\/td>/);
  assert.match(html, /<th scope="row">Input tokens per request<\/th><td>101<\/td><td>151.50<\/td>/);
  assert.match(html, /<th scope="row">Rotations<\/th><td>0<\/td><td>2<\/td>/);
  assert.match(html, /Refused saves<\/th><td>1 <span class="fine">\(0.33\/item\)<\/span><\/td><td>1 <span class="fine">\(0.50\/item\)/);
  assert.match(html, /data-handler-ab-flag="insufficient">Response time · O vs S: too few items/);
  assert.match(html, /data-handler-ab-flag="within_noise">Refused saves · O vs S: within noise/);
  assert.match(html, /data-handler-ab-flag="difference">Handler tokens · O vs S: difference/);
  assert.match(html, /not a significance test/);
  assert.match(html, /data-handler-ab-item="wi_1111111111111111"><td>First item <span class="fine">· template differs \(policy, other_arm\)<\/span><\/td><td>O <span class="fine">\(fallback from S\)<\/span><\/td><td>1<\/td><td>not done<\/td>/);
  assert.equal(handlerABHtml({ items: [], arms: [] }).includes("<table"), false);
});

test("Report escapes hub text", () => {
  const html = handlerABHtml({
    ...report,
    arms: [{ ...report.arms[0], arm: "<b>x</b>", model: "<img src=x>" }],
    items: [{ ...report.items[0], title: "<script>alert(1)</script>", arm: "<i>" }],
    comparisons: [{ metric: "<svg>", a: "<a>", b: "b", flag: "<u>" }],
  });
  assert.equal(/<b>|<img|<script|<i>|<svg>|<a>|<u>/.test(html), false);
});

test("The Delivery row shows the arm and a fallback", () => {
  assert.equal(handlerArmText(null), "");
  assert.equal(handlerArmText({ arm: "S", drawnArm: "S", fallback: false }), " · arm S");
  assert.equal(handlerArmText({ arm: "O", drawnArm: "S", fallback: true }), " · arm O (fallback from S)");
  const html = renderTeamDelivery(
    {
      entries: [
        {
          id: "tqe_0123456789abcdef",
          itemId: "wi_1111111111111111",
          state: "running",
          handlerId: "agt_handler",
          handlerLeaseGeneration: 4,
          handlerArm: { arm: "O", drawnArm: "S", fallback: true },
          ownership: [],
        },
      ],
    },
    [{ id: "agt_handler", name: "db-handler-sol" }],
  );
  assert.match(html, /Handler db-handler-sol · lease 4 · arm O \(fallback from S\)/);
});

// A minimal DOM stand-in: innerHTML plus the two controls the view binds.
function fakeNode() {
  const controls = {};
  return {
    controls,
    innerHTML: "",
    querySelector(selector) {
      if (!this.innerHTML.includes(selector.slice(1, -1))) return null;
      return (controls[selector] ||= {});
    },
  };
}

test("The disclosure is closed, loads only when opened and keeps saved state", async () => {
  let calls = 0;
  let status = "Live";
  const hub = {
    cacheStatus: () => ({ label: status }),
    handlerABReport: async (task) => {
      calls++;
      assert.equal(task, "tsk_fixture");
      return report;
    },
  };
  const view = createHandlerABView({ client: () => hub });
  const node = fakeNode();
  view.mount(node, "tsk_fixture");
  assert.match(node.innerHTML, /<details class="project-handler-ab" {2}data-handler-ab-disclosure>/);
  assert.match(node.innerHTML, /Not loaded/);
  assert.equal(calls, 0);
  node.controls["[data-handler-ab-disclosure]"].ontoggle({ target: { open: true } });
  await new Promise((resolve) => setTimeout(resolve, 0));
  assert.equal(calls, 1);
  assert.match(node.innerHTML, /<details class="project-handler-ab" open/);
  assert.match(node.innerHTML, /data-handler-ab-arms/);
  // Re-mounting the same project keeps the loaded report without a request.
  view.mount(node, "tsk_fixture");
  assert.equal(calls, 1);
  status = "Saved data · offline";
  view.mount(node, "tsk_fixture");
  assert.match(node.innerHTML, /Saved data · offline/);
  assert.match(node.innerHTML, /data-handler-ab-arms/);
  node.controls["[data-handler-ab-refresh]"].onclick();
  await new Promise((resolve) => setTimeout(resolve, 0));
  assert.equal(calls, 2);
  view.dispose();
});

test("A hub without the route shows an unsupported message", async () => {
  const hub = {
    handlerABReport: async () => {
      throw Object.assign(new Error("not found"), { status: 404 });
    },
  };
  const view = createHandlerABView({ client: () => hub });
  const node = fakeNode();
  view.mount(node, "tsk_fixture");
  node.controls["[data-handler-ab-disclosure]"].ontoggle({ target: { open: true } });
  await new Promise((resolve) => setTimeout(resolve, 0));
  assert.match(node.innerHTML, /Handler A\/B unsupported by this hub/);
  const legacy = {};
  const old = createHandlerABView({ client: () => legacy });
  const other = fakeNode();
  old.mount(other, "tsk_fixture");
  other.controls["[data-handler-ab-disclosure]"].ontoggle({ target: { open: true } });
  await new Promise((resolve) => setTimeout(resolve, 0));
  assert.match(other.innerHTML, /Handler A\/B unsupported by this hub/);
});
