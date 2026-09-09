// Independent QA: wi_b1d07b59cdf519eb-fix-1 (#661), assignment #663.
// Only throwaway hub, encrypted vault and browser state. Native continuity uses
// actual pointer/keyboard input and never selectOption(); the status regression
// separately labels its synthetic input-only event sequence.
import { chromium, webkit, expect } from "@playwright/test";
import { createServer } from "node:http";
import { once } from "node:events";
import { execFile, spawn } from "node:child_process";
import { promisify } from "node:util";
import { mkdtemp, mkdir, readFile, writeFile, rm } from "node:fs/promises";
import { createHash } from "node:crypto";
import { tmpdir } from "node:os";
import path from "node:path";
import assert from "node:assert/strict";

const exec = promisify(execFile), root = process.cwd();
const source = path.resolve(process.env.DROPDOWN_SOURCE || root);
const label = process.env.DROPDOWN_LABEL || "candidate";
assert.match(label, /^[a-z0-9_-]+$/i);
const caseFilter = process.env.DROPDOWN_CASES ? new RegExp(process.env.DROPDOWN_CASES) : null;
const engines = (process.env.DROPDOWN_ENGINES || "chromium,webkit").split(",");
assert.ok(engines.length && engines.every(e => ["chromium", "webkit"].includes(e)));
const artifacts = path.join(root, ".build/dropdown-continuity", label);
await mkdir(artifacts, { recursive: true });
const fixtureSource = await readFile(new URL(import.meta.url));
const fixtureHash = createHash("sha256").update(fixtureSource).digest("hex");
await writeFile(path.join(artifacts, "fixture-source.mjs"), fixtureSource);
const stateDir = await mkdtemp(path.join(tmpdir(), "tailterm-dropdown-"));
const fixtureEnv = Object.fromEntries(Object.entries(process.env).filter(([key]) => !/^(TAILTERM_|CODEX_|TT_|TMUX)/.test(key) && key !== "HOME"));
const results = [], observations = [], screenshots = [], servedSources = {};
let backend, web, browser, page, hub, origin, data, revision = 0;
const pause = ms => new Promise(resolve => setTimeout(resolve, ms));
async function api(method, route, body, status = 200) {
  if (method !== "GET") await pause(60);
  const response = await fetch(hub + route, { method, signal: AbortSignal.timeout(10000),
    headers: body === undefined ? undefined : { "content-type": "application/json" },
    body: body === undefined ? undefined : JSON.stringify(body) });
  const text = await response.text(), value = text ? JSON.parse(text) : null;
  assert.equal(response.status, status, `${method} ${route}: ${text}`);
  return value;
}
async function project(name) {
  const task = await api("POST", "/v1/tasks", { name, goal: "Original synthetic goal", orchestrator: "runner-a" }, 201);
  const agents = [];
  for (const name of ["runner-a", "runner-b", "runner-c"]) agents.push(await api("POST", `/v1/tasks/${task.id}/agents`, {
    name, host: "synthetic.invalid", session: "unused-fixture", runtime: "codex", cwd: stateDir,
  }, 201));
  return { task, agents };
}
async function seed(name) {
  const alpha = await project(`${name} Alpha`), beta = await project(`${name} Beta`), closed = await project(`${name} Closed`);
  const items = {};
  for (const kind of ["bug", "feature"]) {
    items[kind] = await api("POST", `/v1/tasks/${alpha.task.id}/work-items`, {
      kind, title: `${name} Synthetic ${kind}`, description: "Isolated dropdown fixture", priority: "normal", requestId: `${name}-${kind}`,
    }, 201);
    await api("POST", `/v1/tasks/${beta.task.id}/work-items`, {
      kind, title: `${name} Other ${kind}`, priority: "high", requestId: `${name}-other-${kind}`,
    }, 201);
  }
  await api("DELETE", `/v1/tasks/${closed.task.id}`);
  const decision = await api("POST", `/v1/tasks/${alpha.task.id}/decisions`, {
    agentId: alpha.agents[0].id, requestId: `${name}-question`, question: "Which synthetic path?",
    options: [{ id: "one", label: "First path", description: "An isolated option." }, { id: "two", label: "Second path", description: "Another isolated option." }],
    recommendedOptionId: "one", recommendationReason: "It keeps this fixture small.",
  }, 201);
  await api("POST", `/v1/tasks/${alpha.task.id}/decisions/${decision.seq}/answer`, { requestId: `${name}-answer`, optionId: "one" }, 201);
  return { alpha, beta, closed, items, decision };
}

function html() { return `<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<link rel="stylesheet" href="/client/style.css"><link rel="stylesheet" href="/client/work-items.css"></head><body><div id="app"><div id="workspace"><aside><h2>Fixture machines</h2></aside><main><header></header><div class="terminal-shell" hidden></div></main></div><dialog id="dialog"></dialog><p id="notice" hidden></p></div>
<script type="module">
import {createBoardView} from '/client/board-view.js';
import {createTasksView} from '/client/tasks-view.js';
import {createWorkItemsView} from '/client/work-items-view.js';
import {createHubClient} from '/client/hub-client.js';
import {createCachedHubClient} from '/client/cached-hub-client.js';
import {createHubReadCache} from '/client/hub-read-cache.js';
import * as vault from '/client/local-vault.js';
import {setupModes} from '/client/modes.js';
const data=${JSON.stringify(data)}, state={online:true,reads:[],actions:[],notices:[]}, pending=new Set();
await vault.localAPI('/unlock','POST',{username:'synthetic-dropdown',password:'isolated dropdown fixture password'});
const live=createHubClient({baseURL:location.origin,fetchImpl:(url,init)=>{
  if(!state.online)return Promise.reject(Error('Synthetic hub offline'));
  if(new URL(url).pathname.endsWith('/events'))return fetch(url,init);
  let work;work=fetch(url,init).then(async response=>{const text=await response.text();state.reads.push({url,text,method:init.method});if(state.reads.length>80)state.reads.shift();return {status:response.status,statusText:response.statusText,headers:response.headers,text:async()=>text}}).finally(()=>pending.delete(work));pending.add(work);return work;
}});
const client=createCachedHubClient({client:live,cache:createHubReadCache(vault.hubReadCachePersistence()),online:()=>state.online,refreshMs:1000});
const noop=()=>{}, notice=text=>{state.notices.push(text);document.querySelector('#notice').textContent=text};
const closeDialog=()=>{const d=document.querySelector('#dialog');d.close();d.replaceChildren()};
const dialog=(title,body)=>{const d=document.querySelector('#dialog');if(d.open)d.close();d.innerHTML='<div class="dialog-head"><h2>'+title+'</h2><button id="dialog-close" type="button">Close</button></div>'+body;d.querySelector('#dialog-close').onclick=closeDialog;d.showModal()};
const common={client:()=>client,getTabs:()=>[],activate:noop,notice,configure:noop,openBoard:noop};
const taskHub={groupOf:()=>null,newTask:noop,addAgent:noop,setupHandler:noop,attachToCurrent:noop,revealAgent:noop,settings:id=>state.actions.push({action:'settings',id})};
const views={board:createBoardView({...common,addAgent:noop,revealAgent:noop,attachTask:noop,newTask:noop}),tasks:createTasksView({...common,taskHub,confirm:async()=>false}),bugs:createWorkItemsView({...common,kind:'bug',dialog,closeDialog}),features:createWorkItemsView({...common,kind:'feature',dialog,closeDialog})};
let current;
const modes=setupModes({header:document.querySelector('header'),main:document.querySelector('main'),onChange:(mode,view)=>{if(current)views[current]?.hide();view.replaceChildren();current=mode;if(views[mode]){views[mode].mount(view);void views[mode].show(mode==='tasks'?undefined:data.alpha.task.id)}}});
window.qa={data,state,views,modes,client,vault,
  async show(mode){modes.set(mode);if(views[mode])await views[mode].show(mode==='tasks'?undefined:data.alpha.task.id)},
  async showLikeMain(mode){for(const view of Object.values(views))view.hide();modes.set(mode);if(views[mode])await views[mode].show(mode==='tasks'?undefined:data.alpha.task.id)},
  async refresh(){await client.refreshConnection();await views[current]?.reload()},
  async connection(value){state.online=value;await client.refreshConnection();await views[current]?.reload()},
  async stop(){for(const view of Object.values(views))view.hide();client.dispose();await Promise.allSettled([...pending])},
};
modes.set(new URLSearchParams(location.search).get('mode')||'board');
</script></body></html>`; }

async function serve(req, res) {
  try {
    const url = new URL(req.url, origin || "http://127.0.0.1");
    if (url.pathname === "/") { res.writeHead(200, { "content-type": "text/html" }); res.end(html()); return; }
    if (url.pathname.startsWith("/v1/")) {
      const chunks = []; for await (const chunk of req) chunks.push(chunk);
      const response = await fetch(hub + req.url, { method: req.method, signal: AbortSignal.timeout(35000),
        headers: req.headers["content-type"] ? { "content-type": req.headers["content-type"] } : undefined,
        body: ["GET", "HEAD"].includes(req.method) ? undefined : Buffer.concat(chunks) });
      const text = await response.text(); res.writeHead(response.status, { "content-type": "application/json" }); res.end(text); return;
    }
    const file = path.resolve(source, "." + decodeURIComponent(url.pathname));
    if (!file.startsWith(source + path.sep)) throw Error("Invalid static path");
    if (url.searchParams.has("url")) { res.writeHead(200, { "content-type": "text/javascript" }); res.end(`export default ${JSON.stringify(url.pathname)};`); return; }
    const contents = await readFile(file), hash = createHash("sha256").update(contents).digest("hex");
    if (servedSources[url.pathname] && servedSources[url.pathname] !== hash) throw Error(`Source changed during acceptance: ${url.pathname}`);
    servedSources[url.pathname] = hash;
    res.writeHead(200, { "cache-control": "no-store", "content-type": file.endsWith(".js") ? "text/javascript" : file.endsWith(".css") ? "text/css" : "application/octet-stream" });
    res.end(contents);
  } catch (error) { if (!res.destroyed) { res.writeHead(500, { "content-type": "application/json" }); res.end(JSON.stringify({ error: error.message })); } }
}
async function open(mode, width = 1440) {
  if (page) { await page.evaluate(async () => { if (window.qa) await qa.stop(); }).catch(() => {}); await page.close(); }
  page = await browser.newPage({ viewport: { width, height: width === 390 ? 844 : 1000 } });
  await page.route("**/*", route => new URL(route.request().url()).origin === origin ? route.continue() : route.abort());
  page.setDefaultTimeout(6000);
  await page.goto(`${origin}/?mode=${mode}`);
  await page.waitForFunction(() => !!window.qa);
  await expect(page.locator(".board")).toBeVisible();
  if (mode === "tasks") {
    await page.locator(`[data-task-select="${data.alpha.task.id}"]`).click();
    await expect(page.locator(".task-detail-card")).toHaveAttribute("data-task-card", data.alpha.task.id);
  }
}
async function refresh(name) {
  const marker = `Synthetic refresh ${++revision}: ${name}`;
  await api("PATCH", `/v1/tasks/${data.alpha.task.id}`, { goal: marker });
  await page.evaluate(() => qa.refresh());
  await page.waitForFunction(marker => qa.state.reads.some(read => read.method === "GET" && read.text.includes(marker)), marker);
  return marker;
}
async function shot(name) {
  const file = name.replace(/[^a-z0-9_-]/gi, "-") + ".png";
  await page.screenshot({ path: path.join(artifacts, file) }); screenshots.push(file);
}
async function check(name, fn) {
  if (caseFilter && !caseFilter.test(name)) return;
  const result = { name, pass: false }; results.push(result);
  try { await fn(); result.pass = true; console.log(`PASS ${name}`); }
  catch (error) {
    result.error = error.stack; console.error(`FAIL ${name}: ${error.stack}`);
    if (page && !page.isClosed()) await shot(`failure-${name}`).catch(() => {});
  }
}
async function nativeState(selector) {
  return page.evaluate(selector => {
    const el = document.querySelector(selector);
    let open = null; try { open = el.matches(":open"); } catch {}
    return { open, focused: document.activeElement === el, same: qa.anchor === el && qa.anchor?.isConnected,
      value: el.value, index: el.selectedIndex, count: el.options.length };
  }, selector);
}
async function activateSelect(selector) {
  await page.locator(selector).click();
  await page.evaluate(selector => { qa.anchor = document.querySelector(selector); }, selector);
  return nativeState(selector);
}
async function nativeCase(engine, mode, selector, width = 1440) {
  await open(mode, width);
  if (mode === "board") await page.locator("#board-text").fill("Keep this unsent synthetic compose draft.");
  const initial = await activateSelect(selector);
  const observation = { engine, mode, selector, width, nativeOpenObserved: initial.open === true, initial };
  observations.push(observation);
  assert.ok(initial.focused, "physical native-select activation must focus the control");
  const marker = await refresh(`${engine} ${mode} native select`);
  const during = await nativeState(selector); observation.afterRefresh = during;
  assert.ok(during.same && during.focused, "refresh replaced or unfocused the active native select");
  if (initial.open === true) assert.equal(during.open, true, "observed native popup closed during refresh");
  await shot(`${engine}-${mode}-${selector}-open-${width}`);
  // macOS Playwright protocol keys do not reach OS-native menu layers in
  // either engine (also reproduced with headed browsers and a bare form).
  // Record Escape honestly; never use selectOption or submit the form with
  // Enter as a false substitute for committing a native menu selection.
  await page.keyboard.press("Escape");
  observation.afterEscapeProtocolKey = await nativeState(selector);
  observation.keyboardCommitAndDismissal = "unverified: native OS layer does not receive Playwright protocol keys";
  await page.locator(".board-head h2").click();
  observation.afterOutside = await nativeState(selector);
  if (initial.open === true) assert.equal(observation.afterOutside.open, false, "outside pointer dismissal must close the popup");
  await expect(page.locator(".board")).toContainText(marker);
  await expect(page.locator(".board")).toContainText(await refresh(`${engine} ${mode} after outside dismissal`));
  // Focus without opening a menu must never indefinitely hold future renders.
  await page.locator(selector).focus();
  observation.focusWithoutPopup = await nativeState(selector);
  await expect(page.locator(".board")).toContainText(await refresh(`${engine} ${mode} focused closed select`));
  await activateSelect(selector);
  await page.evaluate(() => qa.connection(false));
  await page.locator(".board-head h2").click();
  await expect(page.locator(".board")).toContainText(/offline/i);
  await page.evaluate(() => qa.connection(true));
  await expect(page.locator(".board")).toContainText(await refresh(`${engine} ${mode} after reconnect`));
  await activateSelect(selector);
  await page.evaluate(async mode => { qa.modes.set('terminals'); await qa.show(mode); }, mode);
  await expect(page.locator(selector)).toBeVisible();
  if (initial.open === true) assert.equal((await nativeState(selector)).open, false, "view hide/navigation must release the native menu");
  if (mode === "board") {
    await expect(page.locator("#board-text")).toHaveValue("Keep this unsent synthetic compose draft.");
    await page.evaluate(async () => { await qa.show('tasks'); await qa.show('board'); });
    await expect(page.locator("#board-text")).toHaveValue("Keep this unsent synthetic compose draft.");
  }
  if (width === 390) await expect(async () => {
    const r = await page.evaluate(selector => { const r = document.querySelector(selector).getBoundingClientRect(); return { left:r.left,right:r.right,width:r.width }; }, selector);
    assert.ok(r.width > 0 && r.left >= 0 && r.right <= 391, "native control must fit390px viewport");
  }).toPass({ timeout: 5000 });
}
async function inputOnlyStatusCommitCase(engine, mode) {
  await open(mode);
  const selector = "[data-items-status]";
  const before = await page.locator(".work-item").count();
  assert.ok(before > 0, "fixture must start with an unfiltered work item");
  await page.evaluate(selector => {
    const control = document.querySelector(selector);
    control.value = "done";
    control.dispatchEvent(new Event("input", { bubbles: true }));
  }, selector);
  await expect(page.locator(selector)).toHaveValue("done");
  await expect(page.locator(".work-items-empty")).toContainText(
    `No ${mode} match these filters.`,
  );
  assert.equal(
    await page.locator(".work-item").count(),
    0,
    "input-only commit must filter without change, blur, or outside click",
  );
  observations.push({
    engine,
    mode,
    selector,
    commitSequence: "synthetic input only; no change/blur/outside click",
    filteredImmediately: true,
  });
}
async function keyboardStatusCommitCase(engine, mode) {
  await open(mode);
  const selector = "[data-items-status]";
  await page.locator(selector).focus();
  await page.keyboard.press("i");
  await expect(page.locator(selector)).toHaveValue("in_progress");
  await expect(page.locator(".work-items-empty")).toContainText(
    `No ${mode} match these filters.`,
  );
  observations.push({
    engine,
    mode,
    selector,
    commitSequence: "Playwright engine keyboard typeahead from focused closed select",
    filteredImmediately: true,
  });
}

try {
  const binary = path.join(stateDir, "tailterm-hub");
  await exec("go", ["build", "-o", binary, "./cmd/tailterm-hub"], { cwd: path.join(source, "hub") });
  const reserve = createServer(); reserve.listen(0, "127.0.0.1"); await once(reserve, "listening");
  const port = reserve.address().port; await new Promise(resolve => reserve.close(resolve));
  hub = `http://127.0.0.1:${port}`;
  backend = spawn(binary, { env: { ...fixtureEnv, TAILTERM_STATE: stateDir, TAILTERM_DEV_LISTEN: `127.0.0.1:${port}`, TAILTERM_TCP_LISTEN: "" }, stdio: "ignore" });
  let ready = false;
  for (let i = 0; i < 100; i++) { try { await api("GET", "/v1/tasks"); ready = true; break; } catch { await pause(100); } }
  assert.ok(ready, "isolated hub failed to start");
  web = createServer(serve); web.listen(0, "127.0.0.1"); await once(web, "listening"); origin = `http://127.0.0.1:${web.address().port}`;
  for (const engine of [chromium, webkit].filter(engine => engines.includes(engine.name()))) {
    const name = engine.name(); data = await seed(name); browser = await engine.launch();
    for (const mode of ["board", "tasks", "bugs", "features"]) await check(`${name} ${mode} Closed disclosure continuity`, async () => {
      await open(mode);
      const details = page.locator(".board-closed"), summary = details.locator("summary");
      if (!(await details.evaluate(el => el.open))) await summary.click();
      await refresh(`${name} ${mode} open disclosure`);
      await expect.poll(() => details.evaluate(el => el.open)).toBe(true);
      const alternate = mode === "features" ? "bugs" : "features";
      await page.evaluate(mode => qa.show(mode), alternate);
      await expect.poll(() => page.locator(".board-closed").evaluate(el => el.open)).toBe(false);
      await page.evaluate(mode => qa.show(mode), mode);
      await expect.poll(() => details.evaluate(el => el.open)).toBe(true);
      await page.evaluate(async mode => { qa.modes.set('terminals'); await qa.show(mode); }, mode);
      await expect.poll(() => details.evaluate(el => el.open)).toBe(true);
      await summary.click(); await refresh(`${name} ${mode} closed disclosure`);
      await expect.poll(() => details.evaluate(el => el.open)).toBe(false);
      await summary.click(); await page.locator(`[data-board-task="${data.closed.task.id}"]`).click();
      await expect(page.locator(`[data-board-task="${data.closed.task.id}"]`)).toHaveAttribute("aria-pressed", "true");
      await summary.click(); await refresh(`${name} ${mode} explicitly collapsed selected closed project`);
      await expect.poll(() => details.evaluate(el => el.open)).toBe(false);
      await shot(`${name}-${mode}-closed-choice`);
    });
    await check(`${name} Projects30s status clock keeps Closed expanded`, async () => {
      await open("tasks"); await page.locator(".board-closed summary").click();
      await page.clock.install({ time: new Date(Date.now() + 95000) });
      await page.clock.fastForward(30001);
      await expect(page.locator(".task-detail-card")).toContainText("offline");
      await expect.poll(() => page.locator(".board-closed").evaluate(el => el.open)).toBe(true);
      await page.clock.resume();
    });
    for (const [mode, selector] of [["board", "#board-to"], ["bugs", "[data-items-project]"], ["bugs", "[data-items-status]"], ["features", "[data-items-project]"], ["features", "[data-items-status]"]])
      await check(`${name} ${mode} native ${selector} continuity`, () => nativeCase(name, mode, selector, selector === "[data-items-project]" ? 390 : 1440));
    for (const mode of ["bugs", "features"]) {
      await check(`${name} ${mode} input-only status commit filters before blur`, () => inputOnlyStatusCommitCase(name, mode));
      await check(`${name} ${mode} keyboard status selection filters immediately`, () => keyboardStatusCommitCase(name, mode));
    }
    await check(`${name} Board physical rail press survives changed-data refresh`, async () => {
      await open("board");
      try {
        await page.locator(`[data-board-task="${data.beta.task.id}"]`).hover();
        await page.mouse.down();
        const marker = await refresh(`${name} held Board rail pointer`);
        await page.mouse.up();
        await expect(page.locator(".board-head h2")).toHaveText(data.beta.task.name);
        await page.locator(`[data-board-task="${data.alpha.task.id}"]`).focus();
        await page.keyboard.press("Enter");
        await expect(page.locator(".board-head h2")).toHaveText(data.alpha.task.name);
        await expect(page.locator(".board")).toContainText(marker);
      } finally { await page.mouse.up(); }
    });
    await check(`${name} shared root popup navigation through all views remains live`, async () => {
      await open("board");
      for (const mode of ["board", "tasks", "bugs", "features", "board"]) {
        await page.evaluate(mode => qa.showLikeMain(mode), mode);
        if (mode === "tasks") {
          await page.locator(`[data-task-select="${data.alpha.task.id}"]`).click();
          await expect(page.locator(".task-detail-card")).toHaveAttribute("data-task-card", data.alpha.task.id);
        }
        await expect(page.locator(".board")).toContainText(await refresh(`${name} shared root ${mode}`));
        if (mode === "tasks") await page.locator(`[data-task-more="${data.alpha.task.id}"]`).click();
        else {
          const select = mode === "board" ? "#board-to" : "[data-items-status]";
          const opened = await activateSelect(select);
          assert.equal(opened.open, true);
          await refresh(`${name} shared root ${mode} held`);
          assert.ok((await nativeState(select)).same, "shared root listeners must preserve the current view control");
        }
      }
      await page.locator(".board-head h2").click();
      await expect(page.locator(".board")).toContainText(await refresh(`${name} shared root final dismissal`));
    });
    await check(`${name} removed selected recipient refreshes after native dismissal`, async () => {
      await open("board");
      const recipient = data.alpha.agents[2];
      const message = await api("POST", `/v1/tasks/${data.alpha.task.id}/messages`, {
        agentId: recipient.id, text: `${name} synthetic removable sender`,
      }, 201);
      await page.evaluate(() => qa.refresh());
      await page.locator(`[data-reply="${message.seq}"]`).click();
      await expect(page.locator("#board-to")).toHaveValue(recipient.id);
      await page.locator("#board-text").fill("Keep removed-recipient draft unsent.");
      const opened = await activateSelect("#board-to");
      await api("DELETE", `/v1/tasks/${data.alpha.task.id}/agents/${recipient.id}`);
      await refresh(`${name} recipient removed while menu held`);
      assert.ok((await nativeState("#board-to")).same);
      if (opened.open) assert.equal((await nativeState("#board-to")).open, true);
      await page.locator(".board-head h2").click();
      await expect(page.locator(`#board-to option[value="${recipient.id}"]`)).toHaveCount(0);
      await expect(page.locator("#board-to")).toHaveValue("");
      await expect(page.locator("#board-text")).toHaveValue("Keep removed-recipient draft unsent.");
      await expect(page.locator(".board")).toContainText(await refresh(`${name} after recipient removal dismissal`));
    });
    await check(`${name} Projects More popover continuity and dismissal`, async () => {
      await open("tasks"); const button = page.locator(`[data-task-more="${data.alpha.task.id}"]`), menu = page.locator(`#task-menu-${data.alpha.task.id}`);
      await button.click(); const marker = await refresh(`${name} More popover`);
      await expect.poll(() => menu.evaluate(el => el.matches(":popover-open"))).toBe(true);
      await expect(button).toHaveAttribute("aria-expanded", "true");
      await shot(`${name}-projects-more-open`);
      await page.keyboard.press("Escape"); await expect.poll(() => menu.evaluate(el => el.matches(":popover-open"))).toBe(false);
      await expect(button).toHaveAttribute("aria-expanded", "false");
      await expect(page.locator(".board")).toContainText(marker);
      await button.click(); await page.locator(".board-head h2").click(); await expect.poll(() => menu.evaluate(el => el.matches(":popover-open"))).toBe(false);
      await expect(page.locator(".board")).toContainText(await refresh(`${name} after More outside dismissal`));
      await button.click(); await menu.locator("[data-task-settings]").click();
      await expect.poll(() => page.evaluate(() => qa.state.actions.length)).toBe(1);
      await expect.poll(() => menu.evaluate(el => el.matches(":popover-open"))).toBe(false);
      await expect(button).toHaveAttribute("aria-expanded", "false");
    });
    await check(`${name} Board answered disclosure retains manual choice`, async () => {
      await open("board"); const details = page.locator(".decision-history");
      await details.locator("summary").click(); await refresh(`${name} answered history open`);
      await expect.poll(() => details.evaluate(el => el.open)).toBe(true);
      await details.locator("summary").click(); await refresh(`${name} answered history closed`);
      await expect.poll(() => details.evaluate(el => el.open)).toBe(false);
    });
    for (const mode of ["bugs", "features"]) await check(`${name} ${mode} edit priority dropdown and draft continuity`, async () => {
      await open(mode); await page.locator(`[data-item-edit="${data.items[mode === "bugs" ? "bug" : "feature"].id}"]`).click();
      await page.locator("#work-item-title").fill("Retained unsaved synthetic title");
      const initial = await activateSelect("#work-item-priority"); await refresh(`${name} edit priority`);
      const after = await nativeState("#work-item-priority");
      observations.push({ engine:name, mode, selector:"#work-item-priority", nativeOpenObserved:initial.open===true, initial, afterRefresh:after });
      assert.ok(after.same && after.focused, "background refresh must retain the dialog native control");
      if (initial.open === true) assert.equal(after.open, true);
      await page.locator("#dialog .dialog-head h2").click();
      await expect(page.locator("#work-item-priority")).toHaveValue("normal");
      await expect(page.locator("#work-item-title")).toHaveValue("Retained unsaved synthetic title");
      await page.locator("#dialog-close").click();
    });
    await check(`${name} mobile390 Board native control`, () => nativeCase(name, "board", "#board-to", 390));
    await check(`${name} mobile390 Features native control`, () => nativeCase(name, "features", "[data-items-status]", 390));
    await page.evaluate(() => qa.stop()); await page.close(); page = null;
    await browser.close(); browser = null;
  }
} catch (error) { results.push({ name:"fixture setup/execution",pass:false,error:error.stack }); console.error(error.stack); }
finally {
  await browser?.close();
  if (web) { web.closeAllConnections(); await new Promise(resolve => web.close(resolve)); }
  if (backend && backend.exitCode === null && backend.signalCode === null) { const ended=once(backend,"exit"); backend.kill("SIGTERM"); await ended; }
  await rm(stateDir, { recursive:true,force:true });
  const report={ label,source,fixtureHash,engines,caseFilter:caseFilter?.source||null,results,observations,screenshots,servedSources,
    nativeObservationLimit:"Browser screenshots do not reliably capture OS-native menu layers. nativeOpenObserved records actual :open visibility; OS menu appearance and keyboard commit/Escape/same-value dismissal remain unverified because Playwright protocol keys do not reach the native layer on this macOS host (confirmed in both headless and headed bare-form probes). Pointer outside dismissal is verified. The separately labelled input-only regression is synthetic; neither it nor selectOption substitutes for native acceptance." };
  await writeFile(path.join(artifacts,"results.json"),JSON.stringify(report,null,2));
  const esc=s=>String(s).replace(/[&<>"']/g,c=>({"&":"&amp;","<":"&lt;",">":"&gt;",'"':"&quot;","'":"&#39;"})[c]);
  await writeFile(path.join(artifacts,"index.html"),`<!doctype html><meta charset="utf-8"><title>Dropdown continuity ${esc(label)}</title><style>body{font:16px system-ui;max-width:1100px;margin:32px auto}img{max-width:100%;border:1px solid #aaa}pre{white-space:pre-wrap}</style><h1>Dropdown continuity: ${esc(label)}</h1><p>wi_b1d07b59cdf519eb-fix-1/#661 · QA #663 · status regression wi_a65c688c4b09f458/#1165</p><p>${esc(report.nativeObservationLimit)}</p><ul>${results.map(r=>`<li>${r.pass?'PASS':'FAIL'} ${esc(r.name)}${r.error?`<pre>${esc(r.error)}</pre>`:''}</li>`).join('')}</ul>${screenshots.map(f=>`<h2>${esc(f)}</h2><a href="${esc(f)}"><img src="${esc(f)}"></a>`).join('')}`);
}
assert.ok(results.length > 0, "No acceptance cases ran");
assert.equal(results.filter(r=>!r.pass).length,0,`Dropdown continuity failures: ${results.filter(r=>!r.pass).map(r=>r.name).join(', ')}`);
