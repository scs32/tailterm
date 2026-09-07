import { createServer } from "vite";
import { chromium, webkit } from "@playwright/test";
import assert from "node:assert/strict";
const server = await createServer({
  configFile: false,
  server: { host: "127.0.0.1", port: 0 },
});
await server.listen();
try {
  for (const engine of [chromium, webkit]) {
    const browser = await engine.launch();
    try {
      const page = await browser.newPage({
        viewport: { width: 900, height: 600 },
      });
      await page.route("**/renderer-test", (route) =>
        route.fulfill({
          contentType: "text/html",
          body: '<!doctype html><html><head><meta charset="utf-8"></head><body><div id="host" style="width:600px;height:300px"></div></body></html>',
        }),
      );
      await page.goto(
        `http://127.0.0.1:${server.httpServer.address().port}/renderer-test`,
      );
      const supported = await page.evaluate(async () => {
        await import("/node_modules/@xterm/xterm/css/xterm.css");
        const { Terminal } =
          await import("/node_modules/@xterm/xterm/lib/xterm.mjs");
        const { createRenderer, webglSupported } =
          await import("/client/renderer.js");
        window.enabled = true;
        window.term = new Terminal({ allowProposedApi: false });
        term.open(document.querySelector("#host"));
        term.write("hello webgl\r\n");
        window.renderer = createRenderer(
          term,
          document.querySelector("#host"),
          {
            enabled: () => window.enabled,
          },
        );
        return webglSupported();
      });
      const state = () =>
        page.evaluate(() => ({
          active: renderer.active(),
          canvases: document.querySelectorAll("#host canvas").length,
          text: term.buffer.active.getLine(0)?.translateToString(true),
        }));
      const settle = async () => {
        await page.evaluate(() => document.fonts.ready);
        await page.waitForTimeout(150);
      };
      await settle();
      let s = await state();
      assert.equal(s.text, "hello webgl");
      if (!supported) {
        console.log(`${engine.name()}: WebGL2 unavailable; DOM renderer kept`);
        assert.equal(s.active, false);
        continue;
      }
      assert.equal(s.active, true, "attached while visible");
      assert.ok(s.canvases >= 1, "webgl canvas present");
      // Hidden terminals release their context.
      await page.evaluate(
        () => (document.querySelector("#host").hidden = true),
      );
      await settle();
      s = await state();
      assert.equal(s.active, false, "detached while hidden");
      assert.equal(s.canvases, 0, "no canvas while hidden");
      await page.evaluate(
        () => (document.querySelector("#host").hidden = false),
      );
      await settle();
      s = await state();
      assert.equal(s.active, true, "reattached when shown");
      // Preference off removes it; on restores it.
      await page.evaluate(() => {
        window.enabled = false;
        renderer.update();
      });
      await settle();
      assert.equal((await state()).active, false, "toggle off detaches");
      await page.evaluate(() => {
        window.enabled = true;
        renderer.update();
      });
      await settle();
      assert.equal((await state()).active, true, "toggle on reattaches");
      // A lost context falls back to DOM and then recovers.
      const lost = await page.evaluate(() => {
        const gl = [...document.querySelectorAll("#host canvas")]
          .map((c) => c.getContext("webgl2"))
          .find(Boolean);
        const ext = gl?.getExtension("WEBGL_lose_context");
        if (!ext) return false;
        ext.loseContext();
        return true;
      });
      if (lost) {
        // The addon gives the browser three seconds to restore the context.
        await page.waitForTimeout(3500);
        s = await state();
        assert.equal(s.active, false, "detached after context loss");
        assert.equal(s.text, "hello webgl", "buffer survives loss");
        await page.waitForTimeout(1300);
        assert.equal((await state()).active, true, "reattached after loss");
      }
      await page.evaluate(() => {
        term.write("still fine\r\n");
        renderer.dispose();
      });
      await settle();
      s = await state();
      assert.equal(s.active, false);
      assert.equal(s.canvases, 0);
      console.log(
        `${engine.name()}: renderer ok (context loss ${lost ? "tested" : "skipped"})`,
      );
    } finally {
      await browser.close();
    }
  }
} finally {
  await server.close();
}
