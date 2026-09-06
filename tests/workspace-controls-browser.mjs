import assert from "node:assert/strict";

export async function exerciseWorkspaceControls(page) {
  const original = page.viewportSize();
  const originalTheme = await page.locator("html").getAttribute("data-theme");
  const fullscreen = await page.evaluate(() => !!document.fullscreenElement);
  if (fullscreen) await page.evaluate(() => document.exitFullscreen());
  const style = (el) => {
    const s = getComputedStyle(el);
    return Object.fromEntries(
      [
        "borderTopWidth",
        "borderTopStyle",
        "borderTopColor",
        "borderRadius",
        "backgroundColor",
        "color",
        "fontFamily",
        "boxShadow",
      ].map((key) => [key, s[key]]),
    );
  };
  const setTheme = async (theme) => {
    await page.setViewportSize(original);
    await page.locator("#appearance").click();
    await page.locator(`[data-theme-choice="${theme}"]`).click();
    await page.locator("#dialog-close").click();
  };
  assert.equal(
    await page.locator(".terminal-footer > div > #commands").count(),
    1,
  );
  assert.equal(await page.locator("#edit-server, #group-tabs").count(), 0);
  let checked = 0;
  for (const theme of ["tokyo", "dawn"]) {
    await setTheme(theme);
    assert.equal(
      await page
        .locator("#filter")
        .evaluate((el) => getComputedStyle(el).borderRadius),
      "0px",
    );

    for (const mobile of [false, true]) {
      await page.setViewportSize(
        mobile ? { width: 390, height: 650 } : original,
      );
      const plus = await page.locator("#new-tab").boundingBox();
      const tab = await page.locator("#tabs .tab").first().boundingBox();
      assert.ok(
        Math.abs(plus.y - tab.y) < 1 && Math.abs(plus.height - tab.height) < 1,
        "Plus matches the tab frame",
      );
      await page.locator("#tailscale-login").hover();
      const reference = await page.locator("#tailscale-login").evaluate(style);
      assert.equal(reference.borderTopWidth, "1px");
      assert.equal(reference.borderTopStyle, "solid");
      assert.equal(reference.borderRadius, "0px");
      await page.locator("#new-tab").hover();
      const ordinary = await page.locator("#new-tab").evaluate(style);
      const controls = page.locator(
        "#workspace button:visible:not(.tab:not(.active) > [data-close]):not(.tab:not(.active) > [data-session-menu])",
      );
      const count = await controls.count();
      for (let i = 0; i < count; i++) {
        const button = controls.nth(i);
        const label = await button.evaluate(
          (el) => el.id || el.getAttribute("aria-label") || el.textContent,
        );
        if (await button.isDisabled()) {
          const before = await button.evaluate(style);
          await button.hover({ force: true });
          assert.deepEqual(
            await button.evaluate(style),
            before,
            `${theme} disabled ${label}`,
          );
          continue;
        }
        const expected = (await button.evaluate((el) =>
          el.matches(
            '#tailscale-login:has(.online), .server-item.selected, #all-servers[aria-pressed="true"]',
          ),
        ))
          ? reference
          : ordinary;
        await button.hover();
        assert.deepEqual(
          await button.evaluate(style),
          expected,
          `${theme} ${mobile ? "mobile" : "desktop"} hover ${label}`,
        );
        await page.keyboard.press("Tab");
        await button.focus();
        assert.deepEqual(
          await button.evaluate(style),
          expected,
          `${theme} keyboard focus ${label}`,
        );
        await button.evaluate((el) => el.blur());
        checked++;
      }
      const activeId = await page
        .locator(".tab.active [data-tab]")
        .getAttribute("data-tab");
      await page.locator("#new-tab").click();
      const launcher = page.locator("#empty-terminal button:visible");
      for (let i = 0; i < (await launcher.count()); i++) {
        const button = launcher.nth(i);
        if (await button.isDisabled()) continue;
        await button.hover();
        const actual = await button.evaluate(style);
        if (await button.evaluate((el) => el.classList.contains("primary"))) {
          assert.equal(actual.borderTopWidth, "1px");
          assert.equal(actual.borderRadius, "0px");
        } else
          assert.deepEqual(
            actual,
            ordinary,
            `launcher ${await button.innerText()}`,
          );
        checked++;
      }
      await page.locator(`[data-tab="${activeId}"]`).click();
      for (const [name, selector] of [
        ["upper-right", "#tailscale-login"],
        ["bottom-commands", "#commands"],
        ["lower-nav", "#keys"],
        ["lower-right", "#clear"],
        ["tabs", ".tab.active [data-tab]"],
        ["mobile-keys", ".mobile-terminal-keys button"],
      ]) {
        const control = page.locator(selector).first();
        if (await control.isVisible()) {
          await control.hover();
          await page.screenshot({
            path: `.build/controls-${theme}-${mobile ? "mobile" : "desktop"}-${name}.png`,
          });
        }
      }
    }
  }
  await setTheme(originalTheme);
  await page.mouse.move(0, 0);
  await page.keyboard.press("Escape");
  if (fullscreen) {
    await page.locator("#fullscreen").click();
    await page.waitForFunction(() => !!document.fullscreenElement);
    await page.waitForTimeout(250);
  }
  console.log(
    `Workspace controls: ${checked} actual hover/focus states match Tailscale; dark/light, desktop/mobile, disabled controls checked.`,
  );
}
