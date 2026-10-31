// verify-matrix: browser-helper
// Top-level tab switch timing on a synthetic hub sized like the live one,
// with a long retained event feed and a live event about every second. Only
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
// Retained global events the feed replays from the start, and the interval of
// new live events while the page is open.
const BACKLOG_EVENTS = 30000;
const LIVE_EVENT_MS = 1000;
// Time spent on a tab before leaving it in the revisit case.
const DWELL_MS = 1000;
// After client.dispose() no long poll may stay open longer than this.
const DISPOSE_MS = 500;
// Bugs first: the owner reported it as the slowest.
const DATA_TABS = [
  "bugs",
  "board",
  "tasks",
  "agents",
  "teams",
  "features",
  "queue",
];
const TABS = [...DATA_TABS, "terminals"];
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
// Tabs that render from the local vault at once and paint no loading shell
// ([data-view-shell]); every other tab must show one on an empty cache.
const SHELL_FREE = new Set(["agents", "teams", "terminals"]);

// Runs in the page, serialized with toString, so it may only use its
// arguments and dynamic imports.
async function fixturePage({ latency, refreshMs, backlog, liveEventMs }) {
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

  // Fixture sized like the live hub: 16 projects (2 open); the first has 3,199
  // agents (3,173 closed); 200 long messages per project; 1,336 work items
  // (1,089 bugs, 247 features, 35 open) with descriptions of about 2,900
  // characters and every 20th 8,039; 10 queue entries; 4 saved teams.
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
  const items = Array.from({ length: 1336 }, (_, i) => ({
    id: "wi_" + hex(i + 1),
    taskId: tasks[i % 5 === 4 ? 1 : 0].id,
    kind: (i * 247) % 1336 < 247 ? "feature" : "bug",
    title: "Synthetic work item " + i + " " + lorem(60, i),
    description: lorem(
      i % 20 === 0 ? 8039 : i % 2 ? 2905 + (i % 7) * 300 : 1500 + (i % 9) * 150,
      i,
    ),
    priority: ["normal", "high", "low"][i % 3],
    status: i % 39 === 0 ? "open" : i % 5 < 3 ? "done" : "dismissed",
    revision: 1 + (i % 4),
    itemSeq: i + 1,
    createdAt: "2026-10-01T00:00:00Z",
    updatedAt: "2026-10-02T00:00:00Z",
  }));
  const queueEntries = Array.from({ length: 10 }, (_, i) => ({
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
    state: i % 2 ? "waiting" : "running",
    template: "planned",
    createdAt: "2026-10-30T00:00:00Z",
    updatedAt: "2026-10-30T00:00:00Z",
  }));

  const stats = {
    requested: new Set(),
    unknown: new Set(),
    writes: 0,
    eventReads: 0,
    // Long polls (feed reads that may wait) in flight now and at most.
    openPolls: 0,
    maxOpenPolls: 0,
  };
  // The global feed: a retained backlog, then a live event every interval.
  // Events are generated from their sequence number, so the backlog costs no
  // memory. A waiting read returns on a new event or after 25 s.
  let eventSeq = backlog;
  const waiters = new Set();
  setInterval(() => {
    eventSeq++;
    for (const wake of waiters) wake();
    waiters.clear();
  }, liveEventMs);
  const eventAt = (seq) => ({
    seq,
    taskId: tasks[seq % 2].id,
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
    if (p === "/v1/events" || /^\/v1\/tasks\/[^/]+\/events$/.test(p)) {
      // Feed reads honor init.signal as window.fetch does: an abort ends the
      // read at once with an AbortError.
      const signal = init.signal;
      const abortError = () =>
        new DOMException("The operation was aborted.", "AbortError");
      if (signal?.aborted) throw abortError();
      const poll = !!u.searchParams.get("wait");
      stats.eventReads++;
      if (poll)
        stats.maxOpenPolls = Math.max(stats.maxOpenPolls, ++stats.openPolls);
      try {
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
        const limit = Math.min(
          1000,
          Number(u.searchParams.get("limit") || 100),
        );
        let events = eventsAfter(task, after, limit);
        if (!events.length && poll) {
          await hold(25000, true);
          events = eventsAfter(task, after, limit);
        }
        return response({
          events,
          next: events.length ? events.at(-1).seq : after,
        });
      } finally {
        if (poll) stats.openPolls--;
      }
    }
    if (p.endsWith("/changes")) {
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
  // A reload unlocks the same vault; its saved teams are already there.
  if (!vault.localData().teams?.length)
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
    // Returns the time from the click to the first visible shell or data,
    // the time to data, the shell's structure when one was seen, and whether
    // a shell was left behind once data painted.
    async switchTo(mode, selector) {
      const t0 = performance.now();
      const shellSelector = "#mode-view [data-view-shell]";
      let shell = null,
        firstSeen = false,
        resolveFirst;
      const first = new Promise((resolve) => (resolveFirst = resolve));
      const settle = (resolve) =>
        requestAnimationFrame(() =>
          setTimeout(() => resolve(Math.round(performance.now() - t0)), 0),
        );
      const look = () => {
        const node = document.querySelector(shellSelector);
        if (node && !shell)
          shell = {
            heading: !!node.querySelector("h2")?.textContent.trim(),
            control: !!node.querySelector("button, input, select"),
            status: !!node.querySelector('[role="status"]')?.textContent.trim(),
          };
        const data = !!document.querySelector(selector);
        if (!firstSeen && (node || data)) {
          firstSeen = true;
          settle(resolveFirst);
        }
        return data;
      };
      document.querySelector(`header [data-mode="${mode}"]`).click();
      let shellAfterData = false;
      const painted = await new Promise((resolve) => {
        const done = () => {
          observer.disconnect();
          clearTimeout(timer);
          shellAfterData = !!document.querySelector(shellSelector);
          settle(resolve);
        };
        const observer = new MutationObserver(() => {
          if (look()) done();
        });
        const timer = setTimeout(() => {
          observer.disconnect();
          if (!firstSeen) resolveFirst(-1);
          resolve(-1);
        }, 30000);
        if (look()) done();
        else
          observer.observe(document.body, {
            childList: true,
            subtree: true,
            attributes: true,
            attributeFilter: ["hidden"],
          });
      });
      await window.rendered;
      shellAfterData ||= !!document.querySelector(shellSelector);
      return { first: await first, data: painted, shell, shellAfterData };
    },
    polls: () => ({ open: stats.openPolls, max: stats.maxOpenPolls }),
    // Disposes the read client and returns how long its long polls took to
    // close (-1 if any is still open after limitMs).
    async dispose(limitMs) {
      const t0 = performance.now();
      client.dispose();
      while (stats.openPolls > 0) {
        if (performance.now() - t0 > limitMs) return -1;
        await sleep(10);
      }
      return Math.round(performance.now() - t0);
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
        eventReads: stats.eventReads,
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

async function measure(browser, origin, engine) {
  const context = await browser.newContext({
    viewport: { width: 1440, height: 900 },
  });
  try {
    const page = await context.newPage();
    const errors = [];
    page.on("pageerror", (error) => errors.push(error.message));
    const ready = async () => {
      await page.waitForFunction(
        () => window.ready || window.fixtureError,
        null,
        { timeout: 60000 },
      );
      const fixtureError = await page.evaluate(() => window.fixtureError);
      assert.equal(fixtureError, undefined, fixtureError);
    };
    await page.goto(
      `${origin}/tab-switch-test?` +
        new URLSearchParams({
          latency: LATENCY_MS,
          refreshMs: 20000,
          backlog: BACKLOG_EVENTS,
          liveEventMs: LIVE_EVENT_MS,
        }),
    );
    await ready();
    const shellFailures = [];
    let warmShells = 0,
      warmSwitches = 0;
    const visit = async (mode, cold = false) => {
      const result = await page.evaluate(
        ([m, s]) => window.qa.switchTo(m, s),
        [mode, SELECTORS[mode]],
      );
      if (result.shell) {
        const { heading, control, status } = result.shell;
        if (!heading || !control || !status)
          shellFailures.push(`${mode} shell ${JSON.stringify(result.shell)}`);
        if (!cold) warmShells++;
      }
      if (!cold) warmSwitches++;
      if (result.shellAfterData)
        shellFailures.push(`${mode} shell remained after data painted`);
      return result;
    };
    const switchTo = async (mode) => (await visit(mode)).data;
    const samples = {};
    const record = (name, mode, ms) =>
      ((samples[name] ||= {})[mode] ||= []).push(ms);
    // The first visit with an empty cache has to wait for the network for
    // its data, so cold-first is recorded but not held to the target; it
    // fills the saved cache. Its shell (or data) is: cold-shell.
    for (const mode of TABS) {
      const result = await visit(mode, true);
      record("cold-first", mode, result.data);
      record("cold-shell", mode, result.first);
      if (DATA_TABS.includes(mode) && !SHELL_FREE.has(mode) && !result.shell)
        shellFailures.push(`${mode} painted no shell on an empty cache`);
    }
    await page.evaluate(() => window.qa.idle());
    // The owner's usual first visit: a reload, the same vault unlocked again
    // with its saved cache, and a feed that replays the backlog from the start.
    await page.reload();
    await ready();
    for (const mode of TABS) {
      record("warm-first", mode, await switchTo(mode));
      await page.waitForTimeout(DWELL_MS);
    }
    // wi_6fad's case: rounds through every tab while the data changes.
    for (let round = 1; round <= ROUNDS; round++) {
      await page.evaluate((r) => window.qa.changeItem(r), round);
      for (const mode of TABS) {
        record("repeat", mode, await switchTo(mode));
        await page.waitForTimeout(100);
      }
    }
    // A revisit after two other tabs, each with a real dwell.
    for (const mode of DATA_TABS) {
      const others = DATA_TABS.filter((other) => other !== mode);
      await switchTo(mode);
      for (let round = 0; round < ROUNDS; round++) {
        for (const other of [
          others[round % others.length],
          others[(round + 3) % others.length],
        ]) {
          await page.waitForTimeout(DWELL_MS);
          await switchTo(other);
        }
        await page.waitForTimeout(DWELL_MS);
        record("revisit-after-two", mode, await switchTo(mode));
      }
    }
    await page.evaluate(() => window.qa.idle());
    const persisted = await page.evaluate(() => window.qa.persisted);
    const coverage = await page.evaluate(() => window.qa.coverage());
    const polls = await page.evaluate(() => window.qa.polls());
    const disposed = await page.evaluate(
      (limit) => window.qa.dispose(limit),
      DISPOSE_MS,
    );
    const failures = [];
    for (const [name, byMode] of Object.entries(samples)) {
      const medians = Object.fromEntries(
        Object.entries(byMode).map(([mode, ms]) => [mode, median(ms)]),
      );
      console.log(
        `${engine} ${name}: median ms ${JSON.stringify(medians)} samples ${JSON.stringify(byMode)}`,
      );
      for (const [mode, ms] of Object.entries(byMode)) {
        if (!ms.every((value) => value >= 0))
          failures.push(`${name} ${mode} did not paint: ${ms}`);
        else if (name !== "cold-first" && medians[mode] > TARGET_MS)
          failures.push(
            `${name} ${mode} median ${medians[mode]} ms exceeds ${TARGET_MS} ms (${ms})`,
          );
      }
    }
    console.log(
      `${engine}: saves ${persisted.saves} cache loads ${persisted.loads} event reads ${coverage.eventReads}`,
    );
    console.log(
      `${engine}: shells on warm switches ${warmShells}/${warmSwitches} (information only); most open long polls ${polls.max}; long polls closed ${disposed} ms after dispose`,
    );
    assert.deepEqual(errors, [], `${engine}: page errors`);
    assert.deepEqual(failures, [], `${engine}: tab switches`);
    assert.deepEqual(shellFailures, [], `${engine}: loading shells`);
    assert.ok(
      polls.max <= 1,
      `${engine}: ${polls.max} long polls were open at once`,
    );
    assert.ok(
      disposed >= 0,
      `${engine}: long polls still open ${DISPOSE_MS} ms after dispose`,
    );
    assert.equal(persisted.loads, 1, `${engine}: cache loads after reload`);
    assert.ok(persisted.saves > 0, `${engine}: no saves`);
    assert.ok(
      coverage.eventReads > BACKLOG_EVENTS / 100,
      `${engine}: the feed did not replay its backlog (${coverage.eventReads} reads)`,
    );
    assert.ok(coverage.wanted.length > 2, `${engine}: ${coverage.wanted}`);
    assert.deepEqual(coverage.missing, [], `${engine}: working set evicted`);
    assert.deepEqual(coverage.unknown, [], `${engine}: unexpected routes`);
    assert.equal(coverage.writes, 0, `${engine}: hub writes`);
    assert.equal(coverage.filesTabs, 0, `${engine}: Files tab is visible`);
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
                backlog: Number(query.get("backlog")),
                liveEventMs: Number(query.get("liveEventMs")),
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
    await measure(browser, origin, browser.browserType().name());
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
