import assert from "node:assert/strict";
import { finishRestoration } from "./restore-browser.mjs";

export async function exerciseWorkspaceContinuity(page, context) {
  const activeId = await page
    .locator(".tab.active [data-tab]")
    .getAttribute("data-tab");
  await page.evaluate(() => {
    globalThis.__testTerminal = document.querySelector(
      ".terminal-instance:not([hidden])",
    );
  });
  await context.setOffline(true);
  await page.waitForFunction(() =>
    document.querySelector("#terminal-status").textContent.includes("Error"),
  );
  await context.setOffline(false);
  await page.waitForFunction(() =>
    document
      .querySelector("#terminal-status")
      .textContent.includes("Connected"),
  );
  assert.ok(
    await page.evaluate(
      () =>
        globalThis.__testTerminal ===
        document.querySelector(".terminal-instance:not([hidden])"),
    ),
  );
  assert.match(
    await page.locator(".terminal-instance:not([hidden])").innerText(),
    /after-rename-probe/,
  );
  const ids = await page
    .locator("#tabs [data-tab]")
    .evaluateAll((nodes) => nodes.map((n) => n.dataset.tab));
  assert.ok(ids.length >= 2);
  const index = ids.indexOf(activeId),
    neighbor = ids[index > 0 ? index - 1 : 1];
  await page
    .locator(`.tab:has([data-tab="${activeId}"])`)
    .dragTo(page.locator(`.tab:has([data-tab="${neighbor}"])`));
  await page
    .locator(`.pane-header[data-pane="${activeId}"] .pane-label`)
    .click();
  const order = await page
    .locator("#tabs [data-tab]")
    .evaluateAll((nodes) => nodes.map((n) => n.dataset.tab));
  const paneCount = await page.locator(".pane-header").count();
  await page.waitForTimeout(700);
  await page.reload();
  await page.locator("#password").fill("static browser vault passphrase");
  await page.locator("#unlock-button").click();
  await finishRestoration(page);
  assert.deepEqual(
    await page
      .locator("#tabs [data-tab]")
      .evaluateAll((nodes) => nodes.map((n) => n.dataset.tab)),
    order,
  );
  assert.equal(await page.locator(".pane-header").count(), paneCount);
  assert.equal(await page.locator(".focused-pane").count(), 1);
  assert.match(
    await page.locator(".tab.active").innerText(),
    /renamed-static-launcher/,
  );
  await page.screenshot({ path: "/tmp/tailterm-restored-workspace.png" });
}
