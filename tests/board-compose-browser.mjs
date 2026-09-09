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
const attempts = [];

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
const board=createBoardView({client:()=>client,getTabs:()=>[],activate:noop,notice,addAgent:noop,
  settings:noop,revealAgent:noop,attachTask:noop,newTask:noop,configure:noop});
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
    return { task, agent, source };
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
            "response loss must follow a committed message",
          );
          res.writeHead(503, { "content-type": "application/json" });
          res.end(
            JSON.stringify({ error: "Synthetic response lost after commit" }),
          );
          return;
        }
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

      await input.focus();
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
      assert.deepEqual(pageErrors, [], `${name}: browser errors`);
      results.push(name);
      await page.evaluate(() => qa.board.hide());
    } finally {
      await browser.close();
    }
  }
  console.log(
    `Board composer passed in ${results.join(", ")}: real Enter, Shift+Enter multiline, synthetic composition/repeat guards, empty/disabled/pending suppression, recipient/cancel retention, and committed failure retry exactly once.`,
  );
} finally {
  if (web) await new Promise((resolve) => web.close(resolve));
  if (backend) {
    backend.kill("SIGTERM");
    await Promise.race([once(backend, "exit"), pause(3000)]);
  }
  await rm(state, { recursive: true, force: true });
}
