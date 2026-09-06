import assert from "node:assert/strict";
export async function exercisePopupReview(page) {
  const wasFullscreen = await page.evaluate(() => !!document.fullscreenElement);
  if (wasFullscreen) {
    await page.evaluate(() => document.exitFullscreen());
    await page.waitForFunction(() => !document.fullscreenElement);
    // Chromium updates native window bounds after the DOM fullscreen event.
    await page.waitForTimeout(250);
  }
  const original = page.viewportSize();
  const fits = async (name) => {
    await page.waitForTimeout(180);
    const box = await page.locator("#dialog").evaluate((el) => {
      const r = el.getBoundingClientRect();
      return {
        x: r.x,
        y: r.y,
        right: r.right,
        bottom: r.bottom,
        width: innerWidth,
        height: innerHeight,
        scroll: el.scrollWidth,
        client: el.clientWidth,
        label: el.getAttribute("aria-labelledby"),
        radius: getComputedStyle(el).borderRadius,
      };
    });
    assert.ok(
      box.x >= 0 &&
        box.y >= 0 &&
        box.right <= box.width + 1 &&
        box.bottom <= box.height + 1,
      `${name}: ${JSON.stringify(box)}`,
    );
    assert.ok(
      box.scroll <= box.client + 1,
      `${name} horizontal overflow: ${JSON.stringify(box)}`,
    );
    assert.equal(box.label, "dialog-title");
    assert.equal(box.radius, "0px", `${name} uses a straight frame`);
  };
  for (const [name, selector] of [
    ["server", "command:Edit selected server"],
    ["keys", "#keys"],
    ["appearance", "#appearance"],
    ["backup", "#backup-vault"],
    ["commands", "#commands"],
    ["forget", "#forget-device"],
    ["diagnostics", "#connection-diagnostics"],
    ["groups", "command:Manage terminal groups"],
    ["session-actions", ".tab.active [data-session-menu]"],
  ]) {
    if (selector.startsWith("command:")) {
      await page.locator("#commands").click();
      await page.locator("#command-query").fill(selector.slice(8));
      await page
        .locator("#command-results button")
        .filter({ hasText: selector.slice(8) })
        .click();
    } else await page.locator(selector).click();
    await fits(name);
    await page.screenshot({ path: `.build/popup-${name}.png` });
    if (name === "server") {
      assert.ok(await page.locator("[name=tmuxPath]").isHidden());
      await page.locator("#server-advanced > summary").click();
      assert.ok(await page.locator("[name=tmuxPath]").isVisible());
      await page.locator("#delete-server").click();
      await page.locator(".confirmation-dialog").waitFor();
      await page.screenshot({ path: ".build/popup-delete-confirmation.png" });
      await page.locator(".confirmation-dialog [data-cancel]").click();
      assert.ok(await page.locator("#server-form").isVisible());
    }
    if (name === "session-actions") {
      await page.locator("#decorate-tab").click();
      await fits("tab appearance");
      await page
        .locator("#tab-decoration-form [name=label]")
        .fill("Build machine");
      await page.locator(".emoji-picker > summary").click();
      await page.locator("#emoji-search").fill("wrench");
      await page.locator('#emoji-options [aria-label="Wrench tools"]').click();
      assert.equal(
        await page.locator("#tab-decoration-form [name=emoji]").inputValue(),
        "🔧",
      );
      await page.setViewportSize({ width: 390, height: 650 });
      await fits("emoji picker mobile");
      await page.screenshot({ path: ".build/tab-emoji-picker-mobile.png" });
      await page.setViewportSize(original);
      await page.locator(".emoji-picker > summary").click();
      await page
        .locator("#tab-decoration-form [name=fill]")
        .selectOption("blue");
      await page
        .locator("#tab-decoration-form [name=color]")
        .selectOption("amber");
      await page
        .locator("#tab-decoration-form [name=font]")
        .selectOption("cascadia");
      await page.locator("#tab-decoration-form .primary").click();
      assert.equal(
        await page.locator(".tab.active").getAttribute("data-tab-color"),
        "amber",
      );
      assert.equal(
        await page.locator(".tab.active .tab-name").innerText(),
        "🔧 Build machine",
      );
      assert.equal(
        await page.locator(".tab.active").getAttribute("data-tab-fill"),
        "blue",
      );
      const frame = await page
        .locator(".tab.active")
        .evaluate((el) => getComputedStyle(el).borderTopColor);
      const assertFrame = async () => {
        const colors = await page.locator(".tab.active").evaluate((el) =>
          [el, ...el.querySelectorAll("button")].flatMap((item) => {
            const s = getComputedStyle(item);
            return [
              s.borderTopColor,
              s.borderRightColor,
              s.borderBottomColor,
              s.borderLeftColor,
            ];
          }),
        );
        assert.ok(
          colors.every((color) => color === frame),
          JSON.stringify(colors),
        );
      };
      await assertFrame();
      for (const button of await page.locator(".tab.active button").all()) {
        await button.hover();
        await assertFrame();
        await button.focus();
        await assertFrame();
      }
      const background = await page
        .locator(".tab.active [data-tab]")
        .evaluate((el) => getComputedStyle(el).backgroundColor);
      assert.notEqual(
        background,
        await page
          .locator("#tailscale-login")
          .evaluate((el) => getComputedStyle(el).backgroundColor),
      );
      await page.screenshot({ path: ".build/tab-decoration-applied.png" });
      await page.locator(".tab.active [data-session-menu]").click();
      await page.locator("#decorate-tab").click();
      assert.equal(
        await page.locator("[name=label]").inputValue(),
        "Build machine",
      );
      assert.equal(await page.locator("[name=fill]").inputValue(), "blue");
      await page.locator("[name=label]").fill("");
      await page.locator("[name=fill]").selectOption("default");
      await page.locator("#tab-decoration-form .primary").click();
      assert.match(
        await page.locator(".tab.active .tab-name").innerText(),
        / #\d+$/,
      );
      await page.locator(".tab.active [data-session-menu]").click();
    }
    if (name === "backup") {
      await page
        .locator("summary")
        .filter({ hasText: "Restore a backup" })
        .click();
      assert.ok(await page.locator("#restore-backup").isVisible());
    }
    await page.setViewportSize({ width: 390, height: 650 });
    await fits(name + " mobile");
    await page.screenshot({ path: `.build/popup-${name}-mobile.png` });
    await page.locator("#dialog-close").click();
    await page.setViewportSize(original);
  }
  if (wasFullscreen) await page.locator("#fullscreen").click();
  console.log(
    "Popup review passed: desktop/mobile layouts, shared headers, advanced options and confirmation cancellation.",
  );
}
