// Owner interventions in Projects (wi_d55e7d8c840a3739, order #13865).
// A real compiled hub and tt record synthetic interventions in a disposable
// state directory; the Projects view reads them through the real HTTP client
// in Chromium and WebKit. No live hub, tailnet, token or tmux session is used.
import assert from "node:assert/strict";
import { execFile, spawn } from "node:child_process";
import { mkdir, mkdtemp, rm } from "node:fs/promises";
import { createServer as createNetServer } from "node:net";
import { once } from "node:events";
import { tmpdir } from "node:os";
import path from "node:path";
import { promisify } from "node:util";
import { chromium, webkit } from "@playwright/test";
import { createServer } from "vite";
import { prepareTestBinary } from "./test-binaries.mjs";

const exec = promisify(execFile),
  root = process.cwd();
const state = await mkdtemp(path.join(tmpdir(), "tailterm-interventions-"));
const artifacts = path.join(root, ".build/interventions");
await mkdir(artifacts, { recursive: true });
// Omit HOME too: tt must not read the host's hub credential file.
const fixtureEnv = Object.fromEntries(
  Object.entries(process.env).filter(
    ([key]) => !/^(TAILTERM_|CODEX_|TT_|TMUX)/.test(key) && key !== "HOME",
  ),
);
const zone = "America/Los_Angeles";
const pause = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

async function freePort() {
  const server = createNetServer().listen(0, "127.0.0.1");
  await once(server, "listening");
  const port = server.address().port;
  await new Promise((resolve) => server.close(resolve));
  return port;
}

let backend, vite, hub;
async function api(method, route, body, expected = 200) {
  if (method !== "GET") await pause(60); // the hub's write bucket
  const response = await fetch(hub + route, {
    method,
    signal: AbortSignal.timeout(10000),
    headers:
      body === undefined ? undefined : { "content-type": "application/json" },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const text = await response.text();
  assert.equal(response.status, expected, `${method} ${route}: ${text}`);
  return text ? JSON.parse(text) : null;
}
const ttBinary = path.join(state, "tt");
async function tt(task, args, agent = null) {
  await pause(60);
  const env = { ...fixtureEnv, TAILTERM_HUB: hub, TAILTERM_TASK: task };
  if (agent)
    Object.assign(env, {
      TAILTERM_AGENT: agent.id,
      TAILTERM_AGENT_NAME: agent.name,
    });
  return exec(ttBinary, ["owner", ...args], {
    cwd: state,
    timeout: 15000,
    env,
  });
}

try {
  const hubBinary = await prepareTestBinary({
    root,
    target: "hub",
    output: path.join(state, "tailterm-hub"),
  });
  await prepareTestBinary({ root, target: "tt", output: ttBinary });
  hub = `http://127.0.0.1:${await freePort()}`;
  backend = spawn(hubBinary, {
    env: {
      ...fixtureEnv,
      TAILTERM_STATE: state,
      TAILTERM_DEV_LISTEN: new URL(hub).host,
      TAILTERM_TCP_LISTEN: "",
    },
    stdio: "ignore",
  });
  let ready = false;
  for (let i = 0; i < 100 && !ready; i++) {
    try {
      await api("GET", "/v1/tasks");
      ready = true;
    } catch {
      await pause(100);
    }
  }
  assert.ok(ready, "isolated hub failed to start");

  // Synthetic shadow project, product home project and an empty neighbour.
  const shadow = await api(
    "POST",
    "/v1/tasks",
    { name: "Synthetic shadow week" },
    201,
  );
  const home = await api(
    "POST",
    "/v1/tasks",
    { name: "Synthetic product home" },
    201,
  );
  const quiet = await api(
    "POST",
    "/v1/tasks",
    { name: "Synthetic quiet project" },
    201,
  );
  const item = await api(
    "POST",
    `/v1/tasks/${shadow.id}/work-items`,
    { kind: "feature", title: "Synthetic delivery", requestId: "shadow-item" },
    201,
  );
  const product = await api(
    "POST",
    `/v1/tasks/${home.id}/work-items`,
    {
      kind: "feature",
      title: "Synthetic product fix",
      requestId: "product-item",
    },
    201,
  );
  const agent = await api(
    "POST",
    `/v1/tasks/${shadow.id}/agents`,
    {
      name: "synthetic-builder",
      host: "synthetic.invalid",
      session: "unused",
      runtime: "codex",
      cwd: state,
    },
    201,
  );

  // An agent session is refused by the real CLI before anything is stored.
  await assert.rejects(
    tt(
      shadow.id,
      [
        "intervene",
        "--kind",
        "nudge",
        "--item",
        item.id,
        "--text",
        "agent attempt",
      ],
      agent,
    ),
    (error) => /not agent sessions/.test(error.stderr),
  );
  const recorded = [];
  for (const [kind, linked, text] of [
    ["nudge", true, "Nudged the stalled builder"],
    ["release", true, "Released the candidate by hand"],
    ["nudge", true, "Nudged the reviewer"],
    ["gate-fix", false, "Fixed the verification gate by hand"],
  ]) {
    const args = [
      "intervene",
      "--kind",
      kind,
      "--item",
      item.id,
      "--text",
      text,
      "--json",
      "--request-id",
      `browser-${recorded.length}`,
    ];
    if (linked) args.push("--product-item", product.id);
    const { stdout } = await tt(shadow.id, args);
    recorded.push(JSON.parse(stdout));
  }
  for (const m of recorded) {
    assert.equal(m.from.agentId || "", "");
    assert.deepEqual(m.workItems, [
      {
        itemTaskId: shadow.id,
        itemId: item.id,
        itemRevision: item.revision,
        relationship: "primary",
      },
    ]);
  }
  const unlinked = recorded[3];
  const cli = await tt(shadow.id, ["interventions", "--tz", zone]);
  assert.match(
    cli.stdout,
    /^4 interventions \(America\/Los_Angeles\); 3 of 4 linked to a product item/,
  );
  const board = await api("GET", `/v1/tasks/${shadow.id}/messages?limit=50`);
  assert.equal(
    board.messages.filter((m) => m.intervention).length,
    4,
    "interventions are ordinary Board messages",
  );
  const today = new Intl.DateTimeFormat("en-CA", {
    timeZone: zone,
    year: "numeric",
    month: "2-digit",
    day: "2-digit",
  }).format(new Date());

  const html = `<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><link rel="stylesheet" href="/client/style.css"></head><body><main id="projects"></main><script type="module">
import {createTasksView} from '/client/tasks-view.js';
import {createHubClient} from '/client/hub-client.js';
const tasks=${JSON.stringify([shadow, quiet])};
window.calls=[];window.pending=[];window.delay=false;window.offline=false;window.oldHub=false;
const live=createHubClient({baseURL:location.origin,fetchImpl:async(url,init)=>{
  window.calls.push(url);
  if(window.oldHub)return {status:404,statusText:'Not Found',text:async()=>'{"error":"not found"}'};
  const response=await fetch(url,init),text=await response.text();
  const result={status:response.status,statusText:response.statusText,headers:response.headers,text:async()=>text};
  return window.delay?await new Promise(resolve=>window.pending.push(()=>resolve(result))):result;
}});
const client={listTasks:async()=>tasks,getTask:async id=>({task:tasks.find(t=>t.id===id),agents:[]}),listTeamDelivery:async()=>({entries:[],concurrencyLimit:1}),listOwnerObligations:async()=>[],capabilities:async()=>({}),subscribe:()=>({stop(){}}),cacheStatus:()=>({label:window.offline?'Saved data · offline':''}),getUsage:async()=>({summary:{},items:[],overhead:{summary:{},phases:[],roles:[],models:[],phaseRoles:[]}}),listInterventions:(id,params)=>live.listInterventions(id,params)};
const view=createTasksView({client:()=>client,taskHub:{hasPendingResume:()=>false,groupOf:()=>null},getTabs:()=>[],activate:()=>{},notice:()=>{},confirm:async()=>true,openBoard:()=>{},openWorkItems:()=>{},configure:()=>{}});
view.mount(document.querySelector('#projects'));window.ready=view.show();window.reload=()=>view.reload();
</script></body></html>`;
  vite = await createServer({
    configFile: false,
    root,
    logLevel: "silent",
    server: { host: "127.0.0.1", port: 0, proxy: { "/v1": hub } },
    plugins: [
      {
        name: "interventions-fixture",
        configureServer(s) {
          s.middlewares.use("/interventions-test", (_req, res) => {
            res.setHeader("Content-Type", "text/html");
            res.end(html);
          });
        },
      },
    ],
  });
  await vite.listen();
  const origin = "http://127.0.0.1:" + vite.httpServer.address().port;

  for (const engine of [chromium, webkit]) {
    const browser = await engine.launch();
    try {
      const context = await browser.newContext({
        viewport: { width: 1200, height: 900 },
        timezoneId: zone,
      });
      await context.route("**/*", (r) =>
        new URL(r.request().url()).origin === origin ? r.continue() : r.abort(),
      );
      const page = await context.newPage(),
        errors = [];
      page.on("pageerror", (e) => errors.push(e.message));
      page.setDefaultTimeout(7000);
      await page.goto(origin + "/interventions-test");
      await page.evaluate(() => window.ready);
      await page.locator(`[data-task-select="${shadow.id}"]`).click();
      await page.waitForFunction(() =>
        document
          .querySelector(".tasks-detail h2")
          ?.textContent.includes("Synthetic shadow week"),
      );
      const disclosure = page.locator("[data-interventions-disclosure]");
      await disclosure.waitFor({ state: "attached" });
      // Closed by default and lazy: nothing is requested until it opens.
      assert.equal(await disclosure.evaluate((el) => el.open), false);
      assert.equal((await page.evaluate(() => window.calls)).length, 0);

      // A slow read never holds the project detail.
      await page.evaluate(() => (window.delay = true));
      await disclosure.locator(":scope > summary").click();
      await page.waitForFunction(() => window.pending.length === 1);
      assert.match(
        await page.locator(".tasks-detail h2").innerText(),
        /Synthetic shadow week/,
      );
      assert.match(await disclosure.innerText(), /Loading interventions…/);
      await page.evaluate(() => {
        window.delay = false;
        window.pending.shift()();
      });
      await page.locator("[data-interventions-headline]").waitFor();
      const call = new URL((await page.evaluate(() => window.calls))[0]);
      assert.equal(call.pathname, `/v1/tasks/${shadow.id}/interventions`);
      assert.equal(call.searchParams.get("tz"), zone);
      assert.equal(call.searchParams.get("limit"), "1");

      const text = await disclosure.innerText();
      assert.match(text, /4 interventions · 3 of 4 linked to a product item/);
      assert.match(text, /days in America\/Los_Angeles/);
      const kinds = await page
        .locator("[data-interventions-kinds] tr")
        .allInnerTexts();
      assert.deepEqual(
        kinds.map((row) => row.replace(/\s+/g, " ").trim()),
        ["release 1", "nudge 2", "gate-fix 1"],
      );
      const days = await page
        .locator("[data-intervention-day]")
        .allInnerTexts();
      assert.equal(days.length, 1);
      assert.deepEqual(
        days[0].split("\t").map((cell) => cell.trim()),
        [today, "4", "3", "release 1 · nudge 2 · gate-fix 1"],
      );
      const unlinkedRows = await page
        .locator("[data-intervention-unlinked]")
        .allInnerTexts();
      assert.deepEqual(unlinkedRows, [
        `#${unlinked.seq} · gate-fix · ${item.id} · ${today}`,
      ]);
      await page.screenshot({
        path: path.join(artifacts, `interventions-${engine.name()}.png`),
        fullPage: true,
      });

      // Offline shows the saved-data label; an old hub says it is unsupported.
      await page.evaluate(() => (window.offline = true));
      await page.locator("[data-interventions-refresh]").click();
      await page.waitForFunction(() =>
        document
          .querySelector(".project-interventions")
          .textContent.includes("Saved data · offline"),
      );
      assert.match(await disclosure.innerText(), /4 interventions/);
      await page.evaluate(() => {
        window.offline = false;
        window.oldHub = true;
      });
      await page.locator("[data-interventions-refresh]").click();
      await page.waitForFunction(() =>
        document
          .querySelector(".project-interventions")
          .textContent.includes("Interventions unsupported by this hub"),
      );
      await page.evaluate(() => (window.oldHub = false));
      await page.locator("[data-interventions-refresh]").click();
      await page.waitForFunction(
        () =>
          !document
            .querySelector(".project-interventions")
            .textContent.includes("unsupported"),
      );

      // Per-project state: the neighbour starts closed and shows its own empty summary.
      await page.locator(`[data-task-select="${quiet.id}"]`).click();
      await page.waitForFunction(
        (id) =>
          document
            .querySelector(".tasks-detail h2")
            ?.textContent.includes("Synthetic quiet project"),
        quiet.id,
      );
      assert.equal(
        await page
          .locator("[data-interventions-disclosure]")
          .evaluate((el) => el.open),
        false,
      );
      await page.locator("[data-interventions-disclosure] > summary").click();
      await page.getByText("No interventions recorded").waitFor();
      assert.equal(await page.locator("[data-interventions-days]").count(), 0);

      // A core reload keeps the open disclosure and its content.
      await page.evaluate(() => window.reload());
      assert.equal(
        await page
          .locator("[data-interventions-disclosure]")
          .evaluate((el) => el.open),
        true,
      );

      await page.setViewportSize({ width: 390, height: 844 });
      await page.locator(`[data-task-select="${shadow.id}"]`).click();
      await page.locator("[data-interventions-headline]").waitFor();
      const dimensions = await page.evaluate(() => ({
        scroll: document.documentElement.scrollWidth,
        width: window.innerWidth,
      }));
      assert.ok(
        dimensions.scroll <= dimensions.width + 1,
        JSON.stringify(dimensions),
      );
      await page.screenshot({
        path: path.join(artifacts, `interventions-${engine.name()}-narrow.png`),
        fullPage: true,
      });
      assert.deepEqual(errors, []);
      await context.close();
      console.log(
        `${engine.name()}: Interventions disclosure per-day, per-kind, linked and unlinked checks pass`,
      );
    } finally {
      await browser.close();
    }
  }
} finally {
  await vite?.close();
  if (backend && backend.exitCode === null) {
    backend.kill();
    await once(backend, "exit");
  }
  await rm(state, { recursive: true, force: true });
}
