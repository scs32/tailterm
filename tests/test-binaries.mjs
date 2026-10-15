import { execFile } from "node:child_process";
import { createHash } from "node:crypto";
import { mkdir, mkdtemp, readFile, readdir, realpath, rm, stat } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { promisify } from "node:util";

const exec = promisify(execFile);
const sha256 = (bytes) => createHash("sha256").update(bytes).digest("hex");
// The build environment is read per call, never at import: the matrix runner
// passes the verifier's own environment, so its builds see the run's HOME, Go
// caches and toolchain and never the user's.
export const buildEnvironment = (source = process.env) => Object.fromEntries(
  Object.entries(source).filter(([key]) => !/^(TAILTERM_|CODEX_|TT_|TMUX)/.test(key)));
// Recorded with each build; a fixed list, so no secret reaches the log.
export const GO_ENV_KEYS = ["GOVERSION", "GOROOT", "GOPATH", "GOMODCACHE", "GOCACHE", "GOFLAGS",
  "GOPROXY", "GOTOOLCHAIN", "GOOS", "GOARCH"];

async function sourceFiles(dir, prefix = "") {
  const files = [];
  for (const entry of await readdir(dir, { withFileTypes: true })) {
    const name = path.join(prefix, entry.name);
    if (entry.isDirectory()) files.push(...await sourceFiles(path.join(dir, entry.name), name));
    else if (entry.isFile()) files.push(name);
  }
  return files.sort();
}

export async function sourceIdentity(root) {
  const resolved = await realpath(root);
  const hub = path.join(resolved, "hub");
  const hash = createHash("sha256");
  for (const name of await sourceFiles(hub)) {
    hash.update(name).update("\0");
    hash.update(await readFile(path.join(hub, name))).update("\0");
  }
  return { root: resolved, sha256: hash.digest("hex") };
}

export async function fileHash(file) {
  return sha256(await readFile(file));
}

export async function prepareTestBinary({ root = process.cwd(), target, output, historicalCommit, manifestPath = process.env.TAILTERM_TEST_BINARIES, onBuild, env = buildEnvironment() }) {
  if (!(["hub", "tt"].includes(target))) throw new Error("Unknown test binary target");
  // A passed environment gets the same filter as the inherited one.
  env = buildEnvironment(env);
  const source = await sourceIdentity(root);
  if (manifestPath) {
    const manifest = JSON.parse(await readFile(manifestPath, "utf8"));
    const entry = manifest.binaries?.find((item) =>
      item.target === target && item.historicalCommit === (historicalCommit || null) &&
      (historicalCommit || item.source.root === source.root));
    if (!entry) throw new Error(`No prepared ${target} binary for ${source.root}`);
    if (entry.source.sha256 !== source.sha256) throw new Error(`Source mismatch for ${target} binary`);
    if ((await fileHash(entry.path)) !== entry.sha256) throw new Error(`Binary hash mismatch for ${target}`);
    if (!((await stat(entry.path)).mode & 0o111)) throw new Error(`Binary is not executable: ${entry.path}`);
    return entry.path;
  }
  if (!output) throw new Error("Standalone test binary output required");
  await mkdir(path.dirname(output), { recursive: true });
  const args = ["build", ...(target === "hub" ? ["-ldflags=-s -w"] : []), "-o", output,
    target === "hub" ? "./cmd/tailterm-hub" : "./cmd/tt"];
  const cwd = path.join(source.root, "hub");
  let goEnv;
  try {
    const found = await exec("go", ["env", "-json", ...GO_ENV_KEYS], { cwd, env, maxBuffer: 1024 * 1024 });
    goEnv = JSON.parse(found.stdout);
  } catch (error) {
    const detail = String(error.stderr || error.message).trim().split("\n")[0];
    throw new Error(`go env failed for ${target} build: ${detail}`);
  }
  const result = await exec("go", args, { cwd, env, maxBuffer: 8 * 1024 * 1024 });
  onBuild?.({ argv: ["go", ...args], goEnv, stdout: result.stdout, stderr: result.stderr });
  return output;
}

export async function buildMatrixBinaries(root, output, { env = buildEnvironment() } = {}) {
  const source = await sourceIdentity(root);
  const binaries = [];
  for (const target of ["hub", "tt"]) {
    const file = path.join(output, `tailterm-${target}-test`);
    const startedAt = new Date().toISOString();
    let buildLog;
    const binary = await prepareTestBinary({ root, target, output: file, manifestPath: null, env,
      onBuild: (log) => { buildLog = log; } });
    binaries.push({ target, historicalCommit: null, source, path: binary,
      sha256: await fileHash(binary), buildLog,
      startedAt, endedAt: new Date().toISOString() });
  }
  return binaries;
}

export async function buildHistoricalHub(root, output, commit, { env = buildEnvironment() } = {}) {
  const temporary = await mkdtemp(path.join(tmpdir(), "tailterm-historical-hub-"));
  try {
    const archive = path.join(temporary, "source.tar");
    const sourceRoot = path.join(temporary, "source");
    const { mkdir, writeFile } = await import("node:fs/promises");
    const { stdout } = await exec("git", ["archive", "--format=tar", commit], { cwd: root,
      env, encoding: "buffer", maxBuffer: 128 * 1024 * 1024 });
    await writeFile(archive, stdout);
    await mkdir(sourceRoot);
    await exec("tar", ["-xf", archive, "-C", sourceRoot]);
    const source = await sourceIdentity(sourceRoot);
    const startedAt = new Date().toISOString();
    let buildLog;
    const binary = await prepareTestBinary({ root: sourceRoot, target: "hub", output, manifestPath: null, env,
      onBuild: (log) => { buildLog = log; } });
    return { target: "hub", historicalCommit: commit, source: { ...source, root: null }, path: binary,
      sha256: await fileHash(binary), buildLog: { archive: ["git", "archive", "--format=tar", commit], ...buildLog },
      startedAt, endedAt: new Date().toISOString() };
  } finally {
    await rm(temporary, { recursive: true, force: true });
  }
}
