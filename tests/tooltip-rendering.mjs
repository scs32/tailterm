import { chromium, webkit } from "@playwright/test";
const engine = process.env.TEST_BROWSER || "chromium";
if (!["chromium", "webkit"].includes(engine))
  throw new Error("Unknown TEST_BROWSER");
const selectedBrowser = engine === "webkit" ? webkit : chromium;
import { readFile } from "node:fs/promises";
import { createServer } from "node:http";
import { once } from "node:events";
import assert from "node:assert/strict";
const stylesheets = new Map([
  ["/client/style.css", await readFile("client/style.css", "utf8")],
  ["/client/usage.css", await readFile("client/usage.css", "utf8")],
]);
const cssLoads = new Set();
const html = '<button id="action" title="Workspace action" aria-describedby="existing-help">Action</button><span id="existing-help">Help</span><button id="keys" title="SSH keys">Keys</button><button id="diagnostics" title="Connection diagnostics">Diagnostics</button><button id="tab" title="Workspace tab">Tab</button><div class="xterm"><div id="rows"></div></div>';
const server = createServer((request, response) => {
  if (request.url === "/") {
    response.writeHead(200, { "content-type": "text/html" });
    response.end(html);
  } else if (stylesheets.has(request.url)) {
    cssLoads.add(request.url);
    response.writeHead(200, { "content-type": "text/css" });
    response.end(stylesheets.get(request.url));
  } else {
    response.writeHead(404);
    response.end();
  }
});
server.listen(0, "127.0.0.1");
await once(server, "listening");
const origin = `http://127.0.0.1:${server.address().port}`;
let browser;
try {
  browser = await selectedBrowser.launch();
  const page = await browser.newPage();
  await page.goto(origin);
  await page.addStyleTag({ url: `${origin}/client/style.css` });
  assert.ok(cssLoads.has("/client/style.css"), "style.css was not loaded");
  assert.ok(cssLoads.has("/client/usage.css"), "usage.css import was not loaded");
  const source = await readFile("client/tooltips.js", "utf8");
  await page.addScriptTag({
    content:
      source.replace(
        "export function setupTooltips",
        "function setupTooltips",
      ) + "\nsetupTooltips();",
  });
  const result = await page.evaluate(async () => {
    let scans = 0;
    const query = Element.prototype.querySelectorAll;
    Element.prototype.querySelectorAll = function (selector) {
      if (selector === "[title]" && this.closest(".xterm")) scans++;
      return query.call(this, selector);
    };
    const rows = document.querySelector("#rows"),
      start = performance.now();
    for (let frame = 0; frame < 40; frame++) {
      const fragment = document.createDocumentFragment();
      for (let i = 0; i < 60; i++) {
        const row = document.createElement("div");
        row.className = "xterm-row";
        for (let j = 0; j < 20; j++) {
          const span = document.createElement("span");
          span.textContent = "output";
          row.append(span);
        }
        fragment.append(row);
      }
      rows.replaceChildren(fragment);
      await new Promise((resolve) => setTimeout(resolve, 0));
    }
    const action = document.querySelector("#action");
    action.title = "Updated workspace action";
    await new Promise((resolve) => setTimeout(resolve, 0));
    Element.prototype.querySelectorAll = query;
    return {
      terminalTooltipScans: scans,
      elapsedMs: Math.round(performance.now() - start),
      tooltip: action.dataset.tooltip,
    };
  });
  await page.locator("#action").focus();
  await page.locator("#workspace-tooltip:popover-open").waitFor();
  for (const [id, title] of [
    ["keys", "SSH keys"],
    ["diagnostics", "Connection diagnostics"],
    ["tab", "Workspace tab"],
  ]) {
    await page.locator("#" + id).hover();
    await page.waitForFunction(
      (text) =>
        document.querySelector("#workspace-tooltip:popover-open")
          ?.textContent === text,
      title,
    );
    assert.equal(
      await page
        .locator("#workspace-tooltip")
        .evaluate((el) => getComputedStyle(el).borderRadius),
      "0px",
    );
    assert.equal(await page.locator("#" + id).getAttribute("title"), null);
  }
  await page.keyboard.press("Escape");
  assert.equal(
    await page.locator("#action").getAttribute("aria-describedby"),
    "existing-help",
  );
  console.log(JSON.stringify(result));
  assert.equal(result.tooltip, "Updated workspace action");
  if (!process.argv.includes("--baseline"))
    assert.equal(
      result.terminalTooltipScans,
      0,
      "Terminal redraws must not trigger tooltip subtree scans",
    );
} finally {
  try {
    await browser?.close();
  } finally {
    await new Promise((resolve, reject) =>
      server.close((error) => (error ? reject(error) : resolve())),
    );
  }
  assert.equal(server.listening, false);
  console.log("Isolated tooltip browser and CSS server closed.");
}
