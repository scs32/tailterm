import test from "node:test";
import assert from "node:assert/strict";
import { createHubClient } from "../client/hub-client.js";
import { createHubReadCache } from "../client/hub-read-cache.js";
import { createCachedHubClient } from "../client/cached-hub-client.js";

function fixture() {
  let disk,
    connected = true,
    response = { tasks: [{ id: "one", name: "Saved project" }] };
  let gate,
    status = 200,
    requests = [];
  const cache = () =>
    createHubReadCache({
      load: async () => disk,
      save: async (value) => {
        disk = structuredClone(value);
      },
    });
  const live = createHubClient({
    baseURL: "http://fixture",
    token: "first",
    fetchImpl: async (url, init) => {
      requests.push({ url, method: init.method });
      if (!connected) throw Error("Synthetic offline");
      const body = gate ? await gate : structuredClone(response);
      return { status, text: async () => JSON.stringify(body) };
    },
  });
  const make = (client = live) =>
    createCachedHubClient({ client, cache: cache(), online: () => connected });
  return {
    live,
    cache,
    make,
    requests,
    offline() {
      connected = false;
    },
    response(value) {
      response = value;
    },
    hold(value) {
      gate = value;
    },
    status(value) {
      status = value;
    },
  };
}

test("saved views return before a slow refresh and update when it finishes", async () => {
  const f = fixture(),
    view = f.make();
  try {
    assert.equal((await view.listTasks())[0].name, "Saved project");
    let finish;
    f.hold(
      new Promise((resolve) => {
        finish = resolve;
      }),
    );
    const refreshing = view.refreshConnection();
    const value = await Promise.race([
      view.listTasks(),
      new Promise((_, reject) =>
        setTimeout(() => reject(Error("cached read waited for network")), 250),
      ),
    ]);
    assert.equal(value[0].name, "Saved project");
    assert.match(view.cacheStatus().label, /refreshing/);
    finish({ tasks: [{ id: "one", name: "Updated project" }] });
    await refreshing;
    assert.equal((await view.listTasks())[0].name, "Updated project");
  } finally {
    view.dispose();
  }
});

test("routine saved-status transitions do not notify views, but data and errors do", async () => {
  const f = fixture();
  const view = f.make({
    ...f.live,
    subscribe: () => ({ stop() {}, cursor: 0 }),
  });
  const settle = () => new Promise((resolve) => setTimeout(resolve, 10));
  try {
    await view.listTasks();
    await settle();
    let notifications = 0;
    view.subscribe("", () => notifications++);
    await view.refreshConnection();
    await settle();
    assert.equal(notifications, 0, "unchanged save and refresh labels are quiet");

    f.response({ tasks: [{ id: "one", name: "Updated project" }] });
    await view.refreshConnection();
    await settle();
    assert.equal(notifications, 1, "changed content still notifies views");

    f.offline();
    await view.refreshConnection();
    await settle();
    assert.equal(notifications, 2, "visible offline status still notifies views");
  } finally {
    view.dispose();
  }
});

test("reload can read saved data offline while the authoritative client cannot", async () => {
  const f = fixture(),
    first = f.make();
  await first.listTasks();
  first.dispose();
  f.offline();
  const restored = f.make();
  try {
    assert.equal((await restored.listTasks())[0].name, "Saved project");
    assert.match(restored.cacheStatus().label, /offline/);
    await assert.rejects(f.live.listTasks(), /Synthetic offline/);
  } finally {
    restored.dispose();
  }
});

test("credential scopes do not share saved records", async () => {
  const f = fixture(),
    first = f.make();
  await first.listTasks();
  first.dispose();
  f.offline();
  const other = f.make({ ...f.live, token: "different" });
  try {
    await assert.rejects(other.listTasks(), /not been saved/);
  } finally {
    other.dispose();
  }
});

test("confirmed writes invalidate saved reads and failed writes never replay", async () => {
  const f = fixture(),
    view = f.make();
  try {
    await view.listTasks();
    f.response({ tasks: [{ id: "one", name: "Confirmed update" }] });
    await view.postMessage("one", { text: "synthetic" });
    assert.equal((await view.listTasks())[0].name, "Confirmed update");
    f.offline();
    await assert.rejects(
      view.postMessage("one", { text: "do not queue" }),
      /Synthetic offline/,
    );
    await view.listTasks();
    await view.refreshConnection();
    assert.equal(f.requests.filter((r) => r.method === "POST").length, 2);
  } finally {
    view.dispose();
  }
});

test("authorization errors clear saved records instead of serving them as offline", async () => {
  const f = fixture(),
    view = f.make();
  try {
    await view.listTasks();
    f.status(401);
    f.response({ error: "Expired credential" });
    await view.refreshConnection();
    await assert.rejects(view.listTasks(), /Expired credential/);
    view.dispose();
    f.offline();
    const restored = f.make();
    try {
      await assert.rejects(restored.listTasks(), /not been saved/);
    } finally {
      restored.dispose();
    }
  } finally {
    view.dispose();
  }
});

test("locking during refresh prevents the old response from entering the cache", async () => {
  const f = fixture(),
    view = f.make();
  await view.listTasks();
  let finish;
  f.hold(
    new Promise((resolve) => {
      finish = resolve;
    }),
  );
  const refresh = view.refreshConnection();
  view.dispose();
  finish({ tasks: [{ id: "private", name: "Late response" }] });
  await refresh;
  f.offline();
  const restored = f.make();
  try {
    assert.equal((await restored.listTasks())[0].name, "Saved project");
  } finally {
    restored.dispose();
  }
});

test("Board refreshes fetch only new immutable messages after the saved cursor", async () => {
  let disk;
  const requests = [];
  const client = createHubClient({
    baseURL: "http://fixture",
    fetchImpl: async (url) => {
      requests.push(url);
      const after = new URL(url).searchParams.get("after");
      return {
        status: 200,
        text: async () =>
          JSON.stringify({
            messages: after
              ? [{ seq: 4, text: "New" }]
              : [
                  { seq: 2, text: "Old" },
                  { seq: 3, text: "Saved" },
                ],
          }),
      };
    },
  });
  const view = createCachedHubClient({
    client,
    cache: createHubReadCache({
      load: async () => disk,
      save: async (value) => {
        disk = value;
      },
    }),
  });
  try {
    await view.listMessages("one", { limit: 200, latest: 1 });
    await view.refreshConnection();
    assert.match(requests[1], /after=3/);
    assert.doesNotMatch(requests[1], /latest=/);
    assert.deepEqual(
      (await view.listMessages("one", { limit: 200, latest: 1 })).map(
        (m) => m.seq,
      ),
      [2, 3, 4],
    );
  } finally {
    view.dispose();
  }
});

test("audit overlay advances only after a complete unfiltered frozen window", async () => {
  let disk;
  let failSecond = true;
  const calls = [];
  const client = {
    base: "http://fixture",
    token: "scope",
    onMutation: () => () => {},
    subscribe: () => ({ stop() {}, cursor: () => 0 }),
    listMessageAuditChanges: async (_, params) => {
      calls.push(structuredClone(params));
      if (!params.cursor)
        return {
          events: [
            {
              message: { taskId: "one", seq: 4 },
              after: { revision: 1, classification: "intake", workItems: [] },
            },
          ],
          nextCursor: "page-two",
        };
      if (failSecond) throw new Error("partial window");
      return {
        events: [
          {
            message: { taskId: "one", seq: 4 },
            after: {
              revision: 2,
              classification: "work",
              workItems: [],
            },
          },
        ],
        checkpoint: "complete-checkpoint",
      };
    },
  };
  const make = () =>
    createCachedHubClient({
      client,
      cache: createHubReadCache({
        load: async () => disk,
        save: async (value) => {
          disk = structuredClone(value);
        },
      }),
    });
  const first = make();
  await assert.rejects(first.refreshMessageAuditOverlay("one"), /partial/);
  first.dispose();
  assert.equal(disk, undefined, "partial window persisted an audit checkpoint");
  failSecond = false;
  const second = make();
  const complete = await second.refreshMessageAuditOverlay("one");
  assert.equal(complete.checkpoint, "complete-checkpoint");
  assert.equal(complete.records["4"].current.revision, 2);
  second.dispose();
  assert.equal(calls[2].checkpoint, "", "partial window incorrectly advanced");
});

test("empty audit window checkpoint and exact historical bootstrap survive offline", async () => {
  let disk;
  let connected = true;
  const client = {
    base: "http://fixture",
    token: "scope",
    onMutation: () => () => {},
    subscribe: () => ({ stop() {}, cursor: () => 0 }),
    listMessageAuditChanges: async () => ({
      events: [],
      checkpoint: "empty-complete",
    }),
    getMessageAudit: async (_, seq) => ({
      message: { taskId: "one", seq },
      original: { classification: "unclassified", workItems: [] },
    }),
  };
  const cache = () =>
    createHubReadCache({
      load: async () => disk,
      save: async (value) => {
        disk = structuredClone(value);
      },
    });
  const first = createCachedHubClient({
    client,
    cache: cache(),
    online: () => connected,
  });
  const records = await first.loadMessageAudits("one", [{ seq: 99 }]);
  assert.equal(records["99"].original.classification, "unclassified");
  first.dispose();
  connected = false;
  const restored = createCachedHubClient({
    client,
    cache: cache(),
    online: () => connected,
  });
  assert.equal(
    (await restored.loadMessageAudits("one", [{ seq: 99 }]))["99"].original
      .classification,
    "unclassified",
  );
  restored.dispose();
});

test("overlapping exact audit bootstraps return and persist only the newest projection", async () => {
  let disk;
  let mutation;
  let releaseOld;
  let oldStarted;
  const oldStartedPromise = new Promise((resolve) => (oldStarted = resolve));
  const oldResult = new Promise((resolve) => (releaseOld = resolve));
  let exactReads = 0;
  const record = (revision) => ({
    message: { taskId: "one", seq: 7 },
    original: { classification: "work", workItems: [] },
    current: { revision, classification: "work", workItems: [] },
  });
  const client = {
    base: "http://fixture",
    token: "scope",
    onMutation: (listener) => {
      mutation = listener;
      return () => {};
    },
    subscribe: () => ({ stop() {}, cursor: () => 0 }),
    listMessageAuditChanges: async () => ({
      events: [],
      checkpoint: crypto.randomUUID(),
    }),
    getMessageAudit: async () => {
      exactReads++;
      if (exactReads === 1) {
        oldStarted();
        return oldResult;
      }
      return record(2);
    },
  };
  const cache = () =>
    createHubReadCache({
      load: async () => disk,
      save: async (value) => {
        disk = structuredClone(value);
      },
    });
  const view = createCachedHubClient({ client, cache: cache() });
  const older = view.loadMessageAudits("one", [{ seq: 7 }]);
  await oldStartedPromise;
  mutation();
  const newer = await view.loadMessageAudits("one", [{ seq: 7 }]);
  assert.equal(newer["7"].current.revision, 2);
  releaseOld(record(1));
  assert.equal((await older)["7"].current.revision, 2);
  view.dispose();

  const offline = createCachedHubClient({
    client,
    cache: cache(),
    online: () => false,
  });
  assert.equal(
    (await offline.loadMessageAudits("one", [{ seq: 7 }]))["7"].current
      .revision,
    2,
  );
  offline.dispose();
});

test("disposing during an exact audit bootstrap rejects the late projection", async () => {
  let release;
  let started;
  const startedPromise = new Promise((resolve) => (started = resolve));
  const result = new Promise((resolve) => (release = resolve));
  const client = {
    base: "http://fixture",
    token: "scope",
    onMutation: () => () => {},
    subscribe: () => ({ stop() {}, cursor: () => 0 }),
    listMessageAuditChanges: async () => ({ events: [], checkpoint: "empty" }),
    getMessageAudit: async () => {
      started();
      return result;
    },
  };
  const view = createCachedHubClient({
    client,
    cache: createHubReadCache({
      load: async () => undefined,
      save: async () => {},
    }),
  });
  const pending = view.loadMessageAudits("one", [{ seq: 8 }]);
  await startedPromise;
  view.dispose();
  release({
    message: { taskId: "one", seq: 8 },
    original: { classification: "intake", workItems: [] },
    current: { revision: 1, classification: "intake", workItems: [] },
  });
  await assert.rejects(pending, /locked|disposed/i);
});
