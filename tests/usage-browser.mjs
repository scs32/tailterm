// Synthetic Projects integration with disposable browsers and local-only transport.
import assert from "node:assert/strict";
import { readFileSync, mkdtempSync, rmSync } from "node:fs";
import { execFileSync } from "node:child_process";
import { tmpdir } from "node:os";
import { join } from "node:path";
// Generate the report through the real parser/HTTP/store fixture on every run,
// so both-engine cost ordering cannot pass on hand-written completeness flags.
const reportDirectory = mkdtempSync(join(tmpdir(), "tailterm-usage-report-"));
let realReport;
try {
  const fixture =
    process.env.USAGE_REPORT_FIXTURE || join(reportDirectory, "report.json");
  if (!process.env.USAGE_REPORT_FIXTURE) {
    execFileSync(
      "go",
      [
        "-C",
        "hub",
        "test",
        "./cmd/tt",
        "-run",
        "^TestUsageRealShapedBothRuntimesProduceCompletePricedReports$",
        "-count=1",
      ],
      {
        cwd: process.cwd(),
        env: { ...process.env, USAGE_REPORT_FIXTURE: fixture },
        stdio: "pipe",
      },
    );
  }
  realReport = JSON.parse(readFileSync(fixture, "utf8"));
} finally {
  rmSync(reportDirectory, { recursive: true, force: true });
}
import { chromium, webkit } from "@playwright/test";
import { createServer } from "vite";
const html = `<!doctype html><html><head><meta charset="utf-8"><link rel="stylesheet" href="/client/style.css"></head><body><main id="projects"></main><script type="module">
import {createTasksView} from '/client/tasks-view.js';
const open={id:'tsk_1111111111111111',name:'Open synthetic',status:'open',goal:'Fixture only',createdAt:'2026-09-27T00:00:00Z',pauseState:'active',lifecycleGeneration:0};
const closed={...open,id:'tsk_2222222222222222',name:'Closed synthetic',status:'closed'};
const summary=(n,cost,complete=true)=>({state:n?'measured':'not measured',requests:n,allocatedTurns:String(n),tokens:{input:'21/2',cached:'40',output:'7/2'},measuredRequests:{input:n,cached:n},averageContext:n?'101':null,cachedShare:n?'80/101':null,pricedSubtotal:cost===null?{}:{USD:String(cost)},costComplete:cost!==null&&complete});
const item=(id,title,cost,n=1,complete=true)=>({taskId:open.id,itemId:id,title,summary:summary(n,cost,complete),phases:[{key:'a minor',label:'a minor',summary:summary(n,cost===null?null:1,complete)},{key:'z dominant',label:'z dominant',summary:summary(n,cost===null?null:2,complete)}],roles:[{key:'builder',label:'builder',summary:summary(n,cost,complete)}],models:[{key:'unknown',label:'<img src=x onerror=alert(1)>unknown',summary:summary(n,cost,complete)}],phaseRoles:[]});
const report={version:1,projectId:open.id,priceRevision:0,summary:summary(4,3),items:[item('wi_1111111111111111','Low',1),item('wi_2222222222222222','High',2),item('wi_3333333333333333','Not measured',null,0),item('wi_4444444444444444','<b>Unknown model</b>',10,1,false)],overhead:item('','Project overhead',null)};
let prices={revision:0,rows:[]};window.calls=[];window.pending=[];window.delay=false;window.offline=false;window.oldHub=false;
const client={listTasks:async()=>[open,closed],getTask:async id=>({task:id===open.id?open:closed,agents:[]}),listTeamDelivery:async()=>({entries:[],concurrencyLimit:1}),listOwnerObligations:async()=>[],capabilities:async()=>({}),subscribe:()=>({stop(){}}),cacheStatus:()=>({label:window.offline?'Saved data · offline':''}),getUsage:async(id,filters)=>{window.calls.push({id,filters});if(window.oldHub){const error=new Error('missing');error.status=404;throw error;}const data=structuredClone({...report,projectId:id});if(window.delay)return await new Promise(resolve=>window.pending.push(()=>resolve(data)));return data;},getUsagePrices:async()=>structuredClone(prices),setUsagePrices:async(id,body)=>{if(body.expectedRevision!==prices.revision){const e=new Error('conflict');e.status=409;throw e;}prices={revision:prices.revision+1,rows:body.rows};return structuredClone(prices);}};
window.fixture={client,report,prices:()=>prices};
const view=createTasksView({client:()=>client,taskHub:{hasPendingResume:()=>false,groupOf:()=>null},getTabs:()=>[],activate:()=>{},notice:()=>{},confirm:async()=>true,openBoard:()=>{},openWorkItems:()=>{},configure:()=>{}});view.mount(document.querySelector('#projects'));window.ready=view.show();window.reload=()=>view.reload();
</script></body></html>`;
const vite = await createServer({
  configFile: false,
  root: process.cwd(),
  server: { host: "127.0.0.1", port: 0 },
  plugins: [
    {
      name: "usage-fixture",
      configureServer(s) {
        s.middlewares.use("/usage-test", (_req, res) => {
          res.setHeader("Content-Type", "text/html");
          res.end(html);
        });
      },
    },
  ],
});
await vite.listen();
const origin = "http://127.0.0.1:" + vite.httpServer.address().port;
try {
  for (const engine of [chromium, webkit]) {
    const browser = await engine.launch();
    try {
      const context = await browser.newContext({
        viewport: { width: 1200, height: 900 },
      });
      await context.route("**/*", (r) =>
        new URL(r.request().url()).origin === origin ? r.continue() : r.abort(),
      );
      const page = await context.newPage(),
        errors = [];
      page.on("pageerror", (e) => errors.push(e.message));
      await page.goto(origin + "/usage-test");
      await page.evaluate(() => window.ready);
      if (!(await page.locator("[data-usage-disclosure]").count()))
        throw new Error(
          "Usage mount absent: " +
            (await page.locator("body").innerText()) +
            " errors=" +
            JSON.stringify(errors),
        );
      await page.locator("[data-usage-disclosure] > summary").click();
      await page.locator("[data-usage-item]").first().waitFor();
      let titles = await page
        .locator("[data-usage-item] > summary")
        .allTextContents();
      assert.match(titles[0], /^High/);
      assert.match(titles[1], /^Low/);
      assert.match(titles.join(" "), /Partially priced subtotal/);
      assert.match(titles.at(-1), /Project overhead/);
      await page
        .locator("[data-usage-item]")
        .first()
        .locator(":scope > summary")
        .click();
      await page
        .locator("[data-usage-group]")
        .first()
        .locator(":scope > summary")
        .click();
      assert.match(
        await page.locator("[data-usage-item]").first().innerText(),
        /average context 101 · cached input 79.2%/,
      );
      const rankedPhases = await page
        .locator("[data-usage-item]")
        .first()
        .locator("[data-usage-group]")
        .first()
        .locator("tr td:first-child")
        .allTextContents();
      assert.deepEqual(rankedPhases, ["z dominant", "a minor"]);
      assert.equal(
        await page
          .locator(".usage-item script,.usage-item b,.usage-item img")
          .count(),
        0,
      );
      await page.locator("[data-usage-prices]").click();
      await page.locator("[data-price-add]").click();
      const fields = {
        runtime: "codex",
        model: "fixture-model",
        currency: "USD",
        effectiveAt: "2026-09-27T00:00:00Z",
        "rate:input": "2",
      };
      for (const [k, v] of Object.entries(fields))
        await page.locator('[data-price-key="' + k + '"]').fill(v);
      await page.locator("[data-price-save]").click();
      await page.waitForFunction(() => window.fixture.prices().revision === 1);
      assert.equal(
        (await page.evaluate(() => window.fixture.prices())).rows[0].rates
          .input,
        "2",
      );
      // Same-project core refresh retains edited filter focus/caret and disclosure.
      const from = page.locator('[data-usage-field="from"]');
      await from.fill("2026-09-27T00:00:00Z");
      await from.focus();
      await page.evaluate(() => window.reload());
      assert.equal(await from.inputValue(), "2026-09-27T00:00:00Z");
      assert.equal(
        await from.evaluate((el) => document.activeElement === el),
        true,
      );
      await page.locator("[data-usage-refresh]").click();
      await page.waitForFunction(
        () => window.calls.at(-1).filters.from === "2026-09-27T00:00:00Z",
      );
      await page.evaluate(() => {
        window.offline = true;
      });
      await page.locator("[data-usage-refresh]").click();
      await page.waitForTimeout(30);
      assert.equal(await page.locator("[data-price-save]").isDisabled(), true);
      assert.match(
        await page.locator(".project-usage").innerText(),
        /Saved data · offline/,
      );
      await page.evaluate(() => {
        window.offline = false;
        window.delay = true;
      });
      await page.locator("[data-usage-refresh]").click();
      await page.waitForFunction(() => window.pending.length === 1);
      await page.locator(".tasks-closed > summary").click();
      await page.locator('[data-task-select="tsk_2222222222222222"]').click();
      await page.locator("[data-usage-disclosure] > summary").click();
      await page.waitForFunction(() => window.pending.length === 2);
      await page.evaluate(() => window.pending[1]());
      await page.locator("[data-usage-item]").first().waitFor();
      await page.evaluate(() => window.pending[0]());
      assert.match(
        await page.locator(".tasks-detail h2").innerText(),
        /Closed synthetic/,
      );
      await page.evaluate(() => {
        window.delay = false;
        window.oldHub = true;
      });
      await page.locator("[data-usage-refresh]").click();
      await page.waitForFunction(() =>
        document
          .querySelector(".project-usage")
          .textContent.includes("Usage unsupported"),
      );
      await page.setViewportSize({ width: 390, height: 844 });
      await page.waitForTimeout(30);
      const dimensions = await page.evaluate(() => ({
        scroll: document.documentElement.scrollWidth,
        width: window.innerWidth,
      }));
      assert.ok(
        dimensions.scroll <= dimensions.width + 1,
        JSON.stringify(dimensions),
      );
      if (realReport) {
        assert.equal(realReport.summary.state, "measured");
        assert.equal(realReport.summary.costComplete, true);
        await page.evaluate((report) => {
          window.oldHub = false;
          window.delay = false;
          Object.assign(window.fixture.report, report);
        }, realReport);
        await page.locator("[data-usage-refresh]").click();
        await page.waitForFunction(() =>
          document
            .querySelector("[data-usage-item] > summary")
            ?.textContent.startsWith("Z Codex"),
        );
        const actualTitles = await page
          .locator("[data-usage-item] > summary")
          .allTextContents();
        assert.match(actualTitles[0], /^Z Codex · Estimated/);
        assert.match(actualTitles[1], /^A Claude · Estimated/);
        assert.ok(!actualTitles.join(" ").includes("Partially priced"));
      }
      assert.deepEqual(errors, []);
      await context.close();
      console.log(
        engine.name() +
          ": Usage project integration, cost order, editor, stale/offline/closed/focus/narrow checks pass",
      );
    } finally {
      await browser.close();
    }
  }
} finally {
  await vite.close();
}
