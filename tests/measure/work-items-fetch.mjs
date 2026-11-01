// Measurement harness for bug wi_782e0ea7627f9fca (order #53690). Not an
// asserting suite and not in the matrix: it lives outside the browser
// inventory scan on purpose. It measures, on isolated synthetic state only:
//
//   P1  the real hub binary's GET /v1/work-items page cost (ms and bytes per
//       page) on a temporary state seeded to the live hub's counts;
//   P3  how the Bugs (and Features) tab reaches its first painted row in
//       Chromium and WebKit, cold and warm cache, at two round-trip times;
//   P4  what a background refresh fetches with the Bugs, Board, Agents and
//       Tasks tabs open alone while a live event arrives every second.
//
// No live hub, profile, tmux or network access: the browser pages talk to an
// in-page synthetic transport, and the hub runs on 127.0.0.1 with a temporary
// TAILTERM_STATE that is removed at the end. Only this run's own hub PID is
// stopped. Item text is generated lorem; only the counts mirror the live hub.
//
// Usage: node tests/measure/work-items-fetch.mjs
//   TEST_BROWSER=chromium|webkit|both (default both)
//   MEASURE_PHASES=hub,first,refresh (default all three)
//   MEASURE_SAMPLES=5 MEASURE_REFRESH_SECONDS=60 MEASURE_PAGE_MS=<ms>
//   MEASURE_OUT=<file.json> (raw results; the tables print to stdout)
import { chromium, webkit } from "@playwright/test";
import { createServer } from "vite";
import { spawn } from "node:child_process";
import { createServer as createNetServer } from "node:net";
import { once } from "node:events";
import { mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { viteCacheDir } from "../vite-cache.mjs";
import { prepareTestBinary } from "../test-binaries.mjs";

const RTTS = [0, 80];
const SAMPLES = Number(process.env.MEASURE_SAMPLES || 5);
const REFRESH_SECONDS = Number(process.env.MEASURE_REFRESH_SECONDS || 60);
const LIVE_EVENT_MS = 1000;
const REFRESH_MS = 20000;
// After the first row paints, totals keep counting for this long, so a warm
// load's after-paint background refresh is included.
const SETTLE_MS = 5000;
const PHASES = new Set(
  (process.env.MEASURE_PHASES || "hub,first,refresh").split(","),
);
const ENGINES = {
  chromium: [chromium],
  webkit: [webkit],
  both: [chromium, webkit],
}[process.env.TEST_BROWSER || "both"];
if (!ENGINES) throw new Error("TEST_BROWSER must be chromium, webkit or both");

// Live-sized counts (item text, 10-31 snapshot): 16 projects, 1,336 items =
// 1,089 bugs + 247 features, 35 open. The same formulas as
// tests/tab-switch-browser.mjs so both fixtures describe the same hub.
const ITEM_COUNT = 1336;
const kindOf = (i) => ((i * 247) % ITEM_COUNT < 247 ? "feature" : "bug");
const descriptionLength = (i) =>
  i % 20 === 0 ? 8039 : i % 2 ? 2905 + (i % 7) * 300 : 1500 + (i % 9) * 150;
const words =
  "synthetic fixture board message about queue lanes verification matrix reviewer builder planner handler release order criteria evidence".split(
    " ",
  );
const lorem = (n, seed) => {
  let s = "";
  for (let i = 0; s.length < n; i++)
    s += words[(i * 7 + seed) % words.length] + " ";
  return s.slice(0, n);
};
const pause = (ms) => new Promise((resolve) => setTimeout(resolve, ms));
const median = (values) => {
  const sorted = [...values].sort((a, b) => a - b);
  return sorted[Math.floor(sorted.length / 2)];
};
const max = (values) => Math.max(...values);

async function freePort() {
  const server = createNetServer();
  server.listen(0, "127.0.0.1");
  await once(server, "listening");
  const port = server.address().port;
  await new Promise((resolve) => server.close(resolve));
  return port;
}

// P1: the real hub binary on a temporary state.
async function measureHub() {
  const state = await mkdtemp(path.join(tmpdir(), "tailterm-wi-fetch-"));
  const env = Object.fromEntries(
    Object.entries(process.env).filter(
      ([key]) => !/^(TAILTERM_|CODEX_|TT_|TMUX)/.test(key) && key !== "HOME",
    ),
  );
  let hub;
  try {
    const binary = await prepareTestBinary({
      target: "hub",
      output: path.join(state, "tailterm-hub"),
      manifestPath: null,
    });
    const base = `http://127.0.0.1:${await freePort()}`;
    hub = spawn(binary, {
      env: {
        ...env,
        TAILTERM_STATE: path.join(state, "hub"),
        TAILTERM_DEV_LISTEN: new URL(base).host,
        TAILTERM_TCP_LISTEN: "",
      },
      stdio: "ignore",
    });
    const call = async (method, route, body) => {
      for (let attempt = 0; ; attempt++) {
        const response = await fetch(base + route, {
          method,
          signal: AbortSignal.timeout(30000),
          headers: body ? { "content-type": "application/json" } : undefined,
          body: body ? JSON.stringify(body) : undefined,
        });
        const text = await response.text();
        // The hub allows 20 writes a second; back off and retry the same
        // request ID on a 429.
        if (response.status === 429 && attempt < 20) {
          await pause(1000);
          continue;
        }
        if (!response.ok)
          throw new Error(`${method} ${route}: ${response.status} ${text}`);
        return { data: JSON.parse(text), bytes: Buffer.byteLength(text) };
      }
    };
    for (let i = 0; ; i++) {
      try {
        await call("GET", "/v1/tasks");
        break;
      } catch (error) {
        if (i > 100) throw error;
        await pause(100);
      }
    }
    const tasks = [];
    for (let i = 0; i < 16; i++) {
      await pause(55);
      tasks.push(
        (
          await call("POST", "/v1/tasks", {
            name: "Synthetic project " + i,
            orchestrator: "fixture-lead",
          })
        ).data,
      );
    }
    const seedStart = performance.now();
    for (let i = 0; i < ITEM_COUNT; i++) {
      await pause(52);
      await call("POST", `/v1/tasks/${tasks[i % 5 === 4 ? 1 : 0].id}/work-items`, {
        kind: kindOf(i),
        // The hub refuses a title with surrounding spaces.
        title: ("Synthetic work item " + i + " " + lorem(60, i)).trim(),
        description: lorem(descriptionLength(i), i),
        priority: ["normal", "high", "low"][i % 3],
        requestId: "seed-" + i,
      });
    }
    const seedSeconds = Math.round((performance.now() - seedStart) / 1000);
    const pages = {};
    for (const kind of ["bug", "feature"]) {
      const runs = [];
      for (let run = 0; run < SAMPLES; run++) {
        const list = [];
        let after = 0;
        for (;;) {
          const t0 = performance.now();
          const { data, bytes } = await call(
            "GET",
            `/v1/work-items?kind=${kind}&after=${after}&limit=200`,
          );
          list.push({
            ms: performance.now() - t0,
            bytes,
            items: data.items.length,
          });
          if (!data.items.length || !data.next || data.next <= after) break;
          after = data.next;
        }
        runs.push(list);
      }
      pages[kind] = runs;
    }
    return { seedSeconds, pages };
  } finally {
    if (hub && hub.exitCode === null) {
      hub.kill("SIGTERM");
      await Promise.race([once(hub, "exit"), pause(5000)]);
      if (hub.exitCode === null && hub.signalCode === null) hub.kill("SIGKILL");
    }
    await rm(state, { recursive: true, force: true });
  }
}

// Runs in the page, serialized with toString, so it may only use its
// arguments and dynamic imports. The fixture data matches
// tests/tab-switch-browser.mjs; /v1/work-items answers with hub paging (next
// is the last seq on every non-empty page, then one empty page).
async function fixturePage({
  latency,
  pageMs,
  backlog,
  refreshMs,
  itemCount,
  kindSource,
  descriptionSource,
}) {
  const vault = await import("/client/local-vault.js");
  const { createHubClient } = await import("/client/hub-client.js");
  const { createCachedHubClient } =
    await import("/client/cached-hub-client.js");
  const { createHubReadCache } = await import("/client/hub-read-cache.js");
  const { createBoardView } = await import("/client/board-view.js");
  const { createTasksView } = await import("/client/tasks-view.js");
  const { createAgentsView } = await import("/client/agents-view.js");
  const { createWorkItemsView } = await import("/client/work-items-view.js");
  const { setupModes } = await import("/client/modes.js");
  const kindOf = new Function("i", "ITEM_COUNT", "return " + kindSource);
  const descriptionLength = new Function("i", "return " + descriptionSource);

  const hex = (n, w = 16) => n.toString(16).padStart(w, "0");
  const words =
    "synthetic fixture board message about queue lanes verification matrix reviewer builder planner handler release order criteria evidence".split(
      " ",
    );
  const lorem = (n, seed) => {
    let s = "";
    for (let i = 0; s.length < n; i++)
      s += words[(i * 7 + seed) % words.length] + " ";
    return s.slice(0, n);
  };
  const tasks = Array.from({ length: 16 }, (_, i) => ({
    id: "tsk_" + hex(i + 1),
    name: i === 0 ? "Synthetic large project" : "Synthetic project " + i,
    goal: lorem(160, i),
    status: i < 2 ? "open" : "closed",
    pauseState: "active",
    allowAgentSpawn: true,
    orchestrator: "fixture-lead",
    cleanupPending: 0,
    createdAt: "2026-09-01T00:00:00Z",
    closedAt: i < 2 ? null : "2026-09-20T00:00:00Z",
    createdBy: { node: "workspace", user: "fixture" },
  }));
  const agent = (taskId, n, status) => {
    const role = ["lead", "planner", "builder", "verifier", "reviewer"][n % 5];
    return {
      id: "agt_" + hex(n * 31 + 7),
      taskId,
      name: `${role}-${hex(n, 8)}`,
      host: "fixture.invalid",
      session: `${role}-${hex(n, 8)}`,
      runtime: n % 2 ? "codex" : "claude",
      cwd: "/synthetic/worktrees/queue-" + hex(n, 8),
      parentAgentId: "",
      runId: "run_" + hex(n),
      role: "",
      status,
      activity: status === "running" ? "working" : "unknown",
      title: "",
      createdAt: "2026-10-01T00:00:00Z",
      lastEventAt: "2026-10-01T00:10:00Z",
      readUpTo: 100 + n,
      unread: 0,
      lastTransition: {
        tokens: 1000000 + n,
        observed: "2026-10-01T00:10:00Z",
        pending: "",
      },
    };
  };
  const agentsByTask = new Map(
    tasks.map((task, ti) => {
      const count = ti === 0 ? 3199 : task.status === "open" ? 40 : 30;
      const needsInput = ti === 0 ? 20 : 17;
      return [
        task.id,
        Array.from({ length: count }, (_, n) => {
          const left = count - n;
          let status = "closed";
          if (task.status === "open")
            status =
              left <= 6
                ? "running"
                : left <= needsInput
                  ? "needs_input"
                  : left <= needsInput + 6
                    ? "done"
                    : "closed";
          return agent(task.id, n + ti * 10000, status);
        }),
      ];
    }),
  );
  let messageSeq = 51199;
  const message = (task, roster, i, ti) => ({
    seq: 51000 + i,
    taskId: task.id,
    from: {
      agentId: roster[(i * 13) % roster.length].id,
      node: "workspace",
      user: "fixture",
    },
    to: i % 3 ? roster[(i * 7) % roster.length].id : undefined,
    text: `RESULT: synthetic ${i}\n` + lorem(600 + (i % 5) * 200, i + ti),
    createdAt: "2026-10-30T10:00:00Z",
  });
  const messagesByTask = new Map(
    tasks.map((task, ti) => {
      const roster = agentsByTask.get(task.id);
      return [
        task.id,
        Array.from({ length: 200 }, (_, i) => message(task, roster, i, ti)),
      ];
    }),
  );
  const items = Array.from({ length: itemCount }, (_, i) => ({
    id: "wi_" + hex(i + 1),
    taskId: tasks[i % 5 === 4 ? 1 : 0].id,
    kind: kindOf(i, itemCount),
    title: "Synthetic work item " + i + " " + lorem(60, i),
    description: lorem(descriptionLength(i), i),
    priority: ["normal", "high", "low"][i % 3],
    status: i % 39 === 0 ? "open" : i % 5 < 3 ? "done" : "dismissed",
    revision: 1 + (i % 4),
    seq: i + 1,
    itemSeq: i + 1,
    createdAt: "2026-10-01T00:00:00Z",
    updatedAt: "2026-10-02T00:00:00Z",
  }));

  const log = [];
  const stats = { unknown: new Set(), writes: 0 };
  let eventSeq = backlog;
  const waiters = new Set();
  let liveTimer = null;
  const changed = { workItem: 0, message: 0, agent: 0 };
  const changedBytes = { workItem: 0, message: 0, agent: 0 };
  // One live event: one work item changes, one message is posted and one
  // agent changes activity, so each view has one changed record to show.
  const liveEvent = () => {
    const n = eventSeq - backlog;
    const item = items[(n * 37) % items.length];
    item.revision++;
    item.updatedAt = new Date().toISOString();
    changed.workItem++;
    changedBytes.workItem += JSON.stringify(item).length;
    const task = tasks[0];
    const roster = agentsByTask.get(task.id);
    const posted = message(task, roster, ++messageSeq - 51000, 0);
    messagesByTask.get(task.id).push(posted);
    changed.message++;
    changedBytes.message += JSON.stringify(posted).length;
    const a = roster[roster.length - 1 - (n % 6)];
    a.activity = a.activity === "working" ? "idle" : "working";
    a.lastEventAt = new Date().toISOString();
    changed.agent++;
    changedBytes.agent += JSON.stringify(a).length;
    eventSeq++;
    for (const wake of waiters) wake();
    waiters.clear();
  };
  const eventAt = (seq) => ({
    seq,
    taskId: tasks[0].id,
    kind: "message",
    agentId: "",
    text: "synthetic event " + seq,
    by: { node: "workspace", user: "fixture" },
    createdAt: "2026-10-30T10:00:00Z",
  });
  const eventsAfter = (task, after, limit) => {
    const out = [];
    for (let seq = after + 1; seq <= eventSeq && out.length < limit; seq++) {
      const event = eventAt(seq);
      if (!task || event.taskId === task) out.push(event);
    }
    return out;
  };
  const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));
  const classify = (u) => {
    const p = u.pathname;
    if (p === "/v1/events" || /\/events$/.test(p)) return "events";
    if (p === "/v1/work-items")
      return "work-items" + (u.searchParams.get("kind") ? "" : " (no kind)");
    if (p === "/v1/tasks") return "tasks list";
    if (/^\/v1\/tasks\/[^/]+$/.test(p)) return "task detail (with agents)";
    if (/\/agents$/.test(p)) return "agents list";
    if (/\/messages$/.test(p))
      return u.searchParams.get("after") ? "messages after cursor" : "messages latest";
    return "other";
  };
  async function transport(url, init = {}) {
    const u = new URL(url);
    const p = u.pathname;
    const method = init.method || "GET";
    const entry = {
      route: classify(u),
      path: p + u.search,
      start: performance.now(),
      end: 0,
      bytes: 0,
    };
    log.push(entry);
    const respond = (body, status = 200) => {
      const text = JSON.stringify(body);
      entry.bytes = text.length;
      entry.end = performance.now();
      return new Response(text, {
        status,
        headers: { "Content-Type": "application/json" },
      });
    };
    if (entry.route === "events") {
      const signal = init.signal;
      const abortError = () =>
        new DOMException("The operation was aborted.", "AbortError");
      if (signal?.aborted) throw abortError();
      const poll = !!u.searchParams.get("wait");
      const hold = (ms, wake) =>
        new Promise((resolve, reject) => {
          const done = () => {
            clearTimeout(timer);
            waiters.delete(done);
            signal?.removeEventListener("abort", stop);
            resolve();
          };
          const stop = () => {
            clearTimeout(timer);
            waiters.delete(done);
            reject(abortError());
          };
          const timer = setTimeout(done, ms);
          if (wake) waiters.add(done);
          signal?.addEventListener("abort", stop, { once: true });
        });
      await hold(latency, false);
      const task = p.startsWith("/v1/tasks/") ? p.split("/")[3] : "";
      const after = Number(u.searchParams.get("after") || 0);
      const limit = Math.min(1000, Number(u.searchParams.get("limit") || 100));
      let events = eventsAfter(task, after, limit);
      if (!events.length && poll) {
        await hold(25000, true);
        events = eventsAfter(task, after, limit);
      }
      return respond({ events, next: events.length ? events.at(-1).seq : after });
    }
    if (p.endsWith("/changes")) {
      if (u.searchParams.get("wait")) await new Promise(() => {});
      return respond({
        events: [],
        changes: [],
        next: Number(u.searchParams.get("after") || 0),
      });
    }
    await sleep(latency + (p === "/v1/work-items" ? pageMs : 0));
    if (method !== "GET") {
      stats.writes++;
      return respond({ error: "Synthetic fixture is read-only" }, 503);
    }
    if (p === "/v1/capabilities")
      return respond({ messageAudit: { versions: [] }, queue: { versions: [1] } });
    if (p === "/v1/whoami") return respond({ node: "workspace", user: "fixture" });
    if (p === "/v1/tasks") return respond({ tasks });
    if (p === "/v1/work-items") {
      const q = (name) => u.searchParams.get(name);
      const after = Number(q("after") || 0);
      const limit = Math.min(200, Number(q("limit") || 200));
      const page = items
        .filter(
          (x) =>
            (!q("taskId") || x.taskId === q("taskId")) &&
            (!q("status") || x.status === q("status")) &&
            (!q("kind") || x.kind === q("kind")) &&
            x.seq > after,
        )
        .slice(0, limit);
      // Hub semantics (hub/internal/store/work_items.go ListWorkItems):
      // next is the last seq of every non-empty page.
      return respond({ items: page, next: page.length ? page.at(-1).seq : 0 });
    }
    const match = p.match(/^\/v1\/tasks\/([^/]+)(.*)$/);
    const task = match && tasks.find((t) => t.id === match[1]);
    if (!task) {
      stats.unknown.add(p);
      return respond({ error: "not found" }, 404);
    }
    const tail = match[2];
    if (!tail)
      return respond({
        task,
        agents: agentsByTask.get(task.id),
        latestSeq: messageSeq,
      });
    if (tail === "/agents") return respond({ agents: agentsByTask.get(task.id) });
    if (tail === "/messages") {
      const after = Number(u.searchParams.get("after") || 0);
      const limit = Math.min(1000, Number(u.searchParams.get("limit") || 50));
      const list = messagesByTask.get(task.id).filter((x) => x.seq > after);
      return respond({
        messages:
          u.searchParams.get("latest") === "1"
            ? list.slice(-limit)
            : list.slice(0, limit),
      });
    }
    if (tail === "/decisions") return respond({ decisions: [], nextAfter: 0 });
    if (tail === "/obligations") return respond({ obligations: [] });
    if (tail === "/delegation-windows") return respond({ windows: [] });
    if (tail === "/queue") return respond({ entries: [], cursor: "" });
    if (tail === "/team-queue") return respond({ entries: [] });
    stats.unknown.add(p);
    return respond({ error: "not found" }, 404);
  }

  await vault.localAPI("/unlock", "POST", {
    password: "Synthetic work items fetch QA passphrase",
  });
  const live = createHubClient({
    baseURL: "http://synthetic-items.invalid",
    token: "synthetic-only",
    fetchImpl: transport,
  });
  const persistence = vault.hubReadCachePersistence();
  const persisted = { saving: 0, saves: 0 };
  const cache = createHubReadCache({
    check: persistence.check,
    load: () => persistence.load(),
    save: async (value) => {
      persisted.saving++;
      try {
        await persistence.save(value);
        persisted.saves++;
      } finally {
        persisted.saving--;
      }
    },
  });
  const client = createCachedHubClient({ client: live, cache, refreshMs });
  // Every listWorkItems call the view makes, network-served or cache-served.
  const reads = [];
  const listWorkItems = client.listWorkItems;
  client.listWorkItems = async (params) => {
    const read = { start: performance.now(), end: 0, after: params.after, network: false };
    reads.push(read);
    const before = log.length;
    try {
      return await listWorkItems(params);
    } finally {
      read.end = performance.now();
      read.network = log
        .slice(before)
        .some(
          (entry) =>
            entry.route === "work-items" &&
            new URL(entry.path, "http://x.invalid").searchParams.get("after") ===
              String(params.after ?? 0),
        );
    }
  };
  const noop = () => {};
  const common = {
    client: () => client,
    notice: noop,
    configure: noop,
    getTabs: () => [],
    activate: noop,
  };
  const libraryHost = {
    currentServer: () => null,
    openSFTP: () => {
      throw new Error("Synthetic fixture has no SFTP.");
    },
    getData: () => vault.localData(),
    getServers: () => vault.localData().servers,
    api: vault.localAPI,
    reloadData: async () => {},
    dialog: noop,
    closeDialog: noop,
    notice: noop,
    confirm: async () => true,
    inspectTools: async () => ({}),
  };
  const itemsView = (kind) =>
    createWorkItemsView({
      ...common,
      kind,
      dialog: noop,
      closeDialog: noop,
      openBoard: noop,
      draftPersistence: vault.workItemDraftPersistence(),
    });
  let modes;
  const views = {
    bugs: itemsView("bug"),
    features: itemsView("feature"),
    board: createBoardView({
      ...common,
      addAgent: noop,
      revealAgent: noop,
      attachTask: noop,
      newTask: noop,
      settings: noop,
      intentPersistence: vault.boardIntentPersistence(),
    }),
    agents: createAgentsView({
      ...libraryHost,
      openAgents: () => modes?.set("agents"),
    }),
    tasks: createTasksView({
      ...common,
      taskHub: {
        groupOf: () => null,
        newTask: noop,
        addAgent: noop,
        setupHandler: noop,
        attachToCurrent: noop,
        settings: noop,
        revealAgent: noop,
        inspectTools: async () => ({}),
      },
      confirm: async () => true,
      openWorkItems: noop,
      openBoard: noop,
    }),
  };
  modes = setupModes({
    header: document.querySelector("main > header"),
    main: document.querySelector("main"),
    onChange: (mode, view) => {
      for (const v of Object.values(views)) v.hide();
      view.replaceChildren();
      if (views[mode]) {
        views[mode].mount(view);
        window.rendered = views[mode].show();
      } else window.rendered = Promise.resolve();
    },
  });
  const sum = (list) => list.reduce((a, b) => a + b, 0);
  const firstRow = (selector, t0) =>
    new Promise((resolve) => {
      const seen = () => {
        observer.disconnect();
        clearTimeout(timer);
        const at = performance.now();
        requestAnimationFrame(() =>
          setTimeout(() => resolve({ seen: at, painted: performance.now() }), 0),
        );
      };
      const observer = new MutationObserver(() => {
        if (document.querySelector(selector)) seen();
      });
      const timer = setTimeout(() => {
        observer.disconnect();
        resolve({ seen: -1, painted: -1 });
      }, 60000);
      if (document.querySelector(selector)) seen();
      else
        observer.observe(document.body, {
          childList: true,
          subtree: true,
          attributes: true,
        });
    });
  window.qa = {
    // P3: click the tab, wait for the first painted row, keep counting for
    // settleMs, then summarize every request and listWorkItems read.
    async firstRow(mode, selector, settleMs) {
      const logFrom = log.length,
        readsFrom = reads.length;
      const t0 = performance.now();
      document.querySelector(`header [data-mode="${mode}"]`).click();
      const row = await firstRow(selector, t0);
      await sleep(settleMs);
      const net = log
        .slice(logFrom)
        .filter((entry) => entry.route !== "events");
      const before = net.filter((entry) => entry.start <= row.seen);
      const pagesBefore = before.filter((entry) => entry.route === "work-items");
      const viewReads = reads.slice(readsFrom);
      const readsBefore = viewReads.filter((read) => read.end && read.end <= row.seen);
      // Fetch ends when the last read the first render depended on resolved.
      const fetchEnd = Math.max(t0, ...readsBefore.map((read) => read.end));
      // The tail: from the first page resolving to the last one resolving,
      // the time a first-page paint would save.
      const firstPage = readsBefore.find((read) => !read.after);
      return {
        firstRowMs: row.painted < 0 ? -1 : row.painted - t0,
        fetchMs: fetchEnd - t0,
        firstPageMs: firstPage ? firstPage.end - t0 : -1,
        tailMs: firstPage ? fetchEnd - firstPage.end : 0,
        renderMs: row.painted < 0 ? -1 : row.painted - fetchEnd,
        requestsBefore: before.length,
        bytesBefore: sum(before.map((entry) => entry.bytes)),
        pagesBefore: pagesBefore.length,
        pageBytesBefore: sum(pagesBefore.map((entry) => entry.bytes)),
        totalRequests: net.length,
        totalBytes: sum(net.map((entry) => entry.bytes)),
        readsBefore: readsBefore.length,
        cacheServedReadsBefore: readsBefore.filter((read) => !read.network).length,
        rows: document.querySelectorAll(selector).length,
      };
    },
    // Saved-cache coverage of every page path the given kind requested.
    async coverage(kind) {
      while (persisted.saving) await sleep(20);
      const saved = (await persistence.load())?.entries || [];
      const keys = new Set(saved.map((entry) => entry.key));
      const wanted = [
        ...new Set(
          log
            .filter((entry) => entry.route === "work-items")
            .map((entry) => entry.path)
            .filter(
              (p) =>
                new URL(p, "http://x.invalid").searchParams.get("kind") === kind,
            ),
        ),
      ];
      return { wanted: wanted.length, missing: wanted.filter((p) => !keys.has(p)).length };
    },
    async idle() {
      await sleep(500);
      while (persisted.saving) await sleep(20);
    },
    // P4: open the tab, wait for its data, then run live events for seconds
    // and count every request by route.
    async refresh(mode, selector, seconds, eventMs) {
      document.querySelector(`header [data-mode="${mode}"]`).click();
      if (selector) await firstRow(selector, performance.now());
      await sleep(3000);
      const logFrom = log.length;
      const start = { ...changed },
        startBytes = { ...changedBytes };
      const t0 = performance.now();
      // eventMs 0: no live events, only the cached client's refresh timer.
      if (eventMs) liveTimer = setInterval(liveEvent, eventMs);
      await sleep(seconds * 1000);
      clearInterval(liveTimer);
      const elapsed = (performance.now() - t0) / 60000;
      const routes = {};
      for (const entry of log.slice(logFrom)) {
        if (entry.route === "events") continue;
        const r = (routes[entry.route] ||= { requests: 0, bytes: 0 });
        r.requests++;
        r.bytes += entry.bytes;
      }
      for (const r of Object.values(routes)) {
        r.requestsPerMin = Math.round(r.requests / elapsed);
        r.bytesPerMin = Math.round(r.bytes / elapsed);
      }
      const delta = (key) => ({
        records: changed[key] - start[key],
        bytesPerMin: Math.round((changedBytes[key] - startBytes[key]) / elapsed),
      });
      return {
        minutes: elapsed,
        routes,
        changed: {
          workItem: delta("workItem"),
          message: delta("message"),
          agent: delta("agent"),
        },
        unknown: [...stats.unknown],
        writes: stats.writes,
      };
    },
  };
  window.ready = true;
}

const html = (options) =>
  `<!doctype html><html><head><meta charset="utf-8"><link rel="stylesheet" href="/client/style.css"><link rel="stylesheet" href="/client/work-items.css"></head>
<body><div id="app"><div id="workspace"><aside></aside><main><header></header><div class="terminal-shell"></div></main></div><dialog id="dialog"></dialog><div id="notice" hidden></div></div>
<script type="module">(${fixturePage.toString()})(${JSON.stringify(options)}).catch((error) => { window.fixtureError = String(error?.stack || error); });</script></body></html>`;

const ROW = "#mode-view .work-item";
const SELECTORS = {
  bugs: ROW,
  features: ROW,
  board: "#mode-view #board-messages .board-message",
  agents: "",
  tasks: "#mode-view [data-task-select]",
};

async function openPage(browser, origin, options) {
  const context = await browser.newContext({ viewport: { width: 1440, height: 900 } });
  const page = await context.newPage();
  const errors = [];
  page.on("pageerror", (error) => errors.push(error.message));
  const ready = async () => {
    await page.waitForFunction(() => window.ready || window.fixtureError, null, {
      timeout: 60000,
    });
    const fixtureError = await page.evaluate(() => window.fixtureError);
    if (fixtureError) throw new Error(fixtureError);
  };
  await page.goto(`${origin}/work-items-fetch?` + new URLSearchParams({ o: JSON.stringify(options) }));
  await ready();
  return { context, page, errors, ready };
}

async function measureFirstRow(browser, origin, engine, pageMs) {
  const out = [];
  for (const rtt of RTTS) {
    for (const mode of ["bugs", "features"]) {
      const samples = mode === "bugs" ? SAMPLES : 1;
      for (let s = 0; s < samples; s++) {
        const { context, page, errors, ready } = await openPage(browser, origin, {
          latency: rtt,
          pageMs,
          backlog: 0,
          refreshMs: REFRESH_MS,
        });
        try {
          const kind = mode === "bugs" ? "bug" : "feature";
          const cold = await page.evaluate(
            ([m, sel, ms]) => window.qa.firstRow(m, sel, ms),
            [mode, ROW, SETTLE_MS],
          );
          await page.evaluate(() => window.qa.idle());
          const coverage = await page.evaluate((k) => window.qa.coverage(k), kind);
          await page.reload();
          await ready();
          const warm = await page.evaluate(
            ([m, sel, ms]) => window.qa.firstRow(m, sel, ms),
            [mode, ROW, SETTLE_MS],
          );
          out.push({ engine, rtt, mode, sample: s, cold, warm, coverage, errors: [...errors] });
          console.error(
            `${engine} rtt=${rtt} ${mode} #${s}: cold ${Math.round(cold.firstRowMs)} ms, warm ${Math.round(warm.firstRowMs)} ms, coverage missing ${coverage.missing}/${coverage.wanted}`,
          );
        } finally {
          await context.close();
        }
      }
    }
  }
  return out;
}

async function measureRefresh(browser, origin, engine, pageMs) {
  const out = [];
  const runs = [
    ...["bugs", "board", "agents", "tasks"].map((mode) => [mode, LIVE_EVENT_MS]),
    // Quiet hub: what the refresh timer alone costs, Chromium only.
    ...(engine === "chromium" ? ["bugs", "board"].map((mode) => [mode, 0]) : []),
  ];
  for (const [mode, eventMs] of runs) {
    const { context, page, errors } = await openPage(browser, origin, {
      latency: 80,
      pageMs,
      backlog: 0,
      refreshMs: REFRESH_MS,
    });
    try {
      const result = await page.evaluate(
        ([m, sel, seconds, eventMs]) => window.qa.refresh(m, sel, seconds, eventMs),
        [mode, SELECTORS[mode], REFRESH_SECONDS, eventMs],
      );
      out.push({ engine, mode, eventMs, ...result, errors: [...errors] });
      console.error(`${engine} refresh ${mode} events=${eventMs}: ${JSON.stringify(result.routes)}`);
    } finally {
      await context.close();
    }
  }
  return out;
}

const r = (n) => Math.round(n);
const kb = (n) => (n / 1024).toFixed(0);
function printFirstRow(rows) {
  const lines = [
    "| Engine | RTT ms | Tab | Cache | Time to first row ms (med / max) | Fetch ms | Render ms | Fetch share | First page resolved ms | Tail fetch ms (share) | Requests before row | Pages before row | KiB before row | Cache-served reads before row | Total requests | Total KiB | Rows |",
    "|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|",
  ];
  const groups = new Map();
  for (const row of rows)
    for (const cache of ["cold", "warm"]) {
      const key = [row.engine, row.rtt, row.mode, cache].join("|");
      if (!groups.has(key)) groups.set(key, []);
      groups.get(key).push(row[cache]);
    }
  for (const [key, list] of groups) {
    const [engine, rtt, mode, cache] = key.split("|");
    const m = (f) => median(list.map(f));
    const first = list.map((x) => x.firstRowMs);
    lines.push(
      `| ${engine} | ${rtt} | ${mode} | ${cache} | ${r(median(first))} / ${r(max(first))} | ${r(m((x) => x.fetchMs))} | ${r(m((x) => x.renderMs))} | ${r((100 * m((x) => x.fetchMs)) / Math.max(1, median(first)))}% | ${r(m((x) => x.firstPageMs))} | ${r(m((x) => x.tailMs))} (${r((100 * m((x) => x.tailMs)) / Math.max(1, median(first)))}%) | ${m((x) => x.requestsBefore)} | ${m((x) => x.pagesBefore)} | ${kb(m((x) => x.bytesBefore))} | ${m((x) => x.cacheServedReadsBefore)} of ${m((x) => x.readsBefore)} | ${m((x) => x.totalRequests)} | ${kb(m((x) => x.totalBytes))} | ${m((x) => x.rows)} |`,
    );
  }
  return lines.join("\n");
}
function printRefresh(rows) {
  const lines = [
    "| Engine | Tab | Live events | Route | Requests/min | KiB/min |",
    "|---|---|---|---|---|---|",
  ];
  for (const row of rows) {
    const events = row.eventMs ? `1 per ${row.eventMs} ms` : "none";
    if (!Object.keys(row.routes).length)
      lines.push(`| ${row.engine} | ${row.mode} | ${events} | (no hub reads) | 0 | 0 |`);
    for (const [route, v] of Object.entries(row.routes))
      lines.push(
        `| ${row.engine} | ${row.mode} | ${events} | ${route} | ${v.requestsPerMin} | ${kb(v.bytesPerMin)} |`,
      );
  }
  return lines.join("\n");
}
function printHub(hub) {
  const lines = [
    "| Kind | Page | Items | KiB | ms (med / max of runs) |",
    "|---|---|---|---|---|",
  ];
  for (const [kind, runs] of Object.entries(hub.pages))
    runs[0].forEach((page, i) => {
      const ms = runs.map((run) => run[i].ms);
      lines.push(
        `| ${kind} | ${i + 1} | ${page.items} | ${kb(page.bytes)} | ${median(ms).toFixed(1)} / ${max(ms).toFixed(1)} |`,
      );
    });
  return lines.join("\n");
}

const results = { startedAt: new Date().toISOString(), samples: SAMPLES };
if (PHASES.has("hub")) {
  results.hub = await measureHub();
  if (process.env.MEASURE_OUT)
    await writeFile(process.env.MEASURE_OUT, JSON.stringify(results, null, 2));
  console.log("\n## P1 hub pages (seeded in " + results.hub.seedSeconds + " s)\n");
  console.log(printHub(results.hub));
}
// The browser transport delays each work-items page by the hub's median
// non-empty page time from P1 (or MEASURE_PAGE_MS) on top of the RTT.
let pageMs = Number(process.env.MEASURE_PAGE_MS || 0);
if (results.hub && !process.env.MEASURE_PAGE_MS) {
  const times = results.hub.pages.bug.flatMap((run) =>
    run.filter((page) => page.items).map((page) => page.ms),
  );
  pageMs = Math.round(median(times));
}
results.pageMs = pageMs;
if (PHASES.has("first") || PHASES.has("refresh")) {
  const server = await createServer({
    configFile: false,
    cacheDir: viteCacheDir(import.meta.url),
    root: process.cwd(),
    logLevel: "error",
    server: { host: "127.0.0.1", port: 0 },
    plugins: [
      {
        name: "work-items-fetch-fixture",
        configureServer(vite) {
          vite.middlewares.use("/work-items-fetch", (req, res) => {
            const query = new URL(req.url, "http://fixture.invalid").searchParams;
            const options = JSON.parse(query.get("o"));
            res.setHeader("Content-Type", "text/html");
            res.end(
              html({
                ...options,
                itemCount: ITEM_COUNT,
                kindSource: "((i * 247) % ITEM_COUNT < 247 ? 'feature' : 'bug')",
                descriptionSource:
                  "i % 20 === 0 ? 8039 : i % 2 ? 2905 + (i % 7) * 300 : 1500 + (i % 9) * 150",
              }),
            );
          });
        },
      },
    ],
  });
  await server.listen();
  const origin = `http://127.0.0.1:${server.httpServer.address().port}`;
  results.first = [];
  results.refresh = [];
  try {
    for (const engine of ENGINES) {
      const browser = await engine.launch();
      try {
        const name = engine.name();
        if (PHASES.has("first"))
          results.first.push(...(await measureFirstRow(browser, origin, name, pageMs)));
        if (PHASES.has("refresh"))
          results.refresh.push(...(await measureRefresh(browser, origin, name, pageMs)));
      } finally {
        await browser.close();
      }
      // Saved after each engine, so a stopped run keeps what it measured.
      if (process.env.MEASURE_OUT)
        await writeFile(process.env.MEASURE_OUT, JSON.stringify(results, null, 2));
    }
  } finally {
    await server.close();
  }
  console.log(`\n## P3 first row (work-items page delay ${pageMs} ms + RTT)\n`);
  if (results.first.length) console.log(printFirstRow(results.first));
  if (results.refresh.length) {
    console.log(`\n## P4 background refresh (${REFRESH_SECONDS} s, event every ${LIVE_EVENT_MS} ms, RTT 80)\n`);
    console.log(printRefresh(results.refresh));
    console.log("\nChanged records per minute (bytes):");
    for (const row of results.refresh)
      console.log(`${row.engine} ${row.mode} events=${row.eventMs}: ${JSON.stringify(row.changed)} errors=${row.errors.length} unknown=${row.unknown} writes=${row.writes}`);
  }
}
if (process.env.MEASURE_OUT)
  await writeFile(process.env.MEASURE_OUT, JSON.stringify(results, null, 2));
