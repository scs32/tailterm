// Read-only delivery UI fixture. No live hub, project, or browser profile.
import assert from "node:assert/strict";
import { chromium, webkit } from "@playwright/test";
import { createServer } from "vite";

const html = `<!doctype html><html><head><link rel="stylesheet" href="/client/style.css"><link rel="stylesheet" href="/client/work-items.css"></head><body><main id="delivery"></main><script type="module">
import {renderTeamDelivery} from '/client/team-delivery-view.js';
const queue={concurrencyLimit:2,entries:[
 {itemId:'wi_aaaaaaaaaaaaaaaa',state:'running',ownership:['src/a'],handlerId:'agt_handler_a',acceptance:{branch:'feature/a',commit:'c'.repeat(40)}},
 {itemId:'wi_bbbbbbbbbbbbbbbb',state:'queued',ownership:['src/a/child'],blockedBy:['tqe_aaaaaaaaaaaaaaaa'],blockReason:'ownership overlap'},
 {itemId:'wi_cccccccccccccccc',state:'finished',ownership:['src/c'],handlerId:'agt_handler_c',integration:{repository:'/fixture/git',baseCommit:'a'.repeat(40),worktree:'/fixture/builder-c',branch:'feature/c',commit:'b'.repeat(40),evidence:'item=C;close=receipt'}}
]};
const agents=[{id:'agt_lead_a',name:'Lead A',itemLead:true,workItem:{itemId:'wi_aaaaaaaaaaaaaaaa'}},{id:'agt_handler_a',name:'Handler A'},{id:'agt_handler_c',name:'Handler C'}];
document.querySelector('#delivery').innerHTML=renderTeamDelivery(queue,agents);
window.fixture={queue,agents,rerender(){document.querySelector('#delivery').innerHTML=renderTeamDelivery(queue,agents)}};
</script></body></html>`;

const vite = await createServer({
  configFile: false,
  root: process.cwd(),
  server: { host: "127.0.0.1", port: 0 },
  plugins: [{ name: "parallel-delivery-fixture", configureServer(server) {
    server.middlewares.use("/parallel-delivery-test", (_req, res) => {
      res.setHeader("Content-Type", "text/html");
      res.end(html);
    });
  } }],
});
await vite.listen();
const origin = `http://127.0.0.1:${vite.httpServer.address().port}`;
try {
  for (const engine of [chromium, webkit]) {
    const browser = await engine.launch();
    try {
      const context = await browser.newContext({ viewport: { width: 1280, height: 800 } });
      await context.route("**/*", (route) => new URL(route.request().url()).origin === origin ? route.continue() : route.abort());
      const page = await context.newPage();
      const errors = [];
      page.on("pageerror", (error) => errors.push(error.message));
      await page.goto(origin + "/parallel-delivery-test");
      await page.locator('[data-testid="team-delivery-entry"]').first().waitFor();
      assert.equal(await page.locator('[data-testid="team-delivery-entry"]').count(), 3);
      assert.match(await page.locator('[data-testid="team-delivery-panel"]').innerText(), /Limit 2/);
      assert.match(await page.locator('[data-testid="team-delivery-entry"]').nth(1).innerText(), /Waiting for tqe_aaaaaaaaaaaaaaaa/);
      assert.match(await page.locator('[data-testid="team-delivery-entry"]').nth(0).innerText(), /Lead Lead A · Handler Handler A/);
      assert.match(await page.locator('[data-testid="team-delivery-entry"]').nth(0).innerText(), /Accepted feature\/a @ c{40} · waiting for team cleanup/);
      assert.match(await page.locator('[data-testid="team-ready-to-integrate"]').innerText(), /feature\/c @ b{40}/);
      assert.match(await page.locator('[data-testid="team-ready-to-integrate"]').innerText(), /\/fixture\/builder-c/);
      assert.equal(await page.locator('[data-testid="team-delivery-panel"] button').count(), 0);
      await page.evaluate(() => {
        fixture.queue.entries[2].state = "running";
        fixture.rerender();
      });
      assert.equal(await page.locator('[data-testid="team-ready-to-integrate"]').count(), 0);
      assert.deepEqual(errors, []);
      await context.close();
      console.log(`${engine.name()}: delivery ownership, blocker, item roles and gated integration receipt rendered`);
    } finally {
      await browser.close();
    }
  }
} finally {
  await vite.close();
}
