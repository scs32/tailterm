// Preflight, matrix host lock and budget warning tests, split from
// tests/verification-2.test.js (wi_2b82df2dbac73bc0, order #36382) so node --test
// runs them beside the rest of that file.
import test from "node:test";
import assert from "node:assert/strict";
import { mkdirSync, mkdtempSync, writeFileSync, readFileSync, rmSync, existsSync, readdirSync, realpathSync, lstatSync } from "node:fs";
import { tmpdir, homedir } from "node:os";
import { join, relative, isAbsolute } from "node:path";
import { spawnSync, execFileSync } from "node:child_process";
import { crc32 as zlibCrc32 } from "node:zlib";
import { createHash } from "node:crypto";
import { once } from "node:events";
import {
  selectChecks,
  runPlan,
  digest,
  receiptEligible,
  removeVerifierHome,
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
  commitChange,
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
  browserFixture,
  within,
  fixturePids,
  leakingCheck,
  pidOf,
  alive,
} from "./verification-fixtures.mjs";

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
      const printed = [];
      const options = {
        getAvailableBytes: () => 2147483648,
        acquireHostLock: () => { acquisitions++; throw new Error("unexpected acquisition"); },
        removeHome: (home) => { homes++; removeVerifierHome(home); },
        // The fixture is a main worktree, so nothing is provisioned.
        installPrerequisites: () => assert.fail("non-worktree checkout provisioned"),
        hostLock: { print: (line) => printed.push(line) },
      };
      const refuse = async (overrides, expected) => {
        const output = tempDir(t, "verification-preflight-logs-");
        const directoriesBefore = readdirSync(homeRoot);
        await assert.rejects(preflightRun(mode, f, output, { ...options, ...overrides }), expected);
        assert.equal(acquisitions, 0, "no host acquisition call");
        assert.deepEqual(printed, [], "no provisioning output");
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

const sha256 = (bytes) => createHash("sha256").update(bytes).digest("hex");
const provisionLine = (line) => /^matrix run: (provisioned prerequisites from |prerequisite )/.test(line);
// A detached linked worktree of the fixture at its candidate, which has none of
// the ignored prerequisites.
function linkedWorktree(t, f) {
  const wt = join(tempDir(t, "verification-linked-"), "wt");
  f.git("worktree", "add", "-q", "--detach", wt, f.context.commit);
  return wt;
}
// Stands in for npm ci: writes the install marker in the worktree.
function fakeInstall(calls) {
  return (argv, dir) => {
    calls.push([argv, dir]);
    mkdirSync(join(dir, "node_modules"), { recursive: true });
    writeFileSync(join(dir, "node_modules/.package-lock.json"), "{}");
  };
}

test("a linked worktree provisions missing prerequisites from the main worktree before admission", async (t) => {
  const f = preflightFixture(t, true);
  const main = f.git("rev-parse", "--show-toplevel");
  const wt = linkedWorktree(t, f);
  for (const path of prerequisitePaths) assert(!existsSync(join(wt, path)), path + " starts missing");
  const calls = [], events = [];
  const options = {
    getAvailableBytes: () => 2147483648,
    installPrerequisites: fakeInstall(calls),
    hostLock: { print: (line) => events.push(line) },
    acquireHostLock: (args) => { events.push("acquire"); return acquireHostLock(args); },
  };
  const receipt = await runPlan(f.plan, wt, tempDir(t, "verification-linked-logs-"), options);
  assert(receipt.checks.every((c) => c.exitCode === 0));
  assert.deepEqual(calls, [[["npm", "ci"], wt]]);
  const acquired = events.indexOf("acquire");
  assert(acquired > 0, "admission happened after provisioning");
  const lines = events.filter((line) => line !== "acquire" && provisionLine(line));
  assert.deepEqual(events.slice(0, lines.length), lines, "provisioning output precedes admission");
  assert.equal(lines[0], "matrix run: provisioned prerequisites from " + main);
  const printed = lines.slice(1).map((line) => line.split(" ").slice(3));
  assert.deepEqual(printed.map(([path, action]) => [path, action]),
    prerequisitePaths.map((path, i) => [path, i === 0 ? "installed" : "copied"]));
  // The receipt keeps its {path, sha256} form; the copies carry main's bytes.
  assert.deepEqual(receipt.prerequisites.map((p) => Object.keys(p).sort()), prerequisitePaths.map(() => ["path", "sha256"]));
  assert.deepEqual(receipt.prerequisites.map((p) => p.path), prerequisitePaths);
  for (const [i, [path, , hash]] of printed.entries()) {
    assert.equal(hash, receipt.prerequisites[i].sha256, path);
    assert.equal(hash, sha256(readFileSync(join(wt, path))), path);
    if (i > 0) assert.equal(hash, sha256(readFileSync(join(main, path))), path + " matches main");
  }
  assert.equal(f.git("-C", wt, "status", "--porcelain"), "");

  events.length = 0;
  const again = await runPlan(f.plan, wt, tempDir(t, "verification-linked-logs-"), options);
  assert(again.checks.every((c) => c.exitCode === 0));
  assert.equal(calls.length, 1, "no second install");
  assert.deepEqual(events.filter(provisionLine), [], "nothing provisioned twice");
  assert.deepEqual(again.prerequisites, receipt.prerequisites);
});

test("linked worktree provisioning never overwrites an existing prerequisite", async (t) => {
  const f = preflightFixture(t, true);
  const wt = linkedWorktree(t, f);
  mkdirSync(join(wt, ".build"));
  writeFileSync(join(wt, ".build/test.wasm"), "local build");
  const calls = [], printed = [];
  const receipt = await runPlan(f.plan, wt, tempDir(t, "verification-linked-logs-"), {
    getAvailableBytes: () => 2147483648,
    installPrerequisites: fakeInstall(calls),
    hostLock: { print: (line) => printed.push(line) },
  });
  assert.equal(readFileSync(join(wt, ".build/test.wasm"), "utf8"), "local build");
  assert(printed.includes(`matrix run: prerequisite .build/test.wasm present ${digest("local build")}`));
  assert.equal(receipt.prerequisites.find((p) => p.path === ".build/test.wasm").sha256, digest("local build"));
  assert.equal(calls.length, 1);
});

test("linked worktree provisioning refuses before admission when the main worktree lacks a file or the worktree is dirty", async (t) => {
  for (const mode of ["run", "targeted"]) {
    const f = preflightFixture(t, true);
    const main = f.git("rev-parse", "--show-toplevel");
    rmSync(join(main, ".build/speech-fixture.wav"));
    const wt = linkedWorktree(t, f);
    let acquisitions = 0;
    const calls = [], printed = [], output = tempDir(t, "verification-linked-logs-");
    const options = {
      getAvailableBytes: () => 2147483648,
      installPrerequisites: fakeInstall(calls),
      hostLock: { print: (line) => printed.push(line) },
      acquireHostLock: () => { acquisitions++; throw new Error("unexpected acquisition"); },
    };
    await assert.rejects(preflightRun(mode, { ...f, cwd: wt }, output, options),
      (error) => error.message === "Missing matrix prerequisites: .build/speech-fixture.wav");
    assert.equal(acquisitions, 0);
    assert.deepEqual(printed, []);
    assert.deepEqual(readdirSync(output), [], "no receipt");
    assert(!existsSync(join(wt, ".build/speech-fixture.wav")));
  }

  // runPlan refuses a dirty checkout before this step; the step itself also
  // leaves a dirty linked worktree alone, so the gate names the missing file.
  const f = preflightFixture(t, true);
  const wt = linkedWorktree(t, f);
  writeFileSync(join(wt, "stray.txt"), "untracked\n");
  let acquisitions = 0;
  const calls = [], printed = [];
  const options = {
    getAvailableBytes: () => 2147483648,
    installPrerequisites: fakeInstall(calls),
    hostLock: { print: (line) => printed.push(line) },
    acquireHostLock: () => { acquisitions++; throw new Error("unexpected acquisition"); },
  };
  await assert.rejects(runPlan(f.plan, wt, tempDir(t, "verification-linked-logs-"), options), /Clean detached worktree required/);
  matrixRunner.provisionWorktreePrerequisites(f.plan.checks, wt, options);
  assert.throws(() => matrixRunner.readPrerequisites(f.plan.checks, wt), /Missing prerequisite: node_modules\/\.package-lock\.json/);
  assert.deepEqual(calls, []);
  assert.deepEqual(printed, []);
  assert.equal(acquisitions, 0);
  for (const path of prerequisitePaths) assert(!existsSync(join(wt, path)), path + " not provisioned");
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

// wi_88e34f3c5b654745 / order #40884: a real run that warns, and the removed
// receipt taking the list with it. The fixture repo carries its own matrix
// with a short npm-unit timeout; the check sleeps past 70 percent of it.
const BUDGET_TIMEOUT_MS = 6000,
  BUDGET_SLEEP_MS = 4300;
function budgetFixture(t, source = sleepingCheck(BUDGET_SLEEP_MS)) {
  const f = fixture(t);
  const m = JSON.parse(readFileSync(join(f.cwd, "verification/matrix.json"), "utf8"));
  writeFileSync(
    join(f.cwd, "verification/matrix.json"),
    JSON.stringify({ ...m, checkTimeoutMs: { "npm-unit": BUDGET_TIMEOUT_MS } }),
  );
  return { f, plan: commandPlan(f, source) };
}

test("budget warning: a real run whose check passes above 70 percent of its timeout prints the line and writes the list", async (t) => {
  const { f, plan } = budgetFixture(t),
    output = tempDir(t, "verification-budget-warn-"),
    lines = [];
  assert.equal(plan.checks[0].environment.VERIFICATION_TIMEOUT_MS, String(BUDGET_TIMEOUT_MS));
  const receipt = await runPlan(plan, f.cwd, output, {
    minFreeBytes: 0,
    hostLock: { print: (line) => lines.push(line) },
  });
  assert(receiptEligible(receipt));
  const durationMs = Math.max(
    ...(receipt.checks[0].attempts ?? [receipt.checks[0]])
      .filter((a) => a.exitCode === 0)
      .map((a) => a.durationMs),
  );
  assert(durationMs > BUDGET_SLEEP_MS && durationMs < BUDGET_TIMEOUT_MS, "duration " + durationMs);
  const percent = Math.round((durationMs / BUDGET_TIMEOUT_MS) * 10000) / 100;
  assert(percent > 70);
  assert.deepEqual(lines.filter((line) => /budget warning/.test(line)), [
    `matrix run: budget warning: npm-unit passed in ${durationMs} ms, ${percent}% of its ${BUDGET_TIMEOUT_MS} ms timeout`,
  ]);
  const expected = [{ check: "npm-unit", durationMs, timeoutMs: BUDGET_TIMEOUT_MS, percent }];
  assert.equal(
    readFileSync(join(output, "budget-warnings.json"), "utf8"),
    JSON.stringify(expected, null, 2) + "\n",
  );
  const raw = readFileSync(join(output, "receipt.json"), "utf8");
  assert.equal(raw, JSON.stringify(receipt, null, 2) + "\n");
  assert(!/budget/i.test(raw.replaceAll(output, "")), "nothing about budgets is in the receipt");
});

test("budget warning: a cleanup failure that removes the receipt of a warned run removes its list too", async (t) => {
  const { f, plan } = budgetFixture(t),
    output = tempDir(t, "verification-budget-cleanup-"),
    lines = [];
  let home;
  await assert.rejects(
    runPlan(plan, f.cwd, output, {
      minFreeBytes: 0,
      hostLock: { print: (line) => lines.push(line) },
      removeHome: (path) => {
        home = path;
        removeVerifierHome(path);
        throw new Error("fixture removal failure");
      },
    }),
    /Failed to remove verifier home .*fixture removal failure/,
  );
  assert(home && !existsSync(home));
  assert.equal(lines.filter((line) => /budget warning: npm-unit /.test(line)).length, 1, "the run warned");
  assert(existsSync(join(output, "cleanup-error.json")));
  assert(!existsSync(join(output, "receipt.json")), "no receipt survives the cleanup failure");
  assert(!existsSync(join(output, "budget-warnings.json")), "no list survives without its receipt");
});

for (const signal of ["SIGINT", "SIGTERM"])
  test(`budget warning: a CLI ${signal} after the receipt is written removes the receipt and its list`, async (t) => {
    // A TERM-ignoring child under the home holds cleanup in its SIGTERM grace
    // period, so the signal lands after the receipt and list are written.
    const pids = fixturePids(t);
    const { f, plan } = budgetFixture(
      t,
      leakingCheck(
        pids,
        ["held"],
        `start('held',{cwd:fs.realpathSync(home),env:{...elsewhere,IGNORE_TERM:'1'}});`,
      ),
    );
    const root = tempDir(t, "verification-budget-signal-"),
      planFile = join(root, "plan.json"),
      output = tempDir(t, "verification-budget-signal-logs-");
    writeFileSync(planFile, JSON.stringify(plan));
    const child = matrixCLI(t, f, ["run", planFile, output, "--min-free-bytes", "0"], { TMPDIR: root });
    await untilHost(() => existsSync(join(output, "budget-warnings.json")), "the budget warning list");
    assert(existsSync(join(output, "receipt.json")), "the receipt is written before the list");
    assert(alive(pidOf(pids, "held")), "cleanup is still waiting on the held child");
    child.kill(signal);
    const [code] = await once(child, "close");
    assert.equal(code, signal === "SIGINT" ? 130 : 143, child.text);
    assert(!alive(pidOf(pids, "held")), "cleanup still stopped the held child");
    assert(!existsSync(join(output, "receipt.json")), "no receipt survives the interruption");
    assert(!existsSync(join(output, "budget-warnings.json")), "no list survives without its receipt");
  });

// wi_37750145fc63b2f6 / order #41687: a plan without wasm-test-build never
// runs a WASM reader on WASM that was not built from the candidate's sources.
// The fixture's build stub writes all three files from the tracked
// wasm/src.go and appends "rebuild" to .build/order; the browser suite
// appends "reader", so the file shows which ran first. A wasm/ rule lets a
// candidate select wasm-test-build.
const WASM_REBUILD_STUB =
  'mkdir -p .build\nsrc=$(cat wasm/src.go)\n' +
  'printf "built %s" "$src" > wasm/tailserve.wasm\nprintf "test %s" "$src" > .build/test.wasm\n' +
  'printf "modules %s" "$src" > .build/go-modules.txt\necho rebuild >> .build/order\n' +
  'echo "$*" >> .build/rebuild-calls\nenv > .build/rebuild-env\n';
const wasmBuilt = [".build/test.wasm", "wasm/tailserve.wasm", ".build/go-modules.txt"];
function wasmSourceFixture(t, script = WASM_REBUILD_STUB) {
  const f = browserFixture(t, {
    "fixture-browser.mjs": 'import { appendFileSync } from "node:fs";\nappendFileSync(".build/order", "reader\\n");\n',
  });
  writeFileSync(join(f.cwd, ".gitignore"), "node_modules/\n.build/\nwasm/*.wasm\n");
  f.git("rm", "-q", "--cached", "wasm/tailserve.wasm");
  const m = JSON.parse(readFileSync(join(f.cwd, "verification/matrix.json"), "utf8"));
  m.rules.push({ prefixes: ["wasm/"], groups: ["wasm", "unit", "browser"] });
  writeFileSync(join(f.cwd, "verification/matrix.json"), JSON.stringify(m));
  mkdirSync(join(f.cwd, "scripts"));
  writeFileSync(join(f.cwd, "scripts/build-wasm.sh"), script);
  writeFileSync(join(f.cwd, "wasm/src.go"), "package wasm // one\n");
  f.git("add", ".");
  f.git("commit", "-qm", "wasm sources");
  const candidate = (path = "client/candidate.txt", owned = ["client/"]) => {
    const baseCommit = f.git("rev-parse", "HEAD");
    const commit = commitChange(f, path, "change " + Date.now() + "\n", "candidate");
    const context = { baseCommit, commit };
    return { context, plan: makePlan({ ...context, owned }, f.cwd) };
  };
  return { ...f, candidate, ...candidate() };
}
const wasmLine = (line) => line.startsWith("matrix run: WASM prerequisite") || line.startsWith("matrix run: rebuilt WASM");
const readLines = (path) => existsSync(path) ? readFileSync(path, "utf8").trim().split("\n").filter(Boolean) : [];
const wasmOptions = (events) => ({
  getAvailableBytes: () => 2147483648,
  installPrerequisites: fakeInstall([]),
  hostLock: { print: (line) => events.push(line) },
  acquireHostLock: (args) => { events.push("acquire"); return acquireHostLock(args); },
});
// The receipt and every reader name the rebuilt bytes in their {path, sha256}
// form; the printed lines name the cause and the same digests.
function assertRebuilt(receipt, events, cwd, cause) {
  assert(receipt.checks.every((c) => c.exitCode === 0), "every check passed");
  const stale = events.indexOf(`matrix run: WASM prerequisites stale (${cause}); rebuilding with bash scripts/build-wasm.sh --test`);
  assert(stale > events.indexOf("acquire") && events.includes("acquire"), "rebuilt after admission: " + events.join("\n"));
  const src = readFileSync(join(cwd, "wasm/src.go"), "utf8").trimEnd();
  const built = { "wasm/tailserve.wasm": "built " + src, ".build/test.wasm": "test " + src, ".build/go-modules.txt": "modules " + src };
  for (const [path, bytes] of Object.entries(built)) {
    assert.equal(readFileSync(join(cwd, path), "utf8"), bytes, path);
    assert(events.includes(`matrix run: rebuilt WASM prerequisite ${path} ${digest(bytes)}`), path + " printed");
    assert.deepEqual(receipt.prerequisites.find((p) => p.path === path), { path, sha256: digest(bytes) });
  }
  assert(events.some((line) => /^matrix run: WASM prerequisite rebuild took \d+ ms$/.test(line)));
  const wasm = ["wasm/tailserve.wasm", ".build/test.wasm"].map((path) => ({ path, sha256: digest(built[path]) }));
  const readers = receipt.checks.filter((c) => c.id === "00-static-build" || c.id.includes("browser"));
  assert.equal(readers.length, 2);
  for (const reader of readers) assert.deepEqual(reader.wasm, wasm, reader.id);
  const record = JSON.parse(readFileSync(join(cwd, ".build/wasm-sources.json"), "utf8"));
  assert.deepEqual(record.files.map((e) => e.path).sort(), Object.keys(built).sort());
  assert.equal(record.sources, matrixRunner.wasmSourceDigest(cwd, receipt.commit));
  assert.equal(spawnSync("git", ["status", "--porcelain", "--untracked-files=normal"], { cwd, encoding: "utf8" }).stdout, "");
}

test("WASM prerequisite: a provisioned build with no source record is rebuilt once after admission, before any reader", async (t) => {
  const sentinel = "TAILTERM_WASM_REBUILD_SENTINEL";
  process.env[sentinel] = "inherited";
  t.after(() => delete process.env[sentinel]);
  for (const mode of ["run", "targeted"]) {
    const f = wasmSourceFixture(t);
    const wt = linkedWorktree(t, f);
    const events = [], options = wasmOptions(events);
    const receipt = await preflightRun(mode, { ...f, cwd: wt }, tempDir(t, "verification-wasm-rebuild-"), options);
    assert(events.includes(`matrix run: prerequisite .build/test.wasm copied ${digest("fixture")}`), "main's stale build was copied");
    assertRebuilt(receipt, events, wt, "no source record");
    assert.deepEqual(readLines(join(wt, ".build/rebuild-calls")), ["--test"], mode + ": one rebuild");
    assert.deepEqual(readLines(join(wt, ".build/order")), ["rebuild", "reader"], mode);
    // The rebuild sees none of the runner's inherited TAILTERM_* variables.
    // This fixture has no hub/go.sum, so its Go caches are the verifier
    // home's: build-wasm.sh is pointed at that home, never the checkout.
    const env = readLines(join(wt, ".build/rebuild-env"));
    const run = receipt.environment;
    assert(env.includes("PATH=" + run.PATH), mode);
    assert.deepEqual(env.filter((line) => line.startsWith("TAILTERM_")),
      [`TAILTERM_VERIFICATION_CACHE=${join(run.HOME, "wasm-rebuild-cache")}`], mode);
    for (const name of ["HOME", "TMPDIR", "GOPATH", "GOCACHE", "GOMODCACHE", "GOFLAGS", "GOTOOLCHAIN"])
      assert(env.includes(`${name}=${run[name]}`), `${mode}: ${name}`);
    assert.equal(run.GOCACHE, join(run.HOME, "go-build"));
    for (const local of [".build/go", ".build/cache"]) assert(!existsSync(join(wt, local)), local);

    // The next run in the same worktree trusts the record and rebuilds nothing.
    events.length = 0;
    const again = await preflightRun(mode, { ...f, cwd: wt }, tempDir(t, "verification-wasm-rebuild-"), options);
    assert(again.checks.every((c) => c.exitCode === 0));
    assert.deepEqual(readLines(join(wt, ".build/rebuild-calls")), ["--test"], mode + ": no second rebuild");
    assert.deepEqual(events.filter(wasmLine),
      ["matrix run: WASM prerequisites current for sources " + matrixRunner.wasmSourceDigest(wt, f.context.commit).slice(0, 12)]);
    assert.deepEqual(again.prerequisites, receipt.prerequisites);
  }
});

test("WASM prerequisite: a source record from other sources or edited WASM bytes is rebuilt before readers", async (t) => {
  for (const change of ["wasm/src.go", "scripts/build-wasm.sh"]) {
    const f = wasmSourceFixture(t);
    matrixRunner.recordWasmSources(f.cwd, f.context.commit);
    // The candidate's base already holds the source change, so it is not in
    // the plan's diff and selects no wasm-test-build.
    const content = change === "wasm/src.go" ? "package wasm // two\n" : WASM_REBUILD_STUB + "# changed\n";
    commitChange(f, change, content, "source change");
    const { context, plan } = f.candidate();
    assert(!plan.checks.some((c) => c.id === "wasm-test-build"));
    const events = [];
    const receipt = await runPlan(plan, f.cwd, tempDir(t, "verification-wasm-rebuild-"), wasmOptions(events));
    assert.equal(receipt.commit, context.commit);
    assertRebuilt(receipt, events, f.cwd, "built from other sources");
    assert.deepEqual(readLines(join(f.cwd, ".build/order")), ["rebuild", "reader"], change);
  }
  for (const path of wasmBuilt) {
    const f = wasmSourceFixture(t);
    matrixRunner.recordWasmSources(f.cwd, f.context.commit);
    writeFileSync(join(f.cwd, path), "edited");
    const events = [];
    const receipt = await preflightRun("targeted", f, tempDir(t, "verification-wasm-rebuild-"), wasmOptions(events));
    assertRebuilt(receipt, events, f.cwd, "changed since its source record: " + path);
    assert.deepEqual(readLines(join(f.cwd, ".build/order")), ["rebuild", "reader"], path);
  }
});

test("WASM prerequisite: a failed rebuild or one that changes the checkout refuses before any check starts", async (t) => {
  const cases = [
    [WASM_REBUILD_STUB + "exit 3\n", /^Stale WASM prerequisites \(no source record\): rebuild failed \(exit 3\), see .*wasm-prerequisite-rebuild\.log$/],
    [WASM_REBUILD_STUB + "echo dirty >> wasm/src.go\n", /^Stale WASM prerequisites \(no source record\): rebuild changed the checkout$/],
  ];
  for (const mode of ["run", "targeted"])
    for (const [script, expected] of cases) {
      const f = wasmSourceFixture(t, script);
      const events = [], output = tempDir(t, "verification-wasm-refused-");
      await assert.rejects(preflightRun(mode, f, output, wasmOptions(events)), (error) => expected.test(error.message));
      assert.deepEqual(readLines(join(f.cwd, ".build/rebuild-calls")), ["--test"]);
      assert.deepEqual(readLines(join(f.cwd, ".build/order")), ["rebuild"], "no reader ran");
      assert.deepEqual(readdirSync(output).filter((name) => name.endsWith("receipt.json")), [], "no receipt");
      assert(!existsSync(join(f.cwd, ".build/wasm-sources.json")), "no source record");
      assert(!events.some((line) => line.startsWith("matrix run: rebuilt WASM")));
    }
});

test("WASM prerequisite: a build recorded from the candidate's sources is used as is", async (t) => {
  const f = wasmSourceFixture(t);
  const before = matrixRunner.recordWasmSources(f.cwd, f.context.commit);
  assert.equal(matrixRunner.staleWasmCause(f.cwd, f.context.commit), "");
  const prerequisites = matrixRunner.readPrerequisites(f.plan.checks, f.cwd);
  for (const mode of ["run", "targeted"]) {
    const events = [];
    const receipt = await preflightRun(mode, f, tempDir(t, "verification-wasm-current-"), wasmOptions(events));
    assert(receipt.checks.every((c) => c.exitCode === 0));
    assert.deepEqual(events.filter(wasmLine), ["matrix run: WASM prerequisites current for sources " + before.sources.slice(0, 12)]);
    assert(events.indexOf(events.find(wasmLine)) > events.indexOf("acquire"));
    assert.deepEqual(receipt.prerequisites, prerequisites);
    for (const path of wasmBuilt) assert.equal(readFileSync(join(f.cwd, path), "utf8"), "fixture", path);
  }
  assert(!existsSync(join(f.cwd, ".build/rebuild-calls")), "no rebuild");
  assert.deepEqual(JSON.parse(readFileSync(join(f.cwd, ".build/wasm-sources.json"), "utf8")), before);
});

test("WASM prerequisite: plans with no WASM reader, no build script or their own wasm-test-build read no record", async (t) => {
  // No WASM reader: a docs-only plan.
  const f = wasmSourceFixture(t);
  const docs = f.candidate("docs/candidate.md", ["docs/"]);
  assert(!docs.plan.checks.some((c) => c.id.includes("browser") || c.id === "00-static-build"));
  const events = [];
  const receipt = await runPlan(docs.plan, f.cwd, tempDir(t, "verification-wasm-none-"), wasmOptions(events));
  assert(receipt.checks.every((c) => c.exitCode === 0));
  assert.deepEqual(events.filter(wasmLine), []);
  assert(!existsSync(join(f.cwd, ".build/rebuild-calls")));
  assert(!existsSync(join(f.cwd, ".build/wasm-sources.json")));

  // No tracked scripts/build-wasm.sh: a browser plan uses the WASM as is.
  const g = preflightFixture(t, true);
  const browserEvents = [];
  const browser = await runPlan(g.plan, g.cwd, tempDir(t, "verification-wasm-none-"), wasmOptions(browserEvents));
  assert(browser.checks.every((c) => c.exitCode === 0));
  assert.deepEqual(browserEvents.filter(wasmLine), []);
  assert.equal(browser.prerequisites.find((p) => p.path === ".build/test.wasm").sha256, digest("fixture"));
  assert(!existsSync(join(g.cwd, ".build/wasm-sources.json")));

  // A plan with wasm-test-build runs the script only as that check.
  const h = wasmSourceFixture(t);
  const wasm = h.candidate("wasm/src.go", ["wasm/"]);
  assert.equal(wasm.plan.checks[0].id, "wasm-test-build");
  const wasmEvents = [];
  const built = await runPlan(wasm.plan, h.cwd, tempDir(t, "verification-wasm-none-"), wasmOptions(wasmEvents));
  assert(built.checks.every((c) => c.exitCode === 0));
  assert.deepEqual(wasmEvents.filter(wasmLine), []);
  assert.deepEqual(readLines(join(h.cwd, ".build/rebuild-calls")), ["--test"]);
  assert.deepEqual(readLines(join(h.cwd, ".build/order")), ["rebuild", "reader"]);
  assert(!existsSync(join(h.cwd, ".build/wasm-sources.json")));
});

// wi_52c459fde3a00e00 / order #42103: with shared caches the stale-WASM
// rebuild runs the real build-wasm.sh with the run's own Go caches, so each
// fresh worktree reuses one build cache, one module generation and one
// tarball instead of making its own. A tiny tailscale.com (whose wasm package
// needs example.com/jsdep only through the copied wasm/upstream_js.go) is
// served from a file:// proxy, and its release tarball by a curl stand-in;
// these helpers follow tests/verification-4.test.js, which this item does
// not own.
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
// Go makes module directories read-only.
function goCacheDirectory(t, prefix) {
  const directory = mkdtempSync(join(tmpdir(), prefix));
  t.after(() => existsSync(directory) && removeVerifierHome(directory));
  return directory;
}
// The Go environment the fixture's own setup uses: never the caller's caches.
function setupGoEnvironment(t, proxy) {
  const scratch = goCacheDirectory(t, "verification-go-setup-");
  const environment = Object.fromEntries(Object.entries(process.env).filter(([key]) => !/^(GO|TAILTERM_)/.test(key)));
  return { ...environment, GOMODCACHE: join(scratch, "mod"), GOCACHE: join(scratch, "build"), GOPATH: join(scratch, "path"),
    GOFLAGS: "-mod=mod", GOSUMDB: "off", GOTOOLCHAIN: "local", GOPROXY: proxy.url };
}
// A proxy serving example.com/jsdep and tailscale.com v1.0.0; returns the
// tailscale.com files.
function tailscaleProxy(t) {
  const directory = tempDir(t, "verification-goproxy-"), proxy = { directory, url: "file://" + directory };
  const publish = (path, files) => {
    const dir = join(directory, path, "@v");
    mkdirSync(dir, { recursive: true });
    writeFileSync(join(dir, "list"), "v1.0.0\n");
    writeFileSync(join(dir, "v1.0.0.info"), '{"Version":"v1.0.0","Time":"2026-01-01T00:00:00Z"}\n');
    writeFileSync(join(dir, "v1.0.0.mod"), files["go.mod"]);
    writeFileSync(join(dir, "v1.0.0.zip"), storedZip(Object.fromEntries(
      Object.entries(files).map(([name, text]) => [`${path}@v1.0.0/${name}`, text]))));
  };
  publish("example.com/jsdep", { "go.mod": "module example.com/jsdep\n\ngo 1.21\n",
    "jsdep.go": "package jsdep\n\nfunc Value() int { return 2 }\n" });
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
  execFileSync("go", ["mod", "tidy"], { cwd: source, env: setupGoEnvironment(t, proxy), stdio: "pipe" });
  for (const name of ["go.mod", "go.sum"]) files[name] = readFileSync(join(source, name), "utf8");
  publish("tailscale.com", files);
  return { proxy, files };
}
// The release tarball (top directory tailscale-1.0.0/) and its sha256.
function tailscaleRelease(t, files) {
  const scratch = tempDir(t, "verification-tailscale-tarball-");
  for (const [name, text] of Object.entries(files)) {
    mkdirSync(join(scratch, "tailscale-1.0.0", name, ".."), { recursive: true });
    writeFileSync(join(scratch, "tailscale-1.0.0", name), text);
  }
  const path = join(scratch, "release.tar.gz");
  execFileSync("tar", ["-czf", path, "-C", scratch, "tailscale-1.0.0"]);
  return { path, sha: sha256(readFileSync(path)) };
}
// A curl first on PATH: logs each call and copies the tarball to -o.
function curlStandIn(t, source) {
  const tools = tempDir(t, "verification-curl-"), log = join(tools, "curl.log");
  writeFileSync(join(tools, "curl"), `#!/bin/sh\nprintf '%s\\n' "$*" >> '${log}'\n` +
    `out=\nwhile [ $# -gt 0 ]; do [ "$1" = -o ] && out=$2; shift; done\ncp '${source}' "$out"\n`, { mode: 0o755 });
  return { tools, calls: () => readLines(log).length };
}
const hostGoRoot = () => execFileSync("go", ["env", "GOROOT"], { encoding: "utf8", env: { ...process.env, GOTOOLCHAIN: "local" } }).trim();
// A Homebrew GOROOT has no LICENSE (a downloaded toolchain does).
const hostGoLicense = () => (existsSync(join(hostGoRoot(), "LICENSE")) ? "LICENSE" : "PATENTS");
// The real scripts/build-wasm.sh for the fixture release, which also writes
// its environment to .build/rebuild-env. Each replacement must match as often
// as expected, so a reshaped script fails here instead of testing something else.
function fixtureWasmScript(sha) {
  let text = readFileSync(new URL("../scripts/build-wasm.sh", import.meta.url), "utf8");
  const swap = (from, to, count) => {
    assert.equal(text.split(from).length - 1, count, `build-wasm.sh mentions ${from} ${count} times`);
    text = text.replaceAll(from, to);
  };
  swap("1.102.3", "1.0.0", 3);
  swap(/^sha256=([0-9a-f]{64})$/m.exec(text)[1], sha, 1);
  swap('"$(go env GOROOT)/LICENSE"', `"$(go env GOROOT)/${hostGoLicense()}"`, 1);
  swap("\nroot=$(pwd)\n", '\nroot=$(pwd)\nmkdir -p .build\nenv > .build/rebuild-env\n', 1);
  return text;
}
const STALE_WASM = "\0asm\x01\0\0\0stale";
// A browser fixture whose hub module requires tailscale.com v1.0.0, with the
// build script and every checkout file it reads or rewrites, and a client/
// candidate whose plan reads the WASM without building it.
function sharedWasmFixture(t, proxy, release) {
  const f = browserFixture(t, { "fixture-browser.mjs": "console.log('reader ran');\n" });
  writeFileSync(join(f.cwd, ".gitignore"), "node_modules/\n.build/\nwasm/*.wasm\n");
  f.git("rm", "-q", "--cached", "wasm/tailserve.wasm");
  const files = {
    "hub/go.mod": "module example.com/hub\n\ngo 1.21\n\nrequire tailscale.com v1.0.0\n",
    "hub/ts.go": 'package hub\n\nimport "tailscale.com/x"\n\nvar _ = x.V\n',
    "wasm/upstream_js.go": 'package main\n\nimport _ "example.com/jsdep"\n',
    ...Object.fromEntries(["ssh_js.go", "upload_js.go", "sftp_js.go", "fixture_js.go"].map((name) => ["wasm/" + name, "package main\n"])),
    "wasm/wasm_exec.js": readFileSync(join(hostGoRoot(), "lib/wasm/wasm_exec.js"), "utf8"),
    "wasm/LICENSE.go": readFileSync(join(hostGoRoot(), hostGoLicense()), "utf8"),
    "scripts/build-wasm.sh": fixtureWasmScript(release.sha),
  };
  for (const [name, text] of Object.entries(files)) {
    mkdirSync(join(f.cwd, name, ".."), { recursive: true });
    writeFileSync(join(f.cwd, name), text);
  }
  execFileSync("go", ["mod", "tidy"], { cwd: join(f.cwd, "hub"), env: setupGoEnvironment(t, proxy), stdio: "pipe" });
  // The main worktree's stale build, which go build overwrites only as an
  // object file.
  for (const path of ["wasm/tailserve.wasm", ".build/test.wasm"]) writeFileSync(join(f.cwd, path), STALE_WASM);
  f.git("add", ".");
  f.git("commit", "-qm", "wasm sources");
  const baseCommit = f.git("rev-parse", "HEAD");
  const commit = commitChange(f, "client/candidate.txt", "change\n", "candidate");
  const context = { baseCommit, commit };
  return { ...f, context, plan: makePlan({ ...context, owned: ["client/"] }, f.cwd) };
}
const treeBytes = (dir) => {
  let total = 0;
  for (const entry of readdirSync(dir, { withFileTypes: true, recursive: true }))
    if (entry.isFile()) total += lstatSync(join(entry.parentPath, entry.name)).size;
  return total;
};
const insidePath = (child, parent) => {
  const rel = relative(parent, child);
  return rel === "" || (rel.split("/")[0] !== ".." && !isAbsolute(rel));
};

test("WASM prerequisite: with shared caches two fresh worktrees rebuild with the run's Go caches and one tarball", { timeout: 300000 }, async (t) => {
  const { proxy, files } = tailscaleProxy(t);
  const release = tailscaleRelease(t, files);
  const curl = curlStandIn(t, release.path);
  const f = sharedWasmFixture(t, proxy, release);
  assert(!f.plan.checks.some((check) => check.id === "wasm-test-build"), "the plan does not build the WASM itself");
  const root = join(goCacheDirectory(t, "verification-cache-wasm-rebuild-"), "root");
  const savedPath = process.env.PATH;
  process.env.PATH = curl.tools + ":" + savedPath;
  t.after(() => (process.env.PATH = savedPath));
  // Every spelling of the user's own Go caches.
  const userGo = JSON.parse(execFileSync("go", ["env", "-json", "GOMODCACHE", "GOCACHE"],
    { encoding: "utf8", env: { ...process.env, GOTOOLCHAIN: "local" } }));
  const userCaches = [join(homedir(), "go"), join(homedir(), "Library", "Caches", "go-build"), userGo.GOMODCACHE, userGo.GOCACHE];
  const runs = [];
  for (const pass of ["cold", "fresh worktree"]) {
    const wt = linkedWorktree(t, f);
    t.after(() => existsSync(join(wt, ".build")) && removeVerifierHome(join(wt, ".build")));
    const events = [], output = tempDir(t, "verification-wasm-shared-");
    const buildBefore = existsSync(join(root, "build")) ? treeBytes(join(root, "build")) : 0;
    const started = Date.now();
    const receipt = await runPlan(f.plan, wt, output, { ...wasmOptions(events), caches: { root, goProxy: proxy.url } });
    const run = { receipt, seconds: (Date.now() - started) / 1000, growth: treeBytes(join(root, "build")) - buildBefore };
    runs.push(run);
    const log = readFileSync(join(output, "wasm-prerequisite-rebuild.log"), "utf8");
    assert(receipt.checks.every((c) => c.exitCode === 0), `${pass}: ${log}`);
    assert(events.includes("matrix run: WASM prerequisites stale (no source record); rebuilding with bash scripts/build-wasm.sh --test"), pass);
    const cacheLine = events.find((line) => line.startsWith("matrix run: go caches:"));
    assert.match(cacheLine, /, WASM rebuild: shared modules$/, pass);
    assert(events.indexOf(cacheLine) < events.findIndex((line) => line.startsWith("matrix run: WASM prerequisites stale")), "caches first");

    // The rebuild's environment is the run's: its Go caches, toolchain and
    // cache root, the verifier home, and no other TAILTERM_* variable.
    const env = Object.fromEntries(readLines(join(wt, ".build/rebuild-env")).map((line) => [line.slice(0, line.indexOf("=")), line.slice(line.indexOf("=") + 1)]));
    const runEnv = receipt.environment;
    for (const name of ["GOCACHE", "GOMODCACHE", "GOPROXY", "GOFLAGS", "GOTOOLCHAIN", "PATH", "HOME", "TMPDIR", "GOPATH", "TAILTERM_VERIFICATION_CACHE"])
      assert.equal(env[name], runEnv[name], `${pass}: ${name}`);
    assert.deepEqual(Object.keys(env).filter((name) => name.startsWith("TAILTERM_")), ["TAILTERM_VERIFICATION_CACHE"], pass);
    assert.equal(env.TAILTERM_VERIFICATION_CACHE, realpathSync(root));
    assert.equal(env.GOPROXY, "off");
    for (const name of ["GOCACHE", "GOMODCACHE"]) {
      assert(insidePath(env[name], realpathSync(root)), `${pass}: ${name} ${env[name]} is under the cache root`);
      assert(!insidePath(env[name], realpathSync(wt)), `${pass}: ${name} is outside the checkout`);
      for (const user of userCaches) assert(!insidePath(env[name], user) && !insidePath(user, env[name]), `${pass}: ${name} is not ${user}`);
    }
    const state = pass === "cold" ? "downloaded" : "cached";
    assert.equal(/^build-wasm: .*$/m.exec(log)?.[0],
      `build-wasm: GOCACHE=${env.GOCACHE} GOMODCACHE=${env.GOMODCACHE} tarball=${join(realpathSync(root), "downloads", release.sha + ".tar.gz")} (${state})`, pass);
    assert.deepEqual(readdirSync(join(root, "downloads")), [release.sha + ".tar.gz"]);
    assert.equal(curl.calls(), 1, `${pass}: one download in all`);

    // The worktree has no Go cache or tarball of its own, stays clean, and
    // every reader saw the rebuilt WASM.
    for (const local of [".build/go", ".build/cache", ".build/tailscale.tar.gz"]) assert(!existsSync(join(wt, local)), `${pass}: ${local}`);
    assert.equal(spawnSync("git", ["status", "--porcelain", "--untracked-files=normal"], { cwd: wt, encoding: "utf8" }).stdout, "", pass);
    const built = ["wasm/tailserve.wasm", ".build/test.wasm"].map((path) => ({ path, sha256: sha256(readFileSync(join(wt, path))) }));
    assert(built.every((entry) => entry.sha256 !== digest(STALE_WASM)), `${pass}: rebuilt`);
    for (const entry of built) assert.deepEqual(receipt.prerequisites.find((p) => p.path === entry.path), entry);
    for (const reader of receipt.checks.filter((c) => c.wasm)) assert.deepEqual(reader.wasm, built, reader.id);
    assert.equal(matrixRunner.staleWasmCause(wt, f.context.commit), "", `${pass}: recorded`);
    run.env = env;
    run.report = JSON.parse(readFileSync(join(output, "cache-report.json"), "utf8"));
  }
  // The fresh worktree reused the same build cache, module generation and
  // tarball: nothing seeded or downloaded, and the build cache barely grew.
  assert.equal(runs[1].env.GOCACHE, runs[0].env.GOCACHE);
  assert.equal(runs[1].env.GOMODCACHE, runs[0].env.GOMODCACHE);
  assert.equal(runs[1].report.modules.state, "warm");
  assert.deepEqual(runs[1].report.downloads, { toolchains: 0, modules: 0 });
  assert.equal(runs[1].report.wasm.copy, "verified");
  assert(runs[1].growth < runs[0].growth / 10, `build cache grew ${runs[1].growth} bytes after a cold ${runs[0].growth}`);
  console.log(`wasm rebuild runs: cold ${runs[0].seconds} s (+${runs[0].growth} build bytes), ` +
    `fresh worktree ${runs[1].seconds} s (+${runs[1].growth})`);
});

// wi_5a4491949182661c: Playwright's own per-OS browser cache, not the macOS
// path on every OS; an explicit PLAYWRIGHT_BROWSERS_PATH wins unchanged.
test("browser checks use Playwright's per-OS browser cache", () => {
  const { playwrightBrowsersPath } = matrixRunner;
  const home = "/home/verifier";
  assert.equal(playwrightBrowsersPath({ HOME: "/Users/verifier" }, "darwin"), "/Users/verifier/Library/Caches/ms-playwright");
  assert.equal(playwrightBrowsersPath({ HOME: "/Users/verifier", XDG_CACHE_HOME: "/xdg" }, "darwin"), "/Users/verifier/Library/Caches/ms-playwright");
  assert.equal(playwrightBrowsersPath({ HOME: home }, "linux"), "/home/verifier/.cache/ms-playwright");
  assert.equal(playwrightBrowsersPath({ HOME: home, XDG_CACHE_HOME: "/xdg" }, "linux"), "/xdg/ms-playwright");
  assert.equal(playwrightBrowsersPath({ HOME: home, XDG_CACHE_HOME: "" }, "linux"), "/home/verifier/.cache/ms-playwright");
  for (const platform of ["darwin", "linux"]) {
    for (const named of ["/srv/browsers", "0"])
      assert.equal(playwrightBrowsersPath({ HOME: home, XDG_CACHE_HOME: "/xdg", PLAYWRIGHT_BROWSERS_PATH: named }, platform), named, platform);
  }
  // The default reads the runner's environment and platform.
  assert.equal(playwrightBrowsersPath(), process.env.PLAYWRIGHT_BROWSERS_PATH ||
    (process.platform === "darwin" ? join(process.env.HOME, "Library/Caches/ms-playwright") : join(process.env.XDG_CACHE_HOME || join(process.env.HOME, ".cache"), "ms-playwright")));
});
