import test from "node:test";
import assert from "node:assert/strict";
import {
  mkdtempSync,
  mkdirSync,
  writeFileSync,
  readFileSync,
  rmSync,
  existsSync,
  statSync,
  symlinkSync,
  readdirSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join, dirname } from "node:path";
import { execFileSync, spawn, spawnSync } from "node:child_process";
import { once } from "node:events";
import { fileURLToPath } from "node:url";
import {
  acquireHostLock,
  execWithHostLock,
  lockPath,
  lockPaths,
  readHostState as rawReadHostState,
  holdersOf,
  readJournal,
  resolvePriority,
  statusText,
  pidGone,
  groupGone,
  EXIT_WAIT_EXPIRED,
  EXIT_LOCK_UNUSABLE,
  DEFAULT_HOLDER_CAP_MS,
  DEFAULT_HOST_WAIT_MS,
  RUN_TIMEOUT_GRACE_MS,
  holderCapMs,
  holderLimit,
  canonicalResource,
  updateHostState,
} from "../scripts/verify-matrix-host-lock.mjs";
import { makePlan, runPlan, digest, runScheduled, planRunTimeout } from "../scripts/verify-matrix.mjs";

// No test here may fall back to the host's own lock file.
const isolated = mkdtempSync(join(tmpdir(), "matrix-host-lock-default-"));
process.env.TAILTERM_MATRIX_HOST_LOCK = join(isolated, "host.json");
process.env.TAILTERM_MATRIX_MAX_HOLDERS = "1";
const readHostState = (...args) => {
  const state = rawReadHostState(...args);
  const held = holdersOf(state);
  assert(held.length <= 1, "legacy capacity-one fixture has at most one holder");
  return state ? { ...state, holder: held[0] || null } : state;
};
delete process.env.TAILTERM_MATRIX_PRIORITY;
delete process.env.TAILTERM_MATRIX_HOLDER_CAP_MINUTES;
process.on("exit", () => rmSync(isolated, { recursive: true, force: true }));

const moduleFile = fileURLToPath(new URL("../scripts/verify-matrix-host-lock.mjs", import.meta.url));
const moduleURL = new URL("../scripts/verify-matrix-host-lock.mjs", import.meta.url).href;

function tempDir(t, prefix = "matrix-host-lock-") {
  const directory = mkdtempSync(join(tmpdir(), prefix));
  t.after(() => rmSync(directory, { recursive: true, force: true }));
  return directory;
}
const lockFile = (t) => join(tempDir(t), "host.json");
const request = (path, extra = {}) => ({
  path,
  runTimeoutMs: 60000,
  pollMs: 20,
  item: "wi_test",
  agent: "tester",
  ...extra,
  environment: { TAILTERM_MATRIX_MAX_HOLDERS: "1", ...extra.environment },
});
async function until(condition, what, ms = 15000) {
  const deadline = Date.now() + ms;
  for (;;) {
    const value = condition();
    if (value) return value;
    if (Date.now() > deadline) assert.fail("timed out waiting for " + what);
    await new Promise((resolve) => setTimeout(resolve, 10));
  }
}
const waitersOf = (path) => rawReadHostState(path)?.waiters || [];
const events = (path) => readJournal(path).map((line) => line.event);
const RECOVERY = ["stale-recovered", "overlap", "waiter-dropped", "mutex-recovered", "guard-stale", "corrupt-file"];
function exitedPid() {
  return spawnSync(process.execPath, ["-e", ""]).pid;
}
const kill = (target, signal = "SIGKILL") => {
  try {
    process.kill(target, signal);
  } catch {}
};

// A real second process using the module. Modes:
//   hold    acquire, optionally start a detached long-running group and record
//           it, write the marker, then finish as `after` says
//   cycle   acquire and release `count` times, logging enter/leave while held
//   pause   take the file mutex inside an update and stay in it until `go`
const CHILD = `
import fs from 'node:fs';
import { spawn } from 'node:child_process';
import { acquireHostLock, updateHostState } from ${JSON.stringify(moduleURL)};
const o = JSON.parse(process.argv[2]);
const nap = (ms) => Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, ms);
if (o.mode === 'hold') {
  const lease = await acquireHostLock(o.request);
  let group = null;
  if (o.group) {
    const child = spawn(process.execPath, ['-e', 'setInterval(()=>{},1000)'], { detached: true, stdio: 'ignore' });
    await new Promise((resolve) => child.on('spawn', resolve));
    child.unref();
    group = child.pid;
    lease.addGroup(group);
  }
  process.on('SIGUSR1', async () => { await lease.release(); process.exit(0); });
  fs.writeFileSync(o.marker, JSON.stringify({ pid: process.pid, id: lease.id, group }));
  if (o.after === 'exit') process.exit(0);
  if (o.after === 'throw') throw new Error('fixture failure while holding');
  setInterval(() => {}, 1000);
} else if (o.mode === 'cycle') {
  for (let i = 0; i < o.count; i++) {
    const lease = await acquireHostLock(o.request);
    fs.appendFileSync(o.log, 'enter ' + process.pid + '\\n');
    await new Promise((resolve) => setTimeout(resolve, 3));
    fs.appendFileSync(o.log, 'leave ' + process.pid + '\\n');
    await lease.release();
  }
} else if (o.mode === 'pause') {
  const result = updateHostState(o.request.path, (state) => {
    fs.writeFileSync(o.marker, String(process.pid));
    while (!fs.existsSync(o.go)) nap(10);
    state.requestSeq += 1000;
  });
  process.exit(result.done ? 0 : 9);
}
`;
function childScript(t) {
  const file = join(tempDir(t, "matrix-host-lock-child-"), "child.mjs");
  writeFileSync(file, CHILD);
  return file;
}
function startChild(t, script, options) {
  const child = spawn(process.execPath, [script, JSON.stringify(options)], { stdio: ["ignore", "pipe", "pipe"] });
  let output = "";
  child.stdout.on("data", (data) => (output += data));
  child.stderr.on("data", (data) => (output += data));
  child.output = () => output;
  t.after(() => {
    if (child.exitCode === null && child.signalCode === null) child.kill("SIGKILL");
  });
  return child;
}
async function heldByChild(t, path, { request: extra, ...options } = {}) {
  const marker = join(tempDir(t), "marker");
  const child = startChild(t, childScript(t), { mode: "hold", marker, ...options, request: request(path, { item: "wi_child", ...extra }) });
  await until(() => existsSync(marker), "the child to hold the lock");
  const info = JSON.parse(readFileSync(marker, "utf8"));
  if (info.group) t.after(() => kill(-info.group));
  if (info.group) await until(() => holdersOf(rawReadHostState(path)).some(h => h.id === info.id && h.groups?.includes(info.group)), "the group to be on file");
  return { child, ...info };
}
const exited = (child) => (child.exitCode !== null || child.signalCode !== null ? Promise.resolve() : once(child, "close"));

test("a1 a11 the lock path is the documented home path unless the override names another", (t) => {
  assert.equal(lockPath({ HOME: "/home/someone" }), "/home/someone/.local/state/tailterm-matrix/host.json");
  assert.equal(lockPath({ HOME: "/home/someone", TAILTERM_MATRIX_HOST_LOCK: "/x/host.json" }), "/x/host.json");
  assert.throws(() => lockPath({ TAILTERM_MATRIX_HOST_LOCK: "relative/host.json" }), /absolute/);
  assert(lockPath().startsWith(isolated + "/"), "this test file resolves a path under its temp directory");
  const home = tempDir(t, "matrix-host-lock-home-");
  const { TAILTERM_MATRIX_HOST_LOCK, ...environment } = process.env;
  const text = execFileSync(process.execPath, [moduleFile, "status"], { env: { ...environment, HOME: home }, encoding: "utf8" });
  assert.match(text, new RegExp("^matrix host lock: " + home + "/\\.local/state/tailterm-matrix/host\\.json\\nholder: none\\nwaiters: none"));
  assert(!existsSync(join(home, ".local")), "status creates nothing");
});

test("a1 the lock file is version 2 JSON with the holder, its groups and the waitlist, and status prints the same", async (t) => {
  const path = lockFile(t),
    record = tempDir(t);
  const run = execWithHostLock([process.execPath, "-e", "setTimeout(()=>{},600)"], {
    ...request(path, { item: "wi_exec", priority: "high", prioritySource: "flag" }),
    recordDirectory: record,
    stdio: "ignore",
    handleSignals: false,
  });
  const state = await until(() => {
    const s = readHostState(path);
    return s?.holder?.groups?.length ? s : null;
  }, "the exec holder and its group");
  const waiting = acquireHostLock(request(path, { item: "wi_waiter" }));
  await until(() => waitersOf(path).length === 1, "the waiter");
  const now = readHostState(path);
  assert.equal(now.version, 2);
  assert.equal(now.holderLimit, 1);
  assert.equal(now.holder.kind, "exec");
  assert.equal(now.holder.item, "wi_exec");
  assert.equal(now.holder.priority, "high");
  assert.equal(now.holder.pid, process.pid);
  assert.deepEqual(now.holder.groups, state.holder.groups);
  assert(!groupGone(now.holder.groups[0]), "the recorded group is the running command");
  assert.deepEqual(now.waiters.map((w) => [w.item, w.priority, w.prioritySource]), [["wi_waiter", "normal", "default"]]);
  assert.equal(statSync(path).mode & 0o777, 0o600);
  assert.equal(statSync(dirname(path)).mode & 0o777, 0o700);
  const text = statusText(path);
  assert.match(text, new RegExp(`holder: wi_exec/tester/pid ${process.pid} \\(exec, high/flag\\).*check groups ${now.holder.groups[0]} \\(alive: ${now.holder.groups[0]}\\)`));
  assert.match(text, /\n {2}1 of 1: wi_waiter\/tester\/pid \d+ \(run, normal\/default\)/);
  const json = JSON.parse(execFileSync(process.execPath, [moduleFile, "status", "--json"], { env: { ...process.env, TAILTERM_MATRIX_HOST_LOCK: path }, encoding: "utf8" }));
  assert.equal(json.path, path);
  assert.equal(json.state.holders[0].id, now.holder.id);
  assert.equal(await run, 0);
  await (await waiting).release();
  assert.equal(readHostState(path).holder, null);
});

test("priority comes from the flag, then the environment variable, then normal", () => {
  assert.deepEqual(resolvePriority(undefined, {}), { priority: "normal", prioritySource: "default" });
  assert.deepEqual(resolvePriority(undefined, { TAILTERM_MATRIX_PRIORITY: "high" }), { priority: "high", prioritySource: "environment" });
  assert.deepEqual(resolvePriority("urgent", { TAILTERM_MATRIX_PRIORITY: "high" }), { priority: "urgent", prioritySource: "flag" });
  assert.throws(() => resolvePriority("soon", {}), /--priority must be urgent, high or normal/);
  assert.throws(() => resolvePriority("", {}), /--priority must be/);
  assert.throws(() => resolvePriority(undefined, { TAILTERM_MATRIX_PRIORITY: "asap" }), /TAILTERM_MATRIX_PRIORITY must be/);
});

test("L1 behind a holder, requests are granted urgent, then high, then normal, FIFO within a priority", async (t) => {
  const path = lockFile(t);
  const holder = await acquireHostLock(request(path, { item: "wi_holder" }));
  const granted = [],
    runs = [];
  for (const [name, priority] of [["normal", "normal"], ["high-1", "high"], ["urgent", "urgent"], ["high-2", "high"]]) {
    runs.push(
      acquireHostLock(request(path, { item: name, priority, prioritySource: "flag" })).then(async (lease) => {
        granted.push(name);
        assert.equal(readHostState(path).holder.id, lease.id);
        await lease.release();
        return lease.record;
      }),
    );
    await until(() => waitersOf(path).some((w) => w.item === name), name + " to join");
  }
  assert.deepEqual(waitersOf(path).map((w) => w.item), ["urgent", "high-1", "high-2", "normal"], "the file lists the waitlist in grant order");
  assert.equal(granted.length, 0, "nobody is granted while the holder runs");
  await holder.release();
  const records = await Promise.all(runs);
  assert.deepEqual(granted, ["urgent", "high-1", "high-2", "normal"]);
  // normal queued first and watched three later requests go ahead of it.
  assert.deepEqual(records.map((r) => [r.item, r.queuePosition, r.queueLength, r.grantsBeforeStart, r.overtakenBy]), [
    ["normal", 1, 1, 3, 3],
    ["high-1", 1, 2, 1, 1],
    ["urgent", 1, 3, 0, 0],
    ["high-2", 3, 4, 2, 0],
  ]);
});

test("L2 same-priority requests with one identical timestamp are granted in arrival order", async (t) => {
  const path = lockFile(t),
    fixed = Date.parse("2026-10-01T00:00:00.000Z");
  const now = () => fixed;
  const holder = await acquireHostLock(request(path, { now }));
  const granted = [],
    runs = [];
  for (const name of ["first", "second", "third"]) {
    runs.push(
      acquireHostLock(request(path, { item: name, now })).then(async (lease) => {
        granted.push(name);
        await lease.release();
      }),
    );
    await until(() => waitersOf(path).some((w) => w.item === name), name + " to join");
  }
  const waiters = waitersOf(path);
  assert.deepEqual(new Set(waiters.map((w) => w.requestedAt)), new Set(["2026-10-01T00:00:00.000Z"]));
  assert.deepEqual(waiters.map((w) => w.seq), [2, 3, 4]);
  await holder.release();
  await Promise.all(runs);
  assert.deepEqual(granted, ["first", "second", "third"]);
});

test("L3 a killed runner whose check group still runs keeps the lock until the group is gone", async (t) => {
  const path = lockFile(t);
  const held = await heldByChild(t, path, { group: true });
  held.child.kill("SIGKILL");
  await exited(held.child);
  await until(() => pidGone(held.pid), "the killed runner to be gone");
  const lines = [];
  let granted = false;
  const waiting = acquireHostLock(request(path, { item: "wi_next", print: (line) => lines.push(line) })).then((lease) => {
    granted = true;
    return lease;
  });
  const expected = `matrix host: waiting 1 of 1, holder wi_child/tester/pid ${held.pid} gone, check group ${held.group} still running`;
  await until(() => lines.includes(expected), "the still-running line; saw " + lines.join(" | "));
  // Several more polls pass while the group lives.
  const polls = readJournal(path).length;
  await new Promise((resolve) => setTimeout(resolve, 200));
  assert.equal(granted, false, "not granted while the recorded group is alive");
  assert.equal(readHostState(path).holder.id, held.id);
  assert.equal(readJournal(path).length, polls, "no recovery was journaled yet");
  assert(!groupGone(held.group));
  kill(-held.group);
  const lease = await waiting;
  const recovery = readJournal(path).find((line) => line.event === "stale-recovered");
  assert.equal(recovery.reason, "pid-gone");
  assert.equal(recovery.holder, held.id);
  assert.equal(recovery.holderPid, held.pid);
  assert.equal(lease.record.overlap, 0);
  await lease.release();
});

test("L4 a holder past its run timeout stops its own command and releases", async (t) => {
  const path = lockFile(t),
    record = tempDir(t);
  let group;
  const run = execWithHostLock([process.execPath, "-e", "process.on('SIGTERM',()=>{});setInterval(()=>{},1000)"], {
    ...request(path, { runTimeoutMs: 400 }),
    recordDirectory: record,
    stdio: "ignore",
    handleSignals: false,
    killAfterMs: 100,
  });
  group = await until(() => readHostState(path)?.holder?.groups?.[0], "the command's group");
  t.after(() => kill(-group));
  assert.equal(await run, 124);
  assert(groupGone(group), "the command's group was stopped");
  assert.equal(readHostState(path).holder, null);
  assert.deepEqual(events(path), ["request", "acquire", "run-timeout-abort", "release"]);
  const sidecar = JSON.parse(readFileSync(join(record, "host-lock.json"), "utf8"));
  assert.equal(sidecar.runTimeoutAbort, true);
  assert.equal(sidecar.exitCode, 124);
});

// A one-check plan in a throwaway repository, run by the real matrix runner.
function runnerFixture(t, source) {
  const cwd = tempDir(t, "matrix-host-lock-repo-");
  const git = (...args) => execFileSync("git", args, { cwd, encoding: "utf8" }).trim();
  mkdirSync(join(cwd, "verification"));
  mkdirSync(join(cwd, "tests"));
  mkdirSync(join(cwd, "docs"));
  const matrix = JSON.stringify({ version: 1, browserSuites: [], excludedBrowserSuites: [], rules: [{ prefixes: ["docs/"], groups: ["unit"] }] });
  writeFileSync(join(cwd, "verification/matrix.json"), matrix);
  writeFileSync(join(cwd, "docs/a.md"), "a\n");
  writeFileSync(join(cwd, "check.mjs"), source);
  writeFileSync(join(cwd, "package.json"), JSON.stringify({ scripts: { test: "node check.mjs" } }));
  writeFileSync(join(cwd, ".gitignore"), "node_modules/\n");
  mkdirSync(join(cwd, "node_modules"));
  writeFileSync(join(cwd, "node_modules/.package-lock.json"), "{}");
  git("init", "-q");
  git("config", "user.name", "Fixture");
  git("config", "user.email", "fixture@example.invalid");
  git("add", ".");
  git("commit", "-qm", "fixture");
  const commit = git("rev-parse", "HEAD");
  git("checkout", "-q", "--detach", commit);
  const plan = makePlan({ baseCommit: commit, commit, owned: ["docs/"], approvedMatrixDigest: digest(matrix), matrixApprovalMessageSeq: 1 }, cwd);
  return { cwd, plan };
}
test("L4 a matrix run past its run timeout stops its check, writes no receipt and releases", async (t) => {
  const path = lockFile(t),
    output = tempDir(t, "matrix-host-lock-logs-"),
    pidFile = join(tempDir(t), "check.pid");
  const { cwd, plan } = runnerFixture(
    t,
    `import fs from 'node:fs';fs.writeFileSync(${JSON.stringify(pidFile)},String(process.pid));setInterval(()=>{},1000);`,
  );
  await assert.rejects(
    () => runPlan(plan, cwd, output, { minFreeBytes: 0, hostLock: { path, runTimeoutMs: 1500, pollMs: 20 } }),
    /Verification interrupted by run-timeout/,
  );
  assert(!existsSync(join(output, "receipt.json")), "no receipt");
  assert(existsSync(pidFile), "the check had started");
  assert(pidGone(Number(readFileSync(pidFile, "utf8"))), "the check was stopped");
  assert.equal(readHostState(path).holder, null);
  assert.deepEqual(events(path), ["request", "acquire", "run-timeout-abort", "release"]);
  assert.match(readFileSync(join(output, "host-lock.log"), "utf8"), /run timeout of 1500 ms reached, stopping this run/);
});

test("L5 past the run timeout plus grace a waiter takes the lock as a recorded overlap and signals nothing", async (t) => {
  const path = lockFile(t);
  const held = await heldByChild(t, path, { group: true, request: { runTimeoutMs: 300 } });
  held.child.kill("SIGKILL");
  await exited(held.child);
  const lines = [];
  const lease = await acquireHostLock(request(path, { item: "wi_next", graceMs: 300, print: (line) => lines.push(line) }));
  const heldSince = Date.parse(readJournal(path).find((line) => line.event === "acquire").at);
  assert(Date.parse(lease.record.acquiredAt) >= heldSince + 300 + 300 - 5, "not before the run timeout plus grace");
  assert.equal(lease.record.overlap, 1);
  assert.deepEqual(lease.record.overlapDetails.groups, [held.group]);
  const overlap = readJournal(path).find((line) => line.event === "overlap");
  assert.equal(overlap.reason, "run-timeout");
  assert.equal(overlap.holder, held.id);
  assert.deepEqual(overlap.groups, [held.group]);
  assert(lines.some((line) => /WARNING overlap, lock taken after run timeout from wi_child\/tester/.test(line) && line.includes(String(held.group))), lines.join(" | "));
  assert(lines.some((line) => line.includes(`pid ${held.pid} gone, check group ${held.group} still running`)), "it waited visibly first");
  assert(!groupGone(held.group), "the old run's group was never signalled");
  await lease.release();
});

test("L6 inside its run timeout a live holder is never taken over, whatever the waiter's priority or age", async (t) => {
  const path = lockFile(t);
  const holder = await acquireHostLock(request(path, { item: "wi_holder", runTimeoutMs: 60000 }));
  // The waiter's clock is just inside the holder's timeout plus grace.
  let polls = 0;
  const inside = Date.now() + 60000 - 2000;
  const lines = [];
  let granted = false;
  const waiting = acquireHostLock(
    request(path, { item: "wi_urgent", priority: "urgent", prioritySource: "flag", graceMs: 0, maxWaitMs: 86400000, now: () => (polls++, inside), print: (line) => lines.push(line) }),
  ).then((lease) => {
    granted = true;
    return lease;
  });
  await until(() => polls > 40, "many polls");
  assert.equal(granted, false);
  assert.equal(readHostState(path).holder.id, holder.id);
  assert.deepEqual(lines, [`matrix host: waiting 1 of 1, holder wi_holder/tester/pid ${process.pid}`]);
  await holder.release();
  const lease = await waiting;
  assert.equal(lease.record.overlap, 0);
  await lease.release();
  assert(!events(path).some((event) => RECOVERY.includes(event)));
});

test("L7 the lock is released on normal exit, an uncaught error, SIGTERM and SIGINT with no recovery", async (t) => {
  for (const after of ["exit", "throw"]) {
    const path = lockFile(t);
    const held = await heldByChild(t, path, { after });
    await exited(held.child);
    assert.equal(held.child.exitCode, after === "exit" ? 0 : 1, held.child.output());
    assert.equal(readHostState(path).holder, null, after);
    assert.deepEqual(events(path), ["request", "acquire", "release"], after);
    assert.equal(readJournal(path).at(-1).fallback, true);
  }
  for (const signal of ["SIGTERM", "SIGINT"]) {
    const path = lockFile(t),
      record = tempDir(t);
    const child = spawn(
      process.execPath,
      [moduleFile, "exec", "--item", "wi_signal", "--timeout-minutes", "5", "--record", record, "--", process.execPath, "-e", "setInterval(()=>{},1000)"],
      { env: { ...process.env, TAILTERM_MATRIX_HOST_LOCK: path }, stdio: "ignore" },
    );
    t.after(() => child.exitCode === null && child.signalCode === null && child.kill("SIGKILL"));
    const group = await until(() => readHostState(path)?.holder?.groups?.[0], "the exec command's group");
    t.after(() => kill(-group));
    child.kill(signal);
    const [code] = await once(child, "close");
    assert.equal(code, signal === "SIGINT" ? 130 : 143);
    assert(groupGone(group), "the signal reached the command's group");
    assert.equal(readHostState(path).holder, null, signal);
    assert.deepEqual(events(path), ["request", "acquire", "release"], signal);
  }
});

test("L8 a holder killed with no live group is recovered by the next request", async (t) => {
  const path = lockFile(t);
  const held = await heldByChild(t, path);
  held.child.kill("SIGKILL");
  await exited(held.child);
  const lease = await acquireHostLock(request(path, { item: "wi_next" }));
  const journal = readJournal(path);
  assert.deepEqual(journal.map((l) => l.event), ["request", "acquire", "request", "stale-recovered", "acquire"]);
  assert.equal(journal[3].reason, "pid-gone");
  assert.equal(journal[3].holderPid, held.pid);
  assert.deepEqual(lease.record.recovered, { reason: "pid-gone", holder: held.id, pid: held.pid });
  await lease.release();
});

function assertExclusive(log) {
  const lines = readFileSync(log, "utf8").trim().split("\n");
  for (let i = 0; i < lines.length; i += 2) {
    assert.match(lines[i], /^enter \d+$/, "line " + i);
    assert.equal(lines[i + 1], lines[i].replace("enter", "leave"), "two holders overlapped near line " + i);
  }
  return lines.length / 2;
}
async function race(t, path, children, count) {
  const script = childScript(t),
    log = join(tempDir(t), "holds.log");
  const running = Array.from({ length: children }, () =>
    startChild(t, script, { mode: "cycle", count, log, request: request(path, { pollMs: 5 }) }),
  );
  await Promise.all(running.map(exited));
  for (const child of running) assert.equal(child.exitCode, 0, child.output());
  return log;
}
test("L9 concurrent requests from several processes never hold together and none is lost", async (t) => {
  const path = lockFile(t);
  const log = await race(t, path, 5, 6);
  assert.equal(assertExclusive(log), 30);
  const state = readHostState(path);
  // Both counters change only under the file mutex.
  assert.equal(state.requestSeq, 30);
  assert.equal(state.grantSeq, 30);
  assert.equal(state.holder, null);
  assert.deepEqual(state.waiters, []);
  assert(!events(path).some((event) => RECOVERY.includes(event)));
});

test("L10 a waiter prints and logs its position and the holder, updates on change, and leaves at its wait bound", async (t) => {
  const path = lockFile(t),
    record = tempDir(t);
  const holder = await acquireHostLock(request(path, { item: "wi_holder", agent: "verifier-a" }));
  const lines = [];
  const first = acquireHostLock(request(path, { item: "wi_first", recordDirectory: record, print: (line) => lines.push(line) }));
  const holderText = `holder wi_holder/verifier-a/pid ${process.pid}`;
  await until(() => lines.length === 1, "the first waiting line");
  assert.deepEqual(lines, [`matrix host: waiting 1 of 1, ${holderText}`]);
  const urgent = acquireHostLock(request(path, { item: "wi_urgent", priority: "urgent", prioritySource: "flag" }));
  await until(() => lines.length === 2, "the line after being overtaken");
  assert.equal(lines[1], `matrix host: waiting 2 of 2, ${holderText}`);

  // A waiter whose process is gone is dropped by the others.
  const dead = startChild(t, childScript(t), { mode: "hold", marker: join(tempDir(t), "never"), request: request(path, { item: "wi_dead" }) });
  await until(() => waitersOf(path).some((w) => w.item === "wi_dead"), "the doomed waiter");
  await until(() => lines.length === 3, "the line counting three waiters");
  assert.equal(lines[2], `matrix host: waiting 2 of 3, ${holderText}`);
  dead.kill("SIGKILL");
  await until(() => !waitersOf(path).some((w) => w.item === "wi_dead"), "the dead waiter to be dropped");
  const dropped = readJournal(path).find((line) => line.event === "waiter-dropped");
  assert.equal(dropped.item, "wi_dead");
  assert.equal(dropped.pid, dead.pid);

  // The wait bound: the waiter removes itself and names where it stood.
  const expiredRecord = tempDir(t);
  await assert.rejects(
    () => acquireHostLock(request(path, { item: "wi_bounded", maxWaitMs: 150, recordDirectory: expiredRecord })),
    (error) => {
      assert.equal(error.code, "wait-expired");
      assert.equal(error.exitCode, EXIT_WAIT_EXPIRED);
      assert.equal(error.exitCode, 75);
      assert.match(error.message, new RegExp(`^Host lock wait expired after 150 ms at position 3 of 3, ${holderText}$`));
      return true;
    },
  );
  assert(!waitersOf(path).some((w) => w.item === "wi_bounded"));
  const expired = readJournal(path).find((line) => line.event === "wait-expired");
  assert.equal(expired.item, "wi_bounded");
  assert.equal(expired.position, 3);
  assert.equal(expired.removed, true);
  const sidecar = JSON.parse(readFileSync(join(expiredRecord, "host-lock.json"), "utf8"));
  assert.equal(sidecar.outcome, "wait-expired");
  assert(sidecar.waitMs >= 150);
  assert.equal(readHostState(path).holder.id, holder.id, "the holder was not disturbed");

  await holder.release();
  const urgentLease = await urgent;
  await until(() => lines.some((line) => line.startsWith("matrix host: waiting 1 of 1, holder wi_urgent/")), "the first waiter to see the new holder");
  await urgentLease.release();
  const lease = await first;
  assert.match(lines.at(-1), /^matrix host: acquired after \d+ ms$/);
  const logged = readFileSync(join(record, "host-lock.log"), "utf8").trim().split("\n");
  assert.deepEqual(logged.map((line) => line.replace(/^\S+ /, "")), lines, "host-lock.log carries the same lines");
  for (const line of logged) assert.match(line, /^\d{4}-\d\d-\d\dT[\d:.]+Z matrix host: /);
  await lease.release();
});

test("L11 a paused writer's mutex is never reclaimed, and its update lands intact when it resumes", async (t) => {
  const path = lockFile(t),
    directory = tempDir(t);
  const seed = await acquireHostLock(request(path, { item: "wi_seed" }));
  await seed.release();
  const marker = join(directory, "in-update"),
    go = join(directory, "go");
  const writer = startChild(t, childScript(t), { mode: "pause", marker, go, request: { path } });
  await until(() => existsSync(marker), "the writer to hold the mutex");
  writer.kill("SIGSTOP");
  t.after(() => kill(writer.pid, "SIGCONT"));
  const before = readFileSync(path, "utf8"),
    journalBefore = readJournal(path).length;
  const lines = [];
  let granted = false;
  const waiting = acquireHostLock(request(path, { item: "wi_requester", print: (line) => lines.push(line) })).then((lease) => {
    granted = true;
    return lease;
  });
  await until(() => lines.length, "the busy line");
  assert.deepEqual(lines, [`matrix host: lock file busy, mutex owner pid ${writer.pid}`]);
  await new Promise((resolve) => setTimeout(resolve, 150));
  assert.equal(granted, false);
  assert.equal(readFileSync(path, "utf8"), before, "the lock file is unchanged");
  assert.equal(JSON.parse(readFileSync(lockPaths(path).mutex, "utf8")).pid, writer.pid, "the mutex is still the writer's");
  assert.equal(readJournal(path).length, journalBefore, "nothing was reclaimed or journaled");
  writeFileSync(go, "");
  writer.kill("SIGCONT");
  await exited(writer);
  assert.equal(writer.exitCode, 0, writer.output());
  const lease = await waiting;
  assert.equal(readHostState(path).requestSeq, 1002, "the paused update and the request both landed");
  assert.equal(lease.record.seq, 1002);
  assert(!events(path).includes("mutex-recovered"));
  await lease.release();
});

test("L12 a dead owner's mutex is reclaimed exactly once while several processes race", async (t) => {
  const path = lockFile(t);
  mkdirSync(dirname(path), { recursive: true });
  const deadPid = exitedPid();
  assert(pidGone(deadPid));
  writeFileSync(lockPaths(path).mutex, JSON.stringify({ pid: deadPid, token: "left-behind", at: new Date().toISOString() }));
  const log = await race(t, path, 5, 6);
  assert.equal(assertExclusive(log), 30);
  const recovered = readJournal(path).filter((line) => line.event === "mutex-recovered");
  assert.equal(recovered.length, 1);
  assert.equal(recovered[0].deadPid, deadPid);
  assert.equal(recovered[0].token, "left-behind");
  const state = readHostState(path);
  assert.equal(state.requestSeq, 30);
  assert.equal(state.grantSeq, 30);
  assert.equal(state.holder, null);
  assert(!existsSync(lockPaths(path).guard), "the reclaim guard was removed");
});

test("L13 a corrupt lock file refuses requests and is left unchanged, even with a live holder", async (t) => {
  const path = lockFile(t);
  const lines = [];
  const holder = await acquireHostLock(request(path, { item: "wi_holder", print: (line) => lines.push(line) }));
  for (const bytes of ["{ not json", "", JSON.stringify({ version: 2, requestSeq: 1, grantSeq: 1, holder: null, waiters: [] })]) {
    writeFileSync(path, bytes);
    await assert.rejects(
      () => acquireHostLock(request(path, { item: "wi_refused" })),
      (error) => {
        assert.equal(error.code, "corrupt-file");
        assert.equal(error.exitCode, EXIT_LOCK_UNUSABLE);
        assert.equal(error.exitCode, 78);
        assert(error.message.includes(path), error.message);
        return true;
      },
    );
    assert.equal(readFileSync(path, "utf8"), bytes, "the file bytes are unchanged");
    const refusal = readJournal(path).at(-1);
    assert.equal(refusal.event, "corrupt-file");
    assert.equal(refusal.item, "wi_refused");
    assert.equal(refusal.file, path);
  }
  assert(!existsSync(lockPaths(path).mutex), "the mutex was released after each refusal");
  const bytes = readFileSync(path, "utf8");
  await holder.release();
  assert.equal(readFileSync(path, "utf8"), bytes, "the holder's release did not overwrite it either");
  assert(lines.some((line) => line.startsWith("matrix host: WARNING Host lock file " + path)), lines.join(" | "));
  assert.equal(readJournal(path).at(-1).event, "corrupt-file");
});

test("L14 a reclaim guard left by a dead process fails closed and names the file", async (t) => {
  const path = lockFile(t),
    paths = lockPaths(path);
  mkdirSync(dirname(path), { recursive: true });
  const deadPid = exitedPid();
  writeFileSync(paths.mutex, JSON.stringify({ pid: deadPid, token: "left-behind", at: new Date().toISOString() }));
  writeFileSync(paths.guard, JSON.stringify({ pid: deadPid, token: "guard", at: new Date().toISOString() }));
  await assert.rejects(
    () => acquireHostLock(request(path)),
    (error) => {
      assert.equal(error.code, "guard-stale");
      assert.equal(error.exitCode, 78);
      assert(error.message.includes(paths.guard), error.message);
      return true;
    },
  );
  assert(existsSync(paths.mutex) && existsSync(paths.guard), "neither file was removed");
  assert(!existsSync(path), "no lock was granted");
  const stale = readJournal(path).at(-1);
  assert.equal(stale.event, "guard-stale");
  assert.equal(stale.file, paths.guard);
  assert.equal(stale.guardPid, deadPid);
});

test("L15 exec records its wait, position, overtakes, duration and load, and passes the exit code through", async (t) => {
  const path = lockFile(t),
    records = tempDir(t);
  const environment = { ...process.env, TAILTERM_MATRIX_HOST_LOCK: path };
  const exec = (name, sleepMs, code, flags = []) => {
    const child = spawn(
      process.execPath,
      [moduleFile, "exec", "--item", name, "--agent", "racer", "--timeout-minutes", "5", "--record", join(records, name), ...flags, "--", process.execPath, "-e", `setTimeout(()=>process.exit(${code}),${sleepMs})`],
      { env: environment, stdio: ["ignore", "pipe", "pipe"] },
    );
    let output = "";
    child.stdout.on("data", (data) => (output += data));
    child.stderr.on("data", (data) => (output += data));
    child.output = () => output;
    t.after(() => child.exitCode === null && child.signalCode === null && child.kill("SIGKILL"));
    return child;
  };
  const holder = await acquireHostLock(request(path, { item: "wi_holder" }));
  const a = exec("wi_a", 700, 3);
  await until(() => waitersOf(path).some((w) => w.item === "wi_a"), "A to queue");
  const b = exec("wi_b", 600, 0, ["--priority", "urgent"]);
  await until(() => waitersOf(path).length === 2, "B to queue");
  const status = JSON.parse(execFileSync(process.execPath, [moduleFile, "status", "--json"], { env: environment, encoding: "utf8" }));
  assert.deepEqual(status.state.waiters.map((w) => [w.item, w.priority]), [["wi_b", "urgent"], ["wi_a", "normal"]], "status shows B ahead of A");
  assert.match(execFileSync(process.execPath, [moduleFile, "status"], { env: environment, encoding: "utf8" }), /1 of 2: wi_b\/racer\/pid \d+ \(exec, urgent\/flag\).*\n {2}2 of 2: wi_a\/racer\/pid \d+ \(exec, normal\/default\)/);
  await holder.release();
  const [[codeA], [codeB]] = await Promise.all([once(a, "close"), once(b, "close")]);
  assert.equal(codeA, 3, a.output());
  assert.equal(codeB, 0, b.output());
  const recordOf = (name) => JSON.parse(readFileSync(join(records, name, "host-lock.json"), "utf8"));
  const ra = recordOf("wi_a"),
    rb = recordOf("wi_b");
  assert.deepEqual(
    [rb.queuePosition, rb.queueLength, rb.overtakenBy, rb.grantsBeforeStart, rb.priority, rb.prioritySource, rb.exitCode],
    [1, 2, 0, 0, "urgent", "flag", 0],
  );
  assert.deepEqual(
    [ra.queuePosition, ra.queueLength, ra.overtakenBy, ra.grantsBeforeStart, ra.priority, ra.prioritySource, ra.exitCode],
    [1, 1, 1, 1, "normal", "default", 3],
  );
  assert(rb.durationMs >= 600, "B held for its command: " + rb.durationMs);
  assert(ra.waitMs >= rb.durationMs, `A waited ${ra.waitMs} ms, at least B's hold of ${rb.durationMs} ms`);
  assert(ra.durationMs >= 700 && ra.durationMs < 60000, "A's duration covers its sleep: " + ra.durationMs);
  assert(Date.parse(ra.acquiredAt) >= Date.parse(rb.releasedAt) - 5, "A started after B released");
  for (const record of [ra, rb]) {
    assert.equal(record.checkSet, "supplemental");
    assert.equal(record.kind, "exec");
    assert.equal(record.agent, "racer");
    assert.equal(record.overlap, 0);
    assert.equal(record.outcome, "released");
    assert.equal(record.lockPath, path);
    assert(record.cpuCount >= 1);
    assert(record.loadSamples.length >= 2);
    for (const sample of record.loadSamples) {
      assert(Number.isFinite(sample.load1) && sample.load1 >= 0);
      assert(Number.isFinite(sample.load5) && sample.load5 >= 0);
      assert(!Number.isNaN(Date.parse(sample.at)));
    }
  }
  assert.equal(ra.argv.at(-1), "setTimeout(()=>process.exit(3),700)");
  assert.match(a.output(), /matrix host: waiting 1 of 1, holder wi_holder\/tester\/pid \d+\n/);
  assert.match(readFileSync(join(records, "wi_a", "host-lock.log"), "utf8"), /matrix host: acquired after \d+ ms\n$/);
  assert.equal(readHostState(path).holder, null);
});

test("exec refuses bad options before joining the list", (t) => {
  const path = lockFile(t),
    record = tempDir(t);
  const run = (...args) => spawnSync(process.execPath, [moduleFile, "exec", ...args], { env: { ...process.env, TAILTERM_MATRIX_HOST_LOCK: path }, encoding: "utf8" });
  const ok = ["--item", "wi_x", "--timeout-minutes", "1", "--record", record];
  for (const [args, message] of [
    [[...ok.slice(2), "--", "true"], /exec requires --item ID/],
    [[...ok.slice(0, 2), "--record", record, "--", "true"], /exec requires --timeout-minutes N/],
    [["--item", "wi_x", "--timeout-minutes", "0", "--record", record, "--", "true"], /--timeout-minutes requires a whole number of minutes from 1 to 1440/],
    [["--item", "wi_x", "--timeout-minutes", "1", "--record", "relative", "--", "true"], /absolute directory/],
    [[...ok, "--priority", "soon", "--", "true"], /--priority must be urgent, high or normal/],
    [[...ok, "--host-wait-minutes", "x", "--", "true"], /--host-wait-minutes requires a whole number/],
    [ok, /Usage:/],
  ]) {
    const result = run(...args);
    assert.equal(result.status, 1, result.stderr);
    assert.match(result.stderr, message);
  }
  assert(!existsSync(path), "no request reached the lock file");
});

// Holder bound (wi_c5cb667695c3614c, d5). Plan shapes for the scheduler: the
// ids, cwd, argv and ports are what its lock, lane and barrier rules read.
const planCheck = (id, minutes, extra = {}) => ({
  id,
  cwd: ".",
  argv: ["node", id],
  ...extra,
  environment: { VERIFICATION_TIMEOUT_MS: String(minutes * 60000), ...(extra.environment || {}) },
});
const goCheck = (id, minutes) => planCheck(id, minutes, { cwd: "hub", argv: ["go", "test", id] });
const portCheck = (id, minutes, port) => planCheck(id, minutes, { environment: { VERIFICATION_REQUIRED_PORTS: String(port) } });
function fullPlan() {
  const checks = [
    planCheck("00-static-build", 10),
    planCheck("01-static-release-verify", 10),
    planCheck("wasm-test-build", 10),
    goCheck("go-vet", 10),
    goCheck("go-test", 20),
    goCheck("go-race", 30),
  ];
  for (let i = 0; checks.length < 68; i++)
    checks.push(i % 4 === 0 ? portCheck("browser-" + i, 10, 4300 + (i % 3)) : planCheck("unit-" + i, 10));
  return { maxAttempts: 3, checks };
}
const SHAPES = {
  "the 68-check fixture": fullPlan(),
  "three exclusive 30-minute checks": {
    maxAttempts: 1,
    checks: ["00-static-build", "01-static-release-verify", "wasm-test-build"].map((id) => planCheck(id, 30)),
  },
  "a chain sharing one port": { maxAttempts: 3, checks: [1, 2, 3, 4, 5, 6].map((i) => portCheck("browser-" + i, 15, 4173)) },
  "a Go lane with go-race": {
    maxAttempts: 3,
    checks: [goCheck("go-test", 20), goCheck("go-race", 30), goCheck("go-vet", 5), goCheck("go-build", 10), planCheck("unit", 10)],
  },
};
// The real scheduler on a virtual clock: every check takes its whole timeout
// on every attempt, and the clock jumps to the next finish once the scheduler
// has started everything it can.
async function makespan(plan, jobs) {
  let clock = 0;
  const running = [];
  const done = runScheduled(plan.checks, { jobs }, (check) =>
    new Promise((finish) =>
      running.push({ at: clock + Number(check.environment.VERIFICATION_TIMEOUT_MS) * (plan.maxAttempts || 1), finish }),
    ),
  );
  let settled = false;
  done.then(
    () => (settled = true),
    () => (settled = true),
  );
  for (;;) {
    await new Promise((resolve) => setImmediate(resolve));
    if (settled) break;
    assert(running.length, "the scheduler is idle with checks left");
    running.sort((a, b) => a.at - b.at);
    const next = running.shift();
    clock = next.at;
    next.finish({});
  }
  await done;
  return clock;
}

test("a8 (i) a 68-check plan's holder bound is the 120-minute cap, at most half the waiters' bound", async (t) => {
  const plan = fullPlan();
  assert.equal(plan.checks.length, 68);
  const old = planRunTimeout(plan);
  assert(old > 24 * 3600000, "the serial sum is above 24 hours: " + old);
  const directory = tempDir(t);
  const lines = [];
  const lease = await acquireHostLock(request(lockFile(t), { runTimeoutMs: old, recordDirectory: directory, print: (line) => lines.push(line) }));
  assert.equal(lease.record.runTimeoutMs, 7200000);
  assert.equal(lease.record.runTimeoutMs, DEFAULT_HOLDER_CAP_MS);
  assert.equal(lease.record.requestedRunTimeoutMs, old);
  assert(lease.record.runTimeoutMs <= DEFAULT_HOST_WAIT_MS / 2);
  assert.equal(readHostState(lease.path).holder.runTimeoutMs, 7200000, "the lock file carries the clamped deadline");
  assert.deepEqual(lines.filter((line) => line.includes("clamped")), [`matrix host: run timeout ${old} ms clamped to the holder cap of 7200000 ms`]);
  await lease.release();
  assert.equal(JSON.parse(readFileSync(join(directory, "host-lock.json"), "utf8")).requestedRunTimeoutMs, old);
});

test("a8 (ii) the requested, uncapped timeout is never below the real scheduler's all-timeouts makespan", async () => {
  for (const [name, plan] of Object.entries(SHAPES))
    for (const jobs of [1, 2, 5, 8]) {
      const requested = planRunTimeout(plan);
      const took = await makespan(plan, jobs);
      assert(took > 0 && requested >= took, `${name} at ${jobs} jobs: requested ${requested} ms, makespan ${took} ms`);
    }
  assert.equal(await makespan(SHAPES["three exclusive 30-minute checks"], 5), 90 * 60000, "exclusive checks run one after another whatever the job count");
  assert.equal(await makespan(SHAPES["a chain sharing one port"], 8), 6 * 15 * 3 * 60000, "a shared port is a chain");
});

test("a9 (i) a request above the cap is recorded clamped and stops itself at the cap, for exec too", async (t) => {
  const path = lockFile(t);
  const environment = { TAILTERM_MATRIX_HOLDER_CAP_MINUTES: "1" };
  const directory = tempDir(t);
  t.mock.timers.enable({ apis: ["setTimeout"] });
  const lease = await acquireHostLock(request(path, { runTimeoutMs: 600000, environment, recordDirectory: directory }));
  assert.equal(readHostState(path).holder.runTimeoutMs, 60000);
  assert.deepEqual([lease.record.runTimeoutMs, lease.record.requestedRunTimeoutMs], [60000, 600000]);
  t.mock.timers.tick(59999);
  assert.equal(lease.signal.aborted, false, "not before the cap");
  t.mock.timers.tick(1);
  assert.equal(lease.signal.aborted, true, "the holder stops itself at the cap, not at the requested timeout");
  assert.equal(lease.signal.reason, "run-timeout");
  await lease.release();
  const sidecar = JSON.parse(readFileSync(join(directory, "host-lock.json"), "utf8"));
  assert.deepEqual([sidecar.runTimeoutMs, sidecar.requestedRunTimeoutMs, sidecar.runTimeoutAbort], [60000, 600000, true]);

  const record = tempDir(t);
  const running = execWithHostLock([process.execPath, "-e", "setInterval(()=>{},1000)"], {
    path,
    item: "wi_exec",
    agent: "tester",
    runTimeoutMs: 600000,
    environment,
    recordDirectory: record,
    stdio: "ignore",
    handleSignals: false,
  });
  // Timers stay mocked, so this waits on the event loop rather than a timer.
  let holder;
  for (let i = 0; i < 200000 && !(holder = readHostState(path)?.holder)?.groups?.length; i++)
    await new Promise((resolve) => setImmediate(resolve));
  assert.equal(holder?.kind, "exec", "the exec command is on file");
  assert.equal(holder.runTimeoutMs, 60000);
  t.mock.timers.tick(59999);
  await new Promise((resolve) => setImmediate(resolve));
  assert(!readJournal(path).some((line) => line.event === "run-timeout-abort" && line.item === "wi_exec"), "not before the cap");
  t.mock.timers.tick(1);
  assert.equal(await running, 124);
  const execSidecar = JSON.parse(readFileSync(join(record, "host-lock.json"), "utf8"));
  assert.deepEqual([execSidecar.kind, execSidecar.runTimeoutMs, execSidecar.requestedRunTimeoutMs], ["exec", 60000, 600000]);
});

test("a9 (ii) the cap comes from TAILTERM_MATRIX_HOLDER_CAP_MINUTES, and an invalid value refuses before joining", async (t) => {
  assert.equal(holderCapMs({}), 7200000);
  assert.equal(holderCapMs({ TAILTERM_MATRIX_HOLDER_CAP_MINUTES: "" }), 7200000);
  assert.equal(holderCapMs({ TAILTERM_MATRIX_HOLDER_CAP_MINUTES: "1" }), 60000);
  assert.equal(holderCapMs({ TAILTERM_MATRIX_HOLDER_CAP_MINUTES: "1440" }), 86400000);
  const path = lockFile(t);
  for (const bad of ["0", "x", "1441"]) {
    await assert.rejects(
      acquireHostLock(request(path, { environment: { TAILTERM_MATRIX_HOLDER_CAP_MINUTES: bad } })),
      /TAILTERM_MATRIX_HOLDER_CAP_MINUTES requires a whole number of minutes from 1 to 1440/,
    );
    assert.equal(readHostState(path), null, "nothing joined the list for " + bad);
    assert.deepEqual(readJournal(path), []);
  }
  const lease = await acquireHostLock(request(path, { runTimeoutMs: 60001, environment: { TAILTERM_MATRIX_HOLDER_CAP_MINUTES: "1" } }));
  assert.equal(lease.record.runTimeoutMs, 60000);
  await lease.release();
  const under = await acquireHostLock(request(path, { runTimeoutMs: 59000, environment: { TAILTERM_MATRIX_HOLDER_CAP_MINUTES: "1" } }));
  assert.deepEqual([under.record.runTimeoutMs, under.record.requestedRunTimeoutMs], [59000, 59000], "a request under the cap is kept");
  await under.release();
});

test("a9 (iii) a waiter under a smaller cap never shortens the deadline a living holder recorded", async (t) => {
  const cases = { "an older checkout's unclamped 33-hour deadline": 2000 * 60000, "a deadline recorded under a larger cap": 240 * 60000 };
  for (const [name, recorded] of Object.entries(cases)) {
    const path = lockFile(t);
    const holder = await acquireHostLock(request(path, { item: "wi_holder", runTimeoutMs: 60000 }));
    assert(updateHostState(path, (state) => void (state.holders[0].runTimeoutMs = recorded)).done);
    const started = Date.parse(readHostState(path).holder.startedAt);
    // First the waiter's clock is far past its own one-minute cap, yet inside
    // the holder's recorded deadline plus grace.
    let clock = started + recorded + RUN_TIMEOUT_GRACE_MS - 1000,
      polls = 0,
      granted = false;
    const waiting = acquireHostLock(
      request(path, {
        item: "wi_waiter",
        priority: "urgent",
        prioritySource: "flag",
        maxWaitMs: 400 * 86400000,
        environment: { TAILTERM_MATRIX_HOLDER_CAP_MINUTES: "1" },
        now: () => (polls++, clock),
      }),
    ).then((lease) => ((granted = true), lease));
    await until(() => polls > 40, "many polls");
    assert.equal(granted, false, name + ": not taken over inside the recorded deadline");
    assert.equal(readHostState(path).holder.id, holder.id);
    assert.equal(readHostState(path).holder.runTimeoutMs, recorded, "the recorded deadline is left as written");
    clock = started + recorded + RUN_TIMEOUT_GRACE_MS + 1000;
    const lease = await waiting;
    assert.equal(lease.record.overlap, 1, name + ": taken only past the holder's own timeout plus grace");
    assert.equal(lease.record.runTimeoutMs, 60000, "the waiter clamps only itself");
    await lease.release();
    await holder.release();
  }
});

// wi_af52350b616dd0e4 / order21517 / ASSIGN22512: real default-two cohorts.
const two = { environment: { TAILTERM_MATRIX_MAX_HOLDERS: undefined }, maxWaitMs: 5000 };
const allHeld = path => holdersOf(rawReadHostState(path));
async function releaseChild(held) { held.child.kill("SIGUSR1"); await exited(held.child); }
test("two holders run concurrently; either freed slot grants priority then FIFO with a third waiting", async t => {
  for (const slot of [0, 1]) {
    const path = lockFile(t);
    const cohort = slot === 0 ? two : { ...two, environment: { TAILTERM_MATRIX_MAX_HOLDERS: "" } };
    const peers = [await heldByChild(t, path, { request: { ...cohort, item: "A" } }), await heldByChild(t, path, { request: { ...cohort, item: "B" } })];
    assert.equal(rawReadHostState(path).version, 2); assert.equal(rawReadHostState(path).holderLimit, 2);
    assert.equal(allHeld(path).length, 2);
    const requests = [], order = [];
    for (const [item, priority] of [["normal", "normal"], ["high-first", "high"], ["urgent", "urgent"], ["high-second", "high"]]) {
      requests.push(acquireHostLock(request(path, { ...two, item, priority })).then(async lease => {
        order.push(item); assert(allHeld(path).some(h => h.id === lease.id)); assert.equal(allHeld(path).length, 2);
        assert.equal(lease.record.overlap, 0); await lease.release();
      }));
      await until(() => waitersOf(path).some(w => w.item === item), item);
    }
    assert.deepEqual(order, []); assert.equal(waitersOf(path).length, 4);
    await releaseChild(peers[slot]); await Promise.all(requests);
    assert.deepEqual(order, ["urgent", "high-first", "high-second", "normal"]);
    assert.deepEqual(allHeld(path).map(h => h.id), [peers[1-slot].id]);
    await releaseChild(peers[1-slot]); assert.deepEqual(allHeld(path), []);
    assert.equal(events(path).filter(e => e === "acquire").length, 6);
    assert(!events(path).includes("overlap"));
  }
});
test("holder configuration is strict, defaults to two, and capacity freezes until the cohort drains", async t => {
  assert.equal(holderLimit({}), 2); assert.equal(holderLimit({ TAILTERM_MATRIX_MAX_HOLDERS: "" }), 2);
  for (const value of ["0", "-1", "2.1", " 2", "2 ", "x", "17", "02"])
    assert.throws(() => holderLimit({ TAILTERM_MATRIX_MAX_HOLDERS: value }), /decimal integer/);
  const path = lockFile(t), lease = await acquireHostLock(request(path, two));
  const before = readFileSync(path, "utf8"), seq = rawReadHostState(path).requestSeq;
  await assert.rejects(acquireHostLock(request(path, { environment: { TAILTERM_MATRIX_MAX_HOLDERS: "3" } })), /frozen at 2/);
  assert.equal(readFileSync(path, "utf8"), before); assert.equal(rawReadHostState(path).requestSeq, seq);
  const record = join(tempDir(t), "invalid-sidecar");
  await assert.rejects(acquireHostLock(request(path, { environment: { TAILTERM_MATRIX_MAX_HOLDERS: "0" }, recordDirectory: record })), /decimal integer/);
  assert(!existsSync(record)); await lease.release();
  const leases = [];
  for (let i = 0; i < 3; i++) leases.push(await acquireHostLock(request(path, { environment: { TAILTERM_MATRIX_MAX_HOLDERS: "3" } })));
  assert.equal(allHeld(path).length, 3); for (const l of leases) await l.release();
});
test("an occupied v1 cohort drains at capacity one then migrates without losing counters", async t => {
  const path = lockFile(t), owner = await acquireHostLock(request(path, { environment: { TAILTERM_MATRIX_MAX_HOLDERS: "1" } }));
  const state = rawReadHostState(path), original = allHeld(path)[0];
  writeFileSync(path, JSON.stringify({ ...state, version: 1, holder: original, holders: undefined, holderLimit: undefined }));
  const queued = acquireHostLock(request(path, two)); await until(() => waitersOf(path).length === 1, "v1 waiter");
  assert.equal(rawReadHostState(path).version, 1); await owner.release(); const next = await queued;
  assert.equal(rawReadHostState(path).version, 1); assert.equal(allHeld(path).length, 1); await next.release();
  const migrated = await acquireHostLock(request(path, two));
  assert.equal(rawReadHostState(path).version, 2); assert.equal(rawReadHostState(path).requestSeq, 3); assert.equal(rawReadHostState(path).grantSeq, 3);
  await migrated.release();
  // The original parser's v1-only gate refuses these bytes, never interpreting a free singleton.
  const old = execFileSync("git", ["show", "235f87402cfca765cf0051b6421e767592f3be63:scripts/verify-matrix-host-lock.mjs"], { encoding: "utf8" });
  const oldFile = join(tempDir(t), "old-lock.mjs"); writeFileSync(oldFile, old);
  const before = readFileSync(path, "utf8");
  const attempt = spawnSync(process.execPath, [canonicalResource(oldFile), "status", "--json"], { env: { ...process.env, TAILTERM_MATRIX_HOST_LOCK: path }, encoding: "utf8" });
  assert.equal(attempt.status, EXIT_LOCK_UNUSABLE); assert.match(attempt.stderr, /unknown version/); assert.equal(readFileSync(path, "utf8"), before);
});
test("unknown or malformed v2 state is refused byte-for-byte", async t => {
  const path = lockFile(t);
  for (const value of [{ version: 9 }, { version: 2, holderLimit: 0, holders: [], waiters: [], requestSeq: 0, grantSeq: 0 },
    { version: 2, holderLimit: 2, holders: [{ id: "x", pid: 0 }], waiters: [], requestSeq: 0, grantSeq: 0 }]) {
    const raw = JSON.stringify(value); writeFileSync(path, raw);
    await assert.rejects(acquireHostLock(request(path, two)), /unusable/); assert.equal(readFileSync(path, "utf8"), raw);
  }
});
test("canonical worktree head cannot be bypassed and duplicate output aliases refuse before sidecars", async t => {
  const path = lockFile(t), root = tempDir(t), alias = join(tempDir(t), "alias"); symlinkSync(root, alias);
  assert.equal(canonicalResource(join(alias, "new")), join(canonicalResource(root), "new"));
  const output = join(root, "records"), sidecars = join(root, "sidecars"), owner = await acquireHostLock(request(path, { ...two, worktree: root, output, recordDirectory: sidecars }));
  const queued = acquireHostLock(request(path, { ...two, worktree: alias, item: "head" }));
  await until(() => waitersOf(path).length === 1, "blocked head");
  const later = acquireHostLock(request(path, { ...two, worktree: tempDir(t), item: "later" }));
  await until(() => waitersOf(path).length === 2, "later waiter");
  assert.equal(allHeld(path).length, 1, "head not skipped despite spare capacity");
  const before = readFileSync(path, "utf8");
  await assert.rejects(acquireHostLock(request(path, { ...two, output: join(alias, "records"), recordDirectory: join(alias, "records") })), /already in use/);
  assert.equal(readFileSync(path, "utf8"), before); assert(!existsSync(output), "no rejected sidecar directory");
  const peerRecord = readFileSync(join(sidecars, "host-lock.json"), "utf8"), otherOutput = join(root, "other-output");
  await assert.rejects(acquireHostLock(request(path, { ...two, output: otherOutput, recordDirectory: join(alias, "sidecars") })), /already in use/);
  assert.equal(readFileSync(path, "utf8"), before); assert(!existsSync(otherOutput));
  assert.equal(readFileSync(join(sidecars, "host-lock.json"), "utf8"), peerRecord);
  const mutex = lockPaths(path).mutex;
  writeFileSync(mutex, JSON.stringify({ pid: process.pid, token: "busy-fixture" }));
  try {
    await assert.rejects(acquireHostLock(request(path, { ...two, output: otherOutput, recordDirectory: sidecars, maxWaitMs: 60 })), /wait expired/);
    assert.equal(readFileSync(join(sidecars, "host-lock.json"), "utf8"), peerRecord);
    assert.equal(readFileSync(path, "utf8"), before);
  } finally { rmSync(mutex, { force: true }); }
  await owner.release(); const head = await queued, tail = await later;
  assert.equal(allHeld(path).length, 2); await head.release(); await tail.release();
});
test("group tracking, release and exit recovery address only the exact holder and leave its live peer", async t => {
  const path = lockFile(t), first = await heldByChild(t, path, { group: true, request: two }), second = await heldByChild(t, path, { group: true, request: two });
  assert.equal(allHeld(path).length, 2); assert(allHeld(path).every(h => h.groups.length === 1));
  const secondBefore = structuredClone(allHeld(path).find(h => h.id === second.id));
  await releaseChild(first); assert.deepEqual(allHeld(path), [secondBefore]); assert(!groupGone(second.group));
  const own = await acquireHostLock(request(path, two)); own.addGroup(first.group); own.removeGroup(first.group); await own.release();
  assert.deepEqual(allHeld(path), [secondBefore]);
  second.child.kill("SIGKILL"); await exited(second.child); kill(-second.group); await until(() => groupGone(second.group), "second group gone");
  const recovered = await acquireHostLock(request(path, two)); assert.equal(allHeld(path).length, 1); assert.equal(recovered.record.overlap, 0);
  assert(events(path).includes("stale-recovered")); await recovered.release();
});
test("a confirmed-dead output lease can be retried while live or group-owning duplicates remain protected", async t => {
  for (const group of [false, true]) {
    const path = lockFile(t), output = tempDir(t), peerOutput = tempDir(t);
    const dead = await heldByChild(t, path, { group, request: { ...two, output, recordDirectory: output } });
    const peer = await acquireHostLock(request(path, { ...two, output: peerOutput, recordDirectory: peerOutput }));
    try {
      const peerBefore = structuredClone(allHeld(path).find(h => h.id === peer.id));
      dead.child.kill("SIGKILL"); await exited(dead.child); assert(pidGone(dead.pid));
      if (group) {
        assert(!groupGone(dead.group));
        const before = readFileSync(path, "utf8"), sidecar = readFileSync(join(output, "host-lock.json"), "utf8");
        await assert.rejects(acquireHostLock(request(path, { ...two, output, recordDirectory: output })), /already in use/);
        assert.equal(readFileSync(path, "utf8"), before); assert.equal(readFileSync(join(output, "host-lock.json"), "utf8"), sidecar);
        kill(-dead.group); await until(() => groupGone(dead.group), "dead holder group gone");
      }
      const retry = await acquireHostLock(request(path, { ...two, output, recordDirectory: output }));
      try {
        assert.equal(retry.record.overlap, 0); assert.equal(retry.record.recovered.holder, dead.id);
        assert.equal(allHeld(path).length, 2); assert.deepEqual(allHeld(path).find(h => h.id === peer.id), peerBefore);
        assert(events(path).includes("stale-recovered"));
        const before = readFileSync(path, "utf8"), sidecar = readFileSync(join(output, "host-lock.json"), "utf8");
        await assert.rejects(acquireHostLock(request(path, { ...two, output, recordDirectory: output })), /already in use/);
        assert.equal(readFileSync(path, "utf8"), before); assert.equal(readFileSync(join(output, "host-lock.json"), "utf8"), sidecar);
      } finally { await retry.release(); }
    } finally { await peer.release(); }
  }
});

test("two real runPlan invocations have private homes, tmux namespaces and ports; cancelling one preserves its peer", async t => {
  const inheritedTmp = process.env.TMPDIR, longTmp = join(tempDir(t), "private-" + "x".repeat(120));
  mkdirSync(longTmp); process.env.TMPDIR = longTmp;
  t.after(() => { if (inheritedTmp === undefined) delete process.env.TMPDIR; else process.env.TMPDIR = inheritedTmp; });
  const path = lockFile(t), markers = [join(tempDir(t), "a.json"), join(tempDir(t), "b.json")], done = join(tempDir(t), "done");
  const source = (marker, finish) => `import fs from 'node:fs'; import net from 'node:net'; import {spawnSync} from 'node:child_process'; import {randomUUID} from 'node:crypto';
    const tmuxSocket='tailterm-menu-test-'+randomUUID();
    const tmux=spawnSync('tmux',['-L',tmuxSocket,'new-session','-d','sleep 120'],{encoding:'utf8'}); if(tmux.status!==0)throw new Error(tmux.stderr);
    const tmuxPid=Number(spawnSync('tmux',['-L',tmuxSocket,'display-message','-p','#{pid}'],{encoding:'utf8'}).stdout.trim());
    process.once('SIGTERM',()=>{spawnSync('tmux',['-L',tmuxSocket,'kill-server']);process.exit(0);});
    const server=net.createServer(s=>s.end('peer alive')); server.listen(0,'127.0.0.1',()=>fs.writeFileSync(${JSON.stringify(marker)},JSON.stringify({pid:process.pid,tmuxPid,tmuxSocket,port:server.address().port,HOME:process.env.HOME,TMPDIR:process.env.TMPDIR,TMUX_TMPDIR:process.env.TMUX_TMPDIR,GOPATH:process.env.GOPATH,GOMODCACHE:process.env.GOMODCACHE,GOCACHE:process.env.GOCACHE})));
    setInterval(()=>{if(${finish} && fs.existsSync(${JSON.stringify(done)})){spawnSync('tmux',['-L',tmuxSocket,'kill-server']);server.close(()=>process.exit(0));}},30);`;
  const a = runnerFixture(t, source(markers[0], false)), b = runnerFixture(t, source(markers[1], true));
  const outs = [tempDir(t), tempDir(t)], abort = new AbortController();
  const first = runPlan(a.plan, a.cwd, outs[0], { minFreeBytes: 0, abortSignal: abort.signal, hostLock: { path, ...two, pollMs: 20 } });
  const second = runPlan(b.plan, b.cwd, outs[1], { minFreeBytes: 0, hostLock: { path, ...two, pollMs: 20 } });
  const outcomes = [];
  [first, second].forEach((promise, i) => promise.then(receipt => { outcomes[i] = { receipt }; }, error => { outcomes[i] = { error }; }));
  let failure;
  try {
  await until(() => {
    const early = outcomes.find(Boolean);
    if (early?.error) throw early.error;
    if (early) throw new Error("Fixture completed before cancellation; retained check logs follow");
    return markers.every(existsSync);
  }, "both real checks started", 30000);
  const [ea, eb] = markers.map(f=>JSON.parse(readFileSync(f,"utf8")));
  assert.equal(allHeld(path).length, 2); assert.notEqual(ea.port, eb.port);
  for (const key of ["HOME", "TMPDIR", "TMUX_TMPDIR", "GOPATH", "GOMODCACHE", "GOCACHE"])assert.notEqual(ea[key],eb[key],key);
  for(const e of [ea,eb]) {
    const socket = join(e.TMUX_TMPDIR,`tmux-${process.getuid()}`,e.tmuxSocket);
    assert.equal(Buffer.byteLength(e.tmuxSocket), 55, "unchanged longest participating menu socket name");
    assert(Buffer.byteLength(socket) < 104, "private namespace fits the host Unix socket bound despite long inherited TMPDIR");
    const maximumSocket = join(e.TMUX_TMPDIR, "tmux-4294967295", e.tmuxSocket);
    assert(Buffer.byteLength(maximumSocket) <= 103, "full unsigned32 UID and longest name fit with NUL reserved");
    assert.equal(statSync(e.HOME).mode & 0o777, 0o700);
    assert.equal(statSync(e.TMUX_TMPDIR).mode & 0o777, 0o700);
    assert(existsSync(socket), "private actual tmux socket");
    console.log(JSON.stringify({ socketBudget: { socket, actualBytes: Buffer.byteLength(socket), maximumBytes: Buffer.byteLength(maximumSocket) } }));
  }
  abort.abort("cancel-peer"); await assert.rejects(first, /Verification interrupted by cancel-peer/);
  await until(()=>pidGone(ea.tmuxPid), "cancelled fixture tmux cleaned");assert(!pidGone(eb.tmuxPid));
  assert(!existsSync(ea.HOME)); assert(existsSync(eb.HOME)); assert(!pidGone(eb.pid));
  assert.equal(allHeld(path).length, 1); assert.equal(allHeld(path)[0].output, canonicalResource(outs[1]));
  const {createConnection}=await import('node:net');
  const socket=createConnection({host:'127.0.0.1',port:eb.port}); const message=await once(socket,'data');assert.equal(message[0].toString(),'peer alive');socket.destroy();
  assert(!existsSync(join(outs[0],"receipt.json"))); writeFileSync(done,"done"); await second;
  assert(existsSync(join(outs[1],"receipt.json"))); assert(!existsSync(eb.HOME));assert.deepEqual(allHeld(path),[]);
  await until(()=>pidGone(eb.tmuxPid), "completed fixture tmux cleaned");
  for (const out of outs)assert.equal(JSON.parse(readFileSync(join(out,"host-lock.json"),"utf8")).overlap,0);
  } catch (error) { failure = error; throw error; }
  finally {
    abort.abort("cleanup"); writeFileSync(done,"done");
    const cleanup = await Promise.allSettled([first, second]);
    if (failure) for (const [i, out] of outs.entries()) {
      const files = {};
      try { for (const file of readdirSync(out)) if (/\.(log|json)$/.test(file)) files[file] = readFileSync(join(out, file), "utf8"); }
      catch (error) { files.readError = error.message; }
      console.error(JSON.stringify({ fixture: i, output: out, cleanup: cleanup[i].status,
        cleanupError: cleanup[i].reason?.message, files }));
    }
  }
});

test("a third real child remains waiting until one of two real holder children releases", async t => {
 const path=lockFile(t),a=await heldByChild(t,path,{request:two}),b=await heldByChild(t,path,{request:two});
 const marker=join(tempDir(t),"third"),child=startChild(t,childScript(t),{mode:"hold",marker,request:request(path,{...two,item:"C"})});
 await until(()=>waitersOf(path).some(w=>w.item==="C"),"third child queued");assert(!existsSync(marker));assert.equal(allHeld(path).length,2);
 await releaseChild(b);await until(()=>existsSync(marker),"third child granted");const c=JSON.parse(readFileSync(marker,"utf8"));
 assert.deepEqual(new Set(allHeld(path).map(h=>h.id)),new Set([a.id,c.id]));
 assert.equal(readJournal(path).filter(e=>e.event==="acquire").at(-1).overlap,0);
 await releaseChild({child});await releaseChild(a);assert.deepEqual(allHeld(path),[]);
});
test("overdue takeover in a two-holder cohort records exceptional overlap and leaves the other peer intact", async t => {
 const path=lockFile(t),a=await heldByChild(t,path,{group:true,request:{...two,runTimeoutMs:300}}),b=await heldByChild(t,path,{group:true,request:two});
 const peer=structuredClone(allHeld(path).find(h=>h.id===b.id));
 const next=await acquireHostLock(request(path,{...two,graceMs:20}));
 assert.equal(next.record.overlap,1);assert.equal(next.record.overlapDetails.holder,a.id);assert(!pidGone(a.pid));assert(!groupGone(a.group));
 assert.deepEqual(allHeld(path).find(h=>h.id===b.id),peer);assert(!groupGone(b.group));await next.release();await releaseChild(a);await releaseChild(b);
});


test("a complete socket budget rejection removes its allocated home before launching checks", async t => {
  const marker = join(tempDir(t), "check-started");
  const fixture = runnerFixture(t, `import fs from 'node:fs'; fs.writeFileSync(${JSON.stringify(marker)}, 'started');`);
  const output = tempDir(t), path = lockFile(t);
  const parent = join(tempDir(t), "private-" + "x".repeat(120));
  mkdirSync(parent);
  let home, groups = 0;
  await assert.rejects(runPlan(fixture.plan, fixture.cwd, output, {
    minFreeBytes: 0,
    hostLock: { path, ...two, pollMs: 20 },
    createHome: () => { home = mkdtempSync(join(parent, "tv-")); return home; },
    onGroup: () => { groups++; },
  }), /Private verifier tmux socket budget exceeds 103 bytes/);
  assert(home, "real private home allocated before refusal");
  assert(!existsSync(home), "real allocated home cleanup after refusal");
  assert(!existsSync(marker), "no check launched");
  assert.equal(groups, 0, "no process group launched");
  assert(!existsSync(join(output, "receipt.json")), "no acceptance receipt");
  assert(!existsSync(join(output, "cleanup-error.json")), "cleanup succeeded");
  assert.deepEqual(allHeld(path), [], "own lease released after refusal");
  const lock = JSON.parse(readFileSync(join(output, "host-lock.json"), "utf8"));
  assert.equal(lock.outcome, "released");
  assert.equal(lock.overlap, 0);
  console.log(JSON.stringify({ socketBudgetRejection: { home, homeRemoved: !existsSync(home), groups, noCheck: !existsSync(marker), noReceipt: !existsSync(join(output, "receipt.json")) } }));
});
