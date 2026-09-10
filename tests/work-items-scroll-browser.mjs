// Acceptance for wi_72084e89ad88fd3f revision 2, work order #1294.
// Uses only an in-memory client and disposable browser contexts.
import { chromium, webkit } from "@playwright/test";
import { createServer } from "vite";
import assert from "node:assert/strict";

const expectReplacementBaseline =
  process.env.WORK_ITEMS_SCROLL_REPLACEMENT_BASELINE === "1";

const html = `<!doctype html><html><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<link rel="stylesheet" href="/client/style.css">
<link rel="stylesheet" href="/client/work-items.css">
<style>
html,body{height:100%;margin:0} body{padding:20px;box-sizing:border-box}
#mode-view{display:flex;height:640px;overflow:hidden}
</style></head><body><main id="mode-view"></main><dialog id="dialog"></dialog>
<p id="notice"></p><script type="module">
import {createWorkItemsView} from '/client/work-items-view.js';
const kind=new URLSearchParams(location.search).get('kind')||'bug';
const tasks=[
  {id:'tsk_alpha',name:'Alpha project',goal:'Primary scroll fixture',status:'open',orchestrator:'lead'},
  {id:'tsk_beta',name:'Beta project',goal:'Secondary scroll fixture',status:'open',orchestrator:'lead'},
];
const item=(index,status='open',prefix='Current')=>({
  id:'wi_'+kind+'_'+String(index).padStart(3,'0'),
  taskId:index%2?'tsk_alpha':'tsk_beta',kind,
  title:prefix+' '+kind+' '+index+' '+'reading anchor '.repeat((index%3)+1),
  description:'Isolated fixture record',status,priority:index%4===0?'high':'normal',revision:2,
});
const state={items:[...Array.from({length:52},(_,index)=>item(index+1)),...Array.from({length:8},(_,index)=>item(index+101,'done'))],listCalls:[]};
const client={
  async listTasks(){return tasks.map(task=>({...task}))},
  async listWorkItems(query){
    state.listCalls.push({...query});
    const filtered=state.items.filter(entry=>entry.kind===query.kind&&(!query.taskId||entry.taskId===query.taskId)&&(!query.status||entry.status===query.status));
    const start=Number(query.after)||0,end=Math.min(start+14,filtered.length);
    return {items:filtered.slice(start,end).map(entry=>({...entry})),next:end<filtered.length?end:0};
  },
  async listWorkItemRevisions(_taskId,itemId){const entry=state.items.find(candidate=>candidate.id===itemId);return {revisions:[{...entry,changeKind:'created',updatedAt:'2026-09-09T12:00:00Z',provenance:'native'}],nextAfter:0}},
  async listWorkItemHistoryGaps(){return {gaps:[],nextAfter:0}},
  async listWorkItemMessages(){return {links:[],nextAfter:0}},
  subscribe(){return {stop(){}}},cacheStatus(){return {label:'Fixture current'}},
};
const dialogNode=document.querySelector('#dialog');
const closeDialog=()=>{dialogNode.close();dialogNode.replaceChildren()};
const dialog=(title,body)=>{if(dialogNode.open)dialogNode.close();dialogNode.innerHTML='<h2>'+title+'</h2>'+body;dialogNode.showModal()};
const root=document.querySelector('#mode-view');
const view=createWorkItemsView({kind,client:()=>client,dialog,closeDialog,notice:text=>document.querySelector('#notice').textContent=text,configure(){},openBoard(){}});
view.mount(root);await view.show();
const main=()=>document.querySelector('.work-items-main');
const rows=()=>[...document.querySelectorAll('[data-work-item]')];
const filteredCount=()=>state.items.filter(entry=>entry.kind===kind&&(!document.querySelector('[data-items-project]')?.value||entry.taskId===document.querySelector('[data-items-project]').value)&&(!document.querySelector('[data-items-status]')?.value||entry.status===document.querySelector('[data-items-status]').value)).length;
const viewport=()=>{const container=main(),bounds=container.getBoundingClientRect(),visible=rows().find(row=>row.getBoundingClientRect().bottom>bounds.top),rect=visible?.getBoundingClientRect();return {id:visible?.dataset.workItem||'',top:rect?rect.top-bounds.top:0,scrollTop:container.scrollTop,scrollHeight:container.scrollHeight,clientHeight:container.clientHeight,rowCount:rows().length,stateCount:filteredCount(),status:document.querySelector('[data-items-status]')?.value||'',project:document.querySelector('[data-items-project]')?.value||'',focused:document.activeElement?.dataset?.viewControl||document.activeElement?.dataset?.itemEdit||document.activeElement?.tagName||''}};
const place=(id,top=-10)=>{const container=main(),row=document.querySelector('[data-work-item="'+id+'"]'),bounds=container.getBoundingClientRect(),rect=row.getBoundingClientRect();container.scrollTop+=rect.top-bounds.top-top;return viewport()};
let watchedMain=null,watchedFocus=null;
const watchScroll=()=>{watchedMain=main();watchedFocus=document.activeElement;return scrollWatch()};
const scrollWatch=()=>({sameMain:watchedMain===main(),mainConnected:Boolean(watchedMain?.isConnected),watchedScrollTop:watchedMain?.scrollTop||0,sameFocus:watchedFocus===document.activeElement,focusConnected:Boolean(watchedFocus?.isConnected),...viewport()});
window.qa={state,view,viewport,place,watchScroll,scrollWatch,
  focusVisible(){const row=document.querySelector('[data-work-item="'+viewport().id+'"]'),control=row?.querySelector('[data-item-edit]');control?.focus();return viewport()},
  async prepend(){state.items.unshift(item(900+state.items.length,'open','Newest incoming with extra height'));await view.reload()},
  smoothScroll(distance=560){const watched=watchScroll();watchedMain.scrollBy({top:distance,behavior:'smooth'});return watched},
  moveWatched(distance=120){watchedMain.scrollTop+=distance;return viewport()},
  resetCalls(){state.listCalls=[]},
};
</script></body></html>`;

const server = await createServer({
  configFile: false,
  root: process.cwd(),
  server: { host: "127.0.0.1", port: 0 },
  plugins: [{
    name: "work-items-scroll-fixture",
    configureServer(vite) {
      vite.middlewares.use("/work-items-scroll-test", (_request, response) => {
        response.setHeader("Content-Type", "text/html");
        response.end(html);
      });
    },
  }],
});
await server.listen();
const origin = `http://127.0.0.1:${server.httpServer.address().port}`;
const near = (actual, expected, label) =>
  assert.ok(Math.abs(actual - expected) <= 1, `${label}: expected ${expected}, received ${actual}`);
const sameAnchor = (before, after, label) => {
  assert.equal(
    after.id,
    before.id,
    `${label}: visible item changed: ${JSON.stringify({ before, after })}`,
  );
  near(after.top, before.top, `${label}: visible item offset`);
};

try {
  for (const engine of [chromium, webkit]) {
    const browser = await engine.launch();
    const context = await browser.newContext({viewport:{width:1000,height:760}});
    const page = await context.newPage();
    const errors=[];
    page.on('pageerror',error=>errors.push(error.message));
    try {
      for (const kind of ['bug','feature']) {
        const label=`${engine.name()} ${kind}`;
        await page.goto(`${origin}/work-items-scroll-test?kind=${kind}`);
        await page.waitForFunction(()=>Boolean(window.qa));
        await page.locator('[data-items-status]').selectOption('open');
        await page.waitForFunction(()=>qa.viewport().rowCount===qa.viewport().stateCount);
        const calls=await page.evaluate(()=>qa.state.listCalls.map(call=>({...call})));
        assert.ok(calls.some(call=>call.status==='open'&&call.after===42),`${label}: filtered pagination did not reach the fourth page`);

        await page.evaluate(id=>qa.place(id),`wi_${kind}_025`);
        await page.waitForTimeout(180);
        await page.locator('[data-items-status]').focus();
        const focusedBeforeWheel=await page.evaluate(()=>qa.viewport());
        const watched=await page.evaluate(()=>qa.watchScroll());
        const box=await page.locator('.work-items-main').boundingBox();
        assert.ok(box,`${label}: missing scrolling viewport`);
        await page.mouse.move(box.x+box.width/2,box.y+box.height/2);
        await page.mouse.wheel(0,180);
        await page.waitForFunction(scrollTop=>qa.scrollWatch().watchedScrollTop>scrollTop,watched.watchedScrollTop);
        const beforeActiveRefresh=await page.evaluate(()=>qa.scrollWatch());
        await page.evaluate(()=>qa.prepend());
        const duringActiveRefresh=await page.evaluate(()=>qa.scrollWatch());
        if(expectReplacementBaseline){
          assert.equal(duringActiveRefresh.sameMain,false,`${label}: baseline refresh unexpectedly retained the scrolling element`);
          assert.equal(duringActiveRefresh.mainConnected,false);
          assert.equal(duringActiveRefresh.sameFocus,false);
          assert.equal(duringActiveRefresh.focusConnected,false);
          console.log(`${label}: baseline active refresh replaced the scrolling element and focused filter`);
          continue;
        }
        assert.equal(duringActiveRefresh.sameMain,true,`${label}: active refresh replaced the scrolling element`);
        assert.equal(duringActiveRefresh.mainConnected,true);
        assert.equal(duringActiveRefresh.sameFocus,true);
        assert.equal(duringActiveRefresh.focusConnected,true);
        assert.equal(duringActiveRefresh.rowCount,duringActiveRefresh.stateCount-1,`${label}: active refresh was not held`);
        await page.mouse.wheel(0,180);
        await page.waitForFunction(scrollTop=>qa.scrollWatch().watchedScrollTop>scrollTop,beforeActiveRefresh.watchedScrollTop);
        const beforeIdleRefresh=await page.evaluate(()=>qa.viewport());
        await page.waitForFunction(()=>qa.viewport().rowCount===qa.viewport().stateCount);
        const afterIdleRefresh=await page.evaluate(()=>qa.viewport());
        sameAnchor(beforeIdleRefresh,afterIdleRefresh,`${label} active-scroll idle refresh`);
        assert.equal(afterIdleRefresh.status,'open');
        assert.equal(afterIdleRefresh.focused,focusedBeforeWheel.focused);
        console.log(`${label}: wheel sequence retained DOM/focus, continued scrolling, then repainted latest state at idle`);

        await page.evaluate(id=>qa.place(id),`wi_${kind}_021`);
        await page.waitForTimeout(180);
        const beforeOrdinary=await page.evaluate(()=>qa.viewport());
        await page.evaluate(()=>qa.prepend());
        sameAnchor(beforeOrdinary,await page.evaluate(()=>qa.viewport()),`${label} ordinary refresh`);

        await page.evaluate(id=>qa.place(id),`wi_${kind}_019`);
        await page.waitForTimeout(180);
        const focusedBeforeSmooth=await page.evaluate(()=>qa.focusVisible());
        const smoothStart=await page.evaluate(()=>qa.smoothScroll());
        await page.waitForFunction(scrollTop=>qa.scrollWatch().watchedScrollTop>scrollTop+10,smoothStart.watchedScrollTop);
        const beforeSmoothRefresh=await page.evaluate(()=>qa.scrollWatch());
        await page.evaluate(()=>qa.prepend());
        const duringSmoothRefresh=await page.evaluate(()=>qa.scrollWatch());
        assert.equal(duringSmoothRefresh.sameMain,true,`${label}: smooth motion refresh replaced the scrolling element`);
        assert.equal(duringSmoothRefresh.rowCount,duringSmoothRefresh.stateCount-1);
        await page.waitForFunction(scrollTop=>qa.scrollWatch().watchedScrollTop>scrollTop+20,beforeSmoothRefresh.watchedScrollTop);
        await page.waitForFunction(()=>qa.viewport().rowCount===qa.viewport().stateCount);
        assert.equal((await page.evaluate(()=>qa.viewport())).focused,focusedBeforeSmooth.focused);
        console.log(`${label}: browser-managed smooth-motion surrogate continued through refresh and repainted at idle`);

        await page.evaluate(id=>qa.place(id),`wi_${kind}_023`);
        await page.waitForTimeout(180);
        const focusedBeforeTouch=await page.evaluate(()=>qa.focusVisible());
        const touchWatch=await page.evaluate(()=>qa.watchScroll());
        await page.locator('.work-items-main').dispatchEvent('touchstart');
        await page.evaluate(()=>qa.prepend());
        const duringTouchRefresh=await page.evaluate(()=>qa.scrollWatch());
        assert.equal(duringTouchRefresh.sameMain,true,`${label}: synthetic touch hold replaced the scrolling element`);
        assert.equal(duringTouchRefresh.rowCount,duringTouchRefresh.stateCount-1);
        const beforeTouchIdle=await page.evaluate(()=>qa.moveWatched());
        assert.ok(beforeTouchIdle.scrollTop>touchWatch.scrollTop);
        await page.locator('.work-items-main').dispatchEvent('touchend');
        await page.waitForFunction(()=>qa.viewport().rowCount===qa.viewport().stateCount);
        sameAnchor(beforeTouchIdle,await page.evaluate(()=>qa.viewport()),`${label} synthetic touch idle refresh`);
        assert.equal((await page.evaluate(()=>qa.viewport())).focused,focusedBeforeTouch.focused);

        await page.locator('[data-item-history]').first().click();
        await page.waitForSelector('.work-item-history [data-history-detail-revision]');
        await page.evaluate(()=>{document.querySelector('#dialog').close();document.querySelector('#dialog').replaceChildren()});
        await page.locator('[data-item-send]').first().click();
        await page.waitForSelector('#work-item-dispatch');
        assert.equal(await page.locator('#work-item-target').inputValue(),'tsk_alpha');
        assert.deepEqual(errors,[],`${label}: browser errors`);
        console.log(`${label}: filter pagination, stable anchors, synthetic touch lifecycle, history, and dispatch remain wired`);
      }
    } finally {
      await context.close();
      await browser.close();
    }
  }
} finally {
  await server.close();
}
