// Browser acceptance checks for Bugs/Features against a real, throwaway hub.
// No Tailnet credentials, live project data, or shared tmux sockets are used.
import { chromium, webkit } from "@playwright/test";
import { createServer } from "node:http";
import { once } from "node:events";
import { spawn, execFile } from "node:child_process";
import { promisify } from "node:util";
import { mkdtemp, readFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import assert from "node:assert/strict";

const exec = promisify(execFile);
const root = process.cwd();
const state = await mkdtemp(path.join(tmpdir(), "tailterm-work-items-"));
// This test itself is coordinated through Tailterm. Its inherited identity must
// never become part of the isolated fixture's hub requests or launch commands.
const fixtureEnv = Object.fromEntries(
  Object.entries(process.env).filter(([key]) => !key.startsWith("TAILTERM_")),
);
const binary = path.join(root, ".build/ttbin/tailterm-hub-work-items-test");
await exec("go", ["build", "-ldflags=-s -w", "-o", binary, "./cmd/tailterm-hub"], {
  cwd: path.join(root, "hub"),
});
const reserve = createServer();
reserve.listen(0, "127.0.0.1");
await once(reserve, "listening");
const hubPort = reserve.address().port;
await new Promise((resolve) => reserve.close(resolve));
const backend = spawn(binary, {
  env: {
    ...fixtureEnv,
    TAILTERM_STATE: state,
    TAILTERM_DEV_LISTEN: `127.0.0.1:${hubPort}`,
    TAILTERM_TCP_LISTEN: "",
  },
  stdio: "ignore",
});

const hub = `http://127.0.0.1:${hubPort}`;
async function api(method, route, body) {
  const response = await fetch(hub + route, {
    method,
    headers: body === undefined ? undefined : { "content-type": "application/json" },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const text = await response.text();
  const data = text ? JSON.parse(text) : null;
  if (!response.ok) {
    const error = new Error(data?.error || response.statusText);
    error.status = response.status;
    throw error;
  }
  return data;
}
for (let i = 0; i < 50; i++) {
  try {
    await api("GET", "/v1/tasks");
    break;
  } catch {
    await new Promise((resolve) => setTimeout(resolve, 100));
  }
}

async function project(name, orchestrator, hostName) {
  const task = await api("POST", "/v1/tasks", { name, orchestrator });
  const agent = await api("POST", `/v1/tasks/${task.id}/agents`, {
    name: orchestrator,
    host: hostName,
    session: `${orchestrator}-session`,
    runtime: "codex",
    cwd: root,
  });
  return { task, agent };
}

async function fixture(label) {
  const source = await project(`${label} Source project`, "source-lead", "host-a");
  const target = await project(`${label} Target project`, "target-lead", "host-a");
  const other = await project(`${label} Other project`, "other-lead", "host-a");
  const missing = {
    task: await api("POST", "/v1/tasks", {
      name: `${label} Missing orchestrator project`,
      orchestrator: "missing-lead",
    }),
  };
  const exited = await project(`${label} Exited orchestrator project`, "exited-lead", "host-a");
  await api("PATCH", `/v1/tasks/${exited.task.id}/agents/${exited.agent.id}`, {
    status: "exited",
  });
  const legacy = await project(`${label} Legacy handler project`, "lead-a", "host-a");
  const legacyHandlerID =
    label === "chromium" ? "agt_1111111111111111" : "agt_2222222222222222";
  await api("POST", `/v1/tasks/${legacy.task.id}/agents`, {
    agentId: legacyHandlerID,
    name: "db-handler",
    host: "host-b",
    session: `legacy-${label}`,
    runtime: "claude",
    cwd: "/legacy/b",
    role: "database_handler",
  });
  await api("PATCH", `/v1/tasks/${legacy.task.id}/agents/${legacyHandlerID}`, {
    status: "exited",
  });
  return { source, target, other, missing, exited, legacy, legacyHandlerID };
}

let dropNextCreate = false, dropNextUpdate = false;
const html = `<!doctype html><html><head><meta charset="utf-8"><link rel="stylesheet" href="/client/style.css"><link rel="stylesheet" href="/client/work-items.css"></head><body><div id="app"><div id="workspace"><main><header></header></main></div><dialog id="dialog"></dialog><p id="notice"></p></div><script type="module">
import {createHubClient} from '/client/hub-client.js';
import {createWorkItemsView} from '/client/work-items-view.js';
import {createTaskHub} from '/client/task-hub.js';
import {setupModes} from '/client/modes.js';
const data={hub:{url:location.origin},projectHandlerPlans:[]};
const servers=[
  {id:'a',name:'Host A',host:'host-a',username:'fixture',runtimes:['codex','claude']},
  {id:'b',name:'Host B',host:'host-b',username:'fixture',runtimes:['claude','codex']}
];
const dialog=(title,body)=>{const d=document.querySelector('#dialog');if(d.open)d.close();d.innerHTML='<div class="dialog-head"><h2>'+title+'</h2><button id="dialog-close" type="button">×</button></div>'+body;d.querySelector('#dialog-close').onclick=()=>host.closeDialog();d.showModal()};
const client=createHubClient({baseURL:location.origin,fetchImpl:(url,init)=>fetch(url,init)});
const host={
  getIPN:()=>({fetch:(url,init)=>fetch(url,init)}),getData:()=>data,getServers:()=>servers,currentServer:()=>servers[0],currentTab:()=>null,getTabs:()=>[],paneGroups:()=>({model:{groups:[]},sync(){}}),render(){},scheduleWorkspaceSave(){},bookmark(){},closeTab(){},connect:async()=>null,
  notice:text=>document.querySelector('#notice').textContent=text,
  dialog,closeDialog:()=>{const d=document.querySelector('#dialog');d.close();d.replaceChildren()},
  api:async(url,method,body)=>{if(url==='/project-handler-plans'&&method==='POST')data.projectHandlerPlans=[...data.projectHandlerPlans.filter(p=>p.hub!==body.hub||p.taskId!==body.taskId),structuredClone(body)];return {sessions:[]}},reloadData:async()=>{},
  openBoard:id=>window.qa.lastBoard=id,
  browserCommand:async(server,command)=>{window.qa.commands.push({server:server.id,command});if(window.qa.failLaunch)throw Error('Synthetic response loss after launch');return JSON.stringify({id:'agt_abcdef0123456789',host:server.host})}
};
const taskHub=createTaskHub(host);taskHub.refresh();
const bugs=createWorkItemsView({kind:'bug',client:()=>client,dialog,closeDialog:host.closeDialog,notice:host.notice,configure(){},openBoard:host.openBoard});
const features=createWorkItemsView({kind:'feature',client:()=>client,dialog,closeDialog:host.closeDialog,notice:host.notice,configure(){},openBoard:host.openBoard});
const modes=setupModes({header:document.querySelector('header'),main:document.querySelector('main'),onChange:(mode,view)=>{bugs.hide();features.hide();view.replaceChildren();if(mode==='bugs'){bugs.mount(view);bugs.show()}if(mode==='features'){features.mount(view);features.show()}}});
window.qa={client,bugs,features,modes,taskHub,data,commands:[],failLaunch:false,lastBoard:null};
</script></body></html>`;

const web = createServer(async (req, res) => {
  try {
    if (req.url === "/") {
      res.setHeader("content-type", "text/html");
      res.end(html);
      return;
    }
    if (req.url === "/qa/drop-next-create" && req.method === "POST") {
      dropNextCreate = true;
      res.end("ok");
      return;
    }
    if (req.url === "/qa/drop-next-update" && req.method === "POST") {
      dropNextUpdate = true;
      res.end("ok");
      return;
    }
    if (req.url.startsWith("/v1/")) {
      const chunks = [];
      for await (const chunk of req) chunks.push(chunk);
      const upstream = await fetch(hub + req.url, {
        method: req.method,
        headers: req.headers["content-type"] ? { "content-type": req.headers["content-type"] } : undefined,
        body: ["GET", "HEAD"].includes(req.method) ? undefined : Buffer.concat(chunks),
      });
      const body = await upstream.text();
      // Simulate a response disappearing only after the hub has committed it.
      // The retry must use the form's retained request ID, not create a second item.
      if (dropNextCreate && req.method === "POST" && /\/work-items$/.test(req.url)) {
        dropNextCreate = false;
        res.writeHead(503, { "content-type": "application/json" });
        res.end(JSON.stringify({ error: "Synthetic response loss" }));
        return;
      }
      if (dropNextUpdate && req.method === "POST" && /\/work-items\/wi_[a-f0-9]+\/updates$/.test(req.url)) {
        dropNextUpdate = false;
        res.writeHead(503, { "content-type": "application/json" });
        res.end(JSON.stringify({ error: "Synthetic update response loss" }));
        return;
      }
      res.writeHead(upstream.status, { "content-type": "application/json" });
      res.end(body);
      return;
    }
    const file = path.resolve(root, "." + decodeURIComponent(req.url));
    if (!file.startsWith(root + path.sep)) throw new Error("invalid static path");
    res.setHeader("content-type", file.endsWith(".js") ? "text/javascript" : file.endsWith(".css") ? "text/css" : "application/octet-stream");
    res.end(await readFile(file));
  } catch (error) {
    res.writeHead(500, { "content-type": "application/json" });
    res.end(JSON.stringify({ error: error.message }));
  }
});
web.listen(0, "127.0.0.1");
await once(web, "listening");
const origin = `http://127.0.0.1:${web.address().port}`;

async function item(taskID, id) {
  return api("GET", `/v1/tasks/${taskID}/work-items/${id}`);
}
async function allItems(kind = "bug") {
  return (await api("GET", `/v1/work-items?kind=${kind}`)).items;
}
async function assertRowInModeViewport(page, label) {
  const geometry = await page.evaluate(() => {
    const viewport = document.querySelector("#mode-view")?.getBoundingClientRect();
    const row = document.querySelector(".work-item")?.getBoundingClientRect();
    return viewport && row
      ? { viewport: { top: viewport.top, bottom: viewport.bottom }, row: { top: row.top, bottom: row.bottom, height: row.height } }
      : null;
  });
  assert.ok(geometry?.row.height > 0, `${label} row has no height`);
  assert.ok(
    geometry.row.top >= geometry.viewport.top && geometry.row.bottom <= geometry.viewport.bottom,
    `${label} row is clipped outside the mode viewport`,
  );
}
async function assertControlsInModeViewport(page, label) {
  const geometry = await page.evaluate(() => {
    const viewport = document.querySelector("#mode-view")?.getBoundingClientRect();
    const controls = ["[data-items-project]", "[data-items-status]", "[data-items-new]"]
      .map((selector) => document.querySelector(selector)?.getBoundingClientRect())
      .filter(Boolean);
    return viewport && controls.length === 3
      ? { viewport: { left: viewport.left, top: viewport.top, right: viewport.right, bottom: viewport.bottom }, controls: controls.map(({ left, top, right, bottom, width, height }) => ({ left, top, right, bottom, width, height })) }
      : null;
  });
  assert.equal(geometry?.controls.length, 3, `${label} controls are missing`);
  for (const control of geometry.controls) {
    assert.ok(control.width > 0 && control.height > 0, `${label} control has no size`);
    assert.ok(
      control.left >= geometry.viewport.left && control.right <= geometry.viewport.right && control.top >= geometry.viewport.top && control.bottom <= geometry.viewport.bottom,
      `${label} control is clipped outside the mode viewport`,
    );
  }
}

try {
  for (const [name, engine] of [["chromium", chromium], ["webkit", webkit]]) {
    const { source, target, other, missing, exited, legacy, legacyHandlerID } = await fixture(name);
    const browser = await engine.launch();
    try {
      const page = await browser.newPage({ viewport: { width: 390, height: 720 } });
      const errors = [];
      page.on("pageerror", (error) => errors.push(error.message));
      await page.goto(origin);
      await page.waitForFunction(() => !!window.qa);
      assert.equal(await page.locator('[data-mode="tasks"]').textContent(), "Projects");
      await page.locator('[data-mode="bugs"]').click();
      await page.locator('[data-items-new]').waitFor();
      assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), "Bugs header overflows a narrow viewport");

      // A project is mandatory even if a malformed/cleared selector reaches the
      // submit handler. This is the client-side guard before a hub write occurs.
      await page.locator('[data-items-new]').click();
      await page.locator('#work-item-title').fill(`${name} no-project bug`);
      await page.evaluate(() => (document.querySelector('#work-item-project').value = ''));
      await page.locator('#work-item-form button[type=submit]').click();
      await page.locator('#work-item-error').filter({ hasText: "Choose a project" }).waitFor();
      await page.locator('#dialog-close').click();

      // A create whose HTTP response is lost must replay one durable record.
      await fetch(origin + "/qa/drop-next-create", { method: "POST" });
      await page.locator('[data-items-new]').click();
      await page.locator('#work-item-project').selectOption(source.task.id);
      await page.locator('#work-item-title').fill(`${name} response-loss bug`);
      await page.locator('#work-item-description').fill("Created once even when the response disappears.");
      await page.locator('#work-item-form button[type=submit]').click();
      await page.locator('#work-item-error').filter({ hasText: "Synthetic response loss" }).waitFor();
      await page.locator('#work-item-form button[type=submit]').click();
      await page.locator('#dialog').waitFor({ state: "hidden" });
      const created = (await allItems()).filter((entry) => entry.title === `${name} response-loss bug`);
      assert.equal(created.length, 1, "response-loss retry duplicated a bug");
      const createdID = created[0].id;
      await page.locator(`[data-work-item="${createdID}"]`).waitFor();

      // Scope, update, reload persistence, and stale CAS rejection.
      const targetBug = await api("POST", `/v1/tasks/${target.task.id}/work-items`, { kind: "bug", title: `${name} target bug`, requestId: `${name}-target-bug` });
      await page.evaluate(() => qa.bugs.reload());
      await page.locator(`[data-work-item="${createdID}"]`).waitFor();
      assert.ok(await page.locator(`[data-work-item="${targetBug.id}"]`).count(), "all-project scope omitted a record");
      await page.locator('[data-items-project]').selectOption(source.task.id);
      await page.locator(`[data-work-item="${createdID}"]`).waitFor();
      await page.locator(`[data-work-item="${targetBug.id}"]`).waitFor({ state: "detached" });
      assert.equal(await page.locator('[data-work-item]').count(), 1, "project scope leaked another project's record");
      await page.locator(`[data-item-edit="${createdID}"]`).click();
      await page.locator('#work-item-title').fill(`${name} edited bug`);
      const beforeStale = await item(source.task.id, createdID);
      await api("PATCH", `/v1/tasks/${source.task.id}/work-items/${createdID}`, { revision: beforeStale.revision, description: "Changed by another writer." });
      await page.locator('#work-item-form button[type=submit]').click();
      await page.locator('#work-item-error').filter({ hasText: "Reopen this item" }).waitFor();
      assert.equal(await page.locator('#work-item-title').inputValue(), `${name} edited bug`, "stale edit text was discarded");
      await page.locator('#dialog-close').click();
      await page.evaluate(() => qa.bugs.reload());
      await page.locator(`[data-item-edit="${createdID}"]`).click();
      await page.locator('#work-item-title').fill(`${name} persisted edit`);
      await page.locator('#work-item-status').selectOption("in_progress");
      await page.locator('#work-item-priority').selectOption("high");
      await fetch(origin + "/qa/drop-next-update", { method: "POST" });
      await page.locator('#work-item-form button[type=submit]').click();
      await page.locator('#work-item-error').filter({ hasText: "Synthetic update response loss" }).waitFor();
      assert.equal(await page.locator('#work-item-title').inputValue(), `${name} persisted edit`, "lost update response discarded the draft");
      await page.locator('#work-item-form button[type=submit]').click();
      await page.locator('#dialog').waitFor({ state: "hidden" });
      const edited = await item(source.task.id, createdID);
      assert.equal(edited.title, `${name} persisted edit`);
      assert.equal(edited.status, "in_progress");
      assert.equal(edited.priority, "high");
      await page.evaluate(() => qa.bugs.reload());
      await page.locator(`[data-work-item="${createdID}"]`).filter({ hasText: "In progress" }).waitFor();
      await page.locator(`[data-item-history="${createdID}"]`).click();
      await page.locator('[data-history-revision="3"]').waitFor();
      assert.equal(await page.locator('[data-history-revision]').count(), 3, "history did not retain all exact revisions");
      await page.locator('[data-history-revision="1"]').click();
      await page.locator('[data-history-detail]').filter({ hasText: "Created once even when the response disappears." }).waitFor();
      await page.locator('[data-history-revision="2"]').click();
      await page.locator('[data-history-detail]').filter({ hasText: "Changed by another writer." }).waitFor();
      await page.locator('[data-history-revision="3"]').click();
      await page.locator('[data-history-detail]').filter({ hasText: `${name} persisted edit` }).waitFor();
      await page.locator('#dialog-close').click();
      await page.locator('[data-items-project]').waitFor();
      await page.waitForTimeout(150);
      await page.evaluate(() => {
        window.scrollTo(0, 0);
        document.querySelector('#mode-view').scrollTop = 0;
      });
      const bugsBounds = await page.locator('.work-items-head').boundingBox();
      assert.ok(
        bugsBounds && bugsBounds.y >= 0 && bugsBounds.y + bugsBounds.height <= 720,
        "populated Bugs controls are vertically clipped at 390px",
      );
      await assertControlsInModeViewport(page, "Bugs");
      await assertRowInModeViewport(page, "Bugs");
      await page.screenshot({ path: `.build/project-work-items-bugs-${name}.png` });
      await page.locator(`[data-item-edit="${createdID}"]`).click();
      await page.screenshot({ path: `.build/project-work-item-editor-${name}.png` });
      await page.locator('#dialog-close').click();

      // A feature is its own kind and survives a mode switch/reload.
      await page.locator('[data-mode="features"]').click();
      await page.locator('[data-items-new]').waitFor();
      await page.locator('[data-items-new]').click();
      await page.locator('#work-item-project').selectOption(source.task.id);
      await page.locator('#work-item-title').fill(`${name} feature`);
      await page.locator('#work-item-form button[type=submit]').click();
      await page.locator('#dialog').waitFor({ state: "hidden" });
      const feature = (await allItems("feature")).find((entry) => entry.title === `${name} feature`);
      assert.ok(feature, "feature was not persisted as a feature");
      await page.reload();
      await page.locator('[data-mode="features"]').click();
      await page.locator(`[data-work-item="${feature.id}"]`).waitFor();
      await page.locator(`[data-item-edit="${feature.id}"]`).click();
      await page.locator('#work-item-description').fill(`${name} feature revision two`);
      await page.locator('#work-item-form button[type=submit]').click();
      await page.locator('#dialog').waitFor({ state: "hidden" });
      await page.locator(`[data-item-history="${feature.id}"]`).click();
      assert.equal(await page.locator('[data-history-revision]').count(), 2, "feature history did not retain both revisions");
      await page.locator('[data-history-revision="2"]').click();
      await page.locator('[data-history-detail]').filter({ hasText: `${name} feature revision two` }).waitFor();
      await page.locator('#dialog-close').click();
      await page.evaluate(() => {
        window.scrollTo(0, 0);
        document.querySelector('#mode-view').scrollTop = 0;
      });
      await assertControlsInModeViewport(page, "Features");
      await assertRowInModeViewport(page, "Features");
      await page.screenshot({ path: `.build/project-work-items-features-${name}.png` });

      // Dispatch defaults to ownership, allows an explicit open target, never moves
      // ownership, and the hub's request receipt deduplicates a replay.
      await page.locator('[data-mode="bugs"]').click();
      await page.locator('[data-items-project]').selectOption(source.task.id);
      await page.locator(`[data-item-send="${createdID}"]`).click();
      assert.equal(await page.locator('#work-item-target').inputValue(), source.task.id, "dispatch did not default to the owning project");
      await page.locator('#work-item-dispatch button[type=submit]').click();
      await page.locator('#dialog').waitFor({ state: "hidden" });
      await page.locator('#notice').filter({ hasText: "board message #" }).waitFor();
      assert.equal((await item(source.task.id, createdID)).lastDispatch.targetTaskId, source.task.id);
      await page.locator(`[data-item-send="${createdID}"]`).click();
      await page.locator('#work-item-target').selectOption(target.task.id);
      await page.locator('#work-item-dispatch button[type=submit]').click();
      await page.locator('#dialog').waitFor({ state: "hidden" });
      await page.locator('#notice').filter({ hasText: "board message #" }).waitFor();
      const cross = await item(source.task.id, createdID);
      assert.equal(cross.taskId, source.task.id, "cross-project dispatch moved item ownership");
      assert.equal(cross.lastDispatch.targetTaskId, target.task.id);
      const replayKey = `${name}-dispatch-replay`;
      const replayBody = { revision: cross.revision, targetTaskId: target.task.id, requestId: replayKey };
      const first = await api("POST", `/v1/tasks/${source.task.id}/work-items/${createdID}/dispatch`, replayBody);
      const second = await api("POST", `/v1/tasks/${source.task.id}/work-items/${createdID}/dispatch`, replayBody);
      assert.equal(first.dispatch.messageSeq, second.dispatch.messageSeq, "dispatch response-loss replay created a second message");

      // Targets that name a missing or exited orchestrator are actionable errors:
      // no board message/dispatch receipt may be represented as a success.
      for (const [badTarget, expected] of [
        [missing.task, "has no open agent"],
        [exited.task, "has no open agent"],
      ]) {
        if (await page.locator('#dialog').isVisible()) await page.locator('#dialog-close').click();
        await page.locator(`[data-item-send="${createdID}"]`).click();
        await page.locator('#work-item-target').selectOption(badTarget.id);
        await page.locator('#work-item-dispatch button[type=submit]').click();
        const status = page.locator('#work-item-dispatch-status');
        await status.filter({ hasText: expected }).waitFor();
        assert.doesNotMatch(await status.innerText(), /message #/, "failed dispatch displayed a success receipt");
        const afterFailure = await item(source.task.id, createdID);
        assert.equal(afterFailure.lastDispatch.targetTaskId, target.task.id, "failed target dispatch changed the durable receipt");
      }

      // Closed owners become read-only in both the row and editor.
      await api("DELETE", `/v1/tasks/${source.task.id}`);
      await page.locator('#dialog-close').click();
      await page.evaluate(() => qa.bugs.reload());
      // A subscription refresh may already be running; wait for the closed
      // state to render instead of accepting the still-visible previous row.
      await page.locator(`[data-item-send="${createdID}"]:disabled`).waitFor();
      assert.equal(await page.locator(`[data-item-send="${createdID}"]`).isDisabled(), true);
      await page.locator(`[data-item-edit="${createdID}"]`).click();
      assert.equal(await page.locator('#work-item-title').isDisabled(), true);
      await page.locator('#work-item-error').filter({ hasText: "read-only" }).waitFor();
      await page.locator('#dialog-close').click();
      await page.locator(`[data-item-history="${createdID}"]`).click();
      await page.locator('[data-history-revision="3"]').waitFor();
      await page.locator('[data-history-detail]').filter({ hasText: `${name} persisted edit` }).waitFor();

      // An exited legacy handler without local settings uses that handler's host,
      // runtime, and folder rather than silently falling back to the lead.
      await page.locator('#dialog-close').click();
      await page.evaluate((taskID) => qa.taskHub.setupHandler(taskID), legacy.task.id);
      await page.locator('#project-handler-form').waitFor();
      assert.equal(await page.locator('#task-server').inputValue(), "b");
      assert.equal(await page.locator('#agent-runtime').inputValue(), "claude");
      assert.equal(await page.locator('#agent-cwd').inputValue(), "/legacy/b");

      // Seed an older local setting, stage a failed edit, recover it, then retry the
      // same dialog. The stable handler ID and receipt cleanup prevent duplicates.
      await page.evaluate(({ taskID, handlerID }) => {
        qa.data.projectHandlerPlans = [{hub:location.origin,taskId:taskID,serverId:'b',fields:{name:'db-handler',run:'claude',cwd:'/legacy/b',runtime:'claude',model:'',permissionMode:'',prompt:'',allowedTools:[],agentRole:'database_handler',agentId:handlerID}}];
        qa.modes.set('terminals', {silent:true});
      }, { taskID: legacy.task.id, handlerID: legacyHandlerID });
      await page.locator('#dialog-close').click();
      await page.evaluate((taskID) => qa.taskHub.setupHandler(taskID), legacy.task.id);
      await page.locator('#project-handler-form').waitFor();
      await page.locator('#task-server').selectOption("a");
      await page.locator('#agent-runtime').selectOption("codex");
      await page.locator('#agent-cwd').fill(root);
      await page.evaluate(() => (qa.failLaunch = true));
      await page.locator('#handler-start').click();
      await page.locator('#task-error').filter({ hasText: "Previous settings are retained" }).waitFor();
      assert.ok(await page.locator('#handler-restore-settings').count() === 0, "restore belongs to the next setup dialog");
      await page.locator('#dialog-close').click();
      await page.evaluate((taskID) => qa.taskHub.setupHandler(taskID), legacy.task.id);
      await page.locator('#handler-restore-settings').click();
      await page.locator('#project-handler-form').waitFor();
      await page.waitForFunction(() => document.querySelector('#task-server')?.value === 'b');
      assert.equal(await page.locator('#task-server').inputValue(), "b");
      assert.equal(await page.locator('#agent-cwd').inputValue(), "/legacy/b");
      await page.locator('#task-server').selectOption("a");
      await page.locator('#agent-runtime').selectOption("codex");
      await page.locator('#agent-cwd').fill(root);
      await page.evaluate(() => (qa.failLaunch = true));
      await page.locator('#handler-start').click();
      await page.locator('#task-error').filter({ hasText: "Synthetic response loss" }).waitFor();
      await page.evaluate(() => (qa.failLaunch = false));
      await page.locator('#handler-start').click();
      await page.locator('#dialog').waitFor({ state: "hidden" });
      const commands = await page.evaluate(() => qa.commands.filter((entry) => entry.server === 'a').map((entry) => entry.command));
      assert.ok(commands.length >= 3, "handler retry did not issue launch commands");
      assert.equal(commands.at(-1), commands.at(-2), "same-dialog handler retry changed its launch identity");
      assert.match(commands.at(-1), new RegExp(legacyHandlerID));
      assert.equal(await page.evaluate(() => qa.data.projectHandlerPlans[0].previous), undefined, "successful handler start did not clear recovered previous settings");
      assert.deepEqual(errors, [], `${name} page errors`);
      console.log(`${name}: project work-item CRUD, scopes, retries, dispatch, closed state, and handler recovery passed.`);
    } finally {
      await browser.close();
    }
  }
} finally {
  await new Promise((resolve) => web.close(resolve));
  backend.kill("SIGTERM");
  await rm(state, { recursive: true, force: true });
}
