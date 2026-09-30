import { createHash } from "node:crypto";
import { tmpdir } from "node:os";
import { fileURLToPath } from "node:url";
import { execFileSync } from "node:child_process";
import { openSync, closeSync, fsyncSync, writeFileSync, readFileSync, renameSync, mkdirSync, mkdtempSync, existsSync, rmSync, copyFileSync, cpSync, chmodSync, statSync, constants } from "node:fs";
import { join, resolve, dirname } from "node:path";
import { digest, diffPaths } from "./verify-matrix.mjs";
import { selectReleaseTargets, releaseBaselines, schemaChanged } from "./release-targets.mjs";
import { buildInfo } from "./release-probe.mjs";

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
    if (r==="identity" || r==="integrity") return false;
    if(now()>=until && ++failures>=policy.failures)return false;
    await sleep(policy.intervalMs);
  }
}
// Adapter operations receive nonsecret job/artifact references. Neither captured
// subprocess output nor arbitrary error strings are put into journal/receipt;
// a failed target step records only the tagged reason failureReason allows.
export async function runRelease(config, adapter) {
  const {cwd,job,baselines,journalPath}=config;
  const policy={startupMs:60000,failures:3,intervalMs:5000,relayCleanMs:30000,...config.testPolicy};
  if(!job || !(["claimed","merged"].includes(job.state)) || !sha(job.commit) || !/^[a-f0-9]{64}$/.test(job.verificationDigest||""))throw new Error("Claimed verified job required");
  const lock=join(tmpdir(),"tailterm-release-locks",digest(cwd+"\0"+(job.taskId||"fixture"))+".lock");mkdirSync(dirname(lock),{recursive:true,mode:0o700});
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
    checkpoint();await fence();
    const integration=["waiting_matrix","waiting_inputs"].includes(state.phase)?{expected:state.expected,integrated:state.integrated}:integrateCandidate(cwd,job);state.integrated=integration.integrated;state.expected=integration.expected;state.phase="integrated";checkpoint();
    if(integration.integrated!==job.commit){
      await fence();if(await adapter.verifyIntegrated({...job,integratedCommit:integration.integrated})!==true){state.phase="waiting_matrix";checkpoint();return {jobId:job.id,outcome:"waiting_matrix"};}
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
        if(!await liveCheck(adapter,t,policy,config.sleep,config.now))throw releaseError("live verification failed");
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
      state.phase="refusing";checkpoint();await adapter.refuse();state.phase="refused";checkpoint();await adapter.escalate({jobId:job.id,outcome:"refused"});throw new Error("Release refused before publication");
    }
    let blocked=state.effects.length===0;
    for(const effect of [...state.effects].reverse()){
      if(effect.rollbackAttempted){blocked=true;continue;}
      effect.rollbackAttempted=true;checkpoint();
      let restored=false;
      try{await fence();restored=await adapter.rollback(effect.target,effect.release)===true;}catch{}
      blocked ||= !restored;effect.rollback=restored?"restored":"blocked";checkpoint();
    }
    state.phase="blocked";state.outcome=blocked?"blocked":"rolled_back";checkpoint();
    if(state.published && !state.revert){
      state.revert={outcome:"failed"};checkpoint();
      try{state.revert.commit=revertCommit(cwd,job,state.integrated,state.expected);checkpoint();if(moveReleaseRef(cwd,state.revert.commit,state.integrated))state.revert.outcome="committed";}catch{}
      checkpoint();
    }
    // One attempt; a failed post must not stop the bug request or the receipt.
    if(!state.escalationAttempted){state.escalationAttempted=true;checkpoint();try{await adapter.escalate({jobId:job.id,outcome:state.outcome,...(state.revert?{revert:state.revert.outcome}:{}),...(state.effects.some(e=>e.rollback==="blocked")?{rollbackBlocked:true}:{})});}catch{}}
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
  constructor(config,job){this.config=config;this.job=job;this.artifacts=new Map();this.serial=0;}
  command(argv,cwd=this.config.cwd){
    if(!Array.isArray(argv)||!argv.length||argv.some(a=>typeof a!=="string"||/[\0\r\n]/.test(a)))throw new Error("Invalid host operation argv");
    try{return execFileSync(argv[0],argv.slice(1),{cwd,encoding:"utf8",stdio:["ignore","pipe","pipe"],maxBuffer:8*1024*1024,timeout:600000});}
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
    const dir=join(this.config.journalDirectory,job.id+"-integrated-verification");mkdirSync(dir,{recursive:true,mode:0o700});
    const contextPath=join(dir,"context.json"),planPath=join(dir,"plan.json"),receiptPath=join(dir,"receipt.json");
    if(!existsSync(receiptPath)){
      save(contextPath,{...job.plan,commit:job.integratedCommit,verifierAgentId:this.job.agentId,verifierRunId:this.job.runId});
      this.command(["node","scripts/verify-matrix.mjs","plan",contextPath,planPath]);
      this.command(["node","scripts/verify-matrix.mjs","run",planPath,dir]);
    }
    this.command([this.config.tt||"tt","send","--kind","request","--to",this.handler(),"--subject","Import verification for the integrated release commit","--ask",`Import release verification plan and receipt for job ${job.id} integrated commit ${job.integratedCommit} through tt deployment verification --plan-file and --file. Preserve exact job generation and inspect logs; release publication waits for saved import.`,"--request-id",`${job.id}-integrated-matrix-${job.integratedCommit}`,"--ref",`release-job=${job.id}`,"--ref",`integrated-commit=${job.integratedCommit}`,"--attachment",planPath,"--attachment",receiptPath]);
    return false;
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
    const {copyFileSync}=await import("node:fs");const copy=a.backupCopy+".rehearsal-"+this.job.id;
    if(existsSync(copy))throw new Error("Rehearsal copy already exists; inspect prior attempt");copyFileSync(a.backupCopy,copy);
    if(fileDigest(a.migrationBinary)!==a.migrationBinarySHA256)throw new Error("Candidate migration binary changed");
    this.command([a.migrationBinary,"--migrate-only",copy]);
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
    if(target==="tailos"){
      try{const response=await fetch("https://tailos.tailarr.com/release.json",{signal:AbortSignal.timeout(10000),cache:"no-store"});if(!response.ok)return false;const r=await response.json();return r.commit===a.commit?true:"identity";}catch{return false;}
    }
    try{
      const r=JSON.parse(this.command(a.liveProbe));
      if(r.commit!==a.commit || r.artifactSHA256!==a.artifactSHA256)return "identity";
      if(r.integrity!==true)return "integrity";
      if(target==="mini")return r.relayRunning===true && r.newErrors===0;
      return r.hubResponds===true && r.migrationsApplied===true && r.containersRunning===true;
    }catch{return false;}
  }
  async rollback(target){
    const a=this.artifacts.get(target);if(!a || a.rollbackSafe!==true)return false;
    if(target==="mini"){
      if(!a.rollbackCaptured || fileDigest(a.rollbackPath)!==a.priorArtifactSHA256)return false;
      const {copyFileSync}=await import("node:fs");copyFileSync(a.rollbackPath,a.installPath+".rollback");renameSync(a.installPath+".rollback",a.installPath);this.command(a.relayRestart);
    }else{this.command(a.rollbackProgram);}
    const r=JSON.parse(this.command(a.rollbackProbe));return r.restored===true && r.databaseWritesPreserved===true;
  }
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
    if(details.push==="failed")return this.command([this.config.tt||"tt","send","--kind","notice","--subject","Release is live but the tasks-hub push failed","--text",`Release ${this.job.id} is live and verified, but the fast-forward push of tasks-hub to origin failed. Live targets were not rolled back; inspect the remote and push tasks-hub by hand.`,"--request-id",`${this.job.id}-push-failure`,"--ref",`release-job=${this.job.id}`]);
    return this.command([this.config.tt||"tt","send","--kind","notice","--subject","Release failed and requires recovery","--text",`Release failed for ${this.job.id}; inspect the private host journal. Automatic rollback attempted once; handler reconciliation required.${details.revert==="failed"?" The tasks-hub revert failed, so the rolled-back change is still on tasks-hub.":""}${details.rollbackBlocked===true&&details.revert==="committed"?" tasks-hub was reverted, but at least one target could not be rolled back and still runs the released code; roll it back by hand before the next release.":""}`,"--request-id",`${this.job.id}-release-failure`,"--ref",`release-job=${this.job.id}`]);
  }
}

export function runnableJob(jobs,agent,run,skipped=new Set()){
  const owned=jobs.find(j=>["claimed","merged"].includes(j.state) && j.agentId===agent && j.runId===run);
  if(owned)return owned;
  if(jobs.some(j=>["claimed","merged","blocked"].includes(j.state)))return null;
  return jobs.find(j=>j.state==="verified" && !skipped.has(j.id))||null;
}
export function reconcileHostLocks(config,jobs){
  for(const job of jobs){
    const r=job.reconciliations?.at(-1);
    if(!r?.lockDigest || !r.noActiveExecution || !r.refResolved || !["no_effects","restored"].includes(r.journalState) || !["verified","refused"].includes(job.state))continue;
    const lock=join(tmpdir(),"tailterm-release-locks",digest(config.cwd+"\0"+(job.taskId||"fixture"))+".lock");
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
// The four last-successful baselines, read from the private config at every
// poll so an operator's edit after a hand release applies to the next pass
// without restarting the daemon. Only baselines are re-read: cwd, journal and
// host references stay those the daemon started with.
export function readBaselines(configPath){
  const b=JSON.parse(readFileSync(configPath,"utf8")).baselines,targets=["hub","bridge","mini","tailos"];
  if(!targets.every(t=>sha(b?.[t])))throw new Error("Four last-successful baselines required");
  return Object.fromEntries(targets.map(t=>[t,b[t]]));
}
export async function serveDeployment(config,{once=false,signal,configPath,release=runRelease}={}) {
  if(config.version!==1 || config.enabled!==true || !config.cwd || !config.journalDirectory)throw new Error("Explicit private activation config required");
  while(!signal?.aborted){
    try {
    // An unreadable or invalid edit holds the whole poll, before any claim.
    const configured=configPath?readBaselines(configPath):config.baselines;
    const reader=new HostAdapter(config,{});
    const jobs=JSON.parse(reader.command([config.tt||"tt","deployment","list"]));
    reconcileReceipts(config,jobs);reconcileHostLocks(config,jobs);
    const skipped=new Set();
    for (;;) {
      const job=runnableJob(jobs,process.env.TAILTERM_AGENT,process.env.TAILTERM_RUN,skipped);
      if(!job)break;
      const adapter=new HostAdapter(config,job);
      try{if(job.state==="verified")adapter.native("claim");}
      catch{skipped.add(job.id);process.stderr.write("Release claim held; handler reconciliation required.\n");continue;}
      const current=adapter.job,baselines=releaseBaselines(configured,jobs);adapter.baselines=baselines;
      const {testPolicy,sleep,now,...activation}=config;
      try{await release({...activation,baselines,job:current,journalPath:join(config.journalDirectory,current.id+".json")},adapter);}catch{process.stderr.write("Release held; inspect handler fence and private journal.\n");}
      break;
    }
    } catch {process.stderr.write("Deployment poll held; inspect native input or recovery evidence.\n");}
    if(once)return;
    await new Promise(r=>setTimeout(r,30000));
  }
}
if(process.argv[1] && resolve(process.argv[1])===fileURLToPath(import.meta.url)){
  const index=process.argv.indexOf("--config");
  try{
    if(index<0)throw new Error("Private config required");
    const config=JSON.parse(readFileSync(process.argv[index+1],"utf8"));
    const controller=new AbortController();process.on("SIGTERM",()=>controller.abort());process.on("SIGINT",()=>controller.abort());
    await serveDeployment(config,{once:process.argv.includes("--once"),signal:controller.signal,configPath:process.argv[index+1]});
  }catch{process.stderr.write("Deployment blocked; inspect private host journal and handler release gate.\n");process.exitCode=1;}
}
