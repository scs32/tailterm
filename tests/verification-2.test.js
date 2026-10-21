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
// Named separately so the red tests load on a runner that lacks these exports.
import * as matrixRunner from "../scripts/verify-matrix.mjs";
import {
  readHostState,
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
  goRaceFixture,
  shardPlanFixture,
  bigMax,
  bigMin,
  criticalPathPlan,
  hostLockFile,
  matrixScript,
} from "./verification-fixtures.mjs";

// A stand-in go's answer to the go env -json the binary helper records with
// each build, read from the stand-in's own environment.
const goEnvStandIn =
  `if [ "$1" = env ]; then printf '{"GOVERSION":"go1.99.0","GOROOT":"/stand-in","GOPATH":"%s","GOMODCACHE":"%s",` +
  `"GOCACHE":"%s","GOFLAGS":"%s","GOPROXY":"%s","GOTOOLCHAIN":"%s","GOOS":"js","GOARCH":"wasm"}\\n' ` +
  `"$GOPATH" "$GOMODCACHE" "$GOCACHE" "$GOFLAGS" "$GOPROXY" "$GOTOOLCHAIN"; exit 0; fi\n`;
const goEnvCall = "env -json GOVERSION GOROOT GOPATH GOMODCACHE GOCACHE GOFLAGS GOPROXY GOTOOLCHAIN GOOS GOARCH";

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
    "-shards=./cmd/tt=5",
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
test("wasm build script diff selects the wasm build alongside unit and browser checks", () => {
  const ids = (paths) => selectChecks(matrix, paths, paths).map((c) => c.id);
  const script = ids(["scripts/build-wasm.sh"]);
  assert(script.includes("wasm-test-build"));
  assert(script.includes("npm-unit"));
  for (const suite of matrix.browserSuites)
    assert(script.some((id) => id === suite.file || id.startsWith(suite.file + ":")));
  const other = ids(["scripts/verify-matrix.mjs"]);
  assert(other.includes("npm-unit"));
  assert(!other.includes("wasm-test-build"));
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

test("the runner marks its home before the first go command", { timeout: 120000 }, async (t) => {
  // wi_60dbbbe5b36bf66b m1: a stand-in go first on PATH logs, at every call,
  // the provenance marker it finds in its HOME (the verifier home), then
  // answers go env and builds a small executable, as above.
  const tools = tempDir(t, "verification-home-marker-"),
    goCalls = join(tools, "go-calls.log"),
    output = tempDir(t, "verification-home-marker-logs-");
  writeFileSync(
    join(tools, "go"),
    `#!/bin/sh\nif [ -f "$HOME/tailterm-cache.json" ]; then printf 'marker %s\\n' "$(tr -d '\\n' < "$HOME/tailterm-cache.json")" >> '${goCalls}'; ` +
      `else printf 'no marker\\n' >> '${goCalls}'; fi\n${goEnvStandIn}[ "$1" = build ] || exit 1\nout=\nwhile [ $# -gt 0 ]; do [ "$1" = -o ] && out=$2; shift; done\n` +
      `[ -n "$out" ] || exit 1\nprintf '#!/bin/sh\\nexit 0\\n' > "$out" && chmod +x "$out"\n`,
    { mode: 0o755 },
  );
  const f = browserFixture(t, { "profile-sync-browser.mjs": "import './test-binaries.mjs';" });
  writeFileSync(join(f.cwd, "tests/test-binaries.mjs"), "export {};\n");
  mkdirSync(join(f.cwd, "hub"));
  writeFileSync(join(f.cwd, "hub/go.mod"), "module fixture\n\ngo 1.22\n");
  const commit = commitChange(f, "hub/cmd/tt/main.go", "package main\n\nfunc main() {}\n", "hub tree");
  const plan = makePlan({ baseCommit: commit, commit, owned: ["client/"] }, f.cwd);
  writeFileSync(join(tools, "plan.json"), JSON.stringify(plan));
  writeFileSync(
    join(tools, "run.mjs"),
    `import { mkdtempSync, existsSync, readFileSync, realpathSync } from "node:fs";
import { join } from "node:path";
const [runner, planFile, cwd, output] = process.argv.slice(2);
const { runPlan } = await import(runner);
let home = "", error = "";
try {
  await runPlan(JSON.parse(readFileSync(planFile, "utf8")), cwd, output, {
    minFreeBytes: 0,
    jobs: 1,
    createHome: () => (home = mkdtempSync(join(realpathSync("/tmp"), "tv-"))),
  });
} catch (failure) {
  error = failure.message;
}
console.log(JSON.stringify({ error, home, exists: existsSync(home), pid: process.pid }));
`,
  );
  const child = spawn(
    process.execPath,
    [join(tools, "run.mjs"), new URL("../scripts/verify-matrix.mjs", import.meta.url).href, join(tools, "plan.json"), f.cwd, output],
    {
      cwd: f.cwd,
      env: { ...process.env, PATH: tools + ":" + process.env.PATH, TAILTERM_MATRIX_HOST_LOCK: join(tools, "host.json") },
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
  assert.equal(result.error, "");
  const calls = readFileSync(goCalls, "utf8").split("\n").filter(Boolean);
  assert(calls.length > 0, "the run made no go call");
  for (const call of calls) assert.match(call, /^marker /, "a go command ran before the home was marked");
  const marker = JSON.parse(calls[0].slice("marker ".length));
  assert.equal(marker.version, 1);
  assert.equal(marker.kind, "home");
  assert.equal(marker.producer, "scripts/verify-matrix.mjs@" + commit);
  assert.equal(marker.item, "unknown");
  assert.equal(marker.run, output);
  assert(!Number.isNaN(Date.parse(marker.createdAt)), marker.createdAt);
  assert.deepEqual(marker.owns, ["go-build", "go/pkg/mod"]);
  assert.equal(marker.keepHome, false);
  assert.equal(marker.pid, result.pid);
  assert.equal(marker.output, output);
  assert(!result.exists, "the run removed its marked home");
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
    "-shards=./cmd/tt=5",
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
    fakeCheck("01-static-release-verify"),
  ];
  const { results, starts } = await simulate(checks, 6, (c) =>
    c.id === "early-2" ? 40 : 10,
  );
  assert.deepEqual(overlapViolations(results), []);
  assert.deepEqual(starts.slice(0, 2), ["early-1", "early-2"]);
  assert.equal(starts[2], "00-static-build", "later checks wait for the barrier");
  assert.equal(starts.at(-1), "01-static-release-verify");
  const forged = results.map((r) => ({ ...r }));
  forged[3].startedAt = forged[0].startedAt;
  assert(
    overlapViolations(forged).some((v) => v.reason === "barrier"),
    "scanner detects a later check started past an exclusive barrier",
  );
});
// wi_bc1e7d15925ee533 / order #40151: wasm-test-build rewrites the WASM that
// 00-static-build packs and the browser suites load, so it runs before them.
const isWasmReader = (check) =>
  check.id === "00-static-build" || Boolean(check.environment?.TEST_BROWSER);
test("wasm-test-build leads the plan and finishes before the static build and every browser check", async () => {
  const checks = selectChecks(matrix, ["wasm/"], []);
  assert.equal(checks[0].id, "wasm-test-build");
  assert.equal(matrixRunner.wasmOrderCause(checks), "");
  const rest = checks.slice(1).map((c) => c.id);
  assert.deepEqual(rest, [...rest].sort((a, b) => a.localeCompare(b, "en")));
  const { results } = await simulate(checks, 5, () => 1 + Math.random() * 3);
  assert.deepEqual(overlapViolations(results), []);
  const build = results.find((r) => r.id === "wasm-test-build");
  const readers = results.filter(isWasmReader);
  assert(readers.some((r) => r.id === "00-static-build") && readers.length > 2);
  for (const reader of readers)
    assert(reader.startedAt >= build.endedAt, reader.id + " started before wasm-test-build ended");
  const late = [...checks.slice(1), checks[0]];
  assert.equal(matrixRunner.wasmOrderCause(late), "00-static-build runs before wasm-test-build");
  assert.equal(matrixRunner.wasmOrderCause(checks.slice(1)), "");
  // A plan accepted under the old order (wasm-test-build last) is rebuilt in
  // the new order with every check kept.
  const { checks: rebuilt, preserved } = matrixRunner.keepAcceptedChecks(checks, late);
  assert.deepEqual(rebuilt.map((c) => c.id), checks.map((c) => c.id));
  assert.equal(preserved.kept.length, checks.length);
});
// A browser fixture with wasm/tailserve.wasm ignored, as in a real checkout,
// and a wasm/ rule that selects wasm-test-build. The build stub writes
// "built" to both WASM files and the module list; scripts and suites replace
// the fixture's stubs.
function wasmFixture(t, { suite, scripts = {} }) {
  const f = browserFixture(t, { "wasm-browser.mjs": suite });
  writeFileSync(join(f.cwd, ".gitignore"), "node_modules/\n.build/\nwasm/tailserve.wasm\n");
  f.git("rm", "-q", "--cached", "wasm/tailserve.wasm");
  const m = JSON.parse(readFileSync(join(f.cwd, "verification/matrix.json"), "utf8"));
  m.rules.push({ prefixes: ["wasm/"], groups: ["wasm", "unit", "browser"] });
  writeFileSync(join(f.cwd, "verification/matrix.json"), JSON.stringify(m));
  const pkg = JSON.parse(readFileSync(join(f.cwd, "package.json"), "utf8"));
  Object.assign(pkg.scripts, scripts);
  writeFileSync(join(f.cwd, "package.json"), JSON.stringify(pkg));
  mkdirSync(join(f.cwd, "scripts"));
  writeFileSync(join(f.cwd, "scripts/build-wasm.sh"),
    "printf built > wasm/tailserve.wasm\nprintf built > .build/test.wasm\nprintf built > .build/go-modules.txt\n");
  f.git("add", ".");
  f.git("commit", "-qm", "wasm fixture");
  return { ...f, baseCommit: f.git("rev-parse", "HEAD") };
}
const readsBuilt = (path) =>
  `import { readFileSync } from "node:fs";\n` +
  `if (readFileSync(${JSON.stringify(path)}, "utf8") !== "built") process.exit(1);\n`;
const builtStatic = `node -e "process.exit(require('fs').readFileSync('wasm/tailserve.wasm','utf8')==='built'?0:1)"`;
const wasmEntries = (sha256) => [
  { path: "wasm/tailserve.wasm", sha256 },
  { path: ".build/test.wasm", sha256 },
];
test("browser checks and the static build use this run's WASM and the receipt records it", async (t) => {
  const f = wasmFixture(t, { suite: readsBuilt(".build/test.wasm"), scripts: { "build:static": builtStatic } });
  const commit = commitChange(f, "wasm/x.go", "package wasm\n", "wasm candidate");
  const plan = makePlan({ baseCommit: f.baseCommit, commit, owned: ["wasm/"] }, f.cwd);
  const receipt = await runPlan(plan, f.cwd, tempDir(t, "verification-wasm-"), { minFreeBytes: 0, jobs: 4 });
  assert.equal(receipt.checks[0].id, "wasm-test-build");
  assert(receipt.checks.every((c) => c.status === "pass"), "every check passed");
  assert.deepEqual(overlapViolations(receipt.checks), []);
  const build = receipt.checks[0];
  for (const reader of receipt.checks.filter(isWasmReader)) {
    assert(reader.startedAt >= build.endedAt, reader.id + " started before wasm-test-build ended");
    assert.deepEqual(reader.wasm, wasmEntries(digest("built")), reader.id);
  }
  assert.deepEqual(receipt.checks.filter(isWasmReader).map((c) => c.id),
    ["00-static-build", "tests/wasm-browser.mjs"]);
  for (const check of receipt.checks.filter((c) => !isWasmReader(c)))
    assert(!("wasm" in check), check.id + " records no WASM");
  // wi_fcc0892578a426b9 / order #40565: the module list the build rewrote
  // is recorded like the WASM it wrote.
  const rebuilt = ["wasm/tailserve.wasm", ".build/test.wasm", ".build/go-modules.txt"];
  for (const entry of receipt.prerequisites)
    assert.deepEqual(entry, rebuilt.includes(entry.path)
      ? { path: entry.path, sha256: digest("built"), priorSha256: digest("fixture"), source: "wasm-test-build" }
      : { path: entry.path, sha256: entry.sha256 });
  assert.equal(receipt.prerequisites.filter((p) => p.source).length, 3);

  // A suite that rewrites the built WASM fails the run with no receipt.
  const g = wasmFixture(t, {
    suite: `import { writeFileSync } from "node:fs";\nwriteFileSync(".build/test.wasm", "rewritten");\n`,
  });
  const changed = commitChange(g, "wasm/x.go", "package wasm\n", "wasm candidate");
  const output = tempDir(t, "verification-wasm-changed-");
  await assert.rejects(
    runPlan(makePlan({ baseCommit: g.baseCommit, commit: changed, owned: ["wasm/"] }, g.cwd), g.cwd, output,
      { minFreeBytes: 0, jobs: 4 }),
    /WASM changed after wasm-test-build: \.build\/test\.wasm/,
  );
  assert(!existsSync(join(output, "receipt.json")));

  // So does a suite that rewrites the module list after the build.
  const m = wasmFixture(t, {
    suite: `import { writeFileSync } from "node:fs";\nwriteFileSync(".build/go-modules.txt", "rewritten");\n`,
  });
  const modules = commitChange(m, "wasm/x.go", "package wasm\n", "wasm candidate");
  const modulesOutput = tempDir(t, "verification-wasm-modules-");
  await assert.rejects(
    runPlan(makePlan({ baseCommit: m.baseCommit, commit: modules, owned: ["wasm/"] }, m.cwd), m.cwd, modulesOutput,
      { minFreeBytes: 0, jobs: 4 }),
    /WASM changed after wasm-test-build: \.build\/go-modules\.txt/,
  );
  assert(!existsSync(join(modulesOutput, "receipt.json")));

  // A passing build that leaves no test.wasm fails before any reader runs.
  const h = wasmFixture(t, { suite: readsBuilt(".build/test.wasm") });
  writeFileSync(join(h.cwd, "scripts/build-wasm.sh"), "printf built > wasm/tailserve.wasm\nrm .build/test.wasm\n");
  h.git("commit", "-qam", "build writes no test.wasm");
  const missing = commitChange(h, "wasm/x.go", "package wasm\n", "wasm candidate");
  const missingOutput = tempDir(t, "verification-wasm-missing-");
  await assert.rejects(
    runPlan(makePlan({ baseCommit: h.git("rev-parse", "HEAD~1"), commit: missing, owned: ["wasm/"] }, h.cwd), h.cwd,
      missingOutput, { minFreeBytes: 0, jobs: 4 }),
    /wasm-test-build passed without writing \.build\/test\.wasm/,
  );
  assert(!existsSync(join(missingOutput, "receipt.json")));
});
// wi_fcc0892578a426b9 / order #40565: a failed build leaves the readers on
// the previous WASM with nothing to compare, so it can never be excused.
test("wasm-test-build cannot be a known failure and a failed build leaves no eligible receipt", async (t) => {
  const bug = { bugTaskId: "tsk_aaaaaaaaaaaaaaaa", bugId: "wi_bbbbbbbbbbbbbbbb" };
  const m = { ...matrix, maxAttempts: 3 };
  assert.throws(
    () => matrixPolicy({ ...m, knownFailures: [{ checkId: "wasm-test-build", ...bug }] }, []),
    /^Error: wasm-test-build cannot be a known failure: browser readers would run on stale WASM$/,
  );
  const npmUnit = { checkId: "npm-unit", ...bug };
  assert.deepEqual(matrixPolicy({ ...m, knownFailures: [npmUnit] }, [{ id: "npm-unit" }]),
    { maxAttempts: 3, knownFailures: [npmUnit] });

  const f = wasmFixture(t, { suite: "console.log('browser ran');\n" });
  writeFileSync(join(f.cwd, "scripts/build-wasm.sh"), "exit 1\n");
  f.git("commit", "-qam", "build fails");
  const commit = commitChange(f, "wasm/x.go", "package wasm\n", "wasm candidate");
  const plan = makePlan({ baseCommit: f.git("rev-parse", "HEAD~1"), commit, owned: ["wasm/"] }, f.cwd);
  assert.equal(plan.knownFailures, undefined);
  const receipt = await runPlan(plan, f.cwd, tempDir(t, "verification-wasm-failed-"), { minFreeBytes: 0, jobs: 4 });
  const build = receipt.checks.find((c) => c.id === "wasm-test-build");
  assert.equal(build.status, "fail");
  assert(!("knownFailure" in build));
  assert.equal(receiptEligible(receipt), false);
  assert(receipt.prerequisites.every((p) => !p.source), "nothing is recorded as rebuilt");
});
test("a plan without wasm-test-build keeps id order and records the prerequisite WASM each reader loaded", async (t) => {
  const f = wasmFixture(t, { suite: "console.log('browser ran');\n" });
  const commit = commitChange(f, "client/app.js", "export {}\n", "client candidate");
  const plan = makePlan({ baseCommit: f.baseCommit, commit, owned: ["client/"] }, f.cwd);
  const ids = plan.checks.map((c) => c.id);
  assert(!ids.includes("wasm-test-build"));
  assert.deepEqual(ids, [...ids].sort((a, b) => a.localeCompare(b, "en")));
  matrixRunner.recordWasmSources(f.cwd, commit);
  const prerequisites = matrixRunner.readPrerequisites(plan.checks, f.cwd);
  const receipt = await runPlan(plan, f.cwd, tempDir(t, "verification-no-wasm-"), { minFreeBytes: 0, jobs: 4 });
  assert(receipt.checks.every((c) => c.status === "pass"), "every check passed");
  assert.deepEqual(receipt.prerequisites, prerequisites);
  assert.deepEqual(prerequisites.filter((p) => p.path.endsWith(".wasm")), wasmEntries(digest("fixture")));
  assert.deepEqual(receipt.prerequisites.find((p) => p.path === ".build/go-modules.txt"),
    { path: ".build/go-modules.txt", sha256: digest("fixture") });
  for (const reader of receipt.checks.filter(isWasmReader))
    assert.deepEqual(reader.wasm, wasmEntries(digest("fixture")), reader.id);
  assert.equal(receipt.checks.filter(isWasmReader).length, 2);
  for (const path of ["wasm/tailserve.wasm", ".build/test.wasm"])
    assert.equal(readFileSync(join(f.cwd, path), "utf8"), "fixture");

  // A static build that rewrites a prerequisite refuses the next reader.
  const g = wasmFixture(t, {
    suite: "console.log('browser ran');\n",
    scripts: { "build:static": `node -e "require('fs').writeFileSync('.build/test.wasm','rewritten')"` },
  });
  const changed = commitChange(g, "client/app.js", "export {}\n", "client candidate");
  matrixRunner.recordWasmSources(g.cwd, changed);
  const output = tempDir(t, "verification-no-wasm-changed-");
  await assert.rejects(
    runPlan(makePlan({ baseCommit: g.baseCommit, commit: changed, owned: ["client/"] }, g.cwd), g.cwd, output,
      { minFreeBytes: 0, jobs: 4 }),
    /WASM prerequisite changed during verification: \.build\/test\.wasm/,
  );
  assert(!existsSync(join(output, "receipt.json")));
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
    "-shards=./cmd/tt=5", "-shards=./internal/store=4", "./internal/store",
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

// wi_af64945097c67c68 (order #35357): several race shard packages, each with
// its own K, and one pool for the other packages' job and every shard.
test("goRaceShards accepts several packages, each with its own K", () => {
  const several = { ...matrix, goRaceShards: { "./internal/store": 4, "./internal/api": 2 } };
  const race = (m, owned) => selectChecks(m, owned, []).find((c) => c.id === "go-race").argv;
  const prefix = [
    "node", "../scripts/verify-matrix.mjs", "go-race", "-timeout=45m",
    "-shards=./internal/api=2", "-shards=./internal/store=4",
  ];
  // One flag per package, in package order, on every go-race whatever its packages.
  assert.deepEqual(race(several, ["hub/internal/api/releases.go"]), [...prefix, "./internal/api"]);
  assert.deepEqual(race(several, ["hub/cmd/tt/main.go"]), [...prefix, "./cmd/tt"]);
  assert.deepEqual(race(several, ["hub/go.mod"]), [...prefix, "./..."]);
  // An invalid K in either entry is refused as today.
  for (const k of [1, 9, "2", 2.5, 0, -2, null])
    for (const shards of [
      { "./internal/store": 4, "./internal/api": k },
      { "./internal/store": k, "./internal/api": 2 },
    ])
      assert.throws(
        () => selectChecks({ ...matrix, goRaceShards: shards }, ["hub/cmd/tt/main.go"], []),
        /^Error: Invalid matrix goRaceShards$/,
        JSON.stringify(shards),
      );
  // The spec names the flags whose package the check tests: both for ./...,
  // one for a list naming one, none for neither.
  const check = (...packages) => ({ id: "go-race", argv: [...prefix, ...packages], cwd: "hub", environment: {} });
  const api = { package: "./internal/api", k: 2 },
    store = { package: "./internal/store", k: 4 };
  assert.deepEqual(matrixRunner.goRaceShardSpec(check("./...")), [api, store]);
  assert.deepEqual(matrixRunner.goRaceShardSpec(check("./internal/api", "./internal/store")), [api, store]);
  assert.deepEqual(matrixRunner.goRaceShardSpec(check("./cmd/tt", "./internal/store")), [store]);
  assert.equal(matrixRunner.goRaceShardSpec(check("./cmd/tt")), null);
  // The weight follows the pool: the other packages' job plus every shard, at
  // most the job count, so K 2 and 7 hold 2 slots at 5 jobs and 3 at 8.
  const wide = {
    id: "go-race",
    argv: [...prefix.slice(0, 4), "-shards=./internal/api=2", "-shards=./internal/store=7", "./..."],
    cwd: "hub",
    environment: {},
  };
  assert.equal(matrixRunner.checkWeight(wide, 5), 2);
  assert.equal(matrixRunner.checkWeight(wide, 8), 3);
  assert.equal(matrixRunner.checkWeight(wide, 2), 2, "capped at the job count");
  assert.equal(matrixRunner.checkWeight(check("./cmd/tt"), 8), 2, "a go-race that will not shard keeps weight 2");
});

test("go-race command shards two packages in one pool after listing both", { timeout: 600000 }, (t) => {
  const f = goRaceFixture(t);
  const two = ["-timeout=5m", "-shards=./other=2", "-shards=./store=2", "./..."];
  // TestOther hashes to the catch-all, so ./other's first shard does not start.
  assert.equal(matrixRunner.goRaceShardOf("TestOther", 2), 1);
  const storeParts = matrixRunner.goRacePartition(
    ["TestAlpha", "TestBeta", "TestGamma", "TestDelta", "TestEpsilon", "FuzzSeed", "ExampleHello"],
    2,
  ).parts;
  const storeCommands = [
    `test -race -json -count=1 -timeout=5m -run ^(${storeParts[0].join("|")})$ ./store`,
    `test -race -json -count=1 -timeout=5m -skip ^(${storeParts[0].join("|")})$ ./store`,
  ];
  const queue = [
    "test -race -p=1 -timeout=5m ./internal/fixturelog ./third",
    "test -race -json -count=1 -timeout=5m ./other",
    ...storeCommands,
  ];
  const lists = ["list -f {{.Dir}} ./...", "test -race -list . ./other", "test -race -list . ./store"];
  const lines = (out) => ({
    starts: out.match(/^go-race shards: \.\/\w+ K=2 tests=\d+ partition sha256:[a-f0-9]{64}$/gm) ?? [],
    ends: out.match(/^go-race shards: \.\/\w+ K=2 tests=(\d+) ran-once=\1 partition sha256:[a-f0-9]{64} peak-shard-processes=\d+$/gm) ?? [],
  });

  // One job: every list first, then the queue strictly in order.
  let result = f.run(two, { VERIFICATION_JOBS: "1" });
  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.deepEqual(result.commands, [...lists, ...queue]);
  const at = (pkg) => result.entries.filter((e) => e.pkg === pkg).map((e) => e.at);
  assert(bigMax(at("third")) < bigMin(at("other")), "the other packages' job ran first");
  assert(bigMax(at("other")) < bigMin(at("store")), "./other's shards ran before ./store's");
  assert.deepEqual(result.entries.map((e) => e.pkg + "." + e.name).sort(), [
    "other.TestOther", "third.TestThird",
    ...["ExampleHello", "FuzzSeed", "TestAlpha", "TestBeta", "TestDelta", "TestEpsilon", "TestGamma"].map((name) => "store." + name),
  ].sort());
  let { starts, ends } = lines(result.stdout);
  assert.deepEqual(starts.map((line) => line.split(" ")[2]), ["./other", "./store"]);
  assert.deepEqual(ends.map((line) => line.split(" ")[2]), ["./other", "./store"]);
  assert(ends.every((line) => line.endsWith(" peak-shard-processes=1")), ends.join("\n"));
  const out = result.stdout;
  const index = (text) => {
    assert(out.includes(text), text + "\n" + out);
    return out.indexOf(text);
  };
  assert(index(starts[1]) < index("go-race other packages: 2 packages,"));
  assert.match(out, /^go-race shard \.\/other 1\/2: 0 tests, 0\.0 s, exit 0 \(not started: no test hashes to it\)$/m);
  assert.match(out, /^go-race shard \.\/other 2\/2: 1 tests, [0-9.]+ s, exit 0$/m);
  assert(index("go-race other packages:") < index("go-race shard ./other 1/2:"));
  assert(index("go-race shard ./other 2/2:") < index("go-race shard ./store 1/2:"));
  assert(index("go-race shard ./store 1/2:") < index("go-race shard ./store 2/2:"));
  assert(index("go-race shard ./store 2/2:") < index(ends[0]));

  // Five jobs: the other packages' job and the shards overlap.
  result = f.run(two);
  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.deepEqual(result.commands.slice(0, 3), lists);
  assert.deepEqual(result.commands.slice(3).sort(), [...queue].sort());
  ({ ends } = lines(result.stdout));
  assert.equal(ends.length, 2, result.stdout);
  const peak = Number(/peak-shard-processes=(\d+)$/.exec(ends[0])[1]);
  assert(peak > 1 && peak <= 3, ends[0]);

  // A failing ./other shard on one job: nothing starts after it.
  result = f.run(two, { VERIFICATION_JOBS: "1", FIXTURE_FAIL: "other" });
  assert.equal(result.status, 1, result.stdout + result.stderr);
  assert.deepEqual(result.commands, [...lists, ...queue.slice(0, 2)]);
  assert.deepEqual(result.entries.filter((e) => e.pkg === "store"), []);
  assert.match(result.stdout, /^go-race shard \.\/other 2\/2: 1 tests, [0-9.]+ s, exit 1$/m);
  assert.match(result.stdout, /^go-race: 2 jobs not started after a failure$/m);
  assert(!result.stdout.includes("go-race shard ./store"), result.stdout);
  assert.doesNotMatch(result.stdout + result.stderr, /not run \(first|did not run every test/);

  // A duplicate package flag and an invalid K are usage errors; nothing runs.
  for (const args of [
    ["-timeout=5m", "-shards=./store=2", "-shards=./store=3", "./..."],
    ["-timeout=5m", "-shards=./store=2", "-shards=./store=2", "./store"],
    ["-timeout=5m", "-shards=./other=9", "-shards=./store=2", "./..."],
    ["-timeout=5m", "-shards=./store=2", "-shards=./other=1", "./..."],
    ["-timeout=5m", "-timeout=5m", "-shards=./store=2", "./store"],
  ]) {
    result = f.run(args);
    assert.equal(result.status, 2, args.join(" "));
    assert.match(result.stderr, /^Usage: node \.\.\/scripts\/verify-matrix\.mjs go-race .*\[-shards=\.\/PACKAGE=K\]\.\.\. /);
    assert.deepEqual(result.commands, [], args.join(" "));
  }
});

test("plan creation partitions each shard package and prints one line per package", (t) => {
  const f = shardPlanFixture(t);
  // The two-package matrix is the base, so the diff holds only the tests.
  const base = commitChange(
    f,
    "verification/matrix.json",
    JSON.stringify({
      ...JSON.parse(readFileSync(join(f.cwd, "verification/matrix.json"), "utf8")),
      goRaceShards: { "./internal/store": 2, "./internal/api": 3 },
    }),
    "two shard packages",
  );
  const context = (commit, owned) => ({ ...f.context(commit, owned), baseCommit: base });
  const apiTests = ["TestApiOne", "TestApiTwo", "TestApiThree"];
  const every = commitChange(
    f,
    "hub/internal/api/api_test.go",
    'package api\n\nimport "testing"\n\n' + apiTests.map((name) => `func ${name}(t *testing.T) {}\n`).join(""),
    "api tests",
  );
  const { plan, goRaceShards } = matrixRunner.planWithPreservation(
    context(every, ["hub/internal/api/api_test.go", "hub/internal/store/store.go"]),
    f.cwd,
  );
  assert.deepEqual(plan.checks.find((c) => c.id === "go-race").argv, [
    "node", "../scripts/verify-matrix.mjs", "go-race", "-timeout=5m",
    "-shards=./internal/api=3", "-shards=./internal/store=2", "./internal/api", "./internal/store",
  ]);
  const expected = [
    ["./internal/api", 3, apiTests],
    ["./internal/store", 2, f.names],
  ].map(([pkg, k, names]) => {
    const { digest: partitionDigest } = matrixRunner.goRacePartition(names, k);
    const line = `go-race shards: ${pkg} K=${k} tests=${names.length} partition sha256:${partitionDigest}`;
    return { package: pkg, k, tests: names.length, digest: partitionDigest, line };
  });
  assert.deepEqual(goRaceShards, expected);
  // Only the api package in the list: only its line.
  const apiOnly = matrixRunner.planWithPreservation(context(every, ["hub/internal/api/api_test.go"]), f.cwd);
  assert.deepEqual(apiOnly.goRaceShards, [expected[0]]);
  // Plan mode prints each line on stderr.
  const directory = tempDir(t, "verification-shard-plan-two-");
  writeFileSync(join(directory, "context.json"), JSON.stringify(context(every, ["hub/internal/api/api_test.go", "hub/internal/store/store.go"])));
  const result = spawnSync(
    process.execPath,
    [matrixScript, "plan", join(directory, "context.json"), join(directory, "plan.json")],
    { cwd: f.cwd, encoding: "utf8" },
  );
  assert.equal(result.status, 0, result.stderr);
  assert.equal(result.stderr, expected.map((entry) => entry.line + "\n").join(""));
});
