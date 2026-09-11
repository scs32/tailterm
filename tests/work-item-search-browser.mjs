// Acceptance for wi_1b3c05505232a68e revision 1, work order #2213.
// Uses only synthetic in-memory records and disposable browser contexts.
import { chromium, webkit } from "@playwright/test";
import { createServer } from "vite";
import assert from "node:assert/strict";

const html = `<!doctype html><html><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<link rel="stylesheet" href="/client/style.css"><link rel="stylesheet" href="/client/work-items.css">
<style>html,body{height:100%;margin:0}#mode-view{display:flex;height:100%}</style></head>
<body><main id="mode-view"></main><dialog id="dialog"></dialog><p id="notice"></p>
<script type="module">
import {createWorkItemsView} from '/client/work-items-view.js';
const kind=new URLSearchParams(location.search).get('kind')||'bug';
const task={id:'tsk_fixture',name:'Synthetic project',status:'open',goal:'isolated'};
const items=[
 {id:'wi_current',taskId:task.id,kind,title:'Visible alpha',description:'Current writeup needle',status:'open',priority:'normal',revision:2},
 {id:'wi_history',taskId:task.id,kind,title:'Visible beta',description:'Present text',status:'open',priority:'high',revision:2},
];
const client={base:'isolated://search',token:'fixture',cacheStatus:()=>({label:'Fixture current'}),subscribe:()=>({stop(){}}),
 async listTasks(){return [task]},async listWorkItems(){return {items:items.map(item=>({...item})),next:0}},
 async listWorkItemRevisions(_task,id){
  return {revisions:id==='wi_history'?[{...items[1],revision:1,title:'Retired gamma title',description:'Archived delta description'},{...items[1]}]:[{...items[0]}],nextAfter:0};
 },
 async listWorkItemMessages(_task,id,{revision}){return {links:id==='wi_history'&&revision===1?[{message:{text:'Board epsilon evidence'}}]:[],nextAfter:0}},
};
const dialogNode=document.querySelector('#dialog');
const closeDialog=()=>{dialogNode.close();dialogNode.replaceChildren()};
const dialog=(title,body)=>{dialogNode.innerHTML='<h2>'+title+'</h2>'+body;dialogNode.showModal()};
const view=createWorkItemsView({kind,client:()=>client,dialog,closeDialog,notice(){},configure(){},openBoard(){}});
view.mount(document.querySelector('#mode-view'));await view.show();window.qa={view};
</script></body></html>`;

const server = await createServer({
  configFile: false,
  root: process.cwd(),
  server: { host: "127.0.0.1", port: 0 },
  plugins: [
    {
      name: "work-item-search-fixture",
      configureServer(vite) {
        vite.middlewares.use("/work-item-search-test", (_req, res) => {
          res.setHeader("Content-Type", "text/html");
          res.end(html);
        });
      },
    },
  ],
});
await server.listen();
const origin = `http://127.0.0.1:${server.httpServer.address().port}`;
try {
  for (const engine of [chromium, webkit]) {
    const browser = await engine.launch();
    try {
      for (const kind of ["bug", "feature"]) {
        const page = await browser.newPage();
        const errors = [];
        page.on("pageerror", (error) => errors.push(error.message));
        await page.goto(`${origin}/work-item-search-test?kind=${kind}`);
        await page.waitForFunction(() => Boolean(window.qa));
        const search = page.locator("[data-items-search]");
        await search.fill("writeup needle");
        await page.waitForFunction(
          () => document.querySelectorAll("[data-work-item]").length === 1,
        );
        assert.equal(
          await page.locator("[data-work-item]").getAttribute("data-work-item"),
          "wi_current",
        );
        await search.fill("archived delta");
        await page.waitForFunction(() =>
          document
            .querySelector("[data-items-search-status]")
            .textContent.includes("1 match"),
        );
        assert.equal(
          await page.locator("[data-work-item]").getAttribute("data-work-item"),
          "wi_history",
        );
        await search.fill("epsilon evidence");
        await page.waitForFunction(() =>
          document
            .querySelector("[data-items-search-status]")
            .textContent.includes("1 match"),
        );
        assert.equal(
          await page.locator("[data-work-item]").getAttribute("data-work-item"),
          "wi_history",
        );
        await search.fill("no such record");
        await page.waitForFunction(
          () => document.querySelectorAll("[data-work-item]").length === 0,
        );
        assert.match(
          await page.locator(".work-items-empty").innerText(),
          /No (bugs|features) match this search/,
        );
        await search.fill("");
        await page.waitForFunction(
          () => document.querySelectorAll("[data-work-item]").length === 2,
        );
        await page.locator('[data-item-edit="wi_current"]').click();
        assert.equal(
          await page.locator("#work-item-title").inputValue(),
          "Visible alpha",
        );
        assert.deepEqual(errors, [], `${engine.name()} ${kind} page errors`);
        await page.close();
      }
    } finally {
      await browser.close();
    }
  }
} finally {
  await server.close();
}
console.log(
  "Chromium/WebKit Bugs/Features live search, history, clearing, empty state, and editor navigation passed.",
);
