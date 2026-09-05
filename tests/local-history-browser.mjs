import assert from "node:assert/strict";
export async function exerciseLocalHistory(page, getInput) {
  await page.evaluate(
    () => document.fullscreenElement && document.exitFullscreen(),
  );
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
    "Ordinary scroll must never switch modes",
  );
  const before = getInput();
  await toggle.click();
  const output = page.locator(".local-history pre");
  await page.waitForFunction(() =>
    document
      .querySelector(".local-history pre")
      ?.textContent.includes("cached history line 299"),
  );
  assert.equal(await toggle.getAttribute("aria-pressed"), "true");
  assert.equal(
    getInput(),
    before,
    "History fetch must not send terminal input",
  );
  const top = await output.evaluate((el) => el.scrollTop);
  await output.hover();
  await page.mouse.wheel(0, -150);
  await page.waitForFunction(
    (top) => document.querySelector(".local-history pre").scrollTop < top,
    top,
  );
  assert.equal(getInput(), before, "Cached scrolling stays local");
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
}
