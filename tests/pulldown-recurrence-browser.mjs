// wi_85a66e159b50a309 r8, recorded order #1888; entirely in-memory fixtures.
// Protocol clicks expose :open; selectOption and consumed releases are synthetic.
import { chromium, webkit, expect } from "@playwright/test";
import { createServer } from "vite";
import { mkdir, writeFile } from "node:fs/promises";
import assert from "node:assert/strict";
const engineFilter = process.env.PULLDOWN_ENGINE;
const modeFilter = process.env.PULLDOWN_MODE;
const baseline = process.env.PULLDOWN_BASELINE === "1";
const artifacts = `.build/pulldown-recurrence/${baseline ? "baseline" : "candidate"}`;
await mkdir(artifacts, { recursive: true });
const html = `<!doctype html><html><head><meta charset="utf-8"><link rel="stylesheet" href="/client/style.css"><link rel="stylesheet" href="/client/work-items.css">
<style>html,body{height:100%;margin:0}#workspace{display:block;padding:20px;height:100%}#mode-view{height:720px}</style></head><body><div id="workspace"><div id="mode-view"></div></div><dialog id="dialog"></dialog><script type="module">
import {createBoardView} from '/client/board-view.js';
import {createWorkItemsView} from '/client/work-items-view.js';
import {installDialogSelectInteraction} from '/client/dialog-interaction.js';
const mode=new URLSearchParams(location.search).get('mode');
const task={id:'tsk_fixture',name:'Fixture project',goal:'Synthetic only',status:'open',orchestrator:'lead'};
const agent={id:'agt_fixture',name:'lead',status:'running',host:'synthetic.invalid',session:'none'};
const item={id:'wi_fixture',taskId:task.id,kind:mode==='bugs'?'bug':'feature',title:'Synthetic item',description:'Retained description',status:'open',priority:'normal',revision:1};
const state={revision:0,updates:[],events:[]};
const client={base:'isolated://fixture',token:'non-secret-fixture',async listTasks(){return [{...task}]},async getTask(){return {task:{...task},agents:[{...agent}]}},async listMessages(){return [{seq:1,from:{agentId:agent.id},text:'Synthetic message '+state.revision,createdAt:'2026-09-10T12:00:00Z'}]},async listDecisions(){return {decisions:[],nextAfter:0}},async listWorkItems(){return {items:[{...item}],next:0}},async createWorkItemUpdate(_task,_item,request){state.updates.push(request);Object.assign(item,request,{revision:item.revision+1});return {...item}},subscribe(){return {stop(){}}},cacheStatus(){return {label:'Refresh '+state.revision}}};
const d=document.querySelector('#dialog');const closeDialog=()=>{d.close();d.replaceChildren()};
const dialog=(title,body)=>{if(d.open)d.close();installDialogSelectInteraction(d);d.innerHTML='<div class="dialog-head"><h2>'+title+'</h2><button id="dialog-close" type="button">Close</button></div>'+body;d.querySelector('#dialog-close').onclick=closeDialog;d.showModal()};
const noop=()=>{};const common={client:()=>client,getTabs:()=>[],activate:noop,notice:noop,addAgent:noop,settings:noop,revealAgent:noop,attachTask:noop,newTask:noop,configure:noop,openBoard:noop,dialog,closeDialog};
const view=mode==='board'?createBoardView(common):createWorkItemsView({...common,kind:item.kind});view.mount(document.querySelector('#mode-view'));await view.show(task.id);
for(const type of ['pointerdown','pointerup','keydown','keyup','change','focusout','cancel'])document.addEventListener(type,e=>{state.events.push({cancelable:e.cancelable,type,key:e.key,target:e.target.id||e.target.dataset?.viewControl||e.target.tagName,time:performance.now(),active:document.activeElement?.id,pickerOpen:document.activeElement?.matches?.('select:open')});if(state.events.length>100)state.events.shift()},true);
window.qa={state,view,async refresh(){state.revision++;await view.reload();return state.revision}};
</script></body></html>`;
const server = await createServer({
  configFile: false,
  root: process.cwd(),
  server: { host: "127.0.0.1", port: 0 },
  plugins: [
    {
      name: "recurrence-fixture",
      configureServer(vite) {
        vite.middlewares.use("/recurrence-test", (_req, res) => {
          res.setHeader("Content-Type", "text/html");
          res.end(html);
        });
      },
    },
  ],
});
await server.listen();
const origin = `http://127.0.0.1:${server.httpServer.address().port}`;
const evidence = [];
try {
  for (const engine of [chromium, webkit].filter(
    (e) => !engineFilter || e.name() === engineFilter,
  )) {
    const browser = await engine.launch();
    try {
      for (const mode of ["board", "bugs", "features"].filter(
        (m) => !modeFilter || m === modeFilter,
      )) {
        const context = await browser.newContext({
          viewport: { width: 1100, height: 800 },
        });
        const page = await context.newPage();
        const errors = [];
        page.on("pageerror", (e) => errors.push(e.message));
        await page.route("**/*", (route) =>
          new URL(route.request().url()).origin === origin
            ? route.continue()
            : route.abort(),
        );
        await page.goto(origin + "/recurrence-test?mode=" + mode);
        await page.waitForFunction(() => !!window.qa);
        const label = engine.name() + " " + mode;
        if (mode === "board") {
          await page.locator("[data-reply]").click();
          const input = page.locator("#board-text");
          await input.fill("Keep this composer draft");
          await input.evaluate((n) => n.setSelectionRange(5, 9));
          const geometry = () =>
            input.evaluate((n) => {
              const s = getComputedStyle(n);
              return {
                focusVisible: n.matches(":focus-visible"),
                border: s.borderTopWidth,
                borderColor: s.borderTopColor,
                outline: s.outlineWidth,
                outlineStyle: s.outlineStyle,
                offset: s.outlineOffset,
                outlineColor: s.outlineColor,
                boxShadow: s.boxShadow,
                width: n.getBoundingClientRect().width,
                height: n.getBoundingClientRect().height,
              };
            });
          const focused = await geometry();
          assert.equal(focused.focusVisible, true);
          if (baseline) {
            assert.equal(focused.offset, "3px");
            assert.equal(focused.outline, "2px");
            assert.equal(focused.border, "1px");
            assert.equal(focused.borderColor, focused.outlineColor);
          } else {
            assert.equal(focused.offset, "-2px");
            assert.equal(focused.outline, "2px");
            assert.equal(focused.outlineStyle, "solid");
          }
          await page.screenshot({
            path: artifacts + "/" + engine.name() + "-composer-focused.png",
          });
          await page.evaluate(() => qa.refresh());
          await expect(input).toHaveValue("Keep this composer draft");
          assert.deepEqual(
            await input.evaluate((n) => [n.selectionStart, n.selectionEnd]),
            [5, 9],
          );
          await expect(input).toBeFocused();
          await page.locator("#board-to").focus();
          await page.keyboard.press("Tab");
          await expect(input).toBeFocused();
          assert.equal((await geometry()).focusVisible, true);
          await page.locator(".board-head h2").click();
          await page.mouse.move(0, 0);
          const unfocused = await geometry();
          assert.equal(unfocused.outlineStyle, "none");
          assert.equal(unfocused.width, focused.width);
          assert.equal(unfocused.height, focused.height);
          evidence.push({ label, focused, unfocused });
        }
        const selectors =
          mode === "board"
            ? ["#board-to"]
            : ["[data-items-project]", "[data-items-status]"];
        for (const selector of selectors) {
          await page.setViewportSize({
            width: selector.includes("project") ? 600 : 1100,
            height: 800,
          });
          for (let cycle = 1; cycle <= 10; cycle++) {
            const control = page.locator(selector);
            await control.click();
            await expect
              .poll(() => control.evaluate((n) => n.matches(":open")))
              .toBe(true);
            await control.evaluate((n) => (window.watched = n));
            const rev = await page.evaluate(() => qa.refresh());
            await page.waitForTimeout(90);
            assert.equal(
              await control.evaluate(
                (n) => n === window.watched && n.matches(":open"),
              ),
              true,
              label + " refresh interrupted " + selector + " cycle " + cycle,
            );
            await page.keyboard.press("Escape");
            // Protocol Escape does not reliably dismiss macOS native menus; outside click
            // is the separately verified cancellation fallback for these nonmodal views.
            const afterEscape = await control.evaluate((n) =>
              n.matches(":open"),
            );
            await page.locator(".board-head h2").click();
            await expect(
              page.locator(
                mode === "board" ? "#board-messages" : "[data-work-items-sync]",
              ),
            ).toContainText(
              mode === "board" ? "Synthetic message " + rev : "Refresh " + rev,
            );
            const value =
              selector === "#board-to"
                ? cycle % 2
                  ? "agt_fixture"
                  : ""
                : selector.includes("project")
                  ? cycle % 2
                    ? ""
                    : "tsk_fixture"
                  : cycle % 2
                    ? "open"
                    : "";
            // Explicitly synthetic platform-consumed pointer release followed by commit.
            await control.dispatchEvent("pointerdown", {
              button: 0,
              pointerId: 42,
            });
            await page.evaluate(() => qa.refresh());
            await control.selectOption(value);
            await expect(control).toHaveValue(value);
            const committed = await page.evaluate(() => qa.refresh());
            await expect(
              page.locator(
                mode === "board" ? "#board-messages" : "[data-work-items-sync]",
              ),
            ).toContainText(
              mode === "board"
                ? "Synthetic message " + committed
                : "Refresh " + committed,
            );
            if ([1, 2, 10].includes(cycle))
              evidence.push({
                label,
                selector,
                cycle,
                openHeld: true,
                afterEscape,
                syntheticCommit: value,
                eventTrace: await page.evaluate(() => qa.state.events),
              });
          }
        }
        if (mode !== "board") {
          await page.setViewportSize({ width: 1100, height: 800 });
          await page.locator("[data-item-edit]").click();
          await page.locator("#work-item-title").fill("Retained editor draft");
          for (const selector of ["#work-item-status", "#work-item-priority"]) {
            for (let cycle = 1; cycle <= 10; cycle++) {
              const control = page.locator(selector);
              await control.click();
              await expect
                .poll(() => control.evaluate((n) => n.matches(":open")))
                .toBe(true);
              const before = await control.evaluate((n) => ({
                open: n.matches(":open"),
                focus: document.activeElement?.id,
              }));
              await page.evaluate(() => qa.refresh());
              const after = await control.evaluate((n) => ({
                open: n.matches(":open"),
                focus: document.activeElement?.id,
              }));
              await page.keyboard.press("Escape");
              if (!(await page.locator("#dialog").isVisible())) {
                const failure = {
                  label,
                  selector,
                  cycle,
                  before,
                  after,
                  defect: "dialog closed on picker Escape",
                  events: await page.evaluate(() => qa.state.events.slice(-20)),
                };
                evidence.push(failure);
                assert.equal(baseline, true, JSON.stringify(failure));
                await page.locator("[data-item-edit]").click();
                await expect(page.locator("#work-item-title")).toHaveValue(
                  "Retained editor draft",
                );
                console.log(
                  label +
                    " " +
                    selector +
                    " cycle " +
                    cycle +
                    ": baseline premature close",
                );
                continue;
              }
              await expect(page.locator("#dialog")).toBeVisible();
              await expect
                .poll(() => control.evaluate((n) => n.matches(":open")))
                .toBe(false);
              await expect(control).toBeFocused();
              const value = selector.endsWith("status")
                ? cycle % 2
                  ? "blocked"
                  : "open"
                : cycle % 2
                  ? "high"
                  : "normal";
              await control.selectOption(value);
              await expect(control).toHaveValue(value);
              await expect(page.locator("#work-item-title")).toHaveValue(
                "Retained editor draft",
              );
              if ([1, 2, 10].includes(cycle))
                evidence.push({
                  label,
                  selector,
                  cycle,
                  cancelRetainedDialog: true,
                  syntheticCommit: value,
                });
            }
          }
          assert.equal(await page.evaluate(() => qa.state.updates.length), 0);
          await page.keyboard.press("Escape");
          await expect(page.locator("#dialog")).not.toBeVisible();
        }
        assert.deepEqual(errors, []);
        console.log(
          label +
            (baseline
              ? ": baseline matrix recorded, including any premature closes above"
              : ": repeated first/second/tenth cancellation, refresh, synthetic commits and drafts passed"),
        );
        await context.close();
      }
    } finally {
      await browser.close();
    }
  }
} finally {
  await writeFile(
    artifacts + "/evidence.json",
    JSON.stringify({ baseline, evidence }, null, 2),
  );
  await server.close();
}
