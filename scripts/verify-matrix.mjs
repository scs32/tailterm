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
export function selectChecks(matrix, owned, changed, candidatePackages) {
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
  const add = (id, argv, cwd = ".", environment = {}) => {
    const timeout =
      matrix.checkTimeoutMs?.[id] ?? matrix.defaultTimeoutMs ?? 600000;
    if (!Number.isSafeInteger(timeout) || timeout <= 0 || timeout > 1800000)
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
    add("go-test", ["go", "test", "./..."], "hub");
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
  return receipt.checks.every(
    (c) => c.exitCode === 0 || c.knownFailure === true,
  );
}
export function makePlan(context, cwd) {
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
  const changed = diffPaths(cwd, context.baseCommit, context.commit);
  const candidatePackages = new Set(
    execFileSync(
      "git",
      ["ls-tree", "-r", "--name-only", context.commit, "hub"],
      { cwd, encoding: "utf8" },
    )
      .split("\n")
      .filter((p) => p.endsWith(".go"))
      .map((p) => "./" + p.slice(4, p.lastIndexOf("/"))),
  );
  const checks = selectChecks(
    matrix,
    context.owned,
    changed,
    candidatePackages,
  );
  for (const check of checks)
    if (check.argv[0] === "go")
      check.environment.VERIFICATION_BASE_COMMIT = context.baseCommit;
  const { maxAttempts, knownFailures, ...inputContext } = context;
  return {
    ...inputContext,
    version: 1,
    ...matrixPolicy(matrix, checks),
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
async function checkController() {
  const { spawn } = await import("node:child_process");
  const argv = JSON.parse(process.argv[1]),
    timeout = Number(process.argv[2]);
  const child = spawn(argv[0], argv.slice(1), {
    detached: true,
    stdio: ["ignore", "pipe", "pipe"],
  });
  let stdout = [],
    stderr = [],
    size = 0,
    reason = "",
    closed = false,
    forced = false,
    code = -1,
    signal = "",
    timer,
    killTimer;
  const signalGroup = (kind) => {
    if (!child.pid) return;
    try {
      process.kill(-child.pid, kind);
    } catch (error) {
      if (error.code !== "ESRCH")
        stderr.push(Buffer.from("\nGroup signal: " + error.message));
    }
  };
  let finished = false;
  const finish = () => {
    if (finished || !closed || (reason && !forced)) return;
    finished = true;
    clearTimeout(timer);
    clearTimeout(killTimer);
    process.stdout.write(
      JSON.stringify({
        status:
          reason === "timeout" ? 124 : reason === "output-limit" ? 125 : code,
        stdout: Buffer.concat(stdout).toString(),
        stderr: Buffer.concat(stderr).toString(),
        failureReason: reason || (code !== 0 ? "exit" : ""),
        signal,
      }),
    );
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
  const capture = (dest) => (data) => {
    size += data.length;
    if (size > 128 * 1024 * 1024) {
      stop("output-limit");
      return;
    }
    dest.push(data);
  };
  child.stdout.on("data", capture(stdout));
  child.stderr.on("data", capture(stderr));
  child.on("error", (error) => {
    stderr.push(Buffer.from(error.message));
    reason = "spawn";
    forced = true;
    closed = true;
    finish();
  });
  child.on("close", (status, whichSignal) => {
    code = status ?? -1;
    signal = whichSignal || "";
    closed = true;
    finish();
  });
  timer = setTimeout(() => stop("timeout"), timeout);
}
export function runCheck(check, cwd, environment) {
  const timeout = Number(check.environment.VERIFICATION_TIMEOUT_MS);
  if (!Number.isSafeInteger(timeout) || timeout <= 0 || timeout > 1800000)
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
  const raw = execFileSync(
    process.execPath,
    [
      "--input-type=module",
      "-e",
      "(" + checkController.toString() + ")()",
      JSON.stringify(check.argv),
      String(timeout),
    ],
    {
      cwd,
      env: { ...environment, ...check.environment },
      encoding: "utf8",
      maxBuffer: 512 * 1024 * 1024,
    },
  );
  return JSON.parse(raw);
}
const DEFAULT_MIN_FREE_BYTES = 2 * 1024 * 1024 * 1024;

export function availableBytes(path = tmpdir()) {
  const { bavail, bsize } = statfsSync(path);
  return bavail * bsize;
}

export function requireFreeSpace(reserve, getAvailableBytes = availableBytes) {
  if (!Number.isSafeInteger(reserve) || reserve < 0)
    throw new Error("Invalid --min-free-bytes reserve");
  const free = getAvailableBytes();
  if (!Number.isSafeInteger(free) || free < 0)
    throw new Error("Unable to determine free space for verifier home");
  if (free < reserve)
    throw new Error(
      `Insufficient free space for verifier home: ${free} bytes available, ${reserve} bytes required`,
    );
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

export function runPlan(plan, cwd, output, options = {}) {
  const {
    keepHome = false,
    minFreeBytes = DEFAULT_MIN_FREE_BYTES,
    getAvailableBytes = availableBytes,
  } = options;
  if (typeof keepHome !== "boolean")
    throw new Error("Invalid --keep-home value");
  checkClean(cwd, plan.commit);
  const expected = makePlan(plan, cwd);
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
  if (!isAbsolute(output) || relative(cwd, output).split("/")[0] !== "..")
    throw new Error("Logs/receipt must be outside worktree");
  requireFreeSpace(minFreeBytes, getAvailableBytes);
  const home = mkdtempSync(join(tmpdir(), "tailterm-verifier-"));
  try {
    mkdirSync(output, { recursive: true });
    const environment = {
      PATH: process.env.PATH,
      HOME: home,
      TMPDIR: home,
      GOPATH: join(home, "go"),
      GOMODCACHE: join(home, "go", "pkg", "mod"),
      GOCACHE: join(home, "go-build"),
      VERIFICATION_KEEP_HOME: keepHome ? "1" : "0",
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
      const attempts = [];
      for (let attempt = 1; attempt <= (plan.maxAttempts || 1); attempt++) {
        requireFreeSpace(minFreeBytes, getAvailableBytes);
        const startedAt = new Date().toISOString(),
          start = performance.now();
        const run = runCheck(check, resolve(cwd, check.cwd), environment);
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
        if (run.status === 0) break;
      }
      const { attempt, ...final } = attempts.at(-1);
      const knownFailure = plan.knownFailures?.some(
        (e) => e.checkId === check.id,
      );
      results.push({
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
  } finally {
    if (!keepHome) removeVerifierHome(home);
  }
}
if (
  process.argv[1] &&
  resolve(process.argv[1]) === fileURLToPath(import.meta.url)
) {
  // Installing handlers before synchronous checks prevents a signal's default
  // termination from bypassing runPlan's finally block. The check completes (or
  // times out), its external evidence is saved, then the home is removed.
  let interruptedBy = "";
  for (const signal of ["SIGINT", "SIGTERM"])
    process.on(signal, () => {
      interruptedBy ||= signal;
      process.exitCode = signal === "SIGINT" ? 130 : 143;
    });
  try {
    const [mode, file, output, ...flags] = process.argv.slice(2),
      input = JSON.parse(readFileSync(file, "utf8"));
    if (mode === "plan")
      writeFileSync(
        output,
        JSON.stringify(makePlan(input, process.cwd()), null, 2) + "\n",
      );
    else if (mode === "run") {
      let keepHome = false;
      let minFreeBytes = DEFAULT_MIN_FREE_BYTES;
      for (let i = 0; i < flags.length; i++) {
        if (flags[i] === "--keep-home") keepHome = true;
        else if (flags[i] === "--min-free-bytes" && i + 1 < flags.length)
          minFreeBytes = Number(flags[++i]);
        else throw new Error("Unknown verifier run option: " + flags[i]);
      }
      const r = runPlan(input, process.cwd(), resolve(output), {
        keepHome,
        minFreeBytes,
      });
      // A signal received during spawnSync is dispatched when the event loop
      // runs again, after runPlan has written evidence and cleaned its home.
      await new Promise((resolve) => setImmediate(resolve));
      process.exitCode = interruptedBy
        ? interruptedBy === "SIGINT"
          ? 130
          : 143
        : receiptEligible(r)
          ? 0
          : 1;
    } else
      throw new Error(
        "Usage: node scripts/verify-matrix.mjs plan|run INPUT OUTPUT [--keep-home] [--min-free-bytes N]",
      );
  } catch (e) {
    console.error(e.message);
    process.exitCode = 1;
  }
}
