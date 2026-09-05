import assert from "node:assert/strict";
export async function exerciseImageUpload(page, files, getInput, control) {
  const bytes = Array.from({ length: 100000 }, (_, i) => i % 256);
  const drop = async () => {
    const data = await page.evaluateHandle((bytes) => {
      const data = new DataTransfer();
      data.items.add(
        new File([new Uint8Array(bytes)], "drop-test.png", {
          type: "image/png",
        }),
      );
      return data;
    }, bytes);
    await page
      .locator(".terminal-instance:not([hidden])")
      .first()
      .dispatchEvent("drop", { dataTransfer: data });
    await data.dispose();
    await page.waitForFunction(
      () =>
        document.querySelector("#upload-directory")?.value ===
        "/tmp/fixture uploads",
    );
  };
  await drop();
  await page.locator("#upload-start").click();
  await page.waitForFunction(
    () => !document.querySelector("#upload-start").disabled,
  );
  assert.match(await page.locator("#upload-note").innerText(), /Uploaded\./);
  await page.getByRole("textbox", { name: "Uploaded image paths" }).waitFor();
  assert.deepEqual(
    files.get("/tmp/fixture uploads/drop-test.png").data,
    Buffer.from(bytes),
  );
  assert.equal(
    files.get("/tmp/fixture uploads/drop-test.png").mode & 0o777,
    0o600,
  );
  assert.equal(
    [...files.keys()].some((path) => path.includes(".tailterm-upload-")),
    false,
  );
  await page.waitForTimeout(100);
  assert.ok(getInput().includes("'/tmp/fixture uploads/drop-test.png'"));
  await page.screenshot({ path: "/tmp/tailterm-upload-preview.png" });
  await page.locator("#upload-cancel").click();
  // Clipboard images use the review flow, and hiding it does not stop transfer.
  control.delay = 100;
  await page
    .locator(".terminal-instance:not([hidden])")
    .first()
    .evaluate((el) => {
      const data = new DataTransfer();
      data.items.add(
        new File([new Uint8Array(200000)], "screenshot.png", {
          type: "image/png",
        }),
      );
      el.dispatchEvent(
        new ClipboardEvent("paste", {
          clipboardData: data,
          bubbles: true,
          cancelable: true,
        }),
      );
    });
  await page.waitForFunction(
    () =>
      document.querySelector("#upload-directory")?.value ===
      "/tmp/fixture uploads",
  );
  await page.locator("#upload-start").click();
  await page.locator("#upload-hide").click();
  await page.waitForFunction(
    () =>
      document.querySelector("#upload-tray").textContent ===
      "Uploads: complete",
  );
  await page.locator("#upload-tray").click();
  assert.match(
    await page.locator("#upload-note").innerText(),
    /Copy the paths/,
  );
  assert.ok([...files.keys()].some((path) => path.includes("clipboard-")));
  control.delay = 0;
  await page.locator("#upload-cancel").click();
  await drop();
  await page.locator("#upload-start").click();
  await page.waitForFunction(() =>
    document
      .querySelector("#upload-note")
      .textContent.includes("already exists"),
  );
  assert.deepEqual(
    files.get("/tmp/fixture uploads/drop-test.png").data,
    Buffer.from(bytes),
  );
  await page.locator("#upload-cancel").click();
  control.delay = 500;
  const beforeCancel = getInput();
  await page
    .locator(".terminal-instance:not([hidden])")
    .first()
    .evaluate((el) => {
      const data = new DataTransfer();
      data.items.add(
        new File([new Uint8Array(200000)], "cancelled.png", {
          type: "image/png",
        }),
      );
      el.dispatchEvent(
        new DragEvent("drop", {
          dataTransfer: data,
          bubbles: true,
          cancelable: true,
        }),
      );
    });
  await page.waitForFunction(
    () =>
      document.querySelector("#upload-directory")?.value ===
      "/tmp/fixture uploads",
  );
  await page.locator("#upload-start").click();
  await page.locator("#upload-hide").click();
  await page.locator("#upload-tray").click();
  await page.locator("#upload-cancel").click();
  await page.waitForFunction(
    () =>
      document.querySelector("#upload-tray").textContent ===
      "Uploads: cancelled",
  );
  assert.ok(!files.has("/tmp/fixture uploads/cancelled.png"));
  assert.equal(getInput(), beforeCancel);
  await page.locator("#upload-cancel").click();
  control.delay = 0;
}
