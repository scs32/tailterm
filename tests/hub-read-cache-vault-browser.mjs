// Real encrypted IndexedDB in disposable contexts; no hub, sync, or SSH calls.
import { chromium, webkit } from "@playwright/test";
import { createServer } from "vite";
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
      await page.route("**/hub-cache-vault-test", (route) =>
        route.fulfill({
          contentType: "text/html",
          body: "<!doctype html><title>Isolated hub cache vault fixture</title>",
        }),
      );
      await page.goto(
        `http://127.0.0.1:${server.httpServer.address().port}/hub-cache-vault-test`,
      );
      const password = "isolated hub cache vault passphrase";
      const first = await page.evaluate(async (password) => {
        const vault = await import("/client/local-vault.js");
        const { createHubReadCache } =
          await import("/client/hub-read-cache.js");
        await vault.localAPI("/unlock", "POST", { password });
        const cache = createHubReadCache(vault.hubReadCachePersistence());
        const beforeSerial = (await vault.profileSnapshot()).serial;
        let profileChanges = 0;
        window.addEventListener(
          "tailterm-profile-change",
          () => profileChanges++,
        );
        const source = {
          marker: "PRIVATE_CACHED_PROJECT_FIXTURE",
          projects: [{ id: "tsk_fixture", name: "Cached fixture" }],
        };
        await cache.put("credential-qualified-a", "projects", source);
        source.projects[0].name = "mutated";
        const found = await cache.get("credential-qualified-a", "projects");
        return {
          found,
          localStorage: Object.values(localStorage),
          profileChanges,
          profileSerialUnchanged:
            (await vault.profileSnapshot()).serial === beforeSerial,
          hiddenFromLocalData: !Object.hasOwn(
            vault.localData(),
            "hubReadCache",
          ),
        };
      }, password);
      assert.equal(first.found.value.projects[0].name, "Cached fixture");
      assert.ok(
        first.localStorage.every(
          (value) => !value.includes("PRIVATE_CACHED_PROJECT_FIXTURE"),
        ),
      );
      assert.equal(first.profileChanges, 0);
      assert.equal(first.profileSerialUnchanged, true);
      assert.equal(first.hiddenFromLocalData, true);

      await page.reload();
      const reloaded = await page.evaluate(async (password) => {
        const vault = await import("/client/local-vault.js");
        const { createHubReadCache } =
          await import("/client/hub-read-cache.js");
        const { openVault } = await import("/client/vault-crypto.js");
        await vault.localAPI("/unlock", "POST", { password });
        const cache = createHubReadCache(vault.hubReadCachePersistence());
        const found = await cache.get("credential-qualified-a", "projects");
        const backup = await openVault(await vault.exportBackup(), password);
        const profile = await vault.profileSnapshot();
        const envelope = await new Promise((resolve, reject) => {
          const open = indexedDB.open("tailserve", 1);
          open.onerror = () => reject(open.error);
          open.onsuccess = () => {
            const database = open.result;
            const request = database
              .transaction("vault")
              .objectStore("vault")
              .get("encrypted");
            request.onerror = () => reject(request.error);
            request.onsuccess = () => {
              resolve(request.result);
              database.close();
            };
          };
        });
        await vault.localAPI("/lock", "POST");
        let lockedError = "";
        try {
          await cache.get("credential-qualified-a", "projects");
        } catch (error) {
          lockedError = error.message;
        }
        await vault.localAPI("/unlock", "POST", { password });
        let staleGetError = "",
          stalePutError = "";
        try {
          await cache.get("credential-qualified-a", "projects");
        } catch (error) {
          staleGetError = error.message;
        }
        try {
          await cache.put("credential-qualified-a", "projects", {
            marker: "MUST_NOT_REPLACE_CACHE",
          });
        } catch (error) {
          stalePutError = error.message;
        }
        const freshCache = createHubReadCache(vault.hubReadCachePersistence());
        const fresh = await freshCache.get(
          "credential-qualified-a",
          "projects",
        );
        return {
          found,
          fresh,
          backupHasCache: Object.hasOwn(backup.data, "hubReadCache"),
          profileHasCache: Object.hasOwn(profile.data, "hubReadCache"),
          encryptedAtRest: !JSON.stringify(envelope).includes(
            "PRIVATE_CACHED_PROJECT_FIXTURE",
          ),
          lockedError,
          staleGetError,
          stalePutError,
        };
      }, password);
      assert.equal(reloaded.found.value.projects[0].name, "Cached fixture");
      assert.equal(reloaded.backupHasCache, false);
      assert.equal(reloaded.profileHasCache, false);
      assert.equal(reloaded.encryptedAtRest, true);
      assert.match(reloaded.lockedError, /Unlock your local vault/);
      assert.match(reloaded.staleGetError, /different vault unlock/);
      assert.match(reloaded.stalePutError, /different vault unlock/);
      assert.equal(
        reloaded.fresh.value.marker,
        "PRIVATE_CACHED_PROJECT_FIXTURE",
      );
      console.log(
        `${engine.name()}: hub cache survives encrypted reload, stays out of plaintext storage and portable/profile exports, and stops reading when locked.`,
      );
    } finally {
      await browser.close();
    }
  }
} finally {
  await server.close();
}
