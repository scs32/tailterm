import assert from "node:assert/strict";

const record = (page) =>
  page.evaluate(
    () =>
      new Promise((resolve, reject) => {
        const request = indexedDB.open("tailserve", 1);
        request.onerror = () => reject(request.error);
        request.onsuccess = () => {
          const database = request.result;
          const tx = database.transaction("vault");
          const read = tx.objectStore("vault").get("encrypted");
          tx.oncomplete = () => {
            database.close();
            resolve(read.result);
          };
          tx.onerror = () => {
            database.close();
            reject(tx.error);
          };
        };
      }),
  );

export async function exerciseVaultReset(page, context, origin) {
  await page.setViewportSize({ width: 1440, height: 1100 });
  await page.waitForTimeout(700);
  const before = await record(page);
  const locked = await context.newPage();
  await locked.goto(origin);
  await locked.locator("#reset-vault").click();
  await locked.locator("#vault-reset-confirmation").fill("RESET");
  await locked.locator("#vault-reset-submit").click();
  await locked
    .getByText("This workspace is open in another browser tab.", {
      exact: false,
    })
    .waitFor();
  assert.deepEqual(
    await record(page),
    before,
    "An active vault must not be reset by another tab",
  );
  await locked.close();
  await page.locator("#lock").click();
  await page.locator("#reset-vault").waitFor();
  await page.locator("#password").fill("a forgotten incorrect passphrase");
  await page.locator("#unlock-button").click();
  await page.waitForFunction(
    () => !document.querySelector("#unlock-button").disabled,
  );
  assert.equal(await page.locator("#workspace").count(), 0);
  await page.locator("#reset-vault").click();
  await page.locator("#vault-reset-confirmation").fill("reset");
  assert.equal(await page.locator("#vault-reset-submit").isDisabled(), true);
  await page.locator("#vault-reset-cancel").click();
  assert.deepEqual(
    await record(page),
    before,
    "Cancel must preserve the encrypted vault",
  );
  await page.locator("#reset-vault").click();
  await page.locator("#vault-reset-confirmation").fill("RESET");
  await page.locator("#vault-reset-submit").click();
  await page.getByRole("button", { name: "Create encrypted vault" }).waitFor();
  assert.equal(
    await record(page),
    undefined,
    "Reset must remove the entire encrypted record, including node identity",
  );
  await page.locator("#password").fill("a completely new vault passphrase");
  await page.locator("#unlock-button").click();
  await page.locator("#workspace").waitFor();
  assert.equal(await page.locator("[data-launch-server]").count(), 0);
  assert.equal(await page.locator("#key-count").textContent(), "0");
  await page.locator("#lock").click();
  await page.locator("#password").fill("a completely new vault passphrase");
  await page.locator("#unlock-button").click();
  await page.locator("#workspace").waitFor();
}
