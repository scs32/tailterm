// Work-item drafts and idempotency keys live only inside the disposable,
// encrypted browser vault. No hub, profile, or live credential data is used.
import { chromium, webkit } from "@playwright/test";
import { createServer } from "vite";
import assert from "node:assert/strict";

const server = await createServer({ server: { host: "127.0.0.1", port: 0 } });
await server.listen();
try {
  for (const engine of [chromium, webkit]) {
    const browser = await engine.launch();
    try {
      const page = await browser.newPage();
      await page.route("**/work-item-draft-vault-test", (route) => route.fulfill({ contentType: "text/html", body: "<!doctype html><title>Isolated work-item draft vault fixture</title>" }));
      await page.goto(`http://127.0.0.1:${server.httpServer.address().port}/work-item-draft-vault-test`);
      const password = "isolated work item draft passphrase";
      const first = await page.evaluate(async (password) => {
        const vault = await import("/client/local-vault.js");
        await vault.localAPI("/unlock", "POST", { password });
        const drafts = vault.workItemDraftPersistence();
        const scopeA = "a".repeat(64), scopeB = "b".repeat(64);
        for (let index = 0; index < 30; index++)
          await drafts.save({ scope: scopeA, id: `feature:task:${index}`, requestId: `key-${index}`, values: { title: `draft-${index}` }, updatedAt: new Date(Date.now()+index).toISOString() });
        const description = "x".repeat(8192);
        await drafts.save({
          scope: scopeA,
          id: "bug:task:item",
          requestId: "PRIVATE_RETRY_KEY",
          values: { taskId: "task", title: "PRIVATE_DRAFT_MARKER", description, status: "open", priority: "normal" },
          intent: {
            taskId: "task",
            itemId: "item",
            request: { title: "PRIVATE_DRAFT_MARKER", description, status: "open", priority: "normal", expectedRevision: 42, requestId: "PRIVATE_RETRY_KEY" },
          },
          updatedAt: new Date(Date.now()+1000).toISOString(),
        });
        return {
          found: await drafts.load(scopeA, "bug:task:item"),
          otherScope: await drafts.load(scopeB, "bug:task:item"),
          oldestEvicted: await drafts.load(scopeA, "feature:task:0"),
          latestRetained: await drafts.load(scopeA, "feature:task:29"),
          hidden: !Object.hasOwn(vault.localData(), "workItemDrafts"),
          localStorage: Object.values(localStorage),
        };
      }, password);
      assert.equal(first.found?.requestId, "PRIVATE_RETRY_KEY");
      assert.equal(first.found?.intent.request.expectedRevision, 42);
      assert.equal(first.otherScope, null);
      assert.equal(first.oldestEvicted, null);
      assert.equal(first.latestRetained?.values.title, "draft-29");
      assert.equal(first.hidden, true);
      assert.ok(first.localStorage.every((value) => !/PRIVATE_(DRAFT|RETRY)/.test(value)));

      await page.reload();
      const reloaded = await page.evaluate(async (password) => {
        const vault = await import("/client/local-vault.js");
        const { openVault } = await import("/client/vault-crypto.js");
        await vault.localAPI("/unlock", "POST", { password });
        const drafts = vault.workItemDraftPersistence(), scope = "a".repeat(64);
        const found = await drafts.load(scope, "bug:task:item");
        const backup = await openVault(await vault.exportBackup(), password);
        const profile = await vault.profileSnapshot();
        const envelope = await new Promise((resolve, reject) => {
          const open = indexedDB.open("tailserve", 1);
          open.onerror = () => reject(open.error);
          open.onsuccess = () => {
            const database = open.result, request = database.transaction("vault").objectStore("vault").get("encrypted");
            request.onerror = () => reject(request.error);
            request.onsuccess = () => { resolve(request.result); database.close(); };
          };
        });
        await drafts.remove(scope, "bug:task:item");
        return {
          found,
          removed: await drafts.load(scope, "bug:task:item"),
          backupHasDrafts: Object.hasOwn(backup.data, "workItemDrafts"),
          profileHasDrafts: Object.hasOwn(profile.data, "workItemDrafts"),
          encryptedAtRest: !JSON.stringify(envelope).includes("PRIVATE_DRAFT_MARKER") && !JSON.stringify(envelope).includes("PRIVATE_RETRY_KEY"),
        };
      }, password);
      assert.equal(reloaded.found?.requestId, "PRIVATE_RETRY_KEY");
      assert.equal(reloaded.found?.intent.request.expectedRevision, 42);
      assert.equal(reloaded.removed, null);
      assert.equal(reloaded.backupHasDrafts, false);
      assert.equal(reloaded.profileHasDrafts, false);
      assert.equal(reloaded.encryptedAtRest, true);
      console.log(`${engine.name()}: work-item drafts are bounded, credential-scoped, encrypted across reload, excluded from portable/profile data, and explicitly removable.`);
    } finally { await browser.close(); }
  }
} finally { await server.close(); }
