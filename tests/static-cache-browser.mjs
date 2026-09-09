import { chromium, webkit } from "@playwright/test";
import assert from "node:assert/strict";
import { createServer } from "node:http";
import { once } from "node:events";
import { readFile } from "node:fs/promises";

const headers = await readFile("deploy/_headers", "utf8");
const assetRule = headers.match(/\/bundles\/\*\s+Cache-Control:\s*([^\n]+)/);
assert.ok(assetRule, "deploy/_headers must define the bundle cache policy");
const assetCacheControl = assetRule[1].trim();
assert.equal(assetCacheControl, "no-cache");

for (const engine of [chromium, webkit]) {
  let cssRequests = 0;
  const server = createServer((request, response) => {
    if (request.url === "/bundles/index-upgrade.css") {
      cssRequests++;
      if (cssRequests === 1) {
        // Cloudflare Pages' SPA fallback when a deploy-time asset is not yet
        // available. nosniff makes browsers correctly reject it as CSS.
        const body = "<!doctype html><title>TailOS fallback</title>";
        response.writeHead(200, {
          "Cache-Control": assetCacheControl,
          "Content-Type": "text/html; charset=utf-8",
          "Content-Length": Buffer.byteLength(body),
          "X-Content-Type-Options": "nosniff",
        });
        response.end(body);
        return;
      }
      const body = "body { background: rgb(1, 2, 3); }";
      response.writeHead(200, {
        "Cache-Control": assetCacheControl,
        "Content-Type": "text/css; charset=utf-8",
        "Content-Length": Buffer.byteLength(body),
        "X-Content-Type-Options": "nosniff",
      });
      response.end(body);
      return;
    }

    const body =
      '<!doctype html><link rel="stylesheet" href="/bundles/index-upgrade.css"><p>TailOS</p>';
    response.writeHead(200, {
      "Cache-Control": "no-cache",
      "Content-Type": "text/html; charset=utf-8",
      "Content-Length": Buffer.byteLength(body),
    });
    response.end(body);
  });
  server.listen(0, "127.0.0.1");
  await once(server, "listening");

  const browser = await engine.launch();
  try {
    const context = await browser.newContext();
    const page = await context.newPage();
    const origin = `http://127.0.0.1:${server.address().port}`;
    await page.goto(origin);
    assert.equal(
      await page.evaluate(
        () => getComputedStyle(document.body).backgroundColor,
      ),
      "rgba(0, 0, 0, 0)",
      `${engine.name()} must reject the HTML fallback as CSS`,
    );
    await page.reload();
    assert.equal(cssRequests, 2, `${engine.name()} must revalidate failed CSS`);
    assert.equal(
      await page.evaluate(
        () => getComputedStyle(document.body).backgroundColor,
      ),
      "rgb(1, 2, 3)",
      `${engine.name()} must apply CSS after reload`,
    );
    await context.close();
    console.log(
      `${engine.name()}: deploy-time CSS fallback recovered on reload`,
    );
  } finally {
    await browser.close();
    server.close();
    await once(server, "close");
  }
}
