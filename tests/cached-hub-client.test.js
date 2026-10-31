import test from "node:test";
import assert from "node:assert/strict";
import { createHubClient, HubError } from "../client/hub-client.js";
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
  const make = (client = live, options = {}) =>
    createCachedHubClient({
      client,
      cache: cache(),
      online: () => connected,
      ...options,
    });
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

const tick = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

// Views hear about changes on a coalesced zero-delay timer, and a recovery
// read refreshes in the background, so a fixed sleep can end before either
// lands under load. Poll for the expected state within a bound, then keep the
// 10 ms quiet window so the exact counts still catch an extra notification.
async function settle(ready = () => true, boundMs = 2000) {
  const deadline = Date.now() + boundMs;
  while (!ready() && Date.now() < deadline) await tick(1);
  await tick(10);
}

for (const dirty of [false, true]) {
  test(`warm owner requests stay stale through bounded retries${dirty ? " after invalidation" : ""}`, async () => {
    let clock = 0;
    const f = fixture();
    const view = f.make(f.live, { now: () => clock, refreshMs: 1000 });
    const original = [{ id: "saved-request" }];
    try {
      f.response({ obligations: original });
      assert.deepEqual(await view.listOwnerObligations("one"), original);
      if (dirty) view.invalidate();
      f.status(503);
      f.response({ error: "Synthetic transient failure" });
      await view.refreshConnection();
      assert.equal(f.requests.length, 2);

      for (clock of [0, 1, 999]) {
        assert.deepEqual(await view.listOwnerObligations("one"), original);
        assert.equal(view.cacheStatus().label, "Saved data · stale");
        assert.equal(
          f.requests.length,
          2,
          "retry interval suppresses requests",
        );
      }

      clock = 1000;
      let release;
      f.hold(new Promise((resolve) => (release = resolve)));
      assert.deepEqual(await view.listOwnerObligations("one"), original);
      const retry = view.refreshConnection();
      assert.equal(view.cacheStatus().label, "Saved data · stale");
      release({ error: "Repeated transient failure" });
      await retry;
      assert.equal(
        f.requests.length,
        3,
        "the concurrent retry is deduplicated",
      );
      clock = 1999;
      assert.deepEqual(await view.listOwnerObligations("one"), original);
      assert.equal(view.cacheStatus().label, "Saved data · stale");
      assert.equal(f.requests.length, 3);

      clock = 2000;
      f.status(200);
      f.hold(new Promise((resolve) => (release = resolve)));
      assert.deepEqual(await view.listOwnerObligations("one"), original);
      const recovery = view.refreshConnection();
      assert.equal(view.cacheStatus().label, "Saved data · stale");
      release({ obligations: [{ id: "recovered-request" }] });
      await recovery;
      assert.equal(f.requests.length, 4);
      assert.deepEqual(await view.listOwnerObligations("one"), [
        { id: "recovered-request" },
      ]);
      assert.equal(view.cacheStatus().label, "Saved data");
    } finally {
      view.dispose();
    }
  });
}

test("cold optional failures remain errors and do not label ordinary data stale", async () => {
  let clock = 0;
  const f = fixture();
  const view = f.make(f.live, { now: () => clock, refreshMs: 1000 });
  try {
    await view.listTasks();
    f.status(503);
    f.response({ error: "Cold optional failure" });
    await assert.rejects(view.listOwnerObligations("one"), /Cold optional/);
    clock = 999;
    await assert.rejects(view.listOwnerObligations("one"), /Cold optional/);
    assert.equal(f.requests.length, 2);
    assert.equal(view.cacheStatus().label, "Saved data");
    clock = 1000;
    await assert.rejects(view.listOwnerObligations("one"), /Cold optional/);
    assert.equal(f.requests.length, 3);
  } finally {
    view.dispose();
  }
});

for (const status of [401, 403, 404, 410]) {
  test(`warm optional ${status} rejects saved data rather than serving it stale`, async () => {
    const f = fixture();
    const view = f.make();
    try {
      f.response({ obligations: [{ id: "saved-request" }] });
      await view.listOwnerObligations("one");
      f.status(status);
      f.response({ error: `Synthetic ${status}` });
      await view.refreshConnection();
      await assert.rejects(view.listOwnerObligations("one"), {
        status,
      });
      assert.equal(f.requests.length, 2);
      if ([401, 403].includes(status))
        assert.equal(view.cacheStatus().label, "Hub sign-in required");
      else assert.doesNotMatch(view.cacheStatus().label, /stale|offline/);
    } finally {
      view.dispose();
    }
  });
}

test("ordinary success preserves optional stale status and unchanged recovery notifies", async () => {
  let clock = 0;
  const f = fixture();
  const view = f.make(
    {
      ...f.live,
      subscribe: () => ({ stop() {}, cursor: 0 }),
    },
    { now: () => clock, refreshMs: 1000 },
  );
  const label = () => view.cacheStatus().label;
  const original = { obligations: [{ id: "saved-request" }] };
  try {
    f.response(original);
    await view.listOwnerObligations("one");
    await settle(() => label() === "Saved data");
    let notifications = 0;
    view.subscribe("", () => notifications++);
    f.status(503);
    f.response({ error: "Transient optional failure" });
    await view.refreshConnection();
    await settle(() => notifications >= 1 && label() === "Saved data · stale");
    assert.equal(notifications, 1, "stale transition notifies once");
    f.status(200);
    f.response({ tasks: [{ id: "ordinary" }] });
    await view.listTasks();
    assert.equal(view.cacheStatus().label, "Saved data · stale");
    await settle(() => notifications >= 2);
    const beforeRecovery = notifications;
    clock = 1000;
    f.response(original);
    await view.listOwnerObligations("one");
    await settle(
      () => notifications >= beforeRecovery + 1 && label() === "Saved data",
    );
    assert.equal(view.cacheStatus().label, "Saved data");
    assert.equal(notifications, beforeRecovery + 1);
  } finally {
    view.dispose();
  }
});

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
  try {
    await view.listTasks();
    await settle(() => view.cacheStatus().label === "Saved data");
    let notifications = 0;
    view.subscribe("", () => notifications++);
    await view.refreshConnection();
    await settle();
    assert.equal(
      notifications,
      0,
      "unchanged save and refresh labels are quiet",
    );

    f.response({ tasks: [{ id: "one", name: "Updated project" }] });
    await view.refreshConnection();
    await settle(() => notifications >= 1);
    assert.equal(notifications, 1, "changed content still notifies views");

    f.offline();
    await view.refreshConnection();
    await settle(() => notifications >= 2);
    assert.equal(
      notifications,
      2,
      "visible offline status still notifies views",
    );
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

test("Usage saves presentation reads offline and optional old-hub errors leave Projects usable", async () => {
  const f = fixture(),
    view = f.make();
  try {
    f.response({
      version: 1,
      projectId: "synthetic",
      items: [],
      priceRevision: 0,
    });
    assert.equal((await view.getUsage("synthetic")).version, 1);
    f.offline();
    assert.equal((await view.getUsage("synthetic")).projectId, "synthetic");
    assert.match(view.cacheStatus().label, /offline/);
  } finally {
    view.dispose();
  }
  const g = fixture(),
    old = g.make();
  try {
    g.status(404);
    await assert.rejects(old.getUsage("missing"));
    g.status(200);
    g.response({ tasks: [{ id: "synthetic", name: "Project usable" }] });
    assert.equal((await old.listTasks())[0].name, "Project usable");
    assert.doesNotMatch(old.cacheStatus().label, /offline/);
  } finally {
    old.dispose();
  }
});

// A hub client whose feed is driven by the test: deliver(task, events) hands
// a page to every underlying subscription of that task.
function feedClient(live) {
  const feeds = [];
  return {
    feeds,
    client: {
      ...live,
      subscribe(task, onEvents, options = {}) {
        const feed = { task, onEvents, options, stopped: false, cursor: 0 };
        feeds.push(feed);
        return {
          stop() {
            feed.stopped = true;
          },
          cursor: () => feed.cursor,
        };
      },
    },
    async deliver(task, count, kind = "message") {
      for (const feed of feeds.filter((f) => f.task === task && !f.stopped)) {
        const events = Array.from({ length: count }, () => ({
          seq: ++feed.cursor,
          kind,
        }));
        await feed.onEvents(events, feed.cursor);
      }
    },
  };
}

const within = (promise, ms, label) =>
  Promise.race([
    promise,
    new Promise((_, reject) =>
      setTimeout(
        () => reject(Error(`${label} waited longer than ${ms} ms`)),
        ms,
      ),
    ),
  ]);

test("a feed event serves saved data at once and refreshes it in the background", async () => {
  const f = fixture(),
    feed = feedClient(f.live),
    view = f.make(feed.client);
  try {
    assert.equal((await view.listTasks())[0].name, "Saved project");
    await settle();
    let reloads = 0;
    view.subscribe("", () => reloads++);
    await feed.deliver("", 1);
    let finish;
    f.hold(
      new Promise((resolve) => {
        finish = resolve;
      }),
    );
    const before = f.requests.length;
    const value = await within(view.listTasks(), 250, "stale read");
    assert.equal(value[0].name, "Saved project");
    await settle(() => f.requests.length > before);
    assert.equal(f.requests.length, before + 1, "one background refresh");
    await view.listTasks();
    assert.equal(f.requests.length, before + 1, "the refresh is shared");
    finish({ tasks: [{ id: "one", name: "Event update" }] });
    await settle(() => reloads >= 2);
    assert.equal((await view.listTasks())[0].name, "Event update");
    assert.equal(f.requests.length, before + 1, "fresh data is not refetched");
  } finally {
    view.dispose();
  }
});

test("an event during a refresh keeps the path stale and refreshes it again", async () => {
  const f = fixture(),
    feed = feedClient(f.live),
    view = f.make(feed.client);
  try {
    await view.listTasks();
    view.subscribe("", () => {});
    await feed.deliver("", 1);
    let finish;
    f.hold(
      new Promise((resolve) => {
        finish = resolve;
      }),
    );
    const before = f.requests.length;
    await view.listTasks();
    await settle(() => f.requests.length > before);
    await feed.deliver("", 1);
    f.hold(null);
    f.response({ tasks: [{ id: "one", name: "After the second event" }] });
    finish({ tasks: [{ id: "one", name: "Before the second event" }] });
    await settle(() => f.requests.length > before + 1);
    assert.equal(f.requests.length, before + 2, "one follow-up refresh");
    assert.equal((await view.listTasks())[0].name, "After the second event");
  } finally {
    view.dispose();
  }
});

test("heartbeats alone leave saved reads fresh", async () => {
  const f = fixture(),
    feed = feedClient(f.live),
    view = f.make(feed.client);
  try {
    await view.listTasks();
    view.subscribe("", () => {});
    await feed.deliver("", 1, "heartbeat");
    const before = f.requests.length;
    await view.listTasks();
    await settle();
    assert.equal(f.requests.length, before);
  } finally {
    view.dispose();
  }
});

test("a read returns before its encrypted save finishes", async () => {
  const f = fixture();
  let disk = null,
    saving = 0,
    release;
  const held = new Promise((resolve) => {
    release = resolve;
  });
  const view = f.make(f.live, {
    cache: createHubReadCache({
      load: async () => disk,
      save: async (value) => {
        saving++;
        const copy = structuredClone(value);
        await held;
        disk = copy;
        saving--;
      },
    }),
  });
  try {
    const value = await within(view.listTasks(), 250, "network read");
    assert.equal(value[0].name, "Saved project");
    assert.equal(saving, 1, "the save is still running");
    assert.equal(disk, null);
    assert.equal((await view.listTasks())[0].name, "Saved project");
    release();
    await settle(() => saving === 0);
    assert.equal(disk.entries.length, 1);
    await settle(() => view.cacheStatus().label === "Saved data");
  } finally {
    release();
    view.dispose();
  }
});

test("a feed catch-up marks saved reads stale once, after its last full page", async () => {
  const f = fixture(),
    feed = feedClient(f.live),
    view = f.make(feed.client);
  try {
    await view.listTasks();
    await settle();
    let reloads = 0;
    view.subscribe("", () => reloads++);
    const before = f.requests.length;
    for (let page = 0; page < 3; page++) {
      await feed.deliver("", 100);
      await view.listTasks();
    }
    await settle();
    assert.equal(reloads, 0, "full catch-up pages do not reload views");
    assert.equal(f.requests.length, before, "catch-up pages do not refetch");
    await feed.deliver("", 4);
    await view.listTasks();
    await settle();
    assert.equal(reloads, 1, "the end of the catch-up reloads views once");
    assert.equal(f.requests.length, before + 1, "and refreshes once");
    await feed.deliver("", 1);
    await view.listTasks();
    await settle();
    assert.equal(reloads, 2, "later live events still reach views");
    assert.equal(f.requests.length, before + 2, "and still mark reads stale");
  } finally {
    view.dispose();
  }
});

test("views share one feed per project that keeps its cursor", async () => {
  const f = fixture(),
    feed = feedClient(f.live),
    view = f.make(feed.client);
  try {
    const seen = { first: 0, second: 0 };
    const first = view.subscribe("", () => seen.first++, { after: 0 });
    await feed.deliver("", 3);
    first.stop();
    assert.equal(feed.feeds[0].stopped, false, "the feed outlives its view");
    const second = view.subscribe("", () => seen.second++, { after: 0 });
    assert.equal(feed.feeds.length, 1, "a later view does not replay");
    assert.equal(second.cursor(), 3);
    const other = view.subscribe("tsk_other", () => {});
    assert.equal(feed.feeds.length, 2, "each project has its own feed");
    await feed.deliver("", 1);
    await settle();
    assert.deepEqual(seen, { first: 1, second: 1 });
    other.stop();
    second.stop();
    view.dispose();
    assert.ok(
      feed.feeds.every((f) => f.stopped),
      "disposing stops every feed",
    );
  } finally {
    view.dispose();
  }
});

test("feed errors reach every attached view", async () => {
  const f = fixture(),
    feed = feedClient(f.live),
    view = f.make(feed.client);
  try {
    const errors = [];
    view.subscribe("", () => {}, { onError: (e) => errors.push(1 + e.n) });
    view.subscribe("", () => {}, { onError: (e) => errors.push(2 + e.n) });
    feed.feeds[0].options.onError({ n: 10 });
    assert.deepEqual(errors, [11, 12]);
  } finally {
    view.dispose();
  }
});

test("capabilities are kept in memory for this client only", async () => {
  const f = fixture();
  let calls = 0,
    answer = () => ({ queue: { versions: [1] } });
  const client = {
    ...f.live,
    capabilities: async () => {
      calls++;
      return answer();
    },
  };
  let clock = 0;
  const view = f.make(client, { now: () => clock, refreshMs: 1000 });
  try {
    assert.deepEqual(await view.capabilities(), { queue: { versions: [1] } });
    let finish;
    answer = () =>
      new Promise((resolve) => {
        finish = resolve;
      });
    clock = 1000;
    const kept = await within(view.capabilities(), 250, "capabilities");
    assert.deepEqual(kept, { queue: { versions: [1] } });
    kept.queue.versions.push(99);
    assert.equal(calls, 2, "a background refresh started");
    assert.deepEqual(await view.capabilities(), { queue: { versions: [1] } });
    assert.equal(calls, 2, "the refresh is shared and recent");
    finish({ queue: { versions: [1, 2] } });
    await settle();
    assert.deepEqual(await view.capabilities(), {
      queue: { versions: [1, 2] },
    });

    answer = () => {
      throw new HubError(401, "Expired credential");
    };
    clock = 2000;
    await view.capabilities();
    await settle();
    await assert.rejects(view.capabilities(), /Expired credential/);
    answer = () => ({ queue: { versions: [3] } });
    assert.deepEqual(await view.capabilities(), { queue: { versions: [3] } });
  } finally {
    view.dispose();
  }
  const fresh = f.make(client);
  try {
    const before = calls;
    await fresh.capabilities();
    assert.equal(calls, before + 1, "a new client never reuses the memo");
  } finally {
    fresh.dispose();
  }
});

test("capabilities are not served from memory offline", async () => {
  const f = fixture(),
    view = f.make({
      ...f.live,
      capabilities: async () => {
        if (offline) throw new Error("Synthetic offline");
        return { queue: { versions: [1] } };
      },
    });
  let offline = false;
  try {
    await view.capabilities();
    offline = true;
    f.offline();
    await assert.rejects(view.capabilities(), /Synthetic offline/);
  } finally {
    view.dispose();
  }
});

test("an invalidation drops the capabilities memo and any read in flight", async () => {
  const f = fixture();
  let answer = () => ({ queue: { versions: [1] } });
  let clock = 0;
  const view = f.make(
    { ...f.live, capabilities: async () => answer() },
    { now: () => clock, refreshMs: 60000 },
  );
  try {
    assert.deepEqual(await view.capabilities(), { queue: { versions: [1] } });
    answer = () => ({ queue: { versions: [2] } });
    view.invalidate();
    assert.deepEqual(await view.capabilities(), { queue: { versions: [2] } });

    let finish;
    answer = () =>
      new Promise((resolve) => {
        finish = resolve;
      });
    clock = 60000;
    await view.capabilities();
    view.invalidate();
    answer = () => ({ queue: { versions: [3] } });
    assert.deepEqual(await view.capabilities(), { queue: { versions: [3] } });
    finish({ queue: { versions: [2] } });
    await settle();
    assert.deepEqual(
      await view.capabilities(),
      { queue: { versions: [3] } },
      "a read from before the invalidation never becomes the memo",
    );
  } finally {
    view.dispose();
  }
});

test("a change to served data before any view subscribes reaches the first subscriber", async () => {
  const f = fixture(),
    feed = feedClient(f.live);
  const first = f.make(feed.client);
  await first.listTasks();
  await settle();
  first.dispose();
  const view = f.make(feed.client);
  try {
    f.response({ tasks: [{ id: "one", name: "Changed while unheard" }] });
    const before = f.requests.length;
    assert.equal((await view.listTasks())[0].name, "Saved project");
    await settle(() => f.requests.length > before);
    let reloads = 0;
    const one = view.subscribe("", () => reloads++);
    await settle(() => reloads > 0);
    assert.equal(reloads, 1, "the subscriber reloads once");
    assert.equal((await view.listTasks())[0].name, "Changed while unheard");
    let later = 0;
    const two = view.subscribe("", () => later++);
    await settle();
    assert.equal(later, 0, "the missed change is delivered once");

    // Data read while a view listened, changing after every view left, does
    // not reload the next view.
    one.stop();
    two.stop();
    f.response({ tasks: [{ id: "one", name: "Changed after views left" }] });
    await view.refreshConnection();
    await settle();
    let next = 0;
    view.subscribe("", () => next++);
    await settle();
    assert.equal(next, 0, "an unrelated later view is not reloaded");
  } finally {
    view.dispose();
  }
});

test("an unchanged refresh skips the encrypted save until the entry ages", async () => {
  const f = fixture();
  let clock = 1000,
    disk = null,
    saves = 0;
  const view = f.make(f.live, {
    now: () => clock,
    cache: createHubReadCache({
      load: async () => disk,
      save: async (value) => {
        saves++;
        disk = structuredClone(value);
      },
      now: () => clock,
    }),
  });
  try {
    await view.listTasks();
    await settle(() => saves === 1);
    await view.refreshConnection();
    await settle();
    assert.equal(saves, 1, "unchanged data is not saved again");
    f.response({ tasks: [{ id: "one", name: "Changed project" }] });
    await view.refreshConnection();
    await settle(() => saves === 2);
    assert.equal(saves, 2, "changed data is saved");
    clock += 10 * 60 * 1000;
    await view.listTasks();
    await settle(() => saves === 3);
    assert.equal(saves, 3, "an aged entry is saved again");
    assert.equal(disk.entries[0].savedAt, clock);
  } finally {
    view.dispose();
  }
});

test("a full live page after the catch-up still marks reads stale and reaches views", async () => {
  const f = fixture(),
    feed = feedClient(f.live),
    view = f.make(feed.client);
  try {
    await view.listTasks();
    await settle();
    let reloads = 0;
    view.subscribe("", () => reloads++);
    await feed.deliver("", 4);
    await view.listTasks();
    await settle();
    assert.equal(reloads, 1);
    const before = f.requests.length;
    await feed.deliver("", 100);
    await view.listTasks();
    await settle();
    assert.equal(reloads, 2, "the full live page reaches views");
    assert.equal(f.requests.length, before + 1, "and marks reads stale");
  } finally {
    view.dispose();
  }
});

test("a backlog that ends on a full page marks reads stale once the feed goes quiet", async () => {
  const f = fixture(),
    feed = feedClient(f.live),
    view = f.make(feed.client);
  try {
    await view.listTasks();
    await settle();
    let reloads = 0;
    view.subscribe("", () => reloads++);
    const before = f.requests.length;
    await feed.deliver("", 100);
    await feed.deliver("", 100);
    await settle(() => reloads >= 1, 1000);
    assert.equal(reloads, 1, "the coalesced backlog reaches views once");
    await view.listTasks();
    await settle();
    assert.equal(f.requests.length, before + 1, "and marks reads stale once");
  } finally {
    view.dispose();
  }
});
