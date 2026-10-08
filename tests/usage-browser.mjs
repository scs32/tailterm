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
// The scroll variant differs only in layout and transport: the real rules that
// make the Projects detail and rail scroll are scoped to #mode-view, the rail
// lists enough projects to overflow, and subscribe keeps its callback so the
// test can deliver a hub message the way production does.
const fixturePage = (scroll) => `<!doctype html><html><head><meta charset="utf-8"><link rel="stylesheet" href="/client/style.css"></head><body${scroll ? ' data-mode="tasks"' : ""}>${scroll ? '<main id="mode-view" style="height:600px"></main>' : '<main id="projects"></main>'}<script type="module">
import {createTasksView} from '/client/tasks-view.js';
const open={id:'tsk_1111111111111111',name:'Open synthetic',status:'open',goal:'Fixture only',createdAt:'2026-09-27T00:00:00Z',pauseState:'active',lifecycleGeneration:0};
const closed={...open,id:'tsk_2222222222222222',name:'Closed synthetic',status:'closed'};
const summary=(n,cost,complete=true)=>({state:n?'measured':'not measured',requests:n,allocatedTurns:String(n),tokens:{input:'21/2',cached:'40',output:'7/2'},measuredRequests:{input:n,cached:n},averageContext:n?'101':null,cachedShare:n?'80/101':null,pricedSubtotal:cost===null?{}:{USD:String(cost)},costComplete:cost!==null&&complete});
const item=(id,title,cost,n=1,complete=true)=>({taskId:open.id,itemId:id,title,summary:summary(n,cost,complete),phases:[{key:'a minor',label:'a minor',summary:summary(n,cost===null?null:1,complete)},{key:'z dominant',label:'z dominant',summary:summary(n,cost===null?null:2,complete)}],roles:[{key:'builder',label:'builder',summary:summary(n,cost,complete)}],models:[{key:'unknown',label:'<img src=x onerror=alert(1)>unknown',summary:summary(n,cost,complete)}],phaseRoles:[]});
const split=(key,model,tool,waiting)=>({key,modelMs:String(model),toolMs:String(tool),waitingMs:String(waiting)});
const time={wallMs:'3600000',from:'2026-01-01T10:00:00Z',to:'2026-01-01T11:00:00Z',unmeasuredMs:'0',modelMs:'1920000',toolMs:'1020000',waitingMs:'11460000',agents:[{agentId:'agt_1111111111111111',name:'builder',role:'builder',modelMs:'480000',toolMs:'600000',waitingMs:'2520000',unmeasuredMs:'0',polls:2,pollMs:'3000'}],phases:[split('a minor',480000,120000,3000000),split('z dominant',480000,600000,2520000)],roles:[split('builder',480000,600000,2520000)],phaseRoles:[],timeline:{modelMs:'1680000',toolsOnlyMs:'900000',idleMs:'1020000',unmeasuredMs:'0'},waits:[{cause:'owner',messageSeq:12620,subject:'<b>Approve</b> the matrix <img src=x onerror=alert(1)>',awaitedBy:'builder, reviewer',ms:'5760000'},{cause:'unknown',awaitedBy:'builder',ms:'305000'}],causes:{owner:'5760000',handler:'0',teammate:'0',unknown:'305000'},polls:2,pollMs:'3000'};
const waitingOnly={...time,wallMs:'600000',modelMs:'0',toolMs:'0',waitingMs:'600000',phases:[split('waiting only',0,0,600000)],roles:[],timeline:{modelMs:'0',toolsOnlyMs:'0',idleMs:'600000',unmeasuredMs:'0'},waits:[],polls:0,pollMs:'0'};
// Budgets: no estimate, measured with a markup basis, estimate without usage, and a partial actual. Project overhead has none.
const estimate=(tokens,basis)=>({tokens,basis,setAt:'2026-10-07T16:00:00Z',setBy:{agentId:'agt_1111111111111111',node:'fixture',user:'owner'}});
const budgets={wi_1111111111111111:{actualTokens:'4200000',actualState:'measured'},wi_2222222222222222:{estimate:estimate(12000000,'<img src=x onerror=alert(1)>Small, 2 paths, median of 8 <b>Small</b> items'),actualTokens:'15300000',actualState:'measured',ratio:'51/40'},wi_3333333333333333:{estimate:estimate(5000000,'Small, 1 path'),actualTokens:'0',actualState:'not measured'},wi_4444444444444444:{estimate:estimate(12000000,'Planned, 9 paths'),actualTokens:'30600001/2',actualState:'partial',ratio:'30600001/24000000'}};
const budgeted=row=>({...row,budget:budgets[row.itemId]});
const report={version:1,timeVersion:1,projectId:open.id,priceRevision:0,summary:summary(4,3),items:[{...item('wi_1111111111111111','Low',1),time:waitingOnly},{...item('wi_2222222222222222','High',2),time},item('wi_3333333333333333','Not measured',null,0),item('wi_4444444444444444','<b>Unknown model</b>',10,1,false)].map(budgeted),overhead:item('','Project overhead',null)};
let prices={revision:0,rows:[]};window.calls=[];window.pending=[];window.delay=false;window.offline=false;window.oldHub=false;
const scroll=${scroll ? "true" : "false"};
const fillers=scroll?Array.from({length:40},(_,i)=>({...open,id:'tsk_'+String(i+10).padStart(16,'0'),name:'Filler '+i})):[];
const team=scroll?Array.from({length:12},(_,i)=>({id:'agt_'+String(i+10).padStart(16,'0'),name:'member-'+i,status:'running',host:'fixture',session:'s'+i,activity:{},createdAt:'2026-09-27T00:00:00Z'})):[];
const client={listTasks:async()=>[...fillers,open,closed],getTask:async id=>({task:id===open.id?open:id===closed.id?closed:fillers.find(t=>t.id===id),agents:id===open.id?team:[]}),listTeamDelivery:async()=>({entries:[],concurrencyLimit:1}),listOwnerObligations:async()=>[],capabilities:async()=>({}),subscribe:(_id,cb)=>{window.push=cb;return{stop(){}}},cacheStatus:()=>({label:window.offline?'Saved data · offline':''}),getUsage:async(id,filters)=>{window.calls.push({id,filters});if(window.oldHub){const error=new Error('missing');error.status=404;throw error;}const data=structuredClone({...report,projectId:id});if((filters.from||'').startsWith('2099')){/* like the hub: a window with no requests empties the rows and keeps each lifetime budget */for(const row of data.items){row.summary=summary(0,null);row.phases=[];row.roles=[];row.models=[];row.phaseRoles=[];delete row.time;}}if(window.delay)return await new Promise(resolve=>window.pending.push(()=>resolve(data)));return data;},getUsagePrices:async()=>structuredClone(prices),setUsagePrices:async(id,body)=>{if(body.expectedRevision!==prices.revision){const e=new Error('conflict');e.status=409;throw e;}prices={revision:prices.revision+1,rows:body.rows};return structuredClone(prices);}};
window.fixture={client,report,open,prices:()=>prices};
const view=createTasksView({client:()=>client,taskHub:{hasPendingResume:()=>false,groupOf:()=>null},getTabs:()=>[],activate:()=>{},notice:()=>{},confirm:async()=>true,openBoard:()=>{},openWorkItems:()=>{},configure:()=>{}});view.mount(document.querySelector(scroll?'#mode-view':'#projects'));window.ready=view.show();window.reload=()=>view.reload();
</script></body></html>`;
const vite = await createServer({
  configFile: false,
  root: process.cwd(),
  server: { host: "127.0.0.1", port: 0 },
  plugins: [
    {
      name: "usage-fixture",
      configureServer(s) {
        s.middlewares.use("/usage-test", (req, res) => {
          res.setHeader("Content-Type", "text/html");
          res.end(fixturePage(/[?&]scroll=1/.test(req.url || "")));
        });
      },
    },
  ],
});
await vite.listen();
const origin = "http://127.0.0.1:" + vite.httpServer.address().port;
// A refresh of the Projects view must leave every scrolled container where the
// user left it. Its own pages keep the fake clock away from the checks above.
async function scrollSection(browser, name) {
  const context = await browser.newContext({
    viewport: { width: 1200, height: 900 },
  });
  await context.route("**/*", (r) =>
    new URL(r.request().url()).origin === origin ? r.continue() : r.abort(),
  );
  const errors = [];
  const start = Date.parse("2026-09-27T12:00:00Z");
  // The Projects clock is created in show(), so the fake clock is installed
  // before the page loads; paused, only the test moves time.
  const openPage = async () => {
    const page = await context.newPage();
    page.on("pageerror", (e) => errors.push(e.message));
    await page.clock.install({ time: start });
    await page.goto(origin + "/usage-test?scroll=1");
    await page.clock.pauseAt(start + 1000);
    await page.evaluate(() => window.ready);
    await page.locator("[data-usage-disclosure]").waitFor();
    return page;
  };
  const positions = (page) =>
    page.evaluate(() => ({
      detail: document.querySelector(".tasks-detail").scrollTop,
      rail: document.querySelector(".board-rail").scrollTop,
    }));
  // Every refresh the page can receive: the 30 s clock, a hub message through
  // the subscription, and a direct reload. Each settles its queued repaints.
  const refreshes = {
    "the 30 s clock tick": (page) => page.clock.fastForward(30001),
    "a pushed hub message": (page) => page.evaluate(() => window.push({})),
    "a reload": (page) => page.evaluate(() => window.reload()),
  };
  const settle = async (page) => {
    await page.clock.runFor(5);
    await page.evaluate(() => 0);
  };
  const near = (actual, expected, what) =>
    assert.ok(
      Math.abs(actual.detail - expected.detail) <= 1 &&
        Math.abs(actual.rail - expected.rail) <= 1,
      `${name}: ${what}: scrollTop ${JSON.stringify(actual)}, expected ${JSON.stringify(expected)}`,
    );

  const page = await openPage();
  await page.locator("[data-usage-disclosure] > summary").click();
  const item = page.locator("[data-usage-item]").first();
  await item.waitFor();
  await item.locator(":scope > summary").click();
  await item.locator("[data-usage-group]").first().locator(":scope > summary").click();
  const scroller = await page.evaluate(() => {
    const detail = document.querySelector(".tasks-detail");
    const rail = document.querySelector(".board-rail");
    return {
      detailOverflow: getComputedStyle(detail).overflowY,
      detailRoom: detail.scrollHeight - detail.clientHeight,
      railOverflow: getComputedStyle(rail).overflowY,
      railRoom: rail.scrollHeight - rail.clientHeight,
      page: document.scrollingElement.scrollHeight - window.innerHeight,
    };
  });
  assert.equal(scroller.detailOverflow, "auto", JSON.stringify(scroller));
  assert.equal(scroller.railOverflow, "auto", JSON.stringify(scroller));
  assert.ok(scroller.detailRoom > 200, JSON.stringify(scroller));
  assert.ok(scroller.railRoom > 200, JSON.stringify(scroller));
  assert.ok(scroller.page <= 0, JSON.stringify(scroller));
  // Bring the Usage summary to the top of the detail section.
  await page.evaluate(() => {
    const detail = document.querySelector(".tasks-detail");
    const usage = document.querySelector("[data-usage-disclosure]");
    detail.scrollTop +=
      usage.getBoundingClientRect().top - detail.getBoundingClientRect().top;
    document.querySelector(".board-rail").scrollTop = 150;
  });
  const scrolled = await positions(page);
  assert.ok(scrolled.detail > 0 && scrolled.rail > 0, JSON.stringify(scrolled));
  for (const [what, refresh] of Object.entries(refreshes)) {
    const goal = "Goal changed before " + what;
    await page.evaluate((text) => {
      window.fixture.open.goal = text;
      document.querySelector(".tasks-detail").dataset.stale = "1";
    }, goal);
    await refresh(page);
    await settle(page);
    const after = await page.evaluate(() => ({
      rebuilt: !document.querySelector(".tasks-detail").dataset.stale,
      head: document.querySelector(".tasks-detail .board-head").textContent,
      usage: document.querySelector("[data-usage-disclosure]").open,
      item: document.querySelector("[data-usage-item]").open,
      group: document.querySelector("[data-usage-item] [data-usage-group]").open,
    }));
    assert.equal(after.rebuilt, true, `${name}: no rebuild after ${what}`);
    near(await positions(page), scrolled, "after " + what);
    assert.ok(after.head.includes(goal), `${name}: stale after ${what}: ${after.head}`);
    assert.deepEqual(
      { usage: after.usage, item: after.item, group: after.group },
      { usage: true, item: true, group: true },
      `${name}: disclosures after ${what}`,
    );
  }
  // A refresh during a wheel gesture over the detail section waits for the
  // gesture to go idle, so the element under the gesture is not replaced.
  const held = await page.evaluate(async () => {
    const detail = document.querySelector(".tasks-detail");
    window.heldDetail = detail;
    window.fixture.open.goal = "Goal changed during a wheel gesture";
    detail
      .querySelector("[data-usage-disclosure]")
      .dispatchEvent(new WheelEvent("wheel", { bubbles: true, deltaY: 40 }));
    detail.scrollTop += 40;
    await window.reload();
    return {
      connected: detail.isConnected,
      head: detail.querySelector(".board-head").textContent,
      detail: detail.scrollTop,
      rail: document.querySelector(".board-rail").scrollTop,
    };
  });
  await page.clock.runFor(100);
  assert.equal(
    await page.evaluate(() => window.heldDetail.isConnected),
    true,
    `${name}: the detail section was replaced during a wheel gesture`,
  );
  assert.equal(held.connected, true);
  assert.doesNotMatch(held.head, /during a wheel gesture/);
  await page.clock.runFor(200);
  await settle(page);
  assert.deepEqual(
    await page.evaluate(() => ({
      connected: window.heldDetail.isConnected,
      shown: document
        .querySelector(".tasks-detail .board-head")
        .textContent.includes("Goal changed during a wheel gesture"),
    })),
    { connected: false, shown: true },
    `${name}: the held refresh was not shown once the gesture went idle`,
  );
  near(await positions(page), held, "after the held refresh");
  await page.close();

  // A page that was never scrolled stays at the top.
  const top = await openPage();
  for (const [what, refresh] of Object.entries(refreshes)) {
    await refresh(top);
    await settle(top);
    assert.deepEqual(
      await positions(top),
      { detail: 0, rail: 0 },
      `${name}: an unscrolled page moved after ${what}`,
    );
  }
  assert.deepEqual(errors, []);
  await context.close();
  console.log(
    name +
      ": Projects scroll kept across clock, hub message and reload; disclosures, fresh content, wheel hold and top-of-page checks pass",
  );
}
try {
  // USAGE_ENGINE=chromium|webkit runs one engine; the default runs both.
  const engines = [chromium, webkit].filter(
    (engine) =>
      !process.env.USAGE_ENGINE || process.env.USAGE_ENGINE === engine.name(),
  );
  assert.ok(engines.length, "USAGE_ENGINE must be chromium or webkit");
  for (const engine of engines) {
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
      // Hold the initial read across real Projects reloads. Repaint must keep
      // that request and deliver its result to the newest usage mount.
      await page.evaluate(() => {
        window.delay = true;
      });
      await page.locator("[data-usage-disclosure] > summary").click();
      await page.waitForFunction(() => window.pending.length === 1);
      for (let i = 0; i < 5; i++) await page.evaluate(() => window.reload());
      assert.deepEqual(
        await page.evaluate(() => ({
          calls: window.calls.length,
          pending: window.pending.length,
        })),
        { calls: 1, pending: 1 },
        "same-project reloads must retain the initial usage request",
      );
      assert.match(
        await page.locator(".project-usage [role=status]").innerText(),
        /Loading usage/,
      );
      await page.evaluate(() => {
        window.delay = false;
        window.pending.shift()();
      });
      await page.locator("[data-usage-item]").first().waitFor();
      assert.doesNotMatch(
        await page.locator(".project-usage [role=status]").innerText(),
        /Loading usage/,
      );
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
      // Budget: each item's estimate against its lifetime actual, in the
      // item's summary line and again with its basis inside the item.
      const budgetOf = (id) =>
        page.locator(`[data-usage-item="${id}"] [data-usage-budget]`);
      const summaryOf = (id) =>
        page.locator(`[data-usage-item="${id}"] > summary`).innerText();
      const budgetTexts = async () => ({
        high: await summaryOf("wi_2222222222222222"),
        low: await summaryOf("wi_1111111111111111"),
        unmeasured: await summaryOf("wi_3333333333333333"),
        partial: await summaryOf("wi_4444444444444444"),
        overhead: await summaryOf("overhead"),
      });
      const wantBudgets = {
        high: "High · Estimated USD 2 · Estimate 12.0M · lifetime actual 15.3M · 1.28×",
        low: "Low · Estimated USD 1 · No estimate · lifetime actual 4.2M",
        unmeasured:
          "Not measured · Tokens only · Estimate 5.0M · lifetime actual not measured",
        partial:
          "<b>Unknown model</b> · Partially priced subtotal USD 10 · Estimate 12.0M · lifetime actual at least 15.3M (partial) · at least 1.28×",
        // No budget key: the row is as it was before budgets existed.
        overhead: "Project overhead · Tokens only",
      };
      assert.deepEqual(await budgetTexts(), wantBudgets);
      // A partial actual never shows a bare ratio.
      assert.ok(!/\(partial\) · \d/.test(wantBudgets.partial));
      assert.equal(
        await budgetOf("wi_2222222222222222").innerText(),
        "Estimate 12.0M · lifetime actual 15.3M · 1.28×\nEstimate basis: <img src=x onerror=alert(1)>Small, 2 paths, median of 8 <b>Small</b> items",
      );
      assert.equal(
        await budgetOf("wi_1111111111111111").innerText(),
        "No estimate · lifetime actual 4.2M",
      );
      assert.equal(await page.locator("[data-usage-budget]").count(), 4);
      assert.equal(await budgetOf("overhead").count(), 0);
      assert.equal(
        await page
          .locator(".usage-item script,.usage-item b,.usage-item img")
          .count(),
        0,
      );
      // The role rows of the expanded item stay beside its budget.
      assert.match(
        await high.locator("[data-usage-group]").nth(1).innerText(),
        /Roles · wi_2222222222222222[\s\S]*builder/,
      );
      // From/To changes the rows below; the budget is lifetime and stays.
      const filterFrom = page.locator('[data-usage-field="from"]');
      await filterFrom.fill("2099-01-01T00:00:00Z");
      await page.locator("[data-usage-refresh]").click();
      await page.waitForFunction(
        () =>
          document.querySelector('[data-usage-item="wi_2222222222222222"] > p')
            ?.textContent === "Not measured",
      );
      assert.equal(
        (await page.evaluate(() => window.calls.at(-1).filters)).from,
        "2099-01-01T00:00:00Z",
      );
      assert.equal(await high.locator("[data-usage-group] tr").count(), 0);
      assert.deepEqual(await budgetTexts(), {
        ...wantBudgets,
        high: "High · Tokens only · Estimate 12.0M · lifetime actual 15.3M · 1.28×",
        low: "Low · Tokens only · No estimate · lifetime actual 4.2M",
        partial:
          "<b>Unknown model</b> · Tokens only · Estimate 12.0M · lifetime actual at least 15.3M (partial) · at least 1.28×",
      });
      assert.match(
        await budgetOf("wi_2222222222222222").innerText(),
        /^Estimate 12\.0M · lifetime actual 15\.3M · 1\.28×/,
      );
      await filterFrom.fill("");
      await page.locator("[data-usage-refresh]").click();
      await page.waitForFunction(
        () => document.querySelectorAll("[data-usage-time]").length === 5,
      );
      assert.deepEqual(await budgetTexts(), wantBudgets);
      assert.match(
        await high.locator("[data-usage-group]").nth(1).innerText(),
        /builder[\s\S]*Time: model 8m 0s · tool 10m 0s · waiting 42m 0s/,
      );
      // A report from a hub that sends no budget renders as before.
      const beforeBudget = await page.evaluate(() => {
        const saved = structuredClone(window.fixture.report);
        for (const row of window.fixture.report.items) delete row.budget;
        return saved;
      });
      await page.locator("[data-usage-refresh]").click();
      await page.waitForFunction(
        () => document.querySelectorAll("[data-usage-budget]").length === 0,
      );
      assert.deepEqual(await budgetTexts(), {
        high: "High · Estimated USD 2",
        low: "Low · Estimated USD 1",
        unmeasured: "Not measured · Tokens only",
        partial: "<b>Unknown model</b> · Partially priced subtotal USD 10",
        overhead: "Project overhead · Tokens only",
      });
      assert.doesNotMatch(
        await high.innerText(),
        /Estimate \d|Estimate basis|No estimate|lifetime/,
      );
      await page.evaluate((saved) => {
        Object.assign(window.fixture.report, saved);
      }, beforeBudget);
      await page.locator("[data-usage-refresh]").click();
      await page.waitForFunction(
        () => document.querySelectorAll("[data-usage-budget]").length === 4,
      );
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
        window.fixture.report.items[1].title = "Stale explicit refresh";
      });
      await page.locator("[data-usage-refresh]").click();
      await page.waitForFunction(() => window.pending.length === 1);
      await page.evaluate(() => {
        window.fixture.report.items[1].title = "Latest explicit refresh";
      });
      await page.locator("[data-usage-refresh]").click();
      await page.waitForFunction(() => window.pending.length === 2);
      const refreshCalls = await page.evaluate(() => window.calls.length);
      for (let i = 0; i < 3; i++) await page.evaluate(() => window.reload());
      assert.equal(
        await page.evaluate(() => window.calls.length),
        refreshCalls,
      );
      await page.evaluate(() => window.pending[1]());
      await page.waitForFunction(() =>
        document
          .querySelector(".usage-items")
          .textContent.includes("Latest explicit refresh"),
      );
      await page.evaluate(() => window.pending[0]());
      assert.doesNotMatch(
        await page.locator(".usage-items").innerText(),
        /Stale explicit refresh/,
      );
      await page.evaluate(() => {
        window.pending = [];
        window.fixture.report.items[1].title = "Stale open project";
      });
      await page.locator("[data-usage-refresh]").click();
      await page.waitForFunction(() => window.pending.length === 1);
      await page.evaluate(() => {
        window.fixture.report.items[1].title = "Closed project usage";
      });
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
      assert.match(
        await page.locator(".usage-items").innerText(),
        /Closed project usage/,
      );
      assert.doesNotMatch(
        await page.locator(".usage-items").innerText(),
        /Stale open project/,
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
          ": Usage project integration, cost order, budget, time, waits, editor, stale/offline/closed/focus/narrow checks pass",
      );
      await scrollSection(browser, engine.name());
    } finally {
      await browser.close();
    }
  }
} finally {
  await vite.close();
}
