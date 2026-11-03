// wi_57c903a832b38b70 a3: the project's host schedule gates team matrix
// starts. While it is active and the item holds no grant, acquireHostLock
// (verify-matrix run and targeted, host-lock exec) prints the reason line and
// does not join the lock; a grant, a clear or the expiry lets it proceed; a
// release run never asks; without a project no tt call is made; a failed
// lookup prints the unknown line and starts under native admission.
//
// verification-fixtures.mjs gives every run here a private lock file and
// removes TAILTERM_TASK and TAILTERM_MATRIX_RELEASE from this process, so no
// test reads the live project's schedule. Each child that should look up the
// schedule gets TAILTERM_TASK=tsk_fixture and a stand-in tt first on PATH.
import test from "node:test";
import assert from "node:assert/strict";
import { once } from "node:events";
import { spawn } from "node:child_process";
import { existsSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import {
  plannedFixture,
  fixture,
  matrixCLI,
  tempDir,
  untilHost,
  readHostState,
  hostLockFile,
} from "./verification-fixtures.mjs";
import {
  acquireHostLock,
  holdersOf,
  hostScheduleHold,
  hostScheduleGate,
  EXEC_HOLDER_ENV,
  EXIT_WAIT_EXPIRED,
} from "../scripts/verify-matrix-host-lock.mjs";

process.env.TAILTERM_MATRIX_MEMORY_PRESSURE_MAX = "off";
assert.equal(process.env.TAILTERM_TASK, undefined, "the fixtures remove the project");

const lockScript = new URL("../scripts/verify-matrix-host-lock.mjs", import.meta.url).pathname;
const schedule = (fields = {}) => ({
  reason: "release turns: team matrices start only on a handler grant",
  setBy: "agt_handler",
  setAt: "2026-11-03T08:00:00Z",
  expiresAt: "2026-11-03T12:00:00Z",
  ...fields,
});
const active = (fields) => ({ state: "active", schedule: schedule(fields) });
const holdLine = (item) =>
  `matrix host: waiting for a host turn: host schedule "release turns: team matrices start only on a handler grant"` +
  ` (set by agt_handler at 2026-11-03T08:00:00Z, until 2026-11-03T12:00:00Z);` +
  ` a run starts only on a database handler grant for ${item} (tt team queue schedule grant --item ${item})`;
const lockIsEmpty = () => {
  const state = readHostState(hostLockFile());
  return !state || (holdersOf(state).length === 0 && state.waiters.length === 0);
};

// A stand-in tt that answers tt team queue schedule from a file (or fails)
// and refuses everything else, recording each call.
function standIn(t, view) {
  const dir = tempDir(t, "host-schedule-tt-");
  const calls = join(dir, "calls.jsonl"),
    file = join(dir, "schedule.json");
  writeFileSync(file, JSON.stringify(view));
  writeFileSync(
    join(dir, "tt"),
    `#!${process.execPath}
const fs = require("node:fs");
fs.appendFileSync(${JSON.stringify(calls)}, JSON.stringify(process.argv.slice(2)) + "\\n");
const [a, b, c] = process.argv.slice(2);
if (a === "team" && b === "queue" && c === "schedule" && !process.env.SCHEDULE_TT_FAIL) process.stdout.write(fs.readFileSync(${JSON.stringify(file)}, "utf8"));
else {
  console.error(a === "team" ? "hub unreachable" : "work item not found");
  process.exit(1);
}
`,
    { mode: 0o755 },
  );
  return {
    env: { PATH: dir + ":" + process.env.PATH, TAILTERM_TASK: "tsk_fixture" },
    set: (next) => writeFileSync(file, JSON.stringify(next)),
    scheduleCalls: () =>
      existsSync(calls)
        ? readFileSync(calls, "utf8").trim().split("\n").map((line) => JSON.parse(line)).filter((argv) => argv[0] === "team")
        : [],
  };
}

test("the hold decision: only an active schedule without the item's grant holds", () => {
  assert.equal(hostScheduleHold(active(), { item: "wi_a" }), holdLine("wi_a"));
  assert.equal(hostScheduleHold(active(), { item: "unknown" }), holdLine("ITEM"));
  assert.equal(hostScheduleHold(active(), {}), holdLine("ITEM"));
  assert.equal(hostScheduleHold(active({ grants: [{ itemId: "wi_a" }] }), { item: "wi_a" }), null);
  assert.equal(hostScheduleHold(active({ grants: [{ itemId: "wi_b" }] }), { item: "wi_a" }), holdLine("wi_a"));
  assert.equal(hostScheduleHold(active(), { item: "wi_a", release: true }), null);
  for (const state of ["off", "expired", "cleared"])
    assert.equal(hostScheduleHold({ state, schedule: state === "off" ? undefined : schedule() }, { item: "wi_a" }), null, state);
  assert.equal(hostScheduleHold(undefined, { item: "wi_a" }), null);
});

test("the gate exists only in a project session, never for a release or inside a hold", async () => {
  let lookups = 0;
  const lookup = () => (lookups++, active());
  assert.equal(hostScheduleGate("wi_a", {}, lookup), undefined);
  assert.equal(hostScheduleGate("wi_a", { TAILTERM_TASK: "tsk_x", TAILTERM_MATRIX_RELEASE: "1" }, lookup), undefined);
  assert.equal(hostScheduleGate("wi_a", { TAILTERM_TASK: "tsk_x", [EXEC_HOLDER_ENV]: "lease" }, lookup), undefined);
  assert.equal(lookups, 0, "no lookup without a gate");
  const gate = hostScheduleGate("wi_a", { TAILTERM_TASK: "tsk_x" }, lookup);
  assert.deepEqual(await gate(), { hold: holdLine("wi_a") });
  assert.equal(lookups, 1);
  const failing = hostScheduleGate("wi_a", { TAILTERM_TASK: "tsk_x" }, () => {
    throw new Error("the lookup failed: hub unreachable");
  });
  assert.deepEqual(await failing(), { unknown: "the lookup failed: hub unreachable" });
});

for (const [name, next] of [
  ["a grant", active({ grants: [{ itemId: "wi_gate" }] })],
  ["a clear", { state: "cleared", schedule: schedule({ clearedBy: "owner", clearedAt: "2026-11-03T09:00:00Z" }) }],
  ["the expiry", { state: "expired", schedule: schedule() }],
])
  test(`a held run stays out of the lock until ${name} lets it proceed`, async (t) => {
    let view = active(),
      calls = 0;
    const lines = [];
    const pending = acquireHostLock({
      item: "wi_gate",
      agent: "gate",
      runTimeoutMs: 60000,
      pollMs: 20,
      schedulePollMs: 20,
      print: (line) => lines.push(line),
      scheduleGate: async () => (calls++, { hold: hostScheduleHold(view, { item: "wi_gate" }) }),
    });
    await untilHost(() => calls >= 3, "three schedule reads");
    assert.deepEqual(lines, [holdLine("wi_gate")], "the reason line once");
    assert(lockIsEmpty(), "a held run joins no waitlist");
    view = next;
    const lease = await pending;
    t.after(() => lease.release());
    assert.equal(lines[1], "matrix host: host turn given; joining the host lock");
    assert.match(lines.at(-1), /^matrix host: acquired after \d+ ms$/);
    assert.equal(holdersOf(readHostState(hostLockFile()))[0].item, "wi_gate");
  });

test("a held run expires with the wait bound and leaves when interrupted, never joining", async () => {
  const held = async () => ({ hold: holdLine("wi_gate") });
  await assert.rejects(
    acquireHostLock({ item: "wi_gate", runTimeoutMs: 60000, maxWaitMs: 60, schedulePollMs: 10, scheduleGate: held }),
    (error) => error.code === "wait-expired" && error.exitCode === EXIT_WAIT_EXPIRED && /waiting for a host turn under the host schedule/.test(error.message),
  );
  const stop = new AbortController();
  const pending = acquireHostLock({ item: "wi_gate", runTimeoutMs: 60000, schedulePollMs: 10000, scheduleGate: held, signal: stop.signal });
  setTimeout(() => stop.abort("SIGINT"), 30);
  await assert.rejects(pending, (error) => error.code === "withdrawn" && /while waiting for a host turn/.test(error.message));
  assert(lockIsEmpty());
});

test("a release run never consults the gate, and a failed lookup starts under native admission", async () => {
  const lease = await acquireHostLock({
    item: "wi_release",
    runTimeoutMs: 60000,
    release: true,
    scheduleGate: async () => assert.fail("a release run asked for a host turn"),
  });
  await lease.release();
  const lines = [];
  const open = await acquireHostLock({
    item: "wi_unknown",
    runTimeoutMs: 60000,
    print: (line) => lines.push(line),
    scheduleGate: hostScheduleGate("wi_unknown", { TAILTERM_TASK: "tsk_x" }, () => {
      throw new Error("the lookup timed out after 15000 ms");
    }),
  });
  await open.release();
  assert.equal(lines[0], "matrix host: host schedule unknown (the lookup timed out after 15000 ms); starting under native admission");
  assert.match(lines.at(-1), /^matrix host: acquired after \d+ ms$/);
});

// host-lock exec, through the CLI with the stand-in tt.
function execCLI(t, environment, item = "wi_exec") {
  const dir = tempDir(t, "host-schedule-exec-"),
    marker = join(dir, "ran");
  const child = spawn(
    process.execPath,
    [lockScript, "exec", "--item", item, "--timeout-minutes", "1", "--record", join(dir, "record"), "--", process.execPath, "-e", `require("node:fs").writeFileSync(${JSON.stringify(marker)}, "ran")`],
    { env: { ...process.env, ...environment }, stdio: ["ignore", "pipe", "pipe"] },
  );
  child.text = "";
  child.stdout.on("data", (data) => (child.text += data));
  child.stderr.on("data", (data) => (child.text += data));
  t.after(() => {
    if (child.exitCode === null && child.signalCode === null) child.kill("SIGKILL");
  });
  child.ran = () => existsSync(marker);
  return child;
}

test("host-lock exec waits under an active schedule and starts on a grant", async (t) => {
  const tt = standIn(t, active());
  const held = execCLI(t, tt.env);
  await untilHost(() => held.text.includes(holdLine("wi_exec")), "the reason line; saw " + held.text);
  assert(lockIsEmpty(), "the held exec joined the lock");
  assert(!held.ran(), "the held exec ran its command");
  held.kill("SIGTERM");
  const [code] = await once(held, "close");
  assert.equal(code, 143, held.text);
  assert(!held.ran());
  assert.deepEqual(tt.scheduleCalls()[0], ["team", "queue", "schedule", "--json", "--task", "tsk_fixture"]);

  tt.set(active({ grants: [{ itemId: "wi_exec" }] }));
  const granted = execCLI(t, tt.env);
  const [grantedCode] = await once(granted, "close");
  assert.equal(grantedCode, 0, granted.text);
  assert(granted.ran());
  assert(!granted.text.includes("waiting for a host turn"), granted.text);
});

test("host-lock exec starts on a failed lookup, and makes no lookup without a project or for a release", async (t) => {
  const tt = standIn(t, active());
  const unknown = execCLI(t, { ...tt.env, SCHEDULE_TT_FAIL: "1" });
  const [code] = await once(unknown, "close");
  assert.equal(code, 0, unknown.text);
  assert(unknown.ran());
  assert.match(unknown.text, /matrix host: host schedule unknown \(the lookup failed: hub unreachable\); starting under native admission\n/);
  assert.equal(tt.scheduleCalls().length, 1);

  for (const environment of [{ PATH: tt.env.PATH }, { ...tt.env, TAILTERM_MATRIX_RELEASE: "1" }]) {
    const child = execCLI(t, environment);
    const [exit] = await once(child, "close");
    assert.equal(exit, 0, child.text);
    assert(child.ran());
  }
  assert.equal(tt.scheduleCalls().length, 1, "a run with no project or a release run looked the schedule up");
});

test("verify-matrix run waits under an active schedule and runs once it is cleared", async (t) => {
  const tt = standIn(t, active());
  const { f, planFile } = plannedFixture(t, "console.log('ran');");
  const heldOutput = tempDir(t, "host-schedule-run-");
  const held = matrixCLI(t, f, ["run", planFile, heldOutput, "--item", "wi_run"], tt.env);
  await untilHost(() => held.text.includes(holdLine("wi_run")), "the reason line; saw " + held.text);
  assert(lockIsEmpty(), "the held run joined the lock");
  held.kill("SIGINT");
  const [code] = await once(held, "close");
  assert.equal(code, 130, held.text);
  assert(!existsSync(join(heldOutput, "receipt.json")), "a held run wrote a receipt");

  tt.set({ state: "cleared", schedule: schedule({ clearedBy: "owner", clearedAt: "2026-11-03T09:00:00Z" }) });
  const output = tempDir(t, "host-schedule-run-");
  const cleared = matrixCLI(t, f, ["run", planFile, output, "--item", "wi_run"], tt.env);
  const [clearedCode] = await once(cleared, "close");
  assert.equal(clearedCode, 0, cleared.text);
  assert(existsSync(join(output, "receipt.json")));
  assert(!cleared.text.includes("waiting for a host turn"), cleared.text);
  assert.ok(tt.scheduleCalls().length >= 2);
});

test("verify-matrix targeted waits under an active schedule for its context's item", async (t) => {
  const tt = standIn(t, active());
  const f = fixture(t);
  const file = join(tempDir(t, "host-schedule-context-"), "context.json"),
    output = tempDir(t, "host-schedule-targeted-");
  writeFileSync(file, JSON.stringify({ baseCommit: f.base, commit: f.commit, itemId: "wi_ctx" }));
  const held = matrixCLI(t, f, ["targeted", file, output, "--min-free-bytes", "0"], tt.env);
  await untilHost(() => held.text.includes(holdLine("wi_ctx")), "the reason line; saw " + held.text);
  assert(lockIsEmpty(), "the held targeted run joined the lock");
  held.kill("SIGINT");
  const [code] = await once(held, "close");
  assert.equal(code, 130, held.text);
  assert(!existsSync(join(output, "targeted-receipt.json")));
});
