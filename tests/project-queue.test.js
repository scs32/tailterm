import test from "node:test";
import assert from "node:assert/strict";
import { createHubClient } from "../client/hub-client.js";
import { MODES } from "../client/modes.js";

test("Queue follows Features while Files remains hidden", () => {
  const ids = MODES.map(([id]) => id);
  assert.equal(ids.at(ids.indexOf("features") + 1), "queue");
  assert.equal(ids.includes("files"), false);
  assert.equal(new Set(MODES.map(([, , key]) => key)).size, MODES.length);
});

test("Queue client preserves typed frozen, action, history, change and receipt contracts", async () => {
  const calls = [];
  const client = createHubClient({
    baseURL: "http://fixture",
    fetchImpl: async (url, init) => {
      calls.push({ url, init });
      return {
        status: init.method === "POST" ? 201 : 200,
        text: async () => JSON.stringify({ entries: [], events: [] }),
      };
    },
  });
  await client.listQueue("tsk_one", {
    cursor: "frozen",
    limit: 17,
    includeTerminal: 1,
  });
  await client.getQueueEntry("tsk_one", "que_one");
  await client.listQueueHistory("tsk_one", "que_one", {
    cursor: "history",
    limit: 19,
  });
  await client.listQueueChanges("tsk_one", {
    after: 7,
    cutoff: 12,
    limit: 23,
  });
  const action = {
    operation: "priority",
    requestId: "same-exact-key",
    expectedRevision: 3,
    cycle: 1,
    priority: "urgent",
  };
  await client.queueAction("tsk_one", "que_one", action);
  await client.getQueueReceipt("tsk_one", "same/exact key", "agt_one");
  assert.deepEqual(
    calls.map((call) => call.url),
    [
      "http://fixture/v1/tasks/tsk_one/queue?cursor=frozen&limit=17&includeTerminal=1",
      "http://fixture/v1/tasks/tsk_one/queue/que_one",
      "http://fixture/v1/tasks/tsk_one/queue/que_one/history?cursor=history&limit=19",
      "http://fixture/v1/tasks/tsk_one/queue/changes?after=7&cutoff=12&limit=23",
      "http://fixture/v1/tasks/tsk_one/queue/que_one/actions",
      "http://fixture/v1/tasks/tsk_one/queue-receipts/same%2Fexact%20key?agentId=agt_one",
    ],
  );
  assert.deepEqual(JSON.parse(calls[4].init.body), action);
  assert.equal(calls[4].init.method, "POST");
  assert.equal(calls.filter((call) => call.init.method === "POST").length, 1);
});
