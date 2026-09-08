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
    } finally {
      await browser.close();
    }
  }
} finally {
  await vite.close();
}
