// Encrypted Queue intent recovery for wi_a222a4d8a69c1d53 r2, order #1640/#1642.
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
      await page.route("**/project-queue-vault-test", (route) =>
        route.fulfill({
          contentType: "text/html",
          body: "<!doctype html><title>Queue vault fixture</title>",
        }),
      );
      await page.goto(
        `http://127.0.0.1:${server.httpServer.address().port}/project-queue-vault-test`,
      );
      const password = `isolated Queue vault ${engine.name()}`;
      const first = await page.evaluate(async (password) => {
        const vault = await import("/client/local-vault.js");
        await vault.localAPI("/unlock", "POST", {
          password,
          username: "queue-profile",
        });
        const persistence = vault.queueIntentPersistence();
        const scope = "d".repeat(64);
        const make = (index, requestId = `queue-key-${index}`) => ({
          scope,
          id: `tsk_0000000000000001:que_0000000000000002:priority:${index}`,
          state: "uncertain",
          requestId,
          taskId: "tsk_0000000000000001",
          entryId: "que_0000000000000002",
          payload: {
            operation: "priority",
            requestId,
            expectedRevision: index + 1,
            cycle: 1,
            priority: "high",
          },
          updatedAt: new Date(Date.now() + index).toISOString(),
        });
        const older = make(0, "PRIVATE_QUEUE_OLD_KEY");
        await persistence.save(older);
        const newer = make(0, "PRIVATE_QUEUE_NEW_KEY");
        newer.payload.priority = "urgent";
        newer.updatedAt = new Date(Date.now() + 100).toISOString();
        await persistence.save(newer);
        await persistence.remove(scope, newer.id, older.requestId);
        for (let index = 1; index < 24; index++)
          await persistence.save(make(index));
        let fullError = "";
        try {
          await persistence.save(make(24));
        } catch (error) {
          fullError = error.message;
        }
        const backup = await vault.exportBackup();
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
        return {
          rows: await persistence.list(scope),
          fullError,
          localHidden: !Object.hasOwn(vault.localData(), "queueIntents"),
          backupHidden: !JSON.stringify(backup).includes("PRIVATE_QUEUE"),
          profileHidden: !Object.hasOwn(profile.data, "queueIntents"),
          encrypted: !JSON.stringify(envelope).includes("PRIVATE_QUEUE"),
        };
      }, password);
      assert.equal(first.rows.length, 24);
      assert.equal(
        first.rows.find((row) => row.id.endsWith(":0")).requestId,
        "PRIVATE_QUEUE_NEW_KEY",
      );
      assert.match(first.fullError, /storage is full/i);
      assert.equal(first.localHidden, true);
      assert.equal(first.backupHidden, true);
      assert.equal(first.profileHidden, true);
      assert.equal(first.encrypted, true);

      await page.reload();
      const recovered = await page.evaluate(async (password) => {
        const vault = await import("/client/local-vault.js");
        await vault.localAPI("/unlock", "POST", {
          password,
          username: "queue-profile",
        });
        const persistence = vault.queueIntentPersistence(),
          scope = "d".repeat(64);
        const rows = await persistence.list(scope),
          newest = rows.find((row) => row.id.endsWith(":0"));
        await persistence.remove(scope, newest.id, newest.requestId);
        const afterRemove = await persistence.list(scope);
        await vault.localAPI("/lock", "POST");
        let locked = "";
        try {
          await persistence.list(scope);
        } catch (error) {
          locked = error.message;
        }
        return { rows, afterRemove, locked };
      }, password);
      assert.equal(recovered.rows.length, 24);
      assert.equal(
        recovered.rows.find((row) => row.id.endsWith(":0")).payload.priority,
        "urgent",
      );
      assert.equal(recovered.afterRemove.length, 23);
      assert.match(recovered.locked, /unlock/i);
      console.log(
        `${engine.name()}: Queue intents remained encrypted, bounded, scoped and newer-edit safe across reload.`,
      );
    } finally {
      await browser.close();
    }
  }
} finally {
  await server.close();
}
