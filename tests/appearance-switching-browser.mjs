import assert from "node:assert/strict";
import { randomUUID } from "node:crypto";
import { once } from "node:events";
import { chromium, webkit } from "@playwright/test";
import { createStaticPreviewServer } from "../scripts/preview-static.mjs";
// The suite serves dist-static itself on a port the system picks, and checks a
// per-run token so it never drives a server it did not start.
const token = randomUUID();
const server = createStaticPreviewServer("dist-static", { token });
server.listen(0, "127.0.0.1");
await once(server, "listening");
const origin = `http://127.0.0.1:${server.address().port}`;
let browser;
for (const [signal, code] of [
  ["SIGINT", 130],
  ["SIGTERM", 143],
])
  process.once(signal, async () => {
    setTimeout(() => process.exit(code), 8000).unref();
    await browser?.close().catch(() => {});
    server.close();
    process.exit(code);
  });
try {
  for (const engine of [chromium, webkit]) {
    browser = await engine.launch();
    try {
      const page = await browser.newPage({
        viewport: { width: 1100, height: 800 },
      });
      page.setDefaultTimeout(10000);
      const errors = [];
      page.on("pageerror", (e) => errors.push(e.message));
      const response = await page.goto(`${origin}/`);
      assert.equal(
        response.headers()["x-tailterm-preview-token"],
        token,
        `${engine.name()}: the page came from a server this run did not start`,
      );
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
            // The typeface cards use bundled web fonts with font-display:
            // swap. A card measured in its fallback font is 2 px shorter than
            // once its font arrives, so read only after every font the page
            // asked for has loaded and that layout has been painted.
            const metrics = () =>
              page.locator("#dialog").evaluate(async (d) => {
                do {
                  await document.fonts.ready;
                  for (let frame = 0; frame < 2; frame++)
                    await new Promise(requestAnimationFrame);
                } while (document.fonts.status !== "loaded");
                return {
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
                };
              });
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
  server.closeAllConnections();
  await new Promise((r) => server.close(r));
}
