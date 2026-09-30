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
} from "node:fs";
import { tmpdir, getPriority } from "node:os";
import { join } from "node:path";
import { execFileSync, spawn, spawnSync } from "node:child_process";
import { prepareTestBinary, sourceIdentity, fileHash } from "./test-binaries.mjs";
import { createServer } from "node:net";
import { once } from "node:events";
import {
  selectChecks,
  diffPaths,
  makePlan as rawMakePlan,
  runPlan,
  digest,
  assertInventory,
  runCheck,
  occupiedPorts,
  receiptEligible,
  matrixPolicy,
  removeVerifierHome,
  runScheduled,
  executionLocks,
  overlapViolations,
  defaultJobs,
  SERIAL_SUITES,
  NON_GO_NICE,
  makeTargetedPlan,
  runTargeted,
} from "../scripts/verify-matrix.mjs";
const makePlan = (context, cwd) =>
  rawMakePlan(
    {
      approvedMatrixDigest: digest(
        readFileSync(join(cwd, "verification/matrix.json"), "utf8"),
      ),
      matrixApprovalMessageSeq: 1,
      ...context,
    },
    cwd,
  );
const matrix = JSON.parse(
  readFileSync(new URL("../verification/matrix.json", import.meta.url)),
);
test("prepared binary reuse rejects source changes, tampering and a different checkout", async () => {
  const root = mkdtempSync(join(tmpdir(), "test-binary-source-"));
  mkdirSync(join(root, "hub"));
  writeFileSync(join(root, "hub", "go.mod"), "module fixture\n");
  const binary = join(root, "hub-test");
  writeFileSync(binary, "synthetic executable");
  chmodSync(binary, 0o755);
  const manifest = join(root, "manifest.json");
  const source = await sourceIdentity(root);
  writeFileSync(manifest, JSON.stringify({ binaries: [{ target: "hub", historicalCommit: null,
    source, path: binary, sha256: await fileHash(binary) }] }));
  const options = { root, target: "hub", manifestPath: manifest };
  assert.equal(await prepareTestBinary(options), binary);
  writeFileSync(binary, "tampered executable");
  await assert.rejects(prepareTestBinary(options), /hash mismatch/);
  writeFileSync(binary, "synthetic executable");
  writeFileSync(join(root, "hub", "go.mod"), "module changed\n");
  await assert.rejects(prepareTestBinary(options), /Source mismatch/);
  const other = mkdtempSync(join(tmpdir(), "test-binary-other-"));
  mkdirSync(join(other, "hub"));
  writeFileSync(join(other, "hub", "go.mod"), "module fixture\n");
  await assert.rejects(prepareTestBinary({ ...options, root: other }), /No prepared/);
});
test("profile startup failure emits child exit, stderr and phase diagnostics", async () => {
  const root = new URL("..", import.meta.url).pathname;
  const dir = mkdtempSync(join(tmpdir(), "profile-startup-failure-"));
  const binary = join(dir, "hub");
  writeFileSync(binary, "#!/bin/sh\necho synthetic-startup-boom >&2\nexit 7\n");
  chmodSync(binary, 0o755);
  const manifest = join(dir, "manifest.json");
  writeFileSync(manifest, JSON.stringify({ binaries: [{ target: "hub", historicalCommit: null,
    source: await sourceIdentity(root), path: binary, sha256: await fileHash(binary) }] }));
  const result = spawnSync(process.execPath, ["tests/profile-sync-browser.mjs"], {
    cwd: root, encoding: "utf8", timeout: 10000,
    env: { PATH: process.env.PATH, HOME: process.env.HOME, TMPDIR: process.env.TMPDIR,
      TAILTERM_TEST_BINARIES: manifest },
  });
  assert.equal(result.status, 1);
  assert.match(result.stderr, /profile-sync failure diagnostics:/);
  assert.match(result.stderr, /"phase":"startup"/);
  assert.match(result.stderr, /"ready":false/);
  assert.match(result.stderr, /"code":7/);
  assert.match(result.stderr, /synthetic-startup-boom/);
});
test("ownership union diff selects all engines, migration and touched race packages", () => {
  const checks = selectChecks(
    matrix,
    ["client/tasks.js"],
    ["hub/internal/store/migrate.go"],
  );
  assert(checks.some((c) => c.id === "npm-unit"));
  assert(checks.some((c) => c.id === "go-vet"));
  assert(checks.some((c) => c.id === "migration-rehearsal"));
  assert.deepEqual(checks.find((c) => c.id === "go-race").argv, [
    "go",
    "test",
    "-race",
    "-timeout=25m",
    "./internal/store",
  ]);
  for (const suite of matrix.browserSuites)
    for (const engine of ["chromium", "webkit"])
      assert(
        checks.some(
          (c) =>
            c.argv.includes(suite.file) &&
            (c.environment.TEST_BROWSER === engine ||
              c.environment.TEST_BROWSER === "both"),
        ),
      );
  assert.deepEqual(
    checks,
    selectChecks(
      matrix,
      ["hub/internal/store/migrate.go"],
      ["client/tasks.js", "client/tasks.js"],
    ),
  );
});
test("unknown and traversal paths fail closed", () => {
  for (const p of ["unknown/thing", "../client/x", "/client/x", "client/../x"])
    assert.throws(() => selectChecks(matrix, [p], []));
});
test("browser inventory includes every standalone entry and only explicit deployed exclusions", () =>
  assertInventory(matrix, new URL("..", import.meta.url).pathname));
function fixture(t) {
  const cwd = tempDir(t, "verification-fixture-");
  mkdirSync(join(cwd, "tests"));
  mkdirSync(join(cwd, "verification"));
  const git = (...args) =>
    execFileSync("git", args, { cwd, encoding: "utf8" }).trim();
  git("init", "-q");
  git("config", "user.name", "Fixture");
  git("config", "user.email", "fixture@example.invalid");
  const m = {
    maxAttempts: 3,
    knownFailures: [],
    version: 1,
    browserSuites: [],
    excludedBrowserSuites: [],
    rules: [{ prefixes: ["docs/"], groups: ["unit"] }],
  };
  writeFileSync(join(cwd, "verification/matrix.json"), JSON.stringify(m));
  mkdirSync(join(cwd, "docs"));
  writeFileSync(join(cwd, "docs/old.md"), "hello\n");
  writeFileSync(
    join(cwd, "package.json"),
    JSON.stringify({ scripts: { test: 'node -e "process.exit(0)"' } }),
  );
  git("add", ".");
  git("commit", "-qm", "fixture");
  const base = git("rev-parse", "HEAD");
  git("mv", "docs/old.md", "docs/new.md");
  git("commit", "-qm", "rename");
  const commit = git("rev-parse", "HEAD");
  git("checkout", "--detach", commit);
  return { cwd, git, base, commit };
}
function tempDir(t, prefix) {
  const directory = mkdtempSync(join(tmpdir(), prefix));
  t.after(() => rmSync(directory, { recursive: true, force: true }));
  return directory;
}
function commandPlan(f, source) {
  writeFileSync(join(f.cwd, "check.mjs"), source);
  writeFileSync(
    join(f.cwd, "package.json"),
    JSON.stringify({ scripts: { test: "node check.mjs" } }),
  );
  f.git("add", ".");
  f.git("commit", "-qm", "fixture command");
  const commit = f.git("rev-parse", "HEAD");
  return makePlan({ baseCommit: commit, commit, owned: ["docs/"] }, f.cwd);
}
test("run removes locked Go cache after success and leaves symlink target untouched", async (t) => {
  const f = fixture(t),
    outside = tempDir(t, "verification-sibling-");
  writeFileSync(join(outside, "keep"), "sibling");
  const plan = commandPlan(
    f,
    `import fs from 'node:fs';import path from 'node:path';const home=process.env.HOME;const cache=path.join(process.env.GOMODCACHE,'locked');fs.mkdirSync(cache,{recursive:true});fs.writeFileSync(path.join(cache,'module'), 'module');fs.chmodSync(path.join(cache,'module'),0o400);fs.chmodSync(cache,0o500);fs.symlinkSync(${JSON.stringify(outside)},path.join(home,'sibling'));console.log(JSON.stringify({HOME:home,GOPATH:process.env.GOPATH,GOMODCACHE:process.env.GOMODCACHE,GOCACHE:process.env.GOCACHE}));`,
  );
  const output = tempDir(t, "verification-clean-logs-");
  const receipt = await runPlan(plan, f.cwd, output);
  assert.equal(receipt.checks[0].exitCode, 0);
  assert.equal(receipt.environment.VERIFICATION_KEEP_HOME, "0");
  assert(!existsSync(receipt.environment.HOME));
  assert.equal(readFileSync(join(outside, "keep"), "utf8"), "sibling");
  assert(existsSync(receipt.checks[0].logURI));
  assert(existsSync(join(output, "receipt.json")));
  const childEnvironment = JSON.parse(
    readFileSync(receipt.checks[0].logURI, "utf8")
      .split("\n")
      .find((line) => line.startsWith('{"HOME":')),
  );
  for (const key of ["HOME", "GOPATH", "GOMODCACHE", "GOCACHE"])
    assert.equal(childEnvironment[key], receipt.environment[key]);
  for (const key of ["GOPATH", "GOMODCACHE", "GOCACHE"])
    assert(receipt.environment[key].startsWith(receipt.environment.HOME + "/"));
});
test("failed run removes home after saving failed logs", async (t) => {
  const f = fixture(t),
    plan = commandPlan(f, "console.error('fixture failure');process.exit(7);");
  const receipt = await runPlan(
    plan,
    f.cwd,
    tempDir(t, "verification-failed-logs-"),
  );
  assert.equal(receipt.checks[0].exitCode, 7);
  assert(!existsSync(receipt.environment.HOME));
  assert.match(
    readFileSync(receipt.checks[0].logURI, "utf8"),
    /fixture failure/,
  );
});
test("keep-home retains exact home and records opt-in with cache isolation", async (t) => {
  const f = fixture(t),
    plan = commandPlan(f, "console.log(process.env.HOME);");
  const output = tempDir(t, "verification-kept-logs-");
  const receipt = await runPlan(plan, f.cwd, output, { keepHome: true });
  t.after(() => removeVerifierHome(receipt.environment.HOME));
  assert.equal(receipt.environment.VERIFICATION_KEEP_HOME, "1");
  assert(existsSync(receipt.environment.HOME));
  assert.equal(
    JSON.parse(readFileSync(join(output, "receipt.json"))).environment.HOME,
    receipt.environment.HOME,
  );
  for (const key of ["GOPATH", "GOMODCACHE", "GOCACHE"])
    assert(receipt.environment[key].startsWith(receipt.environment.HOME + "/"));
});
test("free-space reserve rejects before run and before retry without filling disk", async (t) => {
  const f = fixture(t),
    output = tempDir(t, "verification-space-logs-");
  const counter = join(output, "count");
  const plan = commandPlan(
    f,
    `import fs from 'node:fs';fs.appendFileSync(${JSON.stringify(counter)},'x');process.exit(9);`,
  );
  await assert.rejects(
    () => runPlan(plan, f.cwd, output, { minFreeBytes: -1 }),
    /Invalid --min-free-bytes/,
  );
  await assert.rejects(
    () =>
      runPlan(plan, f.cwd, output, {
        minFreeBytes: 10,
        getAvailableBytes: () => 9,
      }),
    /Insufficient free space/,
  );
  assert(!existsSync(counter), "low-space refusal starts no command");
  let probes = 0;
  await assert.rejects(
    () =>
      runPlan(plan, f.cwd, output, {
        minFreeBytes: 10,
        getAvailableBytes: () => (++probes < 3 ? 10 : 9),
      }),
    /Insufficient free space/,
  );
  assert.equal(
    readFileSync(counter, "utf8"),
    "x",
    "only the first attempt ran",
  );
  assert.equal(probes, 3);
  const receipt = await runPlan(plan, f.cwd, output, {
    minFreeBytes: 0,
    getAvailableBytes: () => 10,
  });
  assert.equal(receipt.checks[0].exitCode, 9);
  assert.equal(readFileSync(counter, "utf8"), "xxxx");
});
test("missing prerequisites and output errors clean their allocated homes", async (t) => {
  const f = fixture(t),
    root = tempDir(t, "verification-home-root-");
  const prerequisiteOutput = tempDir(t, "verification-prereq-logs-");
  const outputFile = join(tempDir(t, "verification-output-parent-"), "file");
  writeFileSync(outputFile, "not a directory");
  const original = process.env.TMPDIR;
  process.env.TMPDIR = root;
  t.after(() => {
    if (original === undefined) delete process.env.TMPDIR;
    else process.env.TMPDIR = original;
  });
  const browser = JSON.parse(
    readFileSync(join(f.cwd, "verification/matrix.json")),
  );
  browser.browserSuites = [{ file: "tests/fixture.mjs", mode: "selected" }];
  browser.rules = [{ prefixes: ["docs/"], groups: ["browser"] }];
  writeFileSync(join(f.cwd, "tests/fixture.mjs"), "// browser.launch(");
  writeFileSync(
    join(f.cwd, "verification/matrix.json"),
    JSON.stringify(browser),
  );
  f.git("add", ".");
  f.git("commit", "-qm", "browser fixture");
  const commit = f.git("rev-parse", "HEAD");
  const plan = makePlan(
    { baseCommit: commit, commit, owned: ["docs/"] },
    f.cwd,
  );
  await assert.rejects(
    () => runPlan(plan, f.cwd, prerequisiteOutput),
    /Missing prerequisite/,
  );
  assert.deepEqual(readdirSync(root), []);
  await assert.rejects(() => runPlan(plan, f.cwd, outputFile), /EEXIST/);
  assert.deepEqual(readdirSync(root), []);
});
test("CLI flags retain home only when requested", (t) => {
  const f = fixture(t),
    plan = commandPlan(f, "console.log(process.env.HOME);");
  const root = tempDir(t, "verification-cli-root-");
  const planFile = join(root, "plan.json");
  writeFileSync(planFile, JSON.stringify(plan));
  const script = new URL("../scripts/verify-matrix.mjs", import.meta.url)
    .pathname;
  const output = tempDir(t, "verification-cli-logs-");
  execFileSync(
    process.execPath,
    [script, "run", planFile, output, "--keep-home", "--min-free-bytes", "0"],
    { cwd: f.cwd, env: { ...process.env, TMPDIR: root } },
  );
  const receipt = JSON.parse(readFileSync(join(output, "receipt.json")));
  assert.equal(receipt.environment.VERIFICATION_KEEP_HOME, "1");
  assert(existsSync(receipt.environment.HOME));
  t.after(() => {
    if (existsSync(receipt.environment.HOME))
      removeVerifierHome(receipt.environment.HOME);
  });
  for (const value of ["-1", "", null]) {
    const args = [script, "run", planFile, output, "--min-free-bytes"];
    if (value !== null) args.push(value);
    assert.throws(
      () =>
        execFileSync(process.execPath, args, {
          cwd: f.cwd,
          env: { ...process.env, TMPDIR: root },
          stdio: "pipe",
        }),
      /--min-free-bytes requires/,
    );
  }
});
for (const signal of ["SIGINT", "SIGTERM"])
  test(`CLI ${signal} stops after the interrupted first attempt`, async (t) => {
    const f = fixture(t),
      root = tempDir(t, "verification-signal-root-");
    const marker = join(root, "started");
    const plan = commandPlan(
      f,
      `import fs from 'node:fs';fs.appendFileSync(${JSON.stringify(marker)},'s');setTimeout(()=>process.exit(${signal === "SIGINT" ? 0 : 1}),2000);`,
    );
    const planFile = join(root, "plan.json"),
      output = tempDir(t, "verification-signal-logs-");
    writeFileSync(planFile, JSON.stringify(plan));
    const script = new URL("../scripts/verify-matrix.mjs", import.meta.url)
      .pathname;
    const child = spawn(
      process.execPath,
      [script, "run", planFile, output, "--min-free-bytes", "0"],
      { cwd: f.cwd, env: { ...process.env, TMPDIR: root }, stdio: "ignore" },
    );
    t.after(() => {
      if (child.exitCode === null && child.signalCode === null)
        child.kill("SIGKILL");
    });
    for (let i = 0; !existsSync(marker) && i < 200; i++)
      await new Promise((resolve) => setTimeout(resolve, 20));
    assert(existsSync(marker), "check command started before interruption");
    child.kill(signal);
    const [code, closedBy] = await once(child, "close");
    assert.equal(
      code,
      signal === "SIGINT" ? 130 : 143,
      `closed by ${closedBy}`,
    );
    assert.deepEqual(readdirSync(root).sort(), ["plan.json", "started"]);
    assert.equal(readFileSync(marker, "utf8"), "s", "no retry started");
    assert(
      !existsSync(join(output, "receipt.json")),
      "no eligible receipt survives interruption",
    );
    assert(existsSync(join(output, digest("npm-unit") + ".attempt-1.log")));
  });
test("foreground group SIGINT stops detached check descendants before cleanup", async (t) => {
  const f = fixture(t),
    root = tempDir(t, "verification-group-root-");
  const info = join(root, "descendant.json");
  const descendant = `const fs=require('node:fs'),net=require('node:net'),path=require('node:path');process.on('SIGTERM',()=>{});const server=net.createServer();server.listen(0,'127.0.0.1',()=>{fs.writeFileSync(${JSON.stringify(info)},JSON.stringify({pid:process.pid,port:server.address().port,home:process.env.HOME}));console.log('listener ready')});setInterval(()=>fs.writeFileSync(path.join(process.env.HOME,'w'),'w'),100);`;
  const plan = commandPlan(
    f,
    `import {spawn} from 'node:child_process';spawn(process.execPath,['-e',${JSON.stringify(descendant)}],{stdio:'inherit'});setInterval(()=>{},1000);`,
  );
  const planFile = join(root, "plan.json"),
    output = tempDir(t, "verification-group-logs-");
  writeFileSync(planFile, JSON.stringify(plan));
  const script = new URL("../scripts/verify-matrix.mjs", import.meta.url)
    .pathname;
  const runner = spawn(
    process.execPath,
    [script, "run", planFile, output, "--min-free-bytes", "0"],
    {
      cwd: f.cwd,
      env: { ...process.env, TMPDIR: root },
      detached: true,
      stdio: "ignore",
    },
  );
  let descendantInfo;
  try {
    for (let i = 0; !existsSync(info) && i < 250; i++)
      await new Promise((resolve) => setTimeout(resolve, 20));
    assert(existsSync(info), "descendant listener started");
    descendantInfo = JSON.parse(readFileSync(info));
    process.kill(-runner.pid, "SIGINT");
    const [code] = await once(runner, "close");
    assert.equal(code, 130);
    assert.deepEqual(
      occupiedPorts({
        environment: {
          VERIFICATION_REQUIRED_PORTS: String(descendantInfo.port),
        },
      }),
      [],
    );
    assert(!existsSync(descendantInfo.home));
    await new Promise((resolve) => setTimeout(resolve, 350));
    assert(!existsSync(descendantInfo.home), "orphan did not recreate home");
    assert(!existsSync(join(output, "receipt.json")));
    assert(existsSync(join(output, digest("npm-unit") + ".attempt-1.log")));
  } finally {
    if (runner.exitCode === null && runner.signalCode === null)
      runner.kill("SIGKILL");
    if (descendantInfo) {
      try {
        process.kill(descendantInfo.pid, "SIGKILL");
      } catch {}
    }
  }
});
test("cleanup failure removes eligible receipt and reports retained home", async (t) => {
  const f = fixture(t),
    plan = commandPlan(f, "console.log('check passed');");
  const output = tempDir(t, "verification-cleanup-failure-logs-");
  let retained = "";
  await assert.rejects(
    () =>
      runPlan(plan, f.cwd, output, {
        removeHome: (home) => {
          retained = home;
          throw new Error("EPERM synthetic cleanup failure");
        },
      }),
    /Failed to remove verifier home .*EPERM synthetic cleanup failure/,
  );
  t.after(() => {
    if (retained && existsSync(retained)) removeVerifierHome(retained);
  });
  assert(existsSync(retained));
  assert(!existsSync(join(output, "receipt.json")));
  assert.equal(
    JSON.parse(readFileSync(join(output, "cleanup-error.json"))).home,
    retained,
  );
  assert(existsSync(join(output, digest("npm-unit") + ".attempt-1.log")));
});
test("immutable cache entry cannot leave a passing receipt", async (t) => {
  if (process.platform !== "darwin")
    return t.skip("chflags uchg requires macOS");
  const f = fixture(t);
  const output = tempDir(t, "verification-immutable-logs-");
  const homeFile = join(output, "home-path");
  const plan = commandPlan(
    f,
    `import fs from 'node:fs';import {execFileSync} from 'node:child_process';fs.writeFileSync(${JSON.stringify(homeFile)},process.env.HOME);const locked=process.env.HOME+'/locked';fs.writeFileSync(locked,'locked');execFileSync('chflags',['uchg',locked]);`,
  );
  let home = "";
  try {
    await assert.rejects(
      () => runPlan(plan, f.cwd, output),
      /Failed to remove verifier home .*EPERM/,
    );
    home = JSON.parse(readFileSync(join(output, "cleanup-error.json"))).home;
    assert(existsSync(join(home, "locked")));
    assert(!existsSync(join(output, "receipt.json")));
  } finally {
    home ||= existsSync(homeFile) ? readFileSync(homeFile, "utf8") : "";
    if (home && existsSync(home)) {
      if (existsSync(join(home, "locked")))
        execFileSync("chflags", ["nouchg", join(home, "locked")]);
      removeVerifierHome(home);
    }
  }
});
test("rename and delete paths preserved in source selection", (t) => {
  const f = fixture(t);
  assert.deepEqual(diffPaths(f.cwd, f.base, f.commit), [
    "docs/old.md",
    "docs/new.md",
  ]);
  f.git("rm", "docs/new.md");
  f.git("commit", "-qm", "delete");
  assert.deepEqual(diffPaths(f.cwd, f.commit, f.git("rev-parse", "HEAD")), [
    "docs/new.md",
  ]);
});
test("clean detached run emits complete command evidence and unsubmitted AIV bindings", async (t) => {
  const f = fixture(t),
    plan = makePlan(
      {
        operationKey: "fixture",
        repository: "fixture",
        baseCommit: f.base,
        commit: f.commit,
        owned: ["docs/"],
        verifierAgentId: "fixture",
        verifierRunId: "fixture",
      },
      f.cwd,
    );
  const out = tempDir(t, "verification-logs-");
  const receipt = await runPlan(plan, f.cwd, out);
  assert.equal(receipt.checks[0].exitCode, 0);
  assert.equal(receipt.planDigest, digest(plan));
  assert.equal(receipt.aiv.state, "unsubmitted");
  assert.match(receipt.checks[0].logDigest, /^[a-f0-9]{64}$/);
  await assert.rejects(
    () => runPlan({ ...plan, checks: [] }, f.cwd, out),
    /omitted/,
  );
  writeFileSync(join(f.cwd, "docs/untracked.md"), "dirty");
  await assert.rejects(() => runPlan(plan, f.cwd, out), /Clean detached/);
});
test("attached or wrong SHA cannot execute checks", async (t) => {
  const f = fixture(t),
    plan = makePlan(
      { baseCommit: f.base, commit: f.commit, owned: ["docs/"] },
      f.cwd,
    ),
    out = tempDir(t, "verification-out-");
  f.git("checkout", "-b", "attached");
  await assert.rejects(() => runPlan(plan, f.cwd, out), /detached/);
  await assert.rejects(
    () => runPlan({ ...plan, commit: f.base }, f.cwd, out),
    /Wrong candidate/,
  );
});

test("browser run refuses missing prerequisite assets before commands execute", async (t) => {
  const f = fixture(t);
  writeFileSync(join(f.cwd, "tests/fixture.mjs"), "// browser.launch(");
  writeFileSync(
    join(f.cwd, "verification/matrix.json"),
    JSON.stringify({
      version: 1,
      browserSuites: [{ file: "tests/fixture.mjs", mode: "selected" }],
      excludedBrowserSuites: [],
      rules: [{ prefixes: ["docs/"], groups: ["browser"] }],
    }),
  );
  f.git("add", ".");
  f.git("commit", "-qm", "browser fixture");
  const commit = f.git("rev-parse", "HEAD");
  const plan = makePlan(
    { baseCommit: commit, commit, owned: ["docs/"] },
    f.cwd,
  );
  const output = tempDir(t, "verification-missing-assets-");
  await assert.rejects(
    () => runPlan(plan, f.cwd, output),
    /Missing prerequisite: node_modules/,
  );
});
test("test binary build failure starts no matrix check", async (t) => {
  const f = fixture(t);
  const homeParent = tempDir(t, "verification-build-failure-home-");
  writeFileSync(join(f.cwd, ".gitignore"), "node_modules/\n.build/\n");
  mkdirSync(join(f.cwd, "wasm"));
  writeFileSync(join(f.cwd, "wasm/tailserve.wasm"), "fixture");
  mkdirSync(join(f.cwd, "node_modules"));
  writeFileSync(join(f.cwd, "node_modules/.package-lock.json"), "{}");
  mkdirSync(join(f.cwd, ".build"));
  for (const name of ["test.wasm", "speech-fixture.wav", "go-modules.txt"])
    writeFileSync(join(f.cwd, ".build", name), "fixture");
  writeFileSync(join(f.cwd, "tests/profile-sync-browser.mjs"), "// browser.launch(\n");
  writeFileSync(join(f.cwd, "verification/matrix.json"), JSON.stringify({
    version: 1, browserSuites: [{ file: "tests/profile-sync-browser.mjs", mode: "both" }],
    excludedBrowserSuites: [], rules: [{ prefixes: ["docs/"], groups: ["browser"] }],
  }));
  f.git("add", ".gitignore", "wasm", "tests", "verification");
  f.git("commit", "-qm", "browser fixture");
  const commit = f.git("rev-parse", "HEAD");
  const plan = makePlan({ baseCommit: commit, commit, owned: ["docs/"] }, f.cwd);
  const output = tempDir(t, "verification-build-failure-");
  const previousTmpdir = process.env.TMPDIR;
  process.env.TMPDIR = homeParent;
  try {
    await assert.rejects(() => runPlan(plan, f.cwd, output), /ENOENT.*hub|no such file.*hub/i);
    assert.deepEqual(readdirSync(output), []);
    assert.deepEqual(readdirSync(homeParent), [], "failed binary preparation removed its runner home");
  } finally {
    if (previousTmpdir === undefined) delete process.env.TMPDIR;
    else process.env.TMPDIR = previousTmpdir;
  }
});

test("candidate matrix weakening cannot replace the independently approved digest", (t) => {
  const f = fixture(t);
  const approved = digest(
    readFileSync(join(f.cwd, "verification/matrix.json"), "utf8"),
  );
  writeFileSync(
    join(f.cwd, "verification/matrix.json"),
    JSON.stringify({
      version: 1,
      browserSuites: [],
      excludedBrowserSuites: [],
      rules: [{ prefixes: ["docs/"], groups: [] }],
    }),
  );
  assert.throws(
    () =>
      makePlan(
        {
          baseCommit: f.base,
          commit: f.commit,
          owned: ["docs/"],
          approvedMatrixDigest: approved,
          matrixApprovalMessageSeq: 1,
        },
        f.cwd,
      ),
    /approved matrix/,
  );
});
test("every store schema path selects plan-base rehearsal", () => {
  const checks = selectChecks(
    matrix,
    ["hub/internal/store/review_convergence.go"],
    [],
  );
  assert(checks.some((c) => c.id === "migration-rehearsal"));
});
test("removed Go package is excluded from candidate race targets", () => {
  const checks = selectChecks(
    matrix,
    ["hub/internal/deleted-fixture/removed.go"],
    [],
    new Set(),
  );
  assert.deepEqual(checks.find((c) => c.id === "go-race").argv, [
    "go",
    "test",
    "-race",
    "-timeout=25m",
    "./...",
  ]);
});

test("actual renamed and deleted Go packages run race only in candidate packages", (t) => {
  const f = fixture(t);
  const m = {
    version: 1,
    browserSuites: [],
    excludedBrowserSuites: [],
    rules: [
      { prefixes: ["hub/"], groups: ["go"] },
      { prefixes: ["docs/"], groups: ["unit"] },
    ],
  };
  writeFileSync(join(f.cwd, "verification/matrix.json"), JSON.stringify(m));
  mkdirSync(join(f.cwd, "hub/old"), { recursive: true });
  mkdirSync(join(f.cwd, "hub/survivor"), { recursive: true });
  writeFileSync(join(f.cwd, "hub/go.mod"), "module fixture\n\ngo 1.24.0\n");
  writeFileSync(join(f.cwd, "hub/old/fixture.go"), "package moved\n");
  writeFileSync(join(f.cwd, "hub/survivor/fixture.go"), "package survivor\n");
  f.git("add", ".");
  f.git("commit", "-qm", "base packages");
  const base = f.git("rev-parse", "HEAD");
  f.git("mv", "hub/old", "hub/new");
  f.git("commit", "-qm", "move package");
  let commit = f.git("rev-parse", "HEAD");
  let plan = makePlan(
    { baseCommit: base, commit, owned: ["hub/old/fixture.go"] },
    f.cwd,
  );
  let race = plan.checks.find((c) => c.id === "go-race");
  assert.deepEqual(race.argv, ["go", "test", "-race", "./new"]);
  assert.equal(race.environment.VERIFICATION_BASE_COMMIT, base);
  execFileSync(race.argv[0], race.argv.slice(1), {
    cwd: join(f.cwd, "hub"),
    stdio: "pipe",
  });
  f.git("rm", "-r", "hub/new");
  f.git("commit", "-qm", "delete package");
  commit = f.git("rev-parse", "HEAD");
  plan = makePlan(
    { baseCommit: base, commit, owned: ["hub/old/fixture.go"] },
    f.cwd,
  );
  race = plan.checks.find((c) => c.id === "go-race");
  assert.deepEqual(race.argv, ["go", "test", "-race", "./..."]);
  execFileSync(race.argv[0], race.argv.slice(1), {
    cwd: join(f.cwd, "hub"),
    stdio: "pipe",
  });
});

test("approved matrix records default, per-check timeout and required fixed ports", () => {
  const checks = selectChecks(
    matrix,
    ["client/app.js", "hub/internal/store/migrate.go"],
    [],
  );
  assert.equal(
    checks.find((c) => c.id === "go-race").environment.VERIFICATION_TIMEOUT_MS,
    "1800000",
  );
  assert.equal(
    checks.find((c) => c.id === "npm-unit").environment.VERIFICATION_TIMEOUT_MS,
    "120000",
  );
  assert.equal(
    checks.find((c) => c.id === "tests/browser.mjs:chromium").environment
      .VERIFICATION_REQUIRED_PORTS,
    "14318",
  );
  assert.equal(
    checks.find((c) => c.id === "tests/appearance-switching-browser.mjs")
      .environment.VERIFICATION_REQUIRED_PORTS,
    "4319",
  );
  assert.throws(
    () =>
      selectChecks(
        { ...matrix, defaultTimeoutMs: 0, checkTimeoutMs: {} },
        ["docs/readme.md"],
        [],
      ),
    /timeout/,
  );
  assert.throws(
    () =>
      selectChecks(
        { ...matrix, checkTimeoutMs: { "npm-unit": 0 } },
        ["docs/readme.md"],
        [],
      ),
    /timeout/,
  );
});

test("occupied required port reports its PID and never starts or kills a listener", async (t) => {
  const listener = createServer();
  listener.listen(0, "127.0.0.1");
  await once(listener, "listening");
  try {
    const port = listener.address().port;
    const check = {
      id: "fixture-browser",
      argv: [process.execPath, "-e", 'console.log("SHOULD_NOT_START")'],
      cwd: ".",
      environment: {
        VERIFICATION_TIMEOUT_MS: "1000",
        VERIFICATION_REQUIRED_PORTS: String(port),
      },
    };
    const holders = occupiedPorts(check);
    assert(
      holders.some((h) => h.port === port && h.pids.includes(process.pid)),
    );
    const result = await runCheck(check, process.cwd(), {
      PATH: process.env.PATH,
    });
    assert.equal(result.status, -1);
    assert.equal(result.failureReason, "port-conflict");
    assert.match(
      result.stderr,
      new RegExp("Required port " + port + " occupied by PID.*" + process.pid),
    );
    assert(!result.stdout.includes("SHOULD_NOT_START"));
    assert(listener.listening);
    assert(
      occupiedPorts(check).some((h) => h.pids.includes(process.pid)),
      "pre-existing fixture listener remains alive",
    );
  } finally {
    await new Promise((resolve) => listener.close(resolve));
  }
});

test("matrix timeout kills its own process group descendants and retains failed receipt evidence", async (t) => {
  const f = fixture(t),
    external = tempDir(t, "verification-timeout-logs-"),
    info = join(external, "descendant.json");
  const childCode = `const net=require('node:net'),fs=require('node:fs');process.on('SIGTERM',()=>{});const server=net.createServer();server.listen(0,'127.0.0.1',()=>fs.appendFileSync(${JSON.stringify(info)},JSON.stringify({pid:process.pid,parent:process.ppid,port:server.address().port})+String.fromCharCode(10)));setInterval(()=>{},1000);`;
  const parentCode = `import {spawn} from 'node:child_process';process.on('SIGTERM',()=>{});spawn(process.execPath,['-e',${JSON.stringify(childCode)}],{stdio:'ignore'});setInterval(()=>{},1000);`;
  writeFileSync(join(f.cwd, "timeout-fixture.mjs"), parentCode);
  writeFileSync(
    join(f.cwd, "package.json"),
    JSON.stringify({ scripts: { test: "node timeout-fixture.mjs" } }),
  );
  const m = JSON.parse(readFileSync(join(f.cwd, "verification/matrix.json")));
  m.defaultTimeoutMs = 1500;
  writeFileSync(join(f.cwd, "verification/matrix.json"), JSON.stringify(m));
  f.git("add", ".");
  f.git("commit", "-qm", "timeout fixture");
  const commit = f.git("rev-parse", "HEAD");
  const plan = makePlan(
    {
      baseCommit: commit,
      commit,
      owned: ["docs/"],
      verifierAgentId: "fixture",
      verifierRunId: "fixture",
    },
    f.cwd,
  );
  const started = Date.now();
  const receipt = await runPlan(plan, f.cwd, external);
  assert(Date.now() - started < 10000, "timeout completes promptly");
  assert(
    !existsSync(receipt.environment.HOME),
    "timed out run removed its home",
  );
  assert.equal(receipt.checks[0].exitCode, 124);
  assert.equal(receipt.checks[0].failureReason, "timeout");
  const log = readFileSync(receipt.checks[0].logURI, "utf8");
  assert.match(log, /verification failureReason: timeout/);
  assert.equal(receipt.checks[0].logDigest, digest(log));
  assert.equal(receipt.checks[0].environment.VERIFICATION_TIMEOUT_MS, "1500");
  assert.equal(receipt.checks[0].status, "fail");
  assert.equal(receipt.checks[0].attempts.length, 3);
  for (const attempt of receipt.checks[0].attempts) {
    assert.equal(attempt.exitCode, 124);
    assert.equal(attempt.failureReason, "timeout");
    assert.equal(
      attempt.logDigest,
      digest(readFileSync(attempt.logURI, "utf8")),
    );
  }
  const descendants = readFileSync(info, "utf8")
    .trim()
    .split("\n")
    .map((line) => JSON.parse(line));
  assert.equal(descendants.length, 3);
  for (const descendant of descendants) {
    assert.deepEqual(
      occupiedPorts({
        environment: { VERIFICATION_REQUIRED_PORTS: String(descendant.port) },
      }),
      [],
      "descendant listener ended despite ignored SIGTERM",
    );
    let state = "";
    try {
      state = execFileSync(
        "ps",
        ["-p", String(descendant.pid), "-o", "stat="],
        {
          encoding: "utf8",
        },
      ).trim();
    } catch {}
    assert(
      !state || state.startsWith("Z"),
      "descendant is exited rather than a surviving service",
    );
  }
  const saved = JSON.parse(readFileSync(join(external, "receipt.json")));
  assert.equal(saved.checks[0].exitCode, 124);
  assert.equal(saved.checks[0].failureReason, "timeout");
  const schema = JSON.parse(
    readFileSync(
      new URL("../verification/receipt.schema.json", import.meta.url),
    ),
  );
  assert(
    schema.properties.checks.items.properties.failureReason.enum.includes(
      saved.checks[0].failureReason,
    ),
  );
});

for (const [codes, expected] of [
  [[0], "pass"],
  [[1, 0], "flaky"],
  [[1, 1, 0], "flaky"],
  [[1, 1, 1], "fail"],
]) {
  test(
    "actual retry sequence " + codes.join(",") + " records exact evidence",
    async (t) => {
      const f = fixture(t),
        output = tempDir(t, "verification-retry-");
      const counter = join(output, "count.json");
      writeFileSync(
        join(f.cwd, "retry.mjs"),
        `import fs from 'node:fs';const p=${JSON.stringify(counter)};const n=fs.existsSync(p)?JSON.parse(fs.readFileSync(p)):0;fs.writeFileSync(p,JSON.stringify(n+1));console.log('attempt '+(n+1));process.exit(${JSON.stringify(codes)}[n]??1);`,
      );
      writeFileSync(
        join(f.cwd, "package.json"),
        JSON.stringify({ scripts: { test: "node retry.mjs" } }),
      );
      f.git("add", ".");
      f.git("commit", "-qm", "retry fixture");
      const commit = f.git("rev-parse", "HEAD");
      const plan = makePlan(
        { baseCommit: commit, commit, owned: ["docs/"] },
        f.cwd,
      );
      const r = await runPlan(plan, f.cwd, output),
        c = r.checks[0];
      assert.equal(c.status, expected);
      assert.equal(c.attempts.length, codes.length);
      assert.deepEqual(
        c.attempts.map((a) => a.exitCode),
        codes,
      );
      assert.equal(receiptEligible(r), expected !== "fail");
      assert.equal(new Set(c.attempts.map((a) => a.logURI)).size, codes.length);
      for (const [i, a] of c.attempts.entries()) {
        assert.equal(a.attempt, i + 1);
        const log = readFileSync(a.logURI, "utf8");
        assert.match(log, new RegExp("attempt " + (i + 1)));
        assert.equal(a.logDigest, digest(log));
      }
      for (const key of [
        "startedAt",
        "endedAt",
        "durationMs",
        "exitCode",
        "logURI",
        "logDigest",
      ])
        assert.equal(c[key], c.attempts.at(-1)[key]);
    },
  );
}
for (const pass of [false, true])
  test(
    "known failure " +
      (pass ? "now passes" : "exhausts retries without blocking"),
    async (t) => {
      const f = fixture(t),
        m = JSON.parse(readFileSync(join(f.cwd, "verification/matrix.json")));
      m.knownFailures = [
        {
          checkId: "npm-unit",
          bugTaskId: "tsk_aaaaaaaaaaaaaaaa",
          bugId: "wi_bbbbbbbbbbbbbbbb",
        },
      ];
      writeFileSync(join(f.cwd, "verification/matrix.json"), JSON.stringify(m));
      writeFileSync(
        join(f.cwd, "package.json"),
        JSON.stringify({
          scripts: { test: `node -e "process.exit(${pass ? 0 : 1})"` },
        }),
      );
      f.git("add", ".");
      f.git("commit", "-qm", "known fixture");
      const commit = f.git("rev-parse", "HEAD");
      const plan = makePlan(
          { baseCommit: commit, commit, owned: ["docs/"] },
          f.cwd,
        ),
        r = await runPlan(plan, f.cwd, tempDir(t, "known-logs-"));
      assert.equal(r.checks[0].knownFailure, true);
      assert.equal(!!r.checks[0].nowPassing, pass);
      assert.equal(r.checks[0].status, pass ? "pass" : "fail");
      assert.equal(receiptEligible(r), true);
      assert.equal(r.checks[0].attempts.length, pass ? 1 : 3);
    },
  );
test("only knownFailures bytes change invalidates prior approval and plan", (t) => {
  const f = fixture(t),
    raw = readFileSync(join(f.cwd, "verification/matrix.json"), "utf8"),
    approved = digest(raw),
    m = JSON.parse(raw);
  m.knownFailures = [
    {
      checkId: "npm-unit",
      bugTaskId: "tsk_aaaaaaaaaaaaaaaa",
      bugId: "wi_bbbbbbbbbbbbbbbb",
    },
  ];
  writeFileSync(join(f.cwd, "verification/matrix.json"), JSON.stringify(m));
  assert.throws(
    () =>
      rawMakePlan(
        {
          baseCommit: f.base,
          commit: f.commit,
          owned: ["docs/"],
          approvedMatrixDigest: approved,
          matrixApprovalMessageSeq: 1,
        },
        f.cwd,
      ),
    /approved matrix/,
  );
  for (const knownFailures of [
    [m.knownFailures[0], m.knownFailures[0]],
    [{ ...m.knownFailures[0], checkId: "unknown" }],
  ])
    assert.throws(
      () => matrixPolicy({ ...m, knownFailures }, []),
      /known failure/,
    );
});

// wi_82ed4c6924930bad / order #13844: parallel scheduling, targeted fix runs
// and fast-forward bases.
const fakeCheck = (id, extra = {}) => ({
  id,
  argv: ["node", id],
  cwd: ".",
  environment: {},
  ...extra,
});
async function simulate(checks, jobs, duration = () => 5 + Math.random() * 10) {
  let active = 0,
    peak = 0;
  const starts = [];
  const results = await runScheduled(checks, { jobs }, async (check) => {
    starts.push(check.id);
    peak = Math.max(peak, ++active);
    const startedAt = new Date().toISOString();
    await new Promise((resolve) => setTimeout(resolve, duration(check)));
    active--;
    return { ...check, startedAt, endedAt: new Date().toISOString() };
  });
  return { results, peak, starts };
}
test("scheduler bounds concurrency by jobs and one job runs strictly in plan order", async () => {
  const checks = Array.from({ length: 12 }, (_, i) =>
    fakeCheck("check-" + String(i).padStart(2, "0")),
  );
  const parallel = await simulate(checks, 3);
  assert.equal(parallel.peak, 3);
  assert.deepEqual(
    parallel.results.map((r) => r.id),
    checks.map((c) => c.id),
    "results keep plan order",
  );
  const serial = await simulate(checks, 1);
  assert.equal(serial.peak, 1);
  assert.deepEqual(serial.starts, checks.map((c) => c.id));
  for (let i = 1; i < serial.results.length; i++)
    assert(serial.results[i].startedAt >= serial.results[i - 1].endedAt);
  assert.equal(defaultJobs(10, 16 * 1024 ** 3), 5, "Mini default");
  assert.equal(defaultJobs(1, 1024 ** 3), 1);
  assert.equal(defaultJobs(64, 512 * 1024 ** 3), 8);
  for (const jobs of [0, 17, 1.5, "4"])
    await assert.rejects(() => runScheduled(checks, { jobs }, async () => ({})), /--jobs/);
});
test("shared ports, engine splits, Go checks and serial suites never overlap", async (t) => {
  SERIAL_SUITES.set("tests/serial-a.mjs", "fixture shared resource");
  SERIAL_SUITES.set("tests/serial-b.mjs", "fixture shared resource");
  t.after(() => {
    SERIAL_SUITES.delete("tests/serial-a.mjs");
    SERIAL_SUITES.delete("tests/serial-b.mjs");
  });
  const port = { VERIFICATION_REQUIRED_PORTS: "4319" };
  const checks = [
    fakeCheck("go-race", { argv: ["go", "test", "-race"], cwd: "hub" }),
    fakeCheck("go-test", { argv: ["go", "test"], cwd: "hub" }),
    fakeCheck("go-vet", { argv: ["go", "vet"], cwd: "hub" }),
    fakeCheck("npm-unit", { argv: ["npm", "test"] }),
    fakeCheck("tests/a.mjs", { environment: port }),
    fakeCheck("tests/b.mjs", { environment: port }),
    fakeCheck("tests/c.mjs:chromium", { argv: ["node", "tests/c.mjs"] }),
    fakeCheck("tests/c.mjs:webkit", { argv: ["node", "tests/c.mjs"] }),
    fakeCheck("tests/free-1.mjs"),
    fakeCheck("tests/free-2.mjs"),
    fakeCheck("tests/serial-a.mjs", { argv: ["node", "tests/serial-a.mjs"] }),
    fakeCheck("tests/serial-b.mjs", { argv: ["node", "tests/serial-b.mjs"] }),
  ];
  assert.deepEqual(
    executionLocks(checks[4]).filter((l) => executionLocks(checks[5]).includes(l)),
    ["port:4319"],
  );
  assert(executionLocks(checks[0]).includes("go"));
  assert(executionLocks(checks[10]).includes("serial-suites"));
  assert(!executionLocks(checks[8]).includes("serial-suites"));
  const { results, peak } = await simulate(checks, 8, () => 25);
  assert.deepEqual(overlapViolations(results), []);
  assert(peak > 1, "independent checks ran in parallel");
  const span = (id) => results.find((r) => r.id === id);
  const overlaps = (a, b) =>
    span(a).startedAt < span(b).endedAt && span(b).startedAt < span(a).endedAt;
  assert(overlaps("tests/free-1.mjs", "tests/free-2.mjs"));
  for (const [a, b] of [
    ["go-race", "go-test"],
    ["tests/a.mjs", "tests/b.mjs"],
    ["tests/c.mjs:chromium", "tests/c.mjs:webkit"],
    ["tests/serial-a.mjs", "tests/serial-b.mjs"],
  ])
    assert(!overlaps(a, b), a + " overlapped " + b);
  const forged = results.map((r) => ({ ...r }));
  forged[5].startedAt = forged[4].startedAt;
  assert.deepEqual(
    overlapViolations(forged).map((v) => v.reason),
    ["port:4319"],
    "scanner detects a shared-port overlap",
  );
});
test("exclusive checks run alone and hold back every later check", async () => {
  const checks = [
    fakeCheck("early-1"),
    fakeCheck("early-2"),
    fakeCheck("00-static-build"),
    fakeCheck("late-1"),
    fakeCheck("late-2"),
    fakeCheck("wasm-test-build"),
  ];
  const { results, starts } = await simulate(checks, 6, (c) =>
    c.id === "early-2" ? 40 : 10,
  );
  assert.deepEqual(overlapViolations(results), []);
  assert.deepEqual(starts.slice(0, 2), ["early-1", "early-2"]);
  assert.equal(starts[2], "00-static-build", "later checks wait for the barrier");
  assert.equal(starts.at(-1), "wasm-test-build");
  const forged = results.map((r) => ({ ...r }));
  forged[3].startedAt = forged[0].startedAt;
  assert(
    overlapViolations(forged).some((v) => v.reason === "barrier"),
    "scanner detects a later check started past an exclusive barrier",
  );
});
test("full diagnostic selection schedules with no lock or barrier violation", async () => {
  const checks = selectChecks(
    matrix,
    ["client/", "hub/", "scripts/", "tests/", "wasm/"],
    [],
  );
  assert(checks.length >= 70, "diagnostic selection is the full matrix");
  const { results, peak } = await simulate(checks, defaultJobs(10, 16 * 1024 ** 3), () =>
    1 + Math.random() * 6,
  );
  assert.equal(peak, 5);
  assert.deepEqual(results.map((r) => r.id), checks.map((c) => c.id));
  assert.deepEqual(overlapViolations(results), []);
});
test("scheduler stops running checks, starts nothing new and throws on the first error", async () => {
  const checks = [fakeCheck("a"), fakeCheck("b"), fakeCheck("c"), fakeCheck("d")];
  const started = [],
    stopped = [];
  await assert.rejects(
    () =>
      runScheduled(checks, { jobs: 2 }, async (check, _i, signal) => {
        started.push(check.id);
        if (check.id === "b") throw new Error("disk reserve");
        await new Promise((resolve) => signal.addEventListener("abort", resolve));
        stopped.push(check.id);
        return check;
      }),
    /disk reserve/,
  );
  assert.deepEqual(started, ["a", "b"]);
  assert.deepEqual(stopped, ["a"]);
});
function browserFixture(t, suites) {
  const f = fixture(t);
  writeFileSync(join(f.cwd, ".gitignore"), "node_modules/\n.build/\n");
  mkdirSync(join(f.cwd, "wasm"));
  writeFileSync(join(f.cwd, "wasm/tailserve.wasm"), "fixture");
  mkdirSync(join(f.cwd, "node_modules"));
  writeFileSync(join(f.cwd, "node_modules/.package-lock.json"), "{}");
  mkdirSync(join(f.cwd, ".build"));
  for (const name of ["test.wasm", "speech-fixture.wav", "go-modules.txt"])
    writeFileSync(join(f.cwd, ".build", name), "fixture");
  for (const [name, source] of Object.entries(suites))
    writeFileSync(join(f.cwd, "tests", name), "// browser.launch(\n" + source);
  writeFileSync(
    join(f.cwd, "package.json"),
    JSON.stringify({
      scripts: {
        test: 'node -e "process.exit(0)"',
        "build:static": 'node -e "process.exit(0)"',
        "verify:release": 'node -e "process.exit(0)"',
      },
    }),
  );
  writeFileSync(
    join(f.cwd, "verification/matrix.json"),
    JSON.stringify({
      version: 1,
      maxAttempts: 3,
      knownFailures: [],
      browserSuites: Object.keys(suites).map((name) => ({
        file: "tests/" + name,
        mode: "both",
      })),
      excludedBrowserSuites: [],
      rules: [
        { prefixes: ["docs/"], groups: ["unit"] },
        { prefixes: ["client/"], groups: ["unit", "browser"] },
        { prefixes: ["hub/"], groups: ["go"] },
      ],
    }),
  );
  f.git("add", ".");
  f.git("commit", "-qm", "browser fixture");
  const commit = f.git("rev-parse", "HEAD");
  f.git("checkout", "-q", "--detach", commit);
  return { ...f, commit };
}
test("concurrent checks keep plan-ordered receipts and record the job count", async (t) => {
  const f = browserFixture(t, {
    "slow-browser.mjs": "setTimeout(()=>console.log('slow done'),600);",
    "fast-browser.mjs": "console.log('fast done');",
  });
  const plan = makePlan(
    { baseCommit: f.commit, commit: f.commit, owned: ["client/"] },
    f.cwd,
  );
  const receipt = await runPlan(plan, f.cwd, tempDir(t, "verification-parallel-"), {
    minFreeBytes: 0,
    jobs: 4,
  });
  assert.deepEqual(receipt.checks.map((c) => c.id), plan.checks.map((c) => c.id));
  assert.equal(receipt.environment.VERIFICATION_JOBS, "4");
  assert.deepEqual(overlapViolations(receipt.checks), []);
  const slow = receipt.checks.find((c) => c.id === "tests/slow-browser.mjs"),
    fast = receipt.checks.find((c) => c.id === "tests/fast-browser.mjs");
  assert(fast.startedAt < slow.endedAt && slow.startedAt < fast.endedAt);
  assert(receipt.checks.every((c) => c.status === "pass"));
});
test("CLI SIGINT stops every concurrent check group, keeps partial logs and leaves no receipt", async (t) => {
  const root = tempDir(t, "verification-concurrent-signal-");
  const suite = (name) =>
    `import {spawn} from 'node:child_process';import fs from 'node:fs';` +
    `const c=spawn(process.execPath,['-e','process.on("SIGTERM",()=>{});setInterval(()=>{},1000)'],{stdio:'ignore'});` +
    `fs.writeFileSync(${JSON.stringify(join(root, name))},JSON.stringify([process.pid,c.pid]));` +
    `console.log('partial ${name}');setInterval(()=>{},1000);`;
  const f = browserFixture(t, {
    "one-browser.mjs": suite("one"),
    "two-browser.mjs": suite("two"),
  });
  const plan = makePlan(
    { baseCommit: f.commit, commit: f.commit, owned: ["client/"] },
    f.cwd,
  );
  const planFile = join(root, "plan.json"),
    output = tempDir(t, "verification-concurrent-signal-logs-");
  writeFileSync(planFile, JSON.stringify(plan));
  const script = new URL("../scripts/verify-matrix.mjs", import.meta.url).pathname;
  const runner = spawn(
    process.execPath,
    [script, "run", planFile, output, "--min-free-bytes", "0", "--jobs", "3"],
    { cwd: f.cwd, env: { ...process.env, TMPDIR: root }, stdio: "ignore" },
  );
  const pids = [];
  try {
    for (let i = 0; !(existsSync(join(root, "one")) && existsSync(join(root, "two"))) && i < 250; i++)
      await new Promise((resolve) => setTimeout(resolve, 20));
    for (const name of ["one", "two"])
      pids.push(...JSON.parse(readFileSync(join(root, name), "utf8")));
    runner.kill("SIGINT");
    const [code] = await once(runner, "close");
    assert.equal(code, 130);
    const alive = (pid) => {
      try {
        process.kill(pid, 0);
        return true;
      } catch {
        return false;
      }
    };
    for (let i = 0; pids.some(alive) && i < 100; i++)
      await new Promise((resolve) => setTimeout(resolve, 20));
    assert.deepEqual(pids.filter(alive), [], "every check group was stopped");
    assert(!existsSync(join(output, "receipt.json")));
    for (const name of ["one", "two"]) {
      const log = readFileSync(
        join(output, digest(`tests/${name}-browser.mjs`) + ".attempt-1.log"),
        "utf8",
      );
      assert.match(log, new RegExp("partial " + name));
      assert.match(log, /failureReason: interrupted/);
    }
  } finally {
    if (runner.exitCode === null && runner.signalCode === null) runner.kill("SIGKILL");
    for (const pid of pids)
      try {
        process.kill(pid, "SIGKILL");
      } catch {}
  }
});
function commitChange(f, path, content, message) {
  mkdirSync(join(f.cwd, path, ".."), { recursive: true });
  writeFileSync(join(f.cwd, path), content);
  f.git("add", ".");
  f.git("commit", "-qm", message);
  return f.git("rev-parse", "HEAD");
}
test("targeted mode selects checks only from the fix's changed paths", async (t) => {
  const f = browserFixture(t, { "fixture-browser.mjs": "" });
  mkdirSync(join(f.cwd, "hub"));
  writeFileSync(join(f.cwd, "hub/go.mod"), "module fixture\n\ngo 1.24.0\n");
  const previous = commitChange(f, "hub/pkg/a.go", "package pkg\n", "hub package");
  const docsFix = commitChange(f, "docs/fix.md", "fix\n", "docs fix");
  const hubFix = commitChange(f, "hub/pkg/a.go", "package pkg\n\n// fix\n", "hub fix");
  const clientFix = commitChange(f, "client/app.js", "export {}\n", "client fix");
  const ids = (baseCommit, commit) =>
    makeTargetedPlan({ baseCommit, commit }, f.cwd).checks.map((c) => c.id);
  assert.deepEqual(ids(previous, docsFix), ["npm-unit"]);
  assert.deepEqual(ids(docsFix, hubFix), ["go-race", "go-test", "go-vet"]);
  assert.deepEqual(
    makeTargetedPlan({ baseCommit: docsFix, commit: hubFix }, f.cwd).checks.find(
      (c) => c.id === "go-race",
    ).argv,
    ["go", "test", "-race", "./pkg"],
  );
  assert.deepEqual(ids(hubFix, clientFix), [
    "00-static-build",
    "01-static-release-verify",
    "npm-unit",
    "tests/fixture-browser.mjs",
  ]);
  const unknown = commitChange(f, "unknown/file.txt", "x\n", "unknown path");
  assert.throws(() => ids(clientFix, unknown), /Unknown path: unknown\/file.txt/);
  assert.throws(() => ids(docsFix, docsFix), /differs from its previous/);

  f.git("checkout", "-q", "--detach", docsFix);
  const output = tempDir(t, "verification-targeted-");
  const receipt = await runTargeted({ baseCommit: previous, commit: docsFix }, f.cwd, output, {
    minFreeBytes: 0,
  });
  assert.equal(receipt.targeted, true);
  assert.deepEqual(receipt.checks.map((c) => c.id), ["npm-unit"]);
  assert.equal(receipt.checks[0].status, "pass");
  assert(existsSync(join(output, "targeted-receipt.json")));
  assert(!existsSync(join(output, "receipt.json")), "never a gating receipt");
  assert.equal(receipt.aiv, undefined);

  writeFileSync(join(f.cwd, "docs/fix.md"), "dirty\n");
  await assert.rejects(
    () => runTargeted({ baseCommit: previous, commit: docsFix }, f.cwd, tempDir(t, "vt-dirty-")),
    /Clean detached/,
  );
  f.git("checkout", "--", "docs/fix.md");
  f.git("checkout", "-q", "-B", "attached", docsFix);
  await assert.rejects(
    () => runTargeted({ baseCommit: previous, commit: docsFix }, f.cwd, tempDir(t, "vt-attached-")),
    /Clean detached/,
  );
});
test("plans and targeted runs refuse a base the candidate does not contain", async (t) => {
  const f = fixture(t);
  f.git("checkout", "-q", "--detach", f.base);
  writeFileSync(join(f.cwd, "docs/other.md"), "diverged\n");
  f.git("add", ".");
  f.git("commit", "-qm", "diverged tip");
  const diverged = f.git("rev-parse", "HEAD");
  f.git("checkout", "-q", "--detach", f.commit);
  const context = { baseCommit: diverged, commit: f.commit, owned: ["docs/"] };
  assert.throws(() => makePlan(context, f.cwd), /not a fast-forward of its base; rebase onto the current tip/);
  const linear = makePlan({ ...context, baseCommit: f.base }, f.cwd);
  await assert.rejects(
    () => runPlan({ ...linear, baseCommit: diverged }, f.cwd, tempDir(t, "vt-diverged-")),
    /not a fast-forward/,
  );
  assert.throws(
    () => makeTargetedPlan({ baseCommit: diverged, commit: f.commit }, f.cwd),
    /not a fast-forward/,
  );
  assert.equal(makePlan({ ...context, baseCommit: f.commit }, f.cwd).changed.length, 0);
  assert.deepEqual(linear.changed, ["docs/old.md", "docs/new.md"]);
});

// #15095: the Go lane is the critical path. Go checks start first, go-race
// holds two slots, go-vet leaves the lane, Go checks bypass the exclusive
// barriers, and non-Go checks run at a lower CPU priority.
async function simulateLoad(checks, jobs, duration) {
  let load = 0,
    peak = 0;
  const starts = [];
  const results = await runScheduled(checks, { jobs }, async (check) => {
    starts.push(check.id);
    const weight = check.id === "go-race" ? Math.min(jobs, 2) : 1;
    peak = Math.max(peak, (load += weight));
    const startedAt = new Date().toISOString();
    await new Promise((resolve) => setTimeout(resolve, duration(check)));
    load -= weight;
    return { ...check, startedAt, endedAt: new Date().toISOString() };
  });
  return { results, starts, peak };
}
const goFixture = (id, argv) => fakeCheck(id, { argv, cwd: "hub" });
const criticalPathPlan = () => [
  fakeCheck("00-static-build", { argv: ["npm", "run", "build:static"] }),
  fakeCheck("01-static-release-verify", { argv: ["npm", "run", "verify:release"] }),
  goFixture("go-race", ["go", "test", "-race", "./..."]),
  goFixture("go-test", ["go", "test", "./..."]),
  goFixture("go-vet", ["go", "vet", "./..."]),
  fakeCheck("npm-unit", { argv: ["npm", "test"] }),
  ...Array.from({ length: 8 }, (_, i) => fakeCheck(`tests/s${i}.mjs`)),
  fakeCheck("wasm-test-build", { argv: ["bash", "scripts/build-wasm.sh", "--test"] }),
];
test("Go checks start first and bypass exclusive barriers while one job keeps plan order", async () => {
  const checks = criticalPathPlan();
  const duration = (c) => (c.id === "go-race" ? 120 : c.id === "go-test" ? 20 : 10);
  const { results, starts } = await simulateLoad(checks, 5, duration);
  assert.equal(starts[0], "go-race", "the longest check starts at once");
  assert.deepEqual(overlapViolations(results), []);
  const span = (id) => results.find((r) => r.id === id);
  const overlaps = (a, b) =>
    span(a).startedAt < span(b).endedAt && span(b).startedAt < span(a).endedAt;
  assert(overlaps("go-race", "00-static-build"), "Go does not wait for the static build");
  assert(overlaps("go-race", "go-vet"), "go-vet runs beside go-race");
  assert(!overlaps("go-race", "go-test"), "go-test stays in the Go lane");
  assert(overlaps("go-race", "wasm-test-build"), "wasm-test-build is off the Go tail");
  assert(span("wasm-test-build").endedAt <= span("go-race").endedAt);
  for (let i = 0; i < 8; i++)
    assert(span(`tests/s${i}.mjs`).startedAt >= span("01-static-release-verify").endedAt);
  const serial = await simulateLoad(checks, 1, () => 2);
  assert.deepEqual(serial.starts, checks.map((c) => c.id), "one job runs in plan order");
});
test("go-race holds two job slots so fewer checks run beside it", async () => {
  const checks = criticalPathPlan();
  let besideRace = 0;
  const { peak, results } = await simulateLoad(checks, 5, (c) =>
    c.id === "go-race" ? 80 : 10,
  );
  assert.equal(peak, 5, "slots are fully used and never exceeded");
  const race = results.find((r) => r.id === "go-race");
  const times = results.flatMap((r) => [r.startedAt, r.endedAt]).filter((t) => t > race.startedAt && t < race.endedAt);
  for (const t of times) {
    const running = results.filter((r) => r.id !== "go-race" && r.startedAt <= t && t < r.endedAt).length;
    besideRace = Math.max(besideRace, running);
  }
  assert(besideRace <= 3, `at most three checks beside go-race, saw ${besideRace}`);
  assert.equal(
    (await simulateLoad([goFixture("go-race", ["go"]), fakeCheck("x")], 2, () => 5)).peak,
    2,
    "a weight never exceeds the job count",
  );
});
test("overlap scan exempts Go checks from barriers but not from the Go lane", () => {
  const at = (s) => new Date(Date.UTC(2026, 8, 29, 0, 0, s)).toISOString();
  const run = (check, start, end) => ({ ...check, startedAt: at(start), endedAt: at(end) });
  const checks = criticalPathPlan();
  const byId = Object.fromEntries(checks.map((c) => [c.id, c]));
  assert.deepEqual(
    overlapViolations([
      run(byId["00-static-build"], 0, 10),
      run(byId["go-race"], 0, 100),
      run(byId["go-vet"], 1, 2),
      run(byId["wasm-test-build"], 50, 60),
    ]),
    [],
  );
  assert.deepEqual(
    overlapViolations([run(byId["go-race"], 0, 100), run(byId["go-test"], 50, 60)]).map((v) => v.reason),
    ["go"],
  );
  assert.deepEqual(
    overlapViolations([run(byId["00-static-build"], 0, 10), run(byId["npm-unit"], 5, 20)]).map((v) => v.reason),
    ["barrier"],
  );
});
test("non-Go check process groups run at the lower priority and Go checks do not", async () => {
  const probe = (cwd) => ({
    id: "probe-" + cwd,
    argv: [process.execPath, "-e", "console.log(require('node:os').getPriority())"],
    cwd,
    environment: { VERIFICATION_TIMEOUT_MS: "10000" },
  });
  const dir = mkdtempSync(join(tmpdir(), "verification-priority-"));
  try {
    const nonGo = await runCheck(probe("."), dir, { PATH: process.env.PATH });
    const goRun = await runCheck(probe("hub"), dir, { PATH: process.env.PATH });
    // Relative to the runner's priority, which is itself raised when this
    // file runs inside a niced npm-unit check.
    const own = getPriority();
    assert.equal(nonGo.stdout.trim(), String(Math.min(19, own + NON_GO_NICE)));
    assert.equal(goRun.stdout.trim(), String(own));
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

// Owner decision #15114: the approved matrix carries go test's package
// timeout for go-test and go-race only.
test("approved goTestFlags reach only go-test and go-race, and only as a timeout", () => {
  assert.deepEqual(matrix.goTestFlags, ["-timeout=25m"]);
  const checks = selectChecks(matrix, ["hub/internal/store/migrate.go"], []);
  const argv = (id) => checks.find((c) => c.id === id).argv;
  assert.deepEqual(argv("go-test"), ["go", "test", "-timeout=25m", "./..."]);
  assert.deepEqual(argv("go-race"), ["go", "test", "-race", "-timeout=25m", "./internal/store"]);
  assert.deepEqual(argv("go-vet"), ["go", "vet", "./..."]);
  assert(!argv("migration-rehearsal").includes("-timeout=25m"));
  const { goTestFlags, ...legacy } = matrix;
  assert.deepEqual(
    selectChecks(legacy, ["hub/cmd/tt/main.go"], []).find((c) => c.id === "go-test").argv,
    ["go", "test", "./..."],
    "a matrix without the key keeps the previous argv",
  );
  for (const flags of ["-timeout=14m", ["-run=X"], ["-timeout=0m"], ["-timeout=14m", "-count=1"]])
    assert.throws(
      () => selectChecks({ ...matrix, goTestFlags: flags }, ["hub/cmd/tt/main.go"], []),
      /Invalid matrix goTestFlags/,
    );
});
