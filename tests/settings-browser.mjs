// TailOS Settings with the Discord section (wi_dc04ac6a9ccf7402, order
// #53689). The real modes, Settings view, confirm dialog and hub client run
// in Chromium and WebKit against a fake hub served by this script; the hub's
// own routes are covered by hub/internal/server/bridge_settings_test.go.
// Disposable browser contexts only: no live hub, bridge, bot or token.
import assert from "node:assert/strict";
import { mkdir } from "node:fs/promises";
import path from "node:path";
import { chromium, webkit } from "@playwright/test";
import { createServer } from "vite";
import { viteCacheDir } from "./vite-cache.mjs";

const root = process.cwd();
const artifacts = path.join(root, ".build/settings");
await mkdir(artifacts, { recursive: true });
const which = process.env.TEST_BROWSER || "both";
const engines = {
  chromium: [chromium],
  webkit: [webkit],
  both: [chromium, webkit],
}[which];
if (!engines) throw new Error("Unknown TEST_BROWSER");

// Fixture secrets the hub and page must never hold.
const BRIDGE_TOKEN = "fixture-bridge-token-0123456789abcdef";
const DISCORD_TOKEN = "fixture-discord-bot-token-0123456789";
const GUILD_A = "100000000000000001",
  GUILD_B = "200000000000000002",
  OWNER = "900000000000000009",
  OWNER_2 = "900000000000000010",
  HELPER = "tsk_0123456789abcdef";

// The fake hub: the api.BridgeSettingsView contract, a revision check and the
// hub's validation messages.
let hub;
function resetHub() {
  hub = {
    settings: null,
    revision: 0,
    updatedAt: null,
    report: null,
    puts: [],
    gets: 0,
  };
}
const SNOWFLAKE = /^[0-9]{1,20}$/;
function validate(s) {
  if (!SNOWFLAKE.test(s.guildId || ""))
    return "guildId must be a Discord server ID (digits only)";
  const owners = [...new Set((s.ownerIds || []).filter(Boolean))];
  if (!owners.length || owners.some((id) => !SNOWFLAKE.test(id)))
    return "ownerIds needs 1 to 25 Discord user IDs";
  if (s.helperTask && !/^tsk_[0-9a-f]{16}$/.test(s.helperTask))
    return "helperTask must be empty or a project ID (tsk_…)";
  if (
    s.helperChannelId &&
    (!SNOWFLAKE.test(s.helperChannelId) || !s.helperTask)
  )
    return "helperChannelId needs helperTask";
  if (s.tailosUrl && !/^https:\/\/[^/@]+/.test(s.tailosUrl))
    return "tailosUrl must be empty or an absolute https:// URL";
  return "";
}
function view() {
  return {
    settings: hub.settings,
    revision: hub.revision,
    ...(hub.updatedAt
      ? {
          updatedAt: hub.updatedAt,
          updatedBy: { node: "workspace", user: "owner" },
        }
      : {}),
    bridge: hub.report,
    secrets: hub.report
      ? { bridgeToken: "configured", discordToken: "configured-on-bridge-host" }
      : { bridgeToken: "not-confirmed", discordToken: "not-confirmed" },
  };
}
function report(r) {
  hub.report = {
    reportedAt: new Date().toISOString(),
    applied: 0,
    refused: 0,
    effective: {
      guildId: "",
      ownerIds: [],
      helperTask: "",
      helperChannelId: "",
      tailosUrl: "",
    },
    ...r,
  };
}
function save(body) {
  const known = new Set(["settings", "expectedRevision"]);
  const fields = new Set([
    "guildId",
    "ownerIds",
    "helperTask",
    "helperChannelId",
    "tailosUrl",
  ]);
  if (
    Object.keys(body).some((k) => !known.has(k)) ||
    Object.keys(body.settings || {}).some((k) => !fields.has(k))
  )
    return [
      400,
      {
        error:
          "invalid request: the body must be {settings, expectedRevision} with only the known non-secret fields",
      },
    ];
  if (body.expectedRevision !== hub.revision)
    return [
      409,
      {
        error: `conflict: the bridge settings changed elsewhere (saved revision ${hub.revision}, expected ${body.expectedRevision}); reload and try again`,
      },
    ];
  const problem = validate(body.settings);
  if (problem) return [400, { error: "invalid request: " + problem }];
  hub.settings = {
    ...body.settings,
    ownerIds: [...new Set(body.settings.ownerIds)],
  };
  hub.revision++;
  hub.updatedAt = new Date().toISOString();
  return [200, view()];
}

const html = `<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><link rel="stylesheet" href="/client/style.css"><link rel="stylesheet" href="/client/settings.css"></head><body><div id="app"><div id="workspace"><aside><div class="sidebar-bottom"><button id="keys"><span class="nav-label">SSH keys</span></button><button id="lock"><span class="nav-label">Lock vault</span></button></div></aside><main><header><div class="header-right"></div></header><section class="terminal-shell"><p>terminals</p></section></main></div></div><script type="module">
import { setupModes, MODES } from "/client/modes.js";
import { createSettingsView, addSettingsNav, SECTIONS } from "/client/settings-view.js";
import { confirmDialog } from "/client/confirm-dialog.js";
import { createHubClient } from "/client/hub-client.js";
window.requests = [];
window.errors = [];
const hub = createHubClient({ baseURL: location.origin, fetchImpl: async (url, init) => {
  window.requests.push({ url, method: init.method, headers: { ...init.headers }, body: init.body || "" });
  const response = await fetch(url, init), text = await response.text();
  return { status: response.status, statusText: response.statusText, headers: response.headers, text: async () => text };
}});
window.connected = true;
const settings = createSettingsView({ client: () => (window.connected ? hub : null), configure: () => (window.configured = true), confirm: confirmDialog });
// The same shape as client/main.js: header tabs, the nav entry before Lock,
// and one mode area every view renders into.
const modes = setupModes({ header: document.querySelector("main > header"), main: document.querySelector("main"), onChange: (mode, view) => {
  settings.hide();
  view.replaceChildren();
  if (mode === "settings") { settings.mount(view); window.shown = settings.show(); }
  else if (mode !== "terminals") view.textContent = mode + " view";
}});
addSettingsNav(document.querySelector(".sidebar-bottom"), () => modes.set("settings"));
window.modes = modes;
window.modeIds = MODES.map(([id]) => id);
window.sectionIds = SECTIONS.map((s) => s.id);
window.ready = true;
</script></body></html>`;

const vite = await createServer({
  configFile: false,
  cacheDir: viteCacheDir(import.meta.url),
  root,
  logLevel: "silent",
  server: { host: "127.0.0.1", port: 0 },
  plugins: [
    {
      name: "settings-fixture",
      configureServer(s) {
        s.middlewares.use("/settings-test", (_req, res) => {
          res.setHeader("Content-Type", "text/html");
          res.end(html);
        });
        s.middlewares.use("/v1/bridge/settings", async (req, res) => {
          let raw = "";
          for await (const chunk of req) raw += chunk;
          let status = 200,
            out;
          if (req.method === "GET") {
            hub.gets++;
            out = view();
          } else if (req.method === "PUT") {
            let body = {};
            try {
              body = JSON.parse(raw);
            } catch {
              body = { bad: true };
            }
            hub.puts.push(body);
            [status, out] = save(body);
          } else [status, out] = [405, { error: "method not allowed" }];
          res.statusCode = status;
          res.setHeader("Content-Type", "application/json");
          res.end(JSON.stringify(out));
        });
      },
    },
  ],
});
await vite.listen();
const origin = "http://127.0.0.1:" + vite.httpServer.address().port;

const transparent = (color) =>
  color === "transparent" || /rgba\(.*,\s*0\)$/.test(color);

try {
  for (const engine of engines) {
    resetHub();
    const browser = await engine.launch();
    try {
      const context = await browser.newContext({
        viewport: { width: 1280, height: 900 },
      });
      await context.route("**/*", (r) =>
        new URL(r.request().url()).origin === origin ? r.continue() : r.abort(),
      );
      const page = await context.newPage();
      const errors = [];
      page.on("pageerror", (e) => errors.push(e.message));
      page.setDefaultTimeout(7000);
      await page.goto(origin + "/settings-test");
      await page.waitForFunction(() => window.ready);
      const nav = page.locator("#settings-nav");
      const status = page.locator("[data-settings-status]");
      const source = page.locator("[data-bridge-source]");
      const saveButton = page.locator("[data-settings-save]");
      const field = (key) => page.locator(`[name="${key}"]`);
      // Each reload re-renders the section; callers wait for the content.
      const reload = () => page.locator("[data-settings-reload]").click();

      // a1: the left-nav entry opens Settings; no header tab is selected;
      // a header tab leaves it; Files stays hidden.
      assert.equal(
        await page.locator("header [data-mode=files]").count(),
        0,
        "Files stays hidden",
      );
      assert.ok(!(await page.evaluate(() => window.modeIds)).includes("files"));
      assert.deepEqual(
        await page
          .locator(".sidebar-bottom > button")
          .evaluateAll((b) => b.map((x) => x.id)),
        ["keys", "settings-nav", "lock"],
      );
      assert.equal(await nav.getAttribute("aria-pressed"), "false");
      // Hover is an outline, not a fill, while not selected.
      await nav.hover();
      const hover = await nav.evaluate((el) => {
        const s = getComputedStyle(el);
        return { bg: s.backgroundColor, shadow: s.boxShadow };
      });
      assert.ok(transparent(hover.bg), `hover fill ${hover.bg}`);
      assert.match(hover.shadow, /inset/, `hover outline ${hover.shadow}`);
      await nav.click();
      await page.locator(".settings-view h2").waitFor();
      assert.equal(await nav.getAttribute("aria-pressed"), "true");
      assert.equal(
        await page.evaluate(() => document.body.dataset.mode),
        "settings",
      );
      assert.equal(await page.locator("#mode-view").isVisible(), true);
      assert.equal(await page.locator(".terminal-shell").isVisible(), false);
      assert.deepEqual(
        await page
          .locator("header [data-mode]")
          .evaluateAll((b) => b.map((x) => x.getAttribute("aria-pressed"))),
        Array(await page.locator("header [data-mode]").count()).fill("false"),
        "no header tab is selected",
      );
      await page.mouse.move(700, 700);
      const fill = await nav.evaluate(
        (el) => getComputedStyle(el).backgroundColor,
      );
      assert.ok(!transparent(fill), `selection fill ${fill}`);
      await page.locator('header [data-mode="board"]').click();
      await page.getByText("board view").waitFor();
      assert.equal(await nav.getAttribute("aria-pressed"), "false");
      assert.equal(
        await page
          .locator('header [data-mode="board"]')
          .getAttribute("aria-pressed"),
        "true",
      );
      await nav.click();
      await page.locator("[data-discord-form]").waitFor();

      // a8: titled sections from one registry, Discord first.
      assert.deepEqual(await page.evaluate(() => window.sectionIds), [
        "discord",
      ]);
      assert.deepEqual(
        await page.locator(".settings-section > h3").allInnerTexts(),
        ["Discord"],
      );
      assert.deepEqual(
        await page
          .locator(".settings-section")
          .evaluateAll((s) => s.map((x) => x.dataset.settingsSection)),
        ["discord"],
      );

      // a2/a3: nothing reported yet; secrets are status words, never inputs.
      assert.match(
        await source.innerText(),
        /not reported since the hub started/,
      );
      assert.equal(
        await page.locator('[data-secret="bridgeToken"]').innerText(),
        "Not confirmed",
      );
      assert.equal(
        await page.locator('[data-secret="discordToken"]').innerText(),
        "Not confirmed",
      );
      assert.deepEqual(
        await page
          .locator(".settings-view input")
          .evaluateAll((i) => i.map((x) => x.name)),
        ["guildId", "ownerIds", "helperTask", "helperChannelId", "tailosUrl"],
      );
      assert.equal(
        await page.locator('.settings-view input[type="password"]').count(),
        0,
      );

      // The bridge reports deployment values: the fields start from them.
      report({
        source: "deployment",
        effective: {
          guildId: GUILD_A,
          ownerIds: [OWNER],
          helperTask: "",
          helperChannelId: "",
          tailosUrl: "https://tailos.example",
        },
      });
      await reload();
      await page.waitForFunction(() =>
        document
          .querySelector("[data-bridge-source]")
          ?.textContent.includes("deployment values"),
      );
      assert.equal(
        await page.locator('[data-secret="bridgeToken"]').innerText(),
        "Configured",
      );
      assert.equal(
        await page.locator('[data-secret="discordToken"]').innerText(),
        "Configured on the bridge host",
      );
      assert.equal(
        await page.locator('[data-effective="guildId"]').innerText(),
        GUILD_A,
      );
      assert.equal(
        await page.locator('[data-effective="helperTask"]').innerText(),
        "not set",
      );
      assert.equal(await field("guildId").inputValue(), GUILD_A);
      assert.match(
        await page.locator("[data-settings-saved]").innerText(),
        /Not saved yet/,
      );

      // a10: one control height, muted placeholders, no decorative filler.
      const ui = await page.evaluate(() => {
        const heights = [
          ...document.querySelectorAll(
            ".settings-view input, .settings-actions button",
          ),
        ].map((el) => Math.round(el.getBoundingClientRect().height));
        // The app's muted placeholder color, measured from a probe.
        const probe = document.createElement("span");
        probe.style.color = "var(--placeholder)";
        document.querySelector(".settings-view").append(probe);
        const muted = getComputedStyle(probe).color;
        probe.remove();
        const helper = document.querySelector('[name="helperChannelId"]');
        return {
          heights,
          placeholder: getComputedStyle(helper, "::placeholder").color,
          text: getComputedStyle(helper).color,
          muted,
          empties: [...document.querySelectorAll(".settings-view *")].filter(
            (el) =>
              !el.children.length &&
              !el.textContent.trim() &&
              !["INPUT", "BR", "svg", "path"].includes(el.tagName) &&
              !el.matches("[data-settings-status], [data-settings-body]") &&
              el.getBoundingClientRect().height > 0,
          ).length,
        };
      });
      assert.equal(
        new Set(ui.heights).size,
        1,
        `control heights ${ui.heights}`,
      );
      assert.equal(
        ui.placeholder,
        ui.muted,
        "placeholders use the muted color",
      );
      assert.notEqual(ui.placeholder, ui.text);
      assert.equal(ui.empties, 0, "no empty decorative elements");

      // a2: changing the guild asks once; Cancel saves nothing.
      await field("guildId").fill(GUILD_B);
      await saveButton.click();
      const dialog = page.locator(".confirmation-dialog");
      await dialog.waitFor();
      assert.match(
        await dialog.innerText(),
        /Move the bridge to another Discord server\?/,
      );
      await dialog.locator("[data-cancel]").click();
      await dialog.waitFor({ state: "detached" });
      assert.equal(hub.puts.length, 0, "Cancel saved nothing");
      await saveButton.click();
      await dialog.waitFor();
      await dialog.locator("[data-confirm]").click();
      await page.waitForFunction(() =>
        document
          .querySelector("[data-settings-status]")
          ?.textContent.includes("Saved revision 1"),
      );
      assert.equal(hub.puts.length, 1);
      assert.equal(hub.revision, 1);
      assert.equal(hub.settings.guildId, GUILD_B);

      // Other fields never ask.
      await field("ownerIds").fill(`${OWNER}, ${OWNER_2}`);
      await field("helperTask").fill(HELPER);
      await saveButton.click();
      await page.waitForFunction(() =>
        document
          .querySelector("[data-settings-status]")
          ?.textContent.includes("Saved revision 2"),
      );
      assert.equal(await page.locator(".confirmation-dialog").count(), 0);
      assert.deepEqual(hub.settings.ownerIds, [OWNER, OWNER_2]);
      assert.deepEqual(hub.puts[1], {
        settings: {
          guildId: GUILD_B,
          ownerIds: [OWNER, OWNER_2],
          helperTask: HELPER,
          helperChannelId: "",
          tailosUrl: "https://tailos.example",
        },
        expectedRevision: 1,
      });
      // Unchanged values: Save is off.
      assert.equal(await saveButton.isDisabled(), true);

      // a2: an invalid value shows the hub's error and saves nothing.
      await field("tailosUrl").fill("http://tailos.example");
      await saveButton.click();
      await page.waitForFunction(
        () =>
          document.querySelector("[data-settings-status]")?.dataset.kind ===
          "error",
      );
      assert.match(
        await status.innerText(),
        /tailosUrl must be empty or an absolute https:\/\/ URL.*Nothing was saved/,
      );
      assert.equal(hub.revision, 2);

      // a2: a stale save says the settings changed elsewhere and offers reload.
      save({ settings: { ...hub.settings }, expectedRevision: 2 });
      await field("tailosUrl").fill("https://tailos.example/next");
      await saveButton.click();
      await page.waitForFunction(() =>
        document
          .querySelector("[data-settings-status]")
          ?.textContent.includes("changed elsewhere"),
      );
      assert.match(
        await status.innerText(),
        /Reload to see the saved values; nothing was saved/,
      );
      assert.equal(await saveButton.isDisabled(), true);
      assert.equal(hub.revision, 3);
      await reload();
      await page.waitForFunction(() =>
        document
          .querySelector("[data-settings-saved]")
          ?.textContent.includes("Saved revision 3"),
      );
      assert.equal(
        await field("tailosUrl").inputValue(),
        "https://tailos.example",
      );

      // a2: the bridge's source line for hub, refused, last-good and parked.
      report({ source: "hub", applied: 3, effective: { ...hub.settings } });
      await reload();
      await page.waitForFunction(() =>
        document
          .querySelector("[data-bridge-source]")
          ?.textContent.includes("hub revision 3"),
      );
      assert.equal(
        await page.locator('[data-effective="ownerIds"]').innerText(),
        `${OWNER}, ${OWNER_2}`,
      );
      report({
        source: "deployment",
        refused: 3,
        reason: "startup-failed",
        effective: {
          guildId: GUILD_A,
          ownerIds: [OWNER],
          helperTask: "",
          helperChannelId: "",
          tailosUrl: "",
        },
      });
      await reload();
      await page.waitForFunction(() =>
        document
          .querySelector("[data-bridge-source]")
          ?.textContent.includes("refused revision 3"),
      );
      assert.match(
        await source.innerText(),
        /Bridge is using: deployment values — refused revision 3: the bridge failed to start on it/,
      );
      assert.equal(await saveButton.innerText(), "Save again to retry");
      assert.equal(await saveButton.isDisabled(), false);
      await saveButton.click();
      await page.waitForFunction(() =>
        document
          .querySelector("[data-settings-status]")
          ?.textContent.includes("Saved revision 4"),
      );
      assert.equal(
        await page.locator(".confirmation-dialog").count(),
        0,
        "a retry with the same guild does not ask",
      );
      report({
        source: "last-good",
        applied: 2,
        refused: 4,
        reason: "guild-change",
        effective: { ...hub.settings },
      });
      await reload();
      await page.waitForFunction(() =>
        document
          .querySelector("[data-bridge-source]")
          ?.textContent.includes("last good"),
      );
      assert.match(
        await source.innerText(),
        /last good saved settings \(revision 2\) — refused revision 4: it names another Discord server/,
      );
      report({ source: "none", refused: 4, reason: "guild-change" });
      await reload();
      await page.waitForFunction(() =>
        document
          .querySelector("[data-bridge-source]")
          ?.textContent.includes("not connected"),
      );
      assert.match(
        await source.innerText(),
        /Bridge is not connected: the guild changed; save the bound guild again or re-set up \(move the bridge state dir aside\)/,
      );
      assert.equal(
        await page.locator('[data-effective="guildId"]').innerText(),
        "not set",
      );
      await page.screenshot({
        path: path.join(artifacts, `settings-${engine.name()}.png`),
        fullPage: true,
      });

      // An older hub without the route, and no hub at all.
      await page.route("**/v1/bridge/settings", (r) =>
        r.fulfill({
          status: 404,
          contentType: "application/json",
          body: '{"error":"not found"}',
        }),
      );
      await reload();
      await page.getByText("This hub has no bridge settings yet").waitFor();
      await page.unroute("**/v1/bridge/settings");
      await page.evaluate(() => (window.connected = false));
      await page.locator('header [data-mode="board"]').click();
      await nav.click();
      await page.locator("[data-settings-configure]").click();
      assert.equal(await page.evaluate(() => window.configured), true);
      await page.evaluate(() => (window.connected = true));

      // a3: no request the view sent holds a token, a token field or a
      // credential header, and the page never shows one.
      const requests = await page.evaluate(() => window.requests);
      assert.ok(requests.length >= 10, `captured ${requests.length} requests`);
      for (const r of requests) {
        const all = JSON.stringify(r);
        assert.ok(
          !all.includes(BRIDGE_TOKEN) && !all.includes(DISCORD_TOKEN),
          "a request holds a token",
        );
        assert.ok(
          !Object.keys(r.headers).some((h) => /authorization/i.test(h)),
          "a request holds a credential header",
        );
        assert.ok(
          !/token/i.test(r.body),
          `a request body names a token: ${r.body}`,
        );
        assert.ok(
          !/token/i.test(new URL(r.url).search),
          `a request query names a token: ${r.url}`,
        );
      }
      const pageText = await page.evaluate(
        () => document.documentElement.outerHTML,
      );
      assert.ok(
        !pageText.includes(BRIDGE_TOKEN) && !pageText.includes(DISCORD_TOKEN),
      );

      // Narrow screens keep the section within the mode area's width (the
      // fixture page lacks the app's phone layout, so only width is measured).
      await page.locator('header [data-mode="board"]').click();
      await nav.click();
      await page.locator("[data-discord-form]").waitFor();
      await page.setViewportSize({ width: 390, height: 844 });
      const width = await page.evaluate(() => ({
        form: document.querySelector("[data-discord-form]").scrollWidth,
        view: document.querySelector("#mode-view").clientWidth,
      }));
      assert.ok(width.form <= width.view + 1, JSON.stringify(width));

      assert.deepEqual(errors, []);
      await context.close();
      console.log(
        `${engine.name()}: Settings nav, Discord section, save, confirm, refusal and token checks pass`,
      );
    } finally {
      await browser.close();
    }
  }
} finally {
  await vite.close();
}
