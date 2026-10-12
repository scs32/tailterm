import test from "node:test";
import assert from "node:assert/strict";
import {mkdtempSync,mkdirSync,copyFileSync,writeFileSync,readFileSync,chmodSync,existsSync,readdirSync,rmSync,realpathSync,utimesSync} from "node:fs";
import {tmpdir} from "node:os";
import {join,dirname,resolve} from "node:path";
import {execFileSync,spawn,spawnSync} from "node:child_process";
import {integrateCandidate,runRelease,liveCheck,HostAdapter,serveDeployment,pruneJournal,failureReason,failureDetail,MATRIX_PREREQUISITES,MATRIX_DEADLINE_SLACK_MS,RUNNER_CODE_FILES,publishedCode,runnerCodeGate,WAKE} from "../scripts/release-runner.mjs";
import {acquireHostLock,readHostState as rawReadHostState,holdersOf,RUN_TIMEOUT_GRACE_MS} from "../scripts/verify-matrix-host-lock.mjs";
import {createHash} from "node:crypto";

// Release batching (wi_50e80d45647b342a slice one).
import {addBatchSolo,BATCH_IMPORT_WAIT_MS} from "../scripts/release-runner.mjs";
import {planWithPreservation,digest as matrixDigest} from "../scripts/verify-matrix.mjs";
export function releaseReply(jobs,argv) {
 const flag=n=>{const i=argv.indexOf(n);return i<0?undefined:argv[i+1];};
 if(argv[1]==="get")return jobs.find(j=>j.id===flag("--job"))||{id:flag("--job")};
 const view=flag("--view")||"active",terminal=["released","rolled_back","refused","superseded"],limit=+(flag("--limit")||50),after=+(flag("--after")||0);
 const all=jobs.map((j,i)=>{
  const {plan,integratedPlan,integratedVerification,integratedCoverage,integratedMatrix,reconciliations,retryOf,matrixApprovals,repository,...summary}=j;
  if(j.receipt)summary.receipt={commit:j.receipt.commit,outcome:j.receipt.outcome,targets:j.receipt.targets.map(t=>({target:t.target,outcome:t.outcome}))};
  if(j.supersession)summary.supersession={releasedCommit:j.supersession.releasedCommit,targets:j.supersession.targets};
  return {...summary,summary:true,rowId:i+1};
 }).filter(j=>terminal.includes(j.state)===(view==="settled")&&j.rowId>after);
 const rows=all.slice(0,limit);return {version:1,jobs:rows,page:{view,limit,snapshot:"a".repeat(64),nextAfter:all.length>limit?String(rows.at(-1).rowId):""}};
}
export const releaseReplySource = "const releaseReply="+releaseReply.toString()+";\n";
// No test here may fall back to the host's own verification lock file.
process.env.TAILTERM_MATRIX_HOST_LOCK=join(mkdtempSync(join(tmpdir(),"release-matrix-lock-")),"host.json");
process.env.TAILTERM_MATRIX_MAX_HOLDERS = "1";
// No test here reads the host's memory pressure (wi_fc5776e011eaabe1).
process.env.TAILTERM_MATRIX_MEMORY_PRESSURE_MAX = "off";
export const readHostState = (...args) => {
  const state = rawReadHostState(...args);
  const held = holdersOf(state);
  assert(held.length <= 1, "legacy capacity-one fixture has at most one holder");
  return state ? { ...state, holder: held[0] || null } : state;
};
delete process.env.TAILTERM_MATRIX_PRIORITY;
delete process.env.TAILTERM_MATRIX_HOLDER_CAP_MINUTES;
export const git=(cwd,...args)=>execFileSync("git",args,{cwd,encoding:"utf8",stdio:["ignore","pipe","pipe"]}).trim();
export function bufferTT(t,body){
 const cwd=mkdtempSync(join(tmpdir(),"release-buffer-")),tt=join(cwd,"tt"),log=join(cwd,"calls"),configPath=join(cwd,"config.json");
 t.after(()=>rmSync(cwd,{recursive:true,force:true}));
 writeFileSync(tt,"#!"+process.execPath+"\n"+releaseReplySource+`const fs=require('fs');const args=process.argv.slice(2);fs.appendFileSync(${JSON.stringify(log)},args.join(' ')+'\\n');if(args[0]!=='deployment'||args[1]!=='list')process.exit(99);`+body);chmodSync(tt,0o755);
 writeFileSync(configPath,JSON.stringify({version:1,enabled:true,cwd,journalDirectory:cwd,tt,baselines:Object.fromEntries(["hub","bridge","mini","tailos"].map(x=>[x,"a".repeat(40)]))}));
 const poll=()=>spawnSync(process.execPath,[RUNNER,"--config",configPath,"--once"],{cwd,encoding:"utf8",stdio:["ignore","pipe","pipe"],timeout:30000});
 return {cwd,tt,log,poll};
}
export function fixture(){const cwd=mkdtempSync(join(tmpdir(),"release-git-")),origin=mkdtempSync(join(tmpdir(),"release-origin-"));git(origin,"init","--bare","-b","tasks-hub");git(cwd,"init","-b","tasks-hub");git(cwd,"config","user.email","fixture@example.invalid");git(cwd,"config","user.name","Fixture");mkdirSync(join(cwd,"client"));writeFileSync(join(cwd,"client/base.js"),"base");git(cwd,"add",".");git(cwd,"commit","-m","base");const base=git(cwd,"rev-parse","HEAD");git(cwd,"remote","add","origin",origin);git(cwd,"push","--quiet","origin","tasks-hub");git(cwd,"checkout","-b","candidate");return {cwd,base,origin};}
export function change(f,file,text){writeFileSync(join(f.cwd,file),text);git(f.cwd,"add",".");git(f.cwd,"commit","-m","candidate");return git(f.cwd,"rev-parse","HEAD");}
export function job(f,commit){return {id:"rel_fixture",state:"claimed",commit,baseCommit:f.base,verificationDigest:"a".repeat(64),plan:{commit}};}
export function fake(){const calls=[];return {calls,fence:async()=>true,verifyIntegrated:async()=>true,merged:async()=>calls.push("merged"),prepare:async t=>({release:"fixture-release",artifactSHA256:"b".repeat(64)}),deploy:async t=>calls.push("deploy:"+t),check:async()=>true,rollback:async t=>{calls.push("rollback:"+t);return true;},finish:async r=>calls.push("finish"),escalate:async()=>calls.push("escalate"),requestBug:async()=>{calls.push("bug");return "rel_fixture-rollback-bug";},block:async()=>calls.push("block"),refuse:async()=>calls.push("refuse")};}
export function config(f,j){const home=mkdtempSync(join(tmpdir(),"release-journal-"));return {cwd:f.cwd,job:j,baselines:Object.fromEntries(["hub","bridge","mini","tailos"].map(t=>[t,f.base])),journalPath:join(home,"journal.json"),testPolicy:{startupMs:0,failures:1,intervalMs:0,relayCleanMs:0}};}

// The Mini tests always supply host setup stubs, so no test runs a real
// tt host setup; the artifact is a text file, so an omitted stub fails.
export function miniFixture(over={}){
 const cwd=mkdtempSync(join(tmpdir(),"mini-adapter-")),install=join(cwd,"tt"),artifact=join(cwd,"next"),previous=join(cwd,"tt.previous"),db=join(cwd,"fixture.sqlite"),restarts=join(cwd,"restarts"),probes=join(cwd,"probes");
 writeFileSync(install,"old CLI");writeFileSync(artifact,"new CLI");writeFileSync(db,"newer writes");
 const node=(script,...args)=>[process.execPath,"-e",script,...args];
 const a={installPath:install,artifactPath:artifact,rollbackSafe:true,artifactSHA256:hash(readFileSync(artifact)),
  hostSetup:node("const fs=require('fs'),[i,a,p]=process.argv.slice(1);fs.copyFileSync(i,p);fs.copyFileSync(a,i)",install,artifact,previous),
  hostRollback:node("const fs=require('fs'),[i,p]=process.argv.slice(1);fs.copyFileSync(p,i)",install,previous),
  relayRestart:node("require('fs').appendFileSync(process.argv[1],'restart\\n')",restarts),
  rollbackProbe:node("require('fs').appendFileSync(process.argv[1],'probe\\n');console.log(JSON.stringify({restored:true,databaseWritesPreserved:true}))",probes),...over(node,{install,artifact,previous})};
 const adapter=new HostAdapter({cwd,journalDirectory:cwd},{id:"rel_fixture"});adapter.captureMiniRollback(a);adapter.artifacts.set("mini",a);
 return {adapter,a,install,artifact,db,restarts,probes};
}

export const hash=b=>createHash("sha256").update(b).digest("hex");
export const stamped=(commit,modified="false")=>`x: go1.26\n\tbuild\tvcs=git\n\tbuild\tvcs.revision=${commit}\n\tbuild\tvcs.modified=${modified}\n`;
// The handler's import as the runner sees it afterwards: a manifest named for
// the generation before the job's present one (the import moved the job on by
// one), recording that generation and the attempt. A job with no generation,
// or at its first, is moved on instead.
export const inputsGeneration=job=>job.generation>1?job.generation-1:1;
export const inputsKey=adapter=>`${adapter.job.id}-g${inputsGeneration(adapter.job)}`;
export function importInputs(adapter,commit,targets,extra={}){
 const generation=inputsGeneration(adapter.job);if(!(adapter.job.generation>generation))adapter.job.generation=generation+1;
 const inputs={version:1,jobId:adapter.job.id,generation,attempt:adapter.job.reconciliations?.length||0,commit,acceptedCommit:adapter.job.commit,verificationDigest:adapter.job.verificationDigest,...extra,targets};
 const path=join(adapter.config.journalDirectory,`${adapter.job.id}-g${generation}-inputs.json`),raw=JSON.stringify(inputs);writeFileSync(path,raw);
 adapter.job.inputsCommit=commit;adapter.job.inputsDigest=hash(raw);return path;
}
export function hostFixture(){
 const f=fixture();mkdirSync(join(f.cwd,"hub/cmd/tt"),{recursive:true});mkdirSync(join(f.cwd,"hub/internal/store"),{recursive:true});
 const commit=change(f,"hub/cmd/tt/main.go","fixture");const home=mkdtempSync(join(tmpdir(),"release-host-inputs-")),install=join(home,"tt");writeFileSync(install,"v1");
 const config={cwd:f.cwd,journalDirectory:home,baselines:Object.fromEntries(["hub","bridge","mini","tailos"].map(t=>[t,f.base])),targets:{mini:{installPath:install,rollbackPath:join(home,"stale-before"),release:"stale-release",hostSetup:["host-setup-stub"],hostRollback:["host-rollback-stub"],relayRestart:[process.execPath,"-e","process.exit(0)"],rollbackProbe:[process.execPath,"-e","console.log(JSON.stringify({restored:true,databaseWritesPreserved:true}))"]}}};
 return {f,commit,home,install,config};
}
// A manifest file as the inputs command writes it, under any name.
export function manifestFile(adapter,commit,name,fields){
 const raw=JSON.stringify({version:1,jobId:adapter.job.id,...fields,commit,acceptedCommit:adapter.job.commit,verificationDigest:adapter.job.verificationDigest,targets:{}}),path=join(adapter.config.journalDirectory,name);
 writeFileSync(path,raw);return {path,raw,digest:hash(raw)};
}
// A job claimed again after one set-aside: the first attempt wrote at
// generation 3, the second at 8, and the import moved the job to 9.
export function reclaimed(){
 const {f,commit,config}=hostFixture(),adapter=new HostAdapter({...config,journalDirectory:mkdtempSync(join(tmpdir(),"release-inputs-select-"))},{...job(f,commit),id:"rel_sel",generation:9,reconciliations:[{disposition:"requeue"}],inputsCommit:commit});
 return {commit,adapter,dir:adapter.config.journalDirectory,bind:file=>{adapter.job.inputsDigest=file.digest;}};
}
export const otherGeneration=e=>failureReason(e)==="Input manifest bound to another generation" && failureDetail(e).text==="Input manifest bound to another generation";
// The production hub service as hub_compose prints it for a plain plan.
export const HUB_SERVICE={image:"gcr.io/distroless/static-debian12:nonroot",user:"950:950",restart:"unless-stopped",entrypoint:["/opt/tailterm-hub"],read_only:true,cap_drop:["ALL"],security_opt:["no-new-privileges:true"],ports:["100.116.238.37:18765:18765"],
 environment:{TAILTERM_TCP_LISTEN:"0.0.0.0:18765",TAILTERM_STATE:"/state",SQLITE_TMPDIR:"/state",TAILTERM_TOKEN_FILE:"/run/hub-token",TAILTERM_MAX_AGENTS:"32"},
 volumes:["/mnt/deepfreeze/tailterm-hub/releases/fixture/tailterm-hub:/opt/tailterm-hub:ro","/mnt/deepfreeze/tailterm-hub/state:/state","/mnt/deepfreeze/tailterm-hub/hub-token:/run/hub-token:ro"],mem_limit:"512m",cpus:"1.0"};

// Journal retention (j1-j3). A rehearsal fixture: an imported backup copy and a
// migration binary in a private journal directory, with the container run
// replaced by one that writes the copy and SQLite sidecars as a real one does.
// status and run are what the runtime check and the run report.
export const SIDECARS=["","-wal","-shm","-journal"];
export function rehearsalFixture(id="rel_rehearse"){
 const home=mkdtempSync(join(tmpdir(),"release-rehearsal-")),backup=join(home,id+"-truenas-backup.sqlite"),binary=join(home,id+"-migration");
 writeFileSync(backup,"imported backup");writeFileSync(binary,"migration binary");
 const adapter=new HostAdapter({cwd:home,journalDirectory:home},{id});let clock=Date.parse("2026-10-01T09:00:00Z");adapter.now=()=>clock+=1000;
 const artifact={backupCopy:backup,backupSHA256:hash("imported backup"),migrationBinary:binary,migrationBinarySHA256:hash("migration binary"),planPath:join(home,"plan.json"),release:"fixture"};
 const base=backup+".rehearsal-"+id,state=base+".state",copy=join(state,"hub.sqlite"),seen=[],calls=[],commands=[];
 const r={home,backup,binary,adapter,artifact,state,copy,marker:base+".json",seen,calls,commands,service:HUB_SERVICE,status:{status:0},run:{status:0},left:()=>existsSync(state)?[".state",...readdirSync(state)]:[]};
 adapter.command=argv=>{commands.push(argv);if(argv[0]==="python3")return JSON.stringify(r.service);throw new Error("unexpected host command");};
 adapter.container=argv=>{calls.push(argv);if(argv[1]==="system")return {stdout:"",stderr:"",...r.status};if(argv[1]!=="run")return {status:0,stdout:"",stderr:""};
  seen.push({argv,copy:readFileSync(copy,"utf8"),wal:existsSync(copy+"-wal")});for(const s of SIDECARS)writeFileSync(copy+s,"migrated");return {stdout:"",stderr:"",...r.run};};
 return r;
}
// Container rehearsal (wi_78521ba68a8ad197), criteria c1-c9 of order #27293's plan (its a1, a4-a7, a9).
export const REPO=resolve(dirname(new URL(import.meta.url).pathname),"..");
export function composePlan(extra={}){
 const dir=mkdtempSync(join(tmpdir(),"release-compose-")),release="rel_compose-fixture",BASE="/mnt/deepfreeze/tailterm-hub",planPath=join(dir,"plan.json");
 writeFileSync(planPath,JSON.stringify({route:{id:"truenas-ssh",kind:"ssh",host:"truenas",batchMode:true,connectTimeoutSeconds:10},databaseOwner:"db-handler",operation:"sqlite-online-backup",
  checks:["source-integrity","source-foreign-keys","backup-integrity","backup-foreign-keys","profile-snapshots"],profileTables:["profile_meta","profiles","profile_history"],
  sourceDatabase:BASE+"/state/hub.sqlite",allowedBackupRoot:BASE+"/backups",backupOwner:{uid:950,gid:950},
  deployment:{releaseName:release,binaryDestination:`${BASE}/releases/${release}/tailterm-hub`,stateDirectory:BASE+"/state",tokenPath:BASE+"/hub-token",appName:"tailterm-hub",tcpListener:"100.116.238.37:18765",...extra}}));
 return {planPath,release};
}
// A fake adapter whose every call is recorded with its arguments, to show what leaves the runner.
export function recorded(a){const said=[];for(const [k,fn] of Object.entries(a))if(typeof fn==="function")a[k]=async(...args)=>{said.push(JSON.stringify([k,...args])??k);return fn(...args);};return said;}
export function schemaRelease(id){
 const f=fixture();mkdirSync(join(f.cwd,"hub/internal/api"),{recursive:true});const j=job(f,change(f,"hub/internal/api/fixture.go","fixture")),c=config(f,j),a=fake(),e=rehearsalFixture(id);
 a.prepare=async()=>({release:"fixture",artifactSHA256:"b".repeat(64),backup:"copy",backupSHA256:"c".repeat(64),preflightReceiptSHA256:"d".repeat(64),schemaChanged:true});a.rehearse=()=>e.adapter.rehearse(e.artifact);
 return {c,a,e,said:recorded(a),journal:()=>JSON.parse(readFileSync(c.journalPath,"utf8"))};
}
// A fake journal: files maps a name to its size in bytes.
export function journalFixture(files){
 const dir=mkdtempSync(join(tmpdir(),"release-retention-"));for(const [name,bytes] of Object.entries(files))writeFileSync(join(dir,name),"x".repeat(bytes));
 return {dir,names:()=>readdirSync(dir).filter(n=>n!=="retention.jsonl").sort(),lines:()=>existsSync(join(dir,"retention.jsonl"))?readFileSync(join(dir,"retention.jsonl"),"utf8").split("\n").filter(Boolean).map(l=>JSON.parse(l)):[]};
}
export const copyOf=id=>id+"-truenas-backup.sqlite",sweep=(j,jobs,retention)=>pruneJournal({journalDirectory:j.dir,retention},jobs,{now:()=>Date.parse("2026-10-01T09:30:00Z")});
// Dist and attempt retention (wi_d203eb94ef27d8f8), tests t1-t7 of order #30140's plan.
// C(n) is a commit of one repeated hex digit; a dist holds 15 bytes in two files, one nested.
export const C=n=>String(n).repeat(40),DIST=n=>"tailos-dist-"+C(n),SWEEP_AT=Date.parse("2026-10-01T09:30:00Z");
export const tailosJob=(id,n,settledAt)=>({id,state:"released",settledAt,commit:C(n),receipt:{commit:C(n),outcome:"released",targets:[{target:"hub",outcome:"unchanged"},{target:"tailos",outcome:"released"}]}});
export const fiveReleased=()=>[3,1,5,2,4].map(n=>tailosJob("rel_"+n,n,`2026-09-2${n}T10:00:00Z`));
export function addDist(j,n){mkdirSync(join(j.dir,DIST(n),"assets"),{recursive:true});writeFileSync(join(j.dir,DIST(n),"index.html"),"x".repeat(10));writeFileSync(join(j.dir,DIST(n),"assets","app.js"),"x".repeat(5));}
export function distFixture(...ns){const j=journalFixture({});for(const n of ns)addDist(j,n);return j;}
export const sweepAt=(j,jobs,retention,options={})=>pruneJournal({journalDirectory:j.dir,retention},jobs,{now:()=>SWEEP_AT,...options});
export const distRecord=(jobId,n,reason)=>({version:1,at:"2026-10-01T09:30:00.000Z",jobId,file:DIST(n),kind:"dist",bytes:15,reason});
// An attempt directory: files maps a name to [content, age in days at the sweep].
export function addAttempt(j,id,files,attempt=C(7)+"-r0"){
 const dir=join(j.dir,id+"-integrated-verification",attempt);mkdirSync(dir,{recursive:true});
 for(const [name,[content,days]] of Object.entries(files)){writeFileSync(join(dir,name),content);const at=new Date(SWEEP_AT-days*86400000);utimesSync(join(dir,name),at,at);}
 return dir;
}
export const EVIDENCE={"receipt.json":["receipt",9],"context.json":["context",9],"plan.json":["plan",9],"plan.preserved.json":["preserved",9],[C(8)+".attempt-1.log"]:["check log",9],"host-lock.log":["lock log",9]};
export const BULK={"tailterm-hub-test":["h".repeat(40),5],"run.out":["out",5],"host-lock.json":["{}",3],"test-binaries.json":["[]",4]},YOUNG={"tailterm-tt-test":["t".repeat(20),2]};
export const attemptRecord=(jobId,name,bytes,attempt=C(7)+"-r0")=>({version:1,at:"2026-10-01T09:30:00.000Z",jobId,file:`${jobId}-integrated-verification/${attempt}/${name}`,kind:"attempt",bytes,reason:"age"});
export const bulkRecords=id=>[["host-lock.json",2],["run.out",3],["tailterm-hub-test",40],["test-binaries.json",2]].map(([name,bytes])=>attemptRecord(id,name,bytes));

export const remoteHead=f=>git(f.origin,"rev-parse","refs/heads/tasks-hub");
export function goWorktreeFixture(){
 const f=fixture();mkdirSync(join(f.cwd,"hub/cmd/tt"),{recursive:true});writeFileSync(join(f.cwd,"hub/go.mod"),"module example.com/hub\n\ngo 1.22\n");writeFileSync(join(f.cwd,"hub/cmd/tt/main.go"),"package main\n\nfunc main() {}\n");
 git(f.cwd,"add",".");git(f.cwd,"commit","-m","go candidate");const commit=git(f.cwd,"rev-parse","HEAD");
 const deployer=join(mkdtempSync(join(tmpdir(),"release-deployer-")),"checkout");git(f.cwd,"worktree","add","--quiet","--detach",deployer,commit);
 const home=mkdtempSync(join(tmpdir(),"release-deployer-home-")),install=join(home,"tt");writeFileSync(install,"v1");
 const config={cwd:deployer,journalDirectory:home,baselines:Object.fromEntries(["hub","bridge","mini","tailos"].map(t=>[t,f.base])),targets:{mini:{installPath:install,relayRestart:[process.execPath,"-e","0"]}}};
 const adapter=new HostAdapter(config,{...job(f,commit),id:"rel_stamp"});importInputs(adapter,commit,{mini:{release:"rel_stamp-"+commit.slice(0,12)+"-mini",rollbackSafe:true}});
 return {f,commit,deployer,adapter};
}

// Hub and bridge released together share one TrueNAS plan (wi_bf16d39731a39104).
export const PAIRED={release:"rel_fixture-000000000000-truenas",artifactSHA256:"b".repeat(64),planPath:"/private/rel_fixture-truenas-plan.json",planTargets:["hub","bridge"],backup:"/b/before-rel_fixture-truenas.sqlite",backupSHA256:"c".repeat(64),preflightReceiptSHA256:"d".repeat(64)};
export function pairedRelease(opts={}){
 const f=fixture();mkdirSync(join(f.cwd,"hub/internal/api"),{recursive:true});const j=job(f,change(f,"hub/internal/api/x.go","x")),a=fake(),c=config(f,j);let receipt;
 a.prepare=async t=>["hub","bridge"].includes(t)?{...PAIRED,schemaChanged:opts.schema===true}:{release:"rel_fixture-mini",artifactSHA256:"b".repeat(64)};
 a.rehearse=async()=>{a.calls.push("rehearse");return true;};a.finish=async r=>{receipt=r;a.calls.push("finish");};
 return {f,j,a,c,receipt:()=>receipt,journal:()=>JSON.parse(readFileSync(c.journalPath,"utf8"))};
}

export function pairedHost({bridgeRelease}={}){
 const f=fixture();mkdirSync(join(f.cwd,"hub/internal/api"),{recursive:true});const commit=change(f,"hub/internal/api/x.go","x"),home=mkdtempSync(join(tmpdir(),"release-paired-"));
 const config={cwd:f.cwd,journalDirectory:home,baselines:Object.fromEntries(["hub","bridge","mini","tailos"].map(t=>[t,f.base])),targets:{hub:{},bridge:{}}};
 const adapter=new HostAdapter(config,{...job(f,commit),id:"rel_pair"}),release="rel_pair-"+commit.slice(0,12)+"-truenas",BASE="/mnt/deepfreeze/tailterm-hub";
 const planPath=join(home,"rel_pair-g1-truenas-plan.json"),receipt=join(home,"rel_pair-g1-truenas-preflight.json"),backup=`${BASE}/backups/before-rel_pair-g1-truenas.sqlite`;writeFileSync(receipt,"{}");
 writeFileSync(planPath,JSON.stringify({backupDestination:backup,deployment:{releaseName:release,targets:["hub","bridge"],binaryDestination:`${BASE}/releases/${release}/tailterm-hub`,bridgeBinaryDestination:`${BASE}/releases/${bridgeRelease||release}/tailterm-discord`}}));
 const shared={release,backupJobId:"rel_pair",backup,backupSHA256:"c".repeat(64),planPath,preflightReceipt:receipt,preflightReceiptSHA256:hash("{}"),planTargets:["hub","bridge"],rollbackSafe:true};
 const calls=[];adapter.command=argv=>{calls.push(argv);if(argv[1]==="version")return stamped(commit);if(argv.includes("build")){const out=argv[argv.indexOf("-o")+1];writeFileSync(out,"binary "+out);return "";}return "";};
 return {f,commit,adapter,calls,shared,planPath,importTargets:targets=>importInputs(adapter,commit,targets)};
}

// wi_5b03fe47520b7c4f: the deployer verifies an integrated commit in its own
// checkout, so the matrix prerequisites, the run's timeout and other matrix
// runs on the host are the runner's concern.
export const RUNNER=new URL("../scripts/release-runner.mjs",import.meta.url).pathname;
export const ignorePrerequisites=cwd=>writeFileSync(join(cwd,".git/info/exclude"),"node_modules/\n.build/\nwasm/*.wasm\n");
export function placePrerequisites(dir,skip=[]){for(const p of MATRIX_PREREQUISITES.filter(p=>!skip.includes(p))){mkdirSync(join(dir,dirname(p)),{recursive:true});writeFileSync(join(dir,p),"fixture "+p);}}
export const workedPlan={maxAttempts:3,checks:[{id:"go-race",environment:{VERIFICATION_TIMEOUT_MS:"1800000"}},{id:"npm-unit",environment:{VERIFICATION_TIMEOUT_MS:"120000"}}]};
export const exitedPid=()=>spawnSync(process.execPath,["-e",""]).pid;
export const attemptDir=(home,commit,n)=>join(home,"rel_fixture-integrated-verification",`${commit}-r${n}`);
export function matrixHost({plan=workedPlan,receipt={environment:{},checks:[{exitCode:0}]},jobs=[]}={}){
 const f=fixture(),home=mkdtempSync(join(tmpdir(),"matrix-host-"));ignorePrerequisites(f.cwd);placePrerequisites(f.cwd);
 const adapter=new HostAdapter({cwd:f.cwd,journalDirectory:home},{id:"rel_fixture",agentId:"agt_fixture",runId:"run_fixture",generation:1});
 // The stub run has already ended with this receipt when the next poll looks.
 const calls=[];adapter.processStartTime=()=>null;
 adapter.startMatrixRun=(argv,dir)=>{calls.push({argv,dir});writeFileSync(join(dir,"receipt.json"),JSON.stringify(receipt));return exitedPid();};
 adapter.command=(argv,cwd,options)=>{calls.push({argv,timeout:options?.timeout??600000});
  if(argv[1]==="deployment"&&argv[2]==="get")return JSON.stringify({...adapter.job,state:"claimed",generation:adapter.job.generation??1,...(jobs.find(j=>j.id==="rel_fixture")||{})});
  if(argv[1]==="deployment"&&argv[2]==="handler")return JSON.stringify({id:"agt_0123abcd"});
  if(argv[1]==="scripts/verify-matrix.mjs"&&argv[2]==="plan"){writeFileSync(argv[4],JSON.stringify(plan));return "";}
  return "";};
 const integrated={id:"rel_fixture",integratedCommit:"c".repeat(40),plan:{commit:"a".repeat(40)}};
 // One poll starts the run, the next finds it ended.
 const verify=async j=>{await adapter.verifyIntegrated(j);return adapter.verifyIntegrated(j);};
 return {f,home,adapter,calls,integrated,verify,run:(n=0,commit=integrated.integratedCommit)=>JSON.parse(readFileSync(join(attemptDir(home,commit,n),"run.json"),"utf8")),matrix:()=>calls.filter(c=>c.argv[1]==="scripts/verify-matrix.mjs"),sends:()=>calls.filter(c=>c.argv[1]==="send")};
}
export function provisionFixture(){
 const cwd=mkdtempSync(join(tmpdir(),"provision-checkout-")),from=mkdtempSync(join(tmpdir(),"provision-source-"));
 git(cwd,"init","-q","-b","tasks-hub");writeFileSync(join(cwd,".gitignore"),"node_modules/\n.build/\nwasm/*.wasm\n");git(cwd,"add",".");git(cwd,"-c","user.email=f@example.invalid","-c","user.name=F","commit","-q","-m","base");
 placePrerequisites(from);const installs=[];
 const run=(argv,dir)=>{installs.push([argv,dir]);mkdirSync(join(dir,"node_modules"),{recursive:true});writeFileSync(join(dir,"node_modules/.package-lock.json"),"installed");};
 return {cwd,from,installs,run};
}
// The integrated matrix run as a member of the verification host's ordered
// waitlist (wi_c5cb667695c3614c). Each test uses its own lock file; the child
// below is a real second process joining it the way a matrix run does.
export const LOCK_MODULE=new URL("../scripts/verify-matrix-host-lock.mjs",import.meta.url).href;
export const RUN_CHILD=`
import fs from 'node:fs';
import {spawn} from 'node:child_process';
import {acquireHostLock} from ${JSON.stringify(LOCK_MODULE)};
const o=JSON.parse(process.argv[2]);
if(o.resist)process.on('SIGTERM',()=>{});
const lease=await acquireHostLock({path:o.path,priority:o.priority,prioritySource:'flag',item:o.item,agent:'deployer',output:o.dir,recordDirectory:o.dir,runTimeoutMs:600000,pollMs:20,environment:{TAILTERM_MATRIX_MAX_HOLDERS:o.limit||'1',TAILTERM_MATRIX_MEMORY_PRESSURE_MAX:'off'}});
let group=null;
if(o.group){const c=spawn(process.execPath,['-e','setInterval(()=>{},1000)'],{detached:true,stdio:'ignore'});await new Promise(r=>c.on('spawn',r));c.unref();group=c.pid;lease.addGroup(group);}
if(o.mode==='receipt'){fs.writeFileSync(o.dir+'/receipt.json',JSON.stringify({environment:{},checks:[{exitCode:0}]}));await lease.release();process.exit(0);}
if(!o.resist)process.on('SIGTERM',async()=>{await lease.release();process.exit(143);});
fs.writeFileSync(o.marker,JSON.stringify({pid:process.pid,group}));
setInterval(()=>{},1000);
`;
export async function until(condition,what,ms=20000){const end=Date.now()+ms;for(;;){const v=condition();if(v)return v;if(Date.now()>end)assert.fail("timed out waiting for "+what);await new Promise(r=>setTimeout(r,10));}}
export const ended=c=>c.exitCode!==null||c.signalCode!==null?Promise.resolve():new Promise(r=>c.once("exit",r));
export function idle(t){const c=spawn(process.execPath,["-e","setInterval(()=>{},1000)"],{detached:true,stdio:"ignore"});t.after(()=>{try{process.kill(c.pid,"SIGKILL");}catch{}});return c;}
export function lockHost(t,child={}){
 const h=matrixHost(),script=join(h.home,"run-child.mjs");writeFileSync(script,RUN_CHILD);
 h.path=join(mkdtempSync(join(tmpdir(),"runner-host-lock-")),"host.json");h.children=[];h.signals=[];h.dir=attemptDir(h.home,h.integrated.integratedCommit,0);
 delete h.adapter.processStartTime;h.adapter.hostState=()=>rawReadHostState(h.path);
 h.adapter.signalProcess=(target,name)=>{h.signals.push([target,name]);process.kill(target,name);};
 h.adapter.startMatrixRun=(argv,dir)=>{
  const flag=name=>argv[argv.indexOf(name)+1];
  const c=spawn(process.execPath,[script,JSON.stringify({path:h.path,dir,marker:join(dir,"marker"),priority:flag("--priority"),item:flag("--item"),mode:"hold",...child})],{detached:true,stdio:"ignore"});
  h.calls.push({argv,dir});h.children.push(c);return c.pid;};
 t.after(()=>{for(const c of h.children){try{process.kill(-c.pid,"SIGKILL");}catch{}}});
 h.marker=()=>until(()=>existsSync(join(h.dir,"marker"))&&JSON.parse(readFileSync(join(h.dir,"marker"),"utf8")),"the run to hold the host");
 h.starts=()=>h.matrix().filter(c=>c.argv[2]==="run").length;
 h.deadline=()=>{const r=h.run();return r.launchedAt+r.hostWaitMs+r.boundMs+RUN_TIMEOUT_GRACE_MS+MATRIX_DEADLINE_SLACK_MS;};
 return h;
}
export const holderEntry=(pid,dir,extra={})=>({id:"00000000-0000-4000-8000-000000000001",seq:1,pid,kind:"run",item:"wi_fixture",agent:"deployer",priority:"high",prioritySource:"flag",requestedAt:new Date().toISOString(),runTimeoutMs:600000,output:dir,startedAt:new Date().toISOString(),groups:[],...extra});
export const otherHolder=()=>holderEntry(process.pid,"/elsewhere",{id:"00000000-0000-4000-8000-000000000002",item:"wi_other",agent:"verifier"});
export const reasonIs=text=>e=>failureReason(e)===text;
export function releaseHost(t,child){
 const h=lockHost(t,child),j=job(h.f,change(h.f,"client/a.js","a"));git(h.f.cwd,"checkout","tasks-hub");change(h.f,"client/c.js","c");
 Object.assign(j,{agentId:"agt_fixture",runId:"run_fixture",generation:1});
 const c={...config(h.f,j),journalPath:join(h.home,"rel_fixture.json")};
 const through=host=>{const a=fake();a.verifyIntegrated=x=>host.verifyIntegrated(x);a.settleMatrixRuns=x=>host.settleMatrixRuns(x);return a;};
 // A second runner process: a fresh adapter with no child handle.
 const restarted=()=>{const host=new HostAdapter(h.adapter.config,{id:"rel_fixture",agentId:"agt_fixture",runId:"run_fixture",generation:1});
  for(const key of ["command","hostState","signalProcess","startMatrixRun"])host[key]=h.adapter[key];return host;};
 return Object.assign(h,{j,c,through,restarted});
}
// TailOS switch window: fake release.json reads and a fake clock whose sleep
// only advances time. No network and no real wait.
export function fakeTailOS(responses){let t=0;const fetches=[];return {fetches,now:()=>t,sleep:async ms=>{t+=ms;},fetchJSON:async(url,timeoutMs)=>{const r=responses[Math.min(fetches.length,responses.length-1)];fetches.push({url,timeoutMs});if(r instanceof Error)throw r;return r;}};}
export function tailosAdapter(responses,tailos={}){
 const home=mkdtempSync(join(tmpdir(),"tailos-wait-")),a=new HostAdapter({cwd:home,journalDirectory:home,targets:{tailos:{url:"https://tailos.test/release.json",...tailos}}},{id:"rel_fixture"});
 a.probeDeps=fakeTailOS(responses);a.artifacts.set("tailos",{commit:"d".repeat(40)});return a;
}
export const MATRIX_A='{"matrix":"approved"}\n',MATRIX_B='{"matrix":"changed"}\n';
export const uncoveredReason=`Matrix digest changed ${hash(MATRIX_A).slice(0,8)} to ${hash(MATRIX_B).slice(0,8)}; no owner approval covers it`;
// The integrated checkout's matrix file is MATRIX_B; the job was approved under MATRIX_A by message 11.
export function changedMatrixHost(approvals){
 const h=matrixHost({jobs:[{id:"rel_fixture",state:"claimed",...(approvals?{matrixApprovals:approvals}:{})}]});
 mkdirSync(join(h.f.cwd,"verification"));writeFileSync(join(h.f.cwd,"verification/matrix.json"),MATRIX_B);
 h.integrated.plan={commit:"a".repeat(40),matrixDigest:hash(MATRIX_A),approvedMatrixDigest:hash(MATRIX_A),matrixApprovalMessageSeq:11};
 h.context=()=>JSON.parse(readFileSync(join(attemptDir(h.home,h.integrated.integratedCommit,0),"context.json"),"utf8"));
 return h;
}
// tasks-hub gains a matrix change after the candidate was accepted: the real
// integration cherry-picks the candidate onto it, and the real host adapter
// verifies, refuses and escalates through stubbed host commands.
export function matrixChangeRelease(approvals){
 const f=fixture(),originHead=remoteHead(f);git(f.cwd,"checkout","tasks-hub");mkdirSync(join(f.cwd,"verification"));f.base=change(f,"verification/matrix.json",MATRIX_A);
 git(f.cwd,"checkout","-B","candidate");const j=job(f,change(f,"client/a.js","a"));
 Object.assign(j.plan,{matrixDigest:hash(MATRIX_A),approvedMatrixDigest:hash(MATRIX_A),matrixApprovalMessageSeq:11});
 git(f.cwd,"checkout","tasks-hub");const tip=change(f,"verification/matrix.json",MATRIX_B);
 ignorePrerequisites(f.cwd);placePrerequisites(f.cwd);
 const c=config(f,j),a=fake(),argvs=[],home=dirname(c.journalPath);
 const host=new HostAdapter({cwd:f.cwd,journalDirectory:home},{id:"rel_fixture",agentId:"agt_fixture",runId:"run_fixture",generation:1});host.processStartTime=()=>null;
 host.startMatrixRun=(argv,dir)=>{argvs.push(argv);writeFileSync(join(dir,"receipt.json"),JSON.stringify({environment:{},checks:[{exitCode:0}]}));return exitedPid();};
 host.command=argv=>{argvs.push(argv);
  if(argv[1]==="deployment"&&argv[2]==="get")return JSON.stringify({...host.job,state:"claimed",matrixApprovals:approvals});
  if(argv[1]==="deployment"&&argv[2]==="handler")return JSON.stringify({id:"agt_0123abcd"});
  if(argv[1]==="scripts/verify-matrix.mjs"&&argv[2]==="plan"){writeFileSync(argv[4],JSON.stringify(workedPlan));return "";}
  return "";};
 a.verifyIntegrated=x=>host.verifyIntegrated(x);a.escalate=async d=>{a.calls.push("escalate");await host.escalate(d);};
 return {f,j,c,a,argvs,home,tip,originHead,sends:()=>argvs.filter(x=>x[1]==="send"),matrix:()=>argvs.filter(x=>x[1]==="scripts/verify-matrix.mjs")};
}
// Hub and bridge readiness window: each probe argv prints one fixed JSON line,
// as the real probe does after its own wait. No host is contacted.
export const printJSON=v=>[process.execPath,"-e",`console.log(${JSON.stringify(JSON.stringify(v))})`];
export const READY_ID={commit:"d".repeat(40),artifactSHA256:"e".repeat(64)},READY_UP={...READY_ID,integrity:true,hubResponds:true,migrationsApplied:true,containersRunning:true,release:"rel_fixture-hub"};
export const READY_CAPTURE={app:{state:"RUNNING",containers:[{service:"hub",state:"running",id:"1".repeat(12)}]},logs:[{service:"hub",lines:["migration 41 started"]},{service:"discord-bridge",unavailable:"no log lines"}]};
export const READY_DOWN={...READY_UP,hubResponds:false,migrationsApplied:false,waitedMs:240000,polls:49,capture:READY_CAPTURE};
export function readyAdapter(target,{live=READY_UP,rollback={restored:true,databaseWritesPreserved:true},program=[process.execPath,"-e","0"]}={}){
 const home=mkdtempSync(join(tmpdir(),"ready-wait-")),a=new HostAdapter({cwd:home,journalDirectory:home},{id:"rel_fixture"}),ran=[],command=a.command.bind(a);
 a.command=(...args)=>{ran.push(args[0]);return command(...args);};
 const artifact={...READY_ID,liveProbe:Array.isArray(live)?live:printJSON(live),rollbackSafe:true,rollbackProgram:program,rollbackProbe:Array.isArray(rollback)?rollback:printJSON(rollback)};
 a.artifacts.set(target,artifact);return Object.assign(a,{ran,artifact});
}
export const READY_POLICY={startupMs:60000,failures:3,intervalMs:5000,relayCleanMs:0},EXIT1=[process.execPath,"-e","process.exit(1)"];
export async function countedLiveCheck(a,target){let now=0;const sleeps=[];const out=await liveCheck(a,target,READY_POLICY,async ms=>{sleeps.push(ms);now+=ms;},()=>now);return {out,sleeps,checks:a.ran.length};}
// A paired release driven by the fake adapter, with check, rollback and
// probeWait handed to a real HostAdapter whose probes print fixed output.
export function readyRelease(liveHub,rollbackHub={restored:true,databaseWritesPreserved:true,waitedMs:15000,polls:4}){
 const p=pairedRelease(),h=readyAdapter("hub",{live:liveHub,rollback:rollbackHub});let details;
 h.artifacts.set("bridge",{...h.artifact,liveProbe:printJSON(READY_UP),rollbackProbe:printJSON({restored:true,databaseWritesPreserved:true,waitedMs:0,polls:1})});
 p.a.check=t=>h.check(t);p.a.rollback=t=>{p.a.calls.push("rollback:"+t);return h.rollback(t);};p.a.probeWait=(t,kind)=>h.probeWait(t,kind);p.a.escalate=async d=>{details=d;p.a.calls.push("escalate");};
 return {...p,h,details:()=>details};
}

// Tagged refusals (g1-g5, g7). A candidate and tasks-hub that write the same
// files differently, so the cherry-pick leaves each of them unmerged.
export const TOKEN="SYNTHETIC_PRIVATE_TOKEN",NEXT_ACTION="Next: re-apply the candidate on the current tasks-hub through a follow-through item and re-review the new commit.";
export function conflictFixture(files=["client/base.js"]){
 const f=fixture(),write=text=>{for(const p of files){mkdirSync(dirname(join(f.cwd,p)),{recursive:true});writeFileSync(join(f.cwd,p),text+p);}git(f.cwd,"add",".");git(f.cwd,"commit","-m",text);return git(f.cwd,"rev-parse","HEAD");};
 const commit=write("candidate ");git(f.cwd,"checkout","tasks-hub");const prior=write("release ");return {f,j:job(f,commit),prior};
}
export async function journalOf(f,j,arrange=()=>{},expected=/refused/){
 const a=fake(),c=config(f,j);let escalation;a.escalate=async d=>{escalation=d;a.calls.push("escalate");};arrange(a,c);
 await assert.rejects(runRelease(c,a),expected);const raw=readFileSync(c.journalPath,"utf8");return {a,c,raw,journal:JSON.parse(raw),escalation};
}
export const checkoutState=cwd=>({status:git(cwd,"status","--porcelain"),head:git(cwd,"rev-parse","HEAD"),ref:git(cwd,"rev-parse","refs/heads/tasks-hub")});
export function refusesUnchanged(cwd,j,tag){
 const before=checkoutState(cwd);assert.throws(()=>integrateCandidate(cwd,j),e=>failureReason(e)===tag,tag);assert.deepEqual(checkoutState(cwd),before,tag);return before;
}

// Runner code gate (wi_2be015df9af54c5c). A fixture repository whose scripts/
// and tests/ hold small stand-ins for the watched files: commit a, then
// commit b changing one of them (the runner unless another is named).
export const CODE_AGENT="agt_c0defixture",CODE_RUN="run_c0defixture",SAFE_TEXT=/^[A-Za-z0-9 ,.:;()_\/-]+$/;
export function codeRepo(t,real=false,changed="release-runner.mjs"){
 // The real path: the daemon entry compares its argv with the module's own URL.
 const cwd=realpathSync(mkdtempSync(join(tmpdir(),"release-code-")));t.after(()=>rmSync(cwd,{recursive:true,force:true}));
 git(cwd,"init","-b","tasks-hub");git(cwd,"config","user.email","fixture@example.invalid");git(cwd,"config","user.name","Fixture");mkdirSync(join(cwd,"scripts"));mkdirSync(join(cwd,"tests"));
 for(const n of RUNNER_CODE_FILES){if(real)copyFileSync(new URL("../scripts/"+n,import.meta.url),join(cwd,"scripts",n));else writeFileSync(join(cwd,"scripts",n),`// ${n} a\n`);}
 git(cwd,"add",".");git(cwd,"commit","-m","a");const a=git(cwd,"rev-parse","HEAD"),codeA=publishedCode(cwd);
 writeFileSync(join(cwd,"scripts",changed),readFileSync(join(cwd,"scripts",changed),"utf8")+"// b\n");git(cwd,"add",".");git(cwd,"commit","-m","b");
 const b=git(cwd,"rev-parse","HEAD"),codeB=publishedCode(cwd);
 // at(head,published): detached at head with tasks-hub at published.
 const at=(head,published)=>{git(cwd,"checkout","--quiet","--detach",head);git(cwd,"update-ref","refs/heads/tasks-hub",published);};
 return {cwd,a,b,codeA,codeB,at,head:()=>git(cwd,"rev-parse","HEAD")};
}
// A fake tt that reads its job list from a file on every call, logs each argv
// as a JSON line, accepts claim, check and send, and fails anything else.
export function codeHost(t,cwd,extra=""){
 const home=mkdtempSync(join(tmpdir(),"release-code-home-"));t.after(()=>rmSync(home,{recursive:true,force:true}));
 const tt=join(home,"tt"),log=join(home,"calls.jsonl"),jobsPath=join(home,"jobs.json");
 writeFileSync(tt,"#!"+process.execPath+"\n"+releaseReplySource+`const fs=require('fs'),cp=require('child_process');const a=process.argv.slice(2),jobs=JSON.parse(fs.readFileSync(${JSON.stringify(jobsPath)},'utf8'));
let head='';try{head=cp.execFileSync('git',['rev-parse','HEAD'],{cwd:${JSON.stringify(cwd)},encoding:'utf8',stdio:['ignore','pipe','ignore']}).trim();}catch{}
fs.appendFileSync(${JSON.stringify(log)},JSON.stringify({argv:a,ppid:process.ppid,run:process.env.TAILTERM_RUN||null,agent:process.env.TAILTERM_AGENT||null,head})+'\\n');
const flag=n=>a[a.indexOf(n)+1],job=jobs.find(j=>j.id===flag('--job'));${extra}
if(['list','get'].includes(a[1]))console.log(JSON.stringify(releaseReply(jobs,a)));
else if(a[1]==='claim')console.log(JSON.stringify({...job,state:'claimed',generation:+flag('--generation')+1,agentId:process.env.TAILTERM_AGENT,runId:process.env.TAILTERM_RUN}));
else if(a[1]==='check')console.log(JSON.stringify({...job,state:'claimed',generation:+flag('--generation'),agentId:process.env.TAILTERM_AGENT,runId:process.env.TAILTERM_RUN}));
else if(a[0]!=='send')process.exit(2);`);chmodSync(tt,0o755);
 const config={version:1,enabled:true,cwd,journalDirectory:home,tt,baselines:Object.fromEntries(["hub","bridge","mini","tailos"].map(x=>[x,"a".repeat(40)]))};
 const setJobs=jobs=>writeFileSync(jobsPath,JSON.stringify(jobs));setJobs([]);
 const calls=()=>existsSync(log)?readFileSync(log,"utf8").trim().split("\n").filter(Boolean).map(l=>JSON.parse(l)):[];
 const sends=()=>calls().filter(c=>c.argv[0]==="send").map(c=>({subject:c.argv[c.argv.indexOf("--subject")+1],text:c.argv[c.argv.indexOf("--text")+1],requestId:c.argv[c.argv.indexOf("--request-id")+1]}));
 const claims=()=>calls().filter(c=>c.argv[1]==="claim");
 const record=()=>JSON.parse(readFileSync(join(home,"runner-code.json"),"utf8"));
 // One poll in this process under a fixed agent and run; returns its stderr.
 const poll=async(code,release=async()=>{},options={once:true})=>{
  const saved=[process.env.TAILTERM_AGENT,process.env.TAILTERM_RUN],write=process.stderr.write;let err="";
  process.env.TAILTERM_AGENT=CODE_AGENT;process.env.TAILTERM_RUN=CODE_RUN;process.stderr.write=(chunk,encoding,done=encoding)=>{err+=chunk;if(typeof done==="function")done();return true;};
  try{await serveDeployment(config,{...options,release,...(code?{code}:{})});}
  finally{process.stderr.write=write;for(const [i,k] of ["TAILTERM_AGENT","TAILTERM_RUN"].entries()){if(saved[i]===undefined)delete process.env[k];else process.env[k]=saved[i];}}
  return err;
 };
 return {home,tt,config,setJobs,calls,sends,claims,record,poll};
}
export const verifiedJob=(id="rel_code")=>({id,state:"verified",generation:1,commit:"c".repeat(40)});
// The gate with a spy in place of the process replacement.
export function codeGate(loaded,over={}){const execs=[];return {execs,gate:runnerCodeGate({loaded,marker:null,restartedFrom:null,now:()=>Date.parse("2026-10-06T06:00:00Z"),execve:(...args)=>{execs.push(args);},...over})};}
export const codeSends=h=>h.sends().filter(s=>s.requestId.startsWith("runner-code-"));
// Held claim notice (wi_03c526946b24ce35). One deployer run of several polls
// over a fake hub whose stderr for a refused claim carries a synthetic secret
// and the private journal path.
export const HELD_ITEM="wi_0123456789abcdef",HELD_SECRET="tth_SYNTHETICsecret0123456789abcdef";
export const HELD_HANDLER="agt_0123abcd",HELD_HELPER="agt_feedbeef";
// The fake hub's roster and handler: one live owner helper beside a closed earlier one.
export const heldRoster=`if(a[0]==='deployment'&&a[1]==='handler'){console.log(JSON.stringify({id:${JSON.stringify(HELD_HANDLER)}}));process.exit(0);}
if(a[0]==='agents'){console.log(JSON.stringify([{id:'agt_0ld',role:'owner_helper',status:'closed'},{id:${JSON.stringify(HELD_HANDLER)},role:'database_handler',status:'done'},{id:'agt_b111de7',status:'running'},{id:${JSON.stringify(HELD_HELPER)},role:'owner_helper',status:'running'}]));process.exit(0);}`;
export const sendOf=c=>{const a=c.argv,field=n=>a[a.indexOf(n)+1];return {to:a.includes("--to")?field("--to"):null,kind:field("--kind"),subject:field("--subject"),text:field("--text"),requestId:field("--request-id"),refs:a.filter((w,i)=>a[i-1]==="--ref")};};
export async function heldRun(t,h,polls,release){
 // Each poll ends in one release call (the later job); the mocked 30 s wait is then ticked.
 const saved=[process.env.TAILTERM_AGENT,process.env.TAILTERM_RUN],write=process.stderr.write;let err="",polled=0;
 process.env.TAILTERM_AGENT=CODE_AGENT;process.env.TAILTERM_RUN=CODE_RUN;process.stderr.write=(chunk,encoding,done=encoding)=>{err+=chunk;if(typeof done==="function")done();return true;};
 const controller=new AbortController();t.mock.timers.enable({apis:["setTimeout"]});
 const settle=async()=>{for(let i=0;i<20;i++)await new Promise(r=>setImmediate(r));};
 try{
  const serving=serveDeployment(h.config,{signal:controller.signal,release:async(c,adapter)=>{const out=await release(c,adapter);polled++;return out;}});
  for(let n=1;n<=polls;n++){while(polled<n)await settle();await settle();if(n===polls)controller.abort();t.mock.timers.tick(30000);}
  await serving;
 }finally{t.mock.timers.reset();process.stderr.write=write;for(const [i,k] of ["TAILTERM_AGENT","TAILTERM_RUN"].entries()){if(saved[i]===undefined)delete process.env[k];else process.env[k]=saved[i];}}
 return err;
}
// Every "./" or "../" import of the watched files that is not itself watched,
// as "importer imports path" with paths from the repository root.
export function unwatchedImports(watched,directory=dirname(RUNNER)){
 const root=dirname(directory),paths=new Set(watched.map(n=>resolve(directory,n))),found=[];
 for(const file of paths){
  const specifiers=[...readFileSync(file,"utf8").matchAll(/(?:\bfrom\s*|\bimport\s*\(?\s*)["'](\.\.?\/[^"']+)["']/g)].map(m=>m[1]);
  for(const specifier of specifiers){const imported=resolve(dirname(file),specifier);if(!paths.has(imported))found.push(`${file.slice(root.length+1)} imports ${imported.slice(root.length+1)}`);}
 }
 return found;
}

// Held-job restart (wi_876c1eb560903f04): a stale runner whose only owned job
// is held restarts onto the published scripts instead of draining for ever.
export const heldOwn=(over={})=>({id:"rel_held",state:"claimed",generation:2,commit:"c".repeat(40),agentId:CODE_AGENT,runId:CODE_RUN,...over});
export const heldJournal=(h,job,over={})=>writeFileSync(join(h.home,job.id+".json"),JSON.stringify({version:1,jobId:job.id,commit:job.commit,agentId:job.agentId,runId:job.runId,phase:"refusing",refusalReason:"release fence lost",refuseFailure:"release generation changed",effects:[],...over}));
export const HELD_SUBJECT="Deployer is restarting itself onto the published scripts with a held job",DRAINING_SUBJECT="Deployer code is out of date; it restarts itself after the current release";
// One job per file, each a single commit on the published base; the first is
// the claimed lead. tasks-hub then moves, so every job is cherry-picked.
export function batchJobs(files,{moved="client/hub.js"}={}){
 const f=fixture();
 const jobs=files.map(([file,text],i)=>{
  git(f.cwd,"checkout","--quiet","--detach",f.base);const commit=change(f,file,text);
  return {id:`rel_b${i+1}`,taskId:"tsk_fixture",entryId:`tqe_b${i+1}`,itemId:`wi_b${i+1}`,itemRevision:1,scopeRevision:1,orderMessageSeq:7,repository:"fixture",baseCommit:f.base,commit,verificationDigest:String(i+1).repeat(64),state:i?"verified":"claimed",generation:i?1:2,pauseGeneration:1,plan:{commit},...(i?{}:{agentId:"agt_fixture",runId:"run_fixture"})};
 });
 git(f.cwd,"checkout","--quiet","tasks-hub");if(moved)change(f,moved,"release");git(f.cwd,"checkout","--quiet","--detach","tasks-hub");
 return {f,lead:jobs[0],members:jobs.slice(1),tip:git(f.cwd,"rev-parse","refs/heads/tasks-hub")};
}
// A fake adapter that also takes the batch calls, as the hub would: each
// accepted call moves the lead's generation.
export function batchFake(lead,{refuse=()=>false}={}){
 const a=fake();a.job={...lead};a.ops=[];a.refused=[];a.receipts=[];a.verifies=[];
 a.batch=async(op,fields,generation)=>{
  const name=op+(fields.entryId?":"+fields.entryId:"");
  if(refuse(op,fields)){a.refused.push(name);throw new Error("refused");}
  assert.equal(generation,a.job.generation,"batch call at the journaled generation");
  a.ops.push({op,...fields,generation});a.calls.push(name);a.job={...a.job,generation:a.job.generation+1};return true;
 };
 a.generation=async()=>a.job.generation;
 a.verifyIntegrated=async j=>{a.calls.push("verify");a.verifies.push(j);return true;};
 a.finish=async r=>{a.receipts.push(structuredClone(r));a.calls.push("finish");};
 return a;
}
export function batchConfig(b,max,over={}){
 const home=mkdtempSync(join(tmpdir(),"release-batch-")),solo=[];
 return {cwd:b.f.cwd,job:b.lead,baselines:Object.fromEntries(["hub","bridge","mini","tailos"].map(t=>[t,b.f.base])),journalPath:join(home,b.lead.id+".json"),testPolicy:{startupMs:0,failures:1,intervalMs:0,relayCleanMs:0},home,solo,
  batch:{max,importWaitMs:BATCH_IMPORT_WAIT_MS,candidates:b.members,onFallback:ids=>{solo.push(...ids);addBatchSolo(home,ids);},...over}};
}
export const batchJournal=c=>JSON.parse(readFileSync(c.journalPath,"utf8"));
export const batchFiles=home=>readdirSync(home).filter(n=>n.startsWith("bat_")||n==="batch-solo.json");
export const three=()=>batchJobs([["client/a.js","a"],["client/b.js","b"],["client/c.js","c"]]);
export const clean=cwd=>assert.equal(git(cwd,"status","--porcelain"),"");
// A repository with an approved matrix and Go packages, for real plans.
export function matrixRepo(){
 const f=fixture();
 for(const p of ["tests","verification","hub/cmd/tt/testdata","hub/internal/store","hub/internal/api","docs"])mkdirSync(join(f.cwd,p),{recursive:true});
 git(f.cwd,"checkout","--quiet","tasks-hub");
 writeFileSync(join(f.cwd,"verification/matrix.json"),JSON.stringify({maxAttempts:3,knownFailures:[],version:1,browserSuites:[],excludedBrowserSuites:[],rules:[{prefixes:["hub/"],groups:["go"]},{prefixes:["docs/"],groups:["unit"]}]}));
 writeFileSync(join(f.cwd,"hub/go.mod"),"module fixture\n\ngo 1.24.0\n");writeFileSync(join(f.cwd,"hub/cmd/tt/main.go"),"package main\n\nfunc main() {}\n");
 writeFileSync(join(f.cwd,"hub/cmd/tt/testdata/README.md"),"old\n");writeFileSync(join(f.cwd,"hub/internal/store/store.go"),"package store\n");writeFileSync(join(f.cwd,"hub/internal/api/api.go"),"package api\n");
 writeFileSync(join(f.cwd,"tests/keep.txt"),"x");writeFileSync(join(f.cwd,"package.json"),JSON.stringify({scripts:{test:'node -e "process.exit(0)"'}}));
 git(f.cwd,"add",".");git(f.cwd,"commit","-qm","packages");const base=git(f.cwd,"rev-parse","HEAD");
 const approved=matrixDigest(readFileSync(join(f.cwd,"verification/matrix.json"),"utf8"));
 // An accepted job: one commit on base changing one path, with its real plan.
 const accept=(n,path,text,over={})=>{
  git(f.cwd,"checkout","--quiet","--detach",over.base||base);writeFileSync(join(f.cwd,path),text);git(f.cwd,"add",".");git(f.cwd,"commit","-qm","job "+n);const commit=git(f.cwd,"rev-parse","HEAD");
  const {plan}=planWithPreservation({operationKey:"fixture-"+n,repository:"fixture",baseCommit:over.base||base,commit,owned:[path],verifierAgentId:"agt_verifier",verifierRunId:"run_verifier",approvedMatrixDigest:approved,matrixApprovalMessageSeq:9},f.cwd);
  return {id:`rel_m${n}`,taskId:"tsk_fixture",entryId:`tqe_m${n}`,itemId:`wi_m${n}`,itemRevision:1,scopeRevision:1,orderMessageSeq:7,repository:"fixture",baseCommit:over.base||base,commit,verificationDigest:String(n).repeat(64),state:n===1?"claimed":"verified",generation:n===1?2:1,pauseGeneration:1,plan,...(n===1?{agentId:"agt_fixture",runId:"run_fixture"}:{})};
 };
 const settle=()=>git(f.cwd,"checkout","--quiet","--detach","tasks-hub");
 return {f:{...f,base},base,approved,accept,settle};
}
export const race=plan=>plan.checks.find(c=>c.id==="go-race").argv;
// The integrated plan as launchMatrixRun asks for it: the job's plan with the
// integrated commit and the deployer as verifier.
export const integratedPlan=(m,job,commit)=>planWithPreservation({...job.plan,commit,verifierAgentId:"agt_fixture",verifierRunId:"run_fixture"},m.f.cwd).plan;
export const covers=(...jobs)=>jobs.map(j=>({jobId:j.id,checks:j.plan.checks,matrixDigest:j.plan.matrixDigest}));
// A hub change selects the paired hub and bridge, which need pinned inputs.
export const matrixFake=lead=>{const a=batchFake(lead);a.prepare=async t=>["hub","bridge"].includes(t)?{...PAIRED}:{release:"fixture-release",artifactSHA256:"b".repeat(64)};return a;};
// A hub in a file: a fake tt that keeps the lead's record, replays a saved
// request id, and can refuse a call or lose its response after saving it.
export function batchHub(b,{refuse=[],lose={}}={}){
 const home=mkdtempSync(join(tmpdir(),"release-batch-hub-")),statePath=join(home,"hub.json"),tt=join(home,"tt");
 writeFileSync(statePath,JSON.stringify({job:b.lead,saved:{},executed:{},refuse,lose,log:[]}));
 writeFileSync(tt,"#!"+process.execPath+`
const fs=require('fs'),cp=require('child_process');const a=process.argv.slice(2),S=JSON.parse(fs.readFileSync(${JSON.stringify(statePath)},'utf8'));
const flag=n=>{const i=a.indexOf(n);return i<0?undefined:a[i+1];};
let head='';try{head=cp.execFileSync('git',['rev-parse','HEAD'],{encoding:'utf8',stdio:['ignore','pipe','ignore']}).trim();}catch{}
S.log.push({argv:a.join(' '),head});
const end=(value,code)=>{fs.writeFileSync(${JSON.stringify(statePath)},JSON.stringify(S));if(value===undefined)process.exit(code||2);console.log(JSON.stringify(value));process.exit(0);};
if(a[0]!=='deployment')end({});
if(a[1]==='get')end(S.job);
if(a[1]==='check')end(+flag('--generation')===S.job.generation?S.job:undefined);
if(a[1].startsWith('batch-')){
 const id=flag('--request-id'),name=a[1]+(flag('--entry')?':'+flag('--entry'):'');
 if(!S.saved[id]){
  if(S.refuse.includes(name)||+flag('--generation')!==S.job.generation)end(undefined);
  S.job={...S.job,generation:S.job.generation+1};S.saved[id]=S.job;S.executed[name]=(S.executed[name]||0)+1;
 }
 if(S.lose[name]>0){S.lose[name]--;end(undefined,3);}
 end(S.saved[id]);
}
end(undefined);
`);chmodSync(tt,0o755);
 const read=()=>JSON.parse(readFileSync(statePath,"utf8"));
 // A host adapter whose batch calls, fence and detail go through that tt.
 const adapter=()=>{
  const host=new HostAdapter({cwd:b.f.cwd,journalDirectory:home,tt},read().job),stub=fake();host.calls=stub.calls;
  for(const key of ["merged","prepare","deploy","check","rollback","finish","escalate","requestBug","block","refuse"])host[key]=stub[key];
  host.verifyIntegrated=async()=>{stub.calls.push("verify");return true;};host.verifyInputs=async()=>true;host.settleMatrixRuns=async()=>true;host.retain=async()=>{};
  return host;
 };
 const cfg=(max=3)=>({cwd:b.f.cwd,job:read().job,baselines:Object.fromEntries(["hub","bridge","mini","tailos"].map(t=>[t,b.f.base])),journalPath:join(home,b.lead.id+".json"),testPolicy:{startupMs:0,failures:1,intervalMs:0,relayCleanMs:0},batch:{max,importWaitMs:BATCH_IMPORT_WAIT_MS,candidates:b.members,onFallback:()=>{}}});
 const batchCalls=()=>read().log.filter(l=>/^deployment batch-/.test(l.argv));
 return {home,read,adapter,cfg,batchCalls,journal:()=>JSON.parse(readFileSync(join(home,b.lead.id+".json"),"utf8"))};
}
// A hub for the daemon: the ledger, claims, fence checks, merged and finish,
// and the batch calls unless it is an older hub.
export function daemonHub(b,{batchSupported=true,releaseBatch,unreadable=[]}={}){
 const home=mkdtempSync(join(tmpdir(),"release-batch-daemon-")),statePath=join(home,"hub.json"),tt=join(home,"tt"),configPath=join(home,"deploy.json");
 const jobs=[{...b.lead,state:"verified",generation:1,agentId:undefined,runId:undefined},...b.members];
 writeFileSync(statePath,JSON.stringify({jobs,log:[],batch:[]}));
 writeFileSync(tt,"#!"+process.execPath+"\n"+releaseReplySource+`
const fs=require('fs');const a=process.argv.slice(2),S=JSON.parse(fs.readFileSync(${JSON.stringify(statePath)},'utf8'));
const flag=n=>{const i=a.indexOf(n);return i<0?undefined:a[i+1];};
S.log.push(a.join(' '));
const end=value=>{fs.writeFileSync(${JSON.stringify(statePath)},JSON.stringify(S));if(value===undefined)process.exit(2);console.log(JSON.stringify(value));process.exit(0);};
if(a[0]!=='deployment')end({});
if(a[1]==='handler')end({id:'agt_0123abcd'});
if(a[1]==='get'&&${JSON.stringify(unreadable)}.includes(flag('--job')))end({id:flag('--job')});
if(['list','get'].includes(a[1]))end(releaseReply(S.jobs,a));
const at=S.jobs.findIndex(j=>j.id===flag('--job')),job=S.jobs[at];
if(!job||(a[1]!=='check'&&+flag('--generation')!==job.generation))end(undefined);
const save=next=>{S.jobs[at]=next;end(next);};
if(a[1]==='check')end(+flag('--generation')===job.generation?job:undefined);
if(a[1]==='claim')save({...job,state:'claimed',generation:job.generation+1,agentId:process.env.TAILTERM_AGENT,runId:process.env.TAILTERM_RUN});
if(a[1]==='merged')save({...job,state:'merged',generation:job.generation+1,integratedCommit:flag('--commit'),published:true});
if(a[1]==='finish'){const receipt=JSON.parse(fs.readFileSync(flag('--file'),'utf8'));save({...job,state:receipt.outcome,generation:job.generation+1,receipt,settledAt:'2026-10-06T16:00:00Z'});}
if(a[1].startsWith('batch-')&&${JSON.stringify(batchSupported)}){S.batch.push(a[1]+(flag('--entry')?':'+flag('--entry'):''));save({...job,generation:job.generation+1});}
end(undefined);
`);chmodSync(tt,0o755);
 const config={version:1,enabled:true,cwd:b.f.cwd,journalDirectory:home,tt,baselines:Object.fromEntries(["hub","bridge","mini","tailos"].map(t=>[t,b.f.base])),testPolicy:{startupMs:0,failures:1,intervalMs:0,relayCleanMs:0},...(releaseBatch===undefined?{}:{releaseBatch})};
 writeFileSync(configPath,JSON.stringify(config));
 const read=()=>JSON.parse(readFileSync(statePath,"utf8"));
 // The real runRelease over the daemon's own adapter: only the host work
 // (matrix, inputs, artifacts, probes) is stubbed.
 const seen=[];
 const release=(c,adapter)=>{
  seen.push(c);const stub=fake();
  for(const key of ["prepare","deploy","check","rollback","escalate","requestBug"])adapter[key]=stub[key];
  adapter.verifyIntegrated=async()=>true;adapter.verifyInputs=async()=>true;adapter.settleMatrixRuns=async()=>true;adapter.retain=async()=>{};
  return runRelease({...c,testPolicy:config.testPolicy},adapter);
 };
 return {home,config,configPath,read,release,seen,poll:()=>serveDeployment(config,{once:true,configPath,release})};
}
// What the published runner (aff59de) sends for one verified job, three more
// queued behind it and a diverged tasks-hub: recorded from that runner.
export const SINGLE_JOB_TRACE=["list --view active","list --view settled","get --job rel_b1","list --view active","list --view active","claim --job rel_b1 --generation 1","check --job rel_b1 --generation 2","check --job rel_b1 --generation 2","check --job rel_b1 --generation 2","check --job rel_b1 --generation 2","merged --job rel_b1 --generation 2","check --job rel_b1 --generation 3","check --job rel_b1 --generation 3","check --job rel_b1 --generation 3","finish --job rel_b1 --generation 3"];
export const traceOf=log=>log.filter(l=>l.startsWith("deployment ")).map(l=>l.split(" ").slice(1).filter((w,i,all)=>!["--limit","--snapshot","--request-id","--commit","--file"].includes(w)&&!["--limit","--snapshot","--request-id","--commit","--file"].includes(all[i-1])).join(" "));
// The batching exclusion record (wi_a72bba736a8c1daf): every queued candidate
// the daemon leaves out at claim is in the lead's journal with its reason.
export const GO_CHECKS=[{id:"go-vet",argv:["go","vet","./..."],cwd:"hub",environment:{}},{id:"go-test",argv:["go","test","./..."],cwd:"hub",environment:{}}];
export const BROWSER_CHECK={id:"tests/board-browser.mjs",argv:["node","tests/board-browser.mjs"],cwd:".",environment:{TEST_BROWSER:"1"}};
// One job per check list, each on the published base; the first is the lead.
export const planned=(...checks)=>{
 const b=batchJobs(checks.map((_,i)=>[`client/${"abcdef"[i]}.js`,"abcdef"[i]]));
 for(const [i,job] of [b.lead,...b.members].entries())if(checks[i])job.plan={...job.plan,checks:structuredClone(checks[i])};
 return b;
};
export const daemonJournal=h=>JSON.parse(readFileSync(join(h.home,"rel_b1.json"),"utf8"));
export const MISMATCH=[{jobId:"rel_b2",reason:"batch-plan-mismatch"}];
// Integrated check selection (wi_e30d4baf0243a036): the plan selects from the
// tasks-hub tip the candidates were integrated onto, not from the job's base.
export const MATRIX_SCRIPT=new URL("../scripts/verify-matrix.mjs",import.meta.url).pathname;
// tasks-hub moves through another release after the jobs were accepted.
export const moveTip=(m,path="hub/internal/store/store.go",text="package store\n\nvar Released = 1\n")=>{
 git(m.f.cwd,"checkout","--quiet","tasks-hub");writeFileSync(join(m.f.cwd,path),text);git(m.f.cwd,"add",".");git(m.f.cwd,"commit","-qm","another release");
 const tip=git(m.f.cwd,"rev-parse","HEAD");m.settle();return tip;
};
// What the host adapter does with the job the runner hands it: the saved
// context, and the plan and record the real plan command writes from it.
export async function hostPlan(m,handed){
 const home=mkdtempSync(join(tmpdir(),"release-selection-host-"));
 const {integratedCommit,selection,batchCovers,...record}=handed;
 const host=new HostAdapter({cwd:m.f.cwd,journalDirectory:home},{...record});const started=[];let stderr="";
 host.processStartTime=()=>null;host.startMatrixRun=(argv,dir)=>{started.push(argv);return exitedPid();};
 host.command=argv=>{
  if(argv[1]==="deployment"&&argv[2]==="get")return JSON.stringify(record);
  if(argv[1]==="scripts/verify-matrix.mjs"&&argv[2]==="plan"){const r=spawnSync(process.execPath,[MATRIX_SCRIPT,"plan",argv[3],argv[4]],{cwd:m.f.cwd,encoding:"utf8"});stderr=r.stderr;if(r.status!==0)throw new Error(r.stderr);}
  return "";
 };
 assert.equal(await host.verifyIntegrated(handed),false);
 const dir=join(home,handed.id+"-integrated-verification",handed.integratedCommit+"-r0"),read=n=>JSON.parse(readFileSync(join(dir,n),"utf8"));
 return {context:read("context.json"),plan:read("plan.json"),record:read("plan.preserved.json"),started,stderr};
}
export const checkIds=plan=>plan.checks.map(c=>c.id);
// A second docs job from the base, where docs/ does not exist yet.
export const acceptDocs=(m,n,path,text)=>{m.settle();mkdirSync(join(m.f.cwd,"docs"),{recursive:true});return m.accept(n,path,text);};

// A generation the hub advanced under the claim (wi_395d07d432a93bc7, g1-g4).
// stale() is the error the adapter throws for the hub's "generation changed".
export const stale=()=>Object.assign(new Error("Host operation failed"),{generationChanged:true});
export function claimed(f,commit){return {...job(f,commit),agentId:"agt_fixture",runId:"run_fixture"};}
// fence answers "generation-changed" on the listed calls (1-based), else true.
export function advancing(staleCalls,reread=true){
 const a=fake();a.fences=0;a.rereads=0;
 a.fence=async()=>staleCalls.includes(++a.fences)?"generation-changed":true;
 a.adoptGeneration=async()=>{a.rereads++;return reread;};
 return a;
}
export const journalAt=c=>JSON.parse(readFileSync(c.journalPath,"utf8"));
// A job refused at integrate: its plan names another commit.
export function refusable(f){const j=claimed(f,change(f,"client/a.js","a"));j.plan.commit=f.base;return j;}
// The host adapter against a CLI that answers like the hub: check and refuse
// succeed only at the job's generation, and a stale one gets the 409 text.
// advanceOn: the first call of that operation finds the generation moved on.
export function nativeFixture(hubJob,{conflict="conflict: release: generation changed",advanceOn=""}={}){
 const home=mkdtempSync(join(tmpdir(),"release-generation-")),tt=join(home,"tt"),log=join(home,"calls"),state=join(home,"job.json");
 writeFileSync(state,JSON.stringify(hubJob));
 writeFileSync(tt,"#!"+process.execPath+`
const fs=require('fs'),a=process.argv.slice(2),flag=n=>a.includes(n)?a[a.indexOf(n)+1]:'',job=JSON.parse(fs.readFileSync(${JSON.stringify(state)},'utf8'));
fs.appendFileSync(${JSON.stringify(log)},a.slice(0,2).join(' ')+' '+flag('--generation')+'\\n');
if(a[1]===${JSON.stringify(advanceOn)} && !fs.existsSync(${JSON.stringify(state)}+'.advanced')){fs.writeFileSync(${JSON.stringify(state)}+'.advanced','');job.generation++;fs.writeFileSync(${JSON.stringify(state)},JSON.stringify(job));}
if(a[1]==='get'){console.log(JSON.stringify(job));process.exit(0);}
if(+flag('--generation')!==job.generation){process.stderr.write('tt: hub: 409 '+${JSON.stringify(conflict)}+'\\n');process.exit(1);}
if(a[1]==='refuse'){job.state='refused';job.generation++;}
console.log(JSON.stringify(job));`);chmodSync(tt,0o755);
 return {home,config:{cwd:home,journalDirectory:home,tt},calls:()=>readFileSync(log,"utf8").trim().split("\n").map(l=>l.trim()),set:j=>writeFileSync(state,JSON.stringify(j))};
}
export const hubJob=(generation,more={})=>({id:"rel_fixture",taskId:"tsk_fixture",entryId:"qe_fixture",itemId:"wi_fixture",itemRevision:1,scopeRevision:1,orderMessageSeq:7,repository:"fixture",baseCommit:"a".repeat(40),commit:"b".repeat(40),verificationDigest:"c".repeat(64),pauseGeneration:1,state:"claimed",generation,agentId:"agt_fixture",runId:"run_fixture",...more});

// Wake class (wi_991125f66a787459). Every notice the runner posts with no
// recipient carries one wake ref; the relay wakes nobody for wake=routine.
export const wakeRefs=argv=>argv.filter((w,i)=>argv[i-1]==="--ref" && w.startsWith("wake="));
export const noticeArgv=h=>h.calls().map(c=>c.argv).filter(a=>a[0]==="send" && a[a.indexOf("--kind")+1]==="notice");
export const argvField=(a,n)=>a[a.indexOf(n)+1];
export const WAKE_ATTEMPT="c".repeat(40)+"-r0",WAKE_WAIT={attempt:WAKE_ATTEMPT,position:1,length:2,priority:"high",holders:[{id:"h1",item:"wi_a",agent:"a",pid:1}],change:1},WAKE_HELD={attempt:WAKE_ATTEMPT,pid:1,groups:[],reason:"fixture"};
// The notice sends of a runner source that pass no wake ref, as 1-based line
// numbers, and how many notice sends it has. The held claim send carries the
// ref on its Board copy only, which wake a1 pins on the argv.
export function untaggedNoticeSends(source){
 const sends=source.split("\n").map((text,i)=>({line:i+1,text})).filter(l=>/"--kind",\s*"notice"/.test(l.text));
 return {count:sends.length,untagged:sends.filter(l=>!/\.\.\.(?:wakeRef\((?:notice\.wake|WAKE\.(?:routine|attention))\)|\(to\?\[\]:wakeRef\(notice\.wake\)\))\]\)/.test(l.text)).map(l=>l.line)};
}
