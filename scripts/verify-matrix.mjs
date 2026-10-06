import {
  readFileSync,
  writeFileSync,
  mkdtempSync,
  mkdirSync,
  existsSync,
  readdirSync,
  statSync,
  statfsSync,
  lstatSync,
  chmodSync,
  rmSync,
  realpathSync,
} from "node:fs";
import { resolve, relative, join, isAbsolute, dirname, basename } from "node:path";
import {
  tmpdir,
  availableParallelism,
  totalmem,
  getPriority,
  setPriority,
} from "node:os";
import { createHash } from "node:crypto";
import { execFileSync, spawn, spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";
import { buildHistoricalHub, buildMatrixBinaries, fileHash } from "../tests/test-binaries.mjs";
import {
  acquireHostLock,
  resolveRunPriority,
  receiptKeys,
  minutesFlag,
  DEFAULT_HOST_WAIT_MS,
} from "./verify-matrix-host-lock.mjs";

const binarySuites = new Set([
  "profile-sync", "task-form", "lead-replacement", "project-work-items",
  "board-compose", "item-message-compose", "project-queue", "board-decisions",
  "owner-obligations", "dropdown-continuity", "board-audit-old-hub",
]);
const historicalHubCommit = "58f9185981625b148b30ad4b5070a1a658e17f77";

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
// A plan verifies exactly what merges only when the candidate contains its
// base, so the candidate lands on that base as a fast-forward.
export function assertFastForward(cwd, base, commit) {
  const ancestry = spawnSync("git", ["merge-base", "--is-ancestor", base, commit], {
    cwd,
    encoding: "utf8",
  });
  if (ancestry.status === 1)
    throw new Error(
      "candidate is not a fast-forward of its base; rebase onto the current tip",
    );
  if (ancestry.status !== 0)
    throw new Error(
      "Unable to check candidate ancestry: " +
        (ancestry.error?.message || ancestry.stderr.trim()),
    );
}
export const MAX_CHECK_TIMEOUT_MS = 3600000;
export function selectChecks(
  matrix,
  owned,
  changed,
  candidatePackages,
  { targeted = false } = {},
) {
  if (matrix.version !== 1 || (!owned.length && !targeted))
    throw new Error("Versioned matrix and ownership required");
  // Approved go test flags for go-test and go-race. Only a package timeout
  // is allowed: hub/internal/store's race suite runs 556-574 s against go
  // test's default 600 s (owner decision #15114).
  const goTestFlags = matrix.goTestFlags ?? [];
  if (
    !Array.isArray(goTestFlags) ||
    goTestFlags.some((flag) => !/^-timeout=[1-9][0-9]*m$/.test(flag))
  )
    throw new Error("Invalid matrix goTestFlags");
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
  const add = (id, argv, cwd = ".", environment = {}) => {
    const timeout =
      matrix.checkTimeoutMs?.[id] ?? matrix.defaultTimeoutMs ?? 600000;
    if (!Number.isSafeInteger(timeout) || timeout <= 0 || timeout > MAX_CHECK_TIMEOUT_MS)
      throw new Error("Invalid matrix check timeout: " + id);
    checks.push({
      id,
      argv,
      cwd,
      environment: { ...environment, VERIFICATION_TIMEOUT_MS: String(timeout) },
    });
  };
  if (groups.has("unit")) add("npm-unit", ["npm", "test"]);
  if (groups.has("browser")) {
    add("00-static-build", ["npm", "run", "build:static"]);
    add("01-static-release-verify", ["npm", "run", "verify:release"]);
  }
  if (groups.has("browser"))
    for (const s of matrix.browserSuites) {
      if (s.mode === "both")
        add(s.file, ["node", s.file], ".", {
          TEST_BROWSER: "both",
          ...(s.requiredPorts?.length
            ? { VERIFICATION_REQUIRED_PORTS: s.requiredPorts.join(",") }
            : {}),
        });
      else
        for (const engine of ["chromium", "webkit"])
          add(s.file + ":" + engine, ["node", s.file], ".", {
            TEST_BROWSER: engine,
            ...(s.requiredPorts?.length
              ? { VERIFICATION_REQUIRED_PORTS: s.requiredPorts.join(",") }
              : {}),
          });
    }
  if (groups.has("go")) {
    add("go-vet", ["go", "vet", "./..."], "hub");
    add("go-test", ["go", "test", ...goTestFlags, "./..."], "hub");
    const packages = [
      ...new Set(
        paths
          .filter((p) => p.startsWith("hub/") && p.endsWith(".go"))
          .map((p) => "./" + p.slice(4, p.lastIndexOf("/")))
          .filter((p) =>
            candidatePackages
              ? candidatePackages.has(p)
              : existsSync(join(process.cwd(), "hub", p)) &&
                statSync(join(process.cwd(), "hub", p)).isDirectory(),
          ),
      ),
    ].sort();
    add(
      "go-race",
      [
        "go",
        "test",
        "-race",
        ...goTestFlags,
        ...(packages.length ? packages : ["./..."]),
      ],
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
        "-v",
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
export function matrixPolicy(matrix, checks) {
  if (matrix.maxAttempts === undefined && matrix.knownFailures === undefined)
    return {};
  if (matrix.maxAttempts !== 3 || !Array.isArray(matrix.knownFailures))
    throw new Error("Approved maxAttempts=3 and knownFailures required");
  const ids = new Set([
    "npm-unit",
    "00-static-build",
    "01-static-release-verify",
    "go-vet",
    "go-test",
    "go-race",
    "migration-rehearsal",
    "wasm-test-build",
  ]);
  for (const suite of matrix.browserSuites)
    for (const id of suite.mode === "both"
      ? [suite.file]
      : [suite.file + ":chromium", suite.file + ":webkit"])
      ids.add(id);
  const seen = new Set();
  for (const entry of matrix.knownFailures) {
    if (
      !ids.has(entry.checkId) ||
      seen.has(entry.checkId) ||
      !/^tsk_[a-f0-9]{16}$/.test(entry.bugTaskId || "") ||
      !/^wi_[a-f0-9]{16}$/.test(entry.bugId || "") ||
      Object.keys(entry).sort().join(",") !== "bugId,bugTaskId,checkId"
    )
      throw new Error("Invalid, unknown or duplicate known failure");
    seen.add(entry.checkId);
  }
  const selected = new Set(checks.map((c) => c.id));
  const knownFailures = matrix.knownFailures.filter((e) =>
    selected.has(e.checkId),
  );
  return { maxAttempts: 3, ...(knownFailures.length ? { knownFailures } : {}) };
}
export function receiptEligible(receipt) {
  return (
    !receipt.environment?.VERIFICATION_HOME_CLEANUP_ERROR &&
    receipt.checks.every((c) => c.exitCode === 0 || c.knownFailure === true)
  );
}
const candidateGoPackages = (cwd, commit) =>
  new Set(
    execFileSync("git", ["ls-tree", "-r", "--name-only", commit, "hub"], {
      cwd,
      encoding: "utf8",
    })
      .split("\n")
      .filter((p) => p.endsWith(".go"))
      .map((p) => "./" + p.slice(4, p.lastIndexOf("/"))),
  );
// go-race argv splits at its first package; every later element is a package
// too, the same shape the hub's coverage rule reads.
function goRacePackages(argv) {
  const first = argv.findIndex((a) => a.startsWith("./"));
  if (first < 1 || argv.slice(first).some((a) => !a.startsWith("./")))
    return null;
  return { flags: argv.slice(0, first), packages: argv.slice(first) };
}
// A plan built from a context that carries an accepted plan's checks never
// runs less than that plan. The release runner builds the integrated plan from
// the accepted plan and the integrated commit, whose diff from the accepted
// base holds other items' changes: a docs-only item's go-race ran ./..., and
// selected from the integrated diff alone it would name only those items'
// packages, which the hub refuses. Only go-race depends on the diff, so it is
// the one check that may differ: its packages are the union of both lists,
// and ./... wins. Any other difference has one approved cause: the owner
// approved a new matrix after acceptance, which may rebuild any check
// (timeouts, flags, environment). Then the rebuilt check stands, and a rebuilt
// go-race still names every accepted package, the hub's matrix-change rule.
// Under an unchanged matrix such a difference, and under either an accepted
// check this selection lacks, fails here rather than at the import.
// Returns the checks and a record of what was kept.
export function keepAcceptedChecks(
  selected,
  accepted,
  acceptedDigest,
  { matrixChanged = false } = {},
) {
  const refuse = (message) =>
    Object.assign(new Error(message), { acceptedChecks: true });
  const wellFormed = (c) =>
    c &&
    typeof c.id === "string" &&
    typeof c.cwd === "string" &&
    Array.isArray(c.argv) &&
    c.argv.every((a) => typeof a === "string") &&
    c.environment &&
    typeof c.environment === "object" &&
    Object.values(c.environment).every((v) => typeof v === "string");
  if (
    !Array.isArray(accepted) ||
    !accepted.every(wellFormed) ||
    new Set(accepted.map((c) => c.id)).size !== accepted.length
  )
    throw refuse("Invalid accepted checks");
  if (acceptedDigest !== undefined && acceptedDigest !== digest(accepted))
    throw refuse("Accepted checks do not match their checksDigest");
  const byId = new Map(accepted.map((c) => [c.id, c]));
  for (const check of accepted)
    if (!selected.some((c) => c.id === check.id))
      throw refuse("Accepted check is not selected for this commit: " + check.id);
  const kept = [],
    widened = [],
    rebuilt = [],
    added = [],
    narrowerSelection = [];
  const checks = selected.map((check) => {
    const prior = byId.get(check.id);
    if (!prior) {
      added.push(check.id);
      return check;
    }
    if (canonical(prior) === canonical(check)) {
      kept.push(check.id);
      return check;
    }
    const race = check.id === "go-race",
      want = race && goRacePackages(prior.argv),
      have = want && goRacePackages(check.argv);
    const samePolicy =
      want &&
      have &&
      canonical(want.flags) === canonical(have.flags) &&
      canonical({ ...prior, argv: [] }) === canonical({ ...check, argv: [] });
    if ((race && !(want && have)) || (!samePolicy && !matrixChanged))
      throw refuse("Accepted check differs from the selected one: " + check.id);
    if (!race) {
      rebuilt.push(check.id);
      return check;
    }
    const all = [want, have].some((p) => p.packages.includes("./..."));
    const packages = all
      ? ["./..."]
      : [...new Set([...want.packages, ...have.packages])].sort();
    const merged = { ...check, argv: [...have.flags, ...packages] };
    if (!samePolicy) rebuilt.push(check.id);
    else if (canonical(merged) === canonical(prior)) kept.push(check.id);
    else widened.push(check.id);
    if (canonical(merged) !== canonical(check)) narrowerSelection.push(check.id);
    return merged;
  });
  return {
    checks,
    preserved: {
      version: 1,
      acceptedChecksDigest: digest(accepted),
      kept,
      widened,
      rebuilt,
      added,
      narrowerSelection,
    },
  };
}
// Which diff an integrated plan selects its checks from. The release runner
// hands over the tasks-hub tip its candidate was integrated onto
// (selectionBaseCommit) and the candidates' own changed and owned paths
// (selectionPaths). The tip rule applies only to a readable tip on the line
// from the bound base to the commit; anything else, a git failure included,
// selects from the bound base as before and never throws.
function selectionRule(context, cwd) {
  const jobBase = (reason) => ({
    rule: "job-base",
    baseCommit: context.baseCommit,
    reason,
  });
  const tip = context.selectionBaseCommit,
    paths = context.selectionPaths;
  if (tip === undefined) return jobBase("no-selection-base");
  if (typeof tip !== "string" || !/^[a-f0-9]{40}$/.test(tip))
    return jobBase("invalid-selection-base");
  if (!Array.isArray(paths) || paths.some((p) => typeof p !== "string"))
    return jobBase("invalid-selection-paths");
  const ok = (...args) =>
    spawnSync("git", args, { cwd, encoding: "utf8" }).status === 0;
  if (!ok("rev-parse", "--verify", "--quiet", `${tip}^{commit}`))
    return jobBase("unreadable-selection-base");
  if (
    !ok("merge-base", "--is-ancestor", tip, context.commit) ||
    !ok("merge-base", "--is-ancestor", context.baseCommit, tip)
  )
    return jobBase("selection-base-off-line");
  return {
    rule: "integration-tip",
    baseCommit: context.baseCommit,
    selectionBaseCommit: tip,
  };
}
// The plan, and for a context that carries accepted checks the record of what
// was kept. The record stays outside the plan: the hub binds a receipt to the
// digest of the plan fields it knows, so a plan carries no others. selection
// names the rule the checks were selected by, also outside the plan.
export function planWithPreservation(context, cwd) {
  if (
    !/^[a-f0-9]{40}$/.test(context.commit) ||
    !/^[a-f0-9]{40}$/.test(context.baseCommit)
  )
    throw new Error("Exact base/candidate SHA required");
  const raw = readFileSync(join(cwd, "verification/matrix.json"), "utf8"),
    matrix = JSON.parse(raw);
  if (
    !/^[a-f0-9]{64}$/.test(context.approvedMatrixDigest || "") ||
    context.approvedMatrixDigest !== digest(raw) ||
    !(context.matrixApprovalMessageSeq > 0)
  )
    throw new Error(
      "Independent owner-approved matrix digest and source required",
    );
  assertInventory(matrix, cwd);
  assertFastForward(cwd, context.baseCommit, context.commit);
  const selection = selectionRule(context, cwd);
  let changed;
  if (selection.rule === "integration-tip") {
    try {
      changed = [
        ...new Set([
          ...diffPaths(cwd, selection.selectionBaseCommit, context.commit),
          ...context.selectionPaths,
        ]),
      ].sort();
    } catch {
      delete selection.selectionBaseCommit;
      selection.rule = "job-base";
      selection.reason = "unreadable-selection-base";
    }
  }
  changed ??= diffPaths(cwd, context.baseCommit, context.commit);
  let checks = selectChecks(
    matrix,
    context.owned,
    changed,
    candidateGoPackages(cwd, context.commit),
  );
  for (const check of checks)
    if (check.argv[0] === "go")
      check.environment.VERIFICATION_BASE_COMMIT = context.baseCommit;
  // The carried matrixDigest is the accepted plan's; the release runner swaps
  // only the approval fields when the integrated checkout's matrix is newer.
  const matrixChanged =
    /^[a-f0-9]{64}$/.test(context.matrixDigest || "") &&
    context.matrixDigest !== digest(raw);
  let preserved = null;
  if (context.checks !== undefined) {
    ({ checks, preserved } = keepAcceptedChecks(
      checks,
      context.checks,
      context.checksDigest,
      { matrixChanged },
    ));
    if (matrixChanged) preserved.acceptedMatrixDigest = context.matrixDigest;
  }
  const {
    maxAttempts,
    knownFailures,
    selectionBaseCommit,
    selectionPaths,
    ...inputContext
  } = context;
  const plan = {
    ...inputContext,
    version: 1,
    ...matrixPolicy(matrix, checks),
    matrixDigest: digest(raw),
    checksDigest: digest(checks),
    changed,
    checks,
  };
  if (preserved) {
    if (matrixChanged) preserved.matrixDigest = plan.matrixDigest;
    preserved.checksDigest = plan.checksDigest;
  }
  return { plan, preserved, selection };
}
export function makePlan(context, cwd) {
  return planWithPreservation(context, cwd).plan;
}
// Where plan mode writes the record of kept accepted checks for a plan file.
export function preservationPath(planPath) {
  return planPath.replace(/\.json$/, "") + ".preserved.json";
}
// The selection inputs a plan's record names, for run mode to re-derive the
// plan by the rule it was built with. A missing, unreadable or malformed
// record, or one that names no integration tip, gives none: the plan is then
// re-derived from its bound base.
export function recordedSelection(planPath) {
  try {
    const { selection } = JSON.parse(
      readFileSync(preservationPath(planPath), "utf8"),
    );
    if (
      selection?.rule !== "integration-tip" ||
      typeof selection.selectionBaseCommit !== "string" ||
      !Array.isArray(selection.selectionPaths)
    )
      return undefined;
    return {
      selectionBaseCommit: selection.selectionBaseCommit,
      selectionPaths: selection.selectionPaths,
    };
  } catch {
    return undefined;
  }
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
// Port and timeout policy is included in the approved command environment,
// and failure reasons are retained in the receipt and its hashed logs.
export function occupiedPorts(check) {
  const raw = check.environment.VERIFICATION_REQUIRED_PORTS;
  if (!raw) return [];
  const ports = raw.split(",").map(Number);
  if (ports.some((p) => !Number.isInteger(p) || p < 1 || p > 65535))
    throw new Error("Invalid required port list");
  return ports.flatMap((port) => {
    const probe = spawnSync(
      "lsof",
      ["-nP", "-iTCP:" + port, "-sTCP:LISTEN", "-Fp"],
      { encoding: "utf8", timeout: 5000 },
    );
    if (probe.error)
      throw new Error(
        "Port owner inspection unavailable: " + probe.error.message,
      );
    if (probe.status === 1 && !probe.stdout && !probe.stderr) return [];
    if (probe.status !== 0)
      throw new Error("Port owner inspection failed: " + probe.stderr);
    const pids = [...probe.stdout.matchAll(/^p(\d+)$/gm)].map((m) =>
      Number(m[1]),
    );
    if (!pids.length)
      throw new Error("Occupied port " + port + " has unavailable PID");
    return [{ port, pids: [...new Set(pids)] }];
  });
}
// onGroup(pgid, running) reports the check's process group as it starts and
// ends, so the host lock holder can record what is still running.
export async function runCheck(check, cwd, environment, abortSignal, onGroup) {
  const timeout = Number(check.environment.VERIFICATION_TIMEOUT_MS);
  if (!Number.isSafeInteger(timeout) || timeout <= 0 || timeout > MAX_CHECK_TIMEOUT_MS)
    throw new Error("Invalid approved check timeout");
  if (process.platform === "win32")
    throw new Error("POSIX process groups required for verification");
  try {
    const holders = occupiedPorts(check);
    if (holders.length)
      return {
        status: -1,
        stdout: "",
        stderr: holders
          .map(
            (h) =>
              "Required port " +
              h.port +
              " occupied by PID " +
              h.pids.join(","),
          )
          .join("\n"),
        failureReason: "port-conflict",
      };
  } catch (error) {
    return {
      status: -1,
      stdout: "",
      stderr: error.message,
      failureReason: "port-inspection",
    };
  }
  if (abortSignal?.aborted)
    return {
      status: abortSignal.reason === "SIGINT" ? 130 : 143,
      stdout: "",
      stderr: "",
      failureReason: "interrupted",
    };
  return new Promise((resolveResult) => {
    const stdout = [],
      stderr = [];
    let size = 0,
      reason = "",
      closed = false,
      forced = false;
    let code = -1,
      whichSignal = "",
      timer,
      killTimer,
      finished = false;
    let child;
    const signalGroup = (kind) => {
      if (!child?.pid) return;
      try {
        process.kill(-child.pid, kind);
      } catch (error) {
        if (error.code !== "ESRCH")
          stderr.push(Buffer.from("\nGroup signal: " + error.message));
      }
    };
    const finish = () => {
      if (finished || !closed || (reason && !forced)) return;
      finished = true;
      clearTimeout(timer);
      clearTimeout(killTimer);
      abortSignal?.removeEventListener("abort", onAbort);
      resolveResult({
        status:
          reason === "timeout"
            ? 124
            : reason === "output-limit"
              ? 125
              : reason === "interrupted"
                ? abortSignal.reason === "SIGINT"
                  ? 130
                  : 143
                : code,
        stdout: Buffer.concat(stdout).toString(),
        stderr: Buffer.concat(stderr).toString(),
        failureReason: reason || (code !== 0 ? "exit" : ""),
        signal: whichSignal,
      });
    };
    const stop = (why) => {
      if (reason) return;
      reason = why;
      clearTimeout(timer);
      signalGroup("SIGTERM");
      killTimer = setTimeout(() => {
        signalGroup("SIGKILL");
        forced = true;
        finish();
      }, 250);
    };
    const onAbort = () => stop("interrupted");
    const capture = (dest) => (data) => {
      size += data.length;
      if (size > 128 * 1024 * 1024) {
        stop("output-limit");
        return;
      }
      dest.push(data);
    };
    try {
      child = spawn(check.argv[0], check.argv.slice(1), {
        cwd,
        env: { ...environment, ...check.environment },
        detached: true,
        stdio: ["ignore", "pipe", "pipe"],
      });
    } catch (error) {
      resolveResult({
        status: -1,
        stdout: "",
        stderr: error.message,
        failureReason: "spawn",
      });
      return;
    }
    // Relative to the runner's own priority. Descendants inherit it; the
    // child sets up its runtime before it can spawn any, so this runs first.
    if (!isGoCheck(check) && child.pid)
      try {
        setPriority(child.pid, Math.min(19, getPriority() + NON_GO_NICE));
      } catch (error) {
        stderr.push(Buffer.from("verification priority: " + error.message + "\n"));
      }
    if (child.pid) onGroup?.(child.pid, true);
    abortSignal?.addEventListener("abort", onAbort, { once: true });
    child.stdout.on("data", capture(stdout));
    child.stderr.on("data", capture(stderr));
    child.on("error", (error) => {
      stderr.push(Buffer.from(error.message));
      reason = "spawn";
      forced = true;
      closed = true;
      finish();
    });
    child.on("close", (status, signal) => {
      code = status ?? -1;
      whichSignal = signal || "";
      closed = true;
      if (child.pid) onGroup?.(child.pid, false);
      finish();
    });
    timer = setTimeout(() => stop("timeout"), timeout);
    if (abortSignal?.aborted) onAbort();
  });
}
const DEFAULT_MIN_FREE_BYTES = 2 * 1024 * 1024 * 1024;

export function availableBytes(path = tmpdir()) {
  const { bavail, bsize } = statfsSync(path);
  return bavail * bsize;
}

export function requireFreeSpace(reserve, getAvailableBytes = availableBytes) {
  if (!Number.isSafeInteger(reserve) || reserve < 0)
    throw new Error("Invalid --min-free-bytes reserve");
  let free;
  try {
    free = getAvailableBytes();
  } catch (error) {
    throw new Error("Unable to determine free space for verifier home: " + error.message, { cause: error });
  }
  if (!Number.isSafeInteger(free) || free < 0)
    throw new Error("Unable to determine free space for verifier home");
  if (free < reserve)
    throw new Error(
      `Insufficient free space for verifier home: ${free} bytes available, ${reserve} bytes required`,
    );
}

const isBrowserCheck = (check) =>
  check.id.includes("browser") || check.environment?.TEST_BROWSER;

// Readability is checked by reading the bytes, not just testing existence.
// Call again after admission: these hashes must describe the held run's inputs.
export function readPrerequisites(checks, cwd, read = readFileSync) {
  const browser = checks.some(isBrowserCheck);
  const paths = new Set();
  if (browser || checks.some((check) => check.argv[0] === "npm"))
    paths.add("node_modules/.package-lock.json");
  if (browser)
    for (const path of [
      "wasm/tailserve.wasm",
      ".build/test.wasm",
      ".build/speech-fixture.wav",
      ".build/go-modules.txt",
    ]) paths.add(path);
  return [...paths].map((path) => {
    let bytes;
    try {
      bytes = read(join(cwd, path));
    } catch (error) {
      throw new Error(
        (error.code === "ENOENT" ? "Missing prerequisite: " : "Unreadable prerequisite: ") +
          path + (error.code === "ENOENT" ? "" : ": " + error.message),
        { cause: error },
      );
    }
    return { path, sha256: createHash("sha256").update(bytes).digest("hex") };
  });
}

// Go makes module-cache directories read-only. Only walk the exact home this
// invocation created; lstat avoids traversing symlinks into sibling runs.
export function removeVerifierHome(home) {
  const visit = (path) => {
    const entry = lstatSync(path);
    if (entry.isSymbolicLink()) return;
    if (entry.isDirectory()) {
      chmodSync(path, entry.mode | 0o700);
      for (const name of readdirSync(path)) visit(join(path, name));
    } else if (entry.isFile()) {
      chmodSync(path, entry.mode | 0o600);
    }
  };
  visit(home);
  rmSync(home, { recursive: true, force: true });
}

// A check that reaches codex through PATH gets this failing stub instead: the
// real CLI starts a setsid'd managed daemon under the verifier home that no
// process-group signal reaches (wi_39bd40d33a4d8acc). Each call is logged to
// the output directory as audit evidence of which check reached codex.
export function installCodexStub(home, output) {
  const bin = join(home, ".verifier-bin"),
    log = "'" + join(output, "codex-stub-calls.log").replaceAll("'", "'\\''") + "'";
  mkdirSync(bin, { mode: 0o700 });
  writeFileSync(
    join(bin, "codex"),
    `#!/bin/sh
args=$(printf '%s ' "$@" | tr '\\n\\t' '  ' | cut -c1-500)
printf '%s\\t%s\\t%s\\t%s\\t%s\\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$$" "$PPID" "$PWD" "\${args% }" >> ${log}
echo 'tailterm verifier: codex is disabled during matrix checks' >&2
exit 1
`,
    { mode: 0o700 },
  );
  return bin;
}

// The spellings a process may use for the verifier home: the path mkdtemp
// returned and its realpath (/var links to /private/var, and lsof reports the
// realpath). Resolving the parent keeps this valid after the home is removed.
function homeRoots(home) {
  return [...new Set([home, join(realpathSync(dirname(home)), basename(home))])];
}
const underRoots = (roots, path) =>
  roots.some((root) => path === root || path.startsWith(root + "/"));

// HOME, TMPDIR or PWD values of each listed process. Raw environments hold
// other processes' credentials, so they are matched here and never kept.
function environmentMatches(run, table, roots) {
  const matches = new Set(),
    variable = /(?:^|\s)(?:HOME|TMPDIR|PWD)=(\S+)/g;
  if (process.platform === "darwin") {
    for (const line of run("ps", ["-axww", "-E", "-o", "pid=,command="]).split("\n")) {
      const found = /^\s*(\d+)\s(.*)$/.exec(line);
      if (found && table.has(Number(found[1])))
        for (const [, value] of found[2].matchAll(variable))
          if (underRoots(roots, value)) matches.add(Number(found[1]));
    }
  } else if (process.platform === "linux") {
    for (const pid of table.keys()) {
      let environ;
      try {
        environ = readFileSync(`/proc/${pid}/environ`, "utf8");
      } catch (error) {
        if (error.code === "ENOENT" || error.code === "ESRCH") continue;
        throw error;
      }
      for (const entry of environ.split("\0")) {
        const found = /^(?:HOME|TMPDIR|PWD)=(.*)$/.exec(entry);
        if (found && underRoots(roots, found[1])) matches.add(pid);
      }
    }
  } else throw new Error("Unsupported platform for the verifier home sweep");
  return matches;
}

// Processes of this user that still live in or hold files under the verifier
// home, found by environment, argv[0], cwd or any open file. The caller and
// its ancestors are never listed. ps or lsof failure throws (fail closed).
export function verifierHomeProcesses(home) {
  const roots = homeRoots(home),
    uid = process.getuid();
  const run = (command, args) =>
    execFileSync(command, args, {
      encoding: "utf8",
      timeout: 60_000,
      maxBuffer: 512 * 1024 * 1024,
      stdio: ["ignore", "pipe", "pipe"],
    });
  const all = new Map();
  for (const line of run("ps", ["-axww", "-o", "pid=,ppid=,uid=,command="]).split("\n")) {
    const found = /^\s*(\d+)\s+(\d+)\s+(\d+)\s(.*)$/.exec(line);
    if (found)
      all.set(Number(found[1]), {
        ppid: Number(found[2]),
        uid: Number(found[3]),
        command: found[4].trim(),
      });
  }
  const excluded = new Set([0, 1]);
  for (let pid = process.pid; pid > 1 && !excluded.has(pid); pid = all.get(pid)?.ppid ?? 0)
    excluded.add(pid);
  const table = new Map(
    [...all].filter(([pid, entry]) => entry.uid === uid && !excluded.has(pid)),
  );
  const found = new Map();
  const add = (pid, reason) => {
    const entry = table.get(pid);
    if (!entry) return;
    const hit = found.get(pid) ?? { pid, reasons: [], command: entry.command.slice(0, 300) };
    if (!hit.reasons.includes(reason)) hit.reasons.push(reason);
    found.set(pid, hit);
  };
  for (const pid of environmentMatches(run, table, roots)) add(pid, "env");
  for (const [pid, entry] of table)
    if (underRoots(roots, entry.command.split(" ")[0])) add(pid, "argv");
  let pid = 0;
  for (const line of run("lsof", ["-nP", "-w", "-u", String(uid), "-F", "pn"]).split("\n")) {
    if (line[0] === "p") pid = Number(line.slice(1));
    else if (line[0] === "n" && underRoots(roots, line.slice(1))) add(pid, "file");
  }
  return [...found.values()].sort((a, b) => a.pid - b.pid);
}

const processAlive = (pid) => {
  try {
    process.kill(pid, 0);
    return true;
  } catch (error) {
    return error.code !== "ESRCH";
  }
};

// Stops every process verifierHomeProcesses finds: SIGTERM, then SIGKILL for
// survivors, rescanning for children that appear later. Throws if any
// remains, so the home is kept and no receipt stands.
export async function stopVerifierHomeProcesses(home, find = verifierHomeProcesses) {
  const stopped = new Map();
  const signal = (pids, kind) => {
    for (const pid of pids)
      try {
        process.kill(pid, kind);
      } catch (error) {
        if (error.code !== "ESRCH") throw error;
      }
  };
  const settle = async (pids) => {
    for (let i = 0; i < 40 && pids.some(processAlive); i++)
      await new Promise((resolveWait) => setTimeout(resolveWait, 50));
  };
  for (let round = 0; round < 3; round++) {
    const found = find(home);
    if (!found.length) return [...stopped.values()];
    for (const hit of found) stopped.set(hit.pid, hit);
    const pids = found.map((hit) => hit.pid);
    signal(pids, "SIGTERM");
    await settle(pids);
    const survivors = pids.filter(processAlive);
    signal(survivors, "SIGKILL");
    await settle(survivors);
  }
  const remaining = find(home);
  if (remaining.length)
    throw new Error(
      "processes survived SIGKILL: " + remaining.map((hit) => hit.pid).join(", "),
    );
  return [...stopped.values()];
}

// Checks that rewrite inputs later checks read (dist-static, the wasm fixtures
// and their prerequisite digests) run alone among non-Go checks and act as
// barriers: no later non-Go check starts until an earlier exclusive check has
// finished. Go checks read only the hub/ module, so they neither wait for nor
// block an exclusive check.
export const EXCLUSIVE_CHECKS = new Set([
  "00-static-build",
  "01-static-release-verify",
  "wasm-test-build",
]);
// Browser suites that share a resource the lock rules below cannot see. All
// members hold one lock, so no two of them overlap. Each entry names the
// shared resource that was found by running the suite in parallel.
export const SERIAL_SUITES = new Map([]);

// Receipt evidence for the Go rules below (wi_82ed4c6924930bad, #15093):
// go-race is the longest check (625 s alone; no other attempt exceeded 171 s)
// and its hub/internal/store package takes 570-573 s against go test's 600 s
// package timeout, so it timed out when four browser lanes ran beside it.
// Go checks therefore start first, go-race holds two job slots, and non-Go
// checks run at a lower CPU priority. go-vet took 3 s and only type-checks,
// so it runs outside the Go lane.
export const isGoCheck = (check) => check.cwd === "hub";
export const GO_LANE_EXEMPT = new Set(["go-vet"]);
export const CHECK_WEIGHTS = new Map([["go-race", 2]]);
export const NON_GO_NICE = 10;
export const checkWeight = (check, jobs) =>
  Math.min(jobs, CHECK_WEIGHTS.get(check.id) ?? 1);

// Locks a check holds while it runs; two checks sharing any lock never
// overlap. Fixed ports come from the approved matrix, identical argv+cwd
// covers engine splits of one script (same screenshots and build output), and
// the heavy Go checks share one lane because each can saturate the CPU.
export function executionLocks(check) {
  const locks = [];
  for (const port of (check.environment?.VERIFICATION_REQUIRED_PORTS || "")
    .split(",")
    .filter(Boolean))
    locks.push("port:" + Number(port));
  locks.push("argv:" + canonical({ argv: check.argv, cwd: check.cwd }));
  if (isGoCheck(check) && !GO_LANE_EXEMPT.has(check.id)) locks.push("go");
  if (check.argv.some((arg) => SERIAL_SUITES.has(arg)))
    locks.push("serial-suites");
  return locks;
}

export function defaultJobs(
  cpus = availableParallelism(),
  memory = totalmem(),
) {
  return Math.max(
    1,
    Math.min(8, Math.floor(cpus / 2), Math.floor(memory / (3 * 1024 ** 3))),
  );
}

function validJobs(jobs) {
  if (!Number.isSafeInteger(jobs) || jobs < 1 || jobs > 16)
    throw new Error("Invalid --jobs value; use an integer from 1 to 16");
  return jobs;
}

// Greedy list scheduling. With more than one job, Go checks are offered first
// and the rest follow in plan order; with one job every check runs in plan
// order. A check starts when its weight fits the free job slots, it shares no
// held lock, and (for a non-Go check) no exclusive check is running or earlier
// in the plan and unfinished; an exclusive check also waits for every earlier
// non-Go check. Results keep plan positions, so a receipt lists checks exactly
// as the plan does. On the first error, or when the caller aborts, nothing new
// starts, every running check is stopped through the shared signal, and the
// error is thrown once all have settled.
export async function runScheduled(checks, options, runOne) {
  const jobs = validJobs(options.jobs);
  const outer = options.abortSignal;
  const stop = new AbortController();
  const forward = () => stop.abort(outer.reason);
  if (outer?.aborted) forward();
  else outer?.addEventListener("abort", forward, { once: true });
  const results = new Array(checks.length);
  const locks = checks.map(executionLocks);
  const weights = checks.map((check) => checkWeight(check, jobs));
  const go = checks.map(isGoCheck);
  const exclusive = checks.map((check) => EXCLUSIVE_CHECKS.has(check.id));
  const order = checks.map((_, i) => i);
  if (jobs > 1) order.sort((a, b) => go[b] - go[a] || a - b);
  const state = checks.map(() => "pending");
  const held = new Set();
  let load = 0,
    nonGoRunning = 0,
    exclusiveRunning = false,
    failure;
  const eligible = (i) => {
    if (load + weights[i] > jobs || locks[i].some((lock) => held.has(lock)))
      return false;
    if (go[i]) return true;
    if (exclusiveRunning) return false;
    for (let k = 0; k < i; k++)
      if (!go[k] && state[k] !== "done" && (exclusive[k] || exclusive[i]))
        return false;
    return !exclusive[i] || nonGoRunning === 0;
  };
  try {
    await new Promise((resolveAll) => {
      const launch = () => {
        if (!failure && !stop.signal.aborted)
          for (const i of order) {
            if (state[i] !== "pending" || !eligible(i)) continue;
            state[i] = "running";
            load += weights[i];
            if (!go[i]) nonGoRunning++;
            if (exclusive[i]) exclusiveRunning = true;
            for (const lock of locks[i]) held.add(lock);
            Promise.resolve()
              .then(() => runOne(checks[i], i, stop.signal))
              .then(
                (result) => {
                  results[i] = result;
                },
                (error) => {
                  failure ??= error;
                  if (!stop.signal.aborted) stop.abort("SIGTERM");
                },
              )
              .finally(() => {
                state[i] = "done";
                load -= weights[i];
                if (!go[i]) nonGoRunning--;
                if (exclusive[i]) exclusiveRunning = false;
                for (const lock of locks[i]) held.delete(lock);
                launch();
              });
          }
        if (load === 0) resolveAll();
      };
      launch();
    });
  } finally {
    outer?.removeEventListener("abort", forward);
  }
  if (outer?.aborted)
    throw new Error("Verification interrupted by " + outer.reason);
  if (failure) throw failure;
  return results;
}

// Scans receipt checks (plan order, with recorded attempt times) for runs
// that broke the scheduling rules: an overlap of two checks sharing a lock, a
// non-Go overlap with an exclusive check, or a later non-Go check starting
// before an earlier exclusive check ended. The verifier runs this over a real receipt.
export function overlapViolations(checks) {
  const spans = checks.map((check) => {
    const attempts = check.attempts?.length ? check.attempts : [check];
    return {
      id: check.id,
      locks: executionLocks(check),
      go: isGoCheck(check),
      exclusive: EXCLUSIVE_CHECKS.has(check.id),
      start: Date.parse(attempts[0].startedAt),
      end: Date.parse(attempts.at(-1).endedAt),
    };
  });
  const violations = [];
  for (let i = 0; i < spans.length; i++)
    for (let j = i + 1; j < spans.length; j++) {
      const a = spans[i],
        b = spans[j],
        overlap = a.start < b.end && b.start < a.end,
        shared = a.locks.filter((lock) => b.locks.includes(lock)),
        exempt = a.go || b.go;
      const reason =
        !exempt && a.exclusive && b.start < a.end
          ? "barrier"
          : !exempt && b.exclusive && a.end > b.start
            ? "exclusive"
            : overlap && shared.length
              ? shared.join(" ")
              : "";
      if (reason) violations.push({ checks: [a.id, b.id], reason });
    }
  return violations;
}

async function runCheckWithAttempts(check, plan, cwd, output, environment, options) {
  const { abortSignal, minFreeBytes, getAvailableBytes, onGroup } = options;
  const attempts = [];
  for (let attempt = 1; attempt <= (plan.maxAttempts || 1); attempt++) {
    if (abortSignal?.aborted)
      throw new Error("Verification interrupted by " + abortSignal.reason);
    requireFreeSpace(minFreeBytes, getAvailableBytes);
    const startedAt = new Date().toISOString(),
      start = performance.now();
    const run = await runCheck(
      check,
      resolve(cwd, check.cwd),
      environment,
      abortSignal,
      onGroup,
    );
    const log =
      (run.stdout || "") +
      (run.stderr || "") +
      (run.failureReason
        ? "\nverification failureReason: " + run.failureReason + "\n"
        : "") +
      (run.signal ? "verification signal: " + run.signal + "\n" : "");
    const logURI = join(
      output,
      digest(check.id) +
        (plan.maxAttempts ? ".attempt-" + attempt : "") +
        ".log",
    );
    writeFileSync(logURI, log, { mode: 0o600 });
    attempts.push({
      attempt,
      startedAt,
      endedAt: new Date().toISOString(),
      durationMs: Math.round(performance.now() - start),
      exitCode: run.status ?? -1,
      ...(run.failureReason ? { failureReason: run.failureReason } : {}),
      logURI,
      logDigest: digest(log),
    });
    if (abortSignal?.aborted)
      throw new Error("Verification interrupted by " + abortSignal.reason);
    if (run.status === 0) break;
  }
  const { attempt, ...final } = attempts.at(-1);
  const knownFailure = plan.knownFailures?.some(
    (e) => e.checkId === check.id,
  );
  return {
    ...check,
    ...final,
    ...(plan.maxAttempts
      ? {
          attempts,
          status:
            final.exitCode === 0
              ? attempts.length > 1
                ? "flaky"
                : "pass"
              : "fail",
          ...(knownFailure
            ? {
                knownFailure: true,
                ...(final.exitCode === 0 ? { nowPassing: true } : {}),
              }
            : {}),
        }
      : {}),
  };
}

function validRunOptions({ keepHome = false, jobs = defaultJobs() }) {
  if (typeof keepHome !== "boolean")
    throw new Error("Invalid --keep-home value");
  validJobs(jobs);
}

export async function runPlan(plan, cwd, output, options = {}) {
  validRunOptions(options);
  checkClean(cwd, plan.commit);
  // The plan's own checks are read as accepted checks, so a plan that kept an
  // accepted go-race re-derives to itself. A check edited any other way is
  // refused by that rule, and one dropped or narrowed by the comparison below.
  // The selection inputs are not plan fields; options.selection carries the
  // ones its record names, and the same rule decides whether they apply.
  const { selectionBaseCommit, selectionPaths } = options.selection || {};
  let expected;
  try {
    expected = makePlan(
      options.selection ? { ...plan, selectionBaseCommit, selectionPaths } : plan,
      cwd,
    );
  } catch (error) {
    if (!error.acceptedChecks) throw error;
    throw new Error("Altered or omitted required checks: " + error.message);
  }
  if (
    expected.matrixDigest !== plan.matrixDigest ||
    expected.checksDigest !== plan.checksDigest ||
    canonical(expected.checks) !== canonical(plan.checks) ||
    canonical(
      matrixPolicy(
        JSON.parse(readFileSync(join(cwd, "verification/matrix.json"), "utf8")),
        expected.checks,
      ),
    ) !==
      canonical({
        ...(plan.maxAttempts ? { maxAttempts: plan.maxAttempts } : {}),
        ...(plan.knownFailures?.length
          ? { knownFailures: plan.knownFailures }
          : {}),
      })
  )
    throw new Error("Altered or omitted required checks");
  return executePlan(plan, cwd, output, options, "receipt.json", (run) => ({
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
    ...run,
    aiv: { state: "unsubmitted" },
  }));
}

// What a run exercised, recorded with its host wait and load so later
// concurrency decisions can compare like with like.
export function checkSet(checks) {
  const go = checks.some(isGoCheck),
    browser = checks.some(isBrowserCheck);
  if (checks.length && checks.every(isGoCheck)) return "go-only";
  return go && browser ? "full" : browser ? "browser" : "unit";
}
// The run stops itself after every check's timeout times its attempts, plus
// the test-binary build allowance: the bound the deployer puts on its own
// in-release run (matrixRunTimeout in scripts/release-runner.mjs).
export const RUN_BUILD_ALLOWANCE_MS = 1800000;
export function planRunTimeout(plan) {
  return plan.checks.reduce(
    (total, check) =>
      total + Number(check.environment.VERIFICATION_TIMEOUT_MS) * (plan.maxAttempts || 1),
    RUN_BUILD_ALLOWANCE_MS,
  );
}

// Holds the host lock around one run: waits its turn on the ordered waitlist,
// re-checks the candidate, runs, and releases after the home is cleaned up.
// options.hostLock carries the priority, wait bound and test overrides.
async function executePlan(plan, cwd, output, options, receiptName, makeReceipt) {
  if (!isAbsolute(output) || relative(cwd, output).split("/")[0] !== "..")
    throw new Error("Logs/receipt must be outside worktree");
  mkdirSync(output, { recursive: true });
  const { minFreeBytes = DEFAULT_MIN_FREE_BYTES } = options;
  requireFreeSpace(minFreeBytes, options.getAvailableBytes);
  readPrerequisites(plan.checks, cwd, options.readPrerequisiteFile);
  const hostLock = options.hostLock || {};
  const lease = await (options.acquireHostLock || acquireHostLock)({
    runTimeoutMs: planRunTimeout(plan),
    ...hostLock,
    kind: plan.targeted ? "targeted" : "run",
    item: plan.itemId || hostLock.item || "unknown",
    agent: process.env.TAILTERM_AGENT_NAME || plan.verifierAgentId || "unknown",
    commit: plan.commit,
    output,
    worktree: realpathSync(cwd),
    recordDirectory: output,
    signal: options.abortSignal,
  });
  try {
    checkClean(cwd, plan.commit);
    return await executeHeldPlan(
      plan,
      cwd,
      output,
      {
        ...options,
        abortSignal: options.abortSignal
          ? AbortSignal.any([options.abortSignal, lease.signal])
          : lease.signal,
        onGroup: (pgid, running) =>
          running ? lease.addGroup(pgid) : lease.removeGroup(pgid),
      },
      receiptName,
      // Host keys go only in the receipt's copy; checks never see them.
      (run) =>
        makeReceipt({
          ...run,
          environment: {
            ...run.environment,
            ...receiptKeys(lease.finish({ checkSet: checkSet(plan.checks) })),
          },
        }),
    );
  } finally {
    await lease.release({ checkSet: checkSet(plan.checks) });
  }
}

// Runs a validated plan in a fresh verifier home and writes receiptName only
// when every check ran to completion on the unchanged clean candidate.
async function executeHeldPlan(plan, cwd, output, options, receiptName, makeReceipt) {
  const {
    keepHome = false,
    minFreeBytes = DEFAULT_MIN_FREE_BYTES,
    getAvailableBytes = availableBytes,
    abortSignal,
    removeHome = removeVerifierHome,
    stopHomeProcesses = stopVerifierHomeProcesses,
    createHome = () => mkdtempSync(join(realpathSync("/tmp"), "tv-")),
    jobs = defaultJobs(),
    onGroup,
  } = options;
  requireFreeSpace(minFreeBytes, getAvailableBytes);
  const prerequisites = readPrerequisites(plan.checks, cwd, options.readPrerequisiteFile);
  // Keep the unique private home independent of arbitrarily deep inherited
  // TMPDIR. createHome is injectable like the cleanup hooks for failure tests.
  const home = createHome();
  const receiptPath = join(output, receiptName);
  let receiptWritten = false;
  try {
    mkdirSync(output, { recursive: true });
    // macOS sun_path has 104 bytes including NUL. Budget the complete canonical
    // path for the longest participating name (menu prefix19 + UUID36), plus
    // the maximum unsigned32 UID. Never fall back to another run's namespace.
    const longestSocket = join(realpathSync(home), "tmux", "tmux-4294967295",
      "tailterm-menu-test-" + "0".repeat(36));
    const socketBytes = Buffer.byteLength(longestSocket);
    if (socketBytes > 103)
      throw new Error(`Private verifier tmux socket budget exceeds 103 bytes: ${socketBytes}`);
    const environment = {
      PATH: installCodexStub(home, output) + ":" + process.env.PATH,
      HOME: home,
      TMPDIR: home,
      TMUX_TMPDIR: join(home, "tmux"),
      GOPATH: join(home, "go"),
      GOMODCACHE: join(home, "go", "pkg", "mod"),
      GOCACHE: join(home, "go-build"),
      VERIFICATION_KEEP_HOME: keepHome ? "1" : "0",
      LANG: "en_US.UTF-8",
      CI: "1",
      GOTOOLCHAIN: "auto",
      VERIFICATION_JOBS: String(jobs),
    };
    mkdirSync(environment.TMUX_TMPDIR, { recursive: true, mode: 0o700 });
    // No inherited task/hub credentials, runtime config, vault or tmux socket.
    if (plan.checks.some(isBrowserCheck)) {
      environment.PLAYWRIGHT_BROWSERS_PATH =
        process.env.PLAYWRIGHT_BROWSERS_PATH ||
        join(process.env.HOME, "Library/Caches/ms-playwright");
    }
    if (abortSignal?.aborted)
      throw new Error("Verification interrupted by " + abortSignal.reason);
    const needsBinaries = plan.checks.some((check) =>
      [...binarySuites].some((suite) => check.id.startsWith(`tests/${suite}-browser.mjs`)));
    if (needsBinaries) {
      const binaries = await buildMatrixBinaries(cwd, output);
      if (abortSignal?.aborted)
        throw new Error("Verification interrupted by " + abortSignal.reason);
      if (plan.checks.some((check) => check.id.startsWith("tests/board-audit-old-hub-browser.mjs")))
        binaries.push(await buildHistoricalHub(cwd, join(output, "tailterm-historical-hub-test"), historicalHubCommit));
      if (abortSignal?.aborted)
        throw new Error("Verification interrupted by " + abortSignal.reason);
      const manifest = join(output, "test-binaries.json");
      writeFileSync(manifest, JSON.stringify({ version: 1, binaries }, null, 2) + "\n", { mode: 0o600 });
      writeFileSync(join(output, "test-binary-builds.json"), JSON.stringify(binaries.map(({ target, historicalCommit, source, buildLog, startedAt, endedAt, path, sha256 }) => ({ target, historicalCommit, source, buildLog, startedAt, endedAt, path, sha256 })), null, 2) + "\n", { mode: 0o600 });
      environment.TAILTERM_TEST_BINARIES = manifest;
      for (const binary of binaries)
        prerequisites.push({ path: binary.path, sha256: binary.sha256,
          source: binary.source, target: binary.target,
          historicalCommit: binary.historicalCommit,
          buildCommand: binary.buildLog.argv,
          startedAt: binary.startedAt, endedAt: binary.endedAt });
    }
    const results = await runScheduled(
      plan.checks,
      { jobs, abortSignal },
      (check, _index, signal) =>
        runCheckWithAttempts(check, plan, cwd, output, environment, {
          abortSignal: signal,
          minFreeBytes,
          getAvailableBytes,
          onGroup,
        }),
    );
    if (abortSignal?.aborted)
      throw new Error("Verification interrupted by " + abortSignal.reason);
    if (needsBinaries) {
      const manifest = JSON.parse(readFileSync(environment.TAILTERM_TEST_BINARIES, "utf8"));
      for (const binary of manifest.binaries)
        if ((await fileHash(binary.path)) !== binary.sha256)
          throw new Error("Prepared test binary changed during verification: " + binary.path);
    }
    if (abortSignal?.aborted)
      throw new Error("Verification interrupted by " + abortSignal.reason);
    checkClean(cwd, plan.commit);
    const receipt = makeReceipt({ environment, prerequisites, checks: results });
    writeFileSync(receiptPath, JSON.stringify(receipt, null, 2) + "\n", {
      mode: 0o600,
    });
    receiptWritten = true;
    return receipt;
  } finally {
    const cleanupFailed = (what, error) => {
      if (receiptWritten) rmSync(receiptPath, { force: true });
      try {
        writeFileSync(
          join(output, "cleanup-error.json"),
          JSON.stringify({ home, error: error.message }) + "\n",
          { mode: 0o600 },
        );
      } catch {}
      return new Error(`Failed to ${what} verifier home ${home}: ${error.message}`);
    };
    // Kept homes keep their files, not their processes. A process left under
    // the home (a setsid'd daemon a check started) would otherwise outlive the
    // run and pin the removed files it holds open.
    let stopped;
    try {
      stopped = await stopHomeProcesses(home);
    } catch (error) {
      throw cleanupFailed("stop processes under", error);
    }
    if (stopped.length)
      try {
        writeFileSync(
          join(output, "home-processes.json"),
          JSON.stringify({ home, processes: stopped }, null, 2) + "\n",
          { mode: 0o600 },
        );
      } catch {}
    if (!keepHome) {
      try {
        removeHome(home);
      } catch (error) {
        throw cleanupFailed("remove", error);
      }
    }
  }
}
// Targeted mode checks one fix between two frozen candidates: the matrix
// rules select checks from the paths the fix changed, nothing else. Its
// receipt is advisory iteration evidence, never a gating receipt: it is named
// targeted-receipt.json, marked targeted, and has no plan binding to import.
export function makeTargetedPlan(context, cwd) {
  if (
    !/^[a-f0-9]{40}$/.test(context.commit) ||
    !/^[a-f0-9]{40}$/.test(context.baseCommit)
  )
    throw new Error("Exact previous and fix candidate SHAs required");
  if (context.baseCommit === context.commit)
    throw new Error("Targeted run needs a fix that differs from its previous candidate");
  const raw = readFileSync(join(cwd, "verification/matrix.json"), "utf8"),
    matrix = JSON.parse(raw);
  assertInventory(matrix, cwd);
  assertFastForward(cwd, context.baseCommit, context.commit);
  const changed = diffPaths(cwd, context.baseCommit, context.commit);
  const checks = selectChecks(
    matrix,
    [],
    changed,
    candidateGoPackages(cwd, context.commit),
    { targeted: true },
  );
  for (const check of checks)
    if (check.argv[0] === "go")
      check.environment.VERIFICATION_BASE_COMMIT = context.baseCommit;
  return {
    targeted: true,
    version: 1,
    ...(context.repository ? { repository: context.repository } : {}),
    baseCommit: context.baseCommit,
    commit: context.commit,
    ...matrixPolicy(matrix, checks),
    matrixDigest: digest(raw),
    checksDigest: digest(checks),
    changed,
    checks,
  };
}

export async function runTargeted(context, cwd, output, options = {}) {
  validRunOptions(options);
  checkClean(cwd, context.commit);
  const plan = makeTargetedPlan(context, cwd);
  return executePlan(
    plan,
    cwd,
    output,
    options,
    "targeted-receipt.json",
    (run) => ({
      targeted: true,
      worktree: resolve(cwd),
      version: 1,
      planDigest: digest(plan),
      ...(plan.repository ? { repository: plan.repository } : {}),
      baseCommit: plan.baseCommit,
      commit: plan.commit,
      matrixDigest: plan.matrixDigest,
      checksDigest: plan.checksDigest,
      changed: plan.changed,
      detached: true,
      cleanBefore: true,
      cleanAfter: true,
      ...run,
    }),
  );
}

if (
  process.argv[1] &&
  resolve(process.argv[1]) === fileURLToPath(import.meta.url)
) {
  const interruption = new AbortController();
  let interruptedBy = "";
  for (const signal of ["SIGINT", "SIGTERM"])
    process.on(signal, () => {
      interruptedBy ||= signal;
      interruption.abort(interruptedBy);
      process.exitCode = signal === "SIGINT" ? 130 : 143;
    });
  try {
    const [mode, file, output, ...flags] = process.argv.slice(2),
      input = JSON.parse(readFileSync(file, "utf8"));
    if (mode === "plan") {
      const { plan, preserved, selection } = planWithPreservation(
        input,
        process.cwd(),
      );
      writeFileSync(output, JSON.stringify(plan, null, 2) + "\n");
      rmSync(preservationPath(output), { force: true });
      // Only a context that named a selection base reports its rule.
      const stated = input.selectionBaseCommit !== undefined;
      if (stated)
        console.error(
          selection.rule === "integration-tip"
            ? `Selected checks from the integration tip ${selection.selectionBaseCommit}`
            : `Selected checks from the job base ${selection.baseCommit} (${selection.reason})`,
        );
      // Run mode re-derives the plan from this record's selection inputs.
      const record = stated
        ? {
            ...(preserved || { version: 1 }),
            selection:
              selection.rule === "integration-tip"
                ? { ...selection, selectionPaths: input.selectionPaths }
                : selection,
          }
        : preserved;
      if (record)
        writeFileSync(
          preservationPath(output),
          JSON.stringify(record, null, 2) + "\n",
        );
      if (preserved) {
        console.error(
          `Kept ${preserved.kept.length + preserved.widened.length} accepted checks (accepted checksDigest ${preserved.acceptedChecksDigest})` +
            (preserved.rebuilt.length
              ? `; rebuilt under the newer approved matrix: ${preserved.rebuilt.join(", ")}`
              : "") +
            (preserved.narrowerSelection.length
              ? "; this commit alone selected less for: " +
                preserved.narrowerSelection.join(", ")
              : ""),
        );
      }
    } else if (mode === "run" || mode === "targeted") {
      let keepHome = false;
      let minFreeBytes = DEFAULT_MIN_FREE_BYTES;
      let jobs = defaultJobs();
      let priority, item;
      let maxWaitMs = DEFAULT_HOST_WAIT_MS;
      for (let i = 0; i < flags.length; i++) {
        if (flags[i] === "--keep-home") keepHome = true;
        else if (flags[i] === "--priority") priority = flags[++i] ?? "";
        else if (flags[i] === "--item") {
          item = flags[++i];
          if (!item) throw new Error("--item requires an item ID");
        } else if (flags[i] === "--host-wait-minutes")
          maxWaitMs = minutesFlag("--host-wait-minutes", flags[++i], 1440);
        else if (flags[i] === "--jobs") {
          const value = flags[++i];
          if (!/^[1-9][0-9]?$/.test(value || ""))
            throw new Error("--jobs requires an integer from 1 to 16");
          jobs = validJobs(Number(value));
        }
        else if (flags[i] === "--min-free-bytes") {
          const value = flags[++i];
          if (!/^(0|[1-9][0-9]*)$/.test(value || ""))
            throw new Error(
              "--min-free-bytes requires a nonnegative integer byte count",
            );
          minFreeBytes = Number(value);
          if (!Number.isSafeInteger(minFreeBytes))
            throw new Error(
              "--min-free-bytes requires a safe integer byte count",
            );
        } else throw new Error("Unknown verifier run option: " + flags[i]);
      }
      const selection = mode === "run" ? recordedSelection(file) : undefined;
      const r = await (mode === "run" ? runPlan : runTargeted)(
        input,
        process.cwd(),
        resolve(output),
        {
          ...(selection ? { selection } : {}),
          keepHome,
          minFreeBytes,
          jobs,
          abortSignal: interruption.signal,
          hostLock: {
            ...resolveRunPriority(priority, input?.itemId || item),
            maxWaitMs,
            item,
            print: (line) => console.log(line),
          },
        },
      );
      await new Promise((resolve) => setImmediate(resolve));
      if (interruptedBy)
        rmSync(
          join(
            resolve(output),
            mode === "run" ? "receipt.json" : "targeted-receipt.json",
          ),
          { force: true },
        );
      process.exitCode = interruptedBy
        ? interruptedBy === "SIGINT"
          ? 130
          : 143
        : receiptEligible(r)
          ? 0
          : 1;
    } else
      throw new Error(
        "Usage: node scripts/verify-matrix.mjs plan|run|targeted INPUT OUTPUT [--keep-home] [--min-free-bytes N] [--jobs N] [--priority urgent|high|normal] [--host-wait-minutes N] [--item ID]",
      );
  } catch (e) {
    console.error(e.message);
    process.exitCode =
      interruptedBy === "SIGINT" ? 130 : interruptedBy === "SIGTERM" ? 143 : (e.exitCode ?? 1);
  }
}
