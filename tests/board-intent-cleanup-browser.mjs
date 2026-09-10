// B1 cleanup acceptance: a confirmed older operation may remove only the
// exact editor/draft generation it submitted. Newer unsent content survives.
import { chromium, webkit } from "@playwright/test";
import { createServer } from "node:http";
import { once } from "node:events";
import { readFile } from "node:fs/promises";
import path from "node:path";
import assert from "node:assert/strict";

const root = process.cwd();
const task = "tsk_aaaaaaaaaaaaaaaa";
const agent = "agt_bbbbbbbbbbbbbbbb";
const item = "wi_cccccccccccccccc";
const pause = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

async function waitFor(check, label) {
  for (let attempt = 0; attempt < 200; attempt++) {
    if (await check()) return;
    await pause(10);
  }
  throw new Error(`Timed out waiting for ${label}`);
}

const html = `<!doctype html><html><head><meta charset="utf-8">
<link rel="stylesheet" href="/client/style.css"></head><body>
<div id="mode-view"></div><p id="notice"></p>
<script type="module">
import {createBoardView} from '/client/board-view.js';
const task=${JSON.stringify(task)},agent=${JSON.stringify(agent)};
const scenario=new URLSearchParams(location.search).get('scenario');
const deferred=()=>{let resolve;const promise=new Promise(done=>resolve=done);return {promise,resolve}};
let operationGate=null,removeGate=null;
const calls={correct:[],resolve:[],post:[],receipts:[]};
const storageKey='cleanup-records:'+scenario;
const records=new Map(JSON.parse(sessionStorage.getItem(storageKey)||'[]'));
const syncRecords=()=>sessionStorage.setItem(storageKey,JSON.stringify([...records]));
const original={classification:'intake',revision:1,workItems:[]};
let current={classification:'intake',revision:1,workItems:[]};
const audit=()=>({message:{taskId:task,seq:1},original,current});
const persistence={
  async list(){return [...records.values()].map(value=>structuredClone(value))},
  async save(record){records.set(record.id,structuredClone(record));syncRecords()},
  async remove(_scope,id){if(removeGate&&id===removeGate.id){const gate=removeGate;removeGate=null;gate.started.resolve();await gate.release.promise}records.delete(id);syncRecords()},
};
async function gated(kind,body){calls[kind].push(structuredClone(body));const gate=operationGate;if(!gate)throw new Error('test operation gate missing');operationGate=null;gate.started.resolve();await gate.release.promise}
const client={
  base:'https://synthetic.invalid',token:'credential',
  async listTasks(){return [{id:task,name:'Cleanup fixture',goal:'Synthetic only',status:'open'}]},
  async getTask(){return {task:{id:task,name:'Cleanup fixture',goal:'Synthetic only',status:'open',swarm:false},agents:[{id:agent,name:'worker',status:'running',host:'synthetic.invalid',session:'none',readUpTo:1,createdAt:'2026-09-10T00:00:00Z'}]}},
  async listMessages(){return [{seq:1,text:'Synthetic source',from:{agentId:agent},to:'',broadcast:false,replyTo:0,createdAt:'2026-09-10T00:00:00Z'}]},
  async listDecisions(){return {decisions:[]}},
  async capabilities(){return {messageAudit:{versions:[2]},auditExport:{versions:[2]},policy:{mode:'observe'}}},
  async loadMessageAudits(){return {'1':audit()}},
  async correctMessageAudit(_task,_seq,body){await gated('correct',body);if(scenario==='old-receipt'||(scenario==='old-retry'&&calls.correct.length===1))throw new Error('Synthetic lost correction response');current={classification:'intake',revision:current.revision+1,workItems:[]};return {state:current}},
  async resolveMessageAudit(_task,_seq,body){await gated('resolve',body);current={classification:'work',revision:current.revision+1,workItems:[body.existingItem]};return {state:current}},
  async postMessage(_task,body){await gated('post',body);return {seq:2}},
  async getMessageAuditReceipt(_task,requestId,options){calls.receipts.push({requestId,...options});return {state:{classification:'intake',revision:2,workItems:[]}}},
  subscribe(){return {stop(){}}},
};
const noop=()=>{};
const board=createBoardView({client:()=>client,getTabs:()=>[],activate:noop,notice:text=>document.querySelector('#notice').textContent=text,addAgent:noop,settings:noop,revealAgent:noop,attachTask:noop,newTask:noop,configure:noop,intentPersistence:persistence});
board.mount(document.querySelector('#mode-view'));
await board.show(task);
window.qa={board,calls,task,
  delayOperation(){operationGate={started:deferred(),release:deferred()};return operationGate},
  delayRemove(id){removeGate={id,started:deferred(),release:deferred()};return removeGate},
  record(id){const value=records.get(id);return value?structuredClone(value):null},
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
      async function pageFor(scenario) {
        const page = await browser.newPage();
        await page.goto(`${origin}/?scenario=${scenario}`);
        await page.locator("#board-messages").waitFor();
        return page;
      }
      async function gate(page, kind = "operation") {
        await page.evaluate((which) => {
          const value =
            which === "remove"
              ? qa.delayRemove("editor:1")
              : qa.delayOperation();
          qa[which + "Started"] = value.started.promise;
          qa[which + "Release"] = value.release.resolve;
        }, kind);
      }

      {
        const page = await pageFor("correction");
        await page.locator("[data-audit-correct-open]").click();
        const reason = page.locator(
          '[data-audit-correct] input[name="reason"]',
        );
        await reason.fill("First correction");
        await gate(page);
        await page
          .locator('[data-audit-correct] button[type="submit"]')
          .click();
        await page.evaluate(() => qa.operationStarted);
        await reason.fill("Newer correction edit");
        await page.evaluate(() => qa.operationRelease());
        await waitFor(
          async () => (await reason.count()) === 1,
          `${name} correction editor`,
        );
        assert.equal(await reason.inputValue(), "Newer correction edit");
        await waitFor(
          async () =>
            (await page.evaluate(
              () => qa.record("editor:1")?.values.reason,
            )) === "Newer correction edit",
          `${name} durable correction editor`,
        );
        await page.evaluate(() => qa.board.hide());
        await page.reload();
        assert.equal(await reason.inputValue(), "Newer correction edit");
        await page.close();
      }

      {
        const page = await pageFor("resolution");
        await page.locator("[data-audit-resolve-open]").click();
        const form = page.locator("[data-audit-resolve]");
        await form.locator('input[name="existing"]').fill(item);
        await form.locator('input[name="existingRevision"]').fill("1");
        const reason = form.locator('input[name="reason"]');
        await reason.fill("First resolution");
        await gate(page);
        await form.locator('button[type="submit"]').click();
        await page.evaluate(() => qa.operationStarted);
        await reason.fill("Newer resolution edit");
        await page.evaluate(() => qa.operationRelease());
        await waitFor(
          async () => (await reason.count()) === 1,
          `${name} resolution editor`,
        );
        assert.equal(await reason.inputValue(), "Newer resolution edit");
        await waitFor(
          async () =>
            (await page.evaluate(
              () => qa.record("editor:1")?.values.reason,
            )) === "Newer resolution edit",
          `${name} durable resolution editor`,
        );
        await page.evaluate(() => qa.board.hide());
        await page.reload();
        assert.equal(await reason.inputValue(), "Newer resolution edit");
        await page.close();
      }

      {
        const page = await pageFor("post");
        const input = page.locator("#board-text");
        await input.fill("First post");
        await gate(page);
        await page.locator('#board-compose button[type="submit"]').click();
        await page.evaluate(() => qa.operationStarted);
        await input.fill("Newer unsent post");
        await page.evaluate(() => qa.operationRelease());
        await waitFor(
          async () => (await input.inputValue()) === "Newer unsent post",
          `${name} newer post draft`,
        );
        await waitFor(
          async () =>
            (await page.evaluate(
              () => qa.record("draft:" + qa.task)?.values.text,
            )) === "Newer unsent post",
          `${name} durable post draft`,
        );
        const saved = await page.evaluate(() => qa.record("draft:" + qa.task));
        assert.equal(saved.values.text, "Newer unsent post");
        await page.evaluate(() => qa.board.hide());
        await page.reload();
        assert.equal(await input.inputValue(), "Newer unsent post");
        await gate(page);
        await page.locator('#board-compose button[type="submit"]').click();
        await page.evaluate(() => qa.operationStarted);
        const calls = await page.evaluate(() => qa.calls.post);
        assert.equal(calls.length, 1);
        assert.equal(calls[0].text, "Newer unsent post");
        assert.notEqual(calls[0].requestId, saved.requestId);
        await page.evaluate(() => qa.operationRelease());
        await page.close();
      }

      {
        const page = await pageFor("post-delayed-remove");
        const input = page.locator("#board-text");
        await input.fill("Post before delayed removal");
        await gate(page);
        await page.locator('#board-compose button[type="submit"]').click();
        await page.evaluate(() => qa.operationStarted);
        await page.evaluate(() => {
          const value = qa.delayRemove("draft:" + qa.task);
          qa.draftRemoveStarted = value.started.promise;
          qa.draftRemoveRelease = value.release.resolve;
          qa.operationRelease();
        });
        await page.evaluate(() => qa.draftRemoveStarted);
        await input.fill("Post written while removal is delayed");
        await page.evaluate(() => qa.draftRemoveRelease());
        await waitFor(
          async () =>
            (await page.evaluate(
              () => qa.record("draft:" + qa.task)?.values.text,
            )) === "Post written while removal is delayed",
          `${name} delayed post removal repair`,
        );
        assert.equal(
          await input.inputValue(),
          "Post written while removal is delayed",
        );
        await page.evaluate(() => qa.board.hide());
        await page.reload();
        assert.equal(
          await input.inputValue(),
          "Post written while removal is delayed",
        );
        await page.close();
      }

      {
        const page = await pageFor("old-receipt");
        await page.locator("[data-audit-correct-open]").click();
        const reason = page.locator(
          '[data-audit-correct] input[name="reason"]',
        );
        await reason.fill("Older committed intent");
        await gate(page);
        await page
          .locator('[data-audit-correct] button[type="submit"]')
          .click();
        await page.evaluate(() => qa.operationStarted);
        await page.evaluate(() => qa.operationRelease());
        await page.locator("[data-intent-recover]").waitFor();
        await reason.fill("Newer editor must remain");
        assert.equal(await reason.inputValue(), "Newer editor must remain");
        await page.locator("[data-intent-recover]").click();
        await waitFor(
          async () =>
            (await page.locator("[data-intent-recover]").count()) === 0,
          `${name} old receipt removal`,
        );
        assert.equal(await reason.inputValue(), "Newer editor must remain");
        assert.equal(
          await page.evaluate(() => qa.record("editor:1")?.values.reason),
          "Newer editor must remain",
        );
        await page.evaluate(() => qa.board.hide());
        await page.reload();
        assert.equal(await reason.inputValue(), "Newer editor must remain");
        await page.close();
      }

      {
        const page = await pageFor("old-retry");
        await page.locator("[data-audit-correct-open]").click();
        const reason = page.locator(
          '[data-audit-correct] input[name="reason"]',
        );
        await reason.fill("Older retry intent");
        await gate(page);
        await page
          .locator('[data-audit-correct] button[type="submit"]')
          .click();
        await page.evaluate(() => qa.operationStarted);
        await page.evaluate(() => qa.operationRelease());
        await page.locator("[data-intent-retry]").waitFor();
        await reason.fill("Newer editor survives exact retry");
        await gate(page);
        await page.locator("[data-intent-retry]").click();
        await page.evaluate(() => qa.operationStarted);
        await page.evaluate(() => qa.operationRelease());
        await waitFor(
          async () => (await page.locator("[data-intent-retry]").count()) === 0,
          `${name} exact retry completion`,
        );
        assert.equal(
          await reason.inputValue(),
          "Newer editor survives exact retry",
        );
        assert.equal(
          await page.evaluate(() => qa.record("editor:1")?.values.reason),
          "Newer editor survives exact retry",
        );
        await page.evaluate(() => qa.board.hide());
        await page.reload();
        assert.equal(
          await reason.inputValue(),
          "Newer editor survives exact retry",
        );
        await page.close();
      }

      {
        const page = await pageFor("delayed-remove");
        await page.locator("[data-audit-correct-open]").click();
        const reason = page.locator(
          '[data-audit-correct] input[name="reason"]',
        );
        await reason.fill("Submitted before delayed removal");
        await gate(page);
        await page
          .locator('[data-audit-correct] button[type="submit"]')
          .click();
        await page.evaluate(() => qa.operationStarted);
        await gate(page, "remove");
        await page.evaluate(() => qa.operationRelease());
        await page.evaluate(() => qa.removeStarted);
        await reason.fill("Written while removal is delayed");
        await page.evaluate(() => qa.removeRelease());
        await waitFor(
          async () =>
            (await page.evaluate(
              () => qa.record("editor:1")?.values.reason,
            )) === "Written while removal is delayed",
          `${name} delayed removal repair`,
        );
        assert.equal(
          await reason.inputValue(),
          "Written while removal is delayed",
        );
        await page.evaluate(() => qa.board.hide());
        await page.reload();
        assert.equal(
          await reason.inputValue(),
          "Written while removal is delayed",
        );
        await page.close();
      }

      console.log(
        `${name}: exact-generation cleanup preserved newer correction, resolution, and post drafts through success, receipt recovery, retry, and delayed editor/draft removal.`,
      );
    } finally {
      await browser.close();
    }
  }
} finally {
  if (server) await new Promise((resolve) => server.close(resolve));
}
