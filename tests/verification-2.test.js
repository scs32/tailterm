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
import { tmpdir, getPriority, homedir, userInfo } from "node:os";
import { join, relative, isAbsolute, basename } from "node:path";
import { createHash } from "node:crypto";
import { crc32 as zlibCrc32 } from "node:zlib";
import { verificationCacheRoot } from "../scripts/package-speech-model.mjs";
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
  canonical,
} from "../scripts/verify-matrix.mjs";
// Named separately so the red tests load on a runner that lacks these exports.
import * as matrixRunner from "../scripts/verify-matrix.mjs";
import {
  acquireHostLock,
  readJournal,
  resolveRunPriority,
  lookupItemPriority,
} from "../scripts/verify-matrix-host-lock.mjs";
import {
  readHostState,
  fakeTTLookups,
  makePlan,
  matrix,
  fixture,
  tempDir,
  commandPlan,
  fixturePids,
  leakingCheck,
  pidOf,
  alive,
  fakeCheck,
  simulate,
  browserFixture,
  tokenProcesses,
  interruptedConcurrentRun,
  commitChange,
  simulateLoad,
  goFixture,
  criticalPathPlan,
  HOST_KEYS,
  hostLockFile,
  matrixScript,
  sleepingCheck,
  untilHost,
  matrixCLI,
  plannedFixture,
  holdHost,
  prerequisitePaths,
  preflightFixture,
  preflightRun,
  within,
} from "./verification-fixtures.mjs";

// A stand-in go's answer to the go env -json the binary helper records with
// each build, read from the stand-in's own environment.
const goEnvStandIn =
  `if [ "$1" = env ]; then printf '{"GOVERSION":"go1.99.0","GOROOT":"/stand-in","GOPATH":"%s","GOMODCACHE":"%s",` +
  `"GOCACHE":"%s","GOFLAGS":"%s","GOPROXY":"%s","GOTOOLCHAIN":"%s","GOOS":"js","GOARCH":"wasm"}\\n' ` +
  `"$GOPATH" "$GOMODCACHE" "$GOCACHE" "$GOFLAGS" "$GOPROXY" "$GOTOOLCHAIN"; exit 0; fi\n`;
const goEnvCall = "env -json GOVERSION GOROOT GOPATH GOMODCACHE GOCACHE GOFLAGS GOPROXY GOTOOLCHAIN GOOS GOARCH";
const pause = (ms) => new Promise((done) => setTimeout(done, ms));
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
test("profile startup failure emits child exit, stderr and phase diagnostics", async (t) => {
  const root = new URL("..", import.meta.url).pathname;
  const dir = mkdtempSync(join(tmpdir(), "profile-startup-failure-"));
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  // The spawned check starts Vite before its readiness wait fails, so give it a
  // cache of its own instead of the real check's .build/vite-cache directory.
  const viteCacheRoot = join(dir, "vite-cache");
  const binary = join(dir, "hub");
  writeFileSync(binary, "#!/bin/sh\necho synthetic-startup-boom >&2\nexit 7\n");
  chmodSync(binary, 0o755);
  const manifest = join(dir, "manifest.json");
  writeFileSync(manifest, JSON.stringify({ binaries: [{ target: "hub", historicalCommit: null,
    source: await sourceIdentity(root), path: binary, sha256: await fileHash(binary) }] }));
  const result = spawnSync(process.execPath, ["tests/profile-sync-browser.mjs"], {
    cwd: root, encoding: "utf8", timeout: 120000,
    env: { PATH: process.env.PATH, HOME: process.env.HOME, TMPDIR: process.env.TMPDIR,
      TAILTERM_TEST_BINARIES: manifest, TAILTERM_VITE_CACHE_ROOT: viteCacheRoot },
  });
  assert.equal(result.error, undefined);
  assert.equal(result.signal, null);
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
    "node",
    "../scripts/verify-matrix.mjs",
    "go-race",
    "-timeout=45m",
    "-shards=./internal/store=4",
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
        getAvailableBytes: () => (++probes < 4 ? 10 : 9),
      }),
    /Insufficient free space/,
  );
  assert.equal(
    readFileSync(counter, "utf8"),
    "x",
    "only the first attempt ran",
  );
  assert.equal(probes, 4);
  assert.equal(readHostState(hostLockFile()).holder, null, "retry refusal releases its lease");
  assert(!existsSync(join(output, "receipt.json")), "no receipt after refused retry");
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
    // The descendant creates descendant.json before it finishes writing it, so
    // wait until the content parses: an existing file can still be empty.
    for (const deadline = Date.now() + 10_000; !descendantInfo; ) {
      try {
        descendantInfo = JSON.parse(readFileSync(info, "utf8"));
      } catch (error) {
        if (error.code !== "ENOENT" && !(error instanceof SyntaxError)) throw error;
        assert(
          runner.exitCode === null && runner.signalCode === null && Date.now() < deadline,
          "descendant listener started and wrote complete descendant.json",
        );
        await new Promise((resolve) => setTimeout(resolve, 20));
      }
    }
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
    // The runner starts the check in a process group of its own, which only
    // the runner's interruption path stops. A failure before that path ran
    // interrupts the runner first, then kills the runner's whole group.
    if (runner.exitCode === null && runner.signalCode === null) {
      try {
        process.kill(-runner.pid, "SIGINT");
      } catch (error) {
        if (error.code !== "ESRCH") throw error;
      }
      await Promise.race([
        once(runner, "close"),
        new Promise((resolve) => setTimeout(resolve, 10_000).unref()),
      ]);
    }
    try {
      process.kill(-runner.pid, "SIGKILL");
    } catch (error) {
      if (error.code !== "ESRCH") throw error;
    }
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
test("fixture pid cleanup stops its own child and spares a recorded pid it does not own", async (t) => {
  // The decoy stands for a stranger that took a recorded pid after its owner
  // exited. The test holds its handle, so nothing here is found by number.
  const decoy = spawn(process.execPath, ["-e", "setInterval(()=>{},1000)"], { stdio: "ignore" });
  t.after(() => decoy.kill("SIGKILL"));
  await once(decoy, "spawn");
  let child, childExit;
  await t.test("a fixture holding one child and the decoy's pid", async (s) => {
    const pids = fixturePids(s);
    child = spawn(process.execPath, [join(pids, "child.cjs"), "own"], { stdio: "ignore" });
    childExit = once(child, "exit");
    // The child creates own.pid before it writes the pid, so wait for the
    // number itself: an empty file would read as pid 0.
    const written = () => existsSync(join(pids, "own.pid")) && /^\d+$/.test(readFileSync(join(pids, "own.pid"), "utf8"));
    for (const deadline = Date.now() + 10_000; !written(); ) {
      assert(child.exitCode === null && Date.now() < deadline, "the fixture child never started");
      await new Promise((resolve) => setTimeout(resolve, 20));
    }
    assert.equal(pidOf(pids, "own"), child.pid);
    writeFileSync(join(pids, "decoy.pid"), String(decoy.pid));
  });
  const [, signal] = await childExit;
  assert.equal(signal, "SIGKILL", "cleanup stopped the fixture's own child");
  // A killed decoy is a zombie, or already reaped, by the time ps answers.
  const state = spawnSync("ps", ["-o", "state=", "-p", String(decoy.pid)], { encoding: "utf8" }).stdout.trim();
  assert(state && !state.startsWith("Z"), `cleanup signalled a pid it does not own (state ${state || "gone"})`);
  assert(decoy.exitCode === null && decoy.signalCode === null);
});
test("codex stub shadows any real codex on the check PATH and logs each call", async (t) => {
  const f = fixture(t),
    plan = commandPlan(
      f,
      `import {spawnSync} from 'node:child_process';const r=spawnSync('sh',['-c','command -v codex; codex --version; echo status=$?'],{encoding:'utf8'});process.stdout.write(r.stdout);process.stderr.write(r.stderr);`,
    );
  const output = tempDir(t, "verification-codex-stub-logs-");
  const receipt = await runPlan(plan, f.cwd, output);
  const home = receipt.environment.HOME,
    log = readFileSync(receipt.checks[0].logURI, "utf8");
  assert(receiptEligible(receipt));
  assert(receipt.environment.PATH.startsWith(home + "/.verifier-bin:"));
  assert(log.split("\n").includes(home + "/.verifier-bin/codex"), log);
  assert.match(log, /tailterm verifier: codex is disabled during matrix checks/);
  assert.match(log, /^status=1$/m, "a real codex would exit 0");
  const calls = readFileSync(join(output, "codex-stub-calls.log"), "utf8")
    .split("\n")
    .filter(Boolean);
  assert.equal(calls.length, 1);
  assert.match(calls[0], /\t--version$/);
  assert.deepEqual(matrixRunner.verifierHomeProcesses(home), []);
  assert(!existsSync(home));
});
test("home sweep stops setsid'd, TERM-ignoring, env-only and realpath-cwd leftovers", async (t) => {
  const f = fixture(t),
    pids = fixturePids(t),
    names = ["held", "env", "cwd"];
  const plan = commandPlan(
    f,
    leakingCheck(
      pids,
      names,
      `start('held',{cwd:pids,env:{...elsewhere,IGNORE_TERM:'1'}},home+'/held');start('env',{cwd:pids});start('cwd',{cwd:fs.realpathSync(home),env:elsewhere});`,
    ),
  );
  const output = tempDir(t, "verification-home-sweep-logs-");
  const receipt = await runPlan(plan, f.cwd, output);
  assert(receiptEligible(receipt));
  assert(!existsSync(receipt.environment.HOME));
  for (const name of names)
    assert(!alive(pidOf(pids, name)), name + " outlived the verifier home");
  const stopped = JSON.parse(readFileSync(join(output, "home-processes.json"), "utf8"));
  const reasons = Object.fromEntries(stopped.processes.map((p) => [p.pid, p.reasons]));
  assert.equal(stopped.home, receipt.environment.HOME);
  assert.deepEqual(reasons[pidOf(pids, "held")], ["file"]);
  assert.deepEqual(reasons[pidOf(pids, "env")], ["env"]);
  assert.deepEqual(reasons[pidOf(pids, "cwd")], ["file"]);
});
test("home sweep spares a sibling-prefix home and never signals the runner", async (t) => {
  const f = fixture(t),
    pids = fixturePids(t);
  const plan = commandPlan(
    f,
    leakingCheck(
      pids,
      ["sibling", "leak"],
      `const sibling=home+'-sibling';fs.mkdirSync(sibling);fs.writeFileSync(pids+'/sibling.path',sibling);start('sibling',{cwd:sibling,env:{...process.env,HOME:sibling,TMPDIR:sibling,PWD:sibling}},sibling+'/held');start('leak',{cwd:pids});`,
    ),
  );
  let signalled = "";
  const onTerm = () => (signalled = "SIGTERM");
  process.on("SIGTERM", onTerm);
  t.after(() => process.off("SIGTERM", onTerm));
  const receipt = await runPlan(plan, f.cwd, tempDir(t, "verification-sibling-logs-"));
  assert(receiptEligible(receipt));
  assert(!alive(pidOf(pids, "leak")));
  assert(alive(pidOf(pids, "sibling")), "sibling-prefix process was stopped");
  assert(existsSync(readFileSync(join(pids, "sibling.path"), "utf8")));
  assert.equal(signalled, "", "runner process was signalled");
});
test("home process stop failure leaves no receipt and retains the home", async (t) => {
  const f = fixture(t),
    plan = commandPlan(f, "console.log('check passed');");
  const output = tempDir(t, "verification-stop-failure-logs-");
  let retained = "";
  await assert.rejects(
    () =>
      runPlan(plan, f.cwd, output, {
        stopHomeProcesses: async (home) => {
          retained = home;
          throw new Error("synthetic survivor pid 1234");
        },
      }),
    /Failed to stop processes under verifier home .*synthetic survivor pid 1234/,
  );
  t.after(() => {
    if (retained && existsSync(retained)) removeVerifierHome(retained);
  });
  assert(existsSync(retained));
  assert(!existsSync(join(output, "receipt.json")));
  const failure = JSON.parse(readFileSync(join(output, "cleanup-error.json")));
  assert.equal(failure.home, retained);
  assert.match(failure.error, /synthetic survivor pid 1234/);
  assert(existsSync(join(output, digest("npm-unit") + ".attempt-1.log")));
});
test("keep-home still stops processes left under the home", async (t) => {
  const f = fixture(t),
    pids = fixturePids(t);
  const plan = commandPlan(f, leakingCheck(pids, ["env"], `start('env',{cwd:pids});`));
  const receipt = await runPlan(plan, f.cwd, tempDir(t, "verification-kept-sweep-logs-"), {
    keepHome: true,
  });
  t.after(() => removeVerifierHome(receipt.environment.HOME));
  assert(existsSync(receipt.environment.HOME));
  assert(!alive(pidOf(pids, "env")));
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
  rmSync(join(f.cwd, "node_modules"), { recursive: true });
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
  mkdirSync(join(f.cwd, "node_modules"), { recursive: true });
  writeFileSync(join(f.cwd, "node_modules/.package-lock.json"), "{}");
  mkdirSync(join(f.cwd, ".build"));
  for (const name of ["test.wasm", "speech-fixture.wav", "go-modules.txt"])
    writeFileSync(join(f.cwd, ".build", name), "fixture");
  writeFileSync(
    join(f.cwd, "tests/profile-sync-browser.mjs"),
    '// browser.launch(\nimport { prepareTestBinary } from "./test-binaries.mjs";\n',
  );
  writeFileSync(join(f.cwd, "verification/matrix.json"), JSON.stringify({
    version: 1, browserSuites: [{ file: "tests/profile-sync-browser.mjs", mode: "both" }],
    excludedBrowserSuites: [], rules: [{ prefixes: ["docs/"], groups: ["browser"] }],
  }));
  // A real module whose hub command does not parse: go build itself fails, at
  // package load, so nothing is compiled.
  mkdirSync(join(f.cwd, "hub/cmd/tt"), { recursive: true });
  mkdirSync(join(f.cwd, "hub/cmd/tailterm-hub"));
  writeFileSync(join(f.cwd, "hub/go.mod"), "module fixture\n\ngo 1.22\n");
  writeFileSync(join(f.cwd, "hub/cmd/tt/main.go"), "package main\n\nfunc main() {}\n");
  writeFileSync(join(f.cwd, "hub/cmd/tailterm-hub/main.go"), "package main\n\nfunc main() {\n");
  f.git("add", ".gitignore", "wasm", "tests", "verification", "hub");
  f.git("commit", "-qm", "browser fixture");
  const commit = f.git("rev-parse", "HEAD");
  const plan = makePlan({ baseCommit: commit, commit, owned: ["docs/"] }, f.cwd);
  const output = tempDir(t, "verification-build-failure-");
  const previousTmpdir = process.env.TMPDIR;
  process.env.TMPDIR = homeParent;
  // The runner's own way of making its home, recording the exact directory.
  const home = { path: "", existed: false };
  const createHome = () => {
    home.path = mkdtempSync(join(realpathSync("/tmp"), "tv-"));
    home.existed = existsSync(home.path);
    return home.path;
  };
  t.after(() => home.path && rmSync(home.path, { recursive: true, force: true }));
  try {
    await assert.rejects(
      () => runPlan(plan, f.cwd, output, { createHome }),
      /go build[^]*cmd\/tailterm-hub\/main\.go:\d+:\d+: syntax error/,
    );
    assert.deepEqual(readdirSync(output).sort(), ["host-lock.json", "host-lock.log"]);
    assert.deepEqual(readdirSync(homeParent), [], "failed binary preparation removed its runner home");
    assert.notEqual(home.path, "");
    assert(home.existed, "the injected runner home was created");
    assert(!existsSync(home.path), "failed binary preparation removed its exact runner home");
  } finally {
    if (previousTmpdir === undefined) delete process.env.TMPDIR;
    else process.env.TMPDIR = previousTmpdir;
  }
});
test("a prepared test binary changed by a check leaves no receipt", { timeout: 120000 }, async (t) => {
  // The run happens in a child process whose PATH puts a stand-in go first
  // before it imports the runner; no real build runs. The stand-in answers go
  // env, writes a small executable for build -o and logs each call.
  const tools = tempDir(t, "verification-binary-mutation-"),
    goCalls = join(tools, "go-calls.log"),
    suiteRecord = join(tools, "suite.json"),
    output = tempDir(t, "verification-binary-mutation-logs-");
  writeFileSync(
    join(tools, "go"),
    `#!/bin/sh\nprintf '%s\\n' "$*" >> '${goCalls}'\n${goEnvStandIn}[ "$1" = build ] || exit 1\nout=\nwhile [ $# -gt 0 ]; do [ "$1" = -o ] && out=$2; shift; done\n[ -n "$out" ] || exit 1\nprintf '#!/bin/sh\\nexit 0\\n' > "$out" && chmod +x "$out"\n`,
    { mode: 0o755 },
  );
  // The suite's name selects binary preparation. It records the home it ran
  // in and appends a byte to the first prepared binary.
  const f = browserFixture(t, {
    "profile-sync-browser.mjs":
      `import './test-binaries.mjs';import fs from 'node:fs';const m=JSON.parse(fs.readFileSync(process.env.TAILTERM_TEST_BINARIES,'utf8'));` +
      `fs.writeFileSync(${JSON.stringify(suiteRecord)},JSON.stringify({home:process.env.HOME,binary:m.binaries[0].path}));` +
      `fs.appendFileSync(m.binaries[0].path,'x');`,
  });
  // A listed binary suite imports the binary helper; a stand-in here.
  writeFileSync(join(f.cwd, "tests/test-binaries.mjs"), "export {};\n");
  mkdirSync(join(f.cwd, "hub"));
  writeFileSync(join(f.cwd, "hub/go.mod"), "module fixture\n\ngo 1.22\n");
  // Planned on its own commit, so the hub tree selects no Go check.
  const commit = commitChange(f, "hub/cmd/tt/main.go", "package main\n\nfunc main() {}\n", "hub tree");
  const plan = makePlan({ baseCommit: commit, commit, owned: ["client/"] }, f.cwd);
  assert(plan.checks.some((check) => check.id === "tests/profile-sync-browser.mjs"));
  writeFileSync(join(tools, "plan.json"), JSON.stringify(plan));
  // The runner's own way of making its home, recording the exact directory.
  writeFileSync(
    join(tools, "run.mjs"),
    `import { mkdtempSync, existsSync, readFileSync, readdirSync, realpathSync } from "node:fs";
import { join } from "node:path";
const [runner, planFile, cwd, output, suiteRecord] = process.argv.slice(2);
const { runPlan } = await import(runner);
const home = { path: "", existed: false };
let error = "";
try {
  await runPlan(JSON.parse(readFileSync(planFile, "utf8")), cwd, output, {
    minFreeBytes: 0,
    jobs: 1,
    createHome: () => {
      home.path = mkdtempSync(join(realpathSync("/tmp"), "tv-"));
      home.existed = existsSync(home.path);
      return home.path;
    },
  });
} catch (failure) {
  error = failure.message;
}
console.log(JSON.stringify({
  error,
  home: home.path,
  existed: home.existed,
  exists: existsSync(home.path),
  suite: existsSync(suiteRecord) ? JSON.parse(readFileSync(suiteRecord, "utf8")) : null,
  output: readdirSync(output).sort(),
}));
`,
  );
  const child = spawn(
    process.execPath,
    [
      join(tools, "run.mjs"),
      new URL("../scripts/verify-matrix.mjs", import.meta.url).href,
      join(tools, "plan.json"),
      f.cwd,
      output,
      suiteRecord,
    ],
    {
      cwd: f.cwd,
      env: {
        ...process.env,
        PATH: tools + ":" + process.env.PATH,
        TAILTERM_MATRIX_HOST_LOCK: join(tools, "host.json"),
      },
      stdio: ["ignore", "pipe", "pipe"],
    },
  );
  t.after(() => child.exitCode === null && child.signalCode === null && child.kill("SIGKILL"));
  let stdout = "",
    stderr = "";
  child.stdout.on("data", (chunk) => (stdout += chunk));
  child.stderr.on("data", (chunk) => (stderr += chunk));
  const [code] = await once(child, "close");
  assert.equal(code, 0, stderr);
  const result = JSON.parse(stdout.trim().split("\n").at(-1));
  t.after(() => result.home && rmSync(result.home, { recursive: true, force: true }));
  assert.match(result.error, /Prepared test binary changed during verification/);
  assert(result.error.endsWith(": " + result.suite.binary), result.error);
  assert(!result.output.includes("receipt.json"), String(result.output));
  assert(
    result.output.includes(digest("tests/profile-sync-browser.mjs") + ".attempt-1.log"),
    String(result.output),
  );
  assert.notEqual(result.home, "");
  assert.equal(result.suite.home, result.home, "the suite ran in the injected runner home");
  assert(result.existed, "the injected runner home was created");
  assert(!result.exists, "the refused run removed its exact runner home");
  assert.deepEqual(readFileSync(goCalls, "utf8").split("\n").filter(Boolean), [
    goEnvCall,
    `build -ldflags=-s -w -o ${join(output, "tailterm-hub-test")} ./cmd/tailterm-hub`,
    goEnvCall,
    `build -o ${join(output, "tailterm-tt-test")} ./cmd/tt`,
  ]);
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
    "node",
    "../scripts/verify-matrix.mjs",
    "go-race",
    "-timeout=45m",
    "-shards=./internal/store=4",
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
    "3000000",
  );
  assert.equal(
    checks.find((c) => c.id === "npm-unit").environment.VERIFICATION_TIMEOUT_MS,
    "480000",
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

test("matrix selection admits one-hour check timeouts and refuses higher or invalid values", () => {
  for (const timeout of [3000000, 3600000]) {
    for (const limits of [
      { defaultTimeoutMs: timeout, checkTimeoutMs: {} },
      { checkTimeoutMs: { "npm-unit": timeout } },
    ]) {
      const checks = selectChecks({ ...matrix, ...limits }, ["docs/readme.md"], []);
      assert.equal(checks[0].environment.VERIFICATION_TIMEOUT_MS, String(timeout));
    }
  }
  for (const timeout of [3600001, 0, -1, 1.5, Number.MAX_SAFE_INTEGER + 1, "3600000"]) {
    for (const limits of [
      { defaultTimeoutMs: timeout, checkTimeoutMs: {} },
      { checkTimeoutMs: { "npm-unit": timeout } },
    ])
      assert.throws(
        () => selectChecks({ ...matrix, ...limits }, ["docs/readme.md"], []),
        /Invalid matrix check timeout: npm-unit/,
      );
  }
});

test("check execution admits a one-hour timer and rejects higher or invalid timers before launch", async () => {
  const check = (timeout) => ({
    id: "timeout-boundary",
    argv: [process.execPath, "-e", 'console.log("boundary child ran")'],
    cwd: ".",
    environment: { VERIFICATION_TIMEOUT_MS: String(timeout) },
  });
  const result = await runCheck(check(3600000), process.cwd(), { PATH: process.env.PATH });
  assert.equal(result.status, 0);
  assert.equal(result.failureReason, "");
  assert.equal(result.stdout.trim(), "boundary child ran");
  for (const timeout of [3600001, 0, -1, 1.5, Number.MAX_SAFE_INTEGER + 1, "invalid"])
    await assert.rejects(
      runCheck(check(timeout), process.cwd(), { PATH: process.env.PATH }),
      /Invalid approved check timeout/,
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
  // Under host load npm, the check and its descendant can take longer than
  // the 1500 ms check timeout to start, and that attempt then records no
  // descendant to kill (seen at load 6 to 8: one of three recorded). Each
  // attempt's timeout is held, one more period at a time and at most 40, until
  // that attempt's descendant has recorded its listener; every other timer is
  // untouched. The periods added are taken off the promptness bound. That
  // bound is 30 s, not 10 s: the three timeouts and kills take 5.25 s and the
  // rest is the runner's own start, home and cleanup work, which alone passed
  // 5 s under a process-start load. A run that waited on the descendants,
  // which ignore SIGTERM and never exit, would not return at all.
  const realSetTimeout = globalThis.setTimeout;
  t.after(() => {
    globalThis.setTimeout = realSetTimeout;
  });
  const recorded = () => (existsSync(info) ? readFileSync(info, "utf8").split("\n").length - 1 : 0);
  let attempts = 0,
    heldMs = 0;
  globalThis.setTimeout = (callback, delay, ...args) => {
    if (delay !== 1500) return realSetTimeout(callback, delay, ...args);
    const attempt = ++attempts;
    let held = 0;
    const timer = realSetTimeout(() => {
      if (held < 40 && recorded() < attempt) {
        held++;
        heldMs += delay;
        timer.refresh();
      } else callback(...args);
    }, delay);
    return timer;
  };
  const started = Date.now();
  const receipt = await runPlan(plan, f.cwd, external);
  globalThis.setTimeout = realSetTimeout;
  assert(Date.now() - started - heldMs < 30000, "timeout completes promptly");
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
test("run refuses a plan whose retry policy was edited after planning, before any check starts", async (t) => {
  const known = {
    checkId: "npm-unit",
    bugTaskId: "tsk_aaaaaaaaaaaaaaaa",
    bugId: "wi_bbbbbbbbbbbbbbbb",
  };
  // The one check writes a marker outside the repository, so a check that
  // started is seen even when the run is refused afterwards.
  const planned = (knownFailures) => {
    const f = fixture(t),
      marker = join(tempDir(t, "verification-policy-marker-"), "ran");
    if (knownFailures.length) {
      const m = JSON.parse(readFileSync(join(f.cwd, "verification/matrix.json")));
      writeFileSync(
        join(f.cwd, "verification/matrix.json"),
        JSON.stringify({ ...m, knownFailures }),
      );
    }
    const plan = commandPlan(
      f,
      `import fs from 'node:fs';fs.writeFileSync(${JSON.stringify(marker)},'ran');`,
    );
    return { f, marker, plan };
  };
  const refused = async ({ f, marker }, edited) => {
    const output = tempDir(t, "verification-policy-logs-");
    await assert.rejects(
      () => runPlan(edited, f.cwd, output),
      /Altered or omitted required checks/,
    );
    assert(!existsSync(marker), "a check ran before the refusal");
    assert.deepEqual(readdirSync(output), []);
  };
  const plain = planned([]);
  assert.equal(plain.plan.maxAttempts, 3);
  assert.equal(plain.plan.knownFailures, undefined);
  await refused(plain, { ...plain.plan, knownFailures: [known] });
  await refused(plain, { ...plain.plan, maxAttempts: 1 });
  const { maxAttempts, ...withoutAttempts } = plain.plan;
  await refused(plain, withoutAttempts);
  const listed = planned([known]);
  assert.deepEqual(listed.plan.knownFailures, [known]);
  const { knownFailures, ...withoutKnown } = listed.plan;
  await refused(listed, withoutKnown);
  // The untouched plan still runs its check, so the marker is a live signal.
  await runPlan(plain.plan, plain.f.cwd, tempDir(t, "verification-policy-logs-"));
  assert(existsSync(plain.marker));
});
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
test("CLI SIGINT stops every concurrent check group, keeps partial logs and leaves no receipt", (t) =>
  interruptedConcurrentRun(t));
test("a check group that never publishes its pid file is still stopped when the wait times out", async (t) => {
  const run = {};
  await assert.rejects(
    () => interruptedConcurrentRun(t, { withhold: ["two"], publishWithinMs: 3_000 }, run),
    /timed out waiting for these check groups to publish a pid file: two$/,
  );
  assert(run.runner.exitCode !== null || run.runner.signalCode !== null, "the runner exited");
  assert.equal(run.started.length, 4);
  // Judged by the run's token and process state, as the cleanup is: a bare
  // pid probe also answers for a zombie or for a stranger that took the pid.
  const left = tokenProcesses(run.token);
  assert.deepEqual(
    run.started.filter((pid) => left.includes(pid)),
    [],
    "a started check group process is still running",
  );
  assert.deepEqual(left, []);
});
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
    /does not contain previous candidate/,
  );
  assert.equal(makePlan({ ...context, baseCommit: f.commit }, f.cwd).changed.length, 0);
  assert.deepEqual(linear.changed, ["docs/old.md", "docs/new.md"]);
});
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
  assert.deepEqual(matrix.goTestFlags, ["-timeout=45m"]);
  const checks = selectChecks(matrix, ["hub/internal/store/migrate.go"], []);
  const argv = (id) => checks.find((c) => c.id === id).argv;
  assert.deepEqual(argv("go-test"), ["go", "test", "-timeout=45m", "./..."]);
  assert.deepEqual(argv("go-race"), [
    "node", "../scripts/verify-matrix.mjs", "go-race", "-timeout=45m",
    "-shards=./internal/store=4", "./internal/store",
  ]);
  assert.deepEqual(argv("go-vet"), ["go", "vet", "./..."]);
  assert(!argv("migration-rehearsal").includes("-timeout=45m"));
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

test("preflight refuses missing or unreadable selected inputs and invalid space in both run modes before acquisition", async (t) => {
  const homeRoot = tempDir(t, "verification-preflight-homes-");
  const previousTmpdir = process.env.TMPDIR;
  process.env.TMPDIR = homeRoot;
  t.after(() => {
    if (previousTmpdir === undefined) delete process.env.TMPDIR;
    else process.env.TMPDIR = previousTmpdir;
  });
  for (const browser of [false, true]) {
    const f = preflightFixture(t, browser);
    for (const mode of ["run", "targeted"]) {
      let acquisitions = 0, homes = 0;
      const options = {
        getAvailableBytes: () => 2147483648,
        acquireHostLock: () => { acquisitions++; throw new Error("unexpected acquisition"); },
        removeHome: (home) => { homes++; removeVerifierHome(home); },
      };
      const refuse = async (overrides, expected) => {
        const output = tempDir(t, "verification-preflight-logs-");
        const directoriesBefore = readdirSync(homeRoot);
        await assert.rejects(preflightRun(mode, f, output, { ...options, ...overrides }), expected);
        assert.equal(acquisitions, 0, "no host acquisition call");
        assert.deepEqual(readdirSync(output), [], "no commands, sidecars or receipts");
        assert.equal(homes, 0, "no verifier home cleanup needed");
        assert.deepEqual(readdirSync(homeRoot), directoriesBefore, "no verifier home allocated");
      };
      for (const path of browser ? prerequisitePaths : prerequisitePaths.slice(0, 1)) {
        const bytes = readFileSync(join(f.cwd, path));
        rmSync(join(f.cwd, path));
        try {
          await refuse({}, new RegExp("Missing prerequisite: " + path.replace(/[.*]/g, "\\$&")));
        } finally {
          writeFileSync(join(f.cwd, path), bytes);
        }
        await refuse({ readPrerequisiteFile: (file) => {
          if (file === join(f.cwd, path)) throw Object.assign(new Error("synthetic access denied"), { code: "EACCES" });
          return readFileSync(file);
        } }, /Unreadable prerequisite: .*synthetic access denied/);
      }
      for (const reserve of [-1, NaN, null, Infinity, 1.5])
        await refuse({ minFreeBytes: reserve }, /Invalid --min-free-bytes reserve/);
      await refuse({ minFreeBytes: 10, getAvailableBytes: () => 9 }, /Insufficient free space/);
      for (const free of [-1, NaN, Infinity, undefined, 1.5])
        await refuse({ getAvailableBytes: () => free }, /Unable to determine free space/);
      await refuse({ getAvailableBytes: () => { throw new Error("synthetic statfs failure"); } }, /Unable to determine free space.*synthetic statfs failure/);
    }
  }
});

test("preflight derives unique npm and browser inputs while Go-only admission reads neither", async (t) => {
  const f = preflightFixture(t, true);
  const read = [];
  const reader = (file) => { read.push(file); return readFileSync(file); };
  const npm = [{ id: "npm-unit", argv: ["npm", "test"] }];
  const browser = [
    ...npm, { id: "test-browser", argv: ["node", "test.mjs"] },
    { id: "other", argv: ["node", "other.mjs"], environment: { TEST_BROWSER: "both" } },
  ];
  assert.deepEqual(matrixRunner.readPrerequisites(npm, f.cwd, reader).map((p) => p.path), prerequisitePaths.slice(0, 1));
  assert.deepEqual(read.splice(0), [join(f.cwd, prerequisitePaths[0])]);
  const prerequisites = matrixRunner.readPrerequisites(browser, f.cwd, reader);
  assert.deepEqual(prerequisites.map((p) => p.path), prerequisitePaths);
  assert.deepEqual(read.splice(0), prerequisitePaths.map((path) => join(f.cwd, path)));
  for (const entry of prerequisites)
    assert.equal(entry.sha256, digest(readFileSync(join(f.cwd, entry.path), "utf8")));
  assert.deepEqual(matrixRunner.readPrerequisites([{ id: "go-test", argv: ["go", "test"], cwd: "hub" }], f.cwd, reader), []);
  assert.deepEqual(read, []);

  for (const browser of [false, true]) {
    const prepared = browser ? f : preflightFixture(t);
    for (const mode of ["run", "targeted"]) {
      await assert.rejects(preflightRun(mode, prepared, tempDir(t, "verification-input-preflight-"), {
        getAvailableBytes: () => 2147483648,
        readPrerequisiteFile: reader,
        acquireHostLock: () => { throw new Error("selected inputs passed preflight"); },
      }), /selected inputs passed preflight/);
      assert.deepEqual(read.splice(0), (browser ? prerequisitePaths : prerequisitePaths.slice(0, 1)).map((path) => join(prepared.cwd, path)));
    }
  }

  mkdirSync(join(f.cwd, "hub"));
  writeFileSync(join(f.cwd, "hub/go.mod"), "module fixture\n\ngo 1.24.0\n");
  f.git("add", ".");
  f.git("commit", "-qm", "Go fixture setup");
  const baseCommit = f.git("rev-parse", "HEAD");
  const commit = commitChange(f, "hub/pkg/a.go", "package pkg\n", "Go-only fix");
  const go = { ...f, context: { baseCommit, commit } };
  go.plan = makePlan({ ...go.context, owned: ["hub/"] }, f.cwd);
  rmSync(join(f.cwd, "node_modules"), { recursive: true });
  rmSync(join(f.cwd, "wasm/tailserve.wasm"));
  rmSync(join(f.cwd, ".build"), { recursive: true });
  for (const mode of ["run", "targeted"]) {
    let admissions = 0;
    await assert.rejects(preflightRun(mode, go, tempDir(t, "verification-go-preflight-"), {
      getAvailableBytes: () => 2147483648,
      readPrerequisiteFile: () => assert.fail("Go-only has no npm or browser inputs"),
      acquireHostLock: () => { admissions++; throw new Error("prepared Go plan reached admission"); },
    }), /prepared Go plan reached admission/);
    assert.equal(admissions, 1);
  }
});

test("preflight rechecks inputs and space after admission and hashes the held bytes", async (t) => {
  for (const [mode, browser] of [["run", false], ["run", true], ["targeted", false], ["targeted", true]]) {
    for (const change of ["delete", "unreadable", "space", "content"]) {
      const f = preflightFixture(t, browser);
      const output = tempDir(t, "verification-held-preflight-");
      const changedPath = prerequisitePaths[browser ? 2 : 0];
      const path = join(f.cwd, changedPath);
      let admitted = false, released = false, homes = 0;
      const options = {
        minFreeBytes: 10,
        getAvailableBytes: () => admitted && change === "space" ? 9 : 10,
        readPrerequisiteFile: (file) => {
          if (admitted && change === "unreadable")
            throw Object.assign(new Error("became unreadable"), { code: "EACCES" });
          return readFileSync(file);
        },
        acquireHostLock: async (args) => {
          const lease = await acquireHostLock(args);
          admitted = true;
          if (change === "delete") rmSync(path);
          if (change === "content") writeFileSync(path, "new held bytes");
          return { ...lease, release: async (summary) => {
            await lease.release(summary);
            released = true;
          } };
        },
        removeHome: (home) => { homes++; removeVerifierHome(home); },
      };
      if (change === "content") {
        const receipt = await preflightRun(mode, f, output, options);
        assert.deepEqual(receipt.prerequisites.map((p) => p.path), browser ? prerequisitePaths : prerequisitePaths.slice(0, 1));
        assert.equal(receipt.prerequisites.find((p) => p.path === changedPath).sha256, digest("new held bytes"));
        assert(receipt.checks.every((c) => c.exitCode === 0));
        assert.equal(homes, 1);
      } else {
        await assert.rejects(preflightRun(mode, f, output, options),
          change === "space" ? /Insufficient free space/ : change === "delete" ? /Missing prerequisite/ : /Unreadable prerequisite/);
        assert.equal(homes, 0, "held refusal allocates no home");
        assert.deepEqual(readdirSync(output).sort(), ["host-lock.json", "host-lock.log"], "no command or receipt");
      }
      assert(admitted && released, "lease released after held recheck");
      assert.equal(readHostState(hostLockFile()).holder, null);
      assert.deepEqual(readHostState(hostLockFile()).waiters, []);
    }
  }
});

test("preflight CLI missing npm dependencies never joins or disturbs a private holder", async (t) => {
  const { f, planFile } = plannedFixture(t, "console.log('must not run');");
  rmSync(join(f.cwd, "node_modules"), { recursive: true, force: true });
  const holder = await holdHost("wi_preflight_holder");
  try {
    const state = readFileSync(hostLockFile(), "utf8");
    const journal = readJournal(hostLockFile());
    const output = tempDir(t, "verification-preflight-cli-");
    const result = spawnSync(process.execPath, [matrixScript, "run", planFile, output], {
      cwd: f.cwd, env: { ...process.env }, encoding: "utf8", timeout: 1500,
    });
    assert.equal(result.status, 1, result.stderr);
    assert.match(result.stderr, /Missing prerequisite: node_modules\/\.package-lock\.json/);
    assert.deepEqual(readdirSync(output), [], "no host record, command log or receipt");
    assert.equal(readFileSync(hostLockFile(), "utf8"), state, "holder and waitlist unchanged");
    assert.deepEqual(readJournal(hostLockFile()), journal, "no request or grant");
  } finally {
    await holder.release();
  }
});

test("preflight CLI rejects every missing browser asset, unreadable inputs and low space without joining in both modes", async (t) => {
  const f = preflightFixture(t, true);
  const inputs = tempDir(t, "verification-preflight-cli-inputs-");
  const homeRoot = tempDir(t, "verification-preflight-cli-homes-");
  const holder = await holdHost("wi_browser_preflight_holder");
  try {
    for (const mode of ["run", "targeted"]) {
      const input = join(inputs, mode + ".json");
      writeFileSync(input, JSON.stringify(mode === "run" ? f.plan : f.context));
      const refuse = (flags, expected) => {
        const state = readFileSync(hostLockFile(), "utf8");
        const journal = readJournal(hostLockFile());
        const output = tempDir(t, "verification-preflight-cli-logs-");
        const result = spawnSync(process.execPath, [matrixScript, mode, input, output, "--priority", "normal", ...flags], {
          cwd: f.cwd, env: { ...process.env, TMPDIR: homeRoot }, encoding: "utf8", timeout: 10000,
        });
        assert.equal(result.status, 1, result.stderr);
        assert.match(result.stderr, expected);
        assert.deepEqual(readdirSync(output), [], "no record, command log or receipt");
        assert.deepEqual(readdirSync(homeRoot), [], "no verifier home");
        assert.equal(readFileSync(hostLockFile(), "utf8"), state, "existing holder undisturbed");
        assert.deepEqual(readJournal(hostLockFile()), journal, "no request or grant");
      };
      for (const path of prerequisitePaths) {
        const file = join(f.cwd, path), bytes = readFileSync(file);
        rmSync(file);
        try {
          refuse([], new RegExp("Missing prerequisite: " + path.replace(/[.*]/g, "\\$&")));
          mkdirSync(file);
          refuse([], /Unreadable prerequisite:/);
        } finally {
          rmSync(file, { recursive: true, force: true });
          writeFileSync(file, bytes);
        }
      }
      refuse(["--min-free-bytes", "9007199254740991"], /Insufficient free space/);
      refuse(["--min-free-bytes", "NaN"], /--min-free-bytes requires a nonnegative integer/);
    }
  } finally {
    await holder.release();
  }
});

test("V1 an uncontended run records the twelve host keys in its receipt, matching the sidecar, inside the receipt schema", async (t) => {
  assert(hostLockFile().startsWith(tmpdir() + "/") || hostLockFile().startsWith("/private" + tmpdir() + "/"));
  const f = fixture(t),
    plan = commandPlan(f, sleepingCheck(300)),
    output = tempDir(t, "verification-host-logs-");
  const started = Date.now();
  const receipt = await runPlan(plan, f.cwd, output, { minFreeBytes: 0 });
  const elapsed = Date.now() - started;
  const e = receipt.environment;
  for (const key of HOST_KEYS) assert.equal(typeof e[key], "string", key);
  assert.equal(HOST_KEYS.length, 12);
  assert.equal(e.VERIFICATION_HOST_LOCK, hostLockFile());
  assert.equal(e.VERIFICATION_HOST_PRIORITY, "normal");
  assert.equal(e.VERIFICATION_HOST_PRIORITY_SOURCE, "default");
  assert.equal(e.VERIFICATION_HOST_QUEUE_POSITION, "0");
  assert.equal(e.VERIFICATION_HOST_QUEUE_LENGTH, "0");
  assert.equal(e.VERIFICATION_HOST_GRANTS_BEFORE_START, "0");
  assert.equal(e.VERIFICATION_HOST_OVERTAKEN_BY, "0");
  assert.equal(e.VERIFICATION_HOST_OVERLAP, "0");
  assert.equal(e.VERIFICATION_CHECK_SET, "unit");
  assert.match(e.VERIFICATION_HOST_WAIT_MS, /^\d+$/);
  assert(Number(e.VERIFICATION_HOST_WAIT_MS) <= elapsed);
  const duration = Number(e.VERIFICATION_RUN_DURATION_MS);
  assert(duration >= 300 && duration <= elapsed, `duration ${duration} within 300..${elapsed}`);
  const loads = e.VERIFICATION_HOST_LOAD.split(",");
  assert(loads.length >= 2 && loads.length <= 64, "sampled at acquire and at the end");
  for (const load of loads) assert(/^\d+(\.\d+)?$/.test(load), load);
  const sidecar = JSON.parse(readFileSync(join(output, "host-lock.json"), "utf8"));
  assert.deepEqual(
    {
      VERIFICATION_HOST_LOCK: sidecar.lockPath,
      VERIFICATION_HOST_PRIORITY: sidecar.priority,
      VERIFICATION_HOST_PRIORITY_SOURCE: sidecar.prioritySource,
      VERIFICATION_HOST_WAIT_MS: String(sidecar.waitMs),
      VERIFICATION_HOST_QUEUE_POSITION: String(sidecar.queuePosition),
      VERIFICATION_HOST_QUEUE_LENGTH: String(sidecar.queueLength),
      VERIFICATION_HOST_GRANTS_BEFORE_START: String(sidecar.grantsBeforeStart),
      VERIFICATION_HOST_OVERTAKEN_BY: String(sidecar.overtakenBy),
      VERIFICATION_HOST_OVERLAP: String(sidecar.overlap),
      VERIFICATION_CHECK_SET: sidecar.checkSet,
      VERIFICATION_RUN_DURATION_MS: String(sidecar.durationMs),
      VERIFICATION_HOST_LOAD: sidecar.loadSamples.map((s) => String(s.load1)).join(","),
    },
    Object.fromEntries(HOST_KEYS.map((key) => [key, e[key]])),
  );
  for (const value of [sidecar.waitMs, sidecar.queuePosition, sidecar.durationMs, sidecar.cpuCount])
    assert.equal(typeof value, "number");
  assert.equal(sidecar.kind, "run");
  assert.equal(sidecar.outcome, "released");
  assert.equal(sidecar.pid, process.pid);
  assert(Date.parse(sidecar.releasedAt) >= Date.parse(sidecar.acquiredAt));
  const schema = JSON.parse(
    readFileSync(new URL("../verification/receipt.schema.json", import.meta.url)),
  );
  const saved = JSON.parse(readFileSync(join(output, "receipt.json"), "utf8"));
  assert.deepEqual(saved, JSON.parse(JSON.stringify(receipt)));
  for (const key of Object.keys(saved)) assert(key in schema.properties, "top-level " + key);
  for (const check of saved.checks)
    for (const key of Object.keys(check))
      assert(key in schema.properties.checks.items.properties, "check " + key);
  assert.equal(readHostState(hostLockFile()).holder, null, "released after the run");
});

test("V2 a CLI run with no flags, as the deployer calls it, waits visibly behind a holder and then runs at normal", async (t) => {
  const { f, planFile } = plannedFixture(t, "console.log('ran');"),
    output = tempDir(t, "verification-host-logs-");
  const holder = await holdHost("wi_v2_holder");
  const child = matrixCLI(t, f, ["run", planFile, output]);
  const line = `matrix host: waiting 1 of 1, holder wi_v2_holder/holder/pid ${process.pid}\n`;
  await untilHost(() => child.text.includes(line), "the waiting line; saw " + child.text);
  assert(!existsSync(join(output, "receipt.json")), "no check ran while waiting");
  const waiter = readHostState(hostLockFile()).waiters[0];
  assert.deepEqual([waiter.kind, waiter.priority, waiter.prioritySource, waiter.pid], ["run", "normal", "default", child.pid]);
  await holder.release();
  const [code] = await once(child, "close");
  assert.equal(code, 0, child.text);
  assert.match(child.text, /matrix host: acquired after \d+ ms\n/);
  const e = JSON.parse(readFileSync(join(output, "receipt.json"), "utf8")).environment;
  assert.equal(e.VERIFICATION_HOST_PRIORITY, "normal");
  assert.equal(e.VERIFICATION_HOST_PRIORITY_SOURCE, "default");
  assert.equal(e.VERIFICATION_HOST_QUEUE_POSITION, "1");
  assert.equal(e.VERIFICATION_HOST_QUEUE_LENGTH, "1");
  assert(Number(e.VERIFICATION_HOST_WAIT_MS) > 0);
  assert(readFileSync(join(output, "host-lock.log"), "utf8").includes(line.trim()));
});

test("V3 SIGTERM while a CLI run waits leaves the list, exits 143, and records the wait without a receipt", async (t) => {
  const { f, planFile } = plannedFixture(t, "console.log('ran');"),
    output = tempDir(t, "verification-host-logs-");
  const holder = await holdHost("wi_v3_holder");
  try {
    const child = matrixCLI(t, f, ["run", planFile, output]);
    const line = `matrix host: waiting 1 of 1, holder wi_v3_holder/holder/pid ${process.pid}`;
    await untilHost(() => child.text.includes(line), "the waiting line; saw " + child.text);
    const waiter = readHostState(hostLockFile()).waiters[0];
    child.kill("SIGTERM");
    const [code] = await once(child, "close");
    assert.equal(code, 143, child.text);
    assert(!existsSync(join(output, "receipt.json")), "no receipt");
    const state = readHostState(hostLockFile());
    assert.deepEqual(state.waiters, [], "the waiter left the list");
    assert.equal(state.holder.id, holder.id, "the holder was not disturbed");
    const log = readFileSync(join(output, "host-lock.log"), "utf8");
    assert.match(log, new RegExp("^\\S+Z " + line.replace(/[/]/g, "\\/") + "$", "m"));
    const sidecar = JSON.parse(readFileSync(join(output, "host-lock.json"), "utf8"));
    assert.equal(sidecar.id, waiter.id);
    assert.equal(sidecar.outcome, "withdrawn");
    assert.equal(sidecar.acquiredAt, null);
    assert.equal(sidecar.queuePosition, 1);
    assert(sidecar.waitMs > 0);
    const withdrawn = readJournal(hostLockFile()).filter((entry) => entry.id === waiter.id).map((entry) => [entry.event, entry.waitMs, entry.reason, entry.removed]);
    assert.deepEqual(withdrawn, [["request", undefined, undefined, undefined], ["withdrawn", sidecar.waitMs, "SIGTERM", true]]);
  } finally {
    await holder.release();
  }
});

test("V4 priority comes from the flag, then the environment, then normal, and bad values are refused before joining", (t) => {
  const { f, plan, planFile } = plannedFixture(t, "console.log('ran');");
  const planBytes = readFileSync(planFile, "utf8");
  for (const key of Object.keys(plan)) assert.doesNotMatch(key, /priority/i, "the plan gains no priority field");
  const run = (flags, environment = {}) => {
    const output = tempDir(t, "verification-host-logs-");
    const result = spawnSync(process.execPath, [matrixScript, "run", planFile, output, "--min-free-bytes", "0", ...flags], {
      cwd: f.cwd,
      env: { ...process.env, ...environment },
      encoding: "utf8",
    });
    return { ...result, output };
  };
  const recorded = (result) => {
    assert.equal(result.status, 0, result.stderr);
    const e = JSON.parse(readFileSync(join(result.output, "receipt.json"), "utf8")).environment;
    return [e.VERIFICATION_HOST_PRIORITY, e.VERIFICATION_HOST_PRIORITY_SOURCE];
  };
  assert.deepEqual(recorded(run([])), ["normal", "default"]);
  assert.deepEqual(recorded(run([], { TAILTERM_MATRIX_PRIORITY: "high" })), ["high", "environment"]);
  assert.deepEqual(recorded(run(["--priority", "urgent"], { TAILTERM_MATRIX_PRIORITY: "high" })), ["urgent", "flag"]);
  assert.deepEqual(recorded(run(["--priority", "high", "--host-wait-minutes", "5", "--item", "wi_named"])), ["high", "flag"]);
  assert.equal(readJournal(hostLockFile()).at(-1).item, "wi_named", "--item names a run whose plan has no item");
  const requests = readHostState(hostLockFile()).requestSeq;
  for (const [flags, environment, message] of [
    [["--priority", "soon"], {}, /--priority must be urgent, high or normal/],
    [["--priority"], {}, /--priority must be urgent, high or normal/],
    [[], { TAILTERM_MATRIX_PRIORITY: "asap" }, /TAILTERM_MATRIX_PRIORITY must be urgent, high or normal/],
    [["--host-wait-minutes", "0"], {}, /--host-wait-minutes requires a whole number of minutes from 1 to 1440/],
    [["--host-wait-minutes", "1441"], {}, /--host-wait-minutes requires/],
    [["--host-wait-minutes"], {}, /--host-wait-minutes requires/],
  ]) {
    const result = run(flags, environment);
    assert.equal(result.status, 1, flags.join(" "));
    assert.match(result.stderr, message);
    assert.deepEqual(readdirSync(result.output), [], "refused before any record");
  }
  assert.equal(readHostState(hostLockFile()).requestSeq, requests, "no refused run joined the list");
  assert.equal(readFileSync(planFile, "utf8"), planBytes);
});


test("V4a with no flag and no environment override a run takes its work item's priority, low as normal, source item", () => {
  const asked = [];
  const from = (priority) =>
    resolveRunPriority(undefined, "wi_mapped", {
      environment: {},
      lookup: (item) => (asked.push(item), priority),
      warn: (line) => assert.fail("no warning expected: " + line),
    });
  assert.deepEqual(from("urgent"), { priority: "urgent", prioritySource: "item" });
  assert.deepEqual(from("high"), { priority: "high", prioritySource: "item" });
  assert.deepEqual(from("normal"), { priority: "normal", prioritySource: "item" });
  assert.deepEqual(from("low"), { priority: "normal", prioritySource: "item" });
  assert.deepEqual(asked, ["wi_mapped", "wi_mapped", "wi_mapped", "wi_mapped"]);
});

test("V4b --priority, then TAILTERM_MATRIX_PRIORITY, win over the item's priority without a lookup, and bad values still throw", () => {
  const options = (environment) => ({
    environment,
    lookup: () => assert.fail("no lookup when a priority is given"),
    warn: (line) => assert.fail("no warning expected: " + line),
  });
  assert.deepEqual(resolveRunPriority("high", "wi_urgent", options({})), { priority: "high", prioritySource: "flag" });
  assert.deepEqual(
    resolveRunPriority(undefined, "wi_urgent", options({ TAILTERM_MATRIX_PRIORITY: "normal" })),
    { priority: "normal", prioritySource: "environment" },
  );
  assert.deepEqual(
    resolveRunPriority("urgent", "wi_urgent", options({ TAILTERM_MATRIX_PRIORITY: "high" })),
    { priority: "urgent", prioritySource: "flag" },
  );
  assert.throws(() => resolveRunPriority("low", "wi_urgent", options({})), /--priority must be urgent, high or normal/);
  assert.throws(() => resolveRunPriority("", "wi_urgent", options({})), /--priority must be urgent, high or normal/);
  assert.throws(
    () => resolveRunPriority(undefined, "wi_urgent", options({ TAILTERM_MATRIX_PRIORITY: "low" })),
    /TAILTERM_MATRIX_PRIORITY must be urgent, high or normal/,
  );
});

test("V4c a lookup that fails, times out, finds no item or an unrecognised priority joins at normal, source default, with one warning", () => {
  const fallback = (item, lookup) => {
    const warnings = [];
    const resolved = resolveRunPriority(undefined, item, { environment: {}, lookup, warn: (line) => warnings.push(line) });
    assert.deepEqual(resolved, { priority: "normal", prioritySource: "default" });
    assert.equal(warnings.length, 1, warnings.join(" | "));
    assert(warnings[0].startsWith("matrix host: joining at normal priority (default): "), warnings[0]);
    return warnings[0];
  };
  const never = () => assert.fail("no lookup without an item");
  assert.match(fallback("wi_gone", () => { throw new Error("the lookup failed: work item not found"); }), /work item wi_gone: the lookup failed: work item not found$/);
  assert.match(fallback("wi_slow", () => { throw new Error("the lookup timed out after 15000 ms"); }), /work item wi_slow: the lookup timed out after 15000 ms$/);
  assert.match(fallback("wi_odd", () => "critical"), /work item wi_odd has the unrecognised priority "critical"$/);
  assert.match(fallback("wi_odd", () => 2), /work item wi_odd has the unrecognised priority 2$/);
  assert.match(fallback("wi_bare", () => undefined), /work item wi_bare has no priority$/);
  assert.match(fallback(undefined, never), /the run names no work item$/);
  assert.match(fallback("unknown", never), /the run names no work item$/);
});

test("V4d the tt lookup reads the item's priority and reports a failed, unreadable, missing or timed-out lookup within its bound", (t) => {
  const environment = (extra) => ({ ...process.env, ...extra });
  const before = fakeTTLookups().length;
  assert.equal(lookupItemPriority("wi_real", environment({ FAKE_TT_PRIORITY: "urgent" })), "urgent");
  assert.deepEqual(fakeTTLookups().slice(before), [["work-items", "get", "--json", "wi_real"]]);
  assert.equal(lookupItemPriority("wi_real", environment({ FAKE_TT_PRIORITY: "" })), undefined);
  assert.throws(() => lookupItemPriority("wi_gone", environment({ FAKE_TT_MODE: "fail" })), /^Error: the lookup failed: work item not found$/);
  assert.throws(() => lookupItemPriority("wi_real", environment({ FAKE_TT_MODE: "garbage" })), /^Error: the lookup returned no JSON$/);
  assert.throws(
    () => lookupItemPriority("wi_real", environment({ PATH: tempDir(t, "matrix-no-tt-") })),
    /^Error: the lookup could not run: /,
  );
  const started = Date.now();
  assert.throws(
    () => lookupItemPriority("wi_real", environment({ FAKE_TT_MODE: "hang" }), 400),
    /^Error: the lookup timed out after 400 ms$/,
  );
  assert(Date.now() - started < 10000, "the hung lookup was cut off");
});

test("V4e a CLI run with no flag joins at its item's priority, recorded as item in the waiter, holder, journal, sidecar and receipt", async (t) => {
  const { f, planFile } = plannedFixture(t, sleepingCheck(600)),
    output = tempDir(t, "verification-host-logs-");
  const holder = await holdHost("wi_v4e_holder");
  const child = matrixCLI(t, f, ["run", planFile, output, "--min-free-bytes", "0", "--item", "wi_v4e"], { FAKE_TT_PRIORITY: "urgent" });
  let waiter;
  try {
    await untilHost(() => (waiter = readHostState(hostLockFile()).waiters[0]), "the waiter; saw " + child.text);
    assert.deepEqual(
      [waiter.item, waiter.priority, waiter.prioritySource, waiter.pid],
      ["wi_v4e", "urgent", "item", child.pid],
    );
  } finally {
    await holder.release();
  }
  let held;
  await untilHost(() => (held = readHostState(hostLockFile()).holder)?.id === waiter.id, "the run holding the host; saw " + child.text);
  assert.deepEqual([held.priority, held.prioritySource], ["urgent", "item"]);
  const [code] = await once(child, "close");
  assert.equal(code, 0, child.text);
  assert.doesNotMatch(child.text, /joining at normal priority/);
  const request = readJournal(hostLockFile()).find((entry) => entry.id === waiter.id && entry.event === "request");
  assert.deepEqual([request.item, request.priority, request.prioritySource], ["wi_v4e", "urgent", "item"]);
  const sidecar = JSON.parse(readFileSync(join(output, "host-lock.json"), "utf8"));
  assert.deepEqual([sidecar.priority, sidecar.prioritySource], ["urgent", "item"]);
  const e = JSON.parse(readFileSync(join(output, "receipt.json"), "utf8")).environment;
  assert.deepEqual([e.VERIFICATION_HOST_PRIORITY, e.VERIFICATION_HOST_PRIORITY_SOURCE], ["urgent", "item"]);
});

test("V4f CLI runs map each item priority, keep the flag and environment first without a lookup, and fall back with one warning", (t) => {
  const { f, plan, planFile } = plannedFixture(t, "console.log('ran');");
  const itemPlanFile = join(tempDir(t, "verification-host-plan-"), "plan.json");
  writeFileSync(itemPlanFile, JSON.stringify({ ...plan, itemId: "wi_from_plan" }));
  const run = (flags, environment = {}, file = planFile) => {
    const output = tempDir(t, "verification-host-logs-");
    const before = fakeTTLookups().length;
    const result = spawnSync(process.execPath, [matrixScript, "run", file, output, "--min-free-bytes", "0", ...flags], {
      cwd: f.cwd,
      env: { ...process.env, ...environment },
      encoding: "utf8",
    });
    assert.equal(result.status, 0, result.stderr);
    const e = JSON.parse(readFileSync(join(output, "receipt.json"), "utf8")).environment;
    return {
      recorded: [e.VERIFICATION_HOST_PRIORITY, e.VERIFICATION_HOST_PRIORITY_SOURCE],
      lookups: fakeTTLookups().slice(before),
      warnings: result.stderr.split("\n").filter((line) => line.includes("joining at normal priority")),
    };
  };
  const lookup = [["work-items", "get", "--json", "wi_cli"]];
  for (const [priority, joined] of [["urgent", "urgent"], ["high", "high"], ["normal", "normal"], ["low", "normal"]])
    assert.deepEqual(
      run(["--item", "wi_cli"], { FAKE_TT_PRIORITY: priority }),
      { recorded: [joined, "item"], lookups: lookup, warnings: [] },
      priority,
    );
  assert.deepEqual(
    run(["--item", "wi_cli"], { FAKE_TT_PRIORITY: "high" }, itemPlanFile),
    { recorded: ["high", "item"], lookups: [["work-items", "get", "--json", "wi_from_plan"]], warnings: [] },
    "the plan's item is the one looked up",
  );
  assert.deepEqual(
    run(["--item", "wi_cli", "--priority", "high"], { FAKE_TT_PRIORITY: "urgent" }),
    { recorded: ["high", "flag"], lookups: [], warnings: [] },
  );
  assert.deepEqual(
    run(["--item", "wi_cli"], { FAKE_TT_PRIORITY: "urgent", TAILTERM_MATRIX_PRIORITY: "normal" }),
    { recorded: ["normal", "environment"], lookups: [], warnings: [] },
  );
  for (const [environment, reason] of [
    [{ FAKE_TT_MODE: "fail" }, "work item wi_cli: the lookup failed: work item not found"],
    [{ FAKE_TT_MODE: "garbage" }, "work item wi_cli: the lookup returned no JSON"],
    [{ FAKE_TT_PRIORITY: "critical" }, 'work item wi_cli has the unrecognised priority "critical"'],
    [{ FAKE_TT_PRIORITY: "" }, "work item wi_cli has no priority"],
  ])
    assert.deepEqual(
      run(["--item", "wi_cli"], environment),
      { recorded: ["normal", "default"], lookups: lookup, warnings: ["matrix host: joining at normal priority (default): " + reason] },
      reason,
    );
  assert.deepEqual(
    run([]),
    { recorded: ["normal", "default"], lookups: [], warnings: ["matrix host: joining at normal priority (default): the run names no work item"] },
  );
});

test("V5 a check sees none of the host lock keys in its environment", async (t) => {
  const f = fixture(t),
    plan = commandPlan(f, "console.log('ENV '+JSON.stringify(Object.keys(process.env)));"),
    output = tempDir(t, "verification-host-logs-");
  const receipt = await runPlan(plan, f.cwd, output, { minFreeBytes: 0, hostLock: { priority: "high", prioritySource: "flag" } });
  assert.equal(receipt.environment.VERIFICATION_HOST_PRIORITY, "high");
  const seen = JSON.parse(
    readFileSync(receipt.checks[0].logURI, "utf8").split("\n").find((line) => line.startsWith("ENV ")).slice(4),
  );
  assert(seen.includes("HOME") && seen.includes("VERIFICATION_JOBS"), "the check printed its environment");
  for (const key of [...HOST_KEYS, "TAILTERM_MATRIX_HOST_LOCK", "TAILTERM_MATRIX_PRIORITY"])
    assert(!seen.includes(key), key + " reached a check");
});

test("V6 receipts record position, grants and overtakes after an urgent run goes ahead of a normal one", async (t) => {
  const { f, planFile } = plannedFixture(t, sleepingCheck(400));
  const outputA = tempDir(t, "verification-host-a-"),
    outputB = tempDir(t, "verification-host-b-");
  const holder = await holdHost("wi_v6_holder");
  const waiting = () => readHostState(hostLockFile()).waiters.map((w) => w.pid);
  const a = matrixCLI(t, f, ["run", planFile, outputA, "--min-free-bytes", "0"]);
  await untilHost(() => waiting().includes(a.pid), "A to queue");
  const b = matrixCLI(t, f, ["run", planFile, outputB, "--min-free-bytes", "0", "--priority", "urgent"]);
  await untilHost(() => waiting().length === 2, "B to queue");
  assert.deepEqual(waiting(), [b.pid, a.pid], "the urgent run is listed first");
  await untilHost(() => a.text.includes("matrix host: waiting 2 of 2, holder wi_v6_holder/"), "A to print its new position");
  await holder.release();
  const [[codeA], [codeB]] = await Promise.all([once(a, "close"), once(b, "close")]);
  assert.equal(codeA, 0, a.text);
  assert.equal(codeB, 0, b.text);
  const environment = (output) => JSON.parse(readFileSync(join(output, "receipt.json"), "utf8")).environment;
  const ea = environment(outputA),
    eb = environment(outputB);
  const values = (e) => [
    e.VERIFICATION_HOST_PRIORITY,
    e.VERIFICATION_HOST_QUEUE_POSITION,
    e.VERIFICATION_HOST_QUEUE_LENGTH,
    e.VERIFICATION_HOST_GRANTS_BEFORE_START,
    e.VERIFICATION_HOST_OVERTAKEN_BY,
    e.VERIFICATION_HOST_OVERLAP,
  ];
  assert.deepEqual(values(eb), ["urgent", "1", "2", "0", "0", "0"]);
  assert.deepEqual(values(ea), ["normal", "1", "1", "1", "1", "0"]);
  assert(Number(eb.VERIFICATION_RUN_DURATION_MS) >= 400);
  assert(
    Number(ea.VERIFICATION_HOST_WAIT_MS) >= Number(eb.VERIFICATION_RUN_DURATION_MS),
    `A waited ${ea.VERIFICATION_HOST_WAIT_MS} ms, at least B's ${eb.VERIFICATION_RUN_DURATION_MS} ms`,
  );

  const go = { id: "go-test", cwd: "hub", environment: {} },
    browser = { id: "tests/a-browser.mjs:chromium", cwd: ".", environment: { TEST_BROWSER: "chromium" } },
    unit = { id: "npm-unit", cwd: ".", environment: {} };
  assert.equal(matrixRunner.checkSet([go, { ...go, id: "go-vet" }]), "go-only");
  assert.equal(matrixRunner.checkSet([go, browser, unit]), "full");
  assert.equal(matrixRunner.checkSet([browser, unit]), "browser");
  assert.equal(matrixRunner.checkSet([{ ...unit, id: "tests/static-browser.mjs" }]), "browser");
  assert.equal(matrixRunner.checkSet([unit]), "unit");
  assert.equal(matrixRunner.checkSet([go, unit]), "unit");
});

// Budget warnings (wi_4828c153bd8599f7). Durations are written into fake check
// results; nothing here waits for a check to be slow.
const budgetCheck = (id, timeoutMs, attempts) => ({
  id,
  environment: { VERIFICATION_TIMEOUT_MS: String(timeoutMs) },
  ...attempts.at(-1),
  attempts: attempts.map((a, i) => ({ attempt: i + 1, ...a })),
});
const passed = (durationMs) => ({ exitCode: 0, durationMs }),
  failed = (durationMs, extra = {}) => ({ exitCode: 1, durationMs, ...extra });

test("budget warning: a passing attempt strictly above 70 percent of the timeout warns, exactly 70 percent and below do not", () => {
  const { budgetWarnings, BUDGET_WARNING_PERCENT } = matrixRunner;
  assert.equal(BUDGET_WARNING_PERCENT, 70);
  assert.deepEqual(
    budgetWarnings([
      budgetCheck("above", 1000, [passed(701)]),
      budgetCheck("exact", 1000, [passed(700)]),
      budgetCheck("below", 1000, [passed(699)]),
      budgetCheck("npm-unit", 480000, [passed(477000)]),
      budgetCheck("exact-large", 480000, [passed(336000)]),
      budgetCheck("just-above-large", 480000, [passed(336001)]),
    ]),
    [
      { check: "above", durationMs: 701, timeoutMs: 1000, percent: 70.1 },
      { check: "npm-unit", durationMs: 477000, timeoutMs: 480000, percent: 99.38 },
      { check: "just-above-large", durationMs: 336001, timeoutMs: 480000, percent: 70 },
    ],
  );
  // A plan with no retry policy records the one attempt on the check itself.
  assert.deepEqual(
    budgetWarnings([
      { id: "plain", environment: { VERIFICATION_TIMEOUT_MS: "1000" }, ...passed(900) },
      { id: "plain-failed", environment: { VERIFICATION_TIMEOUT_MS: "1000" }, ...failed(900) },
    ]),
    [{ check: "plain", durationMs: 900, timeoutMs: 1000, percent: 90 }],
  );
});

test("budget warning: each check is measured against its planned timeout, its own checkTimeoutMs or else the default", () => {
  const { budgetWarnings } = matrixRunner;
  const planned = selectChecks(
    { ...matrix, defaultTimeoutMs: 600000, checkTimeoutMs: { "npm-unit": 480000 } },
    ["client/app.js"],
    [],
  );
  const unit = planned.find((c) => c.id === "npm-unit"),
    other = planned.find((c) => c.id !== "npm-unit" && c.environment.VERIFICATION_TIMEOUT_MS === "600000");
  assert.equal(unit.environment.VERIFICATION_TIMEOUT_MS, "480000");
  assert(other, "a check with no checkTimeoutMs entry takes the default");
  // 400 s is above 70 percent of 480 s (336 s) and below 70 percent of 600 s (420 s).
  const at = (durationMs) =>
    budgetWarnings([unit, other].map((c) => ({ ...c, ...passed(durationMs), attempts: [{ attempt: 1, ...passed(durationMs) }] })));
  assert.deepEqual(at(400000), [
    { check: "npm-unit", durationMs: 400000, timeoutMs: 480000, percent: 83.33 },
  ]);
  assert.deepEqual(at(430000).map((w) => [w.check, w.timeoutMs]), [
    ["npm-unit", 480000],
    [other.id, 600000],
  ]);
  assert.deepEqual(at(336000), []);
});

test("budget warning: only passing attempts count, so a slow failure before a faster pass and a check that never passes give none", () => {
  const { budgetWarnings } = matrixRunner;
  assert.deepEqual(
    budgetWarnings([
      budgetCheck("slow-failure-then-pass", 1000, [failed(990), passed(500)]),
      budgetCheck("timed-out-then-pass", 1000, [
        { exitCode: -1, durationMs: 1001, failureReason: "timeout" },
        passed(700),
      ]),
      budgetCheck("never-passes", 1000, [failed(999), failed(999), failed(999)]),
      budgetCheck("only-timeouts", 1000, [{ exitCode: -1, durationMs: 1001, failureReason: "timeout" }]),
    ]),
    [],
  );
  assert.deepEqual(
    budgetWarnings([budgetCheck("fast-failure-then-slow-pass", 1000, [failed(100), passed(800)])]),
    [{ check: "fast-failure-then-slow-pass", durationMs: 800, timeoutMs: 1000, percent: 80 }],
  );
  // The slowest passing attempt decides, wherever it is in the list.
  assert.deepEqual(
    budgetWarnings([budgetCheck("two-passes", 1000, [passed(750), passed(200)])]),
    [{ check: "two-passes", durationMs: 750, timeoutMs: 1000, percent: 75 }],
  );
  assert.deepEqual(budgetWarnings([]), []);
});

test("budget warning: the report writes budget-warnings.json and prints one line per warned check, and an empty list with no line", (t) => {
  const { reportBudgetWarnings } = matrixRunner;
  const output = tempDir(t, "verification-budget-"),
    file = join(output, "budget-warnings.json"),
    lines = [];
  const checks = [
    budgetCheck("npm-unit", 480000, [passed(477000)]),
    budgetCheck("go-vet", 600000, [passed(1000)]),
    budgetCheck("go-test", 1200000, [failed(1190000), passed(900000)]),
  ];
  const before = JSON.stringify(checks);
  const expected = [
    { check: "npm-unit", durationMs: 477000, timeoutMs: 480000, percent: 99.38 },
    { check: "go-test", durationMs: 900000, timeoutMs: 1200000, percent: 75 },
  ];
  assert.deepEqual(reportBudgetWarnings(checks, output, (line) => lines.push(line)), expected);
  assert.equal(JSON.stringify(checks), before, "the check results are only read");
  assert.equal(readFileSync(file, "utf8"), JSON.stringify(expected, null, 2) + "\n");
  assert.deepEqual(lines, [
    "matrix run: budget warning: npm-unit passed in 477000 ms, 99.38% of its 480000 ms timeout",
    "matrix run: budget warning: go-test passed in 900000 ms, 75% of its 1200000 ms timeout",
  ]);
  assert.deepEqual(readdirSync(output), ["budget-warnings.json"], "no history or cross-run file");

  // A later run with nothing to warn about replaces the list with an empty one.
  lines.length = 0;
  assert.deepEqual(
    reportBudgetWarnings([budgetCheck("npm-unit", 480000, [failed(479000)])], output, (line) => lines.push(line)),
    [],
  );
  assert.equal(readFileSync(file, "utf8"), "[]\n");
  assert.deepEqual(lines, []);

  // An unwritable directory is reported in a line, never thrown; the warning is still printed.
  lines.length = 0;
  assert.deepEqual(
    reportBudgetWarnings([checks[0]], join(output, "missing"), (line) => lines.push(line)),
    [expected[0]],
  );
  assert.equal(lines.length, 2);
  assert.match(lines[0], /^matrix run: could not write budget-warnings\.json: /);
  assert.match(lines[1], /budget warning: npm-unit /);
});

for (const code of [0, 1])
  test(`budget warning: a real ${code ? "failing" : "passing"} run writes an empty list beside an unchanged receipt`, async (t) => {
    const f = fixture(t),
      plan = commandPlan(f, `process.exit(${code});`),
      output = tempDir(t, "verification-budget-run-"),
      lines = [];
    const receipt = await runPlan(plan, f.cwd, output, {
      minFreeBytes: 0,
      hostLock: { print: (line) => lines.push(line) },
    });
    assert.equal(readFileSync(join(output, "budget-warnings.json"), "utf8"), "[]\n");
    assert.deepEqual(lines.filter((line) => /budget warning/.test(line)), []);
    const raw = readFileSync(join(output, "receipt.json"), "utf8");
    assert.equal(raw, JSON.stringify(receipt, null, 2) + "\n");
    assert(!/budget/i.test(raw.replaceAll(output, "")), "nothing about budgets is in the receipt");
    assert.equal(receipt.checks[0].status, code ? "fail" : "pass");
    assert.equal(receiptEligible(receipt), code === 0);
    const schema = JSON.parse(
      readFileSync(new URL("../verification/receipt.schema.json", import.meta.url)),
    );
    for (const key of Object.keys(receipt)) assert(key in schema.properties, "top-level " + key);
    for (const key of Object.keys(receipt.checks[0]))
      assert(key in schema.properties.checks.items.properties, "check " + key);
  });

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
