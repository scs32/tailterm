import test from "node:test";
import assert from "node:assert/strict";
import { createHubClient } from "../client/hub-client.js";
import { createHubReadCache } from "../client/hub-read-cache.js";
import { createCachedHubClient } from "../client/cached-hub-client.js";

function fixture() {
  let disk, connected = true, response = { tasks: [{ id: "one", name: "Saved project" }] };
  let gate, status = 200, requests = [];
  const cache = () => createHubReadCache({ load: async () => disk, save: async (value) => { disk = structuredClone(value); } });
  const live = createHubClient({ baseURL: "http://fixture", token: "first", fetchImpl: async (url, init) => {
    requests.push({ url, method: init.method });
    if (!connected) throw Error("Synthetic offline");
    const body = gate ? await gate : structuredClone(response);
    return { status, text: async () => JSON.stringify(body) };
  } });
  const make = (client = live) => createCachedHubClient({ client, cache: cache(), online: () => connected });
  return { live, cache, make, requests,
    offline() { connected = false; },
    response(value) { response = value; },
    hold(value) { gate = value; },
    status(value) { status = value; },
  };
}

test("saved views return before a slow refresh and update when it finishes", async () => {
  const f = fixture(), view = f.make();
  try {
    assert.equal((await view.listTasks())[0].name, "Saved project");
    let finish;
    f.hold(new Promise((resolve) => { finish = resolve; }));
    const refreshing = view.refreshConnection();
    const value = await Promise.race([view.listTasks(), new Promise((_, reject) => setTimeout(() => reject(Error("cached read waited for network")), 250))]);
    assert.equal(value[0].name, "Saved project");
    assert.match(view.cacheStatus().label, /refreshing/);
    finish({ tasks: [{ id: "one", name: "Updated project" }] });
    await refreshing;
    assert.equal((await view.listTasks())[0].name, "Updated project");
  } finally { view.dispose(); }
});

test("reload can read saved data offline while the authoritative client cannot", async () => {
  const f = fixture(), first = f.make();
  await first.listTasks(); first.dispose(); f.offline();
  const restored = f.make();
  try {
    assert.equal((await restored.listTasks())[0].name, "Saved project");
    assert.match(restored.cacheStatus().label, /offline/);
    await assert.rejects(f.live.listTasks(), /Synthetic offline/);
  } finally { restored.dispose(); }
});

test("credential scopes do not share saved records", async () => {
  const f = fixture(), first = f.make();
  await first.listTasks(); first.dispose(); f.offline();
  const other = f.make({ ...f.live, token: "different" });
  try { await assert.rejects(other.listTasks(), /not been saved/); }
  finally { other.dispose(); }
});

test("confirmed writes invalidate saved reads and failed writes never replay", async () => {
  const f = fixture(), view = f.make();
  try {
    await view.listTasks();
    f.response({ tasks: [{ id: "one", name: "Confirmed update" }] });
    await view.postMessage("one", { text: "synthetic" });
    assert.equal((await view.listTasks())[0].name, "Confirmed update");
    f.offline();
    await assert.rejects(view.postMessage("one", { text: "do not queue" }), /Synthetic offline/);
    await view.listTasks(); await view.refreshConnection();
    assert.equal(f.requests.filter((r) => r.method === "POST").length, 2);
  } finally { view.dispose(); }
});

test("authorization errors clear saved records instead of serving them as offline", async () => {
  const f = fixture(), view = f.make();
  try {
    await view.listTasks(); f.status(401); f.response({ error: "Expired credential" });
    await view.refreshConnection();
    await assert.rejects(view.listTasks(), /Expired credential/);
    view.dispose(); f.offline();
    const restored = f.make();
    try { await assert.rejects(restored.listTasks(), /not been saved/); }
    finally { restored.dispose(); }
  } finally { view.dispose(); }
});

test("locking during refresh prevents the old response from entering the cache", async () => {
  const f = fixture(), view = f.make();
  await view.listTasks();
  let finish; f.hold(new Promise((resolve) => { finish = resolve; }));
  const refresh = view.refreshConnection();
  view.dispose(); finish({ tasks: [{ id: "private", name: "Late response" }] });
  await refresh; f.offline();
  const restored = f.make();
  try { assert.equal((await restored.listTasks())[0].name, "Saved project"); }
  finally { restored.dispose(); }
});

test("Board refreshes fetch only new immutable messages after the saved cursor", async () => {
  let disk;
  const requests = [];
  const client = createHubClient({ baseURL: "http://fixture", fetchImpl: async (url) => {
    requests.push(url);
    const after = new URL(url).searchParams.get("after");
    return { status: 200, text: async () => JSON.stringify({ messages: after ? [{ seq: 4, text: "New" }] : [{ seq: 2, text: "Old" }, { seq: 3, text: "Saved" }] }) };
  } });
  const view = createCachedHubClient({ client, cache: createHubReadCache({ load: async () => disk, save: async (value) => { disk = value; } }) });
  try {
    await view.listMessages("one", { limit: 200, latest: 1 });
    await view.refreshConnection();
    assert.match(requests[1], /after=3/);
    assert.doesNotMatch(requests[1], /latest=/);
    assert.deepEqual((await view.listMessages("one", { limit: 200, latest: 1 })).map((m) => m.seq), [2, 3, 4]);
  } finally { view.dispose(); }
});
