// wi_2f6f24bc62b24a7a / order #13877: owner delegation windows in TailOS.
import test from "node:test";
import assert from "node:assert/strict";
import {
  activeDelegationWindow,
  delegatedAnswers,
  renderDelegationStrip,
  renderOwnerRequests,
} from "../client/owner-obligations.js";
import { createHubClient } from "../client/hub-client.js";
import { createCachedHubClient } from "../client/cached-hub-client.js";
import { createHubReadCache } from "../client/hub-read-cache.js";

const now = Date.parse("2026-09-29T20:00:00Z");
const route = (seq, extra = {}) => ({
  windowId: "dlw_1",
  requestKind: "obligation",
  requestSeq: seq,
  obligationId: "obl_" + seq,
  category: "decision",
  subject: "Request <" + seq + ">",
  routedAt: "2026-09-29T19:00:00Z",
  ...extra,
});
const open = {
  id: "dlw_1",
  delegateName: "lead-x",
  scope: "decisions_merges_deploys",
  state: "open",
  endsAt: "2026-09-29T22:00:00Z",
  routes: [
    route(3),
    route(4, {
      answerSeq: 9,
      answer: "Ship <it>",
      rationale: "Tests pass & the window is quiet.",
      answeredByName: "lead-x",
      answeredAt: "2026-09-29T19:30:00Z",
    }),
  ],
};
const ended = {
  id: "dlw_0",
  delegateName: "old-lead",
  scope: "decisions",
  state: "expired",
  endsAt: "2026-09-29T10:00:00Z",
  routes: [
    route(1, {
      windowId: "dlw_0",
      answerSeq: 2,
      answer: "Earlier",
      rationale: "Earlier rationale",
      answeredAt: "2026-09-29T09:00:00Z",
    }),
    route(5, { windowId: "dlw_0", returnedAt: "2026-09-29T10:00:01Z" }),
  ],
};

test("active window is open and before its end", () => {
  assert.equal(activeDelegationWindow([ended, open], now)?.id, "dlw_1");
  assert.equal(
    activeDelegationWindow([open], Date.parse("2026-09-29T22:00:00Z")),
    null,
  );
  assert.equal(activeDelegationWindow([ended], now), null);
});

test("delegated answers list every window, newest first, after windows end", () => {
  const answers = delegatedAnswers([ended, open]);
  assert.deepEqual(
    answers.map((a) => [a.answerSeq, a.delegateName]),
    [
      [9, "lead-x"],
      [2, "old-lead"],
    ],
  );
});

test("strip shows the open window with End now, escaped answers and rationale", () => {
  const html = renderDelegationStrip([open, ended], { now, answersOpen: true });
  assert.match(html, /Delegated to <strong>lead-x<\/strong> until/);
  assert.match(html, /Decisions, merges and deploys/);
  assert.match(html, /data-delegation-end="dlw_1"/);
  assert.match(html, /Delegated answers \(2\)/);
  assert.match(html, /Ship &lt;it&gt;/);
  assert.match(html, /Rationale: Tests pass &amp; the window is quiet\./);
  assert.match(
    html,
    /<details class="delegated-answers" data-delegated-answers open>/,
  );
  assert.doesNotMatch(html, /data-delegation-open/);
});

test("without a window the owner gets a compact form with live agents only", () => {
  const html = renderDelegationStrip([ended], {
    now,
    agents: [
      { id: "agt_a", name: "lead-x", status: "running" },
      { id: "agt_b", name: "gone", status: "closed" },
    ],
    draft: { delegate: "agt_a", scope: "decisions" },
  });
  assert.match(html, /Delegate decisions…/);
  assert.match(html, /<option value="agt_a" selected>lead-x<\/option>/);
  assert.doesNotMatch(html, /agt_b/);
  assert.match(
    html,
    /type="datetime-local" name="endsAt" required value="\d{4}-\d\d-\d\dT\d\d:\d\d"/,
  );
  assert.match(html, /Matrix approvals always stay with you\./);
  assert.match(html, /Delegated answers \(1\)/);
});

test("closed projects show only past delegated answers, and nothing when there are none", () => {
  assert.equal(renderDelegationStrip([], { archived: true, now }), "");
  const html = renderDelegationStrip([open], { archived: true, now });
  assert.doesNotMatch(html, /data-delegation-end|data-delegation-open/);
  assert.match(html, /Delegated answers \(1\)/);
});

test("routed owner requests carry a Delegated to tag until answered or returned", () => {
  const obligations = [3, 4, 5, 6].map((seq) => ({
    id: "obl_" + seq,
    recipientKind: "owner",
    state: "delivered",
    messageSeq: seq,
    subject: "S" + seq,
    createdAt: "2026-09-29T19:00:00Z",
    dueAt: "2026-09-29T21:00:00Z",
    request: { text: "t" },
  }));
  const html = renderOwnerRequests(obligations, {
    windows: [open, ended],
    now,
  });
  const tagged = [
    ...html.matchAll(/data-owner-request="(obl_\d)"[^]*?<\/article>/g),
  ]
    .filter((m) => m[0].includes("data-delegated-to"))
    .map((m) => m[1]);
  assert.deepEqual(tagged, ["obl_3"]);
  assert.equal(
    renderOwnerRequests(obligations, {
      windows: [open],
      now: Date.parse("2026-09-30T00:00:00Z"),
    }).includes("data-delegated-to"),
    false,
  );
});

test("an unavailable delegation read stays optional: no offline flip, no read loop", async () => {
  let disk;
  const requests = [];
  const live = createHubClient({
    baseURL: "http://fixture",
    token: "t",
    fetchImpl: async (url) => {
      requests.push(url);
      if (url.includes("/delegation-windows"))
        throw Error("Synthetic transport failure");
      return {
        status: 200,
        text: async () =>
          JSON.stringify({ tasks: [{ id: "one", name: "Usable" }] }),
      };
    },
  });
  const view = createCachedHubClient({
    client: live,
    cache: createHubReadCache({
      load: async () => disk,
      save: async (v) => {
        disk = structuredClone(v);
      },
    }),
    online: () => true,
    refreshMs: 60000,
  });
  try {
    await assert.rejects(view.listDelegationWindows("tsk_1"));
    await assert.rejects(view.listDelegationWindows("tsk_1"));
    assert.equal(
      requests.filter((u) => u.includes("/delegation-windows")).length,
      1,
    );
    assert.equal((await view.listTasks())[0].name, "Usable");
    assert.doesNotMatch(view.cacheStatus().label, /offline/i);
  } finally {
    view.dispose();
  }
});
