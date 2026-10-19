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
      // Hold the Nerd Font responses until the dialog has been laid out, as
      // on a slow connection, so its first layout uses the fallback font.
      let releaseNerdFonts;
      const nerdFontsHeld = new Promise((r) => (releaseNerdFonts = r));
      await page.route(/-nerd-[^/]*\.woff2$/, async (route) => {
        await nerdFontsHeld;
        await route.continue();
      });
      // Waits, at most 5 s, until every font the page asked for has loaded
      // and that layout has been painted, and fails naming any face still
      // pending.
      const settleFonts = async (label) => {
        const pending = await page.evaluate(async () => {
          const deadline = performance.now() + 5000;
          do {
            await Promise.race([
              document.fonts.ready,
              new Promise((r) => setTimeout(r, deadline - performance.now())),
            ]);
            for (let frame = 0; frame < 2; frame++)
              await new Promise(requestAnimationFrame);
          } while (
            document.fonts.status !== "loaded" &&
            performance.now() < deadline
          );
          if (document.fonts.status === "loaded") return [];
          const faces = [...document.fonts]
            .filter((f) => f.status !== "loaded" && f.status !== "unloaded")
            .map((f) => `${f.family} ${f.weight} ${f.status}`);
          return faces.length ? faces : ["no face reports its status"];
        });
        assert.deepEqual(
          pending,
          [],
          `${engine.name()} ${label}: fonts still loading after 5 s: ${pending.join("; ")}`,
        );
      };
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
      // The typeface samples must not change the dialog's size when their
      // Nerd Font faces arrive after it opens.
      const size = () =>
        page.locator("#dialog").evaluate(async (d) => {
          for (let frame = 0; frame < 2; frame++)
            await new Promise(requestAnimationFrame);
          return {
            fonts: document.fonts.status,
            height: d.scrollHeight,
            card: d.querySelector(".font-grid").getBoundingClientRect().height,
          };
        });
      const early = await size();
      assert.equal(
        early.fonts,
        "loading",
        `${engine.name()}: the Nerd Font faces were held while the dialog opened`,
      );
      releaseNerdFonts();
      await settleFonts("after the held Nerd Font faces arrive");
      assert.deepEqual(
        await size(),
        { ...early, fonts: "loaded" },
        `${engine.name()}: the appearance dialog keeps its size when the Nerd Font faces arrive after it opens`,
      );
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
            // Choosing a font can start loading its faces, so read only after
            // they have settled.
            const metrics = async () => {
              await settleFonts(`${width} ${attr} ${id}`);
              return page.locator("#dialog").evaluate((d) => ({
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
            };
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
