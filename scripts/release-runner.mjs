import { readReleaseSummaries, readReleaseDetail, releaseProjection, bookendReleaseLedger, deploymentBaselines } from "./release-inputs.mjs";
import { createHash } from "node:crypto";
import { tmpdir } from "node:os";
import { fileURLToPath } from "node:url";
import { execFileSync, spawn, spawnSync } from "node:child_process";
import { openSync, closeSync, fsyncSync, writeFileSync, readFileSync, renameSync, mkdirSync, mkdtempSync, existsSync, rmSync, copyFileSync, cpSync, chmodSync, statSync, lstatSync, realpathSync, readdirSync, constants } from "node:fs";
import { join, resolve, dirname, basename, isAbsolute } from "node:path";
import { digest, canonical, diffPaths, receiptEligible, MAX_CHECK_TIMEOUT_MS } from "./verify-matrix.mjs";
import { PRIORITIES, RUN_TIMEOUT_GRACE_MS, holderCapMs, lockPath, readHostState, holdersOf, canonicalResource, pidGone, groupGone } from "./verify-matrix-host-lock.mjs";
import { selectReleaseTargets, releaseBaselines, schemaChanged } from "./release-targets.mjs";
import { buildInfo, waitForTailOSCommit, tailosWindow, tailosURL, hostDeps, readyWindow, sanitizeCapture } from "./release-probe.mjs";

const fileDigest = p => createHash("sha256").update(readFileSync(p)).digest("hex");
const TRUENAS_BASE = "/mnt/deepfreeze/tailterm-hub", PAIR = ["hub", "bridge"];
const DESTINATION = { hub: ["binaryDestination", "tailterm-hub"], bridge: ["bridgeBinaryDestination", "tailterm-discord"] };
const same = (a, b) => JSON.stringify(a) === JSON.stringify(b);
// b4: a failed target step journals {step, target, reason}. The reason is
// only ever a fixed message this runner or its adapter tagged, or the facts
// HostAdapter.command derives from a child (program name, exit status and the
// classification/stage fields of its last JSON line); anything else is
// "unclassified". Child stderr, messages, paths and tokens never reach it.
const REASON = /^[A-Za-z0-9 ,.:;()_\/-]{1,160}$/;
export function releaseError(reason) { const error = new Error(reason); error.releaseReason = reason; return error; }
export function failureReason(error) { const r = error?.releaseReason; return typeof r === "string" && REASON.test(r) ? r : "unclassified"; }
// A conflicting path is named only when it matches this subset of REASON's
// characters; any other path is counted and its text dropped.
const CONFLICT_PATH = /^[A-Za-z0-9_.\/-]{1,120}$/, CONFLICT_PATHS = 50;
// What the private journal keeps of a failure beside the reason. Nothing is
// copied unless validated here: the tag only when failureReason accepts it
// (a rejected tag leaves its length and why, never its text), conflict paths
// only when they match CONFLICT_PATH, and for an untagged error its class
// name and code. message, stack, cause and child output are never read.
export function failureDetail(error) {
  const detail = {}, tag = error?.releaseReason, count = n => Number.isSafeInteger(n) && n >= 0;
  if (typeof tag === "string") {
    if (REASON.test(tag)) detail.text = tag;
    else { detail.tagRejected = tag.length > 160 && /^[A-Za-z0-9 ,.:;()_\/-]+$/.test(tag) ? "length" : "characters"; detail.tagLength = tag.length; }
  } else {
    if (typeof error?.name === "string" && /^[A-Za-z]{1,40}$/.test(error.name)) detail.name = error.name;
    if (typeof error?.code === "string" && /^[A-Z][A-Z0-9_]{0,39}$/.test(error.code)) detail.code = error.code;
  }
  const conflict = error?.conflict;
  if (conflict && typeof conflict === "object") {
    const listed = Array.isArray(conflict.paths) ? conflict.paths : [], valid = listed.filter(p => typeof p === "string" && CONFLICT_PATH.test(p)), dropped = listed.length - valid.length;
    detail.paths = valid.slice(0, CONFLICT_PATHS);
    if (count(conflict.count)) detail.pathCount = conflict.count;
    if (count(conflict.rejected) || dropped) detail.pathsRejected = (count(conflict.rejected) ? conflict.rejected : 0) + dropped;
  }
  return detail;
}
// For the configured CLI only (cli), the reason also names the subcommand:
// its first one or two argv words when they are plain lowercase words. No
// flag, value or path of the call is ever part of it.
const CLI_WORD = /^[a-z][a-z-]{0,23}$/;
function childReason(argv, error, cli) {
  const base = p => String(p).split("/").pop(), program = /\.(py|mjs)$/.test(argv[1] || "") ? base(argv[1]) : base(argv[0]);
  const word = i => argv[0] === cli && CLI_WORD.test(argv[i] || "") ? " " + argv[i] : "";
  const name = (/^[A-Za-z0-9._-]{1,64}$/.test(program) ? program : "program") + word(1) + (word(1) ? word(2) : "");
  if(error.code === "ENOBUFS" || error.code === "ERR_CHILD_PROCESS_STDIO_MAXBUFFER")return `${name} ENOBUFS (command buffer overflow)`;
  const how = error.code === "ETIMEDOUT" ? "timeout" : /^SIG[A-Z0-9]{1,12}$/.test(error.signal || "") ? `signal ${error.signal}` : Number.isInteger(error.status) ? `exit ${error.status}` : "not started";
  let fields = "";
  try {
    const last = JSON.parse(String(error.stdout || "").trim().split("\n").at(-1)), token = v => typeof v === "string" && /^[a-z][a-z0-9-]{0,47}$/.test(v) ? v : null;
    const classification = token(last?.classification), stage = token(last?.stage);
    if (classification) fields = `: ${classification}${stage ? ` at ${stage}` : ""}`;
  } catch {}
  return `${name} ${how}${fields}`;
}
// What the private journal directory keeps of a failed call of the configured
// CLI, in cli-failures.json (0600): the exact argv, working directory, exit
// code, signal and the end of stderr. argv can hold Board text and private
// paths, so none of it leaves this file; the Board and the job journal get
// only childReason's text. The newest CLI_FAILURES distinct failures are kept;
// a repeat of a kept one only raises its count and lastAt, so a failing poll
// cannot grow the file. A record that cannot be written is dropped.
const CLI_FAILURES = 20, CLI_STDERR_BYTES = 16384;
function recordCliFailure(config, job, argv, cwd, error, reason, now) {
  const text = typeof error.stderr === "string" ? error.stderr : Buffer.isBuffer(error.stderr) ? error.stderr.toString("utf8") : "";
  let tail = Buffer.from(text.slice(-CLI_STDERR_BYTES));
  if (tail.length > CLI_STDERR_BYTES) tail = tail.subarray(tail.length - CLI_STDERR_BYTES);
  const bytes = Buffer.byteLength(text), at = new Date(now).toISOString();
  const entry = {at, jobId: job?.id || null, agentId: process.env.TAILTERM_AGENT || null, runId: process.env.TAILTERM_RUN || null, argv: [...argv], cwd: cwd ?? null,
    exitCode: Number.isInteger(error.status) ? error.status : null, signal: typeof error.signal === "string" ? error.signal : null, errorCode: typeof error.code === "string" ? error.code : null,
    stderr: tail.toString("utf8"), stderrBytes: bytes, stderrTruncated: bytes > tail.length, count: 1, lastAt: at};
  try {
    const path = join(config.journalDirectory, "cli-failures.json"), argvText = JSON.stringify(entry.argv);
    let failures = [];
    try { const prior = JSON.parse(readFileSync(path, "utf8")); if (Array.isArray(prior?.failures)) failures = prior.failures; } catch {}
    const kept = failures.find(f => ["jobId", "runId", "cwd", "exitCode", "signal", "errorCode", "stderr", "stderrBytes"].every(k => f?.[k] === entry[k]) && JSON.stringify(f.argv) === argvText);
    if (kept) { kept.count = (Number.isSafeInteger(kept.count) ? kept.count : 1) + 1; kept.lastAt = at; entry.at = kept.at; }
    else failures.push(entry);
    save(path, {version: 1, failures: failures.slice(-CLI_FAILURES)});
  } catch {}
  return {reason, jobId: entry.jobId, at: entry.at};
}
// The Board notice for a recorded CLI failure: the safe reason and the job,
// nothing else of the call. key is one per job and reason for a process; the
// request id also carries when the kept failure was first seen, so a later
// recurrence is a new notice while a restart's resend is not.
export function cliFailureNotice(failure, run) {
  const reason = REASON.test(failure?.reason || "") ? failure.reason : "unclassified", job = /^rel_[A-Za-z0-9]{1,40}$/.test(failure?.jobId || "") ? failure.jobId : null;
  const id = createHash("sha256").update(JSON.stringify([failure?.at ?? null, job, reason])).digest("hex").slice(0, 16);
  return {key: `cli-failure ${job} ${reason}`, requestId: `cli-failure-${NAME(run)}-${id}`, jobId: job, subject: "A deployer CLI call failed",
    text: `Deployer CLI call failed: ${reason}${job ? ` (release ${job})` : ""}. The exact argv, exit code and stderr are kept in cli-failures.json in the private journal directory of the deployer.`};
}
// What the Board may repeat of a refused CLI call's stderr: its last line,
// word by word. A plain word, a small number or a hub id is kept; any other
// word (a path, a token, a NAME=value pair) becomes "(removed)". null when
// nothing readable is left.
const SAFE_WORD = /^\(?(?:[A-Za-z][A-Za-z-]{0,23}|\d{1,6}|(?:rel|wi|tsk|agt|run)_[a-f0-9]{1,40})[),.:;]*$/;
export function refusalText(stderr) {
  const text = typeof stderr === "string" ? stderr : Buffer.isBuffer(stderr) ? stderr.toString("utf8") : "";
  const line = (text.trim().split("\n").at(-1) || "").trim().replace(/^tt: /, "").slice(0, 1024);
  const words = line.split(/\s+/).filter(Boolean).map(w => SAFE_WORD.test(w) ? w : "(removed)").filter((w, i, all) => w !== "(removed)" || all[i - 1] !== w);
  while (words.length && words.join(" ").length > 120) words.pop();
  return words.some(w => w !== "(removed)") ? words.join(" ") : null;
}
// The Board notice for a verified job the deployer skips: the hub refused its
// claim, or its set-aside journal is held. Only the job, its item and commit
// when they have the hub's shape, and a reason of REASON's characters. key is
// one per job for a process. The request id carries the deployer run and a
// short hash of the case and reason: a restart of that run resends the same
// notice, and one with another reason sends a new one. Each recipient's copy
// adds its own suffix.
export function heldClaimNotice(job, kind, reason, run) {
  if (!/^rel_[A-Za-z0-9]{1,40}$/.test(job?.id || "")) return null;
  const item = /^wi_[a-f0-9]{1,40}$/.test(job.itemId || "") ? job.itemId : null, commit = /^[a-f0-9]{40}$/.test(job.commit || "") ? job.commit : null;
  const why = REASON.test(reason || "") ? reason : "unclassified", journal = kind === "journal";
  const id = createHash("sha256").update(JSON.stringify([journal ? "journal" : "claim", why])).digest("hex").slice(0, 8);
  return {key: `claim-held ${job.id}`, requestId: `${job.id}-claim-held-${NAME(run)}-${id}`, jobId: job.id, item, commit,
    subject: journal ? "A release job is skipped because its set-aside journal is held" : "A release job is skipped because the hub refused its claim",
    text: `Release ${job.id} (item ${item || "unknown"}, commit ${commit ? commit.slice(0, 12) : "unknown"}) is verified but not claimed: ${journal ? "the journal of its set-aside claim is held" : "the hub refused the claim"}. Reason: ${why}. Nothing was integrated, published or deployed. The deployer skips this job at every poll and goes on to later jobs; this notice is sent once per job per deployer run to the database handler and to the owner helper. Database handler: reconcile the job (Held claim in docs/project-deployment.md). Owner helper: for information.`};
}
// The files verify-matrix.mjs requires before a browser check runs, in its
// order (a drift test pins the list). All are gitignored, so provisioning
// them never dirties the deployer's checkout.
export const MATRIX_PREREQUISITES = ["node_modules/.package-lock.json", "wasm/tailserve.wasm", ".build/test.wasm", ".build/speech-fixture.wav", ".build/go-modules.txt"];
export const missingPrerequisites = cwd => MATRIX_PREREQUISITES.filter(p => !existsSync(join(cwd, p)));
const prerequisiteError = names => releaseError("Missing matrix prerequisites: " + names.join(", "));
// Provisioning (tt deployment setup): npm ci when the install marker is
// missing, then every other missing file is copied from the source checkout.
// An existing file is never overwritten.
export function provisionPrerequisites(cwd, {from, run = (argv, dir) => execFileSync(argv[0], argv.slice(1), {cwd: dir, stdio: "ignore", timeout: 900000})} = {}) {
  let source;
  try { source = isAbsolute(from) && statSync(from).isDirectory() ? realpathSync(from) : null; } catch { source = null; }
  if (!source || source === realpathSync(cwd)) throw releaseError("Prerequisite source must be another absolute checkout");
  const [marker] = MATRIX_PREREQUISITES, actions = new Map(), missing = [];
  if (!existsSync(join(cwd, marker))) {
    try { run(["npm", "ci"], cwd); } catch { throw releaseError("Prerequisite install failed: npm ci"); }
    if (existsSync(join(cwd, marker))) actions.set(marker, "installed");
  }
  for (const p of MATRIX_PREREQUISITES) {
    if (actions.has(p)) continue;
    const target = join(cwd, p);
    if (existsSync(target)) { actions.set(p, "present"); continue; }
    if (p === marker || !existsSync(join(source, p))) { missing.push(p); continue; }
    mkdirSync(dirname(target), {recursive: true});
    copyFileSync(join(source, p), target, constants.COPYFILE_EXCL);
    actions.set(p, "copied");
  }
  if (missing.length) throw prerequisiteError(missing);
  if (git(cwd, "status", "--porcelain")) throw releaseError("Prerequisite provisioning changed the checkout");
  return {version: 1, prerequisites: MATRIX_PREREQUISITES.map(p => ({path: p, sha256: fileDigest(join(cwd, p)), action: actions.get(p)}))};
}
// The in-release run is bounded like every other holder of the verification
// host: every check's timeout times its attempts plus the test-binary build
// allowance (the serial sum, an upper bound under every scheduler rule),
// clamped to the holder cap. It is the number the run itself records in the
// host lock; the per-check timers inside verify-matrix remain the real limit.
export const MATRIX_BUILD_ALLOWANCE_MS = 1800000;
export function matrixRunTimeout(plan) {
  const attempts = plan?.maxAttempts ?? 1;
  if (!Array.isArray(plan?.checks) || !plan.checks.length) throw releaseError("Matrix plan has no checks");
  if (!Number.isSafeInteger(attempts) || attempts < 1) throw releaseError("Invalid matrix attempt limit");
  return Math.min(holderCapMs(), plan.checks.reduce((total, check) => {
    const raw = check?.environment?.VERIFICATION_TIMEOUT_MS, ms = Number(raw);
    if (typeof raw !== "string" || !/^[1-9][0-9]*$/.test(raw) || !Number.isSafeInteger(ms) || ms > MAX_CHECK_TIMEOUT_MS) throw releaseError("Invalid matrix check timeout");
    return total + ms * attempts;
  }, MATRIX_BUILD_ALLOWANCE_MS));
}
// The in-release run is a member of the verification host's ordered waitlist
// (scripts/verify-matrix-host-lock.mjs) like any verifier's run: nothing here
// counts processes. It waits at most this long for its turn.
export const MATRIX_HOST_WAIT_MS = 7200000;
// How long a launched run may take to appear in the lock file, how long a
// stopped run gets before its group is killed, and the slack added to the
// run's own wait and bound before the runner stops it.
export const MATRIX_LAUNCH_GRACE_MS = 60000, MATRIX_STOP_GRACE_MS = 30000, MATRIX_DEADLINE_SLACK_MS = 60000;
// The job's priority if the hub ever carries one, else the private config
// key matrixPriority, else high.
export function matrixPriority(job, config) {
  const value = job?.priority ?? config?.matrixPriority ?? "high";
  if (!PRIORITIES.includes(value)) throw releaseError("Invalid matrix priority");
  return value;
}
const NAME = v => typeof v === "string" && /^[A-Za-z0-9_.-]{1,64}$/.test(v) ? v : "unknown";
const ATTEMPT = /^[a-f0-9]{40}-r[0-9]{1,6}$/, ALL_ATTEMPTS = "attempts";
// The attempt record DIR/run.json. null: no record. A record moved to
// run.json.set-aside counts as ended.
function readRun(dir) {
  const path = join(dir, "run.json"); let raw;
  try { raw = readFileSync(path, "utf8"); } catch (error) { return error.code !== "ENOENT" ? {state: "unreadable"} : existsSync(path + ".set-aside") ? {state: "ended", reason: "set-aside"} : null; }
  try { const run = JSON.parse(raw); return ["starting", "started", "ended"].includes(run?.state) ? run : {state: "unreadable"}; } catch { return {state: "unreadable"}; }
}
// Whether any attempt of this job has a matrix run that is not known to have
// ended: starting, started, held or unreadable. Such a run may be using the
// checkout, so the job is never set aside while this holds. True on doubt.
export function matrixRunUnsettled(journalDirectory, job) {
  let names;
  try { names = readdirSync(join(journalDirectory, job.id + "-integrated-verification")); } catch (error) { return error.code !== "ENOENT"; }
  return names.some(name => { const run = readRun(join(journalDirectory, job.id + "-integrated-verification", name)); return run !== null && run.state !== "ended"; });
}
// Matrix runs this process started, by attempt directory. The daemon builds a
// new adapter every poll, so the live child (the proof that a pid is ours)
// is kept for the life of the process, until its attempt ends.
const MATRIX_CHILDREN = new Map();
// One wait notice per position, list length and holder. Only fixed-shape
// names from the lock file reach the text.
// change counts the places the run has had (kept in its attempt record), so a
// return to an earlier place is a new notice while a restart's resend of the
// same place is not.
export function matrixWaitNotice(job, wait) {
  if (!job?.id || !wait || !ATTEMPT.test(wait.attempt || "") || !Number.isSafeInteger(wait.position) || !Number.isSafeInteger(wait.length)) return null;
  const holders = wait.holders || (wait.holder ? [wait.holder] : []);
  const names = holders.map(h => NAME(h.id)).sort();
  const identity = names.length > 1 ? createHash("sha256").update(JSON.stringify(names)).digest("hex").slice(0, 24) : names[0] || "none";
  const again = Number.isSafeInteger(wait.change) && wait.change > 1 ? `-n${wait.change}` : "";
  const id = h => `${job.id}-matrix-wait-${wait.attempt}-p${wait.position}-of${wait.length}-${h}${again}`, full = id(identity);
  return {requestId: full.length <= 128 ? full : id(identity.slice(0, 8)), subject: "A release job is waiting for the verification host",
    text: `Release ${job.id} waits for the verification host at position ${wait.position} of ${wait.length} at priority ${NAME(wait.priority)} ${holders.length ? "behind " + holders.map(h => `${NAME(h.item)}/${NAME(h.agent)}/pid ${Number.isSafeInteger(h.pid) ? h.pid : "unknown"}`).join("; ") : "with no holder"}`};
}
export function matrixHeldNotice(job, held) {
  if (!job?.id || !held || !(ATTEMPT.test(held.attempt || "") || held.attempt === ALL_ATTEMPTS)) return null;
  const groups = (held.groups || []).filter(Number.isSafeInteger);
  return {requestId: `${job.id}-matrix-held-${held.attempt}`, subject: "A release job is held until its matrix run is confirmed stopped",
    text: `Release ${job.id} is held: its matrix run could not be confirmed stopped (pid ${Number.isSafeInteger(held.pid) ? held.pid : "unknown"}; check groups still alive: ${groups.join(",") || "none"}; ${REASON.test(held.reason || "") ? held.reason : "unclassified"}). It keeps the project release fence and no other job integrates. Confirm nothing of that run is alive, then follow the held matrix run step in docs/project-deployment.md.`};
}
const sha = s => /^[a-f0-9]{40}$/.test(s || "");
const git = (cwd,...argv) => execFileSync("git",argv,{cwd,encoding:"utf8",stdio:["ignore","pipe","pipe"]}).trim();
export function integrateCandidate(cwd, job, branch="tasks-hub") {
  if (branch !== "tasks-hub" || !sha(job.commit) || !sha(job.baseCommit) || job.plan?.commit !== job.commit) throw releaseError("Release binding mismatch");
  if (git(cwd,"status","--porcelain")) throw releaseError("Dirty deployment checkout");
  const expected = git(cwd,"rev-parse",`refs/heads/${branch}`);
  // rev-parse itself fails for a commit this repository does not have.
  let known;
  try { known = sha(expected) && git(cwd,"rev-parse",`${job.commit}^{commit}`)===job.commit && git(cwd,"rev-parse",`${job.baseCommit}^{commit}`)===job.baseCommit; } catch { known = false; }
  if (!known) throw releaseError("Unknown commit identity");
  try { git(cwd,"merge-base","--is-ancestor",job.baseCommit,job.commit); } catch { throw releaseError("Unknown candidate base"); }
  git(cwd,"checkout","--detach",expected);
  try {
    try {git(cwd,"merge-base","--is-ancestor",expected,job.commit);git(cwd,"checkout","--detach",job.commit);}
    catch {
      const commits=git(cwd,"rev-list","--reverse",`${job.baseCommit}..${job.commit}`).split("\n").filter(Boolean);
      if (!commits.length || commits.some(c => git(cwd,"rev-list","--parents","-n","1",c).split(" ").length!==2)) throw releaseError("Nonlinear candidate series");
      git(cwd,"cherry-pick",...commits);
    }
  } catch (error) {abortIntegration(cwd,expected,error);}
  const integrated=git(cwd,"rev-parse","HEAD");
  return {expected,integrated,changed:diffPaths(cwd,expected,integrated)};
}
// Ends a failed checkout or cherry-pick back on the commit it started from
// and throws the refusal.
function abortIntegration(cwd,expected,error) {
  // The unmerged paths are read before the abort discards them, untrimmed
  // and without git's stderr, and reduced here to valid paths and counts.
  let unmerged=[];
  try {unmerged=[...new Set(execFileSync("git",["diff","--name-only","--diff-filter=U","-z"],{cwd,encoding:"utf8",stdio:["ignore","pipe","ignore"]}).split("\0").filter(Boolean))].sort();} catch {}
  const valid=unmerged.filter(p=>CONFLICT_PATH.test(p)),conflict={paths:valid.slice(0,CONFLICT_PATHS),count:unmerged.length,rejected:unmerged.length-valid.length};
  try {git(cwd,"cherry-pick","--abort");} catch {}
  git(cwd,"checkout","--detach",expected);
  if (typeof error?.releaseReason==="string") throw error;
  if (!conflict.count) throw releaseError("Candidate integration refused");
  // All the paths or only their count, never a partial list.
  const named="integration-conflict: "+conflict.paths.join(", "),listed=conflict.rejected===0 && conflict.count<=CONFLICT_PATHS && REASON.test(named);
  const refused=releaseError(listed?named:`integration-conflict: ${conflict.count} path${conflict.count===1?"":"s"}`);refused.conflict=conflict;throw refused;
}
// Release batching. With releaseBatch.maxJobs above 1 in the private config,
// the deployer integrates the verified jobs queued behind the one it claimed
// (the lead) that cherry-pick cleanly, in queue order, into one candidate,
// and runs one matrix, one import and one deploy for them. Only the lead is
// claimed; the hub settles the other jobs with the lead's released finish.
// Absent or 1, none of this runs.
export const BATCH_MAX_JOBS=8,BATCH_IMPORT_WAIT_MS=1800000;
export function releaseBatchPolicy(config){
  const batch=config?.releaseBatch;
  if(batch===undefined)return {max:1,importWaitMs:BATCH_IMPORT_WAIT_MS};
  if(!batch || typeof batch!=="object" || Array.isArray(batch))throw releaseError("Invalid release batch size");
  const max=batch.maxJobs===undefined?1:batch.maxJobs,importWaitMs=batch.importWaitMs===undefined?BATCH_IMPORT_WAIT_MS:batch.importWaitMs;
  if(!Number.isSafeInteger(max) || max<1 || max>BATCH_MAX_JOBS)throw releaseError("Invalid release batch size");
  if(!Number.isSafeInteger(importWaitMs) || importWaitMs<=0)throw releaseError("Invalid release batch import wait");
  return {max,importWaitMs};
}
// The hub computes the same id (releaseBatchID in hub/internal/store/releases.go);
// one shared test vector pins both. generation is the lead's before batch-open.
export const batchId=(leadJobId,generation,baseCommit)=>"bat_"+createHash("sha256").update(`${leadJobId}\n${generation}\n${baseCommit}`).digest("hex").slice(0,16);
// The summaries that may join the lead's batch, in queue order: verified,
// unclaimed, queued after the lead in the same project generation, from the
// lead's base commit (one matrix run covers a job only from that base), and
// not marked to release alone.
export function batchCandidates(jobs,lead,solo=new Set()){
  const at=jobs.find(j=>j.id===lead?.id)?.rowId;
  if(!Number.isSafeInteger(at) || solo.has(lead.id))return [];
  return jobs.filter(j=>j.state==="verified" && !j.agentId && Number.isSafeInteger(j.rowId) && j.rowId>at && j.pauseGeneration===lead.pauseGeneration && sha(j.baseCommit) && j.baseCommit===lead.baseCommit && !solo.has(j.id)).sort((a,b)=>a.rowId-b.rowId);
}
const racePackages=argv=>{const first=Array.isArray(argv)?argv.findIndex(a=>typeof a==="string" && a.startsWith("./")):-1;return first<1 || argv.slice(first).some(a=>typeof a!=="string" || !a.startsWith("./"))?null:{flags:argv.slice(0,first),packages:argv.slice(first)};};
// extra's checks carried on base's list, or null when they cannot be: every
// check must equal the listed check of its id, except go-race, which may
// differ in packages only. The merged go-race tests the union, or ./... alone.
function mergeAcceptedChecks(base,extra){
  if(!Array.isArray(base) || !Array.isArray(extra))return null;
  let out=base;
  for(const check of extra){
    const at=out.findIndex(c=>c.id===check?.id);
    if(at<0)return null;
    if(canonical(out[at])===canonical(check))continue;
    const have=racePackages(out[at].argv),want=check.id==="go-race" && racePackages(check.argv);
    if(!have || !want || !same(have.flags,want.flags) || canonical({...out[at],argv:[]})!==canonical({...check,argv:[]}))return null;
    const packages=[have,want].some(p=>p.packages.includes("./..."))?["./..."]:[...new Set([...have.packages,...want.packages])].sort();
    out=out.with(at,{...out[at],argv:[...have.flags,...packages]});
  }
  return out;
}
// The accepted checks one matrix run must keep for the lead and the members
// that can share it; a member whose checks differ in any other way is dropped.
export function batchAcceptedChecks(lead,members){
  let checks=lead.plan.checks;const joined=[],dropped=[];
  for(const member of members){
    const merged=mergeAcceptedChecks(checks,member.plan?.checks);
    if(merged){checks=merged;joined.push(member.id);}else dropped.push({jobId:member.id,reason:"batch-plan-mismatch"});
  }
  return {checks,checksDigest:digest(checks),joined,dropped};
}
// Why a job's full record cannot join the lead's batch, or "". The hub checks
// the same at batch-add.
export function batchMismatch(lead,job){
  if(job?.state!=="verified" || job.agentId || job.pauseGeneration!==lead.pauseGeneration || job.repository!==lead.repository || job.baseCommit!==lead.baseCommit)return "batch-ineligible";
  if(job.plan?.matrixDigest!==lead.plan?.matrixDigest || job.plan?.matrixApprovalMessageSeq!==lead.plan?.matrixApprovalMessageSeq)return "batch-other-matrix";
  if(lead.plan?.checks===undefined && job.plan?.checks===undefined)return "";
  return mergeAcceptedChecks(lead.plan?.checks,job.plan?.checks)?"":"batch-plan-mismatch";
}
// The hub's coverage rule, checked before the matrix runs: every approved
// check of every job is in the plan by digest, by the go-race package
// superset, or, when the plan's matrix is not the job's, by the check of the
// same id (a go-race still testing every approved package). Returns the first
// uncovered {jobId, checkId}, or null.
export function batchPlanCovers(plan,jobs){
  const race=(want,have,policy)=>{
    if(want.id!=="go-race" || have.id!==want.id)return false;
    const w=racePackages(want.argv),h=racePackages(have.argv);
    if(!w || !h || (policy && (have.cwd!==want.cwd || canonical(have.environment)!==canonical(want.environment) || !same(w.flags,h.flags))))return false;
    return h.packages.includes("./...") || w.packages.every(p=>h.packages.includes(p));
  };
  const checks=Array.isArray(plan?.checks)?plan.checks:[];
  for(const job of jobs){
    const changed=job.matrixDigest!==undefined && job.matrixDigest!==plan?.matrixDigest;
    for(const want of job.checks||[]){
      if(!checks.some(have=>canonical(have)===canonical(want) || race(want,have,true) || (changed && have.id===want.id && (want.id!=="go-race" || race(want,have,false)))))return {jobId:job.jobId,checkId:want.id};
    }
  }
  return null;
}
// One member's commits cherry-picked onto head; a failure leaves the checkout
// on head and throws the refusal integrateCandidate would.
function pickCandidate(cwd,job,head){
  if(!sha(job.commit) || !sha(job.baseCommit) || job.plan?.commit!==job.commit)throw releaseError("Release binding mismatch");
  let known;
  try{known=git(cwd,"rev-parse",`${job.commit}^{commit}`)===job.commit && git(cwd,"rev-parse",`${job.baseCommit}^{commit}`)===job.baseCommit;}catch{known=false;}
  if(!known)throw releaseError("Unknown commit identity");
  try{git(cwd,"merge-base","--is-ancestor",job.baseCommit,job.commit);}catch{throw releaseError("Unknown candidate base");}
  try{
    const commits=git(cwd,"rev-list","--reverse",`${job.baseCommit}..${job.commit}`).split("\n").filter(Boolean);
    if(!commits.length || commits.some(c=>git(cwd,"rev-list","--parents","-n","1",c).split(" ").length!==2))throw releaseError("Nonlinear candidate series");
    git(cwd,"cherry-pick",...commits);
  }catch(error){abortIntegration(cwd,head,error);}
  return git(cwd,"rev-parse","HEAD");
}
// Integrates the lead as integrateCandidate does, then each candidate in turn
// until the batch holds max jobs. A candidate that does not pick cleanly, or
// that the hub refuses, drops out with the checkout back on the last job that
// joined. hooks.integrated(first) follows the lead's integration; hooks.open
// (base) declares the batch before the first member, returning its id or
// false, and hooks.add(job, from, to) declares each job's range, the lead's
// first. A hook that throws (an unconfirmed hub call) is passed on with the
// checkout untouched. Returns today's integration, the jobs dropped, and
// batch only when a member joined.
export async function integrateBatch(cwd,lead,candidates,max,hooks={}){
  const first=integrateCandidate(cwd,lead),jobs=[],dropped=[];
  await hooks.integrated?.(first);
  const range=(job,from,to)=>({jobId:job.id,entryId:job.entryId,generation:job.generation,commit:job.commit,verificationDigest:job.verificationDigest,from,to});
  let head=first.integrated,id=null,closed=false;
  for(const member of candidates){
    if(closed || Math.max(jobs.length,1)>=max)break;
    let to;
    try{to=pickCandidate(cwd,member,head);}
    catch(error){dropped.push({jobId:member.id,reason:failureReason(error)});continue;}
    if(!id){
      id=await hooks.open(first.expected);
      if(id && await hooks.add(lead,first.expected,first.integrated)===true)jobs.push(range(lead,first.expected,first.integrated));
      else closed=true;
    }
    if(closed || await hooks.add(member,head,to)!==true){
      git(cwd,"checkout","--detach",head);dropped.push({jobId:member.id,reason:closed?"batch-open-refused":"batch-add-refused"});continue;
    }
    jobs.push(range(member,head,to));head=to;
  }
  return {expected:first.expected,integrated:head,changed:diffPaths(cwd,first.expected,head),dropped,...(jobs.length>1?{batch:{id,base:first.expected,jobs,dropped}}:{})};
}
// The batch's own receipt and, for a released batch, one finished journal per
// member, written from the lead journal's saved intent and final receipt.
// The same bytes on every call, each through a temporary file and a rename,
// so any finish, retry or reconciliation path may repeat it.
export function materializeBatch(journalDirectory,journal){
  const intent=journal?.batchReceipt,final=journal?.receipt;
  if(!intent || !/^bat_[a-f0-9]{16}$/.test(intent.batchId||"") || !Array.isArray(intent.jobs) || !final || !["released","rolled_back","blocked"].includes(final.outcome))return null;
  const receipt={...intent,commit:final.commit,outcome:final.outcome,targets:final.targets,...(final.push?{push:final.push}:{}),...(final.revert?{revert:final.revert}:{})};
  save(join(journalDirectory,intent.batchId+"-receipt.json"),receipt);
  if(final.outcome==="released")for(const member of intent.jobs.slice(1)){
    if(typeof member?.jobId!=="string" || !/^[A-Za-z0-9_-]+$/.test(member.jobId))continue;
    const path=join(journalDirectory,member.jobId+".json");
    // A journal of an earlier claim of this job is kept beside the new one.
    try{if(JSON.parse(readFileSync(path,"utf8"))?.batch?.id!==intent.batchId)renameSync(path,`${path}.before-${intent.batchId}`);}catch{}
    save(path,{version:1,jobId:member.jobId,commit:member.commit,...(journal.taskId?{taskId:journal.taskId}:{}),phase:"complete",effects:[],batch:{id:intent.batchId,leadJobId:intent.leadJobId,from:member.from,to:member.to,integrated:intent.integrated},
      receipt:{version:1,jobId:member.jobId,commit:final.commit,verificationDigest:member.verificationDigest,targets:final.targets,outcome:"released",...(final.push?{push:final.push}:{})}});
  }
  return receipt;
}
// Jobs that left a failed batch release alone and never join another:
// JOURNAL/batch-solo.json. A missing file is empty; an unreadable one throws,
// so nothing is batched on doubt.
const batchSoloPath=dir=>join(dir,"batch-solo.json");
export function readBatchSolo(journalDirectory){
  if(!existsSync(batchSoloPath(journalDirectory)))return new Set();
  let jobs;try{jobs=JSON.parse(readFileSync(batchSoloPath(journalDirectory),"utf8"))?.jobs;}catch{}
  if(!Array.isArray(jobs) || jobs.some(j=>typeof j!=="string"))throw releaseError("Invalid batch solo file");
  return new Set(jobs);
}
export function addBatchSolo(journalDirectory,jobIds){
  const jobs=readBatchSolo(journalDirectory);for(const id of jobIds)jobs.add(id);
  save(batchSoloPath(journalDirectory),{version:1,jobs:[...jobs]});
}
// Moving tasks-hub under a worktree that has it checked out leaves that
// working tree showing the release reversed, so every ref mutation refuses
// first. The owner's main checkout is detached at provisioning.
export function tasksHubCheckedOut(cwd) {
  return git(cwd,"worktree","list","--porcelain").split("\n").includes("branch refs/heads/tasks-hub");
}
export function publishIntegration(cwd, integrated, expected) {
  if (!sha(integrated)||!sha(expected)||git(cwd,"rev-parse","HEAD")!==integrated||git(cwd,"status","--porcelain")) throw releaseError("Integrated checkout changed");
  if (tasksHubCheckedOut(cwd)) throw releaseError("tasks-hub is checked out in a worktree");
  try {git(cwd,"update-ref","refs/heads/tasks-hub",integrated,expected);}catch{throw releaseError("Release ref race");}
}
// D1: after a rollback, tasks-hub gets a commit whose tree is the pre-release
// tree, so the next release does not ship the rolled-back change again.
export function revertCommit(cwd, job, integrated, expected) {
  if (!sha(integrated)||!sha(expected)) throw releaseError("Revert binding required");
  const message=`Revert release ${job.id} (rolled back)\n\nRestores the tree of ${expected} after the live rollback of ${integrated}.\n\nRelease-Job: ${job.id}\n${job.itemId?`Work-Item: ${job.itemId}\n`:""}`;
  return execFileSync("git",["commit-tree",git(cwd,"rev-parse",`${expected}^{tree}`),"-p",integrated,"-F","-"],{cwd,input:message,encoding:"utf8",stdio:["pipe","pipe","pipe"]}).trim();
}
// Compare-and-swap only, never forced. A resumed run whose earlier update
// landed before its checkpoint sees the ref already at the revert.
export function moveReleaseRef(cwd, revert, integrated) {
  if (!sha(revert)||!sha(integrated)||tasksHubCheckedOut(cwd)) return false;
  try {git(cwd,"update-ref","refs/heads/tasks-hub",revert,integrated);return true;}
  catch {try {return git(cwd,"rev-parse","refs/heads/tasks-hub")===revert;} catch {return false;}}
}
// D2: fast-forward only. Credentials come from the host's git helper; a
// prompt would hang the daemon, so terminal prompts are disabled.
export function pushRelease(cwd, commit, remote="origin") {
  if (!sha(commit)) return false;
  try {execFileSync("git",["push","--quiet",remote,`${commit}:refs/heads/tasks-hub`],{cwd,stdio:["ignore","pipe","pipe"],timeout:120000,env:{...process.env,GIT_TERMINAL_PROMPT:"0"}});return true;}
  catch {return false;}
}
// Go stamps vcs.revision only when .git is a directory, and the deployer's
// checkout is a worktree whose .git is a file (a nested checkout would even
// stamp its parent's revision). Go artifacts are therefore built in a
// temporary shared clone detached at the exact commit, removed afterwards.
export function withBuildCheckout(cwd, commit, build) {
  if (!sha(commit)) throw releaseError("Exact build commit required");
  const dir=mkdtempSync(join(tmpdir(),"tailterm-release-build-")),src=join(dir,"src");
  try {
    git(dir,"clone","--quiet","--shared","--no-checkout",resolve(cwd),src);git(src,"checkout","--quiet","--detach",commit);
    if(git(src,"rev-parse","HEAD")!==commit||git(src,"status","--porcelain"))throw releaseError("Build checkout mismatch");
    return build(src);
  } finally {rmSync(dir,{recursive:true,force:true});}
}
function save(path,value) {
  mkdirSync(dirname(path),{recursive:true,mode:0o700});const tmp=path+".tmp";
  const fd=openSync(tmp,"w",0o600);try{writeFileSync(fd,JSON.stringify(value));fsyncSync(fd);}finally{closeSync(fd);}
  renameSync(tmp,path);const d=openSync(dirname(path),"r");try{fsyncSync(d);}finally{closeSync(d);}
}
export async function liveCheck(adapter, target, policy, sleep=ms=>new Promise(r=>setTimeout(r,ms)), now=()=>Date.now()) {
  const until=now()+policy.startupMs; let failures=0;
  for (;;) {
    const r=await adapter.check(target);
    if (r===true) {if(target==="mini") {await sleep(policy.relayCleanMs);if(await adapter.check(target)!==true) return false;}return true;}
    if (r==="identity" || r==="integrity" || r==="readiness") return false;
    if(now()>=until && ++failures>=policy.failures)return false;
    await sleep(policy.intervalMs);
  }
}
// Adapter operations receive nonsecret job/artifact references. Neither captured
// subprocess output nor arbitrary error strings are put into journal/receipt;
// a failed target step records only the tagged reason failureReason allows.
// One host release lock per deployment checkout and project.
const hostLockPath=(cwd,job)=>join(tmpdir(),"tailterm-release-locks",digest(cwd+"\0"+(job.taskId||"fixture"))+".lock");
export async function runRelease(config, adapter) {
  const {cwd,job,baselines,journalPath}=config;
  const policy={startupMs:60000,failures:3,intervalMs:5000,relayCleanMs:30000,...config.testPolicy};
  if(!job || !(["claimed","merged"].includes(job.state)) || !sha(job.commit) || !/^[a-f0-9]{64}$/.test(job.verificationDigest||""))throw releaseError("Claimed verified job required");
  // A lost finish response must retry the original key/generation. The hub
  // may already be terminal, so a fresh execution check cannot gate that retry.
  let pending;
  if(existsSync(journalPath))pending=JSON.parse(readFileSync(journalPath,"utf8"));
  const finishOnly=pending && ["receipt_pending","finishing"].includes(pending.phase) && pending.jobId===job.id && pending.commit===job.commit && pending.agentId===job.agentId && pending.runId===job.runId && pending.receipt?.version===1 && pending.receipt.jobId===job.id && pending.receipt.commit===pending.integrated && pending.receipt.verificationDigest===job.verificationDigest && Array.isArray(pending.receipt.targets);
  if(!finishOnly && await adapter.fence(job)!==true)throw releaseError("release fence lost");
  const lock=hostLockPath(cwd,job);mkdirSync(dirname(lock),{recursive:true,mode:0o700});
  let fd;try{fd=openSync(lock,"wx",0o600);}catch{throw releaseError("Release host locked; inspect prior execution");}
  writeFileSync(fd,JSON.stringify({jobId:job.id,agentId:job.agentId,runId:job.runId}));fsyncSync(fd);
  let state={version:1,jobId:job.id,commit:job.commit,agentId:job.agentId,runId:job.runId,taskId:job.taskId,pauseGeneration:job.pauseGeneration,phase:"prepared",effects:[]};
  if(existsSync(journalPath)){
    const prior=JSON.parse(readFileSync(journalPath,"utf8")),recovery=job.reconciliations?.at(-1);
    const recovered=recovery?.disposition==="requeue" && recovery.noActiveExecution===true && recovery.noPublication===true && recovery.journalState==="no_effects" && recovery.jobId===job.id && recovery.journalDigest===fileDigest(journalPath) && recovery.agentId===prior.agentId && recovery.runId===prior.runId && prior.effects.length===0 && prior.published!==true && prior.jobId===job.id && prior.commit===job.commit;
    if(recovered){renameSync(journalPath,journalPath+".reconciled-"+recovery.journalDigest);}
    else
    // A run stopped while declaring a batch: nothing is published or deployed
    // and the checkout may be anywhere, so the resume below first replays
    // the unconfirmed hub call and only then moves the checkout.
    if(prior.phase==="batching" && prior.jobId===job.id && prior.commit===job.commit && prior.agentId===job.agentId && prior.runId===job.runId && prior.taskId===job.taskId && prior.pauseGeneration===job.pauseGeneration && Array.isArray(prior.effects) && !prior.effects.length && prior.published!==true && sha(prior.expected) && sha(prior.batchLead)) {state=prior;}
    else
    if(prior.jobId===job.id && prior.commit===job.commit && prior.agentId===job.agentId && prior.runId===job.runId && (prior.taskId===undefined || prior.taskId===job.taskId) && (prior.pauseGeneration===undefined || prior.pauseGeneration===job.pauseGeneration) && (["waiting_matrix","waiting_inputs","pushing","receipt_pending","finishing"].includes(prior.phase)) && (!["waiting_matrix","waiting_inputs"].includes(prior.phase) || !prior.effects.length) && git(cwd,"rev-parse","HEAD")===prior.integrated && !git(cwd,"status","--porcelain")) {state=prior;}
    else {
      closeSync(fd);rmSync(lock);
      if(prior.jobId===job.id && prior.commit===job.commit && prior.phase==="complete"){try{materializeBatch(dirname(journalPath),prior);}catch{}return prior.receipt;}
      throw releaseError("Ambiguous journal requires handler reconciliation");
    }
  }
  // The runner code that last ran this job; never part of a resume decision.
  if(config.code)state.code={loaded:config.code.loaded??null,current:config.code.current??null};
  const checkpoint=()=>save(journalPath,state);let step=null;
  const fence=async()=>{if(await adapter.fence(job)!==true)throw releaseError("release fence lost");};
  // The batch receipt and member journals, written once the hub has the
  // final receipt and before the journal leaves its pending phase: a failed
  // write, or a stop here, leaves a pending journal that every later poll
  // retries (the finish replays; reconcileReceipts writes again).
  const materialize=()=>{if(state.batchReceipt)materializeBatch(dirname(journalPath),state);};
  // Batch calls to the hub. Each is journaled with the lead generation it is
  // sent at before it is sent, and its request id is built from that saved
  // generation, so a call whose response was lost is sent again unchanged and
  // the hub replays it. A call that still fails was refused if the lead's
  // generation has not moved; otherwise its result is unknown and the run
  // returns waiting_batch with the checkout untouched, for the next poll.
  const batchOp=async(op,fields={})=>{
    state.batchOps??=[];
    const generation=adapter.job?.generation??job.generation,requestId=`${job.id}-${op}-${generation}`;
    let record=state.batchOps.find(o=>o.status==="sending" && o.op===op) || (op==="batch-drop"?state.batchOps.find(o=>o.requestId===requestId && o.status==="refused"):undefined);
    if(!record){record={op,...fields,generation,requestId};state.batchOps.push(record);}
    record.status="sending";checkpoint();
    return settleBatchOp(record);
  };
  const settleBatchOp=async record=>{
    for(let attempt=0;attempt<2;attempt++){
      try{await adapter.batch(record.op,{entryId:record.entryId,commit:record.commit},record.generation);record.status="done";checkpoint();return true;}catch{}
    }
    let generation;try{generation=await adapter.generation();}catch{}
    if(generation!==record.generation)throw Object.assign(releaseError("Release batch call unconfirmed"),{batchWait:true});
    record.status="refused";checkpoint();return false;
  };
  // What a settled batch-open or batch-add means for the journal's batch.
  const applyBatchOp=(record,ok)=>{
    if(record.applied)return;
    if(ok && record.op==="batch-open")state.batch={id:batchId(job.id,record.generation,record.commit),base:record.commit,jobs:[]};
    if(ok && record.op==="batch-add" && state.batch){state.batch.jobs.push({...record.member,from:record.from,to:record.commit});if(record.goRace)state.batch.goRace=record.goRace;}
    record.applied=true;checkpoint();
  };
  const declare=async(op,fields)=>{const ok=await batchOp(op,fields),record=state.batchOps.findLast(o=>o.op===op);applyBatchOp(record,ok);return ok;};
  const batchDropped=()=>state.batchOps?.some(o=>o.op==="batch-drop" && o.status==="done")===true;
  // A batch no member joined is dropped at the hub and the lead goes alone.
  const closeBatch=async()=>{
    if(state.batch && state.batch.jobs.length<2){if(!batchDropped())await batchOp("batch-drop");state.batch=null;checkpoint();}
    return {expected:state.expected,integrated:state.batch?state.batch.jobs.at(-1).to:state.batchLead};
  };
  const mergedGoRace=member=>{
    const lead=job.plan?.checks,race=lead?.map(c=>c.id==="go-race" && state.batch?.goRace?state.batch.goRace:c);
    return mergeAcceptedChecks(race,member.plan?.checks)?.find(c=>c.id==="go-race");
  };
  const openBatch=async()=>{
    const members=config.batch.candidates.filter(c=>{const reason=batchMismatch(job,c);if(reason)(state.batchDropped??=[]).push({jobId:c.id,reason});return !reason;});
    const result=await integrateBatch(cwd,job,members,config.batch.max,{
      integrated:first=>{state.expected=first.expected;state.integrated=first.integrated;state.batchLead=first.integrated;state.phase="batching";checkpoint();},
      open:async base=>await declare("batch-open",{commit:base})?state.batch.id:false,
      add:(member,from,to)=>{const goRace=member.id===job.id?undefined:mergedGoRace(member);return declare("batch-add",{entryId:member.entryId,commit:to,from,...(goRace?{goRace}:{}),member:{jobId:member.id,entryId:member.entryId,generation:member.generation,commit:member.commit,verificationDigest:member.verificationDigest}});},
    });
    if(result.dropped.length)(state.batchDropped??=[]).push(...result.dropped);
    return closeBatch();
  };
  // Resuming a stopped declaration: the unconfirmed call is replayed before
  // any checkout, the batch closes with the jobs that joined, and only then
  // is the checkout put on its tip.
  const resumeBatch=async()=>{
    const pending=state.batchOps?.find(o=>o.status==="sending");
    if(pending && !await settleBatchOp(pending) && pending.op==="batch-add" && pending.member?.jobId!==job.id)(state.batchDropped??=[]).push({jobId:pending.member.jobId,reason:"batch-add-refused"});
    for(const record of state.batchOps||[])if(record.op!=="batch-drop")applyBatchOp(record,record.status==="done");
    if(batchDropped())state.batch=null;
    const integration=await closeBatch();
    try{git(cwd,"cherry-pick","--abort");}catch{}
    git(cwd,"checkout","--detach",integration.integrated);
    if(git(cwd,"status","--porcelain"))throw releaseError("Dirty deployment checkout");
    return integration;
  };
  // What the integrated plan selects its checks from: the tasks-hub tip the
  // candidates were integrated onto, plus every candidate's own changed and
  // owned paths. Without a known tip, or with a plan whose paths cannot be
  // read, nothing is passed and the plan selects from the job's base.
  const selection=plans=>{
    if(!sha(state.expected) || plans.some(p=>!Array.isArray(p?.changed) || !Array.isArray(p.owned)))return {};
    return {selection:{base:state.expected,paths:[...new Set(plans.flatMap(p=>[...p.changed,...p.owned]))].sort()}};
  };
  // The lead with the plan one matrix run must keep for the whole batch, and
  // each job's approved checks for the proof made before that run starts.
  const batchJob=()=>{
    const checks=job.plan?.checks?.map(c=>c.id==="go-race" && state.batch.goRace?state.batch.goRace:c);
    const members=state.batch.jobs.slice(1).map(j=>config.batch?.candidates?.find(c=>c.id===j.jobId));
    const selected=selection([job.plan,...members.map(m=>m?.plan)]);
    if(!checks)return {...job,integratedCommit:state.integrated,...selected};
    const plans=members.filter(Boolean);
    return {...job,integratedCommit:state.integrated,...selected,plan:{...job.plan,checks,checksDigest:digest(checks)},
      batchCovers:[{jobId:job.id,checks:job.plan.checks,matrixDigest:job.plan.matrixDigest},{jobId:state.batch.id,checks},...plans.map(p=>({jobId:p.id,checks:p.plan?.checks,matrixDigest:p.plan?.matrixDigest}))]};
  };
  // Fallback to per-job releases, before anything is published: the batch is
  // dropped at the hub, its members are marked to release alone, and the
  // lead goes on alone in this claim. False when the hub refuses the drop;
  // "waiting" while the batch's own matrix run is not confirmed over, since
  // the checkout must not move under it.
  const fallback=async(reason,detail)=>{
    state.batchFallback={batchId:state.batch.id,reason,...(detail && detail!==reason?{detail}:{}),members:state.batch.jobs.slice(1).map(j=>j.jobId),status:"dropping"};checkpoint();
    return finishFallback();
  };
  const finishFallback=async()=>{
    if(adapter.settleMatrixRuns && await adapter.settleMatrixRuns(job)!==true)return "waiting";
    if(!batchDropped() && !await batchOp("batch-drop")){state.batchFallback.status="refused";checkpoint();return false;}
    await config.batch?.onFallback?.(state.batchFallback.members,state.batchFallback.batchId);
    // Resumable from here as a declaration that closed with no member.
    state.batch=null;delete state.batchReceipt;state.batchFallback.status="done";state.phase="batching";checkpoint();
    git(cwd,"checkout","--detach",state.batchLead);
    state.integrated=state.batchLead;state.phase="integrated";checkpoint();
    return true;
  };
  // One matrix run and one import for the batch. Returns a waiting outcome,
  // or null once the batch is verified or has fallen back to the lead alone.
  const verifyBatch=async()=>{
    const waiting={jobId:job.id,outcome:"waiting_matrix"};
    // Saved first: every waiting return below resumes from this phase.
    state.phase="waiting_matrix";checkpoint();
    if(state.batchFallback?.status==="dropping"){const fell=await finishFallback();if(fell)return fell===true?null:waiting;}
    const clock=config.now||(()=>Date.now()),wait=config.batch?.importWaitMs??BATCH_IMPORT_WAIT_MS;
    for(let pass=0;;pass++){
      state.phase="waiting_matrix";checkpoint();await fence();
      let verified,failure;
      try{verified=await adapter.verifyIntegrated(batchJob());}catch(error){failure=error;}
      if(verified===true){state.phase="integrated";checkpoint();return null;}
      if(failure){
        const reason=failureReason(failure);
        const fell=await fallback(reason==="batch-plan-uncovered"?reason:"batch-matrix-failed",reason);
        if(fell)return fell===true?null:waiting;
        throw failure;
      }
      if(adapter.matrixImportRequested && state.batchImportRequestedAt===undefined){state.batchImportRequestedAt=clock();checkpoint();}
      if(pass || state.batchImportRequestedAt===undefined || clock()-state.batchImportRequestedAt<wait)return waiting;
      const fell=await fallback("batch-import-timeout");
      if(fell)return fell===true?null:waiting;
      // The drop was refused: the import may just have been saved, so the
      // batch stands and is read once more.
    }
  };
  // Every selected target is live-verified before this runs, so nothing here
  // rolls back: a failed push is escalated once and the release stands.
  const pushAndFinish=async receipt=>{
    state.receipt=receipt;state.phase="pushing";checkpoint();
    receipt.push={remote:"origin",commit:receipt.commit,outcome:pushRelease(cwd,receipt.commit)?"pushed":"failed"};checkpoint();
    if(receipt.push.outcome==="failed" && !state.pushEscalated){state.pushEscalated=true;checkpoint();try{await adapter.escalate({jobId:job.id,outcome:"released",push:"failed"});}catch{}}
    state.phase="finishing";checkpoint();await fence();state.finishGeneration=adapter.job?.generation??job.generation;checkpoint();await adapter.finish(receipt,state.finishGeneration);materialize();state.phase="complete";checkpoint();return receipt;
  };
  try {
    if(state.phase==="receipt_pending" || state.phase==="finishing"){
      await adapter.finish(state.receipt,state.finishGeneration);materialize();state.phase=state.receipt.outcome==="released"?"complete":"blocked";checkpoint();return state.receipt;
    }
    if(state.phase==="pushing")return await pushAndFinish(state.receipt);
    // A job with no resumable journal (a requeue) first settles the matrix
    // runs of its earlier attempts: until each is confirmed stopped, ended or
    // set aside, nothing here touches the checkout, starts a run or refuses.
    if(state.phase==="prepared" && adapter.settleMatrixRuns && await adapter.settleMatrixRuns(job)!==true)return {jobId:job.id,outcome:"waiting_matrix"};
    checkpoint();await fence();
    let integration;
    if(["waiting_matrix","waiting_inputs"].includes(state.phase))integration={expected:state.expected,integrated:state.integrated};
    else if(state.phase==="batching")integration=await resumeBatch();
    else if(config.batch?.max>1 && config.batch.candidates?.length){step={step:"integrate"};integration=await openBatch();step=null;}
    else{step={step:"integrate"};integration=integrateCandidate(cwd,job);step=null;}
    state.integrated=integration.integrated;state.expected=integration.expected;state.phase="integrated";checkpoint();
    if(state.batch){
      const waiting=await verifyBatch();if(waiting)return waiting;
      integration={expected:state.expected,integrated:state.integrated};
    }
    if(!state.batch && integration.integrated!==job.commit){
      // Saved before the matrix run is started, so a runner stopped while
      // the run waits or runs resumes this job from its attempt record.
      state.phase="waiting_matrix";checkpoint();
      await fence();if(await adapter.verifyIntegrated({...job,integratedCommit:integration.integrated,...selection([job.plan])})!==true)return {jobId:job.id,outcome:"waiting_matrix"};
      state.phase="integrated";checkpoint();
    }
    if(adapter.verifyInputs && !await adapter.verifyInputs(integration.integrated)){state.phase="waiting_inputs";checkpoint();return {jobId:job.id,outcome:"waiting_inputs"};}
    await fence();publishIntegration(cwd,integration.integrated,integration.expected);state.published=true;checkpoint();
    await adapter.merged(integration.integrated);state.phase="merged";checkpoint();
    const selected=selectReleaseTargets(cwd,baselines,integration.integrated);
    const receipt={version:1,jobId:job.id,commit:integration.integrated,verificationDigest:job.verificationDigest,targets:[],outcome:"released"};
    state.receipt=receipt;
    // The batch receipt's intent is saved with the lead's, before any deploy.
    if(state.batch)state.batchReceipt={version:1,batchId:state.batch.id,leadJobId:job.id,base:state.batch.base,integrated:integration.integrated,jobs:state.batch.jobs,dropped:state.batchDropped||[]};
    checkpoint();
    // Targets sharing one TrueNAS plan (hub and bridge together) deploy as a
    // group: both effects are journaled before the single deploy, so any
    // failure rolls both back to the prior pair.
    const done=new Set(),pinned=(t,a)=>{
      if(!a || !/^[a-f0-9]{64}$/.test(a.artifactSHA256||"") || !a.release)throw releaseError("Pinned artifact required");
      if(PAIR.includes(t) && (!a.backup || !/^[a-f0-9]{64}$/.test(a.backupSHA256||"") || !/^[a-f0-9]{64}$/.test(a.preflightReceiptSHA256||"")))throw releaseError("Handler backup and external pin required");
    };
    for(const target of selected){
      if(done.has(target))continue;
      step={step:"prepare",target};
      await fence();const artifact=await adapter.prepare(target,integration.integrated);pinned(target,artifact);
      const group=[[target,artifact]];
      for(const partner of (artifact.planTargets||[target]).filter(t=>t!==target)){
        if(!selected.includes(partner))throw releaseError("Paired plan partner is not selected");
        step={step:"prepare",target:partner};
        await fence();const other=await adapter.prepare(partner,integration.integrated);pinned(partner,other);
        if(["planPath","release","backup","backupSHA256","preflightReceiptSHA256"].some(k=>other[k]!==artifact[k]) || !same(other.planTargets,artifact.planTargets))throw releaseError("Paired plan members disagree");
        group.push([partner,other]);
      }
      // A shared backup copy is rehearsed once; the last build pinned the migration binary.
      if(group.some(([,a])=>a.schemaChanged)){step={step:"rehearse",target};await fence();if(await adapter.rehearse(group.at(-1)[1])!==true)throw releaseError("Backup-copy migration rehearsal failed");}
      const records=group.map(([t,a])=>{
        const record={target:t,release:a.release,artifactSHA256:a.artifactSHA256,...(a.backup?{backup:a.backup,backupSHA256:a.backupSHA256,preflightReceiptSHA256:a.preflightReceiptSHA256}:{}),...(a.version?{version:a.version}:{}),...(a.deployment?{deployment:a.deployment}:{}),outcome:"failed"};
        receipt.targets.push(record);state.effects.push({target:t,release:a.release,state:"attempting"});return record;
      });
      checkpoint();
      step={step:"deploy",target};
      await fence();await adapter.deploy(target,artifact);
      group.forEach(([t,a],i)=>{if(a.deployment)records[i].deployment=a.deployment;if(a.version)records[i].version=a.version;state.effects.find(e=>e.target===t).state="deployed";});checkpoint();
      for(const [i,[t,a]] of group.entries()){
        step={step:"live-check",target:t};
        const live=await liveCheck(adapter,t,policy,config.sleep,config.now),wait=adapter.probeWait?.(t,"live");
        if(wait){state.effects.find(e=>e.target===t).liveCheck=wait;checkpoint();}
        if(!live)throw releaseError("live verification failed");
        records[i].outcome="released";state.effects.find(e=>e.target===t).state="verified";checkpoint();
        // A retained copy makes this target's next rollback possible; losing it
        // only makes that later rollback unsafe, never this release.
        try{await adapter.retain?.(t,a);}catch{}
        done.add(t);
      }
    }
    step=null;
    return await pushAndFinish(receipt);
  } catch (error) {
    // An unconfirmed batch call: nothing here may touch the checkout or the job.
    if(error?.batchWait)return {jobId:job.id,outcome:"waiting_batch"};
    // The first failed target step is kept; a resumed run never rewrites it.
    if(step && !state.failure){state.failure={...step,reason:failureReason(error)};state.failureDetail??=failureDetail(error);checkpoint();}
    // The first failure's validated detail is kept even when no step was set;
    // a journal that cannot be written here is left to the checkpoints below.
    if(!state.failureDetail){state.failureDetail=failureDetail(error);try{checkpoint();}catch{}}
    if(state.phase==="pushing")return {jobId:job.id,outcome:"pushing"};
    if(state.phase==="finishing" || state.phase==="receipt_pending"){
      state.phase="receipt_pending";checkpoint();return {jobId:job.id,outcome:"receipt_pending"};
    }
    // An uncertain side effect cannot be replayed. Rollback uses only retained
    // target artifacts; the adapter must never restore an old live database.
    if(!state.published && state.effects.length===0){
      state.phase="refusing";state.refusalReason=failureReason(error);checkpoint();
      // refusalReason stays the cause; a refuse call that itself fails is named beside it.
      try{await adapter.refuse();}catch(refuseError){state.refuseFailure=failureReason(refuseError);checkpoint();throw refuseError;}
      state.phase="refused";checkpoint();await adapter.escalate({jobId:job.id,outcome:"refused",reason:state.refusalReason});throw releaseError("Release refused before publication");
    }
    let blocked=state.effects.length===0;
    // Newest effect first, except that a paired hub is restored before its
    // bridge: the bridge's rollback probe needs a responding hub, and a broken
    // new hub would otherwise leave the pair blocked.
    const order=[...state.effects].reverse(),hub=order.findIndex(e=>e.target==="hub"),bridge=order.findIndex(e=>e.target==="bridge");
    if(hub>=0 && bridge>=0 && hub>bridge)[order[hub],order[bridge]]=[order[bridge],order[hub]];
    for(const effect of order){
      if(effect.rollbackAttempted){blocked=true;continue;}
      effect.rollbackAttempted=true;checkpoint();
      let restored=false;
      try{await fence();restored=await adapter.rollback(effect.target,effect.release)===true;}catch{}
      blocked ||= !restored;effect.rollback=restored?"restored":"blocked";
      const wait=adapter.probeWait?.(effect.target,"rollback");if(wait)effect.rollbackCheck=wait;
      checkpoint();
    }
    state.phase="blocked";state.outcome=blocked?"blocked":"rolled_back";checkpoint();
    // A batch that failed after publication: its members release alone.
    if(state.batch && !state.batchSolo){state.batchSolo=true;checkpoint();try{await config.batch?.onFallback?.(state.batch.jobs.slice(1).map(j=>j.jobId),state.batch.id);}catch{}}
    if(state.published && !state.revert){
      state.revert={outcome:"failed"};checkpoint();
      try{state.revert.commit=revertCommit(cwd,job,state.integrated,state.expected);checkpoint();if(moveReleaseRef(cwd,state.revert.commit,state.integrated))state.revert.outcome="committed";}catch{}
      checkpoint();
    }
    // One attempt; a failed post must not stop the bug request or the receipt.
    const probeWaits=state.effects.flatMap(e=>[["live",e.liveCheck],["rollback",e.rollbackCheck]].filter(([,w])=>w).map(([probe,w])=>({target:e.target,probe,lastCommit:w.lastCommit,waitedMs:w.waitedMs})));
    if(!state.escalationAttempted){state.escalationAttempted=true;checkpoint();try{await adapter.escalate({jobId:job.id,outcome:state.outcome,...(state.revert?{revert:state.revert.outcome}:{}),...(state.effects.some(e=>e.rollback==="blocked")?{rollbackBlocked:true}:{}),...(probeWaits.length?{probeWaits}:{})});}catch{}}
    if(state.revert && !state.bugRequestAttempted){
      state.bugRequestAttempted=true;checkpoint();
      try{state.revert.bugRequestId=await adapter.requestBug({jobId:job.id,commit:state.revert.commit,outcome:state.revert.outcome});}catch{}
      checkpoint();
    }
    if(state.receipt){
      state.receipt.outcome=state.outcome;
      for(const target of state.receipt.targets){const effect=state.effects.find(e=>e.target===target.target);if(effect?.rollback){target.rollback=effect.rollback;target.outcome=effect.rollback==="restored"?"rolled_back":"failed";}}
      if(state.revert)state.receipt.revert={...(state.revert.commit?{commit:state.revert.commit}:{}),outcome:state.revert.outcome,...(state.revert.bugRequestId?{bugRequestId:state.revert.bugRequestId}:{})};
      // Retryable like the success receipt: a lost response resumes this
      // exact receipt and generation, with no second rollback or escalation.
      state.finishGeneration=adapter.job?.generation??job.generation;state.phase="receipt_pending";checkpoint();
      try{await adapter.finish(state.receipt,state.finishGeneration);}catch{throw releaseError("Release failed; final receipt pending retry");}
      try{materialize();}catch{throw releaseError("Release failed; batch receipt pending retry");}
      state.phase="blocked";checkpoint();
    }else{await adapter.block(job.id);}
    throw releaseError("Release failed; inspect saved journal");
  } finally {closeSync(fd);rmSync(lock);}
}

// The private activation config names existing host credential stores and
// handler-produced preflight files. Probe/rollback programs are pinned host
// programs, never commands received from Board text.
export function compatibilityArgv(argv) {
  if (!Array.isArray(argv) || argv.some(a=>typeof a!=="string" || /[\0\r\n]/.test(a))) throw releaseError("Invalid compatibility argv");
  const out = [...argv];
  if (out[0] === "deployment" && ["list", "get"].includes(out[1])) out[1] = "compat-" + out[1];
  return out;
}
export function dispatchCompatibility(binary, argv, {run = spawnSync} = {}) {
  if (!isAbsolute(binary || "")) throw releaseError("Absolute pinned compatibility binary required");
  const path = realpathSync(binary), stat = statSync(path);
  if (!stat.isFile() || !(stat.mode & 0o111) || path === realpathSync(fileURLToPath(import.meta.url)) || path === realpathSync(process.execPath)) throw releaseError("Recursive compatibility executable refused");
  const result = run(path, compatibilityArgv(argv), {stdio:"inherit"});
  if (result.error || result.signal || !Number.isInteger(result.status)) throw releaseError("Compatibility executable failed");
  return result.status;
}
// A successful native action is still untrusted until its full record matches
// the exact job/run and operation. Never replace the execution fence with a
// compact receipt, wrong identity, stale generation or unrelated input pins.
export function validateNativeRelease(prior, next, operation, expectedGeneration, extra = [], receipt) {
  const immutable = ["id", "taskId", "entryId", "itemId", "itemRevision", "scopeRevision", "orderMessageSeq", "repository", "baseCommit", "commit", "verificationDigest", "pauseGeneration"];
  const samePin = k => next?.[k] === prior[k];
  const flag = n => extra[extra.indexOf(n)+1];
  const expectedState = {claim:"claimed", check:prior.state, merged:"merged", finish:receipt?.outcome, block:"blocked", refuse:"refused", "batch-open":"claimed", "batch-add":"claimed", "batch-drop":"claimed"}[operation];
  const advance = operation === "check" ? 0 : 1;
  const allowed = {claim:["verified"],check:["claimed","merged"],merged:["claimed"],finish:["merged"],block:["claimed","merged"],refuse:["claimed"],"batch-open":["claimed"],"batch-add":["claimed"],"batch-drop":["claimed"]};
  if (!allowed[operation]?.includes(prior.state)) throw releaseError("Invalid native release transition");
  if (!next || Array.isArray(next) || next.summary === true || !immutable.every(samePin) || !Number.isSafeInteger(expectedGeneration) || expectedGeneration < 1 || next.generation !== expectedGeneration + advance || !expectedState || next.state !== expectedState) throw releaseError("Invalid native release response");
  const agent = operation === "claim" ? process.env.TAILTERM_AGENT : prior.agentId, run = operation === "claim" ? process.env.TAILTERM_RUN : prior.runId;
  if (next.agentId !== agent || next.runId !== run) throw releaseError("Native release run binding mismatch");
  for (const k of ["inputsCommit", "inputsDigest"]) if (!samePin(k)) throw releaseError("Native release input binding mismatch");
  // A batch-add's --commit is the job's range end, never the lead's integrated
  // commit; batch-drop is the one place a cleared integrated commit is expected.
  const integrated = operation === "merged" ? flag("--commit") : operation === "batch-drop" ? undefined : prior.integratedCommit;
  if (next.integratedCommit !== integrated || (operation === "merged" && !sha(integrated))) throw releaseError("Native integrated commit mismatch");
  if (operation === "finish") {
    if (!receipt || receipt.version !== 1 || receipt.jobId !== prior.id || receipt.commit !== prior.integratedCommit || receipt.verificationDigest !== prior.verificationDigest || !Array.isArray(receipt.targets) || !next.receipt || digest(next.receipt) !== digest(receipt)) throw releaseError("Native full receipt mismatch");
  } else if (!same(prior.receipt, next.receipt)) throw releaseError("Native receipt changed unexpectedly");
  return next;
}

export class HostAdapter {
  constructor(config,job){this.config=config;this.job=job;this.artifacts=new Map();this.serial=0;this.now=()=>Date.now();
    // The TailOS poller's fetch and clock; tests replace them.
    this.probeDeps={fetchJSON:hostDeps.fetchJSON,sleep:hostDeps.sleep,now:hostDeps.now};this.probeWaits=new Map();
    // What the last poll saw: a wait on the host list, or a held run.
    this.matrixChildren=MATRIX_CHILDREN;this.matrixWait=null;this.matrixHeld=null;
    // Failed calls of the configured CLI this adapter recorded privately.
    this.cliFailures=[];}
  command(argv,cwd=this.config.cwd,{timeout=600000}={}){
    if(!Array.isArray(argv)||!argv.length||argv.some(a=>typeof a!=="string"||/[\0\r\n]/.test(a)))throw releaseError("Invalid host operation argv");
    try{return execFileSync(argv[0],argv.slice(1),{cwd,encoding:"utf8",stdio:["ignore","pipe","pipe"],maxBuffer:256*1024*1024,timeout});}
    catch(error){
      const cli=this.config.tt||"tt",failed=new Error("Host operation failed");failed.releaseReason=childReason(argv,error,cli);
      if(argv[0]===cli)failed.refusal=refusalText(error.stderr);
      if(argv[0]===cli && this.config.journalDirectory){try{this.cliFailures.push(recordCliFailure(this.config,this.job,argv,cwd,error,failed.releaseReason,this.now()));}catch{}}
      throw failed;
    }
  }
  detail(id){
    const job=JSON.parse(this.command([this.config.tt||"tt","deployment","get","--job",id]));
    const immutable=["id","taskId","entryId","itemId","itemRevision","scopeRevision","orderMessageSeq","repository","baseCommit","commit","verificationDigest","agentId","runId","pauseGeneration"];
    if(job?.id!==id || job.summary===true || (this.job.id && (!immutable.every(k=>job[k]===this.job[k]) || !Number.isSafeInteger(job.generation) || job.generation<this.job.generation || !["claimed","merged"].includes(job.state))))throw releaseError("Exact release detail required");
    if(this.job.integratedCommit && job.integratedCommit!==this.job.integratedCommit)throw releaseError("Exact integrated release detail required");
    if(this.job.inputsDigest && (job.inputsCommit!==this.job.inputsCommit || job.inputsDigest!==this.job.inputsDigest))throw releaseError("Exact input release detail required");
    return job;
  }
  native(operation,extra=[],expectedGeneration=this.job.generation){
    if(!Number.isSafeInteger(expectedGeneration) || expectedGeneration<1)throw releaseError("Exact native release generation required");
    const args=[this.config.tt||"tt","deployment",operation,"--job",this.job.id,"--generation",String(expectedGeneration),"--request-id",`${this.job.id}-${operation}-${expectedGeneration}`,...extra];
    const next=JSON.parse(this.command(args));
    const receipt=operation==="finish"?JSON.parse(readFileSync(extra[extra.indexOf("--file")+1],"utf8")):undefined;
    this.job=validateNativeRelease(this.job,next,operation,expectedGeneration,extra,receipt);return this.job;
  }
  async fence(){try{this.native("check");return true;}catch{return false;}}
  // The project handler by the project rule (hub operation "handler"): a
  // finished entry holds no item lease, so role addressing is refused there.
  handler(){const h=JSON.parse(this.command([this.config.tt||"tt","deployment","handler"]));if(!/^agt_[a-f0-9]+$/.test(h?.id||""))throw releaseError("Project database handler required");return h.id;}
  async requestBug({commit,outcome}){
    const id=`${this.job.id}-rollback-bug`;
    this.command([this.config.tt||"tt","send","--kind","request","--to",this.handler(),"--subject","File a bug for a release that was rolled back","--ask",`Release job ${this.job.id} was rolled back after publication. File a bug linked to item ${this.job.itemId} so the change is fixed before it ships again. The tasks-hub revert ${commit||"was not created"} is ${outcome}; the private host journal has the rest.`,"--request-id",id,"--work-item",this.job.itemId,"--work-item-revision",String(this.job.itemRevision),"--work-order-message",String(this.job.orderMessageSeq),"--ref",`release-job=${this.job.id}`,...(commit?["--ref",`revert-commit=${commit}`]:[])]);
    return id;
  }
  async merged(commit){this.native("merged",["--commit",commit]);}
  // batch-open, batch-add and batch-drop on the claimed lead, at the lead
  // generation the runner journaled before sending.
  async batch(operation,{entryId,commit}={},generation){
    if(!["batch-open","batch-add","batch-drop"].includes(operation))throw releaseError("Invalid release batch operation");
    this.native(operation,[...(entryId?["--entry",entryId]:[]),...(commit?["--commit",commit]:[])],generation);return true;
  }
  async generation(){return this.detail(this.job.id).generation;}
  async verifyIntegrated(job){
    // Independent release verification is imported by the handler. It is not
    // satisfied by deployer self-certification or a candidate-SHA receipt.
    const current=this.detail(job.id);
    if(current?.integratedCommit===job.integratedCommit && current.integratedVerification?.commit===job.integratedCommit){this.job=current;return true;}
    // One directory per integrated commit and requeue attempt: a handler
    // requeue (a new reconciliation) never reuses an earlier attempt's host
    // wait or receipt, while a runner resumed within an attempt does.
    if(!sha(job.integratedCommit))throw releaseError("Exact integrated commit required");
    const dir=join(this.config.journalDirectory,job.id+"-integrated-verification",`${job.integratedCommit}-r${job.reconciliations?.length||0}`);mkdirSync(dir,{recursive:true,mode:0o700});
    const contextPath=join(dir,"context.json"),planPath=join(dir,"plan.json"),receiptPath=join(dir,"receipt.json");
    this.matrixWait=null;this.matrixHeld=null;
    let run=this.readRun(dir);
    if(!run && !existsSync(receiptPath))return this.launchMatrixRun(job,current,dir,{contextPath,planPath});
    if(run && run.state!=="ended"){
      if(this.resolveMatrixRun(dir,run)!=="ended")return false;
      run=this.readRun(dir);
    }
    if(run?.refusal)throw releaseError(run.refusal);
    if(!existsSync(receiptPath))throw releaseError("Integrated matrix run ended without a receipt");
    // The receipt is read only once its run is gone, and imported only when eligible.
    if(!receiptEligible(JSON.parse(readFileSync(receiptPath,"utf8"))))throw releaseError("Integrated matrix receipt is not eligible");
    this.command([this.config.tt||"tt","send","--kind","request","--to",this.handler(),"--subject","Import verification for the integrated release commit","--ask",`Import release verification plan and receipt for job ${job.id} integrated commit ${job.integratedCommit} through tt deployment verification --plan-file and --file. Preserve exact job generation and inspect logs; release publication waits for saved import.`,"--request-id",`${job.id}-integrated-matrix-${job.integratedCommit}`,"--ref",`release-job=${job.id}`,"--ref",`integrated-commit=${job.integratedCommit}`,"--attachment",planPath,"--attachment",receiptPath]);
    this.matrixImportRequested=true;
    return false;
  }
  // The integrated checkout's matrix file can differ from the one the job was
  // approved under (tasks-hub gained a matrix change). The integrated plan
  // then binds that digest and the newest owner approval of exactly it, taken
  // from the job list as a hint the hub proves at import; the job's own
  // approval is never reused. With none the job is refused by name, before
  // any matrix run. An unchanged digest adds nothing to the job's plan, and an
  // unreadable matrix file or a plan with no digest is left for the plan
  // command to refuse.
  integratedMatrix(job,current){
    let integrated;try{integrated=digest(readFileSync(join(this.config.cwd,"verification/matrix.json"),"utf8"));}catch{return {};}
    const approved=job.plan?.matrixDigest;
    if(integrated===approved || !/^[a-f0-9]{64}$/.test(approved||""))return {};
    const seq=Math.max(0,...(current?.matrixApprovals||[]).filter(a=>a?.digest===integrated && Number.isSafeInteger(a.messageSeq)).map(a=>a.messageSeq));
    if(!(seq>0))throw releaseError(`Matrix digest changed ${approved.slice(0,8)} to ${integrated.slice(0,8)}; no owner approval covers it`);
    return {approvedMatrixDigest:integrated,matrixApprovalMessageSeq:seq};
  }
  // The attempt record DIR/run.json: "starting" is saved before the run is
  // spawned, "started" names its pid and process start time, "ended" is final.
  // A launch is never replayed: whatever the record says, no second run is
  // started in the same directory.
  launchMatrixRun(job,current,dir,{contextPath,planPath}){
    const missing=missingPrerequisites(this.config.cwd);if(missing.length)throw prerequisiteError(missing);
    const matrix=this.integratedMatrix(job,current),priority=matrixPriority(job,this.config);
    const wait=this.config.matrixHostWaitMs??MATRIX_HOST_WAIT_MS;
    if(!Number.isSafeInteger(wait)||wait<=0)throw releaseError("Invalid matrix host wait");
    const minutes=Math.min(1440,Math.max(1,Math.floor(wait/60000)));
    // Checks are selected from the tip the candidate was integrated onto,
    // when the runner named one; baseCommit stays the job's bound base.
    save(contextPath,{...job.plan,...matrix,commit:job.integratedCommit,verifierAgentId:this.job.agentId,verifierRunId:this.job.runId,...(job.selection?{selectionBaseCommit:job.selection.base,selectionPaths:job.selection.paths}:{})});
    this.command(["node","scripts/verify-matrix.mjs","plan",contextPath,planPath]);
    const plan=JSON.parse(readFileSync(planPath,"utf8"));
    // A batch's one run must cover every job's approved checks, as the hub
    // will require of its import; proven here, before any matrix run.
    if(job.batchCovers && batchPlanCovers(plan,job.batchCovers))throw releaseError("batch-plan-uncovered");
    const boundMs=matrixRunTimeout(plan);
    const run={version:1,state:"starting",launchedAt:this.now(),priority,hostWaitMs:minutes*60000,boundMs};
    // A failed intent save starts nothing and takes the ordinary refusal path.
    this.saveRun(dir,run);
    let pid;
    try{pid=this.startMatrixRun(["node","scripts/verify-matrix.mjs","run",planPath,dir,"--priority",priority,"--item",NAME(job.itemId??this.job.itemId),"--host-wait-minutes",String(minutes)],dir);if(!Number.isSafeInteger(pid)||pid<=0)throw releaseError("No matrix run pid");}
    catch{
      // Nothing was started, so this attempt is over.
      try{this.saveRun(dir,{...run,state:"ended",reason:"spawn-failed",refusal:"Integrated matrix run could not start"});}catch{}
      throw releaseError("Integrated matrix run could not start");
    }
    // A run now exists, so a failed save must not refuse: the job keeps the
    // fence and the next poll adopts the run from the lock file.
    try{this.saveRun(dir,{...run,state:"started",pid,processStartedAt:this.processStartTime(pid),groups:[]});}catch{}
    return false;
  }
  // Detached, so the run outlives a stopped runner; it is its own group leader.
  startMatrixRun(argv,dir){
    const out=openSync(join(dir,"run.out"),"a",0o600),err=openSync(join(dir,"run.err"),"a",0o600);
    try{
      // The marker puts this run ahead of team runs on the host waitlist.
      const child=spawn(argv[0],argv.slice(1),{cwd:this.config.cwd,detached:true,stdio:["ignore",out,err],env:{...process.env,TAILTERM_MATRIX_RELEASE:"1"}});
      child.on("error",()=>{});
      if(!Number.isSafeInteger(child.pid))throw releaseError("Matrix run not started");
      child.unref();this.matrixChildren.set(dir,child);return child.pid;
    }finally{closeSync(out);closeSync(err);}
  }
  saveRun(dir,run){save(join(dir,"run.json"),run);}
  readRun(dir){return readRun(dir);}
  // One pid only; the text is compared, never parsed for anything but order.
  processStartTime(pid){
    try{return execFileSync("ps",["-o","lstart=","-p",String(pid)],{encoding:"utf8",stdio:["ignore","pipe","ignore"],timeout:5000}).trim()||null;}catch{return null;}
  }
  signalProcess(target,name){process.kill(target,name);}
  pidGone(pid){return pidGone(pid);}
  groupGone(pgid){return groupGone(pgid);}
  hostState(){return readHostState(lockPath());}
  // The run's entry in the host lock file, found by its output directory:
  // null when it has none, undefined when the file cannot be read.
  matrixEntry(dir,run){
    let state;try{state=this.hostState();}catch{return undefined;}
    if(!state)return null;
    try {
      const output=canonicalResource(dir), matches=[];
      for(const entry of holdersOf(state)) if(entry.output && canonicalResource(entry.output)===output) matches.push({state,role:"holder",entry});
      state.waiters.forEach((entry,index)=>{if(entry.output && canonicalResource(entry.output)===output)matches.push({state,role:"waiter",index,entry});});
      if(matches.length>1)return {ambiguous:true};
      const match=matches[0];
      if(!match)return null;
      let leaseId=run?.leaseId;
      try {leaseId ||= JSON.parse(readFileSync(join(dir,"host-lock.json"),"utf8")).id;} catch {}
      if((leaseId && match.entry.id!==leaseId) || (run?.pid && match.entry.pid!==run.pid))return null;
      return match;
    } catch {return undefined;}
  }
  // The outcome the run itself recorded on leaving the list (released,
  // wait-expired, withdrawn, or a failure while waiting); null until then.
  matrixSidecar(dir){
    try{const outcome=JSON.parse(readFileSync(join(dir,"host-lock.json"),"utf8"))?.outcome;return typeof outcome==="string" && !["waiting","holding"].includes(outcome)?outcome:null;}catch{return null;}
  }
  // One non-blocking step for an attempt that is not ended: "waiting",
  // "held" or "ended" (saved). With stop, the run is stopped now instead of at
  // its deadline (an earlier attempt of a requeued job). A pid is signalled
  // only with process-instance proof: this process holds the live child, or
  // the pid's process start time is the one recorded at launch. A matching
  // pid and directory in the lock file is not proof. Check groups are never
  // signalled. Held means the run could not be confirmed stopped; the job
  // then keeps the fence until it can.
  resolveMatrixRun(dir,run,stop=false){
    const now=this.now(),child=this.matrixChildren.get(dir),found=this.matrixEntry(dir,run),sidecar=this.matrixSidecar(dir);
    const persist=()=>{try{this.saveRun(dir,run);return true;}catch{return false;}};
    const alive=()=>[...new Set([...(run.snapshot?.groups||[]),...(run.groups||[])])].filter(g=>!this.groupGone(g));
    const held=reason=>{persist();this.matrixHeld={attempt:basename(dir),pid:run.pid??null,groups:run.state==="started"?alive():[],reason};return "held";};
    const end=(reason,refusal)=>{Object.assign(run,{state:"ended",reason,endedAt:now,...(refusal?{refusal}:{})});if(!persist())return "waiting";this.matrixChildren.delete(dir);return "ended";};
    // A run that was not stopped and left a receipt ended normally, whatever
    // the record knew of it; the receipt's eligibility is checked at import.
    const ending=()=>run.stopRequestedAt?end("stopped","Integrated matrix run exceeded its bound"):existsSync(join(dir,"receipt.json"))?end("receipt"):sidecar==="wait-expired"?end("wait-expired","Verification host wait expired"):end("no-receipt","Integrated matrix run ended without a receipt");
    if(found?.ambiguous)return held("host lock identity ambiguous");
    if(found?.entry)run.leaseId ||= found.entry.id;
    if(run.state==="unreadable"){this.matrixHeld={attempt:basename(dir),pid:null,groups:[],reason:"attempt record unreadable"};return "held";}
    if(run.state==="starting"){
      // An unconfirmed launch: adopt the run from this process's child or
      // from the lock file. No timer refuses it.
      const pid=child?.pid??found?.entry.pid;
      if(Number.isSafeInteger(pid) && pid>0){
        // A lock-file pid is ours only if that process began before it joined.
        const started=this.processStartTime(pid),ours=child || (started && Date.parse(started)<=Date.parse(found.entry.requestedAt)+1000);
        Object.assign(run,{state:"started",pid,processStartedAt:ours?started:null,groups:[]});
        if(!persist())return "waiting";
      }
      else if(sidecar)return ending();
      else if(!stop && now-run.launchedAt<(this.config.matrixLaunchGraceMs??MATRIX_LAUNCH_GRACE_MS))return "waiting";
      else return held("matrix run launch unconfirmed");
    }
    const mine=found && found.entry.pid===run.pid?found:null;
    if(mine?.role==="holder")run.groups=[...new Set([...(run.groups||[]),...(mine.entry.groups||[]).filter(Number.isSafeInteger)])];
    const exited=child?child.exitCode!==null||child.signalCode!==null:this.pidGone(run.pid);
    if(!exited){
      if(!stop && now<run.launchedAt+run.hostWaitMs+run.boundMs+RUN_TIMEOUT_GRACE_MS+MATRIX_DEADLINE_SLACK_MS){
        const holders=mine ? holdersOf(mine.state) : [];
        if(mine?.role==="waiter"){
          const place=`${mine.index+1}/${mine.state.waiters.length}/${holders.map(h=>h.id).sort().join(",")}`;
          if(run.waitPlace!==place){run.waitPlace=place;run.waitChange=(run.waitChange||0)+1;}
          this.matrixWait={attempt:basename(dir),position:mine.index+1,length:mine.state.waiters.length,priority:run.priority,change:run.waitChange,holders:holders.map(h=>({id:h.id,item:h.item,agent:h.agent,pid:h.pid}))};
        }
        persist();return "waiting";
      }
      if(!(run.pid>1) || (!child && !(run.processStartedAt && this.processStartTime(run.pid)===run.processStartedAt)))return held("no process-instance proof for the matrix run pid");
      const grace=this.config.matrixStopGraceMs??MATRIX_STOP_GRACE_MS;
      // SIGTERM takes the run's own interruption path: it stops its checks,
      // removes its home and releases the host lock.
      if(!run.stopRequestedAt){run.stopRequestedAt=now;persist();try{this.signalProcess(run.pid,"SIGTERM");}catch{}return "waiting";}
      if(!run.killedAt && now-run.stopRequestedAt>=grace){run.killedAt=now;persist();try{this.signalProcess(-run.pid,"SIGKILL");}catch{}return "waiting";}
      if(run.killedAt && now-run.killedAt>=grace)return held("matrix run pid still alive after SIGKILL");
      persist();return "waiting";
    }
    run.goneAt??=now;
    if(existsSync(join(dir,"receipt.json")) && !run.stopRequestedAt)return end("receipt");
    if(!sidecar){
      // Without the run's own release record, its check groups are known only
      // from its lock entry as read after it was gone, which it can no longer
      // change: the entry is read again here, after the pid was seen gone.
      // Another waiter may have replaced that entry first.
      const after=this.matrixEntry(dir,run);
      if(after?.ambiguous)return held("host lock identity ambiguous");
      const last=after?.entry?.pid===run.pid?after:null;
      if(last)run.snapshot={at:now,role:last.role,groups:(last.entry.groups||[]).filter(Number.isSafeInteger)};
      if(!run.snapshot)return held(after===undefined?"host lock file unreadable":"check group state cannot be shown");
      if(alive().length)return held("a check group of the matrix run is still alive");
    }
    return ending();
  }
  // Every attempt of this job that is neither ended nor set aside is stopped
  // and confirmed before the job integrates again. True when none remains.
  async settleMatrixRuns(job){
    this.matrixWait=null;this.matrixHeld=null;
    const base=join(this.config.journalDirectory,job.id+"-integrated-verification");let names;
    try{names=readdirSync(base).sort();}catch(error){if(error.code==="ENOENT")return true;this.matrixHeld={attempt:ALL_ATTEMPTS,pid:null,groups:[],reason:"attempt records unreadable"};return false;}
    let settled=true;
    for(const name of names){
      const dir=join(base,name),run=this.readRun(dir);
      if(run && run.state!=="ended" && this.resolveMatrixRun(dir,run,true)!=="ended")settled=false;
    }
    return settled;
  }
  async verifyInputs(commit){
    const current=this.detail(this.job.id);
    if(current?.inputsCommit===commit && /^[a-f0-9]{64}$/.test(current.inputsDigest||"")){this.job=current;this.jobInputs(commit);return true;}
    this.command([this.config.tt||"tt","send","--kind","request","--to",this.handler(),"--subject","Import immutable inputs for this release job","--ask",`Prepare the private manifest ${join(this.config.journalDirectory,this.job.id+"-inputs.json")} for exact job ${this.job.id} accepted ${this.job.commit} integrated ${commit}; import its digest with tt deployment inputs --job --generation --commit --file. Include fresh exact-job backup/preflight pins and rollback programs; publication waits for saved handler input binding.`,"--request-id",`${this.job.id}-inputs-${commit}`,"--work-item",this.job.itemId,"--work-item-revision",String(this.job.itemRevision),"--work-order-message",String(this.job.orderMessageSeq),"--ref",`release-job=${this.job.id}`]);return false;
  }
  jobInputs(commit){
    const path=join(this.config.journalDirectory,this.job.id+"-inputs.json"),raw=readFileSync(path,"utf8");
    if(fileDigest(path)!==this.job.inputsDigest || this.job.inputsCommit!==commit)throw releaseError("Handler input digest binding required");
    const input=JSON.parse(raw);
    if(input.version!==1 || input.jobId!==this.job.id || input.commit!==commit || input.acceptedCommit!==this.job.commit || input.verificationDigest!==this.job.verificationDigest)throw releaseError("Exact job input binding required");
    // Two TrueNAS plans made before either deploy would each pin the other's
    // pre-release mount, so hub and bridge together must share one plan.
    const {hub,bridge}=input.targets||{};
    if(hub && bridge && (!same(hub.planTargets,PAIR) || !same(bridge.planTargets,PAIR) || ["release","planPath","preflightReceipt","preflightReceiptSHA256","backup","backupSHA256"].some(k=>hub[k]!==bridge[k])))throw releaseError("Hub and bridge need one paired plan");
    return input;
  }
  captureMiniRollback(artifact){
    const path=join(this.config.journalDirectory,this.job.id+"-mini-before");
    copyFileSync(artifact.installPath,path,constants.COPYFILE_EXCL);
    artifact.rollbackPath=path;artifact.priorArtifactSHA256=fileDigest(path);artifact.rollbackCaptured=true;
  }
  async prepare(target,commit){
    if(git(this.config.cwd,"rev-parse","HEAD")!==commit)throw releaseError("Candidate build checkout mismatch");
    const t=this.config.targets[target];if(!t)throw releaseError("Target host config required");
    const input=this.jobInputs(commit),perJob=input.targets?.[target];if(!perJob)throw releaseError("Exact job target input required");
    const planTargets=PAIR.includes(target)?perJob.planTargets||[target]:undefined,paired=same(planTargets,PAIR);
    const release=this.job.id+"-"+commit.slice(0,12)+"-"+(paired?"truenas":target);
    if(perJob.release!==release)throw releaseError("Unique job release identity required");
    // Stable config contains private probe/host references only. Backup, release,
    // compatibility and rollback inputs come from the handler-pinned job manifest.
    const artifact={installPath:t.installPath,relayRestart:t.relayRestart,hostSetup:t.hostSetup,hostRollback:t.hostRollback,liveProbe:t.liveProbe,rollbackProbe:t.rollbackProbe,...perJob,release,commit,rollbackSafe:perJob.rollbackSafe===true,...(planTargets?{planTargets}:{})};
    const baselines=this.baselines||this.config.baselines;
    artifact.schemaChanged=["hub","bridge"].includes(target) && schemaChanged(this.config.cwd,baselines.hub,commit);
    if(target==="tailos"){
      if(diffPaths(this.config.cwd,baselines.tailos,commit).includes("package-lock.json"))this.command(["npm","ci"]);
      this.command(["npm","run","build:static"]);this.command(["npm","run","verify:release"]);
      const manifest=JSON.parse(readFileSync(join(this.config.cwd,"dist-static/release.json"),"utf8"));
      if(manifest.commit!==commit)throw releaseError("Static commit mismatch");
      artifact.artifactSHA256=digest(readFileSync(join(this.config.cwd,"dist-static/release.json"),"utf8"));
    }else{
      const filename={hub:"tailterm-hub-linux-amd64",bridge:"tailterm-discord-linux-amd64",mini:"tt-"+this.job.id}[target];
      const output=join(this.config.cwd,".build/ttbin",filename);mkdirSync(dirname(output),{recursive:true,mode:0o700});
      const pkg={hub:"tailterm-hub",bridge:"tailterm-discord",mini:"tt"}[target];
      const os=target==="mini"?"darwin":"linux",arch=target==="mini"?"arm64":"amd64";
      withBuildCheckout(this.config.cwd,commit,src=>this.command(["env","CGO_ENABLED=0",`GOOS=${os}`,`GOARCH=${arch}`,"go","build","-trimpath","-ldflags=-s -w","-o",output,`./cmd/${pkg}`],join(src,"hub")));
      this.requireStamp(output,commit);
      artifact.artifactPath=output;artifact.artifactSHA256=fileDigest(output);if(target==="mini")artifact.version=commit;
    }
    if(artifact.schemaChanged){
      if(perJob.backupJobId!==this.job.id || !perJob.backup || !perJob.backup.includes(this.job.id))throw releaseError("Fresh job backup identity required");
      artifact.migrationBinary=join(this.config.journalDirectory,this.job.id+"-migration");
      withBuildCheckout(this.config.cwd,commit,src=>this.command(["env","CGO_ENABLED=0",`GOOS=${process.platform}`,`GOARCH=${process.arch==="arm64"?"arm64":"amd64"}`,"go","build","-trimpath","-o",artifact.migrationBinary,"./cmd/tailterm-hub"],join(src,"hub")));
      this.requireStamp(artifact.migrationBinary,commit);
      artifact.migrationBinarySHA256=fileDigest(artifact.migrationBinary);
    }
    if(["hub","bridge"].includes(target)){
      if(perJob.backupJobId!==this.job.id || !perJob.backup?.includes(this.job.id))throw releaseError("Fresh job backup identity required");
      const plan=JSON.parse(readFileSync(perJob.planPath,"utf8"));
      if(plan.deployment.releaseName!==release || plan.backupDestination!==perJob.backup || fileDigest(perJob.preflightReceipt)!==perJob.preflightReceiptSHA256)throw releaseError("Exact job preflight binding required");
      // Every target the plan changes mounts this release; a partner pinned
      // anywhere else would put an older binary back.
      if(!planTargets.includes(target) || !same(plan.deployment.targets,planTargets) || planTargets.some(p=>plan.deployment[DESTINATION[p][0]]!==`${TRUENAS_BASE}/releases/${release}/${DESTINATION[p][1]}`))throw releaseError("Plan must mount this release for every target it changes");
    }
    if(target==="mini")this.captureMiniRollback(artifact);
    this.artifacts.set(target,artifact);return artifact;
  }
  // The live probe identifies a release by this stamp, so an artifact
  // without it is refused here, before any deploy.
  requireStamp(path,commit){
    let info;try{info=buildInfo({run:argv=>({status:0,stdout:this.command(argv)})},path);}catch{throw releaseError("Go artifact has no build revision");}
    if(info.commit!==commit || info.integrity!==true)throw releaseError("Go artifact revision does not match the integrated commit");
  }
  async rehearse(a){
    if(!a.backupCopy || !a.migrationBinary)throw releaseError("Handler backup-copy import required");
    if(fileDigest(a.backupCopy)!==a.backupSHA256)throw releaseError("Imported backup hash mismatch");
    // The migrated copy is removed when the rehearsal ends, pass or fail. The
    // marker beside it, not the copy, refuses a second attempt; it is saved
    // before the copy exists and holds no path, output or error text.
    const copy=a.backupCopy+".rehearsal-"+this.job.id,marker=copy+".json";
    if(existsSync(marker))throw releaseError("Rehearsal already attempted; inspect prior attempt");
    const record={version:1,jobId:this.job.id,backupSHA256:a.backupSHA256,startedAt:new Date(this.now()).toISOString(),outcome:"started"};
    save(marker,record);
    const clear=()=>{let gone=true;for(const suffix of COPY_FILES){try{rmSync(copy+suffix,{force:true});}catch{gone=false;}}return gone;};
    let outcome="failed";
    try{
      // A copy an earlier run left behind is replaced, sidecars included.
      clear();copyFileSync(a.backupCopy,copy);
      if(fileDigest(a.migrationBinary)!==a.migrationBinarySHA256)throw releaseError("Candidate migration binary changed");
      this.command([a.migrationBinary,"--migrate-only",copy]);outcome="passed";
    }finally{
      // Cleanup never changes the rehearsal's result; a copy left here is
      // removed by the retention sweep once the job is terminal.
      const copyRemoved=clear();
      try{save(marker,{...record,outcome,endedAt:new Date(this.now()).toISOString(),copyRemoved});}catch{}
    }
    return true;
  }
  async deploy(target,a){
    if(a.artifactPath && fileDigest(a.artifactPath)!==a.artifactSHA256)throw releaseError("Pinned candidate artifact changed");
    if(target==="hub" || target==="bridge"){
      const plan=JSON.parse(readFileSync(a.planPath,"utf8")),planTargets=a.planTargets||[target];
      if(!planTargets.includes(target) || !same(plan.deployment.targets,planTargets))throw releaseError("Exact middleware plan required");
      // One run deploys every member of a paired plan, so each partner must be
      // prepared from the same plan with its pinned artifact unchanged.
      for(const p of planTargets.filter(t=>t!==target)){const other=this.artifacts.get(p);if(!other || other.planPath!==a.planPath || other.release!==a.release || !other.artifactPath || fileDigest(other.artifactPath)!==other.artifactSHA256)throw releaseError("Paired artifact changed");}
      this.command(["python3","scripts/deploy-truenas-hub.py",a.release,"--plan",a.planPath,"--preflight-receipt",a.preflightReceipt,"--preflight-receipt-sha256",a.preflightReceiptSHA256,"--update"]);
    }else if(target==="mini"){
      if(!a.rollbackCaptured || fileDigest(a.rollbackPath)!==a.priorArtifactSHA256 || fileDigest(a.installPath)!==a.priorArtifactSHA256)throw releaseError("Exact prior-live Mini rollback required");
      // The live probe counts relay errors only after this deploy.
      const log=this.config.targets?.mini?.relayLog;if(log)save(join(this.config.journalDirectory,"mini-relay-offset.json"),{jobId:this.job.id,offset:existsSync(log)?statSync(log).size:0});
      // tt host setup installs the binary with its own rollback copy, updates
      // the hooks, restarts the relay and runs doctor (docs/host-setup.md).
      this.command(a.hostSetup||[a.artifactPath,"host","setup","--from",a.artifactPath]);
      if(fileDigest(a.installPath)!==a.artifactSHA256)throw releaseError("Mini install does not match the pinned artifact");
    }else{
      const output=this.command(["npx","wrangler","pages","deploy","dist-static","--project-name","tailos","--branch","main","--commit-hash",a.commit,"--commit-dirty=false"]);
      // Output is kept out of messages/receipts; a host probe supplies the
      // immutable deployment ID only after independently resolving it.
      const match=output.match(/https:\/\/([a-f0-9]{8})\.tailos\.pages\.dev/);if(!match)throw releaseError("TailOS deployment identity unconfirmed");a.deployment=match[0];
    }
  }
  async check(target){
    const a=this.artifacts.get(target);if(!a)return "identity";
    // The poller owns the whole switch window, so a mismatch after it is an
    // identity failure and liveCheck's generic retry never stretches it.
    if(target==="tailos"){
      try{const w=await waitForTailOSCommit(tailosURL(this.config),a.commit,{windowMs:tailosWindow(this.config),deps:this.probeDeps});this.probeWaits.set("tailos:live",{lastCommit:w.lastCommit,waitedMs:w.waitedMs});return w.matched?true:"identity";}
      catch{return "identity";}
    }
    if(PAIR.includes(target))this.probeWaits.delete(target+":live");
    try{
      const r=JSON.parse(this.command(a.liveProbe));
      const waited=PAIR.includes(target) && this.readyWait(target,"live",r);
      if(r.commit!==a.commit || r.artifactSHA256!==a.artifactSHA256)return "identity";
      if(r.integrity!==true)return "integrity";
      if(target==="mini")return r.relayRunning===true && r.newErrors===0;
      const ready=r.hubResponds===true && r.migrationsApplied===true && r.containersRunning===true;
      // The hub and bridge probes own the whole readiness window, so a probe
      // that waited and is still not ready fails once; liveCheck's generic
      // retry stays for a probe that could not report at all.
      return ready || !waited?ready:"readiness";
    }catch{return false;}
  }
  // A hub or bridge probe reports its own wait. Its capture is reduced to the
  // fixed, redacted shape again here, so nothing else a probe prints can reach
  // the journal.
  readyWait(target,kind,r){
    const count=v=>Number.isSafeInteger(v) && v>=0;
    if(!count(r?.waitedMs))return false;
    this.probeWaits.set(target+":"+kind,{waitedMs:r.waitedMs,polls:count(r.polls)?r.polls:null,...(r.capture?{capture:sanitizeCapture(r.capture)}:{})});
    return true;
  }
  async rollback(target){
    const a=this.artifacts.get(target);if(!a || a.rollbackSafe!==true)return false;
    if(target==="mini"){
      if(!a.rollbackCaptured || fileDigest(a.rollbackPath)!==a.priorArtifactSHA256)return false;
      // The host command is never trusted alone: it may be missing in an old
      // CLI, lack its rollback copy, restore other bytes or fail doctor. Unless
      // it left exactly the prior binary, restore this job's journal copy.
      let hostRestored=true;
      try{this.command(a.hostRollback||[a.installPath,"host","setup","--rollback"]);}catch{hostRestored=false;}
      if(!hostRestored || fileDigest(a.installPath)!==a.priorArtifactSHA256){
        copyFileSync(a.rollbackPath,a.installPath+".rollback");chmodSync(a.installPath+".rollback",0o755);renameSync(a.installPath+".rollback",a.installPath);this.command(a.relayRestart);
      }
      if(fileDigest(a.installPath)!==a.priorArtifactSHA256)return false;
    }else if(PAIR.includes(target)){
      // A failed rollback program is already "not restored"; the probe still
      // runs once so the journal shows the app state it left behind.
      try{this.command(a.rollbackProgram);}
      catch{try{this.readyWait(target,"rollback",JSON.parse(this.command(a.rollbackProbe)));}catch{}return false;}
    }else{this.command(a.rollbackProgram);}
    const r=JSON.parse(this.command(a.rollbackProbe));
    if(PAIR.includes(target))this.readyWait(target,"rollback",r);
    if(target==="tailos")this.probeWaits.set("tailos:rollback",{lastCommit:sha(r.lastCommit)?r.lastCommit:null,waitedMs:Number.isSafeInteger(r.waitedMs)&&r.waitedMs>=0?r.waitedMs:null});
    return r.restored===true && r.databaseWritesPreserved===true;
  }
  probeWait(target,kind){return this.probeWaits.get(target+":"+kind);}
  // B10: the verified TailOS build stays on the host so the next release can
  // roll back to it; release-inputs names it in that job's rollback program.
  async retain(target,a){
    if(target!=="tailos")return;
    const dir=join(this.config.journalDirectory,"tailos-dist-"+a.commit);if(existsSync(dir))return;
    rmSync(dir+".tmp",{recursive:true,force:true});cpSync(join(this.config.cwd,"dist-static"),dir+".tmp",{recursive:true});chmodSync(dir+".tmp",0o700);renameSync(dir+".tmp",dir);
  }
  async finish(receipt,expectedGeneration){const path=join(this.config.journalDirectory || dirname(this.config.journalPath),this.job.id+"-receipt.json");save(path,receipt);this.native("finish",["--file",path],expectedGeneration);}
  async block(){this.native("block");}
  async refuse(){this.native("refuse");}
  async escalate(details={}){
    // Only a re-validated 40-hex commit and whole seconds reach the text.
    const waits=(Array.isArray(details.probeWaits)?details.probeWaits:[]).filter(w=>w?.target==="tailos" && ["live","rollback"].includes(w.probe)).map(w=>` TailOS ${w.probe==="live"?"live check":"rollback probe"} last saw ${sha(w.lastCommit)?w.lastCommit:"no readable release.json"}${Number.isSafeInteger(w.waitedMs)&&w.waitedMs>=0?` after ${Math.round(w.waitedMs/1000)} s`:""}.`).join("");
    if(details.outcome==="refused"){
      const reason=typeof details.reason==="string"&&REASON.test(details.reason)?details.reason:"unclassified";
      return this.command([this.config.tt||"tt","send","--kind","notice","--subject","Release refused before publication","--text",`Release ${this.job.id} was refused before publication; nothing was published or deployed. Reason: ${reason}.${reason.startsWith("integration-conflict")?" Next: re-apply the candidate on the current tasks-hub through a follow-through item and re-review the new commit.":""} Handler reconciliation required.`,"--request-id",`${this.job.id}-release-failure`,"--ref",`release-job=${this.job.id}`]);
    }
    if(details.push==="failed")return this.command([this.config.tt||"tt","send","--kind","notice","--subject","Release is live but the tasks-hub push failed","--text",`Release ${this.job.id} is live and verified, but the fast-forward push of tasks-hub to origin failed. Live targets were not rolled back; inspect the remote and push tasks-hub by hand.`,"--request-id",`${this.job.id}-push-failure`,"--ref",`release-job=${this.job.id}`]);
    return this.command([this.config.tt||"tt","send","--kind","notice","--subject","Release failed and requires recovery","--text",`Release failed for ${this.job.id}; inspect the private host journal. Automatic rollback attempted once; handler reconciliation required.${details.revert==="failed"?" The tasks-hub revert failed, so the rolled-back change is still on tasks-hub.":""}${details.rollbackBlocked===true&&details.revert==="committed"?" tasks-hub was reverted, but at least one target could not be rolled back and still runs the released code; roll it back by hand before the next release.":""}${waits}`,"--request-id",`${this.job.id}-release-failure`,"--ref",`release-job=${this.job.id}`]);
  }
}

// The runner's own code: the modules one process loads once, as static
// imports, each named by its path from scripts/ (the matrix imports one from
// tests/). A drift test pins the list to their "./" and "../" imports. A code
// digest is the sha256 of the "name:sha256" lines.
export const RUNNER_CODE_FILES=["../tests/test-binaries.mjs","release-inputs.mjs","release-probe.mjs","release-runner.mjs","release-targets.mjs","verify-matrix-host-lock.mjs","verify-matrix.mjs"];
const HEX64=/^[a-f0-9]{64}$/,bytesDigest=bytes=>createHash("sha256").update(bytes).digest("hex");
export const codeDigest=files=>bytesDigest(RUNNER_CODE_FILES.map(n=>`${n}:${files[n]}`).join("\n"));
export function diskCode(directory){
  const files=Object.fromEntries(RUNNER_CODE_FILES.map(n=>[n,bytesDigest(readFileSync(join(directory,n)))]));
  return {files,digest:codeDigest(files)};
}
// Read once as this process starts: the bytes beside this module, which are
// the ones it imported. null when they cannot be read; the gate then refuses.
export const LOADED_CODE=(()=>{try{return diskCode(dirname(fileURLToPath(import.meta.url)));}catch{return null;}})();
// The same files as published: the blobs of refs/heads/tasks-hub, never
// the working tree, which holds an unpublished candidate after a refusal.
export function publishedCode(cwd){
  const commit=git(cwd,"rev-parse","--verify","refs/heads/tasks-hub^{commit}");
  if(!sha(commit))throw releaseError("published-unreadable");
  const files=Object.fromEntries(RUNNER_CODE_FILES.map(n=>[n,bytesDigest(execFileSync("git",["cat-file","blob",`${commit}:${join("scripts",n)}`],{cwd,stdio:["ignore","pipe","ignore"],maxBuffer:64*1024*1024}))]));
  return {commit,files,digest:codeDigest(files)};
}
// The digest as computed before tests/test-binaries.mjs was watched: the
// scripts alone. A runner of that code aims its re-exec marker at this value,
// so codeRecord accepts it, only to say that the restart arrived.
const scriptsDigest=files=>bytesDigest(RUNNER_CODE_FILES.filter(n=>!n.startsWith("../")).map(n=>`${n}:${files[n]}`).join("\n"));
const validCode=c=>HEX64.test(c?.digest||"") && RUNNER_CODE_FILES.every(n=>HEX64.test(c.files?.[n]||""));
// What a poll may do about its own code. current: claim as usual. draining:
// stale while a release, matrix run or host release lock is active; claim
// nothing new. restart: stale and idle; re-exec onto the published scripts.
// refused: claim nothing until a person re-provisions the deployer. marker is
// the published digest a re-exec was last aimed at: stale again at that same
// digest means the re-exec did not change what this process loads.
export const CODE_REASONS=["loaded-unreadable","published-unreadable","restart-did-not-refresh","checkout-dirty","checkout-failed","disk-mismatch","exec-failed"];
export function codeDecision({loaded,published,idle,marker}){
  if(!validCode(loaded))return {state:"refused",reason:"loaded-unreadable"};
  if(!validCode(published) || !sha(published.commit))return {state:"refused",reason:"published-unreadable"};
  if(loaded.digest===published.digest)return {state:"current"};
  if(idle!==true)return {state:"draining"};
  if(marker===published.digest)return {state:"refused",reason:"restart-did-not-refresh"};
  return {state:"restart"};
}
// Brings the idle checkout to the published commit before a re-exec, under
// integrateCandidate's cleanliness rule (ignored build outputs do not count),
// and proves the watched files on disk are the published bytes.
export function prepareCode(cwd,published){
  let dirty;try{dirty=git(cwd,"status","--porcelain");}catch{throw releaseError("checkout-failed");}
  if(dirty)throw releaseError("checkout-dirty");
  try{if(git(cwd,"rev-parse","HEAD")!==published.commit)git(cwd,"checkout","--detach",published.commit);}catch{throw releaseError("checkout-failed");}
  let disk;try{disk=diskCode(join(cwd,"scripts"));}catch{throw releaseError("disk-mismatch");}
  if(disk.digest!==published.digest)throw releaseError("disk-mismatch");
}
// The gate serveDeployment consults before every claim; only the daemon entry
// builds it, and tests replace its parts. exec replaces this process image
// (same pid, parent, agent and run) and returns only in tests, or when a stop
// signal arrived first: the new image would not know it was told to stop.
// noticed and said hold what this process already posted and printed.
const stopSignalTurn=()=>new Promise(done=>setImmediate(()=>setImmediate(done)));
export function runnerCodeGate(over={}){
  const from=process.env.TAILTERM_RUNNER_REEXEC_FROM;
  return {loaded:LOADED_CODE,marker:process.env.TAILTERM_RUNNER_REEXEC||null,restartedFrom:HEX64.test(from||"")?from:null,
    published:publishedCode,prepare:prepareCode,now:()=>Date.now(),execve:(file,argv,env)=>process.execve(file,argv,env),
    async exec(published,signal){
      // Whatever this process printed reaches a piped terminal before it is replaced.
      await new Promise(done=>{setTimeout(done,1000).unref();process.stderr.write("",()=>done());});
      // A signal that arrived during this poll's synchronous work is handled
      // only once the event loop turns, so it is given that turn.
      await stopSignalTurn();if(signal?.aborted)return;
      this.execve(process.execPath,[process.execPath,...process.execArgv,...process.argv.slice(1)],{...process.env,TAILTERM_RUNNER_REEXEC:published.digest,TAILTERM_RUNNER_REEXEC_FROM:this.loaded.digest});
    },
    noticed:new Set(),said:new Set(),...over};
}
// JOURNAL/runner-code.json (0600): the code this process loaded and the
// published code it was last compared with. Rewritten only when something
// other than checkedAt changes, so checkedAt is when this state was first seen.
export function codeRecord(config,code,decision,published){
  const iso=ms=>new Date(ms).toISOString(),loaded=validCode(code.loaded)?{digest:code.loaded.digest,files:code.loaded.files}:null;
  const current=validCode(published)&&sha(published.commit)?{commit:published.commit,digest:published.digest,files:published.files}:null;
  code.startedAt??=iso(code.now());
  const record={version:1,agentId:process.env.TAILTERM_AGENT||null,runId:process.env.TAILTERM_RUN||null,pid:process.pid,startedAt:code.startedAt,checkedAt:iso(code.now()),state:decision.state,...(decision.reason?{reason:decision.reason}:{}),
    loaded,current,changed:loaded&&current?RUNNER_CODE_FILES.filter(n=>loaded.files[n]!==current.files[n]):[],...(loaded&&code.restartedFrom&&[loaded.digest,scriptsDigest(loaded.files)].includes(code.marker)?{restartedFrom:code.restartedFrom}:{})};
  const path=join(config.journalDirectory,"runner-code.json"),unchecked=r=>JSON.stringify({...r,checkedAt:null});
  let prior=null;try{prior=JSON.parse(readFileSync(path,"utf8"));}catch{}
  if(prior && unchecked(prior)===unchecked(record))return prior;
  save(path,record);return record;
}
// Board notices about the runner's code. Only 12-hex prefixes, names from
// RUNNER_CODE_FILES and fixed reason tokens reach the text. The request id
// has no time in it, so a restarted process resends the same notice.
const RESTART_ACTION="Action: run tt deployment setup for the deployer with its current run as predecessor (docs/project-deployment.md).";
const short=v=>HEX64.test(v||"")||sha(v)?v.slice(0,12):"none";
export function codeNotice(record,kind=record?.state){
  if(!["draining","restart","restarted","refused"].includes(kind))return null;
  const run=NAME(record.runId),loaded=short(record.loaded?.digest),current=short(record.current?.digest),reason=CODE_REASONS.includes(record.reason)?record.reason:"unclassified";
  const changed=(Array.isArray(record.changed)?record.changed:[]).filter(n=>RUNNER_CODE_FILES.includes(n)).join(", ")||"none";
  const seen=`Deployer run ${run} loaded scripts ${loaded}; ${record.current?`published tasks-hub ${short(record.current.commit)} has ${current}`:"the published tasks-hub scripts could not be read"}. Changed: ${changed}.`;
  const requestId=`runner-code-${run}-${loaded}-${current}-${kind}${kind==="refused"?"-"+reason:""}`;
  if(kind==="draining")return {requestId,subject:"Deployer code is out of date; it restarts itself after the current release",text:`${seen} It claims no new release, and restarts itself onto the published scripts once no release, matrix run or host release lock is active.`};
  if(kind==="restart")return {requestId,subject:"Deployer is restarting itself onto the published scripts",text:`${seen} No release is active, so it restarts itself now with the same agent and run. A notice that it runs the published scripts follows; if none arrives the deployer did not come back. ${RESTART_ACTION}`};
  if(kind==="restarted")return {requestId,subject:"Deployer now runs the published scripts",text:`Deployer run ${run} restarted itself from scripts ${short(record.restartedFrom)} and now runs ${loaded}, the scripts of published tasks-hub ${short(record.current?.commit)}.`};
  return {requestId,subject:["loaded-unreadable","published-unreadable"].includes(reason)?"Deployer is not claiming releases: it cannot compare its scripts with the published ones":"Deployer is not claiming releases: its scripts are out of date",
    text:`${seen} It could not restart itself and claims no release. Reason: ${reason}. ${RESTART_ACTION}`};
}
const CODE_LINES={draining:"Deployer code is out of date; no new release is claimed and a restart follows the active work.\n",restart:"Deployer restarting itself onto the published scripts.\n",refused:"Deployer code is out of date and it cannot restart itself; no release is claimed until it is re-provisioned.\n"};
export function runnableJob(jobs,agent,run,skipped=new Set()){
  const owned=jobs.find(j=>["claimed","merged"].includes(j.state) && j.agentId===agent && j.runId===run);
  if(owned)return owned;
  if(jobs.some(j=>["claimed","merged","blocked"].includes(j.state)))return null;
  return jobs.find(j=>j.state==="verified" && !skipped.has(j.id))||null;
}
// A job the handler set aside (tt deployment set-aside) left the fence with
// its claim's journal still on the host. Before the job is claimed again, a
// journal of exactly that claim with no effects is archived so the re-planned
// run starts fresh on the current tasks-hub; any other journal is held for
// handler reconciliation. "none" leaves the journal to runRelease.
export function setAsideJournal(journalPath,job){
  const r=job?.reconciliations?.at(-1);
  if(job?.state!=="verified" || r?.disposition!=="set_aside" || !existsSync(journalPath))return "none";
  let prior;try{prior=JSON.parse(readFileSync(journalPath,"utf8"));}catch{return "held";}
  if(prior?.jobId!==job.id || prior.commit!==job.commit || prior.agentId!==r.agentId || prior.runId!==r.runId || !Array.isArray(prior.effects) || prior.effects.length || prior.published===true)return "held";
  renameSync(journalPath,`${journalPath}.set-aside-g${job.generation}`);return "archived";
}
const FENCE_REASONS={waiting_matrix:"is waiting for the handler to import integrated verification",waiting_inputs:"is waiting for the handler to bind release inputs",blocked:"is blocked",merged:"is merged and not yet finished",claimed:"is claimed by another deployer run"};
// The notice for a job that holds the project fence while a later job waits:
// null when nothing waits. Only a claim with no effects (unpublished, no
// receipt, no inputs binding) can be set aside; any other holder keeps the
// fence. A claim whose run still names the host release lock (locked) keeps it
// too: set-aside clears the job's run, after which no reconcile record can
// match the lock, and the next job would find the host locked. So does a claim
// whose integrated matrix run is not known to have ended (matrixRun; starting,
// waiting, running or held): that run may be using the checkout, and the host
// release lock is not held while it runs. Unless the caller says otherwise, a
// job waiting for its matrix is taken to have such a run. The request id
// omits generations, which every fence check bumps.
export function fenceWaitNotice(jobs,holder,reason,locked=false,matrixRun=reason==="waiting_matrix"){
  const waiting=jobs.find(j=>j.id!==holder?.id && j.state==="verified");
  if(!holder || !waiting || !FENCE_REASONS[reason])return null;
  const noEffects=holder.state==="claimed" && holder.published!==true && !holder.receipt && !holder.inputsDigest;
  const advice=!noEffects?"It keeps the fence until handler reconciliation.":matrixRun?"It keeps the fence: its integrated matrix run has not ended and may be using the checkout, so it is not set aside until the run.json of each of its attempts says ended.":locked?"It keeps the fence: the host release lock names its run, so the handler reconciles it with the lock digest after that run stops.":"It has no release effects; the handler can move it aside with tt deployment set-aside so the waiting job claims the fence.";
  return {requestId:`${holder.id}-fence-wait-${waiting.id}-${reason}${noEffects&&matrixRun?"-matrix":noEffects&&locked?"-locked":""}`,waitingJobId:waiting.id,subject:"A release job is waiting behind a held project fence",
    text:`Release ${holder.id} holds the project release fence and ${FENCE_REASONS[reason]}; release ${waiting.id} is queued behind it. ${advice}`};
}
// Whether the host release lock names this job's exact claim. An unreadable
// lock counts as naming it: set-aside is never suggested on doubt.
export function hostLockNames(cwd,job){
  const lock=hostLockPath(cwd,job);
  if(!existsSync(lock))return false;
  try{const record=JSON.parse(readFileSync(lock,"utf8"));return record?.jobId===job.id && record.agentId===job.agentId && record.runId===job.runId;}
  catch{return true;}
}
export function reconcileHostLocks(config,jobs){
  for(const job of jobs){
    const r=job.reconciliations?.at(-1);
    if(!r?.lockDigest || !r.noActiveExecution || !r.refResolved || !["no_effects","restored"].includes(r.journalState) || !["verified","refused","superseded"].includes(job.state))continue;
    const lock=hostLockPath(config.cwd,job);
    if(!existsSync(lock) || fileDigest(lock)!==r.lockDigest)continue;
    const record=JSON.parse(readFileSync(lock,"utf8"));
    if(record.jobId===job.id && record.agentId===r.agentId && record.runId===r.runId)rmSync(lock);
  }
}
function fullReleaseReceipt(receipt, job) {
  const targets=new Set();
  return receipt?.version===1 && receipt.jobId===job.id && sha(receipt.commit) && /^[a-f0-9]{64}$/.test(receipt.verificationDigest||"") && Array.isArray(receipt.targets) && ["released","rolled_back","blocked"].includes(receipt.outcome) && receipt.targets.every(t=>{
    if (!t || !["hub","bridge","mini","tailos"].includes(t.target) || targets.has(t.target) || !/^[a-f0-9]{64}$/.test(t.artifactSHA256||"") || typeof t.release!=="string" || !t.release || !["released","failed","rolled_back"].includes(t.outcome)) return false;
    targets.add(t.target);return true;
  });
}
export function reconcileReceipts(config,jobs){
  const updates=[];
  for(const job of jobs){
    if(!job.receipt)continue;
    const path=join(config.journalDirectory,job.id+".json");if(!existsSync(path))continue;
    const journal=JSON.parse(readFileSync(path,"utf8"));
    if(["finishing","receipt_pending"].includes(journal.phase) && journal.jobId===job.id){
      if(!fullReleaseReceipt(job.receipt,job) || journal.commit!==job.commit || journal.agentId!==job.agentId || journal.runId!==job.runId || (journal.taskId!==undefined && journal.taskId!==job.taskId) || (journal.pauseGeneration!==undefined && journal.pauseGeneration!==job.pauseGeneration) || job.receipt.commit!==(job.integratedCommit||job.commit) || job.receipt.verificationDigest!==job.verificationDigest || job.generation!==journal.finishGeneration+1 || digest(journal.receipt)!==digest(job.receipt))throw releaseError("Saved release receipt does not match pending journal");
      journal.phase="complete";updates.push([path,journal]);
    }
  }
  // A batch's receipt and member journals first: a failed write holds the
  // poll with the journal still pending, so the next poll writes again.
  for(const [path,journal] of updates){if(journal.batchReceipt)materializeBatch(config.journalDirectory,journal);save(path,journal);}
}
// Journal retention. The imported backup copy is read only by its job's
// rehearsal (the authoritative backup stays on TrueNAS), so the journal keeps
// it for every job that is not terminal and for the newest released jobs.
const COPY_FILES=["","-wal","-shm","-journal"],BACKUP_NAMES=["truenas","hub","bridge"],TERMINAL=["released","rolled_back","refused","superseded"];
export function retentionPolicy(config){
  const retention=config?.retention??{};
  if(typeof retention!=="object" || Array.isArray(retention))throw releaseError("Invalid journal retention");
  const value=(key,fallback)=>{const v=retention[key]===undefined?fallback:retention[key];if(!Number.isSafeInteger(v) || v<0)throw releaseError("Invalid journal retention "+key);return v;};
  return {releasedBackups:value("releasedBackups",3),backupBudgetBytes:value("backupBudgetBytes",4294967296)};
}
// Removes only the exact backup and rehearsal copy names of jobs in the
// deployment list, each a regular file directly in the journal directory. A
// job in any state not known to be terminal keeps everything; a terminal job
// loses its leftover rehearsal copy, and its backup copy unless it is one of
// the newest released jobs within the size budget. Every removal is appended
// to retention.jsonl before the file goes. Returns the records written.
export function pruneJournal(config,jobs,{now=()=>Date.now()}={}){
  const policy=retentionPolicy(config),dir=config.journalDirectory,records=[];
  const size=file=>{try{const s=lstatSync(join(dir,file));return s.isFile()?s.size:null;}catch{return null;}};
  const remove=(jobId,file,kind,reason)=>{
    const bytes=size(file);if(bytes===null)return 0;
    const record={version:1,at:new Date(now()).toISOString(),jobId,file,kind,bytes,reason};
    const fd=openSync(join(dir,"retention.jsonl"),"a",0o600);try{writeFileSync(fd,JSON.stringify(record)+"\n");fsyncSync(fd);}finally{closeSync(fd);}
    rmSync(join(dir,file));records.push(record);return bytes;
  };
  let total=0;const released=[];
  jobs.forEach((job,index)=>{
    if(typeof job?.id!=="string" || !/^[A-Za-z0-9_-]+$/.test(job.id))return;
    const backups=BACKUP_NAMES.map(name=>`${job.id}-${name}-backup.sqlite`);
    const held=()=>backups.reduce((sum,file)=>sum+(size(file)??0),0);
    if(!TERMINAL.includes(job.state)){total+=held();return;}
    for(const file of backups)for(const suffix of COPY_FILES)remove(job.id,`${file}.rehearsal-${job.id}${suffix}`,"rehearsal","terminal");
    if(job.state!=="released"){for(const file of backups)remove(job.id,file,"backup","not-released");return;}
    // settledAt is RFC3339Nano with trimmed zeros, so it is compared as time;
    // a job without a usable one is the oldest, in list order.
    const at=Date.parse(job.settledAt??"");
    released.push({id:job.id,backups,index,at:Number.isNaN(at)?-Infinity:at,bytes:held()});
  });
  released.sort((a,b)=>a.at===b.at?b.index-a.index:b.at-a.at);
  const drop=(entry,reason)=>{for(const file of entry.backups)remove(entry.id,file,"backup",reason);};
  const kept=released.slice(0,policy.releasedBackups);
  for(const entry of released.slice(policy.releasedBackups))drop(entry,"count");
  total+=kept.reduce((sum,entry)=>sum+entry.bytes,0);
  // Oldest kept released copy first; a non-terminal job's copy stays even over budget.
  while(policy.backupBudgetBytes>0 && total>policy.backupBudgetBytes && kept.length){const entry=kept.pop();total-=entry.bytes;drop(entry,"budget");}
  return records;
}
// The four configured baselines, read from the private config at every poll
// without restarting the daemon. A recorded hand release advances baselines
// by itself through its superseded job (releaseBaselines); a config edit is
// the fallback for a hand release no job carries. Only baselines are re-read:
// cwd, journal and host references stay those the daemon started with.
export function readBaselines(configPath){
  const b=JSON.parse(readFileSync(configPath,"utf8")).baselines,targets=["hub","bridge","mini","tailos"];
  if(!targets.every(t=>sha(b?.[t])))throw releaseError("Four last-successful baselines required");
  return Object.fromEntries(targets.map(t=>[t,b[t]]));
}
export async function serveDeployment(config,{once=false,signal,configPath,release=runRelease,code}={}) {
  if(config.version!==1 || config.enabled!==true || !config.cwd || !config.journalDirectory)throw releaseError("Explicit private activation config required");
  tailosWindow(config);readyWindow(config,"hub");readyWindow(config,"bridge");
  retentionPolicy(config);
  // Read once, at daemon start. At 1 (or absent) nothing below reads a
  // candidate, the solo file or passes a batch key: today's release.
  const batchPolicy=releaseBatchPolicy(config);
  // The batch key for a claimed lead: the candidates' full records while its
  // batch may still be declared or verified, else none.
  const batchFor=(read,jobs,current)=>{
    const batch={max:batchPolicy.max,importWaitMs:batchPolicy.importWaitMs,candidates:[],onFallback:ids=>addBatchSolo(config.journalDirectory,ids)};
    let journal=null;try{journal=JSON.parse(readFileSync(join(config.journalDirectory,current.id+".json"),"utf8"));}catch{}
    if(current.state!=="claimed" || current.integratedVerification || (journal && journal.phase!=="batching" && !journal.batch))return batch;
    let solo;try{solo=readBatchSolo(config.journalDirectory);}catch{process.stderr.write("Batch solo file unreadable; releasing one job at a time.\n");return batch;}
    for(const summary of batchCandidates(jobs,current,solo).slice(0,2*(batchPolicy.max-1))){
      try{const detail=readReleaseDetail(read,summary,{bookend:false});if(!batchMismatch(current,detail))batch.candidates.push(detail);}catch{}
    }
    return batch;
  };
  // One fence-wait notice per holder, waiting job and reason per process; the
  // hub returns the original for a restart's identical resend.
  const posted=new Set();
  const notify=(reader,jobs,holder,reason)=>{
    let locked=true;try{locked=hostLockNames(config.cwd,holder);}catch{}
    let matrixRun=true;try{matrixRun=matrixRunUnsettled(config.journalDirectory,holder);}catch{}
    const notice=fenceWaitNotice(jobs,holder,reason,locked,matrixRun);
    if(!notice || posted.has(notice.requestId))return;
    try{reader.command([config.tt||"tt","send","--kind","notice","--subject",notice.subject,"--text",notice.text,"--request-id",notice.requestId,"--ref",`release-job=${holder.id}`,"--ref",`waiting-job=${notice.waitingJobId}`]);posted.add(notice.requestId);}
    catch{process.stderr.write("Fence wait notice not posted; the next poll retries.\n");}
  };
  // One notice per place on the verification host's waitlist (position, list
  // length and holder), and one per held matrix run.
  const matrixNotify=(reader,job,adapter)=>{
    for(const notice of [matrixWaitNotice(job,adapter.matrixWait),matrixHeldNotice(job,adapter.matrixHeld)]){
      if(!notice || posted.has(notice.requestId))continue;
      try{reader.command([config.tt||"tt","send","--kind","notice","--subject",notice.subject,"--text",notice.text,"--request-id",notice.requestId,"--ref",`release-job=${job.id}`]);posted.add(notice.requestId);}
      catch{process.stderr.write("Matrix host notice not posted; the next poll retries.\n");}
    }
  };
  // One notice and one terminal line per code state, loaded and published
  // digest for this process; a failed post is retried by the next poll.
  const codeNotify=(reader,record,kind=record.state)=>{
    const notice=codeNotice(record,kind);if(!notice)return;
    if(CODE_LINES[kind] && !code.said.has(notice.requestId)){code.said.add(notice.requestId);process.stderr.write(CODE_LINES[kind]);}
    if(code.noticed.has(notice.requestId))return;
    try{reader.command([config.tt||"tt","send","--kind","notice","--subject",notice.subject,"--text",notice.text,"--request-id",notice.requestId]);code.noticed.add(notice.requestId);}
    catch{process.stderr.write("Runner code notice not posted; the next poll retries.\n");}
  };
  // One notice per job and safe reason for the CLI failures a held poll or a
  // held release recorded. It goes through the CLI that just failed, so it is
  // best effort: the private record and the terminal line remain.
  const cliNotify=(reader,failures)=>{
    for(const failure of [...failures]){
      const notice=cliFailureNotice(failure,process.env.TAILTERM_RUN);
      if(posted.has(notice.key))continue;
      try{reader.command([config.tt||"tt","send","--kind","notice","--subject",notice.subject,"--text",notice.text,"--request-id",notice.requestId,...(notice.jobId?["--ref",`release-job=${notice.jobId}`]:[])]);posted.add(notice.key);}catch{}
    }
  };
  // A skipped job (a refused claim or a held set-aside journal) is told once
  // per process to each of the project database handler and the live owner
  // helper, directed so both are woken. While a recipient cannot be resolved
  // or its post fails, one Board notice without a recipient stands in and that
  // recipient is tried again at the next poll. Nothing here changes the skip.
  const HELD_TO={handler:reader=>reader.handler(),helper:reader=>{
    const live=JSON.parse(reader.command([config.tt||"tt","agents","--json"])).filter(a=>a?.role==="owner_helper" && !["closed","exited"].includes(a.status));
    if(live.length!==1 || !/^agt_[a-f0-9]+$/.test(live[0].id||""))throw releaseError("One live owner helper required");return live[0].id;}};
  const heldNotify=(reader,job,kind,reason)=>{
    const notice=heldClaimNotice(job,kind,reason,process.env.TAILTERM_RUN);if(!notice)return;
    const send=(suffix,to)=>reader.command([config.tt||"tt","send","--kind","notice",...(to?["--to",to]:[]),"--subject",notice.subject,"--text",notice.text,"--request-id",`${notice.requestId}-${suffix}`,"--ref",`release-job=${notice.jobId}`,...(notice.item?["--ref",`item=${notice.item}`]:[]),...(notice.commit?["--ref",`commit=${notice.commit}`]:[])]);
    let missed=false;
    for(const [name,resolve] of Object.entries(HELD_TO)){
      if(posted.has(`${notice.key} ${name}`))continue;
      try{send(name,resolve(reader));posted.add(`${notice.key} ${name}`);}catch{missed=true;}
    }
    if(!missed)return;
    process.stderr.write("Held claim notice not delivered to every recipient; the next poll retries.\n");
    if(posted.has(`${notice.key} board`))return;
    try{send("board");posted.add(`${notice.key} board`);}catch{}
  };
  while(!signal?.aborted){
    let reader=null;
    try {
    // An unreadable or invalid edit holds the whole poll, before any claim.
    const configured=configPath?readBaselines(configPath):config.baselines;
    reader=new HostAdapter(config,{});
    const read=argv=>reader.command([config.tt||"tt",...argv]);
    const jobs=readReleaseSummaries(read);
    // Validate complete baseline history before any recovery write, pruning or claim.
    const baselines=deploymentBaselines(configured,jobs);
    // Fetch every locally relevant recovery record and the selected/held job
    // before any write, deletion or claim. A failed detail read holds the poll.
    const selected=runnableJob(jobs,process.env.TAILTERM_AGENT,process.env.TAILTERM_RUN);
    const details=new Map();
    const lock=hostLockPath(config.cwd,jobs[0]||{});
    const lockedJob=existsSync(lock)?JSON.parse(readFileSync(lock,"utf8")).jobId:null;
    for(const summary of jobs){
      const path=join(config.journalDirectory,summary.id+".json");
      const pending=existsSync(path) && ["finishing","receipt_pending"].includes(JSON.parse(readFileSync(path,"utf8")).phase);
      if(summary.id===selected?.id || ["claimed","merged","blocked"].includes(summary.state) || pending || summary.id===lockedJob){
        details.set(summary.id,readReleaseDetail(read,summary));
      }
    }
    bookendReleaseLedger(read,jobs.releaseSnapshot,jobs.releaseHead);
    const recovery=[...details.values()];
    reconcileReceipts(config,recovery);reconcileHostLocks(config,recovery);
    try{pruneJournal(config,jobs);}catch{process.stderr.write("Journal retention sweep failed; remaining backup copies kept.\n");}
    // The runner's own code, once per poll and before any claim. Idle is the
    // only place it restarts: no claimed, merged or blocked job, no matrix run
    // of this process and no host release lock. Whatever is decided, a stale
    // runner claims nothing below; a job it already owns is still run.
    let codeNow=null;
    if(code){
      let published=null;try{published=code.published(config.cwd);}catch{}
      const idle=!jobs.some(j=>["claimed","merged","blocked"].includes(j.state)) && MATRIX_CHILDREN.size===0 && !existsSync(lock);
      let decision=codeDecision({loaded:code.loaded,published,idle,marker:code.marker});
      // A re-exec that failed in this process is not tried again for the same
      // published code, so its refusal is not rewritten on every poll.
      if(decision.state==="restart" && code.execFailed===published.digest)decision={state:"refused",reason:"exec-failed"};
      // A runner told to stop does not restart: nothing below moves the
      // checkout, replaces the process or claims once the signal is set.
      if(decision.state==="restart"){await stopSignalTurn();if(signal?.aborted)return;}
      if(decision.state==="restart"){
        try{code.prepare(config.cwd,published);}
        catch(error){const reason=failureReason(error);decision={state:"refused",reason:CODE_REASONS.includes(reason)?reason:"checkout-failed"};}
      }
      if(decision.state==="restart"){
        // Recorded and announced first: a successful exec never returns.
        codeNotify(reader,codeRecord(config,code,decision,published));
        try{await code.exec(published,signal);}catch{code.execFailed=published.digest;decision={state:"refused",reason:"exec-failed"};}
        if(signal?.aborted)return;
      }
      codeNow=codeRecord(config,code,decision,published);
      // An unreadable published ref is announced only when it withholds a
      // claim (below), so a poll with nothing to claim stays silent.
      if(codeNow.state==="current"){if(codeNow.restartedFrom)codeNotify(reader,codeNow,"restarted");}
      else if(codeNow.reason!=="published-unreadable")codeNotify(reader,codeNow);
    }
    const skipped=new Set();
    for (;;) {
      const summary=runnableJob(jobs,process.env.TAILTERM_AGENT,process.env.TAILTERM_RUN,skipped);
      const job=summary?(details.get(summary.id)||readReleaseDetail(read,summary)):null;
      if(!job){
        const holder=jobs.find(j=>["claimed","merged","blocked"].includes(j.state));
        if(holder)notify(reader,jobs,details.get(holder.id),holder.state);
        break;
      }
      // Stale code never archives a journal or claims.
      if(codeNow && codeNow.state!=="current" && job.state==="verified"){codeNotify(reader,codeNow);break;}
      // Archived only while unclaimed, so a later crash of the new claim
      // leaves its own journal alone.
      if(job.state==="verified"){
        let journal;try{journal=setAsideJournal(join(config.journalDirectory,job.id+".json"),job);}catch{journal="held";}
        if(journal==="held"){skipped.add(job.id);process.stderr.write("Set-aside release journal held; handler reconciliation required.\n");heldNotify(reader,job,"journal","the journal is unreadable, shows effects or is not of exactly the set-aside claim");continue;}
      }
      const adapter=new HostAdapter(config,job);
      try{if(job.state==="verified")adapter.native("claim");}
      catch(error){skipped.add(job.id);process.stderr.write("Release claim held; handler reconciliation required.\n");const call=failureReason(error),said=error?.refusal?`${error.refusal} (${call})`:call;heldNotify(reader,job,"claim",said.length<=160?said:call);continue;}
      const current=adapter.job;adapter.baselines=baselines;
      const {testPolicy,sleep,now,...activation}=config;
      const batch=batchPolicy.max>1?batchFor(read,jobs,current):null;
      let result;
      try{result=await release({...activation,baselines,job:current,journalPath:join(config.journalDirectory,current.id+".json"),...(batch?{batch}:{}),...(codeNow?{code:{loaded:codeNow.loaded?.digest??null,current:codeNow.current?.digest??null}}:{})},adapter);}
      catch{process.stderr.write("Release held; inspect handler fence and private journal.\n");cliNotify(reader,[...reader.cliFailures,...adapter.cliFailures]);}
      // A batch's own members are not waiting behind its lead.
      let waiting=jobs;
      if(batch){try{const members=new Set(JSON.parse(readFileSync(join(config.journalDirectory,current.id+".json"),"utf8")).batch?.jobs?.map(j=>j.jobId));waiting=jobs.filter(j=>!members.has(j.id));}catch{}}
      if(["waiting_matrix","waiting_inputs"].includes(result?.outcome))notify(reader,waiting,adapter.job||current,result.outcome);
      matrixNotify(reader,adapter.job||current,adapter);
      break;
    }
    } catch(error) {process.stderr.write(`Deployment poll held (${failureReason(error)}); inspect native input or recovery evidence.\n`);if(reader)cliNotify(reader,reader.cliFailures);}
    if(once)return;
    await new Promise(r=>setTimeout(r,30000));
  }
}
if(process.argv[1] && resolve(process.argv[1])===fileURLToPath(import.meta.url) && process.argv[2]==="--compat-cli"){
  try {
    if(process.argv[3]!=="--tt" || !process.argv[4] || process.argv[5]!=="--")throw releaseError("Compatibility dispatcher needs pinned binary and argv separator");
    process.exitCode=dispatchCompatibility(process.argv[4],process.argv.slice(6));
  } catch {process.stderr.write("Deployment compatibility dispatch refused.\n");process.exitCode=1;}
}else if(process.argv[1] && resolve(process.argv[1])===fileURLToPath(import.meta.url) && process.argv.includes("--provision-prerequisites")){
  // tt deployment setup runs this in the deployer's checkout. Only the tagged
  // reason is printed on failure.
  const index=process.argv.indexOf("--from");
  try{process.stdout.write(JSON.stringify(provisionPrerequisites(process.cwd(),{from:index<0?undefined:process.argv[index+1]}))+"\n");}
  catch(error){process.stderr.write(failureReason(error)+"\n");process.exitCode=1;}
}else if(process.argv[1] && resolve(process.argv[1])===fileURLToPath(import.meta.url)){
  const index=process.argv.indexOf("--config");
  try{
    if(index<0)throw releaseError("Private config required");
    const config=JSON.parse(readFileSync(process.argv[index+1],"utf8"));
    const controller=new AbortController();process.on("SIGTERM",()=>controller.abort());process.on("SIGINT",()=>controller.abort());
    await serveDeployment(config,{once:process.argv.includes("--once"),signal:controller.signal,configPath:process.argv[index+1],code:runnerCodeGate()});
  }catch{process.stderr.write("Deployment blocked; inspect private host journal and handler release gate.\n");process.exitCode=1;}
}
