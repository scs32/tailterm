import test from "node:test";
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { mkdtempSync, rmSync, readFileSync } from "node:fs";
import os from "node:os";
import path from "node:path";
import { once } from "node:events";
import { generateKeyPairSync, createHash } from "node:crypto";
import ssh2 from "ssh2";
const { Server, utils } = ssh2;
import WebSocket from "ws";
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
  const dir = mkdtempSync(path.join(os.tmpdir(), "tailterm-api-")),
    port = 14317,
    origin = `http://127.0.0.1:${port}`;
  let command = "",
    size;
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
              stream.write("main|2|1\n");
              stream.exit(0);
              stream.end();
            } else stream.write("tmux attached\r\n");
          });
        }),
      );
  });
  ssh.listen(0, "127.0.0.1");
  await once(ssh, "listening");
  const proc = spawn(process.execPath, ["server/index.js"], {
    env: {
      ...process.env,
      PORT: String(port),
      VAULT_PATH: path.join(dir, "vault.enc"),
      NODE_ENV: "production",
    },
    stdio: ["ignore", "pipe", "pipe"],
  });
  let logs = "";
  proc.stdout.on("data", (b) => (logs += b));
  proc.stderr.on("data", (b) => (logs += b));
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
    for (let i = 0; i < 100 && !logs.includes("Tailterm:"); i++)
      await new Promise((r) => setTimeout(r, 50));
    assert.match(logs, /Tailterm:/);
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
    const remote = await request(`/servers/${server.id}/tmux`);
    assert.deepEqual(remote.body.sessions, [
      { name: "main", windows: 2, attached: 1 },
    ]);
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
    password.ws.send(JSON.stringify({ type: "tmux-list", id: "discovery" }));
    await waitFor(() =>
      password.messages.some((m) => m.type === "tmux-result"),
    );
    assert.equal(
      password.messages.find((m) => m.type === "tmux-result").data.sessions[0]
        .name,
      "main",
    );
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
    proc.kill("SIGTERM");
    await once(proc, "exit");
    ssh.close();
    rmSync(dir, { recursive: true, force: true });
  }
});
