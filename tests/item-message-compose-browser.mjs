// Acceptance for wi_b7576fdb3589daaf, order8108/Start8122, synthetic only.
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
<link rel="stylesheet" href="/client/style.css"><link rel="stylesheet" href="/client/work-items.css"><style>#notice:empty{display:none}</style></head><body data-mode="board"><div id="workspace"><aside></aside>
<main><header></header><div id="mode-view"></div></main></div><p id="notice" role="status"></p>
<script type="module">
import {createBoardView} from '/client/board-view.js';
import {createWorkItemsView} from '/client/work-items-view.js';
import {createHubClient} from '/client/hub-client.js';
const task=new URLSearchParams(location.search).get('task');
const client=createHubClient({baseURL:location.origin,fetchImpl:(url,init)=>fetch(url,init)});
// Refresh is explicit in this fixture; subscription/scroll races have separate browser suites.
client.subscribe=()=>({stop(){}});
const notice=text=>document.querySelector('#notice').textContent=text;
const noop=()=>{};
const intentPersistence={
  async list(scope){return JSON.parse(sessionStorage.getItem('intents:'+scope)||'[]')},
  async save(record){const key='intents:'+record.scope;const rows=JSON.parse(sessionStorage.getItem(key)||'[]');sessionStorage.setItem(key,JSON.stringify([...rows.filter(row=>row.id!==record.id),record]))},
  async remove(scope,id){const key='intents:'+scope;const rows=JSON.parse(sessionStorage.getItem(key)||'[]');sessionStorage.setItem(key,JSON.stringify(rows.filter(row=>row.id!==id)))},
};
// Prove Board uses authoritative existing request reads rather than cached item methods.
const boardClient={...client,listWorkItems:()=>{throw new Error('saved item list must not supply eligibility')},getWorkItem:()=>{throw new Error('saved item revision must not supply validation')}};
const board=createBoardView({client:()=>boardClient,getTabs:()=>[],activate:noop,notice,addAgent:noop,
  settings:noop,revealAgent:noop,attachTask:noop,newTask:noop,configure:noop,intentPersistence});
board.mount(document.querySelector('#mode-view'));
await board.show(task);
const itemsRoot=document.querySelector('#mode-view');
let itemView;
window.qa={board, async showItems(kind,project){
  board.hide(); itemView?.hide(); document.body.dataset.mode=kind==='bug'?'bugs':'features';
  itemView=createWorkItemsView({kind,client:()=>client,notice,dialog:noop,closeDialog:noop,configure:noop,
    openBoard:async(id,item)=>{itemView.hide();document.body.dataset.mode='board';await board.show(id,item)}});
  itemView.mount(itemsRoot);await itemView.show(project);
}, drafts(){return Object.keys(sessionStorage).filter(key=>key.startsWith('intents:')).flatMap(key=>JSON.parse(sessionStorage.getItem(key)))}};
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
            "response loss must follow a committed operation",
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
    const fixture = await project(name),
      other = await project(name + "-other");
    async function update(item, values) {
      return api("PATCH", `/v1/tasks/${item.taskId}/work-items/${item.id}`, {
        title: item.title,
        description: item.description,
        status: item.status,
        priority: item.priority,
        ...values,
        revision: item.revision,
      });
    }
    let active = await update(fixture.primary, { status: "in_progress" });
    let bug = await update(fixture.related, { status: "blocked" });
    const browser = await engine.launch();
    try {
      const page = await browser.newPage({
        viewport: { width: 1100, height: 760 },
      });
      const errors = [];
      page.on("pageerror", (e) => errors.push(e.message));
      await page.goto(`${origin}/?task=${fixture.task.id}`);
      const input = page.locator("#board-text"),
        send = page.locator("#board-compose button[type=submit]");
      await input.waitFor();
      async function idle() {
        await waitFor(async () => await send.isEnabled(), name + " idle");
      }
      async function choose(item) {
        await page
          .locator("#board-message-item")
          .selectOption(`${item.id}@${item.revision}`);
      }
      async function draftValue() {
        return page.evaluate(
          () => qa.drafts().find((x) => x.id.startsWith("draft:"))?.values,
        );
      }
      async function openItem(kind, item) {
        await page.evaluate(async ({ kind, id }) => qa.showItems(kind, id), {
          kind,
          id: item.taskId,
        });
        await page.setViewportSize({ width: 390, height: 740 });
        await page.locator("#notice").evaluate((el) => (el.textContent = ""));
        await page.screenshot({
          path: `/tmp/item-message-8108-${name}-${kind}-narrow.png`,
        });
        assert.equal(
          await page.evaluate(
            () => document.documentElement.scrollWidth > innerWidth,
          ),
          false,
        );
        const actionBoxes = await page
          .locator(`[data-work-item="${item.id}"] .work-item-actions button`)
          .evaluateAll((nodes) =>
            nodes.map((node) => {
              const box = node.getBoundingClientRect();
              return { left: box.left, right: box.right };
            }),
          );
        assert.ok(
          actionBoxes.every((box) => box.left >= 0 && box.right <= 390),
          "Every item action must fit the narrow viewport",
        );
        await page.setViewportSize({ width: 1100, height: 760 });
        await page.locator(`[data-item-message="${item.id}"]`).click();
        await page.waitForFunction(
          (id) =>
            document
              .querySelector(".board-item-compose")
              ?.textContent.includes(id),
          item.id,
        );
      }
      await page.locator("#board-message-item-mode").click();
      const opts = await page
        .locator("#board-message-item option")
        .evaluateAll((nodes) => nodes.map((n) => n.value));
      assert.deepEqual(opts, ["", `${active.id}@${active.revision}`]);
      await input.fill("Must select an item");
      const before = attempts.length;
      await send.click();
      await idle();
      assert.equal(attempts.length, before);
      assert.match(await page.locator("#notice").textContent(), /Choose a bug/);
      await choose(active);
      assert.equal(
        await page.evaluate(() => document.activeElement?.id),
        "board-message-item",
      );
      await page.evaluate((id) => qa.board.show(id), fixture.task.id);
      assert.equal(
        await page.locator("#board-message-item").inputValue(),
        `${active.id}@${active.revision}`,
      );
      assert.equal(
        await page.evaluate(() => document.activeElement?.id),
        "board-message-item",
      );
      await page.locator("#board-to").selectOption(fixture.agent.id);
      await input.fill("Feature linked message");
      await input.press("Shift+Enter");
      assert.equal(await input.inputValue(), "Feature linked message\n");
      await input.evaluate((el) =>
        el.dispatchEvent(
          new KeyboardEvent("keydown", {
            key: "Enter",
            isComposing: true,
            bubbles: true,
            cancelable: true,
          }),
        ),
      );
      assert.equal(attempts.length, before);
      await input.press("Enter");
      await waitFor(
        async () =>
          (await messages(fixture)).some(
            (x) => x.text === "Feature linked message",
          ),
        "feature message",
      );
      await idle();
      const posted = (await messages(fixture)).find(
        (x) => x.text === "Feature linked message",
      );
      assert.deepEqual(posted.workItems, [
        {
          itemTaskId: fixture.task.id,
          itemId: active.id,
          itemRevision: active.revision,
          relationship: "primary",
        },
      ]);
      assert.equal(posted.to, fixture.agent.id);
      assert.ok(posted.postReceipt);
      assert.equal(posted.workOrderMessage, undefined);
      assert.equal(attempts.at(-1).body.auditKind, undefined);
      // Both real work-item views hand off an exact revision; blocked direct item remains eligible.
      await openItem("bug", bug);
      assert.equal((await draftValue()).primaryRevision, String(bug.revision));
      await input.fill("Direct blocked bug");
      await send.click();
      await waitFor(
        async () =>
          (await messages(fixture)).some(
            (x) => x.text === "Direct blocked bug",
          ),
        "blocked bug",
      );
      await idle();
      await openItem("feature", active);
      await input.fill("Stale text retained");
      active = await update(active, { title: "Changed feature title" });
      await send.click();
      await idle();
      assert.match(await page.locator("#notice").textContent(), /stale/);
      assert.equal(await input.inputValue(), "Stale text retained");
      assert.equal(
        (await draftValue()).primaryRevision,
        String(active.revision - 1),
      );
      await page.evaluate((id) => qa.board.show(id), fixture.task.id);
      await choose(active);
      // Commit with response loss, reload, then exact retry after a later item revision.
      await input.fill("Lost response once");
      loseNextResponse = true;
      await send.click();
      await idle();
      await page.locator("[data-intent-retry]").waitFor();
      const frozen = structuredClone(attempts.at(-1).body);
      active = await update(active, { title: "Later revision" });
      await page.reload();
      await page.locator("[data-intent-retry]").click();
      await page.locator("[data-intent-retry]").waitFor({ state: "detached" });
      await waitFor(
        async () => (await input.inputValue()) === "",
        "recovered draft cleared",
      );
      assert.deepEqual(attempts.at(-1).body, frozen);
      assert.equal(
        (await messages(fixture)).filter((x) => x.text === "Lost response once")
          .length,
        1,
      );
      await idle();
      // A selection/text/reply change while the original post is held survives its success and reload.
      bug = await update(bug, { status: "in_progress" });
      await page.evaluate((id) => qa.board.show(id), fixture.task.id);
      await page.locator("#board-message-item-mode").click();
      await choose(active);
      await input.fill("Older pending selection");
      const gate = holdNextMessage();
      await send.click();
      await gate.started;
      const oldKey = attempts.at(-1).body.requestId;
      await choose(bug);
      await input.evaluate((el) => {
        el.disabled = false;
        el.value = "Newer bug draft";
        el.dispatchEvent(new Event("input", { bubbles: true }));
      });
      await page.locator(`[data-reply="${fixture.source.seq}"]`).click();
      gate.release();
      await idle();
      assert.equal(await input.inputValue(), "Newer bug draft");
      await page.reload();
      await input.waitFor();
      assert.equal(await input.inputValue(), "Newer bug draft");
      assert.equal((await draftValue()).primaryItem, bug.id);
      assert.equal((await draftValue()).replyTo, fixture.source.seq);
      await send.click();
      await waitFor(
        async () =>
          (await messages(fixture)).some((x) => x.text === "Newer bug draft"),
        "newer draft",
      );
      await idle();
      assert.notEqual(attempts.at(-1).body.requestId, oldKey);
      assert.equal(attempts.at(-1).body.workItems[0].itemId, bug.id);
      // A different project draft cannot be erased by the old completion.
      await page.locator("#board-message-item-mode").click();
      await choose(active);
      await input.fill("Pending project A");
      const projectGate = holdNextMessage();
      await send.click();
      await projectGate.started;
      await page.evaluate((id) => qa.board.show(id), other.task.id);
      await input.fill("Project B unsent");
      projectGate.release();
      await waitFor(
        async () =>
          (await messages(fixture)).some((x) => x.text === "Pending project A"),
        "project A",
      );
      assert.equal(await input.inputValue(), "Project B unsent");
      await page.goto(`${origin}/?task=${other.task.id}`);
      await input.waitFor();
      assert.equal(await input.inputValue(), "Project B unsent");
      await page.locator("#board-message-item-mode").click();
      assert.deepEqual(
        await page
          .locator("#board-message-item option")
          .evaluateAll((nodes) => nodes.map((n) => n.value)),
        [""],
      );
      const emptyProjectBefore = attempts.length;
      await send.click();
      await idle();
      assert.equal(attempts.length, emptyProjectBefore);
      // Explicit classified Work still requires an actual order.
      await selectAuditKind(page, "work");
      await page.locator("#board-primary-task").fill(other.task.id);
      await page.locator("#board-primary-item").fill(other.primary.id);
      await page.locator("#board-primary-revision").fill("1");
      const workBefore = attempts.length;
      await send.click();
      await idle();
      assert.equal(attempts.length, workBefore);
      assert.match(await page.locator("#notice").textContent(), /exact order/);
      await page.evaluate((id) => qa.board.show(id), fixture.task.id);
      await page.locator("#board-message-item-mode").click();
      await choose(active);
      await page.setViewportSize({ width: 390, height: 740 });
      await page.locator("#notice").evaluate((el) => (el.textContent = ""));
      await page.screenshot({
        path: `/tmp/item-message-8108-${name}-narrow.png`,
      });
      assert.equal(
        await page.evaluate(
          () => document.documentElement.scrollWidth > innerWidth,
        ),
        false,
      );
      const box = await page.locator("#board-message-item").boundingBox();
      assert.ok(box.width > 100 && box.x + box.width <= 390);
      await api("DELETE", `/v1/tasks/${fixture.task.id}`);
      await page.evaluate((id) => qa.board.show(id), fixture.task.id);
      assert.equal(await send.count(), 0);
      await page.evaluate(async ({ kind, id }) => qa.showItems(kind, id), {
        kind: "feature",
        id: fixture.task.id,
      });
      assert.equal(
        await page.locator(`[data-item-message="${active.id}"]`).isDisabled(),
        true,
      );
      assert.deepEqual(errors, []);
      results.push({
        engine: name,
        passed: true,
        checks:
          "direct Bug/Feature, picker, empty, primary/recipient, stale, loss+reload+exact retry, pending item/reply/project, new key, Work order, keyboard/IME, narrow, closed",
      });
    } finally {
      await browser.close();
    }
  }
  console.log(JSON.stringify(results, null, 2));
} finally {
  if (web) {
    web.closeAllConnections();
    await new Promise((resolve) => web.close(resolve));
  }
  if (backend) {
    backend.kill("SIGTERM");
    await once(backend, "exit").catch(() => {});
  }
  await rm(state, { recursive: true, force: true });
}
