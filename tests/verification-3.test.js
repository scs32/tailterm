import test from "node:test";
import assert from "node:assert/strict";
import { writeFileSync, readFileSync, rmSync, existsSync } from "node:fs";
import { join } from "node:path";
import { spawnSync } from "node:child_process";
import {
  selectChecks,
  makePlan as rawMakePlan,
  runPlan,
  digest,
  makeTargetedPlan,
} from "../scripts/verify-matrix.mjs";
// Named separately so the red tests load on a runner that lacks these exports.
import * as matrixRunner from "../scripts/verify-matrix.mjs";
import { acquireHostLock } from "../scripts/verify-matrix-host-lock.mjs";
import {
  readHostState,
  fakeTTCalls,
  fakeTTLookups,
  makePlan,
  matrix,
  tempDir,
  alive,
  hostLockFile,
  untilHost,
  plannedFixture,
  holdHost,
  integratedKnownFailure,
  integratedFixture,
  raceArgv,
  withChecks,
  tipFixture,
  checkIds,
  withoutSelection,
  runnerRecord,
  pidAlive,
  launcherCLI,
  queuedRun,
  agentIdentity,
} from "./verification-fixtures.mjs";

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
