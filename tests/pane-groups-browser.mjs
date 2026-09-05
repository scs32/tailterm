import assert from "node:assert/strict";
import { assertTerminalBounds } from "./terminal-bounds.mjs";

export async function exercisePaneGroups(page, getInput = () => undefined) {
  const originalCount = await page.locator("#tabs .tab").count();
  const ids = await page
    .locator("#tabs [data-tab]")
    .evaluateAll((nodes) => nodes.slice(-3).map((n) => n.dataset.tab));
  const tab = (id) => page.locator(`#tabs .tab:has([data-tab="${id}"])`);
  const pane = (id) => page.locator(`.pane-header[data-pane="${id}"]`);
  const originalOrder = await page
    .locator("#tabs [data-tab]")
    .evaluateAll((nodes) => nodes.map((n) => n.dataset.tab));
  const reorderSourceBox = await tab(ids[2]).boundingBox(),
    targetBox = await tab(ids[0]).boundingBox();
  await page.mouse.move(
    reorderSourceBox.x + reorderSourceBox.width / 2,
    reorderSourceBox.y + reorderSourceBox.height / 2,
  );
  await page.mouse.down();
  await page.mouse.move(targetBox.x + 4, targetBox.y + targetBox.height / 2, {
    steps: 12,
  });
  await page.mouse.up();
  const expectedOrder = originalOrder.filter((id) => id !== ids[2]);
  expectedOrder.splice(expectedOrder.indexOf(ids[0]), 0, ids[2]);
  assert.deepEqual(
    await page
      .locator("#tabs [data-tab]")
      .evaluateAll((nodes) => nodes.map((n) => n.dataset.tab)),
    expectedOrder,
  );
  assert.equal(await page.locator("#tabs .tab").count(), originalCount);
  await tab(ids[1]).dragTo(tab(ids[0]));
  assert.equal(await page.locator("#tabs .tab").count(), originalCount - 1);
  assert.equal(
    await page.locator(".terminal-instance:not([hidden])").count(),
    2,
  );
  await tab(ids[2]).dragTo(page.locator(".group-tab"));
  assert.equal(await page.locator("#tabs .tab").count(), originalCount - 2);
  assert.equal(
    await page.locator(".terminal-instance:not([hidden])").count(),
    3,
  );
  assert.equal(await page.locator(".pane-divider").count(), 2);
  await assertTerminalBounds(page);
  const rectangles = await page.locator(".pane-header").evaluateAll((nodes) =>
    nodes.map((node) => {
      const r = node.getBoundingClientRect();
      return { id: node.dataset.pane, x: r.x, y: r.y };
    }),
  );
  const vertical = rectangles
    .filter((a) =>
      rectangles.some((b) => a.id !== b.id && Math.abs(a.x - b.x) < 2),
    )
    .sort((a, b) => a.y - b.y);
  assert.equal(vertical.length, 2);
  const single = rectangles.find((p) => !vertical.includes(p));
  const towardSingle = single.x < vertical[0].x ? "ArrowLeft" : "ArrowRight";
  const towardPair = single.x < vertical[0].x ? "ArrowRight" : "ArrowLeft";
  await pane(vertical[0].id).locator(".pane-label").click();
  await page.waitForFunction(() =>
    document.querySelector(".focused-pane")?.contains(document.activeElement),
  );
  const inputBefore = getInput();
  await page.keyboard.press("Control+Alt+ArrowDown");
  await page.waitForFunction(
    (id) => document.querySelector(".tab.active [data-tab]").dataset.tab === id,
    vertical[1].id,
  );
  await page.keyboard.press("Alt+Shift+ArrowUp");
  await page.waitForFunction(
    (id) => document.querySelector(".tab.active [data-tab]").dataset.tab === id,
    vertical[0].id,
  );
  await page.keyboard.press(`Alt+Shift+${towardSingle}`);
  await page.waitForFunction(
    (id) => document.querySelector(".tab.active [data-tab]").dataset.tab === id,
    single.id,
  );
  await page.keyboard.press(`Alt+Shift+${towardSingle}`);
  assert.equal(
    await page.locator(".tab.active [data-tab]").getAttribute("data-tab"),
    single.id,
  );
  await page.keyboard.press(`Alt+Shift+${towardPair}`);
  await page.waitForFunction(
    (ids) =>
      ids.includes(
        document.querySelector(".tab.active [data-tab]").dataset.tab,
      ),
    vertical.map((p) => p.id),
  );
  assert.equal(
    getInput(),
    inputBefore,
    "Pane shortcuts must never send input to SSH",
  );

  // Real mouse resize, then keyboard reset and keyboard resize.
  const divider = page.locator('.pane-divider[data-axis="x"]').first();
  const before = Number(await divider.getAttribute("aria-valuenow"));
  const bounds = await divider.boundingBox();
  await page.mouse.move(
    bounds.x + bounds.width / 2,
    bounds.y + bounds.height / 2,
  );
  await page.mouse.down();
  await page.mouse.move(bounds.x + 75, bounds.y + bounds.height / 2, {
    steps: 8,
  });
  await page.mouse.up();
  assert.notEqual(Number(await divider.getAttribute("aria-valuenow")), before);
  await divider.press("Enter");
  assert.equal(Number(await divider.getAttribute("aria-valuenow")), 50);
  await divider.press("ArrowLeft");
  assert.equal(Number(await divider.getAttribute("aria-valuenow")), 48);
  // Hover updates the selected connection, not only xterm's focus.
  const header = await pane(ids[0]).boundingBox();
  await page.mouse.move(header.x + 30, header.y + 70, { steps: 4 });
  await page.waitForFunction(
    (id) =>
      document.querySelector(".tab.active [data-tab]")?.dataset.tab === id,
    ids[0],
  );
  await page.waitForFunction(() =>
    document.querySelector(".focused-pane")?.contains(document.activeElement),
  );
  await page.keyboard.type("pane-focus-probe");
  await page.waitForFunction(() =>
    document
      .querySelector(".focused-pane")
      .textContent.includes("pane-focus-probe"),
  );
  assert.ok(
    await page
      .locator(".terminal-instance:not([hidden]):not(.focused-pane)")
      .evaluateAll((nodes) =>
        nodes.every((node) => !node.textContent.includes("pane-focus-probe")),
      ),
  );
  // Reconnect retains the tree and pane identifier.
  await page.locator("#reconnect").click();
  await page.waitForFunction(() =>
    document
      .querySelector("#terminal-status")
      .textContent.includes("Connected"),
  );
  assert.equal(await page.locator(".pane-header").count(), 3);
  await page.locator("#fullscreen").click();
  await page.waitForFunction(() => !!document.fullscreenElement);
  await page.screenshot({ path: "grouped-terminals-preview.png" });
  assert.equal(
    await page.locator(".terminal-instance:not([hidden])").count(),
    3,
  );
  // Drag a member out into its own tab while fullscreen.
  const sourceBox = await pane(ids[2]).locator(".pane-label").boundingBox();
  await page.mouse.move(sourceBox.x + 35, sourceBox.y + sourceBox.height / 2);
  await page.mouse.down();
  await page.mouse.move(sourceBox.x + 45, sourceBox.y + 40, { steps: 4 });
  const releaseBox = await page.locator(".pane-release").boundingBox();
  assert.ok(releaseBox, "Fullscreen drag exposes the detach target");
  await page.mouse.move(
    releaseBox.x + releaseBox.width / 2,
    releaseBox.y + releaseBox.height / 2,
    { steps: 12 },
  );
  await page.mouse.up();
  assert.equal(await page.locator("#tabs .tab").count(), originalCount - 1);
  assert.equal(
    await page.locator(".terminal-instance:not([hidden])").count(),
    1,
  );
  // A real drag can also merge tabs while fullscreen.
  await tab(ids[2]).dragTo(page.locator(".group-tab"));
  assert.equal(await page.locator(".pane-header").count(), 3);
  await page.locator("#fullscreen").click();
  // Expanded mode fills the browser viewport without invoking fullscreen.
  await page.locator("#expand-terminal").click();
  assert.equal(await page.evaluate(() => !!document.fullscreenElement), false);
  const expanded = await page.locator(".terminal-shell").boundingBox();
  assert.equal(Math.round(expanded.x), 0);
  assert.equal(Math.round(expanded.y), 0);
  assert.equal(Math.round(expanded.width), page.viewportSize().width);
  assert.equal(Math.round(expanded.height), page.viewportSize().height);
  await assertTerminalBounds(page);
  await pane(ids[2])
    .getByRole("button", { name: "Ungroup pane", exact: true })
    .click();
  await tab(ids[2]).dragTo(page.locator(".group-tab"));
  assert.equal(await page.locator(".pane-header").count(), 3);
  await page.locator("#new-tab").click();
  await page.locator("#empty-terminal").waitFor({ state: "visible" });
  await page.locator(".group-tab [data-tab]").click();
  await page.screenshot({ path: "expanded-terminal-preview.png" });
  await page.locator("#expand-terminal").click();
  assert.equal(await page.locator(".terminal-shell.expanded").count(), 0);
  await page.setViewportSize({ width: 390, height: 844 });
  await page.waitForFunction(() =>
    document.querySelector('.pane-divider[data-axis="y"]'),
  );
  assert.ok(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= innerWidth,
    ),
  );
  await page.screenshot({
    path: "grouped-terminals-mobile-preview.png",
    fullPage: true,
  });
  await page.setViewportSize({ width: 1440, height: 1050 });
  // Close one member, retaining its siblings, then separate without disconnecting.
  await pane(ids[2])
    .getByRole("button", { name: "Close pane", exact: true })
    .click();
  assert.equal(await page.locator(".pane-header").count(), 2);
  await pane(ids[1])
    .getByRole("button", { name: "Ungroup pane", exact: true })
    .click();
  assert.equal(await page.locator("#tabs .tab").count(), originalCount - 1);
  assert.equal(await page.locator(".pane-divider").count(), 0);
  assert.equal(
    await page.locator(".terminal-instance:not([hidden])").count(),
    1,
  );
}
