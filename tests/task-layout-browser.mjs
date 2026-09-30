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
    } finally {
      await browser.close();
    }
  }
} finally {
  await vite.close();
}
