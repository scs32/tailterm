import { openVault } from "../client/vault-crypto.js";
// Real vault crypto + real Go hub, isolated browser storage and database.
import { chromium, webkit } from "@playwright/test";
import { createServer as createVite } from "vite";
import { createServer } from "node:net";
import { spawn } from "node:child_process";
import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { once } from "node:events";
import assert from "node:assert/strict";
const state = await mkdtemp(path.join(tmpdir(), "tt-profile-test-"));
const reserve = createServer();
reserve.listen(0, "127.0.0.1");
await once(reserve, "listening");
const port = reserve.address().port;
await new Promise((r) => reserve.close(r));
const backend = spawn(".build/ttbin/tailterm-hub-test", {
  env: {
    ...process.env,
    TAILTERM_STATE: state,
    TAILTERM_DEV_LISTEN: "127.0.0.1:" + port,
    TAILTERM_TCP_LISTEN: "",
  },
  stdio: "ignore",
});
const html = `<!doctype html><html><head><meta charset="utf-8"><link rel="stylesheet" href="/client/style.css"></head><body><div id="app"><div id="workspace"><aside><button id="profile-sync"><span class="nav-label">Profile sync</span></button></aside><main><header>Profile check</header><div id="status"></div></main></div><dialog id="dialog"></dialog></div><script type="module">
import * as vault from '/client/local-vault.js';import {createProfileSync} from '/client/profile-sync.js';import {confirmDialog} from '/client/confirm-dialog.js';
let online=false, sync, requests=0, unavailable=false;
const host={getIPN:()=>online?{fetch:async(url,init)=>{requests++;if(unavailable)throw new Error("Host temporarily unavailable");return fetch(url.replace('http://profile-fixture:18765',location.origin+'/profile-hub'),init)}}:null,getPeers:()=>[{name:'profile-fixture.',online:true}],getData:()=>vault.localData(),getAppearance:()=>({theme:'default',font:'system',idleMinutes:15}),connect(){},notice:text=>document.querySelector('#status').textContent=text,download(){},confirm:(title,message)=>confirmDialog({title,message}),reloadData:async()=>{},dialog:(title,body)=>{const d=document.querySelector('#dialog');if(d.open)d.close();d.innerHTML='<div class="dialog-head"><h2>'+title+'</h2><button id="dialog-close">×</button></div>'+body;d.querySelector('#dialog-close').onclick=()=>d.close();d.showModal()}};
window.qa={vault,unavailable:value=>unavailable=value,async unlock(username,password){await vault.localAPI('/unlock','POST',{username,password});sync=createProfileSync(host,vault);document.querySelector('#profile-sync').onclick=()=>sync.show();},online:async(value)=>{online=value;return sync.connected()},show:()=>sync.show(),data:()=>vault.localData(),requests:()=>requests,stop:()=>sync.stop(),async addServer(name,password){await vault.localAPI('/servers','POST',{name,host:'test.example',port:22,username:'test',mode:'ssh'});await vault.rememberCredential(vault.localData().servers.at(-1).id,{password})},async rename(name){const s=vault.localData().servers[0];return vault.localAPI('/servers','POST',{...s,name})},async configure(){await vault.localAPI('/hub','POST',{url:location.origin+'/profile-hub',token:'fixture-token-not-live'})}};
</script></body></html>`;
const vite = await createVite({
  configFile: false,
  root: process.cwd(),
  server: {
    host: "127.0.0.1",
    port: 0,
    proxy: {
      "/profile-hub": {
        target: "http://127.0.0.1:" + port,
        rewrite: (p) => p.replace("/profile-hub", ""),
      },
    },
  },
  plugins: [
    {
      name: "profile-harness",
      configureServer(s) {
        s.middlewares.use("/profile-test", (req, res) => {
          res.setHeader("Content-Type", "text/html");
          res.end(html);
        });
      },
    },
  ],
});
await vite.listen();
const origin = "http://127.0.0.1:" + vite.httpServer.address().port;
const pass = "A sufficiently long profile passphrase";
try {
  for (let i = 0; i < 50; i++) {
    try {
      if ((await fetch("http://127.0.0.1:" + port + "/v1/profiles")).ok) break;
    } catch {}
    await new Promise((r) => setTimeout(r, 100));
  }
  for (const [engineName, engine] of [
    ["chromium", chromium],
    ["webkit", webkit],
  ]) {
    const browser = await engine.launch();
    const contexts = [];
    const errors = [];
    try {
      const fresh = async (password = pass) => {
        const context = await browser.newContext({
          viewport: { width: 1100, height: 820 },
        });
        contexts.push(context);
        const page = await context.newPage();
        page.on("pageerror", (e) => errors.push(e.message));
        await page.goto(origin + "/profile-test");
        await page.waitForFunction(() => !!window.qa);
        await page.evaluate(
          ([u, p]) => qa.unlock(u, p),
          [engineName, password],
        );
        return page;
      };
      const a = await fresh();
      assert.equal(
        await a.evaluate(() => qa.requests()),
        0,
        "must not contact server before Tailscale",
      );
      await a.evaluate(async () => {
        await qa.addServer("Original server", "PRIVATE SSH PASSWORD");
        await qa.configure();
        await qa.vault.localAPI("/teams", "POST", {
          name: "Portable team",
          members: [
            {
              name: "planner",
              serverId: qa.data().servers[0].id,
              runtime: "codex",
              model: "test-model",
              role: "Planner",
              prompt: "Review the objective",
            },
          ],
        });
        await qa.vault.localAPI("/tailscale/state", "PUT", {
          state: { node: "device-a" },
        });
        await qa.online(true);
        qa.show();
      });
      await a.locator("button[value=enable]").click();
      await a.waitForFunction(() => qa.data().profile.revision === 1);
      assert.equal(await a.evaluate(() => qa.data().profile.dirty), false);
      await a.screenshot({
        path: ".build/profile-sync-" + engineName + ".png",
      });
      const b = await fresh();
      assert.equal(await b.evaluate(() => qa.data().servers.length), 0);
      await b.evaluate(async () => {
        qa.unavailable(true);
        await qa.online(true);
        await qa.online(false);
        qa.unavailable(false);
        await qa.vault.localAPI("/tailscale/state", "PUT", {
          state: { node: "device-b" },
        });
        await qa.online(true);
      });
      await b.waitForFunction(() => qa.data().servers.length === 1);
      assert.equal(
        await b.evaluate(() => qa.data().servers[0].name),
        "Original server",
      );
      assert.equal(
        await b.evaluate(
          () => qa.vault.credentials(qa.data().servers[0].id).server.password,
        ),
        "PRIVATE SSH PASSWORD",
      );
      assert.equal(
        await b.evaluate(
          async () =>
            (await qa.vault.localAPI("/tailscale/claim", "POST")).node,
        ),
        "device-b",
      );
      assert.equal(await b.evaluate(() => qa.data().profile.revision), 1);
      assert.equal(
        await b.evaluate(() => qa.data().teams[0].members[0].model),
        "test-model",
      );
      assert.equal(
        await b.evaluate(() => qa.data().teams[0].members[0].prompt),
        "Review the objective",
      );
      await a.evaluate(async () => {
        await qa.online(false);
        await qa.vault.localAPI("/sessions", "POST", {
          serverId: qa.data().servers[0].id,
          name: "saved-session",
        });
        if (!qa.data().profile.dirty)
          throw new Error("New session bookmark must queue profile sync");
        await qa.rename("Changed on A");
      });
      assert.equal(await a.evaluate(() => qa.data().profile.dirty), true);
      await a.evaluate(() => qa.online(true));
      await a.waitForFunction(() => qa.data().profile.revision === 2);
      await b.evaluate(() => qa.online(true));
      await b.waitForFunction(
        () => qa.data().servers[0].name === "Changed on A",
      );
      await a.evaluate(async () => {
        await qa.online(false);
        await qa.rename("New A change");
      });
      await b.evaluate(async () => {
        await qa.online(false);
        await qa.rename("Concurrent B change");
      });
      await a.evaluate(() => qa.online(true));
      await a.waitForFunction(() => qa.data().profile.revision === 3);
      await b.evaluate(async () => {
        await qa.online(true);
        qa.show();
      });
      await b.locator("#profile-use-remote").waitFor();
      assert.equal(
        await b.evaluate(() => qa.data().servers[0].name),
        "Concurrent B change",
        "must retain unsynced local edits",
      );
      await b.locator("#profile-use-remote").click();
      await b.locator(".confirmation-dialog [data-confirm]").click();
      await b.waitForFunction(
        () => qa.data().servers[0].name === "New A change",
      );
      assert.equal(await b.evaluate(() => qa.data().profile.dirty), false);
      const recovery = await b.evaluate(() => qa.vault.exportProfileRecovery());
      assert.equal(recovery.format, "tailserve-vault");
      const recovered = await openVault(recovery, pass);
      assert.equal(recovered.data.servers[0].name, "Concurrent B change");
      assert.deepEqual(recovered.data.tailscale, {});
      // Lock/reopen keeps the association and pending edits, while discarding keys.
      await a.evaluate(async () => {
        await qa.online(false);
        await qa.rename("Persisted offline edit");
        qa.stop();
        await qa.vault.localAPI("/lock", "POST");
      });
      await a.reload();
      await a.waitForFunction(() => !!window.qa);
      await a.evaluate(([u, p]) => qa.unlock(u, p), [engineName, pass]);
      assert.equal(await a.evaluate(() => qa.data().profile.dirty), true);
      assert.equal(
        await a.evaluate(() => qa.data().servers[0].name),
        "Persisted offline edit",
      );
      await a.evaluate(() => qa.online(true));
      await a.waitForFunction(() => qa.data().profile.revision === 4);
      const wrong = await fresh("A different but long passphrase");
      await wrong.evaluate(() => qa.online(true));
      assert.equal(await wrong.evaluate(() => qa.data().servers.length), 0);
      await b.evaluate(() => qa.show());
      await b.locator("#profile-disconnect").click();
      await b.waitForFunction(() => !qa.data().profile.hub);
      assert.equal(
        await b.evaluate(() => qa.data().profile.autoRestore),
        false,
      );
      await b.evaluate(() => qa.online(true));
      assert.ok(
        !(await b.evaluate(() => qa.data().profile.hub)),
        "stopped sync must stay stopped",
      );
      await a.setViewportSize({ width: 390, height: 740 });
      await a.evaluate(() => qa.show());
      const box = await a.locator("#dialog").boundingBox();
      assert.ok(box.width <= 390 && box.x >= 0);
      await a.screenshot({
        path: ".build/profile-sync-mobile-" + engineName + ".png",
      });
      assert.deepEqual(errors, []);
      console.log(
        engineName +
          ": local-first startup, username restore, encrypted credentials, separate device identities, automatic sync, offline changes, conflicts, recovery, wrong passphrase and disconnect passed.",
      );
    } finally {
      for (const c of contexts) await c.close();
      await browser.close();
    }
  }
} finally {
  await vite.close();
  backend.kill("SIGTERM");
  await once(backend, "exit");
  await rm(state, { recursive: true, force: true });
}
