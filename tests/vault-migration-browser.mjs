import { chromium, webkit } from "@playwright/test";
import { createServer } from "vite";
import assert from "node:assert/strict";

const html = `<!doctype html><html><body><script type="module">
import {deriveVaultKey,sealVault} from '/client/vault-crypto.js';
const password='isolated legacy migration passphrase';
const salt=crypto.getRandomValues(new Uint8Array(16)),key=await deriveVaultKey(password,salt);
const legacy={servers:[{id:'machine-demo-01',name:'Synthetic',host:'example.invalid',port:22,username:'test',mode:'ssh'}],keys:[],sessions:[],hub:{url:'',token:''},launchProfiles:[],teams:[{id:'team_legacy',name:'Legacy',orchestrator:'lead',members:[{name:'lead',role:'Lead',serverId:'machine-demo-01',runtime:'codex',model:'gpt-5.3-codex',reasoning:'high',approvalMode:'on-request',sandboxMode:'workspace-write',run:'codex',cwd:'/synthetic/project',prompt:'Preserve this exact long prompt '+'.'.repeat(2000)}]}],tailscale:{}};
const original=await sealVault(legacy,key,salt,1);
const db=await new Promise((resolve,reject)=>{const r=indexedDB.open('tailserve',1);r.onupgradeneeded=()=>r.result.createObjectStore('vault');r.onsuccess=()=>resolve(r.result);r.onerror=()=>reject(r.error)});
const put=(value,name)=>new Promise((resolve,reject)=>{const tx=db.transaction('vault','readwrite');tx.objectStore('vault').put(value,name);tx.oncomplete=resolve;tx.onerror=()=>reject(tx.error)});
const get=(name)=>new Promise((resolve,reject)=>{const tx=db.transaction('vault','readonly'),r=tx.objectStore('vault').get(name);tx.oncomplete=()=>resolve(r.result);tx.onerror=()=>reject(tx.error)});
const del=(name)=>new Promise((resolve,reject)=>{const tx=db.transaction('vault','readwrite');tx.objectStore('vault').delete(name);tx.oncomplete=resolve;tx.onerror=()=>reject(tx.error)});
await put(original,'encrypted');
const vault=await import('/client/local-vault.js');
window.qa={password,original,vault,get,put,del,async seedInvalid(){const invalid={...legacy,teams:[{name:'Invalid',members:[{name:'has spaces',runtime:'codex',run:'codex'}]}]};const envelope=await sealVault(invalid,key,salt,1);await put(envelope,'encrypted');return envelope}};
</script></body></html>`;
const server = await createServer({
  configFile: false,
  root: process.cwd(),
  server: { host: "127.0.0.1", port: 0 },
  plugins: [
    {
      name: "vault-migration",
      configureServer(instance) {
        instance.middlewares.use("/vault-migration", (_req, res) => {
          res.setHeader("Content-Type", "text/html");
          res.end(html);
        });
      },
    },
  ],
});
await server.listen();
const url = `http://127.0.0.1:${server.httpServer.address().port}/vault-migration`;
try {
  for (const [name, engine] of [
    ["chromium", chromium],
    ["webkit", webkit],
  ]) {
    const browser = await engine.launch();
    try {
      const page = await browser.newPage();
      await page.goto(url);
      await page.waitForFunction(() => window.qa);
      const migrated = await page.evaluate(async () => {
        const data = await qa.vault.localAPI("/unlock", "POST", {
          password: qa.password,
        });
        return {
          data,
          pointer: await qa.get("active-vault"),
          v2: await qa.get("encrypted-v2"),
          legacy: await qa.get("encrypted"),
          original: qa.original,
        };
      });
      assert.equal(migrated.pointer.version, 2);
      assert.equal(migrated.v2.version, 2);
      assert.deepEqual(migrated.legacy, migrated.original);
      assert.deepEqual(
        await page.evaluate(() =>
          qa.vault.localAPI("/vault/legacy-envelope", "GET"),
        ),
        migrated.original,
      );
      assert.match(
        migrated.data.agentCatalog.definitions[0].prompt,
        /\.\.{100}/,
      );
      await page.evaluate(async () => {
        await qa.vault.localAPI("/lock", "POST");
        const stale = { ...qa.original, iv: "AAAAAAAAAAAAAAAA" };
        await qa.put(stale, "encrypted");
        await qa.vault.localAPI("/unlock", "POST", { password: qa.password });
      });
      assert.equal(
        await page.evaluate(
          () => qa.vault.localData().agentCatalog.definitions[0].launchName,
        ),
        "lead",
      );
      await page.evaluate(async () => {
        await qa.vault.localAPI("/lock", "POST");
        await qa.del("encrypted-v2");
      });
      const error = await page.evaluate(async () => {
        try {
          await qa.vault.localAPI("/unlock", "POST", { password: qa.password });
          return "";
        } catch (error) {
          return error.message;
        }
      });
      assert.match(error, /migration is incomplete/);
      const invalidContext = await browser.newContext(),
        invalidPage = await invalidContext.newPage();
      await invalidPage.goto(url);
      await invalidPage.waitForFunction(() => window.qa);
      const invalid = await invalidPage.evaluate(async () => {
        const envelope = await qa.seedInvalid();
        let error = "";
        try {
          await qa.vault.localAPI("/unlock", "POST", { password: qa.password });
        } catch (reason) {
          error = reason.message;
        }
        return {
          envelope,
          error,
          pointer: await qa.get("active-vault"),
          legacy: await qa.get("encrypted"),
        };
      });
      assert.match(invalid.error, /Invalid launch name/);
      assert.equal(invalid.pointer, undefined);
      assert.deepEqual(invalid.legacy, invalid.envelope);
      await invalidContext.close();
      console.log(
        `${name}: v1-to-v2 atomic namespace migration, stale old-write fence, and interrupted-pointer failure passed.`,
      );
    } finally {
      await browser.close();
    }
  }
} finally {
  await server.close();
}
