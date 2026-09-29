// Physical Option-key pane placement with real pointer events and disposable DOM.
import assert from "node:assert/strict";
import { chromium, webkit } from "@playwright/test";
import { createServer } from "vite";

const html = `<!doctype html><html><head><link rel="stylesheet" href="/client/style.css"></head><body><div id="app"><section class="terminal-shell" style="width:900px;margin:20px"><div class="terminal-tabs"><div id="tabs"></div></div><div id="terminal-body" style="height:600px;position:relative"></div></section><dialog id="dialog"></dialog></div><script type="module">
import {setupPaneGroups} from "/client/pane-groups.js";
const body=document.querySelector("#terminal-body");
const tabs=["a","b","c"].map(id=>{const el=document.createElement("div");el.className="terminal-instance";el.dataset.terminal=id;body.append(el);return {id,el,task:{taskId:"task",agentId:id},server:{host:"fixture",username:"test"},status:"Connected",tmux:true,session:id}});
let active="c";
const d=document.querySelector("#dialog");
const dialog=(title,html)=>{d.innerHTML="<h2 id=dialog-title></h2>"+html;d.querySelector("h2").textContent=title;if(!d.open)d.showModal()};
const closeDialog=()=>{d.close();d.replaceChildren()};
const groups=setupPaneGroups({getTabs:()=>tabs,getActive:()=>active,activate:id=>{active=id;groups.render()},close(){},dialog,closeDialog,preferences:()=>({focusFollowsMouse:false}),label:t=>"Pane "+t.id});
groups.model.sync(tabs.map(t=>t.id));
groups.model.merge("b","a");
groups.model.merge("c","b");
Object.assign(groups.model.group("a"), {taskId:"task", taskLayout:"manual", guests:[]});
groups.render();
window.fixture={tree:()=>structuredClone(groups.model.group("a").tree),group:()=>structuredClone(groups.model.group("a")),move:id=>groups.showMove(id)};
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

const optionKey = { left: "Alt", right: "AltRight" };
const press = (page, key) => page.keyboard.down(key);
const lift = (page, key) => page.keyboard.up(key);
async function setOption(page, side, down) {
  await page.keyboard[down ? "down" : "up"](optionKey[side]);
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

    const preview = page.locator(".pane-drop-preview:not([hidden])");
    const placementNow = () =>
      page.evaluate(() => {
        const el = document.querySelector(".pane-drop-preview");
        return el.hidden ? null : el.dataset.placement;
      });
    const relation = async (first, second) =>
      directRelation(await page.evaluate(() => fixture.tree()), first, second);

    // a6: both header tooltips describe Shift + Left Option, never Right Option.
    await reset(page);
    for (const title of await page
      .locator(".pane-header")
      .evaluateAll((els) => els.map((el) => el.title))) {
      assert.match(
        title,
        /Left Option: split right · Shift \+ Left Option: split above\./,
      );
      assert.doesNotMatch(title, /Right Option/);
    }

    // a1: Left Option previews and drops to the right.
    await beginDrag(page, "a", "c");
    assert.equal(await page.locator(".group-drop-target").count(), 1);
    await setOption(page, "left", true);
    await preview.waitFor();
    assert.equal(await preview.getAttribute("data-placement"), "right");
    assert.equal(await preview.textContent(), "Split right");
    assert.equal(await page.locator(".group-drop-target").count(), 0);
    const target = await page
        .locator('.pane-header[data-pane="c"]')
        .boundingBox(),
      right = await preview.boundingBox();
    assert.ok(right.x > target.x + target.width / 2);
    assert.ok(Math.abs(right.width - (target.width - 8) / 2) < 1);

    // a2: Shift switches the live preview to above and back.
    await press(page, "Shift");
    assert.equal(
      await placementNow(),
      "above",
      `${name}: Shift + Left Option previews above without a pointer move`,
    );
    assert.equal(await preview.textContent(), "Split above");
    const above = await preview.boundingBox();
    assert.ok(Math.abs(above.x - target.x) < 1);
    assert.ok(Math.abs(above.width - target.width) < 1);
    const paneHeight =
      target.height +
      (await page.locator('[data-terminal="c"]').boundingBox()).height;
    assert.ok(Math.abs(above.y - target.y) < 1, `${name}: top half`);
    assert.ok(Math.abs(above.height - (paneHeight - 8) / 2) < 1);
    await lift(page, "Shift");
    assert.equal(
      await placementNow(),
      "right",
      `${name}: releasing Shift returns to split right`,
    );
    await page.mouse.up();
    await setOption(page, "left", false);
    assert.equal(
      await relation("c", "a"),
      "x",
      `${name}: Left Option places the source right of the target`,
    );

    // a2: Shift first, then Left Option; dropping splits above.
    await reset(page);
    await beginDrag(page, "a", "c");
    await press(page, "Shift");
    assert.equal(await placementNow(), null);
    await setOption(page, "left", true);
    assert.equal(
      await placementNow(),
      "above",
      `${name}: Shift before Left Option also previews above`,
    );
    await page.mouse.up();
    await setOption(page, "left", false);
    await lift(page, "Shift");
    assert.equal(
      await relation("a", "c"),
      "y",
      `${name}: Shift + Left Option places the source above the target`,
    );
    assert.deepEqual(
      await page.evaluate(() => {
        const group = fixture.group();
        return { taskId: group.taskId, taskLayout: group.taskLayout };
      }),
      { taskId: "task", taskLayout: "manual" },
      `${name}: directional placement preserves task ownership and manual persistence`,
    );

    // a3: Right Option alone (and with Shift) selects nothing; drop swaps.
    await reset(page);
    const aStart = await page
        .locator('.pane-header[data-pane="a"]')
        .boundingBox(),
      cStart = await page.locator('.pane-header[data-pane="c"]').boundingBox();
    await beginDrag(page, "a", "c");
    await setOption(page, "right", true);
    assert.equal(await placementNow(), null, `${name}: Right Option alone`);
    assert.equal(await page.locator(".group-drop-target").count(), 1);
    await press(page, "Shift");
    assert.equal(
      await placementNow(),
      null,
      `${name}: Shift + Right Option shows no preview`,
    );
    assert.equal(await page.locator(".group-drop-target").count(), 1);
    await lift(page, "Shift");
    await page.mouse.up();
    await setOption(page, "right", false);
    const aSwapped = await page
        .locator('.pane-header[data-pane="a"]')
        .boundingBox(),
      cSwapped = await page
        .locator('.pane-header[data-pane="c"]')
        .boundingBox();
    assert.ok(
      Math.abs(aSwapped.x - cStart.x) < 1 &&
        Math.abs(aSwapped.y - cStart.y) < 1 &&
        Math.abs(cSwapped.x - aStart.x) < 1 &&
        Math.abs(cSwapped.y - aStart.y) < 1,
      `${name}: Right Option drop performs the ordinary swap`,
    );

    // a4: Right Option never changes a Left Option placement.
    await reset(page);
    await beginDrag(page, "a", "c");
    await setOption(page, "left", true);
    await setOption(page, "right", true);
    assert.equal(await placementNow(), "right");
    await press(page, "Shift");
    assert.equal(await placementNow(), "above");
    await lift(page, "Shift");
    await setOption(page, "left", false);
    assert.equal(
      await placementNow(),
      null,
      `${name}: Right Option alone after releasing Left Option`,
    );
    await page.keyboard.press("Escape");
    await page.mouse.up();
    await setOption(page, "right", false);

    // a5: the keyboard Move pane dialog covers all four directions.
    for (const [direction, first, second, axis] of [
      ["above", "a", "c", "y"],
      ["below", "c", "a", "y"],
      ["left", "a", "c", "x"],
      ["right", "c", "a", "x"],
    ]) {
      await reset(page);
      await page.evaluate(() => fixture.move("a"));
      const dialog = page.locator("#dialog[open]");
      await dialog.waitFor();
      assert.equal(await dialog.locator("h2").textContent(), "Move pane");
      const select = page.locator("#pane-move-target");
      assert.equal(
        await page.evaluate(() => document.activeElement?.id),
        "pane-move-target",
      );
      assert.deepEqual(
        await select
          .locator("option")
          .evaluateAll((els) => els.map((el) => [el.value, el.textContent])),
        [
          ["b", "Pane b"],
          ["c", "Pane c"],
        ],
      );
      await select.selectOption("c");
      await page.locator(`[data-pane-move="${direction}"]`).focus();
      await page.keyboard.press("Enter");
      assert.equal(await page.locator("#dialog[open]").count(), 0);
      assert.equal(
        await relation(first, second),
        axis,
        `${name}: keyboard Move ${direction}`,
      );
    }

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
    // a7: Shift held when the window lost focus does not leak into the next drag.
    await reset(page);
    await page.evaluate(() => {
      document.dispatchEvent(
        new KeyboardEvent("keydown", { key: "Shift", shiftKey: true }),
      );
      window.dispatchEvent(new Event("blur"));
    });
    await beginDrag(page, "a", "c");
    await setOption(page, "left", true);
    assert.equal(
      await placementNow(),
      "right",
      `${name}: Left Option after blur splits right`,
    );
    await page.keyboard.press("Escape");
    await page.mouse.up();
    await setOption(page, "left", false);

    assert.deepEqual(errors, []);
    console.log(
      `${name}: Left Option split right, Shift + Left Option split above, Right Option ignored, keyboard Move pane, ordinary swap and cancellation passed`,
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
