// Real encrypted IndexedDB in disposable contexts; no hub or SSH calls.
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
      await page.route("**/handler-vault-test", (route) =>
        route.fulfill({
          contentType: "text/html",
          body: "<!doctype html><title>Isolated handler vault fixture</title>",
        }),
      );
      await page.goto(
        `http://127.0.0.1:${server.httpServer.address().port}/handler-vault-test`,
      );
      const first = await page.evaluate(async () => {
        const vault = await import("/client/local-vault.js");
        const { withDatabaseHandler } =
          await import("/client/project-handler.js");
        await vault.localAPI("/unlock", "POST", {
          password: "isolated handler vault passphrase",
        });
        await vault.localAPI("/servers", "POST", {
          name: "Fixture",
          host: "fixture.invalid",
          port: 22,
          username: "fixture",
          mode: "ssh",
        });
        const host = vault.localData().servers[0];
        const fields = withDatabaseHandler([
          {
            server: host,
            fields: {
              name: "lead",
              runtime: "codex",
              run: "codex",
              cwd: "/fixture",
              model: "fixture-model",
              permissionMode: "workspace-auto",
            },
          },
        ])[1].fields;
        fields.agentId = "agt_0123456789abcdef";
        const plan = {
          hub: "http://fixture-hub:18765/",
          taskId: "tsk_0123456789abcdef",
          serverId: host.id,
          fields,
        };
        await vault.localAPI("/project-handler-plans", "POST", plan);
        const encrypt = crypto.subtle.encrypt.bind(crypto.subtle);
        let release, reached;
        const gate = new Promise((resolve) => (release = resolve));
        const entered = new Promise((resolve) => (reached = resolve));
        crypto.subtle.encrypt = (...args) => {
          reached();
          return gate.then(() => encrypt(...args));
        };
        let settled = false;
        try {
          const saving = vault
            .localAPI("/project-handler-plans", "POST", {
              ...plan,
              hub: "http://fixture-hub:18765",
              fields: { ...fields, model: "updated-model" },
              previous: { serverId: plan.serverId, fields },
            })
            .then(() => {
              settled = true;
            });
          await entered;
          const before = {
            settled,
            model: vault.localData().projectHandlerPlans[0].fields.model,
          };
          release();
          await saving;
          return { before, plans: vault.localData().projectHandlerPlans };
        } finally {
          release();
          crypto.subtle.encrypt = encrypt;
        }
      });
      assert.deepEqual(first.before, {
        settled: false,
        model: "fixture-model",
      });
      assert.equal(first.plans.length, 1);
      assert.equal(first.plans[0].fields.model, "updated-model");
      assert.equal(first.plans[0].previous.fields.model, "fixture-model");
      await page.reload();
      const reloaded = await page.evaluate(async () => {
        const vault = await import("/client/local-vault.js");
        const { openVault } = await import("/client/vault-crypto.js");
        const password = "isolated handler vault passphrase";
        await vault.localAPI("/unlock", "POST", { password });
        const plan = vault.localData().projectHandlerPlans[0];
        const encrypt = crypto.subtle.encrypt;
        let failed = false;
        crypto.subtle.encrypt = () =>
          Promise.reject(new Error("isolated save failure"));
        try {
          await vault.localAPI("/project-handler-plans", "POST", {
            ...plan,
            fields: { ...plan.fields, model: "must-not-save" },
          });
        } catch {
          failed = true;
        } finally {
          crypto.subtle.encrypt = encrypt;
        }
        const afterFailure = vault.localData().projectHandlerPlans;
        const snapshot = await vault.profileSnapshot();
        const backup = await openVault(await vault.exportBackup(), password);
        const raw = await new Promise((resolve, reject) => {
          const open = indexedDB.open("tailserve", 1);
          open.onerror = () => reject(open.error);
          open.onsuccess = () => {
            const db = open.result;
            const request = db
              .transaction("vault")
              .objectStore("vault")
              .get("encrypted");
            request.onsuccess = () => {
              resolve(request.result);
              db.close();
            };
          };
        });
        await vault.localAPI("/servers/" + plan.serverId, "DELETE");
        return {
          failed,
          afterFailure,
          plan,
          exported: backup.data.projectHandlerPlans,
          synced: snapshot.data.projectHandlerPlans,
          rawContainsPlan: JSON.stringify(raw).includes(plan.taskId),
          afterServerRemoval: vault.localData().projectHandlerPlans,
        };
      });
      assert.equal(reloaded.failed, true);
      assert.deepEqual(reloaded.plan.previous, first.plans[0].previous);
      assert.deepEqual(reloaded.afterFailure, first.plans);
      assert.deepEqual(reloaded.afterServerRemoval, first.plans);
      assert.equal(reloaded.exported, undefined);
      assert.equal(reloaded.synced, undefined);
      assert.equal(reloaded.rawContainsPlan, false);
      await page.reload();
      const deleted = await page.evaluate(async () => {
        const vault = await import("/client/local-vault.js");
        await vault.localAPI("/unlock", "POST", {
          password: "isolated handler vault passphrase",
        });
        const before = vault.localData();
        await vault.localAPI("/project-handler-plans", "DELETE", {
          hub: "http://fixture-hub:18765/",
          taskId: "tsk_0123456789abcdef",
        });
        await vault.localAPI("/lock", "POST");
        await vault.localAPI("/unlock", "POST", {
          password: "isolated handler vault passphrase",
        });
        return {
          missingHostPlan:
            before.servers.length === 0 &&
            before.projectHandlerPlans.length === 1,
          remaining: vault.localData().projectHandlerPlans,
        };
      });
      assert.equal(deleted.missingHostPlan, true);
      assert.deepEqual(deleted.remaining, []);
      console.log(
        `${engine.name()}: handler plans await encrypted save, survive reload/missing host, reject failed writes, stay out of exports/sync, and delete durably.`,
      );
    } finally {
      await browser.close();
    }
  }
} finally {
  await server.close();
}
