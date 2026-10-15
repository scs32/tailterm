import assert from "node:assert/strict";
import ssh2 from "ssh2";

// Measured for wi_783a8e95c7ae4296 (11 runs beside a release matrix): fill to
// row took at most 62 ms, the server's share 3 ms. 10 s is over 150x that, so
// a timeout is a stall, not slow key generation; the phase line says where.
const GENERATED_ROW_TIMEOUT_MS = 10000;

export async function exerciseGeneratedKey(page) {
  // Static mode generates in the page, so no request fires there.
  const phase = { requests: 0, status: null };
  const onRequest = (r) =>
    r.url().endsWith("/api/keys/generate") && phase.requests++;
  const onResponse = (r) =>
    r.url().endsWith("/api/keys/generate") && (phase.status = r.status());
  page.on("request", onRequest);
  page.on("response", onResponse);
  await page.evaluate(() => {
    globalThis.__generateKeySubmits = 0;
    document.addEventListener(
      "submit",
      (e) =>
        e.target.id === "generate-key-form" &&
        globalThis.__generateKeySubmits++,
      true,
    );
  });
  await page
    .locator("#generate-key-form [name=name]")
    .fill("Browser-generated key");
  const clicked = Date.now();
  await page.locator("#generate-key").click();
  const row = page
    .locator(".key-list > div")
    .filter({ hasText: "Browser-generated key" });
  try {
    await row.waitFor({ timeout: GENERATED_ROW_TIMEOUT_MS });
    console.log(
      `Generated key row appeared ${Date.now() - clicked} ms after the click.`,
    );
  } catch (error) {
    const submits = await page
      .evaluate(() => globalThis.__generateKeySubmits)
      .catch(() => "unknown");
    const rows = await row.count().catch(() => "unknown");
    console.log(
      `Generated key row wait failed: submits=${submits} requests=${phase.requests} responseStatus=${phase.status} rowsPresent=${rows} after ${Date.now() - clicked} ms.`,
    );
    throw error;
  } finally {
    page.off("request", onRequest);
    page.off("response", onResponse);
  }
  await row.locator("[data-copy-public-key]").click();
  await page.waitForFunction(() =>
    document
      .querySelector("#public-key-text")
      ?.value.startsWith("ssh-ed25519 "),
  );
  const publicKey = await page.locator("#public-key-text").inputValue();
  const parsed = ssh2.utils.parseKey(publicKey);
  assert.ok(!(parsed instanceof Error));
  assert.equal(parsed.type, "ssh-ed25519");
  assert.equal(
    await page.evaluate(() => navigator.clipboard.readText()),
    publicKey,
  );
  assert.doesNotMatch(
    await page.locator("#dialog").innerText(),
    /BEGIN OPENSSH PRIVATE KEY/,
  );
  await page.evaluate(() => {
    globalThis.__originalClipboardWrite = navigator.clipboard.writeText;
    navigator.clipboard.writeText = async () => {
      throw new DOMException("Denied", "NotAllowedError");
    };
  });
  await row.locator("[data-copy-public-key]").click();
  await page.getByText(/Select and copy the public key above/).waitFor();
  assert.equal(await page.locator("#public-key-text").inputValue(), publicKey);
  assert.equal(
    await page
      .locator("#public-key-text")
      .evaluate((el) => el.selectionEnd - el.selectionStart),
    publicKey.length,
  );
  await page.evaluate(() => {
    navigator.clipboard.writeText = __originalClipboardWrite;
  });
  const size = page.viewportSize();
  await page.setViewportSize({ width: 390, height: 844 });
  assert.ok(
    await page
      .locator("#dialog")
      .evaluate((el) => el.scrollWidth <= el.clientWidth),
  );
  await page.screenshot({ path: `.build/ssh-key-${process.argv[1].split('/').at(-1).replace(/\.mjs$/, '')}-${process.env.TEST_BROWSER || 'chromium'}-mobile.png` });
  await page.setViewportSize(size);
  await page.screenshot({ path: `.build/ssh-key-${process.argv[1].split('/').at(-1).replace(/\.mjs$/, '')}-${process.env.TEST_BROWSER || 'chromium'}-full.png` });
  console.log(
    "Key generation, public-key copy, denied clipboard fallback and mobile layout passed.",
  );
  return parsed;
}
