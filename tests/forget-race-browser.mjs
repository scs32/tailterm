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
      await page.route("**/forget-race", (route) =>
        route.fulfill({
          contentType: "text/html",
          body: "<!doctype html><body>Vault concurrency test</body>",
        }),
      );
      await page.goto(
        `http://127.0.0.1:${server.httpServer.address().port}/forget-race`,
      );
      const result = await page.evaluate(async () => {
        const vault = await import("/client/local-vault.js");
        await vault.localAPI("/unlock", "POST", {
          password: "disposable race test passphrase",
        });
        const encrypt = crypto.subtle.encrypt.bind(crypto.subtle);
        let release;
        const gate = new Promise((resolve) => (release = resolve));
        crypto.subtle.encrypt = (...args) => gate.then(() => encrypt(...args));
        try {
          const forgetting = vault.forgetDevice();
          // A save whose encryption completes after deletion must never recreate the record.
          const late = Promise.allSettled([
            vault.saveWorkspace({ tabs: [], groups: [], active: null }),
          ]);
          await forgetting;
          release();
          const [saved] = await late;
          return {
            saveStatus: saved.status,
            vault: await vault.localAPI("/status"),
          };
        } finally {
          release();
          crypto.subtle.encrypt = encrypt;
        }
      });
      assert.equal(result.saveStatus, "rejected");
      assert.deepEqual(result.vault, { initialized: false, unlocked: false });
      console.log(
        `${engine.name()}: a late encrypted save cannot resurrect a forgotten vault.`,
      );
    } finally {
      await browser.close();
    }
  }
} finally {
  await server.close();
}
