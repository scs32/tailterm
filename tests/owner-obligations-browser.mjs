// wi_deb2ce3cda1968d0 / order #11942 / ASSIGN #12043 / exact Start #12045.
// Compiled loopback hub and CLI, temporary databases and disposable browsers only.
import { chromium, webkit, expect } from "@playwright/test";
import { createServer } from "node:http";
import { once } from "node:events";
import { spawn, execFile } from "node:child_process";
import { promisify } from "node:util";
import { mkdtemp, readFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import assert from "node:assert/strict";
import { prepareTestBinary } from "./test-binaries.mjs";
const exec = promisify(execFile),
  root = process.cwd(),
  temp = await mkdtemp(path.join(tmpdir(), "owner-obligations-"));
const cleanEnv = Object.fromEntries(
  Object.entries(process.env).filter(
    ([k]) => !/^(TAILTERM_|CODEX_|TT_|TMUX)/.test(k) && k !== "HOME",
  ),
);
let backend, web, browser, hub, origin;
let hubBinary = path.join(temp, "hub"), ttBinary = path.join(temp, "tt");
const pause = (ms) => new Promise((r) => setTimeout(r, ms));
async function freePort() {
  const s = createServer();
  s.listen(0, "127.0.0.1");
  await once(s, "listening");
  const p = s.address().port;
  await new Promise((r) => s.close(r));
  return p;
}
async function api(method, route, body, code = 200) {
  if (method !== "GET") await pause(60);
  const r = await fetch(hub + route, {
    method,
    headers: body ? { "content-type": "application/json" } : undefined,
    body: body ? JSON.stringify(body) : undefined,
    signal: AbortSignal.timeout(10000),
  });
  const raw = await r.text();
  assert.equal(r.status, code, route + ": " + raw);
  return raw ? JSON.parse(raw) : null;
}
const html = `<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><link rel="stylesheet" href="/client/style.css"><link rel="stylesheet" href="/client/work-items.css"></head><body><main><div id="view"></div></main><p id="notice"></p><dialog id="dialog"></dialog><script type="module">
import {createBoardView} from '/client/board-view.js';
import {createQueueView} from '/client/queue-view.js';
import {createTasksView} from '/client/tasks-view.js';
import {createHubClient} from '/client/hub-client.js';
import {createCachedHubClient} from '/client/cached-hub-client.js';
const live=createHubClient({baseURL:location.origin,fetchImpl:fetch});
const memory=new Map();const cache={async get(scope,key){return memory.get(scope+'|'+key)||null},async put(scope,key,value){memory.set(scope+'|'+key,{value})},dispose(){}};
const client=createCachedHubClient({client:live,cache,refreshMs:2000});
const noop=()=>{},host=document.querySelector('#view'),notice=s=>document.querySelector('#notice').textContent=s;
const board=createBoardView({client:()=>client,getTabs:()=>[],activate:noop,notice,addAgent:noop,revealAgent:noop,attachTask:noop,newTask:noop,configure:noop});
const queue=createQueueView({client:()=>client,configure:noop,notice,dialog:noop,closeDialog:noop});
const tasks=createTasksView({client:()=>client,taskHub:{groupOf:()=>null},getTabs:()=>[],activate:noop,notice,confirm:async()=>false,openBoard:noop,configure:noop});
async function show(mode,task){for(const v of [board,queue,tasks])v.hide();host.replaceChildren();const v={board,queue,tasks}[mode];v.mount(host);await v.show(task)}
window.qa={board,queue,tasks,client,live,show,async refresh(){await client.refreshConnection();await board.reload();await queue.reload();await tasks.reload()}};
await show('board',new URLSearchParams(location.search).get('task'));
</script></body></html>`;
// wi_5a411a2a78e3ad9e / order #12222: isolated real client/view regressions.
const resilienceHTML = `<!doctype html><html><head><meta charset="utf-8"><link rel="stylesheet" href="/client/style.css"></head><body><main id="view"></main><script type="module">
import {createBoardView} from '/client/board-view.js';
import {createTasksView} from '/client/tasks-view.js';
import {createHubClient} from '/client/hub-client.js';
import {createCachedHubClient} from '/client/cached-hub-client.js';
const ids=['tsk_1111111111111111','tsk_2222222222222222'];
const tasks=ids.map((id,i)=>({id,name:'Project '+i,status:'open',goal:'Synthetic goal',createdAt:'2026-09-08T12:00:00Z'}));
const owner=id=>({id:'owner-'+id,recipientKind:'owner',taskId:id,state:'open',messageSeq:10,subject:'Owner '+id,createdAt:'2026-09-08T12:00:00Z',dueAt:'2026-09-28T12:00:00Z',request:{text:'Synthetic request',workItems:[{itemTaskId:id,itemId:'item-'+id,relationship:'primary'}]}});
const state={mode:'ok',online:true,ownerReads:0,waiting:[],token:'alpha'};
const memory=new Map();
const cache={async get(scope,key){return memory.get(scope+'|'+key)||null},async put(scope,key,value){memory.set(scope+'|'+key,{value});return true},async clear(scope){for(const key of memory.keys())if(key.startsWith(scope+'|'))memory.delete(key)},dispose(){}};
const response=(data,status=200)=>new Response(JSON.stringify(data),{status});
async function transport(url,init){
 const u=new URL(url),p=u.pathname,id=p.split('/')[3];
 if(!state.online)throw Error('Synthetic offline');
 if(p.endsWith('/obligations')){
   state.ownerReads++;
   const result=[owner(id)];
   if(state.mode==='hold')return await new Promise(resolve=>state.waiting.push(()=>resolve(response({obligations:result}))));
   if(state.mode==='reject')throw Error('Optional transport rejected');
   if(['404','503','401'].includes(state.mode))return response({error:'Optional unavailable'},Number(state.mode));
   return response({obligations:state.mode==='empty'?[]:result});
 }
 if(p==='/v1/capabilities')return response({});
 if(p==='/v1/tasks')return response({tasks});
 if(p==='/v1/work-items')return response({items:[],next:0});
 if(p.endsWith('/messages'))return response({messages:[{seq:1,from:{user:'fixture'},text:'Message '+id,createdAt:'2026-09-08T12:00:00Z'}]});
 if(p.endsWith('/decisions'))return response({decisions:[],nextAfter:0});
 if(p.endsWith('/team-queue'))return response({entries:[{itemId:'item-'+id,state:'running'}],concurrencyLimit:1});
 return response({task:tasks.find(t=>t.id===id),agents:[]});
}
let client,live,current;
const root=document.querySelector('#view'),noop=()=>{};
function configure(token='alpha',missing=false){
 client?.dispose();
 live=createHubClient({baseURL:'http://synthetic.invalid',token,fetchImpl:transport});
 // No polling or external connection: only actual cache notifications can reload.
 live.subscribe=()=>({stop(){}});
 client=createCachedHubClient({client:live,cache,online:()=>state.online,refreshMs:60000});
 if(missing)delete client.listOwnerObligations;
}
configure();
const common={client:()=>client,getTabs:()=>[],activate:noop,notice:noop,configure:noop};
const board=createBoardView({...common,addAgent:noop,attachTask:noop,newTask:noop});
const projects=createTasksView({...common,taskHub:{groupOf:()=>null},confirm:async()=>false,openBoard:noop});
async function show(mode,id=ids[0]){
 board.hide();projects.hide();root.replaceChildren();current=mode;
 const view=mode==='board'?board:projects;view.mount(root);await view.show(id);
 if(mode==='projects')root.querySelector('[data-task-select="'+id+'"]').click();
}
window.qa={ids,state,board,projects,show,configure,get client(){return client},
 async warm(){await client.listTasks();for(const id of ids){await client.getTask(id);await client.listMessages(id,{limit:200,latest:1});await client.listDecisions(id)}},
 refresh(){client.invalidate();return current==='board'?board.reload():projects.reload()},
 release(){state.waiting.splice(0).forEach(resolve=>resolve())},
 close(){board.hide();projects.hide();client.dispose()}
};
</script></body></html>`;
async function checkOptionalReads(engine, origin) {
  const browser = await engine.launch();
  try {
    for (const mode of ["board", "projects"]) {
      for (const failure of ["missing", "404", "503", "reject"]) {
        const context = await browser.newContext(),
          page = await context.newPage(),
          errors = [];
        page.on("pageerror", (error) => errors.push(error.message));
        await page.goto(origin + "/resilience");
        await page.waitForFunction(() => !!window.qa);
        await page.evaluate(
          async ({ mode, failure }) => {
            window.qa.state.mode = failure;
            if (failure === "missing") window.qa.configure("alpha", true);
            await window.qa.show(mode);
          },
          { mode, failure },
        );
        const normal =
          mode === "board"
            ? "#board-messages"
            : '[data-testid="team-delivery-panel"]';
        await expect(page.locator(normal)).toContainText(
          mode === "board" ? "Message" : "Delivery",
        );
        await expect(
          page.locator("[data-owner-requests], [data-owner-wait]"),
        ).toHaveCount(0);
        // Repeated view reloads must not repeatedly hit a failing optional route.
        if (mode === "board")
          await page
            .locator("#board-text")
            .fill("Draft survives optional refresh");
        await page.evaluate(async () => {
          for (let i = 0; i < 5; i++) await window.qa.refresh();
        });
        // Explicit invalidations intentionally allow retries; ordinary reloads
        // within the same cache interval must remain bounded and stable.
        const before = await page.evaluate(() => window.qa.state.ownerReads);
        await page.evaluate(async (mode) => {
          for (let i = 0; i < 5; i++)
            await window.qa[mode === "board" ? "board" : "projects"].reload();
        }, mode);
        await page.waitForTimeout(100);
        const after = await page.evaluate(() => window.qa.state.ownerReads);
        assert.ok(after - before <= 1, mode + " optional retry loop");
        if (mode === "board")
          await expect(page.locator("#board-text")).toHaveValue(
            "Draft survives optional refresh",
          );
        assert.deepEqual(errors, []);
        await page.evaluate(() => window.qa.close());
        await context.close();
        console.log(
          "PASS " + engine.name() + ": " + mode + " optional " + failure,
        );
      }
      // Only ordinary reads are warm. An uncached held optional request must
      // not gate first paint, delivery or compose; recovery restores content.
      const context = await browser.newContext(),
        page = await context.newPage();
      await page.goto(origin + "/resilience");
      await page.waitForFunction(() => !!window.qa);
      await page.evaluate(async (mode) => {
        await window.qa.warm();
        window.qa.state.mode = "hold";
        await window.qa.show(mode);
      }, mode);
      await expect(
        page.locator(
          mode === "board"
            ? "#board-messages"
            : '[data-testid="team-delivery-panel"]',
        ),
      ).toBeVisible();
      await expect(
        page.locator("[data-owner-requests], [data-owner-wait]"),
      ).toHaveCount(0);
      if (mode === "board")
        await expect(page.locator("#board-text")).toBeVisible();
      await page.evaluate(() => {
        window.qa.state.mode = "ok";
        window.qa.release();
      });
      await expect(
        page.locator(
          mode === "board" ? "[data-owner-request]" : "[data-owner-wait]",
        ),
      ).toHaveCount(1);
      // A background failure hides previously loaded content, and identical
      // data after recovery must still notify the views to restore it.
      await page.evaluate(async () => {
        window.qa.state.mode = "503";
        await window.qa.client.refreshConnection();
      });
      await expect(
        page.locator("[data-owner-requests], [data-owner-wait]"),
      ).toHaveCount(1);
      await page.evaluate(async () => {
        window.qa.state.mode = "ok";
        await window.qa.client.refreshConnection();
      });
      await expect(
        page.locator(
          mode === "board" ? "[data-owner-request]" : "[data-owner-wait]",
        ),
      ).toHaveCount(1);
      // Same client, different task: release of an older request must not leak.
      await page.evaluate(async (mode) => {
        window.qa.state.mode = "hold";
        window.qa.client.invalidate();
        await window.qa[mode === "board" ? "board" : "projects"].reload();
        window.qa.state.mode = "empty";
        await window.qa.show(mode, window.qa.ids[1]);
        window.qa.release();
      }, mode);
      await expect(
        page.locator("[data-owner-requests], [data-owner-wait]"),
      ).toHaveCount(0);
      await expect(
        page.locator(mode === "board" ? "#board-messages" : ".tasks-detail"),
      ).toContainText(
        mode === "board" ? "Message tsk_2222222222222222" : "Project 1",
      );
      // Same task, different credential/client: late old data is discarded.
      await page.evaluate(async (mode) => {
        window.qa.state.mode = "hold";
        window.qa.client.invalidate();
        await window.qa[mode === "board" ? "board" : "projects"].reload();
        window.qa.configure("beta");
        window.qa.state.mode = "empty";
        await window.qa.show(mode, window.qa.ids[1]);
        window.qa.release();
      }, mode);
      await expect(
        page.locator("[data-owner-requests], [data-owner-wait]"),
      ).toHaveCount(0);
      await page.waitForTimeout(100);
      await expect(
        page.locator("[data-owner-requests], [data-owner-wait]"),
      ).toHaveCount(0);
      await page.evaluate(() => window.qa.close());
      await context.close();
      console.log(
        "PASS " +
          engine.name() +
          ": " +
          mode +
          " warm cache/held recovery/task and client epochs",
      );
      const offlineContext = await browser.newContext(),
        offlinePage = await offlineContext.newPage();
      await offlinePage.goto(origin + "/resilience");
      await offlinePage.waitForFunction(() => !!window.qa);
      await offlinePage.evaluate(async (mode) => {
        await window.qa.warm();
        window.qa.state.online = false;
        await window.qa.show(mode);
      }, mode);
      await expect(
        offlinePage.locator(
          mode === "board" ? "#board-messages" : ".tasks-detail",
        ),
      ).toContainText(mode === "board" ? "Message" : "Project");
      await expect(
        offlinePage.locator("[data-owner-requests], [data-owner-wait]"),
      ).toHaveCount(0);
      await offlinePage.evaluate(() => window.qa.close());
      await offlineContext.close();
      // Authentication remains authoritative even though the optional panel is hidden.
      const authContext = await browser.newContext(),
        authPage = await authContext.newPage();
      await authPage.goto(origin + "/resilience");
      await authPage.waitForFunction(() => !!window.qa);
      await authPage.evaluate(async (mode) => {
        window.qa.state.mode = "401";
        await window.qa.show(mode);
      }, mode);
      await expect
        .poll(() =>
          authPage.evaluate(() => window.qa.client.cacheStatus().label),
        )
        .toBe("Hub sign-in required");
      await assert.rejects(
        authPage.evaluate(() => window.qa.client.listTasks()),
        /Optional unavailable/,
      );
      await authPage.evaluate(() => window.qa.close());
      await authContext.close();
      console.log(
        "PASS " +
          engine.name() +
          ": " +
          mode +
          " uncached obligations offline and authentication fence",
      );
    }
  } finally {
    await browser.close();
  }
}

async function serve(req, res) {
  try {
    const u = new URL(req.url, origin);
    if (u.pathname === "/resilience") {
      res.setHeader("content-type", "text/html");
      res.end(resilienceHTML);
      return;
    }
    if (u.pathname === "/") {
      res.setHeader("content-type", "text/html");
      res.end(html);
      return;
    }
    if (u.pathname.startsWith("/v1/")) {
      const chunks = [];
      for await (const x of req) chunks.push(x);
      const r = await fetch(hub + req.url, {
        method: req.method,
        headers: req.headers["content-type"]
          ? { "content-type": req.headers["content-type"] }
          : undefined,
        body: ["GET", "HEAD"].includes(req.method)
          ? undefined
          : Buffer.concat(chunks),
        signal: AbortSignal.timeout(35000),
      });
      res.writeHead(r.status, { "content-type": "application/json" });
      res.end(await r.text());
      return;
    }
    const file = path.resolve(root, "." + decodeURIComponent(u.pathname));
    if (!file.startsWith(root + path.sep)) throw Error("invalid path");
    if (u.searchParams.has("url")) {
      res.setHeader("content-type", "text/javascript");
      res.end("export default " + JSON.stringify(u.pathname));
      return;
    }
    res.setHeader(
      "content-type",
      file.endsWith(".js")
        ? "text/javascript"
        : file.endsWith(".css")
          ? "text/css"
          : "application/octet-stream",
    );
    res.end(await readFile(file));
  } catch (e) {
    res.writeHead(500);
    res.end(e.message);
  }
}
try {
  [hubBinary, ttBinary] = await Promise.all([
    prepareTestBinary({ root, target: "hub", output: hubBinary }),
    prepareTestBinary({ root, target: "tt", output: ttBinary }),
  ]);
  hub = "http://127.0.0.1:" + (await freePort());
  backend = spawn(hubBinary, {
    env: {
      ...cleanEnv,
      TAILTERM_STATE: temp,
      TAILTERM_DEV_LISTEN: new URL(hub).host,
      TAILTERM_TCP_LISTEN: "",
    },
    stdio: "ignore",
  });
  for (let n = 0; n < 100; n++) {
    try {
      await api("GET", "/v1/tasks");
      break;
    } catch (e) {
      if (n === 99) throw e;
      await pause(100);
    }
  }
  web = createServer(serve);
  web.listen(0, "127.0.0.1");
  await once(web, "listening");
  origin = "http://127.0.0.1:" + web.address().port;
  for (const engine of [chromium, webkit]) {
    await checkOptionalReads(engine, origin);
    const name = engine.name(),
      task = await api(
        "POST",
        "/v1/tasks",
        { name: name + " owner fixture", orchestrator: "lead" },
        201,
      ),
      base = "/v1/tasks/" + task.id;
    const lead = await api(
      "POST",
      base + "/agents",
      {
        name: "lead",
        host: "synthetic.invalid",
        session: "unused",
        runtime: "codex",
      },
      201,
    );
    const worker = await api(
      "POST",
      base + "/agents",
      {
        name: "worker",
        host: "synthetic.invalid",
        session: "unused-worker",
        runtime: "codex",
      },
      201,
    );
    const handler = await api(
      "POST",
      base + "/agents",
      {
        agentId:
          "agt_" +
          (name === "chromium" ? "1111111111111111" : "2222222222222222"),
        name: "fixture-handler",
        role: "database_handler",
        host: "synthetic.invalid",
        session: "unused-handler",
        runtime: "codex",
      },
      201,
    );
    const items = [];
    for (let i = 0; i < 2; i++) {
      const item = await api(
        "POST",
        base + "/work-items",
        {
          kind: "feature",
          title: name + " item " + i,
          description: "Synthetic scope; request and owner answer acceptance.",
          requestId: name + "-item-" + i,
        },
        201,
      );
      items.push(item);
      const workItems = [
        {
          itemTaskId: task.id,
          itemId: item.id,
          itemRevision: item.revision,
          relationship: "primary",
        },
      ];
      const order = await api(
        "POST",
        base + "/messages",
        {
          text: "Bounded synthetic order",
          workItems,
          requestId: name + "-order-" + i,
        },
        201,
      );
      await api(
        "POST",
        base + "/work-items/" + item.id + "/order-scope/confirm",
        {
          requestId: name + "-scope-" + i,
          agentId: handler.id,
          runId: handler.runId,
          expectedRevision: item.revision,
          scopeRevision: item.scopeRevision,
          orderMessageSeq: order.seq,
          complete: true,
        },
        201,
      );
      await api(
        "POST",
        base + "/team-queue/actions",
        {
          operation: "add",
          requestId: name + "-team-queue-" + i,
          itemId: item.id,
          orderMessageSeq: order.seq,
          host: "synthetic.invalid",
          cwd: temp,
        },
        200,
      );
      await api(
        "POST",
        base + "/work-items/" + item.id + "/dispatch",
        {
          revision: item.revision,
          targetTaskId: task.id,
          requestId: name + "-dispatch-" + i,
        },
        201,
      );
      item.order = order;
    }
    const messages = [];
    for (let i = 0; i < 3; i++) {
      const item = items[i === 2 ? 1 : 0];
      messages.push(
        await api(
          "POST",
          base + "/messages",
          {
            agentId: worker.id,
            runId: worker.runId,
            to: i === 0 ? "owner" : "",
            requestId: name + "-request-" + i,
            workItems: [
              {
                itemTaskId: task.id,
                itemId: item.id,
                itemRevision: item.revision,
                relationship: "primary",
              },
            ],
            workOrderMessage: { taskId: task.id, seq: item.order.seq },
            envelope: {
              kind: "request",
              to: i === 0 ? "owner" : "",
              subject: "Approve the full owner fixture request",
              expectedAnswer: " APPROVE " + name + " 矩阵\nsha256:fixture\n ",
              body: { ask: "Full request " + i + " " + "矩阵🙂".repeat(200) },
            },
          },
          201,
        ),
      );
    }
    for (let i = 0; i < 205; i++)
      await api(
        "POST",
        base + "/messages",
        { text: "synthetic history " + i },
        201,
      );
    const latest = await api("GET", base + "/messages?latest=1&limit=200");
    assert.ok(!latest.messages.some((m) => m.seq === messages[0].seq));
    const cli = await exec(
      ttBinary,
      ["obligations", "--owner", "--json"],
      {
        cwd: temp,
        env: { ...cleanEnv, TAILTERM_HUB: hub, TAILTERM_TASK: task.id },
        timeout: 10000,
      },
    );
    assert.equal(JSON.parse(cli.stdout).length, 3);
    browser = await engine.launch();
    const context = await browser.newContext({
        viewport: { width: 390, height: 844 },
      }),
      errors = [];
    let page = await context.newPage();
    page.on("pageerror", (e) => errors.push(e.message));
    await page.goto(origin + "/?task=" + task.id);
    await page.waitForFunction(() => !!window.qa);
    await expect(page.locator("[data-owner-request]")).toHaveCount(3);
    assert.equal(
      await page
        .locator("[data-owner-request]")
        .first()
        .locator("pre")
        .textContent(),
      messages[0].text,
    );
    await page.evaluate((task) => window.qa.show("queue", task), task.id);
    try {
      await expect(page.locator(".queue-row [data-owner-wait]")).toHaveCount(2);
    } catch (e) {
      console.error(
        "Queue diagnostic:",
        await page.locator("#view").innerText(),
        await api("GET", base + "/queue?limit=64"),
      );
      throw e;
    }
    await expect(
      page.locator(".queue-row [data-owner-wait]").first(),
    ).toContainText("2 requests");
    const age = await page.locator("[data-owner-age]").first().textContent();
    await expect
      .poll(() => page.locator("[data-owner-age]").first().textContent())
      .not.toBe(age);
    await page.evaluate(() => window.qa.show("tasks"));
    await page.locator('[data-task-select="' + task.id + '"]').click();
    await expect(
      page.locator('[data-testid="team-delivery-panel"] [data-owner-wait]'),
    ).toHaveCount(2);
    await page.evaluate((task) => window.qa.show("board", task), task.id);
    const owners = await api("GET", base + "/obligations?owner=1&open=1");
    const first = owners.obligations.find(
      (o) => o.messageSeq === messages[0].seq,
    );
    await page.locator('[data-owner-approve="' + first.id + '"]').click();
    await expect(page.locator("[data-owner-request]")).toHaveCount(2);
    const closed = await api("GET", base + "/obligations?owner=1");
    const saved = closed.obligations.find((o) => o.id === first.id);
    assert.equal(saved.state, "closed");
    const all = await api(
      "GET",
      base + "/messages?after=" + messages[2].seq + "&limit=200",
    ); // exact answer may be beyond one history page
    const answer = await api("GET", base + "/messages?latest=1&limit=1");
    assert.equal(
      answer.messages[0].envelope.body.answer,
      messages[0].envelope.expectedAnswer,
    );
    assert.equal(answer.messages[0].to, worker.id);
    for (const o of owners.obligations.filter((o) => o.id !== first.id))
      await api(
        "POST",
        base + "/obligations/" + o.id + "/answer",
        { text: "Owner answered", requestId: name + "-answer-" + o.id },
        201,
      );
    await page.evaluate(() => window.qa.refresh());
    await expect(page.locator("[data-owner-request]")).toHaveCount(0);
    await page.evaluate((task) => window.qa.show("queue", task), task.id);
    await expect(page.locator("[data-owner-wait]")).toHaveCount(0);
    await page.evaluate(() => window.qa.show("tasks"));
    await page.locator('[data-task-select="' + task.id + '"]').click();
    await expect(page.locator("[data-owner-wait]")).toHaveCount(0);
    // wi_2f6f24bc62b24a7a / order #13877: owner delegation windows in TailOS.
    // A reload is a fresh page after the old one stops its reads, so a
    // cancelled in-flight fetch is not mistaken for a page error.
    const reopen = async () => {
      await page.evaluate(() => {
        for (const v of [window.qa.board, window.qa.queue, window.qa.tasks])
          v.hide();
        window.qa.client.dispose();
      });
      await page.close();
      page = await context.newPage();
      page.on("pageerror", (e) => errors.push(e.message));
      await page.goto(origin + "/?task=" + task.id);
      await page.waitForFunction(() => !!window.qa);
    };
    await page.evaluate((task) => window.qa.show("board", task), task.id);
    await page.locator("[data-delegation-toggle] summary").click();
    await page
      .locator("[data-delegation-open] select[name=delegate]")
      .selectOption(lead.id);
    await page
      .locator("[data-delegation-open] select[name=scope]")
      .selectOption("decisions_merges_deploys");
    await page.locator("[data-delegation-open] button[type=submit]").click();
    await expect(page.locator("[data-delegation-window]")).toContainText(
      "Delegated to lead",
    );
    await expect(page.locator("[data-delegation-window]")).toContainText(
      "Decisions, merges and deploys",
    );
    await reopen();
    await expect(page.locator("[data-delegation-window]")).toContainText(
      "Delegated to lead",
    );
    const routed = await api(
      "POST",
      base + "/messages",
      {
        agentId: worker.id,
        runId: worker.runId,
        to: "owner",
        requestId: name + "-delegated-request",
        workItems: [
          {
            itemTaskId: task.id,
            itemId: items[0].id,
            itemRevision: items[0].revision,
            relationship: "primary",
          },
        ],
        workOrderMessage: { taskId: task.id, seq: items[0].order.seq },
        envelope: {
          kind: "request",
          to: "owner",
          subject: "Choose the delegated rollout order",
          body: { ask: "Staged or all at once?" },
        },
      },
      201,
    );
    await page.evaluate(() => window.qa.refresh());
    await expect(page.locator("[data-delegated-to]")).toHaveText(
      "Delegated to lead",
    );
    const delegated = (
      await api("GET", base + "/obligations?owner=1&open=1")
    ).obligations.find((o) => o.messageSeq === routed.seq);
    await api(
      "POST",
      base + "/obligations/" + delegated.id + "/answer",
      {
        agentId: lead.id,
        runId: lead.runId,
        text: "Staged first",
        rationale: "Staged keeps the rollback simple.",
        requestId: name + "-delegate-answer",
      },
      201,
    );
    await page.evaluate(() => window.qa.refresh());
    await expect(page.locator("[data-delegated-answers] summary")).toHaveText(
      "Delegated answers (1)",
    );
    await page.locator("[data-delegated-answers] summary").click();
    await expect(page.locator("[data-delegated-answer]")).toContainText(
      "Rationale: Staged keeps the rollback simple.",
    );
    await expect(page.locator("[data-delegated-answer]")).toContainText(
      "Staged first",
    );
    await page.screenshot({
      path: ".build/owner-delegation-" + name + "-open.png",
    });
    await page.locator("[data-delegation-end]").click();
    await expect(page.locator("[data-delegation-window]")).toHaveCount(0);
    await expect(page.locator("[data-delegation-toggle]")).toHaveCount(1);
    await page.locator("[data-delegation-toggle] summary").click();
    await page.screenshot({
      path: ".build/owner-delegation-" + name + "-form.png",
    });
    const windows = await api("GET", base + "/delegation-windows");
    assert.equal(windows.windows[0].state, "closed");
    assert.equal(windows.windows[0].openedSource.kind, "tailos");
    await reopen();
    await expect(page.locator("[data-delegated-answers] summary")).toHaveText(
      "Delegated answers (1)",
    );
    const strip = await page.locator("[data-delegation]").boundingBox();
    assert.ok(strip.width <= 390, "delegation strip fits a phone width");
    assert.deepEqual(errors, []);
    await page.evaluate(() => {
      for (const v of [window.qa.board, window.qa.queue, window.qa.tasks])
        v.hide();
      window.qa.client.dispose();
    });
    await context.close();
    await browser.close();
    browser = null;
    console.log(
      "PASS " +
        name +
        ": full owner requests beyond 200, exact approval, CLI, item isolation, clocks, resolution refresh and delegation windows",
    );
  }
} finally {
  if (browser) await browser.close();
  if (web) {
    web.closeAllConnections();
    await new Promise((r) => web.close(r));
  }
  if (backend) {
    backend.kill("SIGTERM");
    await once(backend, "exit").catch(() => {});
  }
  await rm(temp, { recursive: true, force: true });
}
