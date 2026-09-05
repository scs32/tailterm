import { createServer } from "vite";
import { chromium } from "@playwright/test";
import assert from "node:assert/strict";
const server = await createServer({
  configFile: false,
  server: { host: "127.0.0.1", port: 0 },
});
await server.listen();
const browser = await chromium.launch();
try {
  const page = await browser.newPage();
  await page.route("**/popup-test", (r) =>
    r.fulfill({
      contentType: "text/html",
      body: '<!doctype html><html><head><link rel="stylesheet" href="/client/style.css"></head><body></body></html>',
    }),
  );
  await page.goto(
    `http://127.0.0.1:${server.httpServer.address().port}/popup-test`,
  );
  await page.evaluate(async () => {
    window.appearance = await import("/client/appearance.js");
    window.prompts = await import("/client/login-prompts.js");
    window.confirmation = await import("/client/confirm-dialog.js");
  });
  for (const theme of ["tailserve", "dawn"]) {
    await page.evaluate(
      (theme) =>
        window.appearance.applyChrome(
          window.appearance.normalizeAppearance({ theme }),
        ),
      theme,
    );
    for (const width of [1000, 390]) {
      await page.setViewportSize({ width, height: 700 });
      for (const kind of ["host-key", "credentials", "keyboard", "confirm"]) {
        await page.evaluate((kind) => {
          if (kind === "confirm") {
            window.pending = window.confirmation.confirmDialog({
              title: "Delete key?",
              message: "Remove this key from the encrypted vault?",
              action: "Delete key",
              destructive: true,
            });
            return;
          }
          window.abort = new AbortController();
          window.pending = window.prompts.loginPrompt(
            {
              kind,
              host: "example.tailnet.ts.net",
              port: 22,
              username: "test",
              fingerprint: "SHA256:abcdefghijklmnopqrstuvwxyz0123456789ABCDEFG",
              methods: ["publickey", "password"],
              keys: [],
              name: "Verification code",
              instructions: "Enter the code from your authenticator.",
              prompts: [{ prompt: "Code", echo: true }],
            },
            window.abort.signal,
          );
        }, kind);
        await page.locator("dialog[open]").waitFor();
        await page.waitForTimeout(160);
        if (kind === "credentials")
          assert.equal(
            await page.evaluate(() => document.activeElement.name),
            "password",
          );
        if (kind === "keyboard")
          assert.equal(
            await page.evaluate(() => document.activeElement.name),
            "answer-0",
          );
        const bounds = await page.locator("dialog[open]").evaluate((el) => {
          const r = el.getBoundingClientRect();
          return {
            x: r.x,
            y: r.y,
            right: r.right,
            bottom: r.bottom,
            scroll: el.scrollWidth,
            client: el.clientWidth,
            label: el.getAttribute("aria-labelledby"),
          };
        });
        assert.ok(
          bounds.x >= 0 &&
            bounds.y >= 0 &&
            bounds.right <= width + 1 &&
            bounds.bottom <= 701,
          JSON.stringify(bounds),
        );
        assert.ok(bounds.scroll <= bounds.client + 1, JSON.stringify(bounds));
        assert.ok(bounds.label);
        await page.screenshot({
          path: `.build/popup-${kind}-${theme}-${width}.png`,
        });
        await page.keyboard.press("Escape");
        const result = await page.evaluate(() => window.pending);
        if (kind === "confirm") assert.equal(result, false);
        else assert.equal(result.cancel, true);
      }
    }
  }
  console.log(
    "PASS: host verification, password/key login, verification codes and confirmations fit desktop/mobile in dark/light palettes and cancel correctly.",
  );
} finally {
  await browser.close();
  await server.close();
}
