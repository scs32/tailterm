import test from "node:test";
import assert from "node:assert/strict";
import { createFilesView } from "../client/files-view.js";
import { createBoardView } from "../client/board-view.js";
import { createTasksView } from "../client/tasks-view.js";
import { createWorkItemsView } from "../client/work-items-view.js";
const root = () => ({
  innerHTML: "",
  nodes: new Map(),
  querySelector(q) {
    if (!this.nodes.has(q))
      this.nodes.set(q, { value: "", classList: { add() {}, remove() {} } });
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
