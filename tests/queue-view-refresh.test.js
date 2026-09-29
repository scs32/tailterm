// Queue view refresh ordering for wi_a9a69169f732121d, order #13847.
// Event-driven refreshes can overlap the view's own actions; each case pins one
// interleaving with a controllable fake client instead of browser timing.
import test, { after } from "node:test";
import assert from "node:assert/strict";
import { createQueueView } from "../client/queue-view.js";

const TASK = "tsk_target";
const views = [];
// Stop each view's owner-age clock even when an assertion fails first.
after(() => views.forEach((view) => view.hide()));

function deferred() {
  let resolve, reject;
  const promise = new Promise((ok, fail) => {
    resolve = ok;
    reject = fail;
  });
  return { promise, resolve, reject };
}

const settle = async () => {
  for (let i = 0; i < 20; i++) await new Promise((r) => setImmediate(r));
};

function entry(revision, queuePriority = "normal") {
  return {
    id: "que_one",
    targetTaskId: TASK,
    sourceTaskId: "tsk_source",
    itemId: "wi_one",
    item: {
      title: "Synthetic entry",
      kind: "bug",
      description: "",
      status: "open",
    },
    offeredItemRevision: 1,
    currentItemRevision: 1,
    state: "waiting",
    cycle: 1,
    revision,
    queuePriority,
    eligible: true,
    orchestratorAgentId: "agt_lead",
  };
}

// A minimal root: render writes innerHTML; the view binds its priority select and
// retry buttons through querySelector/querySelectorAll, which the test drives.
function fakeRoot() {
  const root = {
    innerHTML: "",
    priority: null,
    retry: [],
    querySelector(selector) {
      if (selector === "[data-queue-priority]")
        return {
          addEventListener: (type, handler) => (root.priority = handler),
        };
      return null;
    },
    querySelectorAll(selector) {
      if (selector !== "[data-queue-retry-intent]") return [];
      root.retry = [
        ...root.innerHTML.matchAll(/data-queue-retry-intent="([^"]+)"/g),
      ].map((match) => ({ dataset: { queueRetryIntent: match[1] } }));
      return root.retry;
    },
  };
  return root;
}

// Every list and action call waits for the test unless it is released at once.
function fakeClient() {
  const client = {
    base: "http://queue.invalid",
    token: "",
    lists: [],
    actions: [],
    subscriber: null,
    capabilities: async () => ({ queue: { versions: [1] } }),
    listTasks: async () => [{ id: TASK, name: "Target", status: "open" }],
    listQueue() {
      const call = deferred();
      client.lists.push(call);
      return call.promise;
    },
    queueAction(taskId, entryId, payload) {
      const call = deferred();
      client.actions.push({ payload, ...call });
      return call.promise;
    },
    subscribe(_scope, handler) {
      client.subscriber = handler;
      return { stop() {} };
    },
  };
  return client;
}

async function mountedView() {
  const client = fakeClient(),
    root = fakeRoot(),
    notices = [];
  const view = createQueueView({
    client: () => client,
    configure() {},
    notice: (text) => notices.push(text),
    dialog() {},
    closeDialog() {},
  });
  views.push(view);
  view.mount(root);
  const shown = view.show(TASK);
  await settle();
  client.lists.shift().resolve({ entries: [entry(1)] });
  await shown;
  return { client, root, view, notices };
}

async function changePriority(root, value) {
  root.priority({ target: { value } });
  await settle();
}

test("a refresh during an action keeps its lost-response error and retry", async () => {
  const { client, root, view } = await mountedView();
  await changePriority(root, "high");
  const action = client.actions.shift();
  // The hub committed the change and its event refreshed the view first.
  client.subscriber();
  await settle();
  client.lists.shift().resolve({ entries: [entry(2, "high")] });
  await settle();
  action.reject(new Error("Synthetic lost Queue response"));
  await settle();
  assert.match(root.innerHTML, /Synthetic lost Queue response/);
  assert.match(root.innerHTML, /Retry exact request/);
  view.hide();
});

test("a refresh during a successful action keeps its result notice", async () => {
  const { client, root, view, notices } = await mountedView();
  await changePriority(root, "urgent");
  const action = client.actions.shift();
  client.subscriber();
  await settle();
  action.resolve({ entry: entry(2, "urgent") });
  await settle();
  client.lists.shift().resolve({ entries: [entry(2, "urgent")] });
  await settle();
  assert.match(
    notices.at(-1) || "",
    /Queue revision 2\. No agent was started\./,
  );
  view.hide();
});

test("a refresh read before an action commits cannot restore the older entry", async () => {
  const { client, root, view } = await mountedView();
  client.subscriber();
  await settle();
  const staleList = client.lists.shift();
  await changePriority(root, "urgent");
  client.actions.shift().resolve({ entry: entry(2, "urgent") });
  await settle();
  staleList.resolve({ entries: [entry(1)] });
  await settle();
  await changePriority(root, "high");
  assert.equal(client.actions.shift().payload.expectedRevision, 2);
  view.hide();
});

test("a refresh snapshot cannot drop an intent saved after it", async () => {
  const { client, root, view } = await mountedView();
  client.subscriber();
  await settle();
  const list = client.lists.shift(); // this refresh already read the intents
  await changePriority(root, "high");
  client.actions.shift().reject(new Error("Synthetic lost Queue response"));
  await settle();
  list.resolve({ entries: [entry(2, "high")] });
  await settle();
  assert.match(root.innerHTML, /Retry exact request/);
  assert.match(root.innerHTML, /Synthetic lost Queue response/);
  view.hide();
});

test("a refresh snapshot cannot resurrect an intent settled after it", async () => {
  const { client, root, view } = await mountedView();
  await changePriority(root, "high");
  client.actions.shift().reject(new Error("Synthetic lost Queue response"));
  await settle();
  assert.equal(root.retry.length, 1);
  client.subscriber();
  await settle();
  const list = client.lists.shift(); // this refresh read the uncertain intent
  const retry = root.retry[0];
  root.retry = [];
  retry.onclick();
  await settle();
  const replay = client.actions.shift();
  replay.resolve({ entry: entry(2, "high") });
  await settle();
  list.resolve({ entries: [entry(2, "high")] });
  await settle();
  assert.doesNotMatch(root.innerHTML, /Retry exact request/);
  view.hide();
});

test("hiding the view still drops a late action result", async () => {
  const { client, root, view, notices } = await mountedView();
  await changePriority(root, "urgent");
  const action = client.actions.shift();
  const before = notices.length;
  view.hide();
  action.resolve({ entry: entry(2, "urgent") });
  await settle();
  assert.equal(notices.length, before);
});

test("switching project before a lost response keeps its error off the new project", async () => {
  const { client, root, view } = await mountedView();
  client.listTasks = async () => [
    { id: TASK, name: "Target", status: "open" },
    { id: "tsk_other", name: "Other", status: "open" },
  ];
  await changePriority(root, "high");
  const action = client.actions.shift();
  const shown = view.show("tsk_other");
  await settle();
  client.lists.shift().resolve({ entries: [] });
  await shown;
  action.reject(new Error("Synthetic lost Queue response"));
  await settle();
  assert.match(
    root.innerHTML,
    /data-queue-task="tsk_other" aria-pressed="true"/,
  );
  assert.doesNotMatch(root.innerHTML, /Synthetic lost Queue response/);
});
