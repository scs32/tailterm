// Independent acceptance for wi_65e8fd62e46a4eb8, build order #500 / QA #504.
// Real compiled hub/tt, disposable SQLite/vault/browser state; no live records,
// inherited Tailterm identity, runtime relay, SSH, or tmux sessions.
import { chromium, webkit, expect } from "@playwright/test";
import { createServer } from "node:http";
import { once } from "node:events";
import { spawn, execFile } from "node:child_process";
import { promisify } from "node:util";
import { mkdtemp, mkdir, readFile, writeFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import assert from "node:assert/strict";

const exec = promisify(execFile), root = process.cwd();
const state = await mkdtemp(path.join(tmpdir(), "tailterm-board-decisions-"));
const artifacts = path.join(root, ".build/board-decisions");
await mkdir(artifacts, { recursive: true });
// Omit HOME as well: the CLI must not read the host's hub credential file.
const fixtureEnv = Object.fromEntries(Object.entries(process.env).filter(([key]) =>
  !/^(TAILTERM_|CODEX_|TT_|TMUX)/.test(key) && key !== "HOME"));
const hubBinary = path.join(state, "tailterm-hub"), ttBinary = path.join(state, "tt");
let backend, web, browser, diagnosticPage;
const results = [], failures = [], screenshots = [], requests = [], frozenReads = new Map();
let fault = null;
const engineNames = new Set((process.env.BOARD_DECISIONS_ENGINES || "chromium,webkit").split(","));
assert.ok(engineNames.size > 0 && [...engineNames].every(name => ["chromium", "webkit"].includes(name)), "Choose chromium and/or webkit for the isolated fixture");
const pause = ms => new Promise(resolve => setTimeout(resolve, ms));

async function freePort() {
  const server = createServer();
  server.listen(0, "127.0.0.1");
  await once(server, "listening");
  const port = server.address().port;
  await new Promise(resolve => server.close(resolve));
  return port;
}
let hub, origin;
function frozenResponse(url, freeze) {
  if (!freeze) return null;
  if (url.pathname.endsWith("/decisions")) {
    const matching = freeze.decisions.filter(d => d.request.seq > Number(url.searchParams.get("after") || 0));
    const limit = Math.min(100, Number(url.searchParams.get("limit") || 100));
    return { decisions: matching.slice(0, limit), ...(matching.length > limit ? { nextAfter: matching[limit - 1].request.seq } : {}) };
  }
  if (url.pathname.endsWith("/messages")) {
    const matching = freeze.messages.filter(m => m.seq > Number(url.searchParams.get("after") || 0));
    const limit = Math.min(200, Number(url.searchParams.get("limit") || 50));
    return { messages: url.searchParams.get("latest") === "1" ? matching.slice(-limit) : matching.slice(0, limit) };
  }
  return null;
}
async function api(method, route, body, expected = 200) {
  // Synthetic history setup respects the real server's 20 writes/sec bucket.
  // Do not retry mutations implicitly: retry behavior is itself under test.
  if (method !== "GET") await pause(60);
  const response = await fetch(hub + route, {
    method, signal: AbortSignal.timeout(10000),
    headers: body === undefined ? undefined : { "content-type": "application/json" },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const text = await response.text(), data = text ? JSON.parse(text) : null;
  assert.equal(response.status, expected, `${method} ${route}: ${text}`);
  return data;
}
async function project(name) {
  const task = await api("POST", "/v1/tasks", { name, orchestrator: "fixture-worker" }, 201);
  const agent = await api("POST", `/v1/tasks/${task.id}/agents`, {
    name: "fixture-worker", host: "synthetic.invalid", session: "unused-fixture-session",
    runtime: "codex", cwd: state,
  }, 201);
  return { task, agent };
}
const question = label => ({
  question: `${label}: Which rollout should proceed?`,
  options: [
    { id: "staged", label: "Staged rollout", description: "Verify a small synthetic group before enabling the change broadly." },
    { id: "all", label: "Everyone at once", description: "Enable the change for all synthetic users after testing." },
    { id: "defer", label: "Wait for more evidence", description: "Keep the existing behavior while collecting another round of results." },
    { id: "custom", label: "Use the named custom rollout", description: "Select this listed option whose valid identifier is custom; it is distinct from a free-text answer." },
  ],
  recommendedOptionId: "staged", recommendationReason: "A staged rollout limits the impact of a problem.",
});
async function ask(p, key, body, { base = hub, fail = false } = {}) {
  const file = path.join(state, `request-${key.replace(/[^a-z0-9]/gi, "-")}.json`);
  await writeFile(file, JSON.stringify(body));
  try {
    const output = await exec(ttBinary, ["ask", "--request-id", key, "--file", file, "--json"], {
      cwd: state, timeout: 15000,
      env: { ...fixtureEnv, TAILTERM_HUB: base, TAILTERM_TASK: p.task.id,
        TAILTERM_AGENT: p.agent.id, TAILTERM_AGENT_NAME: p.agent.name,
        TT_TMUX_SOCKET: "board-decisions-never-started" },
    });
    assert.equal(fail, false, "CLI unexpectedly succeeded after a deliberately lost response");
    return JSON.parse(output.stdout);
  } catch (error) {
    if (!fail || !error.code) throw error;
    assert.match(error.stderr, /retry|recover|request/i, "CLI must offer recovery guidance");
    return null;
  }
}
async function records(p, limit = 100) {
  const all = []; let after = 0;
  for (;;) {
    const page = await api("GET", `/v1/tasks/${p.task.id}/decisions?after=${after}&limit=${limit}`);
    all.push(...page.decisions);
    if (!page.nextAfter) return all;
    assert.ok(page.nextAfter > after, "decision pagination must advance");
    after = page.nextAfter;
  }
}
async function record(p, seq) { return (await records(p)).find(d => d.request.seq === seq); }
async function messages(p) {
  const all = []; let after = 0;
  for (;;) {
    const page = (await api("GET", `/v1/tasks/${p.task.id}/messages?after=${after}&limit=200`)).messages;
    all.push(...page);
    if (page.length < 200) return all;
    assert.ok(page.at(-1).seq > after, "message pagination must advance");
    after = page.at(-1).seq;
  }
}
async function assertAnswer(p, request, { optionId, text } = {}) {
  const result = await record(p, request.seq);
  assert.ok(result.answer, "answer must persist on the real hub");
  assert.equal(result.answer.from.agentId || "", "", "answer must be human authored");
  assert.equal(result.answer.to, p.agent.id, "answer must address the requesting worker");
  assert.equal(result.answer.replyTo, request.seq);
  assert.equal(result.answer.decisionAnswer.requestSeq, request.seq);
  assert.equal(result.answer.decisionAnswer.optionId || "", optionId || "");
  assert.equal(result.answer.decisionAnswer.text || "", text || "");
  assert.ok(result.answer.postReceipt?.requestId, "answer needs a recovery receipt");
  const answers = (await messages(p)).filter(m => m.decisionAnswer?.requestSeq === request.seq);
  assert.equal(answers.length, 1, "one request must produce exactly one human reply");
  return result.answer;
}

const html = `<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<link rel="stylesheet" href="/client/style.css"></head><body><div id="app"><div id="workspace"><aside><h2>Fixture machines</h2></aside><main><header></header><div class="terminal-shell" hidden></div></main></div><p id="notice" role="status" hidden></p></div>
<script type="module">
import {createBoardView} from '/client/board-view.js';
import {createHubClient} from '/client/hub-client.js';
import {createCachedHubClient} from '/client/cached-hub-client.js';
import {createHubReadCache} from '/client/hub-read-cache.js';
import * as vault from '/client/local-vault.js';
import {setupModes} from '/client/modes.js';
const params=new URLSearchParams(location.search), state={online:params.get('offline')!=='1',writes:[],notices:[],pointerEvents:[],messageReads:[]}, pendingReads=new Set();
let pressedProject;
for(const type of ['pointerdown','pointerup','click'])document.addEventListener(type,event=>{
  const target=event.target.closest?.('[data-board-task]');
  if(type==='pointerdown')pressedProject=target;
  if(target||pressedProject)state.pointerEvents.push({type,target:target?.dataset.boardTask||'',pressed:pressedProject?.dataset.boardTask||'',pressedConnected:pressedProject?.isConnected||false,selected:document.querySelector('[data-board-task][aria-pressed="true"]')?.dataset.boardTask||''});
  if(type==='click')pressedProject=null;
},true);
await vault.localAPI('/unlock','POST',{username:'synthetic-board-decisions',password:'isolated decision fixture passphrase'});
const live=createHubClient({baseURL:location.origin,fetchImpl:(url,init)=>{
  if(init.method!=='GET')state.writes.push({url,body:JSON.parse(init.body||'{}')});
  if(!state.online)return Promise.reject(Error('Synthetic hub offline'));
  if(init.method!=='GET'||new URL(url).pathname.endsWith('/events'))return fetch(url,init);
  let work;
  work=fetch(url,init).then(async response=>{const text=await response.text();if(new URL(url).pathname.endsWith('/messages')){state.messageReads.push({url,text});if(state.messageReads.length>30)state.messageReads.shift()}return {status:response.status,statusText:response.statusText,headers:response.headers,text:async()=>text}}).finally(()=>pendingReads.delete(work));
  pendingReads.add(work);return work;
}});
const client=createCachedHubClient({client:live,cache:createHubReadCache(vault.hubReadCachePersistence()),online:()=>state.online,refreshMs:1000});
const noop=()=>{}, board=createBoardView({client:()=>client,getTabs:()=>[],activate:noop,notice:text=>{state.notices.push(text);document.querySelector('#notice').textContent=text},addAgent:noop,revealAgent:noop,attachTask:noop,newTask:noop,configure:noop});
const modes=setupModes({header:document.querySelector('header'),main:document.querySelector('main'),onChange:(mode,view)=>{board.hide();view.replaceChildren();if(mode==='board'){board.mount(view);void board.show(params.get('task'))}}});
modes.set('board');
window.qa={board,client,live,vault,state,modes,
  async refresh(){await client.refreshConnection();await board.reload()},
  async online(value){state.online=value;await client.refreshConnection();await board.reload()},
  async show(id){modes.set('board');await board.show(id)},
  async beforeReload(){board.hide();client.dispose();await Promise.allSettled([...pendingReads])},
};
</script></body></html>`;

async function serve(req, res) {
  try {
    const url = new URL(req.url, origin || "http://127.0.0.1");
    if (url.pathname === "/") {
      res.writeHead(200, { "content-type": "text/html" }); res.end(html); return;
    }
    if (url.pathname.startsWith("/v1/")) {
      const chunks = [];
      for await (const chunk of req) chunks.push(chunk);
      const body = Buffer.concat(chunks), method = req.method;
      const requestLog = { method, path: req.url, body: body.length ? JSON.parse(body) : null };
      requests.push(requestLog);
      const task = url.pathname.match(/^\/v1\/tasks\/([^/]+)/)?.[1];
      const freeze = frozenReads.get(task);
      if (method === "GET" && freeze) {
        const snapshot = frozenResponse(url, freeze);
        if (snapshot) { res.writeHead(200, { "content-type": "application/json" }); res.end(JSON.stringify(snapshot)); return; }
      }
      const hit = fault && method === "POST" && url.pathname === fault.path;
      const activeFault = hit ? fault : null;
      if (activeFault) fault = null;
      if (method === "POST" && !activeFault && freeze) frozenReads.delete(task);
      if (activeFault?.freeze) frozenReads.set(task, activeFault.freeze);
      if (activeFault?.winner) {
        // The owner POST is already in flight when an independent writer wins.
        activeFault.committed = await api("POST", url.pathname, activeFault.winner, 201);
      }
      const response = await fetch(hub + req.url, {
        method, signal: AbortSignal.timeout(35000),
        headers: req.headers["content-type"] ? { "content-type": req.headers["content-type"] } : undefined,
        body: ["GET", "HEAD"].includes(method) ? undefined : body,
      });
      const text = await response.text();
      requestLog.status = response.status;
      // Also mask a read that was in flight when the write began, so a live
      // event cannot resolve the card before the deliberate retry is exercised.
      const snapshot = method === "GET" && frozenResponse(url, frozenReads.get(task));
      if (snapshot) { res.writeHead(200, { "content-type": "application/json" }); res.end(JSON.stringify(snapshot)); return; }
      if (activeFault?.wait) {
        activeFault.status = response.status;
        if (!activeFault.winner) activeFault.committed = JSON.parse(text);
        await activeFault.wait;
        frozenReads.delete(task);
        res.writeHead(response.status, { "content-type": "application/json" }); res.end(text); return;
      }
      if (activeFault && !activeFault.winner) {
        assert.equal(response.status, 201, "fault must happen only after a committed write");
        activeFault.committed = JSON.parse(text);
        if (activeFault.freeze) {
          // A gateway loses the successful upstream response. Return an explicit
          // transport failure so browser socket recovery cannot silently retry
          // before the owner's deliberate retry is exercised below.
          res.writeHead(503, { "content-type": "application/json" });
          res.end(JSON.stringify({ error: "Synthetic response lost after commit; retry with the same key." })); return;
        }
        // Destroy the socket only after consuming the hub's committed response.
        res.destroy(); return;
      }
      res.writeHead(response.status, { "content-type": "application/json" }); res.end(text); return;
    }
    const file = path.resolve(root, "." + decodeURIComponent(url.pathname));
    if (!file.startsWith(root + path.sep)) throw Error("invalid static path");
    // Match Vite's asset-URL import used by the real local-vault dependency.
    if (url.searchParams.has("url")) {
      res.writeHead(200, { "content-type": "text/javascript" });
      res.end(`export default ${JSON.stringify(url.pathname)};`); return;
    }
    res.writeHead(200, { "content-type": file.endsWith(".js") ? "text/javascript" : file.endsWith(".css") ? "text/css" : "application/octet-stream" });
    res.end(await readFile(file));
  } catch (error) {
    if (!res.destroyed) { res.writeHead(500, { "content-type": "application/json" }); res.end(JSON.stringify({ error: error.message })); }
  }
}

// Stable selectors are coordinated with the UI owner; assertions below check
// visible behavior and durable hub state, without mutating application internals.
const card = (page, seq) => page.locator(`[data-decision-request="${seq}"]`);
const option = (page, seq, id) => card(page, seq).locator(`[data-decision-option="${id}"]`);
const custom = (page, seq) => card(page, seq).locator("[data-decision-custom]");
const customChoice = (page, seq) => card(page, seq).locator("[data-decision-custom-choice]");
const explanation = (page, seq) => card(page, seq).locator("[data-decision-explanation]");
const submit = (page, seq) => card(page, seq).locator("[data-decision-submit]");
const answerView = (page, seq) => card(page, seq).locator("[data-decision-answer]");
const errorView = (page, seq) => card(page, seq).locator("[data-decision-error]");
async function writeCustom(page, seq, text) {
  await customChoice(page, seq).check();
  await custom(page, seq).fill(text);
}
async function visibleAnswer(page, seq, text) {
  await expect(answerView(page, seq)).toContainText(text);
  const history = card(page, seq).locator("xpath=ancestor::details[1]");
  if (await history.count() && !(await history.evaluate(el => el.open)))
    await history.locator("summary").click();
  await expect(answerView(page, seq)).toBeVisible();
}
async function choose(page, seq, id) {
  const control = option(page, seq, id);
  if (await control.evaluate(el => el.matches('input[type="radio"],input[type="checkbox"]'))) await control.check();
  else await control.click();
}
async function selected(page, seq, id) {
  await expect.poll(() => option(page, seq, id).evaluate(el => el.checked === true || el.getAttribute("aria-checked") === "true" || el.getAttribute("aria-pressed") === "true")).toBe(true);
}
async function snapshotReads(p) { return { decisions: await records(p), messages: await messages(p) }; }
function answerAttempts(p, seq) {
  return requests.filter(r => r.method === "POST" && r.path === `/v1/tasks/${p.task.id}/decisions/${seq}/answer`);
}
async function geometry(page, seq) {
  await expect(async () => {
  const bounds = await page.evaluate(seq => {
    const el = document.querySelector(`[data-decision-request="${seq}"]`);
    if (!el) return null;
    const rect = el.getBoundingClientRect();
    return { width: innerWidth, card: { left: rect.left, right: rect.right, width: rect.width },
      controls: [...el.querySelectorAll('input,textarea,button,label')].map(control => {
        const r = control.getBoundingClientRect(); return { tag: control.tagName, left: r.left, right: r.right, width: r.width, height: r.height };
      }) };
  }, seq);
  assert.ok(bounds && bounds.card.width > 0 && bounds.card.left >= 0 && bounds.card.right <= bounds.width + 1, `decision card must fit the mobile viewport: ${JSON.stringify(bounds)}`);
  for (const control of bounds.controls.filter(c => c.width || c.height)) {
    assert.ok(control.left >= -1 && control.right <= bounds.width + 1, `${control.tag} must not clip horizontally`);
    assert.ok(control.width > 0 && control.height > 0, `${control.tag} must have a usable box`);
  }
  }).toPass({ timeout: 5000 });
}
async function open(page, p, offline = false) {
  await page.evaluate(async () => { if (window.qa) await qa.beforeReload(); });
  await page.goto(`${origin}/?task=${p.task.id}${offline ? "&offline=1" : ""}`);
  await page.waitForFunction(() => !!window.qa);
  await expect(page.locator(".board-head h2")).toHaveText(p.task.name);
}
async function screenshot(page, name) {
  await page.screenshot({ path: path.join(artifacts, `${name}.png`), fullPage: false });
  screenshots.push(`${name}.png`);
}
async function check(name, fn) {
  try { await fn(); results.push({ name, pass: true }); console.log(`PASS ${name}`); }
  catch (error) {
    results.push({ name, pass: false, error: error.stack }); failures.push(name);
    console.error(`FAIL ${name}: ${error.stack}`);
    console.error("Recent synthetic writes:", JSON.stringify(requests.filter(r => r.method !== "GET").slice(-6)));
    if (diagnosticPage && !diagnosticPage.isClosed()) {
      const interaction = await diagnosticPage.evaluate(() => window.qa?.state.pointerEvents.slice(-18)).catch(() => null);
      results.at(-1).interaction = interaction;
      console.error("Recent project pointer phases:", JSON.stringify(interaction));
    }
    if (diagnosticPage && !diagnosticPage.isClosed())
      await screenshot(diagnosticPage, `failure-${name.replace(/[^a-z0-9]/gi, "-")}`).catch(() => {});
  } finally { fault = null; frozenReads.clear(); }
}

try {
  await Promise.all([
    exec("go", ["build", "-o", hubBinary, "./cmd/tailterm-hub"], { cwd: path.join(root, "hub") }),
    exec("go", ["build", "-o", ttBinary, "./cmd/tt"], { cwd: path.join(root, "hub") }),
  ]);
  hub = `http://127.0.0.1:${await freePort()}`;
  backend = spawn(hubBinary, { env: { ...fixtureEnv, TAILTERM_STATE: state, TAILTERM_DEV_LISTEN: new URL(hub).host, TAILTERM_TCP_LISTEN: "" }, stdio: "ignore" });
  let ready = false;
  for (let i = 0; i < 100; i++) {
    try { await api("GET", "/v1/tasks"); ready = true; break; }
    catch { await pause(100); }
  }
  assert.ok(ready, "isolated hub failed to start");
  web = createServer(serve); web.listen(0, "127.0.0.1"); await once(web, "listening");
  origin = `http://127.0.0.1:${web.address().port}`;

  for (const engine of [chromium, webkit].filter(engine => engineNames.has(engine.name()))) {
    const name = engine.name(), p = await project(`${name} Decision project`), other = await project(`${name} Other project`);
    const questions = {};
    for (const label of ["selected", "custom", "option-custom", "offline", "lost", "changed", "remote", "concurrent", "delayed-success", "delayed-conflict", "closed"])
      questions[label] = await ask(p, `${name}:${label}.v1`, question(`${name} ${label}`));
    await check(`${name}: real CLI request, exact replay, legacy text and lost-response recovery`, async () => {
      const replay = await ask(p, `${name}:selected.v1`, question(`${name} selected`));
      assert.deepEqual(replay, questions.selected);
      assert.deepEqual(replay.decisionRequest, question(`${name} selected`));
      for (const value of [replay.decisionRequest.question, ...replay.decisionRequest.options.flatMap(o => [o.label, o.description]), replay.decisionRequest.recommendationReason])
        assert.ok(replay.text.includes(value), "legacy inbox text must carry all decision information");
      const lost = { path: `/v1/tasks/${other.task.id}/decisions` }; fault = lost;
      await ask(other, `${name}:cli-lost.v1`, question("CLI lost response"), { base: origin, fail: true });
      assert.ok(lost.committed, "proxy must have consumed the committed message");
      const recovered = await ask(other, `${name}:cli-lost.v1`, question("CLI lost response"), { base: origin });
      assert.deepEqual(recovered, lost.committed);
      assert.equal((await records(other)).length, 1);
    });
    // Decision discovery must be independent of the Board's latest-200 preview.
    for (let i = 0; i < 205; i++) await api("POST", `/v1/tasks/${p.task.id}/messages`, { text: `Legacy ordinary message ${i}` }, 201);
    await api("POST", `/v1/tasks/${p.task.id}/messages`, { text: "An ordinary reply is not a decision answer", replyTo: questions.selected.seq }, 201);
    console.log(`${name}: synthetic history seeded; launching browser`);
    browser = await engine.launch();
    const context = await browser.newContext({ viewport: { width: 1440, height: 1000 }, acceptDownloads: true });
    await context.route("**/*", route => new URL(route.request().url()).origin === origin ? route.continue() : route.abort());
    const page = await context.newPage(), pageErrors = [];
    diagnosticPage = page;
    page.on("pageerror", error => { pageErrors.push(error.message); console.error(`${name} browser error: ${error.stack}`); });
    page.setDefaultTimeout(7000);
    await open(page, p);

    await check(`${name}: old pending questions, explained choices, one recommendation, no auto-answer`, async () => {
      const q = questions.selected;
      await expect(card(page, q.seq)).toBeVisible();
      await expect(page.locator("#board-messages")).not.toContainText(q.decisionRequest.question);
      for (const text of [q.decisionRequest.question, ...q.decisionRequest.options.flatMap(o => [o.label, o.description]), q.decisionRequest.recommendationReason])
        await expect(card(page, q.seq)).toContainText(text);
      await expect(card(page, q.seq).getByText("Recommended", { exact: true })).toHaveCount(1);
      assert.equal((await records(p)).filter(d => d.answer).length, 0);
      assert.equal(await page.evaluate(() => qa.state.writes.length), 0);
      await screenshot(page, `${name}-pending-desktop`);
    });
    await check(`${name}: physical rail press survives a refreshed Board and keyboard activation works`, async () => {
      try {
        await page.locator(`[data-board-task="${other.task.id}"]`).hover();
        await page.mouse.down();
        const marker = `${name} synthetic presentation refresh while the pointer is held`;
        await api("POST", `/v1/tasks/${p.task.id}/messages`, { text: marker }, 201);
        await page.evaluate(() => qa.refresh());
        await page.waitForFunction(marker => qa.state.messageReads.some(read => read.text.includes(marker)), marker);
        await page.mouse.up();
        await expect(page.locator(".board-head h2")).toHaveText(other.task.name);
        await page.locator(`[data-board-task="${p.task.id}"]`).focus();
        await page.keyboard.press("Enter");
        await expect(page.locator(".board-head h2")).toHaveText(p.task.name);
        await expect(page.locator("#board-messages")).toContainText(marker);
      } finally {
        await page.mouse.up();
        await page.evaluate(id => qa.show(id), p.task.id);
      }
    });
    await check(`${name}: deliberate keyboard selection and explanation persist after reload`, async () => {
      const q = questions.selected;
      await option(page, q.seq, "all").focus(); await page.keyboard.press("Space");
      await selected(page, q.seq, "all");
      await explanation(page, q.seq).fill("Ship after the synthetic review.");
      assert.equal((await record(p, q.seq)).answer, undefined, "selection alone cannot answer");
      await submit(page, q.seq).focus(); await page.keyboard.press("Enter");
      await visibleAnswer(page, q.seq, "Everyone at once");
      await assertAnswer(p, q, { optionId: "all", text: "Ship after the synthetic review." });
      await open(page, p);
      await visibleAnswer(page, q.seq, "Ship after the synthetic review.");
    });
    await check(`${name}: custom answer preserves literal text and survives browser reload`, async () => {
      const q = questions.custom, text = "Use a smaller trial first.\nKeep <script>synthetic text</script> as plain text.";
      await writeCustom(page, q.seq, text);
      assert.equal((await record(p, q.seq)).answer, undefined);
      await submit(page, q.seq).click();
      await visibleAnswer(page, q.seq, "Use a smaller trial first.");
      await assertAnswer(p, q, { text });
      assert.equal(await card(page, q.seq).locator("script").count(), 0);
      await open(page, p);
      await visibleAnswer(page, q.seq, "Keep <script>synthetic text</script> as plain text.");
    });
    await check(`${name}: permitted custom option ID remains distinct from a free-text answer`, async () => {
      const q = questions["option-custom"], text = "This explanation belongs to the listed option.";
      await choose(page, q.seq, "custom"); await explanation(page, q.seq).fill(text);
      await submit(page, q.seq).click();
      await visibleAnswer(page, q.seq, "Use the named custom rollout");
      await assertAnswer(p, q, { optionId: "custom", text });
    });
    await check(`${name}: offline failure retains selection/draft through refresh and navigation without replay`, async () => {
      const q = questions.offline, text = "Wait for the synthetic network to return.";
      const history = page.locator(".decision-history"), panel = page.locator("#board-decisions");
      if (!(await history.evaluate(el => el.open))) await history.locator("summary").click();
      await page.locator("#board-text").focus();
      await expect.poll(() => panel.evaluate(el => {
        el.scrollTop = Math.min(180, el.scrollHeight - el.clientHeight); return el.scrollTop;
      })).toBeGreaterThan(0);
      const scroll = await panel.evaluate(el => {
        el.scrollTop = Math.min(180, el.scrollHeight - el.clientHeight); return el.scrollTop;
      });
      assert.ok(scroll > 0, "fixture must exercise a scrollable decision panel");
      await page.evaluate(() => qa.board.reload());
      await expect.poll(() => history.evaluate(el => el.open)).toBe(true);
      await expect.poll(() => panel.evaluate(el => el.scrollTop)).toBe(scroll);
      await page.evaluate(id => qa.show(id), other.task.id);
      await page.evaluate(id => qa.show(id), p.task.id);
      await choose(page, q.seq, "defer"); await explanation(page, q.seq).fill(text);
      await page.evaluate(() => qa.online(false));
      await expect(page.locator(".hub-sync-status")).toContainText(/offline/i);
      if (await submit(page, q.seq).isEnabled()) {
        await submit(page, q.seq).click();
        await expect(errorView(page, q.seq)).toContainText(/offline|failed|connect/i);
      }
      await page.evaluate(async id => { await qa.board.reload(); await qa.show(id); }, other.task.id);
      await page.evaluate(id => qa.show(id), p.task.id);
      await selected(page, q.seq, "defer"); await expect(explanation(page, q.seq)).toHaveValue(text);
      await page.evaluate(() => { qa.modes.set("terminals"); qa.modes.set("board"); });
      await selected(page, q.seq, "defer"); await expect(explanation(page, q.seq)).toHaveValue(text);
      assert.equal((await record(p, q.seq)).answer, undefined);
      await screenshot(page, `${name}-offline-draft`);
      const attempts = answerAttempts(p, q.seq).length;
      await page.evaluate(() => qa.online(true)); await pause(650);
      assert.equal(answerAttempts(p, q.seq).length, attempts, "reconnect must never replay a failed answer");
      assert.equal((await record(p, q.seq)).answer, undefined);
      await selected(page, q.seq, "defer"); await expect(explanation(page, q.seq)).toHaveValue(text);
      await submit(page, q.seq).click();
      await visibleAnswer(page, q.seq, "Wait for more evidence");
      await assertAnswer(p, q, { optionId: "defer", text });
    });
    await check(`${name}: committed response loss retains stable retry key and creates one answer`, async () => {
      const q = questions.lost, text = "Answer whose first response disappears.";
      await writeCustom(page, q.seq, text);
      const loss = { path: `/v1/tasks/${p.task.id}/decisions/${q.seq}/answer`, freeze: await snapshotReads(p) }; fault = loss;
      await submit(page, q.seq).click();
      await expect(errorView(page, q.seq)).toContainText(/failed|network|load|fetch|retry/i);
      assert.ok(loss.committed, "network failure must follow a real committed answer");
      await expect(custom(page, q.seq)).toHaveValue(text);
      await page.evaluate(() => qa.board.reload());
      await page.evaluate(id => qa.show(id), other.task.id); await page.evaluate(id => qa.show(id), p.task.id);
      await expect(custom(page, q.seq)).toHaveValue(text);
      await submit(page, q.seq).click();
      await expect.poll(() => answerAttempts(p, q.seq).length).toBe(2);
      await visibleAnswer(page, q.seq, text);
      const attempts = answerAttempts(p, q.seq);
      assert.equal(attempts.length, 2);
      assert.deepEqual(attempts[0].body, attempts[1].body, "unchanged retry must retain its exact payload and key");
      assert.deepEqual(await assertAnswer(p, q, { text }), loss.committed);
    });
    await check(`${name}: changed ambiguous draft uses a new key and shows the original winner`, async () => {
      const q = questions.changed, original = "Original saved explanation.";
      await choose(page, q.seq, "staged"); await explanation(page, q.seq).fill(original);
      const loss = { path: `/v1/tasks/${p.task.id}/decisions/${q.seq}/answer`, freeze: await snapshotReads(p) }; fault = loss;
      await submit(page, q.seq).click(); await expect(errorView(page, q.seq)).toContainText(/failed|network|load|fetch|retry/i);
      await explanation(page, q.seq).fill("A later change must not overwrite the winner.");
      await submit(page, q.seq).click();
      await visibleAnswer(page, q.seq, original);
      const attempts = answerAttempts(p, q.seq);
      assert.equal(attempts.length, 2); assert.notEqual(attempts[0].body.requestId, attempts[1].body.requestId);
      await assertAnswer(p, q, { optionId: "staged", text: original });
    });
    await check(`${name}: remote resolution refreshes an unchanged immutable message preview`, async () => {
      const q = questions.remote;
      await choose(page, q.seq, "defer"); await explanation(page, q.seq).fill("Local draft before another owner answers.");
      await api("POST", `/v1/tasks/${p.task.id}/decisions/${q.seq}/answer`, { requestId: `${name}:remote-answer`, optionId: "all", text: "Remote owner won." }, 201);
      // No test-triggered refresh: normal events/polling must resolve stale state.
      await visibleAnswer(page, q.seq, "Remote owner won.");
      assert.equal(await submit(page, q.seq).count(), 0, "resolved card must not remain editable");
      await assertAnswer(p, q, { optionId: "all", text: "Remote owner won." });
    });
    await check(`${name}: an overlapping answer loses with409 and displays the concurrent winner`, async () => {
      const q = questions.concurrent;
      await choose(page, q.seq, "staged");
      const race = { path: `/v1/tasks/${p.task.id}/decisions/${q.seq}/answer`, winner: { requestId: `${name}:concurrent-winner`, optionId: "defer", text: "Another owner answered first." } }; fault = race;
      await submit(page, q.seq).click();
      await visibleAnswer(page, q.seq, "Another owner answered first.");
      assert.ok(race.committed);
      await assertAnswer(p, q, { optionId: "defer", text: "Another owner answered first." });
    });
    await check(`${name}: delayed201/409 completion cannot expand another project history`, async () => {
      const otherRequest = (await records(other))[0].request;
      await api("POST", `/v1/tasks/${other.task.id}/decisions/${otherRequest.seq}/answer`, {
        requestId: `${name}:other-answer`, optionId: "all", text: "Keep this other project history collapsed.",
      }, 201);
      for (const conflict of [false, true]) {
        const q = questions[conflict ? "delayed-conflict" : "delayed-success"];
        await page.locator(`[data-board-task="${other.task.id}"]`).click();
        await visibleAnswer(page, otherRequest.seq, "Keep this other project history collapsed.");
        await page.locator(".decision-history summary").click();
        await expect.poll(() => page.locator(".decision-history").evaluate(el => el.open)).toBe(false);
        await page.locator(`[data-board-task="${p.task.id}"]`).click();
        await choose(page, q.seq, "staged");
        let release;
        const delayed = { path: `/v1/tasks/${p.task.id}/decisions/${q.seq}/answer`,
          freeze: await snapshotReads(p), wait: new Promise(resolve => { release = resolve; }),
          ...(conflict ? { winner: { requestId: `${name}:delayed-winner`, optionId: "defer", text: "Source project concurrent winner." } } : {}),
        };
        fault = delayed;
        try {
          await submit(page, q.seq).click();
          await expect.poll(() => delayed.status).toBe(conflict ? 409 : 201);
          await page.locator(`[data-board-task="${other.task.id}"]`).click();
          await expect(page.locator(".board-head h2")).toHaveText(other.task.name);
          await expect.poll(() => page.locator(".decision-history").evaluate(el => el.open)).toBe(false);
          const completed = page.waitForResponse(response => new URL(response.url()).pathname === delayed.path && response.request().method() === "POST");
          const refreshed = conflict ? page.waitForResponse(response => new URL(response.url()).pathname === `/v1/tasks/${p.task.id}/decisions` && response.request().method() === "GET") : null;
          release(); await completed; if (refreshed) await refreshed;
          await page.evaluate(() => new Promise(resolve => setTimeout(resolve, 150)));
          await page.evaluate(() => qa.board.reload());
          await expect(page.locator(".board-head h2")).toHaveText(other.task.name);
          await expect.poll(() => page.locator(".decision-history").evaluate(el => el.open)).toBe(false);
          await page.locator(`[data-board-task="${p.task.id}"]`).click();
          await visibleAnswer(page, q.seq, conflict ? "Source project concurrent winner." : "Staged rollout");
          await assertAnswer(p, q, conflict ? { optionId: "defer", text: "Source project concurrent winner." } : { optionId: "staged" });
        } finally { release(); frozenReads.delete(p.task.id); }
      }
    });
    await check(`${name}: all decision pages are reachable beyond100 records`, async () => {
      const paged = await project(`${name} Paginated decisions`);
      for (let i = 0; i < 103; i++) await api("POST", `/v1/tasks/${paged.task.id}/decisions`, { ...question(`Pagination question ${i}`), agentId: paged.agent.id, requestId: `${name}:page-${i}` }, 201);
      const all = await records(paged, 2);
      assert.equal(all.length, 103); assert.equal(new Set(all.map(d => d.request.seq)).size, 103);
      await page.locator(`[data-board-task="${paged.task.id}"]`).click();
      await expect(card(page, all.at(-1).request.seq)).toContainText("Pagination question 102");
      await expect(page.locator("[data-decision-request]")).toHaveCount(103);
      assert.ok(requests.some(r => r.method === "GET" && r.path.startsWith(`/v1/tasks/${paged.task.id}/decisions?`) && Number(new URL(r.path, origin).searchParams.get("after")) > 0), "Board must fetch later decision pages");
      await page.evaluate(id => qa.show(id), p.task.id);
    });
    await check(`${name}: keyboard and390px controls fit long choices without clipping`, async () => {
      const mobile = await project(`${name} Mobile decisions`), long = question("Long mobile question ".repeat(8));
      long.options[0].label = "A deliberately long choice label that wraps naturally on the narrow mobile Board";
      long.options[0].description += " " + "UnbrokenSyntheticReference".repeat(8);
      const q = await ask(mobile, `${name}:mobile`, long);
      await page.setViewportSize({ width: 390, height: 844 });
      await page.locator(`[data-board-task="${mobile.task.id}"]`).click();
      await expect(card(page, q.seq)).toBeVisible(); await geometry(page, q.seq);
      await screenshot(page, `${name}-mobile-question`);
      await customChoice(page, q.seq).check();
      await custom(page, q.seq).focus(); await page.keyboard.type("A keyboard-entered custom answer.");
      await page.evaluate(() => qa.board.reload());
      await expect(custom(page, q.seq)).toHaveValue("A keyboard-entered custom answer.");
      await expect(custom(page, q.seq)).toBeFocused();
      await screenshot(page, `${name}-mobile-keyboard`);
      await submit(page, q.seq).focus(); await page.keyboard.press("Enter");
      await visibleAnswer(page, q.seq, "A keyboard-entered custom answer.");
      await assertAnswer(mobile, q, { text: "A keyboard-entered custom answer." });
      await page.setViewportSize({ width: 1440, height: 1000 });
      await page.evaluate(id => qa.show(id), p.task.id);
    });
    await check(`${name}: closed decisions/history remain readable with no enabled answer controls`, async () => {
      await page.setViewportSize({ width: 1440, height: 1000 });
      await page.evaluate(id => qa.show(id), p.task.id);
      await expect(page.locator(".board-head h2")).toHaveText(p.task.name);
      await api("DELETE", `/v1/tasks/${p.task.id}`);
      await page.evaluate(() => qa.refresh());
      await expect(page.locator(".board-head")).toContainText("Closed");
      await expect(card(page, questions.closed.seq)).toContainText(questions.closed.decisionRequest.question);
      assert.equal(await page.locator("#board-decisions input:enabled,#board-decisions textarea:enabled,#board-decisions button:enabled").count(), 0);
      await api("POST", `/v1/tasks/${p.task.id}/decisions/${questions.closed.seq}/answer`, { requestId: `${name}:closed-attempt`, optionId: "all" }, 409);
      const download = page.waitForEvent("download"); await page.locator("#board-download").click();
      const file = await download, history = JSON.parse(await readFile(await file.path(), "utf8"));
      assert.ok(history.messages.some(m => m.text === "Legacy ordinary message 0"));
      for (const q of Object.values(questions)) assert.deepEqual(history.messages.find(m => m.seq === q.seq)?.decisionRequest, q.decisionRequest);
      for (const r of (await records(p)).filter(r => r.answer))
        assert.deepEqual(history.messages.find(m => m.seq === r.answer.seq)?.decisionAnswer, r.answer.decisionAnswer);
      await screenshot(page, `${name}-closed-history`);
      await page.evaluate(() => qa.vault.profileSnapshot());
      await open(page, p, true);
      await expect(page.locator(".hub-sync-status")).toContainText(/offline/i);
      await visibleAnswer(page, questions.selected.seq, "Ship after the synthetic review.");
    });
    await check(`${name}: no uncaught browser errors`, async () => assert.deepEqual(pageErrors, []));
    await context.close(); await browser.close(); browser = null;
    frozenReads.clear(); fault = null;
  }
} catch (error) {
  console.error(`Fixture interrupted: ${error.stack}`);
  if (diagnosticPage && !diagnosticPage.isClosed())
    await screenshot(diagnosticPage, "fixture-interrupted").catch(() => {});
  results.push({ name: "fixture setup/execution", pass: false, error: error.stack });
  failures.push("fixture setup/execution");
  throw error;
} finally {
  await browser?.close();
  if (web) { web.closeAllConnections(); await new Promise(resolve => web.close(resolve)); }
  if (backend && backend.exitCode === null && backend.signalCode === null) { const stopped = once(backend, "exit"); backend.kill("SIGTERM"); await stopped; }
  await rm(state, { recursive: true, force: true });
  await writeFile(path.join(artifacts, "results.json"), JSON.stringify({ engines: [...engineNames], results, failures, screenshots }, null, 2));
  const escape = text => String(text).replace(/[&<>"']/g, char => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[char]);
  await writeFile(path.join(artifacts, "index.html"), `<!doctype html><meta charset="utf-8"><title>Board decision acceptance</title><style>body{font:16px system-ui;max-width:1100px;margin:32px auto;padding:16px}li{margin:8px 0}img{max-width:100%;border:1px solid #aaa}pre{white-space:pre-wrap}</style><h1>Board decision acceptance</h1><p>wi_65e8fd62e46a4eb8 · build #500 · independent QA #504. Real isolated hub and CLI; engines: ${escape([...engineNames].join(", "))}.</p><ul>${results.map(r => `<li>${r.pass ? "PASS" : "FAIL"} ${escape(r.name)}${r.error ? `<pre>${escape(r.error)}</pre>` : ""}</li>`).join("")}</ul>${screenshots.map(file => `<h2>${escape(file)}</h2><a href="${escape(file)}"><img src="${escape(file)}"></a>`).join("")}`);
}
assert.equal(failures.length, 0, `Board decision acceptance failures: ${failures.join(", ")}`);
