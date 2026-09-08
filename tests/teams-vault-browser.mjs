// Real editor + real encrypted vault, isolated browser storage. No live hub writes.
import { chromium, webkit } from "@playwright/test";
import { createServer } from "vite";
import assert from "node:assert/strict";
const html = `<!doctype html><html><head><link rel="stylesheet" href="/client/style.css"></head><body><main id="mode-view"></main><dialog id="dialog"></dialog><p id="notice"></p><script type="module">
import * as vault from '/client/local-vault.js';
import {createTeamsView} from '/client/teams-view.js';
await vault.localAPI('/unlock','POST',{password:'isolated team vault passphrase'});
if(!vault.localData().servers.length)await vault.localAPI('/servers','POST',{name:'Test host',host:'test.example',port:22,username:'test',mode:'ssh'});
const view=createTeamsView({getData:()=>vault.localData(),getServers:()=>vault.localData().servers,api:vault.localAPI,reloadData:async()=>{},notice:t=>document.querySelector('#notice').textContent=t,confirm:async()=>true,inspectTools:async fields=>{window.inspected=fields;return {runtime:fields.runtime,version:'test',host:'fixture host',scope:'host',commands:['git','tt'],servers:[{name:'<img src=x onerror=alert(1)>',runtimeStatus:'authenticationRequired',authStatus:'notLoggedIn',tools:['safe_tool']}],notes:['Host inventory is not a launched agent manifest.']}},newTask(){},addTeam(){},dialog(title,body){const d=document.querySelector('#dialog');d.innerHTML='<div class="dialog-head"><h2>'+title+'</h2></div>'+body;d.showModal()},closeDialog(){document.querySelector('#dialog').close()}});
view.mount(document.querySelector('#mode-view'));view.show();window.qa={vault,view};
</script></body></html>`;
const server = await createServer({
  configFile: false,
  root: process.cwd(),
  server: { host: "127.0.0.1", port: 0 },
  plugins: [
    {
      name: "team-check",
      configureServer(s) {
        s.middlewares.use("/teams-test", (req, res) => {
          res.setHeader("Content-Type", "text/html");
          res.end(html);
        });
      },
    },
  ],
});
await server.listen();
const url =
  "http://127.0.0.1:" + server.httpServer.address().port + "/teams-test";
try {
  for (const [name, engine] of [
    ["chromium", chromium],
    ["webkit", webkit],
  ]) {
    const browser = await engine.launch();
    try {
      const page = await browser.newPage({
          viewport: { width: 1050, height: 780 },
        }),
        errors = [];
      page.on("pageerror", (e) => errors.push(e.message));
      await page.goto(url);
      await page.locator("#teams-new").click();
      await page.locator("#team-name").fill("Eleven agents");
      await page.locator("#team-swarm").check();
      await page.locator("[data-field=name]").fill("planner");
      await page.locator("#team-model-choice").selectOption("gpt-6-astra");
      await page.locator(".agent-controls > summary").click();
      await page
        .locator("[data-field=permissionMode]")
        .selectOption("full-auto");
      await page.locator("[data-field=prompt]").fill("Coordinate the review.");
      for (let i = 2; i <= 11; i++) {
        await page.locator("#team-add-member").click();
        await page
          .locator("[data-field=name]")
          .fill(i === 3 ? "PLANNER" : "agent" + i);
        if (i === 2) {
          await page.locator("[data-field=runtime]").selectOption("claude");
          await page.locator("#team-model-choice").selectOption("sonnet");
        }
        if (i === 8) {
          await page.locator("#team-model-choice").selectOption("__custom");
          await page.locator("#team-model").fill("custom-model-v2");
        }
      }
      assert.equal(await page.locator("#team-add-member").isDisabled(), false);
      await page.locator("#team-orchestrator").selectOption("2");
      await page.locator("#team-form button[type=submit]").click();
      await page
        .locator("#team-error")
        .filter({ hasText: "already used" })
        .waitFor();
      assert.equal(
        await page.locator('[data-member="2"]').getAttribute("aria-pressed"),
        "true",
      );
      assert.equal(
        await page.locator("[data-field=name]").inputValue(),
        "PLANNER",
      );
      assert.equal(
        await page.evaluate(() => qa.vault.localData().teams.length),
        0,
      );
      const errorBox = await page.locator("#team-error").boundingBox();
      assert.ok(
        errorBox.y >= 0 && errorBox.y + errorBox.height <= 780,
        JSON.stringify(errorBox),
      );
      await page.screenshot({
        path: ".build/team-validation-" + name + ".png",
      });
      await page.locator("[data-field=name]").fill("reviewer");
      await page.locator("#team-form button[type=submit]").click();
      await page.locator("#dialog").waitFor({ state: "hidden" });
      let team = await page.evaluate(() => qa.vault.localData().teams[0]);
      assert.equal(team.members.length, 11);
      assert.equal(team.swarm, true);
      assert.equal(team.orchestrator, "reviewer");
      assert.equal(team.members[0].model, "gpt-6-astra");
      assert.equal(team.members[0].permissionMode, "full-auto");
      assert.equal(team.members[1].model, "sonnet");
      assert.equal(team.members[7].model, "custom-model-v2");
      await page.reload();
      await page.locator("[data-edit-team]").click();
      await page.locator('[data-member="7"]').click();
      assert.equal(
        await page.locator("#team-model").inputValue(),
        "custom-model-v2",
      );
      await page.locator('[data-member="0"]').click();
      assert.equal(
        await page.locator("#team-model-choice").inputValue(),
        "gpt-6-astra",
      );
      assert.equal(
        await page.locator("[data-field=permissionMode]").inputValue(),
        "full-auto",
      );
      await page
        .locator("#team-member-editor > details:not(.agent-controls) > summary")
        .click();
      await page.locator("[data-field=cwd]").fill("~/project");
      await page.locator('[data-member="7"]').click();
      await page.setViewportSize({ width: 390, height: 650 });
      await page.locator("#team-form button[type=submit]").click();
      await page
        .locator("#team-error")
        .filter({ hasText: "absolute" })
        .waitFor();
      assert.equal(
        await page.locator('[data-member="0"]').getAttribute("aria-pressed"),
        "true",
      );
      const mobile = await page.locator("#team-error").boundingBox();
      assert.ok(
        mobile.y >= 0 && mobile.y + mobile.height <= 650,
        JSON.stringify(mobile),
      );
      await page.locator("[data-field=cwd]").fill("/project");
      await page.locator("#team-form button[type=submit]").click();
      await page.locator("#dialog").waitFor({ state: "hidden" });
      team = await page.evaluate(() => qa.vault.localData().teams[0]);
      assert.equal(team.members[0].cwd, "/project");
      assert.equal(team.members.length, 11);
      assert.equal(team.swarm, true);
      assert.equal(team.orchestrator, "reviewer");
      await page.setViewportSize({ width: 1050, height: 780 });
      await page.locator("#teams-examples").click();
      assert.equal(await page.locator(".team-example").count(), 9);
      await page.locator("[data-example=feature]").click();
      assert.equal(
        await page.locator("[data-field=serverId]").inputValue(),
        "",
      );
      assert.ok(
        (await page.locator("[data-field=prompt]").inputValue()).length > 2500,
      );
      await page.locator("#team-form button[type=submit]").click();
      await page.locator("#dialog").waitFor({ state: "hidden" });
      const example = await page.evaluate(() =>
        qa.vault.localData().teams.find((t) => t.name === "Feature delivery"),
      );
      assert.equal(example.members.length, 4);
      assert.ok(
        example.members.every(
          (m) => m.serverId === "" && m.prompt.length > 2500,
        ),
      );
      assert.deepEqual(errors, []);
      console.log(
        name +
          ": eleven-member save, encrypted reload, per-agent preset/custom models, hidden-member validation, correction and mobile error visibility passed.",
      );
    } finally {
      await browser.close();
    }
  }
} finally {
  await server.close();
}
