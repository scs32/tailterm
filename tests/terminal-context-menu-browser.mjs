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
      const page = await browser.newPage();
      const errors = [];
      page.on("pageerror", (e) => errors.push(e.message));
      await page.route("**/context-test", (r) =>
        r.fulfill({
          contentType: "text/html",
          body: '<!doctype html><html><body><div id="terminal"></div><pre id="history">Cached history</pre><input id="outside"></body></html>',
        }),
      );
      await page.goto(
        `http://127.0.0.1:${server.httpServer.address().port}/context-test`,
      );
      await page.evaluate(async () => {
        const { Terminal } =
          await import("/node_modules/@xterm/xterm/lib/xterm.mjs");
        await import("/node_modules/@xterm/xterm/css/xterm.css");
        const { setupTerminalContextMenu } =
          await import("/client/terminal-input.js");
        window.term = new Terminal({ cols: 40, rows: 10 });
        term.open(document.querySelector("#terminal"));
        setupTerminalContextMenu(term);
        window.input = [];
        term.onData((data) => input.push(data));
        window.menu = (selector, shiftKey = false) => {
          const event = new MouseEvent("contextmenu", {
            bubbles: true,
            cancelable: true,
            button: 2,
            shiftKey,
          });
          document.querySelector(selector).dispatchEvent(event);
          return event.defaultPrevented;
        };
      });
      const write = async (text) =>
        page.evaluate(
          (text) => new Promise((resolve) => term.write(text, resolve)),
          text,
        );
      assert.equal(
        await page.evaluate(() => menu(".xterm-screen")),
        false,
        "Plain shell keeps browser menu",
      );
      for (const mode of [9, 1000, 1002, 1003]) {
        await write(`\x1b[?${mode}h\x1b[?1006h`);
        assert.equal(
          await page.evaluate(() => menu(".xterm-screen")),
          true,
          "Mouse-enabled terminal suppresses browser menu",
        );
        assert.equal(
          await page.evaluate(() => menu(".xterm-helper-textarea")),
          true,
          "Input helper also suppresses browser menu",
        );
        assert.equal(
          await page.evaluate(() => menu(".xterm-screen", true)),
          false,
          "Shift permits browser menu",
        );
        assert.equal(await page.evaluate(() => menu("#history")), false);
        assert.equal(await page.evaluate(() => menu("#outside")), false);
        await write(`\x1b[?${mode}l`);
        assert.equal(
          await page.evaluate(() => menu(".xterm-screen")),
          false,
          "Leaving mouse mode restores browser menu",
        );
      }
      await write("\x1b[?1000h\x1b[?1006h");
      const screen = page.locator(".xterm-screen");
      await page.evaluate(() => (input = []));
      await screen.click({ button: "right", position: { x: 20, y: 20 } });
      assert.ok(
        (await page.evaluate(() => input.join(""))).includes("\x1b[<2;"),
        "Right click still reaches tmux",
      );
      await page.keyboard.press("Escape");
      await page.evaluate(() => (input = []));
      await screen.click({
        button: "right",
        modifiers: ["Shift"],
        position: { x: 20, y: 20 },
      });
      assert.equal(
        await page.evaluate(() => input.join("")),
        "",
        "Shift-right-click never reaches tmux",
      );
      await page.keyboard.press("Escape");
      await page.evaluate(() => {
        const element = term.element;
        term.dispose();
        document.querySelector("#terminal").append(element);
      });
      assert.equal(
        await page.evaluate(() => menu(".xterm")),
        false,
        "Disposed listener removed",
      );
      assert.deepEqual(errors, []);
      console.log(
        `${engine.name()}: context-menu suppression, Shift bypass, remote mouse input, mode changes and cleanup passed.`,
      );
    } finally {
      await browser.close();
    }
  }
} finally {
  await server.close();
}
