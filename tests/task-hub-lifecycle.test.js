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

test("existing task terminals resolve a machine's different local hostname", async () => {
  const taskId = "tsk_0123456789abcdef";
  const agent = {
    id: "agt_0123456789abcdef",
    name: "orchestrator",
    host: "Stephens-Mini",
    session: "orchestrator",
    status: "needs_input",
  };
  const machine = {
    id: "mini",
    name: "Mini",
    host: "stephens-mac-mini.tail123.ts.net",
    username: "stephen",
  };
  const connected = [],
    probes = [],
    notices = [],
    groups = [];
  const hub = createTaskHub({
    getData: () => ({ hub: { url: "http://hub" } }),
    getIPN: () => ({
      fetch: async (url) => {
        if (url.includes("/events")) return new Promise(() => {});
        return {
          status: 200,
          text: async () =>
            JSON.stringify({
              task: { id: taskId, name: "Best dad joke", status: "open" },
              agents: [agent],
              latestSeq: 0,
            }),
        };
      },
    }),
    getServers: () => [machine],
    getTabs: () => [],
    browserCommand: async (server, command) => {
      probes.push([server.id, command]);
      return "Stephens-Mini\n";
    },
    paneGroups: () => ({
      model: {
        groups,
        taskGroup: () => groups.find((g) => g.taskId === taskId),
        group: () => groups[0],
      },
      sync() {},
    }),
    connect: async (server, tmux, session, options) => {
      connected.push({ server, session, options });
      groups.push({ active: "pane" });
      return { id: "pane" };
    },
    render() {},
    scheduleWorkspaceSave() {},
    bookmark() {},
    closeTab() {},
    notice: (s) => notices.push(s),
  });
  hub.refresh();
  hub.restore([taskId]);
  for (let i = 0; i < 10 && !connected.length; i++)
    await new Promise((r) => setImmediate(r));
  hub.stopAll();
  assert.deepEqual(probes, [["mini", "hostname -s"]]);
  assert.equal(connected.length, 1);
  assert.equal(connected[0].server.id, "mini");
  assert.equal(connected[0].session, "orchestrator");
  assert.equal(connected[0].options.resumeOnly, true);
  assert.equal(groups[0].taskId, taskId);
  assert.equal(hub.matchServer("Stephens-Mini").id, "mini");
  assert.deepEqual(notices, []);
});

test("individual cleanup retries one closed agent while its project stays open", async () => {
  const taskId = "tsk_0123456789abcdef";
  const agentId = "agt_0123456789abcdef";
  let cleaned = false;
  const commands = [];
  const detail = () => ({
    task: { id: taskId, name: "Open project", status: "open" },
    agents: [
      {
        id: agentId,
        runId: "run_0123456789abcdef",
        name: "finished-worker",
        host: "Mini",
        session: "finished-worker",
        status: "closed",
        cleanupDone: cleaned,
      },
    ],
    latestSeq: 0,
  });
  const machine = { id: "mini", name: "Mini", host: "Mini" };
  const hub = createTaskHub({
    getData: () => ({ hub: { url: "http://hub" } }),
    getIPN: () => ({
      fetch: async () => ({
        status: 200,
        text: async () => JSON.stringify(detail()),
      }),
    }),
    getServers: () => [machine],
    getTabs: () => [],
    paneGroups: () => ({ model: { groups: [] } }),
    browserCommand: async (server, command) => {
      commands.push([server.id, command]);
      cleaned = true;
      return JSON.stringify({ confirmed: 1, errors: [] });
    },
    render() {},
  });
  hub.refresh();
  const result = await hub.cleanupAgent(taskId, agentId);
  assert.equal(result.cleanupDone, true);
  assert.deepEqual(result.cleanupErrors, []);
  assert.equal(commands.length, 1);
  assert.equal(commands[0][0], "mini");
  assert.match(commands[0][1], new RegExp(agentId));
  assert.equal(detail().task.status, "open");
});
