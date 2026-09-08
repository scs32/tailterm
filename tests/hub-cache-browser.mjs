// Real presentation modules + cached/live clients + encrypted disposable vaults.
// The synthetic transport never reaches a hub, profile service, SSH, or tmux.
import { chromium, webkit, expect } from "@playwright/test";
import { createServer } from "vite";
import { mkdir } from "node:fs/promises";
import assert from "node:assert/strict";

const html = `<!doctype html><html><head><meta charset="utf-8">
<link rel="stylesheet" href="/client/style.css"><link rel="stylesheet" href="/client/work-items.css">
<style>body{overflow:auto}#mode-view{height:720px;padding:16px}#notice{margin:12px}</style>
</head><body><main id="mode-view"></main><dialog id="dialog"></dialog><p id="notice" role="status"></p>
<script type="module">
import * as vault from '/client/local-vault.js';
import {createHubClient} from '/client/hub-client.js';
import {createCachedHubClient} from '/client/cached-hub-client.js';
import {createHubReadCache} from '/client/hub-read-cache.js';
import {createBoardView} from '/client/board-view.js';
import {createTasksView} from '/client/tasks-view.js';
import {createWorkItemsView} from '/client/work-items-view.js';
import {createTeamsView} from '/client/teams-view.js';
const params=new URLSearchParams(location.search), password='synthetic cache QA passphrase';
const state={online:params.get('network')!=='offline',held:params.get('network')==='held',version:Number(params.get('version')||1),reads:0,responses:0,writes:Number(sessionStorage.getItem('synthetic-write-attempts')||0),waiting:[],notices:[]};
const taskId='tsk_1111111111111111';
const project=()=>({id:taskId,name:'Synthetic project v'+state.version,goal:'Synthetic goal v'+state.version,status:'open',orchestrator:'fixture-lead',createdAt:'2026-09-08T12:00:00Z'});
const agent=()=>({id:'agt_1111111111111111',name:'fixture-lead',status:'running',host:'fixture.invalid',session:'synthetic-only',runtime:'codex',readUpTo:0});
const message=(version)=>({seq:version,from:{user:'fixture'},text:'Synthetic board message v'+version,createdAt:'2026-09-08T12:00:00Z'});
const workItem=(kind)=>({id:kind==='bug'?'bug-fixture':'feature-fixture',taskId,kind,title:'Synthetic '+kind+' v'+state.version,description:'Synthetic description',status:'open',priority:'normal',revision:state.version});
const response=(body,status=200)=>new Response(JSON.stringify(body),{status,headers:{'Content-Type':'application/json'}});
async function transport(url,init){
  const path=new URL(url), method=init.method||'GET';
  if(method!=='GET'){
    state.writes++;
    sessionStorage.setItem('synthetic-write-attempts',state.writes);
    return response({error:'Synthetic write rejected: hub unavailable'},503);
  }
  if(path.pathname.endsWith('/events')){
    await new Promise(resolve=>setTimeout(resolve,100));
    if(!state.online)throw Error('Synthetic hub offline');
    return response({events:[],next:0});
  }
  state.reads++;
  if(!state.online)throw Error('Synthetic hub offline');
  if(state.held)await new Promise(resolve=>state.waiting.push(resolve));
  state.responses++;
  if(path.pathname==='/v1/tasks')return response({tasks:[project()]});
  if(path.pathname==='/v1/tasks/'+taskId)return response({task:project(),agents:[agent()]});
  if(path.pathname.endsWith('/messages')){
    const after=Number(path.searchParams.get('after')||0);
    return response({messages:Array.from({length:state.version},(_,i)=>message(i+1)).filter(m=>m.seq>after)});
  }
  if(path.pathname==='/v1/work-items')return response({items:[workItem(path.searchParams.get('kind'))],next:0});
  throw Error('Unexpected synthetic read: '+path.pathname);
}
await vault.localAPI('/unlock','POST',{password,username:'synthetic-alpha'});
let live,client,views,current;
const root=document.querySelector('#mode-view');
const notice=text=>{state.notices.push(text);document.querySelector('#notice').textContent=text};
const closeDialog=()=>{const d=document.querySelector('#dialog');d.close();d.replaceChildren()};
const dialog=(title,body)=>{const d=document.querySelector('#dialog');if(d.open)d.close();d.innerHTML='<div class="dialog-head"><h2>'+title+'</h2><button id="dialog-close" type="button">×</button></div>'+body;d.querySelector('#dialog-close').onclick=closeDialog;d.showModal()};
const noop=()=>{};
function configure(base='http://synthetic-hub.invalid',token='synthetic-token-alpha'){
  if(views)Object.values(views).forEach(view=>view.hide());
  client?.dispose();root.replaceChildren();current=null;
  live=createHubClient({baseURL:base,token,fetchImpl:transport});
  client=createCachedHubClient({client:live,cache:createHubReadCache(vault.hubReadCachePersistence()),online:()=>state.online,refreshMs:250});
  const common={client:()=>client,notice,configure:noop,getTabs:()=>[],activate:noop,openBoard:noop};
  views={
    board:createBoardView({...common,addAgent:noop,revealAgent:noop,attachTask:noop,newTask:noop}),
    projects:createTasksView({...common,confirm:async()=>false,taskHub:{groupOf:()=>null}}),
    bugs:createWorkItemsView({...common,kind:'bug',dialog,closeDialog}),
    features:createWorkItemsView({...common,kind:'feature',dialog,closeDialog}),
    teams:createTeamsView({getData:()=>vault.localData(),getServers:()=>vault.localData().servers,api:vault.localAPI,reloadData:async()=>{},notice,dialog,closeDialog,confirm:async()=>true,newTask:noop,addTeam:noop})
  };
}
configure();
window.qa={state,vault,taskId,password,
  get client(){return client},get live(){return live},
  async show(mode){if(current)views[current].hide();root.replaceChildren();current=mode;views[mode].mount(root);await views[mode].show()},
  release(){state.held=false;state.waiting.splice(0).forEach(resolve=>resolve())},
  async connection(online){state.online=online;await client.refreshConnection()},
  configure,
  async differentProfile(){
    Object.values(views).forEach(view=>view.hide());client.dispose();root.replaceChildren();
    await vault.localAPI('/lock','POST');await vault.resetVault();
    await vault.localAPI('/unlock','POST',{password,username:'synthetic-beta'});configure();
  }
};
</script></body></html>`;

const server = await createServer({
  configFile: false,
  root: process.cwd(),
  server: { host: "127.0.0.1", port: 0 },
  plugins: [{
    name: "isolated-hub-cache-browser",
    configureServer(vite) {
      vite.middlewares.use("/hub-cache-test", (_req, res) => {
        res.setHeader("Content-Type", "text/html");
        res.end(html);
      });
    },
  }],
});
await server.listen();
await mkdir(".build", { recursive: true });
const origin = `http://127.0.0.1:${server.httpServer.address().port}/hub-cache-test`;
const modes = ["board", "projects", "bugs", "features"];
const content = {
  board: "#board-messages",
  projects: "[data-task-card]",
  bugs: "[data-work-item]",
  features: "[data-work-item]",
};
const marker = (mode, version) => mode === "board" ? `Synthetic board message v${version}`
  : mode === "projects" ? `Synthetic project v${version}`
    : `Synthetic ${mode === "bugs" ? "bug" : "feature"} v${version}`;
async function open(page, network = "online", version = 1) {
  await page.goto(`${origin}?network=${network}&version=${version}`);
  await page.waitForFunction(() => !!window.qa);
}
async function show(page, mode, version) {
  // Start without awaiting show(): a regression blocking on the slow response
  // must fail the content assertion while the response remains held.
  await page.evaluate(mode => { window.showDone=false; qa.show(mode).then(()=>window.showDone=true); }, mode);
  await expect(page.locator(content[mode])).toContainText(marker(mode, version), { timeout: 5000 });
}
async function warm(page) {
  for (const mode of modes) await show(page, mode, 1);
  await page.evaluate(async () => { await qa.vault.profileSnapshot(); });
}

const failures = [];
try {
  for (const engine of [chromium, webkit]) {
    const name = engine.name(), browser = await engine.launch();
    const context = await browser.newContext({ viewport: { width: 1100, height: 800 } });
    const page = await context.newPage(), errors = [];
    page.on("pageerror", error => errors.push(error.message));
    // Guard against any accidental route to a real network service.
    await context.route("**/*", route => new URL(route.request().url()).origin === new URL(origin).origin
      ? route.continue() : route.abort());
    try {
      await open(page);
      await warm(page);
      console.log(`${name}: warmed actual Board/Projects/Bugs/Features through live client reads`);

      // Each mode starts after a real reload and vault unlock, with every hub
      // read held. Cached content must arrive while zero responses have arrived.
      for (const mode of modes) {
        await open(page, "held", 2);
        await show(page, mode, 1);
        await page.waitForFunction(() => qa.state.waiting.length > 0);
        assert.equal(await page.evaluate(() => qa.state.responses), 0, `${name} ${mode}: response arrived before cache assertion`);
        await expect(page.locator("#mode-view [role=status]")).toContainText("Saved data");
        await page.screenshot({ path: `.build/hub-cache-${mode}-held-${name}.png` });
        await page.evaluate(() => qa.release());
        await expect(page.locator(content[mode])).toContainText(marker(mode, 2));
        console.log(`${name}: ${mode} renders saved v1 before held response, then refreshes to v2`);
        // Restore the shared encrypted snapshot through real view reads for the
        // next mode. This does not reach into or seed cache implementation state.
        await open(page);
        await warm(page);
        await page.waitForFunction(() => qa.client.cacheStatus().label !== "Saved data · refreshing");
      }

      await open(page, "offline");
      for (const mode of modes) {
        await show(page, mode, 1);
        await expect(page.locator("#mode-view [role=status]")).toContainText("Saved data · offline");
      }
      assert.equal(await page.evaluate(() => qa.state.responses), 0);
      assert.match(await page.evaluate(async () => {
        try { await qa.live.listTasks(); return "unexpected cached live read"; }
        catch (error) { return error.message; }
      }), /Synthetic hub offline/, "The underlying authoritative client must still fail offline");
      console.log(`${name}: reload/unlock while hub offline restores all four saved views`);

      // Failed writes retain actual form drafts, never fabricate success, and
      // stay unsent when connectivity and presentation polling resume.
      await show(page, "board", 1);
      await page.locator("#board-text").fill("Synthetic unsent board draft");
      await page.locator("#board-compose button[type=submit]").click();
      await expect(page.locator("#notice")).toContainText("Post failed:");
      await expect(page.locator("#board-text")).toHaveValue("Synthetic unsent board draft");
      await expect(page.locator("#board-messages")).not.toContainText("Synthetic unsent board draft");
      for (const mode of ["bugs", "features"]) {
        await show(page, mode, 1);
        await page.locator("[data-items-new]").click();
        await page.locator("#work-item-title").fill(`Synthetic unsent ${mode} draft`);
        await page.locator("#work-item-description").fill("Retain this description after rejection.");
        await page.locator("#work-item-form button[type=submit]").click();
        await expect(page.locator("#work-item-error")).toContainText("Synthetic write rejected");
        await expect(page.locator("#work-item-title")).toHaveValue(`Synthetic unsent ${mode} draft`);
        await expect(page.locator("#work-item-description")).toHaveValue("Retain this description after rejection.");
        await expect(page.locator("#dialog")).toBeVisible();
        await page.locator("#dialog-close").click();
        await page.locator("[data-item-edit]").click();
        await page.locator("#work-item-title").fill(`Synthetic unsent ${mode} edit`);
        await page.locator("#work-item-form button[type=submit]").click();
        await expect(page.locator("#work-item-error")).toContainText("Synthetic write rejected");
        await expect(page.locator("#work-item-title")).toHaveValue(`Synthetic unsent ${mode} edit`);
        await page.locator("#dialog-close").click();
        await expect(page.locator("[data-work-item]")).not.toContainText(`Synthetic unsent ${mode}`);
        await page.locator("[data-item-send]").click();
        await page.locator("#work-item-dispatch button[type=submit]").click();
        await expect(page.locator("#work-item-dispatch-status")).toContainText("Synthetic write rejected");
        await expect(page.locator("#work-item-dispatch-status")).not.toContainText("message #");
        await page.locator("#dialog-close").click();
      }
      const writes = await page.evaluate(() => qa.state.writes);
      assert.equal(writes, 7);
      await page.evaluate(() => qa.connection(true));
      for (const mode of modes) await show(page, mode, 1);
      await page.waitForTimeout(750); // Cross three presentation refresh intervals.
      assert.equal(await page.evaluate(() => qa.state.writes), writes, "Reconnect replayed a rejected write");
      await show(page, "board", 1);
      await expect(page.locator("#board-text")).toHaveValue("Synthetic unsent board draft");
      assert.equal(await page.evaluate(() => qa.state.notices.some(text => /saved|sent successfully/i.test(text))), false);
      console.log(`${name}: rejected board/create/edit/dispatch writes retain drafts, show no success, and do not replay on reconnect`);

      // Teams save locally through the actual vault API while the hub is down.
      await page.evaluate(async () => { await qa.connection(false); await qa.show("teams"); });
      await page.locator("#teams-new").click();
      await page.locator("#team-name").fill("Synthetic offline team");
      await page.locator("[data-field=name]").fill("offline-lead");
      await page.locator("#team-form button[type=submit]").click();
      await expect(page.locator("[data-team]")).toContainText("Synthetic offline team");
      assert.equal(await page.evaluate(() => qa.state.writes), writes, "Local Teams wrote to the hub");
      await open(page, "offline");
      await page.evaluate(() => qa.show("teams"));
      await expect(page.locator("[data-team]")).toContainText("Synthetic offline team");
      await page.locator("[data-edit-team]").click();
      await page.locator("#team-name").fill("Synthetic offline team edited");
      await page.locator("#team-form button[type=submit]").click();
      await expect(page.locator("[data-team]")).toContainText("Synthetic offline team edited");
      await page.evaluate(() => qa.connection(true));
      await page.waitForTimeout(750);
      assert.equal(await page.evaluate(() => qa.state.writes), writes, "Reload/unlock replayed a rejected write");
      await page.evaluate(() => qa.connection(false));
      await page.screenshot({ path: `.build/hub-cache-teams-offline-${name}.png` });
      console.log(`${name}: local Teams create, reload, unlock, and edit work offline`);

      for (const scope of ["token", "hub", "profile"]) {
        await open(page, "offline");
        if (scope === "profile") await page.evaluate(() => qa.differentProfile());
        else await page.evaluate(scope => qa.configure(
          scope === "hub" ? "http://another-synthetic-hub.invalid" : undefined,
          scope === "token" ? "synthetic-token-beta" : undefined,
        ), scope);
        for (const mode of modes) {
          await page.evaluate(mode => qa.show(mode), mode);
          await expect(page.locator("#mode-view")).not.toContainText("Synthetic project");
          await expect(page.locator("#mode-view")).not.toContainText("Synthetic board message");
          await expect(page.locator("#mode-view")).not.toContainText("Synthetic bug");
          await expect(page.locator("#mode-view")).not.toContainText("Synthetic feature");
          await expect(page.locator("#mode-view")).toContainText(/Offline|offline/);
        }
        console.log(`${name}: different ${scope} cannot see any previous scope's cached view`);
      }
      assert.deepEqual(errors, [], "Unexpected browser errors");
      console.log(`${name}: PASS`);
    } catch (error) {
      await page.screenshot({ path: `.build/hub-cache-failure-${name}.png` }).catch(() => {});
      failures.push(new Error(`${name}: ${error.stack || error.message}`));
      console.error(failures.at(-1).message);
    } finally {
      await context.close();
      await browser.close();
    }
  }
} finally {
  await server.close();
}
if (failures.length) throw new AggregateError(failures, "Hub cache browser acceptance failed");
