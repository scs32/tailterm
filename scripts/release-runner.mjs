import { createHash } from "node:crypto";
import { tmpdir } from "node:os";
import { fileURLToPath } from "node:url";
import { execFileSync } from "node:child_process";
import { openSync, closeSync, fsyncSync, writeFileSync, readFileSync, renameSync, mkdirSync, existsSync, rmSync } from "node:fs";
import { join, resolve, dirname } from "node:path";
import { digest, diffPaths } from "./verify-matrix.mjs";
import { selectReleaseTargets } from "./release-targets.mjs";

const fileDigest = p => createHash("sha256").update(readFileSync(p)).digest("hex");
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
export function publishIntegration(cwd, integrated, expected) {
  if (!sha(integrated)||!sha(expected)||git(cwd,"rev-parse","HEAD")!==integrated||git(cwd,"status","--porcelain")) throw new Error("Integrated checkout changed");
  try {git(cwd,"update-ref","refs/heads/tasks-hub",integrated,expected);}catch{throw new Error("Release ref race");}
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
// subprocess output nor arbitrary error strings are put into journal/receipt.
export async function runRelease(config, adapter) {
  const {cwd,job,baselines,journalPath}=config;
  const policy={startupMs:60000,failures:3,intervalMs:5000,relayCleanMs:30000,...config.testPolicy};
  if(!job || !(["claimed","merged"].includes(job.state)) || !sha(job.commit) || !/^[a-f0-9]{64}$/.test(job.verificationDigest||""))throw new Error("Claimed verified job required");
  const lock=join(tmpdir(),"tailterm-release-locks",digest(cwd+"\0"+(job.taskId||"fixture"))+".lock");mkdirSync(dirname(lock),{recursive:true,mode:0o700});
  let fd;try{fd=openSync(lock,"wx",0o600);}catch{throw new Error("Release host locked; inspect prior execution");}
  let state={version:1,jobId:job.id,commit:job.commit,phase:"prepared",effects:[]};
  if(existsSync(journalPath)){
    const prior=JSON.parse(readFileSync(journalPath,"utf8"));
    if(prior.jobId===job.id && prior.commit===job.commit && (prior.phase==="waiting_matrix" || prior.phase==="receipt_pending" || prior.phase==="finishing") && (prior.phase!=="waiting_matrix" || !prior.effects.length) && git(cwd,"rev-parse","HEAD")===prior.integrated && !git(cwd,"status","--porcelain")) {state=prior;}
    else {
      closeSync(fd);rmSync(lock);
      if(prior.jobId===job.id && prior.commit===job.commit && prior.phase==="complete") return prior.receipt;
      throw new Error("Ambiguous journal requires handler reconciliation");
    }
  }
  const checkpoint=()=>save(journalPath,state);
  const fence=async()=>{if(await adapter.fence(job)!==true)throw new Error("Exact release fence lost");};
  try {
    if(state.phase==="receipt_pending" || state.phase==="finishing"){
      await adapter.finish(state.receipt,state.finishGeneration);state.phase="complete";checkpoint();return state.receipt;
    }
    checkpoint();await fence();
    const integration=state.phase==="waiting_matrix"?{expected:state.expected,integrated:state.integrated}:integrateCandidate(cwd,job);state.integrated=integration.integrated;state.expected=integration.expected;state.phase="integrated";checkpoint();
    if(integration.integrated!==job.commit){
      await fence();if(await adapter.verifyIntegrated({...job,integratedCommit:integration.integrated})!==true){state.phase="waiting_matrix";checkpoint();return {jobId:job.id,outcome:"waiting_matrix"};}
    }
    await fence();publishIntegration(cwd,integration.integrated,integration.expected);
    await adapter.merged(integration.integrated);state.phase="merged";checkpoint();
    const selected=selectReleaseTargets(cwd,baselines,integration.integrated);
    const receipt={version:1,jobId:job.id,commit:integration.integrated,verificationDigest:job.verificationDigest,targets:[],outcome:"released"};
    state.receipt=receipt;checkpoint();
    for(const target of selected){
      await fence();const artifact=await adapter.prepare(target,integration.integrated);
      if(!artifact || !/^[a-f0-9]{64}$/.test(artifact.artifactSHA256||"") || !artifact.release)throw new Error("Pinned artifact required");
      if(["hub","bridge"].includes(target)){
        if(!artifact.backup || !/^[a-f0-9]{64}$/.test(artifact.backupSHA256||"") || !/^[a-f0-9]{64}$/.test(artifact.preflightReceiptSHA256||""))throw new Error("Handler backup and external pin required");
        if(artifact.schemaChanged){await fence();if(await adapter.rehearse(artifact)!==true)throw new Error("Backup-copy migration rehearsal failed");}
      }
      const record={target,release:artifact.release,artifactSHA256:artifact.artifactSHA256,...(artifact.backup?{backup:artifact.backup,backupSHA256:artifact.backupSHA256,preflightReceiptSHA256:artifact.preflightReceiptSHA256}:{}),...(artifact.version?{version:artifact.version}:{}),...(artifact.deployment?{deployment:artifact.deployment}:{}),outcome:"failed"};
      receipt.targets.push(record);state.effects.push({target,release:artifact.release,state:"attempting"});checkpoint();
      await fence();await adapter.deploy(target,artifact);if(artifact.deployment)record.deployment=artifact.deployment;if(artifact.version)record.version=artifact.version;state.effects.at(-1).state="deployed";checkpoint();
      if(!await liveCheck(adapter,target,policy,config.sleep,config.now))throw new Error("Live verification failed");
      record.outcome="released";state.effects.at(-1).state="verified";checkpoint();
    }
    state.receipt=receipt;state.phase="finishing";checkpoint();await fence();state.finishGeneration=adapter.job?.generation??job.generation;checkpoint();await adapter.finish(receipt,state.finishGeneration);state.phase="complete";checkpoint();return receipt;
  } catch {
    if(state.phase==="finishing" || state.phase==="receipt_pending"){
      state.phase="receipt_pending";checkpoint();return {jobId:job.id,outcome:"receipt_pending"};
    }
    // An uncertain side effect cannot be replayed. Rollback uses only retained
    // target artifacts; the adapter must never restore an old live database.
    let blocked=state.effects.length===0;
    for(const effect of [...state.effects].reverse()){
      if(effect.rollbackAttempted){blocked=true;continue;}
      effect.rollbackAttempted=true;checkpoint();
      try{await fence();if(await adapter.rollback(effect.target,effect.release)!==true)blocked=true;}catch{blocked=true;}
      effect.rollback=blocked?"blocked":"restored";checkpoint();
    }
    state.phase="blocked";state.outcome=blocked?"blocked":"rolled_back";checkpoint();
    if(!state.escalationAttempted){state.escalationAttempted=true;checkpoint();await adapter.escalate({jobId:job.id,outcome:state.outcome});}
    if(state.receipt){
      state.receipt.outcome=state.outcome;
      for(const target of state.receipt.targets){const effect=state.effects.find(e=>e.target===target.target);if(effect?.rollback){target.rollback=effect.rollback;target.outcome=effect.rollback==="restored"?"rolled_back":"failed";}}
      checkpoint();await adapter.finish(state.receipt);
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
    try{return execFileSync(argv[0],argv.slice(1),{cwd,encoding:"utf8",stdio:["ignore","pipe","pipe"],maxBuffer:8*1024*1024,timeout:600000});}catch{throw new Error("Host operation failed");}
  }
  native(operation,extra=[],expectedGeneration=this.job.generation){
    const args=[this.config.tt||"tt","deployment",operation,"--job",this.job.id,"--generation",String(expectedGeneration),"--request-id",`${this.job.id}-${operation}-${expectedGeneration}`,...extra];
    this.job=JSON.parse(this.command(args));return this.job;
  }
  async fence(){try{this.native("check");return true;}catch{return false;}}
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
    this.command([this.config.tt||"tt","send","--kind","request","--to","db-handler","--subject","Import verification for the integrated release commit","--ask",`Import release verification plan and receipt for job ${job.id} integrated commit ${job.integratedCommit} through tt deployment verification --plan-file and --file. Preserve exact job generation and inspect logs; release publication waits for saved import.`,"--request-id",`${job.id}-integrated-matrix-${job.integratedCommit}`,"--ref",`release-job=${job.id}`,"--ref",`integrated-commit=${job.integratedCommit}`,"--attachment",planPath,"--attachment",receiptPath]);
    return false;
  }
  async prepare(target,commit){
    const t=this.config.targets[target];if(!t)throw new Error("Target host config required");
    const artifact={...t,release:t.release,commit};
    if(target==="tailos"){
      this.command(["npm","run","build:static"]);this.command(["npm","run","verify:release"]);
      const manifest=JSON.parse(readFileSync(join(this.config.cwd,"dist-static/release.json"),"utf8"));
      if(manifest.commit!==commit)throw new Error("Static commit mismatch");
      artifact.artifactSHA256=digest(readFileSync(join(this.config.cwd,"dist-static/release.json"),"utf8"));
    }else{
      const output=resolve(this.config.cwd,t.artifactPath);mkdirSync(dirname(output),{recursive:true,mode:0o700});
      const pkg={hub:"tailterm-hub",bridge:"tailterm-discord",mini:"tt"}[target];
      const os=target==="mini"?"darwin":"linux",arch=target==="mini"?"arm64":"amd64";
      this.command(["env","CGO_ENABLED=0",`GOOS=${os}`,`GOARCH=${arch}`,"go","build","-trimpath","-ldflags=-s -w","-o",output,`./cmd/${pkg}`],join(this.config.cwd,"hub"));
      artifact.artifactPath=output;artifact.artifactSHA256=fileDigest(output);if(target==="mini")artifact.version=commit;
    }
    this.artifacts.set(target,artifact);return artifact;
  }
  async rehearse(a){
    if(!a.backupCopy || !a.migrationBinary)throw new Error("Handler backup-copy import required");
    if(fileDigest(a.backupCopy)!==a.backupSHA256)throw new Error("Imported backup hash mismatch");
    const {copyFileSync}=await import("node:fs");const copy=a.backupCopy+".rehearsal-"+this.job.id;
    if(existsSync(copy))throw new Error("Rehearsal copy already exists; inspect prior attempt");copyFileSync(a.backupCopy,copy);
    this.command([a.migrationBinary,"--migrate-only",copy]);
    return true;
  }
  async deploy(target,a){
    if(target==="hub" || target==="bridge"){
      const plan=JSON.parse(readFileSync(a.planPath,"utf8"));
      if(JSON.stringify(plan.deployment.targets)!==JSON.stringify([target]))throw new Error("Exact per-target middleware plan required");
      this.command(["python3","scripts/deploy-truenas-hub.py",a.release,"--plan",a.planPath,"--preflight-receipt",a.preflightReceipt,"--preflight-receipt-sha256",a.preflightReceiptSHA256,"--update"]);
    }else if(target==="mini"){
      const {copyFileSync,chmodSync}=await import("node:fs");
      if(!a.installPath || !a.rollbackPath || existsSync(a.rollbackPath))throw new Error("Fresh Mini rollback path required");
      copyFileSync(a.installPath,a.rollbackPath);copyFileSync(a.artifactPath,a.installPath+".next");chmodSync(a.installPath+".next",0o755);renameSync(a.installPath+".next",a.installPath);
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
      const {copyFileSync}=await import("node:fs");copyFileSync(a.rollbackPath,a.installPath+".rollback");renameSync(a.installPath+".rollback",a.installPath);this.command(a.relayRestart);
    }else{this.command(a.rollbackProgram);}
    const r=JSON.parse(this.command(a.rollbackProbe));return r.restored===true && r.databaseWritesPreserved===true;
  }
  async finish(receipt,expectedGeneration){const path=join(this.config.journalDirectory || dirname(this.config.journalPath),this.job.id+"-receipt.json");save(path,receipt);this.native("finish",["--file",path],expectedGeneration);}
  async block(){this.native("block");}
  async escalate(){
    return this.command([this.config.tt||"tt","send","--kind","notice","--subject","Release failed and requires recovery","--text",`Release failed for ${this.job.id}; inspect the private host journal. Automatic rollback attempted once; handler reconciliation required.`,"--request-id",`${this.job.id}-release-failure`,"--ref",`release-job=${this.job.id}`]);
  }
}

export function runnableJob(jobs,agent,run){
  const owned=jobs.find(j=>["claimed","merged"].includes(j.state) && j.agentId===agent && j.runId===run);
  if(owned)return owned;
  if(jobs.some(j=>["claimed","merged","blocked"].includes(j.state)))return null;
  return jobs.find(j=>j.state==="verified")||null;
}
export function reconcileReceipts(config,jobs){
  for(const job of jobs){
    if(!job.receipt)continue;
    const path=join(config.journalDirectory,job.id+".json");if(!existsSync(path))continue;
    const journal=JSON.parse(readFileSync(path,"utf8"));
    if(["finishing","receipt_pending"].includes(journal.phase) && journal.jobId===job.id && digest(journal.receipt)===digest(job.receipt)){journal.phase="complete";save(path,journal);}
  }
}
export async function serveDeployment(config,{once=false,signal}={}) {
  if(config.version!==1 || config.enabled!==true || !config.cwd || !config.journalDirectory)throw new Error("Explicit private activation config required");
  while(!signal?.aborted){
    const reader=new HostAdapter(config,{});
    const jobs=JSON.parse(reader.command([config.tt||"tt","deployment","list"]));
    reconcileReceipts(config,jobs);
    const job=runnableJob(jobs,process.env.TAILTERM_AGENT,process.env.TAILTERM_RUN);
    if(job){
      const adapter=new HostAdapter(config,job);
      if(job.state==="verified")adapter.native("claim");
      const current=adapter.job;
      const baselines={...config.baselines};
      for(const released of jobs){if(released.receipt?.outcome!=="released")continue;for(const t of released.receipt.targets){if(t.outcome==="released")baselines[t.target]=released.receipt.commit;}}
      const {testPolicy,sleep,now,...activation}=config;
      try{await runRelease({...activation,baselines,job:current,journalPath:join(config.journalDirectory,current.id+".json")},adapter);}catch{process.stderr.write("Release held; inspect handler fence and private journal.\n");}
    }
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
    await serveDeployment(config,{once:process.argv.includes("--once"),signal:controller.signal});
  }catch{process.stderr.write("Deployment blocked; inspect private host journal and handler release gate.\n");process.exitCode=1;}
}
