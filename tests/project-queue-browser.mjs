// Queue Tab acceptance for wi_a222a4d8a69c1d53 revision 2, order #1640/#1642.
// Uses a disposable SQLite hub, browser contexts and synthetic records only.
import { chromium, webkit } from "@playwright/test";
import { createServer } from "node:http";
import { once } from "node:events";
import { execFile, spawn } from "node:child_process";
import { promisify } from "node:util";
import { mkdtemp, readFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import assert from "node:assert/strict";

const exec = promisify(execFile);
const root = process.cwd();
const state = await mkdtemp(path.join(tmpdir(), "tailterm-project-queue-"));
const binary = path.join(state, "tailterm-hub");
const fixtureEnv = Object.fromEntries(
  Object.entries(process.env).filter(
    ([key]) => !/^(TAILTERM_|CODEX_|TT_|TMUX)/.test(key) && key !== "HOME",
  ),
);
let backend;
let web;
let hub;
let loseNextAction = false;
const actionAttempts = [];
const pause = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

async function freePort() {
  const server = createServer();
  server.listen(0, "127.0.0.1");
  await once(server, "listening");
  const port = server.address().port;
  await new Promise((resolve) => server.close(resolve));
  return port;
}
async function waitFor(check, label) {
  for (let attempt = 0; attempt < 200; attempt++) {
    if (await check()) return;
    await pause(25);
  }
  throw new Error(`Timed out waiting for ${label}`);
}
async function api(method, route, body, expected = 200) {
  const response = await fetch(hub + route, {
    method,
    headers:
      body === undefined ? undefined : { "content-type": "application/json" },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const text = await response.text();
  assert.equal(response.status, expected, `${method} ${route}: ${text}`);
  return text ? JSON.parse(text) : null;
}

const html = `<!doctype html><html><head><meta charset="utf-8">
<link rel="stylesheet" href="/client/style.css"><link rel="stylesheet" href="/client/work-items.css"></head><body>
<main><div id="mode-view"></div></main><p id="notice" role="status"></p><dialog id="dialog"></dialog>
<script type="module">
import {createQueueView} from '/client/queue-view.js';
import {createWorkItemsView} from '/client/work-items-view.js';
import {createHubClient} from '/client/hub-client.js';
const params=new URLSearchParams(location.search),task=params.get('task'),legacy=params.get('legacy')==='1';
const fetchImpl=async(url,init)=>{
  if(legacy&&url.endsWith('/v1/capabilities')) return {status:200,text:async()=>JSON.stringify({schemaVersion:1,messageAudit:{versions:[1,2]},auditExport:{versions:[2]}})};
  return fetch(url,init);
};
const client=createHubClient({baseURL:location.origin,fetchImpl});
const modal=document.querySelector('#dialog');
const persistence={
  async list(scope){return JSON.parse(sessionStorage.getItem('queue:'+scope)||'[]')},
  async save(record){const key='queue:'+record.scope,rows=JSON.parse(sessionStorage.getItem(key)||'[]');sessionStorage.setItem(key,JSON.stringify([...rows.filter(row=>row.id!==record.id),record]))},
  async remove(scope,id,requestId){const key='queue:'+scope,rows=JSON.parse(sessionStorage.getItem(key)||'[]');sessionStorage.setItem(key,JSON.stringify(rows.filter(row=>row.id!==id||(requestId&&row.requestId!==requestId))))},
};
const host=document.querySelector('#mode-view'),notice=text=>document.querySelector('#notice').textContent=text;
const view=createQueueView({client:()=>client,configure:()=>{},notice,
  dialog:(title,body)=>{modal.innerHTML='<h2>'+title+'</h2>'+body;modal.showModal()},closeDialog:()=>modal.close(),intentPersistence:persistence});
const bugs=createWorkItemsView({client:()=>client,kind:'bug',configure:()=>{},openBoard:()=>{},notice,
  dialog:(title,body)=>{modal.innerHTML='<h2>'+title+'</h2>'+body;modal.showModal()},closeDialog:()=>modal.close()});
async function showQueue(id){bugs.hide();view.hide();host.replaceChildren();view.mount(host);await view.show(id)}
async function showBugs(id){view.hide();bugs.hide();host.replaceChildren();bugs.mount(host);await bugs.show(id)}
await (legacy?showQueue(task):showBugs(task));window.qa={view,bugs,showQueue,showBugs};
</script></body></html>`;

try {
  await exec("go", ["build", "-o", binary, "./cmd/tailterm-hub"], {
    cwd: path.join(root, "hub"),
  });
  const hubPort = await freePort();
  hub = `http://127.0.0.1:${hubPort}`;
  backend = spawn(binary, {
    env: {
      ...fixtureEnv,
      TAILTERM_STATE: path.join(state, "hub"),
      TAILTERM_DEV_LISTEN: `127.0.0.1:${hubPort}`,
      TAILTERM_TCP_LISTEN: "",
    },
    stdio: "ignore",
  });
  await waitFor(
    () =>
      fetch(`${hub}/v1/tasks`)
        .then((r) => r.ok)
        .catch(() => false),
    "isolated Queue hub",
  );

  const source = await api(
    "POST",
    "/v1/tasks",
    { name: "Synthetic Queue source" },
    201,
  );
  const target = await api(
    "POST",
    "/v1/tasks",
    { name: "Synthetic Queue target" },
    201,
  );
  const lead = await api(
    "POST",
    `/v1/tasks/${target.id}/agents`,
    {
      name: "queue-lead",
      host: "synthetic.invalid",
      session: "no-provider",
      runtime: "codex",
      cwd: state,
    },
    201,
  );
  await api("PATCH", `/v1/tasks/${target.id}`, { orchestrator: lead.name });
  web = createServer(async (request, response) => {
    try {
      const url = new URL(request.url, "http://127.0.0.1");
      if (url.pathname === "/") {
        response.writeHead(200, { "content-type": "text/html" });
        response.end(html);
        return;
      }
      if (url.pathname === "/qa/lose-next-action") {
        loseNextAction = true;
        response.writeHead(204).end();
        return;
      }
      if (url.pathname.startsWith("/v1/")) {
        const chunks = [];
        for await (const chunk of request) chunks.push(chunk);
        const raw = Buffer.concat(chunks);
        const isAction =
          request.method === "POST" &&
          /\/queue\/[^/]+\/actions$/.test(url.pathname);
        if (isAction) actionAttempts.push(JSON.parse(raw));
        const upstream = await fetch(hub + request.url, {
          method: request.method,
          headers: raw.length
            ? { "content-type": "application/json" }
            : undefined,
          body: ["GET", "HEAD"].includes(request.method) ? undefined : raw,
        });
        const text = await upstream.text();
        if (isAction && loseNextAction) {
          loseNextAction = false;
          response.writeHead(503, { "content-type": "application/json" });
          response.end(
            JSON.stringify({ error: "Synthetic lost Queue response" }),
          );
          return;
        }
        response.writeHead(upstream.status, {
          "content-type":
            upstream.headers.get("content-type") || "application/json",
        });
        response.end(text);
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
      response.writeHead(500, { "content-type": "application/json" });
      response.end(JSON.stringify({ error: error.message }));
    }
  });
  web.listen(0, "127.0.0.1");
  await once(web, "listening");
  const origin = `http://127.0.0.1:${web.address().port}`;

  for (const engine of [chromium, webkit]) {
    const engineName = engine.name();
    const title = `Browser Queue item ${engineName}`;
    const item = await api(
      "POST",
      `/v1/tasks/${source.id}/work-items`,
      {
        kind: "bug",
        title,
        description: "Synthetic Queue acceptance only",
        priority: "normal",
        requestId: `browser-queue-item-${engineName}`,
      },
      201,
    );
    const order = await api(
      "POST",
      `/v1/tasks/${source.id}/messages`,
      {
        text: `Bounded synthetic Queue order ${engineName}`,
        requestId: `browser-queue-order-${engineName}`,
        auditKind: "work",
        workItems: [
          {
            itemTaskId: source.id,
            itemId: item.id,
            itemRevision: item.revision,
            relationship: "primary",
          },
        ],
      },
      201,
    );
    const attemptStart = actionAttempts.length;
    const browser = await engine.launch();
    try {
      const context = await browser.newContext();
      const page = await context.newPage();
      const errors = [];
      page.on("pageerror", (error) => errors.push(error.message));
      await page.goto(`${origin}/?task=${source.id}`);
      await page.getByText(title, { exact: true }).first().waitFor();
      const beforeAgents = await api("GET", `/v1/tasks/${target.id}/agents`);
      await page.locator(`[data-item-send="${item.id}"]`).click();
      assert.match(
        await page.locator("#dialog").innerText(),
        /never starts an agent/i,
      );
      await page.locator("#work-item-target").selectOption(target.id);
      await page.locator('#work-item-dispatch button[type="submit"]').click();
      await page.waitForFunction(() =>
        document.querySelector("#notice")?.textContent.includes("enqueued"),
      );
      const afterSend = await api(
        "GET",
        `/v1/tasks/${target.id}/queue?limit=64`,
      );
      const sentEntry = afterSend.entries.find(
        (entry) => entry.itemId === item.id,
      );
      assert.ok(sentEntry, "UI Send did not atomically enqueue the item");
      await page.evaluate((taskID) => window.qa.showQueue(taskID), target.id);
      await page.getByText(title, { exact: true }).first().waitFor();
      assert.match(
        await page.locator(".queue-main").innerText(),
        /never start an agent/i,
      );
      await page.locator("[data-queue-priority]").selectOption("urgent");
      await page.waitForFunction(() =>
        document
          .querySelector("#notice")
          ?.textContent.includes("No agent was started"),
      );
      await page.request.get(`${origin}/qa/lose-next-action`);
      await page.locator("[data-queue-priority]").selectOption("high");
      await page
        .getByText("Synthetic lost Queue response", { exact: true })
        .waitFor();
      await page.getByRole("button", { name: "Retry exact request" }).click();
      await page.waitForFunction(
        () => !document.querySelector("[data-queue-retry-intent]"),
      );
      await page.getByRole("button", { name: "Pull", exact: true }).click();
      await page.locator('input[name="orderTask"]').fill(source.id);
      await page.locator('input[name="orderSeq"]').fill(String(order.seq));
      await page.getByRole("button", { name: "Pull only" }).click();
      await page.getByText("Claimed", { exact: true }).first().waitFor();
      const afterAgents = await api("GET", `/v1/tasks/${target.id}/agents`);
      assert.equal(
        afterAgents.agents.length,
        beforeAgents.agents.length,
        "Queue UI action launched an agent",
      );
      const history = await api(
        "GET",
        `/v1/tasks/${target.id}/queue/${sentEntry.id}/history?limit=64`,
      );
      assert.equal(
        history.events.filter((event) => event.kind === "priority").length,
        2,
        "exact retry duplicated Queue priority event",
      );
      const uncertainAttempts = actionAttempts
        .slice(attemptStart)
        .filter(
          (attempt) =>
            attempt.operation === "priority" && attempt.priority === "high",
        );
      assert.equal(uncertainAttempts.length, 2);
      assert.equal(
        uncertainAttempts[0].requestId,
        uncertainAttempts[1].requestId,
        "uncertain retry changed identity",
      );
      await page.getByRole("button", { name: "History" }).click();
      await page.locator(".queue-history article").first().waitFor();
      await page.locator("#dialog").evaluate((element) => element.close());
      const dismissed = await api(
        "PATCH",
        `/v1/tasks/${source.id}/work-items/${item.id}`,
        { revision: item.revision, status: "dismissed" },
      );
      assert.equal(dismissed.status, "dismissed");
      const afterDismiss = await api(
        "GET",
        `/v1/tasks/${target.id}/queue?limit=64`,
      );
      assert.equal(afterDismiss.entries.length, 0);
      await page.evaluate(async (taskID) => {
        window.qa.view.hide();
        await window.qa.view.show(taskID);
      }, target.id);
      await page
        .getByText("No entries match this project and history view.", {
          exact: true,
        })
        .waitFor();
      await page.locator("[data-queue-terminal]").check();
      await page.getByText("Cancelled", { exact: true }).first().waitFor();
      assert.deepEqual(errors, []);

      const old = await context.newPage();
      await old.goto(`${origin}/?task=${target.id}&legacy=1`);
      await old.getByText(/does not advertise Queue v1/).waitFor();
      assert.equal(
        await old.locator("[data-queue-priority], [data-queue-pull]").count(),
        0,
      );
      console.log(
        `${engine.name()}: Queue Send/priority/retry/Pull/history/terminal/unsupported passed without launch.`,
      );
    } finally {
      await browser.close();
    }
  }
} finally {
  if (web) await new Promise((resolve) => web.close(resolve));
  if (backend && !backend.killed) {
    backend.kill("SIGTERM");
    await Promise.race([once(backend, "exit"), pause(2000)]).catch(() => {});
  }
  await rm(state, { recursive: true, force: true });
}
