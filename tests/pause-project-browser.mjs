// Synthetic browser proof for Feature wi_87f38179eb1b5bd9 revision 2,
// bounded corrected work order #6450. No live hub, task, profile or tmux socket
// is used: the real Projects view/controller talk to an in-page deterministic
// transport and a fake exact-run cleanup consumer.
import { chromium, webkit } from "@playwright/test";
import assert from "node:assert/strict";
import { createServer } from "vite";
import { mkdirSync } from "node:fs";
import path from "node:path";

const artifactDir = path.resolve(".build/pause-project-ui-6450");
mkdirSync(artifactDir, { recursive: true });

const fixture = `
import '/client/style.css';
import {createTaskHub} from '/client/task-hub.js';
import {createTasksView} from '/client/tasks-view.js';

const clone=value=>structuredClone(value);
const taskA={id:'tsk_aaaaaaaaaaaaaaaa',name:'Synthetic retained project',goal:'Preserve unfinished Feature and result evidence',status:'open',pauseState:'active',lifecycleGeneration:0,pauseGeneration:0,pauseCleanupPending:0,orchestrator:'lead',allowAgentSpawn:true};
const taskB={id:'tsk_bbbbbbbbbbbbbbbb',name:'Unrelated synthetic project',goal:'Must remain untouched',status:'open',pauseState:'active',lifecycleGeneration:0,pauseGeneration:0,pauseCleanupPending:0,orchestrator:'other',allowAgentSpawn:false};
const agents=[
 {id:'agt_1111111111111111',runId:'run_1111111111111111',taskId:taskA.id,name:'lead',host:'synthetic-host',session:'lead',runtime:'codex',cwd:'/synthetic/a',status:'running',online:true,cleanupDone:false},
 {id:'agt_2222222222222222',runId:'run_2222222222222222',taskId:taskA.id,name:'db-handler',role:'database_handler',host:'synthetic-host',session:'handler',runtime:'codex',cwd:'/synthetic/a',status:'running',online:true,cleanupDone:false},
 {id:'agt_3333333333333333',runId:'run_3333333333333333',taskId:taskA.id,name:'worker',host:'synthetic-host',session:'worker',runtime:'codex',cwd:'/synthetic/a',status:'exited',online:false,cleanupDone:false},
 {id:'agt_4444444444444444',runId:'run_4444444444444444',taskId:taskB.id,name:'other',host:'synthetic-host',session:'other',runtime:'codex',cwd:'/synthetic/b',status:'running',online:true,cleanupDone:false},
];
let capability={projectPause:{supported:true,versions:[1]}};
let pause=null;
let cleanupRound=0;
let lostResumeResponse=true;
let failedLeadConfirmation=false;
let failedWorkerLaunch=false;
let resumeRequest=null;
const requests=[];
const commands=[];
const json=(status,value)=>({status,statusText:'',text:async()=>JSON.stringify(value)});
const detail=task=>({task:clone(task),agents:clone(agents.filter(agent=>agent.taskId===task.id)),latestSeq:1});
const network={async fetch(raw,init={}){
 const url=new URL(raw),method=init.method||'GET',body=init.body?JSON.parse(init.body):{};
 requests.push({method,path:url.pathname,body:clone(body)});
 if(url.pathname==='/v1/capabilities') return json(200,clone(capability));
 if(url.pathname==='/v1/tasks') return json(200,{tasks:[clone(taskB),clone(taskA)]});
 if(url.pathname==='/v1/tasks/'+taskA.id) return json(200,detail(taskA));
 if(url.pathname==='/v1/tasks/'+taskB.id) return json(200,detail(taskB));
 if(url.pathname==='/v1/tasks/'+taskA.id+'/pause'&&method==='GET') return json(200,clone(pause));
 if(url.pathname==='/v1/tasks/'+taskA.id+'/pause'&&method==='POST'){
   if(pause&&pause.receipt?.requestId===body.requestId) return json(200,{...clone(pause),replay:true});
   taskA.lifecycleGeneration++;taskA.pauseGeneration++;taskA.pauseState='cleanup_pending';taskA.pauseCleanupPending=body.targets.length;
   for(const agent of agents.filter(agent=>agent.taskId===taskA.id)) agent.status='closed';
   pause={version:1,replay:false,taskId:taskA.id,state:'cleanup_pending',pauseGeneration:taskA.pauseGeneration,lifecycleGeneration:taskA.lifecycleGeneration,cleanupPending:body.targets.length,handoffPending:0,retainedHandoffDigest:'a'.repeat(64),targets:body.targets.map(target=>({...target,name:agents.find(agent=>agent.id===target.agentId).name,serviceDisposition:'none',serviceVerified:false,cleanupDone:false})),previousTeam:{teamId:body.previousTeamId||'',orchestratorName:'lead'},receipt:{id:'ppr_1111111111111111',operation:'pause',requestId:body.requestId,taskId:taskA.id,pauseGeneration:taskA.pauseGeneration}};
   return json(200,clone(pause));
 }
 if(url.pathname==='/v1/tasks/'+taskA.id+'/resume'&&method==='POST'){
   if(!resumeRequest){
     resumeRequest=clone(body);
     taskA.lifecycleGeneration++;
     taskA.pauseState='resuming';
     pause={...pause,state:'resuming',lifecycleGeneration:taskA.lifecycleGeneration,resumeAdmission:{receiptId:'ppr_5555555555555555',selectedTeamId:body.selectedTeamId,orchestrator:clone(body.orchestrator),pending:true},receipt:{id:'ppr_5555555555555555',operation:'resume',requestId:body.requestId,taskId:taskA.id,pauseGeneration:taskA.pauseGeneration}};
     if(lostResumeResponse){lostResumeResponse=false;throw Error('Synthetic resume response lost after commit')}
   }else if(JSON.stringify(resumeRequest)!==JSON.stringify(body)) return json(409,{error:'Resume retry changed'});
   return json(200,{...clone(pause),replay:true});
 }
 return json(404,{error:'Synthetic route not found'});
}};
const data={hub:{url:'http://synthetic.invalid',token:''},teamLaunchPlans:[],agentCatalog:null,teams:[{id:'team_previous',name:'Previous synthetic team',orchestrator:'lead',swarm:false,members:[{name:'lead',role:'Lead',serverId:'synthetic-server',runtime:'codex',model:'',permissionMode:'',allowedTools:[],run:'codex',cwd:'/synthetic/a',prompt:''},{name:'worker',role:'Builder',serverId:'synthetic-server',runtime:'codex',model:'',permissionMode:'',allowedTools:[],run:'codex',cwd:'/synthetic/a',prompt:''}]}]};
const dialog=document.querySelector('#dialog');
const notice=document.querySelector('#notice');
const host={
 getIPN:()=>network,getData:()=>data,getTabs:()=>[],getServers:()=>[{id:'synthetic-server',name:'Synthetic host',host:'synthetic-host',username:'tester',port:22,mode:'ssh',credentialRevision:1}],
 currentTab:()=>null,currentServer:()=>null,paneGroups:()=>({model:{groups:[]},sync(){}}),scheduleWorkspaceSave(){},render(){},bookmark(){},closeTab(){},connect:async()=>null,activate(){},openBoard(){},
 dialog(title,body){dialog.innerHTML='<div class="dialog-head"><h2>'+title+'</h2></div>'+body;dialog.showModal()},closeDialog(){dialog.close()},
 notice(text){notice.textContent=text;notice.hidden=false},
 async api(route,method='GET',body){
   if(route==='/team-launch-plans/validate') return clone(body);
   if(route==='/team-launch-plans'&&method==='POST'){
     const index=data.teamLaunchPlans.findIndex(plan=>plan.id===body.id);
     if(index===-1)data.teamLaunchPlans.push(clone(body));else data.teamLaunchPlans[index]=clone(body);
     return clone(body);
   }
   if(route.startsWith('/team-launch-plans/')&&method==='DELETE'){
     data.teamLaunchPlans=data.teamLaunchPlans.filter(plan=>plan.id!==route.split('/').at(-1));return {};
   }
   return {};
 },
 reloadData:async()=>{},
 launchServerProfile:id=>host.getServers().find(server=>server.id===id),
 async browserCommand(_server,command){
   commands.push(command);
   if(command.includes("'spawn'")){
     const journal=data.teamLaunchPlans.find(plan=>plan.kind==='resume-project');
     const member=journal?.members.find(candidate=>candidate.state==='uncertain');
     if(!member) throw Error('Synthetic spawn has no uncertain frozen member');
     const fields=member.fields;
     if(fields.name==='worker'&&!failedWorkerLaunch){failedWorkerLaunch=true;throw Object.assign(Error('Synthetic worker launch failed before admission'),{verifiedUnstarted:true})}
     let fresh=agents.find(agent=>agent.id===fields.agentId);
     if(!fresh){
       fresh={id:fields.agentId,runId:fields.expectedRunId||('run_'+String(agents.length+1).repeat(16).slice(0,16)),taskId:taskA.id,name:fields.name,host:'synthetic-host',session:'resume-'+fields.name,runtime:fields.runtime,cwd:fields.cwd,parentAgentId:'',role:fields.agentRole||'',status:'running',online:true,cleanupDone:false};
       agents.push(fresh);
     }
     if(fields.agentId===journal.resume.orchestratorAgentId){
       if(fields.expectedRunId!==journal.resume.orchestratorRunId||fields.resumeReceiptId!=='ppr_5555555555555555')throw Error('Fresh orchestrator admission binding changed');
       if(!failedLeadConfirmation){failedLeadConfirmation=true;throw Error('Synthetic lead registered before tmux confirmation')}
       taskA.pauseState='active';taskA.orchestrator=fields.name;
       pause={...pause,state:'active',resumeAdmission:{...pause.resumeAdmission,pending:false}};
     }
     return JSON.stringify(fresh);
   }
   if(!/\\bcleanup\\b/.test(command)) throw Error('Unexpected synthetic host command');
   cleanupRound++;
   const target=agents.filter(agent=>agent.taskId===taskA.id);
   for(const agent of target.slice(0,cleanupRound===1?2:target.length)) agent.cleanupDone=true;
   pause.targets=pause.targets.map(saved=>({...saved,cleanupDone:agents.find(agent=>agent.id===saved.agentId).cleanupDone}));
   taskA.pauseCleanupPending=pause.targets.filter(target=>!target.cleanupDone).length;
   pause.cleanupPending=taskA.pauseCleanupPending;
   if(!taskA.pauseCleanupPending){taskA.pauseState='paused';pause.state='paused'}
   return JSON.stringify({confirmed:target.filter(agent=>agent.cleanupDone).length,errors:[]});
 }
};
const hub=createTaskHub(host);hub.refresh();
const view=createTasksView({client:()=>hub.viewClient(),taskHub:hub,getTabs:()=>[],activate(){},notice:text=>host.notice(text),confirm:async()=>true,openBoard(){},openWorkItems(){},configure(){}});
view.mount(document.querySelector('#mode-view'));await view.show();
window.fixture={hub,view,taskA,taskB,agents,requests,commands,data,get pause(){return pause},setCapability(value){capability=value},async render(){await view.reload()},setUnknown(){taskA.pauseState=undefined}};
`;

const html = `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Pause project synthetic fixture</title></head><body data-mode="tasks"><div id="app"><div id="workspace"><aside><div class="brand"><strong>tailterm</strong></div></aside><main><header></header><section id="mode-view"></section></main></div><dialog id="dialog"></dialog><p id="notice" role="status" hidden></p></div><script type="module">${fixture}</script></body></html>`;

const server = await createServer({
  configFile: false,
  root: process.cwd(),
  server: { host: "127.0.0.1", port: 0 },
  plugins: [
    {
      name: "pause-project-fixture",
      configureServer(vite) {
        vite.middlewares.use("/pause-project", async (request, response) => {
          try {
            const transformed = await vite.transformIndexHtml(
              request.url,
              html,
            );
            response.setHeader("Content-Type", "text/html");
            response.end(transformed);
          } catch (error) {
            response.statusCode = 500;
            response.end(error.stack || error.message);
          }
        });
      },
    },
  ],
});
await server.listen();
const origin = `http://127.0.0.1:${server.httpServer.address().port}`;

try {
  for (const engine of [chromium, webkit]) {
    const browser = await engine.launch();
    try {
      const page = await browser.newPage({
        viewport: { width: 1100, height: 760 },
      });
      const errors = [];
      page.on("pageerror", (error) => errors.push(error.message));
      await page.goto(origin + "/pause-project");
      await page.waitForTimeout(250);
      assert.deepEqual(
        errors,
        [],
        `fixture failed before render: ${await page.locator("body").innerText()}`,
      );
      assert.equal(
        await page.evaluate(() => !!window.fixture),
        true,
        `fixture module did not finish: ${await page.locator("body").innerText()}`,
      );
      assert.match(
        await page.locator("#mode-view").innerHTML(),
        /Synthetic retained project/,
      );
      const heading = page.locator("h2", {
        hasText: "Synthetic retained project",
      });
      assert.equal(
        await heading.count(),
        1,
        await page.locator("#mode-view").innerHTML(),
      );
      assert.equal(
        await heading.isVisible(),
        true,
        JSON.stringify(
          await page.evaluate(() => {
            const node = document.querySelector(".tasks-view h2");
            const chain = [];
            for (let current = node; current; current = current.parentElement) {
              const style = getComputedStyle(current);
              chain.push({
                tag: current.tagName,
                id: current.id,
                className: current.className,
                display: style.display,
                visibility: style.visibility,
                width: current.getBoundingClientRect().width,
                height: current.getBoundingClientRect().height,
              });
            }
            return chain;
          }),
        ),
      );

      const more = page.getByRole("button", { name: "More" });
      await more.focus();
      await page.keyboard.press("Enter");
      const pauseAction = page.getByTestId("pause-project");
      await pauseAction.waitFor();
      assert.equal(await pauseAction.isEnabled(), true);
      await pauseAction.click();
      await page.getByTestId("pause-project-confirmation").waitFor();
      assert.match(
        await page.locator("#project-pause-form").innerText(),
        /unfinished work and evidence/i,
      );
      assert.match(
        await page.locator("#project-pause-form").innerText(),
        /database handler/i,
      );
      assert.equal(
        await page.locator(".pause-target").count(),
        3,
        "exited unsettled run was omitted",
      );
      for (const select of await page.locator("[data-pause-disposition]").all())
        await select.selectOption("none");
      await page.getByTestId("pause-project-confirm").focus();
      await page.keyboard.press("Enter");
      await page.getByTestId("project-pause-cleanup-pending").waitFor();
      assert.equal(await page.getByTestId("resume-project").count(), 0);
      assert.match(
        await page.getByTestId("project-pause-cleanup-pending").innerText(),
        /Resume stays blocked/,
      );
      assert.equal(
        await page.evaluate(() => window.fixture.taskB.pauseState),
        "active",
        "unrelated project changed",
      );

      await page.getByTestId("pause-cleanup-pending").click();
      await page.waitForTimeout(100);
      assert.equal(
        await page.evaluate(() => window.fixture.taskA.pauseState),
        "paused",
        await page.locator("#notice").innerText(),
      );
      await page.getByTestId("project-paused").waitFor();
      assert.match(
        await page.getByTestId("project-paused").innerText(),
        /unfinished work and evidence are preserved/,
      );
      assert.equal(await page.getByTestId("resume-project").count(), 1);
      await page.evaluate(() => {
        document.querySelector("#notice").hidden = true;
      });

      await page.setViewportSize({ width: 320, height: 720 });
      assert.equal(
        await page.evaluate(
          () => document.documentElement.scrollWidth <= innerWidth,
        ),
        true,
        "narrow paused layout overflowed",
      );
      await page.screenshot({
        path: path.join(artifactDir, `${engine.name()}-paused-narrow.png`),
        fullPage: true,
      });
      await page.setViewportSize({ width: 550, height: 380 });
      await page.screenshot({
        path: path.join(
          artifactDir,
          `${engine.name()}-paused-zoom-equivalent.png`,
        ),
        fullPage: true,
      });

      await page.setViewportSize({ width: 900, height: 720 });
      await page.getByTestId("resume-project").click();
      await page.getByTestId("resume-project-form").waitFor();
      assert.equal(
        await page.getByTestId("resume-team").inputValue(),
        "team_previous",
        "previous team was not proposed as the default",
      );
      assert.match(
        await page
          .getByTestId("resume-team")
          .locator("option:checked")
          .innerText(),
        /suggested/i,
      );
      await page.setViewportSize({ width: 320, height: 720 });
      assert.equal(
        await page.evaluate(() => {
          const dialog = document.querySelector("#dialog");
          return dialog.scrollWidth <= dialog.clientWidth;
        }),
        true,
        "narrow Resume chooser overflowed its dialog",
      );
      await page.screenshot({
        path: path.join(
          artifactDir,
          `${engine.name()}-resume-chooser-narrow.png`,
        ),
        fullPage: true,
      });
      await page.setViewportSize({ width: 900, height: 720 });
      await page.getByTestId("resume-project-confirm").focus();
      await page.keyboard.press("Enter");
      await page.waitForFunction(() =>
        document
          .querySelector("#project-resume-status")
          ?.textContent.includes("response lost"),
      );
      assert.equal(
        await page.evaluate(() => window.fixture.taskA.pauseState),
        "resuming",
        "lost response did not leave the persisted admission barrier",
      );
      assert.equal(
        await page.evaluate(
          () => window.fixture.data.teamLaunchPlans[0]?.resume.state,
        ),
        "uncertain",
      );

      await page.getByTestId("resume-project-confirm").click();
      await page.waitForFunction(() =>
        document
          .querySelector("#project-resume-status")
          ?.textContent.includes("registered before tmux confirmation"),
      );
      assert.equal(
        await page.evaluate(() => window.fixture.taskA.pauseState),
        "resuming",
        "agent registration cleared the barrier before tmux confirmation",
      );
      assert.equal(
        await page.evaluate(
          () =>
            window.fixture.agents.filter(
              (agent) =>
                agent.taskId === window.fixture.taskA.id &&
                agent.status === "running",
            ).length,
        ),
        1,
        "the synthetic unconfirmed lead registration was not retained exactly",
      );

      await page.getByTestId("resume-project-confirm").click();
      await page.waitForFunction(() =>
        document
          .querySelector("#project-resume-status")
          ?.textContent.includes("worker launch failed"),
      );
      assert.equal(
        await page.evaluate(() => window.fixture.taskA.pauseState),
        "active",
        "fresh exact orchestrator admission did not open the project",
      );
      assert.equal(
        await page.evaluate(
          () =>
            window.fixture.agents.filter(
              (agent) =>
                agent.taskId === window.fixture.taskA.id &&
                agent.status === "running",
            ).length,
        ),
        2,
        "partial launch did not preserve only the admitted lead and handler",
      );
      const resumeRequests = await page.evaluate(() =>
        window.fixture.requests.filter((request) =>
          request.path.endsWith("/resume"),
        ),
      );
      assert.equal(resumeRequests.length, 2);
      assert.deepEqual(
        resumeRequests[0].body,
        resumeRequests[1].body,
        "lost-response retry changed the frozen resume request",
      );
      const spawnCommands = await page.evaluate(() =>
        window.fixture.commands.filter((command) =>
          command.includes("'spawn'"),
        ),
      );
      assert.match(spawnCommands[0], /--resume-receipt-id/);
      assert.match(spawnCommands[0], /--expected-lifecycle-generation/);
      assert.equal(
        spawnCommands[0],
        spawnCommands[1],
        "unconfirmed lead retry changed the exact frozen host command",
      );
      assert.doesNotMatch(spawnCommands[2], /--resume-receipt-id/);

      await page.getByTestId("resume-project-confirm").click();
      await page.waitForFunction(
        () => window.fixture.data.teamLaunchPlans.length === 0,
      );
      await page.waitForFunction(
        () =>
          window.fixture.agents.filter(
            (agent) =>
              agent.taskId === window.fixture.taskA.id &&
              agent.status === "running",
          ).length === 3,
      );
      const identities = await page.evaluate(() => ({
        old: window.fixture.agents.slice(0, 3).map((agent) => agent.id),
        fresh: window.fixture.agents
          .filter(
            (agent) =>
              agent.taskId === window.fixture.taskA.id &&
              agent.status === "running",
          )
          .map((agent) => agent.id),
      }));
      assert.equal(
        identities.fresh.some((id) => identities.old.includes(id)),
        false,
        "Resume resurrected a prior agent identity",
      );
      assert.equal(
        await page.evaluate(() => window.fixture.taskB.pauseState),
        "active",
        "Resume changed the unrelated project",
      );

      await page.evaluate(async () => {
        window.fixture.taskA.pauseState = "active";
        window.fixture.setCapability({});
        await window.fixture.render();
      });
      await page.getByRole("button", { name: "More" }).click();
      assert.equal(await page.getByTestId("pause-project").isDisabled(), true);
      assert.match(
        await page.getByTestId("pause-project-legacy").innerText(),
        /hub update required/i,
      );
      await page.keyboard.press("Escape");
      await page
        .getByRole("button", { name: "More" })
        .waitFor({ state: "visible" });
      await page.waitForFunction(
        () =>
          document
            .querySelector("[data-task-more]")
            ?.getAttribute("aria-expanded") === "false",
      );

      await page.evaluate(async () => {
        window.fixture.setCapability({
          projectPause: { supported: true, versions: [1] },
        });
        window.fixture.setUnknown();
        await window.fixture.render();
      });
      await page.waitForTimeout(100);
      assert.equal(
        await page.getByTestId("project-pause-unknown").count(),
        1,
        await page.locator("#mode-view").innerHTML(),
      );
      assert.equal(await page.getByTestId("pause-project").count(), 0);
      assert.deepEqual(errors, []);
      console.log(
        `${engine.name()}: Pause, pending cleanup, lost-response Resume, fresh partial-launch retry, legacy/unknown, keyboard and narrow layouts passed.`,
      );
    } finally {
      await browser.close();
    }
  }
} finally {
  await server.close();
}
