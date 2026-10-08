// d2: the database handler builds one release job's private input manifest.
//
//   node scripts/release-inputs.mjs --config PRIVATE --job ID [--dry-run]
//
// It selects targets exactly as the deployer does, creates each TrueNAS
// backup through the handler-owned preflight (one plan for hub and bridge
// together, else one per target), pins its receipt, and names rollback
// programs for the probed live releases. It writes
// journalDirectory/ID-inputs.json (0600) once and prints the exact
// `tt deployment inputs` command to import its digest. --dry-run reads the job
// and prints the planned bindings without host calls or writes. Nothing
// printed carries credentials or captured program output.
import { createHash } from "node:crypto";
import { execFileSync } from "node:child_process";
import { existsSync, openSync, readSync, writeFileSync, closeSync, readFileSync, rmSync } from "node:fs";
import { join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { selectReleaseTargets, releaseBaselines, schemaChanged } from "./release-targets.mjs";
import { probe } from "./release-probe.mjs";

const BASE = "/mnt/deepfreeze/tailterm-hub";
const BINARY = { hub: ["binaryDestination", "tailterm-hub"], bridge: ["bridgeBinaryDestination", "tailterm-discord"] };
const sha = s => /^[a-f0-9]{40}$/.test(s || "");
// The SHA-256 of a file's bytes, read through one fixed buffer: a database
// backup outgrows the 2 GiB that a whole-file read allows. The release
// runner hashes its pinned files with this too.
export const FILE_HASH_CHUNK_BYTES = 1024 * 1024;
export function fileSHA256(path) {
  const hash = createHash("sha256"), chunk = Buffer.allocUnsafe(FILE_HASH_CHUNK_BYTES), fd = openSync(path, "r");
  try { for (let n; (n = readSync(fd, chunk, 0, chunk.length, null)) > 0;) hash.update(n === chunk.length ? chunk : chunk.subarray(0, n)); }
  finally { closeSync(fd); }
  return hash.digest("hex");
}
const fileSHA = fileSHA256;
function writePrivate(path, text) {
  const fd = openSync(path, "wx", 0o600);
  try { writeFileSync(fd, text); } finally { closeSync(fd); }
}

export function hostDeps(config, configPath) {
  const quiet = { cwd: config.cwd, encoding: "utf8", stdio: ["ignore", "pipe", "pipe"], maxBuffer: 64 * 1024 * 1024, timeout: 600000 };
  return {
    tt: argv => execFileSync(config.tt || "tt", argv, quiet),
    git: argv => execFileSync("git", argv, quiet),
    probe: argv => probe(argv, config),
    preflight: (plan, receipt) => { execFileSync("python3", ["scripts/truenas_release_preflight.py", "--plan", plan, "--receipt-output", receipt], quiet); },
    copyBackup: (remote, local) => {
      const fd = openSync(local, "wx", 0o600);
      try { execFileSync("ssh", ["-o", "BatchMode=yes", "-o", "ConnectTimeout=10", config.targets.hub.host, `cat ${remote}`], { ...quiet, stdio: ["ignore", fd, "pipe"] }); }
      finally { closeSync(fd); }
    },
    configPath,
  };
}

const SETTLED = new Set(["released", "rolled_back", "refused", "superseded"]);
// Read both views to terminal metadata before exposing any jobs to a caller.
// The same ledger token covers both traversals; a changed ledger holds the poll.
export function readReleaseSummaries(read) {
  const jobs = [], ids = new Set(), rows = new Set(), cursors = new Set();
  let task;
  let snapshot = "", head;
  for (const view of ["active", "settled"]) {
    let after = "", previous = 0;
    for (;;) {
      const argv = ["deployment", "list", "--view", view, "--limit", "200", ...(after ? ["--after", after] : []), ...(snapshot ? ["--snapshot", snapshot] : [])];
      const result = JSON.parse(read(argv)), p = result?.page;
      if (result?.version !== 1 || !Array.isArray(result.jobs) || !p || p.view !== view || !Number.isSafeInteger(p.limit) || p.limit < 1 || p.limit > 200 || result.jobs.length > p.limit || !/^[a-f0-9]{64}$/.test(p.snapshot || "") || (snapshot && snapshot !== p.snapshot) || typeof p.nextAfter !== "string") throw new Error("Invalid release page");
      if (!snapshot) head = result.jobs;
      snapshot = p.snapshot;
      for (const j of result.jobs) {
        if (j?.summary !== true || typeof j.id !== "string" || !/^[A-Za-z0-9_-]+$/.test(j.id) || !Number.isSafeInteger(j.rowId) || j.rowId <= previous || ids.has(j.id) || rows.has(j.rowId) || typeof j.state !== "string" || SETTLED.has(j.state) !== (view === "settled")) throw new Error("Invalid release summary");
        if (j.taskId !== undefined) { if (task !== undefined && j.taskId !== task) throw new Error("Release page task changed"); task = j.taskId; }
        ids.add(j.id); rows.add(j.rowId); previous = j.rowId; jobs.push(j);
      }
      if (!p.nextAfter) break;
      if (!result.jobs.length || cursors.has(p.nextAfter)) throw new Error("Repeating release cursor");
      cursors.add(p.nextAfter); after = p.nextAfter;
    }
  }
  for (const job of jobs) { Object.defineProperty(job, "releaseSnapshot", {value:snapshot}); Object.defineProperty(job, "releaseHead", {value:head}); }
  Object.defineProperty(jobs, "releaseSnapshot", {value:snapshot});
  Object.defineProperty(jobs, "releaseHead", {value:head});
  return jobs.sort((a,b) => a.rowId-b.rowId);
}
// Compare every compact identity/pin and the projected receipt, including
// omitted optional fields. A valid full detail may carry additional evidence.
export function releaseProjection(job) {
  const keys = ["id", "taskId", "entryId", "itemId", "itemRevision", "scopeRevision", "orderMessageSeq", "baseCommit", "commit", "verificationDigest", "state", "generation", "agentId", "runId", "pauseGeneration", "integratedCommit", "inputsCommit", "inputsDigest", "published", "settledAt"];
  const out = Object.fromEntries(keys.filter(k => job[k] !== undefined).map(k => [k, job[k]]));
  if (job.receipt) out.receipt = { outcome: job.receipt.outcome, commit: job.receipt.commit, targets: job.receipt.targets?.map(t => ({target:t.target, outcome:t.outcome})) };
  if (job.supersession) out.supersession = { releasedCommit:job.supersession.releasedCommit, targets:job.supersession.targets };
  return out;
}
export function readReleaseDetail(read, summary, {bookend = true} = {}) {
  const job = JSON.parse(read(["deployment", "get", "--job", summary.id]));
  if (!job || Array.isArray(job) || job.summary === true || JSON.stringify(releaseProjection(job)) !== JSON.stringify(releaseProjection(summary))) throw new Error("Release detail changed or invalid");
  if (bookend && summary.releaseSnapshot) bookendReleaseLedger(read, summary.releaseSnapshot, summary.releaseHead);
  return job;
}
export function bookendReleaseLedger(read, snapshot, head) {
  const p = JSON.parse(read(["deployment", "list", "--view", "active", "--limit", "200", "--snapshot", snapshot]));
  if (p?.version !== 1 || !Array.isArray(p.jobs) || !p.page || p.page.view !== "active" || p.page.limit !== 200 || p.page.snapshot !== snapshot || typeof p.page.nextAfter !== "string" || p.jobs.length > 200 || (!p.jobs.length && p.page.nextAfter)) throw new Error("Release ledger changed during detail read");
  let row = 0; const ids = new Set();
  for (const j of p.jobs) {
    if (j?.summary !== true || typeof j.id !== "string" || !Number.isSafeInteger(j.rowId) || j.rowId <= row || ids.has(j.id) || typeof j.state !== "string" || SETTLED.has(j.state)) throw new Error("Invalid release bookend summary");
    row = j.rowId; ids.add(j.id);
  }
  if (head && JSON.stringify(p.jobs) !== JSON.stringify(head)) throw new Error("Release bookend projection changed");
}
// The underlying baseline reader orders Date milliseconds, then array order.
// Supply exact RFC3339 nanosecond order first so fractional times within the
// same millisecond remain correct. Equal times preserve ledger row order.
export function deploymentBaselines(configured, jobs) {
  const time = j => {
    if (!j.settledAt) return null;
    const m = /^(.*T\d\d:\d\d:\d\d)(?:\.(\d{1,9}))?(Z|[+-]\d\d:\d\d)$/.exec(j.settledAt);
    const ms = m ? Date.parse(m[1]+m[3]) : NaN;
    if (!Number.isFinite(ms)) throw new Error("Invalid release settledAt");
    return BigInt(ms)*1000000n + BigInt((m[2] || "").padEnd(9,"0"));
  };
  const ordered = jobs.map((job,index)=>({job,index,at:time(job)})).sort((a,b)=> a.at === b.at ? a.index-b.index : a.at === null ? -1 : b.at === null ? 1 : a.at < b.at ? -1 : 1).map(x=>x.job);
  return releaseBaselines(configured, ordered);
}

export async function buildInputs(config, jobId, { dryRun = false, deps }) {
  if (config.version !== 1 || !config.cwd || !config.journalDirectory || !/^rel_[a-f0-9]+$/.test(jobId || "")) throw new Error("Private activation config and job ID required");
  const jobs = readReleaseSummaries(deps.tt), summary = jobs.find(j => j.id === jobId);
  if (!summary) throw new Error("Only a claimed job waiting for inputs gets a manifest");
  const job = readReleaseDetail(deps.tt, summary, {bookend:!dryRun});
  if (job?.state !== "claimed") throw new Error("Only a claimed job waiting for inputs gets a manifest");
  // Fast-forward releases have no imported integrated commit.
  const commit = job.integratedCommit || job.commit;
  if (!Number.isSafeInteger(job.generation) || job.generation < 1 || !sha(commit) || !sha(job.commit) || !/^[a-f0-9]{64}$/.test(job.verificationDigest || "")) throw new Error("Exact job binding required");
  deps.git(["cat-file", "-e", `${commit}^{commit}`]);
  const baselines = deploymentBaselines(config.baselines, jobs), selected = selectReleaseTargets(config.cwd, baselines, commit);
  const schema = schemaChanged(config.cwd, baselines.hub, commit);
  const dir = config.journalDirectory, manifestPath = join(dir, `${job.id}-inputs.json`);
  const command = [config.tt || "tt", "deployment", "inputs", "--job", job.id, "--generation", String(job.generation), "--commit", commit, "--file", manifestPath, "--request-id", `${job.id}-inputs-${commit.slice(0, 12)}`];
  const binding = { version: 1, jobId: job.id, commit, acceptedCommit: job.commit, verificationDigest: job.verificationDigest };
  // Hub and bridge selected together share ONE plan, release and backup:
  // two plans made before either deploy would each pin the other's
  // pre-release mount, and the second would put the old partner back.
  const pair = ["hub", "bridge"].filter(t => selected.includes(t));
  const planTargets = t => pair.length === 2 ? pair : [t], planName = t => pair.length === 2 ? "truenas" : t;
  const releaseOf = t => `${job.id}-${commit.slice(0, 12)}-${BINARY[t] ? planName(t) : t}`;
  const plannedTrueNAS = t => { const name = planName(t); return { backupJobId: job.id, backup: `${BASE}/backups/before-${job.id}-${name}.sqlite`, planPath: join(dir, `${job.id}-${name}-plan.json`), preflightReceipt: join(dir, `${job.id}-${name}-preflight.json`), ...(schema ? { backupCopy: join(dir, `${job.id}-${name}-backup.sqlite`) } : {}), planTargets: planTargets(t) }; };
  if (!dryRun) bookendReleaseLedger(deps.tt, jobs.releaseSnapshot, jobs.releaseHead);
  if (dryRun) {
    const targets = Object.fromEntries(selected.map(t => [t, { release: releaseOf(t), ...(BINARY[t] ? plannedTrueNAS(t) : {}) }]));
    return { dryRun: true, ...binding, generation: job.generation, schemaChanged: schema, targets, manifest: manifestPath, command };
  }
  if (existsSync(manifestPath)) throw new Error("Job manifest already written; the imported digest is immutable");
  const live = {};
  for (const t of selected) {
    if (BINARY[t]) for (const s of ["hub", "bridge"]) live[s] ??= await deps.probe(["live", s]);
    else live[t] = await deps.probe(["live", t]);
  }
  const probeArgv = ["node", "scripts/release-probe.mjs", "rollback"], cfg = ["--config", deps.configPath];
  const targets = {}, plans = {};
  for (const t of selected) {
    const release = releaseOf(t), prior = live[t];
    if (BINARY[t]) {
      const planned = plannedTrueNAS(t);
      plans[planned.planPath] ??= (() => {
        const template = JSON.parse(readFileSync(config.inputs.planTemplate, "utf8"));
        const deployment = { ...template.deployment, releaseName: release, targets: planned.planTargets };
        for (const [s, [field, binary]] of Object.entries(BINARY)) {
          const changing = planned.planTargets.includes(s);
          if (!(field in deployment) && !changing) continue;
          // A target this job does not change keeps its live retained mount.
          deployment[field] = changing ? `${BASE}/releases/${release}/${binary}` : `${BASE}/releases/${live[s].release}/${binary}`;
        }
        writePrivate(planned.planPath, JSON.stringify({ ...template, requestId: `${job.id}-${planName(t)}-backup`, backupDestination: planned.backup, deployment }));
        deps.preflight(planned.planPath, planned.preflightReceipt);
        const receipt = JSON.parse(readFileSync(planned.preflightReceipt, "utf8"));
        if (!["success", "already-satisfied"].includes(receipt.status) || receipt.backupDestination !== planned.backup || !/^[a-f0-9]{64}$/.test(receipt.sha256 || "")) throw new Error("Verified job backup receipt required");
        if (planned.backupCopy) {
          deps.copyBackup(planned.backup, planned.backupCopy);
          if (fileSHA(planned.backupCopy) !== receipt.sha256) { rmSync(planned.backupCopy); throw new Error("Backup copy hash mismatch"); }
        }
        return { backupSHA256: receipt.sha256, preflightReceiptSHA256: fileSHA(planned.preflightReceipt) };
      })();
      const { backupSHA256, preflightReceiptSHA256 } = plans[planned.planPath];
      const safe = /^[A-Za-z0-9._-]+$/.test(prior.release || "") && /^[a-f0-9]{64}$/.test(prior.artifactSHA256 || "") && prior.integrity === true;
      targets[t] = { release, ...planned, backupSHA256, preflightReceiptSHA256, rollbackSafe: safe,
        ...(safe ? { rollbackProgram: ["python3", "scripts/deploy-truenas-hub.py", "--rollback-to", prior.release, "--target", t, "--expect-sha256", prior.artifactSHA256],
          rollbackProbe: [...probeArgv, t, "--expect-release", prior.release, "--expect-sha", prior.artifactSHA256, ...cfg] } : {}) };
    } else if (t === "mini") {
      // The runner captures the installed binary itself; the probe confirms
      // the restored bytes are the ones live now.
      targets.mini = { release, rollbackSafe: true, rollbackProbe: [...probeArgv, "mini", "--expect-sha", prior.artifactSHA256, ...cfg] };
    } else {
      const retained = join(dir, `tailos-dist-${prior.commit}`), safe = sha(prior.commit) && existsSync(retained);
      targets.tailos = { release, rollbackSafe: safe, ...(safe ? { rollbackProgram: ["npx", "wrangler", "pages", "deploy", retained, "--project-name", "tailos", "--branch", "main", "--commit-hash", prior.commit, "--commit-dirty=false"],
        rollbackProbe: [...probeArgv, "tailos", "--expect-commit", prior.commit, ...cfg] } : {}) };
    }
  }
  const raw = JSON.stringify({ ...binding, targets });
  writePrivate(manifestPath, raw);
  return { manifest: manifestPath, sha256: createHash("sha256").update(raw).digest("hex"), targets: Object.keys(targets), command };
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  const argv = process.argv.slice(2), flag = name => { const i = argv.indexOf(name); return i < 0 ? undefined : argv[i + 1]; };
  try {
    const configPath = resolve(flag("--config") || ""), config = JSON.parse(readFileSync(configPath, "utf8"));
    process.stdout.write(JSON.stringify(await buildInputs(config, flag("--job"), { dryRun: argv.includes("--dry-run"), deps: hostDeps(config, configPath) }), null, 2) + "\n");
  } catch (error) {
    // Messages here are this script's own; host output is never attached.
    const reason = error.status !== undefined || error.stderr !== undefined ? "a host command failed" : error instanceof SyntaxError ? "unreadable JSON" : error.message.split("\n")[0].slice(0, 200);
    process.stderr.write(`release inputs refused: ${reason}\n`);
    process.exitCode = 1;
  }
}
