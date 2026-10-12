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
  realpathSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { execFileSync, spawn, spawnSync } from "node:child_process";
import { once } from "node:events";
import {
  makePlan as rawMakePlan,
  runPlan,
  digest,
  runScheduled,
  runTargeted,
} from "../scripts/verify-matrix.mjs";
// Named separately so the red tests load on a runner that lacks these exports.
import * as matrixRunner from "../scripts/verify-matrix.mjs";
import {
  acquireHostLock,
  readHostState as rawReadHostState,
  holdersOf,
} from "../scripts/verify-matrix-host-lock.mjs";
// Every run in this file, in process or through the CLI, takes its host lock
// in a private directory and never the host's own lock file.
export const hostLockDirectory = mkdtempSync(join(tmpdir(), "matrix-host-lock-"));
process.env.TAILTERM_MATRIX_HOST_LOCK = join(hostLockDirectory, "host.json");
process.env.TAILTERM_MATRIX_MAX_HOLDERS = "1";
// CLI runs in this file stay one process, the run itself, unless a test starts
// the detached launcher on purpose (launcherCLI). No run inherits an agent
// identity, so none reports a withdrawal unless its test supplies one.
process.env.TAILTERM_MATRIX_FOREGROUND = "1";
for (const key of ["TAILTERM_MATRIX_RUN_CHILD", "TAILTERM_MATRIX_RELEASE", "TAILTERM_AGENT", "TAILTERM_AGENT_NAME", "TAILTERM_TASK"])
  delete process.env[key];
export const readHostState = (...args) => {
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
export const fakeTTDirectory = mkdtempSync(join(tmpdir(), "matrix-fake-tt-"));
export const fakeTTCalls = join(fakeTTDirectory, "calls.jsonl");
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
export const fakeTTLookups = () =>
  existsSync(fakeTTCalls)
    ? readFileSync(fakeTTCalls, "utf8").trim().split("\n").map((line) => JSON.parse(line))
    : [];
process.on("exit", () => {
  rmSync(hostLockDirectory, { recursive: true, force: true });
  rmSync(fakeTTDirectory, { recursive: true, force: true });
});
export const makePlan = (context, cwd) =>
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
export const matrix = JSON.parse(
  readFileSync(new URL("../verification/matrix.json", import.meta.url)),
);
export function fixture(t) {
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
export function tempDir(t, prefix) {
  const directory = mkdtempSync(join(tmpdir(), prefix));
  t.after(() => rmSync(directory, { recursive: true, force: true }));
  return directory;
}
export function commandPlan(f, source) {
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
// Processes a fixture check leaves behind record their pids outside the
// verifier home, and t.after SIGKILLs every recorded pid that still runs this
// fixture's own child.cjs, so a failing test cannot leak the processes it
// exercises. A recorded pid whose process already exited may by then belong
// to a stranger, which the unique directory in the command line rules out.
export function fixturePids(t) {
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
export function leakingCheck(pids, names, starts) {
  return `import {spawn} from 'node:child_process';import fs from 'node:fs';
const pids=${JSON.stringify(pids)},home=process.env.HOME,elsewhere={...process.env,HOME:pids,TMPDIR:pids,PWD:pids};
const start=(name,options,held='')=>spawn(process.execPath,[pids+'/child.cjs',name,held],{detached:true,stdio:'ignore',...options}).unref();
${starts}
const names=${JSON.stringify(names)},deadline=Date.now()+10000;
while(!names.every((name)=>fs.existsSync(pids+'/'+name+'.pid'))){if(Date.now()>deadline){console.error('children did not start');process.exit(1)}await new Promise((resolve)=>setTimeout(resolve,20))}
console.log('leaked '+names.join(','));`;
}
export const pidOf = (pids, name) => Number(readFileSync(join(pids, name + ".pid"), "utf8"));
export const alive = (pid) => {
  try {
    process.kill(pid, 0);
    return true;
  } catch (error) {
    return error.code !== "ESRCH";
  }
};

// wi_82ed4c6924930bad / order #13844: parallel scheduling, targeted fix runs
// and fast-forward bases.
export const fakeCheck = (id, extra = {}) => ({
  id,
  argv: ["node", id],
  cwd: ".",
  environment: {},
  ...extra,
});
export async function simulate(checks, jobs, duration = () => 5 + Math.random() * 10) {
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
export function browserFixture(t, suites) {
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
    // The literal is written in two pieces so this module does not itself contain the
    // text the browser inventory looks for in tests/*.mjs; the bytes written are the same.
    writeFileSync(join(f.cwd, "tests", name), "// browser.launch" + "(\n" + source);
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
// Pids of the live processes whose command line carries token, read in one
// listing. A zombie awaiting its parent is not running and is left out.
export const tokenProcesses = (token) =>
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
export async function interruptedConcurrentRun(t, { withhold = [], publishWithinMs = 30_000 } = {}, run = {}) {
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
export function commitChange(f, path, content, message) {
  mkdirSync(join(f.cwd, path, ".."), { recursive: true });
  writeFileSync(join(f.cwd, path), content);
  f.git("add", ".");
  f.git("commit", "-qm", message);
  return f.git("rev-parse", "HEAD");
}

// #15095: the Go lane is the critical path. Go checks start first, go-race
// holds two slots, go-vet leaves the lane, Go checks bypass the exclusive
// barriers, and non-Go checks run at a lower CPU priority.
export async function simulateLoad(checks, jobs, duration) {
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
export const goFixture = (id, argv) => fakeCheck(id, { argv, cwd: "hub" });
export const criticalPathPlan = () => [
  fakeCheck("00-static-build", { argv: ["npm", "run", "build:static"] }),
  fakeCheck("01-static-release-verify", { argv: ["npm", "run", "verify:release"] }),
  goFixture("go-race", ["go", "test", "-race", "./..."]),
  goFixture("go-test", ["go", "test", "./..."]),
  goFixture("go-vet", ["go", "vet", "./..."]),
  fakeCheck("npm-unit", { argv: ["npm", "test"] }),
  ...Array.from({ length: 8 }, (_, i) => fakeCheck(`tests/s${i}.mjs`)),
  fakeCheck("wasm-test-build", { argv: ["bash", "scripts/build-wasm.sh", "--test"] }),
];

// Host lock and waitlist (wi_6b4af2fb034ec36e, order #18569). V1-V6 of the plan.
export const HOST_KEYS = [
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
export const hostLockFile = () => process.env.TAILTERM_MATRIX_HOST_LOCK;
export const matrixScript = new URL("../scripts/verify-matrix.mjs", import.meta.url).pathname;
export const sleepingCheck = (ms) => `await new Promise((resolve)=>setTimeout(resolve,${ms}));`;
export async function untilHost(condition, what) {
  const deadline = Date.now() + 20000;
  for (;;) {
    const value = condition();
    if (value) return value;
    if (Date.now() > deadline) assert.fail("timed out waiting for " + what);
    await new Promise((resolve) => setTimeout(resolve, 10));
  }
}
export function matrixCLI(t, f, args, environment = {}) {
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
export function plannedFixture(t, source) {
  const f = fixture(t),
    plan = commandPlan(f, source),
    planFile = join(tempDir(t, "verification-host-plan-"), "plan.json");
  writeFileSync(planFile, JSON.stringify(plan));
  return { f, plan, planFile };
}
export const holdHost = (item) =>
  acquireHostLock({ item, agent: "holder", runTimeoutMs: 60000, pollMs: 20 });

// wi_789dc79d7eff49bc / order21693: refusal must precede host admission.
export const prerequisitePaths = [
  "node_modules/.package-lock.json", "wasm/tailserve.wasm", ".build/test.wasm",
  ".build/speech-fixture.wav", ".build/go-modules.txt",
];
export function preflightFixture(t, browser = false) {
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
export const preflightRun = (mode, f, output, options) => mode === "run"
  ? runPlan(f.plan, f.cwd, output, options)
  : runTargeted(f.context, f.cwd, output, options);

// A docs-only item under hub/cmd/tt changes no Go file, so its accepted plan
// runs go-race on ./...; by integration other items' changes sit between the
// accepted base and the integrated commit.
export const integratedKnownFailure = {
  checkId: "npm-unit",
  bugTaskId: "tsk_0123456789abcdef",
  bugId: "wi_0123456789abcdef",
};
export function integratedFixture(t, itemPath = "hub/cmd/tt/testdata/README.md") {
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
export const raceArgv = (plan) => plan.checks.find((c) => c.id === "go-race").argv;
// An accepted plan whose checks were changed and whose digest still binds them.
export const withChecks = (plan, change) => {
  const checks = structuredClone(plan.checks);
  return { ...plan, checks: change(checks) ?? checks, checksDigest: undefined };
};

// The release runner's case: the job's base is older than the tasks-hub tip,
// which moved through other releases, and the job is picked onto that tip.
// job and tip map each changed path to its new content.
export function tipFixture(
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
export const checkIds = (plan) => plan.checks.map((c) => c.id);
export const withoutSelection = ({ selectionBaseCommit, selectionPaths, ...context }) => context;

// Detached matrix run (wi_912a9789357529d4, order #27051). a1-a10 of the plan.
// The launcher is the CLI without the foreground opt-out: it starts the real
// run detached and only follows its log. Every run it starts is stopped by pid
// when its test ends.
export const runnerRecord = (output) => {
  try {
    return JSON.parse(readFileSync(join(output, "runner.json"), "utf8"));
  } catch {
    return undefined;
  }
};
export const pidAlive = (pid) => {
  try {
    process.kill(pid, 0);
    return true;
  } catch (error) {
    return error.code === "EPERM";
  }
};
export function launcherCLI(t, f, args, environment = {}, options = {}) {
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
export const queuedRun = (output, what) =>
  untilHost(() => {
    const record = runnerRecord(output);
    const waiter = record && readHostState(hostLockFile())?.waiters.find((w) => w.pid === record.pid);
    return waiter && { record, waiter };
  }, what + " to join the waitlist as the detached run");

// The launcher's run once it holds the host.
export const holdingRun = (output, what) =>
  untilHost(() => {
    const record = runnerRecord(output);
    return record && readHostState(hostLockFile())?.holder?.pid === record.pid && record;
  }, what + " to hold the host");

export const agentIdentity = {
  TAILTERM_AGENT: "agt_fake_verifier",
  TAILTERM_AGENT_NAME: "verifier-fake",
  TAILTERM_TASK: "tsk_fake",
};
export const fakeBinding = { itemId: "wi_fake_item", itemRevision: 7, workOrderMessage: { taskId: "tsk_fake", seq: 4321 } };

// wi_d6b6387726c1f846: the store race suite runs as K shards inside the one
// go-race check. A test's shard is the first 8 hex digits of the SHA-256 of
// its name modulo K, and the last shard is a catch-all that skips the others.
export const {
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
export const shardPrefix = (k = 4, pkg = "./internal/store") => [
  "node", "../scripts/verify-matrix.mjs", "go-race", "-timeout=45m", `-shards=${pkg}=${k}`,
];
export const shardedCheck = (k, ...packages) => ({
  id: "go-race",
  argv: [...shardPrefix(k), ...packages],
  cwd: "hub",
  environment: {},
});

export function shardPlanFixture(t) {
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

// A real Go module run through the go-race command. Every fixture test appends
// "package name pid nanoseconds run" to FIXTURE_LOG, and a go wrapper on PATH
// records each go command line before running it.
export const fixtureGoTests = {
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
export const fixtureStoreTests = ["TestAlpha", "TestBeta", "TestGamma", "TestDelta", "TestEpsilon", "FuzzSeed", "ExampleHello"];
export function goRaceFixture(t) {
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
export const bigMax = (values) => values.reduce((a, b) => (a > b ? a : b)),
  bigMin = (values) => values.reduce((a, b) => (a < b ? a : b));
// The fixture's partition for K=2, from the hash rule.
export const fixtureShards = [0, 1].map((shard) => fixtureStoreTests.filter((name) => goRaceShardOf(name, 2) === shard).sort());
export const fixtureShardCommands = [
  `test -race -json -count=1 -timeout=5m -run ^(${fixtureShards[0].join("|")})$ ./store`,
  `test -race -json -count=1 -timeout=5m -skip ^(${fixtureShards[0].join("|")})$ ./store`,
];

// Every process with its group and state, and whether its environment carries
// marker. Read the way the runner's home sweep reads environments, which hold
// other processes' credentials: matched here and never kept.
export function markedProcesses(marker) {
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
export function ownedGroup(pgid, marker, { list = markedProcesses, kill = process.kill } = {}) {
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

// A stand-in go for runGoRace: it lists 300 generated tests and, for a shard,
// emits a run event per selected test. STUB_GO_SHARD makes the first shard
// (the one selected with -run) exit 2 after one output line, kill itself, or
// fail after running everything. No Go toolchain and no race load.
export function stubGoRace(t) {
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

// The files of a queued detached run's output directory: the host lock's wait
// log, and the launcher's record and log.
export const queuedRunFiles = ["host-lock.log", "runner.json", "runner.log"];
export const within = (promise, ms, what) =>
  Promise.race([
    promise,
    new Promise((_, reject) => setTimeout(() => reject(new Error("timed out waiting for " + what)), ms).unref()),
  ]);
export const stopQueuedRun = async (output) => {
  const record = runnerRecord(output);
  if (!record || !pidAlive(record.pid)) return;
  process.kill(record.pid, "SIGTERM");
  await untilHost(() => !pidAlive(record.pid), "the queued run to stop");
};

// A Linux process table for the home sweep, with nothing live behind it:
// canned ps and lsof output, an environ read that fails as told, and a clock.
export function linuxSweep(t, processes, { psFails = false, psSeconds = 0, dropSelfRow = false, garbageRow = false } = {}) {
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
