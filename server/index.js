import { tmuxListCommand, validateTmuxPath } from "../shared/tmux-command.js";
import express from "express";
import { WebSocketServer } from "ws";
import ssh2 from "ssh2";
const { Client, utils } = ssh2;
import { randomBytes, randomUUID, createHash } from "node:crypto";
import { createServer } from "node:http";
import { createConnection } from "node:net";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { interactiveSSHConfig } from "./ssh-auth.js";
import { Vault, tmuxCommand } from "./vault.js";
const root = path.dirname(path.dirname(fileURLToPath(import.meta.url)));
const app = express(),
  http = createServer(app),
  wss = new WebSocketServer({ noServer: true, maxPayload: 65536 });
const vault = new Vault(
  process.env.VAULT_PATH || path.join(root, "data/vault.enc"),
);
const port = Number(process.env.PORT || 4317),
  host = process.env.HOST || "127.0.0.1";
const origin = process.env.APP_ORIGIN || `http://${host}:${port}`;
if (host !== "127.0.0.1" && !origin.startsWith("https://"))
  throw new Error(
    "Remote access requires APP_ORIGIN=https://… behind a TLS proxy.",
  );
const sessions = new Map(),
  attempts = new Map();
let lease = null;
const tokenOf = (req) =>
  /\btt_session=([a-f0-9]{64})\b/.exec(req.headers.cookie || "")?.[1];
function authorized(req) {
  const s = sessions.get(tokenOf(req));
  return s && s.expires > Date.now() && vault.data ? s : null;
}
function publicData() {
  return {
    servers: vault.data.servers.map(({ password, ...server }) => ({
      ...server,
      hasPassword: !!password,
    })),
    keys: vault.data.keys.map(({ id, name, fingerprint }) => ({
      id,
      name,
      fingerprint,
    })),
    sessions: vault.data.sessions,
  };
}
function disconnectAll() {
  for (const ws of wss.clients) ws.close(1000, "Vault locked");
  sessions.clear();
  lease = null;
  vault.lock();
}
app.disable("x-powered-by");
app.use((req, res, next) => {
  res.set({
    "X-Content-Type-Options": "nosniff",
    "Referrer-Policy": "no-referrer",
    "X-Frame-Options": "DENY",
    "Cache-Control": "no-store",
    "Content-Security-Policy":
      "default-src 'self'; script-src 'self' 'wasm-unsafe-eval'; style-src 'self' 'unsafe-inline'; connect-src 'self' https: wss:; img-src 'self' data:; font-src 'self'; object-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'",
  });
  if (req.headers.host !== new URL(origin).host)
    return res.status(403).json({ error: "Invalid host" });
  if (
    req.path.startsWith("/api/") &&
    !["GET", "HEAD"].includes(req.method) &&
    req.headers.origin !== origin
  )
    return res.status(403).json({ error: "Invalid origin" });
  next();
});
app.use(express.json({ limit: "512kb" }));
app.get("/api/status", (req, res) =>
  res.json({ initialized: vault.exists, unlocked: !!authorized(req) }),
);
app.post("/api/unlock", (req, res) => {
  const ip = req.socket.remoteAddress,
    a = attempts.get(ip) || { count: 0, since: Date.now() };
  if (Date.now() - a.since > 300000) {
    a.count = 0;
    a.since = Date.now();
  }
  if (a.count >= 5)
    return res
      .status(429)
      .json({ error: "Too many attempts. Wait five minutes." });
  attempts.set(ip, a);
  a.count++;
  if (
    typeof req.body.password !== "string" ||
    req.body.password.length < 14 ||
    req.body.password.length > 1024
  )
    return res
      .status(400)
      .json({ error: "Use a passphrase of at least 14 characters." });
  try {
    // Authenticate against disk even when another session has unlocked the vault.
    vault.unlock(req.body.password);
    a.count = 0;
    const token = randomBytes(32).toString("hex");
    sessions.set(token, { expires: Date.now() + 8 * 3600000 });
    res
      .cookie("tt_session", token, {
        httpOnly: true,
        sameSite: "strict",
        secure: origin.startsWith("https:"),
        maxAge: 8 * 3600000,
        path: "/",
      })
      .json(publicData());
  } catch {
    res.status(401).json({ error: "Could not unlock vault." });
  }
});
app.use("/api", (req, res, next) =>
  authorized(req)
    ? next()
    : res.status(401).json({ error: "Unlock your vault first." }),
);
app.get("/api/data", (req, res) => res.json(publicData()));
app.post("/api/lock", (req, res) => {
  disconnectAll();
  res.clearCookie("tt_session").json({ ok: true });
});
app.post("/api/servers", (req, res) => {
  const b = req.body;
  if (b.id && !/^[a-f0-9-]{36}$/.test(b.id))
    return res.status(400).json({ error: "Invalid server ID" });
  if (
    !/^[a-zA-Z0-9._:-]{1,253}$/.test(b.host || "") ||
    !/^[a-zA-Z0-9._-]{1,64}$/.test(b.username || "") ||
    !["auto", "wasm", "ssh"].includes(b.mode) ||
    !Number.isInteger(b.port) ||
    b.port < 1 ||
    b.port > 65535 ||
    typeof b.name !== "string" ||
    !b.name.trim() ||
    b.name.length > 80
  )
    return res.status(400).json({
      error: "Check server name, hostname, username, port and connection mode.",
    });
  if (b.mode === "wasm" && b.port !== 22)
    return res.status(400).json({ error: "Tailscale WASM SSH uses port 22." });
  if (b.fingerprint && !/^SHA256:[A-Za-z0-9+/]{43}$/.test(b.fingerprint))
    return res.status(400).json({ error: "Invalid SHA256 host fingerprint." });
  validateTmuxPath(b.tmuxPath || "");
  const previous = vault.data.servers.find((s) => s.id === b.id);
  if (b.keyId && !vault.data.keys.some((k) => k.id === b.keyId))
    return res.status(400).json({ error: "Unknown SSH key" });
  const record = {
    id: b.id || randomUUID(),
    name: b.name,
    host: b.host,
    username: b.username,
    port: b.port,
    mode: b.mode,
    tailnet: !!b.tailnet,
    tmuxPath: b.tmuxPath || "",
    ...(previous?.password &&
    previous.host === b.host &&
    previous.port === b.port &&
    previous.username === b.username &&
    !b.clearPassword
      ? { password: previous.password }
      : {}),
    keyId: b.keyId || "",
    fingerprint: b.fingerprint || "",
    group: String(b.group || "Servers").slice(0, 40),
  };
  const i = vault.data.servers.findIndex((x) => x.id === record.id);
  if (i < 0) vault.data.servers.push(record);
  else vault.data.servers[i] = record;
  vault.save();
  res.json(publicData());
});
app.delete("/api/servers/:id", (req, res) => {
  vault.data.servers = vault.data.servers.filter((s) => s.id !== req.params.id);
  vault.data.sessions = vault.data.sessions.filter(
    (s) => s.serverId !== req.params.id,
  );
  vault.save();
  res.json(publicData());
});
app.get("/api/keys/:id/public-key", (req, res) => {
  const saved = vault.data.keys.find((k) => k.id === req.params.id);
  if (!saved) return res.status(404).json({ error: "SSH key not found." });
  const parsed = utils.parseKey(
    saved.privateKey,
    saved.passphrase || undefined,
  );
  if (parsed instanceof Error || Array.isArray(parsed))
    return res
      .status(400)
      .json({ error: "Invalid private key or passphrase." });
  res.json({
    publicKey: `${parsed.type} ${parsed.getPublicSSH().toString("base64")}`,
  });
});
app.post(["/api/keys", "/api/keys/generate"], (req, res) => {
  const { name, passphrase } = req.body;
  let privateKey = req.body.privateKey;
  const generating = req.path === "/api/keys/generate";
  if (
    typeof name !== "string" ||
    !name.trim() ||
    name.length > 80 ||
    (!generating && typeof privateKey !== "string")
  )
    return res
      .status(400)
      .json({ error: "Name and private key are required." });
  if (generating) privateKey = utils.generateKeyPairSync("ed25519").private;
  const parsed = utils.parseKey(
    privateKey,
    generating ? undefined : passphrase || undefined,
  );
  if (
    parsed instanceof Error ||
    Array.isArray(parsed) ||
    !parsed.isPrivateKey()
  )
    return res
      .status(400)
      .json({ error: "Invalid private key or passphrase." });
  const fingerprint =
    "SHA256:" +
    createHash("sha256")
      .update(parsed.getPublicSSH())
      .digest("base64")
      .replace(/=+$/, "");
  vault.data.keys.push({
    id: randomUUID(),
    name,
    privateKey,
    passphrase: generating ? "" : passphrase,
    fingerprint,
  });
  vault.save();
  res.json(publicData());
});
app.delete("/api/keys/:id", (req, res) => {
  if (vault.data.servers.some((s) => s.keyId === req.params.id))
    return res
      .status(409)
      .json({ error: "Remove this key from saved servers first." });
  vault.data.keys = vault.data.keys.filter((k) => k.id !== req.params.id);
  vault.save();
  res.json(publicData());
});
// One live browser owns the persisted node, preventing cloned WireGuard identities.
app.post("/api/tailscale/claim", (req, res) => {
  const token = tokenOf(req),
    owner = req.body.owner;
  if (typeof owner !== "string" || owner.length > 80)
    return res.status(400).json({ error: "Invalid browser owner" });
  if (
    lease &&
    lease.until > Date.now() &&
    (lease.token !== token || lease.owner !== owner)
  )
    return res.status(409).json({
      error:
        "Tailscale is active in another tab. Close it and wait 45 seconds.",
    });
  lease = { token, owner, until: Date.now() + 45000 };
  res.json(vault.data.tailscale);
});
app.put("/api/tailscale/state", (req, res) => {
  if (
    !lease ||
    lease.token !== tokenOf(req) ||
    lease.owner !== req.body.owner ||
    lease.until < Date.now()
  )
    return res.status(409).json({ error: "Tailscale identity lease expired." });
  const state = req.body.state;
  if (
    !state ||
    typeof state !== "object" ||
    Array.isArray(state) ||
    Object.values(state).some((v) => typeof v !== "string")
  )
    return res.status(400).json({ error: "Invalid node state" });
  vault.data.tailscale = state;
  vault.save();
  res.json({ ok: true });
});
app.post("/api/sessions", (req, res) => {
  const { serverId, name } = req.body;
  tmuxCommand(name);
  if (!vault.data.servers.some((s) => s.id === serverId))
    return res.status(404).json({ error: "Unknown server" });
  let s = vault.data.sessions.find(
    (s) => s.serverId === serverId && s.name === name,
  );
  if (!s) {
    s = { id: randomUUID(), serverId, name };
    vault.data.sessions.push(s);
  }
  s.lastConnected = new Date().toISOString();
  vault.save();
  res.json(publicData());
});
app.delete("/api/sessions/:id", (req, res) => {
  vault.data.sessions = vault.data.sessions.filter(
    (s) => s.id !== req.params.id,
  );
  vault.save();
  res.json(publicData());
});
function sshConfig(s) {
  const key = vault.data.keys.find((k) => k.id === s.keyId);
  if (!s.fingerprint)
    throw new Error("Add the verified SSH host fingerprint before connecting.");
  return {
    host: s.host,
    port: s.port,
    username: s.username,
    privateKey: key?.privateKey,
    passphrase: key?.passphrase,
    password: s.password,
    readyTimeout: 20000,
    keepaliveInterval: 15000,
    hostVerifier: (raw) =>
      "SHA256:" +
        createHash("sha256").update(raw).digest("base64").replace(/=+$/, "") ===
      s.fingerprint,
  };
}
app.get("/api/servers/:id/tmux", (req, res) => {
  const s = vault.data.servers.find((s) => s.id === req.params.id);
  if (!s)
    return res.status(400).json({
      error: "Remote discovery is available for server SSH connections.",
    });
  const conn = new Client();
  let done = false;
  const finish = (err, data) => {
    if (done) return;
    done = true;
    clearTimeout(timer);
    conn.end();
    if (err) res.status(400).json({ error: err.message });
    else res.json({ sessions: data });
  };
  const timer = setTimeout(
    () => finish(new Error("Session discovery timed out")),
    25000,
  );
  conn
    .on("error", (e) => finish(e))
    .on("ready", () =>
      conn.exec(tmuxListCommand(s.tmuxPath), (err, stream) => {
        if (err) return finish(err);
        let out = "",
          stderr = "";
        stream.stderr.on(
          "data",
          (d) => (stderr = (stderr + d.toString()).slice(-8192)),
        );
        stream.on("data", (d) => {
          out += d;
          if (out.length > 65536) finish(new Error("Too many sessions"));
        });
        stream.on("close", (code) =>
          code !== 0
            ? finish(
                new Error(stderr.trim() || "Remote tmux discovery failed."),
              )
            : finish(
                null,
                out
                  .trim()
                  .split("\n")
                  .filter(Boolean)
                  .map((line) => {
                    const [name, windows, attached] = line.split("|");
                    return {
                      name,
                      windows: Number(windows),
                      attached: Number(attached),
                    };
                  }),
              ),
        );
      }),
    );
  try {
    conn.connect(sshConfig(s));
  } catch (e) {
    finish(e);
  }
});
http.on("upgrade", (req, socket, head) => {
  if (
    req.url !== "/terminal" ||
    req.headers.origin !== origin ||
    !authorized(req)
  ) {
    socket.write("HTTP/1.1 403 Forbidden\r\n\r\n");
    socket.destroy();
    return;
  }
  wss.handleUpgrade(req, socket, head, (ws) => wss.emit("connection", ws, req));
});
wss.on("connection", (ws, req) => {
  ws.authRequest = req;
  let conn,
    stream,
    started = false,
    connectedProfile = null;
  const send = (type, data) => {
    if (ws.bufferedAmount > 4 * 1024 * 1024) {
      ws.close(1009, "Terminal output exceeded buffer");
      return;
    }
    if (ws.readyState === 1) ws.send(JSON.stringify({ type, data }));
  };
  const pending = new Map();
  function ask(kind, payload) {
    return new Promise((resolve, reject) => {
      const id = randomUUID();
      const timeout = setTimeout(() => {
        pending.delete(id);
        reject(new Error("Login prompt timed out"));
        ws.close();
      }, 120000);
      pending.set(id, { resolve, reject, timeout });
      send("prompt", { id, kind, ...payload });
    });
  }
  const timer = setTimeout(() => {
    if (!started) ws.close();
  }, 10000);
  ws.on("message", (raw) => {
    try {
      if (!authorized(req)) {
        ws.close();
        return;
      }
      const m = JSON.parse(raw);
      if (m.type === "answer") {
        const prompt = pending.get(m.id);
        if (!prompt) return;
        pending.delete(m.id);
        clearTimeout(prompt.timeout);
        if (m.cancel) {
          prompt.reject(new Error("Connection canceled"));
          ws.close();
          return;
        }
        prompt.resolve(m);
        return;
      }
      if (m.type === "connect" && !started) {
        started = true;
        clearTimeout(timer);
        const s = vault.data.servers.find((x) => x.id === m.serverId);
        if (!s) throw new Error("Unknown SSH server");
        connectedProfile = { ...s };
        const command = m.tmux
          ? tmuxCommand(m.session, s.tmuxPath, m.resumeOnly === true)
          : null;
        const auth = interactiveSSHConfig(
          { ...s },
          {
            keys: () => vault.data?.keys || [],
            ask,
            rememberHost: (fingerprint) => {
              const live = vault.data?.servers.find((x) => x.id === s.id);
              if (!live || live.host !== s.host || live.port !== s.port)
                throw new Error("Server profile changed during login");
              if (live.fingerprint && live.fingerprint !== fingerprint)
                throw new Error("Host key changed");
              live.fingerprint = fingerprint;
              vault.save();
            },
            rememberCredentials: (credentials) => {
              const live = vault.data?.servers.find((x) => x.id === s.id);
              if (
                live &&
                live.host === s.host &&
                live.port === s.port &&
                live.username === s.username
              ) {
                Object.assign(live, credentials);
                vault.save();
              }
            },
          },
        );
        conn = new Client();
        conn
          .on("error", (e) => {
            send(
              "error",
              /ENOTFOUND|ETIMEDOUT|EHOSTUNREACH|ENETUNREACH|ECONNREFUSED/.test(
                e.code || "",
              )
                ? `The application server cannot reach ${s.host}:${s.port}. For a tailnet-only host, the application server also needs Tailscale connectivity. ${e.message}`
                : /verification failed/.test(e.message)
                  ? "SSH host key was rejected or changed. Verify the destination fingerprint before updating this server."
                  : e.message,
            );
            ws.close();
          })
          .on("close", () => ws.close())
          .on("ready", () => {
            auth.onReady();
            const ready = (err, ch) => {
              if (err) {
                send("error", err.message);
                conn.end();
                return;
              }
              stream = ch;
              send("ready", true);
              ch.on("data", (d) => send("data", d.toString("base64")));
              ch.stderr.on("data", (d) => send("data", d.toString("base64")));
              ch.on("close", () => conn.end());
            };
            const pty = {
              term: "xterm-256color",
              cols: clamp(m.cols, 80),
              rows: clamp(m.rows, 24),
            };
            if (command) conn.exec(command, { pty }, ready);
            else conn.shell(pty, ready);
          });
        const socket = createConnection({ host: s.host, port: s.port });
        const connectTimer = setTimeout(
          () =>
            socket.destroy(
              Object.assign(new Error("TCP connection timed out"), {
                code: "ETIMEDOUT",
              }),
            ),
          15000,
        );
        socket.once("connect", () => clearTimeout(connectTimer));
        socket.once("close", () => clearTimeout(connectTimer));
        conn.connect({ ...auth.config, sock: socket });
      } else if (m.type === "tmux-list" && stream) {
        const id = String(m.id).slice(0, 80);
        conn.exec(tmuxListCommand(connectedProfile.tmuxPath), (err, ch) => {
          if (err) {
            send("tmux-result", { id, error: err.message });
            return;
          }
          let output = "",
            stderr = "";
          ch.stderr.on(
            "data",
            (d) => (stderr = (stderr + d.toString()).slice(-8192)),
          );
          const timer = setTimeout(() => ch.close(), 15000);
          ch.on("data", (d) => {
            output += d;
            if (output.length > 65536) ch.close();
          });
          ch.on("close", (code) => {
            clearTimeout(timer);
            send(
              "tmux-result",
              code !== 0
                ? {
                    id,
                    error: stderr.trim() || "Remote tmux discovery failed.",
                  }
                : {
                    id,
                    sessions: output
                      .trim()
                      .split("\n")
                      .filter(Boolean)
                      .map((line) => {
                        const [name, windows, attached] = line.split("|");
                        return {
                          name,
                          windows: Number(windows),
                          attached: Number(attached),
                        };
                      }),
                  },
            );
          });
        });
      } else if (m.type === "input" && stream && typeof m.data === "string") {
        stream.write(m.data);
      } else if (m.type === "resize" && stream)
        stream.setWindow(clamp(m.rows, 24), clamp(m.cols, 80), 0, 0);
    } catch (e) {
      send("error", e.message);
      ws.close();
    }
  });
  ws.on("close", () => {
    for (const p of pending.values()) {
      clearTimeout(p.timeout);
      p.reject(new Error("Connection closed"));
    }
    pending.clear();
    clearTimeout(timer);
    stream?.close();
    conn?.end();
  });
  ws.on("error", () => conn?.end());
});
function clamp(v, f) {
  return Number.isInteger(v) ? Math.min(500, Math.max(2, v)) : f;
}
setInterval(() => {
  for (const [t, s] of sessions) if (s.expires < Date.now()) sessions.delete(t);
  if (vault.data && !sessions.size) disconnectAll();
  for (const ws of wss.clients) {
    if (!authorized(ws.authRequest)) {
      ws.close(1000, "Session expired");
      continue;
    }
    if (ws.isAlive === false) {
      ws.terminate();
      continue;
    }
    ws.isAlive = false;
    ws.ping();
  }
}, 30000).unref();
wss.on("connection", (ws) => {
  ws.isAlive = true;
  ws.on("pong", () => (ws.isAlive = true));
});
if (process.env.NODE_ENV === "production")
  app.use(express.static(path.join(root, "dist")));
else {
  const { createServer } = await import("vite");
  const vite = await createServer({
    server: { middlewareMode: true, hmr: false },
    appType: "spa",
  });
  app.use(vite.middlewares);
}
app.use((err, req, res, next) => {
  res.status(400).json({ error: err.message || "Request failed" });
});
http.listen(port, host, () => console.log(`Tailterm: ${origin}`));
for (const signal of ["SIGINT", "SIGTERM"])
  process.on(signal, () => {
    disconnectAll();
    http.close(() => process.exit(0));
  });
