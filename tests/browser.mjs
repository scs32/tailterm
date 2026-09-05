import { chromium, webkit } from "@playwright/test";
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { mkdtempSync, rmSync } from "node:fs";
import os from "node:os";
import path from "node:path";
import { once } from "node:events";
import { generateKeyPairSync } from "node:crypto";
import ssh2 from "ssh2";
import { exercisePaneGroups } from "./pane-groups-browser.mjs";
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
const dir = mkdtempSync(path.join(os.tmpdir(), "tailterm-browser-"));
const proc = spawn(process.execPath, ["server/index.js"], {
  env: {
    ...process.env,
    PORT: "14318",
    VAULT_PATH: path.join(dir, "vault.enc"),
    NODE_ENV: "production",
  },
  stdio: ["ignore", "pipe", "pipe"],
});
let log = "";
proc.stdout.on("data", (d) => (log += d));
proc.stderr.on("data", (d) => (log += d));
let browser;
try {
  for (let i = 0; i < 100 && !log.includes("Tailterm:"); i++)
    await new Promise((r) => setTimeout(r, 50));
  assert.match(log, /Tailterm:/);
  browser = await (
    process.env.TEST_BROWSER === "webkit" ? webkit : chromium
  ).launch();
  const page = await browser.newPage({
    viewport: { width: 1440, height: 1050 },
  });
  const errors = [];
  page.on("pageerror", (e) => errors.push(e.message));
  await page.goto("http://127.0.0.1:14318");
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
    await page.locator(".server-item.selected strong").textContent(),
    "Development",
  );
  await page.locator("#edit-server").click();
  await page.locator("[name=name]").fill("Development lab");
  await page.getByRole("button", { name: "Save server" }).click();
  await page.locator("#dialog").waitFor({ state: "hidden" });
  assert.equal(
    await page.locator(".server-item.selected strong").textContent(),
    "Development lab",
  );
  await page.locator("#keys").click();
  await page.locator("#key-form").waitFor();
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
    await page.locator(".server-item.selected strong").textContent(),
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
  await exercisePaneGroups(page);
  while ((await page.locator("[data-close]").count()) > 1)
    await page.locator("[data-close]").last().click();
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
      !document.querySelector(".tab.active").textContent.includes("unverified"),
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
  // Selecting a server without a tab must hide the old terminal, not retarget it.
  await page
    .locator("[data-server]")
    .filter({ hasText: "Development lab" })
    .click();
  assert.equal(await page.locator(".terminal-instance:visible").count(), 0);
  assert.equal(
    await page.locator(".server-item.selected strong").textContent(),
    "Development lab",
  );
  await page.locator(`[data-tab="${originalTmuxId}"]`).click();
  assert.equal(
    await page.locator(".server-item.selected strong").textContent(),
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
  await page.locator("#edit-server").click();
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
  await page.locator(`[data-tab="${originalTmuxId}"]`).click();
  assert.equal(
    await page.locator(".server-item.selected strong").textContent(),
    "Password host",
  );
  assert.equal(
    await page.locator(".server-item.selected strong").textContent(),
    "Password host",
  );
  assert.equal(await page.locator("#tmux").count(), 0);
  await page.locator(`[data-close="${secondTabId}"]`).click();
  assert.equal(
    await page.locator(".tab.active [data-tab]").getAttribute("data-tab"),
    originalTmuxId,
  );
  await page.locator("#edit-server").click();
  await page.locator("[name=name]").fill("Renamed host");
  await page.getByRole("button", { name: "Save server" }).click();
  await page.locator("#dialog").waitFor({ state: "hidden" });
  assert.equal(
    await page.locator(".server-item.selected strong").textContent(),
    "Renamed host",
  );
  assert.match(await page.locator(".tab.active").textContent(), /Renamed host/);
  // New connection button is a draft and leaves existing connections intact.
  await page.locator("#new-tab").click();
  assert.equal(await page.locator(".terminal-instance:visible").count(), 0);
  assert.equal(await page.locator(".connection-bar").count(), 0);
  assert.equal(await page.locator("[data-tab]").count(), initialTabCount);
  const beforeDiscovery = discoveryCount;
  await page
    .locator("[data-server]")
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
  const autoTitle = await page.locator(".tab.active").textContent();
  assert.match(autoTitle, /tt-[a-f0-9]{16}/);
  const autoName = autoTitle.match(/tt-[a-f0-9]{16}/)[0];
  await page.waitForFunction(
    () =>
      !document.querySelector(".tab.active").textContent.includes("unverified"),
  );
  await page.locator(`[data-close="${autoId}"]`).click();
  await page.locator("#new-tab").click();
  await page.locator("[data-resume]").filter({ hasText: autoName }).click();
  await page.waitForFunction(() =>
    document
      .querySelector("#terminal-status")
      .textContent.includes("Connected"),
  );
  assert.match(
    await page.locator(".tab.active").textContent(),
    new RegExp(autoName),
  );
  const resumedId = await page
    .locator(".tab.active [data-tab]")
    .getAttribute("data-tab");
  await page.locator(`[data-close="${resumedId}"]`).click();
  await page.locator("#new-tab").click();
  await page.locator("#launcher-name").fill("named-from-fullscreen");
  await page.locator("#start-session").click();
  await page.waitForFunction(() =>
    document
      .querySelector("#terminal-status")
      .textContent.includes("Connected"),
  );
  assert.match(
    await page.locator(".tab.active").textContent(),
    /named-from-fullscreen/,
  );
  const namedId = await page
    .locator(".tab.active [data-tab]")
    .getAttribute("data-tab");
  await page.waitForFunction(
    () =>
      !document.querySelector(".tab.active").textContent.includes("unverified"),
  );
  await page.locator(`[data-close="${namedId}"]`).click();
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
    await page.locator(".tab.active").textContent(),
    /missing \(unverified\)/,
  );
  // Closing a background tab must not switch away from the current one.
  const activeBeforeClose = await page
    .locator(".tab.active [data-tab]")
    .getAttribute("data-tab");
  await page.locator("[data-close]").first().click();
  assert.equal(
    await page.locator(".tab.active [data-tab]").getAttribute("data-tab"),
    activeBeforeClose,
  );
  // Deleting a server removes its connections and bookmarks, without killing remote tmux.
  await page.locator("#edit-server").click();
  page.once("dialog", (d) => d.accept());
  await page.locator("#delete-server").click();
  await page.locator("#dialog").waitFor({ state: "hidden" });
  assert.equal(await page.locator("[data-tab]").count(), 0);
  assert.equal(await page.locator("[data-saved]").count(), 0);
  assert.equal(await page.locator(".terminal-instance").count(), 0);
  // Load and instantiate the actual 26MB Tailscale WASM module; do not authorize a tailnet.
  await page.locator("#tailscale-login").click();
  await page.waitForFunction(
    () =>
      ["NeedsLogin", "Starting", "Running", "NeedsMachineAuth"].includes(
        document.querySelector("#tail-status")?.textContent,
      ) ||
      document
        .querySelector("#dialog")
        ?.textContent.includes("Sign in to Tailscale"),
    { timeout: 30000 },
  );
  assert.deepEqual(errors, []);
  console.log(
    "Browser checks passed: create/unlock, server create/edit, key dialog, lock/reopen, desktop/mobile layout, host trust, password retry/remember, live SSH input, tab/server synchronization, same-tab reconnect, tmux deduplication/discovery/failure, deletion cleanup, actual Tailscale WASM startup.",
  );
} finally {
  await browser?.close();
  proc.kill("SIGTERM");
  await once(proc, "exit");
  ssh.close();
  rmSync(dir, { recursive: true, force: true });
}
