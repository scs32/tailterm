import { chromium } from "@playwright/test";
import assert from "node:assert/strict";
import { isIP } from "node:net";
const origin = process.argv[2] || "https://tailterm.tailarr.com";
const address = process.argv[3];
if (address && !isIP(address))
  throw new Error("Expected a verified public DNS address");
const browser = await chromium.launch({
  args: address
    ? [`--host-resolver-rules=MAP ${new URL(origin).hostname} ${address}`]
    : [],
});
try {
  const page = await browser.newPage({
    viewport: { width: 1440, height: 900 },
    permissions: ["clipboard-read", "clipboard-write"],
  });
  const errors = [];
  page.on("pageerror", (e) => errors.push(e.message));
  const response = await page.goto(origin);
  assert.equal(response.status(), 200);
  assert.ok(
    (await response.allHeaders())["content-security-policy"].includes(
      "wasm-unsafe-eval",
    ),
  );
  await page
    .locator("#password")
    .fill("temporary deployment verification vault");
  await page.locator("#unlock-button").click();
  await page.locator("#workspace").waitFor();
  await page.locator("#keys").click();
  await page
    .locator("#generate-key-form [name=name]")
    .fill("Deployment test key");
  await page.locator("#generate-key").click();
  await page.locator("[data-copy-public-key]").waitFor({ timeout: 90000 });
  await page.locator("[data-copy-public-key]").click();
  await page.waitForFunction(() =>
    document
      .querySelector("#public-key-text")
      ?.value.startsWith("ssh-ed25519 "),
  );
  const publicKey = await page.locator("#public-key-text").inputValue();
  await page.waitForFunction(
    () =>
      document.querySelector("[data-copy-public-key]")?.textContent ===
      "Copied",
  );
  assert.equal(
    await page.evaluate(() => navigator.clipboard.readText()),
    publicKey,
  );
  await page.locator("#dialog-close").click();
  await page.locator("#lock").click();
  await page
    .locator("#password")
    .fill("temporary deployment verification vault");
  await page.locator("#unlock-button").click();
  await page.locator("#keys").click();
  await page.locator("[data-copy-public-key]").click();
  await page.waitForFunction(
    (expected) =>
      document.querySelector("#public-key-text")?.value === expected,
    publicKey,
  );
  await page.locator("#dialog-close").click();
  for (const [width, height] of [
    [1440, 900],
    [1024, 768],
    [1920, 1080],
  ]) {
    await page.setViewportSize({ width, height });
    await page.waitForFunction(() => {
      const box = document
        .querySelector(".terminal-shell")
        .getBoundingClientRect();
      const main = document.querySelector("main").getBoundingClientRect();
      return (
        Math.abs(box.left - main.left - (innerHeight - box.bottom)) < 1 &&
        Math.abs(innerWidth - box.right - (innerHeight - box.bottom)) < 1
      );
    });
  }
  await page.setViewportSize({ width: 1440, height: 900 });
  await page.screenshot({ path: "deployed-layout-preview.png" });
  await page.locator("#tailscale-login").click();
  await page.waitForFunction(() => typeof globalThis.newIPN === "function", {
    timeout: 90000,
  });
  await page.waitForFunction(
    () =>
      /NeedsLogin|Starting|Running|Tailnet connected/.test(
        document.querySelector("#tail-status").textContent,
      ),
    { timeout: 90000 },
  );
  assert.deepEqual(errors, []);
  console.log(
    JSON.stringify({
      origin,
      https: true,
      matchingMargins: true,
      productionWasmStarted: true,
      generatedKeyRestored: true,
      state: await page.locator("#tail-status").textContent(),
    }),
  );
} finally {
  await browser.close();
}
