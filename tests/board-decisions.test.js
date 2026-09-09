import test from "node:test";
import assert from "node:assert/strict";
import {
  captureDecisionPresentation,
  createDecisionDrafts,
  readAllDecisions,
  renderDecisionPanel,
  restoreDecisionPresentation,
} from "../client/board-decisions.js";
import { createHubClient } from "../client/hub-client.js";
import { createCachedHubClient } from "../client/cached-hub-client.js";
import { createHubReadCache } from "../client/hub-read-cache.js";

const request = (seq = 7) => ({
  seq,
  from: { agentId: "agt_worker", user: "owner" },
  decisionRequest: {
    question: "Which <path> should we use?",
    options: [
      {
        id: "staged",
        label: "Staged rollout",
        description: "Limit impact & verify first.",
      },
      {
        id: "all",
        label: "Everyone at once",
        description: "Ship after testing.",
      },
    ],
    recommendedOptionId: "staged",
    recommendationReason: "It keeps the first group small.",
  },
});

test("decision pagination reaches records outside Board message history", async () => {
  const calls = [];
  const client = {
    async listDecisions(task, params) {
      calls.push([task, params]);
      if (!params.after)
        return { decisions: [{ request: request(4) }], nextAfter: 4 };
      return { decisions: [{ request: request(260) }] };
    },
  };
  const records = await readAllDecisions(client, "project", { limit: 1 });
  assert.deepEqual(
    records.map((record) => record.request.seq),
    [4, 260],
  );
  assert.deepEqual(calls, [
    ["project", { after: 0, limit: 1 }],
    ["project", { after: 4, limit: 1 }],
  ]);
});

test("decision pagination rejects a repeated cursor", async () => {
  await assert.rejects(
    readAllDecisions(
      { listDecisions: async () => ({ decisions: [], nextAfter: 2 }) },
      "project",
    ),
    /invalid decision cursor/,
  );
});

test("decision drafts retain exact retries and rotate keys after edits", () => {
  let next = 0;
  const drafts = createDecisionDrafts({ requestId: () => `key-${++next}` });
  const record = { request: request() };
  assert.throws(() => drafts.begin("project", record), /Choose an answer/);
  drafts.update("project", 7, {
    optionId: "staged",
    explanation: "Start with one group.",
  });
  const first = drafts.begin("project", record);
  const retry = drafts.begin("project", record);
  assert.deepEqual(retry, first);
  drafts.update("project", 7, { explanation: "Start with two groups." });
  const edited = drafts.begin("project", record);
  assert.notEqual(edited.requestId, first.requestId);
  assert.equal(edited.optionId, "staged");
  assert.equal(edited.text, "Start with two groups.");
  drafts.update("project", 7, {
    customMode: true,
    custom: "  Wait a week. ",
  });
  assert.deepEqual(drafts.begin("project", record), {
    requestId: "key-3",
    text: "Wait a week.",
  });
});

test("a worker option whose id is custom remains selectable", () => {
  const record = { request: request() };
  record.request.decisionRequest.options[0].id = "custom";
  record.request.decisionRequest.recommendedOptionId = "custom";
  const drafts = createDecisionDrafts({ requestId: () => "stable" });
  drafts.update("project", 7, { optionId: "custom", customMode: false });
  assert.deepEqual(drafts.begin("project", record), {
    requestId: "stable",
    optionId: "custom",
  });
  const html = renderDecisionPanel({
    records: [record],
    taskId: "project",
    archived: false,
    name: () => "Worker",
    drafts,
    sending: new Set(),
    errors: new Map(),
  });
  assert.match(html, /data-decision-option="custom" checked/);
  assert.doesNotMatch(html, /data-decision-custom-choice checked/);
});

test("empty decision projections do not add permanent Board filler", () => {
  assert.equal(
    renderDecisionPanel({
      records: [],
      taskId: "project",
      archived: false,
      name: () => "Worker",
      drafts: createDecisionDrafts({ requestId: () => "stable" }),
      sending: new Set(),
      errors: new Map(),
    }),
    "",
  );
});

test("decision scroll and manually expanded history survive replacement", () => {
  const oldHistory = { open: true };
  const oldPanel = {
    scrollTop: 137,
    querySelector: () => oldHistory,
  };
  const state = captureDecisionPresentation(
    { querySelector: () => oldPanel },
    {},
  );
  assert.deepEqual(state, { scrollTop: 137, historyOpen: true });

  const newHistory = { open: false };
  const newPanel = {
    scrollTop: 0,
    querySelector: () => newHistory,
  };
  restoreDecisionPresentation({ querySelector: () => newPanel }, state);
  assert.equal(newPanel.scrollTop, 137);
  assert.equal(newHistory.open, true);

  const html = renderDecisionPanel({
    records: [
      {
        request: request(),
        answer: {
          seq: 8,
          from: { user: "owner" },
          decisionAnswer: { requestSeq: 7, text: "Done" },
        },
      },
    ],
    taskId: "project",
    archived: false,
    name: () => "Worker",
    drafts: createDecisionDrafts({ requestId: () => "stable" }),
    sending: new Set(),
    errors: new Map(),
    historyOpen: state.historyOpen,
  });
  assert.match(html, /<details class="decision-history" open>/);
});

test("pending decision markup explains every option and marks one recommendation", () => {
  const drafts = createDecisionDrafts({ requestId: () => "stable" });
  const html = renderDecisionPanel({
    records: [{ request: request() }],
    taskId: "project",
    archived: false,
    name: () => "Worker <one>",
    drafts,
    sending: new Set(),
    errors: new Map(),
  });
  assert.match(html, /data-decision-request="7"/);
  assert.match(html, /data-decision-option="staged"/);
  assert.match(html, /data-decision-option="all"/);
  assert.match(html, /Limit impact &amp; verify first/);
  assert.match(html, /Which &lt;path&gt;/);
  assert.match(html, /Worker &lt;one&gt;/);
  assert.equal((html.match(/<strong>Recommended<\/strong>/g) || []).length, 1);
  assert.match(html, /It keeps the first group small/);
  assert.match(html, /data-decision-custom/);
  assert.match(html, /data-decision-explanation/);
  assert.match(html, /data-decision-submit/);
  assert.match(html, /recommendation is not consent/);
});

test("answered and closed decisions render as immutable history", () => {
  const answer = {
    seq: 8,
    from: { user: "owner" },
    decisionAnswer: {
      requestSeq: 7,
      optionId: "staged",
      text: "Proceed carefully.",
    },
  };
  const drafts = createDecisionDrafts({ requestId: () => "stable" });
  const answered = renderDecisionPanel({
    records: [{ request: request(), answer }],
    taskId: "project",
    archived: false,
    name: (message) => message.from.agentId || "You",
    drafts,
    sending: new Set(),
    errors: new Map(),
    revealedAnswers: new Set([7]),
  });
  assert.match(answered, /data-decision-state="answered"/);
  assert.match(answered, /data-decision-answer/);
  assert.match(answered, /Staged rollout/);
  assert.match(answered, /Proceed carefully/);
  assert.match(answered, /<details class="decision-history" open>/);

  const closed = renderDecisionPanel({
    records: [{ request: request() }],
    taskId: "project",
    archived: true,
    name: () => "Worker",
    drafts,
    sending: new Set(),
    errors: new Map(),
  });
  assert.match(closed, /data-decision-state="read-only"/);
  assert.match(closed, /closed.*read-only/i);
  assert.doesNotMatch(closed, /data-decision-submit/);
});

test("hub client uses the decision list and answer contracts exactly", async () => {
  const calls = [];
  const client = createHubClient({
    baseURL: "http://fixture",
    fetchImpl: async (url, init) => {
      calls.push({ url, init });
      return {
        status: init.method === "POST" ? 201 : 200,
        text: async () =>
          JSON.stringify(
            init.method === "POST"
              ? { seq: 8, decisionAnswer: { requestSeq: 7 } }
              : { decisions: [{ request: request() }] },
          ),
      };
    },
  });
  const listed = await client.listDecisions("project", {
    after: 3,
    limit: 100,
  });
  assert.equal(listed.decisions[0].request.seq, 7);
  await client.answerDecision("project", 7, {
    requestId: "answer-key",
    optionId: "staged",
    text: "Why",
  });
  assert.equal(
    calls[0].url,
    "http://fixture/v1/tasks/project/decisions?after=3&limit=100",
  );
  assert.equal(
    calls[1].url,
    "http://fixture/v1/tasks/project/decisions/7/answer",
  );
  assert.deepEqual(JSON.parse(calls[1].init.body), {
    requestId: "answer-key",
    optionId: "staged",
    text: "Why",
  });
});

test("decision cache refreshes without invalidating immutable messages", async () => {
  let disk;
  let answered = false;
  const requests = [];
  const live = createHubClient({
    baseURL: "http://fixture",
    fetchImpl: async (url) => {
      requests.push(url);
      const data = url.includes("/decisions")
        ? {
            decisions: [
              {
                request: request(),
                ...(answered
                  ? {
                      answer: {
                        seq: 8,
                        from: { user: "owner" },
                        decisionAnswer: { requestSeq: 7, text: "Done" },
                      },
                    }
                  : {}),
              },
            ],
          }
        : { messages: [{ seq: 2, text: "Immutable" }] };
      return { status: 200, text: async () => JSON.stringify(data) };
    },
  });
  const cached = createCachedHubClient({
    client: live,
    cache: createHubReadCache({
      load: async () => disk,
      save: async (value) => {
        disk = structuredClone(value);
      },
    }),
  });
  try {
    await cached.listMessages("project", { limit: 200, latest: 1 });
    assert.equal(
      (await cached.listDecisions("project", { after: 0, limit: 100 }))
        .decisions[0].answer,
      undefined,
    );
    answered = true;
    await cached.refreshDecisions("project");
    const refreshed = await cached.listDecisions("project", {
      after: 0,
      limit: 100,
    });
    assert.equal(refreshed.decisions[0].answer.decisionAnswer.text, "Done");
    await cached.listMessages("project", { limit: 200, latest: 1 });
    assert.equal(requests.filter((url) => url.includes("/messages")).length, 1);
  } finally {
    cached.dispose();
  }
});

test("decision refresh drains a stale in-flight page before accepting it", async () => {
  let disk;
  let decisionReads = 0;
  let releaseStale;
  let staleStarted;
  const staleGate = new Promise((resolve) => {
    releaseStale = resolve;
  });
  const started = new Promise((resolve) => {
    staleStarted = resolve;
  });
  const pending = { decisions: [{ request: request() }] };
  const resolved = {
    decisions: [
      {
        request: request(),
        answer: {
          seq: 8,
          from: { user: "owner" },
          decisionAnswer: { requestSeq: 7, text: "Winner" },
        },
      },
    ],
  };
  const live = createHubClient({
    baseURL: "http://fixture",
    fetchImpl: async (url) => {
      assert.match(url, /\/decisions/);
      decisionReads++;
      if (decisionReads === 2) {
        staleStarted();
        await staleGate;
        return { status: 200, text: async () => JSON.stringify(pending) };
      }
      return {
        status: 200,
        text: async () =>
          JSON.stringify(decisionReads > 2 ? resolved : pending),
      };
    },
  });
  const cached = createCachedHubClient({
    client: live,
    cache: createHubReadCache({
      load: async () => disk,
      save: async (value) => {
        disk = structuredClone(value);
      },
    }),
  });
  try {
    await cached.listDecisions("project", { after: 0, limit: 100 });
    const background = cached.refreshConnection();
    await started;
    const refresh = cached.refreshDecisions("project");
    releaseStale();
    await background;
    await refresh;
    const latest = await cached.listDecisions("project", {
      after: 0,
      limit: 100,
    });
    assert.equal(latest.decisions[0].answer.decisionAnswer.text, "Winner");
    assert.equal(decisionReads, 3);
  } finally {
    cached.dispose();
  }
});
