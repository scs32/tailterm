// Acceptance for wi_a1b959960975589a revision 3, work order #1167.
// Uses only an in-memory history/client and disposable browser contexts.
import { chromium, webkit } from "@playwright/test";
import { createServer } from "vite";
import assert from "node:assert/strict";

const taskId = "tsk_scroll_fixture";
const agentId = "agt_scroll_fixture";

const html = `<!doctype html><html><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<link rel="stylesheet" href="/client/style.css">
<style>
html,body{height:100%;margin:0} body{padding:20px} #mode-view{height:720px}
#mode-view .message-footer>span{display:block;flex:0 0 75px;width:75px;white-space:normal}
</style></head><body><div id="mode-view"></div><p id="notice"></p>
<script type="module">
import {createBoardView} from '/client/board-view.js';
const taskId=${JSON.stringify(taskId)},agentId=${JSON.stringify(agentId)};
const message=(seq,prefix='Current')=>({seq,from:{user:'fixture-owner'},text:\`${"${prefix}"} synthetic message ${"${seq}"}\\n${"${'viewport anchor content '.repeat((seq%3)+1)}"}\`,createdAt:'2026-09-09T12:00:00Z'});
const state={messages:Array.from({length:25},(_,index)=>message(index+21)),readUpTo:0,posts:[]};
const decision={request:{seq:901,from:{agentId},createdAt:'2026-09-09T12:00:00Z',decisionRequest:{question:'How should the isolated scroll fixture proceed?',options:[{id:'steady',label:'Keep position',description:'Preserve the visible message anchor.'},{id:'jump',label:'Jump',description:'Move the viewport during refresh.'}],recommendedOptionId:'steady',recommendationReason:'It keeps reading continuity.'}}};
const client={
  async listTasks(){return [{id:taskId,name:'Scroll fixture',goal:'Isolated history viewport checks',status:'open'}]},
  async getTask(){return {task:{id:taskId,name:'Scroll fixture',goal:'Isolated history viewport checks',status:'open',swarm:false},agents:[{id:agentId,name:'fixture-worker',status:'running',host:'synthetic.invalid',session:'none',readUpTo:state.readUpTo,createdAt:'2026-09-09T12:00:00Z'}]}},
  async listMessages(){return state.messages.map(row=>({...row}))},
  async listDecisions(){return {decisions:[decision],nextAfter:0}},
  async postMessage(_id,body){const row={...message(state.messages.at(-1).seq+1,'Own'),...body,from:{user:'fixture-owner'}};state.messages.push(row);state.posts.push(row);return row},
  subscribe(){return {stop(){}}}, cacheStatus(){return {label:'Fixture current'}}
};
const noop=()=>{};
const board=createBoardView({client:()=>client,getTabs:()=>[],activate:noop,notice:text=>document.querySelector('#notice').textContent=text,addAgent:noop,settings:noop,revealAgent:noop,attachTask:noop,newTask:noop,configure:noop});
board.mount(document.querySelector('#mode-view'));
await board.show(taskId);
const list=()=>document.querySelector('#board-messages');
const viewport=()=>{const container=list(),bounds=container.getBoundingClientRect(),rows=[...container.querySelectorAll('[data-message]')],anchor=rows.find(row=>row.getBoundingClientRect().bottom>bounds.top+.5),rect=anchor?.getBoundingClientRect(),receipt=rows[0]?.querySelector('.message-footer span');return {seq:Number(anchor?.dataset.message||0),top:rect?rect.top-bounds.top:0,scrollTop:container.scrollTop,scrollHeight:container.scrollHeight,clientHeight:container.clientHeight,distanceBottom:container.scrollHeight-container.clientHeight-container.scrollTop,rowCount:rows.length,stateCount:state.messages.length,receiptText:receipt?.textContent,receiptHeight:receipt?.getBoundingClientRect().height}};
const place=(seq,top=-12)=>{const container=list(),row=container.querySelector(\`[data-message="${"${seq}"}"]\`),bounds=container.getBoundingClientRect(),rect=row.getBoundingClientRect();container.scrollTop+=rect.top-bounds.top-top;return viewport()};
window.qa={state,board,viewport,place,message,
  async reload(){await board.reload()},
  async prepend(){state.messages=[...Array.from({length:20},(_,index)=>message(index+1,'Older')),...state.messages];await board.reload()},
  async markRead(){state.readUpTo=10000;await board.reload()},
  async append(){state.messages.push(message(state.messages.at(-1).seq+1,'Incoming'));await board.reload()},
  bottom(offset=0){const container=list();container.scrollTop=container.scrollHeight-container.clientHeight-offset;return viewport()}
};
</script></body></html>`;

const server = await createServer({
  configFile: false,
  root: process.cwd(),
  server: { host: "127.0.0.1", port: 0 },
  plugins: [
    {
      name: "board-scroll-fixture",
      configureServer(vite) {
        vite.middlewares.use("/board-scroll-test", (_request, response) => {
          response.setHeader("Content-Type", "text/html");
          response.end(html);
        });
      },
    },
  ],
});
await server.listen();
const origin = `http://127.0.0.1:${server.httpServer.address().port}`;
const near = (actual, expected, label) =>
  assert.ok(
    Math.abs(actual - expected) <= 1,
    `${label}: expected ${expected}, received ${actual}`,
  );
const sameAnchor = (before, after, label) => {
  assert.equal(after.seq, before.seq, `${label}: visible message changed`);
  near(after.top, before.top, `${label}: visible message offset`);
};

try {
  for (const engine of [chromium, webkit]) {
    const browser = await engine.launch();
    const context = await browser.newContext({
      viewport: { width: 1000, height: 800 },
    });
    const page = await context.newPage();
    const errors = [];
    page.on("pageerror", (error) => errors.push(error.message));
    try {
      await page.goto(`${origin}/board-scroll-test`);
      await page.waitForFunction(() => Boolean(window.qa));
      const initial = await page.evaluate(() => qa.viewport());
      near(initial.distanceBottom, 0, `${engine.name()} initial bottom`);

      await page.locator('[data-decision-option="steady"]').check();
      await page
        .locator("[data-decision-explanation]")
        .fill("Retain this decision draft.");
      await page.locator("#board-text").fill("Retain this composer draft.");
      const beforePrepend = await page.evaluate(() => qa.place(31));
      await page.evaluate(() => qa.prepend());
      await page.waitForFunction(() => qa.viewport().rowCount === 45);
      const afterPrepend = await page.evaluate(() => qa.viewport());
      sameAnchor(beforePrepend, afterPrepend, `${engine.name()} prepend`);
      assert.ok(
        afterPrepend.scrollTop > beforePrepend.scrollTop,
        `${engine.name()}: prepend did not add scrollable history: ${JSON.stringify({ beforePrepend, afterPrepend })}`,
      );
      assert.equal(
        await page.locator("#board-text").inputValue(),
        "Retain this composer draft.",
      );
      assert.equal(
        await page.locator('[data-decision-option="steady"]').isChecked(),
        true,
      );
      assert.equal(
        await page.locator("[data-decision-explanation]").inputValue(),
        "Retain this decision draft.",
      );

      const beforeRead = await page.evaluate(() => qa.place(31));
      await page.evaluate(() => qa.markRead());
      await page.waitForFunction(() =>
        document
          .querySelector(".message-footer span")
          ?.textContent.includes("Read by"),
      );
      const afterRead = await page.evaluate(() => qa.viewport());
      assert.notEqual(
        afterRead.receiptHeight,
        beforeRead.receiptHeight,
        `${engine.name()}: read fixture did not change receipt layout`,
      );
      sameAnchor(beforeRead, afterRead, `${engine.name()} read receipt update`);

      const beforeIncoming = await page.evaluate(() => qa.place(36));
      const beforeIncomingCount = beforeIncoming.rowCount;
      await page.evaluate(() => qa.append());
      await page.waitForFunction(
        (count) => qa.viewport().rowCount === count + 1,
        beforeIncomingCount,
      );
      const afterIncoming = await page.evaluate(() => qa.viewport());
      sameAnchor(
        beforeIncoming,
        afterIncoming,
        `${engine.name()} incoming message away from bottom`,
      );
      assert.ok(
        afterIncoming.distanceBottom > 60,
        `${engine.name()}: incoming message jumped to bottom`,
      );

      const nearBottom = await page.evaluate(() => qa.bottom(30));
      assert.ok(
        nearBottom.distanceBottom > 0 && nearBottom.distanceBottom < 60,
      );
      await page.evaluate(() => qa.append());
      await page.waitForFunction(
        (count) => qa.viewport().rowCount === count + 1,
        afterIncoming.rowCount,
      );
      const followed = await page.evaluate(() => qa.viewport());
      near(followed.distanceBottom, 0, `${engine.name()} near-bottom follow`);

      const beforeOwnAway = await page.evaluate(() => qa.place(38));
      await page.locator("#board-text").fill("Own send while reading history");
      await page.locator("#board-compose button[type=submit]").click();
      await page.waitForFunction(
        (count) =>
          qa.state.posts.some(
            (row) => row.text === "Own send while reading history",
          ) &&
          qa.viewport().rowCount === count + 1 &&
          document.querySelector("#board-text")?.value === "",
        beforeOwnAway.rowCount,
      );
      const afterOwnAway = await page.evaluate(() => qa.viewport());
      sameAnchor(
        beforeOwnAway,
        afterOwnAway,
        `${engine.name()} own send away from bottom`,
      );
      assert.ok(
        afterOwnAway.distanceBottom > 60,
        `${engine.name()}: own send jumped to bottom`,
      );
      assert.equal(await page.locator("#board-text").inputValue(), "");
      assert.equal(
        await page.locator("[data-decision-explanation]").inputValue(),
        "Retain this decision draft.",
      );

      await page.evaluate(() => qa.bottom());
      const beforeOwnBottomCount = await page.evaluate(
        () => qa.viewport().rowCount,
      );
      await page.locator("#board-text").fill("Own send at bottom");
      await page.locator("#board-compose button[type=submit]").click();
      await page.waitForFunction(
        (count) =>
          qa.state.posts.some((row) => row.text === "Own send at bottom") &&
          qa.viewport().rowCount === count + 1,
        beforeOwnBottomCount,
      );
      near(
        (await page.evaluate(() => qa.viewport())).distanceBottom,
        0,
        `${engine.name()} own-send bottom follow`,
      );
      assert.deepEqual(errors, [], `${engine.name()} browser errors`);
      console.log(
        `${engine.name()}: stable prepend/read/incoming/own-send anchors, near-bottom follow, and retained drafts`,
      );
    } finally {
      await context.close();
      await browser.close();
    }
  }
} finally {
  await server.close();
}
