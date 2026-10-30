const DAY_MS = 24 * 60 * 60 * 1000;

export const HUB_READ_CACHE_MAX_ENTRIES = 100;
export const HUB_READ_CACHE_MAX_BYTES = 8 * 1024 * 1024;
export const HUB_READ_CACHE_MAX_AGE_MS = 7 * DAY_MS;

const encoder = new TextEncoder();

function validPart(value) {
  return typeof value === "string" && value.length > 0 && value.length <= 4096;
}

function requirePart(value, label) {
  if (!validPart(value))
    throw new TypeError(`${label} must be a non-empty string.`);
}

function cloneJSON(value) {
  let text;
  try {
    text = JSON.stringify(structuredClone(value));
  } catch {
    throw new TypeError("Hub cache values must be JSON-serializable.");
  }
  if (text === undefined)
    throw new TypeError("Hub cache values must be JSON-serializable.");
  return JSON.parse(text);
}

function state(entries) {
  return { version: 1, entries };
}

function byteLength(entries) {
  return encoder.encode(JSON.stringify(state(entries))).byteLength;
}
const EMPTY_STATE_BYTES = byteLength([]);

// One persisted entry's share of the saved state: its JSON plus the
// separating comma. Summed over entries this slightly overcounts, so the
// running total never under-reports the saved size.
function entrySize(entry) {
  return encoder.encode(JSON.stringify(entry)).byteLength + 1;
}

function evict(records, { maxEntries, maxBytes }) {
  let total = records.reduce((sum, record) => sum + record.size, 0);
  while (
    records.length &&
    (records.length > maxEntries || EMPTY_STATE_BYTES + total > maxBytes)
  )
    total -= records.shift().size;
  return records;
}

function normalize(raw, time, { maxEntries, maxBytes, maxAgeMs }) {
  if (raw?.version !== 1 || !Array.isArray(raw.entries)) return [];
  const unique = new Map();
  for (const candidate of raw.entries) {
    if (
      !candidate ||
      !validPart(candidate.scope) ||
      !validPart(candidate.key) ||
      !Number.isFinite(candidate.savedAt) ||
      candidate.savedAt < 0 ||
      time - candidate.savedAt >= maxAgeMs
    )
      continue;
    let value;
    try {
      value = cloneJSON(candidate.value);
    } catch {
      continue;
    }
    const entry = {
      scope: candidate.scope,
      key: candidate.key,
      value,
      savedAt: Math.min(Math.trunc(candidate.savedAt), time),
    };
    const identity = JSON.stringify([entry.scope, entry.key]);
    const previous = unique.get(identity);
    if (!previous || previous.savedAt <= entry.savedAt)
      unique.set(identity, entry);
  }
  const records = [...unique.values()]
    .sort((left, right) => left.savedAt - right.savedAt)
    .map((entry) => ({ entry, size: entrySize(entry) }));
  return evict(records, { maxEntries, maxBytes });
}

// The persisted state is loaded and normalized once, then kept in memory:
// reads never reload it or wait for a save, and writes update memory and join
// one coalesced save. A put or clear resolves only after the save holding its
// change finishes; if that save fails it rejects, and the change stays in
// memory for the next save. A failed load is retried by the next call.
export function createHubReadCache({
  load,
  save,
  now = Date.now,
  maxEntries = HUB_READ_CACHE_MAX_ENTRIES,
  maxBytes = HUB_READ_CACHE_MAX_BYTES,
  maxAgeMs = HUB_READ_CACHE_MAX_AGE_MS,
}) {
  if (typeof load !== "function" || typeof save !== "function")
    throw new TypeError("Hub cache load and save functions are required.");
  if (!Number.isInteger(maxEntries) || maxEntries < 1)
    throw new TypeError("Hub cache maxEntries must be a positive integer.");
  if (!Number.isInteger(maxBytes) || maxBytes < 1)
    throw new TypeError("Hub cache maxBytes must be a positive integer.");
  if (maxBytes < EMPTY_STATE_BYTES)
    throw new TypeError(
      `Hub cache maxBytes must be at least ${EMPTY_STATE_BYTES}.`,
    );
  if (!Number.isFinite(maxAgeMs) || maxAgeMs <= 0)
    throw new TypeError("Hub cache maxAgeMs must be positive.");
  maxEntries = Math.min(maxEntries, HUB_READ_CACHE_MAX_ENTRIES);
  maxBytes = Math.min(maxBytes, HUB_READ_CACHE_MAX_BYTES);
  maxAgeMs = Math.min(maxAgeMs, HUB_READ_CACHE_MAX_AGE_MS);
  const bounds = { maxEntries, maxBytes, maxAgeMs };

  let disposed = false;
  // records: oldest-first { entry, size }, or null until loaded.
  let records = null;
  let loading = null;
  let saving = null;
  let next = null;
  const requireActive = () => {
    if (disposed) throw new Error("Hub read cache is disposed.");
  };
  const time = () => {
    const value = Number(now());
    if (!Number.isFinite(value) || value < 0)
      throw new TypeError("Hub cache clock returned an invalid time.");
    return Math.trunc(value);
  };
  const ready = async () => {
    requireActive();
    if (!loading) {
      const attempt = (async () => {
        const raw = await load();
        requireActive();
        records = normalize(raw, time(), bounds);
      })();
      loading = attempt;
      attempt.catch(() => {
        if (loading === attempt) loading = null;
      });
    }
    await loading;
    requireActive();
    const at = time();
    records = records.filter(({ entry }) => at - entry.savedAt < maxAgeMs);
  };
  const write = async () => {
    requireActive();
    await save(state(records.map(({ entry }) => entry)));
    requireActive();
  };
  const persist = () => {
    if (!saving) {
      saving = write().finally(() => {
        saving = null;
      });
      return saving;
    }
    // Changes made while a save is in flight share the one save after it.
    next ||= saving
      .catch(() => {})
      .then(() => {
        next = null;
        return persist();
      });
    return next;
  };

  return {
    async get(scope, key) {
      requirePart(scope, "Hub cache scope");
      requirePart(key, "Hub cache key");
      await ready();
      const found = records.find(
        ({ entry }) => entry.scope === scope && entry.key === key,
      )?.entry;
      return found
        ? { value: cloneJSON(found.value), savedAt: found.savedAt }
        : null;
    },

    async put(scope, key, value) {
      requirePart(scope, "Hub cache scope");
      requirePart(key, "Hub cache key");
      const copy = cloneJSON(value);
      await ready();
      const entry = { scope, key, value: copy, savedAt: time() };
      const candidate = { entry, size: entrySize(entry) };
      if (EMPTY_STATE_BYTES + candidate.size > maxBytes) return false;
      const kept = records.filter(
        (record) => record.entry.scope !== scope || record.entry.key !== key,
      );
      kept.push(candidate);
      kept.sort((left, right) => left.entry.savedAt - right.entry.savedAt);
      records = evict(kept, bounds);
      if (!records.includes(candidate)) return false;
      await persist();
      return true;
    },

    async clear(scope) {
      if (scope !== undefined) requirePart(scope, "Hub cache scope");
      await ready();
      const kept =
        scope === undefined
          ? []
          : records.filter((record) => record.entry.scope !== scope);
      const removed = records.length - kept.length;
      records = kept;
      if (scope === undefined || removed) await persist();
      return removed;
    },

    dispose() {
      disposed = true;
    },
  };
}
