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
  openSync,
  closeSync,
  readSync,
  renameSync,
  readlinkSync,
  copyFileSync,
  constants,
} from "node:fs";
import { resolve, relative, join, isAbsolute, dirname, basename } from "node:path";
import {
  tmpdir,
  homedir,
  availableParallelism,
  totalmem,
  getPriority,
  setPriority,
} from "node:os";
import { createHash, randomBytes } from "node:crypto";
import { execFileSync, spawn, spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";
import { fileHash } from "../tests/test-binaries.mjs";
import {
  acquireHostLock,
  releaseRun,
  resolveRunPriority,
  resolvePriority,
  receiptKeys,
  minutesFlag,
  DEFAULT_HOST_WAIT_MS,
  lockPaths,
  takeMutex,
  releaseMutex,
} from "./verify-matrix-host-lock.mjs";

const binarySuites = new Set([
  "profile-sync", "task-form", "lead-replacement", "project-work-items",
  "board-compose", "item-message-compose", "project-queue", "board-decisions",
  "owner-obligations", "dropdown-continuity", "board-audit-old-hub",
  "interventions",
]);
const historicalHubCommit = "58f9185981625b148b30ad4b5070a1a658e17f77";

// One fixture for both runtimes: tests/verification.test.js and the hub's
// TestVerificationCanonicalDigestFixture pin the same digests. `special` holds
// the characters where JSON.stringify and Go's encoder disagree (U+2028,
// U+2029, astral-plane keys); `ordinary` holds none of them.
export const CANONICAL_DIGEST_FIXTURE = {
  special: {
    a: "line\u2028separator",
    "\uE000": "paragraph\u2029separator",
    "\uFF21": "both\u2028\u2029",
    "\u{10000}": "literal \\u2028 and \\u2029 text",
    "\u{1F600}": "astral \u{1F600} value",
    "\u{10FFFF}": ["\u2029", { "\u{1F600}": 1, "\uFF21": 2, "\uE000": 3 }],
    "x\u2028": { "\u{10000}": null, "\uE000": true },
    "x\u2029": [{ "\uFF21": "a", "\u{1F600}": "b" }, { "\u{10FFFF}": "c", z: "d" }],
  },
  ordinary: {
    "": "empty key",
    1: "one",
    10: "ten",
    9: "nine",
    A: "<tag> & 'quote' \"double\"",
    b: "\b\f\n\r\t\u0000\u001f\u007f",
    "\u03bb": "lambda \u03bb",
    "\uE000": "private use",
    "\uFFFD": "replacement \uFFFD",
    nested: { list: [0, -1, 1.5, null, true, false], empty: {}, none: [] },
    records: [{ id: "c1", argv: ["node", "fixture.js"] }, { id: "c2", environment: { HOME: "/tmp/isolated" } }],
  },
};

// Mirrors the hub's verificationDigest (Go encoding/json): keys in UTF-8 byte
// order and U+2028/U+2029 escaped. Input without astral keys or those two
// characters encodes exactly as JSON.stringify with sorted keys.
const canonicalString = (s) =>
  JSON.stringify(s).replace(/[\u2028\u2029]/g, (c) => (c === "\u2028" ? "\\u2028" : "\\u2029"));
const utf8Order = (a, b) => Buffer.compare(Buffer.from(a), Buffer.from(b));
export function canonical(value) {
  if (Array.isArray(value)) return "[" + value.map(canonical).join(",") + "]";
  if (value && typeof value === "object")
    return (
      "{" +
      Object.keys(value)
        .sort(utf8Order)
        .map((k) => canonicalString(k) + ":" + canonical(value[k]))
        .join(",") +
      "}"
    );
  return typeof value === "string" ? canonicalString(value) : JSON.stringify(value);
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
// A targeted run's base is the previous candidate, which a rebased fix no
// longer contains: its refusal names that relation and both commits instead of
// advising the rebase that caused it.
export function assertFastForward(cwd, base, commit, { targeted = false } = {}) {
  const ancestry = spawnSync("git", ["merge-base", "--is-ancestor", base, commit], {
    cwd,
    encoding: "utf8",
  });
  if (ancestry.status === 1 && targeted) {
    const common = spawnSync("git", ["merge-base", base, commit], { cwd, encoding: "utf8" });
    const mergeBase = common.status === 0 && /^[a-f0-9]{40}$/.test(common.stdout.trim())
      ? common.stdout.trim()
      : "none";
    throw new Error(
      `fix candidate ${commit} does not contain previous candidate ${base} (merge base ${mergeBase}). ` +
        "A targeted run needs the fix committed on top of the previous candidate; after a rebase run the full plan on the new candidate.",
    );
  }
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
  const goRaceShards = matrixGoRaceShards(matrix);
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
    // With approved shards every go-race has one argv prefix, whatever its
    // packages: one -shards flag per approved package, in package order. The
    // hub and the release runner cover an approved go-race by an integrated
    // one only when the argv before the first package is equal.
    add(
      "go-race",
      [
        ...(goRaceShards
          ? [
              "node",
              "../scripts/verify-matrix.mjs",
              "go-race",
              ...goTestFlags,
              ...goRaceShards.map((s) => `-shards=${s.package}=${s.k}`),
            ]
          : ["go", "test", "-race", ...goTestFlags]),
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
  // wasm-test-build rewrites wasm/tailserve.wasm and .build/test.wasm, which
  // 00-static-build packs and the browser suites load. It leads the plan, and
  // being exclusive, no other non-Go check starts until it ends.
  const rank = (c) => (c.id === "wasm-test-build" ? 0 : 1);
  return checks.sort((a, b) => rank(a) - rank(b) || a.id.localeCompare(b.id, "en"));
}
// The ./name.mjs modules a tests file imports, statically or dynamically.
const importedTestModules = (text) =>
  new Set(
    [...text.matchAll(/\b(?:from|import)\s*\(?\s*["']\.\/([\w.-]+\.mjs)["']/g)].map(
      (found) => found[1],
    ),
  );
// A browser suite is a tests module that calls .launch( itself, carries the
// suite marker, or imports a module carrying the helper marker (one import
// level). Each marker is a whole line. A helper that another tests module
// imports is not itself a suite; one that nothing imports is, so the marker
// cannot hide an undeclared file.
const BROWSER_SUITE_MARKER = /^\/\/ verify-matrix: browser-suite$/m;
const BROWSER_HELPER_MARKER = /^\/\/ verify-matrix: browser-helper$/m;
export function assertInventory(matrix, cwd) {
  const texts = new Map(
    readdirSync(join(cwd, "tests"))
      .filter((f) => f.endsWith(".mjs"))
      .map((f) => [f, readFileSync(join(cwd, "tests", f), "utf8")]),
  );
  const marked = new Set(
    [...texts].filter(([, text]) => BROWSER_HELPER_MARKER.test(text)).map(([f]) => f),
  );
  const imported = new Set(
    [...texts].flatMap(([f, text]) => [...importedTestModules(text)].filter((name) => name !== f)),
  );
  const helpers = new Set([...marked].filter((f) => imported.has(f)));
  const inventory = [...texts]
    .filter(
      ([f, text]) =>
        !helpers.has(f) &&
        (marked.has(f) ||
          text.includes(".launch(") ||
          BROWSER_SUITE_MARKER.test(text) ||
          [...importedTestModules(text)].some((name) => helpers.has(name))),
    )
    .map(([f]) => "tests/" + f)
    .sort();
  const declared = [
    ...matrix.browserSuites.map((s) => s.file),
    ...matrix.excludedBrowserSuites,
  ].sort();
  if (canonical(inventory) !== canonical(declared))
    throw new Error("Browser inventory changed; update approved matrix");
}
// binarySuites decides whether a run builds the test binaries, so it must name
// exactly the declared suites that import the binary helper.
export function assertBinarySuites(matrix, cwd) {
  const declared = matrix.browserSuites.map((s) => s.file);
  const users = declared.filter((file) =>
    importedTestModules(readFileSync(join(cwd, file), "utf8")).has("test-binaries.mjs"),
  );
  const listed = declared.filter((file) => {
    const found = /^tests\/(.+)-browser\.mjs$/.exec(file);
    return found && binarySuites.has(found[1]);
  });
  const only = (from, other) => from.filter((file) => !other.includes(file)).sort();
  const unlisted = only(users, listed),
    unused = only(listed, users);
  if (unlisted.length || unused.length)
    throw new Error(
      "Binary suite list disagrees with the suites that import tests/test-binaries.mjs" +
        (unlisted.length ? `; helper users not listed: ${unlisted.join(", ")}` : "") +
        (unused.length ? `; listed without the helper: ${unused.join(", ")}` : ""),
    );
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
    // A failed build leaves the WASM readers on the previous run's files with
    // nothing to compare them to, so the build may never be excused.
    if (entry?.checkId === "wasm-test-build")
      throw new Error("wasm-test-build cannot be a known failure: browser readers would run on stale WASM");
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
// Race shards (wi_d6b6387726c1f846, several packages wi_af64945097c67c68).
// The matrix names one or more packages, each with its own K; a go-race that
// tests such a package runs it as K shards, in one pool with the job for its
// other packages. A test's shard is a pure function of its name, so a test
// keeps its shard when others are added or removed, and the partition follows
// from the commit and K alone. The last shard is the catch-all: it skips the
// other shards' names instead of listing its own, so a test nobody listed
// still runs once.
export const GO_RACE_PROCESS_BYTES = 1024 ** 3;
export const JOB_SLOT_BYTES = 3 * 1024 ** 3;
const GO_RACE_SHARDS_FLAG = /^-shards=(\.\/[\w./-]+)=([2-8])$/;
const GO_RACE_TIMEOUT_FLAG = /^-timeout=[1-9][0-9]*m$/;
const GO_RACE_USAGE =
  "node ../scripts/verify-matrix.mjs go-race [-timeout=Nm] [-shards=./PACKAGE=K]... ./PACKAGE... (from hub; K 2 to 8, one flag per package)";
// The approved shard packages, sorted by package, or null without the key.
function matrixGoRaceShards(matrix) {
  if (matrix.goRaceShards === undefined) return null;
  const entries =
    matrix.goRaceShards &&
    typeof matrix.goRaceShards === "object" &&
    !Array.isArray(matrix.goRaceShards)
      ? Object.entries(matrix.goRaceShards)
      : [];
  if (
    !entries.length ||
    entries.some(
      ([name, k]) =>
        !Number.isInteger(k) ||
        !GO_RACE_SHARDS_FLAG.test(`-shards=${name}=${k}`) ||
        name.slice(2).split("/").some((part) => !part || /^\.+$/.test(part)),
    )
  )
    throw new Error("Invalid matrix goRaceShards");
  return entries
    .map(([name, k]) => ({ package: name, k }))
    .sort((a, b) => (a.package < b.package ? -1 : a.package > b.package ? 1 : 0));
}
// The shards of a go-race check that will shard: the specs of its -shards
// flags whose package it tests, all of them for ./... . Null for every other
// check.
export function goRaceShardSpec(check) {
  if (check.id !== "go-race" || !Array.isArray(check.argv)) return null;
  const split = goRacePackages(check.argv);
  if (!split) return null;
  const all = split.packages.includes("./...");
  const specs = split.flags
    .map((flag) => GO_RACE_SHARDS_FLAG.exec(flag))
    .filter((found) => found && (all || split.packages.includes(found[1])))
    .map((found) => ({ package: found[1], k: Number(found[2]) }));
  return specs.length ? specs : null;
}
export const goRaceShardOf = (name, k) =>
  parseInt(createHash("sha256").update(name).digest("hex").slice(0, 8), 16) % k;
// The source rule: every top-level Test, Fuzz and Example function of the
// given _test.go texts except TestMain, in order, duplicates kept. It reads no
// build tags; the run compares it with go test -list and stops on a difference.
export function goRaceTestNames(sourceTexts) {
  const names = [];
  for (const text of sourceTexts)
    for (const [, name] of text.matchAll(
      /^func ((?:Test|Fuzz|Example)[A-Z_0-9]\w*|Test|Example)\(/gm,
    ))
      if (name !== "TestMain") names.push(name);
  return names;
}
// The K sorted name lists and their digest. A duplicate name throws.
export function goRacePartition(names, k) {
  if (!Number.isInteger(k) || k < 2 || k > 8)
    throw new Error("go-race shard count must be an integer from 2 to 8");
  const seen = new Set(),
    parts = Array.from({ length: k }, () => []);
  for (const name of names) {
    if (seen.has(name)) throw new Error("Duplicate Go test name: " + name);
    seen.add(name);
    parts[goRaceShardOf(name, k)].push(name);
  }
  for (const part of parts) part.sort();
  return { parts, digest: digest(parts) };
}
// counts maps a name to how often it occurs. Throws unless every wanted name
// occurs exactly once and nothing else occurs.
function assertExactlyOnce(names, counts, what) {
  const wanted = new Set(names);
  const list = (label, found) =>
    found.length ? [`${label}: ${found.sort().join(", ")}`] : [];
  const problems = [
    ...list("missing", [...wanted].filter((name) => !counts.has(name))),
    ...list(
      "more than once",
      [...counts].filter(([, count]) => count > 1).map(([name]) => name),
    ),
    ...list("unlisted", [...counts.keys()].filter((name) => !wanted.has(name))),
    ...(wanted.size !== names.length ? ["the test list repeats a name"] : []),
  ];
  if (problems.length) throw new Error(`${what}: ${problems.join("; ")}`);
}
export function assertGoRacePartition(names, parts) {
  const counts = new Map();
  for (const name of parts.flat()) counts.set(name, (counts.get(name) ?? 0) + 1);
  assertExactlyOnce(
    names,
    counts,
    "go-race shards do not hold every test exactly once",
  );
}
// The go test selector of each shard. Null means the shard is not started (no
// name hashes to it); the catch-all always starts.
export function goRaceShardSelectors(parts) {
  const pattern = (names) => "^(" + names.join("|") + ")$",
    last = parts.length - 1,
    listed = parts.slice(0, last).flat().sort();
  return parts.map((names, i) =>
    i === last
      ? listed.length
        ? ["-skip", pattern(listed)]
        : []
      : names.length
        ? ["-run", pattern(names)]
        : null,
  );
}
// How often each top-level test started, from go test -json lines.
export function goRaceRunCounts(jsonLines, counts = new Map()) {
  for (const line of jsonLines) {
    let event;
    try {
      event = JSON.parse(line);
    } catch {
      continue;
    }
    if (
      event?.Action === "run" &&
      typeof event.Test === "string" &&
      !event.Test.includes("/")
    )
      counts.set(event.Test, (counts.get(event.Test) ?? 0) + 1);
  }
  return counts;
}
export function assertRanExactlyOnce(names, counts) {
  assertExactlyOnce(
    names,
    counts,
    "go-race shards did not run every test exactly once",
  );
}
const goRaceShardsLine = (shards, tests, partitionDigest) =>
  `go-race shards: ${shards.package} K=${shards.k} tests=${tests} partition sha256:${partitionDigest}`;
// Plan-time check: when the plan's go-race will shard, every test of each
// shard package at the commit lands in exactly one of its shards. Returns one
// entry per package, with the line plan mode prints, skipping a package the
// commit does not have; null when nothing shards or none is at the commit.
function planGoRaceShards(checks, cwd, commit, partition) {
  const race = checks.find((check) => check.id === "go-race"),
    specs = race && goRaceShardSpec(race);
  if (!specs) return null;
  const read = (...args) =>
    execFileSync("git", args, {
      cwd,
      encoding: "utf8",
      maxBuffer: 256 * 1024 * 1024,
    });
  const planned = [];
  for (const shards of specs) {
    const files = read(
      "ls-tree",
      "-z",
      "--full-tree",
      commit,
      "hub/" + shards.package.slice(2) + "/",
    )
      .split("\0")
      .map((entry) => /^\d+ blob ([a-f0-9]+)\t(.*)$/.exec(entry))
      .filter((entry) => entry?.[2].endsWith(".go"));
    if (!files.length) continue;
    const names = goRaceTestNames(
      files
        .filter((entry) => entry[2].endsWith("_test.go"))
        .map((entry) => read("cat-file", "blob", entry[1])),
    );
    const { parts, digest: partitionDigest } = partition(names, shards.k);
    assertGoRacePartition(names, parts);
    planned.push({
      ...shards,
      tests: names.length,
      digest: partitionDigest,
      line: goRaceShardsLine(shards, names.length, partitionDigest),
    });
  }
  return planned.length ? planned : null;
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
// from the bound base to the commit that the published branch already holds:
// the runner integrates onto that branch's tip, and a candidate can then never
// name itself or any other unpublished commit to narrow its own selection.
// Anything else, a git failure included, selects from the bound base as
// before and never throws.
const PUBLISHED_REF = "refs/heads/tasks-hub";
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
  if (
    !Array.isArray(paths) ||
    paths.some(
      (p) =>
        typeof p !== "string" ||
        isAbsolute(p) ||
        // Git names no path with an empty or dot segment, so one that has
        // any is not a path this selection may be narrowed by. One trailing
        // slash is an owned directory prefix, as plans record them.
        p
          .replace(/\/$/, "")
          .split("/")
          .some((part) => part === "" || part === "." || part === "..") ||
        /[\0\n]/.test(p),
    )
  )
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
  if (!ok("rev-parse", "--verify", "--quiet", `${PUBLISHED_REF}^{commit}`))
    return jobBase("unreadable-published-ref");
  if (!ok("merge-base", "--is-ancestor", tip, PUBLISHED_REF))
    return jobBase("selection-base-unpublished");
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
export function planWithPreservation(
  context,
  cwd,
  { partition = goRacePartition } = {},
) {
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
  assertBinarySuites(matrix, cwd);
  assertFastForward(cwd, context.baseCommit, context.commit);
  const packages = candidateGoPackages(cwd, context.commit);
  let selection = selectionRule(context, cwd),
    changed,
    checks;
  if (selection.rule === "integration-tip") {
    // A tip diff that cannot be read, or a selection path the matrix has no
    // rule for, falls back like any other unusable selection input.
    let reason = "unreadable-selection-base";
    try {
      changed = [
        ...new Set([
          ...diffPaths(cwd, selection.selectionBaseCommit, context.commit),
          ...context.selectionPaths,
        ]),
      ].sort();
      reason = "invalid-selection-paths";
      checks = selectChecks(matrix, context.owned, changed, packages);
    } catch {
      selection = { rule: "job-base", baseCommit: context.baseCommit, reason };
    }
  }
  if (selection.rule === "job-base") {
    changed = diffPaths(cwd, context.baseCommit, context.commit);
    checks = selectChecks(matrix, context.owned, changed, packages);
  }
  for (const check of checks)
    if (isGoCheck(check))
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
  // Outside the plan like the records above: the partition follows from the
  // commit and K, and its digest changes with any item's added test.
  const goRaceShards = planGoRaceShards(checks, cwd, context.commit, partition);
  return { plan, preserved, selection, goRaceShards };
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

// The prerequisites a plan's checks need, in MATRIX_PREREQUISITES order.
function selectedPrerequisites(checks) {
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
  return [...paths];
}

// Readability is checked by reading the bytes, not just testing existence.
// Call again after admission: these hashes must describe the held run's inputs.
export function readPrerequisites(checks, cwd, read = readFileSync) {
  return selectedPrerequisites(checks).map((path) => {
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

// The files verify-matrix.mjs requires before a browser check runs, in its
// order (a drift test pins the list). All are gitignored, so provisioning
// them never dirties the deployer's checkout. release-runner.mjs re-exports
// these three names for tt deployment setup.
export const MATRIX_PREREQUISITES = ["node_modules/.package-lock.json", "wasm/tailserve.wasm", ".build/test.wasm", ".build/speech-fixture.wav", ".build/go-modules.txt"];
export const missingPrerequisites = (cwd) => MATRIX_PREREQUISITES.filter((p) => !existsSync(join(cwd, p)));
// Tagged like release-runner.mjs's releaseError, so its failureReason reads
// the same reasons.
const reasonError = (reason) => Object.assign(new Error(reason), { releaseReason: reason });
const prerequisiteError = (names) => reasonError("Missing matrix prerequisites: " + names.join(", "));
// Provisioning (tt deployment setup): npm ci when the install marker is
// missing, then every other missing file is copied from the source checkout.
// An existing file is never overwritten.
export function provisionPrerequisites(cwd, { from, run = (argv, dir) => execFileSync(argv[0], argv.slice(1), { cwd: dir, stdio: "ignore", timeout: 900000 }) } = {}) {
  let source;
  try { source = isAbsolute(from) && statSync(from).isDirectory() ? realpathSync(from) : null; } catch { source = null; }
  if (!source || source === realpathSync(cwd)) throw reasonError("Prerequisite source must be another absolute checkout");
  const [marker] = MATRIX_PREREQUISITES, actions = new Map(), missing = [];
  if (!existsSync(join(cwd, marker))) {
    try { run(["npm", "ci"], cwd); } catch { throw reasonError("Prerequisite install failed: npm ci"); }
    if (existsSync(join(cwd, marker))) actions.set(marker, "installed");
  }
  for (const p of MATRIX_PREREQUISITES) {
    if (actions.has(p)) continue;
    const target = join(cwd, p);
    if (existsSync(target)) { actions.set(p, "present"); continue; }
    if (p === marker || !existsSync(join(source, p))) { missing.push(p); continue; }
    mkdirSync(dirname(target), { recursive: true });
    copyFileSync(join(source, p), target, constants.COPYFILE_EXCL);
    actions.set(p, "copied");
  }
  if (missing.length) throw prerequisiteError(missing);
  if (git(cwd, "status", "--porcelain")) throw reasonError("Prerequisite provisioning changed the checkout");
  return { version: 1, prerequisites: MATRIX_PREREQUISITES.map((p) => ({ path: p, sha256: createHash("sha256").update(readFileSync(join(cwd, p))).digest("hex"), action: actions.get(p) })) };
}

// A run in a clean linked worktree first provisions any missing prerequisite
// from the repository's main worktree, before admission, so it holds no host
// slot. Any other checkout is left as it is and readPrerequisites refuses as
// before. options.installPrerequisites replaces npm ci in tests.
export function provisionWorktreePrerequisites(checks, cwd, options = {}) {
  if (!selectedPrerequisites(checks).some((path) => !existsSync(join(cwd, path)))) return;
  const [gitDir, commonDir] = git(cwd, "rev-parse", "--path-format=absolute", "--git-dir", "--git-common-dir").split("\n");
  if (gitDir === commonDir) return;
  const main = git(cwd, "worktree", "list", "--porcelain").split("\n\n")[0].split("\n");
  if (!main[0].startsWith("worktree ") || main.includes("bare")) return;
  const from = main[0].slice("worktree ".length);
  if (from === realpathSync(cwd)) return;
  if (git(cwd, "status", "--porcelain", "--untracked-files=normal")) return;
  let provisioned;
  try {
    provisioned = provisionPrerequisites(cwd, {
      from,
      ...(options.installPrerequisites ? { run: options.installPrerequisites } : {}),
    });
  } catch (error) {
    throw new Error(error.message);
  }
  const print = options.hostLock?.print ?? console.log;
  print("matrix run: provisioned prerequisites from " + from);
  for (const entry of provisioned.prerequisites)
    print(`matrix run: prerequisite ${entry.path} ${entry.action} ${entry.sha256}`);
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

// Every agent CLI the product can launch: the known list of spawn.Runtimes in
// hub/internal/spawn/spawn.go, which a test keeps equal. No check may start a
// real one: codex starts a setsid'd managed daemon under the verifier home
// that no process-group signal reaches (wi_39bd40d33a4d8acc), and a real
// claude session spends tokens (wi_1c5e64268e8f77b9).
export const AGENT_CLIS = ["claude", "codex", "aider", "gemini", "opencode", "goose", "cursor-agent", "amp", "copilot"];

// A check that reaches an agent CLI through PATH gets this failing stub
// instead. Each call is logged to <output>/<name>-stub-calls.log as audit
// evidence of which check reached which CLI.
export function installAgentCliStubs(home, output) {
  const bin = join(home, ".verifier-bin");
  mkdirSync(bin, { mode: 0o700 });
  for (const name of AGENT_CLIS) {
    const log = "'" + join(output, `${name}-stub-calls.log`).replaceAll("'", "'\\''") + "'";
    writeFileSync(
      join(bin, name),
      `#!/bin/sh
args=$(printf '%s ' "$@" | tr '\\n\\t' '  ' | cut -c1-500)
printf '%s\\t%s\\t%s\\t%s\\t%s\\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$$" "$PPID" "$PWD" "\${args% }" >> ${log}
echo 'tailterm verifier: ${name} is disabled during matrix checks' >&2
exit 1
`,
      { mode: 0o700 },
    );
  }
  return bin;
}

// Shared verification caches (wi_3c81378998da5acd): one harness-owned root
// per host holds the Go module generations, the toolchain, a build cache per
// toolchain and the speech model. Checks only read modules (GOPROXY=off) and
// every go test runs fresh; see docs/objective-verification.md.
const CACHE_PRODUCER = "scripts/verify-matrix.mjs";
// The same rule as scripts/package-speech-model.mjs, which this file does not
// import (the deployer re-execs on the runner's own imports); a test keeps
// the two equal. An absolute TAILTERM_VERIFICATION_CACHE, else the user's
// cache directory.
export function verificationCacheRoot(environment = process.env) {
  const named = environment.TAILTERM_VERIFICATION_CACHE;
  if (named && isAbsolute(named)) return named;
  const home = environment.HOME || homedir();
  if (process.platform === "darwin")
    return join(home, "Library", "Caches", "tailterm-verification");
  const xdg = environment.XDG_CACHE_HOME;
  return join(xdg && isAbsolute(xdg) ? xdg : join(home, ".cache"), "tailterm-verification");
}
const GENERATION = /^gen-[a-f0-9]{16}$/;
const BUILD_OUTPUT = /^[0-9a-f]{64}-d$/;
const BUILD_INDEX = /^[0-9a-f]{64}-a$/;
const BUILD_INDEX_LINE = /^v1 [0-9a-f]{64} [0-9a-f]{64} [ 0-9]{19}[0-9] [ 0-9]{19}[0-9]\n$/;
const BUILD_FILES = new Set(["README", "trim.txt", "testexpire.txt", "tailterm-cache.json"]);
// downloads/ holds scripts/build-wasm.sh's Tailscale tarball by its sha256.
const ROOT_ENTRIES = new Set(["mod", "build", "speech-model", "downloads", "tailterm-cache.json"]);
const DEFAULT_GOPROXY = "https://proxy.golang.org,direct";
// A cold seed (toolchain, hub, historical and wasm module sets) took under a
// minute on the Mini; ten minutes bounds a stalled proxy.
const CACHE_DEADLINE_MS = 10 * 60 * 1000;
// Every check family verification/matrix.json defines can run go: hub checks,
// the wasm build, npm test (release-runner tests build tt) and browser suites
// (prepared or standalone test binaries). Keyed on ids, not argv.
const GO_CACHE_FAMILIES = new Set([
  "go-vet", "go-test", "go-race", "migration-rehearsal", "wasm-test-build",
  "npm-unit", "00-static-build", "01-static-release-verify",
]);
const GO_TEST_CHECKS = new Set(["go-test", "go-race", "migration-rehearsal"]);
export const goCacheFamily = (check) =>
  GO_CACHE_FAMILIES.has(check.id) ||
  /^tests\/[^/]+\.mjs(:(chromium|webkit))?$/.test(check.id);
// A checkout with no hub/go.sum has no module set to seed (every synthetic
// fixture): its checks keep per-home Go caches as before.
export const sharesGoCaches = (checks, cwd) =>
  existsSync(join(cwd, "hub", "go.sum")) && checks.some(goCacheFamily);

// A go test result line marked (cached), plain or inside -json output.
export const cachedGoTestResult = (log) =>
  /(?:^|\n|")ok(?:[ \t]|\\t)+[^\s\\"]+(?:[ \t]|\\t)+\(cached\)/.test(log);
export function assertFreshGoTest(check, log) {
  if (GO_TEST_CHECKS.has(check.id) && cachedGoTestResult(log))
    throw new Error("Go test result came from the build cache: " + check.id);
}

class CacheRefusal extends Error {}
const within = (child, parent) => {
  const rel = relative(parent, child);
  return rel === "" || (rel.split("/")[0] !== ".." && !isAbsolute(rel));
};
// The real path of the deepest existing ancestor plus the rest, so a root that
// does not exist yet still resolves through symlinked parents.
function realish(path) {
  let existing = resolve(path), rest = "";
  while (!existsSync(existing) && dirname(existing) !== existing) {
    rest = join(basename(existing), rest);
    existing = dirname(existing);
  }
  return join(realpathSync(existing), rest);
}
const goos = () => (process.platform === "win32" ? "windows" : process.platform);
const goarch = () => ({ x64: "amd64", ia32: "386" })[process.arch] || process.arch;

// Runs go without blocking the event loop (the host lock heartbeats).
export function runGo(args, { cwd, env, abortSignal }) {
  return new Promise((done) => {
    if (abortSignal?.aborted) return done({ code: -1, stdout: "", stderr: "interrupted" });
    const child = spawn("go", args, { cwd, env, stdio: ["ignore", "pipe", "pipe"] });
    let stdout = "", stderr = "";
    const kill = () => child.kill("SIGTERM");
    abortSignal?.addEventListener("abort", kill, { once: true });
    child.stdout.setEncoding("utf8").on("data", (data) => (stdout += data));
    child.stderr.setEncoding("utf8").on("data", (data) => (stderr += data));
    child.on("error", (error) => {
      abortSignal?.removeEventListener("abort", kill);
      done({ code: -1, stdout, stderr: stderr + error.message });
    });
    child.on("close", (code) => {
      abortSignal?.removeEventListener("abort", kill);
      done({ code: code ?? -1, stdout, stderr });
    });
  });
}

// The Go cache paths of the runner's own environment and the user's defaults.
function userGoPaths(go = runGo) {
  return go(["env", "-json", "GOPATH", "GOMODCACHE", "GOCACHE"], {
    cwd: tmpdir(),
    env: { ...process.env, GOTOOLCHAIN: "local" },
  }).then(({ code, stdout, stderr }) => {
    if (code !== 0) throw new CacheRefusal("go env failed: " + stderr.trim().split("\n")[0]);
    const found = JSON.parse(stdout);
    return [
      join(homedir(), "go"),
      join(homedir(), "Library", "Caches", "go-build"),
      ...String(found.GOPATH || "").split(":").filter(Boolean),
      found.GOMODCACHE,
      found.GOCACHE,
    ].filter((path) => path && isAbsolute(path));
  });
}
// Refuses, before anything is created, a root that overlaps a user Go cache
// or the checkout; returns the root's resolved path.
export async function validateCacheRoot(root, { cwd, paths }) {
  if (typeof root !== "string" || !isAbsolute(root))
    throw new CacheRefusal(`cache root is not an absolute path: ${root}`);
  const real = realish(root);
  for (const path of paths) {
    const other = realish(path);
    if (within(real, other) || within(other, real))
      throw new CacheRefusal(`cache root ${root} overlaps the user's Go cache ${path}`);
  }
  if (within(real, realish(cwd)))
    throw new CacheRefusal(`cache root ${root} is inside the checkout`);
  return real;
}

function readJson(path) {
  let text;
  try {
    text = readFileSync(path, "utf8");
  } catch (error) {
    if (error.code === "ENOENT") return undefined;
    return null;
  }
  try {
    return JSON.parse(text);
  } catch {
    return null;
  }
}
function writeJsonAtomically(path, value) {
  const temporary = `${path}.${process.pid}.${randomBytes(6).toString("hex")}.tmp`;
  writeFileSync(temporary, JSON.stringify(value, null, 2) + "\n", { mode: 0o644 });
  renameSync(temporary, path);
}
// Fields carried from the marker item (wi_60dbbbe5b36bf66b): producer, item,
// run, creation time and the owned directories, relative to the root. The
// shared cache root and every verifier home carry one; the sweep trims only
// what a valid marker owns and reports the rest.
function cacheMarker(kind, context, owns, extra = {}) {
  return {
    version: 1,
    kind,
    producer: CACHE_PRODUCER + (context.commit ? "@" + context.commit : ""),
    item: context.item,
    run: context.run,
    createdAt: new Date().toISOString(),
    owns,
    ...extra,
  };
}
export const validCacheMarker = (marker, kind) =>
  Boolean(marker) && typeof marker === "object" && marker.version === 1 &&
  marker.kind === kind && Array.isArray(marker.owns) &&
  marker.owns.every((path) => typeof path === "string" && path && !isAbsolute(path) &&
    !path.split("/").includes(".."));

// The provenance a run's markers carry: its item, run and candidate commit.
function homeMarkerContext(plan, output) {
  return {
    item: plan.itemId || (plan.orderMessageSeq ? `order ${plan.orderMessageSeq}` : "unknown"),
    run: plan.verifierRunId || output,
    commit: plan.commit,
  };
}
// The private Go caches of a verifier home, relative to it. Listed even when
// the shared cache root leaves them unused, so an absent one is skipped.
export const HOME_MARKER_OWNS = ["go-build", "go/pkg/mod"];
// Marks a verifier home before its first Go command: the sweep removes a
// marked home whose run is gone, or trims a kept one's owned caches, and
// only reports an unmarked one. keepHome, pid and output identify the run.
export function writeHomeMarker(home, context, { keepHome, output }) {
  if (!/^[0-9a-f]{40}$/.test(String(context.commit)))
    throw new Error("Verifier home marker needs the candidate's 40-hex commit");
  if (typeof context.item !== "string" || !context.item || typeof context.run !== "string" || !context.run)
    throw new Error("Verifier home marker needs an item and a run");
  if (typeof output !== "string" || !isAbsolute(output))
    throw new Error("Verifier home marker needs an absolute output directory");
  writeJsonAtomically(
    join(home, "tailterm-cache.json"),
    cacheMarker("home", context, HOME_MARKER_OWNS, { keepHome: keepHome === true, pid: process.pid, output }),
  );
}

function ensureCacheRoot(root, context) {
  const markerPath = join(root, "tailterm-cache.json");
  if (!existsSync(root)) mkdirSync(root, { recursive: true, mode: 0o700 });
  else if (validCacheMarker(readJson(markerPath), "root")) return;
  else {
    const foreign = readdirSync(root).filter((name) => !ROOT_ENTRIES.has(name) && !name.endsWith(".tmp"));
    if (foreign.length)
      throw new CacheRefusal(`cache root ${root} has no tailterm marker and holds ${foreign.slice(0, 3).join(", ")}`);
  }
  writeJsonAtomically(markerPath, cacheMarker("root", context, ["mod", "build", "speech-model", "downloads"]));
}

// Go version strings compare by their numeric release parts.
const goRelease = (version) => String(version).replace(/^go/, "").split(/[^0-9.]/)[0].split(".").map(Number);
export function goVersionAtLeast(have, want) {
  const a = goRelease(have), b = goRelease(want);
  for (let i = 0; i < Math.max(a.length, b.length); i++)
    if ((a[i] || 0) !== (b[i] || 0)) return (a[i] || 0) > (b[i] || 0);
  return true;
}
export function requiredGoVersion(goMod) {
  const language = /^go\s+(\d+\.\d+(?:\.\d+)?)\s*$/m.exec(goMod)?.[1];
  const toolchain = /^toolchain\s+go(\d+\.\d+(?:\.\d+)?)\s*$/m.exec(goMod)?.[1];
  if (!language) throw new CacheRefusal("hub/go.mod has no go line");
  let want = toolchain && goVersionAtLeast(toolchain, language) ? toolchain : language;
  // Since Go 1.21 a two-part go line names the .0 release.
  if (want.split(".").length === 2 && Number(want.split(".")[1]) >= 21) want += ".0";
  return "go" + want;
}

async function treeDigest(dir) {
  const lines = [];
  const visit = async (path, rel) => {
    for (const name of readdirSync(path).sort()) {
      const full = join(path, name), entry = lstatSync(full);
      if (entry.isDirectory()) await visit(full, rel + name + "/");
      else if (entry.isSymbolicLink()) lines.push(`${rel}${name}\0link\0${readlinkSync(full)}`);
      else if (entry.isFile()) lines.push(`${rel}${name}\0${entry.size}\0${await fileHash(full)}`);
    }
  };
  await visit(dir, "");
  return createHash("sha256").update(lines.join("\n")).digest("hex");
}
const countZips = (dir) => {
  let count = 0;
  const visit = (path) => {
    let names;
    try {
      names = readdirSync(path, { withFileTypes: true });
    } catch {
      return;
    }
    for (const entry of names)
      if (entry.isDirectory()) {
        // The toolchain is counted on its own.
        if (!(entry.name === "toolchain" && basename(path) === "golang.org")) visit(join(path, entry.name));
      } else if (entry.name.endsWith(".zip")) count++;
  };
  visit(join(dir, "cache", "download"));
  return count;
};

// The module sources a plan needs: the candidate's hub module, and the
// historical hub's when that binary is prepared (extracted into the home).
function moduleSources(cwd, home, plan) {
  const sources = [{ name: "hub", dir: join(cwd, "hub") }];
  if (plan.checks.some((check) => check.id.startsWith("tests/board-audit-old-hub-browser.mjs"))) {
    const dir = join(home, ".verifier-sources", historicalHubCommit);
    mkdirSync(dir, { recursive: true, mode: 0o700 });
    const archive = execFileSync("git", ["archive", "--format=tar", historicalHubCommit, "hub"], {
      cwd, maxBuffer: 128 * 1024 * 1024,
    });
    execFileSync("tar", ["-xf", "-", "-C", dir], { input: archive });
    sources.push({ name: "historical " + historicalHubCommit, dir: join(dir, "hub") });
  }
  for (const source of sources)
    source.goSumSha256 = createHash("sha256").update(readFileSync(join(source.dir, "go.sum"))).digest("hex");
  return sources;
}

// scripts/build-wasm.sh builds Tailscale's cmd/tsconnect/wasm with the
// checkout's wasm/*.go copied in. Read-only: its Tailscale version must be the
// one hub/go.mod requires, and its cp lines become a go list -overlay. Returns
// the seed plan, or the cause the wasm check keeps per-home modules. A line
// that looks like a copy into the wasm package but is not of the one shape
// read here is named in the cause, never skipped.
const WASM_COPY = /^cp wasm\/([A-Za-z0-9_.-]+\.go) "\$source\/cmd\/tsconnect\/wasm\/([A-Za-z0-9_.-]+\.go)"$/;
const wasmCopyCandidate = (line) =>
  (/^\s*cp\s/.test(line) && line.includes("wasm/")) || /cmd\/tsconnect\/wasm\/\S/.test(line);
export function wasmSeedSpec(cwd) {
  let script, goMod;
  try {
    script = readFileSync(join(cwd, "scripts", "build-wasm.sh"), "utf8");
    goMod = readFileSync(join(cwd, "hub", "go.mod"), "utf8");
  } catch (error) {
    return { cause: "build-wasm.sh or hub/go.mod unreadable: " + error.code };
  }
  const version = /^source="\$root\/\.build\/tailscale-(\d+\.\d+\.\d+)"$/m.exec(script)?.[1];
  const copies = [];
  for (const [index, line] of script.split("\n").entries()) {
    if (/^\s*#/.test(line) || !wasmCopyCandidate(line)) continue;
    const found = WASM_COPY.exec(line);
    if (!found)
      return { cause: `build-wasm.sh line ${index + 1} is a copy line the runner cannot read: ${line.trim().slice(0, 160)}` };
    copies.push({ from: found[1], to: found[2] });
  }
  if (!version || !copies.length) return { cause: "build-wasm.sh has no Tailscale version or copy lines" };
  const required = /^\s*(?:require\s+)?tailscale\.com\s+v(\S+)/m.exec(goMod)?.[1];
  if (required !== version)
    return { cause: `build-wasm.sh builds tailscale ${version} but hub/go.mod requires ${required || "none"}` };
  return { version, copies };
}
// The wasm source: a copy of the Tailscale module beside the generation (Go
// refuses an overlay of files beneath GOMODCACHE), made once per generation,
// with the checkout's files overlaid as the script copies them. Before any go
// command runs in it, the copy must name module tailscale.com and hold the
// same tree as the generation's module directory (which the hub source's go
// mod verify covers first); a copy that does not is replaced, never used.
function wasmSource(spec, cwd, home) {
  const module = `tailscale.com@v${spec.version}`;
  const overlayPath = join(home, ".verifier-sources", "wasm-overlay.json");
  const source = {
    name: "wasm " + module,
    wasm: true,
    env: { GOOS: "js", GOARCH: "wasm" },
    async resolve(generation) {
      const copy = join(`${generation}.wasm-src`, module);
      if (this.verified === generation && existsSync(copy)) return copy;
      const cached = join(generation, module);
      if (!existsSync(join(cached, "go.mod"))) throw new CacheRefusal(`${module} is not in ${basename(generation)}`);
      const makeCopy = () => {
        mkdirSync(dirname(copy), { recursive: true, mode: 0o755 });
        const temporary = `${copy}.${process.pid}.${randomBytes(6).toString("hex")}.tmp`;
        execFileSync("cp", [process.platform === "darwin" ? "-cR" : "-R", cached, temporary]);
        try {
          renameSync(temporary, copy);
        } catch (error) {
          removeVerifierHome(temporary);
          if (!["EEXIST", "ENOTEMPTY"].includes(error.code)) throw error;
        }
      };
      const want = await treeDigest(cached);
      const mismatch = async () => {
        try {
          const named = /^module\s+(\S+)\s*$/m.exec(readFileSync(join(copy, "go.mod"), "utf8"))?.[1];
          if (named !== "tailscale.com") return `go.mod names module ${named || "none"}`;
          return (await treeDigest(copy)) === want ? "" : `its tree differs from ${module}`;
        } catch (error) {
          return "unreadable: " + (error.code || error.message);
        }
      };
      let state = "verified", cause = "";
      for (let attempt = 0; attempt < 3; attempt++) {
        if (!existsSync(copy)) {
          makeCopy();
          if (state === "verified") state = "made";
        }
        const inode = lstatSync(copy, { throwIfNoEntry: false })?.ino;
        const found = await mismatch();
        if (!found) {
          this.dir = copy;
          this.copy = state;
          this.copyCause = cause;
          this.verified = generation;
          break;
        }
        cause ||= found;
        state = "replaced";
        // A peer that replaced the copy meanwhile is re-verified, not removed.
        if (lstatSync(copy, { throwIfNoEntry: false })?.ino !== inode) continue;
        const aside = `${copy}.${process.pid}.${randomBytes(6).toString("hex")}.tmp`;
        try {
          renameSync(copy, aside);
        } catch (error) {
          if (error.code !== "ENOENT") throw error;
          continue;
        }
        removeVerifierHome(aside);
      }
      if (this.verified !== generation)
        throw new CacheRefusal(`the ${module} copy beside ${basename(generation)} does not match it: ${cause}`);
      mkdirSync(dirname(overlayPath), { recursive: true, mode: 0o700 });
      writeFileSync(overlayPath, JSON.stringify({
        Replace: Object.fromEntries(spec.copies.map(({ from, to }) =>
          [join(copy, "cmd", "tsconnect", "wasm", to), join(cwd, "wasm", from)])),
      }));
      this.goSumSha256 = createHash("sha256").update(readFileSync(join(copy, "go.sum"))).digest("hex");
      return copy;
    },
    lists: [
      ["list", "-deps", "-overlay", overlayPath, "./cmd/tsconnect/wasm"],
      ["list", "-deps", "-tags=tailserve_test", "-overlay", overlayPath, "./cmd/tsconnect/wasm"],
    ],
  };
  return source;
}
const sourceLists = (source) => source.lists || [["list", "-deps", "-test", "./..."]];
const sourceDir = async (source, generation) => {
  try {
    return source.resolve ? await source.resolve(generation) : source.dir;
  } catch (error) {
    throw Object.assign(error, { source });
  }
};

// Validates a generation for every source with the network off. Returns the
// first problem: "incomplete" (a module is missing) or "integrity".
async function validateGeneration(generation, sources, environment, go, abortSignal) {
  const env = { ...environment, GOMODCACHE: generation, GOPROXY: "off" };
  for (const source of sources) {
    const cwd = await sourceDir(source, generation);
    for (const args of sourceLists(source)) {
      const listed = await go(args, { cwd, env: { ...env, ...source.env }, abortSignal });
      if (listed.code !== 0)
        return { problem: "incomplete", source, detail: listed.stderr.trim().split("\n")[0] };
    }
    const verified = await go(["mod", "verify"], { cwd, env, abortSignal });
    if (verified.code !== 0) {
      const detail = verified.stderr.trim().split("\n")[0];
      return { problem: /GOPROXY=off|module lookup disabled/.test(verified.stderr) ? "incomplete" : "integrity", source, detail };
    }
  }
  return {};
}
// Adds a source's module graph and its package modules to a generation.
async function seedGeneration(generation, sources, environment, goProxy, go, abortSignal) {
  const env = { ...environment, GOMODCACHE: generation, GOPROXY: goProxy };
  for (const source of sources) {
    const cwd = await sourceDir(source, generation);
    for (const args of [["list", "-m", "all"], ...sourceLists(source)]) {
      const run = await go(args, { cwd, env: { ...env, ...source.env }, abortSignal });
      if (run.code !== 0)
        throw Object.assign(
          new CacheRefusal(`seeding ${source.name} failed: ${run.stderr.trim().split("\n").at(-1)}`),
          { source });
    }
  }
}

// Selects (and, when needed, seeds) the module generation and toolchain.
async function prepareModules(root, sources, toolchain, environment, context, options) {
  const { goProxy, go, abortSignal } = options;
  const modRoot = join(root, "mod");
  mkdirSync(modRoot, { recursive: true, mode: 0o700 });
  const pointerPath = join(modRoot, "current.json");
  const report = { state: "warm", generation: "", modulesAdded: 0, toolchainDownloads: 0 };
  let generation, cause = "";
  const pointer = readJson(pointerPath);
  if (pointer === null || (pointer !== undefined && !validCacheMarker(pointer, "mod")))
    cause = "unreadable current.json";
  else if (pointer) {
    if (typeof pointer.dir !== "string" || !GENERATION.test(pointer.dir))
      throw new CacheRefusal(`current.json names an unsafe generation: ${JSON.stringify(pointer.dir)}`);
    const path = join(modRoot, pointer.dir);
    if (!existsSync(path)) cause = `generation ${pointer.dir} is missing`;
    else if (realpathSync(path) !== join(realpathSync(modRoot), pointer.dir) || !statSync(path).isDirectory())
      throw new CacheRefusal(`current.json generation ${pointer.dir} resolves outside ${modRoot}`);
    else generation = path;
  }
  const stampPath = (dir, source) => join(`${dir}.stamps`, `${source.goSumSha256}.json`);
  const toolchainDir = (dir) =>
    join(dir, "golang.org", `toolchain@v0.0.1-${toolchain.version}.${goos()}-${goarch()}`);
  const env = () => ({
    ...environment,
    PATH: toolchain.source === "host" ? environment.PATH : join(toolchainDir(generation), "bin") + ":" + environment.PATH,
  });
  // The toolchain an older host needs: present, runnable and, once stamped,
  // with the same tree digest.
  const checkToolchain = async (dir) => {
    if (toolchain.source === "host") return "";
    const tree = toolchainDir(dir);
    if (!existsSync(join(tree, "bin", "go"))) return "incomplete";
    const ran = await go(["version"], {
      cwd: tmpdir(), env: { ...environment, PATH: join(tree, "bin") + ":" + environment.PATH }, abortSignal,
    });
    if (ran.code !== 0 || !ran.stdout.includes(toolchain.version + " ")) return "integrity";
    toolchain.treeSha256 = await treeDigest(tree);
    // Every stamp of the generation, whatever go.sum it was written for.
    let stamps = [];
    try {
      stamps = readdirSync(`${dir}.stamps`).filter((name) => name.endsWith(".json"));
    } catch {}
    const stamped = stamps
      .map((name) => readJson(join(`${dir}.stamps`, name)))
      .filter((stamp) => validCacheMarker(stamp, "stamp") && stamp.toolchain === toolchain.version &&
        typeof stamp.toolchainTreeSha256 === "string");
    return stamped.some((stamp) => stamp.toolchainTreeSha256 !== toolchain.treeSha256) ? "integrity" : "";
  };
  const seedToolchain = async (dir) => {
    const run = await go(["version"], {
      cwd: tmpdir(),
      env: { ...environment, GOMODCACHE: dir, GOPROXY: goProxy, GOTOOLCHAIN: toolchain.version },
      abortSignal,
    });
    if (run.code !== 0 || !run.stdout.includes(toolchain.version))
      throw new CacheRefusal(`toolchain ${toolchain.version} download failed: ${run.stderr.trim().split("\n").at(-1)}`);
    report.toolchainDownloads++;
    toolchain.treeSha256 = await treeDigest(toolchainDir(dir));
  };
  if (generation) {
    const tool = await checkToolchain(generation);
    if (tool === "integrity") {
      cause = "toolchain tree changed";
      generation = undefined;
    } else {
      if (tool === "incomplete") {
        await seedToolchain(generation);
        report.state = "seeded";
      }
      let found = await validateGeneration(generation, sources, env(), go, abortSignal);
      if (found.problem === "incomplete") {
        const before = countZips(generation);
        await seedGeneration(generation, sources, env(), goProxy, go, abortSignal);
        report.modulesAdded += countZips(generation) - before;
        report.state = "seeded";
        found = await validateGeneration(generation, sources, env(), go, abortSignal);
      }
      if (found.problem) {
        cause = `${found.problem} (${found.source.name}): ${found.detail}`;
        generation = undefined;
      }
    }
  }
  if (!generation) {
    const name = "gen-" + randomBytes(8).toString("hex");
    generation = join(modRoot, name);
    mkdirSync(generation, { mode: 0o755 });
    if (toolchain.source !== "host") await seedToolchain(generation);
    await seedGeneration(generation, sources, env(), goProxy, go, abortSignal);
    report.modulesAdded += countZips(generation);
    const found = await validateGeneration(generation, sources, env(), go, abortSignal);
    if (found.problem)
      throw new CacheRefusal(`new generation ${name} failed validation: ${found.problem}: ${found.detail}`);
    writeJsonAtomically(pointerPath, cacheMarker("mod", context, ["mod/" + name], { dir: name }));
    report.state = cause ? "rebuilt" : "seeded";
    report.cause = cause;
  }
  const name = basename(generation);
  mkdirSync(`${generation}.stamps`, { recursive: true, mode: 0o755 });
  report.stampsWritten = 0;
  for (const source of sources) {
    const stamp = readJson(stampPath(generation, source));
    if (report.state === "warm" && validCacheMarker(stamp, "stamp") && stamp.toolchain === toolchain.version &&
        stamp.toolchainTreeSha256 === (toolchain.treeSha256 || null))
      continue;
    report.stampsWritten++;
    writeJsonAtomically(stampPath(generation, source), cacheMarker("stamp", context, ["mod/" + name], {
      dir: name,
      goSumSha256: source.goSumSha256,
      source: source.name,
      toolchain: toolchain.version,
      toolchainTreeSha256: toolchain.treeSha256 || null,
    }));
  }
  report.generation = name;
  report.generations = readdirSync(modRoot).filter((entry) => GENERATION.test(entry)).length;
  return { generation, env: env(), report };
}

// A Go build cache directory is ours when its marker is valid and it holds
// nothing Go would not write there.
function validBuildDir(dir) {
  if (!validCacheMarker(readJson(join(dir, "tailterm-cache.json")), "build")) return false;
  return readdirSync(dir).every((name) =>
    BUILD_FILES.has(name) || /^[0-9a-f]{2}$/.test(name) || name.endsWith(".tmp"));
}
function selectBuildDir(root, version, context) {
  const base = join(root, "build", `${version}-${goos()}-${goarch()}`);
  const skipped = [];
  for (let n = 1; n <= 64; n++) {
    const dir = n === 1 ? base : `${base}.${n}`;
    if (!existsSync(dir)) {
      // Made complete under a temporary name, then renamed: a peer sees either
      // nothing or a directory with its marker.
      mkdirSync(dirname(dir), { recursive: true, mode: 0o755 });
      const temporary = `${dir}.${process.pid}.${randomBytes(6).toString("hex")}.tmp`;
      mkdirSync(temporary, { mode: 0o755 });
      writeJsonAtomically(join(temporary, "tailterm-cache.json"),
        cacheMarker("build", context, [relative(root, dir)], { toolchain: version }));
      try {
        renameSync(temporary, dir);
      } catch (error) {
        rmSync(temporary, { recursive: true, force: true });
        if (!["EEXIST", "ENOTEMPTY"].includes(error.code)) throw error;
      }
    }
    // Confined to <root>/build by real path, like the module generations.
    const real = realpathSync(dir);
    if (lstatSync(dir).isSymbolicLink() || real !== join(realpathSync(join(root, "build")), basename(dir)) ||
        !within(realpathSync(join(root, "build")), root))
      throw new CacheRefusal(`build directory ${basename(dir)} resolves outside ${join(root, "build")}`);
    if (validBuildDir(dir)) return { dir, skipped };
    skipped.push(basename(dir));
  }
  throw new CacheRefusal(`no usable build cache directory under ${dirname(base)}`);
}

const pause = (ms) => new Promise((done) => setTimeout(done, ms));
// Go names each stored output by the sha256 of its bytes and only checks the
// size when it reads one. Every output whose bytes do not hash to its name is
// removed (an executable directory whole), so Go rebuilds it; a malformed
// index entry likewise. A peer writes outputs in place, last byte last: a
// mismatch is removed only once its size and mtime stay put.
export async function scanBuildCache(dir, { settleMs = 200, settleTries = 5 } = {}) {
  const started = Date.now();
  const result = { outputs: 0, removed: 0, inUse: 0, ms: 0 };
  const stable = async (path, check) => {
    let before = statSync(path, { throwIfNoEntry: false });
    if (!before) return false;
    for (let i = 0; i < settleTries; i++) {
      await pause(settleMs);
      const now = statSync(path, { throwIfNoEntry: false });
      if (!now) return false;
      if (now.size === before.size && now.mtimeMs === before.mtimeMs) return !(await check());
      before = now;
    }
    return false;
  };
  const hashMatches = async (path, id) => (await fileHash(path)) === id;
  for (const shard of readdirSync(dir).filter((name) => /^[0-9a-f]{2}$/.test(name)).sort()) {
    const shardDir = join(dir, shard);
    for (const name of readdirSync(shardDir)) {
      const path = join(shardDir, name);
      const entry = lstatSync(path, { throwIfNoEntry: false });
      if (!entry) continue;
      if (BUILD_OUTPUT.test(name)) {
        const id = name.slice(0, 64);
        result.outputs++;
        if (entry.isFile()) {
          if (await hashMatches(path, id)) continue;
          if (await stable(path, () => hashMatches(path, id))) {
            rmSync(path, { force: true });
            result.removed++;
          } else result.inUse++;
        } else if (entry.isDirectory()) {
          for (const inner of readdirSync(path)) {
            const file = join(path, inner);
            if (await hashMatches(file, id)) continue;
            if (await stable(file, () => hashMatches(file, id))) {
              rmSync(path, { recursive: true, force: true });
              result.removed++;
            } else result.inUse++;
            break;
          }
        }
      } else if (BUILD_INDEX.test(name) && entry.isFile()) {
        const wellFormed = () => BUILD_INDEX_LINE.test(readFileSync(path, "latin1"));
        if (wellFormed()) continue;
        if (await stable(path, async () => wellFormed())) {
          rmSync(path, { force: true });
          result.removed++;
        } else result.inUse++;
      }
    }
  }
  result.ms = Date.now() - started;
  return result;
}

// Decides the run's Go caches. Shared: modules from the current generation,
// the toolchain from the host or that generation, a build cache per
// toolchain. Private (refused or failed): today's per-home caches. Writes
// cache-report.json and returns the environment additions and the line.
export async function prepareVerificationCaches({ plan, cwd, home, output, environment, options = {} }) {
  const {
    root: cacheRoot = verificationCacheRoot(process.env),
    goProxy = process.env.GOPROXY && process.env.GOPROXY !== "off" ? process.env.GOPROXY : DEFAULT_GOPROXY,
    go = runGo,
    hostGoVersion,
    userGoPaths: paths,
    settleMs,
    // Runner-side seeding and validation run under the host lock and outside
    // every check timeout: past this the run goes private.
    deadlineMs = CACHE_DEADLINE_MS,
    abortSignal: runSignal,
  } = options;
  const deadline = new AbortController();
  const timer = setTimeout(() => deadline.abort("deadline"), deadlineMs);
  const abortSignal = runSignal ? AbortSignal.any([runSignal, deadline.signal]) : deadline.signal;
  const privateEnvironment = {
    GOMODCACHE: join(home, "go", "pkg", "mod"),
    GOCACHE: join(home, "go-build"),
    GOTOOLCHAIN: "auto",
    GOFLAGS: "-count=1",
  };
  if (!sharesGoCaches(plan.checks, cwd)) {
    clearTimeout(timer);
    return { environment: privateEnvironment };
  }
  const context = homeMarkerContext(plan, output);
  const report = { version: 1, root: cacheRoot, mode: "shared", createdAt: new Date().toISOString() };
  let line;
  try {
    const root = await validateCacheRoot(cacheRoot, { cwd, paths: paths ?? (await userGoPaths(go)) });
    const required = requiredGoVersion(readFileSync(join(cwd, "hub", "go.mod"), "utf8"));
    let host = hostGoVersion;
    if (!host) {
      const found = await go(["env", "GOVERSION"], {
        cwd: tmpdir(), env: { ...environment, GOTOOLCHAIN: "local" }, abortSignal,
      });
      if (found.code !== 0) throw new CacheRefusal("host go is not runnable: " + found.stderr.trim());
      host = found.stdout.trim();
    }
    const toolchain = goVersionAtLeast(host, required)
      ? { source: "host", version: host }
      : { source: "cached", version: required };
    ensureCacheRoot(root, context);
    // The build cache is scanned before the run's first Go command uses it.
    const build = selectBuildDir(root, toolchain.version, context);
    const scan = await scanBuildCache(build.dir, settleMs === undefined ? {} : { settleMs });
    const sources = moduleSources(cwd, home, plan);
    const base = { ...environment, GOTOOLCHAIN: "local", GOFLAGS: "-mod=readonly", GOCACHE: build.dir };
    // wasm-test-build reads Tailscale's module set from the generation too;
    // only a script the runner cannot read safely, or a wasm seed that fails
    // without corruption, leaves that one check on per-home modules.
    let wasm = null;
    if (plan.checks.some((check) => check.id === "wasm-test-build")) {
      const spec = wasmSeedSpec(cwd);
      wasm = spec.cause ? { cause: spec.cause } : { source: wasmSource(spec, cwd, home) };
    }
    const prepare = (list) => prepareModules(root, list, toolchain, base, context, { goProxy, go, abortSignal });
    let modules;
    try {
      modules = await prepare(wasm?.source ? [...sources, wasm.source] : sources);
    } catch (error) {
      if (!error.source?.wasm || abortSignal.aborted) throw error;
      wasm = { cause: error.message.split("\n")[0] };
      modules = await prepare(sources);
    }
    report.modules = modules.report;
    report.toolchain = { version: toolchain.version, source: toolchain.source, host, required, treeSha256: toolchain.treeSha256 || null };
    report.build = { dir: build.dir, skipped: build.skipped, ...scan };
    report.wasm = wasm && (wasm.cause
      ? { modules: "per-home", cause: wasm.cause }
      : { modules: "shared", source: wasm.source.name, copy: wasm.source.copy,
          ...(wasm.source.copyCause ? { copyCause: wasm.source.copyCause } : {}) });
    report.downloads = { toolchains: modules.report.toolchainDownloads, modules: modules.report.modulesAdded };
    const state = modules.report.state === "rebuilt"
      ? `rebuilt: ${modules.report.cause}`
      : modules.report.state === "seeded" ? `seeded ${modules.report.modulesAdded} modules` : "warm";
    line =
      `matrix run: go caches: modules ${modules.report.generation} (${state})` +
      (modules.report.generations > 1 ? ` of ${modules.report.generations} generations` : "") +
      `, toolchain ${toolchain.version} (${toolchain.source === "host" ? "host" : `cached; host ${host} is older than go.mod`})` +
      `, build ${basename(build.dir)} scanned ${scan.outputs} outputs in ${scan.ms} ms, removed ${scan.removed}` +
      (wasm?.cause ? `, wasm-test-build: per-home modules (${wasm.cause})` : wasm ? ", wasm-test-build: shared modules" : "");
    writeFileSync(join(output, "cache-report.json"), JSON.stringify(report, null, 2) + "\n", { mode: 0o600 });
    return {
      line,
      report,
      environment: {
        GOMODCACHE: modules.generation,
        GOCACHE: build.dir,
        GOPROXY: "off",
        GOFLAGS: "-count=1 -mod=readonly",
        GOTOOLCHAIN: "local",
        PATH: modules.env.PATH,
        TAILTERM_VERIFICATION_CACHE: root,
      },
      // The fallback: that check alone keeps a per-home module cache with the
      // network, and the shared toolchain and build cache.
      checkEnvironment: wasm?.cause
        ? { "wasm-test-build": { GOMODCACHE: join(home, "go", "pkg", "mod"), GOPROXY: goProxy } }
        : {},
    };
  } catch (error) {
    if (runSignal?.aborted) throw error;
    const cause = deadline.signal.aborted
      ? `cache preparation exceeded its ${deadlineMs} ms deadline`
      : error.message.split("\n")[0];
    Object.assign(report, { mode: "private", cause });
    writeFileSync(join(output, "cache-report.json"), JSON.stringify(report, null, 2) + "\n", { mode: 0o600 });
    return { line: `matrix run: go caches: private: ${cause}`, report, environment: privateEnvironment };
  } finally {
    clearTimeout(timer);
  }
}

// The spellings a process may use for the verifier home: the path mkdtemp
// returned and its realpath (/var links to /private/var, and lsof reports the
// realpath). Resolving the parent keeps this valid after the home is removed.
function homeRoots(home) {
  return [...new Set([home, join(realpathSync(dirname(home)), basename(home))])];
}
const underRoots = (roots, path) =>
  roots.some((root) => path === root || path.startsWith(root + "/"));

// How much older than the verifier home an unreadable process must be to count
// as started before it.
const UNREADABLE_START_MARGIN_MS = 5000;
// Linux refuses /proc/PID/environ for a same-user process that is not
// dumpable (an ssh-agent, a keyring daemon). Such a process is proven
// unrelated to the verifier home only when it started before the home was
// created: this run cannot have started it, and the home's random name did not
// exist when its environment was set. Those are reported and skipped; a pid
// that is gone by the time ps runs is skipped like any exited process; every
// other case throws (wi_19db6841a2a50d57).
function skipUnreadableOlderThanHome(unreadable, table, { run, self, now, homeCreatedMs, onUnreadable }) {
  const pids = unreadable.map(({ pid }) => pid);
  const unproven = (why, which = pids) =>
    new Error(
      `Cannot read the environment of ${which.length} same-user process${which.length === 1 ? "" : "es"} ` +
        `(pid ${which.slice(0, 10).join(", ")}${which.length > 10 ? ", ..." : ""}) ` +
        `or prove ${which.length === 1 ? "it" : "them"} unrelated to the verifier home: ${why}`,
    );
  if (!Number.isFinite(homeCreatedMs)) throw unproven("the home's creation time is not known");
  // The runner's own pid keeps ps at exit 0 with a row when every listed
  // process has gone.
  let listing;
  try {
    listing = run("ps", ["-o", "pid=,etimes=", "-p", [...pids, self].join(",")]);
  } catch (error) {
    throw unproven("ps could not report start times: " + error.message);
  }
  // etimes is whole seconds elapsed when ps ran, so the clock is read after
  // it: a slow ps can then only make a process look younger.
  const at = now();
  const elapsed = new Map();
  for (const line of listing.split("\n")) {
    if (!line.trim()) continue;
    const found = /^\s*(\d+)\s+(\S+)\s*$/.exec(line);
    if (!found) throw unproven("ps printed a line that could not be read");
    elapsed.set(Number(found[1]), found[2]);
  }
  if (!elapsed.has(self)) throw unproven("ps did not list the runner itself");
  const older = [],
    young = [];
  for (const { pid, code } of unreadable) {
    if (!elapsed.has(pid)) continue;
    const seconds = elapsed.get(pid);
    const startedMs = /^\d+$/.test(seconds) ? at - Number(seconds) * 1000 : NaN;
    if (startedMs < homeCreatedMs - UNREADABLE_START_MARGIN_MS)
      older.push({
        pid,
        code,
        startedAt: new Date(startedMs).toISOString(),
        command: (table.get(pid)?.command ?? "").slice(0, 300),
      });
    else young.push(pid);
  }
  for (const entry of older) onUnreadable?.(entry);
  if (young.length)
    throw unproven("not started before the home was created, or start time unknown", young);
}

// HOME, TMPDIR or PWD values of each listed process. Raw environments hold
// other processes' credentials, so they are matched here and never kept. An
// unreadable process that cannot be proven unrelated is returned as unproven
// beside the matches, so the readable leftovers are still found.
function environmentMatches(table, roots, probes) {
  const { platform, run, readEnviron } = probes;
  let unproven;
  const matches = new Set(),
    variable = /(?:^|\s)(?:HOME|TMPDIR|PWD)=(\S+)/g;
  if (platform === "darwin") {
    for (const line of run("ps", ["-axww", "-E", "-o", "pid=,command="]).split("\n")) {
      const found = /^\s*(\d+)\s(.*)$/.exec(line);
      if (found && table.has(Number(found[1])))
        for (const [, value] of found[2].matchAll(variable))
          if (underRoots(roots, value)) matches.add(Number(found[1]));
    }
  } else if (platform === "linux") {
    const unreadable = [];
    for (const pid of table.keys()) {
      let environ;
      try {
        environ = readEnviron(pid);
      } catch (error) {
        if (error.code === "ENOENT" || error.code === "ESRCH") continue;
        if (error.code !== "EACCES" && error.code !== "EPERM") throw error;
        unreadable.push({ pid, code: error.code });
        continue;
      }
      for (const entry of environ.split("\0")) {
        const found = /^(?:HOME|TMPDIR|PWD)=(.*)$/.exec(entry);
        if (found && underRoots(roots, found[1])) matches.add(pid);
      }
    }
    if (unreadable.length)
      try {
        skipUnreadableOlderThanHome(unreadable, table, probes);
      } catch (error) {
        unproven = error;
      }
  } else throw new Error("Unsupported platform for the verifier home sweep");
  return { matches, unproven };
}

// Processes of this user that still live in or hold files under the verifier
// home, found by environment, argv[0], cwd or any open file. The caller and
// its ancestors are never listed. ps or lsof failure throws (fail closed).
// An unproven unreadable process throws only after the sweep, with what it
// found as error.found, so the stop signals those first.
// probes replaces the platform, the commands, the environ read, the user, the
// runner's pid and the clock for tests, and carries homeCreatedMs and
// onUnreadable for the unreadable-environment rule above.
export function verifierHomeProcesses(home, probes = {}) {
  const {
    platform = process.platform,
    run = (command, args) =>
      execFileSync(command, args, {
        encoding: "utf8",
        timeout: 60_000,
        maxBuffer: 512 * 1024 * 1024,
        stdio: ["ignore", "pipe", "pipe"],
      }),
    readEnviron = (pid) => readFileSync(`/proc/${pid}/environ`, "utf8"),
    uid = process.getuid(),
    self = process.pid,
    now = Date.now,
    homeCreatedMs,
    onUnreadable,
  } = probes;
  const roots = homeRoots(home);
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
  for (let pid = self; pid > 1 && !excluded.has(pid); pid = all.get(pid)?.ppid ?? 0)
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
  const { matches, unproven } = environmentMatches(table, roots, {
    platform,
    run,
    readEnviron,
    self,
    now,
    homeCreatedMs,
    onUnreadable,
  });
  for (const pid of matches) add(pid, "env");
  for (const [pid, entry] of table)
    if (underRoots(roots, entry.command.split(" ")[0])) add(pid, "argv");
  let pid = 0;
  for (const line of run("lsof", ["-nP", "-w", "-u", String(uid), "-F", "pn"]).split("\n")) {
    if (line[0] === "p") pid = Number(line.slice(1));
    else if (line[0] === "n" && underRoots(roots, line.slice(1))) add(pid, "file");
  }
  const result = [...found.values()].sort((a, b) => a.pid - b.pid);
  if (unproven) throw Object.assign(unproven, { found: result });
  return result;
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
// remains, so the home is kept and no receipt stands. A sweep that names an
// unproven process still stops what it found, then throws that diagnostic
// (wi_b42cbccf0bd90795).
export async function stopVerifierHomeProcesses(home, find = verifierHomeProcesses, probes) {
  const stopped = new Map();
  let unproven;
  const sweep = () => {
    unproven = undefined;
    try {
      return find(home, probes);
    } catch (error) {
      if (!Array.isArray(error.found)) throw error;
      unproven = error;
      return error.found;
    }
  };
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
    const found = sweep();
    if (!found.length) {
      if (unproven) throw unproven;
      return [...stopped.values()];
    }
    for (const hit of found) stopped.set(hit.pid, hit);
    const pids = found.map((hit) => hit.pid);
    signal(pids, "SIGTERM");
    await settle(pids);
    const survivors = pids.filter(processAlive);
    signal(survivors, "SIGKILL");
    await settle(survivors);
  }
  const remaining = sweep();
  if (remaining.length)
    throw new Error(
      "processes survived SIGKILL: " + remaining.map((hit) => hit.pid).join(", "),
    );
  if (unproven) throw unproven;
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
// The files scripts/build-wasm.sh --test writes, in MATRIX_PREREQUISITES
// order, and the checks that read them: 00-static-build packs
// wasm/tailserve.wasm into dist-static and the browser suites load both.
const WASM_FILES = ["wasm/tailserve.wasm", ".build/test.wasm"];
// build-wasm.sh --test rewrites the module list last, and 00-static-build
// reads it for the license notices.
const GO_MODULES = ".build/go-modules.txt";
const isWasmReader = (check) => isBrowserCheck(check) || check.id === "00-static-build";
// Why a plan would let a WASM reader start before wasm-test-build rewrote
// its input, or "" when the plan has no such reader or no wasm-test-build.
export function wasmOrderCause(checks) {
  const build = checks.findIndex((check) => check.id === "wasm-test-build");
  const early = build < 0 ? undefined : checks.slice(0, build).find(isWasmReader);
  return early ? early.id + " runs before wasm-test-build" : "";
}
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
// A go-race that will shard runs a pool of race test processes: the job for
// its other packages (go test -p=1, one process) and every shard, at most the
// job count at once. Each process is budgeted at GO_RACE_PROCESS_BYTES against
// the JOB_SLOT_BYTES defaultJobs assumes per slot: 2 slots up to 6 processes,
// 3 for 7 or 8 (measured: 0.63 GB largest store race test process, 0.9 GB
// largest process of the go test tree).
export const goRaceShardWeight = (k) =>
  Math.max(2, Math.ceil((k * GO_RACE_PROCESS_BYTES) / JOB_SLOT_BYTES));
export const checkWeight = (check, jobs) => {
  const shards = goRaceShardSpec(check);
  return Math.min(
    jobs,
    shards
      ? goRaceShardWeight(
          Math.min(1 + shards.reduce((sum, s) => sum + s.k, 0), jobs),
        )
      : (CHECK_WEIGHTS.get(check.id) ?? 1),
  );
};

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
    Math.min(8, Math.floor(cpus / 2), Math.floor(memory / JOB_SLOT_BYTES)),
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
    assertFreshGoTest(check, log);
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

// Budget warnings: a check whose slowest passing attempt in this run took
// strictly more than 70 percent of its planned timeout is close to timing out.
// Only this run's results are read. A failed or timed-out attempt never
// counts, so a check with no passing attempt gives no warning.
export const BUDGET_WARNING_PERCENT = 70;
export function budgetWarnings(checks) {
  const warnings = [];
  for (const check of checks) {
    const timeoutMs = Number(check.environment?.VERIFICATION_TIMEOUT_MS);
    if (!Number.isFinite(timeoutMs) || timeoutMs <= 0) continue;
    const passing = (check.attempts?.length ? check.attempts : [check])
      .filter((a) => a.exitCode === 0 && Number.isFinite(a.durationMs))
      .map((a) => a.durationMs);
    if (!passing.length) continue;
    const durationMs = Math.max(...passing);
    if (durationMs * 100 > timeoutMs * BUDGET_WARNING_PERCENT)
      warnings.push({
        check: check.id,
        durationMs,
        timeoutMs,
        percent: Math.round((durationMs / timeoutMs) * 10000) / 100,
      });
  }
  return warnings;
}
// Writes budget-warnings.json beside the receipt (an empty list when nothing
// warns) and prints one line per warned check. It is advisory: it is not part
// of the receipt, and a failure to write it is printed, never thrown. Whatever
// removes the receipt removes the list with it.
const BUDGET_WARNINGS_FILE = "budget-warnings.json";
export function reportBudgetWarnings(checks, output, print = () => {}) {
  const warnings = budgetWarnings(checks);
  try {
    writeFileSync(
      join(output, BUDGET_WARNINGS_FILE),
      JSON.stringify(warnings, null, 2) + "\n",
      { mode: 0o600 },
    );
  } catch (error) {
    print("matrix run: could not write budget-warnings.json: " + error.message);
  }
  for (const w of warnings)
    print(
      `matrix run: budget warning: ${w.check} passed in ${w.durationMs} ms, ${w.percent}% of its ${w.timeoutMs} ms timeout`,
    );
  return warnings;
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
  provisionWorktreePrerequisites(plan.checks, cwd, options);
  readPrerequisites(plan.checks, cwd, options.readPrerequisiteFile);
  const wasmOrder = wasmOrderCause(plan.checks);
  if (wasmOrder) throw new Error("WASM check order: " + wasmOrder);
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

// The binaries are built by a child whose whole environment is the
// verifier's: its HOME, Go caches and toolchain, never the user's. The child
// leads its own process group, reported through onGroup like a check's, so an
// abort stops go build and the compilers it started, not only the child.
function buildBinariesInVerifierEnvironment(cwd, output, environment, { historical, abortSignal, onGroup }) {
  const helper = new URL("../tests/test-binaries.mjs", import.meta.url).href;
  const source =
    `const [cwd, output, commit] = process.argv.slice(1);` +
    `const m = await import(${JSON.stringify(helper)});` +
    `const binaries = await m.buildMatrixBinaries(cwd, output);` +
    `if (commit) binaries.push(await m.buildHistoricalHub(cwd, output + "/tailterm-historical-hub-test", commit));` +
    `process.stdout.write(JSON.stringify(binaries));`;
  return new Promise((done, fail) => {
    if (abortSignal?.aborted) return fail(new Error("Verification interrupted by " + abortSignal.reason));
    const child = spawn(process.execPath,
      ["--input-type=module", "-e", source, cwd, output, ...(historical ? [historicalHubCommit] : [])],
      { cwd, env: environment, detached: true, stdio: ["ignore", "pipe", "pipe"] });
    let stdout = "", stderr = "", closed = false, killed = false, settled = false, grouped = false;
    let killTimer, giveUpTimer;
    const signalGroup = (kind) => {
      try {
        process.kill(-child.pid, kind);
      } catch (error) {
        if (error.code !== "ESRCH") stderr += "\nGroup signal: " + error.message;
      }
    };
    const groupGone = () => {
      try {
        process.kill(-child.pid, 0);
        return false;
      } catch (error) {
        return error.code === "ESRCH";
      }
    };
    const ungroup = () => {
      if (grouped) onGroup?.(child.pid, false);
      grouped = false;
    };
    const settle = (error, value) => {
      if (settled) return;
      settled = true;
      clearTimeout(killTimer);
      clearTimeout(giveUpTimer);
      abortSignal?.removeEventListener("abort", stop);
      ungroup();
      error ? fail(error) : done(value);
    };
    // Rejected once the child has closed and its group is gone or has been
    // sent SIGKILL; a child that never closes is given up on after a second.
    const interrupted = () => {
      if (closed && (killed || groupGone()))
        settle(new Error("Verification interrupted by " + abortSignal.reason));
    };
    function stop() {
      if (killTimer) return;
      signalGroup("SIGTERM");
      killTimer = setTimeout(() => {
        signalGroup("SIGKILL");
        killed = true;
        interrupted();
        giveUpTimer = setTimeout(() => {
          closed = true;
          interrupted();
        }, 750);
      }, 250);
    }
    if (child.pid) {
      grouped = true;
      onGroup?.(child.pid, true);
    }
    abortSignal?.addEventListener("abort", stop, { once: true });
    child.stdout.setEncoding("utf8").on("data", (data) => (stdout += data));
    child.stderr.setEncoding("utf8").on("data", (data) => (stderr += data));
    child.on("error", (error) => settle(error));
    child.on("close", (code, signal) => {
      closed = true;
      if (abortSignal?.aborted) return interrupted();
      if (code !== 0)
        return settle(new Error(`Test binary preparation failed (${signal || "exit " + code}): ${stderr.trim()}`));
      try {
        settle(null, JSON.parse(stdout));
      } catch (error) {
        settle(new Error("Test binary preparation printed no binary list: " + error.message));
      }
    });
    if (abortSignal?.aborted) stop();
  });
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
  // Read before the home exists: only a process older than this can be
  // shown unrelated to the home when its environment cannot be read.
  const homeCreatedMs = Date.now();
  const home = createHome();
  const receiptPath = join(output, receiptName);
  let receiptWritten = false;
  try {
    // Before anything runs in the home, so a killed run leaves it marked.
    writeHomeMarker(home, homeMarkerContext(plan, output), { keepHome, output });
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
      PATH: installAgentCliStubs(home, output) + ":" + process.env.PATH,
      HOME: home,
      TMPDIR: home,
      TMUX_TMPDIR: join(home, "tmux"),
      GOPATH: join(home, "go"),
      VERIFICATION_KEEP_HOME: keepHome ? "1" : "0",
      LANG: "en_US.UTF-8",
      CI: "1",
      VERIFICATION_JOBS: String(jobs),
    };
    mkdirSync(environment.TMUX_TMPDIR, { recursive: true, mode: 0o700 });
    // Go modules, toolchain and build cache: shared under the harness cache
    // root when the plan can run Go, else per-home as before.
    const caches = await prepareVerificationCaches({
      plan, cwd, home, output, environment,
      options: { ...options.caches, abortSignal },
    });
    Object.assign(environment, caches.environment);
    if (caches.line) (options.hostLock?.print ?? console.log)(caches.line);
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
      const binaries = await buildBinariesInVerifierEnvironment(cwd, output, environment, {
        historical: plan.checks.some((check) => check.id.startsWith("tests/board-audit-old-hub-browser.mjs")),
        abortSignal,
        onGroup,
      });
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
    // Each WASM reader records in its result the WASM it loads, hashed just
    // before it runs. With wasm-test-build in the plan that must be what the
    // build wrote; without it, the prerequisites read before the run.
    const buildsWasm = plan.checks.some((check) => check.id === "wasm-test-build");
    const readsWasm = plan.checks.some(isWasmReader);
    const wasmPaths = buildsWasm
      ? WASM_FILES
      : prerequisites.filter((p) => WASM_FILES.includes(p.path)).map((p) => p.path);
    const hashFiles = (paths, fail) =>
      Promise.all(paths.map(async (path) => {
        try {
          return { path, sha256: await fileHash(join(cwd, path)) };
        } catch (error) {
          throw new Error(fail(path) + ": " + error.message, { cause: error });
        }
      }));
    const hashWasm = (fail) => hashFiles(wasmPaths, fail);
    // The module list the build rewrote, when the plan carries it.
    const modulePaths = buildsWasm && prerequisites.some((p) => p.path === GO_MODULES) ? [GO_MODULES] : [];
    const wasmChanged = buildsWasm
      ? "WASM changed after wasm-test-build: "
      : "WASM prerequisite changed during verification: ";
    // Undefined until wasm-test-build passes; a failed build leaves the
    // readers recording what they load with nothing to compare it to.
    let builtWasm, builtModules = [], wasmBuildEnded = false;
    const expectedWasm = () =>
      buildsWasm ? builtWasm : prerequisites.filter((p) => WASM_FILES.includes(p.path));
    const checkWasm = (loaded) => {
      const expected = expectedWasm();
      for (const entry of expected ? loaded : [])
        if (expected.find((e) => e.path === entry.path)?.sha256 !== entry.sha256)
          throw new Error(wasmChanged + entry.path);
    };
    const results = await runScheduled(
      plan.checks,
      { jobs, abortSignal },
      async (check, _index, signal) => {
        let wasm;
        if (isWasmReader(check) && wasmPaths.length) {
          if (buildsWasm && !wasmBuildEnded)
            throw new Error("WASM check order: " + check.id + " runs before wasm-test-build");
          wasm = await hashWasm((path) => `Unreadable WASM before ${check.id}: ${path}`);
          checkWasm(wasm);
        }
        const result = await runCheckWithAttempts(check, plan, cwd, output, { ...environment, ...caches.checkEnvironment?.[check.id] }, {
          abortSignal: signal,
          minFreeBytes,
          getAvailableBytes,
          onGroup,
        });
        if (check.id === "wasm-test-build") {
          // Hashed after the final attempt, so a retried build records the
          // files its passing attempt wrote.
          if (readsWasm && result.exitCode === 0) {
            const missing = (path) => "wasm-test-build passed without writing " + path;
            builtWasm = await hashWasm(missing);
            builtModules = await hashFiles(modulePaths, missing);
          }
          wasmBuildEnded = true;
        }
        return wasm ? { ...result, wasm } : result;
      },
    );
    if (abortSignal?.aborted)
      throw new Error("Verification interrupted by " + abortSignal.reason);
    if (needsBinaries) {
      const manifest = JSON.parse(readFileSync(environment.TAILTERM_TEST_BINARIES, "utf8"));
      for (const binary of manifest.binaries)
        if ((await fileHash(binary.path)) !== binary.sha256)
          throw new Error("Prepared test binary changed during verification: " + binary.path);
    }
    if (readsWasm && wasmPaths.length && expectedWasm())
      checkWasm(await hashWasm((path) => wasmChanged + path));
    for (const [i, entry] of (await hashFiles(builtModules.map((b) => b.path), (path) => wasmChanged + path)).entries())
      if (entry.sha256 !== builtModules[i].sha256) throw new Error(wasmChanged + entry.path);
    // The hub keeps a prerequisite's sha256, so a built entry names the file
    // the checks used and keeps the pre-run digest as priorSha256.
    if (builtWasm)
      for (const [i, p] of prerequisites.entries()) {
        const built = [...builtWasm, ...builtModules].find((b) => b.path === p.path);
        if (built)
          prerequisites[i] = { path: p.path, sha256: built.sha256, priorSha256: p.sha256, source: "wasm-test-build" };
      }
    if (abortSignal?.aborted)
      throw new Error("Verification interrupted by " + abortSignal.reason);
    checkClean(cwd, plan.commit);
    const receipt = makeReceipt({ environment, prerequisites, checks: results });
    writeFileSync(receiptPath, JSON.stringify(receipt, null, 2) + "\n", {
      mode: 0o600,
    });
    receiptWritten = true;
    reportBudgetWarnings(results, output, options.hostLock?.print);
    return receipt;
  } finally {
    const cleanupFailed = (what, error) => {
      if (receiptWritten)
        for (const path of [receiptPath, join(output, BUDGET_WARNINGS_FILE)])
          rmSync(path, { force: true });
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
    // Each sweep of one stop reports an unreadable process again.
    const unreadable = new Map();
    let stopped;
    try {
      stopped = await stopHomeProcesses(home, undefined, {
        homeCreatedMs,
        onUnreadable: (entry) => unreadable.set(entry.pid, entry),
      });
    } catch (error) {
      throw cleanupFailed("stop processes under", error);
    } finally {
      if (unreadable.size)
        try {
          writeFileSync(
            join(output, "home-sweep-unreadable.json"),
            JSON.stringify(
              { home, count: unreadable.size, processes: [...unreadable.values()].slice(0, 200) },
              null,
              2,
            ) + "\n",
            { mode: 0o600 },
          );
        } catch {}
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
  assertBinarySuites(matrix, cwd);
  assertFastForward(cwd, context.baseCommit, context.commit, { targeted: true });
  const changed = diffPaths(cwd, context.baseCommit, context.commit);
  const checks = selectChecks(
    matrix,
    [],
    changed,
    candidateGoPackages(cwd, context.commit),
    { targeted: true },
  );
  for (const check of checks)
    if (isGoCheck(check))
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

// A run or targeted command starts the real run as a detached process before
// it joins the host waitlist, and then only follows its log. The detached run
// owns the waitlist place, so stopping the command that started it (a tool
// call reaching its time limit) no longer withdraws the run
// (wi_912a9789357529d4). The command stays one process when it is that
// detached run (TAILTERM_MATRIX_RUN_CHILD=1), when it is the deployer's run,
// whose pid the release runner tracks (TAILTERM_MATRIX_RELEASE set), when the
// caller opts out (TAILTERM_MATRIX_FOREGROUND=1), or when the output directory
// is not outside the worktree, which the run itself refuses.
const RUNNER_FOLLOW_MS = 200;
// How long each tt call of a withdrawal report may take.
const WITHDRAWAL_REPORT_MS = 15000;
// The launcher creates the output directory, so it only starts when that is
// outside the worktree both as written (the run's own test) and through any
// symbolic link on the way to it.
function outsideWorktree(output) {
  const outside = (cwd, path) => relative(cwd, path).split("/")[0] === "..";
  if (!outside(process.cwd(), output)) return false;
  let existing = output,
    rest = "";
  while (!existsSync(existing) && dirname(existing) !== existing) {
    rest = join(basename(existing), rest);
    existing = dirname(existing);
  }
  try {
    return outside(realpathSync(process.cwd()), join(realpathSync(existing), rest));
  } catch {
    return false;
  }
}
function launchesDetached(mode, file, output, environment = process.env) {
  return (
    (mode === "run" || mode === "targeted") &&
    Boolean(file && output) &&
    environment.TAILTERM_MATRIX_RUN_CHILD !== "1" &&
    environment.TAILTERM_MATRIX_RELEASE === undefined &&
    environment.TAILTERM_MATRIX_FOREGROUND !== "1" &&
    outsideWorktree(resolve(output))
  );
}
function readRunnerRecord(output) {
  try {
    return JSON.parse(readFileSync(join(output, "runner.json"), "utf8"));
  } catch {
    return undefined;
  }
}
function writeRunnerRecord(output, record) {
  const path = join(output, "runner.json");
  writeFileSync(path + "." + process.pid, JSON.stringify(record, null, 2) + "\n", { mode: 0o600 });
  renameSync(path + "." + process.pid, path);
}
// Signal 0 probes without signalling; a pid we may not signal is alive.
function pidAlive(pid) {
  try {
    process.kill(pid, 0);
    return true;
  } catch (error) {
    return error.code === "EPERM";
  }
}
// The recorded run is still the one to follow: no exit recorded, its pid is
// alive, and that pid is a matrix runner rather than a reused number.
function recordedRunAlive(record) {
  if (!Number.isSafeInteger(record?.pid) || record.pid <= 0 || record.exitCode !== undefined)
    return false;
  if (!pidAlive(record.pid)) return false;
  const ps = spawnSync("ps", ["-p", String(record.pid), "-o", "command="], { encoding: "utf8" });
  return ps.status === 0 && ps.stdout.includes("verify-matrix.mjs");
}
// How long a launcher waits for another launcher's decision on one directory.
const LAUNCH_MUTEX_WAIT_MS = 5000;
const LAUNCH_MUTEX_POLL_MS = 50;
// Starts the run detached, or re-attaches to the one the output directory
// already records, follows its log, and resolves to the exit code. Reading the
// record, deciding and starting happen under a mutex beside the record, so
// launchers started together start one run and the rest re-attach or are
// refused (wi_19db6841a2a50d57).
async function launchDetached(argv, output) {
  // Created here, with the default mode, before the mutex would create it.
  mkdirSync(output, { recursive: true });
  const logPath = join(output, "runner.log");
  const paths = lockPaths(join(output, "runner.json"));
  let token;
  for (const deadline = Date.now() + LAUNCH_MUTEX_WAIT_MS; ; ) {
    const busy = {};
    if ((token = takeMutex(paths, {}, busy))) break;
    if (Date.now() >= deadline)
      throw new Error(
        `Another matrix launcher${Number.isSafeInteger(busy.owner?.pid) ? ` (pid ${busy.owner.pid})` : ""} ` +
          `holds ${paths.mutex}; no run was started. If no launcher for this output directory is running, remove that file and run the command again`,
      );
    await new Promise((resolve) => setTimeout(resolve, LAUNCH_MUTEX_POLL_MS));
  }
  let offset, pid, child, ended;
  try {
    const recorded = readRunnerRecord(output);
    offset = existsSync(logPath) ? statSync(logPath).size : 0;
    if (recordedRunAlive(recorded)) {
      // Only the same command from the same directory follows that run; any
      // other is refused as the host lock refuses a directory already in use.
      if (recorded.cwd !== process.cwd() || JSON.stringify(recorded.argv) !== JSON.stringify(argv))
        throw new Error(
          `Host lock output/record directory is already in use by a different matrix run (pid ${recorded.pid})`,
        );
      pid = recorded.pid;
      console.log(`matrix run: re-attached to pid ${pid}`);
    } else {
      const log = openSync(logPath, "a", 0o600);
      try {
        child = spawn(process.execPath, [fileURLToPath(import.meta.url), ...argv], {
          cwd: process.cwd(),
          detached: true,
          stdio: ["ignore", log, log],
          env: { ...process.env, TAILTERM_MATRIX_RUN_CHILD: "1" },
        });
      } finally {
        closeSync(log);
      }
      child.on("error", (error) => (ended ??= { error }));
      child.on("exit", (code, signal) => (ended ??= { code, signal }));
      if (!child.pid) {
        await new Promise((resolve) => setImmediate(resolve));
        throw new Error("Could not start the detached matrix run: " + (ended?.error?.message || "no pid"));
      }
      pid = child.pid;
      writeRunnerRecord(output, {
        version: 1,
        pid,
        startedAt: new Date().toISOString(),
        argv,
        cwd: process.cwd(),
        log: logPath,
      });
      child.unref();
      console.log(
        `matrix run: detached as pid ${pid}; log ${logPath}; it keeps running if this command is stopped. ` +
          `Run the same command to re-attach; stop the run with: kill ${pid}`,
      );
    }
  } finally {
    releaseMutex(paths, token);
  }
  const logFile = openSync(logPath, "r");
  const buffer = Buffer.alloc(65536);
  const drain = () => {
    for (;;) {
      const read = readSync(logFile, buffer, 0, buffer.length, offset);
      if (!read) return;
      offset += read;
      process.stdout.write(buffer.subarray(0, read));
    }
  };
  let stopped = "";
  const handlers = {
    // Only an interactive cancel stops the run; it then ends through its own
    // interruption path and this command reports its exit code.
    SIGINT: () => {
      try {
        process.kill(pid, "SIGINT");
      } catch {}
    },
    SIGTERM: () => (stopped ||= "SIGTERM"),
    SIGHUP: () => (stopped ||= "SIGHUP"),
  };
  for (const [signal, handler] of Object.entries(handlers)) process.on(signal, handler);
  try {
    for (;;) {
      const over = child ? ended : !pidAlive(pid);
      drain();
      if (stopped) {
        console.log(
          `matrix run: this command was stopped by ${stopped}; the run continues as pid ${pid}, log ${logPath}`,
        );
        return stopped === "SIGHUP" ? 129 : 143;
      }
      if (over) break;
      await new Promise((resolve) => setTimeout(resolve, RUNNER_FOLLOW_MS));
    }
  } finally {
    closeSync(logFile);
    for (const [signal, handler] of Object.entries(handlers)) process.off(signal, handler);
  }
  if (child && Number.isInteger(ended.code)) return ended.code;
  const code = child ? undefined : readRunnerRecord(output)?.exitCode;
  if (Number.isInteger(code) && readRunnerRecord(output)?.pid === pid) return code;
  console.log(
    `matrix run: pid ${pid} ended without recording an exit code` +
      (ended?.signal ? ` (killed by ${ended.signal})` : ""),
  );
  return 1;
}
// The detached run records how it ended for a command that re-attached.
function recordRunnerExit(output) {
  process.on("exit", (code) => {
    try {
      // Only its own record: another run may have been started for the same
      // directory, and the host lock refuses one of the two.
      const record = readRunnerRecord(output);
      if (record?.pid === process.pid)
        writeRunnerRecord(output, { ...record, exitCode: code, endedAt: new Date().toISOString() });
    } catch {}
  });
}
function reportingTT(args, what) {
  const result = spawnSync("tt", args, {
    encoding: "utf8",
    stdio: ["ignore", "pipe", "pipe"],
    timeout: WITHDRAWAL_REPORT_MS,
    killSignal: "SIGKILL",
  });
  if (result.error)
    throw new Error(
      result.error.code === "ETIMEDOUT"
        ? `${what} timed out after ${WITHDRAWAL_REPORT_MS} ms`
        : `${what} could not run: ${result.error.message}`,
    );
  if (result.status !== 0)
    throw new Error(
      `${what} failed: ` +
        ((result.stderr || "").trim().split("\n").pop() ||
          (result.signal ? "signal " + result.signal : "exit " + result.status)),
    );
  return result.stdout;
}
// The hub's reason for refusing a directed message whose recipient is its
// sender (selfAddressedMessage in hub/internal/store/store.go).
const SELF_ADDRESSED_REFUSAL = "a directed message to yourself wakes nobody";
// A run withdrawn by a signal while it was queued tells its item lead why, so
// the loss is not only in its own log directory. The deployer reports its own
// runs and a person's shell has no agent identity: neither sends anything. The
// place is already released when this runs, and a failed report is recorded
// and never retried. A lead that launched the run itself has nobody to tell:
// the hub refuses the notice and that is recorded as a skip.
function reportWithdrawal({ output, signal, item, commit }, environment = process.env) {
  if (environment.TAILTERM_MATRIX_RELEASE !== undefined) return;
  if (!environment.TAILTERM_AGENT || !environment.TAILTERM_TASK) return;
  let sidecar = {};
  try {
    sidecar = JSON.parse(readFileSync(join(output, "host-lock.json"), "utf8"));
  } catch {}
  const report = {
    version: 1,
    signal,
    waitMs: sidecar.waitMs ?? null,
    queuePosition: sidecar.queuePosition ?? null,
  };
  let binding;
  try {
    binding = JSON.parse(reportingTT(["context", "--json"], "the context lookup"))?.binding;
  } catch {}
  const linked =
    typeof binding?.itemId === "string" &&
    Number.isSafeInteger(binding.itemRevision) &&
    Number.isSafeInteger(binding.workOrderMessage?.seq);
  const named = item || (linked ? binding.itemId : "") || sidecar.item;
  const agent = environment.TAILTERM_AGENT_NAME || environment.TAILTERM_AGENT;
  const text =
    `${agent}'s matrix run` +
    (named && named !== "unknown" ? ` for ${named}` : "") +
    (commit ? ` at ${String(commit).slice(0, 12)}` : "") +
    ` was withdrawn by ${signal}` +
    (Number.isFinite(sidecar.waitMs) ? ` after waiting ${Math.round(sidecar.waitMs / 60000)} min` : "") +
    (sidecar.queuePosition ? `, joined at position ${sidecar.queuePosition}` : "") +
    `; no check ran and no receipt exists. Logs: ${output}. Start it again with the same command.`;
  try {
    const sent = reportingTT(
      [
        "send", "--kind", "notice", "--to", "role:lead",
        "--subject", "A queued matrix run was withdrawn by a signal",
        "--text", text,
        ...(linked
          ? [
              "--work-item", binding.itemId,
              "--work-item-revision", String(binding.itemRevision),
              "--work-order-message", String(binding.workOrderMessage.seq),
            ]
          : []),
      ],
      "the notice",
    );
    const seq = /#(\d+)/.exec(sent);
    Object.assign(report, { reported: true }, seq ? { messageSeq: Number(seq[1]) } : {});
    console.log("matrix run: reported the withdrawal to the lead");
  } catch (error) {
    // The launcher is the item's lead: the hub refuses a message to its own
    // sender, which would wake nobody. Nobody else is owed the notice, so this
    // is a skip, not a failed report.
    if (error.message.includes(SELF_ADDRESSED_REFUSAL)) {
      Object.assign(report, { reported: false, skipped: "the launcher is the item's lead" });
      console.log("matrix run: the launcher is the item's lead; no withdrawal notice was sent");
    } else {
      Object.assign(report, { reported: false, reason: error.message });
      console.log("matrix run: could not report the withdrawal to the lead: " + error.message);
    }
  }
  try {
    writeFileSync(join(output, "withdrawal-report.json"), JSON.stringify(report, null, 2) + "\n", { mode: 0o600 });
  } catch {}
}

// The go-race check's command when the matrix approves shards. A package list
// without any shard package (and without ./...) is one plain go test -race, as
// before. Otherwise, before any test runs, every shard package the list tests
// is listed and partitioned (the preflight). Then one pool runs the jobs in
// queue order, at most the job count at once: the other packages as one
// go test -race -p=1 (one race test process at a time), then every shard of
// each shard package in flag order. After a job fails no further job starts;
// running jobs finish. Two serial phases, the other packages and then the
// shards, put both slow packages on the critical path and timed out twice on
// 2026-10-15. Exit 0 needs every job to pass, go test -list to agree with the
// source rule, and every listed test to have started exactly once across its
// package's shards.
export function parseGoRaceArgs(args) {
  const usage = () =>
    Object.assign(new Error("Usage: " + GO_RACE_USAGE), { exitCode: 2 });
  let timeout;
  const shards = [];
  const first = args.findIndex((arg) => arg.startsWith("./"));
  if (first < 0 || args.slice(first).some((arg) => !arg.startsWith("./")))
    throw usage();
  for (const flag of args.slice(0, first)) {
    const found = GO_RACE_SHARDS_FLAG.exec(flag);
    if (GO_RACE_TIMEOUT_FLAG.test(flag) && !timeout) timeout = flag;
    else if (
      found &&
      found[1] !== "./..." &&
      !shards.some((s) => s.package === found[1])
    )
      shards.push({ package: found[1], k: Number(found[2]) });
    else throw usage();
  }
  return { timeout, shards, packages: args.slice(first) };
}
export async function runGoRace(
  args,
  {
    cwd = process.cwd(),
    env = process.env,
    abortSignal,
    write = (text) => process.stdout.write(text),
  } = {},
) {
  const { timeout, shards, packages } = parseGoRaceArgs(args);
  const flags = timeout ? [timeout] : [];
  const children = new Set();
  const forward = () => {
    for (const child of children) child.kill(abortSignal.reason);
  };
  abortSignal?.addEventListener("abort", forward);
  const interrupted = () =>
    abortSignal?.aborted ? (abortSignal.reason === "SIGINT" ? 130 : 143) : 0;
  // output "inherit" passes the command's output straight through; otherwise
  // onLine receives each stdout line and stderr is returned.
  const go = (goArgs, onLine) =>
    new Promise((done) => {
      if (abortSignal?.aborted)
        return done({ code: interrupted(), stderr: "", seconds: 0 });
      const started = Date.now();
      const child = spawn("go", goArgs, {
        cwd,
        env,
        stdio: ["ignore", onLine ? "pipe" : "inherit", onLine ? "pipe" : "inherit"],
      });
      children.add(child);
      let pending = "",
        stderr = "";
      child.stdout?.setEncoding("utf8").on("data", (data) => {
        const lines = (pending + data).split("\n");
        pending = lines.pop();
        lines.forEach(onLine);
      });
      child.stderr?.setEncoding("utf8").on("data", (data) => (stderr += data));
      const end = (code) => {
        if (!children.delete(child)) return;
        if (pending) onLine(pending);
        done({ code, stderr, seconds: (Date.now() - started) / 1000 });
      };
      child.on("error", (error) => {
        stderr += error.message + "\n";
        end(-1);
      });
      child.on("close", (code) => end(code ?? -1));
    });
  const failed = (code) => interrupted() || (code > 0 ? code : 1);
  try {
    const all = packages.includes("./...");
    const sharded = shards.filter((s) => all || packages.includes(s.package));
    if (!sharded.length) {
      const { code } = await go(["test", "-race", ...flags, ...packages]);
      return code === 0 ? interrupted() : failed(code);
    }
    let tested = [...new Set(packages)];
    if (all) {
      const listed = [];
      const { code, stderr } = await go(
        ["list", "-f", "{{.Dir}}", "./..."],
        (line) => line && listed.push(line),
      );
      if (code !== 0) {
        write(stderr);
        return failed(code);
      }
      const root = realpathSync(cwd);
      tested = [
        ...new Set([
          ...listed.map((dir) => "./" + relative(root, realpathSync(dir))),
          ...packages.filter((name) => name !== "./..."),
        ]),
      ];
    }
    const others = tested.filter(
      (name) => !sharded.some((s) => s.package === name),
    );
    const active = sharded.filter((s) => tested.includes(s.package));

    // The preflight: every shard package is listed and partitioned before any
    // test runs, so a disagreement stops the check with nothing started.
    const prepared = [];
    for (const spec of active) {
      const directory = join(cwd, spec.package);
      const source = goRaceTestNames(
        readdirSync(directory)
          .filter((name) => name.endsWith("_test.go"))
          .sort()
          .map((name) => readFileSync(join(directory, name), "utf8")),
      );
      const names = [];
      let listOutput = "";
      const list = await go(
        ["test", "-race", "-list", ".", spec.package],
        (line) => {
          listOutput += line + "\n";
          if (/^(Test|Fuzz|Example)\w*$/.test(line)) names.push(line);
        },
      );
      if (list.code !== 0) {
        write(listOutput + list.stderr);
        return failed(list.code);
      }
      const only = (from, other) =>
        [...new Set(from)].filter((name) => !other.includes(name)).sort();
      const sourceOnly = only(source, names),
        listOnly = only(names, source);
      if (sourceOnly.length || listOnly.length)
        throw new Error(
          `go-race: the test source of ${spec.package} and go test -list disagree` +
            (sourceOnly.length ? `; only in the source: ${sourceOnly.join(", ")}` : "") +
            (listOnly.length ? `; only in go test -list: ${listOnly.join(", ")}` : ""),
        );
      const { parts, digest: partitionDigest } = goRacePartition(source, spec.k);
      assertGoRacePartition(names, parts);
      const line = goRaceShardsLine(spec, names.length, partitionDigest);
      write(line + "\n");
      prepared.push({
        spec,
        names,
        parts,
        line,
        selectors: goRaceShardSelectors(parts),
        counts: new Map(),
      });
    }
    if (interrupted()) return interrupted();

    // The queue: the other packages' job first, then each package's shards.
    const queue = [
      ...(others.length ? [{ others: true }] : []),
      ...prepared.flatMap((p) =>
        p.selectors.map((selector, i) => ({ p, i, selector })),
      ),
    ];
    const jobs = Number(env.VERIFICATION_JOBS);
    const limit = Math.min(
      queue.filter((job) => job.others || job.selector).length,
      Number.isSafeInteger(jobs) && jobs > 0 ? jobs : defaultJobs(),
    );
    const shardLabel = (job) => `shard ${job.p.spec.package} ${job.i + 1}/${job.p.spec.k}`;
    let running = 0,
      peak = 0,
      next = 0,
      printed = 0,
      stopped = false;
    // Each job's output is printed whole, in queue order, as soon as it and
    // every earlier job have ended. The other packages' job prints straight
    // through and is first, so no shard output is printed while it runs.
    const print = () => {
      for (; printed < queue.length && queue[printed].result; printed++) {
        const job = queue[printed],
          r = job.result;
        write(
          job.others
            ? `go-race other packages: ${others.length} packages, ${r.seconds.toFixed(1)} s, exit ${r.code}\n`
            : r.text +
                `go-race ${shardLabel(job)}: ${r.tests} tests, ${r.seconds.toFixed(1)} s, exit ${r.code}` +
                (r.started ? "\n" : " (not started: no test hashes to it)\n"),
        );
      }
    };
    const runJob = async (job) => {
      if (job.others) {
        const { code, seconds } = await go(["test", "-race", "-p=1", ...flags, ...others]);
        job.result = { code, seconds };
        return;
      }
      if (!job.selector) {
        job.result = { text: "", tests: 0, seconds: 0, code: 0, started: false };
        return;
      }
      peak = Math.max(peak, ++running);
      const text = [],
        own = new Map();
      const { code, stderr, seconds } = await go(
        ["test", "-race", "-json", "-count=1", ...flags, ...job.selector, job.p.spec.package],
        (jsonLine) => {
          let event;
          try {
            event = JSON.parse(jsonLine);
          } catch {
            text.push(jsonLine + "\n");
            return;
          }
          if (typeof event?.Output === "string") text.push(event.Output);
          goRaceRunCounts([jsonLine], own);
        },
      );
      running--;
      for (const [name, count] of own)
        job.p.counts.set(name, (job.p.counts.get(name) ?? 0) + count);
      job.result = {
        text: text.join("") + stderr,
        tests: own.size,
        seconds,
        code,
        started: true,
      };
    };
    await Promise.all(
      Array.from({ length: limit }, async () => {
        while (next < queue.length && !stopped && !interrupted()) {
          const job = queue[next++];
          await runJob(job);
          if (job.result.code !== 0) stopped = true;
          print();
        }
      }),
    );
    if (interrupted()) return interrupted();
    // A package whose shards all ran gets its end line; after a failure the
    // shards the queue never reached are not run, not missing.
    const complete = prepared.filter((p) =>
      queue.every((job) => job.p !== p || job.result),
    );
    for (const p of complete) {
      const ranOnce = p.names.filter((name) => p.counts.get(name) === 1).length;
      write(
        `${p.line.replace(" partition ", ` ran-once=${ranOnce} partition `)} peak-shard-processes=${peak}\n`,
      );
    }
    const unstarted = queue.filter((job) => !job.result).length;
    if (unstarted) write(`go-race: ${unstarted} jobs not started after a failure\n`);
    const othersJob = queue.find((job) => job.others);
    if (othersJob && othersJob.result.code !== 0)
      return failed(othersJob.result.code);
    // A shard that exited before running its own tests is the failure to
    // report: its identity and exit first, then a bounded sample of what it
    // left unrun, instead of every missing name.
    const early = queue
      .filter((job) => job.p && job.result?.started && job.result.code !== 0)
      .map((job) => ({
        job,
        unrun: job.p.parts[job.i].filter((name) => !job.p.counts.has(name)),
      }))
      .filter(({ unrun }) => unrun.length);
    if (early.length) {
      // The tests of a reported shard, or of one the queue never reached,
      // are not counted as not run in other shards.
      const theirs = (p) =>
        new Set(
          queue
            .filter((job) => job.p === p && (!job.result || early.some((e) => e.job === job)))
            .flatMap((job) => p.parts[job.i]),
        );
      const sum = (count) => prepared.reduce((total, p) => total + count(p), 0);
      const other = [
        [
          sum((p) => {
            const excused = theirs(p);
            return p.names.filter((name) => !excused.has(name) && !p.counts.has(name)).length;
          }),
          "not run in other shards",
        ],
        [sum((p) => [...p.counts.values()].filter((count) => count > 1).length), "run more than once"],
        [sum((p) => [...p.counts.keys()].filter((name) => !p.names.includes(name)).length), "unlisted"],
      ].filter(([count]) => count);
      throw Object.assign(
        new Error(
          [
            ...early.map(
              ({ job, unrun }) =>
                `go-race: ${shardLabel(job)} ${job.result.code < 0 ? "ended without an exit code" : `exited ${job.result.code}`}` +
                ` before running ${unrun.length} of its ${job.p.parts[job.i].length} tests`,
            ),
            ...early.map(
              ({ job, unrun }) =>
                `${shardLabel(job)} not run (first ${Math.min(10, unrun.length)} of ${unrun.length}): ` +
                unrun.slice(0, 10).join(", "),
            ),
            ...(other.length
              ? ["other coverage problems: " + other.map(([count, what]) => `${count} ${what}`).join(", ")]
              : []),
          ].join("\n"),
        ),
        { exitCode: failed(early[0].job.result.code) },
      );
    }
    for (const p of complete) assertRanExactlyOnce(p.names, p.counts);
    const bad = queue.find((job) => job.result && job.result.code !== 0);
    return bad ? failed(bad.result.code) : 0;
  } finally {
    abortSignal?.removeEventListener("abort", forward);
  }
}

// The flags of a run or targeted command. The launcher reads them before it
// starts anything and the run reads them again, so both refuse the same ones.
function parseRunFlags(flags) {
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
    } else if (flags[i] === "--min-free-bytes") {
      const value = flags[++i];
      if (!/^(0|[1-9][0-9]*)$/.test(value || ""))
        throw new Error("--min-free-bytes requires a nonnegative integer byte count");
      minFreeBytes = Number(value);
      if (!Number.isSafeInteger(minFreeBytes))
        throw new Error("--min-free-bytes requires a safe integer byte count");
    } else throw new Error("Unknown verifier run option: " + flags[i]);
  }
  return { keepHome, minFreeBytes, jobs, priority, maxWaitMs, item };
}

const invokedDirectly =
  process.argv[1] &&
  resolve(process.argv[1]) === fileURLToPath(import.meta.url);
// The go-race check's own command: it takes no plan file.
if (invokedDirectly && process.argv[2] === "go-race") {
  const interruption = new AbortController();
  for (const signal of ["SIGINT", "SIGTERM"])
    process.on(signal, () => interruption.abort(signal));
  try {
    process.exitCode = await runGoRace(process.argv.slice(3), {
      abortSignal: interruption.signal,
    });
  } catch (e) {
    console.error(e.message);
    process.exitCode = e.exitCode ?? 1;
  }
} else if (invokedDirectly) {
  const [mode, file, output, ...flags] = process.argv.slice(2);
  const launches = launchesDetached(mode, file, output);
  const interruption = new AbortController();
  let interruptedBy = "";
  if (!launches)
    for (const signal of ["SIGINT", "SIGTERM"])
      process.on(signal, () => {
        interruptedBy ||= signal;
        interruption.abort(interruptedBy);
        process.exitCode = signal === "SIGINT" ? 130 : 143;
      });
  if (!launches && output && mode !== "plan" && process.env.TAILTERM_MATRIX_RUN_CHILD === "1")
    recordRunnerExit(resolve(output));
  let input, item;
  try {
    input = JSON.parse(readFileSync(file, "utf8"));
    if (launches) {
      // A command the run would refuse for its flags starts no run and
      // creates nothing. The item's priority lookup stays with the run.
      resolvePriority(parseRunFlags(flags).priority);
      process.exitCode = await launchDetached(process.argv.slice(2), resolve(output));
    }
    else if (mode === "plan") {
      const { plan, preserved, selection, goRaceShards } =
        planWithPreservation(input, process.cwd());
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
      for (const shards of goRaceShards ?? []) console.error(shards.line);
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
      const parsed = parseRunFlags(flags);
      const { keepHome, minFreeBytes, jobs, priority, maxWaitMs } = parsed;
      item = parsed.item;
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
            // The deployer marks its integrated run; only run mode honours it.
            ...(mode === "run" && releaseRun(process.env) ? { release: true } : {}),
            maxWaitMs,
            // The item the priority above was looked up for.
            item: input?.itemId || item,
            print: (line) => console.log(line),
          },
        },
      );
      await new Promise((resolve) => setImmediate(resolve));
      if (interruptedBy)
        for (const name of [
          mode === "run" ? "receipt.json" : "targeted-receipt.json",
          BUDGET_WARNINGS_FILE,
        ])
          rmSync(join(resolve(output), name), { force: true });
      process.exitCode = interruptedBy
        ? interruptedBy === "SIGINT"
          ? 130
          : 143
        : receiptEligible(r)
          ? 0
          : 1;
    } else
      throw new Error(
        "Usage: node scripts/verify-matrix.mjs plan|run|targeted INPUT OUTPUT [--keep-home] [--min-free-bytes N] [--jobs N] [--priority urgent|high|normal] [--host-wait-minutes N] [--item ID]\n       " +
          GO_RACE_USAGE,
      );
  } catch (e) {
    console.error(e.message);
    if (e.code === "withdrawn" && interruptedBy)
      reportWithdrawal({
        output: resolve(output),
        signal: interruptedBy,
        item: input?.itemId || item,
        commit: input?.commit,
      });
    process.exitCode =
      interruptedBy === "SIGINT" ? 130 : interruptedBy === "SIGTERM" ? 143 : (e.exitCode ?? 1);
  }
}
