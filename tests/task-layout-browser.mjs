// Real pane renderer with disposable in-memory tabs; no SSH, hub or user vault.
import assert from "node:assert/strict";
import { mkdir } from "node:fs/promises";
import { chromium, webkit } from "@playwright/test";
import { createServer } from "vite";
const html = `<!doctype html><html><head><link rel="stylesheet" href="/client/style.css"></head><body><div id="app"><section class="terminal-shell" style="margin:20px"><div class="terminal-tabs"><div id="tabs"></div></div><div id="terminal-body" style="height:700px;position:relative"></div></section></div><script type="module">
import {setupPaneGroups} from '/client/pane-groups.js';
const tabs=[]; let active='lead';
const groups=setupPaneGroups({getTabs:()=>tabs,getActive:()=>active,activate:id=>{active=id;groups.render()},close(){},preferences:()=>({}),label:t=>t.id});
window.fixture={groups,add(id){const el=document.createElement('div');el.className='terminal-instance';document.querySelector('#terminal-body').append(el);tabs.push({id,el,task:{taskId:'task',agentId:id},server:{host:'fixture',username:'test'},status:'Connected',tmux:true,session:id});groups.model.setTaskOrchestrator('task','lead');groups.sync();groups.render();},sync(){groups.sync();groups.render();},tree(){return structuredClone(groups.model.taskGroup('task').tree)}};
fixture.add('lead');
</script></body></html>`;
// Continued groups: a tab strip from entries(), a real dialog and 10-20 panes.
const continuedHtml = `<!doctype html><html><head><link rel="stylesheet" href="/client/style.css"></head><body><div id="app"><section class="terminal-shell" style="margin:20px"><div class="terminal-tabs"><div id="tabs" style="white-space:nowrap"></div></div><div id="terminal-body" style="height:700px;position:relative"></div></section><dialog id="dialog"></dialog></div><script type="module">
import {setupPaneGroups} from '/client/pane-groups.js';
import {leaves} from '/client/pane-layout.js';
const tabs=[],prefs={paneGroupLimit:8},params=new URLSearchParams(location.search);let active=null;
const dialogEl=document.querySelector('#dialog');
function strip(){document.querySelector('#tabs').innerHTML=groups.entries().map(({tab,group})=>'<div class="tab" style="display:inline-block;width:150px"><button data-tab="'+tab.id+'">'+groups.partName(group)+'</button></div>').join('')}
function activate(id){active=id;groups.render();strip()}
const groups=setupPaneGroups({getTabs:()=>tabs,getActive:()=>active,activate,close(){},dialog(title,body){dialogEl.innerHTML='<h2>'+title+'</h2>'+body;if(!dialogEl.open)dialogEl.showModal()},closeDialog(){dialogEl.close()},preferences:()=>prefs,label:t=>t.id,groupName:g=>g.taskId?(g.taskId==='task'?'Project':'Other'):leaves(g.tree).join(' + '),changed:strip});
function add(id,taskId='task'){const el=document.createElement('div');el.className='terminal-instance';document.querySelector('#terminal-body').append(el);tabs.push({id,el,task:taskId?{taskId,agentId:id}:undefined,server:{host:'fixture',username:'test'},status:'Connected',tmux:true,session:id});groups.model.setTaskOrchestrator('task',params.get('lead')||'p1');groups.sync();active=active||id;activate(active)}
function close(id){const tab=tabs.find(t=>t.id===id);tabs.splice(tabs.indexOf(tab),1);tab.el.remove();groups.sync();if(active===id)active=tabs[0]?.id;activate(active)}
const series=g=>groups.model.series(g);
window.fixture={groups,prefs,add,close,activate,
  parts:(id='p1')=>series(groups.model.group(id)).map(g=>leaves(g.tree)),
  names:()=>[...document.querySelectorAll('#tabs .tab')].map(t=>t.textContent),
  where:()=>Object.fromEntries(tabs.map(t=>[t.id,groups.model.groups.indexOf(groups.model.group(t.id))])),
  partOf:id=>{const g=groups.model.group(id);return series(g).indexOf(g)},
  sync(){groups.sync();activate(active)},
  merge(source,target){groups.model.merge(source,target)}};
const order=(params.get('order')||'').split(',').filter(Boolean);
for(const id of order)add(id);
</script></body></html>`;
// Home: the owner helper and owner shells pinned outside the groups, with a
// Home tab, a project of ten agents at limit 8 and a plain tab. The workspace
// is saved to this context's storage and restored on reload.
const homeHtml = `<!doctype html><html><head><meta charset="utf-8"><link rel="stylesheet" href="/client/style.css"></head><body><div id="app"><section class="terminal-shell" style="margin:20px"><div class="terminal-tabs"><div class="tab-strip"><div id="home-tab" class="tab home-tab"></div><div id="tabs" style="white-space:nowrap"></div></div></div><div id="terminal-body" style="height:700px;position:relative"></div></section><dialog id="dialog"></dialog></div><script type="module">
import {setupPaneGroups} from '/client/pane-groups.js';
import {leaves} from '/client/pane-layout.js';
import {taskMemberIds} from '/client/tasks.js';
import {workspaceSnapshot,normalizeWorkspace} from '/client/workspace-state.js';
const TASK='tsk_3333333333333333',HELPER='agt_00000000000000aa',agentId=n=>'agt_'+String(n).padStart(16,'0');
const server={id:'srv',host:'fixture',port:22,username:'test'};
const tabs=[],prefs={paneGroupLimit:8};let active=null;
const dialogEl=document.querySelector('#dialog');
function strip(){const home=groups.homeEntry(),el=document.querySelector('#home-tab');el.classList.toggle('active',!!home&&home.ids.includes(active));el.innerHTML=home?'<button data-tab="'+home.tab.id+'"><span class="home-mark">⌂</span><span class="tab-name">'+home.tab.id+'</span></button>':'<button data-home-empty><span class="home-mark">⌂</span><span class="tab-name">Home</span></button>';document.querySelector('#tabs').innerHTML=groups.entries().map(({tab,group})=>'<div class="tab" style="display:inline-block;width:150px"><button data-tab="'+tab.id+'">'+groups.partName(group)+'</button></div>').join('')}
function save(){localStorage.setItem('home-fixture',JSON.stringify(workspaceSnapshot(tabs,groups.model.groups,active,null,[TASK],[],groups.model.projectLayoutSnapshot(),groups.model.home)))}
function activate(id){active=id;groups.render();strip();save()}
const groups=setupPaneGroups({getTabs:()=>tabs,getActive:()=>active,activate,close(){},dialog(title,body){dialogEl.innerHTML='<h2>'+title+'</h2>'+body;if(!dialogEl.open)dialogEl.showModal()},closeDialog(){dialogEl.close()},preferences:()=>prefs,label:t=>t.id,groupName:g=>g.taskId?'Project':leaves(g.tree).join(' + '),changed:()=>{strip();save()}});
function add(id,agent,home){const el=document.createElement('div');el.className='terminal-instance';document.querySelector('#terminal-body').append(el);tabs.push({id,el,task:agent?{taskId:TASK,agentId:agent}:undefined,home:!!home,server,status:'Connected',tmux:true,session:id,wasConnected:true})}
const roster=[{id:HELPER,role:'owner_helper',status:'running'},...Array.from({length:10},(_,i)=>({id:agentId(i+1),status:'running'}))];
groups.model.setTaskMembers(TASK,taskMemberIds({status:'open'},roster));
groups.model.setTaskOrchestrator(TASK,'p1');
const saved=normalizeWorkspace(JSON.parse(localStorage.getItem('home-fixture')||'null'));
if(saved){const homeIds=new Set(saved.home?leaves(saved.home.tree):[]);for(const t of saved.tabs)add(t.id,t.task?.agentId,homeIds.has(t.id));groups.model.loadProjectLayouts(saved.projectLayouts);groups.model.groups=saved.groups;groups.model.home=saved.home?structuredClone(saved.home):null;groups.sync();activate(saved.active)}
else{add('helper',HELPER,true);for(let n=1;n<=10;n++)add('p'+n,agentId(n));add('s1',null,true);add('s2',null,true);add('plain1');groups.sync();activate('p1')}
window.fixture={groups,activate,
  state:()=>JSON.stringify({groups:groups.model.groups.map(g=>({tree:leaves(g.tree),taskId:g.taskId,part:g.part})),home:groups.model.home}),
  home:()=>groups.model.home&&{ids:leaves(groups.model.home.tree),active:groups.model.home.active,ratio:groups.model.home.ratio},
  parts:()=>groups.model.series(groups.model.taskGroup(TASK)).map(g=>leaves(g.tree).length),
  names:()=>[...document.querySelectorAll('#tabs .tab')].map(t=>t.textContent),
  inHome:id=>groups.model.inHome(id),group:id=>{const g=groups.model.group(id);return g?leaves(g.tree):null}};
</script></body></html>`;
async function pointerDrag(page, from, to, { x = 0.5 } = {}) {
  const a = await from.boundingBox(),
    b = await to.boundingBox();
  await page.mouse.move(a.x + a.width / 2, a.y + a.height / 2);
  await page.mouse.down();
  await page.mouse.move(b.x + b.width * x, b.y + b.height / 2, { steps: 12 });
  await page.mouse.up();
}
async function homeArea(browser, origin, name) {
  const context = await browser.newContext({
    viewport: { width: 1440, height: 1000 },
  });
  await context.route("**/home-fixture.html", (route) =>
    route.fulfill({ contentType: "text/html", body: homeHtml }),
  );
  const page = await context.newPage();
  const errors = [];
  page.on("pageerror", (e) => errors.push(e.message));
  await page.goto(`${origin}/home-fixture.html`);
  await page.waitForFunction(() => !!window.fixture);
  const run = (fn, arg) => page.evaluate(fn, arg);
  const header = (id) => page.locator(`.pane-header[data-pane="${id}"]`);
  const label = (id) => header(id).locator(".pane-label");
  const projectTab = page.locator("#tabs .tab", { hasText: /^Project$/ });
  const homeTab = page.locator("#home-tab");
  const boxes = (selector) =>
    page.locator(selector).evaluateAll((nodes) =>
      nodes.map((n) => {
        const r = n.getBoundingClientRect();
        return { id: n.dataset.pane, x: r.x, y: r.y, w: r.width, h: r.height };
      }),
    );
  // Every visible pane sits inside the body and no two overlap.
  const assertBounds = async () => {
    const body = await page.locator("#terminal-body").boundingBox();
    const panes = await boxes(".pane-header");
    for (const p of panes) {
      assert.ok(
        p.x >= body.x - 1 && p.x + p.w <= body.x + body.width + 1,
        p.id,
      );
      assert.ok(p.y >= body.y - 1, p.id);
    }
    const terminals = await page
      .locator(".terminal-instance:not([hidden])")
      .evaluateAll((nodes) =>
        nodes.map((n) => {
          const r = n.getBoundingClientRect();
          return [r.left, r.top, r.right, r.bottom];
        }),
      );
    for (let i = 0; i < terminals.length; i++)
      for (let j = i + 1; j < terminals.length; j++) {
        const [a, b] = [terminals[i], terminals[j]];
        const overlap =
          Math.min(a[2], b[2]) - Math.max(a[0], b[0]) > 1 &&
          Math.min(a[3], b[3]) - Math.max(a[1], b[1]) > 1;
        assert.ok(!overlap, `panes ${i} and ${j} overlap`);
      }
  };
  // a6: Home sits left of every group, outside the cap and continued parts.
  assert.deepEqual(await run(() => fixture.parts()), [8, 2]);
  assert.deepEqual(await run(() => fixture.names()), [
    "Project",
    "Project (continued)",
    "plain1",
  ]);
  const stripOrder = await page
    .locator(".tab-strip > *")
    .evaluateAll((nodes) => nodes.map((n) => n.id));
  assert.deepEqual(stripOrder, ["home-tab", "tabs"], "Home tab first");
  const homeBoxes = await boxes(".pane-header[data-home]");
  assert.deepEqual(
    homeBoxes.map((b) => b.id),
    ["helper", "s1", "s2"],
  );
  const groupBoxes = await boxes(".pane-header:not([data-home])");
  assert.equal(groupBoxes.length, 8);
  const homeRight = Math.max(...homeBoxes.map((b) => b.x + b.w));
  assert.ok(
    groupBoxes.every((b) => b.x >= homeRight),
    "all Home headers sit left of every group header",
  );
  assert.equal(await page.locator(".home-divider").count(), 1);
  await assertBounds();
  await page.screenshot({ path: `.build/home-pane-${name}.png` });
  await run(() => fixture.activate("p9"));
  assert.deepEqual(await boxes(".pane-header[data-home]"), homeBoxes);
  assert.equal(await page.locator(".pane-header:not([data-home])").count(), 2);
  await run(() => fixture.activate("s1"));
  assert.deepEqual(await boxes(".pane-header[data-home]"), homeBoxes);
  assert.deepEqual(
    (await boxes(".pane-header:not([data-home])")).map((b) => b.id).sort(),
    ["p10", "p9"],
    "a focused Home pane keeps the last group beside it",
  );
  assert.equal(await homeTab.getAttribute("class"), "tab home-tab active");
  assert.equal(
    await homeTab.locator("[data-tab]").getAttribute("data-tab"),
    "s1",
  );
  await assertBounds();
  // Narrow: only the focused region shows.
  await page.setViewportSize({ width: 390, height: 844 });
  await page.waitForFunction(
    () => !document.querySelector(".pane-header:not([data-home])"),
  );
  assert.equal(await page.locator(".pane-header[data-home]").count(), 3);
  assert.equal(await page.locator(".home-divider").count(), 0);
  await run(() => fixture.activate("p1"));
  assert.equal(await page.locator(".pane-header[data-home]").count(), 0);
  assert.equal(await page.locator(".pane-header:not([data-home])").count(), 8);
  assert.ok(await homeTab.isVisible());
  await page.setViewportSize({ width: 1440, height: 1000 });
  await page.waitForFunction(
    () => document.querySelectorAll(".pane-header[data-home]").length === 3,
  );
  // a7: refused drags change nothing.
  const refused = async (from, to, what) => {
    const before = await run(() => fixture.state());
    await pointerDrag(page, from, to);
    assert.equal(await run(() => fixture.state()), before, what);
  };
  await refused(label("helper"), projectTab, "helper onto a project tab");
  await refused(label("helper"), label("p2"), "helper onto a group pane");
  await refused(label("p3"), label("s1"), "agent onto a Home pane");
  await refused(label("p3"), homeTab, "agent onto the Home tab");
  await refused(
    homeTab.locator("button"),
    projectTab,
    "the Home tab never drags",
  );
  // The helper cannot be dropped on the tab bar either.
  const before = await run(() => fixture.state());
  const helperBox = await label("helper").boundingBox();
  await page.mouse.move(helperBox.x + 30, helperBox.y + helperBox.height / 2);
  await page.mouse.down();
  await page.mouse.move(helperBox.x + 40, helperBox.y + 40, { steps: 4 });
  assert.ok(await page.locator(".pane-release").isHidden());
  const strip = await page.locator(".terminal-tabs").boundingBox();
  await page.mouse.move(
    strip.x + strip.width - 20,
    strip.y + strip.height / 2,
    {
      steps: 8,
    },
  );
  await page.mouse.up();
  assert.equal(
    await run(() => fixture.state()),
    before,
    "helper onto the tab bar",
  );
  // Allowed: a Home shell joins a plain tab's group.
  await pointerDrag(
    page,
    label("s2"),
    page.locator("#tabs .tab", { hasText: /^plain1$/ }),
  );
  assert.equal(await run(() => fixture.inHome("s2")), false);
  assert.deepEqual((await run(() => fixture.group("plain1"))).sort(), [
    "plain1",
    "s2",
  ]);
  // ↗ gives a Home shell its own tab; its tab dropped on Home rejoins Home.
  await run(() => fixture.activate("s1"));
  await header("s1").getByRole("button", { name: "Move out of Home" }).click();
  assert.deepEqual(await run(() => fixture.group("s1")), ["s1"]);
  assert.equal(
    await header("helper")
      .getByRole("button", { name: "Move out of Home" })
      .count(),
    0,
    "the helper has no way out",
  );
  await pointerDrag(
    page,
    page.locator("#tabs .tab", { hasText: /^s1$/ }),
    homeTab,
  );
  assert.equal(await run(() => fixture.inHome("s1")), true);
  // A plain pane dropped on a Home pane joins Home.
  await run(() => fixture.activate("s2"));
  await pointerDrag(page, label("s2"), label("s1"));
  assert.deepEqual(await run(() => fixture.home().ids), ["helper", "s1", "s2"]);
  // a8: Home persists across a reload, with ten agents in 8 + 2.
  await run(() => fixture.activate("helper"));
  await page.locator(".home-divider").focus();
  await page.keyboard.press("ArrowLeft");
  await page.keyboard.press("ArrowLeft");
  const home = await run(() => fixture.home());
  assert.ok(Math.abs(home.ratio - 0.36) < 1e-9, String(home.ratio));
  const parts = await run(() => fixture.parts());
  const groupsBefore = await run(() =>
    fixture.groups.model.groups.map((g) => ({
      taskId: g.taskId,
      part: g.part,
    })),
  );
  await page.reload();
  await page.waitForFunction(() => !!window.fixture);
  assert.deepEqual(await run(() => fixture.home()), home);
  assert.deepEqual(await run(() => fixture.parts()), parts);
  assert.deepEqual(parts, [8, 2]);
  assert.deepEqual(
    await run(() =>
      fixture.groups.model.groups.map((g) => ({
        taskId: g.taskId,
        part: g.part,
      })),
    ),
    groupsBefore,
  );
  assert.equal(await page.locator(".pane-header[data-home]").count(), 3);
  await assertBounds();
  assert.deepEqual(errors, []);
  console.log(
    `${name}: Home area layout, cap exclusion, narrow regions, drag refusals and moves, and reload persistence passed`,
  );
  await context.close();
}
const range = (from, to, prefix = "p") =>
  Array.from({ length: to - from + 1 }, (_, i) => prefix + (from + i));
async function dragHeader(page, source, tabName, where = "center") {
  const from = await page
    .locator(`.pane-header[data-pane="${source}"] .pane-label`)
    .boundingBox();
  const to = await page
    .locator("#tabs .tab", { hasText: new RegExp(`^${tabName}$`) })
    .boundingBox();
  await page.mouse.move(from.x + from.width / 2, from.y + from.height / 2);
  await page.mouse.down();
  const x = where === "after" ? to.x + to.width * 0.9 : to.x + to.width / 2;
  await page.mouse.move(x, to.y + to.height / 2, { steps: 12 });
  await page.mouse.up();
}
async function dragTab(page, tabName, targetName, where) {
  const from = await page
    .locator("#tabs .tab", { hasText: new RegExp(`^${tabName}$`) })
    .boundingBox();
  const to = await page
    .locator("#tabs .tab", { hasText: new RegExp(`^${targetName}$`) })
    .boundingBox();
  await page.mouse.move(from.x + from.width / 2, from.y + from.height / 2);
  await page.mouse.down();
  const x = where === "after" ? to.x + to.width * 0.9 : to.x + to.width * 0.1;
  await page.mouse.move(x, to.y + to.height / 2, { steps: 12 });
  await page.mouse.up();
}
async function continuedGroups(browser, origin, name) {
  const context = await browser.newContext({
    viewport: { width: 1440, height: 1000 },
  });
  await context.route("**/continued-fixture.html*", (route) =>
    route.fulfill({ contentType: "text/html", body: continuedHtml }),
  );
  const page = await context.newPage();
  const errors = [];
  page.on("pageerror", (e) => errors.push(e.message));
  const open = async (query = "") => {
    await page.goto(`${origin}/continued-fixture.html${query}`);
    await page.waitForFunction(() => !!window.fixture);
  };
  const run = (fn, arg) => page.evaluate(fn, arg);
  // a1/a9: twenty panes arrive in order and fill 8 + 8 + 4.
  await open(`?order=${range(1, 20).join(",")}`);
  assert.deepEqual(
    (await run(() => fixture.parts())).map((part) => [...part].sort()),
    [range(1, 8).sort(), range(9, 16).sort(), range(17, 20).sort()],
  );
  assert.deepEqual(await run(() => fixture.names()), [
    "Project",
    "Project (continued)",
    "Project (continued 2)",
  ]);
  await run(() => fixture.activate("p9"));
  assert.equal(await page.locator(".pane-header").count(), 8);
  // a4: a closed pane frees a slot; nothing else changes group; the next fills it.
  const before = await run(() => fixture.where());
  await run(() => fixture.close("p3"));
  const after = await run(() => fixture.where());
  delete before.p3;
  assert.deepEqual(after, before, "no pane changes groups on close");
  assert.deepEqual(
    (await run(() => fixture.parts())).map((part) => part.length),
    [7, 8, 4],
  );
  await run(() => fixture.add("p21"));
  assert.equal(await run(() => fixture.partOf("p21")), 0);
  for (const id of range(9, 16)) await run((id) => fixture.close(id), id);
  assert.deepEqual(await run(() => fixture.names()), [
    "Project",
    "Project (continued)",
  ]);
  assert.deepEqual(
    [...(await run(() => fixture.parts()))[1]].sort(),
    range(17, 20).sort(),
  );
  for (const id of range(17, 20)) await run((id) => fixture.close(id), id);
  assert.deepEqual(await run(() => fixture.names()), ["Project"]);
  // a1: the orchestrator arriving ninth still sits full height in part 0.
  await open(`?lead=p9&order=${range(1, 12).join(",")}`);
  assert.equal(await run(() => fixture.partOf("p9")), 0);
  await run(() => fixture.activate("p9"));
  const lead = await page.locator('.pane-header[data-pane="p9"]').boundingBox();
  const bodyBox = await page.locator("#terminal-body").boundingBox();
  assert.ok(lead.x - bodyBox.x < 2 && lead.y - bodyBox.y < 2);
  assert.equal(await run(() => fixture.partOf("p8")), 1);
  // a2: limit 4 splits ten panes 4/4/2; raising it again does not merge.
  await open(`?order=${range(1, 10).join(",")}`);
  await run(() => {
    fixture.prefs.paneGroupLimit = 4;
    fixture.sync();
  });
  assert.deepEqual(
    (await run(() => fixture.parts())).map((part) => part.length),
    [4, 4, 2],
  );
  await run(() => {
    fixture.prefs.paneGroupLimit = 8;
    fixture.sync();
  });
  assert.deepEqual(
    (await run(() => fixture.parts())).map((part) => part.length),
    [4, 4, 2],
  );
  // a6: a real drag onto a full group's tab asks what to do.
  await open(`?order=${range(1, 8).join(",")}`);
  await run(() => {
    fixture.add("s1", null);
    fixture.add("s2", null);
    fixture.add("o1", "other");
    fixture.activate("s1");
  });
  const dialog = page.locator("#dialog");
  await dragHeader(page, "s1", "Project");
  await dialog.waitFor({ state: "visible" });
  assert.match(await dialog.textContent(), /Project is full \(8 of 8 panes\)/);
  assert.equal(await dialog.locator("[data-pane-full]").count(), 3);
  await dialog.locator('[data-pane-full="cancel"]').click();
  assert.equal(await run(() => fixture.parts("p1").length), 1);
  assert.deepEqual(await run(() => fixture.parts("s1")), [["s1"]]);
  await dragHeader(page, "s1", "Project");
  await dialog.locator('[data-pane-full="send"]').click();
  assert.equal(await run(() => fixture.partOf("s1")), 1);
  assert.ok((await run(() => fixture.names())).includes("Project (continued)"));
  await run(() => fixture.activate("s2"));
  await dragHeader(page, "s2", "Project");
  const room = dialog.locator('[data-pane-full="room"]');
  const moved = (await room.textContent()).match(/move (\S+) to/)[1];
  assert.match(await room.textContent(), /to Project \(continued\)$/);
  await room.click();
  assert.equal(await run(() => fixture.partOf("s2")), 0);
  assert.equal(await run((id) => fixture.partOf(id), moved), 1);
  assert.equal((await run(() => fixture.parts()))[0].length, 8);
  // Own-project parts accept a moved agent once they have room.
  await run(() => fixture.add("p9"));
  assert.equal(await run(() => fixture.partOf("p9")), 1);
  const departing = (await run(() => fixture.parts()))[0].find((id) =>
    id.startsWith("p"),
  );
  await run((id) => fixture.close(id), departing);
  await run(() => fixture.activate("p9"));
  await dragHeader(page, "p9", "Project");
  assert.equal(await run(() => fixture.partOf("p9")), 0);
  assert.equal(await dialog.isVisible(), false);
  // Another project's agent cannot join.
  await run(() => fixture.activate("o1"));
  await dragHeader(page, "o1", "Project");
  assert.equal(await dialog.isVisible(), false);
  assert.deepEqual(await run(() => fixture.parts("o1")), [["o1"]]);
  // a5/a6: the merge that groupDialog and the palette call shows the same
  // dialog; Send starts a plain "(continued)" series.
  await open(`?order=`);
  await run(() => {
    for (const id of [
      "a1",
      "a2",
      "a3",
      "a4",
      "a5",
      "a6",
      "a7",
      "a8",
      "x1",
      "x2",
    ])
      fixture.add(id, null);
    for (const id of ["a2", "a3", "a4", "a5", "a6", "a7", "a8"])
      fixture.merge(id, "a1");
    fixture.merge("x2", "x1");
    fixture.activate("a1");
  });
  const plainName = "a1 + a8 + a7 + a6 + a5 + a4 + a3 + a2";
  assert.ok((await run(() => fixture.names())).includes(plainName));
  await run(() => fixture.groups.merge("x1", "a1"));
  await dialog.waitFor({ state: "visible" });
  assert.equal(
    await dialog.locator('[data-pane-full="room"]').count(),
    0,
    "a whole group offers Send or Cancel",
  );
  await dialog.locator('[data-pane-full="send"]').click();
  assert.deepEqual(await run(() => fixture.names()), [
    plainName,
    `${plainName} (continued)`,
  ]);
  // A continuation dropping to one pane becomes an ordinary tab.
  await run(() => fixture.close("x2"));
  assert.deepEqual(await run(() => fixture.names()), [plainName, "x1"]);
  await run(() => fixture.add("x2", null));
  await run(() => fixture.groups.merge("x2", "a1", false));
  await dialog.locator('[data-pane-full="send"]').click();
  await run(() => fixture.groups.merge("x1", "x2", false));
  assert.deepEqual(await run(() => fixture.names()), [
    plainName,
    `${plainName} (continued)`,
  ]);
  // The original dropping to one pane leaves; its continuation takes its place.
  for (const id of ["a2", "a3", "a4", "a5", "a6", "a7", "a8"])
    await run((id) => fixture.close(id), id);
  const names = await run(() => fixture.names());
  assert.ok(names.includes("a1"));
  assert.ok(
    names.some((n) => /^x\d \+ x\d$/.test(n)),
    names.join(" | "),
  );
  assert.ok(!names.some((n) => n.includes("continued")));
  // a11: dropping a continuation tab at a tab edge moves the whole series.
  await open(`?order=${range(1, 10).join(",")}`);
  await run(() => fixture.add("z1", null));
  assert.deepEqual(await run(() => fixture.names()), [
    "Project",
    "Project (continued)",
    "z1",
  ]);
  await dragTab(page, "Project \\(continued\\)", "z1", "after");
  assert.deepEqual(await run(() => fixture.names()), [
    "z1",
    "Project",
    "Project (continued)",
  ]);
  assert.deepEqual(errors, []);
  console.log(
    `${name}: 20-pane continuation, orchestrator pin, close and refill, limit setting, full-group drag dialog, own-project move, cross-project refusal, plain series and series reorder passed`,
  );
  await context.close();
}
const vite = await createServer({
  configFile: "vite.config.js",
  mode: "static",
  server: { host: "127.0.0.1", port: 0 },
  logLevel: "error",
});
await vite.listen();
await mkdir(".build", { recursive: true });
try {
  for (const [name, engine] of Object.entries({ chromium, webkit })) {
    const browser = await engine.launch();
    try {
      const page = await browser.newPage({
        viewport: { width: 1440, height: 1000 },
      });
      const errors = [];
      page.on("pageerror", (e) => errors.push(e.message));
      await page.route("**/task-layout-fixture.html", (route) =>
        route.fulfill({ contentType: "text/html", body: html }),
      );
      await page.goto(
        `http://127.0.0.1:${vite.httpServer.address().port}/task-layout-fixture.html`,
      );
      await page.locator('[data-pane="lead"]').waitFor();
      for (const id of ["two", "three", "four", "five"]) {
        await page.evaluate((id) => fixture.add(id), id);
        await page.locator(`[data-pane="${id}"]`).waitFor();
      }
      const boxes = await page.evaluate(() =>
        Object.fromEntries(
          [...document.querySelectorAll(".pane-header")].map((header) => {
            const id = header.dataset.pane,
              h = header.getBoundingClientRect();
            const tab = fixture.groups.model.taskGroup("task");
            const body = document.querySelector("#terminal-body");
            const terminals = [...body.querySelectorAll(".terminal-instance")];
            const ids = ["lead", "two", "three", "four", "five"];
            const terminal = terminals[ids.indexOf(id)].getBoundingClientRect();
            return [
              id,
              { x: h.x, y: h.y, width: h.width, height: terminal.bottom - h.y },
            ];
          }),
        ),
      );
      assert.equal(boxes.lead.height, 700);
      assert.ok(boxes.lead.x < boxes.two.x && boxes.two.x < boxes.three.x);
      assert.equal(boxes.two.x, boxes.four.x);
      assert.equal(boxes.three.x, boxes.five.x);
      assert.ok(boxes.four.y > boxes.two.y && boxes.five.y > boxes.three.y);
      assert.equal(
        await page.locator("[data-detach]").count(),
        0,
        "task agents remain owned",
      );
      await page.screenshot({ path: `.build/task-stacking-${name}.png` });
      const divider = page.locator('.pane-divider[data-axis="x"]').first();
      await divider.focus();
      await page.keyboard.press("ArrowRight");
      const before = await page.evaluate(() => fixture.tree());
      assert.equal(
        await page.evaluate(
          () => fixture.groups.model.taskGroup("task").taskLayout,
        ),
        "manual",
      );
      await page.evaluate(() => fixture.sync());
      assert.deepEqual(await page.evaluate(() => fixture.tree()), before);
      await page.setViewportSize({ width: 390, height: 844 });
      await page.waitForFunction(() =>
        [...document.querySelectorAll(".pane-divider")].every(
          (d) => d.dataset.axis === "y",
        ),
      );
      assert.equal(await page.locator(".pane-header").count(), 5);
      assert.deepEqual(errors, []);
      console.log(
        `${name}: task stacking, full-height lead, ownership, actual keyboard resize preservation and compact layout passed`,
      );
      await continuedGroups(
        browser,
        `http://127.0.0.1:${vite.httpServer.address().port}`,
        name,
      );
      await homeArea(
        browser,
        `http://127.0.0.1:${vite.httpServer.address().port}`,
        name,
      );
    } finally {
      await browser.close();
    }
  }
} finally {
  await vite.close();
}
