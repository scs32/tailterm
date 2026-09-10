// Acceptance for wi_f44641a045d49ded revision 2, work order #1551.
// Uses the actual shared work-items view, synthetic records, and disposable
// browser contexts. Set HISTORY_WIDTH_BASELINE=1 to serve the exact baseline
// work-items.css from the authorized commit and assert the known failure.
import { chromium, webkit, expect } from "@playwright/test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { mkdirSync, readFileSync } from "node:fs";
import { createServer } from "vite";

const baselineCommit = "58f9185981625b148b30ad4b5070a1a658e17f77";
const expectBaseline = process.env.HISTORY_WIDTH_BASELINE === "1";
const workItemsCSS = expectBaseline
  ? execFileSync("git", ["show", `${baselineCommit}:client/work-items.css`], {
      encoding: "utf8",
    })
  : readFileSync(new URL("../client/work-items.css", import.meta.url), "utf8");
const artifactDir = new URL("../.build/history-width/", import.meta.url);
mkdirSync(artifactDir, { recursive: true });

const scenarios = [
  { name: "desktop-1366", width: 1366, height: 900, deviceScaleFactor: 1 },
  { name: "desktop-1024", width: 1024, height: 768, deviceScaleFactor: 1 },
  { name: "narrow-640", width: 640, height: 720, deviceScaleFactor: 1 },
  { name: "narrow-390", width: 390, height: 720, deviceScaleFactor: 1 },
  // This models a 1366x900 display at 200% zoom: half the CSS viewport with
  // two device pixels per CSS pixel. It is emulation, not native browser zoom.
  {
    name: "zoom-200-equivalent",
    width: 683,
    height: 450,
    deviceScaleFactor: 2,
  },
];

const html = `<!doctype html><html><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<link rel="stylesheet" href="/client/style.css">
<link rel="stylesheet" href="/qa/work-items.css">
<style>html,body{height:100%;margin:0}#mode-view{display:flex;height:100%}</style>
</head><body><main id="mode-view"></main><dialog id="dialog"></dialog>
<script type="module">
import {createWorkItemsView} from '/client/work-items-view.js';
import {installDialogSelectInteraction} from '/client/dialog-interaction.js';
const kind=new URLSearchParams(location.search).get('kind')||'bug';
const task={id:'tsk_1111111111111111',name:'Synthetic history project',goal:'Isolated browser fixture',status:'open',orchestrator:'lead'};
const longID='wi_'+('abcdef0123456789'.repeat(24));
const longURL='https://example.invalid/history/'+('revision-segment-'.repeat(28));
const longDescription=Array.from({length:48},(_,index)=>'Synthetic revision paragraph '+String(index+1).padStart(2,'0')+' preserves every stored character. '+longID+' '+longURL).join('\\n');
const longMessage='Synthetic explicitly linked message preserves '+('unbroken-message-identifier-'.repeat(28))+' without using live project data.';
const item={id:'wi_2222222222222222',taskId:task.id,kind,title:'Synthetic '+kind+' with complete retained history',description:longDescription,status:'in_progress',priority:'normal',revision:14,scopeRevision:1};
const revisions=Array.from({length:14},(_,offset)=>({...item,itemId:item.id,revision:offset+1,title:'Synthetic '+kind+' revision '+(offset+1),description:'REVISION-'+(offset+1)+'\\n'+longDescription,changeKind:offset?'updated':'created',provenance:'native',updatedAt:'2026-09-10T'+String(offset).padStart(2,'0')+':00:00Z'}));
const linkedMessage={itemRevision:14,revisionCoverage:'verified',source:true,message:{seq:1552,taskId:task.id,from:{user:'synthetic-owner'},text:longMessage,createdAt:'2026-09-10T16:29:26Z'}};
const state={boardOpens:0};
const client={base:'isolated://history-width',token:'synthetic-non-secret',cacheStatus:()=>({label:'Isolated fixture'}),subscribe:()=>({stop(){}}),
 async listTasks(){return [{...task}]},async listWorkItems(){return {items:[{...item}],next:0}},
 async listWorkItemRevisions(){return {revisions:structuredClone(revisions),nextAfter:0}},
 async listWorkItemHistoryGaps(){return {gaps:[],nextAfter:0}},
 async listWorkItemMessages(_taskId,_itemId,{revision}={}){return {links:revision?[structuredClone({...linkedMessage,itemRevision:revision})]:[structuredClone(linkedMessage)],nextAfter:0}},
 async getNarrativeOverview(){return {item:{...item},history:{complete:true},completionReport:null,latestReport:null,coverage:[],defaultGaps:[]}},
 async listNarrativeArtifacts(){return {artifacts:[],nextCursor:''}},async listNarrativeLinks(){return {links:[],nextCursor:''}},
 async listNarrativeCoverage(){return {coverage:[],nextCursor:''}},async listNarrativeReports(){return {reports:[],nextCursor:''}},
 async listNarrativeTimeline(){return {entries:[],nextCursor:''}}
};
const dialogNode=document.querySelector('#dialog');
const closeDialog=()=>{dialogNode.close();dialogNode.replaceChildren()};
const dialog=(title,body)=>{if(dialogNode.open)dialogNode.close();installDialogSelectInteraction(dialogNode);dialogNode.innerHTML='<div class="dialog-head"><h2 id="dialog-title">'+title+'</h2><button id="dialog-close" aria-label="Close dialog">×</button></div>'+body;dialogNode.setAttribute('aria-labelledby','dialog-title');dialogNode.querySelector('#dialog-close').onclick=closeDialog;dialogNode.showModal()};
const view=createWorkItemsView({kind,client:()=>client,dialog,closeDialog,notice(){},configure(){},openBoard(){state.boardOpens++}});
view.mount(document.querySelector('#mode-view'));await view.show(task.id);
window.qa={state,longDescription,longMessage,item,view};
</script></body></html>`;

const server = await createServer({
  configFile: false,
  root: process.cwd(),
  server: { host: "127.0.0.1", port: 0 },
  plugins: [
    {
      name: "work-item-history-width-fixture",
      configureServer(vite) {
        vite.middlewares.use("/qa/work-items.css", (_request, response) => {
          response.setHeader("Content-Type", "text/css");
          response.end(workItemsCSS);
        });
        vite.middlewares.use("/history-width", (_request, response) => {
          response.setHeader("Content-Type", "text/html");
          response.end(html);
        });
      },
    },
  ],
});
await server.listen();
const origin = `http://127.0.0.1:${server.httpServer.address().port}`;

async function horizontalReadability(page) {
  return page.evaluate(() => {
    const dialog = document.querySelector("#dialog");
    const detail = dialog.querySelector("[data-history-detail]");
    const description = detail.querySelector("pre");
    const messages = [
      ...detail.querySelectorAll(".work-item-history-messages button"),
    ];
    const bounds = dialog.getBoundingClientRect();
    const detailBounds = detail.getBoundingClientRect();
    const textFits = (element, containerBounds) => {
      const walker = document.createTreeWalker(element, NodeFilter.SHOW_TEXT);
      let node;
      while ((node = walker.nextNode())) {
        if (!node.textContent) continue;
        const range = document.createRange();
        range.selectNodeContents(node);
        for (const rect of range.getClientRects()) {
          if (
            rect.width &&
            (rect.left < containerBounds.left - 1 ||
              rect.right > containerBounds.right + 1)
          )
            return false;
        }
      }
      return true;
    };
    return {
      viewportWidth: innerWidth,
      documentOverflow: document.documentElement.scrollWidth - innerWidth,
      dialog: {
        left: bounds.left,
        right: bounds.right,
        width: bounds.width,
        clientWidth: dialog.clientWidth,
        scrollWidth: dialog.scrollWidth,
      },
      detail: {
        clientWidth: detail.clientWidth,
        scrollWidth: detail.scrollWidth,
      },
      description: {
        clientWidth: description.clientWidth,
        scrollWidth: description.scrollWidth,
        textFits: textFits(description, detailBounds),
      },
      messages: messages.map((message) => ({
        clientWidth: message.clientWidth,
        scrollWidth: message.scrollWidth,
        textFits: textFits(message, message.getBoundingClientRect()),
      })),
    };
  });
}

const results = [];
try {
  for (const engine of [chromium, webkit]) {
    const browser = await engine.launch();
    try {
      for (const scenario of scenarios) {
        const context = await browser.newContext({
          viewport: { width: scenario.width, height: scenario.height },
          deviceScaleFactor: scenario.deviceScaleFactor,
        });
        const page = await context.newPage();
        const errors = [];
        page.on("pageerror", (error) => errors.push(error.message));
        try {
          for (const kind of ["bug", "feature"]) {
            const label = `${engine.name()} ${kind} ${scenario.name}`;
            await page.goto(`${origin}/history-width?kind=${kind}`);
            await page.waitForFunction(() => Boolean(window.qa));

            const editOpener = page.locator("[data-item-edit]");
            await editOpener.click();
            await expect(page.locator("#work-item-form")).toBeVisible();
            const ordinaryWidth = await page
              .locator("#dialog")
              .evaluate((node) => node.getBoundingClientRect().width);
            await page.locator("#dialog-close").click();

            const historyOpener = page.locator("[data-item-history]");
            await historyOpener.click();
            await expect(
              page.locator('[data-history-detail-revision="14"]'),
            ).toBeVisible();
            await expect(
              page.locator(".work-item-history-messages button"),
            ).toHaveCount(1);
            await expect(page.locator("#dialog-close")).toBeFocused();

            const geometry = await horizontalReadability(page);
            const overflows =
              geometry.documentOverflow > 1 ||
              geometry.dialog.scrollWidth > geometry.dialog.clientWidth + 1 ||
              geometry.detail.scrollWidth > geometry.detail.clientWidth + 1 ||
              geometry.description.scrollWidth >
                geometry.description.clientWidth + 1 ||
              !geometry.description.textFits ||
              geometry.messages.some(
                (message) =>
                  message.scrollWidth > message.clientWidth + 1 ||
                  !message.textFits,
              );
            if (expectBaseline) {
              assert.ok(
                overflows,
                `${label}: baseline did not expose clipping`,
              );
            } else {
              assert.equal(
                overflows,
                false,
                `${label}: ${JSON.stringify(geometry)}`,
              );
              assert.ok(
                geometry.dialog.left >= -1 &&
                  geometry.dialog.right <= geometry.viewportWidth + 1,
                `${label}: history dialog escaped the viewport`,
              );
            }

            const description = await page
              .locator("[data-history-detail] pre")
              .textContent();
            assert.equal(
              description,
              `REVISION-14\n${await page.evaluate(() => qa.longDescription)}`,
            );
            const linkedText = await page
              .locator(".work-item-history-messages button")
              .textContent();
            assert.ok(
              linkedText.includes(await page.evaluate(() => qa.longMessage)),
              `${label}: linked message text was lost`,
            );

            await page.locator('[data-history-revision="1"]').click();
            await expect(
              page.locator('[data-history-revision="1"]'),
            ).toHaveAttribute("aria-pressed", "true");
            await expect(
              page.locator("[data-history-detail] pre"),
            ).toContainText("REVISION-1");
            await page.locator(".work-item-history-messages button").click();
            assert.equal(
              await page.evaluate(() => qa.state.boardOpens),
              1,
              `${label}: explicit-message navigation was lost`,
            );

            const dialogScroll = await page
              .locator("#dialog")
              .evaluate((node) => ({
                clientHeight: node.clientHeight,
                scrollHeight: node.scrollHeight,
              }));
            assert.ok(
              dialogScroll.scrollHeight > dialogScroll.clientHeight,
              `${label}: fixture did not exercise vertical scrolling`,
            );
            await page
              .locator("#dialog")
              .evaluate((node) => (node.scrollTop = node.scrollHeight));
            const closeBounds = await page
              .locator("#dialog-close")
              .boundingBox();
            assert.ok(
              closeBounds &&
                closeBounds.y >= 0 &&
                closeBounds.y + closeBounds.height <= scenario.height,
              `${label}: close control was unreachable after scrolling: ${JSON.stringify(closeBounds)}`,
            );

            const historyWidth = geometry.dialog.width;
            if (scenario.width >= 1024) {
              if (expectBaseline)
                assert.ok(
                  Math.abs(historyWidth - ordinaryWidth) <= 2,
                  `${label}: baseline history dialog was unexpectedly widened`,
                );
              else
                assert.ok(
                  historyWidth >= ordinaryWidth + 240,
                  `${label}: history dialog did not gain usable desktop width`,
                );
            }

            const artifactName = `${expectBaseline ? "baseline" : "candidate"}-${engine.name()}-${kind}-${scenario.name}.png`;
            await page.screenshot({
              path: new URL(artifactName, artifactDir).pathname,
              fullPage: false,
            });
            await page.locator("#dialog-close").click();
            assert.equal(
              await page.evaluate(() => document.activeElement?.isConnected),
              true,
              `${label}: close left focus on disconnected history content`,
            );

            await historyOpener.click();
            await expect(page.locator(".work-item-history")).toBeVisible();
            await expect(page.locator("#dialog-close")).toBeFocused();
            await page.keyboard.press("Escape");
            await expect(page.locator("#dialog")).not.toBeVisible();
            assert.equal(
              await page.evaluate(() => document.activeElement?.isConnected),
              true,
              `${label}: Escape left focus on disconnected history content`,
            );

            if (kind === "feature") {
              const narrativeOpener = page.getByRole("button", {
                name: "History & report",
              });
              await narrativeOpener.click();
              await expect(page.locator(".feature-narrative")).toBeVisible();
              const narrativeWidth = await page
                .locator("#dialog")
                .evaluate((node) => node.getBoundingClientRect().width);
              assert.ok(
                Math.abs(narrativeWidth - ordinaryWidth) <= 2,
                `${label}: separate Feature History & Report width changed`,
              );
              await page.locator("#dialog-close").click();
              assert.equal(
                await page.evaluate(() => document.activeElement?.isConnected),
                true,
                `${label}: narrative close left focus disconnected`,
              );
            }

            assert.deepEqual(errors, [], `${label}: browser page errors`);
            results.push(
              `${label}: ${expectBaseline ? "baseline overflow reproduced" : "wrapped, readable, selectable, scrollable, closable, and scoped"}; dialog ${Math.round(geometry.dialog.width)}px, detail scroll/client ${geometry.detail.scrollWidth}/${geometry.detail.clientWidth}, description scroll/client ${geometry.description.scrollWidth}/${geometry.description.clientWidth}`,
            );
          }
        } finally {
          await context.close();
        }
      }
    } finally {
      await browser.close();
    }
  }
} finally {
  await server.close();
}

for (const result of results) console.log(result);
