// verify-matrix: browser-helper
// Top-level tab switch timing on a synthetic hub of realistic size. Only
// disposable synthetic browser, vault and in-page hub transport state; no live
// hub, profile, network or tmux access. Run directly for Chromium and WebKit;
// tests/static-browser.mjs runs it once per TEST_BROWSER engine.
import { chromium, webkit } from "@playwright/test";
import { createServer } from "vite";
import assert from "node:assert/strict";
import { pathToFileURL } from "node:url";
import { viteCacheDir } from "./vite-cache.mjs";

const TARGET_MS = 200;
const LATENCY_MS = 80;
const ROUNDS = 5;
const TABS = [
  "board",
  "tasks",
  "agents",
  "teams",
  "bugs",
  "features",
  "queue",
  "terminals",
];
const SELECTORS = {
  board: "#mode-view #board-messages .board-message",
  tasks: "#mode-view [data-task-select]",
  agents: "#mode-view [data-agent-select]",
  teams: "#mode-view [data-team-select]",
  bugs: "#mode-view .work-item",
  features: "#mode-view .work-item",
  queue: "#mode-view .queue-row",
  terminals: ".terminal-shell:not([hidden])",
};

// Runs in the page, serialized with toString, so it may only use its
// arguments and dynamic imports.
async function fixturePage({ latency, refreshMs }) {
  const vault = await import("/client/local-vault.js");
  const { createHubClient } = await import("/client/hub-client.js");
  const { createCachedHubClient } =
    await import("/client/cached-hub-client.js");
  const { createHubReadCache } = await import("/client/hub-read-cache.js");
  const { createBoardView } = await import("/client/board-view.js");
  const { createTasksView } = await import("/client/tasks-view.js");
  const { createTeamsView } = await import("/client/teams-view.js");
  const { createAgentsView } = await import("/client/agents-view.js");
  const { createWorkItemsView } = await import("/client/work-items-view.js");
  const { createQueueView } = await import("/client/queue-view.js");
  const { setupModes } = await import("/client/modes.js");

  // Fixture: 30 projects (6 open); the first has 3,182 agents (3,155 closed);
  // 200 long messages per project, 1,400 long work items, 30 queue entries.
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
  const tasks = Array.from({ length: 30 }, (_, i) => ({
    id: "tsk_" + hex(i + 1),
    name: i === 0 ? "Synthetic large project" : "Synthetic project " + i,
    goal: lorem(160, i),
    status: i < 6 ? "open" : "closed",
    pauseState: "active",
    allowAgentSpawn: true,
    orchestrator: "fixture-lead",
    cleanupPending: 0,
    createdAt: "2026-09-01T00:00:00Z",
    closedAt: i < 6 ? null : "2026-09-20T00:00:00Z",
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
      const count = ti === 0 ? 3182 : task.status === "open" ? 40 : 30;
      return [
        task.id,
        Array.from({ length: count }, (_, n) => {
          const left = count - n;
          let status = "closed";
          if (task.status === "open")
            status =
              left <= 6
                ? "running"
                : left <= 17
                  ? "needs_input"
                  : left <= 27
                    ? "done"
                    : "closed";
          return agent(task.id, n + ti * 10000, status);
        }),
      ];
    }),
  );
  const messagesByTask = new Map(
    tasks.map((task, ti) => {
      const roster = agentsByTask.get(task.id);
      return [
        task.id,
        Array.from({ length: 200 }, (_, i) => ({
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
        })),
      ];
    }),
  );
  const items = Array.from({ length: 1400 }, (_, i) => ({
    id: "wi_" + hex(i + 1),
    taskId: tasks[i % 5 === 4 ? 1 : 0].id,
    kind: i % 3 === 2 ? "feature" : "bug",
    title: "Synthetic work item " + i + " " + lorem(60, i),
    description: lorem(1500, i),
    priority: ["normal", "high", "low"][i % 3],
    status: ["open", "done", "in_progress", "blocked", "closed"][i % 5],
    revision: 1 + (i % 4),
    itemSeq: i + 1,
    createdAt: "2026-10-01T00:00:00Z",
    updatedAt: "2026-10-02T00:00:00Z",
  }));
  const queueEntries = Array.from({ length: 30 }, (_, i) => ({
    id: "tqe_" + hex(i + 1),
    taskId: tasks[0].id,
    itemTaskId: tasks[0].id,
    itemId: items[i].id,
    itemRevision: 1,
    sourceTaskId: tasks[0].id,
    item: {
      title: items[i].title,
      kind: items[i].kind,
      description: items[i].description,
      status: items[i].status,
    },
    offeredItemRevision: 1,
    currentItemRevision: 1,
    cycle: 1,
    revision: 1,
    eligible: true,
    queuePriority: "normal",
    state: ["waiting", "running", "completed"][i % 3],
    template: "planned",
    createdAt: "2026-10-30T00:00:00Z",
    updatedAt: "2026-10-30T00:00:00Z",
  }));

  const stats = { requested: new Set(), unknown: new Set(), writes: 0 };
  const response = (body, status = 200) =>
    new Response(JSON.stringify(body), {
      status,
      headers: { "Content-Type": "application/json" },
    });
  const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));
  // Feed reads that wait are held for the life of the page; every other read
  // pays the synthetic latency. Writes are rejected.
  async function transport(url, init = {}) {
    const u = new URL(url);
    const p = u.pathname;
    const method = init.method || "GET";
    if (p.endsWith("/events") || p.endsWith("/changes")) {
      if (u.searchParams.get("wait")) await new Promise(() => {});
      return response({
        events: [],
        changes: [],
        next: Number(u.searchParams.get("after") || 0),
      });
    }
    await sleep(latency);
    if (method !== "GET") {
      stats.writes++;
      return response({ error: "Synthetic fixture is read-only" }, 503);
    }
    stats.requested.add(p + u.search);
    if (p === "/v1/capabilities")
      return response({
        messageAudit: { versions: [] },
        queue: { versions: [1] },
      });
    if (p === "/v1/whoami")
      return response({ node: "workspace", user: "fixture" });
    if (p === "/v1/tasks") return response({ tasks });
    if (p === "/v1/work-items") {
      const q = (name) => u.searchParams.get(name);
      const after = Number(q("after") || 0);
      const limit = Number(q("limit") || 200);
      const list = items.filter(
        (x) =>
          (!q("taskId") || x.taskId === q("taskId")) &&
          (!q("status") || x.status === q("status")) &&
          (!q("kind") || x.kind === q("kind")) &&
          x.itemSeq > after,
      );
      const page = list.slice(0, limit);
      return response({
        items: page,
        next: list.length > limit ? page.at(-1).itemSeq : 0,
      });
    }
    const match = p.match(/^\/v1\/tasks\/([^/]+)(.*)$/);
    const task = match && tasks.find((t) => t.id === match[1]);
    if (!task) {
      stats.unknown.add(p);
      return response({ error: "not found" }, 404);
    }
    const tail = match[2];
    if (!tail)
      return response({
        task,
        agents: agentsByTask.get(task.id),
        latestSeq: 51199,
      });
    if (tail === "/agents")
      return response({ agents: agentsByTask.get(task.id) });
    if (tail === "/messages")
      return response({
        messages: messagesByTask
          .get(task.id)
          .filter((x) => x.seq > Number(u.searchParams.get("after") || 0)),
      });
    if (tail === "/decisions") return response({ decisions: [], nextAfter: 0 });
    if (tail === "/obligations") return response({ obligations: [] });
    if (tail === "/delegation-windows") return response({ windows: [] });
    if (tail === "/queue")
      return response({ entries: queueEntries, cursor: "" });
    if (tail === "/team-queue") return response({ entries: [] });
    stats.unknown.add(p);
    return response({ error: "not found" }, 404);
  }

  await vault.localAPI("/unlock", "POST", {
    password: "Synthetic tab switch QA passphrase",
  });
  for (let i = 0; i < 4; i++)
    await vault.localAPI("/teams", "POST", {
      name: "Synthetic team " + i,
      orchestrator: "fixture-lead",
      members: [
        {
          name: "fixture-lead",
          runtime: "codex",
          role: "Coordinator",
          prompt: "Synthetic instructions",
        },
      ],
    });
  const live = createHubClient({
    baseURL: "http://synthetic-tabs.invalid",
    token: "synthetic-only",
    fetchImpl: transport,
  });
  const persistence = vault.hubReadCachePersistence();
  const persisted = { loads: 0, saving: 0, saves: 0 };
  const cache = createHubReadCache({
    check: persistence.check,
    load: () => {
      persisted.loads++;
      return persistence.load();
    },
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
  const noop = () => {};
  const notice = (text) => console.log("notice: " + text);
  const common = {
    client: () => client,
    notice,
    configure: noop,
    getTabs: () => [],
    activate: noop,
  };
  const taskHub = {
    groupOf: () => null,
    newTask: noop,
    addAgent: noop,
    setupHandler: noop,
    attachToCurrent: noop,
    settings: noop,
    revealAgent: noop,
    inspectTools: async () => ({}),
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
    notice,
    confirm: async () => true,
    inspectTools: async () => ({}),
  };
  let modes;
  const views = {
    board: createBoardView({
      ...common,
      addAgent: noop,
      revealAgent: noop,
      attachTask: noop,
      newTask: noop,
      settings: noop,
      intentPersistence: vault.boardIntentPersistence(),
    }),
    tasks: createTasksView({
      ...common,
      taskHub,
      confirm: async () => true,
      openWorkItems: noop,
      openBoard: noop,
    }),
    bugs: createWorkItemsView({
      ...common,
      kind: "bug",
      dialog: noop,
      closeDialog: noop,
      openBoard: noop,
      draftPersistence: vault.workItemDraftPersistence(),
    }),
    features: createWorkItemsView({
      ...common,
      kind: "feature",
      dialog: noop,
      closeDialog: noop,
      openBoard: noop,
      draftPersistence: vault.workItemDraftPersistence(),
    }),
    queue: createQueueView({
      ...common,
      dialog: noop,
      closeDialog: noop,
      intentPersistence: vault.queueIntentPersistence(),
    }),
    agents: createAgentsView({
      ...libraryHost,
      openAgents: () => modes?.set("agents"),
    }),
    teams: createTeamsView({
      ...libraryHost,
      openAgents: () => modes?.set("agents"),
      newTask: noop,
      addTeam: noop,
    }),
  };
  // Mirrors the onChange in client/main.js: hide every view, clear the mode
  // view, then mount and show the chosen one.
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
  window.qa = {
    persisted,
    async switchTo(mode, selector) {
      const t0 = performance.now();
      document.querySelector(`header [data-mode="${mode}"]`).click();
      const painted = await new Promise((resolve) => {
        const done = () => {
          observer.disconnect();
          clearTimeout(timer);
          requestAnimationFrame(() =>
            setTimeout(() => resolve(performance.now() - t0), 0),
          );
        };
        const observer = new MutationObserver(() => {
          if (document.querySelector(selector)) done();
        });
        const timer = setTimeout(() => {
          observer.disconnect();
          resolve(-1);
        }, 30000);
        if (document.querySelector(selector)) done();
        else
          observer.observe(document.body, {
            childList: true,
            subtree: true,
            attributes: true,
            attributeFilter: ["hidden"],
          });
      });
      await window.rendered;
      return Math.round(painted);
    },
    async idle() {
      while (persisted.saving) await sleep(20);
    },
    changeItem(round) {
      items[0].title = `Synthetic work item 0 changed in round ${round}`;
      items[0].revision++;
    },
    async coverage() {
      const saved = (await persistence.load())?.entries || [];
      const keys = new Set(saved.map((entry) => entry.key));
      const bigProject = "/v1/tasks/" + tasks[0].id;
      // The big project read and every Bugs/Features page. Other work-item
      // reads (the Board's in-progress eligibility) deliberately bypass the
      // read cache.
      const wanted = [...stats.requested].filter((path) => {
        const url = new URL(path, "http://fixture.invalid");
        return (
          url.pathname === bigProject ||
          (url.pathname === "/v1/work-items" &&
            ["bug", "feature"].includes(url.searchParams.get("kind")))
        );
      });
      return {
        wanted,
        missing: wanted.filter((path) => !keys.has(path)),
        unknown: [...stats.unknown],
        writes: stats.writes,
        filesTabs: document.querySelectorAll('header [data-mode="files"]')
          .length,
      };
    },
  };
  window.ready = true;
}

const html = (options) =>
  `<!doctype html><html><head><meta charset="utf-8"><link rel="stylesheet" href="/client/style.css"><link rel="stylesheet" href="/client/work-items.css"><link rel="stylesheet" href="/client/queue.css"></head>
<body><div id="app"><div id="workspace"><aside></aside><main><header></header><div class="terminal-shell"></div></main></div><dialog id="dialog"></dialog><div id="notice" hidden></div></div>
<script type="module">(${fixturePage.toString()})(${JSON.stringify(options)}).catch((error) => { window.fixtureError = String(error?.stack || error); });</script></body></html>`;

const median = (values) => {
  const sorted = [...values].sort((a, b) => a - b);
  return sorted[Math.floor(sorted.length / 2)];
};

async function measure(browser, origin, name, { refreshMs, changeItems }) {
  const context = await browser.newContext({
    viewport: { width: 1440, height: 900 },
  });
  try {
    const page = await context.newPage();
    const errors = [];
    page.on("pageerror", (error) => errors.push(error.message));
    await page.goto(
      `${origin}/tab-switch-test?` +
        new URLSearchParams({ latency: LATENCY_MS, refreshMs }),
    );
    await page.waitForFunction(
      () => window.ready || window.fixtureError,
      null,
      {
        timeout: 60000,
      },
    );
    const fixtureError = await page.evaluate(() => window.fixtureError);
    assert.equal(fixtureError, undefined, fixtureError);
    const switchTo = (mode) =>
      page.evaluate(
        ([m, s]) => window.qa.switchTo(m, s),
        [mode, SELECTORS[mode]],
      );
    // First visit since unlock fills the cache; it is not timed.
    for (const mode of TABS)
      assert.notEqual(
        await switchTo(mode),
        -1,
        `${name}: ${mode} never painted`,
      );
    await page.evaluate(() => window.qa.idle());
    const samples = Object.fromEntries(TABS.map((mode) => [mode, []]));
    for (let round = 1; round <= ROUNDS; round++) {
      if (changeItems)
        await page.evaluate((r) => window.qa.changeItem(r), round);
      for (const mode of TABS) {
        samples[mode].push(await switchTo(mode));
        await page.waitForTimeout(100);
      }
    }
    await page.evaluate(() => window.qa.idle());
    const persisted = await page.evaluate(() => window.qa.persisted);
    const coverage = await page.evaluate(() => window.qa.coverage());
    const medians = Object.fromEntries(
      TABS.map((mode) => [mode, median(samples[mode])]),
    );
    console.log(
      `${name}: median ms ${JSON.stringify(medians)} samples ${JSON.stringify(samples)} saves ${persisted.saves}`,
    );
    assert.deepEqual(errors, [], `${name}: page errors`);
    for (const mode of TABS) {
      assert.ok(
        samples[mode].every((ms) => ms >= 0),
        `${name}: ${mode} did not paint in a round: ${samples[mode]}`,
      );
      assert.ok(
        medians[mode] <= TARGET_MS,
        `${name}: ${mode} median ${medians[mode]} ms exceeds ${TARGET_MS} ms (${samples[mode]})`,
      );
    }
    assert.equal(persisted.loads, 1, `${name}: cache loads`);
    assert.ok(coverage.wanted.length > 2, `${name}: ${coverage.wanted}`);
    assert.deepEqual(coverage.missing, [], `${name}: working set evicted`);
    assert.deepEqual(coverage.unknown, [], `${name}: unexpected routes`);
    assert.equal(coverage.writes, 0, `${name}: hub writes`);
    assert.equal(coverage.filesTabs, 0, `${name}: Files tab is visible`);
    if (changeItems) assert.ok(persisted.saves > 0, `${name}: no saves`);
  } finally {
    await context.close();
  }
}

export async function exerciseTabSwitch(browser) {
  const server = await createServer({
    configFile: false,
    cacheDir: viteCacheDir(import.meta.url),
    root: process.cwd(),
    logLevel: "error",
    server: { host: "127.0.0.1", port: 0 },
    plugins: [
      {
        name: "tab-switch-fixture",
        configureServer(vite) {
          vite.middlewares.use("/tab-switch-test", (req, res) => {
            const query = new URL(req.url, "http://fixture.invalid")
              .searchParams;
            res.setHeader("Content-Type", "text/html");
            res.end(
              html({
                latency: Number(query.get("latency")),
                refreshMs: Number(query.get("refreshMs")),
              }),
            );
          });
        },
      },
    ],
  });
  await server.listen();
  try {
    const origin = `http://127.0.0.1:${server.httpServer.address().port}`;
    const engine = browser.browserType().name();
    await measure(browser, origin, `${engine} cached`, {
      refreshMs: 20000,
      changeItems: false,
    });
    await measure(browser, origin, `${engine} refreshing`, {
      refreshMs: 300,
      changeItems: true,
    });
  } finally {
    await server.close();
  }
}

if (import.meta.url === pathToFileURL(process.argv[1]).href) {
  for (const engine of [chromium, webkit]) {
    const browser = await engine.launch();
    try {
      await exerciseTabSwitch(browser);
    } finally {
      await browser.close();
    }
  }
  console.log("tab switch browser test passed");
}
