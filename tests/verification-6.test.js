// Shared cache plans, containment and the wasm build, split from
// tests/verification-4.test.js (wi_f60103c0670388c8, order #44897) so
// node --test runs them beside the rest of that file.
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
  lstatSync,
  symlinkSync,
} from "node:fs";
import { tmpdir, userInfo } from "node:os";
import { join, relative, isAbsolute } from "node:path";
import { createHash } from "node:crypto";
import { crc32 as zlibCrc32 } from "node:zlib";
import { verificationCacheRoot } from "../scripts/package-speech-model.mjs";
import { execFileSync, spawn } from "node:child_process";
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


test("shared caches: which plans share, and the default root's containment", (t) => {
  const f = fixture(t);
  const ids = (...list) => list.map((id) => ({ id, cwd: id.startsWith("go") ? "hub" : ".", argv: ["true"], environment: {} }));
  // A synthetic plan with no hub/go.sum keeps per-home caches, whatever its ids.
  assert.equal(matrixRunner.sharesGoCaches(ids("npm-unit"), f.cwd), false);
  mkdirSync(join(f.cwd, "hub"));
  writeFileSync(join(f.cwd, "hub/go.sum"), "");
  for (const id of ["npm-unit", "go-test", "go-vet", "go-race", "migration-rehearsal", "wasm-test-build",
    "00-static-build", "01-static-release-verify", "tests/static-browser.mjs:webkit", "tests/usage-browser.mjs"])
    assert.equal(matrixRunner.sharesGoCaches(ids(id), f.cwd), true, id);
  for (const id of ["plain", "fixture-browser", "timeout-boundary", "probe-hub"])
    assert.equal(matrixRunner.sharesGoCaches(ids(id), f.cwd), false, id);
  // The plan itself never depends on the cache root.
  const repo = new URL("..", import.meta.url).pathname;
  const head = execFileSync("git", ["rev-parse", "HEAD"], { cwd: repo, encoding: "utf8" }).trim();
  const context = { baseCommit: head, commit: head, owned: ["docs/objective-verification.md"] };
  const saved = process.env.TAILTERM_VERIFICATION_CACHE;
  const without = (() => { delete process.env.TAILTERM_VERIFICATION_CACHE; return rawMakePlan({ ...context, approvedMatrixDigest: digest(readFileSync(join(repo, "verification/matrix.json"), "utf8")), matrixApprovalMessageSeq: 1 }, repo); })();
  process.env.TAILTERM_VERIFICATION_CACHE = "/srv/tailterm-verification";
  try {
    const withRoot = rawMakePlan({ ...context, approvedMatrixDigest: digest(readFileSync(join(repo, "verification/matrix.json"), "utf8")), matrixApprovalMessageSeq: 1 }, repo);
    assert.equal(canonical(withRoot), canonical(without));
  } finally {
    if (saved === undefined) delete process.env.TAILTERM_VERIFICATION_CACHE;
    else process.env.TAILTERM_VERIFICATION_CACHE = saved;
  }
  // The runner keeps its own copy of the packaging script's root rule.
  for (const environment of [{ HOME: "/h" }, { HOME: "/h", TAILTERM_VERIFICATION_CACHE: "/srv/c" },
    { HOME: "/h", TAILTERM_VERIFICATION_CACHE: "rel" }, { HOME: "/h", XDG_CACHE_HOME: "/x" }, { HOME: "/h", XDG_CACHE_HOME: "rel" }])
    assert.equal(matrixRunner.verificationCacheRoot(environment), verificationCacheRoot(environment), JSON.stringify(environment));
  // The default root for the user's real home: in the user's caches, outside
  // every worktree, the artifacts root and the temporary directories.
  const home = userInfo().homedir;
  const root = verificationCacheRoot({ HOME: home });
  if (process.platform === "darwin") assert(insidePath(root, join(home, "Library", "Caches")));
  const worktrees = execFileSync("git", ["worktree", "list", "--porcelain"], { cwd: repo, encoding: "utf8" })
    .split("\n").filter((line) => line.startsWith("worktree ")).map((line) => line.slice(9));
  for (const path of [...worktrees, join(home, "projects", "tailterm-artifacts"), "/private/tmp", "/tmp", tmpdir()]) {
    assert(!insidePath(root, path), path);
    if (existsSync(path)) assert(!insidePath(root, realpathSync(path)), path);
  }
});

test("shared caches: prepared test binaries are built in the verifier environment, never with the user's Go caches", { timeout: 120000 }, async (t) => {
  // A stand-in go first on PATH records the environment each build sees; the
  // runner's own go calls are injected, so nothing real is built.
  const tools = tempDir(t, "verification-binary-env-"), calls = join(tools, "builds.jsonl");
  writeFileSync(join(tools, "go"),
    `#!/bin/sh\n${goEnvStandIn}[ "$1" = build ] || exit 1\n` +
    `printf '{"HOME":"%s","GOMODCACHE":"%s","GOCACHE":"%s","GOPROXY":"%s","GOFLAGS":"%s","GOTOOLCHAIN":"%s"}\\n' "$HOME" "$GOMODCACHE" "$GOCACHE" "$GOPROXY" "$GOFLAGS" "$GOTOOLCHAIN" >> '${calls}'\n` +
    `out=\nwhile [ $# -gt 0 ]; do [ "$1" = -o ] && out=$2; shift; done\nprintf '#!/bin/sh\\nexit 0\\n' > "$out" && chmod +x "$out"\n`,
    { mode: 0o755 });
  const f = browserFixture(t, { "profile-sync-browser.mjs": "import './test-binaries.mjs';process.exit(0);" });
  writeFileSync(join(f.cwd, "tests/test-binaries.mjs"), "export {};\n");
  mkdirSync(join(f.cwd, "hub"));
  writeFileSync(join(f.cwd, "hub/go.mod"), "module fixture\n\ngo 1.22\n");
  writeFileSync(join(f.cwd, "hub/go.sum"), "");
  const commit = commitChange(f, "hub/cmd/tt/main.go", "package main\n\nfunc main() {}\n", "hub tree");
  const plan = makePlan({ baseCommit: commit, commit, owned: ["client/"] }, f.cwd);
  assert(plan.checks.some((check) => check.id === "tests/profile-sync-browser.mjs"));
  const root = join(cacheDirectory(t, "verification-cache-binaries-"), "root");
  const go = async (args) => ({ code: 0, stdout: args[0] === "env" ? "go1.99.0\n" : "", stderr: "" });
  const output = tempDir(t, "verification-binary-env-logs-");
  const savedPath = process.env.PATH;
  process.env.PATH = tools + ":" + savedPath;
  let receipt;
  try {
    receipt = await runPlan(plan, f.cwd, output, { minFreeBytes: 0, caches: { root, go, userGoPaths: [] } });
  } finally {
    process.env.PATH = savedPath;
  }
  assert(receiptEligible(receipt), JSON.stringify(receipt.checks.map((check) => [check.id, check.exitCode])));
  const builds = readFileSync(calls, "utf8").trim().split("\n").map((line) => JSON.parse(line));
  assert.equal(builds.length, 2, "the hub and tt test binaries");
  const env = receipt.environment, home = userInfo().homedir;
  assert(env.GOMODCACHE.startsWith(join(realpathSync(root), "mod", "gen-")));
  for (const build of builds) {
    assert.deepEqual(build, { HOME: env.HOME, GOMODCACHE: env.GOMODCACHE, GOCACHE: env.GOCACHE,
      GOPROXY: "off", GOFLAGS: "-count=1 -mod=readonly", GOTOOLCHAIN: "local" });
    for (const key of ["GOMODCACHE", "GOCACHE"])
      for (const user of [join(home, "go"), join(home, "Library", "Caches", "go-build")])
        assert(!insidePath(build[key], user), `${key} ${build[key]} is under ${user}`);
  }
  // go env is recorded with each build, from the same environment.
  const recorded = JSON.parse(readFileSync(join(output, "test-binary-builds.json"), "utf8"));
  assert.deepEqual(recorded.map((build) => build.target), ["hub", "tt"]);
  for (const build of recorded) {
    assert.deepEqual(Object.keys(build.buildLog.goEnv).sort(), [
      "GOARCH", "GOCACHE", "GOFLAGS", "GOMODCACHE", "GOOS", "GOPATH", "GOPROXY", "GOROOT", "GOTOOLCHAIN", "GOVERSION"]);
    assert.equal(build.buildLog.goEnv.GOMODCACHE, env.GOMODCACHE);
    assert.equal(build.buildLog.goEnv.GOCACHE, env.GOCACHE);
    for (const key of ["GOMODCACHE", "GOCACHE"])
      for (const user of [join(home, "go"), join(home, "Library", "Caches", "go-build")])
        assert(!insidePath(build.buildLog.goEnv[key], user), `${key} is under ${user}`);
  }
});

test("test binaries: the helper reads no environment at import and builds with the one it is given", async (t) => {
  // This process's PATH finds the real go; only the given environment's PATH
  // finds the stand-in, which records what each call saw.
  const tools = tempDir(t, "verification-binary-given-env-"), calls = join(tools, "calls.jsonl");
  writeFileSync(join(tools, "go"),
    `#!/bin/sh\nprintf '{"call":"%s","GOMODCACHE":"%s","GOCACHE":"%s","HOME":"%s"}\\n' "$1" "$GOMODCACHE" "$GOCACHE" "$HOME" >> '${calls}'\n` +
    `${goEnvStandIn}[ "$1" = build ] || exit 1\nout=\nwhile [ $# -gt 0 ]; do [ "$1" = -o ] && out=$2; shift; done\n` +
    `printf '#!/bin/sh\\nexit 0\\n' > "$out" && chmod +x "$out"\n`,
    { mode: 0o755 });
  const root = tempDir(t, "verification-binary-given-root-");
  mkdirSync(join(root, "hub/cmd/tt"), { recursive: true });
  writeFileSync(join(root, "hub/go.mod"), "module fixture\n\ngo 1.22\n");
  writeFileSync(join(root, "hub/cmd/tt/main.go"), "package main\n\nfunc main() {}\n");
  const caches = tempDir(t, "verification-binary-given-caches-"), output = join(caches, "out");
  const env = { PATH: tools + ":/usr/bin:/bin", HOME: caches, GOMODCACHE: join(caches, "mod"), GOCACHE: join(caches, "build"),
    TAILTERM_TASK: "dropped" };
  assert(!String(process.env.PATH).split(":").includes(tools));
  const { buildMatrixBinaries } = await import("./test-binaries.mjs");
  const binaries = await buildMatrixBinaries(root, output, { env });
  assert.deepEqual(binaries.map((binary) => binary.target), ["hub", "tt"]);
  const seen = readFileSync(calls, "utf8").trim().split("\n").map((line) => JSON.parse(line));
  assert.deepEqual(seen.map((call) => call.call), ["env", "build", "env", "build"]);
  for (const call of seen)
    assert.deepEqual(call, { call: call.call, GOMODCACHE: env.GOMODCACHE, GOCACHE: env.GOCACHE, HOME: caches });
  for (const binary of binaries) {
    assert.equal(binary.buildLog.goEnv.GOMODCACHE, env.GOMODCACHE);
    assert.equal(binary.buildLog.goEnv.GOCACHE, env.GOCACHE);
  }
  // A go env that fails names the build it was for.
  writeFileSync(join(tools, "go"), "#!/bin/sh\necho 'go: broken stand-in' >&2\nexit 2\n", { mode: 0o755 });
  await assert.rejects(() => buildMatrixBinaries(root, output, { env }), /^Error: go env failed for hub build: go: broken stand-in$/);
});

test("shared caches: aborting test-binary preparation stops go build and its children with the run", { timeout: 120000 }, async (t) => {
  // The stand-in go build records its pid and a sleep it starts, then waits:
  // only a signal to the whole preparation group ends both.
  const tools = tempDir(t, "verification-binary-abort-"), pids = join(tools, "pids");
  writeFileSync(join(tools, "go"),
    `#!/bin/sh\n${goEnvStandIn}[ "$1" = build ] || exit 1\necho $$ >> '${pids}'\nsleep 300 &\necho $! >> '${pids}'\nwait\n`,
    { mode: 0o755 });
  const f = browserFixture(t, { "profile-sync-browser.mjs": "import './test-binaries.mjs';process.exit(0);" });
  writeFileSync(join(f.cwd, "tests/test-binaries.mjs"), "export {};\n");
  mkdirSync(join(f.cwd, "hub"));
  writeFileSync(join(f.cwd, "hub/go.mod"), "module fixture\n\ngo 1.22\n");
  writeFileSync(join(f.cwd, "hub/go.sum"), "");
  const commit = commitChange(f, "hub/cmd/tt/main.go", "package main\n\nfunc main() {}\n", "hub tree");
  const plan = makePlan({ baseCommit: commit, commit, owned: ["client/"] }, f.cwd);
  const root = join(cacheDirectory(t, "verification-cache-binary-abort-"), "root");
  const go = async (args) => ({ code: 0, stdout: args[0] === "env" ? "go1.99.0\n" : "", stderr: "" });
  const output = tempDir(t, "verification-binary-abort-logs-");
  const controller = new AbortController(), groups = [], home = { path: "" };
  const savedPath = process.env.PATH;
  process.env.PATH = tools + ":" + savedPath;
  let run;
  try {
    run = runPlan(plan, f.cwd, output, {
      minFreeBytes: 0,
      abortSignal: controller.signal,
      caches: { root, go, userGoPaths: [] },
      createHome: () => (home.path = mkdtempSync(join(realpathSync("/tmp"), "tv-"))),
      acquireHostLock: async (args) => {
        const lease = await acquireHostLock(args);
        return { ...lease,
          addGroup: (pgid) => (groups.push([pgid, true]), lease.addGroup(pgid)),
          removeGroup: (pgid) => (groups.push([pgid, false]), lease.removeGroup(pgid)) };
      },
    });
    run.catch(() => {});
    const started = Date.now();
    const recorded = () => (existsSync(pids) ? readFileSync(pids, "utf8").trim().split("\n").filter(Boolean).map(Number) : []);
    while (recorded().length < 2) {
      assert(Date.now() - started < 60000, "the stand-in build never started");
      await pause(25);
    }
  } finally {
    process.env.PATH = savedPath;
  }
  const [build, sleeper] = readFileSync(pids, "utf8").trim().split("\n").map(Number);
  assert(alive(build) && alive(sleeper));
  controller.abort("SIGTERM");
  await assert.rejects(run, /Verification interrupted by SIGTERM/);
  const rejected = Date.now();
  while ((alive(build) || alive(sleeper)) && Date.now() - rejected < 2000) await pause(25);
  assert(!alive(build), "go build survived the abort");
  assert(!alive(sleeper), "a child of go build survived the abort");
  assert.equal(groups.length, 2, JSON.stringify(groups));
  assert.deepEqual(groups.map(([, running]) => running), [true, false]);
  assert.equal(groups[0][0], groups[1][0]);
  assert(!existsSync(join(output, "test-binaries.json")));
  assert(!existsSync(join(output, "receipt.json")));
  assert.notEqual(home.path, "");
  assert(!existsSync(home.path), "the aborted run removed its home");
});

test("shared caches: seeding past its deadline runs the checks on per-home caches and says why", { timeout: 120000 }, async (t) => {
  const proxy = moduleProxy(t);
  const f = sharedCacheFixture(t, { proxy, script: "cd hub && go test ./...", dependency: false });
  const root = join(cacheDirectory(t, "verification-cache-deadline-"), "root");
  // A stand-in that never returns from seeding until it is signalled.
  let stalled = 0;
  const go = (args, options) =>
    args[0] === "list" && args[1] === "-m"
      ? new Promise((done) => {
          stalled++;
          options.abortSignal.addEventListener("abort", () => done({ code: -1, stdout: "", stderr: "stopped" }), { once: true });
        })
      : matrixRunner.runGo(args, options);
  const started = Date.now();
  const { receipt, lines, report } = await sharedRun(t, f, f.plan(), root, proxy, { caches: { go, deadlineMs: 1500 } });
  assert.equal(stalled, 1);
  assert(Date.now() - started < 60000);
  assert.equal(receipt.checks[0].exitCode, 0, logs(receipt));
  assert.equal(f.runs(), 1);
  assert.equal(lines[0], "matrix run: go caches: private: cache preparation exceeded its 1500 ms deadline");
  assert.equal(report.mode, "private");
  for (const key of ["GOMODCACHE", "GOCACHE"]) assert(insidePath(receipt.environment[key], receipt.environment.HOME), key);
});

test("shared caches: a build directory that is a symlink or leaves the root is refused", { timeout: 120000 }, async (t) => {
  const proxy = moduleProxy(t);
  const f = sharedCacheFixture(t, { proxy, script: "node -e 0", dependency: false });
  const host = execFileSync("go", ["env", "GOVERSION"], { encoding: "utf8", env: { ...process.env, GOTOOLCHAIN: "local" } }).trim();
  for (const shape of ["directory", "parent"]) {
    const root = join(cacheDirectory(t, "verification-cache-buildlink-"), "root");
    const elsewhere = cacheDirectory(t, "verification-cache-elsewhere-");
    writeFileSync(join(elsewhere, "tailterm-cache.json"), JSON.stringify({ version: 1, kind: "build", owns: ["build"] }));
    mkdirSync(root);
    if (shape === "directory") {
      mkdirSync(join(root, "build"));
      symlinkSync(elsewhere, join(root, "build", `${host}-${goPlatform()}`));
    } else {
      mkdirSync(join(elsewhere, `${host}-${goPlatform()}`));
      writeFileSync(join(elsewhere, `${host}-${goPlatform()}`, "tailterm-cache.json"),
        JSON.stringify({ version: 1, kind: "build", owns: ["build"] }));
      symlinkSync(elsewhere, join(root, "build"));
    }
    const before = treeSnapshot(elsewhere);
    const { receipt, lines } = await sharedRun(t, f, f.plan(), root, proxy);
    assert.equal(receipt.checks[0].exitCode, 0, logs(receipt));
    assert.match(lines[0], /^matrix run: go caches: private: build directory go\S+ resolves outside /, shape);
    assert.deepEqual(treeSnapshot(elsewhere), before, shape);
    assert(!existsSync(join(root, "mod")), shape);
  }
});

test("shared caches: the wasm seed plan is read from build-wasm.sh or the check keeps per-home modules", (t) => {
  const cwd = tempDir(t, "verification-wasm-spec-");
  mkdirSync(join(cwd, "scripts"));
  mkdirSync(join(cwd, "hub"));
  const script = (version) => `#!/bin/sh\nsource="$root/.build/tailscale-${version}"\n` +
    `cp wasm/upstream_js.go "$source/cmd/tsconnect/wasm/wasm_js.go"\ncp wasm/ssh_js.go "$source/cmd/tsconnect/wasm/ssh_js.go"\n`;
  writeFileSync(join(cwd, "hub/go.mod"), "module m\n\ngo 1.21\n\nrequire (\n\ttailscale.com v1.102.3\n)\n");
  writeFileSync(join(cwd, "scripts/build-wasm.sh"), script("1.102.3"));
  assert.deepEqual(matrixRunner.wasmSeedSpec(cwd), { version: "1.102.3",
    copies: [{ from: "upstream_js.go", to: "wasm_js.go" }, { from: "ssh_js.go", to: "ssh_js.go" }] });
  writeFileSync(join(cwd, "scripts/build-wasm.sh"), script("1.104.0"));
  assert.equal(matrixRunner.wasmSeedSpec(cwd).cause, "build-wasm.sh builds tailscale 1.104.0 but hub/go.mod requires 1.102.3");
  writeFileSync(join(cwd, "scripts/build-wasm.sh"), "#!/bin/sh\nmake wasm\n");
  assert.equal(matrixRunner.wasmSeedSpec(cwd).cause, "build-wasm.sh has no Tailscale version or copy lines");
  // The real script parses and matches the real hub/go.mod: its version and
  // every one of its copies into the wasm package.
  const repo = new URL("..", import.meta.url).pathname;
  const real = matrixRunner.wasmSeedSpec(repo);
  assert.match(real.version, /^\d+\.\d+\.\d+$/, JSON.stringify(real));
  const realScript = readFileSync(join(repo, "scripts/build-wasm.sh"), "utf8");
  assert.equal(real.copies.length, realScript.split("\n").filter((line) => line.startsWith("cp wasm/")).length);
  assert.deepEqual(real.copies, ["upstream_js.go:wasm_js.go", "ssh_js.go", "upload_js.go", "sftp_js.go", "fixture_js.go"]
    .map((name) => ({ from: name.split(":")[0], to: name.split(":").at(-1) })));
  // A line of any other copy shape is named with its number, never skipped;
  // comments and the lines that only fetch or unpack the tarball are not copies.
  const base = script("1.102.3");
  const tarball = 'tarball="$root/.build/tailscale.tar.gz"\ntar -xzf "$copy" -C "$extract"\n' +
    '# cp wasm/old_js.go "$source/cmd/tsconnect/wasm/old_js.go"\n  # cp -f wasm/x.go $source/cmd/tsconnect/wasm/x.go\n' +
    'GOOS=js GOARCH=wasm go build -o "$root/wasm/tailserve.wasm" ./cmd/tsconnect/wasm\n';
  writeFileSync(join(cwd, "scripts/build-wasm.sh"), base + tarball);
  assert.equal(matrixRunner.wasmSeedSpec(cwd).copies.length, 2, "comments and tarball lines are ignored");
  for (const [line, text] of [
    [5, 'cp wasm/upload_js.go $source/cmd/tsconnect/wasm/upload_js.go'],
    [5, 'cp -f wasm/upload_js.go "$source/cmd/tsconnect/wasm/upload_js.go"'],
    [5, 'install -m 644 wasm/upload_js.go "$source/cmd/tsconnect/wasm/upload_js.go"'],
  ]) {
    writeFileSync(join(cwd, "scripts/build-wasm.sh"), base + text + "\n" + tarball);
    assert.equal(matrixRunner.wasmSeedSpec(cwd).cause, `build-wasm.sh line ${line} is a copy line the runner cannot read: ${text}`);
  }
});

// A fixture tailscale.com whose cmd/tsconnect/wasm needs example.com/jsdep
// only through the overlaid wasm/upstream_js.go, as build-wasm.sh copies it.
function addTailscaleFixture(t, proxy) {
  const jsdepMod = "module example.com/jsdep\n\ngo 1.21\n";
  const versions = (path) => {
    const dir = join(proxy.directory, path, "@v");
    mkdirSync(dir, { recursive: true });
    return dir;
  };
  const jsdep = versions("example.com/jsdep");
  writeFileSync(join(jsdep, "list"), "v1.0.0\n");
  writeFileSync(join(jsdep, "v1.0.0.info"), '{"Version":"v1.0.0","Time":"2026-01-01T00:00:00Z"}\n');
  writeFileSync(join(jsdep, "v1.0.0.mod"), jsdepMod);
  writeFileSync(join(jsdep, "v1.0.0.zip"), storedZip({
    "example.com/jsdep@v1.0.0/go.mod": jsdepMod,
    "example.com/jsdep@v1.0.0/jsdep.go": "package jsdep\n\nfunc Value() int { return 2 }\n",
  }));
  const source = tempDir(t, "verification-tailscale-source-");
  const files = {
    "go.mod": "module tailscale.com\n\ngo 1.21\n\nrequire example.com/jsdep v1.0.0\n",
    "x/x.go": "package x\n\nfunc V() int { return 1 }\n",
    "cmd/tsconnect/wasm/main.go": "package main\n\nfunc main() {}\n",
    "cmd/tsconnect/other/o.go": 'package other\n\nimport _ "example.com/jsdep"\n',
  };
  for (const [name, text] of Object.entries(files)) {
    mkdirSync(join(source, name, ".."), { recursive: true });
    writeFileSync(join(source, name), text);
  }
  execFileSync("go", ["mod", "tidy"], { cwd: source, env: fixtureGoEnvironment(t, { GOPROXY: proxy.url }), stdio: "pipe" });
  files["go.sum"] = readFileSync(join(source, "go.sum"), "utf8");
  files["go.mod"] = readFileSync(join(source, "go.mod"), "utf8");
  const ts = versions("tailscale.com");
  writeFileSync(join(ts, "list"), "v1.0.0\n");
  writeFileSync(join(ts, "v1.0.0.info"), '{"Version":"v1.0.0","Time":"2026-01-01T00:00:00Z"}\n');
  writeFileSync(join(ts, "v1.0.0.mod"), files["go.mod"]);
  writeFileSync(join(ts, "v1.0.0.zip"), storedZip(Object.fromEntries(
    Object.entries(files).map(([name, text]) => ["tailscale.com@v1.0.0/" + name, text]))));
  return files;
}
const wasmScript = (version) => `#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
root=$(pwd)
source="$root/.build/tailscale-${version}"
mkdir -p .build
if [ -d "$source" ]; then chmod -R u+w "$source"; rm -rf "$source"; fi
(cd hub && go mod download tailscale.com) || true
cp -R "$(cd hub && go list -m -f '{{.Dir}}' tailscale.com)" "$source"
chmod -R u+w "$source"
cp wasm/upstream_js.go "$source/cmd/tsconnect/wasm/wasm_js.go"
cd "$source"
echo "wasm GOMODCACHE=$(go env GOMODCACHE) GOPROXY=$(go env GOPROXY)"
GOOS=js GOARCH=wasm go build -tags=tailserve_test -o "$root/.build/test.wasm" ./cmd/tsconnect/wasm
`;

test("shared caches: wasm-test-build reads Tailscale's module set from the generation, cold then warm", { timeout: 300000 }, async (t) => {
  const proxy = moduleProxy(t);
  addTailscaleFixture(t, proxy);
  const f = sharedCacheFixture(t, {
    proxy,
    rules: [{ prefixes: ["wasm/"], groups: ["wasm"] }],
    files: {
      "hub/ts.go": 'package hub\n\nimport "tailscale.com/x"\n\nvar _ = x.V\n',
      "wasm/upstream_js.go": 'package main\n\nimport _ "example.com/jsdep"\n',
      "scripts/build-wasm.sh": wasmScript("1.0.0"),
    },
    extraRequire: "tailscale.com v1.0.0",
  });
  writeFileSync(join(f.cwd, ".gitignore"), "node_modules/\n.build/\n");
  execFileSync("git", ["commit", "-qam", "ignore .build"], { cwd: f.cwd });
  const commit = execFileSync("git", ["rev-parse", "HEAD"], { cwd: f.cwd, encoding: "utf8" }).trim();
  const plan = makePlan({ baseCommit: commit, commit, owned: ["wasm/upstream_js.go"] }, f.cwd);
  assert.deepEqual(plan.checks.map((check) => check.id), ["wasm-test-build"]);
  const root = join(cacheDirectory(t, "verification-cache-wasm-"), "root");
  shareBuildCache(root);
  const cold = await sharedRun(t, f, plan, root, proxy);
  assert.equal(cold.receipt.checks[0].exitCode, 0, logs(cold.receipt));
  assert.match(cold.lines[0], /\(seeded \d+ modules\).*, wasm-test-build: shared modules$/);
  const generation = join(realpathSync(root), "mod", generationOf(root));
  assert(existsSync(join(generation, "cache/download/example.com/jsdep/@v/v1.0.0.zip")), "the overlay's import was seeded");
  assert.match(logs(cold.receipt), new RegExp(`wasm GOMODCACHE=${generation} GOPROXY=off`));
  assert.equal(cold.report.wasm.modules, "shared");
  const warm = await sharedRun(t, f, plan, root, proxy);
  assert.equal(warm.receipt.checks[0].exitCode, 0, logs(warm.receipt));
  assert.match(warm.lines[0], /\(warm\).*, wasm-test-build: shared modules$/);
  assert.deepEqual(warm.report.downloads, { toolchains: 0, modules: 0 });

  // A script whose Tailscale version differs from hub/go.mod: that check
  // alone keeps per-home modules, with the network.
  writeFileSync(join(f.cwd, "scripts/build-wasm.sh"), wasmScript("9.9.9"));
  execFileSync("git", ["commit", "-qam", "other version"], { cwd: f.cwd });
  const other = execFileSync("git", ["rev-parse", "HEAD"], { cwd: f.cwd, encoding: "utf8" }).trim();
  const mismatch = await sharedRun(t, f, makePlan({ baseCommit: other, commit: other, owned: ["wasm/upstream_js.go"] }, f.cwd), root, proxy);
  assert.equal(mismatch.receipt.checks[0].exitCode, 0, logs(mismatch.receipt));
  assert.match(mismatch.lines[0], /, wasm-test-build: per-home modules \(build-wasm\.sh builds tailscale 9\.9\.9 but hub\/go\.mod requires 1\.0\.0\)$/);
  const home = mismatch.receipt.environment.HOME;
  assert.match(logs(mismatch.receipt), new RegExp(`wasm GOMODCACHE=${join(home, "go", "pkg", "mod")} GOPROXY=${proxy.url}`));

  // A copy line of another shape: that check keeps per-home modules and the
  // matrix line names the line.
  const unread = wasmScript("1.0.0").replace("\ncp wasm/upstream_js.go ", "\ncp -f wasm/upstream_js.go ");
  assert.notEqual(unread, wasmScript("1.0.0"));
  writeFileSync(join(f.cwd, "scripts/build-wasm.sh"), unread);
  execFileSync("git", ["commit", "-qam", "copy line of another shape"], { cwd: f.cwd });
  const shape = execFileSync("git", ["rev-parse", "HEAD"], { cwd: f.cwd, encoding: "utf8" }).trim();
  const loud = await sharedRun(t, f, makePlan({ baseCommit: shape, commit: shape, owned: ["wasm/upstream_js.go"] }, f.cwd), root, proxy);
  assert.equal(loud.receipt.checks[0].exitCode, 0, logs(loud.receipt));
  assert(loud.lines[0].endsWith(", wasm-test-build: per-home modules (build-wasm.sh line 11 is a copy line the runner cannot read: " +
    'cp -f wasm/upstream_js.go "$source/cmd/tsconnect/wasm/wasm_js.go")'), loud.lines[0]);
  assert.equal(loud.report.wasm.modules, "per-home");
});

const goRoot = () => execFileSync("go", ["env", "GOROOT"], { encoding: "utf8", env: { ...process.env, GOTOOLCHAIN: "local" } }).trim();
// A Homebrew GOROOT has no LICENSE (a downloaded toolchain does); the script
// copies whichever of the two the host has.
const goLicense = () => (existsSync(join(goRoot(), "LICENSE")) ? "LICENSE" : "PATENTS");
// The real scripts/build-wasm.sh with its Tailscale version and tarball
// sha256 replaced. Each replacement must match as often as expected, so a
// reshaped script fails here instead of testing something else.
function realWasmScript(version, sha) {
  let text = readFileSync(new URL("../scripts/build-wasm.sh", import.meta.url), "utf8");
  const swap = (from, to, count) => {
    assert.equal(text.split(from).length - 1, count, `build-wasm.sh mentions ${from} ${count} times`);
    text = text.replaceAll(from, to);
  };
  swap("1.102.3", version, 3);
  swap(/^sha256=([0-9a-f]{64})$/m.exec(text)[1], sha, 1);
  swap('"$(go env GOROOT)/LICENSE"', `"$(go env GOROOT)/${goLicense()}"`, 1);
  return text;
}
// The fixture tailscale.com as a release tarball (top directory
// tailscale-<version>/), with its sha256.
function tailscaleTarball(t, files, version = "1.0.0") {
  const scratch = tempDir(t, "verification-tailscale-tarball-");
  for (const [name, text] of Object.entries(files)) {
    mkdirSync(join(scratch, `tailscale-${version}`, name, ".."), { recursive: true });
    writeFileSync(join(scratch, `tailscale-${version}`, name), text);
  }
  const path = join(scratch, "release.tar.gz");
  execFileSync("tar", ["-czf", path, "-C", scratch, `tailscale-${version}`]);
  return { path, sha: sha256Hex(readFileSync(path)) };
}
// A curl first on PATH: logs each call and writes the file named in
// <tools>/curl-source (after <tools>/curl-delay seconds, if any) to -o.
function curlStandIn(t, source) {
  const tools = tempDir(t, "verification-curl-"), log = join(tools, "curl.log");
  writeFileSync(join(tools, "curl-source"), source);
  writeFileSync(join(tools, "curl"),
    `#!/bin/sh\nprintf '%s\\n' "$*" >> '${log}'\n[ -f '${tools}/curl-delay' ] && sleep "$(cat '${tools}/curl-delay')"\n` +
    `out=\nwhile [ $# -gt 0 ]; do [ "$1" = -o ] && out=$2; shift; done\ncp "$(cat '${tools}/curl-source')" "$out"\n`,
    { mode: 0o755 });
  return { tools, calls: () => (existsSync(log) ? readFileSync(log, "utf8").split("\n").filter(Boolean).length : 0) };
}
// The checkout files build-wasm.sh reads or rewrites: the five copied
// sources, and wasm_exec.js and LICENSE.go as the script writes them.
const wasmCheckoutFiles = (version, sha) => ({
  "wasm/upstream_js.go": 'package main\n\nimport _ "example.com/jsdep"\n',
  ...Object.fromEntries(["ssh_js.go", "upload_js.go", "sftp_js.go", "fixture_js.go"].map((name) => ["wasm/" + name, "package main\n"])),
  "wasm/wasm_exec.js": readFileSync(join(goRoot(), "lib/wasm/wasm_exec.js"), "utf8"),
  "wasm/LICENSE.go": readFileSync(join(goRoot(), goLicense()), "utf8"),
  "scripts/build-wasm.sh": realWasmScript(version, sha),
});
// One js/wasm build cache for this file's direct script runs: only the first
// compiles the js/wasm runtime.
let fileWasmCache;
function wasmBuildCache() {
  if (fileWasmCache) return fileWasmCache;
  fileWasmCache = mkdtempSync(join(tmpdir(), "verification-file-wasm-cache-"));
  process.on("exit", () => existsSync(fileWasmCache) && removeVerifierHome(fileWasmCache));
  return fileWasmCache;
}
// A plain directory checkout for running build-wasm.sh directly.
function wasmCheckout(t, version, sha) {
  const cwd = tempDir(t, "verification-wasm-checkout-");
  t.after(() => existsSync(join(cwd, ".build")) && removeVerifierHome(join(cwd, ".build")));
  for (const [name, text] of Object.entries(wasmCheckoutFiles(version, sha))) {
    mkdirSync(join(cwd, name, ".."), { recursive: true });
    writeFileSync(join(cwd, name), text);
  }
  return cwd;
}
// build-wasm.sh run directly. shared: the runner's shape (an absolute cache
// root and both Go caches); otherwise the given extras alone.
function wasmEnvironment(t, proxy, curl, extra = {}) {
  const environment = Object.fromEntries(Object.entries(process.env).filter(([key]) => !/^(GO|TAILTERM_)/.test(key)));
  return { ...environment, PATH: curl.tools + ":" + process.env.PATH, GOPROXY: proxy.url, GOSUMDB: "off",
    GOFLAGS: "-modcacherw", GOTOOLCHAIN: "local", ...extra };
}
function runWasmScript(cwd, env) {
  return new Promise((done) => {
    const child = spawn("sh", ["scripts/build-wasm.sh", "--test"], { cwd, env, stdio: ["ignore", "pipe", "pipe"] });
    let output = "";
    child.stdout.on("data", (data) => (output += data));
    child.stderr.on("data", (data) => (output += data));
    child.on("close", (code) => done({ code, output, line: /^build-wasm: .*$/m.exec(output)?.[0] || "" }));
  });
}
const temporaries = (dir) => (existsSync(dir) ? readdirSync(dir).filter((name) => name.endsWith(".tmp")) : []);

test("wasm build: under the runner it uses the shared build cache and one cached tarball, cold then from a fresh checkout", { timeout: 300000 }, async (t) => {
  const proxy = moduleProxy(t);
  const files = addTailscaleFixture(t, proxy);
  const tarball = tailscaleTarball(t, files);
  const curl = curlStandIn(t, tarball.path);
  const root = join(cacheDirectory(t, "verification-cache-wasm-tarball-"), "root");
  shareBuildCache(root);
  const checkout = () => {
    const f = sharedCacheFixture(t, {
      proxy,
      rules: [{ prefixes: ["wasm/"], groups: ["wasm"] }],
      files: { ...wasmCheckoutFiles("1.0.0", tarball.sha), ".gitignore": "node_modules/\n.build/\nwasm/*.wasm\n",
        "hub/ts.go": 'package hub\n\nimport "tailscale.com/x"\n\nvar _ = x.V\n' },
      extraRequire: "tailscale.com v1.0.0",
    });
    t.after(() => existsSync(join(f.cwd, ".build")) && removeVerifierHome(join(f.cwd, ".build")));
    const plan = f.plan(["wasm/upstream_js.go"]);
    assert.deepEqual(plan.checks.map((check) => check.id), ["wasm-test-build"]);
    return { f, plan };
  };
  const savedPath = process.env.PATH;
  process.env.PATH = curl.tools + ":" + savedPath;
  t.after(() => (process.env.PATH = savedPath));
  const runs = [];
  for (const pass of ["cold", "fresh checkout"]) {
    const { f, plan } = checkout();
    const started = Date.now();
    const run = await sharedRun(t, f, plan, root, proxy);
    run.seconds = (Date.now() - started) / 1000;
    runs.push(run);
    assert.equal(run.receipt.checks[0].exitCode, 0, logs(run.receipt));
    assert.match(run.lines[0], /, wasm-test-build: shared modules$/, pass);
    const env = run.receipt.environment;
    const line = /^build-wasm: .*$/m.exec(logs(run.receipt))?.[0];
    const expected = join(realpathSync(root), "downloads", tarball.sha + ".tar.gz");
    assert.equal(line, `build-wasm: GOCACHE=${env.GOCACHE} GOMODCACHE=${env.GOMODCACHE} tarball=${expected} ` +
      `(${pass === "cold" ? "downloaded" : "cached"})`, pass);
    assert(insidePath(env.GOCACHE, join(realpathSync(root), "build")), env.GOCACHE);
    for (const local of [".build/cache", ".build/go"]) assert(!existsSync(join(f.cwd, local)), `${pass}: ${local}`);
    assert.equal(sha256Hex(readFileSync(expected)), tarball.sha);
    assert.deepEqual(readdirSync(join(root, "downloads")), [tarball.sha + ".tar.gz"]);
    assert.equal(curl.calls(), 1, `${pass}: one download in all`);
  }
  assert.equal(runs[0].report.wasm.copy, "made");
  assert.equal(runs[1].report.wasm.copy, "verified");
  console.log(`wasm tarball runs: cold ${runs[0].seconds} s, fresh checkout ${runs[1].seconds} s`);
});

test("wasm build: a plain local build keeps the checkout's caches and tarball and never makes a cache root", { timeout: 300000 }, async (t) => {
  const proxy = moduleProxy(t);
  const tarball = tailscaleTarball(t, addTailscaleFixture(t, proxy));
  const curl = curlStandIn(t, tarball.path);
  const unused = join(cacheDirectory(t, "verification-cache-wasm-plain-"), "root");
  for (const extra of [{}, { TAILTERM_VERIFICATION_CACHE: unused }, { TAILTERM_VERIFICATION_CACHE: unused, GOCACHE: wasmBuildCache() }]) {
    const cwd = wasmCheckout(t, "1.0.0", tarball.sha);
    // The checkout's own build cache, seeded so js/wasm is not compiled again.
    if (existsSync(join(wasmBuildCache(), "README"))) {
      mkdirSync(join(cwd, ".build"));
      execFileSync("cp", [process.platform === "darwin" ? "-cR" : "-R", wasmBuildCache(), join(cwd, ".build/cache")]);
    }
    const result = await runWasmScript(cwd, wasmEnvironment(t, proxy, curl, extra));
    assert.equal(result.code, 0, result.output);
    const real = realpathSync(cwd);
    assert.equal(result.line, `build-wasm: GOCACHE=${real}/.build/cache GOMODCACHE=${real}/.build/go/pkg/mod ` +
      `tarball=${real}/.build/tailscale.tar.gz (downloaded)`, JSON.stringify(extra));
    assert.equal(sha256Hex(readFileSync(join(cwd, ".build/tailscale.tar.gz"))), tarball.sha);
    assert(existsSync(join(cwd, ".build/test.wasm")));
    assert.deepEqual(temporaries(join(cwd, ".build")), []);
    assert(!existsSync(unused), "the cache root was not created");
    if (!existsSync(join(wasmBuildCache(), "README")))
      execFileSync("cp", ["-R", join(cwd, ".build/cache") + "/.", wasmBuildCache()]);
  }
});

test("wasm build: a wrong or partial cached tarball is downloaded again, and a bad download or extraction leaves nothing", { timeout: 300000 }, async (t) => {
  const proxy = moduleProxy(t);
  const tarball = tailscaleTarball(t, addTailscaleFixture(t, proxy));
  const curl = curlStandIn(t, tarball.path);
  const root = cacheDirectory(t, "verification-cache-wasm-bad-");
  const cached = join(root, "downloads", tarball.sha + ".tar.gz");
  const shared = { TAILTERM_VERIFICATION_CACHE: root, GOCACHE: wasmBuildCache(), GOMODCACHE: join(root, "mod-direct") };
  const good = readFileSync(tarball.path);
  for (const [shape, bytes] of [["wrong bytes", Buffer.from("not the release")], ["partial", good.subarray(0, good.length >> 1)]]) {
    mkdirSync(join(root, "downloads"), { recursive: true });
    writeFileSync(cached, bytes);
    const before = curl.calls();
    const cwd = wasmCheckout(t, "1.0.0", tarball.sha);
    const result = await runWasmScript(cwd, wasmEnvironment(t, proxy, curl, shared));
    assert.equal(result.code, 0, result.output);
    assert.match(result.line, / \(downloaded\)$/, shape);
    assert.equal(curl.calls(), before + 1, shape);
    assert.equal(sha256Hex(readFileSync(cached)), tarball.sha, shape);
    assert.deepEqual(readdirSync(join(root, "downloads")), [tarball.sha + ".tar.gz"], shape);
    assert.deepEqual(temporaries(join(cwd, ".build")), [], shape);
  }

  // A download that fails the check: non-zero, nothing kept anywhere.
  rmSync(join(root, "downloads"), { recursive: true });
  const wrong = join(tempDir(t, "verification-wrong-release-"), "wrong.tar.gz");
  writeFileSync(wrong, "not the release");
  writeFileSync(join(curl.tools, "curl-source"), wrong);
  let cwd = wasmCheckout(t, "1.0.0", tarball.sha);
  let result = await runWasmScript(cwd, wasmEnvironment(t, proxy, curl, shared));
  assert.notEqual(result.code, 0, result.output);
  assert.match(result.output, /build-wasm: the downloaded tarball failed its sha256 check/);
  assert.deepEqual(readdirSync(join(root, "downloads")), []);
  assert(!existsSync(join(cwd, ".build/tailscale-1.0.0")));
  assert.deepEqual(temporaries(join(cwd, ".build")), []);

  // A tar that fails part way through leaves no source directory behind.
  writeFileSync(join(curl.tools, "curl-source"), tarball.path);
  writeFileSync(join(curl.tools, "tar"),
    `#!/bin/sh\ndir=\nwhile [ $# -gt 0 ]; do [ "$1" = -C ] && dir=$2; shift; done\n` +
    `mkdir -p "$dir/tailscale-1.0.0/cmd" && echo partial > "$dir/tailscale-1.0.0/go.mod"\necho 'tar: truncated' >&2\nexit 1\n`,
    { mode: 0o755 });
  cwd = wasmCheckout(t, "1.0.0", tarball.sha);
  result = await runWasmScript(cwd, wasmEnvironment(t, proxy, curl, shared));
  assert.notEqual(result.code, 0, result.output);
  assert.match(result.output, /tar: truncated/);
  assert(!existsSync(join(cwd, ".build/tailscale-1.0.0")));
  assert.deepEqual(temporaries(join(cwd, ".build")), []);
  assert.equal(sha256Hex(readFileSync(cached)), tarball.sha, "the verified download is kept");
});

test("wasm build: two runs at once from two checkouts share one verified tarball", { timeout: 300000 }, async (t) => {
  const proxy = moduleProxy(t);
  const tarball = tailscaleTarball(t, addTailscaleFixture(t, proxy));
  const curl = curlStandIn(t, tarball.path);
  writeFileSync(join(curl.tools, "curl-delay"), "1");
  const root = cacheDirectory(t, "verification-cache-wasm-two-");
  const shared = { TAILTERM_VERIFICATION_CACHE: root, GOCACHE: wasmBuildCache(), GOMODCACHE: join(root, "mod-direct") };
  const checkouts = [wasmCheckout(t, "1.0.0", tarball.sha), wasmCheckout(t, "1.0.0", tarball.sha)];
  const results = await Promise.all(checkouts.map((cwd) => runWasmScript(cwd, wasmEnvironment(t, proxy, curl, shared))));
  for (const [index, result] of results.entries()) {
    assert.equal(result.code, 0, result.output);
    assert(existsSync(join(checkouts[index], ".build/test.wasm")));
    assert.deepEqual(temporaries(join(checkouts[index], ".build")), []);
  }
  assert.deepEqual(readdirSync(join(root, "downloads")), [tarball.sha + ".tar.gz"]);
  assert.equal(sha256Hex(readFileSync(join(root, "downloads", tarball.sha + ".tar.gz"))), tarball.sha);
});

test("shared caches: a wasm source copy that differs from the generation is replaced before any go command runs in it", { timeout: 300000 }, async (t) => {
  const proxy = moduleProxy(t);
  addTailscaleFixture(t, proxy);
  const f = sharedCacheFixture(t, {
    proxy,
    rules: [{ prefixes: ["wasm/"], groups: ["wasm"] }],
    files: {
      "hub/ts.go": 'package hub\n\nimport "tailscale.com/x"\n\nvar _ = x.V\n',
      "wasm/upstream_js.go": 'package main\n\nimport _ "example.com/jsdep"\n',
      "scripts/build-wasm.sh": wasmScript("1.0.0"),
      ".gitignore": "node_modules/\n.build/\n",
    },
    extraRequire: "tailscale.com v1.0.0",
  });
  t.after(() => existsSync(join(f.cwd, ".build")) && removeVerifierHome(join(f.cwd, ".build")));
  const plan = f.plan(["wasm/upstream_js.go"]);
  const root = join(cacheDirectory(t, "verification-cache-wasm-copy-"), "root");
  shareBuildCache(root);
  // Every go command whose directory is the copy finds it equal to the
  // generation's module directory.
  let checked = 0;
  const go = (args, options) => {
    const generation = /^(.*)\.wasm-src\//.exec(options.cwd)?.[1];
    if (generation) {
      checked++;
      assert.deepEqual(treeSnapshot(options.cwd), treeSnapshot(join(generation, "tailscale.com@v1.0.0")), args.join(" "));
    }
    return matrixRunner.runGo(args, options);
  };
  const run = (extra = {}) => sharedRun(t, f, plan, root, proxy, { caches: { go }, ...extra });
  const cold = await run();
  assert.equal(cold.receipt.checks[0].exitCode, 0, logs(cold.receipt));
  assert.equal(cold.report.wasm.copy, "made");
  const generation = join(realpathSync(root), "mod", generationOf(root));
  const copy = join(`${generation}.wasm-src`, "tailscale.com@v1.0.0");
  const module = join(generation, "tailscale.com@v1.0.0");
  const warm = await run();
  assert.equal(warm.report.wasm.copy, "verified");
  const plant = (shape) => {
    chmodSync(copy, 0o755);
    if (shape === "module line") {
      chmodSync(join(copy, "go.mod"), 0o644);
      writeFileSync(join(copy, "go.mod"), readFileSync(join(copy, "go.mod"), "utf8").replace("module tailscale.com", "module example.com/other"));
    } else writeFileSync(join(copy, "planted.go"), "package planted\n");
  };
  for (const [shape, cause] of [["module line", "go.mod names module example.com/other"], ["extra file", "its tree differs from tailscale.com@v1.0.0"]]) {
    plant(shape);
    const before = checked;
    const replaced = await run();
    assert.equal(replaced.receipt.checks[0].exitCode, 0, logs(replaced.receipt));
    assert.match(replaced.lines[0], /, wasm-test-build: shared modules$/, shape);
    assert.equal(replaced.report.wasm.copy, "replaced", shape);
    assert.equal(replaced.report.wasm.copyCause, cause, shape);
    assert.deepEqual(treeSnapshot(copy), treeSnapshot(module), shape);
    assert(checked > before, shape);
  }
  // Two runs at once on a planted copy both pass and leave one good copy.
  plant("extra file");
  const two = { environment: { TAILTERM_MATRIX_MAX_HOLDERS: "2", TAILTERM_MATRIX_MEMORY_PRESSURE_MAX: "off" }, maxWaitMs: 60000 };
  const both = await Promise.all([run({ hostLock: two }), run({ hostLock: two })]);
  for (const result of both) {
    assert.equal(result.receipt.checks[0].exitCode, 0, logs(result.receipt));
    assert.equal(result.report.mode, "shared");
  }
  assert.deepEqual(treeSnapshot(copy), treeSnapshot(module));
  assert.deepEqual(readdirSync(`${generation}.wasm-src`), ["tailscale.com@v1.0.0"]);
  assert.equal((await run()).report.wasm.copy, "verified");
});
