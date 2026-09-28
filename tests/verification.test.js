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
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { execFileSync, spawn } from "node:child_process";
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
test("run removes locked Go cache after success and leaves symlink target untouched", (t) => {
  const f = fixture(t),
    outside = tempDir(t, "verification-sibling-");
  writeFileSync(join(outside, "keep"), "sibling");
  const plan = commandPlan(
    f,
    `import fs from 'node:fs';import path from 'node:path';const home=process.env.HOME;const cache=path.join(process.env.GOMODCACHE,'locked');fs.mkdirSync(cache,{recursive:true});fs.writeFileSync(path.join(cache,'module'), 'module');fs.chmodSync(path.join(cache,'module'),0o400);fs.chmodSync(cache,0o500);fs.symlinkSync(${JSON.stringify(outside)},path.join(home,'sibling'));console.log(JSON.stringify({HOME:home,GOPATH:process.env.GOPATH,GOMODCACHE:process.env.GOMODCACHE,GOCACHE:process.env.GOCACHE}));`,
  );
  const output = tempDir(t, "verification-clean-logs-");
  const receipt = runPlan(plan, f.cwd, output);
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
test("failed run removes home after saving failed logs", (t) => {
  const f = fixture(t),
    plan = commandPlan(f, "console.error('fixture failure');process.exit(7);");
  const receipt = runPlan(plan, f.cwd, tempDir(t, "verification-failed-logs-"));
  assert.equal(receipt.checks[0].exitCode, 7);
  assert(!existsSync(receipt.environment.HOME));
  assert.match(
    readFileSync(receipt.checks[0].logURI, "utf8"),
    /fixture failure/,
  );
});
test("keep-home retains exact home and records opt-in with cache isolation", (t) => {
  const f = fixture(t),
    plan = commandPlan(f, "console.log(process.env.HOME);");
  const output = tempDir(t, "verification-kept-logs-");
  const receipt = runPlan(plan, f.cwd, output, { keepHome: true });
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
test("free-space reserve rejects before run and before retry without filling disk", (t) => {
  const f = fixture(t),
    output = tempDir(t, "verification-space-logs-");
  const counter = join(output, "count");
  const plan = commandPlan(
    f,
    `import fs from 'node:fs';fs.appendFileSync(${JSON.stringify(counter)},'x');process.exit(9);`,
  );
  assert.throws(
    () => runPlan(plan, f.cwd, output, { minFreeBytes: -1 }),
    /Invalid --min-free-bytes/,
  );
  assert.throws(
    () =>
      runPlan(plan, f.cwd, output, {
        minFreeBytes: 10,
        getAvailableBytes: () => 9,
      }),
    /Insufficient free space/,
  );
  assert(!existsSync(counter), "low-space refusal starts no command");
  let probes = 0;
  assert.throws(
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
  const receipt = runPlan(plan, f.cwd, output, {
    minFreeBytes: 0,
    getAvailableBytes: () => 10,
  });
  assert.equal(receipt.checks[0].exitCode, 9);
  assert.equal(readFileSync(counter, "utf8"), "xxxx");
});
test("missing prerequisites and output errors clean their allocated homes", (t) => {
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
  assert.throws(
    () => runPlan(plan, f.cwd, prerequisiteOutput),
    /Missing prerequisite/,
  );
  assert.deepEqual(readdirSync(root), []);
  assert.throws(() => runPlan(plan, f.cwd, outputFile), /EEXIST/);
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
  assert.throws(
    () =>
      execFileSync(
        process.execPath,
        [script, "run", planFile, output, "--min-free-bytes", "-1"],
        { cwd: f.cwd, env: { ...process.env, TMPDIR: root }, stdio: "pipe" },
      ),
    /Invalid --min-free-bytes/,
  );
});
for (const signal of ["SIGINT", "SIGTERM"])
  test(`CLI ${signal} interruption removes its home and preserves external logs`, async (t) => {
    const f = fixture(t),
      root = tempDir(t, "verification-signal-root-");
    const marker = join(root, "started");
    const plan = commandPlan(
      f,
      `import fs from 'node:fs';fs.writeFileSync(${JSON.stringify(marker)},'started');setTimeout(()=>console.log('finished'),900);`,
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
    assert(
      code === (signal === "SIGINT" ? 130 : 143) || closedBy === signal,
      `exit code ${code}, signal ${closedBy}`,
    );
    assert.deepEqual(readdirSync(root).sort(), ["plan.json", "started"]);
    assert(
      existsSync(join(output, "receipt.json")),
      "receipt was saved outside home",
    );
    const receipt = JSON.parse(readFileSync(join(output, "receipt.json")));
    assert(!existsSync(receipt.environment.HOME));
    assert(existsSync(receipt.checks[0].logURI));
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
test("clean detached run emits complete command evidence and unsubmitted AIV bindings", (t) => {
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
  const receipt = runPlan(plan, f.cwd, out);
  assert.equal(receipt.checks[0].exitCode, 0);
  assert.equal(receipt.planDigest, digest(plan));
  assert.equal(receipt.aiv.state, "unsubmitted");
  assert.match(receipt.checks[0].logDigest, /^[a-f0-9]{64}$/);
  assert.throws(() => runPlan({ ...plan, checks: [] }, f.cwd, out), /omitted/);
  writeFileSync(join(f.cwd, "docs/untracked.md"), "dirty");
  assert.throws(() => runPlan(plan, f.cwd, out), /Clean detached/);
});
test("attached or wrong SHA cannot execute checks", (t) => {
  const f = fixture(t),
    plan = makePlan(
      { baseCommit: f.base, commit: f.commit, owned: ["docs/"] },
      f.cwd,
    ),
    out = tempDir(t, "verification-out-");
  f.git("checkout", "-b", "attached");
  assert.throws(() => runPlan(plan, f.cwd, out), /detached/);
  assert.throws(
    () => runPlan({ ...plan, commit: f.base }, f.cwd, out),
    /Wrong candidate/,
  );
});

test("browser run refuses missing prerequisite assets before commands execute", (t) => {
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
  assert.throws(
    () => runPlan(plan, f.cwd, output),
    /Missing prerequisite: node_modules/,
  );
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
    "900000",
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
    const result = runCheck(check, process.cwd(), { PATH: process.env.PATH });
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

test("matrix timeout kills its own process group descendants and retains failed receipt evidence", (t) => {
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
  const receipt = runPlan(plan, f.cwd, external);
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
    (t) => {
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
      const r = runPlan(plan, f.cwd, output),
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
    (t) => {
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
        r = runPlan(plan, f.cwd, tempDir(t, "known-logs-"));
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
