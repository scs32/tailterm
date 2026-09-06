import test from "node:test";
import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { createVerifiedModelFetch } from "../client/verified-model-fetch.js";
const hash = (bytes) => createHash("sha256").update(bytes).digest("hex");
const bytes = new TextEncoder().encode('{"model":"test"}');
const digest = hash(bytes);
const manifest = {
  model: "owner/model",
  revision: "pinned",
  files: {
    "config.json": {
      size: bytes.length,
      sha256: digest,
      parts: [
        { path: `parts/${digest}.bin`, size: bytes.length, sha256: digest },
      ],
    },
  },
};
const url =
  "https://tailterm.example/speech-model/pinned/owner/model/config.json";
function setup(fetcher) {
  const entries = new Map();
  const cache = {
    match: async (key) => entries.get(key)?.clone(),
    put: async (key, value) => entries.set(key, value),
    delete: async (key) => entries.delete(key),
  };
  return {
    entries,
    load: createVerifiedModelFetch({
      manifest,
      origin: "https://tailterm.example",
      cacheName: "test",
      fetcher,
      cacheStorage: { open: async () => cache },
    }),
  };
}
test("model reads verify network bytes and reverify cached bytes", async () => {
  let calls = 0;
  const { entries, load } = setup(async (url, options) => {
    calls++;
    assert.ok(url.startsWith("https://tailterm.example/speech-model/"));
    assert.equal(options.redirect, "error");
    assert.equal(options.credentials, "omit");
    return new Response(bytes);
  });
  assert.deepEqual(await (await load(url)).json(), { model: "test" });
  assert.deepEqual(await (await load(url)).json(), { model: "test" });
  assert.equal(calls, 1);
  entries.set(url, new Response(new Uint8Array(bytes.length)));
  assert.deepEqual(await (await load(url)).json(), { model: "test" });
  assert.equal(calls, 2, "tampered cache is discarded and fetched again");
});
test("wrong hash, truncated and oversized model responses fail closed", async () => {
  for (const body of [
    new Uint8Array(bytes.length),
    bytes.slice(1),
    new Uint8Array(bytes.length + 1),
  ]) {
    const { load, entries } = setup(async () => new Response(body));
    await assert.rejects(load(url), /integrity|size mismatch/);
    assert.equal(entries.size, 0);
  }
});
test("models never fall back to external files or unknown paths", async () => {
  const { load } = setup(async () => {
    throw Error("Unexpected fetch");
  });
  for (const path of [
    "https://huggingface.co/owner/model/config.json",
    "/other/config.json",
    url + "?revision=other",
  ])
    await assert.rejects(load(path), /bundled local model/);
  assert.equal(
    (await load(url.replace("config.json", "optional.json"))).status,
    404,
  );
});
test("speech works with browser storage disabled", async () => {
  const load = createVerifiedModelFetch({
    manifest,
    origin: "https://tailterm.example",
    cacheName: "test",
    fetcher: async () => new Response(bytes),
    cacheStorage: {
      open: async () => {
        throw Error("Storage denied");
      },
    },
  });
  assert.deepEqual(await (await load(url)).json(), { model: "test" });
});
