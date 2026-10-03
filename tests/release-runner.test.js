import test from "node:test";
import assert from "node:assert/strict";
import {mkdtempSync,mkdirSync,writeFileSync,readFileSync,chmodSync,existsSync,statSync,readdirSync,rmSync,symlinkSync,renameSync} from "node:fs";
import {tmpdir} from "node:os";
import {join,dirname} from "node:path";
import {execFileSync,spawn,spawnSync} from "node:child_process";
import {integrateCandidate,publishIntegration,runRelease,liveCheck,runnableJob,HostAdapter,serveDeployment,hostLockNames,retentionPolicy,pruneJournal,reconcileHostLocks,revertCommit,moveReleaseRef,tasksHubCheckedOut,releaseError,failureReason,failureDetail,MATRIX_PREREQUISITES,MATRIX_HOST_WAIT_MS,MATRIX_LAUNCH_GRACE_MS,MATRIX_STOP_GRACE_MS,MATRIX_DEADLINE_SLACK_MS,missingPrerequisites,provisionPrerequisites,matrixRunTimeout,matrixPriority,matrixWaitNotice,matrixHeldNotice,fenceWaitNotice,matrixRunUnsettled} from "../scripts/release-runner.mjs";
import {acquireHostLock,readHostState as rawReadHostState,holdersOf,readJournal,updateHostState,pidGone,groupGone,RUN_TIMEOUT_GRACE_MS,DEFAULT_HOLDER_CAP_MS} from "../scripts/verify-matrix-host-lock.mjs";
import {planRunTimeout,readPrerequisites} from "../scripts/verify-matrix.mjs";
import {createHash} from "node:crypto";
import {renderTeamDelivery} from "../client/team-delivery-view.js";
import {targetsForPaths,selectReleaseTargets} from "../scripts/release-targets.mjs";
// No test here may fall back to the host's own verification lock file.
process.env.TAILTERM_MATRIX_HOST_LOCK=join(mkdtempSync(join(tmpdir(),"release-matrix-lock-")),"host.json");
process.env.TAILTERM_MATRIX_MAX_HOLDERS = "1";
const readHostState = (...args) => {
  const state = rawReadHostState(...args);
  const held = holdersOf(state);
  assert(held.length <= 1, "legacy capacity-one fixture has at most one holder");
  return state ? { ...state, holder: held[0] || null } : state;
};
delete process.env.TAILTERM_MATRIX_PRIORITY;delete process.env.TAILTERM_MATRIX_HOLDER_CAP_MINUTES;
const git=(cwd,...args)=>execFileSync("git",args,{cwd,encoding:"utf8",stdio:["ignore","pipe","pipe"]}).trim();
function bufferTT(t,body){
 const cwd=mkdtempSync(join(tmpdir(),"release-buffer-")),tt=join(cwd,"tt"),log=join(cwd,"calls"),configPath=join(cwd,"config.json");
 t.after(()=>rmSync(cwd,{recursive:true,force:true}));
 writeFileSync(tt,"#!"+process.execPath+"\n"+`const fs=require('fs');const args=process.argv.slice(2);fs.appendFileSync(${JSON.stringify(log)},args.join(' ')+'\\n');if(args.join(' ')!=='deployment list')process.exit(99);`+body);chmodSync(tt,0o755);
 writeFileSync(configPath,JSON.stringify({version:1,enabled:true,cwd,journalDirectory:cwd,tt,baselines:Object.fromEntries(["hub","bridge","mini","tailos"].map(x=>[x,"a".repeat(40)]))}));
 const poll=()=>spawnSync(process.execPath,[RUNNER,"--config",configPath,"--once"],{cwd,encoding:"utf8",stdio:["ignore","pipe","pipe"],timeout:30000});
 return {cwd,tt,log,poll};
}
test("buf1 a valid fake tt deployment list above eight MiB parses and polls",t=>{
 const f=bufferTT(t,`fs.writeSync(1,JSON.stringify([{id:'rel_fixture',state:'refused',padding:'x'.repeat(9*1024*1024)}]));`);
 const adapter=new HostAdapter({cwd:f.cwd,tt:f.tt},{}),raw=adapter.command([f.tt,"deployment","list"]);
 assert.ok(Buffer.byteLength(raw)>8*1024*1024);const jobs=JSON.parse(raw);
 assert.equal(jobs.length,1);assert.equal(jobs[0].padding.length,9*1024*1024);assert.equal(jobs[0].id,"rel_fixture");
 const poll=f.poll();assert.equal(poll.status,0);assert.equal(poll.stdout,"");assert.equal(poll.stderr,"");
 assert.equal(readFileSync(f.log,"utf8"),"deployment list\ndeployment list\n");
});
test("buf2 real fake tt command overflow names ENOBUFS without payloads or claims",t=>{
 const f=bufferTT(t,`fs.writeSync(2,'SYNTHETIC_PRIVATE_TOKEN');const chunk=Buffer.alloc(1024*1024,120);for(let i=0;i<257;i++)fs.writeSync(2,chunk);`);
 const adapter=new HostAdapter({cwd:f.cwd},{});
 assert.throws(()=>adapter.command([f.tt,"deployment","list"]),e=>e.message==="Host operation failed" && failureReason(e)==="tt ENOBUFS (command buffer overflow)");
 const poll=f.poll();assert.equal(poll.status,0);assert.equal(poll.stdout,"");const diagnostic=poll.stderr;
 assert.match(diagnostic,/Deployment poll held.*ENOBUFS.*command buffer overflow/);assert.ok(diagnostic.length<256);assert.ok(!diagnostic.includes("SYNTHETIC_PRIVATE_TOKEN"));
 assert.equal(readFileSync(f.log,"utf8"),"deployment list\ndeployment list\n");
});
test("buf3 fake tt errors and invalid JSON keep bounded non-secret poll diagnostics",t=>{
 for(const body of [`fs.writeSync(1,'SYNTHETIC_PRIVATE_TOKEN');fs.writeSync(2,'SYNTHETIC_PRIVATE_TOKEN');process.exit(2);`,`fs.writeSync(1,'SYNTHETIC_PRIVATE_TOKEN');`]){
  const f=bufferTT(t,body),poll=f.poll();assert.equal(poll.status,0);assert.equal(poll.stdout,"");
  const diagnostic=poll.stderr;assert.match(diagnostic,/Deployment poll held/);assert.ok(diagnostic.length<256);assert.ok(!diagnostic.includes("SYNTHETIC_PRIVATE_TOKEN"));
  assert.match(diagnostic,body.includes("process.exit")?/tt exit 2/:/unclassified/);assert.equal(readFileSync(f.log,"utf8"),"deployment list\n");
 }
});
function fixture(){const cwd=mkdtempSync(join(tmpdir(),"release-git-")),origin=mkdtempSync(join(tmpdir(),"release-origin-"));git(origin,"init","--bare","-b","tasks-hub");git(cwd,"init","-b","tasks-hub");git(cwd,"config","user.email","fixture@example.invalid");git(cwd,"config","user.name","Fixture");mkdirSync(join(cwd,"client"));writeFileSync(join(cwd,"client/base.js"),"base");git(cwd,"add",".");git(cwd,"commit","-m","base");const base=git(cwd,"rev-parse","HEAD");git(cwd,"remote","add","origin",origin);git(cwd,"push","--quiet","origin","tasks-hub");git(cwd,"checkout","-b","candidate");return {cwd,base,origin};}
function change(f,file,text){writeFileSync(join(f.cwd,file),text);git(f.cwd,"add",".");git(f.cwd,"commit","-m","candidate");return git(f.cwd,"rev-parse","HEAD");}
function job(f,commit){return {id:"rel_fixture",state:"claimed",commit,baseCommit:f.base,verificationDigest:"a".repeat(64),plan:{commit}};}
test("fast forward pins exact candidate and ref CAS refuses a race",()=>{const f=fixture();const commit=change(f,"client/a.js","a");const out=integrateCandidate(f.cwd,job(f,commit));assert.equal(out.integrated,commit);git(f.cwd,"update-ref","refs/heads/tasks-hub",commit,f.base);assert.throws(()=>publishIntegration(f.cwd,commit,f.base),/race/);});
test("full divergent series is cherry picked and conflict leaves ref unchanged",()=>{const f=fixture();change(f,"client/a.js","a");const commit=change(f,"client/b.js","b");git(f.cwd,"checkout","tasks-hub");change(f,"client/c.js","c");const prior=git(f.cwd,"rev-parse","HEAD");const out=integrateCandidate(f.cwd,job(f,commit));assert.notEqual(out.integrated,commit);assert.equal(readFileSync(join(f.cwd,"client/b.js"),"utf8"),"b");assert.equal(git(f.cwd,"rev-parse","tasks-hub"),prior);
const g=fixture();const conflicting=change(g,"client/base.js","candidate");git(g.cwd,"checkout","tasks-hub");change(g,"client/base.js","release");assert.throws(()=>integrateCandidate(g.cwd,job(g,conflicting)),/integration-conflict/);assert.equal(git(g.cwd,"status","--porcelain"),"");});
test("mismatched verified SHA is refused",()=>{const f=fixture();const commit=change(f,"client/a.js","a");const j=job(f,commit);j.plan.commit=f.base;assert.throws(()=>integrateCandidate(f.cwd,j),/binding/);});
test("target selection handles rename/delete and rejects unknown paths",()=>{assert.deepEqual(targetsForPaths(["hub/internal/api/releases.go"]),["hub","bridge","mini"]);assert.deepEqual(targetsForPaths(["docs/release.md"]),[]);assert.throws(()=>targetsForPaths(["unknown.xyz"]),/Unknown/);const f=fixture();const commit=change(f,"client/a.js","a");git(f.cwd,"mv","client/base.js","client/moved.js");git(f.cwd,"commit","-m","rename");assert.deepEqual(selectReleaseTargets(f.cwd,Object.fromEntries(["hub","bridge","mini","tailos"].map(t=>[t,f.base])),git(f.cwd,"rev-parse","HEAD")),["tailos"]);});
function fake(){const calls=[];return {calls,fence:async()=>true,verifyIntegrated:async()=>true,merged:async()=>calls.push("merged"),prepare:async t=>({release:"fixture-release",artifactSHA256:"b".repeat(64)}),deploy:async t=>calls.push("deploy:"+t),check:async()=>true,rollback:async t=>{calls.push("rollback:"+t);return true;},finish:async r=>calls.push("finish"),escalate:async()=>calls.push("escalate"),requestBug:async()=>{calls.push("bug");return "rel_fixture-rollback-bug";},block:async()=>calls.push("block"),refuse:async()=>calls.push("refuse")};}
function config(f,j){const home=mkdtempSync(join(tmpdir(),"release-journal-"));return {cwd:f.cwd,job:j,baselines:Object.fromEntries(["hub","bridge","mini","tailos"].map(t=>[t,f.base])),journalPath:join(home,"journal.json"),testPolicy:{startupMs:0,failures:1,intervalMs:0,relayCleanMs:0}};}
test("successful receipt retry does not repeat effects",async()=>{const f=fixture(),j=job(f,change(f,"client/a.js","a")),a=fake(),c=config(f,j);const r=await runRelease(c,a);assert.equal(r.outcome,"released");assert.deepEqual(a.calls,["merged","deploy:tailos","finish"]);assert.deepEqual(await runRelease(c,a),r);assert.deepEqual(a.calls,["merged","deploy:tailos","finish"]);});
test("failed live check rolls back once and ambiguous retry preserves journal",async()=>{const f=fixture(),j=job(f,change(f,"client/a.js","a")),a=fake(),c=config(f,j);a.check=async()=>"identity";await assert.rejects(runRelease(c,a),/failed/);assert.deepEqual(a.calls,["merged","deploy:tailos","rollback:tailos","escalate","bug","finish"]);const before=readFileSync(c.journalPath,"utf8");await assert.rejects(runRelease(c,a),/Ambiguous/);assert.equal(readFileSync(c.journalPath,"utf8"),before);});
test("changed integrated SHA refuses deploy without imported matrix",async()=>{const f=fixture(),j=job(f,change(f,"client/a.js","a"));git(f.cwd,"checkout","tasks-hub");change(f,"client/c.js","c");const a=fake();a.verifyIntegrated=async()=>false;assert.equal((await runRelease(config(f,j),a)).outcome,"waiting_matrix");assert.ok(!a.calls.some(c=>c.startsWith("deploy:")));});
test("failed rollback blocks and synthetic secret error is absent from journal",async()=>{const f=fixture(),j=job(f,change(f,"client/a.js","a")),a=fake(),c=config(f,j);a.deploy=async()=>{throw new Error("SYNTHETIC_PRIVATE_TOKEN");};a.rollback=async()=>false;await assert.rejects(runRelease(c,a));const raw=readFileSync(c.journalPath,"utf8");assert.ok(!raw.includes("SYNTHETIC_PRIVATE_TOKEN"));assert.equal(JSON.parse(raw).outcome,"blocked");});
test("bounded readiness and immediate hard failures",async()=>{let now=0,calls=0;const a={check:async()=>{calls++;return false;}};assert.equal(await liveCheck(a,"hub",{startupMs:10,failures:3,intervalMs:5},async ms=>{now+=ms;},()=>now),false);assert.equal(calls,5);a.check=async()=>"integrity";assert.equal(await liveCheck(a,"hub",{startupMs:60000,failures:3,intervalMs:5},async()=>assert.fail()),false);});

test("competing local runners share the project lock even with different journals",async()=>{
 const f=fixture(),j=job(f,change(f,"client/a.js","a")),a=fake(),c=config(f,j);let entered,release;
 const ready=new Promise(r=>entered=r),gate=new Promise(r=>release=r);a.deploy=async()=>{entered();await gate;};
 const first=runRelease(c,a);await ready;await assert.rejects(runRelease(config(f,j),fake()),/locked/);release();await first;
});
test("waiting integrated matrix resumes without repeating integration",async()=>{
 const f=fixture(),j=job(f,change(f,"client/a.js","a"));git(f.cwd,"checkout","tasks-hub");change(f,"client/c.js","c");const c=config(f,j),a=fake();a.verifyIntegrated=async()=>false;
 assert.equal((await runRelease(c,a)).outcome,"waiting_matrix");const sha=git(f.cwd,"rev-parse","HEAD");a.verifyIntegrated=async()=>true;const receipt=await runRelease(c,a);assert.equal(receipt.commit,sha);assert.equal(receipt.outcome,"released");
});
test("schema rehearsal and external backup pin gate every TrueNAS mutation",async()=>{
 const f=fixture();mkdirSync(join(f.cwd,"hub/internal/api"),{recursive:true});const j=job(f,change(f,"hub/internal/api/fixture.go","fixture"));const c=config(f,j),a=fake();
 a.prepare=async()=>({release:"fixture",artifactSHA256:"b".repeat(64),backup:"copy",backupSHA256:"c".repeat(64),preflightReceiptSHA256:"d".repeat(64),schemaChanged:true});a.rehearse=async()=>false;
 await assert.rejects(runRelease(c,a));assert.ok(!a.calls.some(x=>x.startsWith("deploy:")));
});

test("daemon prioritizes its waiting claim and respects another run or blocked fence",()=>{
 const waiting={id:"waiting",state:"claimed",agentId:"agent",runId:"run"},queued={id:"next",state:"verified"};
 assert.equal(runnableJob([queued,waiting],"agent","run"),waiting);
 assert.equal(runnableJob([queued,waiting],"agent","rotated"),null);
 assert.equal(runnableJob([queued,{state:"blocked"}],"agent","run"),null);
});
test("uncertain final receipt storage retries the receipt and never rolls back verified targets",async()=>{
 const f=fixture(),j=job(f,change(f,"client/a.js","a")),a=fake(),c=config(f,j);let finishes=0;
 a.finish=async()=>{finishes++;if(finishes===1)throw new Error("response lost");};
 assert.equal((await runRelease(c,a)).outcome,"receipt_pending");assert.ok(!a.calls.some(x=>x.startsWith("rollback:")||x==="escalate"));
 const result=await runRelease(c,a);assert.equal(result.outcome,"released");assert.equal(finishes,2);assert.equal(a.calls.filter(x=>x==="deploy:tailos").length,1);
});

// The Mini tests always supply host setup stubs, so no test runs a real
// tt host setup; the artifact is a text file, so an omitted stub fails.
function miniFixture(over={}){
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
test("host Mini adapter installs through host setup and restores the prior binary without changing fixture database",async()=>{
 const {adapter,a,install,db,restarts,probes}=miniFixture(()=>({}));
 await adapter.deploy("mini",a);assert.equal(readFileSync(install,"utf8"),"new CLI");assert.equal(readFileSync(a.rollbackPath,"utf8"),"old CLI");
 assert.equal(await adapter.rollback("mini"),true);assert.equal(readFileSync(install,"utf8"),"old CLI");assert.equal(readFileSync(db,"utf8"),"newer writes");
 assert.equal(existsSync(restarts),false,"a host rollback that restored the prior binary needs no separate relay restart");assert.equal(readFileSync(probes,"utf8"),"probe\n");
});
test("host Mini rollback restores the journal copy when the host rollback command fails or leaves other bytes",async()=>{
 for(const [name,hostRollback] of [["exits non-zero",node=>node("process.exit(1)")],["leaves a wrong tt.previous",(node,f)=>node("require('fs').writeFileSync(process.argv[1],'some other CLI')",f.install)]]){
  const {adapter,a,install,db,restarts,probes}=miniFixture((node,f)=>({hostRollback:hostRollback(node,f)}));
  await adapter.deploy("mini",a);assert.equal(readFileSync(install,"utf8"),"new CLI",name);
  assert.equal(await adapter.rollback("mini"),true,name);assert.equal(readFileSync(install,"utf8"),"old CLI",name);assert.equal(hash(readFileSync(install)),a.priorArtifactSHA256,name);
  assert.equal(readFileSync(restarts,"utf8"),"restart\n",name+": the journal restore restarts the relay");assert.equal(readFileSync(probes,"utf8"),"probe\n",name);assert.equal(readFileSync(db,"utf8"),"newer writes",name);
 }
 // A changed journal copy is still refused before any host command runs.
 const {adapter,a,install,probes}=miniFixture(()=>({}));await adapter.deploy("mini",a);writeFileSync(a.rollbackPath,"tampered");
 assert.equal(await adapter.rollback("mini"),false);assert.equal(readFileSync(install,"utf8"),"new CLI");assert.equal(existsSync(probes),false);
});
test("host Mini deploy that fails in host setup rolls back to the prior digest",async()=>{
 const failing=miniFixture((node,f)=>({hostSetup:node("const fs=require('fs');fs.copyFileSync(process.argv[2],process.argv[1]);process.exit(1)",f.install,f.artifact)}));
 await assert.rejects(failing.adapter.deploy("mini",failing.a),/Host operation failed/);assert.equal(readFileSync(failing.install,"utf8"),"new CLI","host setup failed after installing");
 assert.equal(await failing.adapter.rollback("mini"),true);assert.equal(hash(readFileSync(failing.install)),failing.a.priorArtifactSHA256);assert.equal(readFileSync(failing.restarts,"utf8"),"restart\n");assert.equal(readFileSync(failing.db,"utf8"),"newer writes");
 const idle=miniFixture(node=>({hostSetup:node("process.exit(0)")}));
 await assert.rejects(idle.adapter.deploy("mini",idle.a),/does not match the pinned artifact/);assert.equal(readFileSync(idle.install,"utf8"),"old CLI");
 // With no configured stub the runner runs the candidate's own host setup,
 // and the installed tt's rollback.
 const bare=miniFixture(()=>({hostSetup:undefined,hostRollback:undefined}));const argvs=[],real=bare.adapter.command.bind(bare.adapter);
 bare.adapter.command=(argv,cwd)=>{argvs.push(argv);if(argv.includes("host"))throw new Error("Host operation failed");return real(argv,cwd);};
 await assert.rejects(bare.adapter.deploy("mini",bare.a),/Host operation failed/);assert.equal(await bare.adapter.rollback("mini"),true);
 assert.deepEqual(argvs.slice(0,2),[[bare.artifact,"host","setup","--from",bare.artifact],[bare.install,"host","setup","--rollback"]]);assert.equal(readFileSync(bare.install,"utf8"),"old CLI");
});
test("host receipt adapter writes its receipt beneath the provisioned journal directory",async()=>{
 const cwd=mkdtempSync(join(tmpdir(),"receipt-adapter-")),adapter=new HostAdapter({cwd,journalDirectory:cwd},{id:"rel_fixture",generation:5});let args;
 adapter.command=argv=>{args=argv;return JSON.stringify({id:"rel_fixture",generation:6,state:"released"});};
 const receipt={version:1,jobId:"rel_fixture",outcome:"released"};await adapter.finish(receipt,5);
 assert.deepEqual(JSON.parse(readFileSync(join(cwd,"rel_fixture-receipt.json"),"utf8")),receipt);assert.ok(args.includes("rel_fixture-finish-5"));
});

const hash=b=>createHash("sha256").update(b).digest("hex");
const stamped=(commit,modified="false")=>`x: go1.26\n\tbuild\tvcs=git\n\tbuild\tvcs.revision=${commit}\n\tbuild\tvcs.modified=${modified}\n`;
function importInputs(adapter,commit,targets){
 const inputs={version:1,jobId:adapter.job.id,commit,acceptedCommit:adapter.job.commit,verificationDigest:adapter.job.verificationDigest,targets};
 const path=join(adapter.config.journalDirectory,adapter.job.id+"-inputs.json"),raw=JSON.stringify(inputs);writeFileSync(path,raw);
 adapter.job.inputsCommit=commit;adapter.job.inputsDigest=hash(raw);return path;
}
function hostFixture(){
 const f=fixture();mkdirSync(join(f.cwd,"hub/cmd/tt"),{recursive:true});mkdirSync(join(f.cwd,"hub/internal/store"),{recursive:true});
 const commit=change(f,"hub/cmd/tt/main.go","fixture");const home=mkdtempSync(join(tmpdir(),"release-host-inputs-")),install=join(home,"tt");writeFileSync(install,"v1");
 const config={cwd:f.cwd,journalDirectory:home,baselines:Object.fromEntries(["hub","bridge","mini","tailos"].map(t=>[t,f.base])),targets:{mini:{installPath:install,rollbackPath:join(home,"stale-before"),release:"stale-release",hostSetup:["host-setup-stub"],hostRollback:["host-rollback-stub"],relayRestart:[process.execPath,"-e","process.exit(0)"],rollbackProbe:[process.execPath,"-e","console.log(JSON.stringify({restored:true,databaseWritesPreserved:true}))"]}}};
 return {f,commit,home,install,config};
}
test("b1 two consecutive jobs share stable config and restore the exact prior-live Mini",async()=>{
 const {f,commit,home,install,config}=hostFixture();
 for(const [id,version] of [["rel_first","v2"],["rel_second","v3"]]){
  const adapter=new HostAdapter(config,{...job(f,commit),id});const release=id+"-"+commit.slice(0,12)+"-mini";
  const manifest=importInputs(adapter,commit,{mini:{release,rollbackSafe:true}});
  let restarts=0;
  adapter.command=(argv)=>{if(argv[1]==="version")return stamped(commit);if(argv.includes("build")){writeFileSync(argv[argv.indexOf("-o")+1],version);return "";}if(argv[0]==="host-setup-stub"){writeFileSync(install,version);if(id==="rel_second")throw new Error("Synthetic second host setup failure");return "";}if(argv[0]==="host-rollback-stub")throw new Error("Synthetic missing rollback copy");if(argv.includes("process.exit(0)"))restarts++;return JSON.stringify({restored:true,databaseWritesPreserved:true});};
  const artifact=await adapter.prepare("mini",commit);
  if(id==="rel_second")await assert.rejects(adapter.deploy("mini",artifact),/Synthetic second/);else await adapter.deploy("mini",artifact);
  assert.equal(readFileSync(install,"utf8"),version);
  if(id==="rel_second"){assert.equal(readFileSync(artifact.rollbackPath,"utf8"),"v2");assert.equal(await adapter.rollback("mini"),true);assert.equal(readFileSync(install,"utf8"),"v2");assert.equal(restarts,1,"the journal restore restarts the relay");}
  writeFileSync(manifest,"{}");assert.throws(()=>adapter.jobInputs(commit),/digest/);
 }
});
test("b1 schema rehearsal builds the exact candidate host binary and rejects stale job backups",async()=>{
 const {f,home,config}=hostFixture();const commit=change(f,"hub/internal/store/migrate.go","candidate schema"),id="rel_schema";
 config.targets.hub={migrationBinary:join(home,"stale-migration"),schemaChanged:false};const adapter=new HostAdapter(config,{...job(f,commit),id});
 const backup=join(home,id+"-backup"),pin=join(home,"preflight.json"),plan=join(home,"plan.json");writeFileSync(backup,"backup");writeFileSync(pin,"{}");
 const release=id+"-"+commit.slice(0,12)+"-hub";writeFileSync(plan,JSON.stringify({backupDestination:backup,deployment:{releaseName:release,targets:["hub"],binaryDestination:`/mnt/deepfreeze/tailterm-hub/releases/${release}/tailterm-hub`}}));
 const input={release,backupJobId:id,backup,backupCopy:backup,backupSHA256:hash("backup"),preflightReceipt:pin,preflightReceiptSHA256:hash("{}"),planPath:plan,rollbackSafe:true};
 importInputs(adapter,commit,{hub:input});let buildHeads=[],migrationArgs;
 adapter.command=argv=>{if(argv[1]==="version")return stamped(commit);if(argv.includes("build")){buildHeads.push(git(f.cwd,"rev-parse","HEAD"));writeFileSync(argv[argv.indexOf("-o")+1],"candidate migration/binary");return "";}migrationArgs=argv;return "";};
 const artifact=await adapter.prepare("hub",commit);assert.equal(artifact.schemaChanged,true);assert.equal(buildHeads.length,2);assert.deepEqual(buildHeads,[commit,commit]);assert.notEqual(artifact.migrationBinary,config.targets.hub.migrationBinary);
 await adapter.rehearse(artifact);assert.equal(migrationArgs[0],artifact.migrationBinary);assert.notEqual(migrationArgs[2],backup);assert.equal(readFileSync(backup,"utf8"),"backup");
 importInputs(adapter,commit,{hub:{...input,backupJobId:"rel_previous"}});await assert.rejects(adapter.prepare("hub",commit),/backup identity/);
});

// Journal retention (j1-j3). A rehearsal fixture: an imported backup copy and a
// migration binary in a private journal directory, with the migration command
// replaced by one that writes the copy and SQLite sidecars as a real one does.
const SIDECARS=["","-wal","-shm","-journal"];
function rehearsalFixture(id="rel_rehearse"){
 const home=mkdtempSync(join(tmpdir(),"release-rehearsal-")),backup=join(home,id+"-truenas-backup.sqlite"),binary=join(home,id+"-migration");
 writeFileSync(backup,"imported backup");writeFileSync(binary,"migration binary");
 const adapter=new HostAdapter({cwd:home,journalDirectory:home},{id});let clock=Date.parse("2026-10-01T09:00:00Z");adapter.now=()=>clock+=1000;
 const artifact={backupCopy:backup,backupSHA256:hash("imported backup"),migrationBinary:binary,migrationBinarySHA256:hash("migration binary")};
 const copy=backup+".rehearsal-"+id,seen=[];
 adapter.command=argv=>{seen.push({argv,copy:readFileSync(argv[2],"utf8"),wal:existsSync(argv[2]+"-wal")});for(const s of SIDECARS)writeFileSync(argv[2]+s,"migrated");return "";};
 return {home,backup,binary,adapter,artifact,copy,marker:copy+".json",seen,left:()=>SIDECARS.filter(s=>existsSync(copy+s))};
}
test("a1 a passing rehearsal removes its copy and sidecars, marks passed and leaves the imported backup unchanged",async()=>{
 const r=rehearsalFixture();assert.equal(await r.adapter.rehearse(r.artifact),true);
 assert.deepEqual(r.seen.map(c=>c.argv),[[r.binary,"--migrate-only",r.copy]]);assert.equal(r.seen[0].copy,"imported backup");
 assert.deepEqual(r.left(),[]);assert.equal(readFileSync(r.backup,"utf8"),"imported backup");
 assert.deepEqual(JSON.parse(readFileSync(r.marker,"utf8")),{version:1,jobId:"rel_rehearse",backupSHA256:hash("imported backup"),startedAt:"2026-10-01T09:00:01.000Z",outcome:"passed",endedAt:"2026-10-01T09:00:02.000Z",copyRemoved:true});
 assert.equal(statSync(r.marker).mode&0o777,0o600);
});
test("a2 a failing rehearsal rejects with the original error, removes its copy and marks failed",async()=>{
 const r=rehearsalFixture(),failure=new Error("synthetic migration failure"),migrate=r.adapter.command;
 r.adapter.command=argv=>{migrate(argv);throw failure;};
 await assert.rejects(r.adapter.rehearse(r.artifact),error=>error===failure);
 assert.deepEqual(r.left(),[]);assert.equal(readFileSync(r.backup,"utf8"),"imported backup");
 const marker=JSON.parse(readFileSync(r.marker,"utf8"));assert.equal(marker.outcome,"failed");assert.equal(marker.copyRemoved,true);assert.ok(!readFileSync(r.marker,"utf8").includes("synthetic"));
 // A migration binary changed after the build fails the same way, before any migration runs.
 const b=rehearsalFixture("rel_binary");writeFileSync(b.binary,"another binary");
 await assert.rejects(b.adapter.rehearse(b.artifact),/migration binary changed/);assert.equal(b.seen.length,0);assert.deepEqual(b.left(),[]);assert.equal(JSON.parse(readFileSync(b.marker,"utf8")).outcome,"failed");
 // The runner journals the failed step exactly as before.
 const f=fixture();mkdirSync(join(f.cwd,"hub/internal/api"),{recursive:true});const j=job(f,change(f,"hub/internal/api/fixture.go","fixture")),c=config(f,j),a=fake(),e=rehearsalFixture("rel_fixture");
 a.prepare=async()=>({release:"fixture",artifactSHA256:"b".repeat(64),backup:"copy",backupSHA256:"c".repeat(64),preflightReceiptSHA256:"d".repeat(64),schemaChanged:true});
 e.adapter.command=()=>{throw failure;};a.rehearse=artifact=>e.adapter.rehearse(e.artifact);
 await assert.rejects(runRelease(c,a));assert.equal(JSON.parse(readFileSync(c.journalPath,"utf8")).failure.step,"rehearse");assert.ok(!a.calls.some(x=>x.startsWith("deploy:")));assert.deepEqual(e.left(),[]);
});
test("a3 the marker, not the copy, refuses a second rehearsal",async()=>{
 for(const fail of [false,true]){
  const r=rehearsalFixture(),migrate=r.adapter.command;if(fail)r.adapter.command=argv=>{migrate(argv);throw new Error("synthetic migration failure");};
  await r.adapter.rehearse(r.artifact).catch(()=>{});assert.deepEqual(r.left(),[]);const before=readFileSync(r.marker,"utf8");
  await assert.rejects(r.adapter.rehearse(r.artifact),/Rehearsal already attempted; inspect prior attempt/);
  assert.equal(r.seen.length,1,"the migration is not run again");assert.deepEqual(r.left(),[]);assert.equal(readFileSync(r.marker,"utf8"),before);
 }
 // A run stopped mid-rehearsal leaves a started marker, which refuses too and keeps the copy for inspection.
 const stopped=rehearsalFixture("rel_stopped");writeFileSync(stopped.marker,JSON.stringify({version:1,jobId:"rel_stopped",outcome:"started"}));writeFileSync(stopped.copy,"half migrated");
 await assert.rejects(stopped.adapter.rehearse(stopped.artifact),/already attempted/);assert.equal(stopped.seen.length,0);assert.equal(readFileSync(stopped.copy,"utf8"),"half migrated");
 // A copy with no marker does not refuse: it and its sidecars are replaced by a fresh copy.
 const stale=rehearsalFixture("rel_stale");writeFileSync(stale.copy,"stale copy");writeFileSync(stale.copy+"-wal","stale wal");
 assert.equal(await stale.adapter.rehearse(stale.artifact),true);assert.deepEqual(stale.seen.map(c=>[c.copy,c.wal]),[["imported backup",false]]);
 assert.deepEqual(stale.left(),[]);assert.equal(JSON.parse(readFileSync(stale.marker,"utf8")).outcome,"passed");
});
// A fake journal: files maps a name to its size in bytes.
function journalFixture(files){
 const dir=mkdtempSync(join(tmpdir(),"release-retention-"));for(const [name,bytes] of Object.entries(files))writeFileSync(join(dir,name),"x".repeat(bytes));
 return {dir,names:()=>readdirSync(dir).filter(n=>n!=="retention.jsonl").sort(),lines:()=>existsSync(join(dir,"retention.jsonl"))?readFileSync(join(dir,"retention.jsonl"),"utf8").split("\n").filter(Boolean).map(l=>JSON.parse(l)):[]};
}
const copyOf=id=>id+"-truenas-backup.sqlite",sweep=(j,jobs,retention)=>pruneJournal({journalDirectory:j.dir,retention},jobs,{now:()=>Date.parse("2026-10-01T09:30:00Z")});
test("a4 retention by count keeps the newest released jobs by settled time and no copy of a rolled back job",()=>{
 // Newest by time is d then c. Text order would pick a and c; list order would pick b and c.
 const jobs=[{id:"rel_d",state:"released",settledAt:"2026-09-30T10:00:00.95Z"},{id:"rel_a",state:"released",settledAt:"2026-09-30T10:00:00Z"},{id:"rel_e",state:"rolled_back",settledAt:"2026-09-30T11:00:00Z"},{id:"rel_c",state:"released",settledAt:"2026-09-30T10:00:00.9Z"},{id:"rel_b",state:"released",settledAt:"2026-09-30T10:00:00.1Z"}];
 const j=journalFixture(Object.fromEntries(jobs.map(x=>[copyOf(x.id),10])));
 const removed=sweep(j,jobs,{releasedBackups:2,backupBudgetBytes:0});
 assert.deepEqual(j.names(),[copyOf("rel_c"),copyOf("rel_d")]);
 assert.deepEqual(removed.map(r=>[r.jobId,r.reason]).sort(),[["rel_a","count"],["rel_b","count"],["rel_e","not-released"]]);
});
test("a5 retention by budget removes the oldest released copies until the total fits",()=>{
 // rel_legacy has no settledAt, so it is the oldest; the claimed job's copy counts but is never removed.
 const jobs=[{id:"rel_new",state:"released",settledAt:"2026-09-30T12:00:00Z"},{id:"rel_legacy",state:"released"},{id:"rel_mid",state:"released",settledAt:"2026-09-30T11:00:00Z"},{id:"rel_live",state:"claimed"}];
 const j=journalFixture(Object.fromEntries(jobs.map(x=>[copyOf(x.id),100])));
 const removed=sweep(j,jobs,{releasedBackups:10,backupBudgetBytes:250});
 assert.deepEqual(removed.map(r=>[r.jobId,r.reason,r.bytes]),[["rel_legacy","budget",100],["rel_mid","budget",100]]);
 assert.deepEqual(j.names(),[copyOf("rel_live"),copyOf("rel_new")]);
 // Within budget nothing goes, and zero means no budget.
 for(const budget of [300,0]){const k=journalFixture({[copyOf("rel_new")]:100,[copyOf("rel_mid")]:100,[copyOf("rel_live")]:100});assert.deepEqual(sweep(k,jobs,{releasedBackups:10,backupBudgetBytes:budget}),[]);assert.equal(k.names().length,3);}
});
test("a6 a job that is not terminal keeps its backup and rehearsal copies under any policy",()=>{
 const live=["verified","claimed","merged","blocked","some_future_state"].map(state=>({id:"rel_"+state,state})),done={id:"rel_done",state:"released",settledAt:"2026-09-30T12:00:00Z"};
 const blocked=copyOf("rel_blocked")+".rehearsal-rel_blocked",leftover=copyOf("rel_done")+".rehearsal-rel_done";
 const j=journalFixture({...Object.fromEntries([...live,done].map(x=>[copyOf(x.id),50])),[blocked]:50,[blocked+"-wal"]:5,[blocked+".json"]:5,[leftover]:50,[leftover+"-shm"]:5,[leftover+".json"]:5});
 const removed=sweep(j,[...live,done],{releasedBackups:0,backupBudgetBytes:1});
 assert.deepEqual(removed.map(r=>[r.file,r.kind,r.reason]),[[leftover,"rehearsal","terminal"],[leftover+"-shm","rehearsal","terminal"],[copyOf("rel_done"),"backup","count"]]);
 assert.deepEqual(j.names(),[...live.map(x=>copyOf(x.id)),blocked,blocked+"-wal",blocked+".json",leftover+".json"].sort());
});
test("a7 every removal is recorded once and nothing but listed backup and rehearsal copies is touched",()=>{
 const jobs=[{id:"rel_old",state:"released",settledAt:"2026-09-29T12:00:00Z"},{id:"rel_new",state:"released",settledAt:"2026-09-30T12:00:00Z"},{id:"rel_refused",state:"refused"},{id:"../rel_escape",state:"refused"}];
 const others=["rel_old.json","rel_old-inputs.json","rel_old-receipt.json","rel_old-truenas-plan.json","rel_old-migration","rel_old-mini-before","rel_old-truenas-backup.sqlite.rehearsal-rel_old.json","rel_old-truenas-backup.sqlite.tmp","rel_unlisted-truenas-backup.sqlite","rel_unlisted-truenas-backup.sqlite.rehearsal-rel_unlisted","mini-relay-offset.json"];
 const j=journalFixture({[copyOf("rel_old")]:30,"rel_old-hub-backup.sqlite":20,[copyOf("rel_old")+".rehearsal-rel_old"]:7,[copyOf("rel_new")]:30,"rel_refused-bridge-backup.sqlite":11,...Object.fromEntries(others.map(n=>[n,3]))});
 // Only a regular file directly in the journal directory is a copy: a directory or link of that name stays.
 mkdirSync(join(j.dir,"tailos-dist-fixture"));mkdirSync(join(j.dir,"rel_refused-hub-backup.sqlite"));symlinkSync(join(j.dir,"rel_old.json"),join(j.dir,copyOf("rel_refused")));
 const escape=join(dirname(j.dir),"rel_escape-truenas-backup.sqlite");writeFileSync(escape,"outside");
 try{
  const removed=sweep(j,jobs,{releasedBackups:1,backupBudgetBytes:0}),expected=[
   {jobId:"rel_old",file:copyOf("rel_old")+".rehearsal-rel_old",kind:"rehearsal",bytes:7,reason:"terminal"},{jobId:"rel_refused",file:"rel_refused-bridge-backup.sqlite",kind:"backup",bytes:11,reason:"not-released"},
   {jobId:"rel_old",file:copyOf("rel_old"),kind:"backup",bytes:30,reason:"count"},{jobId:"rel_old",file:"rel_old-hub-backup.sqlite",kind:"backup",bytes:20,reason:"count"}].map(r=>({version:1,at:"2026-10-01T09:30:00.000Z",...r}));
  assert.deepEqual(removed,expected);assert.deepEqual(j.lines(),expected);assert.equal(statSync(join(j.dir,"retention.jsonl")).mode&0o777,0o600);
  assert.deepEqual(j.names(),[...others,copyOf("rel_new"),copyOf("rel_refused"),"rel_refused-hub-backup.sqlite","tailos-dist-fixture"].sort());
  assert.equal(readFileSync(escape,"utf8"),"outside");
  assert.deepEqual(sweep(j,jobs,{releasedBackups:1,backupBudgetBytes:0}),[],"a second sweep removes nothing");assert.deepEqual(j.lines(),expected);
 }finally{rmSync(escape,{force:true});}
});
test("a8 retention defaults apply, an invalid value stops the daemon before any command, and a failed sweep does not hold the poll",async()=>{
 assert.deepEqual(retentionPolicy({}),{releasedBackups:3,backupBudgetBytes:4294967296});assert.deepEqual(retentionPolicy({retention:{releasedBackups:0}}),{releasedBackups:0,backupBudgetBytes:4294967296});
 const cwd=mkdtempSync(join(tmpdir(),"release-retention-daemon-")),log=join(cwd,"calls"),fakeTT=join(cwd,"tt");
 const jobs=[...["rel_1","rel_2","rel_3","rel_4"].map((id,i)=>({id,state:"released",settledAt:`2026-09-30T1${i}:00:00Z`})),{id:"next",state:"verified",generation:1}];
 writeFileSync(fakeTT,"#!"+process.execPath+"\n"+`const fs=require('fs');const a=process.argv.slice(2);fs.appendFileSync(${JSON.stringify(log)},a.join(' ')+'\\n');if(a[1]==='list')console.log(${JSON.stringify(JSON.stringify(jobs))});else process.exit(2);`);chmodSync(fakeTT,0o755);
 const base={version:1,enabled:true,cwd,journalDirectory:cwd,tt:fakeTT};
 for(const retention of [{releasedBackups:-1},{releasedBackups:1.5},{releasedBackups:"3"},{backupBudgetBytes:-1},{backupBudgetBytes:0.5},{backupBudgetBytes:null},"3",[3]])await assert.rejects(serveDeployment({...base,retention},{once:true}),/Invalid journal retention/);
 assert.ok(!existsSync(log),"no command ran");
 // The default policy keeps three released copies: the poll removes the oldest and still tries the waiting job.
 for(const x of jobs.slice(0,4))writeFileSync(join(cwd,copyOf(x.id)),"copy");
 await serveDeployment(base,{once:true});assert.ok(!existsSync(join(cwd,copyOf("rel_1"))));assert.deepEqual(["rel_2","rel_3","rel_4"].filter(id=>existsSync(join(cwd,copyOf(id)))),["rel_2","rel_3","rel_4"]);
 assert.match(readFileSync(log,"utf8"),/^deployment list\ndeployment claim --job next /);
 // A removal record that cannot be written fails the sweep: the copy stays and the poll goes on to the claim.
 rmSync(log);rmSync(join(cwd,"retention.jsonl"));mkdirSync(join(cwd,"retention.jsonl"));
 await serveDeployment({...base,retention:{releasedBackups:0}},{once:true});assert.ok(existsSync(join(cwd,copyOf("rel_4"))));assert.match(readFileSync(log,"utf8"),/^deployment list\ndeployment claim --job next /);
});
test("b2 independently records restored Mini after failed TailOS rollback",async()=>{
 const f=fixture();mkdirSync(join(f.cwd,"hub/cmd/tt"),{recursive:true});change(f,"hub/cmd/tt/main.go","fixture");const j=job(f,change(f,"client/a.js","a")),a=fake();let receipt;
 a.check=async t=>t==="tailos"?"identity":true;a.rollback=async t=>{a.calls.push("rollback:"+t);return t==="mini";};a.finish=async r=>{receipt=r;};
 await assert.rejects(runRelease(config(f,j),a));assert.equal(receipt.outcome,"blocked");assert.deepEqual(receipt.targets.map(t=>[t.target,t.outcome,t.rollback]),[["mini","rolled_back","restored"],["tailos","failed","blocked"]]);
});
test("b3 finished releases omit cleanup and imported verification is not merged",()=>{
 const entry={itemId:"wi_fixture",state:"finished",acceptance:{branch:"feature/fixture",commit:"a".repeat(40)},release:{id:"rel_fixture",state:"released",integratedCommit:"b".repeat(40)}};
 const rendered=renderTeamDelivery({entries:[entry]});assert.ok(!rendered.includes("waiting for team cleanup"));assert.ok(rendered.includes("Accepted → verified → merged → released"));
 entry.release.state="claimed";entry.release.integratedVerification={commit:"b".repeat(40)};assert.ok(!renderTeamDelivery({entries:[entry]}).includes("→ merged"));
 entry.release=null;assert.ok(!renderTeamDelivery({entries:[entry]}).includes("waiting for team cleanup"));
});
test("b4 conflict refuses before merge and releases the project instead of blocking",async()=>{
 const f=fixture(),j=job(f,change(f,"client/base.js","candidate"));git(f.cwd,"checkout","tasks-hub");change(f,"client/base.js","release");const prior=git(f.cwd,"rev-parse","tasks-hub"),a=fake();
 await assert.rejects(runRelease(config(f,j),a),/refused/);assert.deepEqual(a.calls,["refuse","escalate"]);assert.equal(git(f.cwd,"rev-parse","tasks-hub"),prior);
});
test("b4 daemon skips an unclaimable first job without exiting",async()=>{
 const cwd=mkdtempSync(join(tmpdir(),"release-daemon-")),log=join(cwd,"calls"),fakeTT=join(cwd,"tt");
 writeFileSync(fakeTT,"#!"+process.execPath+"\n"+`const fs=require('fs');const a=process.argv.slice(2);fs.appendFileSync(${JSON.stringify(log)},a.join(' ')+'\\n');if(a[1]==='list'){console.log(JSON.stringify([{id:'stale',state:'verified',generation:1},{id:'later',state:'verified',generation:1}]));}else{process.exit(2);}`);chmodSync(fakeTT,0o755);
 await serveDeployment({version:1,enabled:true,cwd,journalDirectory:cwd,tt:fakeTT},{once:true});const calls=readFileSync(log,"utf8");assert.match(calls,/--job stale/);assert.match(calls,/--job later/);
});

test("b4 exact handler recovery archives a no-effect old journal and fresh run proceeds",async()=>{
 const f=fixture(),j={...job(f,change(f,"client/a.js","a")),agentId:"new-agent",runId:"new-run"},c=config(f,j);
 const prior={version:1,jobId:j.id,commit:j.commit,agentId:"old-agent",runId:"old-run",phase:"blocked",effects:[]};const raw=JSON.stringify(prior);writeFileSync(c.journalPath,raw);
 j.reconciliations=[{jobId:j.id,agentId:"old-agent",runId:"old-run",disposition:"requeue",noActiveExecution:true,noPublication:true,journalState:"no_effects",journalDigest:hash(raw)}];
 const a=fake();assert.equal((await runRelease(c,a)).outcome,"released");assert.deepEqual(JSON.parse(readFileSync(c.journalPath+".reconciled-"+hash(raw),"utf8")),prior);
 const other=config(f,j);writeFileSync(other.journalPath,JSON.stringify({...prior,effects:[{state:"unknown"}]}));await assert.rejects(runRelease(other,fake()),/Ambiguous/);
});
test("b1 handler input wait does not publish and resumes exact integration",async()=>{
 const f=fixture(),j=job(f,change(f,"client/a.js","a")),a=fake(),c=config(f,j);a.verifyInputs=async()=>false;
 assert.equal((await runRelease(c,a)).outcome,"waiting_inputs");assert.equal(git(f.cwd,"rev-parse","tasks-hub"),f.base);assert.ok(!a.calls.includes("merged"));
 a.verifyInputs=async()=>true;assert.equal((await runRelease(c,a)).outcome,"released");assert.equal(git(f.cwd,"rev-parse","tasks-hub"),j.commit);
});

test("b4 only the exact handler-inspected dead host lock is released",()=>{
 const cwd=mkdtempSync(join(tmpdir(),"release-lock-recovery-")),taskId="tsk_fixture",id="rel_fixture";
 const lock=join(tmpdir(),"tailterm-release-locks",hash(cwd+"\0"+taskId)+".lock");mkdirSync(join(tmpdir(),"tailterm-release-locks"),{recursive:true});const raw=JSON.stringify({jobId:id,agentId:"old-agent",runId:"old-run"});writeFileSync(lock,raw);
 const j={id,taskId,state:"verified",reconciliations:[{jobId:id,agentId:"old-agent",runId:"old-run",lockDigest:hash(raw),noActiveExecution:false,refResolved:true,journalState:"no_effects"}]};
 reconcileHostLocks({cwd},[j]);assert.equal(existsSync(lock),true);j.reconciliations[0].noActiveExecution=true;j.reconciliations[0].runId="wrong-run";reconcileHostLocks({cwd},[j]);assert.equal(existsSync(lock),true);
 j.reconciliations[0].runId="old-run";reconcileHostLocks({cwd},[j]);assert.equal(existsSync(lock),false);
});

const remoteHead=f=>git(f.origin,"rev-parse","refs/heads/tasks-hub");
test("a2 a failed rollback receipt write retries the same receipt and generation without a second rollback, escalation or bug request",async()=>{
 const f=fixture(),j={...job(f,change(f,"client/a.js","a")),generation:7},a=fake(),c=config(f,j);let finishes=[];a.check=async()=>"identity";
 a.finish=async(r,g)=>{finishes.push([JSON.stringify(r),g]);if(finishes.length===1)throw new Error("response lost");};
 await assert.rejects(runRelease(c,a),/pending retry/);assert.equal(JSON.parse(readFileSync(c.journalPath,"utf8")).phase,"receipt_pending");
 const receipt=await runRelease(c,a);assert.equal(receipt.outcome,"rolled_back");assert.equal(finishes.length,2);assert.deepEqual(finishes[1],finishes[0]);assert.equal(finishes[0][1],7);
 assert.deepEqual(a.calls,["merged","deploy:tailos","rollback:tailos","escalate","bug"]);assert.equal(JSON.parse(readFileSync(c.journalPath,"utf8")).phase,"blocked");
 await assert.rejects(runRelease(c,a),/Ambiguous/);
});
test("a3 a rollback commits a compare-and-swap revert of the release, files one bug request and the next job does not ship it again",async()=>{
 const f=fixture(),bad=change(f,"client/bad.js","bad"),j={...job(f,bad),itemId:"wi_fixture"},a=fake(),c=config(f,j);let receipt,bug;a.check=async()=>"identity";a.finish=async r=>{receipt=r;};a.requestBug=async b=>{bug=b;a.calls.push("bug");return "rel_fixture-rollback-bug";};
 await assert.rejects(runRelease(c,a),/failed/);
 const revert=git(f.cwd,"rev-parse","refs/heads/tasks-hub");assert.equal(git(f.cwd,"rev-parse",revert+"^"),bad);assert.equal(git(f.cwd,"rev-parse",revert+"^{tree}"),git(f.cwd,"rev-parse",f.base+"^{tree}"));
 assert.match(git(f.cwd,"log","-1","--format=%B",revert),/Release-Job: rel_fixture\nWork-Item: wi_fixture/);
 assert.deepEqual(receipt.revert,{commit:revert,outcome:"committed",bugRequestId:"rel_fixture-rollback-bug"});assert.deepEqual(bug,{jobId:"rel_fixture",commit:revert,outcome:"committed"});
 assert.equal(a.calls.filter(x=>x==="bug").length,1);assert.equal(remoteHead(f),f.base,"no push on the rollback path");
 git(f.cwd,"checkout","--detach",f.base);const next=change(f,"client/next.js","next");const j2={...job(f,next),id:"rel_next"},a2=fake();const r2=await runRelease(config(f,j2),a2);
 assert.equal(r2.outcome,"released");assert.equal(git(f.cwd,"rev-parse",r2.commit+"^"),revert);assert.ok(!existsSync(join(f.cwd,"client/bad.js")));assert.equal(git(f.cwd,"ls-tree","--name-only",r2.commit,"client/bad.js"),"");
});
test("a3 a concurrent ref move leaves the revert failed, escalated once and recorded, with no forced update",async()=>{
 const f=fixture(),j=job(f,change(f,"client/a.js","a")),a=fake(),c=config(f,j);let receipt,escalation;a.finish=async r=>{receipt=r;};a.escalate=async d=>{escalation=d;a.calls.push("escalate");};
 a.check=async()=>{git(f.cwd,"update-ref","refs/heads/tasks-hub",f.base);return "identity";};
 await assert.rejects(runRelease(c,a),/failed/);assert.equal(git(f.cwd,"rev-parse","refs/heads/tasks-hub"),f.base);
 assert.equal(receipt.revert.outcome,"failed");assert.match(receipt.revert.commit,/^[a-f0-9]{40}$/);assert.equal(escalation.revert,"failed");assert.equal(a.calls.filter(x=>x==="escalate").length,1);
 assert.ok(!readFileSync(new URL("../scripts/release-runner.mjs",import.meta.url),"utf8").match(/--force|\+refs|push -f|update-ref[^\n]*-f/));
});
test("a6 a live-verified release fast-forward pushes tasks-hub, including a zero-target release",async()=>{
 const f=fixture(),j=job(f,change(f,"client/a.js","a")),a=fake(),r=await runRelease(config(f,j),a);
 assert.deepEqual(r.push,{remote:"origin",commit:j.commit,outcome:"pushed"});assert.equal(remoteHead(f),j.commit);
 const g=fixture();mkdirSync(join(g.cwd,"docs"));const d=job(g,change(g,"docs/note.md","note")),b=fake(),z=await runRelease(config(g,d),b);
 assert.deepEqual(z.targets,[]);assert.equal(z.push.outcome,"pushed");assert.equal(remoteHead(g),d.commit);assert.deepEqual(b.calls,["merged","finish"]);
});
test("a6 a diverged remote fails the push with one escalation, keeps the release and never rolls back",async()=>{
 const f=fixture(),j=job(f,change(f,"client/a.js","a")),a=fake();let escalation;a.escalate=async x=>{escalation=x;a.calls.push("escalate");};
 const other=mkdtempSync(join(tmpdir(),"release-other-"));git(other,"clone","--quiet","-b","tasks-hub",f.origin,".");git(other,"config","user.email","o@example.invalid");git(other,"config","user.name","O");writeFileSync(join(other,"x"),"x");git(other,"add",".");git(other,"commit","-m","remote");git(other,"push","--quiet","origin","tasks-hub");const diverged=remoteHead(f);
 const r=await runRelease(config(f,j),a);assert.equal(r.outcome,"released");assert.deepEqual(r.push,{remote:"origin",commit:j.commit,outcome:"failed"});assert.equal(remoteHead(f),diverged);
 assert.deepEqual(escalation,{jobId:"rel_fixture",outcome:"released",push:"failed"});assert.deepEqual(a.calls,["merged","deploy:tailos","escalate","finish"]);
});
test("a6 a run stopped while pushing resumes the push and finishes without redeploying",async()=>{
 const f=fixture(),j=job(f,change(f,"client/a.js","a")),a=fake(),c=config(f,j);let finishes=0;a.finish=async()=>{if(++finishes===1)throw new Error("lost");a.calls.push("finish");};
 assert.equal((await runRelease(c,a)).outcome,"receipt_pending");const journal=JSON.parse(readFileSync(c.journalPath,"utf8"));
 journal.phase="pushing";delete journal.receipt.push;writeFileSync(c.journalPath,JSON.stringify(journal));git(f.origin,"update-ref","refs/heads/tasks-hub",f.base);
 const r=await runRelease(c,a);assert.equal(r.outcome,"released");assert.equal(r.push.outcome,"pushed");assert.equal(remoteHead(f),j.commit);assert.equal(a.calls.filter(x=>x==="deploy:tailos").length,1);
});
test("a7 publish and revert refuse while any worktree has tasks-hub checked out",async()=>{
 const f=fixture(),j=job(f,change(f,"client/a.js","a")),a=fake(),c=config(f,j),main=mkdtempSync(join(tmpdir(),"release-main-"));git(f.cwd,"worktree","add","--quiet",main,"tasks-hub");
 await assert.rejects(runRelease(c,a),/refused/);assert.deepEqual(a.calls,["refuse","escalate"]);assert.equal(git(f.cwd,"rev-parse","refs/heads/tasks-hub"),f.base);assert.deepEqual(JSON.parse(readFileSync(c.journalPath,"utf8")).effects,[]);
 git(f.cwd,"update-ref","refs/heads/tasks-hub",j.commit,f.base);assert.equal(moveReleaseRef(f.cwd,revertCommit(f.cwd,j,j.commit,f.base),j.commit),false);assert.equal(git(f.cwd,"rev-parse","refs/heads/tasks-hub"),j.commit);
 git(f.cwd,"worktree","remove","--force",main);assert.equal(tasksHubCheckedOut(f.cwd),false);
});
test("a4 host handler requests go to the project handler the hub resolves, never a fixed name",async()=>{
 const cwd=mkdtempSync(join(tmpdir(),"handler-adapter-")),calls=[];const adapter=new HostAdapter({cwd,journalDirectory:cwd,tt:"tt"},{id:"rel_fixture",commit:"a".repeat(40),itemId:"wi_fixture",itemRevision:2,orderMessageSeq:15262,generation:3});
 adapter.command=argv=>{calls.push(argv);if(argv[1]==="deployment"&&argv[2]==="handler")return JSON.stringify({id:"agt_0123456789abcdef",name:"db-handler-sol61"});if(argv[2]==="list")return "[]";return "";};
 assert.equal(await adapter.verifyInputs("b".repeat(40)),false);assert.equal(await adapter.requestBug({commit:"c".repeat(40),outcome:"committed"}),"rel_fixture-rollback-bug");
 const sends=calls.filter(a=>a[1]==="send");assert.equal(sends.length,2);for(const s of sends){assert.equal(s[s.indexOf("--to")+1],"agt_0123456789abcdef");assert.equal(s[s.indexOf("--work-item")+1],"wi_fixture");}
 assert.equal(sends[1][sends[1].indexOf("--request-id")+1],"rel_fixture-rollback-bug");assert.equal(sends[1][sends[1].indexOf("--work-order-message")+1],"15262");
 adapter.command=argv=>argv[2]==="handler"?JSON.stringify({id:""}):"";assert.throws(()=>adapter.handler(),/handler required/);
 assert.equal(readFileSync(new URL("../scripts/release-runner.mjs",import.meta.url),"utf8").includes('"db-handler"'),false);
});
test("host escalations: push failure has its own request and a failed revert is named",async()=>{
 const cwd=mkdtempSync(join(tmpdir(),"escalate-adapter-")),calls=[];const adapter=new HostAdapter({cwd,journalDirectory:cwd},{id:"rel_fixture"});adapter.command=argv=>{calls.push(argv);return "";};
 await adapter.escalate({push:"failed"});await adapter.escalate({revert:"failed"});
 assert.equal(calls[0][calls[0].indexOf("--request-id")+1],"rel_fixture-push-failure");assert.equal(calls[1][calls[1].indexOf("--request-id")+1],"rel_fixture-release-failure");assert.match(calls[1][calls[1].indexOf("--text")+1],/revert failed/);
});
test("b10 TailOS keeps the verified dist for the next rollback and reinstalls packages when the lockfile changed",async()=>{
 const f=fixture();mkdirSync(join(f.cwd,"dist-static"));writeFileSync(join(f.cwd,"dist-static/release.json"),"{}");const home=mkdtempSync(join(tmpdir(),"tailos-retain-")),commit="d".repeat(40);
 const adapter=new HostAdapter({cwd:f.cwd,journalDirectory:home},{id:"rel_fixture"});await adapter.retain("tailos",{commit});await adapter.retain("hub",{commit});
 const dir=join(home,"tailos-dist-"+commit);assert.equal(readFileSync(join(dir,"release.json"),"utf8"),"{}");assert.equal(statSync(dir).mode&0o777,0o700);assert.deepEqual(readdirSync(home),["tailos-dist-"+commit]);
 writeFileSync(join(f.cwd,"package-lock.json"),"{}");const next=change(f,"client/a.js","a");const cmds=[];const t=new HostAdapter({cwd:f.cwd,journalDirectory:home,baselines:Object.fromEntries(["hub","bridge","mini","tailos"].map(x=>[x,f.base])),targets:{tailos:{}}},{...job(f,next),id:"rel_fixture"});
 git(f.cwd,"checkout","--detach",next);importInputs(t,next,{tailos:{release:"rel_fixture-"+next.slice(0,12)+"-tailos"}});t.command=argv=>{cmds.push(argv.join(" "));return "";};await assert.rejects(t.prepare("tailos",next));assert.deepEqual(cmds.slice(0,2),["npm ci","npm run build:static"]);
});
test("a10 a superseded job is never selected, claimed or treated as a fence",async()=>{
 const superseded={id:"old",state:"superseded",settledAt:"2026-09-30T12:00:00Z",supersession:{releasedCommit:"c".repeat(40),release:"20260929-owner-helper-c6a8ec1",handReleaseId:"hrl_0123456789abcdef",targets:["hub"]}},next={id:"next",state:"verified"};
 assert.equal(runnableJob([superseded],"agent","run"),null);assert.equal(runnableJob([superseded,next],"agent","run"),next);
 const cwd=mkdtempSync(join(tmpdir(),"release-superseded-")),log=join(cwd,"calls"),fakeTT=join(cwd,"tt");
 writeFileSync(fakeTT,"#!"+process.execPath+"\n"+`const fs=require('fs');const a=process.argv.slice(2);fs.appendFileSync(${JSON.stringify(log)},a.join(' ')+'\\n');if(a[1]==='list')console.log(JSON.stringify([${JSON.stringify(superseded)}]));else process.exit(2);`);chmodSync(fakeTT,0o755);
 await serveDeployment({version:1,enabled:true,cwd,journalDirectory:cwd,tt:fakeTT},{once:true});assert.equal(readFileSync(log,"utf8"),"deployment list\n");
});
test("b1 a recorded hand release advances the next poll's baselines with no config edit",async()=>{
 const f=fixture();mkdirSync(join(f.cwd,"hub/cmd/tt"),{recursive:true});const tt=change(f,"hub/cmd/tt/main.go","cli"),commit=change(f,"client/a.js","a");
 const home=mkdtempSync(join(tmpdir(),"release-hand-baselines-")),configPath=join(home,"deploy.json"),fakeTT=join(home,"tt");
 const all=b=>Object.fromEntries(["hub","bridge","mini","tailos"].map(t=>[t,b]));
 writeFileSync(configPath,JSON.stringify({version:1,enabled:true,cwd:f.cwd,journalDirectory:home,tt:fakeTT,baselines:all(f.base)}));
 const superseded={id:"rel_hand",state:"superseded",settledAt:"2026-09-30T12:00:00.5Z",supersession:{releasedCommit:tt,release:"20260930-hand",handReleaseId:"hrl_0123456789abcdef",targets:["mini"]}};
 const verified={id:"rel_next",state:"verified",generation:1,commit};
 writeFileSync(fakeTT,"#!"+process.execPath+"\n"+`const a=process.argv.slice(2);if(a[1]==='list')console.log(JSON.stringify(${JSON.stringify([superseded,verified])}));else if(a[1]==='claim')console.log(JSON.stringify(${JSON.stringify({...verified,state:"claimed",generation:2})}));else process.exit(2);`);chmodSync(fakeTT,0o755);
 const seen=[];const release=async c=>{seen.push(c.baselines);};
 const config=JSON.parse(readFileSync(configPath,"utf8"));const before=readFileSync(configPath,"utf8");
 await serveDeployment(config,{once:true,configPath,release});
 assert.deepEqual(seen,[{...all(f.base),mini:tt}]);assert.equal(readFileSync(configPath,"utf8"),before);
 assert.deepEqual(selectReleaseTargets(f.cwd,seen[0],commit),["tailos"]);
});
test("D1 a superseded formerly refused job releases its handler-inspected host lock",()=>{
 const cwd=mkdtempSync(join(tmpdir(),"release-lock-superseded-")),taskId="tsk_fixture",id="rel_fixture";
 const lock=join(tmpdir(),"tailterm-release-locks",hash(cwd+"\0"+taskId)+".lock");mkdirSync(join(tmpdir(),"tailterm-release-locks"),{recursive:true});const raw=JSON.stringify({jobId:id,agentId:"old-agent",runId:"old-run"});writeFileSync(lock,raw);
 const j={id,taskId,state:"blocked",reconciliations:[{jobId:id,agentId:"old-agent",runId:"old-run",lockDigest:hash(raw),noActiveExecution:true,refResolved:true,journalState:"no_effects"}]};
 reconcileHostLocks({cwd},[j]);assert.equal(existsSync(lock),true,"a held job keeps its lock");
 j.state="superseded";reconcileHostLocks({cwd},[j]);assert.equal(existsSync(lock),false);
});
test("f3 a failed escalation post still sends the bug request and the final receipt",async()=>{
 const f=fixture(),j=job(f,change(f,"client/a.js","a")),a=fake(),c=config(f,j);let receipt;a.check=async()=>"identity";a.escalate=async()=>{a.calls.push("escalate");throw new Error("post refused");};a.finish=async r=>{receipt=r;a.calls.push("finish");};
 await assert.rejects(runRelease(c,a),/inspect saved journal/);assert.deepEqual(a.calls,["merged","deploy:tailos","rollback:tailos","escalate","bug","finish"]);
 assert.equal(receipt.outcome,"rolled_back");assert.equal(receipt.revert.bugRequestId,"rel_fixture-rollback-bug");const journal=JSON.parse(readFileSync(c.journalPath,"utf8"));assert.equal(journal.phase,"blocked");assert.equal(journal.escalationAttempted,true);
});
test("f4 a blocked rollback after the revert says live still runs the released code",async()=>{
 const f=fixture(),j=job(f,change(f,"client/a.js","a")),a=fake(),c=config(f,j);let receipt,escalation,deployed=false;
 a.deploy=async t=>{deployed=true;a.calls.push("deploy:"+t);};a.fence=async()=>!deployed;a.check=async()=>"identity";a.escalate=async d=>{escalation=d;a.calls.push("escalate");};a.finish=async r=>{receipt=r;};
 await assert.rejects(runRelease(c,a));assert.ok(!a.calls.includes("rollback:tailos"),"fence lost, so no rollback ran");
 assert.equal(receipt.outcome,"blocked");assert.deepEqual(receipt.targets.map(t=>[t.target,t.outcome,t.rollback]),[["tailos","failed","blocked"]]);assert.equal(receipt.revert.outcome,"committed");
 assert.deepEqual(escalation,{jobId:"rel_fixture",outcome:"blocked",revert:"committed",rollbackBlocked:true});
 const cwd=mkdtempSync(join(tmpdir(),"escalate-blocked-")),calls=[],adapter=new HostAdapter({cwd,journalDirectory:cwd},{id:"rel_fixture"});adapter.command=argv=>{calls.push(argv);return "";};
 await adapter.escalate(escalation);assert.match(calls[0][calls[0].indexOf("--text")+1],/tasks-hub was reverted, but at least one target could not be rolled back and still runs the released code/);
 await adapter.escalate({outcome:"rolled_back",revert:"committed"});assert.doesNotMatch(calls[1][calls[1].indexOf("--text")+1],/still runs the released code/);
});
test("f6 a blocked outcome before any deploy does not claim a target still runs the released code",async()=>{
 const f=fixture(),j=job(f,change(f,"client/a.js","a")),a=fake(),c=config(f,j);let escalation;a.merged=async()=>{throw new Error("merged response lost");};a.escalate=async d=>{escalation=d;a.calls.push("escalate");};
 await assert.rejects(runRelease(c,a));assert.deepEqual(a.calls,["escalate","bug","block"]);assert.deepEqual(escalation,{jobId:"rel_fixture",outcome:"blocked",revert:"committed"});
 const cwd=mkdtempSync(join(tmpdir(),"escalate-predeploy-")),calls=[],adapter=new HostAdapter({cwd,journalDirectory:cwd},{id:"rel_fixture"});adapter.command=argv=>{calls.push(argv);return "";};
 await adapter.escalate(escalation);assert.doesNotMatch(calls[0][calls[0].indexOf("--text")+1],/still runs the released code/);
});
test("b1 an edited config baseline changes the next poll's target selection without a restart",async()=>{
 const f=fixture();mkdirSync(join(f.cwd,"hub/cmd/tt"),{recursive:true});const tt=change(f,"hub/cmd/tt/main.go","cli"),commit=change(f,"client/a.js","a");
 const home=mkdtempSync(join(tmpdir(),"release-baselines-")),configPath=join(home,"deploy.json"),fakeTT=join(home,"tt");
 const write=b=>writeFileSync(configPath,JSON.stringify({version:1,enabled:true,cwd:f.cwd,journalDirectory:home,tt:fakeTT,baselines:b}));
 const verified={id:"rel_next",state:"verified",generation:1,commit};
 writeFileSync(fakeTT,"#!"+process.execPath+"\n"+`const a=process.argv.slice(2);if(a[1]==='list')console.log(JSON.stringify([${JSON.stringify(verified)}]));else if(a[1]==='claim')console.log(JSON.stringify(${JSON.stringify({...verified,state:"claimed",generation:2})}));else process.exit(2);`);chmodSync(fakeTT,0o755);
 const all=b=>Object.fromEntries(["hub","bridge","mini","tailos"].map(t=>[t,b]));const selections=[];
 const release=async c=>{selections.push(selectReleaseTargets(f.cwd,c.baselines,commit));};
 write(all(f.base));const config=JSON.parse(readFileSync(configPath,"utf8"));
 await serveDeployment(config,{once:true,configPath,release});
 write({...all(f.base),mini:tt,tailos:commit});await serveDeployment(config,{once:true,configPath,release});
 assert.deepEqual(selections,[["mini","tailos"],[]]);
 write({...all(f.base),mini:"bad"});await serveDeployment(config,{once:true,configPath,release});assert.equal(selections.length,2,"an invalid edit holds the poll before any claim");
});
function goWorktreeFixture(){
 const f=fixture();mkdirSync(join(f.cwd,"hub/cmd/tt"),{recursive:true});writeFileSync(join(f.cwd,"hub/go.mod"),"module example.com/hub\n\ngo 1.22\n");writeFileSync(join(f.cwd,"hub/cmd/tt/main.go"),"package main\n\nfunc main() {}\n");
 git(f.cwd,"add",".");git(f.cwd,"commit","-m","go candidate");const commit=git(f.cwd,"rev-parse","HEAD");
 const deployer=join(mkdtempSync(join(tmpdir(),"release-deployer-")),"checkout");git(f.cwd,"worktree","add","--quiet","--detach",deployer,commit);
 const home=mkdtempSync(join(tmpdir(),"release-deployer-home-")),install=join(home,"tt");writeFileSync(install,"v1");
 const config={cwd:deployer,journalDirectory:home,baselines:Object.fromEntries(["hub","bridge","mini","tailos"].map(t=>[t,f.base])),targets:{mini:{installPath:install,relayRestart:[process.execPath,"-e","0"]}}};
 const adapter=new HostAdapter(config,{...job(f,commit),id:"rel_stamp"});importInputs(adapter,commit,{mini:{release:"rel_stamp-"+commit.slice(0,12)+"-mini",rollbackSafe:true}});
 return {f,commit,deployer,adapter};
}
test("a Go artifact built for a worktree deployer checkout carries the integrated revision",async()=>{
 const {commit,deployer,adapter}=goWorktreeFixture();assert.ok(statSync(join(deployer,".git")).isFile(),"the deployer checkout is a worktree");
 const artifact=await adapter.prepare("mini",commit);const info=execFileSync("go",["version","-m",artifact.artifactPath],{encoding:"utf8"});
 assert.match(info,new RegExp(`\\tbuild\\tvcs\\.revision=${commit}\\n`));assert.match(info,/\tbuild\tvcs\.modified=false\n/);
 assert.equal(readdirSync(tmpdir()).filter(d=>d.startsWith("tailterm-release-build-")).filter(d=>existsSync(join(tmpdir(),d,"src"))).length,0,"the build clone is removed");
});
test("an unstamped or mismatched Go artifact is refused before any deploy",async()=>{
 for(const reply of [c=>"x: go1.26\n",c=>stamped("f".repeat(40)),c=>stamped(c,"true")]){
  const {commit,adapter}=goWorktreeFixture();const real=adapter.command.bind(adapter);adapter.command=(argv,cwd)=>argv[1]==="version"?reply(commit):real(argv,cwd);
  await assert.rejects(adapter.prepare("mini",commit),/build revision|does not match/);assert.equal(adapter.artifacts.size,0);
 }
});

// Hub and bridge released together share one TrueNAS plan (wi_bf16d39731a39104).
const PAIRED={release:"rel_fixture-000000000000-truenas",artifactSHA256:"b".repeat(64),planPath:"/private/rel_fixture-truenas-plan.json",planTargets:["hub","bridge"],backup:"/b/before-rel_fixture-truenas.sqlite",backupSHA256:"c".repeat(64),preflightReceiptSHA256:"d".repeat(64)};
function pairedRelease(opts={}){
 const f=fixture();mkdirSync(join(f.cwd,"hub/internal/api"),{recursive:true});const j=job(f,change(f,"hub/internal/api/x.go","x")),a=fake(),c=config(f,j);let receipt;
 a.prepare=async t=>["hub","bridge"].includes(t)?{...PAIRED,schemaChanged:opts.schema===true}:{release:"rel_fixture-mini",artifactSHA256:"b".repeat(64)};
 a.rehearse=async()=>{a.calls.push("rehearse");return true;};a.finish=async r=>{receipt=r;a.calls.push("finish");};
 return {f,j,a,c,receipt:()=>receipt,journal:()=>JSON.parse(readFileSync(c.journalPath,"utf8"))};
}
test("a2 a paired plan deploys hub and bridge in one call and releases both",async()=>{
 const p=pairedRelease(),r=await runRelease(p.c,p.a);
 assert.deepEqual(p.a.calls,["merged","deploy:hub","deploy:mini","finish"]);
 assert.deepEqual(r.targets.map(t=>[t.target,t.outcome,t.release]),[["hub","released",PAIRED.release],["bridge","released",PAIRED.release],["mini","released","rel_fixture-mini"]]);
 assert.deepEqual(p.journal().effects.map(e=>[e.target,e.state]),[["hub","verified"],["bridge","verified"],["mini","verified"]]);assert.equal(p.journal().failure,undefined);
 const s=pairedRelease({schema:true});await runRelease(s.c,s.a);assert.deepEqual(s.a.calls,["merged","rehearse","deploy:hub","deploy:mini","finish"],"the shared backup copy is rehearsed once");
});
test("a2 a paired plan whose members disagree or whose partner is not selected is refused before any deploy",async()=>{
 for(const other of [{...PAIRED,planPath:"/private/other-plan.json"},{...PAIRED,release:"rel_other"},{...PAIRED,planTargets:["bridge"]}]){
  const p=pairedRelease();p.a.prepare=async t=>t==="hub"?PAIRED:t==="bridge"?other:{release:"m",artifactSHA256:"b".repeat(64)};
  await assert.rejects(runRelease(p.c,p.a));assert.ok(!p.a.calls.some(x=>x.startsWith("deploy:")));assert.deepEqual(p.journal().effects,[]);
  assert.deepEqual(p.journal().failure,{step:"prepare",target:"bridge",reason:"Paired plan members disagree"});
 }
 const f=fixture();mkdirSync(join(f.cwd,"hub/cmd/tailterm-hub"),{recursive:true});const j=job(f,change(f,"hub/cmd/tailterm-hub/x.go","x")),a=fake(),c=config(f,j);a.prepare=async()=>PAIRED;
 await assert.rejects(runRelease(c,a));assert.ok(!a.calls.some(x=>x.startsWith("deploy:")));assert.equal(JSON.parse(readFileSync(c.journalPath,"utf8")).failure.reason,"Paired plan partner is not selected");
});
test("a4 a paired release rolls back hub then bridge to the prior pair whichever step fails",async()=>{
 const cases=[
  ["bridge live check fails",a=>{a.check=async t=>t!=="bridge";},{step:"live-check",target:"bridge",reason:"live verification failed"}],
  ["combined deploy throws",a=>{a.deploy=async t=>{a.calls.push("deploy:"+t);throw new Error("SYNTHETIC_PRIVATE_TOKEN");};},{step:"deploy",target:"hub",reason:"unclassified"}],
  ["hub live check fails",a=>{a.check=async t=>t!=="hub";},{step:"live-check",target:"hub",reason:"live verification failed"}],
 ];
 for(const [name,arrange,failure] of cases){
  const p=pairedRelease();arrange(p.a);await assert.rejects(runRelease(p.c,p.a),/failed/,name);
  assert.deepEqual(p.a.calls,["merged","deploy:hub","rollback:hub","rollback:bridge","escalate","bug","finish"],name);
  assert.equal(p.receipt().outcome,"rolled_back",name);
  assert.deepEqual(p.receipt().targets.map(t=>[t.target,t.outcome,t.rollback]),[["hub","rolled_back","restored"],["bridge","rolled_back","restored"]],name);
  const raw=readFileSync(p.c.journalPath,"utf8");assert.deepEqual(JSON.parse(raw).failure,failure,name);assert.ok(!raw.includes("SYNTHETIC_PRIVATE_TOKEN"),name);
 }
});
test("a4 a new hub that never responds still ends rolled back, because the bridge is restored after the hub",async()=>{
 // Like the real bridge rollback probe, the fake bridge restore succeeds only while the hub responds.
 for(const [name,deployThrows] of [["hub live check fails",false],["combined deploy throws",true]]){
  const p=pairedRelease(),live={hub:true};
  p.a.deploy=async t=>{p.a.calls.push("deploy:"+t);live.hub=false;if(deployThrows)throw new Error("deploy failed");};
  p.a.check=async t=>t==="hub"?live.hub:true;
  p.a.rollback=async t=>{p.a.calls.push("rollback:"+t);if(t==="hub")live.hub=true;return live.hub;};
  await assert.rejects(runRelease(p.c,p.a),/failed/,name);
  assert.deepEqual(p.a.calls.filter(x=>x.startsWith("rollback:")),["rollback:hub","rollback:bridge"],name);
  assert.equal(p.receipt().outcome,"rolled_back",name);
  assert.deepEqual(p.receipt().targets.map(t=>[t.target,t.outcome,t.rollback]),[["hub","rolled_back","restored"],["bridge","rolled_back","restored"]],name);
 }
});
test("a7 the journal names the failed step, target and a bounded non-secret reason",async()=>{
 const f=fixture(),j=job(f,change(f,"client/a.js","a")),a=fake(),c=config(f,j);a.deploy=async()=>{throw new Error("SYNTHETIC_PRIVATE_TOKEN");};a.rollback=async()=>false;
 await assert.rejects(runRelease(c,a));const raw=readFileSync(c.journalPath,"utf8");assert.deepEqual(JSON.parse(raw).failure,{step:"deploy",target:"tailos",reason:"unclassified"});assert.ok(!raw.includes("SYNTHETIC_PRIVATE_TOKEN"));
 const g=fixture(),k=job(g,change(g,"client/a.js","a")),b=fake(),d=config(g,k);b.check=async()=>"identity";
 await assert.rejects(runRelease(d,b));assert.deepEqual(JSON.parse(readFileSync(d.journalPath,"utf8")).failure,{step:"live-check",target:"tailos",reason:"live verification failed"});
 const h=fixture(),m=job(h,change(h,"client/a.js","a")),e=fake(),n=config(h,m);let fences=0;e.fence=async()=>++fences<3;
 await assert.rejects(runRelease(n,e));assert.deepEqual(JSON.parse(readFileSync(n.journalPath,"utf8")).failure,{step:"prepare",target:"tailos",reason:"release fence lost"});
 const dir=mkdtempSync(join(tmpdir(),"release-reason-")),script=join(dir,"fake-deploy.mjs");
 writeFileSync(script,`process.stderr.write("SYNTHETIC_PRIVATE_TOKEN");console.log("progress");console.log(JSON.stringify({status:"failed",classification:"remote-operation-failed",stage:"bridge-binary-upload",message:"SYNTHETIC_PRIVATE_TOKEN",remoteDetail:"SYNTHETIC_PRIVATE_TOKEN /mnt/x"}));process.exit(2);`);
 const adapter=new HostAdapter({cwd:dir,journalDirectory:dir},{id:"rel_fixture"});let thrown;try{adapter.command([process.execPath,script]);}catch(error){thrown=error;}
 assert.equal(thrown.message,"Host operation failed");assert.equal(failureReason(thrown),"fake-deploy.mjs exit 2: remote-operation-failed at bridge-binary-upload");
 writeFileSync(script,`console.log(JSON.stringify({classification:"Bad Value; rm -rf",stage:"x"}));process.exit(3);`);try{adapter.command([process.execPath,script]);}catch(error){thrown=error;}
 assert.equal(failureReason(thrown),"fake-deploy.mjs exit 3");
 try{adapter.command([join(dir,"missing-program")]);}catch(error){thrown=error;}assert.equal(failureReason(thrown),"missing-program not started");
 assert.equal(failureReason(new Error("SYNTHETIC_PRIVATE_TOKEN")),"unclassified");assert.equal(failureReason(releaseError("x".repeat(161))),"unclassified");assert.equal(failureReason(releaseError("token=\"s\"")),"unclassified");
});

function pairedHost({bridgeRelease}={}){
 const f=fixture();mkdirSync(join(f.cwd,"hub/internal/api"),{recursive:true});const commit=change(f,"hub/internal/api/x.go","x"),home=mkdtempSync(join(tmpdir(),"release-paired-"));
 const config={cwd:f.cwd,journalDirectory:home,baselines:Object.fromEntries(["hub","bridge","mini","tailos"].map(t=>[t,f.base])),targets:{hub:{},bridge:{}}};
 const adapter=new HostAdapter(config,{...job(f,commit),id:"rel_pair"}),release="rel_pair-"+commit.slice(0,12)+"-truenas",BASE="/mnt/deepfreeze/tailterm-hub";
 const planPath=join(home,"rel_pair-truenas-plan.json"),receipt=join(home,"rel_pair-truenas-preflight.json"),backup=`${BASE}/backups/before-rel_pair-truenas.sqlite`;writeFileSync(receipt,"{}");
 writeFileSync(planPath,JSON.stringify({backupDestination:backup,deployment:{releaseName:release,targets:["hub","bridge"],binaryDestination:`${BASE}/releases/${release}/tailterm-hub`,bridgeBinaryDestination:`${BASE}/releases/${bridgeRelease||release}/tailterm-discord`}}));
 const shared={release,backupJobId:"rel_pair",backup,backupSHA256:"c".repeat(64),planPath,preflightReceipt:receipt,preflightReceiptSHA256:hash("{}"),planTargets:["hub","bridge"],rollbackSafe:true};
 const calls=[];adapter.command=argv=>{calls.push(argv);if(argv[1]==="version")return stamped(commit);if(argv.includes("build")){const out=argv[argv.indexOf("-o")+1];writeFileSync(out,"binary "+out);return "";}return "";};
 return {f,commit,adapter,calls,shared,planPath,importTargets:targets=>importInputs(adapter,commit,targets)};
}
test("a3 the host adapter refuses unpaired hub and bridge inputs before publication",async()=>{
 const h=pairedHost();h.importTargets({hub:{...h.shared,planTargets:["hub"],planPath:"/p/hub"},bridge:{...h.shared,planTargets:["bridge"],planPath:"/p/bridge"}});
 assert.throws(()=>h.adapter.jobInputs(h.commit),/one paired plan/);
 h.importTargets({hub:h.shared,bridge:{...h.shared,backupSHA256:"e".repeat(64)}});assert.throws(()=>h.adapter.jobInputs(h.commit),/one paired plan/);
 h.importTargets({hub:h.shared,bridge:h.shared});assert.equal(h.adapter.jobInputs(h.commit).targets.bridge.release,h.shared.release);
 const f=fixture(),j=job(f,change(f,"client/a.js","a")),a=fake();a.verifyInputs=async()=>{throw releaseError("Hub and bridge need one paired plan");};
 await assert.rejects(runRelease(config(f,j),a),/refused/);assert.deepEqual(a.calls,["refuse","escalate"]);assert.equal(git(f.cwd,"rev-parse","tasks-hub"),f.base);
});
test("a3 a paired plan deploys once with the shared plan and refuses a stale or changed partner",async()=>{
 const stale=pairedHost({bridgeRelease:"20260929-live"});stale.importTargets({hub:stale.shared,bridge:stale.shared});
 await assert.rejects(stale.adapter.prepare("hub",stale.commit),/mount this release/);await assert.rejects(stale.adapter.prepare("bridge",stale.commit),/mount this release/);
 const h=pairedHost();h.importTargets({hub:h.shared,bridge:h.shared});
 const hub=await h.adapter.prepare("hub",h.commit),bridge=await h.adapter.prepare("bridge",h.commit);
 assert.equal(hub.release,h.shared.release);assert.deepEqual(bridge.planTargets,["hub","bridge"]);assert.notEqual(hub.artifactPath,bridge.artifactPath);
 h.calls.length=0;await h.adapter.deploy("hub",hub);
 assert.deepEqual(h.calls,[["python3","scripts/deploy-truenas-hub.py",h.shared.release,"--plan",h.planPath,"--preflight-receipt",h.shared.preflightReceipt,"--preflight-receipt-sha256",h.shared.preflightReceiptSHA256,"--update"]]);
 writeFileSync(bridge.artifactPath,"changed");await assert.rejects(h.adapter.deploy("hub",hub),/Paired artifact changed/);
 const one=pairedHost();one.importTargets({hub:{...one.shared,planTargets:["hub"]}});await assert.rejects(one.adapter.prepare("hub",one.commit),/release identity/,"a single-target plan keeps its per-target release name");
});

// wi_5b03fe47520b7c4f: the deployer verifies an integrated commit in its own
// checkout, so the matrix prerequisites, the run's timeout and other matrix
// runs on the host are the runner's concern.
const RUNNER=new URL("../scripts/release-runner.mjs",import.meta.url).pathname;
const ignorePrerequisites=cwd=>writeFileSync(join(cwd,".git/info/exclude"),"node_modules/\n.build/\nwasm/*.wasm\n");
function placePrerequisites(dir,skip=[]){for(const p of MATRIX_PREREQUISITES.filter(p=>!skip.includes(p))){mkdirSync(join(dir,dirname(p)),{recursive:true});writeFileSync(join(dir,p),"fixture "+p);}}
const workedPlan={maxAttempts:3,checks:[{id:"go-race",environment:{VERIFICATION_TIMEOUT_MS:"1800000"}},{id:"npm-unit",environment:{VERIFICATION_TIMEOUT_MS:"120000"}}]};
const exitedPid=()=>spawnSync(process.execPath,["-e",""]).pid;
const attemptDir=(home,commit,n)=>join(home,"rel_fixture-integrated-verification",`${commit}-r${n}`);
function matrixHost({plan=workedPlan,receipt={environment:{},checks:[{exitCode:0}]},jobs=[]}={}){
 const f=fixture(),home=mkdtempSync(join(tmpdir(),"matrix-host-"));ignorePrerequisites(f.cwd);placePrerequisites(f.cwd);
 const adapter=new HostAdapter({cwd:f.cwd,journalDirectory:home},{id:"rel_fixture",agentId:"agt_fixture",runId:"run_fixture",generation:1});
 // The stub run has already ended with this receipt when the next poll looks.
 const calls=[];adapter.processStartTime=()=>null;
 adapter.startMatrixRun=(argv,dir)=>{calls.push({argv,dir});writeFileSync(join(dir,"receipt.json"),JSON.stringify(receipt));return exitedPid();};
 adapter.command=(argv,cwd,options)=>{calls.push({argv,timeout:options?.timeout??600000});
  if(argv[1]==="deployment"&&argv[2]==="list")return JSON.stringify(jobs);
  if(argv[1]==="deployment"&&argv[2]==="handler")return JSON.stringify({id:"agt_0123abcd"});
  if(argv[1]==="scripts/verify-matrix.mjs"&&argv[2]==="plan"){writeFileSync(argv[4],JSON.stringify(plan));return "";}
  return "";};
 const integrated={id:"rel_fixture",integratedCommit:"c".repeat(40),plan:{commit:"a".repeat(40)}};
 // One poll starts the run, the next finds it ended.
 const verify=async j=>{await adapter.verifyIntegrated(j);return adapter.verifyIntegrated(j);};
 return {f,home,adapter,calls,integrated,verify,run:(n=0,commit=integrated.integratedCommit)=>JSON.parse(readFileSync(join(attemptDir(home,commit,n),"run.json"),"utf8")),matrix:()=>calls.filter(c=>c.argv[1]==="scripts/verify-matrix.mjs"),sends:()=>calls.filter(c=>c.argv[1]==="send")};
}
test("p1 a missing matrix prerequisite refuses the release by name before any matrix run",async()=>{
 const f=fixture(),j=job(f,change(f,"client/a.js","a"));git(f.cwd,"checkout","tasks-hub");change(f,"client/c.js","c");
 ignorePrerequisites(f.cwd);placePrerequisites(f.cwd,[".build/test.wasm"]);
 const c=config(f,j),a=fake(),argvs=[];let escalation;
 const host=new HostAdapter({cwd:f.cwd,journalDirectory:dirname(c.journalPath)},{id:"rel_fixture"});host.command=argv=>{argvs.push(argv);return argv[2]==="list"?"[]":"";};
 a.verifyIntegrated=x=>host.verifyIntegrated(x);a.escalate=async d=>{escalation=d;a.calls.push("escalate");};
 await assert.rejects(runRelease(c,a),/refused/);
 assert.deepEqual(a.calls,["refuse","escalate"]);assert.ok(!argvs.some(x=>x.includes("scripts/verify-matrix.mjs")),"no matrix argv");
 assert.deepEqual(escalation,{jobId:"rel_fixture",outcome:"refused",reason:"Missing matrix prerequisites: .build/test.wasm"});
 const journal=JSON.parse(readFileSync(c.journalPath,"utf8"));assert.equal(journal.refusalReason,"Missing matrix prerequisites: .build/test.wasm");assert.equal(journal.phase,"refused");
 const calls=[];const notice=new HostAdapter({cwd:f.cwd,journalDirectory:f.cwd},{id:"rel_fixture"});notice.command=argv=>{calls.push(argv);return "";};
 await notice.escalate(escalation);const text=calls[0][calls[0].indexOf("--text")+1];
 assert.match(text,/Reason: Missing matrix prerequisites: \.build\/test\.wasm\./);assert.doesNotMatch(text,/Automatic rollback attempted once/);
 assert.equal(calls[0][calls[0].indexOf("--subject")+1],"Release refused before publication");
 await notice.escalate({jobId:"rel_fixture",outcome:"refused",reason:"token=secret\nleak"});assert.match(calls[1][calls[1].indexOf("--text")+1],/Reason: unclassified\./);
 assert.deepEqual(missingPrerequisites(f.cwd),[".build/test.wasm"]);
});
test("p1 the prerequisite list matches the one verify-matrix.mjs checks",()=>{
 const cwd=join(tmpdir(),"prerequisite-list-fixture"),reads=[];
 const checks=[{id:"npm-unit",argv:["npm","test"]},{id:"fixture-browser",argv:["node","fixture.mjs"],environment:{TEST_BROWSER:"both"}}];
 const prerequisites=readPrerequisites(checks,cwd,file=>{reads.push(file);return Buffer.from("fixture");});
 assert.deepEqual(prerequisites.map(p=>p.path),MATRIX_PREREQUISITES);
 assert.deepEqual(reads,MATRIX_PREREQUISITES.map(p=>join(cwd,p)));
});
function provisionFixture(){
 const cwd=mkdtempSync(join(tmpdir(),"provision-checkout-")),from=mkdtempSync(join(tmpdir(),"provision-source-"));
 git(cwd,"init","-q","-b","tasks-hub");writeFileSync(join(cwd,".gitignore"),"node_modules/\n.build/\nwasm/*.wasm\n");git(cwd,"add",".");git(cwd,"-c","user.email=f@example.invalid","-c","user.name=F","commit","-q","-m","base");
 placePrerequisites(from);const installs=[];
 const run=(argv,dir)=>{installs.push([argv,dir]);mkdirSync(join(dir,"node_modules"),{recursive:true});writeFileSync(join(dir,"node_modules/.package-lock.json"),"installed");};
 return {cwd,from,installs,run};
}
test("p1 provisioning installs and copies each missing prerequisite, never overwrites and is idempotent",()=>{
 const p=provisionFixture();mkdirSync(join(p.cwd,".build"));writeFileSync(join(p.cwd,".build/test.wasm"),"local build");
 const first=provisionPrerequisites(p.cwd,{from:p.from,run:p.run});
 assert.deepEqual(p.installs,[[["npm","ci"],p.cwd]]);
 assert.deepEqual(first.prerequisites.map(x=>[x.path,x.action]),[["node_modules/.package-lock.json","installed"],["wasm/tailserve.wasm","copied"],[".build/test.wasm","present"],[".build/speech-fixture.wav","copied"],[".build/go-modules.txt","copied"]]);
 assert.equal(readFileSync(join(p.cwd,".build/test.wasm"),"utf8"),"local build");assert.equal(readFileSync(join(p.cwd,"wasm/tailserve.wasm"),"utf8"),"fixture wasm/tailserve.wasm");
 for(const x of first.prerequisites)assert.equal(x.sha256,hash(readFileSync(join(p.cwd,x.path))));
 assert.equal(git(p.cwd,"status","--porcelain"),"");
 const again=provisionPrerequisites(p.cwd,{from:p.from,run:p.run});assert.equal(p.installs.length,1);assert.ok(again.prerequisites.every(x=>x.action==="present"));
 assert.deepEqual(again.prerequisites.map(x=>x.sha256),first.prerequisites.map(x=>x.sha256));
});
test("p1 provisioning names every prerequisite missing from both checkouts and refuses a bad source or a dirtied checkout",()=>{
 const p=provisionFixture();for(const x of [".build/test.wasm",".build/speech-fixture.wav"])rmSync(join(p.from,x));
 assert.throws(()=>provisionPrerequisites(p.cwd,{from:p.from,run:p.run}),e=>failureReason(e)==="Missing matrix prerequisites: .build/test.wasm, .build/speech-fixture.wav");
 const q=provisionFixture();assert.throws(()=>provisionPrerequisites(q.cwd,{from:q.from,run:()=>{}}),e=>failureReason(e)==="Missing matrix prerequisites: node_modules/.package-lock.json");
 assert.throws(()=>provisionPrerequisites(q.cwd,{from:q.from,run:()=>{throw new Error("network down");}}),e=>failureReason(e)==="Prerequisite install failed: npm ci");
 for(const from of [undefined,"relative/path",q.cwd,join(q.from,"missing")])assert.throws(()=>provisionPrerequisites(q.cwd,{from,run:q.run}),e=>failureReason(e)==="Prerequisite source must be another absolute checkout");
 const d=provisionFixture();writeFileSync(join(d.cwd,".gitignore"),"node_modules/\n.build/\n");git(d.cwd,"-c","user.email=f@example.invalid","-c","user.name=F","commit","-q","-am","track wasm");
 assert.throws(()=>provisionPrerequisites(d.cwd,{from:d.from,run:d.run}),e=>failureReason(e)==="Prerequisite provisioning changed the checkout");
});
test("p1 the provisioning command prints the prerequisites, or only the named reason",()=>{
 const p=provisionFixture();mkdirSync(join(p.cwd,"node_modules"));writeFileSync(join(p.cwd,"node_modules/.package-lock.json"),"{}");
 const out=JSON.parse(execFileSync(process.execPath,[RUNNER,"--provision-prerequisites","--from",p.from],{cwd:p.cwd,encoding:"utf8"}));
 assert.equal(out.version,1);assert.deepEqual(out.prerequisites.map(x=>x.action),["present","copied","copied","copied","copied"]);assert.equal(git(p.cwd,"status","--porcelain"),"");
 const q=provisionFixture();mkdirSync(join(q.cwd,"node_modules"));writeFileSync(join(q.cwd,"node_modules/.package-lock.json"),"{}");rmSync(join(q.from,".build/go-modules.txt"));
 const r=execFileSync(process.execPath,["-e",`const r=require("child_process").spawnSync(process.execPath,${JSON.stringify([RUNNER,"--provision-prerequisites","--from",q.from])},{cwd:${JSON.stringify(q.cwd)},encoding:"utf8"});console.log(JSON.stringify({status:r.status,stdout:r.stdout,stderr:r.stderr}))`],{encoding:"utf8"});
 assert.deepEqual(JSON.parse(r),{status:1,stdout:"",stderr:"Missing matrix prerequisites: .build/go-modules.txt\n"});
});
// The integrated matrix run as a member of the verification host's ordered
// waitlist (wi_c5cb667695c3614c). Each test uses its own lock file; the child
// below is a real second process joining it the way a matrix run does.
const LOCK_MODULE=new URL("../scripts/verify-matrix-host-lock.mjs",import.meta.url).href;
const RUN_CHILD=`
import fs from 'node:fs';
import {spawn} from 'node:child_process';
import {acquireHostLock} from ${JSON.stringify(LOCK_MODULE)};
const o=JSON.parse(process.argv[2]);
if(o.resist)process.on('SIGTERM',()=>{});
const lease=await acquireHostLock({path:o.path,priority:o.priority,prioritySource:'flag',item:o.item,agent:'deployer',output:o.dir,recordDirectory:o.dir,runTimeoutMs:600000,pollMs:20,environment:{TAILTERM_MATRIX_MAX_HOLDERS:o.limit||'1'}});
let group=null;
if(o.group){const c=spawn(process.execPath,['-e','setInterval(()=>{},1000)'],{detached:true,stdio:'ignore'});await new Promise(r=>c.on('spawn',r));c.unref();group=c.pid;lease.addGroup(group);}
if(o.mode==='receipt'){fs.writeFileSync(o.dir+'/receipt.json',JSON.stringify({environment:{},checks:[{exitCode:0}]}));await lease.release();process.exit(0);}
if(!o.resist)process.on('SIGTERM',async()=>{await lease.release();process.exit(143);});
fs.writeFileSync(o.marker,JSON.stringify({pid:process.pid,group}));
setInterval(()=>{},1000);
`;
async function until(condition,what,ms=20000){const end=Date.now()+ms;for(;;){const v=condition();if(v)return v;if(Date.now()>end)assert.fail("timed out waiting for "+what);await new Promise(r=>setTimeout(r,10));}}
const ended=c=>c.exitCode!==null||c.signalCode!==null?Promise.resolve():new Promise(r=>c.once("exit",r));
function idle(t){const c=spawn(process.execPath,["-e","setInterval(()=>{},1000)"],{detached:true,stdio:"ignore"});t.after(()=>{try{process.kill(c.pid,"SIGKILL");}catch{}});return c;}
function lockHost(t,child={}){
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
const holderEntry=(pid,dir,extra={})=>({id:"00000000-0000-4000-8000-000000000001",seq:1,pid,kind:"run",item:"wi_fixture",agent:"deployer",priority:"high",prioritySource:"flag",requestedAt:new Date().toISOString(),runTimeoutMs:600000,output:dir,startedAt:new Date().toISOString(),groups:[],...extra});
const otherHolder=()=>holderEntry(process.pid,"/elsewhere",{id:"00000000-0000-4000-8000-000000000002",item:"wi_other",agent:"verifier"});
const reasonIs=text=>e=>failureReason(e)===text;
test("a8 a10 the deployer's run bound is the serial sum under the holder cap, the number the run itself records",async t=>{
 assert.equal(7560000>DEFAULT_HOLDER_CAP_MS,true);assert.equal(matrixRunTimeout(workedPlan),7200000,"the serial sum of 126 minutes is clamped to the cap");
 assert.equal(matrixRunTimeout({checks:[{environment:{VERIFICATION_TIMEOUT_MS:"600000"}}]}),2400000,"a plan under the cap keeps its serial sum; without maxAttempts each check runs once");
 process.env.TAILTERM_MATRIX_HOLDER_CAP_MINUTES="30";
 try{assert.equal(matrixRunTimeout(workedPlan),1800000,"the cap is configurable");}finally{delete process.env.TAILTERM_MATRIX_HOLDER_CAP_MINUTES;}
 for(const bad of [{...workedPlan,checks:[]},{checks:[{environment:{}}]},{checks:[{environment:{VERIFICATION_TIMEOUT_MS:"1800001"}}]},{checks:[{environment:{VERIFICATION_TIMEOUT_MS:"0"}}]},{checks:[{environment:{VERIFICATION_TIMEOUT_MS:1800000}}]},{maxAttempts:0,checks:workedPlan.checks}])
  assert.throws(()=>matrixRunTimeout(bad),e=>/^(Matrix plan has no checks|Invalid matrix check timeout|Invalid matrix attempt limit)$/.test(failureReason(e)));
 // The run passes planRunTimeout to the host lock, which records the clamped value.
 for(const plan of [workedPlan,{checks:[{environment:{VERIFICATION_TIMEOUT_MS:"600000"}}]}]){
  const lease=await acquireHostLock({path:join(mkdtempSync(join(tmpdir(),"runner-bound-")),"host.json"),runTimeoutMs:planRunTimeout(plan),item:"wi_fixture",agent:"deployer"});
  assert.equal(lease.record.runTimeoutMs,matrixRunTimeout(plan));await lease.release();
 }
 const h=matrixHost();assert.equal(await h.adapter.verifyIntegrated(h.integrated),false);
 assert.deepEqual(h.matrix().map(c=>[c.argv[2],c.timeout]),[["plan",600000],["run",undefined]],"the run is started, not waited on, so it has no command timeout");
 assert.equal(h.run().boundMs,7200000);assert.equal(h.sends().length,0);
 assert.equal(await h.adapter.verifyIntegrated(h.integrated),false);assert.equal(h.sends().length,1);assert.equal(h.run().reason,"receipt");
 const bad=matrixHost({plan:{maxAttempts:3,checks:[{environment:{VERIFICATION_TIMEOUT_MS:"3600000"}}]}});
 await assert.rejects(bad.adapter.verifyIntegrated(bad.integrated),reasonIs("Invalid matrix check timeout"));assert.deepEqual(bad.matrix().map(c=>c.argv[2]),["plan"]);
 assert.ok(!existsSync(join(attemptDir(bad.home,bad.integrated.integratedCommit,0),"run.json")));
 const failing=matrixHost({receipt:{environment:{},checks:[{exitCode:1}]}});
 await assert.rejects(failing.verify(failing.integrated),reasonIs("Integrated matrix receipt is not eligible"));assert.equal(failing.sends().length,0);
 const real=new HostAdapter({cwd:tmpdir()},{});assert.throws(()=>real.command([process.execPath,"-e","setTimeout(()=>{},5000)"],tmpdir(),{timeout:200}),e=>/ timeout$/.test(failureReason(e)));
 assert.equal(real.command([process.execPath,"-e","console.log('ok')"]).trim(),"ok");
});
test("a1 the runner has no process-count gate",()=>{
 const source=readFileSync(RUNNER,"utf8");assert.doesNotMatch(source,/pgrep|matrixRunsActive|matrixHostFree|MATRIX_RUN_PATTERN|host-wait\.json/);
 const adapter=new HostAdapter({cwd:tmpdir()},{});assert.equal(adapter.matrixRunsActive,undefined);assert.equal(adapter.matrixHostFree,undefined);
});
test("a2 the run's argv carries priority, item and host wait; priority is the job's, else matrixPriority, else high; an invalid value refuses before any run",async()=>{
 const argvOf=async(configure=()=>{},job={})=>{const h=matrixHost();configure(h.adapter.config);assert.equal(await h.adapter.verifyIntegrated({...h.integrated,itemId:"wi_0123456789abcdef",...job}),false);return {h,argv:h.matrix().find(c=>c.argv[2]==="run").argv};};
 const first=await argvOf(),dir=attemptDir(first.h.home,first.h.integrated.integratedCommit,0);
 assert.deepEqual(first.argv,["node","scripts/verify-matrix.mjs","run",join(dir,"plan.json"),dir,"--priority","high","--item","wi_0123456789abcdef","--host-wait-minutes","120"]);
 assert.deepEqual([first.h.run().priority,first.h.run().hostWaitMs],["high",MATRIX_HOST_WAIT_MS]);
 const configured=(await argvOf(c=>{c.matrixPriority="normal";c.matrixHostWaitMs=1800000;})).argv;
 assert.deepEqual(configured.slice(5),["--priority","normal","--item","wi_0123456789abcdef","--host-wait-minutes","30"]);
 assert.equal((await argvOf(c=>{c.matrixPriority="normal";},{priority:"urgent"})).argv[6],"urgent","the job's own priority wins");
 assert.equal((await argvOf(c=>{c.matrixHostWaitMs=50;})).argv.at(-1),"1","the host wait is at least one minute");
 assert.equal(matrixPriority({},{}),"high");assert.equal(matrixPriority({priority:"normal"},{matrixPriority:"urgent"}),"normal");
 for(const [configure,job] of [[c=>{c.matrixPriority="low";},{}],[c=>{c.matrixPriority="";},{}],[()=>{},{priority:"critical"}],[c=>{c.matrixPriority=1;},{}]]){
  const h=matrixHost();configure(h.adapter.config);
  await assert.rejects(h.adapter.verifyIntegrated({...h.integrated,...job}),reasonIs("Invalid matrix priority"));
  assert.equal(h.matrix().length,0,"no plan and no run");assert.ok(!existsSync(join(attemptDir(h.home,h.integrated.integratedCommit,0),"run.json")));
 }
 const wait=matrixHost();wait.adapter.config.matrixHostWaitMs=0;await assert.rejects(wait.adapter.verifyIntegrated(wait.integrated),reasonIs("Invalid matrix host wait"));assert.equal(wait.matrix().length,0);
});
test("a3 behind a live holder and an earlier normal waiter, the deployer's run at high is position 1 of 2, is granted first, and the import is then requested",async t=>{
 const h=lockHost(t,{mode:"receipt"}),lock={path:h.path,agent:"verifier",runTimeoutMs:60000,pollMs:20};
 const holder=await acquireHostLock({...lock,item:"wi_holder"});
 const earlier=acquireHostLock({...lock,item:"wi_earlier",priority:"normal",prioritySource:"flag"});
 await until(()=>readHostState(h.path).waiters.length===1,"the earlier waiter");
 assert.equal(await h.adapter.verifyIntegrated({...h.integrated,itemId:"wi_deployed"}),false);
 await until(()=>readHostState(h.path).waiters.length===2,"the deployer's run to join the list");
 assert.equal(await h.adapter.verifyIntegrated(h.integrated),false);
 assert.deepEqual(h.adapter.matrixWait,{attempt:h.integrated.integratedCommit+"-r0",position:1,length:2,priority:"high",change:1,holders:[{id:holder.id,item:"wi_holder",agent:"verifier",pid:process.pid}]});
 assert.deepEqual(readHostState(h.path).waiters.map(w=>[w.item,w.priority]),[["wi_deployed","high"],["wi_earlier","normal"]]);
 assert.equal(h.sends().length,0);assert.equal(h.starts(),1);
 await holder.release();
 const second=await earlier;await ended(h.children[0]);
 assert.deepEqual(readJournal(h.path).filter(l=>l.event==="acquire").map(l=>l.item),["wi_holder","wi_deployed","wi_earlier"],"the deployer's run is granted before the earlier normal waiter");
 assert.equal(await h.adapter.verifyIntegrated(h.integrated),false);assert.equal(h.adapter.matrixWait,null);
 const sends=h.sends();assert.equal(sends.length,1);assert.equal(sends[0].argv[sends[0].argv.indexOf("--subject")+1],"Import verification for the integrated release commit");
 assert.ok(sends[0].argv.includes(join(h.dir,"receipt.json")));assert.equal(h.starts(),1);
 await second.release();
 // A run that only waits (here a normal waiter that never takes its turn)
 // does not hold the deployer back: no process is counted, and high goes first.
 const alone=lockHost(t,{mode:"receipt"});
 assert(updateHostState(alone.path,state=>{state.requestSeq=1;state.waiters.push({id:"00000000-0000-4000-8000-000000000003",seq:1,pid:process.pid,kind:"run",item:"wi_waiting",agent:"verifier",priority:"normal",prioritySource:"default",requestedAt:new Date().toISOString(),grantSeqAtRequest:0,overtakenBy:0,runTimeoutMs:60000});}).done);
 assert.equal(await alone.adapter.verifyIntegrated(alone.integrated),false);await ended(alone.children[0]);
 assert.equal(readHostState(alone.path).waiters.length,1,"the other run is still only waiting");
 assert.equal(await alone.adapter.verifyIntegrated(alone.integrated),false);assert.equal(alone.sends().length,1);
});
test("a4 one wait notice per change of position, list length or holder, none on an unchanged poll, and one more when an earlier place returns",async t=>{
 const cwd=mkdtempSync(join(tmpdir(),"release-wait-notice-")),log=join(cwd,"log"),fakeTT=join(cwd,"tt"),commit="c".repeat(40),attempt=commit+"-r0";
 const verified={id:"rel_0123456789abcdef",state:"verified",generation:1,commit:"a".repeat(40)};
 writeFileSync(fakeTT,"#!"+process.execPath+"\n"+`const a=process.argv.slice(2);if(a[1]==='list')console.log(JSON.stringify(${JSON.stringify([verified])}));else if(a[1]==='claim')console.log(JSON.stringify(${JSON.stringify({...verified,state:"claimed",generation:2})}));else if(a[0]==='send')require('fs').appendFileSync(${JSON.stringify(log)},JSON.stringify(a)+'\\n');else process.exit(2);`);chmodSync(fakeTT,0o755);
 const A={id:"11111111-1111-4111-8111-111111111111",item:"wi_holder",agent:"verifier-1",pid:4242},B={id:"22222222-2222-4222-8222-222222222222",item:"wi_next",agent:"verifier-2",pid:4343};
 const at=(position,length,holder,change)=>({attempt,position,length,priority:"high",change,holder});
 const heldRun={attempt,pid:777,groups:[888,999],reason:"a check group of the matrix run is still alive"};
 const polls=[{wait:at(2,3,A,1)},{wait:at(2,3,A,1)},{wait:at(1,3,A,2)},{wait:at(1,2,A,3)},{wait:at(1,2,B,4)},{wait:at(1,2,null,5)},{wait:at(2,3,A,6)},{wait:at(2,3,A,6)},{},{held:heldRun},{held:heldRun}];
 let polled=0;const release=async(c,adapter)=>{const p=polls[polled++];adapter.matrixWait=p.wait??null;adapter.matrixHeld=p.held??null;return {jobId:verified.id,outcome:"waiting_matrix"};};
 const controller=new AbortController();t.mock.timers.enable({apis:["setTimeout"]});
 const serving=serveDeployment({version:1,enabled:true,cwd,journalDirectory:cwd,tt:fakeTT},{signal:controller.signal,release});
 const settle=async()=>{for(let i=0;i<20;i++)await new Promise(r=>setImmediate(r));};
 for(let n=1;n<=polls.length;n++){while(polled<n)await settle();await settle();if(n===polls.length)controller.abort();t.mock.timers.tick(30000);}
 await serving;
 const sends=readFileSync(log,"utf8").trim().split("\n").map(l=>JSON.parse(l)),field=(a,name)=>a[a.indexOf(name)+1];
 assert.deepEqual(sends.map(a=>field(a,"--request-id")),[
  `rel_0123456789abcdef-matrix-wait-${attempt}-p2-of3-${A.id}`,`rel_0123456789abcdef-matrix-wait-${attempt}-p1-of3-${A.id}-n2`,`rel_0123456789abcdef-matrix-wait-${attempt}-p1-of2-${A.id}-n3`,
  `rel_0123456789abcdef-matrix-wait-${attempt}-p1-of2-${B.id}-n4`,`rel_0123456789abcdef-matrix-wait-${attempt}-p1-of2-none-n5`,`rel_0123456789abcdef-matrix-wait-${attempt}-p2-of3-${A.id}-n6`,`rel_0123456789abcdef-matrix-held-${attempt}`]);
 assert.equal(field(sends[5],"--text"),field(sends[0],"--text"),"the earlier place, posted again under a new request id");
 assert.ok(sends.every(a=>field(a,"--request-id").length<=128 && field(a,"--kind")==="notice" && a.includes("release-job=rel_0123456789abcdef")));
 assert.equal(field(sends[0],"--subject"),"A release job is waiting for the verification host");
 assert.equal(field(sends[0],"--text"),"Release rel_0123456789abcdef waits for the verification host at position 2 of 3 at priority high behind wi_holder/verifier-1/pid 4242");
 assert.equal(field(sends[4],"--text"),"Release rel_0123456789abcdef waits for the verification host at position 1 of 2 at priority high with no holder");
 assert.match(field(sends[6],"--text"),/^Release rel_0123456789abcdef is held: its matrix run could not be confirmed stopped \(pid 777; check groups still alive: 888,999; a check group of the matrix run is still alive\)\./);
 // Only fixed-shape names from the lock file reach a notice.
 assert.match(matrixWaitNotice(verified,at(1,1,{id:"x y",item:"token=secret\nleak",agent:"a/b",pid:"1"})).text,/behind unknown\/unknown\/pid unknown$/);
 // A request id never exceeds the hub's 128 characters; the holder id is shortened first.
 const long=matrixWaitNotice(verified,{...at(99,99,A,9999),attempt:commit+"-r999"}).requestId;assert.equal(long,`rel_0123456789abcdef-matrix-wait-${commit}-r999-p99-of99-11111111-n9999`);
 assert.equal(matrixWaitNotice(verified,at(12,15,A,999)).requestId.length<=128,true);
 assert.equal(matrixWaitNotice(verified,null),null);assert.equal(matrixHeldNotice(verified,null),null);assert.equal(matrixWaitNotice(verified,{...at(1,1,null),attempt:"../x"}),null);
});
test("a5 (i) a failed intent save starts no run, and a spawn that fails at once refuses by name",async()=>{
 const h=matrixHost();h.adapter.saveRun=()=>{throw new Error("disk full");};
 await assert.rejects(h.adapter.verifyIntegrated(h.integrated),reasonIs("unclassified"));assert.deepEqual(h.matrix().map(c=>c.argv[2]),["plan"],"no run was started");
 const s=matrixHost();let starts=0;s.adapter.startMatrixRun=()=>{starts++;throw new Error("spawn EAGAIN");};
 await assert.rejects(s.adapter.verifyIntegrated(s.integrated),reasonIs("Integrated matrix run could not start"));
 assert.deepEqual([s.run().state,s.run().reason],["ended","spawn-failed"]);
 await assert.rejects(s.adapter.verifyIntegrated(s.integrated),reasonIs("Integrated matrix run could not start"));assert.equal(starts,1,"never replayed");
 const real=matrixHost();delete real.adapter.startMatrixRun;const start=real.adapter.startMatrixRun.bind(real.adapter);
 real.adapter.startMatrixRun=(argv,dir)=>start(["/nonexistent/tailterm-node",...argv.slice(1)],dir);
 await assert.rejects(real.adapter.verifyIntegrated(real.integrated),reasonIs("Integrated matrix run could not start"));assert.equal(real.adapter.matrixChildren.has(attemptDir(real.home,real.integrated.integratedCommit,0)),false);
});
test("a5 (ii) when the save after the spawn fails the poll waits, and the next poll adopts the run from the lock file",async t=>{
 const h=lockHost(t),save=h.adapter.saveRun.bind(h.adapter);let saves=0;
 h.adapter.saveRun=(dir,run)=>{if(++saves===2)throw new Error("disk full");save(dir,run);};
 assert.equal(await h.adapter.verifyIntegrated(h.integrated),false,"a run exists, so the job is not refused");
 assert.equal(h.run().state,"starting");assert.equal(h.run().pid,undefined);
 const {pid}=await h.marker();assert.equal(pid,h.children[0].pid);
 assert.equal(await h.adapter.verifyIntegrated(h.integrated),false);
 const run=h.run();assert.deepEqual([run.state,run.pid],["started",pid]);assert.equal(typeof run.processStartedAt,"string","adopted with its process start time");
 assert.equal(h.starts(),1);assert.equal(h.adapter.matrixHeld,null);
});
test("a5 (iii) a launch unconfirmed beyond the grace is held with the fence, never refused or replayed, then adopted or ended by name",async t=>{
 const launch=t=>{const h=lockHost(t),child=idle(t);let clock=1000000;h.adapter.now=()=>clock;h.tick=ms=>{clock+=ms;};
  h.adapter.startMatrixRun=(argv,dir)=>{h.calls.push({argv,dir});return child.pid;};
  // The run never registers and the confirming save is lost: the record stays "starting".
  const save=h.adapter.saveRun.bind(h.adapter);let saves=0;h.adapter.saveRun=(dir,run)=>{if(++saves===2)throw new Error("lost");save(dir,run);};
  return Object.assign(h,{child});};
 const h=launch(t);assert.equal(await h.adapter.verifyIntegrated(h.integrated),false);assert.equal(h.run().state,"starting");
 h.tick(MATRIX_LAUNCH_GRACE_MS-1);assert.equal(await h.adapter.verifyIntegrated(h.integrated),false);assert.equal(h.adapter.matrixHeld,null,"inside the grace it only waits");
 h.tick(1);
 for(let poll=0;poll<3;poll++){h.tick(3600000);assert.equal(await h.adapter.verifyIntegrated(h.integrated),false,"held, not refused, however long");}
 assert.deepEqual(h.adapter.matrixHeld,{attempt:h.integrated.integratedCommit+"-r0",pid:null,groups:[],reason:"matrix run launch unconfirmed"});
 assert.equal(matrixHeldNotice({id:"rel_fixture"},h.adapter.matrixHeld).requestId,`rel_fixture-matrix-held-${h.integrated.integratedCommit}-r0`);
 assert.equal(h.starts(),1,"no second run in the same directory");assert.equal(h.run().state,"starting");assert.deepEqual(h.signals,[]);
 // The held job is still this runner's claimed job, so the queued one is not chosen.
 const claimed={id:"rel_fixture",state:"claimed",agentId:"agt_fixture",runId:"run_fixture"},queued={id:"rel_queued",state:"verified"};
 assert.equal(runnableJob([queued,claimed],"agt_fixture","run_fixture"),claimed);assert.equal(runnableJob([queued,claimed],"agt_other","run_other"),null);
 // The run appears late: it is adopted, not restarted.
 assert(updateHostState(h.path,state=>{state.waiters.push({...holderEntry(h.child.pid,h.dir),grantSeqAtRequest:0,overtakenBy:0});}).done);
 assert.equal(await h.adapter.verifyIntegrated(h.integrated),false);
 assert.deepEqual([h.run().state,h.run().pid,h.adapter.matrixHeld,h.starts()],["started",h.child.pid,null,1]);assert.equal(typeof h.run().processStartedAt,"string");
 // Or its own sidecar shows it joined and left: the job ends by name.
 for(const [outcome,reason] of [["wait-expired","Verification host wait expired"],["released","Integrated matrix run ended without a receipt"]]){
  const e=launch(t);assert.equal(await e.adapter.verifyIntegrated(e.integrated),false);e.tick(MATRIX_LAUNCH_GRACE_MS+1);
  assert.equal(await e.adapter.verifyIntegrated(e.integrated),false);assert.equal(e.adapter.matrixHeld.reason,"matrix run launch unconfirmed");
  writeFileSync(join(e.dir,"host-lock.json"),JSON.stringify({outcome}));
  await assert.rejects(e.adapter.verifyIntegrated(e.integrated),reasonIs(reason));assert.equal(e.run().state,"ended");assert.equal(e.starts(),1);
 }
 // A pid in the lock file that began after it joined is not proof of our run.
 const reused=launch(t);assert.equal(await reused.adapter.verifyIntegrated(reused.integrated),false);
 assert(updateHostState(reused.path,state=>{state.waiters.push({...holderEntry(reused.child.pid,reused.dir,{requestedAt:"2020-01-01T00:00:00.000Z"}),grantSeqAtRequest:0,overtakenBy:0});}).done);
 assert.equal(await reused.adapter.verifyIntegrated(reused.integrated),false);assert.deepEqual([reused.run().state,reused.run().processStartedAt],["started",null]);
});
function releaseHost(t,child){
 const h=lockHost(t,child),j=job(h.f,change(h.f,"client/a.js","a"));git(h.f.cwd,"checkout","tasks-hub");change(h.f,"client/c.js","c");
 Object.assign(j,{agentId:"agt_fixture",runId:"run_fixture",generation:1});
 const c={...config(h.f,j),journalPath:join(h.home,"rel_fixture.json")};
 const through=host=>{const a=fake();a.verifyIntegrated=x=>host.verifyIntegrated(x);a.settleMatrixRuns=x=>host.settleMatrixRuns(x);return a;};
 // A second runner process: a fresh adapter with no child handle.
 const restarted=()=>{const host=new HostAdapter(h.adapter.config,{id:"rel_fixture",agentId:"agt_fixture",runId:"run_fixture",generation:1});
  for(const key of ["command","hostState","signalProcess","startMatrixRun"])host[key]=h.adapter[key];return host;};
 return Object.assign(h,{j,c,through,restarted});
}
test("a5 (iv) a new runner process resumes a started record without starting a run and with no release lock in its way",async t=>{
 const h=releaseHost(t),first=h.through(h.adapter);
 assert.equal((await runRelease(h.c,first)).outcome,"waiting_matrix");
 const integrated=git(h.f.cwd,"rev-parse","HEAD");h.dir=attemptDir(h.home,integrated,0);const {pid}=await h.marker();
 assert.equal(hostLockNames(h.f.cwd,h.j),false,"the release lock is not held while the run waits or runs");
 const host=h.restarted(),second=h.through(host);
 assert.equal((await runRelease(h.c,second)).outcome,"waiting_matrix");assert.equal((await runRelease(h.c,second)).outcome,"waiting_matrix");
 assert.equal(h.starts(),1);assert.deepEqual(h.signals,[]);assert.deepEqual(second.calls,[]);
 const run=JSON.parse(readFileSync(join(h.dir,"run.json"),"utf8"));assert.deepEqual([run.state,run.pid],["started",pid]);
 // The run finishes; the resumed runner asks for the import.
 writeFileSync(join(h.dir,"receipt.json"),JSON.stringify({environment:{},checks:[{exitCode:0}]}));process.kill(pid,"SIGTERM");await ended(h.children[0]);
 assert.equal((await runRelease(h.c,second)).outcome,"waiting_matrix");assert.equal(h.sends().length,1);assert.equal(h.starts(),1);
});
test("a5 (v) a run that left on its wait bound refuses as Verification host wait expired, any other ending without a receipt by its own name",async()=>{
 for(const [sidecar,reason] of [[{outcome:"wait-expired"},"Verification host wait expired"],[{outcome:"released"},"Integrated matrix run ended without a receipt"],[{outcome:"withdrawn"},"Integrated matrix run ended without a receipt"]]){
  const h=matrixHost(),dir=attemptDir(h.home,h.integrated.integratedCommit,0);
  h.adapter.startMatrixRun=(argv,d)=>{h.calls.push({argv,dir:d});writeFileSync(join(d,"host-lock.json"),JSON.stringify(sidecar));return exitedPid();};
  assert.equal(await h.adapter.verifyIntegrated(h.integrated),false);
  await assert.rejects(h.adapter.verifyIntegrated(h.integrated),reasonIs(reason));
  await assert.rejects(h.adapter.verifyIntegrated(h.integrated),reasonIs(reason),"the saved ending is final");
  assert.equal(h.matrix().filter(c=>c.argv[2]==="run").length,1);assert.equal(h.sends().length,0);assert.equal(JSON.parse(readFileSync(join(dir,"run.json"),"utf8")).state,"ended");
 }
 // A run that passed while its record was still "starting" (the runner stopped
 // after the intent save and never saw it on the list) takes the receipt path.
 const passed=(receipt,sidecar)=>{const h=matrixHost(),dir=attemptDir(h.home,h.integrated.integratedCommit,0);mkdirSync(dir,{recursive:true});h.adapter.hostState=()=>null;
  writeFileSync(join(dir,"run.json"),JSON.stringify({version:1,state:"starting",launchedAt:Date.now()-600000,priority:"high",hostWaitMs:7200000,boundMs:7200000}));
  writeFileSync(join(dir,"host-lock.json"),JSON.stringify(sidecar));if(receipt)writeFileSync(join(dir,"receipt.json"),JSON.stringify(receipt));return h;};
 const ok=passed({environment:{},checks:[{exitCode:0}]},{outcome:"released"});
 assert.equal(await ok.adapter.verifyIntegrated(ok.integrated),false);assert.equal(ok.run().reason,"receipt");assert.equal(ok.run().refusal,undefined);
 assert.equal(ok.sends().length,1);assert.equal(ok.sends()[0].argv[ok.sends()[0].argv.indexOf("--subject")+1],"Import verification for the integrated release commit");assert.equal(ok.matrix().length,0,"no run was started");
 const failed=passed({environment:{},checks:[{exitCode:1}]},{outcome:"released"});
 await assert.rejects(failed.adapter.verifyIntegrated(failed.integrated),reasonIs("Integrated matrix receipt is not eligible"));assert.equal(failed.run().reason,"receipt");
 for(const [sidecar,reason] of [[{outcome:"wait-expired"},"Verification host wait expired"],[{outcome:"released"},"Integrated matrix run ended without a receipt"]]){
  const none=passed(null,sidecar);await assert.rejects(none.adapter.verifyIntegrated(none.integrated),reasonIs(reason));assert.equal(none.sends().length,0);
 }
 // A record the handler set aside counts as ended.
 const h=matrixHost(),dir=attemptDir(h.home,h.integrated.integratedCommit,0);mkdirSync(dir,{recursive:true});writeFileSync(join(dir,"run.json.set-aside"),JSON.stringify({state:"starting"}));
 await assert.rejects(h.adapter.verifyIntegrated(h.integrated),reasonIs("Integrated matrix run ended without a receipt"));assert.equal(h.matrix().length,0);
});
test("a10 (i) past the deadline a run that exits on SIGTERM is refused as exceeded only once it is confirmed stopped",async t=>{
 const h=lockHost(t);let clock=Date.now();h.adapter.now=()=>clock;
 assert.equal(await h.adapter.verifyIntegrated(h.integrated),false);const {pid}=await h.marker();
 clock=h.deadline()-1;assert.equal(await h.adapter.verifyIntegrated(h.integrated),false);assert.deepEqual(h.signals,[],"not before the deadline");
 assert.equal(h.deadline(),h.run().launchedAt+MATRIX_HOST_WAIT_MS+7200000+RUN_TIMEOUT_GRACE_MS+60000);
 clock=h.deadline();assert.equal(await h.adapter.verifyIntegrated(h.integrated),false,"stopping is not yet refusal");
 assert.deepEqual(h.signals,[[pid,"SIGTERM"]]);assert.equal(h.run().stopRequestedAt,clock);
 await ended(h.children[0]);assert.equal(JSON.parse(readFileSync(join(h.dir,"host-lock.json"),"utf8")).outcome,"released");
 await assert.rejects(h.adapter.verifyIntegrated(h.integrated),reasonIs("Integrated matrix run exceeded its bound"));
 assert.deepEqual(h.signals,[[pid,"SIGTERM"]]);assert.deepEqual([h.run().state,h.run().reason],["ended","stopped"]);assert.equal(readHostState(h.path).holder,null);
});
test("a10 (ii) a run that ignores SIGTERM gets its own group killed after the grace, and is refused only once confirmed stopped",async t=>{
 const h=lockHost(t,{resist:true});let clock=Date.now();h.adapter.now=()=>clock;
 assert.equal(await h.adapter.verifyIntegrated(h.integrated),false);const {pid}=await h.marker();
 clock=h.deadline();assert.equal(await h.adapter.verifyIntegrated(h.integrated),false);
 clock+=MATRIX_STOP_GRACE_MS-1;assert.equal(await h.adapter.verifyIntegrated(h.integrated),false);assert.deepEqual(h.signals,[[pid,"SIGTERM"]],"no kill inside the grace");assert.equal(pidGone(pid),false);
 clock+=1;assert.equal(await h.adapter.verifyIntegrated(h.integrated),false,"killing is not yet refusal");assert.deepEqual(h.signals,[[pid,"SIGTERM"],[-pid,"SIGKILL"]]);
 await ended(h.children[0]);
 // No release record of its own: its lock entry, read after it was gone, shows no check group.
 await assert.rejects(h.adapter.verifyIntegrated(h.integrated),reasonIs("Integrated matrix run exceeded its bound"));
 assert.deepEqual(h.run().snapshot.groups,[]);assert.equal(h.run().snapshot.role,"holder");assert.equal(h.signals.length,2);
});
test("a10 (iii) a stale lock entry matching a reused pid is never signalled, and the job is held",async t=>{
 const h=lockHost(t),other=idle(t);let clock=5000000;h.adapter.now=()=>clock;h.adapter.startMatrixRun=(argv,dir)=>{h.calls.push({argv,dir});return other.pid;};
 h.adapter.processStartTime=()=>"Thu Jan  1 00:00:00 2026";
 assert.equal(await h.adapter.verifyIntegrated(h.integrated),false);assert.equal(h.run().processStartedAt,"Thu Jan  1 00:00:00 2026");
 // The pid now belongs to an unrelated live process; the lock file still names it for this directory.
 delete h.adapter.processStartTime;assert.notEqual(h.adapter.processStartTime(other.pid),"Thu Jan  1 00:00:00 2026");
 assert(updateHostState(h.path,state=>{state.holders=[holderEntry(other.pid,h.dir)];}).done);
 clock=h.deadline()+3600000;
 for(let poll=0;poll<3;poll++){clock+=MATRIX_STOP_GRACE_MS;assert.equal(await h.adapter.verifyIntegrated(h.integrated),false);}
 assert.deepEqual(h.signals,[]);assert.equal(pidGone(other.pid),false);
 assert.deepEqual(h.adapter.matrixHeld,{attempt:h.integrated.integratedCommit+"-r0",pid:other.pid,groups:[],reason:"no process-instance proof for the matrix run pid"});
 assert.equal(h.run().state,"started");assert.equal(h.run().stopRequestedAt,undefined);
});
test("a10 (iv) after a takeover while a recorded check group survives, the group is not signalled and the job is held with one notice naming it",async t=>{
 const h=lockHost(t,{group:true});
 assert.equal(await h.adapter.verifyIntegrated(h.integrated),false);const {pid,group}=await h.marker();t.after(()=>{try{process.kill(-group,"SIGKILL");}catch{}});
 await until(()=>readHostState(h.path).holder?.groups?.includes(group),"the check group on file");
 assert.equal(await h.adapter.verifyIntegrated(h.integrated),false);assert.deepEqual(h.run().groups,[group],"seen groups are persisted");
 process.kill(pid,"SIGKILL");await ended(h.children[0]);
 const requestIds=new Set();
 for(const takeover of [false,true,true]){
  if(takeover)assert(updateHostState(h.path,state=>{state.holders=[otherHolder()];}).done);
  assert.equal(await h.adapter.verifyIntegrated(h.integrated),false,"held, never refused");
  assert.deepEqual(h.adapter.matrixHeld,{attempt:h.integrated.integratedCommit+"-r0",pid,groups:[group],reason:"a check group of the matrix run is still alive"});
  const notice=matrixHeldNotice({id:"rel_fixture"},h.adapter.matrixHeld);requestIds.add(notice.requestId);assert.ok(notice.text.includes(`check groups still alive: ${group};`));
 }
 assert.equal(requestIds.size,1,"one held notice");assert.deepEqual(h.signals,[]);assert.equal(groupGone(group),false,"the check group was never signalled");
 assert.deepEqual(h.run().snapshot.groups,[group]);assert.equal(h.run().state,"started");assert.equal(h.sends().length,0);
 // Once the group is gone the persisted evidence is complete.
 process.kill(-group,"SIGKILL");await until(()=>groupGone(group),"the group to end");
 await assert.rejects(h.adapter.verifyIntegrated(h.integrated),reasonIs("Integrated matrix run ended without a receipt"));
});
test("a10 (v) a killed run with no release record whose holder entry was replaced before any snapshot after its exit is held",async t=>{
 const h=lockHost(t);
 assert.equal(await h.adapter.verifyIntegrated(h.integrated),false);const {pid}=await h.marker();
 assert.equal(await h.adapter.verifyIntegrated(h.integrated),false);
 process.kill(pid,"SIGKILL");await ended(h.children[0]);
 assert(updateHostState(h.path,state=>{state.holders=[otherHolder()];}).done);
 for(let poll=0;poll<3;poll++)assert.equal(await h.adapter.verifyIntegrated(h.integrated),false);
 assert.deepEqual(h.adapter.matrixHeld,{attempt:h.integrated.integratedCommit+"-r0",pid,groups:[],reason:"check group state cannot be shown"});
 assert.equal(h.run().snapshot,undefined);assert.equal(h.run().state,"started");assert.deepEqual(h.signals,[]);
});
test("a10 the default launch is detached with its output on file, and this process's live child handle is proof for stopping it",async t=>{
 const h=matrixHost(),dir=attemptDir(h.home,h.integrated.integratedCommit,0),signals=[];let clock=Date.now();h.adapter.now=()=>clock;
 delete h.adapter.startMatrixRun;const start=h.adapter.startMatrixRun.bind(h.adapter);
 h.adapter.startMatrixRun=(argv,d)=>start([process.execPath,"-e","console.log(process.argv.length);setInterval(()=>{},1000)",...argv.slice(1)],d);
 h.adapter.hostState=()=>null;h.adapter.signalProcess=(target,name)=>{signals.push([target,name]);process.kill(target,name);};
 assert.equal(await h.adapter.verifyIntegrated(h.integrated),false);
 const child=h.adapter.matrixChildren.get(dir),run=h.run();t.after(()=>{try{process.kill(-child.pid,"SIGKILL");}catch{}});
 assert.deepEqual([run.state,run.pid,run.processStartedAt],["started",child.pid,null]);
 await until(()=>readFileSync(join(dir,"run.out"),"utf8").trim()==="11","the run's output on file");
 assert.equal(statSync(join(dir,"run.out")).mode&0o777,0o600);assert.equal(execFileSync("ps",["-o","pgid=","-p",String(child.pid)],{encoding:"utf8"}).trim(),String(child.pid),"its own group leader");
 clock=run.launchedAt+run.hostWaitMs+run.boundMs+RUN_TIMEOUT_GRACE_MS+MATRIX_DEADLINE_SLACK_MS;
 assert.equal(await h.adapter.verifyIntegrated(h.integrated),false);assert.deepEqual(signals,[[child.pid,"SIGTERM"]],"no start time was recorded, the handle is the proof");
 await ended(child);
 // It never joined the host list and left no record of its own: held, not refused.
 assert.equal(await h.adapter.verifyIntegrated(h.integrated),false);assert.equal(h.adapter.matrixHeld.reason,"check group state cannot be shown");
});
test("a4 the attempt record counts each place the run has had, so a returned place is a new notice and a restart's resend is not",async t=>{
 const h=lockHost(t),lock={path:h.path,agent:"verifier",runTimeoutMs:60000,pollMs:20};
 const holder=await acquireHostLock({...lock,item:"wi_holder"});
 assert.equal(await h.adapter.verifyIntegrated(h.integrated),false);await until(()=>readHostState(h.path).waiters.length===1,"the run on the list");
 const poll=async adapter=>{assert.equal(await adapter.verifyIntegrated(h.integrated),false);return matrixWaitNotice({id:"rel_fixture"},adapter.matrixWait).requestId;};
 const base=`rel_fixture-matrix-wait-${h.integrated.integratedCommit}-r0`,first=await poll(h.adapter);
 assert.equal(first,`${base}-p1-of1-${holder.id}`);assert.equal(await poll(h.adapter),first,"an unchanged poll is the same notice");
 // A more urgent run joins, then leaves: the place returns to the earlier one.
 const urgent={id:"00000000-0000-4000-8000-000000000009",seq:99,pid:process.pid,kind:"run",item:"wi_urgent",agent:"verifier",priority:"urgent",prioritySource:"flag",requestedAt:new Date().toISOString(),grantSeqAtRequest:0,overtakenBy:0,runTimeoutMs:60000};
 // One attempt at the file mutex can lose to the waiting run's own poll, so the edit is retried.
 const edit=change=>until(()=>updateHostState(h.path,change).done,"the lock file edit");
 await edit(state=>{state.waiters.unshift(urgent);});
 assert.equal(await poll(h.adapter),`${base}-p2-of2-${holder.id}-n2`);
 await edit(state=>{state.waiters=state.waiters.filter(w=>w.id!==urgent.id);});
 const back=await poll(h.adapter);assert.equal(back,`${base}-p1-of1-${holder.id}-n3`);assert.notEqual(back,first);
 // A restarted runner resends the same place under the same id.
 const restarted=new HostAdapter(h.adapter.config,{id:"rel_fixture",agentId:"agt_fixture",runId:"run_fixture",generation:1});restarted.command=h.adapter.command;restarted.hostState=h.adapter.hostState;
 assert.equal(await poll(restarted),back);assert.deepEqual([h.run().waitChange,h.starts()],[3,1]);
 await holder.release();
});
test("a10 a11 the fence notice never advises set-aside while the job's matrix run is starting, waiting, running, held or unreadable",async t=>{
 const holder={id:"rel_a",state:"claimed",agentId:"agt_d",runId:"run_d",taskId:"tsk_x"},jobs=[holder,{id:"rel_b",state:"verified"}];
 const keeps=/^Release rel_a holds the project release fence and is waiting for the handler to import integrated verification; release rel_b is queued behind it\. It keeps the fence: its integrated matrix run has not ended and may be using the checkout, so it is not set aside until the run\.json of each of its attempts says ended\.$/;
 const byDefault=fenceWaitNotice(jobs,holder,"waiting_matrix",false);assert.match(byDefault.text,keeps,"with nothing known about the run, a job waiting for its matrix is not offered for set-aside");
 assert.equal(byDefault.requestId,"rel_a-fence-wait-rel_b-waiting_matrix-matrix");assert.doesNotMatch(byDefault.text,/tt deployment set-aside/);
 assert.match(fenceWaitNotice(jobs,holder,"waiting_matrix",true,true).text,keeps,"the run outranks the lock advice");
 const ended=fenceWaitNotice(jobs,holder,"waiting_matrix",false,false);assert.match(ended.text,/the handler can move it aside with tt deployment set-aside/);assert.equal(ended.requestId,"rel_a-fence-wait-rel_b-waiting_matrix");
 assert.equal(fenceWaitNotice(jobs,holder,"waiting_matrix",true,false).requestId,"rel_a-fence-wait-rel_b-waiting_matrix-locked");
 assert.match(fenceWaitNotice(jobs,holder,"claimed",false).text,/tt deployment set-aside/,"other reasons are unchanged unless a run is found");assert.match(fenceWaitNotice(jobs,holder,"claimed",false,true).text,/is claimed by another deployer run; release rel_b is queued behind it\. It keeps the fence: its integrated matrix run has not ended/);
 assert.match(fenceWaitNotice(jobs,{...holder,inputsDigest:"a".repeat(64)},"waiting_inputs",false,true).text,/It keeps the fence until handler reconciliation\.$/);
 // The daemon reads every attempt record of the holding job.
 const commit="c".repeat(40),started={version:1,state:"started",pid:process.pid,launchedAt:Date.now(),hostWaitMs:7200000,boundMs:7200000,groups:[]};
 const daemon=(records,held)=>{
  const cwd=mkdtempSync(join(tmpdir(),"release-fence-matrix-")),log=join(cwd,"log"),fakeTT=join(cwd,"tt");
  const a={id:"rel_a",state:"verified",generation:1,commit:"a".repeat(40)},b={id:"rel_b",state:"verified",generation:1,commit:"b".repeat(40)};
  writeFileSync(fakeTT,"#!"+process.execPath+"\n"+`const a=process.argv.slice(2);if(a[1]==='list')console.log(JSON.stringify(${JSON.stringify([a,b])}));else if(a[1]==='claim')console.log(JSON.stringify(${JSON.stringify({...a,state:"claimed",generation:2,agentId:"agt_d",runId:"run_d"})}));else if(a[0]==='send')require('fs').appendFileSync(${JSON.stringify(log)},JSON.stringify(a)+'\\n');else process.exit(2);`);chmodSync(fakeTT,0o755);
  records.forEach((record,n)=>{const dir=join(cwd,"rel_a-integrated-verification",`${commit}-r${n}`);mkdirSync(dir,{recursive:true});if(record!==null)writeFileSync(join(dir,record==="set-aside"?"run.json.set-aside":"run.json"),typeof record==="string"?record:JSON.stringify(record));});
  const release=async(c,adapter)=>{adapter.matrixHeld=held?{attempt:commit+"-r0",pid:777,groups:[888],reason:"a check group of the matrix run is still alive"}:null;return {jobId:"rel_a",outcome:"waiting_matrix"};};
  return serveDeployment({version:1,enabled:true,cwd,journalDirectory:cwd,tt:fakeTT},{once:true,release}).then(()=>({cwd,texts:readFileSync(log,"utf8").trim().split("\n").map(l=>JSON.parse(l)).map(x=>x[x.indexOf("--text")+1])}));
 };
 for(const [what,records,held] of [["starting",[{version:1,state:"starting",launchedAt:Date.now()}]],["waiting or running",[started]],["held",[started],true],["an unreadable record",["{not json"]],["an earlier attempt still running",[started,{state:"ended",reason:"receipt"}]]]){
  const {cwd,texts}=await daemon(records,held);
  assert.equal(texts.filter(x=>keeps.test(x)).length,1,what+": the fence notice says the job keeps the fence");
  assert.ok(texts.every(x=>!/tt deployment set-aside/.test(x)),what+": no notice advises set-aside");assert.equal(texts.length,held?2:1);
  assert.equal(matrixRunUnsettled(cwd,{id:"rel_a"}),true);
  if(held)assert.ok(texts.some(x=>/^Release rel_a is held: its matrix run could not be confirmed stopped/.test(x)),"and the held notice agrees with it");
 }
 for(const [what,records] of [["ended",[{state:"ended",reason:"receipt"}]],["set aside",["set-aside",{state:"ended",reason:"no-receipt"}]],["no attempt",[]],["an attempt with no record",[null]]]){
  const {cwd,texts}=await daemon(records);assert.equal(texts.length,1,what);assert.match(texts[0],/the handler can move it aside with tt deployment set-aside/,what);assert.equal(matrixRunUnsettled(cwd,{id:"rel_a"}),false);
 }
 const file=mkdtempSync(join(tmpdir(),"release-fence-doubt-"));writeFileSync(join(file,"rel_a-integrated-verification"),"");assert.equal(matrixRunUnsettled(file,{id:"rel_a"}),true,"true on doubt");
});
test("a10 the snapshot of a gone run's lock entry is read after its pid was seen gone",async t=>{
 const h=lockHost(t),group=idle(t),pid=exitedPid();h.adapter.startMatrixRun=(argv,dir)=>{h.calls.push({argv,dir});return pid;};h.adapter.processStartTime=()=>null;
 assert(updateHostState(h.path,state=>{state.holders=[holderEntry(pid,h.dir)];}).done);
 // The run records a check group between the poll's first read of the lock file and the pid check.
 h.adapter.pidGone=target=>{assert.equal(target,pid);assert(updateHostState(h.path,state=>{state.holders[0].groups=[group.pid];}).done);return true;};
 assert.equal(await h.adapter.verifyIntegrated(h.integrated),false);
 assert.equal(await h.adapter.verifyIntegrated(h.integrated),false,"held: the group recorded last is seen");
 assert.deepEqual(h.adapter.matrixHeld,{attempt:h.integrated.integratedCommit+"-r0",pid,groups:[group.pid],reason:"a check group of the matrix run is still alive"});
 assert.deepEqual(h.run().snapshot.groups,[group.pid]);assert.equal(groupGone(group.pid),false);
});
test("a10 a14 unreadable attempt records hold a requeued job with a notice",async t=>{
 const home=mkdtempSync(join(tmpdir(),"release-attempts-")),a=new HostAdapter({cwd:tmpdir(),journalDirectory:home},{id:"rel_fixture"});
 assert.equal(await a.settleMatrixRuns({id:"rel_fixture"}),true,"no attempts");
 writeFileSync(join(home,"rel_fixture-integrated-verification"),"not a directory");
 assert.equal(await a.settleMatrixRuns({id:"rel_fixture"}),false);assert.deepEqual(a.matrixHeld,{attempt:"attempts",pid:null,groups:[],reason:"attempt records unreadable"});
 const notice=matrixHeldNotice({id:"rel_fixture"},a.matrixHeld);assert.equal(notice.requestId,"rel_fixture-matrix-held-attempts");
 assert.match(notice.text,/^Release rel_fixture is held: its matrix run could not be confirmed stopped \(pid unknown; check groups still alive: none; attempt records unreadable\)\./);
 assert.equal(matrixHeldNotice({id:"rel_fixture"},{attempt:"",pid:null,groups:[],reason:"x"}),null);
});
test("a10 the live child of this process stays proof across the daemon's per-poll adapters",async t=>{
 const h=matrixHost(),dir=attemptDir(h.home,h.integrated.integratedCommit,0),signals=[];let clock=Date.now();
 delete h.adapter.startMatrixRun;const start=h.adapter.startMatrixRun.bind(h.adapter);
 h.adapter.startMatrixRun=(argv,d)=>start([process.execPath,"-e","setInterval(()=>{},1000)"],d);h.adapter.hostState=()=>null;
 assert.equal(await h.adapter.verifyIntegrated(h.integrated),false);assert.equal(h.run().processStartedAt,null,"no start time could be read at launch");
 // The next poll's adapter is a new object in the same process.
 const next=new HostAdapter(h.adapter.config,{id:"rel_fixture",agentId:"agt_fixture",runId:"run_fixture",generation:1});
 const child=next.matrixChildren.get(dir);assert.ok(child);t.after(()=>{try{process.kill(-child.pid,"SIGKILL");}catch{}});
 next.command=h.adapter.command;next.hostState=()=>null;next.processStartTime=()=>null;next.now=()=>clock;next.signalProcess=(target,name)=>{signals.push([target,name]);process.kill(target,name);};
 const run=h.run();clock=run.launchedAt+run.hostWaitMs+run.boundMs+RUN_TIMEOUT_GRACE_MS+MATRIX_DEADLINE_SLACK_MS;
 assert.equal(await next.verifyIntegrated(h.integrated),false);assert.deepEqual(signals,[[child.pid,"SIGTERM"]]);assert.equal(next.matrixHeld,null);
 await ended(child);
});
test("a14 a requeued job with an unresolved earlier attempt does not touch the checkout until that attempt is stopped, ended or set aside",async t=>{
 const requeue=h=>{const digestOf=createHash("sha256").update(readFileSync(h.c.journalPath)).digest("hex");
  return {...h.c,job:{...h.j,generation:3,reconciliations:[{disposition:"requeue",noActiveExecution:true,noPublication:true,journalState:"no_effects",jobId:"rel_fixture",journalDigest:digestOf,agentId:"agt_fixture",runId:"run_fixture"}]}};};
 const attempts=h=>readdirSync(join(h.home,"rel_fixture-integrated-verification")).sort();
 // An earlier attempt whose run is still alive is stopped first.
 const h=releaseHost(t);assert.equal((await runRelease(h.c,h.through(h.adapter))).outcome,"waiting_matrix");
 const integrated=git(h.f.cwd,"rev-parse","HEAD");h.dir=attemptDir(h.home,integrated,0);const {pid}=await h.marker();
 // tasks-hub moves on, so a fresh integration would move the checkout.
 git(h.f.cwd,"update-ref","refs/heads/tasks-hub",git(h.f.cwd,"commit-tree",git(h.f.cwd,"rev-parse","tasks-hub^{tree}"),"-p","tasks-hub","-m","later"));
 const c=requeue(h),host=h.restarted(),a=h.through(host);
 assert.equal((await runRelease(c,a)).outcome,"waiting_matrix");
 assert.deepEqual(h.signals,[[pid,"SIGTERM"]],"the earlier run is stopped with process-instance proof");
 assert.equal(git(h.f.cwd,"rev-parse","HEAD"),integrated,"the checkout is untouched");assert.ok(!existsSync(c.journalPath),"no checkpoint");
 assert.deepEqual(attempts(h),[integrated+"-r0"]);assert.equal(h.starts(),1);assert.deepEqual(a.calls,[]);assert.equal(hostLockNames(h.f.cwd,c.job),false);
 await ended(h.children[0]);
 assert.equal((await runRelease(c,a)).outcome,"waiting_matrix");
 const next=git(h.f.cwd,"rev-parse","HEAD");assert.notEqual(next,integrated);assert.deepEqual(attempts(h),[integrated+"-r0",next+"-r1"].sort());assert.equal(h.starts(),2,"only now is a new run started");
 assert.equal(JSON.parse(readFileSync(join(h.dir,"run.json"),"utf8")).state,"ended");assert.deepEqual(a.calls,[]);
 // An unconfirmed launch in an earlier attempt holds the job until it is set aside.
 const u=releaseHost(t);u.adapter.startMatrixRun=()=>{throw new Error("unused");};
 const a0=u.through(u.adapter);a0.verifyIntegrated=async()=>false;assert.equal((await runRelease(u.c,a0)).outcome,"waiting_matrix");
 const head=git(u.f.cwd,"rev-parse","HEAD"),dir=attemptDir(u.home,head,0);mkdirSync(dir,{recursive:true});
 writeFileSync(join(dir,"run.json"),JSON.stringify({version:1,state:"starting",launchedAt:Date.now(),priority:"high",hostWaitMs:7200000,boundMs:7200000}));
 const uc=requeue(u),ua=u.through(u.restarted());let started=0;
 for(let poll=0;poll<3;poll++)assert.equal((await runRelease(uc,ua)).outcome,"waiting_matrix");
 assert.equal(git(u.f.cwd,"rev-parse","HEAD"),head);assert.ok(!existsSync(uc.journalPath));assert.deepEqual(ua.calls,[],"no refusal");assert.deepEqual(attempts(u),[head+"-r0"]);assert.deepEqual(u.signals,[]);
 assert.equal(JSON.parse(readFileSync(join(dir,"run.json"),"utf8")).state,"starting");
 const claimed={...uc.job,state:"claimed"};assert.equal(runnableJob([{id:"rel_queued",state:"verified"},claimed],"agt_fixture","run_fixture"),claimed,"no other job integrates");
 renameSync(join(dir,"run.json"),join(dir,"run.json.set-aside"));
 const settled=u.restarted();settled.startMatrixRun=(argv,d)=>{started++;return exitedPid();};settled.processStartTime=()=>null;
 assert.equal((await runRelease(uc,u.through(settled))).outcome,"waiting_matrix");assert.equal(started,1,"the set-aside record no longer holds the job");assert.ok(existsSync(uc.journalPath));
});
test("p2 the journal is waiting_matrix during the integrated run, so a stopped runner resumes the same job",async()=>{
 const f=fixture(),j=job(f,change(f,"client/a.js","a"));git(f.cwd,"checkout","tasks-hub");change(f,"client/c.js","c");
 const c=config(f,j),a=fake();let during,phaseAtMerge;
 a.verifyIntegrated=async x=>{during=readFileSync(c.journalPath,"utf8");return false;};
 assert.equal((await runRelease(c,a)).outcome,"waiting_matrix");
 const saved=JSON.parse(during);assert.equal(saved.phase,"waiting_matrix");assert.equal(saved.integrated,git(f.cwd,"rev-parse","HEAD"));
 // The journal as a runner stopped mid-run left it.
 writeFileSync(c.journalPath,during);let integratedCommits=[];
 a.verifyIntegrated=async x=>{integratedCommits.push(x.integratedCommit);return true;};a.merged=async()=>{phaseAtMerge=JSON.parse(readFileSync(c.journalPath,"utf8")).phase;a.calls.push("merged");};
 const receipt=await runRelease(c,a);assert.equal(receipt.outcome,"released");assert.deepEqual(integratedCommits,[saved.integrated]);assert.equal(receipt.commit,saved.integrated);
 assert.equal(phaseAtMerge,"integrated","a verified run leaves waiting_matrix before publication");
});
test("p2 a requeue never reuses an earlier attempt's receipt, for the same or a new integrated commit",async()=>{
 const receipt={environment:{},checks:[{exitCode:1}]},h=matrixHost({receipt}),old=h.integrated.integratedCommit,moved="d".repeat(40);
 await assert.rejects(h.verify(h.integrated),e=>failureReason(e)==="Integrated matrix receipt is not eligible");
 await assert.rejects(h.adapter.verifyIntegrated(h.integrated),/not eligible/);assert.equal(h.matrix().filter(c=>c.argv[2]==="run").length,1,"the same attempt does not rerun");
 receipt.checks=[{exitCode:0}];h.calls.length=0;
 assert.equal(await h.verify({...h.integrated,reconciliations:[{}]}),false);
 assert.deepEqual(h.matrix().map(c=>c.argv[2]),["plan","run"],"a requeue of the same commit reruns");
 assert.ok(h.sends()[0].argv.includes(join(attemptDir(h.home,old,1),"receipt.json")));
 h.calls.length=0;
 assert.equal(await h.verify({...h.integrated,integratedCommit:moved,reconciliations:[{},{}]}),false);
 assert.deepEqual(h.matrix().map(c=>c.argv[2]),["plan","run"],"a new integrated commit reruns");
 const send=h.sends()[0].argv;assert.ok(send.includes(`integrated-commit=${moved}`));assert.ok(send.includes(join(attemptDir(h.home,moved,2),"receipt.json")));
 assert.equal(JSON.parse(readFileSync(join(attemptDir(h.home,moved,2),"context.json"),"utf8")).commit,moved);
 await assert.rejects(h.adapter.verifyIntegrated({...h.integrated,integratedCommit:"../escape"}),/Exact integrated commit/);
});
// TailOS switch window: fake release.json reads and a fake clock whose sleep
// only advances time. No network and no real wait.
function fakeTailOS(responses){let t=0;const fetches=[];return {fetches,now:()=>t,sleep:async ms=>{t+=ms;},fetchJSON:async(url,timeoutMs)=>{const r=responses[Math.min(fetches.length,responses.length-1)];fetches.push({url,timeoutMs});if(r instanceof Error)throw r;return r;}};}
function tailosAdapter(responses,tailos={}){
 const home=mkdtempSync(join(tmpdir(),"tailos-wait-")),a=new HostAdapter({cwd:home,journalDirectory:home,targets:{tailos:{url:"https://tailos.test/release.json",...tailos}}},{id:"rel_fixture"});
 a.probeDeps=fakeTailOS(responses);a.artifacts.set("tailos",{commit:"d".repeat(40)});return a;
}
test("R1 the TailOS live check waits through the domain switch and fails as identity only after the window",async()=>{
 const stale={commit:"c".repeat(40)},fresh={commit:"d".repeat(40)};
 const a=tailosAdapter([stale,stale,stale,fresh]);assert.equal(await a.check("tailos"),true);
 assert.deepEqual(a.probeWait("tailos","live"),{lastCommit:"d".repeat(40),waitedMs:9000});assert.equal(a.probeDeps.fetches.length,4);
 assert.ok(a.probeDeps.fetches.every(f=>f.url==="https://tailos.test/release.json"&&f.timeoutMs===10000));
 const never=tailosAdapter([stale]);assert.equal(await never.check("tailos"),"identity");
 assert.deepEqual(never.probeWait("tailos","live"),{lastCommit:"c".repeat(40),waitedMs:90000});assert.equal(never.probeDeps.fetches.length,31);
 const six=tailosAdapter([stale],{switchWindowMs:6000});assert.equal(await six.check("tailos"),"identity");assert.equal(six.probeWait("tailos","live").waitedMs,6000);
 assert.equal(await tailosAdapter([stale],{switchWindowMs:-1}).check("tailos"),"identity");
});
test("R2 liveCheck does not repeat the TailOS switch window",async()=>{
 const a=tailosAdapter([{commit:"c".repeat(40)}]);let checks=0;const check=a.check.bind(a);a.check=async t=>{checks++;return check(t);};
 assert.equal(await liveCheck(a,"tailos",{startupMs:60000,failures:3,intervalMs:5000,relayCleanMs:0},async()=>{throw new Error("unexpected sleep");},()=>0),false);
 assert.equal(checks,1);assert.equal(a.probeDeps.fetches.length,31);
});
test("R3 the TailOS rollback records the probe's last commit and wait, dropping anything else",async()=>{
 const probe=out=>[process.execPath,"-e",`console.log(${JSON.stringify(JSON.stringify(out))})`];
 const a=tailosAdapter([]);a.artifacts.set("tailos",{rollbackSafe:true,rollbackProgram:[process.execPath,"-e","0"],rollbackProbe:probe({restored:false,databaseWritesPreserved:true,lastCommit:"d".repeat(40),waitedMs:90000})});
 assert.equal(await a.rollback("tailos"),false);assert.deepEqual(a.probeWait("tailos","rollback"),{lastCommit:"d".repeat(40),waitedMs:90000});
 const junk=tailosAdapter([]);junk.artifacts.set("tailos",{rollbackSafe:true,rollbackProgram:[process.execPath,"-e","0"],rollbackProbe:probe({restored:true,databaseWritesPreserved:true,lastCommit:"<b>SYNTHETIC_PRIVATE_TOKEN</b>",waitedMs:-3})});
 assert.equal(await junk.rollback("tailos"),true);assert.deepEqual(junk.probeWait("tailos","rollback"),{lastCommit:null,waitedMs:null});
});
test("R4 a failed TailOS release journals both waits and escalates them without changing the receipt",async()=>{
 const f=fixture(),j=job(f,change(f,"client/a.js","a")),a=fake(),c=config(f,j);let receipt,escalation;
 const live={lastCommit:"c".repeat(40),waitedMs:90000},back={lastCommit:null,waitedMs:90000};
 a.check=async()=>"identity";a.probeWait=(t,kind)=>t==="tailos"?(kind==="live"?live:back):undefined;a.finish=async r=>{receipt=r;a.calls.push("finish");};a.escalate=async d=>{escalation=d;a.calls.push("escalate");};
 await assert.rejects(runRelease(c,a),/inspect saved journal/);
 const effect=JSON.parse(readFileSync(c.journalPath,"utf8")).effects.find(e=>e.target==="tailos");
 assert.deepEqual(effect.liveCheck,live);assert.deepEqual(effect.rollbackCheck,back);
 assert.deepEqual(escalation.probeWaits,[{target:"tailos",probe:"live",...live},{target:"tailos",probe:"rollback",...back}]);
 assert.deepEqual(Object.keys(receipt).sort(),["commit","jobId","outcome","revert","targets","verificationDigest","version"]);
 assert.deepEqual(receipt.targets.map(t=>Object.keys(t).sort()),[["artifactSHA256","outcome","release","rollback","target"]]);
});
test("R5 the escalation names the TailOS waits and never echoes release.json content",async()=>{
 const cwd=mkdtempSync(join(tmpdir(),"escalate-tailos-")),calls=[];const adapter=new HostAdapter({cwd,journalDirectory:cwd},{id:"rel_fixture"});adapter.command=argv=>{calls.push(argv);return "";};
 await adapter.escalate({outcome:"rolled_back",probeWaits:[{target:"tailos",probe:"live",lastCommit:"c".repeat(40),waitedMs:90000},{target:"tailos",probe:"rollback",lastCommit:null,waitedMs:89600},{target:"tailos",probe:"rollback",lastCommit:"<b>SYNTHETIC_PRIVATE_TOKEN</b>",waitedMs:"90 s; rm"}]});
 const text=calls[0][calls[0].indexOf("--text")+1];
 assert.ok(text.includes(`TailOS live check last saw ${"c".repeat(40)} after 90 s.`));assert.ok(text.includes("TailOS rollback probe last saw no readable release.json after 90 s."));
 assert.ok(!text.includes("SYNTHETIC")&&!text.includes("90 s; rm"),text);
});
test("R6 an invalid TailOS switch window stops the daemon before any command",async()=>{
 const cwd=mkdtempSync(join(tmpdir(),"tailos-window-")),log=join(cwd,"calls.log"),fakeTT=join(cwd,"tt");
 writeFileSync(fakeTT,`#!/bin/sh\necho "$@" >> ${JSON.stringify(log)}\necho '[]'\n`);chmodSync(fakeTT,0o755);
 for(const bad of [-1,300001,"90000",1.5])await assert.rejects(serveDeployment({version:1,enabled:true,cwd,journalDirectory:cwd,tt:fakeTT,targets:{tailos:{switchWindowMs:bad}}},{once:true}),/switch window/);
 assert.equal(existsSync(log),false);
 await serveDeployment({version:1,enabled:true,cwd,journalDirectory:cwd,tt:fakeTT,targets:{tailos:{switchWindowMs:120000}}},{once:true});assert.equal(readFileSync(log,"utf8"),"deployment list\n");
});
const MATRIX_A='{"matrix":"approved"}\n',MATRIX_B='{"matrix":"changed"}\n';
const uncoveredReason=`Matrix digest changed ${hash(MATRIX_A).slice(0,8)} to ${hash(MATRIX_B).slice(0,8)}; no owner approval covers it`;
// The integrated checkout's matrix file is MATRIX_B; the job was approved under MATRIX_A by message 11.
function changedMatrixHost(approvals){
 const h=matrixHost({jobs:[{id:"rel_fixture",state:"claimed",...(approvals?{matrixApprovals:approvals}:{})}]});
 mkdirSync(join(h.f.cwd,"verification"));writeFileSync(join(h.f.cwd,"verification/matrix.json"),MATRIX_B);
 h.integrated.plan={commit:"a".repeat(40),matrixDigest:hash(MATRIX_A),approvedMatrixDigest:hash(MATRIX_A),matrixApprovalMessageSeq:11};
 h.context=()=>JSON.parse(readFileSync(join(attemptDir(h.home,h.integrated.integratedCommit,0),"context.json"),"utf8"));
 return h;
}
test("m1 a changed matrix binds the integrated plan to its own digest and the newest owner approval of it",async()=>{
 const h=changedMatrixHost([{digest:hash(MATRIX_B),messageSeq:40},{digest:hash(MATRIX_A),messageSeq:99},{digest:hash(MATRIX_B),messageSeq:55},{digest:hash(MATRIX_B),messageSeq:"77"}]);
 assert.equal(await h.verify(h.integrated),false);
 const context=h.context();
 assert.equal(context.approvedMatrixDigest,hash(MATRIX_B));assert.equal(context.matrixApprovalMessageSeq,55);
 assert.equal(context.commit,h.integrated.integratedCommit);assert.equal(context.verifierAgentId,"agt_fixture");assert.equal(context.verifierRunId,"run_fixture");
 assert.deepEqual(h.matrix().map(c=>c.argv[2]),["plan","run"]);assert.equal(h.sends().length,1);
 // The job's own plan is not rewritten.
 assert.equal(h.integrated.plan.approvedMatrixDigest,hash(MATRIX_A));assert.equal(h.integrated.plan.matrixApprovalMessageSeq,11);
});
test("m2 a changed matrix with no covering owner approval is refused by name before any matrix run",async()=>{
 for(const approvals of [undefined,[],[{digest:hash(MATRIX_A),messageSeq:11}],[{digest:hash(MATRIX_B),messageSeq:0}],[{digest:hash(MATRIX_B)}]]){
  const h=changedMatrixHost(approvals);
  await assert.rejects(h.adapter.verifyIntegrated(h.integrated),e=>failureReason(e)===uncoveredReason);
  assert.equal(h.matrix().length,0);assert.equal(h.sends().length,0);
  assert.ok(!existsSync(join(attemptDir(h.home,h.integrated.integratedCommit,0),"context.json")));
 }
 assert.match(uncoveredReason,/^Matrix digest changed [a-f0-9]{8} to [a-f0-9]{8}; no owner approval covers it$/);
});
test("m3 an unchanged matrix keeps the job's digest and approval, whatever other approvals exist",async()=>{
 const h=changedMatrixHost([{digest:hash(MATRIX_B),messageSeq:55},{digest:hash(MATRIX_A),messageSeq:99}]);
 writeFileSync(join(h.f.cwd,"verification/matrix.json"),MATRIX_A);
 assert.equal(await h.verify(h.integrated),false);
 const context=h.context();
 assert.equal(context.approvedMatrixDigest,hash(MATRIX_A));assert.equal(context.matrixApprovalMessageSeq,11);assert.equal(context.matrixDigest,hash(MATRIX_A));
 assert.deepEqual(h.matrix().map(c=>c.argv[2]),["plan","run"]);
});
// tasks-hub gains a matrix change after the candidate was accepted: the real
// integration cherry-picks the candidate onto it, and the real host adapter
// verifies, refuses and escalates through stubbed host commands.
function matrixChangeRelease(approvals){
 const f=fixture(),originHead=remoteHead(f);git(f.cwd,"checkout","tasks-hub");mkdirSync(join(f.cwd,"verification"));f.base=change(f,"verification/matrix.json",MATRIX_A);
 git(f.cwd,"checkout","-B","candidate");const j=job(f,change(f,"client/a.js","a"));
 Object.assign(j.plan,{matrixDigest:hash(MATRIX_A),approvedMatrixDigest:hash(MATRIX_A),matrixApprovalMessageSeq:11});
 git(f.cwd,"checkout","tasks-hub");const tip=change(f,"verification/matrix.json",MATRIX_B);
 ignorePrerequisites(f.cwd);placePrerequisites(f.cwd);
 const c=config(f,j),a=fake(),argvs=[],home=dirname(c.journalPath);
 const host=new HostAdapter({cwd:f.cwd,journalDirectory:home},{id:"rel_fixture",agentId:"agt_fixture",runId:"run_fixture",generation:1});host.processStartTime=()=>null;
 host.startMatrixRun=(argv,dir)=>{argvs.push(argv);writeFileSync(join(dir,"receipt.json"),JSON.stringify({environment:{},checks:[{exitCode:0}]}));return exitedPid();};
 host.command=argv=>{argvs.push(argv);
  if(argv[1]==="deployment"&&argv[2]==="list")return JSON.stringify([{id:"rel_fixture",state:"claimed",matrixApprovals:approvals}]);
  if(argv[1]==="deployment"&&argv[2]==="handler")return JSON.stringify({id:"agt_0123abcd"});
  if(argv[1]==="scripts/verify-matrix.mjs"&&argv[2]==="plan"){writeFileSync(argv[4],JSON.stringify(workedPlan));return "";}
  return "";};
 a.verifyIntegrated=x=>host.verifyIntegrated(x);a.escalate=async d=>{a.calls.push("escalate");await host.escalate(d);};
 return {f,j,c,a,argvs,home,tip,originHead,sends:()=>argvs.filter(x=>x[1]==="send"),matrix:()=>argvs.filter(x=>x[1]==="scripts/verify-matrix.mjs")};
}
test("m4 end to end: a matrix change on tasks-hub after acceptance verifies under the owner approval of the new digest",async()=>{
 const r=matrixChangeRelease([{digest:hash(MATRIX_A),messageSeq:11},{digest:hash(MATRIX_B),messageSeq:40}]);
 assert.equal((await runRelease(r.c,r.a)).outcome,"waiting_matrix");assert.equal(r.sends().length,0,"the first poll starts the run");
 assert.equal((await runRelease(r.c,r.a)).outcome,"waiting_matrix");
 const integrated=git(r.f.cwd,"rev-parse","HEAD");assert.notEqual(integrated,r.j.commit);assert.equal(git(r.f.cwd,"rev-parse","HEAD^"),r.tip);
 const context=JSON.parse(readFileSync(join(attemptDir(r.home,integrated,0),"context.json"),"utf8"));
 // What verify-matrix.mjs plan requires: the approved digest is the integrated file's.
 assert.equal(context.approvedMatrixDigest,hash(readFileSync(join(r.f.cwd,"verification/matrix.json"),"utf8")));assert.equal(context.approvedMatrixDigest,hash(MATRIX_B));
 assert.equal(context.matrixApprovalMessageSeq,40);assert.equal(context.commit,integrated);
 assert.deepEqual(r.matrix().map(x=>x[2]),["plan","run"]);
 const sends=r.sends();assert.equal(sends.length,1);assert.equal(sends[0][sends[0].indexOf("--subject")+1],"Import verification for the integrated release commit");
 assert.deepEqual(r.a.calls,[]);assert.equal(git(r.f.cwd,"rev-parse","refs/heads/tasks-hub"),r.tip);
 assert.equal(r.j.plan.approvedMatrixDigest,hash(MATRIX_A));
});
test("m5 end to end: a matrix change no owner approval covers refuses the job by name and leaves tasks-hub alone",async()=>{
 const r=matrixChangeRelease([{digest:hash(MATRIX_A),messageSeq:11}]);
 await assert.rejects(runRelease(r.c,r.a),/Release refused before publication/);
 assert.deepEqual(r.a.calls,["refuse","escalate"]);assert.equal(r.matrix().length,0);
 const journal=JSON.parse(readFileSync(r.c.journalPath,"utf8"));assert.equal(journal.phase,"refused");assert.equal(journal.refusalReason,uncoveredReason);assert.notEqual(journal.published,true);assert.deepEqual(journal.effects,[]);
 const sends=r.sends();assert.equal(sends.length,1);assert.equal(sends[0][sends[0].indexOf("--subject")+1],"Release refused before publication");
 const text=sends[0][sends[0].indexOf("--text")+1];assert.ok(text.includes(`Reason: ${uncoveredReason}.`));assert.doesNotMatch(text,/verify-matrix\.mjs exit/);
 assert.equal(git(r.f.cwd,"rev-parse","refs/heads/tasks-hub"),r.tip);assert.equal(remoteHead(r.f),r.originHead);
});
// Hub and bridge readiness window: each probe argv prints one fixed JSON line,
// as the real probe does after its own wait. No host is contacted.
const printJSON=v=>[process.execPath,"-e",`console.log(${JSON.stringify(JSON.stringify(v))})`];
const READY_ID={commit:"d".repeat(40),artifactSHA256:"e".repeat(64)},READY_UP={...READY_ID,integrity:true,hubResponds:true,migrationsApplied:true,containersRunning:true,release:"rel_fixture-hub"};
const READY_CAPTURE={app:{state:"RUNNING",containers:[{service:"hub",state:"running",id:"1".repeat(12)}]},logs:[{service:"hub",lines:["migration 41 started"]},{service:"discord-bridge",unavailable:"no log lines"}]};
const READY_DOWN={...READY_UP,hubResponds:false,migrationsApplied:false,waitedMs:240000,polls:49,capture:READY_CAPTURE};
function readyAdapter(target,{live=READY_UP,rollback={restored:true,databaseWritesPreserved:true},program=[process.execPath,"-e","0"]}={}){
 const home=mkdtempSync(join(tmpdir(),"ready-wait-")),a=new HostAdapter({cwd:home,journalDirectory:home},{id:"rel_fixture"}),ran=[],command=a.command.bind(a);
 a.command=(...args)=>{ran.push(args[0]);return command(...args);};
 const artifact={...READY_ID,liveProbe:Array.isArray(live)?live:printJSON(live),rollbackSafe:true,rollbackProgram:program,rollbackProbe:Array.isArray(rollback)?rollback:printJSON(rollback)};
 a.artifacts.set(target,artifact);return Object.assign(a,{ran,artifact});
}
const READY_POLICY={startupMs:60000,failures:3,intervalMs:5000,relayCleanMs:0},EXIT1=[process.execPath,"-e","process.exit(1)"];
async function countedLiveCheck(a,target){let now=0;const sleeps=[];const out=await liveCheck(a,target,READY_POLICY,async ms=>{sleeps.push(ms);now+=ms;},()=>now);return {out,sleeps,checks:a.ran.length};}
test("T1 a hub or bridge probe that waited and is still not ready fails the live check once",async()=>{
 for(const target of ["hub","bridge"]){
  const a=readyAdapter(target,{live:READY_DOWN});assert.equal(await a.check(target),"readiness");
  assert.deepEqual(a.probeWait(target,"live"),{waitedMs:240000,polls:49,capture:READY_CAPTURE});
  const once=readyAdapter(target,{live:READY_DOWN});assert.deepEqual(await countedLiveCheck(once,target),{out:false,sleeps:[],checks:1});
  const stopped=readyAdapter(target,{live:{...READY_UP,containersRunning:false,waitedMs:240000,polls:49}});assert.equal(await stopped.check(target),"readiness");assert.deepEqual(stopped.probeWait(target,"live"),{waitedMs:240000,polls:49});
  // Ready after a wait passes and still records the wait, with no capture.
  const late=readyAdapter(target,{live:{...READY_UP,waitedMs:15000,polls:4}});assert.deepEqual(await countedLiveCheck(late,target),{out:true,sleeps:[],checks:1});
  assert.deepEqual(late.probeWait(target,"live"),{waitedMs:15000,polls:4});
  // Identity and integrity still win over readiness.
  assert.equal(await readyAdapter(target,{live:{...READY_DOWN,commit:"c".repeat(40)}}).check(target),"identity");
  assert.equal(await readyAdapter(target,{live:{...READY_DOWN,integrity:false}}).check(target),"integrity");
  // A probe that reports no wait, or cannot report at all, keeps the generic retry.
  const old=readyAdapter(target,{live:{...READY_UP,hubResponds:false}});assert.equal(await old.check(target),false);assert.equal(old.probeWait(target,"live"),undefined);
  const silent=readyAdapter(target,{live:EXIT1});assert.deepEqual(await countedLiveCheck(silent,target),{out:false,sleeps:Array.from({length:14},()=>5000),checks:15});
  // A later probe that cannot report drops the earlier wait record.
  const stale=readyAdapter(target,{live:READY_DOWN});await stale.check(target);stale.artifact.liveProbe=EXIT1;assert.equal(await stale.check(target),false);assert.equal(stale.probeWait(target,"live"),undefined);
  for(const junk of [{waitedMs:-1},{waitedMs:"240000"},{waitedMs:1.5}]){const j=readyAdapter(target,{live:{...READY_DOWN,...junk}});assert.equal(await j.check(target),false);assert.equal(j.probeWait(target,"live"),undefined);}
 }
 // Mini is untouched: a waitedMs field there means nothing.
 const mini=readyAdapter("mini",{live:{...READY_ID,integrity:true,relayRunning:false,newErrors:0,waitedMs:5}});assert.equal(await mini.check("mini"),false);assert.equal(mini.probeWait("mini","live"),undefined);
 // An invalid window stops the daemon before any command, like the TailOS one.
 const cwd=mkdtempSync(join(tmpdir(),"ready-window-")),log=join(cwd,"calls.log"),fakeTT=join(cwd,"tt");
 writeFileSync(fakeTT,`#!/bin/sh\necho "$@" >> ${JSON.stringify(log)}\necho '[]'\n`);chmodSync(fakeTT,0o755);
 for(const target of ["hub","bridge"])for(const bad of [-1,300001,"240000",1.5])await assert.rejects(serveDeployment({version:1,enabled:true,cwd,journalDirectory:cwd,tt:fakeTT,targets:{[target]:{host:"truenas",readyWindowMs:bad}}},{once:true}),/readiness window/);
 assert.equal(existsSync(log),false);
 await serveDeployment({version:1,enabled:true,cwd,journalDirectory:cwd,tt:fakeTT,targets:{hub:{host:"truenas",readyWindowMs:120000},bridge:{host:"truenas"}}},{once:true});assert.equal(readFileSync(log,"utf8"),"deployment list\n");
});
test("T2 a rollback that comes up late is restored, and a failed rollback still records what the probe saw",async()=>{
 for(const target of ["hub","bridge"]){
  const late=readyAdapter(target,{rollback:{restored:true,databaseWritesPreserved:true,waitedMs:15000,polls:4}});assert.equal(await late.rollback(target),true);
  assert.deepEqual(late.probeWait(target,"rollback"),{waitedMs:15000,polls:4});assert.deepEqual(late.ran,[late.artifact.rollbackProgram,late.artifact.rollbackProbe]);
  const down={restored:false,databaseWritesPreserved:false,waitedMs:240000,polls:49,capture:READY_CAPTURE};
  const never=readyAdapter(target,{rollback:down});assert.equal(await never.rollback(target),false);
  assert.deepEqual(never.probeWait(target,"rollback"),{waitedMs:240000,polls:49,capture:READY_CAPTURE});
  // The rollback program itself fails: still false, and the probe runs once for the record.
  const broken=readyAdapter(target,{rollback:down,program:[process.execPath,"-e","process.exit(3)"]});assert.equal(await broken.rollback(target),false);
  assert.deepEqual(broken.probeWait(target,"rollback"),{waitedMs:240000,polls:49,capture:READY_CAPTURE});assert.deepEqual(broken.ran,[broken.artifact.rollbackProgram,broken.artifact.rollbackProbe]);
  // A probe that says restored cannot overturn a failed rollback program.
  const lucky=readyAdapter(target,{rollback:{restored:true,databaseWritesPreserved:true,waitedMs:0,polls:1},program:[process.execPath,"-e","process.exit(3)"]});assert.equal(await lucky.rollback(target),false);
  const silent=readyAdapter(target,{rollback:EXIT1,program:[process.execPath,"-e","process.exit(3)"]});assert.equal(await silent.rollback(target),false);assert.equal(silent.probeWait(target,"rollback"),undefined);
  await assert.rejects(readyAdapter(target,{rollback:EXIT1}).rollback(target),/Host operation failed/);
 }
 // TailOS keeps its own path: a failed rollback program still throws and no probe runs.
 const tailos=readyAdapter("tailos",{rollback:{restored:true,databaseWritesPreserved:true,lastCommit:"c".repeat(40),waitedMs:0},program:[process.execPath,"-e","process.exit(3)"]});
 await assert.rejects(tailos.rollback("tailos"),/Host operation failed/);assert.equal(tailos.ran.length,1);assert.equal(tailos.probeWait("tailos","rollback"),undefined);
});
// A paired release driven by the fake adapter, with check, rollback and
// probeWait handed to a real HostAdapter whose probes print fixed output.
function readyRelease(liveHub,rollbackHub={restored:true,databaseWritesPreserved:true,waitedMs:15000,polls:4}){
 const p=pairedRelease(),h=readyAdapter("hub",{live:liveHub,rollback:rollbackHub});let details;
 h.artifacts.set("bridge",{...h.artifact,liveProbe:printJSON(READY_UP),rollbackProbe:printJSON({restored:true,databaseWritesPreserved:true,waitedMs:0,polls:1})});
 p.a.check=t=>h.check(t);p.a.rollback=t=>{p.a.calls.push("rollback:"+t);return h.rollback(t);};p.a.probeWait=(t,kind)=>h.probeWait(t,kind);p.a.escalate=async d=>{details=d;p.a.calls.push("escalate");};
 return {...p,h,details:()=>details};
}
test("T3 a hub that ends not ready journals its capture on the effect and keeps it out of the receipt and escalation",async()=>{
 const p=readyRelease(READY_DOWN);await assert.rejects(runRelease(p.c,p.a),/failed/);
 const journal=p.journal(),hub=journal.effects.find(e=>e.target==="hub"),bridge=journal.effects.find(e=>e.target==="bridge");
 assert.deepEqual(journal.failure,{step:"live-check",target:"hub",reason:"live verification failed"});
 assert.deepEqual(hub.liveCheck,{waitedMs:240000,polls:49,capture:READY_CAPTURE});
 assert.deepEqual(hub.rollbackCheck,{waitedMs:15000,polls:4});assert.equal(hub.rollback,"restored");
 assert.deepEqual(bridge.rollbackCheck,{waitedMs:0,polls:1});assert.equal(bridge.liveCheck,undefined);
 assert.equal(journal.outcome,"rolled_back");assert.equal(p.receipt().outcome,"rolled_back");
 assert.deepEqual(p.receipt().targets.map(t=>[t.target,t.outcome,t.rollback]),[["hub","rolled_back","restored"],["bridge","rolled_back","restored"]]);
 for(const text of [JSON.stringify(p.receipt()),JSON.stringify(p.details())]){assert.ok(!text.includes("capture"));assert.ok(!text.includes("migration 41"));assert.ok(!text.includes("polls"));}
 assert.deepEqual(p.details().probeWaits.find(w=>w.target==="hub"&&w.probe==="live"),{target:"hub",probe:"live",lastCommit:undefined,waitedMs:240000});
 // The live probe ran once for the hub, not once per generic retry.
 assert.equal(p.h.ran.filter(argv=>argv===p.h.artifact.liveProbe).length,1);
 // The Board text names TailOS waits only, whatever the details carry.
 const calls=[],notice=new HostAdapter({cwd:p.f.cwd,journalDirectory:p.f.cwd},{id:"rel_fixture"});notice.command=argv=>{calls.push(argv);return "";};
 await notice.escalate({jobId:"rel_fixture",outcome:"rolled_back",probeWaits:[{target:"hub",probe:"live",waitedMs:240000,capture:READY_CAPTURE}]});
 assert.ok(!calls[0].join(" ").includes("migration 41"));assert.ok(!calls[0].join(" ").includes("240"));
 // A rollback that never comes up is blocked, with its own capture journaled.
 const b=readyRelease(READY_DOWN,{restored:false,databaseWritesPreserved:false,waitedMs:240000,polls:49,capture:READY_CAPTURE});await assert.rejects(runRelease(b.c,b.a),/failed/);
 const blocked=b.journal().effects.find(e=>e.target==="hub");assert.equal(blocked.rollback,"blocked");assert.deepEqual(blocked.rollbackCheck,{waitedMs:240000,polls:49,capture:READY_CAPTURE});
 assert.equal(b.receipt().outcome,"blocked");assert.ok(!JSON.stringify([b.receipt(),b.details()]).includes("migration 41"));
});
test("T4 a probe's capture is reduced to the fixed, redacted shape before it reaches the journal",async()=>{
 const SECRET="SYNTHETIC_PRIVATE_TOKEN",opaque="SYNTHETICa1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6";
 const hostile={app:{state:"RUNNING",config:{token:SECRET},notes:SECRET,containers:[{service:"hub",state:"running",id:"1".repeat(64),environment:{TOKEN:SECRET}},{service:SECRET+" x",state:"running; rm",id:SECRET}]},notes:SECRET,
  logs:[{service:"hub",lines:["x".repeat(5000),"token="+SECRET,"Authorization: Bearer SYNTHETIC.bearer","id "+opaque,"ok line",{nested:SECRET}],extra:SECRET},{service:"discord-bridge",unavailable:"no log lines",detail:SECRET},{service:"hub",unavailable:SECRET},{service:"hub",unavailable:"time budget"},{service:"hub",lines:[SECRET]}]};
 const expected={app:{state:"RUNNING",containers:[{service:"hub",state:"running",id:"1".repeat(12)},{service:null,state:null,id:null}]},
  logs:[{service:"hub",lines:["x".repeat(300),"[redacted]","[redacted]","id [redacted]","ok line"]},{service:"discord-bridge",unavailable:"no log lines"},{service:"hub",unavailable:"log read failed"},{service:"hub",unavailable:"time budget"}]};
 const p=readyRelease({...READY_DOWN,capture:hostile,[SECRET]:SECRET,notes:SECRET},{restored:false,databaseWritesPreserved:false,waitedMs:240000,polls:49,capture:hostile,notes:SECRET});
 await assert.rejects(runRelease(p.c,p.a),/failed/);
 const text=readFileSync(p.c.journalPath,"utf8"),hub=JSON.parse(text).effects.find(e=>e.target==="hub");
 assert.ok(!text.includes("SYNTHETIC"));assert.ok(!text.includes("running; rm"));
 assert.deepEqual(hub.liveCheck,{waitedMs:240000,polls:49,capture:expected});assert.deepEqual(hub.rollbackCheck,{waitedMs:240000,polls:49,capture:expected});
 assert.ok(!JSON.stringify([p.receipt(),p.details()]).includes("SYNTHETIC"));
 // An unavailable capture passes unchanged; a capture that is not an object becomes one.
 const unavailable={app:"unavailable",logs:[{service:"hub",unavailable:"log read failed"},{service:"discord-bridge",unavailable:"invalid container id"}]};
 const u=readyAdapter("hub",{live:{...READY_DOWN,capture:unavailable}});assert.equal(await u.check("hub"),"readiness");assert.deepEqual(u.probeWait("hub","live").capture,unavailable);
 const s=readyAdapter("hub",{live:{...READY_DOWN,capture:SECRET}});assert.equal(await s.check("hub"),"readiness");assert.deepEqual(s.probeWait("hub","live"),{waitedMs:240000,polls:49,capture:{app:"unavailable",logs:[]}});
});

// Tagged refusals (g1-g5, g7). A candidate and tasks-hub that write the same
// files differently, so the cherry-pick leaves each of them unmerged.
const TOKEN="SYNTHETIC_PRIVATE_TOKEN",NEXT_ACTION="Next: re-apply the candidate on the current tasks-hub through a follow-through item and re-review the new commit.";
function conflictFixture(files=["client/base.js"]){
 const f=fixture(),write=text=>{for(const p of files){mkdirSync(dirname(join(f.cwd,p)),{recursive:true});writeFileSync(join(f.cwd,p),text+p);}git(f.cwd,"add",".");git(f.cwd,"commit","-m",text);return git(f.cwd,"rev-parse","HEAD");};
 const commit=write("candidate ");git(f.cwd,"checkout","tasks-hub");const prior=write("release ");return {f,j:job(f,commit),prior};
}
async function journalOf(f,j,arrange=()=>{},expected=/refused/){
 const a=fake(),c=config(f,j);let escalation;a.escalate=async d=>{escalation=d;a.calls.push("escalate");};arrange(a,c);
 await assert.rejects(runRelease(c,a),expected);const raw=readFileSync(c.journalPath,"utf8");return {a,c,raw,journal:JSON.parse(raw),escalation};
}
const checkoutState=cwd=>({status:git(cwd,"status","--porcelain"),head:git(cwd,"rev-parse","HEAD"),ref:git(cwd,"rev-parse","refs/heads/tasks-hub")});
function refusesUnchanged(cwd,j,tag){
 const before=checkoutState(cwd);assert.throws(()=>integrateCandidate(cwd,j),e=>failureReason(e)===tag,tag);assert.deepEqual(checkoutState(cwd),before,tag);return before;
}
test("g4 a candidate that conflicts with tasks-hub is refused at step integrate with a reason naming the paths",async()=>{
 const {f,j,prior}=conflictFixture(),reason="integration-conflict: client/base.js",r=await journalOf(f,j);
 assert.deepEqual(r.a.calls,["refuse","escalate"]);assert.equal(r.journal.phase,"refused");
 assert.deepEqual(r.journal.failure,{step:"integrate",reason});assert.equal(r.journal.refusalReason,reason);
 assert.deepEqual(r.journal.failureDetail,{text:reason,paths:["client/base.js"],pathCount:1,pathsRejected:0});
 assert.deepEqual(r.escalation,{jobId:"rel_fixture",outcome:"refused",reason});assert.ok(!r.raw.includes("unclassified"));
 assert.equal(git(f.cwd,"rev-parse","tasks-hub"),prior);assert.equal(git(f.cwd,"status","--porcelain"),"");
});
test("g4 paths are named only when every one is valid and the reason fits, else they are counted",async()=>{
 const secret=`client/token=${TOKEN}.js`,a=conflictFixture(["client/base.js",secret]),ra=await journalOf(a.f,a.j);
 assert.equal(ra.journal.refusalReason,"integration-conflict: 2 paths");assert.deepEqual(ra.journal.failure,{step:"integrate",reason:"integration-conflict: 2 paths"});
 assert.deepEqual(ra.journal.failureDetail,{text:"integration-conflict: 2 paths",paths:["client/base.js"],pathCount:2,pathsRejected:1});
 for(const text of [ra.raw,JSON.stringify(ra.escalation)]){assert.ok(!text.includes(TOKEN));assert.ok(!text.includes("token="));}
 assert.equal(git(a.f.cwd,"status","--porcelain"),"");
 // The first and last sorted names carry a space that a trimmed read would lose.
 const b=conflictFixture([" lead.js","client/base.js","zz-trail.js "]),rb=await journalOf(b.f,b.j);
 assert.deepEqual(rb.journal.failureDetail,{text:"integration-conflict: 3 paths",paths:["client/base.js"],pathCount:3,pathsRejected:2});assert.ok(!rb.raw.includes("lead.js"));assert.ok(!rb.raw.includes("zz-trail"));
 const long=["a","b","c"].map(x=>"client/"+x.repeat(93)+".js"),c=conflictFixture(long),rc=await journalOf(c.f,c.j);
 assert.deepEqual(rc.journal.failureDetail,{text:"integration-conflict: 3 paths",paths:long,pathCount:3,pathsRejected:0});assert.equal(rc.escalation.reason,"integration-conflict: 3 paths");
 const over="client/"+"d".repeat(111)+".js",d=conflictFixture([...long,over]),rd=await journalOf(d.f,d.j);assert.equal(over.length,121);
 assert.deepEqual(rd.journal.failureDetail,{text:"integration-conflict: 4 paths",paths:long,pathCount:4,pathsRejected:1});assert.ok(!rd.raw.includes(over));
 const one=conflictFixture([secret]),ro=await journalOf(one.f,one.j);assert.equal(ro.journal.refusalReason,"integration-conflict: 1 path");assert.deepEqual(ro.journal.failureDetail.paths,[]);
 // The journal's own validation: at most 50 paths, and only valid strings.
 const many=Array.from({length:60},(_,i)=>`client/f${i}.js`),capped=failureDetail(Object.assign(releaseError("integration-conflict: 60 paths"),{conflict:{paths:many,count:60,rejected:0}}));
 assert.equal(capped.paths.length,50);assert.equal(capped.pathCount,60);assert.equal(capped.pathsRejected,0);
 const mixed=failureDetail(Object.assign(releaseError("integration-conflict: 4 paths"),{conflict:{paths:["client/a.js",7,null,`token=${TOKEN}`,"x".repeat(121)],count:-1,rejected:"many"}}));
 assert.deepEqual(mixed,{text:"integration-conflict: 4 paths",paths:["client/a.js"],pathsRejected:4});
});
test("g4 the refusal notice for a conflict names the reason and the next action, and no other refusal changes",async()=>{
 const calls=[],notice=new HostAdapter({cwd:tmpdir(),journalDirectory:tmpdir()},{id:"rel_fixture"});notice.command=argv=>{calls.push(argv);return "";};
 const text=()=>calls.at(-1)[calls.at(-1).indexOf("--text")+1];
 await notice.escalate({jobId:"rel_fixture",outcome:"refused",reason:"integration-conflict: client/base.js"});
 assert.equal(text(),`Release rel_fixture was refused before publication; nothing was published or deployed. Reason: integration-conflict: client/base.js. ${NEXT_ACTION} Handler reconciliation required.`);
 assert.equal(calls[0][calls[0].indexOf("--subject")+1],"Release refused before publication");assert.equal(calls[0][calls[0].indexOf("--request-id")+1],"rel_fixture-release-failure");
 await notice.escalate({jobId:"rel_fixture",outcome:"refused",reason:"integration-conflict: 2 paths"});assert.ok(text().includes("Reason: integration-conflict: 2 paths. "+NEXT_ACTION));
 await notice.escalate({jobId:"rel_fixture",outcome:"refused",reason:"Missing matrix prerequisites: .build/test.wasm"});
 assert.equal(text(),"Release rel_fixture was refused before publication; nothing was published or deployed. Reason: Missing matrix prerequisites: .build/test.wasm. Handler reconciliation required.");
 await notice.escalate({jobId:"rel_fixture",outcome:"refused",reason:`integration-conflict: token=${TOKEN}`});assert.ok(text().includes("Reason: unclassified. Handler"));assert.ok(!text().includes("Next:"));
});
test("g3 every integrateCandidate refusal has its own tag and leaves the checkout as it found it",()=>{
 const m=fixture(),mj=job(m,change(m,"client/a.js","a"));mj.plan.commit=m.base;refusesUnchanged(m.cwd,mj,"Release binding mismatch");
 // A dirty checkout is the caller's: nothing is reset, stashed or cleaned.
 const d=fixture(),dj=job(d,change(d,"client/a.js","a"));writeFileSync(join(d.cwd,"client/base.js"),"caller edit");writeFileSync(join(d.cwd,"client/untracked.txt"),"caller file");
 const dirty=refusesUnchanged(d.cwd,dj,"Dirty deployment checkout");assert.notEqual(dirty.status,"");
 assert.equal(readFileSync(join(d.cwd,"client/base.js"),"utf8"),"caller edit");assert.equal(readFileSync(join(d.cwd,"client/untracked.txt"),"utf8"),"caller file");
 const u=fixture(),missing="d".repeat(40);change(u,"client/a.js","a");refusesUnchanged(u.cwd,{...job(u,missing),plan:{commit:missing}},"Unknown commit identity");
 const b=fixture(),bc=change(b,"client/a.js","a");git(b.cwd,"checkout","tasks-hub");const later=change(b,"client/c.js","c");
 refusesUnchanged(b.cwd,{...job(b,bc),baseCommit:later},"Unknown candidate base");
 // A merge commit in the series, with tasks-hub moved so it cannot fast-forward.
 const n=fixture();change(n,"client/a.js","a");git(n.cwd,"checkout","-b","side",n.base);change(n,"client/s.js","s");git(n.cwd,"checkout","candidate");git(n.cwd,"merge","--no-ff","-m","merge","side");
 const merged=git(n.cwd,"rev-parse","HEAD");git(n.cwd,"checkout","tasks-hub");change(n,"client/c.js","c");
 assert.equal(refusesUnchanged(n.cwd,job(n,merged),"Nonlinear candidate series").status,"");
 // The candidate's change is already on tasks-hub: an empty pick, no conflict.
 // tasks-hub moves first, so its copy of the change is never the same commit.
 const e=fixture(),ec=change(e,"client/a.js","a");git(e.cwd,"checkout","tasks-hub");change(e,"client/c.js","c");change(e,"client/a.js","a");
 assert.equal(refusesUnchanged(e.cwd,job(e,ec),"Candidate integration refused").status,"");
});
test("g3 the pre-step runner refusals are tagged and leave the journal alone",async()=>{
 const tagged=tag=>e=>failureReason(e)===tag;
 const f=fixture(),j=job(f,change(f,"client/a.js","a")),a=fake(),c=config(f,j);let entered,release;
 const ready=new Promise(r=>entered=r),gate=new Promise(r=>release=r);a.deploy=async()=>{entered();await gate;};
 const first=runRelease(c,a);await ready;const other=config(f,j);
 await assert.rejects(runRelease(other,fake()),tagged("Release host locked; inspect prior execution"));assert.ok(!existsSync(other.journalPath));release();await first;
 const g=fixture(),k=job(g,change(g,"client/a.js","a")),b=fake(),d=config(g,k);b.check=async()=>"identity";await assert.rejects(runRelease(d,b),tagged("Release failed; inspect saved journal"));
 const before=readFileSync(d.journalPath,"utf8");await assert.rejects(runRelease(d,b),tagged("Ambiguous journal requires handler reconciliation"));assert.equal(readFileSync(d.journalPath,"utf8"),before);
 const h=fixture(),unclaimed=config(h,{...job(h,change(h,"client/a.js","a")),state:"verified"});
 await assert.rejects(runRelease(unclaimed,fake()),tagged("Claimed verified job required"));assert.ok(!existsSync(unclaimed.journalPath));
});
test("g2 the four named adapter refusals carry their tag and a prepare refusal journals it",async()=>{
 const tagged=tag=>e=>failureReason(e)===tag && failureDetail(e).text===tag;
 const {f,commit,home,config:hostConfig}=hostFixture(),adapter=new HostAdapter(hostConfig,{...job(f,commit),id:"rel_tags"});
 await assert.rejects(adapter.prepare("mini","e".repeat(40)),tagged("Candidate build checkout mismatch"));
 importInputs(adapter,commit,{});await assert.rejects(adapter.prepare("mini",commit),tagged("Exact job target input required"));
 hostConfig.targets.hub={};const release="rel_tags-"+commit.slice(0,12)+"-hub";adapter.command=argv=>{if(argv[1]==="version")return stamped(commit);if(argv.includes("build"))writeFileSync(argv[argv.indexOf("-o")+1],"binary");return "";};
 importInputs(adapter,commit,{hub:{release,backupJobId:"rel_previous",backup:join(home,"rel_previous-backup")}});
 await assert.rejects(adapter.prepare("hub",commit),tagged("Fresh job backup identity required"));
 adapter.command=()=>"no url";await assert.rejects(adapter.deploy("tailos",{commit}),tagged("TailOS deployment identity unconfirmed"));
 // Through the runner: the real adapter's prepare has no input for the target.
 const g=fixture(),j=job(g,change(g,"client/a.js","a")),r=await journalOf(g,j,(a,c)=>{
  const host=new HostAdapter({cwd:g.cwd,journalDirectory:dirname(c.journalPath),baselines:c.baselines,targets:{tailos:{}}},{...j});importInputs(host,j.commit,{});a.prepare=(t,commit)=>host.prepare(t,commit);
 },/Release failed/);
 assert.deepEqual(r.journal.failure,{step:"prepare",target:"tailos",reason:"Exact job target input required"});assert.deepEqual(r.journal.failureDetail,{text:"Exact job target input required"});
 assert.deepEqual(Object.keys(r.journal.receipt).sort(),["commit","jobId","outcome","revert","targets","verificationDigest","version"]);
});
test("g7 the journal keeps a failure's validated text and nothing that was not validated",async()=>{
 const start=()=>{const f=fixture();return [f,job(f,change(f,"client/a.js","a"))];},absent=(text,...words)=>{for(const w of words)assert.ok(!text.includes(w),w);};
 // An untagged error: only its class name, whatever it carries.
 const ra=await journalOf(...start(),a=>{a.deploy=async()=>{throw Object.assign(new Error(TOKEN,{cause:new Error(TOKEN)}),{stderr:TOKEN,stdout:TOKEN,output:[TOKEN]});};},/Release failed/);
 assert.deepEqual(ra.journal.failureDetail,{name:"Error"});assert.deepEqual(ra.journal.failure,{step:"deploy",target:"tailos",reason:"unclassified"});absent(ra.raw,TOKEN);
 const coded=failureDetail(Object.assign(new TypeError(TOKEN),{code:"ENOENT"}));assert.deepEqual(coded,{name:"TypeError",code:"ENOENT"});
 assert.deepEqual(failureDetail(Object.assign(new Error("x"),{name:`token=${TOKEN}`,code:`token=${TOKEN}`})),{});assert.deepEqual(failureDetail(undefined),{});
 // A token-shaped tag is rejected: its length and why, never its text.
 const shaped=`token="${TOKEN}"`,rb=await journalOf(...start(),a=>{a.prepare=async()=>{throw releaseError(shaped);};},/Release failed/);
 assert.deepEqual(rb.journal.failure,{step:"prepare",target:"tailos",reason:"unclassified"});assert.deepEqual(rb.journal.failureDetail,{tagRejected:"characters",tagLength:shaped.length});absent(rb.raw,TOKEN,"token=");
 const before=await journalOf(...start(),a=>{a.fence=async()=>{throw releaseError(shaped);};});
 assert.equal(before.journal.refusalReason,"unclassified");assert.equal(before.journal.failure,undefined);assert.deepEqual(before.journal.failureDetail,{tagRejected:"characters",tagLength:shaped.length});
 absent(before.raw+JSON.stringify(before.escalation),TOKEN,"token=");
 const long="SYNTHETICPRIVATETOKEN".repeat(10),rc=await journalOf(...start(),a=>{a.prepare=async()=>{throw releaseError(long);};},/Release failed/);
 assert.deepEqual(rc.journal.failureDetail,{tagRejected:"length",tagLength:210});absent(rc.raw,"SYNTHETICPRIVATETOKEN");
 // An attempt record's refusal is read back from disk, so it is validated too.
 const d=fixture(),dj=job(d,change(d,"client/a.js","a"));git(d.cwd,"checkout","tasks-hub");change(d,"client/c.js","c");
 const rd=await journalOf(d,dj,(a,c)=>{
  const host=new HostAdapter({cwd:d.cwd,journalDirectory:dirname(c.journalPath)},{id:"rel_fixture"});host.command=argv=>argv[2]==="list"?"[]":"";
  a.verifyIntegrated=x=>{const dir=attemptDir(dirname(c.journalPath),x.integratedCommit,0);mkdirSync(dir,{recursive:true});writeFileSync(join(dir,"run.json"),JSON.stringify({version:1,state:"ended",refusal:shaped}));return host.verifyIntegrated(x);};
 });
 assert.equal(rd.journal.refusalReason,"unclassified");assert.deepEqual(rd.journal.failureDetail,{tagRejected:"characters",tagLength:shaped.length});absent(rd.raw+JSON.stringify(rd.escalation),TOKEN,"token=");
 // A valid tag is kept as exactly the reason.
 const valid=releaseError("Paired plan members disagree");assert.deepEqual(failureDetail(valid),{text:failureReason(valid)});
 const dir=mkdtempSync(join(tmpdir(),"release-detail-")),script=join(dir,"fake-deploy.mjs");
 writeFileSync(script,`process.stderr.write("${TOKEN}");console.log(JSON.stringify({classification:"remote-operation-failed",stage:"bridge-binary-upload",message:"${TOKEN}"}));process.exit(2);`);
 let thrown;try{new HostAdapter({cwd:dir,journalDirectory:dir},{id:"rel_fixture"}).command([process.execPath,script]);}catch(error){thrown=error;}
 assert.deepEqual(failureDetail(thrown),{text:"fake-deploy.mjs exit 2: remote-operation-failed at bridge-binary-upload"});
 // A tagged failure after publication with no step still names its cause.
 const rg=await journalOf(...start(),a=>{a.merged=async()=>{throw releaseError("Paired plan members disagree");};},/Release failed/);
 assert.equal(rg.journal.failure,undefined);assert.deepEqual(rg.journal.failureDetail,{text:"Paired plan members disagree"});assert.equal(rg.journal.published,true);
});
test("g1 the runner throws no untagged fixed error",()=>{
 const source=readFileSync(RUNNER,"utf8");assert.ok(!source.includes("throw new Error("));assert.equal(source.split("new Error(").length-1,2,"releaseError and the command wrapper");
 for(const tag of ["Release binding mismatch","Dirty deployment checkout","Unknown commit identity","Unknown candidate base","Nonlinear candidate series","Candidate integration refused","Integrated checkout changed","Release ref race","Ambiguous journal requires handler reconciliation","Release host locked; inspect prior execution","Fresh job backup identity required","Exact job target input required","Candidate build checkout mismatch","TailOS deployment identity unconfirmed"]){
  assert.ok(source.includes(`releaseError("${tag}")`),tag);assert.equal(failureReason(releaseError(tag)),tag);
 }
});

test("v2 restarted consumer adopts its own second holder, records only its groups and never launches a peer", async t => {
 const h=lockHost(t,{limit:"2",group:true});
 const peer=await acquireHostLock({path:h.path,item:"wi_peer",agent:"peer",runTimeoutMs:60000,pollMs:20,environment:{TAILTERM_MATRIX_MAX_HOLDERS:"2"}});
 try {
  assert.equal(await h.adapter.verifyIntegrated(h.integrated),false);const {pid,group}=await h.marker();
  t.after(()=>{try{process.kill(-group,"SIGKILL");}catch{}});
  await until(()=>holdersOf(rawReadHostState(h.path)).length===2,"two consumer holders");
  const saved=h.run();h.adapter.matrixChildren.clear();delete saved.pid;delete saved.processStartedAt;saved.state="starting";h.adapter.saveRun(h.dir,saved);
  assert.equal(await h.adapter.verifyIntegrated(h.integrated),false);assert.equal(h.run().pid,pid);assert.equal(h.starts(),1);
  assert.deepEqual(h.run().groups,[group]);assert.equal(h.adapter.matrixHeld,null);assert.equal(h.signals.length,0);
  assert.equal(holdersOf(rawReadHostState(h.path))[0].id,peer.id);
  const own=holdersOf(rawReadHostState(h.path))[1];
  h.adapter.hostState=()=>({version:2,holderLimit:2,holders:[{...own,id:"other-lease",pid:process.pid},own],waiters:[]});
  assert.equal(await h.adapter.verifyIntegrated(h.integrated),false);assert.equal(h.adapter.matrixHeld.reason,"host lock identity ambiguous");
  assert.equal(h.starts(),1);assert.deepEqual(h.signals,[]);
 } finally {await peer.release();for(const child of h.children){try{process.kill(-child.pid,"SIGKILL");}catch{}}}
});
test("v2 holder-set notices preserve resend identity, report both holders and change when the actual set changes", () => {
 const job={id:"rel_0123456789abcdef"},attempt="c".repeat(40)+"-r0";
 const a={id:"a",item:"wi_a",agent:"a",pid:123},b={id:"b",item:"wi_b",agent:"b",pid:456};
 const wait={attempt,position:1,length:1,priority:"normal",change:1,holders:[a,b]};
 const first=matrixWaitNotice(job,wait);assert.match(first.text,/wi_a\/a\/pid 123; wi_b\/b\/pid 456/);
 assert.equal(matrixWaitNotice(job,{...wait,holders:[b,a]}).requestId,first.requestId);
 assert.notEqual(matrixWaitNotice(job,{...wait,holders:[a]}).requestId,first.requestId);
 assert(first.requestId.length<=128);
});
test("v2 consumer holds if identity becomes ambiguous during its after-exit group snapshot", () => {
 const dir=mkdtempSync(join(tmpdir(),"release-after-exit-")),adapter=new HostAdapter({cwd:dir,journalDirectory:dir},{id:"rel_fixture"});
 const run={state:"started",pid:12345,launchedAt:Date.now(),groups:[]};let reads=0;
 adapter.matrixEntry=()=>++reads===1?{role:"holder",entry:{id:"own",pid:run.pid,groups:[]}}:{ambiguous:true};
 adapter.matrixSidecar=()=>null;adapter.pidGone=()=>true;adapter.signalProcess=()=>assert.fail("ambiguous identity must never be signalled");
 assert.equal(adapter.resolveMatrixRun(dir,run),"held");assert.equal(reads,2);
 assert.equal(adapter.matrixHeld.reason,"host lock identity ambiguous");assert.equal(run.snapshot,undefined);
 rmSync(dir,{recursive:true,force:true});
});
