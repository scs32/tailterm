// B1 async-scope acceptance: delayed audit/vault reads must not repaint a
// different project or replacement client in real Chromium and WebKit.
import { chromium, webkit } from "@playwright/test";
import { createServer } from "node:http";
import { once } from "node:events";
import { readFile } from "node:fs/promises";
import path from "node:path";
import assert from "node:assert/strict";

const root = process.cwd();
const taskA = "tsk_aaaaaaaaaaaaaaaa";
const taskB = "tsk_bbbbbbbbbbbbbbbb";
const agent = "agt_cccccccccccccccc";

const html = `<!doctype html><html><head><meta charset="utf-8">
<link rel="stylesheet" href="/client/style.css"></head><body>
<div id="mode-view"></div><p id="notice"></p>
<script type="module">
import {createBoardView} from '/client/board-view.js';
const taskA=${JSON.stringify(taskA)},taskB=${JSON.stringify(taskB)},agent=${JSON.stringify(agent)};
let liveClient;
let clientNumber=1;
let exactUnknown=false;
let auditGate=null,listGate=null;
const deferred=()=>{let resolve;const promise=new Promise(done=>resolve=done);return {promise,resolve}};
const message=id=>({seq:1,text:'message '+id,from:{agentId:agent},to:'',broadcast:false,replyTo:0,createdAt:'2026-09-10T00:00:00Z'});
const record=id=>({message:{taskId:id,seq:1},original:{classification:'intake',revision:1,workItems:[]},current:{classification:'intake',revision:1,workItems:[]}});
function makeClient(token){return {
  base:'https://synthetic.invalid',token,
  async listTasks(){return [{id:taskA,name:'Project A',goal:'A',status:'open'},{id:taskB,name:'Project B',goal:'B',status:'open'}]},
  async getTask(id){return {task:{id,name:id===taskA?'Project A':'Project B',goal:id,status:'open',swarm:false},agents:[{id:agent,name:'worker',status:'running',host:'synthetic.invalid',session:'none',readUpTo:1,createdAt:'2026-09-10T00:00:00Z'}]}},
  async listMessages(id){return [message(id)]},
  async listDecisions(){return {decisions:[]}},
  async capabilities(){return {messageAudit:{versions:[2]},auditExport:{versions:[2]},policy:{mode:'observe'}}},
  async loadMessageAudits(id){if(id===taskA&&auditGate){const gate=auditGate;auditGate=null;gate.started.resolve();await gate.release.promise}return exactUnknown?{}:{'1':record(id)}},
  async getMessageAudit(){throw Object.assign(new Error('Synthetic exact audit unavailable'),{status:503})},
  subscribe(){return {stop(){}}},
};}
const persistence={
  async list(){if(listGate){const gate=listGate;listGate=null;gate.started.resolve();await gate.release.promise;return [{id:'editor:1',state:'draft',values:{mode:'resolve',expectedRevision:1,target:'new',kind:'feature',title:'A only',reason:'A',sources:taskA+'#1'}}]}return []},
  async save(){},async remove(){},
};
liveClient=makeClient('credential-one');
const noop=()=>{};
const board=createBoardView({client:()=>liveClient,getTabs:()=>[],activate:noop,notice:text=>document.querySelector('#notice').textContent=text,addAgent:noop,settings:noop,revealAgent:noop,attachTask:noop,newTask:noop,configure:noop,intentPersistence:persistence});
board.mount(document.querySelector('#mode-view'));
window.qa={board,taskA,taskB,
  delayAudit(){auditGate={started:deferred(),release:deferred()};return auditGate},
  delayList(){listGate={started:deferred(),release:deferred()};return listGate},
  replaceClient(){liveClient=makeClient('credential-'+(++clientNumber))},
  unknown(value){exactUnknown=value},
};
</script></body></html>`;

let server;
try {
  server = createServer(async (request, response) => {
    try {
      const url = new URL(request.url, "http://127.0.0.1");
      if (url.pathname === "/") {
        response.writeHead(200, { "content-type": "text/html" });
        response.end(html);
        return;
      }
      const file = path.resolve(root, "." + decodeURIComponent(url.pathname));
      if (!file.startsWith(root + path.sep)) throw new Error("invalid path");
      response.writeHead(200, {
        "content-type": file.endsWith(".js")
          ? "text/javascript"
          : file.endsWith(".css")
            ? "text/css"
            : "application/octet-stream",
      });
      response.end(await readFile(file));
    } catch (error) {
      response.writeHead(500, { "content-type": "text/plain" });
      response.end(error.message);
    }
  });
  server.listen(0, "127.0.0.1");
  await once(server, "listening");
  const origin = `http://127.0.0.1:${server.address().port}`;

  for (const [name, engine] of [
    ["chromium", chromium],
    ["webkit", webkit],
  ]) {
    const browser = await engine.launch();
    try {
      const page = await browser.newPage();
      const errors = [];
      page.on("pageerror", (error) => errors.push(error.message));
      await page.goto(origin);

      await page.evaluate(() => {
        const gate = qa.delayAudit();
        qa.auditStarted = gate.started.promise;
        qa.auditRelease = gate.release.resolve;
        qa.firstShow = qa.board.show(qa.taskA);
      });
      await page.evaluate(() => qa.auditStarted);
      await page.evaluate(() => qa.board.show(qa.taskB));
      await page.getByRole("heading", { name: "Project B" }).waitFor();
      await page.evaluate(() => qa.auditRelease());
      await page.evaluate(() => qa.firstShow);
      assert.equal(
        await page.getByRole("heading", { name: "Project B" }).count(),
        1,
      );
      assert.match(
        await page.locator("#board-messages").innerText(),
        new RegExp(taskB),
      );
      assert.doesNotMatch(
        await page.locator("#board-messages").innerText(),
        new RegExp(taskA),
      );

      await page.evaluate(() => {
        qa.replaceClient();
        const gate = qa.delayList();
        qa.listStarted = gate.started.promise;
        qa.listRelease = gate.release.resolve;
        qa.secondShow = qa.board.show(qa.taskA);
      });
      await page.evaluate(() => qa.listStarted);
      await page.evaluate(() => {
        qa.replaceClient();
        return qa.board.show(qa.taskB);
      });
      await page.evaluate(() => qa.listRelease());
      await page.evaluate(() => qa.secondShow);
      await page.getByRole("heading", { name: "Project B" }).waitFor();
      assert.equal(await page.locator("[data-audit-resolve]").count(), 0);
      await page.evaluate(() => {
        qa.unknown(true);
        return qa.board.reload();
      });
      await page.locator('[data-audit-kind="unknown"]').waitFor();
      assert.equal(await page.locator("[data-audit-correct-open]").count(), 0);
      await page.locator("[data-audit-retry]").click();
      await page.getByText(/Audit context unavailable/).waitFor();
      assert.deepEqual(errors, [], `${name}: browser errors`);
      await page.evaluate(() => qa.board.hide());
      console.log(
        `${name}: delayed audit and scoped intent reads stayed with their initiating client/project.`,
      );
    } finally {
      await browser.close();
    }
  }
} finally {
  if (server) await new Promise((resolve) => server.close(resolve));
}
