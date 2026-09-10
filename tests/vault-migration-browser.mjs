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
const strictV2={...legacy,agentCatalog:{version:2,definitions:[{id:'agent_strict',revision:1,name:'Strict agent',launchName:'strict-agent',role:'Lead',serverId:'machine-demo-01',runtime:'codex',model:'gpt-5.3-codex',reasoning:'high',approvalMode:'on-request',sandboxMode:'workspace-write',permissionMode:'',allowedTools:[],run:'codex',cwd:'/synthetic/project',prompt:'Strict persisted identity'}]},teamsVersion:2,teams:[{id:'team_strict',name:'Strict team',swarm:false,orchestrator:'strict-agent',members:[{agentDefinitionId:'agent_strict'}]}]};
const invalidV2=kind=>{const value=structuredClone(strictV2);if(kind==='missing-definition-id')delete value.agentCatalog.definitions[0].id;else value.teams=[{...value.teams[0],id:'invalid/id'},{...value.teams[0],id:'invalid/id',name:'Second invalid team'}];return value};
window.qa={password,original,vault,get,put,del,async invalidV2Envelope(kind){return sealVault(invalidV2(kind),key,salt,2)},async seedInvalidV2(kind){const envelope=await sealVault(invalidV2(kind),key,salt,2);await put(envelope,'encrypted-v2');await put({version:2},'active-vault');return envelope},async seedInvalid(){const invalid={...legacy,teams:[{name:'Invalid',members:[{name:'has spaces',runtime:'codex',run:'codex'}]}]};const envelope=await sealVault(invalid,key,salt,1);await put(envelope,'encrypted');return envelope}};
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
          recovery: await qa.get("migration-source-v1"),
          original: qa.original,
        };
      });
      assert.equal(migrated.pointer.version, 2);
      assert.equal(migrated.v2.version, 2);
      assert.deepEqual(migrated.legacy, migrated.original);
      assert.deepEqual(migrated.recovery, migrated.original);
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
      });
      assert.deepEqual(
        await page.evaluate(() =>
          qa.vault.localAPI("/vault/legacy-envelope", "GET"),
        ),
        migrated.original,
      );
      await page.evaluate(async () => {
        await qa.vault.localAPI("/unlock", "POST", { password: qa.password });
      });
      assert.equal(
        await page.evaluate(
          () => qa.vault.localData().agentCatalog.definitions[0].launchName,
        ),
        "lead",
      );
      const rejectedImport = await page.evaluate(async () => {
        const before = {
          pointer: await qa.get("active-vault"),
          v2: await qa.get("encrypted-v2"),
          legacy: await qa.get("encrypted"),
          recovery: await qa.get("migration-source-v1"),
        };
        let error = "";
        try {
          await qa.vault.importBackup(
            await qa.invalidV2Envelope("missing-definition-id"),
            qa.password,
          );
        } catch (reason) {
          error = reason.message;
        }
        return {
          before,
          after: {
            pointer: await qa.get("active-vault"),
            v2: await qa.get("encrypted-v2"),
            legacy: await qa.get("encrypted"),
            recovery: await qa.get("migration-source-v1"),
          },
          error,
        };
      });
      assert.match(rejectedImport.error, /agent definition ID/);
      assert.deepEqual(rejectedImport.after, rejectedImport.before);
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
      assert.deepEqual(
        await page.evaluate(() =>
          qa.vault.localAPI("/vault/legacy-envelope", "GET"),
        ),
        migrated.original,
      );
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
          recovery: await qa.get("migration-source-v1"),
        };
      });
      assert.match(invalid.error, /Invalid launch name/);
      assert.equal(invalid.pointer, undefined);
      assert.deepEqual(invalid.legacy, invalid.envelope);
      assert.equal(invalid.recovery, undefined);
      await invalidContext.close();
      const invalidV2Context = await browser.newContext(),
        invalidV2Page = await invalidV2Context.newPage();
      await invalidV2Page.goto(url);
      await invalidV2Page.waitForFunction(() => window.qa);
      const invalidV2 = await invalidV2Page.evaluate(async () => {
        const envelope = await qa.seedInvalidV2("invalid-team-ids");
        const before = {
          pointer: await qa.get("active-vault"),
          v2: await qa.get("encrypted-v2"),
          legacy: await qa.get("encrypted"),
          recovery: await qa.get("migration-source-v1"),
        };
        let error = "";
        try {
          await qa.vault.localAPI("/unlock", "POST", {
            password: qa.password,
          });
        } catch (reason) {
          error = reason.message;
        }
        return {
          envelope,
          before,
          after: {
            pointer: await qa.get("active-vault"),
            v2: await qa.get("encrypted-v2"),
            legacy: await qa.get("encrypted"),
            recovery: await qa.get("migration-source-v1"),
          },
          error,
        };
      });
      assert.match(invalidV2.error, /Invalid team ID/);
      assert.deepEqual(invalidV2.after, invalidV2.before);
      assert.deepEqual(invalidV2.after.v2, invalidV2.envelope);
      await invalidV2Context.close();
      console.log(
        `${name}: v1-to-v2 atomic namespace migration, strict v2 identity rejection, unchanged unlock/import storage, immutable locked recovery, stale old-write fence, and interrupted-pointer failure passed.`,
      );
    } finally {
      await browser.close();
    }
  }
} finally {
  await server.close();
}
