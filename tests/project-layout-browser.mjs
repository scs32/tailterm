// Actual pane renderer and encrypted disposable vault. No hub, SSH or user data.
import assert from "node:assert/strict";
import { chromium, webkit } from "@playwright/test";
import { createServer } from "vite";

const html = `<!doctype html><html><head><link rel="stylesheet" href="/client/style.css"></head>
<body><div id="app"><section class="terminal-shell" style="margin:20px"><div class="terminal-tabs"><div id="tabs"></div></div><div id="terminal-body" style="height:700px;position:relative"></div></section></div>
<script type="module">
import {setupPaneGroups} from '/client/pane-groups.js';
import {workspaceSnapshot,normalizeWorkspace} from '/client/workspace-state.js';
import * as vault from '/client/local-vault.js';
const taskId='tsk_1111111111111111';
const agentIds=Array.from({length:5},(_,i)=>'agt_'+String(i+1).padStart(16,'0'));
const server={id:'fixture-server',host:'synthetic.invalid',port:22,username:'fixture'};
await vault.localAPI('/unlock','POST',{password:'Synthetic project layout passphrase'});
const saved=normalizeWorkspace(vault.localData().workspace);
const tabs=[];let active=null,loading=true,writes=0,queue=Promise.resolve();
function flush(){
  if(loading)return queue;
  const snapshot=workspaceSnapshot(tabs,groups.model.groups,active,null,[taskId],[],groups.model.projectLayoutSnapshot());
  queue=queue.then(()=>vault.saveWorkspace(snapshot)).then(()=>{writes++});
  return queue;
}
const groups=setupPaneGroups({getTabs:()=>tabs,getActive:()=>active,activate(id){active=id;if(!loading)groups.model.rememberActive(id);groups.render();flush()},close(){},preferences:()=>({}),label:t=>t.session,changed:()=>{void flush()}});
groups.model.loadProjectLayouts(saved?.projectLayouts||[]);
groups.model.setTaskMembers(taskId,agentIds);
const prefix=crypto.randomUUID();
function add(index){
  const id=prefix+'-'+index,el=document.createElement('div');el.className='terminal-instance';document.querySelector('#terminal-body').append(el);
  const tab={id,el,task:{taskId,agentId:agentIds[index]},server,status:'Connected',tmux:true,session:'agent-'+index,target:{id:'$'+index,created:'1234'}};
  tabs.push(tab);if(!active)active=id;
  groups.model.setTaskOrchestrator(taskId,tabs.find(t=>t.task.agentId===agentIds[0])?.id);
  groups.sync();groups.render();return tab;
}
// Resume in a different connection order with entirely new browser pane IDs.
for(const index of saved?[4,2,0,3,1]:[0,1,2,3,4]){add(index);await new Promise(r=>setTimeout(r,15))}
active=groups.model.taskGroup(taskId)?.active||active;groups.render();loading=false;
function material(){
  const group=groups.model.taskGroup(taskId);
  const agent=id=>tabs.find(t=>t.id===id)?.task.agentId;
  const tree=node=>node.tab?{agentId:agent(node.tab)}:{axis:node.axis,ratio:node.ratio,a:tree(node.a),b:tree(node.b)};
  return group?{tree:tree(group.tree),activeAgentId:agent(group.active),taskLayout:group.taskLayout}:null;
}
window.fixture={groups,tabs,agentIds,taskId,vault,flush,material,get writes(){return writes},
  pane(index){return tabs.find(t=>t.task.agentId===agentIds[index])?.id},
  async hideAll(){await flush();loading=true;tabs.splice(0).forEach(t=>t.el.remove());active=null;groups.sync();groups.render();loading=false;await flush()},
  async remove(index){groups.model.setTaskMembers(taskId,agentIds.filter((_,i)=>i!==index));const tab=tabs.find(t=>t.task.agentId===agentIds[index]);tabs.splice(tabs.indexOf(tab),1);tab.el.remove();groups.sync();if(active===tab.id)active=groups.model.taskGroup(taskId)?.active;groups.render();await flush()}
};
</script></body></html>`;

const vite = await createServer({
  configFile: false,
  root: process.cwd(),
  server: { host: "127.0.0.1", port: 0 },
  plugins: [{ name: "project-layout-fixture", configureServer(server) {
    server.middlewares.use("/project-layout-test", (_req, res) => {
      res.setHeader("Content-Type", "text/html");
      res.end(html);
    });
  } }],
});
await vite.listen();
const origin = `http://127.0.0.1:${vite.httpServer.address().port}`;
try {
  for (const engine of [chromium, webkit]) {
    const browser = await engine.launch();
    try {
      const context = await browser.newContext({ viewport: { width: 1440, height: 1000 } });
      await context.route("**/*", route => new URL(route.request().url()).origin === origin ? route.continue() : route.abort());
      const page = await context.newPage(), errors = [];
      page.on("pageerror", error => errors.push(error.message));
      await page.goto(origin + "/project-layout-test");
      await page.waitForFunction(() => !!window.fixture);
      await page.evaluate(() => fixture.groups.place(fixture.pane(4), fixture.pane(1), "above"));
      const divider = page.locator('.pane-divider[data-axis="x"]').first();
      await divider.focus();
      await page.keyboard.press("ArrowRight");
      await page.keyboard.press("ArrowRight");
      const focusId = await page.evaluate(() => fixture.pane(3));
      await page.locator(`[data-pane="${focusId}"] .pane-label`).click();
      await page.evaluate(() => fixture.flush());
      const expected = await page.evaluate(() => fixture.material());
      assert.equal(expected.taskLayout, "manual");
      assert.equal(expected.activeAgentId, "agt_0000000000000004");
      const beforeIDs = await page.evaluate(() => fixture.tabs.map(t => t.id));
      await page.reload();
      await page.waitForFunction(() => !!window.fixture);
      assert.deepEqual(await page.evaluate(() => fixture.material()), expected, "encrypted reload changed project arrangement");
      assert.ok((await page.evaluate(() => fixture.tabs.map(t => t.id))).every(id => !beforeIDs.includes(id)), "fixture must restore with new pane IDs");
      await page.evaluate(() => fixture.hideAll());
      assert.equal(await page.locator(".pane-header").count(), 0);
      await page.reload();
      await page.waitForFunction(() => !!window.fixture);
      assert.deepEqual(await page.evaluate(() => fixture.material()), expected, "empty workspace discarded retained project arrangement");
      await page.evaluate(() => fixture.remove(2));
      const layouts = await page.evaluate(() => fixture.groups.model.projectLayoutSnapshot());
      assert.ok(!JSON.stringify(layouts).includes("agt_0000000000000003"), "authoritatively removed member remains in template");
      assert.deepEqual(errors, []);
      console.log(`${engine.name()}: encrypted exact layout reload, changed pane IDs, out-of-order arrivals, resize/focus, hide-all/reopen and member deletion passed`);
      await context.close();
    } finally { await browser.close(); }
  }
} finally { await vite.close(); }
