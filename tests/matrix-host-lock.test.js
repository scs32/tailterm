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
  overtakeLimit,
  canonicalResource,
  updateHostState,
  memoryPressureMax,
  readMemoryPressure,
  releaseRun,
  receiptKeys,
  DEFAULT_MEMORY_PRESSURE_MAX,
  MEMORY_PROBE_MS,
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
delete process.env.TAILTERM_MATRIX_OVERTAKE_LIMIT;
// No test here may read the host's memory pressure either: the feature is off
// unless a test injects a reading, and a sysctl stub placed first on PATH
// records any probe that still gets through, child processes included (M12).
process.env.TAILTERM_MATRIX_MEMORY_PRESSURE_MAX = "off";
delete process.env.TAILTERM_MATRIX_RELEASE;
const probeMarker = join(isolated, "sysctl-ran");
mkdirSync(join(isolated, "bin"));
writeFileSync(join(isolated, "bin", "sysctl"), `#!/bin/sh\n: >> '${probeMarker}'\necho 50\n`, { mode: 0o755 });
process.env.PATH = join(isolated, "bin") + ":" + process.env.PATH;
process.on("exit", () => rmSync(isolated, { recursive: true, force: true }));

const moduleFile = fileURLToPath(new URL("../scripts/verify-matrix-host-lock.mjs", import.meta.url));
const moduleURL = new URL("../scripts/verify-matrix-host-lock.mjs", import.meta.url).href;
const fixtureURL = new URL("./agent-window-size-fixture.js", import.meta.url).href;

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
  environment: { TAILTERM_MATRIX_MAX_HOLDERS: "1", TAILTERM_MATRIX_MEMORY_PRESSURE_MAX: "off", ...extra.environment },
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
// A marker appears whole or not at all: the parent reads it as soon as it exists.
const mark = (file, text) => { fs.writeFileSync(file + '.part', text); fs.renameSync(file + '.part', file); };
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
  mark(o.marker, JSON.stringify({ pid: process.pid, id: lease.id, group }));
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
    mark(o.marker, String(process.pid));
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

// wi_5fbd2fe86826fc33 / order23970 / ASSIGN23978: bounded non-urgent overtakes.
test("steady high arrivals cannot overtake a normal waiter beyond its configured limit", async t => {
  for (const capacity of [1, 2]) for (const limit of [2, 1, 3]) {
    const path = lockFile(t), abort = new AbortController();
    const environment = { TAILTERM_MATRIX_MAX_HOLDERS: String(capacity),
      ...(limit === 2 ? {} : { TAILTERM_MATRIX_OVERTAKE_LIMIT: String(limit) }) };
    let holder = await acquireHostLock(request(path, { environment }));
    const peer = capacity === 2 ? await acquireHostLock(request(path, { environment, item: "peer" })) : null;
    const normal = acquireHostLock(request(path, { environment, item: "normal", signal: abort.signal }));
    const runs = [normal];
    // Attach a handler even if a regression makes cleanup withdraw the waiter.
    normal.catch(() => {});
    try {
      await until(() => waitersOf(path).some(w => w.item === "normal"), "normal queued");
      for (let i = 0; i < limit; i++) {
        const high = acquireHostLock(request(path, { environment, item: "high-" + i, priority: "high", signal: abort.signal }));
        high.catch(() => {}); runs.push(high);
        await until(() => waitersOf(path).some(w => w.item === "high-" + i), "high queued");
        await holder.release(); holder = await high;
        assert.equal(waitersOf(path).find(w => w.item === "normal").overtakenBy, i + 1);
      }
      const protectedState = waitersOf(path).find(w => w.item === "normal");
      assert.equal(protectedState.nonUrgentOvertakes, limit);
      assert.equal(protectedState.overtakeLimit, limit);
      assert(statusText(path).includes(`non-urgent overtakes ${limit}/${limit}; protected from later non-urgent overtaking (urgent exempt)`));
      const urgent = acquireHostLock(request(path, { environment: { TAILTERM_MATRIX_MAX_HOLDERS: String(capacity) }, item: "urgent", priority: "urgent", signal: abort.signal }));
      urgent.catch(() => {}); runs.push(urgent);
      await until(() => waitersOf(path).some(w => w.item === "urgent"), "urgent queued");
      const late = acquireHostLock(request(path, { environment, item: "late-high", priority: "high", signal: abort.signal }));
      late.catch(() => {}); runs.push(late);
      await until(() => waitersOf(path).some(w => w.item === "late-high"), "late high queued");
      assert.deepEqual(waitersOf(path).map(w => w.item), ["urgent", "normal", "late-high"], "only urgent passes the protected normal");
      await holder.release(); holder = await urgent;
      const protectedAfterUrgent = waitersOf(path).find(w => w.item === "normal");
      assert.equal(protectedAfterUrgent.nonUrgentOvertakes, limit, "urgent does not consume fairness budget");
      assert.equal(protectedAfterUrgent.overtakenBy, limit + 1, "total still counts urgent");
      assert.deepEqual(waitersOf(path).map(w => w.item), ["normal", "late-high"]);
      const text = execFileSync(process.execPath, [moduleFile, "status"], { env: { ...process.env, TAILTERM_MATRIX_HOST_LOCK: path }, encoding: "utf8" });
      assert.match(text, /protected from later non-urgent overtaking \(urgent exempt\)/);
      await holder.release(); holder = await normal;
      assert.equal(holder.record.overtakenBy, limit + 1);
      assert.equal(holder.record.nonUrgentOvertakes, limit);
      await holder.release(); holder = await late;
    } finally {
      abort.abort("fixture cleanup"); await holder.release(); await peer?.release();
      await Promise.allSettled(runs);
    }
  }
});

test("fairness configuration is strict and rejected before creating lock or sidecars", async t => {
  assert.equal(overtakeLimit({}), 2);
  assert.equal(overtakeLimit({ TAILTERM_MATRIX_OVERTAKE_LIMIT: "" }), 2);
  assert.equal(overtakeLimit({ TAILTERM_MATRIX_OVERTAKE_LIMIT: "1" }), 1);
  assert.equal(overtakeLimit({ TAILTERM_MATRIX_OVERTAKE_LIMIT: "1000" }), 1000);
  for (const raw of ["0", "-1", "1.5", "2x", " 2", "02", "1001"]) {
    const path = lockFile(t), record = join(tempDir(t), "record");
    await assert.rejects(acquireHostLock(request(path, { environment: { TAILTERM_MATRIX_OVERTAKE_LIMIT: raw }, recordDirectory: record })), /decimal integer from 1 to 1000/);
    assert(!existsSync(path)); assert(!existsSync(record));
  }
});

test("a younger protected waiter preserves older same-priority FIFO across mixed budgets", async t => {
  const path = lockFile(t), abort = new AbortController(), runs = [];
  let holder = await acquireHostLock(request(path));
  const queue = async (item, priority, limit) => {
    const run = acquireHostLock(request(path, { item, priority, signal: abort.signal,
      environment: { TAILTERM_MATRIX_OVERTAKE_LIMIT: String(limit) } }));
    run.catch(() => {}); runs.push(run);
    await until(() => waitersOf(path).some(w => w.item === item), item + " queued");
    return { run };
  };
  try {
    const older = await queue("older", "normal", 3), younger = await queue("younger", "normal", 1);
    const first = await queue("first-high", "high", 2);
    await holder.release(); holder = await first.run;
    const late = await queue("late-high", "high", 2);
    assert.deepEqual(waitersOf(path).map(w => w.item), ["older", "younger", "late-high"]);
    for (const queued of [older, younger, late]) { await holder.release(); holder = await queued.run; }
  } finally { abort.abort("fixture cleanup"); await holder.release(); await Promise.allSettled(runs); }
});

test("legacy waiter counters retain protection and malformed fairness fields fail closed", async t => {
  for (const version of [1, 2]) {
    const path = lockFile(t), abort = new AbortController(), runs = [];
    let holder = await acquireHostLock(request(path));
    const normal = acquireHostLock(request(path, { item: "legacy-normal", signal: abort.signal }));
    normal.catch(() => {}); runs.push(normal);
    try {
      await until(() => waitersOf(path).length === 1, "legacy normal queued");
      updateHostState(path, state => {
        const w = state.waiters[0];
        delete w.nonUrgentOvertakes; delete w.overtakeLimit; w.overtakenBy = 2;
        if (version === 1) {
          state.version = 1; state.holder = state.holders[0];
          delete state.holders; delete state.holderLimit;
        }
      });
      const late = acquireHostLock(request(path, { item: "late-high", priority: "high", signal: abort.signal }));
      late.catch(() => {}); runs.push(late);
      await until(() => waitersOf(path).length === 2, "legacy late high queued");
      assert.deepEqual(waitersOf(path).map(w => w.item), ["legacy-normal", "late-high"]);
      assert(statusText(path).includes("non-urgent overtakes 2/2; protected"));
      await holder.release(); holder = await normal;
      assert.equal(holder.record.nonUrgentOvertakes, 2);
      await holder.release(); holder = await late;
    } finally { abort.abort("fixture cleanup"); await holder.release(); await Promise.allSettled(runs); }
  }
  const path = lockFile(t), holder = await acquireHostLock(request(path));
  const original = readFileSync(path, "utf8"); await holder.release();
  for (const fields of [{ overtakeLimit: 0 }, { overtakeLimit: 1001 }, { overtakeLimit: "2" },
    { nonUrgentOvertakes: -1 }, { nonUrgentOvertakes: 0.5 }]) {
    const state = JSON.parse(original); Object.assign(state.holders[0], fields);
    const raw = JSON.stringify(state); writeFileSync(path, raw);
    assert.throws(() => rawReadHostState(path), /invalid fairness counters/);
    await assert.rejects(acquireHostLock(request(path)), /invalid fairness counters/);
    assert.equal(readFileSync(path, "utf8"), raw);
  }
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
  // Registered before the temporary directories, so it runs before they are
  // removed: a request still polling when an assertion fails is withdrawn and
  // awaited here instead of rejecting after the test has ended.
  const ended = new AbortController(),
    requests = [];
  const ask = (extra) => {
    const asked = acquireHostLock(request(path, { signal: ended.signal, ...extra }));
    requests.push(asked);
    return asked;
  };
  t.after(async () => {
    ended.abort("the end of the test");
    for (const settled of await Promise.allSettled(requests)) await settled.value?.release();
  });
  const path = lockFile(t),
    record = tempDir(t);
  const holder = await ask({ item: "wi_holder", agent: "verifier-a" });
  const lines = [];
  // The file mutex can be busy for one poll under load. That line is printed
  // too, and is not one of the waiting lines.
  const waiting = () => lines.filter((line) => !line.startsWith("matrix host: lock file busy, mutex owner pid "));
  const first = ask({ item: "wi_first", recordDirectory: record, print: (line) => lines.push(line) });
  const holderText = `holder wi_holder/verifier-a/pid ${process.pid}`;
  await until(() => waiting().length === 1, "the first waiting line");
  assert.deepEqual(waiting(), [`matrix host: waiting 1 of 1, ${holderText}`]);
  const urgent = ask({ item: "wi_urgent", priority: "urgent", prioritySource: "flag" });
  await until(() => waiting().length === 2, "the line after being overtaken");
  assert.equal(waiting()[1], `matrix host: waiting 2 of 2, ${holderText}`);

  // A waiter whose process is gone is dropped by the others.
  const dead = startChild(t, childScript(t), { mode: "hold", marker: join(tempDir(t), "never"), request: request(path, { item: "wi_dead" }) });
  await until(() => waitersOf(path).some((w) => w.item === "wi_dead"), "the doomed waiter");
  await until(() => waiting().length === 3, "the line counting three waiters");
  assert.equal(waiting()[2], `matrix host: waiting 2 of 3, ${holderText}`);
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
  // Nothing is written for a request before it joins the list, so a busy line
  // printed ahead of the first waiting line is the one line the log omits.
  const sinceJoining = lines.slice(lines.indexOf(waiting()[0]));
  assert.deepEqual(logged.map((line) => line.replace(/^\S+ /, "")), sinceJoining, "host-lock.log carries the same lines");
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
const two = { environment: { TAILTERM_MATRIX_MAX_HOLDERS: undefined, TAILTERM_MATRIX_MEMORY_PRESSURE_MAX: "off" }, maxWaitMs: 5000 };
const allHeld = path => holdersOf(rawReadHostState(path));
async function releaseChild(held) { held.child.kill("SIGUSR1"); await exited(held.child); }
test("two holders run concurrently; either freed slot grants priority then FIFO with a third waiting", async t => {
  for (const slot of [0, 1]) {
    const path = lockFile(t);
    const cohort = slot === 0 ? two : { ...two, environment: { ...two.environment, TAILTERM_MATRIX_MAX_HOLDERS: "" } };
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
  const source = (marker, finish) => `import fs from 'node:fs'; import net from 'node:net'; import {spawnSync} from 'node:child_process'; import {randomUUID} from 'node:crypto'; import {readTmuxFormat} from ${JSON.stringify(fixtureURL)};
    const tmuxSocket='tailterm-menu-test-'+randomUUID();
    const tmux=spawnSync('tmux',['-L',tmuxSocket,'new-session','-d','sleep 120'],{encoding:'utf8'}); if(tmux.status!==0)throw new Error(tmux.stderr);
    const tmuxRead=(...args)=>{const r=spawnSync('tmux',['-L',tmuxSocket,...args],{encoding:'utf8'}); if(r.status!==0)throw new Error(r.stderr||String(r.error)); return r.stdout;};
    const tmuxPid=Number(readTmuxFormat(tmuxRead,['display-message','-p','#{pid}'],{shape:/^\\d+$/}));
    process.once('SIGTERM',()=>{spawnSync('tmux',['-L',tmuxSocket,'kill-server']);process.exit(0);});
    const server=net.createServer(s=>s.end('peer alive')); server.listen(0,'127.0.0.1',()=>{fs.writeFileSync(${JSON.stringify(marker + ".part")},JSON.stringify({pid:process.pid,tmuxPid,tmuxSocket,port:server.address().port,HOME:process.env.HOME,TMPDIR:process.env.TMPDIR,TMUX_TMPDIR:process.env.TMUX_TMPDIR,GOPATH:process.env.GOPATH,GOMODCACHE:process.env.GOMODCACHE,GOCACHE:process.env.GOCACHE}));fs.renameSync(${JSON.stringify(marker + ".part")},${JSON.stringify(marker)});});
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
    acquireHostLock: async options => {
      const lease = await acquireHostLock(options);
      return { ...lease, addGroup: pgid => { groups++; return lease.addGroup(pgid); } };
    },
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

// wi_fc5776e011eaabe1 / order 26885 / ASSIGN 26917: memory-aware admission.
// Every reading is injected; two slots and the default limit of 70.
const gated = { environment: { TAILTERM_MATRIX_MAX_HOLDERS: undefined, TAILTERM_MATRIX_MEMORY_PRESSURE_MAX: "" }, maxWaitMs: 30000 };
const items = entries => entries.map(e => e.item);
function meter(reading) {
  const gauge = { reading, calls: 0 };
  gauge.memoryPressure = () => { gauge.calls++; if (gauge.reading instanceof Error) throw gauge.reading; return gauge.reading; };
  return gauge;
}
const asks = (path, gauge, extra) => acquireHostLock(request(path, { ...gated, memoryPressure: gauge.memoryPressure, ...extra }));
const heldAs = (path, item, reason) => until(() => waitersOf(path).some(w => w.item === item && w.heldReason === reason), `${item} held by ${reason}`);
const journaled = (path, event, id) => readJournal(path).filter(line => line.event === event && (!id || line.id === id));

test("M1 over the limit the head keeps its place behind one holder, and is admitted when the reading drops", async t => {
  const path = lockFile(t), gauge = meter(71), granted = [];
  const holder = await asks(path, gauge, { item: "holder" });
  assert.equal(gauge.calls, 0);
  const head = asks(path, gauge, { item: "head" }).then(lease => (granted.push("head"), lease));
  await heldAs(path, "head", "memory");
  const later = asks(path, gauge, { item: "later" }).then(lease => (granted.push("later"), lease));
  await heldAs(path, "later", "memory");
  const seen = gauge.calls;
  await until(() => gauge.calls >= seen + 3, "three more readings over the limit");
  assert.deepEqual(items(waitersOf(path)), ["head", "later"], "the head is still first");
  assert.deepEqual(items(allHeld(path)), ["holder"]); assert.deepEqual(granted, []);
  gauge.reading = 70;
  const second = await head;
  assert.deepEqual(items(allHeld(path)), ["holder", "head"], "two holders on file");
  assert.equal(second.record.overlap, 0); assert.deepEqual(granted, ["head"]);
  await heldAs(path, "later", "slots");
  await holder.release(); await (await later).release(); await second.release();
  assert.deepEqual(allHeld(path), []);
});
test("M2 while the reading stays over the head is admitted when the holder releases, and the next waiter still waits", async t => {
  const path = lockFile(t), gauge = meter(95);
  const holder = await asks(path, gauge, { item: "holder" });
  const head = asks(path, gauge, { item: "head" }); await heldAs(path, "head", "memory");
  const next = asks(path, gauge, { item: "next" }); await heldAs(path, "next", "memory");
  await holder.release();
  const lease = await head;
  assert.deepEqual(items(allHeld(path)), ["head"]); assert.equal(lease.record.overlap, 0);
  const seen = gauge.calls;
  await until(() => gauge.calls >= seen + 3, "the next waiter's own readings");
  assert.deepEqual(items(waitersOf(path)), ["next"]); assert.equal(waitersOf(path)[0].heldReason, "memory");
  assert.deepEqual(items(allHeld(path)), ["head"], "one holder while the reading is over");
  await lease.release(); await (await next).release();
  assert(!events(path).includes("overlap"));
});
test("M3 with no holder a reading of 100 admits at once and is never taken", async t => {
  const path = lockFile(t), gauge = meter(100);
  const lease = await asks(path, gauge, { item: "alone" });
  assert.equal(gauge.calls, 0); assert.deepEqual(events(path), ["request", "acquire"]);
  assert.equal(lease.record.memoryHeldMs, 0); assert(!Object.hasOwn(lease.record, "memoryReading"));
  await lease.release();
  // A waiter left alone by its holder is admitted the same way.
  const holder = await asks(path, gauge, { item: "holder" }), waiting = asks(path, gauge, { item: "waiter" });
  await heldAs(path, "waiter", "memory"); await holder.release();
  const next = await waiting; assert.deepEqual(items(allHeld(path)), ["waiter"]); await next.release();
});
test("M4 a reading at or below the limit admits the second holder as before", async t => {
  for (const reading of [DEFAULT_MEMORY_PRESSURE_MAX, 12, 0]) {
    const path = lockFile(t), gauge = meter(reading);
    const first = await asks(path, gauge), second = await asks(path, gauge, { item: "second" });
    assert.equal(allHeld(path).length, 2); assert.equal(second.record.overlap, 0);
    assert.equal(gauge.calls, 1); assert.equal(second.record.memoryReading, reading); assert.equal(second.record.memoryHeldMs, 0);
    assert.deepEqual(journaled(path, "memory-hold"), []);
    await first.release(); await second.release();
  }
  // A lower configured limit moves the boundary.
  const path = lockFile(t), gauge = meter(41), lower = { environment: { ...gated.environment, TAILTERM_MATRIX_MEMORY_PRESSURE_MAX: "40" } };
  const first = await asks(path, gauge, lower), waiting = asks(path, gauge, { ...lower, item: "second" });
  await heldAs(path, "second", "memory"); gauge.reading = 40;
  const second = await waiting; assert.equal(allHeld(path).length, 2);
  await first.release(); await second.release();
});
test("M5 an unreadable or unsupported reading admits by slot count with one memory-unreadable journal line", async t => {
  const unreadable = [() => null, () => NaN, () => 101, () => -1, () => "55", () => undefined, () => { throw new Error("probe broke"); }, () => readMemoryPressure({ platform: "linux" })];
  for (const memoryPressure of unreadable) {
    const path = lockFile(t), lines = [];
    const first = await acquireHostLock(request(path, { ...gated, memoryPressure }));
    const second = await acquireHostLock(request(path, { ...gated, memoryPressure, item: "second", print: line => lines.push(line) }));
    assert.equal(allHeld(path).length, 2); assert.equal(second.record.overlap, 0);
    assert.equal(journaled(path, "memory-unreadable").length, 1);
    assert.equal(journaled(path, "memory-unreadable", second.id).length, 1);
    assert.equal(typeof journaled(path, "memory-unreadable")[0].reason, "string");
    assert.deepEqual(journaled(path, "memory-hold"), []);
    assert.equal(lines.filter(line => /memory pressure unreadable \(.+\), admitting by slot count/.test(line)).length, 1, lines.join(" | "));
    assert(!Object.hasOwn(second.record, "memoryReading"));
    await first.release(); await second.release();
  }
  // With both slots still full by count, an unreadable reading admits nobody.
  const path = lockFile(t), broken = meter(new Error("probe broke"));
  const first = await asks(path, broken), second = await asks(path, broken, { item: "second" });
  const third = asks(path, broken, { item: "third" }); await heldAs(path, "third", "slots");
  assert.equal(allHeld(path).length, 2); assert.equal(broken.calls, 1);
  await first.release(); const last = await third; assert.equal(allHeld(path).length, 2);
  await second.release(); await last.release();
});
test("M6 waiters persist heldReason memory or slots, status shows it, and holders carry none", async t => {
  const path = lockFile(t), gauge = meter(88), lines = [];
  const holder = await asks(path, gauge, { item: "holder" });
  const head = asks(path, gauge, { item: "head", print: line => lines.push(line) }); await heldAs(path, "head", "memory");
  const behind = asks(path, gauge, { item: "behind" }); await heldAs(path, "behind", "memory");
  assert.deepEqual(rawReadHostState(path).waiters.map(w => [w.item, w.heldReason]), [["head", "memory"], ["behind", "memory"]]);
  const held = statusText(path).split("\n").filter(line => / of 2: /.test(line));
  assert.equal(held.length, 2); for (const line of held) assert.match(line, /; held: memory$/);
  assert(lines.some(line => /^matrix host: waiting 1 of \d, holder holder\/tester\/pid \d+; memory pressure over the limit, one holder admitted$/.test(line)), lines.join(" | "));
  gauge.reading = 10;
  const second = await head; await heldAs(path, "behind", "slots");
  assert.equal(allHeld(path).length, 2);
  for (const entry of allHeld(path)) assert(!Object.hasOwn(entry, "heldReason"), "holders carry no held reason");
  assert.match(statusText(path), / 1 of 1: .*; held: slots$/m); assert.doesNotMatch(statusText(path), /held: memory/);
  assert.equal(JSON.parse(execFileSync(process.execPath, [moduleFile, "status", "--json"], { env: { ...process.env, TAILTERM_MATRIX_HOST_LOCK: path }, encoding: "utf8" })).state.waiters[0].heldReason, "slots");
  await holder.release(); const third = await behind;
  for (const entry of allHeld(path)) assert(!Object.hasOwn(entry, "heldReason"));
  await second.release(); await third.release();
});
test("M7 a hold journals one memory-hold and the sidecar records it, with the twelve receipt keys unchanged", async t => {
  const path = lockFile(t), gauge = meter(93), record = join(tempDir(t), "record");
  const holder = await asks(path, gauge, { item: "holder" });
  const waiting = asks(path, gauge, { item: "head", recordDirectory: record }); await heldAs(path, "head", "memory");
  const seen = gauge.calls; await until(() => gauge.calls >= seen + 3, "a hold lasting several readings");
  gauge.reading = 30;
  const lease = await waiting, holds = journaled(path, "memory-hold");
  assert.equal(holds.length, 1); assert.deepEqual([holds[0].id, holds[0].reading, holds[0].max], [lease.id, 93, 70]);
  const sidecar = JSON.parse(readFileSync(join(record, "host-lock.json"), "utf8"));
  assert.equal(sidecar.memoryPressureMax, 70); assert.equal(sidecar.memoryReading, 30);
  assert(Number.isSafeInteger(sidecar.memoryHeldMs) && sidecar.memoryHeldMs > 0, String(sidecar.memoryHeldMs));
  assert(sidecar.memoryHeldMs <= sidecar.waitMs);
  assert.equal(journaled(path, "acquire", lease.id)[0].memoryHeldMs, sidecar.memoryHeldMs);
  assert.match(readFileSync(join(record, "host-lock.log"), "utf8"), /memory pressure over the limit, one holder admitted/);
  assert.deepEqual(Object.keys(receiptKeys(lease.finish({ checkSet: "unit" }))), [
    "VERIFICATION_HOST_LOCK", "VERIFICATION_HOST_PRIORITY", "VERIFICATION_HOST_PRIORITY_SOURCE", "VERIFICATION_HOST_WAIT_MS",
    "VERIFICATION_HOST_QUEUE_POSITION", "VERIFICATION_HOST_QUEUE_LENGTH", "VERIFICATION_HOST_GRANTS_BEFORE_START",
    "VERIFICATION_HOST_OVERTAKEN_BY", "VERIFICATION_HOST_OVERLAP", "VERIFICATION_CHECK_SET", "VERIFICATION_RUN_DURATION_MS", "VERIFICATION_HOST_LOAD"]);
  // A second hold by the same run is a second line.
  await holder.release(); await lease.release();
  const again = await asks(path, gauge, { item: "holder" }); gauge.reading = 93;
  const twice = asks(path, gauge, { item: "twice" }); await heldAs(path, "twice", "memory");
  gauge.reading = 30; const next = await twice; assert.equal(journaled(path, "memory-hold").length, 2);
  await again.release(); await next.release();
});
// wi_b5c33753fe4ae9fb / order 27946 / ASSIGN 27967: without the release marker
// the run command starts its run detached and only follows it, so killing the
// command leaves the run and its check behind when a test fails. A started
// command is therefore stopped whole: every matrix runner naming its output
// directory, their descendants and any group still on its lock file get
// SIGTERM, which lets a run stop its check, remove its home and release, then
// SIGKILL for whatever is left.
const RUN_COMMAND_STOP_MS = 20000, RUN_COMMAND_KILL_MS = 10000;
// Agent sessions carry command lines of tens of kilobytes each, so the listing
// can pass the default one-megabyte output limit on a busy host.
const processTable = () => execFileSync("ps", ["-Aww", "-o", "pid=,ppid=,pgid=,args="], { encoding: "utf8", maxBuffer: 64 * 1024 * 1024 }).split("\n")
  .map(line => /^\s*(\d+)\s+(\d+)\s+(\d+)\s+(.*)$/.exec(line)).filter(Boolean).map(([, pid, ppid, pgid, args]) => ({ pid: +pid, ppid: +ppid, pgid: +pgid, args }));
async function stopRunCommand({ output, lock, child, text, listing = processTable }) {
  if (!output) return { pids: [], forced: [], ms: 0 };
  const began = Date.now(), groups = existsSync(lock) ? allHeld(lock).flatMap(h => h.groups || []) : [];
  let table;
  try {
    table = listing();
  } catch (error) {
    // Without a listing, what the command itself made known is still stopped:
    // the command while it runs, the run it said it detached, and the groups on file.
    const known = [child?.exitCode === null && child.signalCode === null ? child.pid : 0, Number(/detached as pid (\d+);/.exec(text?.() || "")?.[1])].filter(pid => !pidGone(pid));
    const left = () => known.some(pid => !pidGone(pid)) || groups.some(group => !groupGone(group));
    for (const pid of known) kill(pid, "SIGTERM");
    for (const end = Date.now() + RUN_COMMAND_STOP_MS; left() && Date.now() < end; ) await new Promise(resolve => setTimeout(resolve, 20));
    for (const pid of known) kill(pid);
    for (const group of groups) kill(-group);
    throw new Error("the process listing failed, so only the run command's known processes were stopped: " + error.message, { cause: error });
  }
  const runners = table.filter(p => p.args.includes("verify-matrix.mjs") && p.args.includes(output)).map(p => p.pid);
  const found = new Set([...runners, ...groups]);
  for (let grew = true; grew; ) {
    grew = false;
    for (const p of table) if (!found.has(p.pid) && (found.has(p.ppid) || found.has(p.pgid))) { found.add(p.pid); grew = true; }
  }
  const pids = table.filter(p => found.has(p.pid)).map(p => p.pid), alive = () => pids.filter(pid => !pidGone(pid));
  const gone = async ms => { for (const end = Date.now() + ms; alive().length && Date.now() < end; ) await new Promise(resolve => setTimeout(resolve, 20)); return alive(); };
  for (const pid of runners) kill(pid, "SIGTERM");
  const forced = await gone(RUN_COMMAND_STOP_MS);
  for (const pid of forced) kill(pid);
  const left = await gone(RUN_COMMAND_KILL_MS);
  assert.deepEqual(left, [], "the run command's processes are stopped");
  return { pids, forced, ms: Date.now() - began };
}
// The real run command against a private lock file. Its stop is registered
// before its directories, so it runs first and the run can still clean up.
// The command carries no agent identity: a run stopped while it still waits
// would otherwise report its withdrawal to the live task.
function runCommand(t, source, marker) {
  const run = {};
  t.after(async () => {
    const stopped = await stopRunCommand(run);
    if (stopped.pids.length) console.log(JSON.stringify({ runCommandStopped: stopped }));
  });
  const fixture = runnerFixture(t, source), lock = lockFile(t), planFile = join(tempDir(t), "plan.json"), output = tempDir(t, "matrix-host-lock-logs-");
  writeFileSync(planFile, JSON.stringify(fixture.plan));
  Object.assign(run, { output, lock });
  const child = spawn(process.execPath, [fileURLToPath(new URL("../scripts/verify-matrix.mjs", import.meta.url)), "run", planFile, output, "--priority", "normal", "--min-free-bytes", "0"],
    { cwd: fixture.cwd, env: { ...process.env, TAILTERM_AGENT: undefined, TAILTERM_AGENT_NAME: undefined, TAILTERM_TASK: undefined, TAILTERM_MATRIX_HOST_LOCK: lock, TAILTERM_MATRIX_MAX_HOLDERS: "2", ...(marker === undefined ? {} : { TAILTERM_MATRIX_RELEASE: marker }) }, stdio: ["ignore", "pipe", "pipe"] });
  let said = ""; child.stdout.on("data", d => (said += d)); child.stderr.on("data", d => (said += d));
  return Object.assign(run, { child, text: () => said });
}
test("M8 a waiting release run sorts first and takes the single slot, fairness counts unchanged, and run mode passes the marker", async t => {
  const path = lockFile(t), gauge = meter(90), order = [];
  const joins = (item, extra) => asks(path, gauge, { item, ...extra }).then(lease => (order.push(item), lease));
  const holder = await asks(path, gauge, { item: "holder" });
  const normal = joins("normal"); await until(() => waitersOf(path).length === 1, "normal");
  assert(updateHostState(path, state => { state.waiters[0].nonUrgentOvertakes = 2; }).done);
  const urgent = joins("urgent", { priority: "urgent" }); await until(() => waitersOf(path).length === 2, "urgent");
  const team = joins("team", { priority: "high" }); await until(() => waitersOf(path).length === 3, "team");
  assert.deepEqual(items(waitersOf(path)), ["urgent", "normal", "team"], "a protected normal bars the later high run");
  const release = joins("release", { release: true }); await until(() => waitersOf(path).length === 4, "release");
  assert.deepEqual(items(waitersOf(path)), ["release", "urgent", "normal", "team"]);
  assert.equal(waitersOf(path)[0].release, true); assert(!Object.hasOwn(waitersOf(path)[1], "release"));
  assert.match(statusText(path), / 1 of 4: release\/tester\/pid \d+ \(run, normal\/default, release\)/);
  await heldAs(path, "release", "memory"); assert.deepEqual(order, []);
  const before = waitersOf(path).slice(1).map(w => [w.item, w.nonUrgentOvertakes, w.overtakenBy]);
  await holder.release();
  const deployer = await release;
  assert.deepEqual(order, ["release"]); assert.deepEqual(allHeld(path).map(h => [h.item, h.release]), [["release", true]]);
  assert.deepEqual(items(waitersOf(path)), ["urgent", "normal", "team"]);
  assert.deepEqual(waitersOf(path).map(w => [w.item, w.nonUrgentOvertakes, w.overtakenBy]), before.map(([item, fair, total]) => [item, fair, total + 1]),
    "the release grant is counted, but spends nobody's fairness budget");
  await heldAs(path, "urgent", "memory"); assert.deepEqual(order, ["release"], "the release run takes the one slot, the urgent run waits");
  gauge.reading = 5; await deployer.release();
  for (const waiting of [urgent, normal, team]) await (await waiting).release();
  assert.deepEqual(order, ["release", "urgent", "normal", "team"]);
  // Two release runs keep arrival order.
  gauge.reading = 90;
  const again = await asks(path, gauge, { item: "holder" });
  const a = joins("release-a", { release: true }); await until(() => waitersOf(path).length === 1, "release-a");
  const b = joins("release-b", { release: true, priority: "urgent" }); await until(() => waitersOf(path).length === 2, "release-b");
  assert.deepEqual(items(waitersOf(path)), ["release-a", "release-b"]);
  await again.release(); await (await a).release(); await (await b).release();

  for (const value of [undefined, ""]) assert.equal(releaseRun({ TAILTERM_MATRIX_RELEASE: value }), false);
  assert.equal(releaseRun({}), false); assert.equal(releaseRun({ TAILTERM_MATRIX_RELEASE: "1" }), true);
  for (const value of ["0", "true", "yes", " 1", "1 ", "01", "2"]) assert.throws(() => releaseRun({ TAILTERM_MATRIX_RELEASE: value }), /TAILTERM_MATRIX_RELEASE must be 1 or unset/);

  // runPlan hands the marker to the lock, and the run command sets it from the environment.
  const done = join(tempDir(t), "done");
  const source = `import fs from 'node:fs';const t=setInterval(()=>{if(fs.existsSync(${JSON.stringify(done)})){clearInterval(t);}},20);`;
  const direct = runnerFixture(t, source), runPath = lockFile(t);
  const running = runPlan(direct.plan, direct.cwd, tempDir(t, "matrix-host-lock-logs-"), { minFreeBytes: 0, hostLock: { path: runPath, pollMs: 20, release: true } });
  await until(() => allHeld(runPath).length === 1, "the marked run to hold");
  assert.deepEqual([allHeld(runPath)[0].kind, allHeld(runPath)[0].release], ["run", true]);
  writeFileSync(done, ""); await running; rmSync(done);
  const viaCommand = (marker) => runCommand(t, source, marker);
  const marked = await viaCommand("1");
  await until(() => allHeld(marked.lock).length === 1 || marked.child.exitCode !== null, "the run command to hold");
  assert.equal(allHeld(marked.lock)[0]?.release, true, marked.text());
  const plain = await viaCommand(undefined);
  await until(() => allHeld(plain.lock).length === 1 || plain.child.exitCode !== null, "the unmarked run command to hold");
  assert.equal(allHeld(plain.lock).length, 1, plain.text()); assert(!Object.hasOwn(allHeld(plain.lock)[0], "release"));
  writeFileSync(done, ""); await exited(marked.child); await exited(plain.child);
  assert.deepEqual([marked.child.exitCode, plain.child.exitCode], [0, 0], marked.text() + plain.text());
  const refused = await viaCommand("yes"); await exited(refused.child);
  assert.notEqual(refused.child.exitCode, 0); assert.match(refused.text(), /TAILTERM_MATRIX_RELEASE must be 1 or unset/);
  assert(!existsSync(refused.lock), "refused before joining the list");
});
test("M8 stopping a started run command, as a failed test's teardown does, ends the command, its run and the check and leaves no holder or home", async t => {
  for (const [marker, listing] of [[undefined], ["1"], [undefined, () => { throw new Error("no listing"); }]]) {
    const mark = join(tempDir(t), "check.json");
    const run = runCommand(t, `import fs from 'node:fs';fs.writeFileSync(${JSON.stringify(mark + ".part")},JSON.stringify({pid:process.pid,home:process.env.HOME}));fs.renameSync(${JSON.stringify(mark + ".part")},${JSON.stringify(mark)});setInterval(()=>{},1000);`, marker);
    await until(() => existsSync(mark) || run.child.exitCode !== null, "the run command's check to start");
    assert(existsSync(mark), run.text());
    const check = JSON.parse(readFileSync(mark, "utf8"));
    const [holder] = await until(() => { const held = allHeld(run.lock); return held[0]?.groups?.length ? held : null; }, "the check's group to be on file");
    // Unmarked, the command only follows the run it detached; marked, it is the run.
    assert.equal(holder.pid === run.child.pid, marker === "1", run.text());
    if (marker === undefined) assert.match(run.text(), new RegExp(`detached as pid ${holder.pid};`));
    assert(existsSync(check.home), "the run's private home exists while it runs");
    // A failed listing is reported, and still stops what the command made known.
    if (listing) await assert.rejects(stopRunCommand({ ...run, listing }), /the process listing failed.*no listing/);
    const stopped = await stopRunCommand(run);
    for (const pid of [run.child.pid, holder.pid, check.pid]) { assert(listing || stopped.pids.includes(pid), `pid ${pid} was found`); assert(pidGone(pid), `pid ${pid} is gone`); }
    for (const group of holder.groups) assert(groupGone(group), `group ${group} is gone`);
    assert(stopped.ms < 60000, `stopped in ${stopped.ms} ms`);
    assert.deepEqual([allHeld(run.lock), waitersOf(run.lock)], [[], []], "no holder or waiter is left");
    assert(!existsSync(check.home), "the run removed its private home");
    console.log(JSON.stringify({ runCommandStop: { marker: marker ?? null, listing: listing ? "failed" : "read", command: run.child.pid, run: holder.pid, check: check.pid, groups: holder.groups, home: check.home, ...stopped } }));
  }
});
test("M9 the limit defaults to 70, accepts 1 to 99 and off, and a bad value refuses before any file is written", async t => {
  assert.equal(DEFAULT_MEMORY_PRESSURE_MAX, 70);
  assert.equal(memoryPressureMax({}), 70); assert.equal(memoryPressureMax({ TAILTERM_MATRIX_MEMORY_PRESSURE_MAX: "" }), 70);
  for (const [raw, value] of [["1", 1], ["9", 9], ["70", 70], ["99", 99], ["off", null]])
    assert.equal(memoryPressureMax({ TAILTERM_MATRIX_MEMORY_PRESSURE_MAX: raw }), value);
  for (const raw of ["0", "100", "7.5", "abc", "-1", " 70", "70 ", "070", "OFF", "on"]) {
    assert.throws(() => memoryPressureMax({ TAILTERM_MATRIX_MEMORY_PRESSURE_MAX: raw }), /whole number from 1 to 99, or "off"/);
    const path = lockFile(t), record = join(tempDir(t), "record"), gauge = meter(10);
    await assert.rejects(asks(path, gauge, { environment: { ...gated.environment, TAILTERM_MATRIX_MEMORY_PRESSURE_MAX: raw }, recordDirectory: record }), /whole number from 1 to 99, or "off"/);
    assert(!existsSync(path)); assert(!existsSync(record)); assert.equal(gauge.calls, 0);
  }
  const path = lockFile(t), gauge = meter(100), off = { environment: { ...gated.environment, TAILTERM_MATRIX_MEMORY_PRESSURE_MAX: "off" } };
  const record = join(tempDir(t), "record");
  const first = await asks(path, gauge, off), second = await asks(path, gauge, { ...off, item: "second", recordDirectory: record });
  assert.equal(allHeld(path).length, 2); assert.equal(gauge.calls, 0);
  assert.equal(JSON.parse(readFileSync(join(record, "host-lock.json"), "utf8")).memoryPressureMax, "off");
  await first.release(); await second.release();
});
test("M10 readMemoryPressure with an injected run gives 44 for 56 and is unreadable on failure", () => {
  const calls = [];
  assert.equal(readMemoryPressure({ platform: "darwin", run: (...args) => (calls.push(args), { status: 0, stdout: "56\n" }) }), 44);
  assert.deepEqual(calls[0].slice(0, 2), ["sysctl", ["-n", "kern.memorystatus_level"]]);
  assert.equal(calls[0][2].timeout, MEMORY_PROBE_MS); assert.equal(MEMORY_PROBE_MS, 1000);
  assert.match(calls[0][2].env.PATH, /:\/usr\/sbin$/);
  assert.equal(readMemoryPressure({ platform: "darwin", run: () => ({ status: 0, stdout: "0" }) }), 100);
  assert.equal(readMemoryPressure({ platform: "darwin", run: () => ({ status: 0, stdout: "100\n" }) }), 0);
  const timedOut = Object.assign(new Error("spawnSync sysctl ETIMEDOUT"), { code: "ETIMEDOUT" });
  const missing = Object.assign(new Error("spawnSync sysctl ENOENT"), { code: "ENOENT" });
  for (const [result, why] of [
    [{ status: 1, stdout: "" }, /sysctl failed: exit 1/], [{ status: null, signal: "SIGKILL", stdout: "" }, /sysctl failed: signal SIGKILL/],
    [{ error: timedOut, status: null }, /timed out after 1000 ms/], [{ error: missing, status: null }, /could not run/],
    [{ status: 0, stdout: "abc\n" }, /no percentage/], [{ status: 0, stdout: "56.5\n" }, /no percentage/], [{ status: 0, stdout: "" }, /no percentage/],
    [{ status: 0, stdout: "101\n" }, /no percentage/], [{ status: 0, stdout: "-1\n" }, /no percentage/], [{ status: 0 }, /no percentage/], [undefined, /sysctl failed/]])
    assert.throws(() => readMemoryPressure({ platform: "darwin", run: () => result }), why);
  for (const platform of ["linux", "win32", "freebsd"])
    assert.throws(() => readMemoryPressure({ platform, run: () => assert.fail("no probe off darwin") }), /no memory-pressure reading on/);
});
test("M11 files without the new fields are accepted, bad field values refuse unchanged, and a v1 cohort takes no reading", async t => {
  const stamp = new Date().toISOString();
  const old = extra => ({ id: "old-holder", seq: 1, pid: process.pid, kind: "run", item: "old", agent: "tester", priority: "normal", prioritySource: "default",
    requestedAt: stamp, startedAt: stamp, runTimeoutMs: 60000, groups: [], ...extra });
  const file = holder => JSON.stringify({ version: 2, host: "h", updatedAt: stamp, requestSeq: 1, grantSeq: 1, holderLimit: 2, holders: [holder], waiters: [] });
  const path = lockFile(t), gauge = meter(20);
  writeFileSync(path, file(old()));
  const lease = await asks(path, gauge, { item: "new" });
  assert.deepEqual(items(allHeld(path)), ["old", "new"]); assert.equal(gauge.calls, 1);
  await lease.release();
  for (const bad of [{ heldReason: "other" }, { heldReason: null }, { release: "yes" }, { release: 1 }, { release: null }]) {
    const raw = file(old(bad)); writeFileSync(path, raw);
    await assert.rejects(asks(path, gauge), error => error.code === "corrupt-file" && error.exitCode === EXIT_LOCK_UNUSABLE && /invalid entry/.test(error.message));
    assert.equal(readFileSync(path, "utf8"), raw);
    assert.throws(() => rawReadHostState(path), error => error.exitCode === 78);
  }
  for (const good of [{ heldReason: "slots" }, { heldReason: "memory" }, { release: true }, { release: false }]) {
    writeFileSync(path, file(old(good))); assert.equal(holdersOf(rawReadHostState(path)).length, 1);
  }
  rmSync(path);
  const owner = await acquireHostLock(request(path)), state = rawReadHostState(path), high = meter(100);
  writeFileSync(path, JSON.stringify({ ...state, version: 1, holder: allHeld(path)[0], holders: undefined, holderLimit: undefined }));
  const queued = asks(path, high, { item: "queued" }); await heldAs(path, "queued", "slots");
  assert.equal(rawReadHostState(path).version, 1);
  await owner.release(); await (await queued).release(); assert.equal(high.calls, 0);
});
test("M13 under a memory hold a single holder past its run timeout plus grace is taken over as a recorded overlap", async t => {
  const path = lockFile(t), gauge = meter(90), lines = [];
  const overdue = await asks(path, gauge, { item: "overdue", runTimeoutMs: 200 });
  const lease = await asks(path, gauge, { item: "next", graceMs: 200, print: line => lines.push(line) });
  assert(Date.parse(lease.record.acquiredAt) >= Date.parse(overdue.record.acquiredAt) + 400 - 5, "not before the run timeout plus grace");
  assert.equal(lease.record.overlap, 1); assert.equal(lease.record.overlapDetails.holder, overdue.id);
  assert.deepEqual(items(allHeld(path)), ["next"]);
  assert.equal(journaled(path, "overlap", lease.id).length, 1); assert.equal(journaled(path, "memory-hold", lease.id).length, 1);
  assert(lease.record.memoryHeldMs > 0);
  assert(lines.some(line => /WARNING overlap, lock taken after run timeout from overdue\/tester/.test(line)), lines.join(" | "));
  await lease.release(); await overdue.release();
  // Two live holders, one overdue: nothing is taken over while the reading is over.
  const full = lockFile(t), low = meter(10);
  const late = await asks(full, low, { item: "late", runTimeoutMs: 100 }), peer = await asks(full, low, { item: "peer" });
  low.reading = 90;
  const third = asks(full, low, { item: "third", graceMs: 100 }); await heldAs(full, "third", "memory");
  const seen = low.calls; await until(() => low.calls >= seen + 3 && Date.now() > Date.parse(late.record.acquiredAt) + 300, "readings past the grace");
  assert.deepEqual(items(allHeld(full)), ["late", "peer"]); assert(!events(full).includes("overlap"));
  low.reading = 10; const taken = await third;
  assert.equal(taken.record.overlap, 1); assert.deepEqual(items(allHeld(full)), ["peer", "third"]);
  await taken.release(); await peer.release(); await late.release();
});
// Owner amendment #26938 (m8), ASSIGN #26947. The waiter's injected clock makes
// a living child holder overdue without waiting out its run timeout.
const skewed = () => { const clock = { aheadMs: 0 }; clock.now = () => Date.now() + clock.aheadMs; return clock; };
const DAY_MS = 24 * 3600000;
test("M14 under a one-slot hold two overdue holders are both taken over and the release waiter is admitted", async t => {
  const path = lockFile(t), gauge = meter(90), clock = skewed(), lines = [], record = join(tempDir(t), "record");
  const a = await heldByChild(t, path, { group: true, request: { ...two, item: "hung-a" } }), b = await heldByChild(t, path, { group: true, request: { ...two, item: "hung-b" } });
  const urgent = asks(path, gauge, { item: "urgent", priority: "urgent" }); await heldAs(path, "urgent", "slots");
  const release = asks(path, gauge, { item: "release", release: true, now: clock.now, maxWaitMs: DAY_MS, recordDirectory: record, print: line => lines.push(line) });
  await heldAs(path, "release", "slots");
  assert.deepEqual(items(waitersOf(path)), ["release", "urgent"]); assert.deepEqual(allHeld(path).map(h => h.id), [a.id, b.id]);
  assert.equal(gauge.calls, 0, "a full host in time takes no reading"); assert(!events(path).includes("overlap"));
  clock.aheadMs = 60000 + RUN_TIMEOUT_GRACE_MS + 60000;
  const lease = await release;
  assert.deepEqual(allHeld(path).map(h => h.id), [lease.id], "the release run alone holds");
  const overlaps = journaled(path, "overlap");
  assert.deepEqual(overlaps.map(line => [line.id, line.holder, line.reason, line.item, line.pid, line.groups]),
    [[lease.id, a.id, "run-timeout", "hung-a", a.pid, [a.group]], [lease.id, b.id, "run-timeout", "hung-b", b.pid, [b.group]]]);
  const sidecar = JSON.parse(readFileSync(join(record, "host-lock.json"), "utf8"));
  assert.equal(sidecar.overlap, 1); assert.equal(sidecar.overlapDetails.holder, a.id);
  assert.deepEqual(sidecar.overlaps.map(o => o.holder), [a.id, b.id]); assert.deepEqual(sidecar.overlaps[0], sidecar.overlapDetails);
  assert.equal(receiptKeys(lease.finish({ checkSet: "unit" })).VERIFICATION_HOST_OVERLAP, "1");
  assert.equal(lines.filter(line => /WARNING overlap, lock taken after run timeout from hung-[ab]\/tester/.test(line)).length, 2, lines.join(" | "));
  assert.equal(journaled(path, "memory-hold", lease.id).length, 0, "it was never refused for memory");
  for (const hung of [a, b]) { assert(!pidGone(hung.pid), "no holder process was signalled"); assert(!groupGone(hung.group), "no check group was signalled"); }
  await heldAs(path, "urgent", "memory"); assert.deepEqual(items(waitersOf(path)), ["urgent"]);
  await lease.release(); await (await urgent).release(); await releaseChild(a); await releaseChild(b);
  assert.deepEqual(allHeld(path), []);
});
test("M15 under a one-slot hold an in-time holder is never taken over, and one overdue holder is when the reading falls", async t => {
  const path = lockFile(t), gauge = meter(90), clock = skewed(), record = join(tempDir(t), "record");
  const late = await heldByChild(t, path, { group: true, request: { ...two, item: "late" } });
  const timely = await heldByChild(t, path, { group: true, request: { ...two, item: "timely", runTimeoutMs: 3600000 } });
  const waiting = asks(path, gauge, { item: "head", now: clock.now, maxWaitMs: DAY_MS, recordDirectory: record }); await heldAs(path, "head", "slots");
  const before = JSON.stringify(allHeld(path)), peer = structuredClone(allHeld(path).find(h => h.id === timely.id));
  clock.aheadMs = 60000 + RUN_TIMEOUT_GRACE_MS + 60000;
  await heldAs(path, "head", "memory");
  const seen = gauge.calls; await until(() => gauge.calls >= seen + 3, "three more readings over the limit");
  assert.equal(JSON.stringify(allHeld(path)), before, "both holders are on file unchanged");
  assert.deepEqual(items(waitersOf(path)), ["head"]); assert.equal(waitersOf(path)[0].heldReason, "memory"); assert(!events(path).includes("overlap"));
  gauge.reading = DEFAULT_MEMORY_PRESSURE_MAX;
  const lease = await waiting, overlaps = journaled(path, "overlap");
  assert.deepEqual(overlaps.map(line => [line.id, line.holder]), [[lease.id, late.id]]);
  assert.deepEqual(allHeld(path).map(h => h.id), [timely.id, lease.id]); assert.deepEqual(allHeld(path)[0], peer, "the in-time holder's entry is unchanged");
  const sidecar = JSON.parse(readFileSync(join(record, "host-lock.json"), "utf8"));
  assert.equal(sidecar.overlap, 1); assert.deepEqual(sidecar.overlaps, [sidecar.overlapDetails]); assert.equal(sidecar.overlapDetails.holder, late.id);
  for (const held of [late, timely]) { assert(!pidGone(held.pid)); assert(!groupGone(held.group)); }
  await lease.release(); await releaseChild(late); await releaseChild(timely);
  // Both holders in time under a hold: nothing is taken over, whatever the wait.
  const calm = lockFile(t), high = meter(99);
  const x = await asks(calm, high, { ...two, item: "x" }), y = await asks(calm, high, { ...two, item: "y" });
  const third = asks(calm, high, { item: "third", graceMs: 0 }); await heldAs(calm, "third", "slots");
  assert.equal(high.calls, 0); assert(!events(calm).includes("overlap"));
  await x.release(); await heldAs(calm, "third", "memory"); await y.release();
  const last = await third; assert.equal(last.record.overlap, 0); assert(!Object.hasOwn(last.record, "overlaps")); await last.release();
});
test("M12 no test in this file, child processes included, probed the host's memory", () => {
  assert.equal(process.env.PATH.split(":")[0], dirname(probeMarker) + "/bin");
  assert(!existsSync(probeMarker), "a test ran sysctl");
  // The guard is live: the reader's own command and PATH find the stub, not
  // the host. Only the probe's time bound is lengthened, so a shell that
  // starts slowly under load is not mistaken for a failed guard.
  const patient = (command, args, options) => spawnSync(command, args, { ...options, timeout: 60000 });
  assert.equal(readMemoryPressure({ platform: "darwin", run: patient }), 50);
  assert(existsSync(probeMarker), "the stub records a probe");
});
