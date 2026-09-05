import {
  deriveVaultKey,
  openVault,
  sealVault,
  portableData,
} from "./vault-crypto.js";
import { validateSession, validateTmuxPath } from "../shared/tmux-command.js";
import { validatePrivateKey } from "./wasm-runtime.js";
let database,
  contents,
  key,
  salt,
  releaseLock,
  queue = Promise.resolve();
const empty = () => ({ servers: [], keys: [], sessions: [], tailscale: {} });
async function db() {
  if (database) return database;
  if (!crypto.subtle || !navigator.locks)
    throw new Error(
      "Use HTTPS and a browser with Web Crypto and Web Locks support.",
    );
  database = await new Promise((resolve, reject) => {
    const r = indexedDB.open("tailserve", 1);
    r.onupgradeneeded = () => r.result.createObjectStore("vault");
    r.onsuccess = () => resolve(r.result);
    r.onerror = () => reject(r.error);
  });
  return database;
}
async function disk(value) {
  const database = await db();
  return new Promise((resolve, reject) => {
    const tx = database.transaction(
      "vault",
      value === undefined ? "readonly" : "readwrite",
    );
    const store = tx.objectStore("vault");
    const r =
      value === undefined
        ? store.get("encrypted")
        : store.put(value, "encrypted");
    tx.oncomplete = () => resolve(r.result);
    tx.onerror = () => reject(tx.error);
    tx.onabort = () => reject(tx.error || new Error("Vault save aborted."));
  });
}
async function acquire() {
  if (releaseLock) return;
  await new Promise((resolve, reject) => {
    navigator.locks
      .request("tailserve-vault-owner", { ifAvailable: true }, async (lock) => {
        if (!lock) {
          reject(
            new Error(
              "This workspace is open in another browser tab. Lock or close it first; use + for more terminal sessions.",
            ),
          );
          return;
        }
        await new Promise((release) => {
          releaseLock = release;
          resolve();
        });
      })
      .catch(reject);
  });
}
function requireUnlocked() {
  if (!contents || !key) throw new Error("Unlock your local vault first.");
}
export function localData() {
  requireUnlocked();
  return {
    servers: contents.servers.map(({ password, ...s }) => ({
      ...s,
      hasPassword: !!password,
    })),
    keys: contents.keys.map(({ id, name, fingerprint }) => ({
      id,
      name,
      fingerprint,
    })),
    sessions: structuredClone(contents.sessions),
  };
}
export function credentials(id) {
  requireUnlocked();
  const server = contents.servers.find((s) => s.id === id);
  if (!server) throw new Error("Unknown server.");
  return {
    server: structuredClone(server),
    key: structuredClone(contents.keys.find((k) => k.id === server.keyId)),
  };
}
export function privateKey(id) {
  requireUnlocked();
  return structuredClone(contents.keys.find((k) => k.id === id));
}
async function mutate(fn) {
  const task = queue.then(async () => {
    requireUnlocked();
    const next = structuredClone(contents);
    const result = await fn(next);
    const envelope = await sealVault(next, key, salt);
    await disk(envelope);
    contents = next;
    return result === undefined ? localData() : result;
  });
  queue = task.catch(() => {});
  return task;
}
export async function rememberCredential(id, changes, endpoint) {
  return mutate((data) => {
    const s = data.servers.find((s) => s.id === id);
    if (!s) throw new Error("Server was removed.");
    if (
      endpoint &&
      ["host", "port", "username"].some((k) => s[k] !== endpoint[k])
    )
      throw new Error(
        "Server profile changed during login; credentials were not saved.",
      );
    Object.assign(s, changes);
  });
}
export async function localAPI(url, method = "GET", body = {}) {
  if (url === "/status")
    return { initialized: !!(await disk()), unlocked: !!contents };
  if (url === "/unlock") {
    await db();
    await acquire();
    try {
      const envelope = await disk();
      if (envelope) {
        const opened = await openVault(envelope, body.password);
        validateData(opened.data);
        contents = opened.data;
        key = opened.key;
        salt = opened.salt;
      } else {
        salt = crypto.getRandomValues(new Uint8Array(16));
        key = await deriveVaultKey(body.password, salt);
        const initial = empty();
        await disk(await sealVault(initial, key, salt));
        contents = initial;
      }
      void navigator.storage?.persist?.().catch(() => {});
      return localData();
    } catch (e) {
      contents = key = salt = undefined;
      releaseLock?.();
      releaseLock = undefined;
      throw e;
    }
  }
  requireUnlocked();
  if (url === "/data") return localData();
  if (url === "/lock") {
    await queue;
    contents = key = salt = undefined;
    releaseLock?.();
    releaseLock = undefined;
    return { ok: true };
  }
  if (url === "/tailscale/claim") return structuredClone(contents.tailscale);
  if (url === "/tailscale/state")
    return mutate((d) => {
      d.tailscale = structuredClone(body.state);
      return { ok: true };
    });
  if (url === "/servers" && method === "POST")
    return mutate((d) => {
      validateServer(body);
      const previous = d.servers.find((s) => s.id === body.id);
      if (body.id && !previous) throw new Error("Unknown server.");
      if (body.keyId && !d.keys.some((k) => k.id === body.keyId))
        throw new Error("Unknown SSH key.");
      const server = {
        id: body.id || crypto.randomUUID(),
        name: body.name.trim(),
        group: String(body.group || "Personal").slice(0, 40),
        host: body.host,
        port: body.port,
        username: body.username,
        mode: body.mode,
        tailnet: !!body.tailnet,
        tmuxPath: body.tmuxPath || "",
        fingerprint: body.fingerprint || "",
        keyId: body.keyId || "",
      };
      if (
        previous?.password &&
        !body.clearPassword &&
        previous.host === server.host &&
        previous.port === server.port &&
        previous.username === server.username
      )
        server.password = previous.password;
      if (previous) d.servers[d.servers.indexOf(previous)] = server;
      else d.servers.push(server);
    });
  if (url.startsWith("/servers/") && method === "DELETE")
    return mutate((d) => {
      const id = url.slice(9);
      d.servers = d.servers.filter((s) => s.id !== id);
      d.sessions = d.sessions.filter((s) => s.serverId !== id);
    });
  if (url === "/keys" && method === "POST") {
    if (
      typeof body.name !== "string" ||
      !body.name.trim() ||
      body.name.length > 80 ||
      typeof body.privateKey !== "string" ||
      body.privateKey.length > 64000
    )
      throw new Error("Check key name and private key.");
    const parsed = await validatePrivateKey(
      body.privateKey,
      body.passphrase || "",
    );
    return mutate((d) => {
      d.keys.push({
        id: crypto.randomUUID(),
        name: body.name,
        privateKey: body.privateKey,
        passphrase: body.passphrase || "",
        fingerprint: parsed.fingerprint,
      });
    });
  }
  if (url.startsWith("/keys/") && method === "DELETE")
    return mutate((d) => {
      const id = url.slice(6);
      if (d.servers.some((s) => s.keyId === id))
        throw new Error("Remove this key from server profiles first.");
      d.keys = d.keys.filter((k) => k.id !== id);
    });
  if (url === "/sessions" && method === "POST")
    return mutate((d) => {
      validateSession(body.name);
      if (!d.servers.some((s) => s.id === body.serverId))
        throw new Error("Unknown server.");
      let session = d.sessions.find(
        (s) => s.serverId === body.serverId && s.name === body.name,
      );
      if (!session) {
        session = {
          id: crypto.randomUUID(),
          serverId: body.serverId,
          name: body.name,
        };
        d.sessions.push(session);
      }
      session.lastConnected = new Date().toISOString();
    });
  if (url.startsWith("/sessions/") && method === "DELETE")
    return mutate((d) => {
      d.sessions = d.sessions.filter((s) => s.id !== url.slice(10));
    });
  throw new Error("Unsupported local operation: " + method + " " + url);
}
export async function exportBackup() {
  await queue;
  requireUnlocked();
  return sealVault(portableData(contents), key, salt);
}
export async function importBackup(envelope, password) {
  const opened = await openVault(envelope, password);
  validateData(opened.data);
  // Never import another browser's node identity. Keep this browser's identity, if any.
  return mutate((d) => {
    d.servers = opened.data.servers;
    d.keys = opened.data.keys;
    d.sessions = opened.data.sessions;
  });
}
function validateServer(s) {
  if (
    !s ||
    typeof s.name !== "string" ||
    !s.name.trim() ||
    s.name.length > 80 ||
    !/^[a-zA-Z0-9._:-]{1,253}$/.test(s.host || "") ||
    !/^[a-zA-Z0-9._-]{1,64}$/.test(s.username || "") ||
    !Number.isInteger(s.port) ||
    s.port < 1 ||
    s.port > 65535 ||
    !["auto", "ssh", "wasm"].includes(s.mode)
  )
    throw new Error(
      "Check server name, hostname, username, port and connection mode.",
    );
  if (s.fingerprint && !/^SHA256:[A-Za-z0-9+/]{43}$/.test(s.fingerprint))
    throw new Error("Invalid SSH fingerprint.");
  validateTmuxPath(s.tmuxPath || "");
}
function validateData(d) {
  if (
    !d ||
    !Array.isArray(d.servers) ||
    !Array.isArray(d.keys) ||
    !Array.isArray(d.sessions) ||
    d.servers.length > 1000 ||
    d.keys.length > 1000 ||
    d.sessions.length > 10000
  )
    throw new Error("Invalid vault contents.");
  for (const s of d.servers) {
    validateServer(s);
    if (typeof s.id !== "string") throw new Error("Invalid server ID.");
  }
  for (const k of d.keys)
    if (
      typeof k.id !== "string" ||
      typeof k.name !== "string" ||
      typeof k.privateKey !== "string" ||
      k.privateKey.length > 64000
    )
      throw new Error("Invalid saved key.");
  for (const s of d.sessions) {
    validateSession(s.name);
    if (!d.servers.some((x) => x.id === s.serverId))
      throw new Error("Invalid session bookmark.");
  }
  d.tailscale = d.tailscale || {};
}
