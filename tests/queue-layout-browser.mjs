// Queue layout regression for Bug wi_c36a4696771a2072 revision 3,
// bounded Work Order message 1726. The fixture uses the real Queue view,
// #workspace/#mode-view hierarchy, production stylesheet order, and only
// synthetic data. Set QUEUE_LAYOUT_BASELINE=1 to assert the accepted baseline's
// three owner-observed failure classes. Set QUEUE_LAYOUT_BUILT_CSS to exercise a
// built production stylesheet as a second candidate composition check.
import { chromium, webkit, expect } from "@playwright/test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { mkdirSync, readFileSync, writeFileSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { createServer } from "vite";

const baselineCommit = "4e8e6c5b38905b25b5173d0044560be45e5b6e88";
const expectBaseline = process.env.QUEUE_LAYOUT_BASELINE === "1";
const builtCSSPath = process.env.QUEUE_LAYOUT_BUILT_CSS || "";
assert.ok(
  !(expectBaseline && builtCSSPath),
  "baseline and built-production modes are mutually exclusive",
);
const queueCSS = expectBaseline
  ? execFileSync("git", ["show", `${baselineCommit}:client/queue.css`], {
      encoding: "utf8",
    })
  : readFileSync(new URL("../client/queue.css", import.meta.url), "utf8");
const queueView = expectBaseline
  ? execFileSync("git", ["show", `${baselineCommit}:client/queue-view.js`], {
      encoding: "utf8",
    })
  : readFileSync(new URL("../client/queue-view.js", import.meta.url), "utf8");
const builtCSS = builtCSSPath
  ? readFileSync(path.resolve(builtCSSPath), "utf8")
  : "";
const runKind = expectBaseline ? "baseline" : builtCSS ? "built" : "candidate";
const artifactDir = path.resolve(".build/queue-layout");
mkdirSync(artifactDir, { recursive: true });
const queueStylePath = path.join(artifactDir, "fixture-queue.css");
const queueViewPath = path.join(artifactDir, "fixture-queue-view.js");
const fixtureScriptPath = path.join(artifactDir, "fixture.js");
writeFileSync(queueStylePath, queueCSS);
writeFileSync(queueViewPath, queueView);

const scenarios = [
  { name: "narrow-320", width: 320, height: 720, deviceScaleFactor: 1 },
  { name: "narrow-390", width: 390, height: 780, deviceScaleFactor: 1 },
  { name: "boundary-760", width: 760, height: 900, deviceScaleFactor: 1 },
  { name: "boundary-761", width: 761, height: 900, deviceScaleFactor: 1 },
  { name: "boundary-768", width: 768, height: 900, deviceScaleFactor: 1 },
  { name: "boundary-800", width: 800, height: 900, deviceScaleFactor: 1 },
  { name: "boundary-900", width: 900, height: 900, deviceScaleFactor: 1 },
  { name: "boundary-1024", width: 1024, height: 900, deviceScaleFactor: 1 },
  { name: "desktop-1366", width: 1366, height: 900, deviceScaleFactor: 1 },
  // 1397 CSS pixels is a plausible half-scale comparison to the supplied
  // 2794px raster, not a claim about the owner's actual viewport or DPR.
  { name: "desktop-1397", width: 1397, height: 852, deviceScaleFactor: 2 },
  { name: "desktop-1920", width: 1920, height: 1080, deviceScaleFactor: 1 },
  // Models a 1366x900 display at 200% zoom with a constrained CSS viewport.
  // This is emulation, not native browser zoom.
  {
    name: "zoom-200-equivalent",
    width: 683,
    height: 450,
    deviceScaleFactor: 2,
  },
];

const dependencyStyle = (specifier) =>
  `/@fs/${fileURLToPath(import.meta.resolve(specifier))}`;

const styleImports = builtCSS
  ? ""
  : `import '/client/work-items.css';
import '/@fs/${queueStylePath}';
import '${dependencyStyle("@fontsource/jetbrains-mono/latin-400.css")}';
import '${dependencyStyle("@fontsource/ibm-plex-mono/latin-400.css")}';
import '${dependencyStyle("@fontsource/source-code-pro/latin-400.css")}';
import '${dependencyStyle("@fontsource/source-code-pro/latin-700.css")}';
import '/client/fonts.css';
import '${dependencyStyle("@xterm/xterm/css/xterm.css")}';
import '/client/style.css';`;
const stylesheet = builtCSS
  ? '<link rel="stylesheet" href="/qa/built.css">'
  : "";
const fixtureScript = `${styleImports}
import {createQueueView} from '/@fs/${queueViewPath}';
import {setupModes} from '/client/modes.js';
const longProject='Tailterm Development Receiving Coordination With An Intentionally Long Synthetic Project Name';
const tasks=[
 {id:'tsk_1111111111111111',name:longProject,status:'open'},
 {id:'tsk_2222222222222222',name:'Synthetic Secondary Project With A Long Readable Name',status:'open'},
 {id:'tsk_3333333333333333',name:'Synthetic Closed Project With Retained Queue History',status:'closed'},
];
const longID='wi_'+('abcdef0123456789'.repeat(12));
const longToken='unbroken-synthetic-queue-identifier-'+('0123456789abcdef'.repeat(12));
const description=Array.from({length:12},(_,index)=>
 'Synthetic detail paragraph '+String(index+1).padStart(2,'0')+
 ' preserves readable Queue evidence, explicit scope, retry identities, and exact assignment context. '+longToken
).join(' ');
const entries=Array.from({length:18},(_,index)=>({
 id:'que_'+String(index+1).padStart(16,'0'),targetTaskId:tasks[0].id,
 sourceTaskId:index%2?tasks[1].id:tasks[0].id,itemId:index===0?longID:'wi_'+String(index+1).padStart(16,'0'),
 offeredItemRevision:index+1,currentItemRevision:index+1,revision:index+2,cycle:1,
 state:'waiting',queuePriority:index%3===0?'urgent':index%2?'high':'normal',
 eligible:index!==1,stale:index===2,reviewNeeded:index===3,pendingUpdate:index===4,
 reconciliationNeeded:index===5,claimantAgentId:'',workerRunId:'',orchestratorAgentId:'agt_1111111111111111',
 item:{kind:index%2?'feature':'bug',title:index===0?
   'Complete remaining structured audit and synthetic acceptance evidence without losing any Queue text '+longToken:
   'Synthetic Queue row '+String(index+1).padStart(2,'0')+' with a realistic long title for wrapping and bounds',
   description:index===0?description:'Synthetic description '+(index+1),status:'in_progress',priority:'high'}
}));
const client={base:'isolated://queue-layout',token:'synthetic-non-secret',
 async capabilities(){return {queue:{versions:[1]}}},async listTasks(){return structuredClone(tasks)},
 async listQueue(taskId,{includeTerminal}={}){return {entries:structuredClone(entries.filter(entry=>entry.targetTaskId===taskId&&(includeTerminal||entry.state==='waiting'))),cursor:''}},
 subscribe(){return {stop(){}}}
};
const modal=document.querySelector('#dialog');
const view=createQueueView({client:()=>client,configure(){},notice(text){const node=document.querySelector('#notice');node.textContent=text;node.hidden=false},
 dialog(title,body){modal.innerHTML='<h2>'+title+'</h2>'+body;modal.showModal()},closeDialog(){modal.close()}});
const main=document.querySelector('#workspace > main');
const modes=setupModes({header:main.querySelector(':scope > header'),main,onChange:(mode,container)=>{
 view.hide();container.replaceChildren();if(mode==='queue'){view.mount(container);window.rendered=view.show(tasks[0].id)}
}});
modes.set('queue');await window.rendered;window.qa={view,modes,tasks,entries,longID,longToken};`;
writeFileSync(fixtureScriptPath, fixtureScript);
const html = `<!doctype html><html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
${stylesheet}<title>Queue layout synthetic fixture</title></head>
<body data-mode="terminals"><div id="app"><div id="workspace">
<aside><div class="brand"><span class="brand-icon" aria-hidden="true">▦</span><strong>tailterm</strong></div>
<div class="sidebar-section"><span>SERVERS</span><button id="all-servers" aria-pressed="true">All</button></div>
<input id="filter" class="filter" placeholder="⌕  Find a server…" aria-label="Find a server">
<nav id="server-list"><button class="server-item selected" aria-pressed="true"><span class="node-icon">▤</span><div><strong>Synthetic Mini</strong><small>Isolated · Auto</small></div><span class="node-dot online"></span></button></nav>
<button id="discover">⌕ Discover devices</button><div class="sidebar-bottom"><button>SSH keys</button><button>Lock vault</button></div></aside>
<main><header><div class="header-right"><button id="tailscale-login"><span class="status-dot online"></span> Tailscale connected</button><button>Expand</button><button>⛶</button></div></header>
<section class="terminal-shell" hidden></section></main></div><dialog id="dialog"></dialog><p id="notice" role="status" hidden></p></div>
<script type="module" src="/@fs/${fixtureScriptPath}"></script></body></html>`;

const server = await createServer({
  configFile: false,
  root: process.cwd(),
  server: {
    host: "127.0.0.1",
    port: 0,
    fs: { allow: [process.cwd(), path.resolve(process.cwd(), "../../..")] },
  },
  plugins: [
    {
      name: "queue-layout-fixture",
      configureServer(vite) {
        vite.middlewares.use("/qa/built.css", (_request, response) => {
          response.setHeader("Content-Type", "text/css");
          response.end(builtCSS);
        });
        vite.middlewares.use("/queue-layout", (_request, response) => {
          response.setHeader("Content-Type", "text/html");
          response.end(html);
        });
      },
    },
  ],
});
await server.listen();
const origin = `http://127.0.0.1:${server.httpServer.address().port}`;

async function geometry(page) {
  return page.evaluate(() => {
    const box = (node) => {
      const value = node.getBoundingClientRect();
      return Object.fromEntries(
        ["x", "y", "width", "height", "right", "bottom"].map((key) => [
          key,
          Math.round(value[key] * 100) / 100,
        ]),
      );
    };
    const textFits = (node, bounds = node.getBoundingClientRect()) => {
      const walker = document.createTreeWalker(node, NodeFilter.SHOW_TEXT);
      let current;
      while ((current = walker.nextNode())) {
        if (!current.textContent.trim()) continue;
        const range = document.createRange();
        range.selectNodeContents(current);
        for (const value of range.getClientRects()) {
          if (
            value.width > 0 &&
            (value.left < bounds.left - 1 || value.right > bounds.right + 1)
          )
            return false;
        }
      }
      return true;
    };
    const workspace = document.querySelector("#workspace");
    const mode = document.querySelector("#mode-view");
    const outer = mode.firstElementChild;
    const rail = outer.querySelector(":scope > .board-rail");
    const main = outer.querySelector(":scope > .queue-main");
    const head = main.querySelector(":scope > .board-head");
    const layout = main.querySelector(":scope > .queue-layout");
    const list = layout.querySelector(":scope > .queue-list");
    const detail = layout.querySelector(":scope > .queue-detail");
    const rows = [...list.querySelectorAll(".queue-row")];
    const railButton = rail.querySelector("[data-queue-task]");
    const railTitle = railButton.querySelector(".board-task-name");
    const railSubtitle = railButton.querySelector(".fine");
    const toggle = head.querySelector(".queue-history-toggle");
    const checkbox = toggle.querySelector("input");
    const toggleText = toggle.querySelector("span") || toggle;
    const readable = [
      ...rows
        .slice(0, 3)
        .flatMap((row) => [...row.querySelectorAll("strong,small")]),
      ...detail.querySelectorAll("h3,p,dd"),
      railTitle,
      railSubtitle,
    ];
    const controls = [...outer.querySelectorAll("button,input,select")].filter(
      (node) =>
        node.getBoundingClientRect().width > 0 &&
        (!node.checkVisibility || node.checkVisibility()),
    );
    return {
      viewport: { width: innerWidth, height: innerHeight },
      document: {
        clientWidth: document.documentElement.clientWidth,
        scrollWidth: document.documentElement.scrollWidth,
      },
      workspace: box(workspace),
      mode: box(mode),
      outer: box(outer),
      rail: box(rail),
      main: box(main),
      head: box(head),
      layout: box(layout),
      list: box(list),
      detail: box(detail),
      rows: rows.slice(0, 3).map(box),
      railButton: box(railButton),
      railTitle: box(railTitle),
      railSubtitle: box(railSubtitle),
      toggle: box(toggle),
      checkbox: box(checkbox),
      toggleText: box(toggleText),
      readable: readable.map((node) => ({
        tag: node.tagName,
        className: node.className,
        region: rail.contains(node) ? "rail" : "main",
        box: box(node),
        clientWidth: node.clientWidth,
        scrollWidth: node.scrollWidth,
        textFits: textFits(node),
      })),
      controls: controls.map((node) => ({
        tag: node.tagName,
        label: (
          node.textContent ||
          node.getAttribute("aria-label") ||
          node.type
        )
          .trim()
          .slice(0, 80),
        region: rail.contains(node) ? "rail" : "main",
        box: box(node),
      })),
      scroll: {
        top: main.scrollTop,
        clientHeight: main.clientHeight,
        scrollHeight: main.scrollHeight,
      },
    };
  });
}

async function rowStyle(locator) {
  return locator.evaluate((node) => {
    const style = getComputedStyle(node);
    return {
      background: style.backgroundColor,
      border: style.borderTopColor,
      outlineStyle: style.outlineStyle,
      outlineWidth: style.outlineWidth,
    };
  });
}

function issuesFor(measured, outerStacked) {
  const issues = [];
  const overlapX = (left, right) =>
    Math.min(left.right, right.right) - Math.max(left.x, right.x) > 1;
  const overlapY = (top, bottom) =>
    Math.min(top.bottom, bottom.bottom) - Math.max(top.y, bottom.y) > 1;
  if (measured.document.scrollWidth > measured.document.clientWidth + 1)
    issues.push("document-horizontal-overflow");
  if (outerStacked) {
    if (
      overlapY(measured.rail, measured.main) ||
      measured.rail.bottom > measured.main.y + 1
    )
      issues.push("rail-main-overlap");
  } else {
    if (
      overlapX(measured.rail, measured.main) ||
      measured.rail.right > measured.main.x + 1
    )
      issues.push("rail-main-overlap");
  }
  const detailShouldStack = outerStacked || measured.layout.width <= 540;
  if (detailShouldStack) {
    if (
      overlapY(measured.list, measured.detail) ||
      measured.list.bottom > measured.detail.y + 1
    )
      issues.push("list-detail-overlap");
  } else if (
    overlapX(measured.rows[0], measured.detail) ||
    measured.rows.some((row) => row.right > measured.detail.x + 1)
  )
    issues.push("list-detail-overlap");
  if (
    measured.railTitle.bottom > measured.railSubtitle.y + 1 ||
    measured.railSubtitle.y - measured.railTitle.bottom < 1
  )
    issues.push("rail-label-collision");
  if (
    measured.checkbox.width > 20 ||
    measured.checkbox.height > 20 ||
    measured.toggle.width > 190 ||
    measured.toggleText.x - measured.checkbox.right > 10
  )
    issues.push("history-control-stretched");
  if (
    measured.head.bottom > measured.layout.y + 1 ||
    measured.readable.some((node) => {
      const region = node.region === "rail" ? measured.rail : measured.main;
      return (
        node.scrollWidth > node.clientWidth + 1 ||
        !node.textFits ||
        node.box.x < region.x - 1 ||
        node.box.right > region.right + 1
      );
    })
  )
    issues.push("text-out-of-bounds");
  if (
    measured.controls.some((control) =>
      control.region === "rail"
        ? control.box.y < measured.rail.y - 1 ||
          control.box.bottom > measured.rail.bottom + 1
        : control.box.x < measured.mode.x - 1 ||
          control.box.right > measured.mode.right + 1,
    )
  )
    issues.push("control-out-of-bounds");
  if (measured.scroll.scrollHeight <= measured.scroll.clientHeight)
    issues.push("no-vertical-scroll-range");
  if (measured.scroll.clientHeight < 100)
    issues.push("insufficient-scroll-viewport");
  return [...new Set(issues)];
}

const results = [];
const baselineCodes = new Set();
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
        const browserErrors = [];
        page.on("pageerror", (error) => browserErrors.push(error.message));
        page.on("console", (message) => {
          if (message.type() === "error")
            browserErrors.push(`console: ${message.text()}`);
        });
        page.on("requestfailed", (request) =>
          browserErrors.push(
            `request: ${request.url()} ${request.failure()?.errorText || "failed"}`,
          ),
        );
        const label = `${runKind}-${engine.name()}-${scenario.name}`;
        try {
          await page.goto(`${origin}/queue-layout`);
          await page
            .waitForFunction(() => Boolean(window.qa), null, {
              timeout: 15_000,
            })
            .catch((error) => {
              throw new Error(
                `${error.message}; browser diagnostics: ${browserErrors.join(" | ")}`,
              );
            });
          await expect(page.locator(".queue-detail h3")).toContainText(
            "Complete remaining structured audit",
          );
          const screenshot = path.join(artifactDir, `${label}.png`);
          await page.screenshot({ path: screenshot, fullPage: false });
          const interactionIssues = [];
          const first = page.locator("[data-queue-entry]").first();
          const second = page.locator("[data-queue-entry]").nth(1);

          const selectedRest = await rowStyle(first);
          const unselectedRest = await rowStyle(second);
          if (selectedRest.background === unselectedRest.background)
            interactionIssues.push("selection-fill-missing");

          let unselectedHover = null;
          if (!expectBaseline) {
            await second.hover();
            unselectedHover = await rowStyle(second);
            if (unselectedHover.background !== unselectedRest.background)
              interactionIssues.push("hover-changed-unselected-fill");
            if (unselectedHover.border === unselectedRest.border)
              interactionIssues.push("hover-outline-missing");
            await page.mouse.move(0, 0);
          }

          await first.focus();
          const selectedFocus = await rowStyle(first);
          if (selectedFocus.background !== selectedRest.background)
            interactionIssues.push("focus-changed-selected-fill");

          await second.focus();
          await expect(second).toBeFocused();
          const focusOutline = await rowStyle(second);
          assert.notEqual(
            focusOutline.outlineStyle,
            "none",
            `${label}: focus outline missing`,
          );
          await second.press("Enter");
          await expect(
            page.locator('[data-queue-entry][aria-pressed="true"]'),
          ).toHaveCount(1);
          await expect(
            page.locator('[data-queue-entry][aria-pressed="true"]'),
          ).toContainText("Synthetic Queue row 02");
          const keyboardSelected = await rowStyle(second);
          const keyboardUnselected = await rowStyle(first);
          if (
            keyboardSelected.background !== selectedRest.background ||
            keyboardUnselected.background !== unselectedRest.background
          )
            interactionIssues.push("keyboard-selection-fill-did-not-move");

          let selectedHover = null;
          if (!expectBaseline) {
            await second.hover();
            selectedHover = await rowStyle(second);
            if (selectedHover.background !== selectedRest.background)
              interactionIssues.push("hover-changed-selected-fill");
            await page.mouse.move(0, 0);
          }

          // The baseline's detail physically intercepts the row. A programmatic
          // click is used only to continue collecting all baseline geometry
          // after that known failure; candidate runs retain a real pointer
          // hit-test.
          if (expectBaseline) await first.evaluate((node) => node.click());
          else await first.click();
          await expect(
            page.locator('[data-queue-entry][aria-pressed="true"]'),
          ).toContainText("Complete remaining structured audit");
          const pointerSelected = await rowStyle(first);
          const pointerUnselected = await rowStyle(second);
          if (
            pointerSelected.background !== selectedRest.background ||
            pointerUnselected.background !== unselectedRest.background
          )
            interactionIssues.push("pointer-selection-fill-did-not-move");

          if (!expectBaseline) {
            const historyLabel = page.locator(".queue-history-toggle span");
            await historyLabel.click();
            await expect(
              page.locator(".queue-history-toggle input"),
            ).toBeChecked();
            await page.locator(".queue-history-toggle span").click();
            await expect(
              page.locator(".queue-history-toggle input"),
            ).not.toBeChecked();
            await expect(page.locator(".queue-detail h3")).toContainText(
              "Complete remaining structured audit",
            );
          }

          const main = page.locator(".queue-main");
          await main.evaluate((node) => node.scrollTo(0, 240));
          await expect
            .poll(() => main.evaluate((node) => node.scrollTop))
            .toBeGreaterThan(0);
          const measured = await geometry(page);
          const issues = [
            ...new Set([
              ...issuesFor(measured, scenario.width <= 760),
              ...interactionIssues,
            ]),
          ];
          for (const issue of issues) baselineCodes.add(issue);
          if (!expectBaseline)
            assert.deepEqual(
              issues,
              [],
              `${label}: ${JSON.stringify({ issues, measured })}`,
            );
          assert.deepEqual(browserErrors, [], `${label}: browser errors`);
          results.push({
            runKind,
            engine: engine.name(),
            scenario,
            screenshot,
            issues,
            measured,
            selectionStyles: {
              selectedRest,
              unselectedRest,
              unselectedHover,
              selectedFocus,
              keyboardSelected,
              keyboardUnselected,
              selectedHover,
              pointerSelected,
              pointerUnselected,
            },
          });
          console.log(
            `${label}: ${issues.length ? issues.join(", ") : "nonoverlapping and bounded"}`,
          );
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
  writeFileSync(
    path.join(artifactDir, `${runKind}-results.json`),
    JSON.stringify({ runKind, baselineCommit, builtCSSPath, results }, null, 2),
  );
}

if (expectBaseline) {
  for (const expected of [
    "list-detail-overlap",
    "rail-label-collision",
    "history-control-stretched",
    "selection-fill-missing",
  ])
    assert.ok(
      baselineCodes.has(expected),
      `baseline did not reproduce ${expected}`,
    );
  console.log(
    "PASS: accepted baseline reproduced Queue list/detail overlap, rail label collision, and stretched history control",
  );
} else {
  assert.equal(results.length, scenarios.length * 2);
  console.log(
    `PASS: ${runKind} Queue layout is bounded, readable, focusable, selectable, and scrollable in Chromium/WebKit`,
  );
}
