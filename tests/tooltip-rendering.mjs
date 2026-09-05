import { chromium } from "@playwright/test";
import { readFile } from "node:fs/promises";
import assert from "node:assert/strict";
const browser = await chromium.launch();
try {
  const page = await browser.newPage();
  await page.setContent(
    '<button id="action" title="Workspace action">Action</button><div class="xterm"><div id="rows"></div></div>',
  );
  const source = await readFile("client/tooltips.js", "utf8");
  await page.addScriptTag({
    content:
      source.replace(
        "export function setupTooltips",
        "function setupTooltips",
      ) + "\nsetupTooltips();",
  });
  const result = await page.evaluate(async () => {
    let scans = 0;
    const query = Element.prototype.querySelectorAll;
    Element.prototype.querySelectorAll = function (selector) {
      if (selector === "[title]" && this.closest(".xterm")) scans++;
      return query.call(this, selector);
    };
    const rows = document.querySelector("#rows"),
      start = performance.now();
    for (let frame = 0; frame < 40; frame++) {
      const fragment = document.createDocumentFragment();
      for (let i = 0; i < 60; i++) {
        const row = document.createElement("div");
        row.className = "xterm-row";
        for (let j = 0; j < 20; j++) {
          const span = document.createElement("span");
          span.textContent = "output";
          row.append(span);
        }
        fragment.append(row);
      }
      rows.replaceChildren(fragment);
      await new Promise((resolve) => setTimeout(resolve, 0));
    }
    const action = document.querySelector("#action");
    action.title = "Updated workspace action";
    await new Promise((resolve) => setTimeout(resolve, 0));
    Element.prototype.querySelectorAll = query;
    return {
      terminalTooltipScans: scans,
      elapsedMs: Math.round(performance.now() - start),
      tooltip: action.dataset.tooltip,
    };
  });
  console.log(JSON.stringify(result));
  assert.equal(result.tooltip, "Updated workspace action");
  if (!process.argv.includes("--baseline"))
    assert.equal(
      result.terminalTooltipScans,
      0,
      "Terminal redraws must not trigger tooltip subtree scans",
    );
} finally {
  await browser.close();
}
