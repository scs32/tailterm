import { chromium, webkit, expect } from "@playwright/test";
import assert from "node:assert/strict";
import { mkdir, writeFile } from "node:fs/promises";
import { createServer } from "vite";

const html = `<!doctype html><html><head><meta charset="utf-8">
<link rel="stylesheet" href="/client/style.css"><link rel="stylesheet" href="/client/work-items.css">
</head><body><main id="mode-view"></main><dialog id="dialog"></dialog><p id="notice" role="status"></p>
<script type="module">
import {createWorkItemsView} from '/client/work-items-view.js';
const kind=new URLSearchParams(location.search).get('kind')||'bug';
const tasks=[
  {id:'tsk_source',name:'Source project',goal:'Source',status:'open',orchestrator:'lead'},
  {id:'tsk_target',name:'Target project',goal:'Target',status:'open',orchestrator:'worker'},
];
const item={id:'item_1',taskId:'tsk_source',kind,title:'Synthetic '+kind,status:'open',priority:'normal',revision:3};
const state={outcome:'success',calls:[],notices:[],openBoard:[],pending:[],settled:0};
const result=body=>({dispatch:{targetTaskId:body.targetTaskId,messageSeq:40+state.calls.length},item:{...item,lastDispatch:{targetTaskId:body.targetTaskId}}});
const client={
  async listTasks(){return tasks.map(task=>({...task}))},
  async listWorkItems(){return {items:[{...item}],next:0}},
  subscribe(){return {stop(){}}},
  cacheStatus(){return {label:'Synthetic live data'}},
  async dispatchWorkItem(taskId,itemId,body){
    state.calls.push({taskId,itemId,body:{...body}});
    let outcome=state.outcome;
    if(outcome==='pending')outcome=await new Promise(resolve=>state.pending.push(resolve));
    if(outcome==='failure')throw new Error('Synthetic dispatch rejected');
    item.lastDispatch={targetTaskId:body.targetTaskId};state.settled++;
    return result(body);
  },
};
const root=document.querySelector('#mode-view'),dialogNode=document.querySelector('#dialog');
function closeDialog(){dialogNode.close();dialogNode.replaceChildren()}
function dialog(title,body){
  if(dialogNode.open)dialogNode.close();
  dialogNode.innerHTML='<div class="dialog-head"><h2>'+title+'</h2><button id="dialog-close" type="button">×</button></div>'+body;
  dialogNode.querySelector('#dialog-close').onclick=closeDialog;dialogNode.showModal();
}
const view=createWorkItemsView({kind,client:()=>client,dialog,closeDialog,
  notice:text=>{state.notices.push(text);document.querySelector('#notice').textContent=text},
  configure(){},openBoard:id=>state.openBoard.push(id)});
view.mount(root);await view.show();
window.qa={state,
  outcome(value){state.outcome=value},
  resolve(value='success'){state.pending.shift()?.(value)},
  replacement(){dialog('Replacement dialog','<p id="replacement-dialog">Replacement remains open</p>')},
};
</script></body></html>`;

const server = await createServer({
  configFile: false,
  root: process.cwd(),
  server: { host: "127.0.0.1", port: 0 },
  plugins: [
    {
      name: "work-item-dispatch-fixture",
      configureServer(vite) {
        vite.middlewares.use("/work-item-dispatch", (_request, response) => {
          response.setHeader("Content-Type", "text/html");
          response.end(html);
        });
      },
    },
  ],
});
await server.listen();
const origin = `http://127.0.0.1:${server.httpServer.address().port}/work-item-dispatch`;
const results = [];
const failures = [];

async function load(page, kind) {
  await page.goto(`${origin}?kind=${kind}`);
  await page.waitForFunction(() => Boolean(window.qa));
}

async function openDispatch(page, target = "tsk_target") {
  await page.locator("[data-item-send]").click();
  await page.locator("#work-item-target").selectOption(target);
}

try {
  for (const engine of [chromium, webkit]) {
    const browser = await engine.launch();
    const context = await browser.newContext({
      viewport: { width: 900, height: 700 },
    });
    const page = await context.newPage();
    await context.route("**/*", (route) =>
      new URL(route.request().url()).origin === new URL(origin).origin
        ? route.continue()
        : route.abort(),
    );
    try {
      for (const kind of ["bug", "feature"]) {
        const label = `${engine.name()} ${kind}`;

        await load(page, kind);
        await openDispatch(page);
        await page
          .locator("#work-item-dispatch")
          .evaluate((form) => form.requestSubmit());
        await expect(page.locator("#dialog")).not.toBeVisible();
        await expect(page.locator("#notice")).toContainText(
          `${kind === "bug" ? "Bug" : "Feature"} sent to Target project`,
        );
        let state = await page.evaluate(() => qa.state);
        assert.deepEqual(state.calls[0].body.targetTaskId, "tsk_target");
        assert.equal(state.calls[0].body.revision, 3);
        assert.equal(
          state.openBoard.length,
          0,
          `${label}: navigated on success`,
        );
        results.push(`${label}: success closes with durable notice`);

        await load(page, kind);
        await page.evaluate(() => qa.outcome("failure"));
        await openDispatch(page, "tsk_source");
        await page
          .locator("#work-item-dispatch")
          .evaluate((form) => form.requestSubmit());
        await expect(page.locator("#work-item-dispatch-status")).toContainText(
          "Synthetic dispatch rejected",
        );
        await expect(page.locator("#work-item-target")).toBeEnabled();
        await expect(page.locator("#work-item-target")).toHaveValue(
          "tsk_source",
        );
        await page.locator("#work-item-target").selectOption("tsk_target");
        await page
          .locator("#work-item-dispatch")
          .evaluate((form) => form.requestSubmit());
        await expect(page.locator("#work-item-dispatch-status")).toContainText(
          "Synthetic dispatch rejected",
        );
        await page.evaluate(() => qa.outcome("success"));
        await page
          .locator("#work-item-dispatch")
          .evaluate((form) => form.requestSubmit());
        await expect(page.locator("#dialog")).not.toBeVisible();
        state = await page.evaluate(() => qa.state);
        assert.notEqual(
          state.calls[0].body.requestId,
          state.calls[1].body.requestId,
          `${label}: target change reused a request key`,
        );
        assert.equal(
          state.calls[1].body.requestId,
          state.calls[2].body.requestId,
          `${label}: exact retry rotated its request key`,
        );
        results.push(
          `${label}: failure retains choices and exact retry identity`,
        );

        await load(page, kind);
        await page.evaluate(() => qa.outcome("pending"));
        await openDispatch(page);
        await page
          .locator("#work-item-dispatch")
          .evaluate((form) => form.requestSubmit());
        await expect(page.locator("#work-item-dispatch-status")).toHaveText(
          "Sending…",
        );
        await expect(page.locator("#work-item-target")).toBeDisabled();
        await expect(
          page.locator("#work-item-dispatch button[type=submit]"),
        ).toBeDisabled();
        await page.locator("#dialog-close").click();
        await page.evaluate(() => qa.resolve());
        await page.waitForFunction(() => qa.state.settled === 1);
        await expect(page.locator("#dialog")).not.toBeVisible();
        state = await page.evaluate(() => qa.state);
        assert.equal(
          state.notices.length,
          0,
          `${label}: late cancel announced`,
        );
        results.push(`${label}: pending cancel ignores late success`);

        await load(page, kind);
        await page.evaluate(() => qa.outcome("pending"));
        await openDispatch(page);
        await page
          .locator("#work-item-dispatch")
          .evaluate((form) => form.requestSubmit());
        await expect(page.locator("#work-item-dispatch-status")).toHaveText(
          "Sending…",
        );
        await page.evaluate(() => qa.replacement());
        await expect(page.locator("#replacement-dialog")).toBeVisible();
        await page.evaluate(() => qa.resolve());
        await page.waitForFunction(() => qa.state.settled === 1);
        await expect(page.locator("#replacement-dialog")).toBeVisible();
        state = await page.evaluate(() => qa.state);
        assert.equal(
          state.notices.length,
          0,
          `${label}: replacement received stale notice`,
        );
        results.push(`${label}: replacement dialog survives late success`);
      }
    } catch (error) {
      failures.push(`${engine.name()}: ${error.stack || error.message}`);
    } finally {
      await context.close();
      await browser.close();
    }
  }
} finally {
  await server.close();
}

await mkdir(".build/work-item-dispatch", { recursive: true });
await writeFile(
  ".build/work-item-dispatch/results.json",
  JSON.stringify({ results, failures }, null, 2),
);
for (const result of results) console.log(result);
if (failures.length)
  throw new AggregateError(failures, "Work-item dispatch acceptance failed");
console.log(`${results.length} work-item dispatch checks passed.`);
