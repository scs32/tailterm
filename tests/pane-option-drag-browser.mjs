// Physical Option-key pane placement with real pointer events and disposable DOM.
import assert from "node:assert/strict";
import { chromium, webkit } from "@playwright/test";
import { createServer } from "vite";

const html = `<!doctype html><html><head><link rel="stylesheet" href="/client/style.css"></head><body><div id="app"><section class="terminal-shell" style="width:900px;margin:20px"><div class="terminal-tabs"><div id="tabs"></div></div><div id="terminal-body" style="height:600px;position:relative"></div></section></div><script type="module">
import {setupPaneGroups} from "/client/pane-groups.js";
const body=document.querySelector("#terminal-body");
const tabs=["a","b","c"].map(id=>{const el=document.createElement("div");el.className="terminal-instance";el.dataset.terminal=id;body.append(el);return {id,el,task:{taskId:"task",agentId:id},server:{host:"fixture",username:"test"},status:"Connected",tmux:true,session:id}});
let active="c";
const groups=setupPaneGroups({getTabs:()=>tabs,getActive:()=>active,activate:id=>{active=id;groups.render()},close(){},preferences:()=>({focusFollowsMouse:false}),label:t=>t.id});
groups.model.sync(tabs.map(t=>t.id));
groups.model.merge("b","a");
groups.model.merge("c","b");
Object.assign(groups.model.group("a"), {taskId:"task", taskLayout:"manual", guests:[]});
groups.render();
window.fixture={tree:()=>structuredClone(groups.model.group("a").tree),group:()=>structuredClone(groups.model.group("a"))};
</script></body></html>`;

const vite = await createServer({
  configFile: "vite.config.js",
  mode: "static",
  server: { host: "127.0.0.1", port: 0 },
  logLevel: "error",
});
await vite.listen();
const origin = `http://127.0.0.1:${vite.httpServer.address().port}`;

function directRelation(tree, first, second) {
  if (tree.tab) return null;
  if (tree.a.tab === first && tree.b.tab === second) return tree.axis;
  return (
    directRelation(tree.a, first, second) ||
    directRelation(tree.b, first, second)
  );
}

async function setOption(page, side, down) {
  const key = side === "left" ? "Alt" : "AltRight";
  await page.keyboard[down ? "down" : "up"](key);
}

async function beginDrag(page, source, target) {
  const from = await page
      .locator(`.pane-header[data-pane="${source}"] .pane-label`)
      .boundingBox(),
    to = await page
      .locator(`.pane-header[data-pane="${target}"] .pane-label`)
      .boundingBox();
  await page.mouse.move(from.x + from.width / 2, from.y + from.height / 2);
  await page.mouse.down();
  await page.mouse.move(to.x + to.width / 2, to.y + to.height / 2, {
    steps: 12,
  });
}

async function reset(page) {
  await page.goto(origin + "/pane-option-drag-fixture.html");
  await page.waitForFunction(() => !!window.fixture);
  await page.locator('.pane-header[data-pane="a"]').waitFor();
}

async function run(engine, name) {
  const browser = await engine.launch();
  try {
    const page = await browser.newPage({
      viewport: { width: 1100, height: 760 },
    });
    const errors = [];
    page.on("pageerror", (error) => errors.push(error.message));
    await page.route("**/pane-option-drag-fixture.html", (route) =>
      route.fulfill({ contentType: "text/html", body: html }),
    );

    await reset(page);
    await beginDrag(page, "a", "c");
    assert.equal(await page.locator(".group-drop-target").count(), 1);
    await setOption(page, "left", true);
    const preview = page.locator(".pane-drop-preview:not([hidden])");
    await preview.waitFor();
    assert.equal(await preview.getAttribute("data-placement"), "right");
    assert.equal(await page.locator(".group-drop-target").count(), 0);
    const target = await page
        .locator('.pane-header[data-pane="c"]')
        .boundingBox(),
      right = await preview.boundingBox();
    assert.ok(right.x > target.x + target.width / 2);
    assert.ok(Math.abs(right.width - (target.width - 8) / 2) < 1);

    await setOption(page, "right", true);
    assert.equal(await preview.getAttribute("data-placement"), "above");
    await setOption(page, "right", false);
    assert.equal(
      await preview.getAttribute("data-placement"),
      "right",
      `${name}: releasing the newest Option key falls back to the other key`,
    );
    await page.mouse.up();
    await setOption(page, "left", false);
    assert.equal(
      directRelation(await page.evaluate(() => fixture.tree()), "c", "a"),
      "x",
      `${name}: Left Option places the source right of the target`,
    );
    assert.deepEqual(
      await page.evaluate(() => {
        const group = fixture.group();
        return { taskId: group.taskId, taskLayout: group.taskLayout };
      }),
      { taskId: "task", taskLayout: "manual" },
      `${name}: directional placement preserves task ownership and manual persistence`,
    );

    await reset(page);
    await beginDrag(page, "a", "c");
    await setOption(page, "right", true);
    await preview.waitFor();
    assert.equal(await preview.getAttribute("data-placement"), "above");
    const targetAbove = await page
        .locator('.pane-header[data-pane="c"]')
        .boundingBox(),
      above = await preview.boundingBox();
    assert.ok(Math.abs(above.x - targetAbove.x) < 1);
    assert.ok(Math.abs(above.width - targetAbove.width) < 1);
    await page.mouse.up();
    await setOption(page, "right", false);
    assert.equal(
      directRelation(await page.evaluate(() => fixture.tree()), "a", "c"),
      "y",
      `${name}: Right Option places the source above the target`,
    );

    await reset(page);
    const aBefore = await page
        .locator('.pane-header[data-pane="a"]')
        .boundingBox(),
      cBefore = await page.locator('.pane-header[data-pane="c"]').boundingBox();
    await beginDrag(page, "a", "c");
    await page.mouse.up();
    const aAfter = await page
        .locator('.pane-header[data-pane="a"]')
        .boundingBox(),
      cAfter = await page.locator('.pane-header[data-pane="c"]').boundingBox();
    assert.ok(
      Math.abs(aAfter.x - cBefore.x) < 1 && Math.abs(aAfter.y - cBefore.y) < 1,
      `${name}: ordinary drag still swaps source into the target geometry`,
    );
    assert.ok(
      Math.abs(cAfter.x - aBefore.x) < 1 && Math.abs(cAfter.y - aBefore.y) < 1,
    );

    for (const cancel of ["escape", "blur", "pointercancel"]) {
      await reset(page);
      const before = await page.evaluate(() => fixture.tree());
      await beginDrag(page, "a", "c");
      await setOption(page, "left", true);
      await preview.waitFor();
      if (cancel === "escape") await page.keyboard.press("Escape");
      else if (cancel === "blur")
        await page.evaluate(() => window.dispatchEvent(new Event("blur")));
      else
        await page.locator(".terminal-shell").dispatchEvent("pointercancel", {
          pointerId: 1,
          isPrimary: true,
        });
      assert.equal(
        await page.locator(".pane-drop-preview:not([hidden])").count(),
        0,
      );
      assert.equal(await page.locator(".group-drop-target").count(), 0);
      assert.deepEqual(await page.evaluate(() => fixture.tree()), before);
      await page.mouse.up();
      await setOption(page, "left", false);
    }
    assert.deepEqual(errors, []);
    console.log(
      `${name}: physical Option placement, live preview, fallback, ordinary swap and cancellation passed`,
    );
  } finally {
    await browser.close();
  }
}

try {
  for (const [name, engine] of Object.entries({ chromium, webkit }))
    await run(engine, name);
} finally {
  await vite.close();
}
