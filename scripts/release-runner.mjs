import { createHash } from "node:crypto";
import { tmpdir } from "node:os";
import { fileURLToPath } from "node:url";
import { execFileSync, spawn } from "node:child_process";
import { openSync, closeSync, fsyncSync, writeFileSync, readFileSync, renameSync, mkdirSync, mkdtempSync, existsSync, rmSync, copyFileSync, cpSync, chmodSync, statSync, lstatSync, realpathSync, readdirSync, constants } from "node:fs";
import { join, resolve, dirname, basename, isAbsolute } from "node:path";
import { digest, diffPaths, receiptEligible } from "./verify-matrix.mjs";
import { PRIORITIES, RUN_TIMEOUT_GRACE_MS, holderCapMs, lockPath, readHostState, pidGone, groupGone } from "./verify-matrix-host-lock.mjs";
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
function childReason(argv, error) {
  const base = p => String(p).split("/").pop(), program = /\.(py|mjs)$/.test(argv[1] || "") ? base(argv[1]) : base(argv[0]);
  const name = /^[A-Za-z0-9._-]{1,64}$/.test(program) ? program : "program";
  const how = error.code === "ETIMEDOUT" ? "timeout" : /^SIG[A-Z0-9]{1,12}$/.test(error.signal || "") ? `signal ${error.signal}` : Number.isInteger(error.status) ? `exit ${error.status}` : "not started";
  let fields = "";
  try {
    const last = JSON.parse(String(error.stdout || "").trim().split("\n").at(-1)), token = v => typeof v === "string" && /^[a-z][a-z0-9-]{0,47}$/.test(v) ? v : null;
    const classification = token(last?.classification), stage = token(last?.stage);
    if (classification) fields = `: ${classification}${stage ? ` at ${stage}` : ""}`;
  } catch {}
  return `${name} ${how}${fields}`;
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
    if (typeof raw !== "string" || !/^[1-9][0-9]*$/.test(raw) || !Number.isSafeInteger(ms) || ms > 1800000) throw releaseError("Invalid matrix check timeout");
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
  const h = wait.holder, pid = Number.isSafeInteger(h?.pid) ? h.pid : "unknown", again = Number.isSafeInteger(wait.change) && wait.change > 1 ? `-n${wait.change}` : "";
  const id = holder => `${job.id}-matrix-wait-${wait.attempt}-p${wait.position}-of${wait.length}-${holder}${again}`, full = id(h ? NAME(h.id) : "none");
  // The hub accepts at most 128 characters; a shortened holder id keeps the place distinct.
  return {requestId: full.length <= 128 ? full : id(NAME(h?.id).slice(0, 8)), subject: "A release job is waiting for the verification host",
    text: `Release ${job.id} waits for the verification host at position ${wait.position} of ${wait.length} at priority ${NAME(wait.priority)} ${h ? `behind ${NAME(h.item)}/${NAME(h.agent)}/pid ${pid}` : "with no holder"}`};
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
  if (branch !== "tasks-hub" || !sha(job.commit) || !sha(job.baseCommit) || job.plan?.commit !== job.commit) throw new Error("Release binding mismatch");
  if (git(cwd,"status","--porcelain")) throw new Error("Dirty deployment checkout");
  const expected = git(cwd,"rev-parse",`refs/heads/${branch}`);
  if (!sha(expected) || git(cwd,"rev-parse",`${job.commit}^{commit}`)!==job.commit || git(cwd,"rev-parse",`${job.baseCommit}^{commit}`)!==job.baseCommit) throw new Error("Unknown commit identity");
  try { git(cwd,"merge-base","--is-ancestor",job.baseCommit,job.commit); } catch { throw new Error("Unknown candidate base"); }
  git(cwd,"checkout","--detach",expected);
  try {
    try {git(cwd,"merge-base","--is-ancestor",expected,job.commit);git(cwd,"checkout","--detach",job.commit);}
    catch {
      const commits=git(cwd,"rev-list","--reverse",`${job.baseCommit}..${job.commit}`).split("\n").filter(Boolean);
      if (!commits.length || commits.some(c => git(cwd,"rev-list","--parents","-n","1",c).split(" ").length!==2)) throw new Error("Nonlinear candidate series");
      git(cwd,"cherry-pick",...commits);
    }
  } catch {
    try {git(cwd,"cherry-pick","--abort");} catch {}
    git(cwd,"checkout","--detach",expected);
    throw new Error("Candidate integration refused");
  }
  const integrated=git(cwd,"rev-parse","HEAD");
  return {expected,integrated,changed:diffPaths(cwd,expected,integrated)};
}
// Moving tasks-hub under a worktree that has it checked out leaves that
// working tree showing the release reversed, so every ref mutation refuses
// first. The owner's main checkout is detached at provisioning.
export function tasksHubCheckedOut(cwd) {
  return git(cwd,"worktree","list","--porcelain").split("\n").includes("branch refs/heads/tasks-hub");
}
export function publishIntegration(cwd, integrated, expected) {
  if (!sha(integrated)||!sha(expected)||git(cwd,"rev-parse","HEAD")!==integrated||git(cwd,"status","--porcelain")) throw new Error("Integrated checkout changed");
  if (tasksHubCheckedOut(cwd)) throw new Error("tasks-hub is checked out in a worktree");
  try {git(cwd,"update-ref","refs/heads/tasks-hub",integrated,expected);}catch{throw new Error("Release ref race");}
}
// D1: after a rollback, tasks-hub gets a commit whose tree is the pre-release
// tree, so the next release does not ship the rolled-back change again.
export function revertCommit(cwd, job, integrated, expected) {
  if (!sha(integrated)||!sha(expected)) throw new Error("Revert binding required");
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
  if (!sha(commit)) throw new Error("Exact build commit required");
  const dir=mkdtempSync(join(tmpdir(),"tailterm-release-build-")),src=join(dir,"src");
  try {
    git(dir,"clone","--quiet","--shared","--no-checkout",resolve(cwd),src);git(src,"checkout","--quiet","--detach",commit);
    if(git(src,"rev-parse","HEAD")!==commit||git(src,"status","--porcelain"))throw new Error("Build checkout mismatch");
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
  if(!job || !(["claimed","merged"].includes(job.state)) || !sha(job.commit) || !/^[a-f0-9]{64}$/.test(job.verificationDigest||""))throw new Error("Claimed verified job required");
  const lock=hostLockPath(cwd,job);mkdirSync(dirname(lock),{recursive:true,mode:0o700});
  let fd;try{fd=openSync(lock,"wx",0o600);}catch{throw new Error("Release host locked; inspect prior execution");}
  writeFileSync(fd,JSON.stringify({jobId:job.id,agentId:job.agentId,runId:job.runId}));fsyncSync(fd);
  let state={version:1,jobId:job.id,commit:job.commit,agentId:job.agentId,runId:job.runId,phase:"prepared",effects:[]};
  if(existsSync(journalPath)){
    const prior=JSON.parse(readFileSync(journalPath,"utf8")),recovery=job.reconciliations?.at(-1);
    const recovered=recovery?.disposition==="requeue" && recovery.noActiveExecution===true && recovery.noPublication===true && recovery.journalState==="no_effects" && recovery.jobId===job.id && recovery.journalDigest===fileDigest(journalPath) && recovery.agentId===prior.agentId && recovery.runId===prior.runId && prior.effects.length===0 && prior.published!==true && prior.jobId===job.id && prior.commit===job.commit;
    if(recovered){renameSync(journalPath,journalPath+".reconciled-"+recovery.journalDigest);}
    else
    if(prior.jobId===job.id && prior.commit===job.commit && (["waiting_matrix","waiting_inputs","pushing","receipt_pending","finishing"].includes(prior.phase)) && (!["waiting_matrix","waiting_inputs"].includes(prior.phase) || !prior.effects.length) && git(cwd,"rev-parse","HEAD")===prior.integrated && !git(cwd,"status","--porcelain")) {state=prior;}
    else {
      closeSync(fd);rmSync(lock);
      if(prior.jobId===job.id && prior.commit===job.commit && prior.phase==="complete") return prior.receipt;
      throw new Error("Ambiguous journal requires handler reconciliation");
    }
  }
  const checkpoint=()=>save(journalPath,state);let step=null;
  const fence=async()=>{if(await adapter.fence(job)!==true)throw releaseError("release fence lost");};
  // Every selected target is live-verified before this runs, so nothing here
  // rolls back: a failed push is escalated once and the release stands.
  const pushAndFinish=async receipt=>{
    state.receipt=receipt;state.phase="pushing";checkpoint();
    receipt.push={remote:"origin",commit:receipt.commit,outcome:pushRelease(cwd,receipt.commit)?"pushed":"failed"};checkpoint();
    if(receipt.push.outcome==="failed" && !state.pushEscalated){state.pushEscalated=true;checkpoint();try{await adapter.escalate({jobId:job.id,outcome:"released",push:"failed"});}catch{}}
    state.phase="finishing";checkpoint();await fence();state.finishGeneration=adapter.job?.generation??job.generation;checkpoint();await adapter.finish(receipt,state.finishGeneration);state.phase="complete";checkpoint();return receipt;
  };
  try {
    if(state.phase==="receipt_pending" || state.phase==="finishing"){
      await adapter.finish(state.receipt,state.finishGeneration);state.phase=state.receipt.outcome==="released"?"complete":"blocked";checkpoint();return state.receipt;
    }
    if(state.phase==="pushing")return await pushAndFinish(state.receipt);
    // A job with no resumable journal (a requeue) first settles the matrix
    // runs of its earlier attempts: until each is confirmed stopped, ended or
    // set aside, nothing here touches the checkout, starts a run or refuses.
    if(state.phase==="prepared" && adapter.settleMatrixRuns && await adapter.settleMatrixRuns(job)!==true)return {jobId:job.id,outcome:"waiting_matrix"};
    checkpoint();await fence();
    const integration=["waiting_matrix","waiting_inputs"].includes(state.phase)?{expected:state.expected,integrated:state.integrated}:integrateCandidate(cwd,job);state.integrated=integration.integrated;state.expected=integration.expected;state.phase="integrated";checkpoint();
    if(integration.integrated!==job.commit){
      // Saved before the matrix run is started, so a runner stopped while
      // the run waits or runs resumes this job from its attempt record.
      state.phase="waiting_matrix";checkpoint();
      await fence();if(await adapter.verifyIntegrated({...job,integratedCommit:integration.integrated})!==true)return {jobId:job.id,outcome:"waiting_matrix"};
      state.phase="integrated";checkpoint();
    }
    if(adapter.verifyInputs && !await adapter.verifyInputs(integration.integrated)){state.phase="waiting_inputs";checkpoint();return {jobId:job.id,outcome:"waiting_inputs"};}
    await fence();publishIntegration(cwd,integration.integrated,integration.expected);state.published=true;checkpoint();
    await adapter.merged(integration.integrated);state.phase="merged";checkpoint();
    const selected=selectReleaseTargets(cwd,baselines,integration.integrated);
    const receipt={version:1,jobId:job.id,commit:integration.integrated,verificationDigest:job.verificationDigest,targets:[],outcome:"released"};
    state.receipt=receipt;checkpoint();
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
    // The first failed target step is kept; a resumed run never rewrites it.
    if(step && !state.failure){state.failure={...step,reason:failureReason(error)};checkpoint();}
    if(state.phase==="pushing")return {jobId:job.id,outcome:"pushing"};
    if(state.phase==="finishing" || state.phase==="receipt_pending"){
      state.phase="receipt_pending";checkpoint();return {jobId:job.id,outcome:"receipt_pending"};
    }
    // An uncertain side effect cannot be replayed. Rollback uses only retained
    // target artifacts; the adapter must never restore an old live database.
    if(!state.published && state.effects.length===0){
      state.phase="refusing";state.refusalReason=failureReason(error);checkpoint();await adapter.refuse();state.phase="refused";checkpoint();await adapter.escalate({jobId:job.id,outcome:"refused",reason:state.refusalReason});throw new Error("Release refused before publication");
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
      try{await adapter.finish(state.receipt,state.finishGeneration);}catch{throw new Error("Release failed; final receipt pending retry");}
      state.phase="blocked";checkpoint();
    }else{await adapter.block(job.id);}
    throw new Error("Release failed; inspect saved journal");
  } finally {closeSync(fd);rmSync(lock);}
}

// The private activation config names existing host credential stores and
// handler-produced preflight files. Probe/rollback programs are pinned host
// programs, never commands received from Board text.
export class HostAdapter {
  constructor(config,job){this.config=config;this.job=job;this.artifacts=new Map();this.serial=0;this.now=()=>Date.now();
    // The TailOS poller's fetch and clock; tests replace them.
    this.probeDeps={fetchJSON:hostDeps.fetchJSON,sleep:hostDeps.sleep,now:hostDeps.now};this.probeWaits=new Map();
    // What the last poll saw: a wait on the host list, or a held run.
    this.matrixChildren=MATRIX_CHILDREN;this.matrixWait=null;this.matrixHeld=null;}
  command(argv,cwd=this.config.cwd,{timeout=600000}={}){
    if(!Array.isArray(argv)||!argv.length||argv.some(a=>typeof a!=="string"||/[\0\r\n]/.test(a)))throw new Error("Invalid host operation argv");
    try{return execFileSync(argv[0],argv.slice(1),{cwd,encoding:"utf8",stdio:["ignore","pipe","pipe"],maxBuffer:8*1024*1024,timeout});}
    catch(error){const failed=new Error("Host operation failed");failed.releaseReason=childReason(argv,error);throw failed;}
  }
  native(operation,extra=[],expectedGeneration=this.job.generation){
    const args=[this.config.tt||"tt","deployment",operation,"--job",this.job.id,"--generation",String(expectedGeneration),"--request-id",`${this.job.id}-${operation}-${expectedGeneration}`,...extra];
    this.job=JSON.parse(this.command(args));return this.job;
  }
  async fence(){try{this.native("check");return true;}catch{return false;}}
  // The project handler by the project rule (hub operation "handler"): a
  // finished entry holds no item lease, so role addressing is refused there.
  handler(){const h=JSON.parse(this.command([this.config.tt||"tt","deployment","handler"]));if(!/^agt_[a-f0-9]+$/.test(h?.id||""))throw new Error("Project database handler required");return h.id;}
  async requestBug({commit,outcome}){
    const id=`${this.job.id}-rollback-bug`;
    this.command([this.config.tt||"tt","send","--kind","request","--to",this.handler(),"--subject","File a bug for a release that was rolled back","--ask",`Release job ${this.job.id} was rolled back after publication. File a bug linked to item ${this.job.itemId} so the change is fixed before it ships again. The tasks-hub revert ${commit||"was not created"} is ${outcome}; the private host journal has the rest.`,"--request-id",id,"--work-item",this.job.itemId,"--work-item-revision",String(this.job.itemRevision),"--work-order-message",String(this.job.orderMessageSeq),"--ref",`release-job=${this.job.id}`,...(commit?["--ref",`revert-commit=${commit}`]:[])]);
    return id;
  }
  async merged(commit){this.native("merged",["--commit",commit]);}
  async verifyIntegrated(job){
    // Independent release verification is imported by the handler. It is not
    // satisfied by deployer self-certification or a candidate-SHA receipt.
    const current=JSON.parse(this.command([this.config.tt||"tt","deployment","list"])).find(j=>j.id===job.id);
    if(current?.integratedCommit===job.integratedCommit && current.integratedVerification?.commit===job.integratedCommit){this.job=current;return true;}
    // One directory per integrated commit and requeue attempt: a handler
    // requeue (a new reconciliation) never reuses an earlier attempt's host
    // wait or receipt, while a runner resumed within an attempt does.
    if(!sha(job.integratedCommit))throw new Error("Exact integrated commit required");
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
    save(contextPath,{...job.plan,...matrix,commit:job.integratedCommit,verifierAgentId:this.job.agentId,verifierRunId:this.job.runId});
    this.command(["node","scripts/verify-matrix.mjs","plan",contextPath,planPath]);
    const boundMs=matrixRunTimeout(JSON.parse(readFileSync(planPath,"utf8")));
    const run={version:1,state:"starting",launchedAt:this.now(),priority,hostWaitMs:minutes*60000,boundMs};
    // A failed intent save starts nothing and takes the ordinary refusal path.
    this.saveRun(dir,run);
    let pid;
    try{pid=this.startMatrixRun(["node","scripts/verify-matrix.mjs","run",planPath,dir,"--priority",priority,"--item",NAME(job.itemId??this.job.itemId),"--host-wait-minutes",String(minutes)],dir);if(!Number.isSafeInteger(pid)||pid<=0)throw new Error("No matrix run pid");}
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
      const child=spawn(argv[0],argv.slice(1),{cwd:this.config.cwd,detached:true,stdio:["ignore",out,err]});
      child.on("error",()=>{});
      if(!Number.isSafeInteger(child.pid))throw new Error("Matrix run not started");
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
  matrixEntry(dir){
    let state;try{state=this.hostState();}catch{return undefined;}
    if(!state)return null;
    const output=resolve(dir);
    if(state.holder?.output===output)return {state,role:"holder",entry:state.holder};
    const index=state.waiters.findIndex(w=>w?.output===output);
    return index<0?null:{state,role:"waiter",index,entry:state.waiters[index]};
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
    const now=this.now(),child=this.matrixChildren.get(dir),found=this.matrixEntry(dir),sidecar=this.matrixSidecar(dir);
    const persist=()=>{try{this.saveRun(dir,run);return true;}catch{return false;}};
    const alive=()=>[...new Set([...(run.snapshot?.groups||[]),...(run.groups||[])])].filter(g=>!this.groupGone(g));
    const held=reason=>{persist();this.matrixHeld={attempt:basename(dir),pid:run.pid??null,groups:run.state==="started"?alive():[],reason};return "held";};
    const end=(reason,refusal)=>{Object.assign(run,{state:"ended",reason,endedAt:now,...(refusal?{refusal}:{})});if(!persist())return "waiting";this.matrixChildren.delete(dir);return "ended";};
    // A run that was not stopped and left a receipt ended normally, whatever
    // the record knew of it; the receipt's eligibility is checked at import.
    const ending=()=>run.stopRequestedAt?end("stopped","Integrated matrix run exceeded its bound"):existsSync(join(dir,"receipt.json"))?end("receipt"):sidecar==="wait-expired"?end("wait-expired","Verification host wait expired"):end("no-receipt","Integrated matrix run ended without a receipt");
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
        const h=mine?.state.holder;
        if(mine?.role==="waiter"){
          const place=`${mine.index+1}/${mine.state.waiters.length}/${h?.id??""}`;
          if(run.waitPlace!==place){run.waitPlace=place;run.waitChange=(run.waitChange||0)+1;}
          this.matrixWait={attempt:basename(dir),position:mine.index+1,length:mine.state.waiters.length,priority:run.priority,change:run.waitChange,holder:h?{id:h.id,item:h.item,agent:h.agent,pid:h.pid}:null};
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
      const after=this.matrixEntry(dir),last=after && after.entry.pid===run.pid?after:null;
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
    const current=JSON.parse(this.command([this.config.tt||"tt","deployment","list"])).find(j=>j.id===this.job.id);
    if(current?.inputsCommit===commit && /^[a-f0-9]{64}$/.test(current.inputsDigest||"")){this.job=current;this.jobInputs(commit);return true;}
    this.command([this.config.tt||"tt","send","--kind","request","--to",this.handler(),"--subject","Import immutable inputs for this release job","--ask",`Prepare the private manifest ${join(this.config.journalDirectory,this.job.id+"-inputs.json")} for exact job ${this.job.id} accepted ${this.job.commit} integrated ${commit}; import its digest with tt deployment inputs --job --generation --commit --file. Include fresh exact-job backup/preflight pins and rollback programs; publication waits for saved handler input binding.`,"--request-id",`${this.job.id}-inputs-${commit}`,"--work-item",this.job.itemId,"--work-item-revision",String(this.job.itemRevision),"--work-order-message",String(this.job.orderMessageSeq),"--ref",`release-job=${this.job.id}`]);return false;
  }
  jobInputs(commit){
    const path=join(this.config.journalDirectory,this.job.id+"-inputs.json"),raw=readFileSync(path,"utf8");
    if(fileDigest(path)!==this.job.inputsDigest || this.job.inputsCommit!==commit)throw new Error("Handler input digest binding required");
    const input=JSON.parse(raw);
    if(input.version!==1 || input.jobId!==this.job.id || input.commit!==commit || input.acceptedCommit!==this.job.commit || input.verificationDigest!==this.job.verificationDigest)throw new Error("Exact job input binding required");
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
    if(git(this.config.cwd,"rev-parse","HEAD")!==commit)throw new Error("Candidate build checkout mismatch");
    const t=this.config.targets[target];if(!t)throw new Error("Target host config required");
    const input=this.jobInputs(commit),perJob=input.targets?.[target];if(!perJob)throw new Error("Exact job target input required");
    const planTargets=PAIR.includes(target)?perJob.planTargets||[target]:undefined,paired=same(planTargets,PAIR);
    const release=this.job.id+"-"+commit.slice(0,12)+"-"+(paired?"truenas":target);
    if(perJob.release!==release)throw releaseError("Unique job release identity required");
    // Stable config contains private probe/host references only. Backup, release,
    // compatibility and rollback inputs come from the handler-pinned job manifest.
    const artifact={installPath:t.installPath,relayRestart:t.relayRestart,liveProbe:t.liveProbe,rollbackProbe:t.rollbackProbe,...perJob,release,commit,rollbackSafe:perJob.rollbackSafe===true,...(planTargets?{planTargets}:{})};
    const baselines=this.baselines||this.config.baselines;
    artifact.schemaChanged=["hub","bridge"].includes(target) && schemaChanged(this.config.cwd,baselines.hub,commit);
    if(target==="tailos"){
      if(diffPaths(this.config.cwd,baselines.tailos,commit).includes("package-lock.json"))this.command(["npm","ci"]);
      this.command(["npm","run","build:static"]);this.command(["npm","run","verify:release"]);
      const manifest=JSON.parse(readFileSync(join(this.config.cwd,"dist-static/release.json"),"utf8"));
      if(manifest.commit!==commit)throw new Error("Static commit mismatch");
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
      if(perJob.backupJobId!==this.job.id || !perJob.backup || !perJob.backup.includes(this.job.id))throw new Error("Fresh job backup identity required");
      artifact.migrationBinary=join(this.config.journalDirectory,this.job.id+"-migration");
      withBuildCheckout(this.config.cwd,commit,src=>this.command(["env","CGO_ENABLED=0",`GOOS=${process.platform}`,`GOARCH=${process.arch==="arm64"?"arm64":"amd64"}`,"go","build","-trimpath","-o",artifact.migrationBinary,"./cmd/tailterm-hub"],join(src,"hub")));
      this.requireStamp(artifact.migrationBinary,commit);
      artifact.migrationBinarySHA256=fileDigest(artifact.migrationBinary);
    }
    if(["hub","bridge"].includes(target)){
      if(perJob.backupJobId!==this.job.id || !perJob.backup?.includes(this.job.id))throw new Error("Fresh job backup identity required");
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
    let info;try{info=buildInfo({run:argv=>({status:0,stdout:this.command(argv)})},path);}catch{throw new Error("Go artifact has no build revision");}
    if(info.commit!==commit || info.integrity!==true)throw new Error("Go artifact revision does not match the integrated commit");
  }
  async rehearse(a){
    if(!a.backupCopy || !a.migrationBinary)throw new Error("Handler backup-copy import required");
    if(fileDigest(a.backupCopy)!==a.backupSHA256)throw new Error("Imported backup hash mismatch");
    // The migrated copy is removed when the rehearsal ends, pass or fail. The
    // marker beside it, not the copy, refuses a second attempt; it is saved
    // before the copy exists and holds no path, output or error text.
    const copy=a.backupCopy+".rehearsal-"+this.job.id,marker=copy+".json";
    if(existsSync(marker))throw new Error("Rehearsal already attempted; inspect prior attempt");
    const record={version:1,jobId:this.job.id,backupSHA256:a.backupSHA256,startedAt:new Date(this.now()).toISOString(),outcome:"started"};
    save(marker,record);
    const clear=()=>{let gone=true;for(const suffix of COPY_FILES){try{rmSync(copy+suffix,{force:true});}catch{gone=false;}}return gone;};
    let outcome="failed";
    try{
      // A copy an earlier run left behind is replaced, sidecars included.
      clear();copyFileSync(a.backupCopy,copy);
      if(fileDigest(a.migrationBinary)!==a.migrationBinarySHA256)throw new Error("Candidate migration binary changed");
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
      const {copyFileSync,chmodSync}=await import("node:fs");
      if(!a.rollbackCaptured || fileDigest(a.rollbackPath)!==a.priorArtifactSHA256 || fileDigest(a.installPath)!==a.priorArtifactSHA256)throw new Error("Exact prior-live Mini rollback required");
      copyFileSync(a.artifactPath,a.installPath+".next");chmodSync(a.installPath+".next",0o755);renameSync(a.installPath+".next",a.installPath);
      // The live probe counts relay errors only after this deploy.
      const log=this.config.targets?.mini?.relayLog;if(log)save(join(this.config.journalDirectory,"mini-relay-offset.json"),{jobId:this.job.id,offset:existsSync(log)?statSync(log).size:0});
      this.command(a.relayRestart);
    }else{
      const output=this.command(["npx","wrangler","pages","deploy","dist-static","--project-name","tailos","--branch","main","--commit-hash",a.commit,"--commit-dirty=false"]);
      // Output is kept out of messages/receipts; a host probe supplies the
      // immutable deployment ID only after independently resolving it.
      const match=output.match(/https:\/\/([a-f0-9]{8})\.tailos\.pages\.dev/);if(!match)throw new Error("TailOS deployment identity unconfirmed");a.deployment=match[0];
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
      const {copyFileSync}=await import("node:fs");copyFileSync(a.rollbackPath,a.installPath+".rollback");renameSync(a.installPath+".rollback",a.installPath);this.command(a.relayRestart);
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
      return this.command([this.config.tt||"tt","send","--kind","notice","--subject","Release refused before publication","--text",`Release ${this.job.id} was refused before publication; nothing was published or deployed. Reason: ${reason}. Handler reconciliation required.`,"--request-id",`${this.job.id}-release-failure`,"--ref",`release-job=${this.job.id}`]);
    }
    if(details.push==="failed")return this.command([this.config.tt||"tt","send","--kind","notice","--subject","Release is live but the tasks-hub push failed","--text",`Release ${this.job.id} is live and verified, but the fast-forward push of tasks-hub to origin failed. Live targets were not rolled back; inspect the remote and push tasks-hub by hand.`,"--request-id",`${this.job.id}-push-failure`,"--ref",`release-job=${this.job.id}`]);
    return this.command([this.config.tt||"tt","send","--kind","notice","--subject","Release failed and requires recovery","--text",`Release failed for ${this.job.id}; inspect the private host journal. Automatic rollback attempted once; handler reconciliation required.${details.revert==="failed"?" The tasks-hub revert failed, so the rolled-back change is still on tasks-hub.":""}${details.rollbackBlocked===true&&details.revert==="committed"?" tasks-hub was reverted, but at least one target could not be rolled back and still runs the released code; roll it back by hand before the next release.":""}${waits}`,"--request-id",`${this.job.id}-release-failure`,"--ref",`release-job=${this.job.id}`]);
  }
}

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
export function reconcileReceipts(config,jobs){
  for(const job of jobs){
    if(!job.receipt)continue;
    const path=join(config.journalDirectory,job.id+".json");if(!existsSync(path))continue;
    const journal=JSON.parse(readFileSync(path,"utf8"));
    if(["finishing","receipt_pending"].includes(journal.phase) && journal.jobId===job.id && digest(journal.receipt)===digest(job.receipt)){journal.phase="complete";save(path,journal);}
  }
}
// Journal retention. The imported backup copy is read only by its job's
// rehearsal (the authoritative backup stays on TrueNAS), so the journal keeps
// it for every job that is not terminal and for the newest released jobs.
const COPY_FILES=["","-wal","-shm","-journal"],BACKUP_NAMES=["truenas","hub","bridge"],TERMINAL=["released","rolled_back","refused","superseded"];
export function retentionPolicy(config){
  const retention=config?.retention??{};
  if(typeof retention!=="object" || Array.isArray(retention))throw new Error("Invalid journal retention");
  const value=(key,fallback)=>{const v=retention[key]===undefined?fallback:retention[key];if(!Number.isSafeInteger(v) || v<0)throw new Error("Invalid journal retention "+key);return v;};
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
  if(!targets.every(t=>sha(b?.[t])))throw new Error("Four last-successful baselines required");
  return Object.fromEntries(targets.map(t=>[t,b[t]]));
}
export async function serveDeployment(config,{once=false,signal,configPath,release=runRelease}={}) {
  if(config.version!==1 || config.enabled!==true || !config.cwd || !config.journalDirectory)throw new Error("Explicit private activation config required");
  tailosWindow(config);readyWindow(config,"hub");readyWindow(config,"bridge");
  retentionPolicy(config);
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
  while(!signal?.aborted){
    try {
    // An unreadable or invalid edit holds the whole poll, before any claim.
    const configured=configPath?readBaselines(configPath):config.baselines;
    const reader=new HostAdapter(config,{});
    const jobs=JSON.parse(reader.command([config.tt||"tt","deployment","list"]));
    reconcileReceipts(config,jobs);reconcileHostLocks(config,jobs);
    try{pruneJournal(config,jobs);}catch{process.stderr.write("Journal retention sweep failed; remaining backup copies kept.\n");}
    const skipped=new Set();
    for (;;) {
      const job=runnableJob(jobs,process.env.TAILTERM_AGENT,process.env.TAILTERM_RUN,skipped);
      if(!job){
        const holder=jobs.find(j=>["claimed","merged","blocked"].includes(j.state));
        if(holder)notify(reader,jobs,holder,holder.state);
        break;
      }
      // Archived only while unclaimed, so a later crash of the new claim
      // leaves its own journal alone.
      if(job.state==="verified"){
        let journal;try{journal=setAsideJournal(join(config.journalDirectory,job.id+".json"),job);}catch{journal="held";}
        if(journal==="held"){skipped.add(job.id);process.stderr.write("Set-aside release journal held; handler reconciliation required.\n");continue;}
      }
      const adapter=new HostAdapter(config,job);
      try{if(job.state==="verified")adapter.native("claim");}
      catch{skipped.add(job.id);process.stderr.write("Release claim held; handler reconciliation required.\n");continue;}
      const current=adapter.job,baselines=releaseBaselines(configured,jobs);adapter.baselines=baselines;
      const {testPolicy,sleep,now,...activation}=config;
      let result;
      try{result=await release({...activation,baselines,job:current,journalPath:join(config.journalDirectory,current.id+".json")},adapter);}catch{process.stderr.write("Release held; inspect handler fence and private journal.\n");}
      if(["waiting_matrix","waiting_inputs"].includes(result?.outcome))notify(reader,jobs,adapter.job||current,result.outcome);
      matrixNotify(reader,adapter.job||current,adapter);
      break;
    }
    } catch {process.stderr.write("Deployment poll held; inspect native input or recovery evidence.\n");}
    if(once)return;
    await new Promise(r=>setTimeout(r,30000));
  }
}
if(process.argv[1] && resolve(process.argv[1])===fileURLToPath(import.meta.url) && process.argv.includes("--provision-prerequisites")){
  // tt deployment setup runs this in the deployer's checkout. Only the tagged
  // reason is printed on failure.
  const index=process.argv.indexOf("--from");
  try{process.stdout.write(JSON.stringify(provisionPrerequisites(process.cwd(),{from:index<0?undefined:process.argv[index+1]}))+"\n");}
  catch(error){process.stderr.write(failureReason(error)+"\n");process.exitCode=1;}
}else if(process.argv[1] && resolve(process.argv[1])===fileURLToPath(import.meta.url)){
  const index=process.argv.indexOf("--config");
  try{
    if(index<0)throw new Error("Private config required");
    const config=JSON.parse(readFileSync(process.argv[index+1],"utf8"));
    const controller=new AbortController();process.on("SIGTERM",()=>controller.abort());process.on("SIGINT",()=>controller.abort());
    await serveDeployment(config,{once:process.argv.includes("--once"),signal:controller.signal,configPath:process.argv[index+1]});
  }catch{process.stderr.write("Deployment blocked; inspect private host journal and handler release gate.\n");process.exitCode=1;}
}
