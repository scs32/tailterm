import test from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { createHash } from "node:crypto";
import {
  copyFileSync,
  existsSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";
import { SPEECH_MODEL, SPEECH_REVISION } from "../client/speech-config.js";

const repo = fileURLToPath(new URL("..", import.meta.url));
const sha = (bytes) => createHash("sha256").update(bytes).digest("hex");
const part = (bytes) => ({ path: `parts/${sha(bytes)}.bin`, sha256: sha(bytes), size: bytes.length });
const files = {
  "config.json": Buffer.from('{"synthetic":"speech model fixture"}\n'),
  "onnx/model.onnx": Buffer.from(Array.from({ length: 300 }, (_, i) => (i * 7) % 256)),
};
// Real model and revision, so the copied config matches; synthetic small files.
const manifest = {
  model: SPEECH_MODEL,
  revision: SPEECH_REVISION,
  files: {
    "config.json": { sha256: sha(files["config.json"]), size: files["config.json"].length,
      parts: [part(files["config.json"])] },
    "onnx/model.onnx": { sha256: sha(files["onnx/model.onnx"]), size: 300,
      parts: [part(files["onnx/model.onnx"].subarray(0, 150)), part(files["onnx/model.onnx"].subarray(150))] },
  },
};

// A fresh copy of the script and its config in a scratch tree, with no
// checkout-local .build/speech-source/ unless a test writes one.
function scratchTree(t) {
  const tree = mkdtempSync(join(tmpdir(), "speech-model-tree-"));
  t.after(() => rmSync(tree, { recursive: true, force: true }));
  mkdirSync(join(tree, "scripts"));
  mkdirSync(join(tree, "client"));
  copyFileSync(join(repo, "scripts/package-speech-model.mjs"), join(tree, "scripts/package-speech-model.mjs"));
  copyFileSync(join(repo, "client/speech-config.js"), join(tree, "client/speech-config.js"));
  writeFileSync(join(tree, "client/speech-model-manifest.json"), JSON.stringify(manifest));
  const home = join(tree, "home");
  mkdirSync(home);
  return { tree, home, script: join(tree, "scripts/package-speech-model.mjs") };
}
const load = (script) => import(pathToFileURL(script).href);
const sharedFile = (cacheRoot, name) =>
  join(cacheRoot, "speech-model", SPEECH_REVISION, SPEECH_MODEL, name);
function populate(cacheRoot) {
  for (const [name, bytes] of Object.entries(files)) {
    mkdirSync(dirname(sharedFile(cacheRoot, name)), { recursive: true });
    writeFileSync(sharedFile(cacheRoot, name), bytes);
  }
}
function assertPackaged(output) {
  const target = join(output, "speech-model", SPEECH_REVISION, SPEECH_MODEL);
  for (const item of Object.values(manifest.files))
    for (const p of item.parts) assert.equal(sha(readFileSync(join(target, p.path))), p.sha256);
  assert.deepEqual(JSON.parse(readFileSync(join(target, "manifest.json"), "utf8")), manifest);
}
function serving(responses) {
  const calls = [];
  const fetch = async (url) => {
    calls.push(url);
    const name = url.split(`/resolve/${SPEECH_REVISION}/`)[1];
    const next = responses(name);
    if (next instanceof Error) throw next;
    return next;
  };
  return { calls, fetch };
}

test("a fresh checkout with the shared cache populated makes no network request", async (t) => {
  const { tree, home, script } = scratchTree(t);
  const { verificationCacheRoot } = await load(script);
  const root = verificationCacheRoot({ HOME: home });
  assert(root.startsWith(home + "/"));
  if (process.platform === "darwin") assert.equal(root, join(home, "Library/Caches/tailterm-verification"));
  populate(root);
  const output = join(tree, "out");
  const child = `let calls=0;globalThis.fetch=async()=>{calls++;throw new Error("network forbidden")};` +
    `const {packageSpeechModel}=await import(${JSON.stringify(pathToFileURL(script).href)});` +
    `const counts=await packageSpeechModel(${JSON.stringify(pathToFileURL(output + "/").href)});` +
    `console.log("RESULT "+JSON.stringify({calls,counts}));`;
  // The packaging script's own default root, from HOME alone.
  const stdout = execFileSync(process.execPath, ["--input-type=module", "-e", child], {
    env: { HOME: home }, encoding: "utf8",
  });
  const result = JSON.parse(stdout.split("\n").find((line) => line.startsWith("RESULT ")).slice(7));
  assert.equal(result.calls, 0);
  assert.deepEqual(result.counts, { files: 2, shared: 2, checkout: 0, downloaded: 0, bytes: 0 });
  assert.match(stdout, /^Speech model: files=2 shared=2 checkout=0 downloaded=0 bytes=0 /m);
  assert(!existsSync(join(tree, ".build")));
  assertPackaged(output);
});

test("a 429 after a cache miss names the file and the cause once", async (t) => {
  const { tree, script } = scratchTree(t);
  const { packageSpeechModel } = await load(script);
  const cacheRoot = join(tree, "cache");
  const { calls, fetch } = serving(() => new Response("slow down", { status: 429, statusText: "Too Many Requests" }));
  await assert.rejects(
    packageSpeechModel(pathToFileURL(join(tree, "out") + "/"), { manifest, cacheRoot: join(cacheRoot, "speech-model"), fetch }),
    (error) => {
      assert.equal(error.message,
        "Speech model download failed after a cache miss: config.json (HTTP 429 Too Many Requests)");
      return true;
    },
  );
  assert.equal(calls.length, 1);
  assert(!existsSync(sharedFile(cacheRoot, "config.json")));
});

test("a network failure after a cache miss names its error code", async (t) => {
  const { tree, script } = scratchTree(t);
  const { packageSpeechModel } = await load(script);
  const refused = Object.assign(new TypeError("fetch failed"), { cause: Object.assign(new Error("connect"), { code: "ECONNREFUSED" }) });
  const { fetch } = serving(() => refused);
  await assert.rejects(
    packageSpeechModel(pathToFileURL(join(tree, "out") + "/"), { manifest, cacheRoot: join(tree, "cache", "speech-model"), fetch }),
    { message: "Speech model download failed after a cache miss: config.json (network error ECONNREFUSED)" },
  );
});

test("a cached file with wrong bytes of the same size is refetched and replaced", async (t) => {
  const { tree, script } = scratchTree(t);
  const { packageSpeechModel } = await load(script);
  const cacheRoot = join(tree, "cache");
  populate(cacheRoot);
  const flipped = Buffer.from(files["onnx/model.onnx"]);
  flipped[150] ^= 0xff;
  writeFileSync(sharedFile(cacheRoot, "onnx/model.onnx"), flipped);
  const { calls, fetch } = serving((name) => new Response(files[name]));
  const output = join(tree, "out");
  const counts = await packageSpeechModel(pathToFileURL(output + "/"), { manifest, cacheRoot: join(cacheRoot, "speech-model"), fetch });
  assert.deepEqual(calls.map((url) => url.split("/").slice(-2).join("/")), ["onnx/model.onnx"]);
  assert.deepEqual(counts, { files: 2, shared: 1, checkout: 0, downloaded: 1, bytes: 300 });
  assert.deepEqual(readFileSync(sharedFile(cacheRoot, "onnx/model.onnx")), files["onnx/model.onnx"]);
  assertPackaged(output);
});

test("downloaded bytes that fail the manifest check are not cached", async (t) => {
  const { tree, script } = scratchTree(t);
  const { packageSpeechModel } = await load(script);
  const cacheRoot = join(tree, "cache");
  const { fetch } = serving((name) => new Response(Buffer.alloc(files[name].length)));
  await assert.rejects(
    packageSpeechModel(pathToFileURL(join(tree, "out") + "/"), { manifest, cacheRoot: join(cacheRoot, "speech-model"), fetch }),
    /integrity check failed/,
  );
  assert(!existsSync(sharedFile(cacheRoot, "config.json")));
});

test("a good checkout-local copy is used and copied up without a fetch", async (t) => {
  const { tree, script } = scratchTree(t);
  const { packageSpeechModel } = await load(script);
  for (const [name, bytes] of Object.entries(files)) {
    mkdirSync(dirname(join(tree, ".build/speech-source", name)), { recursive: true });
    writeFileSync(join(tree, ".build/speech-source", name), bytes);
  }
  const cacheRoot = join(tree, "cache");
  const { calls, fetch } = serving(() => new Error("network forbidden"));
  const output = join(tree, "out");
  const counts = await packageSpeechModel(pathToFileURL(output + "/"), { manifest, cacheRoot: join(cacheRoot, "speech-model"), fetch });
  assert.equal(calls.length, 0);
  assert.deepEqual(counts, { files: 2, shared: 0, checkout: 2, downloaded: 0, bytes: 0 });
  for (const [name, bytes] of Object.entries(files))
    assert.deepEqual(readFileSync(sharedFile(cacheRoot, name)), bytes);
  assertPackaged(output);
});

test("an absolute TAILTERM_VERIFICATION_CACHE names the root; a relative one is ignored", async (t) => {
  const { home, script } = scratchTree(t);
  const { verificationCacheRoot } = await load(script);
  assert.equal(verificationCacheRoot({ HOME: home, TAILTERM_VERIFICATION_CACHE: "/srv/cache" }), "/srv/cache");
  assert.equal(
    verificationCacheRoot({ HOME: home, TAILTERM_VERIFICATION_CACHE: "relative" }),
    verificationCacheRoot({ HOME: home }),
  );
});
