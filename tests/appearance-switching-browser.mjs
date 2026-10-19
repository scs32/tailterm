import assert from "node:assert/strict";
import { randomUUID } from "node:crypto";
import { once } from "node:events";
import { chromium, webkit } from "@playwright/test";
import { createStaticPreviewServer } from "../scripts/preview-static.mjs";
// The suite serves dist-static itself on a port the system picks, and checks a
// per-run token so it never drives a server it did not start.
const token = randomUUID();
const server = createStaticPreviewServer("dist-static", { token });
server.listen(0, "127.0.0.1");
await once(server, "listening");
const origin = `http://127.0.0.1:${server.address().port}`;
let browser;
for (const [signal, code] of [
  ["SIGINT", 130],
  ["SIGTERM", 143],
])
  process.once(signal, async () => {
    setTimeout(() => process.exit(code), 8000).unref();
    await browser?.close().catch(() => {});
    server.close();
    process.exit(code);
  });
try {
  for (const engine of [chromium, webkit]) {
    browser = await engine.launch();
    try {
      const page = await browser.newPage({
        viewport: { width: 1100, height: 800 },
      });
      page.setDefaultTimeout(10000);
      const errors = [];
      page.on("pageerror", (e) => errors.push(e.message));
      // Hold the Nerd Font responses until the dialog has been laid out, as
      // on a slow connection, so its first layout uses the fallback font.
      let releaseNerdFonts;
      const nerdFontsHeld = new Promise((r) => (releaseNerdFonts = r));
      await page.route(/-nerd-[^/]*\.woff2$/, async (route) => {
        await nerdFontsHeld;
        await route.continue();
      });
      // Waits, at most 5 s, until every font the page asked for has loaded
      // and that layout has been painted, and fails naming any face still
      // pending.
      const settleFonts = async (label, target = page) => {
        const pending = await target.evaluate(async () => {
          const deadline = performance.now() + 5000;
          do {
            await Promise.race([
              document.fonts.ready,
              new Promise((r) => setTimeout(r, deadline - performance.now())),
            ]);
            for (let frame = 0; frame < 2; frame++)
              await new Promise(requestAnimationFrame);
          } while (
            document.fonts.status !== "loaded" &&
            performance.now() < deadline
          );
          if (document.fonts.status === "loaded") return [];
          const faces = [...document.fonts]
            .filter((f) => f.status !== "loaded" && f.status !== "unloaded")
            .map((f) => `${f.family} ${f.weight} ${f.status}`);
          return faces.length ? faces : ["no face reports its status"];
        });
        assert.deepEqual(
          pending,
          [],
          `${engine.name()} ${label}: fonts still loading after 5 s: ${pending.join("; ")}`,
        );
      };
      const response = await page.goto(`${origin}/`);
      assert.equal(
        response.headers()["x-tailterm-preview-token"],
        token,
        `${engine.name()}: the page came from a server this run did not start`,
      );
      await page
        .locator("#password")
        .fill("temporary appearance test password");
      await page.locator("#unlock-button").click();
      await page.locator("#appearance").click();
      // The typeface samples must not change the dialog's size when their
      // Nerd Font faces arrive after it opens.
      const size = () =>
        page.locator("#dialog").evaluate(async (d) => {
          for (let frame = 0; frame < 2; frame++)
            await new Promise(requestAnimationFrame);
          return {
            fonts: document.fonts.status,
            height: d.scrollHeight,
            card: d.querySelector(".font-grid").getBoundingClientRect().height,
          };
        });
      const early = await size();
      assert.equal(
        early.fonts,
        "loading",
        `${engine.name()}: the Nerd Font faces were held while the dialog opened`,
      );
      releaseNerdFonts();
      await settleFonts("after the held Nerd Font faces arrive");
      assert.deepEqual(
        await size(),
        { ...early, fonts: "loaded" },
        `${engine.name()}: the appearance dialog keeps its size when the Nerd Font faces arrive after it opens`,
      );
      for (const width of [1100, 390]) {
        await page.setViewportSize({ width, height: 800 });
        for (const attr of ["theme", "font"])
          for (const id of await page
            .locator(`[data-${attr}-choice]`)
            .evaluateAll(
              (ns, attr) =>
                ns.map((n) => n.getAttribute(`data-${attr}-choice`)),
              attr,
            )) {
            const button = page.locator(`[data-${attr}-choice="${id}"]`);
            await button.scrollIntoViewIfNeeded();
            // Choosing a font can start loading its faces, so read only after
            // they have settled.
            const metrics = async () => {
              await settleFonts(`${width} ${attr} ${id}`);
              return page.locator("#dialog").evaluate((d) => ({
                scroll: d.scrollTop,
                width: d.clientWidth,
                scrollWidth: d.scrollWidth,
                height: d.scrollHeight,
                head: d.querySelector(".dialog-head").getBoundingClientRect()
                  .top,
                preview: d
                  .querySelector("#appearance-preview")
                  .getBoundingClientRect().height,
                card: d.querySelector(".font-grid").getBoundingClientRect()
                  .height,
              }));
            };
            const before = await metrics();
            await button.click();
            await page.waitForTimeout(200);
            assert.deepEqual(
              await metrics(),
              before,
              `${engine.name()} ${width} ${attr} ${id}: dialog geometry and scroll position remain stable`,
            );
          }
      }
      assert.deepEqual(errors, []);
      // With a Nerd Font chosen as the terminal font, the dialog text uses
      // that face. Each case starts a fresh context so the face is not cached.
      const delayMs = Number(process.env.APPEARANCE_FONT_DELAY_MS || 150);
      const waits = [];
      for (const font of ["cascadia", "fira"])
        for (const arrival of ["held", "delayed", "stale", "modal"]) {
          const label = `${engine.name()} ${font} ${arrival}`;
          const context = await browser.newContext({
            viewport: { width: 1100, height: 800 },
          });
          try {
            await context.addInitScript((font) => {
              if (!localStorage.getItem("tailserve.appearance"))
                localStorage.setItem(
                  "tailserve.appearance",
                  JSON.stringify({ font }),
                );
              // Records when each dialog opens, and whether the click that
              // asked for it had already opened it when its handlers ran.
              window.showModals = [];
              const showModal = HTMLDialogElement.prototype.showModal;
              HTMLDialogElement.prototype.showModal = function () {
                window.showModals.push(performance.now());
                // The faces still downloading when the dialog shows.
                window.loadingAtOpen ??= [...document.fonts]
                  .filter((f) => f.status === "loading")
                  .map((f) => f.family.replace(/"/g, ""));
                return showModal.call(this);
              };
              document.addEventListener(
                "click",
                (e) => {
                  if (e.target.closest?.("#appearance"))
                    window.clickedAt ??= performance.now();
                },
                true,
              );
              document.addEventListener("click", (e) => {
                if (e.target.closest?.("#appearance"))
                  window.openInClick = document.querySelector("#dialog").open;
              });
            }, font);
            const page = await context.newPage();
            page.setDefaultTimeout(10000);
            const caseErrors = [];
            page.on("pageerror", (e) => caseErrors.push(e.message));
            let releaseFaces;
            const facesHeld = new Promise((r) => (releaseFaces = r));
            await page.route(/-nerd-[^/]*\.woff2$/, async (route) => {
              await facesHeld;
              await route.continue();
            });
            await page.goto(`${origin}/`);
            await page
              .locator("#password")
              .fill("temporary appearance test password");
            await page.locator("#unlock-button").click();
            await page.locator("#appearance").waitFor();
            const metrics = () =>
              page.locator("#dialog").evaluate(async (d) => {
                for (let frame = 0; frame < 2; frame++)
                  await new Promise(requestAnimationFrame);
                const strong = d.querySelector(".font-choice strong");
                return {
                  height: d.scrollHeight,
                  card: d.querySelector(".font-grid").getBoundingClientRect()
                    .height,
                  uiFont: d.style.getPropertyValue("--appearance-ui-font"),
                  textFont: getComputedStyle(strong).fontFamily,
                  textWidth: strong.getBoundingClientRect().width,
                };
              });
            const family = await page.evaluate(
              (font) =>
                [...document.fonts].find((f) =>
                  f.family.toLowerCase().includes(font),
                ).family,
              font === "cascadia" ? "caskaydia" : "firacode",
            );
            if (arrival === "delayed") setTimeout(releaseFaces, delayMs);
            await page.locator("#appearance").click();
            // A repeated click during the wait must not open a second time.
            if (arrival === "held")
              await page.evaluate(() =>
                document.querySelector("#appearance").click(),
              );
            if (arrival === "stale" || arrival === "modal") {
              // Another dialog opened during the wait keeps the screen: one
              // from the app's dialog(), or a module's own modal.
              if (arrival === "stale") await page.locator("#keys").click();
              else
                await page.evaluate(() =>
                  document.body
                    .appendChild(document.createElement("dialog"))
                    .showModal(),
                );
              await page.waitForTimeout(500);
              assert.equal(
                await page.locator("#dialog .theme-grid").count(),
                0,
                `${label}: a stale wait does not open over another dialog`,
              );
              assert.equal(
                await page.evaluate(() => window.showModals.length),
                1,
                `${label}: only the other dialog opened`,
              );
              releaseFaces();
              assert.deepEqual(caseErrors, []);
              continue;
            }
            await page.locator("#dialog .theme-grid").waitFor();
            const opened = await page.evaluate(() => ({
              wait: Math.round(window.showModals[0] - window.clickedAt),
              count: window.showModals.length,
              inClick: window.openInClick,
              fonts: document.fonts.status,
              loading: window.loadingAtOpen,
            }));
            const early = await metrics();
            releaseFaces();
            await settleFonts(
              `${font} ${arrival} after the face arrives`,
              page,
            );
            assert.deepEqual(
              await metrics(),
              early,
              `${label}: the dialog keeps its size and wrapping when the chosen face arrives`,
            );
            // Text is visible: it renders in a face that had loaded when the
            // dialog showed, or in the generic fallback, which needs none.
            assert.ok(early.textWidth > 0, `${label}: dialog text is visible`);
            assert.ok(
              early.textFont === "monospace" ||
                !opened.loading.includes(family.replace(/"/g, "")),
              `${label}: the dialog text face was not downloading when it showed`,
            );
            assert.equal(opened.inClick, false, `${label}: the open waited`);
            assert.ok(
              opened.wait < 450,
              `${label}: the open waited at most the 300 ms bound, took ${opened.wait} ms`,
            );
            assert.equal(opened.count, 1, `${label}: the dialog opened once`);
            if (arrival === "held") {
              assert.equal(
                opened.fonts,
                "loading",
                `${label}: the chosen face was held while the dialog opened`,
              );
              assert.ok(
                opened.wait >= 290,
                `${label}: the open waited for the bound, took ${opened.wait} ms`,
              );
              assert.equal(
                early.uiFont,
                "monospace",
                `${label}: the dialog opened in the fallback, which needs no download`,
              );
              assert.equal(
                early.textFont,
                "monospace",
                `${label}: the dialog text renders in the fallback`,
              );
            } else
              assert.equal(
                early.uiFont.split(",")[0].replace(/"/g, ""),
                family.replace(/"/g, ""),
                `${label}: a face arriving within the bound is used`,
              );
            waits.push(`${font} ${arrival} ${opened.wait} ms`);
            // The face is loaded now, so the next open is not delayed.
            await page.locator("#dialog-close").click();
            await page.locator("#appearance").click();
            const reopened = await page.evaluate(() => window.openInClick);
            assert.equal(
              reopened,
              true,
              `${label}: with the face loaded, the dialog opens within the click`,
            );
            assert.equal(
              (await metrics()).uiFont.split(",")[0].replace(/"/g, ""),
              family.replace(/"/g, ""),
              `${label}: the reopened dialog uses the chosen face`,
            );
            assert.deepEqual(caseErrors, []);
          } finally {
            await context.close();
          }
        }
      console.log(
        `${engine.name()}: theme/font switching preserves dialog layout on desktop and mobile.`,
      );
      console.log(
        `${engine.name()}: a chosen Nerd Font arriving late keeps the dialog layout; opens waited ${waits.join(", ")} (face delay ${delayMs} ms).`,
      );
    } finally {
      await browser.close();
    }
  }
} finally {
  server.closeAllConnections();
  await new Promise((r) => server.close(r));
}
