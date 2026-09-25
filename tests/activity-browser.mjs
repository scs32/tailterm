// Synthetic Projects and Delivery activity fixture. No live hub or profile.
import assert from "node:assert/strict";
import { chromium, webkit } from "@playwright/test";
import { createServer } from "vite";

const html = `<!doctype html><html><head><link rel="stylesheet" href="/client/style.css"><link rel="stylesheet" href="/client/work-items.css"></head><body><main id="projects"></main><script type="module">
import {createTasksView} from '/client/tasks-view.js';
const states=['working','hung_tool','finished_silent','crashed','looping','idle','unknown'];
const task={id:'tsk_1111111111111111',name:'Synthetic activity project',goal:'Synthetic only',status:'open',createdAt:'2026-09-25T20:00:00Z'};
const agents=states.map((state,i)=>({id:'agt_'+String(i+1).padStart(16,'0'),name:'member-'+i,host:'fixture.invalid',session:'fake',status:'running',runId:'run_'+String(i+1).padStart(16,'0'),lastSeenAt:'2026-09-25T20:00:00Z',createdAt:'2026-09-25T20:00:00Z',activity:{state,observedAt:new Date().toISOString(),pendingTool:state==='hung_tool'?'exec_command':'',tokens:{total:100+i}}}));
const queue={concurrencyLimit:1,entries:[{itemId:'wi_1111111111111111',state:'running',ownership:['synthetic'],activities:agents.map(a=>({agentId:a.id,name:a.name,activity:a.activity})),tokens:{total:721}}]};
const client={listTasks:async()=>[task],getTask:async()=>({task,agents}),listTeamDelivery:async()=>queue,capabilities:async()=>({}),subscribe:()=>({stop(){}})};
const view=createTasksView({client:()=>client,taskHub:{hasPendingResume:()=>false,groupOf:()=>null},getTabs:()=>[],activate:()=>{},notice:()=>{},confirm:async()=>true,openBoard:()=>{},openWorkItems:()=>{},configure:()=>{}});
view.mount(document.querySelector('#projects'));
window.ready=view.show();
</script></body></html>`;

const vite = await createServer({configFile:false,root:process.cwd(),server:{host:"127.0.0.1",port:0},plugins:[{name:"activity-fixture",configureServer(server){server.middlewares.use("/activity-test",(_req,res)=>{res.setHeader("Content-Type","text/html");res.end(html)})}}]});
await vite.listen();
const origin=`http://127.0.0.1:${vite.httpServer.address().port}`;
try {
  for(const engine of [chromium,webkit]) {
    const browser=await engine.launch();
    try {
      const context=await browser.newContext({viewport:{width:1280,height:800}});
      await context.route("**/*",route=>new URL(route.request().url()).origin===origin?route.continue():route.abort());
      const page=await context.newPage(),errors=[];
      page.on("pageerror",error=>errors.push(error.message));
      await page.goto(origin+"/activity-test");
      await page.evaluate(()=>window.ready);
      await page.locator('[data-team-toggle]').click();
      const roster=await page.locator('[data-team-roster]').innerText();
      for(const state of ["Working","Hung tool","Finished silently","Crashed","Looping","Idle","Unknown"]) assert.match(roster,new RegExp(state));
      assert.match(await page.locator('[data-task-agent]').first().getAttribute('title'),/Last transition snapshot: 100 tokens/);
      assert.match(await page.locator('[data-testid="team-delivery-panel"]').innerText(),/Last transition snapshot: 721 tokens/);
      assert.deepEqual(errors,[]);
      await context.close();
      console.log(`${engine.name()}: synthetic Projects activity and Delivery totals rendered`);
    } finally { await browser.close() }
  }
} finally { await vite.close() }
