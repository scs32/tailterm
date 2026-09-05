import { createServer } from "vite";
import { chromium } from "@playwright/test";
import assert from "node:assert/strict";
const server = await createServer({
  configFile: false,
  server: { host: "127.0.0.1", port: 0 },
});
await server.listen();
const browser = await chromium.launch({
  args: [
    "--use-fake-device-for-media-stream",
    "--use-fake-ui-for-media-stream",
  ],
});
try {
  const page = await browser.newPage();
  await page.route("**/speech-worker.js*", (r) =>
    r.fulfill({
      contentType: "text/javascript",
      body: `self.onmessage=({data})=>setTimeout(()=>self.postMessage({id:data.id,type:'result',value:data.type==='load'?true:'safe text\\nsecond line'}),data.type==='load'?50:800);`,
    }),
  );
  await page.route("**/dictation-test", (r) =>
    r.fulfill({
      contentType: "text/html",
      body: "<!doctype html><html><body></body></html>",
    }),
  );
  await page.goto(
    `http://127.0.0.1:${server.httpServer.address().port}/dictation-test`,
  );
  await page.evaluate(async () => {
    const { setupVoiceDictation } = await import("/client/voice-dictation.js");
    window.sent = [];
    window.destination = {
      id: "original",
      server: { host: "fixture", port: 22, username: "test" },
      status: "Connected",
      send() {},
      term: {
        paste(text) {
          window.sent.push(text);
        },
        focus() {},
      },
    };
    window.active = window.destination;
    window.voice = setupVoiceDictation({
      getActive: () => window.active,
      available: () => true,
      notice() {},
    });
  });
  const record = async () => {
    await page.keyboard.press("Alt+Space");
    await page.waitForFunction(
      () => document.querySelector("#voice-stop")?.disabled === false,
    );
    await page.waitForTimeout(800);
  };
  await record();
  await page.locator("#voice-stop").click();
  await page.waitForFunction(() => window.sent.length === 1);
  assert.deepEqual(await page.evaluate(() => window.sent), [
    "safe text second line",
  ]);
  await record();
  await page.evaluate(
    () => (window.active = { ...window.destination, id: "other" }),
  );
  await page.locator("#voice-stop").click();
  await page.locator("#voice-transcript").waitFor();
  assert.equal(
    await page.locator("#voice-transcript").inputValue(),
    "safe text second line",
  );
  assert.equal(await page.evaluate(() => window.sent.length), 1);
  await page.keyboard.press("Escape");
  await page.evaluate(() => (window.active = window.destination));
  await record();
  await page.locator("#voice-stop").click();
  await page.keyboard.press("Escape");
  await page.waitForTimeout(1200);
  assert.equal(await page.evaluate(() => window.sent.length), 1);
  assert.equal(await page.locator("#voice-dialog").count(), 0);
  console.log(
    "PASS: Stop inserts once without Enter; changed destination preserves text; cancellation during transcription inserts nothing.",
  );
} finally {
  await browser.close();
  await server.close();
}
