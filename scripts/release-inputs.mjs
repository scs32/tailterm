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
import { existsSync, openSync, writeFileSync, closeSync, readFileSync, rmSync } from "node:fs";
import { join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { selectReleaseTargets, releaseBaselines, schemaChanged } from "./release-targets.mjs";
import { probe } from "./release-probe.mjs";

const BASE = "/mnt/deepfreeze/tailterm-hub";
const BINARY = { hub: ["binaryDestination", "tailterm-hub"], bridge: ["bridgeBinaryDestination", "tailterm-discord"] };
const sha = s => /^[a-f0-9]{40}$/.test(s || "");
const fileSHA = p => createHash("sha256").update(readFileSync(p)).digest("hex");
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

export async function buildInputs(config, jobId, { dryRun = false, deps }) {
  if (config.version !== 1 || !config.cwd || !config.journalDirectory || !/^rel_[a-f0-9]+$/.test(jobId || "")) throw new Error("Private activation config and job ID required");
  const jobs = JSON.parse(deps.tt(["deployment", "list"])), job = jobs.find(j => j.id === jobId);
  if (job?.state !== "claimed") throw new Error("Only a claimed job waiting for inputs gets a manifest");
  // Fast-forward releases have no imported integrated commit.
  const commit = job.integratedCommit || job.commit;
  if (!sha(commit) || !sha(job.commit) || !/^[a-f0-9]{64}$/.test(job.verificationDigest || "")) throw new Error("Exact job binding required");
  deps.git(["cat-file", "-e", `${commit}^{commit}`]);
  const baselines = releaseBaselines(config.baselines, jobs), selected = selectReleaseTargets(config.cwd, baselines, commit);
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
        rollbackProbe: [...probeArgv, "tailos", "--expect-commit", prior.commit] } : {}) };
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
