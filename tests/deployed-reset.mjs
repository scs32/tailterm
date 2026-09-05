import { chromium } from "@playwright/test";
import { exerciseVaultReset } from "./vault-reset-browser.mjs";

const origin = process.argv[2] || "https://tailterm.pages.dev";
const browser = await chromium.launch();
try {
  // An isolated, disposable browser context; never uses the user's vault.
  const context = await browser.newContext();
  const page = await context.newPage();
  await page.goto(origin);
  await page.locator("#password").fill("disposable reset verification passphrase");
  await page.locator("#unlock-button").click();
  await page.locator("#workspace").waitFor();
  await exerciseVaultReset(page, context, origin);
  await page.locator("#lock").click();
  await page.locator("#reset-vault").click();
  await page.screenshot({ path: "/tmp/tailterm-vault-reset.png" });
  console.log("Live vault reset passed: cancellation, active-tab protection, deletion, and a fresh passphrase.");
} finally {
  await browser.close();
}
