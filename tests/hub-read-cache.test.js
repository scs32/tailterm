import test from "node:test";
import assert from "node:assert/strict";
import { createHubReadCache } from "../client/hub-read-cache.js";

function memory(initial = null) {
  let saved = initial;
  return {
    load: async () => saved,
    save: async (value) => {
      saved = structuredClone(value);
    },
    saved: () => saved,
  };
}

test("hub read cache clones values on put, persistence, and get", async () => {
  const storage = memory();
  const cache = createHubReadCache({ ...storage, now: () => 100 });
  const source = { tasks: [{ id: "tsk_fixture", nested: { open: true } }] };
  assert.equal(await cache.put("profile-a", "projects", source), true);
  source.tasks[0].nested.open = false;
  assert.equal(storage.saved().entries[0].value.tasks[0].nested.open, true);

  const first = await cache.get("profile-a", "projects");
  assert.deepEqual(first, {
    value: { tasks: [{ id: "tsk_fixture", nested: { open: true } }] },
    savedAt: 100,
  });
  first.value.tasks[0].nested.open = false;
  assert.equal(
    (await cache.get("profile-a", "projects")).value.tasks[0].nested.open,
    true,
  );
});

test("hub read cache expires entries at the maximum age", async () => {
  let now = 1_000;
  const storage = memory();
  const cache = createHubReadCache({
    ...storage,
    now: () => now,
    maxAgeMs: 700,
  });
  await cache.put("profile-a", "board", { messages: [1] });
  now += 699;
  assert.ok(await cache.get("profile-a", "board"));
  now++;
  assert.equal(await cache.get("profile-a", "board"), null);

  const capped = createHubReadCache({
    ...memory({
      version: 1,
      entries: [{ scope: "scope", key: "old", value: true, savedAt: 1_000 }],
    }),
    now: () => 1_000 + 8 * 24 * 60 * 60 * 1000,
    maxAgeMs: 30 * 24 * 60 * 60 * 1000,
  });
  assert.equal(await capped.get("scope", "old"), null);
});

test("hub read cache evicts oldest entries by count and total bytes", async () => {
  let now = 10;
  const countStorage = memory();
  const countCache = createHubReadCache({
    ...countStorage,
    now: () => now++,
    maxEntries: 2,
  });
  await countCache.put("scope", "first", { n: 1 });
  await countCache.put("scope", "second", { n: 2 });
  await countCache.put("scope", "third", { n: 3 });
  assert.equal(await countCache.get("scope", "first"), null);
  assert.equal((await countCache.get("scope", "second")).value.n, 2);
  assert.equal((await countCache.get("scope", "third")).value.n, 3);

  const byteStorage = memory();
  const byteCache = createHubReadCache({
    ...byteStorage,
    now: () => now++,
    maxBytes: 230,
  });
  await byteCache.put("scope", "first", { text: "a".repeat(50) });
  await byteCache.put("scope", "second", { text: "b".repeat(50) });
  assert.ok(
    new TextEncoder().encode(JSON.stringify(byteStorage.saved())).byteLength <=
      230,
  );
  assert.equal(await byteCache.get("scope", "first"), null);
  assert.ok(await byteCache.get("scope", "second"));
  assert.equal(
    await byteCache.put("scope", "oversized", { text: "x".repeat(500) }),
    false,
  );
  assert.ok(await byteCache.get("scope", "second"));
});

test("hub read cache rejects a byte cap smaller than its empty state", () => {
  assert.throws(
    () =>
      createHubReadCache({
        ...memory(),
        maxBytes: 1,
      }),
    /at least/,
  );
});

test("hub read cache isolates and clears opaque scopes", async () => {
  const storage = memory();
  const cache = createHubReadCache({ ...storage, now: () => 50 });
  await cache.put("profile-a", "projects", { owner: "a" });
  await cache.put("profile-b", "projects", { owner: "b" });
  assert.equal((await cache.get("profile-a", "projects")).value.owner, "a");
  assert.equal((await cache.get("profile-b", "projects")).value.owner, "b");
  assert.equal(await cache.clear("profile-a"), 1);
  assert.equal(await cache.get("profile-a", "projects"), null);
  assert.equal((await cache.get("profile-b", "projects")).value.owner, "b");
  assert.equal(await cache.clear(), 1);
  assert.equal(await cache.get("profile-b", "projects"), null);
});

test("hub read cache tolerates malformed persisted data", async () => {
  const cache = createHubReadCache({
    ...memory({
      version: 1,
      entries: [
        null,
        { scope: "", key: "bad", value: 1, savedAt: 1 },
        { scope: "scope", key: "expired", value: 1, savedAt: 1 },
        { scope: "scope", key: "valid", value: { old: true }, savedAt: 90 },
        { scope: "scope", key: "valid", value: { old: false }, savedAt: 95 },
      ],
    }),
    now: () => 100,
    maxAgeMs: 20,
  });
  assert.equal(await cache.get("scope", "expired"), null);
  assert.deepEqual(await cache.get("scope", "valid"), {
    value: { old: false },
    savedAt: 95,
  });

  const invalid = createHubReadCache({
    ...memory({ version: 99, entries: "not-an-array" }),
  });
  assert.equal(await invalid.get("scope", "valid"), null);
});

test("clearing all replaces malformed persisted state", async () => {
  const storage = memory({ version: 99, entries: "not-an-array" });
  const cache = createHubReadCache(storage);
  assert.equal(await cache.clear(), 0);
  assert.deepEqual(storage.saved(), { version: 1, entries: [] });
});

test("disposing invalidates pending and future cache operations", async () => {
  let release;
  const entered = new Promise((resolve) => {
    release = resolve;
  });
  let continueLoad;
  const loading = new Promise((resolve) => {
    continueLoad = resolve;
  });
  const cache = createHubReadCache({
    load: async () => {
      release();
      return loading;
    },
    save: async () => {},
  });
  const pending = cache.get("scope", "key");
  await entered;
  cache.dispose();
  continueLoad({
    version: 1,
    entries: [{ scope: "scope", key: "key", value: 1, savedAt: Date.now() }],
  });
  await assert.rejects(pending, /disposed/);
  await assert.rejects(cache.get("scope", "key"), /disposed/);
  await assert.rejects(cache.put("scope", "key", 2), /disposed/);
  await assert.rejects(cache.clear(), /disposed/);
});
