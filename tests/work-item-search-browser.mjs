// Acceptance for wi_1b3c05505232a68e revision 1, work order #2213, and for
// the progressive page load of bug wi_782e0ea7627f9fca (order #53690): the
// first page paints before later pages arrive, search covers later pages,
// and a filter change mid-load never shows the old filter's rows.
// Uses only synthetic in-memory records and disposable browser contexts.
import { chromium, webkit } from "@playwright/test";
import { createServer } from "vite";
import { viteCacheDir } from "./vite-cache.mjs";
import assert from "node:assert/strict";

const html = `<!doctype html><html><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<link rel="stylesheet" href="/client/style.css"><link rel="stylesheet" href="/client/work-items.css">
<style>html,body{height:100%;margin:0}#mode-view{display:flex;height:100%}</style></head>
<body><main id="mode-view"></main><dialog id="dialog"></dialog><p id="notice"></p>
<script type="module">
import {createWorkItemsView} from '/client/work-items-view.js';
const params=new URLSearchParams(location.search);
const kind=params.get('kind')||'bug';
// paged=1: 450 records in hub pages (next is the last seq of every non-empty
// page), at most cap per page; hold=1 holds the default filter's later pages
// until qa.release().
const paged=params.get('paged')==='1', cap=Number(params.get('cap')||200);
const task={id:'tsk_fixture',name:'Synthetic project',status:'open',goal:'isolated'};
const other={id:'tsk_other',name:'Other project',status:'open',goal:'isolated'};
const items=[
 {id:'wi_current',taskId:task.id,kind,title:'Visible alpha',description:'Current writeup needle',status:'open',priority:'normal',revision:2},
 {id:'wi_history',taskId:task.id,kind,title:'Visible beta',description:'Present text',status:'open',priority:'high',revision:2},
];
const extra=paged?Array.from({length:448},(_,i)=>({id:'wi_page'+String(i).padStart(4,'0'),taskId:i%2?task.id:other.id,kind,title:i===447?'Later omega record':'Paged record '+i,description:'Synthetic paged text',status:i%3?'done':'open',priority:'normal',revision:1})):[];
const all=[...items,...extra].map((item,i)=>({...item,seq:i+1}));
const requests=[];
// cursor=1: pages carry a change cursor and are served as a saved cache
// serves a followed page (the first answer per filter, until a reset), and
// listWorkItemChanges answers from a change log.
const cursorMode=params.get('cursor')==='1';
const live=[...items,{id:'wi_closed',taskId:task.id,kind,title:'Closed record',description:'Done text',status:'done',priority:'low',revision:1}].map((item,i)=>({...item,seq:i+1}));
const savedPages=new Map(), changeLog=[], changeReads=[];
let changeSeq=0, resetNext=false, failChanges=false, listCalls=0;
// Each held page waits on its own release, oldest first.
const held=[];
let holding=params.get('hold')==='1';
let notify=()=>{}, messageText='Board epsilon evidence', reportText='Durable zeta report writeup', reportFetches=0, failReport=false;
const client={base:'isolated://search',token:'fixture',cacheStatus:()=>({label:'Fixture current'}),subscribe:(_scope,callback)=>{notify=callback;return {stop(){}}},
 async listTasks(){return paged?[task,other]:[task]},
 async listWorkItems({taskId='',status='',after=0,limit=200}={}){
  listCalls++;
  if(cursorMode){
   const key=JSON.stringify([taskId,status,after]);
   if(!savedPages.has(key)){
    const page=live.filter(item=>item.seq>after&&(!status||item.status===status)&&(!taskId||item.taskId===taskId)).slice(0,limit);
    savedPages.set(key,{items:page.map(item=>({...item})),next:page.length?page.at(-1).seq:0,cursor:changeSeq+'.0'});
   }
   return structuredClone(savedPages.get(key));
  }
  if(!paged)return {items:items.map(item=>({...item})),next:0};
  requests.push({taskId,status,after});
  if(after>0&&!taskId&&!status&&holding)await new Promise(resolve=>held.push(resolve));
  const page=all.filter(item=>item.seq>after&&(!status||item.status===status)&&(!taskId||item.taskId===taskId)).slice(0,Math.min(limit,cap));
  return {items:page.map(item=>({...item})),next:page.length?page.at(-1).seq:0};
 },
 async listWorkItemRevisions(_task,id){
  return {revisions:id==='wi_history'?[{...items[1],revision:1,title:'Retired gamma title',description:'Archived delta description'},{...items[1]}]:[{...(all.find(item=>item.id===id)||items[0])}],nextAfter:0};
 },
 async listWorkItemMessages(_task,id,{revision}){return {links:id==='wi_history'&&revision===1?[{message:{text:messageText}}]:[],nextAfter:0}},
 async listNarrativeReports(){return {reports:[{reportId:'nrpt_fixture',version:1}]}},
 async getNarrativeReportVersion(){reportFetches++;if(failReport)throw new Error('transient report failure');return {sections:{deliveredWork:reportText}}},
 async listNarrativeArtifacts(){return {artifacts:[{artifactId:'nart_fixture'}]}},
 async listNarrativeArtifactVersions(){return {versions:[{version:1}]}},
 async getNarrativeArtifactVersion(){return {title:'Submitted artifact',content:'Captured eta artifact body'}},
};
if(cursorMode)client.listWorkItemChanges=async({taskId='',since})=>{
 changeReads.push(since);
 if(failChanges)throw new Error('synthetic changes read failure');
 if(resetNext){resetNext=false;savedPages.clear();return {items:[],next:0,cursor:changeSeq+'.0',reset:true}}
 const after=Number(String(since).split('.')[0]);
 const ids=new Set(changeLog.filter(entry=>entry.seq>after).map(entry=>entry.id));
 return {items:live.filter(item=>ids.has(item.id)&&(!taskId||item.taskId===taskId)).map(item=>({...item})),next:0,cursor:changeSeq+'.0',changes:true};
};
const dialogNode=document.querySelector('#dialog');
const closeDialog=()=>{dialogNode.close();dialogNode.replaceChildren()};
const dialog=(title,body)=>{dialogNode.innerHTML='<h2>'+title+'</h2>'+body;dialogNode.showModal()};
const view=createWorkItemsView({kind,client:()=>client,dialog,closeDialog,notice(){},configure(){},openBoard(){}});
view.mount(document.querySelector('#mode-view'));const shown=view.show();if(!paged)await shown;window.qa={view,refresh(){notify()},
 event(kind){notify([{seq:Date.now(),kind,taskId:task.id,data:{}}],0)},
 change(id,patch){const item=live.find(entry=>entry.id===id);Object.assign(item,patch,{revision:item.revision+1});changeLog.push({seq:++changeSeq,id})},
 resetNext(){resetNext=true},
 failChanges(value){failChanges=value},
 reads(){return {pages:listCalls,changes:changeReads.slice()}},requests(){return requests.map(r=>({...r}))},release(){holding=false;held.splice(0).forEach(resolve=>resolve())},releaseNext(){held.shift()?.()},held(){return held.length},
 count(status,taskId){return all.filter(item=>(!status||item.status===status)&&(!taskId||item.taskId===taskId)).length},setMessage(text){messageText=text},setReport(text){reportText=text},setReportFailure(value){failReport=value},reportFetches(){return reportFetches}};
</script></body></html>`;

const server = await createServer({
  configFile: false,
  cacheDir: viteCacheDir(import.meta.url),
  root: process.cwd(),
  server: { host: "127.0.0.1", port: 0 },
  plugins: [
    {
      name: "work-item-search-fixture",
      configureServer(vite) {
        vite.middlewares.use("/work-item-search-test", (_req, res) => {
          res.setHeader("Content-Type", "text/html");
          res.end(html);
        });
      },
    },
  ],
});
await server.listen();

const rows = (page) =>
  page.evaluate(() =>
    [...document.querySelectorAll("[data-work-item]")].map((row) => ({
      id: row.dataset.workItem,
      text: row.querySelector(".work-item-copy .fine").textContent,
    })),
  );
const statusText = (page) =>
  page.locator("[data-items-search-status]").textContent();

async function openPaged(browser, kind, query) {
  const page = await browser.newPage();
  const errors = [];
  page.on("pageerror", (error) => errors.push(error.message));
  await page.goto(
    `${origin}/work-item-search-test?kind=${kind}&paged=1&${query}`,
  );
  await page.waitForFunction(() => Boolean(window.qa));
  return { page, errors };
}

async function pagedCases(browser, engine, kind) {
  const label = `${engine} ${kind}`;
  // First rows before the last page; a search for a later-page record says
  // it is still loading, then finds it; the read stops at the short page.
  {
    const { page, errors } = await openPaged(browser, kind, "hold=1");
    await page.waitForFunction(
      () => document.querySelectorAll("[data-work-item]").length === 200,
    );
    assert.match(await statusText(page), /Loading more (bugs|features)/, label);
    assert.equal(await page.locator("[data-items-status]").inputValue(), "");
    assert.equal(await page.locator(".count-badge").textContent(), "200");
    assert.deepEqual(
      (await page.evaluate(() => window.qa.requests())).map((r) => r.after),
      [0, 200],
      `${label}: page 2 is requested and held`,
    );
    const search = page.locator("[data-items-search]");
    await search.fill("later omega");
    await page.waitForFunction(() =>
      document
        .querySelector(".work-items-empty")
        ?.textContent.includes("Loading more"),
    );
    assert.doesNotMatch(
      await page.locator(".work-items-empty").innerText(),
      /match this search/,
      `${label}: a held later page is not "no match"`,
    );
    assert.match(await statusText(page), /0 matches so far · Loading more/);
    await page.evaluate(() => window.qa.release());
    await page.waitForFunction(
      () => document.querySelectorAll("[data-work-item]").length === 1,
    );
    assert.equal(
      await page.locator("[data-work-item]").getAttribute("data-work-item"),
      "wi_page0447",
    );
    await page.waitForFunction(() =>
      document
        .querySelector("[data-items-search-status]")
        .textContent.includes("1 match"),
    );
    assert.doesNotMatch(await statusText(page), /Loading more|so far/);
    await search.fill("");
    await page.waitForFunction(
      () => document.querySelectorAll("[data-work-item]").length === 450,
    );
    assert.equal(await page.locator(".count-badge").textContent(), "450");
    // 200, 200, 50: the short third page ends the read, with no empty page.
    assert.deepEqual(
      (await page.evaluate(() => window.qa.requests())).map((r) => r.after),
      [0, 200, 400],
    );
    assert.deepEqual(errors, [], `${label}: page errors`);
    await page.close();
  }
  // A status or project change while later pages are held: no row of the
  // old filter appears after the change, and the new filter's list is whole.
  for (const change of ["status", "project"]) {
    const { page, errors } = await openPaged(browser, kind, "hold=1");
    await page.waitForFunction(
      () => document.querySelectorAll("[data-work-item]").length === 200,
    );
    await page.waitForFunction(() => window.qa.held() === 1);
    const [status, taskId] =
      change === "status" ? ["open", ""] : ["", "tsk_other"];
    const expected = await page.evaluate(
      ([s, t]) => window.qa.count(s, t),
      [status, taskId],
    );
    const matches = (text) =>
      change === "status"
        ? text.includes(" · Open · ")
        : text.startsWith("Other project");
    // Page 2 of the old filter arrives, and the filter changes while its
    // list repaint is still pending. From the change on, every inserted
    // row is recorded.
    await page.evaluate(async (which) => {
      window.qa.releaseNext();
      await new Promise((resolve) => setTimeout(resolve, 0));
      window.inserted = [];
      new MutationObserver((records) => {
        for (const record of records)
          for (const node of record.addedNodes)
            if (node.querySelectorAll)
              for (const row of [
                ...(node.matches?.("[data-work-item]") ? [node] : []),
                ...node.querySelectorAll("[data-work-item]"),
              ])
                window.inserted.push(
                  row.querySelector(".work-item-copy .fine").textContent,
                );
      }).observe(document.querySelector("#mode-view"), {
        childList: true,
        subtree: true,
      });
      if (which === "status") {
        const select = document.querySelector("[data-items-status]");
        select.value = "open";
        select.dispatchEvent(new Event("change", { bubbles: true }));
      } else document.querySelector('[data-items-scope="tsk_other"]').click();
    }, change);
    // The old read is still paging, held at page 3. Nothing of the old
    // filter is on screen, before or after the pending repaint.
    await page.waitForFunction(() => window.qa.held() === 1);
    await page.waitForTimeout(300);
    const whileHeld = await rows(page);
    assert.deepEqual(
      whileHeld.filter((row) => !matches(row.text)),
      [],
      `${label} ${change}: old-filter rows while the old read is held`,
    );
    assert.match(await statusText(page), /Loading (bugs|features)…/);
    await page.evaluate(() => window.qa.release());
    await page.waitForFunction(
      (n) =>
        document.querySelectorAll("[data-work-item]").length === n &&
        !document
          .querySelector("[data-items-search-status]")
          .textContent.includes("Loading"),
      expected,
    );
    await page.waitForTimeout(300);
    const shown = await rows(page);
    assert.equal(shown.length, expected, `${label} ${change}: row count`);
    assert.ok(
      shown.every((row) => matches(row.text)),
      `${label} ${change}`,
    );
    const inserted = await page.evaluate(() => window.inserted);
    assert.ok(inserted.length > 0, `${label} ${change}: new rows rendered`);
    assert.deepEqual(
      inserted.filter((text) => !matches(text)),
      [],
      `${label} ${change}: old-filter rows after the change`,
    );
    // The old read finished on its own filter, unshown, before the new one.
    const requests = await page.evaluate(() => window.qa.requests());
    assert.deepEqual(
      requests.filter((r) => !r.status && !r.taskId).map((r) => r.after),
      [0, 200, 400],
      `${label} ${change}: old-filter reads`,
    );
    assert.ok(
      requests.findIndex((r) => r.status || r.taskId) > 2,
      `${label} ${change}: the new filter read after the old one`,
    );
    assert.deepEqual(errors, [], `${label} ${change}: page errors`);
    await page.close();
  }
  // A hub that pages below the view's limit still lists every record.
  {
    const { page, errors } = await openPaged(browser, kind, "cap=100");
    await page.waitForFunction(
      () => document.querySelectorAll("[data-work-item]").length === 450,
    );
    await page.waitForFunction(
      () =>
        !document
          .querySelector("[data-items-search-status]")
          .textContent.includes("Loading more"),
    );
    assert.deepEqual(
      (await page.evaluate(() => window.qa.requests())).map((r) => r.after),
      [0, 100, 200, 300, 400],
    );
    assert.deepEqual(errors, [], `${label} cap: page errors`);
    await page.close();
  }
}
// Changes-since refresh (bug wi_c83e8ef138265cf9, order #54541): with a hub
// that names a cursor, a change notice after a complete read makes one
// changes read and no page reads; without one, the view reloads every page.
async function cursorCases(browser, engine, kind) {
  const label = `${engine} ${kind} cursor`;
  const reads = (page) => page.evaluate(() => window.qa.reads());
  const titles = (page) =>
    page.evaluate(() =>
      [...document.querySelectorAll("[data-work-item]")].map((row) => [
        row.dataset.workItem,
        row.querySelector(".work-item-title").textContent,
      ]),
    );
  const changesAfter = (page, count) =>
    page.waitForFunction((n) => window.qa.reads().changes.length > n, count);
  {
    const page = await browser.newPage();
    const errors = [];
    page.on("pageerror", (error) => errors.push(error.message));
    await page.goto(`${origin}/work-item-search-test?kind=${kind}&cursor=1`);
    await page.waitForFunction(() => Boolean(window.qa));
    await page.waitForFunction(
      () => document.querySelectorAll("[data-work-item]").length === 3,
    );
    // The complete read is followed by one changes read from its cursor.
    await page.waitForFunction(() => window.qa.reads().changes.length === 1);
    let before = await reads(page);
    assert.deepEqual(before.changes, ["0.0"], `${label}: first changes read`);
    // The page reads of one complete read: the list, then its empty end.
    const perReload = before.pages;
    assert.equal(perReload, 2, `${label}: one complete read`);
    // A message alone changes no row: no read at all.
    await page.evaluate(() => window.qa.event("message"));
    await page.waitForTimeout(150);
    assert.deepEqual(await reads(page), before, `${label}: message event`);
    // A work-item change: one changes read, no page read, the row updates.
    await page.evaluate(() => {
      window.qa.change("wi_current", { title: "Renamed alpha" });
      window.qa.event("work_item_updated");
    });
    await page.waitForFunction(() =>
      document
        .querySelector('[data-item-edit="wi_current"]')
        ?.textContent.includes("Renamed alpha"),
    );
    let after = await reads(page);
    assert.equal(after.pages, before.pages, `${label}: no page reads`);
    assert.deepEqual(after.changes, ["0.0", "0.0"], `${label}: changes read`);
    assert.deepEqual(
      (await titles(page)).map(([id]) => id),
      ["wi_current", "wi_history", "wi_closed"],
      `${label}: row order kept`,
    );
    // An item leaving the status filter disappears.
    await page.locator("[data-items-status]").selectOption("open");
    await page.waitForFunction(
      () => document.querySelectorAll("[data-work-item]").length === 2,
    );
    await changesAfter(page, after.changes.length);
    before = await reads(page);
    await page.evaluate(() => {
      window.qa.change("wi_history", { status: "done" });
      window.qa.event("work_item_updated");
    });
    await page.waitForFunction(
      () => !document.querySelector('[data-work-item="wi_history"]'),
    );
    after = await reads(page);
    assert.equal(after.pages, before.pages, `${label}: leaving needs no page`);
    assert.equal(after.changes.length, before.changes.length + 1);
    assert.equal(await page.locator(".count-badge").textContent(), "1");
    await page.locator("[data-items-status]").selectOption("");
    await page.waitForFunction(
      () => document.querySelectorAll("[data-work-item]").length === 3,
    );
    // A reset makes one full reload: one page read, then no changes read.
    await page.waitForTimeout(150);
    before = await reads(page);
    await page.evaluate(() => {
      window.qa.resetNext();
      window.qa.change("wi_closed", { title: "Reset record" });
      window.qa.event("work_item_updated");
    });
    await page.waitForFunction(() =>
      document
        .querySelector('[data-item-edit="wi_closed"]')
        ?.textContent.includes("Reset record"),
    );
    await page.waitForTimeout(150);
    after = await reads(page);
    assert.equal(
      after.pages,
      before.pages + perReload,
      `${label}: one full reload`,
    );
    assert.equal(after.changes.length, before.changes.length + 1);
    // A change while the tab is hidden shows after show(), through one
    // changes read from the saved page's cursor.
    await page.evaluate(() => window.qa.view.hide());
    await page.evaluate(() =>
      window.qa.change("wi_history", { title: "Hidden change" }),
    );
    before = await reads(page);
    await page.evaluate(() => window.qa.view.show());
    await page.waitForFunction(() =>
      document
        .querySelector('[data-item-edit="wi_history"]')
        ?.textContent.includes("Hidden change"),
    );
    after = await reads(page);
    assert.equal(
      after.pages,
      before.pages + perReload,
      `${label}: saved pages reread`,
    );
    assert.equal(after.changes.length, before.changes.length + 1);
    // A failing changes read falls back to one full reload and no further
    // changes read until the next relevant notice (review blocker b1).
    await page.evaluate(() => window.qa.failChanges(true));
    for (let notice = 1; notice <= 2; notice++) {
      before = await reads(page);
      await page.evaluate(() => window.qa.event("work_item_updated"));
      await page.waitForFunction(
        (pages) => window.qa.reads().pages >= pages,
        before.pages + perReload,
      );
      await page.waitForTimeout(400);
      after = await reads(page);
      assert.equal(
        after.changes.length,
        before.changes.length + 1,
        `${label}: notice ${notice} after a failed changes read: one changes read`,
      );
      assert.equal(
        after.pages,
        before.pages + perReload,
        `${label}: notice ${notice} after a failed changes read: one reload`,
      );
    }
    await page.evaluate(() => {
      window.qa.failChanges(false);
      window.qa.change("wi_current", { title: "Recovered alpha" });
      window.qa.event("work_item_updated");
    });
    await page.waitForFunction(() =>
      document
        .querySelector('[data-item-edit="wi_current"]')
        ?.textContent.includes("Recovered alpha"),
    );
    assert.deepEqual(errors, [], `${label}: page errors`);
    await page.close();
  }
  // A hub without a cursor: a change notice rereads every page.
  {
    const page = await browser.newPage();
    await page.goto(`${origin}/work-item-search-test?kind=${kind}`);
    await page.waitForFunction(() => Boolean(window.qa));
    await page.waitForFunction(
      () => document.querySelectorAll("[data-work-item]").length === 2,
    );
    const before = await reads(page);
    await page.evaluate(() => window.qa.event("work_item_updated"));
    await page.waitForFunction(
      (n) => window.qa.reads().pages > n,
      before.pages,
    );
    assert.deepEqual((await reads(page)).changes, [], `${label}: no cursor`);
    await page.close();
  }
}

const origin = `http://127.0.0.1:${server.httpServer.address().port}`;
try {
  for (const engine of [chromium, webkit]) {
    const browser = await engine.launch();
    try {
      for (const kind of ["bug", "feature"]) {
        const page = await browser.newPage();
        const errors = [];
        page.on("pageerror", (error) => errors.push(error.message));
        await page.goto(`${origin}/work-item-search-test?kind=${kind}`);
        await page.waitForFunction(() => Boolean(window.qa));
        const search = page.locator("[data-items-search]");
        await search.fill("writeup needle");
        await page.waitForFunction(
          () => document.querySelectorAll("[data-work-item]").length === 1,
        );
        assert.equal(
          await page.locator("[data-work-item]").getAttribute("data-work-item"),
          "wi_current",
        );
        await search.fill("archived delta");
        await page.waitForFunction(() =>
          document
            .querySelector("[data-items-search-status]")
            .textContent.includes("1 match"),
        );
        assert.equal(
          await page.locator("[data-work-item]").getAttribute("data-work-item"),
          "wi_history",
        );
        await search.fill("epsilon evidence");
        await page.waitForFunction(() =>
          document
            .querySelector("[data-items-search-status]")
            .textContent.includes("1 match"),
        );
        assert.equal(
          await page.locator("[data-work-item]").getAttribute("data-work-item"),
          "wi_history",
        );
        await search.fill("same revision message");
        await page.evaluate(() => {
          const input = document.querySelector("[data-items-search]");
          input.focus();
          input.setSelectionRange(4, 12, "forward");
          window.qa.setMessage("Fresh same revision message");
          window.qa.refresh();
        });
        await page.waitForFunction(() =>
          document
            .querySelector("[data-items-search-status]")
            .textContent.includes("1 match"),
        );
        assert.deepEqual(
          await search.evaluate((input) => ({
            focused: document.activeElement === input,
            value: input.value,
            start: input.selectionStart,
            end: input.selectionEnd,
            direction: input.selectionDirection,
          })),
          {
            focused: true,
            value: "same revision message",
            start: 4,
            end: 12,
            direction: "forward",
          },
        );
        if (kind === "feature") {
          await search.fill("zeta report");
          await page.waitForFunction(() =>
            document
              .querySelector("[data-items-search-status]")
              .textContent.includes("2 matches"),
          );
          assert.equal(await page.locator("[data-work-item]").count(), 2);
          await search.fill("eta artifact");
          await page.waitForFunction(() =>
            document
              .querySelector("[data-items-search-status]")
              .textContent.includes("2 matches"),
          );
          assert.equal(await page.locator("[data-work-item]").count(), 2);
          await search.fill("same revision narrative");
          await page.evaluate(() => {
            window.qa.setReport("Fresh same revision narrative");
            window.qa.refresh();
          });
          await page.waitForFunction(() =>
            document
              .querySelector("[data-items-search-status]")
              .textContent.includes("2 matches"),
          );
          const fetchesBeforeFailure = await page.evaluate(() =>
            window.qa.reportFetches(),
          );
          await page.evaluate(() => {
            window.qa.setReportFailure(true);
            window.qa.refresh();
          });
          await page.waitForFunction(
            (before) => window.qa.reportFetches() > before,
            fetchesBeforeFailure,
          );
          await page.waitForFunction(() =>
            document
              .querySelector("[data-items-search-status]")
              .textContent.includes("history unavailable"),
          );
          const failedFetches = await page.evaluate(() =>
            window.qa.reportFetches(),
          );
          await page.evaluate(() => window.qa.setReportFailure(false));
          await search.press("End");
          await search.type(" ");
          await page.waitForFunction(
            (before) => window.qa.reportFetches() > before,
            failedFetches,
          );
          await page.waitForFunction(() =>
            !document
              .querySelector("[data-items-search-status]")
              .textContent.includes("history unavailable"),
          );
        }
        await search.fill("no such record");
        await page.waitForFunction(
          () => document.querySelectorAll("[data-work-item]").length === 0,
        );
        assert.match(
          await page.locator(".work-items-empty").innerText(),
          /No (bugs|features) match this search/,
        );
        await search.fill("");
        await page.waitForFunction(
          () => document.querySelectorAll("[data-work-item]").length === 2,
        );
        await page.locator('[data-item-edit="wi_current"]').click();
        assert.equal(
          await page.locator("#work-item-title").inputValue(),
          "Visible alpha",
        );
        assert.deepEqual(errors, [], `${engine.name()} ${kind} page errors`);
        await page.close();
        await pagedCases(browser, engine.name(), kind);
        await cursorCases(browser, engine.name(), kind);
      }
    } finally {
      await browser.close();
    }
  }
} finally {
  await server.close();
}
console.log(
  "Chromium/WebKit Bugs/Features live search, history, clearing, empty state, and editor navigation passed.",
);
