import { chromium, webkit } from "@playwright/test";
import { createServer } from "node:http";
import { readFile } from "node:fs/promises";
import { once } from "node:events";
import assert from "node:assert/strict";
import { mockSpeechWorker } from "./voice-dictation-browser.mjs";
const server = createServer(async (req, res) => {
  try {
    const name = new URL(req.url, "http://localhost").pathname;
    const file = "dist-static" + (name === "/" ? "/index.html" : name);
    const bytes = await readFile(file);
    res.setHeader(
      "Content-Type",
      file.endsWith(".js")
        ? "text/javascript"
        : file.endsWith(".css")
          ? "text/css"
          : "text/html",
    );
    res.end(bytes);
  } catch {
    res.writeHead(404);
    res.end();
  }
});
server.listen(0, "127.0.0.1");
await once(server, "listening");
try {
  for (const engine of [chromium, webkit]) {
    const browser = await engine.launch();
    try {
      const context = await browser.newContext();
      await mockSpeechWorker(context);
      const page = await context.newPage();
      await page.addInitScript(() => {
        const originalNow = Date.now;
        window.idleTestOffset = 0;
        Date.now = () => originalNow() + window.idleTestOffset;
      });
      await page.goto(`http://127.0.0.1:${server.address().port}/`);
      const unlock = async () => {
        await page
          .locator("#password")
          .fill("inactivity browser test password");
        await page.locator("#unlock-button").click();
        await page.locator("#workspace").waitFor();
      };
      await unlock();
      await page.locator("#appearance").click();
      assert.equal(await page.locator("#idle-lock-minutes").inputValue(), "15");
      await page.locator("#idle-lock-minutes").selectOption("5");
      await page.locator("#dialog-close").click();
      // Simulate suspended timers, then resume through an actual DOM event.
      await page.evaluate(() => {
        window.idleTestOffset = 300001;
        window.dispatchEvent(new Event("focus"));
      });
      await page.locator("#lockscreen").waitFor();
      await unlock();
      await page.locator("#appearance").click();
      assert.equal(
        await page.locator("#idle-lock-minutes").inputValue(),
        "5",
        "setting survives locking and reload",
      );
      await page.locator("#dialog-close").click();
      await page.evaluate(() => {
        window.expiredClickReached = false;
        document
          .querySelector("#appearance")
          .addEventListener("pointerdown", () => {
            window.expiredClickReached = true;
          });
        window.idleTestOffset = 300001;
        document.querySelector("#appearance").dispatchEvent(
          new PointerEvent("pointerdown", {
            bubbles: true,
            cancelable: true,
          }),
        );
        if (window.expiredClickReached)
          throw Error("Expired input reached the control");
      });
      await page.locator("#lockscreen").waitFor();
      console.log(
        `PASS ${engine.name()}: configurable lock, sleep/resume, expired input intercepted, encrypted vault remains unlockable.`,
      );
    } finally {
      await browser.close();
    }
  }
} finally {
  server.close();
}
