// Focused Chromium/WebKit checks against a real, isolated Go hub database.
import { chromium, webkit } from "@playwright/test";
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
  state = await mkdtemp(path.join(tmpdir(), "tailterm-task-form-"));
const reserve = createServer();
reserve.listen(0, "127.0.0.1");
await once(reserve, "listening");
const port = reserve.address().port;
await new Promise((r) => reserve.close(r));
const backend = spawn(path.join(root, ".build/ttbin/tailterm-hub-test"), {
  env: {
    ...process.env,
    TAILTERM_STATE: state,
    TAILTERM_DEV_LISTEN: `127.0.0.1:${port}`,
    TAILTERM_TCP_LISTEN: "",
  },
  stdio: "ignore",
});
let failLaunch = false,
  execs = 0;
const html = `<!doctype html><html><head><link rel="stylesheet" href="/client/style.css"></head><body><div id="app"><div id="workspace"><aside></aside><main><header><div class="header-right"><button id="tailscale-login"><span class="online"></span>Connected</button></div></header><section class="terminal-shell"></section></main></div><dialog id="dialog"></dialog><div id="notice" hidden></div></div><script type="module">
import {createTaskHub} from '/client/task-hub.js';
import {createBoardView} from '/client/board-view.js';
import {createTasksView} from '/client/tasks-view.js';
import {setupModes} from '/client/modes.js';
let board, tasks, modes;const data={hub:{url:location.origin},launchProfiles:[]};
const servers=[{id:'local',name:'Test host',host:'stephens-macbook-air',username:'test',runtimes:['claude']}];
const model={groups:[],taskGroup:()=>null};
const host={getIPN:()=>({fetch:(url,init)=>fetch(url,init)}),getData:()=>data,getServers:()=>servers,currentServer:()=>servers[0],currentTab:()=>null,getTabs:()=>[],paneGroups:()=>({model,sync(){}}),render(){},scheduleWorkspaceSave(){},bookmark(){},closeTab(){},connect:async()=>null,notice:t=>{document.querySelector('#notice').textContent=t},api:async()=>({sessions:[]}),
 dialog:(title,body)=>{const d=document.querySelector('#dialog');if(d.open)d.close();d.innerHTML='<div class="dialog-head"><h2>'+title+'</h2><button id="dialog-close">×</button></div>'+body;d.querySelector('#dialog-close').onclick=()=>host.closeDialog();d.showModal()},
 closeDialog:()=>{const d=document.querySelector('#dialog');d.close();d.replaceChildren()},
 openBoard:id=>{modes.set('board');board.show(id)},
 browserCommand:async(server,command)=>{const r=await fetch('/exec',{method:'POST',body:JSON.stringify({command})});const d=await r.json();if(!r.ok)throw new Error(d.error);return d.output}
};
const hub=createTaskHub(host);hub.refresh();
board=createBoardView({client:()=>hub.client(),getTabs:()=>[],activate(){},notice:host.notice,newTask:()=>hub.newTask(),addAgent:id=>hub.addAgent(id),attachTask(){},configure(){}});
tasks=createTasksView({client:()=>hub.client(),taskHub:hub,getTabs:()=>[],activate(){},notice:host.notice,openBoard:host.openBoard,configure(){}});
modes=setupModes({header:document.querySelector('header'),main:document.querySelector('main'),onChange:(mode,view)=>{board.hide();tasks.hide();view.replaceChildren();if(mode==='board'){board.mount(view);board.show()}else if(mode==='tasks'){tasks.mount(view);tasks.show()}}});
modes.set('tasks');window.qa={hub,board,modes};
</script></body></html>`;
const server = createServer(async (req, res) => {
  try {
    if (req.url === "/") {
      res.setHeader("content-type", "text/html");
      res.end(html);
      return;
    }
    if (req.url.startsWith("/v1/")) {
      const chunks = [];
      for await (const b of req) chunks.push(b);
      const r = await fetch(`http://127.0.0.1:${port}` + req.url, {
        method: req.method,
        headers: { "content-type": "application/json" },
        body: ["GET", "HEAD"].includes(req.method)
          ? undefined
          : Buffer.concat(chunks),
      });
      res.writeHead(r.status, { "content-type": "application/json" });
      res.end(await r.text());
      return;
    }
    if (req.url === "/exec") {
      execs++;
      const chunks = [];
      for await (const b of req) chunks.push(b);
      const { command } = JSON.parse(Buffer.concat(chunks));
      if (failLaunch) {
        failLaunch = false;
        res.writeHead(500, { "content-type": "application/json" });
        res.end(JSON.stringify({ error: "Test launch failure" }));
        return;
      }
      // Point the SSH-generated command at the isolated hub listener.
      const result = await exec(
        "/bin/sh",
        ["-c", command.replaceAll(origin, `http://127.0.0.1:${port}`)],
        {
          env: { ...process.env, TT_TMUX_SOCKET: "tailterm-form-check" },
          timeout: 20000,
        },
      );
      res.setHeader("content-type", "application/json");
      res.end(JSON.stringify({ output: result.stdout }));
      return;
    }
    const file = path.resolve(root, "." + decodeURIComponent(req.url));
    if (!file.startsWith(root + "/")) throw new Error("invalid path");
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
    res.writeHead(500, { "content-type": "application/json" });
    res.end(JSON.stringify({ error: e.message }));
  }
});
server.listen(0, "127.0.0.1");
await once(server, "listening");
const origin = `http://127.0.0.1:${server.address().port}`;
const getTasks = async () =>
  (await (await fetch(`http://127.0.0.1:${port}/v1/tasks`)).json()).tasks;
try {
  for (let i = 0; i < 50; i++) {
    try {
      await getTasks();
      break;
    } catch {
      await new Promise((r) => setTimeout(r, 100));
    }
  }
  for (const [name, engine] of [
    ["chromium", chromium],
    ["webkit", webkit],
  ]) {
    const browser = await engine.launch();
    try {
      const page = await browser.newPage({
        viewport: { width: 1100, height: 780 },
      });
      const errors = [];
      page.on("pageerror", (e) => {
        errors.push(e.message);
        console.error(name, e.message);
      });
      await page.goto(origin);
      await page.waitForFunction(() => !!window.qa);
      await page.locator("#tasks-new").waitFor({ state: "attached" });
      if (!(await page.locator("#tasks-new").isVisible())) {
        console.error(
          await page.locator("#tasks-new").evaluate((el) => {
            const out = [];
            for (; el; el = el.parentElement) {
              const s = getComputedStyle(el);
              out.push([
                el.tagName,
                el.id,
                s.display,
                s.visibility,
                el.getBoundingClientRect().toJSON(),
              ]);
            }
            return out;
          }),
        );
        await page.screenshot({ path: ".build/task-form-error.png" });
        throw new Error("task button hidden");
      }
      const selected = page.locator(".mode-switch [data-mode=tasks]"),
        other = page.locator(".mode-switch [data-mode=board]");
      const color = (el) => getComputedStyle(el).backgroundColor;
      assert.notEqual(
        await selected.evaluate(color),
        await other.evaluate(color),
      );
      const fill = await selected.evaluate(color);
      await selected.hover();
      assert.equal(await selected.evaluate(color), fill);
      const otherFill = await other.evaluate(color);
      await other.hover();
      assert.equal(await other.evaluate(color), otherFill);
      await page.locator("#tasks-new").click();
      await page.locator("#task-create").click();
      await page
        .locator("#task-error")
        .filter({ hasText: "Enter a task name" })
        .waitFor();
      await page.locator("#task-name").fill(name + " task with spaces");
      await page
        .locator("#task-goal")
        .fill("A normal multi-line objective.\nSecond line.");
      await page.locator("#task-create").click();
      await page.locator("#dialog").waitFor({ state: "hidden" });
      await page
        .locator(".board-head h2")
        .filter({ hasText: name + " task with spaces" })
        .waitFor();
      assert.ok(
        (await getTasks()).some((t) => t.name === name + " task with spaces"),
      );
      assert.equal(execs, name === "chromium" ? 0 : 2);
      await page.evaluate(() => qa.hub.newTask());
      await page.locator("#task-name").fill(name + " launch retry");
      await page.locator("#task-with-agent").check();
      await page.locator("#agent-runtime").selectOption("");
      await page.locator("#task-agent-fields summary").click();
      await page.locator("#agent-run").fill("sleep 10");
      await page.locator("#agent-name").fill("probe-" + name);
      await page.locator("#agent-prompt").fill("Multi-line\nassignment");
      failLaunch = true;
      await page.locator("#task-create").click();
      await page
        .locator("#task-error")
        .filter({
          hasText: "Task created. Agent launch failed: Test launch failure",
        })
        .waitFor();
      assert.equal(
        (await getTasks()).filter((t) => t.name === name + " launch retry")
          .length,
        1,
      );
      await page.locator("#task-create").click();
      await page.locator("#dialog").waitFor({ state: "hidden" });
      assert.equal(
        (await getTasks()).filter((t) => t.name === name + " launch retry")
          .length,
        1,
      );
      await page
        .locator(".board-head h2")
        .filter({ hasText: name + " launch retry" })
        .waitFor();
      await page.setViewportSize({ width: 390, height: 650 });
      await page.evaluate(() => qa.hub.newTask());
      await page.locator("#task-name").fill(name + " mobile task");
      await page.locator("#task-create").click();
      await page.locator("#dialog").waitFor({ state: "hidden" });
      await page
        .locator(".board-head h2")
        .filter({ hasText: name + " mobile task" })
        .waitFor();
      const compose = await page.locator("#board-compose").boundingBox();
      assert.ok(
        compose.y >= 0 && compose.y + compose.height <= 650,
        JSON.stringify(compose),
      );
      await page.mouse.move(0, 0);
      await page.screenshot({ path: ".build/task-flow-" + name + ".png" });
      assert.deepEqual(errors, []);
      console.log(
        name +
          ": creation, inline validation, optional launch, multiline assignment, failure/retry without duplicates, mobile, mode selection passed.",
      );
    } finally {
      await browser.close();
    }
  }
} finally {
  await exec("tmux", ["-L", "tailterm-form-check", "kill-server"]).catch(
    () => {},
  );
  backend.kill("SIGTERM");
  server.closeAllConnections();
  await new Promise((r) => server.close(r));
  await once(backend, "exit");
  await rm(state, { recursive: true, force: true });
}
