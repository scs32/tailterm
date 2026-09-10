// Acceptance for wi_85a66e159b50a309 revision 2, work order #1358.
// Uses in-memory work items and disposable browser contexts. Pointer activation
// opens the browser-owned select, but protocol input cannot choose an option in
// the macOS native popup; selectOption cases below are labelled synthetic.
import { chromium, webkit, expect } from "@playwright/test";
import { createServer } from "vite";
import assert from "node:assert/strict";

const expectBaseline = process.env.DROPDOWN_DIALOG_BASELINE === "1";
const html = `<!doctype html><html><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<link rel="stylesheet" href="/client/style.css">
<link rel="stylesheet" href="/client/work-items.css">
<style>html,body{height:100%;margin:0}#mode-view{display:flex;height:100%}</style>
</head><body><main id="mode-view"></main><dialog id="dialog"></dialog>
<p id="notice" role="status"></p><script type="module">
import {createWorkItemsView} from '/client/work-items-view.js';
import {installDialogSelectInteraction} from '/client/dialog-interaction.js';
const kind=new URLSearchParams(location.search).get('kind')||'bug';
const task={id:'tsk_fixture',name:'Isolated project',goal:'No live data',status:'open',orchestrator:'lead'};
const item={id:'wi_'+kind,taskId:task.id,kind,title:'Synthetic '+kind,description:'Isolated editor fixture',status:'open',priority:'normal',revision:1};
const state={updates:[]};
const client={base:'isolated://fixture',token:'non-secret-fixture',
 async listTasks(){return [{...task}]},
 async listWorkItems(){return {items:[{...item}],next:0}},
 async createWorkItemUpdate(_taskId,_itemId,request){state.updates.push(structuredClone(request));Object.assign(item,request,{revision:item.revision+1});return {...item}},
 subscribe(){return {stop(){}}},cacheStatus(){return {label:'Fixture current'}},
};
const dialogNode=document.querySelector('#dialog');
const closeDialog=()=>{dialogNode.close();dialogNode.replaceChildren()};
const dialog=(title,body)=>{if(dialogNode.open)dialogNode.close();${expectBaseline ? "dialogNode.oncancel=null;dialogNode.onkeydown=null" : "installDialogSelectInteraction(dialogNode)"};dialogNode.innerHTML='<div class="dialog-head"><h2 id="dialog-title">'+title+'</h2><button id="dialog-close" aria-label="Close dialog">×</button></div>'+body;dialogNode.setAttribute('aria-labelledby','dialog-title');dialogNode.querySelector('#dialog-close').onclick=closeDialog;dialogNode.showModal()};
const view=createWorkItemsView({kind,client:()=>client,dialog,closeDialog,notice:text=>document.querySelector('#notice').textContent=text,configure(){},openBoard(){}});
view.mount(document.querySelector('#mode-view'));await view.show(task.id);
window.qa={state,item,view};
</script></body></html>`;

const server = await createServer({
  configFile: false,
  root: process.cwd(),
  server: { host: "127.0.0.1", port: 0 },
  plugins: [
    {
      name: "dropdown-dialog-fixture",
      configureServer(vite) {
        vite.middlewares.use("/dropdown-dialog-test", (_request, response) => {
          response.setHeader("Content-Type", "text/html");
          response.end(html);
        });
      },
    },
  ],
});
await server.listen();
const origin = `http://127.0.0.1:${server.httpServer.address().port}`;

try {
  for (const engine of [chromium, webkit]) {
    const browser = await engine.launch();
    const context = await browser.newContext({
      viewport: { width: 1000, height: 760 },
    });
    const page = await context.newPage();
    const errors = [];
    page.on("pageerror", (error) => errors.push(error.message));
    try {
      for (const kind of ["bug", "feature"]) {
        const label = `${engine.name()} ${kind}`;
        await page.goto(`${origin}/dropdown-dialog-test?kind=${kind}`);
        await page.waitForFunction(() => Boolean(window.qa));
        const openEditor = async () => {
          await page.locator("[data-item-edit]").click();
          await expect(page.locator("#work-item-form")).toBeVisible();
        };
        await openEditor();
        if (expectBaseline) {
          const status = page.locator("#work-item-status");
          await status.click();
          await expect
            .poll(() => status.evaluate((node) => node.matches(":open")))
            .toBe(true);
          await page.keyboard.press("Escape");
          await expect(page.locator("#dialog")).not.toBeVisible();

          await openEditor();
          await page
            .locator("#work-item-title")
            .fill(`${label} premature submit`);
          await page.locator("#work-item-status").focus();
          await page.keyboard.press("Enter");
          await expect(page.locator("#dialog")).not.toBeVisible();
          assert.equal(
            await page.evaluate(() => qa.state.updates.length),
            1,
            `${label}: baseline select Enter did not submit the editor`,
          );
          console.log(
            `${label}: baseline reproduced native-select Escape and Enter closing the editor.`,
          );
          continue;
        }

        await page.locator("#work-item-title").focus();
        await page.keyboard.press("Escape");
        await expect(page.locator("#dialog")).not.toBeVisible();

        await openEditor();
        await page.locator("#work-item-title").fill(`${label} retained draft`);

        const status = page.locator("#work-item-status");
        await status.focus();
        await page.keyboard.press("Enter");
        await expect(page.locator("#dialog")).toBeVisible();
        await page.waitForTimeout(100);
        const enterOpenedPicker = await status.evaluate((node) =>
          node.matches(":open"),
        );
        if (engine.name() === "chromium")
          assert.equal(
            enterOpenedPicker,
            true,
            `${label}: trusted Enter did not open the picker`,
          );
        assert.equal(
          await page.evaluate(() => qa.state.updates.length),
          0,
          `${label}: native-select Enter submitted the editor`,
        );
        if (enterOpenedPicker) {
          await page.keyboard.press("Escape");
          await expect(page.locator("#dialog")).toBeVisible();
          await expect
            .poll(() => status.evaluate((node) => node.matches(":open")))
            .toBe(false);
        } else {
          await expect(status).toBeFocused();
        }
        await page.locator("#dialog-close").click();
        await expect(page.locator("#dialog")).not.toBeVisible();
        await openEditor();
        await expect(page.locator("#work-item-title")).toHaveValue(
          `${label} retained draft`,
        );

        for (const selector of ["#work-item-status", "#work-item-priority"]) {
          const control = page.locator(selector);
          const beforeScroll = await page
            .locator("#dialog")
            .evaluate((node) => node.scrollTop);
          await control.click();
          await expect
            .poll(() => control.evaluate((node) => node.matches(":open")))
            .toBe(true);
          await page.keyboard.press("Escape");
          await expect(page.locator("#dialog")).toBeVisible();
          await expect
            .poll(() => control.evaluate((node) => node.matches(":open")))
            .toBe(false);
          await expect(control).toBeFocused();
          assert.equal(
            await page.locator("#dialog").evaluate((node) => node.scrollTop),
            beforeScroll,
            `${label} ${selector}: focus restoration scrolled dialog`,
          );
          await expect(page.locator("#work-item-title")).toHaveValue(
            `${label} retained draft`,
          );
        }

        // Playwright's DOM-level option selection verifies commit/value wiring,
        // not physical interaction with the native macOS picker layer.
        await page.locator("#work-item-status").selectOption("blocked");
        await page.locator("#work-item-priority").selectOption("high");
        await expect(page.locator("#dialog")).toBeVisible();
        assert.equal(
          await page.evaluate(() => qa.state.updates.length),
          0,
          `${label}: synthetic option commits submitted the editor`,
        );
        await page.locator("#dialog-close").click();
        await openEditor();
        await expect(page.locator("#work-item-title")).toHaveValue(
          `${label} retained draft`,
        );
        await expect(page.locator("#work-item-status")).toHaveValue("blocked");
        await expect(page.locator("#work-item-priority")).toHaveValue("high");

        await page.locator("#work-item-form button[type=submit]").click();
        await expect(page.locator("#dialog")).not.toBeVisible();
        const update = await page.evaluate(() => qa.state.updates.at(-1));
        assert.equal(
          update.status,
          "blocked",
          `${label}: status commit was lost`,
        );
        assert.equal(
          update.priority,
          "high",
          `${label}: priority commit was lost`,
        );
        assert.equal(
          update.title,
          `${label} retained draft`,
          `${label}: title draft was lost`,
        );

        await openEditor();
        const select = page.locator("#work-item-status");
        await select.click();
        await expect
          .poll(() => select.evaluate((node) => node.matches(":open")))
          .toBe(true);
        await page.keyboard.press("Escape");
        await expect(page.locator("#dialog")).toBeVisible();
        await expect
          .poll(() => select.evaluate((node) => node.matches(":open")))
          .toBe(false);
        await expect(select).toBeFocused();
        await page.locator("#dialog-close").click();
        await expect(page.locator("#dialog")).not.toBeVisible();

        await openEditor();
        await page.locator("#dialog-close").click();
        await expect(page.locator("#dialog")).not.toBeVisible();
        assert.deepEqual(errors, [], `${label}: page errors`);
        console.log(
          `${label}: select Enter does not submit; native-select Escape stays in the editor; synthetic commits, drafts, focus, ordinary Escape and close button passed${enterOpenedPicker ? "" : " (protocol Enter did not expose WebKit's native picker)"}.`,
        );
      }
    } finally {
      await context.close();
      await browser.close();
    }
  }
} finally {
  await server.close();
}
