import {
  AGENT_CATALOG_VERSION,
  MAX_AGENT_DEFINITIONS,
  definitionUsers,
  migrateAgentData,
  normalizeAgentCatalog,
  normalizeAgentDefinition,
  normalizeReferencedTeam,
} from "./agents.js";
import {
  migrateUnsupportedUnstartedLaunchReasoning,
  MAX_TEAM_LAUNCH_PLANS,
  normalizeTeamLaunchPlans,
} from "./launch-journal.js";
import {
  normalizeUsername,
  validUsername,
  profileMaster,
} from "./profile-crypto.js";
import { credentialCache } from "./credential-cache.js";
import { normalizeHubURL } from "./hub-client.js";
import { normalizeTaskRef, normalizeRuntimes } from "./task-ref.js";
import {
  deriveVaultKey,
  openVault,
  sealVault,
  portableData,
} from "./vault-crypto.js";
import {
  validateSession,
  validateTmuxPath,
  validateTarget,
} from "../shared/tmux-command.js";
import { validatePrivateKey, generatePrivateKey } from "./wasm-runtime.js";
import { normalizeWorkspace } from "./workspace-state.js";
import {
  MAX_HANDLER_PLANS,
  normalizeHandlerPlan,
  normalizeHandlerPlanKey,
  normalizeHandlerPlans,
} from "./project-handler.js";
let profileUnlockKey,
  profileSerial = 0;
let database,
  contents,
  key,
  salt,
  releaseLock,
  queue = Promise.resolve();
const empty = () => ({
  servers: [],
  keys: [],
  sessions: [],
  tailscale: {},
  hub: { url: "" },
  agentCatalog: { version: AGENT_CATALOG_VERSION, definitions: [] },
  teamsVersion: 2,
  teams: [],
});
const MAX_WORK_ITEM_DRAFTS = 24;
const MAX_WORK_ITEM_DRAFT_BYTES = 24000;
export const MAX_BOARD_INTENTS = 32;
export const MAX_BOARD_INTENT_BYTES = 64 * 1024;
export const MAX_BOARD_INTENTS_BYTES = 1024 * 1024;
export const MAX_QUEUE_INTENTS = 24;
export const MAX_QUEUE_INTENT_BYTES = 64 * 1024;
export const MAX_QUEUE_INTENTS_BYTES = 1024 * 1024;
const vaultEncoder = new TextEncoder();
function normalizeBoardIntents(value) {
  if (!Array.isArray(value)) return [];
  const intents = value
    .filter(
      (intent) =>
        intent &&
        /^[a-f0-9]{64}$/.test(intent.scope || "") &&
        typeof intent.id === "string" &&
        intent.id.length > 0 &&
        intent.id.length <= 256 &&
        ["draft", "uncertain"].includes(intent.state) &&
        typeof intent.requestId === "string" &&
        intent.requestId.length <= 128 &&
        typeof intent.updatedAt === "string" &&
        vaultEncoder.encode(JSON.stringify(intent)).byteLength <=
          MAX_BOARD_INTENT_BYTES,
    )
    .map((intent) => structuredClone(intent));
  const unique = new Map();
  for (const intent of intents)
    unique.set(`${intent.scope}\0${intent.id}`, intent);
  const out = [...unique.values()].sort((a, b) =>
    a.updatedAt.localeCompare(b.updatedAt),
  );
  if (
    out.length > MAX_BOARD_INTENTS ||
    vaultEncoder.encode(JSON.stringify(out)).byteLength >
      MAX_BOARD_INTENTS_BYTES
  )
    throw new Error(
      "Board intent storage is full. Retry, recover, or discard an existing intent before saving another.",
    );
  return out;
}
function normalizeWorkItemDrafts(value) {
  if (!Array.isArray(value)) return [];
  return value
    .filter(
      (draft) =>
        draft &&
        typeof draft.scope === "string" &&
        /^[a-f0-9]{64}$/.test(draft.scope) &&
        typeof draft.id === "string" &&
        draft.id.length <= 256 &&
        typeof draft.requestId === "string" &&
        draft.requestId.length <= 128 &&
        typeof draft.updatedAt === "string" &&
        JSON.stringify(draft).length <= MAX_WORK_ITEM_DRAFT_BYTES,
    )
    .sort((a, b) => b.updatedAt.localeCompare(a.updatedAt))
    .slice(0, MAX_WORK_ITEM_DRAFTS)
    .map((draft) => structuredClone(draft));
}
function normalizeQueueIntents(value) {
  if (!Array.isArray(value)) return [];
  const intents = value
    .filter(
      (intent) =>
        intent &&
        /^[a-f0-9]{64}$/.test(intent.scope || "") &&
        typeof intent.id === "string" &&
        intent.id.length > 0 &&
        intent.id.length <= 256 &&
        typeof intent.requestId === "string" &&
        intent.requestId.length > 0 &&
        intent.requestId.length <= 128 &&
        ["draft", "uncertain"].includes(intent.state) &&
        typeof intent.payload === "object" &&
        intent.payload !== null &&
        intent.payload.requestId === intent.requestId &&
        [
          "priority",
          "adopt",
          "claim",
          "start",
          "launch_unknown",
          "release",
          "transfer",
          "withdraw",
          "requeue",
          "reconcile_recipient",
        ].includes(intent.payload.operation) &&
        Number.isSafeInteger(intent.payload.expectedRevision) &&
        intent.payload.expectedRevision > 0 &&
        Number.isSafeInteger(intent.payload.cycle) &&
        intent.payload.cycle > 0 &&
        /^tsk_[a-f0-9]{16}$/.test(intent.taskId || "") &&
        /^que_[a-f0-9]{16}$/.test(intent.entryId || "") &&
        (intent.payload.reason === undefined ||
          (typeof intent.payload.reason === "string" &&
            vaultEncoder.encode(intent.payload.reason).byteLength <= 1024)) &&
        typeof intent.updatedAt === "string" &&
        vaultEncoder.encode(JSON.stringify(intent)).byteLength <=
          MAX_QUEUE_INTENT_BYTES,
    )
    .map((intent) => structuredClone(intent));
  const unique = new Map();
  for (const intent of intents)
    unique.set(`${intent.scope}\0${intent.id}`, intent);
  const out = [...unique.values()].sort((a, b) =>
    a.updatedAt.localeCompare(b.updatedAt),
  );
  if (
    out.length > MAX_QUEUE_INTENTS ||
    vaultEncoder.encode(JSON.stringify(out)).byteLength >
      MAX_QUEUE_INTENTS_BYTES
  )
    throw new Error(
      "Queue intent storage is full. Retry or discard an existing action first.",
    );
  return out;
}
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
async function vaultRecord(name) {
  const database = await db();
  return new Promise((resolve, reject) => {
    const tx = database.transaction("vault", "readonly");
    const request = tx.objectStore("vault").get(name);
    tx.oncomplete = () => resolve(request.result);
    tx.onerror = () => reject(tx.error);
    tx.onabort = () => reject(tx.error || new Error("Vault read aborted."));
  });
}
async function writeV2(envelope, legacySource) {
  if (
    envelope?.version !== 2 ||
    typeof envelope.ciphertext !== "string" ||
    envelope.ciphertext.length > 16 * 1024 * 1024
  )
    throw new Error(
      "The encrypted vault exceeds this browser's 16 MiB recovery boundary.",
    );
  const database = await db();
  return new Promise((resolve, reject) => {
    const tx = database.transaction("vault", "readwrite");
    const store = tx.objectStore("vault");
    const writeActive = () => {
      store.put(envelope, "encrypted-v2");
      store.put({ version: 2 }, "active-vault");
    };
    if (legacySource !== undefined) {
      const retained = store.get("migration-source-v1");
      retained.onsuccess = () => {
        if (retained.result === undefined)
          store.put(structuredClone(legacySource), "migration-source-v1");
        writeActive();
      };
    } else {
      writeActive();
    }
    tx.oncomplete = resolve;
    tx.onerror = () => reject(tx.error);
    tx.onabort = () => reject(tx.error || new Error("Vault save aborted."));
  });
}
async function activeEnvelope() {
  const pointer = await vaultRecord("active-vault");
  if (pointer !== undefined) {
    if (pointer?.version !== 2)
      throw new Error("Unsupported local vault namespace.");
    const envelope = await vaultRecord("encrypted-v2");
    if (!envelope)
      throw new Error(
        "The v2 vault migration is incomplete. Restore the preserved legacy backup explicitly.",
      );
    return { envelope, version: 2 };
  }
  return { envelope: await disk(), version: 1 };
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
export async function resetVault() {
  if (contents)
    throw new Error("Lock this workspace before resetting its vault.");
  const database = await db();
  return navigator.locks.request(
    "tailserve-vault-owner",
    { ifAvailable: true },
    async (lock) => {
      if (!lock)
        throw new Error(
          "This workspace is open in another browser tab. Lock or close it before resetting this vault.",
        );
      await queue;
      await new Promise((resolve, reject) => {
        const tx = database.transaction("vault", "readwrite");
        tx.objectStore("vault").clear();
        tx.oncomplete = resolve;
        tx.onerror = () => reject(tx.error);
        tx.onabort = () =>
          reject(tx.error || new Error("Vault reset aborted."));
      });
      contents = key = salt = profileUnlockKey = undefined;
      localStorage.removeItem("tailterm.username");
    },
  );
}
export async function forgetDevice() {
  requireUnlocked();
  credentialCache.clear();
  // Serialize deletion with writes. Saves queued during deletion must observe
  // the locked state, rather than re-creating a vault after the clear commits.
  const task = queue.then(async () => {
    requireUnlocked();
    const database = await db();
    await new Promise((resolve, reject) => {
      const tx = database.transaction("vault", "readwrite");
      tx.objectStore("vault").clear();
      tx.oncomplete = resolve;
      tx.onerror = () => reject(tx.error);
      tx.onabort = () =>
        reject(tx.error || new Error("Could not forget this device."));
    });
    contents = key = salt = profileUnlockKey = undefined;
    releaseLock?.();
    releaseLock = undefined;
    localStorage.removeItem("tailterm.username");
  });
  queue = task.catch(() => {});
  return task;
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
    workspace: normalizeWorkspace(contents.workspace),
    hub: { url: contents.hub?.url || "", token: contents.hub?.token || "" },
    launchProfiles: structuredClone(contents.launchProfiles || []),
    agentCatalog: structuredClone(contents.agentCatalog),
    agents: structuredClone(contents.agentCatalog.definitions),
    teamsVersion: contents.teamsVersion,
    teams: structuredClone(contents.teams),
    projectHandlerPlans: structuredClone(contents.projectHandlerPlans || []),
    teamLaunchPlans: structuredClone(contents.teamLaunchPlans || []),
    profile: structuredClone(contents.profile || {}),
    profileAppearance: structuredClone(contents.profileAppearance || null),
    backup: {
      changed: contents.backupChanged || null,
      exported: contents.backupExported || null,
    },
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
export function launchServerProfile(id) {
  requireUnlocked();
  const server = contents.servers.find((entry) => entry.id === id);
  if (!server) throw new Error("Unknown server.");
  const { password, ...profile } = server;
  return {
    ...structuredClone(profile),
    hasPassword: !!password,
    credentialRevision: server.credentialRevision || 1,
  };
}
export function privateKey(id) {
  requireUnlocked();
  return structuredClone(contents.keys.find((k) => k.id === id));
}
async function mutate(fn, backupChanged = false) {
  const task = queue.then(async () => {
    requireUnlocked();
    const next = structuredClone(contents);
    const result = await fn(next);
    const sharedChanged =
      (backupChanged || next.backupChanged !== contents.backupChanged) &&
      JSON.stringify(sharedProfileData(next)) !==
        JSON.stringify(sharedProfileData(contents));
    if (sharedChanged) {
      next.backupChanged = new Date().toISOString();
      if (next.profile?.hub) next.profile.dirty = true;
    }
    const envelope = await sealVault(next, key, salt);
    await writeV2(envelope);
    contents = next;
    if (sharedChanged) {
      profileSerial++;
      window.dispatchEvent(new Event("tailterm-profile-change"));
    }
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
    if (
      Object.entries(changes).some(
        ([field, value]) => JSON.stringify(s[field]) !== JSON.stringify(value),
      )
    )
      s.credentialRevision = (s.credentialRevision || 1) + 1;
    Object.assign(s, changes);
  }, true);
}
export async function saveWorkspace(workspace) {
  return mutate((d) => {
    d.workspace = normalizeWorkspace(workspace);
  });
}
export function hubReadCachePersistence() {
  requireUnlocked();
  const vaultKey = key;
  const requireSameVault = () => {
    requireUnlocked();
    if (key !== vaultKey)
      throw new Error(
        "Hub cache persistence belongs to a different vault unlock.",
      );
  };
  return {
    load: async () => {
      await queue;
      requireSameVault();
      return structuredClone(contents.hubReadCache || null);
    },
    save: async (value) => {
      const copy = structuredClone(value);
      await mutate((d) => {
        requireSameVault();
        d.hubReadCache = copy;
        return true;
      });
    },
  };
}
export function workItemDraftPersistence() {
  requireUnlocked();
  const vaultKey = key;
  const requireSameVault = () => {
    requireUnlocked();
    if (key !== vaultKey)
      throw new Error("Work-item drafts belong to a different vault unlock.");
  };
  return {
    load: async (scope, id) => {
      await queue;
      requireSameVault();
      const draft = (contents.workItemDrafts || []).find(
        (entry) => entry.scope === scope && entry.id === id,
      );
      return draft ? structuredClone(draft) : null;
    },
    save: async (draft) => {
      const copy = normalizeWorkItemDrafts([draft])[0];
      if (!copy) throw new Error("Invalid work-item draft.");
      await mutate((d) => {
        requireSameVault();
        d.workItemDrafts = normalizeWorkItemDrafts([
          copy,
          ...(d.workItemDrafts || []).filter(
            (entry) => entry.scope !== copy.scope || entry.id !== copy.id,
          ),
        ]);
        return true;
      });
    },
    remove: async (scope, id) =>
      mutate((d) => {
        requireSameVault();
        d.workItemDrafts = (d.workItemDrafts || []).filter(
          (entry) => entry.scope !== scope || entry.id !== id,
        );
        return true;
      }),
  };
}
export function boardIntentPersistence() {
  requireUnlocked();
  const vaultKey = key;
  const requireSameVault = () => {
    requireUnlocked();
    if (key !== vaultKey)
      throw new Error("Board intents belong to a different vault unlock.");
  };
  return {
    list: async (scope) => {
      await queue;
      requireSameVault();
      return normalizeBoardIntents(contents.boardIntents).filter(
        (intent) => intent.scope === scope,
      );
    },
    save: async (intent) => {
      const copy = normalizeBoardIntents([intent])[0];
      if (!copy) throw new Error("Invalid Board intent.");
      await mutate((data) => {
        requireSameVault();
        data.boardIntents = normalizeBoardIntents([
          ...(data.boardIntents || []).filter(
            (entry) => entry.scope !== copy.scope || entry.id !== copy.id,
          ),
          copy,
        ]);
        return true;
      });
    },
    remove: async (scope, id) =>
      mutate((data) => {
        requireSameVault();
        data.boardIntents = (data.boardIntents || []).filter(
          (entry) => entry.scope !== scope || entry.id !== id,
        );
        return true;
      }),
  };
}
export function queueIntentPersistence() {
  requireUnlocked();
  const vaultKey = key;
  const requireSameVault = () => {
    requireUnlocked();
    if (key !== vaultKey)
      throw new Error("Queue intents belong to a different vault unlock.");
  };
  return {
    list: async (scope) => {
      await queue;
      requireSameVault();
      return normalizeQueueIntents(contents.queueIntents).filter(
        (intent) => intent.scope === scope,
      );
    },
    save: async (intent) => {
      const copy = normalizeQueueIntents([intent])[0];
      if (!copy) throw new Error("Invalid Queue intent.");
      await mutate((data) => {
        requireSameVault();
        data.queueIntents = normalizeQueueIntents([
          ...(data.queueIntents || []).filter(
            (entry) => entry.scope !== copy.scope || entry.id !== copy.id,
          ),
          copy,
        ]);
        return true;
      });
    },
    remove: async (scope, id, requestId) =>
      mutate((data) => {
        requireSameVault();
        data.queueIntents = (data.queueIntents || []).filter(
          (entry) =>
            entry.scope !== scope ||
            entry.id !== id ||
            (requestId && entry.requestId !== requestId),
        );
        return true;
      }),
  };
}
export async function markBackupExported(started = new Date().toISOString()) {
  return mutate((d) => {
    d.backupExported = started;
  });
}
const editVault = (fn) => mutate(fn, true);
export async function localAPI(url, method = "GET", body = {}) {
  if (url === "/status")
    return {
      initialized: !!(await vaultRecord("active-vault")) || !!(await disk()),
      unlocked: !!contents,
      username: localStorage.getItem("tailterm.username") || "",
    };
  if (url === "/vault/legacy-envelope" && method === "GET") {
    const pointer = await vaultRecord("active-vault");
    const legacy = pointer
      ? await vaultRecord("migration-source-v1")
      : await disk();
    if (!legacy)
      throw new Error("No retained legacy vault source is available.");
    return structuredClone(legacy);
  }
  if (url === "/unlock") {
    await db();
    await acquire();
    const username = normalizeUsername(
      body.username || localStorage.getItem("tailterm.username"),
    );
    try {
      if (username && !validUsername(username))
        throw new Error("Enter a valid username.");
      const stored = await activeEnvelope();
      const envelope = stored.envelope;
      if (envelope) {
        const opened = await openVault(envelope, body.password);
        const migrated = migrateAgentData(opened.data);
        const hasLaunchPlans = Object.hasOwn(opened.data, "teamLaunchPlans");
        const launchPlans = hasLaunchPlans
          ? migrateUnsupportedUnstartedLaunchReasoning(
              opened.data.teamLaunchPlans,
            )
          : undefined;
        const restored = {
          ...opened.data,
          ...migrated,
          ...(hasLaunchPlans && { teamLaunchPlans: launchPlans }),
        };
        const agentDataChanged =
          JSON.stringify({
            agentCatalog: opened.data.agentCatalog,
            teamsVersion: opened.data.teamsVersion,
            teams: opened.data.teams,
          }) !== JSON.stringify(migrated);
        const launchPlansChanged =
          hasLaunchPlans &&
          JSON.stringify(opened.data.teamLaunchPlans) !==
            JSON.stringify(launchPlans);
        validateData(restored);
        if (
          opened.data.profile?.username &&
          restored.profile?.username &&
          restored.profile.username !== username
        )
          throw new Error(
            "This browser has a different local profile. Use its username or Forget this device first.",
          );
        if (stored.version === 1 || agentDataChanged || launchPlansChanged)
          await writeV2(
            await sealVault(restored, opened.key, opened.salt),
            envelope,
          );
        contents = restored;
        key = opened.key;
        salt = opened.salt;
      } else {
        salt = crypto.getRandomValues(new Uint8Array(16));
        key = await deriveVaultKey(body.password, salt);
        const initial = empty();
        await writeV2(await sealVault(initial, key, salt));
        contents = initial;
      }
      profileUnlockKey = username
        ? await profileMaster(body.password, username)
        : null;
      if (username) {
        contents.profile = { ...contents.profile, username };
        await writeV2(await sealVault(contents, key, salt));
        localStorage.setItem("tailterm.username", username);
      }
      void navigator.storage?.persist?.().catch(() => {});
      return localData();
    } catch (e) {
      contents = key = salt = profileUnlockKey = undefined;
      releaseLock?.();
      releaseLock = undefined;
      throw e;
    }
  }
  requireUnlocked();
  const operationKey = key;
  const sameVault = () => {
    if (!key || key !== operationKey)
      throw new Error("Vault was locked. Unlock and try again.");
  };
  if (url === "/data") return localData();
  if (url === "/lock") {
    credentialCache.clear();
    profileUnlockKey = undefined;
    await queue;
    contents = key = salt = profileUnlockKey = undefined;
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
    return editVault((d) => {
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
        runtimes: normalizeRuntimes(body.runtimes),
        credentialRevision: previous?.credentialRevision || 1,
      };
      if (
        previous?.password &&
        !body.clearPassword &&
        previous.host === server.host &&
        previous.port === server.port &&
        previous.username === server.username
      )
        server.password = previous.password;
      if (
        previous &&
        ([
          "host",
          "port",
          "username",
          "mode",
          "tmuxPath",
          "fingerprint",
          "keyId",
          "tailnet",
        ].some(
          (field) =>
            JSON.stringify(previous[field] ?? "") !==
            JSON.stringify(server[field] ?? ""),
        ) ||
          !!previous.password !== !!server.password)
      )
        server.credentialRevision++;
      if (previous) d.servers[d.servers.indexOf(previous)] = server;
      else d.servers.push(server);
    });
  if (url.startsWith("/servers/") && method === "DELETE")
    return editVault((d) => {
      const id = url.slice(9);
      const used = d.agentCatalog.definitions
        .filter((definition) => definition.serverId === id)
        .map((definition) => definition.name);
      if (used.length)
        throw new Error(
          `Remove this machine from agent definitions first: ${used.join(", ")}.`,
        );
      if (
        normalizeTeamLaunchPlans(d.teamLaunchPlans).some((plan) =>
          plan.members.some((member) => member.serverId === id),
        )
      )
        throw new Error(
          "Reconcile or discard the pending team launch that uses this machine first.",
        );
      d.servers = d.servers.filter((s) => s.id !== id);
      d.sessions = d.sessions.filter((s) => s.serverId !== id);
    });
  if (/^\/keys\/[^/]+\/public-key$/.test(url) && method === "GET") {
    const saved = contents.keys.find((k) => k.id === url.split("/")[2]);
    if (!saved) throw new Error("SSH key not found.");
    const parsed = await validatePrivateKey(
      saved.privateKey,
      saved.passphrase || "",
    );
    sameVault();
    return { publicKey: parsed.publicKey };
  }
  if (url === "/keys/generate" && method === "POST") {
    if (
      typeof body.name !== "string" ||
      !body.name.trim() ||
      body.name.length > 80
    )
      throw new Error("Enter a key name of up to 80 characters.");
    const privateKey = await generatePrivateKey();
    sameVault();
    return localAPI("/keys", "POST", { name: body.name.trim(), privateKey });
  }
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
    return editVault((d) => {
      sameVault();
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
    return editVault((d) => {
      const id = url.slice(6);
      if (d.servers.some((s) => s.keyId === id))
        throw new Error("Remove this key from server profiles first.");
      d.keys = d.keys.filter((k) => k.id !== id);
    });
  if (url === "/sessions" && method === "POST")
    return mutate((d) => {
      validateSession(body.name);
      if (body.target) validateTarget(body.target);
      if (!d.servers.some((s) => s.id === body.serverId))
        throw new Error("Unknown server.");
      let session = d.sessions.find(
        (s) => s.serverId === body.serverId && s.name === body.name,
      );
      if (!session) {
        d.backupChanged = new Date().toISOString();
        session = {
          id: crypto.randomUUID(),
          serverId: body.serverId,
          name: body.name,
        };
        d.sessions.push(session);
      }
      session.lastConnected = new Date().toISOString();
      if (body.task !== undefined) {
        const task = normalizeTaskRef(body.task);
        if (JSON.stringify(session.task || null) !== JSON.stringify(task))
          d.backupChanged = new Date().toISOString();
        if (task) session.task = task;
        else delete session.task;
      }
      if (body.target) {
        if (JSON.stringify(session.target) !== JSON.stringify(body.target))
          d.backupChanged = new Date().toISOString();
        session.target = structuredClone(body.target);
      }
    });
  if (url === "/project-handler-plans" && method === "POST")
    return mutate((d) => {
      const plan = normalizeHandlerPlan(body, d.servers);
      const plans = (d.projectHandlerPlans || []).filter(
        (p) => p.hub !== plan.hub || p.taskId !== plan.taskId,
      );
      if (plans.length >= MAX_HANDLER_PLANS)
        throw new Error(`At most ${MAX_HANDLER_PLANS} database handler plans.`);
      d.projectHandlerPlans = [...plans, plan];
    });
  if (url === "/project-handler-plans" && method === "DELETE")
    return mutate((d) => {
      const key = normalizeHandlerPlanKey(body);
      d.projectHandlerPlans = (d.projectHandlerPlans || []).filter(
        (p) => p.hub !== key.hub || p.taskId !== key.taskId,
      );
    });
  if (url === "/team-launch-plans/validate" && method === "POST") {
    normalizeTeamLaunchPlans([
      ...normalizeTeamLaunchPlans(contents.teamLaunchPlans).filter(
        (entry) => entry.id !== body.id,
      ),
      body,
    ]);
    return { ok: true };
  }
  if (url === "/team-launch-plans" && method === "POST")
    return mutate((d) => {
      const plan = normalizeTeamLaunchPlans([body])[0];
      const plans = normalizeTeamLaunchPlans(d.teamLaunchPlans).filter(
        (entry) => entry.id !== plan.id,
      );
      if (plans.length >= MAX_TEAM_LAUNCH_PLANS)
        throw new Error(
          `At most ${MAX_TEAM_LAUNCH_PLANS} unresolved team launch plans. Reconcile or discard one first.`,
        );
      d.teamLaunchPlans = normalizeTeamLaunchPlans([...plans, plan]);
    });
  if (url.startsWith("/team-launch-plans/") && method === "DELETE")
    return mutate((d) => {
      const id = url.slice("/team-launch-plans/".length);
      d.teamLaunchPlans = normalizeTeamLaunchPlans(d.teamLaunchPlans).filter(
        (entry) => entry.id !== id,
      );
    });
  if (url === "/agents" && method === "POST")
    return editVault((d) => {
      const previous = d.agentCatalog.definitions.find(
        (entry) => entry.id === body.id,
      );
      if (previous && body.revision !== previous.revision)
        throw new Error(
          "This agent changed in another editor. Reopen it and review the latest revision.",
        );
      const definition = normalizeAgentDefinition({
        ...body,
        revision: previous ? previous.revision + 1 : 1,
      });
      if (
        definition.serverId &&
        !d.servers.some((server) => server.id === definition.serverId)
      )
        throw new Error("Choose a saved machine for this agent.");
      const definitions = d.agentCatalog.definitions.filter(
        (entry) => entry.id !== definition.id,
      );
      if (!previous && definitions.length >= MAX_AGENT_DEFINITIONS)
        throw new Error(`At most ${MAX_AGENT_DEFINITIONS} agent definitions.`);
      const catalog = normalizeAgentCatalog({
        version: AGENT_CATALOG_VERSION,
        definitions: [...definitions, definition],
      });
      // Editing a shared default may change resolved aliases/roles. Validate
      // every referencing team in the same encrypted mutation before commit.
      d.teams = d.teams.map((team) => {
        let next = team;
        if (previous) {
          const reference = team.members.find(
            (member) => member.agentDefinitionId === previous.id,
          );
          const oldName = reference?.alias || previous.launchName;
          if (reference && team.orchestrator === oldName)
            next = {
              ...team,
              orchestrator: reference.alias || definition.launchName,
            };
        }
        return normalizeReferencedTeam(next, catalog.definitions);
      });
      d.agentCatalog = catalog;
    });
  if (url.startsWith("/agents/") && method === "DELETE")
    return editVault((d) => {
      const id = url.slice(8);
      const users = definitionUsers(id, d.teams);
      if (users.length)
        throw new Error(`This agent is used by teams: ${users.join(", ")}.`);
      d.agentCatalog = normalizeAgentCatalog({
        version: AGENT_CATALOG_VERSION,
        definitions: d.agentCatalog.definitions.filter(
          (entry) => entry.id !== id,
        ),
      });
    });
  if (url === "/teams" && method === "POST")
    return editVault((d) => {
      let submittedTeam = body.team || body;
      let additions = Array.isArray(body.definitions)
        ? body.definitions.map((entry) => normalizeAgentDefinition(entry))
        : [];
      // Updated callers save references. This compatibility seam converts an
      // in-memory legacy editor submission atomically; it never reads or merges
      // the retained v1 storage namespace.
      if (
        !body.team &&
        Array.isArray(body.members) &&
        body.members.some((member) => !member?.agentDefinitionId)
      ) {
        const migrated = migrateAgentData({ teams: [body] });
        submittedTeam = migrated.teams[0];
        additions = migrated.agentCatalog.definitions;
      }
      const catalog = normalizeAgentCatalog({
        version: AGENT_CATALOG_VERSION,
        definitions: [...d.agentCatalog.definitions, ...additions],
      });
      for (const definition of additions)
        if (
          definition.serverId &&
          !d.servers.some((server) => server.id === definition.serverId)
        )
          throw new Error("Choose a saved server for every agent definition.");
      const team = normalizeReferencedTeam(submittedTeam, catalog.definitions);
      const teams = d.teams.filter((t) => t.id !== team.id);
      if (teams.length >= 30) throw new Error("At most 30 teams.");
      d.agentCatalog = catalog;
      d.teams = [...teams, team];
    });
  if (url.startsWith("/teams/") && method === "DELETE")
    return editVault((d) => {
      d.teams = d.teams.filter((t) => t.id !== url.slice(7));
    });
  if (url === "/launch-profiles" && method === "POST")
    return editVault((d) => {
      const p = body;
      if (
        !/^[A-Za-z0-9_-]{1,64}$/.test(p.name || "") ||
        !d.servers.some((s) => s.id === p.serverId) ||
        typeof p.run !== "string" ||
        !p.run.trim() ||
        (p.model && !/^[A-Za-z0-9][A-Za-z0-9._:/@+-]{0,199}$/.test(p.model)) ||
        p.run.length > 1024 ||
        /[\x00-\x1f\x7f]/.test(p.run) ||
        typeof p.cwd !== "string" ||
        p.cwd.length > 512 ||
        (p.cwd && !p.cwd.startsWith("/"))
      )
        throw new Error("Invalid saved setup.");
      const profiles = (d.launchProfiles || []).filter(
        (x) => x.name !== p.name,
      );
      if (profiles.length >= 30) throw new Error("At most 30 saved setups.");
      d.launchProfiles = [
        ...profiles,
        {
          name: p.name,
          serverId: p.serverId,
          run: p.run,
          cwd: p.cwd,
          runtime: String(p.runtime || "").slice(0, 64),
          model: p.model || "",
        },
      ];
    });
  if (url === "/hub/forget-task" && method === "POST")
    return mutate((d) => {
      for (const session of d.sessions)
        if (session.task?.taskId === body.taskId) delete session.task;
    });
  if (url === "/hub" && method === "POST")
    return editVault((d) => {
      const value = String(body?.url || "").trim();
      if (value && !normalizeHubURL(value))
        throw new Error(
          "Hub URL must look like http://tailterm-hub or http://host:port.",
        );
      const token = String(body?.token || "").trim();
      if (token.length > 512 || /[\x00-\x20\x7f]/.test(token))
        throw new Error("Invalid hub token.");
      d.hub = {
        url: value ? normalizeHubURL(value) : "",
        token: value ? token : "",
      };
    });
  if (url.startsWith("/sessions/") && method === "DELETE")
    return editVault((d) => {
      d.sessions = d.sessions.filter((s) => s.id !== url.slice(10));
    });
  throw new Error("Unsupported local operation: " + method + " " + url);
}
export async function exportBackup() {
  await queue;
  requireUnlocked();
  return sealVault(portableData(contents), key, salt);
}
export async function renameSessionBookmark(serverId, name, nextName) {
  validateSession(name);
  validateSession(nextName);
  return editVault((d) => {
    const previous = d.sessions.find(
      (s) => s.serverId === serverId && s.name === name,
    );
    if (!previous) return;
    d.sessions = d.sessions.filter(
      (s) => s === previous || s.serverId !== serverId || s.name !== nextName,
    );
    previous.name = nextName;
  });
}
export async function importBackup(envelope, password) {
  const opened = await openVault(envelope, password);
  const imported = { ...opened.data, ...migrateAgentData(opened.data) };
  const hasTeams =
    Object.hasOwn(imported, "teams") ||
    Object.hasOwn(imported, "launchProfiles");
  const hasHub = Object.hasOwn(imported, "hub"),
    hasSetups = Object.hasOwn(imported, "launchProfiles");
  validateData(imported);
  // Never import another browser's node identity. Keep this browser's identity, if any.
  return editVault((d) => {
    d.servers = imported.servers;
    d.keys = imported.keys;
    d.sessions = imported.sessions;
    if (hasHub) d.hub = imported.hub;
    if (hasSetups) d.launchProfiles = imported.launchProfiles;
    if (hasTeams) {
      d.agentCatalog = imported.agentCatalog;
      d.teamsVersion = imported.teamsVersion;
      d.teams = imported.teams;
    }
    if (imported.profileAppearance)
      d.profileAppearance = imported.profileAppearance;
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
    if (
      typeof s.id !== "string" ||
      (s.credentialRevision !== undefined &&
        (!Number.isSafeInteger(s.credentialRevision) ||
          s.credentialRevision < 1))
    )
      throw new Error("Invalid server ID or credential revision.");
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
    const task = normalizeTaskRef(s.task);
    if (task) s.task = task;
    else delete s.task;
  }
  for (const s of d.servers) s.runtimes = normalizeRuntimes(s.runtimes);
  d.tailscale = d.tailscale || {};
  const hubURL = normalizeHubURL(d.hub?.url || "");
  const token = typeof d.hub?.token === "string" ? d.hub.token : "";
  if (token.length > 512 || /[\x00-\x20\x7f]/.test(token))
    throw new Error("Invalid hub token.");
  d.hub = { url: hubURL || "", token };
  const migrated = migrateAgentData(d);
  d.agentCatalog = migrated.agentCatalog;
  d.teamsVersion = migrated.teamsVersion;
  d.teams = migrated.teams;
  for (const definition of d.agentCatalog.definitions)
    if (
      definition.serverId &&
      !d.servers.some((server) => server.id === definition.serverId)
    )
      throw new Error(
        `Agent definition ${definition.name} references a missing server.`,
      );
  d.projectHandlerPlans = normalizeHandlerPlans(
    d.projectHandlerPlans,
    d.servers,
  );
  d.workItemDrafts = normalizeWorkItemDrafts(d.workItemDrafts);
  d.boardIntents = normalizeBoardIntents(d.boardIntents);
  d.queueIntents = normalizeQueueIntents(d.queueIntents);
  d.teamLaunchPlans = normalizeTeamLaunchPlans(d.teamLaunchPlans);
  d.launchProfiles = (Array.isArray(d.launchProfiles) ? d.launchProfiles : [])
    .filter(
      (p) =>
        p &&
        /^[A-Za-z0-9_-]{1,64}$/.test(p.name || "") &&
        d.servers.some((s) => s.id === p.serverId) &&
        typeof p.run === "string" &&
        p.run.trim() &&
        p.run.length <= 1024 &&
        !/[\x00-\x1f\x7f]/.test(p.run) &&
        typeof p.cwd === "string" &&
        p.cwd.length <= 512 &&
        (!p.cwd || p.cwd.startsWith("/")) &&
        (!p.model || /^[A-Za-z0-9][A-Za-z0-9._:/@+-]{0,199}$/.test(p.model)) &&
        typeof p.runtime === "string" &&
        /^[A-Za-z0-9._-]{0,64}$/.test(p.runtime),
    )
    .slice(0, 30);
}

export function profileMasterKey() {
  requireUnlocked();
  return profileUnlockKey;
}
export async function nameProfile(username, password) {
  requireUnlocked();
  username = normalizeUsername(username);
  if (!validUsername(username)) throw new Error("Enter a valid username.");
  if (contents.profile?.hub && contents.profile.username !== username)
    throw new Error("Disconnect profile sync before changing usernames.");
  const currentKey = key;
  await openVault((await activeEnvelope()).envelope, password); // Confirm this local vault's passphrase.
  const master = await profileMaster(password, username);
  if (!key || key !== currentKey)
    throw new Error("Vault was locked. Unlock and retry.");
  await mutate((d) => {
    d.profile = { ...d.profile, username };
  });
  if (!key || key !== currentKey)
    throw new Error("Vault was locked. Unlock and retry.");
  profileUnlockKey = master;
  localStorage.setItem("tailterm.username", username);
}
function sharedProfileData(d) {
  return {
    ...portableData(d),
    profileAppearance: structuredClone(d.profileAppearance || null),
  };
}
export async function profileSnapshot() {
  await queue;
  requireUnlocked();
  return {
    serial: profileSerial,
    data: sharedProfileData(contents),
  };
}
export async function saveProfileAppearance(value) {
  requireUnlocked();
  if (JSON.stringify(contents.profileAppearance) === JSON.stringify(value))
    return;
  return mutate((d) => {
    d.profileAppearance = structuredClone(value);
  }, true);
}
export async function setProfileConnection(connection, serial) {
  return mutate((d) => {
    d.profile = {
      ...d.profile,
      ...connection,
      dirty: serial !== undefined && serial !== profileSerial,
    };
  });
}
export async function applyRemoteProfile(payload, connection, serial) {
  return mutate((d) => {
    if (serial !== profileSerial)
      throw new Error(
        "Local settings changed during sync. Retry after saving.",
      );
    payload = { ...payload, ...migrateAgentData(payload) };
    validateData(payload);
    // Retain the previous local snapshot inside the encrypted local vault.
    d.profileRecovery = {
      ...portableData(d),
      profileAppearance: d.profileAppearance || null,
    };
    d.servers = payload.servers;
    d.keys = payload.keys;
    d.sessions = payload.sessions;
    d.hub = payload.hub;
    d.launchProfiles = payload.launchProfiles;
    d.agentCatalog = payload.agentCatalog;
    d.teamsVersion = payload.teamsVersion;
    d.teams = payload.teams;
    d.profileAppearance = payload.profileAppearance || null;
    d.profile = { ...d.profile, ...connection, dirty: false };
    // d.tailscale and d.workspace are deliberately device-specific.
  });
}
export async function exportProfileRecovery() {
  await queue;
  requireUnlocked();
  if (!contents.profileRecovery)
    throw new Error("No earlier local profile is stored.");
  return sealVault(contents.profileRecovery, key, salt);
}
