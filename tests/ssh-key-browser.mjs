import assert from "node:assert/strict";
import ssh2 from "ssh2";

export async function exerciseGeneratedKey(page) {
  await page
    .locator("#generate-key-form [name=name]")
    .fill("Browser-generated key");
  await page.locator("#generate-key").click();
  const row = page
    .locator(".key-list > div")
    .filter({ hasText: "Browser-generated key" });
  await row.waitFor();
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
  await page.screenshot({ path: "/tmp/tailterm-ssh-keys-mobile.png" });
  await page.setViewportSize(size);
  await page.screenshot({ path: "/tmp/tailterm-ssh-keys.png" });
  console.log(
    "Key generation, public-key copy, denied clipboard fallback and mobile layout passed.",
  );
  return parsed;
}
