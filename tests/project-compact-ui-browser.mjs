// Builder acceptance for wi_929b036151c7f488 order8454 Start8502 release8504.
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
const scenario=new URLSearchParams(location.search).get('scenario')||'populated';
const long='A very long synthetic project label with enough words to exercise truncation and wrapping across narrow screens';
const ids=['tsk_1111111111111111','tsk_2222222222222222','tsk_3333333333333333'];
const state={online:true,failWrites:false,calls:[],notices:[],writes:0,held:false,waiting:[]};
const tasks=scenario==='empty'?[]:ids.map((id,i)=>({id,name:scenario==='long'?long+' '+(i+1):['Alpha project','Beta project','Closed project'][i],goal:'Synthetic goal '+(i+1),status:i===2?'closed':'open',cleanupPending:i===2?1:0,closedAt:i===2?'2026-09-08T12:00:00Z':undefined,orchestrator:'fixture-lead',createdAt:'2026-09-08T11:00:00Z'}));
const originalAgents=id=>[{id:'agt_'+id.slice(4),name:'fixture-lead',role:'orchestrator',status:tasks.find(t=>t.id===id)?.status==='closed'?'closed':'running',host:'fixture.invalid',session:'synthetic-session',runtime:'codex',readUpTo:0}];
const agents=id=>Array.from({length:state.teamCount??Number(new URLSearchParams(location.search).get('count')||12)},(_,i)=>({...originalAgents(id)[0],id:'agt_'+String(i+1).padStart(16,'0'),name:'fixture-agent-'+i,status:i===1?'needs_input':i===2?'retired':i===3?'closed':'running'}));
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
 if(tail==='/decisions')return response({decisions:[],nextAfter:0});
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
let client=createCachedHubClient({client:live,cache:createHubReadCache(vault.hubReadCachePersistence()),online:()=>state.online,refreshMs:60000});
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
window.qa={state,views,modes,tasks,items,ids,vault,get client(){return client},
 replaceClient(){client.dispose();client=createCachedHubClient({client:live,cache:createHubReadCache(vault.hubReadCachePersistence()),online:()=>state.online,refreshMs:60000})},
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
try {
  for (const engine of [chromium, webkit]) {
    const browser = await engine.launch();
    try {
      for (const width of [1440, 390])
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
              if (!count) continue;
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
              await page.locator("#board-compose button[type=submit]").click();
              // Force the same view refresh that a live poll can trigger while
              // the fixture post remains pending.
              await page.evaluate(() => qa.client.invalidate());
              await expect(page.locator("#board-text")).toBeDisabled();
              const pendingWrites = await page.evaluate(() => qa.state.writes);
              await page.locator("[data-team-toggle]").click();
              await page.locator("[data-team-toggle]").click();
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
              await expect(page.locator("#board-text")).toHaveValue("");
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
