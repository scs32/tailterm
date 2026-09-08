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
await exec(
  "go",
  [
    "build",
    "-o",
    path.join(root, ".build/ttbin/tailterm-hub-test"),
    "./cmd/tailterm-hub",
  ],
  { cwd: path.join(root, "hub") },
);
await exec("go", ["build", "-o", path.join(state, "tt"), "./cmd/tt"], {
  cwd: path.join(root, "hub"),
});
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
const launchServers = [];
const launchCommands = [];
let failLaunch = false,
  failAt = 0,
  execs = 0;
const html = `<!doctype html><html><head><meta charset="utf-8"><link rel="stylesheet" href="/client/style.css"></head><body><div id="app"><div id="workspace"><aside></aside><main><header><div class="header-right"><button id="tailscale-login"><span class="online"></span>Connected</button></div></header><section class="terminal-shell"></section></main></div><dialog id="dialog"></dialog><div id="notice" hidden></div></div><script type="module">
import {createTaskHub} from '/client/task-hub.js';
import {createBoardView} from '/client/board-view.js';
import {createTasksView} from '/client/tasks-view.js';
import {createTeamsView} from '/client/teams-view.js';
import {setupModes} from '/client/modes.js';
let board, tasks, modes, teams;const data={hub:{url:location.origin},launchProfiles:[],teams:[]};
const servers=[{id:'local',name:'Test host',host:'stephens-macbook-air',username:'test',runtimes:['claude']},{id:'secondary',name:'Second host',host:'second-fixture',username:'test',runtimes:['codex']}];
const model={groups:[],taskGroup:()=>null};
const host={getIPN:()=>({fetch:(url,init)=>fetch(url,init)}),getData:()=>data,getServers:()=>servers,currentServer:()=>servers[0],currentTab:()=>null,getTabs:()=>[],paneGroups:()=>({model,sync(){}}),render(){},scheduleWorkspaceSave(){},bookmark(){},closeTab(){},connect:async()=>null,notice:t=>{document.querySelector('#notice').textContent=t},api:async(url,method,body)=>{if(url==="/teams")data.teams=[...data.teams.filter(t=>t.id!==body.id),body];if(url.startsWith("/teams/"))data.teams=data.teams.filter(t=>t.id!==url.slice(7));if(url==="/launch-profiles")data.launchProfiles=[...data.launchProfiles.filter(p=>p.name!==body.name),body];return {sessions:[]}},reloadData:async()=>{},
 dialog:(title,body)=>{const d=document.querySelector('#dialog');if(d.open)d.close();d.innerHTML='<div class="dialog-head"><h2>'+title+'</h2><button id="dialog-close">×</button></div>'+body;d.querySelector('#dialog-close').onclick=()=>host.closeDialog();d.showModal()},
 closeDialog:()=>{const d=document.querySelector('#dialog');d.close();d.replaceChildren()},
 openBoard:id=>{modes.set('board');board.show(id)},
 browserCommand:async(server,command)=>{const r=await fetch('/exec',{method:'POST',body:JSON.stringify({command,serverId:server.id})});const d=await r.json();if(!r.ok)throw new Error(d.error);return d.output}
};
const hub=createTaskHub(host);hub.refresh();
board=createBoardView({client:()=>hub.client(),getTabs:()=>[],activate(){},notice:host.notice,newTask:()=>hub.newTask(),addAgent:id=>hub.addAgent(id),attachTask(){},configure(){}});
tasks=createTasksView({client:()=>hub.client(),taskHub:hub,getTabs:()=>[],activate(){},notice:host.notice,openBoard:host.openBoard,configure(){}});
teams=createTeamsView({...host,confirm:async()=>true,newTask:t=>hub.newTask(undefined,t),addTeam:t=>hub.addTeam(t)});
modes=setupModes({header:document.querySelector('header'),main:document.querySelector('main'),onChange:(mode,view)=>{board.hide();tasks.hide();teams.hide();view.replaceChildren();if(mode==='board'){board.mount(view);board.show()}else if(mode==='teams'){teams.mount(view);teams.show()}else if(mode==='tasks'){tasks.mount(view);tasks.show()}}});
modes.set('tasks');window.qa={hub,board,modes,data};
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
      const { command, serverId } = JSON.parse(Buffer.concat(chunks));
      launchServers.push(serverId);
      launchCommands.push(command);
      if (failLaunch || execs === failAt) {
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
          env: {
            ...process.env,
            PATH: state + path.delimiter + process.env.PATH,
            TT_TMUX_SOCKET: "tailterm-form-check",
          },
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
      await page.locator("#task-allow-spawn").check();
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
      const created = (await getTasks()).find(
        (t) => t.name === name + " task with spaces",
      );
      assert.equal(created.allowAgentSpawn, true);
      await page.evaluate(() => qa.modes.set("tasks"));
      const card = page.locator(`[data-task-card="${created.id}"]`);
      await card.waitFor();
      const cardRect = async () =>
        (
          await page.waitForFunction((id) => {
            const el = document.querySelector(`[data-task-card="${id}"]`);
            if (!el || !el.getClientRects().length) return false;
            const { x, y, width, height } = el.getBoundingClientRect();
            return { x, y, width, height };
          }, created.id)
        ).jsonValue();
      const beforeMore = await cardRect();
      await card.locator("[data-task-more]").click();
      await card.locator(".task-menu:popover-open").waitFor();
      assert.deepEqual(
        await cardRect(),
        beforeMore,
        "More must not reflow the task row",
      );
      const menuSize = await card.locator(".task-menu").evaluate((el) => ({
        height: el.getBoundingClientRect().height,
        rows: [...el.children].map((b) => b.getBoundingClientRect().height),
      }));
      assert.equal(menuSize.height, 98, JSON.stringify(menuSize));
      assert.deepEqual(menuSize.rows, [28, 28, 28]);
      await page.screenshot({ path: ".build/task-menu-" + name + ".png" });
      await page.keyboard.press("Escape");
      assert.equal(await card.locator(".task-menu:popover-open").count(), 0);
      await card.locator("[data-task-more]").click();
      assert.equal(
        (await card.locator(".task-menu").boundingBox()).height,
        98,
        "Reopened menus must stay compact",
      );
      await card.locator("[data-task-settings]").click();
      await page.locator("#task-settings-spawn").uncheck();
      await page.locator("#task-settings button[type=submit]").click();
      await page.locator("#dialog").waitFor({ state: "hidden" });
      assert.equal(
        (await getTasks()).find((t) => t.id === created.id).allowAgentSpawn,
        false,
      );
      await page.evaluate((id) => {
        qa.modes.set("board");
        qa.board.show(id);
      }, created.id);
      await page
        .locator("#board-text")
        .fill(
          "The morning slips through curtains thin,\nAnd lets a little wonder in.",
        );
      await page.locator("#board-compose button[type=submit]").click();
      await page
        .locator(".board-text")
        .filter({ hasText: "The morning slips" })
        .waitFor();
      const bounds = await page.locator("#board-compose").evaluate((el) => {
        const box = (selector) =>
          el.querySelector(selector).getBoundingClientRect().toJSON();
        return {
          text: box("textarea"),
          send: box("button[type=submit]"),
          to: box("select"),
          width: el.clientWidth,
        };
      });
      assert.ok(
        bounds.send.height <= 30 && Math.abs(bounds.send.y - bounds.to.y) <= 1,
        JSON.stringify(bounds),
      );
      assert.ok(bounds.text.width >= bounds.width - 2, JSON.stringify(bounds));
      await page.mouse.move(0, 0);
      await page.screenshot({ path: ".build/board-compact-" + name + ".png" });

      await page.evaluate(() => qa.hub.newTask());
      await page.locator("#task-name").fill(name + " launch retry");
      await page.locator("#task-with-agent").check();
      await page.locator("#agent-runtime").selectOption("codex");
      await page.locator("#agent-model-choice").selectOption("__custom");
      await page.locator("#agent-model").fill("test-model-v2");
      await page
        .locator("#task-agent-fields .agent-launch-fields > details > summary")
        .click();
      await page.locator("#agent-runtime").selectOption("claude");
      assert.equal(await page.locator("#agent-model").inputValue(), "");
      await page.locator("#agent-runtime").selectOption("codex");
      await page.locator("#agent-model-choice").selectOption("__custom");
      await page.locator("#agent-model").fill("test-model-v2");
      const controls = await page
        .locator("#task-agent-fields")
        .evaluate((el) =>
          [...el.querySelectorAll("input,select")].map((e) => [
            e.id,
            e.getBoundingClientRect().height,
          ]),
        );
      assert.ok(
        controls.every(([, height]) => height === 36),
        JSON.stringify(controls),
      );
      const ink = await page
        .locator("#agent-model")
        .evaluate((el) => [
          getComputedStyle(el).color,
          getComputedStyle(el, "::placeholder").color,
        ]);
      assert.notEqual(ink[0], ink[1]);
      await page.screenshot({ path: ".build/task-setup-" + name + ".png" });
      await page.locator("#agent-runtime").selectOption("");
      assert.ok(await page.locator("#agent-model").isDisabled());
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
      await page.locator("#board-compose").waitFor({ state: "visible" });
      const compose = await (
        await page.waitForFunction(() => {
          const el = document.querySelector("#board-compose");
          if (!el || !el.getClientRects().length) return false;
          const r = el.getBoundingClientRect();
          return { y: r.y, height: r.height };
        })
      ).jsonValue();
      assert.ok(
        compose.y >= 0 && compose.y + compose.height <= 650,
        JSON.stringify(compose),
      );
      await page.mouse.move(0, 0);
      await page.screenshot({ path: ".build/task-flow-" + name + ".png" });
      await page.setViewportSize({ width: 1100, height: 820 });
      await page.evaluate(() => qa.modes.set("teams"));
      await page.locator("#teams-new").click();
      await page.locator("#team-name").fill("Review team");
      await page.locator("#team-swarm").check();
      await page.locator("[data-field=name]").fill("team-planner");
      await page.locator(".agent-controls > summary").click();
      await page
        .locator("[data-field=permissionMode]")
        .selectOption("on-request");
      await page.locator("[data-field=role]").fill("Planner");
      await page.locator("#team-model-choice").selectOption("__custom");
      await page.locator("[data-field=model]").fill("fixture-model");
      await page
        .locator("[data-field=prompt]")
        .fill("Inspect the task objective.");
      await page
        .locator("#team-member-editor > details:not(.agent-controls) > summary")
        .click();
      await page.locator("[data-field=run]").fill("sleep 30");
      await page.locator("#team-add-member").click();
      await page.locator("[data-field=name]").fill("team-reviewer");
      await page.locator("[data-field=role]").fill("Reviewer");
      await page
        .locator("#team-member-editor > details:not(.agent-controls) > summary")
        .click();
      await page.locator("[data-field=run]").fill("sleep 30");
      await page.locator("#team-form button[type=submit]").click();
      await page.locator("#dialog").waitFor({ state: "hidden" });
      assert.equal(await page.locator(".team-row").count(), 1);
      await page.screenshot({ path: ".build/teams-" + name + ".png" });
      const launchStart = execs;
      failAt = execs + 2;
      await page.locator("[data-new-task]").click();
      assert.notEqual(await page.locator("#task-team").inputValue(), "");
      await page.locator("#task-main-server").selectOption("secondary");
      await page.locator("#task-allow-spawn").check();
      await page.locator("#task-max-new-agents").fill("3");
      await page.locator("#task-name").fill(name + " team task");
      await page.locator("#task-create").click();
      await page
        .locator("#task-error")
        .filter({ hasText: "Test launch failure" })
        .waitFor();
      await page.locator("#task-create").click();
      await page.locator("#dialog").waitFor({ state: "hidden" });
      assert.equal(
        execs - launchStart,
        3,
        "retry starts only the failed team member",
      );
      const teamTask = (await getTasks()).find(
        (t) => t.name === name + " team task",
      );
      const detail = await (
        await fetch("http://127.0.0.1:" + port + "/v1/tasks/" + teamTask.id)
      ).json();
      assert.equal(detail.agents.length, 2);
      assert.equal(detail.task.maxNewAgents, 3);
      assert.equal(detail.task.swarm, true);
      assert.equal(detail.task.orchestrator, "team-planner");
      assert.match(launchCommands.at(-3), /--permission-mode/);
      assert.match(launchCommands.at(-3), /on-request/);
      const blockedAgent = await (
        await fetch(`http://127.0.0.1:${port}/v1/tasks/${teamTask.id}/agents`, {
          method: "POST",
          headers: { "content-type": "application/json" },
          body: JSON.stringify({
            name: "blocked-fixture",
            host: "stephens-macbook-air",
            session: "blocked-fixture",
          }),
        })
      ).json();
      const blockerResponse = await fetch(
        `http://127.0.0.1:${port}/v1/tasks/${teamTask.id}/events`,
        {
          method: "POST",
          headers: { "content-type": "application/json" },
          body: JSON.stringify({
            agentId: blockedAgent.id,
            runId: blockedAgent.runId,
            kind: "needs_input",
            text: "Test command requires permission",
            data: { reason: "permission" },
          }),
        },
      );
      assert.equal(blockerResponse.status, 201);
      await page
        .locator(".board-agent")
        .filter({ hasText: "blocked-fixture" })
        .filter({ hasText: "Permission blocked" })
        .waitFor();
      assert.equal(
        await page
          .locator(".board-agent")
          .filter({ hasText: "blocked-fixture" })
          .getAttribute("title"),
        "Test command requires permission",
      );

      assert.deepEqual(launchServers.slice(-3), [
        "secondary",
        "secondary",
        "secondary",
      ]);
      await page.evaluate(() => qa.modes.set("teams"));
      await page.locator("[data-add-team]").click();
      const target = (await getTasks()).find(
        (t) => t.name === name + " mobile task",
      );
      await page.locator("#team-task").selectOption(target.id);
      await page.locator("#team-main-server").selectOption("local");
      await page.locator("#team-launch-form button").click();
      await page.locator("#dialog").waitFor({ state: "hidden" });
      const attached = await (
        await fetch("http://127.0.0.1:" + port + "/v1/tasks/" + target.id)
      ).json();
      assert.equal(attached.agents.length, 2);
      await page.evaluate(() => qa.modes.set("teams"));
      await page.locator("[data-edit-team]").click();
      await page.setViewportSize({ width: 390, height: 650 });
      const dialog = await page.locator("#dialog").boundingBox();
      assert.ok(
        dialog.width <= 390 && dialog.height <= 650,
        JSON.stringify(dialog),
      );
      await page.screenshot({
        path: ".build/team-editor-mobile-" + name + ".png",
      });
      await page.locator("#dialog-close").click();
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
