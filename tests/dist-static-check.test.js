// Unit tests for the standalone browser suites' dist-static start check
// (wi_6e2dbfea3fbe54f3 / order #55913). No browser and no network.
import test from "node:test";
import assert from "node:assert/strict";
import { execFileSync, spawnSync } from "node:child_process";
import {
  mkdirSync,
  mkdtempSync,
  readFileSync,
  realpathSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { checkDistStatic } from "./dist-static-check.mjs";

const helper = new URL("./dist-static-check.mjs", import.meta.url);
const A = "a".repeat(40);
const B = "b".repeat(40);
const childEnv = () =>
  Object.fromEntries(
    Object.entries(process.env).filter(
      ([key]) => !/^(TAILTERM_|GIT_)/.test(key),
    ),
  );
function tempDir(t) {
  // realpath: macOS tmpdir is a /var symlink to /private/var, and the helper
  // resolves the directory a child runs in to its real path.
  const dir = realpathSync(mkdtempSync(join(tmpdir(), "dist-static-check-")));
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  return dir;
}
function build(dir, release) {
  mkdirSync(join(dir, "dist-static"), { recursive: true });
  writeFileSync(join(dir, "dist-static", "index.html"), "<!doctype html>");
  if (release !== undefined)
    writeFileSync(join(dir, "dist-static", "release.json"), release);
}
const missingLine = (dir) =>
  `dist-static is missing: no dist-static/index.html in ${dir}. Run npm run build:static in this checkout first (the verification matrix builds it itself).`;
const UNKNOWN =
  "warning: dist-static has no readable source commit in release.json; run npm run build:static if it may be stale.";
function runHelper(cwd) {
  return spawnSync(
    process.execPath,
    [
      "--input-type=module",
      "-e",
      `import {requireDistStatic} from ${JSON.stringify(helper.href)}; requireDistStatic(); console.log("continued");`,
    ],
    { cwd, env: childEnv(), encoding: "utf8" },
  );
}

test("t1: an empty checkout reports dist-static missing in one line", (t) => {
  const dir = tempDir(t);
  const result = checkDistStatic({ cwd: dir, head: () => A });
  assert.equal(result.warning, null);
  assert.equal(result.missing, missingLine(dir));
  assert.match(result.missing, /^dist-static is missing:/);
  assert.ok(result.missing.includes(dir));
  assert.ok(result.missing.includes("npm run build:static"));
  assert.ok(!result.missing.includes("\n"));
});

test("t2: a dist-static directory without index.html is missing", (t) => {
  const dir = tempDir(t);
  mkdirSync(join(dir, "dist-static", "assets"), { recursive: true });
  writeFileSync(
    join(dir, "dist-static", "release.json"),
    JSON.stringify({ commit: A }),
  );
  assert.deepEqual(checkDistStatic({ cwd: dir, head: () => A }), {
    missing: missingLine(dir),
    warning: null,
  });
});

test("t3: a build from the checkout's HEAD passes silently", (t) => {
  const dir = tempDir(t);
  build(dir, JSON.stringify({ commit: A, dirty: false }));
  assert.deepEqual(checkDistStatic({ cwd: dir, head: () => A }), {
    missing: null,
    warning: null,
  });
});

test("t4: a build from another commit warns with both short commits", (t) => {
  const dir = tempDir(t);
  build(dir, JSON.stringify({ commit: A }));
  const result = checkDistStatic({ cwd: dir, head: () => B });
  assert.equal(result.missing, null);
  assert.equal(
    result.warning,
    `warning: dist-static was built from ${A.slice(0, 12)} but this checkout is at ${B.slice(0, 12)}; run npm run build:static to test the current source.`,
  );
});

test("t5: no readable source commit warns that it is unknown", (t) => {
  for (const release of [
    undefined,
    JSON.stringify({ commit: "unknown" }),
    "{not json",
    JSON.stringify({ commit: "" }),
  ]) {
    const dir = tempDir(t);
    build(dir, release);
    assert.deepEqual(
      checkDistStatic({ cwd: dir, head: () => A }),
      { missing: null, warning: UNKNOWN },
      `release.json ${release}`,
    );
  }
});

test("t6: without a HEAD commit the staleness check is skipped", (t) => {
  const dir = tempDir(t);
  build(dir, JSON.stringify({ commit: A }));
  assert.deepEqual(checkDistStatic({ cwd: dir, head: () => null }), {
    missing: null,
    warning: null,
  });
});

test("t7: requireDistStatic exits 1 with exactly the missing line", (t) => {
  const dir = tempDir(t);
  const result = runHelper(dir);
  assert.equal(result.status, 1);
  assert.equal(result.stderr, missingLine(dir) + "\n");
  assert.ok(!result.stdout.includes("continued"));
});

test("t8: requireDistStatic continues quietly outside a git checkout", (t) => {
  const dir = tempDir(t);
  // Only meaningful if git really finds no repository here.
  assert.notEqual(
    spawnSync("git", ["rev-parse", "HEAD"], { cwd: dir, env: childEnv() })
      .status,
    0,
  );
  build(dir, JSON.stringify({ commit: A }));
  const result = runHelper(dir);
  assert.equal(result.status, 0, result.stderr);
  assert.equal(result.stdout, "continued\n");
  assert.equal(result.stderr, "");
});

test("t9: the helper is not counted as a browser suite", () => {
  const source = readFileSync(fileURLToPath(helper), "utf8");
  assert.doesNotMatch(source, /^\/\/ verify-matrix:/m);
  assert.ok(!source.includes(".launch("));
});

test("gitHead reads HEAD in a real repository", (t) => {
  const dir = tempDir(t);
  const git = (...args) =>
    execFileSync("git", args, {
      cwd: dir,
      env: childEnv(),
      encoding: "utf8",
    }).trim();
  git("init", "-q");
  git(
    "-c",
    "user.name=t",
    "-c",
    "user.email=t@example.invalid",
    "-c",
    "commit.gpgsign=false",
    "commit",
    "-q",
    "--allow-empty",
    "-m",
    "t",
  );
  const head = git("rev-parse", "HEAD");
  build(dir, JSON.stringify({ commit: head }));
  assert.deepEqual(checkDistStatic({ cwd: dir }), {
    missing: null,
    warning: null,
  });
  build(dir, JSON.stringify({ commit: A }));
  assert.match(
    checkDistStatic({ cwd: dir }).warning,
    /^warning: dist-static was built from aaaaaaaaaaaa but this checkout is at /,
  );
});
