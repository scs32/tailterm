import test from "node:test";
import assert from "node:assert/strict";
import { createTaskHub } from "../client/task-hub.js";

test("deleted tasks are forgotten without closing terminals or retrying", async () => {
  const id = "tsk_0123456789abcdef";
  const tab = {
    id: "tab1",
    task: { taskId: id, agentId: "agt_0123456789abcdef" },
  };
  const group = { taskId: id };
  const notices = [],
    bookmarks = [],
    cleared = [];
  let requests = 0,
    saves = 0;
  const hub = createTaskHub({
    getData: () => ({ hub: { url: "http://hub" } }),
    getIPN: () => ({
      fetch: async () => {
        requests++;
        return { status: 404, text: async () => '{"error":"not found"}' };
      },
    }),
    paneGroups: () => ({ model: { groups: [group] } }),
    getTabs: () => [tab],
    bookmark: (t) => bookmarks.push(t),
    clearTaskBookmarks: (id) => cleared.push(id),
    notice: (s) => notices.push(s),
    scheduleWorkspaceSave: () => saves++,
    render() {},
  });
  hub.refresh();
  hub.restore([id]);
  await new Promise((r) => setImmediate(r));
  assert.deepEqual(hub.bound(), []);
  assert.equal(group.taskId, undefined);
  assert.equal(tab.task, undefined);
  assert.deepEqual(notices, []);
  assert.deepEqual(cleared, [id]);
  assert.equal(bookmarks.length, 1);
  assert.ok(saves > 0);
  hub.sync();
  assert.equal(requests, 1);
});
