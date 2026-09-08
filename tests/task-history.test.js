import test from "node:test";
import assert from "node:assert/strict";
import { readTaskHistory } from "../client/task-history.js";

test("history export reads all pages with separate message/event sequence spaces", async () => {
  const messages = Array.from({ length: 405 }, (_, i) => ({
    seq: i + 1000,
    text: `message ${i}`,
  }));
  const events = Array.from({ length: 203 }, (_, i) => ({ seq: i + 1 }));
  const client = {
    getTask: async () => ({
      task: { id: "task", status: "closed" },
      agents: [],
      latestSeq: 202,
    }),
    listMessages: async (_, { after, limit }) =>
      messages.filter((m) => m.seq > after).slice(0, limit),
    events: async (_, { after, limit }) => ({
      events: events.filter((e) => e.seq > after).slice(0, limit),
    }),
  };
  const history = await readTaskHistory(client, "task");
  assert.deepEqual(history.messages, messages);
  assert.deepEqual(history.events, events.slice(0, 202));
  assert.equal(history.task.status, "closed");
});
