import test from "node:test";
import assert from "node:assert/strict";
import { createFilesView } from "../client/files-view.js";
import {
  createBoardView,
  shouldReleaseRailPointer,
} from "../client/board-view.js";
import { createTasksView } from "../client/tasks-view.js";
import { createWorkItemsView } from "../client/work-items-view.js";
const root = () => ({
  innerHTML: "",
  nodes: new Map(),
  querySelector(q) {
    if (!this.nodes.has(q))
      this.nodes.set(q, {
        value: "",
        classList: { add() {}, remove() {} },
        querySelector: () => null,
        querySelectorAll: () => [],
      });
    return this.nodes.get(q);
  },
  querySelectorAll() {
    return [];
  },
});
const deferred = () => {
  let resolve;
  const promise = new Promise((r) => (resolve = r));
  return { promise, resolve };
};
globalThis.localStorage = { getItem: () => null, setItem() {} };
globalThis.document = { activeElement: null };

test("Board rail pointer holds release outside, on cancel, or on window blur", () => {
  assert.equal(
    shouldReleaseRailPointer(7, { type: "pointerup", pointerId: 7 }),
    true,
  );
  assert.equal(
    shouldReleaseRailPointer(7, { type: "pointercancel", pointerId: 7 }),
    true,
  );
  assert.equal(shouldReleaseRailPointer(7, { type: "blur" }), true);
  assert.equal(
    shouldReleaseRailPointer(7, { type: "pointerup", pointerId: 8 }),
    false,
  );
  assert.equal(shouldReleaseRailPointer(null, { type: "blur" }), false);
});

test("switching SFTP servers cancels remaining files in an upload batch", async () => {
  const servers = [
    { id: "a", name: "A", host: "a" },
    { id: "b", name: "B", host: "b" },
  ];
  const writes = [],
    barrier = deferred();
  const sftp = (s) => ({
    done: new Promise(() => {}),
    close() {},
    home: async () => "/home/" + s.id,
    realpath: async (p) => p,
    list: async (p) => ({ path: p, entries: [] }),
    write: async (p) => {
      writes.push({ server: s.id, path: p });
      if (s.id === "a") await barrier.promise;
    },
  });
  const files = createFilesView({
    getServers: () => servers,
    currentServer: () => servers[0],
    openSFTP: async (s) => sftp(s),
    notice() {},
    confirm() {},
    dialog() {},
    closeDialog() {},
    openTerminal() {},
  });
  const el = root();
  files.mount(el);
  await files.show();
  el.querySelector("#files-upload").onchange({
    target: {
      files: [
        { name: "one.txt", size: 1 },
        { name: "two.txt", size: 1 },
      ],
      value: "",
    },
  });
  assert.equal(writes.length, 1);
  await el.querySelector("#files-server").onchange({ target: { value: "b" } });
  barrier.resolve();
  await new Promise((r) => setImmediate(r));
  assert.deepEqual(writes, [{ server: "a", path: "/home/a/one.txt" }]);
  files.hide();
});
for (const [name, create] of [
  ["Board", createBoardView],
  ["Tasks", createTasksView],
  ["Bugs", (props) => createWorkItemsView({ ...props, kind: "bug" })],
]) {
  test(`${name} cannot repaint or start polling after hiding during a request`, async () => {
    const barrier = deferred();
    let subscriptions = 0;
    const view = create({
      client: () => ({
        listTasks: () => barrier.promise,
        subscribe() {
          subscriptions++;
          return { stop() {} };
        },
      }),
    });
    const el = root();
    view.mount(el);
    const showing = view.show();
    view.hide();
    el.innerHTML = "Files view";
    barrier.resolve([]);
    await showing;
    assert.equal(el.innerHTML, "Files view");
    assert.equal(subscriptions, 0);
  });
}
test("Board handles a second show while the first request is pending", async () => {
  const first = deferred();
  let requests = 0,
    subscriptions = 0;
  const view = createBoardView({
    client: () => ({
      listTasks: () => (++requests === 1 ? first.promise : Promise.resolve([])),
      subscribe() {
        subscriptions++;
        return { stop() {} };
      },
    }),
  });
  const el = root();
  view.mount(el);
  const old = view.show();
  await view.show();
  assert.match(el.innerHTML, /board-thread/);
  assert.doesNotMatch(el.innerHTML, /Bring a team together/);
  first.resolve([]);
  await old;
  assert.equal(subscriptions, 1);
  view.hide();
});
test("Board keeps known controls and audits during a same-client refresh", async () => {
  const task = { id: "tsk_1111111111111111", name: "Project", status: "open" };
  const messages = [
    {
      seq: 1,
      from: { user: "owner" },
      text: "Message",
      createdAt: "2026-09-25T00:00:00Z",
    },
  ];
  const heldItems = deferred(),
    itemRefreshStarted = deferred();
  let refresh,
    capabilityCalls = 0,
    itemCalls = 0;
  const client = {
    listTasks: async () => [task],
    getTask: async () => ({ task, agents: [] }),
    listMessages: async () => messages,
    listDecisions: async () => ({ decisions: [], nextAfter: 0 }),
    capabilities: async () => {
      capabilityCalls++;
      return {
        messageAudit: { versions: [2] },
        auditExport: { versions: [2] },
      };
    },
    loadMessageAudits: async () => ({
      1: {
        original: { revision: 1, classification: "valid" },
        current: { revision: 1, classification: "valid" },
      },
    }),
    listWorkItems: async () => {
      itemCalls++;
      if (itemCalls === 2) itemRefreshStarted.resolve();
      return itemCalls === 2 ? heldItems.promise : { items: [], next: 0 };
    },
    subscribe(_path, callback) {
      refresh = callback;
      return { stop() {} };
    },
  };
  const view = createBoardView({ client: () => client, notice() {} });
  const el = root();
  view.mount(el);
  await view.show();
  assert.equal(capabilityCalls, 1);
  assert.match(el.innerHTML, /board-audit-export/);
  assert.match(el.innerHTML, /data-audit-kind="valid"/);
  assert.match(el.innerHTML, /board-audit-compose/);
  const pending = refresh();
  await itemRefreshStarted.promise;
  assert.equal(capabilityCalls, 1);
  assert.match(el.innerHTML, /board-audit-export/);
  assert.match(el.innerHTML, /data-audit-kind="valid"/);
  assert.match(el.innerHTML, /board-audit-compose/);
  assert.doesNotMatch(el.innerHTML, /Legacy JSON/);
  heldItems.resolve({ items: [], next: 0 });
  await pending;
  assert.equal(capabilityCalls, 1);
  view.hide();
});
test("Board reports a failure after its cached conversation paints", async () => {
  const task = { id: "tsk_1111111111111111", name: "Project", status: "open" };
  const notices = [];
  let failCapabilities = true,
    refresh,
    subscriptions = 0;
  const client = {
    listTasks: async () => [task],
    getTask: async () => ({ task, agents: [] }),
    listMessages: async () => [],
    listDecisions: async () => ({ decisions: [], nextAfter: 0 }),
    capabilities: async () => {
      if (!failCapabilities) return { auditExport: { versions: [2] } };
      const error = new Error("capabilities failed");
      error.status = 500;
      throw error;
    },
    subscribe(_path, callback) {
      subscriptions++;
      refresh = callback;
      return { stop() {} };
    },
  };
  const view = createBoardView({
    client: () => client,
    notice: (text) => notices.push(text),
  });
  const el = root();
  view.mount(el);
  await view.show();
  assert.match(el.innerHTML, /board-thread/);
  assert.ok(
    notices.some((text) =>
      /Board refresh unavailable: capabilities failed/.test(text),
    ),
  );
  assert.equal(subscriptions, 1);
  failCapabilities = false;
  await refresh();
  assert.match(el.innerHTML, /board-audit-export/);
  view.hide();
});
test("Projects switches clients while a cached load is pending", async () => {
  const first = deferred();
  let subscriptions = 0;
  const oldClient = { listTasks: () => first.promise };
  const newClient = {
    listTasks: async () => [],
    capabilities: async () => ({}),
    subscribe() {
      subscriptions++;
      return { stop() {} };
    },
  };
  let current = oldClient;
  const view = createTasksView({ client: () => current });
  const el = root();
  view.mount(el);
  const old = view.show();
  current = newClient;
  first.resolve([]);
  await old;
  await new Promise((resolve) => setImmediate(resolve));
  assert.match(el.innerHTML, /No projects yet/);
  assert.equal(subscriptions, 1);
  view.hide();
});
test("Projects keeps a known Pause control during a same-client refresh", async () => {
  const task = {
    id: "tsk_1111111111111111",
    name: "Project",
    status: "open",
    pauseState: "active",
    lifecycleGeneration: 1,
  };
  const heldCapabilities = deferred(),
    capabilityRefreshStarted = deferred();
  let refresh,
    capabilityCalls = 0;
  const client = {
    listTasks: async () => [task],
    getTask: async () => ({ task, agents: [] }),
    capabilities: async () => {
      capabilityCalls++;
      if (capabilityCalls === 2) {
        capabilityRefreshStarted.resolve();
        await heldCapabilities.promise;
      }
      return { projectPause: { supported: true, versions: [1] } };
    },
    subscribe(_path, callback) {
      refresh = callback;
      return { stop() {} };
    },
  };
  const view = createTasksView({
    client: () => client,
    taskHub: { groupOf: () => null },
    getTabs: () => [],
  });
  const el = root();
  view.mount(el);
  await view.show();
  const pause = () =>
    el.innerHTML.match(/<button data-task-pause="[^"]*"[^>]*>/)?.[0];
  assert.ok(pause());
  assert.doesNotMatch(pause(), /disabled/);
  const pending = refresh();
  await capabilityRefreshStarted.promise;
  assert.equal(pause().includes("disabled"), false);
  heldCapabilities.resolve();
  await pending;
  assert.doesNotMatch(pause(), /disabled/);
  view.hide();
});
test("work item pagination keeps one project scope while a new filter is pending", async () => {
  const first = deferred(),
    calls = [];
  const client = {
    listTasks: async () => [
      { id: "a", name: "A", status: "open" },
      { id: "b", name: "B", status: "open" },
    ],
    listWorkItems: async (p) => {
      calls.push(p);
      if (calls.length === 1) return first.promise;
      return { items: [], next: 0 };
    },
    subscribe: () => ({ stop() {} }),
  };
  const view = createWorkItemsView({ kind: "bug", client: () => client }),
    el = root();
  view.mount(el);
  const initial = view.show("a");
  await new Promise((r) => setImmediate(r));
  await view.show("b");
  first.resolve({
    items: [{ id: "old", seq: 1, taskId: "a", title: "Old project result" }],
    next: 1,
  });
  await initial;
  await new Promise((r) => setImmediate(r));
  assert.deepEqual(
    calls.map((c) => [c.taskId, c.after]),
    [
      ["a", 0],
      ["a", 1],
      ["b", 0],
    ],
  );
  assert.doesNotMatch(el.innerHTML, /Old project result/);
  view.hide();
});
