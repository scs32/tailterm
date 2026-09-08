const DAY_MS = 24 * 60 * 60 * 1000;

export const HUB_READ_CACHE_MAX_ENTRIES = 100;
export const HUB_READ_CACHE_MAX_BYTES = 4 * 1024 * 1024;
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
  const entries = [...unique.values()].sort(
    (left, right) => left.savedAt - right.savedAt,
  );
  while (
    entries.length &&
    (entries.length > maxEntries || byteLength(entries) > maxBytes)
  )
    entries.shift();
  return entries;
}

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

  let disposed = false;
  let operations = Promise.resolve();
  const requireActive = () => {
    if (disposed) throw new Error("Hub read cache is disposed.");
  };
  const serial = (fn) => {
    if (disposed)
      return Promise.reject(new Error("Hub read cache is disposed."));
    const task = operations.then(async () => {
      requireActive();
      return fn();
    });
    operations = task.catch(() => {});
    return task;
  };
  const time = () => {
    const value = Number(now());
    if (!Number.isFinite(value) || value < 0)
      throw new TypeError("Hub cache clock returned an invalid time.");
    return Math.trunc(value);
  };
  const read = async (at) => {
    const raw = await load();
    requireActive();
    return normalize(raw, at, { maxEntries, maxBytes, maxAgeMs });
  };

  return {
    get(scope, key) {
      requirePart(scope, "Hub cache scope");
      requirePart(key, "Hub cache key");
      return serial(async () => {
        const entries = await read(time());
        const found = entries.find(
          (entry) => entry.scope === scope && entry.key === key,
        );
        return found
          ? { value: cloneJSON(found.value), savedAt: found.savedAt }
          : null;
      });
    },

    put(scope, key, value) {
      requirePart(scope, "Hub cache scope");
      requirePart(key, "Hub cache key");
      const copy = cloneJSON(value);
      return serial(async () => {
        const at = time();
        const candidate = { scope, key, value: copy, savedAt: at };
        if (byteLength([candidate]) > maxBytes) return false;
        const entries = (await read(at)).filter(
          (entry) => entry.scope !== scope || entry.key !== key,
        );
        entries.push(candidate);
        entries.sort((left, right) => left.savedAt - right.savedAt);
        while (
          entries.length &&
          (entries.length > maxEntries || byteLength(entries) > maxBytes)
        )
          entries.shift();
        if (!entries.includes(candidate)) return false;
        requireActive();
        await save(state(cloneJSON(entries)));
        requireActive();
        return true;
      });
    },

    clear(scope) {
      if (scope !== undefined) requirePart(scope, "Hub cache scope");
      return serial(async () => {
        const entries = await read(time());
        const kept =
          scope === undefined
            ? []
            : entries.filter((entry) => entry.scope !== scope);
        const removed = entries.length - kept.length;
        if (scope === undefined || removed) {
          requireActive();
          await save(state(cloneJSON(kept)));
          requireActive();
        }
        return removed;
      });
    },

    dispose() {
      disposed = true;
    },
  };
}
