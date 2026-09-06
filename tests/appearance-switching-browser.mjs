import { preview } from "vite";
import assert from "node:assert/strict";
import { chromium, webkit } from "@playwright/test";
const server = await preview({
  build: { outDir: "dist-static" },
  preview: { host: "127.0.0.1", port: 4319 },
});
try {
  for (const engine of [chromium, webkit]) {
    const browser = await engine.launch();
    try {
      const page = await browser.newPage({
        viewport: { width: 1100, height: 800 },
      });
      page.setDefaultTimeout(10000);
      const errors = [];
      page.on("pageerror", (e) => errors.push(e.message));
      await page.goto(`http://127.0.0.1:${server.httpServer.address().port}/`);
      await page
        .locator("#password")
        .fill("temporary appearance test password");
      await page.locator("#unlock-button").click();
      await page.locator("#appearance").click();
      for (const width of [1100, 390]) {
        await page.setViewportSize({ width, height: 800 });
        for (const attr of ["theme", "font"])
          for (const id of await page
            .locator(`[data-${attr}-choice]`)
            .evaluateAll(
              (ns, attr) =>
                ns.map((n) => n.getAttribute(`data-${attr}-choice`)),
              attr,
            )) {
            const button = page.locator(`[data-${attr}-choice="${id}"]`);
            await button.scrollIntoViewIfNeeded();
            const metrics = () =>
              page
                .locator("#dialog")
                .evaluate((d) => ({
                  scroll: d.scrollTop,
                  width: d.clientWidth,
                  scrollWidth: d.scrollWidth,
                  height: d.scrollHeight,
                  head: d.querySelector(".dialog-head").getBoundingClientRect()
                    .top,
                  preview: d
                    .querySelector("#appearance-preview")
                    .getBoundingClientRect().height,
                  card: d.querySelector(".font-grid").getBoundingClientRect()
                    .height,
                }));
            const before = await metrics();
            await button.click();
            await page.waitForTimeout(200);
            assert.deepEqual(
              await metrics(),
              before,
              `${engine.name()} ${width} ${attr} ${id}: dialog geometry and scroll position remain stable`,
            );
          }
      }
      assert.deepEqual(errors, []);
      console.log(
        `${engine.name()}: theme/font switching preserves dialog layout on desktop and mobile.`,
      );
    } finally {
      await browser.close();
    }
  }
} finally {
  await new Promise((r) => server.httpServer.close(r));
}
