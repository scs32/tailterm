export async function finishRestoration(page) {
  const deadline = Date.now() + 90000;
  while (Date.now() < deadline) {
    if (await page.locator('#workspace[data-restoring="false"]').count())
      return;
    const method = page.locator(".login-prompt [name=method]");
    if (await method.isVisible()) await method.selectOption("password");
    const password = page.locator(".login-prompt [name=password]");
    if (await password.isVisible()) {
      await password.fill("static-ssh-password");
      await page.locator(".login-prompt button[type=submit]").click();
    }
    const trust = page.getByRole("button", { name: "Trust & continue" });
    if (await trust.isVisible()) await trust.click();
    await page.waitForTimeout(100);
  }
  throw Error(
    "Workspace restoration did not finish: " +
      (await page.locator("body").innerText()),
  );
}
