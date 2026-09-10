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
// The test process is itself task-aware. Never leak its live coordinator
// identity into the isolated hub or a fixture tt child.
const fixtureEnv = Object.fromEntries(
  Object.entries(process.env).filter(([key]) => !key.startsWith("TAILTERM_")),
);
await exec(
  "go",
  [
    "build",
    "-ldflags=-s -w",
    "-o",
    path.join(root, ".build/ttbin/tailterm-hub-test"),
    "./cmd/tailterm-hub",
  ],
  { cwd: path.join(root, "hub") },
);
await exec(
  "go",
  ["build", "-ldflags=-s -w", "-o", path.join(state, "tt"), "./cmd/tt"],
  {
    cwd: path.join(root, "hub"),
  },
);
const reserve = createServer();
reserve.listen(0, "127.0.0.1");
await once(reserve, "listening");
const port = reserve.address().port;
await new Promise((r) => reserve.close(r));
const backend = spawn(path.join(root, ".build/ttbin/tailterm-hub-test"), {
  env: {
    ...fixtureEnv,
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
  loseLaunchAt = 0,
  loseTaskCreateReply = false,
  dropTaskCreateBeforeCommit = false,
  taskCreateRequests = 0,
  taskListGate = null,
  execs = 0,
  historyRequests = 0;
const html = `<!doctype html><html><head><meta charset="utf-8"><link rel="stylesheet" href="/client/style.css"></head><body><div id="app"><div id="workspace"><aside></aside><main><header><div class="header-right"><button id="tailscale-login"><span class="online"></span>Connected</button></div></header><section class="terminal-shell"></section></main></div><dialog id="dialog"></dialog><div id="notice" hidden></div></div><script type="module">
import {createTaskHub} from '/client/task-hub.js';
import {createBoardView} from '/client/board-view.js';
import {createTasksView} from '/client/tasks-view.js';
import {createTeamsView} from '/client/teams-view.js';
import {setupModes} from '/client/modes.js';
import {localAPI} from '/client/local-vault.js';
import {reconciledAgentProblem} from '/client/launch-reconciliation.js';
await localAPI('/unlock','POST',{password:'synthetic vault passphrase'});
const savedLocal=await localAPI('/data');
let board, tasks, modes, teams;const baseAgent=(id,name,serverId,role)=>({id,revision:1,name,launchName:name,role,serverId,runtime:'codex',model:'gpt-5.3-codex',reasoning:'',approvalMode:'on-request',sandboxMode:'workspace-write',permissionMode:'',allowedTools:[],run:"sh -c 'sleep 300' --",cwd:'',prompt:'Inspect the exact bounded assignment.'});const data={hub:{url:location.origin,token:'synthetic-token'},profile:{username:'synthetic',instanceId:'profile-one'},launchProfiles:[],agentCatalog:{version:2,definitions:[baseAgent('agent_team_planner','team-planner','', 'Planner'),baseAgent('agent_team_reviewer','team-reviewer','secondary','Reviewer')]},teamsVersion:2,teams:JSON.parse(localStorage.getItem('qa-teams')||'[]'),teamLaunchPlans:savedLocal.teamLaunchPlans,projectHandlerPlans:[]};
const servers=[{id:'local',name:'Test host',host:'stephens-macbook-air',username:'test',runtimes:['claude'],credentialRevision:1},{id:'secondary',name:'Second host',host:'second-fixture',username:'test',runtimes:['codex'],credentialRevision:1}];let failJournalWrite=false,commandDelay=null,journalDelay=null;
const model={groups:[],taskGroup:()=>null};
const host={openSFTP:async server=>({done:new Promise(()=>{}),home:async()=>${JSON.stringify(root)},realpath:async p=>p,list:async p=>({path:p,entries:p===${JSON.stringify(root)}?[{name:'hub',isDir:true},{name:'ignored.txt',isDir:false}]:[]}),close(){}}),getIPN:()=>({fetch:(url,init)=>fetch(url,init)}),getData:()=>data,getServers:()=>servers,launchServerProfile:id=>structuredClone(servers.find(server=>server.id===id)),currentServer:()=>servers[0],currentTab:()=>null,getTabs:()=>[],paneGroups:()=>({model,sync(){}}),render(){},scheduleWorkspaceSave(){},bookmark(){},closeTab(){},connect:async()=>null,confirm:async()=>true,notice:t=>{document.querySelector('#notice').textContent=t},api:async(url,method,body)=>{if(url.startsWith('/team-launch-plans')){if(journalDelay){const gate=journalDelay;gate.entered();await gate.wait;if(journalDelay===gate)journalDelay=null}if(url==='/team-launch-plans'&&method==='POST'&&failJournalWrite){failJournalWrite=false;throw new Error('Synthetic encrypted journal write failure')}const result=await localAPI(url,method,body);data.teamLaunchPlans=(await localAPI('/data')).teamLaunchPlans;return result}if(url==="/teams"){data.teams=[...data.teams.filter(t=>t.id!==body.id),body];localStorage.setItem('qa-teams',JSON.stringify(data.teams))}if(url.startsWith("/teams/")){data.teams=data.teams.filter(t=>t.id!==url.slice(7));localStorage.setItem('qa-teams',JSON.stringify(data.teams))}if(url==="/launch-profiles")data.launchProfiles=[...data.launchProfiles.filter(p=>p.name!==body.name),body];if(url==="/project-handler-plans"&&method==="POST")data.projectHandlerPlans=[...data.projectHandlerPlans.filter(p=>p.hub!==body.hub||p.taskId!==body.taskId),structuredClone(body)];return {sessions:[]}},reloadData:async()=>{data.teamLaunchPlans=(await localAPI('/data')).teamLaunchPlans},
 dialog:(title,body)=>{const d=document.querySelector('#dialog');if(d.open)d.close();d.innerHTML='<div class="dialog-head"><h2>'+title+'</h2><button id="dialog-close">×</button></div>'+body;d.querySelector('#dialog-close').onclick=()=>host.closeDialog();d.showModal()},
 closeDialog:()=>{const d=document.querySelector('#dialog');d.close();d.replaceChildren()},
 openBoard:id=>{modes.set('board');board.show(id)},
 browserCommand:async(server,command)=>{if(commandDelay){const gate=commandDelay;gate.entered();await gate.wait;if(commandDelay===gate)commandDelay=null}const r=await fetch('/exec',{method:'POST',body:JSON.stringify({command,serverId:server.id})});const d=await r.json();if(!r.ok){const error=new Error(d.error);error.verifiedUnstarted=d.verifiedUnstarted===true;throw error}return d.output}
};
const hub=createTaskHub(host);hub.refresh();
board=createBoardView({client:()=>hub.client(),getTabs:()=>[],activate(){},notice:host.notice,newTask:()=>hub.newTask(),addAgent:id=>hub.addAgent(id),attachTask(){},configure(){}});
tasks=createTasksView({confirm:async()=>true,client:()=>hub.client(),taskHub:hub,getTabs:()=>[],activate(){},notice:host.notice,openBoard:host.openBoard,configure(){}});
teams=createTeamsView({...host,confirm:async()=>true,newTask:t=>hub.newTask(undefined,t),addTeam:t=>hub.addTeam(t)});
modes=setupModes({header:document.querySelector('header'),main:document.querySelector('main'),onChange:(mode,view)=>{board.hide();tasks.hide();teams.hide();view.replaceChildren();if(mode==='board'){board.mount(view);board.show()}else if(mode==='teams'){teams.mount(view);teams.show()}else if(mode==='tasks'){tasks.mount(view);tasks.show()}}});
modes.set('tasks');window.qa={hub,board,modes,data,servers,localData:()=>localAPI('/data'),reconciledAgentProblem,failJournalWrite:()=>{failJournalWrite=true},delayNextJournal:()=>{let release,entered;const wait=new Promise(resolve=>release=resolve),started=new Promise(resolve=>entered=resolve);journalDelay={wait,release,entered,started}},waitForDelayedJournal:()=>journalDelay?.started,releaseJournal:()=>journalDelay?.release(),delayNextCommand:()=>{let release,entered;const wait=new Promise(resolve=>release=resolve),started=new Promise(resolve=>entered=resolve);commandDelay={wait,release,entered,started}},waitForDelayedCommand:()=>commandDelay?.started,releaseCommand:()=>commandDelay?.release(),clearLaunchPlans:async()=>{for(const plan of (await localAPI('/data')).teamLaunchPlans)await localAPI('/team-launch-plans/'+plan.id,'DELETE');data.teamLaunchPlans=(await localAPI('/data')).teamLaunchPlans},changeToken:token=>{data.hub={...data.hub,token}},changeProfile:instanceId=>{data.profile={...data.profile,instanceId};hub.refresh()},cycleProfile:()=>{data.profile={...data.profile,instanceId:'profile-two'};hub.refresh();data.profile={...data.profile,instanceId:'profile-one'};hub.refresh()},changeServerCredentials:()=>{servers[1].credentialRevision++;}};
</script></body></html>`;
const server = createServer(async (req, res) => {
  try {
    if (req.url === "/") {
      res.setHeader("content-type", "text/html");
      res.end(html);
      return;
    }
    if (req.url === "/qa/history-requests") {
      res.setHeader("content-type", "application/json");
      res.end(JSON.stringify({ historyRequests }));
      return;
    }
    if (req.url === "/qa/lose-task-create-reply") {
      loseTaskCreateReply = true;
      res.end("ok");
      return;
    }
    if (req.url === "/qa/drop-task-create-before-commit") {
      dropTaskCreateBeforeCommit = true;
      res.end("ok");
      return;
    }
    if (req.url === "/qa/lose-launch-reply") {
      loseLaunchAt = execs + 1;
      res.end("ok");
      return;
    }
    if (req.url.startsWith("/v1/")) {
      if (
        req.method === "GET" &&
        /\/work-items\/wi_[0-9a-f]{16}\/(?:revisions|history-gaps|messages)/.test(
          req.url,
        )
      )
        historyRequests++;
      const chunks = [];
      for await (const b of req) chunks.push(b);
      if (req.method === "GET" && req.url === "/v1/tasks" && taskListGate) {
        const gate = taskListGate;
        gate.entered = true;
        await gate.wait;
        if (taskListGate === gate) taskListGate = null;
      }
      if (req.method === "POST" && req.url === "/v1/tasks") {
        taskCreateRequests++;
        if (dropTaskCreateBeforeCommit) {
          dropTaskCreateBeforeCommit = false;
          res.writeHead(500, { "content-type": "application/json" });
          res.end(
            JSON.stringify({ error: "Synthetic uncommitted create response" }),
          );
          return;
        }
      }
      const r = await fetch(`http://127.0.0.1:${port}` + req.url, {
        method: req.method,
        headers: { "content-type": "application/json" },
        body: ["GET", "HEAD"].includes(req.method)
          ? undefined
          : Buffer.concat(chunks),
      });
      if (
        loseTaskCreateReply &&
        req.method === "POST" &&
        req.url === "/v1/tasks"
      ) {
        loseTaskCreateReply = false;
        await r.text();
        res.writeHead(500, { "content-type": "application/json" });
        res.end(JSON.stringify({ error: "Synthetic lost create response" }));
        return;
      }
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
        const verifiedUnstarted = !failLaunch && execs === failAt;
        failLaunch = false;
        res.writeHead(500, { "content-type": "application/json" });
        res.end(
          JSON.stringify({ error: "Test launch failure", verifiedUnstarted }),
        );
        return;
      }
      // Point the SSH-generated command at the isolated hub listener.
      const result = await exec(
        "/bin/sh",
        ["-c", command.replaceAll(origin, `http://127.0.0.1:${port}`)],
        {
          env: {
            ...fixtureEnv,
            PATH: state + path.delimiter + process.env.PATH,
            TAILTERM_HUB: `http://127.0.0.1:${port}`,
            TT_TMUX_SOCKET: "tailterm-form-check",
            TAILTERM_RELAY_STATE: path.join(state, "relay"),
          },
          timeout: 20000,
        },
      );
      if (execs === loseLaunchAt) {
        loseLaunchAt = 0;
        res.writeHead(500, { "content-type": "application/json" });
        res.end(JSON.stringify({ error: "Synthetic lost launch response" }));
        return;
      }
      res.setHeader("content-type", "application/json");
      res.end(JSON.stringify({ output: result.stdout }));
      return;
    }
    if (req.url.endsWith("?url")) {
      res.setHeader("content-type", "text/javascript");
      res.end(`export default ${JSON.stringify(req.url.slice(0, -4))}`);
      return;
    }
    const file = path.resolve(
      root,
      "." + decodeURIComponent(req.url.split("?", 1)[0]),
    );
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
      try {
        await page.waitForFunction(() => !!window.qa);
      } catch (error) {
        throw new Error(`${error.message}\n${errors.join("\n")}`);
      }
      await page.locator("#tasks-new").waitFor({ state: "attached" });
      assert.equal(await page.locator(".tasks-empty, .tasks-clear").count(), 0);
      await page.evaluate(() => qa.modes.set("teams"));
      await page.locator("#teams-new").waitFor();
      assert.equal(await page.locator(".tasks-empty").count(), 0);
      await page.evaluate(() => qa.modes.set("board"));
      await page.locator("#board-new-task").waitFor();
      assert.equal(await page.locator(".mode-empty, .board-empty").count(), 0);
      await page.evaluate(() => qa.modes.set("tasks"));
      await page.locator("#tasks-new").waitFor();
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
        .filter({ hasText: "Enter a project name" })
        .waitFor();
      await page.locator("#task-name").fill(name + " task with spaces");
      await page.locator("#task-allow-spawn").check();
      await page.locator("#agent-cwd").fill(root);
      await page
        .locator("#task-goal")
        .fill("A normal multi-line objective.\nSecond line.");
      await page.locator("#task-create").click();
      try {
        await page.locator("#dialog").waitFor({ state: "hidden" });
      } catch (error) {
        throw new Error(
          `${error.message}\n${await page.locator("#task-error").innerText()}`,
        );
      }
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
      await page.locator(`[data-task-select="${created.id}"]`).click();
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
      await page.locator("#task-create").click();
      await page
        .locator("#task-error")
        .filter({ hasText: "Choose a project folder" })
        .waitFor();
      await page
        .locator("#agent-cwd")
        .locator("..")
        .locator("[data-folder-browse]")
        .click();
      await page.locator('[data-folder-entry="0"]').click();
      await page.locator("[data-folder-use]").click();
      assert.equal(
        await page.locator("#agent-cwd").inputValue(),
        path.join(root, "hub"),
      );

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
          [...el.querySelectorAll("input:not([type=hidden]),select")].map(
            (e) => [e.id, e.getBoundingClientRect().height],
          ),
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
          hasText: "Project created. Agent launch failed: Test launch failure",
        })
        .waitFor();
      assert.equal(
        (await getTasks()).filter((t) => t.name === name + " launch retry")
          .length,
        1,
      );
      await page.locator("#task-create").click();
      try {
        await page.locator("#dialog").waitFor({ state: "hidden" });
      } catch (error) {
        throw new Error(
          `${error.message}\n${await page.locator("#task-error").innerText()}`,
        );
      }
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
      await page.locator("#agent-cwd").fill(root);
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
      await page.locator("[data-field=role]").fill("Planner");
      await page.locator("#team-add-member").click();
      await page
        .locator('[data-field="agentDefinitionId"]')
        .selectOption("agent_team_reviewer");
      await page.locator("[data-field=role]").fill("Reviewer");
      await page.locator("#team-form button[type=submit]").click();
      await page.locator("#dialog").waitFor({ state: "hidden" });
      assert.equal(await page.locator(".team-row").count(), 1);
      await page.screenshot({ path: ".build/teams-" + name + ".png" });

      const journalFailureName = name + " journal failure";
      const tasksBeforeJournalFailure = (await getTasks()).length;
      const execsBeforeJournalFailure = execs;
      await page.locator("[data-new-task]").click();
      await page.locator("#task-main-server").selectOption("secondary");
      await page.locator('[data-project-server="secondary"]').fill(root);
      await page.locator("#task-name").fill(journalFailureName);
      await page.evaluate(() => qa.failJournalWrite());
      await page.locator("#task-create").click();
      await page
        .locator("#task-error")
        .filter({ hasText: "Synthetic encrypted journal write failure" })
        .waitFor();
      assert.equal((await getTasks()).length, tasksBeforeJournalFailure);
      assert.equal(execs, execsBeforeJournalFailure);
      assert.equal(
        (
          await page.evaluate(
            async () => (await qa.localData()).teamLaunchPlans,
          )
        ).some((plan) => plan.creation?.request?.name === journalFailureName),
        false,
      );
      await page.locator("#dialog-close").click();

      // The synchronous single-flight guard also holds while the first
      // encrypted journal write is delayed and more submit signals arrive.
      await page.locator("[data-new-task]").click();
      await page.locator("#task-main-server").selectOption("secondary");
      await page.locator('[data-project-server="secondary"]').fill(root);
      const journalDelayName = name + " delayed journal single flight";
      await page.locator("#task-name").fill(journalDelayName);
      const journalDelayRequests = taskCreateRequests;
      const journalDelayExecs = execs;
      await page.evaluate(() => qa.delayNextJournal());
      await page.evaluate(() => {
        const form = document.querySelector("#task-form");
        document.querySelector("#task-create").click();
        form.requestSubmit();
      });
      await page.evaluate(() => qa.waitForDelayedJournal());
      await page.evaluate(() => {
        const form = document.querySelector("#task-form");
        form.requestSubmit();
        form.requestSubmit();
      });
      await page.evaluate(() => qa.releaseJournal());
      await page.locator("#dialog").waitFor({ state: "hidden" });
      assert.equal(
        (await getTasks()).filter((task) => task.name === journalDelayName)
          .length,
        1,
      );
      assert.equal(taskCreateRequests, journalDelayRequests + 1);
      assert.equal(execs, journalDelayExecs + 3);

      // A committed project with a lost HTTP reply remains uncertain. Field
      // equality is not a receipt, so neither same-dialog nor reload attempts
      // can adopt it or issue another create.
      await page.evaluate(() => qa.modes.set("teams"));
      await page.locator("[data-new-task]").click();
      await page.locator("#task-main-server").selectOption("secondary");
      await page.locator('[data-project-server="secondary"]').fill(root);
      const lostCreateName = name + " lost create same dialog";
      await page.locator("#task-name").fill(lostCreateName);
      const lostCreateRequests = taskCreateRequests;
      const lostCreateExecs = execs;
      await fetch(origin + "/qa/lose-task-create-reply");
      let releaseTaskList;
      taskListGate = {
        entered: false,
        wait: new Promise((resolve) => (releaseTaskList = resolve)),
        release: () => releaseTaskList(),
      };
      await page.evaluate(() => {
        const form = document.querySelector("#task-form");
        document.querySelector("#task-create").click();
        form.requestSubmit();
      });
      for (let attempt = 0; attempt < 100 && !taskListGate?.entered; attempt++)
        await new Promise((resolve) => setTimeout(resolve, 10));
      assert.equal(taskListGate?.entered, true);
      await page.evaluate(() => {
        const form = document.querySelector("#task-form");
        form.requestSubmit();
        form.requestSubmit();
      });
      taskListGate.release();
      await page
        .locator("#task-error")
        .filter({ hasText: "prior project-creation response is unknown" })
        .waitFor();
      assert.equal(
        (await getTasks()).filter((task) => task.name === lostCreateName)
          .length,
        1,
      );
      let persistedPlans = await page.evaluate(
        async () => (await qa.localData()).teamLaunchPlans,
      );
      const uncertainCreate = persistedPlans.find(
        (plan) => plan.creation?.request?.name === lostCreateName,
      );
      assert.equal(uncertainCreate.creation.state, "uncertain");
      assert.equal(uncertainCreate.taskId, undefined);
      assert.equal(taskCreateRequests, lostCreateRequests + 1);
      assert.equal(execs, lostCreateExecs);
      const encryptedJournal = await page.evaluate(
        () =>
          new Promise((resolve, reject) => {
            const open = indexedDB.open("tailserve", 1);
            open.onerror = () => reject(open.error);
            open.onsuccess = () => {
              const read = open.result
                .transaction("vault", "readonly")
                .objectStore("vault")
                .get("encrypted-v2");
              read.onerror = () => reject(read.error);
              read.onsuccess = () => resolve(read.result);
            };
          }),
      );
      assert.equal(encryptedJournal.version, 2);
      assert.doesNotMatch(encryptedJournal.ciphertext, /lost create/);
      await page.evaluate(() =>
        document
          .querySelector("#task-form")
          .dispatchEvent(
            new Event("submit", { bubbles: true, cancelable: true }),
          ),
      );
      await page
        .locator("#task-error")
        .filter({ hasText: "cannot safely identify" })
        .waitFor();
      assert.equal(
        taskCreateRequests,
        lostCreateRequests + 1,
        "uncertain retry sent another project create",
      );
      assert.equal(
        execs,
        lostCreateExecs,
        "uncertain retry launched into a possible match",
      );
      await page.reload();
      await page.waitForFunction(() => !!window.qa);
      await page.evaluate(() => qa.modes.set("teams"));
      await page.locator("[data-new-task]").click();
      await page
        .locator("#task-create")
        .filter({ hasText: "Creation status unknown" })
        .waitFor();
      assert.equal(
        await page.locator("#task-name").inputValue(),
        lostCreateName,
      );
      assert.equal(await page.locator("#task-create").isDisabled(), true);
      assert.equal(
        taskCreateRequests,
        lostCreateRequests + 1,
        "reload sent another project create",
      );
      await page.evaluate(() => qa.clearLaunchPlans());
      await page.locator("#dialog-close").click();

      // If our POST never committed, one unrelated project with identical
      // fields is still not an authoritative match and must never be adopted.
      await page.evaluate(() => qa.modes.set("teams"));
      await page.locator("[data-new-task]").click();
      await page.locator("#task-main-server").selectOption("secondary");
      await page.locator('[data-project-server="secondary"]').fill(root);
      const foreignName = name + " foreign exact fields";
      await page.locator("#task-name").fill(foreignName);
      const foreignRequests = taskCreateRequests;
      const foreignExecs = execs;
      await fetch(origin + "/qa/drop-task-create-before-commit");
      await page.locator("#task-create").click();
      await page
        .locator("#task-error")
        .filter({ hasText: "prior project-creation response is unknown" })
        .waitFor();
      persistedPlans = await page.evaluate(
        async () => (await qa.localData()).teamLaunchPlans,
      );
      const foreignPlan = persistedPlans.find(
        (plan) => plan.creation?.request?.name === foreignName,
      );
      assert.ok(foreignPlan);
      assert.equal(
        (await getTasks()).filter((task) => task.name === foreignName).length,
        0,
      );
      const foreignCreate = await fetch(`http://127.0.0.1:${port}/v1/tasks`, {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify(foreignPlan.creation.request),
      });
      assert.equal(foreignCreate.status, 201);
      await page.evaluate(() =>
        document
          .querySelector("#task-form")
          .dispatchEvent(
            new Event("submit", { bubbles: true, cancelable: true }),
          ),
      );
      await page
        .locator("#task-error")
        .filter({ hasText: "cannot safely identify" })
        .waitFor();
      assert.equal(taskCreateRequests, foreignRequests + 1);
      assert.equal(execs, foreignExecs);
      persistedPlans = await page.evaluate(
        async () => (await qa.localData()).teamLaunchPlans,
      );
      assert.equal(
        persistedPlans.find(
          (plan) => plan.creation?.request?.name === foreignName,
        ).taskId,
        undefined,
      );
      await page.evaluate(() => qa.clearLaunchPlans());
      await page.locator("#dialog-close").click();

      // Concurrent submits are single-flight from the first synchronous turn.
      // A known successful create can then reconcile a lost worker response by
      // exact preallocated agent/run identity and continue remaining members.
      await page.evaluate(() => qa.modes.set("teams"));
      await page.locator("[data-new-task]").click();
      await page.locator("#task-main-server").selectOption("secondary");
      await page.locator('[data-project-server="secondary"]').fill(root);
      const lostWorkerName = name + " lost worker";
      await page.locator("#task-name").fill(lostWorkerName);
      const lostWorkerRequests = taskCreateRequests;
      const lostWorkerStart = execs;
      await fetch(origin + "/qa/lose-launch-reply");
      await page.evaluate(() => {
        const form = document.querySelector("#task-form");
        form.requestSubmit();
        form.requestSubmit();
      });
      await page
        .locator("#task-error")
        .filter({ hasText: "Synthetic lost launch response" })
        .waitFor();
      assert.equal(
        taskCreateRequests,
        lostWorkerRequests + 1,
        "concurrent submit created more than one project",
      );
      const lostWorkerTask = (await getTasks()).find(
        (task) => task.name === lostWorkerName,
      );
      let lostWorkerDetail = await (
        await fetch(`http://127.0.0.1:${port}/v1/tasks/${lostWorkerTask.id}`)
      ).json();
      assert.equal(lostWorkerDetail.agents.length, 1);
      const originalLostAgent = lostWorkerDetail.agents[0];
      await page.locator("#task-create").click();
      await page.locator("#dialog").waitFor({ state: "hidden" });
      lostWorkerDetail = await (
        await fetch(`http://127.0.0.1:${port}/v1/tasks/${lostWorkerTask.id}`)
      ).json();
      assert.equal(lostWorkerDetail.agents.length, 3);
      assert.ok(
        lostWorkerDetail.agents.some(
          (agent) =>
            agent.id === originalLostAgent.id &&
            agent.runId === originalLostAgent.runId,
        ),
        "lost worker identity/run changed during reconciliation",
      );
      assert.equal(
        execs - lostWorkerStart,
        3,
        "reconciliation spawned the lost worker twice or skipped a remaining member",
      );
      assert.equal(
        (await getTasks()).filter((task) => task.name === lostWorkerName)
          .length,
        1,
        "concurrent submit duplicated a known project",
      );

      await page.evaluate(() => qa.modes.set("teams"));
      const launchStart = execs;
      failAt = execs + 2;
      await page.locator("[data-new-task]").click();
      assert.notEqual(await page.locator("#task-team").inputValue(), "");
      await page.locator("#task-main-server").selectOption("secondary");
      await page.locator('[data-project-server="secondary"]').fill(root);

      await page.locator("#task-allow-spawn").check();
      await page.locator("#task-max-new-agents").fill("3");
      await page.locator("#task-name").fill(name + " team task");
      await page.locator("#task-create").click();
      await page
        .locator("#task-error")
        .filter({ hasText: "Test launch failure" })
        .waitFor();
      await page
        .locator('[data-project-server="secondary"]')
        .fill(path.join(root, "hub"));
      await page.locator("#task-create").click();
      await page.locator("#dialog").waitFor({ state: "hidden" });
      assert.equal(
        execs - launchStart,
        4,
        "retry starts only the failed member after the mandatory handler launch",
      );
      const teamTask = (await getTasks()).find(
        (t) => t.name === name + " team task",
      );
      const detail = await (
        await fetch("http://127.0.0.1:" + port + "/v1/tasks/" + teamTask.id)
      ).json();
      assert.equal(detail.agents.length, 3);
      assert.equal(detail.task.maxNewAgents, 3);
      assert.deepEqual(
        detail.agents.map((a) => a.cwd),
        [root, root, path.join(root, "hub")],
      );
      assert.equal(
        detail.agents.filter((a) => a.role === "database_handler").length,
        1,
        "project gets one visible database handler",
      );
      assert.equal(detail.task.swarm, true);
      assert.equal(detail.task.orchestrator, "team-planner");
      assert.match(launchCommands.at(-3), /--approval-mode/);
      assert.match(launchCommands.at(-3), /--sandbox-mode/);
      assert.doesNotMatch(
        launchCommands.at(-3),
        /--reasoning/,
        "synthetic custom wrappers must inherit reasoning",
      );
      const plannerLaunch = launchCommands
        .slice(launchStart)
        .find((command) => command.includes("team-planner"));
      assert.match(
        plannerLaunch,
        /--planned-team-members.*2/,
        "first team launch carries the complete ordinary-member count",
      );
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

      await page.evaluate((id) => qa.hub.settings(id), teamTask.id);
      await page.locator("#task-settings details > summary").click();
      const retirement = page.locator(
        `[data-agent-retirement="${blockedAgent.id}"]`,
      );
      await retirement.click();
      await page.waitForFunction(
        (id) =>
          document.querySelector(`[data-agent-retirement="${id}"]`)
            ?.textContent === "Resume",
        blockedAgent.id,
      );
      let retired = await (
        await fetch(
          `http://127.0.0.1:${port}/v1/tasks/${teamTask.id}/agents/${blockedAgent.id}`,
        )
      ).json();
      assert.equal(retired.status, "retired");
      assert.equal(retired.session, "blocked-fixture");
      await page.screenshot({ path: `.build/retirement-settings-${name}.png` });
      await retirement.click();
      await page.waitForFunction(
        (id) =>
          document.querySelector(`[data-agent-retirement="${id}"]`)
            ?.textContent === "Retire",
        blockedAgent.id,
      );
      retired = await (
        await fetch(
          `http://127.0.0.1:${port}/v1/tasks/${teamTask.id}/agents/${blockedAgent.id}`,
        )
      ).json();
      assert.equal(retired.status, "done");
      await page.locator("#dialog-close").click();

      assert.deepEqual(launchServers.slice(-3), [
        "secondary",
        "secondary",
        "secondary",
      ]);
      const closeResponse = await fetch(
        `http://127.0.0.1:${port}/v1/tasks/${teamTask.id}/agents/${blockedAgent.id}?runId=${blockedAgent.runId}`,
        { method: "DELETE" },
      );
      assert.equal(closeResponse.status, 200);
      let closedWorker = await closeResponse.json();
      assert.equal(closedWorker.status, "closed");
      assert.equal(closedWorker.cleanupDone, false);
      let openDetail = await (
        await fetch(`http://127.0.0.1:${port}/v1/tasks/${teamTask.id}`)
      ).json();
      assert.equal(openDetail.task.status, "open");
      await page.evaluate((id) => qa.hub.settings(id), teamTask.id);
      await page.locator("#task-settings details > summary").click();
      const cleanupRetry = page.locator(
        `[data-agent-cleanup="${blockedAgent.id}"]`,
      );
      await cleanupRetry.waitFor();
      await page.screenshot({
        path: `.build/agent-cleanup-pending-${name}.png`,
      });
      await cleanupRetry.click();
      await page.waitForFunction(
        (id) => !document.querySelector(`[data-agent-cleanup="${id}"]`),
        blockedAgent.id,
      );
      closedWorker = await (
        await fetch(
          `http://127.0.0.1:${port}/v1/tasks/${teamTask.id}/agents/${blockedAgent.id}`,
        )
      ).json();
      assert.equal(closedWorker.cleanupDone, true);
      openDetail = await (
        await fetch(`http://127.0.0.1:${port}/v1/tasks/${teamTask.id}`)
      ).json();
      assert.equal(openDetail.task.status, "open");
      await page.locator("#dialog-close").click();
      await page.evaluate(() => qa.modes.set("teams"));
      await page.locator("[data-add-team]").click();
      const target = (await getTasks()).find(
        (t) => t.name === name + " mobile task",
      );
      const targetBefore = await (
        await fetch("http://127.0.0.1:" + port + "/v1/tasks/" + target.id)
      ).json();
      const targetLead = targetBefore.agents.find((agent) => !agent.role);
      const routedItem = await (
        await fetch(
          `http://127.0.0.1:${port}/v1/tasks/${target.id}/work-items`,
          {
            method: "POST",
            headers: { "content-type": "application/json" },
            body: JSON.stringify({
              kind: "bug",
              title: "Synthetic routed browser item " + name,
              description: "Restore this item and no other browser history.",
              agentId: targetLead.id,
              requestId: "browser-route-item-" + name,
            }),
          },
        )
      ).json();
      const routedOrder = await (
        await fetch(`http://127.0.0.1:${port}/v1/tasks/${target.id}/messages`, {
          method: "POST",
          headers: { "content-type": "application/json" },
          body: JSON.stringify({
            text: "Synthetic bounded browser work order",
            agentId: targetLead.id,
            requestId: "browser-route-order-" + name,
            workItems: [
              {
                itemTaskId: target.id,
                itemId: routedItem.id,
                itemRevision: routedItem.revision,
                relationship: "primary",
              },
            ],
          }),
        })
      ).json();
      const unrelatedRouteMessage = await (
        await fetch(`http://127.0.0.1:${port}/v1/tasks/${target.id}/messages`, {
          method: "POST",
          headers: { "content-type": "application/json" },
          body: JSON.stringify({ text: "UNRELATED BROWSER ROUTING HISTORY" }),
        })
      ).json();
      await page.locator("#team-task").selectOption(target.id);
      await page
        .locator("#team-work-item")
        .selectOption({ value: routedItem.id });
      await page.locator("#team-work-order").fill(String(routedOrder.seq));
      await page.locator("#team-main-server").selectOption("local");
      await page.locator('[data-project-server="local"]').fill(root);
      const badRoutedFolder = "/missing-tailterm-item-routing-folder";
      const correctedRoutedFolder = path.join(root, "hub");
      await page
        .locator('[data-project-server="secondary"]')
        .fill(badRoutedFolder);
      const routedLaunchStart = execs;
      const historyBefore = await (
        await fetch(origin + "/qa/history-requests")
      ).json();
      await page.locator('#team-launch-form button[type="submit"]').click();
      await page
        .locator("#team-launch-status")
        .filter({ hasText: "is not a directory" })
        .waitFor();
      assert.equal(await page.locator("#team-task").isDisabled(), true);
      assert.equal(await page.locator("#team-work-item").isDisabled(), true);
      assert.equal(await page.locator("#team-work-order").isDisabled(), true);
      assert.equal(
        await page.locator('[data-project-server="local"]').isDisabled(),
        true,
        "completed member's folder remained editable",
      );
      assert.equal(
        await page.locator('[data-project-server="secondary"]').isDisabled(),
        false,
        "failed member's folder could not be corrected",
      );
      const historyAfterFailure = await (
        await fetch(origin + "/qa/history-requests")
      ).json();
      const reconciliationChecks = await page.evaluate(async (taskId) => {
        const journal = (await qa.localData()).teamLaunchPlans.find(
          (plan) => plan.kind === "add-team" && plan.taskId === taskId,
        );
        const member = journal.members.find(
          (candidate) => candidate.state === "started",
        );
        const detail = await qa.hub.client().getTask(taskId);
        const agent = detail.agents.find(
          (candidate) => candidate.id === member.fields.agentId,
        );
        const entry = {
          fields: {
            ...member.fields,
            workContextBundle: journal.workContextBundle,
          },
        };
        return {
          exact: await qa.reconciledAgentProblem(taskId, entry, member, agent),
          run: await qa.reconciledAgentProblem(
            taskId,
            entry,
            {
              ...member,
              agent: { ...member.agent, runId: "run_ffffffffffffffff" },
            },
            agent,
          ),
          lifecycle: await qa.reconciledAgentProblem(taskId, entry, member, {
            ...agent,
            status: "closed",
          }),
          binding: await qa.reconciledAgentProblem(taskId, entry, member, {
            ...agent,
            workItem: {
              ...agent.workItem,
              itemRevision: agent.workItem.itemRevision + 1,
            },
          }),
        };
      }, target.id);
      assert.deepEqual(reconciliationChecks, {
        exact: "",
        run: "agent run",
        lifecycle: "lifecycle",
        binding: "work-item/order/context binding",
      });
      await page
        .locator('[data-project-server="secondary"]')
        .fill(correctedRoutedFolder);
      await page.locator('#team-launch-form button[type="submit"]').click();
      try {
        await page.locator("#dialog").waitFor({ state: "hidden" });
      } catch (error) {
        throw new Error(
          `${error.message}\n${await page.locator("#team-launch-status").innerText()}`,
        );
      }
      const historyAfterRetry = await (
        await fetch(origin + "/qa/history-requests")
      ).json();
      assert.equal(
        historyAfterFailure.historyRequests - historyBefore.historyRequests,
        4,
      );
      assert.equal(
        historyAfterRetry.historyRequests,
        historyAfterFailure.historyRequests,
        "partial retry must not rebuild the immutable history bundle",
      );
      assert.equal(
        execs - routedLaunchStart,
        3,
        "item-scoped retry starts only the failed team member",
      );
      assert.equal(
        launchCommands
          .at(-2)
          .replaceAll(badRoutedFolder, correctedRoutedFolder),
        launchCommands.at(-1),
        "item-scoped retry changed fields other than the failed member's folder",
      );
      const attached = await (
        await fetch("http://127.0.0.1:" + port + "/v1/tasks/" + target.id)
      ).json();
      assert.equal(
        attached.agents.length,
        4,
        "existing project keeps its orchestrator and handler when a team joins",
      );
      const routedAgents = attached.agents.filter(
        (agent) => agent.workItem?.itemId === routedItem.id,
      );
      assert.equal(routedAgents.length, 2);
      assert.deepEqual(
        routedAgents.map((agent) => agent.cwd).sort(),
        [root, correctedRoutedFolder].sort(),
      );
      assert.ok(
        routedAgents.every(
          (agent) =>
            !agent.parentAgentId &&
            agent.readUpTo === unrelatedRouteMessage.seq &&
            agent.name.endsWith("-" + routedItem.id.slice(-8)),
        ),
        JSON.stringify(routedAgents),
      );
      const restoredContext = await (
        await fetch(
          `http://127.0.0.1:${port}/v1/tasks/${target.id}/agents/${routedAgents[0].id}/work-context?runId=${routedAgents[0].runId}`,
        )
      ).json();
      assert.deepEqual(
        restoredContext.bundle.history.messages.map(
          (link) => link.message.text,
        ),
        ["Synthetic bounded browser work order"],
      );
      assert.ok(
        launchCommands
          .slice(-3)
          .every(
            (command) =>
              command.includes("--work-item") &&
              command.includes(routedItem.id) &&
              command.includes("--work-order-message"),
          ),
      );
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
      await page.setViewportSize({ width: 1200, height: 800 });
      await page.evaluate(() => qa.modes.set("tasks"));
      await page.locator(`[data-task-select="${teamTask.id}"]`).click();
      await page.locator(`[data-task-more="${teamTask.id}"]`).click();
      // Seed only this temporary database, bypassing write-rate limits for a
      // long-history fixture. Production task/message data is never touched.
      await exec("python3", [
        "-c",
        `import sqlite3,sys,datetime
s=sqlite3.connect(sys.argv[1])
s.executemany("insert into messages(task_id,from_node,from_user,text,created_at) values(?,?,?,?,?)", [(sys.argv[2],"fixture","owner",f"Archived message {i}",datetime.datetime.now(datetime.timezone.utc).isoformat()) for i in range(1,206)])
s.commit()
`,
        path.join(state, "hub.sqlite"),
        teamTask.id,
      ]);
      failLaunch = true;
      await page.locator(`[data-task-close="${teamTask.id}"]`).click();
      await page.waitForFunction(() =>
        document
          .querySelector("#notice")
          .textContent.includes("pending cleanup"),
      );
      await page.locator(".tasks-closed summary").click();
      await page.locator(`[data-task-cleanup="${teamTask.id}"]`).waitFor();
      await page.screenshot({
        path: `.build/task-cleanup-pending-${name}.png`,
      });
      await page.locator(`[data-task-cleanup="${teamTask.id}"]`).click();
      await page.waitForFunction(() =>
        document
          .querySelector("#notice")
          .textContent.includes("agent sessions stopped"),
      );
      const closedDetail = await (
        await fetch(`http://127.0.0.1:${port}/v1/tasks/${teamTask.id}`)
      ).json();
      assert.equal(closedDetail.task.status, "closed");
      assert.equal(closedDetail.task.cleanupPending, 0);
      assert.ok(
        closedDetail.agents.every(
          (a) => a.status === "closed" && a.cleanupDone,
        ),
      );
      for (const agent of closedDetail.agents) {
        const exists = await exec("tmux", [
          "-L",
          "tailterm-form-check",
          "has-session",
          "-t",
          "=" + agent.session,
        ]).then(
          () => true,
          () => false,
        );
        assert.equal(exists, false, "closed agent session survived");
      }
      await page.locator(".tasks-closed summary").click();
      await page.screenshot({ path: `.build/task-cleanup-${name}.png` });
      await page.locator(`[data-task-board="${teamTask.id}"]`).click();
      await page.locator("#board-download").waitFor();
      assert.equal(
        await page
          .locator(
            "#board-compose, #board-attach, #board-add-agent, [data-reply]",
          )
          .count(),
        0,
      );
      assert.equal(await page.locator(".board-message").count(), 200);
      await page.locator("#board-full-history").click();
      await page.waitForFunction(
        () => document.querySelectorAll(".board-message").length === 205,
      );
      const downloadReady = page.waitForEvent("download");
      await page.locator("#board-download").click();
      const download = await downloadReady;
      const history = JSON.parse(await readFile(await download.path(), "utf8"));
      assert.equal(history.task.id, teamTask.id);
      assert.equal(history.messages.length, 205);
      assert.equal(history.agents.length, closedDetail.agents.length);
      assert.ok(history.events.some((e) => e.kind === "task_closed"));
      assert.equal(history.messages[0].text, "Archived message 1");
      await page.screenshot({ path: `.build/task-history-${name}.png` });

      const delayedScopeCheck = async (suffix, mutate, expected) => {
        await page.evaluate(() => qa.hub.newTask(undefined, qa.data.teams[0]));
        await page.locator("#task-main-server").selectOption("secondary");
        await page.locator('[data-project-server="secondary"]').fill(root);
        await page.locator("#task-name").fill(`${name} ${suffix}`);
        await page.evaluate(() => qa.delayNextCommand());
        await page.locator("#task-create").click();
        await page.evaluate(() => qa.waitForDelayedCommand());
        await page.evaluate(mutate);
        await page.evaluate(() => qa.releaseCommand());
        await page
          .locator("#task-error")
          .filter({ hasText: expected })
          .waitFor();
        await page.evaluate(() => qa.clearLaunchPlans());
      };
      await delayedScopeCheck(
        "delayed token scope",
        () => qa.changeToken("replacement-token"),
        "hub credential or profile changed",
      );
      await page.evaluate(() => {
        qa.changeToken("synthetic-token");
        qa.hub.refresh();
      });
      await delayedScopeCheck(
        "delayed profile scope",
        () => qa.cycleProfile(),
        "launch view changed",
      );
      await delayedScopeCheck(
        "delayed endpoint scope",
        () => qa.changeServerCredentials(),
        "saved machine profile",
      );
      assert.deepEqual(errors, []);
      console.log(
        name +
          ": creation single-flight across task-list/journal delays, encrypted journal failure/reload, unknown-create and foreign-match blocking, lost-worker reconciliation, delayed token/profile/endpoint guards, exact identity/mismatch checks, remaining-member retry, mobile and mode selection passed.",
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
