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
  symlinkSync,
  realpathSync,
} from "node:fs";
import { tmpdir, getPriority } from "node:os";
import { join } from "node:path";
import { execFileSync, spawn, spawnSync } from "node:child_process";
import { prepareTestBinary, sourceIdentity, fileHash } from "./test-binaries.mjs";
import { createServer } from "node:net";
import { createHash, randomUUID } from "node:crypto";
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
  acquireHostLock,
  readHostState as rawReadHostState,
  holdersOf,
  readJournal,
  resolveRunPriority,
  lookupItemPriority,
} from "../scripts/verify-matrix-host-lock.mjs";
// Every run in this file, in process or through the CLI, takes its host lock
// in a private directory and never the host's own lock file.
const hostLockDirectory = mkdtempSync(join(tmpdir(), "matrix-host-lock-"));
process.env.TAILTERM_MATRIX_HOST_LOCK = join(hostLockDirectory, "host.json");
process.env.TAILTERM_MATRIX_MAX_HOLDERS = "1";
// CLI runs in this file stay one process, the run itself, unless a test starts
// the detached launcher on purpose (launcherCLI). No run inherits an agent
// identity, so none reports a withdrawal unless its test supplies one.
process.env.TAILTERM_MATRIX_FOREGROUND = "1";
for (const key of ["TAILTERM_MATRIX_RUN_CHILD", "TAILTERM_MATRIX_RELEASE", "TAILTERM_AGENT", "TAILTERM_AGENT_NAME", "TAILTERM_TASK"])
  delete process.env[key];
const readHostState = (...args) => {
  const state = rawReadHostState(...args);
  const held = holdersOf(state);
  assert(held.length <= 1, "legacy capacity-one fixture has at most one holder");
  return state ? { ...state, holder: held[0] || null } : state;
};
delete process.env.TAILTERM_MATRIX_PRIORITY;
// A stand-in tt comes first on PATH, so an item priority lookup made by any
// run in this file never reaches a hub. FAKE_TT_PRIORITY is the priority it
// reports, FAKE_TT_MODE one of fail, garbage or hang, and it appends each
// call's arguments to fakeTTCalls. With neither set it fails. For a withdrawal
// report it prints FAKE_TT_CONTEXT for context (failing without one) and
// accepts send.
const fakeTTDirectory = mkdtempSync(join(tmpdir(), "matrix-fake-tt-"));
const fakeTTCalls = join(fakeTTDirectory, "calls.jsonl");
writeFileSync(
  join(fakeTTDirectory, "tt"),
  `#!${process.execPath}
const { appendFileSync } = require("node:fs");
appendFileSync(${JSON.stringify(fakeTTCalls)}, JSON.stringify(process.argv.slice(2)) + "\\n");
const mode = process.env.FAKE_TT_MODE, priority = process.env.FAKE_TT_PRIORITY;
const command = process.argv[2], context = process.env.FAKE_TT_CONTEXT;
if (mode === "hang") setInterval(() => {}, 1000);
else if (mode === "garbage") console.log("not json");
else if (mode !== "fail" && command === "context" && context) console.log(context);
else if (mode !== "fail" && command === "context") {
  console.error("no bound work item");
  process.exit(1);
} else if (mode !== "fail" && command === "send") console.log("posted #4242");
else if (mode === "fail" || priority === undefined) {
  console.error("work item not found");
  process.exit(1);
} else console.log(JSON.stringify({ id: process.argv.at(-1), priority: priority || undefined }));
`,
  { mode: 0o755 },
);
process.env.PATH = fakeTTDirectory + ":" + process.env.PATH;
delete process.env.FAKE_TT_MODE;
delete process.env.FAKE_TT_PRIORITY;
delete process.env.FAKE_TT_CONTEXT;
const fakeTTLookups = () =>
  existsSync(fakeTTCalls)
    ? readFileSync(fakeTTCalls, "utf8").trim().split("\n").map((line) => JSON.parse(line))
    : [];
process.on("exit", () => {
  rmSync(hostLockDirectory, { recursive: true, force: true });
  rmSync(fakeTTDirectory, { recursive: true, force: true });
});
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
    cwd: root, encoding: "utf8", timeout: 120000,
    env: { PATH: process.env.PATH, HOME: process.env.HOME, TMPDIR: process.env.TMPDIR,
      TAILTERM_TEST_BINARIES: manifest },
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
  writeFileSync(join(cwd, ".gitignore"), "node_modules/\n");
  mkdirSync(join(cwd, "node_modules"));
  writeFileSync(join(cwd, "node_modules/.package-lock.json"), "{}");
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
// Processes a fixture check leaves behind record their pids outside the
// verifier home, and t.after SIGKILLs every recorded pid that still runs this
// fixture's own child.cjs, so a failing test cannot leak the processes it
// exercises. A recorded pid whose process already exited may by then belong
// to a stranger, which the unique directory in the command line rules out.
function fixturePids(t) {
  const directory = mkdtempSync(join(tmpdir(), "verification-pids-"));
  const owns = (pid) =>
    Number.isInteger(pid) &&
    pid > 0 &&
    (spawnSync("ps", ["-ww", "-o", "command=", "-p", String(pid)], { encoding: "utf8" }).stdout || "")
      .includes(join(directory, "child.cjs"));
  t.after(() => {
    for (const name of readdirSync(directory)) {
      if (name.endsWith(".pid"))
        try {
          const pid = Number(readFileSync(join(directory, name), "utf8"));
          if (owns(pid)) process.kill(pid, "SIGKILL");
        } catch {}
      if (name.endsWith(".path"))
        rmSync(readFileSync(join(directory, name), "utf8"), { recursive: true, force: true });
    }
    rmSync(directory, { recursive: true, force: true });
  });
  // argv: name, optional file to hold open. IGNORE_TERM forces the SIGKILL path.
  writeFileSync(
    join(directory, "child.cjs"),
    `const fs=require('node:fs'),path=require('node:path');const [name,held]=process.argv.slice(2);if(process.env.IGNORE_TERM)process.on('SIGTERM',()=>{});if(held)fs.openSync(held,'w');fs.writeFileSync(path.join(__dirname,name+'.pid'),String(process.pid));setInterval(()=>{},1000);`,
  );
  return directory;
}
// A check that starts setsid'd children (detached, as the codex daemon does)
// and exits once each has recorded its pid. starts is check source calling
// start(name, spawnOptions, heldFile) with pids, home and elsewhere in scope.
function leakingCheck(pids, names, starts) {
  return `import {spawn} from 'node:child_process';import fs from 'node:fs';
const pids=${JSON.stringify(pids)},home=process.env.HOME,elsewhere={...process.env,HOME:pids,TMPDIR:pids,PWD:pids};
const start=(name,options,held='')=>spawn(process.execPath,[pids+'/child.cjs',name,held],{detached:true,stdio:'ignore',...options}).unref();
${starts}
const names=${JSON.stringify(names)},deadline=Date.now()+10000;
while(!names.every((name)=>fs.existsSync(pids+'/'+name+'.pid'))){if(Date.now()>deadline){console.error('children did not start');process.exit(1)}await new Promise((resolve)=>setTimeout(resolve,20))}
console.log('leaked '+names.join(','));`;
}
const pidOf = (pids, name) => Number(readFileSync(join(pids, name + ".pid"), "utf8"));
const alive = (pid) => {
  try {
    process.kill(pid, 0);
    return true;
  } catch (error) {
    return error.code !== "ESRCH";
  }
};
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
    for (const deadline = Date.now() + 10_000; !existsSync(join(pids, "own.pid")); ) {
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
  // tests/test-binaries.mjs captures its build environment when imported, so
  // this process cannot redirect go. The run happens in a child process whose
  // PATH puts a stand-in go first before it imports the runner; no real build
  // runs. The stand-in writes a small executable for build -o and logs each call.
  const tools = tempDir(t, "verification-binary-mutation-"),
    goCalls = join(tools, "go-calls.log"),
    suiteRecord = join(tools, "suite.json"),
    output = tempDir(t, "verification-binary-mutation-logs-");
  writeFileSync(
    join(tools, "go"),
    `#!/bin/sh\nprintf '%s\\n' "$*" >> '${goCalls}'\n[ "$1" = build ] || exit 1\nout=\nwhile [ $# -gt 0 ]; do [ "$1" = -o ] && out=$2; shift; done\n[ -n "$out" ] || exit 1\nprintf '#!/bin/sh\\nexit 0\\n' > "$out" && chmod +x "$out"\n`,
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
    `build -ldflags=-s -w -o ${join(output, "tailterm-hub-test")} ./cmd/tailterm-hub`,
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
  mkdirSync(join(f.cwd, "node_modules"), { recursive: true });
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
// Pids of the live processes whose command line carries token, read in one
// listing. A zombie awaiting its parent is not running and is left out.
const tokenProcesses = (token) =>
  spawnSync("ps", ["-axww", "-o", "pid=,state=,command="], { encoding: "utf8", maxBuffer: 64 * 1024 * 1024 })
    .stdout.split("\n")
    .map((line) => line.match(/^\s*(\d+)\s+(\S+)\s+(.*)$/))
    .filter((match) => match && !match[2].startsWith("Z") && match[3].includes(token))
    .map((match) => Number(match[1]));
// Starts two concurrent check groups through the CLI, interrupts the run once
// both published a pid file and checks what it left. Every process of the run
// carries its token, the base name of a fresh directory: the runner in its
// plan path, each suite in its file name and each suite's TERM-ignoring child
// as an argument. Cleanup signals the runner by its handle and otherwise only
// processes that carry the token, so it stops check groups that never
// published and never signals a pid merely read from a file. withhold names
// suites that start but never publish; run receives the token, the runner and
// each started suite's pids for the caller's own assertions.
async function interruptedConcurrentRun(t, { withhold = [], publishWithinMs = 30_000 } = {}, run = {}) {
  const root = tempDir(t, "verification-concurrent-signal-"),
    token = root.split("/").at(-1),
    names = ["one", "two"];
  const waitFor = async (pending, deadline, [did, todo], runner) => {
    while (pending().length) {
      assert(
        runner.exitCode === null && runner.signalCode === null,
        `the runner exited before these check groups ${did}: ${pending()}`,
      );
      assert(Date.now() < deadline, `timed out waiting for these check groups to ${todo}: ${pending()}`);
      await new Promise((resolve) => setTimeout(resolve, 20));
    }
  };
  // The runner writes an attempt log only when its check ends, so the pid file
  // is the readiness event: each suite first records that it started, then
  // puts its partial line in the runner's pipe with a synchronous write, then
  // publishes the complete pid file by rename.
  const publish = (file) =>
    `fs.writeFileSync(${JSON.stringify(join(root, file + ".tmp"))},JSON.stringify([process.pid,c.pid]));` +
    `fs.renameSync(${JSON.stringify(join(root, file + ".tmp"))},${JSON.stringify(join(root, file))});`;
  const suite = (name) =>
    `import {spawn} from 'node:child_process';import fs from 'node:fs';` +
    `const c=spawn(process.execPath,['-e','process.on("SIGTERM",()=>{});setInterval(()=>{},1000)',${JSON.stringify(token)}],{stdio:'ignore'});` +
    publish(name + ".started") +
    `fs.writeSync(1,'partial ${name}\\n');` +
    (withhold.includes(name) ? "" : publish(name)) +
    `setInterval(()=>{},1000);`;
  const f = browserFixture(
    t,
    Object.fromEntries(names.map((name) => [`${name}-${token}-browser.mjs`, suite(name)])),
  );
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
  const closed = once(runner, "close");
  Object.assign(run, { token, runner, started: [] });
  const pids = [];
  try {
    // Both suites are running before any publication deadline applies.
    await waitFor(
      () => names.filter((name) => !existsSync(join(root, name + ".started"))),
      Date.now() + 30_000,
      ["started", "start"],
      runner,
    );
    for (const name of names)
      run.started.push(...JSON.parse(readFileSync(join(root, name + ".started"), "utf8")));
    await waitFor(
      () => names.filter((name) => !existsSync(join(root, name))),
      Date.now() + publishWithinMs,
      ["published a pid file", "publish a pid file"],
      runner,
    );
    for (const name of names)
      pids.push(...JSON.parse(readFileSync(join(root, name), "utf8")));
    runner.kill("SIGINT");
    const [code] = await closed;
    assert.equal(code, 130);
    const alive = (pid) => {
      try {
        process.kill(pid, 0);
        return true;
      } catch {
        return false;
      }
    };
    for (const deadline = Date.now() + 10_000; pids.some(alive) && Date.now() < deadline; )
      await new Promise((resolve) => setTimeout(resolve, 20));
    assert.deepEqual(pids.filter(alive), [], "every check group was stopped");
    assert(!existsSync(join(output, "receipt.json")));
    for (const name of names) {
      const log = readFileSync(
        join(output, digest(`tests/${name}-${token}-browser.mjs`) + ".attempt-1.log"),
        "utf8",
      );
      assert.match(log, new RegExp("partial " + name));
      assert.match(log, /failureReason: interrupted/);
    }
  } finally {
    const running = () => runner.exitCode === null && runner.signalCode === null;
    // The runner stops its own check groups on SIGINT; SIGKILL would orphan them.
    if (running()) {
      runner.kill("SIGINT");
      await Promise.race([closed, new Promise((resolve) => setTimeout(resolve, 15_000))]);
    }
    if (running()) {
      runner.kill("SIGKILL");
      await closed;
    }
    let left = tokenProcesses(token);
    for (const pid of left)
      try {
        process.kill(pid, "SIGKILL");
      } catch {}
    for (const deadline = Date.now() + 5_000; left.length && Date.now() < deadline; ) {
      await new Promise((resolve) => setTimeout(resolve, 20));
      left = tokenProcesses(token);
    }
    assert.deepEqual(left, [], "a process of this run outlived its cleanup");
  }
}
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
    /does not contain previous candidate/,
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

// Host lock and waitlist (wi_6b4af2fb034ec36e, order #18569). V1-V6 of the plan.
const HOST_KEYS = [
  "VERIFICATION_HOST_LOCK",
  "VERIFICATION_HOST_PRIORITY",
  "VERIFICATION_HOST_PRIORITY_SOURCE",
  "VERIFICATION_HOST_WAIT_MS",
  "VERIFICATION_HOST_QUEUE_POSITION",
  "VERIFICATION_HOST_QUEUE_LENGTH",
  "VERIFICATION_HOST_GRANTS_BEFORE_START",
  "VERIFICATION_HOST_OVERTAKEN_BY",
  "VERIFICATION_HOST_OVERLAP",
  "VERIFICATION_CHECK_SET",
  "VERIFICATION_RUN_DURATION_MS",
  "VERIFICATION_HOST_LOAD",
];
const hostLockFile = () => process.env.TAILTERM_MATRIX_HOST_LOCK;
const matrixScript = new URL("../scripts/verify-matrix.mjs", import.meta.url).pathname;
const sleepingCheck = (ms) => `await new Promise((resolve)=>setTimeout(resolve,${ms}));`;
async function untilHost(condition, what) {
  const deadline = Date.now() + 20000;
  for (;;) {
    const value = condition();
    if (value) return value;
    if (Date.now() > deadline) assert.fail("timed out waiting for " + what);
    await new Promise((resolve) => setTimeout(resolve, 10));
  }
}
function matrixCLI(t, f, args, environment = {}) {
  const child = spawn(process.execPath, [matrixScript, ...args], {
    cwd: f.cwd,
    env: { ...process.env, ...environment },
    stdio: ["ignore", "pipe", "pipe"],
  });
  child.text = "";
  child.stdout.on("data", (data) => (child.text += data));
  child.stderr.on("data", (data) => (child.text += data));
  t.after(() => {
    if (child.exitCode === null && child.signalCode === null) child.kill("SIGKILL");
  });
  return child;
}
function plannedFixture(t, source) {
  const f = fixture(t),
    plan = commandPlan(f, source),
    planFile = join(tempDir(t, "verification-host-plan-"), "plan.json");
  writeFileSync(planFile, JSON.stringify(plan));
  return { f, plan, planFile };
}
const holdHost = (item) =>
  acquireHostLock({ item, agent: "holder", runTimeoutMs: 60000, pollMs: 20 });

// wi_789dc79d7eff49bc / order21693: refusal must precede host admission.
const prerequisitePaths = [
  "node_modules/.package-lock.json", "wasm/tailserve.wasm", ".build/test.wasm",
  ".build/speech-fixture.wav", ".build/go-modules.txt",
];
function preflightFixture(t, browser = false) {
  const f = browser
    ? browserFixture(t, { "fixture-browser.mjs": "console.log('browser ran');" })
    : fixture(t);
  // All prerequisites are ignored, just as on real detached worktrees. Their
  // removal must exercise the input gate rather than clean-candidate rejection.
  if (browser) {
    writeFileSync(join(f.cwd, ".gitignore"), "node_modules/\n.build/\nwasm/tailserve.wasm\n");
    f.git("rm", "--cached", "wasm/tailserve.wasm");
    f.git("add", ".gitignore");
    f.git("commit", "-qm", "ignore generated wasm");
  }
  const baseCommit = f.git("rev-parse", "HEAD");
  const prefix = browser ? "client/" : "docs/";
  const commit = commitChange(f, prefix + "preflight.txt", "fix\n", "preflight candidate");
  const context = { baseCommit, commit };
  return { ...f, context, plan: makePlan({ ...context, owned: [prefix] }, f.cwd) };
}
const preflightRun = (mode, f, output, options) => mode === "run"
  ? runPlan(f.plan, f.cwd, output, options)
  : runTargeted(f.context, f.cwd, output, options);

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

// A docs-only item under hub/cmd/tt changes no Go file, so its accepted plan
// runs go-race on ./...; by integration other items' changes sit between the
// accepted base and the integrated commit.
const integratedKnownFailure = {
  checkId: "npm-unit",
  bugTaskId: "tsk_0123456789abcdef",
  bugId: "wi_0123456789abcdef",
};
function integratedFixture(t, itemPath = "hub/cmd/tt/testdata/README.md") {
  const f = fixture(t);
  writeFileSync(
    join(f.cwd, "verification/matrix.json"),
    JSON.stringify({
      maxAttempts: 3,
      knownFailures: [integratedKnownFailure],
      version: 1,
      browserSuites: [],
      excludedBrowserSuites: [],
      rules: [
        { prefixes: ["hub/"], groups: ["go"] },
        { prefixes: ["docs/"], groups: ["unit"] },
      ],
    }),
  );
  for (const p of ["cmd/tt/testdata", "internal/store", "internal/api"])
    mkdirSync(join(f.cwd, "hub", p), { recursive: true });
  writeFileSync(join(f.cwd, "hub/go.mod"), "module fixture\n\ngo 1.24.0\n");
  writeFileSync(join(f.cwd, "hub/cmd/tt/main.go"), "package main\n\nfunc main() {}\n");
  writeFileSync(join(f.cwd, "hub/cmd/tt/testdata/README.md"), "old\n");
  writeFileSync(join(f.cwd, "hub/internal/store/store.go"), "package store\n");
  writeFileSync(join(f.cwd, "hub/internal/api/api.go"), "package api\n");
  f.git("add", ".");
  f.git("commit", "-qm", "base packages");
  const base = f.git("rev-parse", "HEAD");
  writeFileSync(join(f.cwd, itemPath), "// the item's change\n" + readFileSync(join(f.cwd, itemPath), "utf8"));
  f.git("commit", "-qam", "the item");
  const context = {
    operationKey: "fixture",
    repository: "fixture",
    baseCommit: base,
    commit: f.git("rev-parse", "HEAD"),
    owned: [itemPath],
    verifierAgentId: "fixture",
    verifierRunId: "fixture",
  };
  const accepted = makePlan(context, f.cwd);
  // What the release runner builds: the accepted plan with the integrated
  // commit, here after other items changed the given paths.
  const integrate = (paths = ["hub/internal/store/store.go"], plan = accepted) => {
    for (const p of paths) writeFileSync(join(f.cwd, p), "package store\n\nvar Other = 1\n");
    f.git("add", ".");
    f.git("commit", "-qm", "other items");
    return { ...plan, commit: f.git("rev-parse", "HEAD") };
  };
  return { ...f, base, context, accepted, integrate };
}
const raceArgv = (plan) => plan.checks.find((c) => c.id === "go-race").argv;
// An accepted plan whose checks were changed and whose digest still binds them.
const withChecks = (plan, change) => {
  const checks = structuredClone(plan.checks);
  return { ...plan, checks: change(checks) ?? checks, checksDigest: undefined };
};

test("integrated plan of a docs-only item under hub/cmd/tt keeps the accepted go-race on every package", (t) => {
  const f = integratedFixture(t);
  assert.deepEqual(raceArgv(f.accepted), ["go", "test", "-race", "./..."]);
  const context = f.integrate();
  // Selected from the integrated diff alone, go-race names only the package
  // another item changed: narrower than the accepted check.
  const { checks, checksDigest, ...withoutAccepted } = context;
  assert.deepEqual(raceArgv(makePlan(withoutAccepted, f.cwd)), [
    "go", "test", "-race", "./internal/store",
  ]);
  const integrated = makePlan(context, f.cwd);
  assert.deepEqual(raceArgv(integrated), ["go", "test", "-race", "./..."]);
  assert.deepEqual(integrated.checks, f.accepted.checks);
  assert.equal(integrated.checksDigest, f.accepted.checksDigest);
  assert.deepEqual(integrated.changed, [
    "hub/cmd/tt/testdata/README.md",
    "hub/internal/store/store.go",
  ]);
  // The hub binds a receipt to the plan fields it knows: no new plan field.
  assert.deepEqual(Object.keys(integrated).sort(), Object.keys(f.accepted).sort());
  const { preserved } = matrixRunner.planWithPreservation(context, f.cwd);
  assert.deepEqual(preserved, {
    version: 1,
    acceptedChecksDigest: f.accepted.checksDigest,
    kept: ["go-race", "go-test", "go-vet"],
    widened: [],
    rebuilt: [],
    added: [],
    narrowerSelection: ["go-race"],
    checksDigest: integrated.checksDigest,
  });
});

test("integrated plan keeps every accepted check, adds newly selected ones and unions go-race packages", (t) => {
  const f = integratedFixture(t, "hub/internal/api/api.go");
  assert.deepEqual(raceArgv(f.accepted), ["go", "test", "-race", "./internal/api"]);
  assert.equal(f.accepted.knownFailures, undefined);
  // The accepted go-race also named a package the integrated diff does not.
  const accepted = withChecks(f.accepted, (checks) => {
    checks.find((c) => c.id === "go-race").argv.push("./cmd/tt");
  });
  const context = f.integrate(["hub/internal/store/store.go", "docs/other.md"], accepted);
  const { plan, preserved } = matrixRunner.planWithPreservation(context, f.cwd);
  assert.deepEqual(raceArgv(plan), [
    "go", "test", "-race", "./cmd/tt", "./internal/api", "./internal/store",
  ]);
  for (const check of accepted.checks) {
    const kept = plan.checks.find((c) => c.id === check.id);
    if (check.id === "go-race")
      assert.deepEqual({ ...kept, argv: [] }, { ...check, argv: [] });
    else assert.deepEqual(kept, check);
    for (const argument of check.argv) assert(kept.argv.includes(argument), argument);
  }
  assert.deepEqual(plan.checks.map((c) => c.id), ["go-race", "go-test", "go-vet", "npm-unit"]);
  // Digest and matrix policy describe the final list, npm-unit included.
  assert.equal(plan.checksDigest, digest(plan.checks));
  assert.equal(plan.maxAttempts, 3);
  assert.deepEqual(plan.knownFailures, [integratedKnownFailure]);
  assert.deepEqual(preserved, {
    version: 1,
    acceptedChecksDigest: digest(accepted.checks),
    kept: ["go-test", "go-vet"],
    widened: ["go-race"],
    rebuilt: [],
    added: ["npm-unit"],
    narrowerSelection: ["go-race"],
    checksDigest: plan.checksDigest,
  });
  // An accepted package list under a selection of every package: ./... wins.
  const race = plan.checks.find((c) => c.id === "go-race");
  const every = matrixRunner.keepAcceptedChecks(
    [{ ...race, argv: ["go", "test", "-race", "./..."] }],
    [{ ...race, argv: ["go", "test", "-race", "./internal/api"] }],
  );
  assert.deepEqual(every.checks[0].argv, ["go", "test", "-race", "./..."]);
  assert.deepEqual(every.preserved.widened, ["go-race"]);
  assert.deepEqual(every.preserved.narrowerSelection, []);
});

test("an accepted check that differs any other way, or is not selected, refuses the plan by name", async (t) => {
  const f = integratedFixture(t);
  const context = f.integrate(["hub/internal/store/store.go", "docs/other.md"]);
  const refused = (change, pattern) =>
    assert.throws(() => makePlan(withChecks(context, change), f.cwd), pattern);
  refused((checks) => {
    checks.find((c) => c.id === "go-vet").argv = ["true"];
  }, /^Error: Accepted check differs from the selected one: go-vet$/);
  refused((checks) => {
    checks.find((c) => c.id === "go-race").argv.splice(3, 0, "-timeout=45m");
  }, /^Error: Accepted check differs from the selected one: go-race$/);
  refused((checks) => {
    checks.find((c) => c.id === "go-race").environment.VERIFICATION_TIMEOUT_MS = "1";
  }, /^Error: Accepted check differs from the selected one: go-race$/);
  refused((checks) => {
    checks.find((c) => c.id === "go-race").argv.push("-run=None");
  }, /^Error: Accepted check differs from the selected one: go-race$/);
  refused((checks) => {
    checks.push({ id: "wasm-test-build", argv: ["true"], cwd: ".", environment: {} });
  }, /^Error: Accepted check is not selected for this commit: wasm-test-build$/);
  refused((checks) => [...checks, checks[0]], /^Error: Invalid accepted checks$/);
  assert.throws(
    () => makePlan({ ...context, checks: context.checks.slice(1) }, f.cwd),
    /^Error: Accepted checks do not match their checksDigest$/,
  );
  // The run guard reads the plan's own checks as accepted ones, so the same
  // rule refuses an edited plan, and the comparison one that drops a check or
  // a package this commit selects. A go-race cut back to exactly this commit's
  // selection is the plan the hub refuses.
  const plan = makePlan(context, f.cwd);
  const edited = (change) => {
    const checks = structuredClone(plan.checks);
    change(checks);
    return { ...plan, checks, checksDigest: digest(checks) };
  };
  const out = tempDir(t, "verification-logs-");
  for (const change of [
    (checks) => (checks.find((c) => c.id === "npm-unit").argv = ["true"]),
    (checks) => checks.push({ id: "extra", argv: ["true"], cwd: ".", environment: {} }),
    (checks) => (checks.find((c) => c.id === "go-race").argv[3] = "./cmd/tt"),
    (checks) => checks.pop(),
  ])
    await assert.rejects(
      () => runPlan(edited(change), f.cwd, out),
      /^Error: Altered or omitted required checks/,
    );
  assert.equal(existsSync(join(out, "receipt.json")), false);
});

test("run re-derives an integrated plan that kept go-race on every package and executes it", async (t) => {
  const f = integratedFixture(t);
  const plan = makePlan(f.integrate(), f.cwd);
  assert.deepEqual(raceArgv(plan), ["go", "test", "-race", "./..."]);
  const receipt = await runPlan(plan, f.cwd, tempDir(t, "verification-logs-"), { jobs: 1 });
  assert.equal(receipt.planDigest, digest(plan));
  assert.equal(receipt.checksDigest, f.accepted.checksDigest);
  assert.deepEqual(
    receipt.checks.map((c) => [c.id, c.exitCode]),
    [["go-race", 0], ["go-test", 0], ["go-vet", 0]],
  );
  assert.deepEqual(receipt.checks[0].argv, ["go", "test", "-race", "./..."]);
});

test("a context without checks plans as before, and only plan mode writes the kept-checks record", (t) => {
  const f = integratedFixture(t);
  const matrixRaw = readFileSync(join(f.cwd, "verification/matrix.json"), "utf8");
  const context = {
    approvedMatrixDigest: digest(matrixRaw),
    matrixApprovalMessageSeq: 1,
    ...f.context,
  };
  const checks = selectChecks(
    JSON.parse(matrixRaw),
    context.owned,
    ["hub/cmd/tt/testdata/README.md"],
    new Set(["./cmd/tt", "./internal/api", "./internal/store"]),
  );
  for (const check of checks) check.environment.VERIFICATION_BASE_COMMIT = f.base;
  const before = {
    ...context,
    version: 1,
    maxAttempts: 3,
    matrixDigest: digest(matrixRaw),
    checksDigest: digest(checks),
    changed: ["hub/cmd/tt/testdata/README.md"],
    checks,
  };
  assert.equal(JSON.stringify(rawMakePlan(context, f.cwd)), JSON.stringify(before));
  assert.equal(matrixRunner.planWithPreservation(context, f.cwd).preserved, null);
  const directory = tempDir(t, "verification-plan-");
  const contextFile = join(directory, "context.json"),
    planFile = join(directory, "plan.json"),
    recordFile = join(directory, "plan.preserved.json");
  assert.equal(matrixRunner.preservationPath(planFile), recordFile);
  const planMode = (input) => {
    writeFileSync(contextFile, JSON.stringify(input));
    const result = spawnSync(
      process.execPath,
      [new URL("../scripts/verify-matrix.mjs", import.meta.url).pathname, "plan", contextFile, planFile],
      { cwd: f.cwd, encoding: "utf8" },
    );
    assert.equal(result.status, 0, result.stderr);
    return result.stderr;
  };
  assert.equal(planMode(context), "");
  assert.equal(readFileSync(planFile, "utf8"), JSON.stringify(before, null, 2) + "\n");
  assert.equal(existsSync(recordFile), false);
  const integrated = f.integrate(undefined, before);
  assert.equal(
    planMode(integrated),
    `Kept 3 accepted checks (accepted checksDigest ${before.checksDigest}); this commit alone selected less for: go-race\n`,
  );
  const written = JSON.parse(readFileSync(planFile, "utf8"));
  assert.deepEqual(Object.keys(written).sort(), Object.keys(before).sort());
  assert.deepEqual(raceArgv(written), ["go", "test", "-race", "./..."]);
  assert.deepEqual(JSON.parse(readFileSync(recordFile, "utf8")), {
    version: 1,
    acceptedChecksDigest: before.checksDigest,
    kept: ["go-race", "go-test", "go-vet"],
    widened: [],
    rebuilt: [],
    added: [],
    narrowerSelection: ["go-race"],
    checksDigest: written.checksDigest,
  });
  // A later plan without accepted checks leaves no stale record behind.
  const { checks: accepted, checksDigest, ...plain } = integrated;
  assert.equal(planMode(plain), "");
  assert.equal(existsSync(recordFile), false);
  // Targeted plans select from the fix's paths alone and ignore carried checks.
  const targeted = { baseCommit: f.base, commit: integrated.commit };
  assert.deepEqual(
    makeTargetedPlan({ ...targeted, checks: [{ id: "x" }] }, f.cwd),
    makeTargetedPlan(targeted, f.cwd),
  );
});

test("after an approved matrix change the rebuilt checks stand and go-race still covers accepted packages", async (t) => {
  // The item changes a non-Go file under hub/, so its accepted go-race is ./...
  const f = integratedFixture(t);
  const oldRaw = readFileSync(join(f.cwd, "verification/matrix.json"), "utf8");
  assert.equal(f.accepted.matrixDigest, digest(oldRaw));
  writeFileSync(
    join(f.cwd, "verification/matrix.json"),
    JSON.stringify({
      ...JSON.parse(oldRaw),
      goTestFlags: ["-timeout=30m"],
      checkTimeoutMs: { "go-vet": 120000 },
      rules: [
        ...JSON.parse(oldRaw).rules,
        { prefixes: ["verification/"], groups: ["go"] },
      ],
    }),
  );
  // What the release runner builds: the accepted plan, the newer approval and
  // the integrated commit; matrixDigest still names the accepted matrix.
  const carried = f.integrate();
  const newRaw = readFileSync(join(f.cwd, "verification/matrix.json"), "utf8");
  const context = {
    ...carried,
    approvedMatrixDigest: digest(newRaw),
    matrixApprovalMessageSeq: 2,
  };
  const { plan, preserved } = matrixRunner.planWithPreservation(context, f.cwd);
  // Flags and timeouts come from the new matrix; the packages stay ./...,
  // where the integrated diff alone selects only another item's package.
  assert.deepEqual(raceArgv(plan), ["go", "test", "-race", "-timeout=30m", "./..."]);
  assert.deepEqual(plan.checks.find((c) => c.id === "go-test").argv, [
    "go", "test", "-timeout=30m", "./...",
  ]);
  const vet = plan.checks.find((c) => c.id === "go-vet");
  assert.equal(vet.environment.VERIFICATION_TIMEOUT_MS, "120000");
  assert.equal(plan.matrixDigest, digest(newRaw));
  assert.equal(plan.checksDigest, digest(plan.checks));
  assert.deepEqual(Object.keys(plan).sort(), Object.keys(f.accepted).sort());
  assert.deepEqual(preserved, {
    version: 1,
    acceptedChecksDigest: f.accepted.checksDigest,
    kept: [],
    widened: [],
    rebuilt: ["go-race", "go-test", "go-vet"],
    added: [],
    narrowerSelection: ["go-race"],
    acceptedMatrixDigest: digest(oldRaw),
    matrixDigest: digest(newRaw),
    checksDigest: plan.checksDigest,
  });
  // A rebuilt go-race unions an accepted package list too.
  const race = plan.checks.find((c) => c.id === "go-race");
  const listed = matrixRunner.keepAcceptedChecks(
    [{ ...race, argv: ["go", "test", "-race", "-timeout=30m", "./internal/store"] }],
    [{ ...race, argv: ["go", "test", "-race", "./cmd/tt"] }],
    undefined,
    { matrixChanged: true },
  );
  assert.deepEqual(listed.checks[0].argv, [
    "go", "test", "-race", "-timeout=30m", "./cmd/tt", "./internal/store",
  ]);
  assert.deepEqual(listed.preserved.rebuilt, ["go-race"]);
  // Still refused by name under a changed matrix: an accepted go-race whose
  // packages cannot be read, and an accepted check the new matrix drops.
  assert.throws(
    () => makePlan(withChecks(context, (checks) => {
      checks.find((c) => c.id === "go-race").argv.push("-run=None");
    }), f.cwd),
    /^Error: Accepted check differs from the selected one: go-race$/,
  );
  assert.throws(
    () => makePlan(withChecks(context, (checks) => {
      checks.push({ id: "npm-unit", argv: ["npm", "test"], cwd: ".", environment: {} });
    }), f.cwd),
    /^Error: Accepted check is not selected for this commit: npm-unit$/,
  );
  // With the accepted matrix digest the same differences are refused: only a
  // digest the owner approved again explains a rebuilt check.
  assert.throws(
    () => makePlan({ ...context, matrixDigest: digest(newRaw) }, f.cwd),
    /^Error: Accepted check differs from the selected one: go-race$/,
  );
  // The plan names the new matrix, so run re-derives it as unchanged.
  const receipt = await runPlan(plan, f.cwd, tempDir(t, "verification-logs-"), { jobs: 1 });
  assert.equal(receipt.planDigest, digest(plan));
  assert.deepEqual(receipt.checks[0].argv, ["go", "test", "-race", "-timeout=30m", "./..."]);
  assert.deepEqual(receipt.checks.map((c) => c.exitCode), [0, 0, 0]);
});

// The release runner's case: the job's base is older than the tasks-hub tip,
// which moved through other releases, and the job is picked onto that tip.
// job and tip map each changed path to its new content.
function tipFixture(
  t,
  job = { "docs/x.md": "the job's change\n" },
  tip = { "hub/internal/store/store.go": "package store\n\nvar Tip = 1\n" },
) {
  const f = integratedFixture(t);
  mkdirSync(join(f.cwd, "docs"), { recursive: true });
  const commit = (files, message) => {
    for (const [path, text] of Object.entries(files)) writeFileSync(join(f.cwd, path), text);
    f.git("add", ".");
    f.git("commit", "-qm", message);
    return f.git("rev-parse", "HEAD");
  };
  f.git("checkout", "-q", "--detach", f.base);
  const candidate = commit(job, "the job");
  const accepted = makePlan(
    { ...f.context, commit: candidate, owned: Object.keys(job).sort() },
    f.cwd,
  );
  f.git("checkout", "-q", "--detach", f.base);
  const tipCommit = commit(tip, "another release");
  // The published branch the release runner integrates onto.
  f.git("update-ref", "refs/heads/tasks-hub", tipCommit);
  f.git("cherry-pick", candidate);
  const integrated = f.git("rev-parse", "HEAD");
  // What the release runner saves as the plan context.
  const context = {
    ...accepted,
    commit: integrated,
    selectionBaseCommit: tipCommit,
    selectionPaths: [...new Set([...accepted.changed, ...accepted.owned])].sort(),
  };
  return { ...f, candidate, accepted, tip: tipCommit, integrated, context };
}
const checkIds = (plan) => plan.checks.map((c) => c.id);
const withoutSelection = ({ selectionBaseCommit, selectionPaths, ...context }) => context;

test("a1 a docs-only job picked onto a tip that moved through a hub change selects the docs checks only", (t) => {
  const f = tipFixture(t);
  assert.notEqual(f.tip, f.base);
  assert.deepEqual(checkIds(f.accepted), ["npm-unit"]);
  const { plan, preserved, selection } = matrixRunner.planWithPreservation(f.context, f.cwd);
  assert.deepEqual(checkIds(plan), ["npm-unit"]);
  assert.deepEqual(plan.checks, f.accepted.checks);
  assert.deepEqual(plan.changed, ["docs/x.md"]);
  assert.deepEqual(preserved.kept, ["npm-unit"]);
  assert.deepEqual(preserved.added, []);
  assert.deepEqual(selection, {
    rule: "integration-tip",
    baseCommit: f.base,
    selectionBaseCommit: f.tip,
  });
  // The bound base does not move and the plan gains no field: the hub binds
  // a receipt to the plan fields it knows.
  assert.equal(plan.baseCommit, f.base);
  assert.equal(plan.commit, f.integrated);
  assert.deepEqual(Object.keys(plan).sort(), Object.keys(f.accepted).sort());
  // Selected from the job's base, the same commit also runs the hub checks
  // of the release the tip moved through.
  const before = matrixRunner.planWithPreservation(withoutSelection(f.context), f.cwd);
  assert.deepEqual(checkIds(before.plan), ["go-race", "go-test", "go-vet", "npm-unit"]);
  assert.deepEqual(before.plan.changed, ["docs/x.md", "hub/internal/store/store.go"]);
  assert.deepEqual(before.selection, {
    rule: "job-base",
    baseCommit: f.base,
    reason: "no-selection-base",
  });
});

test("a Go job picked onto a moved tip still selects Go, for its own package", (t) => {
  const f = tipFixture(
    t,
    { "hub/internal/api/api.go": "package api\n\nvar Job = 1\n" },
    {
      "hub/internal/store/store.go": "package store\n\nvar Tip = 1\n",
      "docs/other.md": "another release\n",
    },
  );
  const { plan, selection } = matrixRunner.planWithPreservation(f.context, f.cwd);
  assert.equal(selection.rule, "integration-tip");
  assert.deepEqual(checkIds(plan), ["go-race", "go-test", "go-vet"]);
  assert.deepEqual(raceArgv(plan), ["go", "test", "-race", "./internal/api"]);
  assert.deepEqual(plan.checks, f.accepted.checks);
  for (const check of plan.checks)
    assert.equal(check.environment.VERIFICATION_BASE_COMMIT, f.base);
  const before = makePlan(withoutSelection(f.context), f.cwd);
  assert.deepEqual(checkIds(before), ["go-race", "go-test", "go-vet", "npm-unit"]);
  assert.deepEqual(raceArgv(before), ["go", "test", "-race", "./internal/api", "./internal/store"]);
});

test("a path the tip already holds stays selected through the job's own paths", (t) => {
  // The tip made the job's hub change already, so the pick leaves only the
  // docs path in the tip-to-integrated diff.
  const api = "package api\n\nvar Same = 1\n";
  const f = tipFixture(
    t,
    { "docs/x.md": "the job's change\n", "hub/internal/api/api.go": api },
    { "hub/internal/api/api.go": api },
  );
  assert.deepEqual(matrixRunner.diffPaths(f.cwd, f.tip, f.integrated), ["docs/x.md"]);
  const { plan, selection } = matrixRunner.planWithPreservation(f.context, f.cwd);
  assert.equal(selection.rule, "integration-tip");
  assert.deepEqual(plan.changed, ["docs/x.md", "hub/internal/api/api.go"]);
  assert.deepEqual(plan.checks, f.accepted.checks);
  // Without the job's own paths the accepted Go checks are not selected and
  // the plan is refused rather than narrowed.
  assert.throws(
    () => makePlan({ ...f.context, selectionPaths: [], owned: ["docs/x.md"] }, f.cwd),
    /Accepted check is not selected for this commit: go-race/,
  );
});

test("a3 an unknown, malformed or off-line selection base selects from the job's base as before", (t) => {
  const f = tipFixture(t);
  const before = makePlan(withoutSelection(f.context), f.cwd);
  assert.deepEqual(checkIds(before), ["go-race", "go-test", "go-vet", "npm-unit"]);
  const side = f.git("commit-tree", "-m", "unrelated", f.git("rev-parse", f.base + "^{tree}"));
  const cases = [
    [{ selectionBaseCommit: "d".repeat(40) }, "unreadable-selection-base"],
    [{ selectionBaseCommit: "tasks-hub" }, "invalid-selection-base"],
    [{ selectionBaseCommit: f.tip.slice(0, 12) }, "invalid-selection-base"],
    [{ selectionBaseCommit: 7 }, "invalid-selection-base"],
    [{ selectionBaseCommit: null }, "invalid-selection-base"],
    // Real commits off the line from the bound base to the integrated commit.
    [{ selectionBaseCommit: f.candidate }, "selection-base-off-line"],
    [{ selectionBaseCommit: side }, "selection-base-off-line"],
    [{ selectionBaseCommit: f.git("rev-parse", f.base + "^") }, "selection-base-off-line"],
    [{ selectionPaths: undefined }, "invalid-selection-paths"],
    [{ selectionPaths: "docs/x.md" }, "invalid-selection-paths"],
    [{ selectionPaths: [7] }, "invalid-selection-paths"],
    // A malformed path, or one the matrix has no rule for, falls back too.
    [{ selectionPaths: ["../outside.md"] }, "invalid-selection-paths"],
    [{ selectionPaths: ["/etc/hosts"] }, "invalid-selection-paths"],
    [{ selectionPaths: [""] }, "invalid-selection-paths"],
    [{ selectionPaths: ["docs/x.md\nhub/y.go"] }, "invalid-selection-paths"],
    [{ selectionPaths: ["unknown/path.txt"] }, "invalid-selection-paths"],
    // A commit the published branch does not hold: the integrated commit itself.
    [{ selectionBaseCommit: f.integrated }, "selection-base-unpublished"],
  ];
  const fallsBack = ([change, reason]) => {
    const { plan, selection } = matrixRunner.planWithPreservation({ ...f.context, ...change }, f.cwd);
    assert.deepEqual(plan, before, reason);
    assert.deepEqual(selection, { rule: "job-base", baseCommit: f.base, reason }, reason);
  };
  cases.forEach(fallsBack);
  // The true tip, while the published branch is behind it or cannot be read.
  f.git("update-ref", "refs/heads/tasks-hub", f.base);
  fallsBack([{}, "selection-base-unpublished"]);
  f.git("update-ref", "-d", "refs/heads/tasks-hub");
  fallsBack([{}, "unreadable-published-ref"]);
  f.git("update-ref", "refs/heads/tasks-hub", f.tip);
  assert.equal(matrixRunner.planWithPreservation(f.context, f.cwd).selection.rule, "integration-tip");
});

test("b1 a candidate cannot narrow its own selection by naming itself or an unpublished commit as the tip", async (t) => {
  // The reviewer's trigger: a docs and Go change whose context, or whose
  // forged plan record, names the candidate itself with no selection paths.
  const f = tipFixture(
    t,
    { "docs/x.md": "the job's change\n", "hub/internal/api/api.go": "package api\n\nvar Job = 1\n" },
    { "docs/other.md": "another release\n" },
  );
  // A team context: the candidate on its base, owning only the docs path.
  f.git("checkout", "-q", "--detach", f.candidate);
  const { checks, checksDigest, changed, ...team } = { ...f.accepted, owned: ["docs/x.md"] };
  const honest = makePlan(team, f.cwd);
  assert.deepEqual(checkIds(honest), ["go-race", "go-test", "go-vet", "npm-unit"]);
  const named = matrixRunner.planWithPreservation(
    { ...team, selectionBaseCommit: f.candidate, selectionPaths: [] },
    f.cwd,
  );
  assert.deepEqual(named.plan, honest);
  assert.deepEqual(named.selection, {
    rule: "job-base",
    baseCommit: f.base,
    reason: "selection-base-unpublished",
  });
  assert.deepEqual(
    makePlan({ ...team, selectionBaseCommit: f.base, selectionPaths: [] }, f.cwd),
    honest,
  );
  // The narrowed plan such a context produced before this rule: docs only.
  const kept = honest.checks.filter((c) => c.id === "npm-unit");
  const narrowed = { ...honest, changed: [], checks: kept, checksDigest: digest(kept) };
  const run = (options) =>
    runPlan(narrowed, f.cwd, tempDir(t, "verification-logs-"), { jobs: 1, ...options });
  await assert.rejects(run({}), /Altered or omitted required checks/);
  for (const selectionBaseCommit of [f.candidate, f.base])
    await assert.rejects(
      run({ selection: { selectionBaseCommit, selectionPaths: [] } }),
      /Altered or omitted required checks/,
      selectionBaseCommit,
    );
  // The same on the integrated commit: the latest tip a record can name is
  // the published one, whose diff holds the whole change.
  f.git("checkout", "-q", "--detach", f.integrated);
  const integrated = makePlan(f.context, f.cwd);
  assert.deepEqual(checkIds(integrated), ["go-race", "go-test", "go-vet", "npm-unit"]);
  assert.deepEqual(
    makePlan({ ...f.context, selectionBaseCommit: f.integrated, selectionPaths: [] }, f.cwd),
    makePlan(withoutSelection(f.context), f.cwd),
  );
  assert.deepEqual(
    makePlan({ ...f.context, selectionPaths: [] }, f.cwd).checks,
    integrated.checks,
    "the published tip with no paths still selects the candidate's own change",
  );
});

test("plan mode records the selection and run mode re-derives a tip-selected plan only with that record", async (t) => {
  const f = tipFixture(t);
  const directory = tempDir(t, "verification-plan-");
  const contextFile = join(directory, "context.json"),
    planFile = join(directory, "plan.json"),
    recordFile = matrixRunner.preservationPath(planFile);
  const planMode = (input) => {
    writeFileSync(contextFile, JSON.stringify(input));
    const result = spawnSync(
      process.execPath,
      [new URL("../scripts/verify-matrix.mjs", import.meta.url).pathname, "plan", contextFile, planFile],
      { cwd: f.cwd, encoding: "utf8" },
    );
    assert.equal(result.status, 0, result.stderr);
    return result.stderr;
  };
  assert.equal(
    planMode(f.context),
    `Selected checks from the integration tip ${f.tip}\nKept 1 accepted checks (accepted checksDigest ${f.accepted.checksDigest})\n`,
  );
  const plan = JSON.parse(readFileSync(planFile, "utf8"));
  assert.deepEqual(checkIds(plan), ["npm-unit"]);
  assert.deepEqual(Object.keys(plan).sort(), Object.keys(f.accepted).sort());
  const record = JSON.parse(readFileSync(recordFile, "utf8"));
  assert.deepEqual(record.selection, {
    rule: "integration-tip",
    baseCommit: f.base,
    selectionBaseCommit: f.tip,
    selectionPaths: ["docs/x.md"],
  });
  assert.deepEqual(record.kept, ["npm-unit"]);
  const selection = matrixRunner.recordedSelection(planFile);
  assert.deepEqual(selection, {
    selectionBaseCommit: f.tip,
    selectionPaths: ["docs/x.md"],
  });
  const run = (options) =>
    runPlan(plan, f.cwd, tempDir(t, "verification-logs-"), { jobs: 1, ...options });
  const receipt = await run({ selection });
  assert.equal(receipt.planDigest, digest(plan));
  assert.equal(receipt.baseCommit, f.base);
  assert.deepEqual(receipt.checks.map((c) => c.id), ["npm-unit"]);
  // Without the record, or with a tip off the line, the plan is re-derived
  // from the job's base and the narrower plan is refused.
  await assert.rejects(run({}), /Altered or omitted required checks/);
  for (const forged of [f.candidate, "d".repeat(40), "tasks-hub"])
    await assert.rejects(
      run({ selection: { ...selection, selectionBaseCommit: forged } }),
      /Altered or omitted required checks/,
      forged,
    );
  // A record naming the integrated commit itself as the tip is refused: it is
  // on the line but not on the published branch, so the plan is re-derived
  // from the job's base. (This record was accepted before the published
  // branch condition; accepting it let a commit name itself as its tip.)
  await assert.rejects(
    run({ selection: { ...selection, selectionBaseCommit: f.integrated } }),
    /Altered or omitted required checks/,
  );
  // The run command reads the record beside its plan file, with no argument.
  const root = tempDir(t, "verification-cli-root-");
  const cli = () =>
    spawnSync(
      process.execPath,
      [
        new URL("../scripts/verify-matrix.mjs", import.meta.url).pathname,
        "run", planFile, tempDir(t, "verification-cli-logs-"), "--min-free-bytes", "0",
      ],
      { cwd: f.cwd, env: { ...process.env, TMPDIR: root }, encoding: "utf8" },
    );
  const ran = cli();
  assert.equal(ran.status, 0, ran.stderr);
  rmSync(recordFile);
  assert.equal(matrixRunner.recordedSelection(planFile), undefined);
  const refused = cli();
  assert.equal(refused.status, 1);
  assert.match(refused.stderr, /Altered or omitted required checks/);
  writeFileSync(recordFile, "{");
  assert.equal(matrixRunner.recordedSelection(planFile), undefined);
  writeFileSync(recordFile, JSON.stringify({ version: 1, selection: { rule: "integration-tip", selectionBaseCommit: f.tip } }));
  assert.equal(matrixRunner.recordedSelection(planFile), undefined);
  // A base the rule refused is recorded with its reason and names no tip.
  assert.equal(
    planMode({ ...f.context, selectionBaseCommit: "d".repeat(40) }),
    `Selected checks from the job base ${f.base} (unreadable-selection-base)\nKept 1 accepted checks (accepted checksDigest ${f.accepted.checksDigest})\n`,
  );
  assert.deepEqual(JSON.parse(readFileSync(recordFile, "utf8")).selection, {
    rule: "job-base",
    baseCommit: f.base,
    reason: "unreadable-selection-base",
  });
  assert.equal(matrixRunner.recordedSelection(planFile), undefined);
  // With no accepted checks the record holds the selection alone.
  const { checks, checksDigest, ...plain } = f.context;
  assert.equal(planMode(plain), `Selected checks from the integration tip ${f.tip}\n`);
  assert.deepEqual(Object.keys(JSON.parse(readFileSync(recordFile, "utf8"))), ["version", "selection"]);
});

// Detached matrix run (wi_912a9789357529d4, order #27051). a1-a10 of the plan.
// The launcher is the CLI without the foreground opt-out: it starts the real
// run detached and only follows its log. Every run it starts is stopped by pid
// when its test ends.
const runnerRecord = (output) => {
  try {
    return JSON.parse(readFileSync(join(output, "runner.json"), "utf8"));
  } catch {
    return undefined;
  }
};
const pidAlive = (pid) => {
  try {
    process.kill(pid, 0);
    return true;
  } catch (error) {
    return error.code === "EPERM";
  }
};
function launcherCLI(t, f, args, environment = {}, options = {}) {
  const env = { ...process.env, TAILTERM_MATRIX_FOREGROUND: "", ...environment };
  for (const key of ["TAILTERM_MATRIX_RUN_CHILD", "TAILTERM_MATRIX_RELEASE"])
    if (!(key in environment)) delete env[key];
  const child = spawn(process.execPath, [matrixScript, ...args], {
    cwd: f.cwd,
    env,
    stdio: ["ignore", "pipe", "pipe"],
    detached: options.group === true,
  });
  child.text = "";
  child.stdout.on("data", (data) => (child.text += data));
  child.stderr.on("data", (data) => (child.text += data));
  const closed = once(child, "close");
  child.closed = closed.then(([code, signal]) => code ?? signal);
  // The run is asked to stop first, so it stops its own checks and leaves the
  // lock file, and is killed only if it does not.
  t.after(async () => {
    if (child.exitCode === null && child.signalCode === null) child.kill("SIGKILL");
    const record = runnerRecord(args[2]);
    if (!record || record.exitCode !== undefined || !Number.isInteger(record.pid) || !pidAlive(record.pid)) return;
    try {
      process.kill(record.pid, "SIGTERM");
      for (let i = 0; i < 500 && pidAlive(record.pid); i++)
        await new Promise((resolve) => setTimeout(resolve, 10));
      if (pidAlive(record.pid)) process.kill(record.pid, "SIGKILL");
    } catch {}
  });
  return child;
}
// The queued run of a launcher: its waitlist entry once runner.json names it.
const queuedRun = (output, what) =>
  untilHost(() => {
    const record = runnerRecord(output);
    const waiter = record && readHostState(hostLockFile())?.waiters.find((w) => w.pid === record.pid);
    return waiter && { record, waiter };
  }, what + " to join the waitlist as the detached run");

for (const [name, stop, expected] of [
  ["SIGTERM to the launcher", (child) => child.kill("SIGTERM"), 143],
  ["SIGKILL to the launcher's process group", (child) => process.kill(-child.pid, "SIGKILL"), "SIGKILL"],
])
  test(`detached a1 ${name} while the run is queued leaves the run its waitlist place and it runs after the release`, async (t) => {
    const { f, planFile } = plannedFixture(t, "console.log('ran');"),
      output = tempDir(t, "verification-detached-logs-");
    const holder = await holdHost("wi_a1_holder");
    let released = false;
    try {
      const child = launcherCLI(t, f, ["run", planFile, output], {}, { group: true });
      const { record, waiter } = await queuedRun(output, "the run");
      assert.notEqual(waiter.pid, child.pid, "the launcher does not hold the place");
      assert.equal(waiter.pid, record.pid);
      await untilHost(() => child.text.includes("matrix host: waiting 1 of 1"), "the waiting line; saw " + child.text);
      stop(child);
      assert.equal(await child.closed, expected, child.text);
      if (expected === 143)
        assert.match(child.text, new RegExp(`stopped by SIGTERM; the run continues as pid ${record.pid}`));
      // The place outlives the launcher.
      await new Promise((resolve) => setTimeout(resolve, 300));
      const after = readHostState(hostLockFile()).waiters;
      assert.deepEqual(after.map((w) => [w.id, w.seq, w.pid]), [[waiter.id, waiter.seq, record.pid]]);
      assert(pidAlive(record.pid), "the run is still alive");
      released = true;
      await holder.release();
      await untilHost(() => runnerRecord(output)?.exitCode !== undefined, "the run to finish");
      assert.equal(runnerRecord(output).exitCode, 0, readFileSync(join(output, "runner.log"), "utf8"));
      const e = JSON.parse(readFileSync(join(output, "receipt.json"), "utf8")).environment;
      assert.equal(e.VERIFICATION_HOST_QUEUE_POSITION, "1");
      assert.equal(JSON.parse(readFileSync(join(output, "host-lock.json"), "utf8")).id, waiter.id);
    } finally {
      if (!released) await holder.release();
    }
  });

test("detached a2 with nothing stopped the launcher shows the run's lines and exits with the run's code", async (t) => {
  const { f, planFile } = plannedFixture(t, "console.log('ran');"),
    output = tempDir(t, "verification-detached-logs-");
  const holder = await holdHost("wi_a2_holder");
  const child = launcherCLI(t, f, ["run", planFile, output]);
  const { record } = await queuedRun(output, "the run");
  await untilHost(() => /matrix host: waiting 1 of 1, holder wi_a2_holder\/holder/.test(child.text), "the waiting line; saw " + child.text);
  assert(!existsSync(join(output, "receipt.json")), "no check ran while waiting");
  await holder.release();
  assert.equal(await child.closed, 0, child.text);
  assert.match(child.text, new RegExp(`^matrix run: detached as pid ${record.pid}; log \\S+runner\\.log; it keeps running if this command is stopped\\. Run the same command to re-attach; stop the run with: kill ${record.pid}\\n`));
  assert.match(child.text, /matrix host: acquired after \d+ ms\n/);
  assert(existsSync(join(output, "receipt.json")), "the receipt was written before the launcher returned");
  assert.equal(readFileSync(join(output, "runner.log"), "utf8"), child.text.slice(child.text.indexOf("\n") + 1), "the launcher printed the whole log");
  const ended = runnerRecord(output);
  assert.deepEqual([ended.version, ended.pid, ended.exitCode, ended.log], [1, record.pid, 0, join(output, "runner.log")]);
  assert(Date.parse(ended.endedAt) >= Date.parse(ended.startedAt));
  assert.deepEqual(ended.argv, ["run", planFile, output]);
  // A failed check is the run's exit code 1.
  const failing = plannedFixture(t, "process.exit(1);"),
    failedOutput = tempDir(t, "verification-detached-logs-");
  const failed = launcherCLI(t, failing.f, ["run", failing.planFile, failedOutput]);
  assert.equal(await failed.closed, 1, failed.text);
  assert.equal(runnerRecord(failedOutput).exitCode, 1);
});

test("detached a2 a6 a wait that expires exits the launcher 75 and reports nothing", async (t) => {
  rmSync(fakeTTCalls, { force: true });
  const { f, planFile } = plannedFixture(t, "console.log('ran');"),
    output = tempDir(t, "verification-detached-logs-");
  const holder = await acquireHostLock({ item: "wi_a2_expiry_holder", agent: "holder", runTimeoutMs: 120000, pollMs: 20 });
  try {
    const child = launcherCLI(t, f, ["run", planFile, output, "--host-wait-minutes", "1"], agentIdentity);
    assert.equal(await child.closed, 75, child.text);
    assert.match(child.text, /Host lock wait expired after 60000 ms/);
    assert.equal(runnerRecord(output).exitCode, 75);
    assert.deepEqual(readHostState(hostLockFile()).waiters, []);
    assert.deepEqual(fakeTTLookups(), [], "an expired wait is not a signal withdrawal");
    assert(!existsSync(join(output, "withdrawal-report.json")));
    assert(!existsSync(join(output, "receipt.json")));
  } finally {
    await holder.release();
  }
});

test("detached a3 the same command again re-attaches to the queued run, and a dead recorded pid starts a fresh run", async (t) => {
  const { f, planFile } = plannedFixture(t, "console.log('ran');"),
    output = tempDir(t, "verification-detached-logs-");
  const journalBefore = readJournal(hostLockFile()).length;
  const holder = await holdHost("wi_a3_holder");
  let released = false;
  try {
    const first = launcherCLI(t, f, ["run", planFile, output]);
    const { record, waiter } = await queuedRun(output, "the first run");
    const second = launcherCLI(t, f, ["run", planFile, output]);
    await untilHost(() => second.text.includes(`matrix run: re-attached to pid ${record.pid}\n`), "the re-attach line; saw " + second.text);
    assert(!second.text.includes("detached as pid"), second.text);
    await new Promise((resolve) => setTimeout(resolve, 300));
    assert.deepEqual(readHostState(hostLockFile()).waiters.map((w) => w.id), [waiter.id], "one waiter");
    assert.equal(runnerRecord(output).pid, record.pid);
    // A different command into the same directory is refused while the first
    // run is queued: it neither follows that run nor starts its own. So is
    // the same command from another checkout.
    const other = plannedFixture(t, "process.exit(1);");
    const logBefore = readFileSync(join(output, "runner.log"), "utf8");
    for (const [checkout, args] of [
      [other.f, ["run", other.planFile, output]],
      [f, ["run", planFile, output, "--jobs", "1"]],
      [other.f, ["run", planFile, output]],
    ]) {
      const refused = launcherCLI(t, checkout, args);
      assert.equal(await refused.closed, 1, refused.text);
      assert.equal(refused.text, `Host lock output/record directory is already in use by a different matrix run (pid ${record.pid})\n`);
    }
    assert.deepEqual(readHostState(hostLockFile()).waiters.map((w) => [w.id, w.pid]), [[waiter.id, record.pid]], "still one waiter, the first run");
    assert.deepEqual(runnerRecord(output), record, "the first run's record is untouched");
    assert.equal(readFileSync(join(output, "runner.log"), "utf8"), logBefore, "no second run wrote to the log");
    assert.equal(record.cwd, f.git("rev-parse", "--show-toplevel"));
    released = true;
    await holder.release();
    assert.deepEqual([await first.closed, await second.closed], [0, 0], first.text + second.text);
    assert.match(second.text, /matrix host: acquired after \d+ ms\n/);
    const requests = readJournal(hostLockFile()).slice(journalBefore).filter((entry) => entry.event === "request" && entry.kind !== "hold" && entry.agent !== "holder");
    assert.deepEqual(requests.map((entry) => entry.id), [waiter.id], "one request");
    assert(existsSync(join(output, "receipt.json")));
  } finally {
    if (!released) await holder.release();
  }
  // A record naming a pid that is gone is not followed.
  const dead = spawnSync(process.execPath, ["-e", ""]).pid,
    fresh = tempDir(t, "verification-detached-logs-");
  assert(!pidAlive(dead));
  writeFileSync(join(fresh, "runner.json"), JSON.stringify({ version: 1, pid: dead, startedAt: new Date().toISOString(), argv: [], log: join(fresh, "runner.log") }));
  const again = launcherCLI(t, f, ["run", planFile, fresh]);
  assert.equal(await again.closed, 0, again.text);
  assert(!again.text.includes("re-attached"), again.text);
  const started = runnerRecord(fresh);
  assert.notEqual(started.pid, dead);
  assert.match(again.text, new RegExp(`^matrix run: detached as pid ${started.pid};`));
  assert.equal(started.exitCode, 0);
  assert(existsSync(join(fresh, "receipt.json")));
});

for (const [name, signal, code, target] of [
  ["SIGTERM to the run's pid", "SIGTERM", 143, "run"],
  ["SIGINT to the launcher", "SIGINT", 130, "launcher"],
])
  test(`detached a4 ${name} while it is queued withdraws the run with ${code} and no receipt`, async (t) => {
    const { f, planFile } = plannedFixture(t, "console.log('ran');"),
      output = tempDir(t, "verification-detached-logs-");
    const holder = await holdHost("wi_a4_holder");
    try {
      const child = launcherCLI(t, f, ["run", planFile, output]);
      const { record, waiter } = await queuedRun(output, "the run");
      if (target === "run") process.kill(record.pid, signal);
      else child.kill(signal);
      assert.equal(await child.closed, code, child.text);
      assert.match(child.text, new RegExp(`Verification interrupted by ${signal} while waiting for the host lock`));
      const state = readHostState(hostLockFile());
      assert.deepEqual(state.waiters, [], "the waiter left the list");
      assert.equal(state.holder.id, holder.id, "the holder was not disturbed");
      const sidecar = JSON.parse(readFileSync(join(output, "host-lock.json"), "utf8"));
      assert.deepEqual([sidecar.id, sidecar.outcome, sidecar.acquiredAt], [waiter.id, "withdrawn", null]);
      assert.equal(runnerRecord(output).exitCode, code);
      assert(!pidAlive(record.pid));
      assert(!existsSync(join(output, "receipt.json")), "no receipt");
      assert(!existsSync(join(output, "withdrawal-report.json")), "no identity, no report");
    } finally {
      await holder.release();
    }
  });

// The launcher's run once it holds the host.
const holdingRun = (output, what) =>
  untilHost(() => {
    const record = runnerRecord(output);
    return record && readHostState(hostLockFile())?.holder?.pid === record.pid && record;
  }, what + " to hold the host");
test("detached a5 SIGTERM to the launcher while the run holds the host stops only the launcher", async (t) => {
  const { f, planFile } = plannedFixture(t, sleepingCheck(1500)),
    output = tempDir(t, "verification-detached-logs-");
  const child = launcherCLI(t, f, ["run", planFile, output]);
  const record = await holdingRun(output, "the run");
  child.kill("SIGTERM");
  assert.equal(await child.closed, 143, child.text);
  assert.match(child.text, new RegExp(`stopped by SIGTERM; the run continues as pid ${record.pid}`));
  assert(pidAlive(record.pid), "the run is still alive");
  assert.equal(readHostState(hostLockFile()).holder.pid, record.pid, "and still holds the host");
  await untilHost(() => runnerRecord(output)?.exitCode !== undefined, "the run to finish");
  assert.equal(runnerRecord(output).exitCode, 0, readFileSync(join(output, "runner.log"), "utf8"));
  assert(existsSync(join(output, "receipt.json")));
  assert.equal(readHostState(hostLockFile()).holder, null);
});
for (const [name, signal, code, target] of [
  ["SIGINT to the launcher", "SIGINT", 130, "launcher"],
  ["SIGTERM to the run's pid", "SIGTERM", 143, "run"],
])
  test(`detached a5 ${name} while the run holds the host stops the run with ${code} and no receipt`, async (t) => {
    const started = join(tempDir(t, "verification-detached-check-"), "started");
    const { f, planFile } = plannedFixture(t, `import fs from 'node:fs';fs.writeFileSync(${JSON.stringify(started)},'');` + sleepingCheck(60000)),
      output = tempDir(t, "verification-detached-logs-");
    const child = launcherCLI(t, f, ["run", planFile, output]);
    const record = await holdingRun(output, "the run");
    await untilHost(() => existsSync(started), "the check to start");
    if (target === "run") process.kill(record.pid, signal);
    else child.kill(signal);
    assert.equal(await child.closed, code, child.text);
    assert.equal(runnerRecord(output).exitCode, code);
    assert(!pidAlive(record.pid));
    assert(!existsSync(join(output, "receipt.json")), "no receipt");
    assert.equal(readHostState(hostLockFile()).holder, null, "the lock was released");
    assert(!existsSync(join(output, "withdrawal-report.json")), "a run stopped while holding is not a queued withdrawal");
  });

const agentIdentity = {
  TAILTERM_AGENT: "agt_fake_verifier",
  TAILTERM_AGENT_NAME: "verifier-fake",
  TAILTERM_TASK: "tsk_fake",
};
const fakeBinding = { itemId: "wi_fake_item", itemRevision: 7, workOrderMessage: { taskId: "tsk_fake", seq: 4321 } };
test("detached a6 a run withdrawn by a signal while queued sends its lead a linked notice with the reason", async (t) => {
  rmSync(fakeTTCalls, { force: true });
  const { f, plan, planFile } = plannedFixture(t, "console.log('ran');"),
    output = tempDir(t, "verification-detached-logs-");
  const holder = await holdHost("wi_a6_holder");
  try {
    const child = launcherCLI(t, f, ["run", planFile, output], {
      ...agentIdentity,
      FAKE_TT_CONTEXT: JSON.stringify({ version: 1, binding: fakeBinding }),
    });
    const { record } = await queuedRun(output, "the run");
    process.kill(record.pid, "SIGTERM");
    assert.equal(await child.closed, 143, child.text);
    const sidecar = JSON.parse(readFileSync(join(output, "host-lock.json"), "utf8"));
    const calls = fakeTTLookups();
    assert.equal(calls.length, 2, JSON.stringify(calls));
    assert.deepEqual(calls[0], ["context", "--json"]);
    const text = `verifier-fake's matrix run for wi_fake_item at ${plan.commit.slice(0, 12)} was withdrawn by SIGTERM after waiting ${Math.round(sidecar.waitMs / 60000)} min, joined at position 1; no check ran and no receipt exists. Logs: ${output}. Start it again with the same command.`;
    assert.deepEqual(calls[1], [
      "send", "--kind", "notice", "--to", "role:lead",
      "--subject", "A queued matrix run was withdrawn by a signal",
      "--text", text,
      "--work-item", "wi_fake_item", "--work-item-revision", "7", "--work-order-message", "4321",
    ]);
    assert.deepEqual(JSON.parse(readFileSync(join(output, "withdrawal-report.json"), "utf8")), {
      version: 1, signal: "SIGTERM", waitMs: sidecar.waitMs, queuePosition: 1, reported: true, messageSeq: 4242,
    });
    assert.match(child.text, /matrix run: reported the withdrawal to the lead\n/);
    assert.deepEqual(readHostState(hostLockFile()).waiters, []);
  } finally {
    await holder.release();
  }
});

test("detached a7 when tt fails the withdrawn run still exits 143, has left the list, and records why nobody was told", async (t) => {
  rmSync(fakeTTCalls, { force: true });
  const { f, planFile } = plannedFixture(t, "console.log('ran');"),
    output = tempDir(t, "verification-detached-logs-");
  const holder = await holdHost("wi_a7_holder");
  try {
    const child = launcherCLI(t, f, ["run", planFile, output], { ...agentIdentity, FAKE_TT_MODE: "fail" });
    const { record } = await queuedRun(output, "the run");
    process.kill(record.pid, "SIGTERM");
    assert.equal(await child.closed, 143, child.text);
    assert.equal(runnerRecord(output).exitCode, 143);
    assert.deepEqual(readHostState(hostLockFile()).waiters, [], "the place is released");
    const report = JSON.parse(readFileSync(join(output, "withdrawal-report.json"), "utf8"));
    assert.deepEqual([report.reported, report.reason, report.signal], [false, "the notice failed: work item not found", "SIGTERM"]);
    assert(!("messageSeq" in report));
    assert(readFileSync(join(output, "runner.log"), "utf8").includes("matrix run: could not report the withdrawal to the lead: the notice failed: work item not found\n"));
    // The unlinked notice was still tried once, after the failed context lookup.
    const calls = fakeTTLookups();
    assert.deepEqual(calls.map((call) => call[0]), ["context", "send"]);
    assert(!calls[1].includes("--work-item"));
    // No agent identity: tt is never called.
    rmSync(fakeTTCalls, { force: true });
    const plainOutput = tempDir(t, "verification-detached-logs-");
    const plain = launcherCLI(t, f, ["run", planFile, plainOutput], { TAILTERM_AGENT: "agt_fake_verifier" });
    const queued = await queuedRun(plainOutput, "the run without an identity");
    process.kill(queued.record.pid, "SIGTERM");
    assert.equal(await plain.closed, 143, plain.text);
    assert.deepEqual(fakeTTLookups(), []);
    assert(!existsSync(join(plainOutput, "withdrawal-report.json")));
  } finally {
    await holder.release();
  }
});

for (const [name, environment] of [
  ["the release marker", { TAILTERM_MATRIX_RELEASE: "1", ...agentIdentity }],
  ["the foreground opt-out", { TAILTERM_MATRIX_FOREGROUND: "1" }],
])
  test(`detached a8 a7 with ${name} the command is the run itself: its pid waits, no runner files, no report`, async (t) => {
    rmSync(fakeTTCalls, { force: true });
    const { f, planFile } = plannedFixture(t, "console.log('ran');"),
      output = tempDir(t, "verification-detached-logs-");
    const holder = await holdHost("wi_a8_holder");
    try {
      const child = launcherCLI(t, f, ["run", planFile, output], environment);
      const waiter = await untilHost(() => readHostState(hostLockFile()).waiters[0], "the waiter; saw " + child.text);
      assert.equal(waiter.pid, child.pid, "the started pid holds the place");
      assert(!child.text.includes("matrix run: detached"), child.text);
      child.kill("SIGTERM");
      assert.equal(await child.closed, 143, child.text);
      assert.deepEqual(readHostState(hostLockFile()).waiters, []);
      assert.deepEqual(readdirSync(output).sort(), ["host-lock.json", "host-lock.log"]);
      assert.deepEqual(fakeTTLookups(), []);
    } finally {
      await holder.release();
    }
  });

test("detached a10 an output directory inside the worktree is refused before anything is created", async (t) => {
  const { f, planFile } = plannedFixture(t, "console.log('ran');");
  // The checkout's real path, as the command itself sees its directory.
  const worktree = f.git("rev-parse", "--show-toplevel");
  for (const output of [join(worktree, "logs"), "logs", "."]) {
    const child = launcherCLI(t, f, ["run", planFile, output]);
    assert.equal(await child.closed, 1, child.text);
    assert.match(child.text, /^Logs\/receipt must be outside worktree$/m);
    assert(!child.text.includes("matrix run:"), child.text);
  }
  assert(!existsSync(join(f.cwd, "logs")));
  assert.equal(f.git("status", "--short"), "");
  assert.deepEqual(readHostState(hostLockFile())?.waiters ?? [], []);
  // A path that reaches the checkout through a symbolic link starts no
  // launcher either: the command stays the run itself.
  const link = join(tempDir(t, "verification-detached-link-"), "checkout");
  symlinkSync(worktree, link);
  const linked = launcherCLI(t, f, ["run", planFile, join(link, "logs")]);
  assert.notEqual(await linked.closed, 0, linked.text);
  assert(!linked.text.includes("matrix run:"), linked.text);
  assert(!existsSync(join(f.cwd, "logs", "runner.log")) && !existsSync(join(f.cwd, "logs", "runner.json")));
});

// wi_d6b6387726c1f846: the store race suite runs as K shards inside the one
// go-race check. A test's shard is the first 8 hex digits of the SHA-256 of
// its name modulo K, and the last shard is a catch-all that skips the others.
const {
  goRaceShardOf,
  goRaceTestNames,
  goRacePartition,
  assertGoRacePartition,
  goRaceShardSelectors,
  goRaceRunCounts,
  assertRanExactlyOnce,
  goRaceShardSpec,
  checkWeight,
} = matrixRunner;
const shardPrefix = (k = 4, pkg = "./internal/store") => [
  "node", "../scripts/verify-matrix.mjs", "go-race", "-timeout=45m", `-shards=${pkg}=${k}`,
];
const shardedCheck = (k, ...packages) => ({
  id: "go-race",
  argv: [...shardPrefix(k), ...packages],
  cwd: "hub",
  environment: {},
});

test("a test's shard is pinned to its name and does not move when other tests come or go", () => {
  // printf TestAlpha | shasum -a 256 starts 3993fc05, TestGamma 0592ea06.
  assert.equal(goRaceShardOf("TestAlpha", 4), 0x3993fc05 % 4);
  assert.equal(goRaceShardOf("TestGamma", 4), 0x0592ea06 % 4);
  const pinned = {
    TestAlpha: 1, TestBeta: 3, TestGamma: 2, TestDelta: 1, TestEpsilon: 2,
    FuzzSeed: 1, ExampleHello: 2, TestInboxScale: 1, TestUsageTimeWaitCauses: 2,
  };
  for (const [name, shard] of Object.entries(pinned))
    assert.equal(goRaceShardOf(name, 4), shard, name);
  const names = Object.keys(pinned);
  const { parts, digest: partitionDigest } = goRacePartition(names, 4);
  assert.deepEqual(parts, [
    [],
    ["FuzzSeed", "TestAlpha", "TestDelta", "TestInboxScale"],
    ["ExampleHello", "TestEpsilon", "TestGamma", "TestUsageTimeWaitCauses"],
    ["TestBeta"],
  ]);
  assert.equal(
    partitionDigest,
    createHash("sha256").update(JSON.stringify(parts)).digest("hex"),
    "the digest is the SHA-256 of the canonical JSON of the K sorted lists",
  );
  assert.deepEqual(goRacePartition([...names].reverse(), 4), { parts, digest: partitionDigest });
  const shardsOf = (list) =>
    Object.fromEntries(goRacePartition(list, 4).parts.flatMap((part, i) => part.map((name) => [name, i])));
  const grown = shardsOf([...names, "TestAdded", "TestAlsoAdded", "ExampleNew"]),
    shrunk = shardsOf(names.filter((name) => name !== "TestAlpha" && name !== "TestBeta"));
  for (const [name, shard] of Object.entries(pinned)) {
    assert.equal(grown[name], shard, name + " moved when tests were added");
    if (name in shrunk) assert.equal(shrunk[name], shard, name + " moved when tests were removed");
  }
  assert.equal(Object.keys(shrunk).length, names.length - 2);
  assert.throws(() => goRacePartition(["TestAlpha", "TestBeta", "TestAlpha"], 4), /^Error: Duplicate Go test name: TestAlpha$/);
  for (const k of [1, 9, 2.5, "4"])
    assert.throws(() => goRacePartition(names, k), /shard count must be an integer from 2 to 8/);
});

test("shard selectors list every shard but the catch-all, which skips the others", () => {
  const { parts } = goRacePartition(["TestAlpha", "TestBeta", "TestGamma", "TestDelta"], 4);
  assert.deepEqual(goRaceShardSelectors(parts), [
    null,
    ["-run", "^(TestAlpha|TestDelta)$"],
    ["-run", "^(TestGamma)$"],
    ["-skip", "^(TestAlpha|TestDelta|TestGamma)$"],
  ]);
  // No name outside the catch-all: it still starts, with nothing to skip.
  assert.deepEqual(goRaceShardSelectors([[], ["TestBeta"]]), [null, []]);
  assertGoRacePartition(["TestAlpha", "TestBeta", "TestGamma", "TestDelta"], parts);
  const refused = (names, lists, pattern) =>
    assert.throws(() => assertGoRacePartition(names, lists), pattern);
  refused(["TestA", "TestB"], [["TestA"], []], /^Error: go-race shards do not hold every test exactly once: missing: TestB$/);
  refused(["TestA", "TestB"], [["TestA"], ["TestA", "TestB"]], /more than once: TestA$/);
  refused(["TestA"], [["TestA"], ["TestC"]], /unlisted: TestC$/);
  refused(["TestA", "TestA"], [["TestA"], []], /the test list repeats a name$/);
});

test("the source rule names every top-level Test, Fuzz and Example function except TestMain", () => {
  const names = goRaceTestNames([
    'package store\n\nfunc TestOne(t *testing.T) {}\nfunc TestMain(m *testing.M) {}\nfunc Test(t *testing.T) {}\n' +
      "func Testlower(t *testing.T) {}\nfunc (s *suite) TestMethod(t *testing.T) {}\n\tfunc TestIndented(t *testing.T) {}\n" +
      "func helperTest() {}\nfunc Test_underscore(t *testing.T) {}\nfunc Test2(t *testing.T) {}\n",
    "func FuzzThree(f *testing.F) {}\nfunc ExampleFour() {}\nfunc Example() {}\nfunc Example_suffix() {}\nfunc Examples() {}\nfunc BenchmarkFive(b *testing.B) {}\nfunc TestOne(t *testing.T) {}\n",
  ]);
  assert.deepEqual(names, [
    "TestOne", "Test", "Test_underscore", "Test2",
    "FuzzThree", "ExampleFour", "Example", "Example_suffix", "TestOne",
  ]);
});

test("run counts read top-level run events only, and exactly-once names what is wrong", () => {
  const event = (Action, Test) => JSON.stringify({ Action, Package: "fixture/store", ...(Test ? { Test } : {}) });
  const counts = goRaceRunCounts([
    event("start"), event("run", "TestA"), event("output", "TestA"), event("run", "TestA/sub"),
    event("pause", "TestA"), event("cont", "TestA"), event("pass", "TestA"),
    event("run", "FuzzB"), event("run", "FuzzB/seed#0"), event("run", "ExampleC"),
    "not json", event("pass"),
  ]);
  assert.deepEqual([...counts], [["TestA", 1], ["FuzzB", 1], ["ExampleC", 1]]);
  assertRanExactlyOnce(["TestA", "FuzzB", "ExampleC"], counts);
  assert.throws(
    () => assertRanExactlyOnce(["TestA", "TestMissing"], goRaceRunCounts([event("run", "TestA"), event("run", "TestA"), event("run", "TestExtra")])),
    /^Error: go-race shards did not run every test exactly once: missing: TestMissing; more than once: TestA; unlisted: TestExtra$/,
  );
});

test("with approved shards every go-race argv has one prefix, and without the key the argv is unchanged", () => {
  assert.deepEqual(matrix.goRaceShards, { "./internal/store": 4 });
  const race = (m, owned) => selectChecks(m, owned, []).find((c) => c.id === "go-race").argv;
  const lists = {
    "hub/internal/store/migrate.go": ["./internal/store"],
    "hub/internal/api/releases.go": ["./internal/api"],
    "hub/go.mod": ["./..."],
  };
  for (const [owned, packages] of Object.entries(lists)) {
    const argv = race(matrix, [owned]);
    assert.deepEqual(argv.slice(0, 5), shardPrefix(), owned);
    assert.deepEqual(argv.slice(5), packages, owned);
  }
  assert.deepEqual(
    race(matrix, ["hub/internal/api/releases.go", "hub/internal/store/migrate.go"]),
    [...shardPrefix(), "./internal/api", "./internal/store"],
  );
  // The key changes go-race only.
  const { goRaceShards, ...legacy } = matrix;
  const others = (m) => selectChecks(m, ["hub/internal/store/migrate.go", "client/app.js"], []).filter((c) => c.id !== "go-race");
  assert.deepEqual(others(matrix), others(legacy));
  assert.deepEqual(race(legacy, ["hub/internal/store/migrate.go"]), ["go", "test", "-race", "-timeout=45m", "./internal/store"]);
  assert.deepEqual(race(legacy, ["hub/internal/api/releases.go"]), ["go", "test", "-race", "-timeout=45m", "./internal/api"]);
  assert.deepEqual(race(legacy, ["hub/go.mod"]), ["go", "test", "-race", "-timeout=45m", "./..."]);
  for (const shards of [
    null, [], "./internal/store=4", {}, { "./internal/store": 4, "./internal/api": 2 },
    { "./internal/store": 1 }, { "./internal/store": 9 }, { "./internal/store": "4" }, { "./internal/store": 2.5 },
    { "./...": 4 }, { "internal/store": 4 }, { "./internal/../store": 4 }, { "./internal//store": 4 },
    { "./internal/store -run=X": 4 }, { "./": 4 },
  ])
    assert.throws(
      () => selectChecks({ ...matrix, goRaceShards: shards }, ["hub/cmd/tt/main.go"], []),
      /^Error: Invalid matrix goRaceShards$/,
      JSON.stringify(shards),
    );
});

test("a go-race that will shard holds slots for K race processes, capped at the job count", () => {
  assert.equal(matrixRunner.GO_RACE_PROCESS_BYTES, 1024 ** 3);
  assert.equal(matrixRunner.JOB_SLOT_BYTES, 3 * 1024 ** 3);
  assert.equal(defaultJobs(10, 16 * 1024 ** 3), 5);
  assert.deepEqual(goRaceShardSpec(shardedCheck(4, "./internal/store")), { package: "./internal/store", k: 4 });
  assert.deepEqual(goRaceShardSpec(shardedCheck(4, "./...")), { package: "./internal/store", k: 4 });
  assert.equal(goRaceShardSpec(shardedCheck(4, "./internal/api")), null, "this list will not shard");
  assert.equal(goRaceShardSpec({ ...shardedCheck(4, "./internal/store"), id: "go-test" }), null);
  assert.equal(checkWeight(shardedCheck(4, "./internal/store"), 5), 2);
  assert.equal(checkWeight(shardedCheck(4, "./..."), 5), 2);
  assert.equal(checkWeight(shardedCheck(6, "./internal/store"), 5), 2);
  assert.equal(checkWeight(shardedCheck(7, "./internal/store"), 5), 3);
  assert.equal(checkWeight(shardedCheck(8, "./internal/api", "./internal/store"), 5), 3);
  assert.equal(checkWeight(shardedCheck(4, "./internal/api"), 5), 2, "a go-race that will not shard keeps weight 2");
  assert.equal(checkWeight(goFixture("go-race", ["go", "test", "-race", "-timeout=45m", "./internal/store"]), 5), 2);
  assert.equal(checkWeight(shardedCheck(7, "./internal/store"), 2), 2, "capped at the job count");
  assert.equal(checkWeight(shardedCheck(4, "./internal/store"), 1), 1);
  assert.equal(checkWeight(goFixture("go-test", ["go", "test", "./..."]), 5), 1);
  // The sharded check is still a Go check: it holds the Go lane.
  assert(executionLocks(shardedCheck(4, "./internal/store")).includes("go"));
});

test("an accepted go test race is rebuilt as shards only under a changed matrix, and sharded lists union", () => {
  const environment = { VERIFICATION_TIMEOUT_MS: "3000000" };
  const old = { ...goFixture("go-race", ["go", "test", "-race", "-timeout=45m", "./cmd/tt"]), environment };
  const sharded = (...packages) => ({ ...shardedCheck(4, ...packages), environment });
  assert.throws(
    () => matrixRunner.keepAcceptedChecks([sharded("./internal/store")], [old]),
    /^Error: Accepted check differs from the selected one: go-race$/,
  );
  const rebuilt = matrixRunner.keepAcceptedChecks([sharded("./internal/store")], [old], undefined, { matrixChanged: true });
  assert.deepEqual(rebuilt.checks[0].argv, [...shardPrefix(), "./cmd/tt", "./internal/store"]);
  assert.deepEqual(rebuilt.preserved.rebuilt, ["go-race"]);
  const widened = matrixRunner.keepAcceptedChecks([sharded("./internal/store")], [sharded("./internal/api")]);
  assert.deepEqual(widened.checks[0].argv, [...shardPrefix(), "./internal/api", "./internal/store"]);
  assert.deepEqual(widened.preserved.widened, ["go-race"]);
  assert.deepEqual(
    matrixRunner.keepAcceptedChecks([sharded("./internal/store")], [sharded("./...")]).checks[0].argv,
    [...shardPrefix(), "./..."],
  );
  // Another shard count is another prefix: refused under one matrix.
  assert.throws(
    () => matrixRunner.keepAcceptedChecks([sharded("./internal/store")], [{ ...shardedCheck(5, "./internal/store"), environment }]),
    /^Error: Accepted check differs from the selected one: go-race$/,
  );
});

function shardPlanFixture(t) {
  const f = fixture(t);
  writeFileSync(
    join(f.cwd, "verification/matrix.json"),
    JSON.stringify({
      maxAttempts: 3,
      knownFailures: [],
      version: 1,
      browserSuites: [],
      excludedBrowserSuites: [],
      rules: [
        { prefixes: ["hub/"], groups: ["go"] },
        { prefixes: ["docs/"], groups: ["unit"] },
      ],
      goTestFlags: ["-timeout=5m"],
      goRaceShards: { "./internal/store": 2 },
    }),
  );
  const files = {
    "hub/go.mod": "module fixture\n\ngo 1.24.0\n",
    "hub/internal/api/api.go": "package api\n",
    "hub/internal/store/store.go": "package store\n",
    "hub/internal/store/a_test.go":
      'package store\n\nimport "testing"\n\nfunc TestMain(m *testing.M) { m.Run() }\nfunc TestOne(t *testing.T) {}\nfunc TestTwo(t *testing.T) {}\nfunc FuzzThree(f *testing.F) {}\n',
    "hub/internal/store/b_test.go":
      'package store\n\nimport "testing"\n\nfunc ExampleFour() {}\nfunc Test(t *testing.T) {}\nfunc helperTest() {}\nfunc Testlower() {}\n',
    // Another package's tests are not the shard package's.
    "hub/internal/store/sub/sub_test.go": 'package sub\n\nimport "testing"\n\nfunc TestSub(t *testing.T) {}\n',
    "hub/internal/store/notes_test.txt": "func TestNotGo(t *testing.T) {}\n",
  };
  for (const path of Object.keys(files)) commitChange(f, path, files[path], "add " + path);
  const base = f.git("rev-parse", "HEAD");
  const context = (commit, owned) => ({
    operationKey: "fixture",
    repository: "fixture",
    baseCommit: base,
    commit,
    owned,
    verifierAgentId: "fixture",
    verifierRunId: "fixture",
    approvedMatrixDigest: digest(readFileSync(join(f.cwd, "verification/matrix.json"), "utf8")),
    matrixApprovalMessageSeq: 1,
  });
  return { ...f, base, context, names: ["TestOne", "TestTwo", "FuzzThree", "ExampleFour", "Test"] };
}

test("plan creation partitions the shard package's tests at the commit and prints K, the count and the digest", (t) => {
  const f = shardPlanFixture(t);
  const store = commitChange(f, "hub/internal/store/store.go", "package store\n\nvar Changed = 1\n", "store change");
  const context = f.context(store, ["hub/internal/store/store.go"]);
  const { plan, goRaceShards } = matrixRunner.planWithPreservation(context, f.cwd);
  const race = plan.checks.find((c) => c.id === "go-race");
  assert.deepEqual(race.argv, [
    "node", "../scripts/verify-matrix.mjs", "go-race", "-timeout=5m", "-shards=./internal/store=2", "./internal/store",
  ]);
  assert.equal(race.environment.VERIFICATION_BASE_COMMIT, f.base);
  for (const check of plan.checks)
    assert.equal(check.environment.VERIFICATION_BASE_COMMIT, check.cwd === "hub" ? f.base : undefined, check.id);
  assert.equal(
    makeTargetedPlan({ baseCommit: f.base, commit: store }, f.cwd).checks.find((c) => c.id === "go-race").environment.VERIFICATION_BASE_COMMIT,
    f.base,
  );
  // The partition: disjoint lists whose union is the source list, each name
  // in the shard its hash gives.
  const { parts, digest: partitionDigest } = goRacePartition(f.names, 2);
  assert.deepEqual(parts.flat().sort(), [...f.names].sort());
  assert.equal(new Set(parts.flat()).size, f.names.length);
  parts.forEach((part, i) => part.forEach((name) => assert.equal(goRaceShardOf(name, 2), i)));
  assert.equal(partitionDigest, createHash("sha256").update(JSON.stringify(parts)).digest("hex"));
  const line = `go-race shards: ./internal/store K=2 tests=5 partition sha256:${partitionDigest}`;
  assert.match(line, /^go-race shards: \.\/internal\/store K=2 tests=5 partition sha256:[a-f0-9]{64}$/);
  assert.deepEqual(goRaceShards, { package: "./internal/store", k: 2, tests: 5, digest: partitionDigest, line });
  assert.equal("goRaceShards" in plan, false, "the plan itself carries no partition");

  const directory = tempDir(t, "verification-shard-plan-");
  const planMode = (input) => {
    writeFileSync(join(directory, "context.json"), JSON.stringify(input));
    return spawnSync(
      process.execPath,
      [matrixScript, "plan", join(directory, "context.json"), join(directory, "plan.json")],
      { cwd: f.cwd, encoding: "utf8" },
    );
  };
  let result = planMode(context);
  assert.equal(result.status, 0, result.stderr);
  assert.equal(result.stderr, line + "\n");
  assert.deepEqual(JSON.parse(readFileSync(join(directory, "plan.json"), "utf8")), plan);

  // A go-race that will not shard prints nothing; ./... does.
  const api = commitChange(f, "hub/internal/api/api.go", "package api\n\nvar Changed = 1\n", "api change");
  result = planMode({ ...f.context(api, ["hub/internal/api/api.go"]), baseCommit: store });
  assert.equal(result.status, 0, result.stderr);
  assert.equal(result.stderr, "");
  const every = commitChange(f, "hub/go.mod", "module fixture\n\ngo 1.24.0\n\n// changed\n", "module change");
  result = planMode({ ...f.context(every, ["hub/go.mod"]), baseCommit: api });
  assert.deepEqual(raceArgv(JSON.parse(readFileSync(join(directory, "plan.json"), "utf8"))).slice(-1), ["./..."]);
  assert.equal(result.status, 0, result.stderr);
  assert.equal(result.stderr, line + "\n");

  // Plan creation fails when the partition does not hold every test exactly
  // once: a dropped name, a name in two shards, and a real duplicate.
  const tampered = (change) => (names, k) => {
    const made = goRacePartition(names, k);
    change(made.parts);
    return made;
  };
  assert.throws(
    () => matrixRunner.planWithPreservation(context, f.cwd, {
      partition: tampered((lists) => lists.forEach((list, i) => (lists[i] = list.filter((name) => name !== "TestTwo")))),
    }),
    /^Error: go-race shards do not hold every test exactly once: missing: TestTwo$/,
  );
  assert.throws(
    () => matrixRunner.planWithPreservation(context, f.cwd, {
      partition: tampered((lists) => lists[1 - goRaceShardOf("TestTwo", 2)].push("TestTwo")),
    }),
    /^Error: go-race shards do not hold every test exactly once: more than once: TestTwo$/,
  );
  const duplicate = commitChange(f, "hub/internal/store/c_test.go", 'package store\n\nimport "testing"\n\nfunc TestOne(t *testing.T) {}\n', "duplicate test");
  result = planMode(f.context(duplicate, ["hub/internal/store/c_test.go"]));
  assert.equal(result.status, 1);
  assert.equal(result.stderr, "Duplicate Go test name: TestOne\n");
});

// A real Go module run through the go-race command. Every fixture test appends
// "package name pid nanoseconds run" to FIXTURE_LOG, and a go wrapper on PATH
// records each go command line before running it.
const fixtureGoTests = {
  "go.mod": "module fixture\n\ngo 1.24.0\n",
  "internal/fixturelog/log.go": `package fixturelog

import (
	"fmt"
	"os"
	"time"
)

func Record(pkg, name string) {
	f, err := os.OpenFile(os.Getenv("FIXTURE_LOG"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		panic(err)
	}
	defer f.Close()
	// Reading the run's number keeps go test from reusing a cached result.
	fmt.Fprintf(f, "%s %s %d %d %s\\n", pkg, name, os.Getpid(), time.Now().UnixNano(), os.Getenv("FIXTURE_RUN"))
	time.Sleep(20 * time.Millisecond)
}
`,
  "store/store.go": 'package store\n\nfunc Hello() string { return "hello" }\n',
  "store/store_test.go": `package store

import (
	"fmt"
	"os"
	"testing"
	"time"

	"fixture/internal/fixturelog"
)

func TestAlpha(t *testing.T) {
	fixturelog.Record("store", "TestAlpha")
	if os.Getenv("FIXTURE_SLEEP") != "" {
		time.Sleep(time.Minute)
	}
}
func TestBeta(t *testing.T) {
	fixturelog.Record("store", "TestBeta")
	t.Run("one", func(t *testing.T) {})
	t.Run("two", func(t *testing.T) {})
}
func TestGamma(t *testing.T) { fixturelog.Record("store", "TestGamma") }
func TestDelta(t *testing.T) {
	fixturelog.Record("store", "TestDelta")
	if os.Getenv("FIXTURE_FAIL") == "store" {
		t.Fatal("fixture failure")
	}
}
func TestEpsilon(t *testing.T) { fixturelog.Record("store", "TestEpsilon") }
func FuzzSeed(f *testing.F) {
	fixturelog.Record("store", "FuzzSeed")
	f.Add("seed")
	f.Fuzz(func(t *testing.T, s string) {})
}
func ExampleHello() {
	fixturelog.Record("store", "ExampleHello")
	fmt.Println(Hello())
	// Output: hello
}
`,
  "other/other.go": "package other\n",
  "other/other_test.go": `package other

import (
	"os"
	"testing"

	"fixture/internal/fixturelog"
)

func TestOther(t *testing.T) {
	fixturelog.Record("other", "TestOther")
	if os.Getenv("FIXTURE_FAIL") == "other" {
		t.Fatal("fixture failure")
	}
}
`,
  "third/third.go": "package third\n",
  "third/third_test.go": `package third

import (
	"testing"

	"fixture/internal/fixturelog"
)

func TestThird(t *testing.T) { fixturelog.Record("third", "TestThird") }
`,
};
const fixtureStoreTests = ["TestAlpha", "TestBeta", "TestGamma", "TestDelta", "TestEpsilon", "FuzzSeed", "ExampleHello"];
function goRaceFixture(t) {
  const cwd = tempDir(t, "verification-go-race-"),
    tools = tempDir(t, "verification-go-race-tools-");
  for (const [path, content] of Object.entries(fixtureGoTests)) {
    mkdirSync(join(cwd, path, ".."), { recursive: true });
    writeFileSync(join(cwd, path), content);
  }
  const realGo = execFileSync("sh", ["-c", "command -v go"], { encoding: "utf8" }).trim();
  writeFileSync(
    join(tools, "go"),
    `#!/bin/sh\nprintf '%s\\n' "$*" >> "$FIXTURE_GO_ARGV"\nexec '${realGo}' "$@"\n`,
    { mode: 0o755 },
  );
  const log = join(tools, "tests.log"),
    commands = join(tools, "go-argv.log");
  const environment = (extra = {}) => ({
    ...process.env,
    PATH: tools + ":" + process.env.PATH,
    FIXTURE_LOG: log,
    FIXTURE_GO_ARGV: commands,
    VERIFICATION_JOBS: "5",
    ...extra,
  });
  const lines = (path) => (existsSync(path) ? readFileSync(path, "utf8").split("\n").filter(Boolean) : []);
  const entries = () =>
    lines(log).map((line) => {
      const [pkg, name, pid, at] = line.split(" ");
      return { pkg, name, pid, at: BigInt(at) };
    });
  let runs = 0;
  const run = (args, extra = {}) => {
    extra = { FIXTURE_RUN: `${process.pid}-${++runs}`, ...extra };
    rmSync(log, { force: true });
    rmSync(commands, { force: true });
    const result = spawnSync(process.execPath, [matrixScript, "go-race", ...args], {
      cwd,
      env: environment(extra),
      encoding: "utf8",
      timeout: 240000,
    });
    return { ...result, commands: lines(commands), entries: entries() };
  };
  return { cwd, run, environment, entries, log };
}
const bigMax = (values) => values.reduce((a, b) => (a > b ? a : b)),
  bigMin = (values) => values.reduce((a, b) => (a < b ? a : b));
// The fixture's partition for K=2, from the hash rule.
const fixtureShards = [0, 1].map((shard) => fixtureStoreTests.filter((name) => goRaceShardOf(name, 2) === shard).sort());
const fixtureShardCommands = [
  `test -race -json -count=1 -timeout=5m -run ^(${fixtureShards[0].join("|")})$ ./store`,
  `test -race -json -count=1 -timeout=5m -skip ^(${fixtureShards[0].join("|")})$ ./store`,
];

test("go-race command runs the other packages first and then every test of the shard package exactly once", { timeout: 600000 }, (t) => {
  const f = goRaceFixture(t);
  assert.deepEqual(fixtureShards, [
    ["ExampleHello", "TestEpsilon", "TestGamma"],
    ["FuzzSeed", "TestAlpha", "TestBeta", "TestDelta"],
  ]);
  const digestLine = `partition sha256:${goRacePartition(fixtureStoreTests, 2).digest}`;
  const otherThenShards = (result, others) => {
    const store = result.entries.filter((e) => e.pkg === "store"),
      rest = result.entries.filter((e) => e.pkg !== "store");
    assert.deepEqual(rest.map((e) => e.pkg).sort(), others, "each other package ran once");
    assert.deepEqual(store.map((e) => e.name).sort(), [...fixtureStoreTests].sort(), "each store test ran once");
    assert(bigMax(rest.map((e) => e.at)) < bigMin(store.map((e) => e.at)), "the other packages ended before any shard test started");
    const pids = [...new Set(store.map((e) => e.pid))];
    assert.equal(pids.length, 2, "the shard package ran as two shard processes");
    for (const pid of pids)
      assert(
        fixtureShards.some((names) => canonicalNames(store.filter((e) => e.pid === pid)) === names.join()),
        "each process ran exactly one shard's tests",
      );
    return pids.map((pid) => store.filter((e) => e.pid === pid).map((e) => e.at));
  };
  const canonicalNames = (list) => list.map((e) => e.name).sort().join();

  // Every package: ./... is expanded, the shard package taken out.
  let result = f.run(["-timeout=5m", "-shards=./store=2", "./..."]);
  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.deepEqual(result.commands.slice(0, 3), [
    "list -f {{.Dir}} ./...",
    "test -race -timeout=5m ./internal/fixturelog ./other ./third",
    "test -race -list . ./store",
  ]);
  assert.deepEqual(result.commands.slice(3).sort(), [...fixtureShardCommands].sort());
  otherThenShards(result, ["other", "third"]);
  const out = result.stdout;
  const at = (text) => {
    assert(out.includes(text), text + "\n" + out);
    return out.indexOf(text);
  };
  assert.match(out, /^go-race other packages: 3 packages, [0-9.]+ s, exit 0$/m);
  assert(at("go-race other packages:") < at(`go-race shards: ./store K=2 tests=7 ${digestLine}\n`));
  assert(at(`tests=7 ${digestLine}\n`) < at("go-race shard 1/2:"));
  assert(at("go-race shard 1/2:") < at("go-race shard 2/2:"));
  assert.match(out, /^go-race shard 1\/2: 3 tests, [0-9.]+ s, exit 0$/m);
  assert.match(out, /^go-race shard 2\/2: 4 tests, [0-9.]+ s, exit 0$/m);
  assert(
    out.endsWith(`go-race shards: ./store K=2 tests=7 ran-once=7 ${digestLine} peak-shard-processes=2\n`),
    out,
  );
  // The log keeps every RUN line: each top-level name once, in its shard's
  // part of the output, and the subtests.
  for (const name of fixtureStoreTests) {
    assert.equal(out.split(`=== RUN   ${name}\n`).length, 2, name);
    const shard = fixtureShards[0].includes(name) ? 1 : 2;
    assert.equal(at(`=== RUN   ${name}\n`) < at(`go-race shard ${shard}/2:`), true, name);
    if (shard === 2) assert(at(`=== RUN   ${name}\n`) > at("go-race shard 1/2:"), name);
  }
  assert(out.includes("=== RUN   TestBeta/one\n") && out.includes("=== RUN   FuzzSeed/seed#0\n"), out);
  assert(out.includes("ok  \tfixture/other") && out.includes("ok  \tfixture/third"));

  // A mixed list on a one-job host: the other package first, then the shards
  // in turn.
  result = f.run(["-timeout=5m", "-shards=./store=2", "./store", "./other"], { VERIFICATION_JOBS: "1" });
  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.deepEqual(result.commands, [
    "test -race -timeout=5m ./other",
    "test -race -list . ./store",
    ...fixtureShardCommands,
  ]);
  const [first, second] = otherThenShards(result, ["other"]);
  assert(bigMax(first) < bigMin(second) || bigMax(second) < bigMin(first), "with one job the shards do not overlap");
  assert.match(result.stdout, /^go-race other packages: 1 packages, [0-9.]+ s, exit 0$/m);
  assert(result.stdout.endsWith(`tests=7 ran-once=7 ${digestLine} peak-shard-processes=1\n`), result.stdout);

  // The shard package alone: no other-package phase.
  result = f.run(["-timeout=5m", "-shards=./store=2", "./store"]);
  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.deepEqual(result.commands.slice(0, 1), ["test -race -list . ./store"]);
  assert.equal(result.commands.length, 3);
  assert(!result.stdout.includes("go-race other packages"));
  assert(result.stdout.endsWith(`tests=7 ran-once=7 ${digestLine} peak-shard-processes=2\n`), result.stdout);

  // A list without the shard package, and a command without the flag, are one
  // plain go test -race.
  result = f.run(["-timeout=5m", "-shards=./store=2", "./other", "./third"]);
  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.deepEqual(result.commands, ["test -race -timeout=5m ./other ./third"]);
  assert(!result.stdout.includes("go-race"), result.stdout);
  result = f.run(["./other"]);
  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.deepEqual(result.commands, ["test -race ./other"]);

  // Anything but the two flags and packages is a usage error; nothing runs.
  for (const args of [
    [], ["-timeout=5m"], ["-count=2", "./store"], ["-run=TestAlpha", "./store"], ["-shards=./store=9", "./store"],
    ["-shards=./store=1", "./store"], ["-shards=./...=2", "./..."], ["-shards=./store=2", "-shards=./other=2", "./store"],
    ["-timeout=5s", "./store"], ["./store", "-run=TestAlpha"], ["-shards=./store=2", "store"],
  ]) {
    result = f.run(args);
    assert.equal(result.status, 2, args.join(" "));
    assert.match(result.stderr, /^Usage: node \.\.\/scripts\/verify-matrix\.mjs go-race /);
    assert.deepEqual(result.commands, []);
  }
});

test("go-race command fails closed: a failing package, a failing shard, and a test list that disagrees with the source", { timeout: 600000 }, (t) => {
  const f = goRaceFixture(t);
  // A failing other package: no shard starts.
  let result = f.run(["-timeout=5m", "-shards=./store=2", "./..."], { FIXTURE_FAIL: "other" });
  assert.equal(result.status, 1, result.stdout + result.stderr);
  assert.match(result.stdout, /^go-race other packages: 3 packages, [0-9.]+ s, exit 1$/m);
  assert(!result.stdout.includes("go-race shard"), result.stdout);
  assert.deepEqual(result.commands, [
    "list -f {{.Dir}} ./...",
    "test -race -timeout=5m ./internal/fixturelog ./other ./third",
  ]);
  assert.deepEqual(result.entries.filter((e) => e.pkg === "store"), []);

  // A failing test fails its shard and the command; the count still holds.
  result = f.run(["-timeout=5m", "-shards=./store=2", "./store"], { FIXTURE_FAIL: "store" });
  assert.equal(result.status, 1, result.stdout + result.stderr);
  assert.match(result.stdout, /^--- FAIL: TestDelta /m);
  assert.match(result.stdout, /^go-race shard 1\/2: 3 tests, [0-9.]+ s, exit 0$/m);
  assert.match(result.stdout, /^go-race shard 2\/2: 4 tests, [0-9.]+ s, exit 1$/m);
  assert.match(result.stdout, /tests=7 ran-once=7 partition sha256:[a-f0-9]{64} peak-shard-processes=2\n$/);

  // A test in the source that go test -list does not list stops the run
  // before any shard: the catch-all must not hide a disagreement.
  const disagrees = (file, content, pattern) => {
    writeFileSync(join(f.cwd, "store", file), content);
    try {
      result = f.run(["-timeout=5m", "-shards=./store=2", "./store"]);
    } finally {
      rmSync(join(f.cwd, "store", file));
    }
    assert.equal(result.status, 1, result.stdout + result.stderr);
    assert.match(result.stderr, pattern);
    assert.deepEqual(result.commands, ["test -race -list . ./store"]);
    assert.deepEqual(result.entries, []);
    assert(!result.stdout.includes("go-race shard"), result.stdout);
  };
  disagrees(
    "hidden_test.go",
    '//go:build never\n\npackage store\n\nimport "testing"\n\nfunc TestHidden(t *testing.T) {}\n',
    /^go-race: the test source of \.\/store and go test -list disagree; only in the source: TestHidden$/m,
  );
  // Go compiles an Example without an output comment but never runs it.
  disagrees(
    "silent_test.go",
    "package store\n\nfunc Example_silent() {}\n",
    /^go-race: the test source of \.\/store and go test -list disagree; only in the source: Example_silent$/m,
  );
  // A package that does not build fails at the list.
  writeFileSync(join(f.cwd, "store", "broken_test.go"), "package store\n\nfunc TestBroken(t *testing.T) { undefined() }\n");
  result = f.run(["-timeout=5m", "-shards=./store=2", "./store"]);
  rmSync(join(f.cwd, "store", "broken_test.go"));
  assert.notEqual(result.status, 0);
  assert.deepEqual(result.commands, ["test -race -list . ./store"]);
  assert(!result.stdout.includes("go-race shard"), result.stdout);
});

// Every process with its group and state, and whether its environment carries
// marker. Read the way the runner's home sweep reads environments, which hold
// other processes' credentials: matched here and never kept.
function markedProcesses(marker) {
  const ps = (...args) => {
    const result = spawnSync("ps", args, { encoding: "utf8", maxBuffer: 256 * 1024 * 1024 });
    if (result.status !== 0) throw new Error("ps failed: " + (result.error?.message || result.stderr));
    return result.stdout.split("\n");
  };
  const row = (line) => /^\s*(\d+)\s+(\d+)\s+(\S+)(?:\s(.*))?$/.exec(line);
  if (process.platform === "darwin")
    return ps("-axww", "-E", "-o", "pid=,pgid=,state=,command=")
      .map(row)
      .filter(Boolean)
      .map(([, pid, pgid, state, rest = ""]) => ({
        pid: Number(pid),
        pgid: Number(pgid),
        state,
        marked: rest.includes(marker),
      }));
  if (process.platform === "linux")
    return ps("-ax", "-o", "pid=,pgid=,state=")
      .map(row)
      .filter(Boolean)
      .map(([, pid, pgid, state]) => {
        let marked = false;
        try {
          marked = readFileSync(`/proc/${pid}/environ`, "utf8").split("\0").includes(marker);
        } catch {}
        return { pid: Number(pid), pgid: Number(pgid), state, marked };
      });
  throw new Error("Unsupported platform for the process group listing");
}
// The live processes of a detached child's group that this test started. A
// bare group number says nothing about whose it is: once the group's last
// process is reaped the number can name a stranger's group, and a group that
// holds only zombies answers EPERM. So a member is ours only while it is in
// the group, is not a zombie and inherited marker ("NAME=value") from the
// child's environment. After alive() has once answered false the group is
// over for good, and stop() signals nothing. stop() SIGKILLs each member by
// its own pid, never a group.
function ownedGroup(pgid, marker, { list = markedProcesses, kill = process.kill } = {}) {
  let over = false;
  const members = () =>
    over
      ? []
      : list(marker)
          .filter((p) => p.pgid === pgid && !p.state.startsWith("Z") && p.marked)
          .map((p) => p.pid);
  return {
    members,
    alive() {
      if (!over && !members().length) over = true;
      return !over;
    },
    stop() {
      for (const pid of members())
        try {
          kill(pid, "SIGKILL");
        } catch (error) {
          if (error.code !== "ESRCH") throw error;
        }
    },
  };
}
test("owned group probe answers only for marked live members and never signals a group", () => {
  // A kernel over a process table: a group of zombies answers EPERM, as macOS does.
  const kernel = (table) => {
    const signals = [];
    const kill = (pid, signal) => {
      const targets = table.filter((p) => (pid < 0 ? p.pgid === -pid : p.pid === pid));
      const refuse = (code) => {
        throw Object.assign(new Error("kill " + code), { code });
      };
      if (!targets.length) refuse("ESRCH");
      if (targets.every((p) => p.state.startsWith("Z"))) refuse("EPERM");
      signals.push([pid, signal]);
    };
    return { table, signals, group: ownedGroup(500, "M=1", { list: () => table.map((p) => ({ ...p })), kill }) };
  };
  const mine = { pid: 501, pgid: 500, state: "S", marked: true },
    stranger = { pid: 502, pgid: 500, state: "R", marked: false },
    elsewhere = { pid: 503, pgid: 600, state: "S", marked: true };
  // A marked live member: alive, and stopped by its own pid.
  let k = kernel([mine, stranger, elsewhere]);
  assert.equal(k.group.alive(), true);
  assert.deepEqual(k.group.members(), [501]);
  k.group.stop();
  assert.deepEqual(k.signals, [[501, "SIGKILL"]]);
  // A same-user stranger holding the group number, before any false answer.
  k = kernel([stranger, elsewhere]);
  k.group.stop();
  assert.deepEqual(k.signals, []);
  assert.equal(k.group.alive(), false);
  k.group.stop();
  assert.deepEqual(k.signals, []);
  // Only a zombie left in the group: an answer, not EPERM.
  k = kernel([{ ...mine, state: "Z" }]);
  assert.equal(k.group.alive(), false);
  k.group.stop();
  assert.deepEqual(k.signals, []);
  // After a false answer a marked process in the reused group is not ours.
  k = kernel([]);
  assert.equal(k.group.alive(), false);
  k.table.push(mine);
  assert.equal(k.group.alive(), false);
  assert.deepEqual(k.group.members(), []);
  k.group.stop();
  assert.deepEqual(k.signals, []);
  // A member that exits between the listing and the signal is not an error.
  const racing = ownedGroup(500, "M=1", {
    list: () => [{ ...mine }],
    kill: () => {
      throw Object.assign(new Error("kill ESRCH"), { code: "ESRCH" });
    },
  });
  racing.stop();
});
test("go-race command stopped through its process group leaves no shard process behind", { timeout: 600000 }, async (t) => {
  const f = goRaceFixture(t);
  rmSync(f.log, { force: true });
  // Every descendant inherits the marker, as it does FIXTURE_RUN.
  const marker = randomUUID();
  const child = spawn(process.execPath, [matrixScript, "go-race", "-timeout=5m", "-shards=./store=2", "./store"], {
    cwd: f.cwd,
    env: f.environment({ FIXTURE_SLEEP: "1", FIXTURE_GROUP_MARKER: marker }),
    detached: true,
    stdio: "ignore",
  });
  const group = ownedGroup(child.pid, "FIXTURE_GROUP_MARKER=" + marker);
  t.after(() => group.stop());
  const exited = once(child, "exit");
  const deadline = Date.now() + 240000;
  while (!f.entries().some((e) => e.name === "TestAlpha")) {
    assert(child.exitCode === null && Date.now() < deadline, "the sleeping shard test never started");
    await new Promise((resolve) => setTimeout(resolve, 50));
  }
  // The listing sees the running check, so a later empty answer means stopped.
  const running = group.members();
  assert(running.includes(child.pid) && running.length > 1, `the group listing misses the running check: ${running}`);
  // What the matrix runner does to a check at its timeout.
  process.kill(-child.pid, "SIGTERM");
  const [code, signal] = await exited;
  assert.equal(signal ?? code, 143);
  for (let i = 0; i < 100 && group.alive(); i++) await new Promise((resolve) => setTimeout(resolve, 50));
  assert.equal(group.alive(), false, "a process of the check's group survived");
});

// wi_19db6841a2a50d57 (order #29140): the verify-matrix runner bundle. Each
// test below is named for the criterion it covers.
test("bundle v8 a targeted run joins the host under its context's item, the one its priority was looked up for", async (t) => {
  const f = fixture(t);
  const targeted = async (context, flags, expected) => {
    const file = join(tempDir(t, "bundle-v8-context-"), "context.json"),
      output = tempDir(t, "bundle-v8-logs-");
    writeFileSync(file, JSON.stringify({ baseCommit: f.base, commit: f.commit, ...context }));
    const before = fakeTTLookups().length;
    const holder = await holdHost("wi_v8_holder");
    const child = matrixCLI(t, f, ["targeted", file, output, "--min-free-bytes", "0", ...flags], { FAKE_TT_PRIORITY: "high" });
    let waiter;
    try {
      await untilHost(() => (waiter = readHostState(hostLockFile()).waiters[0]), "the waiter; saw " + child.text);
    } finally {
      await holder.release();
    }
    const [code] = await once(child, "close");
    assert.equal(code, 0, child.text);
    const request = readJournal(hostLockFile()).find((entry) => entry.id === waiter.id && entry.event === "request");
    const sidecar = JSON.parse(readFileSync(join(output, "host-lock.json"), "utf8"));
    assert.deepEqual(
      {
        waiter: [waiter.item, waiter.kind, waiter.priority, waiter.prioritySource],
        request: request.item,
        sidecar: sidecar.item,
        lookups: fakeTTLookups().slice(before),
      },
      {
        waiter: [expected, "targeted", "high", "item"],
        request: expected,
        sidecar: expected,
        lookups: [["work-items", "get", "--json", expected]],
      },
      JSON.stringify(flags),
    );
  };
  await targeted({ itemId: "wi_ctx" }, [], "wi_ctx");
  await targeted({ itemId: "wi_ctx" }, ["--item", "wi_other"], "wi_ctx");
  await targeted({}, ["--item", "wi_cli"], "wi_cli");
});

test("bundle v6 a targeted run on a rebased fix names the previous candidate, the fix and their merge base", (t) => {
  const f = fixture(t);
  // The previous candidate P sits on base B; the fix was rebuilt on a newer
  // tip B2, so it no longer contains P.
  const previous = f.commit;
  const fix = commitChange(f, "docs/fix.md", "fix\n", "fix on the previous candidate");
  f.git("checkout", "-q", "--detach", f.base);
  commitChange(f, "docs/other.md", "moved\n", "the tip moved");
  const rebased = commitChange(f, "docs/fix.md", "fix\n", "fix after the rebase");
  const refusal = (baseCommit, commit) => {
    try {
      makeTargetedPlan({ baseCommit, commit }, f.cwd);
    } catch (error) {
      return error.message;
    }
    return assert.fail("the targeted plan was not refused");
  };
  const message = refusal(previous, rebased);
  assert.equal(
    message,
    `fix candidate ${rebased} does not contain previous candidate ${previous} (merge base ${f.base}). ` +
      "A targeted run needs the fix committed on top of the previous candidate; after a rebase run the full plan on the new candidate.",
  );
  assert.doesNotMatch(message, /rebase onto the current tip|fast-forward/);
  // Histories that share nothing have no merge base to name.
  const orphan = f.git("commit-tree", "-m", "unrelated", f.git("rev-parse", f.base + "^{tree}"));
  assert.match(refusal(orphan, rebased), new RegExp(`previous candidate ${orphan} \\(merge base none\\)`));
  // A fix committed on top of the previous candidate is planned as before.
  assert.deepEqual(makeTargetedPlan({ baseCommit: previous, commit: fix }, f.cwd).changed, ["docs/fix.md"]);
  // Plans keep their own wording.
  assert.throws(
    () => makePlan({ baseCommit: previous, commit: rebased, owned: ["docs/"] }, f.cwd),
    { message: "candidate is not a fast-forward of its base; rebase onto the current tip" },
  );
});

// A stand-in go for runGoRace: it lists 300 generated tests and, for a shard,
// emits a run event per selected test. STUB_GO_SHARD makes the first shard
// (the one selected with -run) exit 2 after one output line, kill itself, or
// fail after running everything. No Go toolchain and no race load.
function stubGoRace(t) {
  const cwd = tempDir(t, "bundle-v9-"),
    bin = join(cwd, "bin"),
    names = Array.from({ length: 300 }, (_, i) => "TestCase" + String(i).padStart(3, "0"));
  mkdirSync(join(cwd, "pkg"));
  mkdirSync(bin);
  writeFileSync(
    join(cwd, "pkg", "cases_test.go"),
    'package pkg\n\nimport "testing"\n\n' + names.map((name) => `func ${name}(t *testing.T) {}\n`).join(""),
  );
  writeFileSync(
    join(bin, "go"),
    `#!${process.execPath}
const args = process.argv.slice(2), names = ${JSON.stringify(names)};
const say = (event) => console.log(JSON.stringify(event));
if (args.includes("-list")) console.log(names.join("\\n") + "\\nok");
else {
  const run = args.indexOf("-run"), skip = args.indexOf("-skip"), mode = run >= 0 ? process.env.STUB_GO_SHARD : "";
  say({ Action: "output", Output: "stub shard starting\\n" });
  if (mode === "exit2") process.exit(2);
  if (mode === "kill") process.kill(process.pid, "SIGKILL");
  const selected = run >= 0 ? new RegExp(args[run + 1]) : null, skipped = skip >= 0 ? new RegExp(args[skip + 1]) : null;
  for (const name of names)
    if ((!selected || selected.test(name)) && !(skipped && skipped.test(name))) say({ Action: "run", Test: name });
  if (mode === "fail") process.exit(1);
}
`,
    { mode: 0o755 },
  );
  return async (mode) => {
    let text = "";
    const outcome = { text: () => text };
    try {
      outcome.code = await matrixRunner.runGoRace(["-shards=./pkg=2", "./pkg"], {
        cwd,
        env: { ...process.env, PATH: bin + ":" + process.env.PATH, VERIFICATION_JOBS: "2", STUB_GO_SHARD: mode },
        write: (chunk) => (text += chunk),
      });
    } catch (error) {
      outcome.error = error;
    }
    return outcome;
  };
}
test("bundle v9 a race shard that stops early is reported by its shard and exit, with a bounded list of the tests it did not run", async (t) => {
  const run = stubGoRace(t);
  const bounded = (error) => {
    assert(error, "the command did not reject");
    assert(error.message.length < 1000, `${error.message.length} characters`);
    const lists = error.message.split("\n").filter((line) => line.includes("not run (first"));
    assert.equal(lists.length, 1, error.message);
    assert.match(lists[0], /^shard 1\/2 not run \(first 10 of \d+\): (TestCase\d{3}, ){9}TestCase\d{3}$/);
    assert.doesNotMatch(error.message, /did not run every test exactly once/);
  };
  let { error, text } = await run("exit2");
  bounded(error);
  assert.equal(error.exitCode, 2);
  const [first] = error.message.split("\n");
  const [, unrun, own] = /^go-race: shard 1\/2 exited 2 before running (\d+) of its (\d+) tests$/.exec(first) ?? [];
  assert(own > 100 && unrun === own, error.message);
  // The shard's own summary line is still printed before the error.
  assert.match(text(), /^stub shard starting\ngo-race shard 1\/2: 0 tests, [0-9.]+ s, exit 2$/m);

  // A killed shard has no exit code: the command exits 1 and never says -1.
  ({ error } = await run("kill"));
  bounded(error);
  assert.equal(error.exitCode, 1);
  assert.match(error.message.split("\n")[0], /^go-race: shard 1\/2 ended without an exit code before running \d+ of its \d+ tests$/);
  assert.doesNotMatch(error.message, /-1/);

  // A shard that fails after running every test is an ordinary failure.
  let outcome = await run("fail");
  assert.equal(outcome.error, undefined);
  assert.equal(outcome.code, 1);
  assert.doesNotMatch(outcome.text(), /before running/);
  assert.match(outcome.text(), /tests=300 ran-once=300 /);
  outcome = await run("");
  assert.deepEqual([outcome.error, outcome.code], [undefined, 0]);
});

test("bundle v5 the binary suite list must name exactly the declared suites that import the binary helper", (t) => {
  const helper = 'import { prepareTestBinary } from "./test-binaries.mjs";\n';
  const check = (suites) => {
    const cwd = tempDir(t, "bundle-v5-");
    mkdirSync(join(cwd, "tests"));
    for (const [name, text] of Object.entries(suites)) writeFileSync(join(cwd, "tests", name), text);
    const declared = { browserSuites: Object.keys(suites).map((name) => ({ file: "tests/" + name, mode: "both" })) };
    return () => matrixRunner.assertBinarySuites(declared, cwd);
  };
  // A suite that starts importing the helper without joining the list.
  assert.throws(check({ "new-thing-browser.mjs": helper, "task-form-browser.mjs": helper }), {
    message:
      "Binary suite list disagrees with the suites that import tests/test-binaries.mjs; helper users not listed: tests/new-thing-browser.mjs",
  });
  assert.throws(
    check({ "lazy-browser.mjs": 'const { prepareTestBinary } = await import("./test-binaries.mjs");\n' }),
    /helper users not listed: tests\/lazy-browser\.mjs$/,
  );
  // A listed suite that no longer imports it.
  assert.throws(check({ "task-form-browser.mjs": "// no helper\n", "plain-browser.mjs": "" }), {
    message:
      "Binary suite list disagrees with the suites that import tests/test-binaries.mjs; listed without the helper: tests/task-form-browser.mjs",
  });
  assert.throws(
    check({ "new-thing-browser.mjs": helper, "task-form-browser.mjs": "" }),
    /; helper users not listed: tests\/new-thing-browser\.mjs; listed without the helper: tests\/task-form-browser\.mjs$/,
  );
  // Agreement, with listed names the fixture does not declare ignored.
  check({ "task-form-browser.mjs": helper, "interventions-browser.mjs": helper, "plain-browser.mjs": "" })();
  check({})();
  // The real tree agrees with the approved matrix.
  matrixRunner.assertBinarySuites(matrix, new URL("..", import.meta.url).pathname);
  // Plans and targeted plans refuse the drift before selecting anything.
  const f = browserFixture(t, { "new-thing-browser.mjs": helper });
  const drift = /helper users not listed: tests\/new-thing-browser\.mjs/;
  assert.throws(() => makePlan({ baseCommit: f.base, commit: f.commit, owned: ["docs/"] }, f.cwd), drift);
  assert.throws(() => makeTargetedPlan({ baseCommit: f.base, commit: f.commit }, f.cwd), drift);
});

test("bundle v4 a browser suite is found by its marker or by importing a marked helper, without a literal launch call", (t) => {
  const inventory = (files, declared, excluded = []) => {
    const cwd = tempDir(t, "bundle-v4-");
    mkdirSync(join(cwd, "tests"));
    for (const [name, text] of Object.entries(files)) writeFileSync(join(cwd, "tests", name), text);
    return () =>
      assertInventory(
        { browserSuites: declared.map((name) => ({ file: "tests/" + name })), excludedBrowserSuites: excluded.map((name) => "tests/" + name) },
        cwd,
      );
  };
  const changed = /Browser inventory changed; update approved matrix/;
  const files = {
    "helper.mjs": "// verify-matrix: browser-helper\nexport const open = (engine) => engine.launch();\n",
    "a-browser.mjs": 'import { open } from "./helper.mjs";\nawait open();\n',
    "b-browser.mjs": "// A suite started some other way.\n// verify-matrix: browser-suite\n",
    "util.mjs": "export const sum = (a, b) => a + b;\n",
    "util-user.mjs": 'import { sum } from "./util.mjs";\n',
    "notes.js": "browser.launch(\n",
  };
  inventory(files, ["a-browser.mjs", "b-browser.mjs"])();
  inventory(files, ["a-browser.mjs"], ["b-browser.mjs"])();
  // A helper-launched or marked suite missing from the matrix is refused.
  assert.throws(inventory(files, ["b-browser.mjs"]), changed);
  assert.throws(inventory(files, ["a-browser.mjs"]), changed);
  // The helper is not a suite, so declaring it is refused too.
  assert.throws(inventory(files, ["a-browser.mjs", "b-browser.mjs", "helper.mjs"]), changed);
  // A dynamic import of the helper counts.
  const dynamic = { ...files, "c-browser.mjs": 'const { open } = await import("./helper.mjs");\n' };
  assert.throws(inventory(dynamic, ["a-browser.mjs", "b-browser.mjs"]), changed);
  inventory(dynamic, ["a-browser.mjs", "b-browser.mjs", "c-browser.mjs"])();
  // A marker counts only as a whole line.
  const mentions = {
    "mention.mjs": "// see verify-matrix: browser-suite above\nconst s = '// verify-matrix: browser-suite';\n  // verify-matrix: browser-suite\n",
    "not-helper.mjs": "// not a // verify-matrix: browser-helper\n",
    "imports-not-helper.mjs": 'import "./not-helper.mjs";\n',
  };
  inventory(mentions, [])();
  // The literal call still classifies a file, as before.
  inventory({ "direct.mjs": "await chromium.launch();\n" }, ["direct.mjs"])();
  assert.throws(inventory({ "direct.mjs": "await chromium.launch();\n" }, []), changed);
});

test("bundle v2 a launch with a bad flag is refused by the launcher and creates or changes nothing", async (t) => {
  const { f, planFile } = plannedFixture(t, "console.log('ran');");
  const context = join(tempDir(t, "bundle-v2-context-"), "context.json");
  writeFileSync(context, JSON.stringify({ baseCommit: f.base, commit: f.git("rev-parse", "HEAD") }));
  const cases = [
    ["run", ["--bogus"], {}, "Unknown verifier run option: --bogus"],
    ["run", ["--min-free-bytes", "0", "--jobs", "0"], {}, "--jobs requires an integer from 1 to 16"],
    ["run", ["--item"], {}, "--item requires an item ID"],
    ["run", ["--priority", "loud"], {}, "--priority must be urgent, high or normal"],
    ["run", ["--host-wait-minutes", "x"], {}, "--host-wait-minutes requires a whole number of minutes from 1 to 1440"],
    ["run", ["--min-free-bytes", "-1"], {}, "--min-free-bytes requires a nonnegative integer byte count"],
    ["run", [], { TAILTERM_MATRIX_PRIORITY: "loud" }, "TAILTERM_MATRIX_PRIORITY must be urgent, high or normal"],
    ["targeted", ["--bogus"], {}, "Unknown verifier run option: --bogus"],
  ];
  for (const [mode, flags, environment, message] of cases) {
    const file = mode === "run" ? planFile : context,
      what = mode + " " + flags.join(" ");
    // Into a path that does not exist: not even the directory is created.
    const missing = join(tempDir(t, "bundle-v2-logs-"), "logs");
    const fresh = launcherCLI(t, f, [mode, file, missing, ...flags], environment);
    assert.equal(await fresh.closed, 1, what + ": " + fresh.text);
    assert.equal(fresh.text, message + "\n", what);
    assert(!existsSync(missing), what + ": the output directory was created");
    // The run itself refuses the same flags with the same message.
    const foreground = matrixCLI(t, f, [mode, file, missing, ...flags], environment);
    assert.equal((await once(foreground, "close"))[0], 1, what);
    assert.equal(foreground.text, message + "\n", what);
    // Into a finished run's directory: its files are byte-identical after.
    const finished = tempDir(t, "bundle-v2-logs-");
    const dead = spawnSync(process.execPath, ["-e", ""]).pid;
    const files = {
      "runner.json": JSON.stringify({ version: 1, pid: dead, argv: [mode, file, finished], cwd: f.cwd, exitCode: 0 }) + "\n",
      "runner.log": "an earlier run's log\n",
      "receipt.json": '{"an":"earlier receipt"}\n',
    };
    for (const [name, text] of Object.entries(files)) writeFileSync(join(finished, name), text);
    const again = launcherCLI(t, f, [mode, file, finished, ...flags], environment);
    assert.equal(await again.closed, 1, what + ": " + again.text);
    assert.equal(again.text, message + "\n", what);
    assert.deepEqual(
      Object.fromEntries(readdirSync(finished).sort().map((name) => [name, readFileSync(join(finished, name), "utf8")])),
      Object.fromEntries(Object.entries(files).sort(([a], [b]) => (a < b ? -1 : 1))),
      what,
    );
  }
  assert.deepEqual(readHostState(hostLockFile())?.waiters ?? [], [], "no refused launch joined the host");
});

// The files of a queued detached run's output directory: the host lock's wait
// log, and the launcher's record and log.
const queuedRunFiles = ["host-lock.log", "runner.json", "runner.log"];
const within = (promise, ms, what) =>
  Promise.race([
    promise,
    new Promise((_, reject) => setTimeout(() => reject(new Error("timed out waiting for " + what)), ms).unref()),
  ]);
const stopQueuedRun = async (output) => {
  const record = runnerRecord(output);
  if (!record || !pidAlive(record.pid)) return;
  process.kill(record.pid, "SIGTERM");
  await untilHost(() => !pidAlive(record.pid), "the queued run to stop");
};
test("bundle v1 launchers started together into one directory start one run; the rest re-attach or are refused", { timeout: 240000 }, async (t) => {
  const { f, planFile } = plannedFixture(t, "console.log('ran');");
  const requests = (from) =>
    readJournal(hostLockFile()).slice(from).filter((entry) => entry.event === "request" && entry.agent !== "holder");
  // The same command, four at once, five times.
  for (let round = 0; round < 5; round++) {
    const output = tempDir(t, "bundle-v1-logs-"),
      journalBefore = readJournal(hostLockFile()).length;
    const holder = await holdHost("wi_bundle_v1_holder");
    let released = false;
    try {
      const launchers = Array.from({ length: 4 }, () => launcherCLI(t, f, ["run", planFile, output]));
      await untilHost(
        () => launchers.every((child) => /^matrix run: (detached as|re-attached to) pid \d+/m.test(child.text)),
        "every launcher's decision; saw " + launchers.map((child) => child.text).join("|"),
      );
      const started = launchers.flatMap((child) => /detached as pid (\d+)/.exec(child.text)?.[1] ?? []),
        attached = launchers.flatMap((child) => /re-attached to pid (\d+)/.exec(child.text)?.[1] ?? []);
      assert.equal(started.length, 1, `round ${round}: ${started.length} runs were started`);
      assert.deepEqual(attached, [started[0], started[0], started[0]], `round ${round}`);
      const { record } = await queuedRun(output, "the one run");
      assert.equal(String(record.pid), started[0]);
      await untilHost(() => readFileSync(join(output, "runner.log"), "utf8").includes("matrix host: waiting"), "the waiting line");
      await new Promise((resolve) => setTimeout(resolve, 300));
      assert.equal(readFileSync(join(output, "runner.log"), "utf8").match(/matrix host: waiting/g).length, 1, "one run wrote the log");
      assert.deepEqual(readHostState(hostLockFile()).waiters.map((w) => w.pid), [record.pid], "one waiter");
      assert.deepEqual(requests(journalBefore).map((entry) => entry.pid), [record.pid], "one request");
      assert.deepEqual(readdirSync(output).sort(), queuedRunFiles, "no mutex, guard, temporary or journal file is left");
      released = true;
      await holder.release();
      assert.deepEqual(await Promise.all(launchers.map((child) => child.closed)), [0, 0, 0, 0], launchers.map((child) => child.text).join("|"));
      assert.deepEqual(requests(journalBefore).length, 1, "still one request");
    } finally {
      if (!released) await holder.release();
    }
  }

  // Two different commands at once: one starts, the other is refused.
  const output = tempDir(t, "bundle-v1-logs-"),
    journalBefore = readJournal(hostLockFile()).length;
  const holder = await holdHost("wi_bundle_v1_holder");
  try {
    const pair = [
      launcherCLI(t, f, ["run", planFile, output]),
      launcherCLI(t, f, ["run", planFile, output, "--jobs", "1"]),
    ];
    const { record } = await queuedRun(output, "the one run");
    const refused = await within(Promise.race(pair.map((child) => child.closed.then(() => child))), 20000, "the refused launcher");
    assert.equal(await refused.closed, 1, refused.text);
    assert.equal(refused.text, `Host lock output/record directory is already in use by a different matrix run (pid ${record.pid})\n`);
    const winner = pair.find((child) => child !== refused);
    assert.match(winner.text, new RegExp(`^matrix run: detached as pid ${record.pid};`));
    await new Promise((resolve) => setTimeout(resolve, 300));
    assert.deepEqual(readHostState(hostLockFile()).waiters.map((w) => w.pid), [record.pid], "one waiter");
    assert.deepEqual(requests(journalBefore).map((entry) => entry.pid), [record.pid], "one request");
    assert.deepEqual(readdirSync(output).sort(), queuedRunFiles);
    await stopQueuedRun(output);
    assert.equal(await winner.closed, 143, winner.text);
  } finally {
    await holder.release();
  }
});

test("bundle v1 a launch mutex left by a dead launcher is recovered, and one that cannot be reclaimed refuses with the file to remove", { timeout: 120000 }, async (t) => {
  const { f, planFile } = plannedFixture(t, "console.log('ran');");
  const holder = await holdHost("wi_bundle_v1_holder");
  try {
    // A live owner (this process) and an owner record that does not parse are
    // never reclaimed: no run starts and the refusal names the file.
    for (const [what, owner, holderText] of [
      ["live owner", JSON.stringify({ pid: process.pid, token: "live", at: new Date().toISOString() }) + "\n", ` (pid ${process.pid})`],
      ["unparseable owner", "{", ""],
    ]) {
      const output = tempDir(t, "bundle-v1-logs-"),
        lock = join(output, "runner.json.lock");
      writeFileSync(lock, owner);
      const started = Date.now();
      const child = launcherCLI(t, f, ["run", planFile, output]);
      assert.equal(await within(child.closed, 20000, `the ${what} refusal; saw ${child.text}`), 1, child.text);
      assert(Date.now() - started >= 5000, what + ": the launcher waited for the holder");
      assert.equal(
        child.text,
        `Another matrix launcher${holderText} holds ${lock}; no run was started. ` +
          "If no launcher for this output directory is running, remove that file and run the command again\n",
        what,
      );
      assert.deepEqual(readdirSync(output), ["runner.json.lock"], what);
      assert.equal(readFileSync(lock, "utf8"), owner, what);
      assert.deepEqual(readHostState(hostLockFile()).waiters, [], what + ": nothing joined the host");
    }
    // A dead owner is reclaimed; the recovery is journalled beside the record.
    const output = tempDir(t, "bundle-v1-logs-"),
      dead = spawnSync(process.execPath, ["-e", ""]).pid;
    assert(!pidAlive(dead));
    writeFileSync(join(output, "runner.json.lock"), JSON.stringify({ pid: dead, token: "dead", at: new Date().toISOString() }) + "\n");
    const child = launcherCLI(t, f, ["run", planFile, output]);
    const { record } = await queuedRun(output, "the run after the recovery; saw " + child.text);
    assert.match(child.text, new RegExp(`^matrix run: detached as pid ${record.pid};`));
    await untilHost(() => existsSync(join(output, "host-lock.log")), "the run's host lock files");
    assert.deepEqual(readdirSync(output).sort(), ["host.journal.jsonl", ...queuedRunFiles].sort());
    assert.deepEqual(
      readFileSync(join(output, "host.journal.jsonl"), "utf8").trim().split("\n").map((line) => JSON.parse(line)).map((entry) => [entry.event, entry.deadPid, entry.token]),
      [["mutex-recovered", dead, "dead"]],
    );
    assert.deepEqual(readHostState(hostLockFile()).waiters.map((w) => w.pid), [record.pid], "one waiter");
    await stopQueuedRun(output);
    assert.equal(await child.closed, 143, child.text);
  } finally {
    await holder.release();
  }
});

test("bundle v3 a selection path that is not a canonical repository path selects from the job's base", (t) => {
  const f = tipFixture(t);
  const before = makePlan(withoutSelection(f.context), f.cwd);
  assert.deepEqual(checkIds(before), ["go-race", "go-test", "go-vet", "npm-unit"]);
  // The well-formed selection is narrower, so honouring a malformed one
  // under the tip rule would drop the Go checks.
  assert.deepEqual(checkIds(matrixRunner.planWithPreservation(f.context, f.cwd).plan), ["npm-unit"]);
  const sparse = ["docs/x.md"];
  sparse.length = 3;
  const shapes = {
    "a null entry": [null],
    "an object entry": [{}],
    "a nested array": [["docs/x.md"]],
    "a sparse array": sparse,
    "an object": {},
    "a leading dot segment": ["./docs/x.md"],
    "a doubled slash": ["docs//x.md"],
    "an inner dot segment": ["docs/./x.md"],
    "a doubled trailing slash": ["docs//"],
    "a slash alone": ["/"],
    "a well-formed path beside a doubled slash": ["docs/x.md", "docs//x.md"],
  };
  for (const [what, selectionPaths] of Object.entries(shapes)) {
    const { plan, selection } = matrixRunner.planWithPreservation({ ...f.context, selectionPaths }, f.cwd);
    assert.deepEqual(plan, before, what);
    assert.deepEqual(selection, { rule: "job-base", baseCommit: f.base, reason: "invalid-selection-paths" }, what);
  }
  // An owned directory prefix, as plans record them, is still a selection path.
  const owned = matrixRunner.planWithPreservation({ ...f.context, selectionPaths: ["docs/"] }, f.cwd);
  assert.equal(owned.selection.rule, "integration-tip");
  assert.deepEqual(checkIds(owned.plan), ["npm-unit"]);
});

// A Linux process table for the home sweep, with nothing live behind it:
// canned ps and lsof output, an environ read that fails as told, and a clock.
function linuxSweep(t, processes, { psFails = false, psSeconds = 0, dropSelfRow = false, garbageRow = false } = {}) {
  const home = mkdtempSync(join(realpathSync(tmpdir()), "bundle-v7-home-"));
  t.after(() => rmSync(home, { recursive: true, force: true }));
  const self = 500,
    uid = 501,
    homeCreatedMs = 1_000_000_000_000;
  let clock = homeCreatedMs + 600_000;
  const calls = [],
    reported = [];
  const probes = {
    platform: "linux",
    uid,
    self,
    homeCreatedMs,
    now: () => clock,
    onUnreadable: (entry) => reported.push(entry),
    readEnviron: (pid) => {
      const entry = processes[pid];
      if (entry.environ === undefined) throw Object.assign(new Error("environ " + (entry.code ?? "EACCES")), { code: entry.code ?? "EACCES" });
      return entry.environ(home);
    },
    run: (command, args) => {
      calls.push([command, ...args].join(" "));
      if (command === "lsof") return "";
      if (args.includes("-axww"))
        return [`${self} 1 ${uid} node runner`, ...Object.entries(processes).map(([pid, entry]) => `${pid} 1 ${uid} ${entry.command ?? "daemon"}`)].join("\n") + "\n";
      // ps -o pid=,etimes= -p LIST. A slow ps samples each elapsed time as
      // it finishes, psSeconds after it was called.
      clock += psSeconds * 1000;
      const startedAtCall = clock;
      if (psFails) throw new Error("ps exited 1");
      const row = (pid, startedMs) => `${String(pid).padStart(7)} ${String(Math.floor((startedAtCall - startedMs) / 1000)).padStart(7)}`;
      return [
        ...(dropSelfRow ? [] : [row(self, homeCreatedMs - 60_000)]),
        ...(garbageRow ? ["ps: bad output"] : []),
        ...args.at(-1).split(",").map(Number).filter((pid) => pid !== self && processes[pid] && !processes[pid].gone)
          .map((pid) => (processes[pid].etimes !== undefined ? `${pid} ${processes[pid].etimes}` : row(pid, homeCreatedMs + processes[pid].startedAfterHomeMs))),
      ].join("\n") + "\n";
    },
  };
  return { home, probes, calls, reported, self, homeCreatedMs, find: (override = {}) => matrixRunner.verifierHomeProcesses(home, { ...probes, ...override }) };
}
test("bundle v7 an unreadable environ is skipped only for a process that started before the verifier home, and fails closed otherwise", async (t) => {
  const hourBefore = -3_600_000;
  const unproven = /Cannot read the environment of 1 same-user process \(pid 700\) or prove it unrelated to the verifier home: /;
  // Older than the home: skipped, reported, nothing fails. A readable
  // process under the home is still found and a vanished one still skipped.
  let sweep = linuxSweep(t, {
    700: { command: "ssh-agent -s", startedAfterHomeMs: hourBefore },
    701: { code: "EPERM", startedAfterHomeMs: hourBefore },
    710: { command: "leftover", environ: (home) => `PATH=/bin\0HOME=${home}\0` },
    711: { environ: () => "HOME=/somewhere/else\0" },
    712: { code: "ENOENT" },
  });
  assert.deepEqual(sweep.find(), [{ pid: 710, reasons: ["env"], command: "leftover" }]);
  assert.deepEqual(sweep.reported, [
    { pid: 700, code: "EACCES", startedAt: new Date(sweep.homeCreatedMs + hourBefore).toISOString(), command: "ssh-agent -s" },
    { pid: 701, code: "EPERM", startedAt: new Date(sweep.homeCreatedMs + hourBefore).toISOString(), command: "daemon" },
  ]);
  // One ps for start times, naming the unreadable pids and the runner itself;
  // the open-file sweep still runs.
  assert.deepEqual(sweep.calls, [
    "ps -axww -o pid=,ppid=,uid=,command=",
    "ps -o pid=,etimes= -p 700,701,500",
    `lsof -nP -w -u 501 -F pn`,
  ]);
  // No unreadable process: no second ps.
  sweep = linuxSweep(t, { 711: { environ: () => "HOME=/somewhere/else\0" } });
  assert.deepEqual(sweep.find(), []);
  assert.equal(sweep.calls.length, 2);

  // Started after the home, or inside the margin before it: unproven.
  for (const startedAfterHomeMs of [10_000, 0, -4_000]) {
    sweep = linuxSweep(t, { 700: { startedAfterHomeMs }, 701: { startedAfterHomeMs: hourBefore } });
    assert.throws(
      () => sweep.find(),
      {
        message:
          "Cannot read the environment of 1 same-user process (pid 700) or prove it unrelated to the verifier home: " +
          "not started before the home was created, or start time unknown",
      },
      String(startedAfterHomeMs),
    );
    assert.deepEqual(sweep.reported.map((entry) => entry.pid), [701], "the proven one is still reported");
  }
  assert.deepEqual(linuxSweep(t, { 700: { startedAfterHomeMs: -6_000 } }).find(), []);
  // An unknown start time: unparseable, ps failing, a line ps should not
  // print, or no row for the runner itself.
  for (const [processes, options, why] of [
    [{ 700: { etimes: "-" } }, {}, /start time unknown$/],
    [{ 700: { etimes: "12:01" } }, {}, /start time unknown$/],
    [{ 700: { startedAfterHomeMs: hourBefore } }, { psFails: true }, /ps could not report start times: ps exited 1$/],
    [{ 700: { startedAfterHomeMs: hourBefore } }, { garbageRow: true }, /ps printed a line that could not be read$/],
    [{ 700: { startedAfterHomeMs: hourBefore } }, { dropSelfRow: true }, /ps did not list the runner itself$/],
  ]) {
    sweep = linuxSweep(t, processes, options);
    assert.throws(() => sweep.find(), (error) => unproven.test(error.message) && why.test(error.message), String(why));
    assert.deepEqual(sweep.reported, [], String(why));
  }
  // It exited between the environ read and ps: gone, like any exited process.
  sweep = linuxSweep(t, { 700: { gone: true } });
  assert.deepEqual(sweep.find(), []);
  assert.deepEqual(sweep.reported, []);
  // A slow ps: the process started 10 s after the home and ps took 30 s. A
  // clock read before ps would place its start 20 s before the home.
  sweep = linuxSweep(t, { 700: { startedAfterHomeMs: 10_000 } }, { psSeconds: 30 });
  assert.throws(() => sweep.find(), unproven);
  assert.deepEqual(sweep.reported, []);
  // Without the home's creation time nothing is proven.
  sweep = linuxSweep(t, { 700: { startedAfterHomeMs: hourBefore } });
  assert.throws(() => sweep.find({ homeCreatedMs: undefined }), /the home's creation time is not known$/);
  assert.equal(sweep.calls.length, 1, "no start-time ps without it");
  // Any other read error still throws as it is, and many pids are bounded.
  sweep = linuxSweep(t, { 700: { code: "EIO" } });
  assert.throws(() => sweep.find(), { message: "environ EIO" });
  sweep = linuxSweep(t, Object.fromEntries(Array.from({ length: 14 }, (_, i) => [800 + i, { startedAfterHomeMs: 1000 }])));
  assert.throws(() => sweep.find(), /Cannot read the environment of 14 same-user processes \(pid 800, 801, 802, 803, 804, 805, 806, 807, 808, 809, \.\.\.\) or prove them unrelated/);

  // The stop passes the probes to each sweep.
  sweep = linuxSweep(t, { 700: { startedAfterHomeMs: hourBefore } });
  assert.deepEqual(await matrixRunner.stopVerifierHomeProcesses(sweep.home, undefined, sweep.probes), []);
  assert.deepEqual(sweep.reported.map((entry) => entry.pid), [700]);
  sweep = linuxSweep(t, { 700: { startedAfterHomeMs: 1000 } });
  await assert.rejects(() => matrixRunner.stopVerifierHomeProcesses(sweep.home, undefined, sweep.probes), unproven);
});

test("bundle v7 a run records the unreadable processes it skipped once each, and an unproven one still leaves no receipt", async (t) => {
  const f = fixture(t),
    plan = commandPlan(f, "console.log('check passed');");
  const entry = { pid: 700, code: "EACCES", startedAt: "2026-01-01T00:00:00.000Z", command: "ssh-agent -s" };
  const output = tempDir(t, "bundle-v7-logs-");
  let given;
  const before = Date.now();
  const receipt = await runPlan(plan, f.cwd, output, {
    stopHomeProcesses: async (home, find, probes) => {
      given = { home, find, homeCreatedMs: probes.homeCreatedMs };
      // One report per sweep of the stop.
      for (let sweep = 0; sweep < 4; sweep++) probes.onUnreadable(entry);
      probes.onUnreadable({ ...entry, pid: 701, code: "EPERM" });
      return [];
    },
  });
  assert(receiptEligible(receipt));
  assert(existsSync(join(output, "receipt.json")));
  assert.equal(given.find, undefined);
  assert(given.homeCreatedMs >= before && given.homeCreatedMs <= Date.now(), "the home's creation time is passed down");
  assert(!existsSync(given.home), "the home was still removed");
  assert.deepEqual(JSON.parse(readFileSync(join(output, "home-sweep-unreadable.json"), "utf8")), {
    home: given.home,
    count: 2,
    processes: [entry, { ...entry, pid: 701, code: "EPERM" }],
  });
  assert(!existsSync(join(output, "home-processes.json")));
  // An unproven process fails the stop: no receipt, the home kept, and what
  // was skipped before it is still recorded.
  const failed = tempDir(t, "bundle-v7-logs-");
  let retained = "";
  t.after(() => retained && existsSync(retained) && removeVerifierHome(retained));
  await assert.rejects(
    () =>
      runPlan(plan, f.cwd, failed, {
        stopHomeProcesses: async (home, find, probes) => {
          retained = home;
          probes.onUnreadable(entry);
          throw new Error("Cannot read the environment of 1 same-user process (pid 702)");
        },
      }),
    /Failed to stop processes under verifier home .*Cannot read the environment of 1 same-user process \(pid 702\)/,
  );
  assert(existsSync(retained), "the home is kept");
  assert(!existsSync(join(failed, "receipt.json")));
  assert.match(JSON.parse(readFileSync(join(failed, "cleanup-error.json"), "utf8")).error, /pid 702/);
  assert.deepEqual(JSON.parse(readFileSync(join(failed, "home-sweep-unreadable.json"), "utf8")).processes, [entry]);
  // A run with nothing unreadable writes no such file.
  const plain = tempDir(t, "bundle-v7-logs-");
  await runPlan(plan, f.cwd, plain);
  assert(!existsSync(join(plain, "home-sweep-unreadable.json")));
});
