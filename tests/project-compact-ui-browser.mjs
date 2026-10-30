// Builder acceptance for wi_929b036151c7f488 order8454 Start8502 release8504.
// Needs you scenario: wi_772031e5c0a5b70f order 51712.
// Only disposable synthetic browser,
// vault, and hub transport state; no live hub/profile/SSH/tmux access.
import { chromium, webkit, expect } from "@playwright/test";
import { createServer } from "vite";
import { mkdir, writeFile } from "node:fs/promises";
import assert from "node:assert/strict";

const html = `<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<link rel="stylesheet" href="/client/style.css"><link rel="stylesheet" href="/client/work-items.css">
</head><body><div id="app"><div id="workspace"><aside><h2>Fixture machines</h2></aside><main><header></header><div class="terminal-shell" hidden></div></main></div><dialog id="dialog"></dialog><div id="notice" role="status" hidden></div></div>
<script type="module">
import * as vault from '/client/local-vault.js';
import {createHubClient} from '/client/hub-client.js';
import {createCachedHubClient} from '/client/cached-hub-client.js';
import {createHubReadCache} from '/client/hub-read-cache.js';
import {createBoardView} from '/client/board-view.js';
import {createTasksView} from '/client/tasks-view.js';
import {createTeamsView} from '/client/teams-view.js';
import {createWorkItemsView} from '/client/work-items-view.js';
import {setupModes} from '/client/modes.js';
const params=new URLSearchParams(location.search);
const scenario=params.get('scenario')||'populated';
const long='A very long synthetic project label with enough words to exercise truncation and wrapping across narrow screens';
const ids=['tsk_1111111111111111','tsk_2222222222222222','tsk_3333333333333333'];
const state={online:true,failWrites:false,calls:[],notices:[],writes:0,held:false,waiting:[]};
const tasks=scenario==='empty'?[]:ids.map((id,i)=>({id,name:scenario==='long'?long+' '+(i+1):['Alpha project','Beta project','Closed project'][i],goal:'Synthetic goal '+(i+1),status:i===2?'closed':'open',cleanupPending:i===2?1:0,closedAt:i===2?'2026-09-08T12:00:00Z':undefined,orchestrator:'fixture-lead',createdAt:'2026-09-08T11:00:00Z'}));
const originalAgents=id=>[{id:'agt_'+id.slice(4),name:'fixture-lead',role:'orchestrator',status:tasks.find(t=>t.id===id)?.status==='closed'?'closed':'running',host:'fixture.invalid',session:'synthetic-session',runtime:'codex',readUpTo:0}];
const agents=id=>needsFor(id)?needsFor(id).agents:Array.from({length:state.teamCount??Number(new URLSearchParams(location.search).get('count')||12)},(_,i)=>({...originalAgents(id)[0],id:'agt_'+String(i+1).padStart(16,'0'),name:'fixture-agent-'+i,status:i===1?'needs_input':i===2?'retired':i===3?'closed':'running',...(i===1?{blockedReason:'permission'}:{})}));
// Owner-waiting fixture: A plain needs_input, B login-blocked, C running with an
// open owner request, D with an unanswered decision, E with only a closed request.
// X (open project) and Y, Z (closed project) are closed agents with an open
// request or unanswered decision; a closed agent never needs the owner.
const needAgent=(key,name,status,extra={})=>({id:'agt_need00000000000'+key,name,status,host:'fixture.invalid',session:'synthetic-session',runtime:'codex',readUpTo:0,...extra});
const from=key=>({agentId:'agt_need00000000000'+key,node:'fixture',user:'fixture'});
const needs=params.get('needs')?{
 agents:[needAgent('l','need-lead','running'),needAgent('a','need-a-plain','needs_input'),needAgent('b','need-b-login','needs_input',{blockedReason:'authentication',blockedText:'Synthetic login expired'}),needAgent('c','need-c-request','running'),needAgent('d','need-d-decision','needs_input'),needAgent('e','need-e-closed','needs_input'),needAgent('x','need-x-closed','closed')],
 obligations:[{id:'obl_0000000000000c01',recipientKind:'owner',state:'delivered',taskId:ids[0],messageSeq:5,subject:'Synthetic owner request',sourceKind:'request',needs:'ack_outcome',createdAt:'2026-09-08T12:00:00Z',dueAt:'2026-09-08T12:30:00Z',request:{seq:5,taskId:ids[0],from:from('c'),text:'REQUEST: Synthetic owner request',createdAt:'2026-09-08T12:00:00Z'}},{id:'obl_0000000000000e01',recipientKind:'owner',state:'closed',outcome:'result',taskId:ids[0],messageSeq:4,subject:'Synthetic closed request',sourceKind:'request',needs:'ack_outcome',createdAt:'2026-09-08T11:50:00Z',dueAt:'2026-09-08T12:20:00Z',request:{seq:4,taskId:ids[0],from:from('e'),text:'REQUEST: Synthetic closed request',createdAt:'2026-09-08T11:50:00Z'}},{id:'obl_0000000000000x01',recipientKind:'owner',state:'delivered',taskId:ids[0],messageSeq:3,subject:'Synthetic closed agent request',sourceKind:'request',needs:'ack_outcome',createdAt:'2026-09-08T11:40:00Z',dueAt:'2026-09-08T12:10:00Z',request:{seq:3,taskId:ids[0],from:from('x'),text:'REQUEST: Synthetic closed agent request',createdAt:'2026-09-08T11:40:00Z'}}],
 decisions:[{request:{seq:6,taskId:ids[0],from:from('d'),text:'Synthetic decision',createdAt:'2026-09-08T12:00:00Z',decisionRequest:{question:'Synthetic decision?',options:[{id:'keep',label:'Keep',description:'Synthetic keep'},{id:'drop',label:'Drop',description:'Synthetic drop'}],recommendedOptionId:'keep',recommendationReason:'Synthetic reason'}}},{request:{seq:8,taskId:ids[0],from:from('x'),text:'Synthetic closed agent decision',createdAt:'2026-09-08T11:45:00Z',decisionRequest:{question:'Synthetic closed agent decision?',options:[{id:'keep',label:'Keep',description:'Synthetic keep'}],recommendedOptionId:'keep',recommendationReason:'Synthetic reason'}}}],
 closed:{agents:[needAgent('y','need-y-closed','closed'),needAgent('z','need-z-closed','closed')],
  obligations:[{id:'obl_0000000000000z01',recipientKind:'owner',state:'delivered',taskId:ids[2],messageSeq:10,subject:'Synthetic archived request',sourceKind:'request',needs:'ack_outcome',createdAt:'2026-09-08T11:30:00Z',dueAt:'2026-09-08T12:00:00Z',request:{seq:10,taskId:ids[2],from:from('z'),text:'REQUEST: Synthetic archived request',createdAt:'2026-09-08T11:30:00Z'}}],
  decisions:[{request:{seq:9,taskId:ids[2],from:from('y'),text:'Synthetic archived decision',createdAt:'2026-09-08T11:30:00Z',decisionRequest:{question:'Synthetic archived decision?',options:[{id:'keep',label:'Keep',description:'Synthetic keep'}],recommendedOptionId:'keep',recommendationReason:'Synthetic reason'}}}]}
}:null;
const needsFor=id=>needs&&(id===ids[0]?needs:id===ids[2]?needs.closed:null);
const items=tasks.flatMap(task=>['bug','feature'].map(kind=>({id:kind+'-'+task.id,taskId:task.id,kind,title:scenario==='long'?long+' '+kind:task.name+' '+kind,description:'Synthetic description',priority:'normal',status:'open',revision:1})));
const messages=new Map(ids.map(id=>[id,[{seq:1,from:{user:'fixture'},text:'Synthetic message for '+id,createdAt:'2026-09-08T12:00:00Z'}]]));
const response=(body,status=200)=>new Response(JSON.stringify(body),{status,headers:{'Content-Type':'application/json'}});
async function transport(url,init){
 const u=new URL(url),p=u.pathname,method=init.method||'GET',body=init.body?JSON.parse(init.body):{};
 if(method!=='GET'){state.writes++;if(!state.online||state.failWrites)return response({error:'Synthetic write rejected'},503)}
 else if(!state.online)throw Error('Synthetic hub offline');
 // The suite triggers refresh explicitly; keep the synthetic long poll from
 // racing native keydown/keyup on a button during unrelated assertions.
 if(p.endsWith('/events')){if(u.searchParams.get('wait'))await new Promise(r=>setTimeout(r,30000));return response({events:[],next:0})}
 if(method==='GET'&&state.held)await new Promise(r=>state.waiting.push(r));
 if(p==='/v1/capabilities')return response(state.pauseSupported?{projectPause:{supported:true,versions:[1]}}:{});
 if(p==='/v1/tasks')return response({tasks});
 if(p==='/v1/work-items')return response({items:items.filter(x=>(!u.searchParams.get('taskId')||x.taskId===u.searchParams.get('taskId'))&&(!u.searchParams.get('status')||x.status===u.searchParams.get('status'))&&x.kind===u.searchParams.get('kind')),next:0});
 const match=p.match(/^\\/v1\\/tasks\\/([^/]+)(.*)$/);if(!match)throw Error('Unexpected synthetic route '+p);
 const [,id,tail]=match,task=tasks.find(t=>t.id===id);
 if(!tail)return response({task,agents:agents(id)});
 if(tail==='/obligations')return response({obligations:needsFor(id)?.obligations||[]});
 if(tail==='/decisions'){if(needs&&params.get('decisions')==='fail')return response({error:'Synthetic older hub without decisions'},404);return response({decisions:needsFor(id)?.decisions||[],nextAfter:0})}
 if(tail==='/messages'){
   if(method==='POST'){if(state.holdPost)await new Promise(resolve=>state.releasePost=resolve);const list=messages.get(id);const row={...body,from:{user:'fixture'},seq:list.length+1,createdAt:'2026-09-08T12:00:00Z'};list.push(row);return response(row)}
   return response({messages:messages.get(id).filter(m=>m.seq>Number(u.searchParams.get('after')||0))});
 }
 if(tail==='/work-items'&&method==='POST'){const item={...body,id:'item-'+state.writes,taskId:id,status:'open',revision:1};items.push(item);return response(item)}
 if(tail.startsWith('/work-items/')){
   const item=items.find(x=>x.id===tail.split('/')[2]);
   if(tail.endsWith('/dispatch')){item.lastDispatch={targetTaskId:body.targetTaskId,messageSeq:2};return response({dispatch:item.lastDispatch,item})}
   if(method==='PATCH')Object.assign(item,body,{revision:item.revision+1});
   return response(item);
 }
 throw Error('Unexpected synthetic route '+p);
}
await vault.localAPI('/unlock','POST',{password:'Synthetic board layout QA passphrase'});
if(scenario!=='empty')for(let i=0;i<2;i++)await vault.localAPI('/teams','POST',{name:scenario==='long'?long.slice(0,70)+' team '+(i+1):['Alpha team','Beta team'][i],orchestrator:'fixture-lead',members:[{name:'fixture-lead',runtime:'codex',role:'Coordinator',prompt:'Synthetic instructions'}]});
const live=createHubClient({baseURL:'http://synthetic-layout.invalid',token:'synthetic-only',fetchImpl:transport});
// Explicit invalidations below exercise refresh continuity without a timed
// poll replacing a control between the keyboard press and its native click.
// decisions=missing models an older client build without listDecisions.
const withoutDecisions=c=>params.get('decisions')==='missing'?new Proxy(c,{get:(t,k)=>k==='listDecisions'?undefined:t[k]}):c;
let client=withoutDecisions(createCachedHubClient({client:live,cache:createHubReadCache(vault.hubReadCachePersistence()),online:()=>state.online,refreshMs:60000}));
const notice=t=>{state.notices.push(t);const el=document.querySelector('#notice');el.textContent=t;el.hidden=false};
const record=(action,id)=>{state.calls.push({action,id})};
const dialog=(title,body)=>{const d=document.querySelector('#dialog');if(d.open)d.close();d.innerHTML='<div class="dialog-head"><h2>'+title+'</h2><button id="dialog-close" type="button">×</button></div>'+body;d.querySelector('#dialog-close').onclick=closeDialog;d.showModal()};
const closeDialog=()=>{const d=document.querySelector('#dialog');d.close();d.replaceChildren()};
let views,modes;
const common={client:()=>client,notice,configure:()=>record('configure'),getTabs:()=>[],activate:id=>record('activate',id),openBoard:id=>{record('board',id);modes.set('board');views.board.show(id)}};
const taskHub={groupOf:()=>null,newTask:()=>record('new-project'),addAgent:id=>record('add-agent',id),setupHandler:id=>record('handler',id),attachToCurrent:id=>record('attach',id),settings:id=>record('settings',id),revealAgent:id=>record('reveal',id),async closeTask(id){record('close',id);Object.assign(tasks.find(t=>t.id===id),{status:'closed',cleanupPending:1,closedAt:'2026-09-08T12:00:00Z'});client.invalidate();return tasks.find(t=>t.id===id)},async cleanupTask(id){record('cleanup',id);tasks.find(t=>t.id===id).cleanupPending=0;client.invalidate();return tasks.find(t=>t.id===id)}};
views={
 board:createBoardView({...common,addAgent:taskHub.addAgent,revealAgent:taskHub.revealAgent,attachTask:taskHub.attachToCurrent,newTask:taskHub.newTask,settings:taskHub.settings}),
 tasks:createTasksView({...common,taskHub,confirm:async()=>true,openWorkItems:(mode,id)=>{record(mode,id);modes.set(mode);views[mode].show(id)}}),
 teams:createTeamsView({getData:()=>vault.localData(),getServers:()=>vault.localData().servers,api:vault.localAPI,reloadData:async()=>{},notice,dialog,closeDialog,confirm:async()=>true,newTask:team=>record('team-new-project',team.id),addTeam:team=>record('team-add-project',team.id)}),
 bugs:createWorkItemsView({...common,kind:'bug',dialog,closeDialog}),
 features:createWorkItemsView({...common,kind:'feature',dialog,closeDialog})
};
modes=setupModes({header:document.querySelector('main > header'),main:document.querySelector('main'),onChange:(mode,container)=>{Object.values(views).forEach(v=>v.hide());container.replaceChildren();if(views[mode]){views[mode].mount(container);window.rendered=views[mode].show()}}});
window.qa={state,views,modes,tasks,items,ids,vault,needs,get client(){return client},
 replaceClient(){client.dispose();client=withoutDecisions(createCachedHubClient({client:live,cache:createHubReadCache(vault.hubReadCachePersistence()),online:()=>state.online,refreshMs:60000}))},
 async show(mode){modes.set(mode);await window.rendered},
 async offline(value=true){state.online=!value;await client.refreshConnection()},
 release(){state.held=false;state.waiting.splice(0).forEach(r=>r())}
};
</script></body></html>`;

const server = await createServer({
  configFile: false,
  cacheDir: ".build/project-compact-ui/vite-cache",
  root: process.cwd(),
  server: { host: "127.0.0.1", port: 0, hmr: false },
  logLevel: "error",
  plugins: [
    {
      name: "compact-fixture",
      configureServer(vite) {
        vite.middlewares.use("/compact-fixture", (_req, res) => {
          res.setHeader("Content-Type", "text/html");
          res.end(html);
        });
      },
    },
  ],
});
await server.listen();
const origin = `http://127.0.0.1:${server.httpServer.address().port}`;
await mkdir(".build/project-compact-ui", { recursive: true });
const results = [];
const needId = (key) => "agt_need00000000000" + key;
// Needs you lists only agents waiting on the owner (wi_772031e5c0a5b70f).
async function needsScenario(browser, engine, decisions = "") {
  const context = await browser.newContext({
    viewport: { width: 1440, height: 900 },
  });
  await context.route("**/*", (r) =>
    new URL(r.request().url()).origin === origin ? r.continue() : r.abort(),
  );
  const page = await context.newPage(),
    errors = [];
  page.setDefaultTimeout(10000);
  page.on("pageerror", (e) => {
    errors.push(e.message);
    console.error(e.message);
  });
  const prefix = `${engine.name()}-needs${decisions ? "-" + decisions : ""}`;
  const attention = {
    board: ".project-team-attention [data-board-agent]",
    tasks: ".needs-you [data-task-agent]",
  };
  const agentAttr = { board: "data-board-agent", tasks: "data-task-agent" };
  const ids = (mode) =>
    page
      .locator(attention[mode])
      .evaluateAll(
        (els, attr) => els.map((el) => el.getAttribute(attr)).sort(),
        agentAttr[mode],
      );
  const inRoster = (mode, key) =>
    page.locator(`[data-team-roster] [${agentAttr[mode]}="${needId(key)}"]`);
  try {
    await page.goto(
      `${origin}/compact-fixture?count=12&needs=1${decisions ? "&decisions=" + decisions : ""}`,
    );
    await page.waitForFunction(() => !!window.qa);
    for (const mode of decisions ? ["tasks"] : ["board", "tasks"]) {
      await page.evaluate((mode) => qa.show(mode), mode);
      await page
        .locator(mode === "board" ? "[data-board-task]" : "[data-task-select]")
        .first()
        .click();
      await page.waitForFunction(
        () => qa.client.cacheStatus().label === "Saved data",
      );
      const owner = decisions ? ["b", "c"] : ["b", "c", "d"];
      await expect.poll(() => ids(mode)).toEqual(owner.map(needId));
      if (mode === "tasks")
        await expect(page.locator(".needs-you .count-badge")).toHaveText(
          String(owner.length),
        );
      const toggle = page.locator("[data-team-toggle]");
      await expect(toggle).toContainText("6");
      if ((await toggle.getAttribute("aria-expanded")) === "false")
        await toggle.click();
      for (const key of decisions ? ["a", "d", "e"] : ["a", "e"]) {
        const row = inRoster(mode, key);
        await expect(row).toHaveCount(1);
        await expect(row).toContainText("Waiting");
        await expect(row).not.toContainText("Needs you");
        await expect(row.locator(".status-dot")).not.toHaveClass(/attention/);
      }
      const label = (key) =>
        mode === "board"
          ? page.locator(`${attention.board}[data-board-agent="${needId(key)}"]`)
          : inRoster(mode, key);
      await expect(label("b")).toContainText("Login required");
      await expect(label("c")).toContainText("running");
      if (!decisions) await expect(label("d")).toContainText("Needs you");
      if (mode === "board") {
        // Every live agent shows once: owner waiters above, the rest below.
        await expect(inRoster(mode, "c")).toHaveCount(0);
        await expect(page.locator(".board-agent")).toHaveCount(6);
      } else
        await expect(page.locator("[data-team-roster] .board-agent")).toHaveCount(6);
      await page.screenshot({
        path: `.build/project-compact-ui/${prefix}-${mode}.png`,
      });
      if (decisions) continue;
      // Answering the decision and closing the request leave only the blocker.
      await page.evaluate(() => {
        qa.needs.decisions[0].answer = {
          seq: 7,
          from: { node: "fixture", user: "owner" },
          text: "Keep",
          decisionAnswer: { optionId: "keep" },
          createdAt: "2026-09-08T12:05:00Z",
        };
        qa.needs.obligations[0].state = "closed";
        qa.client.invalidate();
      });
      await expect.poll(() => ids(mode)).toEqual([needId("b")]);
      await expect(inRoster(mode, "d")).toContainText("Waiting");
      await expect(inRoster(mode, "c")).toHaveCount(1);
      await page.evaluate(() => {
        delete qa.needs.decisions[0].answer;
        qa.needs.obligations[0].state = "delivered";
        qa.client.invalidate();
      });
      await expect.poll(() => ids(mode)).toEqual(owner.map(needId));
      if (mode !== "board") continue;
      // A closed project's Board keeps its closed agents in the roster only,
      // even with a loaded unanswered decision and an open owner request.
      await page.locator(".board-closed summary").click();
      await page.locator('[data-board-task="tsk_3333333333333333"]').click();
      await expect(page.locator('[data-decision-request="9"]')).toBeVisible();
      await expect(page.locator('[data-owner-request="obl_0000000000000z01"]')).toBeVisible();
      await expect(page.locator(".project-team-attention")).toHaveCount(0);
      await expect(page.locator("[data-team-roster] .board-agent")).toHaveCount(2);
      await expect(page.locator("[data-team-roster]")).toContainText("need-y-closed");
      await expect(page.locator("[data-team-roster]")).toContainText("need-z-closed");
    }
    assert.deepEqual(errors, []);
    console.log("PASS", prefix);
  } catch (error) {
    console.error("FIXTURE DOM", await page.locator("body").innerText());
    await page.screenshot({
      path: ".build/project-compact-ui/" + prefix + "-failure.png",
    });
    throw error;
  } finally {
    await context.close();
  }
}
try {
  for (const engine of [chromium, webkit]) {
    const browser = await engine.launch();
    try {
      for (const width of [1440, 1200, 390])
        for (const count of [0, 1, 12]) {
          const context = await browser.newContext({
            viewport: { width, height: 900 },
          });
          await context.route("**/*", (r) =>
            new URL(r.request().url()).origin === origin
              ? r.continue()
              : r.abort(),
          );
          const page = await context.newPage(),
            errors = [];
          page.setDefaultTimeout(10000);
          page.on("pageerror", (e) => {
            errors.push(e.message);
            console.error(e.message);
          });
          const prefix = `${engine.name()}-${width}-${count}`;
          try {
            await page.goto(`${origin}/compact-fixture?count=${count}`);
            await page.waitForFunction(() => !!window.qa);
            await page.evaluate((count) => (qa.state.teamCount = count), count);
            for (const mode of ["board", "tasks"]) {
              await page.evaluate((mode) => qa.show(mode), mode);
              if (mode === "board")
                await page.locator("[data-board-task]").first().click();
              else await page.locator("[data-task-select]").first().click();
              await page.waitForFunction(
                () => qa.client.cacheStatus().label === "Saved data",
              );
              await page.waitForTimeout(200);
              const toggle = page.locator("[data-team-toggle]");
              await expect(toggle).toHaveCount(count ? 1 : 0);
              await expect(page.locator(".hub-sync-status")).toHaveCount(0);
              await expect(page.locator(".compose-note")).toHaveCount(0);
              if (mode === "tasks") {
                // The footer label wraps only between words, in the app shell
                // and in a view mounted without it.
                const splitWords = () =>
                  page.evaluate(() => {
                    const text = document.querySelector(
                      ".task-detail-card .task-actions > .fine",
                    ).firstChild;
                    return [...text.data.matchAll(/\S+/g)]
                      .filter((m) => {
                        const range = document.createRange();
                        range.setStart(text, m.index);
                        range.setEnd(text, m.index + m[0].length);
                        const tops = [...range.getClientRects()].map((r) =>
                          Math.round(r.top),
                        );
                        return new Set(tops).size !== 1;
                      })
                      .map((m) => m[0]);
                  });
                assert.deepEqual(
                  await splitWords(),
                  [],
                  "label in shell keeps whole words",
                );
                await page.evaluate(
                  () =>
                    (document.querySelector("#mode-view").id =
                      "unshelled-view"),
                );
                assert.deepEqual(
                  await splitWords(),
                  [],
                  "label without shell keeps whole words",
                );
                await page.evaluate(
                  () =>
                    (document.querySelector("#unshelled-view").id =
                      "mode-view"),
                );
              }
              if (!count) continue;
              await page.evaluate(() => {
                window.savedViewHeading = document.querySelector(".board-head");
                window.savedTeamToggle = document.querySelector("[data-team-toggle]");
              });
              await page.evaluate(() => qa.client.refreshConnection());
              assert.equal(
                await page.evaluate(
                  () =>
                    window.savedViewHeading === document.querySelector(".board-head") &&
                    window.savedTeamToggle === document.querySelector("[data-team-toggle]"),
                ),
                true,
                "unchanged saved-data refresh retains visible DOM",
              );
              await page.evaluate(() => (qa.state.held = true));
              await expect(toggle).toHaveAttribute(
                "aria-expanded",
                String(count <= 4),
              );
              assert.equal(
                await toggle.getAttribute("aria-controls"),
                await page.locator("[data-team-roster]").getAttribute("id"),
              );
              const calls = await page.evaluate(() => ({
                writes: qa.state.writes,
                calls: qa.state.calls.length,
              }));
              if (mode === "board") {
                await page
                  .locator("#board-text")
                  .fill("Retained synthetic draft");
                await page.locator("#board-text").evaluate((el) => {
                  el.setSelectionRange(5, 9);
                  window.savedText = el;
                  window.savedTo = document.querySelector("#board-to");
                  window.savedMessages =
                    document.querySelector("#board-messages");
                });
              }
              await toggle.focus();
              await page.keyboard.press("Enter");
              await expect(toggle).toHaveAttribute(
                "aria-expanded",
                String(count > 4),
              );
              await expect(toggle).toBeFocused();
              assert.notEqual(
                await toggle.evaluate((e) => getComputedStyle(e).outlineStyle),
                "none",
              );
              // Hold a real cache refresh while the native Space keydown and
              // keyup activate the focused control.
              await page.evaluate(() => {
                window.pendingTeamRefresh = qa.client.refreshConnection();
              });
              await page.waitForFunction(() => qa.state.waiting.length > 0);
              await toggle.focus();
              await page.keyboard.press("Space");
              await expect(toggle).toHaveAttribute(
                "aria-expanded",
                String(count <= 4),
              );
              if (count > 4) await toggle.click();
              await expect(toggle).toHaveAttribute("aria-expanded", "true");
              const expanded = await page
                .locator(mode === "board" ? "#board-messages" : ".task-agents")
                .evaluate((el) => {
                  const r = el.getBoundingClientRect();
                  return { x: r.x, y: r.y, width: r.width, height: r.height };
                });
              await page.screenshot({
                path: `.build/project-compact-ui/${prefix}-${mode}-expanded.png`,
              });
              await toggle.click();
              await expect(page.locator("[data-team-roster]")).toBeHidden();
              const collapsed = await page
                .locator(mode === "board" ? "#board-messages" : ".task-actions")
                .evaluate((el) => {
                  const r = el.getBoundingClientRect();
                  return { x: r.x, y: r.y, width: r.width, height: r.height };
                });
              await page.screenshot({
                path: `.build/project-compact-ui/${prefix}-${mode}-collapsed.png`,
              });
              if (mode === "board") {
                assert.ok(
                  collapsed.height > expanded.height,
                  "collapse releases Board height",
                );
                await expect(page.locator("#board-add-agent")).toBeVisible();
                await expect(page.locator("#board-text")).toHaveValue(
                  "Retained synthetic draft",
                );
                assert.equal(
                  await page.evaluate(() => {
                    const text = document.querySelector("#board-text");
                    text.setSelectionRange(5, 9);
                    const to = document.querySelector("#board-to"),
                      messages = document.querySelector("#board-messages"),
                      toggle = document.querySelector("[data-team-toggle]");
                    toggle.click();
                    toggle.click();
                    return (
                      text === document.querySelector("#board-text") &&
                      to === document.querySelector("#board-to") &&
                      messages === document.querySelector("#board-messages") &&
                      text.selectionStart === 5 &&
                      text.selectionEnd === 9
                    );
                  }),
                  true,
                  "toggle itself retains controls",
                );
              } else
                await expect(page.locator("[data-task-add]")).toBeVisible();
              if (count > 4)
                await expect(
                  page.locator(
                    mode === "board"
                      ? ".project-team-attention [data-board-agent]"
                      : ".needs-you [data-task-agent]",
                  ),
                ).toBeVisible();
              assert.deepEqual(
                await page.evaluate(() => ({
                  writes: qa.state.writes,
                  calls: qa.state.calls.length,
                })),
                calls,
                "disclosure has no mutations/navigation",
              );
              await page.evaluate(() => qa.release());
              await page.evaluate(() => window.pendingTeamRefresh);
              // Polling preserves the explicit choice and keyboard focus.
              await toggle.focus();
              await page.evaluate(() => qa.client.invalidate());
              await page.waitForTimeout(700);
              await expect(toggle).toHaveAttribute("aria-expanded", "false");
              await expect(toggle).toBeFocused();
              const select =
                mode === "board" ? "[data-board-task]" : "[data-task-select]";
              await page.locator(select).nth(1).click();
              await expect(toggle).toHaveAttribute(
                "aria-expanded",
                String(count <= 4),
              );
              await page.locator(select).first().click();
              await expect(toggle).toHaveAttribute("aria-expanded", "false");
              // Replacing the credential-qualified client clears this view's choices.
              await page.evaluate(async (mode) => {
                Object.values(qa.views).forEach((view) => view.hide());
                qa.replaceClient();
                await qa.views[mode].show(qa.ids[0]);
              }, mode);
              await expect(toggle).toHaveAttribute(
                "aria-expanded",
                String(count <= 4),
              );
              if (count <= 4) await toggle.click();
              // A new lifecycle of the same project does not inherit the old choice.
              await page.evaluate(() => {
                qa.tasks[0].lifecycleGeneration = 2;
                qa.client.invalidate();
              });
              await page.waitForTimeout(700);
              await expect(toggle).toHaveAttribute(
                "aria-expanded",
                String(count <= 4),
              );
              // Restore lifecycle for next independent view.
              await page.evaluate(() => {
                delete qa.tasks[0].lifecycleGeneration;
                qa.client.invalidate();
              });
              assert.ok(
                await page.evaluate(
                  () => document.documentElement.scrollWidth <= innerWidth,
                ),
                "no horizontal page overflow",
              );
              results.push({
                engine: engine.name(),
                width,
                count,
                mode,
                expanded,
                collapsed,
              });
            }
            if (count === 12) {
              // A disclosure remains available while a real fixture post is in flight.
              await page.evaluate(() => qa.show("board"));
              await page.locator("[data-board-task]").first().click();
              await page.locator("#board-text").fill("Synthetic pending send");
              await page.evaluate(() => (qa.state.holdPost = true));
              await page.evaluate(() => {
                window.pendingCompose = document.querySelector("#board-compose");
                window.pendingText = document.querySelector("#board-text");
                window.pendingMessages = document.querySelector("#board-messages");
              });
              await page.locator("#board-compose button[type=submit]").click();
              await expect(page.locator("#board-text")).toBeDisabled();
              await expect(page.locator("#board-to")).toBeDisabled();
              await expect(page.locator("#board-compose button[type=submit]")).toBeDisabled();
              await page.waitForFunction(() => !!qa.state.releasePost);
              const pendingWrites = await page.evaluate(() => qa.state.writes);
              await page.locator("[data-team-toggle]").click();
              await page.locator("[data-team-toggle]").click();
              assert.equal(
                await page.evaluate(() =>
                  window.pendingCompose === document.querySelector("#board-compose") &&
                  window.pendingText === document.querySelector("#board-text") &&
                  window.pendingMessages === document.querySelector("#board-messages"),
                ),
                true,
                "held send retains the visible compose and messages DOM",
              );
              await expect(page.locator("#board-text")).toHaveValue(
                "Synthetic pending send",
              );
              await expect(page.locator("#board-text")).toBeDisabled();
              assert.equal(
                await page.evaluate(() => qa.state.writes),
                pendingWrites,
              );
              await page.evaluate(() => {
                qa.state.holdPost = false;
                qa.state.releasePost();
              });
              await expect(page.locator("#board-text")).toBeEnabled();
              await expect(page.locator("#board-to")).toBeEnabled();
              await expect(page.locator("#board-compose button[type=submit]")).toBeEnabled();
              await expect(page.locator("#board-text")).toHaveValue("");
              await page.locator("#board-text").fill("Synthetic failed send");
              await page.locator("#board-text").evaluate((el) => el.setSelectionRange(5, 9));
              await page.evaluate(() => (qa.state.failWrites = true));
              await page.keyboard.press("Enter");
              await expect(page.locator("#notice")).toContainText("Post failed:");
              await expect(page.locator("#board-text")).toBeEnabled();
              await expect(page.locator("#board-to")).toBeEnabled();
              await expect(page.locator("#board-compose button[type=submit]")).toBeEnabled();
              await expect(page.locator("#board-text")).toHaveValue("Synthetic failed send");
              await expect(page.locator("#board-text")).toBeFocused();
              assert.deepEqual(
                await page.locator("#board-text").evaluate((el) => [el.selectionStart, el.selectionEnd]),
                [5, 9],
                "failed send restores the draft caret",
              );
              await page.evaluate(() => (qa.state.failWrites = false));
              // Closed Board preserves all retained agents; no active lifecycle actions.
              await page.evaluate(() => qa.show("board"));
              await page.locator(".board-closed summary").click();
              await page
                .locator('[data-board-task="tsk_3333333333333333"]')
                .click();
              await expect(page.locator("#board-compose")).toHaveCount(0);
              await expect(page.locator("#board-add-agent")).toHaveCount(0);
              await page.locator("[data-team-toggle]").click();
              await expect(page.locator(".board-agent")).toHaveCount(12);
              assert.equal(
                await page.locator(".board-agent:disabled").count(),
                12,
              );
              // Existing lifecycle controls stay reachable without a misleading roster.
              for (const pauseState of ["paused", "resuming"]) {
                await page.evaluate(async (pauseState) => {
                  qa.state.pauseSupported = true;
                  Object.assign(qa.tasks[0], {
                    pauseState,
                    lifecycleGeneration: 7,
                    pauseGeneration: 2,
                  });
                  qa.client.invalidate();
                  await qa.show("tasks");
                  await qa.views.tasks.show();
                }, pauseState);
                await page.locator("[data-task-select]").first().click();
                await expect(page.locator("[data-task-resume]")).toBeVisible();
                await expect(page.locator("[data-task-board]")).toBeVisible();
                await expect(page.locator("[data-team-toggle]")).toHaveCount(0);
              }
              await page.evaluate(() => {
                qa.state.pauseSupported = false;
                delete qa.tasks[0].pauseState;
                qa.client.invalidate();
              });
            }
            // Meaningful offline status remains, routine successful cache status is absent.
            for (const mode of ["board", "tasks", "bugs", "features"]) {
              await page.evaluate((mode) => qa.show(mode), mode);
              if (mode === "board")
                await page.locator("[data-board-task]").first().click();
              if (mode === "tasks")
                await page.locator("[data-task-select]").first().click();
            }
            await page.waitForTimeout(350);
            await page.evaluate(() => qa.offline());
            for (const mode of ["board", "tasks", "bugs", "features"]) {
              await page.evaluate((mode) => qa.show(mode), mode);
              await expect(
                page.locator(
                  mode === "bugs" || mode === "features"
                    ? "[data-work-items-sync]"
                    : ".hub-sync-status",
                ),
              ).toHaveText("Offline · showing cached data");
            }
            await page.evaluate(() => qa.offline(false));
            assert.deepEqual(errors, []);
            console.log("PASS", prefix);
          } catch (error) {
            console.error(
              "FIXTURE DOM",
              await page.locator("body").innerText(),
            );
            await page.screenshot({
              path: ".build/project-compact-ui/" + prefix + "-failure.png",
            });
            throw error;
          } finally {
            await context.close();
          }
        }
      for (const decisions of ["", "fail", "missing"])
        await needsScenario(browser, engine, decisions);
    } finally {
      await browser.close();
    }
  }
} finally {
  await server.close();
  await writeFile(
    ".build/project-compact-ui/results.json",
    JSON.stringify(results, null, 2),
  );
}
