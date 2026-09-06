import { exerciseWorkspaceControls } from "./workspace-controls-browser.mjs";
import { exerciseGeneratedKey } from "./ssh-key-browser.mjs";
import { exerciseServerFilters } from "./server-filters-browser.mjs";
import { exercisePopupReview } from "./popup-review-browser.mjs";
import {
  exerciseVoiceDictation,
  mockSpeechWorker,
} from "./voice-dictation-browser.mjs";
import { exerciseLocalHistory } from "./local-history-browser.mjs";
import {
  exerciseWorkspaceActions,
  exerciseForgetDevice,
} from "./workspace-actions-browser.mjs";
import { chromium } from "@playwright/test";
import assert from "node:assert/strict";
import { createServer } from "node:http";
import { connect as tcpConnect } from "node:net";
import { readFile, stat } from "node:fs/promises";
import { readFileSync } from "node:fs";
import path from "node:path";
import { once } from "node:events";
import { generateKeyPairSync } from "node:crypto";
import ssh2 from "ssh2";
import { WebSocketServer } from "ws";
import { gzipSync } from "node:zlib";
import { exercisePaneGroups } from "./pane-groups-browser.mjs";
import { assertTerminalBounds } from "./terminal-bounds.mjs";
import { exerciseVaultReset } from "./vault-reset-browser.mjs";
import { openVault } from "../client/vault-crypto.js";
import { attachSFTP } from "./sftp-fixture.mjs";
import { exerciseImageUpload } from "./upload-browser.mjs";
import { finishRestoration } from "./restore-browser.mjs";
import { exerciseWorkspaceContinuity } from "./workspace-browser.mjs";
const hostKey = generateKeyPairSync("rsa", {
  modulusLength: 2048,
  privateKeyEncoding: { type: "pkcs1", format: "pem" },
  publicKeyEncoding: { type: "spki", format: "pem" },
}).privateKey;
const keyPassphrase = "fixture-key-passphrase";
const loginKey = ssh2.utils.generateKeyPairSync("ed25519", {
  passphrase: keyPassphrase,
  cipher: "aes256-cbc",
}).private;
let generatedLogin;
const parsedLogin = ssh2.utils.parseKey(loginKey, keyPassphrase);
assert.equal(typeof parsedLogin.getPublicSSH, "function");
let input = "",
  stream,
  discoveries = 0,
  terminalStarts = 0;
const sessions = new Set(["main"]);
const identities = new Map(),
  uploadedFiles = new Map();
const uploadControl = {};
let nextSessionId = 0;
function identity(name) {
  if (!identities.has(name))
    identities.set(name, { id: "$" + nextSessionId++, created: "1234" });
  return identities.get(name);
}
const authMethods = [];
const ssh = new ssh2.Server(
  { hostKeys: [hostKey], banner: "Fixture SSH sign-in message" },
  (client) => {
    client.on("error", (e) => console.error("SSH fixture error:", e.message));
    client.on("authentication", (ctx) => {
      authMethods.push(ctx.method);
      if (ctx.username === "closeduser") {
        client._sock.end();
        return;
      }
      if (ctx.username === "keyuser") {
        if (
          ctx.method === "publickey" &&
          [parsedLogin, generatedLogin]
            .filter(Boolean)
            .some(
              (key) =>
                ctx.key.data.equals(key.getPublicSSH()) &&
                (!ctx.signature ||
                  key.verify(ctx.blob, ctx.signature, ctx.hashAlgo)),
            )
        )
          ctx.accept();
        else ctx.reject(["publickey"]);
      } else if (
        ctx.method === "password" &&
        ctx.password === "static-ssh-password"
      )
        ctx.accept();
      else ctx.reject(["password"]);
    });
    client.on("ready", () =>
      client.on("session", (accept) => {
        const session = accept();
        attachSFTP(session, uploadedFiles, uploadControl);
        session.on("pty", (accept) => accept());
        session.on("window-change", (accept) => accept?.());
        const terminal = (accepted) => {
          terminalStarts++;
          stream = accepted;
          stream.write("Tailserve fixture ready\r\n$ ");
          stream.on("data", (d) => {
            input += d;
            stream?.writable && accepted.write(d);
          });
        };
        session.on("shell", (accept) => terminal(accept()));
        session.on("exec", (accept, reject, info) => {
          const accepted = accept();
          if (info.command.includes("capture-pane -p -e -J")) {
            accepted.write(
              Array.from(
                { length: 300 },
                (_, i) =>
                  `\x1b[38;2;255;160;90m\x1b[1mcached history line ${i}\x1b[0m`,
              ).join("\n"),
            );
            accepted.exit(0);
            accepted.end();
            return;
          }
          if (info.command.includes("pane_current_path")) {
            accepted.write(
              Buffer.from("/tmp/fixture uploads\n").toString("base64") + "\n",
            );
            accepted.exit(0);
            accepted.end();
            return;
          }
          if (info.command.includes("rename-session -t")) {
            const names = info.command.match(
              /\b(?:renamed-static(?:-launcher)?|named-static|main)\b/g,
            );
            const requestedId = info.command.match(/\$[0-9]+/)?.[0];
            const oldName = requestedId
                ? [...identities].find(
                    ([, target]) => target.id === requestedId,
                  )?.[0]
                : names?.[0],
              nextName = names?.at(-1);
            if (!sessions.has(oldName) || sessions.has(nextName)) {
              accepted.stderr.write("Duplicate name or missing session");
              accepted.exit(1);
            } else {
              sessions.delete(oldName);
              sessions.add(nextName);
              identities.set(nextName, identity(oldName));
              identities.delete(oldName);
              accepted.exit(0);
            }
            accepted.end();
            return;
          }
          if (
            info.command.includes("list-sessions -F") &&
            !info.command.includes("attach-session")
          ) {
            discoveries++;
            accepted.write(
              [...sessions]
                .map(
                  (s) => `${s}|2|1|${identity(s).id}|${identity(s).created}\n`,
                )
                .join(""),
            );
            accepted.exit(0);
            accepted.end();
          } else {
            const name = info.command.match(/tt-[a-f0-9]{16}|named-static/);
            if (name) sessions.add(name[0]);
            terminal(accepted);
          }
        });
      }),
    );
  },
);
ssh.listen(0, "127.0.0.1");
await once(ssh, "listening");
const mime = {
  ".html": "text/html",
  ".js": "text/javascript",
  ".mjs": "text/javascript",
  ".css": "text/css",
  ".wasm": "application/wasm",
  ".woff2": "font/woff2",
  ".woff": "font/woff",
};
const requests = [];
const csp = readFileSync("deploy/_headers", "utf8")
  .split("\n")
  .find((line) => line.includes("Content-Security-Policy:"))
  .split("Content-Security-Policy:")[1]
  .trim();
const http = createServer(async (req, res) => {
  requests.push(req.url);
  try {
    const url = new URL(req.url, "http://localhost");
    const name = url.pathname === "/" ? "/index.html" : url.pathname;
    const root = path.resolve("dist-static");
    const file = path.resolve(root, "." + name);
    if (!file.startsWith(root + path.sep)) throw Error();
    const body = await readFile(file);
    res.writeHead(200, {
      "Content-Type": mime[path.extname(file)] || "application/octet-stream",
      "Content-Security-Policy": csp,
    });
    res.end(body);
  } catch {
    res.writeHead(404);
    res.end("Not found");
  }
});
const wss = new WebSocketServer({ server: http, path: "/fixture-ssh" });
const sockets = new Set();
wss.on("connection", (ws) => {
  sockets.add(ws);
  const socket = tcpConnect(ssh.address().port, "127.0.0.1");
  ws.on("message", (data) => socket.write(data));
  socket.on("data", (data) => {
    if (ws.readyState === 1) ws.send(data);
  });
  socket.on("error", () => ws.close());
  socket.on("close", () => ws.close());
  ws.on("close", () => {
    socket.destroy();
    sockets.delete(ws);
  });
});
http.listen(0, "127.0.0.1");
await once(http, "listening");
const origin = `http://127.0.0.1:${http.address().port}`;
let browser, debugPage;
const browserLog = [];
const wait = async (fn) => {
  for (let i = 0; i < 150; i++) {
    if (await fn()) return;
    await new Promise((r) => setTimeout(r, 40));
  }
  throw new Error("Condition timed out.");
};
try {
  browser = await chromium.launch({
    args: [
      "--use-fake-device-for-media-stream",
      "--use-fake-ui-for-media-stream",
    ],
  });
  const context = await browser.newContext({
    viewport: { width: 1440, height: 1100 },
    permissions: ["clipboard-read", "clipboard-write"],
  });
  await context.addInitScript(
    (url) => {
      globalThis.__tailserveTestSocketURL = url;
    },
    origin.replace("http:", "ws:") + "/fixture-ssh",
  );
  await context.route("**/*.wasm.gz", (route) =>
    route.fulfill({
      body: gzipSync(readFileSync(path.resolve(".build/test.wasm"))),
      contentType: "application/gzip",
    }),
  );
  await mockSpeechWorker(context);
  const page = await context.newPage();
  context.setDefaultTimeout(30000);
  debugPage = page;
  const errors = [];
  page.on("pageerror", (e) => errors.push(e.message));
  page.on("console", (m) => browserLog.push(m.text()));
  await page.goto(origin);
  await page.locator("#password").fill("static browser vault passphrase");
  await page.locator("#unlock-button").click();
  await page.locator("#workspace").waitFor();
  assert.equal(
    await page.locator("#add-server, #add-server-bottom, #empty-add").count(),
    0,
  );
  assert.equal(
    await page
      .locator(
        ".sessions-panel, #activity, .remote-panel, .page-footer, #saved-sessions, .sessions-label",
      )
      .count(),
    0,
  );
  for (const name of [
    "Production",
    "Development",
    "Home lab",
    "Build runner",
    "Archive",
  ]) {
    await page.locator("#discover").click();
    await page.locator("[data-peer]").first().click();
    assert.ok(
      await page.locator("#dialog").evaluate((el) => el.matches(":modal")),
    );
    await page.locator("[name=name]").fill(name);
    await page
      .locator("[name=host]")
      .fill(name.replaceAll(" ", "-").toLowerCase() + ".internal");
    await page.locator("[name=port]").fill("2222");
    await page.locator("[name=username]").fill("testuser");
    await page.getByRole("button", { name: "Save server" }).click();
    await page.locator("#dialog").waitFor({ state: "hidden" });
  }
  assert.equal(await page.locator("#launcher-server select").count(), 0);
  assert.equal(await page.locator("[data-launch-server]").count(), 5);
  assert.equal(await page.locator("#backup-reminder").count(), 0);
  await page.locator("[data-launch-server]").first().click();
  assert.equal(await page.locator("#launcher-name").inputValue(), "");
  await page.screenshot({
    path: "static-launcher-preview.png",
    fullPage: true,
  });
  // Exclusive workspace ownership prevents stale snapshots and cloned Tailscale nodes.
  const second = await context.newPage();
  await second.goto(origin);
  await second.locator("#password").fill("static browser vault passphrase");
  await second.locator("#unlock-button").click();
  await second.getByText(/workspace is open in another browser tab/).waitFor();
  await second.close();
  assert.equal(
    authMethods.length,
    0,
    "Background discovery must not probe without credentials",
  );
  await page.locator("#launcher-shell").click();
  await page.locator(".login-prompt [name=password]").fill("wrong-password");
  assert.equal(authMethods.length, 0, "Choose a credential before opening SSH");
  await page.locator(".login-prompt button[type=submit]").click();
  await page.getByRole("button", { name: "Trust & continue" }).click();
  await page
    .getByText(
      "The previous credential was not accepted, or another authentication step is required.",
    )
    .waitFor();
  await page
    .locator(".login-prompt [name=password]")
    .fill("static-ssh-password");
  // Do not remember: background discovery must reuse memory-only credentials.
  await page.locator(".login-prompt button[type=submit]").click();
  await page.waitForFunction(() =>
    document
      .querySelector("#terminal-status")
      .textContent.includes("Connected"),
  );
  await wait(() => discoveries > 0);
  assert.ok(
    await page
      .locator(".terminal-instance:not([hidden])")
      .innerText()
      .then((t) => t.includes("Fixture SSH sign-in message")),
    "SSH authentication banners must be visible",
  );
  const terminal = page.locator(".terminal-instance:not([hidden])");
  await assertTerminalBounds(page);
  const box = await terminal.boundingBox();
  await page.locator("#filter").focus();
  await page.mouse.move(0, 0);
  await page.mouse.move(box.x + 90, box.y + 55);
  await page.keyboard.type("hover-focus+-");
  await wait(() => input.includes("hover-focus+-"));
  const before = input;
  await page.keyboard.press("Control+=");
  await page.keyboard.press("Control+-");
  await page.keyboard.press("Control+0");
  assert.equal(input, before);
  // xterm bracketed paste and multiline confirmation, including a native keyboard paste.
  stream.write("\x1b[?2004h");
  await page.waitForTimeout(100);
  await page.evaluate(() =>
    navigator.clipboard.writeText("clipboard paste 🦊"),
  );
  await page.keyboard.press("Meta+v");
  await wait(() => input.includes("\x1b[200~clipboard paste 🦊\x1b[201~"));
  await page.evaluate(() =>
    navigator.clipboard.writeText("line-one\nline-two"),
  );
  await page.locator("#paste").click();
  await page.getByRole("heading", { name: "Paste multiple lines?" }).waitFor();
  assert.ok(!input.includes("line-one"));
  await page
    .locator(".clipboard-dialog")
    .getByRole("button", { name: "Paste into this session" })
    .click();
  await wait(() => input.includes("line-one\rline-two"));
  // tmux OSC 52 -> system clipboard. Clipboard reads from remote are ignored.
  stream.write(
    "\x1b]52;c;" +
      Buffer.from("tmux copied café 🦊").toString("base64") +
      "\x07",
  );
  await wait(
    async () =>
      (await page.evaluate(() => navigator.clipboard.readText())) ===
      "tmux copied café 🦊",
  );
  const beforeQuery = input;
  stream.write("\x1b]52;c;?\x07");
  await page.waitForTimeout(100);
  assert.equal(input, beforeQuery);
  // Browser denies automatic writes: keep a per-session copy affordance.
  await page.evaluate(() => {
    globalThis.originalClipboardWrite = navigator.clipboard.writeText.bind(
      navigator.clipboard,
    );
    navigator.clipboard.writeText = () => Promise.reject(new Error("denied"));
  });
  stream.write(
    "\x1b]52;c;" + Buffer.from("manual tmux copy").toString("base64") + "\x07",
  );
  await page.getByRole("button", { name: "Copy terminal text" }).waitFor();
  await page.waitForFunction(() =>
    document.querySelector("#copy").textContent.includes("tmux selection"),
  );
  await page.evaluate(() => {
    navigator.clipboard.writeText = globalThis.originalClipboardWrite;
  });
  await page.locator("#copy").click();
  assert.equal(
    await page.evaluate(() => navigator.clipboard.readText()),
    "manual tmux copy",
  );
  // The same groups must work with the Go WASM transport, without reconnecting on merge.
  for (let i = 0; i < 2; i++) {
    await page.locator("#new-tab").click();
    await page.locator("#launcher-shell").click();
    await page.waitForFunction(() =>
      document
        .querySelector("#terminal-status")
        .textContent.includes("Connected"),
    );
  }
  await exercisePaneGroups(page, () => input);
  while ((await page.locator("[data-close]").count()) > 1) {
    await page.locator("#tabs .tab").last().hover();
    await page.locator("[data-close]").last().click();
  }
  // Themes/fonts/cursor affect existing terminals and persist on reload.
  await page.locator("#appearance").click();
  await page.locator("[data-theme-choice=tokyo]").click();
  await page.locator("[data-font-choice=jetbrains]").click();
  await page.locator("#appearance-larger").click();
  await page.locator("[data-cursor=bar]").click();
  assert.equal(await page.locator("html").getAttribute("data-theme"), "tokyo");
  assert.match(
    await terminal
      .locator(".xterm-rows")
      .evaluate((el) => getComputedStyle(el).fontFamily),
    /JetBrains Mono/,
  );
  await page.screenshot({ path: "appearance-preview.png" });
  await page.locator("#dialog-close").click();
  // Styled tooltips work with keyboard focus and remain inside the viewport.
  await page.locator("#font-up").focus();
  await page.locator("#workspace-tooltip:popover-open").waitFor();
  const tip = await page.locator("#workspace-tooltip").boundingBox();
  assert.ok(tip.x >= 0 && tip.x + tip.width <= 1440);
  await page.screenshot({ path: "tooltips-preview.png" });
  await page.keyboard.press("Escape");
  await page.locator("#new-tab").click();
  await page.locator("#fullscreen").click();
  await page.waitForFunction(() => !!document.fullscreenElement);
  await page.locator("#launcher-discover").click();
  assert.ok(
    await page
      .locator("#dialog")
      .evaluate(
        (el) => el.matches(":modal") && document.fullscreenElement.contains(el),
      ),
  );
  await page.locator("#dialog-close").click();
  await page.locator("#start-session").click();
  await page.waitForFunction(() =>
    document.querySelector(".tab.active").textContent.includes("tt-"),
  );
  await wait(() => sessions.size === 2);
  await page.waitForFunction(
    () =>
      !document.querySelector(".tab.active").textContent.includes("unverified"),
  );
  const autoName = [...sessions].find((s) => s.startsWith("tt-"));
  assert.match(autoName, /^tt-[a-f0-9]{16}$/);
  await page.locator(".tab.active [data-close]").click();
  await page.locator("#new-tab").click();
  await page.locator("[data-resume]").filter({ hasText: autoName }).click();
  await page.waitForFunction(() =>
    document
      .querySelector("#terminal-status")
      .textContent.includes("Connected"),
  );
  await page.locator("#new-tab").click();
  await page.locator("#launcher-name").fill("named-static");
  await page.locator("#start-session").click();
  await wait(() => sessions.has("named-static"));
  await page.waitForFunction(
    () =>
      !document.querySelector(".tab.active").textContent.includes("unverified"),
  );
  const renamedTabId = await page
    .locator(".tab.active [data-tab]")
    .getAttribute("data-tab");
  await page.locator(".tab.active [data-session-menu]").click();
  await page.locator("#rename-tab-session").click();
  assert.equal(
    await page.locator("#rename-session-name").inputValue(),
    "named-static",
  );
  await page.locator("#rename-session-name").fill("bad name");
  await page.locator("#rename-session-submit").click();
  await page.locator("#rename-session-error").waitFor({ state: "visible" });
  assert.ok(sessions.has("named-static"));
  await page.locator("#rename-session-cancel").click();
  await page.locator(".tab.active [data-session-menu]").click();
  await page.locator("#rename-tab-session").click();
  await page.locator("#rename-session-name").fill("main");
  await page.locator("#rename-session-submit").click();
  await page.locator("#rename-session-error").waitFor({ state: "visible" });
  assert.ok(sessions.has("named-static"));
  await page.locator("#rename-session-name").fill("renamed-static");
  await page.screenshot({ path: "/tmp/tailterm-session-rename.png" });
  await page.locator("#rename-session-submit").click();
  await page.locator("#dialog").waitFor({ state: "hidden" });
  assert.ok(sessions.has("renamed-static"));
  assert.equal(
    await page.locator(".tab.active [data-tab]").getAttribute("data-tab"),
    renamedTabId,
  );
  assert.match(await page.locator(".tab.active").innerText(), /renamed-static/);
  await page.keyboard.type("after-rename-probe");
  await wait(() => input.includes("after-rename-probe"));
  await page.locator("#new-tab").click();
  await page
    .getByRole("button", { name: "Rename session renamed-static", exact: true })
    .click();
  await page.locator("#rename-session-name").fill("renamed-static-launcher");
  await page.locator("#rename-session-submit").click();
  await page.locator("#dialog").waitFor({ state: "hidden" });
  await page
    .locator("[data-resume]")
    .filter({ hasText: "renamed-static-launcher" })
    .waitFor();
  await page.locator(`[data-tab="${renamedTabId}"]`).click();
  assert.match(
    await page.locator(".tab.active").innerText(),
    /renamed-static-launcher/,
  );
  await exerciseWorkspaceControls(page);
  await exercisePopupReview(page);
  await exerciseVoiceDictation(page, () => input);
  await exerciseImageUpload(page, uploadedFiles, () => input, uploadControl);
  await page.locator("#connection-diagnostics").click();
  await page.getByText("SSH connection", { exact: true }).count();
  assert.match(
    await page.locator("#diagnostic-facts").innerText(),
    /renamed-static-launcher/,
  );
  await page.locator("#dialog-close").click();
  const outputDownload = page.waitForEvent("download");
  await page.locator("#download-scrollback").click();
  assert.match(
    await readFile(await (await outputDownload).path(), "utf8"),
    /after-rename-probe/,
  );
  await exerciseLocalHistory(page, () => input);
  await exerciseWorkspaceActions(page, stream);
  await exerciseWorkspaceContinuity(page, context, () => terminalStarts);
  console.log(
    "Uploads, reconnect, reordering and workspace restoration passed.",
  );
  await page.evaluate(
    () => document.fullscreenElement && document.exitFullscreen(),
  );
  // Load a real key via Go's key parser and authenticate with it.
  await page.locator("#keys").click();
  await page.locator("#import-key").evaluate((el) => (el.open = true));
  await page.locator("#key-form [name=name]").fill("Fixture key");
  await page.locator("#key-form [name=privateKey]").fill(loginKey);
  await page.locator("#key-form [name=passphrase]").fill(keyPassphrase);
  await page.getByRole("button", { name: "Encrypt & save key" }).click();
  await page.getByText("Fixture key", { exact: true }).waitFor();
  await page.locator("[data-copy-public-key]").click();
  await page.waitForFunction(() =>
    document
      .querySelector("#public-key-text")
      ?.value.startsWith("ssh-ed25519 "),
  );
  assert.deepEqual(
    ssh2.utils
      .parseKey(await page.locator("#public-key-text").inputValue())
      .getPublicSSH(),
    parsedLogin.getPublicSSH(),
  );
  generatedLogin = await exerciseGeneratedKey(page);
  await page.locator("#dialog-close").click();
  await page.locator("#discover").click();
  await page.locator("[data-peer]").first().click();
  assert.ok(
    await page.locator("#dialog").evaluate((el) => el.matches(":modal")),
  );
  await page.locator("[name=name]").fill("Key host");
  await page.locator("[name=host]").fill("fixture.internal");
  await page.locator("[name=port]").fill("22022");
  await page.locator("[name=username]").fill("keyuser");
  await page
    .locator("[name=keyId]")
    .selectOption({ label: "Browser-generated key" });
  await page.getByRole("button", { name: "Save server" }).click();
  await page.locator("#launcher-shell").click();
  await page.getByRole("button", { name: "Trust & continue" }).click();
  await page.waitForFunction(() =>
    document
      .querySelector("#terminal-status")
      .textContent.includes("Connected"),
  );
  assert.ok(authMethods.includes("publickey"));
  const profileCount = await page.locator("#server-list [data-server]").count();
  await page.locator("#discover").click();
  await page.locator("#discovery-filter").fill("nothing-matches");
  assert.equal(await page.locator("[data-peer]").count(), 0);
  await page.locator("#discovery-filter").fill("fixture");
  await page.screenshot({ path: "discovery-modal-preview.png" });
  await page.locator("[data-peer]").first().click();
  assert.ok(
    await page.locator("#dialog").evaluate((el) => el.matches(":modal")),
  );
  assert.equal(
    await page.locator("#delete-server").count(),
    1,
    "Discovery edits an existing device instead of duplicating it",
  );
  await page.locator("#dialog-close").click();
  assert.equal(
    await page.locator("#server-list [data-server]").count(),
    profileCount,
  );
  // A server closing during authentication must preserve its banner and explain EOF.
  await page.locator(".tab.active [data-close]").click();
  await page.locator("#all-servers").click();
  await page.locator(".server-item").filter({ hasText: "Key host" }).click();
  await page.locator("#commands").click();
  await page.locator("#command-query").fill("Edit selected server");
  await page
    .locator("#command-results button")
    .filter({ hasText: "Edit selected server" })
    .click();
  await page.locator("[name=username]").fill("closeduser");
  await page.getByRole("button", { name: "Save server" }).click();
  await page.locator("#launcher-shell").click();
  await page.getByRole("button", { name: "Trust & continue" }).click();
  await page.waitForFunction(() =>
    document.querySelector("#terminal-status").textContent.includes("Error"),
  );
  assert.ok(
    (
      await page.locator(".terminal-instance:not([hidden])").innerText()
    ).includes("closed the connection during SSH sign-in"),
  );
  assert.equal(
    await page.locator(".login-prompt").count(),
    0,
    "EOF must not trigger credential retries",
  );
  // Changing the pinned fingerprint fails closed, even with a working credential.
  await page.locator(".tab.active [data-close]").click();
  await page.locator("#all-servers").click();
  await page.locator(".server-item").filter({ hasText: "Key host" }).click();
  await page.locator("#commands").click();
  await page.locator("#command-query").fill("Edit selected server");
  await page
    .locator("#command-results button")
    .filter({ hasText: "Edit selected server" })
    .click();
  await page.locator("[name=username]").fill("keyuser");
  await page.locator("#server-advanced").evaluate((el) => (el.open = true));
  await page.locator("[name=fingerprint]").fill("SHA256:" + "A".repeat(43));
  await page.getByRole("button", { name: "Save server" }).click();
  await page.locator("#launcher-shell").click();
  await page.waitForFunction(() =>
    document.querySelector("#terminal-status").textContent.includes("Error"),
  );
  assert.equal(await page.locator(".login-prompt").count(), 0);
  await page.locator("#all-servers").click();
  // Disk contains ciphertext only; backup excludes device state after decrypting in a unit test.
  const record = await page.evaluate(
    () =>
      new Promise((resolve, reject) => {
        const r = indexedDB.open("tailserve", 1);
        r.onsuccess = () => {
          const tx = r.result.transaction("vault");
          const q = tx.objectStore("vault").get("encrypted");
          q.onsuccess = () => resolve(q.result);
          q.onerror = reject;
        };
      }),
  );
  assert.equal(record.format, "tailserve-vault");
  assert.doesNotMatch(
    JSON.stringify(record),
    /static-ssh-password|PRIVATE KEY|fixture-node/,
  );
  await page.locator("#backup-vault").click();
  const download = page.waitForEvent("download");
  await page.locator("#export-backup").click();
  const file = await download;
  await page.waitForFunction(() =>
    document
      .querySelector("#backup-vault")
      .dataset.tooltip?.startsWith("Last backup:"),
  );
  const backup = JSON.parse(await readFile(await file.path(), "utf8"));
  assert.equal(backup.format, "tailserve-vault");
  const savedBackup = await openVault(
    backup,
    "static browser vault passphrase",
  );
  assert.ok(
    savedBackup.data.sessions.some((s) => s.name === "renamed-static-launcher"),
  );
  assert.ok(
    !savedBackup.data.sessions.some((s) =>
      ["named-static", "renamed-static"].includes(s.name),
    ),
  );
  await page.locator("#dialog-close").click();
  await page.locator("#lock").click();
  await page.locator("#lockscreen").waitFor();
  await page.locator("#password").fill("static browser vault passphrase");
  await page.locator("#unlock-button").click();
  await page.locator("#workspace").waitFor();
  assert.equal(await page.locator("html").getAttribute("data-theme"), "tokyo");
  await finishRestoration(page);
  console.log("Backup and lock/unlock restoration passed.");
  assert.equal(await page.locator("[data-launch-server]").count(), 6);
  await exerciseServerFilters(page);
  await page.setViewportSize({ width: 390, height: 844 });
  assert.ok(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= innerWidth,
    ),
  );
  await page.screenshot({ path: "static-mobile-preview.png", fullPage: true });
  assert.ok(
    !requests.some((url) => url.startsWith("/api") || url === "/terminal"),
  );
  assert.deepEqual(errors, []);
  const productionContext = await browser.newContext();
  await exerciseVaultReset(page, context, origin);
  await exerciseForgetDevice(page);
  const productionPage = await productionContext.newPage();
  productionPage.on("pageerror", (e) => errors.push(e.message));
  await productionPage.goto(origin);
  await productionPage
    .locator("#password")
    .fill("production startup passphrase");
  await productionPage.locator("#unlock-button").click();
  await productionPage.locator("#workspace").waitFor();
  await productionPage.locator("#tailscale-login").click();
  await productionPage.waitForFunction(
    () =>
      /Sign in to Tailscale|Starting|Tailscale connected|NeedsMachineAuth/.test(
        document.querySelector("#tail-status")?.textContent,
      ) ||
      document
        .querySelector("#dialog")
        ?.textContent.includes("Sign in to Tailscale"),
  );
  assert.deepEqual(errors, []);
  await productionContext.close();
  console.log(
    "Static browser checks passed: local encrypted vault, exclusive tabs, server cards, Go WASM password/key SSH, strict fingerprints, memory-only auth discovery, hover focus, font keys, bracketed/native/multiline paste, OSC52 and denied clipboard fallback, themes/fonts, tooltips, fullscreen create/resume, backups, lock/reopen, mobile, zero application API requests.",
  );
} catch (error) {
  console.error(browserLog.slice(-45).join("\n"));
  console.error(
    await debugPage?.evaluate(() =>
      Object.fromEntries(
        [
          "#launcher-note",
          "#upload-note",
          "#upload-files",
          "#notice",
          "#terminal-status",
          "#launcher-sessions",
          ".terminal-instance:not([hidden]) .xterm-rows",
        ].map((s) => [s, document.querySelector(s)?.textContent]),
      ),
    ),
  );
  throw error;
} finally {
  await browser?.close();
  for (const ws of sockets) ws.terminate();
  wss.close();
  http.close();
  ssh.close();
}
