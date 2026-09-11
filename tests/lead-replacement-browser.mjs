// Browser acceptance checks for Bugs/Features against a real, throwaway hub.
// No Tailnet credentials, live project data, or shared tmux sockets are used.
import { chromium, webkit } from "@playwright/test";
import { createServer } from "node:http";
import { once } from "node:events";
import { spawn, execFile } from "node:child_process";
import { promisify } from "node:util";
import { mkdtemp, readFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import assert from "node:assert/strict";

const exec = promisify(execFile);
const root = process.cwd();
const state = await mkdtemp(path.join(tmpdir(), "tailterm-lead-"));
// This test itself is coordinated through Tailterm. Its inherited identity must
// never become part of the isolated fixture's hub requests or launch commands.
const fixtureEnv = Object.fromEntries(
  Object.entries(process.env).filter(([key]) => !key.startsWith("TAILTERM_")),
);
const binary = path.join(root, ".build/ttbin/tailterm-hub-lead-test");
await exec(
  "go",
  ["build", "-ldflags=-s -w", "-o", binary, "./cmd/tailterm-hub"],
  {
    cwd: path.join(root, "hub"),
  },
);
const reserve = createServer();
reserve.listen(0, "127.0.0.1");
await once(reserve, "listening");
const hubPort = reserve.address().port;
await new Promise((resolve) => reserve.close(resolve));
const backend = spawn(binary, {
  env: {
    ...fixtureEnv,
    TAILTERM_STATE: state,
    TAILTERM_DEV_LISTEN: `127.0.0.1:${hubPort}`,
    TAILTERM_TCP_LISTEN: "",
  },
  stdio: "ignore",
});

const hub = `http://127.0.0.1:${hubPort}`;
async function api(method, route, body) {
  const response = await fetch(hub + route, {
    method,
    headers:
      body === undefined ? undefined : { "content-type": "application/json" },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const text = await response.text();
  const data = text ? JSON.parse(text) : null;
  if (!response.ok) {
    const error = new Error(data?.error || response.statusText);
    error.status = response.status;
    throw error;
  }
  return data;
}
for (let i = 0; i < 100; i++) {
  try {
    await api("GET", "/v1/tasks");
    break;
  } catch {
    await new Promise((r) => setTimeout(r, 50));
  }
}
let dropAssignment = false;
const html = `<!doctype html><html><head><link rel="stylesheet" href="/client/style.css"></head><body><div id="app"><div id="projects"></div><dialog id="dialog"></dialog><p id="notice"></p></div><script type="module">
import {createTaskHub} from '/client/task-hub.js';
import {createTasksView} from '/client/tasks-view.js';
import {createHubClient} from '/client/hub-client.js';
import * as vault from '/client/local-vault.js';
await vault.localAPI('/unlock','POST',{password:'isolated lead recovery browser passphrase'});
const data={hub:{url:location.origin},projectHandlerPlans:[],teamLaunchPlans:(await vault.localAPI('/data')).teamLaunchPlans||[]};
const servers=[{id:'fixture',name:'Fixture host',host:'host-fixture',username:'fixture',runtimes:['codex']}];
const client=createHubClient({baseURL:location.origin,fetchImpl:(u,i)=>fetch(u,i)});
const host={getIPN:()=>({fetch:(u,i)=>fetch(u,i)}),getData:()=>data,getServers:()=>servers,currentServer:()=>servers[0],currentTab:()=>null,getTabs:()=>[],paneGroups:()=>({model:{groups:[]},sync(){}}),render(){},scheduleWorkspaceSave(){},bookmark(){},closeTab(){},connect:async()=>null,
 notice:t=>document.querySelector('#notice').textContent=t,
 dialog:(title,body)=>{const d=document.querySelector('#dialog');if(d.open)d.close();d.innerHTML='<div class="dialog-head"><h2>'+title+'</h2></div>'+body;d.showModal()},
 closeDialog:()=>document.querySelector('#dialog').close(),
 api:(u,m,b)=>vault.localAPI(u,m,b),reloadData:async()=>{data.teamLaunchPlans=(await vault.localAPI('/data')).teamLaunchPlans||[]},
 browserCommand:async(server,command)=>{
  const plan=data.teamLaunchPlans.find(p=>p.kind==='replace-lead');
  const m=plan.members[0];window.qa.launches++;
  const a=await client.addAgent(plan.taskId,{agentId:m.fields.agentId,name:m.fields.name,host:server.host,session:m.fields.name,runtime:m.fields.runtime,cwd:m.fields.cwd});
  await fetch('/v1/tasks/'+plan.taskId+'/events',{method:'POST',headers:{'content-type':'application/json'},body:JSON.stringify({agentId:a.id,runId:a.runId,kind:'started'})});
  if(window.qa.loseLaunch)throw Error('Synthetic lost launch response');return JSON.stringify(a);
 }
};
const taskHub=createTaskHub(host);taskHub.refresh();
const view=createTasksView({client:()=>client,taskHub,getTabs:()=>[],notice:host.notice,openBoard(){},configure(){}});
view.mount(document.querySelector('#projects'));await view.show();
window.qa={taskHub,client,host,data,launches:0,loseLaunch:false};
</script></body></html>`;
const web = createServer(async (req, res) => {
  try {
    if (req.url === "/") {
      res.setHeader("content-type", "text/html");
      res.end(html);
      return;
    }
    if (req.url.startsWith("/v1/")) {
      const chunks = [];
      for await (const c of req) chunks.push(c);
      const r = await fetch(hub + req.url, {
        method: req.method,
        headers: { "content-type": "application/json" },
        body: ["GET", "HEAD"].includes(req.method)
          ? undefined
          : Buffer.concat(chunks),
      });
      const b = await r.text();
      if (
        dropAssignment &&
        req.method === "POST" &&
        req.url.endsWith("/lead")
      ) {
        dropAssignment = false;
        res.writeHead(503, { "content-type": "application/json" });
        res.end(
          JSON.stringify({ error: "Synthetic lost assignment response" }),
        );
        return;
      }
      res.writeHead(r.status, { "content-type": "application/json" });
      res.end(b);
      return;
    }
    if (req.url === "/wasm/tailserve.wasm?url") {
      res.setHeader("content-type", "text/javascript");
      res.end('export default "/wasm/tailserve.wasm"');
      return;
    }
    const file = path.resolve(
      root,
      "." + decodeURIComponent(req.url.split("?")[0]),
    );
    if (!file.startsWith(root + path.sep)) throw Error("invalid path");
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
});
web.listen(0, "127.0.0.1");
await once(web, "listening");
const origin = `http://127.0.0.1:${web.address().port}`;
const add = async (task, name) => {
  const a = await api("POST", `/v1/tasks/${task.id}/agents`, {
    name,
    host: "host-fixture",
    session: name,
    runtime: "codex",
    cwd: "/tmp",
  });
  await api("POST", `/v1/tasks/${task.id}/events`, {
    agentId: a.id,
    runId: a.runId,
    kind: "started",
  });
  return a;
};
try {
  for (const engine of [chromium, webkit]) {
    const browser = await engine.launch();
    const context = await browser.newContext();
    const page = await context.newPage();
    const errors = [];
    page.on("pageerror", (e) => errors.push(e.message));
    try {
      const task = await api("POST", "/v1/tasks", {
        name: "Recovery " + engine.name(),
        orchestrator: "old-lead",
      });
      const old = await add(task, "old-lead");
      const candidate = await add(task, "candidate");
      await api("POST", `/v1/tasks/${task.id}/events`, {
        agentId: old.id,
        runId: old.runId,
        kind: "exited",
      });
      await page.goto(origin);
      await page.waitForFunction(() => window.qa);
      await page.locator(`[data-task-select="${task.id}"]`).click();
      await page.locator(`[data-task-lead="${task.id}"]`).click();
      await page.locator("#lead-candidate").selectOption(candidate.id);
      dropAssignment = true;
      await page.locator("#lead-submit").click();
      await page
        .locator("#task-error")
        .filter({ hasText: "Synthetic lost assignment response" })
        .waitFor();
      await page.locator("#lead-submit").click();
      await page.waitForFunction(() => !document.querySelector("#dialog").open);
      let detail = await api("GET", `/v1/tasks/${task.id}`);
      assert.equal(detail.task.orchestrator, "candidate");
      assert.equal(
        (await api("GET", `/v1/tasks/${task.id}/messages?after=0&limit=100`))
          .messages.length,
        1,
      );
      // Start a new candidate, lose its response after registration, then recover
      // the encrypted journal on reload. No second spawn is permitted.
      await page.evaluate((id) => qa.taskHub.replaceLead(id), task.id);
      await page.locator("#agent-cwd").fill("/tmp");
      await page.locator("#agent-name").fill("fresh-lead");
      await page.evaluate(() => (qa.loseLaunch = true));
      await page.locator("#lead-submit").click();
      await page
        .locator("#task-error")
        .filter({ hasText: "Synthetic lost launch response" })
        .waitFor();
      assert.equal(await page.evaluate(() => qa.launches), 1);
      await page.reload();
      await page.waitForFunction(() => window.qa);
      await page.evaluate((id) => qa.taskHub.replaceLead(id), task.id);
      await page.locator("#lead-submit").click();
      await page.waitForFunction(() => !document.querySelector("#dialog").open);
      assert.equal(await page.evaluate(() => qa.launches), 0);
      detail = await api("GET", `/v1/tasks/${task.id}`);
      assert.equal(detail.task.orchestrator, "fresh-lead");
      assert.equal(detail.agents.length, 3);
      assert.equal(
        await page.evaluate(() => qa.data.teamLaunchPlans.length),
        0,
      );
      // A stale open dialog must not overwrite another owner's newer selection.
      await page.evaluate((id) => qa.taskHub.replaceLead(id), task.id);
      await page.locator("#lead-candidate").selectOption(candidate.id);
      await api("PATCH", `/v1/tasks/${task.id}`, {
        orchestrator: "owner-new-choice",
      });
      await page.locator("#lead-submit").click();
      await page
        .locator("#task-error")
        .filter({ hasText: "project lead changed" })
        .waitFor();
      detail = await api("GET", `/v1/tasks/${task.id}`);
      assert.equal(detail.task.orchestrator, "owner-new-choice");
      for (const width of [1280, 390]) {
        await page.setViewportSize({ width, height: 900 });
        assert.equal(
          await page
            .locator("#replace-lead-form")
            .evaluate((e) => e.scrollWidth <= e.clientWidth + 1),
          true,
        );
      }
      assert.deepEqual(errors, []);
      console.log(
        engine.name() +
          ": existing selection, lost response retry, fresh launch, encrypted reload, exact recovery, stale conflict, compact layouts PASS",
      );
    } finally {
      await context.close();
      await browser.close();
    }
  }
} finally {
  await new Promise((r) => web.close(r));
  backend.kill();
  await once(backend, "exit");
  await rm(state, { recursive: true, force: true });
}
