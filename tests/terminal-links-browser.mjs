import { useDomRenderer } from "./dom-renderer.mjs";
import { createServer } from "vite";
import { chromium } from "@playwright/test";
import assert from "node:assert/strict";
const server = await createServer({
  configFile: false,
  server: { host: "127.0.0.1", port: 0 },
});
await server.listen();
const browser = await chromium.launch();
try {
  const page = await browser.newPage();
  await useDomRenderer(page);
  const errors = [];
  page.on("pageerror", (e) => errors.push(e.message));
  await page.route("**/links-test", (route) =>
    route.fulfill({
      contentType: "text/html",
      body: '<!doctype html><html><body><div id="terminal"></div><pre id="history"></pre></body></html>',
    }),
  );
  await page.goto(
    `http://127.0.0.1:${server.httpServer.address().port}/links-test`,
  );
  await page.evaluate(async () => {
    const { Terminal } =
      await import("/node_modules/@xterm/xterm/lib/xterm.mjs");
    await import("/node_modules/@xterm/xterm/css/xterm.css");
    await import("/client/style.css");
    const links = await import("/client/terminal-links.js");
    window.links = links;
    window.opened = [];
    window.open = (...args) => {
      opened.push(args);
      return null;
    };
    window.term = new Terminal({ cols: 40, rows: 12, fontSize: 16 });
    term.open(document.querySelector("#terminal"));
    links.setupTerminalLinks(term);
  });
  const show = async (text) => {
    await page.mouse.move(1000, 700);
    await page.evaluate(
      (text) =>
        new Promise((resolve) => term.write("\x1b[2J\x1b[H" + text, resolve)),
      text,
    );
    await page.waitForTimeout(120);
    // Move through a different row between independent screen fixtures.
    const blank = await point(3, 5);
    await page.mouse.move(blank.x, blank.y);
  };
  const point = async (col = 3, row = 0) =>
    page.locator(".xterm-screen").evaluate(
      (el, { col, row }) => {
        const b = el.getBoundingClientRect();
        return {
          x: b.x + ((col + 0.5) * b.width) / 40,
          y: b.y + ((row + 0.5) * b.height) / 12,
        };
      },
      { col, row },
    );
  const click = async (modifier, col = 3, row = 0) => {
    const p = await point(col, row);
    await page.mouse.move(p.x, p.y);
    await page.waitForTimeout(100);
    await page.mouse.move(p.x + 1, p.y);
    await page.mouse.move(p.x, p.y);
    await page.waitForTimeout(100);
    if (modifier) await page.keyboard.down(modifier);
    await page.mouse.click(p.x, p.y);
    if (modifier) await page.keyboard.up(modifier);
  };
  const url = "https://example.com/path?q=test";
  await show(url);
  await click();
  assert.deepEqual(
    await page.evaluate(() => opened),
    [],
    "Plain clicks do not navigate",
  );
  await click("Meta");
  assert.deepEqual(await page.evaluate(() => opened.at(-1)), [
    url,
    "_blank",
    "noopener,noreferrer",
  ]);
  await click("Control");
  assert.equal(await page.evaluate(() => opened.length), 2);
  const wrapped = "https://example.com/" + "a".repeat(60);
  await show(wrapped);
  await click("Meta", 3, 1);
  assert.equal(
    await page.evaluate(() => opened.at(-1)[0]),
    wrapped,
    "Wrapped URL detected",
  );
  await show("\x1b[?1000h\x1b[?1006h" + url);
  await click("Meta");
  assert.equal(
    await page.evaluate(() => opened.length),
    4,
    "Links work with tmux-style mouse reporting",
  );
  await show(
    "\x1b[?1000l\x1b[?1006l\x1b]8;;https://example.org/real-target\x07Friendly label\x1b]8;;\x07",
  );
  await click("Meta");

  assert.equal(
    await page.evaluate(() => opened.at(-1)[0]),
    "https://example.org/real-target",
  );
  assert.match(
    await page.locator(".terminal-link-hint").innerText(),
    /example.org\/real-target/,
  );
  await show("\x1b]8;;javascript:alert(1)\x07Bad link\x1b]8;;\x07");
  await click("Meta");
  assert.equal(await page.evaluate(() => opened.length), 5);
  const history =
    "Text <script>unsafe</script> (https://example.com/path). https://example.org/a_(b)\nhttp://100.64.0.1:8080/";
  await page.evaluate(
    (text) =>
      links.renderHistoryLinks(document.querySelector("#history"), text),
    history,
  );
  assert.equal(await page.locator("#history").textContent(), history);
  assert.equal(await page.locator("#history script").count(), 0);
  assert.deepEqual(
    await page
      .locator("#history a")
      .evaluateAll((nodes) => nodes.map((n) => n.href)),
    [
      "https://example.com/path",
      "https://example.org/a_(b)",
      "http://100.64.0.1:8080/",
    ],
  );
  assert.ok(
    await page
      .locator("#history a")
      .evaluateAll((nodes) =>
        nodes.every(
          (n) => n.rel === "noopener noreferrer" && n.target === "_blank",
        ),
      ),
  );
  await show(url);
  await click("Meta");
  await page.evaluate(() => term.dispose());
  assert.equal(await page.locator(".terminal-link-hint").count(), 0);
  assert.deepEqual(errors, []);
  console.log(
    "Terminal URLs passed: ordinary/modifier clicks, wrapped URLs, tmux mouse mode, OSC 8 destinations, protocol restrictions, safe history links and disposal.",
  );
} finally {
  await browser.close();
  await server.close();
}
