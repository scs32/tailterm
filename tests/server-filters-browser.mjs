import assert from "node:assert/strict";
import { finishRestoration } from "./restore-browser.mjs";
import { homeShellsToTabs } from "./pane-groups-browser.mjs";

// What decides which terminals show, for the message of a failed count: the
// pressed filters, the tabs, and each visible terminal with the pane over it.
const filterState = (page) =>
  page.evaluate(() => {
    const text = (n) => n?.textContent.replace(/\s+/g, " ").trim() ?? "";
    const headers = [...document.querySelectorAll(".pane-header")];
    const paneOver = (terminal) => {
      const box = terminal.getBoundingClientRect();
      const header = headers.find((h) => {
        const b = h.getBoundingClientRect();
        return (
          Math.abs(b.left - box.left) < 8 && Math.abs(b.bottom - box.top) < 8
        );
      });
      return header
        ? `pane ${header.dataset.pane} "${text(header.querySelector(".pane-label"))}"`
        : "no pane header over it";
    };
    return {
      filters: [...document.querySelectorAll("#all-servers, .server-item")]
        .filter((n) => n.getAttribute("aria-pressed") === "true")
        .map((n) => text(n.querySelector("strong") || n)),
      tabs: [...document.querySelectorAll("#tabs .tab")].map(
        (n) =>
          `${n.querySelector("[data-tab]")?.dataset.tab}${n.classList.contains("active") ? " (active)" : ""} "${text(n.querySelector("[data-tab]"))}"`,
      ),
      visible: [...document.querySelectorAll(".terminal-instance")].flatMap(
        (n, i) =>
          n.hidden
            ? []
            : [
                `terminal ${i}${n.classList.contains("focused-pane") ? " (focused)" : ""}: ${paneOver(n)}`,
              ],
      ),
      status: text(document.querySelector("#terminal-status")),
    };
  });
// A filter click decides which terminals show. Wait for the count instead of
// reading it once, and say what is showing when it never arrives.
async function visibleTerminals(page, expected, timeout = 10000) {
  const count = () => page.locator(".terminal-instance:not([hidden])").count();
  const deadline = Date.now() + timeout;
  while ((await count()) !== expected && Date.now() < deadline)
    await page.waitForTimeout(50);
  const seen = await count();
  assert.equal(
    seen,
    expected,
    `${seen} visible terminal instances, expected ${expected}, after ${timeout} ms: ${JSON.stringify(await filterState(page))}`,
  );
}

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
  // The launcher redraws its server cards when a background session check
  // ends, and a click split by that redraw reaches no card. Click until
  // Development is the chosen card, or the shell opens on Production.
  const card = page
    .locator("[data-launch-server]")
    .filter({ hasText: "Development" });
  const chosen = Date.now() + 10000;
  for (let tries = 1; ; tries++) {
    await card.click();
    if ((await card.getAttribute("aria-pressed")) === "true") break;
    assert.ok(
      tries < 5 && Date.now() < chosen,
      `Development is chosen in the launcher after ${tries} clicks: ${JSON.stringify(
        await page.evaluate(() => ({
          cards: [...document.querySelectorAll("[data-launch-server]")].map(
            (n) =>
              `${n.querySelector("strong")?.textContent ?? n.dataset.launchServer}${n.getAttribute("aria-pressed") === "true" ? " (chosen)" : ""}`,
          ),
          note: document.querySelector("#launcher-note")?.textContent,
        })),
      )}`,
    );
  }
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
  // TailOS opens the shell in Home; give Home shells tabs for these checks.
  await homeShellsToTabs(page);
  const dev = await page
    .locator(".tab.active [data-tab]")
    .getAttribute("data-tab");
  const tab = (id) => page.locator(`#tabs .tab:has([data-tab="${id}"])`);
  assert.match(
    await tab(dev).innerText(),
    /Development/,
    "The new shell is a Development tab",
  );
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
  await visibleTerminals(page, paneCount - 1);
  const focused = await page
    .locator(".tab.active [data-tab]")
    .getAttribute("data-tab");
  await development.click();
  assert.equal(
    await page.locator(".tab.active [data-tab]").getAttribute("data-tab"),
    focused,
    "Adding a filter preserves active session",
  );
  await visibleTerminals(page, paneCount);
  await production.click();
  assert.equal(
    await page.locator(".tab.active [data-tab]").getAttribute("data-tab"),
    dev,
  );
  await visibleTerminals(page, 1);
  await development.click();
  assert.equal(await page.locator("#tabs .tab").count(), 0);
  await visibleTerminals(page, 0);
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
  await page.screenshot({ path: `.build/server-filters-${process.argv[1].split('/').at(-1).replace(/\.mjs$/, '')}-${process.env.TEST_BROWSER || 'chromium'}.png` });
  console.log(
    "Server filters passed: union, empty selection, focus, mixed groups, terminal identity, refresh persistence.",
  );
}
