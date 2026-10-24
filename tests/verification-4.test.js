// Shared verification caches and the wasm build, split from
// tests/verification-2.test.js (wi_2b82df2dbac73bc0, order #36382) so node --test
// runs them beside the rest of that file.
import test from "node:test";
import assert from "node:assert/strict";
import {
  mkdtempSync,
  mkdirSync,
  writeFileSync,
  readFileSync,
  rmSync,
  existsSync,
  readdirSync,
  chmodSync,
  realpathSync,
  appendFileSync,
  lstatSync,
  statSync,
  symlinkSync,
} from "node:fs";
import { tmpdir, homedir, userInfo } from "node:os";
import { join, relative, isAbsolute, basename } from "node:path";
import { createHash } from "node:crypto";
import { crc32 as zlibCrc32 } from "node:zlib";
import { verificationCacheRoot } from "../scripts/package-speech-model.mjs";
import { execFileSync, spawn } from "node:child_process";
import { createServer } from "node:net";
import { once } from "node:events";
import {
  makePlan as rawMakePlan,
  runPlan,
  digest,
  receiptEligible,
  removeVerifierHome,
  canonical,
} from "../scripts/verify-matrix.mjs";
// Named separately so the red tests load on a runner that lacks these exports.
import * as matrixRunner from "../scripts/verify-matrix.mjs";
import { acquireHostLock } from "../scripts/verify-matrix-host-lock.mjs";
import {
  makePlan,
  matrix,
  fixture,
  tempDir,
  alive,
  browserFixture,
  commitChange,
} from "./verification-fixtures.mjs";

// A stand-in go's answer to the go env -json the binary helper records with
// each build, read from the stand-in's own environment.
const goEnvStandIn =
  `if [ "$1" = env ]; then printf '{"GOVERSION":"go1.99.0","GOROOT":"/stand-in","GOPATH":"%s","GOMODCACHE":"%s",` +
  `"GOCACHE":"%s","GOFLAGS":"%s","GOPROXY":"%s","GOTOOLCHAIN":"%s","GOOS":"js","GOARCH":"wasm"}\\n' ` +
  `"$GOPATH" "$GOMODCACHE" "$GOCACHE" "$GOFLAGS" "$GOPROXY" "$GOTOOLCHAIN"; exit 0; fi\n`;
const pause = (ms) => new Promise((done) => setTimeout(done, ms));

// Shared verification caches (wi_3c81378998da5acd, order #33917). Fixtures
// use a tiny hub module with one dependency served from a file:// proxy, and
// a cache root of their own; nothing here reads or writes a real cache.
const sha256Hex = (bytes) => createHash("sha256").update(bytes).digest("hex");
function cacheDirectory(t, prefix) {
  const directory = mkdtempSync(join(tmpdir(), prefix));
  // Go makes module directories read-only.
  t.after(() => existsSync(directory) && removeVerifierHome(directory));
  return directory;
}
// A stored (uncompressed) zip, enough for a Go module proxy.
function storedZip(entries) {
  const parts = [], central = [];
  let offset = 0;
  for (const [name, text] of Object.entries(entries)) {
    const data = Buffer.from(text), file = Buffer.from(name), crc = zlibCrc32(data);
    const local = Buffer.alloc(30);
    local.writeUInt32LE(0x04034b50, 0);
    local.writeUInt16LE(20, 4);
    local.writeUInt16LE(0x21, 12);
    local.writeUInt32LE(crc, 14);
    local.writeUInt32LE(data.length, 18);
    local.writeUInt32LE(data.length, 22);
    local.writeUInt16LE(file.length, 26);
    const entry = Buffer.alloc(46);
    entry.writeUInt32LE(0x02014b50, 0);
    entry.writeUInt16LE(20, 4);
    entry.writeUInt16LE(20, 6);
    entry.writeUInt16LE(0x21, 14);
    entry.writeUInt32LE(crc, 16);
    entry.writeUInt32LE(data.length, 20);
    entry.writeUInt32LE(data.length, 24);
    entry.writeUInt16LE(file.length, 28);
    entry.writeUInt32LE(offset, 42);
    parts.push(local, file, data);
    central.push(entry, file);
    offset += 30 + file.length + data.length;
  }
  const directory = Buffer.concat(central), end = Buffer.alloc(22);
  end.writeUInt32LE(0x06054b50, 0);
  end.writeUInt16LE(central.length / 2, 8);
  end.writeUInt16LE(central.length / 2, 10);
  end.writeUInt32LE(directory.length, 12);
  end.writeUInt32LE(offset, 16);
  return Buffer.concat([...parts, directory, end]);
}
const depGoMod = "module example.com/dep\n\ngo 1.21\n";
function moduleProxy(t) {
  const directory = tempDir(t, "verification-goproxy-");
  const versions = join(directory, "example.com/dep/@v");
  mkdirSync(versions, { recursive: true });
  writeFileSync(join(versions, "list"), "v1.0.0\n");
  writeFileSync(join(versions, "v1.0.0.info"), '{"Version":"v1.0.0","Time":"2026-01-01T00:00:00Z"}\n');
  writeFileSync(join(versions, "v1.0.0.mod"), depGoMod);
  writeFileSync(join(versions, "v1.0.0.zip"), storedZip({
    "example.com/dep@v1.0.0/go.mod": depGoMod,
    "example.com/dep@v1.0.0/dep.go": "package dep\n\nfunc Value() int { return 1 }\n",
    "example.com/dep@v1.0.0/README": "fixture dependency\n",
  }));
  return { directory, url: "file://" + directory };
}
// The Go environment a fixture's own setup uses: never the caller's caches.
function fixtureGoEnvironment(t, extra = {}) {
  const scratch = cacheDirectory(t, "verification-go-setup-");
  const environment = { ...process.env, GOMODCACHE: join(scratch, "mod"), GOCACHE: join(scratch, "build"),
    GOPATH: join(scratch, "path"), GOFLAGS: "-mod=mod", GOSUMDB: "off", GOTOOLCHAIN: "local", ...extra };
  for (const key of Object.keys(environment)) if (environment[key] === undefined) delete environment[key];
  return environment;
}
const countTest = (counter, name = "TestCount") =>
  `package hub\n\nimport (\n\t"os"\n\t"testing"\n\n\t"example.com/dep"\n)\n\nfunc ${name}(t *testing.T) {\n` +
  `\tf, err := os.OpenFile(${JSON.stringify(counter)}, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)\n` +
  `\tif err != nil {\n\t\tt.Fatal(err)\n\t}\n\tdefer f.Close()\n\tf.WriteString("ran\\n")\n` +
  `\tif dep.Value() != 1 {\n\t\tt.Fatal("dependency")\n\t}\n}\n`;
// A detached fixture checkout whose hub module needs example.com/dep, with
// go.sum written by go mod tidy against the proxy.
function sharedCacheFixture(t, { proxy, script = "cd hub && go version && go env GOTOOLCHAIN && go test ./...", files = {}, rules, dependency = true, extraRequire } = {}) {
  const cwd = tempDir(t, "verification-shared-cache-repo-");
  const counter = join(tempDir(t, "verification-shared-cache-counter-"), "count");
  const git = (...args) => execFileSync("git", args, { cwd, encoding: "utf8" }).trim();
  for (const dir of ["verification", "docs", "hub", "node_modules", "tests"]) mkdirSync(join(cwd, dir));
  writeFileSync(join(cwd, "verification/matrix.json"), JSON.stringify({
    version: 1, browserSuites: [], excludedBrowserSuites: [],
    rules: rules || [{ prefixes: ["docs/"], groups: ["unit"] }, { prefixes: ["hub/"], groups: ["go"] }],
  }));
  writeFileSync(join(cwd, "docs/a.md"), "a\n");
  writeFileSync(join(cwd, "package.json"), JSON.stringify({ scripts: { test: script } }));
  writeFileSync(join(cwd, ".gitignore"), "node_modules/\n");
  writeFileSync(join(cwd, "node_modules/.package-lock.json"), "{}");
  // Without the dependency a check also passes on per-home caches offline.
  writeFileSync(join(cwd, "hub/go.mod"), "module example.com/hub\n\ngo 1.21\n" + (dependency ? "\nrequire example.com/dep v1.0.0\n" : "") +
    (extraRequire ? `\nrequire ${extraRequire}\n` : ""));
  writeFileSync(join(cwd, "hub/count_test.go"), dependency ? countTest(counter)
    : countTest(counter).replace('\n\t"example.com/dep"\n', "\n").replace(/\tif dep\.Value\(\) != 1 \{\n\t\tt\.Fatal\("dependency"\)\n\t\}\n/, ""));
  for (const [name, text] of Object.entries(files)) {
    mkdirSync(join(cwd, name, ".."), { recursive: true });
    writeFileSync(join(cwd, name), text);
  }
  if (dependency)
    execFileSync("go", ["mod", "tidy"], { cwd: join(cwd, "hub"), env: fixtureGoEnvironment(t, { GOPROXY: proxy.url }), stdio: "pipe" });
  else writeFileSync(join(cwd, "hub/go.sum"), "");
  git("init", "-q");
  git("config", "user.name", "Fixture");
  git("config", "user.email", "fixture@example.invalid");
  git("add", ".");
  git("commit", "-qm", "fixture");
  const commit = git("rev-parse", "HEAD");
  git("checkout", "-q", "--detach", commit);
  const plan = (owned = ["docs/a.md"]) => makePlan({ baseCommit: commit, commit, owned }, cwd);
  const runs = () => (existsSync(counter) ? readFileSync(counter, "utf8").split("\n").filter(Boolean).length : 0);
  return { cwd, plan, runs, counter };
}
// One warm Go build cache per file, cloned into a test's root where the test
// neither corrupts nor refuses it: a cold cache is thousands of compiled
// files, the churn this feature removes. Build directories must be real
// directories inside the root, so this is a copy (a clone on APFS).
let fileBuildCache;
const goPlatform = () => `${process.platform}-${{ x64: "amd64" }[process.arch] || process.arch}`;
function templateBuildCache() {
  if (fileBuildCache) return fileBuildCache;
  const scratch = mkdtempSync(join(tmpdir(), "verification-file-build-cache-"));
  process.on("exit", () => existsSync(scratch) && removeVerifierHome(scratch));
  fileBuildCache = join(scratch, "cache");
  mkdirSync(fileBuildCache);
  writeFileSync(join(fileBuildCache, "tailterm-cache.json"),
    JSON.stringify({ version: 1, kind: "build", owns: ["build"], producer: "tests/verification-2.test.js" }));
  const module = join(scratch, "module");
  mkdirSync(module);
  writeFileSync(join(module, "go.mod"), "module example.com/template\n\ngo 1.21\n");
  writeFileSync(join(module, "t_test.go"), 'package template\n\nimport (\n\t"os"\n\t"testing"\n)\n\nfunc TestT(t *testing.T) { _ = os.Getpid() }\n');
  execFileSync("go", ["test", "./..."], { cwd: module, stdio: "pipe", env: { ...process.env, GOCACHE: fileBuildCache,
    GOMODCACHE: join(scratch, "mod"), GOPATH: join(scratch, "path"), GOFLAGS: "-count=1", GOPROXY: "off", GOTOOLCHAIN: "local" } });
  return fileBuildCache;
}
function shareBuildCache(root, version) {
  version ??= execFileSync("go", ["env", "GOVERSION"], { encoding: "utf8", env: { ...process.env, GOTOOLCHAIN: "local" } }).trim();
  const target = join(root, "build", `${version}-${goPlatform()}`);
  mkdirSync(join(root, "build"), { recursive: true });
  execFileSync("cp", [process.platform === "darwin" ? "-cR" : "-R", templateBuildCache(), target]);
  return target;
}
async function sharedRun(t, f, plan, root, proxy, extra = {}) {
  const output = tempDir(t, "verification-shared-cache-logs-"), lines = [];
  const { caches, ...rest } = extra;
  const receipt = await runPlan(plan, f.cwd, output, {
    minFreeBytes: 0,
    hostLock: { print: (line) => lines.push(line), ...(rest.hostLock || {}) },
    caches: { root, goProxy: proxy.url, ...caches },
    ...Object.fromEntries(Object.entries(rest).filter(([key]) => key !== "hostLock")),
  });
  const report = JSON.parse(readFileSync(join(output, "cache-report.json"), "utf8"));
  return { receipt, report, output, lines: lines.filter((line) => line.startsWith("matrix run: go caches:")) };
}
const logs = (receipt) => receipt.checks.map((check) => readFileSync(check.logURI, "utf8")).join("\n");
const generationOf = (root) => JSON.parse(readFileSync(join(root, "mod/current.json"), "utf8")).dir;
// Paths, sizes and sha256 of every file under a directory.
function treeSnapshot(dir) {
  const out = [];
  const visit = (path, rel) => {
    for (const name of readdirSync(path).sort()) {
      const full = join(path, name), entry = lstatSync(full);
      if (entry.isDirectory()) visit(full, rel + name + "/");
      else out.push(`${rel}${name} ${entry.size} ${entry.isFile() ? sha256Hex(readFileSync(full)) : "link"}`);
    }
  };
  visit(dir, "");
  return out;
}
const insidePath = (child, parent) => {
  const rel = relative(parent, child);
  return rel === "" || (rel.split("/")[0] !== ".." && !isAbsolute(rel));
};

test("shared caches: a run reads modules and builds from the cache root and keeps both out of its home", { timeout: 180000 }, async (t) => {
  const proxy = moduleProxy(t), f = sharedCacheFixture(t, { proxy });
  const root = join(cacheDirectory(t, "verification-cache-root-"), "root");
  shareBuildCache(root);
  const plan = f.plan();
  assert.deepEqual(plan.checks.map((check) => check.id), ["npm-unit"]);
  const first = await sharedRun(t, f, plan, root, proxy, { keepHome: true });
  const home = first.receipt.environment.HOME;
  t.after(() => existsSync(home) && removeVerifierHome(home));
  assert.equal(first.receipt.checks[0].exitCode, 0, logs(first.receipt));
  assert.equal(f.runs(), 1);
  const env = first.receipt.environment;
  const generation = generationOf(root);
  assert.equal(env.GOMODCACHE, join(realpathSync(root), "mod", generation));
  assert(env.GOCACHE.startsWith(join(realpathSync(root), "build") + "/"));
  assert(!env.GOCACHE.startsWith(home));
  for (const key of ["TMPDIR", "TMUX_TMPDIR", "GOPATH"]) assert(insidePath(env[key], home), key);
  assert.equal(env.GOPROXY, "off");
  assert.equal(env.GOTOOLCHAIN, "local");
  assert.equal(env.GOFLAGS, "-count=1 -mod=readonly");
  assert(!existsSync(join(home, "go", "pkg", "mod")), "no module cache in the kept home");
  assert(!existsSync(join(home, "go-build")), "no build cache in the kept home");
  // The checks the receipt records are the plan's, byte for byte.
  assert.equal(canonical(first.receipt.checks.map(({ id, argv, cwd, environment }) => ({ id, argv, cwd, environment }))),
    canonical(plan.checks));
  const stamp = JSON.parse(readFileSync(join(root, "mod", generation + ".stamps",
    sha256Hex(readFileSync(join(f.cwd, "hub/go.sum"))) + ".json"), "utf8"));
  for (const [file, kind] of [["tailterm-cache.json", "root"], ["mod/current.json", "mod"]]) {
    const marker = JSON.parse(readFileSync(join(root, file), "utf8"));
    assert.equal(marker.kind, kind);
    for (const key of ["version", "producer", "item", "run", "createdAt", "owns"]) assert(key in marker, key);
    assert(marker.owns.every((path) => !isAbsolute(path)));
  }
  assert.equal(stamp.goSumSha256, sha256Hex(readFileSync(join(f.cwd, "hub/go.sum"))));
  assert(stamp.owns.every((path) => !isAbsolute(path)));
  assert.match(stamp.producer, /^scripts\/verify-matrix\.mjs/);
  assert.deepEqual(first.lines.length, 1);
  assert.match(first.lines[0], new RegExp(`modules ${generation} \\(seeded 1 modules\\), toolchain go\\S+ \\(host\\)`));
  assert.equal(first.report.downloads.modules, 1);

  // Warm: the proxy is gone and any HTTP proxy is a closed port, so nothing
  // can be fetched; the module tree is identical afterwards.
  const before = treeSnapshot(join(root, "mod"));
  rmSync(proxy.directory, { recursive: true, force: true });
  const closed = createServer();
  await new Promise((done) => closed.listen(0, "127.0.0.1", done));
  const port = closed.address().port;
  await new Promise((done) => closed.close(done));
  const saved = { HTTP_PROXY: process.env.HTTP_PROXY, HTTPS_PROXY: process.env.HTTPS_PROXY };
  process.env.HTTP_PROXY = process.env.HTTPS_PROXY = `http://127.0.0.1:${port}`;
  let warm;
  try {
    warm = await sharedRun(t, f, plan, root, proxy);
  } finally {
    for (const [key, value] of Object.entries(saved))
      if (value === undefined) delete process.env[key];
      else process.env[key] = value;
  }
  assert.equal(warm.receipt.checks[0].exitCode, 0, logs(warm.receipt));
  assert.equal(f.runs(), 2, "the go test executed again");
  assert.doesNotMatch(logs(warm.receipt), /\(cached\)/);
  assert.match(warm.lines[0], new RegExp(`modules ${generation} \\(warm\\)`));
  assert.deepEqual(warm.report.downloads, { toolchains: 0, modules: 0 });
  assert.deepEqual(treeSnapshot(join(root, "mod")), before);
});

test("shared caches: real go test checks execute on every run and a cached result line refuses the receipt", { timeout: 240000 }, async (t) => {
  const proxy = moduleProxy(t);
  const f = sharedCacheFixture(t, { proxy });
  const root = join(cacheDirectory(t, "verification-cache-fresh-"), "root");
  shareBuildCache(root);
  const plan = f.plan(["hub/count_test.go"]);
  assert.deepEqual(plan.checks.map((check) => check.id).sort(), ["go-race", "go-test", "go-vet"]);
  for (const run of [1, 2]) {
    const { receipt } = await sharedRun(t, f, plan, root, proxy, { jobs: 1 });
    assert(receipt.checks.every((check) => check.exitCode === 0), logs(receipt));
    assert.doesNotMatch(logs(receipt), /\(cached\)/);
    assert.equal(f.runs(), 2 * run, "go-test and go-race each executed the test");
  }
  // Control: the same shared caches without GOFLAGS do serve a cached result.
  const env = { ...process.env, GOMODCACHE: join(root, "mod", generationOf(root)), GOPROXY: "off",
    GOCACHE: readdirSync(join(root, "build")).map((name) => join(root, "build", name))[0], GOTOOLCHAIN: "local" };
  delete env.GOFLAGS;
  const control = () => execFileSync("go", ["test", "./..."], { cwd: join(f.cwd, "hub"), env, encoding: "utf8" });
  control();
  assert.match(control(), /^ok\s+example\.com\/hub\s+\(cached\)/m);

  // A go test check whose log holds a (cached) result line leaves no receipt.
  const fake = sharedCacheFixture(t, {
    proxy,
    rules: [{ prefixes: ["docs/"], groups: ["migration"] }],
    files: { "hub/internal/store/store_test.go":
      'package store\n\nimport (\n\t"fmt"\n\t"testing"\n)\n\nfunc TestVerificationMigrationRehearsal(t *testing.T) {\n\tfmt.Println("ok  \\texample.com/hub/internal/store\\t(cached)")\n}\n' },
  });
  const output = tempDir(t, "verification-shared-cache-cached-");
  await assert.rejects(
    runPlan(fake.plan(), fake.cwd, output, { minFreeBytes: 0, caches: { root, goProxy: proxy.url } }),
    /Go test result came from the build cache: migration-rehearsal/,
  );
  assert(!existsSync(join(output, "receipt.json")));
});

test("shared caches: the sharded go-race path runs every test fresh under the check GOFLAGS, other packages included", { timeout: 180000 }, async (t) => {
  const module = tempDir(t, "verification-race-module-");
  const counter = join(tempDir(t, "verification-race-counter-"), "count");
  const counting = (pkg, name) => `package ${pkg}\n\nimport (\n\t"os"\n\t"testing"\n)\n\nfunc ${name}(t *testing.T) {\n` +
    `\tf, _ := os.OpenFile(${JSON.stringify(counter)}, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)\n\tdefer f.Close()\n\tf.WriteString("${pkg}.${name}\\n")\n}\n`;
  mkdirSync(join(module, "internal/store"), { recursive: true });
  mkdirSync(join(module, "other"));
  writeFileSync(join(module, "go.mod"), "module example.com/race\n\ngo 1.21\n");
  writeFileSync(join(module, "internal/store/a_test.go"), counting("store", "TestA"));
  writeFileSync(join(module, "internal/store/b_test.go"), counting("store", "TestB"));
  writeFileSync(join(module, "other/o_test.go"), counting("other", "TestOther"));
  // The check environment's GOFLAGS reaches every go test the command starts.
  const env = fixtureGoEnvironment(t, { GOFLAGS: "-count=1 -mod=readonly", GOPROXY: "off" });
  env.GOCACHE = shareBuildCache(join(tempDir(t, "verification-race-cache-"), "root"));
  const race = async (environment) => {
    let text = "";
    const code = await matrixRunner.runGoRace(["-shards=./internal/store=2", "./..."], {
      cwd: module, env: environment, write: (line) => (text += line),
    });
    assert.equal(code, 0, text);
    return text;
  };
  const ran = () => readFileSync(counter, "utf8").split("\n").filter(Boolean).sort();
  for (let run = 1; run <= 2; run++) {
    assert.doesNotMatch(await race(env), /\(cached\)/);
    assert.deepEqual(ran(), ["other.TestOther", "store.TestA", "store.TestB"].flatMap((name) => Array(run).fill(name)).sort());
  }
  // Control: without it the other packages' result comes from the cache (the
  // shards pass their own -count=1), and a check log saying so is refused.
  const { GOFLAGS, ...without } = env;
  void GOFLAGS;
  const count = (name) => ran().filter((entry) => entry === name).length;
  await race(without);
  assert.equal(count("other.TestOther"), 3, "a -count=1 result is never stored, so this one executes");
  await race(without);
  assert.equal(count("other.TestOther"), 3, "served from the cache, not executed");
  assert.equal(count("store.TestA"), 4);
  const cachedLine = "ok  \texample.com/race/other\t(cached)\n";
  assert.throws(() => matrixRunner.assertFreshGoTest({ id: "go-race" }, "go-race other packages: 1\n" + cachedLine),
    /Go test result came from the build cache: go-race/);
  assert.throws(() => matrixRunner.assertFreshGoTest({ id: "go-race" },
    '{"Action":"output","Output":"ok  \\texample.com/race/internal/store\\t(cached)\\n"}'), /build cache/);
  matrixRunner.assertFreshGoTest({ id: "go-race" }, "ok  \texample.com/race/other\t1.2s\n");
  matrixRunner.assertFreshGoTest({ id: "npm-unit" }, cachedLine);
});

test("shared caches: an older host Go uses the cached toolchain locally and says so once", { timeout: 180000 }, async (t) => {
  const proxy = moduleProxy(t), f = sharedCacheFixture(t, { proxy });
  const root = join(cacheDirectory(t, "verification-cache-toolchain-"), "root");
  shareBuildCache(root);
  shareBuildCache(root, "go1.21.0");
  const plan = f.plan();
  const host = await sharedRun(t, f, plan, root, proxy);
  assert.match(host.lines[0], /toolchain go\S+ \(host\)/);
  assert.match(logs(host.receipt), /^local$/m);
  // A stand-in toolchain in the generation where Go would extract go1.21.0.
  const goos = process.platform, goarch = { x64: "amd64" }[process.arch] || process.arch;
  const tree = join(root, "mod", generationOf(root), "golang.org", `toolchain@v0.0.1-go1.21.0.${goos}-${goarch}`);
  mkdirSync(join(tree, "bin"), { recursive: true });
  const realGo = execFileSync("sh", ["-c", "command -v go"], { encoding: "utf8" }).trim();
  writeFileSync(join(tree, "bin/go"),
    `#!/bin/sh\nif [ "$1" = version ]; then echo "go version go1.21.0 ${goos}/${goarch} (cached fixture toolchain)"; exit 0; fi\nexec '${realGo}' "$@"\n`,
    { mode: 0o755 });
  const older = await sharedRun(t, f, plan, root, proxy, { caches: { hostGoVersion: "go1.20.3" } });
  assert.equal(older.receipt.checks[0].exitCode, 0, logs(older.receipt));
  assert.equal(older.lines.length, 1);
  assert.match(older.lines[0], /toolchain go1\.21\.0 \(cached; host go1\.20\.3 is older than go\.mod\)/);
  assert.match(logs(older.receipt), /cached fixture toolchain/);
  assert.match(logs(older.receipt), /^local$/m);
  assert.equal(older.receipt.environment.PATH.split(":")[0], join(realpathSync(root), "mod", generationOf(root), "golang.org",
    `toolchain@v0.0.1-go1.21.0.${goos}-${goarch}`, "bin"));
  // Its tree is stamped: a changed toolchain is corruption, and a new
  // generation is made (here the download fails, so the run goes private).
  const stamp = JSON.parse(readFileSync(join(root, "mod", generationOf(root) + ".stamps",
    sha256Hex(readFileSync(join(f.cwd, "hub/go.sum"))) + ".json"), "utf8"));
  assert.match(stamp.toolchainTreeSha256, /^[0-9a-f]{64}$/);
  // A changed tree is caught by any stamp in the generation, even after a
  // go.sum change leaves no stamp for the new digest (review f2).
  writeFileSync(join(tree, "bin/extra"), "x");
  appendFileSync(join(f.cwd, "hub/go.sum"), "example.com/unused v1.0.0/go.mod h1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=\n");
  execFileSync("git", ["commit", "-qam", "new go.sum"], { cwd: f.cwd });
  const bumped = execFileSync("git", ["rev-parse", "HEAD"], { cwd: f.cwd, encoding: "utf8" }).trim();
  const newSum = makePlan({ baseCommit: bumped, commit: bumped, owned: ["docs/a.md"] }, f.cwd);
  assert(!existsSync(join(root, "mod", generationOf(root) + ".stamps",
    sha256Hex(readFileSync(join(f.cwd, "hub/go.sum"))) + ".json")), "no stamp for the new go.sum yet");
  const changed = await sharedRun(t, f, newSum, root, proxy, { caches: { hostGoVersion: "go1.20.3" } });
  assert.match(changed.lines[0], /^matrix run: go caches: private: toolchain go1\.21\.0 download failed/);
  assert(insidePath(changed.receipt.environment.GOMODCACHE, changed.receipt.environment.HOME));
});

test("shared caches: two runs at once with no module generation share the root, and a paused reader survives a new generation", { timeout: 300000 }, async (t) => {
  const proxy = moduleProxy(t);
  // No module generation yet; the build cache is the file's shared one.
  const root = join(cacheDirectory(t, "verification-cache-concurrent-"), "root");
  shareBuildCache(root);
  // Each check waits for the other's to start: the two runs hold both slots
  // at once or the test times out.
  const barrier = tempDir(t, "verification-cache-barrier-");
  const meet = (me, peer) => `touch '${join(barrier, me)}' && i=0 && while [ ! -e '${join(barrier, peer)}' ] && [ $i -lt 600 ]; do sleep 0.1; i=$((i+1)); done && [ -e '${join(barrier, peer)}' ] && cd hub && go test ./...`;
  const a = sharedCacheFixture(t, { proxy, script: meet("a", "b") }), b = sharedCacheFixture(t, { proxy, script: meet("b", "a") });
  const two = { environment: { TAILTERM_MATRIX_MAX_HOLDERS: "2", TAILTERM_MATRIX_MEMORY_PRESSURE_MAX: "off" }, maxWaitMs: 60000 };
  const both = await Promise.all([a, b].map((f) => sharedRun(t, f, f.plan(), root, proxy, { hostLock: two })));
  for (const run of both) {
    assert.equal(run.receipt.checks[0].exitCode, 0, logs(run.receipt));
    assert.doesNotMatch(logs(run.receipt), /lock|cache.*(error|corrupt)/i);
    assert.equal(run.report.mode, "shared");
  }
  assert.equal(a.runs() + b.runs(), 2);
  const current = generationOf(root);
  assert(existsSync(join(root, "mod", current)));
  for (const run of both) {
    const generation = basename(run.receipt.environment.GOMODCACHE);
    const stamp = JSON.parse(readFileSync(join(root, "mod", generation + ".stamps",
      sha256Hex(readFileSync(join(a.cwd, "hub/go.sum"))) + ".json"), "utf8"));
    assert.equal(stamp.dir, generation);
    assert.equal(execFileSync("go", ["mod", "verify"], { cwd: join(a.cwd, "hub"),
      env: fixtureGoEnvironment(t, { GOMODCACHE: run.receipt.environment.GOMODCACHE, GOPROXY: "off", GOFLAGS: "-mod=readonly" }),
      encoding: "utf8" }).trim(), "all modules verified");
  }

  // Reader A pauses inside its check until released; meanwhile a module file
  // of its generation is tampered with and peer B publishes a new generation.
  const gate = join(tempDir(t, "verification-cache-gate-"), "go");
  const started = gate + ".started";
  const paused = sharedCacheFixture(t, {
    proxy,
    script: `node -e "require('fs').writeFileSync('${started}','')" && while [ ! -e '${gate}' ]; do sleep 0.1; done && cd hub && go test ./...`,
  });
  const reader = sharedRun(t, paused, paused.plan(), root, proxy, { hostLock: two });
  const deadline = Date.now() + 120000;
  while (!existsSync(started)) {
    assert(Date.now() < deadline, "reader check started");
    await new Promise((done) => setTimeout(done, 100));
  }
  const generationA = generationOf(root);
  const readme = join(root, "mod", generationA, "example.com", "dep@v1.0.0", "README");
  chmodSync(join(readme, ".."), 0o755);
  chmodSync(readme, 0o644);
  writeFileSync(readme, "tampered\n");
  const snapshot = treeSnapshot(join(root, "mod", generationA));
  const c = sharedCacheFixture(t, { proxy });
  const peer = await sharedRun(t, c, c.plan(), root, proxy, { hostLock: two });
  assert.equal(peer.receipt.checks[0].exitCode, 0, logs(peer.receipt));
  assert.match(peer.lines[0], /\(rebuilt: integrity \(hub\)/);
  assert.notEqual(generationOf(root), generationA);
  assert.deepEqual(treeSnapshot(join(root, "mod", generationA)), snapshot, "the peer left generation A alone");
  writeFileSync(gate, "");
  const finished = await reader;
  assert.equal(finished.receipt.checks[0].exitCode, 0, logs(finished.receipt));
  assert.equal(basename(finished.receipt.environment.GOMODCACHE), generationA);
  assert.equal(paused.runs(), 1);
});

test("shared caches: corrupt or incomplete caches are repaired before use and every go test still executes", { timeout: 300000 }, async (t) => {
  const proxy = moduleProxy(t), f = sharedCacheFixture(t, { proxy });
  const root = join(cacheDirectory(t, "verification-cache-corrupt-"), "root");
  const plan = f.plan();
  const run = async (extra) => {
    const result = await sharedRun(t, f, plan, root, proxy, extra);
    assert.equal(result.receipt.checks[0].exitCode, 0, logs(result.receipt));
    return result;
  };
  await run();
  let executed = f.runs();
  const ran = () => {
    assert.equal(f.runs(), ++executed, "the go test executed");
  };
  const buildDir = () => JSON.parse(readFileSync(join(run.output || root, "cache-report.json"), "utf8"));
  const outputs = (dir) => readdirSync(dir).filter((name) => /^[0-9a-f]{2}$/.test(name))
    .flatMap((shard) => readdirSync(join(dir, shard)).filter((name) => name.endsWith("-d")).map((name) => join(dir, shard, name)));
  const build = join(root, "build", readdirSync(join(root, "build"))[0]);
  // A same-size byte flip and a truncation in stored outputs.
  const files = outputs(build).filter((path) => statSync(path).isFile() && statSync(path).size > 16);
  const flipped = files[0], truncated = files[1];
  const bytes = readFileSync(flipped);
  bytes[bytes.length >> 1] ^= 0xff;
  writeFileSync(flipped, bytes);
  writeFileSync(truncated, readFileSync(truncated).subarray(0, 8));
  // An executable directory whose file does not hash to its name.
  const exeBytes = Buffer.from("#!/bin/sh\nexit 0\n");
  const exeDir = join(build, sha256Hex(exeBytes).slice(0, 2), sha256Hex(exeBytes) + "-d");
  mkdirSync(exeDir, { recursive: true });
  writeFileSync(join(exeDir, "tool"), Buffer.from("#!/bin/sh\nexit 1\n"));
  const scanned = await run();
  assert.match(scanned.lines[0], /removed 3$/);
  for (const path of [flipped, truncated, exeDir])
    assert(!existsSync(path) || sha256Hex(readFileSync(path)) === basename(path).slice(0, 64), path);
  ran();
  void buildDir;

  // Garbage in the build marker: a fresh build directory under a new suffix.
  writeFileSync(join(build, "tailterm-cache.json"), "{garbage");
  const marker = await run();
  assert.equal(basename(marker.receipt.environment.GOCACHE), basename(build) + ".2");
  ran();

  // A tampered module file fails go mod verify: a new generation.
  const first = generationOf(root);
  const dep = join(root, "mod", first, "example.com", "dep@v1.0.0");
  chmodSync(dep, 0o755);
  chmodSync(join(dep, "README"), 0o644);
  writeFileSync(join(dep, "README"), "tampered\n");
  const tampered = await run();
  assert.match(tampered.lines[0], /\(rebuilt: integrity \(hub\): /);
  const second = generationOf(root);
  assert.notEqual(second, first);
  assert(existsSync(join(root, "mod", first)), "the old generation is left in place");
  ran();

  // A module directory and its zip deleted: incomplete, re-seeded in place.
  const removed = join(root, "mod", second, "example.com", "dep@v1.0.0");
  removeVerifierHome(removed);
  rmSync(join(root, "mod", second, "cache", "download", "example.com", "dep", "@v", "v1.0.0.zip"));
  const reseeded = await run();
  assert.match(reseeded.lines[0], new RegExp(`modules ${second} \\(seeded 1 modules\\)`));
  assert(existsSync(removed));
  ran();

  // Garbage in the pointer: a fresh generation.
  writeFileSync(join(root, "mod", "current.json"), "not json");
  const pointer = await run();
  assert.match(pointer.lines[0], /\(rebuilt: unreadable current\.json\)/);
  assert(![first, second].includes(generationOf(root)));
  ran();
});

test("shared caches: a failed seed runs the checks on per-home caches", { timeout: 180000 }, async (t) => {
  const proxy = moduleProxy(t);
  const f = sharedCacheFixture(t, { proxy, script: "cd hub && go test ./...", dependency: false });
  const plan = f.plan();
  const root = join(cacheDirectory(t, "verification-cache-seedfail-"), "root");
  const go = (args, options) =>
    args[0] === "list" && args[1] === "-m"
      ? Promise.resolve({ code: 1, stdout: "", stderr: "injected seed failure\n" })
      : matrixRunner.runGo(args, options);
  const { receipt, lines, report } = await sharedRun(t, f, plan, root, proxy, { caches: { go } });
  assert.equal(receipt.checks[0].exitCode, 0, logs(receipt));
  assert.equal(f.runs(), 1);
  assert.match(lines[0], /^matrix run: go caches: private: seeding hub failed: injected seed failure$/);
  assert.equal(report.mode, "private");
  for (const key of ["GOMODCACHE", "GOCACHE"]) assert(insidePath(receipt.environment[key], receipt.environment.HOME), key);
  assert.equal(receipt.environment.GOFLAGS, "-count=1");
});

test("shared caches: unsafe roots and pointers are refused before anything is created", { timeout: 180000 }, async (t) => {
  // The check runs no go: refused roots fall back to per-home caches, and a
  // cold per-home build per case is churn this test does not need.
  const proxy = moduleProxy(t), f = sharedCacheFixture(t, { proxy, dependency: false, script: "node -e 0" });
  const plan = f.plan();
  const parent = cacheDirectory(t, "verification-cache-unsafe-");
  const userCache = join(parent, "user-go");
  mkdirSync(join(userCache, "pkg", "mod"), { recursive: true });
  symlinkSync(join(userCache, "pkg"), join(parent, "link"));
  const refused = [
    [join(userCache, "pkg", "mod", "root"), /overlaps the user's Go cache/],
    [userCache, /overlaps the user's Go cache/],
    [parent, /overlaps the user's Go cache/],
    [join(parent, "link", "root"), /overlaps the user's Go cache/],
    [join(f.cwd, ".build", "root"), /inside the checkout/],
  ];
  const listing = () => treeSnapshot(parent).concat(readdirSync(parent));
  for (const [root, cause] of refused) {
    const before = listing();
    const { receipt, lines } = await sharedRun(t, f, plan, root, proxy, { caches: { userGoPaths: [userCache] } });
    assert.equal(receipt.checks[0].exitCode, 0, logs(receipt));
    assert.match(lines[0], cause);
    assert.match(lines[0], /^matrix run: go caches: private: /);
    assert.deepEqual(listing(), before, root);
    assert(!existsSync(join(f.cwd, ".build")));
  }
  // The real defaults: ~/go is always on the list.
  const defaultRoot = join(homedir(), "go", "tailterm-verification-never-created");
  const real = await sharedRun(t, f, plan, defaultRoot, proxy);
  assert.match(real.lines[0], /overlaps the user's Go cache/);
  assert(!existsSync(defaultRoot));

  // Pointers naming anything but a generation directly in mod/ are refused,
  // and no generation is removed or renamed by any run.
  const root = join(parent, "root");
  await sharedRun(t, f, plan, root, proxy, { caches: { userGoPaths: [userCache] } });
  const pointer = JSON.parse(readFileSync(join(root, "mod/current.json"), "utf8"));
  const outside = cacheDirectory(t, "verification-cache-outside-");
  symlinkSync(outside, join(root, "mod", "gen-0123456789abcdef"));
  for (const dir of ["../escape", "/tmp", "gen-x/../../escape", "gen-0123456789abcdef"]) {
    writeFileSync(join(root, "mod/current.json"), JSON.stringify({ ...pointer, dir }));
    const generations = readdirSync(join(root, "mod")).filter((name) => /^gen-[0-9a-f]{16}$/.test(name)).sort();
    const { receipt, lines } = await sharedRun(t, f, plan, root, proxy, { caches: { userGoPaths: [userCache] } });
    assert.equal(receipt.checks[0].exitCode, 0, logs(receipt));
    assert.match(lines[0], /^matrix run: go caches: private: current\.json /, dir);
    assert.deepEqual(readdirSync(join(root, "mod")).filter((name) => /^gen-[0-9a-f]{16}$/.test(name)).sort(), generations);
    assert.deepEqual(readdirSync(outside), []);
  }
});
