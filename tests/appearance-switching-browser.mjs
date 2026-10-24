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
// The app waits at most this long for the chosen face before opening the
// dialog in the fallback (APPEARANCE_FONT_WAIT_MS in client/main.js).
const FONT_WAIT_MS = 300;
// Browsers report a ResizeObserver callback that changes what it observes as
// this error. It is benign and a loaded host makes it likely, so only this
// exact text is ignored; every other page error still fails the run.
const BENIGN_ERROR =
  "ResizeObserver loop completed with undelivered notifications.";
const collectErrors = (page) => {
  const errors = [];
  page.on("pageerror", (e) => {
    if (e.message !== BENIGN_ERROR) errors.push(e.message);
  });
  return errors;
};
const passphrase = "temporary appearance test password";
const unlock = async (page) => {
  await page.locator("#password").fill(passphrase);
  await page.locator("#unlock-button").click();
};
// An unlock submitted before the vault status has been read must neither
// reload the page nor drop the typed passphrase. The status read is held by
// deferring IndexedDB's open until the test releases it.
const checkEarlyUnlock = async (browser, name) => {
  const context = await browser.newContext();
  try {
    await context.addInitScript(() => {
      let release;
      const released = new Promise((r) => (release = r));
      window.releaseVaultStatus = release;
      const open = IDBFactory.prototype.open;
      IDBFactory.prototype.open = function (...args) {
        const request = open.apply(this, args);
        let handler;
        Object.defineProperty(request, "onsuccess", {
          set: (f) => (handler = f),
          get: () => handler,
        });
        request.addEventListener("success", (e) =>
          released.then(() => handler?.call(request, e)),
        );
        return request;
      };
    });
    const page = await context.newPage();
    page.setDefaultTimeout(10000);
    const errors = collectErrors(page);
    await page.goto(`${origin}/`);
    let navigations = 0;
    page.on("framenavigated", (frame) => {
      if (frame === page.mainFrame()) navigations++;
    });
    await page.locator("#password").fill(passphrase);
    // Enter, a click and a scripted submit, all before the status is read.
    await page.locator("#password").press("Enter");
    await page.locator("#unlock-button").click({ force: true });
    await page.locator("#unlock").evaluate((form) => form.requestSubmit());
    await page.waitForTimeout(300);
    assert.equal(
      await page.locator("#vault-hint").textContent(),
      "Checking encrypted vault…",
      `${name}: the early unlock was submitted before the vault status was read`,
    );
    assert.equal(
      navigations,
      0,
      `${name}: an early unlock does not reload the page`,
    );
    assert.equal(page.url(), `${origin}/`, `${name}: the page did not submit`);
    assert.equal(
      await page.locator("#password").inputValue(),
      passphrase,
      `${name}: an early unlock keeps the typed passphrase`,
    );
    assert.equal(await page.locator("#workspace").count(), 0);
    await page.evaluate(() => window.releaseVaultStatus());
    await page.locator("#unlock-button").click();
    await page.locator("#workspace").waitFor();
    assert.equal(navigations, 0, `${name}: unlocking did not reload the page`);
    assert.deepEqual(errors, []);
  } finally {
    await context.close();
  }
};
// A vault status read that fails must say so and offer to read it again,
// rather than leaving the unlock button disabled. The status read fails by
// refusing IndexedDB's open until the test allows it.
const checkStatusFailure = async (browser, name) => {
  const context = await browser.newContext();
  try {
    await context.addInitScript(() => {
      window.failVaultOpen = true;
      window.vaultOpens = 0;
      const open = IDBFactory.prototype.open;
      IDBFactory.prototype.open = function (...args) {
        window.vaultOpens++;
        if (window.failVaultOpen)
          throw new DOMException(
            "The test refused to open the vault.",
            "InvalidStateError",
          );
        return open.apply(this, args);
      };
      // Assistive tech announces a live region's content when it changes, so
      // the hint must already be live when the error text is written. Each
      // text write into #vault-hint is logged with the role and aria-live the
      // hint had at that moment. A batch of records is replayed backwards
      // from the current attributes, since a later role change in the same
      // batch is already applied by the time the observer runs.
      window.hintWrites = [];
      new MutationObserver((records) => {
        const live = (el) => ({
          role: el.getAttribute("role"),
          ariaLive: el.getAttribute("aria-live"),
        });
        const states = new Map();
        const writes = [];
        for (const record of records.reverse()) {
          const el =
            record.type === "characterData"
              ? record.target.parentNode
              : record.target;
          if (el?.id !== "vault-hint") continue;
          if (!states.has(el)) states.set(el, live(el));
          const state = states.get(el);
          if (record.type === "attributes")
            state[record.attributeName === "role" ? "role" : "ariaLive"] =
              record.oldValue;
          else if (record.type === "characterData" || record.addedNodes.length)
            writes.unshift({
              text:
                record.type === "characterData"
                  ? record.target.data
                  : [...record.addedNodes].map((n) => n.textContent).join(""),
              ...state,
            });
        }
        window.hintWrites.push(...writes);
      }).observe(document, {
        subtree: true,
        childList: true,
        characterData: true,
        attributes: true,
        attributeOldValue: true,
        attributeFilter: ["role", "aria-live"],
      });
    });
    const page = await context.newPage();
    page.setDefaultTimeout(10000);
    const errors = collectErrors(page);
    await page.goto(`${origin}/`);
    let navigations = 0;
    page.on("framenavigated", (frame) => {
      if (frame === page.mainFrame()) navigations++;
    });
    const hint = page.locator("#vault-hint");
    const retry = page.locator("#vault-retry");
    const shown = async () =>
      retry.waitFor({ timeout: 5000 }).then(
        () => true,
        () => false,
      );
    assert.ok(
      await shown(),
      `${name}: a failed vault status read offers a retry; the hint reads ${JSON.stringify(await hint.textContent())}`,
    );
    assert.equal(
      await hint.textContent(),
      "Could not read the encrypted vault: The test refused to open the vault.",
      `${name}: the error names the failure`,
    );
    assert.equal(await hint.getAttribute("role"), "alert");
    assert.equal(await page.locator("#unlock-button").isDisabled(), true);
    // The early-submit guard still holds while the status is unread.
    await page.locator("#password").fill(passphrase);
    await page.locator("#password").press("Enter");
    await page.locator("#unlock").evaluate((form) => form.requestSubmit());
    await page.waitForTimeout(300);
    assert.equal(
      navigations,
      0,
      `${name}: a submit after the error does not reload`,
    );
    // A retry that fails again shows the error again.
    const opens = await page.evaluate(() => window.vaultOpens);
    await retry.click();
    await page.waitForFunction((n) => window.vaultOpens > n, opens);
    assert.ok(await shown(), `${name}: a second failure offers a retry again`);
    assert.match(
      await hint.textContent(),
      /^Could not read the encrypted vault/,
    );
    // Once the open is allowed, the retry reads the status and unlocks.
    await page.evaluate(() => (window.failVaultOpen = false));
    await retry.click();
    await page
      .locator("#vault-hint", { hasText: "Create a passphrase" })
      .waitFor();
    assert.equal(await retry.count(), 0, `${name}: the retry is removed`);
    assert.equal(await hint.getAttribute("role"), null);
    // Both failures wrote their error into a hint that was already live, so
    // each was announced; the recovered hint is no longer an alert.
    const writes = await page.evaluate(() => window.hintWrites);
    const errorWrites = writes.filter((w) =>
      w.text.startsWith("Could not read the encrypted vault"),
    );
    assert.equal(
      errorWrites.length,
      2,
      `${name}: both failures wrote the error; writes ${JSON.stringify(writes)}`,
    );
    for (const w of errorWrites)
      assert.ok(
        w.role === "alert" || (w.ariaLive && w.ariaLive !== "off"),
        `${name}: the hint is a live region before the error is written; writes ${JSON.stringify(writes)}`,
      );
    assert.equal(
      writes.at(-1).role,
      null,
      `${name}: the recovered hint is written without an alert role`,
    );
    assert.equal(
      await page.locator("#password").inputValue(),
      passphrase,
      `${name}: the retry keeps the typed passphrase`,
    );
    await page.locator("#unlock-button").click();
    await page.locator("#workspace").waitFor();
    assert.equal(navigations, 0, `${name}: recovering did not reload the page`);
    assert.deepEqual(errors, []);
  } finally {
    await context.close();
  }
};
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
      await checkEarlyUnlock(browser, engine.name());
      console.log(
        `${engine.name()}: an unlock submitted before the vault status is read keeps the page and passphrase.`,
      );
      await checkStatusFailure(browser, engine.name());
      console.log(
        `${engine.name()}: a failed vault status read shows the error and a retry that recovers.`,
      );
      const page = await browser.newPage({
        viewport: { width: 1100, height: 800 },
      });
      page.setDefaultTimeout(10000);
      const errors = collectErrors(page);
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
      await unlock(page);
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
          // On a busy host a delayed face can miss the bound, so that case
          // is tried again, up to 3 times, until one opens inside it.
          let usedInBound = false;
          for (let attempt = 1; ; attempt++) {
            const label = `${engine.name()} ${font} ${arrival}${attempt > 1 ? ` try ${attempt}` : ""}`;
            const context = await browser.newContext({
              viewport: { width: 1100, height: 800 },
            });
            try {
              await context.addInitScript(
                ([font, waitMs]) => {
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
                    // The faces still downloading when the dialog shows, and
                    // each face's status then.
                    window.loadingAtOpen ??= [...document.fonts]
                      .filter((f) => f.status === "loading")
                      .map((f) => f.family.replace(/"/g, ""));
                    window.facesAtOpen ??= [...document.fonts].map((f) => [
                      f.family.replace(/"/g, ""),
                      f.status,
                    ]);
                    // When the bound's timer fired, or null if not yet.
                    if (!("boundAtOpen" in window))
                      window.boundAtOpen = window.boundAt;
                    return showModal.call(this);
                  };
                  document.addEventListener(
                    "click",
                    (e) => {
                      if (!e.target.closest?.("#appearance")) return;
                      window.clickedAt ??= performance.now();
                      // A timer for the bound, started before the app's own in
                      // the same click, so a busy page delays both alike.
                      if (window.boundAt === undefined) {
                        window.boundAt = null;
                        setTimeout(
                          () => (window.boundAt = performance.now()),
                          waitMs,
                        );
                      }
                    },
                    true,
                  );
                  document.addEventListener("click", (e) => {
                    if (e.target.closest?.("#appearance"))
                      window.openInClick =
                        document.querySelector("#dialog").open;
                  });
                },
                [font, FONT_WAIT_MS],
              );
              const page = await context.newPage();
              page.setDefaultTimeout(10000);
              const caseErrors = collectErrors(page);
              let releaseFaces;
              const facesHeld = new Promise((r) => (releaseFaces = r));
              await page.route(/-nerd-[^/]*\.woff2$/, async (route) => {
                await facesHeld;
                await route.continue();
              });
              await page.goto(`${origin}/`);
              await unlock(page);
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
              await page.locator("#appearance").click();
              // Released only once the click has been handled, so on a busy host
              // the face cannot arrive before the click that has to wait for it.
              if (arrival === "delayed") setTimeout(releaseFaces, delayMs);
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
                break;
              }
              await page.locator("#dialog .theme-grid").waitFor();
              const opened = await page.evaluate(() => ({
                wait: Math.round(window.showModals[0] - window.clickedAt),
                count: window.showModals.length,
                inClick: window.openInClick,
                fonts: document.fonts.status,
                loading: window.loadingAtOpen,
                faces: window.facesAtOpen,
                pastBound:
                  window.boundAtOpen === null
                    ? null
                    : Math.round(window.showModals[0] - window.boundAtOpen),
              }));
              // Every face of the chosen family had loaded when the dialog
              // showed, which is what the app checks before using it.
              const chosen = opened.faces.filter(
                ([name]) => name === family.replace(/"/g, ""),
              );
              const arrived =
                chosen.length > 0 &&
                chosen.every(([, status]) => status === "loaded");
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
              assert.ok(
                early.textWidth > 0,
                `${label}: dialog text is visible`,
              );
              assert.ok(
                early.textFont === "monospace" ||
                  !opened.loading.includes(family.replace(/"/g, "")),
                `${label}: the dialog text face was not downloading when it showed`,
              );
              assert.equal(opened.inClick, false, `${label}: the open waited`);
              // Measured from a timer started in the same click rather than from
              // the click, so a busy host that runs the app's timer late does
              // not fail it.
              assert.ok(
                opened.pastBound === null || opened.pastBound < 150,
                `${label}: the open waited at most the ${FONT_WAIT_MS} ms bound, opened ${opened.pastBound} ms after it (${opened.wait} ms after the click)`,
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
              } else if (arrived) {
                assert.equal(
                  early.uiFont.split(",")[0].replace(/"/g, ""),
                  family.replace(/"/g, ""),
                  `${label}: a face arriving within the bound is used`,
                );
                usedInBound = true;
              } else {
                // On a busy host the delayed face can miss the bound. The app
                // then opens in the fallback, but only once the bound has passed.
                assert.notEqual(
                  opened.pastBound,
                  null,
                  `${label}: the open fell back before the bound, ${opened.wait} ms after the click; faces ${JSON.stringify(chosen)}`,
                );
                assert.equal(
                  early.uiFont,
                  "monospace",
                  `${label}: a face missing the bound leaves the dialog in the fallback`,
                );
              }
              waits.push(
                `${font} ${arrival}${attempt > 1 ? ` try ${attempt}` : ""} ${opened.wait} ms${arrival === "held" || arrived ? "" : " (face missed the bound)"}`,
              );
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
            if (arrival !== "delayed" || usedInBound) break;
            assert.ok(
              attempt < 3,
              `${engine.name()} ${font} delayed: the face missed the ${FONT_WAIT_MS} ms bound in all 3 tries, so its use was never checked`,
            );
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
