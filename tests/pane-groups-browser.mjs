import assert from "node:assert/strict";
import { assertTerminalBounds } from "./terminal-bounds.mjs";

// Pane image upload (fa356b7) and the tab session menu with its decoration
// (f9be48d) are static-mode controls. Server mode must not show them; every
// other layout, drag, keyboard and close check runs in both modes.
// TailOS (static mode) pins Home for the owner helper only: every terminal the
// owner opens is an ordinary tab, and with no helper there is no Home tab.
async function groupsDialog(page) {
  await page.locator("#commands").click();
  await page.locator("#command-query").fill("Manage terminal groups");
  await page
    .locator("#command-results button")
    .filter({ hasText: "Manage terminal groups" })
    .click();
}
const homeIds = (page) =>
  page
    .locator(".pane-header[data-home]")
    .evaluateAll((nodes) => nodes.map((n) => n.dataset.pane));
async function openShell(page) {
  const before = await page.locator(".terminal-instance").count();
  await page.locator("#new-tab").click();
  await page.locator("#launcher-shell").click();
  await page.waitForFunction(
    (count) =>
      document.querySelectorAll(".terminal-instance").length > count &&
      document
        .querySelector("#terminal-status")
        .textContent.includes("Connected"),
    before,
  );
}
// No terminal the owner opens is in Home, so there is nothing to move; kept for
// the suites that call it before their per-tab checks.
export async function homeShellsToTabs(page) {
  assert.deepEqual(
    await page
      .locator(".pane-header[data-home]")
      .evaluateAll((nodes) =>
        nodes
          .map((n) => n.dataset.pane)
          .filter((id) => document.querySelector(`#tabs [data-tab="${id}"]`)),
      ),
    [],
  );
  return [];
}
// Static mode with no owner helper: launcher shells are ordinary tabs, nothing
// is in Home, the Home tab is hidden and nothing offers a move into Home.
async function checkNoHome(page) {
  const homeTab = page.locator(".tab-strip > .home-tab");
  assert.equal(await homeTab.count(), 1, "TailOS keeps the Home tab element");
  assert.ok(
    (await page.locator("#tabs [data-tab]").count()) >= 3,
    "the launcher shells opened as tabs",
  );
  assert.deepEqual(await homeIds(page), [], "no shell is in Home");
  assert.equal(await page.locator(".home-divider").count(), 0);
  assert.equal(await page.locator("#tabs .home-tab").count(), 0);
  assert.equal(
    await page.title(),
    "Tailterm · Your servers, one workspace",
    "the default title without a helper",
  );
  const original = page.viewportSize();
  for (const width of [1440, 390]) {
    await page.setViewportSize({ width, height: original.height });
    assert.ok(
      await homeTab.isHidden(),
      `no Home tab without a helper at ${width}px`,
    );
    const plus = await page.locator("#new-tab").boundingBox(),
      tab = await page.locator("#tabs .tab").first().boundingBox();
    assert.ok(
      Math.abs(tab.y - plus.y) < 1 && Math.abs(tab.height - plus.height) < 1,
      `tabs match the + frame at ${width}px`,
    );
  }
  await page.setViewportSize(original);
  const focused = await page
    .locator("#tabs .tab.active [data-tab]")
    .getAttribute("data-tab");
  await page.locator("#new-tab").click();
  await page.locator("#empty-terminal").waitFor({ state: "visible" });
  assert.ok(await homeTab.isHidden(), "no Home tab while the launcher shows");
  await page.locator(`#tabs [data-tab="${focused}"]`).click();
  await groupsDialog(page);
  await page.locator("#dialog [data-merge]").first().waitFor();
  assert.equal(
    await page.locator("[data-home-move]").count(),
    0,
    "Terminal groups offers no move into or out of Home",
  );
  assert.equal(
    await page.locator("#dialog").getByText("Home", { exact: true }).count(),
    0,
  );
  await page.keyboard.press("Escape");
  await page.locator("#dialog").waitFor({ state: "hidden" });
}
// A new launcher shell is one more ordinary tab, never a Home pane.
async function checkShellOpensAsTab(page) {
  const tabCount = () => page.locator("#tabs .tab").count();
  const before = await tabCount();
  await openShell(page);
  assert.equal(await tabCount(), before + 1, "a new shell gets its own tab");
  assert.deepEqual(await homeIds(page), []);
  assert.ok(await page.locator(".tab-strip > .home-tab").isHidden());
  const opened = await page
    .locator("#tabs .tab.active [data-tab]")
    .getAttribute("data-tab");
  await page
    .locator(`.pane-header[data-pane="${opened}"]`)
    .getByRole("button", { name: "Close pane", exact: true })
    .click();
  await page.waitForFunction(
    (count) => document.querySelectorAll("#tabs .tab").length === count,
    before,
  );
}

const sameBox = (a, b) =>
  ["x", "y", "width", "height"].every((key) => Math.abs(a[key] - b[key]) < 1);
// The owner helper's window, for a suite with a hub fixture and a bound
// owner_helper pane `id`. Focuses Home and checks that it is named from the
// hub role and the project (never the tmux session), in the Home tab and the
// browser title, and that the helper is alone at the full terminal width.
// Returns the helper header's box within the terminal area; pass an earlier one
// as `same` to assert the window kept its place and size. `plain` is an ordinary tab that Home refuses.
export async function checkHelperWindow(
  page,
  { id, project, session, same, plain, step = "helper window" },
) {
  const name = `Owner helper - ${project}`;
  const homeTab = page.locator(".tab-strip > .home-tab");
  await homeTab.locator(`[data-tab="${id}"]`).click();
  await page.locator(".tab-strip > .home-tab.active").waitFor();
  assert.equal(await homeTab.locator(".tab-name").textContent(), name, step);
  assert.equal(
    await homeTab.locator(".home-mark").getAttribute("aria-label"),
    `Home: ${name}`,
    step,
  );
  if (session)
    assert.ok(
      !(await homeTab.locator("[data-tab]").textContent()).includes(session),
      `${step}: the Home tab does not show the tmux session name`,
    );
  await page.waitForFunction((text) => document.title.includes(text), name);
  assert.equal(
    await page.locator(`#tabs [data-tab="${id}"]`).count(),
    0,
    `${step}: the helper has no ordinary tab`,
  );
  assert.deepEqual(await homeIds(page), [id], `${step}: the helper alone`);
  assert.equal(
    await page.locator(".pane-header:not([data-home])").count(),
    0,
    `${step}: no group shows beside Home`,
  );
  assert.equal(await page.locator(".home-divider").count(), 0, step);
  const header = page.locator(`.pane-header[data-home][data-pane="${id}"]`);
  assert.equal(await header.locator("[data-detach]").count(), 0, step);
  assert.ok(
    (await header.locator(".pane-label").textContent()).startsWith(name),
    `${step}: the pane header names the helper`,
  );
  // The box is relative to the terminal area: at narrow widths the shell
  // around it can scroll sideways by a pixel, which is not Home moving.
  const box = await header.evaluate((el) => {
    const body = document.querySelector("#terminal-body");
    const r = el.getBoundingClientRect(),
      b = body.getBoundingClientRect();
    return {
      x: r.x - b.x,
      y: r.y - b.y,
      width: r.width,
      height: r.height,
      body: body.clientWidth,
    };
  });
  assert.ok(
    Math.abs(box.x) <= 1 && Math.abs(box.width - box.body) <= 1,
    `${step}: the helper fills the terminal width (${box.width} of ${box.body})`,
  );
  if (same)
    assert.ok(
      sameBox(box, same),
      `${step}: the helper kept its place and size (${JSON.stringify(box)} was ${JSON.stringify(same)})`,
    );
  if (plain) {
    const tab = page.locator(`#tabs .tab:has([data-tab="${plain}"])`);
    for (const target of [homeTab, header]) {
      await tab.dragTo(target);
      await homeTab.locator(`[data-tab="${id}"]`).click();
      await header.waitFor();
      assert.deepEqual(await homeIds(page), [id], `${step}: drop refused`);
      assert.equal(
        await page.locator(`#tabs [data-tab="${plain}"]`).count(),
        1,
        `${step}: the plain tab stays a tab`,
      );
    }
  }
  return box;
}
// Focusing a group shows the group alone: Home never sits beside it.
export async function checkGroupAlone(page, tabId) {
  await page.locator(`#tabs [data-tab="${tabId}"]`).click();
  await page.locator(`.pane-header[data-pane="${tabId}"]`).waitFor();
  assert.deepEqual(await homeIds(page), [], "a focused group hides Home");
  assert.equal(await page.locator(".home-divider").count(), 0);
  assert.ok(
    await page.locator(".tab-strip > .home-tab").isVisible(),
    "the Home tab stays in the strip",
  );
  assert.equal(await page.locator(".tab-strip > .home-tab.active").count(), 0);
  assert.ok(
    !(await page.title()).includes("Owner helper"),
    "the title names the helper only while it is focused",
  );
}

export async function exercisePaneGroups(
  page,
  getInput = () => undefined,
  { staticControls = true } = {},
) {
  if (!staticControls)
    assert.equal(
      await page.locator(".home-tab, .pane-header[data-home]").count(),
      0,
      "server mode has no Home area",
    );
  if (staticControls) await checkNoHome(page);
  const originalCount = await page.locator("#tabs .tab").count();
  const ids = await page
    .locator("#tabs [data-tab]")
    .evaluateAll((nodes) => nodes.slice(-3).map((n) => n.dataset.tab));
  const tab = (id) => page.locator(`#tabs .tab:has([data-tab="${id}"])`);
  const pane = (id) => page.locator(`.pane-header[data-pane="${id}"]`);
  async function checkUploadButton(id) {
    if (!staticControls) {
      assert.equal(
        await pane(id).locator("[data-pane-upload]").count(),
        0,
        "server mode has no pane upload control",
      );
      return;
    }
    const session = (await pane(id).locator(".pane-label").textContent())
      .split(" · ")
      .at(-2);
    const chooserPromise = page.waitForEvent("filechooser");
    await pane(id).locator("[data-pane-upload]").click();
    const chooser = await chooserPromise;
    await chooser.setFiles({
      name: "pane-upload.png",
      mimeType: "image/png",
      buffer: Buffer.from([1, 2, 3]),
    });
    await page.locator("#upload-dialog").waitFor({ state: "visible" });
    assert.ok(
      (await page.locator("#upload-destination").textContent()).endsWith(
        `— ${session === "SSH" ? "SSH shell" : session}`,
      ),
      "upload targets the clicked session",
    );
    await page.locator("#upload-cancel").click();
  }
  // Inactive tabs hide controls and their separators, but remain keyboard usable.
  await tab(ids[0]).locator("[data-tab]").click();
  assert.equal(
    await page.locator(".pane-header").count(),
    1,
    "single sessions use the same pane header",
  );
  assert.equal(await pane(ids[0]).locator("[data-detach]").count(), 0);
  assert.ok(
    await pane(ids[0])
      .getByRole("button", { name: "Close pane", exact: true })
      .isVisible(),
  );
  await assertTerminalBounds(page);
  const singleHeader = await pane(ids[0]).boundingBox();
  const singleTerminal = await page
    .locator(".terminal-instance:not([hidden])")
    .boundingBox();
  assert.equal(singleTerminal.y, singleHeader.y + singleHeader.height);
  await page.screenshot({ path: "/tmp/tailterm-single-pane.png" });
  await checkUploadButton(ids[0]);
  await page.mouse.move(0, 0);
  const inactive = tab(ids[1]);
  if (!staticControls)
    assert.equal(
      await page.locator("#tabs [data-session-menu]").count(),
      0,
      "server mode has no tab session menu",
    );
  // The control a focused inactive tab reveals and Tab moves to next.
  const tabControl = staticControls ? "[data-session-menu]" : "[data-close]";
  assert.ok(await inactive.locator("[data-close]").isHidden());
  assert.ok(await inactive.locator("[data-session-menu]").isHidden());
  assert.ok(await inactive.locator(".tab-tmux").isHidden());
  const collapsedWidth = await inactive
    .locator("[data-tab]")
    .evaluate((el) => el.getBoundingClientRect().width);
  await inactive.hover();
  assert.ok(await tab(ids[0]).locator("[data-close]").isHidden());
  assert.ok(await tab(ids[0]).locator(".tab-tmux").isHidden());
  assert.ok(await inactive.locator("[data-close]").isVisible());
  assert.ok(await inactive.locator(tabControl).isVisible());
  assert.equal(await inactive.locator(".tab-tmux, .tab-activity").count(), 0);
  assert.equal(
    await inactive.locator("[data-tab]").innerText(),
    await inactive.locator(".tab-name").innerText(),
  );
  assert.ok(
    await inactive
      .locator("[data-tab]")
      .evaluate(
        (el, before) => el.getBoundingClientRect().width < before,
        collapsedWidth,
      ),
  );
  await page.mouse.move(0, 0);
  assert.ok(await tab(ids[0]).locator("[data-close]").isVisible());
  await inactive.locator("[data-tab]").focus();
  assert.ok(await inactive.locator(tabControl).isVisible());
  await page.keyboard.press("Tab");
  assert.ok(
    await inactive
      .locator(tabControl)
      .evaluate((el) => el === document.activeElement),
  );
  await page.evaluate(() => document.activeElement.blur());
  await page.screenshot({ path: ".build/quiet-inactive-tabs.png" });
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
  const decorate = async (target, values) => {
    if (!staticControls) return;
    await target.hover();
    await target.locator("[data-session-menu]").click();
    await page.locator("#decorate-tab").click();
    for (const [key, value] of Object.entries(values)) {
      const control = page.locator(`#tab-decoration-form [name="${key}"]`);
      if (["color", "fill", "font"].includes(key))
        await control.selectOption(value);
      else await control.fill(value);
    }
    await page.locator("#tab-decoration-form .primary").click();
  };
  await decorate(tab(ids[0]), {
    label: "Air A",
    color: "blue",
    fill: "blue",
    emoji: "🍎",
  });
  await decorate(tab(ids[1]), {
    label: "Air B",
    color: "rose",
    fill: "rose",
    emoji: "🚀",
  });
  await tab(ids[1]).dragTo(tab(ids[0]));
  assert.equal(await page.locator("#tabs .tab").count(), originalCount - 1);
  assert.equal(
    await page.locator(".terminal-instance:not([hidden])").count(),
    2,
  );
  assert.equal(
    await page.locator(".group-tab").getAttribute("data-tab-color"),
    "default",
    "a new parent does not borrow the focused child's appearance",
  );
  await pane(ids[1]).locator(".pane-label").click();
  await checkUploadButton(ids[0]);
  assert.equal(
    await page.locator(".pane-header.active").getAttribute("data-pane"),
    ids[1],
    "upload can target a different pane without changing focus",
  );
  // Font controls belong to the focused session, not its group or the app default.
  const paneSizes = () =>
    page
      .locator(".terminal-instance .xterm-rows")
      .evaluateAll((nodes) =>
        nodes.map((node) => parseFloat(getComputedStyle(node).fontSize)),
      );
  await pane(ids[0]).click();
  const beforeSizes = await paneSizes();
  await page.locator("#font-up").click();
  await page.waitForTimeout(100);
  const largerSizes = await paneSizes();
  assert.equal(
    largerSizes.filter((size, i) => size !== beforeSizes[i]).length,
    1,
  );
  assert.equal(
    largerSizes.reduce((sum, size) => sum + size, 0),
    beforeSizes.reduce((sum, size) => sum + size, 0) + 1,
  );
  await page.locator("#appearance").click();
  await page.locator("#appearance-larger").click();
  assert.deepEqual(
    await paneSizes(),
    largerSizes,
    "changing the new-session default preserves existing sizes",
  );
  await page.locator("#appearance-smaller").click();
  await page.locator("#dialog-close").click();
  await pane(ids[1]).click();
  assert.deepEqual(
    await paneSizes(),
    largerSizes,
    "changing group focus preserves each size",
  );
  await pane(ids[0]).click();
  await page.locator("#font-down").click();
  await page.waitForTimeout(100);
  assert.deepEqual(await paneSizes(), beforeSizes);
  await decorate(page.locator(".group-tab"), {
    label: "My machines",
    color: "violet",
    fill: "amber",
    emoji: "📦",
  });
  // Server mode cannot decorate: the parent keeps the default appearance.
  const checkParent = async () => {
    assert.equal(
      await page.locator(".group-tab").getAttribute("data-tab-color"),
      staticControls ? "violet" : "default",
    );
    assert.equal(
      await page.locator(".group-tab").getAttribute("data-tab-fill"),
      staticControls ? "amber" : "default",
    );
    if (staticControls)
      assert.match(
        await page.locator(".group-tab .tab-name").innerText(),
        /📦 My machines$/,
      );
  };
  const beforeSwap = await pane(ids[0]).boundingBox();
  const otherBeforeSwap = await pane(ids[1]).boundingBox();
  const connectionsBeforeSwap = await page
    .locator(".terminal-instance")
    .count();
  await pane(ids[0]).dragTo(pane(ids[1]));
  const swapped = await pane(ids[0]).boundingBox();
  assert.ok(
    Math.abs(swapped.x - otherBeforeSwap.x) < 1 &&
      Math.abs(swapped.y - otherBeforeSwap.y) < 1,
    "dragging within a group swaps positions",
  );
  await checkParent();
  assert.equal(
    await page.locator(".terminal-instance").count(),
    connectionsBeforeSwap,
  );
  // The terminal body is also a drop target, not just the narrow header.
  const destination = await pane(ids[1]).boundingBox();
  await pane(ids[0]).hover();
  await page.mouse.down();
  await page.mouse.move(
    destination.x + destination.width / 2,
    destination.y + destination.height + 60,
    { steps: 12 },
  );
  await page.mouse.up();
  const restored = await pane(ids[0]).boundingBox();
  assert.ok(
    Math.abs(restored.x - beforeSwap.x) < 1 &&
      Math.abs(restored.y - beforeSwap.y) < 1,
  );
  await checkParent();
  // Per-session colors need the static session menu's decoration.
  for (const [id, name, color] of staticControls
    ? [
        [ids[0], "🍎 Air A", "blue"],
        [ids[1], "🚀 Air B", "rose"],
      ]
    : []) {
    await pane(id).locator(".pane-label").click();
    await checkParent();
    assert.equal(await pane(id).getAttribute("data-tab-color"), color);
    assert.ok(
      (await pane(id).locator(".pane-label").innerText()).startsWith(
        name + " ·",
      ),
    );
    const frame = await pane(id).evaluate(
      (el) => getComputedStyle(el).borderBottomColor,
    );
    const colors = await page
      .locator(".focused-pane, .pane-header.active")
      .evaluateAll((nodes) =>
        nodes.flatMap((el) => {
          const s = getComputedStyle(el);
          return [
            s.borderTopColor,
            s.borderRightColor,
            s.borderBottomColor,
            s.borderLeftColor,
          ];
        }),
      );
    assert.ok(
      colors.every((c) => c === frame),
      "active session uses its own color for the entire outline",
    );
    const other = id === ids[0] ? ids[1] : ids[0];
    assert.notEqual(
      await pane(other).evaluate(
        (el) => getComputedStyle(el).borderBottomColor,
      ),
      frame,
      "inactive session keeps its own colored underline",
    );
  }
  await page.screenshot({ path: ".build/independent-group-appearance.png" });
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
  await checkParent();
  await page.locator("#fullscreen").click();
  await page.waitForFunction(() => !!document.fullscreenElement);
  await page.screenshot({ path: "grouped-terminals-preview.png" });
  assert.equal(
    await page.locator(".terminal-instance:not([hidden])").count(),
    3,
  );
  assert.equal(
    await page.evaluate(
      () => document.fullscreenElement === document.documentElement,
    ),
    true,
  );
  await page.locator("#expand-terminal").click();
  assert.equal(await page.evaluate(() => !!document.fullscreenElement), true);
  assert.equal(
    await page
      .locator("#workspace")
      .evaluate((el) => el.classList.contains("expanded")),
    true,
  );
  await page.locator("#expand-terminal").click();
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
  const expanded = await page.locator("#workspace").boundingBox();
  assert.equal(Math.round(expanded.x), 0);
  assert.ok(expanded.y >= 0);
  assert.equal(Math.round(expanded.width), page.viewportSize().width);
  assert.ok(await page.locator("#fullscreen").isVisible());
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
  if (staticControls) {
    assert.equal(await tab(ids[0]).getAttribute("data-tab-color"), "blue");
    assert.equal(await tab(ids[1]).getAttribute("data-tab-color"), "rose");
    assert.equal(
      await tab(ids[0]).locator(".tab-name").innerText(),
      "🍎 Air A",
    );
    assert.equal(
      await tab(ids[1]).locator(".tab-name").innerText(),
      "🚀 Air B",
    );
    for (const id of ids.slice(0, 2)) {
      await tab(id).hover();
      await tab(id).locator("[data-session-menu]").click();
      await page.locator("#decorate-tab").click();
      await page.locator("#reset-tab-decoration").click();
      await page.locator("#tab-decoration-form .primary").click();
    }
  } else {
    for (const id of ids.slice(0, 2))
      assert.equal(await tab(id).getAttribute("data-tab-color"), "default");
  }
  if (staticControls) await checkShellOpensAsTab(page);
}
