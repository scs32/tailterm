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

// A fake hub for startup discovery: one open project whose agent runs on
// agentHost. listing(n) decides the nth /v1/tasks response; hold(n), when
// given, delays it until the returned promise settles.
function discoveryHub({ agentHost, listing = () => true, hold, onFetch } = {}) {
  const taskId = "tsk_0123456789abcdef";
  const task = { id: taskId, name: "Discovered", status: "open" };
  const calls = { listings: 0, tokens: [] };
  const fetch = async (url, init) => {
    onFetch?.(url, init);
    if (url.includes("/events")) return new Promise(() => {});
    if (url.endsWith("/v1/tasks")) {
      calls.listings++;
      calls.tokens.push(init?.headers?.Authorization || "");
      const n = calls.listings;
      await hold?.(n);
      if (!listing(n))
        return { status: 503, text: async () => '{"error":"unavailable"}' };
      return {
        status: 200,
        text: async () => JSON.stringify({ tasks: [task] }),
      };
    }
    return {
      status: 200,
      text: async () =>
        JSON.stringify({
          task,
          agents: [
            {
              id: "agt_0123456789abcdef",
              name: "orchestrator",
              host: agentHost,
              session: "orchestrator",
              status: "running",
            },
          ],
          latestSeq: 0,
        }),
    };
  };
  return { taskId, calls, fetch };
}
const discoveryHost = (fake, overrides = {}) => ({
  getData: () => ({ hub: { url: "http://hub" } }),
  getIPN: () => ({ fetch: fake.fetch }),
  getServers: () => [],
  getTabs: () => [],
  paneGroups: () => ({ model: { groups: [] }, sync() {} }),
  render() {},
  scheduleWorkspaceSave() {},
  bookmark() {},
  ...overrides,
});
const flush = async (n = 20) => {
  for (let i = 0; i < n; i++) await new Promise((r) => setImmediate(r));
};

test("a failed startup listing retries with backoff and binds without a refresh", async (t) => {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  const fake = discoveryHub({ agentHost: "Mini", listing: (n) => n >= 3 });
  const hub = createTaskHub(
    discoveryHost(fake, { getServers: () => [{ id: "mini", host: "Mini" }] }),
  );
  hub.refresh();
  void hub.discover();
  await flush();
  assert.equal(fake.calls.listings, 1);
  assert.deepEqual(hub.bound(), []);
  t.mock.timers.tick(4999);
  await flush();
  assert.equal(fake.calls.listings, 1);
  t.mock.timers.tick(1);
  await flush();
  assert.equal(fake.calls.listings, 2);
  t.mock.timers.tick(9999);
  await flush();
  assert.equal(fake.calls.listings, 2);
  t.mock.timers.tick(1);
  await flush();
  assert.equal(fake.calls.listings, 3);
  assert.deepEqual(hub.bound(), [fake.taskId]);
  t.mock.timers.tick(120000);
  await flush();
  assert.equal(fake.calls.listings, 3);
  hub.stopAll();

  // Clearing the hub stops the retry.
  const data = { hub: { url: "http://hub" } };
  const failing = discoveryHub({ agentHost: "Mini", listing: () => false });
  const cleared = createTaskHub(
    discoveryHost(failing, { getData: () => data }),
  );
  cleared.refresh();
  void cleared.discover();
  await flush();
  data.hub.url = "";
  cleared.refresh();
  t.mock.timers.tick(120000);
  await flush();
  assert.equal(failing.calls.listings, 1);
});

test("a client change during startup discovery binds on the next run", async () => {
  const data = { hub: { url: "http://hub", token: "first" } };
  const probes = [];
  let hub;
  const fake = discoveryHub({ agentHost: "Stephens-Mini" });
  hub = createTaskHub(
    discoveryHost(fake, {
      getData: () => data,
      getServers: () => [
        {
          id: "mini",
          host: "stephens-mac-mini.tail123.ts.net",
          fingerprint: "SHA256:fake",
          keyId: "key1",
        },
      ],
      browserCommand: async (server, command) => {
        probes.push([server.id, command]);
        data.hub.token = "second";
        hub.refresh();
        return "Stephens-Mini\n";
      },
    }),
  );
  hub.refresh();
  void hub.discover();
  await flush(40);
  hub.stopAll();
  assert.deepEqual(probes, [["mini", "hostname -s"]]);
  assert.deepEqual(fake.calls.tokens, ["Bearer first", "Bearer second"]);
  assert.deepEqual(hub.bound(), [fake.taskId]);
});

test("startup discovery probes hostnames only on servers that sign in without asking", async () => {
  const probes = [];
  const fake = discoveryHub({ agentHost: "Stephens-Mini" });
  const hub = createTaskHub(
    discoveryHost(fake, {
      getServers: () => [
        { id: "key", host: "a.ts.net", fingerprint: "SHA256:a", keyId: "k" },
        { id: "nocreds", host: "b.ts.net", fingerprint: "SHA256:b" },
        { id: "unverified", host: "c.ts.net", keyId: "k" },
        {
          id: "password",
          host: "d.ts.net",
          fingerprint: "SHA256:d",
          hasPassword: true,
        },
      ],
      browserCommand: async (server, command) => {
        probes.push([server.id, command]);
        return server.id === "password" ? "Stephens-Mini\n" : "other\n";
      },
    }),
  );
  hub.refresh();
  void hub.discover();
  await flush(40);
  hub.stopAll();
  assert.deepEqual(probes, [
    ["key", "hostname -s"],
    ["password", "hostname -s"],
  ]);
  assert.deepEqual(hub.bound(), [fake.taskId]);
});

test("a stop during a failing startup listing cancels its retry, a client change reruns", async (t) => {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  for (const stop of ["dispose", "stopAll"]) {
    let release;
    const fake = discoveryHub({
      agentHost: "Mini",
      listing: () => false,
      hold: () => new Promise((r) => (release = r)),
    });
    const hub = createTaskHub(discoveryHost(fake));
    hub.refresh();
    void hub.discover();
    await flush();
    assert.equal(fake.calls.listings, 1);
    hub[stop]();
    release();
    await flush();
    t.mock.timers.tick(120000);
    await flush();
    assert.equal(fake.calls.listings, 1, stop);
  }

  const data = { hub: { url: "http://hub", token: "first" } };
  let release;
  const fake = discoveryHub({
    agentHost: "Mini",
    listing: (n) => n > 1,
    hold: (n) => n === 1 && new Promise((r) => (release = r)),
  });
  const hub = createTaskHub(
    discoveryHost(fake, {
      getData: () => data,
      getServers: () => [{ id: "mini", host: "Mini" }],
    }),
  );
  hub.refresh();
  void hub.discover();
  await flush();
  data.hub.token = "second";
  hub.refresh();
  release();
  await flush(40);
  hub.stopAll();
  assert.deepEqual(fake.calls.tokens, ["Bearer first", "Bearer second"]);
  assert.deepEqual(hub.bound(), [fake.taskId]);
});

test("startup discovery probes a tailnet server that signs in with Tailscale SSH", async () => {
  const probes = [];
  const fake = discoveryHub({ agentHost: "Stephens-Mini" });
  const hub = createTaskHub(
    discoveryHost(fake, {
      getServers: () => [
        {
          id: "mini",
          host: "stephens-mac-mini.tail123.ts.net",
          port: 22,
          mode: "wasm",
        },
        { id: "sshmode", host: "x.tail123.ts.net", port: 22, mode: "ssh" },
        { id: "altport", host: "y.tail123.ts.net", port: 2222 },
        { id: "lan", host: "192.168.1.5", port: 22 },
      ],
      browserCommand: async (server, command) => {
        probes.push([server.id, command]);
        return "Stephens-Mini\n";
      },
    }),
  );
  hub.refresh();
  void hub.discover();
  await flush(40);
  hub.stopAll();
  assert.deepEqual(probes, [["mini", "hostname -s"]]);
  assert.deepEqual(hub.bound(), [fake.taskId]);
});
