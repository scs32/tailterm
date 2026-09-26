import {
  readFileSync,
  writeFileSync,
  mkdtempSync,
  mkdirSync,
  existsSync,
  readdirSync,
} from "node:fs";
import { resolve, relative, join, isAbsolute } from "node:path";
import { tmpdir } from "node:os";
import { createHash } from "node:crypto";
import { execFileSync, spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";

export function canonical(value) {
  if (Array.isArray(value)) return "[" + value.map(canonical).join(",") + "]";
  if (value && typeof value === "object")
    return (
      "{" +
      Object.keys(value)
        .sort()
        .map((k) => JSON.stringify(k) + ":" + canonical(value[k]))
        .join(",") +
      "}"
    );
  return JSON.stringify(value);
}
export const digest = (value) =>
  createHash("sha256")
    .update(typeof value === "string" ? value : canonical(value))
    .digest("hex");
const git = (cwd, ...args) =>
  execFileSync("git", args, { cwd, encoding: "utf8" }).trim();
export function diffPaths(cwd, base, commit) {
  const tokens = execFileSync(
    "git",
    ["diff", "--name-status", "-z", "--find-renames", base, commit],
    { cwd, encoding: "utf8" },
  ).split("\0");
  const paths = [];
  while (tokens.length && tokens[0]) {
    const status = tokens.shift();
    paths.push(tokens.shift());
    if (/^[RC]/.test(status)) paths.push(tokens.shift());
  }
  return paths;
}
export function selectChecks(matrix, owned, changed) {
  if (matrix.version !== 1 || !owned.length)
    throw new Error("Versioned matrix and ownership required");
  const paths = [...new Set([...owned, ...changed])].sort(),
    groups = new Set();
  for (const p of paths) {
    if (!p || isAbsolute(p) || p.split("/").includes("..") || /[\0\n]/.test(p))
      throw new Error("Invalid owned/diff path");
    const matches = matrix.rules.filter((r) =>
      r.prefixes.some((prefix) =>
        prefix.endsWith("/") ? p.startsWith(prefix) : p === prefix,
      ),
    );
    if (!matches.length) throw new Error("Unknown path: " + p);
    for (const r of matches) for (const g of r.groups) groups.add(g);
  }
  const checks = [];
  const add = (id, argv, cwd = ".", environment = {}) =>
    checks.push({ id, argv, cwd, environment });
  if (groups.has("unit")) add("npm-unit", ["npm", "test"]);
  if (groups.has("browser")) {
    add("00-static-build", ["npm", "run", "build:static"]);
    add("01-static-release-verify", ["npm", "run", "verify:release"]);
  }
  if (groups.has("browser"))
    for (const s of matrix.browserSuites) {
      if (s.mode === "both")
        add(s.file, ["node", s.file], ".", { TEST_BROWSER: "both" });
      else
        for (const engine of ["chromium", "webkit"])
          add(s.file + ":" + engine, ["node", s.file], ".", {
            TEST_BROWSER: engine,
          });
    }
  if (groups.has("go")) {
    add("go-vet", ["go", "vet", "./..."], "hub");
    add("go-test", ["go", "test", "./..."], "hub");
    const packages = [
      ...new Set(
        paths
          .filter((p) => p.startsWith("hub/") && p.endsWith(".go"))
          .map((p) => "./" + p.slice(4, p.lastIndexOf("/"))),
      ),
    ].sort();
    add(
      "go-race",
      ["go", "test", "-race", ...(packages.length ? packages : ["./..."])],
      "hub",
    );
  }
  if (groups.has("migration"))
    add(
      "migration-rehearsal",
      [
        "go",
        "test",
        "./internal/store",
        "-run",
        "TestVerificationMigrationRehearsal",
        "-count=1",
      ],
      "hub",
    );
  if (groups.has("wasm"))
    add("wasm-test-build", ["bash", "scripts/build-wasm.sh", "--test"]);
  return checks.sort((a, b) => a.id.localeCompare(b.id, "en"));
}
export function assertInventory(matrix, cwd) {
  const inventory = readdirSync(join(cwd, "tests"))
    .filter(
      (f) =>
        f.endsWith(".mjs") &&
        readFileSync(join(cwd, "tests", f), "utf8").includes(".launch("),
    )
    .map((f) => "tests/" + f)
    .sort();
  const declared = [
    ...matrix.browserSuites.map((s) => s.file),
    ...matrix.excludedBrowserSuites,
  ].sort();
  if (canonical(inventory) !== canonical(declared))
    throw new Error("Browser inventory changed; update approved matrix");
}
export function makePlan(context, cwd) {
  if (
    !/^[a-f0-9]{40}$/.test(context.commit) ||
    !/^[a-f0-9]{40}$/.test(context.baseCommit)
  )
    throw new Error("Exact base/candidate SHA required");
  const raw = readFileSync(join(cwd, "verification/matrix.json"), "utf8"),
    matrix = JSON.parse(raw);
  assertInventory(matrix, cwd);
  const changed = diffPaths(cwd, context.baseCommit, context.commit);
  const checks = selectChecks(matrix, context.owned, changed);
  return {
    ...context,
    version: 1,
    matrixDigest: digest(raw),
    checksDigest: digest(checks),
    changed,
    checks,
  };
}
export function checkClean(cwd, commit) {
  if (git(cwd, "rev-parse", "HEAD") !== commit)
    throw new Error("Wrong candidate SHA");
  const symbolic = spawnSync("git", ["symbolic-ref", "-q", "HEAD"], { cwd });
  if (
    symbolic.status !== 1 ||
    git(cwd, "status", "--porcelain", "--untracked-files=normal")
  )
    throw new Error("Clean detached worktree required");
}
export function runPlan(plan, cwd, output) {
  checkClean(cwd, plan.commit);
  const expected = makePlan(plan, cwd);
  if (
    expected.matrixDigest !== plan.matrixDigest ||
    expected.checksDigest !== plan.checksDigest ||
    canonical(expected.checks) !== canonical(plan.checks)
  )
    throw new Error("Altered or omitted required checks");
  if (!isAbsolute(output) || relative(cwd, output).split("/")[0] !== "..")
    throw new Error("Logs/receipt must be outside worktree");
  const home = mkdtempSync(join(tmpdir(), "tailterm-verifier-"));
  mkdirSync(output, { recursive: true });
  const environment = {
    PATH: process.env.PATH,
    HOME: home,
    TMPDIR: home,
    LANG: "en_US.UTF-8",
    CI: "1",
    GOTOOLCHAIN: "auto",
  };
  // No inherited task/hub credentials, runtime config, vault or tmux socket.
  const prerequisites = [];
  if (
    plan.checks.some(
      (c) => c.id.includes("browser") || c.environment.TEST_BROWSER,
    )
  ) {
    for (const p of [
      "node_modules/.package-lock.json",
      "wasm/tailserve.wasm",
      ".build/test.wasm",
      ".build/speech-fixture.wav",
      ".build/go-modules.txt",
    ]) {
      const f = join(cwd, p);
      if (!existsSync(f)) throw new Error("Missing prerequisite: " + p);
      prerequisites.push({
        path: p,
        sha256: createHash("sha256").update(readFileSync(f)).digest("hex"),
      });
    }
    environment.PLAYWRIGHT_BROWSERS_PATH =
      process.env.PLAYWRIGHT_BROWSERS_PATH ||
      join(process.env.HOME, "Library/Caches/ms-playwright");
  }
  const results = [];
  for (const check of plan.checks) {
    const startedAt = new Date().toISOString(),
      start = performance.now();
    const run = spawnSync(check.argv[0], check.argv.slice(1), {
      cwd: resolve(cwd, check.cwd),
      env: { ...environment, ...check.environment },
      encoding: "utf8",
      maxBuffer: 128 * 1024 * 1024,
    });
    const log =
      (run.stdout || "") +
      (run.stderr || "") +
      (run.error ? "\n" + run.error.message : "");
    const logURI = join(output, digest(check.id) + ".log");
    writeFileSync(logURI, log, { mode: 0o600 });
    results.push({
      ...check,
      startedAt,
      endedAt: new Date().toISOString(),
      durationMs: Math.round(performance.now() - start),
      exitCode: run.status ?? -1,
      logURI,
      logDigest: digest(log),
    });
  }
  checkClean(cwd, plan.commit);
  const receipt = {
    worktree: resolve(cwd),
    version: 1,
    operationKey: plan.operationKey,
    planDigest: digest(plan),
    repository: plan.repository,
    baseCommit: plan.baseCommit,
    commit: plan.commit,
    matrixDigest: plan.matrixDigest,
    checksDigest: plan.checksDigest,
    verifierAgentId: plan.verifierAgentId,
    verifierRunId: plan.verifierRunId,
    detached: true,
    cleanBefore: true,
    cleanAfter: true,
    environment,
    prerequisites,
    checks: results,
    aiv: { state: "unsubmitted" },
  };
  writeFileSync(
    join(output, "receipt.json"),
    JSON.stringify(receipt, null, 2) + "\n",
    { mode: 0o600 },
  );
  return receipt;
}
if (
  process.argv[1] &&
  resolve(process.argv[1]) === fileURLToPath(import.meta.url)
) {
  try {
    const [mode, file, output] = process.argv.slice(2),
      input = JSON.parse(readFileSync(file, "utf8"));
    if (mode === "plan")
      writeFileSync(
        output,
        JSON.stringify(makePlan(input, process.cwd()), null, 2) + "\n",
      );
    else if (mode === "run") {
      const r = runPlan(input, process.cwd(), resolve(output));
      process.exitCode = r.checks.every((c) => c.exitCode === 0) ? 0 : 1;
    } else
      throw new Error(
        "Usage: node scripts/verify-matrix.mjs plan|run INPUT OUTPUT",
      );
  } catch (e) {
    console.error(e.message);
    process.exitCode = 1;
  }
}
