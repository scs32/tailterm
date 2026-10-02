import { useDomRenderer } from "./dom-renderer.mjs";
import { exerciseGeneratedKey } from "./ssh-key-browser.mjs";
import { chromium, webkit } from "@playwright/test";
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { mkdtempSync, rmSync } from "node:fs";
import os from "node:os";
import path from "node:path";
import { once } from "node:events";
import { generateKeyPairSync } from "node:crypto";
import { createServer as createNetServer } from "node:net";
import ssh2 from "ssh2";
import { exercisePaneGroups } from "./pane-groups-browser.mjs";
// Start-of-fixture: tests/browser.mjs and tests/integration.test.js keep
// identical copies of this block. The suite picks a free port, starts its own
// server/index.js on it and takes the origin only from that child's readiness
// line. Only one process can bind the port, so a live child that printed
// readiness is the server answering there; a port another process holds is
// refused, never reused.
const running = (proc) => proc.exitCode === null && proc.signalCode === null;
async function freePort() {
  const probe = createNetServer();
  probe.listen(0, "127.0.0.1");
  await once(probe, "listening");
  const { port } = probe.address();
  probe.close();
  await once(probe, "close");
  return port;
}
async function stopAppServer(proc) {
  // An exit that already happened never fires again; awaiting it would hang.
  if (!running(proc)) return;
  const exited = once(proc, "exit");
  proc.kill("SIGTERM");
  const force = setTimeout(() => proc.kill("SIGKILL"), 5000);
  await exited;
  clearTimeout(force);
}
async function startAppServer({ dir, allocate = freePort, onSpawn }) {
  let refused;
  for (let attempt = 0; attempt < 5; attempt++) {
    const port = await allocate();
    const proc = spawn(process.execPath, ["server/index.js"], {
      env: {
        ...process.env,
        PORT: String(port),
        VAULT_PATH: path.join(dir, "vault.enc"),
        NODE_ENV: "production",
      },
      stdio: ["ignore", "pipe", "pipe"],
    });
    onSpawn?.(proc);
    let log = "";
    proc.stdout.on("data", (d) => (log += d));
    proc.stderr.on("data", (d) => (log += d));
    const closed = once(proc, "close");
    const ready = new RegExp(`Tailterm: (http://127\\.0\\.0\\.1:${port})\\s`);
    const deadline = Date.now() + 30000;
    while (!ready.test(log) && running(proc) && Date.now() < deadline)
      await new Promise((r) => setTimeout(r, 50));
    const origin = ready.exec(log)?.[1];
    if (origin && running(proc)) return { proc, port, origin, log: () => log };
    await stopAppServer(proc);
    await closed;
    if (!log.includes("EADDRINUSE"))
      throw new Error(
        `server/index.js did not become ready on 127.0.0.1:${port}:\n${log}`,
      );
    refused = port;
  }
  throw new Error(
    `refusing to use 127.0.0.1:${refused}: this run did not start the server on it`,
  );
}
// End-of-fixture.
const hostKey = generateKeyPairSync("rsa", {
  modulusLength: 2048,
  privateKeyEncoding: { type: "pkcs1", format: "pem" },
  publicKeyEncoding: { type: "spki", format: "pem" },
}).privateKey;
const remoteNames = new Set(["main"]);
let discoveryCount = 0;
let input = "",
  remoteCommand = "";
const ssh = new ssh2.Server({ hostKeys: [hostKey] }, (client) => {
  client.on("error", () => {});
  client.on("authentication", (ctx) =>
    ctx.method === "password" && ctx.password === "browser-ssh-password"
      ? ctx.accept()
      : ctx.reject(["password"]),
  );
  client.on("ready", () =>
    client.on("session", (accept) => {
      const session = accept();
      session.on("pty", (accept) => accept());
      session.on("window-change", (accept) => accept?.());
      session.on("shell", (accept) => {
        const stream = accept();
        stream.write("Browser SSH fixture ready\r\n");
        stream.on("data", (d) => {
          input += d;
          stream.write(d);
        });
      });
      session.on("exec", (accept, reject, info) => {
        remoteCommand = info.command;
        const stream = accept();
        if (
          info.command.includes(" list-sessions -F ") &&
          !info.command.includes("attach-session")
        ) {
          discoveryCount++;
          stream.write(
            [...remoteNames].map((name) => `${name}|1|1\n`).join(""),
          );
          stream.exit(0);
          stream.end();
        } else if (info.command.includes("missing")) {
          stream.stderr.write("tmux unavailable");
          stream.exit(1);
          stream.end();
        } else {
          const name = info.command.match(
            /tt-[a-f0-9]{16}|named-from-fullscreen/,
          );
          if (name) remoteNames.add(name[0]);
          stream.write("tmux fixture ready\r\n");
        }
      });
    }),
  );
});
ssh.listen(0, "127.0.0.1");
await once(ssh, "listening");
// Everything this run started is torn down exactly once: on success, on a
// failed assertion, and on a signal (the matrix sends SIGTERM on a timeout).
const children = new Set();
let dir, browser, cleaning;
function cleanup() {
  cleaning ??= (async () => {
    await Promise.allSettled([
      browser?.close(),
      ...[...children].map(stopAppServer),
    ]);
    ssh.close();
    if (dir) rmSync(dir, { recursive: true, force: true });
  })();
  return cleaning;
}
for (const [signal, code] of [
  ["SIGHUP", 129],
  ["SIGINT", 130],
  ["SIGTERM", 143],
])
  process.once(signal, async () => {
    // A browser that will not close must not keep the server alive.
    setTimeout(() => process.exit(code), 8000).unref();
    await cleanup();
    process.exit(code);
  });
// Last resort when the process exits without finishing cleanup.
process.on("exit", () => {
  for (const proc of children) if (running(proc)) proc.kill("SIGKILL");
  if (dir) rmSync(dir, { recursive: true, force: true });
});
try {
  // NODE_ENV=production serves dist/, which the verification matrix does not
  // build (it builds dist-static). Build it from this checkout so the suite never
  // depends on, or serves, a missing or stale bundle.
  const build = spawn(
    process.execPath,
    ["node_modules/vite/bin/vite.js", "build"],
    { stdio: ["ignore", "pipe", "pipe"] },
  );
  children.add(build);
  let buildLog = "";
  build.stdout.on("data", (d) => (buildLog += d));
  build.stderr.on("data", (d) => (buildLog += d));
  const [buildCode] = await once(build, "exit");
  children.delete(build);
  assert.equal(buildCode, 0, "vite build failed:\n" + buildLog);
  dir = mkdtempSync(path.join(os.tmpdir(), "tailterm-browser-"));
  // TAILTERM_TEST_PORT pins the port, only to prove that a port held by
  // another process is refused.
  const pinned = process.env.TAILTERM_TEST_PORT;
  const { proc, origin } = await startAppServer({
    dir,
    allocate: pinned ? () => Number(pinned) : freePort,
    onSpawn: (child) => children.add(child),
  });
  console.log(`Browser suite server: ${origin} pid ${proc.pid}`);
  const isWebKit = process.env.TEST_BROWSER === "webkit";
  browser = await (isWebKit ? webkit : chromium).launch();
  const page = await browser.newPage({
    viewport: { width: 1440, height: 1050 },
    // WebKit has no clipboard-write permission; a click's user activation
    // already allows writeText there.
    permissions: isWebKit
      ? ["clipboard-read"]
      : ["clipboard-read", "clipboard-write"],
  });
  await useDomRenderer(page);
  // Inactive tabs reveal their close control only on hover or focus.
  async function closeTab(id) {
    await page.locator(`#tabs .tab:has([data-tab="${id}"])`).hover();
    await page.locator(`[data-close="${id}"]`).click();
  }
  const errors = [];
  page.on("pageerror", (e) => errors.push(e.message));
  await page.goto(origin);
  await page.locator("#password").fill("browser test passphrase");
  await page.locator("#unlock-button").click();
  await page.locator("#workspace").waitFor();
  assert.equal(await page.locator(".connection-bar,.title-row").count(), 0);
  assert.equal(await page.locator("aside #discover").count(), 1);
  assert.equal(
    await page.locator("#add-server, #add-server-bottom, #empty-add").count(),
    0,
  );
  await page.evaluate(async () => {
    const r = await fetch("/api/servers", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        name: "Development",
        host: "dev.example.ts.net",
        username: "ubuntu",
        port: 22,
        mode: "auto",
      }),
    });
    if (!r.ok) throw Error(await r.text());
  });
  await page.reload();
  await page.locator("#workspace").waitFor();
  assert.equal(
    await page.locator(".server-card.chosen strong").textContent(),
    "Development",
  );
  await page.locator("#commands").click();
  await page.locator("#command-query").fill("Edit selected server");
  await page
    .locator("#command-results button")
    .filter({ hasText: "Edit selected server" })
    .click();
  await page.locator("[name=name]").fill("Development lab");
  await page.getByRole("button", { name: "Save server" }).click();
  await page.locator("#dialog").waitFor({ state: "hidden" });
  assert.equal(
    await page.locator(".server-card.chosen strong").textContent(),
    "Development lab",
  );
  await page.locator("#keys").click();
  await page.locator("#import-key").evaluate((el) => (el.open = true));
  await page.locator("#key-form").waitFor();
  await exerciseGeneratedKey(page);
  await page.locator("#dialog-close").click();
  await page.screenshot({
    path: path.join(process.cwd(), "workspace-preview.png"),
    fullPage: true,
  });
  await page.setViewportSize({ width: 390, height: 844 });
  assert.ok(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= innerWidth,
    ),
  );
  await page.screenshot({
    path: path.join(process.cwd(), "mobile-preview.png"),
    fullPage: true,
  });
  await page.setViewportSize({ width: 1440, height: 1050 });
  await page.locator("#lock").click();
  await page.locator("#lockscreen").waitFor();
  await page.locator("#password").fill("browser test passphrase");
  await page.locator("#unlock-button").click();
  await page.locator("#workspace").waitFor();
  assert.equal(
    await page.locator(".server-card.chosen strong").textContent(),
    "Development lab",
  );
  // Exercise the rendered trust/password prompts against a real SSH server.
  await page.evaluate(async (port) => {
    const r = await fetch("/api/servers", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        name: "Password host",
        host: "127.0.0.1",
        username: "ubuntu",
        port,
        mode: "auto",
      }),
    });
    if (!r.ok) throw Error(await r.text());
  }, ssh.address().port);
  await page.reload();
  await page
    .locator(".server-item")
    .filter({ hasText: "Password host" })
    .click();
  await page.locator("#launcher-shell").click();
  await page.getByRole("button", { name: "Trust & continue" }).click();
  await page.locator(".login-prompt [name=password]").fill("wrong");
  await page
    .locator(".login-prompt")
    .getByRole("button", { name: "Sign in", exact: true })
    .click();
  await page
    .getByText(
      "The previous credential was not accepted, or another authentication step is required.",
    )
    .waitFor();
  await page
    .locator(".login-prompt [name=password]")
    .fill("browser-ssh-password");
  await page.locator(".login-prompt [name=remember]").check();
  await page
    .locator(".login-prompt")
    .getByRole("button", { name: "Sign in", exact: true })
    .click();
  await page.waitForFunction(() =>
    document
      .querySelector("#terminal-status")
      .textContent.includes("Connected"),
  );
  await page.locator(".xterm-helper-textarea").first().focus();
  await page.keyboard.type("browser typed this");
  for (let i = 0; i < 100 && !input.includes("browser typed this"); i++)
    await new Promise((r) => setTimeout(r, 20));
  assert.ok(input.includes("browser typed this"));
  // Titles shrink until minimum width, then scroll without moving the page.
  const firstWidth = await page
    .locator("#tabs .tab")
    .first()
    .evaluate((e) => e.getBoundingClientRect().width);
  for (let i = 0; i < 9; i++) {
    await page.locator("#new-tab").click();
    await page.locator("#launcher-shell").click();
    await page.waitForFunction(() =>
      document
        .querySelector("#terminal-status")
        .textContent.includes("Connected"),
    );
    if (i === 4)
      assert.ok(
        (await page
          .locator("#tabs .tab")
          .first()
          .evaluate((e) => e.getBoundingClientRect().width)) < firstWidth,
      );
  }
  await page.waitForFunction(
    () =>
      !document.querySelector("#tabs-left").hidden &&
      document.querySelector("#tabs").scrollLeft > 0,
  );
  assert.ok(
    (await page
      .locator("#tabs .tab")
      .first()
      .evaluate((e) => e.getBoundingClientRect().width)) >= 109,
  );
  await page.waitForFunction(() => {
    const a = document.querySelector(".tab.active").getBoundingClientRect(),
      v = document.querySelector("#tabs").getBoundingClientRect();
    return a.left >= v.left - 1 && a.right <= v.right + 1;
  });
  await page.screenshot({
    path: path.join(process.cwd(), "tabs-preview.png"),
    fullPage: true,
  });
  const beforeScroll = await page
    .locator("#tabs")
    .evaluate((e) => e.scrollLeft);
  await page.locator("#tabs-left").click();
  await page.waitForFunction(
    (before) => document.querySelector("#tabs").scrollLeft < before,
    beforeScroll,
  );
  await page.locator(".tab.active [data-tab]").press("Home");
  await page.waitForFunction(
    () => document.querySelector("#tabs").scrollLeft < 1,
  );
  assert.equal(
    await page
      .locator("#tabs [data-tab]")
      .first()
      .getAttribute("aria-selected"),
    "true",
  );
  await page.locator("#tabs-right").click();
  await page.waitForFunction(
    () => document.querySelector("#tabs").scrollLeft > 0,
  );
  await page.setViewportSize({ width: 390, height: 844 });
  assert.ok(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= innerWidth,
    ),
  );
  await page.locator("#tabs [data-tab]").first().press("End");
  await page.waitForFunction(() => {
    const a = document.querySelector(".tab.active").getBoundingClientRect(),
      v = document.querySelector("#tabs").getBoundingClientRect();
    return a.left >= v.left - 1 && a.right <= v.right + 1;
  });
  await page.setViewportSize({ width: 1440, height: 1050 });
  // Safari traverses every control with Option+Tab when macOS Full Keyboard
  // Access is off; use that native key as static-browser.mjs does.
  const press = page.keyboard.press.bind(page.keyboard);
  if (isWebKit)
    page.keyboard.press = (key, options) =>
      press(key === "Tab" ? "Alt+Tab" : key, options);
  try {
    await exercisePaneGroups(page, undefined, { staticControls: false });
  } finally {
    page.keyboard.press = press;
  }
  while ((await page.locator("[data-close]").count()) > 1) {
    await page.locator("#tabs .tab").last().hover();
    await page.locator("[data-close]").last().click();
  }
  await page.waitForFunction(
    () =>
      document.querySelector("#tabs-left").hidden &&
      document.querySelector("#tabs-right").hidden,
  );

  await page.locator("#new-tab").click();
  await page.locator("#launcher-name").fill("main");
  await page.locator("#start-session").click();
  await page.waitForFunction(
    () =>
      document.querySelectorAll(".terminal-instance").length === 2 &&
      document
        .querySelector("#terminal-status")
        .textContent.includes("Connected"),
  );
  assert.match(remoteCommand, /new-session -A -s .*main/);
  assert.equal(await page.locator(".login-prompt").count(), 0);
  await page.waitForFunction(
    () =>
      !(
        document.querySelector(".tab.active [data-tab]")?.dataset.tooltip ||
        document.querySelector(".tab.active [data-tab]")?.title ||
        ""
      ).includes("unverified"),
  );
  const originalTmuxId = await page
    .locator(".tab.active [data-tab]")
    .getAttribute("data-tab");
  const initialTabCount = await page.locator("[data-tab]").count();
  await page.locator("#new-tab").click();
  await page.locator("[data-resume]").filter({ hasText: "main" }).click();
  assert.equal(await page.locator("[data-tab]").count(), initialTabCount);
  await page.locator("#new-tab").click();
  await page.locator("#launcher-name").fill("main");
  await page.locator("#start-session").click();
  assert.equal(await page.locator("[data-tab]").count(), initialTabCount);
  await page.locator("#reconnect").click();
  await page.waitForFunction(() =>
    document
      .querySelector("#terminal-status")
      .textContent.includes("Connected"),
  );
  assert.equal(await page.locator("[data-tab]").count(), initialTabCount);
  assert.equal(
    await page.locator(".tab.active [data-tab]").getAttribute("data-tab"),
    originalTmuxId,
  );
  // Filtering to a server without a tab hides the old terminal without retargeting it.
  await page.locator("#all-servers").click();
  await page
    .locator("[data-server]")
    .filter({ hasText: "Development lab" })
    .click();
  assert.equal(await page.locator(".terminal-instance:visible").count(), 0);
  assert.equal(
    await page.locator(".server-card.chosen strong").textContent(),
    "Development lab",
  );
  await page.locator("#all-servers").click();
  await page.locator(`[data-tab="${originalTmuxId}"]`).click();
  assert.equal(
    await page.locator(".server-card.chosen strong").textContent(),
    "Password host",
  );
  assert.equal(await page.locator("#tmux").count(), 0);
  assert.equal(await page.locator("#launcher-name").inputValue(), "");
  // Two connected server profiles must keep header, sidebar and active shell aligned.
  await page.evaluate(async () => {
    const data = await (await fetch("/api/data")).json();
    const original = data.servers.find((s) => s.name === "Password host");
    const { id, hasPassword, ...profile } = original;
    await fetch("/api/servers", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        ...profile,
        name: "Second host",
        username: "second",
      }),
    });
  });
  // Refresh profiles through a normal UI mutation without reloading live tabs.
  await page.locator("#commands").click();
  await page.locator("#command-query").fill("Edit selected server");
  await page
    .locator("#command-results button")
    .filter({ hasText: "Edit selected server" })
    .click();
  await page.getByRole("button", { name: "Save server" }).click();
  await page.locator("#dialog").waitFor({ state: "hidden" });
  await page
    .locator("[data-server]")
    .filter({ hasText: "Second host" })
    .click();
  await page.locator("#launcher-shell").click();
  await page
    .locator(".login-prompt [name=password]")
    .fill("browser-ssh-password");
  await page
    .locator(".login-prompt")
    .getByRole("button", { name: "Sign in", exact: true })
    .click();
  await page.waitForFunction(() =>
    document
      .querySelector("#terminal-status")
      .textContent.includes("Connected"),
  );
  const secondTabId = await page
    .locator(".tab.active [data-tab]")
    .getAttribute("data-tab");
  assert.match(
    await page.locator("#terminal-status").getAttribute("data-tooltip"),
    /second@127/,
  );
  await page.locator("#all-servers").click();
  await page.locator(`[data-tab="${originalTmuxId}"]`).click();
  assert.equal(
    await page.locator(".server-card.chosen strong").textContent(),
    "Password host",
  );
  assert.equal(
    await page.locator(".server-card.chosen strong").textContent(),
    "Password host",
  );
  assert.equal(await page.locator("#tmux").count(), 0);
  await closeTab(secondTabId);
  assert.equal(
    await page.locator(".tab.active [data-tab]").getAttribute("data-tab"),
    originalTmuxId,
  );
  await page.locator("#commands").click();
  await page.locator("#command-query").fill("Edit selected server");
  await page
    .locator("#command-results button")
    .filter({ hasText: "Edit selected server" })
    .click();
  await page.locator("[name=name]").fill("Renamed host");
  await page.getByRole("button", { name: "Save server" }).click();
  await page.locator("#dialog").waitFor({ state: "hidden" });
  assert.equal(
    await page.locator(".server-card.chosen strong").textContent(),
    "Renamed host",
  );
  assert.match(
    await page
      .locator(".tab.active [data-tab]")
      .evaluate((el) => el.dataset.tooltip || el.title),
    /Renamed host/,
  );
  // New connection button is a draft and leaves existing connections intact.
  await page.locator("#new-tab").click();
  assert.equal(await page.locator(".terminal-instance:visible").count(), 0);
  assert.equal(await page.locator(".connection-bar").count(), 0);
  assert.equal(await page.locator("[data-tab]").count(), initialTabCount);
  const beforeDiscovery = discoveryCount;
  await page
    .locator("[data-launch-server]")
    .filter({ hasText: "Renamed host" })
    .click();
  for (let i = 0; i < 100 && discoveryCount === beforeDiscovery; i++)
    await new Promise((r) => setTimeout(r, 20));
  assert.ok(
    discoveryCount > beforeDiscovery,
    "Selecting a server triggers remote discovery",
  );
  await page.locator("#new-tab").click();
  assert.equal(await page.locator("#launcher-name").inputValue(), "");
  await page.locator("#fullscreen").click();
  await page.waitForFunction(() => !!document.fullscreenElement);
  await page.locator("#start-session").click();
  await page.waitForFunction(() =>
    document
      .querySelector("#terminal-status")
      .textContent.includes("Connected"),
  );
  const autoId = await page
    .locator(".tab.active [data-tab]")
    .getAttribute("data-tab");
  const autoTitle = await page
    .locator(".tab.active [data-tab]")
    .evaluate((el) => el.dataset.tooltip || el.title);
  assert.match(autoTitle, /tt-[a-f0-9]{16}/);
  const autoName = autoTitle.match(/tt-[a-f0-9]{16}/)[0];
  await page.waitForFunction(
    () =>
      !(
        document.querySelector(".tab.active [data-tab]")?.dataset.tooltip ||
        document.querySelector(".tab.active [data-tab]")?.title ||
        ""
      ).includes("unverified"),
  );
  await closeTab(autoId);
  await page.locator("#new-tab").click();
  await page.locator("[data-resume]").filter({ hasText: autoName }).click();
  await page.waitForFunction(() =>
    document
      .querySelector("#terminal-status")
      .textContent.includes("Connected"),
  );
  assert.match(
    await page
      .locator(".tab.active [data-tab]")
      .evaluate((el) => el.dataset.tooltip || el.title),
    new RegExp(autoName),
  );
  const resumedId = await page
    .locator(".tab.active [data-tab]")
    .getAttribute("data-tab");
  await closeTab(resumedId);
  await page.locator("#new-tab").click();
  await page.locator("#launcher-name").fill("named-from-fullscreen");
  await page.locator("#start-session").click();
  await page.waitForFunction(() =>
    document
      .querySelector("#terminal-status")
      .textContent.includes("Connected"),
  );
  assert.match(
    await page
      .locator(".tab.active [data-tab]")
      .evaluate((el) => el.dataset.tooltip || el.title),
    /named-from-fullscreen/,
  );
  const namedId = await page
    .locator(".tab.active [data-tab]")
    .getAttribute("data-tab");
  await page.waitForFunction(
    () =>
      !(
        document.querySelector(".tab.active [data-tab]")?.dataset.tooltip ||
        document.querySelector(".tab.active [data-tab]")?.title ||
        ""
      ).includes("unverified"),
  );
  await closeTab(namedId);
  await page.locator("#fullscreen").click();
  await page.locator("#new-tab").click();
  await page.locator("#launcher-name").fill("missing");
  await page.locator("#start-session").click();
  await page.waitForFunction(() =>
    document
      .querySelector("#terminal-status")
      .textContent.includes("Disconnected"),
  );
  assert.equal(await page.locator("[data-saved]").count(), 0);
  assert.match(
    await page
      .locator(".tab.active [data-tab]")
      .evaluate((el) => el.dataset.tooltip || el.title),
    /missing \(unverified\)/,
  );
  // Closing a background tab must not switch away from the current one.
  const activeBeforeClose = await page
    .locator(".tab.active [data-tab]")
    .getAttribute("data-tab");
  await page.locator("#tabs .tab").first().hover();
  await page.locator("[data-close]").first().click();
  assert.equal(
    await page.locator(".tab.active [data-tab]").getAttribute("data-tab"),
    activeBeforeClose,
  );
  // Deleting a server removes its connections and bookmarks, without killing remote tmux.
  await page.locator("#commands").click();
  await page.locator("#command-query").fill("Edit selected server");
  await page
    .locator("#command-results button")
    .filter({ hasText: "Edit selected server" })
    .click();
  await page.locator("#delete-server").click();
  await page.locator(".confirmation-dialog [data-confirm]").click();
  await page.locator("#dialog").waitFor({ state: "hidden" });
  assert.equal(await page.locator("[data-tab]").count(), 0);
  assert.equal(await page.locator("[data-saved]").count(), 0);
  assert.equal(await page.locator(".terminal-instance").count(), 0);
  // Load and instantiate the actual 26MB Tailscale WASM module; do not authorize a tailnet.
  await page.locator("#tailscale-login").click();
  await page.waitForFunction(
    () =>
      /Sign in to Tailscale|Starting|Tailscale connected|NeedsMachineAuth/.test(
        document.querySelector("#tail-status")?.textContent,
      ) ||
      document
        .querySelector("#dialog")
        ?.textContent.includes("Sign in to Tailscale"),
    { timeout: 30000 },
  );
  assert.deepEqual(errors, []);
  // The child held the port for the whole run, so every request reached it.
  assert.ok(running(proc), "the suite's own server exited during the run");
  console.log(
    "Browser checks passed: create/unlock, server create/edit, key dialog, lock/reopen, desktop/mobile layout, host trust, password retry/remember, live SSH input, tab/server synchronization, same-tab reconnect, tmux deduplication/discovery/failure, deletion cleanup, actual Tailscale WASM startup.",
  );
} finally {
  await cleanup();
}
