import { createServer } from "vite";
import { chromium, webkit } from "@playwright/test";
import assert from "node:assert/strict";
const server = await createServer({
  configFile: false,
  server: { host: "127.0.0.1", port: 0 },
});
await server.listen();
try {
  for (const engine of [chromium, webkit]) {
    const browser = await engine.launch();
    try {
      const page = await browser.newPage({
        viewport: { width: 1000, height: 760 },
      });
      await page.route("**/appearance-test", (route) =>
        route.fulfill({
          contentType: "text/html",
          body: '<!doctype html><html><head><meta charset="utf-8"></head><body><dialog open><div class="dialog-head"><h2>Appearance / terminal</h2><button>×</button></div><p>Connected to workspace</p><p class="fine">Muted status · readable in every palette</p><input placeholder="Session name"><button class="primary">Connect</button><button class="danger">Delete</button><p id="rename-session-error" role="alert">Session name is required</p><p id="vault-reset-error" role="alert">Check the reset confirmation</p><div id="appearance-preview"></div></dialog></body></html>',
        }),
      );
      await page.goto(
        `http://127.0.0.1:${server.httpServer.address().port}/appearance-test`,
      );
      await page.evaluate(async () => {
        await import("/node_modules/@xterm/xterm/css/xterm.css");
        await import("/client/style.css");
        await import("/client/fonts.css");
        await import("/node_modules/@fontsource/source-code-pro/latin-400.css");
        await import("/node_modules/@fontsource/source-code-pro/latin-700.css");
        window.appearance = await import("/client/appearance.js");
        const { createAppearancePreview } =
          await import("/client/appearance-preview.js");
        window.preview = createAppearancePreview(
          document.querySelector("#appearance-preview"),
          appearance.normalizeAppearance({ font: "cascadia" }),
        );
      });
      for (const theme of [
        "ember",
        "lagoon",
        "voltage",
        "tailserve",
        "catppuccin",
        "tokyo",
        "rose",
        "nord",
        "dawn",
      ]) {
        await page.evaluate((theme) => {
          const p = appearance.normalizeAppearance({
            theme,
            font: "cascadia",
            fontSize: 14,
          });
          appearance.applyChrome(p);
          preview.update(p);
        }, theme);
        await page.evaluate(() => document.fonts.ready);
        await page.waitForTimeout(180);
        const failures = await page.evaluate(() => {
          const canvas = document.createElement("canvas");
          canvas.width = canvas.height = 1;
          const ctx = canvas.getContext("2d", { willReadFrequently: true });
          const rgb = (css) => {
            ctx.clearRect(0, 0, 1, 1);
            ctx.fillStyle = css;
            ctx.fillRect(0, 0, 1, 1);
            return [...ctx.getImageData(0, 0, 1, 1).data];
          };
          const blend = (fg, bg) =>
            fg
              .slice(0, 3)
              .map((v, i) => (v * fg[3]) / 255 + bg[i] * (1 - fg[3] / 255));
          const background = (el) => {
            if (!el) return [255, 255, 255];
            const c = rgb(getComputedStyle(el).backgroundColor);
            return c[3] === 255
              ? c.slice(0, 3)
              : blend(c, background(el.parentElement));
          };
          const luminance = (c) =>
            c
              .map((v) => {
                v /= 255;
                return v <= 0.04045 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4;
              })
              .reduce((a, v, i) => a + v * [0.2126, 0.7152, 0.0722][i], 0);
          const errors = [];
          for (const el of document.querySelectorAll(
            ".xterm-rows span, dialog p, dialog h2, dialog button",
          )) {
            if (!/[a-zA-Z0-9]/.test(el.textContent)) continue;
            const bg = background(el),
              fg = blend(rgb(getComputedStyle(el).color), bg);
            const a = luminance(bg),
              b = luminance(fg),
              ratio = (Math.max(a, b) + 0.05) / (Math.min(a, b) + 0.05);
            const min = el.closest(".xterm-rows") ? 3.45 : 4.5;
            if (ratio < min)
              errors.push({ text: el.textContent, ratio, fg, bg });
          }
          return errors;
        });
        assert.deepEqual(failures, [], `${engine.name()} ${theme} contrast`);
        await page.screenshot({
          path: `.build/theme-${engine.name()}-${theme}.png`,
        });
      }
      for (const font of ["cascadia", "fira", "source"]) {
        assert.ok(
          await page.evaluate(async (id) => {
            const family = appearance.fonts[id].family.split(",")[0];
            const loaded = await document.fonts.load(
              `14px ${family}`,
              "0O \uf07b \ue0a0",
            );
            const bold = await document.fonts.load(
              `700 14px ${family}`,
              "bold",
            );
            return loaded.length > 0 && bold.length > 0;
          }, font),
          `${font} regular and bold must load`,
        );
      }
      await page.setViewportSize({ width: 390, height: 650 });
      await page.waitForTimeout(180);
      assert.ok(
        await page
          .locator("dialog")
          .evaluate((el) => el.scrollWidth <= el.clientWidth + 1),
      );
      // The sticky header must cover the padding above and beside scrolled content.
      await page.evaluate(() => {
        const content = document.createElement("div");
        content.style.height = "1800px";
        content.textContent = "Scrolling content";
        document.querySelector("dialog").append(content);
      });
      for (const width of [1000, 390]) {
        await page.setViewportSize({ width, height: 650 });
        for (const scrollTop of [0, 80, 400, 1200]) {
          await page
            .locator("dialog")
            .evaluate((el, y) => (el.scrollTop = y), scrollTop);
          const covered = await page.evaluate(() => {
            const dialog = document
              .querySelector("dialog")
              .getBoundingClientRect();
            const head = document
              .querySelector(".dialog-head")
              .getBoundingClientRect();
            return (
              Math.abs(head.top - dialog.top - 1) < 1 &&
              Math.abs(head.left - dialog.left - 1) < 1 &&
              Math.abs(head.right - dialog.right + 1) < 1 &&
              !!document
                .elementFromPoint(dialog.left + 5, dialog.top + 5)
                ?.closest(".dialog-head")
            );
          });
          assert.ok(
            covered,
            `${engine.name()} sticky header covers top/side padding at ${width}px, scroll ${scrollTop}`,
          );
        }
      }
      await page.screenshot({
        path: `.build/appearance-scrolled-${engine.name()}.png`,
      });
      console.log(
        `${engine.name()}: all themes meet contrast checks; Nerd Fonts load; square popup fits mobile.`,
      );
    } finally {
      await browser.close();
    }
  }
} finally {
  await server.close();
}
