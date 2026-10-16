import test from "node:test";
import assert from "node:assert/strict";
import fs, {
  mkdirSync,
  writeFileSync,
  readFileSync,
  rmSync,
  existsSync,
  readdirSync,
  statSync,
  symlinkSync,
} from "node:fs";
import { syncBuiltinESMExports } from "node:module";
import { join } from "node:path";
import { spawn, spawnSync } from "node:child_process";
import { prepareTestBinary } from "./test-binaries.mjs";
import { createHash, randomUUID } from "node:crypto";
import { once } from "node:events";
import {
  selectChecks,
  runPlan,
  digest,
  assertInventory,
  receiptEligible,
  removeVerifierHome,
  executionLocks,
  defaultJobs,
  makeTargetedPlan,
} from "../scripts/verify-matrix.mjs";
// Named separately so the red tests load on a runner that lacks these exports.
import * as matrixRunner from "../scripts/verify-matrix.mjs";
import { readJournal, lockPaths, takeMutex, releaseMutex } from "../scripts/verify-matrix-host-lock.mjs";
import {
  readHostState,
  fakeTTCalls,
  fakeTTLookups,
  makePlan,
  matrix,
  fixture,
  tempDir,
  commandPlan,
  alive,
  browserFixture,
  commitChange,
  goFixture,
  hostLockFile,
  matrixScript,
  sleepingCheck,
  untilHost,
  matrixCLI,
  plannedFixture,
  holdHost,
  raceArgv,
  tipFixture,
  checkIds,
  withoutSelection,
  runnerRecord,
  pidAlive,
  launcherCLI,
  queuedRun,
  holdingRun,
  agentIdentity,
  fakeBinding,
  goRaceShardOf,
  goRaceTestNames,
  goRacePartition,
  assertGoRacePartition,
  goRaceShardSelectors,
  goRaceRunCounts,
  assertRanExactlyOnce,
  goRaceShardSpec,
  checkWeight,
  shardPrefix,
  shardedCheck,
  shardPlanFixture,
  fixtureStoreTests,
  goRaceFixture,
  bigMax,
  bigMin,
  fixtureShards,
  fixtureShardCommands,
  ownedGroup,
  stubGoRace,
  queuedRunFiles,
  within,
  stopQueuedRun,
  linuxSweep,
} from "./verification-fixtures.mjs";

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
  assert.deepEqual(matrix.goRaceShards, { "./cmd/tt": 5, "./internal/store": 4 });
  // One -shards flag per approved package, in package order.
  const prefix = [...shardPrefix().slice(0, 4), "-shards=./cmd/tt=5", "-shards=./internal/store=4"];
  const race = (m, owned) => selectChecks(m, owned, []).find((c) => c.id === "go-race").argv;
  const lists = {
    "hub/internal/store/migrate.go": ["./internal/store"],
    "hub/internal/api/releases.go": ["./internal/api"],
    "hub/go.mod": ["./..."],
  };
  for (const [owned, packages] of Object.entries(lists)) {
    const argv = race(matrix, [owned]);
    assert.deepEqual(argv.slice(0, 6), prefix, owned);
    assert.deepEqual(argv.slice(6), packages, owned);
  }
  assert.deepEqual(
    race(matrix, ["hub/internal/api/releases.go", "hub/internal/store/migrate.go"]),
    [...prefix, "./internal/api", "./internal/store"],
  );
  // The key changes go-race only.
  const { goRaceShards, ...legacy } = matrix;
  const others = (m) => selectChecks(m, ["hub/internal/store/migrate.go", "client/app.js"], []).filter((c) => c.id !== "go-race");
  assert.deepEqual(others(matrix), others(legacy));
  assert.deepEqual(race(legacy, ["hub/internal/store/migrate.go"]), ["go", "test", "-race", "-timeout=45m", "./internal/store"]);
  assert.deepEqual(race(legacy, ["hub/internal/api/releases.go"]), ["go", "test", "-race", "-timeout=45m", "./internal/api"]);
  assert.deepEqual(race(legacy, ["hub/go.mod"]), ["go", "test", "-race", "-timeout=45m", "./..."]);
  for (const shards of [
    null, [], "./internal/store=4", {}, { "./internal/store": 4, "./internal/api": 9 },
    { "./internal/store": 4, "./internal/api": 1 }, { "./internal/store": 4, "./...": 2 },
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

test("a go-race that will shard holds slots for its pool of race processes, capped at the job count", () => {
  assert.equal(matrixRunner.GO_RACE_PROCESS_BYTES, 1024 ** 3);
  assert.equal(matrixRunner.JOB_SLOT_BYTES, 3 * 1024 ** 3);
  assert.equal(defaultJobs(10, 16 * 1024 ** 3), 5);
  assert.deepEqual(goRaceShardSpec(shardedCheck(4, "./internal/store")), [{ package: "./internal/store", k: 4 }]);
  assert.deepEqual(goRaceShardSpec(shardedCheck(4, "./...")), [{ package: "./internal/store", k: 4 }]);
  assert.equal(goRaceShardSpec(shardedCheck(4, "./internal/api")), null, "this list will not shard");
  assert.equal(goRaceShardSpec({ ...shardedCheck(4, "./internal/store"), id: "go-test" }), null);
  assert.equal(checkWeight(shardedCheck(4, "./internal/store"), 5), 2);
  assert.equal(checkWeight(shardedCheck(4, "./..."), 5), 2);
  assert.equal(checkWeight(shardedCheck(6, "./internal/store"), 5), 2);
  // The pool is the other packages' job plus K shards, at most the job count
  // at once: at 5 jobs it holds 5 processes whatever K, at 8 jobs K=7 is 8.
  assert.equal(checkWeight(shardedCheck(7, "./internal/store"), 5), 2);
  assert.equal(checkWeight(shardedCheck(8, "./internal/api", "./internal/store"), 5), 2);
  assert.equal(checkWeight(shardedCheck(7, "./internal/store"), 8), 3);
  assert.equal(checkWeight(shardedCheck(4, "./internal/store"), 8), 2);
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
  assert.deepEqual(goRaceShards, [{ package: "./internal/store", k: 2, tests: 5, digest: partitionDigest, line }]);
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

test("go-race command lists the shard package first, then runs the other packages beside its shards, every test exactly once", { timeout: 600000 }, (t) => {
  const f = goRaceFixture(t);
  assert.deepEqual(fixtureShards, [
    ["ExampleHello", "TestEpsilon", "TestGamma"],
    ["FuzzSeed", "TestAlpha", "TestBeta", "TestDelta"],
  ]);
  const digestLine = `partition sha256:${goRacePartition(fixtureStoreTests, 2).digest}`;
  // serial: with one job, the other packages end before any shard test starts.
  const otherThenShards = (result, others, serial) => {
    const store = result.entries.filter((e) => e.pkg === "store"),
      rest = result.entries.filter((e) => e.pkg !== "store");
    assert.deepEqual(rest.map((e) => e.pkg).sort(), others, "each other package ran once");
    assert.deepEqual(store.map((e) => e.name).sort(), [...fixtureStoreTests].sort(), "each store test ran once");
    if (serial)
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

  // Every package: ./... is expanded and the shard package listed before any
  // test runs; then the other packages' job (one race process at a time) and
  // the shards run in one pool.
  let result = f.run(["-timeout=5m", "-shards=./store=2", "./..."]);
  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.deepEqual(result.commands.slice(0, 2), ["list -f {{.Dir}} ./...", "test -race -list . ./store"]);
  assert.deepEqual(
    result.commands.slice(2).sort(),
    ["test -race -p=1 -timeout=5m ./internal/fixturelog ./other ./third", ...fixtureShardCommands].sort(),
  );
  otherThenShards(result, ["other", "third"]);
  const out = result.stdout;
  const at = (text) => {
    assert(out.includes(text), text + "\n" + out);
    return out.indexOf(text);
  };
  assert.match(out, /^go-race other packages: 3 packages, [0-9.]+ s, exit 0$/m);
  assert(at(`go-race shards: ./store K=2 tests=7 ${digestLine}\n`) < at("go-race other packages:"));
  assert(at("go-race other packages:") < at("go-race shard ./store 1/2:"));
  assert(at("go-race shard ./store 1/2:") < at("go-race shard ./store 2/2:"));
  assert.match(out, /^go-race shard \.\/store 1\/2: 3 tests, [0-9.]+ s, exit 0$/m);
  assert.match(out, /^go-race shard \.\/store 2\/2: 4 tests, [0-9.]+ s, exit 0$/m);
  assert(
    out.endsWith(`go-race shards: ./store K=2 tests=7 ran-once=7 ${digestLine} peak-shard-processes=2\n`),
    out,
  );
  // The log keeps every RUN line: each top-level name once, in its shard's
  // part of the output, and the subtests.
  for (const name of fixtureStoreTests) {
    assert.equal(out.split(`=== RUN   ${name}\n`).length, 2, name);
    const shard = fixtureShards[0].includes(name) ? 1 : 2;
    assert.equal(at(`=== RUN   ${name}\n`) < at(`go-race shard ./store ${shard}/2:`), true, name);
    if (shard === 2) assert(at(`=== RUN   ${name}\n`) > at("go-race shard ./store 1/2:"), name);
  }
  assert(out.includes("=== RUN   TestBeta/one\n") && out.includes("=== RUN   FuzzSeed/seed#0\n"), out);
  assert(out.includes("ok  \tfixture/other") && out.includes("ok  \tfixture/third"));

  // A mixed list on a one-job host: the list, the other package, then the
  // shards in turn.
  result = f.run(["-timeout=5m", "-shards=./store=2", "./store", "./other"], { VERIFICATION_JOBS: "1" });
  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.deepEqual(result.commands, [
    "test -race -list . ./store",
    "test -race -p=1 -timeout=5m ./other",
    ...fixtureShardCommands,
  ]);
  const [first, second] = otherThenShards(result, ["other"], true);
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

  // Anything but the flags and packages is a usage error; nothing runs. A
  // second flag for one package is refused whatever its K.
  for (const args of [
    [], ["-timeout=5m"], ["-count=2", "./store"], ["-run=TestAlpha", "./store"], ["-shards=./store=9", "./store"],
    ["-shards=./store=1", "./store"], ["-shards=./...=2", "./..."], ["-shards=./store=2", "-shards=./store=3", "./store"],
    ["-shards=./store=2", "-shards=./store=2", "./store"],
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
  // A failing other package on a one-job host: it is the first job, so no
  // shard starts after it.
  let result = f.run(["-timeout=5m", "-shards=./store=2", "./..."], { FIXTURE_FAIL: "other", VERIFICATION_JOBS: "1" });
  assert.equal(result.status, 1, result.stdout + result.stderr);
  assert.match(result.stdout, /^go-race other packages: 3 packages, [0-9.]+ s, exit 1$/m);
  assert(!result.stdout.includes("go-race shard ./store"), result.stdout);
  assert.match(result.stdout, /^go-race: 2 jobs not started after a failure$/m);
  assert.deepEqual(result.commands, [
    "list -f {{.Dir}} ./...",
    "test -race -list . ./store",
    "test -race -p=1 -timeout=5m ./internal/fixturelog ./other ./third",
  ]);
  assert.deepEqual(result.entries.filter((e) => e.pkg === "store"), []);
  // At the default job count the shards run beside it and finish; the exit is
  // the other packages' and nothing is reported missing.
  result = f.run(["-timeout=5m", "-shards=./store=2", "./..."], { FIXTURE_FAIL: "other" });
  assert.equal(result.status, 1, result.stdout + result.stderr);
  assert.match(result.stdout, /^go-race other packages: 3 packages, [0-9.]+ s, exit 1$/m);
  assert.equal(result.commands.length, 5, result.commands.join("\n"));
  assert.deepEqual(result.entries.filter((e) => e.pkg === "store").map((e) => e.name).sort(), [...fixtureStoreTests].sort());
  assert.doesNotMatch(result.stdout + result.stderr, /not run|did not run every test/);

  // A failing test fails its shard and the command; the count still holds.
  result = f.run(["-timeout=5m", "-shards=./store=2", "./store"], { FIXTURE_FAIL: "store" });
  assert.equal(result.status, 1, result.stdout + result.stderr);
  assert.match(result.stdout, /^--- FAIL: TestDelta /m);
  assert.match(result.stdout, /^go-race shard \.\/store 1\/2: 3 tests, [0-9.]+ s, exit 0$/m);
  assert.match(result.stdout, /^go-race shard \.\/store 2\/2: 4 tests, [0-9.]+ s, exit 1$/m);
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
test("bundle v9 a race shard that stops early is reported by its shard and exit, with a bounded list of the tests it did not run", async (t) => {
  const run = stubGoRace(t);
  const bounded = (error) => {
    assert(error, "the command did not reject");
    assert(error.message.length < 1000, `${error.message.length} characters`);
    const lists = error.message.split("\n").filter((line) => line.includes("not run (first"));
    assert.equal(lists.length, 1, error.message);
    assert.match(lists[0], /^shard \.\/pkg 1\/2 not run \(first 10 of \d+\): (TestCase\d{3}, ){9}TestCase\d{3}$/);
    assert.doesNotMatch(error.message, /did not run every test exactly once/);
  };
  let { error, text } = await run("exit2");
  bounded(error);
  assert.equal(error.exitCode, 2);
  const [first] = error.message.split("\n");
  const [, unrun, own] = /^go-race: shard \.\/pkg 1\/2 exited 2 before running (\d+) of its (\d+) tests$/.exec(first) ?? [];
  assert(own > 100 && unrun === own, error.message);
  // The shard's own summary line is still printed before the error.
  assert.match(text(), /^stub shard starting\ngo-race shard \.\/pkg 1\/2: 0 tests, [0-9.]+ s, exit 2$/m);

  // A killed shard has no exit code: the command exits 1 and never says -1.
  ({ error } = await run("kill"));
  bounded(error);
  assert.equal(error.exitCode, 1);
  assert.match(error.message.split("\n")[0], /^go-race: shard \.\/pkg 1\/2 ended without an exit code before running \d+ of its \d+ tests$/);
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
  // A helper marker exempts only a module some other tests module imports:
  // one nothing imports is a suite like any other, launching or not.
  const orphan = "// verify-matrix: browser-helper\nimport { chromium } from 'playwright';\nconst b = await chromium.launch();\n";
  assert.throws(inventory({ "new-browser.mjs": orphan }, []), changed);
  inventory({ "new-browser.mjs": orphan }, ["new-browser.mjs"])();
  inventory({ "new-browser.mjs": orphan }, [], ["new-browser.mjs"])();
  assert.throws(inventory({ "quiet.mjs": "// verify-matrix: browser-helper\n" }, []), changed);
  assert.throws(inventory({ "self.mjs": '// verify-matrix: browser-helper\nimport "./self.mjs";\n' }, []), changed);
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
  assert(!sweep.calls.some((call) => call.includes("etimes")), "no start-time ps without it");
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

// link(2) refused as a filesystem without hard links refuses it. The host-lock
// module imports linkSync by name, so the builtin's live binding is replaced
// too, only for the synchronous body.
function withoutHardLinks(code, body) {
  const original = fs.linkSync;
  let links = 0;
  fs.linkSync = () => {
    links++;
    throw Object.assign(new Error(`${code}: operation not supported, link`), { code });
  };
  syncBuiltinESMExports();
  try {
    return body(() => links);
  } finally {
    fs.linkSync = original;
    syncBuiltinESMExports();
  }
}

test("the launch mutex is taken without hard links by an exclusive create with the same owner and stale-holder rules", (t) => {
  for (const code of ["EPERM", "ENOTSUP"]) {
    const output = tempDir(t, "launch-mutex-no-links-");
    const paths = lockPaths(join(output, "runner.json"));
    const owner = () => JSON.parse(readFileSync(paths.mutex, "utf8"));
    withoutHardLinks(code, (links) => {
      // Taken: the owner record is in place, private, and no temp file stays.
      const busy = {};
      const token = takeMutex(paths, {}, busy);
      assert.equal(typeof token, "string", code);
      assert(links() > 0, code + ": the link was tried first");
      assert.deepEqual(Object.keys(owner()).sort(), ["at", "pid", "token"], code);
      assert.equal(owner().pid, process.pid, code);
      assert.equal(owner().token, token, code);
      assert.equal(statSync(paths.mutex).mode & 0o777, 0o600, code);
      assert.deepEqual(readdirSync(output), ["runner.json.lock"], code);
      // Busy while held by a live owner, then released by its token only.
      const second = {};
      assert.equal(takeMutex(paths, {}, second), null, code);
      assert.equal(second.owner.token, token, code);
      assert.equal(releaseMutex(paths, "someone-else"), false, code);
      assert.equal(releaseMutex(paths, token), true, code);
      assert.deepEqual(readdirSync(output), [], code);

      // A dead owner is reclaimed under the guard, which is taken the same way.
      const dead = spawnSync(process.execPath, ["-e", ""]).pid;
      assert(!pidAlive(dead));
      writeFileSync(paths.mutex, JSON.stringify({ pid: dead, token: "dead", at: new Date().toISOString() }) + "\n");
      const reclaimed = takeMutex(paths, {}, {});
      assert.equal(typeof reclaimed, "string", code);
      assert.equal(owner().token, reclaimed, code);
      assert.deepEqual(
        readJournal(paths.file).map((entry) => [entry.event, entry.deadPid, entry.token]),
        [["mutex-recovered", dead, "dead"]],
        code,
      );
      assert.deepEqual(readdirSync(output).sort(), ["host.journal.jsonl", "runner.json.lock"], code + ": guard removed");
      releaseMutex(paths, reclaimed);

      // A name created but not yet written (or left so by a crash) is an
      // owner nobody can identify: busy, and never reclaimed.
      writeFileSync(paths.mutex, "");
      const unknown = {};
      assert.equal(takeMutex(paths, {}, unknown), null, code);
      assert.deepEqual(unknown.owner, { pid: null, token: null }, code);
      assert.equal(readFileSync(paths.mutex, "utf8"), "", code);
    });
  }
  // Any other link error still fails as it is and leaves nothing behind.
  const output = tempDir(t, "launch-mutex-no-links-");
  const paths = lockPaths(join(output, "runner.json"));
  withoutHardLinks("EIO", () => assert.throws(() => takeMutex(paths), { code: "EIO" }));
  assert.deepEqual(readdirSync(output), []);
});

test("the home sweep stops the readable leftovers it found before failing closed on an unproven unreadable process", async (t) => {
  const processes = {
    700: { command: "young daemon", startedAfterHomeMs: 1000 },
    710: { command: "leftover", environ: (home) => `PATH=/bin\0HOME=${home}\0` },
    711: { command: "unrelated", environ: () => "HOME=/somewhere/else\0" },
  };
  const sweep = linuxSweep(t, processes);
  // Found by its executable path under the home rather than its environment.
  processes[712] = { command: `${sweep.home}/bin/daemon --serve`, environ: () => "HOME=/somewhere/else\0" };
  const message =
    "Cannot read the environment of 1 same-user process (pid 700) or prove it unrelated to the verifier home: " +
    "not started before the home was created, or start time unknown";
  // The sweep alone still throws its diagnostic, carrying what it did find.
  assert.throws(
    () => sweep.find(),
    (error) => {
      assert.equal(error.message, message);
      assert.deepEqual(error.found.map((hit) => [hit.pid, hit.reasons]), [[710, ["env"]], [712, ["argv"]]]);
      return true;
    },
  );
  // No live process is signalled: kill is replaced, and a stopped pid leaves
  // the canned process table.
  const stopped = new Set(),
    signalled = [],
    unexpected = [];
  t.mock.method(process, "kill", (pid, signal) => {
    if (!processes[pid]) {
      unexpected.push(pid);
      throw Object.assign(new Error("kill ESRCH"), { code: "ESRCH" });
    }
    if (signal === 0) {
      if (stopped.has(pid)) throw Object.assign(new Error("kill ESRCH"), { code: "ESRCH" });
      return true;
    }
    signalled.push([pid, signal]);
    stopped.add(pid);
    return true;
  });
  const probes = {
    ...sweep.probes,
    run: (command, args) => {
      const out = sweep.probes.run(command, args);
      if (!args.includes("-axww")) return out;
      return out
        .split("\n")
        .filter((line) => !stopped.has(Number(line.trim().split(/\s+/)[0])))
        .join("\n");
    },
  };
  await assert.rejects(() => matrixRunner.stopVerifierHomeProcesses(sweep.home, undefined, probes), { message });
  t.mock.restoreAll();
  assert.deepEqual(signalled, [[710, "SIGTERM"], [712, "SIGTERM"]]);
  assert.deepEqual(unexpected, []);
});
