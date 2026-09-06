import assert from "node:assert/strict";
import { finishRestoration } from "./restore-browser.mjs";

export async function exerciseServerFilters(page) {
  await page.locator("#all-servers").click();
  const production = page
    .locator(".server-item")
    .filter({ hasText: "Production" });
  const development = page
    .locator(".server-item")
    .filter({ hasText: "Development" });
  const original = await page
    .locator("#tabs .tab")
    .filter({ hasText: "Production" })
    .first()
    .locator("[data-tab]")
    .getAttribute("data-tab");
  await page.locator("#new-tab").click();
  await page
    .locator("[data-launch-server]")
    .filter({ hasText: "Development" })
    .click();
  await page.locator("#launcher-shell").click();
  const deadline = Date.now() + 30000;
  while (
    !(await page.locator("#terminal-status").innerText()).includes("Connected")
  ) {
    assert.ok(Date.now() < deadline, "Development SSH connects");
    const trust = page.getByRole("button", { name: "Trust & continue" });
    if (await trust.isVisible()) await trust.click();
    const method = page.locator(".login-prompt [name=method]");
    if (await method.isVisible()) await method.selectOption("password");
    const password = page.locator(".login-prompt [name=password]");
    if (await password.isVisible()) {
      await password.fill("static-ssh-password");
      await page.locator(".login-prompt button[type=submit]").click();
    }
    await page.waitForTimeout(100);
  }
  const dev = await page
    .locator(".tab.active [data-tab]")
    .getAttribute("data-tab");
  const tab = (id) => page.locator(`#tabs .tab:has([data-tab="${id}"])`);
  await tab(dev).dragTo(tab(original));
  const paneCount = await page.locator(".pane-header").count();
  assert.ok(paneCount >= 2, "Mixed-server group created");
  const order = await page
    .locator("#tabs [data-tab]")
    .evaluateAll((nodes) => nodes.map((n) => n.dataset.tab));
  await page.evaluate(() => {
    globalThis.__filterTerminals = [
      ...document.querySelectorAll(".terminal-instance"),
    ];
  });
  await production.click();
  assert.equal(await production.getAttribute("aria-pressed"), "true");
  assert.equal(await development.getAttribute("aria-pressed"), "false");
  assert.doesNotMatch(await page.locator("#tabs").innerText(), /Development/);
  assert.equal(
    await page.locator(".terminal-instance:not([hidden])").count(),
    paneCount - 1,
  );
  const focused = await page
    .locator(".tab.active [data-tab]")
    .getAttribute("data-tab");
  await development.click();
  assert.equal(
    await page.locator(".tab.active [data-tab]").getAttribute("data-tab"),
    focused,
    "Adding a filter preserves active session",
  );
  assert.equal(
    await page.locator(".terminal-instance:not([hidden])").count(),
    paneCount,
  );
  await production.click();
  assert.equal(
    await page.locator(".tab.active [data-tab]").getAttribute("data-tab"),
    dev,
  );
  assert.equal(
    await page.locator(".terminal-instance:not([hidden])").count(),
    1,
  );
  await development.click();
  assert.equal(await page.locator("#tabs .tab").count(), 0);
  assert.equal(
    await page.locator(".terminal-instance:not([hidden])").count(),
    0,
  );
  assert.match(
    await page.locator("#server-filter-empty").innerText(),
    /No servers selected/,
  );
  await page.locator("#all-servers").click();
  await page.locator(`.pane-header[data-pane="${dev}"] .pane-label`).click();
  assert.equal(await page.locator(".pane-header").count(), paneCount);
  assert.deepEqual(
    await page
      .locator("#tabs [data-tab]")
      .evaluateAll((nodes) => nodes.map((n) => n.dataset.tab)),
    order,
  );
  assert.ok(
    await page.evaluate(() => {
      const nodes = [...document.querySelectorAll(".terminal-instance")];
      return (
        nodes.length === __filterTerminals.length &&
        nodes.every((n, i) => n === __filterTerminals[i])
      );
    }),
    "Filtering retains the existing terminal instances",
  );
  assert.match(await page.locator("#terminal-status").innerText(), /Connected/);
  await development.click();
  await page.waitForTimeout(700);
  await page.reload();
  await page.locator("#password").fill("static browser vault passphrase");
  await page.locator("#unlock-button").click();
  await finishRestoration(page);
  assert.equal(await development.getAttribute("aria-pressed"), "true");
  assert.equal(await production.getAttribute("aria-pressed"), "false");
  assert.equal(await page.locator("#tabs .tab").count(), 1);
  await page.locator("#all-servers").click();
  await page.locator(`.pane-header[data-pane="${dev}"] .pane-label`).click();
  assert.equal(
    await page.locator(".pane-header").count(),
    paneCount,
    "Refresh restores hidden members of the group",
  );
  await page.screenshot({ path: "/tmp/tailterm-server-filters.png" });
  console.log(
    "Server filters passed: union, empty selection, focus, mixed groups, terminal identity, refresh persistence.",
  );
}
