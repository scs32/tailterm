import test from "node:test";
import assert from "node:assert/strict";
import { mkdtempSync, mkdirSync, writeFileSync, readFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { execFileSync } from "node:child_process";
import {
  selectChecks,
  diffPaths,
  makePlan,
  runPlan,
  digest,
  assertInventory,
} from "../scripts/verify-matrix.mjs";
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
  const cwd = mkdtempSync(join(tmpdir(), "verification-fixture-"));
  mkdirSync(join(cwd, "tests"));
  mkdirSync(join(cwd, "verification"));
  const git = (...args) =>
    execFileSync("git", args, { cwd, encoding: "utf8" }).trim();
  git("init", "-q");
  git("config", "user.name", "Fixture");
  git("config", "user.email", "fixture@example.invalid");
  const m = {
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
  const out = mkdtempSync(join(tmpdir(), "verification-logs-"));
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
    out = mkdtempSync(join(tmpdir(), "verification-out-"));
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
  const output = mkdtempSync(join(tmpdir(), "verification-missing-assets-"));
  assert.throws(
    () => runPlan(plan, f.cwd, output),
    /Missing prerequisite: node_modules/,
  );
});
