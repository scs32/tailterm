// QR sign-in acceptance checks.  This uses a Vite static-mode server, but routes
// only the WASM runtime module to an in-page IPN fixture: no Tailnet login,
// profile, hub, or persistent browser context is used.
import assert from "node:assert/strict";
import jsQR from "jsqr";
import { chromium, webkit } from "@playwright/test";
import { createServer as createViteServer } from "vite";

const runtimeFixture = `
export async function createIPN() {
  const ipn = {
    callbacks: null,
    logins: 0,
    run(callbacks) {
      this.callbacks = callbacks;
      queueMicrotask(() => callbacks.notifyState("NeedsLogin"));
    },
    login() { this.logins++; },
  };
  globalThis.__tailscaleLoginFixture = ipn;
  return ipn;
}
export async function generatePrivateKey() { return "synthetic-private-key"; }
export async function validatePrivateKey() { return { publicKey: "synthetic-public-key" }; }
`;

const controllerFixture = `<!doctype html><body><dialog id="dialog"></dialog>
<script type="module">
  import { createTailscaleLoginController } from "/client/tailscale-login.js";
  const dialog = document.querySelector("#dialog");
  const pending = new Map();
  const controller = createTailscaleLoginController({
    renderQRCode: (href) => new Promise((resolve, reject) => pending.set(href, { resolve, reject })),
  });
  function show(href, state = "NeedsLogin") {
    if (!dialog.open) dialog.showModal();
    dialog.innerHTML = '<div id="host"></div><a id="link" href="' + href + '">Continue to Tailscale</a><p id="status"></p>';
    return controller.show({ dialog, host: document.querySelector("#host"), status: document.querySelector("#status"), href, state });
  }
  dialog.addEventListener("close", () => controller.clear());
  window.qaTailscaleLogin = {
    show,
    resolve: (href) => pending.get(href).resolve(document.createElement("canvas")),
    reject: (href) => pending.get(href).reject(Error("synthetic encoder failure")),
    state: (value) => controller.updateState(value),
    open: () => dialog.open,
    current: () => controller.isCurrentDialog(dialog),
  };
</script>`;

const vite = await createViteServer({
  configFile: "vite.config.js",
  mode: "static",
  server: { host: "127.0.0.1", port: 0 },
  logLevel: "error",
});
await vite.listen();
const address = vite.httpServer.address();
const origin = `http://127.0.0.1:${address.port}`;

const synthetic = (name) => `https://login.tailscale.com/a/${name}?fixture=1`;

async function decodedQR(page) {
  const image = await page
    .locator("#tailscale-login-qr canvas")
    .evaluate((canvas) => {
      const context = canvas.getContext("2d", { willReadFrequently: true });
      const image = context.getImageData(0, 0, canvas.width, canvas.height);
      return {
        width: image.width,
        height: image.height,
        data: [...image.data],
      };
    });
  return jsQR(new Uint8ClampedArray(image.data), image.width, image.height)
    ?.data;
}

async function runControllerChecks(browser, name) {
  const page = await browser.newPage();
  await page.route("**/qa-tailscale-login-controller.html", (route) =>
    route.fulfill({ contentType: "text/html", body: controllerFixture }),
  );
  await page.goto(origin + "/qa-tailscale-login-controller.html");

  const first = synthetic("late-first"),
    second = synthetic("latest");
  await page.evaluate(
    ([a, b]) => {
      void qaTailscaleLogin.show(a);
      void qaTailscaleLogin.show(b);
    },
    [first, second],
  );
  await page.evaluate((href) => qaTailscaleLogin.resolve(href), second);
  await page.locator("#host canvas").waitFor();
  await page.evaluate((href) => qaTailscaleLogin.resolve(href), first);
  assert.equal(
    await page.locator("#link").getAttribute("href"),
    second,
    `${name}: latest synthetic auth URL remains the fallback`,
  );
  assert.equal(
    await page.locator("#host canvas").count(),
    1,
    `${name}: late QR completion cannot replace the current dialog`,
  );

  const failed = synthetic("encoder-failure");
  await page.evaluate((href) => void qaTailscaleLogin.show(href), failed);
  await page.evaluate((href) => qaTailscaleLogin.reject(href), failed);
  await page
    .getByText("QR unavailable. Continue with the sign-in link below.")
    .waitFor();
  assert.equal(await page.locator("#host canvas").count(), 0);
  assert.equal(await page.locator("#link").getAttribute("href"), failed);

  const approval = synthetic("approval");
  await page.evaluate(
    (href) => void qaTailscaleLogin.show(href, "NeedsMachineAuth"),
    approval,
  );
  await page
    .getByText("Sign-in complete. Waiting for device approval…")
    .waitFor();
  await page.evaluate((href) => qaTailscaleLogin.resolve(href), approval);
  await page.locator("#host canvas").waitFor();

  const escaped = synthetic("escape");
  await page.evaluate((href) => void qaTailscaleLogin.show(href), escaped);
  await page.keyboard.press("Escape");
  await page.waitForFunction(() => !qaTailscaleLogin.open());
  await page.evaluate((href) => qaTailscaleLogin.resolve(href), escaped);
  assert.equal(await page.locator("#host canvas").count(), 0);
  assert.equal(await page.evaluate(() => qaTailscaleLogin.current()), false);
  await page.close();
}

async function runAppChecks(browser, name) {
  const context = await browser.newContext({
    viewport: { width: 1100, height: 820 },
  });
  await context.route("**/client/wasm-runtime.js*", (route) =>
    route.fulfill({ contentType: "text/javascript", body: runtimeFixture }),
  );
  const page = await context.newPage();
  const errors = [];
  page.on("pageerror", (error) => errors.push(error.message));
  await page.goto(origin);
  await page.locator("#password").fill("synthetic QR fixture passphrase");
  await page.locator("#unlock-button").click();
  await page.locator("#workspace").waitFor();
  await page.locator("#appearance").click();
  await page.locator('[data-theme-choice="dawn"]').click();
  await page.locator("#dialog-close").click();
  assert.equal(await page.locator("html").getAttribute("data-theme"), "dawn");
  await page.locator("#tailscale-login").click();
  await page.waitForFunction(
    () => !!globalThis.__tailscaleLoginFixture?.callbacks,
  );
  assert.equal(await page.evaluate(() => __tailscaleLoginFixture.logins), 1);

  const first = synthetic("first"),
    latest = synthetic("latest-app");
  await page.evaluate(
    (href) => __tailscaleLoginFixture.callbacks.notifyBrowseToURL(href),
    first,
  );
  await page.locator("#tailscale-login-link").waitFor();
  assert.equal(
    await page.locator("#tailscale-login-link").getAttribute("href"),
    first,
  );
  assert.equal(
    await decodedQR(page),
    first,
    `${name}: QR decodes to the exact auth URL`,
  );
  await page.evaluate(
    (href) => __tailscaleLoginFixture.callbacks.notifyBrowseToURL(href),
    latest,
  );
  await page.waitForFunction(
    (href) => document.querySelector("#tailscale-login-link")?.href === href,
    latest,
  );
  assert.equal(
    await decodedQR(page),
    latest,
    `${name}: replacement URL replaces stale QR`,
  );
  await page.keyboard.press("Escape");
  await page.locator("#dialog").waitFor({ state: "hidden" });
  const reopened = synthetic("reopened-after-escape");
  await page.evaluate(
    (href) => __tailscaleLoginFixture.callbacks.notifyBrowseToURL(href),
    reopened,
  );
  await page.locator("#tailscale-login-qr canvas").waitFor();
  assert.equal(
    await decodedQR(page),
    reopened,
    `${name}: Escape clears the old login and permits a fresh auth URL`,
  );
  const contrast = await page
    .locator("#tailscale-login-qr canvas")
    .evaluate((canvas) => {
      const { data } = canvas
        .getContext("2d", { willReadFrequently: true })
        .getImageData(0, 0, canvas.width, canvas.height);
      const colors = new Set();
      for (let i = 0; i < data.length; i += 4)
        colors.add(`${data[i]},${data[i + 1]},${data[i + 2]}`);
      return {
        background: getComputedStyle(canvas.parentElement).backgroundColor,
        colors: [...colors],
      };
    });
  assert.equal(
    contrast.background,
    "rgb(255, 255, 255)",
    `${name}: light theme retains a white QR scan field`,
  );
  assert.ok(
    contrast.colors.includes("7,17,12") &&
      contrast.colors.includes("255,255,255"),
    `${name}: QR has dark/light scan contrast`,
  );
  await page.screenshot({
    path: `.build/tailscale-login-${name}-synthetic.png`,
  });

  await page.setViewportSize({ width: 390, height: 740 });
  const compact = await page.locator("#dialog").evaluate((dialog) => ({
    scroll: dialog.scrollWidth,
    client: dialog.clientWidth,
    qr: document
      .querySelector("#tailscale-login-qr")
      ?.getBoundingClientRect()
      .toJSON(),
    canvas: document
      .querySelector("#tailscale-login-qr canvas")
      ?.getBoundingClientRect()
      .toJSON(),
  }));
  assert.ok(
    compact.scroll <= compact.client + 1,
    `${name}: compact dialog has no horizontal overflow`,
  );
  assert.ok(
    Math.abs(compact.qr.x - compact.canvas.x) < 1,
    `${name}: QR retains scan contrast container`,
  );

  await page.evaluate(() =>
    __tailscaleLoginFixture.callbacks.notifyState("NeedsMachineAuth"),
  );
  await page
    .getByText("Sign-in complete. Waiting for device approval…")
    .waitFor();
  await page.evaluate(() =>
    __tailscaleLoginFixture.callbacks.notifyState("Running"),
  );
  await page.locator("#dialog").waitFor({ state: "hidden" });
  assert.match(await page.locator("#tail-status").innerText(), /connected/);

  // A late browse URL cannot reopen a completed login, and a non-login dialog
  // survives an unrelated Running notification.
  await page.evaluate(
    (href) => __tailscaleLoginFixture.callbacks.notifyBrowseToURL(href),
    synthetic("late-running"),
  );
  assert.equal(
    await page.locator("#dialog").evaluate((dialog) => dialog.open),
    false,
  );
  await page.setViewportSize({ width: 1100, height: 820 });
  await page.locator("#keys").click();
  await page.locator("#dialog").waitFor({ state: "visible" });
  await page.evaluate(() =>
    __tailscaleLoginFixture.callbacks.notifyState("Running"),
  );
  assert.ok(await page.locator("#dialog").evaluate((dialog) => dialog.open));
  await page.locator("#dialog-close").click();

  // pagehide models lock/navigation cleanup without persisting any user context.
  await page.evaluate(() => window.dispatchEvent(new Event("pagehide")));
  await page.evaluate(
    (href) => __tailscaleLoginFixture.callbacks.notifyBrowseToURL(href),
    synthetic("late-lock"),
  );
  assert.equal(
    await page.locator("#dialog").evaluate((dialog) => dialog.open),
    false,
  );
  assert.deepEqual(errors, [], `${name}: no client errors`);
  await context.close();
}

async function runLockCheck(browser, name) {
  const context = await browser.newContext();
  await context.route("**/client/wasm-runtime.js*", (route) =>
    route.fulfill({ contentType: "text/javascript", body: runtimeFixture }),
  );
  const page = await context.newPage();
  await page.goto(origin);
  await page.locator("#password").fill("synthetic lock fixture passphrase");
  await page.locator("#unlock-button").click();
  await page.locator("#workspace").waitFor();
  await page.locator("#tailscale-login").click();
  await page.waitForFunction(
    () => !!globalThis.__tailscaleLoginFixture?.callbacks,
  );
  await page.evaluate(
    (href) => __tailscaleLoginFixture.callbacks.notifyBrowseToURL(href),
    synthetic("before-lock"),
  );
  await page.locator("#tailscale-login-qr canvas").waitFor();
  await page.evaluate(() => window.dispatchEvent(new Event("pagehide")));
  await page.evaluate(
    (href) => __tailscaleLoginFixture.callbacks.notifyBrowseToURL(href),
    synthetic("after-lock"),
  );
  assert.equal(
    await page.locator("#tailscale-login-qr canvas").count(),
    0,
    `${name}: locking clears QR and rejects late auth URLs`,
  );
  assert.equal(
    await page.locator("#dialog").getAttribute("data-tailscale-login"),
    null,
    `${name}: locking releases the login dialog identity`,
  );
  await context.close();
}

async function runDiscoveryContinuationCheck(browser, name) {
  const context = await browser.newContext();
  await context.route("**/client/wasm-runtime.js*", (route) =>
    route.fulfill({ contentType: "text/javascript", body: runtimeFixture }),
  );
  const page = await context.newPage();
  await page.goto(origin);
  await page
    .locator("#password")
    .fill("synthetic discovery fixture passphrase");
  await page.locator("#unlock-button").click();
  await page.locator("#workspace").waitFor();
  await page.locator("#discover").click();
  await page.waitForFunction(
    () => !!globalThis.__tailscaleLoginFixture?.callbacks,
  );
  await page.evaluate(
    (href) => __tailscaleLoginFixture.callbacks.notifyBrowseToURL(href),
    synthetic("discovery"),
  );
  await page.locator("#tailscale-login-qr canvas").waitFor();
  await page.evaluate(() =>
    __tailscaleLoginFixture.callbacks.notifyState("Running"),
  );
  await page.getByRole("heading", { name: "Discover devices" }).waitFor();
  assert.equal(
    await page.locator("#dialog").getAttribute("data-discovery"),
    "true",
    `${name}: pending discovery resumes after login`,
  );
  await page.getByText("0 devices available").waitFor();
  await context.close();
}

try {
  for (const [name, engine] of [
    ["chromium", chromium],
    ["webkit", webkit],
  ]) {
    const browser = await engine.launch();
    try {
      await runControllerChecks(browser, name);
      await runAppChecks(browser, name);
      await runLockCheck(browser, name);
      await runDiscoveryContinuationCheck(browser, name);
      console.log(`${name}: Tailscale QR login checks passed.`);
    } finally {
      await browser.close();
    }
  }
} finally {
  await vite.close();
}
