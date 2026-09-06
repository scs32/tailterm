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
        viewport: { width: 1200, height: 850 },
      });
      const errors = [];
      page.on("pageerror", (e) => errors.push(e.message));
      await page.route("**/history-test", (r) =>
        r.fulfill({
          contentType: "text/html",
          body: '<div id="workspace" style="display:block"><div class="terminal-instance" style="position:relative;width:1000px;height:650px"></div></div>',
        }),
      );
      await page.goto(
        `http://127.0.0.1:${server.httpServer.address().port}/history-test`,
      );
      const loadMs = await page.evaluate(async () => {
        await import("/node_modules/@xterm/xterm/css/xterm.css");
        await import("/client/style.css");
        const { Terminal } =
          await import("/node_modules/@xterm/xterm/lib/xterm.mjs");
        const { setupLocalHistory } = await import("/client/local-history.js");
        const { normalizeAppearance, terminalAppearance, applyChrome } =
          await import("/client/appearance.js");
        const p = normalizeAppearance();
        applyChrome(p);
        window.tab = {
          term: new Terminal(terminalAppearance(p)),
          el: document.querySelector(".terminal-instance"),
          tmux: true,
          target: { id: "$1", created: 1 },
          status: "Connected",
        };
        tab.term.open(tab.el);
        window.captures = 0;
        window.remoteInput = "";
        tab.term.onData((d) => (remoteInput += d));
        tab.history = setupLocalHistory(tab, {
          capture: async () => {
            captures++;
            return Array.from(
              { length: 5000 },
              (_, i) =>
                `\x1b[1;38;2;255;170;80mhistory row ${i} \x1b[0m\x1b[38;5;81mhttps://example.com/log/${i}\x1b[0m`,
            ).join("\n");
          },
        });
        const start = performance.now();
        await tab.history.open();
        await new Promise(requestAnimationFrame);
        return performance.now() - start;
      });
      await page.waitForFunction(() =>
        document
          .querySelector(".local-history .xterm-rows")
          ?.textContent.includes("history row 4999"),
      );
      const rows = await page
        .locator(".local-history .xterm-rows > div")
        .count();
      assert.ok(rows < 100);
      const frames = await page.evaluate(async () => {
        const initial = document.querySelector(
          ".local-history .xterm-rows",
        ).textContent;
        let changed = false;
        const v = document.querySelector(
            ".local-history .xterm-scrollable-element",
          ),
          times = [];
        for (let i = 0; i < 30; i++) {
          const start = performance.now();
          v.dispatchEvent(
            new WheelEvent("wheel", {
              deltaY: i % 2 ? 500 : -500,
              bubbles: true,
              cancelable: true,
            }),
          );
          await new Promise(requestAnimationFrame);
          await new Promise(requestAnimationFrame);
          changed ||=
            document.querySelector(".local-history .xterm-rows").textContent !==
            initial;
          times.push(performance.now() - start);
        }
        if (!changed)
          throw new Error("Wheel input did not change rendered history");
        return times.sort((a, b) => a - b);
      });
      assert.equal(await page.evaluate(() => captures), 1);
      await page.evaluate(() => tab.history.findNext("history row 42 "));
      assert.equal(
        await page.evaluate(() => tab.history.getSelection()),
        "history row 42 ",
      );
      await page.locator(".local-history .xterm-helper-textarea").focus();
      await page.keyboard.type("never send");
      await page.keyboard.press("Enter");
      assert.equal(await page.evaluate(() => remoteInput), "");
      await page.screenshot({
        path: `.build/colored-history-${engine.name()}.png`,
      });
      await page.evaluate(() => {
        tab.term.options.fontSize = 18;
        tab.history.update();
      });
      await page.setViewportSize({ width: 390, height: 650 });
      await page.evaluate(() => {
        tab.el.style.width = "360px";
        tab.el.style.height = "550px";
      });
      await page.waitForTimeout(150);
      await page.screenshot({
        path: `.build/colored-history-mobile-${engine.name()}.png`,
      });
      assert.ok(
        await page.locator(".history-terminal").evaluate((e) => {
          const box = e.getBoundingClientRect(),
            screen = e.querySelector(".xterm-screen").getBoundingClientRect();
          return screen.right <= box.right && screen.bottom <= box.bottom;
        }),
      );
      await page.evaluate(() => tab.history.close());
      assert.equal(await page.locator(".local-history").count(), 0);
      assert.deepEqual(errors, []);
      console.log(
        `${engine.name()}: 5,000 colored lines, ${rows} rendered rows; local load ${Math.round(loadMs)}ms, two-frame scroll median ${Math.round(frames[15])}ms / p95 ${Math.round(frames[28])}ms; one capture, no remote input, search/copy/resize passed.`,
      );
    } finally {
      await browser.close();
    }
  }
} finally {
  await server.close();
}
