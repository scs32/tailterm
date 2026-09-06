import assert from "node:assert/strict";
export async function exerciseLocalHistory(page, getInput) {
  await page.evaluate(
    () => document.fullscreenElement && document.exitFullscreen(),
  );
  await page.locator("#appearance").click();
  await page.locator('[data-preference="autoHistory"]').uncheck();
  await page.locator("#dialog-close").click();
  const terminal = page.locator(".terminal-instance:not([hidden])").first();
  const toggle = terminal.locator(".power-scroll-toggle");
  await toggle.waitFor();
  assert.equal(await toggle.getAttribute("aria-pressed"), "false");
  await terminal.dispatchEvent("wheel", {
    deltaY: -120,
    bubbles: true,
    cancelable: true,
  });
  assert.equal(
    await page.locator(".local-history").count(),
    0,
    "Disabling automatic scrolling preserves manual mode",
  );
  const before = getInput();
  await toggle.click();
  const output = page.locator(".local-history .xterm-rows");
  await page.waitForFunction(() =>
    document
      .querySelector(".local-history .xterm-rows")
      ?.textContent.includes("cached history line 299"),
  );
  assert.equal(await toggle.getAttribute("aria-pressed"), "true");
  assert.ok(
    (await page.locator(".local-history .xterm-rows > div").count()) < 100,
    "Only visible history rows are rendered",
  );
  assert.ok(
    await page
      .locator(".local-history .xterm-rows span")
      .evaluateAll((nodes) =>
        nodes.some(
          (el) =>
            el.textContent.includes("cached history") &&
            getComputedStyle(el).fontWeight === "700",
        ),
      ),
    "Snapshot retains bold formatting",
  );
  assert.ok(
    await page
      .locator(".local-history .xterm-rows span")
      .evaluateAll((nodes) =>
        nodes.some(
          (el) =>
            el.textContent.includes("cached history") &&
            getComputedStyle(el).color !==
              getComputedStyle(el.closest(".local-history")).color,
        ),
      ),
    "Snapshot retains color",
  );
  assert.equal(
    getInput(),
    before,
    "History fetch must not send terminal input",
  );
  const beforeScroll = await output.textContent();
  await page.locator(".history-terminal").hover();
  await page.mouse.wheel(0, -150);
  await page.waitForFunction(
    (text) =>
      document.querySelector(".local-history .xterm-rows").textContent !== text,
    beforeScroll,
  );
  assert.equal(getInput(), before, "Cached scrolling stays local");
  await page.locator("#search-toggle").click();
  await page.locator("#terminal-search").fill("cached history line 42");
  await page.locator("#find-next").click();
  await page.waitForFunction(() =>
    document
      .querySelector(".local-history .xterm-rows")
      ?.textContent.includes("cached history line 42"),
  );
  await page.locator("#search-close").click();
  await page.locator(".local-history .xterm-helper-textarea").focus();
  await page.keyboard.type("do not send this");
  await page.keyboard.press("Enter");
  assert.equal(getInput(), before, "History typing never reaches SSH");
  await page.screenshot({ path: "/tmp/tailterm-power-scroll.png" });
  await toggle.click();
  assert.equal(await page.locator(".local-history").count(), 0);
  assert.equal(await toggle.getAttribute("aria-pressed"), "false");
  // Turning off while a fetch is in flight must not resurrect the snapshot.
  await toggle.evaluate((button) => {
    button.click();
    button.click();
  });
  await page.waitForTimeout(700);
  assert.equal(await page.locator(".local-history").count(), 0);
  assert.equal(await toggle.getAttribute("aria-pressed"), "false");
  await page.locator("#appearance").click();
  await page.locator('[data-preference="autoHistory"]').check();
  await page.locator("#dialog-close").click();
  const inputBeforeAuto = getInput();
  await terminal.dispatchEvent("wheel", {
    deltaY: -120,
    bubbles: true,
    cancelable: true,
  });
  await page.waitForFunction(() =>
    document.querySelector(".local-history:not(.history-loading)"),
  );
  const screen = page.locator(".local-history .xterm-screen");
  await screen.dispatchEvent("wheel", {
    deltaY: 2000,
    bubbles: true,
    cancelable: true,
  });
  await page.waitForFunction(() =>
    document
      .querySelector(".local-history .xterm-rows")
      ?.textContent.includes("cached history line 299"),
  );
  await screen.dispatchEvent("wheel", {
    deltaY: 120,
    bubbles: true,
    cancelable: true,
  });
  assert.equal(await page.locator(".local-history").count(), 0);
  await terminal.dispatchEvent("wheel", {
    deltaY: -1,
    bubbles: true,
    cancelable: true,
  });
  assert.equal(
    await page.locator(".local-history").count(),
    0,
    "Trailing momentum cannot reopen history",
  );
  assert.equal(
    getInput(),
    inputBeforeAuto,
    "Automatic handoff never sends wheel input to SSH",
  );
}
