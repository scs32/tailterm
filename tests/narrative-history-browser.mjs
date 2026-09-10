import { chromium, webkit, expect } from "@playwright/test";
import assert from "node:assert/strict";
import { createServer } from "vite";

const taskId = "tsk_1111111111111111";
const itemId = "wi_2222222222222222";
const artifactId = "nart_3333333333333333";
const currentArtifactId = "nart_6666666666666666";
const reportId = "nrpt_4444444444444444";
const reportBody = "Complete report line.\n".repeat(500);
const html = `<!doctype html><html><head><meta charset="utf-8"><link rel="stylesheet" href="/client/style.css"><link rel="stylesheet" href="/client/work-items.css"></head><body><main id="mode-view"></main><dialog id="dialog"></dialog><script type="module">
import {createWorkItemsView} from '/client/work-items-view.js';
const task={id:'${taskId}',name:'Isolated narrative project',goal:'fixture',status:'open'};
const item={id:'${itemId}',taskId:task.id,kind:'feature',title:'Safe durable narrative',status:'done',priority:'normal',revision:3,scopeRevision:2,completionReport:{reportId:'${reportId}',version:1,digest:'abc123',scopeRevision:2}};
const drafts=JSON.parse(localStorage.getItem('narrative-drafts')||'{}');window.pwned=false;window.retractedFetches=0;
const client={base:location.origin,token:'fixture',cacheStatus:()=>({label:'Isolated data'}),subscribe:()=>({stop(){}}),listTasks:async()=>[task],listWorkItems:async()=>({items:[item],next:0}),
 listWorkItemRevisions:async()=>({revisions:[{...item,itemId:item.id,revision:3,changeKind:'updated',provenance:'native',updatedAt:'2026-09-09T00:00:00Z',description:'Retained script-like text without execution'}],nextAfter:0}),
 listWorkItemHistoryGaps:async()=>({gaps:[],nextAfter:0}),listWorkItemMessages:async()=>({links:[{itemRevision:3,revisionCoverage:'verified',source:true,message:{seq:606,taskId:task.id,text:'Original <img src=x onerror=window.pwned=true> request',createdAt:'2026-09-08T00:00:00Z'}}],nextAfter:0}),
 getNarrativeOverview:async()=>({item,history:{complete:true},completionReport:item.completionReport,latestReport:{reportId:'${reportId}',version:2,digest:'def456',scopeRevision:2},coverage:[],defaultGaps:[{source:'pr',scope:'No capture declaration has been submitted.',captureState:'not-ingested',knownGaps:['source history is not captured'],unknownExtent:true,assessment:'unverified'},{source:'ci',scope:'No capture declaration has been submitted.',captureState:'not-ingested',knownGaps:['source history is not captured'],unknownExtent:true,assessment:'unverified'}]}),
 listNarrativeArtifacts:async()=>({artifacts:[{artifactId:'${artifactId}',namespace:'support',sourceId:'stale',latest:{artifactId:'${artifactId}',version:2,title:'Stale artifact',captureState:'stored-content'}},{artifactId:'${currentArtifactId}',namespace:'support',sourceId:'fixture',latest:{artifactId:'${currentArtifactId}',version:2,title:'Unsafe-looking artifact',captureState:'stored-content'}}]}),listNarrativeArtifactVersions:async(_task,_item,artifact)=>artifact==='${artifactId}'?new Promise((_,reject)=>setTimeout(()=>reject(new Error('stale artifact failure')),60)):({versions:[{artifactId:'${currentArtifactId}',version:1},{artifactId:'${currentArtifactId}',version:2,supersedesVersion:1}]}),listNarrativeLinks:async()=>({links:[{linkId:'nlnk_5555555555555555',revision:1,action:'link',relationship:'supports',featureRevision:3,target:{kind:'message',taskId:task.id,messageSeq:99},createdBy:{caller:{node:'fixture'}}},{linkId:'nlnk_5555555555555555',revision:2,action:'retract',relationship:'supports',featureRevision:3,reason:'wrong source',target:{kind:'message',taskId:task.id,messageSeq:99},createdBy:{caller:{node:'fixture'}}}]}),listMessages:async()=>{window.retractedFetches++;return[]},listNarrativeCoverage:async()=>({coverage:[{coverageId:'ncov_7777777777777777',revision:1,source:'pr',scope:'Selected submitted PR body only.',captureState:'stored-content',capturedIds:['body@abc'],knownGaps:['reviews not supplied'],unknownExtent:true,asOf:'2026-09-09T02:00:00Z',assessment:'independently-verified',assessmentText:'Exact submitted body compared.',evidenceReferences:[{kind:'artifact-version',artifactId:'${currentArtifactId}',version:2,label:'submitted body'}],assessmentBy:{caller:{user:'verifier'}},createdBy:{caller:{user:'declarer'}}}]}),listNarrativeReports:async()=>({reports:[{reportId:'${reportId}',version:1,digest:'abc123',scopeRevision:2},{reportId:'${reportId}',version:2,digest:'def456',scopeRevision:2}]}),listNarrativeTimeline:async()=>({entries:[{seq:1,kind:'artifact',objectId:'${currentArtifactId}',version:2,captureState:'stored-content',createdAt:'2026-09-09T01:00:00Z'}]}),
 getNarrativeReportVersion:async(_task,_item,_report,version)=>({reportId:'${reportId}',version,digest:version===1?'abc123':'def456',scopeRevision:2,createdBy:{caller:{user:'reporter'}},sections:{requestedOutcome:version===1?'Original completion outcome':'Latest corrected outcome',deliveredWork:${JSON.stringify(reportBody)},verification:'Chromium and WebKit isolated reader checks.',limitations:'No remote source claim.',remainingWork:'Release separately sequenced.'},references:[{kind:'message',taskId:task.id,messageSeq:606,label:'original request'},{kind:'external',sourceId:'pr',locator:'https://example.invalid/pr/1',label:'durable reference'}]}),
 getNarrativeArtifactVersion:async()=>({artifactId:'${currentArtifactId}',version:2,title:'Unsafe-looking artifact',provenance:'manual-submission',availability:'last-observed',sourceTime:'2026-09-09T00:30:00Z',originalAuthor:{user:'source-author'},ingestedBy:{caller:{user:'ingester'}},ingestedAt:'2026-09-09T01:00:00Z',contentDigest:'def456',content:'<img src=x onerror=window.pwned=true>\\njavascript:alert(1)'})};
const dialogNode=document.querySelector('#dialog');const closeDialog=()=>{dialogNode.close();dialogNode.replaceChildren()};const dialog=(title,body)=>{if(dialogNode.open)dialogNode.close();dialogNode.innerHTML='<div class="dialog-head"><h2>'+title+'</h2><button id="dialog-close">×</button></div>'+body;dialogNode.querySelector('#dialog-close').onclick=closeDialog;dialogNode.showModal()};
const saveDrafts=()=>localStorage.setItem('narrative-drafts',JSON.stringify(drafts));const draftPersistence={load:async(_,id)=>structuredClone(drafts[id]||null),save:async draft=>{drafts[draft.id]=structuredClone(draft);saveDrafts()},remove:async(_,id)=>{delete drafts[id];saveDrafts()}};
const view=createWorkItemsView({kind:'feature',client:()=>client,dialog,closeDialog,notice(){},configure(){},openBoard(){},draftPersistence});view.mount(document.querySelector('#mode-view'));await view.show();const match=location.hash.match(/^#feature-history\\/(tsk_[0-9a-f]{16})\\/(wi_[0-9a-f]{16})$/);if(match)await view.openNarrative(match[2],false);window.qa={view,draftCount:()=>Object.keys(drafts).length};
</script></body></html>`;

const server = await createServer({
  configFile: false,
  root: process.cwd(),
  server: { host: "127.0.0.1", port: 0 },
  plugins: [
    {
      name: "narrative-browser-fixture",
      configureServer(vite) {
        vite.middlewares.use("/narrative-history", (_req, res) => {
          res.setHeader("content-type", "text/html");
          res.end(html);
        });
      },
    },
  ],
});
await server.listen();
const origin = `http://127.0.0.1:${server.httpServer.address().port}/narrative-history`;
const results = [];
try {
  for (const engine of [chromium, webkit]) {
    const browser = await engine.launch();
    const context = await browser.newContext({
      viewport: { width: 1000, height: 760 },
    });
    const page = await context.newPage();
    page.on("pageerror", (error) =>
      console.error(engine.name() + " pageerror: " + error.message),
    );
    const external = [];
    await page.route("**/*", (route) => {
      const url = new URL(route.request().url());
      if (url.origin === new URL(origin).origin) route.continue();
      else {
        external.push(url.href);
        route.abort();
      }
    });
    try {
      await page.goto(origin);
      await page.waitForFunction(() => Boolean(window.qa));
      await expect(
        page.getByRole("button", { name: "History & report" }),
      ).toBeVisible();
      await page.locator("[data-item-edit]").click();
      await page
        .locator("#work-item-description")
        .fill("unsent retained draft");
      await page.waitForFunction(() => qa.draftCount() === 1);
      await page.locator("#dialog-close").click();
      await page.getByRole("button", { name: "History & report" }).click();
      await expect(page.locator(".feature-report")).toContainText(
        "Latest corrected outcome",
      );
      await expect(page.locator(".feature-completion-pin")).toContainText(
        "Completion pin",
      );
      await expect(page.locator(".feature-report")).toContainText(
        "Complete report line.",
      );
      assert.ok(
        (await page.locator(".feature-report").innerText()).length > 8192,
      );
      await expect(
        page
          .locator(".feature-coverage .feature-gap")
          .filter({ hasText: "source history is not captured" })
          .first(),
      ).toBeVisible();
      await expect(page.locator(".feature-coverage")).toContainText(
        "Declared by declarer; assessment attributed to verifier",
      );
      await expect(page.locator(".feature-coverage")).toContainText(
        "Captured IDs: body@abc",
      );
      await expect(page.locator(".feature-link-history")).toContainText(
        "retract · supports",
      );
      assert.equal(await page.evaluate(() => window.retractedFetches), 0);
      await page.locator('[data-report-id][data-report-version="1"]').click();
      await expect(page.locator("[data-feature-report-detail]")).toContainText(
        "Original completion outcome",
      );
      assert.equal(await page.evaluate(() => window.pwned), false);
      assert.equal(external.length, 0);
      await page.locator("[data-narrative-artifact]").nth(0).click();
      await page.locator("[data-narrative-artifact]").nth(1).click();
      await expect(page.locator(".feature-artifact-detail")).toContainText(
        "<img src=x onerror=window.pwned=true>",
      );
      await expect(page.locator(".feature-artifact-detail")).toContainText(
        "Original author: source-author; ingested by: ingester",
      );
      await page.waitForTimeout(80);
      await expect(
        page.locator("[data-narrative-artifact-detail]"),
      ).not.toContainText("stale artifact failure");
      assert.equal(await page.evaluate(() => window.pwned), false);
      assert.equal(external.length, 0);
      assert.match(page.url(), /#feature-history\//);
      await page.reload();
      await page.waitForFunction(() => Boolean(window.qa));
      await expect(page.locator(".feature-report")).toContainText(
        "Latest corrected outcome",
      );
      await expect(page.locator(".feature-chronology")).toContainText(
        "Board message #606",
      );
      await page.locator("#dialog-close").click();
      await page.locator("[data-item-edit]").click();
      await expect(page.locator("#work-item-description")).toHaveValue(
        "unsent retained draft",
      );
      results.push(
        engine.name() +
          ": safe report, reload, completed access, coverage gaps and draft retention",
      );
    } finally {
      await context.close();
      await browser.close();
    }
  }
} finally {
  await server.close();
}
for (const result of results) console.log(result);
