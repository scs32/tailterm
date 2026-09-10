// Real Agents/Teams editors + encrypted vault in isolated Chromium/WebKit contexts.
import { chromium, webkit } from "@playwright/test";
import { createServer } from "vite";
import assert from "node:assert/strict";

const html = `<!doctype html><html><head><meta charset="utf-8"><link rel="stylesheet" href="/client/style.css"></head><body data-mode="agents"><div id="workspace"><aside></aside><main><header></header><section id="mode-view"></section></main></div><dialog id="dialog"></dialog><p id="notice"></p><script type="module">
import * as vault from '/client/local-vault.js';
import {createAgentsView} from '/client/agents-view.js';
import {createTeamsView} from '/client/teams-view.js';
await vault.localAPI('/unlock','POST',{password:'isolated agent library passphrase'});
if(!vault.localData().servers.length)await vault.localAPI('/servers','POST',{name:'Synthetic machine with a realistically long display name',host:'test.example',port:22,username:'test',mode:'ssh'});
const host={getData:()=>vault.localData(),getServers:()=>vault.localData().servers,currentServer:()=>vault.localData().servers[0],api:vault.localAPI,reloadData:async()=>{},notice:t=>document.querySelector('#notice').textContent=t,confirm:async()=>true,inspectTools:async()=>({runtime:'codex',version:'fixture'}),newTask(){},addTeam(){},openAgents(){show('agents')},dialog(title,body){const d=document.querySelector('#dialog');d.innerHTML='<div class="dialog-head"><h2>'+title+'</h2></div>'+body;d.showModal()},closeDialog(){document.querySelector('#dialog').close()}};
const agents=createAgentsView(host),teams=createTeamsView(host),view=document.querySelector('#mode-view');
function show(mode){agents.hide();teams.hide();document.body.dataset.mode=mode;view.replaceChildren();const active=mode==='agents'?agents:teams;active.mount(view);active.show()}
show('agents');window.qa={vault,show};
</script></body></html>`;

const server = await createServer({
  configFile: false,
  root: process.cwd(),
  server: { host: "127.0.0.1", port: 0 },
  plugins: [
    {
      name: "agents-library-check",
      configureServer(instance) {
        instance.middlewares.use("/agents-test", (_req, res) => {
          res.setHeader("Content-Type", "text/html");
          res.end(html);
        });
      },
    },
  ],
});
await server.listen();
const url = `http://127.0.0.1:${server.httpServer.address().port}/agents-test`;
try {
  for (const [name, engine] of [
    ["chromium", chromium],
    ["webkit", webkit],
  ]) {
    const browser = await engine.launch();
    try {
      const context = await browser.newContext({
        viewport: { width: 1120, height: 820 },
      });
      const page = await context.newPage();
      const errors = [];
      page.on("pageerror", (error) => errors.push(error.message));
      await page.goto(url);
      await page.locator("#agents-new").click();
      await page
        .locator('[data-field="name"]')
        .fill("Implementation agent with long reusable context");
      await page.locator('[data-field="launchName"]').fill("builder");
      await page.locator('[data-field="role"]').fill("Builder");
      await page
        .locator("#agent-library-model-choice")
        .selectOption("gpt-6-astra");
      await page.locator(".agent-controls > summary").click();
      await page.locator('[data-field="reasoning"]').selectOption("high");
      await page
        .locator('[data-field="approvalMode"]')
        .selectOption("on-request");
      await page
        .locator('[data-field="sandboxMode"]')
        .selectOption("workspace-write");
      await page
        .locator("#agent-library-cwd")
        .fill("/synthetic/project with spaces");
      await page
        .locator('[data-field="prompt"]')
        .fill(
          "Review exact work-item identity and preserve partial launch state.\n" +
            "Long content remains readable without overlapping controls. ".repeat(
              60,
            ),
        );
      await page.locator('#agent-library-form button[type="submit"]').click();
      await page.waitForTimeout(250);
      assert.equal(
        await page
          .locator("#agent-library-error")
          .textContent()
          .catch(() => ""),
        "",
      );
      await page
        .locator(".agents-detail h2")
        .filter({ hasText: "Implementation agent" })
        .waitFor({ timeout: 5000 });
      const stored = await page.evaluate(() => qa.vault.localData());
      assert.equal(stored.agentCatalog.version, 2);
      assert.equal(stored.agentCatalog.definitions[0].reasoning, "high");
      assert.equal(stored.teamsVersion, 2);
      assert.doesNotMatch(JSON.stringify(stored.teams), /gpt-6-astra/);
      await page.evaluate(() => qa.show("teams"));
      await page.locator("#teams-new").click();
      await page.locator("#team-name").fill("Long-content delivery team");
      await page.locator('[data-field="alias"]').fill("lead-builder");
      await page.locator('[data-field="role"]').fill("Orchestrator");
      await page.locator('#team-form button[type="submit"]').click();
      await page
        .locator(".teams-detail h2")
        .filter({ hasText: "Long-content delivery team" })
        .waitFor();
      const finalData = await page.evaluate(() => qa.vault.localData());
      assert.equal(
        finalData.teams[0].members[0].agentDefinitionId,
        finalData.agentCatalog.definitions[0].id,
      );
      assert.equal(finalData.teams[0].members[0].alias, "lead-builder");
      const fences = await page.evaluate(async () => {
        const original = qa.vault.localData().agentCatalog.definitions[0];
        await qa.vault.localAPI("/agents", "POST", {
          ...original,
          prompt: "A central edit for future launches",
        });
        let stale = "",
          referenced = "";
        try {
          await qa.vault.localAPI("/agents", "POST", {
            ...original,
            prompt: "stale overwrite",
          });
        } catch (error) {
          stale = error.message;
        }
        try {
          await qa.vault.localAPI("/agents/" + original.id, "DELETE");
        } catch (error) {
          referenced = error.message;
        }
        return {
          stale,
          referenced,
          current: qa.vault.localData().agentCatalog.definitions[0],
        };
      });
      assert.match(fences.stale, /changed in another editor/);
      assert.match(fences.referenced, /used by teams/);
      assert.equal(fences.current.revision, 2);
      for (const width of [1120, 680]) {
        await page.setViewportSize({ width, height: 760 });
        const geometry = await page.evaluate(() => {
          const view = document
            .querySelector("#mode-view")
            .getBoundingClientRect();
          const rail = document
            .querySelector(".board-rail")
            .getBoundingClientRect();
          const thread = document
            .querySelector(".board-thread")
            .getBoundingClientRect();
          const actions = [
            ...document.querySelectorAll(".view-actions button"),
          ].map((button) => button.getBoundingClientRect());
          return {
            view: { left: view.left, right: view.right },
            rail: { left: rail.left, right: rail.right, bottom: rail.bottom },
            thread: { left: thread.left, right: thread.right, top: thread.top },
            actions: actions.map((rect) => ({
              left: rect.left,
              right: rect.right,
              top: rect.top,
              bottom: rect.bottom,
            })),
          };
        });
        assert.ok(
          geometry.rail.left >= geometry.view.left - 1 &&
            geometry.thread.right <= geometry.view.right + 1,
          JSON.stringify({ width, geometry }),
        );
        if (width > 760)
          assert.ok(
            geometry.rail.right <= geometry.thread.left + 1,
            "rail and detail must not overlap",
          );
        for (let i = 0; i < geometry.actions.length; i++)
          for (let j = i + 1; j < geometry.actions.length; j++) {
            const a = geometry.actions[i],
              b = geometry.actions[j];
            assert.ok(
              a.right <= b.left ||
                b.right <= a.left ||
                a.bottom <= b.top ||
                b.bottom <= a.top,
              "action controls overlap",
            );
          }
      }
      assert.deepEqual(errors, []);
      await context.close();
      console.log(
        `${name}: Agents catalog, referenced Teams, long content, native controls, and wide/narrow non-overlap passed.`,
      );
    } finally {
      await browser.close();
    }
  }
} finally {
  await server.close();
}
