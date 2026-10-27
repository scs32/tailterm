import { readReleaseSummaries, readReleaseDetail, releaseProjection, bookendReleaseLedger, deploymentBaselines, fileSHA256, RELEASE_READ_ATTEMPTS, pinnedWrangler, wranglerBin } from "./release-inputs.mjs";
import { createHash } from "node:crypto";
import { tmpdir } from "node:os";
import { fileURLToPath, pathToFileURL } from "node:url";
import { execFileSync, spawn, spawnSync } from "node:child_process";
import { openSync, closeSync, fsyncSync, writeFileSync, readFileSync, renameSync, mkdirSync, mkdtempSync, existsSync, rmSync, rmdirSync, copyFileSync, cpSync, chmodSync, statSync, lstatSync, realpathSync, readdirSync, constants } from "node:fs";
import { join, resolve, dirname, basename, isAbsolute } from "node:path";
import { digest, canonical, diffPaths, receiptEligible, MAX_CHECK_TIMEOUT_MS, MATRIX_PREREQUISITES, missingPrerequisites, provisionPrerequisites } from "./verify-matrix.mjs";
import { PRIORITIES, RUN_TIMEOUT_GRACE_MS, holderCapMs, lockPath, readHostState, holdersOf, canonicalResource, pidGone, groupGone } from "./verify-matrix-host-lock.mjs";
import { selectReleaseTargets, releaseBaselines, schemaChanged } from "./release-targets.mjs";
import { buildInfo, waitForTailOSCommit, tailosWindow, tailosURL, hostDeps, readyWindow, sanitizeCapture } from "./release-probe.mjs";

const fileDigest = fileSHA256;
const TRUENAS_BASE = "/mnt/deepfreeze/tailterm-hub", PAIR = ["hub", "bridge"];
const DESTINATION = { hub: ["binaryDestination", "tailterm-hub"], bridge: ["bridgeBinaryDestination", "tailterm-discord"] };
const same = (a, b) => JSON.stringify(a) === JSON.stringify(b);
// b4: a failed target step journals {step, target, reason}. The reason is
// only ever a fixed message this runner or its adapter tagged, or the facts
// HostAdapter.command derives from a child (program name, exit status and the
// classification/stage fields of its last JSON line); anything else is
// "unclassified". Child stderr, messages, paths and tokens never reach it,
// except that the "Unknown release path" tag of release-targets.mjs names one
// repository path it validated.
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
// The failure diagnostics a rolled_back or blocked hub receipt carries
// (wi_d4e96831ab3a0038): receipt.failure {step, target, reason}, the failed
// target's reason, and each target's liveCheck/rollbackCheck {lastCommit,
// waitedMs}. The reason is the stable failure tag; the hub refuses one that
// names a credential word, so such a reason is sent as "unclassified". A
// probe's polls and capture stay in the private journal.
const SECRET_WORDS = ["token", "secret", "password", "passwd", "bearer", "authorization", "api key", "apikey", "private key"];
const STEP = /^[a-z][a-z-]{0,23}$/, TARGETS = ["hub", "bridge", "mini", "tailos"], PROBE_WAIT_MAX_MS = 3600000;
export function receiptReason(reason) {
  return typeof reason === "string" && REASON.test(reason) && !SECRET_WORDS.some(w => reason.toLowerCase().includes(w)) ? reason : "unclassified";
}
export function probeDiagnostics(wait) {
  const out = {};
  if (typeof wait?.lastCommit === "string" && /^[a-f0-9]{40}$/.test(wait.lastCommit)) out.lastCommit = wait.lastCommit;
  if (Number.isSafeInteger(wait?.waitedMs) && wait.waitedMs >= 0 && wait.waitedMs <= PROBE_WAIT_MAX_MS) out.waitedMs = wait.waitedMs;
  return Object.keys(out).length ? out : undefined;
}
// Adds the diagnostics to a failed receipt from the journal's first failure
// and effects. It reads only journal state, so a resumed run builds the same.
function addDiagnostics(receipt, failure, effects) {
  const named = failure && STEP.test(failure.step || "") ? {step: failure.step, ...(TARGETS.includes(failure.target) ? {target: failure.target} : {}), reason: receiptReason(failure.reason)} : undefined;
  if (named) receipt.failure = named;
  for (const target of receipt.targets) {
    const effect = effects.find(e => e.target === target.target), live = probeDiagnostics(effect?.liveCheck), rollback = probeDiagnostics(effect?.rollbackCheck);
    if (named?.target === target.target) target.reason = named.reason;
    if (live) target.liveCheck = live;
    if (rollback) target.rollbackCheck = rollback;
  }
}
export function withoutDiagnostics(receipt) {
  if (!receipt || typeof receipt !== "object") return receipt;
  const {failure, ...rest} = receipt, push = receipt.push;
  return {...rest, ...(Array.isArray(receipt.targets) ? {targets: receipt.targets.map(t => { if (!t || typeof t !== "object") return t; const {reason, liveCheck, rollbackCheck, ...kept} = t; return kept; })} : {}),
    ...(push && typeof push === "object" ? {push: {remote: push.remote, commit: push.commit, outcome: push.outcome}} : {})};
}
// The hub's saved receipt matches the one sent, or, from a hub or tt older
// than the diagnostics that dropped them, matches it without them. A saved
// receipt holding any diagnostic, the push diagnostics (wi_8afe404c4867dafc)
// included, must match exactly.
export function sameReceipt(sent, saved) {
  if (digest(sent) === digest(saved)) return true;
  return digest(withoutDiagnostics(saved)) === digest(saved) && digest(withoutDiagnostics(sent)) === digest(saved);
}
// For the configured CLI only (cli), the reason also names the subcommand:
// its first one or two argv words when they are plain lowercase words. No
// flag, value or path of the call is ever part of it.
const CLI_WORD = /^[a-z][a-z-]{0,23}$/;
function childReason(argv, error, cli, transport) {
  const base = p => String(p).split("/").pop(), program = /\.(py|mjs)$/.test(argv[1] || "") ? base(argv[1]) : base(argv[0]);
  const word = i => argv[0] === cli && CLI_WORD.test(argv[i] || "") ? " " + argv[i] : "";
  const name = (/^[A-Za-z0-9._-]{1,64}$/.test(program) ? program : "program") + word(1) + (word(1) ? word(2) : "");
  if(error.code === "ENOBUFS" || error.code === "ERR_CHILD_PROCESS_STDIO_MAXBUFFER")return `${name} ENOBUFS (command buffer overflow)`;
  if(transport)return `${name} ${transport.cause}, ${transport.tries} ${transport.tries === 1 ? "try" : "tries"}`;
  const how = error.code === "ETIMEDOUT" ? "timeout" : /^SIG[A-Z0-9]{1,12}$/.test(error.signal || "") ? `signal ${error.signal}` : Number.isInteger(error.status) ? `exit ${error.status}` : "not started";
  let fields = "";
  try {
    const last = JSON.parse(String(error.stdout || "").trim().split("\n").at(-1)), token = v => typeof v === "string" && /^[a-z][a-z0-9-]{0,47}$/.test(v) ? v : null;
    const classification = token(last?.classification), stage = token(last?.stage);
    if (classification) fields = `: ${classification}${stage ? ` at ${stage}` : ""}`;
  } catch {}
  return `${name} ${how}${fields}`;
}
// A transport error of a call of the configured CLI (wi_9cb499ac2a14a1a0): the
// hub was not reached, or a proxy answered for it. It is read from the last
// line of the call's stderr, and only one of these fixed phrases is returned,
// never the line. A line that carries the hub's own answer ("hub: STATUS ...")
// is a transport error only for 502, 503 and 504: a refusal or a validation
// answer (4xx with a reason) and a 500 are the hub's decision. Without a hub
// status only these phrases of the Go HTTP client and the network count
// (wi_f2e585b63f57b6f8, wi_27a082e16c37d3cd): a deadline, a refused, reset or
// closed connection, an EOF the client read in place of an answer, a broken
// pipe, an unreachable network or host, a host that is down, a connect or TLS
// handshake that timed out and a host name that does not resolve. The two EOF
// forms count only as the client prints them (METHOD "URL": EOF), since tt
// also reads a local --file and a bare "unexpected EOF" may be its own parse.
// A line that merely says timeout or timed out is not a transport error, so
// the call is not made again and its failure is judged as any other. A call
// the runner itself killed at its command timeout (ETIMEDOUT) is kept as a
// transport error: the hub gave no answer, so it cannot be the hub's decision,
// and dropping it would turn a hung fence check into a lost fence. It is still
// never made again when it used up the retry window.
const TRANSPORT = [[/context deadline exceeded/i, "timed out: context deadline exceeded"], [/connection refused/i, "connection refused"], [/connection reset/i, "connection reset"], [/i\/o timeout/i, "timed out: i/o timeout"], [/Client\.Timeout exceeded/, "timed out: Client.Timeout exceeded"],
  [/"[^"]*": unexpected EOF$/, "unexpected EOF"], [/"[^"]*": EOF$/, "connection closed: EOF"], [/broken pipe/i, "broken pipe"], [/network is unreachable/i, "network is unreachable"], [/no route to host/i, "no route to host"],
  [/host is down/i, "host is down"], [/connect: (?:operation|connection) timed out/i, "timed out: connect"], [/no such host/i, "no such host"], [/TLS handshake timeout/, "timed out: TLS handshake"], [/server closed idle connection/, "server closed idle connection"]];
function transportCause(error) {
  if (error.code === "ENOBUFS" || error.code === "ERR_CHILD_PROCESS_STDIO_MAXBUFFER") return null;
  const text = typeof error.stderr === "string" ? error.stderr : Buffer.isBuffer(error.stderr) ? error.stderr.toString("utf8") : "";
  const line = (text.slice(-CLI_STDERR_BYTES).trim().split("\n").at(-1) || "").slice(-1024), hub = /\bhub: (\d{3})\b/.exec(line);
  if (hub) return ["502", "503", "504"].includes(hub[1]) ? `hub answered ${hub[1]}` : null;
  return TRANSPORT.find(([shape]) => shape.test(line))?.[1] ?? (error.code === "ETIMEDOUT" ? "no answer before the command timeout" : null);
}
// The calls a transport error may repeat: a send or a deployment call that
// carries a request id (the hub answers a repeat of the id with the original),
// and the reads. Any other call is made once.
function cliIdempotent(argv) {
  const id = argv.indexOf("--request-id");
  if (["send", "deployment"].includes(argv[1]) && id > 1 && argv[id + 1]) return true;
  return (argv[1] === "deployment" && (["get", "list", "handler"].includes(argv[2]) || (argv[2] === "window" && argv.length === 3))) || argv[1] === "agents";
}
// The bound of that retry: at most tries calls, delaysMs before the second and
// the third, and no retry that would start later than windowMs after the first
// call began. A retried call runs under at most windowMs, so a call that hung
// for its whole command timeout (600 s by default) is never made again. The
// wait is synchronous, like the calls; tests replace sleep.
export const cliRetry = {tries: 3, delaysMs: [5000, 20000], windowMs: 60000, sleep: ms => { Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, ms); }};
// What the private journal directory keeps of a failed call of the configured
// CLI, in cli-failures.json (0600): the exact argv, working directory, exit
// code, signal and the end of stderr. argv can hold Board text and private
// paths, so none of it leaves this file; the Board and the job journal get
// only childReason's text. The newest CLI_FAILURES distinct failures are kept;
// a repeat of a kept one only raises its count and lastAt, so a failing poll
// cannot grow the file. A record that cannot be written is dropped. Every
// failed try of a retried call is recorded, a later success notwithstanding.
const CLI_FAILURES = 20, CLI_STDERR_BYTES = 16384;
// The hub's refusal of a page whose ledger changed after its token was
// issued, as HostAdapter.command keeps it: the shape release-inputs.mjs
// restarts its summary read on.
const STALE_SNAPSHOT = /\bhub: 409 [^\n]*\brelease: stale list snapshot\s*$/;
const staleSnapshot = error => typeof error?.refusal === "string" && STALE_SNAPSHOT.test(error.refusal);
// The hub's 409 for a native release call sent at a generation the job has
// left (releaseConflict "generation changed"), as the CLI prints it. Only this
// yes or no is taken from the child's stderr; none of its text is kept.
const GENERATION_CHANGED = /\bhub: 409 [^\n]*\brelease: generation changed\s*$/;
const generationChanged = error => GENERATION_CHANGED.test((typeof error?.stderr === "string" ? error.stderr : Buffer.isBuffer(error?.stderr) ? error.stderr.toString("utf8") : "").slice(-CLI_STDERR_BYTES));
// A refuse carrying a refusal answered as by a hub or tt from before refusals
// were kept (wi_9b18784bb4bffb14): the hub refused the request's shape (400:
// the unknown refusal field, or a refusal it holds invalid), tt does not know
// the flag, or the hub saved a bare refuse under this request ID before.
// A block carrying a revert (wi_816228d539a80c48) is answered the same way by
// a hub or tt from before reverts were kept on blocks.
const REFUSAL_REFUSED = [/\bhub: 400 [^\n]*\s*$/, /\bhub: 409 [^\n]*\brelease: retry changed\s*$/, /\bflag provided but not defined: -(?:reason|step|target|revert-outcome|revert-commit|revert-bug-request)\b/];
const refusalRefused = error => {const text = (typeof error?.stderr === "string" ? error.stderr : Buffer.isBuffer(error?.stderr) ? error.stderr.toString("utf8") : "").slice(-CLI_STDERR_BYTES);return REFUSAL_REFUSED.some(p => p.test(text));};
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
// The wake class of a notice posted with no recipient, sent as its wake ref.
// The inbox relay wakes the primary database handler and the owner helper for
// such a notice unless the ref is exactly routine (a notice the Board keeps
// and nobody has to act on); attention is a refused, failed, held or
// recovery-needed release. A directed notice carries no class.
export const WAKE = {routine: "routine", attention: "attention"};
const wakeRef = wake => ["--ref", `wake=${wake}`];
// The Board notice for a recorded CLI failure: the safe reason and the job,
// nothing else of the call. key is one per job and reason for a process; the
// request id also carries when the kept failure was first seen, so a later
// recurrence is a new notice while a restart's resend is not.
export function cliFailureNotice(failure, run) {
  const reason = REASON.test(failure?.reason || "") ? failure.reason : "unclassified", job = /^rel_[A-Za-z0-9]{1,40}$/.test(failure?.jobId || "") ? failure.jobId : null;
  const id = createHash("sha256").update(JSON.stringify([failure?.at ?? null, job, reason])).digest("hex").slice(0, 16);
  return {key: `cli-failure ${job} ${reason}`, requestId: `cli-failure-${NAME(run)}-${id}`, jobId: job, wake: WAKE.routine, subject: "A deployer CLI call failed",
    text: `Deployer CLI call failed: ${reason}${job ? ` (release ${job})` : ""}. The exact argv, exit code and stderr are kept in cli-failures.json in the private journal directory of the deployer.`};
}
// A transport wait (wi_f2e585b63f57b6f8): the polls, one after another, in
// which a call of the configured CLI spent its tries on a transport error. It
// is kept in JOURNAL/transport-wait.json (0600), so it belongs to the job and
// outlives the process: a restarted or re-executed runner goes on with the
// same wait. It names the job whose call failed, once one is known; a failure
// of another job begins a new wait. It ends at the first poll in which no
// call ends that way. One routine notice (cliFailureNotice of its first
// failure, keyed by when the wait began) is posted per wait, and one
// attention notice once it has lasted escalateMs. Either is tried once per
// poll until the hub takes it (posts or refuses it). Both are also tried,
// each on its own, at the poll that ends the wait, and the wait ends only
// once neither is still owed: an unreached one keeps the file for the next
// poll. The file holds only times, the job id and childReason's fixed
// phrases. Tests replace now.
export const transportWait = {escalateMs: 600000, now: () => Date.now()};
// A held claim notice's owner-helper lookup that failed is not repeated for
// rosterMs (wi_56ac00ba2854fa3d c3). Tests replace now.
export const heldRetry = {rosterMs: 300000, now: () => Date.now()};
const transportWaitPath = dir => join(dir, "transport-wait.json");
function readTransportWait(dir) {
  try { const wait = JSON.parse(readFileSync(transportWaitPath(dir), "utf8")); return wait?.version === 1 && Number.isFinite(Date.parse(wait.since)) ? wait : null; } catch { return null; }
}
// The attention notice of a wait that passed the bound. It is built only from
// what was saved when the bound passed (due), so every resend is identical
// and the hub answers it with the original.
export function transportWaitNotice(wait) {
  const due = wait?.due || {}, job = /^rel_[A-Za-z0-9]{1,40}$/.test(due.jobId || "") ? due.jobId : null, cause = REASON.test(due.cause || "") ? due.cause : "unclassified";
  const since = new Date(wait?.since).toISOString(), minutes = Number.isSafeInteger(due.minutes) ? due.minutes : 0;
  return {requestId: `transport-wait-${createHash("sha256").update(JSON.stringify([since, job])).digest("hex").slice(0, 16)}`, jobId: job, wake: WAKE.attention, subject: "The deployer has not reached the hub for longer than its bound",
    text: `${job ? `Release ${job} has` : "The polls of the deployer have"} waited on hub transport failures since ${since}, longer than the ${minutes} minute bound. Cause: ${cause}. A job that is not yet published stays claimed and unrefused, and the deployer tries again at every poll. Check that the hub answers from the host of the deployer; see the transport wait step in docs/project-deployment.md.`};
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
  return {key: `claim-held ${job.id}`, requestId: `${job.id}-claim-held-${NAME(run)}-${id}`, jobId: job.id, item, commit, wake: WAKE.attention,
    subject: journal ? "A release job is skipped because its set-aside journal is held" : "A release job is skipped because the hub refused its claim",
    text: `Release ${job.id} (item ${item || "unknown"}, commit ${commit ? commit.slice(0, 12) : "unknown"}) is verified but not claimed: ${journal ? "the journal of its set-aside claim is held" : "the hub refused the claim"}. Reason: ${why}. Nothing was integrated, published or deployed. The deployer skips this job at every poll and goes on to later jobs; this notice is sent once per job per deployer run to the database handler and to the owner helper. Database handler: reconcile the job (Held claim in docs/project-deployment.md). Owner helper: for information.`};
}
const prerequisiteError = names => releaseError("Missing matrix prerequisites: " + names.join(", "));
export { MATRIX_PREREQUISITES, missingPrerequisites, provisionPrerequisites };
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
  return {requestId: full.length <= 128 ? full : id(identity.slice(0, 8)), wake: WAKE.routine, subject: "A release job is waiting for the verification host",
    text: `Release ${job.id} waits for the verification host at position ${wait.position} of ${wait.length} at priority ${NAME(wait.priority)} ${holders.length ? "behind " + holders.map(h => `${NAME(h.item)}/${NAME(h.agent)}/pid ${Number.isSafeInteger(h.pid) ? h.pid : "unknown"}`).join("; ") : "with no holder"}`};
}
export function matrixHeldNotice(job, held) {
  if (!job?.id || !held || !(ATTEMPT.test(held.attempt || "") || held.attempt === ALL_ATTEMPTS)) return null;
  const groups = (held.groups || []).filter(Number.isSafeInteger);
  return {requestId: `${job.id}-matrix-held-${held.attempt}`, wake: WAKE.attention, subject: "A release job is held until its matrix run is confirmed stopped",
    text: `Release ${job.id} is held: its matrix run could not be confirmed stopped (pid ${Number.isSafeInteger(held.pid) ? held.pid : "unknown"}; check groups still alive: ${groups.join(",") || "none"}; ${REASON.test(held.reason || "") ? held.reason : "unclassified"}). It keeps the project release fence and no other job integrates. Confirm nothing of that run is alive, then follow the held matrix run step in docs/project-deployment.md.`};
}
// The release quiet window (docs/project-deployment.md, "Release quiet
// window") as tt deployment window prints it: null when none is set, else the
// window with the hub's open and, while open, until. Only the fields the
// runner check and its notice use are kept, each checked for its shape;
// anything else is unreadable, and an unreadable window covers every target.
const WINDOW_TIME = /^\d\d:\d\d$/, WINDOW_ZONE = /^[A-Za-z0-9_+\/-]{1,64}$/, WINDOW_UNTIL = /^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d{1,9})?(?:Z|[+-]\d\d:\d\d)$/;
export function releaseWindowStatus(w) {
  if (w === null) return null;
  const unreadable = () => releaseError("Release window unreadable");
  if (!w || typeof w !== "object" || Array.isArray(w) || typeof w.open !== "boolean" || !WINDOW_ZONE.test(w.zone || "") || !WINDOW_TIME.test(w.start || "") || !WINDOW_TIME.test(w.end || "")) throw unreadable();
  if (!Array.isArray(w.targets) || !w.targets.length || w.targets.some((t, i) => !TARGETS.includes(t) || w.targets.indexOf(t) !== i)) throw unreadable();
  if (w.open && (!WINDOW_UNTIL.test(w.until || "") || !Number.isFinite(Date.parse(w.until)))) throw unreadable();
  return {open: w.open, ...(w.open ? {until: w.until} : {}), zone: w.zone, start: w.start, end: w.end, targets: [...w.targets]};
}
export function parseReleaseWindow(raw) {
  let w;try{w=JSON.parse(raw);}catch{throw releaseError("Release window unreadable");}
  return releaseWindowStatus(w);
}
// The notice for a claimed job the runner check holds (runRelease's
// windowWait): the covered targets and the window end, or the read failure.
// One per job and window end (or cause) and covered targets, so a restart
// resends the same notice and the next night's wait is a new one. A window
// that could not be read needs attention; an open one is routine.
export function windowWaitNotice(job, wait) {
  if (!/^rel_[A-Za-z0-9_-]{1,64}$/.test(job?.id || "") || !Array.isArray(wait?.covered) || !wait.covered.length || wait.covered.some(t => !TARGETS.includes(t))) return null;
  const unread = wait.until === undefined, cause = REASON.test(wait.cause || "") ? wait.cause : "unclassified";
  if (!unread && !WINDOW_UNTIL.test(wait.until)) return null;
  const id = createHash("sha256").update(JSON.stringify([unread ? cause : wait.until, wait.covered])).digest("hex").slice(0, 12);
  const span = WINDOW_TIME.test(wait.start || "") && WINDOW_TIME.test(wait.end || "") && WINDOW_ZONE.test(wait.zone || "") ? ` ${wait.start}-${wait.end} ${wait.zone}` : "";
  const kept = "It keeps its claim and integrated run; nothing was bound, published or deployed.";
  return {requestId: `${job.id}-window-wait-${id}`, jobId: job.id, wake: unread ? WAKE.attention : WAKE.routine, subject: "A release job waits for the release quiet window",
    text: unread ? `Release ${job.id} waits: the release quiet window could not be read (${cause}), so every selected target counts as covered: ${wait.covered.join(", ")}. ${kept} It goes on at the first poll that reads the window and finds none of these targets covered; check that tt deployment window answers on the host of the deployer.`
      : `Release ${job.id} waits for the release quiet window${span}, which covers its selected targets ${wait.covered.join(", ")} until ${wait.until}. ${kept} It goes on at the first poll after the window ends; no unhold is needed.`};
}
const sha = s => /^[a-f0-9]{40}$/.test(s || "");
const git = (cwd,...argv) => execFileSync("git",argv,{cwd,encoding:"utf8",stdio:["ignore","pipe","pipe"]}).trim();
const ancestor = (cwd,a,b) => {try{git(cwd,"merge-base","--is-ancestor",a,b);return true;}catch{return false;}};
// Whether tip already carries the candidate: its commit is its base, or it
// is an ancestor of tip and every path it changes has its content at tip.
// Ancestry alone is not enough: a rolled-back release's revert keeps the
// commit an ancestor of tip without its content, and a retry must ship it.
function carried(cwd,job,tip){
  if(job.commit===job.baseCommit)return true;
  if(!ancestor(cwd,job.commit,tip))return false;
  const paths=execFileSync("git",["diff","--name-only","--no-renames","-z",job.baseCommit,job.commit],{cwd,encoding:"utf8",stdio:["ignore","pipe","pipe"]}).split("\0").filter(Boolean);
  if(!paths.length)return true;
  try{execFileSync("git",["--literal-pathspecs","diff","--quiet",job.commit,tip,"--",...paths],{cwd,stdio:["ignore","ignore","ignore"]});return true;}catch{return false;}
}
// What a candidate that cannot fast-forward adds on top of tip: nothing when
// tip carries it (an accepted no-change result), else its linear series from
// base. A merge commit in the series is refused.
function candidateSeries(cwd,job,tip){
  if(carried(cwd,job,tip))return [];
  const commits=git(cwd,"rev-list","--reverse",`${job.baseCommit}..${job.commit}`).split("\n").filter(Boolean);
  if(commits.some(c=>git(cwd,"rev-list","--parents","-n","1",c).split(" ").length!==2))throw releaseError("Nonlinear candidate series");
  return commits;
}
// The reason a single job settles with nothing integrated, published or deployed.
export const NO_CHANGE="candidate-already-on-tasks-hub";
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
  let commits=null;
  try {
    if (ancestor(cwd,expected,job.commit)) git(cwd,"checkout","--detach",job.commit);
    else if ((commits=candidateSeries(cwd,job,expected)).length) git(cwd,"cherry-pick",...commits);
  } catch (error) {abortIntegration(cwd,expected,error);}
  if (commits && !commits.length) return {expected,integrated:expected,changed:[],noChange:NO_CHANGE};
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
// The ids of a matrix receipt's failed checks: named only when they match
// CONFLICT_PATH, at most CONFLICT_PATHS; any other is counted in unnamed.
export function failedChecks(receipt){
  const failed=(Array.isArray(receipt?.checks)?receipt.checks:[]).filter(c=>c?.exitCode!==0 && c?.knownFailure!==true).map(c=>c?.id);
  const named=[...new Set(failed.filter(id=>typeof id==="string" && CONFLICT_PATH.test(id)))].slice(0,CONFLICT_PATHS);
  return {failedChecks:named,unnamed:failed.filter(id=>!named.includes(id)).length};
}
// Release batching. With releaseBatch.maxJobs above 1 in the private config,
// the deployer integrates the verified jobs queued behind the one it claimed
// (the lead) that cherry-pick cleanly, in queue order, into one candidate,
// and runs one matrix, one import and one deploy for them. That matrix run
// keeps the union of the joined jobs' approved checks. Only the lead is
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
// The union of base's and extra's checks, or null when they cannot share one
// run: a check whose id base lacks is appended, and a check whose id base has
// must equal it, except go-race, which may differ in packages only. The merged
// go-race tests the union, or ./... alone.
function mergeAcceptedChecks(base,extra){
  if(!Array.isArray(base) || !Array.isArray(extra))return null;
  let out=base;
  for(const check of extra){
    if(typeof check?.id!=="string")return null;
    const at=out.findIndex(c=>c.id===check.id);
    if(at<0){out=[...out,check];continue;}
    if(canonical(out[at])===canonical(check))continue;
    const have=racePackages(out[at].argv),want=check.id==="go-race" && racePackages(check.argv);
    if(!have || !want || !same(have.flags,want.flags) || canonical({...out[at],argv:[]})!==canonical({...check,argv:[]}))return null;
    const packages=[have,want].some(p=>p.packages.includes("./..."))?["./..."]:[...new Set([...have.packages,...want.packages])].sort();
    out=out.with(at,{...out[at],argv:[...have.flags,...packages]});
  }
  return out;
}
// The accepted checks one matrix run must keep for the lead and the members
// that can share it: their union. A member with a check of an id already in
// the union that differs other than go-race packages is dropped.
export function batchAcceptedChecks(lead,members){
  let checks=lead.plan.checks;const joined=[],dropped=[];
  for(const member of members){
    const merged=mergeAcceptedChecks(checks,member.plan?.checks);
    if(merged){checks=merged;joined.push(member.id);}else dropped.push({jobId:member.id,reason:"batch-plan-mismatch"});
  }
  return {checks,checksDigest:digest(checks),joined,dropped};
}
// Why a job's full record cannot join the lead's batch, or "". The hub checks
// the same at batch-add, except batch-plan-mismatch: a check with an id the
// lead's plan has that differs other than go-race packages. A check id the
// lead lacks joins the batch's union; a conflict with an earlier member's
// check is found as the batch forms (integrateBatch's fits hook).
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
// One member's commits cherry-picked onto head, or head itself for a member
// head already carries; a failure leaves the checkout on head and throws the
// refusal integrateCandidate would.
function pickCandidate(cwd,job,head){
  if(!sha(job.commit) || !sha(job.baseCommit) || job.plan?.commit!==job.commit)throw releaseError("Release binding mismatch");
  let known;
  try{known=git(cwd,"rev-parse",`${job.commit}^{commit}`)===job.commit && git(cwd,"rev-parse",`${job.baseCommit}^{commit}`)===job.baseCommit;}catch{known=false;}
  if(!known)throw releaseError("Unknown commit identity");
  try{git(cwd,"merge-base","--is-ancestor",job.baseCommit,job.commit);}catch{throw releaseError("Unknown candidate base");}
  try{
    const commits=candidateSeries(cwd,job,head);
    if(commits.length)git(cwd,"cherry-pick",...commits);
  }catch(error){abortIntegration(cwd,head,error);}
  return git(cwd,"rev-parse","HEAD");
}
// Integrates the lead as integrateCandidate does, then each candidate in turn
// until the batch holds max jobs. A candidate that does not pick cleanly, or
// that the hub refuses, drops out with the checkout back on the last job that
// joined. hooks.integrated(first) follows the lead's integration; hooks.open
// (base) declares the batch before the first member, returning its id or
// false, and hooks.add(job, from, to) declares each job's range, the lead's
// first. hooks.fits(job), when given, is asked before a candidate is picked:
// a reason drops it with the checkout untouched. A hook that throws (an
// unconfirmed hub call) is passed on with the checkout untouched. Returns today's integration, the jobs dropped, and
// batch only when a member joined. The hub refuses a range that adds no
// commit, so a lead with no change opens no batch, and a member the batch's
// head already carries is dropped as batch-no-change; each releases alone.
export async function integrateBatch(cwd,lead,candidates,max,hooks={}){
  const first=integrateCandidate(cwd,lead),jobs=[],dropped=[];
  await hooks.integrated?.(first);
  if(first.noChange)return {...first,dropped};
  const range=(job,from,to)=>({jobId:job.id,entryId:job.entryId,generation:job.generation,commit:job.commit,verificationDigest:job.verificationDigest,from,to});
  let head=first.integrated,id=null,closed=false;
  for(const member of candidates){
    if(closed || Math.max(jobs.length,1)>=max)break;
    const misfit=hooks.fits?.(member);
    if(typeof misfit==="string" && misfit){dropped.push({jobId:member.id,reason:misfit});continue;}
    let to;
    try{to=pickCandidate(cwd,member,head);}
    catch(error){dropped.push({jobId:member.id,reason:failureReason(error)});continue;}
    if(to===head){dropped.push({jobId:member.id,reason:"batch-no-change"});continue;}
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
// The push limit (wi_8afe404c4867dafc p2): a hand push on 2026-10-19 took
// more than 60 s against the old, unstated 120 s, so the limit is 5 min.
// There is no retry: one read of the remote after a failure finds a push
// that landed late, and any other failure goes to a human.
// The stop (wi_01b550d20e610c85): git runs as its own process-group leader,
// and a failed or timed-out push signals the whole group, SIGTERM and then
// SIGKILL after PUSH_STOP_GRACE_MS, so a transport child (ssh, a remote
// helper) cannot land the push after the re-check. The re-check runs only
// once the group is gone; a group still present after PUSH_STOP_WAIT_MS is
// not read and counts as unknown.
// The resume (wi_56e50fb1bc6bbdaa): the group and its process-instance proof
// (the leader's start time and the host's boot identity) are journaled before
// the runner waits on the push, so a runner that stopped mid-push leaves it
// recorded. A resumed run stops a recorded group that is still ours with the
// same bounds before it reads the remote or pushes again; a group it cannot
// prove ours is never signalled. A clean stop of the runner lets the push
// finish, so only a runner killed mid-push leaves a group for the resume.
export const PUSH_TIMEOUT_MS = 300000, REMOTE_CHECK_TIMEOUT_MS = 30000, PUSH_STDERR_BYTES = 2048;
export const PUSH_STOP_GRACE_MS = 5000, PUSH_STOP_WAIT_MS = 15000;
// The stable reason tags of a failed push, classified from its stderr,
// which is never copied to the receipt or the Board.
export const PUSH_REASONS = ["push-timed-out", "push-killed", "push-rejected", "push-auth-failed", "push-network-failed", "push-failed"];
const PUSH_CLASSES = [
  ["push-rejected", /\[rejected\]|non-fast-forward|fetch first/],
  ["push-auth-failed", /Authentication failed|could not read Username|\b403\b|Permission denied/],
  ["push-network-failed", /Could not resolve host|unable to access|RPC failed|early EOF|Connection (timed out|refused|reset)/],
];
// The push stderr the private journal keeps: control characters other than
// newlines removed, URL userinfo replaced, each line redacted and bounded by
// sanitizeCapture, and only the last PUSH_STDERR_BYTES bytes.
export function pushStderr(raw) {
  const text = Buffer.isBuffer(raw) ? raw.toString("utf8") : typeof raw === "string" ? raw : "";
  const lines = text.replace(/[\u0000-\u0009\u000b-\u001f\u007f-\u009f]/g, "").replace(/([a-z][a-z0-9+.-]*:\/\/)[^\s\/@]+@/gi, "$1[redacted]@").split("\n").filter(l => l !== "");
  const kept = sanitizeCapture({logs: [{service: "git", lines}]}).logs[0].lines.join("\n"), bytes = Buffer.from(kept, "utf8");
  return bytes.length <= PUSH_STDERR_BYTES ? kept : bytes.subarray(bytes.length - PUSH_STDERR_BYTES).toString("utf8").replace(/^\uFFFD+/, "");
}
// ps and sysctl render dates in the caller's TZ and locale, so they are read
// in a fixed one: a runner resumed from another shell reads the same text.
const FIXED_ENV = () => ({...process.env, TZ:"UTC", LC_ALL:"C"});
const MONTHS = ["Jan","Feb","Mar","Apr","May","Jun","Jul","Aug","Sep","Oct","Nov","Dec"];
// The start time of a live pid as an ISO UTC string, or null.
export function processStartTime(pid) {
  let text;
  try {text = execFileSync("ps",["-o","lstart=","-p",String(pid)],{encoding:"utf8",stdio:["ignore","pipe","ignore"],timeout:5000,env:FIXED_ENV()}).trim();} catch {return null;}
  const m = /^[A-Z][a-z]{2} +([A-Z][a-z]{2}) +(\d{1,2}) +(\d{2}):(\d{2}):(\d{2}) +(\d{4})$/.exec(text), month = m ? MONTHS.indexOf(m[1]) : -1;
  return month < 0 ? null : new Date(Date.UTC(+m[6], month, +m[2], +m[3], +m[4], +m[5])).toISOString();
}
// The host's boot identity: Linux's boot_id or kern.boottime's numbers on
// macOS, or null.
export function bootIdentity() {
  try {const id = readFileSync("/proc/sys/kernel/random/boot_id","utf8").trim(); if (id) return id;} catch {}
  try {return bootKey(execFileSync("sysctl",["-n","kern.boottime"],{encoding:"utf8",stdio:["ignore","pipe","ignore"],timeout:5000,env:FIXED_ENV()}));} catch {return null;}
}
// The comparable form of a recorded or live identity, or null for text no
// rule compares. A start time is ISO UTC; one journaled before was ps's
// rendered date. A boot is a boot_id or kern.boottime's sec and usec, which
// an earlier journal's full kern.boottime text still holds.
const startKey = text => typeof text === "string" && /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z$/.test(text) ? text : null;
function bootKey(text) {
  if (typeof text !== "string") return null;
  if (/^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(text.trim())) return text.trim().toLowerCase();
  const m = /\{ *sec *= *(\d+), *usec *= *(\d+) *\}/.exec(text);
  return m ? `{ sec = ${m[1]}, usec = ${m[2]} }` : null;
}
// The one rule both the push group and matrix run proofs use: "same" or
// "other" when the recorded and live values compare, else "unproven".
function identityMatch(recorded, live, key) {
  const a = key(recorded), b = key(live);
  return a === null || b === null ? "unproven" : a === b ? "same" : "other";
}
export const startMatch = (recorded, live) => identityMatch(recorded, live, startKey);
export const bootMatch = (recorded, live) => identityMatch(recorded, live, bootKey);
// Whether a start time compares and is no later than at (ISO) plus slackMs.
export const startedBy = (started, at, slackMs) => startKey(started) !== null && Date.parse(started) <= Date.parse(at) + slackMs;
// What a journaled push group is now: "gone" (no process is left in it, or
// its pid now belongs to another process or another boot, so the push is
// over), "ours" (its leader still runs with the recorded start time on the
// recorded boot) or "unproven" (still present without that proof, as with a
// value that does not compare). Only an "ours" group is ever signalled.
export function pushGroupState(group) {
  const pgid = group?.pgid;
  if (!Number.isSafeInteger(pgid) || pgid <= 1) return "unproven";
  if (groupGone(pgid)) return "gone";
  const boot = bootMatch(group.boot, bootIdentity()), started = startMatch(group.startedAt, processStartTime(pgid));
  if (boot === "other" || started === "other") return "gone";
  return boot === "same" && started === "same" ? "ours" : "unproven";
}
// Signals the push's group, SIGKILL after graceMs, and resolves true once
// no process is left in it, or false at stopMs.
export async function stopPushGroup(pgid, {graceMs=PUSH_STOP_GRACE_MS, stopMs=PUSH_STOP_WAIT_MS}={}) {
  const signal = name => {try {process.kill(-pgid, name);} catch {}}, start = Date.now();
  let killed = false;
  signal("SIGTERM");
  while (!groupGone(pgid)) {
    if (Date.now() - start >= stopMs) return false;
    if (!killed && Date.now() - start >= graceMs) {signal("SIGKILL"); killed = true;}
    await new Promise(r => setTimeout(r, 20));
  }
  return true;
}
// One push: {outcome, elapsedMs} and, for a failed one, timedOut, exitCode
// or signal, reason, the sanitized stderr and stopped (whether the push's
// process group is gone). elapsedMs stays within the hub's bound. onGroup
// receives the group's record ({pgid, startedAt, boot}) before the wait. git,
// timeoutMs, graceMs and stopMs are test seams.
export async function pushRelease(cwd, commit, {remote="origin", git="git", timeoutMs=PUSH_TIMEOUT_MS, graceMs=PUSH_STOP_GRACE_MS, stopMs=PUSH_STOP_WAIT_MS, onGroup}={}) {
  if (!sha(commit)) return {outcome:"failed", elapsedMs:0, reason:"push-failed"};
  const start = Date.now(), elapsed = () => Math.min(PROBE_WAIT_MAX_MS, Math.max(0, Date.now() - start));
  let child;
  try {child = spawn(git,["push","--quiet",remote,`${commit}:refs/heads/tasks-hub`],{cwd,detached:true,stdio:["ignore","ignore","pipe"],env:{...process.env,GIT_TERMINAL_PROMPT:"0"}});}
  catch {return {outcome:"failed", elapsedMs:elapsed(), reason:"push-failed", stopped:true};}
  const group = Number.isSafeInteger(child.pid);
  // A group that cannot be journaled is stopped before the error goes on.
  if (group) {
    try {onGroup?.({pgid:child.pid, startedAt:processStartTime(child.pid), boot:bootIdentity()});}
    catch (error) {child.stderr?.destroy();child.on("error", () => {});await stopPushGroup(child.pid, {graceMs, stopMs});throw error;}
  }
  // The last 64 KiB of stderr; pushStderr keeps at most its last 2 KB.
  let raw = Buffer.alloc(0), exit = null;
  child.stderr?.on("data", chunk => {raw = Buffer.concat([raw, chunk]); if (raw.length > 65536) raw = raw.subarray(raw.length - 65536);});
  child.stderr?.on("error", () => {});
  const ended = new Promise(r => {child.once("exit", (code, signal) => {exit = {code, signal}; r();}); child.once("error", () => {exit ??= {}; r();});});
  const closed = new Promise(r => {child.once("close", r); child.once("error", r);});
  let timer;
  const timedOut = await Promise.race([ended.then(() => false), new Promise(r => {timer = setTimeout(() => r(true), timeoutMs);})]);
  clearTimeout(timer);
  if (!timedOut && exit.code === 0) return {outcome:"pushed", elapsedMs:elapsed()};
  // Any failed push stops its whole group before anything reads the remote.
  const stopped = group ? await stopPushGroup(child.pid, {graceMs, stopMs}) : true;
  if (stopped) await Promise.race([closed, new Promise(r => setTimeout(r, 1000))]);
  const result = {outcome:"failed", elapsedMs:elapsed()}, stderr = pushStderr(raw);
  if (timedOut) result.timedOut = true;
  if (Number.isInteger(exit?.code) && exit.code >= 1 && exit.code <= 255) result.exitCode = exit.code;
  else if (typeof exit?.signal === "string" && /^SIG[A-Z0-9]{1,12}$/.test(exit.signal)) result.signal = exit.signal.toLowerCase();
  result.reason = result.timedOut ? "push-timed-out" : result.signal ? "push-killed" : (PUSH_CLASSES.find(([, pattern]) => pattern.test(stderr)) || ["push-failed"])[0];
  if (stderr) result.stderr = stderr;
  result.stopped = stopped;
  return result;
}
// One bounded read of the remote's tasks-hub: {sha} (null when it has no
// such ref) or {unknown:true} when the read failed or timed out.
export function remoteTasksHub(cwd, {remote="origin", git="git", timeoutMs=REMOTE_CHECK_TIMEOUT_MS}={}) {
  try {
    const out = execFileSync(git,["ls-remote",remote,"refs/heads/tasks-hub"],{cwd,encoding:"utf8",stdio:["ignore","pipe","pipe"],timeout:timeoutMs,env:{...process.env,GIT_TERMINAL_PROMPT:"0"}});
    const line = out.split("\n").find(l => l.endsWith("\trefs/heads/tasks-hub"));
    if (!line) return out.trim() === "" ? {sha:null} : {unknown:true};
    const head = line.split("\t")[0];
    return sha(head) ? {sha:head} : {unknown:true};
  } catch {return {unknown:true};}
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
// One exception, for the journal only: a failed push keeps its stderr,
// sanitized and bounded by pushStderr. The receipt holds at most its sha256.
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
  // A hub write by someone else (the handler's import of integrated
  // verification or inputs) advances the job's generation under this claim.
  // When the fence check or the refuse call fails only for that, the job is
  // re-read once: still this exact claim, the new generation is adopted and
  // the call is sent once more; otherwise the fence is lost as before. Never
  // once the release is published or any effect is journaled.
  let adopted=0;
  const adopt=async journal=>{
    if(journal?.published===true || journal?.effects?.length || typeof adapter.adoptGeneration!=="function" || await adapter.adoptGeneration()!==true)return false;
    adopted++;return true;
  };
  const held=async journal=>{
    let fenced=await adapter.fence(job);
    if(fenced==="generation-changed" && await adopt(journal))fenced=await adapter.fence(job);
    return fenced===true;
  };
  // A fence check that could not reach the hub after its tries is not a lost
  // fence (wi_f2e585b63f57b6f8): while nothing is published and no effect is
  // journaled the job waits for the next poll, claimed and unrefused, with its
  // journal as the last poll left it. Here nothing was touched yet.
  const unpublished=journal=>journal?.published!==true && journal?.mergedSent!==true && !journal?.effects?.length;
  if(!finishOnly){
    let ok;
    try{ok=await held(pending);}catch(error){if(error?.transport===true && unpublished(pending))return {jobId:job.id,outcome:"waiting_transport"};throw error;}
    if(!ok)throw releaseError("release fence lost");
  }
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
    if(prior.jobId===job.id && prior.commit===job.commit && prior.agentId===job.agentId && prior.runId===job.runId && (prior.taskId===undefined || prior.taskId===job.taskId) && (prior.pauseGeneration===undefined || prior.pauseGeneration===job.pauseGeneration) && (["waiting_matrix","waiting_inputs","waiting_window","pushing","receipt_pending","finishing"].includes(prior.phase)) && (!["waiting_matrix","waiting_inputs","waiting_window"].includes(prior.phase) || !prior.effects.length) && git(cwd,"rev-parse","HEAD")===prior.integrated && !git(cwd,"status","--porcelain")) {state=prior;}
    else
    // A refusal whose refuse call failed, with nothing published and no
    // effects: this exact claim completes it below instead of holding the job.
    if(prior.phase==="refusing" && prior.refuseFailure!==undefined && prior.jobId===job.id && prior.commit===job.commit && prior.agentId===job.agentId && prior.runId===job.runId && (prior.taskId===undefined || prior.taskId===job.taskId) && (prior.pauseGeneration===undefined || prior.pauseGeneration===job.pauseGeneration) && Array.isArray(prior.effects) && !prior.effects.length && prior.published!==true) {state=prior;}
    else {
      closeSync(fd);rmSync(lock);
      if(prior.jobId===job.id && prior.commit===job.commit && prior.phase==="complete"){try{materializeBatch(dirname(journalPath),prior);}catch{}return prior.receipt;}
      throw releaseError("Ambiguous journal requires handler reconciliation");
    }
  }
  // The runner code that last ran this job; never part of a resume decision.
  if(config.code)state.code={loaded:config.code.loaded??null,current:config.code.current??null};
  // Progress for the hub's hung-runner check: each new phase at its
  // checkpoint and each step as it begins, once. Best effort; an adapter
  // without progress is not told.
  let step=null,toldPhase=null,toldStep=null;
  const tell=()=>{
    const at=step?`${step.step} ${step.target??""}`:null;
    if(typeof state.phase!=="string" || (state.phase===toldPhase && (at===null || at===toldStep)))return;
    toldPhase=state.phase;if(at!==null)toldStep=at;
    try{const sent=adapter.progress?.({jobId:job.id,phase:state.phase,...(step?{step:step.step}:{})});sent?.catch?.(()=>{});}catch{}
  };
  const checkpoint=()=>{save(journalPath,state);tell();};
  const begin=next=>{step=next;tell();};
  // How often this job went on at a generation the hub advanced under it.
  const noteAdopted=()=>{if(adopted){state.generationAdoptions=(state.generationAdoptions||0)+adopted;adopted=0;}};
  noteAdopted();
  // fenceWait marks the transport error of a fence check made before
  // publication; after it, the error is a failure like any other.
  const fence=async()=>{
    let ok;
    try{ok=await held(state);}catch(error){noteAdopted();if(error?.transport===true && unpublished(state))error.fenceWait=true;throw error;}
    noteAdopted();if(!ok)throw releaseError("release fence lost");
  };
  // The refusal of an unpublished job with no effects. A refuse call the hub
  // answers with "generation changed" is sent once more at the re-read
  // generation. A failed call leaves the journal in refusing with
  // refuseFailure beside the cause, and the next poll resumes here.
  // The refuse carries the refusal the hub keeps on the job (wi_9b18784bb4bffb14):
  // the stable tag, sent as receipt reasons are, and the failed step and target
  // when a step was running. Built from the journal alone, so a resumed
  // refusal sends the same.
  let refusing=false;
  const refuseUnpublished=async()=>{
    refusing=true;
    const failed=state.failure && STEP.test(state.failure.step||"") ? {step:state.failure.step,...(TARGETS.includes(state.failure.target)?{target:state.failure.target}:{})} : {};
    const refusal={...failed,reason:receiptReason(state.refusalReason)};
    try{
      try{await adapter.refuse(refusal);}
      catch(error){if(error?.generationChanged!==true || !await adopt(state))throw error;noteAdopted();await adapter.refuse(refusal);}
    }catch(refuseError){noteAdopted();state.refuseFailure=failureReason(refuseError);checkpoint();throw refuseError;}
    if(state.refuseFailure!==undefined){state.refuseRecovered=state.refuseFailure;delete state.refuseFailure;}
    state.phase="refused";checkpoint();await adapter.escalate({jobId:job.id,outcome:"refused",reason:state.refusalReason});throw releaseError("Release refused before publication");
  };
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
    if(ok && record.op==="batch-add" && state.batch){
      state.batch.jobs.push({...record.member,from:record.from,to:record.commit});
      if(record.checks)state.batch.checks=record.checks;
      // A journal of an earlier runner carries the merged go-race alone.
      if(record.goRace)state.batch.goRace=record.goRace;
    }
    record.applied=true;checkpoint();
  };
  const declare=async(op,fields)=>{const ok=await batchOp(op,fields),record=state.batchOps.findLast(o=>o.op===op);applyBatchOp(record,ok);return ok;};
  const batchDropped=()=>state.batchOps?.some(o=>o.op==="batch-drop" && o.status==="done")===true;
  // A batch no member joined is dropped at the hub and the lead goes alone.
  const closeBatch=async()=>{
    if(state.batch && state.batch.jobs.length<2){if(!batchDropped())await batchOp("batch-drop");state.batch=null;checkpoint();}
    return {expected:state.expected,integrated:state.batch?state.batch.jobs.at(-1).to:state.batchLead};
  };
  // The checks the batch's one run must keep so far: the union of the lead's
  // and every joined member's approved checks, each saved with the batch-add
  // that joined it. A refused add contributes nothing.
  const batchChecks=()=>state.batch?.checks ?? job.plan?.checks?.map(c=>c.id==="go-race" && state.batch?.goRace?state.batch.goRace:c);
  // Why a candidate cannot share the batch's run: a check with the id of one
  // already in it (the lead's or an earlier member's) that differs other than
  // go-race packages.
  const batchFits=member=>job.plan?.checks===undefined && member.plan?.checks===undefined ? "" : mergeAcceptedChecks(batchChecks(),member.plan?.checks)?"":"batch-plan-mismatch";
  // What a job's batch-add saves: the union with its checks, and on its range
  // its approved check ids and, for a member, those the lead's plan lacks.
  const joining=member=>{
    const own=member.plan?.checks,ids=Array.isArray(own)?own.map(c=>c?.id):undefined,lead=new Set((job.plan?.checks||[]).map(c=>c.id));
    const checks=member.id===job.id?undefined:mergeAcceptedChecks(batchChecks(),own);
    return {...(checks?{checks}:{}),fields:ids?{checkIds:ids,...(member.id===job.id?{}:{contributed:ids.filter(id=>!lead.has(id))})}:{}};
  };
  // A job left out of this claim's batch is listed once, with its first reason.
  const noteDropped=entries=>{for(const {jobId,reason} of entries)if(!state.batchDropped?.some(d=>d.jobId===jobId))(state.batchDropped??=[]).push({jobId,reason});};
  const openBatch=async()=>{
    const members=config.batch.candidates.filter(c=>{const reason=batchMismatch(job,c);if(reason)noteDropped([{jobId:c.id,reason}]);return !reason;});
    const result=await integrateBatch(cwd,job,members,config.batch.max,{
      integrated:first=>{state.expected=first.expected;state.integrated=first.integrated;state.batchLead=first.integrated;if(first.noChange)state.noChange=first.noChange;state.phase="batching";checkpoint();},
      open:async base=>await declare("batch-open",{commit:base})?state.batch.id:false,
      fits:batchFits,
      add:(member,from,to)=>{const {checks,fields}=joining(member);return declare("batch-add",{entryId:member.entryId,commit:to,from,...(checks?{checks}:{}),member:{jobId:member.id,entryId:member.entryId,generation:member.generation,commit:member.commit,verificationDigest:member.verificationDigest,...fields}});},
    });
    noteDropped(result.dropped);
    return closeBatch();
  };
  // Resuming a stopped declaration: the unconfirmed call is replayed before
  // any checkout, the batch closes with the jobs that joined, and only then
  // is the checkout put on its tip.
  const resumeBatch=async()=>{
    const pending=state.batchOps?.find(o=>o.status==="sending");
    if(pending && !await settleBatchOp(pending) && pending.op==="batch-add" && pending.member?.jobId!==job.id)noteDropped([{jobId:pending.member.jobId,reason:"batch-add-refused"}]);
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
    const checks=batchChecks();
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
  const fallback=async(reason,detail,failure)=>{
    state.batchFallback={batchId:state.batch.id,reason,...(detail && detail!==reason?{detail}:{}),members:state.batch.jobs.slice(1).map(j=>j.jobId),...attribution(failure),status:"dropping"};checkpoint();
    return finishFallback();
  };
  // Which jobs each failed check of the batch's run belongs to, from the
  // approved check ids saved with each batch-add, the lead first; a check
  // only the selection added belongs to none. A job none of them belongs to
  // is unaffected. Only for a failure that names or counts a failed check;
  // a refusal only for the matrix home cleanup error names that as its cause.
  const attribution=failure=>{
    const unnamed=Number.isSafeInteger(failure?.unnamed) && failure.unnamed>0?failure.unnamed:0;
    if(!Array.isArray(failure?.failedChecks) || (!failure.failedChecks.length && !unnamed))return failure?.homeCleanupError===true?{cause:"matrix-home-cleanup-error"}:{};
    const jobs=state.batch.jobs.filter(j=>Array.isArray(j.checkIds));
    const failed=failure.failedChecks.map(checkId=>({checkId,jobs:jobs.filter(j=>j.checkIds.includes(checkId)).map(j=>j.jobId)}));
    const unaffected=jobs.map(j=>j.jobId).filter(id=>!failed.some(f=>f.jobs.includes(id)));
    return {failed,unaffected,...(unnamed?{unnamed}:{})};
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
    // Targets are selected on the batch's integrated commit before its matrix
    // run starts, with the checkout on that commit. A selection that fails (a
    // path no map knows) drops the batch: each job is then selected alone, and
    // the one with the path is refused. When the hub refuses the drop, the
    // lead is refused with the selection's own reason. A selection that
    // passed is saved, so a resumed wait does not ask again beside its run.
    if(state.batchSelected!==state.integrated){
      let refusal;
      try{selectReleaseTargets(cwd,baselines,state.integrated);}catch(error){refusal=error;}
      if(refusal){
        const fell=await fallback("batch-selection-refused",failureReason(refusal));
        if(fell)return fell===true?null:waiting;
        begin({step:"select"});throw refusal;
      }
      state.batchSelected=state.integrated;checkpoint();
    }
    const clock=config.now||(()=>Date.now()),wait=config.batch?.importWaitMs??BATCH_IMPORT_WAIT_MS;
    for(let pass=0;;pass++){
      state.phase="waiting_matrix";checkpoint();await fence();
      let verified,failure;
      try{verified=await adapter.verifyIntegrated(batchJob());}catch(error){failure=error;}
      if(verified===true){state.phase="integrated";checkpoint();return null;}
      if(failure){
        const reason=failureReason(failure);
        const fell=await fallback(reason==="batch-plan-uncovered"?reason:"batch-matrix-failed",reason,failure);
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
  // The runner check of the release quiet window (wi_3427bf81094d2225): on
  // every pass, after the targets are selected and the integrated matrix is
  // imported, and before the inputs are bound or anything is published. The
  // hub holds only jobs it can see; this sees what the integrated commit
  // actually deploys, a commit no job or hand release records included. While
  // the window is open and covers a selected target, or cannot be read while
  // any target is selected, the job waits in waiting_window with its claim
  // and integrated run, and the next poll checks again. Nothing is refused,
  // fenced, bound or published here, and a read error never reaches the
  // refusal below. An empty selection is never read; neither is a pass whose
  // effects began (a resumed published journal), since those finish. An
  // adapter without releaseWindow (the test fakes) has no window.
  const quietWindow=async selected=>{
    if(!selected.length || state.published===true || state.mergedSent===true || state.effects.length || typeof adapter.releaseWindow!=="function")return null;
    let wait;
    try{
      const w=releaseWindowStatus(await adapter.releaseWindow()),covered=w?.open?selected.filter(t=>w.targets.includes(t)):[];
      if(covered.length)wait={covered,until:w.until,zone:w.zone,start:w.start,end:w.end};
    }catch(error){wait={covered:[...selected],cause:failureReason(error)};}
    if(!wait){delete state.windowWait;return null;}
    state.phase="waiting_window";state.windowWait=wait;checkpoint();
    return {jobId:job.id,outcome:"waiting_window",window:wait};
  };
  // Every selected target is live-verified before this runs, so nothing here
  // rolls back: a failed push, once its process group is gone, reads the
  // remote once and, unless the commit landed there, is escalated once; the
  // release stands. A group that outlived the stop bound is unknown, unread.
  // A resumed run first settles the push group its stopped run journaled
  // (state.pushGroup): one that is gone changes nothing; one still ours is
  // stopped with the push's bounds and, once gone, the remote is read before
  // any new push, which a landed commit makes unneeded; one that outlives the
  // bound, or is present without proof, is unknown, unread and not pushed
  // again. A clean runner stop does not end the push: the release finishes
  // first. config.push (git, timeoutMs, graceMs, stopMs, checkTimeoutMs) is a
  // test seam, never set in deploy.json.
  const pushAndFinish=async receipt=>{
    state.receipt=receipt;state.phase="pushing";checkpoint();
    const seam=config.push||{},readRemote=()=>{const read=remoteTasksHub(cwd,{git:seam.git,timeoutMs:seam.checkTimeoutMs});return read.unknown?"unknown":read.sha===receipt.commit?"landed":"absent";};
    let pushed,remoteCheck,earlier;
    if(state.pushGroup){
      const proof=pushGroupState(state.pushGroup),stopped=proof==="ours"?await stopPushGroup(state.pushGroup.pgid,{graceMs:seam.graceMs,stopMs:seam.stopMs}):proof==="gone";
      earlier={pgid:state.pushGroup.pgid,proof,stopped};
      if(!stopped){pushed={outcome:"failed",reason:"push-failed",stopped:false};remoteCheck="unknown";}
      else{
        delete state.pushGroup;checkpoint();
        if(proof==="ours"){earlier.remoteCheck=readRemote();if(earlier.remoteCheck==="landed")pushed={outcome:"pushed"};}
      }
    }
    if(!pushed){
      pushed=await pushRelease(cwd,receipt.commit,{git:seam.git,timeoutMs:seam.timeoutMs,graceMs:seam.graceMs,stopMs:seam.stopMs,onGroup:group=>{state.pushGroup=group;checkpoint();}});
      if(pushed.stopped!==false)delete state.pushGroup;
      if(pushed.outcome==="failed" && pushed.stopped===false)remoteCheck="unknown";
      else if(pushed.outcome==="failed")remoteCheck=readRemote();
    }
    const stderrSHA256=pushed.stderr?createHash("sha256").update(pushed.stderr).digest("hex"):undefined;
    // The private journal keeps the last five attempts, stderr included.
    state.pushAttempts=[...(state.pushAttempts||[]),{at:new Date().toISOString(),...pushed,...(stderrSHA256?{stderrSHA256}:{}),...(remoteCheck?{remoteCheck}:{}),...(earlier?{earlierGroup:earlier}:{})}].slice(-5);checkpoint();
    const cause=pushed.outcome==="failed"?{...(pushed.timedOut?{timedOut:true}:{}),...(pushed.exitCode!==undefined?{exitCode:pushed.exitCode}:{}),...(pushed.signal?{signal:pushed.signal}:{}),...(pushed.elapsedMs!==undefined?{elapsedMs:pushed.elapsedMs}:{}),reason:pushed.reason,remoteCheck,...(stderrSHA256?{stderrSHA256}:{})}:{};
    receipt.push={remote:"origin",commit:receipt.commit,outcome:pushed.outcome,...cause};checkpoint();
    if(receipt.push.outcome==="failed" && remoteCheck!=="landed" && !state.pushEscalated){state.pushEscalated=true;checkpoint();try{await adapter.escalate({jobId:job.id,outcome:"released",push:"failed",pushCause:cause});}catch{}}
    state.phase="finishing";checkpoint();await fence();state.finishGeneration=adapter.job?.generation??job.generation;checkpoint();await adapter.finish(receipt,state.finishGeneration);materialize();state.phase="complete";checkpoint();return receipt;
  };
  try {
    if(state.phase==="receipt_pending" || state.phase==="finishing"){
      await adapter.finish(state.receipt,state.finishGeneration);materialize();state.phase=state.receipt.outcome==="released"?"complete":"blocked";checkpoint();return state.receipt;
    }
    if(state.phase==="pushing")return await pushAndFinish(state.receipt);
    if(state.phase==="refusing")await refuseUnpublished();
    // A job with no resumable journal (a requeue) first settles the matrix
    // runs of its earlier attempts: until each is confirmed stopped, ended or
    // set aside, nothing here touches the checkout, starts a run or refuses.
    if(state.phase==="prepared" && adapter.settleMatrixRuns && await adapter.settleMatrixRuns(job)!==true)return {jobId:job.id,outcome:"waiting_matrix"};
    // The candidates the daemon left out at this claim, saved before anything
    // is integrated; a resumed run (any later poll) adds none, and with or
    // without a batch the lead's journal keeps them.
    if(state.phase==="prepared" && config.batch?.max>1 && Array.isArray(config.batch.excluded))noteDropped(config.batch.excluded);
    // A new job is journaled only once its fence check answered, so a check
    // that could not reach the hub leaves no journal for the next poll to
    // find ambiguous; a resumed one keeps the journal it has.
    if(state.phase==="prepared"){await fence();checkpoint();}else{checkpoint();await fence();}
    let integration;
    if(["waiting_matrix","waiting_inputs","waiting_window"].includes(state.phase))integration={expected:state.expected,integrated:state.integrated};
    else if(state.phase==="batching")integration=await resumeBatch();
    else if(config.batch?.max>1 && config.batch.candidates?.length){begin({step:"integrate"});integration=await openBatch();step=null;}
    else{begin({step:"integrate"});integration=integrateCandidate(cwd,job);step=null;if(integration.noChange)state.noChange=integration.noChange;}
    state.integrated=integration.integrated;state.expected=integration.expected;state.phase="integrated";checkpoint();
    // An accepted no-change result: its commit is already on tasks-hub, so
    // nothing is selected, verified, published, deployed or pushed. The hub
    // records it merged at its own commit, which needs no integrated matrix
    // receipt, and it settles released with no targets; the journal keeps
    // noChange as the reason. The checkout stays on the tip it found.
    // mergedSent is saved before the call: the hub may hold the job as
    // published even when its reply is lost, so a failed call ends blocked
    // for handler reconciliation, as after publication, and never refuses.
    if(state.noChange){
      await fence();state.mergedSent=true;checkpoint();await adapter.merged(job.commit);state.phase="merged";checkpoint();
      state.receipt={version:1,jobId:job.id,commit:job.commit,verificationDigest:job.verificationDigest,targets:[],outcome:"released"};
      state.phase="finishing";checkpoint();await fence();state.finishGeneration=adapter.job?.generation??job.generation;checkpoint();
      await adapter.finish(state.receipt,state.finishGeneration);state.phase="complete";checkpoint();return state.receipt;
    }
    if(state.batch){
      const waiting=await verifyBatch();if(waiting)return waiting;
      integration={expected:state.expected,integrated:state.integrated};
    }
    // Targets are selected before a single job's integrated matrix run, the
    // inputs request and the push, so a path no map knows refuses the job
    // unpublished. A batch was first selected before its matrix run
    // (verifyBatch) and is selected here again for its targets; a batch that
    // fell back there is selected here as the lead alone. A resumed wait
    // selects again; the answer depends only on the commits.
    begin({step:"select"});const selected=selectReleaseTargets(cwd,baselines,integration.integrated);step=null;
    if(!state.batch && integration.integrated!==job.commit){
      // Saved before the matrix run is started, so a runner stopped while
      // the run waits or runs resumes this job from its attempt record.
      state.phase="waiting_matrix";checkpoint();
      await fence();if(await adapter.verifyIntegrated({...job,integratedCommit:integration.integrated,...selection([job.plan])})!==true)return {jobId:job.id,outcome:"waiting_matrix"};
      state.phase="integrated";checkpoint();
    }
    const windowWait=await quietWindow(selected);if(windowWait)return windowWait;
    if(adapter.verifyInputs && !await adapter.verifyInputs(integration.integrated)){state.phase="waiting_inputs";checkpoint();return {jobId:job.id,outcome:"waiting_inputs"};}
    // w2: the pinned wrangler is installed (npm ci when the lockfile changed)
    // and checked before publication, so a missing one refuses the job
    // unpublished with no target deployed. It runs before the fence check, so
    // a claim lost during npm ci is still found unpublished.
    if(selected.includes("tailos") && adapter.releaseTools){begin({step:"prepare",target:"tailos"});await adapter.releaseTools(integration.integrated);step=null;}
    await fence();publishIntegration(cwd,integration.integrated,integration.expected);state.published=true;checkpoint();
    await adapter.merged(integration.integrated);state.phase="merged";checkpoint();
    const receipt={version:1,jobId:job.id,commit:integration.integrated,verificationDigest:job.verificationDigest,targets:[],outcome:"released"};
    state.receipt=receipt;
    // The batch receipt's intent is saved with the lead's, before any deploy,
    // with the ids and digest of the union of checks its one run kept.
    if(state.batch){const union=batchChecks();state.batchReceipt={version:1,batchId:state.batch.id,leadJobId:job.id,base:state.batch.base,integrated:integration.integrated,...(union?{checks:union.map(c=>c.id),checksDigest:digest(union)}:{}),jobs:state.batch.jobs,dropped:state.batchDropped||[]};}
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
      begin({step:"prepare",target});
      await fence();const artifact=await adapter.prepare(target,integration.integrated);pinned(target,artifact);
      const group=[[target,artifact]];
      for(const partner of (artifact.planTargets||[target]).filter(t=>t!==target)){
        if(!selected.includes(partner))throw releaseError("Paired plan partner is not selected");
        begin({step:"prepare",target:partner});
        await fence();const other=await adapter.prepare(partner,integration.integrated);pinned(partner,other);
        if(["planPath","release","backup","backupSHA256","preflightReceiptSHA256"].some(k=>other[k]!==artifact[k]) || !same(other.planTargets,artifact.planTargets))throw releaseError("Paired plan members disagree");
        group.push([partner,other]);
      }
      // A shared backup copy is rehearsed once; the last build pinned the migration binary.
      if(group.some(([,a])=>a.schemaChanged)){begin({step:"rehearse",target});await fence();if(await adapter.rehearse(group.at(-1)[1])!==true)throw releaseError("Backup-copy migration rehearsal failed");}
      const records=group.map(([t,a])=>{
        const record={target:t,release:a.release,artifactSHA256:a.artifactSHA256,...(a.backup?{backup:a.backup,backupSHA256:a.backupSHA256,preflightReceiptSHA256:a.preflightReceiptSHA256}:{}),...(a.version?{version:a.version}:{}),...(a.deployment?{deployment:a.deployment}:{}),outcome:"failed"};
        receipt.targets.push(record);state.effects.push({target:t,release:a.release,state:"attempting"});return record;
      });
      checkpoint();
      begin({step:"deploy",target});
      await fence();await adapter.deploy(target,artifact);
      group.forEach(([t,a],i)=>{if(a.deployment)records[i].deployment=a.deployment;if(a.version)records[i].version=a.version;state.effects.find(e=>e.target===t).state="deployed";});checkpoint();
      for(const [i,[t,a]] of group.entries()){
        begin({step:"live-check",target:t});
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
    // A resumed refusal that failed or finished above is not refused again.
    if(refusing)throw error;
    // An unreached fence check before publication: the job waits. The journal
    // stays in a phase the next poll resumes (the wait for inputs when the
    // check came just before publication), and the checkout is left as it is.
    if(error?.fenceWait===true && unpublished(state)){
      if(state.phase==="integrated"){state.phase="waiting_inputs";checkpoint();}
      return {jobId:job.id,outcome:"waiting_transport"};
    }
    // An unconfirmed batch call: nothing here may touch the checkout or the job.
    if(error?.batchWait)return {jobId:job.id,outcome:"waiting_batch"};
    // The first failed target step is kept; a resumed run never rewrites it.
    if(step && !state.failure){const rehearsal=failureRehearsal(error);state.failure={...step,reason:failureReason(error),...(rehearsal?{rehearsal}:{})};state.failureDetail??=failureDetail(error);checkpoint();}
    // The first failure's validated detail is kept even when no step was set;
    // a journal that cannot be written here is left to the checkpoints below.
    if(!state.failureDetail){state.failureDetail=failureDetail(error);try{checkpoint();}catch{}}
    if(state.phase==="pushing")return {jobId:job.id,outcome:"pushing"};
    if(state.phase==="finishing" || state.phase==="receipt_pending"){
      state.phase="receipt_pending";checkpoint();return {jobId:job.id,outcome:"receipt_pending"};
    }
    // An uncertain side effect cannot be replayed. Rollback uses only retained
    // target artifacts; the adapter must never restore an old live database.
    if(!state.published && !state.mergedSent && state.effects.length===0){
      state.phase="refusing";state.refusalReason=failureReason(error);checkpoint();
      // refusalReason stays the cause; a refuse call that itself fails is named beside it.
      await refuseUnpublished();
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
    // A target is verified only after its live check passed.
    const probeWaits=state.effects.flatMap(e=>[["live",e.liveCheck],["rollback",e.rollbackCheck]].filter(([,w])=>w).map(([probe,w])=>({target:e.target,probe,lastCommit:w.lastCommit,waitedMs:w.waitedMs,...(probe==="live"&&e.state==="verified"?{passed:true}:{})})));
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
      addDiagnostics(state.receipt,state.failure,state.effects);
      // Retryable like the success receipt: a lost response resumes this
      // exact receipt and generation, with no second rollback or escalation.
      state.finishGeneration=adapter.job?.generation??job.generation;state.phase="receipt_pending";checkpoint();
      try{await adapter.finish(state.receipt,state.finishGeneration);}catch{throw releaseError("Release failed; final receipt pending retry");}
      try{materialize();}catch{throw releaseError("Release failed; batch receipt pending retry");}
      state.phase="blocked";checkpoint();
    }else{
      // No receipt: the block carries the journaled revert, so the hub
      // records what tasks-hub holds (wi_816228d539a80c48).
      const v=state.revert;
      await adapter.block({integrated:state.integrated,...(v?{revert:{outcome:v.outcome,...(v.commit?{commit:v.commit}:{}),...(v.bugRequestId?{bugRequestId:v.bugRequestId}:{})}}:{})});
    }
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
  const expectedState = {claim:"claimed", check:prior.state, merged:"merged", finish:receipt?.outcome, block:"blocked", refuse:"refused", "batch-open":"claimed", "batch-add":"claimed", "batch-drop":"claimed", unclaim:"verified"}[operation];
  const advance = operation === "check" ? 0 : 1;
  const allowed = {claim:["verified"],check:["claimed","merged"],merged:["claimed"],finish:["merged"],block:["claimed","merged"],refuse:["claimed"],"batch-open":["claimed"],"batch-add":["claimed"],"batch-drop":["claimed"],unclaim:["claimed"]};
  if (!allowed[operation]?.includes(prior.state)) throw releaseError("Invalid native release transition");
  if (!next || Array.isArray(next) || next.summary === true || !immutable.every(samePin) || !Number.isSafeInteger(expectedGeneration) || expectedGeneration < 1 || next.generation !== expectedGeneration + advance || !expectedState || next.state !== expectedState) throw releaseError("Invalid native release response");
  // An unclaim gives the claim back: the job names no run afterwards.
  const agent = operation === "claim" ? process.env.TAILTERM_AGENT : operation === "unclaim" ? undefined : prior.agentId, run = operation === "claim" ? process.env.TAILTERM_RUN : operation === "unclaim" ? undefined : prior.runId;
  if (next.agentId !== agent || next.runId !== run) throw releaseError("Native release run binding mismatch");
  for (const k of ["inputsCommit", "inputsDigest"]) if (!samePin(k)) throw releaseError("Native release input binding mismatch");
  // A batch-add's --commit is the job's range end, never the lead's integrated
  // commit; batch-drop and unclaim are the places a cleared integrated commit
  // is expected.
  const integrated = operation === "merged" ? flag("--commit") : ["batch-drop", "unclaim"].includes(operation) ? undefined : prior.integratedCommit;
  if (next.integratedCommit !== integrated || (operation === "merged" && !sha(integrated))) throw releaseError("Native integrated commit mismatch");
  if (operation === "finish") {
    if (!receipt || receipt.version !== 1 || receipt.jobId !== prior.id || receipt.commit !== prior.integratedCommit || receipt.verificationDigest !== prior.verificationDigest || !Array.isArray(receipt.targets) || !next.receipt || !sameReceipt(receipt, next.receipt)) throw releaseError("Native full receipt mismatch");
  } else if (!same(prior.receipt, next.receipt)) throw releaseError("Native receipt changed unexpectedly");
  return next;
}

// The migration rehearsal container (wi_78521ba68a8ad197). The candidate's own
// hub_compose (scripts/deploy-truenas-hub.py) describes the production hub
// service; each constraint becomes a container argument, and a service this
// table does not fully understand is refused, never run with less. Only the
// candidate binary and the private state directory are mounted: every other
// composed volume (tokens, keys) stays out, so no production path is an
// argument. no-new-privileges has no flag in apple/container; restart and
// ports do not apply to a run that starts no listener. The working directory
// is set to / because apple/container refuses to start uid 950 in the image's
// own (/home/nonroot, another user's 0700 directory); both are unwritable.
const REHEARSAL_DB="hub.sqlite",REHEARSAL_LINES=40,REHEARSAL_LINE_LENGTH=300;
const REHEARSAL_KEYS=["image","user","restart","entrypoint","read_only","cap_drop","security_opt","ports","environment","volumes","mem_limit","cpus"];
const REHEARSAL_HOW=/^(exit \d{1,3}|signal SIG[A-Z0-9]{1,12}|timeout|not started)$/,REHEARSAL_NAME=/^[A-Za-z0-9._\/:@-]{1,160}$/;
const REHEARSAL_COMPOSE="import json,sys,importlib.util as u;sys.path.insert(0,'scripts');s=u.spec_from_file_location('deploy_truenas_hub','scripts/deploy-truenas-hub.py');m=u.module_from_spec(s);s.loader.exec_module(m);d=m._deployment_plan(json.load(open(sys.argv[1])),sys.argv[2]);print(json.dumps(m.hub_compose(d,d['binaryDestination'])['services']['hub']))";
export function rehearsalArgv(service,{runtime,name,binary,stateDir}){
  const s=service,plain=v=>typeof v==="string" && v.length>0 && !/[\0\r\n]/.test(v);
  if(!s || typeof s!=="object" || Array.isArray(s) || Object.keys(s).some(k=>!REHEARSAL_KEYS.includes(k)))throw releaseError("Rehearsal compose unsupported");
  const memory=/^([1-9]\d{0,6})m$/.exec(typeof s.mem_limit==="string"?s.mem_limit:""),cpus=["string","number"].includes(typeof s.cpus) && /^\d+(\.\d+)?$/.test(String(s.cpus))?Math.ceil(Number(s.cpus)):0;
  const env=s.environment && typeof s.environment==="object" && !Array.isArray(s.environment)?Object.entries(s.environment):null;
  const entry=Array.isArray(s.entrypoint) && s.entrypoint.length===1 && /^\/[A-Za-z0-9._\/-]+$/.test(s.entrypoint[0])?s.entrypoint[0]:null;
  const volumes=Array.isArray(s.volumes) && s.volumes.every(plain)?s.volumes:null;
  // Besides the binary and /state, production mounts only read-only files; a
  // second writable mount would be storage the rehearsal does not provide.
  const mounted=volumes && entry && volumes.filter(v=>v.endsWith(`:${entry}:ro`)).length===1 && volumes.filter(v=>v.endsWith(":/state")).length===1 && volumes.every(v=>v.endsWith(":ro") || v.endsWith(":/state"));
  if(!plain(s.image) || !/^[A-Za-z0-9][A-Za-z0-9._\/:@-]*$/.test(s.image) || !/^\d+:\d+$/.test(typeof s.user==="string"?s.user:"") || s.read_only!==true || !same(s.cap_drop,["ALL"]) || !same(s.security_opt,["no-new-privileges:true"])
    || !memory || !(cpus>=1) || !env || env.some(([k,v])=>!/^[A-Za-z_][A-Za-z0-9_]*$/.test(k) || typeof v!=="string" || /[\0\r\n]/.test(v)) || !entry || !mounted
    || ![runtime,name,binary,stateDir].every(plain))throw releaseError("Rehearsal compose unsupported");
  return [runtime,"run","--rm","--progress","none","--name",name,"--read-only","--user",s.user,"--cap-drop","ALL","--memory",memory[1]+"M","--cpus",String(cpus),"--workdir","/",
    ...env.sort(([a],[b])=>a<b?-1:a>b?1:0).flatMap(([k,v])=>["-e",`${k}=${v}`]),"-v",`${binary}:${entry}:ro`,"-v",`${stateDir}:/state`,"--entrypoint",entry,s.image,"--migrate-only","/state/"+REHEARSAL_DB];
}
// How a rehearsal run ended, in childReason's vocabulary (exit 137 is the
// out-of-memory kill), with its last output lines redacted and bounded.
function rehearsalFailure(run,runtime,service){
  const how=run?.error?.code==="ETIMEDOUT"?"timeout":/^SIG[A-Z0-9]{1,12}$/.test(run?.signal||"")?`signal ${run.signal}`:Number.isInteger(run?.status) && run.status>=0 && run.status<1000?`exit ${run.status}`:"not started";
  const tail=(text,n)=>n>0?String(text||"").split(/\r?\n/).filter(l=>l.trim()).slice(-n):[];
  const stderr=tail(run?.stderr,REHEARSAL_LINES),lines=[...stderr,...tail(run?.stdout,REHEARSAL_LINES-stderr.length)];
  const error=releaseError("Rehearsal container "+how);
  error.rehearsal={runtime,image:service.image,exit:how,lines:sanitizeCapture({logs:[{service:"hub",lines}]},Object.values(service.environment)).logs[0].lines};
  return error;
}
// The only shape of a failed rehearsal the private journal keeps; anything
// else on the error is dropped whole.
export function failureRehearsal(error){
  const r=error?.rehearsal;
  if(!r || typeof r!=="object" || ![r.runtime,r.image].every(v=>typeof v==="string" && REHEARSAL_NAME.test(v)) || typeof r.exit!=="string" || !REHEARSAL_HOW.test(r.exit)
    || !Array.isArray(r.lines) || r.lines.length>REHEARSAL_LINES || r.lines.some(l=>typeof l!=="string" || l.length>REHEARSAL_LINE_LENGTH))return null;
  return {runtime:r.runtime,image:r.image,exit:r.exit,lines:[...r.lines]};
}
// The cause a push-failure notice names, built only from re-validated typed
// values; anything else in the cause is dropped, never echoed. The stderr
// itself stays in the private journal and only its sha256 prefix is named.
export function pushCauseText(cause){
  if(!cause || typeof cause!=="object")return "";
  const parts=[],seconds=Number.isSafeInteger(cause.elapsedMs)&&cause.elapsedMs>=0?Math.round(cause.elapsedMs/1000):null;
  if(cause.timedOut===true)parts.push(seconds!==null?`timed out after ${seconds} s`:"timed out");
  else if(Number.isInteger(cause.exitCode)&&cause.exitCode>=1&&cause.exitCode<=255)parts.push(`exit ${cause.exitCode}`);
  else if(typeof cause.signal==="string"&&/^sig[a-z0-9]{1,12}$/.test(cause.signal))parts.push(`signal ${cause.signal}`);
  if(cause.timedOut!==true&&seconds!==null)parts.push(`after ${seconds} s`);
  if(PUSH_REASONS.includes(cause.reason))parts.push(`(${cause.reason})`);
  const said=parts.length?` Cause: ${parts.join(" ")}.`:"";
  const check=["absent","unknown"].includes(cause.remoteCheck)?` Remote check: ${cause.remoteCheck}.`:"";
  const stderr=typeof cause.stderrSHA256==="string"&&/^[a-f0-9]{64}$/.test(cause.stderrSHA256)?` The sanitized stderr is in the private journal (sha256 ${cause.stderrSHA256.slice(0,12)}).`:"";
  return said+check+stderr;
}

export class HostAdapter {
  constructor(config,job){this.config=config;this.job=job;this.artifacts=new Map();this.serial=0;this.now=()=>Date.now();
    // The TailOS poller's fetch and clock; tests replace them.
    this.probeDeps={fetchJSON:hostDeps.fetchJSON,sleep:hostDeps.sleep,now:hostDeps.now};this.probeWaits=new Map();
    // What the last poll saw: a wait on the host list, or a held run.
    this.matrixChildren=MATRIX_CHILDREN;this.matrixWait=null;this.matrixHeld=null;
    // Failed calls of the configured CLI this adapter recorded privately.
    this.cliFailures=[];
    // Progress reports are opt-in (the deployment daemon turns them on) and
    // share one withdraw state across the adapters of a daemon.
    this.progressOn=false;this.progressState={withdrawn:false};this.matrixPid=null;}
  // The runner's own progress, for the hub's hung-runner check: sent between
  // steps and at the end of every poll, never from a timer, so a runner that
  // stops also stops reporting. One try of ten seconds, outside command():
  // no retry, no private failure record, no notice, no transport wait and no
  // terminal line. A report the CLI or the hub rejects (not a transport
  // error: this tt or hub is older than the runner) is followed by one
  // withdraw, in the form a tt from before the command forwards, so the hub
  // judges this run as one that never reported. A withdraw that fails is sent
  // again after the next rejected report, until one succeeds; after that it is
  // withdrawn again only after a later report succeeded.
  progress(record){
    if(this.progressOn!==true || typeof record?.phase!=="string")return;
    const cli=this.config.tt||"tt",call=argv=>execFileSync(cli,argv,{cwd:this.config.cwd,encoding:"utf8",stdio:["ignore","pipe","pipe"],timeout:10000});
    try{
      call(["deployment","progress","--phase",record.phase,...(record.jobId?["--job",record.jobId]:[]),...(record.step?["--step",record.step]:[]),...(record.wait?.kind?["--wait",record.wait.kind]:[]),...(Number.isSafeInteger(record.wait?.pid) && record.wait.pid>0?["--pid",String(record.wait.pid)]:[])]);
      this.progressState.withdrawn=false;
    }catch(error){
      try{
        if(transportCause(error) || this.progressState.withdrawn)return;
        call(["deployment","progress","--request-id",`progress-withdraw-${process.env.TAILTERM_RUN||"run"}`]);
        this.progressState.withdrawn=true;
      }catch{}
    }
  }
  command(argv,cwd=this.config.cwd,{timeout=600000,retry=true}={}){
    if(!Array.isArray(argv)||!argv.length||argv.some(a=>typeof a!=="string"||/[\0\r\n]/.test(a)))throw releaseError("Invalid host operation argv");
    // Only an idempotent call of the configured CLI is made again, and only
    // after a transport error (cliRetry). The error thrown carries transport
    // when the last try ended in one, and its reason names the cause and the
    // number of tries. retry false makes the call once: the daemon's notices
    // during an open transport wait (transportWait).
    const cli=this.config.tt||"tt",began=Date.now();
    for(let tries=1;;tries++){
      try{return execFileSync(argv[0],argv.slice(1),{cwd,encoding:"utf8",stdio:["ignore","pipe","pipe"],maxBuffer:256*1024*1024,timeout:tries===1?timeout:Math.min(timeout,cliRetry.windowMs)});}
      catch(error){
        const cause=argv[0]===cli?transportCause(error):null,failed=new Error("Host operation failed");failed.releaseReason=childReason(argv,error,cli,cause?{cause,tries}:null);
        let kept=null;
        if(argv[0]===cli && this.config.journalDirectory){try{kept=recordCliFailure(this.config,this.job,argv,cwd,error,failed.releaseReason,this.now());}catch{}}
        const wait=cliRetry.delaysMs[tries-1];
        if(cause && retry && cliIdempotent(argv) && tries<cliRetry.tries && Number.isFinite(wait) && Date.now()-began+wait<=cliRetry.windowMs){cliRetry.sleep(wait);continue;}
        if(cause)failed.transport=true;
        if(argv[0]===cli)failed.refusal=refusalText(error.stderr);
        if(argv[0]===cli && argv[1]==="deployment" && generationChanged(error))failed.generationChanged=true;
        if(argv[0]===cli && argv[1]==="deployment" && ((argv[2]==="refuse" && argv.includes("--reason")) || (argv[2]==="block" && argv.includes("--revert-outcome"))) && refusalRefused(error))failed.refusalRefused=true;
        if(kept)this.cliFailures.push(cause?{...kept,transport:true}:kept);
        throw failed;
      }
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
  // "generation-changed" when the hub refused the check only because the job's
  // generation moved; runRelease then decides whether to re-read (adoptGeneration).
  // A check that could not reach the hub after its tries is thrown as it is:
  // runRelease lets an unpublished job wait on it, and after publication the
  // job's failure names that cause instead of a lost fence.
  async fence(){try{this.native("check");return true;}catch(error){if(error?.transport===true)throw error;return error?.generationChanged===true?"generation-changed":false;}}
  // One re-read after a "generation changed" answer. detail() refuses a job
  // that is no longer this exact claim (agent and run), whose pause generation,
  // immutable pins, integrated commit or input pins differ, or that left
  // claimed/merged. Only a still claimed job at a later generation is adopted;
  // anything else, or a failed read, is a lost fence. A read that could not
  // reach the hub after its tries is thrown like an unreached check, so an
  // unpublished job waits on it (runRelease re-reads only before publication).
  async adoptGeneration(){
    try{
      const current=this.detail(this.job.id);
      if(!this.job.agentId || !this.job.runId || current.state!=="claimed" || current.generation<=this.job.generation)return false;
      this.job=current;return true;
    }catch(error){if(error?.transport===true)throw error;return false;}
  }
  // The project handler by the project rule (hub operation "handler"): a
  // finished entry holds no item lease, so role addressing is refused there.
  handler(){const h=JSON.parse(this.command([this.config.tt||"tt","deployment","handler"]));if(!/^agt_[a-f0-9]+$/.test(h?.id||""))throw releaseError("Project database handler required");return h.id;}
  // The project's release quiet window as the hub reads it now (tt deployment
  // window): null when none is set, else {open,until,zone,start,end,targets}.
  // Output of any other shape is unreadable; a failed call throws its own
  // reason (a transport error after its tries).
  async releaseWindow(){return parseReleaseWindow(this.command([this.config.tt||"tt","deployment","window"]));}
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
    // This read and the import request below are repeated at every poll, so
    // a transport failure that outlasted its tries leaves the job waiting.
    let current;try{current=this.detail(job.id);}catch(error){if(error?.transport===true)return false;throw error;}
    if(current?.integratedCommit===job.integratedCommit && current.integratedVerification?.commit===job.integratedCommit){this.job=current;return true;}
    // One directory per integrated commit and requeue attempt: a handler
    // requeue (a new reconciliation) never reuses an earlier attempt's host
    // wait or receipt, while a runner resumed within an attempt does.
    if(!sha(job.integratedCommit))throw releaseError("Exact integrated commit required");
    const dir=join(this.config.journalDirectory,job.id+"-integrated-verification",`${job.integratedCommit}-r${job.reconciliations?.length||0}`);mkdirSync(dir,{recursive:true,mode:0o700});
    const contextPath=join(dir,"context.json"),planPath=join(dir,"plan.json"),receiptPath=join(dir,"receipt.json");
    this.matrixWait=null;this.matrixHeld=null;
    let run=this.readRun(dir);
    this.matrixPid=run && run.state!=="ended" && Number.isSafeInteger(run.pid)?run.pid:null;
    if(!run && !existsSync(receiptPath))return this.launchMatrixRun(job,current,dir,{contextPath,planPath});
    if(run && run.state!=="ended"){
      if(this.resolveMatrixRun(dir,run)!=="ended")return false;
      run=this.readRun(dir);
    }
    if(run?.refusal)throw releaseError(run.refusal);
    if(!existsSync(receiptPath))throw releaseError("Integrated matrix run ended without a receipt");
    // The receipt is read only once its run is gone, and imported only when
    // eligible. A refusal names its failed checks for a batch's attribution,
    // and marks the home cleanup error without copying its text.
    const receipt=JSON.parse(readFileSync(receiptPath,"utf8"));
    if(!receiptEligible(receipt))throw Object.assign(releaseError("Integrated matrix receipt is not eligible"),failedChecks(receipt),receipt.environment?.VERIFICATION_HOME_CLEANUP_ERROR?{homeCleanupError:true}:{});
    try{this.command([this.config.tt||"tt","send","--kind","request","--to",this.handler(),"--subject","Import verification for the integrated release commit","--ask",`Import release verification plan and receipt for job ${job.id} integrated commit ${job.integratedCommit} through tt deployment verification --plan-file and --file. Preserve exact job generation and inspect logs; release publication waits for saved import.`,"--request-id",`${job.id}-integrated-matrix-${job.integratedCommit}`,"--ref",`release-job=${job.id}`,"--ref",`integrated-commit=${job.integratedCommit}`,"--attachment",planPath,"--attachment",receiptPath]);}
    catch(error){if(error?.transport===true)return false;throw error;}
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
    this.matrixPid=pid;
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
  // One pid only, as ISO UTC; compared by startMatch and ordered by startedBy.
  processStartTime(pid){
    return processStartTime(pid);
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
  // wait-expired, withdrawn, or a failure while waiting), with the pid and
  // join time it recorded; null until then.
  matrixSidecar(dir){
    try{const record=JSON.parse(readFileSync(join(dir,"host-lock.json"),"utf8")),outcome=record?.outcome;return typeof outcome==="string" && !["waiting","holding"].includes(outcome)?{outcome,pid:record.pid,requestedAt:record.requestedAt}:null;}catch{return null;}
  }
  // One non-blocking step for an attempt that is not ended: "waiting",
  // "held" or "ended" (saved). With stop, the run is stopped now instead of at
  // its deadline (an earlier attempt of a requeued job). A pid is signalled
  // only with process-instance proof: this process holds the live child, or
  // the pid's process start time is the one recorded at launch. A matching
  // pid and directory in the lock file is not proof. Check groups are never
  // signalled. A pid whose start time differs from the recorded one now
  // names another process, so the run is gone. Held means the run could not
  // be confirmed stopped; the job then keeps the fence until it can.
  resolveMatrixRun(dir,run,stop=false){
    const now=this.now(),child=this.matrixChildren.get(dir),found=this.matrixEntry(dir,run),sidecar=this.matrixSidecar(dir);
    const persist=()=>{try{this.saveRun(dir,run);return true;}catch{return false;}};
    const alive=()=>[...new Set([...(run.snapshot?.groups||[]),...(run.groups||[])])].filter(g=>!this.groupGone(g));
    const held=(reason,pid=run.pid)=>{persist();this.matrixHeld={attempt:basename(dir),pid:pid??null,groups:run.state==="started"?alive():[],reason};return "held";};
    const end=(reason,refusal)=>{Object.assign(run,{state:"ended",reason,endedAt:now,...(refusal?{refusal}:{})});if(!persist())return "waiting";this.matrixChildren.delete(dir);return "ended";};
    // A run that was not stopped and left a receipt ended normally, whatever
    // the record knew of it; the receipt's eligibility is checked at import.
    const ending=()=>run.stopRequestedAt?end("stopped","Integrated matrix run exceeded its bound"):existsSync(join(dir,"receipt.json"))?end("receipt"):sidecar?.outcome==="wait-expired"?end("wait-expired","Verification host wait expired"):end("no-receipt","Integrated matrix run ended without a receipt");
    if(found?.ambiguous)return held("host lock identity ambiguous");
    if(found?.entry)run.leaseId ||= found.entry.id;
    if(run.state==="unreadable"){this.matrixHeld={attempt:basename(dir),pid:null,groups:[],reason:"attempt record unreadable"};return "held";}
    if(run.state==="starting"){
      // An unconfirmed launch: adopt the run from this process's child or
      // from the lock file. No timer refuses it.
      const pid=child?.pid??found?.entry.pid;
      if(Number.isSafeInteger(pid) && pid>0){
        // A lock-file pid is ours only if that process began before it joined.
        const started=this.processStartTime(pid),ours=child || startedBy(started,found.entry.requestedAt,1000);
        Object.assign(run,{state:"started",pid,processStartedAt:ours?started:null,groups:[]});
        if(!persist())return "waiting";
      }
      else if(sidecar){
        // Leaving the list is not proof the run exited: it releases before
        // its interruption path removes its receipt. Its own pid decides: gone
        // or begun after it joined (reused) ends it; begun by then is adopted.
        const own=sidecar.pid;
        if(!(Number.isSafeInteger(own) && own>1))return held("matrix run sidecar names no pid");
        if(this.pidGone(own))return ending();
        const started=this.processStartTime(own);
        if(startKey(started)===null || !Number.isFinite(Date.parse(sidecar.requestedAt)))return held("no process-instance proof for the matrix run sidecar pid",own);
        if(!startedBy(started,sidecar.requestedAt,1000))return ending();
        Object.assign(run,{state:"started",pid:own,processStartedAt:started,groups:[]});
        if(!persist())return "waiting";
      }
      else if(!stop && now-run.launchedAt<(this.config.matrixLaunchGraceMs??MATRIX_LAUNCH_GRACE_MS))return "waiting";
      else return held("matrix run launch unconfirmed");
    }
    const mine=found && found.entry.pid===run.pid?found:null;
    if(mine?.role==="holder")run.groups=[...new Set([...(run.groups||[]),...(mine.entry.groups||[]).filter(Number.isSafeInteger)])];
    const reused=()=>startKey(run.processStartedAt)!==null && startMatch(run.processStartedAt,this.processStartTime(run.pid))==="other";
    const exited=child?child.exitCode!==null||child.signalCode!==null:this.pidGone(run.pid)||reused();
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
      if(!(run.pid>1) || (!child && startMatch(run.processStartedAt,this.processStartTime(run.pid))!=="same"))return held("no process-instance proof for the matrix run pid");
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
    // The read and the request are repeated at every poll under one request
    // id, so a transport failure that outlasted its tries leaves the job
    // waiting for the next poll.
    let current;try{current=this.detail(this.job.id);}catch(error){if(error?.transport===true)return false;throw error;}
    if(current?.inputsCommit===commit && /^[a-f0-9]{64}$/.test(current.inputsDigest||"")){this.job=current;this.jobInputs(commit);return true;}
    try{this.command([this.config.tt||"tt","send","--kind","request","--to",this.handler(),"--subject","Import immutable inputs for this release job","--ask",`Prepare the private manifest ${join(this.config.journalDirectory,`${this.job.id}-g${current.generation}-inputs.json`)} for exact job ${this.job.id} accepted ${this.job.commit} integrated ${commit}; import its digest with tt deployment inputs --job --generation --commit --file. Include fresh exact-job backup/preflight pins and rollback programs; publication waits for saved handler input binding.`,"--request-id",`${this.job.id}-g${current.generation}-inputs-${commit}`,"--work-item",this.job.itemId,"--work-item-revision",String(this.job.itemRevision),"--work-order-message",String(this.job.orderMessageSeq),"--ref",`release-job=${this.job.id}`]);}
    catch(error){if(error?.transport!==true)throw error;}
    return false;
  }
  // The manifest the handler bound is the generation-named file whose bytes
  // hash to the hub's digest. The hub does not keep the generation it was
  // imported at and this run's own is later, so the name is found, not built.
  // The file must record its name's generation, which is below the job's, and
  // this attempt (the count of set-asides and requeues). ID-inputs.json, the
  // name before names carried a generation, is never used as the manifest; it
  // only gets its own refusal.
  jobInputs(commit){
    if(this.job.inputsCommit!==commit)throw releaseError("Handler input digest binding required");
    const dir=this.config.journalDirectory,keyed=[];
    for(const name of readdirSync(dir)){
      const m=/^(.+)-g([1-9][0-9]*)-inputs\.json$/.exec(name);
      if(m && m[1]===this.job.id)keyed.push({path:join(dir,name),generation:Number(m[2])});
    }
    const bound=path=>{try{return lstatSync(path).isFile() && fileDigest(path)===this.job.inputsDigest;}catch{return false;}};
    const other=()=>releaseError("Input manifest bound to another generation");
    const found=keyed.sort((a,b)=>b.generation-a.generation).find(f=>bound(f.path));
    if(!found){
      // No file hashes to the digest: a bound manifest whose bytes changed and
      // one never written look the same, as the hub keeps no generation.
      if(bound(join(dir,this.job.id+"-inputs.json")))throw releaseError("Input manifest has no generation in its name");
      throw keyed.length?releaseError("Input manifest digest mismatch"):releaseError("Handler input digest binding required");
    }
    const input=JSON.parse(readFileSync(found.path,"utf8"));
    if(input.generation!==found.generation || !(input.generation<this.job.generation) || input.attempt!==(this.job.reconciliations?.length||0))throw other();
    if(input.version!==1 || input.jobId!==this.job.id || input.commit!==commit || input.acceptedCommit!==this.job.commit || input.verificationDigest!==this.job.verificationDigest)throw releaseError("Exact job input binding required");
    // Two TrueNAS plans made before either deploy would each pin the other's
    // pre-release mount, so hub and bridge together must share one plan.
    const {hub,bridge}=input.targets||{};
    // A lone entry that names the paired plan would otherwise be refused only
    // in the target loop, after publication.
    if(!hub!==!bridge && same((hub||bridge).planTargets,PAIR))throw releaseError("Paired plan partner entry required");
    if(hub && bridge && (!same(hub.planTargets,PAIR) || !same(bridge.planTargets,PAIR) || ["release","planPath","preflightReceipt","preflightReceiptSHA256","backup","backupSHA256"].some(k=>hub[k]!==bridge[k])))throw releaseError("Hub and bridge need one paired plan");
    return input;
  }
  // What a job's backup destination must contain: the job and its generation.
  backupKey(input){return `${this.job.id}-g${input.generation}-`;}
  captureMiniRollback(artifact){
    const path=join(this.config.journalDirectory,this.job.id+"-mini-before");
    copyFileSync(artifact.installPath,path,constants.COPYFILE_EXCL);
    artifact.rollbackPath=path;artifact.priorArtifactSHA256=fileDigest(path);artifact.rollbackCaptured=true;
  }
  // w2: before publication, install the pinned wrangler when the lockfile
  // changed (once per commit; prepare does not repeat it) and refuse the job
  // when the exact pinned version is not installed.
  async releaseTools(commit){
    if(git(this.config.cwd,"rev-parse","HEAD")!==commit)throw releaseError("Candidate build checkout mismatch");
    const baselines=this.baselines||this.config.baselines;
    if(this.installedFor!==commit && diffPaths(this.config.cwd,baselines.tailos,commit).includes("package-lock.json"))this.command(["npm","ci"]);
    this.installedFor=commit;
    if(!pinnedWrangler(this.config.cwd))throw releaseError("Pinned wrangler not installed; run npm ci");
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
      if(this.installedFor!==commit && diffPaths(this.config.cwd,baselines.tailos,commit).includes("package-lock.json"))this.command(["npm","ci"]);
      this.command(["npm","run","build:static"]);this.command(["npm","run","verify:release"]);
      // A resumed run skips the pre-publication check; the deploy needs it.
      if(!pinnedWrangler(this.config.cwd))throw releaseError("Pinned wrangler not installed; run npm ci");
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
      if(perJob.backupJobId!==this.job.id || !perJob.backup || !perJob.backup.includes(this.backupKey(input)))throw releaseError("Fresh job backup identity required");
      artifact.migrationBinary=join(this.config.journalDirectory,this.job.id+"-migration");
      // Built for Linux at the release host's own architecture: it only ever
      // runs inside the rehearsal container, never on the host.
      withBuildCheckout(this.config.cwd,commit,src=>this.command(["env","CGO_ENABLED=0","GOOS=linux",`GOARCH=${process.arch==="arm64"?"arm64":"amd64"}`,"go","build","-trimpath","-o",artifact.migrationBinary,"./cmd/tailterm-hub"],join(src,"hub")));
      this.requireStamp(artifact.migrationBinary,commit);
      artifact.migrationBinarySHA256=fileDigest(artifact.migrationBinary);
    }
    if(["hub","bridge"].includes(target)){
      if(perJob.backupJobId!==this.job.id || !perJob.backup?.includes(this.backupKey(input)))throw releaseError("Fresh job backup identity required");
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
  // Runs the container CLI and keeps its output. It never throws: the caller
  // reads status, signal and error. Tests replace it.
  container(argv,{timeout=600000}={}){
    const r=spawnSync(argv[0],argv.slice(1),{cwd:this.config.cwd,encoding:"utf8",stdio:["ignore","pipe","pipe"],maxBuffer:64*1024*1024,timeout});
    return {status:r.status,signal:r.signal,error:r.error,stdout:r.stdout||"",stderr:r.stderr||""};
  }
  // The production hub service as the integrated checkout composes it for
  // this job's plan and release.
  rehearsalSpec(a){
    if(typeof a.planPath!=="string" || typeof a.release!=="string")throw releaseError("Rehearsal plan required");
    let service;try{service=JSON.parse(this.command(["python3","-c",REHEARSAL_COMPOSE,a.planPath,a.release]));}catch(error){if(error?.releaseReason)throw error;throw releaseError("Rehearsal compose unsupported");}
    return service;
  }
  // The candidate migration runs only in a container under the production
  // hub constraints. Nothing here runs the migration binary on the host: with
  // no usable runtime the release is refused.
  async rehearse(a){
    if(!a.backupCopy || !a.migrationBinary)throw releaseError("Handler backup-copy import required");
    if(fileDigest(a.backupCopy)!==a.backupSHA256)throw releaseError("Imported backup hash mismatch");
    // The migrated copy is hub.sqlite in a private state directory mounted as
    // /state; the directory is removed when the rehearsal ends, pass or fail.
    // The marker beside it, not the copy, refuses a second attempt; it is
    // saved before the copy exists and holds no path, output or error text.
    const base=a.backupCopy+".rehearsal-"+this.job.id,stateDir=base+".state",marker=base+".json";
    if(existsSync(marker))throw releaseError("Rehearsal already attempted; inspect prior attempt");
    // Runtime and compose refusals come before the marker, so the same job
    // can be rehearsed once the host is fixed.
    const runtime=this.config.rehearsal?.runtime??"container";
    if(typeof runtime!=="string" || !REHEARSAL_NAME.test(runtime))throw releaseError("Invalid rehearsal runtime");
    const up=this.container([runtime,"system","status"],{timeout:30000});
    if(up?.error)throw releaseError("Rehearsal container runtime unavailable");
    if(up?.status!==0)throw releaseError("Rehearsal container runtime not running");
    const service=this.rehearsalSpec(a),name="tt-rehearsal-"+this.job.id,argv=rehearsalArgv(service,{runtime,name,binary:a.migrationBinary,stateDir});
    const record={version:1,jobId:this.job.id,backupSHA256:a.backupSHA256,startedAt:new Date(this.now()).toISOString(),outcome:"started"};
    save(marker,record);
    const clear=()=>{try{rmSync(stateDir,{recursive:true,force:true});return !existsSync(stateDir);}catch{return false;}};
    let outcome="failed";
    try{
      // A directory an earlier run left behind is replaced, sidecars included.
      clear();mkdirSync(stateDir,{mode:0o700});copyFileSync(a.backupCopy,join(stateDir,REHEARSAL_DB));
      if(fileDigest(a.migrationBinary)!==a.migrationBinarySHA256)throw releaseError("Candidate migration binary changed");
      const run=this.container(argv);
      if(run?.status!==0 || run.signal || run.error){
        // A run that did not end by itself may have left its container; the
        // removal is best effort and never changes the result.
        if(!Number.isInteger(run?.status)){try{this.container([runtime,"delete","-f",name],{timeout:30000});}catch{}}
        throw rehearsalFailure(run,runtime,service);
      }
      outcome="passed";
    }finally{
      // Cleanup never changes the rehearsal's result; a directory left here
      // is removed by the retention sweep once the job is terminal.
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
      const bin=pinnedWrangler(this.config.cwd);if(!bin)throw releaseError("Pinned wrangler not installed; run npm ci");
      const output=this.command([bin,"pages","deploy","dist-static","--project-name","tailos","--branch","main","--commit-hash",a.commit,"--commit-dirty=false"]);
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
    }else{
      // A program recorded with the pinned binary needs it installed; one an
      // older inputs script recorded (npx wrangler) runs as recorded.
      if(a.rollbackProgram?.[0]===wranglerBin(this.config.cwd) && !pinnedWrangler(this.config.cwd))throw releaseError("Pinned wrangler not installed; run npm ci");
      this.command(a.rollbackProgram);
    }
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
  // The block of a job that failed with no receipt (wi_816228d539a80c48).
  // After publication it carries the journaled tasks-hub revert as flags, and
  // the hub saves it on the job; with none it is the bare block, as before. A
  // block whose revert an older hub or tt refused (refusalRefused) is sent once
  // more bare: same job, generation and request ID, as refuse does. A block
  // the hub refused only because the generation moved is re-read once, and
  // sent again only when the hub saved merged under this exact claim at the
  // runner's integrated commit (the merged reply was lost). A block only stops
  // the job; nothing here resumes deploying. Any other failure is thrown.
  async block(details={}){
    const v=details.revert,flags=v?["--revert-outcome",v.outcome,...(v.commit?["--revert-commit",v.commit]:[]),...(v.bugRequestId?["--revert-bug-request",v.bugRequestId]:[])]:[];
    const send=()=>{
      if(!flags.length)return this.native("block");
      try{return this.native("block",flags);}
      catch(error){if(error?.refusalRefused!==true)throw error;return this.native("block");}
    };
    try{return send();}
    catch(error){if(error?.generationChanged!==true || !this.adoptMerged(details.integrated))throw error;return send();}
  }
  // The one re-read of a block: detail() refuses a job that is no longer this
  // exact claim or its pins; only a published merged job at the given
  // integrated commit and a later generation is adopted. A failed read adopts
  // nothing.
  adoptMerged(integrated){
    let current;
    try{current=this.detail(this.job.id);}catch{return false;}
    if(!this.job.agentId || !this.job.runId || !sha(integrated) || current.state!=="merged" || current.published!==true || current.integratedCommit!==integrated || current.generation<=this.job.generation)return false;
    this.job=current;return true;
  }
  // A refuse whose refusal an older hub or tt refused (refusalRefused) is
  // sent once more bare, the exact call of a runner before refusals were kept:
  // same job, generation and request ID. Both accept the bare form, and a hub
  // that saved it under this request ID replays it; the job is then refused
  // without a reason. Any other failure is thrown as before.
  async refuse(refusal){
    if(!refusal)return this.native("refuse");
    try{return this.native("refuse",["--reason",refusal.reason,...(refusal.step?["--step",refusal.step]:[]),...(refusal.target?["--target",refusal.target]:[])]);}
    catch(error){if(error?.refusalRefused!==true)throw error;return this.native("refuse");}
  }
  async escalate(details={}){
    // Only a re-validated 40-hex commit and whole seconds reach the text. A
    // live check that passed is not a cause of the failure and is left out.
    const waits=(Array.isArray(details.probeWaits)?details.probeWaits:[]).filter(w=>w?.target==="tailos" && ["live","rollback"].includes(w.probe) && !(w.probe==="live" && w.passed===true)).map(w=>` TailOS ${w.probe==="live"?"live check":"rollback probe"} last saw ${sha(w.lastCommit)?w.lastCommit:"no readable release.json"}${Number.isSafeInteger(w.waitedMs)&&w.waitedMs>=0?` after ${Math.round(w.waitedMs/1000)} s`:""}.`).join("");
    if(details.outcome==="refused"){
      const reason=typeof details.reason==="string"&&REASON.test(details.reason)?details.reason:"unclassified";
      return this.command([this.config.tt||"tt","send","--kind","notice","--subject","Release refused before publication","--text",`Release ${this.job.id} was refused before publication; nothing was published or deployed. Reason: ${reason}.${reason.startsWith("integration-conflict")?" Next: re-apply the candidate on the current tasks-hub through a follow-through item and re-review the new commit.":""} Handler reconciliation required.`,"--request-id",`${this.job.id}-release-failure`,"--ref",`release-job=${this.job.id}`,...wakeRef(WAKE.attention)]);
    }
    if(details.push==="failed")return this.command([this.config.tt||"tt","send","--kind","notice","--subject","Release is live but the tasks-hub push failed","--text",`Release ${this.job.id} is live and verified, but the fast-forward push of tasks-hub to origin failed.${pushCauseText(details.pushCause)} Live targets were not rolled back; inspect the remote and push tasks-hub by hand.`,"--request-id",`${this.job.id}-push-failure`,"--ref",`release-job=${this.job.id}`,...wakeRef(WAKE.attention)]);
    return this.command([this.config.tt||"tt","send","--kind","notice","--subject","Release failed and requires recovery","--text",`Release failed for ${this.job.id}; inspect the private host journal. Automatic rollback attempted once; handler reconciliation required.${details.revert==="failed"?" The tasks-hub revert failed, so the rolled-back change is still on tasks-hub.":""}${details.rollbackBlocked===true&&details.revert==="committed"?" tasks-hub was reverted, but at least one target could not be rolled back and still runs the released code; roll it back by hand before the next release.":""}${waits}`,"--request-id",`${this.job.id}-release-failure`,"--ref",`release-job=${this.job.id}`,...wakeRef(WAKE.attention)]);
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
  const files=Object.fromEntries(RUNNER_CODE_FILES.map(n=>[n,fileDigest(join(directory,n))]));
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
  return commitCode(cwd,commit);
}
// The watched files as committed at commit, read from its blobs.
export function commitCode(cwd,commit){
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
// digest means the re-exec did not change what this process loads. held is
// the id of the runner's sole owned job when that job is held (heldRestartJob):
// a stale runner that is not idle restarts with it, and heldJob names it.
export const CODE_REASONS=["loaded-unreadable","published-unreadable","restart-did-not-refresh","checkout-dirty","checkout-failed","disk-mismatch","exec-failed","load-failed"];
export function codeDecision({loaded,published,idle,marker,held}){
  if(!validCode(loaded))return {state:"refused",reason:"loaded-unreadable"};
  if(!validCode(published) || !sha(published.commit))return {state:"refused",reason:"published-unreadable"};
  if(loaded.digest===published.digest)return {state:"current"};
  const heldJob=idle!==true && typeof held==="string" && held?held:null;
  if(idle!==true && !heldJob)return {state:"draining"};
  if(marker===published.digest)return {state:"refused",reason:"restart-did-not-refresh"};
  return {state:"restart",...(heldJob?{heldJob}:{})};
}
// A journal phase that makes no progress when its claim is run again: a
// refusal that is not finished, or one runRelease rejects as ambiguous.
const HELD_PHASES=["prepared","integrated","refusing"];
// The one job a stale runner may restart with, or null. The ledger's only
// claimed, merged or blocked job is claimed by this agent and run; the hub
// shows nothing published, no receipt and no inputs binding unless the
// journal is refusing (wi_56ac00ba2854fa3d c4); its journal is
// readable, of exactly this claim, in a held phase, with no effects and not
// published, and is not one a requeue lets runRelease archive and start again;
// no matrix run of the job is unsettled, this process has no matrix child and
// no host release lock exists. Null on any doubt.
export function heldRestartJob({jobs,details,agent,run,journalDirectory,lock,matrixChildren}){
  try{
    const active=jobs.filter(j=>["claimed","merged","blocked"].includes(j.state));
    if(active.length!==1 || !agent || !run)return null;
    const job=details.get(active[0].id);
    if(!job || job.id!==active[0].id || active[0].state!=="claimed" || job.state!=="claimed" || job.agentId!==agent || job.runId!==run || !sha(job.commit))return null;
    if(job.published===true || job.receipt)return null;
    if(matrixChildren!==0 || existsSync(lock) || matrixRunUnsettled(journalDirectory,job))return null;
    const path=join(journalDirectory,job.id+".json"),prior=JSON.parse(readFileSync(path,"utf8")),recovery=job.reconciliations?.at(-1);
    if(prior?.jobId!==job.id || prior.commit!==job.commit || prior.agentId!==agent || prior.runId!==run || (prior.taskId!==undefined && prior.taskId!==job.taskId) || (prior.pauseGeneration!==undefined && prior.pauseGeneration!==job.pauseGeneration))return null;
    if(!HELD_PHASES.includes(prior.phase) || !Array.isArray(prior.effects) || prior.effects.length || prior.published!==undefined && prior.published!==false)return null;
    if(recovery?.disposition==="requeue" && recovery.journalDigest===fileDigest(path))return null;
    // Bound release inputs are held only by a refusal, which runRelease
    // finishes whatever the inputs; a prepared or integrated job keeps them.
    if(job.inputsDigest && prior.phase!=="refusing")return null;
    return job.id;
  }catch{return null;}
}
// Whether a runner that could not read the job list is idle by its own
// journals (wi_56ac00ba2854fa3d c1): no journal of this agent and run in a
// non-final phase, no matrix child of this process, and no host release lock
// at the lock path of any journal's task or at the last ledger's lock path.
// Archived journals and input manifests are not journals here. False on any
// doubt, an unreadable journal included.
const FINAL_PHASES=["complete","refused"];
export function journalIdle({journalDirectory,cwd,agent,run,lastLock,matrixChildren}){
  try{
    if(matrixChildren!==0 || !agent || !run)return false;
    const locks=new Set(lastLock?[lastLock]:[]);
    for(const name of readdirSync(journalDirectory)){
      if(!/^rel_[A-Za-z0-9_-]{1,64}\.json$/.test(name) || name.endsWith("-inputs.json"))continue;
      const journal=JSON.parse(readFileSync(join(journalDirectory,name),"utf8"));
      if(!journal || typeof journal!=="object")return false;
      locks.add(hostLockPath(cwd,{taskId:journal.taskId}));
      if(journal.agentId===agent && journal.runId===run && !FINAL_PHASES.includes(journal.phase))return false;
    }
    return ![...locks].some(lock=>existsSync(lock));
  }catch{return false;}
}
// Journal phases a clean stop may unclaim: the claim waits for the handler's
// own step (the inputs binding or the integrated matrix import) or for the
// release quiet window to end, and has done nothing to the checkout's remote,
// targets or hub record.
const UNCLAIM_PHASES=["waiting_inputs","waiting_matrix","waiting_window"];
// The one job a cleanly stopping runner may give back with tt deployment
// unclaim (wi_27745813129a396b), or null. As heldRestartJob, the ledger's only
// claimed, merged or blocked job is claimed by this agent and run with no
// publication, receipt or inputs binding, and no matrix run, matrix child or
// host release lock is live; its journal is of exactly this claim, waits for
// inputs or a matrix import with no effects, and was never published, sent as
// merged or part of a batch. Null on any doubt.
export function unclaimableJob({jobs,details,agent,run,journalDirectory,lock,matrixChildren}){
  try{
    const active=jobs.filter(j=>["claimed","merged","blocked"].includes(j.state));
    if(active.length!==1 || !agent || !run)return null;
    const job=details.get(active[0].id);
    if(!job || job.id!==active[0].id || active[0].state!=="claimed" || job.state!=="claimed" || job.agentId!==agent || job.runId!==run || !sha(job.commit))return null;
    if(job.published===true || job.receipt || job.inputsDigest || job.batch)return null;
    if(matrixChildren!==0 || existsSync(lock) || matrixRunUnsettled(journalDirectory,job))return null;
    const prior=JSON.parse(readFileSync(join(journalDirectory,job.id+".json"),"utf8"));
    if(prior?.jobId!==job.id || prior.commit!==job.commit || prior.agentId!==agent || prior.runId!==run || (prior.taskId!==undefined && prior.taskId!==job.taskId) || (prior.pauseGeneration!==undefined && prior.pauseGeneration!==job.pauseGeneration))return null;
    if(!UNCLAIM_PHASES.includes(prior.phase) || !Array.isArray(prior.effects) || prior.effects.length || prior.published!==undefined && prior.published!==false || prior.mergedSent===true || prior.batch!==undefined)return null;
    return job.id;
  }catch{return null;}
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
// Puts a checkout prepareCode moved back on the commit this process loaded
// from, after the published runner did not load (wi_f6bd907b134a9331 s6), so
// re-provisioning from the checkout starts a runner that loads. The checkout
// is moved only when it is clean and that commit's watched blobs are the
// loaded bytes, and counts as restored only when HEAD is then that commit,
// the checkout is clean and the files on disk are the loaded bytes; throws
// otherwise.
export function restoreCode(cwd,head,loaded){
  if(!sha(head) || !validCode(loaded) || git(cwd,"status","--porcelain") || commitCode(cwd,head).digest!==loaded.digest)throw releaseError("not-restored");
  if(git(cwd,"rev-parse","HEAD")!==head)git(cwd,"checkout","--detach",head);
  if(git(cwd,"rev-parse","HEAD")!==head || git(cwd,"status","--porcelain") || diskCode(join(cwd,"scripts")).digest!==loaded.digest)throw releaseError("not-restored");
}
// Whether the published runner in the checkout loads (wi_56ac00ba2854fa3d
// c2): a child imports scripts/release-runner.mjs, and with it every watched
// file, within timeoutMs. No path is passed as an argument, so no daemon
// entry's argv guard matches and nothing runs but the imports. True only on
// exit 0. A stop signal kills the child.
export function runnerLoads(cwd,{timeoutMs=20000,signal}={}){
  const env=Object.fromEntries(Object.entries(process.env).filter(([k])=>!k.startsWith("TAILTERM_RUNNER_REEXEC")));
  const url=pathToFileURL(join(cwd,"scripts","release-runner.mjs")).href;
  return new Promise(done=>{
    try{
      const child=spawn(process.execPath,["--input-type=module","-e",`await import(${JSON.stringify(url)});process.exit(0);`],{cwd,env,stdio:"ignore",timeout:timeoutMs,killSignal:"SIGKILL",...(signal?{signal}:{})});
      child.once("error",()=>done(false));child.once("exit",(code,signal)=>done(code===0 && !signal));
    }catch{done(false);}
  });
}
// The gate serveDeployment consults before every claim; only the daemon entry
// builds it, and tests replace its parts. exec replaces this process image
// (same pid, parent, agent and run) and returns only in tests, or when a stop
// signal arrived first: the new image would not know it was told to stop.
// noticed and said hold what this process already posted and printed. loads
// is the published runner's load check (runnerLoads) after prepare; restore
// (restoreCode) puts the checkout back on loadedHead, the HEAD the first poll
// of this process found, when that check fails. restartGeneration is which
// restart of this run made this process (1 for the first), from the re-exec
// environment; a restart begun by a runner that passed none counts as the
// first (wi_56ac00ba2854fa3d c5).
const stopSignalTurn=()=>new Promise(done=>setImmediate(()=>setImmediate(done)));
const generationOf=v=>Number.isSafeInteger(v) && v>=1 && v<=999999?v:0;
export function runnerCodeGate(over={}){
  const from=process.env.TAILTERM_RUNNER_REEXEC_FROM,gen=process.env.TAILTERM_RUNNER_REEXEC_GEN,generation=/^[1-9][0-9]{0,5}$/.test(gen||"")?Number(gen):0;
  return {loaded:LOADED_CODE,marker:process.env.TAILTERM_RUNNER_REEXEC||null,restartedFrom:HEX64.test(from||"")?from:null,restartGeneration:generation||(HEX64.test(from||"")?1:null),
    published:publishedCode,prepare:prepareCode,restore:restoreCode,now:()=>Date.now(),execve:(file,argv,env)=>process.execve(file,argv,env),
    loadTimeoutMs:20000,loads(cwd,signal){return runnerLoads(cwd,{timeoutMs:this.loadTimeoutMs,signal});},
    async exec(published,signal,generation){
      // Whatever this process printed reaches a piped terminal before it is replaced.
      await new Promise(done=>{setTimeout(done,1000).unref();process.stderr.write("",()=>done());});
      // A signal that arrived during this poll's synchronous work is handled
      // only once the event loop turns, so it is given that turn.
      await stopSignalTurn();if(signal?.aborted)return;
      this.execve(process.execPath,[process.execPath,...process.execArgv,...process.argv.slice(1)],{...process.env,TAILTERM_RUNNER_REEXEC:published.digest,TAILTERM_RUNNER_REEXEC_FROM:this.loaded.digest,TAILTERM_RUNNER_REEXEC_GEN:String(generation)});
    },
    noticed:new Set(),said:new Set(),...over};
}
// The restart generation the last runner-code.json of this agent and run
// recorded, or 0.
function recordedGeneration(prior){
  return prior && prior.agentId===(process.env.TAILTERM_AGENT||null) && prior.runId===(process.env.TAILTERM_RUN||null)?generationOf(prior.restartGeneration):0;
}
function readCodeRecord(config){try{return JSON.parse(readFileSync(join(config.journalDirectory,"runner-code.json"),"utf8"));}catch{return null;}}
// The generation of a restart onto published: one more than any this run
// reached, kept for that published code so a retry of the same restart in
// this process repeats it.
export function restartGeneration(config,code,published){
  if(code.restartFor?.digest!==published.digest)code.restartFor={digest:published.digest,generation:Math.max(generationOf(code.restartGeneration),recordedGeneration(readCodeRecord(config)))+1};
  return code.restartFor.generation;
}
// JOURNAL/runner-code.json (0600): the code this process loaded and the
// published code it was last compared with, and the latest restart
// generation of this run when there is one. A load-failed refusal also says
// whether the checkout is back on loadedCommit (checkout restored) or still
// holds the published scripts that do not load (not-restored). A stop during
// the load check that could not restore the checkout is state stopped,
// reason load-unproven, checkout not-restored. Rewritten
// only when something other than checkedAt changes, so checkedAt is when this
// state was first seen.
export function codeRecord(config,code,decision,published){
  const iso=ms=>new Date(ms).toISOString(),loaded=validCode(code.loaded)?{digest:code.loaded.digest,files:code.loaded.files}:null;
  const current=validCode(published)&&sha(published.commit)?{commit:published.commit,digest:published.digest,files:published.files}:null;
  code.startedAt??=iso(code.now());
  const path=join(config.journalDirectory,"runner-code.json"),unchecked=r=>JSON.stringify({...r,checkedAt:null}),prior=readCodeRecord(config);
  const generation=Math.max(generationOf(code.restartFor?.generation),generationOf(code.restartGeneration),recordedGeneration(prior));
  const record={version:1,agentId:process.env.TAILTERM_AGENT||null,runId:process.env.TAILTERM_RUN||null,pid:process.pid,startedAt:code.startedAt,checkedAt:iso(code.now()),state:decision.state,...(decision.reason?{reason:decision.reason}:{}),...(decision.state==="restart"&&decision.heldJob?{heldJob:decision.heldJob}:{}),
    ...(["load-failed","load-unproven"].includes(decision.reason)?{checkout:decision.checkout==="restored"?"restored":"not-restored",...(sha(decision.loadedCommit)?{loadedCommit:decision.loadedCommit}:{})}:{}),loaded,current,changed:loaded&&current?RUNNER_CODE_FILES.filter(n=>loaded.files[n]!==current.files[n]):[],...(loaded&&code.restartedFrom&&[loaded.digest,scriptsDigest(loaded.files)].includes(code.marker)?{restartedFrom:code.restartedFrom}:{}),...(generation?{restartGeneration:generation}:{})};
  if(prior && unchecked(prior)===unchecked(record))return prior;
  save(path,record);return record;
}
// Board notices about the runner's code. Only 12-hex prefixes, names from
// RUNNER_CODE_FILES and fixed reason tokens reach the text. The request id
// has no time in it, so a restarted process resends the same notice. From a
// run's second restart on, the restart and restarted ids end in -rN, its
// restart generation, so a run that restarts between the same scripts again
// sends a new notice rather than one the hub refuses as a reused id.
const RESTART_ACTION="Action: run tt deployment setup for the deployer with its current run as predecessor (docs/project-deployment.md).";
const short=v=>HEX64.test(v||"")||sha(v)?v.slice(0,12):"none";
export function codeNotice(record,kind=record?.state){
  if(!["draining","restart","restarted","refused"].includes(kind))return null;
  const run=NAME(record.runId),loaded=short(record.loaded?.digest),current=short(record.current?.digest),reason=CODE_REASONS.includes(record.reason)?record.reason:"unclassified";
  const changed=(Array.isArray(record.changed)?record.changed:[]).filter(n=>RUNNER_CODE_FILES.includes(n)).join(", ")||"none";
  const seen=`Deployer run ${run} loaded scripts ${loaded}; ${record.current?`published tasks-hub ${short(record.current.commit)} has ${current}`:"the published tasks-hub scripts could not be read"}. Changed: ${changed}.`;
  const generation=generationOf(record.restartGeneration),restartId=["restart","restarted"].includes(kind) && generation>=2?`-r${generation}`:"";
  const unrestored=kind==="refused" && reason==="load-failed" && record.checkout!=="restored";
  const requestId=`runner-code-${run}-${loaded}-${current}-${kind}${kind==="refused"?"-"+reason:restartId}${unrestored?"-not-restored":""}`;
  if(kind==="draining")return {requestId,wake:WAKE.routine,subject:"Deployer code is out of date; it restarts itself after the current release",text:`${seen} It claims no new release, and restarts itself onto the published scripts once no release, matrix run or host release lock is active.`};
  // A restart with a held job is its own notice and names the job.
  if(kind==="restart" && record.heldJob!==undefined){const job=NAME(record.heldJob);return {requestId:`${requestId}-held-${job}`,wake:WAKE.routine,subject:"Deployer is restarting itself onto the published scripts with a held job",text:`${seen} Its only owned release ${job} is held, with no release effects, nothing published, no unsettled matrix run and no host release lock, so it restarts itself now with that held job and the same agent and run; the published scripts then run the job. A notice that it runs the published scripts follows; if none arrives the deployer did not come back. ${RESTART_ACTION}`};}
  if(kind==="restart")return {requestId,wake:WAKE.routine,subject:"Deployer is restarting itself onto the published scripts",text:`${seen} No release is active, so it restarts itself now with the same agent and run. A notice that it runs the published scripts follows; if none arrives the deployer did not come back. ${RESTART_ACTION}`};
  if(kind==="restarted")return {requestId,wake:WAKE.routine,subject:"Deployer now runs the published scripts",text:`Deployer run ${run} restarted itself from scripts ${short(record.restartedFrom)} and now runs ${loaded}, the scripts of published tasks-hub ${short(record.current?.commit)}.`};
  // A published runner that does not load is not one to re-provision onto
  // (wi_f6bd907b134a9331 s6): the notice names the checkout's state instead.
  if(kind==="refused" && reason==="load-failed"){
    const head=short(record.loadedCommit),fix="fix the published scripts on tasks-hub; the deployer checks a new published digest again and restarts itself once its runner loads";
    if(!unrestored)return {requestId,wake:WAKE.attention,subject:"Deployer is not claiming releases: its scripts are out of date",
      text:`${seen} The published runner does not load, so it did not restart itself and claims no release; its checkout is back on the loaded commit ${head}. Reason: load-failed. Action: ${fix} (docs/project-deployment.md).`};
    return {requestId,wake:WAKE.attention,subject:"Deployer is not claiming releases: do not re-provision it from its checkout",
      text:`${seen} The published runner does not load, so it did not restart itself and claims no release, and its checkout could not be put back on the loaded commit ${head}: it still holds the scripts that do not load. Reason: load-failed. Do not re-provision from this checkout. Action: ${fix}, or restore the checkout to the loaded commit before any re-provision (docs/project-deployment.md).`};
  }
  return {requestId,wake:WAKE.attention,subject:["loaded-unreadable","published-unreadable"].includes(reason)?"Deployer is not claiming releases: it cannot compare its scripts with the published ones":"Deployer is not claiming releases: its scripts are out of date",
    text:`${seen} It could not restart itself and claims no release. Reason: ${reason}. ${RESTART_ACTION}`};
}
const CODE_LINES={draining:"Deployer code is out of date; no new release is claimed and a restart follows the active work.\n",restart:"Deployer restarting itself onto the published scripts.\n",refused:"Deployer code is out of date and it cannot restart itself; no release is claimed until it is re-provisioned.\n",
  restored:"Deployer code is out of date and the published runner does not load; its checkout is back on the loaded commit and no release is claimed until a published runner loads.\n",
  unrestored:"Deployer code is out of date and the published runner does not load; its checkout could not be restored, so do not re-provision from it: restore it to the loaded commit or fix the published scripts.\n",
  unproven:"Deployer stopped during the published runner's load check; its checkout could not be restored and holds published scripts not proven to load (load-unproven), so do not re-provision from it: restore it to the loaded commit first.\n"};
// The terminal line for a code state: a load-failed refusal says whether its
// checkout was restored rather than asking for a re-provision.
const codeLine=(record,kind)=>kind==="refused" && record.reason==="load-failed"?CODE_LINES[record.checkout==="restored"?"restored":"unrestored"]:CODE_LINES[kind];
export function runnableJob(jobs,agent,run,skipped=new Set()){
  const owned=jobs.find(j=>["claimed","merged"].includes(j.state) && j.agentId===agent && j.runId===run);
  if(owned)return owned;
  if(jobs.some(j=>["claimed","merged","blocked"].includes(j.state)))return null;
  return jobs.find(j=>j.state==="verified" && !skipped.has(j.id))||null;
}
// A job the handler set aside (tt deployment set-aside), or one a stopping
// runner unclaimed but could not archive, left the fence with its claim's
// journal still on the host. Before the job is claimed again, a journal of
// exactly that claim with no effects is archived (ID.json.set-aside-gN or
// ID.json.unclaimed-gN) so the re-planned run starts fresh on the current
// tasks-hub; any other journal is held for handler reconciliation. "none"
// leaves the journal to runRelease.
export function setAsideJournal(journalPath,job){
  const r=job?.reconciliations?.at(-1);
  if(job?.state!=="verified" || !["set_aside","unclaimed"].includes(r?.disposition) || !existsSync(journalPath))return "none";
  let prior;try{prior=JSON.parse(readFileSync(journalPath,"utf8"));}catch{return "held";}
  if(prior?.jobId!==job.id || prior.commit!==job.commit || prior.agentId!==r.agentId || prior.runId!==r.runId || !Array.isArray(prior.effects) || prior.effects.length || prior.published===true)return "held";
  renameSync(journalPath,`${journalPath}.${r.disposition==="unclaimed"?"unclaimed":"set-aside"}-g${job.generation}`);return "archived";
}
const FENCE_REASONS={waiting_matrix:"is waiting for the handler to import integrated verification",waiting_inputs:"is waiting for the handler to bind release inputs",waiting_window:"is waiting for the release quiet window to end",blocked:"is blocked",merged:"is merged and not yet finished",claimed:"is claimed by another deployer run"};
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
// omits generations, which move on each handler import. A holder waiting for the
// handler's own step (the matrix import or the inputs binding) is routine,
// since the handler already holds the directed request for it, and so is one
// waiting for the release quiet window, which ends by itself; a blocked or
// merged holder, or one another deployer run claimed, needs attention.
export function fenceWaitNotice(jobs,holder,reason,locked=false,matrixRun=reason==="waiting_matrix"){
  const waiting=jobs.find(j=>j.id!==holder?.id && j.state==="verified");
  if(!holder || !waiting || !FENCE_REASONS[reason])return null;
  const noEffects=holder.state==="claimed" && holder.published!==true && !holder.receipt && !holder.inputsDigest;
  const advice=!noEffects?"It keeps the fence until handler reconciliation.":matrixRun?"It keeps the fence: its integrated matrix run has not ended and may be using the checkout, so it is not set aside until the run.json of each of its attempts says ended.":locked?"It keeps the fence: the host release lock names its run, so the handler reconciles it with the lock digest after that run stops.":"It has no release effects; the handler can move it aside with tt deployment set-aside so the waiting job claims the fence.";
  return {requestId:`${holder.id}-fence-wait-${waiting.id}-${reason}${noEffects&&matrixRun?"-matrix":noEffects&&locked?"-locked":""}`,waitingJobId:waiting.id,wake:["waiting_matrix","waiting_inputs","waiting_window"].includes(reason)?WAKE.routine:WAKE.attention,subject:"A release job is waiting behind a held project fence",
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
      if(!fullReleaseReceipt(job.receipt,job) || journal.commit!==job.commit || journal.agentId!==job.agentId || journal.runId!==job.runId || (journal.taskId!==undefined && journal.taskId!==job.taskId) || (journal.pauseGeneration!==undefined && journal.pauseGeneration!==job.pauseGeneration) || job.receipt.commit!==(job.integratedCommit||job.commit) || job.receipt.verificationDigest!==job.verificationDigest || job.generation!==journal.finishGeneration+1 || !sameReceipt(journal.receipt,job.receipt))throw releaseError("Saved release receipt does not match pending journal");
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
  return {releasedBackups:value("releasedBackups",3),backupBudgetBytes:value("backupBudgetBytes",4294967296),releasedDists:value("releasedDists",3),attemptDays:value("attemptDays",3)};
}
// Removes only the exact backup copy, rehearsal copy and migration binary
// names of jobs in the deployment list, each a regular file directly in the
// journal directory or directly in the job's rehearsal state directory. A job
// in any state not known to be terminal keeps everything; a terminal job loses
// its leftover rehearsal copy, its migration binary ID-migration, and its
// backup copy unless it is one of the newest released jobs within the size
// budget. In a state directory only regular files go, then the directory once
// empty; a link is never followed or removed.
//
// Then the TailOS copies tailos-dist-COMMIT (wi_d203eb94ef27d8f8). The newest
// released TailOS commit, each of keepDists, the newest releasedDists released
// commits and every commit a non-terminal job carries or names in its inputs
// manifests (its rollback program is there) keep theirs. An older released
// commit's copy goes, and so does the copy a terminal job left without
// releasing it once that job settled before the oldest kept release. A copy no
// listed job accounts for stays, as does one holding anything but regular
// files and directories; an unreadable manifest of a non-terminal job keeps
// every copy.
//
// Last, matrix attempt directories ID-integrated-verification/COMMIT-rN of
// terminal jobs whose run record is ended or absent: each regular file
// directly in one that is not evidence (ATTEMPT_KEPT, and every .log) goes
// once it is attemptDays old. The directory itself always stays.
//
// Every removal is appended to retention.jsonl before it happens. A file
// whose earlier record is already there (its removal failed or was stopped) is
// removed again without a second line: a terminal job's file names are never
// made again. A dist folder follows the same rule by job, name and kind,
// though its name can be made again when that commit is rebuilt: a rebuilt
// copy removed for the same job goes without a new line, since the earlier one
// already records that job's copy, and one removed for another job gets its own.
// Returns the records written.
const DIST=/^tailos-dist-([a-f0-9]{40})$/,ATTEMPT_KEPT=["receipt.json","context.json","plan.json","plan.preserved.json","run.json","run.json.set-aside"];
export function pruneJournal(config,jobs,{now=()=>Date.now(),keepDists=[]}={}){
  const policy=retentionPolicy(config),dir=config.journalDirectory,records=[];
  const size=file=>{try{const s=lstatSync(join(dir,file));return s.isFile()?s.size:null;}catch{return null;}};
  const recorded=(jobId,file,kind,bytes,reason,gone)=>{
    const record={version:1,at:new Date(now()).toISOString(),jobId,file,kind,bytes,reason};
    const fd=openSync(join(dir,"retention.jsonl"),"a",0o600);try{writeFileSync(fd,JSON.stringify(record)+"\n");fsyncSync(fd);}finally{closeSync(fd);}
    gone();records.push(record);return bytes;
  };
  const key=(jobId,file,kind)=>JSON.stringify([jobId,file,kind]),logged=new Set();
  try{for(const line of readFileSync(join(dir,"retention.jsonl"),"utf8").split("\n")){try{const r=JSON.parse(line);logged.add(key(r.jobId,r.file,r.kind));}catch{}}}catch{}
  const remove=(jobId,file,kind,reason)=>{
    const bytes=size(file);if(bytes===null)return 0;
    if(logged.has(key(jobId,file,kind))){rmSync(join(dir,file));return bytes;}
    return recorded(jobId,file,kind,bytes,reason,()=>rmSync(join(dir,file)));
  };
  // A job's backup copy names of every generation, and those from before
  // names carried one, read from the directory once. A rehearsal leftover
  // names its copy too, so it is found after the copy has gone.
  let listed=[];try{listed=readdirSync(dir).sort();}catch{}
  const copiesOf=id=>{
    const pattern=new RegExp(`^(${id}-(?:g[1-9][0-9]*-)?(${BACKUP_NAMES.join("|")})-backup\\.sqlite)(?:\\.rehearsal-${id}.*)?$`),found=new Map();
    for(const name of listed){const m=pattern.exec(name);if(m)found.set(m[1],BACKUP_NAMES.indexOf(m[2]));}
    return [...found.keys()].sort((a,b)=>found.get(a)-found.get(b));
  };
  let total=0;const released=[];
  jobs.forEach((job,index)=>{
    if(typeof job?.id!=="string" || !/^[A-Za-z0-9_-]+$/.test(job.id))return;
    const backups=copiesOf(job.id);
    const held=()=>backups.reduce((sum,file)=>sum+(size(file)??0),0);
    if(!TERMINAL.includes(job.state)){total+=held();return;}
    for(const file of backups)for(const suffix of COPY_FILES)remove(job.id,`${file}.rehearsal-${job.id}${suffix}`,"rehearsal","terminal");
    for(const file of backups){
      const state=`${file}.rehearsal-${job.id}.state`;let names;
      try{if(!lstatSync(join(dir,state)).isDirectory())continue;names=readdirSync(join(dir,state)).sort();}catch{continue;}
      for(const name of names)remove(job.id,`${state}/${name}`,"rehearsal","terminal");
      try{rmdirSync(join(dir,state));}catch{}
    }
    // The migration binary is read only by its own job's rehearsal, which the
    // marker allows once, and no rollback reads it, so no released job keeps it.
    remove(job.id,`${job.id}-migration`,"migration","terminal");
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
  pruneDists();pruneAttempts();
  return records;
  function pruneDists(){
    const dists=listed.filter(name=>DIST.test(name) && isDirectory(name));if(!dists.length)return;
    // Released TailOS commits by the rule releaseBaselines uses, newest first.
    const events=[],carried=new Map(),keep=new Set(keepDists.filter(sha));
    const settled=job=>{const at=Date.parse(job.settledAt??"");return Number.isNaN(at)?-Infinity:at;};
    for(const [index,job] of jobs.entries()){
      if(!job || typeof job!=="object")continue;
      const at=settled(job);
      if(job.receipt?.outcome==="released"){if(Array.isArray(job.receipt.targets) && job.receipt.targets.some(t=>t?.target==="tailos" && t.outcome==="released"))events.push({at,index,commit:job.receipt.commit,id:job.id});}
      else if(job.state==="superseded" && Array.isArray(job.supersession?.targets) && job.supersession.targets.includes("tailos"))events.push({at,index,commit:job.supersession.releasedCommit,id:job.id});
      if(TERMINAL.includes(job.state)){
        for(const commit of [job.commit,job.integratedCommit,job.receipt?.commit])if(sha(commit) && !(carried.get(commit)?.at>=at))carried.set(commit,{at,id:job.id});
        continue;
      }
      for(const commit of [job.commit,job.integratedCommit])if(sha(commit))keep.add(commit);
      // An inputs manifest that cannot be read as a regular file may name any copy.
      if(typeof job.id!=="string" || !/^[A-Za-z0-9_-]+$/.test(job.id))return;
      const manifest=new RegExp(`^${job.id}-(?:g[1-9][0-9]*-)?inputs\\.json$`);
      for(const name of listed){
        if(!manifest.test(name))continue;
        let text;try{if(!lstatSync(join(dir,name)).isFile())return;text=readFileSync(join(dir,name),"utf8");}catch{return;}
        for(const m of text.matchAll(/tailos-dist-([a-f0-9]{40})(?![a-f0-9])/g))keep.add(m[1]);
      }
    }
    events.sort((a,b)=>a.at===b.at?b.index-a.index:b.at-a.at);
    const released=new Map();for(const event of events)if(sha(event.commit) && !released.has(event.commit))released.set(event.commit,event);
    const order=[...released.values()],held=order.slice(0,Math.max(policy.releasedDists,1));
    if(!held.length)return;
    for(const event of held)keep.add(event.commit);
    const oldest=held.at(-1).at;
    for(const name of dists){
      const commit=DIST.exec(name)[1];if(keep.has(commit))continue;
      const owner=released.get(commit),left=carried.get(commit);
      if(!owner && !(left && left.at<oldest))continue;
      const bytes=treeBytes(join(dir,name));if(bytes===null)continue;
      const jobId=owner?owner.id:left.id,gone=()=>rmSync(join(dir,name),{recursive:true});
      if(logged.has(key(jobId,name,"dist")))gone();else recorded(jobId,name,"dist",bytes,owner?"count":"not-released",gone);
    }
  }
  function pruneAttempts(){
    const age=policy.attemptDays*86400000;
    for(const job of jobs){
      if(typeof job?.id!=="string" || !/^[A-Za-z0-9_-]+$/.test(job.id) || !TERMINAL.includes(job.state))continue;
      const parent=job.id+"-integrated-verification";if(!isDirectory(parent))continue;
      for(const attempt of readdirSync(join(dir,parent)).sort()){
        const at=`${parent}/${attempt}`;if(!ATTEMPT.test(attempt) || !isDirectory(at))continue;
        // A run record that is a link or unreadable is not known to have ended.
        if(lstatSync(join(dir,at,"run.json"),{throwIfNoEntry:false})?.isFile()===false)continue;
        const run=readRun(join(dir,at));if(run!==null && run.state!=="ended")continue;
        for(const name of readdirSync(join(dir,at)).sort()){
          if(ATTEMPT_KEPT.includes(name) || name.endsWith(".log"))continue;
          const s=lstatSync(join(dir,at,name),{throwIfNoEntry:false});
          if(s?.isFile() && now()-s.mtimeMs>=age)remove(job.id,`${at}/${name}`,"attempt","age");
        }
      }
    }
  }
  function isDirectory(name){return lstatSync(join(dir,name),{throwIfNoEntry:false})?.isDirectory()===true;}
  // The regular-file bytes under a directory, or null when it holds anything
  // that is neither a regular file nor a directory. Nothing is followed.
  function treeBytes(path){
    let bytes=0;
    for(const name of readdirSync(path)){
      const s=lstatSync(join(path,name));
      if(s.isFile())bytes+=s.size;
      else if(s.isDirectory()){const inner=treeBytes(join(path,name));if(inner===null)return null;bytes+=inner;}
      else return null;
    }
    return bytes;
  }
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
export async function serveDeployment(config,{once=false,signal,configPath,release=runRelease,code,progress=false}={}) {
  if(config.version!==1 || config.enabled!==true || !config.cwd || !config.journalDirectory)throw releaseError("Explicit private activation config required");
  tailosWindow(config);readyWindow(config,"hub");readyWindow(config,"bridge");
  retentionPolicy(config);
  // Read once, at daemon start. At 1 (or absent) nothing below reads a
  // candidate, the solo file or passes a batch key: today's release.
  const batchPolicy=releaseBatchPolicy(config);
  // The batch key for a claimed lead: the candidates' full records while its
  // batch may still be declared or verified, else none.
  const batchFor=(read,jobs,current)=>{
    const batch={max:batchPolicy.max,importWaitMs:batchPolicy.importWaitMs,candidates:[],excluded:[],onFallback:ids=>addBatchSolo(config.journalDirectory,ids)};
    let journal=null;try{journal=JSON.parse(readFileSync(join(config.journalDirectory,current.id+".json"),"utf8"));}catch{}
    if(current.state!=="claimed" || current.integratedVerification || (journal && journal.phase!=="batching" && !journal.batch))return batch;
    let solo;try{solo=readBatchSolo(config.journalDirectory);}catch{process.stderr.write("Batch solo file unreadable; releasing one job at a time.\n");return batch;}
    // Every queued candidate left out here is handed over with its reason;
    // the release records them in the lead's journal once, at this claim.
    const limit=2*(batchPolicy.max-1);
    for(const [at,summary] of batchCandidates(jobs,current,solo).entries()){
      let reason="over-limit";
      if(at<limit){
        try{const detail=readReleaseDetail(read,summary,{bookend:false});reason=batchMismatch(current,detail);if(!reason)batch.candidates.push(detail);}
        catch{reason="detail-unreadable";}
      }
      if(reason)batch.excluded.push({jobId:summary.id,reason});
    }
    return batch;
  };
  // One fence-wait notice per holder, waiting job and reason per process; the
  // hub returns the original for a restart's identical resend.
  const posted=new Set();
  // Every notice of the daemon. While a transport wait is open from an
  // earlier poll, once a read of this poll's own adapter spent its tries, or
  // after this poll's release recorded such a failure, a notice is sent once
  // instead of up to three times: the hub just failed that long and the
  // notice would stretch the poll by as much again. A fence wait, host wait,
  // code or held claim notice that fails is sent again by the next poll under
  // the same request id; a failed call notice is sent again only when that
  // failure recurs. A claim that fails in transport is on the job's adapter
  // and does not by itself turn the retries off.
  let waitOpen=false;
  const say=(reader,argv)=>reader.command(argv,undefined,{retry:!(waitOpen || reader.cliFailures.some(f=>f.transport===true))});
  const notify=(reader,jobs,holder,reason)=>{
    let locked=true;try{locked=hostLockNames(config.cwd,holder);}catch{}
    let matrixRun=true;try{matrixRun=matrixRunUnsettled(config.journalDirectory,holder);}catch{}
    const notice=fenceWaitNotice(jobs,holder,reason,locked,matrixRun);
    if(!notice || posted.has(notice.requestId))return;
    try{say(reader,[config.tt||"tt","send","--kind","notice","--subject",notice.subject,"--text",notice.text,"--request-id",notice.requestId,"--ref",`release-job=${holder.id}`,"--ref",`waiting-job=${notice.waitingJobId}`,...wakeRef(notice.wake)]);posted.add(notice.requestId);}
    catch{process.stderr.write("Fence wait notice not posted; the next poll retries.\n");}
  };
  // One notice per place on the verification host's waitlist (position, list
  // length and holder), and one per held matrix run.
  const matrixNotify=(reader,job,adapter)=>{
    for(const notice of [matrixWaitNotice(job,adapter.matrixWait),matrixHeldNotice(job,adapter.matrixHeld)]){
      if(!notice || posted.has(notice.requestId))continue;
      try{say(reader,[config.tt||"tt","send","--kind","notice","--subject",notice.subject,"--text",notice.text,"--request-id",notice.requestId,"--ref",`release-job=${job.id}`,...wakeRef(notice.wake)]);posted.add(notice.requestId);}
      catch{process.stderr.write("Matrix host notice not posted; the next poll retries.\n");}
    }
  };
  // One notice and one terminal line per job the release quiet window holds,
  // per window end (or read failure) and covered targets, sent in the form of
  // a CLI failure notice (failureSend, below).
  const windowNotify=(reader,job,wait)=>{
    const notice=windowWaitNotice(job,wait);
    if(!notice || posted.has(notice.requestId))return;
    if(!posted.has(`said ${notice.requestId}`)){posted.add(`said ${notice.requestId}`);process.stderr.write(wait.until===undefined?"Release waits: the release quiet window could not be read; nothing is bound or published.\n":"Release waits for the release quiet window; nothing is bound or published until it ends.\n");}
    try{failureSend(say,reader,notice);posted.add(notice.requestId);}
    catch{process.stderr.write("Release window notice not posted; the next poll retries.\n");}
  };
  // One notice and one terminal line per code state, loaded and published
  // digest for this process; a failed post is retried by the next poll.
  const codeNotify=(reader,record,kind=record.state)=>{
    const notice=codeNotice(record,kind);if(!notice)return;
    const line=codeLine(record,kind);
    if(line && !code.said.has(notice.requestId)){code.said.add(notice.requestId);process.stderr.write(line);}
    if(code.noticed.has(notice.requestId))return;
    try{say(reader,[config.tt||"tt","send","--kind","notice","--subject",notice.subject,"--text",notice.text,"--request-id",notice.requestId,...wakeRef(notice.wake)]);code.noticed.add(notice.requestId);}
    catch{process.stderr.write("Runner code notice not posted; the next poll retries.\n");}
  };
  // One notice per job and safe reason for the CLI failures a held poll or a
  // held release recorded. It goes through the CLI that just failed, so it is
  // best effort: the private record and the terminal line remain. A transport
  // failure is not told here: it belongs to the transport wait (transportTurn).
  // The transport wait's two notices are sent in the same form, once each.
  const failureSend=(send,reader,notice)=>send(reader,[config.tt||"tt","send","--kind","notice","--subject",notice.subject,"--text",notice.text,"--request-id",notice.requestId,...(notice.jobId?["--ref",`release-job=${notice.jobId}`]:[]),...wakeRef(notice.wake)]);
  const sayOnce=(reader,argv)=>reader.command(argv,undefined,{retry:false});
  const cliNotify=(reader,failures)=>{
    for(const failure of [...failures]){
      if(failure.transport===true)continue;
      const notice=cliFailureNotice(failure,process.env.TAILTERM_RUN);
      if(posted.has(notice.key))continue;
      try{failureSend(say,reader,notice);posted.add(notice.key);}catch{}
    }
  };
  // A skipped job (a refused claim or a held set-aside journal) is told once
  // per process to each of the project database handler and the live owner
  // helper, directed so both are woken. While a recipient cannot be resolved
  // or its post fails, one Board notice without a recipient stands in and that
  // recipient is tried again at the next poll. Only that Board copy carries
  // the wake class. Nothing here changes the skip. A roster read that found
  // no single live owner helper is tried again only after heldRetry.rosterMs;
  // the handler lookup and a failed post are tried at every poll.
  let rosterMissAt=null;
  const HELD_TO={handler:reader=>reader.handler(),helper:reader=>{
    if(rosterMissAt!==null && heldRetry.now()-rosterMissAt<heldRetry.rosterMs)throw releaseError("Owner helper roster read deferred");
    try{
      const live=JSON.parse(reader.command([config.tt||"tt","agents","--json"])).filter(a=>a?.role==="owner_helper" && !["closed","exited"].includes(a.status));
      if(live.length!==1 || !/^agt_[a-f0-9]+$/.test(live[0].id||""))throw releaseError("One live owner helper required");
      rosterMissAt=null;return live[0].id;
    }catch(error){rosterMissAt=heldRetry.now();throw error;}}};
  const heldNotify=(reader,job,kind,reason)=>{
    const notice=heldClaimNotice(job,kind,reason,process.env.TAILTERM_RUN);if(!notice)return;
    const send=(suffix,to)=>say(reader,[config.tt||"tt","send","--kind","notice",...(to?["--to",to]:[]),"--subject",notice.subject,"--text",notice.text,"--request-id",`${notice.requestId}-${suffix}`,"--ref",`release-job=${notice.jobId}`,...(notice.item?["--ref",`item=${notice.item}`]:[]),...(notice.commit?["--ref",`commit=${notice.commit}`]:[]),...(to?[]:wakeRef(notice.wake))]);
    let missed=false;
    for(const [name,resolve] of Object.entries(HELD_TO)){
      if(posted.has(`${notice.key} ${name}`))continue;
      try{send(name,resolve(reader));posted.add(`${notice.key} ${name}`);}catch{missed=true;}
    }
    if(!missed)return;
    // Once per job and process: the retries below are silent.
    if(!posted.has(`undelivered ${notice.jobId}`)){posted.add(`undelivered ${notice.jobId}`);process.stderr.write("Held claim notice not delivered to every recipient; the next poll retries.\n");}
    if(posted.has(`${notice.key} board`))return;
    try{send("board");posted.add(`${notice.key} board`);}catch{}
  };
  // The end of every poll that read the hub: failures are the calls of this
  // poll that spent their tries on a transport error (transportWait).
  const transportTurn=(reader,failures)=>{
    const path=transportWaitPath(config.journalDirectory),now=transportWait.now(),at=new Date(now).toISOString(),safe=r=>REASON.test(r||"")?r:"unclassified";
    const post=notice=>{
      try{failureSend(sayOnce,reader,notice);return "posted";}
      catch(error){return error?.transport===true?"unreached":"refused";}
    };
    let wait=readTransportWait(config.journalDirectory);
    const escalate=()=>{
      if(!wait.due || wait.escalated)return true;
      const outcome=post(transportWaitNotice(wait));if(outcome==="unreached")return false;
      wait.escalated=outcome;process.stderr.write(outcome==="posted"?"Transport wait passed its bound; attention notice posted.\n":"Transport wait passed its bound; the hub refused its attention notice.\n");return true;
    };
    const routine=()=>{
      if(!wait.routine && post(cliFailureNotice({reason:wait.cause,jobId:wait.jobId,at:wait.since},process.env.TAILTERM_RUN))!=="unreached")wait.routine=true;
      return wait.routine;
    };
    if(!failures.length){
      if(!wait)return;
      // The wait ends here unless an owed notice is still unreached; each is
      // tried whatever the other did.
      const told=routine(),escalated=escalate();
      if(told && escalated){rmSync(path,{force:true});waitOpen=false;return;}
    }else{
      const jobId=failures.find(f=>f.jobId)?.jobId??null;
      if(!wait || (wait.jobId && jobId && wait.jobId!==jobId))wait={version:1,jobId,since:at,cause:safe(failures[0].reason),polls:0,routine:false,escalated:false};
      if(!wait.jobId && jobId)wait.jobId=jobId;
      wait.polls=(Number.isSafeInteger(wait.polls)?wait.polls:0)+1;wait.latest=safe(failures.at(-1).reason);
      if(!wait.due && now-Date.parse(wait.since)>=transportWait.escalateMs)wait.due={jobId:wait.jobId,cause:wait.latest,minutes:Math.round(transportWait.escalateMs/60000)};
      routine();
      escalate();
    }
    wait.lastAt=at;waitOpen=true;
    try{save(path,wait);}catch{}
  };
  // Progress reports (HostAdapter.progress), only when the caller asks: one
  // before the first poll and one at the end of every poll, a held one
  // included, naming the job and the wait the poll ended in. The wait is
  // information for the hub's notice; the hub decides nothing by it.
  const progressState={withdrawn:false};
  const reporter=progress===true?Object.assign(new HostAdapter(config,{}),{progressOn:true,progressState}):null;
  // The runner's own code, once per poll and before any claim. It restarts
  // when idle: no claimed, merged or blocked job, no matrix run of this
  // process and no host release lock. It also restarts when its only owned
  // job is held (heldRestartJob), so a fix for a stuck job can be loaded.
  // Whatever is decided, a stale runner claims nothing below; a job it
  // already owns is otherwise still run. With no ledger (a failed read) the
  // runner is idle only by journalIdle, and has no held job. STOP: a stop
  // signal arrived, and the caller returns.
  const STOP=Symbol("stop");let lastLock=null;
  const gateTurn=async(reader,ledger)=>{
    let published=null;try{published=code.published(config.cwd);}catch{}
    let idle,held=null;
    if(ledger){
      const {jobs,details,lock}=ledger;
      idle=!jobs.some(j=>["claimed","merged","blocked"].includes(j.state)) && MATRIX_CHILDREN.size===0 && !existsSync(lock);
      held=idle?null:heldRestartJob({jobs,details,agent:process.env.TAILTERM_AGENT,run:process.env.TAILTERM_RUN,journalDirectory:config.journalDirectory,lock,matrixChildren:MATRIX_CHILDREN.size});
    }else idle=journalIdle({journalDirectory:config.journalDirectory,cwd:config.cwd,agent:process.env.TAILTERM_AGENT,run:process.env.TAILTERM_RUN,lastLock,matrixChildren:MATRIX_CHILDREN.size});
    let decision=codeDecision({loaded:code.loaded,published,idle,marker:code.marker,held});
    // A re-exec that failed in this process is not tried again for the same
    // published code, so its refusal is not rewritten on every poll.
    if(decision.state==="restart" && code.execFailed===published.digest)decision={state:"refused",reason:"exec-failed"};
    // Nor is a published runner that did not load spawned again; its
    // refusal keeps the checkout state that load failure left.
    if(decision.state==="restart" && code.loadFailed===published.digest)decision={state:"refused",reason:"load-failed",checkout:code.loadRestored?"restored":"not-restored",loadedCommit:code.loadedHead};
    // A runner told to stop does not restart: nothing below moves the
    // checkout, replaces the process or claims once the signal is set.
    if(decision.state==="restart"){await stopSignalTurn();if(signal?.aborted)return STOP;}
    // The HEAD this process loaded from is the one its first poll finds:
    // nothing before that poll moves the checkout.
    if(code.loadedHead===undefined){try{code.loadedHead=git(config.cwd,"rev-parse","HEAD");}catch{code.loadedHead=null;}}
    if(decision.state==="restart"){
      try{code.prepare(config.cwd,published);}
      catch(error){const reason=failureReason(error);decision={state:"refused",reason:CODE_REASONS.includes(reason)?reason:"checkout-failed"};}
    }
    // The published runner must load before this process is replaced by it.
    if(decision.state==="restart"){
      let loads=false;try{loads=await code.loads(config.cwd,signal)===true;}catch{}
      // A stop ends the check (its child is killed), so the published
      // scripts are unproven: the checkout goes back to the loaded commit as
      // for load-failed, or the record and line say it holds them
      // (wi_cda779de751962f6 p1).
      if(signal?.aborted){
        try{code.restore(config.cwd,code.loadedHead,code.loaded);}
        catch{
          try{codeRecord(config,code,{state:"stopped",reason:"load-unproven",checkout:"not-restored",loadedCommit:code.loadedHead},published);}catch{}
          process.stderr.write(CODE_LINES.unproven);
        }
        return STOP;
      }
      // The checkout goes back to the loaded commit, so re-provisioning from
      // it starts a runner that loads (wi_f6bd907b134a9331 s6).
      if(!loads){
        let restored=false;try{code.restore(config.cwd,code.loadedHead,code.loaded);restored=true;}catch{}
        code.loadFailed=published.digest;code.loadRestored=restored;
        decision={state:"refused",reason:"load-failed",checkout:restored?"restored":"not-restored",loadedCommit:code.loadedHead};
      }
    }
    if(decision.state==="restart"){
      // Recorded and announced first: a successful exec never returns.
      const generation=restartGeneration(config,code,published);
      codeNotify(reader,codeRecord(config,code,decision,published));
      try{await code.exec(published,signal,generation);}catch{code.execFailed=published.digest;decision={state:"refused",reason:"exec-failed"};}
      if(signal?.aborted)return STOP;
    }
    const codeNow=codeRecord(config,code,decision,published);
    // An unreadable published ref is announced only when it withholds a
    // claim (below), so a poll with nothing to claim stays silent.
    if(codeNow.state==="current"){if(codeNow.restartedFrom)codeNotify(reader,codeNow,"restarted");}
    else if(codeNow.reason!=="published-unreadable")codeNotify(reader,codeNow);
    return codeNow;
  };
  reporter?.progress({phase:"start"});
  while(!signal?.aborted){
    let reader=null;const ran=[];let ended={};
    waitOpen=readTransportWait(config.journalDirectory)!==null;
    try {
    // An unreadable or invalid edit holds the whole poll, before any claim.
    const configured=configPath?readBaselines(configPath):config.baselines;
    reader=new HostAdapter(config,{});
    const read=argv=>reader.command([config.tt||"tt",...argv]);
    // The poll's ledger read writes nothing, so a stale snapshot refused on a
    // detail's bookend (or the closing one) restarts it whole: fresh
    // summaries, details and bookend, at most RELEASE_READ_ATTEMPTS passes,
    // each summary read keeping its own bound. A failure the read recovered
    // from is dropped from reader.cliFailures (the private record keeps it),
    // so a later failure of this poll notices only what ended it.
    const readLedger=()=>{
      for(let pass=1;;pass++){
        const mark=reader.cliFailures.length;
        const jobs=readReleaseSummaries(read);reader.cliFailures.splice(mark);
        // Validate complete baseline history before any recovery write, pruning or claim.
        const baselines=deploymentBaselines(configured,jobs);
        // Fetch every locally relevant recovery record and the selected/held job
        // before any write, deletion or claim. Any other failed detail read holds the poll.
        const selected=runnableJob(jobs,process.env.TAILTERM_AGENT,process.env.TAILTERM_RUN);
        const details=new Map();
        const lock=hostLockPath(config.cwd,jobs[0]||{});
        const lockedJob=existsSync(lock)?JSON.parse(readFileSync(lock,"utf8")).jobId:null;
        try{
          for(const summary of jobs){
            const path=join(config.journalDirectory,summary.id+".json");
            const pending=existsSync(path) && ["finishing","receipt_pending"].includes(JSON.parse(readFileSync(path,"utf8")).phase);
            if(summary.id===selected?.id || ["claimed","merged","blocked"].includes(summary.state) || pending || summary.id===lockedJob){
              details.set(summary.id,readReleaseDetail(read,summary));
            }
          }
          bookendReleaseLedger(read,jobs.releaseSnapshot,jobs.releaseHead);
        }catch(error){if(!staleSnapshot(error) || pass>=RELEASE_READ_ATTEMPTS)throw error;reader.cliFailures.splice(mark);continue;}
        return {jobs,baselines,details,lock};
      }
    };
    // A failed ledger read still runs the code gate (wi_56ac00ba2854fa3d
    // c1), with idleness taken from this run's own journals, before the
    // failure holds the poll as it did.
    let ledger=null,readError=null;
    try{ledger=readLedger();}catch(error){readError=error;}
    if(!ledger){
      if(code){let turn=null;try{turn=await gateTurn(reader,null);}catch{}if(turn===STOP)return;}
      throw readError;
    }
    const {jobs,baselines,details,lock}=ledger;lastLock=lock;
    const recovery=[...details.values()];
    reconcileReceipts(config,recovery);reconcileHostLocks(config,recovery);
    try{pruneJournal(config,jobs,{keepDists:[baselines.tailos]});}catch{process.stderr.write("Journal retention sweep failed; remaining backup copies kept.\n");}
    let codeNow=null;
    if(code){codeNow=await gateTurn(reader,{jobs,details,lock});if(codeNow===STOP)return;}
    const skipped=new Set();
    for (;;) {
      const summary=runnableJob(jobs,process.env.TAILTERM_AGENT,process.env.TAILTERM_RUN,skipped);
      // A verified job behind a skipped one is read here, after this poll's
      // writes (receipts, set-aside archives, notices). A stale snapshot on
      // its bookend does not restart the read, which would repeat them: the
      // poll ends quietly, nothing is claimed and the next poll reads afresh.
      let job=null;
      if(summary){
        const mark=reader.cliFailures.length;
        try{job=details.get(summary.id)||readReleaseDetail(read,summary);}
        catch(error){if(!staleSnapshot(error))throw error;reader.cliFailures.splice(mark);process.stderr.write("Release ledger changed during the poll; the next poll reads it again.\n");break;}
      }
      if(!job){
        const holder=jobs.find(j=>["claimed","merged","blocked"].includes(j.state)),onHold=jobs.find(j=>j.state==="held");
        if(holder)notify(reader,jobs,details.get(holder.id),holder.state);
        if(holder)ended={jobId:holder.id,wait:{kind:existsSync(lock)?"host_lock":"fence"}};
        else if(onHold)ended={jobId:onHold.id,wait:{kind:"owner_hold"}};
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
      const adapter=new HostAdapter(config,job);ran.push(adapter);
      if(reporter){adapter.progressOn=true;adapter.progressState=progressState;}
      try{if(job.state==="verified")adapter.native("claim");}
      catch(error){
        // A claim that failed only in transport is not a held claim: the hub
        // gave no answer, so there is nothing to reconcile. The poll ends
        // quietly here, before any later job spends its own tries or is
        // claimed out of order, and the transport wait below names the
        // failure and escalates it. If the hub did record the claim, the next
        // poll finds the job claimed by this run and runs it as its own.
        if(error?.transport===true){ended={jobId:job.id,wait:{kind:"transport"}};break;}
        skipped.add(job.id);process.stderr.write("Release claim held; handler reconciliation required.\n");const call=failureReason(error),said=error?.refusal?`${error.refusal} (${call})`:call;heldNotify(reader,job,"claim",said.length<=160?said:call);continue;}
      const current=adapter.job;adapter.baselines=baselines;
      const {testPolicy,sleep,now,...activation}=config;
      const batch=batchPolicy.max>1?batchFor(read,jobs,current):null;
      let result;
      try{result=await release({...activation,baselines,job:current,journalPath:join(config.journalDirectory,current.id+".json"),...(batch?{batch}:{}),...(codeNow?{code:{loaded:codeNow.loaded?.digest??null,current:codeNow.current?.digest??null}}:{})},adapter);}
      catch{process.stderr.write("Release held; inspect handler fence and private journal.\n");cliNotify(reader,[...reader.cliFailures,...adapter.cliFailures]);}
      // A batch's own members are not waiting behind its lead.
      let waiting=jobs;
      if(batch){try{const members=new Set(JSON.parse(readFileSync(join(config.journalDirectory,current.id+".json"),"utf8")).batch?.jobs?.map(j=>j.jobId));waiting=jobs.filter(j=>!members.has(j.id));}catch{}}
      const unreached=adapter.cliFailures.some(f=>f.transport===true);if(unreached)waitOpen=true;
      if(["waiting_matrix","waiting_inputs","waiting_window"].includes(result?.outcome))notify(reader,waiting,adapter.job||current,result.outcome);
      if(result?.outcome==="waiting_window")windowNotify(reader,adapter.job||current,result.window);
      // A wait that follows a transport failure, the fence check's included
      // (waiting_transport); transportTurn below names it once per wait. The
      // hub knows no window wait, so the window's is reported as owner_hold.
      if(unreached && ["waiting_matrix","waiting_inputs","waiting_window","waiting_transport"].includes(result?.outcome))process.stderr.write("Release waits: a hub call failed in transport; the next poll retries.\n");
      matrixNotify(reader,adapter.job||current,adapter);
      const waits={waiting_matrix:"matrix",waiting_inputs:"inputs",waiting_window:"owner_hold",waiting_transport:"transport"};
      ended={jobId:current.id,...(waits[result?.outcome]?{wait:{kind:waits[result.outcome],...(result.outcome==="waiting_matrix" && adapter.matrixPid?{pid:adapter.matrixPid}:{})}}:{})};
      break;
    }
    } catch(error) {process.stderr.write(`Deployment poll held (${failureReason(error)}); inspect native input or recovery evidence.\n`);if(reader)cliNotify(reader,reader.cliFailures);}
    if(reader)try{transportTurn(reader,[...reader.cliFailures,...ran.flatMap(a=>a.cliFailures)].filter(f=>f.transport===true));}catch{process.stderr.write("Transport wait not recorded; the next poll retries.\n");}
    reporter?.progress({phase:"poll",...ended});
    if(once)break;
    // A stop signal ends the wait, so the stop below runs well inside the
    // hub's 90 s deployer heartbeat bound.
    await new Promise(r=>{if(signal?.aborted)return r();const done=()=>{clearTimeout(timer);signal?.removeEventListener("abort",done);r();},timer=setTimeout(done,30000);signal?.addEventListener("abort",done,{once:true});});
  }
  if(signal?.aborted)stopRelease(config);
}
// A clean stop (SIGTERM or SIGINT) of a run that holds a claimed job with no
// effects gives the claim back with tt deployment unclaim, an exact-run record
// that needs no incident or prevention item, and archives the claim's journal
// as ID.json.unclaimed-gN; the job is verified again in its queue place. An
// unclaim call that throws (a lost reply, or one that fails validation) is
// followed by one read of that job, a single ten-second try with no retry:
// only a job the hub shows verified at a later generation, naming no run,
// with the same pins and an unclaimed record of this agent and run last, is
// reported returned and its journal archived as on success. Any other claim
// of this run, or any failure here, is left as it is for handler
// reconciliation (docs/project-deployment.md, "Pause").
function stopRelease(config){
  const agent=process.env.TAILTERM_AGENT,run=process.env.TAILTERM_RUN,safe=id=>/^rel_[A-Za-z0-9_-]{1,64}$/.test(id||"")?id:"unknown";
  let owned=null;
  try{
    const reader=new HostAdapter(config,{}),read=argv=>reader.command([config.tt||"tt",...argv]);
    const jobs=readReleaseSummaries(read);
    owned=jobs.find(j=>["claimed","merged","blocked"].includes(j.state) && j.agentId===agent && j.runId===run)||null;
    if(!owned)return;
    const details=new Map([[owned.id,readReleaseDetail(read,owned)]]),job=details.get(owned.id);
    const id=unclaimableJob({jobs,details,agent,run,journalDirectory:config.journalDirectory,lock:hostLockPath(config.cwd,job),matrixChildren:MATRIX_CHILDREN.size});
    if(id){
      let next;
      try{next=new HostAdapter(config,job).native("unclaim");}
      catch{
        const current=JSON.parse(reader.command([config.tt||"tt","deployment","get","--job",id],config.cwd,{timeout:10000,retry:false})),last=current?.reconciliations?.at?.(-1);
        const pins=["id","taskId","entryId","itemId","itemRevision","scopeRevision","orderMessageSeq","commit","verificationDigest","pauseGeneration"];
        if(current?.summary===true || current?.state!=="verified" || current.agentId!==undefined || current.runId!==undefined || !Number.isSafeInteger(current.generation) || current.generation<=job.generation || !pins.every(k=>current[k]===job[k]) || last?.disposition!=="unclaimed" || last.jobId!==id || last.agentId!==agent || last.runId!==run)throw releaseError("Unclaim not confirmed");
        next=current;
      }
      const path=join(config.journalDirectory,id+".json");
      // setAsideJournal archives it before the next claim if this rename fails.
      let archived="journal archived";try{renameSync(path,`${path}.unclaimed-g${next.generation}`);}catch{archived="its journal is archived before the next claim";}
      process.stderr.write(`Release ${safe(id)} returned to verified at stop; ${archived}.\n`);return;
    }
  }catch{}
  if(owned)process.stderr.write(`Stopped while holding release ${safe(owned.id)}; handler reconciliation required.\n`);
  else process.stderr.write("Stopped without reading the release ledger; any claim of this run waits for handler reconciliation.\n");
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
    await serveDeployment(config,{once:process.argv.includes("--once"),signal:controller.signal,configPath:process.argv[index+1],code:runnerCodeGate(),progress:true});
  }catch{process.stderr.write("Deployment blocked; inspect private host journal and handler release gate.\n");process.exitCode=1;}
}
