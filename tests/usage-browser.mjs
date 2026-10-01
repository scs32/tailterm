// Synthetic Projects integration with disposable browsers and local-only transport.
import assert from "node:assert/strict";
import { readFileSync, mkdtempSync, rmSync } from "node:fs";
import { execFileSync } from "node:child_process";
import { tmpdir } from "node:os";
import { join } from "node:path";
// Generate the report through the real parser/HTTP/store fixture on every run,
// so both-engine cost ordering cannot pass on hand-written completeness flags.
const reportDirectory = mkdtempSync(join(tmpdir(), "tailterm-usage-report-"));
let realReport, realTimeReport;
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
  // The time report comes from real transcripts parsed by the relay code,
  // uploaded over HTTP and assembled by the store.
  const timeFixture =
    process.env.USAGE_TIME_REPORT_FIXTURE ||
    join(reportDirectory, "time-report.json");
  if (!process.env.USAGE_TIME_REPORT_FIXTURE) {
    execFileSync(
      "go",
      ["-C", "hub", "test", "./cmd/tt", "-run", "^TestUsageTimeCLIReport$", "-count=1"],
      {
        cwd: process.cwd(),
        env: { ...process.env, USAGE_TIME_REPORT_FIXTURE: timeFixture },
        stdio: "pipe",
      },
    );
  }
  realTimeReport = JSON.parse(readFileSync(timeFixture, "utf8"));
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
const split=(key,model,tool,waiting)=>({key,modelMs:String(model),toolMs:String(tool),waitingMs:String(waiting)});
const time={wallMs:'3600000',from:'2026-01-01T10:00:00Z',to:'2026-01-01T11:00:00Z',unmeasuredMs:'0',modelMs:'1920000',toolMs:'1020000',waitingMs:'11460000',agents:[{agentId:'agt_1111111111111111',name:'builder',role:'builder',modelMs:'480000',toolMs:'600000',waitingMs:'2520000',unmeasuredMs:'0',polls:2,pollMs:'3000'}],phases:[split('a minor',480000,120000,3000000),split('z dominant',480000,600000,2520000)],roles:[split('builder',480000,600000,2520000)],phaseRoles:[],timeline:{modelMs:'1680000',toolsOnlyMs:'900000',idleMs:'1020000',unmeasuredMs:'0'},waits:[{cause:'owner',messageSeq:12620,subject:'<b>Approve</b> the matrix <img src=x onerror=alert(1)>',awaitedBy:'builder, reviewer',ms:'5760000'},{cause:'unknown',awaitedBy:'builder',ms:'305000'}],causes:{owner:'5760000',handler:'0',teammate:'0',unknown:'305000'},polls:2,pollMs:'3000'};
const waitingOnly={...time,wallMs:'600000',modelMs:'0',toolMs:'0',waitingMs:'600000',phases:[split('waiting only',0,0,600000)],roles:[],timeline:{modelMs:'0',toolsOnlyMs:'0',idleMs:'600000',unmeasuredMs:'0'},waits:[],polls:0,pollMs:'0'};
const report={version:1,timeVersion:1,projectId:open.id,priceRevision:0,summary:summary(4,3),items:[{...item('wi_1111111111111111','Low',1),time:waitingOnly},{...item('wi_2222222222222222','High',2),time},item('wi_3333333333333333','Not measured',null,0),item('wi_4444444444444444','<b>Unknown model</b>',10,1,false)],overhead:item('','Project overhead',null)};
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
      const usageStyle = await page.evaluate(() => {
        const items = document.querySelector(".usage-items");
        const item = document.querySelector(".usage-item");
        return {
          layout: getComputedStyle(items).display,
          border: getComputedStyle(item).borderTopWidth,
          radius: getComputedStyle(item).borderTopLeftRadius,
        };
      });
      assert.deepEqual(usageStyle, { layout: "grid", border: "1px", radius: "4px" });
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
      // Time next to tokens: wall time, the three shares, the team timeline
      // and poll requests, inside the item's existing disclosure.
      const high = page.locator('[data-usage-item="wi_2222222222222222"]');
      const timeText = await high.locator("[data-usage-time]").innerText();
      assert.match(
        timeText,
        /Wall time 1h 0m · model 13\.3% · tool 7\.1% · waiting 79\.6% · 2 poll requests \(3s\)/,
      );
      assert.match(
        timeText,
        /Team timeline: some model working 46\.7% · only tools running 25\.0% · nobody active 28\.3%/,
      );
      // Top waits name the cause, the awaited Board message and who waited.
      const waits = await high.locator("[data-usage-waits] tr").allInnerTexts();
      assert.equal(waits.length, 2);
      assert.match(waits[0], /Waiting on owner/);
      assert.match(waits[0], /<b>Approve<\/b> the matrix <img src=x onerror=alert\(1\)> #12620/);
      assert.match(waits[0], /1h 36m · builder, reviewer/);
      assert.match(waits[1], /Waiting on unknown[\s\S]*Nothing the Board shows[\s\S]*5m 5s · builder/);
      // Phase and role rows carry their time beside their tokens.
      const phaseRows = await high
        .locator("[data-usage-group]")
        .first()
        .locator("tr")
        .allInnerTexts();
      assert.match(phaseRows[0], /z dominant[\s\S]*Time: model 8m 0s · tool 10m 0s · waiting 42m 0s/);
      assert.match(phaseRows[1], /a minor[\s\S]*Time: model 8m 0s · tool 2m 0s · waiting 50m 0s/);
      await high.locator("[data-usage-group]").nth(1).locator(":scope > summary").click();
      assert.match(
        await high.locator("[data-usage-group]").nth(1).innerText(),
        /builder[\s\S]*Time: model 8m 0s · tool 10m 0s · waiting 42m 0s/,
      );
      // A phase with time and no metered request still gets its row.
      const low = page.locator('[data-usage-item="wi_1111111111111111"]');
      await low.locator(":scope > summary").click();
      await low.locator("[data-usage-group]").first().locator(":scope > summary").click();
      assert.match(
        await low.locator("[data-usage-group]").first().innerText(),
        /waiting only[\s\S]*Time: model 0s · tool 0s · waiting 10m 0s/,
      );
      // An item with no spans says so.
      assert.equal(
        await page
          .locator('[data-usage-item="wi_3333333333333333"] [data-usage-time]')
          .textContent(),
        "Time not measured",
      );
      assert.equal(await page.locator("[data-usage-time]").count(), 5);
      if (process.env.USAGE_SCREENSHOT_DIR)
        await page.screenshot({
          path: join(
            process.env.USAGE_SCREENSHOT_DIR,
            `usage-time-${engine.name()}.png`,
          ),
          fullPage: true,
        });
      // No new control and no new disclosure.
      assert.equal(await page.locator(".project-usage button").count(), 2);
      assert.equal(await high.locator("details").count(), 4);
      // Narrow layout with the time section open.
      await page.setViewportSize({ width: 390, height: 844 });
      await page.waitForTimeout(30);
      const narrowTime = await page.evaluate(() => ({
        scroll: document.documentElement.scrollWidth,
        width: window.innerWidth,
      }));
      assert.ok(narrowTime.scroll <= narrowTime.width + 1, JSON.stringify(narrowTime));
      await page.setViewportSize({ width: 1200, height: 900 });
      // A report from a hub that reports no time renders as before.
      const beforeTime = await page.evaluate(() => {
        const saved = structuredClone(window.fixture.report);
        delete window.fixture.report.timeVersion;
        for (const row of window.fixture.report.items) delete row.time;
        return saved;
      });
      await page.locator("[data-usage-refresh]").click();
      await page.waitForFunction(
        () => document.querySelectorAll("[data-usage-time]").length === 0,
      );
      const withoutTime = await high.innerText();
      assert.match(withoutTime, /average context 101 · cached input 79.2%/);
      assert.ok(!/Time|Wall time|Waiting on/.test(withoutTime), withoutTime);
      assert.equal(await page.locator("[data-usage-waits]").count(), 0);
      await page.evaluate((saved) => {
        Object.assign(window.fixture.report, saved);
      }, beforeTime);
      await page.locator("[data-usage-refresh]").click();
      await page.waitForFunction(
        () => document.querySelectorAll("[data-usage-time]").length === 5,
      );
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
      if (realTimeReport) {
        const measured = realTimeReport.items[0];
        assert.equal(realTimeReport.timeVersion, 1);
        assert.equal(measured.time.wallMs, "4812000");
        await page.evaluate((report) => {
          Object.assign(window.fixture.report, report);
        }, realTimeReport);
        await page.locator("[data-usage-refresh]").click();
        const real = page.locator(`[data-usage-item="${measured.itemId}"]`);
        await real.waitFor();
        await real.locator(":scope > summary").click();
        assert.match(
          await real.locator("[data-usage-time]").innerText(),
          /Wall time 1h 20m · model 0\.8% · tool 0\.7% · waiting 98\.5% · 5 poll requests \(11s\)[\s\S]*Team timeline: some model working 0\.8% · only tools running 0\.7% · nobody active 98\.5%/,
        );
        const owner = measured.time.waits[0];
        assert.equal(owner.cause, "owner");
        const realWaits = await real.locator("[data-usage-waits] tr").allInnerTexts();
        assert.ok(
          realWaits[0].includes(`Approve the <matrix> plan? #${owner.messageSeq}`) &&
            realWaits[0].includes("1h 13m · builder"),
          realWaits[0],
        );
        assert.equal(await page.locator(".usage-item matrix").count(), 0);
        await real.locator("[data-usage-group]").first().locator(":scope > summary").click();
        assert.match(
          await real.locator("[data-usage-group]").first().innerText(),
          /build[\s\S]*Time: model 38s · tool 32s · waiting 1h 19m/,
        );
        // Project overhead had no spans.
        assert.equal(
          await page.locator('[data-usage-item="overhead"] [data-usage-time]').textContent(),
          "Time not measured",
        );
        const narrowReal = await page.evaluate(() => ({
          scroll: document.documentElement.scrollWidth,
          width: window.innerWidth,
        }));
        assert.ok(narrowReal.scroll <= narrowReal.width + 1, JSON.stringify(narrowReal));
      }
      assert.deepEqual(errors, []);
      await context.close();
      console.log(
        engine.name() +
          ": Usage project integration, cost order, time, waits, editor, stale/offline/closed/focus/narrow checks pass",
      );
    } finally {
      await browser.close();
    }
  }
} finally {
  await vite.close();
}
