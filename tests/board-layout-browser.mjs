// Independent Board-reference acceptance. Only disposable synthetic browser,
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
const agents=id=>[{id:'agt_'+id.slice(4),name:'fixture-lead',role:'orchestrator',status:tasks.find(t=>t.id===id)?.status==='closed'?'closed':'running',host:'fixture.invalid',session:'synthetic-session',runtime:'codex',readUpTo:0}];
const items=tasks.flatMap(task=>['bug','feature'].map(kind=>({id:kind+'-'+task.id,taskId:task.id,kind,title:scenario==='long'?long+' '+kind:task.name+' '+kind,description:'Synthetic description',priority:'normal',status:'open',revision:1})));
const messages=new Map(ids.map(id=>[id,[{seq:1,from:{user:'fixture'},text:'Synthetic message for '+id,createdAt:'2026-09-08T12:00:00Z'}]]));
const response=(body,status=200)=>new Response(JSON.stringify(body),{status,headers:{'Content-Type':'application/json'}});
async function transport(url,init){
 const u=new URL(url),p=u.pathname,method=init.method||'GET',body=init.body?JSON.parse(init.body):{};
 if(method!=='GET'){state.writes++;if(!state.online||state.failWrites)return response({error:'Synthetic write rejected'},503)}
 else if(!state.online)throw Error('Synthetic hub offline');
 if(p.endsWith('/events')){if(u.searchParams.get('wait'))await new Promise(r=>setTimeout(r,150));return response({events:[],next:0})}
 if(method==='GET'&&state.held)await new Promise(r=>state.waiting.push(r));
 if(p==='/v1/capabilities')return response({});
 if(p==='/v1/tasks')return response({tasks});
 if(p==='/v1/work-items')return response({items:items.filter(x=>(!u.searchParams.get('taskId')||x.taskId===u.searchParams.get('taskId'))&&(!u.searchParams.get('status')||x.status===u.searchParams.get('status'))&&x.kind===u.searchParams.get('kind')),next:0});
 const match=p.match(/^\\/v1\\/tasks\\/([^/]+)(.*)$/);if(!match)throw Error('Unexpected synthetic route '+p);
 const [,id,tail]=match,task=tasks.find(t=>t.id===id);
 if(!tail)return response({task,agents:agents(id)});
 if(tail==='/decisions')return response({decisions:[],nextAfter:0});
 if(tail==='/messages'){
   if(method==='POST'){const list=messages.get(id);const row={...body,from:{user:'fixture'},seq:list.length+1,createdAt:'2026-09-08T12:00:00Z'};list.push(row);return response(row)}
   return response({messages:messages.get(id).filter(m=>m.seq>Number(u.searchParams.get('after')||0))});
 }
 if(tail==='/work-items'&&method==='POST'){const item={...body,id:'item-'+state.writes,taskId:id,status:'open',revision:1};items.push(item);return response(item)}
 if(tail.startsWith('/work-items/')){
   const item=items.find(x=>x.id===tail.split('/')[2]);
   if(tail.endsWith('/dispatch')){item.lastDispatch={targetTaskId:body.targetTaskId,messageSeq:2};return response({dispatch:item.lastDispatch,item})}
   if(tail.endsWith('/updates')&&method==='POST'){
     if(body.expectedRevision!==item.revision)return response({error:'Stale synthetic revision'},409);
     Object.assign(item,body,{revision:item.revision+1});return response({item});
   }
   if(method==='PATCH')Object.assign(item,body,{revision:item.revision+1});
   return response(item);
 }
 throw Error('Unexpected synthetic route '+p);
}
await vault.localAPI('/unlock','POST',{password:'Synthetic board layout QA passphrase'});
if(scenario!=='empty')for(let i=0;i<2;i++)await vault.localAPI('/teams','POST',{name:scenario==='long'?long.slice(0,70)+' team '+(i+1):['Alpha team','Beta team'][i],orchestrator:'fixture-lead',members:[{name:'fixture-lead',runtime:'codex',role:'Coordinator',prompt:'Synthetic instructions'}]});
const live=createHubClient({baseURL:'http://synthetic-layout.invalid',token:'synthetic-only',fetchImpl:transport});
const client=createCachedHubClient({client:live,cache:createHubReadCache(vault.hubReadCachePersistence()),online:()=>state.online,refreshMs:300});
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
window.qa={state,views,modes,tasks,items,ids,vault,client,
 async show(mode){modes.set(mode);await window.rendered},
 async offline(value=true){state.online=!value;await client.refreshConnection()},
 release(){state.held=false;state.waiting.splice(0).forEach(r=>r())}
};
</script></body></html>`;

const server = await createServer({
  configFile: false,
  root: process.cwd(),
  server: { host: "127.0.0.1", port: 0 },
  plugins: [{ name: "board-layout-fixture", configureServer(vite) {
    vite.middlewares.use("/board-layout-test", (_req, res) => {
      res.setHeader("Content-Type", "text/html");
      res.end(html);
    });
  } }],
});
await server.listen();
await mkdir(".build", { recursive: true });
const origin = `http://127.0.0.1:${server.httpServer.address().port}`;
const viewNames = ["board", "tasks", "teams", "bugs", "features"];
const selectors = {
  board: "[data-board-task]", tasks: "[data-task-select]", teams: "[data-team-select]",
  bugs: "[data-items-scope]", features: "[data-items-scope]",
};
const results = [], failures = [], skips = [];
function check(label, operation) {
  try { operation(); }
  catch (error) { failures.push(`${label}: ${error.message}`); console.error(failures.at(-1)); }
}
const near = (actual, expected, label) => assert.ok(Math.abs(actual - expected) <= 1,
  `${label}: got ${actual}, Board reference ${expected}`);
async function show(page, mode) {
  await page.locator(`header [data-mode="${mode}"]`).click();
  await page.evaluate(() => window.rendered);
  await page.locator("#mode-view > div > aside").waitFor();
  await page.mouse.move(0, 0);
  await page.locator("#mode-view").evaluate(el => el.scrollTop = 0);
}
async function geometry(page, mode) {
  return page.evaluate(selector => {
    const view = document.querySelector("#mode-view"), outer = view.firstElementChild;
    const rail = outer.querySelector(":scope > aside"), main = outer.querySelector(":scope > section");
    const rect = el => { const r=el.getBoundingClientRect();return Object.fromEntries(['x','y','width','height','right','bottom'].map(k=>[k,r[k]])); };
    const style = el => {const s=getComputedStyle(el);return Object.fromEntries(['fontFamily','fontSize','lineHeight','fontWeight','minHeight','alignItems','borderRightWidth','borderBottomWidth','borderColor','borderRadius','backgroundColor','color','paddingLeft','paddingTop','paddingRight','paddingBottom','gap','display','flexDirection'].map(k=>[k,s[k]]))};
    const button=[...rail.querySelectorAll(selector)].find(el=>el.getBoundingClientRect().height>0);
    const heading=main?.querySelector('h2');
    const headingRow=heading?.closest('.view-heading')||heading?.parentElement;
    const headingControls=headingRow?.querySelector('.work-items-controls');
    const controls=[...main.querySelectorAll('button,select')].filter(el=>{const r=el.getBoundingClientRect();return r.height>0&&!el.closest('[popover]')&&r.y>=0&&r.y<innerHeight});
    const toolbar=[...main.querySelectorAll('.view-actions > button,.work-items-controls > button,.work-items-controls select,.task-actions > button')].filter(el=>el.getBoundingClientRect().height>0);
    const itemHeader = headingRow?.closest('.work-items-head');
    const itemPart = selector => { const el=itemHeader?.querySelector(selector);return el && el.getBoundingClientRect().height>0 ? rect(el) : null; };
    return {view:rect(view),outer:rect(outer),rail:rect(rail),main:rect(main),railStyle:style(rail),mainStyle:style(main),
      button:button?{rect:rect(button),style:style(button),text:button.textContent}:null,
      heading:heading?{rect:rect(heading),style:style(heading),rowStyle:style(headingRow),rowRect:rect(headingRow),controlsRect:headingControls?rect(headingControls):null}:null,
      itemParts:itemHeader?{count:itemPart('.count-badge'),search:itemPart('[data-items-search]'),status:itemPart('[data-items-status]'),newButton:itemPart('[data-items-new]')}:null,
      controls:controls.map(el=>({text:el.textContent.trim().slice(0,40),rect:rect(el),style:style(el)})),
      toolbar:toolbar.map(el=>({text:el.textContent.trim().slice(0,40),rect:rect(el),style:style(el)})),
      pageWidth:document.documentElement.scrollWidth,viewportWidth:innerWidth};
  }, selectors[mode]);
}
function compare(actual, board, width, label, scenario) {
  for (const box of ["view", "outer", "rail", "main"])
    for (const dimension of width <= 760 && box === "rail" ? ["x", "y", "width"]
      : width <= 760 && box === "main" ? ["x", "width", "bottom"] : ["x", "y", "width", "height"])
      check(label, () => near(actual[box][dimension], board[box][dimension], `${box}.${dimension}`));
  for (const side of ["paddingLeft", "paddingTop", "paddingRight", "paddingBottom"])
    check(label, () => assert.equal(actual.mainStyle[side], board.mainStyle[side], `content ${side}`));
  check(label, () => assert.ok(actual.pageWidth <= width, `page overflows: ${actual.pageWidth} > ${width}`));
  if (width > 760) {
    check(label, () => near(actual.rail.height, actual.outer.height, "full-height divider"));
    check(label, () => assert.equal(actual.railStyle.borderRightWidth, "1px", "desktop divider"));
  } else {
    check(label, () => assert.equal(actual.railStyle.flexDirection, board.railStyle.flexDirection, "mobile rail orientation"));
    check(label, () => near(actual.main.y - actual.rail.bottom, board.main.y - board.rail.bottom, "mobile rail/content spacing"));
    check(label, () => assert.ok(actual.rail.bottom <= actual.view.bottom && actual.main.height > 0, "mobile rail clips detail"));
  }
  if (actual.button && board.button) {
    if (width > 760)
      check(label, () => near(actual.button.rect.y - actual.rail.y, board.button.rect.y - board.rail.y, "first selector offset from rail top"));
    if (actual.button.text === board.button.text) for (const dimension of ["height", "width"])
      check(label, () => near(actual.button.rect[dimension], board.button.rect[dimension], `matching-content rail button ${dimension}`));
    for (const property of ["fontFamily", "fontSize", "lineHeight", "minHeight", "borderRadius", "paddingTop", "paddingLeft"])
      check(label, () => assert.equal(actual.button.style[property], board.button.style[property], `rail button ${property}`));
  }
  if (actual.heading && board.heading && scenario !== "empty") {
    for (const property of ["fontFamily", "fontSize", "fontWeight", "lineHeight"])
      check(label, () => assert.equal(actual.heading.style[property], board.heading.style[property], `heading ${property}`));
    for (const property of ["paddingTop", "paddingBottom", "alignItems"])
      check(label, () => assert.equal(actual.heading.rowStyle[property], board.heading.rowStyle[property], `heading row ${property}`));
    check(label, () => near(actual.heading.rect.x, board.heading.rect.x, "heading origin x"));
    // Compare the actual origin for comparable single-line desktop headings.
    if (width > 760 && scenario === "populated") {
      check(label, () => near(actual.heading.rect.y, board.heading.rect.y, "heading origin y"));
      if (width === 1024 && label.endsWith("-features")) {
        const heading = actual.heading.rect, row = actual.heading.rowRect;
        check(label, () => near(heading.height, board.heading.rect.height, "single-line Features heading height"));
        for (const [name, part] of Object.entries(actual.itemParts || {})) {
          check(label, () => assert.ok(part, `${name} missing from Features header`));
          if (part) check(label, () => assert.ok(part.y >= row.y - 1 && part.bottom <= row.bottom + 1, `${name} wraps outside Features header row`));
        }
      }
    }
  }
  for (const control of actual.controls)
    check(label, () => assert.ok(control.rect.x >= actual.view.x - 1 && control.rect.right <= actual.view.right + 1,
      `control clipped horizontally: ${control.text}`));
  if (label.endsWith('-features') && actual.itemParts) {
    const parts = Object.entries({heading:actual.heading?.rect,search:actual.itemParts.search,
      status:actual.itemParts.status,newButton:actual.itemParts.newButton}).filter(([,rect])=>rect);
    for (let i=0;i<parts.length;i++) for(let j=i+1;j<parts.length;j++) {
      const [leftName,left]=parts[i], [rightName,right]=parts[j];
      check(label, () => assert.ok(left.right <= right.x + 1 || right.right <= left.x + 1 ||
        left.bottom <= right.y + 1 || right.bottom <= left.y + 1,
        `${leftName} overlaps ${rightName}`));
    }
  }
  if (board.toolbar.length) for (const control of actual.toolbar) {
    check(label, () => near(control.rect.height, board.toolbar[0].rect.height, `control height: ${control.text}`));
    check(label, () => assert.equal(control.style.fontFamily, board.toolbar[0].style.fontFamily, `control font: ${control.text}`));
  }
}

async function selectedBehavior(page, mode, boardAppearance) {
  const choices = page.locator(`#mode-view > div > aside ${selectors[mode]}`);
  await choices.nth(1).click();
  await expect(choices.nth(1)).toHaveAttribute("aria-pressed", "true");
  const identity = await choices.nth(1).evaluate(el => ({text:el.textContent,attr:el.getAttributeNames().find(n=>n.startsWith('data-'))}));
  await show(page, mode === "board" ? "teams" : "board");
  await show(page, mode);
  await expect(choices.nth(1)).toHaveAttribute("aria-pressed", "true");
  await expect(choices.nth(1)).toHaveText(identity.text);
  await page.mouse.move(0, 0);
  const appearance = locator => locator.evaluate(el => {const s=getComputedStyle(el);return {background:s.backgroundColor,border:s.borderColor,color:s.color}});
  const selected = await appearance(choices.nth(1)), unselected = await appearance(choices.first());
  assert.notEqual(selected.background, unselected.background, `${mode}: selection must have fill`);
  await choices.first().hover();
  const hovered = await appearance(choices.first());
  assert.notEqual(hovered.border, unselected.border, `${mode}: hover did not outline`);
  if (boardAppearance) assert.deepEqual({selected,unselected,hovered}, boardAppearance, `${mode}: selection/hover differs from Board`);
  else if (hovered.background !== unselected.background) console.log("Inherited Board behavior: unselected hover also changes fill.");
  await page.mouse.move(0, 0);
  return {selected,unselected,hovered};
}

async function actions(page) {
  const called = async action => expect.poll(() => page.evaluate(action => qa.state.calls.filter(x=>x.action===action).length, action)).toBeGreaterThan(0);
  await show(page, "tasks");
  await page.locator(selectors.tasks).first().click();
  await page.locator("#tasks-new").click(); await called("new-project");
  await page.locator("[data-task-add]").click(); await called("add-agent");
  await page.locator("[data-handler-setup]").click(); await called("handler");
  for (const [selector, action] of [["[data-task-attach]", "attach"], ["[data-task-settings]", "settings"]]) {
    await page.locator("[data-task-more]").click();
    await page.locator(selector).click(); await called(action);
  }
  await page.locator("[data-task-board]").click(); await called("board");
  await expect(page.locator("#board-text")).toBeVisible();
  await page.locator("#board-text").fill("Synthetic retained layout draft");
  await page.evaluate(() => qa.state.failWrites = true);
  await page.locator("#board-compose button[type=submit]").click();
  await expect(page.locator("#notice")).toContainText("Post failed");
  await show(page, "teams"); await show(page, "board");
  await expect(page.locator("#board-text")).toHaveValue("Synthetic retained layout draft");
  await page.evaluate(() => qa.state.failWrites = false);

  await show(page, "tasks");
  await page.locator("details > summary").click();
  await page.locator(`${selectors.tasks}[data-task-select="tsk_3333333333333333"]`).click();
  await expect(page.locator("[data-task-board]")).toContainText("history");
  await page.locator("[data-task-cleanup]").click(); await called("cleanup");
  await page.locator("[data-task-board]").click();
  await expect(page.locator("#board-compose")).toHaveCount(0);
  const download = page.waitForEvent("download");
  await page.locator("#board-download").click();
  assert.match((await download).suggestedFilename(), /history.json$/);

  await show(page, "teams");
  await page.locator(selectors.teams).first().click();
  await page.locator("[data-new-task]").click(); await called("team-new-project");
  await page.locator("[data-add-team]").click(); await called("team-add-project");
  await page.locator("[data-edit-team]").click();
  await page.locator("#team-name").fill("Edited layout team");
  await page.locator("#team-form button[type=submit]").click();
  await expect(page.locator("#mode-view")).toContainText("Edited layout team");
  await page.locator("#teams-new").click();
  await page.locator("#team-name").fill("New layout team");
  await page.locator("#team-form button[type=submit]").click();
  await expect(page.locator("#mode-view")).toContainText("New layout team");
  await page.locator("[data-delete-team]").click();
  await expect(page.locator("#mode-view")).not.toContainText("New layout team");

  for (const mode of ["bugs", "features"]) {
    await show(page, mode);
    await page.locator('[data-items-scope="tsk_1111111111111111"]').click();
    await expect(page.locator("[data-work-item]")).toHaveCount(1);
    await page.locator("[data-items-status]").selectOption("done");
    await expect(page.locator("[data-work-item]")).toHaveCount(0);
    await page.locator("[data-items-status]").selectOption("");
    await page.locator("[data-items-new]").click();
    await page.locator("#work-item-title").fill(`New layout ${mode}`);
    await page.locator("#work-item-form button[type=submit]").click();
    await expect(page.locator("[data-work-item]")).toHaveCount(2);
    await page.locator("[data-item-edit]").filter({hasText:`New layout ${mode}`}).click();
    await page.locator("#work-item-title").fill(`Edited layout ${mode}`);
    await page.locator("#work-item-status").selectOption("in_progress");
    await page.locator("#work-item-form button[type=submit]").click();
    const item = page.locator("[data-work-item]").filter({hasText:`Edited layout ${mode}`});
    await expect(item).toContainText("In progress");
    const saved = await page.evaluate(title => qa.items.find(x => x.title === title), `Edited layout ${mode}`);
    assert.equal(saved.revision, 2, `${mode}: update revision not saved`);
    assert.equal(saved.status, "in_progress", `${mode}: update status not saved`);
    await item.locator("[data-item-send]").click();
    await page.locator("#work-item-dispatch button[type=submit]").click();
    await expect(page.locator("#dialog")).not.toBeVisible();
    await expect(page.locator("#notice")).toContainText("board message #2");
    const dispatched = await page.evaluate(title => qa.items.find(x => x.title === title).lastDispatch, `Edited layout ${mode}`);
    assert.equal(dispatched.messageSeq, 2, `${mode}: dispatch result not saved`);
  }
  await show(page, "tasks");
  await page.locator('[data-task-select="tsk_2222222222222222"]').click();
  await page.locator("[data-task-more]").click();
  await page.locator("[data-task-close]").click(); await called("close");
  await expect(page.locator("[data-task-board]")).toContainText("history");
  await page.locator('[data-task-select="tsk_1111111111111111"]').click();
  // Presentation remains usable offline with the current layout and local team.
  await page.evaluate(() => qa.offline());
  for (const mode of viewNames) {
    await show(page, mode);
    await expect(page.locator("#mode-view > div > aside")).toBeVisible();
    if (mode !== "teams") await expect(page.locator("#mode-view [role=status]").first()).toContainText("Offline · showing cached data");
  }
}

try {
  for (const engine of [chromium, webkit]) {
    const browser = await engine.launch();
    try {
      for (const width of [1440, 1024, 390]) for (const scenario of ["populated", "empty", "long"]) {
        const context = await browser.newContext({ viewport: {width,height:900} });
        const page = await context.newPage(), errors=[];
        page.on("pageerror",error=>errors.push(error.message));
        await context.route("**/*",route=>new URL(route.request().url()).origin===origin?route.continue():route.abort());
        const prefix=`${engine.name()}-${width}-${scenario}`;
        try {
          await page.goto(`${origin}/board-layout-test?scenario=${scenario}`);
          await page.waitForFunction(()=>!!window.qa);
          let board;
          for (const mode of viewNames) {
            await show(page,mode);
            const measured=await geometry(page,mode);
            if(mode==='board')board=measured;
            compare(measured,board,width,`${prefix}-${mode}`,scenario);
            const screenshot=`board-layout-${prefix}-${mode}.png`;
            await page.screenshot({path:'.build/'+screenshot});
            results.push({engine:engine.name(),width,scenario,mode,screenshot,geometry:measured});
          }
          if(scenario==='populated'){
            let boardAppearance;
            for(const mode of viewNames){
              try {await show(page,mode);const appearance=await selectedBehavior(page,mode,boardAppearance);if(mode==='board')boardAppearance=appearance}
              catch(error){failures.push(`${prefix}-${mode}: ${error.message}`);console.error(failures.at(-1))}
            }
            await actions(page);
          }
          assert.deepEqual(errors,[],`${prefix}: browser errors`);
          console.log(`${prefix}: geometry/screenshots and applicable selection/actions complete`);
        } catch(error){
          failures.push(`${prefix}: ${error.stack||error.message}`);console.error(failures.at(-1));
          await page.screenshot({path:'.build/board-layout-'+prefix+'-failure.png'}).catch(()=>{});
        } finally {await context.close()}
      }
    } finally {await browser.close()}
  }
} finally {
  await server.close();
  await writeFile('.build/board-layout-results.json',JSON.stringify({results,failures,skips},null,2));
  await writeFile('.build/board-layout-report.html','<!doctype html><meta charset="utf-8"><title>Board layout comparison</title><style>body{font:14px system-ui;background:#151916;color:#ddd}section{display:grid;grid-template-columns:repeat(5,minmax(0,1fr));gap:8px}img{width:100%}h2{grid-column:1/-1}figure{margin:0}figcaption{padding:5px}</style>'+[...new Set(results.map(r=>r.engine+' '+r.width+' '+r.scenario))].map(group=>'<section><h2>'+group+'</h2>'+results.filter(r=>r.engine+' '+r.width+' '+r.scenario===group).map(r=>'<figure><figcaption>'+r.mode+'</figcaption><a href="'+r.screenshot+'"><img src="'+r.screenshot+'"></a></figure>').join('')+'</section>').join(''));
}
if(failures.length)throw Error(`Board layout acceptance: ${failures.length} failures; see .build/board-layout-results.json`);
console.log(`PASS: Chromium/WebKit Board layout comparison at 1440, 1024, and 390px; ${skips.length} linked heading checks skipped`);
