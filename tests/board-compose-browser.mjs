// Acceptance for wi_fc695f0cbc3f7a42, build order #820.
// Real Chromium/WebKit and a disposable compiled hub/database; composition and
// repeat flags are synthetic DOM events, not claims about native OS IME input.
import { chromium, webkit } from "@playwright/test";
import { createServer } from "node:http";
import { once } from "node:events";
import { spawn, execFile } from "node:child_process";
import { promisify } from "node:util";
import { mkdtemp, readFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import assert from "node:assert/strict";

const exec = promisify(execFile);
const root = process.cwd();
const state = await mkdtemp(path.join(tmpdir(), "tailterm-board-compose-"));
const binary = path.join(state, "tailterm-hub");
const fixtureEnv = Object.fromEntries(
  Object.entries(process.env).filter(
    ([key]) => !/^(TAILTERM_|CODEX_|TT_|TMUX)/.test(key) && key !== "HOME",
  ),
);
let backend;
let web;
let origin = "";
let hold = null;
let loseNextResponse = false;
let loseNextExportCreate = false;
let failNextExportChunk = false;
let failNextAuditMutation = false;
const attempts = [];
const exportAttempts = [];
const exportChunks = [];
const auditMutationAttempts = [];

const pause = (ms) => new Promise((resolve) => setTimeout(resolve, ms));
async function waitFor(check, label) {
  for (let i = 0; i < 200; i++) {
    if (await check()) return;
    await pause(25);
  }
  throw new Error(`Timed out waiting for ${label}`);
}
async function freePort() {
  const server = createServer();
  server.listen(0, "127.0.0.1");
  await once(server, "listening");
  const port = server.address().port;
  await new Promise((resolve) => server.close(resolve));
  return port;
}
function holdNextMessage() {
  let started;
  let release;
  const startedPromise = new Promise((resolve) => (started = resolve));
  const releasePromise = new Promise((resolve) => (release = resolve));
  hold = { started, releasePromise };
  return { started: startedPromise, release };
}
async function selectAuditKind(page, value) {
  const select = page.locator("#board-audit-kind");
  await select.evaluate((element) => (element.closest("details").open = true));
  await select.selectOption(value, { force: true });
  await page.waitForFunction(
    (expected) =>
      document.querySelector("#board-audit-kind")?.value === expected,
    value,
  );
}

const html = `<!doctype html><html><head><meta charset="utf-8">
<link rel="stylesheet" href="/client/style.css"></head><body>
<main><header></header><div id="mode-view"></div></main><p id="notice" role="status"></p>
<script type="module">
import {createBoardView} from '/client/board-view.js';
import {createHubClient} from '/client/hub-client.js';
const task=new URLSearchParams(location.search).get('task');
const client=createHubClient({baseURL:location.origin,fetchImpl:(url,init)=>fetch(url,init)});
const notice=text=>document.querySelector('#notice').textContent=text;
const noop=()=>{};
const intentPersistence={
  async list(scope){return JSON.parse(sessionStorage.getItem('intents:'+scope)||'[]')},
  async save(record){const key='intents:'+record.scope;const rows=JSON.parse(sessionStorage.getItem(key)||'[]');sessionStorage.setItem(key,JSON.stringify([...rows.filter(row=>row.id!==record.id),record]))},
  async remove(scope,id){const key='intents:'+scope;const rows=JSON.parse(sessionStorage.getItem(key)||'[]');sessionStorage.setItem(key,JSON.stringify(rows.filter(row=>row.id!==id)))},
};
const board=createBoardView({client:()=>client,getTabs:()=>[],activate:noop,notice,addAgent:noop,
  settings:noop,revealAgent:noop,attachTask:noop,newTask:noop,configure:noop,intentPersistence});
board.mount(document.querySelector('#mode-view'));
await board.show(task);
window.qa={board};
</script></body></html>`;

try {
  await exec("go", ["build", "-o", binary, "./cmd/tailterm-hub"], {
    cwd: path.join(root, "hub"),
  });
  const hubPort = await freePort();
  const hub = `http://127.0.0.1:${hubPort}`;
  backend = spawn(binary, {
    env: {
      ...fixtureEnv,
      TAILTERM_STATE: state,
      TAILTERM_DEV_LISTEN: `127.0.0.1:${hubPort}`,
      TAILTERM_TCP_LISTEN: "",
    },
    stdio: "ignore",
  });
  await waitFor(
    async () =>
      fetch(`${hub}/v1/tasks`)
        .then((response) => response.ok)
        .catch(() => false),
    "isolated hub",
  );

  async function api(method, route, body, expected = 200) {
    const response = await fetch(hub + route, {
      method,
      headers:
        body === undefined ? undefined : { "content-type": "application/json" },
      body: body === undefined ? undefined : JSON.stringify(body),
    });
    const text = await response.text();
    assert.equal(response.status, expected, `${method} ${route}: ${text}`);
    return text ? JSON.parse(text) : null;
  }
  async function project(label) {
    const task = await api(
      "POST",
      "/v1/tasks",
      { name: `${label} Board composer` },
      201,
    );
    const agent = await api(
      "POST",
      `/v1/tasks/${task.id}/agents`,
      {
        name: "fixture-worker",
        host: "synthetic.invalid",
        session: "unused-synthetic-session",
        runtime: "codex",
        cwd: state,
      },
      201,
    );
    const source = await api(
      "POST",
      `/v1/tasks/${task.id}/messages`,
      { agentId: agent.id, text: "Synthetic worker source" },
      201,
    );
    const primary = await api(
      "POST",
      `/v1/tasks/${task.id}/work-items`,
      {
        kind: "feature",
        title: `${label} primary`,
        description: "Synthetic only",
        priority: "normal",
        sourceMessageSeq: source.seq,
        requestId: `${label}-primary`,
      },
      201,
    );
    const related = await api(
      "POST",
      `/v1/tasks/${task.id}/work-items`,
      {
        kind: "bug",
        title: `${label} related`,
        description: "Synthetic only",
        priority: "normal",
        sourceMessageSeq: source.seq,
        requestId: `${label}-related`,
      },
      201,
    );
    return { task, agent, source, primary, related };
  }
  async function messages(project) {
    return (await api("GET", `/v1/tasks/${project.task.id}/messages?limit=200`))
      .messages;
  }

  web = createServer(async (req, res) => {
    try {
      const url = new URL(req.url, origin || "http://127.0.0.1");
      if (url.pathname === "/") {
        res.writeHead(200, { "content-type": "text/html" });
        res.end(html);
        return;
      }
      if (url.pathname.startsWith("/v1/")) {
        const chunks = [];
        for await (const chunk of req) chunks.push(chunk);
        const raw = Buffer.concat(chunks);
        const isMessagePost =
          req.method === "POST" &&
          /\/v1\/tasks\/[^/]+\/messages$/.test(url.pathname);
        const isExportCreate =
          req.method === "POST" &&
          /\/v1\/tasks\/[^/]+\/audit-exports$/.test(url.pathname);
        const isExportChunk =
          req.method === "GET" &&
          /\/v1\/tasks\/[^/]+\/audit-exports\/[^/]+$/.test(url.pathname);
        const isAuditMutation =
          req.method === "POST" &&
          /\/v1\/tasks\/[^/]+\/message-audit\/messages\/\d+\/(corrections|resolve)$/.test(
            url.pathname,
          );
        let gate = null;
        let lost = false;
        if (isMessagePost) {
          const body = JSON.parse(raw);
          attempts.push({ path: url.pathname, body });
          gate = hold;
          hold = null;
          lost = loseNextResponse;
          loseNextResponse = false;
          if (gate) {
            gate.started();
            await gate.releasePromise;
          }
        }
        if (isExportCreate) {
          exportAttempts.push({ path: url.pathname, body: JSON.parse(raw) });
          lost = loseNextExportCreate;
          loseNextExportCreate = false;
        }
        if (isExportChunk) {
          exportChunks.push(req.url);
          if (failNextExportChunk) {
            failNextExportChunk = false;
            res.writeHead(503, { "content-type": "application/json" });
            res.end(JSON.stringify({ error: "Synthetic chunk interruption" }));
            return;
          }
        }
        if (isAuditMutation) {
          auditMutationAttempts.push({
            path: url.pathname,
            body: JSON.parse(raw),
          });
          if (failNextAuditMutation) {
            failNextAuditMutation = false;
            res.writeHead(503, { "content-type": "application/json" });
            res.end(JSON.stringify({ error: "Synthetic audit interruption" }));
            return;
          }
        }
        const response = await fetch(hub + req.url, {
          method: req.method,
          headers: raw.length
            ? { "content-type": "application/json" }
            : undefined,
          body: ["GET", "HEAD"].includes(req.method) ? undefined : raw,
          signal: AbortSignal.timeout(35000),
        });
        const text = await response.text();
        if (lost) {
          assert.equal(
            response.status,
            201,
            "response loss must follow a committed operation",
          );
          if (isExportCreate) exportAttempts.at(-1).metadata = JSON.parse(text);
          res.writeHead(503, { "content-type": "application/json" });
          res.end(
            JSON.stringify({ error: "Synthetic response lost after commit" }),
          );
          return;
        }
        if (isExportCreate) exportAttempts.at(-1).metadata = JSON.parse(text);
        res.writeHead(response.status, { "content-type": "application/json" });
        res.end(text);
        return;
      }
      const file = path.resolve(root, "." + decodeURIComponent(url.pathname));
      if (!file.startsWith(root + path.sep))
        throw new Error("invalid static path");
      res.writeHead(200, {
        "content-type": file.endsWith(".js")
          ? "text/javascript"
          : file.endsWith(".css")
            ? "text/css"
            : "application/octet-stream",
      });
      res.end(await readFile(file));
    } catch (error) {
      if (!res.destroyed) {
        res.writeHead(500, { "content-type": "application/json" });
        res.end(JSON.stringify({ error: error.message }));
      }
    }
  });
  web.listen(0, "127.0.0.1");
  await once(web, "listening");
  origin = `http://127.0.0.1:${web.address().port}`;

  const results = [];
  for (const [name, engine] of [
    ["chromium", chromium],
    ["webkit", webkit],
  ]) {
    const fixture = await project(name);
    const browser = await engine.launch();
    try {
      const page = await browser.newPage({
        viewport: { width: 1100, height: 760 },
      });
      const pageErrors = [];
      page.on("pageerror", (error) => pageErrors.push(error.message));
      await page.goto(`${origin}/?task=${fixture.task.id}`);
      const input = page.locator("#board-text");
      const sendButton = page.locator("#board-compose button[type=submit]");
      await input.waitFor();
      const path = `/v1/tasks/${fixture.task.id}/messages`;
      const postCount = () =>
        attempts.filter((attempt) => attempt.path === path).length;

      await selectAuditKind(page, "intake");
      await page.locator("#board-text").fill("Explicit synthetic Intake");
      await page.locator("#board-compose button[type=submit]").click();
      await waitFor(() => postCount() >= 1, `${name} Intake post`);
      assert.equal(
        attempts.filter((attempt) => attempt.path === path).at(-1).body
          .auditKind,
        "intake",
      );
      const intakeRow = page
        .locator(".board-message", { hasText: "Explicit synthetic Intake" })
        .last();
      await intakeRow.locator('[data-audit-kind="intake"]').waitFor();
      await intakeRow.locator("[data-audit-resolve-open]").click();
      await intakeRow
        .locator('[data-audit-resolve] input[name="existing"]')
        .fill(fixture.primary.id);
      await intakeRow
        .locator('[data-audit-resolve] input[name="existingRevision"]')
        .fill(String(fixture.primary.revision));
      await intakeRow
        .locator('[data-audit-resolve] input[name="reason"]')
        .fill("Synthetic intake has an exact owner.");
      await intakeRow
        .locator('[data-audit-resolve] textarea[name="sources"]')
        .fill(`${fixture.task.id}#${fixture.source.seq}`);
      await intakeRow
        .locator('[data-audit-resolve] button[type="submit"]')
        .click();
      await intakeRow.locator('[data-audit-kind="work"]').waitFor();

      await selectAuditKind(page, "intake");
      await page.locator("#board-text").fill("Persistent new-item Intake");
      await page.locator("#board-compose button[type=submit]").click();
      const newItemRow = page
        .locator(".board-message", { hasText: "Persistent new-item Intake" })
        .last();
      await newItemRow.locator('[data-audit-kind="intake"]').waitFor();
      await newItemRow.locator("[data-audit-resolve-open]").click();
      const resolveForm = newItemRow.locator("[data-audit-resolve]");
      await resolveForm.locator('select[name="target"]').selectOption("new");
      await resolveForm.locator('select[name="kind"]').selectOption("feature");
      await resolveForm
        .locator('input[name="title"]')
        .fill("Synthetic recovered feature");
      await resolveForm
        .locator('input[name="reason"]')
        .fill("Exercise pinned new-item recovery.");
      await resolveForm
        .locator('textarea[name="sources"]')
        .fill(`${fixture.task.id}#${fixture.source.seq}`);
      await page.evaluate(() => qa.board.reload());
      assert.equal(
        await resolveForm.locator('select[name="target"]').inputValue(),
        "new",
      );
      assert.equal(
        await resolveForm.locator('select[name="kind"]').inputValue(),
        "feature",
      );
      assert.equal(
        await resolveForm.locator('input[name="title"]').inputValue(),
        "Synthetic recovered feature",
      );
      const resolveAttemptStart = auditMutationAttempts.length;
      failNextAuditMutation = true;
      await resolveForm.locator('button[type="submit"]').click();
      const resolveIntent = page.locator(".board-intent-recovery", {
        hasText: "Uncertain resolve",
      });
      await resolveIntent.waitFor();
      await page.evaluate(() => qa.board.hide());
      await page.reload();
      const restoredNewItemRow = page
        .locator(".board-message", { hasText: "Persistent new-item Intake" })
        .last();
      const restoredResolve = restoredNewItemRow.locator(
        "[data-audit-resolve]",
      );
      assert.equal(
        await restoredResolve.locator('select[name="target"]').inputValue(),
        "new",
      );
      assert.equal(
        await restoredResolve.locator('select[name="kind"]').inputValue(),
        "feature",
      );
      assert.match(
        await restoredResolve.locator("strong").innerText(),
        /pinned r1/,
      );
      await page
        .locator(".board-intent-recovery", { hasText: "Uncertain resolve" })
        .locator("[data-intent-retry]")
        .click();
      await restoredNewItemRow.locator('[data-audit-kind="work"]').waitFor();
      assert.equal(auditMutationAttempts.length, resolveAttemptStart + 2);
      assert.deepEqual(
        auditMutationAttempts.at(-1).body,
        auditMutationAttempts.at(-2).body,
        `${name}: resolution recovery changed pinned intent`,
      );
      assert.equal(auditMutationAttempts.at(-1).body.expectedRevision, 1);
      assert.equal(auditMutationAttempts.at(-1).body.newItem.kind, "feature");

      await selectAuditKind(page, "work");
      await page.locator("#board-primary-task").fill(fixture.task.id);
      await page.locator("#board-primary-item").fill(fixture.primary.id);
      await page
        .locator("#board-primary-revision")
        .fill(String(fixture.primary.revision));
      await page.locator("#board-order-task").fill(fixture.task.id);
      await page.locator("#board-order-seq").fill(String(fixture.source.seq));
      await page
        .locator("#board-related")
        .fill(
          `${fixture.task.id}/${fixture.related.id}@${fixture.related.revision}`,
        );
      await page.locator("#board-text").fill("Explicit synthetic Work");
      const beforeCorrectionCount = (await messages(fixture)).length + 1;
      await page.locator("#board-compose button[type=submit]").click();
      const workRow = page
        .locator(".board-message", { hasText: "Explicit synthetic Work" })
        .last();
      await workRow.locator('[data-audit-kind="work"]').waitFor();
      await workRow.locator("[data-reply]").click();
      assert.equal(
        await page.locator("#board-audit-kind").inputValue(),
        "work",
      );
      assert.equal(
        await page.locator("#board-primary-item").inputValue(),
        fixture.primary.id,
      );
      assert.equal(
        await page.locator("#board-order-seq").inputValue(),
        String(fixture.source.seq),
      );
      await page.locator("#board-cancel-reply").click();
      await page.locator("#board-audit-kind").selectOption("");
      await workRow.locator("[data-audit-correct-open]").click();
      await workRow
        .locator('[data-audit-correct] select[name="classification"]')
        .selectOption("intake");
      await workRow
        .locator('[data-audit-correct] input[name="reason"]')
        .fill("Synthetic reclassification without a new message.");
      await workRow
        .locator('[data-audit-correct] textarea[name="sources"]')
        .fill(`${fixture.task.id}#${fixture.source.seq}`);
      await page.evaluate(() => qa.board.reload());
      assert.equal(
        await workRow
          .locator('[data-audit-correct] select[name="classification"]')
          .inputValue(),
        "intake",
      );
      assert.equal(
        await workRow
          .locator('[data-audit-correct] input[name="reason"]')
          .inputValue(),
        "Synthetic reclassification without a new message.",
      );
      const correctionAttemptStart = auditMutationAttempts.length;
      failNextAuditMutation = true;
      await workRow
        .locator('[data-audit-correct] button[type="submit"]')
        .click();
      const correctionIntent = page.locator(".board-intent-recovery", {
        hasText: "Uncertain correct",
      });
      await correctionIntent.waitFor();
      assert.equal(
        await workRow
          .locator('[data-audit-correct] input[name="reason"]')
          .inputValue(),
        "Synthetic reclassification without a new message.",
      );
      await page.evaluate(() => qa.board.hide());
      await page.reload();
      const restoredWorkRow = page
        .locator(".board-message", { hasText: "Explicit synthetic Work" })
        .last();
      assert.equal(
        await restoredWorkRow
          .locator('[data-audit-correct] select[name="classification"]')
          .inputValue(),
        "intake",
      );
      const restoredCorrectionIntent = page.locator(".board-intent-recovery", {
        hasText: "Uncertain correct",
      });
      await restoredCorrectionIntent.locator("[data-intent-retry]").click();
      await workRow.locator('[data-audit-kind="intake"]').waitFor();
      assert.equal(auditMutationAttempts.length, correctionAttemptStart + 2);
      assert.deepEqual(
        auditMutationAttempts.at(-1).body,
        auditMutationAttempts.at(-2).body,
        `${name}: correction recovery changed pinned intent`,
      );
      const correctedContext = await workRow
        .locator(".message-audit")
        .innerText();
      assert.match(correctedContext, /Original: work/);
      assert.match(correctedContext, /Current: intake/);
      assert.match(correctedContext, new RegExp(fixture.primary.id));
      await workRow.locator("[data-audit-history]").click();
      const correctionHistory = workRow.locator(".message-audit-history");
      await correctionHistory.waitFor();
      const historyText = await correctionHistory.textContent();
      assert.match(historyText, /Before: work/);
      assert.match(historyText, /After: intake/);
      assert.match(historyText, /Actor:/);
      assert.match(historyText, /Sources:/);
      assert.equal(
        (await messages(fixture)).length,
        beforeCorrectionCount,
        `${name}: correction created a new message`,
      );
      const openDownload = page.waitForEvent("download");
      await page.locator("#board-audit-export").click();
      const openPath = await (await openDownload).path();
      const openExport = JSON.parse(await readFile(openPath, "utf8"));
      assert.equal(openExport.formatVersion, 2);
      assert.equal(openExport.sourceProject, fixture.task.id);
      assert.ok(
        openExport.streams.messageAuditEvents.some(
          (event) => event.operation === "correct",
        ),
        `${name}: open Board export omitted audit correction`,
      );

      const lostCreateStart = exportAttempts.length;
      loseNextExportCreate = true;
      await page.locator("#board-audit-export").click();
      const lostIntent = page.locator(".board-intent-recovery", {
        hasText: "Uncertain export",
      });
      await lostIntent.waitFor();
      assert.equal(exportAttempts.length, lostCreateStart + 1);
      const lostRequest = exportAttempts.at(-1);
      await lostIntent.locator("[data-intent-recover]").click();
      await waitFor(
        async () =>
          exportAttempts.length === lostCreateStart + 2 &&
          exportAttempts.at(-1).metadata,
        `${name} export receipt recovery`,
      );
      assert.deepEqual(
        exportAttempts.at(-1).body,
        lostRequest.body,
        `${name}: export receipt recovery changed intent`,
      );
      assert.equal(
        exportAttempts.at(-1).metadata.id,
        lostRequest.metadata.id,
        `${name}: export receipt recovery changed frozen ID`,
      );
      const lostDownload = page.waitForEvent("download");
      await lostIntent.locator("[data-intent-retry]").click();
      const lostPath = await (await lostDownload).path();
      assert.equal(
        JSON.parse(await readFile(lostPath, "utf8")).sourceProject,
        fixture.task.id,
      );
      await lostIntent.waitFor({ state: "detached" });

      const interruptedCreateStart = exportAttempts.length;
      const interruptedChunkStart = exportChunks.length;
      failNextExportChunk = true;
      await page.locator("#board-audit-export").click();
      const interruptedIntent = page.locator(".board-intent-recovery", {
        hasText: "Uncertain export",
      });
      await interruptedIntent.waitFor();
      await waitFor(
        async () =>
          exportAttempts.length === interruptedCreateStart + 1 &&
          exportAttempts.at(-1).metadata,
        `${name} interrupted export metadata`,
      );
      const interruptedID = exportAttempts.at(-1).metadata.id;
      const interruptedDownload = page.waitForEvent("download");
      await interruptedIntent.locator("[data-intent-retry]").click();
      await (await interruptedDownload).path();
      assert.equal(
        exportAttempts.length,
        interruptedCreateStart + 1,
        `${name}: chunk retry created a replacement export`,
      );
      const retryChunks = exportChunks.slice(interruptedChunkStart);
      assert.ok(retryChunks.length >= 2, `${name}: no interrupted chunk retry`);
      assert.ok(
        retryChunks.every((url) =>
          url.includes(`/audit-exports/${interruptedID}`),
        ),
        `${name}: chunk retry changed immutable export ID`,
      );
      await interruptedIntent.waitFor({ state: "detached" });

      await input.focus();
      assert.equal(
        await input.inputValue(),
        "",
        `${name}: confirmed post draft was not cleared`,
      );
      const emptyBefore = postCount();
      await page.keyboard.press("Enter");
      await pause(75);
      assert.equal(postCount(), emptyBefore, `${name}: empty Enter posted`);
      assert.equal(
        await input.inputValue(),
        "",
        `${name}: empty Enter inserted a newline`,
      );

      await input.fill("Synthetic composition draft");
      const compositionBefore = postCount();
      const compositionDispatch = await input.evaluate((element) => {
        element.dispatchEvent(
          new CompositionEvent("compositionstart", { bubbles: true }),
        );
        const activeCompositionPrevented = !element.dispatchEvent(
          new KeyboardEvent("keydown", {
            key: "Enter",
            code: "Enter",
            bubbles: true,
            cancelable: true,
          }),
        );
        element.dispatchEvent(
          new CompositionEvent("compositionend", { bubbles: true }),
        );
        const composingEnter = new KeyboardEvent("keydown", {
          key: "Enter",
          code: "Enter",
          bubbles: true,
          cancelable: true,
        });
        Object.defineProperty(composingEnter, "isComposing", { value: true });
        const isComposingPrevented = !element.dispatchEvent(composingEnter);
        return {
          isComposing: composingEnter.isComposing,
          isComposingPrevented,
          activeCompositionPrevented,
        };
      });
      assert.deepEqual(compositionDispatch, {
        isComposing: true,
        isComposingPrevented: false,
        activeCompositionPrevented: false,
      });
      await pause(75);
      assert.equal(
        postCount(),
        compositionBefore,
        `${name}: composition Enter posted ${JSON.stringify(compositionDispatch)}`,
      );
      assert.equal(await input.inputValue(), "Synthetic composition draft");
      await page.keyboard.press("Enter");
      await waitFor(
        async () =>
          (await messages(fixture)).some(
            (message) => message.text === "Synthetic composition draft",
          ),
        `${name} plain Enter message`,
      );
      await waitFor(
        async () =>
          (await input.inputValue()) === "" && (await sendButton.isEnabled()),
        `${name} plain Enter completion`,
      );

      await input.fill("first line");
      const multilineBefore = postCount();
      await page.keyboard.press("Shift+Enter");
      await page.keyboard.type("second line");
      assert.equal(await input.inputValue(), "first line\nsecond line");
      assert.equal(postCount(), multilineBefore, `${name}: Shift+Enter posted`);
      await page.keyboard.press("Enter");
      await waitFor(
        async () =>
          (await messages(fixture)).some(
            (message) => message.text === "first line\nsecond line",
          ),
        `${name} multiline message`,
      );
      await waitFor(
        async () =>
          (await input.inputValue()) === "" && (await sendButton.isEnabled()),
        `${name} multiline completion`,
      );

      await input.fill("Held Enter sends once");
      const repeatBefore = postCount();
      const repeatedDefaultAllowed = await input.evaluate((element) =>
        element.dispatchEvent(
          new KeyboardEvent("keydown", {
            key: "Enter",
            code: "Enter",
            repeat: true,
            bubbles: true,
            cancelable: true,
          }),
        ),
      );
      assert.equal(
        repeatedDefaultAllowed,
        false,
        `${name}: repeat was not suppressed`,
      );
      await pause(75);
      assert.equal(
        postCount(),
        repeatBefore,
        `${name}: repeated keydown posted`,
      );
      assert.equal(await input.inputValue(), "Held Enter sends once");

      await input.evaluate((element) => (element.disabled = true));
      const disabledDefaultAllowed = await input.evaluate((element) =>
        element.dispatchEvent(
          new KeyboardEvent("keydown", {
            key: "Enter",
            code: "Enter",
            bubbles: true,
            cancelable: true,
          }),
        ),
      );
      assert.equal(
        disabledDefaultAllowed,
        false,
        `${name}: disabled Enter was not suppressed`,
      );
      await pause(75);
      assert.equal(
        postCount(),
        repeatBefore,
        `${name}: disabled composer posted`,
      );
      await input.evaluate((element) => (element.disabled = false));

      const pending = holdNextMessage();
      const pendingBefore = postCount();
      await input.focus();
      await page.keyboard.press("Enter");
      await pending.started;
      assert.equal(postCount(), pendingBefore + 1);
      assert.equal(
        await page.locator("#board-compose button[type=submit]").isDisabled(),
        true,
        `${name}: pending submit button remained enabled`,
      );
      await page.keyboard.press("Enter");
      await pause(75);
      assert.equal(
        postCount(),
        pendingBefore + 1,
        `${name}: pending Enter duplicated request`,
      );
      pending.release();
      await waitFor(
        async () =>
          (await input.inputValue()) === "" && (await sendButton.isEnabled()),
        `${name} pending send completion`,
      );
      assert.equal(
        (await messages(fixture)).filter(
          (message) => message.text === "Held Enter sends once",
        ).length,
        1,
        `${name}: pending send duplicated stored message`,
      );

      await page.locator(`[data-reply="${fixture.source.seq}"]`).click();
      assert.equal(
        await page.locator("#board-to").inputValue(),
        fixture.agent.id,
      );
      await page.locator("#board-cancel-reply").click();
      await waitFor(
        async () => (await page.locator("#board-cancel-reply").count()) === 0,
        `${name} reply cancellation`,
      );
      assert.equal(
        await page.locator("#board-to").inputValue(),
        fixture.agent.id,
      );
      await page.locator(`[data-reply="${fixture.source.seq}"]`).click();
      await input.fill("Failure retry remains exact");
      await input.evaluate((element) => {
        element.focus();
        element.setSelectionRange(2, 9);
      });
      const failureBefore = postCount();
      loseNextResponse = true;
      await page.keyboard.press("Enter");
      await waitFor(
        async () =>
          (await page.locator("#notice").textContent()).includes("Post failed"),
        `${name} failed post notice`,
      );
      assert.equal(await input.inputValue(), "Failure retry remains exact");
      assert.equal(
        await page.locator("#board-to").inputValue(),
        fixture.agent.id,
      );
      assert.equal(await page.locator("#board-cancel-reply").count(), 1);
      assert.deepEqual(
        await input.evaluate((element) => [
          element.selectionStart,
          element.selectionEnd,
        ]),
        [2, 9],
        `${name}: failure changed draft selection`,
      );
      await page.keyboard.press("Enter");
      await waitFor(
        async () =>
          (await input.inputValue()) === "" && (await sendButton.isEnabled()),
        `${name} failure retry completion`,
      );
      const failureAttempts = attempts
        .filter((attempt) => attempt.path === path)
        .slice(failureBefore);
      assert.equal(failureAttempts.length, 2, `${name}: retry request count`);
      assert.deepEqual(failureAttempts[1].body, failureAttempts[0].body);
      assert.ok(
        failureAttempts[0].body.requestId,
        `${name}: retry identity missing`,
      );
      const retried = (await messages(fixture)).filter(
        (message) => message.text === "Failure retry remains exact",
      );
      assert.equal(
        retried.length,
        1,
        `${name}: committed retry duplicated message`,
      );
      assert.equal(retried[0].to, fixture.agent.id);
      assert.equal(retried[0].replyTo, fixture.source.seq);
      assert.equal(
        retried[0].postReceipt.requestId,
        failureAttempts[0].body.requestId,
      );
      await api("DELETE", `/v1/tasks/${fixture.task.id}`);
      await page.evaluate(() => qa.board.hide());
      await page.reload();
      await page.locator("#board-audit-export").waitFor();
      const closedWorkRow = page
        .locator(".board-message", { hasText: "Explicit synthetic Work" })
        .last();
      await closedWorkRow.locator("[data-audit-history]").click();
      await closedWorkRow.locator(".message-audit-history").waitFor();
      const closedDownload = page.waitForEvent("download");
      await page.locator("#board-audit-export").click();
      const closedPath = await (await closedDownload).path();
      const closedExport = JSON.parse(await readFile(closedPath, "utf8"));
      assert.equal(closedExport.streams.task[0].status, "closed");
      assert.deepEqual(pageErrors, [], `${name}: browser errors`);
      results.push(name);
      await page.evaluate(() => qa.board.hide());
    } finally {
      await browser.close();
    }
  }
  console.log(
    `Board audit/composer passed in ${results.join(", ")}: explicit Intake/work/related/resolve/correct, open+closed verified v2 exports, real Enter, Shift+Enter, synthetic composition/repeat guards, recipient/cancel retention, and committed failure retry exactly once.`,
  );
} finally {
  if (web) await new Promise((resolve) => web.close(resolve));
  if (backend) {
    backend.kill("SIGTERM");
    await Promise.race([once(backend, "exit"), pause(3000)]);
  }
  await rm(state, { recursive: true, force: true });
}
