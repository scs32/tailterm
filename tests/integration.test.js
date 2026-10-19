import test from "node:test";
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import {
  existsSync,
  mkdtempSync,
  readFileSync,
  realpathSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import os from "node:os";
import path from "node:path";
import { once } from "node:events";
import { generateKeyPairSync, createHash, randomUUID } from "node:crypto";
import { connect, createServer as createNetServer } from "node:net";
import { pathToFileURL } from "node:url";
import ssh2 from "ssh2";
const { Server, utils } = ssh2;
import WebSocket from "ws";
import { createStaticPreviewServer } from "../scripts/preview-static.mjs";
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
function listening(port) {
  return new Promise((resolve) => {
    const socket = connect(port, "127.0.0.1");
    socket.once("connect", () => {
      socket.destroy();
      resolve(true);
    });
    socket.once("error", () => resolve(false));
  });
}
const pair = generateKeyPairSync("rsa", {
  modulusLength: 2048,
  privateKeyEncoding: { type: "pkcs1", format: "pem" },
  publicKeyEncoding: { type: "spki", format: "pem" },
});
const hostKey = utils.parseKey(pair.privateKey);
const fingerprint =
  "SHA256:" +
  createHash("sha256")
    .update(hostKey.getPublicSSH())
    .digest("base64")
    .replace(/=+$/, "");
test("authenticated API and real SSH transport enforce vault, origin, host key and session boundaries", async () => {
  const dir = mkdtempSync(path.join(os.tmpdir(), "tailterm-api-"));
  let proc, port, origin;
  let command = "",
    size;
  // Each list-sessions exec answers with the next queued listing.
  const wholeRow = "main|2|1|$1|1700000000\n";
  const cutRow = "main|2|1\n";
  const listings = [];
  let listReads = 0;
  const ssh = new Server({ hostKeys: [pair.privateKey] }, (client) => {
    client.on("error", () => {});
    client
      .on("authentication", (ctx) => {
        if (ctx.username === "passworduser") {
          if (ctx.method === "password" && ctx.password === "fixture-password")
            ctx.accept();
          else ctx.reject(["password"]);
        } else if (ctx.username === "otpuser") {
          if (ctx.method === "keyboard-interactive")
            ctx.prompt(
              [{ prompt: "Verification code", echo: false }],
              (answers) =>
                answers[0] === "123456"
                  ? ctx.accept()
                  : ctx.reject(["keyboard-interactive"]),
            );
          else ctx.reject(["keyboard-interactive"]);
        } else
          ctx.method === "publickey" ? ctx.accept() : ctx.reject(["publickey"]);
      })
      .on("ready", () =>
        client.on("session", (accept) => {
          const session = accept();
          session.on("pty", (accept, reject, info) => {
            size = info;
            accept();
          });
          session.on("window-change", (accept, reject, info) => {
            size = info;
            accept?.();
          });
          session.on("shell", (accept) => {
            const stream = accept();
            stream.write("fixture ready\r\n");
            stream.on("data", (d) => stream.write(d));
          });
          session.on("exec", (accept, reject, info) => {
            command = info.command;
            const stream = accept();
            if (command.includes(" list-sessions -F ")) {
              listReads++;
              stream.write(listings.shift() ?? wholeRow);
              stream.exit(0);
              stream.end();
            } else stream.write("tmux attached\r\n");
          });
        }),
      );
  });
  ssh.listen(0, "127.0.0.1");
  await once(ssh, "listening");
  let cookie = "";
  async function request(url, method = "GET", body, headers = {}) {
    const r = await fetch(origin + "/api" + url, {
      method,
      headers: {
        origin,
        "content-type": "application/json",
        cookie,
        ...headers,
      },
      body: body === undefined ? undefined : JSON.stringify(body),
    });
    return {
      status: r.status,
      body: await r.json(),
      cookie: r.headers.get("set-cookie"),
    };
  }
  try {
    ({ proc, port, origin } = await startAppServer({
      dir,
      onSpawn: (child) => (proc = child),
    }));
    assert.equal((await request("/data")).status, 401);
    assert.equal(
      (
        await request(
          "/unlock",
          "POST",
          { password: "very strong test passphrase" },
          { origin: "https://evil.test" },
        )
      ).status,
      403,
    );
    const login = await request("/unlock", "POST", {
      password: "very strong test passphrase",
    });
    assert.equal(login.status, 200);
    assert.match(login.cookie, /HttpOnly/);
    assert.match(login.cookie, /SameSite=Strict/);
    cookie = login.cookie.split(";")[0];
    const key = await request("/keys", "POST", {
      name: "fixture",
      privateKey: pair.privateKey,
    });
    assert.equal(key.status, 200);
    assert.ok(!JSON.stringify(key.body).includes("PRIVATE KEY"));
    const exported = await request(`/keys/${key.body.keys[0].id}/public-key`);
    assert.equal(exported.status, 200);
    assert.deepEqual(Object.keys(exported.body), ["publicKey"]);
    assert.ok(!exported.body.publicKey.includes("PRIVATE"));
    assert.equal((await request("/keys/missing/public-key")).status, 404);
    assert.equal(
      (await request("/keys/generate", "POST", { name: " " })).status,
      400,
    );
    const generated = await request("/keys/generate", "POST", {
      name: "generated",
    });
    assert.equal(generated.status, 200);
    assert.doesNotMatch(
      JSON.stringify(generated.body),
      /PRIVATE KEY|privateKey|passphrase/,
    );
    const generatedPublic = await request(
      `/keys/${generated.body.keys.at(-1).id}/public-key`,
    );
    assert.match(generatedPublic.body.publicKey, /^ssh-ed25519 /);
    const saved = await request("/servers", "POST", {
      name: "Fixture",
      host: "127.0.0.1",
      username: "tester",
      port: ssh.address().port,
      mode: "ssh",
      keyId: key.body.keys[0].id,
      fingerprint,
    });
    assert.equal(saved.status, 200);
    const server = saved.body.servers[0];
    async function terminal(tmux = false, pin = fingerprint) {
      if (pin !== fingerprint)
        await request("/servers", "POST", { ...server, fingerprint: pin });
      const ws = new WebSocket(origin.replace("http", "ws") + "/terminal", {
        origin,
        headers: { cookie },
      });
      await once(ws, "open");
      const messages = [];
      ws.on("message", (b) => messages.push(JSON.parse(b)));
      ws.send(
        JSON.stringify({
          type: "connect",
          serverId: server.id,
          rows: 32,
          cols: 110,
          tmux,
          session: "work",
        }),
      );
      return { ws, messages };
    }
    async function waitFor(fn) {
      for (let i = 0; i < 100; i++) {
        if (fn()) return;
        await new Promise((r) => setTimeout(r, 30));
      }
      assert.fail("Timed out");
    }
    const a = await terminal();
    await waitFor(() => a.messages.some((x) => x.type === "ready"));
    assert.equal(size.cols, 110);
    a.ws.send(JSON.stringify({ type: "input", data: "hello ssh" }));
    await waitFor(() =>
      a.messages.some(
        (x) =>
          x.type === "data" &&
          Buffer.from(x.data, "base64").toString().includes("hello ssh"),
      ),
    );
    a.ws.close();
    await once(a.ws, "close");
    const b = await terminal(true);
    await waitFor(() => b.messages.some((x) => x.type === "ready"));
    assert.match(command, /new-session -A -s .*work/);
    b.ws.close();
    await once(b.ws, "close");
    const main = {
      name: "main",
      windows: 2,
      attached: 1,
      target: { id: "$1", created: "1700000000" },
    };
    // Each reader returns a whole row, reads a cut listing once more, and
    // refuses a listing that stays cut.
    async function routeListing(...queued) {
      listings.splice(0, listings.length, ...queued);
      listReads = 0;
      return request(`/servers/${server.id}/tmux`);
    }
    let remote = await routeListing(wholeRow);
    assert.equal(remote.status, 200);
    assert.deepEqual(remote.body.sessions, [main]);
    assert.equal(listReads, 1);
    remote = await routeListing(cutRow, wholeRow);
    assert.equal(remote.status, 200);
    assert.deepEqual(remote.body.sessions, [main]);
    assert.equal(listReads, 2);
    remote = await routeListing(cutRow, cutRow);
    assert.equal(remote.status, 400);
    assert.match(remote.body.error, /did not finish listing the sessions/);
    assert.equal(remote.body.sessions, undefined);
    assert.equal(listReads, 2);
    const bad = await terminal(false, "SHA256:" + "A".repeat(43));
    await waitFor(() => bad.messages.some((x) => x.type === "error"));
    assert.ok(!bad.messages.some((x) => x.type === "ready"));
    // First-use trust, password retry, encrypted remember, reconnect, and OTP.
    async function prompted(profile, answer) {
      await request("/servers", "POST", { ...server, ...profile });
      const c = await terminal();
      c.ws.on("message", (raw) => {
        const m = JSON.parse(raw);
        if (m.type === "prompt")
          c.ws.send(
            JSON.stringify({
              type: "answer",
              id: m.data.id,
              ...answer(m.data),
            }),
          );
      });
      await waitFor(() =>
        c.messages.some((m) => m.type === "ready" || m.type === "error"),
      );
      assert.ok(
        c.messages.some((m) => m.type === "ready"),
        JSON.stringify(c.messages),
      );
      return c;
    }
    let credentials = 0;
    const kinds = [];
    const password = await prompted(
      { username: "passworduser", mode: "auto", keyId: "", fingerprint: "" },
      (p) => {
        kinds.push(p.kind);
        if (p.kind === "host-key") {
          assert.equal(p.fingerprint, fingerprint);
          return { accept: true };
        }
        credentials++;
        return {
          method: "password",
          password: credentials === 1 ? "wrong-password" : "fixture-password",
          remember: true,
        };
      },
    );
    assert.deepEqual(kinds, ["host-key", "credentials", "credentials"]);
    async function socketListing(id, ...queued) {
      listings.splice(0, listings.length, ...queued);
      listReads = 0;
      password.ws.send(JSON.stringify({ type: "tmux-list", id }));
      const result = (m) => m.type === "tmux-result" && m.data.id === id;
      await waitFor(() => password.messages.some(result));
      return password.messages.find(result).data;
    }
    let listed = await socketListing("whole", wholeRow);
    assert.deepEqual(listed, { id: "whole", sessions: [main] });
    assert.equal(listReads, 1);
    listed = await socketListing("reread", cutRow, wholeRow);
    assert.deepEqual(listed, { id: "reread", sessions: [main] });
    assert.equal(listReads, 2);
    listed = await socketListing("cut", cutRow, cutRow);
    assert.deepEqual(listed, {
      id: "cut",
      error: "tmux did not finish listing the sessions. Try again.",
    });
    assert.equal(listReads, 2);
    password.ws.close();
    await once(password.ws, "close");
    const publicView = (await request("/data")).body;
    assert.equal(publicView.servers[0].hasPassword, true);
    assert.ok(!JSON.stringify(publicView).includes("fixture-password"));
    assert.ok(
      !readFileSync(path.join(dir, "vault.enc"), "utf8").includes(
        "fixture-password",
      ),
    );
    const remembered = await terminal();
    await waitFor(() => remembered.messages.some((m) => m.type === "ready"));
    assert.ok(!remembered.messages.some((m) => m.type === "prompt"));
    remembered.ws.close();
    await once(remembered.ws, "close");
    const otp = await prompted(
      { username: "otpuser", mode: "auto", keyId: "", fingerprint },
      (p) =>
        p.kind === "credentials"
          ? { method: "keyboard-interactive" }
          : { answers: ["123456"] },
    );
    assert.ok(
      otp.messages.some(
        (m) => m.type === "prompt" && m.data.kind === "keyboard",
      ),
    );
    otp.ws.close();
    await once(otp.ws, "close");
    assert.equal((await request("/data")).body.servers[0].hasPassword, false);
    assert.equal(
      (await request("/tailscale/claim", "POST", { owner: "tab-a" })).status,
      200,
    );
    assert.equal(
      (await request("/tailscale/claim", "POST", { owner: "tab-b" })).status,
      409,
    );
    assert.equal(
      (
        await request("/tailscale/state", "PUT", {
          owner: "tab-b",
          state: { key: "secret" },
        })
      ).status,
      409,
    );
    assert.equal(
      (
        await request("/tailscale/state", "PUT", {
          owner: "tab-a",
          state: { key: "NODE SECRET" },
        })
      ).status,
      200,
    );
    assert.ok(
      !readFileSync(path.join(dir, "vault.enc"), "utf8").includes(
        "NODE SECRET",
      ),
    );
    assert.equal((await request("/lock", "POST", {})).status, 200);
    assert.equal((await request("/data")).status, 401);
    assert.equal(
      (await request("/unlock", "POST", { password: "incorrect password" }))
        .status,
      401,
    );
    const again = await request("/unlock", "POST", {
      password: "very strong test passphrase",
    });
    assert.equal(again.body.servers.length, 1);
  } finally {
    if (proc) await stopAppServer(proc);
    ssh.close();
    rmSync(dir, { recursive: true, force: true });
  }
  assert.equal(await listening(port), false);
});
test("two app servers start concurrently on different ports", async () => {
  const dirs = [0, 1].map(() =>
    mkdtempSync(path.join(os.tmpdir(), "tailterm-api-")),
  );
  const procs = [];
  try {
    const [a, b] = await Promise.all(
      dirs.map((dir) =>
        startAppServer({ dir, onSpawn: (proc) => procs.push(proc) }),
      ),
    );
    assert.notEqual(a.port, b.port);
    for (const { origin, port } of [a, b]) {
      assert.equal(new URL(origin).port, String(port));
      assert.equal((await fetch(origin + "/api/status")).status, 200);
    }
  } finally {
    for (const proc of procs) await stopAppServer(proc);
    for (const dir of dirs) rmSync(dir, { recursive: true, force: true });
  }
});
test("a port held by another process is refused, not reused", async () => {
  const dir = mkdtempSync(path.join(os.tmpdir(), "tailterm-api-"));
  const decoy = createNetServer((socket) => socket.destroy());
  decoy.listen(0, "127.0.0.1");
  await once(decoy, "listening");
  const decoyPort = decoy.address().port;
  const spawned = [];
  try {
    const started = Date.now();
    await assert.rejects(
      startAppServer({
        dir,
        allocate: () => decoyPort,
        onSpawn: (proc) => spawned.push(proc),
      }),
      (error) =>
        error.message.includes(`127.0.0.1:${decoyPort}`) &&
        /did not start/.test(error.message),
    );
    assert.ok(Date.now() - started < 15000);
    assert.equal(spawned.length, 5);
    assert.ok(spawned.every((proc) => !running(proc)));
    assert.equal(decoy.listening, true);
    assert.equal(await listening(decoyPort), true);
  } finally {
    for (const proc of spawned) await stopAppServer(proc);
    decoy.close();
    rmSync(dir, { recursive: true, force: true });
  }
});
test("teardown returns when the server already exited", async () => {
  const dir = mkdtempSync(path.join(os.tmpdir(), "tailterm-api-"));
  let proc;
  try {
    ({ proc } = await startAppServer({ dir }));
    const exited = once(proc, "exit");
    proc.kill("SIGKILL");
    await exited;
    const started = Date.now();
    await stopAppServer(proc);
    assert.ok(Date.now() - started < 1000);
  } finally {
    if (proc) await stopAppServer(proc);
    rmSync(dir, { recursive: true, force: true });
  }
});
test("key generation retries a malformed ssh2 key, never a 400", async () => {
  // server/index.js keyGenerationAttempts.
  const attempts = 8;
  const dir = mkdtempSync(path.join(os.tmpdir(), "tailterm-keygen-"));
  // A preload wraps the server's own ssh2 generator: its first attempts + 1
  // calls return a key ssh2 itself emitted malformed, later calls pass through.
  // It logs counts only; keys never leave the server process.
  const preload = path.join(dir, "malformed-first.mjs");
  writeFileSync(
    preload,
    `import { createRequire } from "node:module";
const { utils } = createRequire(${JSON.stringify(path.resolve("server/index.js"))})("ssh2");
const generate = utils.generateKeyPairSync;
const malformed = (key) => utils.parseKey(key.private) instanceof Error;
let calls = 0;
utils.generateKeyPairSync = (...args) => {
  calls++;
  let key = generate(...args);
  if (calls <= ${attempts + 1}) {
    while (!malformed(key)) key = generate(...args);
    console.error(\`keygen-probe call=\${calls} malformed=forced\`);
  } else if (malformed(key))
    console.error(\`keygen-probe call=\${calls} malformed=natural\`);
  return key;
};
`,
  );
  const nodeOptions = process.env.NODE_OPTIONS;
  let proc, origin, log;
  try {
    const flag = `--import=${pathToFileURL(preload)}`;
    process.env.NODE_OPTIONS = `${nodeOptions ?? ""} ${flag}`;
    try {
      ({ proc, origin, log } = await startAppServer({
        dir,
        onSpawn: (child) => (proc = child),
      }));
    } finally {
      if (nodeOptions === undefined) delete process.env.NODE_OPTIONS;
      else process.env.NODE_OPTIONS = nodeOptions;
    }
    let cookie = "";
    const request = async (url, method = "GET", body) => {
      const r = await fetch(origin + "/api" + url, {
        method,
        headers: { origin, "content-type": "application/json", cookie },
        body: body === undefined ? undefined : JSON.stringify(body),
      });
      return { status: r.status, body: await r.json(), r };
    };
    const login = await request("/unlock", "POST", {
      password: "keygen test passphrase",
    });
    assert.equal(login.status, 200);
    cookie = login.r.headers.get("set-cookie").split(";")[0];
    // Probe lines cross a pipe, so they can trail the response they belong to.
    const probes = async (kind, count) => {
      const deadline = Date.now() + 5000;
      const line = new RegExp(`keygen-probe .* malformed=${kind}`, "g");
      const seen = () => log().match(line) ?? [];
      while (seen().length < count && Date.now() < deadline)
        await new Promise((r) => setTimeout(r, 20));
      return seen();
    };
    // Every attempt malformed: a named server failure, nothing saved.
    const exhausted = await request("/keys/generate", "POST", { name: "k0" });
    assert.equal(exhausted.status, 500);
    assert.equal(exhausted.body.error, "SSH key generation failed.");
    assert.equal((await probes("forced", attempts)).length, attempts);
    assert.equal((await request("/data")).body.keys.length, 0);
    // The next call is also malformed, so generation 1 must retry to succeed.
    const loops = 1000;
    for (let i = 0; i < loops; i++) {
      const generated = await request("/keys/generate", "POST", {
        name: `k${i + 1}`,
      });
      assert.equal(generated.status, 200, `generation ${i + 1}`);
      assert.doesNotMatch(
        JSON.stringify(generated.body),
        /PRIVATE KEY|privateKey|passphrase/,
      );
      const [{ id, fingerprint }] = generated.body.keys;
      const exported = await request(`/keys/${id}/public-key`);
      assert.equal(exported.status, 200);
      const [type, base64] = exported.body.publicKey.split(" ");
      assert.equal(type, "ssh-ed25519");
      const blob = Buffer.from(base64, "base64");
      // string "ssh-ed25519" + string of the full 32-byte key.
      assert.equal(blob.length, 4 + 11 + 4 + 32);
      assert.equal(blob.readUInt32BE(15), 32);
      assert.equal(
        fingerprint,
        "SHA256:" +
          createHash("sha256").update(blob).digest("base64").replace(/=+$/, ""),
      );
      assert.equal((await request(`/keys/${id}`, "DELETE")).status, 200);
    }
    assert.equal(
      (await probes("forced", attempts + 1)).at(-1),
      `keygen-probe call=${attempts + 1} malformed=forced`,
    );
    await new Promise((r) => setTimeout(r, 200));
    const natural = (await probes("natural", 0)).length;
    console.log(
      `keygen: ${loops} generations, 0 failures, ` +
        `${natural} natural malformed keys retried`,
    );
  } finally {
    if (proc) await stopAppServer(proc);
    rmSync(dir, { recursive: true, force: true });
  }
});
test("static preview binds port 0 and carries the run token", async () => {
  const root = mkdtempSync(path.join(os.tmpdir(), "tailterm-preview-"));
  writeFileSync(
    path.join(root, "index.html"),
    "<!doctype html><title>t</title>",
  );
  const cli = spawn(
    process.execPath,
    ["scripts/preview-static.mjs", root, "127.0.0.1", "0"],
    { stdio: ["ignore", "pipe", "pipe"] },
  );
  let log = "";
  cli.stdout.on("data", (d) => (log += d));
  cli.stderr.on("data", (d) => (log += d));
  const token = randomUUID();
  const server = createStaticPreviewServer(root, { token });
  try {
    const printed = /Tailterm static preview: (http:\/\/127\.0\.0\.1:(\d+))\s/;
    for (let i = 0; i < 200 && !printed.test(log) && running(cli); i++)
      await new Promise((r) => setTimeout(r, 50));
    assert.match(log, printed);
    const [, url, port] = printed.exec(log);
    assert.notEqual(port, "0");
    const plain = await fetch(url + "/");
    assert.equal(plain.status, 200);
    assert.equal(plain.headers.get("x-tailterm-preview-token"), null);

    server.listen(0, "127.0.0.1");
    await once(server, "listening");
    const origin = `http://127.0.0.1:${server.address().port}`;
    for (const [route, status] of [
      ["/", 200],
      ["/missing.js", 404],
    ]) {
      const response = await fetch(origin + route);
      assert.equal(response.status, status);
      assert.equal(response.headers.get("x-tailterm-preview-token"), token);
    }
  } finally {
    await stopAppServer(cli);
    if (server.listening) await new Promise((r) => server.close(r));
    rmSync(root, { recursive: true, force: true });
  }
});
// A full browser run needs a prepared checkout (npm ci, build:wasm) and takes
// longer than the unit budget allows, so this runs only when a second prepared
// worktree is named.
test(
  "two concurrent browser suite runs from different worktrees do not interfere",
  { timeout: 300000 },
  async (t) => {
    const peer = process.env.TAILTERM_PEER_WORKTREE;
    if (!peer)
      return t.skip("set TAILTERM_PEER_WORKTREE to a second prepared worktree");
    const checkouts = [process.cwd(), peer].map((p) => realpathSync(p));
    assert.notEqual(checkouts[0], checkouts[1]);
    assert.ok(existsSync(path.join(checkouts[1], "tests/browser.mjs")));
    const runs = checkouts.map((cwd) => {
      const proc = spawn(process.execPath, ["tests/browser.mjs"], {
        cwd,
        stdio: ["ignore", "pipe", "pipe"],
      });
      const run = { proc, cwd, log: "" };
      proc.stdout.on("data", (d) => (run.log += d));
      proc.stderr.on("data", (d) => (run.log += d));
      run.exit = once(proc, "exit");
      return run;
    });
    try {
      const servers = [];
      for (const run of runs) {
        const [code] = await run.exit;
        assert.equal(code, 0, `${run.cwd} failed:\n${run.log}`);
        assert.match(run.log, /Browser checks passed/);
        const line =
          /Browser suite server: http:\/\/127\.0\.0\.1:(\d+) pid (\d+)/;
        assert.match(run.log, line);
        const [, port, pid] = line.exec(run.log);
        servers.push({ port: Number(port), pid: Number(pid) });
      }
      assert.notEqual(servers[0].port, servers[1].port);
      assert.notEqual(servers[0].pid, servers[1].pid);
      for (const { port, pid } of servers) {
        assert.equal(await listening(port), false);
        assert.throws(() => process.kill(pid, 0), { code: "ESRCH" });
      }
    } finally {
      // SIGTERM lets a still-running suite stop its own server.
      for (const { proc } of runs) await stopAppServer(proc);
    }
  },
);
