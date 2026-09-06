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
        window.autoHistory = false;
        window.captures = 0;
        window.remoteInput = "";
        tab.term.onData((d) => (remoteInput += d));
        tab.history = setupLocalHistory(tab, {
          automatic: () => autoHistory,
          capture: async () => {
            captures++;
            if (window.pauseCapture)
              await new Promise((resolve) => (window.finishCapture = resolve));
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
      await page.waitForTimeout(150);
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
      await page.evaluate(() => (autoHistory = true));
      await page.waitForTimeout(220);
      await page.locator(".terminal-instance").dispatchEvent("wheel", {
        deltaY: -120,
        bubbles: true,
        cancelable: true,
      });
      await page.waitForFunction(() =>
        document.querySelector(".local-history:not(.history-loading)"),
      );
      assert.equal(await page.evaluate(() => captures), 2);
      await page
        .locator(".local-history .xterm-screen")
        .dispatchEvent("wheel", {
          deltaY: 2000,
          bubbles: true,
          cancelable: true,
        });
      await page.waitForTimeout(200);
      await page.waitForFunction(() =>
        document
          .querySelector(".local-history .xterm-rows")
          ?.textContent.includes("history row 4999"),
      );
      await page
        .locator(".local-history .xterm-screen")
        .dispatchEvent("wheel", {
          deltaY: 120,
          bubbles: true,
          cancelable: true,
        });
      assert.equal(await page.locator(".local-history").count(), 0);
      await page.locator(".terminal-instance").dispatchEvent("wheel", {
        deltaY: -120,
        bubbles: true,
        cancelable: true,
      });
      assert.equal(await page.locator(".local-history").count(), 0);
      assert.equal(await page.evaluate(() => captures), 2);
      await page.waitForTimeout(220);
      await page.locator(".terminal-instance").dispatchEvent("wheel", {
        deltaY: -120,
        shiftKey: true,
        bubbles: true,
        cancelable: true,
      });
      assert.equal(await page.evaluate(() => captures), 2);
      await page.evaluate(() => (window.pauseCapture = true));
      await page.locator(".terminal-instance").dispatchEvent("wheel", {
        deltaY: -120,
        bubbles: true,
        cancelable: true,
      });
      await page.locator(".terminal-instance").dispatchEvent("wheel", {
        deltaY: 600,
        bubbles: true,
        cancelable: true,
      });
      assert.equal(await page.locator(".local-history").count(), 0);
      await page.evaluate(() => window.finishCapture());
      await page.waitForTimeout(100);
      assert.equal(
        await page.locator(".local-history").count(),
        0,
        "Cancelled automatic capture stays closed",
      );
      // A tiny first trackpad tick must not be rounded into a full-row jump.
      await page.evaluate(() => {
        window.pauseCapture = false;
        autoHistory = true;
      });
      await page.waitForTimeout(220);
      await page.evaluate(() => tab.history.open());
      const bottomText = await page
        .locator(".local-history .xterm-rows")
        .textContent();
      await page.evaluate(() => tab.history.close());
      await page.waitForTimeout(220);
      const rowHeight = await page.evaluate(
        () => tab.term.options.fontSize * tab.term.options.lineHeight,
      );
      await page.locator(".terminal-instance").dispatchEvent("wheel", {
        deltaY: -rowHeight / 4,
        bubbles: true,
        cancelable: true,
      });
      await page.waitForFunction(() =>
        document.querySelector(".local-history:not(.history-loading)"),
      );
      assert.equal(
        await page.locator(".local-history .xterm-rows").textContent(),
        bottomText,
        "first sub-row tick does not jump a whole line or reveal an unpainted frame",
      );
      await page
        .locator(".local-history .xterm-screen")
        .dispatchEvent("wheel", {
          deltaY: -rowHeight / 2,
          bubbles: true,
          cancelable: true,
        });
      await page.waitForTimeout(80);
      assert.equal(
        await page.locator(".local-history .xterm-rows").textContent(),
        bottomText,
      );
      await page
        .locator(".local-history .xterm-screen")
        .dispatchEvent("wheel", {
          deltaY: -rowHeight / 4,
          bubbles: true,
          cancelable: true,
        });
      await page.waitForFunction(
        (text) =>
          document.querySelector(".local-history .xterm-rows").textContent !==
          text,
        bottomText,
      );
      await page.evaluate(() => tab.history.close());
      await page.setViewportSize({ width: 1200, height: 850 });
      // Real wheel input exercises the browser's default page scrolling, which
      // synthetic dispatchEvent cannot reproduce. Leave room above and below.
      await page.evaluate(() => {
        window.pauseCapture = false;
        document.body.style.minHeight = "2000px";
        tab.el.style.marginTop = "200px";
        window.scrollTo(0, 100);
      });
      await page.waitForTimeout(220);
      const pageY = await page.evaluate(() => scrollY);
      const bounds = await page.locator(".terminal-instance").boundingBox();
      await page.mouse.move(bounds.x + 2, bounds.y + 2);
      await page.mouse.wheel(50, 1); // sideways-led first tick on terminal padding
      await page.waitForTimeout(120);
      assert.equal(await page.evaluate(() => scrollY), pageY);
      await page.locator(".power-scroll-toggle").hover();
      await page.mouse.wheel(0, 80);
      await page.waitForTimeout(120);
      assert.equal(await page.evaluate(() => scrollY), pageY);
      await page.mouse.move(bounds.x + 40, bounds.y + 80);
      await page.mouse.wheel(0, -80);
      await page.waitForFunction(() =>
        document.querySelector(".local-history:not(.history-loading)"),
      );
      await page.waitForTimeout(120);
      assert.equal(
        await page.evaluate(() => scrollY),
        pageY,
        "opening history must not move the page",
      );
      await page.evaluate(() => tab.history.close());
      await page.mouse.move(1100, 620); // outside the terminal: normal page scrolling remains
      await page.waitForTimeout(400);
      await page.mouse.wheel(0, 120);
      await page.waitForTimeout(200);
      assert.ok(await page.evaluate((y) => scrollY > y, pageY));
      assert.equal(await page.evaluate(() => remoteInput), "");
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
