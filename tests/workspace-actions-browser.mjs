import assert from "node:assert/strict";
export async function exerciseWorkspaceActions(page, stream) {
  await page.evaluate(
    () => document.fullscreenElement && document.exitFullscreen(),
  );
  await page.waitForFunction(() => !document.fullscreenElement);
  const original = await page
    .locator(".tab.active [data-tab]")
    .getAttribute("data-tab");
  await page.keyboard.press("Control+Shift+P");
  await page.locator("#command-query").fill("Switch:");
  const choices = page.locator("#command-results button");
  assert.ok((await choices.count()) >= 2);
  // Choose a different session through the palette.
  const originalText = await page.locator(".tab.active").innerText();
  await choices.first().click();
  const current = await page
    .locator(".tab.active [data-tab]")
    .getAttribute("data-tab");
  if (current === original) {
    await page.locator("#commands").click();
    await page.locator("#command-query").fill("Switch:");
    await page.locator("#command-results button").nth(1).click();
  }
  stream.write("\x1b[?25l\x1b[?25h");
  await page.waitForTimeout(700);
  assert.equal(
    await page.locator(`[data-tab="${original}"] .tab-activity`).textContent(),
    "",
  );
  stream.write("\x07");
  await page.waitForFunction(
    (id) =>
      document
        .querySelector(`[data-tab="${id}"] .tab-activity`)
        ?.textContent.includes("Bell"),
    original,
  );
  assert.match(await page.title(), /Bell.*Tailterm/);
  stream.write("\x1b]133;D;0\x07");
  await page.waitForFunction(
    (id) =>
      document
        .querySelector(`[data-tab="${id}"] .tab-activity`)
        ?.textContent.includes("Command finished"),
    original,
  );
  await page.locator(`[data-tab="${original}"]`).click();
  assert.equal(
    await page.locator(`[data-tab="${original}"] .tab-activity`).textContent(),
    "",
  );
  await page.setViewportSize({ width: 390, height: 600 });
  await page
    .locator(".mobile-terminal-keys")
    .getByRole("button", { name: "Sessions", exact: true })
    .click();
  assert.ok((await page.locator("#mobile-sessions button").count()) >= 2);
  await page.locator("#mobile-sessions button").last().click();
  await page
    .locator(".mobile-terminal-keys")
    .getByRole("button", { name: "Select text", exact: true })
    .click();
  await page.locator("#mobile-output").waitFor();
  await page.locator("#dialog-close").click();
  const bounds = await page.locator(".terminal-shell").boundingBox();
  assert.ok(bounds.y + bounds.height <= 601, JSON.stringify(bounds));
  assert.ok(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= innerWidth,
    ),
  );
  await page.screenshot({ path: "/tmp/tailterm-mobile-controls.png" });
  await page.setViewportSize({ width: 1440, height: 1100 });
  await page.locator(`[data-tab="${original}"]`).click();
  assert.match(await page.locator(".tab.active").innerText(), /Connected/);
  await page.locator("#commands").click();
  await page.locator("#command-query").fill("Forget this device");
  await page.locator("#command-results button").click();
  assert.ok(await page.locator("#forget-submit").isDisabled());
  await page.locator("#forget-confirm").fill("forget");
  assert.ok(await page.locator("#forget-submit").isDisabled());
  await page.locator("#dialog-close").click();
}
export async function exerciseForgetDevice(page) {
  await page.locator("#forget-device").click();
  await page.locator("#forget-confirm").fill("FORGET");
  await page.locator("#forget-submit").click();
  await page.getByRole("button", { name: "Create encrypted vault" }).waitFor();
  assert.equal(
    await page.evaluate(() => localStorage.getItem("tailserve.appearance")),
    null,
  );
  const record = await page.evaluate(
    () =>
      new Promise((resolve) => {
        const request = indexedDB.open("tailserve", 1);
        request.onsuccess = () => {
          const get = request.result
            .transaction("vault")
            .objectStore("vault")
            .get("encrypted");
          get.onsuccess = () => {
            request.result.close();
            resolve(get.result);
          };
        };
      }),
  );
  assert.equal(record, undefined);
}
