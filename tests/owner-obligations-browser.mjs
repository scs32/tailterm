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
const exec = promisify(execFile),
  root = process.cwd(),
  temp = await mkdtemp(path.join(tmpdir(), "owner-obligations-"));
const cleanEnv = Object.fromEntries(
  Object.entries(process.env).filter(
    ([k]) => !/^(TAILTERM_|CODEX_|TT_|TMUX)/.test(k) && k !== "HOME",
  ),
);
let backend, web, browser, hub, origin;
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
async function serve(req, res) {
  try {
    const u = new URL(req.url, origin);
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
  await Promise.all([
    exec("go", ["build", "-o", path.join(temp, "hub"), "./cmd/tailterm-hub"], {
      cwd: path.join(root, "hub"),
    }),
    exec("go", ["build", "-o", path.join(temp, "tt"), "./cmd/tt"], {
      cwd: path.join(root, "hub"),
    }),
  ]);
  hub = "http://127.0.0.1:" + (await freePort());
  backend = spawn(path.join(temp, "hub"), {
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
      path.join(temp, "tt"),
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
      page = await context.newPage(),
      errors = [];
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
        ": full owner requests beyond 200, exact approval, CLI, item isolation, clocks and resolution refresh",
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
