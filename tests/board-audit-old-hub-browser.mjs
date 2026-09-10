// New Board compatibility against the exact accepted A2 hub baseline. The
// fixture is disposable and proves that a capability 404 is an explicit
// legacy/non-snapshot path rather than a silent typed-message downgrade.
import { chromium, webkit } from "@playwright/test";
import { createServer } from "node:http";
import { once } from "node:events";
import { execFile, spawn } from "node:child_process";
import { promisify } from "node:util";
import { mkdir, mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import assert from "node:assert/strict";

const exec = promisify(execFile);
const baseline = "58f9185981625b148b30ad4b5070a1a658e17f77";
const root = process.cwd();
const state = await mkdtemp(path.join(tmpdir(), "tailterm-old-a2-board-"));
const source = path.join(state, "source");
const archive = path.join(state, "source.tar");
const binary = path.join(state, "tailterm-hub-a2");
const fixtureEnv = Object.fromEntries(
  Object.entries(process.env).filter(
    ([key]) => !/^(TAILTERM_|CODEX_|TT_|TMUX)/.test(key) && key !== "HOME",
  ),
);
let backend;
let web;

const pause = (ms) => new Promise((resolve) => setTimeout(resolve, ms));
async function freePort() {
  const server = createServer();
  server.listen(0, "127.0.0.1");
  await once(server, "listening");
  const port = server.address().port;
  await new Promise((resolve) => server.close(resolve));
  return port;
}
async function waitFor(check, label) {
  for (let attempt = 0; attempt < 200; attempt++) {
    if (await check()) return;
    await pause(25);
  }
  throw new Error(`Timed out waiting for ${label}`);
}

const html = `<!doctype html><html><head><meta charset="utf-8">
<link rel="stylesheet" href="/client/style.css"></head><body>
<main><div id="mode-view"></div></main><p id="notice" role="status"></p>
<script type="module">
import {createBoardView} from '/client/board-view.js';
import {createHubClient} from '/client/hub-client.js';
const task=new URLSearchParams(location.search).get('task');
const client=createHubClient({baseURL:location.origin,fetchImpl:(url,init)=>fetch(url,init)});
const noop=()=>{};
const board=createBoardView({client:()=>client,getTabs:()=>[],activate:noop,
  notice:text=>document.querySelector('#notice').textContent=text,addAgent:noop,
  settings:noop,revealAgent:noop,attachTask:noop,newTask:noop,configure:noop});
board.mount(document.querySelector('#mode-view'));
await board.show(task);
</script></body></html>`;

try {
  const archived = await exec("git", ["archive", "--format=tar", baseline], {
    cwd: root,
    encoding: null,
    maxBuffer: 64 * 1024 * 1024,
  });
  await writeFile(archive, archived.stdout);
  await mkdir(source);
  await exec("tar", ["-xf", archive, "-C", source]);
  await exec("go", ["build", "-o", binary, "./cmd/tailterm-hub"], {
    cwd: path.join(source, "hub"),
  });

  const hubPort = await freePort();
  const hub = `http://127.0.0.1:${hubPort}`;
  backend = spawn(binary, {
    env: {
      ...fixtureEnv,
      TAILTERM_STATE: path.join(state, "state"),
      TAILTERM_DEV_LISTEN: `127.0.0.1:${hubPort}`,
      TAILTERM_TCP_LISTEN: "",
    },
    stdio: "ignore",
  });
  await waitFor(
    () =>
      fetch(`${hub}/v1/tasks`)
        .then((response) => response.ok)
        .catch(() => false),
    "A2 baseline hub",
  );
  const create = await fetch(`${hub}/v1/tasks`, {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify({ name: "Exact A2 compatibility" }),
  });
  assert.equal(create.status, 201);
  const task = await create.json();
  const capability = await fetch(`${hub}/v1/capabilities`);
  assert.equal(
    capability.status,
    404,
    "baseline unexpectedly has B1 capability",
  );

  web = createServer(async (request, response) => {
    try {
      const url = new URL(request.url, "http://127.0.0.1");
      if (url.pathname === "/") {
        response.writeHead(200, { "content-type": "text/html" });
        response.end(html);
        return;
      }
      if (url.pathname.startsWith("/v1/")) {
        const body = [];
        for await (const chunk of request) body.push(chunk);
        const raw = Buffer.concat(body);
        const upstream = await fetch(hub + request.url, {
          method: request.method,
          headers: raw.length
            ? { "content-type": "application/json" }
            : undefined,
          body: ["GET", "HEAD"].includes(request.method) ? undefined : raw,
        });
        response.writeHead(upstream.status, {
          "content-type": upstream.headers.get("content-type") || "text/plain",
        });
        response.end(await upstream.text());
        return;
      }
      const file = path.resolve(root, "." + decodeURIComponent(url.pathname));
      if (!file.startsWith(root + path.sep)) throw new Error("invalid path");
      response.writeHead(200, {
        "content-type": file.endsWith(".js")
          ? "text/javascript"
          : file.endsWith(".css")
            ? "text/css"
            : "application/octet-stream",
      });
      response.end(await readFile(file));
    } catch (error) {
      response.writeHead(500, { "content-type": "application/json" });
      response.end(JSON.stringify({ error: error.message }));
    }
  });
  web.listen(0, "127.0.0.1");
  await once(web, "listening");
  const origin = `http://127.0.0.1:${web.address().port}`;

  for (const engine of [chromium, webkit]) {
    const browser = await engine.launch();
    try {
      const page = await browser.newPage();
      const errors = [];
      page.on("pageerror", (error) => errors.push(error.message));
      await page.goto(`${origin}/?task=${task.id}`);
      await page.locator("#board-download").waitFor();
      assert.match(
        await page.locator("#board-download").innerText(),
        /non-snapshot/i,
      );
      assert.equal(await page.locator("#board-audit-export").count(), 0);
      assert.equal(await page.locator(".board-audit-compose").count(), 0);
      await page.locator("#board-text").fill(`ordinary from ${engine.name()}`);
      await page.locator('#board-compose button[type="submit"]').click();
      await page
        .locator(".board-message", {
          hasText: `ordinary from ${engine.name()}`,
        })
        .waitFor();
      const downloadPromise = page.waitForEvent("download");
      await page.locator("#board-download").click();
      const download = await downloadPromise;
      const saved = await download.createReadStream();
      const chunks = [];
      for await (const chunk of saved) chunks.push(chunk);
      const document = JSON.parse(Buffer.concat(chunks));
      assert.equal(document.version, 1);
      assert.equal(document.snapshot, false);
      assert.equal(document.consistency, "non-snapshot-legacy");
      assert.deepEqual(errors, []);
      console.log(
        `${engine.name()}: exact A2 hub capability absence stayed explicit and legacy v1 was labelled non-snapshot.`,
      );
    } finally {
      await browser.close();
    }
  }
} finally {
  if (web) await new Promise((resolve) => web.close(resolve));
  if (backend && !backend.killed) {
    backend.kill("SIGTERM");
    await Promise.race([once(backend, "exit"), pause(2000)]).catch(() => {});
  }
  await rm(state, { recursive: true, force: true });
}
